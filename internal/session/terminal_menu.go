package session

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const TerminalInteractionMenu = "terminal_menu"

var menuFocusRE = regexp.MustCompile(`^(\s*)[❯›>▶]\s+(.+)$`)
var menuNumberRE = regexp.MustCompile(`^\s*(\d{1,3})[.)]\s+(.+)$`)
var menuContextUsageRE = regexp.MustCompile(`^\d+(?:\.\d+)?[kKmM]?/\d+(?:\.\d+)?[kKmM]?(?:\s|$)`)

// DetectTerminalMenu recognizes an active selector, not an arbitrary numbered
// answer: navigation instructions, one focus marker and an active tail are required.
func DetectTerminalMenu(text, sessionID string, version, snapshotVersion int64) *TerminalInteraction {
	lines := splitVisibleLines(trimVisibleText(text))
	// Ink borders are outside the menu's indentation. Strip them, including
	// padded empty rows, before locating the focus marker and navigation footer.
	for i, line := range lines {
		left := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(left, "│") || strings.HasPrefix(left, "┃") {
			line = strings.TrimPrefix(strings.TrimPrefix(left, "│"), "┃")
		}
		lines[i] = strings.TrimRight(line, " \t│┃")
	}
	footer := -1
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.ToLower(lines[i])
		navigation := (strings.Contains(line, "↑") && strings.Contains(line, "↓")) || strings.Contains(line, "arrow keys") || strings.Contains(line, "上下")
		confirm := strings.Contains(line, "enter") || strings.Contains(line, "回车")
		confirmFooter := strings.Contains(line, "esc") && (strings.Contains(line, "confirm") || strings.Contains(line, "select") || strings.Contains(line, "continue") || strings.Contains(line, "确认"))
		if confirm && (navigation || confirmFooter) {
			footer = i
			break
		}
	}
	if footer < 0 {
		return nil
	}
	aidenStatus := false
	for _, line := range lines[footer+1:] {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "recent context usage:") {
			aidenStatus = true
			continue
		}
		if aidenStatus && menuContextUsageRE.MatchString(line) {
			aidenStatus = false
			continue
		}
		if line != "" && !isPureHorizontalRuleLine(line) && !strings.HasPrefix(line, "╰") && !strings.HasPrefix(line, "└") && !isCodexInteractionStatusLine(line) {
			return nil
		}
	}
	focus, indent := -1, 0
	for i := footer - 1; i >= 0; i-- {
		if match := menuFocusRE.FindStringSubmatch(strings.Trim(lines[i], "│┃")); match != nil {
			focus, indent = i, len(match[1])
			break
		}
	}
	if focus < 0 {
		return nil
	}
	rowText := func(line string) (string, bool) {
		line = strings.Trim(line, "│┃")
		if match := menuFocusRE.FindStringSubmatch(line); match != nil {
			return match[2], true
		}
		if len(line) >= indent+2 && strings.TrimSpace(line[:indent+2]) == "" {
			return strings.TrimSpace(line), false
		}
		return "", false
	}
	focusedText, _ := rowText(lines[focus])
	numbered := menuNumberRE.MatchString(focusedText)
	row := func(i int) (string, bool) {
		label, selected := rowText(lines[i])
		if numbered {
			// Numbered rows may start at column zero even when the focus marker doesn't.
			if !selected {
				label = strings.TrimSpace(strings.Trim(lines[i], "│┃"))
			}
			match := menuNumberRE.FindStringSubmatch(label)
			if match == nil {
				return "", false
			}
			label = match[2]
		}
		if label == "" || isPureHorizontalRuleLine(label) || strings.HasPrefix(label, "╰") || strings.HasPrefix(label, "└") {
			return "", false
		}
		return label, selected
	}
	start, end := focus, focus+1
	for start > 0 {
		if label, _ := row(start - 1); label == "" {
			break
		}
		start--
	}
	for end < footer {
		if label, _ := row(end); label == "" {
			break
		}
		end++
	}
	titleIndex := start - 1
	if end-start > maxTerminalInteractionOptions {
		// ponytail: expose a 20-row window; arrow actions reach the rest without
		// maintaining a second model catalog or inventing off-screen options.
		start = max(start, focus-maxTerminalInteractionOptions/2)
		end = min(end, start+maxTerminalInteractionOptions)
	}
	for titleIndex >= 0 && (strings.TrimSpace(lines[titleIndex]) == "" || isPureHorizontalRuleLine(strings.TrimSpace(lines[titleIndex]))) {
		titleIndex--
	}
	if titleIndex < 0 {
		return nil
	}
	for i := titleIndex; i >= max(0, titleIndex-4); i-- {
		line := strings.ToLower(strings.TrimSpace(lines[i]))
		if strings.HasPrefix(line, "select ") || strings.HasPrefix(line, "choose ") || strings.Contains(line, "选择") {
			titleIndex = i
			break
		}
	}
	title := strings.TrimSpace(strings.Trim(lines[titleIndex], "│┃"))
	if strings.Contains(title, "```") {
		return nil
	}
	footerText := strings.ToLower(lines[footer])
	toggleSpace := strings.Contains(footerText, "space ") || strings.Contains(footerText, "space:") || strings.Contains(footerText, "空格")
	options := make([]TerminalInteractionOption, 0, end-start+4)
	markers := 0
	for i := start; i < end; i++ {
		label, focused := row(i)
		if focused {
			markers++
		}
		movement := strings.Repeat("\x1b[B", max(0, i-focus)) + strings.Repeat("\x1b[A", max(0, focus-i))
		if toggleSpace {
			movement += " "
		}
		options = append(options, TerminalInteractionOption{ID: fmt.Sprintf("opt_%d", i-start+1), Label: sanitizeForLarkAudit(label), Input: movement, SubmitWithEnter: !toggleSpace})
	}
	if markers != 1 || len(options) == 0 {
		return nil
	}
	if toggleSpace {
		options = append(options, TerminalInteractionOption{ID: "confirm", Label: "确认选择", SubmitWithEnter: true})
	}
	// Navigation also makes clipped/paged menus reachable without inventing hidden options.
	options = append(options, TerminalInteractionOption{ID: "up", Label: "↑ 向上浏览", Input: "\x1b[A"}, TerminalInteractionOption{ID: "down", Label: "↓ 向下浏览", Input: "\x1b[B"})
	if strings.Contains(footerText, "←") {
		options = append(options, TerminalInteractionOption{ID: "back", Label: "返回上一级", Input: "\x1b[D"})
	}
	if strings.Contains(footerText, "esc") {
		options = append(options, TerminalInteractionOption{ID: "cancel", Label: "返回 / 取消", Input: "\x1b"})
	}
	fingerprint := terminalInteractionFingerprint(TerminalInteractionMenu, title, options)
	return &TerminalInteraction{ID: "ti_" + notifyContentHash(fmt.Sprintf("%s:%d:%s", sessionID, version, fingerprint))[:20], Kind: TerminalInteractionMenu, Title: sanitizeForLarkAudit(title), Fingerprint: fingerprint, NotifyVersion: version, SnapshotVersion: snapshotVersion, Options: options}
}

