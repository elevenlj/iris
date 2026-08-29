package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkcontact "github.com/larksuite/oapi-sdk-go/v3/service/contact/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type LarkReplyBridge struct {
	appID                   string
	appSecret               string
	manager                 *Manager
	apiClient               *lark.Client
	wsClient                *larkBridgeWSClient
	uploadsDir              string
	defaultStartSessionName string
	sessionChatPrefix       string
	ignoreMessagePrefix     string
	autoSummaryPrompt       string
	startPresets            map[string]SessionStartPreset
	namePresets             map[string]SessionStartPreset
	customShortcuts         []LarkCustomShortcut
	developerOpenID         string
	workspaceOptions        []WorkspaceOption
	botIdentity             larkBotIdentity
	mu                      sync.Mutex
	groupSessionMu          sync.Mutex
	seenMessages            map[string]time.Time
	pendingFiles            map[string][]pendingLarkAttachment
	pipelines               map[string][]larkPipelineInput
	replyText               func(context.Context, string, string) error
	downloadFile            func(context.Context, string, string, larkAttachmentRef) (pendingLarkAttachment, error)
	addReaction             func(context.Context, string, string) error
	createChat              func(context.Context, string, string, string) (string, error)
	createContactChat       func(context.Context, string, string, []string) (string, error)
	fetchUserDisplayName    func(context.Context, string) (string, error)
	fetchReferencedMessages func(context.Context, string) ([]larkReferencedMessage, error)
	sendChatText            func(context.Context, string, string) error
	removeBotFromChat       func(context.Context, string) error
	fetchBotIdentity        func(context.Context) (larkBotIdentity, error)
	startMu                 sync.Mutex
	cancelStart             context.CancelFunc
	startID                 int64
}

var structuredInputEnterDelay = 200 * time.Millisecond
var structuredInputEnterSequence = "\r"
var structuredInputNumericOnlyRE = regexp.MustCompile(`^\d+$`)
var workspaceSwitchToastDelay = 2 * time.Second

const larkProcessingReactionEmoji = "THINKING"
const defaultLarkSessionChatPrefix = "Iris · "
const larkDisabledCardToastContent = "已失效，请点击最新卡片的按钮"
const larkRestartAgentContextPrompt = "请使用 iris-feishu-context 读取当前飞书群最近的消息，了解重启前的上下文，并继续处理尚未完成的任务。如果没有明确的未完成任务，请说明已完成上下文同步并等待下一步。"
const maxLarkReferencedItems = 20
const maxLarkReferencedTextRunes = 12000
const maxLarkReferencedAttachments = 10

type SessionStartPreset struct {
	Commands []string `json:"commands"`
}

type larkPipelineInput struct {
	Text          string
	MentionOpenID string
}

type larkRouteContext struct {
	MessageID    string
	ParentID     string
	RootID       string
	ChatID       string
	ChatType     string
	SenderOpenID string
	MessageTime  time.Time
	MentionedBot bool
	Mentions     []*larkim.MentionEvent
}

type larkBotIdentity struct {
	OpenID  string
	UserID  string
	UnionID string
}

type larkFreshTokenCache struct{}

func (larkFreshTokenCache) Get(context.Context, string) (string, error) { return "", nil }
func (larkFreshTokenCache) Set(context.Context, string, string, time.Duration) error {
	return nil
}

func newLarkReplyAPIClient(appID, appSecret string, options ...lark.ClientOptionFunc) *lark.Client {
	return lark.NewClient(appID, appSecret, append(options, lark.WithTokenCache(larkFreshTokenCache{}))...)
}

func NewLarkReplyBridge(appID, appSecret string, manager *Manager, uploadsDir string) *LarkReplyBridge {
	b := &LarkReplyBridge{
		appID: appID, appSecret: appSecret, manager: manager, uploadsDir: uploadsDir,
		sessionChatPrefix:   defaultLarkSessionChatPrefix,
		ignoreMessagePrefix: "/i",
		autoSummaryPrompt:   DefaultLarkAutoSummaryPrompt,
		seenMessages:        make(map[string]time.Time), pendingFiles: make(map[string][]pendingLarkAttachment), pipelines: make(map[string][]larkPipelineInput),
	}
	if manager != nil {
		manager.SetNotificationSentHook(b.OnNotificationSent)
		manager.SetLarkConversationProvider(b)
	}
	if appID != "" && appSecret != "" {
		b.apiClient = newLarkReplyAPIClient(appID, appSecret)
	}
	b.replyText = b.replyTextToMessage
	b.downloadFile = b.downloadLarkAttachment
	b.addReaction = b.addLarkMessageReaction
	b.createChat = b.createLarkChat
	b.createContactChat = b.createLarkContactChat
	b.fetchUserDisplayName = b.fetchLarkUserDisplayName
	if strings.HasPrefix(strings.TrimSpace(appID), "cli_") {
		b.fetchReferencedMessages = b.fetchLarkReferencedMessages
	}
	b.sendChatText = b.sendTextToChat
	b.removeBotFromChat = b.removeLarkBotFromChat
	b.fetchBotIdentity = b.fetchLarkBotIdentity
	return b
}

func (b *LarkReplyBridge) SetStartPresets(presets map[string]SessionStartPreset) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.startPresets = copySessionStartPresets(presets)
}

func (b *LarkReplyBridge) SetNamePresets(presets map[string]SessionStartPreset) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.namePresets = copySessionStartPresets(presets)
}

func (b *LarkReplyBridge) SetCustomShortcuts(shortcuts []LarkCustomShortcut) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.customShortcuts = normalizeLarkCustomShortcuts(shortcuts)
}

func (b *LarkReplyBridge) SetDeveloperOpenID(openID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.developerOpenID = strings.TrimSpace(openID)
}

func (b *LarkReplyBridge) SetWorkspaceOptions(options []WorkspaceOption) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.workspaceOptions = normalizeWorkspaceOptions(options)
}

func (b *LarkReplyBridge) SetDefaultStartSessionName(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.defaultStartSessionName = strings.TrimSpace(name)
}

func (b *LarkReplyBridge) SetSessionChatPrefix(prefix string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sessionChatPrefix = strings.TrimSpace(prefix)
	if b.sessionChatPrefix == "" {
		b.sessionChatPrefix = defaultLarkSessionChatPrefix
	}
}

func (b *LarkReplyBridge) SetIgnoreMessagePrefix(prefix string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ignoreMessagePrefix = strings.TrimSpace(prefix)
}

func (b *LarkReplyBridge) SetAutoSummaryPrompt(prompt string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		prompt = DefaultLarkAutoSummaryPrompt
	}
	b.autoSummaryPrompt = prompt
}

func (b *LarkReplyBridge) Available() bool {
	if b == nil || b.manager == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.apiClient != nil && b.appID != "" && b.appSecret != ""
}

func (b *LarkReplyBridge) Start(ctx context.Context) error {
	if !b.Available() {
		return nil
	}
	b.startMu.Lock()
	if b.cancelStart != nil {
		b.startMu.Unlock()
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	b.startID++
	startID := b.startID
	b.cancelStart = cancel
	b.startMu.Unlock()
	defer func() {
		b.startMu.Lock()
		if b.startID == startID {
			b.cancelStart = nil
		}
		b.startMu.Unlock()
	}()
	handler := dispatcher.NewEventDispatcher("", "").
		OnP2ChatMemberBotAddedV1(func(ctx context.Context, event *larkim.P2ChatMemberBotAddedV1) error {
			return b.HandleP2ChatMemberBotAdded(ctx, event)
		}).
		OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
			return b.HandleP2MessageReceive(ctx, event)
		}).
		OnP1MessageReceiveV1(func(ctx context.Context, event *larkim.P1MessageReceiveV1) error {
			return b.HandleP1MessageReceive(ctx, event)
		}).
		OnP2CardActionTrigger(func(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
			return b.HandleCardActionTrigger(ctx, event)
		}).
		OnP2MessageReactionCreatedV1(func(ctx context.Context, event *larkim.P2MessageReactionCreatedV1) error {
			return nil
		}).
		OnP2MessageReactionDeletedV1(func(ctx context.Context, event *larkim.P2MessageReactionDeletedV1) error {
			return nil
		}).
		OnP2MessageReadV1(func(ctx context.Context, event *larkim.P2MessageReadV1) error {
			return nil
		}).
		OnP1MessageReadV1(func(ctx context.Context, event *larkim.P1MessageReadV1) error {
			return nil
		})
	b.mu.Lock()
	appID := b.appID
	appSecret := b.appSecret
	b.mu.Unlock()
	client := newLarkBridgeWSClient(appID, appSecret, handler, b.handleCardActionPayload)
	b.startMu.Lock()
	b.wsClient = client
	b.startMu.Unlock()
	log.Printf("lark reply bridge listening for incoming messages")
	return client.Start(runCtx)
}

func (b *LarkReplyBridge) Stop() {
	if b == nil {
		return
	}
	b.startMu.Lock()
	cancel := b.cancelStart
	client := b.wsClient
	b.cancelStart = nil
	b.wsClient = nil
	b.startMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if client != nil {
		_ = client.Close()
	}
}

func (b *LarkReplyBridge) SetAppCredentials(appID, appSecret string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.appID = strings.TrimSpace(appID)
	b.appSecret = appSecret
	if b.appID != "" && b.appSecret != "" {
		b.apiClient = newLarkReplyAPIClient(b.appID, b.appSecret)
	} else {
		b.apiClient = nil
	}
	b.botIdentity = larkBotIdentity{}
}

func (b *LarkReplyBridge) handleCardActionPayload(ctx context.Context, payload []byte) (*callback.CardActionTriggerResponse, error) {
	var event callback.CardActionTriggerEvent
	if err := json.Unmarshal(payload, &event); err == nil && event.Event != nil {
		return b.HandleCardActionTrigger(ctx, &event)
	}
	var legacy struct {
		OpenMessageID string                   `json:"open_message_id"`
		OpenChatID    string                   `json:"open_chat_id"`
		Operator      *callback.Operator       `json:"operator"`
		Action        *callback.CallBackAction `json:"action"`
	}
	if err := json.Unmarshal(payload, &legacy); err != nil {
		return larkCardToast("warning", "无效操作"), nil
	}
	operatorOpenID := ""
	if legacy.Operator != nil {
		operatorOpenID = legacy.Operator.OpenID
	}
	return b.handleCardAction(ctx, legacy.Action, legacy.OpenMessageID, legacy.OpenChatID, operatorOpenID)
}

func (b *LarkReplyBridge) HandleCardActionTrigger(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	if event == nil || event.Event == nil || event.Event.Action == nil {
		return larkCardToast("warning", "无效操作"), nil
	}
	openMessageID := ""
	openChatID := ""
	if event.Event.Context != nil {
		openMessageID = event.Event.Context.OpenMessageID
		openChatID = event.Event.Context.OpenChatID
	}
	operatorOpenID := ""
	if event.Event.Operator != nil {
		operatorOpenID = event.Event.Operator.OpenID
	}
	return b.handleCardAction(ctx, event.Event.Action, openMessageID, openChatID, operatorOpenID)
}

func (b *LarkReplyBridge) handleCardAction(ctx context.Context, action *callback.CallBackAction, openMessageID string, openChatID string, operatorOpenID string) (*callback.CardActionTriggerResponse, error) {
	if action == nil {
		return larkCardToast("warning", "无效操作"), nil
	}
	value := action.Value
	if value == nil {
		value = map[string]interface{}{}
	}
	actionName := fmt.Sprint(value["iris_action"])
	if strings.TrimSpace(actionName) == "" || actionName == "<nil>" {
		actionName = fmt.Sprint(value["easy_terminal_action"])
	}
	switch actionName {
	case "shortcut":
		return b.handleCardShortcut(ctx, value, openMessageID)
	case "custom_shortcut":
		return b.handleCardCustomShortcut(ctx, value, openMessageID)
	case "refresh":
		return b.handleCardRefresh(ctx, value, openMessageID)
	case "toggle_auto_refresh":
		return b.handleCardToggleAutoRefresh(ctx, value, openMessageID)
	case "toggle_auto_summary":
		return b.handleCardToggleAutoSummary(ctx, value, openMessageID)
	case "toggle_mention_mode":
		return b.handleCardToggleMentionMode(ctx, value, openMessageID)
	case "toggle_developer_mode":
		return b.handleCardToggleDeveloperMode(ctx, value, openMessageID, operatorOpenID)
	case "restart_agent":
		return b.handleCardRestartAgent(value, openMessageID)
	case "agent_select":
		return b.handleCardAgentSelect(value, action.Option, openMessageID)
	case "workspace_select":
		return b.handleCardWorkspaceSelect(ctx, value, action.Option, openMessageID)
	case "delete_session":
		return b.handleCardDeleteSession(ctx, value, openMessageID, openChatID)
	case "terminal_select":
		return b.handleCardTerminalSelect(ctx, value, action.Option, openMessageID, openChatID, operatorOpenID)
	default:
		return larkCardToast("warning", "未知操作"), nil
	}
}

func (b *LarkReplyBridge) handleCardRestartAgent(value map[string]interface{}, openMessageID string) (*callback.CardActionTriggerResponse, error) {
	sessionID, rt, blocked := b.resolveCardActionRuntime(value, openMessageID)
	if blocked != nil {
		return blocked, nil
	}
	sess := rt.Snapshot()
	if !sess.DeveloperModeEnabled {
		return larkCardToast("warning", "请先开启开发者模式"), nil
	}
	hasLarkContext := strings.TrimSpace(sess.LarkChatID) != ""
	var err error
	if hasLarkContext {
		err = rt.RestartAgentWithFollowUp(larkRestartAgentContextPrompt, rt.NotificationMentionOpenID(), openMessageID)
	} else {
		err = rt.RestartAgent()
	}
	if err != nil {
		return larkCardToast("warning", err.Error()), nil
	}
	b.manager.EnsureBrowser(sessionID)
	if openMessageID != "" {
		defaultLarkMessageRegistry.remember(sessionID, openMessageID)
	}
	if !hasLarkContext {
		rt.NotifyInputRunningOnMessage(openMessageID)
	}
	log.Printf("lark card restarted Agent session=%s message=%s", sessionID, openMessageID)
	return larkCardToast("info", "正在重启 Agent"), nil
}

