package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/elevenlj/iris/internal/httpapi"
	"github.com/elevenlj/iris/internal/session"
	"github.com/elevenlj/iris/internal/store"
)

var version = "dev"

const (
	defaultLarkDefaultSessionName          = "默认会话"
	defaultLarkSessionChatPrefix           = "Iris · "
	defaultLarkIgnoreMessagePrefix         = "/i"
	defaultLarkAutoSummaryPrompt           = session.DefaultLarkAutoSummaryPrompt
	defaultFastWaitingTransitionMs         = 500
	defaultConservativeWaitingTransitionMs = 500
	defaultLarkAutoRefreshIntervalMs       = 5000
	defaultHeadlessSnapshotTimeoutMs       = 10000
	defaultLarkNotifyMaxLines              = 200
	defaultLarkNotifyFallbackTailLines     = 100
	runtimeLogicVersion                    = "card-refresh-no-running-patch-v2"
)

var defaultLarkNotifyDropLineRules = session.LarkNotifyDropLineRules{
	{Title: "空行", Pattern: `^\s*$`},
	{Title: "横线", Pattern: `^\s*[-‐-‒–—―─━═]{3,}\s*$`},
	{Title: "Codex 工具执行过程", Pattern: `^\s*[•⏺]\s+(?:Ran|Waited|Explored|Edited?|Wrote|Read|Searched|Listed|Fetched|Viewed|Inspected|Created|Deleted|Moved|Copied|Executed|Called|Opened|Downloaded|Uploaded|Patched|Applied|Bash)\b.*`, Kind: "block_head", Action: "drop_block"},
	{Pattern: `^(.*)Worked for.*?(─+)$`},
	{Pattern: `^─{1,}.*`},
	{Pattern: `^\s*✶\s+Generating\b.*`, Kind: "block_head", Action: "drop_block"},
	{Pattern: `^[╭╮╰╯│─▐▛▜▝▘ ]+$`},
	{Pattern: `^\s*[▐▛▜▝▘█▙▟▚▞]+\s*$`},
	{Pattern: `[╭╮╰╯│─]`},
	{Pattern: `^\s*$`},
	{Pattern: `^(Welcome back|Tips|What's new|Run |\/usage|\/diff|\/release-notes|deepseek-v4-pro|~\/)`},
	{Title: "Codex 启动提示与 MCP 错误", Pattern: `^\s*(?:Tip:|⚠ MCP (?:client.*failed to start|startup incomplete)).*`, Kind: "block_head", Action: "drop_block"},
}

