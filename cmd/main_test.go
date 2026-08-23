package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"easy_terminal/internal/httpapi"
	"easy_terminal/internal/session"
)

func TestEnvFallback(t *testing.T) {
	t.Setenv("EASY_TERMINAL_TEST_ENV", "")
	if got := env("EASY_TERMINAL_TEST_ENV", "fallback"); got != "fallback" {
		t.Fatalf("expected fallback, got %q", got)
	}
	t.Setenv("EASY_TERMINAL_TEST_ENV", "value")
	if got := env("EASY_TERMINAL_TEST_ENV", "fallback"); got != "value" {
		t.Fatalf("expected env value, got %q", got)
	}
}

func TestParseStartupOptionsPort(t *testing.T) {
	opts, err := parseStartupOptions([]string{"--port", "9090"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Port != "9090" {
		t.Fatalf("expected port override, got %q", opts.Port)
	}

	opts, err = parseStartupOptions([]string{"-p", "7070"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Port != "7070" {
		t.Fatalf("expected short port override, got %q", opts.Port)
	}
}

func TestParseStartupOptionsConfigDir(t *testing.T) {
	opts, err := parseStartupOptions([]string{"--config-dir", "/tmp/easy-config"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.ConfigDir != "/tmp/easy-config" {
		t.Fatalf("expected config dir override, got %q", opts.ConfigDir)
	}
}

func TestParseStartupOptionsRejectsPositionalConfigDir(t *testing.T) {
	if _, err := parseStartupOptions([]string{"/tmp/easy-config"}); err == nil {
		t.Fatal("expected positional config dir to fail")
	}
}

func TestParseStartupOptionsVersion(t *testing.T) {
	for _, arg := range []string{"--version", "-version", "-v"} {
		opts, err := parseStartupOptions([]string{arg})
		if err != nil {
			t.Fatalf("parse %s: %v", arg, err)
		}
		if !opts.Version {
			t.Fatalf("expected version for %s", arg)
		}
	}
}

func TestParseStartupOptionsInstallAgentHooks(t *testing.T) {
	opts, err := parseStartupOptions([]string{"--install-agent-hooks"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.InstallAgentHooks {
		t.Fatal("expected Agent hook installation mode")
	}
}

func TestHeadlessChromeArgsUseStableViewport(t *testing.T) {
	args := headlessChromeArgsForUID("/tmp/profile", "http://localhost:8080/?session=sess-1", 1000)
	for _, want := range []string{
		"--window-size=1440,1000",
		"--force-device-scale-factor=1",
		"--hide-scrollbars",
		"--user-data-dir=/tmp/profile",
		"http://localhost:8080/?session=sess-1",
	} {
		if !slices.Contains(args, want) {
			t.Fatalf("headless args missing %q: %#v", want, args)
		}
	}
}

func TestHeadlessChromeArgsAllowRootContainer(t *testing.T) {
	args := headlessChromeArgsForUID("/tmp/profile", "http://localhost:8080/", 0)
	if !slices.Contains(args, "--no-sandbox") {
		t.Fatalf("root headless args missing --no-sandbox: %#v", args)
	}
}

func TestLoadConfigUsesCurrentDefaultsWhenFieldsMissing(t *testing.T) {
	wd := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWd); err != nil {
			t.Fatalf("restore wd: %v", err)
		}
	})
	t.Setenv("PORT", "")
	t.Setenv("LARK_APP_ID", "")
	t.Setenv("LARK_APP_SECRET", "")
	t.Setenv("LARK_NOTIFY_RECEIVE_ID", "")
	t.Setenv("LARK_DEFAULT_SESSION_NAME", "")
	t.Setenv("LARK_SESSION_CHAT_PREFIX", "")
	t.Setenv("SESSION_PRE_START_COMMAND", "")
	t.Setenv("LARK_MENTION_ENABLED", "")
	t.Setenv("LARK_NOTIFY_MERGE_WRAPPED_LINES", "")

	cfg := loadConfig(filepath.Join(t.TempDir(), "config.local.json"))
	if cfg.FastWaitingTransitionMs != 500 || cfg.ConservativeWaitingTransitionMs != 500 || cfg.LarkAutoRefreshIntervalMs != 5000 || cfg.HeadlessSnapshotTimeoutMs != 10000 || cfg.LarkNotifyMaxLines != 200 || cfg.LarkNotifyFallbackTailLines != 100 {
		t.Fatalf("numeric defaults = %d,%d,%d,%d,%d,%d", cfg.FastWaitingTransitionMs, cfg.ConservativeWaitingTransitionMs, cfg.LarkAutoRefreshIntervalMs, cfg.HeadlessSnapshotTimeoutMs, cfg.LarkNotifyMaxLines, cfg.LarkNotifyFallbackTailLines)
	}
	if cfg.LarkDefaultSessionName != "默认会话" || cfg.LarkSessionChatPrefix != "Iris ·" {
		t.Fatalf("lark defaults = name %q prefix %q", cfg.LarkDefaultSessionName, cfg.LarkSessionChatPrefix)
	}
	if len(cfg.LarkNotifyDropLineRules) != len(defaultLarkNotifyDropLineRules) || cfg.LarkNotifyDropLineRules[0].Title != "空行" || cfg.LarkNotifyDropLineRules[1].Title != "横线" {
		t.Fatalf("default drop line rules = %#v", cfg.LarkNotifyDropLineRules)
	}
	if !cfg.LarkNotifyMergeWrappedLines {
		t.Fatalf("merge wrapped lines should default to true")
	}
}

func TestLoadConfigPrependsRequiredToolFiltersToExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.local.json")
	if err := os.WriteFile(path, []byte(`{
  "lark_notify_drop_line_patterns": [
    {"pattern":"^(• Ran|• Explored).*", "kind":"block_head", "action":"keep_head"}
  ]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(path)
	if len(cfg.LarkNotifyDropLineRules) < 4 {
		t.Fatalf("required filters were not merged: %#v", cfg.LarkNotifyDropLineRules)
	}
	first := cfg.LarkNotifyDropLineRules[0]
	if first.Title != "Codex 工具执行过程" || first.Action != "drop_block" {
		t.Fatalf("required tool filter must precede obsolete local rules: %#v", cfg.LarkNotifyDropLineRules)
	}
}

func TestDefaultPathsUseStableUserDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("IRIS_HOME", "")
	t.Setenv("IRIS_CONFIG_DIR", "")
	t.Setenv("EASY_TERMINAL_HOME", "")
	t.Setenv("EASY_TERMINAL_CONFIG_DIR", "")

	if got := defaultConfigPath(); got != filepath.Join(home, ".iris", "conf", "config.local.json") {
		t.Fatalf("default config path = %q", got)
	}
	if got := defaultDBPath(); got != filepath.Join(home, ".iris", "iris.db") {
		t.Fatalf("default db path = %q", got)
	}
	if got := defaultUploadsDir(); got != filepath.Join(home, ".iris", "data", "uploads") {
		t.Fatalf("default uploads dir = %q", got)
	}
	if got := defaultLogDir(); got != filepath.Join(home, ".iris", "log") {
		t.Fatalf("default log dir = %q", got)
	}
}

func TestDefaultPathsAllowHomeOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IRIS_HOME", dir)
	t.Setenv("IRIS_CONFIG_DIR", "")
	t.Setenv("EASY_TERMINAL_HOME", "")
	t.Setenv("EASY_TERMINAL_CONFIG_DIR", "")
	if got := defaultConfigPath(); got != filepath.Join(dir, "conf", "config.local.json") {
		t.Fatalf("default config path with override = %q", got)
	}
}

func TestDefaultConfigPathAllowsConfigDirOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IRIS_HOME", "")
	t.Setenv("IRIS_CONFIG_DIR", dir)
	t.Setenv("EASY_TERMINAL_HOME", "")
	t.Setenv("EASY_TERMINAL_CONFIG_DIR", "")
	if got := defaultConfigPath(); got != filepath.Join(dir, "config.local.json") {
		t.Fatalf("default config path with config dir override = %q", got)
	}
	if got := defaultDBPath(); got != filepath.Join(dir, "iris.db") {
		t.Fatalf("default db path with config dir override = %q", got)
	}
	if got := defaultUploadsDir(); got != filepath.Join(dir, "data", "uploads") {
		t.Fatalf("default uploads dir with config dir override = %q", got)
	}
	if got := defaultLogDir(); got != filepath.Join(dir, "log") {
		t.Fatalf("default log dir with config dir override = %q", got)
	}
	if got := configPathFromDir(filepath.Join(dir, "custom")); got != filepath.Join(dir, "custom", "config.local.json") {
		t.Fatalf("config path from cli dir = %q", got)
	}
}

func TestCLIConfigDirScopesRuntimeData(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "instance")
	t.Setenv("IRIS_HOME", "")
	t.Setenv("IRIS_CONFIG_DIR", "")
	t.Setenv("EASY_TERMINAL_HOME", "")
	t.Setenv("EASY_TERMINAL_CONFIG_DIR", "")
	if got := dataDirFromConfigDir(dir); got != dir {
		t.Fatalf("data dir from cli config dir = %q", got)
	}
	if got := dbPathInDataDir(dataDirFromConfigDir(dir)); got != filepath.Join(dir, "iris.db") {
		t.Fatalf("db path from cli config dir = %q", got)
	}
	if got := uploadsDirInDataDir(dataDirFromConfigDir(dir)); got != filepath.Join(dir, "data", "uploads") {
		t.Fatalf("uploads dir from cli config dir = %q", got)
	}
	if got := logDirInDataDir(dataDirFromConfigDir(dir)); got != filepath.Join(dir, "log") {
		t.Fatalf("log dir from cli config dir = %q", got)
	}
}

func TestEnsureConfigFileCreatesMissingDirectoryAndConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "conf", "config.local.json")
	if err := ensureConfigFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected config file to be created: %v", err)
	}
	cfg := loadConfig(path)
	if cfg.LarkDefaultSessionName != defaultLarkDefaultSessionName {
		t.Fatalf("generated config default name = %q", cfg.LarkDefaultSessionName)
	}
}

func TestConfigDirMissingFileDoesNotFallBackToDefaultConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("EASY_TERMINAL_HOME", "")
	t.Setenv("EASY_TERMINAL_CONFIG_DIR", "")
	if err := writeConfigFile(defaultConfigPath(), Config{
		Port:                            "8080",
		LarkDefaultSessionName:          "旧默认配置",
		FastWaitingTransitionMs:         defaultFastWaitingTransitionMs,
		ConservativeWaitingTransitionMs: defaultConservativeWaitingTransitionMs,
		LarkAutoRefreshIntervalMs:       defaultLarkAutoRefreshIntervalMs,
		LarkNotifyMaxLines:              defaultLarkNotifyMaxLines,
		LarkNotifyFallbackTailLines:     defaultLarkNotifyFallbackTailLines,
	}); err != nil {
		t.Fatal(err)
	}

	path := configPathFromDir(filepath.Join(t.TempDir(), "new-conf"))
	if err := ensureConfigFile(path); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(path)
	if cfg.LarkDefaultSessionName == "旧默认配置" {
		t.Fatal("custom config dir should not fall back to the default config file")
	}
}

func TestAppConfigServiceUpdatesRuntimeConfigAndPersists(t *testing.T) {
	t.Cleanup(func() { session.SetLarkNotifyMergeWrappedLines(false) })
	path := filepath.Join(t.TempDir(), "config.local.json")
	cfg := Config{
		Port:                            "8080",
		LarkMentionEnabled:              true,
		LarkDefaultSessionName:          "默认会话",
		LarkIgnoreMessagePrefix:         "/i",
		LarkAutoSummaryPrompt:           "总结上一轮输出",
		FastWaitingTransitionMs:         300,
		ConservativeWaitingTransitionMs: 700,
		LarkAutoRefreshIntervalMs:       5000,
		LarkNotifyMaxLines:              300,
		LarkNotifyFallbackTailLines:     100,
	}
	mgr := session.NewManager(nil, nil)
	svc := &appConfigService{path: path, cfg: &cfg, manager: mgr}

	got, err := svc.UpdateRuntimeConfig(httpapi.RuntimeConfig{
		LarkAppID:                       "app",
		LarkAppSecret:                   "secret",
		LarkNotifyReceiveID:             "ou_1",
		LarkMentionEnabled:              false,
		LarkDefaultSessionName:          "默认",
		LarkIgnoreMessagePrefix:         "/silent",
		LarkAutoSummaryPrompt:           "总结上一轮输出",
		FastWaitingTransitionMs:         450,
		ConservativeWaitingTransitionMs: 900,
		LarkAutoRefreshIntervalMs:       6000,
		LarkNotifyMaxLines:              120,
		LarkNotifyFallbackTailLines:     80,
		LarkNotifyMergeWrappedLines:     true,
		LarkNotifyDropLineRules: session.LarkNotifyDropLineRules{
			{Title: "noise", Kind: "block_head", Pattern: "noise", Action: "keep_head"},
			{Title: "debug", Kind: "line_group", Pattern: `(debug=)([^ ]+)`, Groups: []int{2}},
		},
		LarkCustomShortcuts:    []session.LarkCustomShortcut{{Label: "状态", Command: "git status"}},
		OnboardingCompleted:    true,
		SessionPreStartCommand: "source ~/.zshrc",
		SessionStartPresets:    map[string]session.SessionStartPreset{"1": {Commands: []string{"codex"}}},
		SessionNamePresets:     map[string]session.SessionStartPreset{"会话 A": {Commands: []string{"pwd"}}},
		AgentKind:              "codex",
		AgentCommand:           "codex --dangerously-bypass-approvals-and-sandbox",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.FastWaitingTransitionMs != 450 || got.LarkAutoRefreshIntervalMs != 6000 || got.LarkNotifyFallbackTailLines != 80 || got.LarkAppID != "app" || got.LarkIgnoreMessagePrefix != "/silent" || got.LarkAutoSummaryPrompt != "总结上一轮输出" || !got.LarkNotifyMergeWrappedLines {
		t.Fatalf("unexpected runtime config: %#v", got)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.FastWaitingTransitionMs != 450 || saved.LarkAutoRefreshIntervalMs != 6000 || saved.LarkNotifyFallbackTailLines != 80 || saved.SessionPreStartCommand != "source ~/.zshrc" || saved.LarkAppSecret != "secret" {
		t.Fatalf("config file was not updated: %#v", saved)
	}
	if saved.LarkIgnoreMessagePrefix != "/silent" {
		t.Fatalf("ignore prefix was not persisted: %#v", saved)
	}
	if saved.LarkAutoSummaryPrompt != "总结上一轮输出" {
		t.Fatalf("auto summary prompt was not persisted: %#v", saved)
	}
	if !saved.LarkNotifyMergeWrappedLines {
		t.Fatalf("merge wrapped lines was not persisted: %#v", saved)
	}
	if len(saved.LarkNotifyDropLineRules) != 2 ||
		saved.LarkNotifyDropLineRules[0].Pattern != "noise" ||
		saved.LarkNotifyDropLineRules[0].Action != "keep_head" ||
		len(saved.LarkNotifyDropLineRules[1].Groups) != 1 ||
		saved.LarkNotifyDropLineRules[1].Groups[0] != 2 {
		t.Fatalf("drop patterns were not persisted: %#v", saved.LarkNotifyDropLineRules)
	}
	if len(saved.LarkCustomShortcuts) != 1 || saved.LarkCustomShortcuts[0].Command != "git status" {
		t.Fatalf("custom shortcuts were not persisted: %#v", saved.LarkCustomShortcuts)
	}
	if !saved.OnboardingCompleted {
		t.Fatalf("onboarding completion was not persisted: %#v", saved)
	}
	if saved.SessionStartPresets["1"].Commands[0] != "codex" || saved.SessionNamePresets["会话 A"].Commands[0] != "pwd" {
		t.Fatalf("presets were not persisted: start=%#v name=%#v", saved.SessionStartPresets, saved.SessionNamePresets)
	}
}
