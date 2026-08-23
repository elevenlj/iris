package session

import (
	"strconv"
	"strings"
	"testing"
)

func identityAnchorPolicy(previousGuardLine, currentGuardLine int) notifyTextAnchorPolicy {
	return notifyTextAnchorPolicy{
		allowed:            true,
		enforceIdentity:    true,
		previousGuardLine:  previousGuardLine,
		currentGuardLine:   currentGuardLine,
		previousCursorLine: -1,
		currentCursorLine:  -1,
	}
}

func TestTailAnchorPolicyRejectsUniqueTextAtDifferentGuardRelativeLine(t *testing.T) {
	previous := strings.Join([]string{
		"distinctive previous conclusion alpha beta",
		"distinctive previous supporting gamma delta",
	}, "\n")
	visible := strings.Join([]string{
		"────────────────────────────────────────",
		"distinctive previous conclusion alpha beta",
		"distinctive previous supporting gamma delta",
		"CURRENT_REPLY_MUST_NOT_BE_ATTRIBUTED",
	}, "\n")

	got, ok := visibleTextAfterPreviousTailAnchorWithPolicy(
		visible,
		previous,
		"input echo missing from snapshot",
		nil,
		5,
		identityAnchorPolicy(0, 0),
	)
	if ok || got != "" {
		t.Fatalf("a unique textual occurrence at a different guard-relative line is not the same boundary: ok=%v body=%q", ok, got)
	}
}

func TestTailAnchorPolicyAllowsWholeSnapshotMovementWithStableGuardRelativeLine(t *testing.T) {
	previous := strings.Join([]string{
		"distinctive previous conclusion alpha beta",
		"distinctive previous supporting gamma delta",
	}, "\n")
	visible := strings.Join([]string{
		"────────────────────────────────────────",
		"distinctive previous conclusion alpha beta",
		"distinctive previous supporting gamma delta",
		"CURRENT_REPLY_ONLY",
	}, "\n")

	got, ok := visibleTextAfterPreviousTailAnchorWithPolicy(
		visible,
		previous,
		"input echo missing from snapshot",
		nil,
		5,
		identityAnchorPolicy(0, 1),
	)
	if !ok || got != "CURRENT_REPLY_ONLY" {
		t.Fatalf("moving the guard and boundary together must retain their identity: ok=%v body=%q", ok, got)
	}
}

func TestTailAnchorPolicyRejectsBoundaryReemittedByCurrentRound(t *testing.T) {
	previous := strings.Join([]string{
		"distinctive previous conclusion alpha beta",
		"distinctive previous supporting gamma delta",
	}, "\n")
	visible := previous + "\nCURRENT_REPLY_AFTER_REDRAW"
	roundReply := []byte("redrawing the screen\r\n" + previous + "\r\nCURRENT_REPLY_AFTER_REDRAW")

	got, ok := visibleTextAfterPreviousTailAnchorWithPolicy(
		visible,
		previous,
		"input echo missing from snapshot",
		roundReply,
		5,
		identityAnchorPolicy(0, 0),
	)
	if ok || got != "" {
		t.Fatalf("text emitted again by this round cannot prove that the old boundary survived: ok=%v body=%q", ok, got)
	}
}

func TestTailAnchorPolicySupportsTwoThroughFiveLineProductionBoundaries(t *testing.T) {
	for anchorLines := 2; anchorLines <= 5; anchorLines++ {
		t.Run(strconv.Itoa(anchorLines)+"_lines", func(t *testing.T) {
			anchor := []string{
				"> markdown-looking previous conclusion alpha beta",
				"    indented supporting detail gamma delta",
				"third distinctive boundary epsilon zeta",
				"fourth distinctive boundary eta theta",
				"fifth distinctive boundary iota kappa",
			}[:anchorLines]
			previous := strings.Join(anchor, "\n")
			visible := strings.Join(append(append([]string{
				"────────────────────────────────────────",
			}, anchor...), "CURRENT_REPLY_ONLY"), "\n")

			got, ok := visibleTextAfterPreviousTailAnchorWithPolicy(
				visible,
				previous,
				"input echo missing from snapshot",
				nil,
				5,
				identityAnchorPolicy(0, 1),
			)
			if !ok || got != "CURRENT_REPLY_ONLY" {
				t.Fatalf("a guarded %d-line Markdown/indented boundary should remain usable: ok=%v body=%q", anchorLines, ok, got)
			}
		})
	}
}

