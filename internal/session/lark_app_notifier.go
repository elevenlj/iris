package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

const (
	larkAPIRetryAttempts            = 3
	larkAPIRetryDelay               = 120 * time.Millisecond
	larkCustomShortcutButtonsPerRow = 3
)

type LarkAppNotifier struct {
	appID            string
	appSecret        string
	client           *lark.Client
	uncachedClient   *lark.Client
	tokenMu          sync.RWMutex
	tokenRefreshMu   sync.Mutex
	tenantToken      string
	tokenFetcher     func(context.Context) (string, error)
	receiveID        string
	mention          bool
	customShortcutMu sync.RWMutex
	customShortcuts  []LarkCustomShortcut
	tipMu            sync.Mutex
	tipSent          map[string]map[int]bool
	tipSender        func(string, string, int) error
}

func NewLarkAppNotifier(appID, appSecret, receiveID string, mention bool) *LarkAppNotifier {
	if appID == "" || appSecret == "" || receiveID == "" {
		return &LarkAppNotifier{receiveID: receiveID, mention: mention}
	}
	return &LarkAppNotifier{
		appID:          appID,
		appSecret:      appSecret,
		client:         lark.NewClient(appID, appSecret),
		uncachedClient: lark.NewClient(appID, appSecret, lark.WithEnableTokenCache(false)),
		receiveID:      receiveID,
		mention:        mention,
		tipSent:        make(map[string]map[int]bool),
	}
}

func (n *LarkAppNotifier) Available() bool {
	return n != nil && n.client != nil
}

func (n *LarkAppNotifier) tenantTokenSnapshot() string {
	if n == nil {
		return ""
	}
	n.tokenMu.RLock()
	defer n.tokenMu.RUnlock()
	return n.tenantToken
}

func (n *LarkAppNotifier) rememberTenantToken(token string) {
	if n == nil || strings.TrimSpace(token) == "" {
		return
	}
	n.tokenMu.Lock()
	n.tenantToken = strings.TrimSpace(token)
	n.tokenMu.Unlock()
}

// refreshTenantToken explicitly fetches a token instead of relying on the SDK's
// process-wide cache. That cache can retain an invalid token after macOS wakes
// from sleep. Concurrent failed requests share the first newly fetched token.
func (n *LarkAppNotifier) refreshTenantToken(stale string) (string, error) {
	if n == nil {
		return "", errors.New("lark notifier is not configured")
	}
	n.tokenRefreshMu.Lock()
	defer n.tokenRefreshMu.Unlock()
	if current := n.tenantTokenSnapshot(); current != "" && current != stale {
		return current, nil
	}
	fetch := n.tenantAccessToken
	if n.tokenFetcher != nil {
		fetch = n.tokenFetcher
	}
	token, err := fetch(context.Background())
	if err != nil {
		return "", err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("lark tenant access token is empty")
	}
	n.rememberTenantToken(token)
	return token, nil
}

func (n *LarkAppNotifier) SetCustomShortcuts(shortcuts []LarkCustomShortcut) {
	if n == nil {
		return
	}
	n.customShortcutMu.Lock()
	defer n.customShortcutMu.Unlock()
	n.customShortcuts = normalizeLarkCustomShortcuts(shortcuts)
}

func (n *LarkAppNotifier) customShortcutSnapshot() []LarkCustomShortcut {
	n.customShortcutMu.RLock()
	defer n.customShortcutMu.RUnlock()
	cp := make([]LarkCustomShortcut, len(n.customShortcuts))
	copy(cp, n.customShortcuts)
	return cp
}

func (n *LarkAppNotifier) NotifyWaiting(note WaitingNotification) (WaitingNotificationResult, error) {
	if !n.Available() {
		return WaitingNotificationResult{}, errors.New("lark notifier is not configured")
	}
	content, err := larkNotificationCardContent(note, n.receiveID, n.mention, n.customShortcutSnapshot()...)
	if err != nil {
		return WaitingNotificationResult{}, err
	}
	if note.MessageID != "" {
		return n.updateWaiting(note, content)
	}
	return n.createWaiting(note, content)
}

