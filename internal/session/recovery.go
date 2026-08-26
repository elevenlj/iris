package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func newRecoveryKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")
}

func (rt *RuntimeSession) MarkAgentExitActivity() {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		return
	}
	rt.session.LastMode = SessionModeShell
	rt.session.UpdatedAt = time.Now().UTC()
	s := rt.session
	rt.mu.Unlock()
	if rt.manager != nil {
		_ = rt.manager.persist(context.Background(), s)
	}
}

func (rt *RuntimeSession) RecordShellCommandForRecovery(command string) {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		return
	}
	rt.updateRecoveryFromSubmittedInputLocked(command)
	s := rt.session
	rt.mu.Unlock()
	if rt.manager != nil {
		_ = rt.manager.persist(context.Background(), s)
	}
}

func (rt *RuntimeSession) ConfigureAgentForRecovery(agent AgentConfig) {
	if rt == nil {
		return
	}
	agent = normalizeAgentConfig(agent)
	if agent.Command == "" {
		return
	}
	rt.RecordShellCommandForRecovery(agent.Command)
	rt.mu.Lock()
	rt.session.LastAgentID = agent.ID
	if agent.Kind != "custom" {
		s := rt.session
		rt.mu.Unlock()
		if rt.manager != nil {
			_ = rt.manager.persist(context.Background(), s)
		}
		return
	}
	if rt.session.LastMode == SessionModeAgent && rt.session.LastAgentKind != "" && rt.session.LastAgentKind != "custom" {
		s := rt.session
		rt.mu.Unlock()
		if rt.manager != nil {
			_ = rt.manager.persist(context.Background(), s)
		}
		return
	}
	rt.session.LastMode = SessionModeAgent
	rt.session.LastAgentKind = "custom"
	rt.session.LastAgentStartCommand = agent.Command
	rt.session.LastAgentResumeCommand = agent.Command
	rt.session.UpdatedAt = time.Now().UTC()
	s := rt.session
	rt.mu.Unlock()
	if rt.manager != nil {
		_ = rt.manager.persist(context.Background(), s)
	}
}

func (rt *RuntimeSession) updateRecoveryFromSubmittedInputLocked(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if strings.TrimSpace(rt.session.LastMode) == SessionModeAgent {
		if isAgentExitInput(text) {
			rt.session.LastMode = SessionModeShell
			rt.session.UpdatedAt = time.Now().UTC()
		}
		return
	}
	if strings.TrimSpace(rt.session.LastMode) == "" {
		rt.session.LastMode = SessionModeShell
	}
	if strings.TrimSpace(rt.session.LastCWD) == "" && rt.manager != nil {
		rt.session.LastCWD = rt.manager.defaultWorkingDir()
	}
	for _, line := range strings.Split(text, "\n") {
		for _, segment := range splitShellSegments(line) {
			rt.applyShellSegmentForRecoveryLocked(segment)
		}
	}
	rt.session.UpdatedAt = time.Now().UTC()
}

func (rt *RuntimeSession) applyShellSegmentForRecoveryLocked(segment string) {
	argv := shellFields(segment)
	if len(argv) == 0 {
		return
	}
	var envArgs []string
	for len(argv) > 0 && isShellEnvAssignment(argv[0]) {
		envArgs = append(envArgs, argv[0])
		argv = argv[1:]
	}
	if len(argv) == 0 {
		return
	}
	cmd := shellCommandBase(argv[0])
	switch cmd {
	case "cd":
		rt.applyCDForRecoveryLocked(argv[1:])
	case "builtin", "command":
		if len(argv) > 1 && shellCommandBase(argv[1]) == "cd" {
			rt.applyCDForRecoveryLocked(argv[2:])
		}
	default:
		if info, ok := agentLaunchInfo(argv); ok {
			if len(envArgs) > 0 {
				info.ResumeCommand = strings.TrimSpace(joinEnvAssignments(envArgs) + " " + info.ResumeCommand)
			}
			rt.session.LastMode = SessionModeAgent
			rt.session.LastAgentKind = info.Kind
			rt.session.LastAgentStartCommand = strings.TrimSpace(segment)
			rt.session.LastAgentResumeCommand = info.ResumeCommand
			if rt.manager != nil {
				for _, option := range rt.manager.AvailableAgentOptions() {
					if strings.TrimSpace(option.Command) == strings.TrimSpace(segment) {
						rt.session.LastAgentID = option.ID
						break
					}
				}
			}
			if rt.manager != nil {
				rt.session.LastAgentHome = rt.manager.sessionAgentHome(rt.session, info.Kind)
			}
		}
	}
}

