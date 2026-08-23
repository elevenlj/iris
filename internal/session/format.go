package session

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

var emailRE = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

const (
	defaultMaxLarkTextLines     = 300
	defaultFallbackTailLines    = 100
	maxInputAnchorRunes         = 30
	maxLarkTextRunes            = 12000
	larkTruncatedPrefix         = "[truncated]\n"
	codexNoAnchorFallbackPrefix = "[missing input anchor; showing tail]\n"
)

var larkNotifyMaxLines atomic.Int64
var larkNotifyFallbackTailLines atomic.Int64
var larkNotifyDropLinePatterns atomic.Value
var larkNotifyMergeWrappedLines atomic.Bool

type larkNotifyDropLinePattern struct {
	kind   string
	action string
	re     *regexp.Regexp
	groups []int
}

func init() {
	larkNotifyMaxLines.Store(defaultMaxLarkTextLines)
	larkNotifyFallbackTailLines.Store(defaultFallbackTailLines)
	larkNotifyDropLinePatterns.Store([]larkNotifyDropLinePattern{})
}

func SetLarkNotifyMaxLines(lines int) {
	if lines <= 0 {
		lines = defaultMaxLarkTextLines
	}
	larkNotifyMaxLines.Store(int64(lines))
}

func SetLarkNotifyFallbackTailLines(lines int) {
	if lines <= 0 {
		lines = defaultFallbackTailLines
	}
	larkNotifyFallbackTailLines.Store(int64(lines))
}

func SetLarkNotifyMergeWrappedLines(enabled bool) {
	larkNotifyMergeWrappedLines.Store(enabled)
}

func SetLarkNotifyDropLinePatterns(patterns []string) error {
	rules := make([]LarkNotifyDropLineRule, 0, len(patterns))
	for _, pattern := range patterns {
		rules = append(rules, LarkNotifyDropLineRule{Pattern: pattern})
	}
	return SetLarkNotifyDropLineRules(rules)
}

func SetLarkNotifyDropLineRules(rules []LarkNotifyDropLineRule) error {
	compiled := make([]larkNotifyDropLinePattern, 0, len(rules))
	for _, rule := range rules {
		pattern := strings.TrimSpace(rule.Pattern)
		if pattern == "" {
			continue
		}
		kind := normalizeLarkNotifyRuleKind(rule.Kind)
		if kind == "" {
			kind = "line"
		}
		if kind != "line" && kind != "block_head" && kind != "line_group" {
			return fmt.Errorf("invalid lark notify filter kind %q", rule.Kind)
		}
		action := normalizeLarkNotifyRuleAction(kind, rule.Action)
		if kind == "block_head" && action != "drop_block" && action != "keep_head" {
			return fmt.Errorf("invalid lark notify block filter action %q", rule.Action)
		}
		groups := normalizeLarkNotifyRuleGroups(rule.Groups)
		re, err := regexp.Compile(pattern)
		if err != nil {
			title := strings.TrimSpace(rule.Title)
			if title != "" {
				return fmt.Errorf("invalid lark notify drop line pattern %q (%s): %w", pattern, title, err)
			}
			return fmt.Errorf("invalid lark notify drop line pattern %q: %w", pattern, err)
		}
		if kind == "line_group" {
			if len(groups) == 0 {
				return fmt.Errorf("lark notify line group filter %q requires at least one capture group", pattern)
			}
			for _, group := range groups {
				if group > re.NumSubexp() {
					return fmt.Errorf("lark notify line group filter %q references missing capture group %d", pattern, group)
				}
			}
		}
		compiled = append(compiled, larkNotifyDropLinePattern{kind: kind, action: action, re: re, groups: groups})
	}
	larkNotifyDropLinePatterns.Store(compiled)
	return nil
}

func sanitizeForLarkAudit(text string) string {
	return emailRE.ReplaceAllString(text, "[email]")
}

func truncateForLark(text string) string {
	text = truncateLinesFromTail(text, int(larkNotifyMaxLines.Load()), "")
	return truncateRunesFromTail(text, maxLarkTextRunes, larkTruncatedPrefix)
}

func pickLarkNotifyHookAssistantContent(message string) string {
	body := trimVisibleText(message)
	body = applyConfiguredLarkNotifyFilters(body)
	body = trimVisibleText(body)
	if body == "" {
		return ""
	}
	return truncateForLark(sanitizeForLarkAudit(body))
}

// pickLarkNotifyFallbackTailContent is the explicit no-anchor fallback. It
// keeps the newest configured number of visible lines while still applying
// the same prompt cleanup, user filters, audit sanitization, and hard card
// limits as an anchored notification.
func pickLarkNotifyFallbackTailContent(visibleSnapshot string) string {
	body := trimVisibleText(visibleSnapshot)
	body = dropCodexPromptStatusLines(body)
	body = applyConfiguredLarkNotifyFilters(body)
	body = trimVisibleText(body)
	if body == "" {
		return ""
	}
	body = truncateLinesFromTail(body, int(larkNotifyFallbackTailLines.Load()), "")
	return truncateForLark(sanitizeForLarkAudit(body))
}

// pickLarkManualRefreshFallbackTailContent preserves terminal state lines such
// as Codex "Working (...)" for an explicit user refresh. User-configured
// line/block filters still apply, so tool output suppression remains intact.
func pickLarkManualRefreshFallbackTailContent(visibleSnapshot string) string {
	body := trimVisibleText(visibleSnapshot)
	body = dropCodexFooterStatusLines(body)
	body = applyConfiguredLarkNotifyFilters(body)
	body = trimVisibleText(body)
	if body == "" {
		return ""
	}
	body = truncateLinesFromTail(body, int(larkNotifyFallbackTailLines.Load()), "")
	return truncateForLark(sanitizeForLarkAudit(body))
}

func applyConfiguredLarkNotifyFilters(text string) string {
	patterns, _ := larkNotifyDropLinePatterns.Load().([]larkNotifyDropLinePattern)
	if len(patterns) == 0 || text == "" {
		return text
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = applyConfiguredLarkNotifyBlockFilters(text, patterns)
	return applyConfiguredLarkNotifyLineFilters(text, patterns)
}

func applyConfiguredLarkNotifyBlockFilters(text string, patterns []larkNotifyDropLinePattern) string {
	hasBlockFilter := false
	for _, pattern := range patterns {
		if pattern.kind == "block_head" {
			hasBlockFilter = true
			break
		}
	}
	if !hasBlockFilter {
		return text
	}
	lines := strings.Split(text, "\n")
	blocks := splitLarkNotifyBlocks(lines)
	kept := make([]string, 0, len(lines))
	for _, block := range blocks {
		if len(block) == 0 {
			continue
		}
		action := ""
		for _, pattern := range patterns {
			if pattern.kind == "block_head" && pattern.re.MatchString(block[0]) {
				action = pattern.action
				break
			}
		}
		switch action {
		case "drop_block":
			continue
		case "keep_head":
			kept = append(kept, block[0])
		default:
			kept = append(kept, block...)
		}
	}
	return strings.Join(kept, "\n")
}

func splitLarkNotifyBlocks(lines []string) [][]string {
	blocks := make([][]string, 0, len(lines))
	var current []string
	currentUsesMarkerBoundary := false
	for _, line := range lines {
		markerBoundary := startsLarkNotifyMarkerBlock(line)
		startsNext := markerBoundary || (!currentUsesMarkerBoundary && startsLarkNotifyBlock(line))
		if startsNext && len(current) > 0 {
			blocks = append(blocks, current)
			current = nil
			currentUsesMarkerBoundary = markerBoundary
		} else if len(current) == 0 {
			currentUsesMarkerBoundary = markerBoundary
		}
		current = append(current, line)
	}
	if len(current) > 0 {
		blocks = append(blocks, current)
	}
	return blocks
}

// dropLeadingUntitledCodexFragment removes the visible tail of a Codex block
// whose marker/title has already scrolled out of the terminal viewport. A
// later bullet marker proves that the snapshot is using Codex's marker-based
// block layout. We still require a Codex tool-continuation signal in the
// prefix, so ordinary prose before a marker is preserved.
func dropLeadingUntitledCodexFragment(text string) string {
	if strings.TrimSpace(text) == "" {
		return text
	}
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), "\n")
	firstContent := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if firstContent < 0 {
			firstContent = i
			if startsLarkNotifyMarkerBlock(line) {
				return text
			}
			continue
		}
		if startsLarkNotifyMarkerBlock(line) {
			if looksLikeUntitledCodexToolFragment(lines[firstContent:i]) {
				return strings.Join(lines[i:], "\n")
			}
			return text
		}
	}
	return text
}

func looksLikeUntitledCodexToolFragment(lines []string) bool {
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		switch {
		case strings.HasPrefix(trimmed, "└"),
			strings.HasPrefix(trimmed, "├"),
			strings.Contains(lower, "ctrl + t to view transcript"),
			strings.HasPrefix(lower, "process exited with code "):
			return true
		}
	}
	return false
}

func cleanLarkNotifyContentForAgent(text string, mode string, agentKind string) string {
	if strings.TrimSpace(mode) != SessionModeAgent || !strings.EqualFold(strings.TrimSpace(agentKind), "codex") {
		return text
	}
	return dropLeadingUntitledCodexFragment(text)
}

func startsLarkNotifyMarkerBlock(line string) bool {
	trimmed := strings.TrimLeftFunc(line, unicode.IsSpace)
	if trimmed == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(trimmed)
	return r == '•' || r == '⏺'
}

func startsLarkNotifyBlock(line string) bool {
	if strings.TrimSpace(line) == "" {
		return false
	}
	for _, r := range line {
		return !unicode.IsSpace(r)
	}
	return false
}

