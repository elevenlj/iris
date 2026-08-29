package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

const (
	maxOutputBytes                       = 512 * 1024
	maxRoundBytes                        = 64 * 1024
	defaultFastWaitingTransition         = 5 * time.Second
	defaultConservativeWaitingTransition = 5 * time.Second
	defaultAutoRefreshInterval           = 5 * time.Second
	defaultNotificationUpdateCoalesce    = 0
	defaultNotifyRetryDelay              = time.Second
	defaultNotifySnapshotTimeout         = 1200 * time.Millisecond
	defaultNotifySnapshotDeadline        = 2500 * time.Millisecond
	defaultInputBaselineSnapshotDeadline = 2500 * time.Millisecond
	defaultHeadlessSnapshotTimeout       = 10 * time.Second
	defaultStartupPresetSettleDelay      = 2 * time.Second
	defaultStartupInputQueueWindow       = 30 * time.Second
	defaultAgentRestartFallbackDelay     = time.Second
	defaultAgentTerminationTimeout       = 6 * time.Second
	defaultAgentRestartContextTimeout    = 30 * time.Second
	defaultAgentRestartReadyPollInterval = 250 * time.Millisecond
	defaultNotificationSendAttempts      = 3
	defaultNotificationSendRetryDelay    = 120 * time.Millisecond
	defaultWorkspaceDirName              = "default_dir"
)

var agentRestartContextTimeout = defaultAgentRestartContextTimeout
var agentRestartReadyPollInterval = defaultAgentRestartReadyPollInterval

var errNotificationMessageDisabled = errors.New("notification message is disabled")

type startupNotifyMode int

const (
	startupNotifyNormal startupNotifyMode = iota
	startupNotifySuppress
	startupNotifySettling
	startupNotifyFinal
	startupNotifyDiscard
)

type Store interface {
	CreateSession(context.Context, Session) error
	UpdateSession(context.Context, Session) error
	ListSessions(context.Context) ([]Session, error)
	GetSession(context.Context, string) (Session, bool, error)
	DeleteSession(context.Context, string) error
	AppendOutput(context.Context, string, int64, []byte) error
	Output(context.Context, string) ([]byte, error)
	DeleteAllSessions(context.Context) error
	ListQuickCommands(context.Context) ([]QuickCommand, error)
	CreateQuickCommand(context.Context, QuickCommand) error
	DeleteQuickCommand(context.Context, string) error
	GetLarkContactBinding(context.Context, string) (LarkContactBinding, bool, error)
	UpsertLarkContactBinding(context.Context, LarkContactBinding) error
	DeactivateLarkContactBinding(context.Context, string) error
}

type Manager struct {
	mu                       sync.RWMutex
	store                    Store
	launcher                 Launcher
	notifier                 WaitingNotifier
	idCounter                atomic.Int64
	fastWaiting              time.Duration
	conservativeWaiting      time.Duration
	autoRefreshInterval      time.Duration
	headlessSnapshotWait     time.Duration
	updateCoalesce           time.Duration
	preStartCommand          string
	recoveryBaseDir          string
	agentTurnHookURL         string
	sessions                 map[string]*RuntimeSession
	onBrowserNeeded          func(string)
	onBrowserActive          func(string)
	onBrowserStopped         func(string)
	onNotificationSent       func(string)
	onSessionEnded           func(string)
	defaultAgent             AgentConfig
	defaultWorkspaceDir      string
	agentOptions             []AgentOption
	workspaceOptions         []WorkspaceOption
	larkConversationProvider LarkConversationProvider
	larkAgentContexts        map[string]LarkAgentContext
}

func (m *Manager) SetAgentConfig(agent AgentConfig, workspaces []WorkspaceOption) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaultAgent = normalizeAgentConfig(agent)
	m.workspaceOptions = normalizeWorkspaceOptions(workspaces)
}

func (m *Manager) SetDefaultWorkspaceDir(dir string) {
	m.mu.Lock()
	m.defaultWorkspaceDir = strings.TrimSpace(dir)
	m.mu.Unlock()
}

func (m *Manager) AgentConfig() (AgentConfig, []WorkspaceOption) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.defaultAgent, append([]WorkspaceOption(nil), m.workspaceOptions...)
}

func (m *Manager) SetAvailableAgentOptions(options []AgentOption) {
	m.mu.Lock()
	m.agentOptions = normalizeAgentOptions(options)
	m.mu.Unlock()
}

func (m *Manager) AvailableAgentOptions() []AgentOption {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]AgentOption(nil), m.agentOptions...)
}

func (m *Manager) agentOption(id string) (AgentOption, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, option := range m.agentOptions {
		if option.ID == id {
			return option, true
		}
	}
	return AgentOption{}, false
}

func normalizeAgentConfig(agent AgentConfig) AgentConfig {
	agent.ID = strings.ToLower(strings.TrimSpace(agent.ID))
	agent.Name = strings.TrimSpace(agent.Name)
	agent.Kind = strings.ToLower(strings.TrimSpace(agent.Kind))
	agent.Command = strings.TrimSpace(agent.Command)
	if agent.Kind == "codex" {
		agent.ID = "codex"
		agent.Name = "Codex"
		agent.Command = CodexAgentCommand
	}
	if agent.Kind == "claude" {
		agent.ID = "claude"
		agent.Name = "Claude Code"
		agent.Command = ClaudeAgentCommand
	}
	if agent.Kind == "custom" && agent.Name == "" {
		agent.Name = "自定义 Agent"
	}
	if agent.Kind != "codex" && agent.Kind != "claude" && agent.Kind != "custom" {
		return AgentConfig{}
	}
	return agent
}

func normalizeWorkspaceOptions(workspaces []WorkspaceOption) []WorkspaceOption {
	out := make([]WorkspaceOption, 0, len(workspaces))
	seenLabels := map[string]bool{}
	seenPaths := map[string]bool{}
	for _, workspace := range workspaces {
		workspace.Label = strings.TrimSpace(workspace.Label)
		workspace.Value = strings.TrimSpace(workspace.Value)
		if workspace.Label == "" || workspace.Value == "" || seenLabels[workspace.Label] || seenPaths[workspace.Value] {
			continue
		}
		if abs, err := filepath.Abs(workspace.Value); err == nil {
			workspace.Value = abs
		}
		workspace.Default = false
		seenLabels[workspace.Label] = true
		seenPaths[workspace.Value] = true
		out = append(out, workspace)
	}
	return out
}

func (m *Manager) defaultAgentSnapshot() AgentConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.defaultAgent
}

type ManagerOption func(*Manager)

func NewManager(store Store, launcher Launcher, opts ...ManagerOption) *Manager {
	m := &Manager{
		store:                store,
		launcher:             launcher,
		fastWaiting:          defaultFastWaitingTransition,
		conservativeWaiting:  defaultConservativeWaitingTransition,
		autoRefreshInterval:  defaultAutoRefreshInterval,
		headlessSnapshotWait: defaultHeadlessSnapshotTimeout,
		updateCoalesce:       defaultNotificationUpdateCoalesce,
		sessions:             make(map[string]*RuntimeSession),
		larkAgentContexts:    make(map[string]LarkAgentContext),
	}
	for _, opt := range opts {
		opt(m)
	}
	if m.launcher == nil {
		m.launcher = ShellLauncher{}
	}
	return m
}

func WithNotifier(n WaitingNotifier) ManagerOption {
	return func(m *Manager) { m.notifier = n }
}

func (m *Manager) SetNotifier(n WaitingNotifier) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notifier = n
}

func WithWaitingTransitionDelays(fast, conservative time.Duration) ManagerOption {
	return func(m *Manager) {
		if fast > 0 {
			m.fastWaiting = fast
		}
		if conservative > 0 {
			m.conservativeWaiting = conservative
		}
	}
}

func WithNotificationUpdateCoalesce(delay time.Duration) ManagerOption {
	return func(m *Manager) {
		if delay >= 0 {
			m.updateCoalesce = delay
		}
	}
}

func WithAutoRefreshInterval(interval time.Duration) ManagerOption {
	return func(m *Manager) {
		if interval > 0 {
			m.autoRefreshInterval = interval
		}
	}
}

func WithHeadlessSnapshotTimeout(timeout time.Duration) ManagerOption {
	return func(m *Manager) {
		if timeout > 0 {
			m.headlessSnapshotWait = timeout
		}
	}
}

func WithBrowserNeeded(fn func(string)) ManagerOption {
	return func(m *Manager) { m.onBrowserNeeded = fn }
}

func WithBrowserActive(fn func(string)) ManagerOption {
	return func(m *Manager) { m.onBrowserActive = fn }
}

func WithBrowserStopped(fn func(string)) ManagerOption {
	return func(m *Manager) { m.onBrowserStopped = fn }
}

func WithSessionEnded(fn func(string)) ManagerOption {
	return func(m *Manager) { m.onSessionEnded = fn }
}

func WithPreStartCommand(command string) ManagerOption {
	return func(m *Manager) { m.preStartCommand = strings.TrimSpace(command) }
}

func WithRecoveryBaseDir(dir string) ManagerOption {
	return func(m *Manager) {
		dir = strings.TrimSpace(dir)
		if dir != "" {
			if abs, err := filepath.Abs(dir); err == nil {
				dir = abs
			}
		}
		m.recoveryBaseDir = dir
	}
}

// WithAgentTurnHookURL configures the loopback callback URL inherited by
// agent processes. Their turn-ended hook uses it to complete a round without
// relying on terminal output silence.
func WithAgentTurnHookURL(url string) ManagerOption {
	return func(m *Manager) {
		m.agentTurnHookURL = strings.TrimRight(strings.TrimSpace(url), "/")
	}
}

func (m *Manager) AgentTurnHookURL() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.agentTurnHookURL
}

func (m *Manager) SetWaitingTransitionDelays(fast, conservative time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if fast > 0 {
		m.fastWaiting = fast
	}
	if conservative > 0 {
		m.conservativeWaiting = conservative
	}
}

func (m *Manager) SetAutoRefreshInterval(interval time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if interval > 0 {
		m.autoRefreshInterval = interval
	}
}

func (m *Manager) SetHeadlessSnapshotTimeout(timeout time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if timeout > 0 {
		m.headlessSnapshotWait = timeout
	}
}

func (m *Manager) headlessSnapshotTimeout() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.headlessSnapshotWait <= 0 {
		return defaultHeadlessSnapshotTimeout
	}
	return m.headlessSnapshotWait
}

func (m *Manager) autoRefreshDelay() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.autoRefreshInterval <= 0 {
		return defaultAutoRefreshInterval
	}
	return m.autoRefreshInterval
}

func (m *Manager) SetPreStartCommand(command string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.preStartCommand = strings.TrimSpace(command)
}

func (m *Manager) EnsureBrowser(sessionID string) {
	if m.onBrowserNeeded == nil || sessionID == "" {
		return
	}
	go m.onBrowserNeeded(sessionID)
}

func (m *Manager) BrowserActive(sessionID string) {
	if m.onBrowserActive == nil || sessionID == "" {
		return
	}
	go m.onBrowserActive(sessionID)
}

func (m *Manager) StopBrowser(sessionID string) {
	if m.onBrowserStopped == nil || sessionID == "" {
		return
	}
	go m.onBrowserStopped(sessionID)
}

func (m *Manager) RestartBrowser(sessionID string) {
	if m.onBrowserNeeded == nil || sessionID == "" {
		return
	}
	stopped := m.onBrowserStopped
	needed := m.onBrowserNeeded
	go func() {
		if stopped != nil {
			stopped(sessionID)
		}
		needed(sessionID)
	}()
}

func (m *Manager) SetNotificationSentHook(fn func(string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onNotificationSent = fn
}

func (m *Manager) notificationSent(sessionID string) {
	m.mu.RLock()
	fn := m.onNotificationSent
	m.mu.RUnlock()
	if fn != nil && sessionID != "" {
		go fn(sessionID)
	}
}

func (m *Manager) sessionEnded(sessionID string) {
	m.mu.RLock()
	fn := m.onSessionEnded
	m.mu.RUnlock()
	if fn != nil && sessionID != "" {
		fn(sessionID)
	}
}

func (m *Manager) CreateSession(ctx context.Context, name string) (Session, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Session{}, errors.New("session name is required")
	}
	workspaceDir := m.defaultSessionWorkspaceDir()
	now := time.Now().UTC()
	id, err := m.nextSessionID(ctx)
	if err != nil {
		return Session{}, err
	}
	sess := Session{
		ID:          id,
		Name:        name,
		Status:      StatusRunning,
		CreatedAt:   now,
		UpdatedAt:   now,
		Live:        true,
		RecoveryKey: newRecoveryKey(),
		LastMode:    SessionModeShell,
		LastCWD:     workspaceDir,
	}
	handle, err := m.launcher.Launch(context.Background())
	if err != nil {
		code := 1
		sess.Status = StatusFailed
		sess.Live = false
		sess.ExitCode = &code
		return sess, err
	}
	agent := m.defaultAgentSnapshot()
	startupMode := startupNotifyNormal
	if agent.Command != "" {
		startupMode = startupNotifyDiscard
	}
	rt := &RuntimeSession{
		manager:           m,
		session:           sess,
		terminal:          handle.Terminal(),
		process:           handle.Process(),
		subscribers:       make(map[chan RuntimeEvent]runtimeSubscriber),
		terminalCols:      defaultTerminalCols,
		terminalRows:      defaultTerminalRows,
		inputQueueUntil:   now.Add(defaultStartupInputQueueWindow),
		startupNotifyMode: startupMode,
	}
	if m.store != nil {
		if err := m.store.CreateSession(ctx, sess); err != nil {
			_ = handle.Terminal().Close()
			return Session{}, err
		}
	}
	m.mu.Lock()
	m.sessions[id] = rt
	m.mu.Unlock()
	go rt.streamOutput()
	go rt.waitForExit()
	rt.runRecoveryEnvironmentSetup()
	rt.runPreStartCommand()
	if agent.Command != "" {
		workspaceShellPath := m.defaultSessionWorkspaceShellPath()
		_, _ = rt.terminal.Write([]byte("mkdir -p " + workspaceShellPath + "\r"))
		rt.RecordShellCommandForRecovery("cd " + shellQuote(workspaceDir))
		_, _ = rt.terminal.Write([]byte("cd " + workspaceShellPath + "\r"))
		rt.ConfigureAgentForRecovery(agent)
		_, _ = rt.terminal.Write([]byte(agent.Command + "\r"))
		sess = rt.Snapshot()
	}
	sess.NotificationsAvailable = m.notifier != nil && m.notifier.Available()
	return sess, nil
}

func (m *Manager) nextSessionID(ctx context.Context) (string, error) {
	for {
		id := fmt.Sprintf("sess-%d", m.idCounter.Add(1))
		if m.store == nil {
			return id, nil
		}
		_, exists, err := m.store.GetSession(ctx, id)
		if err != nil {
			return "", err
		}
		if !exists {
			return id, nil
		}
	}
}

func (m *Manager) defaultWorkingDir() string {
	if l, ok := m.launcher.(ShellLauncher); ok && strings.TrimSpace(l.Dir) != "" {
		return strings.TrimSpace(l.Dir)
	}
	if l, ok := m.launcher.(*ShellLauncher); ok && l != nil && strings.TrimSpace(l.Dir) != "" {
		return strings.TrimSpace(l.Dir)
	}
	if dir := strings.TrimSpace(os.Getenv("TERMINAL_WORKING_DIR")); dir != "" {
		return dir
	}
	if dir, err := os.UserHomeDir(); err == nil && dir != "" {
		return dir
	}
	return "."
}

func irisWorkspaceRootDir() string {
	if dir := strings.TrimSpace(os.Getenv("IRIS_WORKSPACE_DIR")); dir != "" {
		if abs, err := filepath.Abs(dir); err == nil {
			return abs
		}
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, "Iris_Workspace")
	}
	return filepath.Join(".", "Iris_Workspace")
}

func builtInDefaultWorkspaceDir() string {
	return filepath.Join(irisWorkspaceRootDir(), defaultWorkspaceDirName)
}

func builtInDefaultWorkspaceShellPath() string {
	if strings.TrimSpace(os.Getenv("IRIS_WORKSPACE_DIR")) != "" {
		return shellQuote(filepath.Join(irisWorkspaceRootDir(), defaultWorkspaceDirName))
	}
	return "${HOME}/" + shellQuote("Iris_Workspace/"+defaultWorkspaceDirName)
}

func (m *Manager) defaultSessionWorkspaceDir() string {
	m.mu.RLock()
	dir := strings.TrimSpace(m.defaultWorkspaceDir)
	m.mu.RUnlock()
	if dir != "" {
		return dir
	}
	return builtInDefaultWorkspaceDir()
}

func (m *Manager) defaultSessionWorkspaceShellPath() string {
	m.mu.RLock()
	dir := strings.TrimSpace(m.defaultWorkspaceDir)
	m.mu.RUnlock()
	if dir != "" {
		return shellQuote(dir)
	}
	return builtInDefaultWorkspaceShellPath()
}

func (m *Manager) WorkspaceOptionsForSession(_ Session) []WorkspaceOption {
	defaultDir := m.defaultSessionWorkspaceDir()
	out := []WorkspaceOption{{Label: "默认目录", Value: defaultDir, Default: true}}
	_, configured := m.AgentConfig()
	for _, workspace := range configured {
		workspace.Default = false
		if workspace.Value == defaultDir {
			continue
		}
		out = append(out, workspace)
	}
	return out
}

// sessionSupportsWorkspaceSwitch reports whether the session runs Codex directly or through a compatible wrapper.
func sessionSupportsWorkspaceSwitch(sess Session) bool {
	if strings.EqualFold(strings.TrimSpace(sess.LastAgentKind), "codex") {
		return true
	}
	argv := shellFields(sess.LastAgentStartCommand)
	for len(argv) > 0 && isShellEnvAssignment(argv[0]) {
		argv = argv[1:]
	}
	info, ok := agentLaunchInfo(argv)
	return ok && info.Kind == "codex"
}

func agentKindForCommand(command, fallback string) string {
	argv := shellFields(command)
	for len(argv) > 0 && isShellEnvAssignment(argv[0]) {
		argv = argv[1:]
	}
	if info, ok := agentLaunchInfo(argv); ok {
		return info.Kind
	}
	if fallback = strings.ToLower(strings.TrimSpace(fallback)); fallback != "" {
		return fallback
	}
	return "custom"
}

func (m *Manager) sessionRecoveryDir(sess Session) string {
	if strings.TrimSpace(m.recoveryBaseDir) == "" || strings.TrimSpace(sess.RecoveryKey) == "" {
		return ""
	}
	return filepath.Join(m.recoveryBaseDir, sess.RecoveryKey)
}

func (m *Manager) sessionCodexHome(sess Session) string {
	return m.sessionAgentHome(sess, "codex")
}

func (m *Manager) sessionClaudeHome(sess Session) string {
	return m.sessionAgentHome(sess, "claude")
}

func (m *Manager) sessionAgentHome(sess Session, kind string) string {
	switch strings.TrimSpace(kind) {
	case "codex":
		return defaultCodexHome()
	case "claude":
		dir := m.sessionRecoveryDir(sess)
		if dir == "" {
			return ""
		}
		return filepath.Join(dir, "claude_home")
	default:
		return ""
	}
}

func (m *Manager) ListSessions(ctx context.Context) ([]Session, error) {
	var list []Session
	var err error
	if m.store != nil {
		list, err = m.store.ListSessions(ctx)
		if err != nil {
			return nil, err
		}
	} else {
		m.mu.RLock()
		for _, rt := range m.sessions {
			list = append(list, rt.Snapshot())
		}
		m.mu.RUnlock()
	}
	active := list[:0]
	for _, s := range list {
		if s.Live && s.Status != StatusExited && s.Status != StatusFailed {
			active = append(active, s)
		}
	}
	list = active
	available := m.notifier != nil && m.notifier.Available()
	for i := range list {
		list[i].NotificationsAvailable = available
	}
	return list, nil
}

func (m *Manager) GetRuntime(id string) (*RuntimeSession, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rt, ok := m.sessions[id]
	return rt, ok
}

