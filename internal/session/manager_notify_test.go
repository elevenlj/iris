package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
)

func setTrustedLegacyRoundFixture(rt *RuntimeSession, baseline string, input string, current string) {
	rt.SetVisibleSnapshot(baseline)
	rt.MarkInputActivity(input)
	rt.SetVisibleSnapshot(current)
}

func TestWaitingNotificationRequiresReplyContent(t *testing.T) {
	rt := &RuntimeSession{
		manager: NewManager(nil, nil),
		session: Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true},
	}
	rt.MarkInputActivity("今天天气怎么样\r")
	rt.SetVisibleSnapshot("> 今天天气怎么样")

	rt.mu.Lock()
	_, _, ok := rt.waitingNotificationLocked()
	rt.mu.Unlock()
	if ok {
		t.Fatalf("notification should not be ready when the current round only contains user input")
	}
}

func TestCompleteAgentTurnUsesAuthenticatedHookWithIdleFallback(t *testing.T) {
	manager := NewManager(nil, nil)
	rt := &RuntimeSession{
		manager: manager,
		session: Session{
			ID:                     "sess-1",
			Status:                 StatusRunning,
			Live:                   true,
			RecoveryKey:            "hook-token",
			LastMode:               SessionModeAgent,
			LastAgentKind:          "codex",
			LastAgentResumeCommand: "codex resume --last --dangerously-bypass-approvals-and-sandbox",
		},
	}
	manager.sessions[rt.session.ID] = rt

	if _, accepted, err := manager.CompleteAgentTurn(context.Background(), rt.session.ID, "wrong-token", "", ""); err == nil || accepted {
		t.Fatalf("invalid token should be rejected, accepted=%v err=%v", accepted, err)
	}
	got, accepted, err := manager.CompleteAgentTurn(context.Background(), rt.session.ID, "hook-token", "019f5153-6e7f-7742-9f61-3ffe1530d61c", "")
	if err != nil || !accepted {
		t.Fatalf("authenticated hook result accepted=%v err=%v", accepted, err)
	}
	if got.Status != StatusWaiting || rt.Snapshot().Status != StatusWaiting || !rt.agentTurnHookVerified {
		t.Fatalf("hook should complete the Codex turn, session=%#v verified=%v", got, rt.agentTurnHookVerified)
	}
	if !strings.Contains(got.LastAgentResumeCommand, "019f5153-6e7f-7742-9f61-3ffe1530d61c") || strings.Contains(got.LastAgentResumeCommand, "--last") {
		t.Fatalf("hook should pin Codex recovery to the reported session id, got %q", got.LastAgentResumeCommand)
	}

	rt.HandleOutput([]byte("completed-turn TUI repaint"))
	if got := rt.Snapshot().Status; got != StatusWaiting {
		t.Fatalf("completed-turn TUI repaint must not reopen the round, got %q", got)
	}
	rt.mu.Lock()
	timer := rt.notifyStableTimer
	rt.mu.Unlock()
	if timer != nil {
		t.Fatal("completed-turn TUI repaint must not re-arm the idle completion fallback")
	}

	rt.mu.Lock()
	rt.hookCompletionTipClaimed = true
	rt.mu.Unlock()
	if _, accepted, err := manager.CompleteAgentTurn(context.Background(), rt.session.ID, "hook-token", "", ""); err != nil || accepted {
		t.Fatalf("same-round repeated hook completion accepted=%v err=%v", accepted, err)
	}
	rt.mu.Lock()
	claimedAfterRepeatedHook := rt.hookCompletionTipClaimed
	rt.mu.Unlock()
	if !claimedAfterRepeatedHook {
		t.Fatal("a repeated Hook in the same round must not reopen the completion tip")
	}

	rt.MarkStructuredInputActivity("next round")
	rt.HandleOutput([]byte("next-round output"))
	if got := rt.Snapshot().Status; got != StatusRunning {
		t.Fatalf("new input should open the next round, got %q", got)
	}
	rt.mu.Lock()
	timer = rt.notifyStableTimer
	rt.mu.Unlock()
	if timer == nil {
		t.Fatal("new round should retain the idle completion fallback")
	}
}

func TestHookCompletionTipIsClaimedOncePerRound(t *testing.T) {
	notifier := &recordingNotifier{}
	manager := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager: manager,
		session: Session{
			ID:                     "sess-1",
			Name:                   "A",
			Status:                 StatusRunning,
			Live:                   true,
			NotifyOnWaiting:        true,
			RecoveryKey:            "hook-token",
			LastMode:               SessionModeAgent,
			LastAgentKind:          "codex",
			LastAgentResumeCommand: "codex resume --last",
		},
		subscribers: make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	manager.sessions[rt.session.ID] = rt
	rt.SetVisibleSnapshot("old answer\n›")
	rt.MarkInputActivity("fix bug\r")
	rt.SetVisibleSnapshot("› fix bug\n• first answer")
	rt.mu.Lock()
	rt.lastNotifiedMessageID = "card-1"
	rt.lastNotifiedContent = "上一轮误识别出来的很长历史内容，不能覆盖 Hook 最终回复"
	rt.notificationRunning = true
	rt.mu.Unlock()

	if _, accepted, err := manager.CompleteAgentTurn(context.Background(), rt.session.ID, "hook-token", "", "本轮最终回复"); err != nil || !accepted {
		t.Fatalf("hook completion accepted=%v err=%v", accepted, err)
	}
	first := waitForNotifierNotes(t, notifier, 1)
	if first[0].SuppressUpdateTip {
		t.Fatal("the Hook completion write should be allowed to send the first completion tip")
	}
	if first[0].Content != "本轮最终回复" || first[0].SnapshotSource != "codex_hook:last_assistant_message" {
		t.Fatalf("Hook completion should prefer its final assistant message, got %#v", first[0])
	}
	// A browser reload can provide a different valid rendering of the same
	// completed round. The card may still be patched, but completion is not a
	// new event and must not emit another tip.
	rt.SetVisibleSnapshot("› fix bug\n• first answer\n• final detail")
	rt.mu.Lock()
	version := rt.notifyVersion
	rt.mu.Unlock()
	rt.notifyIfStillWaitingWithMode(version, true, false)
	if notes := notifier.notes(); len(notes) != 1 {
		t.Fatalf("same-round renderer refresh should not rewrite authoritative Hook content, got %#v", notes)
	}

	rt.MarkInputActivity("next round\r")
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.hookCompletedCurrentRound || rt.hookCompletionTipClaimed || rt.hookLastAssistantMessage != "" {
		t.Fatal("new input should reset Hook completion-tip idempotency")
	}
}

func TestLateHookAssistantMessageCorrectsIdleFallback(t *testing.T) {
	notifier := &recordingNotifier{}
	manager := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager: manager,
		session: Session{
			ID:              "sess-1",
			Name:            "A",
			Status:          StatusWaiting,
			Live:            true,
			NotifyOnWaiting: true,
			RecoveryKey:     "hook-token",
			LastMode:        SessionModeAgent,
			LastAgentKind:   "codex",
		},
		lastInputText:            "修复问题",
		lastNotifiedMessageID:    "card-1",
		lastNotifiedContent:      "历史内容\n历史内容\n错误兜底",
		lastNotifiedRoundHash:    notifyContentHash("历史内容\n历史内容\n错误兜底"),
		notificationRunning:      false,
		snapshotAtRoundStartSet:  true,
		snapshotAtRoundVersion:   2,
		visibleSnapshotVersion:   1,
		notifyVersion:            3,
		hookCompletionTipClaimed: true,
	}
	manager.sessions[rt.session.ID] = rt

	if _, accepted, err := manager.CompleteAgentTurn(context.Background(), rt.session.ID, "hook-token", "", "本轮权威最终回复"); err != nil || !accepted {
		t.Fatalf("late Hook completion accepted=%v err=%v", accepted, err)
	}
	notes := waitForNotifierNotes(t, notifier, 1)
	if notes[0].Content != "本轮权威最终回复" || notes[0].MessageID != "card-1" {
		t.Fatalf("late Hook should replace the fallback content, got %#v", notes[0])
	}
	if !notes[0].SuppressUpdateTip {
		t.Fatal("late Hook correction must not announce completion again")
	}
	if _, accepted, err := manager.CompleteAgentTurn(context.Background(), rt.session.ID, "hook-token", "", "本轮权威最终回复"); err != nil || accepted {
		t.Fatalf("duplicate late Hook accepted=%v err=%v", accepted, err)
	}
	if notes := notifier.notes(); len(notes) != 1 {
		t.Fatalf("duplicate late Hook must not rewrite the card, got %#v", notes)
	}
}

func TestAgentIdleFallbackDoesNotRecompleteWaitingTurn(t *testing.T) {
	manager := NewManager(nil, nil)
	rt := &RuntimeSession{
		manager: manager,
		session: Session{
			ID:            "sess-1",
			Status:        StatusWaiting,
			Live:          true,
			LastMode:      SessionModeAgent,
			LastAgentKind: "codex",
		},
		agentTurnHookVerified: true,
		notifyVersion:         7,
		stateVersion:          9,
	}
	rt.mu.Lock()
	rt.resetAgentIdleCompletionTimerLocked()
	timer := rt.notifyStableTimer
	rt.mu.Unlock()
	if timer != nil {
		t.Fatal("completed Agent turn must not schedule another completion")
	}
	rt.notifyAfterStable(9)
	if got := rt.Snapshot(); got.Status != StatusWaiting {
		t.Fatalf("completed Agent turn changed status: %#v", got)
	}
	rt.mu.Lock()
	if rt.notifyVersion != 7 {
		t.Fatalf("completed Agent turn changed notify version to %d", rt.notifyVersion)
	}
	rt.mu.Unlock()
}

func TestCodexModelMenuImmediatelyWaitsWithoutVerifiedHook(t *testing.T) {
	manager := NewManager(nil, nil, WithWaitingTransitionDelays(20*time.Millisecond, 20*time.Millisecond))
	rt := &RuntimeSession{
		manager: manager,
		session: Session{
			ID:            "sess-1",
			Status:        StatusRunning,
			Live:          true,
			LastMode:      SessionModeAgent,
			LastAgentKind: "codex",
		},
		lastInputText: "/model",
	}
	rt.SetVisibleSnapshot(strings.Join([]string{
		"Select Model and Effort",
		"› 1. gpt-5.6-terra (current)   Frontier model",
		"  2. gpt-5.5                 Strong model",
		"Press enter to confirm or esc to go back",
	}, "\n"))

	if got := rt.Snapshot().Status; got != StatusWaiting {
		t.Fatalf("model menu should immediately transition to waiting, got %q", got)
	}
	rt.mu.Lock()
	timer := rt.notifyStableTimer
	rt.mu.Unlock()
	if timer != nil {
		t.Fatal("recognized Codex model menu must not wait for the idle timer")
	}
}

func TestWaitingNotificationDedupesButRepushesFullRoundWhenMoreOutputArrives(t *testing.T) {
	rt := &RuntimeSession{
		manager: NewManager(nil, nil),
		session: Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true},
	}
	setTrustedLegacyRoundFixture(rt, "›", "今天天气怎么样\r", "> 今天天气怎么样\n• 你想查哪个城市的天气？")

	rt.mu.Lock()
	first, firstHash, ok := rt.waitingNotificationLocked()
	if !ok {
		t.Fatal("expected first full-round notification to be ready")
	}
	rt.lastNotifiedRoundHash = firstHash
	_, _, duplicateOK := rt.waitingNotificationLocked()
	rt.mu.Unlock()
	if duplicateOK {
		t.Fatal("same full-round notification should be deduped")
	}
	if first.Content != "• 你想查哪个城市的天气？" {
		t.Fatalf("first content = %q", first.Content)
	}

	rt.HandleOutput([]byte("more output"))
	rt.SetVisibleSnapshot("> 今天天气怎么样\n• 你想查哪个城市的天气？\n• 成都今天晴转多云。")

	rt.mu.Lock()
	second, secondHash, ok := rt.waitingNotificationLocked()
	rt.mu.Unlock()
	if !ok {
		t.Fatal("expected updated full-round notification after more output")
	}
	if secondHash == firstHash {
		t.Fatal("updated full-round notification should have a different hash")
	}
	want := "• 你想查哪个城市的天气？\n• 成都今天晴转多云。"
	if second.Content != want {
		t.Fatalf("second content = %q, want %q", second.Content, want)
	}
}

func TestWaitingNotificationKeepsLongerContentWhenSnapshotRegressesToPrefix(t *testing.T) {
	previous := strings.Join([]string{
		"• 只下你需要的人像数字人版本：约 660MB。",
		"如果用官方全量下载：约 2.14GB。",
		"所以你流量紧张的话，按 700MB 预算准备就够，别下载动物模型和不需要的文件。",
	}, "\n")
	rt := &RuntimeSession{
		manager:                     NewManager(nil, nil),
		session:                     Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true},
		lastInputText:               "需要下载哪些东西？",
		lastNotifiedMessageID:       "msg-1",
		lastNotifiedContent:         previous,
		lastNotifiedRoundHash:       notifyContentHash(previous),
		snapshotAtRoundStart:        "> 需要下载哪些东西？",
		snapshotAtRoundVersion:      1,
		snapshotAtRoundStartSet:     true,
		visibleSnapshot:             "> 需要下载哪些东西？\n• 只下你需要的人像数字人版本：约 660MB。\n如果用官方全量下载：约 2.14GB。\n›",
		visibleSnapshotVersion:      2,
		lastNotifiedVisibleSnapshot: "> 需要下载哪些东西？\n" + previous,
	}

	rt.mu.Lock()
	_, _, ok, reason := rt.waitingNotificationCandidateLocked()
	rt.mu.Unlock()
	if ok {
		t.Fatal("shorter prefix content should not be sent as an update")
	}
	if reason != "duplicate_hash" {
		t.Fatalf("reason = %q, want duplicate_hash", reason)
	}
}

func TestWaitingNotificationFallbackRejectsPromptOnlyContent(t *testing.T) {
	rt := &RuntimeSession{
		manager:                 NewManager(nil, nil),
		session:                 Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true},
		lastInputText:           "继续",
		snapshotAtRoundStart:    "> 继续",
		snapshotAtRoundVersion:  1,
		snapshotAtRoundStartSet: true,
		visibleSnapshot:         "> 继续\n›",
		visibleSnapshotVersion:  2,
	}

	rt.mu.Lock()
	_, _, ok, reason := rt.fallbackWaitingNotificationCandidateLocked()
	rt.mu.Unlock()
	if ok {
		t.Fatal("prompt-only fallback should not be sent")
	}
	if reason != "needs_more_snapshot" {
		t.Fatalf("reason = %q, want needs_more_snapshot", reason)
	}
}

func TestWaitingNotificationWaitsForSnapshotAfterCurrentInput(t *testing.T) {
	rt := &RuntimeSession{
		manager: NewManager(nil, nil),
		session: Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true},
	}
	rt.SetVisibleSnapshot("eleven ~ > ll\ntotal 8\n-rw-r--r-- file.txt\neleven ~ >")
	rt.MarkInputActivity("cdx\r")
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	_, _, ok, reason := rt.waitingNotificationCandidateLocked()
	rt.mu.Unlock()
	if ok {
		t.Fatal("stale snapshot from the previous round should not be ready")
	}
	if reason != "stale_visible_snapshot" {
		t.Fatalf("reason = %q, want stale_visible_snapshot", reason)
	}

	rt.SetVisibleSnapshot("eleven ~ > ll\ntotal 8\n-rw-r--r-- file.txt\neleven ~ >")
	rt.mu.Lock()
	_, _, ok, reason = rt.waitingNotificationCandidateLocked()
	rt.mu.Unlock()
	if ok {
		t.Fatal("fresh snapshot event with unchanged previous-round content should not be ready")
	}
	if reason != "stale_visible_snapshot" {
		t.Fatalf("reason after unchanged snapshot = %q, want stale_visible_snapshot", reason)
	}

	rt.SetVisibleSnapshot("eleven ~ > cdx\nzsh: command not found: cdx\neleven ~ >")
	rt.mu.Lock()
	n, _, ok := rt.waitingNotificationLocked()
	rt.mu.Unlock()
	if !ok {
		t.Fatal("expected notification after a fresh snapshot for the current input")
	}
	want := "zsh: command not found: cdx\neleven ~ >"
	if n.Content != want {
		t.Fatalf("content = %q, want %q", n.Content, want)
	}
}

func TestWaitingNotificationWaitsWhenFreshVisibleSnapshotIsMissing(t *testing.T) {
	rt := &RuntimeSession{
		manager: NewManager(nil, nil),
		session: Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true},
	}
	rt.SetVisibleSnapshot("old TUI screen\nold answer")
	rt.MarkInputActivity("new hidden tui input\r")
	rt.HandleOutput([]byte("current answer from pty\n"))
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	_, _, ok, reason := rt.waitingNotificationCandidateLocked()
	rt.mu.Unlock()
	if ok {
		t.Fatalf("raw round reply should not create notification content")
	}
	if reason != "stale_visible_snapshot" {
		t.Fatalf("reason = %q, want stale_visible_snapshot", reason)
	}
}

func TestNotifyAfterStableDoesNotSendRoundReplyWhenSnapshotDoesNotShowInput(t *testing.T) {
	notifier := &recordingNotifier{}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager: m,
		session: Session{ID: "sess-1", Name: "A", Status: StatusRunning, Live: true, NotifyOnWaiting: true},
	}
	rt.SetVisibleSnapshot("old TUI screen\nold answer")
	rt.MarkInputActivity("hidden tui input\r")
	rt.HandleOutput([]byte("current answer from pty\n"))
	rt.mu.Lock()
	version := rt.stateVersion
	rt.mu.Unlock()

	rt.notifyAfterStable(version)

	notes := notifier.notes()
	if len(notes) != 0 {
		t.Fatalf("raw round reply should not be sent as notification content, got %#v", notes)
	}
}

func TestNotifyIfStillWaitingDoesNotUseTailFallbackForUnanchoredInputOnlySnapshot(t *testing.T) {
	notifier := &recordingNotifier{}
	m := NewManager(nil, nil, WithNotifier(notifier), WithWaitingTransitionDelays(20*time.Millisecond, 20*time.Millisecond), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager: m,
		session: Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
	}
	rt.SetVisibleSnapshot("eleven ~/project >")
	rt.MarkInputActivity("cdx_d\r")
	rt.SetVisibleSnapshot("eleven ~/project > cdx_d")
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	rt.lastNotifiedMessageID = "running-card"
	rt.lastNotifiedContent = RunningNotificationPlaceholder
	rt.notificationRunning = true
	version := rt.notifyVersion
	_, _, ok, reason := rt.waitingNotificationCandidateLocked()
	rt.mu.Unlock()
	if ok {
		t.Fatal("input-only snapshot should not be treated as ideal notification content")
	}
	if reason != "needs_more_snapshot" {
		t.Fatalf("reason = %q, want needs_more_snapshot", reason)
	}

	rt.notifyIfStillWaiting(version)

	notes := notifier.notes()
	if len(notes) != 1 {
		t.Fatalf("input-only completed round should close the running card, got %#v", notes)
	}
	if notes[0].MessageID != "running-card" || notes[0].Running || notes[0].Content != EmptyNotificationPlaceholder {
		t.Fatalf("input-only completed round must not publish the input as a reply, got %#v", notes[0])
	}
	if rt.notificationRunning {
		t.Fatal("input-only completed round should clear the runtime running-card state")
	}
}

