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
	"time"
)

const (
	settingsCookieName     = "iris_settings_session"
	settingsSessionVersion = "v1"
	settingsSessionMaxAge  = 30 * 24 * time.Hour
	passwordRounds         = 120000
)

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
			"skipped":             false,
			"authenticated":       s.settingsAuthenticated(r, security),
			"risk_warning":        false,
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
	if current.PasswordHash != "" {
		writeError(w, http.StatusConflict, errors.New("安全设置已初始化"))
		return
	}
	var req struct {
		Password        string `json:"password"`
		ConfirmPassword string `json:"confirm_password"`
		Skip            bool   `json:"skip"`
	}
	if err := decodeLimitedJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Skip {
		writeError(w, http.StatusBadRequest, errors.New("Iris 必须设置访问密码"))
		return
	}
	if err := validateSettingsPassword(req.Password); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Password != req.ConfirmPassword {
		writeError(w, http.StatusBadRequest, errors.New("两次输入的密码不一致"))
		return
	}
	hash, err := hashSettingsPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	next := SettingsSecurity{PasswordHash: hash, AuthVersion: current.AuthVersion + 1}
	if err := s.securityConfig.UpdateSettingsSecurity(next); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.issueSettingsSession(w, r, next)
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "skipped": false, "authenticated": true}, nil)
}

func (s *Server) handleSettingsLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	security := s.settingsSecurity()
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
	s.issueSettingsSession(w, r, security)
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true}, nil)
}

func (s *Server) handleSettingsLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: settingsCookieName, Path: "/", MaxAge: -1, Expires: time.Unix(1, 0),
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode,
	})
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
	s.issueSettingsSession(w, r, next)
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true}, nil)
}

func (s *Server) settingsSecurity() SettingsSecurity {
	if s.securityConfig == nil {
		return SettingsSecurity{AuthVersion: 1}
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
		return true
	}
	writeError(w, http.StatusUnauthorized, errors.New("需要设置密码验证"))
	return false
}

func (s *Server) settingsAuthenticated(r *http.Request, security SettingsSecurity) bool {
	if s.securityConfig == nil {
		return true
	}
	if security.PasswordHash == "" {
		return false
	}
	cookie, err := r.Cookie(settingsCookieName)
	return err == nil && validSettingsSession(cookie.Value, security, time.Now())
}

func (s *Server) issueSettingsSession(w http.ResponseWriter, r *http.Request, security SettingsSecurity) {
	expires := time.Now().Add(settingsSessionMaxAge).UTC()
	token := signSettingsSession(security, expires, randomToken(24))
	http.SetCookie(w, &http.Cookie{
		Name: settingsCookieName, Value: token, Path: "/", MaxAge: int(settingsSessionMaxAge.Seconds()), Expires: expires,
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode,
	})
}

func signSettingsSession(security SettingsSecurity, expires time.Time, nonce string) string {
	payload := strings.Join([]string{
		settingsSessionVersion,
		strconv.FormatInt(security.AuthVersion, 10),
		strconv.FormatInt(expires.Unix(), 10),
		nonce,
	}, ".")
	mac := hmac.New(sha256.New, []byte(security.PasswordHash))
	_, _ = mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func validSettingsSession(token string, security SettingsSecurity, now time.Time) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 5 || parts[0] != settingsSessionVersion || strings.TrimSpace(security.PasswordHash) == "" {
		return false
	}
	version, versionErr := strconv.ParseInt(parts[1], 10, 64)
	expires, expiresErr := strconv.ParseInt(parts[2], 10, 64)
	if versionErr != nil || expiresErr != nil || version != security.AuthVersion || now.Unix() >= expires {
		return false
	}
	payload := strings.Join(parts[:4], ".")
	provided, err := base64.RawURLEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(security.PasswordHash))
	_, _ = mac.Write([]byte(payload))
	return hmac.Equal(provided, mac.Sum(nil))
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
