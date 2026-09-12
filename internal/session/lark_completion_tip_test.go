package session

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	lark "github.com/larksuite/oapi-sdk-go/v3"
)

type completionTipHTTPClient struct {
	posts []struct {
		path string
		body map[string]interface{}
	}
}

func (c *completionTipHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/im/v1/messages") {
		var body map[string]interface{}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		c.posts = append(c.posts, struct {
			path string
			body map[string]interface{}
		}{req.URL.Path, body})
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"code":0,"tenant_access_token":"test-token","expire":7200,"data":{"message_id":"tip-id"}}`))}, nil
}

func TestCompletionTipRepliesWithTextAndKeepsOriginalAcrossRestart(t *testing.T) {
	for _, inputID := range []string{"input-first", ""} {
		t.Run("source="+inputID, func(t *testing.T) {
			transport := &completionTipHTTPClient{}
			client := lark.NewClient("completion-app", "secret", lark.WithHttpClient(transport))
			n := NewLarkAppNotifier("completion-app", "secret", "developer", false)
			n.client = client
			n.cardsPath = filepath.Join(t.TempDir(), "cards.json")
			note := WaitingNotification{SessionID: "session-1", ChatID: "chat-1", MessageID: "answer-card", InputMessageID: inputID, Running: true, Content: RunningNotificationPlaceholder}
			state := n.cardState()
			state.latest["session-1\x00chat-1"] = note
			n.persistCards(state)

			// A queued input and running-state refresh must not replace this card's source.
			note.InputMessageID = "input-second"
			if err := n.UpdateWaitingRunning(note, false); err != nil {
				t.Fatal(err)
			}
			restarted := NewLarkAppNotifier("completion-app", "secret", "developer", false)
			restarted.client, restarted.cardsPath = client, n.cardsPath
			note.Running, note.Content, note.UpdateNo = false, "回答正文", 1
			for i := 0; i < 2; i++ {
				result, err := restarted.NotifyWaiting(note)
				if err != nil || !result.Updated || !result.TipSent {
					t.Fatalf("completion = %#v, %v", result, err)
				}
			}
			if len(transport.posts) != 1 {
				t.Fatalf("completion posts = %#v", transport.posts)
			}
			post := transport.posts[0]
			if inputID != "" {
				if !strings.HasSuffix(post.path, "/"+inputID+"/reply") || post.body["reply_in_thread"] != false {
					t.Fatalf("quoted completion = %#v", post)
				}
			} else if !strings.HasSuffix(post.path, "/messages") || post.body["receive_id"] != "chat-1" {
				t.Fatalf("unquoted completion = %#v", post)
			}
			if post.body["msg_type"] != "text" || post.body["content"] != `{"text":"任务已完成"}` {
				t.Fatalf("completion should be plain text, got %#v", post.body)
			}
			uuid, _ := post.body["uuid"].(string)
			if len(uuid) != 32 {
				t.Fatalf("missing idempotency key: %q", uuid)
			}
			// Retrying across notifier instances uses the same server-side idempotency key.
			if err := n.sendUpdateTip(note); err != nil {
				t.Fatal(err)
			}
			if transport.posts[1].body["uuid"] != uuid {
				t.Fatal("idempotency key changed across restart")
			}
		})
	}
}

func TestStructuredInputSourceIsClearedByLocalTerminalInput(t *testing.T) {
	rt := &RuntimeSession{manager: NewManager(nil, nil), session: Session{ID: "session-1", Live: true}}
	rt.markStructuredInputActivityWithPreviousRoundState("飞书问题", false, "input-first")
	if rt.notificationInputMessageID != "input-first" {
		t.Fatal("structured input lost source")
	}
	rt.MarkInputActivity("终端问题\r")
	if rt.notificationInputMessageID != "" {
		t.Fatal("local input retained stale Feishu source")
	}
}
