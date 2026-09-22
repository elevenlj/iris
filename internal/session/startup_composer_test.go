package session

import (
	"strings"
	"testing"
	"time"
)

type slowStartupNotifier struct {
	recordingNotifier
	started     chan struct{}
	allowCreate chan struct{}
}

func (n *slowStartupNotifier) NotifyWaiting(note WaitingNotification) (WaitingNotificationResult, error) {
	if note.Startup && note.MessageID == "" {
		close(n.started)
		<-n.allowCreate
	}
	return n.recordingNotifier.NotifyWaiting(note)
}

func TestStartupComposerProbeSurvivesContinuousRepaint(t *testing.T) {
	notifier := &slowStartupNotifier{recordingNotifier: recordingNotifier{messageID: "startup"}, started: make(chan struct{}), allowCreate: make(chan struct{})}
	m := NewManager(nil, nil, WithNotifier(notifier), WithIsolatedMessageRegistry())
	released := make(chan string, 4)
	m.SetNotificationSentHook(func(id string) { released <- id })
	rt := &RuntimeSession{manager: m, startupNotifyMode: startupNotifyDiscard,
		session: Session{ID: "animated-startup", Live: true, Status: StatusRunning,
			LastMode: SessionModeAgent, LastAgentKind: "traecli", LastAgentStartCommand: TraeAgentCommand, NotifyOnWaiting: true}}
	sub, cancel := rt.Subscribe()
	defer cancel()
	defer rt.Close()
	go rt.beginStartupNotification("")
	<-notifier.started
	ready := make(chan struct{})
	go func() {
		for event := range sub {
			if event.Type != RuntimeEventSnapshotRequest {
				continue
			}
			screen := "TraeCode CLI\nLoading..."
			select {
			case <-ready:
				screen = "TraeCode CLI\n❯ Write tests for @filename\nGPT-5.6-Sol medium · Context 100% left"
			default:
			}
			source := strings.Replace(strings.Replace(aidenReadySource, "headless:", "browser:", 1), "cursor_line=-1", "cursor_line=1", 1)
			rt.SetVisibleSnapshotResponseFrom(screen, source, event.RequestID, sub)
		}
	}()
	// Captured TRAE welcome-logo repaint: save cursor, draw, restore cursor.
	repaint := []byte("\x1b7\x1b[3;5H◆\x1b[3;7H◆\x1b8")
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(1500 * time.Millisecond)
loading:
	for {
		select {
		case <-ticker.C:
			rt.HandleOutput(repaint)
		case <-released:
			t.Fatal("released input on a loading banner")
		case <-deadline:
			break loading
		}
	}
	close(ready)
	// The composer is ready while Feishu is still creating its startup card.
	// Completion must not be lost, and queued input must not run before the card exists.
	deadline = time.After(1500 * time.Millisecond)
creating:
	for {
		select {
		case <-ticker.C:
			rt.HandleOutput(repaint)
		case <-released:
			close(notifier.allowCreate)
			t.Fatal("released startup while its card was still being created")
		case <-deadline:
			break creating
		}
	}
	close(notifier.allowCreate)
	deadline = time.After(4 * time.Second)
waiting:
	for {
		select {
		case <-ticker.C:
			rt.HandleOutput(repaint)
		case <-released:
			break waiting
		case <-deadline:
			t.Fatal("continuous repaint starved startup detection")
		}
	}
	rt.HandleOutput(repaint)
	if rt.Snapshot().Status != StatusWaiting || rt.ShouldQueueInputWhileRunning() {
		t.Fatal("ready composer must stay idle and release queued input")
	}
	completed := false
	for _, note := range notifier.notes() {
		completed = completed || note.StartupComplete
		if note.Content != StartupNotificationPlaceholder && note.Content != StartupCompletePlaceholder {
			t.Fatalf("automatic card leaked terminal content: %q", note.Content)
		}
	}
	if !completed {
		t.Fatal("startup card was not marked complete")
	}
	before := notifier.count()
	rt.NotifyInputRunning()
	if notifier.count() != before {
		t.Fatal("idle startup created a phantom task card")
	}
	rt.MarkStructuredInputActivity("hello")
	rt.NotifyInputRunning()
	if notifier.count() != before+1 {
		t.Fatal("real user input did not create its running card")
	}
	rt.HandleOutput(repaint)
	if rt.Snapshot().Status != StatusRunning || rt.hookCompletedCurrentRound {
		t.Fatal("normal tasks must still wait for the real completion event")
	}
}
