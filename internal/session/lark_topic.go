package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type larkTopicReplyKey struct{}

func (rt *RuntimeSession) nameUntitledTopic(ctx context.Context, question string) error {
	rt.mu.Lock()
	if rt.session.LarkTopicRootID == "" || !strings.HasSuffix(rt.session.Name, " · 新话题") || strings.TrimSpace(question) == "" {
		rt.mu.Unlock()
		return nil
	}
	rt.session.Name = strings.TrimSuffix(rt.session.Name, "新话题") + larkTopicTitle(question)
	rt.session.UpdatedAt = time.Now().UTC()
	sess := rt.session
	rt.mu.Unlock()
	return rt.manager.persist(ctx, sess)
}

func parseLarkTopicCommand(text string) (string, bool) {
	parts := strings.Fields(text)
	if len(parts) == 0 || (parts[0] != "/t" && parts[0] != "/topic") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), parts[0])), true
}

func larkTopicTitle(question string) string {
	title := strings.Join(strings.Fields(question), " ")
	if title == "" {
		return "新话题"
	}
	runes := []rune(title)
	if len(runes) > 30 {
		title = string(runes[:30]) + "…"
	}
	return title
}

func (b *LarkReplyBridge) findTopicSession(ctx context.Context, chatID, rootID, threadID string) (Session, bool) {
	if b == nil || b.manager == nil || chatID == "" || (rootID == "" && threadID == "") {
		return Session{}, false
	}
	// ponytail: use the existing session list; index bindings if session counts grow large.
	sessions, err := b.manager.ListSessions(ctx)
	if err != nil {
		return Session{}, false
	}
	for _, sess := range sessions {
		if threadID != "" && sess.LarkThreadID != threadID {
			continue
		}
		if sess.LarkChatID == chatID && sess.LarkTopicRootID != "" &&
			((rootID != "" && sess.LarkTopicRootID == rootID) || (threadID != "" && sess.LarkThreadID == threadID)) {
			return sess, true
		}
	}
	return Session{}, false
}

func (b *LarkReplyBridge) topicReplyContext(ctx context.Context, route larkRouteContext) context.Context {
	return context.WithValue(ctx, larkTopicReplyKey{}, route.ThreadID != "")
}

func (b *LarkReplyBridge) createTopicSession(ctx context.Context, route larkRouteContext, question string, incoming larkIncomingMessage) (sessionID string, err error) {
	defer func() {
		if err != nil {
			_ = b.replyLarkText(ctx, route.MessageID, "创建话题会话失败："+err.Error())
		}
	}()
	if !isLarkGroupChatType(route.ChatType) || route.ChatID == "" || route.MessageID == "" {
		return "", b.replyLarkText(ctx, route.MessageID, "请在群里使用 /t 问题 或 /topic 问题 创建话题。")
	}
	// A reply inside an existing topic cannot create another independent thread.
	if route.ThreadID != "" || route.RootID != "" {
		return "", b.replyLarkText(ctx, route.MessageID, "请回到群里发送 /t 问题，创建一个新的独立话题。")
	}
	b.groupSessionMu.Lock()
	defer b.groupSessionMu.Unlock()
	if existing, found := b.findTopicSession(ctx, route.ChatID, route.MessageID, ""); found {
		return existing.ID, nil
	}
	parent, found, err := b.manager.FindSessionByLarkChatID(ctx, route.ChatID)
	if err != nil {
		return "", err
	}
	if !found || parent.LastAgentStartCommand == "" || parent.LastCWD == "" {
		return "", b.replyLarkText(ctx, route.MessageID, "请先在群主会话中启动 Agent，再创建话题；话题会继承当前 Agent 和目录。")
	}
	if b.apiClient == nil {
		return "", fmt.Errorf("Feishu topic API unavailable")
	}
	groupName := parent.Name
	if b.fetchChatMetadata != nil {
		if meta, err := b.fetchChatMetadata(ctx, route.ChatID); err == nil && strings.TrimSpace(meta.ChatName) != "" {
			groupName = meta.ChatName
		}
	}
	name := "[话题] " + groupName + " · " + larkTopicTitle(question)
	content, _ := json.Marshal(map[string]string{"text": larkTopicTitle(question)})
	req := larkim.NewReplyMessageReqBuilder().MessageId(route.MessageID).Body(
		larkim.NewReplyMessageReqBodyBuilder().MsgType("text").Content(string(content)).ReplyInThread(true).Uuid(larkCreateChatUUID(route.MessageID)).Build(),
	).Build()
	resp, err := b.apiClient.Im.V1.Message.Reply(ctx, req)
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", fmt.Errorf("创建话题失败 code %d: %s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || valueOf(resp.Data.MessageId) == "" || valueOf(resp.Data.ThreadId) == "" {
		return "", fmt.Errorf("飞书未返回新话题 ID")
	}
	ctx = context.WithValue(ctx, larkTopicReplyKey{}, true)
	rootID := valueOf(resp.Data.RootId)
	if rootID == "" {
		rootID = route.MessageID
	}
	seed := Session{LastCWD: parent.LastCWD, LarkChatID: route.ChatID, LarkTopicRootID: rootID, LarkThreadID: valueOf(resp.Data.ThreadId), DeveloperModeEnabled: parent.DeveloperModeEnabled}
	// Only copy the launch command. Recovery identity and Agent home are new.
	agent := AgentConfig{ID: parent.LastAgentID, Kind: parent.LastAgentKind, Command: parent.LastAgentStartCommand}
	sess, err := b.manager.createSession(ctx, name, seed, agent)
	if err != nil {
		return "", err
	}
	b.manager.messageRegistry().remember(sess.ID, route.MessageID, rootID, valueOf(resp.Data.MessageId))
	b.recordAgentLarkContext(sess, route)
	rt, _ := b.manager.GetRuntime(sess.ID)
	rt.SetNotificationMentionOpenID(route.notificationMentionOpenID())
	prompt := "请使用 iris-feishu-context，以 scope=group 读取来源飞书群最近的消息，仅用于了解背景。当前是独立的话题会话；不要恢复或执行历史任务。"
	if question == "" && len(incoming.Attachments) == 0 {
		prompt += "了解背景后等待用户的新输入。"
	} else {
		prompt += "然后只回答下面这条新问题：\n\n" + question
	}
	if incoming.Referenced != nil {
		prompt = formatLarkReferencedInput(incoming.Referenced, prompt)
	}
	if len(incoming.Attachments) > 0 {
		// Reuse attachment delivery after startup; never send file paths before download.
		_, err = b.enableLarkSessionNotifications(ctx, sess)
		if err != nil {
			return sess.ID, err
		}
		route.RootID, route.ThreadID = rootID, sess.LarkThreadID
		return b.routeAttachments(ctx, route, prompt, []string{prompt}, incoming.Attachments, route.MessageID)
	}
	b.enqueuePipelineWithFirstMode(sess.ID, []string{prompt}, route.notificationMentionOpenID(), true, route)
	_, err = b.enableLarkSessionNotifications(ctx, sess)
	if err == nil {
		rt.beginStartupNotification(route.notificationMentionOpenID())
	}
	return sess.ID, err
}
