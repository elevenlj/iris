package session

import (
	"strconv"
	"strings"
	"testing"
)

func pickAnchoredNotifyContent(body string) string {
	const previous = "> formatting test"
	return PickNotifyContent(previous+"\n"+body, previous, nil, "formatting test")
}

func TestPickNotifyContentUsesVisibleSnapshot(t *testing.T) {
	got := pickAnchoredNotifyContent("cmd\nrendered output")
	if got != "cmd\nrendered output" {
		t.Fatalf("unexpected content: %q", got)
	}
}

func TestPickNotifyContentIgnoresRoundStartAndUsesVisibleTail(t *testing.T) {
	SetLarkNotifyMaxLines(4)
	t.Cleanup(func() { SetLarkNotifyMaxLines(defaultMaxLarkTextLines) })
	before := strings.Join([]string{
		"> old question",
		"old answer",
	}, "\n")
	visible := before + "\n" + strings.Join([]string{
		"> current question",
		"current answer",
		"1. first",
		"2. second",
	}, "\n")
	got := PickNotifyContent(visible, before, []byte("current answer1.first2.second"), "current question")
	want := strings.Join([]string{
		"current answer",
		"1. first",
		"2. second",
	}, "\n")
	if got != want {
		t.Fatalf("visible tail should be used:\n%q\nwant:\n%q", got, want)
	}
}

func TestPickNotifyContentDoesNotLeakFullSnapshotWhenDiffCannotMatch(t *testing.T) {
	SetLarkNotifyMaxLines(3)
	t.Cleanup(func() { SetLarkNotifyMaxLines(defaultMaxLarkTextLines) })
	visible := strings.Join([]string{
		"header",
		"old answer",
		"current answer",
		"1. first",
		"2. second",
	}, "\n")
	got := PickNotifyContent(visible, "stale snapshot", []byte("current answer1.first2.second"), "current question")
	if got != "" {
		t.Fatalf("an unanchored snapshot must fail closed instead of leaking terminal history: %q", got)
	}
}

func TestPickNotifyContentUsesExplicitInputWithoutBaseline(t *testing.T) {
	visible := strings.Join([]string{
		"OLD_HISTORY_MUST_NOT_LEAK",
		"› this is a unique current question with enough detail",
		"• this reply still has no trustworthy round baseline",
	}, "\n")
	want := "• this reply still has no trustworthy round baseline"
	if got := PickNotifyContent(visible, "", nil, "this is a unique current question with enough detail"); got != want {
		t.Fatalf("an explicit input prompt must work without renderer baseline metadata: %q", got)
	}
}

func TestPickNotifyContentDiffsAfterPreviousTailAnchor(t *testing.T) {
	previous := strings.Join([]string{
		"old heading",
		"old line 1",
		"old line 2",
		"old line 3",
	}, "\n")
	visible := previous + "\n" + strings.Join([]string{
		"new line 1",
		"",
		"new line 2",
	}, "\n")
	got := PickNotifyContent(visible, previous, nil, "next")
	want := "new line 1\n\nnew line 2"
	if got != want {
		t.Fatalf("content should be diffed after previous tail anchor:\n%q\nwant:\n%q", got, want)
	}
}

func TestPickNotifyContentUsesInputAnchorBeforeSnapshotDiff(t *testing.T) {
	previous := strings.Join([]string{
		"› 上海呢",
		"• 上海现在多云。",
	}, "\n")
	visible := strings.Join([]string{
		"› 上海呢",
		"• 上海现在多云，晚间有小雨。",
		"› 深圳呢",
		"• 深圳现在阴天。",
	}, "\n")
	got := PickNotifyContent(visible, previous, nil, "深圳呢")
	want := "• 深圳现在阴天。"
	if got != want {
		t.Fatalf("input anchor should prevent previous rounds leaking into notification:\n%q\nwant:\n%q", got, want)
	}
}

func TestPickNotifyContentUsesNewestRepeatedInputWithoutBaseline(t *testing.T) {
	visible := strings.Join([]string{
		"› 北京呢",
		"• 北京旧结果。",
		"› 北京呢",
		"• 北京新结果。",
	}, "\n")
	got := PickNotifyContent(visible, "", nil, "北京呢")
	if got != "• 北京新结果。" {
		t.Fatalf("repeated input must select the newest occurrence: %q", got)
	}
}

func TestPickNotifyContentUsesShortInputWithoutBaseline(t *testing.T) {
	for _, input := range []string{"hi", "继续"} {
		t.Run(input, func(t *testing.T) {
			visible := strings.Join([]string{
				"OLD_HISTORY_MUST_NOT_LEAK",
				"› " + input,
				"• historical answer",
				"• ambiguous tail",
			}, "\n")
			want := "• historical answer\n• ambiguous tail"
			if got := PickNotifyContent(visible, "", nil, input); got != want {
				t.Fatalf("a short explicit input prompt must remain a valid anchor: %q", got)
			}
		})
	}
}

func TestNotifyContentNeedsMoreSnapshotUsesLatestRepeatedInputInWindow(t *testing.T) {
	previous := strings.Join([]string{
		"> ask",
		"old answer",
	}, "\n")
	visible := strings.Join([]string{
		previous,
		"> ask",
	}, "\n")
	if !notifyContentNeedsMoreSnapshotWithWindow(visible, previous, nil, "ask", "ask") {
		t.Fatalf("window should wait when the latest repeated input has no reply")
	}

	visible += "\nnew answer"
	if notifyContentNeedsMoreSnapshotWithWindow(visible, previous, nil, "ask", "ask") {
		t.Fatalf("window should be ready once the latest repeated input has a reply")
	}
}

func TestPickNotifyContentSkipsMultilineInputAnchor(t *testing.T) {
	input := "第一行\n第二行\n第三行"
	previous := strings.Join([]string{
		"› 第一行",
		"第二行",
		"第三行",
	}, "\n")
	visible := strings.Join([]string{
		previous,
		"• 多行输入后的回复。",
	}, "\n")
	got := PickNotifyContent(visible, previous, nil, input)
	want := "• 多行输入后的回复。"
	if got != want {
		t.Fatalf("multiline input anchor should be skipped from notification:\n%q\nwant:\n%q", got, want)
	}
}

func TestPickNotifyContentFindsIndentedRichTextInputAnchorWithoutPromptSpace(t *testing.T) {
	input := "关于PE的维护不对啊，你应该用V1、V2、V3、V4这种来。F4是什么版本啊？你要按照这个逻辑来。"
	previous := strings.Join([]string{
		"上一轮有缩进的最终输出。",
		"    Worked for 8m 30s",
		"    ›关于PE的维护不对啊，你应该用V1、V2、V3、V4这种来。",
		"      F4是什么版本啊？你要按照这个逻辑来。",
	}, "\n")
	visible := strings.Join([]string{
		previous,
		"    • 这是当前轮回复。",
	}, "\n")

	got := PickNotifyContent(visible, previous, nil, input)
	want := "• 这是当前轮回复。"
	if got != want {
		t.Fatalf("indented rich-text input anchor should exclude prior output:\n%q\nwant:\n%q", got, want)
	}
}

