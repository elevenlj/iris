package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	codexNotifyModeFlag                    = "--codex-notify"
	codexNotifyForwardFlag                 = "--forward-base64"
	legacyEasyTerminalCodexStopHookCommand = `if [ -z "${EASY_TERMINAL_HOOK_URL:-}" ] || [ -z "${EASY_TERMINAL_SESSION_ID:-}" ] || [ -z "${EASY_TERMINAL_HOOK_TOKEN:-}" ]; then exit 0; fi; /usr/bin/curl --silent --max-time 2 -o /dev/null -X POST "${EASY_TERMINAL_HOOK_URL}/api/sessions/${EASY_TERMINAL_SESSION_ID}/hook/turn-ended" -H "X-Easy-Terminal-Hook-Token: ${EASY_TERMINAL_HOOK_TOKEN}" -H 'Content-Type: application/json' --data-binary @- >/dev/null 2>&1 || true`
)

// IsCodexNotifyInvocation reports whether the executable was launched by the
// Codex notify command rather than as the Iris server.
func IsCodexNotifyInvocation(args []string) bool {
	return len(args) > 0 && args[0] == codexNotifyModeFlag
}

// EnsureCodexNotify installs Iris as Codex's notify command, preserves any
// existing notification command as a downstream recipient, and removes only
// the legacy Stop hook previously managed by Iris.
func EnsureCodexNotify(executable string) error {
	home := defaultCodexHome()
	if home == "" {
		return errors.New("cannot resolve default Codex home")
	}
	executable = strings.TrimSpace(executable)
	if executable == "" {
		return errors.New("cannot resolve Iris executable")
	}
	if absolute, err := filepath.Abs(executable); err == nil {
		executable = absolute
	}
	if err := ensureCodexNotifyConfig(filepath.Join(home, "config.toml"), executable); err != nil {
		return err
	}
	return removeLegacyCodexStopHook(filepath.Join(home, "hooks.json"))
}

func ensureCodexNotifyConfig(path, executable string) error {
	mode := os.FileMode(0o600)
	content, err := os.ReadFile(path)
	if err == nil {
		if info, statErr := os.Stat(path); statErr == nil {
			mode = info.Mode().Perm()
		}
	} else if errors.Is(err, os.ErrNotExist) {
		content = nil
	} else {
		return err
	}

	start, end, current, found, err := findTopLevelNotify(content)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	forward := current
	if isManagedCodexNotify(current) {
		forward, err = managedCodexNotifyForward(current)
		if err != nil {
			return fmt.Errorf("parse managed notify in %s: %w", path, err)
		}
	} else if isLegacyCodexNotify(current) {
		forward = append([]string(nil), current[1:]...)
	}
	managed, err := managedCodexNotify(executable, forward)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(managed)
	if err != nil {
		return err
	}
	replacement := append([]byte("notify = "), encoded...)
	replacement = append(replacement, '\n')

	var updated []byte
	if found {
		updated = make([]byte, 0, len(content)-(end-start)+len(replacement))
		updated = append(updated, content[:start]...)
		updated = append(updated, replacement...)
		updated = append(updated, content[end:]...)
	} else {
		updated = make([]byte, 0, len(content)+len(replacement)+1)
		updated = append(updated, replacement...)
		if len(content) > 0 && content[0] != '\n' {
			updated = append(updated, '\n')
		}
		updated = append(updated, content...)
	}
	if bytes.Equal(content, updated) {
		return nil
	}
	return writeFileAtomically(path, updated, mode)
}

func managedCodexNotify(executable string, forward []string) ([]string, error) {
	result := []string{executable, codexNotifyModeFlag}
	if len(forward) == 0 {
		return result, nil
	}
	data, err := json.Marshal(forward)
	if err != nil {
		return nil, err
	}
	return append(result, codexNotifyForwardFlag, base64.RawURLEncoding.EncodeToString(data)), nil
}

func isManagedCodexNotify(command []string) bool {
	return len(command) >= 2 && command[1] == codexNotifyModeFlag
}

