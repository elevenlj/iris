package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestLarkRegistrationPostFormReturnsOAuthPendingBodyOnHTTP400(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
	}))
	defer server.Close()

	client := &larkAppRegistrationClient{httpClient: server.Client()}
	got, err := client.postForm(context.Background(), server.URL, url.Values{"action": {"poll"}})
	if err != nil {
		t.Fatal(err)
	}
	if stringField(got, "error") != "authorization_pending" {
		t.Fatalf("error = %q", stringField(got, "error"))
	}
}

func TestLarkRegistrationBeginFormRequestsMessageAndCardCapabilities(t *testing.T) {
	form := larkRegistrationBeginForm()
	scope := form.Get("scope")
	for _, want := range []string{
		"im:message",
		"im:message:send_as_bot",
		"im:message.p2p_msg:readonly",
		"im:message.group_msg",
		"im:message.group_msg:readonly",
		"im:message.group_at_msg:readonly",
		"im:message.group_at_msg.include_bot:readonly",
		"im:message:readonly",
		"im:message:update",
		"im:message.reactions:read",
		"im:message.reactions:write_only",
		"im:resource",
		"im:chat:create",
		"im:chat:read",
		"im:chat:update",
		"im:chat.members:read",
		"im:chat.members:write_only",
		"im:chat.members:bot_access",
		"cardkit:card:read",
		"cardkit:card:write",
		"contact:user.base:readonly",
	} {
		if !strings.Contains(scope, want) {
			t.Fatalf("scope %q should contain %q", scope, want)
		}
	}
	events := form.Get("events")
	for _, want := range []string{
		"im.chat.member.bot.added_v1",
		"im.message.receive_v1",
		"im.message.message_read_v1",
		"im.message.reaction.created_v1",
		"im.message.reaction.deleted_v1",
	} {
		if !strings.Contains(events, want) {
			t.Fatalf("events %q should contain %q", events, want)
		}
	}
	if form.Get("callbacks") != "card.action.trigger" {
		t.Fatalf("callbacks = %q", form.Get("callbacks"))
	}
}

func TestLarkRegistrationVerificationURLUsesDefaultTemplate(t *testing.T) {
	got := larkRegistrationVerificationURL("https://open.feishu.cn", "ABCD-EFGH")
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/page/cli" {
		t.Fatalf("path = %q", parsed.Path)
	}
	if parsed.Query().Get("user_code") != "ABCD-EFGH" {
		t.Fatalf("user_code = %q", parsed.Query().Get("user_code"))
	}
	if parsed.Query().Get("tp") != larkRegistrationTemplateID {
		t.Fatalf("tp = %q", parsed.Query().Get("tp"))
	}
}
