package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecoveryTracksCDChain(t *testing.T) {
	manager := NewManager(nil, nil)
	rt := &RuntimeSession{
		manager: manager,
		session: Session{ID: "sess-1", Name: "A", LastMode: SessionModeShell, LastCWD: "/tmp"},
	}

	rt.MarkStructuredInputActivity("cd project && cd ../other")

	if got := rt.Snapshot().LastCWD; got != "/tmp/other" {
		t.Fatalf("LastCWD = %q, want /tmp/other", got)
	}
}

func TestRecoveryRecordsCodexResumeCommandWithFlags(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	base := filepath.Join(t.TempDir(), "sessions")
	manager := NewManager(nil, nil, WithRecoveryBaseDir(base))
	rt := &RuntimeSession{
		manager: manager,
		session: Session{ID: "sess-1", Name: "A", RecoveryKey: "rk", LastMode: SessionModeShell, LastCWD: "/tmp"},
	}

	rt.MarkStructuredInputActivity("codex --dangerously-bypass-approvals-and-sandbox -m gpt-5.5")
	s := rt.Snapshot()

	if s.LastMode != SessionModeAgent || s.LastAgentKind != "codex" {
		t.Fatalf("agent state = mode %q kind %q", s.LastMode, s.LastAgentKind)
	}
	if !strings.Contains(s.LastAgentResumeCommand, "resume") || !strings.Contains(s.LastAgentResumeCommand, "--last") {
		t.Fatalf("resume command = %q, want codex resume --last", s.LastAgentResumeCommand)
	}
	if !strings.Contains(s.LastAgentResumeCommand, "--dangerously-bypass-approvals-and-sandbox") || !strings.Contains(s.LastAgentResumeCommand, "gpt-5.5") {
		t.Fatalf("resume command did not preserve flags: %q", s.LastAgentResumeCommand)
	}
	if s.LastAgentHome != filepath.Join(home, ".codex") {
		t.Fatalf("LastAgentHome = %q", s.LastAgentHome)
	}
}

func TestRecoveryRecordsClaudeResumeCommandWithHome(t *testing.T) {
	base := filepath.Join(t.TempDir(), "sessions")
	manager := NewManager(nil, nil, WithRecoveryBaseDir(base))
	rt := &RuntimeSession{
		manager: manager,
		session: Session{ID: "sess-1", Name: "A", RecoveryKey: "rk", LastMode: SessionModeShell, LastCWD: "/tmp"},
	}

	rt.MarkStructuredInputActivity("claude --dangerously-skip-permissions")
	s := rt.Snapshot()

	if s.LastMode != SessionModeAgent || s.LastAgentKind != "claude" {
		t.Fatalf("agent state = mode %q kind %q", s.LastMode, s.LastAgentKind)
	}
	if !strings.Contains(s.LastAgentResumeCommand, "--continue") {
		t.Fatalf("resume command = %q, want claude --continue", s.LastAgentResumeCommand)
	}
	if !strings.Contains(s.LastAgentResumeCommand, "--dangerously-skip-permissions") {
		t.Fatalf("resume command did not preserve flags: %q", s.LastAgentResumeCommand)
	}
	if !strings.HasSuffix(s.LastAgentHome, filepath.Join("rk", "claude_home")) {
		t.Fatalf("LastAgentHome = %q", s.LastAgentHome)
	}
}

func TestConfiguredCustomClaudeCommandKeepsClaudeRecoveryKind(t *testing.T) {
	manager := NewManager(nil, nil)
	rt := &RuntimeSession{
		manager: manager,
		session: Session{ID: "sess-1", Name: "A", RecoveryKey: "rk", LastMode: SessionModeShell, LastCWD: "/tmp"},
	}

	rt.ConfigureAgentForRecovery(AgentConfig{Kind: "custom", Command: "claude --dangerously-skip-permissions"})
	s := rt.Snapshot()
	if s.LastMode != SessionModeAgent || s.LastAgentKind != "claude" || !strings.Contains(s.LastAgentResumeCommand, "--continue") {
		t.Fatalf("configured Claude recovery state = %#v", s)
	}
}