func TestPickNotifyContentFindsIndentedCodexFileMentionAnchor(t *testing.T) {
	input := "@v2/framework/session/player/audio/audio.go这个的改动中，你看一下，因为线上他用的可能是没有文本的TTS。不会有问题吧？"
	previous := strings.Join([]string{
		"• 已修改并推送，提交：cf9cab20。",
		"  - 删除通用 InitEnabledComponents/IsEnabled。",
		"    › v2/framework/session/player/audio/audio.go这个的改动中，你看一下，因为线上他用",
		"      的可能是没有文本的TTS。不会有问题吧？",
	}, "\n")
	visible := strings.Join([]string{
		previous,
		"    • 我按代码审查方式看这个文件。",
	}, "\n")

	got := PickNotifyContent(visible, previous, nil, input)
	want := "• 我按代码审查方式看这个文件。"
	if got != want {
		t.Fatalf("Codex file mention anchor should ignore hidden @ marker and indentation:\n%q\nwant:\n%q", got, want)
	}
}

func TestPickNotifyContentDoesNotInventAnchorAfterRichTextRewrite(t *testing.T) {
	previous := strings.Join([]string{
		"上一轮完整输出。",
		"  - 上一轮列表项。",
		"    › rich-text-file-chip 请检查这个改动",
		"      以及所有兼容情况。",
	}, "\n")
	visible := strings.Join([]string{
		previous,
		"    • 只保留当前轮回复。",
	}, "\n")

	got := PickNotifyContent(visible, previous, nil, "@/tmp/完全不同的原始富文本引用 请检查这个改动以及所有兼容情况。")
	want := ""
	if got != want {
		t.Fatalf("a rewritten prompt that matches neither full input nor its first 30 runes must not become an anchor:\n%q\nwant:\n%q", got, want)
	}
}

func TestPickNotifyContentNormalizesUnicodeAndInvisiblePromptCharacters(t *testing.T) {
	input := "Café 请检查这个问题"
	visible := strings.Join([]string{
		"OLD_SENTINEL",
		"\u200b❯\u00a0Cafe\u0301 \u200d请检查这个问题",
		"● 只保留当前轮回复。",
	}, "\n")

	got := PickNotifyContent(visible, "OLD_SENTINEL", nil, input)
	if got != "● 只保留当前轮回复。" {
		t.Fatalf("Unicode-normalized prompt should anchor the current round, got %q", got)
	}
}

func TestPickNotifyContentMatchesInputEchoContainingANSIControls(t *testing.T) {
	visible := strings.Join([]string{
		"OLD_SENTINEL",
		"\x1b[36m›\x1b[0m \x1b[1m检查 ANSI 锚点\x1b[0m",
		"• 当前轮回复。",
	}, "\n")

	got := PickNotifyContent(visible, "OLD_SENTINEL", nil, "检查 ANSI 锚点")
	if got != "• 当前轮回复。" {
		t.Fatalf("ANSI-decorated input echo should anchor the current round, got %q", got)
	}
}

func TestPickNotifyContentSupportsLongMultilineInputAndPromptLikeContinuation(t *testing.T) {
	inputLines := make([]string, 0, 42)
	inputLines = append(inputLines, "第一行")
	for i := 1; i <= 40; i++ {
		prefix := "普通"
		if i == 12 {
			prefix = "• 用户输入中的项目"
		}
		if i == 25 {
			prefix = "› 用户输入中的示例"
		}
		inputLines = append(inputLines, prefix+strconv.Itoa(i))
	}
	visibleLines := []string{"OLD_SENTINEL", "› " + inputLines[0]}
	visibleLines = append(visibleLines, inputLines[1:]...)
	visibleLines = append(visibleLines, "• 长输入之后的回复。")

	got := PickNotifyContent(strings.Join(visibleLines, "\n"), "OLD_SENTINEL", nil, strings.Join(inputLines, "\n"))
	if got != "• 长输入之后的回复。" {
		t.Fatalf("long multiline input should remain a valid anchor, got %q", got)
	}
}

func TestPickNotifyContentRestoresSoftWrappedLongInputBeforeMatching(t *testing.T) {
	input := strings.Repeat("就是那个", 29)
	runes := []rune(input)
	previous := strings.Join([]string{
		"› " + string(runes[:18]),
		string(runes[18:47]),
		string(runes[47:83]),
		string(runes[83:]),
	}, "\n")
	visible := previous + "\n• 软换行之后的当前轮回复。"

	if got := PickNotifyContent(visible, previous, nil, input); got != "• 软换行之后的当前轮回复。" {
		t.Fatalf("soft-wrapped input should be reconstructed before matching, got %q", got)
	}
}

func TestPickNotifyContentRestoresIndentedEnglishSoftWrapBeforeMatching(t *testing.T) {
	input := "please inspect the terminal snapshot anchor and preserve every intended word boundary before matching"
	previous := strings.Join([]string{
		"› please inspect the terminal snapshot",
		"    anchor and preserve every intended",
		"    word boundary before matching",
	}, "\n")
	visible := previous + "\n• CURRENT_REPLY_ONLY"

	if got := PickNotifyContent(visible, previous, nil, input); got != "• CURRENT_REPLY_ONLY" {
		t.Fatalf("indented soft-wrap fragments should be reconstructed before matching, got %q", got)
	}
}

func TestPickNotifyContentFallsBackToFirstThirtyRunesForChangedLongInputTail(t *testing.T) {
	prefix := strings.Repeat("就是那个", 8)
	input := prefix + "这是提交时记录的原始长尾内容"
	previous := "› " + prefix + "这是终端重绘后发生变化的长尾内容"
	visible := previous + "\n• 只发送这一轮回复。"

	if got := PickNotifyContent(visible, previous, nil, input); got != "• 只发送这一轮回复。" {
		t.Fatalf("a changed long-input tail should use the first %d runes as its fallback anchor, got %q", maxInputAnchorRunes, got)
	}
}

func TestInputAnchorTextCountsUnicodeCharacters(t *testing.T) {
	input := strings.Repeat("中", maxInputAnchorRunes+5)
	got := inputAnchorText(input)
	if len([]rune(got)) != maxInputAnchorRunes {
		t.Fatalf("input anchor contains %d runes, want %d", len([]rune(got)), maxInputAnchorRunes)
	}
}

func TestPickNotifyContentUsesActiveLongPromptWhenHistorySharesThirtyRunePrefix(t *testing.T) {
	prefix := strings.Repeat("共", maxInputAnchorRunes)
	input := prefix + "当前轮的真实长尾"
	previous := strings.Join([]string{
		"› " + prefix + "历史轮的另一个长尾",
		"• OLD_HISTORY_MUST_NOT_LEAK",
		"› " + input,
	}, "\n")
	visible := previous + "\n• CURRENT_REPLY_ONLY"

	if got := PickNotifyContent(visible, previous, nil, input); got != "• CURRENT_REPLY_ONLY" {
		t.Fatalf("the active baseline occurrence must disambiguate prompts sharing the 30-rune fallback prefix, got %q", got)
	}
}

func TestPickNotifyContentDoesNotTreatOrdinaryMentionAsFileChip(t *testing.T) {
	visible := strings.Join([]string{
		"OLD_SENTINEL",
		"› foo",
		"• unrelated reply",
	}, "\n")
	if got := PickNotifyContent(visible, "", nil, "@foo"); got != "" {
		t.Fatalf("ordinary @mention must not collide with an unprefixed prompt: %q", got)
	}
}

func TestPickNotifyContentDoesNotTreatMentionWithLaterPathAsFileChip(t *testing.T) {
	visible := strings.Join([]string{
		"OLD_SENTINEL",
		"› alice 请检查 /tmp/example.go",
		"• unrelated reply",
	}, "\n")
	if got := PickNotifyContent(visible, "", nil, "@alice 请检查 /tmp/example.go"); got != "" {
		t.Fatalf("a user mention followed by a path must not lose its @ marker: %q", got)
	}
}