type Config struct {
	Port                            string                                `json:"port"`
	LarkAppID                       string                                `json:"lark_app_id"`
	LarkAppSecret                   string                                `json:"lark_app_secret"`
	LarkNotifyReceiveID             string                                `json:"lark_notify_receive_id"`
	LarkMentionEnabled              bool                                  `json:"lark_mention_enabled"`
	LarkDefaultSessionName          string                                `json:"lark_default_session_name"`
	LarkSessionChatPrefix           string                                `json:"lark_session_chat_prefix"`
	LarkIgnoreMessagePrefix         string                                `json:"lark_ignore_message_prefix"`
	LarkAutoSummaryPrompt           string                                `json:"lark_auto_summary_prompt"`
	FastWaitingTransitionMs         int                                   `json:"fast_waiting_transition_ms"`
	ConservativeWaitingTransitionMs int                                   `json:"conservative_waiting_transition_ms"`
	LarkAutoRefreshIntervalMs       int                                   `json:"lark_auto_refresh_interval_ms"`
	HeadlessSnapshotTimeoutMs       int                                   `json:"headless_snapshot_timeout_ms"`
	LarkNotifyMaxLines              int                                   `json:"lark_notify_max_lines"`
	LarkNotifyFallbackTailLines     int                                   `json:"lark_notify_fallback_tail_lines"`
	LarkNotifyMergeWrappedLines     bool                                  `json:"lark_notify_merge_wrapped_lines"`
	LarkNotifyDropLineRules         session.LarkNotifyDropLineRules       `json:"lark_notify_drop_line_patterns"`
	SessionPreStartCommand          string                                `json:"session_pre_start_command"`
	SessionStartPresets             map[string]session.SessionStartPreset `json:"session_start_presets"`
	SessionNamePresets              map[string]session.SessionStartPreset `json:"session_name_presets"`
	LarkCustomShortcuts             []session.LarkCustomShortcut          `json:"lark_custom_shortcuts"`
	OnboardingCompleted             bool                                  `json:"onboarding_completed"`
	AgentName                       string                                `json:"agent_name"`
	AgentKind                       string                                `json:"agent_kind"`
	AgentCommand                    string                                `json:"agent_command"`
	WorkspaceOptions                []session.WorkspaceOption             `json:"workspace_options"`
	SettingsPasswordHash            string                                `json:"settings_password_hash,omitempty"`
	SettingsPasswordSkipped         bool                                  `json:"settings_password_skipped,omitempty"`
	SettingsAuthVersion             int64                                 `json:"settings_auth_version,omitempty"`
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if session.IsCodexNotifyInvocation(os.Args[1:]) {
		if err := session.RunCodexNotify(os.Args[2:]); err != nil {
			log.Printf("Codex notify callback failed: %v", err)
		}
		return nil
	}
	if session.IsClaudeStopInvocation(os.Args[1:]) {
		if err := session.RunClaudeStopHook(os.Stdin); err != nil {
			log.Printf("Claude Stop callback failed: %v", err)
		}
		return nil
	}
	opts, err := parseStartupOptions(os.Args[1:])
	if err != nil {
		return err
	}
	if opts.Version {
		fmt.Printf("iris %s\n", version)
		return nil
	}
	if opts.InstallAgentHooks {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		if err := session.EnsureAgentCompletionHooks(executable); err != nil {
			return err
		}
		fmt.Println("Iris Agent hooks and Feishu context skills installed.")
		return nil
	}
	configPath := configPathFromDir(opts.ConfigDir)
	if err := ensureConfigFile(configPath); err != nil {
		return err
	}
	cfg := loadConfig(configPath)
	cfg, permissionModeChanged := enforceAgentPermissionMode(cfg)
	updated, autoSelected := autoSelectFirstUseAgent(cfg, session.DetectAvailableAgentOptions(session.AgentConfig{}))
	if autoSelected {
		cfg = updated
	}
	if permissionModeChanged || autoSelected {
		if err := writeConfigFile(configPath, cfg); err != nil {
			return err
		}
	}
	if opts.ResetSettingsPassword {
		cfg.SettingsPasswordHash = ""
		cfg.SettingsPasswordSkipped = false
		cfg.SettingsAuthVersion++
		if err := writeConfigFile(configPath, cfg); err != nil {
			return err
		}
		fmt.Println("Iris 设置密码已重置，请重新打开设置页完成安全初始化。")
		return nil
	}
	if opts.Port != "" {
		cfg.Port = opts.Port
	}
	dataDir := dataDirFromConfigDir(opts.ConfigDir)
	dbPath := env("AGENT_MONITOR_DB", dbPathInDataDir(dataDir))
	uploadsDir := env("AGENT_MONITOR_UPLOADS_DIR", uploadsDirInDataDir(dataDir))
	logDir := env("AGENT_MONITOR_LOG_DIR", logDirInDataDir(dataDir))
	_ = os.MkdirAll(filepath.Dir(dbPath), 0o755)
	_ = os.MkdirAll(uploadsDir, 0o755)
	_ = os.MkdirAll(logDir, 0o755)
	logFile, err := os.OpenFile(filepath.Join(logDir, "iris.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("failed to open log file: %v", err)
	} else {
		defer logFile.Close()
		log.SetOutput(io.MultiWriter(os.Stderr, logFile))
		log.Printf("logging to %s", filepath.Join(logDir, "iris.log"))
	}
	wd, _ := os.Getwd()
	log.Printf("iris runtime_logic=%s pid=%d cwd=%s", runtimeLogicVersion, os.Getpid(), wd)
	executable, executableErr := os.Executable()
	if executableErr != nil {
		log.Printf("failed to resolve Iris executable for Codex notify: %v", executableErr)
	} else if err := session.EnsureAgentCompletionHooks(executable); err != nil {
		log.Printf("failed to install Agent completion hooks: %v", err)
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	if sessions, listErr := st.ListSessions(context.Background()); listErr != nil {
		log.Printf("startup Agent upgrade skipped: cannot list sessions: %v", listErr)
	} else {
		configuredAgent := session.AgentConfig{Name: cfg.AgentName, Kind: cfg.AgentKind, Command: cfg.AgentCommand}
		for _, result := range session.UpgradeAgentCLIsOnStartup(context.Background(), sessions, configuredAgent) {
			if result.Err != nil {
				log.Printf("startup Agent upgrade failed agent=%s: %v output=%q", result.Kind, result.Err, result.Output)
				continue
			}
			log.Printf("startup Agent upgrade completed agent=%s before=%q after=%q", result.Kind, result.BeforeVersion, result.AfterVersion)
		}
	}

	notifier := session.NewLarkAppNotifier(cfg.LarkAppID, cfg.LarkAppSecret, cfg.LarkNotifyReceiveID, cfg.LarkMentionEnabled)
	notifier.SetCustomShortcuts(cfg.LarkCustomShortcuts)
	headless := newHeadlessBrowserManager(cfg.Port)
	defer headless.StopAll()
	mgr := session.NewManager(
		st,
		session.ShellLauncher{},
		session.WithNotifier(notifier),
		session.WithWaitingTransitionDelays(
			time.Duration(cfg.FastWaitingTransitionMs)*time.Millisecond,
			time.Duration(cfg.ConservativeWaitingTransitionMs)*time.Millisecond,
		),
		session.WithAutoRefreshInterval(time.Duration(cfg.LarkAutoRefreshIntervalMs)*time.Millisecond),
		session.WithHeadlessSnapshotTimeout(time.Duration(cfg.HeadlessSnapshotTimeoutMs)*time.Millisecond),
		session.WithBrowserNeeded(headless.Ensure),
		session.WithBrowserActive(headless.Stop),
		session.WithBrowserStopped(headless.Stop),
		session.WithPreStartCommand(cfg.SessionPreStartCommand),
		session.WithRecoveryBaseDir(filepath.Join(dataDir, "data", "sessions")),
		session.WithAgentTurnHookURL("http://127.0.0.1:"+cfg.Port),
		session.WithSessionEnded(func(sessionID string) {
			headless.Stop(sessionID)
			_ = os.RemoveAll(filepath.Join(uploadsDir, sessionID))
		}),
	)
	mgr.SetAgentConfig(session.AgentConfig{Name: cfg.AgentName, Kind: cfg.AgentKind, Command: cfg.AgentCommand}, cfg.WorkspaceOptions)
	mgr.SetAvailableAgentOptions(session.DetectAvailableAgentOptions(session.AgentConfig{Name: cfg.AgentName, Kind: cfg.AgentKind, Command: cfg.AgentCommand}))

	bridge := session.NewLarkReplyBridge(cfg.LarkAppID, cfg.LarkAppSecret, mgr, uploadsDir)
	bridge.SetDefaultStartSessionName(cfg.LarkDefaultSessionName)
	bridge.SetSessionChatPrefix(cfg.LarkSessionChatPrefix)
	bridge.SetIgnoreMessagePrefix(cfg.LarkIgnoreMessagePrefix)
	bridge.SetAutoSummaryPrompt(cfg.LarkAutoSummaryPrompt)
	bridge.SetStartPresets(cfg.SessionStartPresets)
	bridge.SetNamePresets(cfg.SessionNamePresets)
	bridge.SetCustomShortcuts(cfg.LarkCustomShortcuts)
	bridge.SetDeveloperOpenID(cfg.LarkNotifyReceiveID)
	bridge.SetWorkspaceOptions(cfg.WorkspaceOptions)
	if bridge.Available() {
		go func() {
			if err := bridge.Start(context.Background()); err != nil {
				log.Printf("lark reply bridge stopped: %v", err)
			}
		}()
	}

	configSvc := &appConfigService{path: configPath, cfg: &cfg, manager: mgr, bridge: bridge}
	srv := httpapi.NewServer(mgr, uploadsDir, configSvc)
	addr := ":" + cfg.Port
	log.Printf("iris listening on http://localhost%s", addr)
	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler()}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- httpSrv.Serve(listener)
	}()
	if !envBool("IRIS_NO_OPEN", false) {
		go func() {
			settingsURL := "http://localhost:" + listenerPort(listener.Addr(), cfg.Port) + "/?settings=1"
			if err := openBrowserURL(settingsURL); err != nil {
				log.Printf("failed to open Iris configuration page: %v", err)
			}
		}()
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, interruptSignals()...)
	defer signal.Stop(sigCh)
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case sig := <-sigCh:
		log.Printf("iris stopping on signal %s", sig)
		headless.StopAll()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			return err
		}
		return nil
	}
}

