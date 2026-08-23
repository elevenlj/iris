package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

type fakeEnvironmentChecker struct {
	config RuntimeConfig
	result EnvironmentCheckResult
	calls  int
}

func (f *fakeEnvironmentChecker) Check(_ context.Context, cfg RuntimeConfig, _ string) EnvironmentCheckResult {
	f.calls++
	f.config = cfg
	return f.result
}

func TestEnvironmentCheckRequiresSettingsAccess(t *testing.T) {
	cfg := &secureTestConfig{runtime: RuntimeConfig{AgentKind: "codex", AgentCommand: "codex"}}
	srv := NewServer(nil, t.TempDir(), cfg)
	srv.environmentChecker = &fakeEnvironmentChecker{}

	req := httptest.NewRequest(http.MethodPost, "/api/environment-check", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestEnvironmentCheckIsOneShotAndDoesNotPersistSubmittedConfig(t *testing.T) {
	stored := RuntimeConfig{AgentKind: "codex", AgentCommand: "codex"}
	cfg := &secureTestConfig{runtime: stored, security: SettingsSecurity{Skipped: true}}
	checker := &fakeEnvironmentChecker{result: EnvironmentCheckResult{
		OK:      true,
		Checked: "2026-08-23T08:00:00Z",
		Steps:   []EnvironmentCheckStep{{ID: "service", Name: "Iris 服务", Status: "ok", Message: "服务运行正常"}},
	}}
	srv := NewServer(nil, t.TempDir(), cfg)
	srv.environmentChecker = checker

	req := httptest.NewRequest(http.MethodPost, "/api/environment-check", strings.NewReader(`{"agent_kind":"custom","agent_command":"my-agent --serve"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if checker.calls != 1 || checker.config.AgentCommand != "my-agent --serve" {
		t.Fatalf("checker received %#v after %d calls", checker.config, checker.calls)
	}
	if !reflect.DeepEqual(cfg.runtime, stored) {
		t.Fatalf("environment check persisted submitted config: %#v", cfg.runtime)
	}
	var result EnvironmentCheckResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || len(result.Steps) != 1 {
		t.Fatalf("unexpected response: %#v", result)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/environment-check", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestFirstEnvironmentCommand(t *testing.T) {
	tests := map[string]string{
		"codex --yolo":                        "codex",
		"FOO=bar claude --dangerously":        "claude",
		"env FOO=bar /usr/local/bin/my-agent": "/usr/local/bin/my-agent",
		"source ~/.profile && codex":          "",
		"":                                    "",
	}
	for input, want := range tests {
		if got := firstEnvironmentCommand(input); got != want {
			t.Errorf("firstEnvironmentCommand(%q) = %q, want %q", input, got, want)
		}
	}
}
