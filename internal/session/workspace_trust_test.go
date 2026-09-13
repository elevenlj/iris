package session

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

func TestWorkspaceTrustPathsIncludeRepositoryRoot(t *testing.T) {
	repo := t.TempDir()
	if output, err := exec.Command("git", "init", "--quiet", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	subdir := filepath.Join(repo, "工作 空间")
	if err := os.Mkdir(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	paths := workspaceTrustPaths(subdir)
	realRoot, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(paths, subdir) || !slices.Contains(paths, realRoot) {
		t.Fatalf("missing workspace/repository trust: %v", paths)
	}
	if paths := workspaceTrustPaths(""); len(paths) != 0 {
		t.Fatalf("empty cwd trusts %v", paths)
	}
}

// Opt-in smoke test: starts the real CLI in an empty directory, sends no task,
// and uses a private config copy. Regular CI needs no account or installed CLI.
func TestClaudeWorkspaceTrustRealCLI(t *testing.T) {
	if os.Getenv("IRIS_TRUST_CLI_E2E") != "1" {
		t.Skip("opt-in real CLI check")
	}
	home, cwd := t.TempDir(), t.TempDir()
	if err := ensureClaudeSessionHome(home); err != nil {
		t.Fatal(err)
	}
	if err := ensureClaudeWorkspaceTrust(home, workspaceTrustPaths(cwd)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", "--dangerously-skip-permissions")
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+home, "IRIS_API_URL=", "IRIS_SESSION_ID=", "IRIS_SESSION_TOKEN=", "EASY_TERMINAL_HOOK_URL=", "TERM=xterm-256color")
	term, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 36, Cols: 120})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); term.Close(); cmd.Wait() }()
	output := ""
	buf := make([]byte, 8192)
	for {
		n, err := term.Read(buf)
		output += string(buf[:n])
		text := strings.ToLower(StripTerminalControls([]byte(output)))
		compact := strings.Join(strings.Fields(text), "")
		if strings.Contains(compact, "yes,itrustthisfolder") {
			t.Fatal("still blocked by folder trust")
		}
		if strings.Contains(compact, "bypasspermissionson") && strings.Contains(text, "❯") {
			return
		}
		if err != nil {
			t.Fatalf("CLI did not reach composer: %v, tail: %s", err, text[max(0, len(text)-1500):])
		}
	}
}