func listenerPort(addr net.Addr, fallback string) string {
	if tcp, ok := addr.(*net.TCPAddr); ok && tcp.Port > 0 {
		return strconv.Itoa(tcp.Port)
	}
	return strings.TrimSpace(fallback)
}

func openBrowserURL(target string) error {
	name, args, err := browserOpenCommand(runtime.GOOS, target)
	if err != nil {
		return err
	}
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func browserOpenCommand(goos, target string) (string, []string, error) {
	switch goos {
	case "darwin":
		return "open", []string{target}, nil
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", target}, nil
	case "linux":
		return "xdg-open", []string{target}, nil
	default:
		return "", nil, fmt.Errorf("browser auto-open is unsupported on %s", goos)
	}
}

type startupOptions struct {
	Port                  string
	ConfigDir             string
	Version               bool
	ResetSettingsPassword bool
	InstallAgentHooks     bool
}

func parseStartupOptions(args []string) (startupOptions, error) {
	var opts startupOptions
	fs := flag.NewFlagSet("iris", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.Port, "port", "", "HTTP listen port")
	fs.StringVar(&opts.Port, "p", "", "HTTP listen port")
	fs.StringVar(&opts.ConfigDir, "config-dir", "", "config file directory")
	fs.BoolVar(&opts.Version, "version", false, "print version")
	fs.BoolVar(&opts.Version, "v", false, "print version")
	fs.BoolVar(&opts.ResetSettingsPassword, "reset-settings-password", false, "reset the local settings password")
	fs.BoolVar(&opts.InstallAgentHooks, "install-agent-hooks", false, "install Codex and Claude completion hooks")
	if err := fs.Parse(args); err != nil {
		return startupOptions{}, err
	}
	opts.ConfigDir = strings.TrimSpace(opts.ConfigDir)
	if fs.NArg() > 0 {
		return startupOptions{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	return opts, nil
}

func loadConfig(path string) Config {
	cfg := defaultConfig()
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	cfg.Port = env("PORT", cfg.Port)
	cfg.LarkAppID = env("LARK_APP_ID", cfg.LarkAppID)
	cfg.LarkAppSecret = env("LARK_APP_SECRET", cfg.LarkAppSecret)
	cfg.LarkNotifyReceiveID = env("LARK_NOTIFY_RECEIVE_ID", cfg.LarkNotifyReceiveID)
	cfg.LarkDefaultSessionName = env("LARK_DEFAULT_SESSION_NAME", cfg.LarkDefaultSessionName)
	cfg.LarkSessionChatPrefix = env("LARK_SESSION_CHAT_PREFIX", cfg.LarkSessionChatPrefix)
	cfg.LarkIgnoreMessagePrefix = env("LARK_IGNORE_MESSAGE_PREFIX", cfg.LarkIgnoreMessagePrefix)
	cfg.LarkAutoSummaryPrompt = env("LARK_AUTO_SUMMARY_PROMPT", cfg.LarkAutoSummaryPrompt)
	cfg.SessionPreStartCommand = env("SESSION_PRE_START_COMMAND", cfg.SessionPreStartCommand)
	if v := os.Getenv("LARK_MENTION_ENABLED"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			cfg.LarkMentionEnabled = parsed
		}
	}
	if v := os.Getenv("LARK_NOTIFY_MERGE_WRAPPED_LINES"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			cfg.LarkNotifyMergeWrappedLines = parsed
		}
	}
	if v := os.Getenv("HEADLESS_SNAPSHOT_TIMEOUT_MS"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			cfg.HeadlessSnapshotTimeoutMs = parsed
		}
	}
	if cfg.FastWaitingTransitionMs <= 0 {
		cfg.FastWaitingTransitionMs = defaultFastWaitingTransitionMs
	}
	if cfg.ConservativeWaitingTransitionMs <= 0 {
		cfg.ConservativeWaitingTransitionMs = defaultConservativeWaitingTransitionMs
	}
	if cfg.LarkAutoRefreshIntervalMs <= 0 {
		cfg.LarkAutoRefreshIntervalMs = defaultLarkAutoRefreshIntervalMs
	}
	if cfg.HeadlessSnapshotTimeoutMs <= 0 {
		cfg.HeadlessSnapshotTimeoutMs = defaultHeadlessSnapshotTimeoutMs
	}
	if cfg.LarkNotifyMaxLines <= 0 {
		cfg.LarkNotifyMaxLines = defaultLarkNotifyMaxLines
	}
	if cfg.LarkNotifyFallbackTailLines <= 0 {
		cfg.LarkNotifyFallbackTailLines = defaultLarkNotifyFallbackTailLines
	}
	if strings.TrimSpace(cfg.LarkAutoSummaryPrompt) == "" {
		cfg.LarkAutoSummaryPrompt = defaultLarkAutoSummaryPrompt
	}
	if strings.TrimSpace(cfg.AgentKind) == "" || strings.TrimSpace(cfg.AgentCommand) == "" {
		if preset, ok := cfg.SessionStartPresets["999999"]; ok && len(preset.Commands) > 0 {
			cfg.AgentCommand = strings.TrimSpace(preset.Commands[len(preset.Commands)-1])
			if strings.Contains(strings.ToLower(cfg.AgentCommand), "codex") {
				cfg.AgentKind = "codex"
			} else if cfg.AgentCommand != "" {
				cfg.AgentKind = "custom"
			}
		}
	}
	switch strings.ToLower(strings.TrimSpace(cfg.AgentKind)) {
	case "codex":
		cfg.AgentName = "Codex"
	case "claude":
		cfg.AgentName = "Claude Code"
	case "custom":
		if strings.TrimSpace(cfg.AgentName) == "" {
			cfg.AgentName = "自定义 Agent"
		}
	}
	if cfg.SettingsAuthVersion <= 0 {
		cfg.SettingsAuthVersion = 1
	}
	cfg.LarkNotifyDropLineRules = mergeRequiredLarkNotifyDropLineRules(cfg.LarkNotifyDropLineRules)
	cfg.LarkSessionChatPrefix = normalizeLarkSessionChatPrefix(cfg.LarkSessionChatPrefix)
	session.SetLarkNotifyMaxLines(cfg.LarkNotifyMaxLines)
	session.SetLarkNotifyFallbackTailLines(cfg.LarkNotifyFallbackTailLines)
	session.SetLarkNotifyMergeWrappedLines(cfg.LarkNotifyMergeWrappedLines)
	if err := session.SetLarkNotifyDropLineRules(cfg.LarkNotifyDropLineRules.Rules()); err != nil {
		log.Printf("invalid lark_notify_drop_line_patterns: %v", err)
	}
	return cfg
}

