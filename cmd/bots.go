package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/elevenlj/iris/internal/httpapi"
	"github.com/elevenlj/iris/internal/session"
	"github.com/elevenlj/iris/internal/store"
)

// One HTTP service; each bot reuses the existing manager/bridge with its own
// database, recovery files, uploads, message registry and headless renderers.
type botRuntime struct {
	manager  *session.Manager
	bridge   *session.LarkReplyBridge
	server   *httpapi.Server
	store    *store.SQLite
	headless *headlessBrowserManager
}

type botService struct {
	createMu sync.Mutex
	root     *appConfigService // root.mu also protects runtimes and all bot config writes
	server   *httpapi.Server
	dataDir  string
	runtimes map[string]*botRuntime
	// Injectable for local regression tests; real creation checks credentials,
	// sends a test card, and verifies the group-message permission.
	test    func(httpapi.RuntimeConfig) httpapi.LarkConfigTestResult
	appName func(context.Context, string, string) (string, error)
}

func (s *botService) CreateBot(ctx context.Context, bot httpapi.BotConfig) (httpapi.BotConfig, error) {
	if !s.createMu.TryLock() {
		return bot, errors.New("已有机器人正在创建，请等待完成")
	}
	defer s.createMu.Unlock()
	if bot.ID != "" || strings.TrimSpace(bot.Name) == "" || len([]rune(bot.Name)) > 40 {
		return bot, errors.New("请填写机器人名称")
	}
	cfg := s.root.RuntimeConfig()
	if _, _, err := validateAgentDefinitions(cfg.Agents, bot.DefaultAgentID); err != nil {
		return bot, err
	}
	if _, err := validateDefaultWorkspaceDir(bot.DefaultWorkspaceDir); err != nil {
		return bot, err
	}
	app, err := createFeishuApp(ctx, bot.Name, s.dataDir)
	if err != nil {
		return bot, err
	}
	bot.AppID, bot.AppSecret = app.AppID, app.AppSecret
	bot.ReceiveID, err = httpapi.ResolveLarkOwner(ctx, app.AppID, app.AppSecret, app.OwnerEmail)
	if err != nil {
		return bot, fmt.Errorf("应用 %s 已创建，但获取开发者身份失败：%w。请勿重复创建", app.AppID, err)
	}
	result, err := s.SaveBot(ctx, bot)
	if err != nil {
		return bot, fmt.Errorf("应用 %s 已创建，但连接验证失败：%w。请勿重复创建", app.AppID, err)
	}
	return result, nil
}

func newBotService(root *appConfigService, server *httpapi.Server, dataDir string) *botService {
	return &botService{root: root, server: server, dataDir: dataDir, runtimes: make(map[string]*botRuntime), test: httpapi.TestLarkConfig, appName: httpapi.LarkAppName}
}

func (s *botService) Start() error {
	s.root.mu.Lock()
	defer s.root.mu.Unlock()
	cfg := *s.root.cfg
	if len(cfg.Bots) == 0 && cfg.LarkAppID != "" {
		cfg.Bots = []httpapi.BotConfig{{ID: "default", Name: "Iris", AppID: cfg.LarkAppID, AppSecret: cfg.LarkAppSecret, ReceiveID: cfg.LarkNotifyReceiveID, DefaultAgentID: cfg.DefaultAgentID, DefaultWorkspaceDir: cfg.DefaultWorkspaceDir}}
		if err := writeConfigFile(s.root.path, cfg); err != nil {
			return err
		}
		*s.root.cfg = cfg
	}
	for _, bot := range cfg.Bots {
		if !validBotID(bot.ID) {
			return fmt.Errorf("无效机器人 ID：%s", bot.ID)
		}
		if bot.ID == "default" {
			if err := applyRuntimeConfig(botEffectiveConfig(cfg, bot), s.root.manager, s.root.bridge, false); err != nil {
				return err
			}
			continue
		}
		if _, err := s.startBot(cfg, bot); err != nil {
			return err
		}
	}
	return nil
}

func validBotID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func botEffectiveConfig(global Config, bot httpapi.BotConfig) Config {
	global.LarkAppID, global.LarkAppSecret, global.LarkNotifyReceiveID = bot.AppID, bot.AppSecret, bot.ReceiveID
	global.DefaultAgentID = bot.DefaultAgentID
	if bot.DefaultWorkspaceDir != "" {
		global.DefaultWorkspaceDir = bot.DefaultWorkspaceDir
	}
	return syncLegacyDefaultAgent(global)
}