func (b *LarkReplyBridge) handleCardAgentSelect(value map[string]interface{}, option, openMessageID string) (*callback.CardActionTriggerResponse, error) {
	sessionID, rt, blocked := b.resolveCardActionRuntime(value, openMessageID)
	if blocked != nil {
		return blocked, nil
	}
	if !rt.Snapshot().DeveloperModeEnabled {
		return larkCardToast("warning", "请先开启开发者模式"), nil
	}
	selected, err := rt.SwitchAgent(strings.TrimSpace(option))
	if err != nil {
		return larkCardToast("warning", err.Error()), nil
	}
	b.manager.EnsureBrowser(sessionID)
	if openMessageID != "" {
		defaultLarkMessageRegistry.remember(sessionID, openMessageID)
	}
	rt.NotifyInputRunningOnMessage(openMessageID)
	log.Printf("lark card switched Agent session=%s message=%s agent=%s", sessionID, openMessageID, selected.Kind)
	return larkCardToast("info", "正在切换至 "+selected.Label), nil
}

func (b *LarkReplyBridge) handleCardToggleDeveloperMode(ctx context.Context, value map[string]interface{}, openMessageID, operatorOpenID string) (*callback.CardActionTriggerResponse, error) {
	b.mu.Lock()
	developerOpenID := b.developerOpenID
	b.mu.Unlock()
	if developerOpenID == "" || strings.TrimSpace(operatorOpenID) != developerOpenID {
		return larkCardToast("warning", "只有配置的开发者可以切换开发者模式"), nil
	}
	sessionID, rt, blocked := b.resolveCardActionRuntime(value, openMessageID)
	if blocked != nil {
		return blocked, nil
	}
	sess := rt.Snapshot()
	updated, ok, err := b.manager.UpdateDeveloperMode(ctx, sessionID, !sess.DeveloperModeEnabled)
	if err != nil {
		return nil, err
	}
	if !ok {
		return larkCardToast("warning", "会话不在线"), nil
	}
	updateNo, _ := strconv.Atoi(strings.TrimSpace(fmt.Sprint(value["update_no"])))
	go func() {
		if err := rt.RefreshNotificationControls(openMessageID, updateNo); err != nil {
			log.Printf("lark card developer mode patch failed session=%s: %v", sessionID, err)
		}
	}()
	if updated.DeveloperModeEnabled {
		return larkCardToast("info", "已开启开发者模式"), nil
	}
	return larkCardToast("info", "已关闭开发者模式"), nil
}

func (b *LarkReplyBridge) handleCardWorkspaceSelect(ctx context.Context, value map[string]interface{}, option, openMessageID string) (*callback.CardActionTriggerResponse, error) {
	sessionID, rt, blocked := b.resolveCardActionRuntime(value, openMessageID)
	if blocked != nil {
		return blocked, nil
	}
	if !rt.Snapshot().DeveloperModeEnabled {
		return larkCardToast("warning", "请先开启开发者模式"), nil
	}
	updated, ok, err := b.manager.SwitchWorkspace(ctx, sessionID, strings.TrimSpace(option))
	if err != nil {
		return larkCardToast("warning", err.Error()), nil
	}
	if !ok {
		return larkCardToast("warning", "会话不在线"), nil
	}
	if workspaceSwitchToastDelay > 0 {
		time.Sleep(workspaceSwitchToastDelay)
	}
	updateNo, _ := strconv.Atoi(strings.TrimSpace(fmt.Sprint(value["update_no"])))
	go func() {
		if err := rt.RefreshNotificationControlsPreservingContent(openMessageID, updateNo); err != nil {
			log.Printf("lark card workspace patch failed session=%s: %v", sessionID, err)
		}
	}()
	return larkCardToast("info", "已成功切换目录："+updated.LastCWD), nil
}

func (b *LarkReplyBridge) handleCardTerminalSelect(ctx context.Context, value map[string]interface{}, optionID string, openMessageID string, openChatID string, operatorOpenID string) (*callback.CardActionTriggerResponse, error) {
	sessionID, rt, blocked := b.resolveCardActionRuntime(value, openMessageID)
	if blocked != nil {
		return blocked, nil
	}
	sess := rt.Snapshot()
	if expectedChatID := strings.TrimSpace(sess.LarkChatID); expectedChatID != "" && strings.TrimSpace(openChatID) != "" && expectedChatID != strings.TrimSpace(openChatID) {
		return larkCardToast("warning", "该选择不属于当前群聊"), nil
	}
	interactionID := strings.TrimSpace(fmt.Sprint(value["interaction_id"]))
	selected, err := rt.consumeTerminalInteraction(interactionID, optionID, openMessageID)
	if err != nil {
		return larkCardToast("warning", err.Error()), nil
	}
	b.manager.EnsureBrowser(sessionID)
	mentionOpenID := strings.TrimSpace(operatorOpenID)
	if mentionOpenID == "" {
		mentionOpenID = rt.NotificationMentionOpenID()
	}
	if err := SubmitTerminalInteractionInputWithMention(rt, selected.Input, mentionOpenID); err != nil {
		return nil, err
	}
	if selected.SubmitWithEnter {
		if err := rt.WriteInput("\r"); err != nil {
			return nil, err
		}
	}
	if openMessageID != "" {
		defaultLarkMessageRegistry.remember(sessionID, openMessageID)
	}
	rt.NotifyInputRunningOnMessage(openMessageID)
	log.Printf("lark card terminal selection session=%s message=%s interaction=%s option=%s input_len=%d", sessionID, openMessageID, interactionID, selected.ID, len(selected.Input))
	return larkCardToast("info", "已选择 "+truncateLarkInteractionText(selected.Label, 60)), nil
}

func (b *LarkReplyBridge) handleCardDeleteSession(ctx context.Context, value map[string]interface{}, openMessageID string, openChatID string) (*callback.CardActionTriggerResponse, error) {
	sessionID, rt, blocked := b.resolveCardActionRuntime(value, openMessageID)
	if blocked != nil {
		return blocked, nil
	}
	sess := rt.Snapshot()
	chatID := strings.TrimSpace(sess.LarkChatID)
	if chatID == "" {
		chatID = strings.TrimSpace(openChatID)
	}
	if chatID != "" {
		defaultLarkMessageRegistry.forgetChat(chatID, sessionID)
	}
	if b.uploadsDir != "" {
		_ = os.RemoveAll(filepath.Join(b.uploadsDir, sessionID))
	}
	if err := b.manager.DeleteSession(ctx, sessionID); err != nil {
		return nil, err
	}
	log.Printf("lark card deleted session=%s message=%s chat=%s", sessionID, openMessageID, chatID)
	if chatID != "" && b.removeBotFromChat != nil {
		if err := b.removeBotFromChat(ctx, chatID); err != nil {
			log.Printf("lark card deleted session but failed to remove bot from chat session=%s chat=%s: %v", sessionID, chatID, err)
			return larkCardToast("warning", "会话已删除，但机器人移出群聊失败，请手动移除机器人"), nil
		}
		return larkCardToast("info", "会话已删除，机器人已退出群聊"), nil
	}
	return larkCardToast("info", "会话已删除"), nil
}

func (b *LarkReplyBridge) handleCardCustomShortcut(ctx context.Context, value map[string]interface{}, openMessageID string) (*callback.CardActionTriggerResponse, error) {
	sessionID, rt, blocked := b.resolveCardActionRuntime(value, openMessageID)
	if blocked != nil {
		return blocked, nil
	}
	command := strings.TrimSpace(fmt.Sprint(value["command"]))
	if command == "" {
		return larkCardToast("warning", "指令为空"), nil
	}
	b.manager.EnsureBrowser(sessionID)
	if err := SubmitStructuredInputWithMention(rt, command, rt.NotificationMentionOpenID()); err != nil {
		return nil, err
	}
	rt.NotifyInputRunning()
	log.Printf("lark card custom shortcut session=%s message=%s command_len=%d", sessionID, openMessageID, len(command))
	return nil, nil
}

func (b *LarkReplyBridge) handleCardShortcut(ctx context.Context, value map[string]interface{}, openMessageID string) (*callback.CardActionTriggerResponse, error) {
	sessionID, rt, blocked := b.resolveCardActionRuntime(value, openMessageID)
	if blocked != nil {
		return blocked, nil
	}
	key := strings.TrimSpace(fmt.Sprint(value["key"]))
	seq, _, ok := larkShortcutInputSequence(key)
	if !ok {
		return larkCardToast("warning", "不支持的快捷键"), nil
	}
	if err := rt.WriteInput(seq); err != nil {
		return nil, err
	}
	log.Printf("lark card shortcut action session=%s key=%s message=%s", sessionID, key, openMessageID)
	if openMessageID != "" {
		defaultLarkMessageRegistry.remember(sessionID, openMessageID)
	}
	rt.NotifyInputRunningOnMessage(openMessageID)
	return nil, nil
}

func (b *LarkReplyBridge) handleCardRefresh(ctx context.Context, value map[string]interface{}, openMessageID string) (*callback.CardActionTriggerResponse, error) {
	sessionID, rt, blocked := b.resolveCardActionRuntimeForRefresh(value, openMessageID)
	if blocked != nil {
		return blocked, nil
	}
	if openMessageID != "" {
		defaultLarkMessageRegistry.remember(sessionID, openMessageID)
	}
	updateNo, _ := strconv.Atoi(strings.TrimSpace(fmt.Sprint(value["update_no"])))
	go func() {
		if err := rt.RefreshNotificationMessage(openMessageID, updateNo); err != nil {
			log.Printf("lark card manual refresh failed session=%s message=%s: %v", sessionID, openMessageID, err)
			return
		}
		log.Printf("lark card manual refresh session=%s message=%s", sessionID, openMessageID)
	}()
	return larkCardToast("info", "刷新成功"), nil
}

func (b *LarkReplyBridge) handleCardToggleAutoRefresh(ctx context.Context, value map[string]interface{}, openMessageID string) (*callback.CardActionTriggerResponse, error) {
	sessionID, rt, blocked := b.resolveCardActionRuntime(value, openMessageID)
	if blocked != nil {
		return blocked, nil
	}
	if openMessageID != "" {
		defaultLarkMessageRegistry.remember(sessionID, openMessageID)
	}
	updateNo, _ := strconv.Atoi(strings.TrimSpace(fmt.Sprint(value["update_no"])))
	enabled, err := rt.ToggleAutoRefresh(openMessageID)
	if err != nil {
		return nil, err
	}
	if enabled {
		log.Printf("lark card auto refresh enabled session=%s message=%s", sessionID, openMessageID)
	} else {
		go func() {
			if err := rt.RefreshNotificationControls(openMessageID, updateNo); err != nil {
				log.Printf("lark card auto refresh toggle patch failed session=%s message=%s enabled=%v: %v", sessionID, openMessageID, enabled, err)
				return
			}
			log.Printf("lark card auto refresh toggled session=%s message=%s enabled=%v", sessionID, openMessageID, enabled)
		}()
	}
	if enabled {
		return larkCardToast("info", "已开启自动刷新"), nil
	}
	return larkCardToast("info", "已关闭自动刷新"), nil
}

func (b *LarkReplyBridge) handleCardToggleAutoSummary(ctx context.Context, value map[string]interface{}, openMessageID string) (*callback.CardActionTriggerResponse, error) {
	sessionID, rt, blocked := b.resolveCardActionRuntime(value, openMessageID)
	if blocked != nil {
		return blocked, nil
	}
	if openMessageID != "" {
		defaultLarkMessageRegistry.remember(sessionID, openMessageID)
	}
	updateNo, _ := strconv.Atoi(strings.TrimSpace(fmt.Sprint(value["update_no"])))
	enabled, err := rt.ToggleAutoSummary()
	if err != nil {
		return nil, err
	}
	go func() {
		if err := rt.RefreshNotificationControls(openMessageID, updateNo); err != nil {
			log.Printf("lark card auto summary toggle patch failed session=%s message=%s enabled=%v: %v", sessionID, openMessageID, enabled, err)
			return
		}
		log.Printf("lark card auto summary toggled session=%s message=%s enabled=%v", sessionID, openMessageID, enabled)
	}()
	if enabled {
		return larkCardToast("info", "已开启自动总结"), nil
	}
	return larkCardToast("info", "已关闭自动总结"), nil
}

func (b *LarkReplyBridge) handleCardToggleMentionMode(ctx context.Context, value map[string]interface{}, openMessageID string) (*callback.CardActionTriggerResponse, error) {
	sessionID, rt, blocked := b.resolveCardActionRuntime(value, openMessageID)
	if blocked != nil {
		return blocked, nil
	}
	updated, ok, err := b.manager.ToggleLarkMentionMode(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return larkCardToast("warning", "会话不在线"), nil
	}
	if openMessageID != "" {
		defaultLarkMessageRegistry.remember(sessionID, openMessageID)
	}
	updateNo, _ := strconv.Atoi(strings.TrimSpace(fmt.Sprint(value["update_no"])))
	go func() {
		if err := rt.RefreshNotificationControls(openMessageID, updateNo); err != nil {
			log.Printf("lark card mention mode toggle patch failed session=%s message=%s enabled=%v: %v", sessionID, openMessageID, updated.LarkMentionModeEnabled, err)
			return
		}
		log.Printf("lark card mention mode toggled session=%s message=%s enabled=%v", sessionID, openMessageID, updated.LarkMentionModeEnabled)
	}()
	if updated.LarkMentionModeEnabled {
		return larkCardToast("info", "已开启艾特模式"), nil
	}
	return larkCardToast("info", "已关闭艾特模式"), nil
}

func larkCardToast(kind, content string) *callback.CardActionTriggerResponse {
	return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: kind, Content: content}}
}

func (b *LarkReplyBridge) resolveCardActionRuntime(value map[string]interface{}, openMessageID string) (string, *RuntimeSession, *callback.CardActionTriggerResponse) {
	return b.resolveCardActionRuntimeWithMode(value, openMessageID, false)
}

func (b *LarkReplyBridge) resolveCardActionRuntimeForRefresh(value map[string]interface{}, openMessageID string) (string, *RuntimeSession, *callback.CardActionTriggerResponse) {
	return b.resolveCardActionRuntimeWithMode(value, openMessageID, true)
}

func (b *LarkReplyBridge) resolveCardActionRuntimeWithMode(value map[string]interface{}, openMessageID string, refresh bool) (string, *RuntimeSession, *callback.CardActionTriggerResponse) {
	sessionID := strings.TrimSpace(fmt.Sprint(value["session_id"]))
	if sessionID == "" && openMessageID != "" {
		if id, ok := defaultLarkMessageRegistry.lookup(openMessageID); ok {
			sessionID = id
		}
	}
	if sessionID == "" {
		return "", nil, larkCardToast("warning", "未找到会话")
	}
	if b.manager == nil {
		return sessionID, nil, larkCardToast("warning", "会话不在线")
	}
	rt, exists := b.manager.GetRuntime(sessionID)
	if !exists {
		return sessionID, nil, larkCardToast("warning", "会话不在线")
	}
	var err error
	if refresh {
		err = rt.ValidateNotificationRefresh(openMessageID)
	} else {
		err = rt.ValidateNotificationAction(openMessageID)
	}
	if err != nil {
		if errors.Is(err, errNotificationMessageDisabled) {
			return sessionID, rt, larkCardToast("warning", larkDisabledCardToastContent)
		}
		return sessionID, rt, larkCardToast("warning", err.Error())
	}
	return sessionID, rt, nil
}

