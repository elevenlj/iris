package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type dashboardObservingConfig struct {
	secureTestConfig
	observed []string
}

func (c *dashboardObservingConfig) ObserveDashboardURL(raw string) error {
	c.observed = append(c.observed, raw)
	return nil
}

func TestDashboardOriginLearnedOnlyFromAuthenticatedSameOriginBrowser(t *testing.T) {
	for _, tc := range []struct {
		name, host, referer, site, want string
		authenticated                   bool
	}{
		{"TLS reverse proxy", "internal:8080", "https://dev.example.com:9443/bots/other/?session=sess-12", "same-origin", "https://dev.example.com:9443", true},
		{"plain HTTP dev machine", "10.1.2.3:8080", "http://10.1.2.3:8080/", "", "http://10.1.2.3:8080", true},
		{"anonymous", "dev.example.com", "https://dev.example.com/", "same-origin", "", false},
		{"cross site", "dev.example.com", "https://evil.example/", "cross-site", "", true},
		{"spoofed referer without metadata", "dev.example.com", "https://evil.example/", "", "", true},
		{"headless", "dev.example.com", "https://dev.example.com/?headless=1", "same-origin", "", true},
		{"no browser referer", "dev.example.com", "", "same-origin", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &dashboardObservingConfig{secureTestConfig: secureTestConfig{security: SettingsSecurity{PasswordHash: "test-hash", AuthVersion: 1}}}
			srv := NewServer(nil, "", cfg)
			req := httptest.NewRequest(http.MethodGet, "/api/settings/security/status", nil)
			req.Host = tc.host
			req.Header.Set("Referer", tc.referer)
			req.Header.Set("Sec-Fetch-Site", tc.site)
			req.Header.Set("X-Forwarded-Host", "evil.example")
			if tc.authenticated {
				req.AddCookie(&http.Cookie{Name: settingsCookieName, Value: signSettingsSession(cfg.security, time.Now().Add(time.Hour), "test")})
			}
			srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
			if tc.want == "" {
				if len(cfg.observed) != 0 {
					t.Fatalf("untrusted request changed dashboard address: %#v", cfg.observed)
				}
			} else if len(cfg.observed) != 1 || cfg.observed[0] != tc.want {
				t.Fatalf("wrong browser origin: %#v, want %q", cfg.observed, tc.want)
			}
		})
	}
}