func (m *Manager) RecoverRuntime(ctx context.Context, id string) (*RuntimeSession, Session, bool, error) {
	if rt, ok := m.GetRuntime(id); ok {
		s := rt.Snapshot()
		s.NotificationsAvailable = m.notifier != nil && m.notifier.Available()
		return rt, s, true, nil
	}
	if m.store == nil {
		return nil, Session{}, false, nil
	}
	sess, ok, err := m.store.GetSession(ctx, id)
	if err != nil || !ok {
		return nil, sess, ok, err
	}
	if !sess.Live || sess.Status == StatusExited || sess.Status == StatusFailed {
		return nil, sess, false, nil
	}
	if strings.TrimSpace(sess.RecoveryKey) == "" {
		sess.RecoveryKey = newRecoveryKey()
	}
	if strings.TrimSpace(sess.LastMode) == "" {
		sess.LastMode = SessionModeShell
	}
	if strings.TrimSpace(sess.LastCWD) == "" {
		sess.LastCWD = m.defaultWorkingDir()
	}
	if migrated, err := m.prepareCodexRecovery(sess); err != nil {
		log.Printf("codex recovery migration skipped session=%s: %v", sess.ID, err)
	} else {
		sess = migrated
	}
	handle, err := m.launcher.Launch(context.Background())
	if err != nil {
		return nil, sess, true, err
	}
	now := time.Now().UTC()
	sess.Live = true
	sess.Status = StatusRunning
	sess.ExitCode = nil
	sess.UpdatedAt = now
	resumingAgent := strings.TrimSpace(sess.LastMode) == SessionModeAgent && strings.TrimSpace(sess.LastAgentResumeCommand) != ""
	startupMode := startupNotifyNormal
	if resumingAgent {
		startupMode = startupNotifyDiscard
	}
	rt := &RuntimeSession{
		manager:           m,
		session:           sess,
		terminal:          handle.Terminal(),
		process:           handle.Process(),
		subscribers:       make(map[chan RuntimeEvent]runtimeSubscriber),
		terminalCols:      defaultTerminalCols,
		terminalRows:      defaultTerminalRows,
		nextSeq:           time.Now().UnixNano(),
		startupNotifyMode: startupMode,
	}
	if resumingAgent {
		rt.inputQueueUntil = time.Now().Add(defaultStartupInputQueueWindow)
	}
	m.mu.Lock()
	if existing, exists := m.sessions[id]; exists {
		m.mu.Unlock()
		_ = handle.Terminal().Close()
		s := existing.Snapshot()
		s.NotificationsAvailable = m.notifier != nil && m.notifier.Available()
		return existing, s, true, nil
	}
	m.sessions[id] = rt
	m.mu.Unlock()
	if err := m.store.UpdateSession(ctx, sess); err != nil {
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
		rt.Close()
		return nil, sess, true, err
	}
	go rt.streamOutput()
	go rt.waitForExit()
	rt.runRecoveryEnvironmentSetup()
	rt.runRecoveryCommand()
	s := rt.Snapshot()
	s.NotificationsAvailable = m.notifier != nil && m.notifier.Available()
	return rt, s, true, nil
}

func (m *Manager) GetSession(ctx context.Context, id string) (Session, bool, error) {
	if rt, ok := m.GetRuntime(id); ok {
		s := rt.Snapshot()
		s.NotificationsAvailable = m.notifier != nil && m.notifier.Available()
		return s, true, nil
	}
	if m.store == nil {
		return Session{}, false, nil
	}
	s, ok, err := m.store.GetSession(ctx, id)
	if ok && (!s.Live || s.Status == StatusExited || s.Status == StatusFailed) {
		return Session{}, false, nil
	}
	s.NotificationsAvailable = m.notifier != nil && m.notifier.Available()
	return s, ok, err
}

func (m *Manager) UpdateNotifyOnWaiting(ctx context.Context, id string, enabled bool) (Session, bool, error) {
	rt, ok := m.GetRuntime(id)
	if !ok {
		s, exists, err := m.GetSession(ctx, id)
		if err != nil || !exists {
			return Session{}, exists, err
		}
		s.NotifyOnWaiting = enabled
		s.UpdatedAt = time.Now().UTC()
		if m.store != nil {
			err = m.store.UpdateSession(ctx, s)
		}
		return s, true, err
	}
	rt.mu.Lock()
	rt.session.NotifyOnWaiting = enabled
	rt.session.UpdatedAt = time.Now().UTC()
	s := rt.session
	rt.mu.Unlock()
	err := m.persist(ctx, s)
	if enabled {
		m.EnsureBrowser(id)
	} else {
		m.StopBrowser(id)
	}
	s.NotificationsAvailable = m.notifier != nil && m.notifier.Available()
	return s, true, err
}

func (m *Manager) ToggleLarkMentionMode(ctx context.Context, id string) (Session, bool, error) {
	rt, ok := m.GetRuntime(id)
	if !ok {
		s, exists, err := m.GetSession(ctx, id)
		if err != nil || !exists {
			return Session{}, exists, err
		}
		s.LarkMentionModeEnabled = !s.LarkMentionModeEnabled
		s.UpdatedAt = time.Now().UTC()
		if m.store != nil {
			err = m.store.UpdateSession(ctx, s)
		}
		s.NotificationsAvailable = m.notifier != nil && m.notifier.Available()
		return s, true, err
	}
	rt.mu.Lock()
	rt.session.LarkMentionModeEnabled = !rt.session.LarkMentionModeEnabled
	rt.session.UpdatedAt = time.Now().UTC()
	s := rt.session
	rt.mu.Unlock()
	err := m.persist(ctx, s)
	s.NotificationsAvailable = m.notifier != nil && m.notifier.Available()
	return s, true, err
}

func (m *Manager) UpdateLarkMentionMode(ctx context.Context, id string, enabled bool) (Session, bool, error) {
	rt, ok := m.GetRuntime(id)
	if !ok {
		s, exists, err := m.GetSession(ctx, id)
		if err != nil || !exists {
			return Session{}, exists, err
		}
		s.LarkMentionModeEnabled = enabled
		s.UpdatedAt = time.Now().UTC()
		if m.store != nil {
			err = m.store.UpdateSession(ctx, s)
		}
		s.NotificationsAvailable = m.notifier != nil && m.notifier.Available()
		return s, true, err
	}
	rt.mu.Lock()
	rt.session.LarkMentionModeEnabled = enabled
	rt.session.UpdatedAt = time.Now().UTC()
	s := rt.session
	rt.mu.Unlock()
	err := m.persist(ctx, s)
	s.NotificationsAvailable = m.notifier != nil && m.notifier.Available()
	return s, true, err
}

func (m *Manager) UpdateDeveloperMode(ctx context.Context, id string, enabled bool) (Session, bool, error) {
	rt, ok := m.GetRuntime(id)
	if !ok {
		return Session{}, false, nil
	}
	rt.mu.Lock()
	rt.session.DeveloperModeEnabled = enabled
	rt.session.UpdatedAt = time.Now().UTC()
	sess := rt.session
	rt.mu.Unlock()
	if err := m.persist(ctx, sess); err != nil {
		return Session{}, false, err
	}
	return sess, true, nil
}

func (m *Manager) SwitchWorkspace(ctx context.Context, id, path string) (Session, bool, error) {
	path = strings.TrimSpace(path)
	rt, ok := m.GetRuntime(id)
	if !ok {
		return Session{}, false, nil
	}
	sess := rt.Snapshot()
	if sess.Status == StatusRunning {
		return Session{}, true, errors.New("当前任务正在进行中，无法切换目录")
	}
	workspaces := m.WorkspaceOptionsForSession(sess)
	allowed := false
	for _, workspace := range workspaces {
		if workspace.Value == path {
			allowed = true
			break
		}
	}
	if !allowed {
		return Session{}, true, errors.New("目录不在配置白名单中")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return Session{}, true, errors.New("目录不存在或不可访问")
	}
	input := "/cd " + path
	if sessionSupportsWorkspaceSwitch(sess) {
		if err := rt.beginControlInput(); err != nil {
			return Session{}, true, err
		}
		if err := SubmitSilentStructuredInput(rt, input); err != nil {
			rt.cancelControlInput()
			return Session{}, true, err
		}
	} else {
		return Session{}, true, errors.New("当前自定义 Agent 不支持运行时切换目录")
	}
	rt.mu.Lock()
	rt.session.LastCWD = path
	rt.pendingAgentDirectory = path
	rt.session.UpdatedAt = time.Now().UTC()
	sess = rt.session
	rt.mu.Unlock()
	if err := m.persist(ctx, sess); err != nil {
		return Session{}, true, err
	}
	return sess, true, nil
}

func (rt *RuntimeSession) beginControlInput() error {
	if rt == nil {
		return errors.New("会话不在线")
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.closed || !rt.session.Live || rt.terminal == nil {
		return errors.New("会话不在线")
	}
	if rt.session.Status == StatusRunning {
		return errors.New("当前任务正在进行中，无法切换目录")
	}
	rt.controlInputActive = true
	return nil
}

func (rt *RuntimeSession) cancelControlInput() {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	rt.controlInputActive = false
	rt.mu.Unlock()
}

func (m *Manager) GetLarkContactBinding(ctx context.Context, senderOpenID string) (LarkContactBinding, bool, error) {
	if m.store == nil {
		return LarkContactBinding{}, false, nil
	}
	return m.store.GetLarkContactBinding(ctx, strings.TrimSpace(senderOpenID))
}

func (m *Manager) UpsertLarkContactBinding(ctx context.Context, binding LarkContactBinding) error {
	if m.store == nil {
		return nil
	}
	return m.store.UpsertLarkContactBinding(ctx, binding)
}

func (m *Manager) DeactivateLarkContactBinding(ctx context.Context, senderOpenID string) error {
	if m.store == nil {
		return nil
	}
	return m.store.DeactivateLarkContactBinding(ctx, strings.TrimSpace(senderOpenID))
}

func (m *Manager) BindLarkChat(ctx context.Context, id string, chatID string) (Session, bool, error) {
	chatID = strings.TrimSpace(chatID)
	rt, ok := m.GetRuntime(id)
	if !ok {
		s, exists, err := m.GetSession(ctx, id)
		if err != nil || !exists {
			return Session{}, exists, err
		}
		s.LarkChatID = chatID
		s.UpdatedAt = time.Now().UTC()
		if m.store != nil {
			err = m.store.UpdateSession(ctx, s)
		}
		if err == nil && chatID != "" {
			defaultLarkMessageRegistry.rememberChat(chatID, id)
		}
		return s, true, err
	}
	rt.mu.Lock()
	rt.session.LarkChatID = chatID
	if chatID != "" {
		rt.requireLarkChat = false
	}
	rt.session.UpdatedAt = time.Now().UTC()
	s := rt.session
	rt.mu.Unlock()
	if m.store != nil {
		if err := m.store.UpdateSession(ctx, s); err != nil {
			return s, true, err
		}
	}
	if chatID != "" {
		defaultLarkMessageRegistry.rememberChat(chatID, id)
	}
	return s, true, nil
}

func (m *Manager) FindSessionByLarkChatID(ctx context.Context, chatID string) (Session, bool, error) {
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return Session{}, false, nil
	}
	m.mu.RLock()
	for _, rt := range m.sessions {
		s := rt.Snapshot()
		if s.LarkChatID == chatID && s.Live && s.Status != StatusExited && s.Status != StatusFailed {
			defaultLarkMessageRegistry.rememberChat(chatID, s.ID)
			m.mu.RUnlock()
			return s, true, nil
		}
	}
	m.mu.RUnlock()
	if m.store == nil {
		return Session{}, false, nil
	}
	list, err := m.store.ListSessions(ctx)
	if err != nil {
		return Session{}, false, err
	}
	for _, s := range list {
		if s.LarkChatID == chatID && s.Live && s.Status != StatusExited && s.Status != StatusFailed {
			defaultLarkMessageRegistry.rememberChat(chatID, s.ID)
			return s, true, nil
		}
	}
	return Session{}, false, nil
}

func (m *Manager) LatestLarkChatID() string {
	if m == nil {
		return ""
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var latest Session
	for _, rt := range m.sessions {
		s := rt.Snapshot()
		if strings.TrimSpace(s.LarkChatID) == "" {
			continue
		}
		if latest.ID == "" || s.CreatedAt.After(latest.CreatedAt) {
			latest = s
		}
	}
	return latest.LarkChatID
}

func (m *Manager) DeleteSession(ctx context.Context, id string) error {
	m.mu.Lock()
	rt, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	delete(m.larkAgentContexts, id)
	m.mu.Unlock()
	if ok {
		rt.Close()
	}
	if m.store != nil {
		return m.store.DeleteSession(ctx, id)
	}
	return nil
}

func (m *Manager) Output(ctx context.Context, id string) ([]byte, bool, error) {
	if rt, ok := m.GetRuntime(id); ok {
		return rt.OutputSnapshot(), true, nil
	}
	if m.store == nil {
		return nil, false, nil
	}
	if _, ok, err := m.store.GetSession(ctx, id); err != nil || !ok {
		return nil, ok, err
	}
	out, err := m.store.Output(ctx, id)
	return out, true, err
}

func (m *Manager) ListQuickCommands(ctx context.Context) ([]QuickCommand, error) {
	return m.store.ListQuickCommands(ctx)
}

func (m *Manager) CreateQuickCommand(ctx context.Context, name, text string) (QuickCommand, error) {
	qc := QuickCommand{ID: fmt.Sprintf("qc-%d", time.Now().UnixNano()), Name: strings.TrimSpace(name), Text: text, CreatedAt: time.Now().UTC()}
	if qc.Name == "" || strings.TrimSpace(qc.Text) == "" {
		return QuickCommand{}, errors.New("name and text are required")
	}
	return qc, m.store.CreateQuickCommand(ctx, qc)
}

func (m *Manager) DeleteQuickCommand(ctx context.Context, id string) error {
	return m.store.DeleteQuickCommand(ctx, id)
}

func (m *Manager) persist(ctx context.Context, sess Session) error {
	if m.store == nil {
		return nil
	}
	return m.store.UpdateSession(ctx, sess)
}

type RuntimeSession struct {
	mu                                sync.Mutex
	notificationPatchMu               sync.Mutex
	terminalCloseOnce                 sync.Once
	sessionEndedOnce                  sync.Once
	closed                            bool
	manager                           *Manager
	session                           Session
	terminal                          Terminal
	process                           Waiter
	output                            []byte
	roundReply                        []byte
	visibleSnapshot                   string
	visibleSnapshotSource             string
	visibleSnapshotResponder          chan RuntimeEvent
	visibleSnapshotCols               uint16
	visibleSnapshotVersion            int64
	terminalCols                      uint16
	terminalRows                      uint16
	snapshotAtRoundStart              string
	snapshotAtRoundSource             string
	snapshotAtRoundResponder          chan RuntimeEvent
	snapshotAtRoundCols               uint16
	snapshotAtRoundVersion            int64
	snapshotAtRoundStartSet           bool
	capturedInputBaselineResponder    chan RuntimeEvent
	capturedInputBaselineHeadless     bool
	lastInputText                     string
	inputLineBuffer                   string
	inputCursor                       int
	inputRecordUnreliable             bool
	inputBracketedPaste               bool
	lastNotifiedRoundHash             string
	lastNotifiedMessageID             string
	lastNotifiedContent               string
	lastNotifiedVisibleSnapshot       string
	lastNotifiedVisibleSnapshotSource string
	lastNotifiedVisibleResponder      chan RuntimeEvent
	lastNotifiedVisibleCols           uint16
	notificationMentionOpenID         string
	notificationUpdateNo              int
	notificationRunning               bool
	notificationWindowInputText       string
	frozenNotificationMessages        map[string]struct{}
	pendingTerminalInteraction        *TerminalInteraction
	lastConsumedTerminalInteractionID string
	lastTerminalAgentContext          *TerminalAgentContext
	pendingAgentDirectory             string
	controlInputActive                bool
	agentTurnHookVerified             bool
	hookCompletedCurrentRound         bool
	hookCompletionTipClaimed          bool
	hookLastAssistantMessage          string
	suppressRunningMarker             bool
	requireLarkChat                   bool
	notificationPatchVersion          int64
	autoRefreshEnabled                bool
	autoRefreshMessageID              string
	autoRefreshStop                   chan struct{}
	autoSummaryEnabled                bool
	startupNotifyMode                 startupNotifyMode
	inputQueueUntil                   time.Time
	subscribers                       map[chan RuntimeEvent]runtimeSubscriber
	snapshotRequests                  map[string]*pendingSnapshotRequest
	nextSnapshotRequestID             int64
	latestAppliedSnapshotRequestID    int64
	snapshotRoundGeneration           int64
	preferredBrowserSnapshotSource    chan RuntimeEvent
	preferredHeadlessSnapshotSource   chan RuntimeEvent
	pendingHeadlessSnapshots          int
	nextSeq                           int64
	stateVersion                      int64
	notifyVersion                     int64
	notifyRetryTimer                  *time.Timer
	notifyStableTimer                 *time.Timer
	startupNotifyTimer                *time.Timer
	agentRestartPending               bool
}

type RuntimeEvent struct {
	Type      string
	Data      []byte
	Cols      uint16
	Rows      uint16
	RequestID string
	Purpose   string
}

// agentIdleCompletionFallback is a safety net for turns that cannot invoke
// their completion hook (for example, when the user interrupts Codex).
const agentIdleCompletionFallback = 5 * time.Second

type runtimeSubscriber struct {
	Headless bool
}

type pendingSnapshotRequest struct {
	waiter         chan struct{}
	headless       bool
	seq            int64
	round          int64
	target         chan RuntimeEvent
	requiredTarget chan RuntimeEvent
	purpose        string
	applied        bool
}

const (
	RuntimeEventOutput           = "output"
	RuntimeEventSnapshotRequest  = "snapshot_request"
	RuntimeEventTerminalResize   = "terminal_resize"
	SnapshotPurposeInputBaseline = "input_baseline"
	SnapshotPurposeAgentRestart  = "agent_restart"
)

func (rt *RuntimeSession) Snapshot() Session {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.session
}

func (rt *RuntimeSession) ShouldQueueInputWhileRunning() bool {
	if rt == nil {
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.shouldQueueInputWhileRunningLocked(time.Now())
}

func (rt *RuntimeSession) shouldQueueInputWhileRunningLocked(now time.Time) bool {
	if strings.TrimSpace(rt.session.LastMode) != SessionModeAgent {
		return false
	}
	if rt.startupNotifyMode == startupNotifyDiscard {
		return false
	}
	return rt.session.Status == StatusRunning &&
		!rt.inputQueueUntil.IsZero() &&
		now.Before(rt.inputQueueUntil)
}

func (rt *RuntimeSession) OutputSnapshot() []byte {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	cp := make([]byte, len(rt.output))
	copy(cp, rt.output)
	return cp
}

func (rt *RuntimeSession) TerminalSize() (uint16, uint16) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.terminalSizeLocked()
}

func (rt *RuntimeSession) terminalSizeLocked() (uint16, uint16) {
	cols := rt.terminalCols
	rows := rt.terminalRows
	if cols < 80 {
		cols = defaultTerminalCols
	}
	if rows < 20 {
		rows = defaultTerminalRows
	}
	return cols, rows
}

func (rt *RuntimeSession) Subscribe() (chan RuntimeEvent, func()) {
	return rt.SubscribeWithMode(false)
}

func (rt *RuntimeSession) SubscribeWithMode(headless bool) (chan RuntimeEvent, func()) {
	ch := make(chan RuntimeEvent, 64)
	rt.mu.Lock()
	if rt.closed {
		close(ch)
		rt.mu.Unlock()
		return ch, func() {}
	}
	if rt.subscribers == nil {
		rt.subscribers = make(map[chan RuntimeEvent]runtimeSubscriber)
	}
	rt.subscribers[ch] = runtimeSubscriber{Headless: headless}
	// A headless renderer is commonly started after the request that needs it.
	// Dispatch still happens while holding rt.mu so cancel/terminal shutdown
	// cannot close the channel between the membership check and the send.
	for requestID, request := range rt.snapshotRequests {
		if request == nil || request.headless != headless || request.target != nil {
			continue
		}
		// Central dispatch enforces requiredTarget. A renderer that happens to
		// subscribe while an owner-bound request is pending must not steal it.
		rt.dispatchSnapshotRequestLocked(requestID, request)
	}
	sessionID := rt.session.ID
	reportBrowserActive := !headless && !rt.headlessRendererOwnsRoundLocked()
	rt.mu.Unlock()
	if reportBrowserActive && rt.manager != nil {
		rt.manager.BrowserActive(sessionID)
	}
	cancel := func() {
		rt.mu.Lock()
		if _, ok := rt.subscribers[ch]; ok {
			delete(rt.subscribers, ch)
			if rt.preferredBrowserSnapshotSource == ch {
				rt.preferredBrowserSnapshotSource = nil
			}
			if rt.preferredHeadlessSnapshotSource == ch {
				rt.preferredHeadlessSnapshotSource = nil
			}
			for requestID, request := range rt.snapshotRequests {
				if request == nil {
					continue
				}
				if request.requiredTarget == ch {
					// A round-owner request must never move to another renderer.
					// Removing it while holding rt.mu makes a simultaneous late
					// response fail closed and avoids racing channel closure.
					delete(rt.snapshotRequests, requestID)
					close(request.waiter)
					continue
				}
				if request.target != ch {
					continue
				}
				request.target = nil
				rt.dispatchSnapshotRequestLocked(requestID, request)
			}
			close(ch)
		}
		rt.mu.Unlock()
	}
	return ch, cancel
}

// headlessRendererOwnsRoundLocked protects the renderer identity used by the
// current round boundary. Opening the same session on another computer must
// not stop that headless renderer until a browser has successfully captured
// and committed the next round's input baseline.
func (rt *RuntimeSession) headlessRendererOwnsRoundLocked() bool {
	if rt.capturedInputBaselineHeadless {
		return true
	}
	if rt.snapshotAtRoundStartSet {
		if isHeadlessSnapshotSource(rt.snapshotAtRoundSource) {
			return true
		}
		if sub, ok := rt.subscribers[rt.snapshotAtRoundResponder]; ok && sub.Headless {
			return true
		}
	}
	for _, request := range rt.snapshotRequests {
		if request != nil && request.headless && request.purpose == SnapshotPurposeInputBaseline {
			return true
		}
	}
	return false
}

func (rt *RuntimeSession) dispatchSnapshotRequestLocked(requestID string, request *pendingSnapshotRequest) bool {
	if request == nil || request.target != nil {
		return request != nil && request.target != nil
	}
	if request.requiredTarget != nil {
		target := request.requiredTarget
		sub, ok := rt.subscribers[target]
		if !ok || sub.Headless != request.headless {
			return false
		}
		select {
		case target <- RuntimeEvent{Type: RuntimeEventSnapshotRequest, RequestID: requestID, Purpose: request.purpose}:
			request.target = target
			return true
		default:
			return false
		}
	}
	preferred := rt.preferredBrowserSnapshotSource
	if request.headless {
		preferred = rt.preferredHeadlessSnapshotSource
	}
	if preferred != nil {
		if sub, ok := rt.subscribers[preferred]; ok && sub.Headless == request.headless {
			select {
			case preferred <- RuntimeEvent{Type: RuntimeEventSnapshotRequest, RequestID: requestID, Purpose: request.purpose}:
				request.target = preferred
				return true
			default:
			}
		}
	}
	for ch, sub := range rt.subscribers {
		if sub.Headless != request.headless || ch == preferred {
			continue
		}
		select {
		case ch <- RuntimeEvent{Type: RuntimeEventSnapshotRequest, RequestID: requestID, Purpose: request.purpose}:
			request.target = ch
			return true
		default:
		}
	}
	return false
}

func (rt *RuntimeSession) WriteInput(data string) error {
	return rt.writeInput(data, nil, nil)
}

// WriteInputFrom binds subsequent browser snapshots and resize ownership to
// the WebSocket that is actively typing. With two computers connected, an
// idle renderer must not randomly become the source of this round's boundary.
func (rt *RuntimeSession) WriteInputFrom(data string, responder chan RuntimeEvent) error {
	return rt.writeInput(data, nil, responder)
}

type inputSnapshotBaseline struct {
	data      string
	source    string
	responder chan RuntimeEvent
}

// WriteInputWithSnapshotBaseline atomically binds the browser-rendered screen
// to the Enter that starts a new round. A snapshot response from another
// WebSocket cannot slip between the baseline update and the round-generation
// change and replace the boundary with a stale screen.
func (rt *RuntimeSession) WriteInputWithSnapshotBaseline(data string, snapshot string, source string) error {
	return rt.writeInput(data, &inputSnapshotBaseline{data: snapshot, source: source}, nil)
}

func (rt *RuntimeSession) WriteInputWithSnapshotBaselineFrom(data string, snapshot string, source string, responder chan RuntimeEvent) error {
	return rt.writeInput(data, &inputSnapshotBaseline{data: snapshot, source: source, responder: responder}, responder)
}

type agentRestartFollowUp struct {
	prompt        string
	mentionOpenID string
	messageID     string
}

func (rt *RuntimeSession) RestartAgent() error {
	return rt.restartAgent(nil)
}

func (rt *RuntimeSession) RestartAgentWithFollowUp(prompt, mentionOpenID, messageID string) error {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return rt.RestartAgent()
	}
	return rt.restartAgent(&agentRestartFollowUp{
		prompt:        prompt,
		mentionOpenID: strings.TrimSpace(mentionOpenID),
		messageID:     strings.TrimSpace(messageID),
	})
}