func TestPickNotifyContentRejectsFullyReflowedUnanchoredHistory(t *testing.T) {
	previous := "OLD_SENTINEL one\nOLD_SENTINEL two\nold footer"
	visible := "reflowed OLD_SENTINEL one and two\nnew answer without a prompt\nnew footer"
	if got := PickNotifyContent(visible, previous, nil, "input missing from snapshot"); got != "" {
		t.Fatalf("fully reflowed history without a trusted boundary must fail closed: %q", got)
	}
}

func TestPickLarkNotifyFallbackTailContentUsesConfiguredNewestLines(t *testing.T) {
	SetLarkNotifyFallbackTailLines(3)
	t.Cleanup(func() { SetLarkNotifyFallbackTailLines(defaultFallbackTailLines) })

	got := pickLarkNotifyFallbackTailContent(strings.Join([]string{
		"older line one",
		"older line two",
		"fallback line three",
		"fallback line four",
		"fallback line five",
	}, "\n"))
	want := strings.Join([]string{
		"fallback line three",
		"fallback line four",
		"fallback line five",
	}, "\n")
	if got != want {
		t.Fatalf("fallback tail = %q, want %q", got, want)
	}
}

func TestPickLarkNotifyFallbackTailContentAppliesConfiguredFilters(t *testing.T) {
	SetLarkNotifyFallbackTailLines(2)
	if err := SetLarkNotifyDropLineRules([]LarkNotifyDropLineRule{{
		Kind: "line", Pattern: `^noise:`,
	}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		SetLarkNotifyFallbackTailLines(defaultFallbackTailLines)
		if err := SetLarkNotifyDropLineRules(nil); err != nil {
			t.Fatal(err)
		}
	})

	got := pickLarkNotifyFallbackTailContent(strings.Join([]string{
		"older line",
		"noise: tool status",
		"new reply one",
		"new reply two",
	}, "\n"))
	if got != "new reply one\nnew reply two" {
		t.Fatalf("filtered fallback tail = %q", got)
	}
}

func TestPickLarkNotifyHookAssistantContentAppliesConfiguredFilters(t *testing.T) {
	if err := SetLarkNotifyDropLineRules([]LarkNotifyDropLineRule{{
		Kind: "line", Pattern: `.*PostToolUse.*`,
	}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := SetLarkNotifyDropLineRules(nil); err != nil {
			t.Fatal(err)
		}
	})

	got := pickLarkNotifyHookAssistantContent("PostToolUse noisy status\n本轮最终回复")
	if got != "本轮最终回复" {
		t.Fatalf("filtered Hook assistant content = %q", got)
	}
}

func TestPickNotifyContentRejectsReflowWithOnlyWeakCommonFooter(t *testing.T) {
	previous := strings.Join([]string{
		"OLD_SENTINEL first historical line",
		"OLD_SENTINEL second historical line",
		"gpt-5.6-sol medium fast · ~/project/iris",
	}, "\n")
	visible := strings.Join([]string{
		"reflowed OLD_SENTINEL first and second historical lines",
		"current answer without a trustworthy prompt",
		"gpt-5.6-sol medium fast · ~/project/iris",
	}, "\n")
	if got := PickNotifyContent(visible, previous, nil, "missing current input"); got != "" {
		t.Fatalf("a shared status footer must not authorize a near-full-screen diff: %q", got)
	}
}

func TestPickNotifyContentExtractsOnlyActiveInteractionMenu(t *testing.T) {
	visible := strings.Join([]string{
		"OLD_SENTINEL historical question",
		"OLD_SENTINEL historical answer",
		"Select Model and Effort",
		"1. gpt-5.6-sol (current)  Frontier model",
		"2. gpt-5.6-terra          Balanced model",
		"Press enter to confirm or esc to go back",
	}, "\n")
	got := PickNotifyContent(visible, "unrelated baseline", nil, "/model")
	if strings.Contains(got, "OLD_SENTINEL") || !strings.HasPrefix(got, "Select Model and Effort\n") {
		t.Fatalf("interaction fallback must return only the active menu block: %q", got)
	}
}

func TestPickNotifyContentRejectsStaleReasoningMenuForUnrelatedInput(t *testing.T) {
	visible := strings.Join([]string{
		"OLD_SENTINEL",
		"Select Reasoning Level for gpt-5.6-sol",
		"1. Low",
		"2. Medium (default)",
		"3. High",
	}, "\n")
	if got := PickNotifyContent(visible, "different baseline", nil, "成都天气如何"); got != "" {
		t.Fatalf("an old reasoning menu must not match an unrelated turn: %q", got)
	}
}

func TestPickNotifyContentDoesNotDiffStaleReasoningMenuAfterNumericInput(t *testing.T) {
	previous := "stable unrelated baseline line one\nstable unrelated baseline line two"
	visible := previous + "\n" + strings.Join([]string{
		"Select Reasoning Level for gpt-5.6-sol",
		"1. Low",
		"2. Medium (default)",
		"3. High",
	}, "\n")
	if got := PickNotifyContent(visible, previous, nil, "1"); got != "" {
		t.Fatalf("generic diff must not bypass stale Reasoning-menu context checks: %q", got)
	}
}

func TestPickNotifyContentUsesExplicitRepeatedInputAfterReflow(t *testing.T) {
	previous := "OLD_SENTINEL historical header\n› repeat question\n• historical answer"
	visible := "reflowed OLD_SENTINEL historical header\n› repeat question\n• historical answer\n• ambiguous new tail"
	want := "• historical answer\n• ambiguous new tail"
	if got := PickNotifyContent(visible, previous, nil, "repeat question"); got != want {
		t.Fatalf("the explicit input prompt must remain the boundary after a reflow: %q", got)
	}
}

func TestPickNotifyContentUsesComposerAnchorAlreadyPresentInBaseline(t *testing.T) {
	previous := strings.Join([]string{
		"OLD_SENTINEL historical answer",
		"› 当前问题",
		"gpt-5.6-sol medium fast · ~/project/iris",
	}, "\n")
	visible := strings.Join([]string{
		"OLD_SENTINEL historical answer",
		"› 当前问题",
		"• 只发送当前回答。",
		"gpt-5.6-sol medium fast · ~/project/iris",
	}, "\n")
	if got := PickNotifyContent(visible, previous, nil, "当前问题"); got != "• 只发送当前回答。" {
		t.Fatalf("the pre-Enter composer line should remain a valid round anchor, got %q", got)
	}
}

func TestPickNotifyContentUsesCorrectRepeatedComposerOccurrenceFromBaseline(t *testing.T) {
	previous := strings.Join([]string{
		"› 重复问题",
		"• OLD_SENTINEL old answer",
		"› 重复问题",
		"gpt-5.6-sol high fast · ~/project/iris",
	}, "\n")
	visible := strings.Join([]string{
		"› 重复问题",
		"• OLD_SENTINEL old answer",
		"› 重复问题",
		"• 当前回答里引用了：重复问题",
		"gpt-5.6-sol high fast · ~/project/iris",
	}, "\n")
	want := "• 当前回答里引用了：重复问题"
	if got := PickNotifyContent(visible, previous, nil, "重复问题"); got != want {
		t.Fatalf("the baseline's newest repeated composer must be selected by occurrence: got %q, want %q", got, want)
	}
}

func TestPickNotifyContentUsesNewestVisiblePromptRegardlessOfBaselineCursor(t *testing.T) {
	previous := strings.Join([]string{
		"› 重复问题",
		"• OLD_SENTINEL historical reply",
	}, "\n")
	visible := previous + "\n• ambiguous new tail"
	if _, ok := newestInputAnchorSpan(splitVisibleLines(visible), splitVisibleLines(previous), "重复问题"); !ok {
		t.Fatal("an explicit input prompt must not be rejected by baseline cursor state")
	}
	want := "• OLD_SENTINEL historical reply\n• ambiguous new tail"
	if got := PickNotifyContent(visible, previous, nil, "重复问题"); got != want {
		t.Fatalf("content must be cut after the newest visible input prompt, got %q", got)
	}
}