func autoSelectFirstUseAgent(cfg Config, options []session.AgentOption) (Config, bool) {
	if strings.TrimSpace(cfg.AgentKind) != "" && strings.TrimSpace(cfg.AgentCommand) != "" {
		return cfg, false
	}
	for _, option := range options {
		if option.Kind != "codex" && option.Kind != "claude" {
			continue
		}
		cfg.AgentKind = option.Kind
		cfg.AgentName = option.Label
		cfg.AgentCommand = option.Command
		cfg.OnboardingCompleted = true
		return cfg, true
	}
	return cfg, false
}

func enforceAgentPermissionMode(cfg Config) (Config, bool) {
	kind := strings.ToLower(strings.TrimSpace(cfg.AgentKind))
	want := ""
	wantName := ""
	switch kind {
	case "codex":
		want = session.CodexAgentCommand
		wantName = "Codex"
	case "claude":
		want = session.ClaudeAgentCommand
		wantName = "Claude Code"
	default:
		return cfg, false
	}
	if strings.TrimSpace(cfg.AgentCommand) == want && cfg.AgentKind == kind && strings.TrimSpace(cfg.AgentName) == wantName {
		return cfg, false
	}
	cfg.AgentName = wantName
	cfg.AgentKind = kind
	cfg.AgentCommand = want
	return cfg, true
}

// mergeRequiredLarkNotifyDropLineRules keeps existing user rules while making
// the Codex tool-output filters available to installations that already had a
// config.local.json before those defaults were introduced. Required rules are
// prepended so obsolete keep_head variants cannot override them.
func mergeRequiredLarkNotifyDropLineRules(current session.LarkNotifyDropLineRules) session.LarkNotifyDropLineRules {
	required := []session.LarkNotifyDropLineRule{
		defaultLarkNotifyDropLineRules[2],
		defaultLarkNotifyDropLineRules[5],
		defaultLarkNotifyDropLineRules[11],
	}
	merged := make([]session.LarkNotifyDropLineRule, 0, len(current)+len(required))
	existing := current.Rules()
	for _, requiredRule := range required {
		found := false
		for _, rule := range existing {
			if sameLarkNotifyDropLineRule(rule, requiredRule) {
				found = true
				break
			}
		}
		if !found {
			merged = append(merged, requiredRule)
		}
	}
	merged = append(merged, existing...)
	return session.LarkNotifyDropLineRules(merged)
}

