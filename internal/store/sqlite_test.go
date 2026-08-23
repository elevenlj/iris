package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/elevenlj/iris/internal/session"
)

func TestSQLitePersistsLarkContactBindings(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "iris.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC().Truncate(time.Millisecond)
	binding := session.LarkContactBinding{
		SenderOpenID: "ou-contact", DisplayName: "小林", ChatID: "oc-chat", SessionID: "sess-1",
		Active: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.UpsertLarkContactBinding(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.GetLarkContactBinding(context.Background(), "ou-contact")
	if err != nil || !ok || got.ChatID != "oc-chat" || got.SessionID != "sess-1" || !got.Active {
		t.Fatalf("unexpected binding: %#v ok=%v err=%v", got, ok, err)
	}
	if err := st.DeactivateLarkContactBinding(context.Background(), "ou-contact"); err != nil {
		t.Fatal(err)
	}
	got, ok, err = st.GetLarkContactBinding(context.Background(), "ou-contact")
	if err != nil || !ok || got.Active {
		t.Fatalf("binding should be inactive: %#v ok=%v err=%v", got, ok, err)
	}
}

func TestSQLiteSessionLifecycle(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	sess := session.Session{ID: "sess-1", Name: "test", Status: session.StatusRunning, CreatedAt: now, UpdatedAt: now, Live: true}
	if err := st.CreateSession(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendOutput(context.Background(), sess.ID, 0, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	out, err := st.Output(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "hello" {
		t.Fatalf("unexpected output: %q", out)
	}
	list, err := st.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != sess.ID {
		t.Fatalf("unexpected sessions: %#v", list)
	}
	if list[0].LarkMentionModeEnabled {
		t.Fatalf("mention mode should default to disabled: %#v", list[0])
	}
	list[0].LarkMentionModeEnabled = true
	if err := st.UpdateSession(context.Background(), list[0]); err != nil {
		t.Fatal(err)
	}
	updated, ok, err := st.GetSession(context.Background(), sess.ID)
	if err != nil || !ok {
		t.Fatalf("GetSession ok=%v err=%v", ok, err)
	}
	if !updated.LarkMentionModeEnabled {
		t.Fatalf("mention mode should persist: %#v", updated)
	}
}
