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
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	workspace := t.TempDir()
	manager.SetAgentConfig(AgentConfig{Kind: "codex", Command: "codex --dangerously-bypass-approvals-and-sandbox"}, []WorkspaceOption{
		{Label: "主项目", Value: workspace, Default: true},
	})

	sess, err := manager.CreateSession(context.Background(), "Iris")
	if err != nil {
		t.Fatal(err)
	}
	writes := launcher.terminals[0].writes()
	abs, _ := filepath.Abs(workspace)
	if !strings.Contains(writes, "cd "+shellQuote(abs)+"\r") || !strings.Contains(writes, "codex --dangerously-bypass-approvals-and-sandbox\r") {
		t.Fatalf("configured workspace and Agent were not started: %q", writes)
	}
	if sess.LastMode != SessionModeAgent || sess.LastAgentKind != "codex" || sess.LastCWD != abs {
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