func managedCodexNotifyForward(command []string) ([]string, error) {
	if len(command) == 2 {
		return nil, nil
	}
	if len(command) != 4 || command[2] != codexNotifyForwardFlag {
		return nil, errors.New("invalid managed notify arguments")
	}
	data, err := base64.RawURLEncoding.DecodeString(command[3])
	if err != nil {
		return nil, err
	}
	var forward []string
	if err := json.Unmarshal(data, &forward); err != nil {
		return nil, err
	}
	if len(forward) > 0 && strings.TrimSpace(forward[0]) == "" {
		return nil, errors.New("empty forwarded command")
	}
	return forward, nil
}

func isLegacyCodexNotify(command []string) bool {
	return len(command) > 0 && filepath.Base(command[0]) == "codex-turn-ended-hook.sh"
}

func findTopLevelNotify(content []byte) (start, end int, values []string, found bool, err error) {
	offset := 0
	for offset < len(content) {
		lineEnd := bytes.IndexByte(content[offset:], '\n')
		if lineEnd < 0 {
			lineEnd = len(content)
		} else {
			lineEnd += offset
		}
		line := content[offset:lineEnd]
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 && trimmed[0] == '[' {
			break
		}
		equal := bytes.IndexByte(line, '=')
		if equal >= 0 && strings.TrimSpace(string(line[:equal])) == "notify" {
			arrayStart := offset + equal + 1
			for arrayStart < len(content) && (content[arrayStart] == ' ' || content[arrayStart] == '\t' || content[arrayStart] == '\r' || content[arrayStart] == '\n') {
				arrayStart++
			}
			arrayEnd, scanErr := scanTOMLArrayEnd(content, arrayStart)
			if scanErr != nil {
				return 0, 0, nil, false, scanErr
			}
			values, scanErr = parseTOMLStringArray(content[arrayStart:arrayEnd])
			if scanErr != nil {
				return 0, 0, nil, false, scanErr
			}
			end = arrayEnd
			for end < len(content) && content[end] != '\n' {
				end++
			}
			if end < len(content) {
				end++
			}
			return offset, end, values, true, nil
		}
		if lineEnd == len(content) {
			break
		}
		offset = lineEnd + 1
	}
	return 0, 0, nil, false, nil
}

func scanTOMLArrayEnd(content []byte, start int) (int, error) {
	if start >= len(content) || content[start] != '[' {
		return 0, errors.New("notify must be an array")
	}
	quote := byte(0)
	escaped := false
	comment := false
	for index := start + 1; index < len(content); index++ {
		current := content[index]
		if comment {
			if current == '\n' {
				comment = false
			}
			continue
		}
		if quote != 0 {
			if quote == '"' && escaped {
				escaped = false
				continue
			}
			if quote == '"' && current == '\\' {
				escaped = true
				continue
			}
			if current == quote {
				quote = 0
			}
			continue
		}
		switch current {
		case '"', '\'':
			quote = current
		case '#':
			comment = true
		case ']':
			return index + 1, nil
		}
	}
	return 0, errors.New("unterminated notify array")
}

func parseTOMLStringArray(data []byte) ([]string, error) {
	index := 0
	skip := func() {
		for index < len(data) {
			switch data[index] {
			case ' ', '\t', '\r', '\n', ',':
				index++
			case '#':
				for index < len(data) && data[index] != '\n' {
					index++
				}
			default:
				return
			}
		}
	}
	skip()
	if index >= len(data) || data[index] != '[' {
		return nil, errors.New("notify must be an array")
	}
	index++
	var result []string
	for {
		skip()
		if index >= len(data) {
			return nil, errors.New("unterminated notify array")
		}
		if data[index] == ']' {
			return result, nil
		}
		quote := data[index]
		if quote != '"' && quote != '\'' {
			return nil, fmt.Errorf("notify entry at byte %d must be a string", index)
		}
		start := index
		index++
		if quote == '\'' {
			valueStart := index
			for index < len(data) && data[index] != '\'' {
				index++
			}
			if index >= len(data) {
				return nil, errors.New("unterminated literal string in notify")
			}
			result = append(result, string(data[valueStart:index]))
			index++
			continue
		}
		escaped := false
		for index < len(data) {
			current := data[index]
			index++
			if escaped {
				escaped = false
				continue
			}
			if current == '\\' {
				escaped = true
				continue
			}
			if current == '"' {
				break
			}
		}
		if index > len(data) || data[index-1] != '"' {
			return nil, errors.New("unterminated basic string in notify")
		}
		value, unquoteErr := strconv.Unquote(string(data[start:index]))
		if unquoteErr != nil {
			return nil, unquoteErr
		}
		result = append(result, value)
	}
}