func (rt *RuntimeSession) applyCDForRecoveryLocked(args []string) {
	target := ""
	if len(args) == 0 {
		target = userHomeDir()
	} else {
		target = args[0]
	}
	current := strings.TrimSpace(rt.session.LastCWD)
	if current == "" && rt.manager != nil {
		current = rt.manager.defaultWorkingDir()
	}
	next, ok := resolveShellCWD(current, rt.session.LastPrevCWD, target)
	if !ok || next == "" {
		return
	}
	rt.session.LastPrevCWD = current
	rt.session.LastCWD = next
}

func splitShellSegments(line string) []string {
	var out []string
	var b strings.Builder
	quote := rune(0)
	escaped := false
	flush := func() {
		if s := strings.TrimSpace(b.String()); s != "" {
			out = append(out, s)
		}
		b.Reset()
	}
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			b.WriteRune(r)
			escaped = true
			continue
		}
		if quote != 0 {
			b.WriteRune(r)
			if r == quote {
				quote = 0
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			b.WriteRune(r)
			continue
		}
		if r == ';' {
			flush()
			continue
		}
		if (r == '&' || r == '|') && i+1 < len(runes) && runes[i+1] == r {
			flush()
			i++
			continue
		}
		b.WriteRune(r)
	}
	flush()
	return out
}

func shellFields(s string) []string {
	var out []string
	var b strings.Builder
	quote := rune(0)
	escaped := false
	for _, r := range s {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				b.WriteRune(r)
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
			if b.Len() > 0 {
				out = append(out, b.String())
				b.Reset()
			}
			continue
		}
		b.WriteRune(r)
	}
	if escaped {
		b.WriteRune('\\')
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

func isShellEnvAssignment(arg string) bool {
	if strings.HasPrefix(arg, "-") {
		return false
	}
	i := strings.IndexByte(arg, '=')
	if i <= 0 {
		return false
	}
	name := arg[:i]
	for j, r := range name {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || j > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func shellCommandBase(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}
	return filepath.Base(cmd)
}

func resolveShellCWD(current, previous, target string) (string, bool) {
	target = strings.TrimSpace(target)
	if target == "" || target == "~" {
		return userHomeDir(), true
	}
	if target == "-" {
		if strings.TrimSpace(previous) == "" {
			return "", false
		}
		return previous, true
	}
	if strings.Contains(target, "$") {
		if target == "$HOME" || strings.HasPrefix(target, "$HOME/") {
			target = userHomeDir() + strings.TrimPrefix(target, "$HOME")
		} else if target == "${HOME}" || strings.HasPrefix(target, "${HOME}/") {
			target = userHomeDir() + strings.TrimPrefix(target, "${HOME}")
		} else {
			return "", false
		}
	}
	if strings.HasPrefix(target, "~/") {
		target = filepath.Join(userHomeDir(), strings.TrimPrefix(target, "~/"))
	}
	if filepath.IsAbs(target) {
		return filepath.Clean(target), true
	}
	if strings.TrimSpace(current) == "" {
		current = userHomeDir()
	}
	return filepath.Clean(filepath.Join(current, target)), true
}

func userHomeDir() string {
	if dir, err := os.UserHomeDir(); err == nil && dir != "" {
		return dir
	}
	return "."
}

func isAgentExitInput(text string) bool {
	text = strings.TrimSpace(text)
	switch text {
	case "exit", "quit", "/exit", "/quit":
		return true
	default:
		return false
	}
}

type agentInfo struct {
	Kind          string
	ResumeCommand string
}

func agentLaunchInfo(argv []string) (agentInfo, bool) {
	if len(argv) == 0 {
		return agentInfo{}, false
	}
	cmd := shellCommandBase(argv[0])
	args := argv[1:]
	switch cmd {
	case "codex":
		return codexAgentInfo(argv[0], args)
	case "claude", "claude-code":
		return claudeAgentInfo(argv[0], args)
	case "aiden":
		if len(args) >= 2 && args[0] == "x" && args[1] == "codex" {
			return codexAgentInfoWithPrefix([]string{argv[0], "x", "codex"}, args[2:])
		}
		return genericAgentInfo(cmd, argv[0], args)
	case "gemini", "opencode":
		return genericAgentInfo(cmd, argv[0], args)
	default:
		return agentInfo{}, false
	}
}

func codexAgentInfo(command string, args []string) (agentInfo, bool) {
	return codexAgentInfoWithPrefix([]string{command}, args)
}

func codexAgentInfoWithPrefix(command []string, args []string) (agentInfo, bool) {
	if hasAnyArg(args, "--version", "-V", "--help", "-h") {
		return agentInfo{}, false
	}
	sub := firstCodexSubcommand(args)
	switch sub {
	case "exec", "review", "login", "logout", "mcp", "plugin", "mcp-server", "app-server", "remote-control", "app", "completion", "update", "sandbox", "debug", "apply", "cloud", "exec-server", "features", "help":
		return agentInfo{}, false
	case "resume", "fork":
		return agentInfo{Kind: "codex", ResumeCommand: joinShellCommand(append(append([]string(nil), command...), args...))}, true
	default:
		flags := preserveCLIFlags(args)
		resume := append(append(append([]string(nil), command...), "resume", "--last"), flags...)
		return agentInfo{Kind: "codex", ResumeCommand: joinShellCommand(resume)}, true
	}
}

func firstCodexSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return ""
		}
		if strings.HasPrefix(arg, "-") {
			if cliFlagTakesValue(arg) && !strings.Contains(arg, "=") {
				i++
			}
			continue
		}
		return arg
	}
	return ""
}

