package session

import (
	"strings"
	"testing"
)

func TestStartupCardsPreserveMentionMode(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[enabled], func(t *testing.T) {
			notifier := &recordingNotifier{createMessageIDs: []string{"startup-card"}}
			m := NewManager(nil, nil, WithNotifier(notifier), WithIsolatedMessageRegistry())
			rt := &RuntimeSession{manager: m, startupNotifyMode: startupNotifyDiscard, session: Session{ID: "controls", Live: true, NotifyOnWaiting: true, DeveloperModeEnabled: true, LarkMentionModeEnabled: enabled}}
			defer rt.Close()
			id := rt.beginStartupNotification("")
			if id != "startup-card" {
				t.Fatalf("startup card = %q", id)
			}
			if _, err := rt.refreshStartupNotification(id, false); err != nil {
				t.Fatal(err)
			}
			rt.completeStartupNotification()
			notes := notifier.notes()
			if len(notes) != 3 {
				t.Fatalf("expected startup, refresh and completion, got %d", len(notes))
			}
			for _, note := range notes {
				if note.MentionModeEnabled != enabled || rt.Snapshot().LarkMentionModeEnabled != enabled {
					t.Fatalf("mention mode changed: enabled=%v note=%+v", enabled, note)
				}
				card, err := larkNotificationCardContent(note, "", false)
				if err != nil {
					t.Fatal(err)
				}
				want := "艾特模式：关"
				if enabled {
					want = "艾特模式：开"
				}
				if !strings.Contains(card, want) {
					t.Fatalf("card does not reflect saved mode %q: %s", want, card)
				}
			}
		})
	}
}

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
