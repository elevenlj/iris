package session

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
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
		{"trae long", TraeAgentCommand, SessionModeAgent, long, true, true, false},
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

func TestStructuredSubmissionsDoNotInterleave(t *testing.T) {
	oldDelay := structuredInputEnterDelay
	structuredInputEnterDelay = 0
	defer func() { structuredInputEnterDelay = oldDelay }()
	term := &recordingTerminal{readCh: make(chan []byte)}
	rt := &RuntimeSession{manager: NewManager(nil, nil), terminal: term,
		session: Session{ID: "serial", Live: true, LastMode: SessionModeAgent, LastAgentStartCommand: CodexAgentCommand}}
	defer rt.Close()
	firstWritten, releaseFirst, secondWritten := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	defer release()
	var enterInputs, enterMessages []string
	term.onWrite = func(data string) {
		switch data {
		case "\x1b[200~first\x1b[201~":
			close(firstWritten)
			<-releaseFirst
		case "\x1b[200~second\x1b[201~":
			close(secondWritten)
		case "\r":
			rt.mu.Lock()
			enterInputs = append(enterInputs, rt.lastInputText)
			enterMessages = append(enterMessages, rt.notificationInputMessageID)
			rt.mu.Unlock()
		}
	}
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	go func() { firstDone <- SubmitStructuredInputWithMention(rt, "first", "", "message-1") }()
	select {
	case <-firstWritten:
	case <-time.After(time.Second):
		t.Fatal("first submission did not reach terminal")
	}
	go func() { secondDone <- SubmitQueuedStructuredInputWithMention(rt, "second", "", "message-2") }()
	select {
	case <-secondWritten:
		t.Fatal("second paste interleaved before first Enter")
	case <-time.After(100 * time.Millisecond):
	}
	// A blocked session must not block submissions to another session.
	otherTerm := &recordingTerminal{readCh: make(chan []byte)}
	other := &RuntimeSession{terminal: otherTerm}
	otherDone := make(chan error, 1)
	go func() { otherDone <- SubmitSilentStructuredInput(other, "independent") }()
	select {
	case err := <-otherDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("another session was blocked")
	}
	release()
	for _, done := range []chan error{firstDone, secondDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("submission did not finish")
		}
	}
	want := []string{"\x1b[200~first\x1b[201~", "\r", "\x1b[200~second\x1b[201~", "\r"}
	if !reflect.DeepEqual(term.writeParts(), want) || !reflect.DeepEqual(enterInputs, []string{"first", "second"}) || !reflect.DeepEqual(enterMessages, []string{"message-1", "message-2"}) {
		t.Fatalf("interleaved submission: writes=%q inputs=%q messages=%q", term.writeParts(), enterInputs, enterMessages)
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
