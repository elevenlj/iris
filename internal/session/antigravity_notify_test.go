package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const testAntigravityID = "ec33ebf9-0cba-4100-8142-c61503f6c587"

func TestAntigravityDiscoveryAndRecovery(t *testing.T) {
	options := detectAvailableAgentOptions(nil, fakeAgentExecutableFinder{"agy": true})
	if len(options) != 1 || options[0].ID != "antigravity" || options[0].Command != AntigravityAgentCommand {
		t.Fatalf("discovery: %#v", options)
	}
	if got := normalizeAgentConfig(AgentConfig{Kind: "antigravity"}); got.Command != AntigravityAgentCommand || got.ID != "antigravity" {
		t.Fatalf("normalized: %#v", got)
	}
	for _, command := range []string{AntigravityAgentCommand, "FOO=bar /bin/agy --model test --conversation old"} {
		info, ok := agentLaunchInfo(shellFields(command))
		if !ok || info.Kind != "antigravity" {
			t.Fatalf("not recognized: %q", command)
		}
		fresh := Session{LastAgentKind: "antigravity", LastAgentStartCommand: command, LastAgentResumeCommand: info.ResumeCommand}
		if exactAgentResumeCommand(fresh) != "" {
			t.Fatal("must not recover without an exact valid conversation")
		}
		pinned, ok := pinAntigravityResumeCommand(command, testAntigravityID)
		if !ok || antigravityConversationID(pinned) != testAntigravityID || strings.Contains(pinned, "--continue") {
			t.Fatalf("not pinned: %q", pinned)
		}
		fresh.LastAgentResumeCommand = pinned
		if exactAgentResumeCommand(fresh) != pinned || !sessionSupportsWorkspaceSwitch(fresh) {
			t.Fatal("exact recovery or workspace switch unavailable")
		}
	}
	for _, command := range []string{"agy --help", "agy update", "agy acp", "agy -p hello"} {
		if _, ok := agentLaunchInfo(shellFields(command)); ok {
			t.Fatalf("noninteractive command recognized: %s", command)
		}
	}
	for _, resume := range []string{"", "agy --continue", "agy --conversation invalid"} {
		terminal := &recordingTerminal{}
		rt := &RuntimeSession{terminal: terminal, session: Session{LastMode: SessionModeAgent, LastAgentKind: "antigravity", LastAgentStartCommand: AntigravityAgentCommand, LastAgentResumeCommand: resume}}
		rt.runRecoveryCommand()
		if got := terminal.writes(); got != AntigravityAgentCommand+"\r" {
			t.Fatalf("recovered without exact conversation: %q", got)
		}
	}
}