func larkNotificationCardContent(note WaitingNotification, receiveID string, mention bool, customShortcuts ...LarkCustomShortcut) (string, error) {
	elements := []map[string]any{}
	mentionID := larkNotificationMentionID(note, receiveID)
	if mention && mentionID != "" {
		elements = append(elements, map[string]any{"tag": "markdown", "content": "<at id=" + mentionID + "></at>"})
	}
	var interactionElement map[string]any
	if note.DeveloperModeEnabled && !note.Disabled && !note.Running {
		interactionElement = larkTerminalInteractionElement(note.SessionID, note.Interaction)
	}
	if interactionElement == nil {
		elements = append(elements, larkTerminalTextElement(note.Content, note.SnapshotSource))
	} else {
		if note.Interaction.Kind == TerminalInteractionCodexResume {
			elements = append(elements, larkTerminalInteractionHeadingElement("选择要恢复的会话"))
		}
		elements = append(elements, interactionElement)
	}
	if note.DeveloperModeEnabled {
		if contextElement := larkTerminalAgentContextElement(note.AgentContext); contextElement != nil {
			elements = append(elements, map[string]any{"tag": "hr"})
			elements = append(elements, contextElement)
		}
		developerSelectors := make([]map[string]any, 0, 2)
		if agentElement := larkAgentSelectElement(note.SessionID, note.AgentOptions, note.AgentKind); agentElement != nil {
			developerSelectors = append(developerSelectors, agentElement)
		}
		if strings.EqualFold(note.AgentKind, "codex") {
			if workspaceElement := larkWorkspaceSelectElement(note.SessionID, note.WorkspaceOptions, note.AgentContext); workspaceElement != nil {
				developerSelectors = append(developerSelectors, workspaceElement)
			}
		}
		if selectorRow := larkDeveloperSelectorRow(developerSelectors...); selectorRow != nil {
			elements = append(elements, selectorRow)
		}
	}
	if !note.Disabled {
		elements = append(elements, larkShortcutActionElements(note.SessionID, note.UpdateNo, note.MentionModeEnabled, note.DeveloperModeEnabled)...)
		if shortcuts := normalizeLarkCustomShortcuts(customShortcuts); note.DeveloperModeEnabled && len(shortcuts) > 0 {
			elements = append(elements, map[string]any{"tag": "hr"})
			elements = append(elements, larkCustomShortcutActionElements(note.SessionID, shortcuts)...)
		}
	}
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"wide_screen_mode": true, "update_multi": true},
		"header": map[string]any{
			"template": "blue",
			"title":    map[string]any{"tag": "plain_text", "content": larkNotificationTitle(note)},
		},
		"body": map[string]any{"elements": elements},
	}
	b, err := json.Marshal(card)
	return string(b), err
}

func larkAgentSelectElement(sessionID string, agents []AgentOption, currentKind string) map[string]any {
	agents = normalizeAgentOptions(agents)
	if len(agents) < 2 {
		return nil
	}
	options := make([]map[string]any, 0, len(agents))
	initial := ""
	currentKind = strings.ToLower(strings.TrimSpace(currentKind))
	for _, agent := range agents {
		options = append(options, map[string]any{
			"text":  map[string]any{"tag": "plain_text", "content": agent.Label},
			"value": agent.ID,
		})
		if initial == "" && agent.Kind == currentKind {
			initial = agent.ID
		}
	}
	selector := map[string]any{
		"tag": "select_static", "name": "iris_agent",
		"placeholder": map[string]any{"tag": "plain_text", "content": "切换 Agent"},
		"options":     options,
		"behaviors": []map[string]any{{"type": "callback", "value": map[string]any{
			"iris_action": "agent_select", "session_id": sessionID,
		}}},
	}
	if initial != "" {
		selector["initial_option"] = initial
	}
	return map[string]any{
		"tag":              "column_set",
		"flex_mode":        "none",
		"horizontal_align": "left",
		"columns": []map[string]any{{
			"tag": "column", "width": "auto", "vertical_align": "center", "vertical_spacing": "4px",
			"elements": []map[string]any{
				{"tag": "div", "text": map[string]any{"tag": "plain_text", "content": "Agent"}},
				selector,
			},
		}},
	}
}

func larkWorkspaceSelectElement(sessionID string, workspaces []WorkspaceOption, context *TerminalAgentContext) map[string]any {
	if len(workspaces) == 0 {
		return nil
	}
	options := make([]map[string]any, 0, len(workspaces))
	initial := ""
	current := ""
	if context != nil {
		current = strings.TrimSpace(context.Directory)
	}
	for _, workspace := range workspaces {
		options = append(options, map[string]any{
			"text":  map[string]any{"tag": "plain_text", "content": workspace.Label},
			"value": workspace.Value,
		})
		if workspace.Value == current || (initial == "" && workspace.Default) {
			initial = workspace.Value
		}
	}
	selector := map[string]any{
		"tag": "select_static", "name": "iris_workspace",
		"placeholder": map[string]any{"tag": "plain_text", "content": "切换工作目录"},
		"options":     options,
		"behaviors": []map[string]any{{"type": "callback", "value": map[string]any{
			"iris_action": "workspace_select", "session_id": sessionID,
		}}},
	}
	if initial != "" {
		selector["initial_option"] = initial
	}
	return map[string]any{
		"tag":              "column_set",
		"flex_mode":        "none",
		"horizontal_align": "left",
		"columns": []map[string]any{{
			"tag": "column", "width": "auto", "vertical_align": "center", "vertical_spacing": "4px",
			"elements": []map[string]any{
				{"tag": "div", "text": map[string]any{"tag": "plain_text", "content": "工作目录"}},
				selector,
			},
		}},
	}
}

