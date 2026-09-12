package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	lark "github.com/larksuite/oapi-sdk-go/v3"
)

type cardTestTransport func(*http.Request) (*http.Response, error)

func (f cardTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBotMessageRegistriesAreIsolated(t *testing.T) {
	a := NewManager(nil, nil, WithIsolatedMessageRegistry())
	b := NewManager(nil, nil, WithIsolatedMessageRegistry())
	a.messageRegistry().remember("sess-1", "message-a")
	a.messageRegistry().rememberChat("chat-a", "sess-1")
	if _, ok := b.messageRegistry().lookup("message-a"); ok {
		t.Fatal("message crossed bot boundary")
	}
	if _, ok := b.messageRegistry().lookupChat("chat-a"); ok {
		t.Fatal("chat crossed bot boundary")
	}
	if b.messageRegistry().latestNotifiedSessionID() != "" {
		t.Fatal("fallback crossed bot boundary")
	}
	n := NewLarkAppNotifier("a", "s", "owner", false)
	a.SetNotifier(n)
	if n.messageRegistry() != a.messageRegistry() {
		t.Fatal("notifier registry not scoped")
	}
}

func TestNextCardRetiresOnlyPreviousSuccessfulCard(t *testing.T) {
	created := 0
	failCreate := false
	missingID := false
	patches := map[string]string{}
	transport := cardTestTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"code":0}`
		if strings.Contains(r.URL.Path, "tenant_access_token") {
			body = `{"code":0,"tenant_access_token":"test-token","expire":7200}`
		} else if r.Method == http.MethodPost && r.URL.Path == "/open-apis/im/v1/messages" {
			if missingID {
				body = `{"code":0,"data":{}}`
			} else if failCreate {
				body = `{"code":999,"msg":"failed"}`
			} else {
				created++
				body = fmt.Sprintf(`{"code":0,"data":{"message_id":"msg-%d"}}`, created)
			}
		} else if r.Method == http.MethodPatch {
			var payload struct {
				Content string `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			patches[r.URL.Path] = payload.Content
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewBufferString(body))}, nil
	})
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = previous }()
	n := NewLarkAppNotifier("app-a", "secret", "owner", false)
	n.registry = &LarkMessageRegistry{}
	n.cardsPath = filepath.Join(t.TempDir(), "cards.json")
	n.client = lark.NewClient("app-a", "secret", lark.WithHttpClient(&http.Client{Transport: transport}))
	note := WaitingNotification{SessionID: "sess-1", ChatID: "chat", Name: "A", Content: "Original answer", DeveloperModeEnabled: true, WorkspaceOptions: []WorkspaceOption{{Label: "work", Value: "/tmp"}}, AgentOptions: []AgentOption{{ID: "codex", Label: "Codex", Kind: "codex"}}}
	first, err := n.NotifyWaiting(note)
	if err != nil {
		t.Fatal(err)
	}
	failCreate = true
	if _, err := n.NotifyWaiting(note); err == nil {
		t.Fatal("failed creation reported success")
	}
	if len(patches) != 0 {
		t.Fatal("old controls hidden before new card succeeded")
	}
	failCreate = false
	missingID = true
	if _, err := n.NotifyWaiting(note); err == nil || len(patches) != 0 {
		t.Fatal("missing ID treated as successful delivery")
	}
	missingID = false
	// Simulate an Iris restart before the next successful task card.
	n.cards = nil
	other := note
	other.SessionID = "sess-2"
	if _, err := n.NotifyWaiting(other); err != nil {
		t.Fatal(err)
	}
	if len(patches) != 0 {
		t.Fatal("another session retired current card")
	}
	note.Content = "New answer"
	if _, err := n.NotifyWaiting(note); err != nil {
		t.Fatal(err)
	}
	old := patches["/open-apis/im/v1/messages/"+first.MessageID]
	if !strings.Contains(old, "Original answer") {
		t.Fatal("old answer lost")
	}
	for _, tag := range []string{`"tag":"button"`, `"tag":"select_static"`, `"tag":"form"`} {
		if strings.Contains(old, tag) {
			t.Fatalf("old card contains %s", tag)
		}
	}
	if _, err := n.NotifyWaiting(other); err != nil {
		t.Fatal(err)
	}
	note.MessageID = first.MessageID
	if err := n.UpdateWaitingRunning(note, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(patches["/open-apis/im/v1/messages/"+first.MessageID], `"tag":"button"`) {
		t.Fatal("late update restored old controls")
	}
	if _, err := n.NotifyWaiting(note); err != nil {
		t.Fatal(err)
	}
	if n.messageRegistry().latestNotifiedSessionID() != other.SessionID {
		t.Fatal("retired card update changed the latest session fallback")
	}
}

