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
	"regexp"
	"strconv"
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

var (
	larkMarkdownImagePattern          = regexp.MustCompile(`!\[([^\]\r\n]*)\]\(\s*(?:<[^>\r\n]+>|[^)\r\n]+)\s*\)`)
	larkMarkdownTableRowPattern       = regexp.MustCompile(`^\s*\|(.+)\|\s*$`)
	larkMarkdownTableSeparatorPattern = regexp.MustCompile(`^\s*\|(?:\s*:?-{3,}:?\s*\|)+\s*$`)
)

type LarkAppNotifier struct {
	registry         *LarkMessageRegistry
	cardsMu          sync.Mutex
	cards            *larkCardState
	cardsPath        string
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

type larkCardState struct {
	mu       sync.Mutex
	latest   map[string]WaitingNotification
	retired  map[string]bool
	recalled map[string]bool
	pending  map[string]WaitingNotification
}

func (n *LarkAppNotifier) cardState() *larkCardState {
	n.cardsMu.Lock()
	defer n.cardsMu.Unlock()
	if n.cards == nil {
		n.cards = &larkCardState{latest: map[string]WaitingNotification{}, retired: map[string]bool{}, recalled: map[string]bool{}, pending: map[string]WaitingNotification{}}
		if n.cardsPath != "" {
			data, err := os.ReadFile(n.cardsPath)
			if err == nil {
				var saved struct {
					Latest   map[string]WaitingNotification
					Retired  map[string]bool
					Recalled map[string]bool
					Pending  map[string]WaitingNotification
				}
				if err := json.Unmarshal(data, &saved); err != nil {
					log.Printf("load card controls state: %v", err)
				} else {
					if saved.Latest != nil {
						n.cards.latest = saved.Latest
					}
					if saved.Retired != nil {
						n.cards.retired = saved.Retired
					}
					if saved.Recalled != nil {
						n.cards.recalled = saved.Recalled
					}
					if saved.Pending != nil {
						n.cards.pending = saved.Pending
					}
				}
			}
		}
	}
	return n.cards
}

// Called with state.mu held; the existing atomic writer keeps crash recovery safe.
func (n *LarkAppNotifier) persistCards(state *larkCardState) {
	if n.cardsPath == "" {
		return
	}
	data, err := json.Marshal(map[string]any{"Latest": state.latest, "Retired": state.retired, "Recalled": state.recalled, "Pending": state.pending})
	if err == nil {
		err = writeFileAtomically(n.cardsPath, data, 0600)
	}
	if err != nil {
		log.Printf("save card controls state: %v", err)
	}
}

func (n *LarkAppNotifier) messageRegistry() *LarkMessageRegistry {
	if n.registry != nil {
		return n.registry
	}
	return defaultLarkMessageRegistry
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
	// ponytail: serialize card writes per bot; use per-session locks if traffic requires it.
	state := n.cardState()
	state.mu.Lock()
	defer state.mu.Unlock()
	key := note.SessionID + "\x00" + note.ChatID
	previous := state.latest[key]
	if state.recalled[note.MessageID] {
		return WaitingNotificationResult{MessageID: note.MessageID, Updated: true}, nil
	}
	if state.retired[note.MessageID] {
		note.Disabled = true
		note.SuppressUpdateTip = true
	}
	result, err := n.writeWaiting(note)
	if err != nil {
		return result, err
	}
	if result.MessageID != "" && !note.Disabled {
		note.MessageID = result.MessageID
		state.latest[key] = note
		if previous.MessageID != "" && previous.MessageID != note.MessageID {
			previous.Disabled = true
			previous.SuppressUpdateTip = true
			state.retired[previous.MessageID] = true
			state.pending[previous.MessageID] = previous
			// A running marker can already have been cleared by the new input.
			// Only recall our empty placeholder, never an answer or a startup card.
			if !previous.Startup && previous.UpdateNo == 0 && previous.Content == RunningNotificationPlaceholder {
				state.recalled[previous.MessageID] = true
			}
		}
	} else if note.MessageID != "" && note.MessageID == previous.MessageID && !state.retired[note.MessageID] {
		// Freezing the current card can also deliver its final body before the
		// next card arrives. Retain that body when deciding whether to recall it.
		state.latest[key] = note
	}
	// A failed retirement never retries creation of a card already delivered.
	n.persistCards(state)
	for id, old := range state.pending {
		var err error
		if state.recalled[id] {
			err = n.recallMessage(id)
		} else {
			_, err = n.writeWaiting(old)
		}
		if err != nil {
			log.Printf("retire old card failed message=%s: %v", id, err)
		} else {
			delete(state.pending, id)
		}
	}
	n.persistCards(state)
	return result, nil
}

func (n *LarkAppNotifier) writeWaiting(note WaitingNotification) (WaitingNotificationResult, error) {
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
	if !note.Startup && !note.Running && strings.TrimSpace(note.AssistantName) != "" {
		elements = append(elements, map[string]any{"tag": "div", "text": map[string]any{"tag": "plain_text", "content": "我是" + strings.TrimSpace(note.AssistantName) + "的助理。"}})
	}
	mentionID := larkNotificationMentionID(note, receiveID)
	if mention && mentionID != "" {
		elements = append(elements, map[string]any{"tag": "markdown", "content": "<at id=" + mentionID + "></at>"})
	}
	if note.Startup {
		elements = append(elements, larkTerminalTextElements(note.Content, note.SnapshotSource)...)
		if note.StartupInputEnabled && !note.StartupComplete && !note.Disabled {
			elements = append(elements, larkStartupInputFormElement(note.SessionID))
		}
		if !note.StartupComplete && !note.Disabled && !note.DeveloperModeEnabled {
			elements = append(elements, larkFlowShortcutActionElement(larkRestartAgentButtonColumn(note.SessionID)))
		}
		if note.StartupComplete && !note.StartupFailed {
			if contextElement := larkTerminalAgentContextElement(note.AgentContext, larkNotificationAgentLabel(note)); contextElement != nil {
				elements = append(elements, map[string]any{"tag": "hr"}, contextElement)
			}
			if workspaceElement := larkWorkspaceSelectElement(note.SessionID, note.WorkspaceOptions, note.AgentContext); workspaceElement != nil && !note.Disabled {
				elements = append(elements, workspaceElement)
			}
		}
		if !note.Disabled {
			elements = append(elements, larkShortcutActionElements(note.SessionID, note.UpdateNo, note.MentionModeEnabled, note.AssistantModeEnabled, note.DeveloperModeEnabled, note.TerminalURL)...)
			if shortcuts := normalizeLarkCustomShortcuts(customShortcuts); note.DeveloperModeEnabled && len(shortcuts) > 0 {
				elements = append(elements, map[string]any{"tag": "hr"})
				elements = append(elements, larkCustomShortcutActionElements(note.SessionID, shortcuts)...)
			}
		}
	} else {
		var interactionElement map[string]any
		if note.DeveloperModeEnabled && !note.Disabled && !note.Running {
			interactionElement = larkTerminalInteractionElement(note.SessionID, note.Interaction)
		}
		if interactionElement == nil {
			elements = append(elements, larkTerminalTextElements(note.Content, note.SnapshotSource)...)
		} else {
			if note.Interaction.Kind == TerminalInteractionCodexResume {
				elements = append(elements, larkTerminalInteractionHeadingElement("选择要恢复的会话"))
			}
			elements = append(elements, interactionElement)
		}
		if note.DeveloperModeEnabled && note.AssistantName == "" && !note.Disabled {
			if contextElement := larkTerminalAgentContextElement(note.AgentContext, ""); contextElement != nil {
				elements = append(elements, map[string]any{"tag": "hr"})
				elements = append(elements, contextElement)
			}
			developerSelectors := make([]map[string]any, 0, 2)
			currentAgentID := strings.TrimSpace(note.AgentID)
			if currentAgentID == "" {
				currentAgentID = strings.TrimSpace(note.AgentKind)
			}
			if agentElement := larkAgentSelectElement(note.SessionID, note.AgentOptions, currentAgentID); agentElement != nil {
				developerSelectors = append(developerSelectors, agentElement)
			}
			if len(note.WorkspaceOptions) > 0 {
				if workspaceElement := larkWorkspaceSelectElement(note.SessionID, note.WorkspaceOptions, note.AgentContext); workspaceElement != nil {
					developerSelectors = append(developerSelectors, workspaceElement)
				}
			}
			if selectorRow := larkDeveloperSelectorRow(developerSelectors...); selectorRow != nil {
				elements = append(elements, selectorRow)
			}
		}
		if !note.Disabled && note.AssistantName == "" {
			elements = append(elements, larkShortcutActionElements(note.SessionID, note.UpdateNo, note.MentionModeEnabled, note.AssistantModeEnabled, note.DeveloperModeEnabled, note.TerminalURL)...)
			if shortcuts := normalizeLarkCustomShortcuts(customShortcuts); note.DeveloperModeEnabled && len(shortcuts) > 0 {
				elements = append(elements, map[string]any{"tag": "hr"})
				elements = append(elements, larkCustomShortcutActionElements(note.SessionID, shortcuts)...)
			}
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

func larkStartupInputFormElement(sessionID string) map[string]any {
	return map[string]any{
		"tag":  "form",
		"name": "iris_startup_form",
		"elements": []map[string]any{
			{
				"tag":   "input",
				"name":  "iris_startup_input",
				"width": "fill",
				"placeholder": map[string]any{
					"tag":     "plain_text",
					"content": "输入启动选项或确认内容",
				},
			},
			{
				"tag":              "button",
				"name":             "iris_startup_submit",
				"type":             "primary",
				"width":            "fill",
				"form_action_type": "submit",
				"text":             map[string]any{"tag": "plain_text", "content": "提交"},
				"behaviors": []map[string]any{
					{
						"type": "callback",
						"value": map[string]any{
							"iris_action": "startup_submit",
							"session_id":  sessionID,
						},
					},
				},
			},
		},
	}
}

func larkAgentSelectElement(sessionID string, agents []AgentOption, currentID string) map[string]any {
	agents = normalizeAgentOptions(agents)
	if len(agents) < 2 {
		return nil
	}
	options := make([]map[string]any, 0, len(agents))
	initial := ""
	currentID = strings.ToLower(strings.TrimSpace(currentID))
	for _, agent := range agents {
		options = append(options, map[string]any{
			"text":  map[string]any{"tag": "plain_text", "content": agent.Label},
			"value": agent.ID,
		})
		if initial == "" && agent.ID == currentID {
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
	currentPath, currentPathOK := "", false
	if current != "" {
		currentPath, currentPathOK = resolveShellCWD("", "", current)
	}
	for _, workspace := range workspaces {
		options = append(options, map[string]any{
			"text":  map[string]any{"tag": "plain_text", "content": workspace.Label},
			"value": workspace.Value,
		})
		workspacePath, workspacePathOK := resolveShellCWD("", "", workspace.Value)
		if (currentPathOK && workspacePathOK && workspacePath == currentPath) || (initial == "" && workspace.Default) {
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

func larkTerminalTextElements(content string, snapshotSource ...string) []map[string]any {
	preserveOriginalMarkdown := false
	if len(snapshotSource) > 0 {
		preserveOriginalMarkdown = strings.Contains(snapshotSource[0], "hook:last_assistant_message")
	}
	plain := larkTerminalPlainTextWithMerge(content, !preserveOriginalMarkdown)
	sourceLines := strings.Split(plain, "\n")
	elements := make([]map[string]any, 0, 3)
	textStart, tableCount := 0, 0
	inCodeFence := false
	for i := 0; i < len(sourceLines); {
		if isMarkdownCodeFenceLine(strings.TrimSpace(sourceLines[i])) {
			inCodeFence = !inCodeFence
			i++
			continue
		}
		headers, rows, consumed := parseLarkMarkdownTable(sourceLines[i:])
		if inCodeFence || consumed == 0 || len(headers) > 50 || tableCount == 5 {
			i++
			continue
		}
		if text := strings.Join(sourceLines[textStart:i], "\n"); strings.TrimSpace(text) != "" {
			elements = append(elements, map[string]any{"tag": "markdown", "content": larkTerminalMarkdownTextWithMerge(text, false)})
		}
		elements = append(elements, larkMarkdownTableElement(headers, rows))
		tableCount++
		i += consumed
		textStart = i
	}
	if text := strings.Join(sourceLines[textStart:], "\n"); strings.TrimSpace(text) != "" || len(elements) == 0 {
		elements = append(elements, map[string]any{"tag": "markdown", "content": larkTerminalMarkdownTextWithMerge(text, false)})
	}
	return elements
}

func larkTerminalMarkdownText(content string) string {
	return larkTerminalMarkdownTextWithMerge(content, true)
}

func larkTerminalMarkdownTextWithMerge(content string, allowWrappedLineMerge bool) string {
	sourceLines := strings.Split(larkTerminalPlainTextWithMerge(content, allowWrappedLineMerge), "\n")
	lines := make([]string, 0, len(sourceLines))
	inCodeFence := false
	for i := 0; i < len(sourceLines); i++ {
		line := sourceLines[i]
		startsTopLevelBlock := !inCodeFence && (startsLarkNotifyMarkerBlock(line) || startsLarkNotifyInputPrompt(line))
		if startsTopLevelBlock {
			line = strings.TrimLeftFunc(line, unicode.IsSpace)
			if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
				lines = append(lines, "")
			}
		}
		if !inCodeFence {
			if table, consumed := larkMarkdownTableBlock(sourceLines[i:]); consumed > 0 {
				lines = append(lines, table...)
				i += consumed - 1
				continue
			}
			if larkMarkdownTableSeparatorPattern.MatchString(line) {
				continue
			}
			if match := larkMarkdownTableRowPattern.FindStringSubmatch(line); match != nil {
				line = strings.TrimSpace(match[1])
			}
			line = larkMarkdownImagePattern.ReplaceAllString(line, "$1（图片未随卡片发送）")
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

func larkMarkdownTableBlock(lines []string) ([]string, int) {
	headers, rows, consumed := parseLarkMarkdownTable(lines)
	if consumed == 0 {
		return nil, 0
	}
	formatted := make([]string, 0)
	for _, cells := range rows {
		if len(formatted) > 0 {
			formatted = append(formatted, "")
		}
		for i, cell := range cells {
			cell = strings.TrimSpace(larkMarkdownImagePattern.ReplaceAllString(cell, "$1（图片未随卡片发送）"))
			if cell == "" {
				continue
			}
			if i == 0 {
				formatted = append(formatted, "**"+cell+"**")
			} else if i < len(headers) && strings.TrimSpace(headers[i]) != "" {
				formatted = append(formatted, "**"+strings.TrimSpace(headers[i])+"：** "+cell)
			} else {
				formatted = append(formatted, cell)
			}
		}
	}
	return formatted, consumed
}

func parseLarkMarkdownTable(lines []string) ([]string, [][]string, int) {
	if len(lines) < 3 || !larkMarkdownTableSeparatorPattern.MatchString(lines[1]) {
		return nil, nil, 0
	}
	headerMatch := larkMarkdownTableRowPattern.FindStringSubmatch(lines[0])
	if headerMatch == nil {
		return nil, nil, 0
	}
	headers := strings.Split(headerMatch[1], "|")
	rows := make([][]string, 0)
	consumed := 2
	for consumed < len(lines) {
		rowMatch := larkMarkdownTableRowPattern.FindStringSubmatch(lines[consumed])
		if rowMatch == nil {
			break
		}
		rows = append(rows, strings.Split(rowMatch[1], "|"))
		consumed++
	}
	if len(rows) == 0 {
		return nil, nil, 0
	}
	return headers, rows, consumed
}

func larkMarkdownTableElement(headers []string, rows [][]string) map[string]any {
	columns := make([]map[string]any, len(headers))
	for i, header := range headers {
		columns[i] = map[string]any{
			"name": "col_" + strconv.Itoa(i), "display_name": strings.Trim(strings.TrimSpace(header), "*_`"),
			"data_type": "lark_md", "width": "auto", "vertical_align": "top",
		}
	}
	tableRows := make([]map[string]any, len(rows))
	for i, cells := range rows {
		tableRows[i] = make(map[string]any, len(headers))
		for j := range headers {
			cell := ""
			if j < len(cells) {
				cell = strings.TrimSpace(larkMarkdownImagePattern.ReplaceAllString(cells[j], "$1（图片未随卡片发送）"))
			}
			tableRows[i]["col_"+strconv.Itoa(j)] = cell
		}
	}
	pageSize := len(rows)
	if pageSize > 10 {
		pageSize = 10
	}
	return map[string]any{
		"tag": "table", "columns": columns, "rows": tableRows, "page_size": pageSize,
		"row_height": "auto", "row_max_height": "999px", "freeze_first_column": len(headers) > 2,
		"header_style": map[string]any{"bold": true, "background_style": "grey", "lines": 1},
	}
}

func startsLarkNotifyInputPrompt(line string) bool {
	line = strings.TrimLeftFunc(line, unicode.IsSpace)
	return strings.HasPrefix(line, "›")
}

func larkTerminalAgentContextElement(context *TerminalAgentContext, agentLabel string) map[string]any {
	parts := []string{}
	if agentLabel = strings.TrimSpace(agentLabel); agentLabel != "" {
		parts = append(parts, "Agent："+truncateLarkInteractionText(agentLabel, 80))
	}
	if context == nil || strings.TrimSpace(context.Directory) == "" {
		if len(parts) == 0 {
			return nil
		}
	} else {
		parts = append(parts, "目录："+truncateLarkInteractionText(context.Directory, 140))
		if model := strings.TrimSpace(context.Model); model != "" {
			parts = append(parts, "模型："+truncateLarkInteractionText(model, 80))
		}
		if reasoning := strings.TrimSpace(context.Reasoning); reasoning != "" {
			parts = append(parts, "Reasoning："+truncateLarkInteractionText(reasoning, 40))
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return map[string]any{
		"tag": "div",
		"text": map[string]any{
			"tag":     "plain_text",
			"content": strings.Join(parts, " · "),
		},
	}
}

func larkNotificationAgentLabel(note WaitingNotification) string {
	for _, option := range note.AgentOptions {
		if strings.EqualFold(strings.TrimSpace(option.ID), strings.TrimSpace(note.AgentID)) {
			return strings.TrimSpace(option.Label)
		}
	}
	agent := normalizeAgentConfig(AgentConfig{Kind: note.AgentKind})
	if agent.Kind != "custom" {
		return agent.Name
	}
	return ""
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

func larkShortcutActionElements(sessionID string, updateNo int, mentionModeEnabled, assistantModeEnabled, developerModeEnabled bool, terminalURL string) []map[string]any {
	columns := []map[string]any{larkRefreshButtonColumn(sessionID, updateNo), larkDeveloperModeButtonColumn(sessionID, updateNo, developerModeEnabled)}
	elements := []map[string]any{}
	if developerModeEnabled {
		columns = append(columns,
			larkMentionModeButtonColumn(sessionID, updateNo, mentionModeEnabled),
			larkAssistantModeButtonColumn(sessionID, updateNo, assistantModeEnabled),
			larkRestartAgentButtonColumn(sessionID),
			larkDeleteSessionButtonColumn(sessionID),
		)
		elements = append(elements, larkFlowShortcutActionElement(columns...))
		shortcutColumns := []map[string]any{
			larkShortcutButtonColumn("Ctrl-C", "default", sessionID, "ctrl_c"),
			larkShortcutButtonColumn("Esc", "default", sessionID, "esc"),
			larkShortcutButtonColumn("Enter", "default", sessionID, "enter"),
		}
		if strings.TrimSpace(terminalURL) != "" {
			shortcutColumns = append(shortcutColumns, larkOpenTerminalButtonColumn(terminalURL))
		}
		elements = append(elements, larkFlowShortcutActionElement(shortcutColumns...))
		return elements
	}
	return []map[string]any{larkFlowShortcutActionElement(columns...)}
}

func larkOpenTerminalButtonColumn(terminalURL string) map[string]any {
	return map[string]any{
		"tag": "column", "width": "auto", "vertical_spacing": "8px",
		"elements": []map[string]any{{
			"tag": "button", "type": "default", "size": "tiny", "width": "default",
			"text":      map[string]any{"tag": "plain_text", "content": "打开终端"},
			"behaviors": []map[string]any{{"type": "open_url", "default_url": terminalURL, "pc_url": terminalURL}},
		}},
	}
}

func larkAssistantModeButtonColumn(sessionID string, updateNo int, enabled bool) map[string]any {
	label := "助理模式：关"
	if enabled {
		label = "助理模式：开"
	}
	return map[string]any{
		"tag": "column", "width": "auto", "vertical_spacing": "8px",
		"elements": []map[string]any{{
			"tag": "button", "type": "default", "size": "tiny", "width": "default",
			"text": map[string]any{"tag": "plain_text", "content": label},
			"behaviors": []map[string]any{{"type": "callback", "value": map[string]any{
				"iris_action": "toggle_assistant_mode", "session_id": sessionID, "update_no": updateNo,
			}}},
		}},
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
	label := "开发者模式：关"
	if enabled {
		label = "开发者模式：开"
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
	label := "艾特模式：关"
	if enabled {
		label = "艾特模式：开"
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
	if note.Startup {
		if note.StartupFailed {
			return note.Name + "（启动失败）"
		}
		if note.StartupComplete {
			return note.Name + "（启动完成）"
		}
		return note.Name + "（启动中）"
	}
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
	if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		return WaitingNotificationResult{}, fmt.Errorf("invalid lark message response: %w", err)
	}
	if createResp.Code != 0 {
		return WaitingNotificationResult{}, fmt.Errorf("lark message API returned code %d", createResp.Code)
	}
	if strings.TrimSpace(createResp.Data.MessageID) == "" {
		return WaitingNotificationResult{}, errors.New("lark message API did not return a message ID")
	}
	n.messageRegistry().remember(note.SessionID, createResp.Data.MessageID, createResp.Data.RootID, createResp.Data.ParentID)
	return WaitingNotificationResult{MessageID: createResp.Data.MessageID, RootID: createResp.Data.RootID, ParentID: createResp.Data.ParentID}, nil
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
	if !note.Disabled {
		n.messageRegistry().remember(note.SessionID, note.MessageID)
	}
	return WaitingNotificationResult{MessageID: note.MessageID, Updated: true, TipSent: tipSent}, nil
}

func (n *LarkAppNotifier) UpdateWaitingRunning(note WaitingNotification, running bool) error {
	if !n.Available() || note.MessageID == "" {
		return nil
	}
	note.Running = running
	state := n.cardState()
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.recalled[note.MessageID] {
		return nil
	}
	if state.retired[note.MessageID] {
		note.Disabled = true
	}
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
	if !note.Disabled {
		n.messageRegistry().remember(note.SessionID, note.MessageID)
	}
	key := note.SessionID + "\x00" + note.ChatID
	if !note.Disabled && state.latest[key].MessageID == note.MessageID {
		state.latest[key] = note
		n.persistCards(state)
	}
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

func (n *LarkAppNotifier) recallMessage(messageID string) error {
	req := larkim.NewDeleteMessageReqBuilder().MessageId(messageID).Build()
	return retryLarkVoid(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		token := n.tenantTokenSnapshot()
		call := func(token string) (*larkim.DeleteMessageResp, error) {
			if token == "" {
				return n.client.Im.V1.Message.Delete(ctx, req)
			}
			if n.uncachedClient == nil {
				return nil, errors.New("lark uncached client is not configured")
			}
			return n.uncachedClient.Im.V1.Message.Delete(ctx, req, larkcore.WithTenantAccessToken(token))
		}
		resp, err := call(token)
		if err == nil && resp != nil && invalidLarkAccessTokenCode(resp.Code) {
			token, err = n.refreshTenantToken(token)
			if err == nil {
				resp, err = call(token)
			}
		}
		if err != nil {
			return err
		}
		if resp == nil {
			return errors.New("empty lark recall message response")
		}
		if !resp.Success() && resp.Code != 230011 { // Already recalled: safe after a lost response/restart.
			return fmt.Errorf("lark recall message API returned code %d: %s", resp.Code, resp.Msg)
		}
		return nil
	})
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