func larkDeveloperSelectorRow(selectors ...map[string]any) map[string]any {
	columns := make([]map[string]any, 0, len(selectors))
	for _, selector := range selectors {
		if selector == nil {
			continue
		}
		selectorColumns, _ := selector["columns"].([]map[string]any)
		columns = append(columns, selectorColumns...)
	}
	if len(columns) == 0 {
		return nil
	}
	return map[string]any{
		"tag":                "column_set",
		"flex_mode":          "flow",
		"horizontal_align":   "left",
		"horizontal_spacing": "8px",
		"columns":            columns,
	}
}

func larkTerminalInteractionHeadingElement(title string) map[string]any {
	return map[string]any{
		"tag": "div",
		"text": map[string]any{
			"tag":     "plain_text",
			"content": strings.TrimSpace(title),
		},
	}
}

func larkNotificationMentionID(note WaitingNotification, receiveID string) string {
	if id := strings.TrimSpace(note.MentionOpenID); id != "" {
		return id
	}
	if strings.TrimSpace(note.ChatID) != "" {
		return ""
	}
	return strings.TrimSpace(receiveID)
}

func normalizeLarkCustomShortcuts(shortcuts []LarkCustomShortcut) []LarkCustomShortcut {
	out := make([]LarkCustomShortcut, 0, len(shortcuts))
	for _, shortcut := range shortcuts {
		label := strings.TrimSpace(shortcut.Label)
		command := strings.TrimSpace(shortcut.Command)
		if label == "" || command == "" {
			continue
		}
		out = append(out, LarkCustomShortcut{Label: label, Command: command})
	}
	return out
}

func larkTerminalTextElement(content string, snapshotSource ...string) map[string]any {
	preserveOriginalMarkdown := false
	if len(snapshotSource) > 0 {
		preserveOriginalMarkdown = strings.Contains(snapshotSource[0], "hook:last_assistant_message")
	}
	return map[string]any{
		"tag":     "markdown",
		"content": larkTerminalMarkdownTextWithMerge(content, !preserveOriginalMarkdown),
	}
}

func larkTerminalMarkdownText(content string) string {
	return larkTerminalMarkdownTextWithMerge(content, true)
}

func larkTerminalMarkdownTextWithMerge(content string, allowWrappedLineMerge bool) string {
	sourceLines := strings.Split(larkTerminalPlainTextWithMerge(content, allowWrappedLineMerge), "\n")
	lines := make([]string, 0, len(sourceLines))
	inCodeFence := false
	for _, line := range sourceLines {
		startsTopLevelBlock := !inCodeFence && (startsLarkNotifyMarkerBlock(line) || startsLarkNotifyInputPrompt(line))
		if startsTopLevelBlock {
			line = strings.TrimLeftFunc(line, unicode.IsSpace)
			if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
				lines = append(lines, "")
			}
		}
		lines = append(lines, line)
		trimmed := strings.TrimSpace(line)
		if isMarkdownCodeFenceLine(trimmed) {
			inCodeFence = !inCodeFence
		}
	}

	inCodeFence = false
	for i := range lines {
		trimmed := strings.TrimSpace(lines[i])
		if isMarkdownCodeFenceLine(trimmed) {
			inCodeFence = !inCodeFence
			continue
		}
		if i < len(lines)-1 && !inCodeFence && trimmed != "" && !strings.HasSuffix(lines[i], "  ") {
			lines[i] += "  "
		}
	}
	return strings.Join(lines, "\n")
}

func startsLarkNotifyInputPrompt(line string) bool {
	line = strings.TrimLeftFunc(line, unicode.IsSpace)
	return strings.HasPrefix(line, "›")
}

func larkTerminalAgentContextElement(context *TerminalAgentContext) map[string]any {
	if context == nil || strings.TrimSpace(context.Directory) == "" || strings.TrimSpace(context.Model) == "" {
		return nil
	}
	parts := []string{
		"目录：" + truncateLarkInteractionText(context.Directory, 140),
		"模型：" + truncateLarkInteractionText(context.Model, 80),
	}
	if reasoning := strings.TrimSpace(context.Reasoning); reasoning != "" {
		parts = append(parts, "Reasoning："+truncateLarkInteractionText(reasoning, 40))
	}
	return map[string]any{
		"tag": "div",
		"text": map[string]any{
			"tag":     "plain_text",
			"content": strings.Join(parts, " · "),
		},
	}
}

