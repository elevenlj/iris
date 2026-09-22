package session

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTraeIntegration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TRAE_HOME", t.TempDir())
	options := detectAvailableAgentOptions(nil, fakeAgentExecutableFinder{"traecli": true})
	if len(options) != 1 || options[0].Command != "traecli --yolo" || options[0].Kind != "traecli" {
		t.Fatalf("options = %#v", options)
	}
	for _, command := range []string{TraeAgentCommand, "/opt/bin/traecli --yolo", "traex --yolo"} {
		info, ok := agentLaunchInfo(shellFields(command))
		if !ok || info.Kind != "traecli" || !containsAdjacentArgs(shellFields(info.ResumeCommand), "resume", "--last") {
			t.Fatalf("launch %q: %#v, %v", command, info, ok)
		}
	}
	for _, command := range []string{"traecli --version", "traecli login", "traecli models", "traecli exec hi", "traecli --acp", "traecli dashboard"} {
		if _, ok := agentLaunchInfo(shellFields(command)); ok {
			t.Fatalf("non-interactive command recognized: %s", command)
		}
	}
	path := filepath.Join(defaultTraeHome(), "traecli.toml")
	original := "notify = [\"previous-hook\"]\nmodel = \"my-model\"\n[features]\nexample = true\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := EnsureTraeNotify("/opt/iris"); err != nil {
			t.Fatal(err)
		}
	}
	config, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _, notify, found, err := findTopLevelNotify(config)
	if err != nil || !found || !isManagedCodexNotify(notify) {
		t.Fatalf("notify = %v, err=%v", notify, err)
	}
	forward, err := managedCodexNotifyForward(notify)
	if err != nil || !reflect.DeepEqual(forward, []string{"previous-hook"}) || !strings.Contains(string(config), "model = \"my-model\"") {
		t.Fatal("existing configuration not preserved")
	}
	if _, err := os.Stat(filepath.Join(defaultCodexHome(), "config.toml")); !os.IsNotExist(err) {
		t.Fatal("TRAE installer touched Codex configuration")
	}

	m := NewManager(nil, nil)
	rt := &RuntimeSession{manager: m, session: Session{ID: "trae", Live: true,
		Status: StatusRunning, RecoveryKey: "token", LastMode: SessionModeAgent,
		LastAgentKind: "traecli", LastAgentStartCommand: TraeAgentCommand,
		LastAgentResumeCommand: "traecli resume --last --yolo"}}
	m.sessions["trae"] = rt
	defer rt.Close()
	if _, accepted, err := m.CompleteAgentTurn(context.Background(), "trae", "wrong", "", "bad"); err == nil || accepted {
		t.Fatal("accepted invalid callback token")
	}
	thread := "01a0c73d-600f-7343-8c84-3f9cbb15317f"
	sess, accepted, err := m.CompleteAgentTurn(context.Background(), "trae", "token", thread, "TRAE final reply")
	if err != nil || !accepted || sess.Status != StatusWaiting || sess.LastAgentHome != defaultTraeHome() {
		t.Fatalf("completion: %#v %v %v", sess, accepted, err)
	}
	if !reflect.DeepEqual(shellFields(exactAgentResumeCommand(sess)), []string{"traecli", "resume", thread, "--yolo"}) {
		t.Fatalf("resume must use exact TRAE session: %q", sess.LastAgentResumeCommand)
	}
	rt.mu.Lock()
	if rt.hookLastAssistantMessage != "TRAE final reply" || !rt.hookCompletedCurrentRound {
		t.Error("final reply not passed to event-based notification")
	}
	rt.mu.Unlock()
	if recovered, err := m.prepareCodexRecovery(sess); err != nil || recovered.LastAgentHome != defaultTraeHome() {
		t.Fatal("TRAE recovery incorrectly migrated to Codex home")
	}
	if recovered := normalizeTraeRecovery(sess); recovered.LastAgentResumeCommand != sess.LastAgentResumeCommand {
		t.Fatal("lost exact resume command")
	}
	legacy := sess
	legacy.LastAgentKind, legacy.LastAgentResumeCommand = "custom", "traecli resume --last --yolo"
	if recovered := normalizeTraeRecovery(legacy); recovered.LastAgentKind != "traecli" || recovered.LastAgentResumeCommand != TraeAgentCommand {
		t.Fatal("old custom TRAE session not migrated safely")
	}
	rt.mu.Lock()
	note, _, send, reason := rt.waitingNotificationCandidateLocked()
	rt.mu.Unlock()
	if !send || note.Content != "TRAE final reply" {
		t.Fatalf("notification did not use final event: %q %s", note.Content, reason)
	}
	rt.HandleOutput([]byte("terminal repaint"))
	if rt.Snapshot().Status != StatusWaiting {
		t.Fatal("terminal repaint reopened completed turn")
	}
}

func TestTraeStartupComposer(t *testing.T) {
	// TRAE CLI 0.205.1 --yolo, captured from a real PTY and rendered with xterm.
	screen := "TraeCode CLI (v0.205.1)\npermissions: YOLO mode\n\n────────────────────\n❯ Explain this codebase\n────────────────────\ngpt-5.5 medium · Context 100% left · /tmp/work ☢ Full Access (shift+tab to cycle)"
	source := strings.Replace(aidenReadySource, "cursor_line=-1", "cursor_line=4", 1)
	if !startupAgentComposerReady(screen, source, "traecli") {
		t.Fatal("TRAE composer not recognized")
	}
	if startupAgentComposerReady("TraeCode CLI (v0.205.1)\nLoading...", source, "traecli") {
		t.Fatal("welcome banner mistaken for ready composer")
	}
	if startupAgentComposerReady("Select model\n❯ 1. model-a\n  2. model-b\nEnter to select · Esc to cancel", source, "traecli") {
		t.Fatal("model selector mistaken for ready composer")
	}
}