func TestTailAnchorRequiresTwoThroughFiveLinesWhenShorterSuffixRepeats(t *testing.T) {
	lines := []string{
		"boundary alpha has distinctive context 01",
		"boundary beta has distinctive context 02",
		"boundary gamma has distinctive context 03",
		"boundary delta has distinctive context 04",
		"boundary epsilon has distinctive context 05",
	}
	for required := 2; required <= 5; required++ {
		t.Run(strconv.Itoa(required)+"_lines_required", func(t *testing.T) {
			// Repeat the final required-1 lines immediately before the real tail.
			// Thus one line is ambiguous in the 2-line case, two lines are
			// ambiguous in the 3-line case, and so on through a required
			// five-line identity boundary.
			shorter := append([]string{}, lines[1:required]...)
			previousLines := append(append([]string{}, shorter...), lines[:required]...)
			previous := strings.Join(previousLines, "\n")
			visible := previous + "\nCURRENT_REPLY_ONLY"

			got, ok := visibleTextAfterPreviousTailAnchorWithPolicy(
				visible,
				previous,
				"input echo missing from snapshot",
				nil,
				required,
				permissiveNotifyTextAnchorPolicy(true),
			)
			if !ok || got != "CURRENT_REPLY_ONLY" {
				t.Fatalf("the unique %d-line suffix must win over its repeated %d-line suffix: ok=%v body=%q", required, required-1, ok, got)
			}

			if required > 2 {
				got, ok = visibleTextAfterPreviousTailAnchorWithPolicy(
					visible,
					previous,
					"input echo missing from snapshot",
					nil,
					required-1,
					permissiveNotifyTextAnchorPolicy(true),
				)
				if ok || got != "" {
					t.Fatalf("a repeated %d-line suffix must fail closed: ok=%v body=%q", required-1, ok, got)
				}
			}
		})
	}
}

func TestInputAnchorWinsWhenCurrentReplyReemitsPreviousTail(t *testing.T) {
	const input = "请审视这个锚点并只发送当前轮回答"
	previousTail := []string{
		"> markdown-looking previous conclusion alpha beta",
		"    indented code-like supporting detail gamma delta",
	}
	previous := strings.Join([]string{
		previousTail[0],
		previousTail[1],
		"› " + input,
		"gpt-5.6-sol medium fast · ~/project/iris",
	}, "\n")
	visible := strings.Join([]string{
		previousTail[0],
		previousTail[1],
		"› " + input,
		"• CURRENT_REPLY_START",
		"• 当前轮复述旧尾段如下：",
		previousTail[0],
		previousTail[1],
		"• CURRENT_REPLY_END",
		"gpt-5.6-sol medium fast · ~/project/iris",
	}, "\n")
	roundReply := []byte(strings.Join([]string{
		"• CURRENT_REPLY_START",
		previousTail[0],
		previousTail[1],
		"• CURRENT_REPLY_END",
	}, "\r\n"))

	policy := identityAnchorPolicy(0, 0)
	policy.previousCursorLine = 2
	got := pickNotifyContentWithWindowAnchorPolicy(
		visible,
		previous,
		roundReply,
		input,
		"",
		policy,
	)
	want := strings.Join([]string{
		"• CURRENT_REPLY_START",
		"• 当前轮复述旧尾段如下：",
		previousTail[0],
		previousTail[1],
		"• CURRENT_REPLY_END",
	}, "\n")
	if got != want {
		t.Fatalf("a verified input anchor must take precedence over an ambiguous reemitted tail:\n%q\nwant:\n%q", got, want)
	}
}

func TestTailAnchorPreservesMarkdownCodeBlankAndIndentationBoundaries(t *testing.T) {
	previous := strings.Join([]string{
		"> markdown quote with distinctive previous context",
		"",
		"    func stableBoundary() string {",
		"        return \"distinctive previous code value\"",
		"    }",
	}, "\n")
	visible := previous + "\n\nCURRENT_REPLY_ONLY"

	got, ok := visibleTextAfterPreviousTailAnchorWithPolicy(
		visible,
		previous,
		"input echo missing from snapshot",
		nil,
		5,
		permissiveNotifyTextAnchorPolicy(true),
	)
	if !ok || got != "\nCURRENT_REPLY_ONLY" {
		t.Fatalf("Markdown/code indentation must remain exact while blank lines remain response data: ok=%v body=%q", ok, got)
	}

	reflowed := strings.Replace(visible, "    func stableBoundary", "func stableBoundary", 1)
	if got, ok := visibleTextAfterPreviousTailAnchorWithPolicy(
		reflowed,
		previous,
		"input echo missing from snapshot",
		nil,
		5,
		permissiveNotifyTextAnchorPolicy(true),
	); ok || got != "" {
		t.Fatalf("changed code indentation must invalidate the textual boundary: ok=%v body=%q", ok, got)
	}
}

