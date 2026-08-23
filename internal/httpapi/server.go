package httpapi

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/elevenlj/iris/internal/session"
)

//go:embed static/*
var staticFiles embed.FS

type Server struct {
	manager             *session.Manager
	uploadsDir          string
	config              ConfigService
	securityConfig      SecurityConfigService
	settingsAuth        *settingsAuth
	larkAppRegistration interface {
		Begin(context.Context, string) (LarkAppRegistrationBegin, error)
		Poll(context.Context, string, string) (LarkAppRegistrationResult, error)
	}
	larkConfigTester   LarkConfigTester
	environmentChecker EnvironmentChecker
	mux                *http.ServeMux
}

func NewServer(manager *session.Manager, uploadsDir string, config ...ConfigService) *Server {
	s := &Server{
		manager:             manager,
		uploadsDir:          uploadsDir,
		larkAppRegistration: newLarkAppRegistrationClient(),
		larkConfigTester:    realLarkConfigTester{probe: manager},
		environmentChecker:  realEnvironmentChecker{},
		mux:                 http.NewServeMux(),
		settingsAuth:        newSettingsAuth(),
	}
	if len(config) > 0 {
		s.config = config[0]
		if security, ok := config[0].(SecurityConfigService); ok {
			s.securityConfig = security
		}
	}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleStatic)
	s.mux.HandleFunc("/api/sessions", s.handleSessions)
	s.mux.HandleFunc("/api/sessions/", s.handleSessionByID)
	s.mux.HandleFunc("/api/quick-commands", s.handleQuickCommands)
	s.mux.HandleFunc("/api/quick-commands/", s.handleQuickCommandByID)
	s.mux.HandleFunc("/api/config", s.handleConfig)
	s.mux.HandleFunc("/api/config/lark-test", s.handleLarkConfigTest)
	s.mux.HandleFunc("/api/environment-check", s.handleEnvironmentCheck)
	s.mux.HandleFunc("/api/lark-app-registration", s.handleLarkAppRegistration)
	s.mux.HandleFunc("/api/lark-app-registration/poll", s.handleLarkAppRegistrationPoll)
	s.mux.HandleFunc("/api/lark-app-registration/qr", s.handleLarkAppRegistrationQR)
	s.mux.HandleFunc("/api/settings/security/", s.handleSettingsSecurity)
	s.mux.HandleFunc("/api/settings/security", s.handleSettingsSecurity)
}

