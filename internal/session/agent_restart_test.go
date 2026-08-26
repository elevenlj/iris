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

func TestRestartAgentResumesExactAidenCodexSession(t *testing.T) {
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
	if writes := terminal.snapshotWrites(); len(writes) != 1 || writes[0] != resumeCommand+"\r" {
		t.Fatalf("restart writes = %#v", writes)
	}
	if got := rt.Snapshot(); got.LastAgentStartCommand != startCommand || got.LastAgentResumeCommand != resumeCommand || got.LastAgentKind != "codex" {
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
