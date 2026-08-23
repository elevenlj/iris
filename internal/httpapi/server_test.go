package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"easy_terminal/internal/session"
)

type httpTestLarkProvider struct{}

func (httpTestLarkProvider) LarkChatMetadata(context.Context, string) (session.LarkChatMetadata, error) {
	return session.LarkChatMetadata{ChatName: "当前绑定群", ChatType: "group"}, nil
}

func (httpTestLarkProvider) LarkChatMessages(context.Context, string, int) ([]session.LarkChatMessage, error) {
	return []session.LarkChatMessage{{MessageID: "om_latest", Text: "群里的最新讨论"}}, nil
}

func TestEmbeddedStaticAssetsDisableStaleBrowserCaching(t *testing.T) {
	server := NewServer(nil, "")

	for _, path := range []string{"/", "/app.js", "/app.css"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, path, nil)

			server.Handler().ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want %d", path, recorder.Code, http.StatusOK)
			}
			if got, want := recorder.Header().Get("Cache-Control"), "no-store, max-age=0, must-revalidate"; got != want {
				t.Fatalf("GET %s Cache-Control = %q, want %q", path, got, want)
			}
			if got, want := recorder.Header().Get("Pragma"), "no-cache"; got != want {
				t.Fatalf("GET %s Pragma = %q, want %q", path, got, want)
			}
			if got, want := recorder.Header().Get("Expires"), "0"; got != want {
				t.Fatalf("GET %s Expires = %q, want %q", path, got, want)
			}
		})
	}
}

func TestAgentStopHookAcceptsLastAssistantMessage(t *testing.T) {
	terminal := newWSBridgeTestTerminal()
	manager := session.NewManager(nil, wsBridgeTestLauncher{terminal: terminal})
	sess, err := manager.CreateSession(context.Background(), "hook-content")
	if err != nil {
		t.Fatal(err)
	}
	rt, ok := manager.GetRuntime(sess.ID)
	if !ok {
		t.Fatal("runtime session missing")
	}
	defer rt.Close()
	rt.RecordShellCommandForRecovery("codex --dangerously-bypass-approvals-and-sandbox")

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+sess.ID+"/hook/turn-ended", strings.NewReader(`{
		"session_id":"019f5153-6e7f-7742-9f61-3ffe1530d61c",
		"last_assistant_message":"本轮 Hook 最终回复"
	}`))
	req.Header.Set("X-Easy-Terminal-Hook-Token", sess.RecoveryKey)
	rec := httptest.NewRecorder()
	NewServer(manager, "").Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("hook status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rt.CachedCurrentRoundContent(); got != "本轮 Hook 最终回复" {
		t.Fatalf("hook content = %q", got)
	}
}

func TestAgentLarkContextEndpointsUseSessionTokenAndBoundChat(t *testing.T) {
	terminal := newWSBridgeTestTerminal()
	manager := session.NewManager(nil, wsBridgeTestLauncher{terminal: terminal})
	sess, err := manager.CreateSession(context.Background(), "context-api")
	if err != nil {
		t.Fatal(err)
	}
	rt, _ := manager.GetRuntime(sess.ID)
	defer rt.Close()
	if _, ok, err := manager.BindLarkChat(context.Background(), sess.ID, "oc_bound"); err != nil || !ok {
		t.Fatalf("BindLarkChat() ok=%v err=%v", ok, err)
	}
	manager.SetLarkConversationProvider(httpTestLarkProvider{})
	manager.RecordLarkAgentContext(sess.ID, session.LarkAgentContext{LatestMessageID: "om_latest"})
	server := NewServer(manager, "").Handler()

	unauthorized := httptest.NewRecorder()
	server.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/sessions/"+sess.ID+"/lark/context", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/api/sessions/"+sess.ID+"/lark/messages?limit=25", nil)
	request.Header.Set("Authorization", "Bearer "+sess.RecoveryKey)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("messages status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var page session.LarkChatMessagePage
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Context.ChatID != "oc_bound" || page.Context.ChatName != "当前绑定群" || page.Count != 1 || page.Messages[0].Text != "群里的最新讨论" {
		t.Fatalf("unexpected messages response: %#v", page)
	}
}
