package session

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

var (
	ErrInvalidAgentContextToken = errors.New("invalid agent context token")
	ErrSessionNotBoundToLark    = errors.New("session is not bound to a Feishu chat")
	ErrLarkContextUnavailable   = errors.New("Feishu context service is unavailable")
)

type LarkChatMetadata struct {
	ChatID   string `json:"chat_id"`
	ChatName string `json:"chat_name"`
	ChatType string `json:"chat_type"`
}

type LarkMessageAttachment struct {
	Kind string `json:"kind"`
	Key  string `json:"key,omitempty"`
	Name string `json:"name,omitempty"`
}

type LarkChatMessage struct {
	MessageID   string                  `json:"message_id"`
	ParentID    string                  `json:"parent_id,omitempty"`
	RootID      string                  `json:"root_id,omitempty"`
	ThreadID    string                  `json:"thread_id,omitempty"`
	MessageType string                  `json:"message_type"`
	Text        string                  `json:"text,omitempty"`
	SenderID    string                  `json:"sender_id,omitempty"`
	SenderType  string                  `json:"sender_type,omitempty"`
	CreatedAt   time.Time               `json:"created_at,omitempty"`
	UpdatedAt   time.Time               `json:"updated_at,omitempty"`
	Deleted     bool                    `json:"deleted,omitempty"`
	Attachments []LarkMessageAttachment `json:"attachments,omitempty"`
}

type LarkAgentContext struct {
	SessionID         string    `json:"session_id"`
	SessionName       string    `json:"session_name"`
	ChatID            string    `json:"chat_id"`
	ChatName          string    `json:"chat_name"`
	ChatType          string    `json:"chat_type,omitempty"`
	LatestMessageID   string    `json:"latest_message_id,omitempty"`
	LatestParentID    string    `json:"latest_parent_id,omitempty"`
	LatestRootID      string    `json:"latest_root_id,omitempty"`
	LatestSenderID    string    `json:"latest_sender_id,omitempty"`
	LatestMessageTime time.Time `json:"latest_message_time,omitempty"`
	UpdatedAt         time.Time `json:"updated_at,omitempty"`
}

type LarkChatMessagePage struct {
	Context  LarkAgentContext  `json:"context"`
	Messages []LarkChatMessage `json:"messages"`
	Count    int               `json:"count"`
}

type LarkConversationProvider interface {
	LarkChatMetadata(context.Context, string) (LarkChatMetadata, error)
	LarkChatMessages(context.Context, string, int) ([]LarkChatMessage, error)
}

func (m *Manager) SetLarkConversationProvider(provider LarkConversationProvider) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.larkConversationProvider = provider
	m.mu.Unlock()
}