func applyConfiguredLarkNotifyLineFilters(text string, patterns []larkNotifyDropLinePattern) string {
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	for _, line := range lines {
		drop := false
		for _, pattern := range patterns {
			switch pattern.kind {
			case "line":
				if pattern.re.MatchString(line) {
					drop = true
				}
			case "line_group":
				line = applyLarkNotifyLineGroupFilter(line, pattern)
			}
			if drop {
				break
			}
		}
		if !drop {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

type larkNotifyDropRange struct {
	start int
	end   int
}

func applyLarkNotifyLineGroupFilter(line string, pattern larkNotifyDropLinePattern) string {
	matches := pattern.re.FindAllStringSubmatchIndex(line, -1)
	if len(matches) == 0 {
		return line
	}
	ranges := make([]larkNotifyDropRange, 0, len(matches)*len(pattern.groups))
	for _, match := range matches {
		for _, group := range pattern.groups {
			index := group * 2
			if index+1 >= len(match) || match[index] < 0 || match[index+1] <= match[index] {
				continue
			}
			ranges = append(ranges, larkNotifyDropRange{start: match[index], end: match[index+1]})
		}
	}
	if len(ranges) == 0 {
		return line
	}
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].start == ranges[j].start {
			return ranges[i].end < ranges[j].end
		}
		return ranges[i].start < ranges[j].start
	})
	merged := ranges[:0]
	for _, item := range ranges {
		if len(merged) == 0 || item.start > merged[len(merged)-1].end {
			merged = append(merged, item)
			continue
		}
		if item.end > merged[len(merged)-1].end {
			merged[len(merged)-1].end = item.end
		}
	}
	var b strings.Builder
	start := 0
	for _, item := range merged {
		if item.start > start {
			b.WriteString(line[start:item.start])
		}
		start = item.end
	}
	if start < len(line) {
		b.WriteString(line[start:])
	}
	return b.String()
}

func mergeTerminalWrappedLinesForLark(text string) string {
	if text == "" {
		return text
	}
	lines := strings.Split(text, "\n")
	if len(lines) <= 1 {
		return text
	}
	var b strings.Builder
	b.WriteString(lines[0])
	inCodeFence := isMarkdownCodeFenceLine(lines[0])
	for i := 1; i < len(lines); i++ {
		currentFence := isMarkdownCodeFenceLine(lines[i])
		previousFence := isMarkdownCodeFenceLine(lines[i-1])
		if !inCodeFence && !currentFence && !previousFence && shouldMergeTerminalWrappedLineBreak(lines[i-1], lines[i]) {
			b.WriteString(lines[i])
			continue
		}
		b.WriteByte('\n')
		b.WriteString(lines[i])
		if currentFence {
			inCodeFence = !inCodeFence
		}
	}
	return b.String()
}

func isMarkdownCodeFenceLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < 3 {
		return false
	}
	return strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
}

func shouldMergeTerminalWrappedLineBreak(left, right string) bool {
	if strings.TrimSpace(left) == "" || strings.TrimSpace(right) == "" {
		return false
	}
	leftEdge, ok := lastBoundaryRune(left)
	if !ok || isLarkLineBreakSeparator(leftEdge) {
		return false
	}
	rightEdge, ok := firstBoundaryRune(right)
	if !ok || isLarkLineBreakSeparator(rightEdge) {
		return false
	}
	if startsWithOrderedListMarker(strings.TrimSpace(right)) {
		return false
	}
	if startsWithNumberedDiffLine(strings.TrimSpace(right)) {
		return false
	}
	return true
}

func startsWithNumberedDiffLine(text string) bool {
	i := 0
	for i < len(text) && text[i] >= '0' && text[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(text) || (text[i] != ' ' && text[i] != '\t') {
		return false
	}
	for i < len(text) && (text[i] == ' ' || text[i] == '\t') {
		i++
	}
	return i < len(text) && (text[i] == '+' || text[i] == '-')
}

func lastBoundaryRune(text string) (rune, bool) {
	text = strings.TrimRightFunc(text, unicode.IsSpace)
	if text == "" {
		return 0, false
	}
	var last rune
	for _, r := range text {
		last = r
	}
	return last, true
}

func firstBoundaryRune(text string) (rune, bool) {
	text = strings.TrimLeftFunc(text, unicode.IsSpace)
	for _, r := range text {
		return r, true
	}
	return 0, false
}

func isLarkLineBreakSeparator(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r)
}

func startsWithOrderedListMarker(text string) bool {
	runes := []rune(text)
	if len(runes) >= 2 && isCJKListNumber(runes[0]) {
		switch runes[1] {
		case '、', '.', '．', ')', '）':
			return true
		}
	}
	digits := 0
	for _, r := range text {
		if !unicode.IsDigit(r) {
			return digits > 0 && (r == '.' || r == ')' || r == '、' || r == '．')
		}
		digits++
		if digits > 4 {
			return false
		}
	}
	return false
}

func isCJKListNumber(r rune) bool {
	return strings.ContainsRune("一二三四五六七八九十", r)
}

func truncateLinesFromTail(text string, maxLines int, prefix string) string {
	lines := strings.Split(text, "\n")
	if len(lines) <= maxLines {
		return text
	}
	return prefix + strings.Join(lines[len(lines)-maxLines:], "\n")
}

func truncateRunesFromTail(text string, maxRunes int, prefix string) string {
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	prefixRunes := []rune(prefix)
	keep := maxRunes - len(prefixRunes)
	if keep < 1 {
		return string(runes[len(runes)-maxRunes:])
	}
	return prefix + string(runes[len(runes)-keep:])
}

func StripTerminalControls(data []byte) string {
	return compactRepeatedLines(stripTerminalControlsRaw(data))
}

func stripTerminalControlsRaw(data []byte) string {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch c {
		case 0x1b:
			i = skipEscape(data, i)
		case '\r':
			out = append(out, '\n')
		case '\b':
			if len(out) > 0 {
				_, size := utf8.DecodeLastRune(out)
				if size <= 0 {
					size = 1
				}
				out = out[:len(out)-size]
			}
		default:
			if c == '\n' || c == '\t' || (c >= 0x20 && c != 0x7f) {
				out = append(out, c)
			}
		}
	}
	return string(out)
}

func HasRenderableContent(data []byte) bool {
	text := StripTerminalControls(data)
	for _, r := range text {
		if r == '\n' || r == '\t' {
			continue
		}
		if !unicode.IsSpace(r) {
			return true
		}
	}
	return false
}

func PickNotifyContent(visibleSnapshot string, previousVisibleSnapshot string, roundReply []byte, lastInputText string) string {
	return pickNotifyContentWithWindow(visibleSnapshot, previousVisibleSnapshot, roundReply, lastInputText, "")
}

// notifyTextAnchorPolicy separates legacy/unit-call behavior from production
// renderer identity checks. Production snapshots carry a guard-line index for
// both frames; a textual occurrence is the same boundary only when it remains
// at the same logical offset from that live xterm marker.
type notifyTextAnchorPolicy struct {
	allowed            bool
	enforceIdentity    bool
	previousGuardLine  int
	currentGuardLine   int
	previousCursorLine int
	currentCursorLine  int
}

func permissiveNotifyTextAnchorPolicy(allowed bool) notifyTextAnchorPolicy {
	return notifyTextAnchorPolicy{
		allowed:            allowed,
		previousGuardLine:  -1,
		currentGuardLine:   -1,
		previousCursorLine: -1,
		currentCursorLine:  -1,
	}
}

func pickNotifyContentWithWindow(visibleSnapshot string, previousVisibleSnapshot string, roundReply []byte, lastInputText string, windowStartInputText string) string {
	return pickNotifyContentWithWindowPolicy(visibleSnapshot, previousVisibleSnapshot, roundReply, lastInputText, windowStartInputText, true)
}

func pickNotifyContentWithWindowPolicy(visibleSnapshot string, previousVisibleSnapshot string, roundReply []byte, lastInputText string, windowStartInputText string, allowPreviousTextAnchors bool) string {
	return pickNotifyContentWithWindowAnchorPolicy(visibleSnapshot, previousVisibleSnapshot, roundReply, lastInputText, windowStartInputText, permissiveNotifyTextAnchorPolicy(allowPreviousTextAnchors))
}

func pickNotifyContentWithWindowAnchorPolicy(visibleSnapshot string, previousVisibleSnapshot string, roundReply []byte, lastInputText string, windowStartInputText string, anchorPolicy notifyTextAnchorPolicy) string {
	if isRawLarkNotifyInput(lastInputText) {
		return pickRawNotifyContentWithWindowAnchorPolicy(visibleSnapshot, previousVisibleSnapshot, roundReply, lastInputText, windowStartInputText, anchorPolicy)
	}
	lastInputText = strings.TrimSpace(lastInputText)
	windowStartInputText = strings.TrimSpace(windowStartInputText)
	body, _ := selectNotifyBodyWithWindowAnchorPolicy(visibleSnapshot, previousVisibleSnapshot, roundReply, lastInputText, windowStartInputText, anchorPolicy)
	if body == "" {
		return ""
	}
	body = trimVisibleText(body)
	body = dropCodexPromptStatusLines(body)
	body = applyConfiguredLarkNotifyFilters(body)
	body = trimVisibleText(body)
	return truncateForLark(sanitizeForLarkAudit(body))
}

// pickManualRefreshNotifyContentWithWindowAnchorPolicy uses exactly the same
// submitted-input boundary as ordinary notifications, but keeps transient TUI
// state such as "Working (...)" so an explicit refresh reflects what is on the
// terminal right now.
func pickManualRefreshNotifyContentWithWindowAnchorPolicy(visibleSnapshot string, previousVisibleSnapshot string, roundReply []byte, lastInputText string, windowStartInputText string, anchorPolicy notifyTextAnchorPolicy) string {
	if isRawLarkNotifyInput(lastInputText) {
		return pickRawNotifyContentWithWindowAnchorPolicy(visibleSnapshot, previousVisibleSnapshot, roundReply, lastInputText, windowStartInputText, anchorPolicy)
	}
	inputText := strings.TrimSpace(windowStartInputText)
	if inputText == "" {
		inputText = strings.TrimSpace(lastInputText)
	}
	if _, ok := newestSubmittedInputAnchorSpan(strings.Split(visibleSnapshot, "\n"), inputText); !ok {
		return ""
	}
	body, _ := selectNotifyBodyWithWindowAnchorPolicy(visibleSnapshot, previousVisibleSnapshot, roundReply, lastInputText, windowStartInputText, anchorPolicy)
	if body == "" {
		return ""
	}
	body = trimVisibleText(body)
	body = dropCodexFooterStatusLines(body)
	body = applyConfiguredLarkNotifyFilters(body)
	body = trimVisibleText(body)
	return truncateForLark(sanitizeForLarkAudit(body))
}

