package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/elevenlj/iris/internal/session"
)

type RuntimeConfig struct {
	LarkAppID                       string                                `json:"lark_app_id"`
	LarkAppSecret                   string                                `json:"lark_app_secret"`
	LarkNotifyReceiveID             string                                `json:"lark_notify_receive_id"`
	LarkMentionEnabled              bool                                  `json:"lark_mention_enabled"`
	LarkDefaultSessionName          string                                `json:"lark_default_session_name"`
	LarkSessionChatPrefix           string                                `json:"lark_session_chat_prefix"`
	LarkIgnoreMessagePrefix         string                                `json:"lark_ignore_message_prefix"`
	LarkAutoSummaryPrompt           string                                `json:"lark_auto_summary_prompt"`
	FastWaitingTransitionMs         int                                   `json:"fast_waiting_transition_ms"`
	ConservativeWaitingTransitionMs int                                   `json:"conservative_waiting_transition_ms"`
	LarkAutoRefreshIntervalMs       int                                   `json:"lark_auto_refresh_interval_ms"`
	HeadlessSnapshotTimeoutMs       int                                   `json:"headless_snapshot_timeout_ms"`
	LarkNotifyMaxLines              int                                   `json:"lark_notify_max_lines"`
	LarkNotifyFallbackTailLines     int                                   `json:"lark_notify_fallback_tail_lines"`
	LarkNotifyMergeWrappedLines     bool                                  `json:"lark_notify_merge_wrapped_lines"`
	LarkNotifyDropLineRules         session.LarkNotifyDropLineRules       `json:"lark_notify_drop_line_patterns"`
	SessionPreStartCommand          string                                `json:"session_pre_start_command"`
	SessionStartPresets             map[string]session.SessionStartPreset `json:"session_start_presets"`
	SessionNamePresets              map[string]session.SessionStartPreset `json:"session_name_presets"`
	LarkCustomShortcuts             []session.LarkCustomShortcut          `json:"lark_custom_shortcuts"`
	OnboardingCompleted             bool                                  `json:"onboarding_completed"`
	AgentKind                       string                                `json:"agent_kind"`
	AgentCommand                    string                                `json:"agent_command"`
	WorkspaceOptions                []session.WorkspaceOption             `json:"workspace_options"`
}

type SettingsSecurity struct {
	PasswordHash string `json:"-"`
	Skipped      bool   `json:"skipped"`
	AuthVersion  int64  `json:"auth_version"`
}

type ConfigService interface {
	RuntimeConfig() RuntimeConfig
	UpdateRuntimeConfig(RuntimeConfig) (RuntimeConfig, error)
}

type SecurityConfigService interface {
	SettingsSecurity() SettingsSecurity
	UpdateSettingsSecurity(SettingsSecurity) error
}

type LarkConfigTestStep struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

type LarkConfigTestResult struct {
	OK    bool                 `json:"ok"`
	Steps []LarkConfigTestStep `json:"steps"`
}

type LarkConfigTester interface {
	Test(RuntimeConfig) LarkConfigTestResult
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if s.config == nil {
		http.NotFound(w, r)
		return
	}
	if !s.requireSettingsAuth(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.config.RuntimeConfig(), nil)
	case http.MethodPatch:
		var req RuntimeConfig
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		cfg, err := s.config.UpdateRuntimeConfig(req)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, cfg, nil)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLarkConfigTest(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettingsAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req RuntimeConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.larkConfigTester.Test(req), nil)
}

type realLarkConfigTester struct {
	probe larkGroupPermissionProbe
}

func (t realLarkConfigTester) Test(cfg RuntimeConfig) LarkConfigTestResult {
	result := LarkConfigTestResult{}
	appID := strings.TrimSpace(cfg.LarkAppID)
	appSecret := strings.TrimSpace(cfg.LarkAppSecret)
	receiveID := strings.TrimSpace(cfg.LarkNotifyReceiveID)
	missing := []string{}
	if appID == "" {
		missing = append(missing, "App ID")
	}
	if appSecret == "" {
		missing = append(missing, "App Secret")
	}
	if receiveID == "" {
		missing = append(missing, "通知接收 ID")
	}
	if len(missing) > 0 {
		result.Steps = append(result.Steps, LarkConfigTestStep{
			Name:    "配置完整性",
			OK:      false,
			Message: "缺少：" + strings.Join(missing, "、"),
		})
		return result
	}
	result.Steps = append(result.Steps, LarkConfigTestStep{Name: "配置完整性", OK: true, Message: "必填项已填写"})

	notifier := session.NewLarkAppNotifier(appID, appSecret, receiveID, cfg.LarkMentionEnabled)
	_, err := notifier.NotifyWaiting(session.WaitingNotification{
		SessionID: "config-test",
		Name:      "Iris 测试通知",
		Content:   "这是一条配置测试消息，用于确认飞书 App 凭证和通知接收 ID 可以正常发送。\n\n时间：" + time.Now().Format("2006-01-02 15:04:05"),
	})
	if err != nil {
		result.Steps = append(result.Steps, LarkConfigTestStep{
			Name:    "发送测试通知",
			OK:      false,
			Message: err.Error(),
		})
		return result
	}
	result.Steps = append(result.Steps, LarkConfigTestStep{Name: "发送测试通知", OK: true, Message: "已向通知接收 ID 发送测试卡片"})
	probeChatID := ""
	if t.probe != nil {
		probeChatID = t.probe.LatestLarkChatID()
	}
	if err := checkLarkGroupAllMessagesPermission(appID, appSecret, receiveID, probeChatID); err != nil {
		result.Steps = append(result.Steps, LarkConfigTestStep{
			Name:    "群所有消息权限",
			OK:      false,
			Message: err.Error(),
		})
		return result
	}
	result.Steps = append(result.Steps, LarkConfigTestStep{Name: "群所有消息权限", OK: true, Message: "im:message.group_msg 已生效"})
	result.OK = true
	return result
}

