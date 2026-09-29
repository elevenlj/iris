package session

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAidenCodexUsesPrivateConfigWithoutForbiddenFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	source := filepath.Join(defaultCodexHome(), "config.toml")
	original := "model = \"user-model\"\nnotify = [\"computer-use\", \"turn-ended\"]\n[features]\nweb_search = true\n"
	if err := writeFileAtomically(source, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(nil, nil, WithAgentTurnHookURL("http://127.0.0.1:8080/bots/b"))
	m.recoveryBaseDir = t.TempDir()
	m.SetSystemPrompt("bot instructions")
	var previousHome string
	for _, command := range []string{AidenCodexAgentCommand, "FOO=bar '/usr/local/bin/aiden' x codex resume exact-thread --no-alt-screen"} {
		rt := &RuntimeSession{manager: m, session: Session{ID: "session-b", RecoveryKey: "secret-b"}}
		launch, err := rt.agentLaunchCommand(command)
		if err != nil || strings.Contains(launch, " -c ") || strings.Contains(launch, "secret-b") || rt.pendingSystemPrompt != "bot instructions" {
			t.Fatalf("invalid launch %q: %v", launch, err)
		}
		out, err := exec.Command("sh", "-c", "set -- "+strings.TrimPrefix(launch, "FOO=bar ")+`; printf '%s\000' "$@"`).Output()
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
		home := strings.TrimPrefix(args[len(args)-1], "CODEX_HOME=")
		if args[len(args)-2] != "--env" || home == defaultCodexHome() || (previousHome != "" && previousHome != home) {
			t.Fatalf("incorrect config home: %q", args)
		}
		previousHome = home
		content, err := os.ReadFile(filepath.Join(home, "config.toml"))
		if err != nil || !strings.Contains(string(content), "--route-file") || !strings.Contains(string(content), codexNotifyForwardFlag) || !strings.Contains(string(content), "[features]") {
			t.Fatalf("lost config or notify: %s, %v", content, err)
		}
		if info, err := os.Stat(filepath.Join(home, "config.toml")); err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("config not private")
		}
		shared, err := os.Readlink(filepath.Join(home, "sessions"))
		if err != nil || shared != filepath.Join(defaultCodexHome(), "sessions") {
			t.Fatalf("lost existing history: %q %v", shared, err)
		}
	}
	content, _ := os.ReadFile(source)
	if string(content) != original {
		t.Fatal("overwrote global configuration")
	}
	rt := &RuntimeSession{manager: m, session: Session{ID: "session-c", RecoveryKey: "secret-c"}}
	launch, err := rt.agentLaunchCommand(AidenCodexAgentCommand)
	if err != nil || strings.Contains(launch, previousHome) {
		t.Fatal("two sessions shared their config")
	}
}

func TestNotifyRouteOverridesSharedDaemonEnvironment(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TRAE_HOME", t.TempDir())
	t.Setenv("IRIS_API_URL", "http://127.0.0.1:1")
	t.Setenv("IRIS_SESSION_ID", "wrong-session")
	t.Setenv("IRIS_SESSION_TOKEN", "wrong-token")
	config := filepath.Join(defaultCodexHome(), "config.toml")
	if err := writeFileAtomically(config, []byte("notify = [\"computer-use\", \"turn-ended\"]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/bots/bot-b/api/sessions/session-b/hook/turn-ended" || r.Header.Get("X-Iris-Agent-Token") != "secret-b" {
			t.Errorf("wrong callback route: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	m := NewManager(nil, nil, WithAgentTurnHookURL(server.URL+"/bots/bot-b"))
	m.recoveryBaseDir = t.TempDir()
	rt := &RuntimeSession{manager: m, session: Session{ID: "session-b", RecoveryKey: "secret-b"}}
	for _, command := range []string{CodexAgentCommand, "codex resume exact-thread", "traecli --yolo"} {
		launch, err := rt.agentNotifyLaunchCommand(command)
		if err != nil || !strings.Contains(launch, "--route-file") || strings.Contains(launch, "secret-b") {
			t.Fatalf("launch = %q, err = %v", launch, err)
		}
		if command != "traecli --yolo" && !strings.Contains(launch, codexNotifyForwardFlag) {
			t.Fatal("lost existing downstream notify")
		}
	}
	path := filepath.Join(m.recoveryBaseDir, "notify-routes", fmt.Sprintf("%x.json", sha256.Sum256([]byte("secret-b"))))
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private route permissions: %v, %v", info, err)
	}
	payload := `{"type":"agent-turn-complete","thread-id":"thread-b","turn-id":"turn-b","last-assistant-message":"done"}`
	if err := RunCodexNotify([]string{"--route-file", path, payload}); err != nil || !called {
		t.Fatalf("callback failed: called=%v err=%v", called, err)
	}
	for _, command := range []string{ClaudeAgentCommand, AidenAgentCommand, "custom-agent"} {
		launch, err := rt.agentNotifyLaunchCommand(command)
		if err != nil || launch != command {
			t.Fatalf("changed unrelated agent: %q, %v", launch, err)
		}
	}
	data, _ := json.Marshal(agentNotifyRoute{URL: server.URL})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := RunCodexNotify([]string{"--route-file", path, payload}); err == nil {
		t.Fatal("accepted incomplete route")
	}
}
