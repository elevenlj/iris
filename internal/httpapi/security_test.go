package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
)

type secureTestConfig struct {
	runtime  RuntimeConfig
	security SettingsSecurity
}

func (c *secureTestConfig) RuntimeConfig() RuntimeConfig { return c.runtime }
func (c *secureTestConfig) UpdateRuntimeConfig(cfg RuntimeConfig) (RuntimeConfig, error) {
	c.runtime = cfg
	return cfg, nil
}
func (c *secureTestConfig) SettingsSecurity() SettingsSecurity { return c.security }
func (c *secureTestConfig) UpdateSettingsSecurity(security SettingsSecurity) error {
	c.security = security
	return nil
}

func TestSettingsPasswordProtectsConfigAndInvalidatesOldSessions(t *testing.T) {
	cfg := &secureTestConfig{runtime: RuntimeConfig{AgentKind: "codex", AgentCommand: "codex"}}
	server := httptest.NewServer(NewServer(nil, t.TempDir(), cfg).Handler())
	defer server.Close()

	jarA, _ := cookiejar.New(nil)
	clientA := &http.Client{Jar: jarA}
	if status := requestStatus(t, clientA, http.MethodGet, server.URL+"/api/config", nil); status != http.StatusUnauthorized {
		t.Fatalf("uninitialized settings should protect config, got %d", status)
	}
	if status := requestStatus(t, clientA, http.MethodPost, server.URL+"/api/settings/security/setup", map[string]any{"password": "short"}); status != http.StatusBadRequest {
		t.Fatalf("short password status = %d", status)
	}
	if status := requestStatus(t, clientA, http.MethodPost, server.URL+"/api/settings/security/setup", map[string]any{"password": "strong-pass-1"}); status != http.StatusOK {
		t.Fatalf("setup status = %d", status)
	}
	if cfg.security.PasswordHash == "" || cfg.security.PasswordHash == "strong-pass-1" || cfg.security.Skipped {
		t.Fatalf("password must be stored only as a salted hash: %#v", cfg.security)
	}
	if status := requestStatus(t, clientA, http.MethodGet, server.URL+"/api/config", nil); status != http.StatusOK {
		t.Fatalf("authenticated config status = %d", status)
	}

	jarB, _ := cookiejar.New(nil)
	clientB := &http.Client{Jar: jarB}
	if status := requestStatus(t, clientB, http.MethodPost, server.URL+"/api/settings/security/login", map[string]any{"password": "wrong-pass"}); status != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d", status)
	}
	if status := requestStatus(t, clientB, http.MethodPost, server.URL+"/api/settings/security/login", map[string]any{"password": "strong-pass-1"}); status != http.StatusOK {
		t.Fatalf("login status = %d", status)
	}
	if status := requestStatus(t, clientA, http.MethodPost, server.URL+"/api/settings/security/password", map[string]any{"current_password": "strong-pass-1", "new_password": "strong-pass-2"}); status != http.StatusOK {
		t.Fatalf("password update status = %d", status)
	}
	if status := requestStatus(t, clientB, http.MethodGet, server.URL+"/api/config", nil); status != http.StatusUnauthorized {
		t.Fatalf("old authenticated session should be invalidated, got %d", status)
	}
	if status := requestStatus(t, clientA, http.MethodGet, server.URL+"/api/config", nil); status != http.StatusOK {
		t.Fatalf("password-changing session should receive a fresh login, got %d", status)
	}
}

func TestSettingsPasswordSkipRequiresExplicitRiskConfirmation(t *testing.T) {
	cfg := &secureTestConfig{}
	server := httptest.NewServer(NewServer(nil, t.TempDir(), cfg).Handler())
	defer server.Close()
	client := server.Client()
	if status := requestStatus(t, client, http.MethodPost, server.URL+"/api/settings/security/setup", map[string]any{"skip": true}); status != http.StatusBadRequest {
		t.Fatalf("unconfirmed skip status = %d", status)
	}
	if status := requestStatus(t, client, http.MethodPost, server.URL+"/api/settings/security/setup", map[string]any{"skip": true, "confirm_risk": true}); status != http.StatusOK {
		t.Fatalf("confirmed skip status = %d", status)
	}
	if !cfg.security.Skipped || cfg.security.PasswordHash != "" {
		t.Fatalf("unexpected skipped security state: %#v", cfg.security)
	}
	if status := requestStatus(t, client, http.MethodGet, server.URL+"/api/config", nil); status != http.StatusOK {
		t.Fatalf("skipped mode should allow config with warning, got %d", status)
	}
}

func requestStatus(t *testing.T, client *http.Client, method, url string, body any) int {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
