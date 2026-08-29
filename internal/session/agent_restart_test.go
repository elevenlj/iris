package session

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type controlledForegroundTerminal struct {
	mu        sync.Mutex
	writes    []string
	started   chan struct{}
	release   chan struct{}
	stopErr   error
	startOnce sync.Once
}

func newControlledForegroundTerminal() *controlledForegroundTerminal {
	return &controlledForegroundTerminal{started: make(chan struct{}), release: make(chan struct{})}
}

func (t *controlledForegroundTerminal) Read([]byte) (int, error) { return 0, io.EOF }
func (t *controlledForegroundTerminal) Write(data []byte) (int, error) {
	t.mu.Lock()
	t.writes = append(t.writes, string(data))
	t.mu.Unlock()
	return len(data), nil
}
func (t *controlledForegroundTerminal) Close() error                { return nil }
func (t *controlledForegroundTerminal) Resize(uint16, uint16) error { return nil }
func (t *controlledForegroundTerminal) TerminateForegroundProcess(ctx context.Context) error {
	t.startOnce.Do(func() { close(t.started) })
	if t.stopErr != nil {
		return t.stopErr
	}
	select {
	case <-t.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (t *controlledForegroundTerminal) snapshotWrites() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.writes...)
}

func TestRestartAgentWaitsForConfirmedProcessExitBeforeStarting(t *testing.T) {
	terminal := newControlledForegroundTerminal()
	rt := &RuntimeSession{
		terminal: terminal,
		session: Session{
			ID:                     "sess-restart",
			Live:                   true,
			LastMode:               SessionModeAgent,
			LastAgentKind:          "codex",
			LastAgentStartCommand:  "codex --dangerously-bypass-approvals-and-sandbox",
			LastAgentResumeCommand: "codex resume --last",
		},
	}
	if err := rt.RestartAgent(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-terminal.started:
	case <-time.After(time.Second):
		t.Fatal("foreground termination did not start")
	}
	if writes := terminal.snapshotWrites(); len(writes) != 0 {
		t.Fatalf("Agent command started before exit confirmation: %#v", writes)
	}
	if err := rt.RestartAgent(); err == nil || !strings.Contains(err.Error(), "正在重启") {
		t.Fatalf("duplicate restart error = %v", err)
	}
	close(terminal.release)
	waitForAgentRestartWrites(t, terminal, 1)
	if writes := terminal.snapshotWrites(); len(writes) != 1 || writes[0] != "codex --dangerously-bypass-approvals-and-sandbox\r" {
		t.Fatalf("restart writes = %#v", writes)
	}
	if sess := rt.Snapshot(); sess.LastMode != SessionModeAgent || sess.LastAgentKind != "codex" {
		t.Fatalf("restart recovery state = %#v", sess)
	}
}

func TestRestartAgentSubmitsFollowUpAfterNewComposerIsReady(t *testing.T) {
	oldTimeout := agentRestartContextTimeout
	oldPoll := agentRestartReadyPollInterval
	oldEnterDelay := structuredInputEnterDelay
	agentRestartContextTimeout = time.Second
	agentRestartReadyPollInterval = time.Millisecond
	structuredInputEnterDelay = 0
	t.Cleanup(func() {
		agentRestartContextTimeout = oldTimeout
		agentRestartReadyPollInterval = oldPoll
		structuredInputEnterDelay = oldEnterDelay
	})

	terminal := newControlledForegroundTerminal()
	notifier := &recordingNotifier{messageID: "bot-card"}
	manager := NewManager(nil, nil, WithNotifier(notifier))
	rt := &RuntimeSession{
		manager:                   manager,
		terminal:                  terminal,
		lastNotifiedMessageID:     "bot-card",
		lastNotifiedContent:       "原卡片正文",
		notificationRunning:       true,
		notificationMentionOpenID: "ou-old",
		session: Session{
			ID:                    "sess-restart-follow-up",
			Status:                StatusWaiting,
			Live:                  true,
			NotifyOnWaiting:       true,
			LarkChatID:            "oc-group",
			LastMode:              SessionModeAgent,
			LastAgentKind:         "codex",
			LastAgentStartCommand: "codex --dangerously-bypass-approvals-and-sandbox",
		},
	}
	subscriber, cancel := rt.Subscribe()
	t.Cleanup(cancel)
	readySnapshot := "OpenAI Codex\n› Ask Codex to do anything"
	readySource := "browser:buffer;continuity_version=2;render_epoch=2;buffer_type=normal;buffer_at_capacity=false;anchor_guard_active=false;anchor_guard_line=-1;cursor_line=1"
	var outputOnce sync.Once
	go func() {
		for event := range subscriber {
			if event.Type != RuntimeEventSnapshotRequest {
				continue
			}
			outputOnce.Do(func() { rt.HandleOutput([]byte("new Agent boot output")) })
			rt.SetVisibleSnapshotResponseFrom(readySnapshot, readySource, event.RequestID, subscriber)
		}
	}()

	prompt := "读取当前群消息并继续任务"
	if err := rt.RestartAgentWithFollowUp(prompt, "ou-user", "bot-card"); err != nil {
		t.Fatal(err)
	}
	<-terminal.started
	close(terminal.release)
	waitForAgentRestartWrites(t, terminal, 3)
	writes := terminal.snapshotWrites()
	if writes[0] != "codex --dangerously-bypass-approvals-and-sandbox\r" || writes[1] != prompt || writes[2] != "\r" {
		t.Fatalf("restart follow-up writes = %#v", writes)
	}
	rt.mu.Lock()
	pending := rt.agentRestartPending
	mentionOpenID := rt.notificationMentionOpenID
	messageID := rt.lastNotifiedMessageID
	_, frozen := rt.frozenNotificationMessages["bot-card"]
	rt.mu.Unlock()
	if pending || mentionOpenID != "ou-user" || messageID != "bot-card" || frozen {
		t.Fatalf("restart follow-up state: pending=%v mention=%q message=%q frozen=%v", pending, mentionOpenID, messageID, frozen)
	}
}