func (rt *RuntimeSession) restartAgent(followUp *agentRestartFollowUp) error {
	if rt == nil {
		return errors.New("会话不在线")
	}
	rt.mu.Lock()
	if rt.closed || rt.terminal == nil {
		rt.mu.Unlock()
		return errors.New("会话不在线")
	}
	if rt.agentRestartPending {
		rt.mu.Unlock()
		return errors.New("Agent 正在重启")
	}
	startCommand := strings.TrimSpace(rt.session.LastAgentStartCommand)
	agentKind := strings.TrimSpace(rt.session.LastAgentKind)
	agentID := strings.TrimSpace(rt.session.LastAgentID)
	command := startCommand
	if command == "" && rt.manager != nil {
		agent, _ := rt.manager.AgentConfig()
		command = strings.TrimSpace(agent.Command)
		startCommand = command
		agentKind = agent.Kind
		agentID = agent.ID
	}
	if command == "" {
		rt.mu.Unlock()
		return errors.New("当前会话没有可用的 Agent 启动命令")
	}
	resumeCommand := command
	if info, ok := agentLaunchInfo(shellFields(command)); ok {
		resumeCommand = info.ResumeCommand
	}
	rt.agentRestartPending = true
	terminal := rt.terminal
	rt.mu.Unlock()
	return rt.restartAgentAfterConfirmedExit(terminal, command, agentID, agentKind, startCommand, resumeCommand, followUp)
}

func (rt *RuntimeSession) SwitchAgent(optionID string) (AgentOption, error) {
	if rt == nil || rt.manager == nil {
		return AgentOption{}, errors.New("会话不在线")
	}
	option, ok := rt.manager.agentOption(optionID)
	if !ok {
		return AgentOption{}, errors.New("所选 Agent 当前不可用")
	}
	rt.mu.Lock()
	if rt.closed || rt.terminal == nil {
		rt.mu.Unlock()
		return AgentOption{}, errors.New("会话不在线")
	}
	if rt.agentRestartPending {
		rt.mu.Unlock()
		return AgentOption{}, errors.New("Agent 正在重启")
	}
	if strings.EqualFold(strings.TrimSpace(rt.session.LastAgentID), option.ID) {
		rt.mu.Unlock()
		return AgentOption{}, errors.New("当前已经是 " + option.Label)
	}
	rt.agentRestartPending = true
	terminal := rt.terminal
	rt.mu.Unlock()
	if err := rt.restartAgentAfterConfirmedExit(terminal, option.Command, option.ID, option.Kind, option.Command, "", nil); err != nil {
		return AgentOption{}, err
	}
	return option, nil
}

func (rt *RuntimeSession) restartAgentAfterConfirmedExit(terminal Terminal, launchCommand, agentID, agentKind, startCommand, resumeCommand string, followUp *agentRestartFollowUp) error {
	launchCommand = strings.TrimSpace(launchCommand)
	if launchCommand == "" {
		rt.mu.Lock()
		rt.agentRestartPending = false
		rt.mu.Unlock()
		return errors.New("当前会话没有可用的 Agent 启动命令")
	}
	rt.MarkAgentExitActivity()
	agentKind = agentKindForCommand(launchCommand, agentKind)
	go func() {
		if err := terminateAgentForegroundProcess(terminal); err != nil {
			rt.finishAgentRestartFailure()
			log.Printf("agent restart cancelled session=%s: %v", rt.Snapshot().ID, err)
			return
		}
		rt.RecordShellCommandForRecovery(launchCommand)
		rt.mu.Lock()
		rt.session.LastMode = SessionModeAgent
		rt.session.LastAgentID = strings.TrimSpace(agentID)
		rt.session.LastAgentKind = agentKind
		if strings.TrimSpace(startCommand) != "" {
			rt.session.LastAgentStartCommand = strings.TrimSpace(startCommand)
		}
		if strings.TrimSpace(resumeCommand) != "" {
			rt.session.LastAgentResumeCommand = strings.TrimSpace(resumeCommand)
		}
		baselineSnapshotVersion := rt.visibleSnapshotVersion
		baselineHistorySize := rt.session.HistorySize
		rt.snapshotRoundGeneration++
		rt.cancelSnapshotRequestsLocked(false)
		rt.snapshotAtRoundStartSet = false
		rt.snapshotAtRoundResponder = nil
		rt.capturedInputBaselineResponder = nil
		rt.capturedInputBaselineHeadless = false
		rt.agentTurnHookVerified = false
		rt.hookCompletedCurrentRound = false
		rt.hookCompletionTipClaimed = false
		rt.hookLastAssistantMessage = ""
		rt.session.UpdatedAt = time.Now().UTC()
		sess := rt.session
		rt.mu.Unlock()
		if rt.manager != nil {
			_ = rt.manager.persist(context.Background(), sess)
		}
		if !strings.HasSuffix(launchCommand, "\r") && !strings.HasSuffix(launchCommand, "\n") {
			launchCommand += "\r"
		}
		_, err := terminal.Write([]byte(launchCommand))
		if err != nil {
			rt.finishAgentRestartContextFailure("Agent 重启失败：启动命令未能写入终端。")
			log.Printf("agent restart command failed session=%s: %v", rt.Snapshot().ID, err)
			return
		}
		if followUp == nil {
			rt.mu.Lock()
			rt.agentRestartPending = false
			rt.mu.Unlock()
			return
		}
		if !rt.waitForRestartedAgentComposer(agentKind, baselineSnapshotVersion, baselineHistorySize, agentRestartContextTimeout) {
			rt.finishAgentRestartContextFailure("Agent 已重启，但上下文恢复指令发送失败。")
			log.Printf("agent restart context follow-up timed out session=%s agent=%s", rt.Snapshot().ID, agentKind)
			return
		}
		rt.mu.Lock()
		rt.notificationRunning = false
		rt.notificationPatchVersion++
		rt.mu.Unlock()
		if err := SubmitStructuredInputWithMention(rt, followUp.prompt, followUp.mentionOpenID); err != nil {
			rt.finishAgentRestartContextFailure("Agent 已重启，但上下文恢复指令发送失败。")
			log.Printf("agent restart context follow-up failed session=%s: %v", rt.Snapshot().ID, err)
			return
		}
		rt.NotifyInputRunningOnMessage(followUp.messageID)
		rt.mu.Lock()
		rt.agentRestartPending = false
		rt.mu.Unlock()
		log.Printf("agent restart context follow-up submitted session=%s agent=%s prompt_len=%d", rt.Snapshot().ID, agentKind, len(followUp.prompt))
	}()
	return nil
}

func (rt *RuntimeSession) waitForRestartedAgentComposer(agentKind string, baselineSnapshotVersion, baselineHistorySize int64, timeout time.Duration) bool {
	if rt == nil || timeout <= 0 {
		return false
	}
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		delay := minDuration(agentRestartReadyPollInterval, remaining)
		if delay > 0 {
			time.Sleep(delay)
		}
		remaining = time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		rt.requestFreshSnapshot(minDuration(defaultNotifySnapshotTimeout, remaining), SnapshotPurposeAgentRestart)
		rt.mu.Lock()
		live := !rt.closed && rt.session.Live
		ready := rt.visibleSnapshotVersion > baselineSnapshotVersion && rt.session.HistorySize > baselineHistorySize && startupAgentComposerReady(rt.visibleSnapshot, rt.visibleSnapshotSource, agentKind)
		rt.mu.Unlock()
		if !live {
			return false
		}
		if ready {
			return true
		}
		remaining = time.Until(deadline)
		if remaining <= 0 {
			return false
		}
	}
}

func (rt *RuntimeSession) finishAgentRestartContextFailure(message string) {
	message = strings.TrimSpace(message)
	rt.mu.Lock()
	rt.agentRestartPending = false
	rt.session.Status = StatusWaiting
	rt.session.UpdatedAt = time.Now().UTC()
	rt.stateVersion++
	rt.notifyVersion++
	rt.stopNotifyTimerLocked()
	rt.stopNotifyStableTimerLocked()
	if message != "" {
		if previous := strings.TrimSpace(rt.lastNotifiedContent); previous != "" {
			rt.lastNotifiedContent = previous + "\n\n" + message
		} else {
			rt.lastNotifiedContent = message
		}
	}
	note := WaitingNotification{}
	updateCard := false
	if rt.manager != nil {
		note, updateCard = rt.markNotificationWaitingLocked()
	}
	sess := rt.session
	rt.mu.Unlock()
	if rt.manager != nil {
		_ = rt.manager.persist(context.Background(), sess)
	}
	if updateCard {
		go rt.updateNotificationRunning(note, false)
	}
}

func (rt *RuntimeSession) finishAgentRestartFailure() {
	rt.mu.Lock()
	rt.agentRestartPending = false
	rt.session.LastMode = SessionModeAgent
	rt.session.Status = StatusWaiting
	rt.session.UpdatedAt = time.Now().UTC()
	rt.stateVersion++
	rt.notifyVersion++
	message := "Agent 重启失败：未能确认旧 Agent 已完全退出，因此没有执行新的启动命令。"
	if previous := strings.TrimSpace(rt.lastNotifiedContent); previous != "" {
		rt.lastNotifiedContent = previous + "\n\n" + message
	} else {
		rt.lastNotifiedContent = message
	}
	note := WaitingNotification{}
	updateCard := false
	if rt.manager != nil {
		note, updateCard = rt.markNotificationWaitingLocked()
	}
	sess := rt.session
	rt.mu.Unlock()
	if rt.manager != nil {
		_ = rt.manager.persist(context.Background(), sess)
	}
	if updateCard {
		go rt.updateNotificationRunning(note, false)
	}
}

func terminateAgentForegroundProcess(terminal Terminal) error {
	if terminal == nil {
		return io.ErrClosedPipe
	}
	if controller, ok := terminal.(ForegroundProcessController); ok {
		ctx, cancel := context.WithTimeout(context.Background(), defaultAgentTerminationTimeout)
		err := controller.TerminateForegroundProcess(ctx)
		cancel()
		if err == nil {
			return nil
		}
		if !errors.Is(err, errForegroundProcessControlUnavailable) {
			return err
		}
	}
	if _, err := terminal.Write([]byte("\x03\x03")); err != nil {
		return err
	}
	time.Sleep(defaultAgentRestartFallbackDelay)
	return nil
}

func (rt *RuntimeSession) writeInput(data string, baseline *inputSnapshotBaseline, responder chan RuntimeEvent) error {
	if data == "" {
		return nil
	}
	if !rt.recordInputActivityWithBaseline(data, inputChangesSessionState(data), baseline, responder) {
		return io.ErrClosedPipe
	}
	if strings.Contains(data, "\x03\x03") {
		rt.MarkAgentExitActivity()
	}
	if rt.terminal == nil {
		return io.ErrClosedPipe
	}
	_, err := rt.terminal.Write([]byte(data))
	return err
}

func (rt *RuntimeSession) SuppressStartupNotifications() {
	rt.mu.Lock()
	rt.startupNotifyMode = startupNotifySuppress
	rt.mu.Unlock()
}

func (rt *RuntimeSession) discardingStartupNotifications() bool {
	if rt == nil {
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.startupNotifyMode == startupNotifyDiscard
}

func (rt *RuntimeSession) FinishStartupNotifications() {
	rt.finishStartupNotificationsAfter(defaultStartupPresetSettleDelay)
}

func (rt *RuntimeSession) finishStartupNotificationsAfter(delay time.Duration) {
	rt.mu.Lock()
	if rt.startupNotifyMode == startupNotifySuppress {
		rt.startupNotifyMode = startupNotifySettling
		rt.scheduleStartupNotifyFinalLocked(delay)
	}
	rt.mu.Unlock()
}

func (rt *RuntimeSession) runPreStartCommand() {
	command := strings.TrimSpace(rt.manager.preStartCommand)
	if command == "" {
		return
	}
	rt.RecordShellCommandForRecovery(command)
	if !strings.HasSuffix(command, "\r") && !strings.HasSuffix(command, "\n") {
		command += "\r"
	}
	if _, err := rt.terminal.Write([]byte(command)); err != nil {
		log.Printf("pre-start command failed session=%s: %v", rt.session.ID, err)
	}
}

func (rt *RuntimeSession) runRecoveryEnvironmentSetup() {
	if rt == nil || rt.manager == nil {
		return
	}
	rt.mu.Lock()
	sess := rt.session
	rt.mu.Unlock()
	var exports []string
	claudeHome := rt.manager.sessionClaudeHome(sess)
	if claudeHome != "" {
		if err := ensureClaudeSessionHome(claudeHome); err != nil {
			log.Printf("recovery claude home setup failed session=%s: %v", sess.ID, err)
		}
		exports = append(exports, "CLAUDE_CONFIG_DIR="+shellQuote(claudeHome))
	}
	if hookURL := rt.manager.AgentTurnHookURL(); hookURL != "" && strings.TrimSpace(sess.RecoveryKey) != "" {
		exports = append(exports,
			"IRIS_API_URL="+shellQuote(hookURL),
			"IRIS_SESSION_ID="+shellQuote(sess.ID),
			"IRIS_SESSION_TOKEN="+shellQuote(sess.RecoveryKey),
		)
	}
	command := ""
	if strings.TrimSpace(rt.manager.recoveryBaseDir) != "" {
		command = "unset CODEX_HOME"
	}
	if len(exports) > 0 {
		if command != "" {
			command += "; "
		}
		command += "export " + strings.Join(exports, " ")
	}
	if command == "" {
		return
	}
	command += "\r"
	if _, err := rt.terminal.Write([]byte(command)); err != nil {
		log.Printf("recovery environment setup failed session=%s: %v", sess.ID, err)
	}
}

func (rt *RuntimeSession) runRecoveryCommand() {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	sess := rt.session
	rt.mu.Unlock()
	if cwd := strings.TrimSpace(sess.LastCWD); cwd != "" {
		if _, err := rt.terminal.Write([]byte("cd " + shellQuote(cwd) + "\r")); err != nil {
			log.Printf("recovery cwd restore failed session=%s cwd=%q: %v", sess.ID, cwd, err)
		}
	}
	if strings.TrimSpace(sess.LastMode) != SessionModeAgent || strings.TrimSpace(sess.LastAgentResumeCommand) == "" {
		return
	}
	command := strings.TrimSpace(sess.LastAgentResumeCommand)
	if strings.TrimSpace(sess.LastAgentKind) == "codex" && codexHomeIsLegacy(sess.LastAgentHome) {
		command = "CODEX_HOME=" + shellQuote(sess.LastAgentHome) + " " + command
	}
	if !strings.HasSuffix(command, "\r") && !strings.HasSuffix(command, "\n") {
		command += "\r"
	}
	if _, err := rt.terminal.Write([]byte(command)); err != nil {
		log.Printf("agent resume command failed session=%s agent=%s: %v", sess.ID, sess.LastAgentKind, err)
	}
}

func (rt *RuntimeSession) Resize(cols, rows uint16) error {
	return rt.ResizeFrom(cols, rows, nil)
}

func (rt *RuntimeSession) ResizeFrom(cols, rows uint16, responder chan RuntimeEvent) error {
	if cols < 80 || rows < 20 {
		return nil
	}
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		return io.ErrClosedPipe
	}
	if responder != nil {
		sub, ok := rt.subscribers[responder]
		if !ok || sub.Headless || (rt.preferredBrowserSnapshotSource != nil && rt.preferredBrowserSnapshotSource != responder) {
			rt.mu.Unlock()
			return nil
		}
	}
	currentCols, currentRows := rt.terminalSizeLocked()
	terminal := rt.terminal
	if currentCols == cols && currentRows == rows {
		rt.mu.Unlock()
		return nil
	}
	rt.mu.Unlock()
	if terminal == nil {
		return nil
	}
	if err := terminal.Resize(cols, rows); err != nil {
		return err
	}
	rt.mu.Lock()
	rt.terminalCols = cols
	rt.terminalRows = rows
	for ch, sub := range rt.subscribers {
		if !sub.Headless {
			continue
		}
		select {
		case ch <- RuntimeEvent{Type: RuntimeEventTerminalResize, Cols: cols, Rows: rows}:
		default:
		}
	}
	rt.mu.Unlock()
	return nil
}

func (rt *RuntimeSession) SetVisibleSnapshot(data string) {
	rt.SetVisibleSnapshotWithSource(data, "legacy")
}

func (rt *RuntimeSession) applyInputSnapshotBaselineLocked(data string, source string, responder chan RuntimeEvent) (string, int64) {
	source = strings.TrimSpace(source)
	if source == "" {
		source = "browser:input-baseline"
	}
	rt.visibleSnapshot = data
	rt.visibleSnapshotSource = source
	rt.visibleSnapshotResponder = responder
	rt.visibleSnapshotCols = rt.terminalCols
	rt.visibleSnapshotVersion++
	return source, rt.visibleSnapshotVersion
}

func (rt *RuntimeSession) SetVisibleSnapshotWithSource(data string, source string) {
	rt.setVisibleSnapshot(data, source, "", nil, true)
}

// SetVisibleSnapshotResponse applies a snapshot returned for a specific
// request. Late or unrelated responses must not overwrite the canonical
// snapshot used as the current round boundary.
func (rt *RuntimeSession) SetVisibleSnapshotResponse(data string, source string, requestID string) {
	rt.SetVisibleSnapshotResponseFrom(data, source, requestID, nil)
}

// SetVisibleSnapshotResponseFrom additionally binds the response to the
// subscriber that received the request. A disconnected renderer cannot race
// its late response against a request that has already been reassigned.
func (rt *RuntimeSession) SetVisibleSnapshotResponseFrom(data string, source string, requestID string, responder chan RuntimeEvent) {
	rt.setVisibleSnapshot(data, source, strings.TrimSpace(requestID), responder, false)
}

