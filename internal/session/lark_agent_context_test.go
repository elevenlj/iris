package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type fakeLarkConversationProvider struct {
	metadata LarkChatMetadata
	messages []LarkChatMessage
	err      error
	chatID   string
	limit    int
}

func (p *fakeLarkConversationProvider) LarkChatMetadata(_ context.Context, chatID string) (LarkChatMetadata, error) {
	p.chatID = chatID
	if p.err != nil {
		return LarkChatMetadata{}, p.err
	}
	return p.metadata, nil
}

func (p *fakeLarkConversationProvider) LarkChatMessages(_ context.Context, chatID string, limit int) ([]LarkChatMessage, error) {
	p.chatID = chatID
	p.limit = limit
	if p.err != nil {
		return nil, p.err
	}
	return append([]LarkChatMessage(nil), p.messages...), nil
}

func TestAgentLarkContextUsesBoundChatWithoutCallerChatID(t *testing.T) {
	manager := NewManager(nil, &recordingLauncher{})
	sess, err := manager.CreateSession(context.Background(), "研发讨论")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.DeleteSession(context.Background(), sess.ID)
	if _, ok, err := manager.BindLarkChat(context.Background(), sess.ID, "oc_bound_chat"); err != nil || !ok {
		t.Fatalf("BindLarkChat() ok=%v err=%v", ok, err)
	}
	provider := &fakeLarkConversationProvider{
		metadata: LarkChatMetadata{ChatID: "oc_bound_chat", ChatName: "Iris 方案讨论", ChatType: "group"},
		messages: []LarkChatMessage{{MessageID: "om_1", Text: "第一条"}, {MessageID: "om_2", Text: "第二条"}},
	}
	manager.SetLarkConversationProvider(provider)
	manager.RecordLarkAgentContext(sess.ID, LarkAgentContext{
		ChatID: "oc_bound_chat", LatestMessageID: "om_2", LatestSenderID: "ou_sender",
	})

	current, ok, err := manager.AgentLarkContext(context.Background(), sess.ID, sess.RecoveryKey)
	if err != nil || !ok {
		t.Fatalf("AgentLarkContext() ok=%v err=%v", ok, err)
	}
	if current.ChatID != "oc_bound_chat" || current.ChatName != "Iris 方案讨论" || current.LatestMessageID != "om_2" {
		t.Fatalf("unexpected current context: %#v", current)
	}
	page, ok, err := manager.AgentLarkMessages(context.Background(), sess.ID, sess.RecoveryKey, 500)
	if err != nil || !ok {
		t.Fatalf("AgentLarkMessages() ok=%v err=%v", ok, err)
	}
	if provider.chatID != "oc_bound_chat" || provider.limit != 100 {
		t.Fatalf("provider received chat=%q limit=%d", provider.chatID, provider.limit)
	}
	if page.Count != 2 || page.Messages[1].Text != "第二条" {
		t.Fatalf("unexpected page: %#v", page)
	}
}

func TestAgentLarkContextRejectsWrongTokenAndUnboundSession(t *testing.T) {
	manager := NewManager(nil, &recordingLauncher{})
	sess, err := manager.CreateSession(context.Background(), "安全测试")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.DeleteSession(context.Background(), sess.ID)
	if _, _, err := manager.AgentLarkContext(context.Background(), sess.ID, "wrong-token"); !errors.Is(err, ErrInvalidAgentContextToken) {
		t.Fatalf("wrong token error = %v", err)
	}
	if _, _, err := manager.AgentLarkContext(context.Background(), sess.ID, sess.RecoveryKey); !errors.Is(err, ErrSessionNotBoundToLark) {
		t.Fatalf("unbound error = %v", err)
	}
}

func TestNormalizeLarkHistoryMessagePreservesTextAttachmentsAndTime(t *testing.T) {
	messageID := "om_post"
	messageType := "post"
	created := "1787472000123"
	content := `{"content":[[{"tag":"text","text":"请查看"},{"tag":"img","image_key":"img_1"}]]}`
	senderID := "ou_user"
	senderType := "user"
	got := normalizeLarkHistoryMessage(&larkim.Message{
		MessageId:  &messageID,
		MsgType:    &messageType,
		CreateTime: &created,
		Body:       &larkim.MessageBody{Content: &content},
		Sender:     &larkim.Sender{Id: &senderID, SenderType: &senderType},
	})
	if got.Text != "请查看" || got.SenderID != "ou_user" || len(got.Attachments) != 1 || got.Attachments[0].Key != "img_1" {
		t.Fatalf("normalized message = %#v", got)
	}
	wantTime := time.UnixMilli(1787472000123).UTC()
	if !got.CreatedAt.Equal(wantTime) {
		t.Fatalf("created_at = %v, want %v", got.CreatedAt, wantTime)
	}
}

func TestEnsureAgentContextSkillsWritesCodexAndClaudeSkills(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := EnsureAgentContextSkills(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(home, ".agents", "skills", "iris-feishu-context", "SKILL.md"),
		filepath.Join(home, ".claude", "skills", "iris-feishu-context", "SKILL.md"),
	} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, want := range []string{"name: iris-feishu-context", "${IRIS_SESSION_ID}", "/lark/messages?limit=50"} {
			if !strings.Contains(string(content), want) {
				t.Fatalf("%s does not contain %q", path, want)
			}
		}
	}
}
