package session

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"
)

func (m *Manager) SetSystemPrompt(prompt string) {
	m.mu.Lock()
	m.systemPrompt = strings.TrimSpace(prompt)
	m.mu.Unlock()
}

func (rt *RuntimeSession) agentLaunchCommand(command string) (string, error) {
	if rt.manager == nil {
		return command, nil
	}
	rt.manager.mu.RLock()
	prompt := rt.manager.systemPrompt
	rt.manager.mu.RUnlock()
	kind := agentKindForCommand(command, "")
	fallback := ""
	if prompt != "" {
		switch kind {
		case "codex":
			value, _ := json.Marshal(prompt)
			command += " -c " + shellQuote("developer_instructions="+string(value))
		case "claude", "aiden":
			command += " --append-system-prompt " + shellQuote(prompt)
			// New Claude versions snapshot prompts across resume. Opt out only
			// when supported, so older CLIs can still launch normally.
			if kind == "claude" {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				help, _ := exec.CommandContext(ctx, "claude", "--help").Output()
				cancel()
				if strings.Contains(string(help), "--system-prompt-snapshot") {
					command += " --system-prompt-snapshot off"
				}
			}
		default:
			// TRAE currently retains old native instructions on resume, so use
			// the same first-request fallback as custom CLIs until it supports updates.
			fallback = prompt
		}
	}
	command, err := rt.agentNotifyLaunchCommand(command)
	if err != nil {
		return "", err
	}
	rt.mu.Lock()
	rt.pendingSystemPrompt = fallback
	rt.mu.Unlock()
	return command, nil
}
