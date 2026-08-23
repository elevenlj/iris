package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	claudeStopModeFlag = "--claude-stop"
	claudeStopTimeout  = 5
)

// IsClaudeStopInvocation reports whether Iris was launched as a Claude Code
// Stop hook rather than as the long-running server.
func IsClaudeStopInvocation(args []string) bool {
	return len(args) > 0 && args[0] == claudeStopModeFlag
}

// EnsureAgentCompletionHooks installs every supported Agent integration,
// including completion callbacks and the shared Feishu context skill. Each
// installer is attempted so one malformed external config does not prevent
// the other integrations from being configured.
func EnsureAgentCompletionHooks(executable string) error {
	var errs []error
	if err := EnsureCodexNotify(executable); err != nil {
		errs = append(errs, fmt.Errorf("Codex notify: %w", err))
	}
	if err := EnsureClaudeStopHook(executable); err != nil {
		errs = append(errs, fmt.Errorf("Claude Stop hook: %w", err))
	}
	if err := EnsureAgentContextSkills(); err != nil {
		errs = append(errs, fmt.Errorf("Agent Feishu context skills: %w", err))
	}
	return errors.Join(errs...)
}

// EnsureClaudeStopHook adds one Iris-managed Stop hook while preserving all
// other Claude Code settings and hooks.
func EnsureClaudeStopHook(executable string) error {
	dir := defaultClaudeConfigDir()
	if dir == "" {
		return errors.New("cannot resolve default Claude config directory")
	}
	executable = strings.TrimSpace(executable)
	if executable == "" {
		return errors.New("cannot resolve Iris executable")
	}
	if absolute, err := filepath.Abs(executable); err == nil {
		executable = absolute
	}
	return ensureClaudeStopHookConfig(filepath.Join(dir, "settings.json"), executable)
}

func defaultClaudeConfigDir() string {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return filepath.Clean(dir)
	}
	home := strings.TrimSpace(userHomeDir())
	if home == "" || home == "." {
		return ""
	}
	return filepath.Join(home, ".claude")
}

func ensureClaudeStopHookConfig(path, executable string) error {
	mode := os.FileMode(0o600)
	root := map[string]any{}
	content, err := os.ReadFile(path)
	if err == nil {
		if info, statErr := os.Stat(path); statErr == nil {
			mode = info.Mode().Perm()
		}
		if len(strings.TrimSpace(string(content))) > 0 {
			if err := json.Unmarshal(content, &root); err != nil {
				return fmt.Errorf("parse %s: %w", path, err)
			}
			if root == nil {
				root = map[string]any{}
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		if _, exists := root["hooks"]; exists {
			return fmt.Errorf("parse %s: hooks must be an object", path)
		}
		hooks = map[string]any{}
		root["hooks"] = hooks
	}
	stop, ok := hooks["Stop"].([]any)
	if !ok {
		if _, exists := hooks["Stop"]; exists {
			return fmt.Errorf("parse %s: hooks.Stop must be an array", path)
		}
		stop = []any{}
	}

	command := shellQuote(executable) + " " + claudeStopModeFlag
	found := false
	changed := false
	groups := make([]any, 0, len(stop)+1)
	for _, group := range stop {
		groupMap, groupOK := group.(map[string]any)
		if !groupOK {
			groups = append(groups, group)
			continue
		}
		handlers, handlersOK := groupMap["hooks"].([]any)
		if !handlersOK {
			groups = append(groups, group)
			continue
		}
		keptHandlers := make([]any, 0, len(handlers))
		for _, handler := range handlers {
			handlerMap, handlerOK := handler.(map[string]any)
			if !handlerOK || !isManagedClaudeStopCommand(handlerMap["command"]) {
				keptHandlers = append(keptHandlers, handler)
				continue
			}
			if found {
				changed = true
				continue
			}
			found = true
			if handlerMap["type"] != "command" || handlerMap["command"] != command || numericJSONValue(handlerMap["timeout"]) != claudeStopTimeout {
				changed = true
			}
			handlerMap["type"] = "command"
			handlerMap["command"] = command
			handlerMap["timeout"] = claudeStopTimeout
			keptHandlers = append(keptHandlers, handlerMap)
		}
		if len(keptHandlers) > 0 {
			groupMap["hooks"] = keptHandlers
			groups = append(groups, groupMap)
		}
	}
	if !found {
		changed = true
		groups = append(groups, map[string]any{
			"hooks": []any{map[string]any{
				"type":    "command",
				"command": command,
				"timeout": claudeStopTimeout,
			}},
		})
	}
	if !changed {
		return nil
	}
	hooks["Stop"] = groups
	updated, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	updated = append(updated, '\n')
	return writeFileAtomically(path, updated, mode)
}

func isManagedClaudeStopCommand(value any) bool {
	command, _ := value.(string)
	for _, arg := range shellFields(command) {
		if arg == claudeStopModeFlag {
			return true
		}
	}
	return false
}

func numericJSONValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case float64:
		return int(typed)
	default:
		return 0
	}
}

// RunClaudeStopHook reads Claude Code's official Stop-hook JSON from stdin
// and forwards the final assistant response to the authenticated Iris session.
func RunClaudeStopHook(reader io.Reader) error {
	if reader == nil {
		return errors.New("missing Claude Stop hook input")
	}
	limited := io.LimitReader(reader, (1<<20)+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if len(payload) > 1<<20 {
		return errors.New("Claude Stop hook input exceeds 1 MiB")
	}
	var event struct {
		HookEventName        string `json:"hook_event_name"`
		SessionID            string `json:"session_id"`
		LastAssistantMessage string `json:"last_assistant_message"`
		StopHookActive       bool   `json:"stop_hook_active"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return fmt.Errorf("parse Claude Stop hook input: %w", err)
	}
	if event.HookEventName != "Stop" {
		return nil
	}
	return postAgentTurnCompleted(payload)
}
