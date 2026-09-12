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

func TestDetectAvailableAgentOptionsIncludesCompatibleAidenModesAndCustom(t *testing.T) {
	got := detectAvailableAgentOptions([]AgentConfig{
		{ID: "codex", Name: "Codex", Kind: "codex", Command: "codex --custom-flags"},
		{ID: "custom-plan", Name: "方案助手", Kind: "custom", Command: "my-agent --full-access"},
		{ID: "custom-review", Name: "审查助手", Kind: "custom", Command: "review-agent"},
	}, fakeAgentExecutableFinder{"codex": true, "claude": true, "aiden": true})
	want := []AgentOption{
		{ID: "codex", Label: "Codex", Kind: "codex", Command: CodexAgentCommand},
		{ID: "claude", Label: "Claude Code", Kind: "claude", Command: ClaudeAgentCommand},
		{ID: "aiden", Label: "Aiden", Kind: "aiden", Command: AidenAgentCommand},
		{ID: "aiden-claude", Label: "Aiden X Claude Code", Kind: "aiden-claude", Command: AidenClaudeAgentCommand},
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

func TestDetectAvailableAgentOptionsRequiresAidenAndClaudeForAidenClaude(t *testing.T) {
	for _, test := range []struct {
		name      string
		installed fakeAgentExecutableFinder
		wantIDs   []string
	}{
		{name: "aiden only", installed: fakeAgentExecutableFinder{"aiden": true}, wantIDs: []string{"aiden"}},
		{name: "claude only", installed: fakeAgentExecutableFinder{"claude": true}, wantIDs: []string{"claude"}},
		{name: "both", installed: fakeAgentExecutableFinder{"aiden": true, "claude": true}, wantIDs: []string{"claude", "aiden", "aiden-claude"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := detectAvailableAgentOptions(nil, test.installed)
			gotIDs := make([]string, 0, len(got))
			for _, option := range got {
				gotIDs = append(gotIDs, option.ID)
			}
			if !reflect.DeepEqual(gotIDs, test.wantIDs) {
				t.Fatalf("IDs = %#v, want %#v", gotIDs, test.wantIDs)
			}
		})
	}
}