func (s *botService) ListBots() []httpapi.BotConfig {
	s.root.mu.Lock()
	defer s.root.mu.Unlock()
	out := append([]httpapi.BotConfig{}, s.root.cfg.Bots...)
	// Keep the migrated default robot in sync with older clients of /api/config.
	for i := range out {
		if out[i].ID == "default" {
			out[i].AppID = s.root.cfg.LarkAppID
			out[i].AppSecret = s.root.cfg.LarkAppSecret
			out[i].ReceiveID = s.root.cfg.LarkNotifyReceiveID
		}
	}
	return out
}

func (s *botService) refreshAppNames() {
	for _, bot := range s.ListBots() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		name, err := s.appName(ctx, bot.AppID, bot.AppSecret)
		cancel()
		if err != nil || name == "" {
			continue
		}
		s.root.mu.Lock()
		cfg := *s.root.cfg
		cfg.Bots = append([]httpapi.BotConfig(nil), cfg.Bots...)
		for i := range cfg.Bots {
			if cfg.Bots[i].ID == bot.ID && cfg.Bots[i].AppID == bot.AppID {
				cfg.Bots[i].AppName = name
			}
		}
		if writeConfigFile(s.root.path, cfg) == nil {
			*s.root.cfg = cfg
		}
		s.root.mu.Unlock()
	}
}

func (s *botService) BotHandler(id string) http.Handler {
	s.root.mu.Lock()
	defer s.root.mu.Unlock()
	if id == "default" {
		return s.server.Handler()
	}
	if rt := s.runtimes[id]; rt != nil {
		return rt.server.Handler()
	}
	return nil
}

func (s *botService) SaveBot(ctx context.Context, bot httpapi.BotConfig) (httpapi.BotConfig, error) {
	bot.Name, bot.AppID, bot.AppSecret, bot.ReceiveID = strings.TrimSpace(bot.Name), strings.TrimSpace(bot.AppID), strings.TrimSpace(bot.AppSecret), strings.TrimSpace(bot.ReceiveID)
	if bot.Name == "" || len([]rune(bot.Name)) > 40 {
		return bot, errors.New("机器人名称须为 1–40 个字符")
	}
	if bot.AppID == "" || bot.AppSecret == "" || bot.ReceiveID == "" {
		return bot, errors.New("请填写 App ID、App Secret 和通知接收 ID")
	}
	if bot.ID != "" && !validBotID(bot.ID) {
		return bot, errors.New("无效机器人 ID")
	}
	s.root.mu.Lock()
	cfg := *s.root.cfg
	s.root.mu.Unlock()
	_, _, err := validateAgentDefinitions(cfg.Agents, bot.DefaultAgentID)
	if err != nil {
		return bot, err
	}
	bot.DefaultWorkspaceDir, err = validateDefaultWorkspaceDir(bot.DefaultWorkspaceDir)
	if err != nil {
		return bot, err
	}
	var old httpapi.BotConfig
	for _, item := range cfg.Bots {
		if item.ID == bot.ID {
			old = item
		}
		if item.ID != bot.ID && item.AppID == bot.AppID {
			return bot, errors.New("此飞书应用已接入，不能重复创建机器人")
		}
	}
	if bot.ID != "" && old.ID == "" {
		return bot, errors.New("机器人不存在")
	}
	credentialsChanged := old.AppID != bot.AppID || old.AppSecret != bot.AppSecret || old.ReceiveID != bot.ReceiveID
	if credentialsChanged {
		if err := ctx.Err(); err != nil {
			return bot, err
		}
		result := s.test(runtimeConfigFromConfig(botEffectiveConfig(cfg, bot)))
		if !result.OK {
			for _, step := range result.Steps {
				if !step.OK {
					return bot, fmt.Errorf("%s：%s", step.Name, step.Message)
				}
			}
			return bot, errors.New("飞书连接验证失败")
		}
		bot.AppName, err = s.appName(ctx, bot.AppID, bot.AppSecret)
		if err != nil {
			return bot, err
		}
	} else {
		bot.AppName = old.AppName
	}
	s.root.mu.Lock()
	defer s.root.mu.Unlock()
	cfg = *s.root.cfg
	if err := ctx.Err(); err != nil {
		return bot, err
	}
	if _, _, err := validateAgentDefinitions(cfg.Agents, bot.DefaultAgentID); err != nil {
		return bot, err
	}
	for _, item := range cfg.Bots {
		if item.ID != bot.ID && item.AppID == bot.AppID {
			return bot, errors.New("此飞书应用已接入")
		}
	}
	if bot.ID == "" {
		if len(cfg.Bots) == 0 {
			bot.ID = "default"
		} else {
			token, err := randomHex(12)
			if err != nil {
				return bot, err
			}
			bot.ID = "bot-" + token
		}
	}
	next := append([]httpapi.BotConfig(nil), cfg.Bots...)
	found := false
	for i := range next {
		if next[i].ID == bot.ID {
			next[i] = bot
			found = true
		}
	}
	if !found {
		next = append(next, bot)
	}
	cfg.Bots = next
	if bot.ID == "default" {
		cfg.LarkAppID = bot.AppID
		cfg.LarkAppSecret = bot.AppSecret
		cfg.LarkNotifyReceiveID = bot.ReceiveID
	}
	// Persist atomically before making the new credentials live.
	if err := writeConfigFile(s.root.path, cfg); err != nil {
		return bot, err
	}
	previous := *s.root.cfg
	if bot.ID == "default" {
		err = applyRuntimeConfig(botEffectiveConfig(cfg, bot), s.root.manager, s.root.bridge, credentialsChanged)
	} else if rt := s.runtimes[bot.ID]; rt != nil {
		err = applyRuntimeConfig(botEffectiveConfig(cfg, bot), rt.manager, rt.bridge, credentialsChanged)
	} else {
		_, err = s.startBot(cfg, bot)
	}
	if err != nil {
		_ = writeConfigFile(s.root.path, previous)
		return bot, err
	}
	*s.root.cfg = cfg
	return bot, nil
}

