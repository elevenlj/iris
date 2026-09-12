package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elevenlj/iris/internal/httpapi"
	"github.com/elevenlj/iris/internal/session"
	"github.com/elevenlj/iris/internal/store"
)

func TestBotsIsolationPersistenceAndLegacyMigration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "iris.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := defaultConfig()
	cfg.LarkAppID = "cli_legacy"
	cfg.LarkAppSecret = "secret-a"
	cfg.LarkNotifyReceiveID = "ou_a"
	cfg.SettingsPasswordHash = "configured"
	cfg.Agents = []session.AgentConfig{{ID: "custom-test", Kind: "custom", Name: "Test", Command: "sh"}}
	cfg.DefaultAgentID = "custom-test"
	cfg = syncLegacyDefaultAgent(cfg)
	mgr := session.NewManager(st, nil, session.WithIsolatedMessageRegistry())
	svc := &appConfigService{cfg: &cfg, path: filepath.Join(dir, "config.json"), manager: mgr}
	server := httpapi.NewServer(mgr, filepath.Join(dir, "uploads"), svc)
	bots := newBotService(svc, server, dir)
	server.SetBotService(bots)
	svc.bots = bots
	if err := bots.Start(); err != nil {
		t.Fatal(err)
	}
	defer bots.Close()
	if len(cfg.Bots) != 1 || cfg.Bots[0].ID != "default" {
		t.Fatal("legacy bot not migrated")
	}
	now := time.Now().UTC()
	legacy := session.Session{ID: "sess-1", Name: "legacy", Live: true, Status: session.StatusWaiting, CreatedAt: now, UpdatedAt: now, LarkChatID: "oc_same", RecoveryKey: "old-key"}
	if err := st.CreateSession(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	other := httpapi.BotConfig{ID: "bot-test", Name: "Other", DefaultAgentID: "custom-test", DefaultWorkspaceDir: t.TempDir()}
	rt, err := bots.startBot(cfg, other)
	if err != nil {
		t.Fatal(err)
	}
	second := legacy
	second.Name = "other"
	second.RecoveryKey = "other-key"
	if err := rt.store.CreateSession(ctx, second); err != nil {
		t.Fatal(err)
	}
	cfg.Bots = append(cfg.Bots, other)
	for path, want := range map[string]string{"/api/sessions": "legacy", "/bots/bot-test/api/sessions": "other"} {
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var sessions []session.Session
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &sessions) != nil || len(sessions) != 1 || sessions[0].Name != want {
			t.Fatalf("%s isolation failed: %s", path, rec.Body.String())
		}
	}
	if rt.manager.AgentTurnHookURL() == mgr.AgentTurnHookURL() || !strings.Contains(rt.manager.AgentTurnHookURL(), "/bots/bot-test") {
		t.Fatal("callback URL not scoped")
	}
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/bots", nil))
	if strings.Contains(rec.Body.String(), "secret-a") || strings.Contains(rec.Body.String(), "ou_a") {
		t.Fatal("public bot list leaks secrets")
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/bots", strings.NewReader(`{}`)))
	if rec.Code == 200 {
		t.Fatal("unauthenticated bot write accepted")
	}
	if bots.BotHandler("../default") != nil {
		t.Fatal("unknown route accepted")
	}
	if err := rt.store.DeleteSession(ctx, "sess-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.GetSession(ctx, "sess-1"); err != nil || !ok {
		t.Fatal("other bot deletion removed legacy session")
	}
	if err := writeConfigFile(svc.path, cfg); err != nil {
		t.Fatal(err)
	}
	reloaded := loadConfig(svc.path)
	if len(reloaded.Bots) != 2 || reloaded.Bots[1].ID != "bot-test" {
		t.Fatal("bot config not persisted")
	}
	info, _ := os.Stat(svc.path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("config permissions")
	}
}

func TestBotValidationAndAutomaticTest(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.Agents = []session.AgentConfig{{ID: "custom-test", Kind: "custom", Name: "Test", Command: "sh"}}
	cfg.DefaultAgentID = "custom-test"
	cfg = syncLegacyDefaultAgent(cfg)
	mgr := session.NewManager(nil, nil, session.WithIsolatedMessageRegistry())
	svc := &appConfigService{cfg: &cfg, path: filepath.Join(dir, "config.json"), manager: mgr}
	bots := newBotService(svc, httpapi.NewServer(mgr, dir, svc), dir)
	calls := 0
	bots.test = func(httpapi.RuntimeConfig) httpapi.LarkConfigTestResult {
		calls++
		return httpapi.LarkConfigTestResult{OK: true}
	}
	bots.appName = func(context.Context, string, string) (string, error) { return "Real app name", nil }
	bot := httpapi.BotConfig{Name: "My bot", AppID: "cli_new", AppSecret: "secret", ReceiveID: "ou_owner", DefaultAgentID: "custom-test", DefaultWorkspaceDir: dir}
	saved, err := bots.SaveBot(context.Background(), bot)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || saved.ID != "default" || saved.AppName != "Real app name" {
		t.Fatalf("creation not tested: %#v", saved)
	}
	saved.Name = "Renamed"
	if _, err := bots.SaveBot(context.Background(), saved); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("name-only edit should not send a test card")
	}
	if _, err := bots.SaveBot(context.Background(), bot); err == nil {
		t.Fatal("duplicate app accepted")
	}
	saved.AppID = "cli_replacement"
	bots.test = func(httpapi.RuntimeConfig) httpapi.LarkConfigTestResult {
		return httpapi.LarkConfigTestResult{Steps: []httpapi.LarkConfigTestStep{{Name: "permission", Message: "missing"}}}
	}
	if _, err := bots.SaveBot(context.Background(), saved); err == nil {
		t.Fatal("failed permission test was accepted")
	}
	if cfg.Bots[0].AppID != "cli_new" {
		t.Fatal("failed update replaced live credentials")
	}
	for _, id := range []string{"../a", "a/b", "", "A", "a.b"} {
		if validBotID(id) {
			t.Fatalf("unsafe id %q", id)
		}
	}
}

