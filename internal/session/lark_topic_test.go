package session

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	lark "github.com/larksuite/oapi-sdk-go/v3"
)

type topicHTTPClient struct {
	mu       sync.Mutex
	requests []string
}

func (c *topicHTTPClient) Do(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data := `{"code":0,"tenant_access_token":"topic-token","expire":7200,"data":{}}`
	if strings.HasSuffix(req.URL.Path, "/reply") {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		if body["reply_in_thread"] != true {
			panic("topic reply escaped to group")
		}
		root := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/open-apis/im/v1/messages/"), "/reply")
		data = `{"code":0,"data":{"message_id":"reply-` + root + `","root_id":"` + root + `","thread_id":"omt-` + root + `"}}`
	}
	c.requests = append(c.requests, req.URL.String())
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(data))}, nil
}

func TestTopicCommandsCreateIsolatedInheritedSessions(t *testing.T) {
	ctx := context.Background()
	launcher := &recordingLauncher{}
	st := newMemoryStore()
	notifier := &recordingNotifier{messageIDs: []string{"startup-one", "startup-two", "running-one", "running-two"}}
	m := NewManager(st, launcher, WithIsolatedMessageRegistry(), WithNotifier(notifier))
	b := NewLarkReplyBridge("topic-app", "secret", m, t.TempDir())
	b.apiClient = newLarkReplyAPIClient("topic-app", "secret", lark.WithHttpClient(&topicHTTPClient{}))
	b.fetchChatMetadata = func(context.Context, string) (LarkChatMetadata, error) {
		return LarkChatMetadata{ChatName: "研发群"}, nil
	}
	b.replyText = func(context.Context, string, string) error { return nil }
	main, err := m.CreateSession(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	defer m.DeleteSession(ctx, main.ID)
	rt, _ := m.GetRuntime(main.ID)
	rt.RecordShellCommandForRecovery("cd '/tmp/current project'")
	rt.ConfigureAgentForRecovery(AgentConfig{Kind: "codex"})
	if _, _, err := m.BindLarkChat(ctx, main.ID, "oc-group"); err != nil {
		t.Fatal(err)
	}
	parent := rt.Snapshot()
	// The topic inherits the running main session, not this different default.
	m.SetAgentConfig(AgentConfig{Kind: "aiden"}, nil)
	var topics []Session
	for i, command := range []string{"/t 修复登录问题", "/topic"} {
		messageID := []string{"om-one", "om-two"}[i]
		id, err := b.RouteIncomingWithContext(ctx, larkRouteContext{MessageID: messageID, ChatID: "oc-group", ChatType: "group", SenderType: "user", SenderOpenID: "ou-user"}, larkIncomingMessage{Text: command})
		if err != nil {
			t.Fatal(err)
		}
		defer m.DeleteSession(ctx, id)
		sess, _, _ := m.GetSession(ctx, id)
		topics = append(topics, sess)
		if id == main.ID || sess.LarkTopicRootID != messageID || sess.LarkThreadID != "omt-"+messageID || sess.LastCWD != parent.LastCWD || sess.LastAgentKind != parent.LastAgentKind || sess.LastAgentID != parent.LastAgentID || sess.RecoveryKey == parent.RecoveryKey {
			t.Fatalf("incorrect topic inheritance: %#v parent=%#v", sess, parent)
		}
		if !strings.HasPrefix(sess.Name, "[话题] 研发群 · ") {
			t.Fatal(sess.Name)
		}
		if got := launcher.terminals[i+1].writes(); !strings.Contains(got, "cd '/tmp/current project'") || strings.Contains(got, "aiden") {
			t.Fatalf("topic launch: %s", got)
		}
		queued := b.popPipeline(id)
		if !strings.Contains(queued.Text, "scope=group") || !strings.Contains(queued.Text, "不要恢复或执行历史任务") || queued.InputMessageID != messageID {
			t.Fatalf("context prompt missing: %#v", queued)
		}
		if i == 0 && !strings.Contains(queued.Text, "修复登录问题") {
			t.Fatal("question missing")
		}
		if i == 1 && !strings.Contains(queued.Text, "等待用户") {
			t.Fatal("bare topic must await input")
		}
	}
	if topics[0].ID == topics[1].ID || topics[0].RecoveryKey == topics[1].RecoveryKey {
		t.Fatal("topics share identity")
	}
	for _, note := range notifier.notes() {
		if note.Startup && (note.TopicRootID == "" || note.ChatID != "oc-group") {
			t.Fatalf("startup escaped topic: %#v", note)
		}
	}
	b.addReaction = nil
	b.fetchReferencedMessages = nil
	b.botIdentity = larkBotIdentity{OpenID: "ou-self"}
	for i, topic := range topics {
		question := []string{"followup-one", "followup-two"}[i]
		event := p2MessageWithChat("input-"+question, topic.LarkTopicRootID, topic.LarkTopicRootID, "text", `{"text":"`+question+`"}`, "group", "oc-group", "ou-user")
		event.Event.Message.ThreadId = strPtr(topic.LarkThreadID)
		if err := b.HandleP2MessageReceive(ctx, event); err != nil {
			t.Fatal(err)
		}
		b.OnNotificationSent(topic.ID)
		if !strings.Contains(launcher.terminals[i+1].writes(), question) {
			t.Fatal("topic input was not delivered")
		}
		if strings.Contains(launcher.terminals[0].writes(), question) {
			t.Fatal("topic input reached main terminal")
		}
	}
	if _, err := b.RouteIncomingWithContext(ctx, larkRouteContext{MessageID: "om-one", ChatID: "oc-group", ChatType: "group"}, larkIncomingMessage{Text: "/t duplicate"}); err != nil {
		t.Fatal(err)
	}
	if len(launcher.terminals) != 3 {
		t.Fatal("duplicate command created a session")
	}
	// Restart loses all in-memory bindings; persisted topic identity must still win.
	restarted := NewManager(st, &recordingLauncher{}, WithIsolatedMessageRegistry())
	bridge := NewLarkReplyBridge("topic-app", "secret", restarted, t.TempDir())
	if id, err := bridge.RouteIncomingWithContext(ctx, larkRouteContext{MessageID: "om-one", ChatID: "oc-group", ChatType: "group"}, larkIncomingMessage{Text: "/t replay after restart"}); err != nil || id != topics[0].ID {
		t.Fatalf("duplicate after restart: %s %v", id, err)
	}
	for _, topic := range topics {
		if got := bridge.resolveSessionID(ctx, "hello", "", topic.LarkTopicRootID, "oc-group", "group", topic.LarkThreadID); got != topic.ID {
			t.Fatalf("restored topic=%s want %s", got, topic.ID)
		}
		if got := bridge.resolveSessionID(ctx, "hello", "", "", "oc-group", "group", topic.LarkThreadID); got != topic.ID {
			t.Fatalf("thread-only route=%s", got)
		}
		if got := bridge.resolveSessionID(ctx, "hello", "", topic.LarkTopicRootID, "other-group", "group", topic.LarkThreadID); got != "" {
			t.Fatal("cross-group route")
		}
	}
	if got := bridge.resolveSessionID(ctx, "hello", "", topics[0].LarkTopicRootID, "oc-group", "group", ""); got != main.ID {
		t.Fatal("ordinary quote escaped main group")
	}
	if got := bridge.resolveSessionID(ctx, main.ID, "", "unknown-root", "oc-group", "group", "unknown-thread"); got != "" {
		t.Fatal("unknown topic fell back to main")
	}
	if got := bridge.resolveSessionID(ctx, "hello", "", "", "oc-group", "group", ""); got != main.ID {
		t.Fatalf("main binding overwritten: %s", got)
	}
	otherBot := NewLarkReplyBridge("other-app", "secret", NewManager(nil, nil, WithIsolatedMessageRegistry()), t.TempDir())
	if got := otherBot.resolveSessionID(ctx, "hello", "", topics[0].LarkTopicRootID, "oc-group", "group", topics[0].LarkThreadID); got != "" {
		t.Fatal("cross-bot topic route")
	}
	untitled, _ := m.GetRuntime(topics[1].ID)
	if err := untitled.nameUntitledTopic(ctx, "测试目录切换"); err != nil {
		t.Fatal(err)
	}
	if got := untitled.Snapshot().Name; got != "[话题] 研发群 · followup-two" {
		t.Fatal(got)
	}
	removedBot := false
	b.removeBotFromChat = func(context.Context, string) error { removedBot = true; return nil }
	if _, err := b.handleCardDeleteSession(ctx, map[string]interface{}{"session_id": topics[0].ID}, "", "oc-group"); err != nil {
		t.Fatal(err)
	}
	if removedBot {
		t.Fatal("deleting topic removed the group bot")
	}
	if _, exists, _ := m.GetSession(ctx, topics[0].ID); exists {
		t.Fatal("topic was not deleted")
	}
	if got := b.resolveSessionID(ctx, "hello", "", "", "oc-group", "group", ""); got != main.ID {
		t.Fatal("deleting topic damaged main binding")
	}
	if err := m.DeleteSession(ctx, main.ID); err != nil {
		t.Fatal(err)
	}
	if got := b.resolveSessionID(ctx, "hello", "", topics[1].LarkTopicRootID, "oc-group", "group", ""); got != "" {
		t.Fatal("main quote routed to topic after main deletion")
	}
	if got := b.resolveSessionID(ctx, topics[1].ID, "", "", "oc-group", "group", ""); got != "" {
		t.Fatal("topic ID in ordinary group text bypassed isolation")
	}
}

func TestTopicParserAndHistoryScope(t *testing.T) {
	for _, value := range []string{"/tmp", "/topical x", "hi /t", "/topicx"} {
		if _, ok := parseLarkTopicCommand(value); ok {
			t.Fatal(value)
		}
	}
	for _, value := range []string{"/t", "/topic", " /t\n问题 "} {
		if _, ok := parseLarkTopicCommand(value); !ok {
			t.Fatal(value)
		}
	}
	ctx := context.Background()
	m := NewManager(nil, &recordingLauncher{}, WithIsolatedMessageRegistry())
	sess, err := m.createSession(ctx, "[话题] 群 · 问题", Session{LastCWD: t.TempDir(), LarkChatID: "oc-group", LarkTopicRootID: "om-root", LarkThreadID: "omt-thread"}, AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.DeleteSession(ctx, sess.ID)
	httpClient := &topicHTTPClient{}
	b := NewLarkReplyBridge("history-topic", "secret", m, t.TempDir())
	b.apiClient = newLarkReplyAPIClient("history-topic", "secret", lark.WithHttpClient(httpClient))
	for _, scope := range []string{"", "group"} {
		page, ok, err := m.AgentLarkMessages(ctx, sess.ID, sess.RecoveryKey, 100, scope)
		if err != nil || !ok || page.Context.ThreadID != "omt-thread" {
			t.Fatalf("context %#v %v", page, err)
		}
		query := httpClient.requests[len(httpClient.requests)-1]
		if scope == "" && (!strings.Contains(query, "container_id_type=thread") || !strings.Contains(query, "container_id=omt-thread") || !strings.Contains(query, "page_size=50")) {
			t.Fatal(query)
		}
		if scope == "group" && (!strings.Contains(query, "container_id_type=chat") || !strings.Contains(query, "container_id=oc-group")) {
			t.Fatal(query)
		}
	}
}
