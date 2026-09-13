package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

type BotConfig struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	AppName             string `json:"app_name"`
	AppID               string `json:"app_id"`
	AppSecret           string `json:"app_secret,omitempty"`
	ReceiveID           string `json:"receive_id,omitempty"`
	DefaultAgentID      string `json:"default_agent_id"`
	DefaultWorkspaceDir string `json:"default_workspace_dir"`
}

type BotService interface {
	ListFeishuApps(context.Context) ([]FeishuApp, error)
	CreateBot(context.Context, BotConfig) (BotConfig, error)
	ListBots() []BotConfig
	SaveBot(context.Context, BotConfig) (BotConfig, error)
	DeleteBot(context.Context, string) (BotDeletion, error)
	PreviewBotDeletion(context.Context, string) (BotDeletionPreview, error)
	BotHandler(string) http.Handler
}

type BotDeletion struct {
	BackupPath string `json:"backup_path"`
	Warning    string `json:"warning,omitempty"`
}

type BotDeletionPreview struct {
	Sessions int `json:"sessions"`
	Running  int `json:"running"`
}

type FeishuApp struct {
	AppID string `json:"app_id"`
	Name  string `json:"name"`
}

type BotCreationProgress struct {
	Stage   string      `json:"stage"`
	Message string      `json:"message,omitempty"`
	AppID   string      `json:"app_id,omitempty"`
	BotID   string      `json:"bot_id,omitempty"`
	Error   string      `json:"error,omitempty"`
	Apps    []FeishuApp `json:"apps,omitempty"`
}

type botProgressKey struct{}

func ReportBotCreationProgress(ctx context.Context, stage, message, appID string) {
	if report, ok := ctx.Value(botProgressKey{}).(func(BotCreationProgress)); ok {
		report(BotCreationProgress{Stage: stage, Message: message, AppID: appID})
	}
}

func (s *Server) handleBotCreate(w http.ResponseWriter, r *http.Request) {
	if s.bots == nil {
		http.NotFound(w, r)
		return
	}
	if !s.requireSettingsAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		BotConfig
		ListApps bool `json:"list_apps"`
	}
	if err := decodeLimitedJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx := r.Context()
	if r.Header.Get("Accept") == "application/x-ndjson" {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		report := func(event BotCreationProgress) {
			_ = json.NewEncoder(w).Encode(event)
			_ = http.NewResponseController(w).Flush()
		}
		ctx = context.WithValue(ctx, botProgressKey{}, report)
		if request.ListApps {
			apps, err := s.bots.ListFeishuApps(ctx)
			if err != nil {
				report(BotCreationProgress{Stage: "error", Error: err.Error()})
			} else {
				report(BotCreationProgress{Stage: "apps", Apps: apps})
			}
			return
		}
		result, err := s.bots.CreateBot(ctx, request.BotConfig)
		if err != nil {
			report(BotCreationProgress{Stage: "error", Error: err.Error()})
		} else {
			report(BotCreationProgress{Stage: "done", BotID: result.ID})
		}
		return
	}
	if request.ListApps {
		apps, err := s.bots.ListFeishuApps(ctx)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
		} else {
			writeJSON(w, http.StatusOK, apps, nil)
		}
		return
	}
	result, err := s.bots.CreateBot(ctx, request.BotConfig)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, result, nil)
}

func (s *Server) SetBotService(bots BotService) { s.bots = bots }

func (s *Server) handleBots(w http.ResponseWriter, r *http.Request) {
	if s.bots == nil {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if id := r.URL.Query().Get("delete_id"); id != "" {
			if !s.requireSettingsAuth(w, r) {
				return
			}
			info, err := s.bots.PreviewBotDeletion(r.Context(), id)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			writeJSON(w, http.StatusOK, info, nil)
			return
		}
		bots := s.bots.ListBots()
		// The session list may be viewed without opening authenticated settings.
		if !s.settingsAuthenticated(r, s.settingsSecurity()) {
			for i := range bots {
				bots[i].AppSecret = ""
				bots[i].ReceiveID = ""
			}
		}
		writeJSON(w, http.StatusOK, bots, nil)
	case http.MethodDelete:
		if !s.requireSettingsAuth(w, r) {
			return
		}
		var request struct {
			ID string `json:"id"`
		}
		if err := decodeLimitedJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		result, err := s.bots.DeleteBot(r.Context(), request.ID)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, result, nil)
	case http.MethodPost, http.MethodPatch:
		if !s.requireSettingsAuth(w, r) {
			return
		}
		var bot BotConfig
		if err := decodeLimitedJSON(w, r, &bot); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if r.Method == http.MethodPost && bot.ID != "" {
			writeError(w, http.StatusBadRequest, errors.New("新增机器人不能指定 ID"))
			return
		}
		if r.Method == http.MethodPatch && bot.ID == "" {
			writeError(w, http.StatusBadRequest, errors.New("缺少机器人 ID"))
			return
		}
		result, err := s.bots.SaveBot(r.Context(), bot)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, result, nil)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleBotRoute(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/bots/"), "/", 2)
	if s.bots == nil || len(parts) != 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	handler := s.bots.BotHandler(parts[0])
	if handler == nil {
		http.NotFound(w, r)
		return
	}
	http.StripPrefix("/bots/"+parts[0], handler).ServeHTTP(w, r)
}