func claudeAgentInfo(command string, args []string) (agentInfo, bool) {
	if hasAnyArg(args, "--version", "-v", "--help", "-h") {
		return agentInfo{}, false
	}
	if hasAnyArg(args, "--resume", "--continue") {
		return agentInfo{Kind: "claude", ResumeCommand: joinShellCommand(append([]string{command}, args...))}, true
	}
	flags := preserveCLIFlags(args)
	resume := append([]string{command, "--continue"}, flags...)
	return agentInfo{Kind: "claude", ResumeCommand: joinShellCommand(resume)}, true
}

func genericAgentInfo(kind, command string, args []string) (agentInfo, bool) {
	if hasAnyArg(args, "--version", "-v", "-V", "--help", "-h") {
		return agentInfo{}, false
	}
	if hasResumeLikeArg(args) {
		return agentInfo{Kind: kind, ResumeCommand: joinShellCommand(append([]string{command}, args...))}, true
	}
	return agentInfo{Kind: kind, ResumeCommand: joinShellCommand(append([]string{command}, args...))}, true
}

func preserveCLIFlags(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			break
		}
		out = append(out, arg)
		if cliFlagTakesValue(arg) && !strings.Contains(arg, "=") && i+1 < len(args) {
			i++
			out = append(out, args[i])
		}
	}
	return out
}

func cliFlagTakesValue(arg string) bool {
	switch arg {
	case "-c", "--config", "-i", "--image", "-m", "--model", "-p", "--profile", "-s", "--sandbox", "-C", "--cd", "--add-dir", "-a", "--ask-for-approval", "--local-provider", "--remote", "--remote-auth-token-env":
		return true
	default:
		return false
	}
}

func hasAnyArg(args []string, values ...string) bool {
	want := map[string]bool{}
	for _, v := range values {
		want[v] = true
	}
	for _, arg := range args {
		if want[arg] {
			return true
		}
	}
	return false
}

func hasResumeLikeArg(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "resume", "--resume", "--continue", "continue", "--last":
			return true
		}
	}
	return false
}

func joinShellCommand(args []string) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		if strings.TrimSpace(arg) == "" {
			continue
		}
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func joinEnvAssignments(args []string) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		i := strings.IndexByte(arg, '=')
		if i <= 0 {
			continue
		}
		parts = append(parts, arg[:i]+"="+shellQuote(arg[i+1:]))
	}
	return strings.Join(parts, " ")
}

func ensureClaudeSessionHome(claudeHome string) error {
	claudeHome = strings.TrimSpace(claudeHome)
	if claudeHome == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(claudeHome, "sessions"), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(claudeHome, "projects"), 0o755); err != nil {
		return err
	}
	source := sourceClaudeHome(claudeHome)
	for _, name := range []string{".claude.json", "settings.json", "settings.local.json", "plugins", "commands", "agents", "hooks", "skills", "output-styles"} {
		if err := linkAgentHomeEntry(source, claudeHome, name); err != nil {
			log.Printf("claude home link skipped name=%s: %v", name, err)
		}
	}
	return nil
}

