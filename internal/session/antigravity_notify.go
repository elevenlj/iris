package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const antigravityStopModeFlag = "--antigravity-stop"

func IsAntigravityStopInvocation(args []string) bool {
	return len(args) > 0 && args[0] == antigravityStopModeFlag
}

func EnsureAntigravityStopHook(executable string) error {
	home := strings.TrimSpace(userHomeDir())
	if home == "" || home == "." || strings.TrimSpace(executable) == "" {
		return errors.New("cannot resolve Antigravity config directory or Iris executable")
	}
	executable, err := filepath.Abs(executable)
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".gemini", "config", "hooks.json")
	root := map[string]json.RawMessage{}
	content, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(content) > 0 {
		if err := json.Unmarshal(content, &root); err != nil {
			return fmt.Errorf("parse Antigravity hooks: %w", err)
		}
	}
	if root == nil {
		root = map[string]json.RawMessage{}
	}
	// Antigravity uses named hook definitions, not Claude's hooks.Stop schema.
	root["iris-antigravity-completion"], _ = json.Marshal(map[string]any{
		"Stop": []any{map[string]any{"type": "command", "command": shellQuote(executable) + " " + antigravityStopModeFlag, "timeout": 5}},
	})
	updated, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(content)) == string(updated) {
		return nil
	}
	return writeFileAtomically(path, append(updated, '\n'), 0o600)
}

func RunAntigravityStopHook(reader io.Reader) error {
	if reader == nil {
		return errors.New("missing Antigravity Stop input")
	}
	var event struct {
		ConversationID    string `json:"conversationId"`
		TranscriptPath    string `json:"transcriptPath"`
		FullyIdle         *bool  `json:"fullyIdle"`
		TerminationReason string `json:"terminationReason"`
		Error             string `json:"error"`
	}
	data, err := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("Antigravity Stop input exceeds 1 MiB")
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return err
	}
	if firstNonEmptyEnv("IRIS_SESSION_TOKEN") == "" || event.FullyIdle == nil || !*event.FullyIdle {
		return nil
	}
	if !validCodexThreadID(event.ConversationID) {
		return errors.New("invalid Antigravity conversation ID")
	}
	message := ""
	if event.TerminationReason == "error" && strings.TrimSpace(event.Error) != "" {
		message = "Antigravity 执行失败：" + event.Error
	} else {
		message, err = antigravityAssistantMessage(event.TranscriptPath, event.ConversationID)
		if err != nil {
			return err
		}
	}
	payload, err := json.Marshal(map[string]any{"session_id": event.ConversationID, "last_assistant_message": message})
	if err != nil {
		return err
	}
	return postAgentTurnCompleted(payload)
}

func antigravityAssistantMessage(path, conversationID string) (string, error) {
	path = filepath.Clean(path)
	logs := filepath.Dir(path)
	conversation := filepath.Dir(filepath.Dir(logs))
	if !validCodexThreadID(conversationID) || !filepath.IsAbs(path) || filepath.Base(path) != "transcript.jsonl" || filepath.Base(logs) != "logs" || filepath.Base(filepath.Dir(logs)) != ".system_generated" || filepath.Base(conversation) != conversationID {
		return "", errors.New("Antigravity transcript does not match the event conversation")
	}
	// The compact transcript can truncate replies. Read the same conversation's
	// full transcript when present; never search another conversation or HOME.
	full := filepath.Join(logs, "transcript_full.jsonl")
	if _, err := os.Stat(full); err == nil {
		path = full
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<20 {
		return "", errors.New("Antigravity transcript is not a regular file or exceeds 64 MiB")
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	message := ""
	for scanner.Scan() {
		var step struct {
			Type      string            `json:"type"`
			Source    string            `json:"source"`
			Status    string            `json:"status"`
			Content   json.RawMessage   `json:"content"`
			ToolCalls []json.RawMessage `json:"tool_calls"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &step); err != nil {
			return "", fmt.Errorf("parse Antigravity transcript: %w", err)
		}
		if step.Type == "USER_INPUT" {
			message = ""
		}
		if step.Type == "PLANNER_RESPONSE" && step.Source == "MODEL" {
			message = ""
			if step.Status == "DONE" && len(step.ToolCalls) == 0 {
				_ = json.Unmarshal(step.Content, &message)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(message) == "" {
		return "", errors.New("Antigravity transcript has no final assistant reply for the current turn")
	}
	return strings.TrimSpace(message), nil
}

func antigravityConversationID(command string) string {
	argv := shellFields(command)
	for i, arg := range argv {
		if arg == "--conversation" && i+1 < len(argv) && validCodexThreadID(argv[i+1]) {
			return argv[i+1]
		}
		if id, ok := strings.CutPrefix(arg, "--conversation="); ok && validCodexThreadID(id) {
			return id
		}
	}
	return ""
}

func pinAntigravityResumeCommand(command, conversationID string) (string, bool) {
	if !validCodexThreadID(conversationID) {
		return command, false
	}
	argv := shellFields(command)
	start := 0
	for start < len(argv) && isShellEnvAssignment(argv[start]) {
		start++
	}
	if start >= len(argv) || shellCommandBase(argv[start]) != "agy" {
		return command, false
	}
	result := append([]string{}, argv[:start+1]...)
	for i := start + 1; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--continue" || arg == "-c" || strings.HasPrefix(arg, "--conversation=") {
			continue
		}
		if arg == "--conversation" {
			i++
			continue
		}
		result = append(result, arg)
	}
	result = append(result, "--conversation", conversationID)
	resume := joinShellCommand(result[start:])
	if start > 0 {
		resume = joinEnvAssignments(result[:start]) + " " + resume
	}
	return resume, true
}
