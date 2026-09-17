package session

import "testing"

func TestControlsRefreshNeverFallsBackToTerminal(t *testing.T) {
	for _, startup := range []bool{true, false} {
		notifier := &recordingNotifier{}
		m := NewManager(nil, nil, WithNotifier(notifier), WithIsolatedMessageRegistry())
		rt := &RuntimeSession{manager: m, session: Session{ID: "controls", Live: true, NotifyOnWaiting: true, Status: StatusWaiting, LastMode: SessionModeAgent}, visibleSnapshot: "private terminal transcript"}
		defer rt.Close()
		want := RunningNotificationPlaceholder
		if startup {
			rt.startupNotificationMessageID = "card"
			rt.startupNotificationContent = StartupCompletePlaceholder
			want = StartupCompletePlaceholder
		}
		if err := rt.RefreshNotificationControls("card"); err != nil {
			t.Fatal(err)
		}
		notes := notifier.notes()
		if len(notes) != 1 || notes[0].Content != want {
			t.Fatalf("controls refresh changed body: %#v", notes)
		}
	}
}
