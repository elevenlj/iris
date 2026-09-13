package session

import (
	"strings"
	"testing"
	"time"
)

// Native Aiden 2.0.2 layout from the reported startup card. Ink's terminal
// cursor is below the footer, not at the visible > composer.
const aidenReadySnapshot = `>_ Aiden (v2.0.2)
1. Ask questions, edit files, or run commands.
✦ ⚠ LSP server 'go': command 'gopls' not found.

                         agent full mode (shift + tab to toggle)
warning: 代码改动上报服务异常，将导致 AI 代码贡献率统计缺失。请运行 aiden doctor code-adoption --fix 修复

─────────────────────────────────────────────────────────────────
> Summarize the main points...
─────────────────────────────────────────────────────────────────
🔌 MCP(2/4   ❌ slardar-mcp: Failed to connect to stdio     Type /mcp
Serverconnecserver "slardar-mcp": McpError: MCP error      to view
      ed)   -32000: Connection closed                      details`

const aidenReadySource = "headless:buffer;continuity_version=2;render_epoch=1;buffer_type=normal;buffer_at_capacity=false;anchor_guard_active=false;anchor_guard_line=-1;cursor_line=-1"

// Claude Code 2.1.270 / Aiden X Claude: the alternate-screen cursor is on
// the input row ABOVE the visible suggestion, not on its prompt marker.
const claudeReadySnapshot = `Claude Code v2.1.270
SessionStart:startup hook error
Failed with non-blocking status code: npm error code E404
────────────────────────────────────────────────────────────
- ��
❯ Try "how do I log an error?"
────────────────────────────────────────────────────────────
⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents`

func TestClaudeStartupRecognizesCursorInsideComposer(t *testing.T) {
	source := strings.ReplaceAll(strings.Replace(aidenReadySource, "cursor_line=-1", "cursor_line=4", 1), "buffer_type=normal", "buffer_type=alternate")
	for _, test := range []struct {
		name, snapshot, source string
		ready                  bool
	}{
		{"reported layout", claudeReadySnapshot, source, true},
		{"blank input row", strings.ReplaceAll(claudeReadySnapshot, "- ��", ""), source, true},
		{"non-breaking prompt space", strings.ReplaceAll(claudeReadySnapshot, "❯ ", "❯\u00a0"), source, true},
		{"trailing empty rows", claudeReadySnapshot + "\n\n", source, true},
		{"browser snapshot", claudeReadySnapshot, strings.Replace(source, "headless:", "browser:", 1), true},
		{"normal buffer", claudeReadySnapshot, strings.Replace(source, "buffer_type=alternate", "buffer_type=normal", 1), true},
		{"missing cursor", claudeReadySnapshot, strings.Replace(source, "cursor_line=4", "cursor_line=-1", 1), false},
		{"cursor outside composer", claudeReadySnapshot, strings.Replace(source, "cursor_line=4", "cursor_line=2", 1), false},
		{"missing metadata", claudeReadySnapshot, "headless:buffer", false},
		{"DOM", claudeReadySnapshot, strings.Replace(source, ":buffer", ":dom", 1), false},
		{"no mode footer", strings.ReplaceAll(claudeReadySnapshot, "bypass permissions on", "unknown"), source, false},
		{"shell below old composer", claudeReadySnapshot + "\n$ ", source, false},
		{"login below old composer", claudeReadySnapshot + "\nNot logged in · run /login", source, false},
		{"approval below old composer", claudeReadySnapshot + "\n❯ 1. Allow\n  2. Deny", source, false},
		{"new modal", claudeReadySnapshot + "\n─────────\nSelect model\n─────────", source, false},
		{"trust modal", strings.ReplaceAll(claudeReadySnapshot, "⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents", "Enter to confirm · Esc to cancel"), source, false},
	} {
		for _, command := range []string{ClaudeAgentCommand, AidenClaudeAgentCommand} {
			t.Run(test.name+"/"+command, func(t *testing.T) {
				if got := startupAgentComposerReady(test.snapshot, test.source, agentKindForCommand(command, "custom")); got != test.ready {
					t.Fatalf("ready = %v, want %v", got, test.ready)
				}
			})
		}
	}
}