func TestNotifyIfStillWaitingClosesRunningCardWhenFilteredContentIsEmpty(t *testing.T) {
	notifier := &recordingNotifier{messageID: "running-card"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager:               m,
		session:               Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastNotifiedMessageID: "running-card",
		lastNotifiedContent:   RunningNotificationPlaceholder,
		notificationRunning:   true,
		lastInputText:         "codex --dangerously-bypass-approvals-and-sandbox",
		notifyVersion:         3,
	}

	rt.notifyIfStillWaitingWithMode(3, true, false)

	notes := notifier.notes()
	if len(notes) != 1 {
		t.Fatalf("empty completed round should close the existing running card, got %#v", notes)
	}
	if notes[0].MessageID != "running-card" || notes[0].Running {
		t.Fatalf("empty completed round should patch the current card to waiting, got %#v", notes[0])
	}
	if notes[0].Content != EmptyNotificationPlaceholder {
		t.Fatalf("empty completed round content = %q, want %q", notes[0].Content, EmptyNotificationPlaceholder)
	}
	if rt.notificationRunning {
		t.Fatal("runtime should clear the running-card state after the empty completion patch")
	}
}

func TestTailFallbackDoesNotPublishHistoryBeforeQueuedInputStarts(t *testing.T) {
	rt := &RuntimeSession{
		manager:         NewManager(nil, nil),
		session:         Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastInputText:   "hello",
		visibleSnapshot: "› previous question\n• previous answer that must not become the hello reply",
	}

	rt.mu.Lock()
	_, _, ok := rt.fallbackTailWaitingNotificationCandidateLocked()
	rt.mu.Unlock()
	if ok {
		t.Fatal("tail fallback must wait until the queued input has terminal evidence")
	}
}

func TestTailFallbackAllowsStartedRoundWhenInputAnchorHasRolledOut(t *testing.T) {
	rt := &RuntimeSession{
		manager:         NewManager(nil, nil),
		session:         Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastInputText:   "a long request whose input anchor rolled out",
		roundReply:      []byte("current round produced terminal output"),
		visibleSnapshot: "• current reply tail one\n• current reply tail two",
	}

	rt.mu.Lock()
	n, _, ok := rt.fallbackTailWaitingNotificationCandidateLocked()
	rt.mu.Unlock()
	if !ok || n.Content != rt.visibleSnapshot {
		t.Fatalf("a started round with a rolled-out input anchor should use the visible tail, got %#v", n)
	}
}

func TestWaitingNotificationUsesFreshVisibleListInsteadOfRawRoundReply(t *testing.T) {
	SetLarkNotifyMaxLines(4)
	t.Cleanup(func() { SetLarkNotifyMaxLines(defaultMaxLarkTextLines) })
	rt := &RuntimeSession{
		manager: NewManager(nil, nil),
		session: Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true},
	}
	rt.SetVisibleSnapshot("menu command")
	rt.MarkInputActivity("menu command\r")
	rt.HandleOutput([]byte("Available options:1.Create session2.Attach session3.Quit\n"))
	rt.SetVisibleSnapshot(strings.Join([]string{
		"menu command",
		"Available options:",
		"1. Create session",
		"2. Attach session",
		"3. Quit",
	}, "\n"))
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	n, _, ok, reason := rt.waitingNotificationCandidateLocked()
	rt.mu.Unlock()
	if !ok {
		t.Fatalf("expected visible list notification, reason=%s", reason)
	}
	want := strings.Join([]string{
		"Available options:",
		"1. Create session",
		"2. Attach session",
		"3. Quit",
	}, "\n")
	if n.Content != want {
		t.Fatalf("notification should preserve visible list formatting:\n%q\nwant:\n%q", n.Content, want)
	}
}

func TestWaitingNotificationKeepsCodexModelMenusFromVisibleSnapshot(t *testing.T) {
	SetLarkNotifyMaxLines(5)
	t.Cleanup(func() { SetLarkNotifyMaxLines(defaultMaxLarkTextLines) })
	rt := &RuntimeSession{
		manager: NewManager(nil, nil),
		session: Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true},
	}
	start := strings.Join([]string{
		"╭────────────────────────────╮",
		"│ >_ OpenAI Codex (v0.130.0) │",
		"│ model: gpt-5.5 medium fast │",
		"│ directory: ~/project       │",
		"╰────────────────────────────╯",
	}, "\n")
	modelMenu := strings.Join([]string{
		start,
		"Select Model and Effort",
		"Access legacy models by running codex -m <model_name> or in your config.toml",
		"› 1. gpt-5.5 (current)   Frontier model for complex coding, research, and real-world work.",
		"  2. gpt-5.4             Strong model for everyday coding.",
		"Press enter to confirm or esc to go back",
	}, "\n")
	rt.SetVisibleSnapshot(start)
	rt.MarkInputActivity("/model\r")
	rt.HandleOutput([]byte("/model choose what model and reasoning effort to useSelect Model and EffortAccess legacy models by running codex -m <model_name> or in your config.toml› 1. gpt-5.5 (current) Frontier model2.gpt-5.4Strong model"))
	rt.SetVisibleSnapshot(modelMenu)
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	n, _, ok, reason := rt.waitingNotificationCandidateLocked()
	rt.mu.Unlock()
	if !ok {
		t.Fatalf("expected model menu notification, reason=%s", reason)
	}
	wantModel := strings.Join([]string{
		"Select Model and Effort",
		"Access legacy models by running codex -m <model_name> or in your config.toml",
		"› 1. gpt-5.5 (current)   Frontier model for complex coding, research, and real-world work.",
		"  2. gpt-5.4             Strong model for everyday coding.",
		"Press enter to confirm or esc to go back",
	}, "\n")
	if n.Content != wantModel {
		t.Fatalf("model menu should preserve visible formatting:\n%q\nwant:\n%q", n.Content, wantModel)
	}
	if n.Interaction == nil || n.Interaction.Kind != TerminalInteractionCodexModel || len(n.Interaction.Options) != 2 {
		t.Fatalf("model menu should include a structured interaction, got %#v", n.Interaction)
	}
	if n.AgentContext == nil || n.AgentContext.Directory != "~/project" || n.AgentContext.Model != "gpt-5.5" || n.AgentContext.Reasoning != "Medium" {
		t.Fatalf("model menu should include the Codex card context, got %#v", n.AgentContext)
	}

	SetLarkNotifyMaxLines(11)
	reasoningMenu := strings.Join([]string{
		start,
		"Select Reasoning Level for gpt-5.5",
		"1. Low                  Fast responses with lighter reasoning",
		"2. Medium (default)     Balances speed and reasoning depth for everyday tasks",
		"3. High                 Greater reasoning depth for complex problems",
		"› 4. Extra high (current)  Extra high reasoning depth for complex problems",
		"Press enter to confirm or esc to go back",
	}, "\n")
	rt.MarkInputActivity("1\r")
	rt.HandleOutput([]byte("1Select Reasoning Level for gpt-5.51.LowFast responses with lighter reasoning2.Medium(default)Balances speed and reasoning depth for everyday tasks3.HighGreater reasoning depth for complex problems› 4. Extra high (current) Extra high reasoning depthPress enter to confirm or esc to go back"))
	rt.SetVisibleSnapshot(reasoningMenu)
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	n, _, ok, reason = rt.waitingNotificationCandidateLocked()
	rt.mu.Unlock()
	if !ok {
		t.Fatalf("expected reasoning menu notification, reason=%s", reason)
	}
	wantReasoning := strings.Join([]string{
		"Select Reasoning Level for gpt-5.5",
		"1. Low                  Fast responses with lighter reasoning",
		"2. Medium (default)     Balances speed and reasoning depth for everyday tasks",
		"3. High                 Greater reasoning depth for complex problems",
		"› 4. Extra high (current)  Extra high reasoning depth for complex problems",
		"Press enter to confirm or esc to go back",
	}, "\n")
	if n.Content != wantReasoning {
		t.Fatalf("reasoning menu should preserve visible formatting:\n%q\nwant:\n%q", n.Content, wantReasoning)
	}
	if n.Interaction == nil || n.Interaction.Kind != TerminalInteractionCodexReasoning || len(n.Interaction.Options) != 4 {
		t.Fatalf("reasoning menu should include a structured interaction, got %#v", n.Interaction)
	}
}

func TestNotifyEndToEndRequestsFrontendSnapshotWhenNoBrowserIsOpen(t *testing.T) {
	notifier := &recordingNotifier{}
	launcher := &recordingLauncher{}
	browserRequested := make(chan struct{})
	var browserRequestedOnce sync.Once
	var m *Manager
	m = NewManager(
		nil,
		launcher,
		WithNotifier(notifier),
		WithWaitingTransitionDelays(20*time.Millisecond, 20*time.Millisecond),
		WithNotificationUpdateCoalesce(0),
		WithBrowserNeeded(func(sessionID string) {
			browserRequestedOnce.Do(func() { close(browserRequested) })
			if rt, ok := m.GetRuntime(sessionID); ok {
				rt.mu.Lock()
				currentInput := rt.lastInputText
				rt.mu.Unlock()
				if currentInput != "echo frontend-snapshot" {
					rt.SetVisibleSnapshot("eleven ~ >")
					return
				}
				rt.SetVisibleSnapshot("eleven ~ >  echo frontend-snapshot\nfrontend ok\neleven ~ >")
			}
		}),
	)
	sess, err := m.CreateSession(context.Background(), "no-browser")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.UpdateNotifyOnWaiting(context.Background(), sess.ID, true); err != nil {
		t.Fatal(err)
	}
	rt, ok := m.GetRuntime(sess.ID)
	if !ok {
		t.Fatal("runtime session missing")
	}

	if err := SubmitStructuredInput(rt, "echo frontend-snapshot"); err != nil {
		t.Fatal(err)
	}
	launcher.terminals[0].readCh <- []byte("frontend ok\r\neleven ~ > ")

	notes := waitForNotifierNotes(t, notifier, 1)
	select {
	case <-browserRequested:
	default:
		t.Fatal("frontend snapshot should be requested when no browser is open")
	}
	if len(notes) == 0 {
		t.Fatal("expected notification from post-input frontend snapshot")
	}
	if notes[0].Content != "frontend ok\neleven ~ >" {
		t.Fatalf("notification should come from frontend snapshot, got %q", notes[0].Content)
	}
}

func TestNotifyStableDelayFastForPlainOutputAndConservativeForCodex(t *testing.T) {
	m := NewManager(nil, nil, WithWaitingTransitionDelays(120*time.Millisecond, 450*time.Millisecond))
	rt := &RuntimeSession{
		manager: m,
		session: Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true},
	}
	setTrustedLegacyRoundFixture(rt, "$", "echo hello\r", "$ echo hello\nhello\n$")
	rt.mu.Lock()
	fast := rt.notifyStableDelayLocked()
	rt.mu.Unlock()
	if fast != 120*time.Millisecond {
		t.Fatalf("plain output stable delay = %v, want %v", fast, 120*time.Millisecond)
	}

	rt.MarkInputActivity("今天天气怎么样\r")
	rt.SetVisibleSnapshot("> 今天天气怎么样\n• Working (1s • esc to interrupt)")
	rt.mu.Lock()
	conservative := rt.notifyStableDelayLocked()
	rt.mu.Unlock()
	if conservative != 450*time.Millisecond {
		t.Fatalf("codex output stable delay = %v, want %v", conservative, 450*time.Millisecond)
	}
}

func TestNotifyAfterStableTransitionsWaitingAndSends(t *testing.T) {
	notifier := &recordingNotifier{}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager: m,
		session: Session{ID: "sess-1", Name: "A", Status: StatusRunning, Live: true, NotifyOnWaiting: true},
	}
	setTrustedLegacyRoundFixture(rt, "$", "echo hello\r", "$ echo hello\nhello\n$")
	version := rt.stateVersion

	rt.notifyAfterStable(version)
	if got := rt.Snapshot().Status; got != StatusWaiting {
		t.Fatalf("stable output should transition to waiting, got %s", got)
	}
	notes := notifier.notes()
	if len(notes) != 1 {
		t.Fatalf("expected stable notification, got %#v", notes)
	}
	if notes[0].Content != "hello\n$" {
		t.Fatalf("unexpected stable notification content: %q", notes[0].Content)
	}
}

func TestNotifyIfStillWaitingUpdatesSameRoundMessage(t *testing.T) {
	notifier := &recordingNotifier{messageID: "msg-1"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager: m,
		session: Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
	}
	setTrustedLegacyRoundFixture(rt, "›", "今天天气怎么样\r", "> 今天天气怎么样\n• 你想查哪个城市的天气？")
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	version := rt.notifyVersion
	rt.mu.Unlock()

	rt.notifyIfStillWaiting(version)
	rt.HandleOutput([]byte("more output"))
	rt.SetVisibleSnapshot("> 今天天气怎么样\n• 你想查哪个城市的天气？\n• 成都今天晴转多云。")
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	version = rt.notifyVersion
	rt.mu.Unlock()
	rt.notifyIfStillWaiting(version)

	notes := notifier.notes()
	if len(notes) != 2 {
		t.Fatalf("expected create and update notifications, got %#v", notes)
	}
	if notes[0].MessageID != "" || notes[0].UpdateNo != 0 {
		t.Fatalf("first notification should create a new message, got %#v", notes[0])
	}
	if notes[1].MessageID != "msg-1" || notes[1].UpdateNo != 1 || notes[1].SuppressUpdateTip {
		t.Fatalf("second notification should update msg-1 with update marker 1, got %#v", notes[1])
	}
	if notes[1].Running {
		t.Fatalf("waiting notification update should clear running title state, got %#v", notes[1])
	}
	if rt.lastNotifiedMessageID != "msg-1" {
		t.Fatalf("runtime should keep updated message id, got %q", rt.lastNotifiedMessageID)
	}

	rt.MarkInputActivity("echo next\r")
	rt.SetVisibleSnapshot("$ echo next\nnext\n$")
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	version = rt.notifyVersion
	rt.mu.Unlock()
	rt.notifyIfStillWaiting(version)
	notes = notifier.notes()
	if len(notes) != 3 {
		t.Fatalf("expected next round notification, got %#v", notes)
	}
	if notes[2].MessageID != "" || notes[2].UpdateNo != 0 {
		t.Fatalf("new round should create a new message, got %#v", notes[2])
	}
}

func TestNotifyPreservesCreatedMessageIDWhenSameRoundAdvancesDuringSend(t *testing.T) {
	notifier := &advancingNotifier{recordingNotifier: recordingNotifier{messageID: "msg-1"}}
	m := NewManager(nil, nil, WithNotifier(notifier), WithWaitingTransitionDelays(time.Hour, time.Hour), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager: m,
		session: Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
	}
	notifier.afterNotify = func() {
		rt.HandleOutput([]byte("more output"))
		rt.SetVisibleSnapshot("> echo hello\npartial\ncomplete")
		rt.mu.Lock()
		rt.session.Status = StatusWaiting
		rt.mu.Unlock()
	}
	setTrustedLegacyRoundFixture(rt, ">", "echo hello\r", "> echo hello\npartial")
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	version := rt.notifyVersion
	rt.mu.Unlock()

	rt.notifyIfStillWaiting(version)
	rt.mu.Lock()
	preservedMessageID := rt.lastNotifiedMessageID
	nextVersion := rt.notifyVersion
	rt.mu.Unlock()
	if preservedMessageID != "msg-1" {
		t.Fatalf("same-round create should preserve message id after version advance, got %q", preservedMessageID)
	}

	rt.notifyIfStillWaiting(nextVersion)
	notes := notifier.notes()
	if len(notes) != 2 {
		t.Fatalf("expected create then update, got %#v", notes)
	}
	if notes[0].MessageID != "" {
		t.Fatalf("first notification should create, got %#v", notes[0])
	}
	if notes[1].MessageID != "msg-1" || notes[1].UpdateNo != 1 || notes[1].SuppressUpdateTip {
		t.Fatalf("second same-round notification should update msg-1 with update marker 1, got %#v", notes[1])
	}
}

func TestConcurrentSameRoundNotificationsCreateThenUpdateOneCard(t *testing.T) {
	notifier := newBlockingRefreshNotifier("msg-1")
	m := NewManager(nil, nil, WithNotifier(notifier), WithWaitingTransitionDelays(time.Hour, time.Hour), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager: m,
		session: Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
	}
	setTrustedLegacyRoundFixture(rt, ">", "hello\r", "> hello\npartial")
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	firstVersion := rt.notifyVersion
	rt.mu.Unlock()

	firstDone := make(chan struct{})
	go func() {
		rt.notifyIfStillWaiting(firstVersion)
		close(firstDone)
	}()
	<-notifier.notifyStarted

	rt.HandleOutput([]byte("more output"))
	rt.SetVisibleSnapshot("> hello\npartial\ncomplete")
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	secondVersion := rt.notifyVersion
	rt.mu.Unlock()
	secondDone := make(chan struct{})
	go func() {
		rt.notifyIfStillWaiting(secondVersion)
		close(secondDone)
	}()

	close(notifier.releaseNotify)
	<-firstDone
	<-secondDone
	notes := notifier.notes()
	if len(notes) != 2 || notes[0].MessageID != "" || notes[1].MessageID != "msg-1" {
		t.Fatalf("same-round notifications should create then update one card, got %#v", notes)
	}
}

func TestNotifyIfStillWaitingIncrementsExistingUpdateNumber(t *testing.T) {
	notifier := &recordingNotifier{messageID: "msg-1"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager:                 m,
		session:                 Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastNotifiedMessageID:   "msg-1",
		notificationUpdateNo:    2,
		lastInputText:           "echo hello",
		snapshotAtRoundStart:    "$ echo hello",
		snapshotAtRoundVersion:  1,
		snapshotAtRoundStartSet: true,
		roundReply:              []byte("old\nnew"),
		visibleSnapshot:         "$ echo hello\nold\nnew\n$",
		visibleSnapshotVersion:  2,
	}
	rt.mu.Lock()
	version := rt.notifyVersion
	rt.mu.Unlock()

	rt.notifyIfStillWaiting(version)

	notes := notifier.notes()
	if len(notes) != 1 {
		t.Fatalf("expected one update notification, got %#v", notes)
	}
	if notes[0].MessageID != "msg-1" || notes[0].UpdateNo != 3 || notes[0].SuppressUpdateTip {
		t.Fatalf("automatic update should increment update marker and allow update tip, got %#v", notes[0])
	}
	if rt.notificationUpdateNo != 3 {
		t.Fatalf("runtime update marker should increment, got %d", rt.notificationUpdateNo)
	}
}