type larkGroupPermissionProbe interface {
	LatestLarkChatID() string
}

func checkLarkGroupAllMessagesPermission(appID, appSecret string, receiveID string, probeChatID string) error {
	probeChatID = strings.TrimSpace(probeChatID)
	payload, _ := json.Marshal(map[string]string{"app_id": appID, "app_secret": appSecret})
	req, err := http.NewRequest(http.MethodPost, feishuOpenBase+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var tokenResp struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&tokenResp); err != nil {
		return err
	}
	if tokenResp.Code != 0 || tokenResp.TenantAccessToken == "" {
		return errors.New("获取飞书访问令牌失败：" + tokenResp.Msg)
	}
	if probeChatID == "" {
		var err error
		probeChatID, err = createLarkPermissionProbeChat(client, tokenResp.TenantAccessToken, receiveID)
		if err != nil {
			return err
		}
	}
	u, _ := url.Parse(feishuOpenBase + "/open-apis/im/v1/messages")
	q := u.Query()
	q.Set("container_id_type", "chat")
	q.Set("container_id", probeChatID)
	q.Set("page_size", "1")
	u.RawQuery = q.Encode()
	req, err = http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tokenResp.TenantAccessToken)
	resp, err = client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var msgResp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&msgResp); err != nil {
		return err
	}
	if msgResp.Code == 230027 && strings.Contains(msgResp.Msg, "im:message.group_msg") {
		return errors.New("当前飞书应用缺少 im:message.group_msg，请在飞书后台开通“获取群组中所有消息（敏感权限）”并发布/审批后重新保存配置")
	}
	if msgResp.Code != 0 {
		return errors.New("检测 im:message.group_msg 失败：" + msgResp.Msg)
	}
	return nil
}

func createLarkPermissionProbeChat(client *http.Client, token string, receiveID string) (string, error) {
	receiveID = strings.TrimSpace(receiveID)
	if receiveID == "" {
		return "", errors.New("无法创建权限测试群：通知接收 ID 为空")
	}
	u, _ := url.Parse(feishuOpenBase + "/open-apis/im/v1/chats")
	q := u.Query()
	q.Set("user_id_type", "open_id")
	q.Set("uuid", "iris-permission-probe-"+time.Now().Format("20060102150405"))
	u.RawQuery = q.Encode()
	body, _ := json.Marshal(map[string]any{
		"name":                     "Iris 权限测试",
		"user_id_list":             []string{receiveID},
		"chat_mode":                "group",
		"chat_type":                "private",
		"join_message_visibility":  "not_anyone",
		"leave_message_visibility": "not_anyone",
		"membership_approval":      "no_approval_required",
	})
	req, err := http.NewRequest(http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var createResp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			ChatID string `json:"chat_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&createResp); err != nil {
		return "", err
	}
	if createResp.Code != 0 || strings.TrimSpace(createResp.Data.ChatID) == "" {
		return "", errors.New("创建权限测试群失败：" + createResp.Msg)
	}
	return createResp.Data.ChatID, nil
}

func (s *Server) handleLarkAppRegistration(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettingsAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Brand string `json:"brand"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.larkAppRegistration.Begin(r.Context(), req.Brand)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result, nil)
}

func (s *Server) handleLarkAppRegistrationPoll(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettingsAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Brand      string `json:"brand"`
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.larkAppRegistration.Poll(r.Context(), req.Brand, req.DeviceCode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, result, nil)
}

func (s *Server) handleLarkAppRegistrationQR(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettingsAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	text := r.URL.Query().Get("text")
	if strings.TrimSpace(text) == "" {
		writeError(w, http.StatusBadRequest, errors.New("text is required"))
		return
	}
	png, err := qrPNG(text)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}
