package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elevenlj/iris/internal/session"
)

func TestConfigEndpointGetAndPatch(t *testing.T) {
	svc := &fakeConfigService{cfg: RuntimeConfig{
		LarkAppID:                       "app",
		LarkAppSecret:                   "secret",
		LarkNotifyReceiveID:             "ou_1",
		LarkMentionEnabled:              true,
		LarkDefaultSessionName:          "默认会话",
		OnboardingCompleted:             false,
		LarkSessionChatPrefix:           "ET · ",
		LarkIgnoreMessagePrefix:         "/i",
		LarkAutoSummaryPrompt:           "总结上一轮输出",
		FastWaitingTransitionMs:         300,
		ConservativeWaitingTransitionMs: 700,
		LarkAutoRefreshIntervalMs:       5000,
		HeadlessSnapshotTimeoutMs:       10000,
		LarkNotifyMaxLines:              300,
		LarkNotifyFallbackTailLines:     100,
		LarkNotifyMergeWrappedLines:     false,
		LarkCustomShortcuts:             []session.LarkCustomShortcut{{Label: "状态", Command: "git status"}},
		SessionStartPresets:             map[string]session.SessionStartPreset{},
		SessionNamePresets:              map[string]session.SessionStartPreset{},
	}}
	srv := NewServer(nil, "", svc)

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d", rec.Code)
	}

	body := `{"lark_app_id":"new-app","lark_app_secret":"new-secret","lark_notify_receive_id":"ou_new","lark_mention_enabled":false,"lark_default_session_name":"默认","lark_session_chat_prefix":"DEV ·","lark_ignore_message_prefix":"/silent","lark_auto_summary_prompt":"总结上一轮输出","fast_waiting_transition_ms":450,"conservative_waiting_transition_ms":900,"lark_auto_refresh_interval_ms":6000,"headless_snapshot_timeout_ms":15000,"lark_notify_max_lines":120,"lark_notify_fallback_tail_lines":80,"lark_notify_merge_wrapped_lines":true,"lark_notify_drop_line_patterns":["noise"],"lark_custom_shortcuts":[{"label":"状态","command":"git status"}],"onboarding_completed":true,"session_pre_start_command":"source ~/.zshrc","session_start_presets":{"1":{"commands":["codex"]}},"session_name_presets":{"会话 A":{"commands":["pwd"]}}}`
	req = httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(body))
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got RuntimeConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.FastWaitingTransitionMs != 450 || got.LarkAutoRefreshIntervalMs != 6000 || got.LarkAppID != "new-app" {
		t.Fatalf("unexpected config response: %#v", got)
	}
	if got.LarkNotifyFallbackTailLines != 80 {
		t.Fatalf("fallback tail lines = %d", got.LarkNotifyFallbackTailLines)
	}
	if got.HeadlessSnapshotTimeoutMs != 15000 {
		t.Fatalf("headless snapshot timeout = %d", got.HeadlessSnapshotTimeoutMs)
	}
	if got.LarkSessionChatPrefix != "DEV ·" {
		t.Fatalf("chat prefix = %q", got.LarkSessionChatPrefix)
	}
	if got.LarkIgnoreMessagePrefix != "/silent" {
		t.Fatalf("ignore prefix = %q", got.LarkIgnoreMessagePrefix)
	}
	if got.LarkAutoSummaryPrompt != "总结上一轮输出" {
		t.Fatalf("auto summary prompt = %q", got.LarkAutoSummaryPrompt)
	}
	if !got.LarkNotifyMergeWrappedLines {
		t.Fatalf("merge wrapped lines = %#v", got)
	}
	if len(got.LarkCustomShortcuts) != 1 || got.LarkCustomShortcuts[0].Label != "状态" {
		t.Fatalf("custom shortcuts = %#v", got.LarkCustomShortcuts)
	}
	if !got.OnboardingCompleted {
		t.Fatalf("onboarding completion = %#v", got)
	}
	if svc.cfg.SessionPreStartCommand != "source ~/.zshrc" || svc.cfg.SessionStartPresets["1"].Commands[0] != "codex" {
		t.Fatalf("service was not updated: %#v", svc.cfg)
	}
}

