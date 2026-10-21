package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexCompletionRejectsChildAndStaleTurns(t *testing.T) {
	const root = "01a0e5f4-f80b-7131-b97f-f0aa406fa0fa"
	const child = "01a0e810-3314-7f31-a05d-e4b48b3332ae"
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "09", "28")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(id, source, parent, turn string, at time.Time) {
		t.Helper()
		meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "session_id": root, "source": source, "parent_thread_id": parent}})
		event, _ := json.Marshal(map[string]any{"type": "event_msg", "timestamp": at, "payload": map[string]string{"type": "task_started", "turn_id": turn}})
		if err := os.WriteFile(filepath.Join(dir, "rollout-2026-09-28T11-00-31-"+id+".jsonl"), append(append(meta, '\n'), append(event, '\n')...), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m := NewManager(nil, nil)
	rt := &RuntimeSession{manager: m, session: Session{ID: "parent", Live: true, Status: StatusRunning, RecoveryKey: "token", LastMode: SessionModeAgent, LastAgentKind: "codex", LastAgentStartCommand: "codex", LastAgentResumeCommand: "codex resume --last", LastAgentHome: home}}
	m.sessions[rt.session.ID] = rt
	defer rt.Close()
	rt.MarkStructuredInputActivity("do the work")
	write(child, "subagent", root, "child-turn", time.Now().Add(time.Second))
	write(root, "cli", "", "root-turn", time.Now().Add(time.Second))
	complete := func(id, turn string, want bool) {
		t.Helper()
		_, accepted, err := m.CompleteAgentTurnForTurn(context.Background(), rt.session.ID, "token", id, turn, "result:"+turn)
		if err != nil || accepted != want {
			t.Fatalf("thread=%s turn=%s accepted=%v err=%v", id, turn, accepted, err)
		}
	}
	// A child can inherit Iris credentials and the parent's session_id. It must
	// not bind recovery, finish the parent, or replace the reply even when first.
	complete(child, "child-turn", false)
	if rt.Snapshot().Status != StatusRunning || rt.hookLastAssistantMessage != "" || !strings.Contains(rt.Snapshot().LastAgentResumeCommand, "--last") {
		t.Fatal("child changed parent state")
	}
	complete(root, "old-turn", false)
	complete(root, "root-turn", true)
	if rt.Snapshot().Status != StatusWaiting || !strings.Contains(rt.Snapshot().LastAgentResumeCommand, root) {
		t.Fatal("root did not complete and pin")
	}
	rt.MarkStructuredInputActivity("next task")
	rt.mu.Lock()
	rt.session.LastAgentHome = home
	rt.mu.Unlock()
	write(root, "cli", "", "root-turn", time.Now().Add(-time.Second))
	complete(root, "root-turn", false) // Arrives before the new task_started is flushed.
	write(root, "cli", "", "next-turn", time.Now().Add(time.Second))
	complete(root, "root-turn", false)
	complete(child, "child-turn", false)
	rt.HandleOutput([]byte("Working · 2 background terminals running"))
	if rt.Snapshot().Status != StatusRunning || rt.hookCompletedCurrentRound || rt.hookLastAssistantMessage != "" {
		t.Fatal("stale/child completion stopped live work")
	}
	complete(root, "next-turn", true)
}

func TestCodexRecapCannotCompleteOrOverwriteRound(t *testing.T) {
	for _, status := range []string{StatusRunning, StatusWaiting} {
		n := &recordingNotifier{}
		m := NewManager(nil, nil, WithNotifier(n))
		rt := &RuntimeSession{manager: m, session: Session{ID: "recap", Live: true, Status: status, RecoveryKey: "token", LastMode: SessionModeAgent, LastAgentKind: "codex", NotifyOnWaiting: true}, hookLastAssistantMessage: "已加上，刷新即可。", hookCompletedCurrentRound: status == StatusWaiting, notifyVersion: 7}
		m.sessions[rt.session.ID] = rt
		_, accepted, err := m.CompleteAgentTurn(context.Background(), rt.session.ID, "token", "", `{"summary":"内部摘要","next_action":null}`)
		if err != nil || accepted || rt.Snapshot().Status != status || rt.hookLastAssistantMessage != "已加上，刷新即可。" || rt.notifyVersion != 7 || rt.hookCompletedCurrentRound != (status == StatusWaiting) || len(n.notes()) != 0 {
			t.Fatalf("recap changed round or notification: status=%s accepted=%v err=%v", status, accepted, err)
		}
		rt.Close()
	}
}

