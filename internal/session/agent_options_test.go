package session

import (
	"errors"
	"reflect"
	"testing"
)

type fakeAgentExecutableFinder map[string]bool

func (f fakeAgentExecutableFinder) LookPath(name string) (string, error) {
	if f[name] {
		return "/bin/" + name, nil
	}
	return "", errors.New("not found")
}

func TestDetectAvailableAgentOptionsPrioritizesCodexThenClaudeAndIncludesCustom(t *testing.T) {
	got := detectAvailableAgentOptions([]AgentConfig{
		{ID: "codex", Name: "Codex", Kind: "codex", Command: "codex --custom-flags"},
		{ID: "custom-plan", Name: "方案助手", Kind: "custom", Command: "my-agent --full-access"},
		{ID: "custom-review", Name: "审查助手", Kind: "custom", Command: "review-agent"},
	}, fakeAgentExecutableFinder{"codex": true, "claude": true})
	want := []AgentOption{
		{ID: "codex", Label: "Codex", Kind: "codex", Command: CodexAgentCommand},
		{ID: "claude", Label: "Claude Code", Kind: "claude", Command: ClaudeAgentCommand},
		{ID: "custom-plan", Label: "方案助手", Kind: "custom", Command: "my-agent --full-access"},
		{ID: "custom-review", Label: "审查助手", Kind: "custom", Command: "review-agent"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("options = %#v, want %#v", got, want)
	}
}

func TestDetectAvailableAgentOptionsExcludesMissingBuiltins(t *testing.T) {
	got := detectAvailableAgentOptions(nil, fakeAgentExecutableFinder{"claude": true})
	if len(got) != 1 || got[0].ID != "claude" || got[0].Command != ClaudeAgentCommand {
		t.Fatalf("options = %#v", got)
	}
}
