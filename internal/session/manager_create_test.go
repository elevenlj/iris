package session

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateSessionRunsPreStartCommand(t *testing.T) {
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher, WithPreStartCommand("source ~/.zshrc"))

	if _, err := manager.CreateSession(context.Background(), "test"); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	if len(launcher.terminals) != 1 {
		t.Fatalf("terminal count = %d, want 1", len(launcher.terminals))
	}
	if got := launcher.terminals[0].writes(); got != "source ~/.zshrc\r" {
		t.Fatalf("pre-start write = %q, want command with carriage return", got)
	}
}

func TestCreateSessionAlwaysStartsConfiguredAgentInDefaultWorkspace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workspaceRoot := t.TempDir()
	t.Setenv("IRIS_WORKSPACE_DIR", workspaceRoot)
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	customWorkspace := t.TempDir()
	manager.SetAgentConfig(AgentConfig{Kind: "codex", Command: "codex --dangerously-bypass-approvals-and-sandbox"}, []WorkspaceOption{
		{Label: "主项目", Value: customWorkspace, Default: true},
	})

	sess, err := manager.CreateSession(context.Background(), "Iris")
	if err != nil {
		t.Fatal(err)
	}
	writes := launcher.terminals[0].writes()
	workspace := filepath.Join(workspaceRoot, "Iris")
	if !strings.Contains(writes, "mkdir -p "+shellQuote(workspace)+"\r") || !strings.Contains(writes, "cd "+shellQuote(workspace)+"\r") || !strings.Contains(writes, "codex --dangerously-bypass-approvals-and-sandbox\r") {
		t.Fatalf("configured workspace and Agent were not started: %q", writes)
	}
	if strings.Contains(writes, customWorkspace) {
		t.Fatalf("custom workspace must not replace the session default: %q", writes)
	}
	if sess.LastMode != SessionModeAgent || sess.LastAgentKind != "codex" || sess.LastCWD != workspace {
		t.Fatalf("unexpected Agent session metadata: %#v", sess)
	}
}

func TestCreateSessionSkipsEmptyPreStartCommand(t *testing.T) {
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher, WithPreStartCommand("  "))

	if _, err := manager.CreateSession(context.Background(), "test"); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	if len(launcher.terminals) != 1 {
		t.Fatalf("terminal count = %d, want 1", len(launcher.terminals))
	}
	if got := launcher.terminals[0].writes(); got != "" {
		t.Fatalf("empty pre-start command should not write, got %q", got)
	}
}

func TestWorkspaceOptionsForSessionAlwaysStartsWithDedicatedDefault(t *testing.T) {
	workspaceRoot := t.TempDir()
	t.Setenv("IRIS_WORKSPACE_DIR", workspaceRoot)
	customWorkspace := t.TempDir()
	manager := NewManager(nil, &recordingLauncher{})
	manager.SetAgentConfig(AgentConfig{Kind: "codex", Command: CodexAgentCommand}, []WorkspaceOption{
		{Label: "现有项目", Value: customWorkspace, Default: true},
	})

	options := manager.WorkspaceOptionsForSession(Session{Name: "方案 讨论"})
	if len(options) != 2 {
		t.Fatalf("workspace options = %#v", options)
	}
	wantDefault := filepath.Join(workspaceRoot, "方案 讨论")
	if options[0].Label != "默认目录" || options[0].Value != wantDefault || !options[0].Default {
		t.Fatalf("default workspace option = %#v, want %q", options[0], wantDefault)
	}
	if options[1].Label != "现有项目" || options[1].Value != customWorkspace || options[1].Default {
		t.Fatalf("custom workspace option = %#v", options[1])
	}
}