func larkShortcutInputSequence(key string) (string, string, bool) {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "ctrl_c", "ctrl-c", "control_c", "control-c":
		return "\x03", "Ctrl-C", true
	case "exit_agent", "exit-agent", "quit_agent", "quit-agent":
		return "\x03\x03", "退出 Agent", true
	case "esc", "escape":
		return "\x1b", "Esc", true
	case "enter", "return":
		return "\r", "Enter", true
	default:
		return "", "", false
	}
}

func (b *LarkReplyBridge) HandleP2MessageReceive(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return nil
	}
	msg := event.Event.Message
	if shouldIgnoreLarkP2Message(event.Event.Sender) {
		return nil
	}
	messageType := valueOf(msg.MessageType)
	incoming := extractLarkIncomingMessage(valueOf(msg.Content), messageType)
	if b.shouldIgnoreIncomingText(incoming.Text) {
		return nil
	}
	routeCtx := larkRouteContext{
		MessageID:    valueOf(msg.MessageId),
		ParentID:     valueOf(msg.ParentId),
		RootID:       valueOf(msg.RootId),
		ChatID:       valueOf(msg.ChatId),
		ChatType:     valueOf(msg.ChatType),
		SenderOpenID: larkSenderOpenID(event.Event.Sender),
		MessageTime:  parseLarkMillisecondTime(valueOf(msg.CreateTime)),
		Mentions:     msg.Mentions,
	}
	if _, ignored := b.shouldIgnoreForMentionMode(ctx, routeCtx, incoming); ignored {
		return nil
	}
	log.Printf("lark reply bridge received P2 message=%s chat=%s chat_type=%s msg_type=%s text_len=%d attachments=%d",
		valueOf(msg.MessageId), valueOf(msg.ChatId), valueOf(msg.ChatType), messageType, len(incoming.Text), len(incoming.Attachments))
	if isUnsupportedLarkForwardedCard(messageType) {
		if err := b.replyLarkText(ctx, valueOf(msg.MessageId), "收到转发卡片，但飞书没有把卡片内容作为普通文本开放给机器人。请直接在原卡片所在会话操作，或把需要处理的内容复制成文本/截图后发送。"); err != nil {
			return err
		}
		return nil
	}
	if incoming.Text == "" && len(incoming.Attachments) == 0 && strings.TrimSpace(valueOf(msg.ParentId)) == "" {
		if mayContainUnsupportedLarkContent(messageType) {
			if err := b.replyLarkText(ctx, valueOf(msg.MessageId), "收到消息，但当前无法读取其中的卡片或富媒体内容。请改为发送文本、图片或文件。"); err != nil {
				return err
			}
		}
		return nil
	}
	b.markLarkMessageProcessing(ctx, valueOf(msg.MessageId))
	sessionID, err := b.RouteIncomingWithContext(ctx, routeCtx, incoming)
	if err != nil {
		log.Printf("lark reply bridge failed to route message %s: %v", valueOf(msg.MessageId), err)
		return err
	}
	log.Printf("lark reply bridge routed message %s to %s", valueOf(msg.MessageId), sessionID)
	return nil
}

func (b *LarkReplyBridge) HandleP2ChatMemberBotAdded(ctx context.Context, event *larkim.P2ChatMemberBotAddedV1) error {
	if b == nil || b.manager == nil || event == nil || event.Event == nil {
		return nil
	}
	if event.Event.External != nil && *event.Event.External {
		log.Printf("lark reply bridge ignored bot-added event chat=%s reason=external_group", valueOf(event.Event.ChatId))
		return nil
	}
	chatID := strings.TrimSpace(valueOf(event.Event.ChatId))
	name := strings.TrimSpace(valueOf(event.Event.Name))
	if chatID == "" || name == "" {
		return fmt.Errorf("lark bot-added event missing chat id or name")
	}

	b.groupSessionMu.Lock()
	defer b.groupSessionMu.Unlock()
	if sess, ok, err := b.manager.FindSessionByLarkChatID(ctx, chatID); err != nil {
		return err
	} else if ok {
		if !sess.LarkMentionModeEnabled {
			_, _, err = b.manager.UpdateLarkMentionMode(ctx, sess.ID, true)
		}
		return err
	}

	sess, err := b.createLarkSessionForMessage(ctx, name, larkRouteContext{
		ChatID:       chatID,
		ChatType:     "group",
		SenderOpenID: larkUserOpenID(event.Event.OperatorId),
	})
	if err != nil {
		return err
	}
	if updated, ok, updateErr := b.manager.UpdateLarkMentionMode(ctx, sess.ID, true); updateErr != nil {
		log.Printf("lark reply bridge failed to enable mention mode session=%s chat=%s: %v", sess.ID, chatID, updateErr)
	} else if ok {
		sess = updated
	}
	if agent, _ := b.manager.AgentConfig(); agent.Command == "" {
		if err := b.runDefaultWorkspacePreset(sess); err != nil {
			log.Printf("lark bot-added workspace preset failed session=%s name=%q: %v", sess.ID, sess.Name, err)
		} else if err := b.runSessionStartPresets(sess, "999999"); err != nil {
			log.Printf("lark bot-added agent preset failed session=%s name=%q: %v", sess.ID, sess.Name, err)
		}
	}
	if b.sendChatText != nil {
		if err := b.sendChatText(ctx, chatID, fmt.Sprintf("已创建并绑定会话「%s」，请 @机器人发送任务。", sess.Name)); err != nil {
			log.Printf("lark bot-added intro failed session=%s chat=%s: %v", sess.ID, chatID, err)
		}
	}
	log.Printf("lark reply bridge created group session=%s chat=%s name=%q", sess.ID, chatID, sess.Name)
	return nil
}

func (b *LarkReplyBridge) markLarkMessageProcessing(ctx context.Context, messageID string) {
	if b.addReaction == nil || messageID == "" {
		return
	}
	if err := b.addReaction(ctx, messageID, larkProcessingReactionEmoji); err != nil {
		log.Printf("lark reply bridge failed to add processing reaction message=%s emoji=%s: %v", messageID, larkProcessingReactionEmoji, err)
	}
}

func shouldIgnoreLarkP2Message(sender *larkim.EventSender) bool {
	if sender == nil || sender.SenderType == nil {
		return false
	}
	return *sender.SenderType != "" && *sender.SenderType != "user"
}

func (b *LarkReplyBridge) shouldIgnoreIncomingText(text string) bool {
	text = cleanLarkText(text)
	if text == "" {
		return false
	}
	b.mu.Lock()
	prefix := b.ignoreMessagePrefix
	b.mu.Unlock()
	prefix = strings.TrimSpace(prefix)
	if prefix == "" || !strings.HasPrefix(text, prefix) {
		return false
	}
	rest := strings.TrimPrefix(text, prefix)
	for _, r := range rest {
		return unicode.IsSpace(r)
	}
	return false
}

func (b *LarkReplyBridge) shouldIgnoreForMentionMode(ctx context.Context, routeCtx larkRouteContext, incoming larkIncomingMessage) (string, bool) {
	if b == nil || b.manager == nil || !isLarkGroupChatType(routeCtx.ChatType) {
		return "", false
	}
	sessionID := b.mentionModeSessionID(ctx, routeCtx, incoming)
	if sessionID == "" {
		return "", false
	}
	sess, ok, err := b.manager.GetSession(ctx, sessionID)
	if err != nil || !ok || !sess.LarkMentionModeEnabled {
		return sessionID, false
	}
	if b.routeContextMentionsBot(ctx, routeCtx) {
		return sessionID, false
	}
	log.Printf("lark reply bridge ignored message=%s session=%s reason=mention_mode_requires_bot_mention", routeCtx.MessageID, sessionID)
	return sessionID, true
}

func (b *LarkReplyBridge) mentionModeSessionID(ctx context.Context, routeCtx larkRouteContext, incoming larkIncomingMessage) string {
	text := cleanLarkText(incoming.Text)
	parts := splitLarkPipeline(text)
	if len(parts) > 0 {
		text = parts[0]
	}
	return b.resolveSessionID(ctx, text, routeCtx.ParentID, routeCtx.RootID, routeCtx.ChatID, routeCtx.ChatType)
}

func (b *LarkReplyBridge) routeContextMentionsBot(ctx context.Context, routeCtx larkRouteContext) bool {
	if routeCtx.MentionedBot {
		return true
	}
	if len(routeCtx.Mentions) == 0 {
		return false
	}
	identity, ok := b.currentBotIdentity(ctx)
	if !ok {
		return false
	}
	for _, mention := range routeCtx.Mentions {
		if larkMentionMatchesBot(mention, identity) {
			return true
		}
	}
	return false
}

func larkMentionMatchesBot(mention *larkim.MentionEvent, identity larkBotIdentity) bool {
	if mention == nil || mention.Id == nil {
		return false
	}
	if identity.OpenID != "" && strings.TrimSpace(valueOf(mention.Id.OpenId)) == identity.OpenID {
		return true
	}
	if identity.UserID != "" && strings.TrimSpace(valueOf(mention.Id.UserId)) == identity.UserID {
		return true
	}
	return identity.UnionID != "" && strings.TrimSpace(valueOf(mention.Id.UnionId)) == identity.UnionID
}

func isLarkGroupChatType(chatType string) bool {
	switch strings.ToLower(strings.TrimSpace(chatType)) {
	case "group", "topic_group":
		return true
	default:
		return false
	}
}

func isLarkDirectChatType(chatType string) bool {
	switch strings.ToLower(strings.TrimSpace(chatType)) {
	case "p2p", "private":
		return true
	default:
		return false
	}
}

func isUnsupportedLarkForwardedCard(messageType string) bool {
	return messageType == "interactive"
}

func mayContainUnsupportedLarkContent(messageType string) bool {
	switch messageType {
	case "interactive", "post", "share_chat", "share_user":
		return true
	default:
		return false
	}
}

func (b *LarkReplyBridge) HandleP1MessageReceive(ctx context.Context, event *larkim.P1MessageReceiveV1) error {
	if event == nil || event.Event == nil {
		return nil
	}
	e := event.Event
	text := strings.TrimSpace(e.TextWithoutAtBot)
	if text == "" {
		text = strings.TrimSpace(e.Text)
	}
	if text == "" {
		return nil
	}
	if b.shouldIgnoreIncomingText(text) {
		return nil
	}
	routeCtx := larkRouteContext{
		MessageID:    e.OpenMessageID,
		ParentID:     e.ParentID,
		RootID:       e.RootID,
		ChatID:       e.OpenChatID,
		ChatType:     e.ChatType,
		SenderOpenID: e.OpenID,
		MentionedBot: e.IsMention,
	}
	if _, ignored := b.shouldIgnoreForMentionMode(ctx, routeCtx, larkIncomingMessage{Text: text}); ignored {
		return nil
	}
	log.Printf("lark reply bridge received P1 message=%s chat=%s chat_type=%s msg_type=%s mention=%v text_len=%d",
		e.OpenMessageID, e.OpenChatID, e.ChatType, e.MsgType, e.IsMention, len(text))
	b.markLarkMessageProcessing(ctx, e.OpenMessageID)
	sessionID, err := b.RouteIncomingWithContext(ctx, routeCtx, larkIncomingMessage{Text: text})
	if err != nil {
		log.Printf("lark reply bridge failed to route P1 message %s: %v", e.OpenMessageID, err)
		return err
	}
	log.Printf("lark reply bridge routed P1 message %s to %s", e.OpenMessageID, sessionID)
	return nil
}

func (b *LarkReplyBridge) RouteText(ctx context.Context, messageID, parentID, rootID, text string) (string, error) {
	return b.RouteIncoming(ctx, messageID, parentID, rootID, larkIncomingMessage{Text: text})
}

func (b *LarkReplyBridge) RouteIncoming(ctx context.Context, messageID, parentID, rootID string, incoming larkIncomingMessage) (string, error) {
	return b.RouteIncomingWithContext(ctx, larkRouteContext{MessageID: messageID, ParentID: parentID, RootID: rootID}, incoming)
}