func TestWaitingNotificationUsesRoundStartSnapshotBeforeLastNotificationSnapshot(t *testing.T) {
	notifier := &recordingNotifier{messageID: "msg-1"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager:                     m,
		session:                     Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastInputText:               "关闭 8083",
		lastNotifiedVisibleSnapshot: "stale previous round\nold footer",
		snapshotAtRoundStart: strings.Join([]string{
			"old output",
			"› 关闭 8083",
			"gpt-5.4 low fast · ~/Iris_Workspace/测试",
		}, "\n"),
		snapshotAtRoundStartSet: true,
		visibleSnapshot: strings.Join([]string{
			"old output",
			"› 关闭 8083",
			"• Ran lsof -nP -iTCP:8083 -sTCP:LISTEN",
			"  (no output)",
			"已关闭 8083 接口。",
			"gpt-5.4 low fast · ~/Iris_Workspace/测试",
		}, "\n"),
		visibleSnapshotVersion: 2,
		snapshotAtRoundVersion: 1,
		notifyVersion:          7,
	}

	rt.notifyIfStillWaiting(7)

	notes := notifier.notes()
	if len(notes) != 1 {
		t.Fatalf("expected one notification, got %#v", notes)
	}
	want := strings.Join([]string{
		"• Ran lsof -nP -iTCP:8083 -sTCP:LISTEN",
		"  (no output)",
		"已关闭 8083 接口。",
	}, "\n")
	if notes[0].Content != want {
		t.Fatalf("notification should diff from round start snapshot:\n%q\nwant:\n%q", notes[0].Content, want)
	}
}

func TestWaitingNotificationUsesInputAnchorWhenRoundBaselineIsEmpty(t *testing.T) {
	rt := &RuntimeSession{
		manager:                     NewManager(nil, nil),
		session:                     Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastInputText:               "ask",
		lastNotifiedVisibleSnapshot: "> ask\nfirst",
		snapshotAtRoundStart:        "",
		snapshotAtRoundVersion:      0,
		snapshotAtRoundStartSet:     true,
		visibleSnapshot:             "> ask\nfirst\nsecond",
		visibleSnapshotVersion:      1,
	}

	rt.mu.Lock()
	n, _, ok := rt.waitingNotificationLocked()
	rt.mu.Unlock()
	if !ok || n.Content != "first\nsecond" {
		t.Fatalf("an explicit input prompt must work without a round baseline: ok=%v content=%q", ok, n.Content)
	}
}

func TestWaitingNotificationTakesFreshSnapshotAfterRoundOwnerDisconnects(t *testing.T) {
	notifier := &recordingNotifier{}
	rt := &RuntimeSession{
		manager:                 NewManager(nil, nil, WithNotifier(notifier)),
		session:                 Session{ID: "sess-owner-takeover", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastInputText:           "确认发布结果",
		notifyVersion:           7,
		snapshotAtRoundStart:    "› 确认发布结果",
		snapshotAtRoundVersion:  1,
		snapshotAtRoundStartSet: true,
		visibleSnapshot:         "› 确认发布结果",
		visibleSnapshotVersion:  1,
		subscribers:             make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	owner, cancelOwner := rt.SubscribeWithMode(false)
	replacement, cancelReplacement := rt.SubscribeWithMode(false)
	defer cancelReplacement()
	rt.mu.Lock()
	rt.snapshotAtRoundResponder = owner
	rt.snapshotAtRoundSource = "browser:buffer"
	rt.visibleSnapshotResponder = owner
	rt.visibleSnapshotSource = "browser:buffer"
	rt.mu.Unlock()
	cancelOwner()

	done := make(chan struct{})
	go func() {
		rt.notifyIfStillWaitingImmediately(7)
		close(done)
	}()
	event := receiveSnapshotRequestEvent(t, replacement)
	rt.SetVisibleSnapshotResponseFrom("› 确认发布结果\n• 已完成并发布。", "browser:buffer", event.RequestID, replacement)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waiting notification did not finish after renderer takeover")
	}
	notes := notifier.notes()
	if len(notes) != 1 || notes[0].Content != "• 已完成并发布。" || notes[0].Running {
		t.Fatalf("renderer takeover must finalize the waiting card with current content, got %#v", notes)
	}
}

func TestOutputAfterNotificationPatchesRunningTitleOnWaitingToRunning(t *testing.T) {
	notifier := &recordingNotifier{messageID: "msg-1"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager: m,
		session: Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
	}
	setTrustedLegacyRoundFixture(rt, "›", "今天天气怎么样\r", "> 今天天气怎么样\n• 你想查哪个城市的天气？")
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	version := rt.notifyVersion
	rt.mu.Unlock()

	rt.notifyIfStillWaiting(version)
	rt.HandleOutput([]byte("more output"))

	running := waitForRunningNotes(t, notifier, 1)
	if len(running) != 1 || running[0].MessageID != "msg-1" || !running[0].Running {
		t.Fatalf("terminal output should patch the card title to running, got %#v", running)
	}
}

func TestNotifyInputRunningUsesClickedMessageAnchorWithoutPlaceholderPatch(t *testing.T) {
	notifier := &recordingNotifier{messageID: "bot-card"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager: m,
		session: Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
	}

	rt.NotifyInputRunningOnMessage("bot-card")

	notes := notifier.notes()
	if len(notes) != 0 {
		t.Fatalf("running card should not overwrite clicked message with placeholder, got %#v", notes)
	}
	if rt.lastNotifiedMessageID != "bot-card" {
		t.Fatalf("runtime anchor = %q, want bot-card", rt.lastNotifiedMessageID)
	}
}

func TestRefreshNotificationMessageUsesCurrentRoundSnapshot(t *testing.T) {
	notifier := &recordingNotifier{messageID: "bot-card"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager:              m,
		session:              Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		notificationUpdateNo: 2,
	}
	rt.SetVisibleSnapshot("$ echo hello")
	rt.MarkInputActivity("echo hello\r")
	rt.SetVisibleSnapshot("$ echo hello\nhello\n$")
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	rt.mu.Unlock()

	if err := rt.RefreshNotificationMessage("bot-card", 2); err != nil {
		t.Fatal(err)
	}

	notes := notifier.notes()
	if len(notes) != 1 {
		t.Fatalf("expected one manual refresh update, got %#v", notes)
	}
	if notes[0].MessageID != "bot-card" || notes[0].Content != "hello\n$" || notes[0].Running {
		t.Fatalf("manual refresh should patch clicked card with current round, got %#v", notes[0])
	}
	if notes[0].UpdateNo != 2 || !notes[0].SuppressUpdateTip {
		t.Fatalf("manual refresh should preserve existing update marker without increasing count, got %#v", notes[0])
	}
	if rt.notificationUpdateNo != 2 {
		t.Fatalf("runtime update marker should not increase on manual refresh, got %d", rt.notificationUpdateNo)
	}
}

func TestRefreshBeforeStopHookStillUsesVisibleTail(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refresh func(*RuntimeSession) error
	}{
		{name: "manual", refresh: func(rt *RuntimeSession) error { return rt.RefreshNotificationMessage("bot-card") }},
		{name: "auto", refresh: func(rt *RuntimeSession) error { return rt.AutoRefreshNotificationMessage("bot-card") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notifier := &recordingNotifier{messageID: "bot-card"}
			rt := &RuntimeSession{
				manager:               NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0)),
				session:               Session{ID: "sess-1", Name: "A", Status: StatusRunning, Live: true, NotifyOnWaiting: true, LastMode: SessionModeAgent, LastAgentKind: "codex"},
				lastInputText:         "正在执行的任务",
				lastNotifiedMessageID: "bot-card",
				roundReply:            []byte("current round produced terminal output"),
				visibleSnapshot:       "历史内容\n• 本轮尚未结束时的可见进度",
			}

			if err := tc.refresh(rt); err != nil {
				t.Fatal(err)
			}
			notes := notifier.notes()
			if len(notes) != 1 || !strings.Contains(notes[0].Content, "本轮尚未结束时的可见进度") {
				t.Fatalf("refresh before Stop Hook must keep the visible-tail fallback, got %#v", notes)
			}
		})
	}
}

func TestRefreshNotificationMessageUsesInputAnchorWhenRoundBaselineIsEmpty(t *testing.T) {
	notifier := &recordingNotifier{messageID: "bot-card"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager:                     m,
		session:                     Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastInputText:               "ask",
		lastNotifiedMessageID:       "bot-card",
		lastNotifiedVisibleSnapshot: "> ask\nfirst",
		snapshotAtRoundStart:        "",
		snapshotAtRoundVersion:      0,
		snapshotAtRoundStartSet:     true,
		visibleSnapshot:             "> ask\nfirst\nsecond",
		visibleSnapshotVersion:      1,
	}

	if err := rt.RefreshNotificationMessage("bot-card", 1); err != nil {
		t.Fatal(err)
	}

	notes := notifier.notes()
	if len(notes) != 1 {
		t.Fatalf("expected one manual refresh update, got %#v", notes)
	}
	want := "first\nsecond"
	if notes[0].Content != want {
		t.Fatalf("manual refresh should use the explicit input anchor without a round baseline:\n%q\nwant:\n%q", notes[0].Content, want)
	}
}