func removeLegacyCodexStopHook(path string) error {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	}
	root := map[string]any{}
	if err := json.Unmarshal(content, &root); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return nil
	}
	stop, ok := hooks["Stop"].([]any)
	if !ok {
		return nil
	}
	changed := false
	keptGroups := make([]any, 0, len(stop))
	for _, group := range stop {
		groupMap, groupOK := group.(map[string]any)
		if !groupOK {
			keptGroups = append(keptGroups, group)
			continue
		}
		handlers, handlersOK := groupMap["hooks"].([]any)
		if !handlersOK {
			keptGroups = append(keptGroups, group)
			continue
		}
		keptHandlers := make([]any, 0, len(handlers))
		for _, handler := range handlers {
			handlerMap, _ := handler.(map[string]any)
			command, _ := handlerMap["command"].(string)
			if command == legacyEasyTerminalCodexStopHookCommand {
				changed = true
				continue
			}
			keptHandlers = append(keptHandlers, handler)
		}
		if len(keptHandlers) > 0 {
			groupMap["hooks"] = keptHandlers
			keptGroups = append(keptGroups, groupMap)
		}
	}
	if !changed {
		return nil
	}
	if len(keptGroups) == 0 {
		delete(hooks, "Stop")
	} else {
		hooks["Stop"] = keptGroups
	}
	updated, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	updated = append(updated, '\n')
	return writeFileAtomically(path, updated, mode)
}

func writeFileAtomically(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".iris-notify-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// RunCodexNotify forwards the official Codex notification to Iris and then to
// the notification command that existed before Iris was installed.
func RunCodexNotify(args []string) error {
	if len(args) == 0 {
		return errors.New("missing Codex notify payload")
	}
	payload := args[len(args)-1]
	forward := []string(nil)
	if len(args) > 1 {
		if len(args) != 3 || args[0] != codexNotifyForwardFlag {
			return errors.New("invalid Codex notify invocation")
		}
		data, err := base64.RawURLEncoding.DecodeString(args[1])
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &forward); err != nil {
			return err
		}
	}
	if len(forward) > 0 {
		command := exec.Command(forward[0], append(forward[1:], payload)...)
		command.Stdin = nil
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		if err := command.Start(); err == nil {
			_ = command.Process.Release()
		}
	}
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return fmt.Errorf("parse Codex notify payload: %w", err)
	}
	if event.Type != "agent-turn-complete" {
		return nil
	}
	return postAgentTurnCompleted([]byte(payload))
}

func postAgentTurnCompleted(payload []byte) error {
	hookURL := firstNonEmptyEnv("IRIS_API_URL", "EASY_TERMINAL_HOOK_URL")
	hookURL = strings.TrimRight(strings.TrimSpace(hookURL), "/")
	sessionID := strings.TrimSpace(firstNonEmptyEnv("IRIS_SESSION_ID", "EASY_TERMINAL_SESSION_ID"))
	token := strings.TrimSpace(firstNonEmptyEnv("IRIS_SESSION_TOKEN", "EASY_TERMINAL_HOOK_TOKEN"))
	if hookURL == "" || sessionID == "" || token == "" {
		return nil
	}
	endpoint := hookURL + "/api/sessions/" + url.PathEscape(sessionID) + "/hook/turn-ended"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Iris-Agent-Token", token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Iris notify callback returned HTTP %d", response.StatusCode)
	}
	return nil
}

func firstNonEmptyEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}
