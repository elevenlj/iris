package session

import (
	"path/filepath"
	"strings"
	"unicode"
)

const terminalAgentContextTailLineLimit = 80

type TerminalAgentContext struct {
	Directory string
	Model     string
	Reasoning string
}

func DetectCodexTerminalAgentContext(text string) *TerminalAgentContext {
	lines := terminalAgentContextTailLines(splitVisibleLines(text))
	hasCodexHeader := false
	modelLine := ""
	directory := ""
	for _, line := range lines {
		line = cleanTerminalContextLine(line)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "openai codex") {
			hasCodexHeader = true
		}
		if value, ok := terminalContextLineValue(line, "model"); ok {
			modelLine = value
		}
		if value, ok := terminalContextLineValue(line, "directory"); ok {
			directory = value
		}
	}
	if hasCodexHeader && modelLine != "" && directory != "" {
		model, reasoning := splitCodexModelAndReasoning(modelLine)
		if model != "" {
			return &TerminalAgentContext{
				Directory: sanitizeForLarkAudit(directory),
				Model:     sanitizeForLarkAudit(model),
				Reasoning: reasoning,
			}
		}
	}
	return detectCodexStatusLineContext(lines)
}

func terminalAgentContextTailLines(lines []string) []string {
	if len(lines) <= terminalAgentContextTailLineLimit {
		return lines
	}
	return lines[len(lines)-terminalAgentContextTailLineLimit:]
}

func detectCodexStatusLineContext(lines []string) *TerminalAgentContext {
	for i := len(lines) - 1; i >= 0; i-- {
		line := cleanTerminalContextLine(lines[i])
		separator := strings.LastIndex(line, "·")
		if separator < 0 {
			continue
		}
		status := strings.TrimSpace(line[:separator])
		if !strings.Contains(strings.ToLower(status), "fast") {
			continue
		}
		model, reasoning := splitCodexModelAndReasoning(status)
		directory := strings.TrimSpace(line[separator+len("·"):])
		if model == "" || reasoning == "" || !looksLikeCodexModel(model) || !looksLikeTerminalDirectory(directory) {
			continue
		}
		return newCodexTerminalAgentContext(directory, model, reasoning)
	}
	return nil
}

func looksLikeCodexModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(model, "gpt-") || strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3") ||
		strings.HasPrefix(model, "o4") || strings.Contains(model, "codex")
}

func looksLikeTerminalDirectory(directory string) bool {
	directory = strings.TrimSpace(directory)
	return strings.HasPrefix(directory, "~") || strings.HasPrefix(directory, "/")
}

func newCodexTerminalAgentContext(directory, model, reasoning string) *TerminalAgentContext {
	return &TerminalAgentContext{
		Directory: sanitizeForLarkAudit(directory),
		Model:     sanitizeForLarkAudit(model),
		Reasoning: reasoning,
	}
}

func cleanTerminalContextLine(line string) string {
	line = strings.TrimSpace(line)
	line = strings.Trim(line, "│┃| ")
	return strings.TrimSpace(line)
}

func terminalContextLineValue(line string, key string) (string, bool) {
	colon := strings.IndexRune(line, ':')
	if colon < 0 {
		return "", false
	}
	if !strings.EqualFold(strings.TrimSpace(line[:colon]), key) {
		return "", false
	}
	value := strings.TrimSpace(line[colon+1:])
	return value, value != ""
}

func splitCodexModelAndReasoning(modelLine string) (string, string) {
	fields := strings.Fields(modelLine)
	for i := 0; i < len(fields); i++ {
		current := normalizeCodexReasoningToken(fields[i])
		if current == "extra" && i+1 < len(fields) && normalizeCodexReasoningToken(fields[i+1]) == "high" {
			return strings.Join(fields[:i], " "), "Extra high"
		}
		if reasoning, ok := codexReasoningLabel(current); ok {
			return strings.Join(fields[:i], " "), reasoning
		}
	}
	return strings.TrimSpace(modelLine), ""
}

func normalizeCodexReasoningToken(value string) string {
	return strings.ToLower(strings.TrimFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("()[]{}.,:;", r)
	}))
}

func codexReasoningLabel(value string) (string, bool) {
	switch value {
	case "minimal":
		return "Minimal", true
	case "low":
		return "Low", true
	case "medium", "med":
		return "Medium", true
	case "high":
		return "High", true
	case "xhigh", "x-high", "extrahigh", "extra-high":
		return "Extra high", true
	case "max":
		return "Max", true
	case "ultra":
		return "Ultra", true
	default:
		return "", false
	}
}

func cloneTerminalAgentContext(context *TerminalAgentContext) *TerminalAgentContext {
	if context == nil {
		return nil
	}
	cloned := *context
	return &cloned
}

func (rt *RuntimeSession) notificationAgentContextLocked() *TerminalAgentContext {
	if strings.TrimSpace(rt.visibleSnapshot) == "" {
		return rt.applyPendingAgentDirectoryLocked(cloneTerminalAgentContext(rt.lastTerminalAgentContext))
	}
	context := DetectCodexTerminalAgentContext(rt.visibleSnapshot)
	if context == nil {
		if strings.TrimSpace(rt.pendingAgentDirectory) != "" {
			return rt.applyPendingAgentDirectoryLocked(cloneTerminalAgentContext(rt.lastTerminalAgentContext))
		}
		rt.lastTerminalAgentContext = nil
		return nil
	}
	context = rt.applyPendingAgentDirectoryLocked(context)
	rt.lastTerminalAgentContext = cloneTerminalAgentContext(context)
	return cloneTerminalAgentContext(context)
}

func (rt *RuntimeSession) applyPendingAgentDirectoryLocked(context *TerminalAgentContext) *TerminalAgentContext {
	pending := strings.TrimSpace(rt.pendingAgentDirectory)
	if context == nil || pending == "" {
		return context
	}
	current, currentOK := resolveShellCWD("", "", context.Directory)
	wanted, wantedOK := resolveShellCWD("", "", pending)
	if currentOK && wantedOK && current == wanted {
		rt.pendingAgentDirectory = ""
		return context
	}
	context.Directory = compactTerminalDirectory(pending)
	return context
}

func compactTerminalDirectory(path string) string {
	path = strings.TrimSpace(path)
	home := strings.TrimSpace(userHomeDir())
	if path == home {
		return "~"
	}
	if home != "" && strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}
