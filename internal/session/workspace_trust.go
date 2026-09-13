package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Trust only the directory being used, not a wildcard parent such as / or HOME.
// Include the repository/main worktree root because the CLIs key trust there.
func workspaceTrustPaths(cwd string) []string {
	if strings.TrimSpace(cwd) == "" {
		return nil
	}
	path, err := filepath.Abs(cwd)
	if err != nil {
		return nil
	}
	paths := []string{path}
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != path {
		paths = append(paths, resolved)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir").Output()
	if err == nil {
		for _, root := range strings.Split(strings.TrimSpace(string(output)), "\n") {
			if filepath.Base(root) == ".git" {
				root = filepath.Dir(root)
			}
			if filepath.IsAbs(root) && root != path && (len(paths) == 1 || paths[len(paths)-1] != root) {
				paths = append(paths, root)
			}
		}
	}
	return paths
}

func (rt *RuntimeSession) prepareAgentWorkspaceTrust(command string) {
	sess := rt.Snapshot()
	kind := agentKindForCommand(command, sess.LastAgentKind)
	if kind != "claude" {
		// Native Aiden's agentFull already skips its workspace permission check.
		// Codex's TUI requires persisted trust even with -c overrides; let its
		// native menu record the decision via autoTrustWorkspaceLocked instead.
		return
	}
	paths := workspaceTrustPaths(sess.LastCWD)
	if rt.manager != nil {
		if home := rt.manager.sessionClaudeHome(sess); home != "" {
			if err := ensureClaudeWorkspaceTrust(home, paths); err != nil {
				log.Printf("workspace trust setup failed session=%s: %v", sess.ID, err)
			}
		}
	}
}

func ensureClaudeWorkspaceTrust(home string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	path := filepath.Join(home, ".claude.json")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	config := map[string]json.RawMessage{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &config); err != nil {
			return err // Never replace an unreadable configuration.
		}
	}
	if config == nil {
		return fmt.Errorf("invalid Claude configuration: null")
	}
	projects := map[string]map[string]json.RawMessage{}
	if raw := config["projects"]; len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &projects); err != nil {
			return err
		}
	}
	for _, path := range paths {
		if projects[path] == nil {
			projects[path] = map[string]json.RawMessage{}
		}
		projects[path]["hasTrustDialogAccepted"] = json.RawMessage("true")
	}
	config["projects"], err = json.Marshal(projects)
	if err != nil {
		return err
	}
	updated, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	// Replace the session's symlink with a private copy. Never follow it on write
	// and accidentally change ~/.claude.json or lose another session's updates.
	return writeFileAtomically(path, append(updated, '\n'), 0o600)
}

// Only accept a live, fully rendered directory-trust menu, never a historical
// copy above the shell/composer or an unrelated command/permission dialog.
func workspaceTrustMenu(snapshot, source, kind string) (string, bool) {
	metadata := parseSnapshotSourceContinuity(source)
	if !metadata.valid || !isBufferSnapshotContinuityBase(metadata.base) {
		return "", false
	}
	lines := splitVisibleLines(snapshot)
	cursor := metadata.cursorLine
	if kind == "codex" {
		// Codex hides the hardware cursor at the footer, not on the highlighted
		// option. Require exactly one selected option in the final active menu.
		cursor = -1
		for i, line := range lines {
			if m := terminalInteractionOptionRE.FindStringSubmatch(line); m != nil {
				if _, marked := submittedInputPromptText(line); marked {
					if cursor >= 0 {
						return "", false
					}
					cursor = i
				}
			}
		}
	}
	if cursor < 0 || cursor >= len(lines) {
		return "", false
	}
	selected, ok := submittedInputPromptText(lines[cursor])
	if !ok {
		return "", false
	}
	option := func(line string) string {
		line = strings.TrimSpace(line)
		if m := terminalInteractionOptionRE.FindStringSubmatch(line); m != nil {
			line = m[2]
		}
		return strings.ToLower(line)
	}
	yes := "yes, i trust this folder"
	header := "accessing workspace:"
	if kind == "codex" {
		yes = "yes, continue"
		header = "do you trust the contents of this directory?"
	} else if kind != "claude" {
		return "", false
	}
	selected = option(selected)
	if selected != yes && selected != "no, exit" && selected != "no, quit" {
		return "", false
	}
	start, end, yesLine := -1, -1, -1
	for i, line := range lines {
		lower := strings.ToLower(strings.TrimSpace(line))
		if i < cursor && strings.Contains(lower, header) {
			start = i
		}
		if i > cursor && strings.Contains(lower, "enter") &&
			(strings.Contains(lower, "confirm") || strings.Contains(lower, "continue")) {
			end = i
			break
		}
	}
	if start < 0 || end < 0 || end-start > 25 || !codexInteractionHasActiveTail(lines[end+1:]) {
		return "", false
	}
	if kind == "codex" && metadata.cursorLine >= 0 && (metadata.cursorLine < start || metadata.cursorLine > end) {
		return "", false
	}
	for i := start + 1; i < end; i++ {
		line := lines[i]
		if text, marked := submittedInputPromptText(line); marked {
			line = text
		}
		if option(line) == yes {
			if yesLine >= 0 {
				return "", false
			}
			yesLine = i
		}
	}
	if yesLine < 0 {
		return "", false
	}
	if selected == yes {
		return "\r", true
	}
	if yesLine < cursor {
		return "\x1b[A", true
	}
	return "\x1b[B", true
}

