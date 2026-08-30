package session

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEnsureCodexNotifyMigratesLegacyWrapper(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(codexHome, "config.toml")
	content := "notify = [\n  '/old/scripts/codex-turn-ended-hook.sh',\n  \"computer-use\",\n  \"turn-ended\", # preserved\n]\n\n[features]\nexample = true\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureCodexNotify("/new/iris"); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _, command, found, err := findTopLevelNotify(updated)
	if err != nil || !found {
		t.Fatalf("notify missing: found=%v err=%v config=%s", found, err, updated)
	}
	forward, err := managedCodexNotifyForward(command)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(forward, []string{"computer-use", "turn-ended"}) {
		t.Fatalf("forward = %#v", forward)
	}
	if strings.Contains(string(updated), "codex-turn-ended-hook.sh") || !strings.Contains(string(updated), "[features]") {
		t.Fatalf("legacy wrapper was not migrated cleanly: %s", updated)
	}
}

func TestEnsureCodexNotifyCreatesConfigAndUpdatesExecutable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := EnsureCodexNotify("/old/iris"); err != nil {
		t.Fatal(err)
	}
	if err := EnsureCodexNotify("/new/iris"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, command, found, err := findTopLevelNotify(content)
	if err != nil || !found {
		t.Fatalf("notify missing: found=%v err=%v config=%s", found, err, content)
	}
	if !reflect.DeepEqual(command, []string{"/new/iris", codexNotifyModeFlag}) {
		t.Fatalf("notify = %#v", command)
	}
}

func TestEnsureCodexNotifyRemovesRecursivePreviousNotify(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	computerUse := []string{"computer-use", "turn-ended"}
	previous, err := managedCodexNotify("/old/iris", computerUse)
	if err != nil {
		t.Fatal(err)
	}
	previousJSON, err := json.Marshal(previous)
	if err != nil {
		t.Fatal(err)
	}
	current, err := managedCodexNotify("/current/iris", append(computerUse, "--previous-notify", string(previousJSON)))
	if err != nil {
		t.Fatal(err)
	}
	content, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(codexHome, "config.toml")
	if err := os.WriteFile(path, append(append([]byte("notify = "), content...), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := EnsureCodexNotify("/new/iris"); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _, command, found, err := findTopLevelNotify(updated)
	if err != nil || !found {
		t.Fatalf("notify missing: found=%v err=%v config=%s", found, err, updated)
	}
	forward, err := managedCodexNotifyForward(command)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(forward, computerUse) {
		t.Fatalf("forward = %#v", forward)
	}
}

func TestRunCodexNotifyPostsOfficialPayload(t *testing.T) {
	payload := `{"type":"agent-turn-complete","thread-id":"thread-1","turn-id":"turn-2","last-assistant-message":"本轮最终回复"}`
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/api/sessions/session-9/hook/turn-ended" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("X-Iris-Agent-Token"); got != "token-9" {
			t.Errorf("token = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != payload {
			t.Errorf("payload = %q", body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("IRIS_API_URL", server.URL)
	t.Setenv("IRIS_SESSION_ID", "session-9")
	t.Setenv("IRIS_SESSION_TOKEN", "token-9")

	if err := RunCodexNotify([]string{payload}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("notify callback was not invoked")
	}
}

func TestRunCodexNotifyIgnoresOtherEvents(t *testing.T) {
	t.Setenv("IRIS_API_URL", "http://127.0.0.1:1")
	t.Setenv("IRIS_SESSION_ID", "session-9")
	t.Setenv("IRIS_SESSION_TOKEN", "token-9")
	if err := RunCodexNotify([]string{`{"type":"approval-requested"}`}); err != nil {
		t.Fatal(err)
	}
}

func TestRunCodexNotifyIgnoresTitleGeneration(t *testing.T) {
	t.Setenv("IRIS_API_URL", "http://127.0.0.1:1")
	t.Setenv("IRIS_SESSION_ID", "session-9")
	t.Setenv("IRIS_SESSION_TOKEN", "token-9")
	payload := `{"type":"agent-turn-complete","thread-id":"thread-title","last-assistant-message":"{\"title\":\"排查未完成任务的异常推送\"}"}`
	if err := RunCodexNotify([]string{payload}); err != nil {
		t.Fatal(err)
	}
}
