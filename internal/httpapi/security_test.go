package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elevenlj/iris/internal/session"
	"github.com/gorilla/websocket"
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
	if status := requestStatus(t, clientA, http.MethodPost, server.URL+"/api/settings/security/setup", map[string]any{"password": "short", "confirm_password": "short"}); status != http.StatusBadRequest {
		t.Fatalf("short password status = %d", status)
	}
	if status := requestStatus(t, clientA, http.MethodPost, server.URL+"/api/settings/security/setup", map[string]any{"password": "strong-pass-1", "confirm_password": "strong-pass-1"}); status != http.StatusOK {
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
	if status := requestStatus(t, clientA, http.MethodPost, server.URL+"/api/settings/security/password", map[string]any{"current_password": "strong-pass-1", "new_password": "strong-pass-2", "confirm_password": "strong-pass-2"}); status != http.StatusOK {
		t.Fatalf("password update status = %d", status)
	}
	if status := requestStatus(t, clientB, http.MethodGet, server.URL+"/api/config", nil); status != http.StatusUnauthorized {
		t.Fatalf("old authenticated session should be invalidated, got %d", status)
	}
	if status := requestStatus(t, clientA, http.MethodGet, server.URL+"/api/config", nil); status != http.StatusOK {
		t.Fatalf("password-changing session should receive a fresh login, got %d", status)
	}
}

func TestSettingsPasswordChangeRequiresConfirmation(t *testing.T) {
	hash, err := hashSettingsPassword("strong-pass-1")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &secureTestConfig{runtime: RuntimeConfig{AgentKind: "codex", AgentCommand: "codex"}, security: SettingsSecurity{PasswordHash: hash, AuthVersion: 2}}
	server := httptest.NewServer(NewServer(nil, t.TempDir(), cfg).Handler())
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	if status := requestStatus(t, client, http.MethodPost, server.URL+"/api/settings/security/login", map[string]any{"password": "strong-pass-1"}); status != http.StatusOK {
		t.Fatalf("login status = %d", status)
	}
	status := requestStatus(t, client, http.MethodPost, server.URL+"/api/settings/security/password", map[string]any{
		"current_password": "strong-pass-1", "new_password": "strong-pass-2", "confirm_password": "strong-pass-3",
	})
	if status != http.StatusBadRequest || !verifySettingsPassword("strong-pass-1", cfg.security.PasswordHash) {
		t.Fatalf("mismatched confirmation changed password: status=%d security=%#v", status, cfg.security)
	}
}

func TestSettingsPasswordCannotBeSkipped(t *testing.T) {
	cfg := &secureTestConfig{}
	server := httptest.NewServer(NewServer(nil, t.TempDir(), cfg).Handler())
	defer server.Close()
	client := server.Client()
	if status := requestStatus(t, client, http.MethodPost, server.URL+"/api/settings/security/setup", map[string]any{"skip": true, "confirm_risk": true}); status != http.StatusBadRequest {
		t.Fatalf("skip status = %d", status)
	}
	if cfg.security.Skipped || cfg.security.PasswordHash != "" {
		t.Fatalf("skip request changed security state: %#v", cfg.security)
	}
	if status := requestStatus(t, client, http.MethodGet, server.URL+"/api/config", nil); status != http.StatusUnauthorized {
		t.Fatalf("unconfigured settings should remain protected, got %d", status)
	}
}

func TestSettingsPasswordSetupRequiresConfirmation(t *testing.T) {
	cfg := &secureTestConfig{}
	server := httptest.NewServer(NewServer(nil, t.TempDir(), cfg).Handler())
	defer server.Close()
	status := requestStatus(t, server.Client(), http.MethodPost, server.URL+"/api/settings/security/setup", map[string]any{
		"password": "strong-pass-1", "confirm_password": "strong-pass-2",
	})
	if status != http.StatusBadRequest || cfg.security.PasswordHash != "" {
		t.Fatalf("mismatched confirmation status=%d security=%#v", status, cfg.security)
	}
}

func TestSettingsSessionCookieLastsThirtyDaysAndSurvivesRestart(t *testing.T) {
	cfg := &secureTestConfig{runtime: RuntimeConfig{AgentKind: "codex", AgentCommand: "codex"}}
	serverA := httptest.NewServer(NewServer(nil, t.TempDir(), cfg).Handler())
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	body, _ := json.Marshal(map[string]any{"password": "strong-pass-1", "confirm_password": "strong-pass-1"})
	req, _ := http.NewRequest(http.MethodPost, serverA.URL+"/api/settings/security/setup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("setup status = %d", resp.StatusCode)
	}
	cookies := resp.Cookies()
	resp.Body.Close()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].MaxAge != 30*24*60*60 {
		t.Fatalf("unexpected settings cookie: %#v", cookies)
	}
	if strings.Contains(cookies[0].Value, "strong-pass-1") {
		t.Fatal("settings cookie must never contain the password")
	}
	serverA.Close()

	serverB := httptest.NewServer(NewServer(nil, t.TempDir(), cfg).Handler())
	defer serverB.Close()
	if status := requestStatus(t, client, http.MethodGet, serverB.URL+"/api/config", nil); status != http.StatusOK {
		t.Fatalf("thirty-day cookie should survive server restart, got %d", status)
	}
}