func TestRefreshNotificationMessageKeepsLongerContentWhenSnapshotRegressesToPrefix(t *testing.T) {
	notifier := &recordingNotifier{messageID: "bot-card"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	previous := strings.Join([]string{
		"• 只下你需要的人像数字人版本：约 660MB。",
		"如果用官方全量下载：约 2.14GB。",
		"所以你流量紧张的话，按 700MB 预算准备就够，别下载动物模型和不需要的文件。",
	}, "\n")
	rt := &RuntimeSession{
		manager:                 m,
		session:                 Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastInputText:           "需要下载哪些东西？",
		lastNotifiedMessageID:   "bot-card",
		lastNotifiedContent:     previous,
		lastNotifiedRoundHash:   notifyContentHash(previous),
		snapshotAtRoundStart:    "> 需要下载哪些东西？",
		snapshotAtRoundVersion:  1,
		snapshotAtRoundStartSet: true,
		visibleSnapshot:         "> 需要下载哪些东西？\n• 只下你需要的人像数字人版本：约 660MB。\n如果用官方全量下载：约 2.14GB。\n›",
		visibleSnapshotVersion:  2,
	}

	if err := rt.RefreshNotificationMessage("bot-card", 1); err != nil {
		t.Fatal(err)
	}

	notes := notifier.notes()
	if len(notes) != 1 {
		t.Fatalf("expected one manual refresh update, got %#v", notes)
	}
	if notes[0].Content != previous {
		t.Fatalf("manual refresh should preserve longer content:\n%q\nwant:\n%q", notes[0].Content, previous)
	}
}

func TestRefreshNotificationMessageUsesCachedTailWhenFreshSnapshotIsUnavailable(t *testing.T) {
	notifier := &recordingNotifier{messageID: "bot-card"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager:                 m,
		session:                 Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastInputText:           "next question",
		lastNotifiedMessageID:   "bot-card",
		snapshotAtRoundStart:    "> next question",
		snapshotAtRoundVersion:  2,
		snapshotAtRoundStartSet: true,
		visibleSnapshot:         "> next question",
		visibleSnapshotVersion:  2,
	}

	if err := rt.RefreshNotificationMessage("bot-card", 1); err != nil {
		t.Fatal(err)
	}
	notes := notifier.notes()
	if len(notes) != 1 || notes[0].Content != "> next question" {
		t.Fatalf("stale manual refresh must use the cached visible tail instead of returning nothing, got %#v", notes)
	}
}

func TestRefreshNotificationMessageFallsBackToUnchangedVisibleTail(t *testing.T) {
	notifier := &recordingNotifier{messageID: "bot-card"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	visible := "$ echo hello\nhello\n$"
	rt := &RuntimeSession{
		manager:                     m,
		session:                     Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastNotifiedMessageID:       "bot-card",
		lastNotifiedVisibleSnapshot: visible,
	}
	rt.SetVisibleSnapshot(visible)

	if err := rt.RefreshNotificationMessage("bot-card"); err != nil {
		t.Fatal(err)
	}

	notes := notifier.notes()
	if len(notes) != 1 {
		t.Fatalf("expected one manual refresh update, got %#v", notes)
	}
	if notes[0].Content != visible {
		t.Fatalf("empty diff should use the latest visible tail, got %#v", notes[0])
	}
}

func TestRefreshNotificationMessageUsesVisibleTailForUnbaselinedOrdinaryRound(t *testing.T) {
	notifier := &recordingNotifier{messageID: "bot-card"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager:                m,
		session:                Session{ID: "sess-unbaselined", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastInputText:          "this is a unique current question with enough detail",
		lastNotifiedMessageID:  "bot-card",
		visibleSnapshot:        "OLD_HISTORY_MUST_NOT_LEAK\n› this is a unique current question with enough detail\n• untrusted reply",
		visibleSnapshotVersion: 1,
		visibleSnapshotSource:  "browser:buffer",
	}

	if err := rt.RefreshNotificationMessage("bot-card"); err != nil {
		t.Fatal(err)
	}

	notes := notifier.notes()
	if len(notes) != 1 {
		t.Fatalf("expected one manual refresh update, got %#v", notes)
	}
	want := "• untrusted reply"
	if notes[0].Content != want {
		t.Fatalf("an unbaselined manual refresh should use the explicit input anchor, got %#v", notes[0])
	}
}

func TestTailAnchorRequiresSameRendererSourceAndWidth(t *testing.T) {
	const normalSource = "browser:buffer;continuity_version=2;render_epoch=31;buffer_type=normal;buffer_at_capacity=false;anchor_guard_active=true;anchor_guard_line=0"
	const domSource = "browser:dom;continuity_version=2;render_epoch=31;buffer_type=normal;buffer_at_capacity=false;anchor_guard_active=true;anchor_guard_line=0"
	baseline := strings.Join([]string{
		"renderer-specific historical header",
		"distinctive previous conclusion alpha",
		"distinctive previous detail beta",
		"distinctive previous detail gamma",
		"distinctive previous detail delta",
		"distinctive previous detail epsilon",
	}, "\n")
	visible := strings.Join([]string{
		"different renderer historical header",
		"distinctive previous conclusion alpha",
		"distinctive previous detail beta",
		"distinctive previous detail gamma",
		"distinctive previous detail delta",
		"distinctive previous detail epsilon",
		"CURRENT_REPLY_ONLY",
	}, "\n")
	firstRenderer := make(chan RuntimeEvent)
	secondRenderer := make(chan RuntimeEvent)

	tests := []struct {
		name             string
		baselineSource   string
		currentSource    string
		baselineRenderer chan RuntimeEvent
		currentRenderer  chan RuntimeEvent
		baselineCols     uint16
		currentCols      uint16
		want             string
	}{
		{
			name:             "same renderer",
			baselineSource:   normalSource,
			currentSource:    normalSource,
			baselineRenderer: firstRenderer,
			currentRenderer:  firstRenderer,
			baselineCols:     120,
			currentCols:      120,
			want:             "CURRENT_REPLY_ONLY",
		},
		{
			name:             "different browser",
			baselineSource:   normalSource,
			currentSource:    normalSource,
			baselineRenderer: firstRenderer,
			currentRenderer:  secondRenderer,
			baselineCols:     120,
			currentCols:      120,
		},
		{
			name:             "buffer to DOM fallback",
			baselineSource:   normalSource,
			currentSource:    domSource,
			baselineRenderer: firstRenderer,
			currentRenderer:  firstRenderer,
			baselineCols:     120,
			currentCols:      120,
		},
		{
			name:             "terminal width changed",
			baselineSource:   normalSource,
			currentSource:    normalSource,
			baselineRenderer: firstRenderer,
			currentRenderer:  firstRenderer,
			baselineCols:     120,
			currentCols:      90,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &RuntimeSession{
				lastInputText:            "input missing from snapshot",
				snapshotAtRoundStart:     baseline,
				snapshotAtRoundStartSet:  true,
				snapshotAtRoundSource:    tt.baselineSource,
				snapshotAtRoundResponder: tt.baselineRenderer,
				snapshotAtRoundCols:      tt.baselineCols,
				visibleSnapshot:          visible,
				visibleSnapshotSource:    tt.currentSource,
				visibleSnapshotResponder: tt.currentRenderer,
				visibleSnapshotCols:      tt.currentCols,
			}
			if got := rt.currentNotifyContentLocked(); got != tt.want {
				t.Fatalf("content = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInputAnchorDoesNotCrossRendererWhenBaselineChanged(t *testing.T) {
	const normalSource = "browser:buffer;continuity_version=2;render_epoch=41;buffer_type=normal;buffer_at_capacity=false;anchor_guard_active=true;anchor_guard_line=0"
	baseline := strings.Join([]string{
		"OLD_BASELINE_CONTEXT",
		"old answer",
		"› 请检查这一次跨浏览器锚点",
	}, "\n")
	visible := strings.Join([]string{
		"OLD_BASELINE_CONTEXT",
		"old answer",
		"› 请检查这一次跨浏览器锚点",
		"• CURRENT_REPLY_ONLY",
	}, "\n")
	firstRenderer := make(chan RuntimeEvent)
	secondRenderer := make(chan RuntimeEvent)

	content := func(currentRenderer chan RuntimeEvent) string {
		rt := &RuntimeSession{
			lastInputText:            "请检查这一次跨浏览器锚点",
			snapshotAtRoundStart:     baseline,
			snapshotAtRoundStartSet:  true,
			snapshotAtRoundSource:    normalSource,
			snapshotAtRoundResponder: firstRenderer,
			snapshotAtRoundCols:      120,
			visibleSnapshot:          visible,
			visibleSnapshotSource:    normalSource,
			visibleSnapshotResponder: currentRenderer,
			visibleSnapshotCols:      120,
		}
		return rt.currentNotifyContentLocked()
	}

	if got := content(firstRenderer); got != "• CURRENT_REPLY_ONLY" {
		t.Fatalf("same-renderer input anchor should remain available, got %q", got)
	}
	if got := content(secondRenderer); got != "• CURRENT_REPLY_ONLY" {
		t.Fatalf("renderer identity must not veto an explicit input occurrence: %q", got)
	}
}

func TestTerminalOutputSnapshotIsUnaffectedByNotificationDiff(t *testing.T) {
	rt := &RuntimeSession{
		manager:                 NewManager(nil, nil),
		session:                 Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastInputText:           "ask",
		snapshotAtRoundStart:    "old screen\n> ask",
		snapshotAtRoundVersion:  1,
		snapshotAtRoundStartSet: true,
		visibleSnapshot: strings.Join([]string{
			"old screen",
			"> ask",
			"first",
			"second",
		}, "\n"),
		visibleSnapshotVersion: 2,
	}
	rt.HandleOutput([]byte("old terminal history\n"))
	rt.HandleOutput([]byte("current terminal output\n"))

	rt.mu.Lock()
	n, _, ok := rt.waitingNotificationLocked()
	rt.mu.Unlock()
	if !ok {
		t.Fatal("expected notification to be ready")
	}
	if strings.Contains(n.Content, "old screen") {
		t.Fatalf("notification should use diff content, got %q", n.Content)
	}
	out := string(rt.OutputSnapshot())
	if !strings.Contains(out, "old terminal history") || !strings.Contains(out, "current terminal output") {
		t.Fatalf("terminal output snapshot should remain full raw history, got %q", out)
	}
}

func TestManualRefreshSchedulesAutoRefreshWhenEnabled(t *testing.T) {
	notifier := &recordingNotifier{messageID: "bot-card"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithAutoRefreshInterval(20*time.Millisecond), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager:               m,
		session:               Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastNotifiedMessageID: "bot-card",
		autoRefreshEnabled:    true,
		autoRefreshMessageID:  "bot-card",
		autoRefreshStop:       make(chan struct{}),
	}
	rt.SetVisibleSnapshot("$ echo hello\nhello\n$")

	if err := rt.RefreshNotificationMessage("bot-card"); err != nil {
		t.Fatal(err)
	}

	notes := waitForNotifierNotes(t, notifier, 2)
	if len(notes) != 2 {
		t.Fatalf("expected manual refresh plus one scheduled auto refresh, got %#v", notes)
	}
	if !notes[0].SuppressUpdateTip {
		t.Fatalf("first refresh should be manual, got %#v", notes[0])
	}
	if notes[1].SuppressUpdateTip {
		t.Fatalf("scheduled refresh should use auto refresh behavior, got %#v", notes[1])
	}
}

func TestAutoRefreshRebindsToNewRunningCard(t *testing.T) {
	notifier := &recordingNotifier{messageID: "new-card"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithAutoRefreshInterval(time.Hour))
	rt := &RuntimeSession{
		manager: m,
		session: Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
	}
	if enabled, err := rt.ToggleAutoRefresh("old-card"); err != nil || !enabled {
		t.Fatalf("toggle auto refresh = %v, %v", enabled, err)
	}

	rt.MarkInputActivity("echo hello\r")
	rt.NotifyInputRunning()

	rt.mu.Lock()
	messageID := rt.autoRefreshMessageID
	rt.mu.Unlock()
	if messageID != "new-card" {
		t.Fatalf("auto refresh should follow the new running card, got %q", messageID)
	}
	notes := notifier.notes()
	if len(notes) != 1 || notes[0].MessageID != "" || !notes[0].AutoRefreshEnabled {
		t.Fatalf("running notification should create a new auto-refresh-enabled card, got %#v", notes)
	}
}

func TestOverlappingRunningInputFreezesPreviousCardAndCarriesWindowStart(t *testing.T) {
	notifier := &recordingNotifier{createMessageIDs: []string{"card-2", "card-3"}}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager: m,
		session: Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
	}
	rt.SetVisibleSnapshot("chat\n>")

	rt.MarkInputActivity("second question\r")
	rt.NotifyInputRunning()
	rt.SetVisibleSnapshot("chat\n>\n> second question\npartial second answer")

	rt.MarkInputActivity("third question\r")
	rt.NotifyInputRunning()
	if !rt.NotificationMessageFrozen("card-2") {
		t.Fatal("previous running card should be frozen after overlapping input")
	}
	if err := rt.RefreshNotificationMessage("card-2"); err == nil {
		t.Fatal("frozen card should reject manual refresh")
	}

	rt.SetVisibleSnapshot("chat\n>\n> second question\npartial second answer\n> third question\nfinal third answer\n>")
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	version := rt.notifyVersion
	rt.mu.Unlock()
	rt.notifyIfStillWaiting(version)

	notes := waitForNotifierNotes(t, notifier, 4)
	if len(notes) != 4 {
		t.Fatalf("expected two running creates, one disabled update, and one final update, got %#v", notes)
	}
	runningCreates := 0
	disabledOldCard := false
	finalLatestCard := false
	want := "> second question\npartial second answer\n> third question\nfinal third answer\n>"
	for _, note := range notes {
		if note.MessageID == "" && note.Content == RunningNotificationPlaceholder && note.Running {
			runningCreates++
		}
		if note.MessageID == "card-2" && note.Disabled && !note.Running {
			disabledOldCard = true
		}
		if note.MessageID == "card-3" && note.Content == want && !note.Running && !note.Disabled {
			finalLatestCard = true
		}
	}
	if runningCreates != 2 || !disabledOldCard || !finalLatestCard {
		t.Fatalf("overlap should disable old card and patch latest card, got %#v", notes)
	}
}

func TestRefreshNotificationMessagePreventsStaleRunningPlaceholderOverwrite(t *testing.T) {
	notifier := &recordingNotifier{messageID: "bot-card"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager: m,
		session: Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
	}
	setTrustedLegacyRoundFixture(rt, "$", "echo hello\r", "$ echo hello\nhello\n$")

	rt.notificationPatchMu.Lock()
	runningDone := make(chan struct{})
	go func() {
		rt.NotifyInputRunningOnMessage("bot-card")
		close(runningDone)
	}()
	time.Sleep(50 * time.Millisecond)
	refreshDone := make(chan error, 1)
	go func() {
		refreshDone <- rt.RefreshNotificationMessage("bot-card")
	}()
	time.Sleep(50 * time.Millisecond)
	rt.notificationPatchMu.Unlock()

	select {
	case <-runningDone:
	case <-time.After(time.Second):
		t.Fatal("stale running update did not return")
	}
	select {
	case err := <-refreshDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("refresh update did not return")
	}
	notes := notifier.notes()
	if len(notes) != 1 {
		t.Fatalf("refresh should be the only card patch, got %#v", notes)
	}
	if notes[0].Content == RunningNotificationPlaceholder || notes[0].Content != "hello\n$" {
		t.Fatalf("stale running placeholder should not overwrite refresh content, got %#v", notes[0])
	}
}

func TestNotifyInputRunningDoesNotPatchExistingCard(t *testing.T) {
	notifier := &recordingNotifier{messageID: "bot-card"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager:               m,
		session:               Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastNotifiedMessageID: "bot-card",
		lastNotifiedContent:   "$ echo hello\nhello\n$",
	}

	rt.NotifyInputRunningOnMessage("bot-card")

	notes := notifier.notes()
	if len(notes) != 0 {
		t.Fatalf("running update should not patch existing card, got %#v", notes)
	}
}

func TestNotifyInputRunningDoesNotPatchExistingCardFromCurrentSnapshot(t *testing.T) {
	notifier := &recordingNotifier{messageID: "bot-card"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager:               m,
		session:               Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastNotifiedMessageID: "bot-card",
	}
	rt.MarkInputActivity("echo hello\r")
	rt.lastNotifiedMessageID = "bot-card"
	rt.SetVisibleSnapshot("$ echo hello\nhello\n$")

	rt.NotifyInputRunningOnMessage("bot-card")

	notes := notifier.notes()
	if len(notes) != 0 {
		t.Fatalf("running update should not patch existing card from current snapshot, got %#v", notes)
	}
}

func TestOutputPatchesRunningTitleFromCurrentRoundOnWaitingToRunning(t *testing.T) {
	notifier := &recordingNotifier{messageID: "bot-card"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager:               m,
		session:               Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastNotifiedMessageID: "bot-card",
	}
	rt.SetVisibleSnapshot("$")
	rt.MarkInputActivity("echo hello\r")
	rt.lastNotifiedMessageID = "bot-card"
	rt.SetVisibleSnapshot("$ echo hello\nhello\n$")
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	rt.mu.Unlock()

	rt.HandleOutput([]byte("still running\n"))

	running := waitForRunningNotes(t, notifier, 1)
	if len(running) != 1 || running[0].MessageID != "bot-card" || running[0].Content != "hello\n$" || !running[0].Running {
		t.Fatalf("terminal output should patch current round title as running, got %#v", running)
	}
}

func TestOutputAfterNotificationDoesNotPatchWhenAlreadyRunning(t *testing.T) {
	notifier := &recordingNotifier{messageID: "msg-1"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager: m,
		session: Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
	}
	rt.MarkInputActivity("今天天气怎么样\r")
	rt.SetVisibleSnapshot("> 今天天气怎么样\n• 先给一版计划。")
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	version := rt.notifyVersion
	rt.mu.Unlock()

	rt.notifyIfStillWaiting(version)
	rt.mu.Lock()
	rt.session.Status = StatusRunning
	rt.mu.Unlock()
	rt.HandleOutput([]byte("more output"))

	running := notifier.runningNotes()
	if len(running) != 0 {
		t.Fatalf("running output should not re-patch card when session is already running, got %#v", running)
	}
}

func TestManualRefreshAllowsWaitingToRunningTitleMarker(t *testing.T) {
	notifier := &recordingNotifier{messageID: "bot-card"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager:               m,
		session:               Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastNotifiedMessageID: "bot-card",
	}
	rt.SetVisibleSnapshot("$")
	rt.MarkInputActivity("echo hello\r")
	rt.lastNotifiedMessageID = "bot-card"
	rt.SetVisibleSnapshot("$ echo hello\nhello\n$")

	if err := rt.RefreshNotificationMessage("bot-card"); err != nil {
		t.Fatal(err)
	}
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	rt.mu.Unlock()
	rt.HandleOutput([]byte("late output\n"))

	notes := notifier.notes()
	if len(notes) != 1 || notes[0].MessageID != "bot-card" || notes[0].Content != "hello\n$" {
		t.Fatalf("manual refresh should patch current content, got %#v", notes)
	}
	if running := waitForRunningNotes(t, notifier, 1); len(running) != 1 || running[0].MessageID != "bot-card" || !running[0].Running {
		t.Fatalf("waiting-to-running output should patch running title after manual refresh, got %#v", running)
	}
}

func TestManualRefreshWithConcurrentOutputPatchesRunningTitleAfterRefresh(t *testing.T) {
	notifier := newBlockingRefreshNotifier("bot-card")
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager:               m,
		session:               Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastNotifiedMessageID: "bot-card",
		lastNotifiedContent:   "$ echo hello\nhello\n$",
	}
	rt.SetVisibleSnapshot("$")
	rt.MarkInputActivity("echo hello\r")
	rt.lastNotifiedMessageID = "bot-card"
	rt.SetVisibleSnapshot("$ echo hello\nhello\n$")

	refreshDone := make(chan error, 1)
	go func() {
		refreshDone <- rt.RefreshNotificationMessage("bot-card")
	}()
	select {
	case <-notifier.notifyStarted:
	case <-time.After(time.Second):
		t.Fatal("manual refresh did not start")
	}
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	rt.mu.Unlock()
	rt.HandleOutput([]byte("late output\n"))
	close(notifier.releaseNotify)
	select {
	case err := <-refreshDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("manual refresh did not finish")
	}

	notes := notifier.notes()
	if len(notes) != 1 || notes[0].MessageID != "bot-card" || notes[0].Content != "hello\n$" {
		t.Fatalf("manual refresh should patch current content, got %#v", notes)
	}
	if running := waitForBlockingRunningNotes(t, notifier, 1); len(running) != 1 || running[0].MessageID != "bot-card" || !running[0].Running {
		t.Fatalf("concurrent waiting-to-running output should patch running title after refresh, got %#v", running)
	}
}

func TestManualRefreshKeepsRunningStatus(t *testing.T) {
	notifier := &recordingNotifier{messageID: "bot-card"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager:               m,
		session:               Session{ID: "sess-1", Name: "A", Status: StatusRunning, Live: true, NotifyOnWaiting: true},
		lastNotifiedMessageID: "bot-card",
	}
	rt.MarkInputActivity("echo hello\r")
	rt.lastNotifiedMessageID = "bot-card"
	rt.SetVisibleSnapshot("$ echo hello\nhello\n$")

	if err := rt.RefreshNotificationMessage("bot-card"); err != nil {
		t.Fatal(err)
	}

	notes := notifier.notes()
	if len(notes) != 1 {
		t.Fatalf("expected one manual refresh update, got %#v", notes)
	}
	if !notes[0].Running {
		t.Fatalf("manual refresh should keep running status, got %#v", notes[0])
	}
	if !rt.notificationRunning {
		t.Fatalf("manual refresh should keep runtime notification state as running")
	}
}

func TestWaitingTransitionKeepsRunningTitleUntilFinalNotification(t *testing.T) {
	notifier := &recordingNotifier{messageID: "msg-1"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager:                 m,
		session:                 Session{ID: "sess-1", Name: "A", Status: StatusRunning, Live: true, NotifyOnWaiting: true},
		lastNotifiedMessageID:   "msg-1",
		lastNotifiedContent:     "> echo hello\nhello",
		notificationUpdateNo:    1,
		notificationRunning:     true,
		visibleSnapshot:         "> echo hello\nhello",
		visibleSnapshotSource:   "legacy",
		visibleSnapshotVersion:  1,
		snapshotAtRoundStart:    "> echo hello",
		snapshotAtRoundSource:   "legacy",
		snapshotAtRoundVersion:  0,
		snapshotAtRoundStartSet: true,
		lastInputText:           "echo hello",
		stateVersion:            7,
		notifyVersion:           3,
	}

	rt.notifyAfterStable(7)

	running := notifier.runningNotes()
	if len(running) != 0 {
		t.Fatalf("waiting transition should not clear running title before final notification, got %#v", running)
	}
	if got := rt.Snapshot().Status; got != StatusWaiting {
		t.Fatalf("session should transition to waiting, got %s", got)
	}
	if rt.notificationRunning {
		t.Fatal("runtime should clear running notification state after final waiting notification")
	}
	notes := notifier.notes()
	if len(notes) != 1 || notes[0].MessageID != "msg-1" || notes[0].Running {
		t.Fatalf("final waiting notification should replace current running message with non-running card, got %#v", notes)
	}
}

func TestOutputDuringInFlightWaitingPatchPatchesRunningTitleAfterWaitingPatch(t *testing.T) {
	notifier := newSequencingNotifier("msg-1")
	m := NewManager(nil, nil, WithNotifier(notifier), WithWaitingTransitionDelays(time.Hour, time.Hour), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager:                 m,
		session:                 Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastNotifiedMessageID:   "msg-1",
		lastNotifiedContent:     "> echo hello\npartial",
		notificationUpdateNo:    1,
		notificationRunning:     false,
		lastInputText:           "echo hello",
		visibleSnapshot:         "> echo hello\npartial\ncomplete",
		visibleSnapshotVersion:  1,
		snapshotAtRoundStart:    "> echo hello",
		snapshotAtRoundStartSet: true,
		roundReply:              []byte("partial\ncomplete"),
		notifyVersion:           4,
	}

	done := make(chan struct{})
	go func() {
		rt.notifyIfStillWaiting(4)
		close(done)
	}()

	select {
	case <-notifier.notifyStarted:
	case <-time.After(time.Second):
		t.Fatal("waiting notification update did not start")
	}
	outputDone := make(chan struct{})
	go func() {
		rt.HandleOutput([]byte("more output"))
		close(outputDone)
	}()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-notifier.runningStarted:
		t.Fatal("terminal output should not patch running title")
	default:
	}
	select {
	case <-outputDone:
	case <-time.After(time.Second):
		t.Fatal("output handling should not wait for waiting patch")
	}

	close(notifier.releaseNotify)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waiting notification update did not finish")
	}
	select {
	case <-notifier.runningStarted:
	case <-time.After(time.Second):
		t.Fatal("terminal output should patch running title after waiting patch")
	}

	select {
	case <-outputDone:
	case <-time.After(time.Second):
		t.Fatal("output handling should finish after running title patch")
	}

	events := notifier.events()
	if len(events) != 2 {
		t.Fatalf("expected waiting patch then running title patch, got %#v", events)
	}
	if events[0] != "notify:false" || events[1] != "running:true" {
		t.Fatalf("unexpected events: %#v", events)
	}
}

func TestNotifyIfStillWaitingSkipsStaleSendAfterNewOutput(t *testing.T) {
	notifier := &recordingNotifier{}
	m := NewManager(nil, nil, WithNotifier(notifier), WithWaitingTransitionDelays(time.Hour, time.Hour))
	rt := &RuntimeSession{
		manager: m,
		session: Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
	}
	rt.MarkInputActivity("echo hello\r")
	rt.SetVisibleSnapshot("$ echo hello\npartial")
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	version := rt.notifyVersion
	rt.mu.Unlock()

	rt.notificationPatchMu.Lock()
	done := make(chan struct{})
	go func() {
		rt.notifyIfStillWaiting(version)
		close(done)
	}()
	time.Sleep(200 * time.Millisecond)
	rt.HandleOutput([]byte("complete\n"))
	rt.SetVisibleSnapshot("$ echo hello\npartial\ncomplete\n$")
	rt.notificationPatchMu.Unlock()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stale waiting notification did not return")
	}
	if got := notifier.count(); got != 0 {
		t.Fatalf("stale waiting notification should not be sent after new output, got %d", got)
	}
}

func TestNotifyIfStillWaitingCoalescesSameRoundUpdate(t *testing.T) {
	notifier := &recordingNotifier{messageID: "msg-1"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithWaitingTransitionDelays(time.Hour, time.Hour), WithNotificationUpdateCoalesce(250*time.Millisecond))
	rt := &RuntimeSession{
		manager: m,
		session: Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
	}
	setTrustedLegacyRoundFixture(rt, "$", "echo hello\r", "$ echo hello\npartial")
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	version := rt.notifyVersion
	rt.mu.Unlock()
	rt.notifyIfStillWaiting(version)

	rt.HandleOutput([]byte(" more"))
	rt.SetVisibleSnapshot("$ echo hello\npartial more")
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	version = rt.notifyVersion
	rt.mu.Unlock()
	done := make(chan struct{})
	go func() {
		rt.notifyIfStillWaiting(version)
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)
	rt.HandleOutput([]byte(" complete"))
	rt.SetVisibleSnapshot("$ echo hello\npartial more complete\n$")

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("coalesced waiting notification did not return")
	}
	notes := notifier.notes()
	if len(notes) != 1 {
		t.Fatalf("same-round update should be coalesced after newer output, got %#v", notes)
	}
}

func TestRunningTitleUpdateSkipsStaleMessageAfterReplacement(t *testing.T) {
	notifier := &recordingNotifier{}
	m := NewManager(nil, nil, WithNotifier(notifier))
	rt := &RuntimeSession{
		manager:               m,
		session:               Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastNotifiedMessageID: "msg-new",
	}

	rt.updateNotificationRunning(WaitingNotification{
		SessionID: "sess-1",
		Name:      "A",
		Content:   "old",
		MessageID: "msg-old",
		Running:   false,
	}, false)

	if got := len(notifier.runningNotes()); got != 0 {
		t.Fatalf("stale running title update should not patch old message, got %d", got)
	}
}

func TestLarkNotificationCardContentKeepsSessionTitleWithoutStatusNoise(t *testing.T) {
	content, err := larkNotificationCardContent(WaitingNotification{
		SessionID: "sess-1",
		Name:      "A",
		Content:   "done",
		UpdateNo:  2,
	}, "open-id", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, "已更新-2") || strings.Contains(content, "状态：") {
		t.Fatalf("card content should hide update and status markers, got %s", content)
	}
	if !strings.Contains(content, `"update_no":2`) {
		t.Fatalf("refresh action should still carry its internal update number, got %s", content)
	}
	if !strings.Contains(content, `"content":"A"`) {
		t.Fatalf("card header should keep the session title, got %s", content)
	}
}

func TestLarkNotificationCardContentMentionsRoundSender(t *testing.T) {
	note := WaitingNotification{
		SessionID:     "sess-1",
		Name:          "A",
		Content:       "done",
		ChatID:        "oc_group",
		MentionOpenID: "ou_asker",
	}
	content, err := larkNotificationCardContent(note, "ou_owner", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, `\u003cat id=ou_asker\u003e\u003c/at\u003e`) {
		t.Fatalf("card content should mention asker, got %s", content)
	}
	if strings.Contains(content, `ou_owner`) {
		t.Fatalf("card content should not mention fallback receiver when asker is known, got %s", content)
	}
	tip, err := larkUpdateTipCardContent(1, larkNotificationMentionID(note, "ou_owner"), true)
	if err != nil || !strings.Contains(tip, `\u003cat id=ou_asker\u003e\u003c/at\u003e`) || strings.Contains(tip, `ou_owner`) {
		t.Fatalf("completion tip should mention the same asker as the card, got %s err=%v", tip, err)
	}
}

func TestLarkGroupNotificationDoesNotFallBackToDeveloperMention(t *testing.T) {
	note := WaitingNotification{ChatID: "oc_group"}
	if got := larkNotificationMentionID(note, "ou_developer"); got != "" {
		t.Fatalf("group notification without an asker must not mention the developer, got %q", got)
	}
}

func TestLarkNotificationCardContentPreservesTerminalLineBreaks(t *testing.T) {
	content, err := larkNotificationCardContent(WaitingNotification{
		SessionID: "sess-1",
		Name:      "A",
		Content:   "Select Model and Effort\n› 1. gpt-5.5 (current)\n  2. gpt-5.4\n  3. gpt-5.4-mini",
	}, "open-id", false)
	if err != nil {
		t.Fatal(err)
	}

	var card struct {
		Body struct {
			Elements []struct {
				Tag     string `json:"tag"`
				Content string `json:"content"`
			} `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(content), &card); err != nil {
		t.Fatal(err)
	}
	if len(card.Body.Elements) < 1 || card.Body.Elements[0].Tag != "markdown" {
		t.Fatalf("card should put terminal output in a markdown element, got %#v", card.Body.Elements)
	}
	want := "Select Model and Effort  \n\n› 1. gpt-5.5 (current)  \n  2. gpt-5.4  \n  3. gpt-5.4-mini"
	if card.Body.Elements[0].Content != want {
		t.Fatalf("terminal output should keep visible line breaks, got %q", card.Body.Elements[0].Content)
	}
}

func TestLarkTerminalMarkdownTextUsesHardBreaksOutsideCodeFences(t *testing.T) {
	got := larkTerminalMarkdownText(strings.Join([]string{
		"- 列表项",
		"    • 第二块",
		"",
		"```sh",
		"    • code content",
		"echo two",
		"```",
		"最后一行",
	}, "\n"))
	want := strings.Join([]string{
		"- 列表项  ",
		"",
		"• 第二块  ",
		"",
		"```sh",
		"    • code content",
		"echo two",
		"```",
		"最后一行",
	}, "\n")
	if got != want {
		t.Fatalf("markdown hard breaks = %q, want %q", got, want)
	}
}