func TestQueuedInputPreservesCodexRoundAndContinuesAfterCompletion(t *testing.T) {
	const thread = "01a13f83-fbc7-7921-b4aa-d77e8e667ae9"
	t.Setenv("HOME", t.TempDir())
	oldDelay := structuredInputEnterDelay
	structuredInputEnterDelay = 0
	defer func() { structuredInputEnterDelay = oldDelay }()
	n := &recordingNotifier{createMessageIDs: []string{"card-1", "card-2", "card-3"}}
	m := NewManager(nil, &recordingLauncher{}, WithNotifier(n), WithNotificationUpdateCoalesce(0), WithIsolatedMessageRegistry())
	b := NewLarkReplyBridge("app", "secret", m, t.TempDir())
	dispatched := make(chan struct{}, 3)
	m.SetNotificationSentHook(func(id string) {
		b.OnNotificationSent(id)
		dispatched <- struct{}{}
	})
	sess, err := m.CreateSession(context.Background(), "queue")
	if err != nil {
		t.Fatal(err)
	}
	rt, _ := m.GetRuntime(sess.ID)
	defer rt.Close()
	rt.mu.Lock()
	rt.session.LastMode, rt.session.LastAgentKind = SessionModeAgent, "codex"
	rt.session.LastAgentStartCommand, rt.session.LastAgentResumeCommand = "codex", "codex resume "+thread
	rt.session.LastAgentHome, rt.session.NotifyOnWaiting = defaultCodexHome(), true
	rt.inputQueueUntil = time.Now().Add(time.Minute)
	rt.mu.Unlock()
	term := rt.terminal.(*recordingTerminal)
	submitted := make(chan string, 3)
	term.onWrite = func(data string) {
		if data == "\r" {
			rt.mu.Lock()
			input := rt.lastInputText
			rt.mu.Unlock()
			submitted <- input
		}
	}
	rt.SetNotificationAssistantName("first assistant")
	if err := SubmitStructuredInputWithMention(rt, "first", "ou-first", "input-first"); err != nil {
		t.Fatal(err)
	}
	<-submitted
	rt.NotifyInputRunning()
	rt.mu.Lock()
	at, generation, version := rt.hookRoundSubmittedAt, rt.snapshotRoundGeneration, rt.notifyVersion
	card := rt.lastNotifiedMessageID
	rt.mu.Unlock()
	started := time.Now().UTC()
	for _, text := range []string{"second", "third"} {
		if !b.enqueueInputIfRuntimeBusy(rt, sess.ID, []string{text}, "ou-"+text, larkRouteContext{MessageID: "input-" + text, AssistantName: text + " assistant"}) {
			t.Fatal("input was not queued")
		}
	}
	rt.mu.Lock()
	unchanged := rt.lastInputText == "first" && rt.hookRoundSubmittedAt.Equal(at) && rt.snapshotRoundGeneration == generation && rt.notifyVersion == version && rt.notificationInputMessageID == "input-first" && rt.notificationMentionOpenID == "ou-first" && rt.notificationAssistantName == "first assistant" && rt.lastNotifiedMessageID == card
	rt.mu.Unlock()
	if !unchanged || len(n.notes()) != 1 {
		t.Fatal("queued input overwrote the active round or its notification")
	}
	dir := filepath.Join(defaultCodexHome(), "sessions", "2026", "10", "21")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "rollout-queue-"+thread+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	if err := enc.Encode(map[string]any{"type": "session_meta", "payload": map[string]string{"id": thread, "source": "cli"}}); err != nil {
		t.Fatal(err)
	}
	for i, input := range []string{"first", "second", "third"} {
		turn := input + "-turn"
		for _, event := range []any{
			map[string]any{"type": "event_msg", "timestamp": started, "payload": map[string]string{"type": "task_started", "turn_id": turn}},
			map[string]any{"type": "event_msg", "timestamp": started.Add(time.Millisecond), "payload": map[string]any{"type": "item_completed", "turn_id": turn, "item": map[string]any{"type": "UserMessage", "content": []map[string]string{{"type": "text", "text": input}}}}},
			map[string]any{"type": "event_msg", "timestamp": started.Add(2 * time.Millisecond), "payload": map[string]string{"type": "task_complete", "turn_id": turn}},
		} {
			if err := enc.Encode(event); err != nil {
				t.Fatal(err)
			}
		}
		_, accepted, err := m.CompleteAgentTurnForTurn(context.Background(), sess.ID, rt.Snapshot().RecoveryKey, thread, turn, input+" reply")
		if err != nil || !accepted {
			t.Fatalf("completion rejected for %s: %v", input, err)
		}
		select {
		case <-dispatched:
		case <-time.After(3 * time.Second):
			t.Fatal("completion did not automatically advance the input queue")
		}
		if i < 2 {
			select {
			case next := <-submitted:
				want := []string{"second", "third"}[i]
				if next != want {
					t.Fatalf("submitted=%q want=%q", next, want)
				}
				rt.mu.Lock()
				correctOrigin := rt.notificationInputMessageID == "input-"+next && rt.notificationMentionOpenID == "ou-"+next && rt.notificationAssistantName == next+" assistant"
				rt.mu.Unlock()
				if !correctOrigin {
					t.Fatal("queued input lost its origin")
				}
				started = time.Now().UTC()
			case <-time.After(3 * time.Second):
				t.Fatal("completion did not automatically release the next queued input")
			}
		}
	}
	if rt.Snapshot().Status != StatusWaiting {
		t.Fatal("completed queue remained running")
	}
	firstReplyFound := false
	for _, note := range n.notes() {
		if strings.Contains(note.Content, "first reply") {
			firstReplyFound = true
			if note.MentionOpenID != "ou-first" || note.InputMessageID != "input-first" {
				t.Fatal("first completion was attributed to queued input")
			}
		}
	}
	if !firstReplyFound {
		t.Fatal("active task result was not pushed")
	}
}

