package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureClaudeStopHookPreservesSettingsAndIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(claudeDir, "settings.json")
	existing := `{
  "theme": "dark",
  "hooks": {
    "Stop": [{"hooks":[{"type":"command","command":"existing-stop","timeout":30}]}],
    "PostToolUse": [{"matcher":"Edit","hooks":[]}]
  }
}`
	if err := os.WriteFile(path, []byte(existing), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := EnsureClaudeStopHook("/old/iris"); err != nil {
		t.Fatal(err)
	}
	if err := EnsureClaudeStopHook("/new/iris"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(content, &root); err != nil {
		t.Fatal(err)
	}
	if root["theme"] != "dark" {
		t.Fatalf("settings were not preserved: %s", content)
	}
	hooks := root["hooks"].(map[string]any)
	if _, ok := hooks["PostToolUse"]; !ok {
		t.Fatalf("existing hooks were not preserved: %s", content)
	}
	stop := hooks["Stop"].([]any)
	managedCount := 0
	existingFound := false
	for _, group := range stop {
		for _, handler := range group.(map[string]any)["hooks"].([]any) {
			command, _ := handler.(map[string]any)["command"].(string)
			if command == "existing-stop" {
				existingFound = true
			}
			if strings.Contains(command, claudeStopModeFlag) {
				managedCount++
				if !strings.Contains(command, "/new/iris") {
					t.Fatalf("managed command was not updated: %q", command)
				}
			}
		}
	}
	if !existingFound || managedCount != 1 {
		t.Fatalf("Stop hooks existing=%v managed=%d config=%s", existingFound, managedCount, content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("settings mode = %v", info.Mode().Perm())
	}
}

func TestEnsureClaudeStopHookUsesCustomConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if err := EnsureClaudeStopHook("/opt/iris"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), claudeStopModeFlag) {
		t.Fatalf("managed hook missing: %s", content)
	}
}

func TestRunClaudeStopHookPostsOfficialPayload(t *testing.T) {
	payload := `{"session_id":"019f5153-6e7f-7742-9f61-3ffe1530d61c","hook_event_name":"Stop","last_assistant_message":"Claude 本轮最终回复","stop_hook_active":false}`
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/api/sessions/session-claude/hook/turn-ended" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("X-Iris-Agent-Token"); got != "token-claude" {
			t.Errorf("token = %q", got)
		}
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		if got["last_assistant_message"] != "Claude 本轮最终回复" {
			t.Errorf("payload = %#v", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("IRIS_API_URL", server.URL)
	t.Setenv("IRIS_SESSION_ID", "session-claude")
	t.Setenv("IRIS_SESSION_TOKEN", "token-claude")

	if err := RunClaudeStopHook(strings.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("Claude Stop callback was not invoked")
	}
}

func TestCompleteAgentTurnSupportsClaudeAndPinsResume(t *testing.T) {
	manager := NewManager(nil, nil)
	rt := &RuntimeSession{
		manager: manager,
		session: Session{
			ID:                     "sess-claude",
			Status:                 StatusRunning,
			Live:                   true,
			RecoveryKey:            "hook-token",
			LastMode:               SessionModeAgent,
			LastAgentKind:          "claude",
			LastAgentResumeCommand: "claude --continue --dangerously-skip-permissions",
		},
	}
	manager.sessions[rt.session.ID] = rt
	claudeSessionID := "019f5153-6e7f-7742-9f61-3ffe1530d61c"
	got, accepted, err := manager.CompleteAgentTurn(context.Background(), rt.session.ID, "hook-token", claudeSessionID, "完成")
	if err != nil || !accepted {
		t.Fatalf("Claude completion accepted=%v err=%v", accepted, err)
	}
	if got.Status != StatusWaiting || !rt.agentTurnHookVerified {
		t.Fatalf("Claude completion state = %#v", got)
	}
	resumeArgs := shellFields(got.LastAgentResumeCommand)
	if !containsAdjacentArgs(resumeArgs, "--resume", claudeSessionID) || slicesContain(resumeArgs, "--continue") {
		t.Fatalf("Claude resume command = %q", got.LastAgentResumeCommand)
	}
}

func containsAdjacentArgs(args []string, first, second string) bool {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == first && args[index+1] == second {
			return true
		}
	}
	return false
}

func slicesContain(args []string, value string) bool {
	for _, arg := range args {
		if arg == value {
			return true
		}
	}
	return false
}