func TestPickNotifyContentUsesNewestRemainingDuplicateWhenBaselineOccurrenceDisappears(t *testing.T) {
	previous := strings.Join([]string{
		"› 重复问题",
		"ok",
		"› 重复问题",
		"gpt-5.6-sol medium fast · ~/project/iris",
	}, "\n")
	visible := strings.Join([]string{
		"› 重复问题",
		"ok",
		"• ambiguous answer after the active echo disappeared",
	}, "\n")
	want := "ok\n• ambiguous answer after the active echo disappeared"
	if got := PickNotifyContent(visible, previous, nil, "重复问题"); got != want {
		t.Fatalf("the newest remaining matching input must be used: %q", got)
	}
}

func TestPickNotifyContentRejectsLongButDifferentPromptPrefix(t *testing.T) {
	visible := strings.Join([]string{
		"OLD_SENTINEL",
		"› 请你认真分析一下这个系统中的模块A具体实现",
		"• historical answer",
	}, "\n")
	if got := PickNotifyContent(visible, "", nil, "请你认真分析一下这个系统中的模块B安全边界"); got != "" {
		t.Fatalf("a long common prefix alone must not select another prompt: %q", got)
	}
}

func TestPickNotifyContentRejectsNearIdenticalHistoricalPromptWithoutBaseline(t *testing.T) {
	visible := strings.Join([]string{
		"› 请完整检查这个模块的全部兼容性问题，并修复发现的缺陷。",
		"• OLD_SENTINEL historical answer",
	}, "\n")
	input := "请完整检查这个模块的全部兼容性问题，并修复发现的缺陷！"
	if got := PickNotifyContent(visible, "", nil, input); got != "" {
		t.Fatalf("a one-character difference must not fuzzy-match a historical prompt: %q", got)
	}
}

func TestPickNotifyContentPreservesSemanticCodeIndentation(t *testing.T) {
	previous := "› show code"
	visible := strings.Join([]string{
		previous,
		"    func main() {",
		"        println(\"ok\")",
		"    }",
	}, "\n")
	want := "    func main() {\n        println(\"ok\")\n    }"
	if got := PickNotifyContent(visible, previous, nil, "show code"); got != want {
		t.Fatalf("semantic response indentation must be preserved:\n%q\nwant:\n%q", got, want)
	}
}

func TestPickNotifyContentKeepsWhitespaceBoundariesInSingleLineAnchor(t *testing.T) {
	visible := "OLD_SENTINEL\n› a bc\n• wrong answer"
	if got := PickNotifyContent(visible, "", nil, "ab c"); got != "" {
		t.Fatalf("different whitespace boundaries must not collide: %q", got)
	}
}

func TestPickNotifyContentUsesTailBeforeStableFooter(t *testing.T) {
	previous := strings.Join([]string{
		"older stable output detail",
		"old output",
		"gpt-5.4 low fast · ~/Iris_Workspace/测试",
	}, "\n")
	visible := strings.Join([]string{
		"older stable output detail",
		"old output",
		"• Ran lsof -nP -iTCP:8083 -sTCP:LISTEN",
		"  (no output)",
		"已关闭 8083 接口。",
		"gpt-5.4 low fast · ~/Iris_Workspace/测试",
	}, "\n")
	got := PickNotifyContent(visible, previous, nil, "关闭 8083")
	want := strings.Join([]string{
		"• Ran lsof -nP -iTCP:8083 -sTCP:LISTEN",
		"  (no output)",
		"已关闭 8083 接口。",
	}, "\n")
	if got != want {
		t.Fatalf("content after the stable output tail should be selected:\n%q\nwant:\n%q", got, want)
	}
}

func TestPickNotifyContentKeepsFullAppendWhenReplyRepeatsPreviousTail(t *testing.T) {
	previous := strings.Join([]string{
		"anchor one",
		"anchor two",
		"anchor three",
	}, "\n")
	visible := strings.Join([]string{
		"anchor one",
		"anchor two",
		"anchor three",
		"old duplicate content",
		"anchor one",
		"anchor two",
		"anchor three",
		"new content only",
	}, "\n")
	got := PickNotifyContent(visible, previous, nil, "next")
	want := strings.Join([]string{
		"old duplicate content",
		"anchor one",
		"anchor two",
		"anchor three",
		"new content only",
	}, "\n")
	if got != want {
		t.Fatalf("an appended repeated tail must not replace the original boundary: got %q, want %q", got, want)
	}
}

func TestPickNotifyContentUsesUniquePreviousTailAnchorsFromTwoThroughFiveLines(t *testing.T) {
	for anchorLines := 2; anchorLines <= 5; anchorLines++ {
		t.Run(strconv.Itoa(anchorLines)+"_lines", func(t *testing.T) {
			anchor := make([]string, 0, anchorLines)
			for i := 0; i < anchorLines; i++ {
				anchor = append(anchor, "distinctive previous boundary line "+strconv.Itoa(i+1)+" alpha beta")
			}
			previous := strings.Join(anchor, "\n")
			visible := strings.Join(append(append([]string{"OLDER_VIEWPORT_HISTORY"}, anchor...), "CURRENT_REPLY_ONLY"), "\n")
			if got := PickNotifyContent(visible, previous, nil, "input echo missing from current snapshot"); got != "CURRENT_REPLY_ONLY" {
				t.Fatalf("the unique %d-line previous tail should locate only the current reply, got %q", anchorLines, got)
			}
		})
	}
}

func TestAdaptiveTailAnchorPrefersLongestUniqueSuffix(t *testing.T) {
	anchor := []string{
		"distinctive previous boundary one alpha beta",
		"distinctive previous boundary two gamma delta",
		"distinctive previous boundary three epsilon zeta",
		"distinctive repeated suffix four eta theta",
		"distinctive repeated suffix five iota kappa",
	}
	previous := strings.Join(anchor, "\n")
	want := strings.Join([]string{anchor[3], anchor[4], "CURRENT_REPLY_ONLY"}, "\n")
	visible := strings.Join(append(append(append([]string{"OLDER_VIEWPORT_HISTORY"}, anchor...), anchor[3], anchor[4]), "CURRENT_REPLY_ONLY"), "\n")
	if got := PickNotifyContent(visible, previous, nil, "input echo missing from current snapshot"); got != want {
		t.Fatalf("the unique five-line suffix must win even when its two-line suffix repeats later: got %q, want %q", got, want)
	}
}

func TestAdaptiveTailAnchorFailsClosedForUnsafeOccurrencesAndIndentationRewrite(t *testing.T) {
	previous := strings.Join([]string{
		"distinctive previous conclusion alpha beta",
		"    distinctive previous supporting gamma delta",
	}, "\n")
	tests := []struct {
		name    string
		visible string
	}{
		{
			name:    "zero occurrences",
			visible: "UNRELATED_HISTORY\nOLD_HISTORY_MUST_NOT_LEAK",
		},
		{
			name:    "multiple occurrences",
			visible: previous + "\nreply quote\n" + previous + "\nOLD_HISTORY_MUST_NOT_LEAK",
		},
		{
			name: "indentation changed",
			visible: strings.Join([]string{
				"distinctive previous conclusion alpha beta",
				"distinctive previous supporting gamma delta",
				"OLD_HISTORY_MUST_NOT_LEAK",
			}, "\n"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, ok := visibleTextAfterPreviousTailAnchor(tt.visible, previous, "", 5); ok || got != "" {
				t.Fatalf("unsafe previous-tail match must fail closed: ok=%v body=%q", ok, got)
			}
		})
	}
}