func (rt *RuntimeSession) setVisibleSnapshot(data string, source string, requestID string, responder chan RuntimeEvent, legacy bool) {
	source = strings.TrimSpace(source)
	if source == "" {
		source = "unknown"
	}
	rt.mu.Lock()
	if rt.closed {
		sessionID := rt.session.ID
		rt.mu.Unlock()
		log.Printf("visible snapshot ignored session=%s source=%s request_id=%s reason=session_closed len=%d", sessionID, source, requestID, len(data))
		return
	}
	var acceptedRequest *pendingSnapshotRequest
	waiters := make([]chan struct{}, 0, 1)
	if !legacy {
		if requestID == "" && responder != nil {
			requestID = rt.uniqueLegacySnapshotRequestIDLocked(source, responder)
		}
		if rt.closed || !rt.session.Live || requestID == "" {
			sessionID := rt.session.ID
			rt.mu.Unlock()
			reason := "missing_request_id"
			if requestID != "" {
				reason = "session_not_live"
			}
			log.Printf("visible snapshot ignored session=%s source=%s request_id=%s reason=%s len=%d", sessionID, source, requestID, reason, len(data))
			return
		}
		acceptedRequest = rt.snapshotRequests[requestID]
		wrongResponder := responder != nil && acceptedRequest != nil && acceptedRequest.target != responder
		if acceptedRequest == nil || wrongResponder || acceptedRequest.round != rt.snapshotRoundGeneration || acceptedRequest.headless != isHeadlessSnapshotSource(source) {
			sessionID := rt.session.ID
			rt.mu.Unlock()
			log.Printf("visible snapshot ignored session=%s source=%s request_id=%s reason=stale_wrong_source_or_responder len=%d", sessionID, source, requestID, len(data))
			return
		}
		delete(rt.snapshotRequests, requestID)
		waiters = append(waiters, acceptedRequest.waiter)
		if acceptedRequest.seq < rt.latestAppliedSnapshotRequestID {
			// A newer correlated request has already produced the canonical
			// snapshot. Wake this caller as satisfied so it does not launch a
			// redundant headless fallback that could replace the newer view.
			acceptedRequest.applied = true
			sessionID := rt.session.ID
			latest := rt.latestAppliedSnapshotRequestID
			rt.mu.Unlock()
			close(acceptedRequest.waiter)
			log.Printf("visible snapshot ignored session=%s source=%s request_id=%s reason=older_than_applied request_seq=%d latest_seq=%d len=%d", sessionID, source, requestID, acceptedRequest.seq, latest, len(data))
			return
		}
		rt.latestAppliedSnapshotRequestID = acceptedRequest.seq
		acceptedRequest.applied = true
		if acceptedRequest.target != nil {
			if acceptedRequest.headless {
				rt.preferredHeadlessSnapshotSource = acceptedRequest.target
			} else {
				rt.preferredBrowserSnapshotSource = acceptedRequest.target
			}
		}
	} else {
		// Preserve the direct API used by tests and internal callers. Production
		// WebSocket responses always take the correlated path above.
		if isBrowserSnapshotSource(source) && (rt.pendingHeadlessSnapshots > 0 || (rt.hasHeadlessSubscriberLocked() && !rt.hasRealSubscriberLocked())) {
			sessionID := rt.session.ID
			rt.mu.Unlock()
			log.Printf("visible snapshot ignored session=%s source=%s reason=headless_snapshot_active len=%d", sessionID, source, len(data))
			return
		}
		for pendingID, request := range rt.snapshotRequests {
			if request == nil {
				continue
			}
			delete(rt.snapshotRequests, pendingID)
			request.applied = true
			if request.seq > rt.latestAppliedSnapshotRequestID {
				rt.latestAppliedSnapshotRequestID = request.seq
			}
			waiters = append(waiters, request.waiter)
		}
	}
	rt.visibleSnapshot = data
	rt.visibleSnapshotSource = source
	if responder == nil && acceptedRequest != nil {
		responder = acceptedRequest.target
	}
	rt.visibleSnapshotResponder = responder
	rt.visibleSnapshotCols = rt.terminalCols
	rt.visibleSnapshotVersion++
	if acceptedRequest != nil && acceptedRequest.purpose == SnapshotPurposeInputBaseline {
		rt.capturedInputBaselineResponder = responder
		rt.capturedInputBaselineHeadless = acceptedRequest.headless
	}
	var interactionNotifyVersion int64
	var interactionSession Session
	if rt.manager != nil && rt.session.Status == StatusRunning && rt.hasPendingCodexInteractionLocked() {
		rt.stopNotifyTimerLocked()
		rt.stopNotifyStableTimerLocked()
		rt.session.Status = StatusWaiting
		rt.session.UpdatedAt = time.Now().UTC()
		rt.stateVersion++
		rt.notifyVersion++
		interactionNotifyVersion = rt.notifyVersion
		interactionSession = rt.session
	}
	version := rt.visibleSnapshotVersion
	sessionID := rt.session.ID
	rt.mu.Unlock()
	log.Printf("visible snapshot updated session=%s source=%s request_id=%s version=%d len=%d lines=%d waiters=%d", sessionID, source, requestID, version, len(data), countLogLines(data), len(waiters))
	for _, ch := range waiters {
		close(ch)
	}
	if interactionNotifyVersion != 0 {
		_ = rt.manager.persist(context.Background(), interactionSession)
		go rt.notifyIfStillWaitingForInteraction(interactionNotifyVersion)
	}
}

// uniqueLegacySnapshotRequestIDLocked keeps one release of compatibility with
// pages that were already open before request IDs were introduced. The
// response is accepted only when its exact WebSocket subscriber owns one
// unambiguous request in the current round; otherwise it remains fail-closed.
func (rt *RuntimeSession) uniqueLegacySnapshotRequestIDLocked(source string, responder chan RuntimeEvent) string {
	wantHeadless := isHeadlessSnapshotSource(source)
	candidate := ""
	for requestID, request := range rt.snapshotRequests {
		if request == nil || request.target != responder || request.round != rt.snapshotRoundGeneration || request.headless != wantHeadless {
			continue
		}
		if candidate != "" {
			return ""
		}
		candidate = requestID
	}
	return candidate
}

func isHeadlessSnapshotSource(source string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(source)), "headless:")
}

func isBrowserSnapshotSource(source string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(source)), "browser:")
}

func (rt *RuntimeSession) RequireLarkChatForNotifications() {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	rt.requireLarkChat = true
	rt.mu.Unlock()
}

func (rt *RuntimeSession) SetNotificationMentionOpenID(openID string) {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	rt.notificationMentionOpenID = strings.TrimSpace(openID)
	rt.mu.Unlock()
}

func (rt *RuntimeSession) NotificationMentionOpenID() string {
	if rt == nil {
		return ""
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.notificationMentionOpenID
}

func (rt *RuntimeSession) CurrentRoundContent() string {
	content, _ := rt.currentRoundContentWithFreshSnapshot(800 * time.Millisecond)
	return content
}

func (rt *RuntimeSession) CurrentRoundRawContent() string {
	rt.RequestFreshSnapshot(800 * time.Millisecond)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return pickRawNotifyContentWithWindowAnchorPolicy(rt.visibleSnapshot, rt.previousNotifySnapshotLocked(), rt.roundReply, rt.lastInputText, rt.notificationWindowInputText, rt.notifyTextAnchorPolicyLocked())
}

func (rt *RuntimeSession) CachedCurrentRoundContent() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.currentNotifyContentLocked()
}

// CurrentRoundDebug exposes the exact renderer boundary used by the
// environment-gated E2E debug endpoint. It intentionally lives below the
// runtime lock so a real browser/Codex test can diagnose anchor failures
// without adding sensitive terminal text to normal service logs.
func (rt *RuntimeSession) CurrentRoundDebug() map[string]any {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	policy := rt.notifyTextAnchorPolicyLocked()
	return map[string]any{
		"content":                   rt.currentNotifyContentLocked(),
		"current_snapshot":          rt.visibleSnapshot,
		"current_source":            rt.visibleSnapshotSource,
		"previous_snapshot":         rt.previousNotifySnapshotLocked(),
		"previous_source":           rt.snapshotAtRoundSource,
		"last_input":                rt.lastInputText,
		"window_start_input":        rt.notificationWindowInputText,
		"anchor_allowed":            policy.allowed,
		"anchor_identity":           policy.enforceIdentity,
		"previous_guard_line":       policy.previousGuardLine,
		"current_guard_line":        policy.currentGuardLine,
		"previous_cursor_line":      policy.previousCursorLine,
		"current_cursor_line":       policy.currentCursorLine,
		"previous_snapshot_version": rt.snapshotAtRoundVersion,
		"current_snapshot_version":  rt.visibleSnapshotVersion,
	}
}

func (rt *RuntimeSession) CurrentVisibleContent() string {
	rt.RequestFreshSnapshot(800 * time.Millisecond)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	content := pickNotifyContentWithWindowAnchorPolicy(rt.visibleSnapshot, rt.previousNotifySnapshotLocked(), rt.roundReply, rt.lastInputText, rt.notificationWindowInputText, rt.notifyTextAnchorPolicyLocked())
	return rt.cleanLarkNotifyContentForAgentLocked(content)
}

func (rt *RuntimeSession) previousNotifySnapshotLocked() string {
	if rt.snapshotAtRoundStartSet {
		return rt.snapshotAtRoundStart
	}
	return rt.lastNotifiedVisibleSnapshot
}

func (rt *RuntimeSession) currentNotifyContentLocked() string {
	if content := rt.hookAssistantNotifyContentLocked(); content != "" {
		return content
	}
	content := pickNotifyContentWithWindowAnchorPolicy(rt.visibleSnapshot, rt.previousNotifySnapshotLocked(), rt.roundReply, rt.lastInputText, rt.notificationWindowInputText, rt.notifyTextAnchorPolicyLocked())
	return rt.cleanLarkNotifyContentForAgentLocked(content)
}

func (rt *RuntimeSession) hookAssistantNotifyContentLocked() string {
	return pickLarkNotifyHookAssistantContent(rt.hookLastAssistantMessage)
}

func (rt *RuntimeSession) cleanLarkNotifyContentForAgentLocked(content string) string {
	if isRawLarkNotifyInput(rt.lastInputText) {
		return content
	}
	return cleanLarkNotifyContentForAgent(content, rt.session.LastMode, rt.session.LastAgentKind)
}

// previousTextAnchorsAllowedLocked limits input/tail occurrence matching to
// snapshots captured by the same renderer at the same terminal width. Strict
// append diffs may still succeed across a reconnect, but a short text sequence
// from another computer or DOM mode is not allowed to identify this round.
func (rt *RuntimeSession) previousTextAnchorsAllowedLocked() bool {
	return rt.notifyTextAnchorPolicyLocked().allowed
}

func (rt *RuntimeSession) notifyTextAnchorPolicyLocked() notifyTextAnchorPolicy {
	policy := notifyTextAnchorPolicy{
		previousGuardLine:  -1,
		currentGuardLine:   -1,
		previousCursorLine: -1,
		currentCursorLine:  -1,
	}
	previousSource := rt.lastNotifiedVisibleSnapshotSource
	previousResponder := rt.lastNotifiedVisibleResponder
	previousCols := rt.lastNotifiedVisibleCols
	if rt.snapshotAtRoundStartSet {
		previousSource = rt.snapshotAtRoundSource
		previousResponder = rt.snapshotAtRoundResponder
		previousCols = rt.snapshotAtRoundCols
	}
	if previousResponder != rt.visibleSnapshotResponder {
		return policy
	}
	previousMetadata := parseSnapshotSourceContinuity(previousSource)
	currentMetadata := parseSnapshotSourceContinuity(rt.visibleSnapshotSource)
	if previousMetadata.present || currentMetadata.present {
		if !previousMetadata.valid || !currentMetadata.valid ||
			previousMetadata.continuityVersion != 2 || currentMetadata.continuityVersion != 2 ||
			previousMetadata.base != currentMetadata.base ||
			!isBufferSnapshotContinuityBase(previousMetadata.base) ||
			previousMetadata.renderEpoch != currentMetadata.renderEpoch ||
			previousCols != rt.visibleSnapshotCols ||
			previousMetadata.bufferType != "normal" || currentMetadata.bufferType != "normal" ||
			!previousMetadata.anchorGuardActive || !currentMetadata.anchorGuardActive ||
			previousMetadata.anchorGuardLine < 0 || currentMetadata.anchorGuardLine < 0 {
			return policy
		}
		// Capacity may legitimately change from false to true during one long
		// reply. The baseline marker makes that transition safe as long as it is
		// alive in both snapshots. Capacity cannot decrease within one epoch.
		if previousMetadata.bufferAtCapacity && !currentMetadata.bufferAtCapacity {
			return policy
		}
		policy.allowed = true
		policy.enforceIdentity = true
		policy.previousGuardLine = previousMetadata.anchorGuardLine
		policy.currentGuardLine = currentMetadata.anchorGuardLine
		policy.previousCursorLine = previousMetadata.cursorLine
		policy.currentCursorLine = currentMetadata.cursorLine
		return policy
	}
	if previousCols != 0 && rt.visibleSnapshotCols != 0 && previousCols != rt.visibleSnapshotCols {
		return policy
	}
	// Direct in-process callers and older unit fixtures have no WebSocket
	// responder and retain their legacy behavior. A real renderer without the
	// continuity metadata is not allowed to identify a round by text: an old
	// page, alternate buffer, or trimmed scrollback could otherwise make a
	// historical occurrence look like the current boundary.
	policy.allowed = previousSource == rt.visibleSnapshotSource && previousResponder == nil && rt.visibleSnapshotResponder == nil
	return policy
}

type snapshotSourceContinuity struct {
	base              string
	continuityVersion uint64
	renderEpoch       uint64
	bufferType        string
	bufferAtCapacity  bool
	anchorGuardActive bool
	anchorGuardLine   int
	cursorLine        int
	present           bool
	valid             bool
}

func isBufferSnapshotContinuityBase(base string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(base)), ":buffer")
}

// parseSnapshotSourceContinuity decodes renderer continuity metadata appended
// by the WebSocket bridge. The capture source remains separate so a buffer
// snapshot can never be matched against a DOM fallback, even within one epoch.
func parseSnapshotSourceContinuity(source string) snapshotSourceContinuity {
	parts := strings.Split(source, ";")
	metadata := snapshotSourceContinuity{base: strings.TrimSpace(parts[0]), anchorGuardLine: -1, cursorLine: -1}
	values := make(map[string]string, 7)
	for _, part := range parts[1:] {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		switch key {
		case "continuity_version", "render_epoch", "buffer_type", "buffer_at_capacity", "anchor_guard_active", "anchor_guard_line", "cursor_line":
			metadata.present = true
			if _, duplicate := values[key]; duplicate {
				return metadata
			}
			values[key] = strings.TrimSpace(strings.ToLower(value))
		}
	}
	if !metadata.present {
		return metadata
	}
	metadata.bufferType = values["buffer_type"]
	continuityVersion, err := strconv.ParseUint(values["continuity_version"], 10, 64)
	if err != nil || continuityVersion != 2 {
		return metadata
	}
	epoch, err := strconv.ParseUint(values["render_epoch"], 10, 64)
	if err != nil || epoch == 0 || metadata.base == "" || metadata.bufferType == "" {
		return metadata
	}
	metadata.continuityVersion = continuityVersion
	metadata.renderEpoch = epoch
	capacity, capacityKnown := values["buffer_at_capacity"]
	if !capacityKnown || (capacity != "true" && capacity != "false") {
		return metadata
	}
	guard, guardKnown := values["anchor_guard_active"]
	if !guardKnown || (guard != "true" && guard != "false") {
		return metadata
	}
	guardLine, err := strconv.Atoi(values["anchor_guard_line"])
	if err != nil || guardLine < -1 || (guard == "true" && guardLine < 0) {
		return metadata
	}
	metadata.bufferAtCapacity = capacity == "true"
	metadata.anchorGuardActive = guard == "true"
	metadata.anchorGuardLine = guardLine
	if cursorValue, ok := values["cursor_line"]; ok {
		cursorLine, err := strconv.Atoi(cursorValue)
		if err != nil || cursorLine < -1 {
			return metadata
		}
		metadata.cursorLine = cursorLine
	}
	metadata.valid = true
	return metadata
}

func startupAgentComposerReady(snapshot, source, agentKind string) bool {
	switch strings.ToLower(strings.TrimSpace(agentKind)) {
	case "codex", "claude", "aiden":
	default:
		return true
	}
	cursorLine := parseSnapshotSourceContinuity(source).cursorLine
	lines := splitVisibleLines(snapshot)
	if cursorLine < 0 || cursorLine >= len(lines) {
		return false
	}
	_, ready := submittedInputPromptText(lines[cursorLine])
	return ready
}

func (rt *RuntimeSession) currentRoundContentWithFreshSnapshot(timeout time.Duration) (string, bool) {
	fresh := rt.RequestFreshSnapshot(timeout)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.currentNotifyContentLocked(), fresh
}

func (rt *RuntimeSession) stableNotifyContentForMessageLocked(messageID string, content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return content
	}
	messageID = strings.TrimSpace(messageID)
	if messageID != "" && rt.lastNotifiedMessageID != "" && messageID != rt.lastNotifiedMessageID {
		return content
	}
	if rt.lastNotifiedMessageID == "" {
		return content
	}
	previous := strings.TrimSpace(rt.lastNotifiedContent)
	if !shouldPreservePreviousNotifyContent(previous, content) {
		return content
	}
	log.Printf("lark notify shorter content suppressed session=%s message=%s previous_len=%d candidate_len=%d",
		rt.session.ID, rt.lastNotifiedMessageID, len(previous), len(content))
	return previous
}

func (rt *RuntimeSession) NotificationMessageFrozen(messageID string) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.notificationMessageFrozenLocked(messageID)
}

func (rt *RuntimeSession) ValidateNotificationAction(messageID string) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.validateNotificationActionLocked(messageID)
}

func (rt *RuntimeSession) ValidateNotificationRefresh(messageID string) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return nil
	}
	if rt.notificationMessageFrozenLocked(messageID) {
		return errNotificationMessageDisabled
	}
	return nil
}

func (rt *RuntimeSession) validateNotificationActionLocked(messageID string) error {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return nil
	}
	if rt.notificationMessageFrozenLocked(messageID) {
		return errNotificationMessageDisabled
	}
	if rt.lastNotifiedMessageID != "" && rt.lastNotifiedMessageID != messageID {
		return errNotificationMessageDisabled
	}
	return nil
}

func (rt *RuntimeSession) notificationMessageFrozenLocked(messageID string) bool {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" || len(rt.frozenNotificationMessages) == 0 {
		return false
	}
	_, ok := rt.frozenNotificationMessages[messageID]
	return ok
}

func (rt *RuntimeSession) freezeNotificationMessageLocked(messageID string) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return
	}
	if rt.frozenNotificationMessages == nil {
		rt.frozenNotificationMessages = make(map[string]struct{})
	}
	rt.frozenNotificationMessages[messageID] = struct{}{}
	if rt.autoRefreshMessageID == messageID {
		rt.autoRefreshMessageID = ""
	}
}

func (rt *RuntimeSession) disabledNotificationLocked(messageID string) (WaitingNotification, bool) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" || rt.manager == nil || rt.manager.notifier == nil || !rt.manager.notifier.Available() {
		return WaitingNotification{}, false
	}
	content := strings.TrimSpace(rt.lastNotifiedContent)
	if content == "" {
		content = RunningNotificationPlaceholder
	}
	return WaitingNotification{
		SessionID:          rt.session.ID,
		Name:               rt.session.Name,
		Content:            content,
		MessageID:          messageID,
		ChatID:             rt.session.LarkChatID,
		MentionOpenID:      rt.notificationMentionOpenID,
		UpdateNo:           rt.notificationUpdateNo,
		Running:            false,
		Disabled:           true,
		AutoRefreshEnabled: false,
		AutoSummaryEnabled: false,
		MentionModeEnabled: rt.session.LarkMentionModeEnabled,
		SuppressUpdateTip:  true,
		AgentContext:       cloneTerminalAgentContext(rt.lastTerminalAgentContext),
	}, true
}

func (rt *RuntimeSession) RequestFreshSnapshot(timeout time.Duration) bool {
	return rt.requestFreshSnapshotFrom(timeout, "", nil)
}

func (rt *RuntimeSession) requestFreshSnapshot(timeout time.Duration, purpose string) bool {
	return rt.requestFreshSnapshotFrom(timeout, purpose, nil)
}

func (rt *RuntimeSession) requestFreshSnapshotFrom(timeout time.Duration, purpose string, origin chan RuntimeEvent) bool {
	if timeout <= 0 {
		return false
	}
	purpose = strings.TrimSpace(purpose)
	primaryTimeout := timeout
	allowHeadlessFallback := false
	primaryHeadless := false
	requiredTarget := origin
	roundOwnerPinned := false
	disconnectedRoundOwner := false
	rt.mu.Lock()
	roundSessionID := rt.session.ID
	if purpose != SnapshotPurposeInputBaseline && rt.snapshotAtRoundStartSet && rt.snapshotAtRoundResponder != nil {
		if _, connected := rt.subscribers[rt.snapshotAtRoundResponder]; connected {
			requiredTarget = rt.snapshotAtRoundResponder
			roundOwnerPinned = true
		} else {
			// A renderer that owned the round may disappear after sleep, a page
			// reload, or headless startup. Keeping a dead channel pinned forever
			// makes every completion snapshot stale and prevents the waiting card
			// from ever being finalized. Once the owner is actually absent from
			// subscribers, allow the normal correlated-request path to select a
			// live renderer. A still-connected owner remains strictly pinned.
			requiredTarget = nil
			disconnectedRoundOwner = true
		}
	}
	if requiredTarget != nil {
		if sub, ok := rt.subscribers[requiredTarget]; ok {
			primaryHeadless = sub.Headless
		}
	}
	canStartHeadless := rt.manager != nil && rt.manager.onBrowserNeeded != nil
	canUseHeadless := rt.headlessSubscriberCountLocked() > 0 || canStartHeadless
	if !roundOwnerPinned && rt.session.Live && canUseHeadless &&
		(rt.realSubscriberCountLocked() > 0 || (purpose == SnapshotPurposeInputBaseline && origin != nil)) {
		allowHeadlessFallback = true
		if timeout >= 300*time.Millisecond {
			primaryTimeout = minDuration(timeout/2, 500*time.Millisecond)
		}
	}
	rt.mu.Unlock()
	if disconnectedRoundOwner {
		log.Printf("snapshot round owner disconnected; allowing live renderer takeover session=%s", roundSessionID)
	}
	fresh, attempted := rt.requestFreshSnapshotAttempt(primaryTimeout, primaryHeadless, purpose, requiredTarget)
	if roundOwnerPinned {
		return fresh
	}
	if fresh || !attempted || !allowHeadlessFallback {
		return fresh
	}
	fallbackTimeout := timeout - primaryTimeout
	if fallbackTimeout < timeout {
		fallbackTimeout = timeout
	}
	fallbackTimeout = minDuration(fallbackTimeout, defaultNotifySnapshotDeadline)
	if fallbackTimeout <= 0 {
		return false
	}
	fresh, _ = rt.requestFreshSnapshotAttempt(fallbackTimeout, true, purpose, nil)
	return fresh
}