func TestNotifyAnchorPolicyDoesNotLetStrictAppendBypassIdentity(t *testing.T) {
	previous := strings.Join([]string{
		"distinctive previous conclusion alpha beta",
		"distinctive previous supporting gamma delta",
	}, "\n")
	visible := previous + "\nCURRENT_REPLY_MUST_NOT_BE_ATTRIBUTED"

	got := pickNotifyContentWithWindowAnchorPolicy(
		visible,
		previous,
		nil,
		"input echo missing from snapshot",
		"",
		identityAnchorPolicy(0, 1),
	)
	if got != "" {
		t.Fatalf("strict textual append must not bypass a failed renderer-identity check: %q", got)
	}
}

func TestComposerAnchorPolicyUsesGuardRelativeIdentity(t *testing.T) {
	const input = "请分析这个跨会话通信方案并给出结论"
	previous := "    › " + input

	t.Run("whole snapshot movement is accepted", func(t *testing.T) {
		visible := strings.Join([]string{
			"────────────────────────────────────────",
			"    › " + input,
			"    • CURRENT_REPLY_ONLY",
		}, "\n")

		policy := identityAnchorPolicy(0, 1)
		policy.previousCursorLine = 0
		got := pickNotifyContentWithWindowAnchorPolicy(
			visible,
			previous,
			nil,
			input,
			"",
			policy,
		)
		if got != "• CURRENT_REPLY_ONLY" {
			t.Fatalf("the same prompt at the same guard-relative line should locate its reply: %q", got)
		}
	})

	t.Run("same text at another relative line is accepted", func(t *testing.T) {
		visible := strings.Join([]string{
			"────────────────────────────────────────",
			"    › " + input,
			"    • CURRENT_REPLY_MUST_NOT_BE_ATTRIBUTED",
		}, "\n")

		policy := identityAnchorPolicy(0, 0)
		policy.previousCursorLine = 0
		got := pickNotifyContentWithWindowAnchorPolicy(
			visible,
			previous,
			nil,
			input,
			"",
			policy,
		)
		if got != "• CURRENT_REPLY_MUST_NOT_BE_ATTRIBUTED" {
			t.Fatalf("renderer-relative position must not veto a matching input prompt: %q", got)
		}
	})
}

func TestComposerAnchorPolicyAcceptsPromptSettledBelowPreEnterCursor(t *testing.T) {
	const input = "你好"
	previous := strings.Join([]string{
		"› 你好",
		"• historical answer",
		"› 你好",
	}, "\n")
	visible := strings.Join([]string{
		"› 你好",
		"• historical answer",
		"────────────────────",
		"› 你好",
		"• CURRENT_REPLY_ONLY",
	}, "\n")

	policy := identityAnchorPolicy(0, 0)
	policy.previousCursorLine = 2
	policy.currentCursorLine = 4
	got := pickNotifyContentWithWindowAnchorPolicy(visible, previous, nil, input, "", policy)
	if got != "• CURRENT_REPLY_ONLY" {
		t.Fatalf("the submitted prompt should be found between the projected pre-Enter cursor and current cursor: %q", got)
	}
}

func TestComposerCursorWindowUsesLatestMatchingPrompt(t *testing.T) {
	const input = "你好"
	previous := "› 你好"
	visible := strings.Join([]string{
		"────────────────────",
		"› 你好",
		"• CURRENT_REPLY_START",
		"› 你好",
		"• CURRENT_REPLY_END",
	}, "\n")

	policy := identityAnchorPolicy(0, 0)
	policy.previousCursorLine = 0
	policy.currentCursorLine = 4
	got := pickNotifyContentWithWindowAnchorPolicy(visible, previous, nil, input, "", policy)
	want := "• CURRENT_REPLY_END"
	if got != want {
		t.Fatalf("the latest prompt in the submitted cursor window must be used as the ambiguity fallback:\n%q\nwant:\n%q", got, want)
	}
}

func TestComposerAnchorPolicyUsesNewInputPromptWhenTUIBaselineTextIsRedrawn(t *testing.T) {
	const input = "hello"
	previous := strings.Join([]string{
		"› hello",
		"• historical answer",
		"Find and fix a bug in @filenamegpt-5.6-sol high fast · ~/project/irishello",
	}, "\n")
	visible := strings.Join([]string{
		"› hello",
		"• historical answer",
		"────────────────────────────────────────",
		"› hello",
		"• CURRENT_REPLY_ONLY",
	}, "\n")

	policy := identityAnchorPolicy(0, 1)
	policy.previousCursorLine = 2
	got := pickNotifyContentWithWindowAnchorPolicy(visible, previous, nil, input, "", policy)
	if got != "• CURRENT_REPLY_ONLY" {
		t.Fatalf("the newly-added Codex input prompt should recover a TUI-redrawn composer baseline: %q", got)
	}
}