// pickRawNotifyContentWithWindowAnchorPolicy is the explicit /c path. It uses
// the real round input, window boundary, and renderer identity when they can
// prove a boundary, but deliberately skips every notification transformation.
// If the boundary cannot be proven, /c is the one user-requested operation
// that may return the complete visible terminal snapshot.
func pickRawNotifyContentWithWindowAnchorPolicy(visibleSnapshot string, previousVisibleSnapshot string, roundReply []byte, lastInputText string, windowStartInputText string, anchorPolicy notifyTextAnchorPolicy) string {
	lastInputText = strings.TrimSpace(lastInputText)
	windowStartInputText = strings.TrimSpace(windowStartInputText)
	body, _ := selectNotifyBodyWithWindowAnchorPolicy(visibleSnapshot, previousVisibleSnapshot, roundReply, lastInputText, windowStartInputText, anchorPolicy)
	if strings.TrimSpace(body) == "" {
		body = visibleSnapshot
	}
	return strings.TrimSpace(body)
}

func isRawLarkNotifyInput(input string) bool {
	input = strings.TrimSpace(input)
	return input == "/c" || strings.HasPrefix(input, "/c ")
}

func shouldPreservePreviousNotifyContent(previous string, current string) bool {
	previous = strings.TrimSpace(previous)
	current = strings.TrimSpace(current)
	if previous == "" || current == "" || previous == RunningNotificationPlaceholder || current == RunningNotificationPlaceholder {
		return false
	}
	previousNorm := normalizedNotifyContentForRegression(previous)
	currentNorm := normalizedNotifyContentForRegression(current)
	if previousNorm == "" {
		return false
	}
	if currentNorm == "" {
		return true
	}
	previousRunes := []rune(previousNorm)
	currentRunes := []rune(currentNorm)
	if len(currentRunes) >= len(previousRunes) {
		return false
	}
	if strings.HasPrefix(previousNorm, currentNorm) {
		return len(currentRunes)*100 <= len(previousRunes)*95
	}
	common := commonPrefixRunes(previousNorm, currentNorm)
	return common >= 80 && common*100 >= len(currentRunes)*80 && len(currentRunes)*100 <= len(previousRunes)*85
}

func hasMeaningfulNotifyContent(text string) bool {
	return normalizedNotifyContentForRegression(text) != ""
}

func normalizedNotifyContentForRegression(text string) string {
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || isTransientStatusLine(trimmed) || isPromptStatusLine(trimmed) {
			continue
		}
		if text, ok := inputEchoText(trimmed); ok && strings.TrimSpace(text) == "" {
			continue
		}
		if text, ok := shellInputEchoText(trimmed); ok && strings.TrimSpace(text) == "" {
			continue
		}
		kept = append(kept, trimmed)
	}
	return strings.Join(strings.Fields(strings.Join(kept, "\n")), " ")
}

func commonPrefixRunes(left string, right string) int {
	leftRunes := []rune(left)
	rightRunes := []rune(right)
	limit := len(leftRunes)
	if len(rightRunes) < limit {
		limit = len(rightRunes)
	}
	for i := 0; i < limit; i++ {
		if leftRunes[i] != rightRunes[i] {
			return i
		}
	}
	return limit
}

func NotifyContentNeedsMoreSnapshot(visibleSnapshot string, previousVisibleSnapshot string, roundReply []byte, lastInputText string) bool {
	return notifyContentNeedsMoreSnapshotWithWindow(visibleSnapshot, previousVisibleSnapshot, roundReply, lastInputText, "")
}

func notifyContentNeedsMoreSnapshotWithWindow(visibleSnapshot string, previousVisibleSnapshot string, roundReply []byte, lastInputText string, windowStartInputText string) bool {
	return notifyContentNeedsMoreSnapshotWithWindowPolicy(visibleSnapshot, previousVisibleSnapshot, roundReply, lastInputText, windowStartInputText, true)
}

func notifyContentNeedsMoreSnapshotWithWindowPolicy(visibleSnapshot string, previousVisibleSnapshot string, roundReply []byte, lastInputText string, windowStartInputText string, allowPreviousTextAnchors bool) bool {
	return notifyContentNeedsMoreSnapshotWithWindowAnchorPolicy(visibleSnapshot, previousVisibleSnapshot, roundReply, lastInputText, windowStartInputText, permissiveNotifyTextAnchorPolicy(allowPreviousTextAnchors))
}

func notifyContentNeedsMoreSnapshotWithWindowAnchorPolicy(visibleSnapshot string, previousVisibleSnapshot string, roundReply []byte, lastInputText string, windowStartInputText string, anchorPolicy notifyTextAnchorPolicy) bool {
	lastInputText = strings.TrimSpace(lastInputText)
	windowStartInputText = strings.TrimSpace(windowStartInputText)
	body, _ := selectNotifyBodyWithWindowAnchorPolicy(visibleSnapshot, previousVisibleSnapshot, roundReply, lastInputText, windowStartInputText, anchorPolicy)
	if strings.TrimSpace(body) == "" {
		return true
	}
	if windowStartInputText != "" && lastInputText != "" && !containsInputEchoLine(trimVisibleText(body), lastInputText) {
		return true
	}
	hasReply := hasReplyLine(trimVisibleText(body), lastInputText)
	return !hasReply || (containsTransientStatusLine(body) && !hasReply)
}

func selectNotifyBody(visibleSnapshot string, previousVisibleSnapshot string, roundReply []byte, lastInputText string) (string, bool) {
	return selectNotifyBodyWithWindow(visibleSnapshot, previousVisibleSnapshot, roundReply, lastInputText, "")
}

func selectNotifyBodyWithWindow(visibleSnapshot string, previousVisibleSnapshot string, roundReply []byte, lastInputText string, windowStartInputText string) (string, bool) {
	return selectNotifyBodyWithWindowPolicy(visibleSnapshot, previousVisibleSnapshot, roundReply, lastInputText, windowStartInputText, true)
}

func selectNotifyBodyWithWindowPolicy(visibleSnapshot string, previousVisibleSnapshot string, roundReply []byte, lastInputText string, windowStartInputText string, allowPreviousTextAnchors bool) (string, bool) {
	return selectNotifyBodyWithWindowAnchorPolicy(visibleSnapshot, previousVisibleSnapshot, roundReply, lastInputText, windowStartInputText, permissiveNotifyTextAnchorPolicy(allowPreviousTextAnchors))
}

func selectNotifyBodyWithWindowAnchorPolicy(visibleSnapshot string, previousVisibleSnapshot string, roundReply []byte, lastInputText string, windowStartInputText string, anchorPolicy notifyTextAnchorPolicy) (string, bool) {
	if strings.TrimSpace(windowStartInputText) != "" {
		visibleBody, fromVisible := currentWindowVisibleText(visibleSnapshot, previousVisibleSnapshot, roundReply, windowStartInputText, anchorPolicy)
		if strings.TrimSpace(visibleBody) != "" {
			return trimVisibleText(visibleBody), fromVisible
		}
	}
	visibleBody, fromVisible := currentRoundVisibleText(visibleSnapshot, previousVisibleSnapshot, roundReply, lastInputText, anchorPolicy)
	if strings.TrimSpace(visibleBody) == "" {
		return "", false
	}
	return visibleBody, fromVisible
}

func NotifyContentNeedsConservativeDelay(visibleSnapshot string, previousVisibleSnapshot string, lastInputText string) bool {
	return notifyContentNeedsConservativeDelayWithWindow(visibleSnapshot, previousVisibleSnapshot, lastInputText, "")
}

func notifyContentNeedsConservativeDelayWithWindow(visibleSnapshot string, previousVisibleSnapshot string, lastInputText string, windowStartInputText string) bool {
	return notifyContentNeedsConservativeDelayWithWindowPolicy(visibleSnapshot, previousVisibleSnapshot, lastInputText, windowStartInputText, true)
}

func notifyContentNeedsConservativeDelayWithWindowPolicy(visibleSnapshot string, previousVisibleSnapshot string, lastInputText string, windowStartInputText string, allowPreviousTextAnchors bool) bool {
	return notifyContentNeedsConservativeDelayWithWindowAnchorPolicy(visibleSnapshot, previousVisibleSnapshot, nil, lastInputText, windowStartInputText, permissiveNotifyTextAnchorPolicy(allowPreviousTextAnchors))
}

func notifyContentNeedsConservativeDelayWithWindowAnchorPolicy(visibleSnapshot string, previousVisibleSnapshot string, roundReply []byte, lastInputText string, windowStartInputText string, anchorPolicy notifyTextAnchorPolicy) bool {
	lastInputText = strings.TrimSpace(lastInputText)
	windowStartInputText = strings.TrimSpace(windowStartInputText)
	var body string
	var fromVisible bool
	if windowStartInputText != "" {
		body, fromVisible = currentWindowVisibleText(visibleSnapshot, previousVisibleSnapshot, roundReply, windowStartInputText, anchorPolicy)
	} else {
		body, fromVisible = currentRoundVisibleText(visibleSnapshot, previousVisibleSnapshot, roundReply, lastInputText, anchorPolicy)
	}
	if !fromVisible || strings.TrimSpace(body) == "" {
		return true
	}
	if windowStartInputText != "" && lastInputText != "" && !containsInputEchoLine(trimVisibleText(body), lastInputText) {
		return true
	}
	if containsTransientStatusLine(body) {
		return true
	}
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if isPromptStatusLine(trimmed) {
			return true
		}
		if isCodexSuggestionLine(trimmed) && !isInputEchoLine(trimmed, lastInputText) {
			return true
		}
	}
	return false
}

