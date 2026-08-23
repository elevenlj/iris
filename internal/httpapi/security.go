package httpapi

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	settingsCookieName = "iris_settings_session"
	passwordRounds     = 120000
)

type settingsAuth struct {
	mu       sync.Mutex
	sessions map[string]int64
}

func newSettingsAuth() *settingsAuth {
	return &settingsAuth{sessions: make(map[string]int64)}
}

func (s *Server) handleSettingsSecurity(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/settings/security/")
	if path == r.URL.Path {
		path = "status"
	}
	switch path {
	case "status":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		security := s.settingsSecurity()
		onboardingRequired := false
		if s.config != nil {
			onboardingRequired = !s.config.RuntimeConfig().OnboardingCompleted
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"configured":          strings.TrimSpace(security.PasswordHash) != "",
			"skipped":             security.Skipped,
			"authenticated":       s.settingsAuthenticated(r, security),
			"risk_warning":        security.Skipped,
			"onboarding_required": onboardingRequired,
		}, nil)
	case "setup":
		s.handleSettingsSetup(w, r)
	case "login":
		s.handleSettingsLogin(w, r)
	case "logout":
		s.handleSettingsLogout(w, r)
	case "password":
		s.handleSettingsPassword(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleSettingsSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || s.securityConfig == nil {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	current := s.settingsSecurity()
	if current.PasswordHash != "" || current.Skipped {
		writeError(w, http.StatusConflict, errors.New("安全设置已初始化"))
		return
	}
	var req struct {
		Password    string `json:"password"`
		Skip        bool   `json:"skip"`
		ConfirmRisk bool   `json:"confirm_risk"`
	}
	if err := decodeLimitedJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	next := SettingsSecurity{AuthVersion: current.AuthVersion + 1}
	if req.Skip {
		if !req.ConfirmRisk {
			writeError(w, http.StatusBadRequest, errors.New("跳过密码前必须确认风险"))
			return
		}
		next.Skipped = true
	} else {
		if err := validateSettingsPassword(req.Password); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		hash, err := hashSettingsPassword(req.Password)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		next.PasswordHash = hash
	}
	if err := s.securityConfig.UpdateSettingsSecurity(next); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.settingsAuth.invalidateAll()
	if !next.Skipped {
		s.issueSettingsSession(w, r, next.AuthVersion)
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": !next.Skipped, "skipped": next.Skipped, "authenticated": true}, nil)
}

func (s *Server) handleSettingsLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	security := s.settingsSecurity()
	if security.Skipped {
		writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true}, nil)
		return
	}
	if security.PasswordHash == "" {
		writeError(w, http.StatusPreconditionRequired, errors.New("请先完成安全初始化"))
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := decodeLimitedJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !verifySettingsPassword(req.Password, security.PasswordHash) {
		writeError(w, http.StatusUnauthorized, errors.New("密码错误"))
		return
	}
	s.issueSettingsSession(w, r, security.AuthVersion)
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true}, nil)
}

func (s *Server) handleSettingsLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if cookie, err := r.Cookie(settingsCookieName); err == nil {
		s.settingsAuth.remove(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: settingsCookieName, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSettingsPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || s.securityConfig == nil {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	current := s.settingsSecurity()
	if !s.settingsAuthenticated(r, current) {
		writeError(w, http.StatusUnauthorized, errors.New("请先验证设置密码"))
		return
	}
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := decodeLimitedJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if current.PasswordHash != "" && !verifySettingsPassword(req.CurrentPassword, current.PasswordHash) {
		writeError(w, http.StatusUnauthorized, errors.New("当前密码错误"))
		return
	}
	if err := validateSettingsPassword(req.NewPassword); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	hash, err := hashSettingsPassword(req.NewPassword)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	next := SettingsSecurity{PasswordHash: hash, AuthVersion: current.AuthVersion + 1}
	if err := s.securityConfig.UpdateSettingsSecurity(next); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.settingsAuth.invalidateAll()
	s.issueSettingsSession(w, r, next.AuthVersion)
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true}, nil)
}

func (s *Server) settingsSecurity() SettingsSecurity {
	if s.securityConfig == nil {
		return SettingsSecurity{Skipped: true, AuthVersion: 1}
	}
	security := s.securityConfig.SettingsSecurity()
	if security.AuthVersion <= 0 {
		security.AuthVersion = 1
	}
	return security
}

func (s *Server) requireSettingsAuth(w http.ResponseWriter, r *http.Request) bool {
	security := s.settingsSecurity()
	if s.settingsAuthenticated(r, security) {
		if security.Skipped {
			w.Header().Set("X-Iris-Security-Warning", "password-skipped")
		}
		return true
	}
	writeError(w, http.StatusUnauthorized, errors.New("需要设置密码验证"))
	return false
}

func (s *Server) settingsAuthenticated(r *http.Request, security SettingsSecurity) bool {
	if security.Skipped || s.securityConfig == nil {
		return true
	}
	if security.PasswordHash == "" {
		return false
	}
	cookie, err := r.Cookie(settingsCookieName)
	return err == nil && s.settingsAuth.valid(cookie.Value, security.AuthVersion)
}

func (s *Server) issueSettingsSession(w http.ResponseWriter, r *http.Request, version int64) {
	token := randomToken(32)
	s.settingsAuth.add(token, version)
	http.SetCookie(w, &http.Cookie{
		Name: settingsCookieName, Value: token, Path: "/", MaxAge: 12 * 60 * 60,
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode,
	})
}

func (a *settingsAuth) add(token string, version int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions[token] = version
}

func (a *settingsAuth) valid(token string, version int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return token != "" && a.sessions[token] == version
}

func (a *settingsAuth) remove(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, token)
}

func (a *settingsAuth) invalidateAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions = make(map[string]int64)
}

func decodeLimitedJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	return json.NewDecoder(r.Body).Decode(dst)
}

func validateSettingsPassword(password string) error {
	if len([]rune(password)) < 8 {
		return errors.New("密码至少需要 8 个字符")
	}
	if len(password) > 256 {
		return errors.New("密码过长")
	}
	return nil
}

func hashSettingsPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := pbkdf2SHA256([]byte(password), salt, passwordRounds, 32)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", passwordRounds,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

func verifySettingsPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	rounds, err := strconv.Atoi(parts[1])
	if err != nil || rounds < 10000 || rounds > 1000000 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got := pbkdf2SHA256([]byte(password), salt, rounds, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func pbkdf2SHA256(password, salt []byte, rounds, keyLen int) []byte {
	blocks := (keyLen + sha256.Size - 1) / sha256.Size
	out := make([]byte, 0, blocks*sha256.Size)
	for block := 1; block <= blocks; block++ {
		mac := hmac.New(sha256.New, password)
		mac.Write(salt)
		mac.Write([]byte{byte(block >> 24), byte(block >> 16), byte(block >> 8), byte(block)})
		u := mac.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 1; i < rounds; i++ {
			mac = hmac.New(sha256.New, password)
			mac.Write(u)
			u = mac.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func randomToken(size int) string {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
