package session

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const defaultAgentUpgradeTimeout = 3 * time.Minute

type AgentUpgradeResult struct {
	Kind          string
	BeforeVersion string
	AfterVersion  string
	Output        string
	Err           error
}

type agentUpgradeRunner interface {
	LookPath(string) (string, error)
	Run(context.Context, string, ...string) (string, error)
}

type execAgentUpgradeRunner struct{}

func (execAgentUpgradeRunner) LookPath(name string) (string, error) {
	if resolved, err := exec.LookPath(name); err == nil {
		return resolved, nil
	}
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".local", "bin", name),
		filepath.Join(home, ".node", "bin", name),
		filepath.Join(home, ".npm-global", "bin", name),
		filepath.Join(home, "bin", name),
		filepath.Join("/opt/homebrew/bin", name),
		filepath.Join("/usr/local/bin", name),
	}
	if prefix := strings.TrimSpace(os.Getenv("NPM_CONFIG_PREFIX")); prefix != "" {
		candidates = append(candidates, filepath.Join(prefix, "bin", name))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}

func (execAgentUpgradeRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = agentUpgradeEnvironment()
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	return strings.TrimSpace(output.String()), err
}

func agentUpgradeEnvironment() []string {
	home, _ := os.UserHomeDir()
	pathEntries := []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, ".node", "bin"),
		filepath.Join(home, ".npm-global", "bin"),
		filepath.Join(home, "bin"),
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/usr/bin",
		"/bin",
	}
	if prefix := strings.TrimSpace(os.Getenv("NPM_CONFIG_PREFIX")); prefix != "" {
		pathEntries = append([]string{filepath.Join(prefix, "bin")}, pathEntries...)
	}
	if current := strings.TrimSpace(os.Getenv("PATH")); current != "" {
		pathEntries = append(pathEntries, strings.Split(current, string(os.PathListSeparator))...)
	}
	seen := map[string]bool{}
	path := make([]string, 0, len(pathEntries))
	for _, entry := range pathEntries {
		entry = strings.TrimSpace(entry)
		if entry == "" || seen[entry] {
			continue
		}
		seen[entry] = true
		path = append(path, entry)
	}
	environment := make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "PATH=") {
			environment = append(environment, value)
		}
	}
	return append(environment, "PATH="+strings.Join(path, string(os.PathListSeparator)))
}

func UpgradeAgentCLIsOnStartup(ctx context.Context, sessions []Session, configured AgentConfig) []AgentUpgradeResult {
	return upgradeAgentCLIs(ctx, sessions, configured, execAgentUpgradeRunner{})
}

func upgradeAgentCLIs(ctx context.Context, sessions []Session, configured AgentConfig, runner agentUpgradeRunner) []AgentUpgradeResult {
	kinds := startupAgentUpgradeKinds(sessions, configured)
	results := make([]AgentUpgradeResult, 0, len(kinds))
	for _, kind := range kinds {
		executable := agentUpgradeExecutable(kind)
		if executable == "" {
			continue
		}
		resolved, err := runner.LookPath(executable)
		if err != nil {
			results = append(results, AgentUpgradeResult{Kind: kind, Err: err})
			continue
		}
		upgradeCtx, cancel := context.WithTimeout(ctx, defaultAgentUpgradeTimeout)
		before, beforeErr := runner.Run(upgradeCtx, resolved, "--version")
		output, updateErr := runner.Run(upgradeCtx, resolved, "update")
		after, afterErr := runner.Run(upgradeCtx, resolved, "--version")
		cancel()
		result := AgentUpgradeResult{Kind: kind, BeforeVersion: before, AfterVersion: after, Output: output}
		result.Err = errors.Join(beforeErr, updateErr, afterErr)
		results = append(results, result)
	}
	return results
}

func startupAgentUpgradeKinds(sessions []Session, configured AgentConfig) []string {
	kinds := map[string]bool{}
	add := func(kind, command string) {
		kind = strings.ToLower(strings.TrimSpace(kind))
		if kind != "codex" && kind != "claude" {
			kind = recognizedUpgradeableAgentKind(command)
		}
		if kind == "codex" || kind == "claude" {
			kinds[kind] = true
		}
	}
	add(configured.Kind, configured.Command)
	for _, sess := range sessions {
		if !sess.Live || strings.TrimSpace(sess.LastMode) != SessionModeAgent {
			continue
		}
		command := sess.LastAgentStartCommand
		if strings.TrimSpace(command) == "" {
			command = sess.LastAgentResumeCommand
		}
		add(sess.LastAgentKind, command)
	}
	out := make([]string, 0, len(kinds))
	for kind := range kinds {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

func recognizedUpgradeableAgentKind(command string) string {
	for _, segment := range strings.Split(command, ";") {
		argv := shellFields(segment)
		for len(argv) > 0 && isShellEnvAssignment(argv[0]) {
			argv = argv[1:]
		}
		if len(argv) == 0 {
			continue
		}
		switch strings.ToLower(strings.TrimSuffix(filepath.Base(argv[0]), ".exe")) {
		case "codex":
			return "codex"
		case "claude", "claude-code":
			return "claude"
		}
	}
	return ""
}

func agentUpgradeExecutable(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "codex":
		return "codex"
	case "claude":
		return "claude"
	default:
		return ""
	}
}
