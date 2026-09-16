package session

import (
	"strings"
	"testing"
)

func TestStartupCardsHideInternalLaunchEnvironment(t *testing.T) {
	for _, command := range []string{
		"unset CODEX_HOME; export CLAUDE_CONFIG_DIR='/private/session' IRIS_SESSION_TOKEN='test-secret'\ncd /private/work\naiden --resume test-session",
		"export IRIS_SESS\nION_TOKEN='test-\nsecret'\ncd /private/work",
	} {
		if got := pickLarkStartupFallbackContent(command); got != StartupNotificationPlaceholder {
			t.Fatalf("launch script not suppressed: %q", got)
		}
		// Check before tail truncation, which could otherwise lose the marker.
		if got := pickLarkStartupFallbackContent(command + strings.Repeat("\nwrapped-private-value", 40)); got != StartupNotificationPlaceholder {
			t.Fatal("wrapped launch script leaked through tail truncation")
		}
		for _, startup := range []bool{true, false} {
			card, err := larkNotificationCardContent(WaitingNotification{Content: command, Startup: startup}, "", false)
			if err != nil || strings.Contains(card, "private") || strings.Contains(card, "secret") || strings.Contains(card, "IRIS_") || strings.Contains(card, "aiden --resume") {
				t.Fatalf("card leaked launch script: %s %v", card, err)
			}
		}
	}
	blocker := "Select Model\n❯ 1. model-a\n  2. model-b\nEnter to confirm · Esc to cancel"
	if got := pickLarkStartupFallbackContent(blocker); got != blocker {
		t.Fatalf("ordinary startup blocker changed: %q", got)
	}
}