func TestRecoveryBaseDirIsAbsolute(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	manager := NewManager(nil, nil, WithRecoveryBaseDir(filepath.Join("conf", "data", "sessions")))
	got := manager.sessionCodexHome(Session{RecoveryKey: "rk"})

	if got != filepath.Join(home, ".codex") {
		t.Fatalf("codex home = %q, want default Codex home", got)
	}
	got = manager.sessionClaudeHome(Session{RecoveryKey: "rk"})
	if !filepath.IsAbs(got) {
		t.Fatalf("claude home = %q, want absolute path", got)
	}
	if !strings.HasSuffix(got, filepath.Join("conf", "data", "sessions", "rk", "claude_home")) {
		t.Fatalf("claude home = %q, want recovery path suffix", got)
	}
}

func TestRecoveryEnvironmentExportsAgentHomes(t *testing.T) {
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher, WithRecoveryBaseDir(filepath.Join(t.TempDir(), "sessions")))

	if _, err := manager.CreateSession(context.Background(), "test"); err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	writes := launcher.terminals[0].writes()
	for _, want := range []string{"unset CODEX_HOME", "CLAUDE_CONFIG_DIR=", "claude_home"} {
		if !strings.Contains(writes, want) {
			t.Fatalf("recovery environment writes = %q, want %q", writes, want)
		}
	}
	if strings.Contains(writes, "export CODEX_HOME=") || strings.Contains(writes, "codex_home") {
		t.Fatalf("recovery environment must leave Codex on its default home, got %q", writes)
	}
}

func TestEnsureCodexNotifyPreservesExistingNotifyAndRemovesManagedStopHook(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(codexHome, "config.toml")
	config := "model = \"gpt-test\"\nnotify = [\"existing-notify\", \"turn-ended\"]\n\n[features]\nexample = true\n"
	if err := os.WriteFile(configPath, []byte(config), 0o640); err != nil {
		t.Fatal(err)
	}
	hooksPath := filepath.Join(codexHome, "hooks.json")
	hooks := map[string]any{"hooks": map[string]any{
		"Stop": []any{
			map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "existing-stop", "timeout": 30}}},
			map[string]any{"hooks": []any{map[string]any{"type": "command", "command": legacyEasyTerminalCodexStopHookCommand, "timeout": 5}}},
		},
		"PostToolUse": []any{map[string]any{"matcher": ".*", "hooks": []any{}}},
	}}
	hookData, err := json.Marshal(hooks)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath, hookData, 0o640); err != nil {
		t.Fatal(err)
	}

	if err := EnsureCodexNotify("/opt/iris"); err != nil {
		t.Fatal(err)
	}
	if err := EnsureCodexNotify("/opt/iris"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	_, _, notify, found, err := findTopLevelNotify(data)
	if err != nil || !found {
		t.Fatalf("managed notify missing: found=%v err=%v config=%s", found, err, data)
	}
	if len(notify) != 4 || notify[0] != "/opt/iris" || notify[1] != codexNotifyModeFlag {
		t.Fatalf("managed notify = %#v", notify)
	}
	forward, err := managedCodexNotifyForward(notify)
	if err != nil || len(forward) != 2 || forward[0] != "existing-notify" || forward[1] != "turn-ended" {
		t.Fatalf("forward notify = %#v, err=%v", forward, err)
	}
	if strings.Count(string(data), "notify =") != 1 || !strings.Contains(string(data), "[features]") {
		t.Fatalf("config was not preserved idempotently: %s", data)
	}
	hookData, err = os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(hookData), "/hook/turn-ended") || !strings.Contains(string(hookData), "existing-stop") || !strings.Contains(string(hookData), "PostToolUse") {
		t.Fatalf("legacy hook migration was not selective: %s", hookData)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("config.toml mode = %v", info.Mode().Perm())
	}
}

