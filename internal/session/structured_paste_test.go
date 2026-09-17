package session

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestStructuredCodexPasteBoundaries(t *testing.T) {
	oldDelay := structuredInputEnterDelay
	structuredInputEnterDelay = 0
	defer func() { structuredInputEnterDelay = oldDelay }()
	long := strings.Repeat("中文长文本", 400)
	for _, tc := range []struct {
		name, command, mode, text string
		enter, paste, rejected    bool
	}{
		{"codex long", CodexAgentCommand, SessionModeAgent, long, true, true, false},
		{"aiden codex long", AidenCodexAgentCommand, SessionModeAgent, long, true, true, false},
		{"multiline", AidenCodexAgentCommand, SessionModeAgent, "first\nsecond", true, true, false},
		{"short", CodexAgentCommand, SessionModeAgent, "hello", true, true, false},
		{"switch context", CodexAgentCommand, SessionModeAgent, larkAgentContextPrompt, true, true, false},
		{"slash", CodexAgentCommand, SessionModeAgent, "/review " + long, true, false, false},
		{"menu", CodexAgentCommand, SessionModeAgent, "2", false, false, false},
		{"claude", ClaudeAgentCommand, SessionModeAgent, long, true, false, false},
		{"aiden", AidenAgentCommand, SessionModeAgent, long, true, false, false},
		{"shell", CodexAgentCommand, "shell", long, true, false, false},
		{"paste terminator injection", CodexAgentCommand, SessionModeAgent, long + "\x1b[201~", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			term := &recordingTerminal{readCh: make(chan []byte)}
			rt := &RuntimeSession{manager: NewManager(nil, nil), terminal: term,
				session: Session{ID: "paste", Live: true, LastMode: tc.mode, LastAgentStartCommand: tc.command}}
			defer rt.Close()
			err := submitStructuredInputWithMode(rt, tc.text, "", true, tc.enter, nil, false)
			parts := term.writeParts()
			if tc.rejected {
				if err == nil || len(parts) != 0 {
					t.Fatalf("unsafe input written: %v", err)
				}
				return
			}
			want := tc.text
			if tc.paste {
				want = "\x1b[200~" + want + "\x1b[201~"
			}
			count := 1
			if tc.enter {
				count++
			}
			if err != nil || len(parts) != count || parts[0] != want || (tc.enter && parts[1] != "\r") {
				t.Fatalf("incorrect paste/Enter writes: count=%d error=%v", len(parts), err)
			}
			if tc.enter && rt.lastInputText != tc.text {
				t.Fatal("paste framing polluted input anchor")
			}
		})
	}
}

type shortPasteTerminal struct{ recordingTerminal }

func (t *shortPasteTerminal) Write(p []byte) (int, error) {
	_, _ = t.recordingTerminal.Write(p)
	return len(p) - 1, nil
}

func TestStructuredPasteShortWriteDoesNotPressEnter(t *testing.T) {
	term := &shortPasteTerminal{recordingTerminal: recordingTerminal{readCh: make(chan []byte)}}
	rt := &RuntimeSession{terminal: term, session: Session{LastMode: SessionModeAgent, LastAgentStartCommand: AidenCodexAgentCommand}}
	err := SubmitSilentStructuredInput(rt, strings.Repeat("long text", 200))
	if !errors.Is(err, io.ErrShortWrite) || len(term.writeParts()) != 1 {
		t.Fatalf("short write must abort before Enter: %v", err)
	}
}