func currentWindowVisibleText(visibleSnapshot string, previousVisibleSnapshot string, roundReply []byte, windowStartInputText string, anchorPolicy notifyTextAnchorPolicy) (string, bool) {
	visibleSnapshot = trimVisibleText(visibleSnapshot)
	if visibleSnapshot == "" {
		return "", false
	}
	// A submitted prompt is the only round boundary. Renderer epochs, guards,
	// cursor projections, visual diffs and previous-output tails must not veto or
	// replace an explicit `›/> + input` match.
	if body := visibleTextFromSubmittedInputStart(visibleSnapshot, windowStartInputText); strings.TrimSpace(body) != "" {
		return trimVisibleText(body), true
	}
	// Selection menus are interactive terminal state rather than an alternative
	// round anchor. They still need to render immediately for /model, /resume,
	// reasoning, and similar commands.
	if body := codexTerminalInteractionVisibleBlock(visibleSnapshot, previousVisibleSnapshot, windowStartInputText); body != "" {
		if anchorPolicy.allowed || codexTerminalInteractionChangedSinceBaseline(visibleSnapshot, previousVisibleSnapshot, windowStartInputText) {
			return body, true
		}
	}
	if containsSubmittedInputPrompt(visibleSnapshot) {
		return "", false
	}
	// Non-agent terminals do not have a Codex `›/>` composer. Preserve their
	// existing shell/diff behavior without using it to identify an agent round.
	if codexTerminalInteractionContextRejected(visibleSnapshot, previousVisibleSnapshot, windowStartInputText) {
		return "", false
	}
	if anchorPolicy.allowed {
		if !anchorPolicy.enforceIdentity {
			if body, ok := visibleTextChangedSincePrevious(visibleSnapshot, previousVisibleSnapshot); ok && strings.TrimSpace(body) != "" {
				return trimVisibleText(body), true
			}
		}
		if body, ok := visibleTextAfterPreviousTailAnchorWithPolicy(visibleSnapshot, previousVisibleSnapshot, windowStartInputText, roundReply, 5, anchorPolicy); ok && strings.TrimSpace(body) != "" {
			return trimVisibleText(body), true
		}
	}
	return "", false
}

func visibleTextFromSubmittedInputStart(visibleSnapshot string, inputText string) string {
	lines := strings.Split(visibleSnapshot, "\n")
	span, ok := newestSubmittedInputAnchorSpan(lines, inputText)
	if !ok {
		return ""
	}
	return trimVisibleText(strings.Join(lines[span.start:], "\n"))
}

func currentRoundVisibleText(visibleSnapshot string, previousVisibleSnapshot string, roundReply []byte, lastInputText string, anchorPolicy notifyTextAnchorPolicy) (string, bool) {
	visibleSnapshot = trimVisibleText(visibleSnapshot)
	if visibleSnapshot == "" {
		return "", false
	}
	if strings.TrimSpace(lastInputText) != "" {
		if body := visibleTextFromLastInputWithPolicy(visibleSnapshot, previousVisibleSnapshot, lastInputText, anchorPolicy); strings.TrimSpace(body) != "" {
			return trimVisibleText(body), true
		}
	}
	if body := codexTerminalInteractionVisibleBlock(visibleSnapshot, previousVisibleSnapshot, lastInputText); body != "" {
		if anchorPolicy.allowed || codexTerminalInteractionChangedSinceBaseline(visibleSnapshot, previousVisibleSnapshot, lastInputText) {
			return body, true
		}
	}
	if containsSubmittedInputPrompt(visibleSnapshot) {
		return "", false
	}
	if codexTerminalInteractionContextRejected(visibleSnapshot, previousVisibleSnapshot, lastInputText) {
		return "", false
	}
	if anchorPolicy.allowed {
		if !anchorPolicy.enforceIdentity {
			if body, ok := visibleTextChangedSincePrevious(visibleSnapshot, previousVisibleSnapshot); ok {
				return trimVisibleText(body), true
			}
		}
		if body, ok := visibleTextAfterPreviousTailAnchorWithPolicy(visibleSnapshot, previousVisibleSnapshot, lastInputText, roundReply, 5, anchorPolicy); ok {
			return trimVisibleText(body), true
		}
	}
	return "", false
}

func visibleTextChangedSincePrevious(visibleSnapshot string, previousVisibleSnapshot string) (string, bool) {
	visibleSnapshot = trimVisibleText(visibleSnapshot)
	previousVisibleSnapshot = trimVisibleText(previousVisibleSnapshot)
	if visibleSnapshot == "" || previousVisibleSnapshot == "" {
		return "", false
	}
	currentLines := splitVisibleLines(visibleSnapshot)
	previousLines := splitVisibleLines(previousVisibleSnapshot)
	if len(currentLines) == 0 || len(previousLines) == 0 {
		return "", false
	}
	if len(currentLines) < len(previousLines) {
		return "", false
	}
	for i := range previousLines {
		if currentLines[i] != previousLines[i] {
			return "", false
		}
	}
	if len(currentLines) == len(previousLines) {
		return "", true
	}
	return strings.Join(currentLines[len(previousLines):], "\n"), true
}

func isStrongVisibleBoundaryLine(line, normalized string) bool {
	if len([]rune(normalized)) < 4 {
		return false
	}
	trimmed := strings.TrimSpace(line)
	if isTransientStatusLine(trimmed) || isPromptStatusLine(trimmed) || isCodexSuggestionLine(trimmed) || isCodexInteractionStatusLine(trimmed) {
		return false
	}
	if text, ok := inputEchoText(trimmed); ok && strings.TrimSpace(text) == "" {
		return false
	}
	if text, ok := shellInputEchoText(trimmed); ok && strings.TrimSpace(text) == "" {
		return false
	}
	return true
}

type indexedVisibleAnchorLine struct {
	index int
	text  string
}

// The browser installs a scrollback marker before the oldest of its final 64
// logical lines. Keeping text-tail candidates inside the final 32 lines leaves
// a conservative guard band for footer redraws and wrapped rows. If the guard
// marker is evicted, the renderer epoch changes and the manager disables this
// entire text-anchor path.
const tailAnchorSearchLineLimit = 32

// visibleTextAfterPreviousTailAnchor is the second-line round boundary when
// the user's input echo cannot be found. It chooses one 2-5 line suffix using
// only the baseline, then requires that exact sequence to occur exactly once
// in the current snapshot. Candidate strength must never be weakened after
// looking at current text: doing so lets an older short duplicate impersonate
// a longer baseline tail whose distinguishing context disappeared.
func visibleTextAfterPreviousTailAnchor(visibleSnapshot string, previousVisibleSnapshot string, lastInputText string, maxAnchorLines int) (string, bool) {
	return visibleTextAfterPreviousTailAnchorWithPolicy(visibleSnapshot, previousVisibleSnapshot, lastInputText, nil, maxAnchorLines, permissiveNotifyTextAnchorPolicy(true))
}

func visibleTextAfterPreviousTailAnchorWithPolicy(visibleSnapshot string, previousVisibleSnapshot string, lastInputText string, roundReply []byte, maxAnchorLines int, anchorPolicy notifyTextAnchorPolicy) (string, bool) {
	if !anchorPolicy.allowed {
		return "", false
	}
	if maxAnchorLines < 2 {
		return "", false
	}
	previous := stableVisibleAnchorLines(previousVisibleSnapshot, lastInputText)
	previousTail := visibleAnchorLinesInTail(previous, len(splitVisibleLines(trimVisibleText(previousVisibleSnapshot))), tailAnchorSearchLineLimit)
	current := stableVisibleAnchorLines(visibleSnapshot, "")
	if len(previousTail) < 2 || len(current) < 2 {
		return "", false
	}
	if maxAnchorLines > len(previousTail) {
		maxAnchorLines = len(previousTail)
	}
	var candidate []string
	var candidateLines []indexedVisibleAnchorLine
	// Prefer the longest available suffix. Shortening only because the current
	// snapshot no longer contains its distinguishing prefix would turn an older
	// duplicate into a false boundary.
	for size := maxAnchorLines; size >= 2; size-- {
		candidateLines = previousTail[len(previousTail)-size:]
		currentCandidate := make([]string, size)
		for i := range candidateLines {
			currentCandidate[i] = candidateLines[i].text
		}
		if !tailAnchorSequenceTrusted(currentCandidate) || len(normalizedSequenceStarts(previous, currentCandidate)) != 1 {
			continue
		}
		candidate = currentCandidate
		break
	}
	if len(candidate) == 0 {
		return "", false
	}
	currentStarts := normalizedSequenceStarts(current, candidate)
	if len(currentStarts) != 1 {
		// Zero occurrences means the selected boundary disappeared. Multiple
		// occurrences are ambiguous between a redraw and the reply quoting the
		// old tail. Neither case is safe to guess from text alone.
		return "", false
	}
	visibleLines := splitVisibleLines(visibleSnapshot)
	selected := currentStarts[0]
	if anchorPolicy.enforceIdentity {
		for i := range candidateLines {
			if !anchorPolicy.sameGuardRelativeLine(candidateLines[i].index, current[selected+i].index) {
				return "", false
			}
		}
		if roundReplyReemitsTailAnchor(roundReply, candidate) {
			// The old boundary appearing in this round's PTY output means it may
			// have been redrawn or quoted rather than preserved in place.
			return "", false
		}
	}
	start := current[selected+len(candidate)-1].index + 1
	if start >= len(visibleLines) {
		return "", true
	}
	return dropLeadingTailInputEcho(strings.Join(visibleLines[start:], "\n"), lastInputText), true
}

func (policy notifyTextAnchorPolicy) sameGuardRelativeLine(previousLine int, currentLine int) bool {
	if !policy.allowed {
		return false
	}
	if !policy.enforceIdentity {
		return true
	}
	if policy.previousGuardLine < 0 || policy.currentGuardLine < 0 ||
		previousLine < policy.previousGuardLine || currentLine < policy.currentGuardLine {
		return false
	}
	return previousLine-policy.previousGuardLine == currentLine-policy.currentGuardLine
}