func (rt *RuntimeSession) requestFreshSnapshotAttempt(timeout time.Duration, forceHeadless bool, purpose string, requiredTarget chan RuntimeEvent) (bool, bool) {
	if timeout <= 0 {
		return false, false
	}
	headlessRequest := false
	rt.mu.Lock()
	if !rt.session.Live {
		rt.mu.Unlock()
		return false, false
	}
	sessionID := rt.session.ID
	hasSubscribers := len(rt.subscribers) > 0
	headlessSubscribers := rt.headlessSubscriberCountLocked()
	realSubscribers := rt.realSubscriberCountLocked()
	canStartHeadless := rt.manager != nil && rt.manager.onBrowserNeeded != nil
	useHeadless := false
	if requiredTarget != nil {
		sub, ok := rt.subscribers[requiredTarget]
		if !ok {
			rt.mu.Unlock()
			return false, true
		}
		useHeadless = sub.Headless
	} else {
		useHeadless = (forceHeadless && (headlessSubscribers > 0 || canStartHeadless)) || (realSubscribers == 0 && (headlessSubscribers > 0 || canStartHeadless))
	}
	needsBrowser := useHeadless && headlessSubscribers == 0 && canStartHeadless
	if !hasSubscribers && !needsBrowser {
		rt.mu.Unlock()
		return false, false
	}
	if rt.snapshotRequests == nil {
		rt.snapshotRequests = make(map[string]*pendingSnapshotRequest)
	}
	rt.nextSnapshotRequestID++
	requestSeq := rt.nextSnapshotRequestID
	requestID := fmt.Sprintf("snapshot-%s-%d", sessionID, requestSeq)
	waiter := make(chan struct{})
	request := &pendingSnapshotRequest{waiter: waiter, headless: useHeadless, seq: requestSeq, round: rt.snapshotRoundGeneration, requiredTarget: requiredTarget, purpose: purpose}
	rt.snapshotRequests[requestID] = request
	if useHeadless {
		rt.pendingHeadlessSnapshots++
		headlessRequest = true
	}
	dispatched := rt.dispatchSnapshotRequestLocked(requestID, request)
	if requiredTarget != nil && !dispatched {
		delete(rt.snapshotRequests, requestID)
		if headlessRequest && rt.pendingHeadlessSnapshots > 0 {
			rt.pendingHeadlessSnapshots--
		}
		rt.mu.Unlock()
		return false, true
	}
	rt.mu.Unlock()
	if needsBrowser {
		rt.manager.RestartBrowser(sessionID)
		timeout = rt.manager.headlessSnapshotTimeout()
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-waiter:
	case <-timer.C:
	}
	rt.mu.Lock()
	applied := request.applied
	if request.requiredTarget == nil {
		applied = applied ||
			(request.round == rt.snapshotRoundGeneration && rt.latestAppliedSnapshotRequestID > request.seq)
	}
	delete(rt.snapshotRequests, requestID)
	if headlessRequest && rt.pendingHeadlessSnapshots > 0 {
		rt.pendingHeadlessSnapshots--
	}
	fresh := applied
	subscriberCount := len(rt.subscribers)
	realSubscriberCount := rt.realSubscriberCountLocked()
	headlessSubscriberCount := rt.headlessSubscriberCountLocked()
	rt.mu.Unlock()
	log.Printf("snapshot request finished session=%s request_id=%s purpose=%s fresh=%v dispatched=%v subscribers=%d real_subscribers=%d headless_subscribers=%d needed_browser=%v headless_request=%v timeout=%s", sessionID, requestID, purpose, fresh, dispatched, subscriberCount, realSubscriberCount, headlessSubscriberCount, needsBrowser, useHeadless, timeout)
	return fresh, true
}

func (rt *RuntimeSession) hasRealSubscriberLocked() bool {
	return rt.realSubscriberCountLocked() > 0
}

func (rt *RuntimeSession) realSubscriberCountLocked() int {
	count := 0
	for _, sub := range rt.subscribers {
		if !sub.Headless {
			count++
		}
	}
	return count
}

func (rt *RuntimeSession) hasHeadlessSubscriberLocked() bool {
	return rt.headlessSubscriberCountLocked() > 0
}

func (rt *RuntimeSession) headlessSubscriberCountLocked() int {
	count := 0
	for _, sub := range rt.subscribers {
		if sub.Headless {
			count++
		}
	}
	return count
}

func (rt *RuntimeSession) MarkInputActivity(data string) {
	rt.recordInputActivity(data, true)
}

func (rt *RuntimeSession) recordInputActivity(data string, changeSessionState bool) {
	rt.recordInputActivityWithBaseline(data, changeSessionState, nil, nil)
}

func (rt *RuntimeSession) recordInputActivityWithBaseline(data string, changeSessionState bool, baseline *inputSnapshotBaseline, responder chan RuntimeEvent) bool {
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		return false
	}
	rt.preferBrowserSnapshotSourceLocked(responder)
	baselineSource := ""
	baselineVersion := int64(0)
	if baseline != nil && rt.session.Live {
		rt.preferBrowserSnapshotSourceLocked(baseline.responder)
		baselineSource, baselineVersion = rt.applyInputSnapshotBaselineLocked(baseline.data, baseline.source, baseline.responder)
	}
	previousInput := rt.lastInputText
	submitted := rt.recordInputLocked(data)
	if submitted {
		rt.updateRecoveryFromSubmittedInputLocked(rt.lastInputText)
	}
	var disabledNote WaitingNotification
	disabledOK := false
	if changeSessionState {
		disabledNote, disabledOK = rt.markInputActivityLocked(submitted, previousInput)
	}
	s := rt.session
	sessionID := rt.session.ID
	reportBrowserActive := submitted && rt.browserOwnsCurrentRoundLocked()
	rt.mu.Unlock()
	if baselineVersion > 0 {
		log.Printf("input snapshot baseline updated session=%s source=%s version=%d len=%d lines=%d", sessionID, baselineSource, baselineVersion, len(baseline.data), countLogLines(baseline.data))
	}
	if disabledOK {
		go rt.updateDisabledNotification(disabledNote)
	}
	if changeSessionState {
		_ = rt.manager.persist(context.Background(), s)
	}
	if reportBrowserActive && rt.manager != nil {
		rt.manager.BrowserActive(sessionID)
	}
	return true
}

func (rt *RuntimeSession) preferBrowserSnapshotSourceLocked(responder chan RuntimeEvent) {
	if responder == nil {
		return
	}
	if sub, ok := rt.subscribers[responder]; ok && !sub.Headless {
		rt.preferredBrowserSnapshotSource = responder
	}
}

func (rt *RuntimeSession) MarkStructuredInputActivity(text string) {
	rt.markStructuredInputActivity(text, nil)
}

func (rt *RuntimeSession) markStructuredInputActivityWithPreviousRoundState(text string, previousRoundUnfinished bool) {
	rt.markStructuredInputActivity(text, &previousRoundUnfinished)
}

func (rt *RuntimeSession) markStructuredInputActivity(text string, previousRoundUnfinishedOverride *bool) {
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		return
	}
	previousInput := rt.lastInputText
	if cleaned := strings.TrimSpace(cleanInputForRecord(text)); cleaned != "" {
		rt.lastInputText = cleaned
	}
	rt.inputLineBuffer = ""
	rt.inputCursor = 0
	rt.inputRecordUnreliable = false
	rt.inputBracketedPaste = false
	rt.updateRecoveryFromSubmittedInputLocked(rt.lastInputText)
	var disabledNote WaitingNotification
	var disabledOK bool
	if previousRoundUnfinishedOverride != nil {
		disabledNote, disabledOK = rt.markInputActivityLockedWithPreviousRoundState(true, previousInput, *previousRoundUnfinishedOverride)
	} else {
		disabledNote, disabledOK = rt.markInputActivityLocked(true, previousInput)
	}
	s := rt.session
	sessionID := rt.session.ID
	reportBrowserActive := rt.browserOwnsCurrentRoundLocked()
	rt.mu.Unlock()
	if disabledOK {
		go rt.updateDisabledNotification(disabledNote)
	}
	_ = rt.manager.persist(context.Background(), s)
	if reportBrowserActive && rt.manager != nil {
		rt.manager.BrowserActive(sessionID)
	}
}

func (rt *RuntimeSession) structuredInputPreviousRoundUnfinished() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return !rt.closed && rt.session.Status == StatusRunning && strings.TrimSpace(rt.lastInputText) != "" && rt.snapshotAtRoundStartSet
}

func (rt *RuntimeSession) browserOwnsCurrentRoundLocked() bool {
	if !rt.snapshotAtRoundStartSet || rt.snapshotAtRoundResponder == nil || !isBrowserSnapshotSource(rt.snapshotAtRoundSource) {
		return false
	}
	sub, ok := rt.subscribers[rt.snapshotAtRoundResponder]
	return ok && !sub.Headless
}

func (rt *RuntimeSession) PrepareInputSnapshotBaseline() bool {
	return rt.PrepareInputSnapshotBaselineFrom(nil)
}

func (rt *RuntimeSession) PrepareInputSnapshotBaselineFrom(responder chan RuntimeEvent) bool {
	return rt.prepareInputSnapshotBaselineFrom(defaultInputBaselineSnapshotDeadline, responder)
}

func (rt *RuntimeSession) prepareInputSnapshotBaseline(deadline time.Duration) bool {
	return rt.prepareInputSnapshotBaselineFrom(deadline, nil)
}

func (rt *RuntimeSession) prepareInputSnapshotBaselineFrom(deadline time.Duration, responder chan RuntimeEvent) bool {
	if deadline <= 0 {
		return false
	}
	expires := time.Now().Add(deadline)
	fresh := rt.requestFreshSnapshotFrom(minDuration(defaultNotifySnapshotTimeout, deadline), SnapshotPurposeInputBaseline, responder)
	if fresh {
		return true
	}
	remaining := time.Until(expires)
	if remaining <= 0 {
		return fresh
	}
	secondTimeout := minDuration(600*time.Millisecond, remaining)
	if secondTimeout > 0 && rt.requestFreshSnapshotFrom(secondTimeout, SnapshotPurposeInputBaseline, responder) {
		fresh = true
	}
	return fresh
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func (rt *RuntimeSession) markInputActivityLocked(submitted bool, previousInput string) (WaitingNotification, bool) {
	previousRoundUnfinished := rt.session.Status == StatusRunning && strings.TrimSpace(previousInput) != "" && rt.snapshotAtRoundStartSet
	return rt.markInputActivityLockedWithPreviousRoundState(submitted, previousInput, previousRoundUnfinished)
}

func (rt *RuntimeSession) markInputActivityLockedWithPreviousRoundState(submitted bool, previousInput string, previousRoundUnfinished bool) (WaitingNotification, bool) {
	var disabledNote WaitingNotification
	disabledOK := false
	rt.controlInputActive = false
	rt.pendingTerminalInteraction = nil
	if submitted {
		previousRoundUnfinished = previousRoundUnfinished && strings.TrimSpace(previousInput) != "" && rt.snapshotAtRoundStartSet
		windowStartInput := strings.TrimSpace(rt.notificationWindowInputText)
		if windowStartInput == "" {
			windowStartInput = strings.TrimSpace(previousInput)
		}
		rt.snapshotRoundGeneration++
		rt.cancelSnapshotRequestsLocked(false)
		overlapRunningCard := rt.notificationRunning && rt.lastNotifiedMessageID != ""
		if overlapRunningCard {
			disabledNote, disabledOK = rt.disabledNotificationLocked(rt.lastNotifiedMessageID)
			rt.freezeNotificationMessageLocked(rt.lastNotifiedMessageID)
		}
		// Every structured Enter prepares a fresh renderer baseline containing
		// the new active composer. If the prior round is still running, retain
		// the oldest unanswered input as the logical notification-window start;
		// otherwise this input starts a new window.
		rt.snapshotAtRoundStart = rt.visibleSnapshot
		rt.snapshotAtRoundSource = rt.visibleSnapshotSource
		rt.snapshotAtRoundResponder = rt.visibleSnapshotResponder
		rt.snapshotAtRoundCols = rt.visibleSnapshotCols
		rt.snapshotAtRoundVersion = rt.visibleSnapshotVersion
		rt.snapshotAtRoundStartSet = true
		if previousRoundUnfinished {
			rt.notificationWindowInputText = windowStartInput
		} else {
			rt.notificationWindowInputText = ""
		}
		rt.roundReply = nil
		rt.capturedInputBaselineResponder = nil
		rt.capturedInputBaselineHeadless = false
		rt.lastNotifiedRoundHash = ""
		rt.lastNotifiedMessageID = ""
		rt.lastNotifiedContent = ""
		rt.notificationUpdateNo = 0
		rt.notificationRunning = false
		rt.hookCompletedCurrentRound = false
		rt.hookCompletionTipClaimed = false
		rt.hookLastAssistantMessage = ""
		rt.suppressRunningMarker = false
	}
	rt.session.Status = StatusRunning
	rt.session.UpdatedAt = time.Now().UTC()
	rt.stateVersion++
	rt.notifyVersion++
	rt.stopNotifyTimerLocked()
	rt.stopNotifyStableTimerLocked()
	rt.stopStartupNotifyTimerLocked()
	// User input always starts an ordinary round, including input that answers
	// an updater, trust prompt, approval, or another startup modal.
	rt.startupNotifyMode = startupNotifyNormal
	return disabledNote, disabledOK
}

func (rt *RuntimeSession) NotifyInputRunning() {
	rt.NotifyInputRunningOnMessage("")
}

func (rt *RuntimeSession) NotifyInputRunningOnMessage(messageID string) {
	source := "input"
	if strings.TrimSpace(messageID) != "" {
		source = "card_shortcut"
	}
	if rt == nil || rt.manager == nil || rt.manager.notifier == nil || !rt.manager.notifier.Available() {
		return
	}
	rt.mu.Lock()
	if !rt.session.Live || !rt.session.NotifyOnWaiting {
		rt.mu.Unlock()
		return
	}
	if rt.requireLarkChat && strings.TrimSpace(rt.session.LarkChatID) == "" {
		rt.mu.Unlock()
		return
	}
	messageID = strings.TrimSpace(messageID)
	if messageID != "" && rt.notificationMessageFrozenLocked(messageID) {
		log.Printf("lark card write skipped source=%s action=running_anchor session=%s message=%s reason=frozen_message", source, rt.session.ID, messageID)
		messageID = ""
	}
	if rt.lastNotifiedMessageID != "" && rt.notificationMessageFrozenLocked(rt.lastNotifiedMessageID) {
		rt.lastNotifiedMessageID = ""
	}
	if messageID != "" {
		rt.lastNotifiedMessageID = messageID
	}
	if rt.lastNotifiedMessageID != "" && rt.notificationRunning {
		rt.mu.Unlock()
		return
	}
	if rt.lastNotifiedMessageID != "" {
		rt.mu.Unlock()
		log.Printf("lark card write skipped source=%s action=running_patch session=%s message=%s reason=existing_card", source, rt.session.ID, rt.lastNotifiedMessageID)
		return
	}
	content := RunningNotificationPlaceholder
	rt.notificationPatchVersion++
	patchVersion := rt.notificationPatchVersion
	n := WaitingNotification{
		SessionID:           rt.session.ID,
		Name:                rt.session.Name,
		Content:             content,
		MessageID:           rt.lastNotifiedMessageID,
		ChatID:              rt.session.LarkChatID,
		MentionOpenID:       rt.notificationMentionOpenID,
		UpdateNo:            rt.notificationUpdateNo,
		Running:             true,
		AutoRefreshEnabled:  rt.autoRefreshEnabled,
		AutoSummaryEnabled:  rt.autoSummaryEnabled,
		MentionModeEnabled:  rt.session.LarkMentionModeEnabled,
		NotificationVersion: patchVersion,
		AgentContext:        cloneTerminalAgentContext(rt.lastTerminalAgentContext),
	}
	rt.notificationRunning = true
	rt.mu.Unlock()
	log.Printf("lark card write queued source=%s action=running_create session=%s message=%s running=%v placeholder=%v update_no=%d content_len=%d", source, n.SessionID, n.MessageID, n.Running, n.Content == RunningNotificationPlaceholder, n.UpdateNo, len(n.Content))

	rt.notificationPatchMu.Lock()
	rt.mu.Lock()
	if rt.notificationPatchVersion != n.NotificationVersion {
		rt.mu.Unlock()
		rt.notificationPatchMu.Unlock()
		log.Printf("running notification send skipped session=%s message=%s reason=stale_patch", n.SessionID, n.MessageID)
		return
	}
	rt.mu.Unlock()
	result, err := rt.notifyWaitingWithRetry(n)
	rt.notificationPatchMu.Unlock()
	if err != nil {
		log.Printf("running notification send failed session=%s message=%s: %v", n.SessionID, n.MessageID, err)
		rt.mu.Lock()
		if rt.notificationPatchVersion == n.NotificationVersion && rt.lastNotifiedMessageID == n.MessageID {
			rt.notificationRunning = false
			if n.MessageID == "" {
				rt.lastNotifiedContent = ""
			}
		}
		rt.mu.Unlock()
		return
	}
	rt.mu.Lock()
	if rt.notificationPatchVersion == n.NotificationVersion {
		if result.MessageID != "" {
			rt.lastNotifiedMessageID = result.MessageID
			rt.bindAutoRefreshMessageLocked(result.MessageID)
		}
		rt.lastNotifiedContent = n.Content
		rt.notificationRunning = true
	}
	rt.mu.Unlock()
	defaultLarkMessageRegistry.rememberLatest(n.SessionID)
}

func (rt *RuntimeSession) RefreshNotificationMessage(messageID string, preserveUpdateNo ...int) error {
	if err := rt.refreshNotificationMessage(messageID, true, false, preserveUpdateNo...); err != nil {
		return err
	}
	rt.scheduleAutoRefreshOnce(messageID)
	return nil
}

func (rt *RuntimeSession) RefreshNotificationControls(messageID string, preserveUpdateNo ...int) error {
	rt.mu.Lock()
	preserveContent := strings.TrimSpace(rt.lastNotifiedContent) != ""
	rt.mu.Unlock()
	return rt.refreshNotificationMessage(messageID, true, preserveContent, preserveUpdateNo...)
}

func (rt *RuntimeSession) RefreshNotificationControlsPreservingContent(messageID string, preserveUpdateNo ...int) error {
	return rt.refreshNotificationMessage(messageID, true, true, preserveUpdateNo...)
}

func (rt *RuntimeSession) AutoRefreshNotificationMessage(messageID string, preserveUpdateNo ...int) error {
	return rt.refreshNotificationMessage(messageID, false, false, preserveUpdateNo...)
}

func (rt *RuntimeSession) refreshNotificationMessage(messageID string, suppressUpdateTip, preserveContent bool, preserveUpdateNo ...int) error {
	if rt == nil || rt.manager == nil || rt.manager.notifier == nil || !rt.manager.notifier.Available() {
		return errors.New("lark notifier is not configured")
	}
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		rt.mu.Lock()
		messageID = rt.lastNotifiedMessageID
		rt.mu.Unlock()
	}
	if messageID == "" {
		return errors.New("notification message is not available")
	}
	rt.mu.Lock()
	if rt.notificationMessageFrozenLocked(messageID) {
		rt.mu.Unlock()
		return errors.New("notification message is frozen")
	}
	rt.mu.Unlock()
	content := ""
	fresh := true
	if preserveContent {
		rt.mu.Lock()
		content = rt.lastNotifiedContent
		rt.mu.Unlock()
		if strings.TrimSpace(content) == "" {
			return errors.New("notification content is not available")
		}
	} else {
		content, fresh = rt.currentRoundContentWithFreshSnapshot(800 * time.Millisecond)
	}
	rt.mu.Lock()
	runningAtRefresh := rt.session.Status == StatusRunning
	rt.mu.Unlock()
	if !preserveContent && (suppressUpdateTip || runningAtRefresh) {
		rt.mu.Lock()
		manualContent := pickManualRefreshNotifyContentWithWindowAnchorPolicy(rt.visibleSnapshot, rt.previousNotifySnapshotLocked(), rt.roundReply, rt.lastInputText, rt.notificationWindowInputText, rt.notifyTextAnchorPolicyLocked())
		manualContent = rt.cleanLarkNotifyContentForAgentLocked(manualContent)
		rt.mu.Unlock()
		if strings.TrimSpace(manualContent) != "" {
			content = manualContent
		}
	}
	content = strings.TrimSpace(content)
	rt.mu.Lock()
	stale := rt.visibleSnapshotStaleForCurrentRoundLocked()
	hasVisibleSnapshot := strings.TrimSpace(rt.visibleSnapshot) != ""
	rt.mu.Unlock()
	if !preserveContent && !fresh && stale {
		if !hasVisibleSnapshot {
			return errors.New("current visible snapshot is stale and empty")
		}
		// A renderer may fail to answer after sleep/reconnect even though the
		// runtime still has a useful cached screen. Manual refresh must never
		// turn that into a blank card: force the bounded visible-tail fallback.
		content = ""
	}
	hasSnapshotContent := !preserveContent && content != ""
	hasContent := content != ""
	usedTailFallback := false
	if !hasContent {
		rt.mu.Lock()
		fallbackContent := pickLarkNotifyFallbackTailContent(rt.visibleSnapshot)
		if suppressUpdateTip {
			fallbackContent = pickLarkManualRefreshFallbackTailContent(rt.visibleSnapshot)
		}
		fallbackContent = rt.cleanLarkNotifyContentForAgentLocked(fallbackContent)
		fallbackContent = strings.TrimSpace(fallbackContent)
		rt.mu.Unlock()
		if fallbackContent != "" {
			content = fallbackContent
			hasContent = true
			hasSnapshotContent = true
			usedTailFallback = true
		}
	}
	if !hasContent {
		content = EmptyNotificationPlaceholder
	}
	rt.mu.Lock()
	lastInputText := rt.lastInputText
	if !rt.session.Live || !rt.session.NotifyOnWaiting {
		rt.mu.Unlock()
		return errors.New("notification is not enabled")
	}
	if rt.notificationMessageFrozenLocked(messageID) {
		rt.mu.Unlock()
		return errors.New("notification message is frozen")
	}
	if hasSnapshotContent && !usedTailFallback && strings.TrimSpace(lastInputText) != "" && !hasReplyLine(content, lastInputText) &&
		!((suppressUpdateTip || runningAtRefresh) && containsTransientStatusLine(content)) {
		rt.mu.Unlock()
		return errors.New("current round has no reply content")
	}
	running := rt.session.Status == StatusRunning
	updateNo := rt.notificationUpdateNo
	if len(preserveUpdateNo) > 0 && preserveUpdateNo[0] > 0 {
		updateNo = preserveUpdateNo[0]
	}
	content = rt.stableNotifyContentForMessageLocked(messageID, content)
	contentHash := notifyContentHash(content)
	rt.notificationPatchVersion++
	patchVersion := rt.notificationPatchVersion
	n := WaitingNotification{
		SessionID:           rt.session.ID,
		Name:                rt.session.Name,
		Content:             content,
		MessageID:           messageID,
		ChatID:              rt.session.LarkChatID,
		MentionOpenID:       rt.notificationMentionOpenID,
		UpdateNo:            updateNo,
		Running:             running,
		AutoRefreshEnabled:  rt.autoRefreshEnabled,
		AutoSummaryEnabled:  rt.autoSummaryEnabled,
		MentionModeEnabled:  rt.session.LarkMentionModeEnabled,
		SuppressUpdateTip:   suppressUpdateTip,
		NotificationVersion: patchVersion,
	}
	if !running {
		n.Interaction = rt.notificationInteractionLocked(messageID)
	}
	n.AgentContext = rt.notificationAgentContextLocked()
	rt.notificationRunning = n.Running
	rt.mu.Unlock()
	source := "auto_refresh"
	if preserveContent {
		source = "controls_refresh"
	} else if suppressUpdateTip {
		source = "manual_refresh"
	}
	log.Printf("lark card write queued source=%s action=patch session=%s message=%s running=%v placeholder=%v update_no=%d content_len=%d", source, n.SessionID, n.MessageID, n.Running, n.Content == RunningNotificationPlaceholder, n.UpdateNo, len(n.Content))

	rt.notificationPatchMu.Lock()
	rt.mu.Lock()
	if rt.notificationMessageFrozenLocked(messageID) {
		rt.mu.Unlock()
		rt.notificationPatchMu.Unlock()
		return errors.New("notification message is frozen")
	}
	rt.mu.Unlock()
	result, err := rt.notifyWaitingWithRetry(n)
	rt.notificationPatchMu.Unlock()
	if err != nil {
		return err
	}
	rt.mu.Lock()
	if rt.notificationPatchVersion == n.NotificationVersion {
		if result.MessageID != "" {
			rt.lastNotifiedMessageID = result.MessageID
			rt.bindAutoRefreshMessageLocked(result.MessageID)
		} else {
			rt.lastNotifiedMessageID = messageID
			rt.bindAutoRefreshMessageLocked(messageID)
		}
		rt.lastNotifiedContent = content
		rt.lastNotifiedRoundHash = contentHash
		if hasSnapshotContent {
			rt.lastNotifiedVisibleSnapshot = rt.visibleSnapshot
			rt.lastNotifiedVisibleSnapshotSource = rt.visibleSnapshotSource
			rt.lastNotifiedVisibleResponder = rt.visibleSnapshotResponder
			rt.lastNotifiedVisibleCols = rt.visibleSnapshotCols
		}
		if result.Updated {
			rt.notificationUpdateNo = n.UpdateNo
		}
		rt.notificationRunning = n.Running
		boundMessageID := messageID
		if result.MessageID != "" {
			boundMessageID = result.MessageID
		}
		rt.bindTerminalInteractionMessageLocked(n.Interaction, boundMessageID)
	}
	rt.mu.Unlock()
	defaultLarkMessageRegistry.remember(rt.session.ID, messageID)
	defaultLarkMessageRegistry.rememberLatest(rt.session.ID)
	return nil
}

