package session

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Aiden OptionsList/KeyTips render numbered rows but accept arrows, not digits.
const nativeModelMenu = "Select Model\n\n❯  1. gateway model-a\n   2. gateway model-b\n\n↑↓ Navigate • Enter Confirm • Esc Cancel"
const nativeReasoningMenu = "Select Reasoning Level for model-b\nCurrent reasoning: Medium\n\n   1. Low\n❯  2. Medium\n   3. High\n\n↑/↓ select · Enter confirm · ← back to models"

func TestTerminalMenuDetection(t *testing.T) {
	for _, tc := range []struct {
		name, screen, key string
		index             int
		want              bool
	}{
		{"Aiden models", nativeModelMenu, "\x1b[B", 1, true},
		{"Aiden reasoning", nativeReasoningMenu, "\x1b[A", 0, true},
		{"unnumbered", "Choose model\n❯ model-a\n  model-b\n↑↓ Navigate · Enter Confirm · Esc Cancel", "\x1b[B", 1, true},
		{"Chinese", "请选择模型\n❯ 模型甲\n  模型乙\n上下选择，回车确认，Esc 取消", "\x1b[B", 1, true},
		{"Claude confirmation", "Do you want to proceed?\n❯ 1. Yes\n  2. No\nEnter to confirm · Esc to cancel", "\x1b[B", 1, true},
		{"multi", "Choose tools\n❯ [x] Read\n  [ ] Write\nSpace to select · Enter to submit · Tab/Arrow keys to navigate · Esc to cancel", "\x1b[B ", 1, true},
		{"checkbox enter", "Choose tools\n❯ ☒ Read\n  ☐ Write\n↑↓ Navigate • Enter Confirm • Esc Cancel", "\x1b[B", 1, true},
		{"plain list", "Models\n1. model-a\n2. model-b", "", 0, false},
		{"no focus", strings.Replace(nativeModelMenu, "❯", " ", 1), "", 0, false},
		{"old menu", nativeModelMenu + "\n> new prompt", "", 0, false},
		{"shell", nativeModelMenu + "\n$ ", "", 0, false},
		{"incomplete", strings.Split(nativeModelMenu, "↑")[0], "", 0, false},
		{"quoted menu", "```\n" + nativeModelMenu + "\n```", "", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			menu := DetectTerminalMenu(tc.screen, "s", 1, 2)
			if (menu != nil) != tc.want {
				t.Fatalf("menu=%#v", menu)
			}
			if menu == nil {
				return
			}
			if menu.Options[tc.index].Input != tc.key {
				t.Fatalf("option=%#v", menu.Options[tc.index])
			}
			if startupAgentComposerReady(tc.screen, aidenReadySource, "aiden") {
				t.Fatal("menu counted as ready")
			}
			card, err := larkNotificationCardContent(WaitingNotification{SessionID: "s", Content: tc.screen, Interaction: menu}, "u", true)
			if err != nil || !strings.Contains(card, `"tag":"select_static"`) {
				t.Fatalf("selector missing without developer mode: %s %v", card, err)
			}
		})
	}
}

func TestTerminalMenuStartupAndChainedNotifications(t *testing.T) {
	for _, startup := range []bool{false, true} {
		notifier := &recordingNotifier{createMessageIDs: []string{"card"}}
		m := NewManager(nil, nil, WithNotifier(notifier), WithIsolatedMessageRegistry())
		rt := &RuntimeSession{manager: m, session: Session{ID: "s", Live: true, Status: StatusWaiting, NotifyOnWaiting: true, LastMode: SessionModeAgent, LastAgentKind: "aiden", LastAgentStartCommand: AidenAgentCommand}, notifyVersion: 1, visibleSnapshotVersion: 1, visibleSnapshotSource: aidenReadySource, visibleSnapshot: nativeModelMenu}
		if startup {
			rt.startupNotifyMode = startupNotifyDiscard
			rt.beginStartupNotification("")
		}
		rt.notifyIfStillWaitingForInteraction(1)
		first := notifier.notes()
		if len(first) == 0 || first[len(first)-1].Interaction == nil {
			t.Fatalf("no startup=%v selector: %#v", startup, first)
		}
		card, err := larkNotificationCardContent(first[len(first)-1], "user", true)
		if err != nil || !strings.Contains(card, `"tag":"select_static"`) {
			t.Fatalf("startup selector missing %s %v", card, err)
		}
		rt.mu.Lock()
		rt.visibleSnapshot = nativeReasoningMenu
		rt.visibleSnapshotVersion++
		rt.notifyVersion++
		version := rt.notifyVersion
		rt.mu.Unlock()
		rt.notifyIfStillWaitingForInteraction(version)
		notes := notifier.notes()
		last := notes[len(notes)-1]
		if last.Interaction == nil || last.MessageID != "card" || last.Completed || !last.SuppressUpdateTip {
			t.Fatalf("chain lost card or reported task completion: %#v", last)
		}
		rt.mu.Lock()
		rt.visibleSnapshot = aidenReadySnapshot
		rt.visibleSnapshotVersion++
		rt.mu.Unlock()
		rt.notifyIfStillWaitingForInteraction(version)
		notes = notifier.notes()
		last = notes[len(notes)-1]
		if last.Interaction != nil || last.Completed || !last.SuppressUpdateTip {
			t.Fatalf("menu not cleared: %#v", last)
		}
		if startup && !last.StartupComplete {
			t.Fatal("startup not completed")
		}
		rt.Close()
	}
}