func larkTerminalInteractionElement(sessionID string, interaction *TerminalInteraction) map[string]any {
	minimumOptions := 2
	if interaction != nil && interaction.Kind == TerminalInteractionCodexResume {
		minimumOptions = 1
	}
	if interaction == nil || strings.TrimSpace(interaction.ID) == "" || len(interaction.Options) < minimumOptions {
		return nil
	}
	options := make([]map[string]any, 0, len(interaction.Options))
	initialOption := ""
	for _, option := range interaction.Options {
		optionID := strings.TrimSpace(option.ID)
		label := larkTerminalInteractionOptionLabel(option)
		if optionID == "" || label == "" {
			continue
		}
		options = append(options, map[string]any{
			"text":  map[string]any{"tag": "plain_text", "content": label},
			"value": optionID,
		})
		if option.Current || (initialOption == "" && option.Default) {
			initialOption = optionID
		}
	}
	if len(options) < minimumOptions {
		return nil
	}
	element := map[string]any{
		"tag":        "select_static",
		"element_id": interaction.ID,
		"name":       "iris_select",
		"width":      "fill",
		"placeholder": map[string]any{
			"tag":     "plain_text",
			"content": larkTerminalInteractionPlaceholder(interaction),
		},
		"options": options,
		"behaviors": []map[string]any{
			{
				"type": "callback",
				"value": map[string]any{
					"iris_action":    "terminal_select",
					"session_id":     sessionID,
					"interaction_id": interaction.ID,
				},
			},
		},
	}
	if initialOption != "" {
		element["initial_option"] = initialOption
	}
	return element
}

func larkTerminalInteractionPlaceholder(interaction *TerminalInteraction) string {
	label := "请选择选项"
	switch interaction.Kind {
	case TerminalInteractionCodexModel:
		label = "请选择模型"
	case TerminalInteractionCodexReasoning:
		label = "请选择推理等级"
	case TerminalInteractionCodexResume:
		label = "请选择历史会话"
	}
	for _, option := range interaction.Options {
		if option.Current {
			return truncateLarkInteractionText(label+"（当前："+option.Label+"）", 100)
		}
	}
	for _, option := range interaction.Options {
		if option.Default {
			return truncateLarkInteractionText(label+"（默认："+option.Label+"）", 100)
		}
	}
	return label
}

func larkTerminalInteractionOptionLabel(option TerminalInteractionOption) string {
	label := strings.TrimSpace(option.Label)
	if label == "" {
		return ""
	}
	if input := strings.TrimSpace(option.Input); input != "" && isDecimalTerminalInteractionInput(input) {
		label = input + ". " + label
	}
	if option.Current {
		label += "（当前）"
	} else if option.Default {
		label += "（默认）"
	}
	return truncateLarkInteractionText(label, 100)
}

func isDecimalTerminalInteractionInput(input string) bool {
	if input == "" {
		return false
	}
	for _, r := range input {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func truncateLarkInteractionText(text string, maxRunes int) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return text
	}
	if maxRunes == 1 {
		return "…"
	}
	return string(runes[:maxRunes-1]) + "…"
}

func larkTerminalPlainText(content string) string {
	return larkTerminalPlainTextWithMerge(content, true)
}

func larkTerminalPlainTextWithMerge(content string, allowWrappedLineMerge bool) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	if allowWrappedLineMerge && larkNotifyMergeWrappedLines.Load() {
		content = mergeTerminalWrappedLinesForLark(content)
	}
	return content
}

func larkShortcutActionElements(sessionID string, updateNo int, mentionModeEnabled bool, developerModeEnabled bool) []map[string]any {
	columns := []map[string]any{larkRefreshButtonColumn(sessionID, updateNo), larkDeveloperModeButtonColumn(sessionID, updateNo, developerModeEnabled)}
	if developerModeEnabled {
		columns = append(columns,
			larkMentionModeButtonColumn(sessionID, updateNo, mentionModeEnabled),
			larkRestartAgentButtonColumn(sessionID),
			larkShortcutButtonColumn("Ctrl-C", "default", sessionID, "ctrl_c"),
			larkShortcutButtonColumn("Esc", "default", sessionID, "esc"),
			larkShortcutButtonColumn("Enter", "default", sessionID, "enter"),
			larkDeleteSessionButtonColumn(sessionID),
		)
	}
	return []map[string]any{
		larkFlowShortcutActionElement(columns...),
	}
}