func TestDisabledStartupCardHasNoWorkspaceSelector(t *testing.T) {
	note := WaitingNotification{SessionID: "sess-1", Content: "Started", Startup: true, StartupComplete: true, Disabled: true, DeveloperModeEnabled: true, WorkspaceOptions: []WorkspaceOption{{Label: "work", Value: "/tmp"}}}
	card, err := larkNotificationCardContent(note, "owner", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(card, `"tag":"select_static"`) || strings.Contains(card, `"tag":"button"`) {
		t.Fatal("startup controls remain")
	}
}

func TestNextCardRecallsOnlyUnfinishedPlaceholder(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		note   WaitingNotification
		recall bool
	}{
		{"running", WaitingNotification{Content: RunningNotificationPlaceholder, Running: true}, true},
		{"marker cleared", WaitingNotification{Content: RunningNotificationPlaceholder}, true},
		{"answer still running", WaitingNotification{Content: "Keep this answer", Running: true}, false},
		{"completed answer", WaitingNotification{Content: "Keep this answer", UpdateNo: 1}, false},
		{"answer matching placeholder", WaitingNotification{Content: RunningNotificationPlaceholder, UpdateNo: 1}, false},
		{"startup", WaitingNotification{Content: RunningNotificationPlaceholder, Startup: true}, false},
		{"final on freeze", WaitingNotification{Content: RunningNotificationPlaceholder, Running: true}, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			created, deletes, patches := 0, 0, 0
			failCreate, failRecall := false, true
			transport := cardTestTransport(func(r *http.Request) (*http.Response, error) {
				body := `{"code":0}`
				switch {
				case strings.Contains(r.URL.Path, "tenant_access_token"):
					body = `{"code":0,"tenant_access_token":"recall-test-token","expire":7200}`
				case r.Method == http.MethodPost && r.URL.Path == "/open-apis/im/v1/messages":
					if failCreate {
						body = `{"code":999,"msg":"failed"}`
					} else {
						created++
						body = fmt.Sprintf(`{"code":0,"data":{"message_id":"recall-%d"}}`, created)
					}
				case r.Method == http.MethodDelete:
					deletes++
					if r.URL.Path != "/open-apis/im/v1/messages/recall-1" {
						t.Errorf("recalled wrong card: %s", r.URL.Path)
					}
					if failRecall {
						body = `{"code":999,"msg":"failed"}`
					} else if scenario.name != "running" {
						body = `{"code":230011,"msg":"The message is recalled."}`
					}
				case r.Method == http.MethodPatch:
					patches++
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			original := http.DefaultTransport
			http.DefaultTransport = transport
			defer func() { http.DefaultTransport = original }()
			n := NewLarkAppNotifier("recall-app", "secret", "owner", false)
			n.client = lark.NewClient("recall-app", "secret", lark.WithHttpClient(&http.Client{Transport: transport}))
			n.registry = &LarkMessageRegistry{}
			n.cardsPath = filepath.Join(t.TempDir(), "cards.json")
			note := scenario.note
			note.SessionID, note.ChatID = "session", "chat"
			first, err := n.NotifyWaiting(note)
			if err != nil {
				t.Fatal(err)
			}
			next := WaitingNotification{SessionID: "session", ChatID: "chat", Content: RunningNotificationPlaceholder, Running: true}
			failCreate = true
			if _, err := n.NotifyWaiting(next); err == nil {
				t.Fatal("failed new card reported success")
			}
			if deletes != 0 || patches != 0 {
				t.Fatal("old card changed before new card arrived")
			}
			failCreate = false
			if scenario.name == "final on freeze" {
				frozen := note
				frozen.MessageID, frozen.Content, frozen.Disabled, frozen.UpdateNo = first.MessageID, "Answer delivered while freezing", true, 1
				if _, err := n.NotifyWaiting(frozen); err != nil {
					t.Fatal(err)
				}
				patches = 0
			}
			n.cards = nil // Restore placeholder state across a restart.
			second, err := n.NotifyWaiting(next)
			if err != nil {
				t.Fatal("retirement failure must not resend the new card:", err)
			}
			if (deletes > 0) != scenario.recall {
				t.Fatalf("deletes=%d recall=%v", deletes, scenario.recall)
			}
			if !scenario.recall {
				if patches != 1 {
					t.Fatal("answer/startup was not retained")
				}
				return
			}
			if patches != 0 || len(n.cardState().pending) != 1 {
				t.Fatal("failed recall must stay pending without patching the placeholder")
			}
			n.cards = nil
			failRecall = false
			next.MessageID, next.Content, next.Running = second.MessageID, "New answer", false
			if _, err := n.NotifyWaiting(next); err != nil {
				t.Fatal(err)
			}
			if len(n.cardState().pending) != 0 {
				t.Fatal("pending recall not retried after restart")
			}
			before := patches
			note.MessageID = first.MessageID
			if _, err := n.NotifyWaiting(note); err != nil {
				t.Fatal(err)
			}
			if err := n.UpdateWaitingRunning(note, true); err != nil {
				t.Fatal(err)
			}
			if patches != before || created != 2 {
				t.Fatal("late update wrote to a recalled card or duplicated a message")
			}
		})
	}
}