func TestAdaptiveTailAnchorRejectsDuplicateIntroducedByReply(t *testing.T) {
	previous := "• distinctive previous conclusion line\n  distinctive previous supporting detail"
	visible := previous + "\nreply introduction\n" + previous + "\nfinal conclusion"
	if got, ok := visibleTextAfterPreviousTailAnchor(visible, previous, "", 5); ok || got != "" {
		t.Fatalf("a reply quoting the tail makes its occurrence ambiguous: ok=%v body=%q", ok, got)
	}
}

func TestAdaptiveTailAnchorDoesNotDowngradeAfterSelectedCandidateDisappears(t *testing.T) {
	previous := strings.Join([]string{
		"UNIQUE_CONTEXT_FOR_BASELINE",
		"shared tail detail alpha",
		"shared tail detail beta",
	}, "\n")
	visible := strings.Join([]string{
		"OTHER_CONTEXT_FROM_HISTORY",
		"shared tail detail alpha",
		"shared tail detail beta",
		"OLD_HISTORY_MUST_NOT_LEAK",
	}, "\n")
	if got, ok := visibleTextAfterPreviousTailAnchor(visible, previous, "", 5); ok || got != "" {
		t.Fatalf("a missing selected candidate must not downgrade to a weaker suffix: ok=%v body=%q", ok, got)
	}
}

func TestPickNotifyContentUsesAdaptiveTailAnchorAcrossChangingTUIFooter(t *testing.T) {
	previous := strings.Join([]string{
		"• 上一轮结论具有足够长度和辨识度。",
		"  上一轮补充说明同样具有足够长度。",
		"gpt-5.6-sol medium fast · ~/project/iris",
		"1 background terminal running · /ps to view · /stop to close",
	}, "\n")
	visible := strings.Join([]string{
		"• 上一轮结论具有足够长度和辨识度。",
		"  上一轮补充说明同样具有足够长度。",
		"• 本轮只应发送这一条新回答。",
		"gpt-5.6-terra high fast · ~/project/iris",
		"2 background terminals running · /ps to view · /stop to close",
	}, "\n")
	if got := PickNotifyContent(visible, previous, nil, "当前输入没有显示在屏幕中"); got != "• 本轮只应发送这一条新回答。" {
		t.Fatalf("stable tail lines should survive changing TUI chrome, got %q", got)
	}
}

func TestAdaptiveTailAnchorUsesPreviousOutputWhenComposerEchoDisappears(t *testing.T) {
	previous := strings.Join([]string{
		"• 上一轮结论具有足够长度和辨识度。",
		"  上一轮补充说明同样具有足够长度。",
		"Worked for 8m 30s",
		"› 当前输入可能在重绘后消失",
		"gpt-5.6-sol medium fast · ~/project/iris",
	}, "\n")
	visible := strings.Join([]string{
		"• 上一轮结论具有足够长度和辨识度。",
		"  上一轮补充说明同样具有足够长度。",
		"Worked for 8m 30s",
		"• CURRENT_REPLY_ONLY",
		"gpt-5.6-sol medium fast · ~/project/iris",
	}, "\n")
	if got := PickNotifyContent(visible, previous, nil, "当前输入可能在重绘后消失"); got != "• CURRENT_REPLY_ONLY" {
		t.Fatalf("the previous output tail should survive a disappearing composer echo, got %q", got)
	}
}

func TestMissingInputDoesNotTreatMarkdownQuoteAsTailAnchor(t *testing.T) {
	previous := strings.Join([]string{
		"stable conclusion alpha with enough entropy",
		"stable supporting beta with enough entropy",
		"> OLD_QUOTE_MUST_NOT_LEAK",
		"gpt-5.6-sol medium fast · ~/project",
	}, "\n")
	visible := strings.Join([]string{
		"stable conclusion alpha with enough entropy",
		"stable supporting beta with enough entropy",
		"> OLD_QUOTE_MUST_NOT_LEAK",
		"CURRENT_REPLY_ONLY",
		"gpt-5.6-terra high fast · ~/project",
	}, "\n")
	if got := PickNotifyContent(visible, previous, nil, "input missing from snapshot"); got != "" {
		t.Fatalf("a > quote that does not match the submitted input must not become another round anchor: %q", got)
	}
}

func TestAdaptiveTailAnchorRejectsReplyQuoteAfterOriginalAnchorWasEvicted(t *testing.T) {
	anchor := []string{
		"distinctive evicted boundary one alpha beta",
		"distinctive evicted boundary two gamma delta",
		"distinctive evicted boundary three epsilon zeta",
		"distinctive evicted boundary four eta theta",
		"distinctive evicted boundary five iota kappa",
	}
	previous := strings.Join(anchor, "\n")
	visible := strings.Join(append(append([]string{
		"• CURRENT_REPLY_STARTED_BEFORE_THE_QUOTE",
		"the assistant now quotes the previous answer verbatim:",
	}, anchor...), "• CURRENT_REPLY_CONTINUES_AFTER_THE_QUOTE"), "\n")
	if got := pickNotifyContentWithWindowPolicy(visible, previous, nil, "input echo missing from current snapshot", "", false); got != "" {
		t.Fatalf("renderer-reported scrollback eviction must disable text-tail matching: %q", got)
	}
}

func TestAdaptiveTailAnchorRejectsDisappearedRepeatedOccurrence(t *testing.T) {
	anchor := []string{
		"• 重复但足够长的上一轮结论。",
		"  重复但足够长的上一轮详情。",
	}
	previous := strings.Join(append(append([]string{}, anchor...), anchor...), "\n")
	visible := strings.Join(append(append([]string{}, anchor...), "• ambiguous new tail"), "\n")
	if got, ok := visibleTextAfterPreviousTailAnchor(visible, previous, "", 5); ok || got != "" {
		t.Fatalf("an older duplicate must not replace a vanished latest tail occurrence: ok=%v body=%q", ok, got)
	}
}

func TestAdaptiveTailAnchorRejectsLowEntropyTwoLineSuffix(t *testing.T) {
	previous := "done\ndone"
	visible := previous + "\nambiguous"
	if got, ok := visibleTextAfterPreviousTailAnchor(visible, previous, "", 5); ok || got != "" {
		t.Fatalf("two short generic lines are not a trustworthy tail anchor: ok=%v body=%q", ok, got)
	}
}

func TestAdaptiveTailAnchorRejectsLowEntropyThreeLineSuffix(t *testing.T) {
	previous := "done\nokay\nready"
	visible := previous + "\nambiguous"
	if got, ok := visibleTextAfterPreviousTailAnchor(visible, previous, "", 5); ok || got != "" {
		t.Fatalf("three short generic lines are not a trustworthy tail anchor: ok=%v body=%q", ok, got)
	}
}

func TestPickNotifyContentRejectsHistoryRevealedAbovePreviousSnapshot(t *testing.T) {
	previous := "stable previous line one\nstable previous line two"
	visible := "OLDER_HISTORY_MUST_NOT_LEAK\n" + previous
	if got := PickNotifyContent(visible, previous, nil, "missing current input"); got != "" {
		t.Fatalf("text revealed above the baseline is old viewport history, not a reply: %q", got)
	}
}