func larkRestartAgentButtonColumn(sessionID string) map[string]any {
	return map[string]any{
		"tag": "column", "width": "auto", "vertical_spacing": "8px",
		"elements": []map[string]any{{
			"tag": "button", "type": "default", "size": "tiny", "width": "default",
			"text": map[string]any{"tag": "plain_text", "content": "重启 Agent"},
			"behaviors": []map[string]any{{"type": "callback", "value": map[string]any{
				"iris_action": "restart_agent", "session_id": sessionID,
			}}},
		}},
	}
}

func larkDeveloperModeButtonColumn(sessionID string, updateNo int, enabled bool) map[string]any {
	label := "开启开发者模式"
	if enabled {
		label = "关闭开发者模式"
	}
	return map[string]any{
		"tag": "column", "width": "auto", "vertical_spacing": "8px",
		"elements": []map[string]any{{
			"tag": "button", "type": "default", "size": "tiny", "width": "default",
			"text": map[string]any{"tag": "plain_text", "content": label},
			"behaviors": []map[string]any{{"type": "callback", "value": map[string]any{
				"iris_action": "toggle_developer_mode", "session_id": sessionID, "update_no": updateNo,
			}}},
		}},
	}
}

func larkShortcutActionElement(columns ...map[string]any) map[string]any {
	return larkShortcutActionElementWithFlexMode("none", columns...)
}

func larkFlowShortcutActionElement(columns ...map[string]any) map[string]any {
	return larkShortcutActionElementWithFlexMode("flow", columns...)
}

func larkShortcutActionElementWithFlexMode(flexMode string, columns ...map[string]any) map[string]any {
	return map[string]any{
		"tag":                "column_set",
		"flex_mode":          flexMode,
		"horizontal_align":   "left",
		"horizontal_spacing": "4px",
		"columns":            columns,
	}
}

func larkMentionModeButtonColumn(sessionID string, updateNo int, enabled bool) map[string]any {
	label := "艾特模式"
	if enabled {
		label = "停艾特"
	}
	return map[string]any{
		"tag":              "column",
		"width":            "auto",
		"vertical_spacing": "8px",
		"elements": []map[string]any{
			{
				"tag":   "button",
				"type":  "default",
				"size":  "tiny",
				"width": "default",
				"text":  map[string]any{"tag": "plain_text", "content": label},
				"behaviors": []map[string]any{
					{
						"type": "callback",
						"value": map[string]any{
							"iris_action": "toggle_mention_mode",
							"session_id":  sessionID,
							"update_no":   updateNo,
						},
					},
				},
			},
		},
	}
}

func larkShortcutButtonColumn(label, buttonType, sessionID, key string) map[string]any {
	return map[string]any{
		"tag":              "column",
		"width":            "auto",
		"vertical_spacing": "8px",
		"elements": []map[string]any{
			larkShortcutButton(label, buttonType, sessionID, key),
		},
	}
}

func larkShortcutButton(label, buttonType, sessionID, key string) map[string]any {
	return map[string]any{
		"tag":   "button",
		"type":  buttonType,
		"size":  "tiny",
		"width": "default",
		"text":  map[string]any{"tag": "plain_text", "content": label},
		"behaviors": []map[string]any{
			{
				"type": "callback",
				"value": map[string]any{
					"iris_action": "shortcut",
					"session_id":  sessionID,
					"key":         key,
				},
			},
		},
	}
}

func larkDeleteSessionButtonColumn(sessionID string) map[string]any {
	return map[string]any{
		"tag":              "column",
		"width":            "auto",
		"vertical_spacing": "8px",
		"elements": []map[string]any{
			{
				"tag":     "button",
				"type":    "danger",
				"size":    "tiny",
				"width":   "default",
				"text":    map[string]any{"tag": "plain_text", "content": "删除会话"},
				"confirm": larkDeleteSessionConfirm(),
				"behaviors": []map[string]any{
					{
						"type": "callback",
						"value": map[string]any{
							"iris_action": "delete_session",
							"session_id":  sessionID,
						},
					},
				},
			},
		},
	}
}

func larkDeleteSessionConfirm() map[string]any {
	return map[string]any{
		"title": map[string]any{"tag": "plain_text", "content": "确认删除会话？"},
		"text":  map[string]any{"tag": "plain_text", "content": "删除后会关闭终端会话，并把机器人从当前群聊移除。"},
	}
}

func larkRefreshButtonColumn(sessionID string, updateNo int) map[string]any {
	return map[string]any{
		"tag":              "column",
		"width":            "auto",
		"vertical_spacing": "8px",
		"elements": []map[string]any{
			{
				"tag":   "button",
				"type":  "primary",
				"size":  "tiny",
				"width": "default",
				"text":  map[string]any{"tag": "plain_text", "content": "刷新"},
				"behaviors": []map[string]any{
					{
						"type": "callback",
						"value": map[string]any{
							"iris_action": "refresh",
							"session_id":  sessionID,
							"update_no":   updateNo,
						},
					},
				},
			},
		},
	}
}