func TestLarkTerminalMarkdownTextPreservesCodeCommandsWhenMergingWrappedLines(t *testing.T) {
	SetLarkNotifyMergeWrappedLines(true)
	t.Cleanup(func() { SetLarkNotifyMergeWrappedLines(false) })

	got := larkTerminalMarkdownText(strings.Join([]string{
		"启动命令：",
		"```bash",
		"iris",
		"iris --port 8080",
		"```",
		"完成。",
	}, "\n"))
	want := strings.Join([]string{
		"启动命令：  ",
		"```bash",
		"iris",
		"iris --port 8080",
		"```",
		"完成。",
	}, "\n")
	if got != want {
		t.Fatalf("Markdown code commands changed during wrapped-line merge: got %q want %q", got, want)
	}
}

func TestLarkNotificationCardKeepsHookMarkdownUnmerged(t *testing.T) {
	SetLarkNotifyMergeWrappedLines(true)
	t.Cleanup(func() { SetLarkNotifyMergeWrappedLines(false) })

	content, err := larkNotificationCardContent(WaitingNotification{
		SessionID:      "sess-hook-markdown",
		Name:           "Iris",
		Content:        "第一行\n第二行\n```bash\niris --port 8080\n```",
		SnapshotSource: "codex_hook:last_assistant_message",
	}, "open-id", false)
	if err != nil {
		t.Fatal(err)
	}
	var card struct {
		Body struct {
			Elements []struct {
				Content string `json:"content"`
			} `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(content), &card); err != nil {
		t.Fatal(err)
	}
	want := "第一行  \n第二行  \n```bash\niris --port 8080\n```"
	if len(card.Body.Elements) == 0 || card.Body.Elements[0].Content != want {
		t.Fatalf("hook Markdown should retain original line boundaries: %#v", card.Body.Elements)
	}
}

func TestLarkNotificationCardContentDoesNotWarnOnBufferFallback(t *testing.T) {
	content, err := larkNotificationCardContent(WaitingNotification{
		SessionID:      "sess-1",
		Name:           "A",
		Content:        "line one\nline two",
		SnapshotSource: "buffer",
	}, "open-id", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, "buffer 兜底") || strings.Contains(content, "细微差异") {
		t.Fatalf("buffer fallback card should not include a visible warning, got %s", content)
	}
}

func TestLarkNotificationCardContentUsesTaskStateInTitleOnly(t *testing.T) {
	content, err := larkNotificationCardContent(WaitingNotification{
		SessionID: "sess-1",
		Name:      "A",
		Content:   "still working",
		Running:   true,
	}, "ou_1", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, `"content":"A（Running）"`) {
		t.Fatalf("card content should include running title suffix, got %s", content)
	}
	if strings.Contains(content, "状态：") {
		t.Fatalf("running card should not duplicate status in the body, got %s", content)
	}
	if !strings.Contains(content, `"template":"blue"`) || !strings.Contains(content, `"tag":"column_set"`) {
		t.Fatalf("running card should use default header and native action rows, got %s", content)
	}
	stopped, err := larkNotificationCardContent(WaitingNotification{
		SessionID: "sess-1",
		Name:      "A",
		Content:   "done",
		Running:   false,
	}, "ou_1", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stopped, `"content":"A"`) || strings.Contains(stopped, "状态：") || !strings.Contains(stopped, `"template":"blue"`) || !strings.Contains(stopped, `"tag":"column_set"`) {
		t.Fatalf("completed card should restore the session title without duplicate body status, got %s", stopped)
	}
}

func TestLarkNotificationCardContentIncludesShortcutButtons(t *testing.T) {
	content, err := larkNotificationCardContent(WaitingNotification{
		SessionID:            "sess-1",
		Name:                 "A",
		Content:              RunningNotificationPlaceholder,
		Running:              true,
		DeveloperModeEnabled: true,
	}, "ou_1", false, LarkCustomShortcut{Label: "状态", Command: "git status"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "Ctrl-C") || !strings.Contains(content, "ctrl_c") || !strings.Contains(content, "重启 Agent") || !strings.Contains(content, "restart_agent") || !strings.Contains(content, "sess-1") {
		t.Fatalf("card content should include shortcut buttons, got %s", content)
	}
	if strings.Contains(content, `"content":"sess-1"`) {
		t.Fatalf("card content should not show session id as visible text, got %s", content)
	}
	if strings.Contains(content, "Ctrl-D") || strings.Contains(content, "ctrl_d") {
		t.Fatalf("card content should not include Ctrl-D, got %s", content)
	}
	if strings.Contains(content, "退出agent") || strings.Contains(content, "exit_agent") {
		t.Fatalf("card content should not include the removed exit agent shortcut, got %s", content)
	}
	if !strings.Contains(content, "刷新") || !strings.Contains(content, `"iris_action":"refresh"`) {
		t.Fatalf("card content should include manual refresh button, got %s", content)
	}
	if strings.Contains(content, "自动刷新") || strings.Contains(content, "停自动") || strings.Contains(content, `"iris_action":"toggle_auto_refresh"`) {
		t.Fatalf("card content should not include auto refresh button, got %s", content)
	}
	if !strings.Contains(content, `"content":"艾特模式：关"`) || !strings.Contains(content, `"iris_action":"toggle_mention_mode"`) {
		t.Fatalf("card content should include mention mode button, got %s", content)
	}
	if !strings.Contains(content, "删除会话") || !strings.Contains(content, `"iris_action":"delete_session"`) || !strings.Contains(content, `"type":"danger"`) {
		t.Fatalf("card content should include prominent delete button, got %s", content)
	}
	if !strings.Contains(content, `"confirm"`) || !strings.Contains(content, "确认删除会话？") || !strings.Contains(content, "机器人从当前群聊移除") {
		t.Fatalf("delete button should include confirmation dialog, got %s", content)
	}
	if !(strings.Index(content, `"content":"刷新"`) < strings.Index(content, `"content":"Ctrl-C"`) &&
		strings.Index(content, `"content":"艾特模式：关"`) < strings.Index(content, `"content":"Ctrl-C"`) &&
		strings.Index(content, `"content":"删除会话"`) < strings.Index(content, `"content":"Ctrl-C"`) &&
		strings.Index(content, `"content":"Ctrl-C"`) < strings.Index(content, `"content":"Esc"`) &&
		strings.Index(content, `"content":"Esc"`) < strings.Index(content, `"content":"Enter"`) &&
		strings.Index(content, `"content":"Enter"`) < strings.Index(content, `"iris_action":"custom_shortcut"`)) {
		t.Fatalf("refresh button should be first and custom shortcuts below system shortcuts, got %s", content)
	}
	if !strings.Contains(content, "状态") || !strings.Contains(content, `"iris_action":"custom_shortcut"`) || !strings.Contains(content, "git status") {
		t.Fatalf("card content should include custom shortcut row, got %s", content)
	}
	for _, label := range []string{"刷新", "开发者模式：开", "艾特模式：关", "删除会话", "Ctrl-C", "Esc", "Enter"} {
		if !strings.Contains(content, `"content":"`+label+`"`) {
			t.Fatalf("card content should include system shortcut %s, got %s", label, content)
		}
	}
	var card map[string]any
	if err := json.Unmarshal([]byte(content), &card); err != nil {
		t.Fatal(err)
	}
	systemRows := findLarkActionRows(card, "refresh")
	if len(systemRows) != 1 {
		t.Fatalf("system shortcut buttons should use one flowing row, got %#v", systemRows)
	}
	wantSystemColumns := []int{5}
	for i, row := range systemRows {
		columns, _ := row["columns"].([]any)
		if row["flex_mode"] != "flow" || len(columns) != wantSystemColumns[i] {
			t.Fatalf("system shortcut row %d should use responsive columns, got %#v", i, row)
		}
	}
	shortcutRows := findLarkActionRows(card, "shortcut")
	if len(shortcutRows) != 1 {
		t.Fatalf("terminal shortcuts should use their own row, got %#v", shortcutRows)
	}
	shortcutColumns, _ := shortcutRows[0]["columns"].([]any)
	if shortcutRows[0]["flex_mode"] != "flow" || len(shortcutColumns) != 3 {
		t.Fatalf("terminal shortcut row should contain Ctrl-C, Esc and Enter, got %#v", shortcutRows[0])
	}
	if strings.Count(content, `"type":"primary"`) != 1 || strings.Count(content, `"type":"default"`) < 7 {
		t.Fatalf("only refresh should be primary while secondary actions stay neutral, got %s", content)
	}
	if strings.Contains(content, `"border_color":"green"`) || strings.Contains(content, `"background_style":"green"`) {
		t.Fatalf("custom shortcut actions should use neutral shadow containers, got %s", content)
	}
	if !strings.Contains(content, `"content":"状态","tag":"plain_text"`) || !strings.Contains(content, `"type":"default"`) {
		t.Fatalf("custom shortcut label should use a neutral tiny button, got %s", content)
	}
	if !strings.Contains(content, `"size":"tiny"`) {
		t.Fatalf("card shortcut buttons should be small, got %s", content)
	}
	if strings.Contains(content, `"size":"small"`) {
		t.Fatalf("card shortcut buttons should all use tiny size, got %s", content)
	}
	if !strings.Contains(content, `"schema":"2.0"`) || !strings.Contains(content, `"behaviors"`) || !strings.Contains(content, `"callback"`) {
		t.Fatalf("card shortcut buttons should use card 2.0 callback behavior, got %s", content)
	}
	enabled, err := larkNotificationCardContent(WaitingNotification{
		SessionID:            "sess-1",
		Name:                 "A",
		Content:              RunningNotificationPlaceholder,
		Running:              true,
		AutoRefreshEnabled:   true,
		DeveloperModeEnabled: true,
	}, "ou_1", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(enabled, "自动刷新") || strings.Contains(enabled, "停自动") || strings.Contains(enabled, `"iris_action":"toggle_auto_refresh"`) {
		t.Fatalf("enabled auto refresh state should not restore the removed button, got %s", enabled)
	}
	mentionModeEnabled, err := larkNotificationCardContent(WaitingNotification{
		SessionID:            "sess-1",
		Name:                 "A",
		Content:              RunningNotificationPlaceholder,
		Running:              true,
		MentionModeEnabled:   true,
		DeveloperModeEnabled: true,
	}, "ou_1", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mentionModeEnabled, `"content":"艾特模式：开"`) || strings.Contains(mentionModeEnabled, "停艾特") {
		t.Fatalf("enabled mention mode card should show its current state, got %s", mentionModeEnabled)
	}
}

func TestLarkNotificationCardContentListsOnlyAvailableAgentOptions(t *testing.T) {
	content, err := larkNotificationCardContent(WaitingNotification{
		SessionID:            "sess-agent-select",
		Name:                 "Iris",
		Content:              "ready",
		DeveloperModeEnabled: true,
		AgentKind:            "claude",
		AgentOptions: []AgentOption{
			{ID: "codex", Label: "Codex", Kind: "codex", Command: CodexAgentCommand},
			{ID: "claude", Label: "Claude Code", Kind: "claude", Command: ClaudeAgentCommand},
		},
	}, "ou_1", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"content":"Agent"`, `"content":"Codex"`, `"content":"Claude Code"`, `"iris_action":"agent_select"`, `"initial_option":"claude"`} {
		if !strings.Contains(content, expected) {
			t.Fatalf("Agent selector missing %s: %s", expected, content)
		}
	}
	if strings.Contains(content, "dangerously-bypass") || strings.Contains(content, "dangerously-skip") {
		t.Fatalf("Agent commands must not be exposed in card payload: %s", content)
	}
	hidden, err := larkNotificationCardContent(WaitingNotification{
		SessionID: "sess-agent-select", Name: "Iris", Content: "ready", AgentKind: "claude",
		AgentOptions: []AgentOption{
			{ID: "codex", Label: "Codex", Kind: "codex", Command: CodexAgentCommand},
			{ID: "claude", Label: "Claude Code", Kind: "claude", Command: ClaudeAgentCommand},
		},
	}, "ou_1", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hidden, `"iris_action":"agent_select"`) {
		t.Fatalf("Agent selector must stay hidden outside developer mode: %s", hidden)
	}
}

func TestMatchingAgentOptionIDUsesStartCommandForLegacyCustomSession(t *testing.T) {
	options := []AgentOption{
		{ID: "codex", Label: "Codex", Kind: "codex", Command: CodexAgentCommand},
		{ID: "custom-aiden", Label: "aiden", Kind: "custom", Command: "aiden x codex --dangerously-bypass-approvals-and-sandbox"},
	}
	got := matchingAgentOptionID(Session{
		LastAgentKind:         "codex",
		LastAgentStartCommand: "aiden x codex --dangerously-bypass-approvals-and-sandbox",
	}, options)
	if got != "custom-aiden" {
		t.Fatalf("matched Agent ID = %q", got)
	}
}

func TestLarkNotificationCardContentDisabledRemovesButtons(t *testing.T) {
	content, err := larkNotificationCardContent(WaitingNotification{
		SessionID:            "sess-1",
		Name:                 "A",
		Content:              RunningNotificationPlaceholder,
		Running:              true,
		DeveloperModeEnabled: true,
		Disabled:             true,
	}, "ou_1", false, LarkCustomShortcut{Label: "状态", Command: "git status"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, "状态：") {
		t.Fatalf("disabled card should not include duplicate status text, got %s", content)
	}
	for _, forbidden := range []string{`"tag":"button"`, `"iris_action"`, `"custom_shortcut"`, `"content":"刷新"`, `"content":"Ctrl-C"`, "任务执行中"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("disabled card should remove buttons and running title, found %q in %s", forbidden, content)
		}
	}
}

func TestLarkNotificationCardContentWrapsCustomShortcutButtons(t *testing.T) {
	shortcuts := []LarkCustomShortcut{
		{Label: "cdx启动", Command: "cdx"},
		{Label: "查看状态", Command: "git status"},
		{Label: "拉取更新", Command: "git pull"},
		{Label: "运行测试", Command: "go test ./..."},
		{Label: "查看日志", Command: "tail -f app.log"},
		{Label: "重启服务", Command: "make restart"},
		{Label: "构建发布", Command: "make release"},
	}
	content, err := larkNotificationCardContent(WaitingNotification{
		SessionID:            "sess-1",
		Name:                 "A",
		Content:              RunningNotificationPlaceholder,
		Running:              true,
		DeveloperModeEnabled: true,
	}, "ou_1", false, shortcuts...)
	if err != nil {
		t.Fatal(err)
	}

	var card map[string]any
	if err := json.Unmarshal([]byte(content), &card); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, "系统操作") || strings.Contains(content, "自定义操作") || !strings.Contains(content, `"tag":"hr"`) || strings.Contains(content, `"interactive_container"`) {
		t.Fatalf("card should use an unlabeled native divider between action groups: %s", content)
	}
	customRows := findLarkActionRows(card, "custom_shortcut")
	if len(customRows) != 1 {
		t.Fatalf("custom shortcut buttons should use one flowing row, got %#v", customRows)
	}
	var labels []string
	for _, row := range customRows {
		columns, _ := row["columns"].([]any)
		if row["flex_mode"] != "flow" || len(columns) != len(shortcuts) {
			t.Fatalf("custom shortcut row should flow all buttons responsively, got %#v", row)
		}
		for _, rawColumn := range columns {
			column, _ := rawColumn.(map[string]any)
			elements, _ := column["elements"].([]any)
			button, _ := elements[0].(map[string]any)
			textValue, _ := button["text"].(map[string]any)
			labels = append(labels, textValue["content"].(string))
		}
	}
	if strings.Join(labels, ",") != "cdx启动,查看状态,拉取更新,运行测试,查看日志,重启服务,构建发布" {
		t.Fatalf("custom shortcut labels should keep order, got %#v", labels)
	}
}