func TestAntigravityHookInstallation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".gemini", "config", "hooks.json")
	if err := writeFileAtomically(path, []byte(`{"my-hook":{"PreToolUse":[]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, exe := range []string{"/old/iris", "/new/iris", "/new/iris"} {
		if err := EnsureAntigravityStopHook(exe); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(path)
	var hooks map[string]json.RawMessage
	if err := json.Unmarshal(data, &hooks); err != nil || len(hooks) != 2 || hooks["my-hook"] == nil || !strings.Contains(string(data), "/new/iris") || strings.Contains(string(data), "/old/iris") {
		t.Fatalf("hooks not preserved/replaced: %s %v", data, err)
	}
	if err := os.WriteFile(path, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureAntigravityStopHook("/new/iris"); err == nil {
		t.Fatal("must preserve invalid user config")
	}
}

func TestAntigravityStopReadsOnlyCurrentConversationAndFinalTurn(t *testing.T) {
	logs := filepath.Join(t.TempDir(), testAntigravityID, ".system_generated", "logs")
	path := filepath.Join(logs, "transcript.jsonl")
	write := func(content string) {
		t.Helper()
		if err := writeFileAtomically(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"type":"USER_INPUT","content":"hi"}
{"type":"PLANNER_RESPONSE","source":"MODEL","status":"DONE","content":"final answer"}
`)
	var payload map[string]any
	calls := 0
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-Iris-Agent-Token") != "test-token" || r.URL.Path != "/api/sessions/iris-session/hook/turn-ended" {
			t.Error("wrong Iris callback identity")
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.WriteHeader(http.StatusOK)
	}))
	defer host.Close()
	t.Setenv("IRIS_API_URL", host.URL)
	t.Setenv("IRIS_SESSION_ID", "iris-session")
	t.Setenv("IRIS_SESSION_TOKEN", "test-token")
	invoke := func(idle any) error {
		event, _ := json.Marshal(map[string]any{"fullyIdle": idle, "conversationId": testAntigravityID, "transcriptPath": path, "terminationReason": "model_stop"})
		return RunAntigravityStopHook(strings.NewReader(string(event)))
	}
	for _, idle := range []any{false, nil} {
		if err := invoke(idle); err != nil || calls != 0 {
			t.Fatalf("completed with background activity: %v", err)
		}
	}
	if err := invoke(true); err != nil || calls != 1 || payload["session_id"] != testAntigravityID || payload["last_assistant_message"] != "final answer" {
		t.Fatalf("callback: %#v %v", payload, err)
	}
	write(`{"type":"PLANNER_RESPONSE","source":"MODEL","status":"DONE","content":"old reply"}
{"type":"USER_INPUT","content":"next question"}
`)
	if err := invoke(true); err == nil || calls != 1 {
		t.Fatal("reused old turn reply")
	}
	if _, err := antigravityAssistantMessage(path, "other-conversation"); err == nil {
		t.Fatal("read mismatched conversation")
	}
	write(`{"type":"PLANNER_RESPONSE","source":"MODEL","status":"DONE","content":"intermediate","tool_calls":[{}]}`)
	if err := invoke(true); err == nil {
		t.Fatal("published a tool-call step")
	}
	if err := writeFileAtomically(filepath.Join(logs, "transcript_full.jsonl"), []byte(`{"type":"PLANNER_RESPONSE","source":"MODEL","status":"DONE","content":"complete answer"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := invoke(true); err != nil || payload["last_assistant_message"] != "complete answer" {
		t.Fatal("did not read untruncated same-conversation transcript", err)
	}
}

func TestAntigravityCompletionAndComposer(t *testing.T) {
	m := NewManager(nil, nil)
	rt := &RuntimeSession{manager: m, session: Session{ID: "agy", Live: true, Status: StatusRunning, RecoveryKey: "token", LastMode: SessionModeAgent, LastAgentKind: "antigravity", LastAgentStartCommand: AntigravityAgentCommand, LastAgentResumeCommand: AntigravityAgentCommand}}
	m.sessions["agy"] = rt
	defer rt.Close()
	sess, accepted, err := m.CompleteAgentTurn(context.Background(), "agy", "token", testAntigravityID, "final reply")
	if err != nil || !accepted || sess.Status != StatusWaiting || !reflect.DeepEqual(shellFields(exactAgentResumeCommand(sess)), []string{"agy", "--dangerously-skip-permissions", "--conversation", testAntigravityID}) {
		t.Fatalf("completion/recovery: %#v %v %v", sess, accepted, err)
	}
	if rt.hookLastAssistantMessage != "final reply" || !rt.hookCompletedCurrentRound {
		t.Fatal("native answer not recorded")
	}
	if _, accepted, err := m.CompleteAgentTurn(context.Background(), "agy", "token", "01a0c73d-600f-7343-8c84-3f9cbb15317f", "wrong conversation"); err != nil || accepted || rt.hookLastAssistantMessage != "final reply" {
		t.Fatal("accepted another conversation's callback")
	}
	source := strings.Replace(aidenReadySource, "cursor_line=-1", "cursor_line=2", 1)
	screen := "Antigravity CLI 1.5.14\n───────────────────\n>\n───────────────────\n? for shortcuts     auto · Gemini"
	if !startupAgentComposerReady(screen, source, "antigravity") {
		t.Fatal("live framed composer not recognized")
	}
	for _, invalid := range []string{"Antigravity CLI\nSigning in…", screen + "\n$ ", "Select model\n> Model A\n  Model B\nenter Confirm"} {
		if startupAgentComposerReady(invalid, source, "antigravity") {
			t.Fatalf("accepted non-ready UI: %q", invalid)
		}
	}
}

func TestAntigravityTrustMenu(t *testing.T) {
	screen := "Accessing workspace:\n/tmp/work\nDo you trust the contents of this project?\nAntigravity CLI requires permission to read, edit, and execute files here.\n> Yes, I trust this folder\n  No, exit\n↑/↓ Navigate · enter Confirm\nGemini 3.8 Flash · high"
	source := strings.Replace(aidenReadySource, "cursor_line=-1", "cursor_line=7", 1)
	if key, ok := workspaceTrustMenu(screen, source, "antigravity"); !ok || key != "\r" {
		t.Fatalf("trust prompt: %q %v", key, ok)
	}
	if _, ok := workspaceTrustMenu(screen+"\n$ ", source, "antigravity"); ok {
		t.Fatal("accepted historical trust prompt")
	}
	if startupAgentComposerReady(screen, source, "antigravity") {
		t.Fatal("trust dialog mistaken for ready input")
	}
}
