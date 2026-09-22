package session

import (
	"errors"
	"path/filepath"
	"strings"
)

func defaultTraeHome() string {
	if home := firstNonEmptyEnv("TRAE_HOME"); home != "" {
		return home
	}
	if home := strings.TrimSpace(userHomeDir()); home != "" && home != "." {
		return filepath.Join(home, ".trae")
	}
	return ""
}

func normalizeTraeRecovery(sess Session) Session {
	if sess.LastMode != SessionModeAgent || agentKindForCommand(sess.LastAgentStartCommand, sess.LastAgentKind) != "traecli" {
		return sess
	}
	sess.LastAgentKind = "traecli"
	sess.LastAgentHome = defaultTraeHome()
	// Old custom sessions have no recorded thread ID. Never resume an arbitrary
	// latest session from the directory; only reuse a known exact session.
	if exactAgentResumeCommand(sess) == "" {
		sess.LastAgentResumeCommand = sess.LastAgentStartCommand
	}
	return sess
}

// TRAE CLI 2.x emits the same notify payload as Codex, but has its own config.
func EnsureTraeNotify(executable string) error {
	home := defaultTraeHome()
	if home == "" || strings.TrimSpace(executable) == "" {
		return errors.New("cannot resolve TRAE home or Iris executable")
	}
	absolute, err := filepath.Abs(executable)
	if err != nil {
		return err
	}
	return ensureCodexNotifyConfig(filepath.Join(home, "traecli.toml"), absolute)
}
