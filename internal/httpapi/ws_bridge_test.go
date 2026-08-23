package httpapi

import (
	"context"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/elevenlj/iris/internal/session"
)

func TestSnapshotRequestPayloadIncludesPurpose(t *testing.T) {
	got := snapshotRequestPayload(session.RuntimeEvent{
		Type:      session.RuntimeEventSnapshotRequest,
		RequestID: "req-1",
		Purpose:   session.SnapshotPurposeInputBaseline,
	})
	want := map[string]string{
		"type":       "snapshot_request",
		"request_id": "req-1",
		"purpose":    "input_baseline",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot request payload = %#v, want %#v", got, want)
	}

	withoutPurpose := snapshotRequestPayload(session.RuntimeEvent{
		Type:      session.RuntimeEventSnapshotRequest,
		RequestID: "req-2",
	})
	if _, exists := withoutPurpose["purpose"]; exists {
		t.Fatalf("blank purpose must be omitted, got %#v", withoutPurpose)
	}
}

func TestSnapshotSourceEncodesContinuityV2Metadata(t *testing.T) {
	b := &wsBridge{}
	cursorLine := 41
	got := b.snapshotSource("buffer", 2, 17, "normal", true, true, 6, &cursorLine)
	want := "browser:buffer;continuity_version=2;render_epoch=17;buffer_type=normal;buffer_at_capacity=true;anchor_guard_active=true;anchor_guard_line=6;cursor_line=41"
	if got != want {
		t.Fatalf("snapshot source = %q, want %q", got, want)
	}
}

func TestSnapshotSourceKeepsLegacyClientSource(t *testing.T) {
	b := &wsBridge{headless: true}
	if got := b.snapshotSource("buffer", 0, 0, "", false, false, 0, nil); got != "headless:buffer" {
		t.Fatalf("legacy snapshot source = %q", got)
	}
}

func TestFilterTerminalResponses(t *testing.T) {
	in := []byte("a\x1b[12;40Rb\x1b[?1;2cc\x1b[>0;276;0cd\x1bP1+r436f=76616c\x1b\\e\x1b]10;rgb:ffff/ffff/ffff\x07f")
	got := string(filterTerminalResponses(in))
	if got != "abcdef" {
		t.Fatalf("unexpected filtered data: %q", got)
	}
}

func TestFilterTerminalResponsesKeepsUserNavigationInput(t *testing.T) {
	in := []byte("a\x1b[A\x1b[B\x1b[Cb")
	got := string(filterTerminalResponses(in))
	if got != string(in) {
		t.Fatalf("user navigation input should be preserved, got %q", got)
	}
}

func TestWebSubmitPassesSnapshotOriginToStructuredInput(t *testing.T) {
	terminal := newWSBridgeTestTerminal()
	manager := session.NewManager(nil, wsBridgeTestLauncher{terminal: terminal})
	sess, err := manager.CreateSession(context.Background(), "web-submit-origin")
	if err != nil {
		t.Fatal(err)
	}
	rt, ok := manager.GetRuntime(sess.ID)
	if !ok {
		t.Fatal("runtime session missing")
	}
	defer rt.Close()

	idle, cancelIdle := rt.SubscribeWithMode(false)
	defer cancelIdle()
	origin, cancelOrigin := rt.SubscribeWithMode(false)
	defer cancelOrigin()
	bridge := &wsBridge{rt: rt, subscriber: origin}
	done := make(chan struct{})
	go func() {
		bridge.handleClientMessage(clientMessage{Type: "submit", Data: "web origin input"})
		close(done)
	}()

	request := receiveWSBridgeSnapshotRequest(t, origin)
	if request.Purpose != session.SnapshotPurposeInputBaseline {
		t.Fatalf("baseline request purpose = %q", request.Purpose)
	}
	assertNoWSBridgeEvent(t, idle, "idle browser received web submit baseline")
	rt.SetVisibleSnapshotResponseFrom("web origin baseline", "browser:buffer", request.RequestID, origin)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("web submit did not finish")
	}

	// The baseline accepted through the submit path must also become the exact
	// owner used by ordinary current-round/notification refreshes.
	fresh := make(chan bool, 1)
	go func() { fresh <- rt.RequestFreshSnapshot(time.Second) }()
	request = receiveWSBridgeSnapshotRequest(t, origin)
	assertNoWSBridgeEvent(t, idle, "ordinary fresh request lost the web submit owner")
	rt.SetVisibleSnapshotResponseFrom("web current", "browser:buffer", request.RequestID, origin)
	select {
	case ok := <-fresh:
		if !ok {
			t.Fatal("origin-owned fresh request failed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("origin-owned fresh request did not finish")
	}
}

func receiveWSBridgeSnapshotRequest(t *testing.T, ch <-chan session.RuntimeEvent) session.RuntimeEvent {
	t.Helper()
	select {
	case event := <-ch:
		if event.Type != session.RuntimeEventSnapshotRequest || event.RequestID == "" {
			t.Fatalf("unexpected runtime event: %#v", event)
		}
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for snapshot request")
		return session.RuntimeEvent{}
	}
}

func assertNoWSBridgeEvent(t *testing.T, ch <-chan session.RuntimeEvent, message string) {
	t.Helper()
	select {
	case event := <-ch:
		t.Fatalf("%s: %#v", message, event)
	default:
	}
}

type wsBridgeTestLauncher struct {
	terminal *wsBridgeTestTerminal
}

func (l wsBridgeTestLauncher) Launch(context.Context) (session.ProcessHandle, error) {
	return wsBridgeTestHandle{terminal: l.terminal}, nil
}

type wsBridgeTestHandle struct {
	terminal *wsBridgeTestTerminal
}

func (h wsBridgeTestHandle) Terminal() session.Terminal { return h.terminal }
func (h wsBridgeTestHandle) Process() session.Waiter {
	return wsBridgeTestWaiter{closed: h.terminal.closed}
}

type wsBridgeTestWaiter struct {
	closed <-chan struct{}
}

func (w wsBridgeTestWaiter) Wait() error {
	<-w.closed
	return nil
}

type wsBridgeTestTerminal struct {
	closeOnce sync.Once
	closed    chan struct{}
}

func newWSBridgeTestTerminal() *wsBridgeTestTerminal {
	return &wsBridgeTestTerminal{closed: make(chan struct{})}
}

func (t *wsBridgeTestTerminal) Read([]byte) (int, error) {
	<-t.closed
	return 0, io.EOF
}

func (t *wsBridgeTestTerminal) Write(p []byte) (int, error) { return len(p), nil }
func (t *wsBridgeTestTerminal) Resize(uint16, uint16) error { return nil }
func (t *wsBridgeTestTerminal) Close() error {
	t.closeOnce.Do(func() { close(t.closed) })
	return nil
}