func sameLarkNotifyDropLineRule(left, right session.LarkNotifyDropLineRule) bool {
	leftRules := session.NormalizeLarkNotifyDropLineRules([]session.LarkNotifyDropLineRule{left})
	rightRules := session.NormalizeLarkNotifyDropLineRules([]session.LarkNotifyDropLineRule{right})
	if len(leftRules) != 1 || len(rightRules) != 1 {
		return false
	}
	left, right = leftRules[0], rightRules[0]
	return left.Pattern == right.Pattern && left.Kind == right.Kind && left.Action == right.Action
}

func defaultConfig() Config {
	return Config{
		Port:                            "8080",
		LarkMentionEnabled:              true,
		LarkDefaultSessionName:          defaultLarkDefaultSessionName,
		LarkSessionChatPrefix:           defaultLarkSessionChatPrefix,
		LarkIgnoreMessagePrefix:         defaultLarkIgnoreMessagePrefix,
		LarkAutoSummaryPrompt:           defaultLarkAutoSummaryPrompt,
		FastWaitingTransitionMs:         defaultFastWaitingTransitionMs,
		ConservativeWaitingTransitionMs: defaultConservativeWaitingTransitionMs,
		LarkAutoRefreshIntervalMs:       defaultLarkAutoRefreshIntervalMs,
		HeadlessSnapshotTimeoutMs:       defaultHeadlessSnapshotTimeoutMs,
		LarkNotifyMaxLines:              defaultLarkNotifyMaxLines,
		LarkNotifyFallbackTailLines:     defaultLarkNotifyFallbackTailLines,
		LarkNotifyMergeWrappedLines:     true,
		LarkNotifyDropLineRules:         defaultLarkNotifyDropLineRules.Rules(),
	}
}

func ensureConfigFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return writeConfigFile(path, defaultConfig())
}

func defaultConfigPath() string {
	return filepath.Join(defaultConfigDir(), "config.local.json")
}

func configPathFromDir(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return defaultConfigPath()
	}
	return filepath.Join(dir, "config.local.json")
}

func defaultConfigDir() string {
	if dir := strings.TrimSpace(os.Getenv("IRIS_CONFIG_DIR")); dir != "" {
		return dir
	}
	if dir := strings.TrimSpace(os.Getenv("EASY_TERMINAL_CONFIG_DIR")); dir != "" {
		return dir
	}
	return filepath.Join(defaultDataDir(), "conf")
}

func defaultDBPath() string {
	return dbPathInDataDir(defaultDataDir())
}

func defaultUploadsDir() string {
	return uploadsDirInDataDir(defaultDataDir())
}

func defaultLogDir() string {
	return logDirInDataDir(defaultDataDir())
}

func dbPathInDataDir(dir string) string {
	return filepath.Join(dir, "iris.db")
}

func uploadsDirInDataDir(dir string) string {
	return filepath.Join(dir, "data", "uploads")
}

func logDirInDataDir(dir string) string {
	return filepath.Join(dir, "log")
}

func dataDirFromConfigDir(dir string) string {
	if dir := strings.TrimSpace(dir); dir != "" {
		return dir
	}
	return defaultDataDir()
}

func defaultDataDir() string {
	if dir := strings.TrimSpace(os.Getenv("IRIS_HOME")); dir != "" {
		return dir
	}
	if dir := strings.TrimSpace(os.Getenv("EASY_TERMINAL_HOME")); dir != "" {
		return dir
	}
	if dir := strings.TrimSpace(os.Getenv("IRIS_CONFIG_DIR")); dir != "" {
		return dir
	}
	if dir := strings.TrimSpace(os.Getenv("EASY_TERMINAL_CONFIG_DIR")); dir != "" {
		return dir
	}
	if dir, err := os.UserHomeDir(); err == nil && dir != "" {
		return filepath.Join(dir, ".iris")
	}
	return ".iris"
}

type appConfigService struct {
	mu      sync.Mutex
	path    string
	cfg     *Config
	manager *session.Manager
	bridge  *session.LarkReplyBridge
}

func (s *appConfigService) RuntimeConfig() httpapi.RuntimeConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return runtimeConfigFromConfig(*s.cfg)
}

func (s *appConfigService) SettingsSecurity() httpapi.SettingsSecurity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return httpapi.SettingsSecurity{
		PasswordHash: s.cfg.SettingsPasswordHash,
		Skipped:      s.cfg.SettingsPasswordSkipped,
		AuthVersion:  s.cfg.SettingsAuthVersion,
	}
}

func (s *appConfigService) UpdateSettingsSecurity(security httpapi.SettingsSecurity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := *s.cfg
	cfg.SettingsPasswordHash = strings.TrimSpace(security.PasswordHash)
	cfg.SettingsPasswordSkipped = security.Skipped
	cfg.SettingsAuthVersion = security.AuthVersion
	if cfg.SettingsAuthVersion <= 0 {
		cfg.SettingsAuthVersion = 1
	}
	if err := writeConfigFile(s.path, cfg); err != nil {
		return err
	}
	*s.cfg = cfg
	return nil
}