func TestCodexForkCompletionRebindsOnlyVerifiedDescendants(t *testing.T) {
	const root = "01a15247-486f-7b93-a7c4-a0fde640c88c"
	const fork = "01a15249-e70f-7820-a1a9-38b6f7102655"
	const next = "01a13540-53a1-7de0-8f03-1137b0c72f60"
	for _, tc := range []struct {
		name                                                           string
		parent                                                         string
		source                                                         string
		input                                                          string
		oldInput, copiedInput, missingTurn, wrongToken, chained, cycle bool
		missingCreation, subagentAncestor                              bool
		want                                                           bool
	}{
		{name: "direct fork", parent: root, source: "cli", input: "好了吗", want: true},
		{name: "multiple forks", parent: next, source: "cli", input: "好了吗", chained: true, want: true},
		{name: "subagent ancestor", parent: next, source: "cli", input: "好了吗", chained: true, subagentAncestor: true},
		{name: "missing creation time", parent: root, source: "cli", input: "好了吗", missingCreation: true},
		{name: "unrelated thread", source: "cli", input: "好了吗"},
		{name: "unrelated branch", parent: next, source: "cli", input: "好了吗"},
		{name: "subagent", parent: root, source: "subagent", input: "好了吗"},
		{name: "different input", parent: root, source: "cli", input: "另一个问题"},
		{name: "queued input not consumed", parent: root, source: "cli"},
		{name: "same text from old submission", parent: root, source: "cli", input: "好了吗", oldInput: true},
		{name: "copied parent history", parent: root, source: "cli", input: "好了吗", copiedInput: true},
		{name: "legacy hook lacks turn", parent: root, source: "cli", input: "好了吗", missingTurn: true},
		{name: "wrong token", parent: root, source: "cli", input: "好了吗", wrongToken: true},
		{name: "cyclic lineage", parent: next, source: "cli", input: "好了吗", cycle: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			home := defaultCodexHome()
			dir := filepath.Join(home, "sessions", "2026", "10", "19")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			st := newMemoryStore()
			m := NewManager(st, nil)
			rt := &RuntimeSession{manager: m, session: Session{ID: "fork-test", Live: true, Status: StatusRunning, RecoveryKey: "token", LastMode: SessionModeAgent, LastAgentKind: "codex", LastAgentStartCommand: "codex --no-alt-screen", LastAgentResumeCommand: "codex resume " + root + " --no-alt-screen", LastAgentHome: home}}
			m.sessions[rt.session.ID] = rt
			defer rt.Close()
			if err := st.CreateSession(context.Background(), rt.session); err != nil {
				t.Fatal(err)
			}
			rt.MarkStructuredInputActivity("好了吗")
			submitted := rt.hookRoundSubmittedAt
			write := func(id, parent, source, input string, at, created time.Time) {
				t.Helper()
				f, err := os.Create(filepath.Join(dir, "rollout-test-"+id+".jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				enc := json.NewEncoder(f)
				for _, event := range []any{
					map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "source": source, "forked_from_id": parent, "timestamp": created}},
					map[string]any{"type": "event_msg", "timestamp": at, "payload": map[string]string{"type": "task_started", "turn_id": "turn"}},
					map[string]any{"type": "event_msg", "timestamp": at.Add(time.Millisecond), "payload": map[string]any{"type": "item_completed", "turn_id": "turn", "item": map[string]any{"type": "UserMessage", "content": []map[string]string{{"type": "text", "text": input}}}}},
					map[string]any{"type": "event_msg", "timestamp": at.Add(time.Second), "payload": map[string]string{"type": "task_complete", "turn_id": "turn"}},
				} {
					if err := enc.Encode(event); err != nil {
						t.Fatal(err)
					}
				}
			}
			at, created := submitted.Add(time.Second), submitted.Add(-time.Minute)
			if tc.oldInput {
				at = submitted.Add(-time.Second)
			}
			if tc.copiedInput {
				created = at.Add(time.Second)
			}
			if tc.missingCreation {
				created = time.Time{}
			}
			if tc.chained {
				source := "cli"
				if tc.subagentAncestor {
					source = "subagent"
				}
				write(next, root, source, "", at, created)
			}
			if tc.cycle {
				write(next, fork, "cli", "", at, created)
			}
			write(fork, tc.parent, tc.source, tc.input, at, created)
			turn, token := "turn", "token"
			if tc.missingTurn {
				turn = ""
			}
			if tc.wrongToken {
				token = "wrong"
			}
			_, accepted, err := m.CompleteAgentTurnForTurn(context.Background(), rt.session.ID, token, fork, turn, "好了，可以开始了。")
			if accepted != tc.want || (!tc.wrongToken && err != nil) {
				t.Fatalf("accepted=%v want=%v err=%v", accepted, tc.want, err)
			}
			wantID, wantStatus := root, StatusRunning
			if tc.want {
				wantID, wantStatus = fork, StatusWaiting
			}
			stored, _, err := st.GetSession(context.Background(), rt.session.ID)
			if err != nil || codexResumeThreadID(stored.LastAgentResumeCommand) != wantID || rt.Snapshot().Status != wantStatus || strings.Contains(stored.LastAgentResumeCommand, "--last") {
				t.Fatalf("incorrect state: %#v err=%v", stored, err)
			}
			if !tc.want && rt.hookLastAssistantMessage != "" {
				t.Fatal("rejected event replaced the reply")
			}
			if tc.want {
				// Once rebound, old parent/sibling events must not roll recovery back.
				rt.MarkStructuredInputActivity("好了吗")
				write(root, "", "cli", "好了吗", time.Now().Add(time.Second), created)
				_, accepted, err = m.CompleteAgentTurnForTurn(context.Background(), rt.session.ID, "token", root, "turn", "old parent")
				if err != nil || accepted || rt.Snapshot().Status != StatusRunning {
					t.Fatal("old parent rolled binding back")
				}
				write(next, root, "cli", "好了吗", time.Now().Add(time.Second), created)
				_, accepted, err = m.CompleteAgentTurnForTurn(context.Background(), rt.session.ID, "token", next, "turn", "sibling fork")
				if err != nil || accepted || rt.Snapshot().Status != StatusRunning {
					t.Fatal("sibling fork replaced the active branch")
				}
				write(next, fork, "cli", "好了吗", time.Now().Add(time.Second), created)
				_, accepted, err = m.CompleteAgentTurnForTurn(context.Background(), rt.session.ID, "token", next, "turn", "next fork")
				if err != nil || !accepted || codexResumeThreadID(rt.Snapshot().LastAgentResumeCommand) != next {
					t.Fatalf("subsequent fork failed: accepted=%v err=%v", accepted, err)
				}
			}
		})
	}
}