func (b *LarkReplyBridge) RouteIncomingWithContext(ctx context.Context, routeCtx larkRouteContext, incoming larkIncomingMessage) (string, error) {
	messageID, parentID, rootID := routeCtx.MessageID, routeCtx.ParentID, routeCtx.RootID
	if sessionID, ignored := b.shouldIgnoreForMentionMode(ctx, routeCtx, incoming); ignored {
		return sessionID, nil
	}
	if b.duplicate(messageID) {
		return "", nil
	}
	incoming = b.resolveReferencedIncoming(ctx, routeCtx, incoming)
	text := cleanLarkText(incoming.Text)
	parts := splitLarkPipeline(text)
	inputParts := append([]string(nil), parts...)
	if incoming.Referenced != nil {
		if len(parts) == 0 {
			parts = []string{""}
			inputParts = []string{formatLarkReferencedInput(incoming.Referenced, "")}
		} else {
			inputParts[0] = formatLarkReferencedInput(incoming.Referenced, parts[0])
		}
	}
	if b.contactAssistantEnabled(routeCtx) {
		if _, _, isStart := b.parseLarkStartCommand(text); !isStart {
			return b.routeDirectContactMessage(ctx, routeCtx, incoming, inputParts)
		}
	}
	if len(incoming.Attachments) > 0 {
		return b.routeAttachments(ctx, routeCtx, text, inputParts, incoming.Attachments)
	}
	if len(parts) == 0 {
		return "", nil
	}
	text = parts[0]
	if sessionID := b.resolveSessionID(ctx, text, parentID, rootID, routeCtx.ChatID, routeCtx.ChatType); sessionID != "" && b.hasPendingFiles(sessionID) {
		rt, _, ok, err := b.ensureRouteRuntime(ctx, sessionID, routeCtx)
		if err != nil {
			return sessionID, err
		}
		if !ok {
			if err := b.replyLarkText(ctx, messageID, "会话不在线"); err != nil {
				return sessionID, err
			}
			return sessionID, nil
		}
		b.manager.EnsureBrowser(sessionID)
		b.enqueuePipeline(sessionID, inputParts[1:], routeCtx.SenderOpenID)
		if err := SubmitStructuredInputWithMention(rt, inputParts[0], routeCtx.SenderOpenID); err != nil {
			return sessionID, err
		}
		b.scheduleAutoSummary(rt, text)
		b.clearPendingFiles(sessionID)
		defaultLarkMessageRegistry.remember(sessionID, messageID, parentID, rootID)
		b.notifyInputRunning(sessionID)
		return sessionID, nil
	}
	if name, presetCodes, ok := b.parseLarkStartCommand(text); ok {
		s, err := b.createLarkSessionForMessage(ctx, name, routeCtx)
		if err == nil {
			if updated, found, updateErr := b.manager.UpdateDeveloperMode(ctx, s.ID, true); updateErr == nil && found {
				s = updated
			}
			if updated, found, updateErr := b.manager.UpdateLarkMentionMode(ctx, s.ID, false); updateErr == nil && found {
				s = updated
			}
			defaultLarkMessageRegistry.remember(s.ID, messageID)
			namePresetMatched, presetErr := false, error(nil)
			if agent, _ := b.manager.AgentConfig(); agent.Command == "" {
				namePresetMatched, presetErr = b.runSessionNamePreset(s, presetCodes)
			}
			if presetErr != nil {
				log.Printf("lark name preset failed session=%s name=%q: %v", s.ID, s.Name, presetErr)
			}
			if agent, _ := b.manager.AgentConfig(); agent.Command == "" && !namePresetMatched {
				if workspaceErr := b.runDefaultWorkspacePreset(s); workspaceErr != nil {
					log.Printf("lark default workspace preset failed session=%s name=%q: %v", s.ID, s.Name, workspaceErr)
				}
				if strings.TrimSpace(presetCodes) == "" {
					presetCodes = "999999"
				}
				if presetErr := b.runSessionStartPresets(s, presetCodes); presetErr != nil {
					log.Printf("lark start presets failed session=%s codes=%q: %v", s.ID, presetCodes, presetErr)
				}
			}
			b.enqueuePipeline(s.ID, parts[1:], routeCtx.SenderOpenID)
			if rt, found := b.manager.GetRuntime(s.ID); found && !rt.discardingStartupNotifications() {
				rt.NotifyInputRunning()
			}
		}
		return s.ID, err
	}
	sessionID := b.resolveSessionID(ctx, text, parentID, rootID, routeCtx.ChatID, routeCtx.ChatType)
	if isStopCommand(text) {
		if sessionID == "" {
			if err := b.replyLarkText(ctx, messageID, "未找到会话"); err != nil {
				return "", err
			}
			return "", nil
		}
		rt, ok := b.manager.GetRuntime(sessionID)
		if !ok {
			if err := b.replyLarkText(ctx, messageID, "会话不在线"); err != nil {
				return sessionID, err
			}
			return sessionID, nil
		}
		rt.SetNotificationMentionOpenID(routeCtx.SenderOpenID)
		if err := rt.WriteInput("\x03"); err != nil {
			return sessionID, err
		}
		defaultLarkMessageRegistry.remember(sessionID, messageID, parentID, rootID)
		b.notifyInputRunning(sessionID)
		return sessionID, nil
	}
	if isCurrentRoundCommand(text) {
		if sessionID == "" {
			if err := b.replyLarkText(ctx, messageID, "未找到会话"); err != nil {
				return "", err
			}
			return "", nil
		}
		rt, ok := b.manager.GetRuntime(sessionID)
		if !ok {
			if err := b.replyLarkText(ctx, messageID, "会话不在线"); err != nil {
				return sessionID, err
			}
			return sessionID, nil
		}
		content := rt.CurrentRoundRawContent()
		if strings.TrimSpace(content) == "" {
			content = "当前轮暂无内容"
		}
		if err := b.replyLarkRawText(ctx, messageID, content); err != nil {
			return sessionID, err
		}
		defaultLarkMessageRegistry.remember(sessionID, messageID, parentID, rootID)
		return sessionID, nil
	}
	if sessionID == "" {
		s, err := b.createImplicitLarkSessionForMessage(ctx, routeCtx)
		if err != nil {
			return "", err
		}
		sessionID = s.ID
	}
	rt, _, ok, err := b.ensureRouteRuntime(ctx, sessionID, routeCtx)
	if err != nil {
		return sessionID, err
	}
	if !ok {
		s, err := b.createLarkSessionForMessage(ctx, sessionID, routeCtx)
		if err != nil {
			return "", err
		}
		sessionID = s.ID
		rt, _ = b.manager.GetRuntime(sessionID)
	}
	b.manager.EnsureBrowser(sessionID)
	if b.enqueueInputIfRuntimeBusy(rt, sessionID, inputParts, routeCtx.SenderOpenID) {
		defaultLarkMessageRegistry.remember(sessionID, messageID, parentID, rootID)
		return sessionID, nil
	}
	b.enqueuePipeline(sessionID, inputParts[1:], routeCtx.SenderOpenID)
	if err := SubmitStructuredInputWithMention(rt, inputParts[0], routeCtx.SenderOpenID); err != nil {
		return sessionID, err
	}
	b.scheduleAutoSummary(rt, text)
	defaultLarkMessageRegistry.remember(sessionID, messageID, parentID, rootID)
	b.notifyInputRunning(sessionID)
	return sessionID, nil
}

func (b *LarkReplyBridge) contactAssistantEnabled(routeCtx larkRouteContext) bool {
	if !isLarkDirectChatType(routeCtx.ChatType) || strings.TrimSpace(routeCtx.SenderOpenID) == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.developerOpenID != "" && b.createContactChat != nil
}

func (b *LarkReplyBridge) routeDirectContactMessage(ctx context.Context, routeCtx larkRouteContext, incoming larkIncomingMessage, parts []string) (string, error) {
	b.groupSessionMu.Lock()
	defer b.groupSessionMu.Unlock()
	forwardText := strings.TrimSpace(incoming.Text)
	if forwardText == "" && len(incoming.Attachments) > 0 {
		forwardText = "发送了 " + larkAttachmentKindLabel(incoming.Attachments[0].Kind)
	}

	binding, ok, err := b.manager.GetLarkContactBinding(ctx, routeCtx.SenderOpenID)
	if err != nil {
		return "", err
	}
	var rt *RuntimeSession
	if ok && binding.Active {
		rt, _, ok, err = b.ensureRouteRuntime(ctx, binding.SessionID, larkRouteContext{
			ChatID: binding.ChatID, ChatType: "group", SenderOpenID: routeCtx.SenderOpenID,
		})
		if err != nil {
			return binding.SessionID, err
		}
		if !ok || rt == nil {
			binding, rt, err = b.rebindContactConversation(ctx, binding)
			if err != nil {
				return binding.SessionID, err
			}
			ok = true
		}
	}
	if !ok || !binding.Active || rt == nil {
		binding, rt, err = b.createContactConversation(ctx, routeCtx.SenderOpenID, forwardText)
		if err != nil {
			return "", err
		}
	}

	if b.sendChatText != nil && forwardText != "" {
		if err := b.sendChatText(ctx, binding.ChatID, binding.DisplayName+"："+forwardText); err != nil {
			_ = b.manager.DeactivateLarkContactBinding(ctx, routeCtx.SenderOpenID)
			binding, rt, err = b.createContactConversation(ctx, routeCtx.SenderOpenID, forwardText)
			if err != nil {
				return "", err
			}
			if err := b.sendChatText(ctx, binding.ChatID, binding.DisplayName+"："+forwardText); err != nil {
				return binding.SessionID, err
			}
		}
	}

	groupRoute := routeCtx
	groupRoute.ChatID = binding.ChatID
	groupRoute.ChatType = "group"
	b.recordAgentLarkContext(rt.Snapshot(), groupRoute)
	if len(incoming.Attachments) > 0 {
		return b.routeAttachments(ctx, groupRoute, incoming.Text, parts, incoming.Attachments)
	}
	if len(parts) == 0 {
		return binding.SessionID, nil
	}
	b.manager.EnsureBrowser(binding.SessionID)
	if b.enqueueInputIfRuntimeBusy(rt, binding.SessionID, parts, routeCtx.SenderOpenID) {
		defaultLarkMessageRegistry.remember(binding.SessionID, routeCtx.MessageID)
		return binding.SessionID, nil
	}
	b.enqueuePipeline(binding.SessionID, parts[1:], routeCtx.SenderOpenID)
	if err := SubmitStructuredInputWithMention(rt, parts[0], routeCtx.SenderOpenID); err != nil {
		return binding.SessionID, err
	}
	b.scheduleAutoSummary(rt, parts[0])
	defaultLarkMessageRegistry.remember(binding.SessionID, routeCtx.MessageID)
	b.notifyInputRunning(binding.SessionID)
	return binding.SessionID, nil
}

func (b *LarkReplyBridge) rebindContactConversation(ctx context.Context, binding LarkContactBinding) (LarkContactBinding, *RuntimeSession, error) {
	b.mu.Lock()
	developerOpenID := b.developerOpenID
	b.mu.Unlock()
	contactName := b.larkUserDisplayNameOrOpenID(ctx, binding.SenderOpenID)
	developerName := b.larkUserDisplayNameOrOpenID(ctx, developerOpenID)
	name := larkContactConversationName(contactName, developerName)
	sess, err := b.createLarkSession(ctx, name)
	if err != nil {
		return binding, nil, err
	}
	rt, _ := b.manager.GetRuntime(sess.ID)
	if rt == nil {
		return binding, nil, errors.New("联系人会话未启动")
	}
	rt.RequireLarkChatForNotifications()
	rt.SetNotificationMentionOpenID(binding.SenderOpenID)
	sess, err = b.bindSessionToLarkChat(ctx, sess, binding.ChatID)
	if err != nil {
		return binding, nil, err
	}
	_, _, _ = b.manager.UpdateLarkMentionMode(ctx, sess.ID, true)
	_, _, _ = b.manager.UpdateDeveloperMode(ctx, sess.ID, false)
	if _, err := b.enableLarkSessionNotifications(ctx, sess); err != nil {
		return binding, nil, err
	}
	binding.SessionID = sess.ID
	binding.DisplayName = contactName
	binding.Active = true
	binding.UpdatedAt = time.Now().UTC()
	if err := b.manager.UpsertLarkContactBinding(ctx, binding); err != nil {
		return binding, nil, err
	}
	return binding, rt, nil
}

func (b *LarkReplyBridge) createContactConversation(ctx context.Context, senderOpenID, _ string) (LarkContactBinding, *RuntimeSession, error) {
	b.mu.Lock()
	developerOpenID := b.developerOpenID
	b.mu.Unlock()
	displayName := b.larkUserDisplayNameOrOpenID(ctx, senderOpenID)
	developerName := b.larkUserDisplayNameOrOpenID(ctx, developerOpenID)
	conversationName := larkContactConversationName(displayName, developerName)
	sess, err := b.createLarkSession(ctx, conversationName)
	if err != nil {
		return LarkContactBinding{}, nil, err
	}
	rt, _ := b.manager.GetRuntime(sess.ID)
	if rt == nil {
		return LarkContactBinding{}, nil, errors.New("联系人会话未启动")
	}
	rt.RequireLarkChatForNotifications()
	rt.SetNotificationMentionOpenID(senderOpenID)
	members := uniqueNonEmptyStrings([]string{senderOpenID, developerOpenID})
	chatID, err := b.createContactChat(ctx, sess.ID, conversationName, members)
	if err != nil {
		_ = b.manager.DeleteSession(context.Background(), sess.ID)
		return LarkContactBinding{}, nil, fmt.Errorf("创建联系人群聊失败: %w", err)
	}
	if strings.TrimSpace(chatID) == "" {
		_ = b.manager.DeleteSession(context.Background(), sess.ID)
		return LarkContactBinding{}, nil, errors.New("创建联系人群聊失败：飞书未返回群聊 ID")
	}
	sess, err = b.bindSessionToLarkChat(ctx, sess, chatID)
	if err != nil {
		_ = b.manager.DeleteSession(context.Background(), sess.ID)
		return LarkContactBinding{}, nil, err
	}
	if updated, found, updateErr := b.manager.UpdateLarkMentionMode(ctx, sess.ID, true); updateErr == nil && found {
		sess = updated
	}
	if updated, found, updateErr := b.manager.UpdateDeveloperMode(ctx, sess.ID, false); updateErr == nil && found {
		sess = updated
	}
	if _, err := b.enableLarkSessionNotifications(ctx, sess); err != nil {
		return LarkContactBinding{}, nil, err
	}
	now := time.Now().UTC()
	binding := LarkContactBinding{
		SenderOpenID: senderOpenID, DisplayName: displayName, ChatID: chatID, SessionID: sess.ID,
		Active: true, CreatedAt: now, UpdatedAt: now,
	}
	if previous, found, _ := b.manager.GetLarkContactBinding(ctx, senderOpenID); found && !previous.CreatedAt.IsZero() {
		binding.CreatedAt = previous.CreatedAt
	}
	if err := b.manager.UpsertLarkContactBinding(ctx, binding); err != nil {
		return LarkContactBinding{}, nil, err
	}
	if b.sendChatText != nil {
		_ = b.sendChatText(ctx, chatID, "Iris 已建立与 "+displayName+" 的对话。群内默认需要 @Iris 才会触发回复。")
	}
	return binding, rt, nil
}

func fallbackLarkContactName(openID string) string {
	openID = strings.TrimSpace(openID)
	if openID == "" {
		return "未知用户"
	}
	return openID
}

func (b *LarkReplyBridge) larkUserDisplayNameOrOpenID(ctx context.Context, openID string) string {
	openID = strings.TrimSpace(openID)
	if b.fetchUserDisplayName != nil && openID != "" {
		name, err := b.fetchUserDisplayName(ctx, openID)
		if err == nil && strings.TrimSpace(name) != "" {
			return strings.TrimSpace(name)
		}
		if err != nil {
			log.Printf("lark contact display name lookup failed open_id=%s: %v", openID, err)
		} else {
			log.Printf("lark contact display name lookup returned empty name open_id=%s", openID)
		}
	}
	return fallbackLarkContactName(openID)
}

func larkContactConversationName(contactName, developerName string) string {
	return fallbackLarkContactName(contactName) + "和" + fallbackLarkContactName(developerName) + "的会话"
}

func uniqueNonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func (b *LarkReplyBridge) enqueueInputIfRuntimeBusy(rt *RuntimeSession, sessionID string, parts []string, mentionOpenID string) bool {
	if rt == nil || sessionID == "" || len(parts) == 0 {
		return false
	}
	if !rt.ShouldQueueInputWhileRunning() {
		return false
	}
	if structuredInputNumericOnlyRE.MatchString(strings.TrimSpace(parts[0])) {
		return false
	}
	starting := rt.discardingStartupNotifications()
	rt.SetNotificationMentionOpenID(mentionOpenID)
	if !starting {
		rt.MarkStructuredInputActivity(parts[0])
	}
	b.enqueuePipeline(sessionID, parts, mentionOpenID)
	if !starting {
		rt.NotifyInputRunning()
	}
	log.Printf("lark reply bridge queued input session=%s parts=%d reason=runtime_running", sessionID, len(parts))
	return true
}