func roundReplyReemitsTailAnchor(roundReply []byte, sequence []string) bool {
	if len(roundReply) == 0 || len(sequence) < 2 {
		return false
	}
	anchor := canonicalAnchorText(strings.Join(sequence, "\n"))
	if anchor == "" {
		return false
	}
	output := canonicalAnchorText(stripTerminalControlsRaw(roundReply))
	return strings.Contains(output, anchor)
}

func stableVisibleAnchorLines(text string, activeInputText string) []indexedVisibleAnchorLine {
	lines := splitVisibleLines(trimVisibleText(text))
	skippedInputLines := make(map[int]struct{})
	if strings.TrimSpace(activeInputText) != "" {
		spans := inputAnchorSpans(lines, activeInputText)
		if len(spans) > 0 {
			span := spans[len(spans)-1]
			if inputAnchorAtActiveBaselineTail(lines, span) {
				for i := span.start; i <= span.end; i++ {
					skippedInputLines[i] = struct{}{}
				}
			}
		}
	}
	out := make([]indexedVisibleAnchorLine, 0, len(lines))
	for i, line := range lines {
		if _, skip := skippedInputLines[i]; skip {
			continue
		}
		normalized := normalizeVisibleAnchorLine(line)
		if normalized == "" || !isStrongVisibleBoundaryLine(line, normalized) || isPureHorizontalRuleLine(line) {
			continue
		}
		out = append(out, indexedVisibleAnchorLine{index: i, text: normalized})
	}
	return out
}

func visibleAnchorLinesInTail(lines []indexedVisibleAnchorLine, physicalLineCount int, limit int) []indexedVisibleAnchorLine {
	if limit <= 0 || physicalLineCount <= 0 || len(lines) == 0 {
		return nil
	}
	start := physicalLineCount - limit
	if start <= 0 {
		return lines
	}
	for i, line := range lines {
		if line.index >= start {
			return lines[i:]
		}
	}
	return nil
}

func dropLeadingTailInputEcho(text string, lastInputText string) string {
	if strings.TrimSpace(text) == "" || strings.TrimSpace(lastInputText) == "" {
		return text
	}
	lines := splitVisibleLines(text)
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || isTransientStatusLine(trimmed) || isPromptStatusLine(trimmed) ||
			isCodexSuggestionLine(trimmed) || isCodexInteractionStatusLine(trimmed) || isPureHorizontalRuleLine(trimmed) {
			continue
		}
		if end, ok := inputAnchorEndLine(lines, i, lastInputText); ok {
			return strings.Join(append(append([]string{}, lines[:i]...), lines[end+1:]...), "\n")
		}
		break
	}
	return text
}

func tailAnchorSequenceTrusted(sequence []string) bool {
	if len(sequence) < 2 {
		return false
	}
	totalRunes := 0
	for _, line := range sequence {
		totalRunes += len([]rune(line))
	}
	// Length alone is not entropy: "done / okay / ready" and repeated braces
	// are common terminal tails. Require both enough material and a useful
	// spread of non-punctuation characters before treating the suffix as an ID.
	minimumRunes := 20
	if len(sequence) == 2 {
		minimumRunes = 16
	}
	if totalRunes < minimumRunes {
		return false
	}
	distinct := make(map[rune]struct{})
	for _, line := range sequence {
		for _, r := range line {
			if unicode.IsLetter(r) || unicode.IsNumber(r) {
				distinct[unicode.ToLower(r)] = struct{}{}
			}
		}
	}
	return len(distinct) >= 8
}

func normalizedSequenceStarts(lines []indexedVisibleAnchorLine, sequence []string) []int {
	if len(sequence) == 0 || len(lines) < len(sequence) {
		return nil
	}
	starts := make([]int, 0, 2)
	for i := 0; i+len(sequence) <= len(lines); i++ {
		matched := true
		for j := range sequence {
			if lines[i+j].text != sequence[j] {
				matched = false
				break
			}
		}
		if matched {
			starts = append(starts, i)
		}
	}
	return starts
}

func splitVisibleLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return strings.Split(text, "\n")
}

func normalizeVisibleAnchorLine(line string) string {
	// Tail anchors identify rendered output, not user input. Preserve leading
	// whitespace and internal spacing exactly: collapsing indentation lets a
	// reflowed or differently formatted historical block impersonate the old
	// boundary. xterm snapshots already remove unused cell padding; trim only
	// horizontal padding at the physical line end for legacy test/DOM sources.
	return strings.TrimRight(line, " \t")
}

func visibleTextAfterRoundStart(visibleSnapshot string, snapshotAtRoundStart string) string {
	visibleSnapshot = trimVisibleText(visibleSnapshot)
	snapshotAtRoundStart = trimVisibleText(snapshotAtRoundStart)
	if visibleSnapshot == "" || snapshotAtRoundStart == "" {
		return ""
	}
	if visibleSnapshot == snapshotAtRoundStart {
		return ""
	}
	if strings.HasPrefix(visibleSnapshot, snapshotAtRoundStart) {
		return trimVisibleText(strings.TrimPrefix(visibleSnapshot, snapshotAtRoundStart))
	}
	return ""
}

func trimVisibleText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

func normalizeSnapshotText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return strings.TrimSpace(text)
}

func visibleTextFromLastInput(visibleSnapshot string, previousVisibleSnapshot string, lastInputText string) string {
	return visibleTextFromLastInputWithPolicy(visibleSnapshot, previousVisibleSnapshot, lastInputText, permissiveNotifyTextAnchorPolicy(true))
}

func visibleTextFromLastInputWithPolicy(visibleSnapshot string, previousVisibleSnapshot string, lastInputText string, anchorPolicy notifyTextAnchorPolicy) string {
	lines := strings.Split(visibleSnapshot, "\n")
	span, ok := newestInputAnchorSpanWithPolicy(lines, strings.Split(previousVisibleSnapshot, "\n"), lastInputText, anchorPolicy)
	if !ok || span.end+1 >= len(lines) {
		return ""
	}
	return trimAnchoredResponseBody(strings.Join(lines[span.end+1:], "\n"), leadingHorizontalWhitespace(lines[span.start]))
}

func visibleTextFromInputStart(visibleSnapshot string, previousVisibleSnapshot string, inputText string) string {
	return visibleTextFromInputStartWithPolicy(visibleSnapshot, previousVisibleSnapshot, inputText, permissiveNotifyTextAnchorPolicy(true))
}

func visibleTextFromInputStartWithPolicy(visibleSnapshot string, previousVisibleSnapshot string, inputText string, anchorPolicy notifyTextAnchorPolicy) string {
	lines := strings.Split(visibleSnapshot, "\n")
	span, ok := newestInputAnchorSpanWithPolicy(lines, strings.Split(previousVisibleSnapshot, "\n"), inputText, anchorPolicy)
	if !ok {
		return ""
	}
	return trimVisibleText(strings.Join(lines[span.start:], "\n"))
}

// visibleTextFromWindowStartWithPolicy locates the oldest still-open input
// when another input is submitted before the previous round has completed.
// In a strict renderer snapshot the active composer is at the baseline cursor,
// so the open-window anchor is the newest matching prompt before that cursor.
func visibleTextFromWindowStartWithPolicy(visibleSnapshot string, previousVisibleSnapshot string, inputText string, anchorPolicy notifyTextAnchorPolicy) string {
	if !anchorPolicy.enforceIdentity {
		currentLines := strings.Split(visibleSnapshot, "\n")
		current := inputAnchorSpans(currentLines, inputText)
		previous := inputAnchorSpans(strings.Split(previousVisibleSnapshot, "\n"), inputText)
		if len(previous) == 0 || len(current) < len(previous) {
			return ""
		}
		// The window start is intentionally no longer the active composer. Map
		// its newest baseline occurrence by ordinal instead of applying the
		// current-round cursor proof used for lastInputText.
		return trimVisibleText(strings.Join(currentLines[current[len(previous)-1].start:], "\n"))
	}
	currentLines := strings.Split(visibleSnapshot, "\n")
	previousLines := strings.Split(previousVisibleSnapshot, "\n")
	current := inputAnchorSpans(currentLines, inputText)
	previous := inputAnchorSpans(previousLines, inputText)
	if len(current) == 0 || len(previous) == 0 || anchorPolicy.previousCursorLine < 0 {
		return ""
	}
	baselineIndex := -1
	for i := len(previous) - 1; i >= 0; i-- {
		if previous[i].end < anchorPolicy.previousCursorLine {
			baselineIndex = i
			break
		}
	}
	if baselineIndex < 0 {
		return ""
	}
	baseline := previous[baselineIndex]
	matches := make([]inputAnchorSpan, 0, 1)
	for _, candidate := range current {
		if anchorPolicy.sameGuardRelativeLine(baseline.start, candidate.start) &&
			anchorPolicy.sameGuardRelativeLine(baseline.end, candidate.end) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) != 1 {
		return ""
	}
	return trimVisibleText(strings.Join(currentLines[matches[0].start:], "\n"))
}

type inputAnchorSpan struct {
	start int
	end   int
}

func inputAnchorSpans(lines []string, inputText string) []inputAnchorSpan {
	spans := make([]inputAnchorSpan, 0, 2)
	for i := 0; i < len(lines); i++ {
		end, ok := inputAnchorEndLine(lines, i, inputText)
		if !ok {
			continue
		}
		spans = append(spans, inputAnchorSpan{start: i, end: end})
		i = end
	}
	return spans
}

func newestInputAnchorSpan(currentLines, previousLines []string, inputText string) (inputAnchorSpan, bool) {
	return newestInputAnchorSpanWithPolicy(currentLines, previousLines, inputText, permissiveNotifyTextAnchorPolicy(true))
}