func TestTerminalMenuSelectionRejectsChangedScreen(t *testing.T) {
	rt := &RuntimeSession{session: Session{ID: "s", Live: true, Status: StatusWaiting}, notifyVersion: 1, visibleSnapshotVersion: 2, visibleSnapshot: nativeModelMenu, visibleSnapshotSource: aidenReadySource, lastNotifiedMessageID: "card"}
	menu := rt.notificationInteractionLocked("card")
	if menu == nil {
		t.Fatal("no menu")
	}
	rt.visibleSnapshot = aidenReadySnapshot
	if _, err := rt.consumeTerminalInteraction(menu.ID, "opt_2", "card"); err == nil {
		t.Fatal("accepted stale selector")
	}
}

func TestTerminalMenuLiveStartupSnapshotDoesNotLoop(t *testing.T) {
	notifier := &recordingNotifier{createMessageIDs: []string{"startup-card"}}
	m := NewManager(nil, nil, WithNotifier(notifier), WithIsolatedMessageRegistry())
	var released, requests atomic.Int32
	m.SetNotificationSentHook(func(string) { released.Add(1) })
	rt := &RuntimeSession{manager: m, session: Session{ID: "live-menu", Live: true, Status: StatusRunning, NotifyOnWaiting: true, LastMode: SessionModeAgent, LastAgentKind: "aiden"}, startupNotifyMode: startupNotifyDiscard}
	subscriber, cancel := rt.Subscribe()
	defer cancel()
	defer rt.Close()
	go func() {
		for event := range subscriber {
			if event.Type == RuntimeEventSnapshotRequest {
				requests.Add(1)
				rt.SetVisibleSnapshotResponseFrom(nativeModelMenu, strings.Replace(aidenReadySource, "headless:", "browser:", 1), event.RequestID, subscriber)
			}
		}
	}()
	rt.SetVisibleSnapshotWithSource(nativeModelMenu, aidenReadySource)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		notes := notifier.notes()
		if len(notes) > 0 && notes[len(notes)-1].Interaction != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	notes := notifier.notes()
	if len(notes) != 2 || notes[1].Interaction == nil || notes[1].MessageID != "startup-card" {
		t.Fatalf("startup selector was not patched: %#v", notes)
	}
	if !rt.RequestFreshSnapshot(time.Second) {
		t.Fatal("renderer did not respond")
	}
	if requests.Load() > 3 || released.Load() != 0 {
		t.Fatalf("repeated menu caused snapshot loop or released input: requests=%d released=%d", requests.Load(), released.Load())
	}
	if _, err := rt.consumeTerminalInteraction(notes[1].Interaction.ID, "opt_1", "startup-card"); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.consumeTerminalInteraction(notes[1].Interaction.ID, "opt_1", "startup-card"); err == nil {
		t.Fatal("accepted a repeated selection")
	}
	// A repaint or a failed selection may leave identical menu text. Its new
	// callback ID must still reach the card instead of being content-deduplicated.
	rt.HandleOutput([]byte("repaint"))
	rt.SetVisibleSnapshotWithSource(nativeModelMenu, aidenReadySource)
	deadline = time.Now().Add(time.Second)
	for notifier.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	notes = notifier.notes()
	if len(notes) != 3 || notes[2].Interaction == nil || notes[2].Interaction.ID == notes[1].Interaction.ID {
		t.Fatalf("identical menu did not refresh its callback: %#v", notes)
	}
}

func TestTerminalMenuProbeDoesNotCompleteTask(t *testing.T) {
	notifier := &recordingNotifier{}
	rt := &RuntimeSession{manager: NewManager(nil, nil, WithNotifier(notifier)), session: Session{ID: "s", Live: true, Status: StatusRunning, NotifyOnWaiting: true, LastMode: SessionModeAgent, LastAgentKind: "aiden"}, notifyVersion: 1}
	rt.SetVisibleSnapshotWithSource("The following options are available:\n1. First\n2. Second", aidenReadySource)
	time.Sleep(20 * time.Millisecond)
	if len(notifier.notes()) != 0 {
		t.Fatal("ordinary output triggered notification")
	}
	if rt.Snapshot().Status != StatusRunning {
		t.Fatal("ordinary output completed task")
	}
	rt.Close()
}