func TestClaudeFramedComposerCompletesStartupCard(t *testing.T) {
	notifier := &recordingNotifier{createMessageIDs: []string{"startup-card"}}
	m := NewManager(nil, nil, WithNotifier(notifier))
	released := make(chan string, 1)
	m.SetNotificationSentHook(func(id string) { released <- id })
	rt := &RuntimeSession{manager: m,
		session: Session{ID: "claude-ready", Status: StatusWaiting, Live: true, NotifyOnWaiting: true,
			LastMode: SessionModeAgent, LastAgentKind: "claude", LastAgentStartCommand: AidenClaudeAgentCommand},
		startupNotifyMode: startupNotifyDiscard, notifyVersion: 1,
		visibleSnapshot:       claudeReadySnapshot,
		visibleSnapshotSource: strings.Replace(aidenReadySource, "cursor_line=-1", "cursor_line=4", 1),
	}
	rt.beginStartupNotification("")
	rt.notifyIfStillWaitingForInteraction(1)
	notes := notifier.notes()
	if len(notes) != 2 || !notes[1].StartupComplete || notes[1].StartupInputEnabled || notes[1].MessageID != "startup-card" {
		t.Fatalf("startup completion = %#v", notes)
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("queued input not released")
	}
	if rt.discardingStartupNotifications() {
		t.Fatal("still starting")
	}
}

func TestAidenStartupRecognizesFramedComposerWithoutCursor(t *testing.T) {
	for _, test := range []struct {
		name, snapshot, source, kind string
		ready                        bool
	}{
		{"native with MCP errors", aidenReadySnapshot, aidenReadySource, "aiden", true},
		{"empty input", strings.ReplaceAll(aidenReadySnapshot, "> Summarize the main points...", ">"), aidenReadySource, "aiden", true},
		{"plan mode", strings.ReplaceAll(aidenReadySnapshot, "agent full mode", "plan mode"), aidenReadySource, "aiden", true},
		{"browser", aidenReadySnapshot, strings.Replace(aidenReadySource, "headless:", "browser:", 1), "aiden", true},
		{"cursor parked in footer", aidenReadySnapshot, strings.Replace(aidenReadySource, "cursor_line=-1", "cursor_line=10", 1), "aiden", true},
		{"welcome only", ">_ Aiden (v2.0.2)\nCheck user login status...", aidenReadySource, "aiden", false},
		{"no native mode", strings.ReplaceAll(aidenReadySnapshot, "mode (shift + tab to toggle)", ""), aidenReadySource, "aiden", false},
		{"login dialog", strings.Split(aidenReadySnapshot, "🔌 MCP")[0] + "Please sign in\n> Login", aidenReadySource, "aiden", false},
		{"modal below old composer", aidenReadySnapshot + "\n> 1. Allow\n  2. Deny", aidenReadySource, "aiden", false},
		{"new frame below old composer", aidenReadySnapshot + "\n────────────\nSelect model\n────────────", aidenReadySource, "aiden", false},
		{"DOM lacks buffer identity", aidenReadySnapshot, strings.Replace(aidenReadySource, ":buffer", ":dom", 1), "aiden", false},
		{"missing metadata", aidenReadySnapshot, "headless:buffer", "aiden", false},
		{"Codex unchanged", aidenReadySnapshot, aidenReadySource, "codex", false},
		{"Claude unchanged", aidenReadySnapshot, aidenReadySource, "claude", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := startupAgentComposerReady(test.snapshot, test.source, test.kind); got != test.ready {
				t.Fatalf("ready = %v, want %v", got, test.ready)
			}
		})
	}
}