func findLarkActionRows(value any, action string) []map[string]any {
	var rows []map[string]any
	var walk func(any)
	walk = func(value any) {
		switch typed := value.(type) {
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			if typed["tag"] == "column_set" && strings.Contains(mustJSONForTest(typed), `"iris_action":"`+action+`"`) {
				columns, _ := typed["columns"].([]any)
				if len(columns) > 0 {
					column, _ := columns[0].(map[string]any)
					elements, _ := column["elements"].([]any)
					if len(elements) > 0 {
						button, _ := elements[0].(map[string]any)
						if button["tag"] == "button" {
							rows = append(rows, typed)
							return
						}
					}
				}
			}
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
	return rows
}

func mustJSONForTest(value any) string {
	b, _ := json.Marshal(value)
	return string(b)
}

func TestRefreshNotificationMessageRetriesNotifierFailures(t *testing.T) {
	notifier := &flakyNotifier{
		recordingNotifier: recordingNotifier{messageID: "bot-card"},
		failNotify:        2,
	}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager: m,
		session: Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
	}
	setTrustedLegacyRoundFixture(rt, "$", "echo hello\r", "$ echo hello\nhello\n$")

	if err := rt.RefreshNotificationMessage("bot-card"); err != nil {
		t.Fatal(err)
	}
	if got := notifier.notifyAttemptCount(); got != 3 {
		t.Fatalf("refresh should retry notifier failures, got %d attempts", got)
	}
	notes := notifier.notes()
	if len(notes) != 1 || notes[0].MessageID != "bot-card" || notes[0].Content != "hello\n$" {
		t.Fatalf("refresh should patch card after retry, got %#v", notes)
	}
}

func TestUpdateNotificationRunningRetriesNotifierFailures(t *testing.T) {
	notifier := &flakyNotifier{failRunning: 2}
	m := NewManager(nil, nil, WithNotifier(notifier))
	rt := &RuntimeSession{
		manager:                  m,
		session:                  Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastNotifiedMessageID:    "bot-card",
		notificationPatchVersion: 1,
	}
	rt.updateNotificationRunning(WaitingNotification{
		SessionID:           "sess-1",
		Name:                "A",
		Content:             "done",
		MessageID:           "bot-card",
		NotificationVersion: 1,
	}, true)

	if got := notifier.runningAttemptCount(); got != 3 {
		t.Fatalf("running update should retry notifier failures, got %d attempts", got)
	}
	notes := notifier.runningNotes()
	if len(notes) != 1 || notes[0].MessageID != "bot-card" || !notes[0].Running {
		t.Fatalf("running update should succeed after retry, got %#v", notes)
	}
}

func TestLarkUpdateWaitingSendsTaskCompletedTip(t *testing.T) {
	notifier := &LarkAppNotifier{}
	notifier.client = fakeLarkSuccessClient(t)
	var sent int
	notifier.tipSender = func(messageID, chatID string, updateNo int) error {
		sent++
		return nil
	}

	result, err := notifier.updateWaiting(WaitingNotification{
		SessionID: "sess-1",
		Name:      "A",
		Content:   "updated",
		MessageID: "msg-1",
		UpdateNo:  2,
	}, "{}")
	if err != nil {
		t.Fatal(err)
	}
	if !result.TipSent || sent != 1 {
		t.Fatalf("card completion should create one completion-tip message, got result=%#v sent=%d", result, sent)
	}
	content, err := larkUpdateTipCardContent(2, "", false)
	if err != nil || !strings.Contains(content, `"content":"任务已完成"`) || strings.Contains(content, "已更新") {
		t.Fatalf("completion tip content is wrong: content=%s err=%v", content, err)
	}
}

func TestAutoRefreshNotificationMessageKeepsUpdateNumberButAllowsTip(t *testing.T) {
	notifier := &recordingNotifier{messageID: "bot-card"}
	m := NewManager(nil, nil, WithNotifier(notifier), WithNotificationUpdateCoalesce(0))
	rt := &RuntimeSession{
		manager:              m,
		session:              Session{ID: "sess-1", Name: "A", Status: StatusRunning, Live: true, NotifyOnWaiting: true},
		notificationUpdateNo: 2,
	}
	rt.MarkInputActivity("echo hello\r")
	rt.SetVisibleSnapshot("$ echo hello\nhello\n$")
	rt.mu.Lock()
	rt.session.Status = StatusRunning
	rt.mu.Unlock()

	if err := rt.AutoRefreshNotificationMessage("bot-card", 2); err != nil {
		t.Fatal(err)
	}

	notes := notifier.notes()
	if len(notes) != 1 {
		t.Fatalf("expected one auto refresh update, got %#v", notes)
	}
	if notes[0].UpdateNo != 2 || notes[0].SuppressUpdateTip {
		t.Fatalf("auto refresh should preserve update number and allow tip, got %#v", notes[0])
	}
	if rt.notificationUpdateNo != 2 {
		t.Fatalf("auto refresh should not increase update number, got %d", rt.notificationUpdateNo)
	}
}

type fakeLarkHTTPClient struct{}

func (fakeLarkHTTPClient) Do(*http.Request) (*http.Response, error) {
	header := make(http.Header)
	header.Set("Content-Type", "application/json; charset=utf-8")
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     header,
		Body:       io.NopCloser(bytes.NewBufferString(`{"code":0,"msg":"success","data":{}}`)),
	}, nil
}

func fakeLarkSuccessClient(t *testing.T) *lark.Client {
	t.Helper()
	return lark.NewClient("app", "secret", lark.WithHttpClient(fakeLarkHTTPClient{}))
}

type tokenRecoveryLarkHTTPClient struct {
	state *tokenRecoveryLarkHTTPState
}

type tokenRecoveryLarkHTTPState struct {
	mu         sync.Mutex
	patchCalls int
}

