package session

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func TestExtractLarkMessageText(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		messageType string
		want        string
	}{
		{name: "text", content: `{"text":"开始 会话A"}`, messageType: "text", want: "开始 会话A"},
		{name: "post", content: `{"content":[[{"tag":"text","text":"echo "},{"tag":"a","text":"hello"}]]}`, messageType: "post", want: "echo hello"},
		{name: "raw fallback", content: `开始 会话B`, messageType: "text", want: "开始 会话B"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractLarkMessageText(tt.content, tt.messageType); got != tt.want {
				t.Fatalf("extractLarkMessageText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractLarkIncomingMessageWithPostAttachments(t *testing.T) {
	got := extractLarkIncomingMessage(`{"content":[[{"tag":"img","image_key":"img_a"},{"tag":"text","text":"请分析"},{"tag":"file","file_key":"file_a","file_name":"报告.pdf"}]]}`, "post")
	if got.Text != "请分析" {
		t.Fatalf("text = %q, want 请分析", got.Text)
	}
	if len(got.Attachments) != 2 {
		t.Fatalf("attachments length = %d, want 2: %#v", len(got.Attachments), got.Attachments)
	}
	if got.Attachments[0].Kind != "image" || got.Attachments[0].Key != "img_a" {
		t.Fatalf("first attachment = %#v, want image img_a", got.Attachments[0])
	}
	if got.Attachments[1].Kind != "file" || got.Attachments[1].Key != "file_a" || got.Attachments[1].Name != "报告.pdf" {
		t.Fatalf("second attachment = %#v, want file file_a", got.Attachments[1])
	}
}

func TestLarkReplyBridgeReferencedTextIsSubmittedAsContext(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.fetchReferencedMessages = func(context.Context, string) ([]larkReferencedMessage, error) {
		return []larkReferencedMessage{{
			MessageID: "quoted-message", MessageType: "text", Content: `{"text":"这是被引用的原消息"}`,
		}}, nil
	}
	sess, err := manager.CreateSession(context.Background(), "引用测试")
	if err != nil {
		t.Fatal(err)
	}
	rt, _ := manager.GetRuntime(sess.ID)
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	rt.mu.Unlock()
	defaultLarkMessageRegistry.remember(sess.ID, "quoted-message")

	oldDelay := structuredInputEnterDelay
	structuredInputEnterDelay = 0
	defer func() { structuredInputEnterDelay = oldDelay }()
	_, err = bridge.RouteIncomingWithContext(context.Background(), larkRouteContext{
		MessageID: "current-message", ParentID: "quoted-message",
	}, larkIncomingMessage{Text: "请总结重点"})
	if err != nil {
		t.Fatal(err)
	}
	writes := launcher.terminals[0].writes()
	for _, want := range []string{"【引用消息：仅作为用户提供的上下文", "[文本] 这是被引用的原消息", "【用户当前请求】", "请总结重点"} {
		if !strings.Contains(writes, want) {
			t.Fatalf("submitted input should contain %q: %s", want, writes)
		}
	}
}

func TestLarkReplyBridgeReferencedAttachmentUsesOriginalMessageAndDegradesOnFailure(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.fetchReferencedMessages = func(context.Context, string) ([]larkReferencedMessage, error) {
		return []larkReferencedMessage{{
			MessageID: "quoted-image", MessageType: "image", Content: `{"image_key":"img-quoted"}`,
		}}, nil
	}
	var downloadedFrom string
	bridge.downloadFile = func(_ context.Context, messageID, _ string, ref larkAttachmentRef) (pendingLarkAttachment, error) {
		downloadedFrom = messageID
		if !ref.Optional || ref.SourceMessageID != "quoted-image" {
			t.Fatalf("quoted attachment metadata = %#v", ref)
		}
		return pendingLarkAttachment{}, errors.New("resource unavailable")
	}
	sess, err := manager.CreateSession(context.Background(), "引用图片测试")
	if err != nil {
		t.Fatal(err)
	}
	rt, _ := manager.GetRuntime(sess.ID)
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	rt.mu.Unlock()
	defaultLarkMessageRegistry.remember(sess.ID, "quoted-image")

	oldDelay := structuredInputEnterDelay
	structuredInputEnterDelay = 0
	defer func() { structuredInputEnterDelay = oldDelay }()
	_, err = bridge.RouteIncomingWithContext(context.Background(), larkRouteContext{
		MessageID: "current-message", ParentID: "quoted-image",
	}, larkIncomingMessage{Text: "分析这张图"})
	if err != nil {
		t.Fatalf("optional referenced attachment failure must not block input: %v", err)
	}
	if downloadedFrom != "quoted-image" {
		t.Fatalf("attachment downloaded from %q, want quoted-image", downloadedFrom)
	}
	writes := launcher.terminals[0].writes()
	if !strings.Contains(writes, "[图片] 图片消息") || !strings.Contains(writes, "引用附件无法下载") || !strings.Contains(writes, "分析这张图") {
		t.Fatalf("degraded referenced input = %q", writes)
	}
}

func TestResolveReferencedIncomingSupportsRichCardStickerAndLimits(t *testing.T) {
	bridge := NewLarkReplyBridge("app", "secret", nil, t.TempDir())
	messages := []larkReferencedMessage{
		{MessageID: "post-1", MessageType: "post", Content: `{"content":[[{"tag":"text","text":"方案说明"},{"tag":"img","image_key":"img-1"},{"tag":"file","file_key":"file-1","file_name":"方案.pdf"}]]}`},
		{MessageID: "card-1", MessageType: "interactive", Content: `{"header":{"title":{"tag":"plain_text","content":"审批结果"}},"body":{"elements":[{"tag":"div","text":{"tag":"plain_text","content":"已通过"}}]}}`},
		{MessageID: "sticker-1", MessageType: "sticker", Content: `{"file_key":"sticker-1"}`},
	}
	bridge.fetchReferencedMessages = func(context.Context, string) ([]larkReferencedMessage, error) {
		return messages, nil
	}
	got := bridge.resolveReferencedIncoming(context.Background(), larkRouteContext{ParentID: "parent"}, larkIncomingMessage{Text: "继续"})
	if got.Referenced == nil || len(got.Referenced.Items) != 3 {
		t.Fatalf("referenced context = %#v", got.Referenced)
	}
	if got.Referenced.Items[0].TypeLabel != "富文本" || got.Referenced.Items[0].Text != "方案说明" || got.Referenced.Items[0].Attachments != 2 {
		t.Fatalf("post reference = %#v", got.Referenced.Items[0])
	}
	if !strings.Contains(got.Referenced.Items[1].Text, "审批结果") || !strings.Contains(got.Referenced.Items[1].Text, "已通过") {
		t.Fatalf("card visible text = %q", got.Referenced.Items[1].Text)
	}
	if got.Referenced.Items[2].Text != "表情消息（飞书不提供表情资源下载）" || got.Referenced.Items[2].Attachments != 0 {
		t.Fatalf("sticker reference = %#v", got.Referenced.Items[2])
	}
	if len(got.Attachments) != 2 || got.Attachments[0].SourceMessageID != "post-1" || !got.Attachments[0].Optional {
		t.Fatalf("referenced attachments = %#v", got.Attachments)
	}

	bridge.fetchReferencedMessages = func(context.Context, string) ([]larkReferencedMessage, error) {
		many := make([]larkReferencedMessage, maxLarkReferencedItems+5)
		for i := range many {
			many[i] = larkReferencedMessage{MessageID: "m", MessageType: "text", Content: `{"text":"内容"}`}
		}
		return many, nil
	}
	limited := bridge.resolveReferencedIncoming(context.Background(), larkRouteContext{ParentID: "parent"}, larkIncomingMessage{})
	if len(limited.Referenced.Items) != maxLarkReferencedItems || !limited.Referenced.Truncated {
		t.Fatalf("referenced item limit = %#v", limited.Referenced)
	}
}

func TestLarkReplyBridgeReferencedFetchFailureStillSubmitsCurrentRequest(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.fetchReferencedMessages = func(context.Context, string) ([]larkReferencedMessage, error) {
		return nil, errors.New("permission denied")
	}
	sess, err := manager.CreateSession(context.Background(), "引用降级测试")
	if err != nil {
		t.Fatal(err)
	}
	rt, _ := manager.GetRuntime(sess.ID)
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	rt.mu.Unlock()
	defaultLarkMessageRegistry.remember(sess.ID, "quoted-unavailable")

	oldDelay := structuredInputEnterDelay
	structuredInputEnterDelay = 0
	defer func() { structuredInputEnterDelay = oldDelay }()
	_, err = bridge.RouteIncomingWithContext(context.Background(), larkRouteContext{
		MessageID: "current-message", ParentID: "quoted-unavailable",
	}, larkIncomingMessage{Text: "即使引用失效也继续回答"})
	if err != nil {
		t.Fatal(err)
	}
	writes := launcher.terminals[0].writes()
	if !strings.Contains(writes, "引用消息无法读取") || !strings.Contains(writes, "即使引用失效也继续回答") {
		t.Fatalf("fetch failure fallback = %q", writes)
	}
}

func TestLarkReplyBridgeAddsProcessingReactionForP2Message(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	var reactions []string
	bridge.addReaction = func(_ context.Context, messageID string, emoji string) error {
		reactions = append(reactions, messageID+":"+emoji)
		return nil
	}

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-react", "", "", "text", `{"text":"echo from lark"}`)); err != nil {
		t.Fatal(err)
	}
	if len(reactions) != 1 || reactions[0] != "m-react:"+larkProcessingReactionEmoji {
		t.Fatalf("expected processing reaction on incoming message, got %#v", reactions)
	}
	if len(launcher.terminals) != 1 {
		t.Fatalf("message should still route to terminal, got %d terminals", len(launcher.terminals))
	}
	if got := launcher.terminals[0].writes(); !strings.Contains(got, PrepareStructuredInput("echo from lark")) {
		t.Fatalf("terminal should receive submitted input despite reaction, got %q", got)
	}
}

func TestLarkReplyBridgeBotAddedCreatesBoundMentionModeAgentSession(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.SetStartPresets(map[string]SessionStartPreset{
		"999999": {Commands: []string{"codex --dangerously-bypass-approvals-and-sandbox"}},
	})
	var chatMessages []string
	bridge.sendChatText = func(_ context.Context, chatID, text string) error {
		chatMessages = append(chatMessages, chatID+":"+text)
		return nil
	}
	external := false
	event := &larkim.P2ChatMemberBotAddedV1{Event: &larkim.P2ChatMemberBotAddedV1Data{
		ChatId:     strPtr("oc-project-group"),
		Name:       strPtr("项目开发群"),
		External:   &external,
		OperatorId: &larkim.UserId{OpenId: strPtr("ou-owner")},
	}}

	if err := bridge.HandleP2ChatMemberBotAdded(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	sessions, err := manager.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	sess := sessions[0]
	if sess.Name != "项目开发群" || sess.LarkChatID != "oc-project-group" || !sess.LarkMentionModeEnabled || !sess.NotifyOnWaiting {
		t.Fatalf("unexpected auto-created session: %#v", sess)
	}
	if sess.LastMode != SessionModeAgent || sess.LastAgentKind != "codex" {
		t.Fatalf("session should enter configured agent mode: %#v", sess)
	}
	writes := launcher.terminals[0].writeParts()
	wantWrites := []string{
		"mkdir -p ${HOME}/'Easy_Terminal_Workspace/项目开发群'\r",
		"cd ${HOME}/'Easy_Terminal_Workspace/项目开发群'\r",
		"codex --dangerously-bypass-approvals-and-sandbox\r",
	}
	if len(writes) < len(wantWrites) {
		t.Fatalf("terminal writes = %#v, want suffix %#v", writes, wantWrites)
	}
	writes = writes[len(writes)-len(wantWrites):]
	for i := range wantWrites {
		if writes[i] != wantWrites[i] {
			t.Fatalf("terminal write[%d] = %q, want %q", i, writes[i], wantWrites[i])
		}
	}
	wantMessage := "oc-project-group:已创建并绑定会话「项目开发群」，请 @机器人发送任务。"
	if len(chatMessages) != 1 || chatMessages[0] != wantMessage {
		t.Fatalf("chat messages = %#v, want %q", chatMessages, wantMessage)
	}
	if strings.Contains(chatMessages[0], "Easy_Terminal_Workspace") || strings.Contains(chatMessages[0], "codex") {
		t.Fatalf("intro should not expose workspace or agent: %q", chatMessages[0])
	}

	if err := bridge.HandleP2ChatMemberBotAdded(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if len(launcher.terminals) != 1 || len(chatMessages) != 1 {
		t.Fatalf("duplicate event created side effects: terminals=%d messages=%#v", len(launcher.terminals), chatMessages)
	}
}

func TestLarkReplyBridgeDirectContactCreatesAndReusesAssistantGroup(t *testing.T) {
	resetLarkRegistryForTest()
	store := newMemoryStore()
	launcher := &recordingLauncher{}
	manager := NewManager(store, launcher)
	manager.SetAgentConfig(AgentConfig{Kind: "codex", Command: "codex --dangerously-bypass-approvals-and-sandbox"}, nil)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.SetDeveloperOpenID("ou-owner")
	bridge.addReaction = func(context.Context, string, string) error { return nil }
	bridge.fetchUserDisplayName = func(_ context.Context, openID string) (string, error) {
		switch openID {
		case "ou-user":
			return "小林", nil
		case "ou-owner":
			return "Eleven", nil
		default:
			return "", errors.New("unknown user")
		}
	}
	created := 0
	bridge.createContactChat = func(_ context.Context, sessionID, chatTitle string, members []string) (string, error) {
		created++
		if chatTitle != "小林和Eleven的会话" || len(members) != 2 || members[0] != "ou-user" || members[1] != "ou-owner" {
			t.Fatalf("unexpected contact chat request: session=%s title=%q members=%#v", sessionID, chatTitle, members)
		}
		return "oc-contact", nil
	}
	var messages []string
	bridge.sendChatText = func(_ context.Context, chatID, text string) error {
		messages = append(messages, chatID+":"+text)
		return nil
	}

	if err := bridge.HandleP2MessageReceive(context.Background(), p2MessageWithChat("m-contact-1", "", "", "text", `{"text":"我想约个时间"}`, "p2p", "oc-direct", "ou-user")); err != nil {
		t.Fatal(err)
	}
	if err := bridge.HandleP2MessageReceive(context.Background(), p2MessageWithChat("m-contact-2", "", "", "text", `{"text":"明天下午可以吗"}`, "p2p", "oc-direct", "ou-user")); err != nil {
		t.Fatal(err)
	}
	if created != 1 || len(launcher.terminals) != 1 {
		t.Fatalf("contact conversation should be reused: created=%d terminals=%d", created, len(launcher.terminals))
	}
	binding, ok, err := store.GetLarkContactBinding(context.Background(), "ou-user")
	if err != nil || !ok || binding.ChatID != "oc-contact" || binding.DisplayName != "小林" || !binding.Active {
		t.Fatalf("unexpected contact binding: %#v ok=%v err=%v", binding, ok, err)
	}
	sess, ok, err := manager.GetSession(context.Background(), binding.SessionID)
	if err != nil || !ok || !sess.LarkMentionModeEnabled || sess.DeveloperModeEnabled || sess.LastAgentKind != "codex" {
		t.Fatalf("unexpected contact session: %#v ok=%v err=%v", sess, ok, err)
	}
	if sess.Name != "小林和Eleven的会话" {
		t.Fatalf("contact session name = %q", sess.Name)
	}
	if got := launcher.terminals[0].writes(); !strings.Contains(got, "codex --dangerously-bypass-approvals-and-sandbox\r") {
		t.Fatalf("configured Agent should start in the terminal: %q", got)
	}
	bridge.mu.Lock()
	queued := append([]larkPipelineInput(nil), bridge.pipelines[binding.SessionID]...)
	bridge.mu.Unlock()
	if len(queued) != 2 || queued[0].Text != "我想约个时间" || queued[1].Text != "明天下午可以吗" {
		t.Fatalf("contact messages should queue while Agent starts: %#v", queued)
	}
	joined := strings.Join(messages, "\n")
	if !strings.Contains(joined, "oc-contact:小林：我想约个时间") || !strings.Contains(joined, "oc-contact:小林：明天下午可以吗") {
		t.Fatalf("private messages should be forwarded into the contact group: %#v", messages)
	}
}

func TestLarkContactConversationNameUsesFullOpenIDFallback(t *testing.T) {
	if got := larkContactConversationName("小林", "Eleven"); got != "小林和Eleven的会话" {
		t.Fatalf("contact conversation name = %q", got)
	}
	if got := larkContactConversationName("ou-user-full", "ou-owner-full"); got != "ou-user-full和ou-owner-full的会话" {
		t.Fatalf("fallback contact conversation name = %q", got)
	}
}

func TestLarkContactConversationNameFallsBackPerUser(t *testing.T) {
	tests := []struct {
		name      string
		responses map[string]string
		want      string
	}{
		{name: "both names", responses: map[string]string{"ou-user": "小林", "ou-owner": "Eleven"}, want: "小林和Eleven的会话"},
		{name: "contact open id", responses: map[string]string{"ou-owner": "Eleven"}, want: "ou-user和Eleven的会话"},
		{name: "developer open id", responses: map[string]string{"ou-user": "小林"}, want: "小林和ou-owner的会话"},
		{name: "both open ids", responses: map[string]string{}, want: "ou-user和ou-owner的会话"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bridge := &LarkReplyBridge{}
			bridge.fetchUserDisplayName = func(_ context.Context, openID string) (string, error) {
				if name := tt.responses[openID]; name != "" {
					return name, nil
				}
				return "", errors.New("name unavailable")
			}
			contactName := bridge.larkUserDisplayNameOrOpenID(context.Background(), "ou-user")
			developerName := bridge.larkUserDisplayNameOrOpenID(context.Background(), "ou-owner")
			if got := larkContactConversationName(contactName, developerName); got != tt.want {
				t.Fatalf("contact conversation name = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLarkReplyBridgeBotAddedIgnoresExternalGroup(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	external := true
	event := &larkim.P2ChatMemberBotAddedV1{Event: &larkim.P2ChatMemberBotAddedV1Data{
		ChatId:   strPtr("oc-external"),
		Name:     strPtr("外部群"),
		External: &external,
	}}

	if err := bridge.HandleP2ChatMemberBotAdded(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if len(launcher.terminals) != 0 {
		t.Fatalf("external group should not create a terminal, got %d", len(launcher.terminals))
	}
}

func TestLarkReplyBridgeIgnoresConfiguredP2PrefixWithFollowingSpace(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	var reactions []string
	bridge.addReaction = func(_ context.Context, messageID string, emoji string) error {
		reactions = append(reactions, messageID+":"+emoji)
		return nil
	}

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-ignore", "", "", "text", `{"text":"/i 这条不要响应"}`)); err != nil {
		t.Fatal(err)
	}
	if len(reactions) != 0 {
		t.Fatalf("ignored message should not add reactions, got %#v", reactions)
	}
	if len(launcher.terminals) != 0 {
		t.Fatalf("ignored message should not create terminals, got %d", len(launcher.terminals))
	}

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-not-ignore", "", "", "text", `{"text":"/itest should route"}`)); err != nil {
		t.Fatal(err)
	}
	if len(launcher.terminals) != 1 {
		t.Fatalf("message without prefix-space should route, got %d terminals", len(launcher.terminals))
	}
}

func TestLarkReplyBridgeIgnoresCustomP1PrefixWithFollowingSpace(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.SetIgnoreMessagePrefix("/silent")
	var reactions []string
	bridge.addReaction = func(_ context.Context, messageID string, emoji string) error {
		reactions = append(reactions, messageID+":"+emoji)
		return nil
	}

	err := bridge.HandleP1MessageReceive(context.Background(), &larkim.P1MessageReceiveV1{
		Event: &larkim.P1MessageReceiveV1Data{
			OpenMessageID:    "p1-ignore",
			TextWithoutAtBot: "/silent 这条不要响应",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reactions) != 0 {
		t.Fatalf("ignored P1 message should not add reactions, got %#v", reactions)
	}
	if len(launcher.terminals) != 0 {
		t.Fatalf("ignored P1 message should not create terminals, got %d", len(launcher.terminals))
	}
}

func TestLarkReplyBridgeContinuesWhenProcessingReactionFails(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.addReaction = func(context.Context, string, string) error {
		return errors.New("missing reaction permission")
	}

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-react-fail", "", "", "text", `{"text":"echo from lark"}`)); err != nil {
		t.Fatal(err)
	}
	if len(launcher.terminals) != 1 {
		t.Fatalf("reaction failure should not block routing, got %d terminals", len(launcher.terminals))
	}
	if got := launcher.terminals[0].writes(); !strings.Contains(got, PrepareStructuredInput("echo from lark")) {
		t.Fatalf("terminal should receive submitted input despite reaction failure, got %q", got)
	}
}

func TestLarkReplyBridgeStartCreatesDedicatedChat(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.createChat = func(_ context.Context, sessionID, name, ownerOpenID string) (string, error) {
		if sessionID != "sess-1" || name != "手机会话" || ownerOpenID != "ou-user" {
			t.Fatalf("unexpected create chat args: session=%q name=%q owner=%q", sessionID, name, ownerOpenID)
		}
		return "oc-chat-1", nil
	}
	var chatMessages []string
	bridge.sendChatText = func(_ context.Context, chatID, text string) error {
		chatMessages = append(chatMessages, chatID+":"+text)
		return nil
	}

	err := bridge.HandleP2MessageReceive(context.Background(), p2MessageWithChat("m-start-chat", "", "", "text", `{"text":"开始 手机会话"}`, "p2p", "oc-main", "ou-user"))
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := manager.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].LarkChatID != "oc-chat-1" {
		t.Fatalf("created session should bind lark chat, got %#v", sessions)
	}
	if !sessions[0].DeveloperModeEnabled || sessions[0].LarkMentionModeEnabled {
		t.Fatalf("private start command should enable developer mode and disable mention mode: %#v", sessions[0])
	}
	if got, ok := defaultLarkMessageRegistry.lookupChat("oc-chat-1"); !ok || got != "sess-1" {
		t.Fatalf("registry chat lookup = %q,%v; want sess-1,true", got, ok)
	}
	if len(chatMessages) != 1 || !strings.Contains(chatMessages[0], "oc-chat-1:已创建会话 手机会话") {
		t.Fatalf("expected intro message to session chat, got %#v", chatMessages)
	}
}

func TestLarkReplyBridgeDoesNotNotifyBeforeDedicatedChatBind(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	notifier := &recordingNotifier{messageID: "bot-card"}
	manager := NewManager(
		nil,
		launcher,
		WithNotifier(notifier),
		WithWaitingTransitionDelays(10*time.Millisecond, 10*time.Millisecond),
		WithNotificationUpdateCoalesce(0),
	)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.createChat = func(_ context.Context, sessionID, name, ownerOpenID string) (string, error) {
		if len(launcher.terminals) != 1 {
			t.Fatalf("expected terminal before chat creation, got %d", len(launcher.terminals))
		}
		launcher.terminals[0].readCh <- []byte("eleven ~ > ")
		time.Sleep(80 * time.Millisecond)
		return "oc-chat-1", nil
	}

	err := bridge.HandleP2MessageReceive(context.Background(), p2MessageWithChat("m-start-chat-bind", "", "", "text", `{"text":"开始 手机会话"}`, "p2p", "oc-main", "ou-user"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	for _, note := range notifier.notes() {
		if note.ChatID != "oc-chat-1" {
			t.Fatalf("notification should wait until dedicated chat is bound, got %#v", note)
		}
	}
}

func TestLarkReplyBridgeDoesNotFallbackToDefaultReceiverWhenDedicatedChatMissing(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	notifier := &recordingNotifier{messageID: "bot-card"}
	manager := NewManager(nil, launcher, WithNotifier(notifier), WithWaitingTransitionDelays(10*time.Millisecond, 10*time.Millisecond), WithNotificationUpdateCoalesce(0))
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.createChat = func(context.Context, string, string, string) (string, error) {
		return "", nil
	}

	err := bridge.HandleP2MessageReceive(context.Background(), p2MessageWithChat("m-start-chat-empty", "", "", "text", `{"text":"开始 手机会话"}`, "p2p", "oc-main", "ou-user"))
	if err != nil {
		t.Fatal(err)
	}
	if len(launcher.terminals) != 1 {
		t.Fatalf("expected created terminal, got %d", len(launcher.terminals))
	}
	launcher.terminals[0].readCh <- []byte("eleven ~ > ")
	time.Sleep(80 * time.Millisecond)
	if notes := notifier.notes(); len(notes) != 0 {
		t.Fatalf("dedicated-chat session must not notify default receiver before chat bind, got %#v", notes)
	}
}

func TestLarkReplyBridgeUsesConfiguredChatPrefix(t *testing.T) {
	bridge := NewLarkReplyBridge("app", "secret", NewManager(nil, &recordingLauncher{}), t.TempDir())
	bridge.SetSessionChatPrefix("DEV ·")

	if got := bridge.larkSessionChatName("手机会话"); got != "DEV ·手机会话" {
		t.Fatalf("chat name = %q", got)
	}
}

func TestLarkCreateChatUUIDIsUniqueAcrossSameSessionID(t *testing.T) {
	first := larkCreateChatUUID("sess-1")
	time.Sleep(time.Nanosecond)
	second := larkCreateChatUUID("sess-1")
	if first == second {
		t.Fatalf("chat create uuid should be unique across reused session ids, got %q", first)
	}
	if !strings.HasPrefix(first, "easy-terminal-sess-1-") || !strings.HasPrefix(second, "easy-terminal-sess-1-") {
		t.Fatalf("unexpected chat create uuid format: %q %q", first, second)
	}
}

func TestLarkReplyBridgeRoutesByDedicatedChatID(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	sess, err := manager.CreateSession(context.Background(), "A")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := manager.BindLarkChat(context.Background(), sess.ID, "oc-chat-a"); err != nil || !ok {
		t.Fatalf("BindLarkChat ok=%v err=%v", ok, err)
	}

	err = bridge.HandleP2MessageReceive(context.Background(), p2MessageWithChat("m-chat-input", "", "", "text", `{"text":"pwd"}`, "group", "oc-chat-a", "ou-user"))
	if err != nil {
		t.Fatal(err)
	}
	if len(launcher.terminals) != 1 {
		t.Fatalf("terminal count = %d, want 1", len(launcher.terminals))
	}
	if got := launcher.terminals[0].writes(); !strings.Contains(got, PrepareStructuredInput("pwd")) {
		t.Fatalf("dedicated chat input should route to existing terminal, got %q", got)
	}
}

func TestLarkReplyBridgeGroupInputMentionsSender(t *testing.T) {
	resetLarkRegistryForTest()
	previousDelay := structuredInputEnterDelay
	structuredInputEnterDelay = 0
	defer func() { structuredInputEnterDelay = previousDelay }()

	launcher := &recordingLauncher{}
	notifier := &recordingNotifier{messageID: "bot-running"}
	manager := NewManager(nil, launcher, WithNotifier(notifier))
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	sess, err := manager.CreateSession(context.Background(), "Group")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := manager.BindLarkChat(context.Background(), sess.ID, "oc-group"); err != nil || !ok {
		t.Fatalf("BindLarkChat ok=%v err=%v", ok, err)
	}
	if _, ok, err := manager.UpdateNotifyOnWaiting(context.Background(), sess.ID, true); err != nil || !ok {
		t.Fatalf("UpdateNotifyOnWaiting ok=%v err=%v", ok, err)
	}

	err = bridge.HandleP2MessageReceive(context.Background(), p2MessageWithChat("m-group-mention", "", "", "text", `{"text":"pwd | whoami"}`, "group", "oc-group", "ou-bob"))
	if err != nil {
		t.Fatal(err)
	}
	notes := notifier.notes()
	if len(notes) == 0 {
		t.Fatal("expected running notification")
	}
	got := notes[len(notes)-1]
	if got.MentionOpenID != "ou-bob" || got.ChatID != "oc-group" || !got.Running {
		t.Fatalf("group notification should mention sender, got %#v", got)
	}

	bridge.OnNotificationSent(sess.ID)
	rt, ok := manager.GetRuntime(sess.ID)
	if !ok {
		t.Fatal("runtime not found")
	}
	if got := rt.NotificationMentionOpenID(); got != "ou-bob" {
		t.Fatalf("pipeline input should keep sender mention, got %q", got)
	}
}

func TestLarkReplyBridgeMentionModeRequiresBotMentionInGroup(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.fetchBotIdentity = func(context.Context) (larkBotIdentity, error) {
		return larkBotIdentity{OpenID: "ou-bot"}, nil
	}
	var reactions []string
	bridge.addReaction = func(_ context.Context, messageID string, emoji string) error {
		reactions = append(reactions, messageID+":"+emoji)
		return nil
	}
	sess, err := manager.CreateSession(context.Background(), "Group")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := manager.BindLarkChat(context.Background(), sess.ID, "oc-group"); err != nil || !ok {
		t.Fatalf("BindLarkChat ok=%v err=%v", ok, err)
	}
	if _, ok, err := manager.ToggleLarkMentionMode(context.Background(), sess.ID); err != nil || !ok {
		t.Fatalf("ToggleLarkMentionMode ok=%v err=%v", ok, err)
	}

	if err := bridge.HandleP2MessageReceive(context.Background(), p2MessageWithChat("m-no-mention", "", "", "text", `{"text":"pwd"}`, "group", "oc-group", "ou-user")); err != nil {
		t.Fatal(err)
	}
	if got := launcher.terminals[0].writes(); got != "" {
		t.Fatalf("unmentioned group message should be ignored in mention mode, got %q", got)
	}
	if len(reactions) != 0 {
		t.Fatalf("ignored unmentioned message should not add reaction, got %#v", reactions)
	}

	other := p2MessageWithChat("m-other-mention", "", "", "text", `{"text":"<at user_id=\"ou-other\">Other</at> whoami"}`, "group", "oc-group", "ou-user")
	other.Event.Message.Mentions = []*larkim.MentionEvent{{Id: &larkim.UserId{OpenId: strPtr("ou-other")}}}
	if err := bridge.HandleP2MessageReceive(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if got := launcher.terminals[0].writes(); got != "" {
		t.Fatalf("message mentioning another user should be ignored in mention mode, got %q", got)
	}

	bot := p2MessageWithChat("m-bot-mention", "", "", "text", `{"text":"<at user_id=\"ou-bot\">Bot</at> date"}`, "group", "oc-group", "ou-user")
	bot.Event.Message.Mentions = []*larkim.MentionEvent{{Id: &larkim.UserId{OpenId: strPtr("ou-bot")}}}
	if err := bridge.HandleP2MessageReceive(context.Background(), bot); err != nil {
		t.Fatal(err)
	}
	if got := launcher.terminals[0].writes(); !strings.Contains(got, PrepareStructuredInput("date")) {
		t.Fatalf("message mentioning current bot should route, got %q", got)
	}
	if len(reactions) != 1 || reactions[0] != "m-bot-mention:"+larkProcessingReactionEmoji {
		t.Fatalf("only routed bot mention should add reaction, got %#v", reactions)
	}
}

func TestLarkReplyBridgeMentionModeDoesNotFilterDirectChat(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	sess, err := manager.CreateSession(context.Background(), "Direct")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := manager.BindLarkChat(context.Background(), sess.ID, "oc-direct"); err != nil || !ok {
		t.Fatalf("BindLarkChat ok=%v err=%v", ok, err)
	}
	if _, ok, err := manager.ToggleLarkMentionMode(context.Background(), sess.ID); err != nil || !ok {
		t.Fatalf("ToggleLarkMentionMode ok=%v err=%v", ok, err)
	}

	err = bridge.HandleP2MessageReceive(context.Background(), p2MessageWithChat("m-direct", "", "", "text", `{"text":"pwd"}`, "p2p", "oc-direct", "ou-user"))
	if err != nil {
		t.Fatal(err)
	}
	if got := launcher.terminals[0].writes(); !strings.Contains(got, PrepareStructuredInput("pwd")) {
		t.Fatalf("direct chat should not be filtered by mention mode, got %q", got)
	}
}

func TestLarkReplyBridgeDirectBotChatDoesNotRouteToLatestSession(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())

	latest, err := manager.CreateSession(context.Background(), "Latest")
	if err != nil {
		t.Fatal(err)
	}
	defaultLarkMessageRegistry.rememberLatest(latest.ID)

	err = bridge.HandleP2MessageReceive(context.Background(), p2MessageWithChat("m-direct-bot", "", "", "text", `{"text":"pwd"}`, "p2p", "oc-direct-bot", "ou-user"))
	if err != nil {
		t.Fatal(err)
	}
	if len(launcher.terminals) != 2 {
		t.Fatalf("direct bot chat should create its own session, terminal count=%d", len(launcher.terminals))
	}
	if got := launcher.terminals[0].writes(); strings.Contains(got, PrepareStructuredInput("pwd")) {
		t.Fatalf("latest session should not receive direct bot input, got %q", got)
	}
	if got := launcher.terminals[1].writes(); !strings.Contains(got, PrepareStructuredInput("pwd")) {
		t.Fatalf("direct bot session should receive input, got %q", got)
	}
	if got, ok := defaultLarkMessageRegistry.lookupChat("oc-direct-bot"); !ok || got == "" || got == latest.ID {
		t.Fatalf("direct bot chat should bind to its own session, got %q,%v latest=%s", got, ok, latest.ID)
	}
}

func TestLarkReplyBridgeDirectBotChatReusesOwnSession(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())

	if err := bridge.HandleP2MessageReceive(context.Background(), p2MessageWithChat("m-direct-bot-1", "", "", "text", `{"text":"pwd"}`, "p2p", "oc-direct-bot", "ou-user")); err != nil {
		t.Fatal(err)
	}
	if err := bridge.HandleP2MessageReceive(context.Background(), p2MessageWithChat("m-direct-bot-2", "", "", "text", `{"text":"whoami"}`, "p2p", "oc-direct-bot", "ou-user")); err != nil {
		t.Fatal(err)
	}
	if len(launcher.terminals) != 1 {
		t.Fatalf("direct bot chat should reuse bound session, terminal count=%d", len(launcher.terminals))
	}
	got := launcher.terminals[0].writes()
	if !strings.Contains(got, PrepareStructuredInput("pwd")) || !strings.Contains(got, PrepareStructuredInput("whoami")) {
		t.Fatalf("direct bot session should receive both inputs, got %q", got)
	}
}

func TestLarkReplyBridgeP1MentionModeUsesBotMentionFlag(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	sess, err := manager.CreateSession(context.Background(), "P1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := manager.BindLarkChat(context.Background(), sess.ID, "oc-p1-group"); err != nil || !ok {
		t.Fatalf("BindLarkChat ok=%v err=%v", ok, err)
	}
	if _, ok, err := manager.ToggleLarkMentionMode(context.Background(), sess.ID); err != nil || !ok {
		t.Fatalf("ToggleLarkMentionMode ok=%v err=%v", ok, err)
	}

	if err := bridge.HandleP1MessageReceive(context.Background(), &larkim.P1MessageReceiveV1{
		Event: &larkim.P1MessageReceiveV1Data{
			OpenMessageID:    "p1-unmentioned",
			OpenChatID:       "oc-p1-group",
			ChatType:         "group",
			OpenID:           "ou-user",
			TextWithoutAtBot: "pwd",
			IsMention:        false,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if got := launcher.terminals[0].writes(); got != "" {
		t.Fatalf("unmentioned P1 group message should be ignored in mention mode, got %q", got)
	}

	if err := bridge.HandleP1MessageReceive(context.Background(), &larkim.P1MessageReceiveV1{
		Event: &larkim.P1MessageReceiveV1Data{
			OpenMessageID:    "p1-mentioned",
			OpenChatID:       "oc-p1-group",
			ChatType:         "group",
			OpenID:           "ou-user",
			TextWithoutAtBot: "whoami",
			IsMention:        true,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if got := launcher.terminals[0].writes(); !strings.Contains(got, PrepareStructuredInput("whoami")) {
		t.Fatalf("P1 bot mention should route in mention mode, got %q", got)
	}
}

func TestLarkReplyBridgeIgnoresStaleDedicatedChatRegistry(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	sess, err := manager.CreateSession(context.Background(), "A")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := manager.BindLarkChat(context.Background(), sess.ID, "oc-chat-a"); err != nil || !ok {
		t.Fatalf("BindLarkChat ok=%v err=%v", ok, err)
	}
	rt, ok := manager.GetRuntime(sess.ID)
	if !ok {
		t.Fatal("runtime not found")
	}
	rt.markTerminal(StatusExited, 0)
	defaultLarkMessageRegistry.rememberChat("oc-chat-a", sess.ID)

	err = bridge.HandleP2MessageReceive(context.Background(), p2MessageWithChat("m-chat-stale", "", "", "text", `{"text":"pwd"}`, "group", "oc-chat-a", "ou-user"))
	if err != nil {
		t.Fatal(err)
	}
	if got := launcher.terminals[0].writes(); strings.Contains(got, PrepareStructuredInput("pwd")) {
		t.Fatalf("stale dedicated chat should not route to exited terminal, got %q", got)
	}
	if got, ok := defaultLarkMessageRegistry.lookupChat("oc-chat-a"); ok && got == sess.ID {
		t.Fatalf("stale chat mapping should be cleared, got %q", got)
	}
}

func TestLarkReplyBridgeRoutesP1ByDedicatedChatID(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	sess, err := manager.CreateSession(context.Background(), "P1A")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := manager.BindLarkChat(context.Background(), sess.ID, "oc-p1-chat"); err != nil || !ok {
		t.Fatalf("BindLarkChat ok=%v err=%v", ok, err)
	}

	err = bridge.HandleP1MessageReceive(context.Background(), &larkim.P1MessageReceiveV1{
		Event: &larkim.P1MessageReceiveV1Data{
			OpenMessageID:    "p1-chat-input",
			OpenChatID:       "oc-p1-chat",
			ChatType:         "group",
			OpenID:           "ou-user",
			TextWithoutAtBot: "echo p1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(launcher.terminals) != 1 {
		t.Fatalf("terminal count = %d, want 1", len(launcher.terminals))
	}
	if got := launcher.terminals[0].writes(); !strings.Contains(got, PrepareStructuredInput("echo p1")) {
		t.Fatalf("P1 dedicated chat input should route to existing terminal, got %q", got)
	}
}

func TestLarkNotificationCardCanTargetDedicatedChat(t *testing.T) {
	content, err := larkNotificationCardContent(WaitingNotification{
		SessionID: "sess-1",
		Name:      "A",
		Content:   "done",
		ChatID:    "oc-chat-a",
	}, "open-id", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "done") {
		t.Fatalf("card content should still render body, got %s", content)
	}
}

func TestLarkReplyBridgeImageWaitsForTextBeforeEnter(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.downloadFile = func(_ context.Context, _ string, _ string, ref larkAttachmentRef) (pendingLarkAttachment, error) {
		return pendingLarkAttachment{Kind: ref.Kind, Path: "/tmp/lark-image.png"}, nil
	}
	var replies []string
	bridge.replyText = func(_ context.Context, _ string, text string) error {
		replies = append(replies, text)
		return nil
	}

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-image", "", "", "image", `{"image_key":"img_a"}`)); err != nil {
		t.Fatal(err)
	}
	if len(launcher.terminals) != 1 {
		t.Fatalf("expected one terminal, got %d", len(launcher.terminals))
	}
	if got := launcher.terminals[0].writes(); got != " /tmp/lark-image.png " {
		t.Fatalf("image-only message should write path without enter, got %q", got)
	}
	if len(replies) != 1 || replies[0] != "图片已上传成功，等待你的说明后执行。" {
		t.Fatalf("unexpected replies: %#v", replies)
	}

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-image-text", "", "", "text", `{"text":"请分析这张图"}`)); err != nil {
		t.Fatal(err)
	}
	parts := launcher.terminals[0].writeParts()
	if !lastSubmittedWrite(parts, "请分析这张图") {
		t.Fatalf("followup text should append text and enter, got %#v", parts)
	}
	if got := launcher.terminals[0].writes(); strings.Count(got, "/tmp/lark-image.png") != 1 {
		t.Fatalf("pending image path should not be duplicated, writes: %q", got)
	}
}

func TestSubmitStructuredInputDelaysEnterForTUI(t *testing.T) {
	term := &recordingTerminal{readCh: make(chan []byte)}
	rt := &RuntimeSession{
		manager:  NewManager(nil, nil),
		terminal: term,
		session:  Session{ID: "sess-1", Name: "TUI", Status: StatusWaiting, Live: true},
	}

	if err := SubmitStructuredInput(rt, "hello tui"); err != nil {
		t.Fatal(err)
	}
	parts := term.writeParts()
	if !lastSubmittedWrite(parts, "hello tui") {
		t.Fatalf("structured input should write text and enter separately, got %#v", parts)
	}
	times := term.writeTimes()
	if len(times) < 2 {
		t.Fatalf("expected text and enter write times, got %d", len(times))
	}
	if got := times[len(times)-1].Sub(times[len(times)-2]); got < structuredInputEnterDelay {
		t.Fatalf("enter should be delayed after text by at least %s, got %s", structuredInputEnterDelay, got)
	}
}

func TestSubmitStructuredInputUsesSingleCarriageReturnEnter(t *testing.T) {
	previousDelay := structuredInputEnterDelay
	structuredInputEnterDelay = 0
	defer func() {
		structuredInputEnterDelay = previousDelay
	}()

	term := &recordingTerminal{readCh: make(chan []byte)}
	rt := &RuntimeSession{
		manager:  NewManager(nil, nil),
		terminal: term,
		session:  Session{ID: "sess-1", Name: "TUI", Status: StatusWaiting, Live: true},
	}

	if err := SubmitStructuredInput(rt, "谢谢哈"); err != nil {
		t.Fatal(err)
	}
	parts := term.writeParts()
	if len(parts) < 2 {
		t.Fatalf("expected text and enter writes, got %#v", parts)
	}
	if got := parts[len(parts)-1]; got != "\r" {
		t.Fatalf("structured input should submit with a single carriage return, got %q", got)
	}
}

func TestSubmitStructuredInputSkipsEnterForNumericInput(t *testing.T) {
	previousDelay := structuredInputEnterDelay
	structuredInputEnterDelay = 0
	defer func() {
		structuredInputEnterDelay = previousDelay
	}()

	term := &recordingTerminal{readCh: make(chan []byte)}
	rt := &RuntimeSession{
		manager:  NewManager(nil, nil),
		terminal: term,
		session:  Session{ID: "sess-1", Name: "TUI", Status: StatusWaiting, Live: true},
		visibleSnapshot: strings.Join([]string{
			"Select Model and Effort",
			"Access legacy models by running codex -m <model_name>",
			"› 1. gpt-5.5 (current)",
			"2. gpt-5.4",
			"3. gpt-5.4-mini",
			"Press enter to confirm or esc to go back",
		}, "\n"),
	}

	if err := SubmitStructuredInput(rt, "1"); err != nil {
		t.Fatal(err)
	}
	parts := term.writeParts()
	if len(parts) != 1 || parts[0] != "1" {
		t.Fatalf("numeric input should write only the digit, got %#v", parts)
	}
}

func TestSubmitStructuredInputSkipsEnterForPlainNumericInput(t *testing.T) {
	previousDelay := structuredInputEnterDelay
	structuredInputEnterDelay = 0
	defer func() {
		structuredInputEnterDelay = previousDelay
	}()

	term := &recordingTerminal{readCh: make(chan []byte)}
	rt := &RuntimeSession{
		manager:         NewManager(nil, nil),
		terminal:        term,
		session:         Session{ID: "sess-1", Name: "TUI", Status: StatusWaiting, Live: true},
		visibleSnapshot: "› 1",
	}

	if err := SubmitStructuredInput(rt, "1"); err != nil {
		t.Fatal(err)
	}
	if parts := term.writeParts(); len(parts) != 1 || parts[0] != "1" {
		t.Fatalf("plain numeric input should write only the digit, got %#v", parts)
	}
}

func TestPrepareStructuredInputKeepsInputText(t *testing.T) {
	if got := PrepareStructuredInput("hello tui"); got != "hello tui\r" {
		t.Fatalf("structured input = %q", got)
	}
	if got := PrepareStructuredInput("/exit"); got != "/exit\r" {
		t.Fatalf("slash command should be kept as-is, got %q", got)
	}
}

func TestSubmitStructuredInputClearsPreviousNotificationBeforeEcho(t *testing.T) {
	previousDelay := structuredInputEnterDelay
	structuredInputEnterDelay = 0
	defer func() { structuredInputEnterDelay = previousDelay }()

	notifier := &recordingNotifier{}
	manager := NewManager(nil, nil, WithNotifier(notifier))
	rt := &RuntimeSession{
		manager:                 manager,
		session:                 Session{ID: "sess-1", Name: "TUI", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastNotifiedMessageID:   "msg-old",
		lastNotifiedContent:     "old reply",
		notificationUpdateNo:    0,
		notificationRunning:     false,
		snapshotAtRoundStart:    "old snapshot",
		snapshotAtRoundStartSet: true,
		visibleSnapshot:         "old snapshot",
		visibleSnapshotVersion:  1,
	}
	term := &recordingTerminal{readCh: make(chan []byte)}
	term.onWrite = func(data string) {
		if data == "second input" {
			rt.HandleOutput([]byte(data))
		}
	}
	rt.terminal = term

	if err := SubmitStructuredInput(rt, "second input"); err != nil {
		t.Fatal(err)
	}
	if running := notifier.runningNotes(); len(running) != 0 {
		t.Fatalf("input echo should not mark previous notification running, got %#v", running)
	}
	if rt.lastNotifiedMessageID != "" {
		t.Fatalf("new input round should clear previous message id before terminal echo, got %q", rt.lastNotifiedMessageID)
	}
}

func TestSubmitStructuredInputDoesNotTreatItsOwnEchoAsPreviousUnfinishedRound(t *testing.T) {
	previousDelay := structuredInputEnterDelay
	structuredInputEnterDelay = 0
	defer func() { structuredInputEnterDelay = previousDelay }()

	rt := &RuntimeSession{
		manager:                 NewManager(nil, nil),
		session:                 Session{ID: "sess-own-echo", Name: "TUI", Status: StatusWaiting, Live: true},
		lastInputText:           "你好",
		snapshotAtRoundStart:    "› 你好\n• previous completed answer",
		snapshotAtRoundStartSet: true,
		visibleSnapshot:         "› 你好\n• previous completed answer",
		subscribers:             make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	ch, cancel := rt.SubscribeWithMode(false)
	defer cancel()
	go func() {
		for ev := range ch {
			if ev.Type == RuntimeEventSnapshotRequest {
				rt.SetVisibleSnapshotWithSource("› 你好\n• previous completed answer\n› 你好", "browser:buffer")
				return
			}
		}
	}()
	term := &recordingTerminal{readCh: make(chan []byte)}
	term.onWrite = func(data string) {
		if data == "你好" {
			// PTY echo arrives before MarkStructuredInputActivity and changes the
			// live status. It belongs to the new input, not the previous round.
			rt.HandleOutput([]byte(data))
		}
	}
	rt.terminal = term

	if err := SubmitStructuredInput(rt, "你好"); err != nil {
		t.Fatal(err)
	}
	rt.mu.Lock()
	windowStart := rt.notificationWindowInputText
	rt.mu.Unlock()
	if windowStart != "" {
		t.Fatalf("the new input's own echo must not preserve the completed previous input as an open window: %q", windowStart)
	}
}

func TestSubmitStructuredInputRefreshesComposerBaselineBeforeEnter(t *testing.T) {
	previousDelay := structuredInputEnterDelay
	structuredInputEnterDelay = 0
	defer func() { structuredInputEnterDelay = previousDelay }()

	manager := NewManager(nil, nil)
	rt := &RuntimeSession{
		manager:                manager,
		session:                Session{ID: "sess-1", Name: "TUI", Status: StatusWaiting, Live: true},
		visibleSnapshot:        "old cached snapshot",
		visibleSnapshotVersion: 1,
		subscribers:            make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	ch, cancel := rt.SubscribeWithMode(false)
	defer cancel()
	go func() {
		if ev := <-ch; ev.Type == RuntimeEventSnapshotRequest {
			rt.SetVisibleSnapshotWithSource("fresh baseline\n> new input", "browser:buffer")
		}
	}()
	term := &recordingTerminal{readCh: make(chan []byte)}
	term.onWrite = func(data string) {
		if data != "\r" {
			return
		}
		rt.mu.Lock()
		defer rt.mu.Unlock()
		if rt.snapshotAtRoundStart != "fresh baseline\n> new input" {
			t.Fatalf("input baseline = %q, want rendered composer baseline", rt.snapshotAtRoundStart)
		}
	}
	rt.terminal = term

	if err := SubmitStructuredInput(rt, "new input"); err != nil {
		t.Fatal(err)
	}
}

func TestSubmitStructuredInputFromTargetsOriginBaseline(t *testing.T) {
	previousDelay := structuredInputEnterDelay
	structuredInputEnterDelay = 0
	defer func() { structuredInputEnterDelay = previousDelay }()

	rt := &RuntimeSession{
		manager:     NewManager(nil, nil),
		session:     Session{ID: "sess-origin-submit", Name: "TUI", Status: StatusWaiting, Live: true},
		terminal:    &recordingTerminal{readCh: make(chan []byte)},
		subscribers: make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	idle, cancelIdle := rt.SubscribeWithMode(false)
	defer cancelIdle()
	origin, cancelOrigin := rt.SubscribeWithMode(false)
	defer cancelOrigin()

	done := make(chan error, 1)
	go func() { done <- SubmitStructuredInputFrom(rt, "origin input", origin) }()
	event := receiveSnapshotRequestEvent(t, origin)
	if event.Purpose != SnapshotPurposeInputBaseline {
		t.Fatalf("baseline request purpose = %q", event.Purpose)
	}
	assertNoSnapshotRequestEvent(t, idle, "idle renderer received web submit baseline")
	rt.SetVisibleSnapshotResponseFrom("origin baseline", "browser:buffer", event.RequestID, origin)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("origin-bound structured submit did not finish")
	}

	rt.mu.Lock()
	owner := rt.snapshotAtRoundResponder
	baseline := rt.snapshotAtRoundStart
	rt.mu.Unlock()
	if owner != origin || baseline != "origin baseline" {
		t.Fatalf("round baseline owner/content = %p/%q, want origin/%q", owner, baseline, "origin baseline")
	}
}

func TestSubmitStructuredNumericInputKeepsPreInputMenuBaseline(t *testing.T) {
	previousDelay := structuredInputEnterDelay
	structuredInputEnterDelay = 0
	defer func() { structuredInputEnterDelay = previousDelay }()

	const menu = "Select Model and Effort\n1. gpt-5.6-sol\n2. gpt-5.6-terra"
	rt := &RuntimeSession{
		manager:                NewManager(nil, nil),
		session:                Session{ID: "sess-menu", Name: "TUI", Status: StatusWaiting, Live: true},
		visibleSnapshot:        menu,
		visibleSnapshotVersion: 1,
		subscribers:            make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	ch, cancel := rt.SubscribeWithMode(false)
	defer cancel()
	go func() {
		if ev := <-ch; ev.Type == RuntimeEventSnapshotRequest {
			rt.SetVisibleSnapshotWithSource(menu, "browser:buffer")
		}
	}()
	term := &recordingTerminal{readCh: make(chan []byte)}
	term.onWrite = func(data string) {
		if data != "1" {
			return
		}
		rt.mu.Lock()
		defer rt.mu.Unlock()
		if rt.snapshotAtRoundStart != menu {
			t.Fatalf("numeric menu baseline = %q, want pre-selection menu", rt.snapshotAtRoundStart)
		}
	}
	rt.terminal = term

	if err := SubmitStructuredInput(rt, "1"); err != nil {
		t.Fatal(err)
	}
	if got := term.writes(); got != "1" {
		t.Fatalf("numeric input must not append Enter, got %q", got)
	}
}

func TestSubmitStructuredInputFreshBaselineKeepsPreviousRoundsOutOfDiff(t *testing.T) {
	previousDelay := structuredInputEnterDelay
	structuredInputEnterDelay = 0
	defer func() { structuredInputEnterDelay = previousDelay }()

	manager := NewManager(nil, nil)
	rt := &RuntimeSession{
		manager:                manager,
		session:                Session{ID: "sess-1", Name: "TUI", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		terminal:               &recordingTerminal{readCh: make(chan []byte)},
		visibleSnapshot:        "stale cached screen",
		visibleSnapshotVersion: 1,
		subscribers:            make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	ch, cancel := rt.SubscribeWithMode(false)
	defer cancel()
	go func() {
		if ev := <-ch; ev.Type == RuntimeEventSnapshotRequest {
			rt.SetVisibleSnapshotWithSource("previous question\nprevious answer\n› next question", "browser:buffer")
		}
	}()

	if err := SubmitStructuredInput(rt, "next question"); err != nil {
		t.Fatal(err)
	}
	rt.SetVisibleSnapshotWithSource("previous question\nprevious answer\n› next question\nnew answer", "browser:buffer")
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	n, _, ok, reason := rt.waitingNotificationCandidateLocked()
	rt.mu.Unlock()
	if !ok {
		t.Fatalf("expected notification candidate, reason=%s", reason)
	}
	if strings.Contains(n.Content, "previous answer") {
		t.Fatalf("current round diff should not include previous round, got %q", n.Content)
	}
	if !strings.Contains(n.Content, "new answer") {
		t.Fatalf("current round diff should include new answer, got %q", n.Content)
	}
}

func TestLarkReplyBridgeMultiImageWithTextSubmitsImmediately(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	paths := map[string]string{"img_a": "/tmp/a.png", "img_b": "/tmp/b.png"}
	bridge.downloadFile = func(_ context.Context, _ string, _ string, ref larkAttachmentRef) (pendingLarkAttachment, error) {
		return pendingLarkAttachment{Kind: ref.Kind, Path: paths[ref.Key]}, nil
	}
	var replies []string
	bridge.replyText = func(_ context.Context, _ string, text string) error {
		replies = append(replies, text)
		return nil
	}

	content := `{"content":[[{"tag":"img","image_key":"img_a"},{"tag":"img","image_key":"img_b"},{"tag":"text","text":"对比这两张图"}]]}`
	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-images-text", "", "", "post", content)); err != nil {
		t.Fatal(err)
	}
	parts := launcher.terminals[0].writeParts()
	if !lastSubmittedWrite(parts, " /tmp/a.png /tmp/b.png 对比这两张图") {
		t.Fatalf("image+text should submit immediately, got %#v", parts)
	}
	if len(replies) != 0 {
		t.Fatalf("image+text should not send upload-success reply, got %#v", replies)
	}
}

func TestLarkReplyBridgeImageMessageWithTextSubmitsImmediately(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.downloadFile = func(_ context.Context, _ string, _ string, ref larkAttachmentRef) (pendingLarkAttachment, error) {
		return pendingLarkAttachment{Kind: ref.Kind, Path: "/tmp/a.png"}, nil
	}
	var replies []string
	bridge.replyText = func(_ context.Context, _ string, text string) error {
		replies = append(replies, text)
		return nil
	}

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-image-caption", "", "", "image", `{"image_key":"img_a","text":"请分析这张图"}`)); err != nil {
		t.Fatal(err)
	}
	parts := launcher.terminals[0].writeParts()
	if !lastSubmittedWrite(parts, " /tmp/a.png 请分析这张图") {
		t.Fatalf("image message with text should submit immediately, got %#v", parts)
	}
	if len(replies) != 0 {
		t.Fatalf("image message with text should not send upload-success reply, got %#v", replies)
	}
}

func TestLarkReplyBridgeAttachmentWithTextClearsStalePendingInput(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	paths := map[string]string{"old_img": "/tmp/old.png", "new_img": "/tmp/new.png"}
	bridge.downloadFile = func(_ context.Context, _ string, _ string, ref larkAttachmentRef) (pendingLarkAttachment, error) {
		return pendingLarkAttachment{Kind: ref.Kind, Path: paths[ref.Key]}, nil
	}
	bridge.replyText = func(_ context.Context, _ string, _ string) error { return nil }

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-old-image", "", "", "image", `{"image_key":"old_img"}`)); err != nil {
		t.Fatal(err)
	}
	content := `{"content":[[{"tag":"img","image_key":"new_img"},{"tag":"text","text":"分析新的"}]]}`
	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-new-image-text", "", "", "post", content)); err != nil {
		t.Fatal(err)
	}
	parts := launcher.terminals[0].writeParts()
	if len(parts) < 3 {
		t.Fatalf("expected pending input, clear, submitted new input; got %#v", parts)
	}
	if parts[len(parts)-3] != "\x15" {
		t.Fatalf("new attachment+text should clear stale pending input before submit, got %#v", parts)
	}
	if !lastSubmittedWrite(parts, " /tmp/new.png 分析新的") {
		t.Fatalf("new attachment+text should submit only current attachment and text, got %#v", parts)
	}
}

func TestLarkReplyBridgeFileWaitsForTextBeforeEnter(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.downloadFile = func(_ context.Context, _ string, _ string, ref larkAttachmentRef) (pendingLarkAttachment, error) {
		return pendingLarkAttachment{Kind: ref.Kind, Path: "/tmp/report.pdf"}, nil
	}
	var replies []string
	bridge.replyText = func(_ context.Context, _ string, text string) error {
		replies = append(replies, text)
		return nil
	}

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-file", "", "", "file", `{"file_key":"file_a","file_name":"报告.pdf"}`)); err != nil {
		t.Fatal(err)
	}
	if got := launcher.terminals[0].writes(); got != " /tmp/report.pdf " {
		t.Fatalf("file-only message should write path without enter, got %q", got)
	}
	if len(replies) != 1 || replies[0] != "文件已上传成功，等待你的说明后执行。" {
		t.Fatalf("unexpected replies: %#v", replies)
	}
}

func TestLarkReplyBridgeIgnoresInteractiveCards(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	var reply string
	bridge.replyText = func(_ context.Context, _ string, text string) error {
		reply = text
		return nil
	}

	err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-card", "", "", "interactive", `{"title":"测试","elements":[{"tag":"div"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(launcher.terminals) != 0 {
		t.Fatalf("interactive card should not create or write a terminal, got %d", len(launcher.terminals))
	}
	if !strings.Contains(reply, "收到转发卡片") {
		t.Fatalf("interactive card should get an explanatory reply, got %q", reply)
	}
}

func TestLarkReplyBridgeRepliesWhenPostContentIsUnreadable(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	var reply string
	bridge.replyText = func(_ context.Context, _ string, text string) error {
		reply = text
		return nil
	}

	err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-unreadable-post", "", "", "post", `{"content":[[{"tag":"media","media_key":"unsupported"}]]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(launcher.terminals) != 0 {
		t.Fatalf("unreadable post should not create or write a terminal, got %d", len(launcher.terminals))
	}
	if !strings.Contains(reply, "无法读取") {
		t.Fatalf("unreadable post should get an explanatory reply, got %q", reply)
	}
}

func TestLarkReplyBridgeIgnoresNonUserSender(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())

	err := bridge.HandleP2MessageReceive(context.Background(), p2MessageWithSender("m-app", "", "", "text", `{"text":"开始 测试"}`, "app"))
	if err != nil {
		t.Fatal(err)
	}
	if len(launcher.terminals) != 0 {
		t.Fatalf("app sender should not create or write a terminal, got %d", len(launcher.terminals))
	}
}

func TestLarkReplyBridgeRoutesP2StartAndFollowup(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	var browserMu sync.Mutex
	var browserRequests []string
	manager := NewManager(nil, launcher, WithBrowserNeeded(func(sessionID string) {
		browserMu.Lock()
		defer browserMu.Unlock()
		browserRequests = append(browserRequests, sessionID)
	}))
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())

	err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start", "", "", "text", `{"text":"开始 飞书会话"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(launcher.terminals) != 1 {
		t.Fatalf("expected one terminal, got %d", len(launcher.terminals))
	}
	sessions, err := manager.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Name != "飞书会话" {
		t.Fatalf("unexpected sessions: %#v", sessions)
	}
	if !sessions[0].NotifyOnWaiting {
		t.Fatalf("lark-created session should enable notifications by default: %#v", sessions[0])
	}
	waitForBrowserRequest(t, &browserMu, &browserRequests, "sess-1")

	err = bridge.HandleP2MessageReceive(context.Background(), p2Message("m-follow", "m-start", "", "text", `{"text":"echo from lark"}`))
	if err != nil {
		t.Fatal(err)
	}
	got := launcher.terminals[0].writes()
	if !strings.Contains(got, PrepareStructuredInput("echo from lark")) {
		t.Fatalf("terminal did not receive followup input: %q", got)
	}
	parts := launcher.terminals[0].writeParts()
	if !lastSubmittedWrite(parts, "echo from lark") {
		t.Fatalf("lark followup should submit text and enter atomically, got %#v", parts)
	}
	waitForBrowserRequest(t, &browserMu, &browserRequests, "sess-1")
}

func TestLarkReplyBridgeFollowupCreatesRunningCard(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	notifier := &recordingNotifier{messageID: "bot-running"}
	manager := NewManager(nil, launcher, WithNotifier(notifier))
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-running", "", "", "text", `{"text":"开始 Running会话"}`)); err != nil {
		t.Fatal(err)
	}
	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-follow-running", "m-start-running", "", "text", `{"text":"echo from lark"}`)); err != nil {
		t.Fatal(err)
	}

	notes := notifier.notes()
	if len(notes) == 0 {
		t.Fatal("expected an immediate running card")
	}
	got := notes[len(notes)-1]
	if got.Content != RunningNotificationPlaceholder || !got.Running {
		t.Fatalf("running card = %#v", got)
	}
	if got.SessionID == "" {
		t.Fatalf("running card should include session id: %#v", got)
	}
}

func TestLarkReplyBridgeQueuesFollowupWhileRuntimeRunningDuringStartupWindow(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	notifier := &recordingNotifier{createMessageIDs: []string{"old-card", "new-card"}}
	manager := NewManager(nil, launcher, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())

	sess, err := manager.CreateSession(context.Background(), "Queue")
	if err != nil {
		t.Fatal(err)
	}
	rt, ok := manager.GetRuntime(sess.ID)
	if !ok {
		t.Fatal("runtime session missing")
	}
	rt.mu.Lock()
	rt.session.Status = StatusRunning
	rt.session.LastMode = SessionModeAgent
	rt.session.Live = true
	rt.session.NotifyOnWaiting = true
	rt.inputQueueUntil = time.Now().Add(time.Minute)
	rt.mu.Unlock()
	rt.NotifyInputRunning()
	notes := waitForNotifierNotes(t, notifier, 1)
	if len(notes) != 1 || !notes[0].Running || notes[0].MessageID != "" {
		t.Fatalf("expected initial running card create, got %#v", notes)
	}
	defaultLarkMessageRegistry.remember(sess.ID, "bot-card")

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-follow-queue", "bot-card", "", "text", `{"text":"queued question"}`)); err != nil {
		t.Fatal(err)
	}
	notes = waitForNotifierNotes(t, notifier, 3)
	if len(notes) < 3 {
		t.Fatalf("queued followup should disable old card and create new running card, got %#v", notes)
	}
	disabledOldCard := false
	newRunningCard := false
	for _, note := range notes {
		if note.MessageID == "old-card" && note.Disabled && !note.Running {
			disabledOldCard = true
		}
		if note.MessageID == "" && note.Running && note.Content == RunningNotificationPlaceholder {
			newRunningCard = true
		}
	}
	if !disabledOldCard || !newRunningCard {
		t.Fatalf("queued followup should disable previous running card and create latest running card, got %#v", notes)
	}

	parts := launcher.terminals[0].writeParts()
	if lastSubmittedWrite(parts, "queued question") || strings.Contains(launcher.terminals[0].writes(), "queued question") {
		t.Fatalf("running followup should be queued instead of written immediately, parts=%#v", parts)
	}

	bridge.OnNotificationSent("sess-1")
	parts = launcher.terminals[0].writeParts()
	if !lastSubmittedWrite(parts, "queued question") {
		t.Fatalf("queued followup should submit after notification, got %#v", parts)
	}

	rt.mu.Lock()
	rt.session.Status = StatusRunning
	rt.inputQueueUntil = time.Now().Add(-time.Second)
	rt.mu.Unlock()
	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-follow-direct", "bot-card", "", "text", `{"text":"direct question"}`)); err != nil {
		t.Fatal(err)
	}
	parts = launcher.terminals[0].writeParts()
	if !lastSubmittedWrite(parts, "direct question") {
		t.Fatalf("running followup after startup window should be written immediately, got %#v", parts)
	}
}

func TestLarkReplyBridgeOverlappingFollowupFreezesRunningCardEndToEnd(t *testing.T) {
	resetLarkRegistryForTest()
	previousDelay := structuredInputEnterDelay
	structuredInputEnterDelay = 0
	defer func() { structuredInputEnterDelay = previousDelay }()

	launcher := &recordingLauncher{}
	notifier := &recordingNotifier{createMessageIDs: []string{"card-start", "card-2", "card-3"}}
	manager := NewManager(
		nil,
		launcher,
		WithNotifier(notifier),
		WithWaitingTransitionDelays(20*time.Millisecond, 20*time.Millisecond),
		WithNotificationUpdateCoalesce(0),
	)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-overlap", "", "", "text", `{"text":"开始 Overlap"}`)); err != nil {
		t.Fatal(err)
	}
	rt, ok := manager.GetRuntime("sess-1")
	if !ok {
		t.Fatal("runtime session missing")
	}
	rt.SetVisibleSnapshot("chat\n>")

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-second-overlap", "m-start-overlap", "", "text", `{"text":"second question"}`)); err != nil {
		t.Fatal(err)
	}
	rt.SetVisibleSnapshot("chat\n>\n> second question\npartial second answer")
	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-third-overlap", "m-second-overlap", "", "text", `{"text":"third question"}`)); err != nil {
		t.Fatal(err)
	}
	if !rt.NotificationMessageFrozen("card-2") {
		t.Fatal("previous running card should be frozen")
	}

	rt.SetVisibleSnapshot("chat\n>\n> second question\npartial second answer\n> third question\nfinal third answer\n>")
	launcher.terminals[0].readCh <- []byte("final third answer\r\n")

	notes := waitForNotifierNotes(t, notifier, 6)
	if len(notes) != 6 {
		t.Fatalf("expected three running cards, two disabled updates, and one final update, got %#v", notes)
	}
	runningCreates := 0
	disabledStart := false
	disabledSecond := false
	finalLatest := false
	want := "> second question\npartial second answer\n> third question\nfinal third answer\n>"
	for _, note := range notes {
		if note.MessageID == "" && note.Content == RunningNotificationPlaceholder && note.Running {
			runningCreates++
		}
		if note.MessageID == "card-start" && note.Disabled && !note.Running {
			disabledStart = true
		}
		if note.MessageID == "card-2" && note.Disabled && !note.Running {
			disabledSecond = true
		}
		if note.MessageID == "card-3" && note.Content == want && !note.Running && !note.Disabled {
			finalLatest = true
		}
	}
	if runningCreates != 3 || !disabledStart || !disabledSecond || !finalLatest {
		t.Fatalf("overlap should disable old cards and update latest card, got %#v", notes)
	}
}

func TestLarkReplyBridgeCardShortcutSendsCtrlC(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	notifier := &recordingNotifier{messageID: "bot-card"}
	manager := NewManager(nil, launcher, WithNotifier(notifier))
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	sess, err := manager.CreateSession(context.Background(), "Shortcut")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.UpdateNotifyOnWaiting(context.Background(), sess.ID, true); err != nil {
		t.Fatal(err)
	}
	event := &callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
		Action: &callback.CallBackAction{Value: map[string]interface{}{
			"easy_terminal_action": "shortcut",
			"session_id":           sess.ID,
			"key":                  "ctrl_c",
		}},
		Context: &callback.Context{OpenMessageID: "bot-card"},
	}}

	resp, err := bridge.HandleCardActionTrigger(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if resp != nil {
		t.Fatalf("unexpected response: %#v", resp)
	}
	parts := launcher.terminals[0].writeParts()
	if len(parts) == 0 || parts[len(parts)-1] != "\x03" {
		t.Fatalf("shortcut should send Ctrl-C to terminal, got %#v", parts)
	}
	notes := notifier.notes()
	if len(notes) != 0 {
		t.Fatalf("shortcut should not overwrite clicked card with placeholder, got %#v", notes)
	}
	rt, ok := manager.GetRuntime(sess.ID)
	if !ok {
		t.Fatal("runtime not found")
	}
	if rt.lastNotifiedMessageID != "bot-card" {
		t.Fatalf("shortcut should keep clicked card as notification anchor, got %q", rt.lastNotifiedMessageID)
	}
}

func TestLarkReplyBridgeLegacyCardPayloadSendsCtrlC(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	sess, err := manager.CreateSession(context.Background(), "Shortcut")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"open_message_id":"bot-card","action":{"value":{"easy_terminal_action":"shortcut","session_id":"` + sess.ID + `","key":"ctrl_c"}}}`)

	resp, err := bridge.handleCardActionPayload(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}
	if resp != nil {
		t.Fatalf("unexpected response: %#v", resp)
	}
	parts := launcher.terminals[0].writeParts()
	if len(parts) == 0 || parts[len(parts)-1] != "\x03" {
		t.Fatalf("legacy card payload should send Ctrl-C to terminal, got %#v", parts)
	}
}

func TestLarkReplyBridgeCardDeleteSessionRemovesBotFromChat(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	uploadsDir := t.TempDir()
	bridge := NewLarkReplyBridge("cli_a", "secret", manager, uploadsDir)
	var removedChats []string
	bridge.removeBotFromChat = func(_ context.Context, chatID string) error {
		removedChats = append(removedChats, chatID)
		return nil
	}
	sess, err := manager.CreateSession(context.Background(), "Delete")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := manager.BindLarkChat(context.Background(), sess.ID, "oc-chat-delete"); err != nil || !ok {
		t.Fatalf("BindLarkChat ok=%v err=%v", ok, err)
	}
	uploadPath := filepath.Join(uploadsDir, sess.ID)
	if err := os.MkdirAll(uploadPath, 0o755); err != nil {
		t.Fatal(err)
	}
	defaultLarkMessageRegistry.remember(sess.ID, "bot-card")
	event := &callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
		Action: &callback.CallBackAction{Value: map[string]interface{}{
			"easy_terminal_action": "delete_session",
			"session_id":           sess.ID,
		}},
		Context: &callback.Context{OpenMessageID: "bot-card", OpenChatID: "oc-chat-delete"},
	}}

	resp, err := bridge.HandleCardActionTrigger(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Content != "会话已删除，机器人已退出群聊" {
		t.Fatalf("unexpected delete response: %#v", resp)
	}
	if _, ok := manager.GetRuntime(sess.ID); ok {
		t.Fatal("deleted session runtime should be removed")
	}
	if _, ok, err := manager.GetSession(context.Background(), sess.ID); err != nil || ok {
		t.Fatalf("deleted session should not be listed, ok=%v err=%v", ok, err)
	}
	if len(removedChats) != 1 || removedChats[0] != "oc-chat-delete" {
		t.Fatalf("delete should remove bot from bound chat, got %#v", removedChats)
	}
	if got, ok := defaultLarkMessageRegistry.lookupChat("oc-chat-delete"); ok || got != "" {
		t.Fatalf("deleted chat route should be forgotten, got %q,%v", got, ok)
	}
	if _, err := os.Stat(uploadPath); !os.IsNotExist(err) {
		t.Fatalf("delete should remove session upload dir, err=%v", err)
	}
}

func TestLarkReplyBridgeCardDeleteSessionReportsBotRemovalFailure(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("cli_a", "secret", manager, t.TempDir())
	bridge.removeBotFromChat = func(context.Context, string) error {
		return errors.New("missing permission")
	}
	sess, err := manager.CreateSession(context.Background(), "Delete")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := manager.BindLarkChat(context.Background(), sess.ID, "oc-chat-delete"); err != nil || !ok {
		t.Fatalf("BindLarkChat ok=%v err=%v", ok, err)
	}
	event := &callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
		Action: &callback.CallBackAction{Value: map[string]interface{}{
			"easy_terminal_action": "delete_session",
			"session_id":           sess.ID,
		}},
		Context: &callback.Context{OpenMessageID: "bot-card", OpenChatID: "oc-chat-delete"},
	}}

	resp, err := bridge.HandleCardActionTrigger(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Type != "warning" || !strings.Contains(resp.Toast.Content, "机器人移出群聊失败") {
		t.Fatalf("unexpected delete failure response: %#v", resp)
	}
	if _, ok := manager.GetRuntime(sess.ID); ok {
		t.Fatal("session should still be deleted when bot removal fails")
	}
}

func TestLarkReplyBridgeCardShortcutExitsAgentWithDoubleCtrlC(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	sess, err := manager.CreateSession(context.Background(), "Shortcut")
	if err != nil {
		t.Fatal(err)
	}
	event := &callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
		Action: &callback.CallBackAction{Value: map[string]interface{}{
			"easy_terminal_action": "shortcut",
			"session_id":           sess.ID,
			"key":                  "exit_agent",
		}},
		Context: &callback.Context{OpenMessageID: "bot-card"},
	}}

	resp, err := bridge.HandleCardActionTrigger(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if resp != nil {
		t.Fatalf("unexpected response: %#v", resp)
	}
	parts := launcher.terminals[0].writeParts()
	if len(parts) == 0 || parts[len(parts)-1] != "\x03\x03" {
		t.Fatalf("exit agent shortcut should send two Ctrl-C inputs, got %#v", parts)
	}
}

func TestLarkReplyBridgeCardRefreshUpdatesClickedMessage(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	notifier := &recordingNotifier{messageID: "bot-card"}
	manager := NewManager(nil, launcher, WithNotifier(notifier))
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	sess, err := manager.CreateSession(context.Background(), "Shortcut")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.UpdateNotifyOnWaiting(context.Background(), sess.ID, true); err != nil {
		t.Fatal(err)
	}
	rt, _ := manager.GetRuntime(sess.ID)
	setTrustedLegacyRoundFixture(rt, "$", "echo hello\r", "$ echo hello\nhello\n$")
	event := &callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
		Action: &callback.CallBackAction{Value: map[string]interface{}{
			"easy_terminal_action": "refresh",
			"session_id":           sess.ID,
		}},
		Context: &callback.Context{OpenMessageID: "bot-card"},
	}}

	resp, err := bridge.HandleCardActionTrigger(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Content != "刷新成功" {
		t.Fatalf("unexpected response: %#v", resp)
	}
	notes := waitForNotifierNotes(t, notifier, 1)
	if len(notes) != 1 || notes[0].MessageID != "bot-card" || notes[0].Content != "hello\n$" {
		t.Fatalf("manual refresh should patch clicked card, got %#v", notes)
	}
}

func TestLarkReplyBridgeCardToggleAutoRefreshWaitsForInterval(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	notifier := &recordingNotifier{messageID: "bot-card"}
	manager := NewManager(nil, launcher, WithNotifier(notifier), WithAutoRefreshInterval(80*time.Millisecond))
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	sess, err := manager.CreateSession(context.Background(), "Shortcut")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.UpdateNotifyOnWaiting(context.Background(), sess.ID, true); err != nil {
		t.Fatal(err)
	}
	rt, _ := manager.GetRuntime(sess.ID)
	rt.MarkInputActivity("echo hello\r")
	rt.SetVisibleSnapshot("$ echo hello\nhello\n$")
	event := &callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
		Action: &callback.CallBackAction{Value: map[string]interface{}{
			"easy_terminal_action": "toggle_auto_refresh",
			"session_id":           sess.ID,
		}},
		Context: &callback.Context{OpenMessageID: "bot-card"},
	}}

	resp, err := bridge.HandleCardActionTrigger(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Content != "已开启自动刷新" {
		t.Fatalf("unexpected response: %#v", resp)
	}
	time.Sleep(40 * time.Millisecond)
	if notes := notifier.notes(); len(notes) != 0 {
		t.Fatalf("toggle should not refresh before configured interval, got %#v", notes)
	}
	notes := waitForNotifierNotes(t, notifier, 1)
	if len(notes) != 1 || notes[0].MessageID != "bot-card" || !notes[0].AutoRefreshEnabled || notes[0].SuppressUpdateTip {
		t.Fatalf("auto refresh should patch clicked card after interval, got %#v", notes)
	}

	resp, err = bridge.HandleCardActionTrigger(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Content != "已关闭自动刷新" {
		t.Fatalf("unexpected close response: %#v", resp)
	}
	notes = waitForNotifierNotes(t, notifier, 2)
	if len(notes) < 2 || notes[1].AutoRefreshEnabled {
		t.Fatalf("toggle close should patch clicked card as auto refresh disabled, got %#v", notes)
	}
}

func TestLarkReplyBridgeCardToggleAutoSummaryPatchesCard(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	notifier := &recordingNotifier{messageID: "bot-card"}
	manager := NewManager(nil, launcher, WithNotifier(notifier))
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	sess, err := manager.CreateSession(context.Background(), "Shortcut")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.UpdateNotifyOnWaiting(context.Background(), sess.ID, true); err != nil {
		t.Fatal(err)
	}
	rt, _ := manager.GetRuntime(sess.ID)
	rt.MarkInputActivity("echo hello\r")
	rt.SetVisibleSnapshot("$ echo hello\nhello\n$")
	event := &callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
		Action: &callback.CallBackAction{Value: map[string]interface{}{
			"easy_terminal_action": "toggle_auto_summary",
			"session_id":           sess.ID,
		}},
		Context: &callback.Context{OpenMessageID: "bot-card"},
	}}

	resp, err := bridge.HandleCardActionTrigger(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Content != "已开启自动总结" {
		t.Fatalf("unexpected response: %#v", resp)
	}
	notes := waitForNotifierNotes(t, notifier, 1)
	if len(notes) != 1 || notes[0].MessageID != "bot-card" || !notes[0].AutoSummaryEnabled {
		t.Fatalf("auto summary should patch clicked card as enabled, got %#v", notes)
	}

	resp, err = bridge.HandleCardActionTrigger(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Content != "已关闭自动总结" {
		t.Fatalf("unexpected close response: %#v", resp)
	}
	notes = waitForNotifierNotes(t, notifier, 2)
	if len(notes) < 2 || notes[1].AutoSummaryEnabled {
		t.Fatalf("toggle close should patch clicked card as auto summary disabled, got %#v", notes)
	}
}

func TestLarkReplyBridgeCardToggleMentionModePatchesCard(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	notifier := &recordingNotifier{messageID: "bot-card"}
	manager := NewManager(nil, launcher, WithNotifier(notifier))
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	sess, err := manager.CreateSession(context.Background(), "Shortcut")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.UpdateNotifyOnWaiting(context.Background(), sess.ID, true); err != nil {
		t.Fatal(err)
	}
	rt, _ := manager.GetRuntime(sess.ID)
	rt.MarkInputActivity("echo hello\r")
	rt.SetVisibleSnapshot("$ echo hello\nhello\n$")
	event := &callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
		Action: &callback.CallBackAction{Value: map[string]interface{}{
			"easy_terminal_action": "toggle_mention_mode",
			"session_id":           sess.ID,
		}},
		Context: &callback.Context{OpenMessageID: "bot-card"},
	}}

	resp, err := bridge.HandleCardActionTrigger(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Content != "已开启艾特模式" {
		t.Fatalf("unexpected response: %#v", resp)
	}
	notes := waitForNotifierNotes(t, notifier, 1)
	if len(notes) != 1 || notes[0].MessageID != "bot-card" || !notes[0].MentionModeEnabled {
		t.Fatalf("mention mode should patch clicked card as enabled, got %#v", notes)
	}

	resp, err = bridge.HandleCardActionTrigger(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Content != "已关闭艾特模式" {
		t.Fatalf("unexpected close response: %#v", resp)
	}
	notes = waitForNotifierNotes(t, notifier, 2)
	if len(notes) < 2 || notes[1].MentionModeEnabled {
		t.Fatalf("toggle close should patch clicked card as mention mode disabled, got %#v", notes)
	}
}

func TestLarkReplyBridgeDeveloperModeToggleIsOwnerOnly(t *testing.T) {
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.SetDeveloperOpenID("ou-owner")
	sess, err := manager.CreateSession(context.Background(), "Iris")
	if err != nil {
		t.Fatal(err)
	}
	action := &callback.CallBackAction{Value: map[string]interface{}{
		"easy_terminal_action": "toggle_developer_mode",
		"session_id":           sess.ID,
	}}
	resp, err := bridge.handleCardAction(context.Background(), action, "", "", "ou-guest")
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Toast == nil || !strings.Contains(resp.Toast.Content, "只有配置的开发者") {
		t.Fatalf("guest should be rejected: %#v", resp)
	}
	if manager.sessions[sess.ID].Snapshot().DeveloperModeEnabled {
		t.Fatal("guest must not enable developer mode")
	}
	resp, err = bridge.handleCardAction(context.Background(), action, "", "", "ou-owner")
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Content != "已开启开发者模式" {
		t.Fatalf("owner toggle response = %#v", resp)
	}
	if !manager.sessions[sess.ID].Snapshot().DeveloperModeEnabled {
		t.Fatal("owner should enable developer mode")
	}
}

func TestLarkReplyBridgeWorkspaceSelectionRequiresDeveloperModeAndUsesConfiguredPath(t *testing.T) {
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	workspace := t.TempDir()
	manager.SetAgentConfig(AgentConfig{Kind: "codex", Command: "codex"}, []WorkspaceOption{{Label: "项目", Value: workspace, Default: true}})
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	sess, err := manager.CreateSession(context.Background(), "Iris")
	if err != nil {
		t.Fatal(err)
	}
	action := &callback.CallBackAction{Option: workspace, Value: map[string]interface{}{
		"easy_terminal_action": "workspace_select",
		"session_id":           sess.ID,
	}}
	resp, err := bridge.handleCardAction(context.Background(), action, "", "", "ou-member")
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Content != "请先开启开发者模式" {
		t.Fatalf("workspace selection should be hidden behind developer mode: %#v", resp)
	}
	if _, _, err := manager.UpdateDeveloperMode(context.Background(), sess.ID, true); err != nil {
		t.Fatal(err)
	}
	resp, err = bridge.handleCardAction(context.Background(), action, "", "", "ou-member")
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Toast == nil || !strings.Contains(resp.Toast.Content, "已切换目录") {
		t.Fatalf("ordinary members should be able to use visible workspace selector: %#v", resp)
	}
	updated, ok, err := manager.GetSession(context.Background(), sess.ID)
	if err != nil || !ok || updated.LastCWD != workspace {
		t.Fatalf("workspace metadata was not updated: %#v ok=%v err=%v", updated, ok, err)
	}
	if writes := launcher.terminals[0].writes(); !strings.Contains(writes, "/cd "+workspace+"\r") {
		t.Fatalf("Codex workspace command was not submitted: %q", writes)
	}
}

func TestLarkReplyBridgeRestartAgentRequiresDeveloperModeAndReusesStartCommand(t *testing.T) {
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	manager.SetAgentConfig(AgentConfig{Kind: "custom", Command: "my-agent --fast"}, nil)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	sess, err := manager.CreateSession(context.Background(), "Iris")
	if err != nil {
		t.Fatal(err)
	}
	action := &callback.CallBackAction{Value: map[string]interface{}{
		"easy_terminal_action": "restart_agent",
		"session_id":           sess.ID,
	}}
	resp, err := bridge.handleCardAction(context.Background(), action, "", "", "ou-member")
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Content != "请先开启开发者模式" {
		t.Fatalf("restart should require developer mode: %#v", resp)
	}
	if _, _, err := manager.UpdateDeveloperMode(context.Background(), sess.ID, true); err != nil {
		t.Fatal(err)
	}
	before := len(launcher.terminals[0].writeParts())
	resp, err = bridge.handleCardAction(context.Background(), action, "", "", "ou-member")
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Content != "正在重启 Agent" {
		t.Fatalf("restart response = %#v", resp)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(launcher.terminals[0].writeParts()) < before+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	parts := launcher.terminals[0].writeParts()
	if len(parts) < before+2 || parts[before] != "\x03\x03" || parts[before+1] != "my-agent --fast\r" {
		t.Fatalf("restart writes = %#v", parts)
	}
	updated := manager.sessions[sess.ID].Snapshot()
	if updated.LastMode != SessionModeAgent || updated.LastAgentKind != "custom" || updated.LastAgentStartCommand != "my-agent --fast" {
		t.Fatalf("restart recovery state = %#v", updated)
	}
}

func TestLarkReplyBridgeDisabledCardActionsAreBlockedEndToEnd(t *testing.T) {
	tests := []struct {
		name  string
		value map[string]interface{}
	}{
		{
			name: "shortcut",
			value: map[string]interface{}{
				"easy_terminal_action": "shortcut",
				"key":                  "ctrl_c",
			},
		},
		{
			name: "custom shortcut",
			value: map[string]interface{}{
				"easy_terminal_action": "custom_shortcut",
				"command":              "git status",
			},
		},
		{
			name: "refresh",
			value: map[string]interface{}{
				"easy_terminal_action": "refresh",
			},
		},
		{
			name: "toggle auto refresh",
			value: map[string]interface{}{
				"easy_terminal_action": "toggle_auto_refresh",
			},
		},
		{
			name: "toggle auto summary",
			value: map[string]interface{}{
				"easy_terminal_action": "toggle_auto_summary",
			},
		},
		{
			name: "toggle mention mode",
			value: map[string]interface{}{
				"easy_terminal_action": "toggle_mention_mode",
			},
		},
		{
			name: "restart agent",
			value: map[string]interface{}{
				"easy_terminal_action": "restart_agent",
			},
		},
		{
			name: "delete session",
			value: map[string]interface{}{
				"easy_terminal_action": "delete_session",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bridge, rt, launcher, notifier, sessionID := newDisabledCardBridge(t)
			tt.value["session_id"] = sessionID
			event := &callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
				Action:  &callback.CallBackAction{Value: tt.value},
				Context: &callback.Context{OpenMessageID: "old-card"},
			}}

			resp, err := bridge.HandleCardActionTrigger(context.Background(), event)
			if err != nil {
				t.Fatal(err)
			}
			if resp == nil || resp.Toast == nil || resp.Toast.Content != larkDisabledCardToastContent {
				t.Fatalf("disabled card should return disabled toast, got %#v", resp)
			}
			if parts := launcher.terminals[0].writeParts(); len(parts) != 0 {
				t.Fatalf("disabled card should not write terminal input, got %#v", parts)
			}
			if notes := notifier.notes(); len(notes) != 0 {
				t.Fatalf("disabled card should not update notifications, got %#v", notes)
			}
			rt.mu.Lock()
			autoRefreshEnabled := rt.autoRefreshEnabled
			lastInputText := rt.lastInputText
			rt.mu.Unlock()
			if autoRefreshEnabled || lastInputText != "" {
				t.Fatalf("disabled card should not mutate runtime action state, auto=%v input=%q", autoRefreshEnabled, lastInputText)
			}
		})
	}
}

func TestLarkReplyBridgeRefreshCanReanchorClickedCard(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	notifier := &recordingNotifier{}
	manager := NewManager(nil, launcher, WithNotifier(notifier))
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	sess, err := manager.CreateSession(context.Background(), "Refresh")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.UpdateNotifyOnWaiting(context.Background(), sess.ID, true); err != nil {
		t.Fatal(err)
	}
	rt, ok := manager.GetRuntime(sess.ID)
	if !ok {
		t.Fatal("runtime not found")
	}
	rt.SetVisibleSnapshot("> ask\nanswer\n>")
	rt.mu.Lock()
	rt.lastInputText = "ask"
	rt.snapshotAtRoundStart = ">"
	rt.snapshotAtRoundStartSet = true
	rt.lastNotifiedMessageID = "newer-card"
	rt.lastNotifiedContent = "newer content"
	rt.mu.Unlock()
	defaultLarkMessageRegistry.remember(sess.ID, "clicked-card", "newer-card")
	event := &callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
		Action: &callback.CallBackAction{Value: map[string]interface{}{
			"easy_terminal_action": "refresh",
			"session_id":           sess.ID,
		}},
		Context: &callback.Context{OpenMessageID: "clicked-card"},
	}}

	resp, err := bridge.HandleCardActionTrigger(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Content != "刷新成功" {
		t.Fatalf("refresh should be accepted, got %#v", resp)
	}
	notes := waitForNotifierNotes(t, notifier, 1)
	if len(notes) != 1 || notes[0].MessageID != "clicked-card" {
		t.Fatalf("refresh should patch clicked card, got %#v", notes)
	}
	rt.mu.Lock()
	last := rt.lastNotifiedMessageID
	rt.mu.Unlock()
	if last != "clicked-card" {
		t.Fatalf("refresh should reanchor clicked card, got %q", last)
	}
}

func waitForNotifierNotes(t *testing.T, notifier *recordingNotifier, want int) []WaitingNotification {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		notes := notifier.notes()
		if len(notes) >= want {
			return notes
		}
		time.Sleep(10 * time.Millisecond)
	}
	return notifier.notes()
}

func newDisabledCardBridge(t *testing.T) (*LarkReplyBridge, *RuntimeSession, *recordingLauncher, *recordingNotifier, string) {
	t.Helper()
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	notifier := &recordingNotifier{messageID: "latest-card"}
	manager := NewManager(nil, launcher, WithNotifier(notifier), WithAutoRefreshInterval(80*time.Millisecond))
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	sess, err := manager.CreateSession(context.Background(), "Disabled")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.UpdateNotifyOnWaiting(context.Background(), sess.ID, true); err != nil {
		t.Fatal(err)
	}
	rt, ok := manager.GetRuntime(sess.ID)
	if !ok {
		t.Fatal("runtime not found")
	}
	rt.mu.Lock()
	rt.lastNotifiedMessageID = "latest-card"
	rt.lastNotifiedContent = "latest content"
	rt.frozenNotificationMessages = map[string]struct{}{"old-card": {}}
	rt.mu.Unlock()
	defaultLarkMessageRegistry.remember(sess.ID, "old-card")
	defaultLarkMessageRegistry.remember(sess.ID, "latest-card")
	return bridge, rt, launcher, notifier, sess.ID
}

func TestLarkReplyBridgeCardCustomShortcutSubmitsCommand(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	notifier := &recordingNotifier{messageID: "new-card"}
	manager := NewManager(nil, launcher, WithNotifier(notifier))
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	sess, err := manager.CreateSession(context.Background(), "Shortcut")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.UpdateNotifyOnWaiting(context.Background(), sess.ID, true); err != nil {
		t.Fatal(err)
	}
	event := &callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
		Action: &callback.CallBackAction{Value: map[string]interface{}{
			"easy_terminal_action": "custom_shortcut",
			"session_id":           sess.ID,
			"command":              "git status",
		}},
		Context: &callback.Context{OpenMessageID: "bot-card"},
	}}

	resp, err := bridge.HandleCardActionTrigger(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if resp != nil {
		t.Fatalf("unexpected response: %#v", resp)
	}
	writes := launcher.terminals[0].writes()
	if !strings.Contains(writes, "git status") {
		t.Fatalf("custom shortcut should submit command, writes=%q", writes)
	}
	notes := notifier.notes()
	if len(notes) != 1 || notes[0].MessageID != "" || !notes[0].Running {
		t.Fatalf("custom shortcut should create a new running card instead of updating clicked card, got %#v", notes)
	}
	if rt, ok := manager.GetRuntime(sess.ID); !ok || rt.lastInputText != "git status" {
		t.Fatalf("custom shortcut should be recorded as user input, runtime=%v input=%q", ok, rt.lastInputText)
	}
}

func TestLarkReplyBridgeAutoSummaryRunsAfterUserMessageOnly(t *testing.T) {
	resetLarkRegistryForTest()
	previousDelay := structuredInputEnterDelay
	structuredInputEnterDelay = 0
	defer func() { structuredInputEnterDelay = previousDelay }()

	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.SetAutoSummaryPrompt("请总结上一轮输出")
	sess, err := manager.CreateSession(context.Background(), "Summary")
	if err != nil {
		t.Fatal(err)
	}
	rt, ok := manager.GetRuntime(sess.ID)
	if !ok {
		t.Fatal("expected runtime")
	}
	if enabled, err := rt.ToggleAutoSummary(); err != nil || !enabled {
		t.Fatalf("ToggleAutoSummary enabled=%v err=%v", enabled, err)
	}
	defaultLarkMessageRegistry.rememberChat("oc-summary", sess.ID)

	if err := bridge.HandleP2MessageReceive(context.Background(), p2MessageWithChat("m-summary", "", "", "text", `{"text":"echo hello"}`, "group", "oc-summary", "ou-user")); err != nil {
		t.Fatal(err)
	}
	waitForTerminalText(t, launcher.terminals[0], "请总结上一轮输出", 1500*time.Millisecond)
	writes := launcher.terminals[0].writes()
	if !strings.Contains(writes, "echo hello") || !strings.Contains(writes, "请总结上一轮输出") {
		t.Fatalf("auto summary should write original input then prompt, got %q", writes)
	}
}

func TestShouldScheduleAutoSummarySkipsNumericAndSlashCommands(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{input: "", want: false},
		{input: "123", want: false},
		{input: "  456  ", want: false},
		{input: "/c", want: false},
		{input: " /stop", want: false},
		{input: "／c", want: false},
		{input: "echo hello", want: true},
		{input: "123 abc", want: true},
	}
	for _, tt := range tests {
		if got := shouldScheduleAutoSummary(tt.input); got != tt.want {
			t.Fatalf("shouldScheduleAutoSummary(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestLarkReplyBridgeAutoSummarySkipsNumericAndSlashCommands(t *testing.T) {
	resetLarkRegistryForTest()
	previousDelay := structuredInputEnterDelay
	structuredInputEnterDelay = 0
	defer func() { structuredInputEnterDelay = previousDelay }()

	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.SetAutoSummaryPrompt("请总结上一轮输出")
	sess, err := manager.CreateSession(context.Background(), "Summary")
	if err != nil {
		t.Fatal(err)
	}
	rt, ok := manager.GetRuntime(sess.ID)
	if !ok {
		t.Fatal("expected runtime")
	}
	if enabled, err := rt.ToggleAutoSummary(); err != nil || !enabled {
		t.Fatalf("ToggleAutoSummary enabled=%v err=%v", enabled, err)
	}
	defaultLarkMessageRegistry.rememberChat("oc-summary-skip", sess.ID)

	if err := bridge.HandleP2MessageReceive(context.Background(), p2MessageWithChat("m-summary-number", "", "", "text", `{"text":"123"}`, "group", "oc-summary-skip", "ou-user")); err != nil {
		t.Fatal(err)
	}
	if err := bridge.HandleP2MessageReceive(context.Background(), p2MessageWithChat("m-summary-command", "", "", "text", `{"text":"/noop"}`, "group", "oc-summary-skip", "ou-user")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	if writes := launcher.terminals[0].writes(); strings.Contains(writes, "请总结上一轮输出") {
		t.Fatalf("numeric and slash commands should not trigger auto summary, got %q", writes)
	}
}

func TestLarkReplyBridgeAutoSummaryDoesNotCreateOrDisableCards(t *testing.T) {
	resetLarkRegistryForTest()
	previousDelay := structuredInputEnterDelay
	structuredInputEnterDelay = 0
	defer func() { structuredInputEnterDelay = previousDelay }()

	launcher := &recordingLauncher{}
	notifier := &recordingNotifier{messageID: "running-card"}
	manager := NewManager(nil, launcher, WithNotifier(notifier))
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.SetAutoSummaryPrompt("请总结上一轮输出")
	sess, err := manager.CreateSession(context.Background(), "Summary")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.UpdateNotifyOnWaiting(context.Background(), sess.ID, true); err != nil {
		t.Fatal(err)
	}
	rt, ok := manager.GetRuntime(sess.ID)
	if !ok {
		t.Fatal("expected runtime")
	}
	if enabled, err := rt.ToggleAutoSummary(); err != nil || !enabled {
		t.Fatalf("ToggleAutoSummary enabled=%v err=%v", enabled, err)
	}
	defaultLarkMessageRegistry.rememberChat("oc-summary-card", sess.ID)

	if err := bridge.HandleP2MessageReceive(context.Background(), p2MessageWithChat("m-summary-card", "", "", "text", `{"text":"echo hello"}`, "group", "oc-summary-card", "ou-user")); err != nil {
		t.Fatal(err)
	}
	notes := waitForNotifierNotes(t, notifier, 1)
	if len(notes) != 1 || !notes[0].Running || notes[0].Disabled {
		t.Fatalf("user input should create one running card, got %#v", notes)
	}

	waitForTerminalText(t, launcher.terminals[0], "请总结上一轮输出", 1500*time.Millisecond)
	time.Sleep(150 * time.Millisecond)
	notes = notifier.notes()
	if len(notes) != 1 {
		t.Fatalf("auto summary should not create or disable notification cards, got %#v", notes)
	}
	rt.mu.Lock()
	messageID := rt.lastNotifiedMessageID
	running := rt.notificationRunning
	lastInput := rt.lastInputText
	rt.mu.Unlock()
	if messageID != "running-card" || !running || lastInput != "echo hello" {
		t.Fatalf("auto summary should keep original notification state, message=%q running=%v input=%q", messageID, running, lastInput)
	}
}

func TestLarkReplyBridgeCardShortcutsDoNotTriggerAutoSummary(t *testing.T) {
	resetLarkRegistryForTest()
	previousDelay := structuredInputEnterDelay
	structuredInputEnterDelay = 0
	defer func() { structuredInputEnterDelay = previousDelay }()

	launcher := &recordingLauncher{}
	notifier := &recordingNotifier{messageID: "new-card"}
	manager := NewManager(nil, launcher, WithNotifier(notifier))
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.SetAutoSummaryPrompt("请总结上一轮输出")
	sess, err := manager.CreateSession(context.Background(), "Summary")
	if err != nil {
		t.Fatal(err)
	}
	rt, ok := manager.GetRuntime(sess.ID)
	if !ok {
		t.Fatal("expected runtime")
	}
	if enabled, err := rt.ToggleAutoSummary(); err != nil || !enabled {
		t.Fatalf("ToggleAutoSummary enabled=%v err=%v", enabled, err)
	}

	customEvent := &callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
		Action: &callback.CallBackAction{Value: map[string]interface{}{
			"easy_terminal_action": "custom_shortcut",
			"session_id":           sess.ID,
			"command":              "git status",
		}},
		Context: &callback.Context{OpenMessageID: "bot-card"},
	}}
	if resp, err := bridge.HandleCardActionTrigger(context.Background(), customEvent); err != nil || resp != nil {
		t.Fatalf("custom shortcut resp=%#v err=%v", resp, err)
	}

	shortcutEvent := &callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
		Action: &callback.CallBackAction{Value: map[string]interface{}{
			"easy_terminal_action": "shortcut",
			"session_id":           sess.ID,
			"key":                  "enter",
		}},
		Context: &callback.Context{OpenMessageID: "bot-card"},
	}}
	if resp, err := bridge.HandleCardActionTrigger(context.Background(), shortcutEvent); err != nil || resp != nil {
		t.Fatalf("shortcut resp=%#v err=%v", resp, err)
	}

	time.Sleep(1200 * time.Millisecond)
	if writes := launcher.terminals[0].writes(); strings.Contains(writes, "请总结上一轮输出") {
		t.Fatalf("card shortcuts should not trigger auto summary, got %q", writes)
	}
}

func TestLarkReplyBridgeStartRunsConfiguredPresets(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.SetStartPresets(map[string]SessionStartPreset{
		"12": {Commands: []string{"mkdir -p {{session_name}}", "cd {{session_name}}", "codex"}},
	})

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-presets", "", "", "text", `{"text":"开始 测试 12"}`)); err != nil {
		t.Fatal(err)
	}
	sessions, err := manager.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Name != "测试" {
		t.Fatalf("preset suffix should not be part of session name, got %#v", sessions)
	}
	parts := launcher.terminals[0].writeParts()
	want := []string{
		"mkdir -p ${HOME}/'Easy_Terminal_Workspace/测试'\r",
		"cd ${HOME}/'Easy_Terminal_Workspace/测试'\r",
		"mkdir -p '测试'\r",
		"cd '测试'\r",
		"codex\r",
	}
	if len(parts) != len(want) {
		t.Fatalf("preset writes = %#v, want %#v", parts, want)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("preset write %d = %q, want %q; all writes=%#v", i, parts[i], want[i], parts)
		}
	}
}

func TestLarkReplyBridgeStartRunsStringPresetCode(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.SetStartPresets(map[string]SessionStartPreset{
		"setup":    {Commands: []string{"setup-project"}},
		"qa-agent": {Commands: []string{"qa-agent --run"}},
	})

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-string-code", "", "", "text", `{"text":"开始 测试 setup,qa-agent"}`)); err != nil {
		t.Fatal(err)
	}
	sessions, err := manager.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Name != "测试" {
		t.Fatalf("string preset suffix should not be part of session name, got %#v", sessions)
	}
	parts := launcher.terminals[0].writeParts()
	want := []string{
		"mkdir -p ${HOME}/'Easy_Terminal_Workspace/测试'\r",
		"cd ${HOME}/'Easy_Terminal_Workspace/测试'\r",
		"setup-project\r",
		"qa-agent --run\r",
	}
	if len(parts) != len(want) {
		t.Fatalf("string preset writes = %#v, want %#v", parts, want)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("string preset write %d = %q, want %q; all writes=%#v", i, parts[i], want[i], parts)
		}
	}
}

func TestLarkReplyBridgeStartWithoutCodesUsesDefaultAgentPreset(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.SetStartPresets(map[string]SessionStartPreset{
		"999999": {Commands: []string{"codex --dangerously-bypass-approvals-and-sandbox"}},
	})

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-no-code", "", "", "text", `{"text":"开始 测试"}`)); err != nil {
		t.Fatal(err)
	}
	parts := launcher.terminals[0].writeParts()
	want := []string{
		"mkdir -p ${HOME}/'Easy_Terminal_Workspace/测试'\r",
		"cd ${HOME}/'Easy_Terminal_Workspace/测试'\r",
		"codex --dangerously-bypass-approvals-and-sandbox\r",
	}
	if len(parts) != len(want) {
		t.Fatalf("default workspace writes = %#v, want %#v", parts, want)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("default workspace write %d = %q, want %q; all writes=%#v", i, parts[i], want[i], parts)
		}
	}
}

func TestLarkReplyBridgeSlashStartWithoutCodesUsesDefaultAgentPreset(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.SetStartPresets(map[string]SessionStartPreset{
		"999999": {Commands: []string{"codex --dangerously-bypass-approvals-and-sandbox"}},
	})

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-slash-start-no-code", "", "", "text", `{"text":"/start 测试"}`)); err != nil {
		t.Fatal(err)
	}
	parts := launcher.terminals[0].writeParts()
	want := []string{
		"mkdir -p ${HOME}/'Easy_Terminal_Workspace/测试'\r",
		"cd ${HOME}/'Easy_Terminal_Workspace/测试'\r",
		"codex --dangerously-bypass-approvals-and-sandbox\r",
	}
	if len(parts) != len(want) {
		t.Fatalf("slash start default workspace writes = %#v, want %#v", parts, want)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("slash start default workspace write %d = %q, want %q; all writes=%#v", i, parts[i], want[i], parts)
		}
	}
}

func TestLarkReplyBridgeStartCodeZeroOnlyEntersWorkspace(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.SetStartPresets(map[string]SessionStartPreset{
		"0": {Commands: []string{"codex --dangerously-bypass-approvals-and-sandbox"}},
	})

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-code-zero", "", "", "text", `{"text":"开始 测试 0"}`)); err != nil {
		t.Fatal(err)
	}
	parts := launcher.terminals[0].writeParts()
	want := []string{
		"mkdir -p ${HOME}/'Easy_Terminal_Workspace/测试'\r",
		"cd ${HOME}/'Easy_Terminal_Workspace/测试'\r",
	}
	if len(parts) != len(want) {
		t.Fatalf("code zero writes = %#v, want %#v", parts, want)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("code zero write %d = %q, want %q; all writes=%#v", i, parts[i], want[i], parts)
		}
	}
}

func TestLarkReplyBridgeStartDefaultAgentPresetUsesReservedCode(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.SetStartPresets(map[string]SessionStartPreset{
		"999999": {Commands: []string{"codex --dangerously-bypass-approvals-and-sandbox"}},
	})

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-default-agent-code", "", "", "text", `{"text":"开始 测试 999999"}`)); err != nil {
		t.Fatal(err)
	}
	parts := launcher.terminals[0].writeParts()
	want := []string{
		"mkdir -p ${HOME}/'Easy_Terminal_Workspace/测试'\r",
		"cd ${HOME}/'Easy_Terminal_Workspace/测试'\r",
		"codex --dangerously-bypass-approvals-and-sandbox\r",
	}
	if len(parts) != len(want) {
		t.Fatalf("default agent writes = %#v, want %#v", parts, want)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("default agent write %d = %q, want %q; all writes=%#v", i, parts[i], want[i], parts)
		}
	}
}

func TestLarkReplyBridgeStartUsesConfiguredDefaultName(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.SetDefaultStartSessionName("默认会话")

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-default", "", "", "text", `{"text":"开始"}`)); err != nil {
		t.Fatal(err)
	}
	sessions, err := manager.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Name != "默认会话" {
		t.Fatalf("start command should use configured default session name, got %#v", sessions)
	}
}

func TestLarkReplyBridgeStartWithoutDefaultKeepsFallbackBehavior(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-no-default", "", "", "text", `{"text":"开始"}`)); err != nil {
		t.Fatal(err)
	}
	sessions, err := manager.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Name != "lark-session" {
		t.Fatalf("start without configured default should keep fallback behavior, got %#v", sessions)
	}
	parts := launcher.terminals[0].writeParts()
	if !lastSubmittedWrite(parts, "开始") {
		t.Fatalf("fallback session should receive original text, got %#v", parts)
	}
}

func TestLarkReplyBridgeStartRunsNamePresetOnExactMatch(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.SetNamePresets(map[string]SessionStartPreset{
		"会话 A": {Commands: []string{"cd sessions/a", "echo {{session_name_raw}}"}},
	})

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-name-preset", "", "", "text", `{"text":"开始 会话 A"}`)); err != nil {
		t.Fatal(err)
	}
	parts := launcher.terminals[0].writeParts()
	want := []string{"cd sessions/a\r", "echo 会话 A\r"}
	if len(parts) != len(want) {
		t.Fatalf("name preset writes = %#v, want %#v", parts, want)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("name preset write %d = %q, want %q; all writes=%#v", i, parts[i], want[i], parts)
		}
	}
}

func TestLarkReplyBridgeStartNamePresetRequiresExactMatch(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.SetNamePresets(map[string]SessionStartPreset{
		"会话 A": {Commands: []string{"cd sessions/a"}},
	})

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-name-preset-miss", "", "", "text", `{"text":"开始 会话 A 草稿"}`)); err != nil {
		t.Fatal(err)
	}
	parts := launcher.terminals[0].writeParts()
	want := []string{
		"mkdir -p ${HOME}/'Easy_Terminal_Workspace/会话 A 草稿'\r",
		"cd ${HOME}/'Easy_Terminal_Workspace/会话 A 草稿'\r",
	}
	if len(parts) != len(want) {
		t.Fatalf("non-exact name preset should only run default workspace commands, got %#v want %#v", parts, want)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("workspace write %d = %q, want %q; all writes=%#v", i, parts[i], want[i], parts)
		}
	}
}

func TestLarkReplyBridgeStartNamePresetTakesPriorityOverCodePresets(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.SetNamePresets(map[string]SessionStartPreset{
		"会话 A": {Commands: []string{"name-one", "name-two"}},
	})
	bridge.SetStartPresets(map[string]SessionStartPreset{
		"1": {Commands: []string{"code-one"}},
	})

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-name-before-code", "", "", "text", `{"text":"开始 会话 A 1"}`)); err != nil {
		t.Fatal(err)
	}
	sessions, err := manager.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Name != "会话 A" {
		t.Fatalf("preset suffix should not be part of session name, got %#v", sessions)
	}
	parts := launcher.terminals[0].writeParts()
	want := []string{"name-one\r", "name-two\r"}
	if len(parts) != len(want) {
		t.Fatalf("preset writes = %#v, want %#v", parts, want)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("preset write %d = %q, want %q; all writes=%#v", i, parts[i], want[i], parts)
		}
	}
}

func TestLarkReplyBridgeExactNamePresetCanEndWithStringCode(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.SetNamePresets(map[string]SessionStartPreset{
		"会话 dev": {Commands: []string{"name-preset"}},
	})
	bridge.SetStartPresets(map[string]SessionStartPreset{
		"dev": {Commands: []string{"code-preset"}},
	})

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-name-ending-code", "", "", "text", `{"text":"开始 会话 dev"}`)); err != nil {
		t.Fatal(err)
	}
	sessions, err := manager.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Name != "会话 dev" {
		t.Fatalf("exact name preset should not be split as code suffix, got %#v", sessions)
	}
	parts := launcher.terminals[0].writeParts()
	want := []string{"name-preset\r"}
	if len(parts) != len(want) {
		t.Fatalf("exact name preset writes = %#v, want %#v", parts, want)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("exact name preset write %d = %q, want %q; all writes=%#v", i, parts[i], want[i], parts)
		}
	}
}

func TestLarkReplyBridgeStartRunsHyphenSeparatedPresetCodes(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.SetStartPresets(map[string]SessionStartPreset{
		"1":   {Commands: []string{"one"}},
		"23":  {Commands: []string{"twenty-three"}},
		"223": {Commands: []string{"two-two-three"}},
	})

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-hyphen-presets", "", "", "text", `{"text":"开始 测试 1-23-223"}`)); err != nil {
		t.Fatal(err)
	}
	sessions, err := manager.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Name != "测试" {
		t.Fatalf("hyphen preset suffix should not be part of session name, got %#v", sessions)
	}
	parts := launcher.terminals[0].writeParts()
	want := []string{
		"mkdir -p ${HOME}/'Easy_Terminal_Workspace/测试'\r",
		"cd ${HOME}/'Easy_Terminal_Workspace/测试'\r",
		"one\r",
		"twenty-three\r",
		"two-two-three\r",
	}
	if len(parts) != len(want) {
		t.Fatalf("preset writes = %#v, want %#v", parts, want)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("preset write %d = %q, want %q; all writes=%#v", i, parts[i], want[i], parts)
		}
	}
}

func TestLarkReplyBridgeStartPresetQuotesVariables(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	bridge.SetStartPresets(map[string]SessionStartPreset{
		"1": {Commands: []string{"mkdir -p {{session_name}}", "echo {{session_name_raw}}"}},
	})

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-quoted", "", "", "text", `{"text":"开始 项目 O'Brien 1"}`)); err != nil {
		t.Fatal(err)
	}
	parts := launcher.terminals[0].writeParts()
	if len(parts) != 4 {
		t.Fatalf("preset writes = %#v", parts)
	}
	if parts[0] != "mkdir -p ${HOME}/'Easy_Terminal_Workspace/项目 O'\\''Brien'\r" {
		t.Fatalf("workspace mkdir write = %q", parts[0])
	}
	if parts[1] != "cd ${HOME}/'Easy_Terminal_Workspace/项目 O'\\''Brien'\r" {
		t.Fatalf("workspace cd write = %q", parts[1])
	}
	if parts[2] != "mkdir -p '项目 O'\\''Brien'\r" {
		t.Fatalf("quoted session name write = %q", parts[2])
	}
	if parts[3] != "echo 项目 O'Brien\r" {
		t.Fatalf("raw session name write = %q", parts[3])
	}
}

func TestLarkReplyBridgePipelineRunsNextCommandAfterNotification(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-pipe", "", "", "text", `{"text":"开始 Pipe会话"}`)); err != nil {
		t.Fatal(err)
	}
	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-pipeline", "m-start-pipe", "", "text", `{"text":"pwd | cd /tmp | pwd"}`)); err != nil {
		t.Fatal(err)
	}
	parts := launcher.terminals[0].writeParts()
	if !lastSubmittedWrite(parts, "pwd") {
		t.Fatalf("first pipeline command should be submitted immediately, got %#v", parts)
	}
	if strings.Contains(launcher.terminals[0].writes(), "cd /tmp") {
		t.Fatalf("later pipeline commands should wait for notification, writes: %q", launcher.terminals[0].writes())
	}

	bridge.OnNotificationSent("sess-1")
	parts = launcher.terminals[0].writeParts()
	if !lastSubmittedWrite(parts, "cd /tmp") {
		t.Fatalf("second pipeline command should run after notification, got %#v", parts)
	}
	if strings.Contains(launcher.terminals[0].writes(), "pwdpwd") {
		t.Fatalf("pipeline commands should be submitted as separate turns, writes: %q", launcher.terminals[0].writes())
	}

	bridge.OnNotificationSent("sess-1")
	parts = launcher.terminals[0].writeParts()
	if !lastSubmittedWrite(parts, "pwd") {
		t.Fatalf("third pipeline command should run after next notification, got %#v", parts)
	}
}

func TestSplitLarkPipelineSupportsEscapedPipe(t *testing.T) {
	got := splitLarkPipeline(`echo a \| b | pwd`)
	want := []string{"echo a | b", "pwd"}
	if len(got) != len(want) {
		t.Fatalf("split length = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("part %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSplitLarkPipelineSupportsFullWidthPipe(t *testing.T) {
	got := splitLarkPipeline("开始 测试 ｜ pwd")
	want := []string{"开始 测试", "pwd"}
	if len(got) != len(want) {
		t.Fatalf("split length = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("part %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLarkReplyBridgeStartPipelineWithFullWidthPipe(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-wide-pipe", "", "", "text", `{"text":"开始 测试 ｜ pwd"}`)); err != nil {
		t.Fatal(err)
	}
	sessions, err := manager.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Name != "测试" {
		t.Fatalf("start pipeline should use only first segment as session name, got %#v", sessions)
	}
	if got := launcher.terminals[0].writes(); strings.Contains(got, "pwd") {
		t.Fatalf("queued command should wait for first notification, writes: %q", got)
	}

	bridge.OnNotificationSent("sess-1")
	parts := launcher.terminals[0].writeParts()
	if !lastSubmittedWrite(parts, "pwd") {
		t.Fatalf("queued start pipeline command should run after notification, got %#v", parts)
	}
}

func TestLarkReplyBridgeRoutesP1Start(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	var reactions []string
	bridge.addReaction = func(_ context.Context, messageID string, emoji string) error {
		reactions = append(reactions, messageID+":"+emoji)
		return nil
	}

	err := bridge.HandleP1MessageReceive(context.Background(), &larkim.P1MessageReceiveV1{
		Event: &larkim.P1MessageReceiveV1Data{
			OpenMessageID:    "p1-start",
			TextWithoutAtBot: "新会话 P1会话",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := manager.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Name != "P1会话" {
		t.Fatalf("unexpected sessions: %#v", sessions)
	}
	if !sessions[0].NotifyOnWaiting {
		t.Fatalf("P1 lark-created session should enable notifications by default: %#v", sessions[0])
	}
	if len(reactions) != 1 || reactions[0] != "p1-start:"+larkProcessingReactionEmoji {
		t.Fatalf("expected processing reaction on P1 message, got %#v", reactions)
	}
}

func TestLarkReplyBridgeFallbackSessionEnablesNotifications(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-fallback", "", "", "text", `{"text":"echo no explicit session"}`)); err != nil {
		t.Fatal(err)
	}
	sessions, err := manager.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || !sessions[0].NotifyOnWaiting {
		t.Fatalf("fallback lark session should enable notifications by default: %#v", sessions)
	}
}

func TestLarkReplyBridgeCurrentRoundCommandRepliesWithoutWritingTerminal(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	var replies []string
	bridge.replyText = func(_ context.Context, messageID string, text string) error {
		replies = append(replies, messageID+":"+text)
		return nil
	}

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-c", "", "", "text", `{"text":"开始 C会话"}`)); err != nil {
		t.Fatal(err)
	}
	rt, ok := manager.GetRuntime("sess-1")
	if !ok {
		t.Fatal("expected sess-1 runtime")
	}
	setTrustedLegacyRoundFixture(rt, "›", "今天天气怎么样\r", strings.Join([]string{
		"> 今天天气怎么样",
		"• 你想查哪个城市的天气？",
		"比如：上海、北京、纽约。",
	}, "\n"))

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-current", "m-start-c", "", "text", `{"text":"/c"}`)); err != nil {
		t.Fatal(err)
	}
	if got := launcher.terminals[0].writes(); strings.Contains(got, "/c") {
		t.Fatalf("/c should not be sent to terminal, writes: %q", got)
	}
	if len(replies) != 1 {
		t.Fatalf("expected one lark reply, got %#v", replies)
	}
	if strings.Contains(replies[0], "> 今天天气怎么样") || !strings.Contains(replies[0], "你想查哪个城市") {
		t.Fatalf("reply did not include current round content: %#v", replies)
	}
}

func TestLarkReplyBridgeCurrentRoundCommandKeepsRawFilteredAndToolContent(t *testing.T) {
	resetLarkRegistryForTest()
	if err := SetLarkNotifyDropLineRules([]LarkNotifyDropLineRule{
		{Kind: "line", Pattern: `^FILTER_ME$`, Action: "drop_line"},
		{Kind: "block_head", Pattern: `^• Ran\b.*`, Action: "drop_block"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := SetLarkNotifyDropLineRules(nil); err != nil {
			t.Fatal(err)
		}
	})
	SetLarkNotifyMaxLines(1)
	t.Cleanup(func() { SetLarkNotifyMaxLines(defaultMaxLarkTextLines) })

	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	var reply string
	bridge.replyText = func(_ context.Context, _ string, text string) error {
		reply = text
		return nil
	}

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-raw", "", "", "text", `{"text":"开始 Raw会话"}`)); err != nil {
		t.Fatal(err)
	}
	rt, ok := manager.GetRuntime("sess-1")
	if !ok {
		t.Fatal("expected sess-1 runtime")
	}
	want := strings.Join([]string{
		"FILTER_ME",
		"• Ran go test ./...",
		"  tool output that configured block filtering would remove",
		"contact dev@example.com",
		"FINAL_RAW_LINE",
	}, "\n")
	setTrustedLegacyRoundFixture(rt, "$", "show raw\r", "$ show raw\n"+want)

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-current-raw", "m-start-raw", "", "text", `{"text":"/c"}`)); err != nil {
		t.Fatal(err)
	}
	if reply != want {
		t.Fatalf("/c must bypass configured filtering, tool-block removal, email sanitizing, and truncation:\n%q\nwant:\n%q", reply, want)
	}
}

func TestLarkReplyBridgeCurrentRoundCommandFallsBackToRawVisibleSnapshotWithoutAnchor(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	var reply string
	bridge.replyText = func(_ context.Context, _ string, text string) error {
		reply = text
		return nil
	}

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-raw-full", "", "", "text", `{"text":"开始 RawFull会话"}`)); err != nil {
		t.Fatal(err)
	}
	rt, ok := manager.GetRuntime("sess-1")
	if !ok {
		t.Fatal("expected sess-1 runtime")
	}
	want := "OLD_HISTORY_IS_EXPLICITLY_REQUESTED\n› prompt without a trusted baseline\n• raw visible reply"
	rt.SetVisibleSnapshot("\n" + want + "\n")

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-current-raw-full", "m-start-raw-full", "", "text", `{"text":"/c"}`)); err != nil {
		t.Fatal(err)
	}
	if reply != want {
		t.Fatalf("unanchored /c must return the trimmed complete visible snapshot:\n%q\nwant:\n%q", reply, want)
	}
}

func TestLarkReplyBridgeStopCommandSendsCtrlC(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-stop", "", "", "text", `{"text":"开始 Stop会话"}`)); err != nil {
		t.Fatal(err)
	}
	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-stop", "m-start-stop", "", "text", `{"text":"/stop"}`)); err != nil {
		t.Fatal(err)
	}
	parts := launcher.terminals[0].writeParts()
	if len(parts) == 0 || parts[len(parts)-1] != "\x03" {
		t.Fatalf("/stop should send Ctrl-C to terminal, got %#v", parts)
	}
	if strings.Contains(launcher.terminals[0].writes(), "/stop") {
		t.Fatalf("/stop should not be sent as text, writes: %q", launcher.terminals[0].writes())
	}
}

func TestLarkReplyBridgeStopCommandWithoutSessionDoesNotCreateTerminal(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	var reply string
	bridge.replyText = func(_ context.Context, _ string, text string) error {
		reply = text
		return nil
	}

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-stop-missing", "", "", "text", `{"text":"/stop"}`)); err != nil {
		t.Fatal(err)
	}
	if len(launcher.terminals) != 0 {
		t.Fatalf("/stop without a session should not create a terminal, got %d", len(launcher.terminals))
	}
	if reply != "未找到会话" {
		t.Fatalf("reply = %q, want 未找到会话", reply)
	}
}

func TestLarkReplyBridgeCurrentRoundCommandUsesRepliedNotificationSession(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	var reply string
	bridge.replyText = func(_ context.Context, _ string, text string) error {
		reply = text
		return nil
	}

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-a", "", "", "text", `{"text":"开始 A会话"}`)); err != nil {
		t.Fatal(err)
	}
	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-start-b", "", "", "text", `{"text":"开始 B会话"}`)); err != nil {
		t.Fatal(err)
	}
	rtA, ok := manager.GetRuntime("sess-1")
	if !ok {
		t.Fatal("expected sess-1 runtime")
	}
	setTrustedLegacyRoundFixture(rtA, "eleven ~ >", "echo A\r", "eleven ~ > echo A\nA content\neleven ~ >")
	rtB, ok := manager.GetRuntime("sess-2")
	if !ok {
		t.Fatal("expected sess-2 runtime")
	}
	setTrustedLegacyRoundFixture(rtB, "eleven ~ >", "echo B\r", "eleven ~ > echo B\nB content\neleven ~ >")
	defaultLarkMessageRegistry.remember("sess-1", "bot-notify-a")
	defaultLarkMessageRegistry.rememberLatest("sess-2")

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-current-a", "bot-notify-a", "", "text", `{"text":"/c"}`)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "A content") || strings.Contains(reply, "B content") {
		t.Fatalf("/c should use replied notification session, reply=%q", reply)
	}
}

func TestLarkReplyBridgeCurrentRoundCommandWithoutSessionDoesNotCreateTerminal(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	var reply string
	bridge.replyText = func(_ context.Context, _ string, text string) error {
		reply = text
		return nil
	}

	if err := bridge.HandleP2MessageReceive(context.Background(), p2Message("m-current-missing", "", "", "text", `{"text":"/c"}`)); err != nil {
		t.Fatal(err)
	}
	if len(launcher.terminals) != 0 {
		t.Fatalf("/c without a session should not create a terminal, got %d", len(launcher.terminals))
	}
	if reply != "未找到会话" {
		t.Fatalf("reply = %q, want 未找到会话", reply)
	}
}

func resetLarkRegistryForTest() {
	defaultLarkMessageRegistry.mu.Lock()
	defer defaultLarkMessageRegistry.mu.Unlock()
	defaultLarkMessageRegistry.messageToSession = make(map[string]string)
	defaultLarkMessageRegistry.latestSessionID = ""
	defaultLarkMessageRegistry.chatToSession = make(map[string]string)
}

func p2MessageWithChat(messageID, parentID, rootID, messageType, content, chatType, chatID, openID string) *larkim.P2MessageReceiveV1 {
	msg := p2MessageWithSender(messageID, parentID, rootID, messageType, content, "user")
	msg.Event.Message.ChatId = strPtr(chatID)
	msg.Event.Message.ChatType = strPtr(chatType)
	msg.Event.Sender.SenderId = &larkim.UserId{OpenId: strPtr(openID)}
	return msg
}

func waitForBrowserRequest(t *testing.T, mu *sync.Mutex, requests *[]string, sessionID string) {
	t.Helper()
	for i := 0; i < 50; i++ {
		mu.Lock()
		for _, got := range *requests {
			if got == sessionID {
				mu.Unlock()
				return
			}
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("expected browser request for %s, got %#v", sessionID, *requests)
}

func p2Message(messageID, parentID, rootID, messageType, content string) *larkim.P2MessageReceiveV1 {
	return p2MessageWithSender(messageID, parentID, rootID, messageType, content, "")
}

func p2MessageWithSender(messageID, parentID, rootID, messageType, content, senderType string) *larkim.P2MessageReceiveV1 {
	var sender *larkim.EventSender
	if senderType != "" {
		sender = &larkim.EventSender{SenderType: strPtr(senderType)}
	}
	return &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: sender,
			Message: &larkim.EventMessage{
				MessageId:   strPtr(messageID),
				ParentId:    strPtr(parentID),
				RootId:      strPtr(rootID),
				MessageType: strPtr(messageType),
				Content:     strPtr(content),
			},
		},
	}
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type recordingLauncher struct {
	mu        sync.Mutex
	terminals []*recordingTerminal
}

func (l *recordingLauncher) Launch(context.Context) (ProcessHandle, error) {
	term := &recordingTerminal{readCh: make(chan []byte)}
	l.mu.Lock()
	l.terminals = append(l.terminals, term)
	l.mu.Unlock()
	return recordingHandle{terminal: term}, nil
}

type recordingHandle struct {
	terminal *recordingTerminal
}

func (h recordingHandle) Terminal() Terminal { return h.terminal }
func (h recordingHandle) Process() Waiter    { return blockingWaiter{} }

type recordingTerminal struct {
	mu        sync.Mutex
	buf       strings.Builder
	parts     []string
	writeTime []time.Time
	onWrite   func(string)
	readCh    chan []byte
	closed    bool
}

func (t *recordingTerminal) Read(p []byte) (int, error) {
	b, ok := <-t.readCh
	if !ok {
		return 0, io.EOF
	}
	return copy(p, b), nil
}

func (t *recordingTerminal) Write(p []byte) (int, error) {
	data := string(p)
	t.mu.Lock()
	t.parts = append(t.parts, data)
	t.writeTime = append(t.writeTime, time.Now())
	n, err := t.buf.Write(p)
	onWrite := t.onWrite
	t.mu.Unlock()
	if onWrite != nil {
		onWrite(data)
	}
	return n, err
}

func (t *recordingTerminal) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		close(t.readCh)
		t.closed = true
	}
	return nil
}

func (t *recordingTerminal) Resize(cols, rows uint16) error { return nil }

func (t *recordingTerminal) writes() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}

func (t *recordingTerminal) writeParts() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	cp := make([]string, len(t.parts))
	copy(cp, t.parts)
	return cp
}

func (t *recordingTerminal) writeTimes() []time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	cp := make([]time.Time, len(t.writeTime))
	copy(cp, t.writeTime)
	return cp
}

func waitForTerminalText(t *testing.T, term *recordingTerminal, text string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(term.writes(), text) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("terminal did not contain %q before timeout, got %q", text, term.writes())
}

func lastSubmittedWrite(parts []string, text string) bool {
	return len(parts) >= 2 && parts[len(parts)-2] == text && isEnterWrite(parts[len(parts)-1])
}

func isEnterWrite(text string) bool {
	return text == "\r"
}

type blockingWaiter struct{}

func (blockingWaiter) Wait() error {
	select {}
}