func (s *botService) startBot(global Config, bot httpapi.BotConfig) (*botRuntime, error) {
	base := filepath.Join(s.dataDir, "bots", bot.ID)
	uploads := uploadsDirInDataDir(base)
	if err := os.MkdirAll(uploads, 0700); err != nil {
		return nil, err
	}
	st, err := store.Open(dbPathInDataDir(base))
	if err != nil {
		return nil, err
	}
	headless := newHeadlessBrowserManager(global.Port)
	headless.pathPrefix = "/bots/" + bot.ID
	mgr := session.NewManager(st, session.ShellLauncher{}, session.WithIsolatedMessageRegistry(),
		session.WithRecoveryBaseDir(filepath.Join(base, "data", "sessions")),
		session.WithAgentTurnHookURL("http://127.0.0.1:"+global.Port+headless.pathPrefix),
		session.WithBrowserNeeded(headless.Ensure), session.WithBrowserActive(headless.Stop), session.WithBrowserStopped(headless.Stop),
		session.WithSessionEnded(func(id string) { headless.Stop(id); _ = os.RemoveAll(filepath.Join(uploads, id)) }))
	bridge := session.NewLarkReplyBridge("", "", mgr, uploads)
	if err := applyRuntimeConfig(botEffectiveConfig(global, bot), mgr, bridge, true); err != nil {
		st.Close()
		return nil, err
	}
	rt := &botRuntime{manager: mgr, bridge: bridge, store: st, headless: headless, server: httpapi.NewServer(mgr, uploads, s.root)}
	s.runtimes[bot.ID] = rt
	return rt, nil
}

// Called while root.mu is held by global settings updates.
func (s *botService) applyGlobal(cfg Config) error {
	for _, bot := range cfg.Bots {
		if bot.ID == "default" {
			if err := applyRuntimeConfig(botEffectiveConfig(cfg, bot), s.root.manager, s.root.bridge, false); err != nil {
				return err
			}
			continue
		}
		if rt := s.runtimes[bot.ID]; rt != nil {
			if err := applyRuntimeConfig(botEffectiveConfig(cfg, bot), rt.manager, rt.bridge, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *botService) Close() {
	s.root.mu.Lock()
	defer s.root.mu.Unlock()
	if s.root.bridge != nil {
		s.root.bridge.Stop()
	}
	for _, rt := range s.runtimes {
		rt.bridge.Stop()
		rt.headless.StopAll()
		rt.store.Close()
	}
}