func TestLarkAppRegistrationEndpoints(t *testing.T) {
	srv := NewServer(nil, "", nil)
	srv.larkAppRegistration = &fakeLarkAppRegistration{}

	req := httptest.NewRequest(http.MethodPost, "/api/lark-app-registration", strings.NewReader(`{"brand":"feishu"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("begin status = %d body=%s", rec.Code, rec.Body.String())
	}
	var begin LarkAppRegistrationBegin
	if err := json.Unmarshal(rec.Body.Bytes(), &begin); err != nil {
		t.Fatal(err)
	}
	if begin.DeviceCode != "dev-1" || begin.UserCode != "USER-1" || begin.VerificationURIComplete == "" {
		t.Fatalf("unexpected begin response: %#v", begin)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/lark-app-registration/poll", strings.NewReader(`{"brand":"feishu","device_code":"dev-1"}`))
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("poll status = %d body=%s", rec.Code, rec.Body.String())
	}
	var poll LarkAppRegistrationResult
	if err := json.Unmarshal(rec.Body.Bytes(), &poll); err != nil {
		t.Fatal(err)
	}
	if poll.AppID != "cli_test" || poll.AppSecret != "secret" || poll.NotifyReceiveID != "ou_test" {
		t.Fatalf("unexpected poll response: %#v", poll)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/lark-app-registration/qr?text=https%3A%2F%2Fopen.feishu.cn", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" || rec.Body.Len() == 0 {
		t.Fatalf("qr response code=%d content-type=%q len=%d", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}
}

func TestLarkConfigTestEndpoint(t *testing.T) {
	srv := NewServer(nil, "", nil)
	srv.larkConfigTester = fakeLarkConfigTester{}

	req := httptest.NewRequest(http.MethodPost, "/api/config/lark-test", strings.NewReader(`{"lark_app_id":"app","lark_app_secret":"secret","lark_notify_receive_id":"ou_1"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var result LarkConfigTestResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || len(result.Steps) != 1 || !result.Steps[0].OK {
		t.Fatalf("unexpected result: %#v", result)
	}
}

type fakeConfigService struct {
	cfg RuntimeConfig
}

func (s *fakeConfigService) RuntimeConfig() RuntimeConfig {
	return s.cfg
}

func (s *fakeConfigService) UpdateRuntimeConfig(cfg RuntimeConfig) (RuntimeConfig, error) {
	s.cfg = cfg
	return s.cfg, nil
}

type fakeLarkAppRegistration struct{}

func (f *fakeLarkAppRegistration) Begin(context.Context, string) (LarkAppRegistrationBegin, error) {
	return LarkAppRegistrationBegin{
		DeviceCode:              "dev-1",
		UserCode:                "USER-1",
		VerificationURIComplete: "https://open.feishu.cn/page/cli?user_code=USER-1",
		ExpiresIn:               3600,
		Interval:                5,
		Brand:                   "feishu",
	}, nil
}

func (f *fakeLarkAppRegistration) Poll(context.Context, string, string) (LarkAppRegistrationResult, error) {
	return LarkAppRegistrationResult{
		AppID:           "cli_test",
		AppSecret:       "secret",
		Brand:           "feishu",
		OpenID:          "ou_test",
		NotifyReceiveID: "ou_test",
	}, nil
}

type fakeLarkConfigTester struct{}

func (fakeLarkConfigTester) Test(RuntimeConfig) LarkConfigTestResult {
	return LarkConfigTestResult{
		OK: true,
		Steps: []LarkConfigTestStep{{
			Name:    "发送测试通知",
			OK:      true,
			Message: "已发送",
		}},
	}
}