func (rt *RuntimeSession) bindAutoRefreshMessageLocked(messageID string) {
	messageID = strings.TrimSpace(messageID)
	if rt.autoRefreshEnabled && messageID != "" {
		rt.autoRefreshMessageID = messageID
	}
}

func (rt *RuntimeSession) ToggleAutoRefresh(messageID string) (bool, error) {
	if rt == nil {
		return false, errors.New("session is not available")
	}
	messageID = strings.TrimSpace(messageID)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !rt.session.Live || !rt.session.NotifyOnWaiting {
		return false, errors.New("notification is not enabled")
	}
	if messageID == "" {
		messageID = rt.lastNotifiedMessageID
	}
	if messageID == "" {
		return false, errors.New("notification message is not available")
	}
	if rt.notificationMessageFrozenLocked(messageID) {
		return false, errors.New("notification message is frozen")
	}
	if rt.autoRefreshEnabled {
		rt.stopAutoRefreshLocked()
		return false, nil
	}
	rt.autoRefreshEnabled = true
	rt.autoRefreshMessageID = messageID
	stop := make(chan struct{})
	rt.autoRefreshStop = stop
	go rt.autoRefreshLoop(stop)
	return true, nil
}

func (rt *RuntimeSession) ToggleAutoSummary() (bool, error) {
	if rt == nil {
		return false, errors.New("session is not available")
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !rt.session.Live || rt.session.Status == StatusExited || rt.session.Status == StatusFailed {
		return false, errors.New("session is not active")
	}
	rt.autoSummaryEnabled = !rt.autoSummaryEnabled
	return rt.autoSummaryEnabled, nil
}

func (rt *RuntimeSession) AutoSummaryEnabled() bool {
	if rt == nil {
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.autoSummaryEnabled
}

func (rt *RuntimeSession) stopAutoRefreshLocked() {
	if rt.autoRefreshStop != nil {
		close(rt.autoRefreshStop)
		rt.autoRefreshStop = nil
	}
	rt.autoRefreshEnabled = false
	rt.autoRefreshMessageID = ""
}

func (rt *RuntimeSession) autoRefreshLoop(stop <-chan struct{}) {
	timer := time.NewTimer(rt.manager.autoRefreshDelay())
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-timer.C:
		}
		rt.mu.Lock()
		if !rt.autoRefreshEnabled || rt.autoRefreshStop != stop {
			rt.mu.Unlock()
			return
		}
		if !rt.session.Live || rt.session.Status == StatusExited || rt.session.Status == StatusFailed {
			rt.stopAutoRefreshLocked()
			rt.mu.Unlock()
			return
		}
		messageID := rt.autoRefreshMessageID
		updateNo := rt.notificationUpdateNo
		running := rt.session.Status == StatusRunning
		sessionID := rt.session.ID
		rt.mu.Unlock()
		if running && messageID != "" {
			if err := rt.AutoRefreshNotificationMessage(messageID, updateNo); err != nil {
				log.Printf("lark card auto refresh failed session=%s message=%s: %v", sessionID, messageID, err)
			}
		}
		timer.Reset(rt.manager.autoRefreshDelay())
	}
}

func (rt *RuntimeSession) scheduleAutoRefreshOnce(messageID string) {
	if rt == nil || rt.manager == nil {
		return
	}
	messageID = strings.TrimSpace(messageID)
	rt.mu.Lock()
	if !rt.autoRefreshEnabled || rt.autoRefreshStop == nil {
		rt.mu.Unlock()
		return
	}
	if messageID == "" {
		messageID = rt.autoRefreshMessageID
	}
	if messageID == "" {
		rt.mu.Unlock()
		return
	}
	delay := rt.manager.autoRefreshDelay()
	sessionID := rt.session.ID
	rt.mu.Unlock()
	time.AfterFunc(delay, func() {
		rt.mu.Lock()
		if !rt.autoRefreshEnabled || rt.autoRefreshMessageID != messageID || !rt.session.Live || rt.session.Status == StatusExited || rt.session.Status == StatusFailed {
			rt.mu.Unlock()
			return
		}
		updateNo := rt.notificationUpdateNo
		rt.mu.Unlock()
		if err := rt.AutoRefreshNotificationMessage(messageID, updateNo); err != nil {
			log.Printf("lark card auto refresh after manual refresh failed session=%s message=%s: %v", sessionID, messageID, err)
		}
	})
}

func (rt *RuntimeSession) recordInputLocked(data string) bool {
	runes := []rune(data)
	submitted := false
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == 0x1b {
			end := skipInputEscape(runes, i)
			sequence := string(runes[i : end+1])
			rt.applyInputEscapeLocked(sequence)
			i = end
			continue
		}
		if rt.inputBracketedPaste {
			rt.insertInputRuneLocked(r)
			continue
		}
		switch r {
		case '\r', '\n':
			submitted = true
			if text := strings.TrimSpace(rt.inputLineBuffer); text != "" && !rt.inputRecordUnreliable {
				rt.lastInputText = text
			} else if rt.inputRecordUnreliable {
				rt.lastInputText = ""
			}
			rt.inputLineBuffer = ""
			rt.inputCursor = 0
			rt.inputRecordUnreliable = false
		case '\b', 0x7f:
			rt.deleteInputRuneBeforeCursorLocked()
		case 0x01: // Ctrl-A / Home
			rt.inputCursor = 0
		case 0x05: // Ctrl-E / End
			rt.inputCursor = len([]rune(rt.inputLineBuffer))
		case 0x03: // Ctrl-C cancels the current editor line.
			rt.inputLineBuffer = ""
			rt.inputCursor = 0
			rt.inputRecordUnreliable = false
			rt.lastInputText = ""
		case 0x04: // Ctrl-D / Delete
			rt.deleteInputRuneAtCursorLocked()
		case 0x0b: // Ctrl-K
			rs := []rune(rt.inputLineBuffer)
			if rt.inputCursor < len(rs) {
				rt.inputLineBuffer = string(rs[:rt.inputCursor])
			}
		case 0x15: // Ctrl-U
			rs := []rune(rt.inputLineBuffer)
			if rt.inputCursor > len(rs) {
				rt.inputCursor = len(rs)
			}
			rt.inputLineBuffer = string(rs[rt.inputCursor:])
			rt.inputCursor = 0
		case 0x17: // Ctrl-W
			rt.deleteInputWordBeforeCursorLocked()
		case '\t', 0x12: // Completion/history search changes text outside our view.
			rt.inputRecordUnreliable = true
		default:
			if r >= 0x20 {
				rt.insertInputRuneLocked(r)
			}
		}
	}
	if !submitted {
		if text := strings.TrimSpace(rt.inputLineBuffer); text != "" && !rt.inputRecordUnreliable {
			rt.lastInputText = text
		}
	}
	return submitted
}

func (rt *RuntimeSession) applyInputEscapeLocked(sequence string) {
	switch sequence {
	case "\x1b[200~":
		rt.inputBracketedPaste = true
	case "\x1b[201~":
		rt.inputBracketedPaste = false
	case "\x1b[D", "\x1bOD":
		if rt.inputCursor > 0 {
			rt.inputCursor--
		}
	case "\x1b[C", "\x1bOC":
		if rt.inputCursor < len([]rune(rt.inputLineBuffer)) {
			rt.inputCursor++
		}
	case "\x1b[H", "\x1b[1~", "\x1b[7~", "\x1bOH":
		rt.inputCursor = 0
	case "\x1b[F", "\x1b[4~", "\x1b[8~", "\x1bOF":
		rt.inputCursor = len([]rune(rt.inputLineBuffer))
	case "\x1b[3~":
		rt.deleteInputRuneAtCursorLocked()
	case "\x1b[A", "\x1b[B", "\x1bOA", "\x1bOB":
		// History navigation replaces the line with text the backend does not
		// know. Clear the anchor rather than guessing and selecting old output.
		rt.inputRecordUnreliable = true
	default:
		rt.inputRecordUnreliable = true
	}
}

func (rt *RuntimeSession) insertInputRuneLocked(r rune) {
	rs := []rune(rt.inputLineBuffer)
	if rt.inputCursor < 0 || rt.inputCursor > len(rs) {
		rt.inputCursor = len(rs)
	}
	rs = append(rs, 0)
	copy(rs[rt.inputCursor+1:], rs[rt.inputCursor:])
	rs[rt.inputCursor] = r
	rt.inputCursor++
	rt.inputLineBuffer = string(rs)
}

func (rt *RuntimeSession) deleteInputRuneBeforeCursorLocked() {
	rs := []rune(rt.inputLineBuffer)
	if rt.inputCursor <= 0 || len(rs) == 0 {
		return
	}
	if rt.inputCursor > len(rs) {
		rt.inputCursor = len(rs)
	}
	rs = append(rs[:rt.inputCursor-1], rs[rt.inputCursor:]...)
	rt.inputCursor--
	rt.inputLineBuffer = string(rs)
}

func (rt *RuntimeSession) deleteInputRuneAtCursorLocked() {
	rs := []rune(rt.inputLineBuffer)
	if rt.inputCursor < 0 || rt.inputCursor >= len(rs) {
		return
	}
	rs = append(rs[:rt.inputCursor], rs[rt.inputCursor+1:]...)
	rt.inputLineBuffer = string(rs)
}

func (rt *RuntimeSession) deleteInputWordBeforeCursorLocked() {
	rs := []rune(rt.inputLineBuffer)
	if rt.inputCursor > len(rs) {
		rt.inputCursor = len(rs)
	}
	start := rt.inputCursor
	for start > 0 && unicode.IsSpace(rs[start-1]) {
		start--
	}
	for start > 0 && !unicode.IsSpace(rs[start-1]) {
		start--
	}
	rs = append(rs[:start], rs[rt.inputCursor:]...)
	rt.inputCursor = start
	rt.inputLineBuffer = string(rs)
}

func inputChangesSessionState(data string) bool {
	for _, r := range cleanInputForRecord(data) {
		switch r {
		case '\r', '\n':
			return true
		case '\b', 0x7f:
			continue
		default:
			if r >= 0x20 && r != 0x1b && !unicode.IsSpace(r) {
				return true
			}
		}
	}
	return false
}

func cleanInputForRecord(data string) string {
	runes := []rune(data)
	var b strings.Builder
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == 0x1b {
			i = skipInputEscape(runes, i)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func skipInputEscape(runes []rune, i int) int {
	if i+1 >= len(runes) {
		return i
	}
	switch runes[i+1] {
	case '[':
		j := i + 2
		for j < len(runes) {
			r := runes[j]
			if r >= 0x40 && r <= 0x7e {
				return j
			}
			j++
		}
		return len(runes) - 1
	case 'O':
		if i+2 < len(runes) {
			return i + 2
		}
		return i + 1
	default:
		return i + 1
	}
}

func (rt *RuntimeSession) HandleOutput(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	cp := append([]byte(nil), chunk...)
	renderable := HasRenderableContent(cp)
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		return
	}
	rt.output = append(rt.output, cp...)
	if len(rt.output) > maxOutputBytes {
		rt.output = rt.output[len(rt.output)-maxOutputBytes:]
	}
	controlOutput := rt.controlInputActive
	restartOutput := rt.agentRestartPending
	if !controlOutput {
		rt.roundReply = append(rt.roundReply, cp...)
		if len(rt.roundReply) > maxRoundBytes {
			rt.roundReply = rt.roundReply[len(rt.roundReply)-maxRoundBytes:]
		}
	}
	rt.session.HistorySize += int64(len(cp))
	rt.session.UpdatedAt = time.Now().UTC()
	var runningNote WaitingNotification
	markRunning := false
	// An Agent may repaint its TUI after its completion callback has already
	// supplied the authoritative final response. Keep recording and broadcasting
	// that tail repaint, but do not reopen the completed round or re-arm the idle
	// completion fallback. A submitted input clears hookCompletedCurrentRound.
	completedHookRound := rt.agentTurnHookVerified && rt.hookCompletedCurrentRound && rt.session.Status == StatusWaiting
	if renderable && !completedHookRound && !controlOutput && !restartOutput {
		previousStatus := rt.session.Status
		rt.session.Status = StatusRunning
		rt.stateVersion++
		rt.notifyVersion++
		if previousStatus != StatusRunning {
			log.Printf("session status transition source=terminal_output session=%s from=%s to=%s notify_version=%d card_message=%s notification_running=%v suppress_running_marker=%v",
				rt.session.ID, previousStatus, rt.session.Status, rt.notifyVersion, rt.lastNotifiedMessageID, rt.notificationRunning, rt.suppressRunningMarker)
		}
		if previousStatus == StatusWaiting {
			runningNote, markRunning = rt.markNotificationRunningLocked()
		}
		if rt.startupNotifyMode == startupNotifySettling {
			rt.scheduleStartupNotifyFinalLocked(defaultStartupPresetSettleDelay)
		}
		if rt.agentTurnHookVerified {
			rt.resetAgentIdleCompletionTimerLocked()
		} else {
			rt.resetNotifyStableTimerLocked()
		}
	}
	for ch := range rt.subscribers {
		select {
		case ch <- RuntimeEvent{Type: RuntimeEventOutput, Data: cp}:
		default:
		}
	}
	seq := rt.nextSeq
	rt.nextSeq++
	s := rt.session
	rt.mu.Unlock()
	if rt.manager.store != nil {
		_ = rt.manager.store.AppendOutput(context.Background(), s.ID, seq, cp)
		_ = rt.manager.store.UpdateSession(context.Background(), s)
	}
	if markRunning {
		go rt.updateNotificationRunning(runningNote, true)
	}
}

// CompleteAgentTurn marks the current Agent round as complete after a local
// completion callback. The recovery key is a per-session bearer credential
// injected only into that session's shell environment.
func (m *Manager) CompleteAgentTurn(ctx context.Context, sessionID, token, agentSessionID, lastAssistantMessage string) (Session, bool, error) {
	rt, ok := m.GetRuntime(strings.TrimSpace(sessionID))
	if !ok {
		return Session{}, false, nil
	}
	return rt.completeAgentTurn(ctx, token, agentSessionID, lastAssistantMessage)
}

func (rt *RuntimeSession) completeAgentTurn(ctx context.Context, token, agentSessionID, lastAssistantMessage string) (Session, bool, error) {
	if rt == nil || rt.manager == nil {
		return Session{}, false, nil
	}
	rt.mu.Lock()
	if strings.TrimSpace(token) == "" || strings.TrimSpace(token) != strings.TrimSpace(rt.session.RecoveryKey) {
		rt.mu.Unlock()
		return Session{}, false, errors.New("invalid agent hook token")
	}
	if !rt.session.Live || rt.session.Status == StatusExited || rt.session.Status == StatusFailed {
		s := rt.session
		rt.mu.Unlock()
		return s, false, nil
	}
	agentKind := strings.TrimSpace(rt.session.LastAgentKind)
	if strings.TrimSpace(rt.session.LastMode) != SessionModeAgent || (agentKind != "codex" && agentKind != "claude" && agentKind != "aiden") {
		s := rt.session
		rt.mu.Unlock()
		return s, false, nil
	}
	pinnedRecovery := false
	switch agentKind {
	case "codex":
		if command, ok := pinCodexResumeCommand(rt.session.LastAgentResumeCommand, strings.TrimSpace(agentSessionID)); ok {
			rt.session.LastAgentResumeCommand = command
			rt.session.LastAgentHome = defaultCodexHome()
			pinnedRecovery = true
		}
	case "claude":
		if command, ok := pinClaudeResumeCommand(rt.session.LastAgentResumeCommand, strings.TrimSpace(agentSessionID)); ok {
			rt.session.LastAgentResumeCommand = command
			pinnedRecovery = true
		}
	}
	lastAssistantMessage = strings.TrimSpace(lastAssistantMessage)
	newAssistantMessage := lastAssistantMessage != "" && lastAssistantMessage != rt.hookLastAssistantMessage
	if newAssistantMessage {
		rt.hookLastAssistantMessage = lastAssistantMessage
	}
	if rt.session.Status == StatusWaiting {
		if !newAssistantMessage {
			s := rt.session
			rt.mu.Unlock()
			if pinnedRecovery {
				_ = rt.manager.persist(ctx, s)
			}
			return s, false, nil
		}
		rt.agentTurnHookVerified = true
		rt.stopNotifyTimerLocked()
		rt.stopNotifyStableTimerLocked()
		if !rt.hookCompletedCurrentRound {
			rt.hookCompletedCurrentRound = true
			rt.hookCompletionTipClaimed = rt.lastNotifiedMessageID != "" && !rt.notificationRunning
		}
		rt.notifyVersion++
		version := rt.notifyVersion
		s := rt.session
		rt.mu.Unlock()
		_ = rt.manager.persist(ctx, s)
		go rt.notifyIfStillWaitingImmediately(version)
		return s, true, nil
	}
	rt.agentTurnHookVerified = true
	if !rt.hookCompletedCurrentRound {
		rt.hookCompletedCurrentRound = true
		rt.hookCompletionTipClaimed = false
	}
	rt.stopNotifyTimerLocked()
	rt.stopNotifyStableTimerLocked()
	rt.session.Status = StatusWaiting
	rt.session.UpdatedAt = time.Now().UTC()
	rt.stateVersion++
	rt.notifyVersion++
	version := rt.notifyVersion
	s := rt.session
	rt.mu.Unlock()
	_ = rt.manager.persist(ctx, s)
	go rt.notifyIfStillWaitingImmediately(version)
	return s, true, nil
}

func (rt *RuntimeSession) Close() {
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		rt.signalSessionEnded()
		return
	}
	rt.closed = true
	rt.session.Live = false
	rt.snapshotRoundGeneration++
	rt.stateVersion++
	rt.notifyVersion++
	rt.stopAutoRefreshLocked()
	rt.stopNotifyTimerLocked()
	rt.stopNotifyStableTimerLocked()
	rt.stopStartupNotifyTimerLocked()
	rt.cancelPendingSnapshotRequestsLocked()
	for ch := range rt.subscribers {
		close(ch)
		delete(rt.subscribers, ch)
	}
	rt.mu.Unlock()
	rt.closeTerminal()
	rt.signalSessionEnded()
}