func TestPickNotifyContentRejectsHistoryInsertedBetweenHeaderAndFooter(t *testing.T) {
	previous := "Codex\ngpt-5.6-sol medium fast · ~/project"
	visible := "Codex\nOLD_HISTORY_MUST_NOT_LEAK\ngpt-5.6-sol medium fast · ~/project"
	if got := PickNotifyContent(visible, previous, nil, "missing current input"); got != "" {
		t.Fatalf("a weak shared header/footer cannot authorize a middle diff: %q", got)
	}
}

func TestPickNotifyContentRequiresRawExactAppendPrefix(t *testing.T) {
	previous := "stable line one\n    indented old line"
	visible := "stable line one\nindented old line\nOLD_HISTORY_MUST_NOT_LEAK"
	if got := PickNotifyContent(visible, previous, nil, "missing current input"); got != "" {
		t.Fatalf("whitespace-normalized history is not a strict append boundary: %q", got)
	}
}

func TestAdaptiveTailAnchorRejectsReflowedPreviousLines(t *testing.T) {
	previous := "• first distinctive previous line\n  second distinctive previous line"
	visible := "• first distinctive previous line second distinctive previous line\n• ambiguous new tail"
	if got, ok := visibleTextAfterPreviousTailAnchor(visible, previous, "", 5); ok || got != "" {
		t.Fatalf("reflowed physical lines must not be treated as an exact tail boundary: ok=%v body=%q", ok, got)
	}
}

func TestNotifyContentNeedsMoreSnapshotWhenPreviousTailAnchorHasNoNewContent(t *testing.T) {
	previous := strings.Join([]string{
		"anchor one",
		"anchor two",
		"anchor three",
	}, "\n")
	if !NotifyContentNeedsMoreSnapshot(previous, previous, nil, "next") {
		t.Fatalf("matching previous tail anchor with no new content should wait")
	}
}

func TestPickNotifyContentDoesNotUseRoundReplyWithoutVisibleSnapshot(t *testing.T) {
	got := PickNotifyContent("", "", []byte("current answer only"), "missing input")
	if got != "" {
		t.Fatalf("raw round reply should not be used without a visible snapshot: %q", got)
	}
	if !NotifyContentNeedsMoreSnapshot("", "", []byte("current answer only"), "missing input") {
		t.Fatalf("missing visible snapshot should wait")
	}
}

func TestPickNotifyContentWithUnknownInputNeverFallsBackToHistoricalPrompt(t *testing.T) {
	previous := "different baseline that cannot be matched"
	visible := strings.Join([]string{
		"› historical input",
		"• OLD_SENTINEL historical answer",
		"• ambiguous tail",
	}, "\n")
	if got := PickNotifyContent(visible, previous, nil, ""); got != "" {
		t.Fatalf("without a recorded input or trusted boundary, historical prompts must not authorize output: %q", got)
	}
}

func TestPickNotifyContentWithUnknownInputStillAllowsExactAppendOnlyDiff(t *testing.T) {
	previous := "stable previous line one\nstable previous line two"
	visible := previous + "\nnew content only"
	if got := PickNotifyContent(visible, previous, nil, ""); got != "new content only" {
		t.Fatalf("an exact append-only boundary remains safe without input text: %q", got)
	}
}

func TestPickNotifyContentKeepsGenericVisibleListFormatting(t *testing.T) {
	SetLarkNotifyMaxLines(4)
	t.Cleanup(func() { SetLarkNotifyMaxLines(defaultMaxLarkTextLines) })
	before := "menu command"
	visible := strings.Join([]string{
		"menu command",
		"Available options:",
		"1. Create session",
		"2. Attach session",
		"3. Quit",
	}, "\n")
	round := []byte("Available options:1.Create session2.Attach session3.Quit")
	got := PickNotifyContent(visible, before, round, "menu command")
	want := strings.Join([]string{
		"Available options:",
		"1. Create session",
		"2. Attach session",
		"3. Quit",
	}, "\n")
	if got != want {
		t.Fatalf("generic list should keep visible terminal formatting:\n%q\nwant:\n%q", got, want)
	}
}

func TestPickNotifyContentKeepsCodexModelMenusAsVisibleText(t *testing.T) {
	SetLarkNotifyMaxLines(5)
	t.Cleanup(func() { SetLarkNotifyMaxLines(defaultMaxLarkTextLines) })
	before := strings.Join([]string{
		"╭────────────────────────────╮",
		"│ >_ OpenAI Codex (v0.130.0) │",
		"│ model: gpt-5.5 medium fast │",
		"│ directory: ~/project       │",
		"╰────────────────────────────╯",
	}, "\n")
	modelVisible := before + "\n" + strings.Join([]string{
		"Select Model and Effort",
		"Access legacy models by running codex -m <model_name> or in your config.toml",
		"› 1. gpt-5.5 (current)   Frontier model for complex coding, research, and real-world work.",
		"  2. gpt-5.4             Strong model for everyday coding.",
		"Press enter to confirm or esc to go back",
	}, "\n")
	got := PickNotifyContent(modelVisible, before, []byte("Select Model and Effort1.gpt-5.52.gpt-5.4"), "/model")
	want := strings.Join([]string{
		"Select Model and Effort",
		"Access legacy models by running codex -m <model_name> or in your config.toml",
		"› 1. gpt-5.5 (current)   Frontier model for complex coding, research, and real-world work.",
		"  2. gpt-5.4             Strong model for everyday coding.",
		"Press enter to confirm or esc to go back",
	}, "\n")
	if got != want {
		t.Fatalf("model menu should come from visible text:\n%q\nwant:\n%q", got, want)
	}

	SetLarkNotifyMaxLines(6)
	reasoningVisible := before + "\n" + strings.Join([]string{
		"Select Reasoning Level for gpt-5.5",
		"1. Low                  Fast responses with lighter reasoning",
		"2. Medium (default)     Balances speed and reasoning depth for everyday tasks",
		"3. High                 Greater reasoning depth for complex problems",
		"› 4. Extra high (current)  Extra high reasoning depth for complex problems",
		"Press enter to confirm or esc to go back",
	}, "\n")
	got = PickNotifyContent(reasoningVisible, modelVisible, []byte("1Select Reasoning Level1.Low2.Medium3.High"), "1")
	want = strings.Join([]string{
		"Select Reasoning Level for gpt-5.5",
		"1. Low                  Fast responses with lighter reasoning",
		"2. Medium (default)     Balances speed and reasoning depth for everyday tasks",
		"3. High                 Greater reasoning depth for complex problems",
		"› 4. Extra high (current)  Extra high reasoning depth for complex problems",
		"Press enter to confirm or esc to go back",
	}, "\n")
	if got != want {
		t.Fatalf("reasoning menu should come from visible text:\n%q\nwant:\n%q", got, want)
	}
}

func TestNotifyContentNeedsMoreSnapshotForInputOnlyOrTransientOnly(t *testing.T) {
	const previous = "> current question"
	if !NotifyContentNeedsMoreSnapshot(previous, previous, nil, "current question") {
		t.Fatalf("input-only visible text should wait")
	}
	visible := strings.Join([]string{
		"> current question",
		"• Working (2s • esc to interrupt)",
		"gpt-5.5 medium · ~",
	}, "\n")
	if !NotifyContentNeedsMoreSnapshot(visible, previous, nil, "current question") {
		t.Fatalf("transient-only visible text should wait")
	}
	complete := visible + "\nanswer"
	if NotifyContentNeedsMoreSnapshot(complete, previous, nil, "current question") {
		t.Fatalf("completed visible text should be ready")
	}
}

