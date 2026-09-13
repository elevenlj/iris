package session

import (
	"context"
	"os"
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
	workspace := filepath.Join(t.TempDir(), "my workspace")
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	manager.SetDefaultWorkspaceDir(workspace)
	customWorkspace := t.TempDir()
	manager.SetAgentConfig(AgentConfig{Kind: "codex", Command: CodexAgentCommand}, []WorkspaceOption{
		{Label: "主项目", Value: customWorkspace, Default: true},
	})

	sess, err := manager.CreateSession(context.Background(), "Iris")
	if err != nil {
		t.Fatal(err)
	}
	writes := launcher.terminals[0].writes()
	if !strings.Contains(writes, "mkdir -p "+shellQuote(workspace)+"\r") || !strings.Contains(writes, "cd "+shellQuote(workspace)+"\r") || !strings.Contains(writes, CodexAgentCommand+"\r") {
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

func TestWorkspaceOptionsForSessionAlwaysStartsWithSharedDefault(t *testing.T) {
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
	wantDefault := filepath.Join(workspaceRoot, defaultWorkspaceDirName)
	if options[0].Label != "默认目录" || options[0].Value != wantDefault || !options[0].Default {
		t.Fatalf("default workspace option = %#v, want %q", options[0], wantDefault)
	}
	if options[1].Label != "现有项目" || options[1].Value != customWorkspace || options[1].Default {
		t.Fatalf("custom workspace option = %#v", options[1])
	}
}

func TestSwitchWorkspaceSubmitsAgentSpecificInput(t *testing.T) {
	for _, test := range []struct {
		name        string
		configKind  string
		runtimeKind string
		command     string
		wantPrefix  string
	}{
		{name: "Codex", configKind: "codex", runtimeKind: "codex", command: CodexAgentCommand, wantPrefix: "/cd "},
		{name: "Claude Code", configKind: "claude", runtimeKind: "claude", command: ClaudeAgentCommand, wantPrefix: "后续任务切换到工作目录："},
		{name: "Aiden", configKind: "aiden", runtimeKind: "aiden", command: AidenAgentCommand, wantPrefix: "/cd "},
		{name: "Aiden X Codex", configKind: "aiden-codex", runtimeKind: "codex", command: AidenCodexAgentCommand, wantPrefix: "/cd "},
		{name: "Aiden X Claude Code", configKind: "aiden-claude", runtimeKind: "claude", command: AidenClaudeAgentCommand, wantPrefix: "后续任务切换到工作目录："},
		{name: "Custom Aiden X Claude", configKind: "custom", runtimeKind: "custom", command: "FOO=bar aiden x claude", wantPrefix: "后续任务切换到工作目录："},
		{name: "Custom Claude", configKind: "custom", runtimeKind: "custom", command: "/usr/local/bin/claude --dangerously-skip-permissions", wantPrefix: "后续任务切换到工作目录："},
		{name: "Claude wrapper", configKind: "claude", runtimeKind: "claude", command: "my-claude-wrapper", wantPrefix: "后续任务切换到工作目录："},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := filepath.Join(t.TempDir(), "项目 with spaces")
			if err := os.Mkdir(workspace, 0700); err != nil {
				t.Fatal(err)
			}
			terminal := &recordingTerminal{readCh: make(chan []byte)}
			manager := NewManager(nil, nil)
			manager.SetAgentConfig(AgentConfig{Kind: test.configKind, Command: test.command}, []WorkspaceOption{{Label: "项目", Value: workspace}})
			rt := &RuntimeSession{
				manager:  manager,
				terminal: terminal,
				session:  Session{ID: "sess-workspace", Status: StatusWaiting, Live: true, LastMode: SessionModeAgent, LastAgentKind: test.runtimeKind, LastAgentStartCommand: test.command},
			}
			manager.sessions[rt.session.ID] = rt

			got, ok, err := manager.SwitchWorkspace(context.Background(), rt.session.ID, workspace)
			if err != nil || !ok || got.LastCWD != workspace {
				t.Fatalf("SwitchWorkspace() ok=%v err=%v session=%#v", ok, err, got)
			}
			if writes := terminal.writes(); !strings.Contains(writes, test.wantPrefix+workspace+"\r") || (test.wantPrefix != "/cd " && strings.Contains(writes, "/cd ")) {
				t.Fatalf("workspace input = %q", writes)
			}
		})
	}
}

func TestCreateSessionStartsAidenBuiltins(t *testing.T) {
	for _, test := range []struct {
		kind        string
		command     string
		runtimeKind string
	}{
		{kind: "aiden", command: AidenAgentCommand, runtimeKind: "aiden"},
		{kind: "aiden-codex", command: AidenCodexAgentCommand, runtimeKind: "codex"},
		{kind: "aiden-claude", command: AidenClaudeAgentCommand, runtimeKind: "claude"},
	} {
		t.Run(test.kind, func(t *testing.T) {
			launcher := &recordingLauncher{}
			manager := NewManager(nil, launcher)
			manager.SetAgentConfig(AgentConfig{Kind: test.kind}, nil)

			sess, err := manager.CreateSession(context.Background(), "test")
			if err != nil {
				t.Fatal(err)
			}
			writes := launcher.terminals[0].writes()
			if !strings.Contains(writes, test.command+"\r") {
				t.Fatalf("startup writes = %q, want %q", writes, test.command)
			}
			if sess.LastAgentKind != test.runtimeKind || sess.LastAgentID != test.kind {
				t.Fatalf("startup session = %#v", sess)
			}
		})
	}
}