func (rt *RuntimeSession) closeTerminal() {
	rt.terminalCloseOnce.Do(func() {
		if rt.terminal != nil {
			_ = rt.terminal.Close()
		}
	})
}

func (rt *RuntimeSession) signalSessionEnded() {
	if rt == nil || rt.manager == nil {
		return
	}
	rt.sessionEndedOnce.Do(func() {
		rt.manager.sessionEnded(rt.session.ID)
	})
}

func (rt *RuntimeSession) streamOutput() {
	buf := make([]byte, 8192)
	for {
		n, err := rt.terminal.Read(buf)
		if n > 0 {
			rt.HandleOutput(buf[:n])
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				rt.HandleOutput([]byte("\r\n[terminal closed]\r\n"))
			}
			return
		}
	}
}

func (rt *RuntimeSession) waitForExit() {
	err := rt.process.Wait()
	code := 0
	status := StatusExited
	if err != nil {
		status = StatusFailed
		code = 1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
	}
	rt.markTerminal(status, code)
}

func (rt *RuntimeSession) markTerminal(status string, code int) {
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		rt.closeTerminal()
		rt.signalSessionEnded()
		return
	}
	rt.closed = true
	rt.snapshotRoundGeneration++
	rt.stopAutoRefreshLocked()
	rt.stopNotifyTimerLocked()
	rt.stopNotifyStableTimerLocked()
	rt.stopStartupNotifyTimerLocked()
	rt.session.Status = status
	rt.session.Live = false
	rt.session.ExitCode = &code
	rt.session.UpdatedAt = time.Now().UTC()
	s := rt.session
	rt.cancelPendingSnapshotRequestsLocked()
	for ch := range rt.subscribers {
		close(ch)
		delete(rt.subscribers, ch)
	}
	rt.mu.Unlock()
	rt.manager.mu.Lock()
	delete(rt.manager.sessions, s.ID)
	rt.manager.mu.Unlock()
	rt.closeTerminal()
	if rt.manager.store != nil {
		_ = rt.manager.store.UpdateSession(context.Background(), s)
	}
	rt.signalSessionEnded()
}

func (rt *RuntimeSession) cancelPendingSnapshotRequestsLocked() {
	rt.cancelSnapshotRequestsLocked(true)
}

func (rt *RuntimeSession) cancelSnapshotRequestsLocked(clearPreferred bool) {
	for requestID, request := range rt.snapshotRequests {
		delete(rt.snapshotRequests, requestID)
		if request != nil {
			close(request.waiter)
		}
	}
	rt.pendingHeadlessSnapshots = 0
	if clearPreferred {
		rt.preferredBrowserSnapshotSource = nil
		rt.preferredHeadlessSnapshotSource = nil
	}
}

func (rt *RuntimeSession) notifyStableDelayLocked() time.Duration {
	if notifyContentNeedsConservativeDelayWithWindowAnchorPolicy(rt.visibleSnapshot, rt.previousNotifySnapshotLocked(), rt.roundReply, rt.lastInputText, rt.notificationWindowInputText, rt.notifyTextAnchorPolicyLocked()) {
		return rt.manager.conservativeWaiting
	}
	return rt.manager.fastWaiting
}

func (rt *RuntimeSession) resetNotifyStableTimerLocked() {
	rt.stopNotifyStableTimerLocked()
	if !rt.session.Live {
		return
	}
	version := rt.stateVersion
	delay := rt.notifyStableDelayLocked()
	rt.notifyStableTimer = time.AfterFunc(delay, func() {
		rt.notifyAfterStable(version)
	})
}

func (rt *RuntimeSession) resetAgentIdleCompletionTimerLocked() {
	rt.stopNotifyStableTimerLocked()
	if !rt.session.Live || rt.session.Status != StatusRunning {
		return
	}
	version := rt.stateVersion
	rt.notifyStableTimer = time.AfterFunc(agentIdleCompletionFallback, func() {
		rt.notifyAfterStable(version)
	})
}

func (rt *RuntimeSession) hasPendingCodexInteractionLocked() bool {
	body, _ := selectNotifyBodyWithWindowAnchorPolicy(
		rt.visibleSnapshot,
		rt.previousNotifySnapshotLocked(),
		rt.roundReply,
		rt.lastInputText,
		rt.notificationWindowInputText,
		rt.notifyTextAnchorPolicyLocked(),
	)
	return DetectCodexTerminalInteraction(body, rt.session.ID, rt.lastInputText, rt.notifyVersion, rt.visibleSnapshotVersion) != nil
}

func (rt *RuntimeSession) notifyAfterStable(version int64) {
	rt.mu.Lock()
	if !rt.session.Live || rt.stateVersion != version || rt.session.Status == StatusExited || rt.session.Status == StatusFailed {
		rt.mu.Unlock()
		return
	}
	if rt.session.Status == StatusRunning {
		rt.session.Status = StatusWaiting
		rt.session.UpdatedAt = time.Now().UTC()
		rt.notifyVersion++
		s := rt.session
		notifyVersion := rt.notifyVersion
		rt.mu.Unlock()
		_ = rt.manager.persist(context.Background(), s)
		rt.notifyIfStillWaiting(notifyVersion)
		return
	}
	notifyVersion := rt.notifyVersion
	rt.mu.Unlock()
	rt.notifyIfStillWaiting(notifyVersion)
}

func (rt *RuntimeSession) notifyIfStillWaiting(version int64) {
	rt.notifyIfStillWaitingWithMode(version, false, true)
}

func (rt *RuntimeSession) notifyIfStillWaitingImmediately(version int64) {
	rt.notifyIfStillWaitingWithMode(version, true, true)
}

func (rt *RuntimeSession) notifyIfStillWaitingForInteraction(version int64) {
	rt.notifyIfStillWaitingWithMode(version, true, false)
}

func (rt *RuntimeSession) notifyIfStillWaitingWithMode(version int64, immediate, requestFreshSnapshot bool) {
	if !immediate {
		time.Sleep(100 * time.Millisecond)
	}
	rt.mu.Lock()
	if rt.session.Status != StatusWaiting || !rt.session.Live || rt.notifyVersion != version {
		rt.mu.Unlock()
		return
	}
	startupFallback := false
	if rt.startupNotifyMode == startupNotifyDiscard {
		rt.mu.Unlock()
		rt.RequestFreshSnapshot(defaultNotifySnapshotTimeout)
		rt.mu.Lock()
		if rt.session.Status != StatusWaiting || !rt.session.Live || rt.notifyVersion != version || rt.startupNotifyMode != startupNotifyDiscard {
			rt.mu.Unlock()
			return
		}
		agentKind := agentKindForCommand(rt.session.LastAgentStartCommand, rt.session.LastAgentKind)
		if !startupAgentComposerReady(rt.visibleSnapshot, rt.visibleSnapshotSource, agentKind) {
			rt.stopNotifyTimerLocked()
			rt.startupNotifyMode = startupNotifyNormal
			startupFallback = true
			requestFreshSnapshot = false
			sessionID := rt.session.ID
			source := rt.visibleSnapshotSource
			log.Printf("startup fallback entering ordinary notification flow session=%s version=%d snapshot_source=%s", sessionID, version, source)
		} else {
			rt.stopNotifyTimerLocked()
			rt.startupNotifyMode = startupNotifyNormal
			sessionID := rt.session.ID
			rt.mu.Unlock()
			log.Printf("startup terminal output discarded session=%s version=%d", sessionID, version)
			rt.manager.notificationSent(sessionID)
			return
		}
	}
	if !rt.session.NotifyOnWaiting || rt.manager.notifier == nil || !rt.manager.notifier.Available() {
		rt.mu.Unlock()
		return
	}
	hasHookAssistantMessage := rt.hookAssistantNotifyContentLocked() != ""
	rt.mu.Unlock()
	if requestFreshSnapshot && !hasHookAssistantMessage {
		rt.RequestFreshSnapshot(defaultNotifySnapshotTimeout)
	}
	rt.notificationPatchMu.Lock()
	defer rt.notificationPatchMu.Unlock()
	rt.mu.Lock()
	if rt.session.Status != StatusWaiting || !rt.session.Live || !rt.session.NotifyOnWaiting || rt.notifyVersion != version {
		rt.mu.Unlock()
		return
	}
	var n WaitingNotification
	var contentHash string
	var ok bool
	if startupFallback {
		n, contentHash, ok = rt.startupFallbackWaitingNotificationCandidateLocked()
	} else {
		n, contentHash, ok = rt.waitingNotificationLocked()
	}
	if !ok {
		reason := "empty_content"
		if !startupFallback {
			_, _, _, reason = rt.waitingNotificationCandidateLocked()
		}
		if reason == "needs_more_snapshot" {
			n, contentHash, ok, reason = rt.fallbackWaitingNotificationCandidateLocked()
			if ok {
				log.Printf("waiting notification fallback ready session=%s version=%d hash=%s snapshot_source=%s content_len=%d content_lines=%d preview=%q",
					n.SessionID, version, shortNotifyHash(contentHash), n.SnapshotSource, len(n.Content), countLogLines(n.Content), previewLogText(n.Content, 160))
			}
		}
		if !ok && (reason == "empty_content" || reason == "needs_more_snapshot") {
			n, contentHash, ok = rt.fallbackTailWaitingNotificationCandidateLocked()
			if ok {
				policy := rt.notifyTextAnchorPolicyLocked()
				currentAnchors := len(inputAnchorSpans(splitVisibleLines(rt.visibleSnapshot), rt.lastInputText))
				previousAnchors := len(inputAnchorSpans(splitVisibleLines(rt.previousNotifySnapshotLocked()), rt.lastInputText))
				log.Printf("waiting notification tail fallback ready session=%s version=%d lines=%d hash=%s snapshot_source=%s input_anchors=%d/%d anchor_allowed=%v identity=%v guard=%d/%d cursor=%d/%d window_start=%q",
					n.SessionID, version, countLogLines(n.Content), shortNotifyHash(contentHash), n.SnapshotSource,
					currentAnchors, previousAnchors, policy.allowed, policy.enforceIdentity, policy.previousGuardLine,
					policy.currentGuardLine, policy.previousCursorLine, policy.currentCursorLine, rt.notificationWindowInputText)
			}
		}
		if !ok && reason == "empty_content" {
			n, contentHash, ok = rt.emptyWaitingNotificationCandidateLocked()
			if ok {
				log.Printf("waiting notification empty completion ready session=%s version=%d message=%s hash=%s",
					n.SessionID, version, rt.lastNotifiedMessageID, shortNotifyHash(contentHash))
			}
		}
		if !ok {
			log.Printf("waiting notification not ready session=%s version=%d reason=%s status=%s live=%v notify_version=%d last_input=%q visible_len=%d round_len=%d",
				rt.session.ID, version, reason, rt.session.Status, rt.session.Live, rt.notifyVersion, rt.lastInputText, len(rt.visibleSnapshot), len(rt.roundReply))
			var waitingNote WaitingNotification
			clearRunning := false
			if reason == "duplicate_hash" && rt.notificationRunning {
				waitingNote, clearRunning = rt.markNotificationWaitingLocked()
			}
			if reason != "duplicate_hash" {
				rt.rescheduleNotifyRetryLocked(version)
			}
			rt.mu.Unlock()
			if clearRunning {
				rt.updateNotificationRunning(waitingNote, false)
			}
			return
		}
	}
	if rt.startupNotifyMode == startupNotifySuppress || rt.startupNotifyMode == startupNotifySettling {
		mode := rt.startupNotifyMode
		rt.mu.Unlock()
		log.Printf("waiting notification suppressed during startup presets session=%s version=%d mode=%d hash=%s", n.SessionID, version, mode, shortNotifyHash(contentHash))
		return
	}
	if rt.startupNotifyMode == startupNotifyFinal {
		rt.startupNotifyMode = startupNotifyNormal
	}
	if rt.lastNotifiedMessageID != "" {
		n.MessageID = rt.lastNotifiedMessageID
		n.UpdateNo = rt.notificationUpdateNo + 1
		n.Running = false
	}
	n.AutoRefreshEnabled = rt.autoRefreshEnabled
	n.AutoSummaryEnabled = rt.autoSummaryEnabled
	rt.notificationPatchVersion++
	n.NotificationVersion = rt.notificationPatchVersion
	notifiedVisibleSnapshot := rt.visibleSnapshot
	notifiedVisibleSnapshotSource := rt.visibleSnapshotSource
	notifiedVisibleResponder := rt.visibleSnapshotResponder
	notifiedVisibleCols := rt.visibleSnapshotCols
	roundInput := rt.lastInputText
	roundSnapshotVersion := rt.snapshotAtRoundVersion
	updateCoalesce := time.Duration(0)
	if n.MessageID != "" {
		updateCoalesce = rt.manager.updateCoalesce
	}
	rt.mu.Unlock()
	if updateCoalesce > 0 && !rt.waitForNotificationUpdateCoalesce(version, updateCoalesce) {
		log.Printf("waiting notification update coalesced session=%s version=%d hash=%s delay=%s",
			n.SessionID, version, shortNotifyHash(contentHash), updateCoalesce)
		return
	}
	action := "create"
	if n.MessageID != "" {
		action = "patch"
	}
	log.Printf("lark card write queued source=waiting action=%s session=%s message=%s running=%v placeholder=%v update_no=%d version=%d hash=%s snapshot_source=%s content_len=%d content_lines=%d preview=%q",
		action, n.SessionID, n.MessageID, n.Running, n.Content == RunningNotificationPlaceholder, n.UpdateNo, version, shortNotifyHash(contentHash), n.SnapshotSource, len(n.Content), countLogLines(n.Content), previewLogText(n.Content, 160))
	rt.mu.Lock()
	if rt.session.Status != StatusWaiting || !rt.session.Live || !rt.session.NotifyOnWaiting || rt.notifyVersion != version {
		currentVersion := rt.notifyVersion
		currentStatus := rt.session.Status
		rt.mu.Unlock()
		log.Printf("waiting notification send skipped session=%s version=%d current_version=%d status=%s reason=stale_before_send",
			n.SessionID, version, currentVersion, currentStatus)
		return
	}
	if rt.notificationPatchVersion != n.NotificationVersion {
		currentPatchVersion := rt.notificationPatchVersion
		rt.mu.Unlock()
		log.Printf("waiting notification send skipped session=%s version=%d current_patch_version=%d note_patch_version=%d reason=stale_patch",
			n.SessionID, version, currentPatchVersion, n.NotificationVersion)
		return
	}
	claimHookCompletionTip := rt.applyHookCompletionTipPolicyLocked(&n)
	rt.mu.Unlock()
	result, err := rt.notifyWaitingWithRetry(n)
	if err != nil {
		log.Printf("waiting notification send failed session=%s version=%d hash=%s: %v", n.SessionID, version, shortNotifyHash(contentHash), err)
		return
	}
	log.Printf("waiting notification sent session=%s version=%d hash=%s", n.SessionID, version, shortNotifyHash(contentHash))
	rt.mu.Lock()
	if claimHookCompletionTip && rt.notifyVersion == version && rt.hookCompletedCurrentRound {
		rt.hookCompletionTipClaimed = true
	}
	boundInteractionMessageID := n.MessageID
	if result.MessageID != "" {
		boundInteractionMessageID = result.MessageID
	}
	rt.bindTerminalInteractionMessageLocked(n.Interaction, boundInteractionMessageID)
	sameRound := rt.lastInputText == roundInput && rt.snapshotAtRoundVersion == roundSnapshotVersion
	if rt.session.Status == StatusWaiting && rt.session.Live && rt.session.NotifyOnWaiting && rt.notifyVersion == version {
		if rt.notificationPatchVersion == n.NotificationVersion {
			rt.lastNotifiedRoundHash = contentHash
			if result.MessageID != "" {
				rt.lastNotifiedMessageID = result.MessageID
				rt.bindAutoRefreshMessageLocked(result.MessageID)
			}
			rt.lastNotifiedContent = n.Content
			rt.lastNotifiedVisibleSnapshot = notifiedVisibleSnapshot
			rt.lastNotifiedVisibleSnapshotSource = notifiedVisibleSnapshotSource
			rt.lastNotifiedVisibleResponder = notifiedVisibleResponder
			rt.lastNotifiedVisibleCols = notifiedVisibleCols
			if result.Updated {
				rt.notificationUpdateNo = n.UpdateNo
			}
			rt.notificationRunning = n.Running
		}
	} else if result.MessageID != "" && !result.Updated && sameRound && rt.lastNotifiedMessageID == "" {
		rt.lastNotifiedMessageID = result.MessageID
		rt.bindAutoRefreshMessageLocked(result.MessageID)
		rt.lastNotifiedContent = n.Content
		rt.lastNotifiedVisibleSnapshot = notifiedVisibleSnapshot
		rt.lastNotifiedVisibleSnapshotSource = notifiedVisibleSnapshotSource
		rt.lastNotifiedVisibleResponder = notifiedVisibleResponder
		rt.lastNotifiedVisibleCols = notifiedVisibleCols
		rt.notificationUpdateNo = n.UpdateNo
		rt.notificationRunning = n.Running
	}
	rt.mu.Unlock()
	defaultLarkMessageRegistry.rememberLatest(n.SessionID)
	rt.manager.notificationSent(n.SessionID)
}

// applyHookCompletionTipPolicyLocked allows at most one completion-tip write
// after a Hook completes the current round. A renderer reconnect may produce
// another valid snapshot and card patch, but that patch must not announce the
// same completion again. Submitting the next input resets both flags.
func (rt *RuntimeSession) applyHookCompletionTipPolicyLocked(note *WaitingNotification) bool {
	if note == nil || !rt.hookCompletedCurrentRound {
		return false
	}
	if rt.hookCompletionTipClaimed || note.Running {
		note.SuppressUpdateTip = true
		return false
	}
	return true
}

func (rt *RuntimeSession) waitForNotificationUpdateCoalesce(version int64, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	<-timer.C
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.session.Status == StatusWaiting &&
		rt.session.Live &&
		rt.session.NotifyOnWaiting &&
		rt.notifyVersion == version
}

func (rt *RuntimeSession) waitingNotificationLocked() (WaitingNotification, string, bool) {
	n, contentHash, ok, _ := rt.waitingNotificationCandidateLocked()
	return n, contentHash, ok
}

