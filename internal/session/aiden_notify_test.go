package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testAidenSessionID = "2f9c5f44-3899-4297-8764-4526f32d8a2a"
const testAidenCheckpointID = "1f1af083-b2e3-6840-8002-6330fbd21701"

func writeAidenCheckpointFixture(t *testing.T, dir, threadID string, last map[string]any) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]any{
		"latest.json": map[string]any{"latest": testAidenCheckpointID},
		testAidenCheckpointID + ".json": map[string]any{
			"config": map[string]any{"configurable": map[string]any{"thread_id": threadID}},
			"checkpoint": map[string]any{"id": testAidenCheckpointID, "channel_values": map[string]any{
				"messages": []any{map[string]any{"type": "ai", "content": "Previous round; never forward"}, map[string]any{"type": "human", "content": "hi"}, last},
			}},
		},
	}
	for name, value := range files {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAidenStopHookEnrichesReplyAndReplacesTerminalFallback(t *testing.T) {
	dir := filepath.Join(t.TempDir(), testAidenSessionID)
	writeAidenCheckpointFixture(t, dir, testAidenSessionID, map[string]any{"type": "ai", "content": []any{
		map[string]any{"type": "thinking", "text": "private reasoning"},
		map[string]any{"type": "text", "text": "Hi! What can I help you with?"},
	}, "tool_calls": []any{}})
	notifier := &recordingNotifier{}
	m := NewManager(nil, nil, WithNotifier(notifier))
	rt := &RuntimeSession{manager: m, session: Session{ID: "iris-aiden", Live: true, Status: StatusWaiting, NotifyOnWaiting: true,
		LastMode: SessionModeAgent, LastAgentKind: "aiden", LastAgentResumeCommand: AidenAgentCommand, RecoveryKey: "test-token"},
		lastNotifiedMessageID: "answer-card", lastNotifiedContent: "Read AGENTS.md\nMCP error", notificationInputMessageID: "input-hi",
		visibleSnapshot: "Read AGENTS.md\nHi!\nMCP error", visibleSnapshotSource: "headless:buffer"}
	m.sessions[rt.session.ID] = rt
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event struct {
			SessionID string `json:"session_id"`
			Content   string `json:"last_assistant_message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Error(err)
			return
		}
		if event.Content != "Hi! What can I help you with?" {
			t.Errorf("reply = %q", event.Content)
		}
		if _, _, err := m.CompleteAgentTurn(r.Context(), rt.session.ID, r.Header.Get("X-Iris-Agent-Token"), event.SessionID, event.Content); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("IRIS_API_URL", server.URL)
	t.Setenv("IRIS_SESSION_ID", rt.session.ID)
	t.Setenv("IRIS_SESSION_TOKEN", "test-token")
	payload, _ := json.Marshal(map[string]any{"hook_event_name": "Stop", "session_id": testAidenSessionID, "transcript_path": dir, "stop_hook_active": false})
	if err := RunClaudeStopHook(strings.NewReader(string(payload))); err != nil {
		t.Fatal(err)
	}
	notes := waitForNotifierNotes(t, notifier, 1)
	if len(notes) != 1 || notes[0].Content != "Hi! What can I help you with?" || notes[0].SnapshotSource != "aiden_hook:last_assistant_message" || notes[0].MessageID != "answer-card" || notes[0].InputMessageID != "input-hi" {
		t.Fatalf("completion = %#v", notes)
	}
	if err := RunClaudeStopHook(strings.NewReader(string(payload))); err != nil {
		t.Fatal(err)
	}
	if notifier.count() != 1 {
		t.Fatal("duplicate Stop sent another answer")
	}
}

func TestAidenCheckpointRejectsWrongSessionAndNonFinalMessages(t *testing.T) {
	for _, test := range []struct {
		name     string
		last     map[string]any
		threadID string
		want     string
	}{
		{"string", map[string]any{"type": "ai", "content": "final"}, testAidenSessionID, "final"},
		{"new user", map[string]any{"type": "human", "content": "new question"}, testAidenSessionID, ""},
		{"tool result", map[string]any{"type": "tool", "content": "tool output"}, testAidenSessionID, ""},
		{"tool call", map[string]any{"type": "ai", "content": "working", "tool_calls": []any{map[string]any{"name": "Bash"}}}, testAidenSessionID, ""},
		{"empty reply", map[string]any{"type": "ai", "content": ""}, testAidenSessionID, ""},
		{"wrong session", map[string]any{"type": "ai", "content": "other session"}, testAidenCheckpointID, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), testAidenSessionID)
			writeAidenCheckpointFixture(t, dir, test.threadID, test.last)
			got, err := aidenCheckpointAssistantMessage(dir, testAidenSessionID)
			if test.want == "" && err == nil || test.want != "" && (err != nil || got != test.want) {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
	dir := filepath.Join(t.TempDir(), testAidenSessionID)
	writeAidenCheckpointFixture(t, dir, testAidenSessionID, map[string]any{"type": "ai", "content": "reply"})
	if _, err := aidenCheckpointAssistantMessage(dir, testAidenCheckpointID); err == nil {
		t.Fatal("mismatched directory accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "latest.json"), []byte(`{"latest":"../other"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := aidenCheckpointAssistantMessage(dir, testAidenSessionID); err == nil {
		t.Fatal("checkpoint path traversal accepted")
	}
}