func TestPickNotifyContentDropsCodexTUIStatusOnlySnapshot(t *testing.T) {
	visible := strings.Join([]string{
		"› 你好",
		"• Working (1s • esc to interrupt)",
		"1 background terminal running · /ps to view · /stop to close",
		"› Run /review on my current changes",
		"gpt-5.5 xhigh fast · ~/Iris_Workspace/减肥",
	}, "\n")
	if !NotifyContentNeedsMoreSnapshot(visible, "", nil, "你好") {
		t.Fatalf("TUI status-only snapshot should wait for real reply")
	}
}

func TestPickNotifyContentSanitizesEmail(t *testing.T) {
	got := pickAnchoredNotifyContent("contact me@example.com")
	if strings.Contains(got, "me@example.com") || !strings.Contains(got, "[email]") {
		t.Fatalf("email was not sanitized: %q", got)
	}
}

func TestPickNotifyContentKeepsRawOutputForCodexCCommand(t *testing.T) {
	if err := SetLarkNotifyDropLineRules([]LarkNotifyDropLineRule{{
		Kind: "block_head", Pattern: `^\s*[•⏺]\s+Ran\b.*`, Action: "drop_block",
	}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = SetLarkNotifyDropLineRules(nil) })

	visible := strings.Join([]string{
		"• Ran go test ./...",
		"tool output me@example.com",
		"gpt-5.6-sol medium fast · ~/project/iris",
	}, "\n")
	if got := PickNotifyContent(visible, "", nil, "/c"); got != visible {
		t.Fatalf("/c must bypass every notification filter:\n%q\nwant:\n%q", got, visible)
	}
}

func TestPickNotifyContentAppliesDropLinePatterns(t *testing.T) {
	if err := SetLarkNotifyDropLinePatterns([]string{`^noise:`, `secret`}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := SetLarkNotifyDropLinePatterns(nil); err != nil {
			t.Fatal(err)
		}
	})

	got := pickAnchoredNotifyContent(strings.Join([]string{
		"keep first",
		"noise: drop this",
		"keep second",
		"contains secret token",
	}, "\n"))
	want := "keep first\nkeep second"
	if got != want {
		t.Fatalf("drop line patterns were not applied:\n%q\nwant:\n%q", got, want)
	}
}