func defaultCodexHome() string {
	home := strings.TrimSpace(userHomeDir())
	if home == "" || home == "." {
		return ""
	}
	return filepath.Join(home, ".codex")
}

func codexHomeIsLegacy(home string) bool {
	home = strings.TrimSpace(home)
	defaultHome := defaultCodexHome()
	return home != "" && defaultHome != "" && filepath.Clean(home) != filepath.Clean(defaultHome)
}

func (m *Manager) prepareCodexRecovery(sess Session) (Session, error) {
	if strings.TrimSpace(sess.LastAgentKind) != "codex" || strings.TrimSpace(sess.LastAgentResumeCommand) == "" {
		return sess, nil
	}
	defaultHome := defaultCodexHome()
	if defaultHome == "" {
		return sess, errors.New("cannot resolve default Codex home")
	}
	legacyHome := strings.TrimSpace(sess.LastAgentHome)
	if legacyHome == "" || filepath.Clean(legacyHome) == filepath.Clean(defaultHome) {
		sess.LastAgentHome = defaultHome
		return sess, nil
	}

	wantedID := codexResumeThreadID(sess.LastAgentResumeCommand)
	rollout, threadID, err := findCodexRollout(legacyHome, wantedID)
	if err != nil {
		return sess, err
	}
	if err := migrateCodexRollout(legacyHome, defaultHome, rollout); err != nil {
		return sess, err
	}
	if wantedID == "" {
		command, ok := pinCodexResumeCommand(sess.LastAgentResumeCommand, threadID)
		if !ok {
			return sess, fmt.Errorf("cannot pin Codex recovery command %q", sess.LastAgentResumeCommand)
		}
		sess.LastAgentResumeCommand = command
	}
	sess.LastAgentHome = defaultHome
	return sess, nil
}

type codexSessionMeta struct {
	Payload struct {
		SessionID    string          `json:"session_id"`
		ThreadSource json.RawMessage `json:"thread_source"`
	} `json:"payload"`
}

func findCodexRollout(codexHome, wantedID string) (string, string, error) {
	root := filepath.Join(codexHome, "sessions")
	var selectedPath, selectedID string
	var selectedTime time.Time
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "rollout-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		id := codexThreadIDFromRolloutName(entry.Name())
		if id == "" || (wantedID != "" && id != wantedID) {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		var meta codexSessionMeta
		decodeErr := json.NewDecoder(file).Decode(&meta)
		_ = file.Close()
		if decodeErr != nil {
			return decodeErr
		}
		if meta.Payload.SessionID != "" {
			id = meta.Payload.SessionID
		}
		if !validCodexThreadID(id) || (wantedID != "" && id != wantedID) {
			return nil
		}
		if wantedID == "" && strings.Contains(strings.ToLower(string(meta.Payload.ThreadSource)), "subagent") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if selectedPath == "" || info.ModTime().After(selectedTime) {
			selectedPath, selectedID, selectedTime = path, id, info.ModTime()
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	if selectedPath == "" {
		if wantedID != "" {
			return "", "", fmt.Errorf("Codex session %s not found under %s", wantedID, root)
		}
		return "", "", fmt.Errorf("no Codex session found under %s", root)
	}
	return selectedPath, selectedID, nil
}

func codexThreadIDFromRolloutName(name string) string {
	name = strings.TrimSuffix(name, ".jsonl")
	if len(name) < 36 {
		return ""
	}
	id := name[len(name)-36:]
	if validCodexThreadID(id) {
		return id
	}
	return ""
}

func validCodexThreadID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if id[i] != '-' {
				return false
			}
			continue
		}
		if !((id[i] >= '0' && id[i] <= '9') || (id[i] >= 'a' && id[i] <= 'f') || (id[i] >= 'A' && id[i] <= 'F')) {
			return false
		}
	}
	return true
}

func codexResumeThreadID(command string) string {
	argv := shellFields(command)
	for i, arg := range argv {
		if arg != "resume" {
			continue
		}
		for _, candidate := range argv[i+1:] {
			if validCodexThreadID(candidate) {
				return candidate
			}
		}
		break
	}
	return ""
}

func exactAgentResumeCommand(sess Session) string {
	command := strings.TrimSpace(sess.LastAgentResumeCommand)
	switch strings.ToLower(strings.TrimSpace(sess.LastAgentKind)) {
	case "codex":
		if codexResumeThreadID(command) != "" {
			return command
		}
	case "claude":
		if claudeResumeSessionID(command) != "" {
			return command
		}
	}
	return ""
}