func newestInputAnchorSpanWithPolicy(currentLines, previousLines []string, inputText string, anchorPolicy notifyTextAnchorPolicy) (inputAnchorSpan, bool) {
	// Input prompt text is the sole boundary identity. The caller intentionally
	// ignores renderer/baseline metadata and always chooses the newest match.
	if span, ok := newestSubmittedInputAnchorSpan(currentLines, inputText); ok {
		return span, true
	}
	// Keep prompt matching for ordinary shells, which have no Codex composer.
	// This path is never allowed to replace a missing `›/>` agent anchor.
	if containsSubmittedInputPrompt(strings.Join(currentLines, "\n")) || !anchorPolicy.allowed {
		return inputAnchorSpan{}, false
	}
	current := inputAnchorSpans(currentLines, inputText)
	if len(current) == 0 {
		return inputAnchorSpan{}, false
	}
	previous := inputAnchorSpans(previousLines, inputText)
	if anchorPolicy.enforceIdentity {
		if len(previous) == 0 || anchorPolicy.previousCursorLine < 0 {
			return inputAnchorSpan{}, false
		}
		baseline := previous[len(previous)-1]
		if baseline.end != anchorPolicy.previousCursorLine {
			return inputAnchorSpan{}, false
		}
		matches := make([]inputAnchorSpan, 0, 1)
		for _, candidate := range current {
			if anchorPolicy.sameGuardRelativeLine(baseline.start, candidate.start) &&
				anchorPolicy.sameGuardRelativeLine(baseline.end, candidate.end) {
				matches = append(matches, candidate)
			}
		}
		if len(matches) != 1 {
			return inputAnchorSpan{}, false
		}
		return matches[0], true
	}
	if len(current) < len(previous) {
		return inputAnchorSpan{}, false
	}
	if len(current) == len(previous) {
		if len(previous) == 0 || !inputAnchorAtActiveBaselineTail(previousLines, previous[len(previous)-1]) {
			return inputAnchorSpan{}, false
		}
		return current[len(previous)-1], true
	}
	return current[len(previous)], true
}

// newestSubmittedInputAnchorSpan implements the notification boundary in
// three explicit passes, always scanning from the end of the terminal:
//  1. exact one-line `›/> + full input`;
//  2. exact full input after joining terminal soft-wrap continuation lines;
//  3. the first 30 runes of the input, on one line or soft-wrapped.
func newestSubmittedInputAnchorSpan(lines []string, inputText string) (inputAnchorSpan, bool) {
	inputText = strings.TrimSpace(inputText)
	if inputText == "" {
		return inputAnchorSpan{}, false
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if rendered, ok := submittedInputPromptText(lines[i]); ok && anchorTextsEqual(rendered, inputText) {
			return inputAnchorSpan{start: i, end: i}, true
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if end, ok := wrappedSubmittedInputEndAt(lines, i, inputText); ok {
			return inputAnchorSpan{start: i, end: end}, true
		}
	}
	prefix := inputAnchorText(inputText)
	if prefix == inputText {
		return inputAnchorSpan{}, false
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if rendered, ok := submittedInputPromptText(lines[i]); ok && anchorTextHasPrefix(rendered, prefix) {
			return inputAnchorSpan{start: i, end: i}, true
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if end, ok := wrappedSubmittedInputEndAt(lines, i, prefix); ok {
			return inputAnchorSpan{start: i, end: end}, true
		}
	}
	return inputAnchorSpan{}, false
}

func submittedInputPromptText(line string) (string, bool) {
	trimmed := strings.TrimSpace(stripAnchorIgnorables(StripTerminalControls([]byte(line))))
	for _, prompt := range []string{"›", "❯", "»", ">"} {
		if rest, ok := trimPromptPrefix(trimmed, prompt); ok {
			return rest, true
		}
	}
	return "", false
}

func containsSubmittedInputPrompt(text string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if _, ok := submittedInputPromptText(line); ok {
			return true
		}
	}
	return false
}

func wrappedSubmittedInputEndAt(lines []string, start int, inputText string) (int, bool) {
	first, ok := submittedInputPromptText(lines[start])
	if !ok {
		return start, false
	}
	states := anchorPrefixStates(inputText, first)
	if len(states) == 0 || anchorPrefixComplete(states) {
		return start, false
	}
	for i := start + 1; i < len(lines); i++ {
		fragment := strings.TrimSpace(lines[i])
		if fragment == "" {
			continue
		}
		states = extendAnchorPrefixStates(states, fragment)
		if len(states) == 0 {
			return start, false
		}
		if anchorPrefixComplete(states) {
			return i, true
		}
	}
	return start, false
}

func inputAnchorSpanEndsAtCursor(spans []inputAnchorSpan, cursorLine int) bool {
	for _, span := range spans {
		if span.end == cursorLine {
			return true
		}
	}
	return false
}

func latestCodexInputAnchorAfterGuard(lines []string, spans []inputAnchorSpan, guardLine int) (inputAnchorSpan, bool) {
	for i := len(spans) - 1; i >= 0; i-- {
		candidate := spans[i]
		if candidate.end > guardLine && isCodexInputAnchorSpan(lines, candidate) {
			return candidate, true
		}
	}
	return inputAnchorSpan{}, false
}

func latestCodexInputAnchorAfterGuardWithCursorProof(lines []string, spans []inputAnchorSpan, policy notifyTextAnchorPolicy) (inputAnchorSpan, bool) {
	if !policy.enforceIdentity || policy.currentCursorLine < 0 {
		return inputAnchorSpan{}, false
	}
	return latestCodexInputAnchorAfterGuard(lines, spans, policy.currentGuardLine)
}

func submittedCodexInputAnchorInCursorWindow(lines []string, spans []inputAnchorSpan, policy notifyTextAnchorPolicy) (inputAnchorSpan, bool) {
	if !policy.enforceIdentity || policy.previousGuardLine < 0 || policy.currentGuardLine < 0 ||
		policy.previousCursorLine < policy.previousGuardLine || policy.currentCursorLine < 0 {
		return inputAnchorSpan{}, false
	}
	projectedBaselineCursor := policy.currentGuardLine + (policy.previousCursorLine - policy.previousGuardLine)
	if projectedBaselineCursor > policy.currentCursorLine {
		return inputAnchorSpan{}, false
	}
	for i := len(spans) - 1; i >= 0; i-- {
		candidate := spans[i]
		if candidate.end < projectedBaselineCursor || candidate.end > policy.currentCursorLine {
			continue
		}
		if isCodexInputAnchorSpan(lines, candidate) {
			return candidate, true
		}
	}
	return inputAnchorSpan{}, false
}

func isCodexInputAnchorSpan(lines []string, span inputAnchorSpan) bool {
	if span.start < 0 || span.start >= len(lines) || span.end < span.start || span.end >= len(lines) {
		return false
	}
	_, ok := agentInputPromptText(lines[span.start])
	return ok
}

func baselineInputAnchorSpansWithCursorProof(spans []inputAnchorSpan, cursorLine int) []inputAnchorSpan {
	if len(spans) == 0 || cursorLine < 0 {
		return nil
	}
	newest := spans[len(spans)-1]
	if newest.end != cursorLine {
		// The baseline is captured immediately before Enter. Regardless of the
		// visible prompt glyph (`›`, `$`, `>`, ...), only the newest occurrence
		// ending on the renderer cursor line can be the active composer. Do not
		// downgrade to an older equal-text occurrence when this proof fails.
		return nil
	}
	return spans
}

func inputAnchorAtActiveBaselineTail(lines []string, span inputAnchorSpan) bool {
	if span.end < 0 || span.end >= len(lines) {
		return false
	}
	for _, line := range lines[span.end+1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || isTransientStatusLine(trimmed) || isPromptStatusLine(trimmed) ||
			isCodexSuggestionLine(trimmed) || isCodexInteractionStatusLine(trimmed) || isPureHorizontalRuleLine(trimmed) {
			continue
		}
		return false
	}
	return true
}

func inputAnchorEndLine(lines []string, i int, lastInputText string) (int, bool) {
	if strings.TrimSpace(lastInputText) == "" {
		return i, false
	}
	if isStructuredInputAnchorLine(lines[i], lastInputText) {
		return i, true
	}
	if isInputEchoLine(lines[i], lastInputText) {
		return i, true
	}
	if end, ok := wrappedInputEchoEndAt(lines, i, lastInputText); ok {
		return end, true
	}
	// The browser normally restores xterm soft wraps before the snapshot is
	// sent, and wrappedInputEchoEndAt handles DOM snapshots line by line. If a
	// renderer redraw still changes or drops the tail of a long composer, use a
	// bounded prefix as the identity anchor instead of requiring the entire
	// submitted input to survive byte-for-byte.
	anchorText := inputAnchorText(lastInputText)
	if anchorText == strings.TrimSpace(lastInputText) {
		return i, false
	}
	if renderedText, ok := inputEchoText(lines[i]); ok && anchorTextHasPrefix(renderedText, anchorText) {
		return i, true
	}
	if end, ok := wrappedInputEchoEndAt(lines, i, anchorText); ok {
		return end, true
	}
	return i, false
}

func inputAnchorText(text string) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) <= maxInputAnchorRunes {
		return text
	}
	return string(runes[:maxInputAnchorRunes])
}

func visibleTextAfterLastInputPrompt(visibleSnapshot string, previousVisibleSnapshot string, lastInputText string) string {
	lines := strings.Split(visibleSnapshot, "\n")
	starts := inputPromptReplyStarts(lines, lastInputText)
	if len(starts) == 0 {
		return ""
	}
	if strings.TrimSpace(previousVisibleSnapshot) != "" {
		previousStarts := inputPromptReplyStarts(strings.Split(previousVisibleSnapshot, "\n"), lastInputText)
		if len(starts) <= len(previousStarts) {
			return ""
		}
	} else {
		return ""
	}
	selected := starts[len(starts)-1]
	return trimAnchoredResponseBody(strings.Join(lines[selected.responseLine:], "\n"), selected.promptIndent)
}