func TestPrepareCodexRecoveryMigratesAndPinsLegacyRollout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacyHome := filepath.Join(t.TempDir(), "codex_home")
	rel := filepath.Join("2026", "07", "11", "rollout-2026-07-11T21-17-30-019f5153-6e7f-7742-9f61-3ffe1530d61c.jsonl")
	rollout := filepath.Join(legacyHome, "sessions", rel)
	if err := os.MkdirAll(filepath.Dir(rollout), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"type":"session_meta","payload":{"session_id":"019f5153-6e7f-7742-9f61-3ffe1530d61c","thread_source":"user"}}` + "\n"
	if err := os.WriteFile(rollout, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	sess, err := NewManager(nil, nil).prepareCodexRecovery(Session{
		LastAgentKind:          "codex",
		LastAgentHome:          legacyHome,
		LastAgentResumeCommand: "codex resume --last --dangerously-bypass-approvals-and-sandbox",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sess.LastAgentHome != filepath.Join(home, ".codex") || strings.Contains(sess.LastAgentResumeCommand, "--last") ||
		!strings.Contains(sess.LastAgentResumeCommand, "019f5153-6e7f-7742-9f61-3ffe1530d61c") {
		t.Fatalf("legacy recovery was not migrated and pinned: %#v", sess)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "sessions", rel)); err != nil {
		t.Fatalf("migrated rollout missing: %v", err)
	}
}

func TestEnsureClaudeSessionHomeLinksSharedConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	source := filepath.Join(home, ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	if err := os.MkdirAll(filepath.Join(source, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"userID":"u1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "settings.json"), []byte(`{"theme":"dark"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "claude_home")

	if err := ensureClaudeSessionHome(target); err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{"sessions", "projects"} {
		if info, err := os.Stat(filepath.Join(target, dir)); err != nil || !info.IsDir() {
			t.Fatalf("%s dir stat=%v err=%v", dir, info, err)
		}
	}
	for name, want := range map[string]string{
		".claude.json":  filepath.Join(home, ".claude.json"),
		"settings.json": filepath.Join(source, "settings.json"),
		"plugins":       filepath.Join(source, "plugins"),
	} {
		got, err := os.Readlink(filepath.Join(target, name))
		if err != nil {
			t.Fatalf("Readlink(%s) error = %v", name, err)
		}
		if got != want {
			t.Fatalf("link %s = %q, want %q", name, got, want)
		}
	}
}

func TestPinClaudeResumeCommand(t *testing.T) {
	sessionID := "019f5153-6e7f-7742-9f61-3ffe1530d61c"
	for _, test := range []struct {
		name    string
		command string
	}{
		{name: "continue", command: "claude --continue --dangerously-skip-permissions"},
		{name: "short continue", command: "claude -c --model sonnet"},
		{name: "existing resume", command: "claude --resume 019e440b-54a7-7200-8ca0-fe9e9e87d4be --effort high"},
		{name: "resume equals", command: "CLAUDE_CONFIG_DIR=/tmp/claude claude --resume=019e440b-54a7-7200-8ca0-fe9e9e87d4be"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := pinClaudeResumeCommand(test.command, sessionID)
			args := shellFields(got)
			if !ok || !containsAdjacentArgs(args, "--resume", sessionID) || slicesContain(args, "--continue") || slicesContain(args, "-c") {
				t.Fatalf("pinClaudeResumeCommand(%q) = %q, %v", test.command, got, ok)
			}
		})
	}
}

func TestRecoveryDoesNotDuplicateCodexResume(t *testing.T) {
	manager := NewManager(nil, nil)
	rt := &RuntimeSession{
		manager: manager,
		session: Session{ID: "sess-1", Name: "A", LastMode: SessionModeShell, LastCWD: "/tmp"},
	}

	rt.MarkStructuredInputActivity("codex resume 019e440b-54a7-7200-8ca0-fe9e9e87d4be --dangerously-bypass-approvals-and-sandbox")
	got := rt.Snapshot().LastAgentResumeCommand

	if strings.Count(got, "resume") != 1 {
		t.Fatalf("resume command = %q, want single resume", got)
	}
	if strings.Contains(got, "--last") {
		t.Fatalf("explicit resume command should not add --last: %q", got)
	}
}