func (b *LarkReplyBridge) routeAttachments(ctx context.Context, routeCtx larkRouteContext, text string, parts []string, refs []larkAttachmentRef) (string, error) {
	messageID, parentID, rootID := routeCtx.MessageID, routeCtx.ParentID, routeCtx.RootID
	if len(parts) > 0 {
		text = parts[0]
	}
	sessionID := b.resolveSessionID(ctx, text, parentID, rootID, routeCtx.ChatID, routeCtx.ChatType)
	if sessionID == "" {
		s, err := b.createImplicitLarkSessionForMessage(ctx, routeCtx)
		if err != nil {
			return "", err
		}
		sessionID = s.ID
	}
	rt, _, ok, err := b.ensureRouteRuntime(ctx, sessionID, routeCtx)
	if err != nil {
		return sessionID, err
	}
	if !ok {
		s, err := b.createLarkSessionForMessage(ctx, sessionID, routeCtx)
		if err != nil {
			return "", err
		}
		sessionID = s.ID
		rt, _ = b.manager.GetRuntime(sessionID)
	}

	files := make([]pendingLarkAttachment, 0, len(refs))
	optionalFailures := 0
	for _, ref := range refs {
		sourceMessageID := strings.TrimSpace(ref.SourceMessageID)
		if sourceMessageID == "" {
			sourceMessageID = messageID
		}
		file, err := b.downloadFile(ctx, sourceMessageID, sessionID, ref)
		if err != nil {
			if ref.Optional {
				optionalFailures++
				log.Printf("lark reply bridge skipped unavailable referenced attachment message=%s kind=%s: %v", sourceMessageID, ref.Kind, err)
				continue
			}
			kind := larkAttachmentKindLabel(ref.Kind)
			if replyErr := b.replyLarkText(ctx, messageID, kind+"上传失败："+err.Error()); replyErr != nil {
				return sessionID, replyErr
			}
			return sessionID, err
		}
		files = append(files, file)
	}
	if optionalFailures > 0 {
		text = strings.TrimSpace(text) + fmt.Sprintf("\n\n[提示] %d 个引用附件无法下载，已继续处理可读取内容。", optionalFailures)
		text = strings.TrimSpace(text)
	}
	if len(files) == 0 && strings.TrimSpace(text) == "" {
		return sessionID, nil
	}

	b.manager.EnsureBrowser(sessionID)
	input := formatLarkAttachmentInput(files)
	if strings.TrimSpace(text) == "" {
		rt.SetNotificationMentionOpenID(routeCtx.SenderOpenID)
		if err := rt.WriteInput(input + " "); err != nil {
			return sessionID, err
		}
		b.appendPendingFiles(sessionID, files)
		defaultLarkMessageRegistry.remember(sessionID, messageID, parentID, rootID)
		defaultLarkMessageRegistry.rememberLatest(sessionID)
		if err := b.replyLarkText(ctx, messageID, larkAttachmentUploadSuccessMessage(files)); err != nil {
			return sessionID, err
		}
		return sessionID, nil
	}

	if b.hasPendingFiles(sessionID) {
		if err := rt.WriteInput("\x15"); err != nil {
			return sessionID, err
		}
		b.clearPendingFiles(sessionID)
	}
	b.enqueuePipeline(sessionID, parts[1:], routeCtx.SenderOpenID)
	if err := SubmitStructuredInputWithMention(rt, input+" "+text, routeCtx.SenderOpenID); err != nil {
		return sessionID, err
	}
	b.scheduleAutoSummary(rt, text)
	b.clearPendingFiles(sessionID)
	defaultLarkMessageRegistry.remember(sessionID, messageID, parentID, rootID)
	b.notifyInputRunning(sessionID)
	return sessionID, nil
}

func (b *LarkReplyBridge) createLarkSession(ctx context.Context, name string) (Session, error) {
	s, err := b.manager.CreateSession(ctx, name)
	if err != nil {
		return s, err
	}
	return s, nil
}

func (b *LarkReplyBridge) createImplicitLarkSessionForMessage(ctx context.Context, routeCtx larkRouteContext) (Session, error) {
	if routeCtx.ChatID != "" && isLarkDirectChatType(routeCtx.ChatType) {
		return b.createDirectBotSessionForMessage(ctx, routeCtx)
	}
	return b.createLarkSessionForMessage(ctx, "lark-session", routeCtx)
}

func (b *LarkReplyBridge) createDirectBotSessionForMessage(ctx context.Context, routeCtx larkRouteContext) (Session, error) {
	name := "机器人会话"
	b.mu.Lock()
	if strings.TrimSpace(b.defaultStartSessionName) != "" {
		name = b.defaultStartSessionName
	}
	b.mu.Unlock()
	s, err := b.createLarkSession(ctx, name)
	if err != nil {
		return s, err
	}
	if rt, ok := b.manager.GetRuntime(s.ID); ok {
		rt.SetNotificationMentionOpenID(routeCtx.SenderOpenID)
	}
	updated, err := b.bindSessionToLarkChat(ctx, s, routeCtx.ChatID)
	if err != nil {
		return updated, err
	}
	b.recordAgentLarkContext(updated, routeCtx)
	log.Printf("lark reply bridge bound direct bot chat session=%s chat=%s", updated.ID, routeCtx.ChatID)
	return b.enableLarkSessionNotifications(ctx, updated)
}

func (b *LarkReplyBridge) createLarkSessionForMessage(ctx context.Context, name string, routeCtx larkRouteContext) (Session, error) {
	s, err := b.createLarkSession(ctx, name)
	if err != nil {
		return s, err
	}
	if routeCtx.ChatID != "" && routeCtx.ChatType == "group" {
		if rt, ok := b.manager.GetRuntime(s.ID); ok {
			rt.RequireLarkChatForNotifications()
			rt.SetNotificationMentionOpenID(routeCtx.SenderOpenID)
		}
		updated, err := b.bindSessionToLarkChat(ctx, s, routeCtx.ChatID)
		if err != nil {
			return updated, err
		}
		b.recordAgentLarkContext(updated, routeCtx)
		return b.enableLarkSessionNotifications(ctx, updated)
	}
	if routeCtx.SenderOpenID == "" || b.createChat == nil {
		log.Printf("lark reply bridge skipped dedicated chat creation session=%s name=%q reason=missing_sender_or_creator sender=%q",
			s.ID, s.Name, routeCtx.SenderOpenID)
		b.recordAgentLarkContext(s, routeCtx)
		return b.enableLarkSessionNotifications(ctx, s)
	}
	if rt, ok := b.manager.GetRuntime(s.ID); ok {
		rt.RequireLarkChatForNotifications()
		rt.SetNotificationMentionOpenID(routeCtx.SenderOpenID)
	}
	log.Printf("lark reply bridge creating dedicated chat session=%s name=%q owner=%s", s.ID, s.Name, routeCtx.SenderOpenID)
	chatID, err := b.createChat(ctx, s.ID, s.Name, routeCtx.SenderOpenID)
	if err != nil {
		log.Printf("lark reply bridge failed to create session chat session=%s name=%q: %v", s.ID, s.Name, err)
		return s, nil
	}
	if strings.TrimSpace(chatID) == "" {
		log.Printf("lark reply bridge failed to create session chat session=%s name=%q: empty chat id", s.ID, s.Name)
		return s, nil
	}
	updated, err := b.bindSessionToLarkChat(ctx, s, chatID)
	if err != nil {
		return updated, err
	}
	log.Printf("lark reply bridge bound session=%s to lark chat=%s", updated.ID, chatID)
	b.recordAgentLarkContext(updated, routeCtx)
	if b.sendChatText != nil {
		if err := b.sendChatText(ctx, chatID, fmt.Sprintf("已创建会话 %s（%s）。之后直接在这个对话里发消息。", updated.Name, updated.ID)); err != nil {
			log.Printf("lark reply bridge failed to send session chat intro session=%s chat=%s: %v", updated.ID, chatID, err)
		}
	}
	return b.enableLarkSessionNotifications(ctx, updated)
}

func (b *LarkReplyBridge) enableLarkSessionNotifications(ctx context.Context, sess Session) (Session, error) {
	updated, ok, err := b.manager.UpdateNotifyOnWaiting(ctx, sess.ID, true)
	if err != nil || !ok {
		return sess, err
	}
	b.manager.EnsureBrowser(updated.ID)
	return updated, nil
}

func (b *LarkReplyBridge) bindSessionToLarkChat(ctx context.Context, sess Session, chatID string) (Session, error) {
	updated, ok, err := b.manager.BindLarkChat(ctx, sess.ID, chatID)
	if err != nil || !ok {
		return sess, err
	}
	defaultLarkMessageRegistry.rememberChat(chatID, updated.ID)
	return updated, nil
}

func (b *LarkReplyBridge) ensureRouteRuntime(ctx context.Context, sessionID string, routeCtx larkRouteContext) (*RuntimeSession, Session, bool, error) {
	if b == nil || b.manager == nil || strings.TrimSpace(sessionID) == "" {
		return nil, Session{}, false, nil
	}
	if rt, ok := b.manager.GetRuntime(sessionID); ok {
		s := rt.Snapshot()
		b.recordAgentLarkContext(s, routeCtx)
		if routeCtx.SenderOpenID != "" {
			rt.SetNotificationMentionOpenID(routeCtx.SenderOpenID)
		}
		return rt, s, true, nil
	}
	rt, sess, ok, err := b.manager.RecoverRuntime(ctx, sessionID)
	if err != nil || !ok || rt == nil {
		return rt, sess, ok, err
	}
	if routeCtx.ChatID != "" {
		defaultLarkMessageRegistry.rememberChat(routeCtx.ChatID, sess.ID)
	}
	b.recordAgentLarkContext(sess, routeCtx)
	if routeCtx.SenderOpenID != "" {
		rt.SetNotificationMentionOpenID(routeCtx.SenderOpenID)
	}
	if strings.TrimSpace(sess.LastMode) == SessionModeAgent && strings.TrimSpace(sess.LastAgentResumeCommand) != "" {
		time.Sleep(1200 * time.Millisecond)
	}
	return rt, sess, true, nil
}

func (b *LarkReplyBridge) recordAgentLarkContext(sess Session, routeCtx larkRouteContext) {
	if b == nil || b.manager == nil || strings.TrimSpace(sess.ID) == "" {
		return
	}
	chatID := strings.TrimSpace(sess.LarkChatID)
	if chatID == "" {
		chatID = strings.TrimSpace(routeCtx.ChatID)
	}
	chatType := strings.TrimSpace(routeCtx.ChatType)
	if strings.TrimSpace(sess.LarkChatID) != "" && strings.TrimSpace(routeCtx.ChatID) != "" && strings.TrimSpace(sess.LarkChatID) != strings.TrimSpace(routeCtx.ChatID) {
		chatType = "group"
	}
	messageTime := routeCtx.MessageTime
	if messageTime.IsZero() && strings.TrimSpace(routeCtx.MessageID) != "" {
		messageTime = time.Now().UTC()
	}
	b.manager.RecordLarkAgentContext(sess.ID, LarkAgentContext{
		SessionID:         sess.ID,
		SessionName:       sess.Name,
		ChatID:            chatID,
		ChatType:          chatType,
		LatestMessageID:   strings.TrimSpace(routeCtx.MessageID),
		LatestParentID:    strings.TrimSpace(routeCtx.ParentID),
		LatestRootID:      strings.TrimSpace(routeCtx.RootID),
		LatestSenderID:    strings.TrimSpace(routeCtx.SenderOpenID),
		LatestMessageTime: messageTime,
		UpdatedAt:         time.Now().UTC(),
	})
}

func (b *LarkReplyBridge) parseLarkStartCommand(text string) (string, string, bool) {
	text = strings.TrimSpace(text)
	prefixes := []string{"/start", "/new", "新会话", "开始"}
	for _, prefix := range prefixes {
		if text != prefix && !strings.HasPrefix(text, prefix+" ") {
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(text, prefix))
		name, codes := b.splitSessionNameAndPresetCodes(body)
		if name == "" {
			b.mu.Lock()
			name = b.defaultStartSessionName
			b.mu.Unlock()
			if name == "" {
				return "", "", false
			}
		}
		return name, codes, true
	}
	return "", "", false
}

func (b *LarkReplyBridge) splitSessionNameAndPresetCodes(body string) (string, string) {
	body = strings.TrimSpace(body)
	if b.hasNamePreset(body) {
		return body, ""
	}
	fields := strings.Fields(strings.TrimSpace(body))
	if len(fields) <= 1 {
		return strings.TrimSpace(body), ""
	}
	last := fields[len(fields)-1]
	if !b.isStartPresetCodeExpression(last) {
		return strings.TrimSpace(body), ""
	}
	name := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(body), last))
	return name, last
}

func (b *LarkReplyBridge) hasNamePreset(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.namePresets[name]
	return ok
}

func (b *LarkReplyBridge) isStartPresetCodeExpression(text string) bool {
	codes := splitPresetCodes(text)
	if len(codes) == 0 {
		return false
	}
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, code := range codes {
		if code == "0" {
			continue
		}
		if _, ok := b.startPresets[code]; !ok {
			return false
		}
	}
	return true
}

func isLegacyNumericPresetCodeList(text string) bool {
	if text == "" {
		return false
	}
	hasDigit := false
	prevDash := false
	for i, r := range text {
		if r >= '0' && r <= '9' {
			hasDigit = true
			prevDash = false
			continue
		}
		if r == '-' {
			if i == 0 || prevDash {
				return false
			}
			prevDash = true
			continue
		}
		return false
	}
	return hasDigit && !prevDash
}

func splitPresetCodes(codes string) []string {
	codes = strings.TrimSpace(codes)
	if codes == "" {
		return nil
	}
	var parts []string
	if isLegacyNumericPresetCodeList(codes) && strings.Contains(codes, "-") {
		parts = strings.Split(codes, "-")
	} else {
		parts = strings.FieldsFunc(codes, func(r rune) bool {
			return r == ',' || r == '，' || r == '+' || r == '＋'
		})
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil
		}
		if strings.ContainsAny(part, " \t\r\n") {
			return nil
		}
		out = append(out, part)
	}
	return out
}