func (rt *RuntimeSession) activeTerminalMenuLocked() *TerminalInteraction {
	metadata := parseSnapshotSourceContinuity(rt.visibleSnapshotSource)
	if !metadata.valid || !isBufferSnapshotContinuityBase(metadata.base) {
		return nil
	}
	if rt.visibleSnapshotStaleForCurrentRoundLocked() {
		return nil
	}
	return DetectTerminalMenu(rt.visibleSnapshot, rt.session.ID, rt.notifyVersion, rt.visibleSnapshotVersion)
}

// Only request a rendered snapshot here. Raw PTY output never authorizes a selection.
func (rt *RuntimeSession) scheduleTerminalMenuProbeLocked(chunk []byte) {
	if rt.manager == nil || rt.session.LastMode != SessionModeAgent {
		return
	}
	text := strings.ToLower(rt.terminalMenuProbeTail + string(chunk))
	rt.terminalMenuProbeTail = text[max(0, len(text)-128):]
	if !rt.terminalMenuActive && !strings.Contains(text, "select") && !strings.Contains(text, "enter") && !strings.Contains(text, "选择") && !strings.Contains(text, "↑") {
		return
	}
	if rt.terminalMenuProbe != nil {
		rt.terminalMenuProbe.Stop()
	}
	rt.terminalMenuProbe = time.AfterFunc(300*time.Millisecond, func() {
		rt.mu.Lock()
		live := rt.session.Live && !rt.closed
		rt.mu.Unlock()
		if live {
			rt.RequestFreshSnapshot(defaultNotifySnapshotTimeout)
		}
	})
}

func (rt *RuntimeSession) submitTerminalMenuSelection(option TerminalInteractionOption) error {
	// Do not use normal prompt submission: Enter in a menu is not a task and
	// must not reset startup, the source message, or completion-hook state.
	rt.mu.Lock()
	if rt.closed || rt.terminal == nil {
		rt.mu.Unlock()
		return errTerminalInteractionExpired
	}
	terminal := rt.terminal
	rt.notifyVersion++
	rt.session.Status = StatusRunning
	rt.terminalMenuActive = true
	rt.terminalMenuSelecting = true
	rt.mu.Unlock()
	defer func() {
		rt.mu.Lock()
		rt.terminalMenuSelecting = false
		rt.mu.Unlock()
		go rt.RequestFreshSnapshot(defaultNotifySnapshotTimeout)
	}()
	input := option.Input
	for strings.HasPrefix(input, "\x1b[") && len(input) >= 3 {
		if _, err := terminal.Write([]byte(input[:3])); err != nil {
			return err
		}
		input = input[3:]
		// Ink applies selection state on its next render, not in the same key batch.
		time.Sleep(40 * time.Millisecond)
	}
	if (option.SubmitWithEnter || input == " ") && strings.HasPrefix(option.ID, "opt_") {
		if !rt.RequestFreshSnapshot(800 * time.Millisecond) {
			return errors.New("无法确认菜单选中项，请刷新后重试")
		}
		rt.mu.Lock()
		menu := rt.activeTerminalMenuLocked()
		rt.mu.Unlock()
		matched := false
		if menu != nil {
			for _, current := range menu.Options {
				if current.Label == option.Label && (current.Input == "" || current.Input == " ") && strings.HasPrefix(current.ID, "opt_") {
					matched = true
				}
			}
		}
		if !matched {
			return errors.New("菜单已变化，请刷新后重新选择")
		}
	}
	if input != "" {
		if _, err := terminal.Write([]byte(input)); err != nil {
			return err
		}
	}
	if option.SubmitWithEnter {
		if _, err := terminal.Write([]byte("\r")); err != nil {
			return err
		}
	}
	time.Sleep(80 * time.Millisecond)
	return nil
}