func TestRecoverRuntimeRestoresAgentCommand(t *testing.T) {
	launcher := &recordingLauncher{}
	st := newMemoryStore()
	manager := NewManager(st, launcher)
	now := time.Now().UTC()
	sess := Session{
		ID:                     "sess-1",
		Name:                   "A",
		Status:                 StatusWaiting,
		CreatedAt:              now,
		UpdatedAt:              now,
		Live:                   true,
		LastMode:               SessionModeAgent,
		LastCWD:                "/tmp/project",
		LastAgentKind:          "codex",
		LastAgentResumeCommand: "codex resume --last --dangerously-bypass-approvals-and-sandbox",
	}
	if err := st.CreateSession(context.Background(), sess); err != nil {
		t.Fatal(err)
	}

	rt, _, ok, err := manager.RecoverRuntime(context.Background(), sess.ID)
	if err != nil || !ok || rt == nil {
		t.Fatalf("RecoverRuntime ok=%v err=%v rt=%v", ok, err, rt)
	}

	writes := launcher.terminals[0].writes()
	if !strings.Contains(writes, "cd '/tmp/project'\r") || !strings.Contains(writes, "codex resume --last --dangerously-bypass-approvals-and-sandbox\r") {
		t.Fatalf("recovery writes = %q", writes)
	}
}

func TestRecoveryEnvironmentExportsAgentTurnHookCredentials(t *testing.T) {
	terminal := &recordingTerminal{readCh: make(chan []byte)}
	manager := NewManager(nil, nil, WithAgentTurnHookURL("http://127.0.0.1:8001/"))
	rt := &RuntimeSession{
		manager:  manager,
		terminal: terminal,
		session:  Session{ID: "sess-9", RecoveryKey: "secret-token"},
	}

	rt.runRecoveryEnvironmentSetup()
	writes := terminal.writes()
	for _, want := range []string{
		"IRIS_API_URL='http://127.0.0.1:8001'",
		"IRIS_SESSION_ID='sess-9'",
		"IRIS_SESSION_TOKEN='secret-token'",
	} {
		if !strings.Contains(writes, want) {
			t.Fatalf("hook environment missing %q from %q", want, writes)
		}
	}
}

type memoryStore struct {
	sessions map[string]Session
	contacts map[string]LarkContactBinding
}

func newMemoryStore() *memoryStore {
	return &memoryStore{sessions: make(map[string]Session), contacts: make(map[string]LarkContactBinding)}
}

func (s *memoryStore) CreateSession(_ context.Context, sess Session) error {
	s.sessions[sess.ID] = sess
	return nil
}

func (s *memoryStore) UpdateSession(_ context.Context, sess Session) error {
	s.sessions[sess.ID] = sess
	return nil
}

func (s *memoryStore) ListSessions(context.Context) ([]Session, error) {
	out := make([]Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, sess)
	}
	return out, nil
}

func (s *memoryStore) GetSession(_ context.Context, id string) (Session, bool, error) {
	sess, ok := s.sessions[id]
	return sess, ok, nil
}

func (s *memoryStore) DeleteSession(_ context.Context, id string) error {
	delete(s.sessions, id)
	return nil
}

func (s *memoryStore) AppendOutput(context.Context, string, int64, []byte) error { return nil }
func (s *memoryStore) Output(context.Context, string) ([]byte, error)            { return nil, nil }
func (s *memoryStore) DeleteAllSessions(context.Context) error                   { return nil }
func (s *memoryStore) ListQuickCommands(context.Context) ([]QuickCommand, error) { return nil, nil }
func (s *memoryStore) CreateQuickCommand(context.Context, QuickCommand) error    { return nil }
func (s *memoryStore) DeleteQuickCommand(context.Context, string) error          { return nil }
func (s *memoryStore) GetLarkContactBinding(_ context.Context, senderOpenID string) (LarkContactBinding, bool, error) {
	binding, ok := s.contacts[senderOpenID]
	return binding, ok, nil
}
func (s *memoryStore) UpsertLarkContactBinding(_ context.Context, binding LarkContactBinding) error {
	s.contacts[binding.SenderOpenID] = binding
	return nil
}
func (s *memoryStore) DeactivateLarkContactBinding(_ context.Context, senderOpenID string) error {
	binding := s.contacts[senderOpenID]
	binding.Active = false
	s.contacts[senderOpenID] = binding
	return nil
}