func TestClaudeTrustMenuDoesNotReleaseStartupInput(t *testing.T) {
	for _, command := range []string{ClaudeAgentCommand, AidenClaudeAgentCommand} {
		for _, option := range []string{"No, exit", "Yes, I trust this folder", "1. Yes, I trust this folder"} {
			snapshot := "Accessing workspace: /tmp/project\nQuick safety check: Is this a project you created or one you trust?\n❯ " + option + "\n  Yes, I trust this folder\nEnter to confirm · Esc to cancel"
			source := strings.Replace(aidenReadySource, "cursor_line=-1", "cursor_line=2", 1)
			kind := agentKindForCommand(command, "custom")
			if startupAgentComposerReady(snapshot, source, kind) {
				t.Fatalf("trust menu treated as composer: %s / %s", command, option)
			}
			// Historical trust text must not block an actual composer after approval.
			if !startupAgentComposerReady(snapshot+"\n❯ ", strings.Replace(source, "cursor_line=2", "cursor_line=5", 1), kind) {
				t.Fatal("real composer blocked by old trust text")
			}
		}
	}
}

func TestClaudeTrustMenuRemainsVisibleWithoutReleasingQueuedMessage(t *testing.T) {
	notifier := &recordingNotifier{messageID: "trust-card"}
	m := NewManager(nil, nil, WithNotifier(notifier))
	released := false
	m.SetNotificationSentHook(func(string) { released = true })
	rt := &RuntimeSession{manager: m,
		session:           Session{ID: "claude-trust", Live: true, NotifyOnWaiting: true, Status: StatusWaiting, LastMode: SessionModeAgent, LastAgentKind: "claude", LastAgentStartCommand: AidenClaudeAgentCommand},
		startupNotifyMode: startupNotifyDiscard, notifyVersion: 1,
		visibleSnapshot:       "Accessing workspace: /tmp/project\n❯ No, exit\n  Yes, I trust this folder\nEnter to confirm · Esc to cancel",
		visibleSnapshotSource: strings.Replace(aidenReadySource, "cursor_line=-1", "cursor_line=1", 1),
	}
	rt.notifyIfStillWaitingForInteraction(1)
	if released || !rt.discardingStartupNotifications() {
		t.Fatal("trust prompt released queued message")
	}
	notes := notifier.notes()
	if len(notes) == 0 || notes[len(notes)-1].StartupComplete || !strings.Contains(notes[len(notes)-1].Content, "Yes, I trust this folder") {
		t.Fatalf("trust prompt not shown: %#v", notes)
	}
}

func TestAidenStartupCompletesExistingCardAndReleasesQueue(t *testing.T) {
	notifier := &recordingNotifier{createMessageIDs: []string{"startup-card"}}
	m := NewManager(nil, nil, WithNotifier(notifier))
	released := make(chan string, 1)
	m.SetNotificationSentHook(func(id string) { released <- id })
	rt := &RuntimeSession{
		manager: m,
		session: Session{ID: "aiden-ready", Status: StatusWaiting, Live: true, NotifyOnWaiting: true,
			LastAgentKind: "aiden", LastAgentStartCommand: AidenAgentCommand},
		startupNotifyMode: startupNotifyDiscard, notifyVersion: 1,
		visibleSnapshot: aidenReadySnapshot, visibleSnapshotSource: aidenReadySource,
	}
	rt.beginStartupNotification("")
	rt.notifyIfStillWaitingForInteraction(1)
	notes := notifier.notes()
	if len(notes) != 2 || !notes[1].StartupComplete || notes[1].StartupInputEnabled || notes[1].MessageID != "startup-card" || !notes[1].SuppressUpdateTip {
		t.Fatalf("startup completion = %#v", notes)
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("startup completion did not release queued input")
	}
	if rt.discardingStartupNotifications() {
		t.Fatal("startup protection still active")
	}
}