func (b *LarkReplyBridge) runSessionStartPresets(sess Session, codes string) error {
	if strings.TrimSpace(codes) == "" {
		return nil
	}
	rt, ok := b.manager.GetRuntime(sess.ID)
	if !ok {
		return fmt.Errorf("runtime not found")
	}
	b.mu.Lock()
	presets := copySessionStartPresets(b.startPresets)
	b.mu.Unlock()
	vars := sessionStartPresetVars(sess, codes)
	presetCodes := splitPresetCodes(codes)
	if len(presetCodes) > 0 {
		rt.SuppressStartupNotifications()
	}
	for _, code := range presetCodes {
		if code == "0" {
			log.Printf("lark start preset skipped workspace-only code session=%s code=%q", sess.ID, code)
			continue
		}
		preset, ok := presets[code]
		if !ok {
			log.Printf("lark start preset not configured session=%s code=%q", sess.ID, code)
			continue
		}
		if err := runSessionPresetCommands(rt, preset, vars); err != nil {
			return err
		}
	}
	if len(presetCodes) > 0 {
		rt.FinishStartupNotifications()
	}
	return nil
}

func (b *LarkReplyBridge) runDefaultWorkspacePreset(sess Session) error {
	rt, ok := b.manager.GetRuntime(sess.ID)
	if !ok {
		return fmt.Errorf("runtime not found")
	}
	workspaceDir := b.manager.defaultSessionWorkspaceShellPath()
	preset := SessionStartPreset{Commands: []string{
		"mkdir -p " + workspaceDir,
		"cd " + workspaceDir,
	}}
	rt.SuppressStartupNotifications()
	if err := runSessionPresetCommands(rt, preset, sessionStartPresetVars(sess, "")); err != nil {
		return err
	}
	rt.FinishStartupNotifications()
	return nil
}

func (b *LarkReplyBridge) runSessionNamePreset(sess Session, codes string) (bool, error) {
	rt, ok := b.manager.GetRuntime(sess.ID)
	if !ok {
		return false, fmt.Errorf("runtime not found")
	}
	b.mu.Lock()
	presets := copySessionStartPresets(b.namePresets)
	b.mu.Unlock()
	preset, ok := presets[sess.Name]
	if !ok {
		return false, nil
	}
	rt.SuppressStartupNotifications()
	vars := sessionStartPresetVars(sess, codes)
	if err := runSessionPresetCommands(rt, preset, vars); err != nil {
		return true, err
	}
	rt.FinishStartupNotifications()
	return true, nil
}

func runSessionPresetCommands(rt *RuntimeSession, preset SessionStartPreset, vars map[string]string) error {
	for _, template := range preset.Commands {
		command := renderSessionStartPresetCommand(template, vars)
		if strings.TrimSpace(command) == "" {
			continue
		}
		rt.RecordShellCommandForRecovery(command)
		if !strings.HasSuffix(command, "\r") && !strings.HasSuffix(command, "\n") {
			command += "\r"
		}
		if _, err := rt.terminal.Write([]byte(command)); err != nil {
			return err
		}
	}
	return nil
}

func copySessionStartPresets(in map[string]SessionStartPreset) map[string]SessionStartPreset {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]SessionStartPreset, len(in))
	for key, preset := range in {
		cp := make([]string, len(preset.Commands))
		copy(cp, preset.Commands)
		out[key] = SessionStartPreset{Commands: cp}
	}
	return out
}

func sessionStartPresetVars(sess Session, codes string) map[string]string {
	timestamp := time.Now().Format(time.RFC3339)
	return map[string]string{
		"session_name":          shellQuote(sess.Name),
		"session_name_raw":      sess.Name,
		"session_id":            shellQuote(sess.ID),
		"session_id_raw":        sess.ID,
		"preset_codes":          shellQuote(codes),
		"preset_codes_raw":      codes,
		"timestamp":             shellQuote(timestamp),
		"timestamp_raw":         timestamp,
		"session_name_slug":     shellQuote(slugForShellPath(sess.Name)),
		"session_name_slug_raw": slugForShellPath(sess.Name),
	}
}

func renderSessionStartPresetCommand(template string, vars map[string]string) string {
	out := template
	for key, value := range vars {
		out = strings.ReplaceAll(out, "{{"+key+"}}", value)
	}
	return out
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func safeWorkspaceSessionDir(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "session"
	}
	var b strings.Builder
	lastSeparator := false
	for _, r := range value {
		if r == '/' || r == '\\' || r == ':' || r < 32 || r == 127 {
			if !lastSeparator {
				b.WriteByte('-')
				lastSeparator = true
			}
			continue
		}
		if r == ' ' || r == '\t' {
			if !lastSeparator {
				b.WriteByte(' ')
				lastSeparator = true
			}
			continue
		}
		b.WriteRune(r)
		lastSeparator = false
	}
	out := strings.Trim(b.String(), " -")
	if out == "" || out == "." || out == ".." {
		return "session"
	}
	return out
}

func slugForShellPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "session"
	}
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r > 127 {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func (b *LarkReplyBridge) OnNotificationSent(sessionID string) {
	next := b.popPipeline(sessionID)
	if next.Text == "" {
		return
	}
	rt, ok := b.manager.GetRuntime(sessionID)
	if !ok {
		return
	}
	b.manager.EnsureBrowser(sessionID)
	if err := SubmitStructuredInputWithMention(rt, next.Text, next.MentionOpenID); err != nil {
		log.Printf("lark reply bridge failed to continue pipeline for %s: %v", sessionID, err)
		return
	}
	b.scheduleAutoSummary(rt, next.Text)
	rt.NotifyInputRunning()
}

func (b *LarkReplyBridge) notifyInputRunning(sessionID string) {
	if b == nil || b.manager == nil || sessionID == "" {
		return
	}
	rt, ok := b.manager.GetRuntime(sessionID)
	if !ok {
		return
	}
	rt.NotifyInputRunning()
}

func (b *LarkReplyBridge) scheduleAutoSummary(rt *RuntimeSession, originalInput string) {
	if b == nil || rt == nil || !rt.AutoSummaryEnabled() || !shouldScheduleAutoSummary(originalInput) {
		return
	}
	b.mu.Lock()
	prompt := b.autoSummaryPrompt
	b.mu.Unlock()
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		prompt = DefaultLarkAutoSummaryPrompt
	}
	sessionID := rt.Snapshot().ID
	time.AfterFunc(time.Second, func() {
		if !rt.AutoSummaryEnabled() {
			return
		}
		if err := SubmitSilentStructuredInput(rt, prompt); err != nil {
			log.Printf("lark auto summary prompt failed session=%s: %v", sessionID, err)
			return
		}
		log.Printf("lark auto summary prompt submitted session=%s prompt_len=%d", sessionID, len(prompt))
	})
}

func shouldScheduleAutoSummary(input string) bool {
	input = strings.TrimSpace(input)
	if input == "" {
		return false
	}
	if structuredInputNumericOnlyRE.MatchString(input) {
		return false
	}
	return !strings.HasPrefix(input, "/") && !strings.HasPrefix(input, "／")
}

func (b *LarkReplyBridge) enqueuePipeline(sessionID string, parts []string, mentionOpenID string) {
	if sessionID == "" || len(parts) == 0 {
		return
	}
	cleaned := make([]larkPipelineInput, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			cleaned = append(cleaned, larkPipelineInput{Text: part, MentionOpenID: strings.TrimSpace(mentionOpenID)})
		}
	}
	if len(cleaned) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pipelines[sessionID] = append(b.pipelines[sessionID], cleaned...)
}

func (b *LarkReplyBridge) popPipeline(sessionID string) larkPipelineInput {
	b.mu.Lock()
	defer b.mu.Unlock()
	queue := b.pipelines[sessionID]
	if len(queue) == 0 {
		return larkPipelineInput{}
	}
	next := queue[0]
	if len(queue) == 1 {
		delete(b.pipelines, sessionID)
	} else {
		b.pipelines[sessionID] = queue[1:]
	}
	return next
}

func (b *LarkReplyBridge) appendPendingFiles(sessionID string, files []pendingLarkAttachment) {
	if sessionID == "" || len(files) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pendingFiles[sessionID] = append(b.pendingFiles[sessionID], files...)
}

func (b *LarkReplyBridge) hasPendingFiles(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pendingFiles[sessionID]) > 0
}

func (b *LarkReplyBridge) clearPendingFiles(sessionID string) {
	if sessionID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pendingFiles, sessionID)
}

func isCurrentRoundCommand(text string) bool {
	text = strings.TrimSpace(text)
	return text == "/c" || text == "／c"
}

func isStopCommand(text string) bool {
	text = strings.TrimSpace(text)
	return text == "/stop" || text == "／stop"
}

func (b *LarkReplyBridge) replyLarkText(ctx context.Context, messageID string, text string) error {
	if b.replyText == nil || messageID == "" {
		return nil
	}
	return b.replyText(ctx, messageID, truncateForLark(sanitizeForLarkAudit(text)))
}

func (b *LarkReplyBridge) replyLarkRawText(ctx context.Context, messageID string, text string) error {
	if b.replyText == nil || messageID == "" {
		return nil
	}
	return b.replyText(ctx, messageID, text)
}

func (b *LarkReplyBridge) replyTextToMessage(ctx context.Context, messageID string, text string) error {
	if b.apiClient == nil {
		return nil
	}
	content, _ := json.Marshal(map[string]string{"text": text})
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType("text").
			Content(string(content)).
			ReplyInThread(false).
			Build()).
		Build()
	resp, err := b.apiClient.Im.V1.Message.Reply(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("lark reply API returned code %d: %s", resp.Code, resp.Msg)
	}
	return nil
}

func (b *LarkReplyBridge) createLarkChat(ctx context.Context, sessionID, sessionName, ownerOpenID string) (string, error) {
	if b.apiClient == nil {
		return "", nil
	}
	name := b.larkSessionChatName(sessionName)
	b.mu.Lock()
	developerOpenID := b.developerOpenID
	b.mu.Unlock()
	body := larkim.NewCreateChatReqBodyBuilder().
		Name(name).
		UserIdList(uniqueNonEmptyStrings([]string{ownerOpenID, developerOpenID})).
		ChatMode("group").
		ChatType("private").
		JoinMessageVisibility("not_anyone").
		LeaveMessageVisibility("not_anyone").
		MembershipApproval("no_approval_required").
		Build()
	req := larkim.NewCreateChatReqBuilder().
		UserIdType("open_id").
		Uuid(larkCreateChatUUID(sessionID)).
		Body(body).
		Build()
	resp, err := b.apiClient.Im.V1.Chat.Create(ctx, req)
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", fmt.Errorf("lark create chat API returned code %d: %s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.ChatId == nil || *resp.Data.ChatId == "" {
		return "", fmt.Errorf("lark create chat API returned empty chat_id")
	}
	return *resp.Data.ChatId, nil
}

func (b *LarkReplyBridge) createLarkContactChat(ctx context.Context, sessionID, chatTitle string, members []string) (string, error) {
	if b.apiClient == nil {
		return "", errors.New("飞书客户端未配置")
	}
	chatTitle = strings.TrimSpace(chatTitle)
	if chatTitle == "" {
		chatTitle = "未知用户和未知用户的会话"
	}
	body := larkim.NewCreateChatReqBodyBuilder().
		Name(chatTitle).
		UserIdList(uniqueNonEmptyStrings(members)).
		ChatMode("group").
		ChatType("private").
		JoinMessageVisibility("not_anyone").
		LeaveMessageVisibility("not_anyone").
		MembershipApproval("no_approval_required").
		Build()
	req := larkim.NewCreateChatReqBuilder().UserIdType("open_id").Uuid(larkCreateChatUUID(sessionID)).Body(body).Build()
	resp, err := b.apiClient.Im.V1.Chat.Create(ctx, req)
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", fmt.Errorf("lark create contact chat API returned code %d: %s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.ChatId == nil || strings.TrimSpace(*resp.Data.ChatId) == "" {
		return "", errors.New("lark create contact chat API returned empty chat_id")
	}
	return strings.TrimSpace(*resp.Data.ChatId), nil
}

func (b *LarkReplyBridge) fetchLarkUserDisplayName(ctx context.Context, openID string) (string, error) {
	if b.apiClient == nil {
		return "", errors.New("飞书客户端未配置")
	}
	req := larkcontact.NewGetUserReqBuilder().UserId(strings.TrimSpace(openID)).UserIdType("open_id").Build()
	resp, err := b.apiClient.Contact.V3.User.Get(ctx, req)
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", fmt.Errorf("lark get user API returned code %d: %s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.User == nil {
		return "", errors.New("lark get user API returned empty user")
	}
	name := strings.TrimSpace(valueOf(resp.Data.User.Name))
	if name == "" {
		name = strings.TrimSpace(valueOf(resp.Data.User.Nickname))
	}
	return name, nil
}

func (b *LarkReplyBridge) fetchLarkReferencedMessages(ctx context.Context, messageID string) ([]larkReferencedMessage, error) {
	if b.apiClient == nil {
		return nil, errors.New("飞书客户端未配置")
	}
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return nil, errors.New("引用消息 ID 为空")
	}
	req := larkim.NewGetMessageReqBuilder().MessageId(messageID).UserIdType("open_id").Build()
	resp, err := b.apiClient.Im.V1.Message.Get(ctx, req)
	if err != nil {
		return nil, err
	}
	if !resp.Success() {
		return nil, fmt.Errorf("飞书消息接口返回 code %d: %s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || len(resp.Data.Items) == 0 {
		return nil, errors.New("飞书未返回引用消息内容")
	}
	items := make([]larkReferencedMessage, 0, len(resp.Data.Items))
	for _, item := range resp.Data.Items {
		if item == nil {
			continue
		}
		content := ""
		if item.Body != nil {
			content = valueOf(item.Body.Content)
		}
		items = append(items, larkReferencedMessage{
			MessageID:      valueOf(item.MessageId),
			UpperMessageID: valueOf(item.UpperMessageId),
			MessageType:    valueOf(item.MsgType),
			Content:        content,
			Deleted:        item.Deleted != nil && *item.Deleted,
		})
	}
	if len(items) == 0 {
		return nil, errors.New("飞书未返回可用的引用消息")
	}
	return items, nil
}

func larkCreateChatUUID(sessionID string) string {
	return fmt.Sprintf("iris-%s-%d", strings.TrimSpace(sessionID), time.Now().UnixNano())
}

func (b *LarkReplyBridge) larkSessionChatName(sessionName string) string {
	sessionName = strings.TrimSpace(sessionName)
	if sessionName == "" {
		sessionName = "session"
	}
	b.mu.Lock()
	prefix := b.sessionChatPrefix
	b.mu.Unlock()
	if strings.TrimSpace(prefix) == "" {
		prefix = defaultLarkSessionChatPrefix
	}
	rs := []rune(sessionName)
	if len(rs) > 40 {
		sessionName = string(rs[:40])
	}
	return prefix + sessionName
}

