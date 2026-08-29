package session

import (
	"encoding/json"
	"strings"
	"time"
)

const (
	StatusRunning = "running"
	StatusWaiting = "waiting"
	StatusExited  = "exited"
	StatusFailed  = "failed"
)

const (
	SessionModeShell = "shell"
	SessionModeAgent = "agent"
)

type Session struct {
	ID                     string    `json:"id"`
	Name                   string    `json:"name"`
	Status                 string    `json:"status"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
	ExitCode               *int      `json:"exit_code"`
	Live                   bool      `json:"live"`
	NotifyOnWaiting        bool      `json:"notify_on_waiting"`
	PeerSessionID          string    `json:"peer_session_id,omitempty"`
	BridgeEnabled          bool      `json:"bridge_enabled,omitempty"`
	LarkChatID             string    `json:"lark_chat_id,omitempty"`
	LarkMentionModeEnabled bool      `json:"lark_mention_mode_enabled,omitempty"`
	DeveloperModeEnabled   bool      `json:"developer_mode_enabled,omitempty"`
	HistorySize            int64     `json:"history_size,omitempty"`
	RecoveryKey            string    `json:"recovery_key,omitempty"`
	LastMode               string    `json:"last_mode,omitempty"`
	LastCWD                string    `json:"last_cwd,omitempty"`
	LastPrevCWD            string    `json:"last_prev_cwd,omitempty"`
	LastAgentID            string    `json:"last_agent_id,omitempty"`
	LastAgentKind          string    `json:"last_agent_kind,omitempty"`
	LastAgentStartCommand  string    `json:"last_agent_start_command,omitempty"`
	LastAgentResumeCommand string    `json:"last_agent_resume_command,omitempty"`
	LastAgentHome          string    `json:"last_agent_home,omitempty"`
	NotificationsAvailable bool      `json:"notifications_available"`
}

type AgentConfig struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Command string `json:"command"`
}

type WorkspaceOption struct {
	Label   string `json:"label"`
	Value   string `json:"value"`
	Default bool   `json:"default,omitempty"`
}

type LarkContactBinding struct {
	SenderOpenID string    `json:"sender_open_id"`
	DisplayName  string    `json:"display_name"`
	ChatID       string    `json:"chat_id"`
	SessionID    string    `json:"session_id"`
	Active       bool      `json:"active"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type QuickCommand struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

type WaitingNotification struct {
	SessionID            string
	Name                 string
	Content              string
	MessageID            string
	ChatID               string
	MentionOpenID        string
	UpdateNo             int
	Running              bool
	Disabled             bool
	AutoRefreshEnabled   bool
	AutoSummaryEnabled   bool
	MentionModeEnabled   bool
	DeveloperModeEnabled bool
	WorkspaceOptions     []WorkspaceOption
	AgentOptions         []AgentOption
	AgentID              string
	AgentKind            string
	SuppressUpdateTip    bool
	NotificationVersion  int64
	SnapshotSource       string
	Interaction          *TerminalInteraction
	AgentContext         *TerminalAgentContext
	Startup              bool
	StartupInputEnabled  bool
	StartupComplete      bool
	StartupFailed        bool
}

const DefaultLarkAutoSummaryPrompt = "请用对用户阅读友好的方式，总结上一轮输出内容。要求精简但不要遗漏主要结论、已完成事项、关键文件或下一步动作。不要复述无关日志。"

type LarkCustomShortcut struct {
	Label   string `json:"label"`
	Command string `json:"command"`
}

type LarkNotifyDropLineRule struct {
	Title   string `json:"title"`
	Pattern string `json:"pattern"`
	Kind    string `json:"kind,omitempty"`
	Action  string `json:"action,omitempty"`
	Groups  []int  `json:"groups,omitempty"`
}

type LarkNotifyDropLineRules []LarkNotifyDropLineRule

func (r *LarkNotifyDropLineRules) UnmarshalJSON(data []byte) error {
	var rules []LarkNotifyDropLineRule
	if err := json.Unmarshal(data, &rules); err == nil {
		*r = NormalizeLarkNotifyDropLineRules(rules)
		return nil
	}
	var patterns []string
	if err := json.Unmarshal(data, &patterns); err != nil {
		return err
	}
	rules = make([]LarkNotifyDropLineRule, 0, len(patterns))
	for _, pattern := range patterns {
		rules = append(rules, LarkNotifyDropLineRule{Pattern: pattern})
	}
	*r = NormalizeLarkNotifyDropLineRules(rules)
	return nil
}

func (r LarkNotifyDropLineRules) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.Rules())
}

func (r LarkNotifyDropLineRules) Rules() []LarkNotifyDropLineRule {
	return NormalizeLarkNotifyDropLineRules([]LarkNotifyDropLineRule(r))
}

func NormalizeLarkNotifyDropLineRules(rules []LarkNotifyDropLineRule) []LarkNotifyDropLineRule {
	out := make([]LarkNotifyDropLineRule, 0, len(rules))
	for _, rule := range rules {
		title := strings.TrimSpace(rule.Title)
		pattern := strings.TrimSpace(rule.Pattern)
		kind := normalizeLarkNotifyRuleKind(rule.Kind)
		action := normalizeLarkNotifyRuleAction(kind, rule.Action)
		groups := normalizeLarkNotifyRuleGroups(rule.Groups)
		if title == "" && pattern == "" {
			continue
		}
		out = append(out, LarkNotifyDropLineRule{Title: title, Pattern: pattern, Kind: kind, Action: action, Groups: groups})
	}
	return out
}

func normalizeLarkNotifyRuleKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", "line":
		return ""
	case "block", "block_head", "block-head", "head":
		return "block_head"
	case "line_group", "line-group", "group":
		return "line_group"
	default:
		return strings.ToLower(strings.TrimSpace(kind))
	}
}

func normalizeLarkNotifyRuleAction(kind string, action string) string {
	if kind != "block_head" {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "keep_head", "head_only", "show_head":
		return "keep_head"
	case "", "drop", "drop_block", "hide_block":
		return "drop_block"
	default:
		return strings.ToLower(strings.TrimSpace(action))
	}
}

func normalizeLarkNotifyRuleGroups(groups []int) []int {
	if len(groups) == 0 {
		return nil
	}
	out := make([]int, 0, len(groups))
	seen := map[int]bool{}
	for _, group := range groups {
		if group <= 0 || seen[group] {
			continue
		}
		seen[group] = true
		out = append(out, group)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

type WaitingNotificationResult struct {
	MessageID string
	RootID    string
	ParentID  string
	Updated   bool
	TipSent   bool
}

type WaitingNotifier interface {
	Available() bool
	NotifyWaiting(WaitingNotification) (WaitingNotificationResult, error)
}

type WaitingRunningNotifier interface {
	UpdateWaitingRunning(WaitingNotification, bool) error
}

const (
	RunningNotificationPlaceholder = "正在执行中，请稍等。"
	StartupNotificationPlaceholder = "Agent 正在启动，请稍等。"
	StartupCompletePlaceholder     = "Agent 启动完成。"
	EmptyNotificationPlaceholder   = "当前轮暂无内容"
)