func TestClaudeWorkspaceTrustIsPrivateAndPreservesConfiguration(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), ".claude.json")
	original := `{"userID":"existing-user","unknown":9007199254740993,"projects":{"/old":{"allowedTools":["Read"]},"/new":{"other":true}}}`
	if err := os.WriteFile(source, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude.json")
	if err := os.Symlink(source, path); err != nil {
		t.Skip(err)
	}
	for _, paths := range [][]string{{"/new", "/工作 空间"}, {"/another"}} {
		if err := ensureClaudeWorkspaceTrust(home, paths); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(path)
	var config struct {
		UserID   string
		Unknown  json.Number
		Projects map[string]struct {
			HasTrustDialogAccepted bool
			Other                  bool
			AllowedTools           []string
		}
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if config.UserID != "existing-user" || config.Unknown.String() != "9007199254740993" || !config.Projects["/new"].Other || len(config.Projects["/old"].AllowedTools) != 1 {
		t.Fatalf("lost unrelated configuration: %s", data)
	}
	for _, cwd := range []string{"/new", "/工作 空间", "/another"} {
		if !config.Projects[cwd].HasTrustDialogAccepted {
			t.Fatalf("directory not trusted: %s", cwd)
		}
	}
	unchanged, _ := os.ReadFile(source)
	if string(unchanged) != original {
		t.Fatal("changed global configuration through symlink")
	}
	info, _ := os.Lstat(path)
	if info.Mode().Perm() != 0600 || info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("session config is not private")
	}
	for _, broken := range []string{"{broken", "null", `{"projects":[]}`} {
		os.WriteFile(path, []byte(broken), 0600)
		if err := ensureClaudeWorkspaceTrust(home, []string{"/new"}); err == nil {
			t.Fatalf("accepted malformed config: %s", broken)
		}
		data, _ := os.ReadFile(path)
		if string(data) != broken {
			t.Fatal("overwrote malformed config")
		}
	}
}

func TestAgentLaunchWorkspaceTrust(t *testing.T) {
	cwd := t.TempDir()
	for _, command := range []string{CodexAgentCommand, AidenCodexAgentCommand, "codex resume exact-session", ClaudeAgentCommand, AidenClaudeAgentCommand, AidenAgentCommand, "custom-agent"} {
		t.Run(command, func(t *testing.T) {
			m := NewManager(nil, nil)
			m.recoveryBaseDir = t.TempDir()
			rt := &RuntimeSession{manager: m, session: Session{LastCWD: cwd, RecoveryKey: "test"}}
			rt.prepareAgentWorkspaceTrust(command)
			switch agentKindForCommand(command, "custom") {
			case "claude":
				data, err := os.ReadFile(filepath.Join(m.sessionClaudeHome(rt.session), ".claude.json"))
				if err != nil || !strings.Contains(string(data), `"hasTrustDialogAccepted": true`) {
					t.Fatalf("missing session trust: %s, %v", data, err)
				}
			default:
				if _, err := os.Stat(m.sessionClaudeHome(rt.session)); !os.IsNotExist(err) {
					t.Fatal("created config for unrelated agent")
				}
			}
		})
	}
}

func TestWorkspaceTrustMenuSafetyAndSelection(t *testing.T) {
	claude := "Accessing workspace: /tmp/project\nQuick safety check: Is this a project you created or one you trust?\n❯ No, exit\n  Yes, I trust this folder\nEnter to confirm · Esc to cancel"
	source := strings.Replace(aidenReadySource, "cursor_line=-1", "cursor_line=2", 1)
	for _, tc := range []struct{ name, snapshot, source, kind, key string }{
		{"Claude select", claude, source, "claude", "\x1b[B"},
		{"Claude confirm", strings.ReplaceAll(claude, "❯ No, exit\n  Yes, I trust this folder", "  No, exit\n❯ Yes, I trust this folder"), strings.Replace(source, "cursor_line=2", "cursor_line=3", 1), "claude", "\r"},
		{"numbered", strings.ReplaceAll(claude, "❯ No, exit\n  Yes, I trust this folder", "  1. Yes, I trust this folder\n❯ 2. No, exit"), strings.Replace(source, "cursor_line=2", "cursor_line=3", 1), "claude", "\x1b[A"},
		{"Codex", "Do you trust the contents of this directory?\nWorking with untrusted contents comes with higher risk of prompt injection.\n› 1. Yes, continue\n  2. No, quit\nPress enter to continue", source, "codex", "\r"},
		{"wrong agent", claude, source, "custom", ""},
		{"DOM", claude, "browser:dom", "claude", ""},
		{"missing metadata", claude, "headless:buffer", "claude", ""},
		{"shell below", claude + "\n➜ project git:(main) ✗", source, "claude", ""},
		{"composer below", claude + "\n❯ ", strings.Replace(source, "cursor_line=2", "cursor_line=5", 1), "claude", ""},
		{"unrelated approval", strings.ReplaceAll(claude, "Accessing workspace:", "Allow command in:"), source, "claude", ""},
		{"partial menu", strings.TrimSuffix(claude, "Enter to confirm · Esc to cancel"), source, "claude", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, ok := workspaceTrustMenu(tc.snapshot, tc.source, tc.kind)
			if key != tc.key || ok != (tc.key != "") {
				t.Fatalf("got %q, %v", key, ok)
			}
			if ok && startupAgentComposerReady(tc.snapshot, tc.source, tc.kind) {
				t.Fatal("menu released queued input")
			}
		})
	}
	for _, command := range []string{ClaudeAgentCommand, AidenClaudeAgentCommand} {
		term := &recordingTerminal{}
		rt := &RuntimeSession{terminal: term, session: Session{Live: true, LastMode: SessionModeAgent, LastAgentStartCommand: command}, visibleSnapshot: claude, visibleSnapshotSource: source}
		rt.autoTrustWorkspaceLocked()
		rt.autoTrustWorkspaceLocked()
		if term.writes() != "\x1b[B" {
			t.Fatalf("duplicate navigation: %q", term.writes())
		}
		rt.visibleSnapshot = strings.ReplaceAll(claude, "❯ No, exit\n  Yes, I trust this folder", "  No, exit\n❯ Yes, I trust this folder")
		rt.visibleSnapshotSource = strings.Replace(source, "cursor_line=2", "cursor_line=3", 1)
		rt.autoTrustWorkspaceLocked()
		rt.autoTrustWorkspaceLocked()
		if term.writes() != "\x1b[B\r" {
			t.Fatalf("duplicate confirmation: %q", term.writes())
		}
		rt.visibleSnapshot = "❯ "
		rt.visibleSnapshotSource = strings.Replace(source, "cursor_line=2", "cursor_line=0", 1)
		rt.autoTrustWorkspaceLocked()
		if rt.workspaceTrustAction != "" {
			t.Fatal("next directory still blocked")
		}
		rt.visibleSnapshot, rt.visibleSnapshotSource = claude, source
		rt.closed = true
		rt.autoTrustWorkspaceLocked()
		if term.writes() != "\x1b[B\r" {
			t.Fatal("acted on closed runtime")
		}
	}
}