func TestTerminalMenuConfirmsOnlyRenderedSelection(t *testing.T) {
	for _, stale := range []bool{false, true} {
		term := &recordingTerminal{readCh: make(chan []byte)}
		rt := &RuntimeSession{manager: NewManager(nil, nil), terminal: term, session: Session{ID: "menu-input", Live: true, Status: StatusWaiting, LastMode: SessionModeAgent, LastAgentKind: "aiden"}, visibleSnapshot: nativeModelMenu, visibleSnapshotSource: aidenReadySource, visibleSnapshotVersion: 1}
		subscriber, cancel := rt.Subscribe()
		go func() {
			for event := range subscriber {
				if event.Type != RuntimeEventSnapshotRequest {
					continue
				}
				screen := nativeModelMenu
				if !stale && strings.Contains(term.writes(), "\x1b[B") {
					screen = strings.ReplaceAll(screen, "❯  1.", "   1.")
					screen = strings.ReplaceAll(screen, "   2.", "❯  2.")
				}
				rt.SetVisibleSnapshotResponseFrom(screen, strings.Replace(aidenReadySource, "headless:", "browser:", 1), event.RequestID, subscriber)
			}
		}()
		option := DetectTerminalMenu(nativeModelMenu, "s", 0, 0).Options[1]
		err := rt.submitTerminalMenuSelection(option)
		if stale && err == nil {
			t.Fatal("confirmed without rendered focus")
		}
		if !stale && err != nil {
			t.Fatal(err)
		}
		want := "\x1b[B"
		if !stale {
			want += "\r"
		}
		if term.writes() != want {
			t.Fatalf("keys=%q want=%q", term.writes(), want)
		}
		rt.Close()
		cancel()
	}
}

func TestTerminalMenuPausesStartupDeadline(t *testing.T) {
	term := &recordingTerminal{readCh: make(chan []byte)}
	rt := &RuntimeSession{manager: NewManager(nil, nil), terminal: term, session: Session{ID: "menu-wait", Live: true, HistorySize: 1, LastMode: SessionModeAgent, LastAgentKind: "aiden"}}
	subscriber, cancel := rt.Subscribe()
	defer cancel()
	defer rt.Close()
	start := time.Now()
	go func() {
		for event := range subscriber {
			if event.Type != RuntimeEventSnapshotRequest {
				continue
			}
			screen := nativeModelMenu
			if time.Since(start) > 1200*time.Millisecond {
				screen = aidenReadySnapshot
			}
			rt.SetVisibleSnapshotResponseFrom(screen, strings.Replace(aidenReadySource, "headless:", "browser:", 1), event.RequestID, subscriber)
		}
	}()
	if !rt.waitForRestartedAgentComposer("aiden", 0, 0, 600*time.Millisecond) {
		t.Fatal("model selection exhausted startup deadline")
	}
}

func TestTerminalMenuLongListAndPendingTask(t *testing.T) {
	screen := "Select Model\n"
	for i := 1; i <= 40; i++ {
		prefix := "  "
		if i == 25 {
			prefix = "❯ "
		}
		screen += fmt.Sprintf("%s%d. model-%d\n", prefix, i, i)
	}
	screen += "↑↓ Navigate • Enter Confirm • Esc Cancel"
	menu := DetectTerminalMenu(screen, "s", 1, 1)
	if menu == nil || menu.Title != "Select Model" || len(menu.Options) > 24 {
		t.Fatalf("long menu=%#v", menu)
	}
	rt := &RuntimeSession{manager: NewManager(nil, nil), session: Session{ID: "s", Live: true, Status: StatusWaiting, LastMode: SessionModeAgent, LastAgentKind: "aiden"}, terminalMenuActive: true, visibleSnapshot: aidenReadySnapshot, visibleSnapshotSource: aidenReadySource, visibleSnapshotVersion: 1, hookCompletedCurrentRound: true, hookLastAssistantMessage: "actual final answer"}
	note, _, ok, _ := rt.waitingNotificationCandidateLocked()
	if !ok || note.Content != "actual final answer" || rt.terminalMenuActive {
		t.Fatalf("menu hid real completion: %#v", note)
	}
	rt.visibleSnapshot = nativeModelMenu
	if !rt.ShouldQueueInputWhileRunning() {
		t.Fatal("input allowed into a selector")
	}
	rt.Close()
}
