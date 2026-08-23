package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type recordingAgentUpgradeRunner struct {
	missing map[string]bool
	calls   []string
}

func (r *recordingAgentUpgradeRunner) LookPath(name string) (string, error) {
	if r.missing[name] {
		return "", errors.New("not found")
	}
	return "/bin/" + name, nil
}

func (r *recordingAgentUpgradeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, name+" "+args[0])
	if args[0] == "--version" {
		return name + " 1.0", nil
	}
	return "updated", nil
}

func TestStartupAgentUpgradeKindsIncludesCodexClaudeAndCustomCommands(t *testing.T) {
	sessions := []Session{
		{Live: true, LastMode: SessionModeAgent, LastAgentKind: "codex", LastAgentStartCommand: "codex --yolo"},
		{Live: true, LastMode: SessionModeAgent, LastAgentKind: "custom", LastAgentStartCommand: "CLAUDE_CONFIG_DIR=/tmp/claude claude --continue"},
		{Live: false, LastMode: SessionModeAgent, LastAgentKind: "claude", LastAgentStartCommand: "claude"},
		{Live: true, LastMode: SessionModeAgent, LastAgentKind: "custom", LastAgentStartCommand: "other-agent"},
	}
	got := startupAgentUpgradeKinds(sessions, AgentConfig{Kind: "custom", Command: "other-agent"})
	if want := []string{"claude", "codex"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("upgrade kinds = %#v, want %#v", got, want)
	}
}

func TestUpgradeAgentCLIsRunsOneUpgradePerInstalledAgent(t *testing.T) {
	runner := &recordingAgentUpgradeRunner{}
	results := upgradeAgentCLIs(context.Background(), []Session{
		{Live: true, LastMode: SessionModeAgent, LastAgentKind: "codex", LastAgentStartCommand: "codex"},
		{Live: true, LastMode: SessionModeAgent, LastAgentKind: "claude", LastAgentStartCommand: "claude"},
		{Live: true, LastMode: SessionModeAgent, LastAgentKind: "claude", LastAgentStartCommand: "claude --continue"},
	}, AgentConfig{}, runner)
	if len(results) != 2 {
		t.Fatalf("upgrade results = %#v", results)
	}
	wantCalls := []string{
		"/bin/claude --version", "/bin/claude update", "/bin/claude --version",
		"/bin/codex --version", "/bin/codex update", "/bin/codex --version",
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("upgrade calls = %#v, want %#v", runner.calls, wantCalls)
	}
}

func TestUpgradeAgentCLIsDoesNotBlockOtherAgentsWhenOneIsMissing(t *testing.T) {
	runner := &recordingAgentUpgradeRunner{missing: map[string]bool{"claude": true}}
	results := upgradeAgentCLIs(context.Background(), []Session{
		{Live: true, LastMode: SessionModeAgent, LastAgentKind: "claude"},
		{Live: true, LastMode: SessionModeAgent, LastAgentKind: "codex"},
	}, AgentConfig{}, runner)
	if len(results) != 2 || results[0].Kind != "claude" || results[0].Err == nil || results[1].Kind != "codex" || results[1].Err != nil {
		t.Fatalf("upgrade results = %#v", results)
	}
}

func TestExecAgentUpgradeRunnerFindsUserLocalBinOutsideServicePath(t *testing.T) {
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(binDir, "claude")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin")
	got, err := (execAgentUpgradeRunner{}).LookPath("claude")
	if err != nil || got != executable {
		t.Fatalf("resolved executable = %q, err=%v", got, err)
	}
}

func TestAgentUpgradeEnvironmentAddsCommonAgentAndNodePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin")
	var got string
	for _, value := range agentUpgradeEnvironment() {
		if len(value) > 5 && value[:5] == "PATH=" {
			got = value[5:]
			break
		}
	}
	for _, want := range []string{filepath.Join(home, ".local", "bin"), filepath.Join(home, ".node", "bin"), "/opt/homebrew/bin"} {
		if !strings.Contains(got, want) {
			t.Fatalf("upgrade PATH %q does not contain %q", got, want)
		}
	}
}
