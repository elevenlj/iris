package session

import (
	"context"
	"testing"
)

func TestRetireSerializesLateCreationAndRecovery(t *testing.T) {
	m := NewManager(nil, nil)
	entered, release, retired := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		retired <- m.Retire(context.Background(), func() error { close(entered); <-release; return nil })
	}()
	<-entered
	created, recovered := make(chan error, 1), make(chan error, 1)
	go func() { _, err := m.CreateSession(context.Background(), "late message"); created <- err }()
	go func() { _, _, _, err := m.RecoverRuntime(context.Background(), "sess-1"); recovered <- err }()
	close(release)
	if err := <-retired; err != nil {
		t.Fatal(err)
	}
	if <-created == nil || <-recovered == nil {
		t.Fatal("late callback started an Agent after deletion")
	}
}

func TestRetiredBridgeCannotStart(t *testing.T) {
	b := NewLarkReplyBridge("cli_deleted", "secret", NewManager(nil, nil), t.TempDir())
	b.Retire()
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.startID != 0 || b.ConnectionStatus() != "stopped" {
		t.Fatal("deleted bridge restarted")
	}
}