func TestWorkspaceTrustWithoutNotifications(t *testing.T) {
	for _, command := range []string{ClaudeAgentCommand, AidenClaudeAgentCommand, CodexAgentCommand, AidenCodexAgentCommand} {
		t.Run(command, func(t *testing.T) {
			term := &recordingTerminal{readCh: make(chan []byte)}
			rt := &RuntimeSession{manager: NewManager(nil, nil), terminal: term,
				session: Session{ID: "trust-test", Live: true, LastMode: SessionModeAgent, LastAgentStartCommand: command},
			}
			subscriber, cancel := rt.Subscribe()
			defer cancel()
			defer rt.Close()
			kind := agentKindForCommand(command, "custom")
			go func() {
				for event := range subscriber {
					if event.Type != RuntimeEventSnapshotRequest {
						continue
					}
					snapshot, cursor := "OpenAI Codex\n› ", "1"
					if !strings.HasSuffix(term.writes(), "\r") {
						if kind == "codex" {
							snapshot = "> You are in /tmp/test\nDo you trust the contents of this directory?\n› 1. Yes, continue\n  2. No, quit\nPress enter to continue"
							cursor = "4" // Hardware cursor is parked at the footer.
						} else if term.writes() == "" {
							snapshot = "Accessing workspace: /tmp/test\n❯ No, exit\n  Yes, I trust this folder\nEnter to confirm · Esc to cancel"
						} else {
							snapshot = "Accessing workspace: /tmp/test\n  No, exit\n❯ Yes, I trust this folder\nEnter to confirm · Esc to cancel"
							cursor = "2"
						}
					}
					source := strings.Replace(aidenReadySource, "headless:", "browser:", 1)
					source = strings.Replace(source, "cursor_line=-1", "cursor_line="+cursor, 1)
					rt.SetVisibleSnapshotResponseFrom(snapshot, source, event.RequestID, subscriber)
				}
			}()
			// Split the trigger across PTY reads as can happen on a real terminal.
			rt.HandleOutput([]byte("Do you tru"))
			rt.HandleOutput([]byte("st this folder?"))
			waitForTerminalText(t, term, "\r", 3*time.Second)
			want := "\r"
			if kind == "claude" {
				want = "\x1b[B\r"
			}
			if term.writes() != want {
				t.Fatalf("trust keys = %q, want %q", term.writes(), want)
			}
		})
	}
}