func larkCustomShortcutActionElements(sessionID string, shortcuts []LarkCustomShortcut) []map[string]any {
	return []map[string]any{larkCustomShortcutActionElement(sessionID, shortcuts)}
}

func larkCustomShortcutActionElement(sessionID string, shortcuts []LarkCustomShortcut) map[string]any {
	columns := make([]map[string]any, 0, len(shortcuts))
	for _, shortcut := range shortcuts {
		columns = append(columns, larkCustomShortcutButtonColumn(sessionID, shortcut))
	}
	return map[string]any{
		"tag":                "column_set",
		"flex_mode":          "flow",
		"horizontal_align":   "left",
		"horizontal_spacing": "4px",
		"columns":            columns,
	}
}

func larkCustomShortcutButtonColumn(sessionID string, shortcut LarkCustomShortcut) map[string]any {
	return map[string]any{
		"tag":              "column",
		"width":            "auto",
		"vertical_spacing": "8px",
		"elements": []map[string]any{
			{
				"tag":   "button",
				"type":  "default",
				"size":  "tiny",
				"width": "default",
				"text":  map[string]any{"tag": "plain_text", "content": shortcut.Label},
				"behaviors": []map[string]any{
					{
						"type": "callback",
						"value": map[string]any{
							"iris_action": "custom_shortcut",
							"session_id":  sessionID,
							"command":     shortcut.Command,
						},
					},
				},
			},
		},
	}
}

func larkNotificationTitle(note WaitingNotification) string {
	if note.Running && !note.Disabled {
		return note.Name + "（Running）"
	}
	return note.Name
}