func TestPickNotifyContentAppliesBlockHeadDropRule(t *testing.T) {
	if err := SetLarkNotifyDropLineRules([]LarkNotifyDropLineRule{
		{Kind: "block_head", Pattern: `^• 已完成发布`, Action: "drop_block"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := SetLarkNotifyDropLineRules(nil); err != nil {
			t.Fatal(err)
		}
	})

	got := pickAnchoredNotifyContent(strings.Join([]string{
		"• 已完成发布。",
		"  发布结果：",
		"  - GitHub Release 已生成",
		"• 下一段保留",
	}, "\n"))
	if got != "• 下一段保留" {
		t.Fatalf("block should be dropped:\n%q", got)
	}
}

func TestPickNotifyContentDropsCodexToolBlocksAndFooterStatus(t *testing.T) {
	if err := SetLarkNotifyDropLineRules([]LarkNotifyDropLineRule{
		{
			Kind:    "block_head",
			Pattern: `^\s*[•⏺]\s+(?:Ran|Waited|Explored|Edited?|Read|Searched|Viewed)\b.*`,
			Action:  "drop_block",
		},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := SetLarkNotifyDropLineRules(nil); err != nil {
			t.Fatal(err)
		}
	})

	const previous = "› 修复问题"
	got := PickNotifyContent(strings.Join([]string{
		previous,
		"• Viewed Image",
		"└ conf_8081/data/uploads/example.png",
		"• Ran go test ./...",
		"wrapped command argument without indentation",
		"ok github.com/elevenlj/iris/internal/session",
		"• Waited for background terminal · go test ./...",
		"PASS",
		"• Explored",
		"  Read manager.go",
		"• 已完成修复并通过全部测试。",
		"gpt-5.6-terra high fast · ~/project/iris",
	}, "\n"), previous, nil, "修复问题")
	if got != "• 已完成修复并通过全部测试。" {
		t.Fatalf("Codex process output should be hidden from the final notification:\n%q", got)
	}
}

func TestPickNotifyContentDropsLeadingToolFragmentWithoutMarkerTitle(t *testing.T) {
	content := strings.Join([]string{
		"└ PID STARTED COMMAND 46748 Mon Aug 3 01:55:14 2026",
		"/Users/eleven/Library/Caches/go-build/example/cmd -p 8001",
		"… +371 lines (ctrl + t to view transcript)",
		"source=browser:buffer;continuity_version=2;reason=stale_wrong_source_or_responder",
		"• 从日志看，状态已经变成 waiting。",
		"  真正阻断推送的是完成后的候选内容被判定为 stale_visible_snapshot。",
	}, "\n")
	got := cleanLarkNotifyContentForAgent(content, SessionModeAgent, "codex")
	want := strings.Join([]string{
		"• 从日志看，状态已经变成 waiting。",
		"  真正阻断推送的是完成后的候选内容被判定为 stale_visible_snapshot。",
	}, "\n")
	if got != want {
		t.Fatalf("untitled leading tool fragment must be removed:\n%q\nwant:\n%q", got, want)
	}
}

func TestUntitledBulletRuleDoesNotApplyToClaude(t *testing.T) {
	want := "Claude plain opening\n• Claude may use a different marker convention"
	if got := cleanLarkNotifyContentForAgent(want, SessionModeAgent, "claude"); got != want {
		t.Fatalf("Codex bullet rule must not alter Claude output: %q", got)
	}
}

func TestUntitledCodexRuleKeepsOrdinaryTextBeforeWorkingMarker(t *testing.T) {
	want := "普通正文必须保留\n• Working (8s • esc to interrupt)"
	if got := cleanLarkNotifyContentForAgent(want, SessionModeAgent, "codex"); got != want {
		t.Fatalf("ordinary Codex text before a marker must be preserved: %q", got)
	}
}

func TestPickNotifyContentKeepsPlainOutputWithoutCodexMarker(t *testing.T) {
	want := "plain terminal output\nsecond line"
	if got := pickAnchoredNotifyContent(want); got != want {
		t.Fatalf("plain terminal output without Codex marker must be preserved: %q", got)
	}
}

func TestPickNotifyContentRawCommandKeepsUntitledLeadingFragment(t *testing.T) {
	visible := strings.Join([]string{
		"└ raw tool output",
		"… +371 lines (ctrl + t to view transcript)",
		"• normal marked block",
	}, "\n")
	if got := PickNotifyContent(visible, "", nil, "/c"); got != visible {
		t.Fatalf("/c must keep the complete raw output:\n%q\nwant:\n%q", got, visible)
	}
}

func TestToolMarkerBlockEndsOnlyAtNextMarker(t *testing.T) {
	if err := SetLarkNotifyDropLineRules([]LarkNotifyDropLineRule{{
		Kind: "block_head", Pattern: `^\s*[•⏺]\s+Ran\b.*`, Action: "drop_block",
	}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = SetLarkNotifyDropLineRules(nil) })

	got := pickAnchoredNotifyContent(strings.Join([]string{
		"• 准备查询天气。",
		"• Ran curl --max-time 15",
		"https://wttr.in/Chengdu?format=j1",
		"command output without indentation",
		"• 成都目前 31°C，多云。",
		"- 体感 36°C",
	}, "\n"))
	want := "• 准备查询天气。\n• 成都目前 31°C，多云。\n- 体感 36°C"
	if got != want {
		t.Fatalf("tool marker block should be dropped through the next marker:\n%q\nwant:\n%q", got, want)
	}
}

func TestPickNotifyContentAppliesBlockHeadKeepRule(t *testing.T) {
	if err := SetLarkNotifyDropLineRules([]LarkNotifyDropLineRule{
		{Kind: "block_head", Pattern: `^• 已完成发布`, Action: "keep_head"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := SetLarkNotifyDropLineRules(nil); err != nil {
			t.Fatal(err)
		}
	})

	got := pickAnchoredNotifyContent(strings.Join([]string{
		"• 已完成发布。",
		"  发布结果：",
		"  - GitHub Release 已生成",
		"• 下一段保留",
	}, "\n"))
	want := "• 已完成发布。\n• 下一段保留"
	if got != want {
		t.Fatalf("block body should be dropped:\n%q\nwant:\n%q", got, want)
	}
}

func TestPickNotifyContentDropsCodexStartupTipAndMCPErrors(t *testing.T) {
	if err := SetLarkNotifyDropLineRules([]LarkNotifyDropLineRule{
		{
			Kind:    "block_head",
			Pattern: `^\s*(?:Tip:|⚠ MCP (?:client.*failed to start|startup incomplete)).*`,
			Action:  "drop_block",
		},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := SetLarkNotifyDropLineRules(nil); err != nil {
			t.Fatal(err)
		}
	})

	got := pickAnchoredNotifyContent(strings.Join([]string{
		"Tip: Try the Codex App. Run 'codex app' or visit https://chatgpt.com/codex",
		"  This promotional message is terminal-wrapped.",
		"⚠ MCP client for `codex_apps` failed to start: MCP startup failed",
		"  [rmcp::transport::worker::WorkerTransport] error: Transport channel closed",
		"  send initialize request",
		"⚠ MCP startup incomplete (failed: codex_apps)",
		"真正需要推送的结果",
	}, "\n"))
	if got != "真正需要推送的结果" {
		t.Fatalf("Codex startup noise should be dropped:\n%q", got)
	}
}

func TestPickNotifyContentAppliesLineGroupRule(t *testing.T) {
	if err := SetLarkNotifyDropLineRules([]LarkNotifyDropLineRule{
		{Kind: "line_group", Pattern: `(token=)([^ ]+)`, Groups: []int{2}},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := SetLarkNotifyDropLineRules(nil); err != nil {
			t.Fatal(err)
		}
	})

	got := pickAnchoredNotifyContent("deploy token=abc123 done")
	want := "deploy token= done"
	if got != want {
		t.Fatalf("capture group should be removed:\n%q\nwant:\n%q", got, want)
	}
}

func TestSetLarkNotifyDropLineRulesRejectsMissingGroup(t *testing.T) {
	err := SetLarkNotifyDropLineRules([]LarkNotifyDropLineRule{
		{Kind: "line_group", Pattern: `(token=)([^ ]+)`, Groups: []int{3}},
	})
	if err == nil {
		t.Fatal("expected missing capture group to be rejected")
	}
}

func TestLarkTerminalPlainTextMergesWrappedLinesWhenEnabled(t *testing.T) {
	SetLarkNotifyMergeWrappedLines(true)
	t.Cleanup(func() { SetLarkNotifyMergeWrappedLines(false) })

	got := larkTerminalPlainText(strings.Join([]string{
		"这是一个因为终端宽度被折断的长句",
		"下一段仍然属于同一句话。",
		"新的一句保留换行",
		"",
		"1. 列表保留换行",
	}, "\n"))
	want := strings.Join([]string{
		"这是一个因为终端宽度被折断的长句下一段仍然属于同一句话。",
		"新的一句保留换行",
		"",
		"1. 列表保留换行",
	}, "\n")
	if got != want {
		t.Fatalf("wrapped lines were not merged as expected:\n%q\nwant:\n%q", got, want)
	}
}

func TestLarkTerminalPlainTextKeepsWrappedLinesByDefault(t *testing.T) {
	SetLarkNotifyMergeWrappedLines(false)
	got := larkTerminalPlainText("第一行\n第二行")
	if got != "第一行\n第二行" {
		t.Fatalf("wrapped line merge should be disabled by default: %q", got)
	}
}

func TestLarkTerminalPlainTextKeepsNumberedDiffLinesWhenMergingWraps(t *testing.T) {
	SetLarkNotifyMergeWrappedLines(true)
	t.Cleanup(func() { SetLarkNotifyMergeWrappedLines(false) })

	got := larkTerminalPlainText(strings.Join([]string{
		"• Added scripts/build.sh (+3 -0)",
		"  1 +#!/bin/zsh",
		"  2 +set -euo pipefail",
		"  3 +ROOT=/tmp/project",
	}, "\n"))
	want := strings.Join([]string{
		"• Added scripts/build.sh (+3 -0)",
		"  1 +#!/bin/zsh",
		"  2 +set -euo pipefail",
		"  3 +ROOT=/tmp/project",
	}, "\n")
	if got != want {
		t.Fatalf("numbered diff lines should keep real line breaks:\n%q\nwant:\n%q", got, want)
	}
}

func TestTruncateForLarkKeepsTailLinesWithoutPrefix(t *testing.T) {
	SetLarkNotifyMaxLines(3)
	t.Cleanup(func() { SetLarkNotifyMaxLines(defaultMaxLarkTextLines) })
	got := truncateForLark("one\ntwo\nthree\nfour\nfive")
	want := "three\nfour\nfive"
	if got != want {
		t.Fatalf("truncateForLark() = %q, want %q", got, want)
	}
}

func TestTruncateForLarkKeepsTailForLongText(t *testing.T) {
	SetLarkNotifyMaxLines(defaultMaxLarkTextLines)
	t.Cleanup(func() { SetLarkNotifyMaxLines(defaultMaxLarkTextLines) })
	lines := make([]string, 0, defaultMaxLarkTextLines+20)
	for i := 0; i < defaultMaxLarkTextLines+20; i++ {
		lines = append(lines, "line-"+strconv.Itoa(i))
	}
	got := truncateForLark(strings.Join(lines, "\n"))
	if strings.Contains(got, "line-0\n") {
		t.Fatalf("expected head lines to be dropped")
	}
	if strings.Contains(got, larkTruncatedPrefix) {
		t.Fatalf("line truncation should not add a prefix")
	}
	if !strings.Contains(got, "line-319") {
		t.Fatalf("expected tail line to be kept")
	}
}

func TestTruncateForLarkKeepsTailForLongRunes(t *testing.T) {
	SetLarkNotifyMaxLines(defaultMaxLarkTextLines)
	t.Cleanup(func() { SetLarkNotifyMaxLines(defaultMaxLarkTextLines) })
	got := truncateForLark("开头不能保留" + strings.Repeat("头", maxLarkTextRunes) + "最后这一段必须保留")
	if !strings.HasPrefix(got, larkTruncatedPrefix) {
		t.Fatalf("expected rune truncation prefix")
	}
	if strings.Contains(got, "开头不能保留") {
		t.Fatalf("expected original head to be dropped")
	}
	if !strings.HasSuffix(got, "最后这一段必须保留") {
		t.Fatalf("expected final content to be kept, got %q", got)
	}
	if len([]rune(got)) > maxLarkTextRunes {
		t.Fatalf("truncated text has %d runes, want <= %d", len([]rune(got)), maxLarkTextRunes)
	}
}

func TestStripTerminalControls(t *testing.T) {
	got := StripTerminalControls([]byte("\x1b[31mhello\x1b[0m\r\n"))
	if strings.TrimSpace(got) != "hello" {
		t.Fatalf("unexpected stripped output: %q", got)
	}
}
