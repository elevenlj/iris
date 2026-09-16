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

func TestCompletionCardMentionsOnlyAfterCompletionAcrossRestart(t *testing.T) {
	for _, senderType := range []string{"app", "user", ""} {
		t.Run(senderType, func(t *testing.T) {
			m := NewManager(nil, nil, WithIsolatedMessageRegistry())
			m.messageRegistry().rememberInput("original", senderType)
			rt := &RuntimeSession{manager: m, session: Session{ID: "session", LarkChatID: "chat"}}
			transport := &completionCardHTTPClient{}
			client := newLarkReplyAPIClient("card-test", "secret", lark.WithHttpClient(transport))
			n := NewLarkAppNotifier("card-test", "secret", "developer", true)
			n.client, n.cardsPath = client, filepath.Join(t.TempDir(), "cards.json")
			note := rt.decorateWaitingNotification(WaitingNotification{SessionID: "session", ChatID: "chat", MessageID: "card", InputMessageID: "original", MentionOpenID: "sender", Running: true, Content: RunningNotificationPlaceholder})
			if _, err := n.NotifyWaiting(note); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(transport.lastCard, "sender") {
				t.Fatal("running card mentioned sender")
			}
			// Removing a running marker or manually refreshing is not a completion event.
			if err := n.UpdateWaitingRunning(note, false); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(transport.lastCard, "sender") {
				t.Fatal("waiting marker mentioned sender before completion")
			}
			note.Running, note.Completed, note.UpdateNo = false, true, 1
			note.Content = "回答正文 <at id=chosen></at>"
			if _, err := n.NotifyWaiting(note); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(transport.lastCard, "sender") != (senderType != "app") || !strings.Contains(transport.lastCard, "chosen") {
				t.Fatalf("incorrect completion mentions: %s", transport.lastCard)
			}
			// A later input or a refresh by someone else must not replace the owner.
			restarted := NewLarkAppNotifier("card-test", "secret", "developer", true)
			restarted.client, restarted.cardsPath = client, n.cardsPath
			note.InputMessageID, note.MentionOpenID, note.BotInput, note.Completed = "next-input", "other-user", false, false
			if err := restarted.UpdateWaitingRunning(note, false); err != nil {
				t.Fatal(err)
			}
			if _, err := restarted.NotifyWaiting(note); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(transport.lastCard, "sender") != (senderType != "app") || strings.Contains(transport.lastCard, "other-user") || !strings.Contains(transport.lastCard, "chosen") {
				t.Fatalf("refresh changed mentions: %s", transport.lastCard)
			}
			if len(transport.posts) != 0 {
				t.Fatalf("completion sent extra messages: %#v", transport.posts)
			}
		})
	}
}

func TestCardCreationRepliesToInputWithinTopic(t *testing.T) {
	for _, tc := range []struct{ name, input, root, path string }{
		{"group", "input", "", "/im/v1/messages/input/reply"},
		{"topic", "topic-input", "topic-root", "/im/v1/messages/topic-input/reply"},
		{"topic without input", "", "topic-root", "/im/v1/messages/topic-root/reply"},
		{"local input", "", "", "/im/v1/messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &completionCardHTTPClient{}
			previous := http.DefaultTransport
			http.DefaultTransport = cardTestTransport(transport.Do)
			defer func() { http.DefaultTransport = previous }()
			n := NewLarkAppNotifier("quote-card", "secret", "developer", true)
			note := WaitingNotification{SessionID: "session", ChatID: "chat", InputMessageID: tc.input, TopicRootID: tc.root, Running: true}
			if _, err := n.createWaiting(note, `{"elements":[]}`); err != nil {
				t.Fatal(err)
			}
			if len(transport.posts) != 1 {
				t.Fatalf("posts=%#v", transport.posts)
			}
			post := transport.posts[0]
			if post.path != "/open-apis"+tc.path || post.body["msg_type"] != "interactive" {
				t.Fatalf("wrong card target: %#v", post)
			}
			if tc.input != "" || tc.root != "" {
				if post.body["reply_in_thread"] != (tc.root != "") || post.body["receive_id"] != nil {
					t.Fatalf("wrong reply scope: %#v", post)
				}
			} else if post.body["receive_id"] != "chat" {
				t.Fatalf("local card lost chat: %#v", post)
			}
		})
	}
}

type completionCardHTTPClient struct {
	posts []struct {
		path string
		body map[string]interface{}
	}
	lastCard string
}

func (c *completionCardHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Path, "/im/v1/messages") {
		var body map[string]interface{}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		if req.Method == http.MethodPost {
			c.posts = append(c.posts, struct {
				path string
				body map[string]interface{}
			}{req.URL.Path, body})
		}
		if req.Method == http.MethodPatch {
			c.lastCard, _ = body["content"].(string)
		}
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"code":0,"tenant_access_token":"test-token","expire":7200,"data":{"message_id":"card-id"}}`))}, nil
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