func (n *LarkAppNotifier) createWaiting(note WaitingNotification, content string) (WaitingNotificationResult, error) {
	token, err := n.tenantAccessToken(context.Background())
	if err != nil {
		return WaitingNotificationResult{}, err
	}
	// Card creation already obtains a fresh token directly. Reuse it for later
	// PATCH requests so they do not fall back to the SDK's stale global cache.
	n.rememberTenantToken(token)
	receiveID := n.receiveID
	receiveIDType := "open_id"
	if note.ChatID != "" {
		receiveID = note.ChatID
		receiveIDType = "chat_id"
	}
	if receiveID == "" {
		return WaitingNotificationResult{}, errors.New("lark notification receiver is not configured")
	}
	payload, _ := json.Marshal(map[string]any{
		"receive_id": receiveID,
		"msg_type":   "interactive",
		"content":    string(content),
	})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://open.feishu.cn/open-apis/im/v1/messages?receive_id_type="+receiveIDType, bytes.NewReader(payload))
	if err != nil {
		return WaitingNotificationResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := doHTTPRequestWithRetry(req)
	if err != nil {
		return WaitingNotificationResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return WaitingNotificationResult{}, fmt.Errorf("lark message API returned %s: %s", resp.Status, string(body))
	}
	var createResp struct {
		Code int `json:"code"`
		Data struct {
			MessageID string `json:"message_id"`
			RootID    string `json:"root_id"`
			ParentID  string `json:"parent_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&createResp); err == nil && createResp.Code == 0 {
		defaultLarkMessageRegistry.remember(note.SessionID, createResp.Data.MessageID, createResp.Data.RootID, createResp.Data.ParentID)
		return WaitingNotificationResult{MessageID: createResp.Data.MessageID, RootID: createResp.Data.RootID, ParentID: createResp.Data.ParentID}, nil
	} else {
		defaultLarkMessageRegistry.rememberLatest(note.SessionID)
		if createResp.Code != 0 {
			return WaitingNotificationResult{}, fmt.Errorf("lark message API returned code %d", createResp.Code)
		}
	}
	return WaitingNotificationResult{}, nil
}

func (n *LarkAppNotifier) updateWaiting(note WaitingNotification, content string) (WaitingNotificationResult, error) {
	req := larkim.NewPatchMessageReqBuilder().
		MessageId(note.MessageID).
		Body(larkim.NewPatchMessageReqBodyBuilder().
			Content(content).
			Build()).
		Build()
	resp, err := n.patchMessage(req)
	if err != nil {
		return WaitingNotificationResult{}, err
	}
	if !resp.Success() {
		return WaitingNotificationResult{}, fmt.Errorf("lark patch message API returned code %d: %s", resp.Code, resp.Msg)
	}
	tipSent := false
	if note.UpdateNo > 0 && !note.SuppressUpdateTip {
		if err := n.sendUpdateTipOnce(note.MessageID, note.ChatID, note.UpdateNo, larkNotificationMentionID(note, n.receiveID)); err == nil {
			tipSent = true
		}
	}
	defaultLarkMessageRegistry.remember(note.SessionID, note.MessageID)
	return WaitingNotificationResult{MessageID: note.MessageID, Updated: true, TipSent: tipSent}, nil
}

func (n *LarkAppNotifier) UpdateWaitingRunning(note WaitingNotification, running bool) error {
	if !n.Available() || note.MessageID == "" {
		return nil
	}
	note.Running = running
	content, err := larkNotificationCardContent(note, n.receiveID, n.mention, n.customShortcutSnapshot()...)
	if err != nil {
		return err
	}
	req := larkim.NewPatchMessageReqBuilder().
		MessageId(note.MessageID).
		Body(larkim.NewPatchMessageReqBodyBuilder().
			Content(content).
			Build()).
		Build()
	resp, err := n.patchMessage(req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("lark patch message API returned code %d: %s", resp.Code, resp.Msg)
	}
	defaultLarkMessageRegistry.remember(note.SessionID, note.MessageID)
	return nil
}

func (n *LarkAppNotifier) sendUpdateTipOnce(messageID string, chatID string, updateNo int, mentionID string) error {
	if messageID == "" || updateNo <= 0 {
		return nil
	}
	n.tipMu.Lock()
	if n.tipSent == nil {
		n.tipSent = make(map[string]map[int]bool)
	}
	sent := n.tipSent[messageID]
	if sent == nil {
		sent = make(map[int]bool)
		n.tipSent[messageID] = sent
	}
	if sent[updateNo] {
		n.tipMu.Unlock()
		return nil
	}
	n.tipMu.Unlock()

	send := n.sendUpdateTip
	if n.tipSender != nil {
		send = func(messageID string, chatID string, updateNo int, _ string) error {
			return n.tipSender(messageID, chatID, updateNo)
		}
	}
	if err := retryLarkVoid(func() error { return send(messageID, chatID, updateNo, mentionID) }); err != nil {
		return err
	}

	n.tipMu.Lock()
	if n.tipSent[messageID] == nil {
		n.tipSent[messageID] = make(map[int]bool)
	}
	n.tipSent[messageID][updateNo] = true
	n.tipMu.Unlock()
	return nil
}

func (n *LarkAppNotifier) sendUpdateTip(messageID string, chatID string, updateNo int, mentionID string) error {
	content, err := larkUpdateTipCardContent(updateNo, mentionID, n.mention)
	if err != nil {
		return err
	}
	receiveID := strings.TrimSpace(chatID)
	receiveIDType := "chat_id"
	if receiveID == "" {
		receiveID = strings.TrimSpace(n.receiveID)
		receiveIDType = "open_id"
	}
	if receiveID == "" {
		return nil
	}
	req := larkim.NewCreateMessageReqBuilder().ReceiveIdType(receiveIDType).Body(
		larkim.NewCreateMessageReqBodyBuilder().ReceiveId(receiveID).MsgType("interactive").Content(content).Build(),
	).Build()
	resp, err := n.createMessage(req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("lark completion tip message API returned code %d: %s", resp.Code, resp.Msg)
	}
	return nil
}

func (n *LarkAppNotifier) patchMessage(req *larkim.PatchMessageReq) (*larkim.PatchMessageResp, error) {
	return retryLarkPatchMessage(func() (*larkim.PatchMessageResp, error) {
		if n == nil || n.client == nil {
			return nil, errors.New("lark notifier is not configured")
		}
		staleToken := n.tenantTokenSnapshot()
		resp, err := n.patchLarkMessageWithToken(req, staleToken)
		if err != nil || !larkAccessTokenInvalid(resp) {
			return resp, err
		}

		freshToken, refreshErr := n.refreshTenantToken(staleToken)
		if refreshErr != nil {
			return resp, fmt.Errorf("refresh lark tenant access token: %w", refreshErr)
		}
		return n.patchLarkMessageWithToken(req, freshToken)
	})
}

func (n *LarkAppNotifier) createMessage(req *larkim.CreateMessageReq) (*larkim.CreateMessageResp, error) {
	return retryLarkCreateMessage(func() (*larkim.CreateMessageResp, error) {
		if n == nil || n.client == nil {
			return nil, errors.New("lark notifier is not configured")
		}
		staleToken := n.tenantTokenSnapshot()
		resp, err := n.createLarkMessageWithToken(req, staleToken)
		if err != nil || !larkCreateAccessTokenInvalid(resp) {
			return resp, err
		}

		freshToken, refreshErr := n.refreshTenantToken(staleToken)
		if refreshErr != nil {
			return resp, fmt.Errorf("refresh lark tenant access token: %w", refreshErr)
		}
		return n.createLarkMessageWithToken(req, freshToken)
	})
}

func (n *LarkAppNotifier) patchLarkMessageWithToken(req *larkim.PatchMessageReq, token string) (*larkim.PatchMessageResp, error) {
	if token == "" {
		return n.client.Im.V1.Message.Patch(context.Background(), req)
	}
	if n.uncachedClient == nil {
		return nil, errors.New("lark uncached client is not configured")
	}
	return n.uncachedClient.Im.V1.Message.Patch(context.Background(), req, larkcore.WithTenantAccessToken(token))
}

func (n *LarkAppNotifier) createLarkMessageWithToken(req *larkim.CreateMessageReq, token string) (*larkim.CreateMessageResp, error) {
	if token == "" {
		return n.client.Im.V1.Message.Create(context.Background(), req)
	}
	if n.uncachedClient == nil {
		return nil, errors.New("lark uncached client is not configured")
	}
	return n.uncachedClient.Im.V1.Message.Create(context.Background(), req, larkcore.WithTenantAccessToken(token))
}

func larkAccessTokenInvalid(resp *larkim.PatchMessageResp) bool {
	return resp != nil && invalidLarkAccessTokenCode(resp.Code)
}

func larkCreateAccessTokenInvalid(resp *larkim.CreateMessageResp) bool {
	return resp != nil && invalidLarkAccessTokenCode(resp.Code)
}

func invalidLarkAccessTokenCode(code int) bool {
	return code == 99991663
}

func larkUpdateTipCardContent(_ int, receiveID string, mention bool) (string, error) {
	elements := []map[string]any{}
	if mention && strings.TrimSpace(receiveID) != "" {
		elements = append(elements, map[string]any{"tag": "markdown", "content": "<at id=" + strings.TrimSpace(receiveID) + "></at>"})
	}
	elements = append(elements, map[string]any{"tag": "note", "elements": []map[string]any{{"tag": "plain_text", "content": "任务已完成"}}})
	b, err := json.Marshal(map[string]any{
		"config":   map[string]any{"wide_screen_mode": false},
		"elements": elements,
	})
	return string(b), err
}

func (n *LarkAppNotifier) tenantAccessToken(ctx context.Context) (string, error) {
	payload, _ := json.Marshal(map[string]string{"app_id": n.appID, "app_secret": n.appSecret})
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

func doHTTPRequestWithRetry(req *http.Request) (*http.Response, error) {
	var lastErr error
	for attempt := 1; attempt <= larkAPIRetryAttempts; attempt++ {
		cloned := req.Clone(req.Context())
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			cloned.Body = body
		}
		resp, err := http.DefaultClient.Do(cloned)
		if err == nil && resp != nil && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}
		if resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("%s: %s", resp.Status, string(body))
		} else {
			lastErr = err
		}
		if attempt < larkAPIRetryAttempts {
			time.Sleep(time.Duration(attempt) * larkAPIRetryDelay)
		}
	}
	if lastErr == nil {
		lastErr = errors.New("lark request failed")
	}
	return nil, lastErr
}

func retryLarkPatchMessage(fn func() (*larkim.PatchMessageResp, error)) (*larkim.PatchMessageResp, error) {
	var lastResp *larkim.PatchMessageResp
	err := retryLarkVoid(func() error {
		resp, err := fn()
		lastResp = resp
		if err != nil {
			return err
		}
		if resp == nil {
			return errors.New("lark patch message API returned empty response")
		}
		if resp != nil && !resp.Success() && retryableLarkCode(resp.Code) {
			return fmt.Errorf("lark patch message API returned code %d: %s", resp.Code, resp.Msg)
		}
		return nil
	})
	return lastResp, err
}

func retryLarkCreateMessage(fn func() (*larkim.CreateMessageResp, error)) (*larkim.CreateMessageResp, error) {
	var lastResp *larkim.CreateMessageResp
	err := retryLarkVoid(func() error {
		resp, err := fn()
		lastResp = resp
		if err != nil {
			return err
		}
		if resp == nil {
			return errors.New("lark create message API returned empty response")
		}
		if resp != nil && !resp.Success() && retryableLarkCode(resp.Code) {
			return fmt.Errorf("lark create message API returned code %d: %s", resp.Code, resp.Msg)
		}
		return nil
	})
	return lastResp, err
}

func retryLarkVoid(fn func() error) error {
	var lastErr error
	for attempt := 1; attempt <= larkAPIRetryAttempts; attempt++ {
		if err := fn(); err != nil {
			lastErr = err
			if attempt < larkAPIRetryAttempts {
				time.Sleep(time.Duration(attempt) * larkAPIRetryDelay)
			}
			continue
		}
		return nil
	}
	return lastErr
}

func retryableLarkCode(code int) bool {
	return code == 99991400 || code == 99991663 || code >= 50000000
}