func (rt *RuntimeSession) autoTrustWorkspaceLocked() bool {
	if rt.closed || !rt.session.Live || rt.terminal == nil || rt.session.LastMode != SessionModeAgent {
		return false
	}
	kind := agentKindForCommand(rt.session.LastAgentStartCommand, rt.session.LastAgentKind)
	key, menu := workspaceTrustMenu(rt.visibleSnapshot, rt.visibleSnapshotSource, kind)
	if !menu {
		if startupAgentComposerReady(rt.visibleSnapshot, rt.visibleSnapshotSource, kind) {
			rt.workspaceTrustAction = ""
		}
		return false
	}
	if rt.workspaceTrustAction == "confirm" || (key != "\r" && rt.workspaceTrustAction == "select") {
		return false
	}
	// Wait for a new snapshot showing Yes selected before sending Enter. Ink can
	// otherwise process Down+Enter against the same old (No) selection.
	rt.workspaceTrustAction = "select"
	if key == "\r" {
		rt.workspaceTrustAction = "confirm"
	}
	if _, err := rt.terminal.Write([]byte(key)); err != nil {
		log.Printf("workspace trust selection failed session=%s: %v", rt.session.ID, err)
		return false // Do not retry an ambiguous partial write.
	}
	log.Printf("workspace trust selection session=%s agent=%s action=%s", rt.session.ID, kind, rt.workspaceTrustAction)
	return true
}

// Probe a settled screen even when Feishu notifications are disabled. Raw
// output only requests a snapshot; it never authorizes a keypress itself.
func (rt *RuntimeSession) scheduleWorkspaceTrustProbeLocked(chunk []byte) {
	kind := agentKindForCommand(rt.session.LastAgentStartCommand, rt.session.LastAgentKind)
	if rt.manager == nil || rt.session.LastMode != SessionModeAgent || (kind != "claude" && kind != "codex") {
		return
	}
	text := rt.workspaceTrustProbeTail + string(chunk)
	rt.workspaceTrustProbeTail = text[max(0, len(text)-128):]
	if rt.workspaceTrustProbe == nil && !strings.Contains(strings.ToLower(text), "trust") {
		return
	}
	if rt.workspaceTrustProbe != nil {
		rt.workspaceTrustProbe.Stop()
	}
	version := rt.session.HistorySize
	rt.workspaceTrustProbe = time.AfterFunc(300*time.Millisecond, func() {
		rt.mu.Lock()
		if rt.closed || rt.session.HistorySize != version {
			rt.mu.Unlock()
			return
		}
		rt.workspaceTrustProbe = nil
		rt.mu.Unlock()
		rt.RequestFreshSnapshot(defaultNotifySnapshotTimeout)
	})
}
