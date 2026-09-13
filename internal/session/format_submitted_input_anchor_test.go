package session

import (
	"strings"
	"testing"
)

func TestSubmittedInputAnchorIgnoresRejectedRendererIdentity(t *testing.T) {
	const input = "成都今天天气怎么样"
	visible := strings.Join([]string{
		"› " + input,
		"• 只保留本轮回答",
	}, "\n")

	got := pickNotifyContentWithWindowAnchorPolicy(visible, "旧快照", nil, input, "", notifyTextAnchorPolicy{})
	if got != "• 只保留本轮回答" {
		t.Fatalf("an explicit input prompt must not be vetoed by renderer identity: %q", got)
	}
}

func TestSubmittedInputAnchorRestoresSoftWrap(t *testing.T) {
	const input = "这是一段很长的输入内容需要跨越多行但仍然必须准确识别为本轮输入锚点"
	runes := []rune(input)
	visible := strings.Join([]string{
		"› " + string(runes[:12]),
		string(runes[12:27]),
		string(runes[27:]),
		"• 软换行后的本轮回答",
	}, "\n")

	got := pickNotifyContentWithWindowAnchorPolicy(visible, "旧快照", nil, input, "", notifyTextAnchorPolicy{})
	if got != "• 软换行后的本轮回答" {
		t.Fatalf("soft-wrapped input must be restored before matching: %q", got)
	}
}

func TestSubmittedInputAnchorFallsBackToFirstThirtyRunes(t *testing.T) {
	input := "这是超过三十个字符的用户输入用于验证当前终端只保留输入前缀时仍能找到正确锚点并切出本轮回答"
	prefix := string([]rune(input)[:maxInputAnchorRunes])
	visible := strings.Join([]string{
		"› " + prefix,
		"• 前三十个字符后的本轮回答",
	}, "\n")

	got := pickNotifyContentWithWindowAnchorPolicy(visible, "旧快照", nil, input, "", notifyTextAnchorPolicy{})
	if got != "• 前三十个字符后的本轮回答" {
		t.Fatalf("the first 30 runes must be the final input-anchor fallback: %q", got)
	}
}

func TestSubmittedInputAnchorUsesNewestDuplicate(t *testing.T) {
	const input = "你好"
	visible := strings.Join([]string{
		"› " + input,
		"• 历史回答",
		"› " + input,
		"• 最新回答",
	}, "\n")

	got := pickNotifyContentWithWindowAnchorPolicy(visible, "旧快照", nil, input, "", notifyTextAnchorPolicy{})
	if got != "• 最新回答" {
		t.Fatalf("duplicate input anchors must select the newest occurrence: %q", got)
	}
}

func TestManualRefreshUsesSubmittedInputAnchorAndKeepsWorking(t *testing.T) {
	const input = "帮我分析这个问题"
	visible := strings.Join([]string{
		"› " + input,
		"• Working (8s • esc to interrupt)",
		"gpt-5.6-sol high fast · ~/project/iris",
	}, "\n")

	got := pickManualRefreshNotifyContentWithWindowAnchorPolicy(visible, "旧快照", nil, input, "", notifyTextAnchorPolicy{})
	want := "› " + input + "\n• Working (8s • esc to interrupt)"
	if got != want {
		t.Fatalf("manual refresh must use the same input anchor and preserve Working:\n%q\nwant:\n%q", got, want)
	}
}

func TestManualRefreshTailFallbackDropsOnlyCodexFooter(t *testing.T) {
	visible := strings.Join([]string{
		"普通正文必须保留",
		"• Working (8s • esc to interrupt)",
		"gpt-5.6-sol high fast · ~/project/iris",
	}, "\n")
	want := strings.Join([]string{
		"普通正文必须保留",
		"• Working (8s • esc to interrupt)",
	}, "\n")
	if got := pickLarkManualRefreshFallbackTailContent(visible); got != want {
		t.Fatalf("manual fallback must keep正文/Working and remove only the Codex footer:\n%q\nwant:\n%q", got, want)
	}
}

func TestManualRefreshUsesLatestQuestionInsteadOfUnfinishedWindow(t *testing.T) {
	const oldInput = "前一个还没结束的问题"
	const input = "/Users/bytedance/.iris/data/uploads/sess-12/lark/new_image.png\n【引用消息】旧内容\n【用户当前请求】\n只看最后一个问题"
	visible := "› " + oldInput + "\n历史回答\n› " + input + "\n• 本轮进展\n• Working (8s • esc to interrupt)"
	got := pickManualRefreshNotifyContentWithWindowAnchorPolicy(visible, "", nil, input, oldInput, notifyTextAnchorPolicy{})
	if got != "› "+input+"\n• 本轮进展\n• Working (8s • esc to interrupt)" {
		t.Fatalf("refresh must keep the latest question and exclude older rounds: %q", got)
	}
}

func TestSubmittedInputAnchorRejectsDifferentAttachmentWithSharedPathPrefix(t *testing.T) {
	const prefix = "/Users/bytedance/.iris/data/uploads/sess-12/lark/"
	visible := "› " + prefix + "old_image.png 旧问题\n历史回答"
	input := prefix + "new_image.png 新问题"
	if got := pickManualRefreshNotifyContentWithWindowAnchorPolicy(visible, "", nil, input, "", notifyTextAnchorPolicy{}); got != "" {
		t.Fatalf("a shared upload directory is not the current question: %q", got)
	}
}