func (rt *RuntimeSession) markNotificationRunningLocked() (WaitingNotification, bool) {
	content := strings.TrimSpace(rt.lastNotifiedContent)
	if content == "" {
		content = strings.TrimSpace(rt.currentNotifyContentLocked())
	}
	switch {
	case rt.manager.notifier == nil:
		log.Printf("waiting notification running marker skipped session=%s reason=no_notifier", rt.session.ID)
		return WaitingNotification{}, false
	case rt.requireLarkChat && strings.TrimSpace(rt.session.LarkChatID) == "":
		log.Printf("waiting notification running marker skipped session=%s reason=waiting_for_lark_chat", rt.session.ID)
		return WaitingNotification{}, false
	case rt.lastNotifiedMessageID == "":
		log.Printf("waiting notification running marker skipped session=%s reason=no_message_id", rt.session.ID)
		return WaitingNotification{}, false
	case content == "":
		log.Printf("waiting notification running marker skipped session=%s message=%s reason=no_content", rt.session.ID, rt.lastNotifiedMessageID)
		return WaitingNotification{}, false
	}
	if _, ok := rt.manager.notifier.(WaitingRunningNotifier); !ok {
		log.Printf("waiting notification running marker skipped session=%s message=%s reason=notifier_unsupported", rt.session.ID, rt.lastNotifiedMessageID)
		return WaitingNotification{}, false
	}
	rt.notificationPatchVersion++
	rt.lastNotifiedContent = content
	rt.notificationRunning = true
	return WaitingNotification{
		SessionID:           rt.session.ID,
		Name:                rt.session.Name,
		Content:             content,
		MessageID:           rt.lastNotifiedMessageID,
		ChatID:              rt.session.LarkChatID,
		MentionOpenID:       rt.notificationMentionOpenID,
		UpdateNo:            rt.notificationUpdateNo,
		Running:             true,
		AutoRefreshEnabled:  rt.autoRefreshEnabled,
		AutoSummaryEnabled:  rt.autoSummaryEnabled,
		MentionModeEnabled:  rt.session.LarkMentionModeEnabled,
		NotificationVersion: rt.notificationPatchVersion,
		AgentContext:        cloneTerminalAgentContext(rt.lastTerminalAgentContext),
	}, true
}

func (rt *RuntimeSession) markNotificationWaitingLocked() (WaitingNotification, bool) {
	switch {
	case rt.manager.notifier == nil:
		return WaitingNotification{}, false
	case rt.lastNotifiedMessageID == "":
		return WaitingNotification{}, false
	case rt.lastNotifiedContent == "":
		return WaitingNotification{}, false
	}
	if _, ok := rt.manager.notifier.(WaitingRunningNotifier); !ok {
		return WaitingNotification{}, false
	}
	rt.notificationPatchVersion++
	rt.notificationRunning = false
	return WaitingNotification{
		SessionID:           rt.session.ID,
		Name:                rt.session.Name,
		Content:             rt.lastNotifiedContent,
		MessageID:           rt.lastNotifiedMessageID,
		ChatID:              rt.session.LarkChatID,
		MentionOpenID:       rt.notificationMentionOpenID,
		UpdateNo:            rt.notificationUpdateNo,
		Running:             false,
		AutoRefreshEnabled:  rt.autoRefreshEnabled,
		AutoSummaryEnabled:  rt.autoSummaryEnabled,
		MentionModeEnabled:  rt.session.LarkMentionModeEnabled,
		NotificationVersion: rt.notificationPatchVersion,
		Interaction:         cloneTerminalInteraction(rt.pendingTerminalInteraction),
		AgentContext:        cloneTerminalAgentContext(rt.lastTerminalAgentContext),
	}, true
}

func (rt *RuntimeSession) updateNotificationRunning(note WaitingNotification, running bool) {
	notifier, ok := rt.manager.notifier.(WaitingRunningNotifier)
	if !ok {
		return
	}
	rt.notificationPatchMu.Lock()
	defer rt.notificationPatchMu.Unlock()
	rt.mu.Lock()
	currentMessageID := rt.lastNotifiedMessageID
	if rt.notificationMessageFrozenLocked(note.MessageID) {
		rt.mu.Unlock()
		log.Printf("waiting notification running marker skipped session=%s message=%s running=%v reason=frozen_message",
			note.SessionID, note.MessageID, running)
		return
	}
	if currentMessageID != "" && currentMessageID != note.MessageID {
		rt.mu.Unlock()
		log.Printf("waiting notification running marker skipped session=%s message=%s current_message=%s running=%v reason=stale_message",
			note.SessionID, note.MessageID, currentMessageID, running)
		return
	}
	if note.NotificationVersion > 0 && rt.notificationPatchVersion != note.NotificationVersion {
		currentPatchVersion := rt.notificationPatchVersion
		rt.mu.Unlock()
		log.Printf("waiting notification running marker skipped session=%s message=%s current_patch_version=%d note_patch_version=%d running=%v reason=stale_patch",
			note.SessionID, note.MessageID, currentPatchVersion, note.NotificationVersion, running)
		return
	}
	rt.mu.Unlock()
	if err := rt.updateWaitingRunningWithRetry(notifier, note, running); err != nil {
		log.Printf("waiting notification running marker failed session=%s message=%s running=%v: %v", note.SessionID, note.MessageID, running, err)
		if running {
			rt.mu.Lock()
			if rt.lastNotifiedMessageID == note.MessageID && rt.notificationPatchVersion == note.NotificationVersion {
				rt.notificationRunning = false
			}
			rt.mu.Unlock()
		}
		return
	}
	log.Printf("waiting notification running marker updated session=%s message=%s running=%v", note.SessionID, note.MessageID, running)
}

func (rt *RuntimeSession) updateDisabledNotification(note WaitingNotification) {
	if rt == nil || rt.manager == nil || rt.manager.notifier == nil || !rt.manager.notifier.Available() {
		return
	}
	note.MessageID = strings.TrimSpace(note.MessageID)
	if note.MessageID == "" {
		return
	}
	note.Running = false
	note.Disabled = true
	note.AutoRefreshEnabled = false
	note.AutoSummaryEnabled = false
	note.SuppressUpdateTip = true
	rt.notificationPatchMu.Lock()
	defer rt.notificationPatchMu.Unlock()
	rt.mu.Lock()
	if !rt.notificationMessageFrozenLocked(note.MessageID) {
		rt.mu.Unlock()
		return
	}
	rt.mu.Unlock()
	if _, err := rt.notifyWaitingWithRetry(note); err != nil {
		log.Printf("disabled notification update failed session=%s message=%s: %v", note.SessionID, note.MessageID, err)
		return
	}
	log.Printf("disabled notification updated session=%s message=%s", note.SessionID, note.MessageID)
}

func (rt *RuntimeSession) notifyWaitingWithRetry(note WaitingNotification) (WaitingNotificationResult, error) {
	if rt == nil || rt.manager == nil || rt.manager.notifier == nil {
		return WaitingNotificationResult{}, errors.New("lark notifier is not configured")
	}
	note = rt.decorateWaitingNotification(note)
	var lastErr error
	for attempt := 1; attempt <= defaultNotificationSendAttempts; attempt++ {
		result, err := rt.manager.notifier.NotifyWaiting(note)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if attempt < defaultNotificationSendAttempts {
			time.Sleep(time.Duration(attempt) * defaultNotificationSendRetryDelay)
		}
	}
	return WaitingNotificationResult{}, lastErr
}

func (rt *RuntimeSession) updateWaitingRunningWithRetry(notifier WaitingRunningNotifier, note WaitingNotification, running bool) error {
	note = rt.decorateWaitingNotification(note)
	var lastErr error
	for attempt := 1; attempt <= defaultNotificationSendAttempts; attempt++ {
		if err := notifier.UpdateWaitingRunning(note, running); err != nil {
			lastErr = err
			if attempt < defaultNotificationSendAttempts {
				time.Sleep(time.Duration(attempt) * defaultNotificationSendRetryDelay)
			}
			continue
		}
		return nil
	}
	return lastErr
}

func (rt *RuntimeSession) decorateWaitingNotification(note WaitingNotification) WaitingNotification {
	if rt == nil || rt.manager == nil {
		return note
	}
	sess := rt.Snapshot()
	note.DeveloperModeEnabled = sess.DeveloperModeEnabled
	note.AgentKind = sess.LastAgentKind
	if sessionSupportsWorkspaceSwitch(sess) {
		note.WorkspaceOptions = rt.manager.WorkspaceOptionsForSession(sess)
	}
	note.AgentOptions = rt.manager.AvailableAgentOptions()
	defaultAgent, _ := rt.manager.AgentConfig()
	if defaultAgent.Kind == "custom" && !agentOptionExists(note.AgentOptions, defaultAgent.ID) {
		note.AgentOptions = append(note.AgentOptions, AgentOption{ID: defaultAgent.ID, Label: defaultAgent.Name, Kind: defaultAgent.Kind, Command: defaultAgent.Command})
	}
	note.AgentID = matchingAgentOptionID(sess, note.AgentOptions)
	return note
}

func agentOptionExists(options []AgentOption, id string) bool {
	for _, option := range options {
		if strings.EqualFold(strings.TrimSpace(option.ID), strings.TrimSpace(id)) {
			return true
		}
	}
	return false
}

func matchingAgentOptionID(sess Session, options []AgentOption) string {
	if agentOptionExists(options, sess.LastAgentID) {
		return strings.TrimSpace(sess.LastAgentID)
	}
	startCommand := strings.TrimSpace(sess.LastAgentStartCommand)
	for _, option := range options {
		if startCommand != "" && strings.TrimSpace(option.Command) == startCommand {
			return option.ID
		}
	}
	return ""
}

func (rt *RuntimeSession) waitingNotificationCandidateLocked() (WaitingNotification, string, bool, string) {
	if rt.requireLarkChat && strings.TrimSpace(rt.session.LarkChatID) == "" {
		return WaitingNotification{}, "", false, "waiting_for_lark_chat"
	}
	hookContent := rt.hookAssistantNotifyContentLocked()
	if hookContent != "" {
		contentHash := notifyContentHash(hookContent)
		if contentHash == rt.lastNotifiedRoundHash {
			return WaitingNotification{}, "", false, "duplicate_hash"
		}
		interaction := rt.notificationInteractionLocked(rt.lastNotifiedMessageID)
		agentContext := rt.notificationAgentContextLocked()
		return WaitingNotification{SessionID: rt.session.ID, Name: rt.session.Name, Content: hookContent, ChatID: rt.session.LarkChatID, MentionOpenID: rt.notificationMentionOpenID, AutoSummaryEnabled: rt.autoSummaryEnabled, MentionModeEnabled: rt.session.LarkMentionModeEnabled, SnapshotSource: "codex_hook:last_assistant_message", Interaction: interaction, AgentContext: agentContext}, contentHash, true, "ready"
	}
	if rt.visibleSnapshotStaleForCurrentRoundLocked() {
		return WaitingNotification{}, "", false, "stale_visible_snapshot"
	}
	if notifyContentNeedsMoreSnapshotWithWindowAnchorPolicy(rt.visibleSnapshot, rt.previousNotifySnapshotLocked(), rt.roundReply, rt.lastInputText, rt.notificationWindowInputText, rt.notifyTextAnchorPolicyLocked()) {
		return WaitingNotification{}, "", false, "needs_more_snapshot"
	}
	content := rt.currentNotifyContentLocked()
	content = strings.TrimSpace(content)
	if content == "" {
		return WaitingNotification{}, "", false, "empty_content"
	}
	content = rt.stableNotifyContentForMessageLocked(rt.lastNotifiedMessageID, content)
	contentHash := notifyContentHash(content)
	if contentHash == rt.lastNotifiedRoundHash {
		return WaitingNotification{}, "", false, "duplicate_hash"
	}
	interaction := rt.notificationInteractionLocked(rt.lastNotifiedMessageID)
	agentContext := rt.notificationAgentContextLocked()
	return WaitingNotification{SessionID: rt.session.ID, Name: rt.session.Name, Content: content, ChatID: rt.session.LarkChatID, MentionOpenID: rt.notificationMentionOpenID, AutoSummaryEnabled: rt.autoSummaryEnabled, MentionModeEnabled: rt.session.LarkMentionModeEnabled, SnapshotSource: rt.visibleSnapshotSource, Interaction: interaction, AgentContext: agentContext}, contentHash, true, "ready"
}

func (rt *RuntimeSession) fallbackWaitingNotificationCandidateLocked() (WaitingNotification, string, bool, string) {
	if rt.requireLarkChat && strings.TrimSpace(rt.session.LarkChatID) == "" {
		return WaitingNotification{}, "", false, "waiting_for_lark_chat"
	}
	if rt.visibleSnapshotStaleForCurrentRoundLocked() {
		return WaitingNotification{}, "", false, "stale_visible_snapshot"
	}
	content := pickNotifyContentWithWindowAnchorPolicy(rt.visibleSnapshot, rt.previousNotifySnapshotLocked(), rt.roundReply, rt.lastInputText, rt.notificationWindowInputText, rt.notifyTextAnchorPolicyLocked())
	content = rt.cleanLarkNotifyContentForAgentLocked(content)
	content = strings.TrimSpace(content)
	if content == "" {
		return WaitingNotification{}, "", false, "empty_content"
	}
	if !hasMeaningfulNotifyContent(content) {
		return WaitingNotification{}, "", false, "needs_more_snapshot"
	}
	content = rt.stableNotifyContentForMessageLocked(rt.lastNotifiedMessageID, content)
	contentHash := notifyContentHash(content)
	if contentHash == rt.lastNotifiedRoundHash {
		return WaitingNotification{}, "", false, "duplicate_hash"
	}
	source := strings.TrimSpace(rt.visibleSnapshotSource)
	if source == "" {
		source = "fallback"
	} else {
		source += ":fallback"
	}
	interaction := rt.notificationInteractionLocked(rt.lastNotifiedMessageID)
	agentContext := rt.notificationAgentContextLocked()
	return WaitingNotification{SessionID: rt.session.ID, Name: rt.session.Name, Content: content, ChatID: rt.session.LarkChatID, MentionOpenID: rt.notificationMentionOpenID, AutoSummaryEnabled: rt.autoSummaryEnabled, MentionModeEnabled: rt.session.LarkMentionModeEnabled, SnapshotSource: source, Interaction: interaction, AgentContext: agentContext}, contentHash, true, "ready"
}

func (rt *RuntimeSession) startupFallbackWaitingNotificationCandidateLocked() (WaitingNotification, string, bool) {
	if rt.requireLarkChat && strings.TrimSpace(rt.session.LarkChatID) == "" {
		return WaitingNotification{}, "", false
	}
	content := strings.TrimSpace(pickLarkStartupFallbackContent(rt.visibleSnapshot))
	if content == "" {
		return WaitingNotification{}, "", false
	}
	contentHash := notifyContentHash(content)
	if contentHash == rt.lastNotifiedRoundHash {
		return WaitingNotification{}, "", false
	}
	source := strings.TrimSpace(rt.visibleSnapshotSource)
	if source == "" {
		source = "startup_fallback"
	} else {
		source += ":startup_fallback"
	}
	return WaitingNotification{
		SessionID:          rt.session.ID,
		Name:               rt.session.Name,
		Content:            content,
		ChatID:             rt.session.LarkChatID,
		MentionOpenID:      rt.notificationMentionOpenID,
		AutoSummaryEnabled: rt.autoSummaryEnabled,
		MentionModeEnabled: rt.session.LarkMentionModeEnabled,
		SnapshotSource:     source,
		AgentContext:       rt.notificationAgentContextLocked(),
	}, contentHash, true
}

func (rt *RuntimeSession) fallbackTailWaitingNotificationCandidateLocked() (WaitingNotification, string, bool) {
	if !rt.fallbackTailRoundStartedLocked() {
		return WaitingNotification{}, "", false
	}
	content := strings.TrimSpace(pickLarkNotifyFallbackTailContent(rt.visibleSnapshot))
	content = strings.TrimSpace(rt.cleanLarkNotifyContentForAgentLocked(content))
	if content == "" || !hasReplyLine(content, rt.lastInputText) {
		return WaitingNotification{}, "", false
	}
	content = rt.stableNotifyContentForMessageLocked(rt.lastNotifiedMessageID, content)
	contentHash := notifyContentHash(content)
	if contentHash == rt.lastNotifiedRoundHash && !rt.notificationRunning {
		return WaitingNotification{}, "", false
	}
	messageID := strings.TrimSpace(rt.lastNotifiedMessageID)
	interaction := rt.notificationInteractionLocked(messageID)
	agentContext := rt.notificationAgentContextLocked()
	return WaitingNotification{
		SessionID:          rt.session.ID,
		Name:               rt.session.Name,
		Content:            content,
		ChatID:             rt.session.LarkChatID,
		MentionOpenID:      rt.notificationMentionOpenID,
		AutoSummaryEnabled: rt.autoSummaryEnabled,
		MentionModeEnabled: rt.session.LarkMentionModeEnabled,
		SnapshotSource:     rt.visibleSnapshotSource + ":tail_fallback",
		Interaction:        interaction,
		AgentContext:       agentContext,
	}, contentHash, true
}

func (rt *RuntimeSession) fallbackTailRoundStartedLocked() bool {
	input := strings.TrimSpace(rt.lastInputText)
	if input == "" || len(rt.roundReply) > 0 {
		return true
	}
	return containsInputEchoLine(trimVisibleText(rt.visibleSnapshot), input)
}

// emptyWaitingNotificationCandidateLocked closes an already-created running
// card when the completed round contains no safe, meaningful text to publish.
// It deliberately uses a neutral placeholder instead of falling back to raw
// terminal history, which could belong to an earlier round.
func (rt *RuntimeSession) emptyWaitingNotificationCandidateLocked() (WaitingNotification, string, bool) {
	messageID := strings.TrimSpace(rt.lastNotifiedMessageID)
	if !rt.notificationRunning || messageID == "" || rt.notificationMessageFrozenLocked(messageID) {
		return WaitingNotification{}, "", false
	}
	content := EmptyNotificationPlaceholder
	contentHash := notifyContentHash(content)
	interaction := rt.notificationInteractionLocked(messageID)
	agentContext := rt.notificationAgentContextLocked()
	return WaitingNotification{
		SessionID:          rt.session.ID,
		Name:               rt.session.Name,
		Content:            content,
		ChatID:             rt.session.LarkChatID,
		MentionOpenID:      rt.notificationMentionOpenID,
		AutoSummaryEnabled: rt.autoSummaryEnabled,
		MentionModeEnabled: rt.session.LarkMentionModeEnabled,
		SnapshotSource:     rt.visibleSnapshotSource,
		Interaction:        interaction,
		AgentContext:       agentContext,
	}, contentHash, true
}

func (rt *RuntimeSession) notifyContentNeedsMoreSnapshotLocked() bool {
	if rt.visibleSnapshotStaleForCurrentRoundLocked() {
		return true
	}
	return notifyContentNeedsMoreSnapshotWithWindowAnchorPolicy(rt.visibleSnapshot, rt.previousNotifySnapshotLocked(), rt.roundReply, rt.lastInputText, rt.notificationWindowInputText, rt.notifyTextAnchorPolicyLocked())
}

func (rt *RuntimeSession) visibleSnapshotStaleForCurrentRoundLocked() bool {
	if strings.TrimSpace(rt.lastInputText) == "" || strings.TrimSpace(rt.visibleSnapshot) == "" {
		return false
	}
	if rt.visibleSnapshotVersion <= rt.snapshotAtRoundVersion {
		return true
	}
	return normalizeSnapshotText(rt.visibleSnapshot) == normalizeSnapshotText(rt.snapshotAtRoundStart)
}

func (rt *RuntimeSession) rescheduleNotifyRetryLocked(version int64) {
	if rt.session.Status != StatusWaiting || !rt.session.Live || rt.notifyVersion != version ||
		(!rt.session.NotifyOnWaiting && rt.startupNotifyMode != startupNotifyDiscard) {
		return
	}
	rt.stopNotifyTimerLocked()
	rt.notifyRetryTimer = time.AfterFunc(defaultNotifyRetryDelay, func() {
		rt.notifyIfStillWaiting(version)
	})
}

func notifyContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func shortNotifyHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

func previewLogText(text string, max int) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max]) + "..."
}

func countLogLines(text string) int {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if text == "" {
		return 0
	}
	return strings.Count(text, "\n") + 1
}

func (rt *RuntimeSession) stopNotifyTimerLocked() {
	if rt.notifyRetryTimer != nil {
		rt.notifyRetryTimer.Stop()
		rt.notifyRetryTimer = nil
	}
}

func (rt *RuntimeSession) stopNotifyStableTimerLocked() {
	if rt.notifyStableTimer != nil {
		rt.notifyStableTimer.Stop()
		rt.notifyStableTimer = nil
	}
}

func (rt *RuntimeSession) stopStartupNotifyTimerLocked() {
	if rt.startupNotifyTimer != nil {
		rt.startupNotifyTimer.Stop()
		rt.startupNotifyTimer = nil
	}
}

func (rt *RuntimeSession) scheduleStartupNotifyFinalLocked(delay time.Duration) {
	if delay <= 0 {
		delay = defaultStartupPresetSettleDelay
	}
	rt.stopStartupNotifyTimerLocked()
	rt.startupNotifyTimer = time.AfterFunc(delay, func() {
		rt.mu.Lock()
		if rt.startupNotifyMode != startupNotifySettling || !rt.session.Live {
			rt.mu.Unlock()
			return
		}
		rt.startupNotifyMode = startupNotifyFinal
		version := rt.notifyVersion
		waiting := rt.session.Status == StatusWaiting
		rt.mu.Unlock()
		if waiting {
			rt.notifyIfStillWaiting(version)
		}
	})
}