func TestRestartAgentFollowUpTimesOutWithoutNewComposer(t *testing.T) {
	oldTimeout := agentRestartContextTimeout
	oldPoll := agentRestartReadyPollInterval
	agentRestartContextTimeout = 20 * time.Millisecond
	agentRestartReadyPollInterval = time.Millisecond
	t.Cleanup(func() {
		agentRestartContextTimeout = oldTimeout
		agentRestartReadyPollInterval = oldPoll
	})

	terminal := newControlledForegroundTerminal()
	notifier := &recordingNotifier{messageID: "bot-card"}
	rt := &RuntimeSession{
		manager:               NewManager(nil, nil, WithNotifier(notifier)),
		terminal:              terminal,
		lastNotifiedMessageID: "bot-card",
		lastNotifiedContent:   "原卡片正文",
		notificationRunning:   true,
		session: Session{
			ID:                    "sess-restart-follow-up-timeout",
			Status:                StatusWaiting,
			Live:                  true,
			NotifyOnWaiting:       true,
			LarkChatID:            "oc-group",
			LastMode:              SessionModeAgent,
			LastAgentKind:         "codex",
			LastAgentStartCommand: "codex",
		},
	}
	if err := rt.RestartAgentWithFollowUp("读取群消息", "", ""); err != nil {
		t.Fatal(err)
	}
	<-terminal.started
	close(terminal.release)
	waitForAgentRestartPending(t, rt, false)
	if writes := terminal.snapshotWrites(); len(writes) != 1 || writes[0] != "codex\r" {
		t.Fatalf("timed-out restart follow-up writes = %#v", writes)
	}
	rt.mu.Lock()
	status := rt.session.Status
	content := rt.lastNotifiedContent
	rt.mu.Unlock()
	if status != StatusWaiting || !strings.Contains(content, "上下文恢复指令发送失败") {
		t.Fatalf("timed-out restart state: status=%s content=%q", status, content)
	}
	notes := waitForRunningNotes(t, notifier, 1)
	if len(notes) != 1 || notes[0].MessageID != "bot-card" || !strings.Contains(notes[0].Content, "上下文恢复指令发送失败") || notes[0].Running {
		t.Fatalf("timed-out restart notification = %#v", notes)
	}
}

func TestRestartAgentCancelsRelaunchWhenExitCannotBeConfirmed(t *testing.T) {
	terminal := newControlledForegroundTerminal()
	terminal.stopErr = errors.New("still running")
	rt := &RuntimeSession{
		terminal: terminal,
		session:  Session{ID: "sess-restart-fail", Live: true, LastMode: SessionModeAgent, LastAgentKind: "custom", LastAgentStartCommand: "my-agent"},
	}
	if err := rt.RestartAgent(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		rt.mu.Lock()
		pending := rt.agentRestartPending
		rt.mu.Unlock()
		if !pending {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if writes := terminal.snapshotWrites(); len(writes) != 0 {
		t.Fatalf("failed termination must not relaunch Agent: %#v", writes)
	}
	if sess := rt.Snapshot(); sess.LastMode != SessionModeAgent {
		t.Fatalf("failed restart should restore Agent mode: %#v", sess)
	}
}

func TestRestartAgentStartsFreshAidenCodexSession(t *testing.T) {
	terminal := newControlledForegroundTerminal()
	threadID := "019f5153-6e7f-7742-9f61-3ffe1530d61c"
	startCommand := "aiden x codex --dangerously-bypass-approvals-and-sandbox"
	resumeCommand := "aiden x codex resume " + threadID + " --dangerously-bypass-approvals-and-sandbox"
	rt := &RuntimeSession{
		terminal: terminal,
		session: Session{
			ID:                     "sess-aiden-restart",
			Live:                   true,
			LastMode:               SessionModeAgent,
			LastAgentID:            "custom-aiden",
			LastAgentKind:          "codex",
			LastAgentStartCommand:  startCommand,
			LastAgentResumeCommand: resumeCommand,
		},
	}
	if err := rt.RestartAgent(); err != nil {
		t.Fatal(err)
	}
	<-terminal.started
	close(terminal.release)
	waitForAgentRestartWrites(t, terminal, 1)
	if writes := terminal.snapshotWrites(); len(writes) != 1 || writes[0] != startCommand+"\r" {
		t.Fatalf("restart writes = %#v", writes)
	}
	if got := rt.Snapshot(); got.LastAgentStartCommand != startCommand || got.LastAgentResumeCommand == resumeCommand || !strings.Contains(got.LastAgentResumeCommand, "resume") || !strings.Contains(got.LastAgentResumeCommand, "--last") || got.LastAgentKind != "codex" {
		t.Fatalf("restart state = %#v", got)
	}
}

func waitForAgentRestartWrites(t *testing.T, terminal *controlledForegroundTerminal, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(terminal.snapshotWrites()) >= count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Agent restart writes = %#v, want at least %d", terminal.snapshotWrites(), count)
}

func waitForAgentRestartPending(t *testing.T, rt *RuntimeSession, want bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		rt.mu.Lock()
		pending := rt.agentRestartPending
		rt.mu.Unlock()
		if pending == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("Agent restart pending did not become %v", want)
}