func trimAnchoredResponseBody(text string, promptIndent string) string {
	text = trimVisibleText(text)
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	first := lines[0]
	trimmedFirst := strings.TrimLeft(first, " \t")
	indent := strings.TrimSuffix(first, trimmedFirst)
	if promptIndent == "" || indent == "" || !strings.HasPrefix(indent, promptIndent) || !startsCodexResponseBlock(trimmedFirst) {
		return text
	}
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, promptIndent) {
			lines[i] = strings.TrimPrefix(line, promptIndent)
		}
	}
	return strings.Join(lines, "\n")
}

func leadingHorizontalWhitespace(line string) string {
	return strings.TrimSuffix(line, strings.TrimLeft(line, " \t"))
}

type inputPromptReplyStart struct {
	responseLine int
	promptIndent string
}

func inputPromptReplyStarts(lines []string, lastInputText string) []inputPromptReplyStart {
	starts := make([]inputPromptReplyStart, 0, 2)
	for i := 0; i < len(lines); i++ {
		text, ok := agentInputPromptText(lines[i])
		if !ok || strings.TrimSpace(text) == "" || isCodexSuggestionLine(strings.TrimSpace(lines[i])) {
			continue
		}
		promptText := text
		for j := i + 1; j < len(lines); j++ {
			candidate := strings.TrimSpace(lines[j])
			if candidate == "" {
				continue
			}
			if _, nextPrompt := agentInputPromptText(candidate); nextPrompt {
				break
			}
			if startsCodexResponseBlock(candidate) {
				if inputAnchorTextsLikelySame(promptText, lastInputText) {
					starts = append(starts, inputPromptReplyStart{responseLine: j, promptIndent: leadingHorizontalWhitespace(lines[i])})
				}
				break
			}
			promptText += candidate
		}
	}
	return starts
}

func agentInputPromptText(line string) (string, bool) {
	line = strings.TrimSpace(stripAnchorIgnorables(line))
	for _, prompt := range []string{"›", "❯", "»"} {
		if text, ok := trimPromptPrefix(line, prompt); ok {
			return text, true
		}
	}
	return "", false
}

func anchorTextsLikelySame(left string, right string) bool {
	if anchorTextsEqual(left, right) {
		return true
	}
	rightFirstField := ""
	if fields := strings.Fields(strings.TrimSpace(right)); len(fields) > 0 {
		rightFirstField = fields[0]
	}
	if strings.HasPrefix(rightFirstField, "@") && looksLikeRichTextFileMention(strings.TrimPrefix(rightFirstField, "@")) {
		leftBody := anchorTextAfterFirstField(left)
		rightBody := anchorTextAfterFirstField(right)
		if len([]rune(canonicalAnchorText(rightBody))) >= 8 && anchorTextsEqual(leftBody, rightBody) {
			return true
		}
	}
	// Do not use generic edit-distance/prefix similarity here. Two adjacent
	// user prompts commonly differ by only one word, and accepting an old 95%
	// match is precisely how a previous reply can be mistaken for this round.
	return false
}

func inputAnchorTextsLikelySame(renderedText string, inputText string) bool {
	if anchorTextsLikelySame(renderedText, inputText) {
		return true
	}
	anchorText := inputAnchorText(inputText)
	return anchorText != strings.TrimSpace(inputText) && anchorTextHasPrefix(renderedText, anchorText)
}

func anchorTextHasPrefix(text string, prefix string) bool {
	for _, textVariant := range anchorTextVariants(text) {
		for _, prefixVariant := range anchorTextVariants(prefix) {
			if strings.HasPrefix(textVariant, prefixVariant) {
				return true
			}
		}
	}
	return false
}

func anchorTextAfterFirstField(text string) string {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) < 2 {
		return ""
	}
	return strings.Join(fields[1:], " ")
}

func startsCodexResponseBlock(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(line)
	switch r {
	case '•', '⏺', '■', '⚠', '✦', '●', '◆', '◇', '▪', '▸':
		return true
	default:
		return false
	}
}

func isWrappedInputEchoAt(lines []string, i int, lastInputText string) bool {
	_, ok := wrappedInputEchoEndAt(lines, i, lastInputText)
	return ok
}

func wrappedInputEchoEndAt(lines []string, i int, lastInputText string) (int, bool) {
	text, ok := inputEchoText(lines[i])
	if !ok {
		return i, false
	}
	states := anchorPrefixStates(lastInputText, text)
	if len(states) == 0 {
		return i, false
	}
	if anchorPrefixComplete(states) {
		// A one-line echo must pass the whitespace-boundary-sensitive exact
		// matcher above. The compact prefix matcher exists only for wrapped
		// input where display reflow can remove boundary whitespace.
		return i, false
	}
	for j := i + 1; j < len(lines); j++ {
		trimmed := strings.TrimSpace(lines[j])
		if trimmed == "" {
			continue
		}
		states = extendAnchorPrefixStates(states, trimmed)
		if len(states) == 0 {
			return i, false
		}
		if anchorPrefixComplete(states) {
			return j, true
		}
	}
	return i, false
}

func compactAnchorText(text string) string {
	return canonicalAnchorText(text)
}