func TestComposerPromptRecoveryIgnoresGuardedWindow(t *testing.T) {
	const input = "hello"
	previous := strings.Join([]string{
		"• historical answer",
		"Find and fix a bug in @filenamegpt-5.6-sol high fast · ~/project/irishello",
	}, "\n")
	visible := strings.Join([]string{
		"────────────────────────────────────────",
		"› hello",
		"• CURRENT_REPLY_MUST_NOT_BE_ATTRIBUTED",
	}, "\n")

	policy := identityAnchorPolicy(0, 1)
	policy.previousCursorLine = 1
	if got := pickNotifyContentWithWindowAnchorPolicy(visible, previous, nil, input, "", policy); got != "• CURRENT_REPLY_MUST_NOT_BE_ATTRIBUTED" {
		t.Fatalf("a matching input prompt must not be rejected by the guarded window: %q", got)
	}
}

func TestComposerPromptRecoveryUsesLatestMatchingPromptFallback(t *testing.T) {
	const input = "hello"
	previous := strings.Join([]string{
		"› hello",
		"• historical answer",
		"gpt-5.6-sol high fast · ~/project/irishello",
	}, "\n")
	visible := strings.Join([]string{
		"› hello",
		"• historical answer",
		"────────────────────────────────────────",
		"› hello",
		"• CURRENT_REPLY_START",
		"› hello",
		"• CURRENT_REPLY_END",
	}, "\n")

	policy := identityAnchorPolicy(0, 1)
	policy.previousCursorLine = 2
	got := pickNotifyContentWithWindowAnchorPolicy(visible, previous, nil, input, "", policy)
	want := "• CURRENT_REPLY_END"
	if got != want {
		t.Fatalf("prompt recovery should use the latest matching prompt when identity is ambiguous:\n%q\nwant:\n%q", got, want)
	}
}

func TestComposerPromptRecoveryUsesLatestMatchWhenOccurrenceCountStaysEqual(t *testing.T) {
	const input = "你好"
	previous := strings.Join([]string{
		"────────────────────",
		"› 你好",
		"• historical answer",
		"› 你好",
		"gpt-5.6-sol high fast · ~/project",
	}, "\n")
	visible := strings.Join([]string{
		"────────────────────",
		"› 你好",
		"• historical answer",
		"› 你好",
		"• CURRENT_REPLY_ONLY",
		"gpt-5.6-sol high fast · ~/project",
	}, "\n")
	policy := identityAnchorPolicy(0, 0)
	policy.previousCursorLine = 4 // TUI footer, not the input row.
	policy.currentCursorLine = 5

	if got := pickNotifyContentWithWindowAnchorPolicy(visible, previous, nil, input, "", policy); got != "• CURRENT_REPLY_ONLY" {
		t.Fatalf("equal prompt counts must still use the latest guarded match, got %q", got)
	}
}

func TestComposerPromptRecoveryUsesLatestMatchWhenHeaderRowsDisappear(t *testing.T) {
	const input = "你好"
	previous := strings.Join([]string{
		"guard",
		"banner row one",
		"banner row two",
		"› 你好",
		"• historical answer one",
		"› 你好",
		"• historical answer two",
		"› 你好",
		"footer",
	}, "\n")
	visible := strings.Join([]string{
		"guard",
		"› 你好",
		"• historical answer one",
		"› 你好",
		"• historical answer two",
		"› 你好",
		"• CURRENT_REPLY_ONLY",
	}, "\n")
	policy := identityAnchorPolicy(0, 0)
	policy.previousCursorLine = 8
	policy.currentCursorLine = 6

	if got := pickNotifyContentWithWindowAnchorPolicy(visible, previous, nil, input, "", policy); got != "• CURRENT_REPLY_ONLY" {
		t.Fatalf("a redraw that removes header rows must still use the latest guarded input, got %q", got)
	}
}

