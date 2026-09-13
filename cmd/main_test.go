package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/elevenlj/iris/internal/httpapi"
	"github.com/elevenlj/iris/internal/session"
)

func TestEnvFallback(t *testing.T) {
	t.Setenv("IRIS_TEST_ENV", "")
	if got := env("IRIS_TEST_ENV", "fallback"); got != "fallback" {
		t.Fatalf("expected fallback, got %q", got)
	}
	t.Setenv("IRIS_TEST_ENV", "value")
	if got := env("IRIS_TEST_ENV", "fallback"); got != "value" {
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

func TestParseStartupOptionsNoOpen(t *testing.T) {
	opts, err := parseStartupOptions([]string{"--no-open"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.NoOpen {
		t.Fatal("expected browser auto-open to be disabled")
	}
}

func TestBrowserOpenCommandUsesPlatformLauncher(t *testing.T) {
	target := "http://localhost:8080/?settings=1"
	tests := []struct {
		goos string
		name string
	}{
		{goos: "darwin", name: "open"},
		{goos: "linux", name: "xdg-open"},
		{goos: "windows", name: "rundll32"},
	}
	for _, tt := range tests {
		name, args, err := browserOpenCommand(tt.goos, target)
		if err != nil {
			t.Fatalf("%s: %v", tt.goos, err)
		}
		if name != tt.name || len(args) == 0 || args[len(args)-1] != target {
			t.Fatalf("%s launcher = %q %#v", tt.goos, name, args)
		}
	}
	if _, _, err := browserOpenCommand("plan9", target); err == nil {
		t.Fatal("unsupported platform should return an error")
	}
}

func TestStartupBrowserURLUsesMainPage(t *testing.T) {
	got := startupBrowserURL(&net.TCPAddr{Port: 8080}, "")
	if got != "http://localhost:8080/" {
		t.Fatalf("startup browser URL = %q", got)
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
	if cfg.FastWaitingTransitionMs != 5000 || cfg.ConservativeWaitingTransitionMs != 5000 || cfg.LarkAutoRefreshIntervalMs != 5000 || cfg.HeadlessSnapshotTimeoutMs != 10000 || cfg.LarkNotifyMaxLines != 200 || cfg.LarkNotifyFallbackTailLines != 100 {
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
	if !cfg.AutoStartEnabled {
		t.Fatal("automatic startup should default to true")
	}
}

func TestLoadExistingConfigDefaultsAutomaticStartupToEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.local.json")
	if err := os.WriteFile(path, []byte(`{"port":"9090"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg := loadConfig(path); !cfg.AutoStartEnabled {
		t.Fatal("existing config without auto_start_enabled should default to enabled")
	}
}

func TestSelectRuntimeRecordsSupportsPortAndAll(t *testing.T) {
	records := []runtimeRecord{{Port: "8080"}, {Port: "9090"}}
	selected, err := selectRuntimeRecords(records, "9090")
	if err != nil || len(selected) != 1 || selected[0].Port != "9090" {
		t.Fatalf("port selection = %#v, %v", selected, err)
	}
	selected, err = selectRuntimeRecords(records, "all")
	if err == nil || len(selected) != 0 {
		t.Fatalf("all selection = %#v, %v", selected, err)
	}
}

func TestRuntimeRegistryDiscoversAndStopsExactInstance(t *testing.T) {
	stopping := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(runtimeControlHeader) != "secret" {
			http.NotFound(w, r)
			return
		}
		if stopping {
			http.Error(w, "stopping", http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodDelete {
			stopping = true
			_, _ = w.Write([]byte(`{"stopping":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"instance_id":"instance-1"}`))
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	record := runtimeRecord{InstanceID: "instance-1", Token: "secret", Port: port, StartedAt: time.Now().UTC()}
	if err := registerRuntimeRecord(dataDir, record); err != nil {
		t.Fatal(err)
	}
	records, err := listActiveRuntimeRecords(dataDir)
	if err != nil || len(records) != 1 || records[0].Port != port {
		t.Fatalf("active records = %#v, %v", records, err)
	}
	if err := stopRuntime(records[0]); err != nil {
		t.Fatal(err)
	}
}

func TestHandleServiceCommandRestartsExactInstance(t *testing.T) {
	stopping := false
	instanceID := "instance-1"
	token := "secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(runtimeControlHeader) != token || stopping {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodDelete {
			stopping = true
			_, _ = w.Write([]byte(`{"stopping":true}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"instance_id":%q}`, instanceID)
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	record := runtimeRecord{InstanceID: "instance-1", Token: "secret", Port: port, Executable: "/opt/iris", ConfigDir: "/data/iris"}
	if err := registerRuntimeRecord(dataDir, record); err != nil {
		t.Fatal(err)
	}
	originalLaunch := launchRuntimeProcess
	t.Cleanup(func() { launchRuntimeProcess = originalLaunch })
	var launched runtimeRecord
	launchRuntimeProcess = func(record runtimeRecord) error {
		launched = record
		instanceID = "instance-2"
		token = "new-secret"
		stopping = false
		return registerRuntimeRecord(dataDir, runtimeRecord{InstanceID: instanceID, Token: token, Port: port})
	}
	var output bytes.Buffer
	handled, err := handleServiceCommand([]string{"restart"}, strings.NewReader(""), &output, dataDir, false)
	if err != nil || !handled {
		t.Fatalf("restart handled=%v err=%v", handled, err)
	}
	if launched.Port != port || launched.Executable != "/opt/iris" || launched.ConfigDir != "/data/iris" {
		t.Fatalf("launched record = %#v", launched)
	}
	if !strings.Contains(output.String(), "Iris 服务已重启") {
		t.Fatalf("restart output = %q", output.String())
	}
}

func TestMigrateWaitingTransitionDefaults(t *testing.T) {
	migrated, changed := migrateWaitingTransitionDefaults(Config{
		FastWaitingTransitionMs:         500,
		ConservativeWaitingTransitionMs: 500,
	})
	if !changed || migrated.FastWaitingTransitionMs != 5000 || migrated.ConservativeWaitingTransitionMs != 5000 {
		t.Fatalf("old defaults were not migrated: changed=%v config=%#v", changed, migrated)
	}

	custom, changed := migrateWaitingTransitionDefaults(Config{
		FastWaitingTransitionMs:         450,
		ConservativeWaitingTransitionMs: 900,
	})
	if changed || custom.FastWaitingTransitionMs != 450 || custom.ConservativeWaitingTransitionMs != 900 {
		t.Fatalf("custom delays must be preserved: changed=%v config=%#v", changed, custom)
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

func TestAutoSelectFirstUseAgentPrefersCodexThenClaude(t *testing.T) {
	base := Config{LarkDefaultSessionName: "默认会话"}
	withBoth, changed := autoSelectFirstUseAgent(base, []session.AgentOption{
		{ID: "codex", Label: "Codex", Kind: "codex", Command: session.CodexAgentCommand},
		{ID: "claude", Label: "Claude Code", Kind: "claude", Command: session.ClaudeAgentCommand},
	})
	if !changed || withBoth.AgentName != "Codex" || withBoth.AgentKind != "codex" || withBoth.AgentCommand != session.CodexAgentCommand || !withBoth.OnboardingCompleted {
		t.Fatalf("Codex auto selection = %#v changed=%v", withBoth, changed)
	}
	withClaude, changed := autoSelectFirstUseAgent(base, []session.AgentOption{
		{ID: "claude", Label: "Claude Code", Kind: "claude", Command: session.ClaudeAgentCommand},
	})
	if !changed || withClaude.AgentName != "Claude Code" || withClaude.AgentKind != "claude" || withClaude.AgentCommand != session.ClaudeAgentCommand || !withClaude.OnboardingCompleted {
		t.Fatalf("Claude auto selection = %#v changed=%v", withClaude, changed)
	}
}

func TestAutoSelectFirstUseAgentKeepsExistingOrMissingConfiguration(t *testing.T) {
	existing := Config{AgentKind: "custom", AgentCommand: "my-agent", OnboardingCompleted: false}
	got, changed := autoSelectFirstUseAgent(existing, []session.AgentOption{{ID: "codex", Kind: "codex", Command: session.CodexAgentCommand}})
	if changed || got.AgentKind != "custom" || got.AgentCommand != "my-agent" || got.OnboardingCompleted {
		t.Fatalf("existing Agent was overwritten: %#v changed=%v", got, changed)
	}
	empty, changed := autoSelectFirstUseAgent(Config{}, nil)
	if changed || empty.AgentKind != "" || empty.OnboardingCompleted {
		t.Fatalf("missing Agent should keep onboarding: %#v changed=%v", empty, changed)
	}
}

func TestMigrateAgentDefinitionsResetsBuiltinCommand(t *testing.T) {
	cfg, changed := migrateAgentDefinitions(Config{AgentKind: "codex", AgentCommand: "codex --profile work"})
	if !changed || cfg.DefaultAgentID != "codex" || cfg.AgentCommand != session.CodexAgentCommand {
		t.Fatalf("migrated config = %#v changed=%v", cfg, changed)
	}
	if got := agentConfigByID(cfg.Agents, "codex").Command; got != session.CodexAgentCommand {
		t.Fatalf("Codex command = %q", got)
	}
	for id, want := range map[string]string{
		"aiden":        session.AidenAgentCommand,
		"aiden-codex":  session.AidenCodexAgentCommand,
		"aiden-claude": session.AidenClaudeAgentCommand,
	} {
		if got := agentConfigByID(cfg.Agents, id).Command; got != want {
			t.Fatalf("%s command = %q, want %q", id, got, want)
		}
	}
}

func withAgentExecutables(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

func TestValidateAgentDefinitionsDoesNotAllowBuiltinOverrides(t *testing.T) {
	withAgentExecutables(t, "codex")
	agents, err := validateAgentList([]session.AgentConfig{{ID: "codex", Name: "Changed", Kind: "custom", Command: "other"}})
	if err != nil {
		t.Fatal(err)
	}
	selected := agents[0]
	if selected.Name != "Codex" || selected.Kind != "codex" || selected.Command != session.CodexAgentCommand || agents[0] != selected {
		t.Fatalf("validated built-in = %#v, agents=%#v", selected, agents)
	}
}

func TestValidateAgentDefinitionsRecognizesAidenBuiltins(t *testing.T) {
	withAgentExecutables(t, "aiden", "codex", "claude")
	for _, test := range []struct {
		id      string
		name    string
		kind    string
		command string
	}{
		{id: "aiden", name: "Aiden", kind: "aiden", command: session.AidenAgentCommand},
		{id: "aiden-codex", name: "Aiden X Codex", kind: "aiden-codex", command: session.AidenCodexAgentCommand},
		{id: "aiden-claude", name: "Aiden X Claude Code", kind: "aiden-claude", command: session.AidenClaudeAgentCommand},
	} {
		agents, err := validateAgentList([]session.AgentConfig{{ID: test.id, Name: "Changed", Kind: "custom", Command: "other"}})
		if err != nil {
			t.Fatal(err)
		}
		selected := agents[0]
		if selected.Name != test.name || selected.Kind != test.kind || selected.Command != test.command || agents[0] != selected {
			t.Fatalf("validated %s = %#v, agents=%#v", test.id, selected, agents)
		}
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
	home := t.TempDir()
	dir := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("IRIS_HOME", "")
	t.Setenv("IRIS_CONFIG_DIR", dir)
	t.Setenv("EASY_TERMINAL_HOME", "")
	t.Setenv("EASY_TERMINAL_CONFIG_DIR", "")
	if got := defaultConfigPath(); got != filepath.Join(dir, "config.local.json") {
		t.Fatalf("default config path with config dir override = %q", got)
	}
	if got := defaultDBPath(); got != filepath.Join(home, ".iris", "iris.db") {
		t.Fatalf("default db path with config dir override = %q", got)
	}
	if got := defaultUploadsDir(); got != filepath.Join(home, ".iris", "data", "uploads") {
		t.Fatalf("default uploads dir with config dir override = %q", got)
	}
	if got := defaultLogDir(); got != filepath.Join(home, ".iris", "log") {
		t.Fatalf("default log dir with config dir override = %q", got)
	}
	if got := configPathFromDir(filepath.Join(dir, "custom")); got != filepath.Join(dir, "custom", "config.local.json") {
		t.Fatalf("config path from cli dir = %q", got)
	}
}

func TestPortsUseIsolatedRuntimeData(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("IRIS_HOME", "")
	t.Setenv("IRIS_CONFIG_DIR", "")
	t.Setenv("EASY_TERMINAL_HOME", "")
	t.Setenv("EASY_TERMINAL_CONFIG_DIR", "")
	base := filepath.Join(home, ".iris")
	if got := instanceDataDir(base, "8080"); got != base {
		t.Fatalf("default instance data dir = %q", got)
	}
	want := filepath.Join(base, "instances", "8081")
	if got := instanceDataDir(base, "8081"); got != want {
		t.Fatalf("secondary instance data dir = %q, want %q", got, want)
	}
	if got := configPathForDataDir("", want); got != filepath.Join(want, "conf", "config.local.json") {
		t.Fatalf("secondary config path = %q", got)
	}
	if got := dbPathInDataDir(want); got != filepath.Join(want, "iris.db") {
		t.Fatalf("secondary db path = %q", got)
	}
	if got := uploadsDirInDataDir(want); got != filepath.Join(want, "data", "uploads") {
		t.Fatalf("secondary uploads dir = %q", got)
	}
	if got := logDirInDataDir(want); got != filepath.Join(want, "log") {
		t.Fatalf("secondary log dir = %q", got)
	}
}

func TestEnterRuntimeDirUsesStableUserDataDir(t *testing.T) {
	home := t.TempDir()
	start := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	if err := os.Chdir(start); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("IRIS_HOME", "")
	t.Setenv("EASY_TERMINAL_HOME", "")

	want := filepath.Join(home, ".iris")
	got, err := enterRuntimeDir(want)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("runtime dir = %q, want %q", got, want)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	wdInfo, err := os.Stat(wd)
	if err != nil {
		t.Fatal(err)
	}
	wantInfo, err := os.Stat(want)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(wdInfo, wantInfo) {
		t.Fatalf("working dir = %q, want %q", wd, want)
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
	withAgentExecutables(t, "codex")
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
	defaultWorkspaceDir := filepath.Join(t.TempDir(), "nested", "workspace")

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
		DefaultWorkspaceDir:    defaultWorkspaceDir,
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
	if saved.DefaultWorkspaceDir != defaultWorkspaceDir || got.DefaultWorkspaceDir != defaultWorkspaceDir {
		t.Fatalf("default workspace was not persisted: saved=%q got=%q", saved.DefaultWorkspaceDir, got.DefaultWorkspaceDir)
	}
	if info, statErr := os.Stat(defaultWorkspaceDir); statErr != nil || !info.IsDir() {
		t.Fatalf("default workspace was not created: %v", statErr)
	}
}

func TestAppConfigServiceRequiresAndPublishesCustomAgentName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.local.json")
	cfg := defaultConfig()
	mgr := session.NewManager(nil, nil)
	svc := &appConfigService{path: path, cfg: &cfg, manager: mgr}
	req := httpapi.RuntimeConfig{
		FastWaitingTransitionMs: 1, ConservativeWaitingTransitionMs: 1,
		LarkAutoRefreshIntervalMs: 1, HeadlessSnapshotTimeoutMs: 1,
		LarkNotifyMaxLines: 1, LarkNotifyFallbackTailLines: 1,
		AgentKind: "custom", AgentCommand: "my-agent --yolo",
	}
	if _, err := svc.UpdateRuntimeConfig(req); err == nil || !strings.Contains(err.Error(), "名称") {
		t.Fatalf("missing custom Agent name error = %v", err)
	}
	req.AgentName = "方案助手"
	got, err := svc.UpdateRuntimeConfig(req)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentName != "方案助手" || cfg.AgentName != "方案助手" {
		t.Fatalf("custom Agent name was not persisted: got=%#v cfg=%#v", got, cfg)
	}
	options := mgr.AvailableAgentOptions()
	found := false
	for _, option := range options {
		if option.Kind == "custom" && option.Label == "方案助手" && option.Command == "my-agent --yolo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("custom Agent was not published to switch options: %#v", options)
	}
}
