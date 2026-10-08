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