func (b *LarkReplyBridge) sendTextToChat(ctx context.Context, chatID string, text string) error {
	if b.apiClient == nil || chatID == "" {
		return nil
	}
	content, _ := json.Marshal(map[string]string{"text": text})
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType("text").
			Content(string(content)).
			Build()).
		Build()
	resp, err := b.apiClient.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("lark chat text API returned code %d: %s", resp.Code, resp.Msg)
	}
	return nil
}

func (b *LarkReplyBridge) fetchLarkBotIdentity(ctx context.Context) (larkBotIdentity, error) {
	if b == nil {
		return larkBotIdentity{}, nil
	}
	b.mu.Lock()
	cached := b.botIdentity
	appID := strings.TrimSpace(b.appID)
	appSecret := strings.TrimSpace(b.appSecret)
	b.mu.Unlock()
	if cached.OpenID != "" || cached.UserID != "" || cached.UnionID != "" {
		return cached, nil
	}
	if appID == "" || appSecret == "" {
		return larkBotIdentity{}, nil
	}
	token, err := fetchLarkTenantAccessToken(ctx, appID, appSecret)
	if err != nil {
		return larkBotIdentity{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://open.feishu.cn/open-apis/bot/v3/info", nil)
	if err != nil {
		return larkBotIdentity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := doHTTPRequestWithRetry(req)
	if err != nil {
		return larkBotIdentity{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return larkBotIdentity{}, err
	}
	var data struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Bot  struct {
			OpenID  string `json:"open_id"`
			UserID  string `json:"user_id"`
			UnionID string `json:"union_id"`
		} `json:"bot"`
		Data struct {
			OpenID  string `json:"open_id"`
			UserID  string `json:"user_id"`
			UnionID string `json:"union_id"`
			Bot     struct {
				OpenID  string `json:"open_id"`
				UserID  string `json:"user_id"`
				UnionID string `json:"union_id"`
			} `json:"bot"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return larkBotIdentity{}, err
	}
	if resp.StatusCode >= 300 || data.Code != 0 {
		msg := strings.TrimSpace(data.Msg)
		if msg == "" {
			msg = strings.TrimSpace(string(body))
		}
		return larkBotIdentity{}, fmt.Errorf("lark bot info API returned %s: %s", resp.Status, msg)
	}
	identity := larkBotIdentity{
		OpenID:  strings.TrimSpace(data.Bot.OpenID),
		UserID:  strings.TrimSpace(data.Bot.UserID),
		UnionID: strings.TrimSpace(data.Bot.UnionID),
	}
	if identity.OpenID == "" && identity.UserID == "" && identity.UnionID == "" {
		identity = larkBotIdentity{
			OpenID:  strings.TrimSpace(data.Data.OpenID),
			UserID:  strings.TrimSpace(data.Data.UserID),
			UnionID: strings.TrimSpace(data.Data.UnionID),
		}
	}
	if identity.OpenID == "" && identity.UserID == "" && identity.UnionID == "" {
		identity = larkBotIdentity{
			OpenID:  strings.TrimSpace(data.Data.Bot.OpenID),
			UserID:  strings.TrimSpace(data.Data.Bot.UserID),
			UnionID: strings.TrimSpace(data.Data.Bot.UnionID),
		}
	}
	b.mu.Lock()
	b.botIdentity = identity
	b.mu.Unlock()
	return identity, nil
}

func fetchLarkTenantAccessToken(ctx context.Context, appID, appSecret string) (string, error) {
	payload, _ := json.Marshal(map[string]string{"app_id": appID, "app_secret": appSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := doHTTPRequestWithRetry(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var data struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 || data.Code != 0 || data.TenantAccessToken == "" {
		if data.Msg == "" {
			data.Msg = resp.Status
		}
		return "", errors.New(data.Msg)
	}
	return data.TenantAccessToken, nil
}

func (b *LarkReplyBridge) removeLarkBotFromChat(ctx context.Context, chatID string) error {
	appID := strings.TrimSpace(b.appID)
	chatID = strings.TrimSpace(chatID)
	if b.apiClient == nil || chatID == "" || appID == "" {
		return nil
	}
	req := larkim.NewDeleteChatMembersReqBuilder().
		ChatId(chatID).
		MemberIdType(larkim.MemberIdTypeDeleteChatMembersAppId).
		Body(larkim.NewDeleteChatMembersReqBodyBuilder().
			IdList([]string{appID}).
			Build()).
		Build()
	resp, err := b.apiClient.Im.V1.ChatMembers.Delete(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("lark remove bot from chat API returned code %d: %s", resp.Code, resp.Msg)
	}
	return nil
}

func (b *LarkReplyBridge) currentBotIdentity(ctx context.Context) (larkBotIdentity, bool) {
	if b == nil {
		return larkBotIdentity{}, false
	}
	b.mu.Lock()
	identity := normalizeLarkBotIdentity(b.botIdentity)
	fetch := b.fetchBotIdentity
	b.mu.Unlock()
	if identity.hasAny() {
		return identity, true
	}
	if fetch == nil {
		return larkBotIdentity{}, false
	}
	fetched, err := fetch(ctx)
	if err != nil {
		log.Printf("lark reply bridge failed to fetch bot identity: %v", err)
		return larkBotIdentity{}, false
	}
	fetched = normalizeLarkBotIdentity(fetched)
	if !fetched.hasAny() {
		log.Printf("lark reply bridge fetched empty bot identity")
		return larkBotIdentity{}, false
	}
	b.mu.Lock()
	b.botIdentity = fetched
	b.mu.Unlock()
	return fetched, true
}

func normalizeLarkBotIdentity(identity larkBotIdentity) larkBotIdentity {
	return larkBotIdentity{
		OpenID:  strings.TrimSpace(identity.OpenID),
		UserID:  strings.TrimSpace(identity.UserID),
		UnionID: strings.TrimSpace(identity.UnionID),
	}
}

func (i larkBotIdentity) hasAny() bool {
	return strings.TrimSpace(i.OpenID) != "" || strings.TrimSpace(i.UserID) != "" || strings.TrimSpace(i.UnionID) != ""
}

func (b *LarkReplyBridge) addLarkMessageReaction(ctx context.Context, messageID string, emoji string) error {
	if b.apiClient == nil || messageID == "" || emoji == "" {
		return nil
	}
	req := larkim.NewCreateMessageReactionReqBuilder().
		MessageId(messageID).
		Body(larkim.NewCreateMessageReactionReqBodyBuilder().
			ReactionType(larkim.NewEmojiBuilder().EmojiType(emoji).Build()).
			Build()).
		Build()
	resp, err := b.apiClient.Im.V1.MessageReaction.Create(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("lark reaction API returned code %d: %s", resp.Code, resp.Msg)
	}
	return nil
}

func (b *LarkReplyBridge) duplicate(messageID string) bool {
	if messageID == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	for id, t := range b.seenMessages {
		if now.Sub(t) > 10*time.Minute {
			delete(b.seenMessages, id)
		}
	}
	if _, ok := b.seenMessages[messageID]; ok {
		return true
	}
	b.seenMessages[messageID] = now
	return false
}

func (b *LarkReplyBridge) resolveSessionID(ctx context.Context, text, parentID, rootID, chatID, chatType string) string {
	if id, ok := defaultLarkMessageRegistry.lookupChat(chatID); ok {
		if b.sessionIsActive(ctx, id) {
			return id
		}
		defaultLarkMessageRegistry.forgetChat(chatID, id)
		log.Printf("lark reply bridge ignored stale chat route chat=%s session=%s", chatID, id)
	}
	if b.manager != nil && chatID != "" {
		if s, ok, err := b.manager.FindSessionByLarkChatID(ctx, chatID); err == nil && ok && s.Live && s.Status != StatusExited && s.Status != StatusFailed {
			defaultLarkMessageRegistry.rememberChat(chatID, s.ID)
			return s.ID
		}
	}
	if id, ok := defaultLarkMessageRegistry.lookup(parentID, rootID); ok {
		return id
	}
	if m := regexp.MustCompile(`sess-\d+`).FindString(text); m != "" {
		return m
	}
	if chatID != "" && isLarkGroupChatType(chatType) {
		return ""
	}
	if chatID != "" && isLarkDirectChatType(chatType) {
		return ""
	}
	return defaultLarkMessageRegistry.latestNotifiedSessionID()
}

func (b *LarkReplyBridge) sessionIsActive(ctx context.Context, sessionID string) bool {
	if b == nil || b.manager == nil || sessionID == "" {
		return false
	}
	s, ok, err := b.manager.GetSession(ctx, sessionID)
	return err == nil && ok && s.Live && s.Status != StatusExited && s.Status != StatusFailed
}

func larkSenderOpenID(sender *larkim.EventSender) string {
	if sender == nil || sender.SenderId == nil || sender.SenderId.OpenId == nil {
		return ""
	}
	return *sender.SenderId.OpenId
}

func larkUserOpenID(user *larkim.UserId) string {
	if user == nil {
		return ""
	}
	return valueOf(user.OpenId)
}

func cleanLarkText(text string) string {
	text = regexp.MustCompile(`<at[^>]*>.*?</at>`).ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}

func splitLarkPipeline(text string) []string {
	var parts []string
	var b strings.Builder
	escaped := false
	for _, r := range text {
		switch {
		case escaped:
			if r != '|' {
				b.WriteRune('\\')
			}
			b.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case isLarkPipelineSeparator(r):
			if part := strings.TrimSpace(b.String()); part != "" {
				parts = append(parts, part)
			}
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	if escaped {
		b.WriteRune('\\')
	}
	if part := strings.TrimSpace(b.String()); part != "" {
		parts = append(parts, part)
	}
	return parts
}

func isLarkPipelineSeparator(r rune) bool {
	switch r {
	case '|', '｜', '︱', '￨':
		return true
	default:
		return false
	}
}

func PrepareStructuredInput(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return text + "\r"
}

func SubmitStructuredInput(rt *RuntimeSession, text string) error {
	return SubmitStructuredInputWithMention(rt, text, "")
}

// SubmitStructuredInputFrom binds the pre-Enter baseline request to the WebSocket
// that submitted the composer text. A second connected browser must not become
// this round's renderer owner merely because map iteration selected it first.
func SubmitStructuredInputFrom(rt *RuntimeSession, text string, responder chan RuntimeEvent) error {
	return submitStructuredInputWithMentionFrom(rt, text, "", true, responder)
}

func SubmitStructuredInputWithMention(rt *RuntimeSession, text string, mentionOpenID string) error {
	return submitStructuredInputWithMentionFrom(rt, text, mentionOpenID, true, nil)
}

func SubmitSilentStructuredInput(rt *RuntimeSession, text string) error {
	return submitStructuredInputWithMentionFrom(rt, text, "", false, nil)
}

func SubmitTerminalInteractionInputWithMention(rt *RuntimeSession, text string, mentionOpenID string) error {
	return submitStructuredInputWithMode(rt, text, mentionOpenID, true, false, nil)
}

func submitStructuredInputWithMentionFrom(rt *RuntimeSession, text string, mentionOpenID string, trackActivity bool, responder chan RuntimeEvent) error {
	if rt == nil {
		return fmt.Errorf("runtime not found")
	}
	text = strings.TrimRight(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), "\n")
	pressEnter := structuredInputShouldPressEnter(rt, text)
	return submitStructuredInputWithMode(rt, text, mentionOpenID, trackActivity, pressEnter, responder)
}

func submitStructuredInputWithMode(rt *RuntimeSession, text string, mentionOpenID string, trackActivity bool, pressEnter bool, responder chan RuntimeEvent) error {
	if rt == nil {
		return fmt.Errorf("runtime not found")
	}
	text = strings.TrimRight(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), "\n")
	sessionID := rt.Snapshot().ID
	enterLen := 0
	if pressEnter {
		enterLen = len(structuredInputEnterSequence)
	}
	previousRoundUnfinished := false
	if trackActivity && pressEnter {
		// Capture this before writing the new text. The terminal echo itself can
		// transition waiting -> running, but that must not make a completed prior
		// round look like an overlapping unanswered round.
		previousRoundUnfinished = rt.structuredInputPreviousRoundUnfinished()
	}
	log.Printf("lark reply bridge submitting structured input session=%s text_len=%d enter=%v enter_len=%d track_activity=%v", sessionID, len(text), pressEnter, enterLen, trackActivity)
	if trackActivity && !pressEnter {
		// Numeric/menu navigation does not submit a round. Preserve the screen
		// before changing the selection so menu transitions remain diffable.
		rt.PrepareInputSnapshotBaselineFrom(responder)
		rt.SetNotificationMentionOpenID(mentionOpenID)
		rt.MarkStructuredInputActivity(text)
	}
	if _, err := rt.terminal.Write([]byte(text)); err != nil {
		return err
	}
	if !pressEnter {
		return nil
	}
	if structuredInputEnterDelay > 0 {
		time.Sleep(structuredInputEnterDelay)
	}
	if trackActivity {
		// The baseline is an Enter boundary, so capture it after the text has
		// rendered in the active composer and before Enter submits it. This gives
		// the renderer a cursor-backed input identity instead of a historical
		// screen that cannot contain the user's structured input.
		rt.PrepareInputSnapshotBaselineFrom(responder)
		rt.SetNotificationMentionOpenID(mentionOpenID)
		rt.markStructuredInputActivityWithPreviousRoundState(text, previousRoundUnfinished)
	}
	enter := structuredInputEnterSequence
	if enter == "" {
		enter = "\r"
	}
	if _, err := rt.terminal.Write([]byte(enter)); err != nil {
		return err
	}
	return nil
}

func structuredInputShouldPressEnter(rt *RuntimeSession, text string) bool {
	return !structuredInputNumericOnlyRE.MatchString(strings.TrimSpace(text))
}

type larkIncomingMessage struct {
	Text        string
	Attachments []larkAttachmentRef
	Referenced  *larkReferencedContext
}

type larkAttachmentRef struct {
	Kind            string
	Key             string
	Name            string
	SourceMessageID string
	Optional        bool
}

type larkReferencedMessage struct {
	MessageID      string
	UpperMessageID string
	MessageType    string
	Content        string
	Deleted        bool
}

type larkReferencedItem struct {
	MessageType string
	TypeLabel   string
	Text        string
	Attachments int
}

type larkReferencedContext struct {
	Items      []larkReferencedItem
	Warning    string
	Truncated  bool
	TextRunes  int
	Attachment int
}

type pendingLarkAttachment struct {
	Kind string
	Path string
}

func extractLarkMessageText(content string, messageType string) string {
	return extractLarkIncomingMessage(content, messageType).Text
}

func extractLarkIncomingMessage(content string, messageType string) larkIncomingMessage {
	content = strings.TrimSpace(content)
	if content == "" {
		return larkIncomingMessage{}
	}
	var raw any
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		return larkIncomingMessage{Text: content}
	}
	var incoming larkIncomingMessage
	switch messageType {
	case "text":
		if m, ok := raw.(map[string]any); ok {
			incoming.Text = strings.TrimSpace(stringFromAny(m["text"]))
		}
	case "post":
		var parts []string
		collectPostText(raw, &parts)
		incoming.Text = strings.TrimSpace(strings.Join(parts, ""))
		collectLarkAttachmentRefs(raw, &incoming.Attachments)
	case "image":
		collectLarkAttachmentRefs(raw, &incoming.Attachments)
		incoming.Text = strings.TrimSpace(collectLarkPlainTextFields(raw))
	case "file":
		collectLarkAttachmentRefs(raw, &incoming.Attachments)
		incoming.Text = strings.TrimSpace(collectLarkPlainTextFields(raw))
	case "audio", "media":
		collectLarkAttachmentRefs(raw, &incoming.Attachments)
		incoming.Text = strings.TrimSpace(collectLarkPlainTextFields(raw))
	default:
		if m, ok := raw.(map[string]any); ok {
			if text := strings.TrimSpace(stringFromAny(m["text"])); text != "" {
				incoming.Text = text
			}
		}
		var parts []string
		collectPostText(raw, &parts)
		if incoming.Text == "" && len(parts) > 0 {
			incoming.Text = strings.TrimSpace(strings.Join(parts, ""))
		}
		collectLarkAttachmentRefs(raw, &incoming.Attachments)
	}
	if messageType == "image" || messageType == "file" {
		collectLarkAttachmentRefs(raw, &incoming.Attachments)
	}
	incoming.Attachments = dedupeLarkAttachmentRefs(incoming.Attachments)
	return incoming
}

func (b *LarkReplyBridge) resolveReferencedIncoming(ctx context.Context, routeCtx larkRouteContext, incoming larkIncomingMessage) larkIncomingMessage {
	parentID := strings.TrimSpace(routeCtx.ParentID)
	if parentID == "" || b.fetchReferencedMessages == nil {
		return incoming
	}
	contextData := &larkReferencedContext{}
	messages, err := b.fetchReferencedMessages(ctx, parentID)
	if err != nil {
		contextData.Warning = "引用消息无法读取"
		incoming.Referenced = contextData
		log.Printf("lark reply bridge failed to fetch referenced message=%s: %v", parentID, err)
		return incoming
	}
	if len(messages) > maxLarkReferencedItems {
		messages = messages[:maxLarkReferencedItems]
		contextData.Truncated = true
	}
	remainingText := maxLarkReferencedTextRunes
	remainingAttachments := maxLarkReferencedAttachments
	for _, message := range messages {
		item := larkReferencedItem{
			MessageType: strings.ToLower(strings.TrimSpace(message.MessageType)),
			TypeLabel:   larkReferencedMessageTypeLabel(message.MessageType),
		}
		if message.Deleted {
			item.Text = "消息已撤回"
			contextData.Items = append(contextData.Items, item)
			continue
		}
		parsed := extractLarkIncomingMessage(message.Content, item.MessageType)
		item.Text = strings.TrimSpace(parsed.Text)
		if item.Text == "" {
			item.Text = larkReferencedVisibleText(message.Content)
		}
		if item.MessageType == "interactive" && item.Text == "请升级至最新版本客户端，以查看内容" {
			item.Text = ""
		}
		if item.Text == "" {
			item.Text = larkReferencedMessageFallbackText(item.MessageType)
		}
		itemRunes := []rune(item.Text)
		if len(itemRunes) > remainingText {
			itemRunes = itemRunes[:max(remainingText, 0)]
			item.Text = strings.TrimSpace(string(itemRunes))
			contextData.Truncated = true
		}
		remainingText -= len(itemRunes)
		contextData.TextRunes += len(itemRunes)

		if item.MessageType != "sticker" && item.MessageType != "interactive" && remainingAttachments > 0 {
			for _, ref := range parsed.Attachments {
				if remainingAttachments == 0 {
					contextData.Truncated = true
					break
				}
				ref.SourceMessageID = strings.TrimSpace(message.MessageID)
				if ref.SourceMessageID == "" {
					ref.SourceMessageID = parentID
				}
				ref.Optional = true
				incoming.Attachments = append(incoming.Attachments, ref)
				item.Attachments++
				contextData.Attachment++
				remainingAttachments--
			}
		} else if item.MessageType != "sticker" && item.MessageType != "interactive" && len(parsed.Attachments) > 0 {
			contextData.Truncated = true
		}
		contextData.Items = append(contextData.Items, item)
		if remainingText == 0 {
			contextData.Truncated = contextData.Truncated || len(contextData.Items) < len(messages)
			break
		}
	}
	if len(contextData.Items) == 0 {
		contextData.Warning = "引用消息没有可读取的内容"
	}
	incoming.Attachments = dedupeLarkAttachmentRefs(incoming.Attachments)
	incoming.Referenced = contextData
	return incoming
}

func larkReferencedMessageTypeLabel(messageType string) string {
	switch strings.ToLower(strings.TrimSpace(messageType)) {
	case "text":
		return "文本"
	case "post":
		return "富文本"
	case "image":
		return "图片"
	case "file":
		return "文件"
	case "audio":
		return "音频"
	case "media":
		return "视频"
	case "sticker":
		return "表情"
	case "interactive":
		return "卡片"
	case "share_chat":
		return "群聊分享"
	case "share_user":
		return "联系人分享"
	case "location":
		return "位置"
	case "todo":
		return "任务"
	case "merge_forward", "merge_forwarded":
		return "合并转发"
	case "system":
		return "系统消息"
	default:
		if strings.TrimSpace(messageType) == "" {
			return "未知消息"
		}
		return "其他消息（" + strings.TrimSpace(messageType) + "）"
	}
}

func larkReferencedMessageFallbackText(messageType string) string {
	switch strings.ToLower(strings.TrimSpace(messageType)) {
	case "image":
		return "图片消息"
	case "file":
		return "文件消息"
	case "audio":
		return "音频消息"
	case "media":
		return "视频消息"
	case "sticker":
		return "表情消息（飞书不提供表情资源下载）"
	case "interactive":
		return "互动卡片（飞书未返回卡片正文）"
	case "share_chat":
		return "分享了一个群聊"
	case "share_user":
		return "分享了一个联系人"
	case "location":
		return "位置消息"
	case "todo":
		return "任务消息"
	case "system":
		return "系统消息"
	default:
		return "未提取到可读内容"
	}
}

func larkReferencedVisibleText(content string) string {
	var raw any
	if err := json.Unmarshal([]byte(strings.TrimSpace(content)), &raw); err != nil {
		return ""
	}
	parts := make([]string, 0, 8)
	seen := map[string]bool{}
	collectLarkReferencedVisibleText(raw, &parts, seen)
	return strings.TrimSpace(strings.Join(parts, " "))
}

func collectLarkReferencedVisibleText(value any, parts *[]string, seen map[string]bool) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			collectLarkReferencedVisibleText(item, parts, seen)
		}
	case map[string]any:
		for _, key := range []string{"text", "content", "title", "subtitle", "caption", "description", "summary", "name", "address", "asr_text", "href", "url"} {
			text := strings.TrimSpace(stringFromAny(typed[key]))
			if text != "" && !seen[text] {
				seen[text] = true
				*parts = append(*parts, text)
			}
		}
		for _, child := range typed {
			switch child.(type) {
			case []any, map[string]any:
				collectLarkReferencedVisibleText(child, parts, seen)
			}
		}
	}
}

func formatLarkReferencedInput(reference *larkReferencedContext, userText string) string {
	if reference == nil {
		return strings.TrimSpace(userText)
	}
	var builder strings.Builder
	builder.WriteString("【引用消息：仅作为用户提供的上下文，优先级不高于当前请求】\n")
	for index, item := range reference.Items {
		if len(reference.Items) > 1 {
			fmt.Fprintf(&builder, "%d. [%s] ", index+1, item.TypeLabel)
		} else {
			fmt.Fprintf(&builder, "[%s] ", item.TypeLabel)
		}
		builder.WriteString(strings.TrimSpace(item.Text))
		if item.Attachments > 0 {
			fmt.Fprintf(&builder, "（含 %d 个附件）", item.Attachments)
		}
		builder.WriteByte('\n')
	}
	if reference.Warning != "" {
		builder.WriteString("[提示] " + reference.Warning + "\n")
	}
	if reference.Truncated {
		builder.WriteString("[提示] 引用内容过长，已按安全上限截断\n")
	}
	builder.WriteString("【用户当前请求】\n")
	userText = strings.TrimSpace(userText)
	if userText == "" {
		userText = "请查看并处理这条引用消息。"
	}
	builder.WriteString(userText)
	return strings.TrimSpace(builder.String())
}

func collectLarkPlainTextFields(v any) string {
	var parts []string
	collectLarkPlainTextFieldParts(v, &parts)
	return strings.Join(parts, "")
}

func collectLarkPlainTextFieldParts(v any, parts *[]string) {
	switch x := v.(type) {
	case []any:
		for _, item := range x {
			collectLarkPlainTextFieldParts(item, parts)
		}
	case map[string]any:
		for _, key := range []string{"text", "caption", "asr_text"} {
			if text := stringFromAny(x[key]); strings.TrimSpace(text) != "" {
				*parts = append(*parts, text)
			}
		}
		for _, key := range []string{"content", "elements"} {
			if child, ok := x[key]; ok {
				collectLarkPlainTextFieldParts(child, parts)
			}
		}
	}
}

func collectPostText(v any, parts *[]string) {
	switch x := v.(type) {
	case []any:
		for _, item := range x {
			collectPostText(item, parts)
		}
	case map[string]any:
		tag := stringFromAny(x["tag"])
		switch tag {
		case "text", "a":
			*parts = append(*parts, stringFromAny(x["text"]))
		case "at":
			*parts = append(*parts, " ")
		}
		for _, key := range []string{"content", "elements"} {
			if child, ok := x[key]; ok {
				collectPostText(child, parts)
			}
		}
	}
}

func stringFromAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func collectLarkAttachmentRefs(v any, refs *[]larkAttachmentRef) {
	switch x := v.(type) {
	case []any:
		for _, item := range x {
			collectLarkAttachmentRefs(item, refs)
		}
	case map[string]any:
		if key := stringFromAny(x["image_key"]); key != "" {
			*refs = append(*refs, larkAttachmentRef{Kind: "image", Key: key, Name: stringFromAny(x["file_name"])})
		}
		if key := stringFromAny(x["file_key"]); key != "" {
			*refs = append(*refs, larkAttachmentRef{Kind: "file", Key: key, Name: stringFromAny(x["file_name"])})
		}
		for _, child := range x {
			collectLarkAttachmentRefs(child, refs)
		}
	}
}

func dedupeLarkAttachmentRefs(refs []larkAttachmentRef) []larkAttachmentRef {
	if len(refs) < 2 {
		return refs
	}
	seen := make(map[string]bool, len(refs))
	out := refs[:0]
	for _, ref := range refs {
		id := ref.SourceMessageID + ":" + ref.Kind + ":" + ref.Key
		if ref.Key == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, ref)
	}
	return out
}

func formatLarkAttachmentInput(files []pendingLarkAttachment) string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		if file.Path != "" {
			paths = append(paths, quoteLarkInputPath(file.Path))
		}
	}
	if len(paths) == 0 {
		return ""
	}
	return " " + strings.Join(paths, " ")
}

func quoteLarkInputPath(path string) string {
	if path == "" {
		return ""
	}
	if !strings.ContainsAny(path, " \t\n\r\"'\\") {
		return path
	}
	return strconvQuote(path)
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func larkAttachmentUploadSuccessMessage(files []pendingLarkAttachment) string {
	images, regularFiles := 0, 0
	for _, file := range files {
		if file.Kind == "image" {
			images++
		} else {
			regularFiles++
		}
	}
	switch {
	case images > 0 && regularFiles > 0:
		return fmt.Sprintf("%d 张图片、%d 个文件已上传成功，等待你的说明后执行。", images, regularFiles)
	case images > 1:
		return fmt.Sprintf("%d 张图片已上传成功，等待你的说明后执行。", images)
	case images == 1:
		return "图片已上传成功，等待你的说明后执行。"
	case regularFiles > 1:
		return fmt.Sprintf("%d 个文件已上传成功，等待你的说明后执行。", regularFiles)
	default:
		return "文件已上传成功，等待你的说明后执行。"
	}
}

func larkAttachmentKindLabel(kind string) string {
	if kind == "image" {
		return "图片"
	}
	return "文件"
}

func (b *LarkReplyBridge) downloadLarkAttachment(ctx context.Context, messageID, sessionID string, ref larkAttachmentRef) (pendingLarkAttachment, error) {
	if b.apiClient == nil {
		return pendingLarkAttachment{}, errors.New("lark client is not configured")
	}
	resourceType := ref.Kind
	if resourceType == "" {
		resourceType = "file"
	}
	req := larkim.NewGetMessageResourceReqBuilder().
		MessageId(messageID).
		FileKey(ref.Key).
		Type(resourceType).
		Build()
	resp, err := b.apiClient.Im.V1.MessageResource.Get(ctx, req)
	if err != nil {
		return pendingLarkAttachment{}, err
	}
	if !resp.Success() {
		return pendingLarkAttachment{}, fmt.Errorf("飞书资源接口返回 code %d: %s", resp.Code, resp.Msg)
	}
	dir := filepath.Join(b.uploadsDir, sessionID, "lark")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return pendingLarkAttachment{}, err
	}
	filename := safeLarkAttachmentFilename(ref, resp.FileName)
	path := filepath.Join(dir, time.Now().Format("20060102150405.000000000")+"_"+filename)
	if err := resp.WriteFile(path); err != nil {
		return pendingLarkAttachment{}, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return pendingLarkAttachment{Kind: resourceType, Path: abs}, nil
}

func safeLarkAttachmentFilename(ref larkAttachmentRef, responseName string) string {
	name := ref.Name
	if name == "" {
		name = responseName
	}
	if name == "" {
		if ref.Kind == "image" {
			name = "image"
		} else {
			name = "file"
		}
	}
	name = filepath.Base(name)
	name = regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(name, "_")
	name = strings.Trim(name, "._-")
	if name == "" {
		name = "file"
	}
	if filepath.Ext(name) == "" && ref.Kind == "image" {
		name += ".png"
	}
	return name
}

func valueOf(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
