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