func (s *Server) handleEnvironmentCheck(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettingsAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.environmentChecker == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("环境检测暂不可用"))
		return
	}
	cfg := RuntimeConfig{}
	if s.config != nil {
		cfg = s.config.RuntimeConfig()
	}
	if r.Body != nil && r.ContentLength != 0 {
		var submitted RuntimeConfig
		if err := decodeLimitedJSON(w, r, &submitted); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		cfg = submitted
	}
	writeJSON(w, http.StatusOK, s.environmentChecker.Check(r.Context(), cfg, s.uploadsDir), nil)
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}
	b, err := staticFiles.ReadFile("static/" + path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Static assets are embedded in the executable and keep stable URLs across
	// upgrades. Do not let a browser reuse an older frontend against a newer
	// server protocol (for example, an app.js that cannot echo snapshot request
	// IDs yet).
	w.Header().Set("Cache-Control", "no-store, max-age=0, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	if ct := mime.TypeByExtension(filepath.Ext(path)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	_, _ = w.Write(b)
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.manager.ListSessions(r.Context())
		writeJSON(w, http.StatusOK, list, err)
	case http.MethodPost:
		if s.config != nil {
			cfg := s.config.RuntimeConfig()
			if strings.TrimSpace(cfg.AgentKind) == "" || strings.TrimSpace(cfg.AgentCommand) == "" {
				writeError(w, http.StatusPreconditionRequired, errors.New("请先在设置中配置 Agent"))
				return
			}
		}
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		sess, err := s.manager.CreateSession(r.Context(), req.Name)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusCreated, sess, nil)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSessionByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodPatch:
			var req struct {
				NotifyOnWaiting *bool `json:"notify_on_waiting"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.NotifyOnWaiting == nil {
				writeError(w, http.StatusBadRequest, errors.New("invalid patch body"))
				return
			}
			sess, ok, err := s.manager.UpdateNotifyOnWaiting(r.Context(), id, *req.NotifyOnWaiting)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			if !ok {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, http.StatusOK, sess, nil)
		case http.MethodDelete:
			_ = os.RemoveAll(filepath.Join(s.uploadsDir, id))
			if err := s.manager.DeleteSession(r.Context(), id); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	switch parts[1] {
	case "hook":
		s.handleAgentHook(w, r, id, parts[2:])
	case "lark":
		s.handleAgentLarkContext(w, r, id, parts[2:])
	case "output":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		out, ok, err := s.manager.Output(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"content": string(out)}, nil)
	case "current-round":
		if os.Getenv("IRIS_E2E_DEBUG") != "1" && os.Getenv("EASY_TERMINAL_E2E_DEBUG") != "1" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		rt, ok := s.manager.GetRuntime(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("debug") == "1" {
			writeJSON(w, http.StatusOK, rt.CurrentRoundDebug(), nil)
			return
		}
		content := rt.CachedCurrentRoundContent()
		if r.URL.Query().Get("fresh") != "0" {
			content = rt.CurrentRoundContent()
		}
		writeJSON(w, http.StatusOK, map[string]string{"content": content}, nil)
	case "uploads":
		s.handleUpload(w, r, id)
	case "ws":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		rt, ok := s.manager.GetRuntime(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		serveWS(w, r, rt)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleAgentLarkContext(w http.ResponseWriter, r *http.Request, sessionID string, parts []string) {
	if r.Method != http.MethodGet || s.manager == nil || len(parts) != 1 {
		http.NotFound(w, r)
		return
	}
	token := agentSessionBearerToken(r)
	switch parts[0] {
	case "context":
		current, ok, err := s.manager.AgentLarkContext(r.Context(), sessionID, token)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			writeAgentLarkContextError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, current, nil)
	case "messages":
		limit := 50
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed <= 0 {
				writeError(w, http.StatusBadRequest, errors.New("limit must be a positive integer"))
				return
			}
			limit = parsed
		}
		page, ok, err := s.manager.AgentLarkMessages(r.Context(), sessionID, token, limit)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			writeAgentLarkContextError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, page, nil)
	default:
		http.NotFound(w, r)
	}
}

func writeAgentLarkContextError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrInvalidAgentContextToken):
		writeError(w, http.StatusUnauthorized, err)
	case errors.Is(err, session.ErrSessionNotBoundToLark):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, session.ErrLarkContextUnavailable):
		writeError(w, http.StatusServiceUnavailable, err)
	default:
		writeError(w, http.StatusBadGateway, err)
	}
}

func agentSessionBearerToken(r *http.Request) string {
	if r == nil {
		return ""
	}
	for _, header := range []string{"X-Iris-Agent-Token", "X-Easy-Terminal-Hook-Token"} {
		if token := strings.TrimSpace(r.Header.Get(header)); token != "" {
			return token
		}
	}
	const bearerPrefix = "Bearer "
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(authorization, bearerPrefix) {
		return strings.TrimSpace(strings.TrimPrefix(authorization, bearerPrefix))
	}
	return ""
}

func (s *Server) handleAgentHook(w http.ResponseWriter, r *http.Request, sessionID string, parts []string) {
	if len(parts) != 1 || parts[0] != "turn-ended" || r.Method != http.MethodPost || s.manager == nil {
		http.NotFound(w, r)
		return
	}
	token := agentSessionBearerToken(r)
	var payload struct {
		SessionID                  string `json:"session_id"`
		ThreadID                   string `json:"thread-id"`
		LastAssistantMessage       string `json:"last_assistant_message"`
		LegacyLastAssistantMessage string `json:"last-assistant-message"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	agentSessionID := strings.TrimSpace(payload.SessionID)
	if agentSessionID == "" {
		agentSessionID = strings.TrimSpace(payload.ThreadID)
	}
	lastAssistantMessage := strings.TrimSpace(payload.LastAssistantMessage)
	if lastAssistantMessage == "" {
		lastAssistantMessage = strings.TrimSpace(payload.LegacyLastAssistantMessage)
	}
	sess, accepted, err := s.manager.CompleteAgentTurn(r.Context(), sessionID, token, agentSessionID, lastAssistantMessage)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err)
		return
	}
	if sess.ID == "" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": accepted, "status": sess.Status}, nil)
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.manager.GetRuntime(id); !ok {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	defer file.Close()
	mimeType := r.FormValue("mime_type")
	if mimeType == "" {
		mimeType = header.Header.Get("Content-Type")
	}
	if !strings.HasPrefix(mimeType, "image/") {
		writeError(w, http.StatusBadRequest, errors.New("only image uploads are allowed"))
		return
	}
	ext := filepath.Ext(header.Filename)
	if ext == "" {
		exts, _ := mime.ExtensionsByType(mimeType)
		if len(exts) > 0 {
			ext = exts[0]
		}
	}
	dir := filepath.Join(s.uploadsDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	path := filepath.Join(dir, time.Now().Format("20060102150405.000000000")+ext)
	dst, err := os.Create(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer dst.Close()
	if _, err := io.Copy(dst, file); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	abs, _ := filepath.Abs(path)
	writeJSON(w, http.StatusCreated, map[string]string{"path": abs}, nil)
}

func (s *Server) handleQuickCommands(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.manager.ListQuickCommands(r.Context())
		writeJSON(w, http.StatusOK, list, err)
	case http.MethodPost:
		var req struct {
			Name string `json:"name"`
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		qc, err := s.manager.CreateQuickCommand(r.Context(), req.Name, req.Text)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusCreated, qc, nil)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleQuickCommandByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/quick-commands/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if err := s.manager.DeleteQuickCommand(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any, err error) {
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