func canonicalAnchorText(text string) string {
	text = norm.NFKC.String(stripTerminalControlsRaw([]byte(text)))
	var b strings.Builder
	for _, r := range text {
		if unicode.IsSpace(r) || unicode.IsControl(r) || isAnchorIgnorableRune(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isAnchorIgnorableRune(r rune) bool {
	if unicode.In(r, unicode.Cf) {
		return true
	}
	return r == '\ufe0e' || r == '\ufe0f'
}

func anchorTextVariants(text string) []string {
	normalized := norm.NFKC.String(stripTerminalControlsRaw([]byte(text)))
	normalized = stripAnchorIgnorables(normalized)
	firstField := ""
	if fields := strings.Fields(normalized); len(fields) > 0 {
		firstField = fields[0]
	}
	base := canonicalAnchorText(text)
	if base == "" {
		return nil
	}
	variants := []string{base}
	if strings.HasPrefix(firstField, "@") && looksLikeRichTextFileMention(strings.TrimPrefix(firstField, "@")) {
		variants = append(variants, strings.TrimPrefix(base, "@"))
	}
	return variants
}

func anchorBoundaryVariants(text string) []string {
	text = norm.NFKC.String(stripTerminalControlsRaw([]byte(text)))
	text = stripAnchorIgnorables(text)
	base := strings.Join(strings.Fields(text), " ")
	if base == "" {
		return nil
	}
	variants := []string{base}
	firstField := ""
	if fields := strings.Fields(base); len(fields) > 0 {
		firstField = fields[0]
	}
	if strings.HasPrefix(firstField, "@") && looksLikeRichTextFileMention(strings.TrimPrefix(firstField, "@")) {
		variants = append(variants, strings.TrimPrefix(base, "@"))
	}
	return variants
}

func looksLikeRichTextFileMention(text string) bool {
	return strings.ContainsAny(text, `/\\`)
}

type anchorPrefixState struct {
	target  string
	current string
}

func anchorPrefixStates(targetText string, currentText string) []anchorPrefixState {
	states := make([]anchorPrefixState, 0, 4)
	for _, target := range anchorTextVariants(targetText) {
		for _, current := range anchorTextVariants(currentText) {
			if current != "" && strings.HasPrefix(target, current) {
				states = append(states, anchorPrefixState{target: target, current: current})
			}
		}
	}
	return states
}

func extendAnchorPrefixStates(states []anchorPrefixState, fragment string) []anchorPrefixState {
	fragment = canonicalAnchorText(fragment)
	if fragment == "" {
		return states
	}
	out := states[:0]
	for _, state := range states {
		state.current += fragment
		if strings.HasPrefix(state.target, state.current) {
			out = append(out, state)
		}
	}
	return out
}

func anchorPrefixComplete(states []anchorPrefixState) bool {
	for _, state := range states {
		if state.current == state.target {
			return true
		}
	}
	return false
}

func anchorTextsEqual(left string, right string) bool {
	for _, leftVariant := range anchorBoundaryVariants(left) {
		for _, rightVariant := range anchorBoundaryVariants(right) {
			if leftVariant == rightVariant {
				return true
			}
		}
	}
	return false
}

func visibleTextFromLastShellInput(visibleSnapshot string) string {
	lines := strings.Split(visibleSnapshot, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		text, ok := shellInputEchoText(lines[i])
		if !ok || strings.TrimSpace(text) == "" {
			continue
		}
		return strings.TrimSpace(strings.Join(lines[i:], "\n"))
	}
	return ""
}

func startsWithInputEcho(text string, input string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		return isInputEchoLine(line, input)
	}
	return false
}

func inputEchoText(line string) (string, bool) {
	trimmed := strings.TrimSpace(stripAnchorIgnorables(StripTerminalControls([]byte(line))))
	for _, prompt := range []string{"›", "❯", "»", ">"} {
		if rest, ok := trimPromptPrefix(trimmed, prompt); ok {
			return rest, true
		}
	}
	if rest, ok := trimPromptPrefix(trimmed, "⏺"); ok {
		return unwrapAgentActionName(rest), true
	}
	for _, prompt := range []string{"%", "$", "#", ">"} {
		if rest, ok := trimPromptPrefix(trimmed, prompt); ok {
			return rest, true
		}
		marker := " " + prompt + " "
		if idx := strings.LastIndex(trimmed, marker); idx >= 0 {
			return strings.TrimSpace(trimmed[idx+len(marker):]), true
		}
	}
	return "", false
}

func unwrapAgentActionName(text string) string {
	text = strings.TrimSpace(text)
	if idx := strings.Index(text, "("); idx > 0 && strings.HasSuffix(text, ")") {
		inner := strings.TrimSpace(text[idx+1 : len(text)-1])
		if inner != "" {
			return inner
		}
	}
	return text
}

func isStructuredInputAnchorLine(line string, input string) bool {
	trimmedLeft := strings.TrimLeft(line, " \t")
	raw, ok := trimPromptPrefixRaw(trimmedLeft, ">")
	if !ok {
		return false
	}
	return anchorTextsEqual(raw, input)
}

func inputEchoTextRaw(line string) (string, bool) {
	trimmedLeft := strings.TrimLeft(line, " \t")
	if rest, ok := trimPromptPrefixRaw(trimmedLeft, "›"); ok {
		return rest, true
	}
	if rest, ok := trimPromptPrefixRaw(trimmedLeft, ">"); ok {
		return rest, true
	}
	if rest, ok := trimPromptPrefixRaw(trimmedLeft, "⏺"); ok {
		return unwrapAgentActionName(rest), true
	}
	for _, prompt := range []string{"%", "$", "#", ">"} {
		if rest, ok := trimPromptPrefixRaw(trimmedLeft, prompt); ok {
			return rest, true
		}
		marker := " " + prompt + " "
		if idx := strings.LastIndex(trimmedLeft, marker); idx >= 0 {
			return strings.TrimRight(trimmedLeft[idx+len(marker):], "\r\n"), true
		}
	}
	return "", false
}

func shellInputEchoText(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	for _, prompt := range []string{"%", "$", "#"} {
		if rest, ok := trimPromptPrefix(trimmed, prompt); ok {
			return rest, true
		}
		if strings.HasSuffix(trimmed, " "+prompt) {
			return "", true
		}
		marker := " " + prompt + " "
		if idx := strings.LastIndex(trimmed, marker); idx >= 0 {
			return strings.TrimSpace(trimmed[idx+len(marker):]), true
		}
	}
	if strings.HasSuffix(trimmed, " >") {
		return "", true
	}
	marker := " > "
	if idx := strings.LastIndex(trimmed, marker); idx > 0 {
		return strings.TrimSpace(trimmed[idx+len(marker):]), true
	}
	return "", false
}

func trimPromptPrefix(line string, prompt string) (string, bool) {
	if line == prompt {
		return "", true
	}
	if !strings.HasPrefix(line, prompt) {
		return "", false
	}
	rest := strings.TrimPrefix(line, prompt)
	if prompt == "›" || prompt == "❯" || prompt == "»" {
		return strings.TrimSpace(rest), true
	}
	first, _ := utf8.DecodeRuneInString(rest)
	if unicode.IsSpace(first) {
		return strings.TrimSpace(rest), true
	}
	return "", false
}

func stripAnchorIgnorables(text string) string {
	return strings.Map(func(r rune) rune {
		if isAnchorIgnorableRune(r) {
			return -1
		}
		if r == '\u00a0' {
			return ' '
		}
		return r
	}, text)
}

func trimPromptPrefixRaw(line string, prompt string) (string, bool) {
	if line == prompt {
		return "", true
	}
	prefix := prompt + " "
	if strings.HasPrefix(line, prefix) {
		return strings.TrimRight(strings.TrimPrefix(line, prefix), "\r\n"), true
	}
	if prompt == "›" && strings.HasPrefix(line, prompt) {
		return strings.TrimRight(strings.TrimPrefix(line, prompt), "\r\n"), true
	}
	return "", false
}

func cleanupLarkNotifyText(text string, lastInputText string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if isTransientStatusLine(trimmed) {
			continue
		}
		if isPromptStatusLine(trimmed) {
			continue
		}
		if isPureHorizontalRuleLine(trimmed) {
			continue
		}
		if isCodexSuggestionLine(trimmed) && !isInputEchoLine(trimmed, lastInputText) {
			continue
		}
		out = append(out, strings.TrimRight(line, " \t"))
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func dropCodexPromptStatusLines(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	kept := lines[:0]
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if isPromptStatusLine(trimmed) || isTransientStatusLine(trimmed) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// dropCodexFooterStatusLines removes only the persistent Codex footer (model,
// effort, mode, and cwd). Unlike dropCodexPromptStatusLines it deliberately
// keeps transient progress such as "Working (...)" for running-card refreshes.
func dropCodexFooterStatusLines(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	kept := lines[:0]
	for _, line := range lines {
		if isPromptStatusLine(strings.TrimSpace(line)) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func restoreWrappedInputEcho(text string, lastInputText string) string {
	input := strings.TrimSpace(lastInputText)
	if strings.TrimSpace(text) == "" || input == "" {
		return text
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		if restored, end, ok := restoredWrappedInputEchoAt(lines, i, input); ok {
			out = append(out, restored)
			i = end
			continue
		}
		out = append(out, lines[i])
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func restoredWrappedInputEchoAt(lines []string, i int, input string) (string, int, bool) {
	text, ok := inputEchoText(lines[i])
	if !ok {
		return "", i, false
	}
	target := compactAnchorText(input)
	current := compactAnchorText(text)
	if target == "" || current == "" || current == target || !strings.HasPrefix(target, current) {
		return "", i, false
	}
	for j := i + 1; j < len(lines) && j <= i+32; j++ {
		trimmed := strings.TrimSpace(lines[j])
		if trimmed == "" {
			continue
		}
		if _, ok := inputEchoText(trimmed); ok {
			return "", i, false
		}
		if strings.HasPrefix(trimmed, "• ") || isPromptStatusLine(trimmed) || isCodexSuggestionLine(trimmed) {
			return "", i, false
		}
		current += compactAnchorText(trimmed)
		if current == target || strings.HasPrefix(current, target) {
			return inputEchoPrefix(lines[i]) + input, j, true
		}
		if !strings.HasPrefix(target, current) {
			return "", i, false
		}
	}
	return "", i, false
}

func inputEchoPrefix(line string) string {
	trimmed := strings.TrimSpace(line)
	for _, prompt := range []string{"›", ">", "%", "$", "#"} {
		if trimmed == prompt {
			return prompt + " "
		}
		if strings.HasPrefix(trimmed, prompt+" ") {
			return prompt + " "
		}
		marker := " " + prompt + " "
		if idx := strings.LastIndex(trimmed, marker); idx >= 0 {
			return strings.TrimSpace(trimmed[:idx+len(marker)])
		}
	}
	return ""
}

func isPureHorizontalRuleLine(line string) bool {
	line = strings.TrimSpace(line)
	if len([]rune(line)) < 3 {
		return false
	}
	for _, r := range line {
		if unicode.IsSpace(r) {
			continue
		}
		switch r {
		case '-', '_', '=', '─', '━', '—', '―', '═', '╌', '╍', '┄', '┅', '┈', '┉':
			continue
		default:
			return false
		}
	}
	return true
}

func containsTransientStatusLine(text string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if isTransientStatusLine(strings.TrimSpace(line)) {
			return true
		}
	}
	return false
}

func isTransientStatusLine(line string) bool {
	lower := strings.ToLower(line)
	return strings.Contains(lower, "working (") ||
		strings.HasPrefix(lower, "worked for ") ||
		strings.HasPrefix(lower, "context left ") ||
		strings.HasPrefix(lower, "tokens used ") ||
		strings.Contains(lower, "esc to interrupt") ||
		(strings.Contains(lower, "background terminal") && strings.Contains(lower, "running")) ||
		strings.Contains(lower, "/ps to view") ||
		strings.Contains(lower, "/stop to close") ||
		strings.Contains(lower, "falling back from websockets") ||
		strings.Contains(lower, "stream disconnected before completion")
}

func hasReplyLine(text string, lastInputText string) bool {
	input := strings.TrimSpace(lastInputText)
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	start := 0
	requireAfterInput := false
	if input != "" {
		for i := len(lines) - 1; i >= 0; i-- {
			if end, ok := inputAnchorEndLine(lines, i, input); ok {
				start = end + 1
				requireAfterInput = true
				break
			}
		}
	}
	sawInput := requireAfterInput
	for _, line := range lines[start:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if input != "" && isInputEchoLine(trimmed, input) {
			sawInput = true
			continue
		}
		if _, ok := inputEchoText(trimmed); ok {
			continue
		}
		if shellText, ok := shellInputEchoText(trimmed); ok {
			if input != "" && strings.TrimSpace(shellText) == input {
				sawInput = true
				continue
			}
			if sawInput && strings.TrimSpace(shellText) == "" {
				return true
			}
			continue
		}
		if isTransientStatusLine(trimmed) || isPromptStatusLine(trimmed) || isCodexSuggestionLine(trimmed) {
			continue
		}
		if requireAfterInput && !sawInput {
			continue
		}
		return true
	}
	return input == ""
}

func containsInputEchoLine(text string, input string) bool {
	input = strings.TrimSpace(input)
	if input == "" {
		return false
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if _, ok := inputAnchorEndLine(lines, i, input); ok {
			return true
		}
	}
	return false
}

func isInputEchoLine(line string, input string) bool {
	text, ok := inputEchoText(line)
	return ok && anchorTextsEqual(text, input)
}

func isPromptStatusLine(line string) bool {
	lower := strings.ToLower(line)
	return strings.HasPrefix(lower, "gpt-") && strings.Contains(line, "~")
}

func isCodexSuggestionLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if text, ok := inputEchoText(trimmed); ok {
		trimmed = text
	}
	lower := strings.ToLower(trimmed)
	switch {
	case strings.HasPrefix(lower, "implement {feature}"):
		return true
	case strings.HasPrefix(lower, "find and fix a bug in @filename"):
		return true
	case strings.HasPrefix(lower, "improve documentation in @filename"):
		return true
	case strings.HasPrefix(lower, "run /review on my current changes"):
		return true
	case strings.HasPrefix(lower, "use /skills to list available skills"):
		return true
	default:
		return false
	}
}

func compactRepeatedLines(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	var prev string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed != prev {
			out = append(out, line)
		}
		prev = trimmed
	}
	return strings.Join(out, "\n")
}

func skipEscape(data []byte, i int) int {
	if i+1 >= len(data) {
		return i
	}
	next := data[i+1]
	if next == '[' {
		j := i + 2
		for j < len(data) {
			c := data[j]
			if c >= 0x40 && c <= 0x7e {
				return j
			}
			j++
		}
		return len(data) - 1
	}
	if next == ']' {
		j := i + 2
		for j < len(data) {
			if data[j] == 0x07 {
				return j
			}
			if data[j] == 0x1b && j+1 < len(data) && data[j+1] == '\\' {
				return j + 1
			}
			j++
		}
		return len(data) - 1
	}
	return i + 1
}