func TestServiceCommandsRejectLegacySelectors(t *testing.T) {
	for _, args := range [][]string{{"stop", "all"}, {"restart", "all"}, {"stop", "8080"}} {
		var out strings.Builder
		if handled, err := handleServiceCommand(args, strings.NewReader(""), &out, t.TempDir(), false); !handled || err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestCustomAgentMigrationKeepsExistingDefinition(t *testing.T) {
	custom := session.AgentConfig{ID: "custom-existing", Kind: "custom", Name: "Shell", Command: "sh"}
	for _, selected := range []string{"", custom.ID} {
		cfg, _ := migrateAgentDefinitions(Config{Agents: []session.AgentConfig{custom}, DefaultAgentID: selected, AgentKind: "custom", AgentName: "Shell", AgentCommand: "sh"})
		count := 0
		for _, agent := range cfg.Agents {
			if agent.Kind == "custom" {
				count++
			}
		}
		if count != 1 || cfg.DefaultAgentID != custom.ID {
			t.Fatal("existing custom Agent duplicated or default lost")
		}
	}
}

func TestBotInheritsGlobalDirectoryWithoutLosingAgentChoice(t *testing.T) {
	cfg := defaultConfig()
	cfg.DefaultWorkspaceDir = t.TempDir()
	bot := httpapi.BotConfig{DefaultAgentID: "claude"}
	effective := botEffectiveConfig(cfg, bot)
	if effective.DefaultWorkspaceDir != cfg.DefaultWorkspaceDir || effective.DefaultAgentID != "claude" {
		t.Fatal("bot defaults did not inherit correctly")
	}
	bot.DefaultWorkspaceDir = t.TempDir()
	if botEffectiveConfig(cfg, bot).DefaultWorkspaceDir != bot.DefaultWorkspaceDir {
		t.Fatal("explicit bot directory lost")
	}
}

func TestBotsBrowserIntegration(t *testing.T) {
	if os.Getenv("IRIS_BROWSER_TEST") != "1" {
		t.Skip("set IRIS_BROWSER_TEST=1 with playwright-core on NODE_PATH")
	}
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.AutoStartEnabled = false
	cfg.OnboardingCompleted = true
	cfg.Agents = []session.AgentConfig{{ID: "custom-test", Kind: "custom", Name: "Shell", Command: "bash --noprofile --norc"}}
	cfg.DefaultAgentID = "custom-test"
	cfg = syncLegacyDefaultAgent(cfg)
	cfg.DefaultWorkspaceDir = dir
	cfg.LarkAppID = "cli_fixture"
	cfg.LarkAppSecret = "fixture-secret"
	cfg.LarkNotifyReceiveID = "ou_fixture"
	cfg.Bots = []httpapi.BotConfig{{ID: "default", Name: "开发助手", AppName: "开发助手应用", AppID: cfg.LarkAppID, AppSecret: cfg.LarkAppSecret, ReceiveID: cfg.LarkNotifyReceiveID, DefaultAgentID: "custom-test", DefaultWorkspaceDir: dir}, {ID: "bot-second", Name: "日常助理", DefaultAgentID: "custom-test", DefaultWorkspaceDir: dir}}
	st, err := store.Open(filepath.Join(dir, "browser.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mgr := session.NewManager(st, session.ShellLauncher{}, session.WithIsolatedMessageRegistry())
	mgr.SetDefaultWorkspaceDir(dir)
	svc := &appConfigService{cfg: &cfg, path: filepath.Join(dir, "config.json"), manager: mgr}
	server := httpapi.NewServer(mgr, filepath.Join(dir, "uploads"), svc)
	bots := newBotService(svc, server, dir)
	server.SetBotService(bots)
	svc.bots = bots
	bots.test = func(httpapi.RuntimeConfig) httpapi.LarkConfigTestResult {
		return httpapi.LarkConfigTestResult{OK: true}
	}
	bots.appName = func(context.Context, string, string) (string, error) { return "测试应用", nil }
	host := httptest.NewUnstartedServer(server.Handler())
	_, cfg.Port, _ = net.SplitHostPort(host.Listener.Addr().String())
	defer host.Close()
	rt, err := bots.startBot(cfg, cfg.Bots[1])
	if err != nil {
		t.Fatal(err)
	}
	defer bots.Close()
	for _, item := range []struct {
		m    *session.Manager
		name string
	}{{mgr, "Root terminal"}, {rt.manager, "Other terminal"}} {
		sess, err := item.m.CreateSession(context.Background(), item.name)
		if err != nil {
			t.Fatal(err)
		}
		if runtime, ok := item.m.GetRuntime(sess.ID); ok {
			defer runtime.Close()
		}
		_, _, _ = item.m.UpdateNotifyOnWaiting(context.Background(), sess.ID, false)
	}
	host.Start()
	command := exec.Command("node", "../tests/bots_browser_e2e.cjs")
	command.Env = append(os.Environ(), "IRIS_TEST_URL="+host.URL)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("browser regression failed: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
}