func (s *appConfigService) UpdateRuntimeConfig(req httpapi.RuntimeConfig) (httpapi.RuntimeConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	oldCfg := *s.cfg
	cfg := *s.cfg
	if req.HeadlessSnapshotTimeoutMs <= 0 {
		req.HeadlessSnapshotTimeoutMs = cfg.HeadlessSnapshotTimeoutMs
		if req.HeadlessSnapshotTimeoutMs <= 0 {
			req.HeadlessSnapshotTimeoutMs = defaultHeadlessSnapshotTimeoutMs
		}
	}
	if req.FastWaitingTransitionMs <= 0 || req.ConservativeWaitingTransitionMs <= 0 || req.LarkAutoRefreshIntervalMs <= 0 || req.HeadlessSnapshotTimeoutMs <= 0 || req.LarkNotifyMaxLines <= 0 || req.LarkNotifyFallbackTailLines <= 0 {
		return httpapi.RuntimeConfig{}, errors.New("numeric settings must be greater than zero")
	}
	if req.SessionStartPresets == nil {
		req.SessionStartPresets = map[string]session.SessionStartPreset{}
	}
	if req.SessionNamePresets == nil {
		req.SessionNamePresets = map[string]session.SessionStartPreset{}
	}
	req.AgentKind = strings.ToLower(strings.TrimSpace(req.AgentKind))
	req.AgentName = strings.TrimSpace(req.AgentName)
	req.AgentCommand = strings.TrimSpace(req.AgentCommand)
	if req.AgentKind == "codex" {
		req.AgentName = "Codex"
		req.AgentCommand = session.CodexAgentCommand
	}
	if req.AgentKind == "claude" {
		req.AgentName = "Claude Code"
		req.AgentCommand = session.ClaudeAgentCommand
	}
	if req.AgentKind != "codex" && req.AgentKind != "claude" && req.AgentKind != "custom" {
		return httpapi.RuntimeConfig{}, errors.New("必须选择 Codex、Claude Code 或自定义 Agent")
	}
	if req.AgentCommand == "" {
		return httpapi.RuntimeConfig{}, errors.New("Agent 启动命令不能为空")
	}
	if req.AgentKind == "custom" {
		if req.AgentName == "" {
			return httpapi.RuntimeConfig{}, errors.New("自定义 Agent 名称不能为空")
		}
		if len([]rune(req.AgentName)) > 40 {
			return httpapi.RuntimeConfig{}, errors.New("自定义 Agent 名称不能超过 40 个字符")
		}
	}
	workspaces, err := validateWorkspaceOptions(req.WorkspaceOptions)
	if err != nil {
		return httpapi.RuntimeConfig{}, err
	}
	cfg.LarkAppID = req.LarkAppID
	cfg.LarkAppSecret = req.LarkAppSecret
	cfg.LarkNotifyReceiveID = req.LarkNotifyReceiveID
	cfg.LarkMentionEnabled = req.LarkMentionEnabled
	cfg.LarkDefaultSessionName = req.LarkDefaultSessionName
	cfg.LarkSessionChatPrefix = normalizeLarkSessionChatPrefix(req.LarkSessionChatPrefix)
	cfg.LarkIgnoreMessagePrefix = strings.TrimSpace(req.LarkIgnoreMessagePrefix)
	cfg.LarkAutoSummaryPrompt = normalizeLarkAutoSummaryPrompt(req.LarkAutoSummaryPrompt)
	cfg.FastWaitingTransitionMs = req.FastWaitingTransitionMs
	cfg.ConservativeWaitingTransitionMs = req.ConservativeWaitingTransitionMs
	cfg.LarkAutoRefreshIntervalMs = req.LarkAutoRefreshIntervalMs
	cfg.HeadlessSnapshotTimeoutMs = req.HeadlessSnapshotTimeoutMs
	cfg.LarkNotifyMaxLines = req.LarkNotifyMaxLines
	cfg.LarkNotifyFallbackTailLines = req.LarkNotifyFallbackTailLines
	cfg.LarkNotifyMergeWrappedLines = req.LarkNotifyMergeWrappedLines
	cfg.LarkNotifyDropLineRules = req.LarkNotifyDropLineRules
	cfg.SessionPreStartCommand = req.SessionPreStartCommand
	cfg.SessionStartPresets = req.SessionStartPresets
	cfg.SessionNamePresets = req.SessionNamePresets
	cfg.LarkCustomShortcuts = req.LarkCustomShortcuts
	cfg.OnboardingCompleted = req.OnboardingCompleted
	cfg.AgentName = req.AgentName
	cfg.AgentKind = req.AgentKind
	cfg.AgentCommand = req.AgentCommand
	cfg.WorkspaceOptions = workspaces
	reconnectLark := oldCfg.LarkAppID != cfg.LarkAppID || oldCfg.LarkAppSecret != cfg.LarkAppSecret
	if err := applyRuntimeConfig(cfg, s.manager, s.bridge, reconnectLark); err != nil {
		return httpapi.RuntimeConfig{}, err
	}
	if err := writeConfigFile(s.path, cfg); err != nil {
		return httpapi.RuntimeConfig{}, err
	}
	*s.cfg = cfg
	return runtimeConfigFromConfig(cfg), nil
}

