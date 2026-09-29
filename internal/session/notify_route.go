package session

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type agentNotifyRoute struct {
	URL       string `json:"url"`
	SessionID string `json:"session_id"`
	Token     string `json:"token"`
}

// Bind notify to the launched session's config, not the shared daemon's environment.
// The private file keeps credentials out of terminal output and process arguments.
func (rt *RuntimeSession) agentNotifyLaunchCommand(command string) (string, error) {
	if rt.manager == nil || !isCodexFamily(agentKindForCommand(command, "")) {
		return command, nil
	}
	sess := rt.Snapshot()
	dir := rt.manager.sessionRecoveryDir(sess)
	if dir == "" || rt.manager.AgentTurnHookURL() == "" {
		return command, nil
	}
	routeFile := filepath.Join(filepath.Dir(dir), "notify-routes", fmt.Sprintf("%x.json", sha256.Sum256([]byte(sess.RecoveryKey))))
	data, err := json.Marshal(agentNotifyRoute{URL: rt.manager.AgentTurnHookURL(), SessionID: sess.ID, Token: sess.RecoveryKey})
	if err != nil {
		return "", err
	}
	if err := writeFileAtomically(routeFile, data, 0600); err != nil {
		return "", err
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	home := defaultCodexHome()
	config := "config.toml"
	if agentKindForCommand(command, "") == "traecli" {
		home, config = defaultTraeHome(), "traecli.toml"
	}
	var forward []string
	if content, err := os.ReadFile(filepath.Join(home, config)); err == nil {
		_, _, forward, _, err = findTopLevelNotify(content)
		if err != nil {
			return "", err
		}
		if isManagedCodexNotify(forward) {
			forward, err = managedCodexNotifyForward(forward)
			if err != nil {
				return "", err
			}
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	notify, err := managedCodexNotify(executable, forward)
	if err != nil {
		return "", err
	}
	notify = append(notify, "--route-file", routeFile)
	value, err := json.Marshal(notify)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(command) + " -c " + shellQuote("notify="+string(value)), nil
}