func TestSignedSettingsSessionRejectsExpiryTamperingAndPasswordChanges(t *testing.T) {
	now := time.Now().UTC()
	security := SettingsSecurity{PasswordHash: "stored-password-hash", AuthVersion: 3}
	token := signSettingsSession(security, now.Add(time.Hour), "nonce")
	if !validSettingsSession(token, security, now) {
		t.Fatal("fresh signed settings session should be valid")
	}
	if validSettingsSession(token, security, now.Add(2*time.Hour)) {
		t.Fatal("expired settings session should be invalid")
	}
	if validSettingsSession(token+"tampered", security, now) {
		t.Fatal("tampered settings session should be invalid")
	}
	changed := security
	changed.PasswordHash = "new-password-hash"
	changed.AuthVersion++
	if validSettingsSession(token, changed, now) {
		t.Fatal("password changes should invalidate old settings sessions")
	}
}

func TestSettingsPageRoutesToStandaloneSetupAndLogin(t *testing.T) {
	cfg := &secureTestConfig{}
	server := httptest.NewServer(NewServer(nil, t.TempDir(), cfg).Handler())
	defer server.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/setup-password?") {
		t.Fatalf("first visit = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	hash, err := hashSettingsPassword("strong-pass-1")
	if err != nil {
		t.Fatal(err)
	}
	cfg.security = SettingsSecurity{PasswordHash: hash, AuthVersion: 2}
	resp, err = client.Get(server.URL + "/?settings=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login?") {
		t.Fatalf("returning settings visit = %d %q", resp.StatusCode, resp.Header.Get("Location"))
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

func TestDashboardAuthenticationCoversAllRoutes(t *testing.T) {
	cfg := &secureTestConfig{security: SettingsSecurity{PasswordHash: "configured", AuthVersion: 1}}
	s := NewServer(nil, "", cfg)
	// A sentinel handler verifies the authentication boundary without starting terminals.
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	cookie := &http.Cookie{Name: settingsCookieName, Value: signSettingsSession(cfg.security, time.Now().Add(time.Hour), "test")}
	for _, target := range []string{"/", "/index.html", "/?session=sess-1&headless=1", "/app.js", "/api/bots", "/api/sessions", "/api/sessions/sess-1/output", "/api/sessions/sess-1/ws?headless=1", "/api/sessions/sess-1/uploads", "/api/quick-commands", "/bots/default/api/sessions", "/bots/bot-test/api/sessions/sess-1/ws", "/api/sessions/sess-1/hook/../output"} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
			req := httptest.NewRequest(method, target, nil)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusSeeOther {
				t.Fatalf("anonymous %s %s bypassed auth: %d", method, target, rec.Code)
			}
			req.AddCookie(cookie)
			rec = httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent && rec.Code != http.StatusMovedPermanently {
				t.Fatalf("authenticated %s %s blocked: %d", method, target, rec.Code)
			}
		}
	}
	for _, target := range []string{"/login", "/auth.js", "/api/settings/security/login", "/api/runtime", "/api/sessions/sess-1/hook/turn-ended", "/bots/bot-test/api/sessions/sess-1/lark/context"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("independently authenticated endpoint blocked: %s", target)
		}
	}
	for _, remote := range []string{"127.0.0.1:1234", "192.0.2.1:1234"} {
		for _, token := range []string{s.HeadlessToken(), "invalid"} {
			req := httptest.NewRequest(http.MethodGet, "/?session=sess-1&headless_token="+token, nil)
			req.RemoteAddr = remote
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			cookies := rec.Result().Cookies()
			want := remote == "127.0.0.1:1234" && token == s.HeadlessToken()
			if (len(cookies) > 0) != want {
				t.Fatal("headless bootstrap must require loopback AND secret token")
			}
			if want && (!validSettingsSession(cookies[0].Value, cfg.security, time.Now()) || strings.Contains(rec.Header().Get("Location"), "headless_token")) {
				t.Fatal("invalid bootstrap cookie or leaked token in redirect")
			}
		}
	}
}

func TestTerminalWebSocketRequiresLoginAndSameOrigin(t *testing.T) {
	m := session.NewManager(nil, wsBridgeTestLauncher{terminal: newWSBridgeTestTerminal()})
	sess, err := m.CreateSession(context.Background(), "auth-test")
	if err != nil {
		t.Fatal(err)
	}
	rt, _ := m.GetRuntime(sess.ID)
	defer rt.Close()
	cfg := &secureTestConfig{security: SettingsSecurity{PasswordHash: "configured", AuthVersion: 1}}
	s := NewServer(m, "", cfg)
	host := httptest.NewServer(s.Handler())
	defer host.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(host.URL + "/?headless_token=" + s.HeadlessToken())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(resp.Cookies()) != 1 {
		t.Fatal("headless browser did not get a login cookie")
	}
	for _, tc := range []struct {
		cookie bool
		origin string
		status int
	}{{false, host.URL, 401}, {true, "https://untrusted.example", 403}, {true, host.URL, 101}} {
		headers := http.Header{"Origin": {tc.origin}}
		if tc.cookie {
			headers.Set("Cookie", resp.Cookies()[0].String())
		}
		conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(host.URL, "http")+"/api/sessions/"+sess.ID+"/ws?headless=1", headers)
		if conn != nil {
			conn.Close()
		}
		if response == nil || response.StatusCode != tc.status {
			t.Fatalf("websocket cookie=%v origin=%s: response=%v err=%v", tc.cookie, tc.origin, response, err)
		}
		response.Body.Close()
	}
}