func TestCodexCompletionAcceptsConsumedSteeringOnly(t *testing.T) {
	const thread = "01a119aa-632b-79c1-8524-2245faf55ccc"
	submitted := time.Now().UTC()
	for _, tc := range []struct {
		name             string
		kind             string
		role             string
		turn             string
		text             string
		beforeSubmission bool
		afterCompletion  bool
		newTurn          bool
		want             bool
	}{
		{name: "consumed response", kind: "response_item", role: "user", turn: "turn", text: "补充消息", want: true},
		{name: "legacy response", kind: "response_item", role: "user", text: "补充消息", want: true},
		{name: "consumed native item", kind: "event_msg", role: "UserMessage", turn: "turn", text: "补充消息", want: true},
		{name: "unconsumed queued input"},
		{name: "different input", kind: "response_item", role: "user", turn: "turn", text: "其他消息"},
		{name: "assistant echo", kind: "response_item", role: "assistant", turn: "turn", text: "补充消息"},
		{name: "foreign turn", kind: "response_item", role: "user", turn: "other", text: "补充消息"},
		{name: "repeated old input", kind: "response_item", role: "user", turn: "turn", text: "补充消息", beforeSubmission: true},
		{name: "input after completion", kind: "response_item", role: "user", turn: "turn", text: "补充消息", afterCompletion: true},
		{name: "new task supersedes steering", kind: "event_msg", role: "UserMessage", turn: "turn", text: "补充消息", newTurn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, "sessions", "2026", "10", "08")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			f, err := os.Create(filepath.Join(dir, "rollout-test-"+thread+".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			enc := json.NewEncoder(f)
			write := func(at time.Time, kind string, payload any) {
				t.Helper()
				if err := enc.Encode(map[string]any{"timestamp": at, "type": kind, "payload": payload}); err != nil {
					t.Fatal(err)
				}
			}
			write(submitted.Add(-time.Minute), "session_meta", map[string]any{"id": thread, "source": "cli"})
			write(submitted.Add(-time.Minute), "event_msg", map[string]any{"type": "task_started", "turn_id": "turn"})
			if tc.afterCompletion {
				write(submitted.Add(-time.Second), "event_msg", map[string]any{"type": "task_complete", "turn_id": "turn"})
			}
			at := submitted.Add(time.Second)
			if tc.beforeSubmission {
				at = submitted.Add(-time.Second)
			}
			content := []map[string]string{{"type": "text", "text": tc.text}}
			if tc.kind == "response_item" {
				write(at, tc.kind, map[string]any{"type": "message", "role": tc.role, "content": content, "internal_chat_message_metadata_passthrough": map[string]string{"turn_id": tc.turn}})
			} else if tc.kind == "event_msg" {
				write(at, tc.kind, map[string]any{"type": "item_completed", "turn_id": tc.turn, "item": map[string]any{"type": tc.role, "content": content}})
			}
			write(submitted.Add(2*time.Second), "event_msg", map[string]any{"type": "task_complete", "turn_id": "turn"})
			if tc.newTurn {
				write(submitted.Add(3*time.Second), "event_msg", map[string]any{"type": "task_started", "turn_id": "next"})
			}
			err = validateCodexCompletion(Session{LastAgentHome: home}, thread, "turn", submitted, "补充消息")
			if (err == nil) != tc.want {
				t.Fatalf("accepted=%v want=%v err=%v", err == nil, tc.want, err)
			}
		})
	}
}