func TestSubmittedPromptGlyphDoesNotRequireCursorProof(t *testing.T) {
	const input = "与本轮输入完全相同"
	for _, prompt := range []string{"> ", "$ ", "› "} {
		for _, cursorLine := range []int{-1, 1} {
			name := strings.TrimSpace(prompt) + "_cursor_" + strconv.Itoa(cursorLine)
			t.Run(name, func(t *testing.T) {
				previous := prompt + input
				visible := previous + "\n• HISTORICAL_TEXT_MUST_NOT_AUTHORIZE_THIS_REPLY"
				policy := identityAnchorPolicy(0, 0)
				policy.previousCursorLine = cursorLine

				want := ""
				if prompt == "> " || prompt == "› " {
					want = "• HISTORICAL_TEXT_MUST_NOT_AUTHORIZE_THIS_REPLY"
				}
				if got := visibleTextFromLastInputWithPolicy(visible, previous, input, policy); got != want {
					t.Fatalf("prompt %q cursor-independent result = %q, want %q", prompt, got, want)
				}
				if got := pickNotifyContentWithWindowAnchorPolicy(visible, previous, nil, input, "", policy); got != want {
					t.Fatalf("full selector prompt %q result = %q, want %q", prompt, got, want)
				}
			})
		}
	}
}

func TestStrictInputAnchorsKeepStructuredCodexAndShellPrompts(t *testing.T) {
	tests := []struct {
		name       string
		prompt     string
		cursorLine int
	}{
		{name: "structured input with cursor proof", prompt: "> ", cursorLine: 0},
		{name: "Codex composer", prompt: "› ", cursorLine: 0},
		{name: "shell prompt", prompt: "$ ", cursorLine: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const input = "run the current verified request"
			previous := tt.prompt + input
			visible := previous + "\nCURRENT_REPLY_ONLY"
			policy := identityAnchorPolicy(0, 0)
			policy.previousCursorLine = tt.cursorLine
			if got := visibleTextFromLastInputWithPolicy(visible, previous, input, policy); got != "CURRENT_REPLY_ONLY" {
				t.Fatalf("verified %s must remain a valid input anchor: %q", tt.name, got)
			}
		})
	}
}

func TestStrictCodexComposerCursorProofDoesNotDependOnFooterWhitelist(t *testing.T) {
	const input = "你好"
	previous := strings.Join([]string{
		"› 你好",
		"› Use /skills to list available skills",
	}, "\n")
	visible := strings.Join([]string{
		"› 你好",
		"• CURRENT_REPLY_ONLY",
		"› Use /skills to list available skills",
	}, "\n")
	policy := identityAnchorPolicy(0, 0)
	policy.previousCursorLine = 0
	policy.currentCursorLine = 2

	want := "• CURRENT_REPLY_ONLY\n› Use /skills to list available skills"
	if got := visibleTextFromLastInputWithPolicy(visible, previous, input, policy); got != want {
		t.Fatalf("a prompt ending at the pre-Enter cursor is proven even when Codex adds an unknown footer hint:\n%q\nwant:\n%q", got, want)
	}
}

func TestStrictInputAnchorAcceptsWrappedAndMultilineComposerAtCursor(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		previous string
		cursor   int
	}{
		{
			name:     "Codex multiline",
			input:    "first logical line\nsecond logical line\nthird logical line",
			previous: "› first logical line\nsecond logical line\nthird logical line",
			cursor:   2,
		},
		{
			name:     "structured wrapped",
			input:    "first wrapped fragment second wrapped fragment",
			previous: "> first wrapped fragment\nsecond wrapped fragment",
			cursor:   1,
		},
		{
			name:     "shell wrapped",
			input:    "first shell fragment second shell fragment",
			previous: "$ first shell fragment\nsecond shell fragment",
			cursor:   1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			visible := tt.previous + "\nCURRENT_REPLY_ONLY"
			policy := identityAnchorPolicy(0, 0)
			policy.previousCursorLine = tt.cursor
			if got := visibleTextFromLastInputWithPolicy(visible, tt.previous, tt.input, policy); got != "CURRENT_REPLY_ONLY" {
				t.Fatalf("wrapped/multiline composer ending at cursor must remain valid: %q", got)
			}
		})
	}
}

func TestStrictInputAnchorUsesNewestOccurrenceRegardlessOfCursor(t *testing.T) {
	const input = "重复但只有最新一条才可能是 composer"
	previous := strings.Join([]string{
		"› " + input,
		"• historical answer",
		"› " + input,
	}, "\n")
	visible := previous + "\n• CURRENT_REPLY_MUST_NOT_BE_ATTRIBUTED"
	policy := identityAnchorPolicy(0, 0)
	policy.previousCursorLine = 0

	if got := visibleTextFromLastInputWithPolicy(visible, previous, input, policy); got != "• CURRENT_REPLY_MUST_NOT_BE_ATTRIBUTED" {
		t.Fatalf("the newest matching input must be used regardless of cursor metadata: %q", got)
	}
}
