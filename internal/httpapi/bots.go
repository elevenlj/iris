package httpapi

import (
	"context"
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
	CreateBot(context.Context, BotConfig) (BotConfig, error)
	ListBots() []BotConfig
	SaveBot(context.Context, BotConfig) (BotConfig, error)
	BotHandler(string) http.Handler
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
	var bot BotConfig
	if err := decodeLimitedJSON(w, r, &bot); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	result, err := s.bots.CreateBot(r.Context(), bot)
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
		bots := s.bots.ListBots()
		// The session list may be viewed without opening authenticated settings.
		if !s.settingsAuthenticated(r, s.settingsSecurity()) {
			for i := range bots {
				bots[i].AppSecret = ""
				bots[i].ReceiveID = ""
			}
		}
		writeJSON(w, http.StatusOK, bots, nil)
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