func (m *Manager) RecordLarkAgentContext(sessionID string, current LarkAgentContext) {
	if m == nil {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	current.SessionID = sessionID
	if current.UpdatedAt.IsZero() {
		current.UpdatedAt = time.Now().UTC()
	}
	m.mu.Lock()
	if m.larkAgentContexts == nil {
		m.larkAgentContexts = make(map[string]LarkAgentContext)
	}
	previous := m.larkAgentContexts[sessionID]
	if current.ChatID == "" {
		current.ChatID = previous.ChatID
	}
	if current.ChatName == "" {
		current.ChatName = previous.ChatName
	}
	if current.ChatType == "" {
		current.ChatType = previous.ChatType
	}
	if current.LatestMessageID == "" {
		current.LatestMessageID = previous.LatestMessageID
		current.LatestParentID = previous.LatestParentID
		current.LatestRootID = previous.LatestRootID
		current.LatestSenderID = previous.LatestSenderID
		current.LatestMessageTime = previous.LatestMessageTime
	}
	m.larkAgentContexts[sessionID] = current
	m.mu.Unlock()
}

func (m *Manager) AgentLarkContext(ctx context.Context, sessionID, token string) (LarkAgentContext, bool, error) {
	sess, ok, err := m.authorizedAgentSession(ctx, sessionID, token)
	if err != nil || !ok {
		return LarkAgentContext{}, ok, err
	}
	chatID := strings.TrimSpace(sess.LarkChatID)
	if chatID == "" {
		return LarkAgentContext{}, true, ErrSessionNotBoundToLark
	}

	m.mu.RLock()
	current := m.larkAgentContexts[sess.ID]
	provider := m.larkConversationProvider
	m.mu.RUnlock()
	current.SessionID = sess.ID
	current.SessionName = sess.Name
	current.ChatID = chatID
	if current.ChatName == "" {
		current.ChatName = sess.Name
	}
	if provider != nil {
		if metadata, metadataErr := provider.LarkChatMetadata(ctx, chatID); metadataErr == nil {
			if strings.TrimSpace(metadata.ChatName) != "" {
				current.ChatName = strings.TrimSpace(metadata.ChatName)
			}
			if strings.TrimSpace(metadata.ChatType) != "" {
				current.ChatType = strings.TrimSpace(metadata.ChatType)
			}
		}
	}
	if current.ChatName == "" {
		current.ChatName = chatID
	}
	return current, true, nil
}

func (m *Manager) AgentLarkMessages(ctx context.Context, sessionID, token string, limit int) (LarkChatMessagePage, bool, error) {
	current, ok, err := m.AgentLarkContext(ctx, sessionID, token)
	if err != nil || !ok {
		return LarkChatMessagePage{}, ok, err
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	m.mu.RLock()
	provider := m.larkConversationProvider
	m.mu.RUnlock()
	if provider == nil {
		return LarkChatMessagePage{}, true, ErrLarkContextUnavailable
	}
	messages, err := provider.LarkChatMessages(ctx, current.ChatID, limit)
	if err != nil {
		return LarkChatMessagePage{}, true, err
	}
	return LarkChatMessagePage{Context: current, Messages: messages, Count: len(messages)}, true, nil
}

func (m *Manager) authorizedAgentSession(ctx context.Context, sessionID, token string) (Session, bool, error) {
	if m == nil {
		return Session{}, false, nil
	}
	sess, ok, err := m.GetSession(ctx, strings.TrimSpace(sessionID))
	if err != nil || !ok {
		return sess, ok, err
	}
	want := strings.TrimSpace(sess.RecoveryKey)
	got := strings.TrimSpace(token)
	if want == "" || got == "" || len(want) != len(got) || subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		return Session{}, true, ErrInvalidAgentContextToken
	}
	return sess, true, nil
}

func (b *LarkReplyBridge) LarkChatMetadata(ctx context.Context, chatID string) (LarkChatMetadata, error) {
	if b != nil && b.fetchChatMetadata != nil {
		return b.fetchChatMetadata(ctx, chatID)
	}
	return b.fetchLarkChatMetadata(ctx, chatID)
}

func (b *LarkReplyBridge) fetchLarkChatMetadata(ctx context.Context, chatID string) (LarkChatMetadata, error) {
	chatID = strings.TrimSpace(chatID)
	if b == nil || b.apiClient == nil || chatID == "" {
		return LarkChatMetadata{}, ErrLarkContextUnavailable
	}
	req := larkim.NewGetChatReqBuilder().ChatId(chatID).UserIdType("open_id").Build()
	resp, err := b.apiClient.Im.V1.Chat.Get(ctx, req)
	if err != nil {
		return LarkChatMetadata{}, err
	}
	if !resp.Success() {
		return LarkChatMetadata{}, fmt.Errorf("飞书群信息接口返回 code %d: %s", resp.Code, resp.Msg)
	}
	metadata := LarkChatMetadata{ChatID: chatID}
	if resp.Data != nil {
		metadata.ChatName = strings.TrimSpace(valueOf(resp.Data.Name))
		metadata.ChatType = strings.TrimSpace(valueOf(resp.Data.ChatMode))
	}
	return metadata, nil
}

func (b *LarkReplyBridge) LarkChatMessages(ctx context.Context, chatID string, limit int) ([]LarkChatMessage, error) {
	chatID = strings.TrimSpace(chatID)
	if b == nil || b.apiClient == nil || chatID == "" {
		return nil, ErrLarkContextUnavailable
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	req := larkim.NewListMessageReqBuilder().
		ContainerIdType("chat").
		ContainerId(chatID).
		SortType("ByCreateTimeDesc").
		PageSize(limit).
		Build()
	resp, err := b.apiClient.Im.V1.Message.List(ctx, req)
	if err != nil {
		return nil, err
	}
	if !resp.Success() {
		return nil, fmt.Errorf("飞书群消息接口返回 code %d: %s", resp.Code, resp.Msg)
	}
	if resp.Data == nil {
		return []LarkChatMessage{}, nil
	}
	messages := make([]LarkChatMessage, 0, len(resp.Data.Items))
	for _, item := range resp.Data.Items {
		if item == nil {
			continue
		}
		messages = append(messages, normalizeLarkHistoryMessage(item))
	}
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
	return messages, nil
}

func normalizeLarkHistoryMessage(item *larkim.Message) LarkChatMessage {
	messageType := strings.TrimSpace(valueOf(item.MsgType))
	content := ""
	if item.Body != nil {
		content = valueOf(item.Body.Content)
	}
	incoming := extractLarkIncomingMessage(content, messageType)
	message := LarkChatMessage{
		MessageID:   strings.TrimSpace(valueOf(item.MessageId)),
		ParentID:    strings.TrimSpace(valueOf(item.ParentId)),
		RootID:      strings.TrimSpace(valueOf(item.RootId)),
		ThreadID:    strings.TrimSpace(valueOf(item.ThreadId)),
		MessageType: messageType,
		Text:        strings.TrimSpace(incoming.Text),
		Deleted:     item.Deleted != nil && *item.Deleted,
		CreatedAt:   parseLarkMillisecondTime(valueOf(item.CreateTime)),
		UpdatedAt:   parseLarkMillisecondTime(valueOf(item.UpdateTime)),
	}
	if item.Sender != nil {
		message.SenderID = strings.TrimSpace(valueOf(item.Sender.Id))
		message.SenderType = strings.TrimSpace(valueOf(item.Sender.SenderType))
	}
	for _, attachment := range incoming.Attachments {
		message.Attachments = append(message.Attachments, LarkMessageAttachment{
			Kind: attachment.Kind,
			Key:  attachment.Key,
			Name: attachment.Name,
		})
	}
	if message.Text == "" && !message.Deleted {
		message.Text = larkMessageTypePlaceholder(messageType)
	}
	return message
}

func larkMessageTypePlaceholder(messageType string) string {
	switch strings.TrimSpace(messageType) {
	case "image":
		return "[图片]"
	case "file":
		return "[文件]"
	case "audio":
		return "[音频]"
	case "media":
		return "[视频]"
	case "sticker":
		return "[表情]"
	case "interactive":
		return "[卡片消息]"
	case "share_chat":
		return "[群名片]"
	case "share_user":
		return "[个人名片]"
	default:
		return "[" + strings.TrimSpace(messageType) + "]"
	}
}

func parseLarkMillisecondTime(raw string) time.Time {
	milliseconds, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || milliseconds <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(milliseconds).UTC()
}