func claudeResumeSessionID(command string) string {
	argv := shellFields(command)
	for i, arg := range argv {
		if arg == "--resume" && i+1 < len(argv) && validCodexThreadID(argv[i+1]) {
			return argv[i+1]
		}
		if strings.HasPrefix(arg, "--resume=") {
			id := strings.TrimPrefix(arg, "--resume=")
			if validCodexThreadID(id) {
				return id
			}
		}
	}
	return ""
}

func pinCodexResumeCommand(command, threadID string) (string, bool) {
	if !validCodexThreadID(threadID) {
		return command, false
	}
	argv := shellFields(command)
	envEnd := 0
	for envEnd < len(argv) && isShellEnvAssignment(argv[envEnd]) {
		envEnd++
	}
	if envEnd >= len(argv) {
		return command, false
	}
	base := shellCommandBase(argv[envEnd])
	if base != "codex" && (base != "aiden" || envEnd+2 >= len(argv) || argv[envEnd+1] != "x" || argv[envEnd+2] != "codex") {
		return command, false
	}
	replaced := false
	for i := envEnd + 1; i < len(argv); i++ {
		if argv[i] == "--last" {
			argv[i] = threadID
			replaced = true
			break
		}
	}
	if !replaced {
		return command, false
	}
	result := joinShellCommand(argv[envEnd:])
	if envEnd > 0 {
		result = strings.TrimSpace(joinEnvAssignments(argv[:envEnd]) + " " + result)
	}
	return result, true
}

func pinClaudeResumeCommand(command, sessionID string) (string, bool) {
	if !validCodexThreadID(sessionID) {
		return command, false
	}
	argv := shellFields(command)
	envEnd := 0
	for envEnd < len(argv) && isShellEnvAssignment(argv[envEnd]) {
		envEnd++
	}
	if envEnd >= len(argv) {
		return command, false
	}
	base := shellCommandBase(argv[envEnd])
	if base != "claude" && base != "claude-code" {
		return command, false
	}
	args := make([]string, 0, len(argv)-envEnd+1)
	resumeSet := false
	for index := envEnd + 1; index < len(argv); index++ {
		arg := argv[index]
		switch {
		case arg == "--continue" || arg == "-c":
			if !resumeSet {
				args = append(args, "--resume", sessionID)
				resumeSet = true
			}
		case arg == "--resume" || arg == "-r":
			if !resumeSet {
				args = append(args, "--resume", sessionID)
				resumeSet = true
			}
			if index+1 < len(argv) && !strings.HasPrefix(argv[index+1], "-") {
				index++
			}
		case strings.HasPrefix(arg, "--resume="):
			if !resumeSet {
				args = append(args, "--resume", sessionID)
				resumeSet = true
			}
		default:
			args = append(args, arg)
		}
	}
	if !resumeSet {
		args = append([]string{"--resume", sessionID}, args...)
	}
	result := joinShellCommand(append([]string{argv[envEnd]}, args...))
	if envEnd > 0 {
		result = strings.TrimSpace(joinEnvAssignments(argv[:envEnd]) + " " + result)
	}
	return result, true
}

func migrateCodexRollout(legacyHome, defaultHome, source string) error {
	rel, err := filepath.Rel(filepath.Join(legacyHome, "sessions"), source)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("invalid legacy Codex rollout path %s", source)
	}
	target := filepath.Join(defaultHome, "sessions", rel)
	if _, err := os.Stat(target); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := os.Link(source, target); err == nil {
		return nil
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(target)
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	return nil
}

func sourceClaudeHome(target string) string {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" && filepath.Clean(dir) != filepath.Clean(target) {
		return dir
	}
	return filepath.Join(userHomeDir(), ".claude")
}

func linkAgentHomeEntry(source, target, name string) error {
	src := filepath.Join(source, name)
	if name == ".claude.json" {
		if _, err := os.Stat(src); os.IsNotExist(err) {
			src = filepath.Join(userHomeDir(), ".claude.json")
		}
	}
	dst := filepath.Join(target, name)
	if _, err := os.Stat(src); err != nil {
		return nil
	}
	if _, err := os.Lstat(dst); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Symlink(src, dst); err == nil {
		return nil
	}
	if info, err := os.Stat(src); err == nil && !info.IsDir() {
		b, readErr := os.ReadFile(src)
		if readErr != nil {
			return readErr
		}
		return os.WriteFile(dst, b, 0o600)
	}
	return nil
}
