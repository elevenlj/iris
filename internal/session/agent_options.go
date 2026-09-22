package session

import (
	"strings"
)

const (
	CodexAgentCommand       = "codex --dangerously-bypass-approvals-and-sandbox --no-alt-screen"
	ClaudeAgentCommand      = "claude --dangerously-skip-permissions"
	AidenAgentCommand       = "AIDEN_USE_1X_AGENT=1 aiden --permission-mode agentFull"
	AidenCodexAgentCommand  = "aiden x codex --dangerously-bypass-approvals-and-sandbox --no-alt-screen"
	AidenClaudeAgentCommand = "aiden x claude --dangerously-skip-permissions"
	TraeAgentCommand        = "traecli --yolo"
)

type AgentOption struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Kind    string `json:"kind"`
	Command string `json:"command"`
}

type agentExecutableFinder interface {
	LookPath(string) (string, error)
}

type defaultAgentExecutableFinder struct{}

func (defaultAgentExecutableFinder) LookPath(name string) (string, error) {
	return execAgentUpgradeRunner{}.LookPath(name)
}

func DetectAvailableAgentOptions(configured ...AgentConfig) []AgentOption {
	return detectAvailableAgentOptions(configured, defaultAgentExecutableFinder{})
}

func detectAvailableAgentOptions(configured []AgentConfig, finder agentExecutableFinder) []AgentOption {
	options := make([]AgentOption, 0, len(configured)+5)
	_, codexErr := finder.LookPath("codex")
	if codexErr == nil {
		options = append(options, AgentOption{ID: "codex", Label: "Codex", Kind: "codex", Command: CodexAgentCommand})
	}
	_, claudeErr := finder.LookPath("claude")
	if claudeErr == nil {
		options = append(options, AgentOption{ID: "claude", Label: "Claude Code", Kind: "claude", Command: ClaudeAgentCommand})
	}
	if _, err := finder.LookPath("aiden"); err == nil {
		options = append(options, AgentOption{ID: "aiden", Label: "Aiden", Kind: "aiden", Command: AidenAgentCommand})
		if codexErr == nil {
			options = append(options, AgentOption{ID: "aiden-codex", Label: "Aiden X Codex", Kind: "aiden-codex", Command: AidenCodexAgentCommand})
		}
		if claudeErr == nil {
			options = append(options, AgentOption{ID: "aiden-claude", Label: "Aiden X Claude Code", Kind: "aiden-claude", Command: AidenClaudeAgentCommand})
		}
	}
	if _, err := finder.LookPath("traecli"); err == nil {
		options = append(options, AgentOption{ID: "traecli", Label: "TRAE CLI", Kind: "traecli", Command: TraeAgentCommand})
	}
	for _, agent := range configured {
		agent.ID = strings.ToLower(strings.TrimSpace(agent.ID))
		agent.Name = strings.TrimSpace(agent.Name)
		agent.Kind = strings.ToLower(strings.TrimSpace(agent.Kind))
		agent.Command = strings.TrimSpace(agent.Command)
		if agent.Kind == "custom" && agent.ID != "" && agent.Name != "" && agent.Command != "" {
			options = append(options, AgentOption{ID: agent.ID, Label: agent.Name, Kind: "custom", Command: agent.Command})
		}
	}
	return normalizeAgentOptions(options)
}

func normalizeAgentOptions(options []AgentOption) []AgentOption {
	out := make([]AgentOption, 0, len(options))
	seen := map[string]bool{}
	for _, option := range options {
		option.ID = strings.ToLower(strings.TrimSpace(option.ID))
		option.Label = strings.TrimSpace(option.Label)
		option.Kind = strings.ToLower(strings.TrimSpace(option.Kind))
		option.Command = strings.TrimSpace(option.Command)
		if option.ID == "" || option.Label == "" || option.Command == "" || seen[option.ID] {
			continue
		}
		if option.Kind != "codex" && option.Kind != "claude" && option.Kind != "aiden" && option.Kind != "aiden-codex" && option.Kind != "aiden-claude" && option.Kind != "traecli" && option.Kind != "custom" {
			continue
		}
		seen[option.ID] = true
		out = append(out, option)
	}
	return out
}