func validateWorkspaceOptions(options []session.WorkspaceOption) ([]session.WorkspaceOption, error) {
	out := make([]session.WorkspaceOption, 0, len(options))
	labels := map[string]bool{}
	paths := map[string]bool{}
	for _, option := range options {
		option.Label = strings.TrimSpace(option.Label)
		option.Value = strings.TrimSpace(option.Value)
		if option.Label == "" || option.Value == "" {
			return nil, errors.New("工作目录名称和路径不能为空")
		}
		if !filepath.IsAbs(option.Value) {
			return nil, errors.New("工作目录必须使用绝对路径")
		}
		info, statErr := os.Stat(option.Value)
		if statErr != nil || !info.IsDir() {
			return nil, fmt.Errorf("工作目录不存在或不可访问：%s", option.Value)
		}
		if labels[option.Label] || paths[option.Value] {
			return nil, errors.New("工作目录名称和路径不能重复")
		}
		labels[option.Label] = true
		paths[option.Value] = true
		option.Default = false
		out = append(out, option)
	}
	return out, nil
}

func applyRuntimeConfig(cfg Config, manager *session.Manager, bridge *session.LarkReplyBridge, reconnectLark bool) error {
	manager.SetWaitingTransitionDelays(time.Duration(cfg.FastWaitingTransitionMs)*time.Millisecond, time.Duration(cfg.ConservativeWaitingTransitionMs)*time.Millisecond)
	manager.SetAutoRefreshInterval(time.Duration(cfg.LarkAutoRefreshIntervalMs) * time.Millisecond)
	manager.SetHeadlessSnapshotTimeout(time.Duration(cfg.HeadlessSnapshotTimeoutMs) * time.Millisecond)
	manager.SetPreStartCommand(cfg.SessionPreStartCommand)
	manager.SetAgentConfig(session.AgentConfig{Name: cfg.AgentName, Kind: cfg.AgentKind, Command: cfg.AgentCommand}, cfg.WorkspaceOptions)
	manager.SetAvailableAgentOptions(session.DetectAvailableAgentOptions(session.AgentConfig{Name: cfg.AgentName, Kind: cfg.AgentKind, Command: cfg.AgentCommand}))
	notifier := session.NewLarkAppNotifier(cfg.LarkAppID, cfg.LarkAppSecret, cfg.LarkNotifyReceiveID, cfg.LarkMentionEnabled)
	notifier.SetCustomShortcuts(cfg.LarkCustomShortcuts)
	manager.SetNotifier(notifier)
	session.SetLarkNotifyMaxLines(cfg.LarkNotifyMaxLines)
	session.SetLarkNotifyFallbackTailLines(cfg.LarkNotifyFallbackTailLines)
	session.SetLarkNotifyMergeWrappedLines(cfg.LarkNotifyMergeWrappedLines)
	if err := session.SetLarkNotifyDropLineRules(cfg.LarkNotifyDropLineRules.Rules()); err != nil {
		return err
	}
	if bridge != nil {
		if reconnectLark {
			bridge.Stop()
			bridge.SetAppCredentials(cfg.LarkAppID, cfg.LarkAppSecret)
		}
		bridge.SetDefaultStartSessionName(cfg.LarkDefaultSessionName)
		bridge.SetSessionChatPrefix(cfg.LarkSessionChatPrefix)
		bridge.SetIgnoreMessagePrefix(cfg.LarkIgnoreMessagePrefix)
		bridge.SetAutoSummaryPrompt(cfg.LarkAutoSummaryPrompt)
		bridge.SetStartPresets(cfg.SessionStartPresets)
		bridge.SetNamePresets(cfg.SessionNamePresets)
		bridge.SetCustomShortcuts(cfg.LarkCustomShortcuts)
		bridge.SetDeveloperOpenID(cfg.LarkNotifyReceiveID)
		bridge.SetWorkspaceOptions(cfg.WorkspaceOptions)
		if reconnectLark && bridge.Available() {
			go func() {
				if err := bridge.Start(context.Background()); err != nil {
					log.Printf("lark reply bridge stopped: %v", err)
				}
			}()
		}
	}
	return nil
}

func runtimeConfigFromConfig(cfg Config) httpapi.RuntimeConfig {
	return httpapi.RuntimeConfig{
		FastWaitingTransitionMs:         cfg.FastWaitingTransitionMs,
		ConservativeWaitingTransitionMs: cfg.ConservativeWaitingTransitionMs,
		LarkAutoRefreshIntervalMs:       cfg.LarkAutoRefreshIntervalMs,
		HeadlessSnapshotTimeoutMs:       cfg.HeadlessSnapshotTimeoutMs,
		LarkNotifyMaxLines:              cfg.LarkNotifyMaxLines,
		LarkNotifyFallbackTailLines:     cfg.LarkNotifyFallbackTailLines,
		LarkNotifyMergeWrappedLines:     cfg.LarkNotifyMergeWrappedLines,
		LarkNotifyDropLineRules:         cfg.LarkNotifyDropLineRules.Rules(),
		SessionPreStartCommand:          cfg.SessionPreStartCommand,
		LarkAppID:                       cfg.LarkAppID,
		LarkAppSecret:                   cfg.LarkAppSecret,
		LarkNotifyReceiveID:             cfg.LarkNotifyReceiveID,
		LarkMentionEnabled:              cfg.LarkMentionEnabled,
		LarkDefaultSessionName:          cfg.LarkDefaultSessionName,
		LarkSessionChatPrefix:           cfg.LarkSessionChatPrefix,
		LarkIgnoreMessagePrefix:         cfg.LarkIgnoreMessagePrefix,
		LarkAutoSummaryPrompt:           cfg.LarkAutoSummaryPrompt,
		SessionStartPresets:             cfg.SessionStartPresets,
		SessionNamePresets:              cfg.SessionNamePresets,
		LarkCustomShortcuts:             cfg.LarkCustomShortcuts,
		OnboardingCompleted:             cfg.OnboardingCompleted,
		AgentName:                       cfg.AgentName,
		AgentKind:                       cfg.AgentKind,
		AgentCommand:                    cfg.AgentCommand,
		WorkspaceOptions:                append([]session.WorkspaceOption(nil), cfg.WorkspaceOptions...),
	}
}

func normalizeLarkSessionChatPrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return defaultLarkSessionChatPrefix
	}
	return prefix
}

func normalizeLarkAutoSummaryPrompt(prompt string) string {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return defaultLarkAutoSummaryPrompt
	}
	return prompt
}

func writeConfigFile(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o600)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return parsed
}

type headlessBrowserManager struct {
	port     string
	mu       sync.Mutex
	sessions map[string]*headlessBrowserSession
	starting map[string]struct{}
}

type headlessBrowserSession struct {
	cmd     *exec.Cmd
	profile string
	started time.Time
}

func newHeadlessBrowserManager(port string) *headlessBrowserManager {
	return &headlessBrowserManager{
		port:     port,
		sessions: make(map[string]*headlessBrowserSession),
		starting: make(map[string]struct{}),
	}
}

func (m *headlessBrowserManager) Ensure(sessionID string) {
	if sessionID == "" {
		return
	}
	m.mu.Lock()
	if sess := m.sessions[sessionID]; sess != nil && sess.cmd != nil && sess.cmd.ProcessState == nil {
		m.mu.Unlock()
		return
	}
	if _, ok := m.starting[sessionID]; ok {
		m.mu.Unlock()
		return
	}
	m.starting[sessionID] = struct{}{}
	m.mu.Unlock()
	started := false
	defer func() {
		if started {
			return
		}
		m.mu.Lock()
		delete(m.starting, sessionID)
		m.mu.Unlock()
	}()

	chrome := findChrome()
	if chrome == "" {
		log.Printf("headless browser unavailable: Chrome/Chromium not found")
		return
	}
	profile, err := os.MkdirTemp("", "iris-headless-*")
	if err != nil {
		log.Printf("headless browser profile setup failed: %v", err)
		return
	}
	pageURL := "http://localhost:" + m.port + "/?session=" + url.QueryEscape(sessionID) + "&headless=1"
	cmd := exec.Command(chrome, headlessChromeArgs(profile, pageURL)...)
	cmd.Stderr = log.Writer()
	configureHeadlessCommand(cmd)
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(profile)
		log.Printf("headless browser start failed: %v", err)
		return
	}
	m.mu.Lock()
	delete(m.starting, sessionID)
	if existing := m.sessions[sessionID]; existing != nil && existing.cmd != nil && existing.cmd.ProcessState == nil {
		m.mu.Unlock()
		terminateHeadlessProcess(cmd)
		_ = os.RemoveAll(profile)
		return
	}
	m.sessions[sessionID] = &headlessBrowserSession{cmd: cmd, profile: profile, started: time.Now()}
	m.mu.Unlock()
	started = true
	log.Printf("headless browser started for terminal snapshots (pid=%d, session=%s)", cmd.Process.Pid, sessionID)
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("headless browser exited: %v", err)
		}
		m.mu.Lock()
		if sess := m.sessions[sessionID]; sess != nil && sess.cmd == cmd {
			delete(m.sessions, sessionID)
		}
		m.mu.Unlock()
		_ = os.RemoveAll(profile)
	}()
}

func (m *headlessBrowserManager) Stop(sessionID string) {
	if sessionID == "" {
		return
	}
	m.mu.Lock()
	sess := m.sessions[sessionID]
	if sess != nil {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()
	if sess == nil || sess.cmd == nil || sess.cmd.Process == nil || sess.cmd.ProcessState != nil {
		return
	}
	log.Printf("headless browser stopped (pid=%d, session=%s)", sess.cmd.Process.Pid, sessionID)
	terminateHeadlessProcess(sess.cmd)
}

func (m *headlessBrowserManager) StopAll() {
	m.mu.Lock()
	sessions := m.sessions
	m.sessions = make(map[string]*headlessBrowserSession)
	m.starting = make(map[string]struct{})
	m.mu.Unlock()
	for sessionID, sess := range sessions {
		if sess == nil || sess.cmd == nil || sess.cmd.Process == nil || sess.cmd.ProcessState != nil {
			continue
		}
		log.Printf("headless browser stopped (pid=%d, session=%s)", sess.cmd.Process.Pid, sessionID)
		terminateHeadlessProcess(sess.cmd)
	}
}

func headlessChromeArgs(profile, pageURL string) []string {
	return headlessChromeArgsForUID(profile, pageURL, os.Geteuid())
}

func headlessChromeArgsForUID(profile, pageURL string, uid int) []string {
	args := []string{
		"--headless=new",
		"--disable-gpu",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-dev-shm-usage",
		"--window-size=1440,1000",
		"--force-device-scale-factor=1",
		"--hide-scrollbars",
		"--user-data-dir=" + profile,
	}
	if uid == 0 {
		args = append(args, "--no-sandbox")
	}
	return append(args, pageURL)
}

func findChrome() string {
	candidates := []string{
		os.Getenv("CHROME_BIN"),
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser", "chrome"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}