func (c *tokenRecoveryLarkHTTPClient) Do(req *http.Request) (*http.Response, error) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	body := `{"code":0,"msg":"success","data":{}}`
	if strings.Contains(req.URL.Path, "tenant_access_token") {
		body = `{"code":0,"msg":"success","tenant_access_token":"stale-token","expire":7200}`
	} else if req.Method == http.MethodPatch {
		c.state.patchCalls++
		if req.Header.Get("Authorization") != "Bearer fresh-token" {
			body = `{"code":99991663,"msg":"Invalid access token for authorization."}`
		}
	}
	header := make(http.Header)
	header.Set("Content-Type", "application/json; charset=utf-8")
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func TestLarkPatchRefreshesClientAfterAccessTokenExpires(t *testing.T) {
	state := &tokenRecoveryLarkHTTPState{}
	tokenFetches := 0
	httpClient := &tokenRecoveryLarkHTTPClient{state: state}
	notifier := &LarkAppNotifier{
		appID:     "token-recovery-test-app",
		appSecret: "secret",
		client:    lark.NewClient("token-recovery-test-app", "secret", lark.WithHttpClient(httpClient)),
		uncachedClient: lark.NewClient("token-recovery-test-app", "secret",
			lark.WithHttpClient(httpClient), lark.WithEnableTokenCache(false)),
		tipSent: make(map[string]map[int]bool),
	}
	notifier.tokenFetcher = func(context.Context) (string, error) {
		tokenFetches++
		return "fresh-token", nil
	}

	result, err := notifier.updateWaiting(WaitingNotification{
		SessionID: "sess-1",
		MessageID: "msg-1",
	}, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Updated {
		t.Fatalf("card should be updated after refreshing the expired token: %#v", result)
	}
	if tokenFetches != 1 {
		t.Fatalf("token fetches = %d, want one explicit refresh", tokenFetches)
	}
	if state.patchCalls != 3 {
		t.Fatalf("patch calls = %d, want SDK retry with stale cache plus immediate retry with explicit fresh token", state.patchCalls)
	}
	if got := notifier.tenantTokenSnapshot(); got != "fresh-token" {
		t.Fatalf("remembered tenant token = %q, want fresh token", got)
	}
}

func TestNotifyAfterStableDoesNotSendWhenNotificationDisabled(t *testing.T) {
	notifier := &recordingNotifier{}
	m := NewManager(nil, nil, WithNotifier(notifier))
	rt := &RuntimeSession{
		manager: m,
		session: Session{ID: "sess-1", Name: "A", Status: StatusRunning, Live: true, NotifyOnWaiting: false},
	}
	rt.MarkInputActivity("echo hello\r")
	rt.SetVisibleSnapshot("$ echo hello\nhello\n$")
	version := rt.stateVersion

	rt.notifyAfterStable(version)
	if got := rt.Snapshot().Status; got != StatusWaiting {
		t.Fatalf("stable output should still transition to waiting, got %s", got)
	}
	if got := notifier.count(); got != 0 {
		t.Fatalf("disabled notification should not send, got %d", got)
	}
}

func TestVisibleSnapshotSyncDoesNotScheduleNotification(t *testing.T) {
	notifier := &recordingNotifier{}
	m := NewManager(nil, nil, WithNotifier(notifier))
	rt := &RuntimeSession{
		manager: m,
		session: Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
	}
	rt.lastInputText = "echo hello"
	rt.visibleSnapshot = "$ echo hello\nhello\n$"

	rt.SetVisibleSnapshot("$ echo hello\nhello\n$ ")

	rt.mu.Lock()
	timer := rt.notifyStableTimer
	rt.mu.Unlock()
	if timer != nil {
		t.Fatal("snapshot-only sync should not schedule a notification timer")
	}
	time.Sleep(defaultFastWaitingTransition + 100*time.Millisecond)
	if got := notifier.count(); got != 0 {
		t.Fatalf("snapshot-only sync should not send notification, got %d", got)
	}
}

func TestRequestFreshSnapshotAsksSubscriberAndWaitsForUpdate(t *testing.T) {
	rt := &RuntimeSession{
		manager:     NewManager(nil, nil),
		session:     Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true},
		subscribers: make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	ch, cancel := rt.Subscribe()
	defer cancel()

	done := make(chan bool, 1)
	go func() {
		done <- rt.RequestFreshSnapshot(time.Second)
	}()

	select {
	case ev := <-ch:
		if ev.Type != RuntimeEventSnapshotRequest {
			t.Fatalf("event type = %q, want snapshot request", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("expected snapshot request event")
	}
	rt.SetVisibleSnapshot("> hello\n• world")
	select {
	case fresh := <-done:
		if !fresh {
			t.Fatal("request should report a fresh snapshot")
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot request did not finish")
	}
}

func TestRequestFreshSnapshotRefreshesExistingSnapshot(t *testing.T) {
	rt := &RuntimeSession{
		manager:                NewManager(nil, nil),
		session:                Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true},
		visibleSnapshot:        "old snapshot",
		visibleSnapshotVersion: 1,
		subscribers:            make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	ch, cancel := rt.Subscribe()
	defer cancel()

	done := make(chan bool, 1)
	go func() {
		done <- rt.RequestFreshSnapshot(time.Second)
	}()

	select {
	case ev := <-ch:
		if ev.Type != RuntimeEventSnapshotRequest {
			t.Fatalf("event type = %q, want snapshot request", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("expected snapshot request despite existing snapshot")
	}
	rt.SetVisibleSnapshot("fresh snapshot")
	select {
	case fresh := <-done:
		if !fresh {
			t.Fatal("request should report a fresh snapshot")
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot request did not finish")
	}
}

func TestRequestFreshSnapshotCorrelatesOneSourceAndRejectsLateResponses(t *testing.T) {
	rt := &RuntimeSession{
		manager:     NewManager(nil, nil),
		session:     Session{ID: "sess-correlated", Status: StatusWaiting, Live: true},
		subscribers: make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	first, cancelFirst := rt.Subscribe()
	defer cancelFirst()
	second, cancelSecond := rt.Subscribe()
	defer cancelSecond()

	done := make(chan bool, 1)
	go func() { done <- rt.RequestFreshSnapshot(time.Second) }()

	var event RuntimeEvent
	var other <-chan RuntimeEvent
	select {
	case event = <-first:
		other = second
	case event = <-second:
		other = first
	case <-time.After(time.Second):
		t.Fatal("expected a correlated snapshot request")
	}
	if event.Type != RuntimeEventSnapshotRequest || event.RequestID == "" {
		t.Fatalf("snapshot event = %#v, want request id", event)
	}
	select {
	case duplicate := <-other:
		t.Fatalf("snapshot request should select one rendering source, duplicate=%#v", duplicate)
	default:
	}

	rt.SetVisibleSnapshotResponse("wrong request", "browser:buffer", "snapshot-wrong")
	rt.SetVisibleSnapshotResponse("wrong source", "headless:buffer", event.RequestID)
	select {
	case <-done:
		t.Fatal("wrong request/source must not satisfy the snapshot waiter")
	case <-time.After(40 * time.Millisecond):
	}

	rt.SetVisibleSnapshotResponse("fresh correlated snapshot", "browser:buffer", event.RequestID)
	select {
	case fresh := <-done:
		if !fresh {
			t.Fatal("correlated response should be fresh")
		}
	case <-time.After(time.Second):
		t.Fatal("correlated snapshot request did not finish")
	}

	rt.SetVisibleSnapshotResponse("late stale overwrite", "browser:buffer", event.RequestID)
	rt.mu.Lock()
	visible := rt.visibleSnapshot
	rt.mu.Unlock()
	if visible != "fresh correlated snapshot" {
		t.Fatalf("late response overwrote the accepted snapshot: %q", visible)
	}
}

func TestConcurrentSnapshotResponsesCannotOverwriteNewerRequest(t *testing.T) {
	rt := &RuntimeSession{
		manager:     NewManager(nil, nil),
		session:     Session{ID: "sess-snapshot-order", Status: StatusWaiting, Live: true},
		subscribers: make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	subscriber, cancel := rt.Subscribe()
	defer cancel()

	results := make(chan bool, 2)
	go func() { results <- rt.RequestFreshSnapshot(time.Second) }()
	go func() { results <- rt.RequestFreshSnapshot(time.Second) }()

	events := make([]RuntimeEvent, 0, 2)
	for len(events) < 2 {
		select {
		case event := <-subscriber:
			if event.Type == RuntimeEventSnapshotRequest {
				events = append(events, event)
			}
		case <-time.After(time.Second):
			t.Fatal("expected two concurrent snapshot requests")
		}
	}
	seq := func(event RuntimeEvent) int64 {
		var value int64
		_, _ = fmt.Sscanf(event.RequestID[strings.LastIndex(event.RequestID, "-")+1:], "%d", &value)
		return value
	}
	older, newer := events[0], events[1]
	if seq(older) > seq(newer) {
		older, newer = newer, older
	}

	// A faster response for the newer request becomes canonical. The older
	// request must be completed without being allowed to overwrite it later.
	rt.SetVisibleSnapshotResponseFrom("newer snapshot", "browser:buffer", newer.RequestID, subscriber)
	rt.SetVisibleSnapshotResponseFrom("older late snapshot", "browser:buffer", older.RequestID, subscriber)
	first, second := <-results, <-results
	if !first || !second {
		t.Fatalf("the newer canonical snapshot should satisfy both callers, got %v and %v", first, second)
	}
	rt.mu.Lock()
	visible := rt.visibleSnapshot
	rt.mu.Unlock()
	if visible != "newer snapshot" {
		t.Fatalf("older request overwrote the canonical snapshot: %q", visible)
	}
}

func TestCanceledSnapshotRequestDoesNotUseNewRoundAppliedSequence(t *testing.T) {
	rt := &RuntimeSession{
		manager:     NewManager(nil, nil),
		session:     Session{ID: "sess-snapshot-new-round", Status: StatusWaiting, Live: true},
		subscribers: make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	subscriber, cancel := rt.Subscribe()
	defer cancel()

	result := make(chan bool, 1)
	go func() { result <- rt.RequestFreshSnapshot(time.Second) }()

	var request RuntimeEvent
	select {
	case request = <-subscriber:
		if request.Type != RuntimeEventSnapshotRequest || request.RequestID == "" {
			t.Fatalf("snapshot event = %#v, want correlated request", request)
		}
	case <-time.After(time.Second):
		t.Fatal("expected snapshot request")
	}

	// Submitting input cancels the old round's request. Before its waiter gets
	// the lock again, a newer round may already have applied a higher request
	// sequence. That newer sequence must not make the canceled request report
	// success.
	rt.mu.Lock()
	rt.snapshotRoundGeneration++
	rt.cancelSnapshotRequestsLocked(false)
	rt.latestAppliedSnapshotRequestID = rt.nextSnapshotRequestID + 1
	rt.mu.Unlock()

	select {
	case fresh := <-result:
		if fresh {
			t.Fatal("a newer round's applied sequence must not satisfy the canceled snapshot request")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled snapshot request did not finish promptly")
	}
}

func TestSnapshotRequestReassignsWhenSelectedSubscriberDisconnects(t *testing.T) {
	rt := &RuntimeSession{
		manager:     NewManager(nil, nil),
		session:     Session{ID: "sess-snapshot-reassign", Status: StatusWaiting, Live: true},
		subscribers: make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	first, cancelFirst := rt.Subscribe()
	second, cancelSecond := rt.Subscribe()
	defer cancelFirst()
	defer cancelSecond()

	done := make(chan bool, 1)
	go func() { done <- rt.RequestFreshSnapshot(time.Second) }()

	var selected, replacement chan RuntimeEvent
	var cancelSelected func()
	var event RuntimeEvent
	select {
	case event = <-first:
		selected, replacement, cancelSelected = first, second, cancelFirst
	case event = <-second:
		selected, replacement, cancelSelected = second, first, cancelSecond
	case <-time.After(time.Second):
		t.Fatal("expected initial snapshot request")
	}
	_ = selected
	cancelSelected()

	select {
	case reassigned := <-replacement:
		if reassigned.RequestID != event.RequestID {
			t.Fatalf("reassigned request id = %q, want %q", reassigned.RequestID, event.RequestID)
		}
		rt.SetVisibleSnapshotResponseFrom("replacement snapshot", "browser:buffer", reassigned.RequestID, replacement)
	case <-time.After(time.Second):
		t.Fatal("disconnected snapshot request was not reassigned")
	}
	select {
	case fresh := <-done:
		if !fresh {
			t.Fatal("replacement renderer response should satisfy the request")
		}
	case <-time.After(time.Second):
		t.Fatal("reassigned request did not finish")
	}
}

func TestSnapshotResponsesRequireRequestIDAndLiveSession(t *testing.T) {
	terminal := &recordingTerminal{readCh: make(chan []byte)}
	rt := &RuntimeSession{
		manager:     NewManager(nil, nil),
		session:     Session{ID: "sess-snapshot-close", Status: StatusWaiting, Live: true},
		terminal:    terminal,
		subscribers: make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	rt.SetVisibleSnapshot("trusted snapshot")
	rt.SetVisibleSnapshotResponse("uncorrelated overwrite", "browser:buffer", "")
	rt.mu.Lock()
	if rt.visibleSnapshot != "trusted snapshot" {
		t.Fatalf("missing request id overwrote snapshot: %q", rt.visibleSnapshot)
	}
	rt.mu.Unlock()

	subscriber, _ := rt.Subscribe()
	done := make(chan bool, 1)
	go func() { done <- rt.RequestFreshSnapshot(5 * time.Second) }()
	event := <-subscriber
	rt.Close()
	select {
	case fresh := <-done:
		if fresh {
			t.Fatal("closing the session must cancel, not satisfy, a pending snapshot request")
		}
	case <-time.After(time.Second):
		t.Fatal("session close did not promptly cancel snapshot waiter")
	}
	rt.SetVisibleSnapshotResponseFrom("late closed-session overwrite", "browser:buffer", event.RequestID, subscriber)
	rt.mu.Lock()
	visible := rt.visibleSnapshot
	rt.mu.Unlock()
	if visible != "trusted snapshot" {
		t.Fatalf("late response updated a closed session: %q", visible)
	}
}

func TestNewInputCancelsSnapshotRequestFromPreviousRound(t *testing.T) {
	rt := &RuntimeSession{
		manager:     NewManager(nil, nil),
		session:     Session{ID: "sess-snapshot-round", Status: StatusWaiting, Live: true},
		subscribers: make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	rt.SetVisibleSnapshot("trusted previous-round snapshot")
	subscriber, cancel := rt.Subscribe()
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- rt.RequestFreshSnapshot(5 * time.Second) }()
	event := <-subscriber

	rt.MarkStructuredInputActivity("new round input")
	select {
	case fresh := <-done:
		if fresh {
			t.Fatal("a previous-round request must be cancelled, not treated as the new baseline")
		}
	case <-time.After(time.Second):
		t.Fatal("new input did not promptly cancel the previous round snapshot request")
	}
	rt.SetVisibleSnapshotResponseFrom("late previous-round snapshot", "browser:buffer", event.RequestID, subscriber)
	rt.mu.Lock()
	visible := rt.visibleSnapshot
	rt.mu.Unlock()
	if visible != "trusted previous-round snapshot" {
		t.Fatalf("late previous-round response overwrote the new round boundary: %q", visible)
	}
}

func TestWriteInputWithSnapshotBaselineBindsRoundBeforeCancelingOldRequests(t *testing.T) {
	terminal := &lifecycleRecordingTerminal{}
	rt := &RuntimeSession{
		manager:     NewManager(nil, nil),
		session:     Session{ID: "sess-atomic-baseline", Status: StatusWaiting, Live: true},
		terminal:    terminal,
		subscribers: make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	rt.SetVisibleSnapshot("old canonical snapshot")
	subscriber, cancel := rt.Subscribe()
	defer cancel()

	freshResult := make(chan bool, 1)
	go func() { freshResult <- rt.RequestFreshSnapshot(time.Second) }()
	event := <-subscriber
	if event.Type != RuntimeEventSnapshotRequest || event.RequestID == "" {
		t.Fatalf("snapshot event = %#v", event)
	}

	baseline := "old output\n› typed command\ngpt-5.6-sol high fast · ~/project"
	if err := rt.WriteInputWithSnapshotBaseline("typed command\r", baseline, "browser:buffer"); err != nil {
		t.Fatal(err)
	}
	select {
	case fresh := <-freshResult:
		if fresh {
			t.Fatal("the previous round's request must be canceled, not satisfied by the input baseline")
		}
	case <-time.After(time.Second):
		t.Fatal("the previous round's request was not canceled")
	}

	rt.SetVisibleSnapshotResponseFrom("late stale screen", "browser:buffer", event.RequestID, subscriber)
	rt.mu.Lock()
	visible := rt.visibleSnapshot
	roundStart := rt.snapshotAtRoundStart
	lastInput := rt.lastInputText
	rt.mu.Unlock()
	if visible != baseline || roundStart != baseline || lastInput != "typed command" {
		t.Fatalf("atomic baseline was overwritten: visible=%q round=%q input=%q", visible, roundStart, lastInput)
	}
	if got := terminal.writtenText(); got != "typed command\r" {
		t.Fatalf("terminal input = %q", got)
	}
}

func TestLegacySnapshotResponseUsesUniqueBoundSubscriberRequest(t *testing.T) {
	rt := &RuntimeSession{
		manager:     NewManager(nil, nil),
		session:     Session{ID: "sess-legacy-snapshot", Status: StatusWaiting, Live: true},
		subscribers: make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	subscriber, cancel := rt.Subscribe()
	defer cancel()
	result := make(chan bool, 1)
	go func() { result <- rt.RequestFreshSnapshot(time.Second) }()
	event := <-subscriber
	if event.RequestID == "" {
		t.Fatal("expected a correlated server request")
	}

	// A page opened before request IDs were introduced responds on the exact
	// WebSocket but omits request_id. One unambiguous pending request is safe.
	rt.SetVisibleSnapshotResponseFrom("legacy compatible snapshot", "browser:buffer", "", subscriber)
	select {
	case fresh := <-result:
		if !fresh {
			t.Fatal("the uniquely bound legacy response should satisfy the request")
		}
	case <-time.After(time.Second):
		t.Fatal("legacy snapshot response did not wake its request")
	}
	rt.mu.Lock()
	visible := rt.visibleSnapshot
	rt.mu.Unlock()
	if visible != "legacy compatible snapshot" {
		t.Fatalf("legacy snapshot was not applied: %q", visible)
	}
}

func TestLegacySnapshotResponseRejectsAmbiguousRequests(t *testing.T) {
	rt := &RuntimeSession{
		manager:     NewManager(nil, nil),
		session:     Session{ID: "sess-legacy-ambiguous", Status: StatusWaiting, Live: true},
		subscribers: make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	rt.SetVisibleSnapshot("trusted snapshot")
	subscriber, cancel := rt.Subscribe()
	defer cancel()
	results := make(chan bool, 2)
	go func() { results <- rt.RequestFreshSnapshot(time.Second) }()
	first := <-subscriber
	go func() { results <- rt.RequestFreshSnapshot(time.Second) }()
	second := <-subscriber

	rt.SetVisibleSnapshotResponseFrom("ambiguous legacy overwrite", "browser:buffer", "", subscriber)
	rt.mu.Lock()
	visible := rt.visibleSnapshot
	rt.mu.Unlock()
	if visible != "trusted snapshot" {
		t.Fatalf("an uncorrelated response with two candidates must be rejected: %q", visible)
	}

	rt.SetVisibleSnapshotResponseFrom("first correlated", "browser:buffer", first.RequestID, subscriber)
	rt.SetVisibleSnapshotResponseFrom("second correlated", "browser:buffer", second.RequestID, subscriber)
	for i := 0; i < 2; i++ {
		select {
		case fresh := <-results:
			if !fresh {
				t.Fatal("correlated cleanup response should satisfy both requests")
			}
		case <-time.After(time.Second):
			t.Fatal("correlated request did not finish")
		}
	}
}

func TestBrowserInputOwnsSnapshotAndResizeAcrossMultipleComputers(t *testing.T) {
	terminal := &resizeRecordingTerminal{}
	rt := &RuntimeSession{
		manager:     NewManager(nil, nil),
		session:     Session{ID: "sess-browser-owner", Status: StatusWaiting, Live: true},
		terminal:    terminal,
		subscribers: make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	idleBrowser, cancelIdle := rt.Subscribe()
	defer cancelIdle()
	activeBrowser, cancelActive := rt.Subscribe()
	defer cancelActive()

	if err := rt.WriteInputFrom("x", activeBrowser); err != nil {
		t.Fatal(err)
	}
	result := make(chan bool, 1)
	go func() { result <- rt.RequestFreshSnapshot(time.Second) }()
	var request RuntimeEvent
	select {
	case request = <-activeBrowser:
		if request.Type != RuntimeEventSnapshotRequest {
			t.Fatalf("active browser event = %#v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("the browser that typed should receive the snapshot request")
	}
	select {
	case event := <-idleBrowser:
		t.Fatalf("idle browser unexpectedly received the active round request: %#v", event)
	default:
	}

	if err := rt.ResizeFrom(90, 25, idleBrowser); err != nil {
		t.Fatal(err)
	}
	if terminal.cols != 0 || terminal.rows != 0 {
		t.Fatalf("idle browser changed the shared PTY size to %dx%d", terminal.cols, terminal.rows)
	}
	if err := rt.ResizeFrom(132, 40, activeBrowser); err != nil {
		t.Fatal(err)
	}
	if terminal.cols != 132 || terminal.rows != 40 {
		t.Fatalf("active browser resize = %dx%d", terminal.cols, terminal.rows)
	}

	rt.SetVisibleSnapshotResponseFrom("active browser snapshot", "browser:buffer", request.RequestID, activeBrowser)
	select {
	case fresh := <-result:
		if !fresh {
			t.Fatal("active browser response should satisfy the request")
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot request did not finish")
	}
}

func TestRuntimeSessionCloseIsTerminalForSnapshotsInputAndOutput(t *testing.T) {
	terminal := &lifecycleRecordingTerminal{}
	ended := 0
	rt := &RuntimeSession{
		manager: NewManager(nil, nil, WithSessionEnded(func(sessionID string) {
			if sessionID != "sess-closed-lifecycle" {
				t.Errorf("ended session = %q", sessionID)
			}
			ended++
		})),
		session:     Session{ID: "sess-closed-lifecycle", Status: StatusWaiting, Live: true},
		terminal:    terminal,
		subscribers: make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	rt.SetVisibleSnapshot("trusted before close")
	rt.Close()
	rt.Close()

	if snapshot := rt.Snapshot(); snapshot.Live {
		t.Fatalf("closed session is still live: %#v", snapshot)
	}
	if err := rt.WriteInput("ignored\r"); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("closed input error = %v, want io.ErrClosedPipe", err)
	}
	rt.MarkInputActivity("ignored\r")
	rt.MarkStructuredInputActivity("ignored structured")
	rt.SetVisibleSnapshot("ignored snapshot")
	rt.HandleOutput([]byte("ignored output"))

	closedSubscriber, closedCancel := rt.Subscribe()
	defer closedCancel()
	if _, ok := <-closedSubscriber; ok {
		t.Fatal("a subscriber created after Close must already be closed")
	}
	rt.mu.Lock()
	visible := rt.visibleSnapshot
	lastInput := rt.lastInputText
	outputLen := len(rt.output)
	subscriberCount := len(rt.subscribers)
	rt.mu.Unlock()
	if visible != "trusted before close" || lastInput != "" || outputLen != 0 || subscriberCount != 0 {
		t.Fatalf("closed runtime mutated: visible=%q input=%q output=%d subscribers=%d", visible, lastInput, outputLen, subscriberCount)
	}
	if terminal.closeCalls() != 1 || terminal.writtenText() != "" {
		t.Fatalf("terminal close/write state: closes=%d written=%q", terminal.closeCalls(), terminal.writtenText())
	}
	rt.markTerminal(StatusExited, 0)
	if ended != 1 {
		t.Fatalf("session-ended callback count = %d, want 1", ended)
	}
}

func TestRequestFreshSnapshotPrefersRealBrowserOverHeadless(t *testing.T) {
	rt := &RuntimeSession{
		manager:     NewManager(nil, nil),
		session:     Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true},
		subscribers: make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	headlessCh, headlessCancel := rt.SubscribeWithMode(true)
	defer headlessCancel()
	realCh, realCancel := rt.SubscribeWithMode(false)
	defer realCancel()

	done := make(chan bool, 1)
	go func() {
		done <- rt.RequestFreshSnapshot(time.Second)
	}()

	select {
	case ev := <-realCh:
		if ev.Type != RuntimeEventSnapshotRequest {
			t.Fatalf("event type = %q, want snapshot request", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("expected real browser snapshot request")
	}
	select {
	case ev := <-headlessCh:
		t.Fatalf("headless browser should not receive notification snapshot request while a real browser is active: %#v", ev)
	default:
	}
	rt.SetVisibleSnapshotWithSource("fresh from browser", "browser:buffer")
	select {
	case fresh := <-done:
		if !fresh {
			t.Fatal("request should report a fresh browser snapshot")
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot request did not finish")
	}
}

func TestBrowserSnapshotIgnoredWhenHeadlessActive(t *testing.T) {
	rt := &RuntimeSession{
		manager:         NewManager(nil, nil),
		session:         Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true},
		visibleSnapshot: "headless baseline",
		subscribers:     make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	_, headlessCancel := rt.SubscribeWithMode(true)
	defer headlessCancel()

	rt.SetVisibleSnapshotWithSource("stale browser", "browser:buffer")
	if rt.visibleSnapshot != "headless baseline" {
		t.Fatalf("browser snapshot should be ignored while headless is active, got %q", rt.visibleSnapshot)
	}
	rt.SetVisibleSnapshotWithSource("fresh headless", "headless:buffer")
	if rt.visibleSnapshot != "fresh headless" {
		t.Fatalf("headless snapshot should be accepted, got %q", rt.visibleSnapshot)
	}
}

func TestResizeBroadcastsTerminalSizeToHeadlessOnly(t *testing.T) {
	terminal := &resizeRecordingTerminal{}
	rt := &RuntimeSession{
		manager:      NewManager(nil, nil),
		session:      Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true},
		terminal:     terminal,
		subscribers:  make(map[chan RuntimeEvent]runtimeSubscriber),
		terminalCols: defaultTerminalCols,
		terminalRows: defaultTerminalRows,
	}
	headlessCh, headlessCancel := rt.SubscribeWithMode(true)
	defer headlessCancel()
	realCh, realCancel := rt.SubscribeWithMode(false)
	defer realCancel()

	if err := rt.Resize(150, 44); err != nil {
		t.Fatal(err)
	}

	if terminal.cols != 150 || terminal.rows != 44 {
		t.Fatalf("terminal resize = %dx%d, want 150x44", terminal.cols, terminal.rows)
	}
	select {
	case ev := <-headlessCh:
		if ev.Type != RuntimeEventTerminalResize || ev.Cols != 150 || ev.Rows != 44 {
			t.Fatalf("headless resize event = %#v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("expected resize event for headless subscriber")
	}
	select {
	case ev := <-realCh:
		t.Fatalf("real browser should not receive headless resize sync event: %#v", ev)
	default:
	}

	if err := rt.Resize(150, 44); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-headlessCh:
		t.Fatalf("same size should not rebroadcast resize: %#v", ev)
	default:
	}
}

func TestRequestFreshSnapshotUsesRealBrowserInsteadOfStartingHeadless(t *testing.T) {
	needed := make(chan string, 1)
	rt := &RuntimeSession{
		manager: NewManager(nil, nil, WithBrowserNeeded(func(sessionID string) {
			needed <- sessionID
		})),
		session:     Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true},
		subscribers: make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	realCh, realCancel := rt.SubscribeWithMode(false)
	defer realCancel()

	done := make(chan bool, 1)
	go func() {
		done <- rt.RequestFreshSnapshot(80 * time.Millisecond)
	}()

	select {
	case ev := <-realCh:
		if ev.Type != RuntimeEventSnapshotRequest {
			t.Fatalf("event type = %q, want snapshot request", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("expected real browser snapshot request")
	}
	select {
	case got := <-needed:
		t.Fatalf("headless should not start while a real browser is active, got session %q", got)
	default:
	}
	rt.SetVisibleSnapshotWithSource("fresh from browser", "browser:buffer")
	select {
	case fresh := <-done:
		if !fresh {
			t.Fatal("request should report a fresh browser snapshot")
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot request did not finish")
	}
}

func TestRequestFreshSnapshotFallsBackToHeadlessWhenRealBrowserStalls(t *testing.T) {
	events := make(chan string, 2)
	rt := &RuntimeSession{
		manager: NewManager(nil, nil,
			WithBrowserStopped(func(sessionID string) {
				events <- "stop:" + sessionID
			}),
			WithBrowserNeeded(func(sessionID string) {
				events <- "start:" + sessionID
			}),
		),
		session:     Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true},
		subscribers: make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	realCh, realCancel := rt.SubscribeWithMode(false)
	defer realCancel()

	done := make(chan bool, 1)
	go func() {
		done <- rt.RequestFreshSnapshot(time.Second)
	}()

	select {
	case ev := <-realCh:
		if ev.Type != RuntimeEventSnapshotRequest {
			t.Fatalf("event type = %q, want snapshot request", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("expected real browser snapshot request")
	}
	for _, expected := range []string{"stop:sess-1", "start:sess-1"} {
		select {
		case got := <-events:
			if got != expected {
				t.Fatalf("browser event = %q, want %q", got, expected)
			}
		case <-time.After(time.Second):
			t.Fatalf("expected browser event %q", expected)
		}
	}

	headlessCh, headlessCancel := rt.SubscribeWithMode(true)
	defer headlessCancel()
	select {
	case ev := <-headlessCh:
		if ev.Type != RuntimeEventSnapshotRequest {
			t.Fatalf("headless event type = %q, want snapshot request", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("expected headless snapshot request")
	}
	rt.SetVisibleSnapshotWithSource("fresh from headless", "headless:buffer")
	select {
	case fresh := <-done:
		if !fresh {
			t.Fatal("request should report a fresh headless snapshot")
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot request did not finish")
	}
}

func TestRequestFreshSnapshotRestartsStaleHeadlessWhenNoSubscriber(t *testing.T) {
	events := make(chan string, 2)
	rt := &RuntimeSession{
		manager: NewManager(nil, nil,
			WithHeadlessSnapshotTimeout(80*time.Millisecond),
			WithBrowserStopped(func(sessionID string) {
				events <- "stop:" + sessionID
			}),
			WithBrowserNeeded(func(sessionID string) {
				events <- "start:" + sessionID
			}),
		),
		session:     Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true},
		subscribers: make(map[chan RuntimeEvent]runtimeSubscriber),
	}

	done := make(chan bool, 1)
	go func() {
		done <- rt.RequestFreshSnapshot(10 * time.Millisecond)
	}()

	want := []string{"stop:sess-1", "start:sess-1"}
	for _, expected := range want {
		select {
		case got := <-events:
			if got != expected {
				t.Fatalf("browser event = %q, want %q", got, expected)
			}
		case <-time.After(time.Second):
			t.Fatalf("expected browser event %q", expected)
		}
	}
	select {
	case <-done:
		t.Fatal("configured headless startup timeout should extend the caller deadline")
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case fresh := <-done:
		if fresh {
			t.Fatal("request should not become fresh without a headless snapshot")
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot request did not finish")
	}
}

func TestUpdateNotifyOnWaitingControlsHeadlessLifecycle(t *testing.T) {
	started := make(chan string, 1)
	stopped := make(chan string, 1)
	m := NewManager(nil, &recordingLauncher{},
		WithBrowserNeeded(func(sessionID string) {
			started <- sessionID
		}),
		WithBrowserStopped(func(sessionID string) {
			stopped <- sessionID
		}),
	)
	sess, err := m.CreateSession(context.Background(), "A")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.DeleteSession(context.Background(), sess.ID) }()

	if _, ok, err := m.UpdateNotifyOnWaiting(context.Background(), sess.ID, true); err != nil || !ok {
		t.Fatalf("enable notify ok=%v err=%v", ok, err)
	}
	select {
	case got := <-started:
		if got != sess.ID {
			t.Fatalf("started session = %q, want %q", got, sess.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("expected headless start when notifications are enabled")
	}

	if _, ok, err := m.UpdateNotifyOnWaiting(context.Background(), sess.ID, false); err != nil || !ok {
		t.Fatalf("disable notify ok=%v err=%v", ok, err)
	}
	select {
	case got := <-stopped:
		if got != sess.ID {
			t.Fatalf("stopped session = %q, want %q", got, sess.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("expected headless stop when notifications are disabled")
	}
}

func TestRealBrowserSubscriptionReportsActiveBrowser(t *testing.T) {
	active := make(chan string, 1)
	rt := &RuntimeSession{
		manager: NewManager(nil, nil, WithBrowserActive(func(sessionID string) {
			active <- sessionID
		})),
		session:     Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true},
		subscribers: make(map[chan RuntimeEvent]runtimeSubscriber),
	}
	_, cancel := rt.SubscribeWithMode(false)
	defer cancel()
	select {
	case got := <-active:
		if got != "sess-1" {
			t.Fatalf("active browser session = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("expected active browser callback")
	}
}

func TestNotifyIfStillWaitingRetriesUntilCurrentRoundIsReady(t *testing.T) {
	notifier := &recordingNotifier{}
	m := NewManager(nil, nil, WithNotifier(notifier))
	rt := &RuntimeSession{
		manager: m,
		session: Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
	}
	rt.notifyVersion = 1
	setTrustedLegacyRoundFixture(rt, "›", "今天天气怎么样\r", "> 今天天气怎么样\n• Working (8s • esc to interrupt)")
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	rt.notifyVersion = 2
	version := rt.notifyVersion
	rt.mu.Unlock()

	go rt.notifyIfStillWaiting(version)
	time.Sleep(500 * time.Millisecond)
	if got := notifier.count(); got != 0 {
		t.Fatalf("notifier should not send while current round has only transient content, got %d", got)
	}

	rt.SetVisibleSnapshot("> 今天天气怎么样\n• 你想查哪个城市的天气？例如：上海、北京。")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if notifier.count() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	notes := notifier.notes()
	if len(notes) != 1 {
		t.Fatalf("expected retry to send once after snapshot becomes ready, got %#v", notes)
	}
	if notes[0].Content != "• 你想查哪个城市的天气？例如：上海、北京。" {
		t.Fatalf("unexpected retry notification content: %q", notes[0].Content)
	}
}

func TestManualRefreshKeepsWorkingBelowInputAnchor(t *testing.T) {
	notifier := &recordingNotifier{messageID: "bot-card"}
	rt := &RuntimeSession{
		manager: NewManager(nil, nil, WithNotifier(notifier)),
		session: Session{ID: "sess-1", Name: "减肥", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
	}
	rt.MarkInputActivity("你好\r")
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	rt.mu.Unlock()
	rt.SetVisibleSnapshot(strings.Join([]string{
		"› 你好",
		"• Working (1s • esc to interrupt)",
		"1 background terminal running · /ps to view · /stop to close",
		"› Run /review on my current changes",
		"gpt-5.5 xhigh fast · ~/Iris_Workspace/减肥",
	}, "\n"))

	if err := rt.RefreshNotificationMessage("bot-card"); err != nil {
		t.Fatal(err)
	}
	notes := notifier.notes()
	if len(notes) != 1 {
		t.Fatalf("status-only manual refresh should update the card once, got %#v", notes)
	}
	want := strings.Join([]string{
		"• Working (1s • esc to interrupt)",
		"1 background terminal running · /ps to view · /stop to close",
		"› Run /review on my current changes",
	}, "\n")
	if notes[0].Content != want {
		t.Fatalf("manual refresh must keep the terminal state below the input anchor:\n%q\nwant:\n%q", notes[0].Content, want)
	}
}

func TestManualRefreshEmptyAnchorReturnsConfiguredLastOneHundredLines(t *testing.T) {
	SetLarkNotifyFallbackTailLines(100)
	t.Cleanup(func() { SetLarkNotifyFallbackTailLines(defaultFallbackTailLines) })
	notifier := &recordingNotifier{messageID: "bot-card"}
	lines := make([]string, 105)
	for i := range lines {
		lines[i] = fmt.Sprintf("visible terminal line %03d", i+1)
	}
	rt := &RuntimeSession{
		manager:                NewManager(nil, nil, WithNotifier(notifier)),
		session:                Session{ID: "sess-1", Name: "A", Status: StatusWaiting, Live: true, NotifyOnWaiting: true},
		lastInputText:          "missing anchor input",
		lastNotifiedMessageID:  "bot-card",
		visibleSnapshot:        strings.Join(lines, "\n"),
		visibleSnapshotVersion: 1,
	}

	if err := rt.RefreshNotificationMessage("bot-card"); err != nil {
		t.Fatal(err)
	}
	notes := notifier.notes()
	if len(notes) != 1 {
		t.Fatalf("expected one refresh update, got %#v", notes)
	}
	gotLines := strings.Split(notes[0].Content, "\n")
	if len(gotLines) != 100 || gotLines[0] != "visible terminal line 006" || gotLines[99] != "visible terminal line 105" {
		t.Fatalf("manual refresh fallback must contain the newest 100 lines, got first=%q last=%q count=%d", gotLines[0], gotLines[len(gotLines)-1], len(gotLines))
	}
}

func TestStartupPresetNotificationSuppressionSkipsExternalNotifyAndHook(t *testing.T) {
	notifier := &recordingNotifier{}
	m := NewManager(nil, nil, WithNotifier(notifier))
	hookCh := make(chan string, 1)
	m.SetNotificationSentHook(func(sessionID string) {
		hookCh <- sessionID
	})
	rt := &RuntimeSession{
		manager: m,
		session: Session{ID: "sess-1", Name: "A", Status: StatusRunning, Live: true, NotifyOnWaiting: true},
	}
	rt.MarkInputActivity("echo setup\r")
	rt.SetVisibleSnapshot("$ echo setup\nsetup done\n$")
	rt.SuppressStartupNotifications()
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	version := rt.notifyVersion
	rt.mu.Unlock()

	rt.notifyIfStillWaiting(version)
	if got := notifier.count(); got != 0 {
		t.Fatalf("startup preset notification should be suppressed, got %d", got)
	}
	select {
	case got := <-hookCh:
		t.Fatalf("suppressed startup notification should not run hook, got %q", got)
	default:
	}

	rt.MarkInputActivity("echo real\r")
	rt.mu.Lock()
	mode := rt.startupNotifyMode
	rt.mu.Unlock()
	if mode != startupNotifyNormal {
		t.Fatal("real input should clear startup notification suppression")
	}
}

func TestStartupDiscardSkipsTerminalCardAndSignalsReady(t *testing.T) {
	notifier := &recordingNotifier{}
	m := NewManager(nil, nil, WithNotifier(notifier))
	ready := make(chan string, 1)
	m.SetNotificationSentHook(func(sessionID string) { ready <- sessionID })
	rt := &RuntimeSession{
		manager:           m,
		session:           Session{ID: "sess-1", Name: "A", Status: StatusRunning, Live: true, NotifyOnWaiting: true},
		startupNotifyMode: startupNotifyDiscard,
	}

	rt.HandleOutput([]byte("OpenAI Codex\nmodel: gpt-5.6\ndirectory: /tmp/project\n"))
	rt.mu.Lock()
	version := rt.stateVersion
	rt.stopNotifyStableTimerLocked()
	rt.mu.Unlock()
	rt.notifyAfterStable(version)

	if got := notifier.count(); got != 0 {
		t.Fatalf("startup TUI must not be sent, got %d cards", got)
	}
	select {
	case sessionID := <-ready:
		if sessionID != "sess-1" {
			t.Fatalf("ready session = %q", sessionID)
		}
	case <-time.After(time.Second):
		t.Fatal("discarded startup output should release queued input")
	}
	if rt.discardingStartupNotifications() {
		t.Fatal("startup discard mode should end after the terminal becomes stable")
	}
}

func TestStartupPresetFinalNotificationSendsOnce(t *testing.T) {
	notifier := &recordingNotifier{}
	m := NewManager(nil, nil, WithNotifier(notifier))
	rt := &RuntimeSession{
		manager: m,
		session: Session{ID: "sess-1", Name: "A", Status: StatusRunning, Live: true, NotifyOnWaiting: true},
	}
	setTrustedLegacyRoundFixture(rt, "$", "echo setup\r", "$ echo setup\nsetup done\n$")
	rt.SuppressStartupNotifications()
	rt.finishStartupNotificationsAfter(250 * time.Millisecond)
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	version := rt.notifyVersion
	rt.mu.Unlock()

	rt.notifyIfStillWaiting(version)
	if got := notifier.count(); got != 0 {
		t.Fatalf("startup notification should stay suppressed during settle window, got %d", got)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if notifier.count() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	notes := notifier.notes()
	if len(notes) != 1 {
		t.Fatalf("final startup preset notification should send once after settle, got %#v", notes)
	}
	if notes[0].Content != "setup done\n$" {
		t.Fatalf("final startup notification content = %q", notes[0].Content)
	}
	rt.mu.Lock()
	mode := rt.startupNotifyMode
	rt.mu.Unlock()
	if mode != startupNotifyNormal {
		t.Fatalf("startup notification mode = %v, want normal", mode)
	}
}

type recordingNotifier struct {
	mu               sync.Mutex
	items            []WaitingNotification
	runningItems     []WaitingNotification
	messageID        string
	messageIDs       []string
	createMessageIDs []string
	createIndex      int
}

type resizeRecordingTerminal struct {
	cols uint16
	rows uint16
}

type lifecycleRecordingTerminal struct {
	mu      sync.Mutex
	written bytes.Buffer
	closes  int
}

func (t *lifecycleRecordingTerminal) Read([]byte) (int, error) { return 0, io.EOF }
func (t *lifecycleRecordingTerminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.written.Write(p)
}
func (t *lifecycleRecordingTerminal) Close() error {
	t.mu.Lock()
	t.closes++
	t.mu.Unlock()
	return nil
}
func (t *lifecycleRecordingTerminal) Resize(uint16, uint16) error { return nil }
func (t *lifecycleRecordingTerminal) writtenText() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.written.String()
}
func (t *lifecycleRecordingTerminal) closeCalls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closes
}

func (t *resizeRecordingTerminal) Read([]byte) (int, error) { return 0, io.EOF }
func (t *resizeRecordingTerminal) Write(p []byte) (int, error) {
	return len(p), nil
}
func (t *resizeRecordingTerminal) Close() error { return nil }
func (t *resizeRecordingTerminal) Resize(cols, rows uint16) error {
	t.cols = cols
	t.rows = rows
	return nil
}

func (n *recordingNotifier) Available() bool { return true }

func (n *recordingNotifier) NotifyWaiting(note WaitingNotification) (WaitingNotificationResult, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	index := len(n.items)
	n.items = append(n.items, note)
	messageID := note.MessageID
	if messageID == "" && n.createIndex < len(n.createMessageIDs) {
		messageID = n.createMessageIDs[n.createIndex]
		n.createIndex++
	}
	if messageID == "" && index < len(n.messageIDs) {
		messageID = n.messageIDs[index]
	}
	if messageID == "" && n.messageID != "" {
		messageID = n.messageID
	}
	if messageID == "" {
		messageID = "msg-recording"
	}
	return WaitingNotificationResult{MessageID: messageID, Updated: note.MessageID != ""}, nil
}

func (n *recordingNotifier) UpdateWaitingRunning(note WaitingNotification, running bool) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	note.Running = running
	n.runningItems = append(n.runningItems, note)
	return nil
}

func (n *recordingNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.items)
}

func (n *recordingNotifier) notes() []WaitingNotification {
	n.mu.Lock()
	defer n.mu.Unlock()
	cp := make([]WaitingNotification, len(n.items))
	copy(cp, n.items)
	return cp
}

func (n *recordingNotifier) runningNotes() []WaitingNotification {
	n.mu.Lock()
	defer n.mu.Unlock()
	cp := make([]WaitingNotification, len(n.runningItems))
	copy(cp, n.runningItems)
	return cp
}

type flakyNotifier struct {
	recordingNotifier
	failNotify      int
	failRunning     int
	notifyAttempts  int
	runningAttempts int
}

func (n *flakyNotifier) NotifyWaiting(note WaitingNotification) (WaitingNotificationResult, error) {
	n.mu.Lock()
	n.notifyAttempts++
	if n.failNotify > 0 {
		n.failNotify--
		n.mu.Unlock()
		return WaitingNotificationResult{}, errors.New("temporary notify failure")
	}
	n.mu.Unlock()
	return n.recordingNotifier.NotifyWaiting(note)
}

func (n *flakyNotifier) UpdateWaitingRunning(note WaitingNotification, running bool) error {
	n.mu.Lock()
	n.runningAttempts++
	if n.failRunning > 0 {
		n.failRunning--
		n.mu.Unlock()
		return errors.New("temporary running failure")
	}
	n.mu.Unlock()
	return n.recordingNotifier.UpdateWaitingRunning(note, running)
}

func (n *flakyNotifier) notifyAttemptCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.notifyAttempts
}

func (n *flakyNotifier) runningAttemptCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.runningAttempts
}

type runningNoteRecorder interface {
	runningNotes() []WaitingNotification
}

func waitForRunningNotes(t *testing.T, notifier runningNoteRecorder, want int) []WaitingNotification {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		notes := notifier.runningNotes()
		if len(notes) >= want {
			return notes
		}
		time.Sleep(10 * time.Millisecond)
	}
	return notifier.runningNotes()
}

func waitForBlockingRunningNotes(t *testing.T, notifier runningNoteRecorder, want int) []WaitingNotification {
	t.Helper()
	return waitForRunningNotes(t, notifier, want)
}

type blockingRefreshNotifier struct {
	recordingNotifier
	notifyOnce    sync.Once
	notifyStarted chan struct{}
	releaseNotify chan struct{}
}

func newBlockingRefreshNotifier(messageID string) *blockingRefreshNotifier {
	return &blockingRefreshNotifier{
		recordingNotifier: recordingNotifier{messageID: messageID},
		notifyStarted:     make(chan struct{}),
		releaseNotify:     make(chan struct{}),
	}
}

func (n *blockingRefreshNotifier) NotifyWaiting(note WaitingNotification) (WaitingNotificationResult, error) {
	n.mu.Lock()
	n.items = append(n.items, note)
	messageID := n.messageID
	if messageID == "" {
		messageID = "msg-recording"
	}
	n.mu.Unlock()
	n.notifyOnce.Do(func() { close(n.notifyStarted) })
	<-n.releaseNotify
	return WaitingNotificationResult{MessageID: messageID, Updated: note.MessageID != ""}, nil
}

type advancingNotifier struct {
	recordingNotifier
	afterNotify func()
}

func (n *advancingNotifier) NotifyWaiting(note WaitingNotification) (WaitingNotificationResult, error) {
	result, err := n.recordingNotifier.NotifyWaiting(note)
	if n.afterNotify != nil {
		afterNotify := n.afterNotify
		n.afterNotify = nil
		afterNotify()
	}
	return result, err
}

type sequencingNotifier struct {
	mu             sync.Mutex
	notifyOnce     sync.Once
	runningOnce    sync.Once
	messageID      string
	eventsList     []string
	notifyStarted  chan struct{}
	runningStarted chan struct{}
	releaseNotify  chan struct{}
}

func newSequencingNotifier(messageID string) *sequencingNotifier {
	return &sequencingNotifier{
		messageID:      messageID,
		notifyStarted:  make(chan struct{}),
		runningStarted: make(chan struct{}),
		releaseNotify:  make(chan struct{}),
	}
}

func (n *sequencingNotifier) Available() bool { return true }

func (n *sequencingNotifier) NotifyWaiting(note WaitingNotification) (WaitingNotificationResult, error) {
	n.notifyOnce.Do(func() { close(n.notifyStarted) })
	<-n.releaseNotify
	n.mu.Lock()
	n.eventsList = append(n.eventsList, fmt.Sprintf("notify:%v", note.Running))
	n.mu.Unlock()
	return WaitingNotificationResult{MessageID: n.messageID}, nil
}

func (n *sequencingNotifier) UpdateWaitingRunning(note WaitingNotification, running bool) error {
	n.runningOnce.Do(func() { close(n.runningStarted) })
	n.mu.Lock()
	n.eventsList = append(n.eventsList, fmt.Sprintf("running:%v", running))
	n.mu.Unlock()
	return nil
}

func (n *sequencingNotifier) events() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	cp := make([]string, len(n.eventsList))
	copy(cp, n.eventsList)
	return cp
}
