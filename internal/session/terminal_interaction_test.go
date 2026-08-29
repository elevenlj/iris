package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
)

func TestDetectCodexModelInteraction(t *testing.T) {
	text := strings.Join([]string{
		"Select Model and Effort",
		"Access legacy models by running codex -m <model_name> or in your config.toml",
		"› 1. gpt-5.5 (current)   Frontier model for complex coding, research, and real-world work.",
		"  2. gpt-5.4             Strong model for everyday coding.",
		"  3. gpt-5.4-mini        Faster model for smaller tasks.",
		"Press enter to confirm or esc to go back",
	}, "\n")

	got := DetectCodexTerminalInteraction(text, "sess-1", "/model", 7, 11)
	if got == nil {
		t.Fatal("expected Codex model interaction")
	}
	if got.Kind != TerminalInteractionCodexModel || got.Title != "Select Model and Effort" {
		t.Fatalf("unexpected interaction: %#v", got)
	}
	if got.NotifyVersion != 7 || got.SnapshotVersion != 11 || !strings.HasPrefix(got.ID, "ti_") {
		t.Fatalf("unexpected interaction identity: %#v", got)
	}
	if len(got.Options) != 3 {
		t.Fatalf("options = %#v", got.Options)
	}
	if got.Options[0].Input != "1" || got.Options[0].Label != "gpt-5.5" || !got.Options[0].Current {
		t.Fatalf("current option = %#v", got.Options[0])
	}
	if got.Options[1].ID == got.Options[1].Input || got.Options[1].Description != "Strong model for everyday coding." {
		t.Fatalf("option should use an opaque id and keep its description: %#v", got.Options[1])
	}
}

func TestDetectCodexReasoningInteraction(t *testing.T) {
	text := strings.Join([]string{
		"Select Reasoning Level for gpt-5.5",
		"1. Low                  Fast responses with lighter reasoning",
		"2. Medium (default)     Balances speed and reasoning depth for everyday tasks",
		"3. High                 Greater reasoning depth for complex problems",
		"› 4. Extra high (current)  Extra high reasoning depth for complex problems",
		"Press enter to confirm or esc to go back",
	}, "\n")

	got := DetectCodexTerminalInteraction(text, "sess-1", "1", 9, 13)
	if got == nil || got.Kind != TerminalInteractionCodexReasoning {
		t.Fatalf("expected Codex reasoning interaction, got %#v", got)
	}
	if len(got.Options) != 4 || !got.Options[1].Default || !got.Options[3].Current {
		t.Fatalf("reasoning markers were not parsed: %#v", got.Options)
	}
	if got.Options[3].Label != "Extra high" || got.Options[3].Input != "4" {
		t.Fatalf("reasoning option = %#v", got.Options[3])
	}
}

func TestDetectCodexReasoningInteractionWithoutFooter(t *testing.T) {
	text := strings.Join([]string{
		"Select Reasoning Level for gpt-5.6-terra",
		"1. Low                  Fast responses with lighter reasoning",
		"› 2. Medium (default)   Balances speed and reasoning depth for everyday tasks",
		"3. High                 Greater reasoning depth for complex problems",
		"4. Extra high           Extra high reasoning depth for complex problems",
		"5. Max                  Maximum reasoning depth for the hardest problems",
		"6. Ultra                Maximum reasoning with automatic task delegation",
	}, "\n")

	got := DetectCodexTerminalInteraction(text, "sess-1", "2", 10, 14)
	if got == nil || got.Kind != TerminalInteractionCodexReasoning {
		t.Fatalf("current Codex reasoning menu without a footer should be interactive, got %#v", got)
	}
	if len(got.Options) != 6 || got.Options[1].Input != "2" || !got.Options[1].Default {
		t.Fatalf("reasoning options = %#v", got.Options)
	}
}

func TestDetectCodexResumeInteraction(t *testing.T) {
	text := strings.Join([]string{
		"Resume a previous session",
		"Type to search                    Filter: [Cwd] All    Sort: [Updated] Created",
		"❯ now         修复飞书按钮布局",
		"  12m         排查 Reasoning 菜单重复推送",
		"  2h          实现 Codex Hook 状态流转",
		"──────────────────────────────────────── 1 / 3 · 33% ─",
		"enter resume   esc exit   ctrl+c exit   tab focus   sort/filter   ←/→ change option   ctrl+o",
		"comfortable view   ctrl+t transcript   ctrl+e expand   ↑/↓ browse",
	}, "\n")

	got := DetectCodexTerminalInteraction(text, "sess-1", "/resume", 12, 30)
	if got == nil || got.Kind != TerminalInteractionCodexResume {
		t.Fatalf("expected Codex resume interaction, got %#v", got)
	}
	if len(got.Options) != 3 || !got.Options[0].Current {
		t.Fatalf("resume options = %#v", got.Options)
	}
	if got.Options[0].Input != "" || got.Options[1].Input != "\x1b[B" || got.Options[2].Input != "\x1b[B\x1b[B" || !got.Options[1].SubmitWithEnter {
		t.Fatalf("resume navigation inputs = %#v", got.Options)
	}
	if got.Options[1].Label != "排查 Reasoning 菜单重复推送" || got.Options[1].Description != "12m" {
		t.Fatalf("resume option text = %#v", got.Options[1])
	}
	if label := larkTerminalInteractionOptionLabel(got.Options[1]); label != "排查 Reasoning 菜单重复推送" {
		t.Fatalf("resume dropdown must hide terminal navigation bytes, got %q", label)
	}
	if stale := DetectCodexTerminalInteraction(text, "sess-1", "普通问题", 12, 30); stale != nil {
		t.Fatalf("historical resume menu should require a /resume input: %#v", stale)
	}
}

func TestMenusRequireNovelFingerprintWhenTextAnchorsAreDisabled(t *testing.T) {
	model := strings.Join([]string{
		"Select Model and Effort",
		"Access legacy models by running codex -m <model_name>",
		"› 1. gpt-5.5 (current)   Frontier model",
		"  2. gpt-5.4             Strong model",
		"Press enter to confirm or esc to go back",
	}, "\n")
	resume := strings.Join([]string{
		"Resume a previous session",
		"Type to search                    Filter: [Cwd] All",
		"❯ now         当前会话",
		"  12m         历史会话",
		"enter resume   esc exit",
	}, "\n")

	for _, tt := range []struct {
		name  string
		menu  string
		input string
	}{
		{name: "model", menu: model, input: "/model"},
		{name: "resume", menu: resume, input: "/resume"},
	} {
		t.Run(tt.name+" stale tail", func(t *testing.T) {
			if got := pickNotifyContentWithWindowPolicy(tt.menu, tt.menu, nil, tt.input, "", false); got != "" {
				t.Fatalf("an unchanged historical %s menu must not render without anchor continuity: %q", tt.name, got)
			}
		})
		t.Run(tt.name+" newly appeared", func(t *testing.T) {
			if got := pickNotifyContentWithWindowPolicy(tt.menu, "ordinary baseline", nil, tt.input, "", false); !strings.Contains(got, strings.Split(tt.menu, "\n")[0]) {
				t.Fatalf("a newly appeared %s menu must still render immediately: %q", tt.name, got)
			}
		})
	}
}

func TestCodexInteractionStatusDoesNotHideOrdinaryBrowserText(t *testing.T) {
	for _, line := range []string{
		"The browser reconnect preserved the current reply.",
		"请检查另一台电脑的 browser 快照。",
	} {
		if isCodexInteractionStatusLine(line) {
			t.Fatalf("ordinary reply text was classified as TUI chrome: %q", line)
		}
	}
	if !isCodexInteractionStatusLine("comfortable view   ctrl+t transcript   ctrl+e expand   ↑/↓ browse") {
		t.Fatal("the actual Codex browse footer should remain classified as TUI chrome")
	}
}

func TestDetectCodexResumeInteractionWithOneSession(t *testing.T) {
	text := strings.Join([]string{
		"Resume a previous session",
		"Type to search                    Filter: [Cwd] All    Sort: [Updated] Created",
		"❯ 57s ago     修复飞书按钮布局",
		"enter resume   esc exit   ctrl+c exit   tab focus   sort/filter   ←/→ change option   ctrl+o",
		"comfortable view   ctrl+t transcript   ctrl+e expand   ↑/↓ browse",
	}, "\n")
	got := DetectCodexTerminalInteraction(text, "sess-1", "/resume", 13, 31)
	if got == nil || got.Kind != TerminalInteractionCodexResume || len(got.Options) != 1 {
		t.Fatalf("one available resume session must remain selectable, got %#v", got)
	}
	if element := larkTerminalInteractionElement("sess-1", got); element == nil {
		t.Fatal("one available resume session should render a select control")
	}
}

func TestDetectCodexReasoningRejectsHistoricalMenuAfterTurnCompleted(t *testing.T) {
	text := strings.Join([]string{
		"Select Reasoning Level for gpt-5.6-sol",
		"1. Low",
		"› 2. Medium (default)",
		"3. High",
		"Press enter to confirm or esc to go back",
		"• Ran curl https://wttr.in/Chengdu",
		"成都现在约 31°C，多云。",
		"› Improve documentation in @filename",
		"gpt-5.6-sol medium fast · ~/project/iris",
	}, "\n")

	if got := DetectCodexTerminalInteraction(text, "sess-1", "成都天气如何", 10, 20); got != nil {
		t.Fatalf("historical reasoning menu must not remain interactive after normal output: %#v", got)
	}
}

func TestDetectCodexReasoningWithoutFooterRejectsNewPromptAfterMenu(t *testing.T) {
	text := strings.Join([]string{
		"Select Reasoning Level for gpt-5.6-sol",
		"1. Low",
		"› 2. Medium (default)",
		"3. High",
		"› Improve documentation in @filename",
		"gpt-5.6-sol medium fast · ~/project/iris",
	}, "\n")

	if got := DetectCodexTerminalInteraction(text, "sess-1", "2", 10, 20); got != nil {
		t.Fatalf("reasoning menu without footer must end at the active terminal tail: %#v", got)
	}
}

func TestCodexReasoningMenuCannotAuthorizeAnotherReasoningMenu(t *testing.T) {
	previous := strings.Join([]string{
		"Select Reasoning Level for gpt-5.6-sol",
		"1. Low",
		"› 2. Medium (default)",
		"3. High",
		"Press enter to confirm or esc to go back",
	}, "\n")
	current := strings.Join([]string{
		"Select Reasoning Level for gpt-5.6-sol",
		"1. Low",
		"2. Medium (default)",
		"› 3. High (current)",
		"Press enter to confirm or esc to go back",
	}, "\n")

	if hasCodexModelInteractionContext(previous) {
		t.Fatal("a previous Reasoning menu must not be treated as Model interaction context")
	}
	if got := codexTerminalInteractionVisibleBlock(current, previous, "3"); got != "" {
		t.Fatalf("a Reasoning menu must not authorize another Reasoning selector, got %q", got)
	}
	if !codexTerminalInteractionContextRejected(current, previous, "3") {
		t.Fatal("a repeated Reasoning menu should be rejected instead of falling through to generic notification diffing")
	}
}

func TestCodexModelMenuAuthorizesReasoningMenu(t *testing.T) {
	previous := strings.Join([]string{
		"Select Model and Effort",
		"› 1. gpt-5.6-sol (current)",
		"2. gpt-5.6-terra",
		"Press enter to confirm or esc to go back",
	}, "\n")
	current := strings.Join([]string{
		"Select Reasoning Level for gpt-5.6-sol",
		"1. Low",
		"› 2. Medium (default)",
		"3. High",
		"Press enter to confirm or esc to go back",
	}, "\n")

	if !hasCodexModelInteractionContext(previous) {
		t.Fatal("a previous Model menu should authorize its following Reasoning menu")
	}
	if got := codexTerminalInteractionVisibleBlock(current, previous, "1"); got == "" {
		t.Fatal("the Reasoning menu following a Model menu should remain visible")
	}
	if codexTerminalInteractionContextRejected(current, previous, "1") {
		t.Fatal("the Reasoning menu following a Model menu should not be rejected")
	}
}

func TestDetectCodexInteractionRejectsOrdinaryNumberedReply(t *testing.T) {
	ordinary := strings.Join([]string{
		"建议按以下步骤处理：",
		"1. 更新依赖",
		"2. 运行测试",
		"3. 发布版本",
	}, "\n")
	if got := DetectCodexTerminalInteraction(ordinary, "sess-1", "给我方案", 1, 1); got != nil {
		t.Fatalf("ordinary numbered reply should not become an interaction: %#v", got)
	}

	modelWithoutTrigger := strings.Join([]string{
		"Select Model and Effort",
		"1. gpt-5.5",
		"2. gpt-5.4",
		"Press enter to confirm or esc to go back",
	}, "\n")
	if got := DetectCodexTerminalInteraction(modelWithoutTrigger, "sess-1", "explain this output", 1, 1); got != nil {
		t.Fatalf("quoted model menu should require a /model input: %#v", got)
	}
}

func TestDetectCodexInteractionRejectsPartialOrNonSequentialMenu(t *testing.T) {
	partial := "Select Model and Effort\n1. gpt-5.5\n2. gpt-5.4"
	if got := DetectCodexTerminalInteraction(partial, "sess-1", "/model", 1, 1); got != nil {
		t.Fatalf("partial menu should not be interactive: %#v", got)
	}

	nonSequential := "Select Model and Effort\n1. gpt-5.5\n3. gpt-5.4\nPress enter to confirm or esc to go back"
	if got := DetectCodexTerminalInteraction(nonSequential, "sess-1", "/model", 1, 1); got != nil {
		t.Fatalf("non-sequential menu should not be interactive: %#v", got)
	}
}

func TestCodexInteractionIDIsStableAcrossSnapshotRefreshes(t *testing.T) {
	text := "Select Model and Effort\n1. gpt-5.5\n2. gpt-5.4\nPress enter to confirm or esc to go back"
	first := DetectCodexTerminalInteraction(text, "sess-1", "/model", 3, 10)
	second := DetectCodexTerminalInteraction(text, "sess-1", "/model", 3, 11)
	if first == nil || second == nil || first.ID != second.ID {
		t.Fatalf("snapshot-only refresh should preserve interaction id: first=%#v second=%#v", first, second)
	}
}

func TestRuntimeConsumesTerminalInteractionOnce(t *testing.T) {
	rt := &RuntimeSession{
		session:       Session{ID: "sess-1", Status: StatusWaiting, Live: true},
		notifyVersion: 4,
		pendingTerminalInteraction: &TerminalInteraction{
			ID:            "ti_1",
			NotifyVersion: 4,
			MessageID:     "om_1",
			Options:       []TerminalInteractionOption{{ID: "opt_1", Label: "gpt-5.5", Input: "1"}},
		},
		lastNotifiedMessageID: "om_1",
	}
	got, err := rt.consumeTerminalInteraction("ti_1", "opt_1", "om_1")
	if err != nil || got.Input != "1" {
		t.Fatalf("consume result = %#v, err=%v", got, err)
	}
	if _, err := rt.consumeTerminalInteraction("ti_1", "opt_1", "om_1"); !errors.Is(err, errTerminalInteractionExpired) {
		t.Fatalf("second consume should be rejected, got %v", err)
	}
}

func TestLarkNotificationCardRendersTerminalSelect(t *testing.T) {
	interaction := &TerminalInteraction{
		ID:    "ti_model_1",
		Kind:  TerminalInteractionCodexModel,
		Title: "Select Model and Effort",
		Options: []TerminalInteractionOption{
			{ID: "opt_1", Input: "1", Label: "gpt-5.5", Current: true},
			{ID: "opt_2", Input: "2", Label: "gpt-5.4"},
		},
	}
	content, err := larkNotificationCardContent(WaitingNotification{
		SessionID:            "sess-1",
		Name:                 "Codex",
		Content:              "Select Model and Effort\n1. gpt-5.5\n2. gpt-5.4",
		Interaction:          interaction,
		DeveloperModeEnabled: true,
	}, "ou_1", false)
	if err != nil {
		t.Fatal(err)
	}
	var card struct {
		Body struct {
			Elements []map[string]any `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(content), &card); err != nil {
		t.Fatal(err)
	}
	var selectElement map[string]any
	for _, element := range card.Body.Elements {
		if element["tag"] == "select_static" {
			selectElement = element
			break
		}
	}
	if selectElement == nil {
		t.Fatalf("card should contain select_static: %s", content)
	}
	if got := selectElement["initial_option"]; got != "opt_1" {
		t.Fatalf("current option should be preselected, got %#v from %#v", got, selectElement)
	}
	placeholder := selectElement["placeholder"].(map[string]any)["content"]
	if placeholder != "请选择模型（当前：gpt-5.5）" {
		t.Fatalf("placeholder = %#v", placeholder)
	}
	options := selectElement["options"].([]any)
	if len(options) != 2 || options[1].(map[string]any)["value"] != "opt_2" {
		t.Fatalf("select options = %#v", options)
	}
	behaviors := selectElement["behaviors"].([]any)
	value := behaviors[0].(map[string]any)["value"].(map[string]any)
	if value["iris_action"] != "terminal_select" || value["interaction_id"] != "ti_model_1" || value["session_id"] != "sess-1" {
		t.Fatalf("select callback value = %#v", value)
	}
	if strings.Contains(content, `"value":"2"`) {
		t.Fatalf("card must not expose the raw terminal input as an option value: %s", content)
	}
	if strings.Contains(content, "Select Model and Effort") || strings.Contains(content, "1. gpt-5.5\n2. gpt-5.4") {
		t.Fatalf("selector card should hide the raw terminal menu: %s", content)
	}
}

func TestLarkNotificationCardRendersResumeHeading(t *testing.T) {
	interaction := &TerminalInteraction{
		ID:    "ti_resume_1",
		Kind:  TerminalInteractionCodexResume,
		Title: "Resume a previous session",
		Options: []TerminalInteractionOption{
			{ID: "opt_1", Label: "修复飞书按钮布局", Input: "", SubmitWithEnter: true, Current: true},
		},
	}
	content, err := larkNotificationCardContent(WaitingNotification{
		SessionID: "sess-1", Name: "Codex", Content: "Resume a previous session", Interaction: interaction, DeveloperModeEnabled: true,
	}, "ou_1", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "选择要恢复的会话") || !strings.Contains(content, `"tag":"select_static"`) {
		t.Fatalf("resume card should show a restore-session heading and selector: %s", content)
	}
}

func TestLarkNotificationCardRendersStartupFallbackAsOrdinaryCard(t *testing.T) {
	content, err := larkNotificationCardContent(WaitingNotification{
		SessionID: "sess-1", Name: "Codex", Content: "Update available!\n1. Update now\n2. Skip", DeveloperModeEnabled: true,
	}, "ou_1", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, `"content":"Codex"`) || !strings.Contains(content, "Update available!") {
		t.Fatalf("startup fallback should use an ordinary card title and preserve the blocker: %s", content)
	}
	if strings.Contains(content, "启动等待") || strings.Contains(content, `"iris_action":"startup_shortcut"`) || strings.Contains(content, "上一项") || strings.Contains(content, "下一项") {
		t.Fatalf("startup fallback must not add dedicated status or controls: %s", content)
	}
}

func TestLarkNotificationCardRendersDedicatedStartupInputForm(t *testing.T) {
	content, err := larkNotificationCardContent(WaitingNotification{
		SessionID:           "sess-startup",
		Name:                "Iris",
		Content:             "Choose working directory\n1. Session\n2. Current",
		Startup:             true,
		StartupInputEnabled: true,
	}, "ou_1", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"content":"Iris（启动中）"`, `"tag":"form"`, `"tag":"input"`, `"name":"iris_startup_input"`, `"form_action_type":"submit"`, `"iris_action":"startup_submit"`, `"content":"提交"`, `"iris_action":"restart_agent"`, `"content":"重启 Agent"`} {
		if !strings.Contains(content, expected) {
			t.Fatalf("startup card missing %s: %s", expected, content)
		}
	}

	completed, err := larkNotificationCardContent(WaitingNotification{
		SessionID:       "sess-startup",
		Name:            "Iris",
		Content:         StartupCompletePlaceholder,
		Startup:         true,
		StartupComplete: true,
	}, "ou_1", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(completed, `"content":"Iris（启动完成）"`) || strings.Contains(completed, `"iris_action":"startup_submit"`) {
		t.Fatalf("completed startup card should be frozen: %s", completed)
	}
}

func TestLarkNotificationCardOmitsTerminalSelectWhenRunningOrDisabled(t *testing.T) {
	interaction := &TerminalInteraction{
		ID: "ti_1",
		Options: []TerminalInteractionOption{
			{ID: "opt_1", Input: "1", Label: "one"},
			{ID: "opt_2", Input: "2", Label: "two"},
		},
	}
	for _, note := range []WaitingNotification{
		{SessionID: "sess-1", Name: "A", Content: "running", Running: true, Interaction: interaction},
		{SessionID: "sess-1", Name: "A", Content: "disabled", Disabled: true, Interaction: interaction},
	} {
		content, err := larkNotificationCardContent(note, "ou_1", false)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(content, `"tag":"select_static"`) {
			t.Fatalf("non-actionable card should omit terminal select: %s", content)
		}
	}
}

func TestLarkReplyBridgeTerminalSelectWritesOptionOnceWithoutEnter(t *testing.T) {
	resetLarkRegistryForTest()
	previousDelay := structuredInputEnterDelay
	structuredInputEnterDelay = 0
	defer func() { structuredInputEnterDelay = previousDelay }()

	launcher := &recordingLauncher{}
	notifier := &recordingNotifier{}
	manager := NewManager(nil, launcher, WithNotifier(notifier))
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	sess, err := manager.CreateSession(context.Background(), "Codex")
	if err != nil {
		t.Fatal(err)
	}
	rt, ok := manager.GetRuntime(sess.ID)
	if !ok {
		t.Fatal("runtime not found")
	}
	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	rt.session.NotifyOnWaiting = true
	rt.session.LarkChatID = "oc_chat"
	rt.notifyVersion = 5
	rt.lastNotifiedMessageID = "om_model"
	rt.visibleSnapshot = strings.Join([]string{
		"Select Model and Effort",
		"1. gpt-5.5 (current)  Frontier model",
		"2. gpt-5.4            Everyday coding model",
		"Press enter to confirm or esc to go back",
	}, "\n")
	rt.visibleSnapshotVersion = 1
	rt.pendingTerminalInteraction = &TerminalInteraction{
		ID:            "ti_model",
		Kind:          TerminalInteractionCodexModel,
		NotifyVersion: 5,
		MessageID:     "om_model",
		Options: []TerminalInteractionOption{
			{ID: "opt_1", Input: "1", Label: "gpt-5.5"},
			{ID: "opt_2", Input: "2", Label: "gpt-5.4"},
		},
	}
	rt.mu.Unlock()
	event := &callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
		Operator: &callback.Operator{OpenID: "ou_clicker"},
		Action: &callback.CallBackAction{
			Value: map[string]interface{}{
				"iris_action":    "terminal_select",
				"session_id":     sess.ID,
				"interaction_id": "ti_model",
			},
			Option: "opt_2",
		},
		Context: &callback.Context{OpenMessageID: "om_model", OpenChatID: "oc_chat"},
	}}

	event.Event.Context.OpenChatID = "oc_other"
	resp, err := bridge.HandleCardActionTrigger(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Toast == nil || !strings.Contains(resp.Toast.Content, "不属于当前群聊") {
		t.Fatalf("cross-chat selection should be rejected, got %#v", resp)
	}
	if parts := launcher.terminals[0].writeParts(); len(parts) != 0 {
		t.Fatalf("cross-chat selection must not write terminal input, got %#v", parts)
	}
	event.Event.Context.OpenChatID = "oc_chat"

	resp, err = bridge.HandleCardActionTrigger(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Content != "已选择 gpt-5.4" {
		t.Fatalf("response = %#v", resp)
	}
	parts := launcher.terminals[0].writeParts()
	if len(parts) != 1 || parts[0] != "2" {
		t.Fatalf("selection should write only the mapped number without enter, got %#v", parts)
	}
	if got := rt.NotificationMentionOpenID(); got != "ou_clicker" {
		t.Fatalf("selection mention open id = %q", got)
	}
	rt.mu.Lock()
	if rt.lastNotifiedMessageID != "om_model" {
		t.Fatalf("clicked card should remain the next update anchor, got %q", rt.lastNotifiedMessageID)
	}
	rt.session.Status = StatusWaiting
	version := rt.notifyVersion
	rt.mu.Unlock()
	reasoningMenu := strings.Join([]string{
		"Select Reasoning Level for gpt-5.4",
		"1. Low                  Fast responses with lighter reasoning",
		"2. Medium (default)     Balanced reasoning",
		"3. High (current)       Greater reasoning depth",
	}, "\n")
	rt.SetVisibleSnapshot(reasoningMenu)
	rt.notifyIfStillWaiting(version)
	notes := notifier.notes()
	if len(notes) != 1 || notes[0].MessageID != "om_model" || notes[0].Interaction == nil || notes[0].Interaction.Kind != TerminalInteractionCodexReasoning {
		t.Fatalf("reasoning menu should patch the clicked card with a second dropdown, got %#v", notes)
	}

	resp, err = bridge.HandleCardActionTrigger(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Toast == nil || !strings.Contains(resp.Toast.Content, "已失效") {
		t.Fatalf("duplicate selection should return an expired toast, got %#v", resp)
	}
	if parts := launcher.terminals[0].writeParts(); len(parts) != 1 {
		t.Fatalf("duplicate selection must not write again, got %#v", parts)
	}

	rt.mu.Lock()
	rt.session.Status = StatusWaiting
	rt.notifyVersion++
	rt.pendingTerminalInteraction = &TerminalInteraction{
		ID:            "ti_resume",
		NotifyVersion: rt.notifyVersion,
		MessageID:     "om_model",
		Kind:          TerminalInteractionCodexResume,
		Options: []TerminalInteractionOption{
			{ID: "opt_1", Label: "历史会话", Input: "\x1b[B", SubmitWithEnter: true},
		},
	}
	rt.mu.Unlock()
	event.Event.Action.Value["interaction_id"] = "ti_resume"
	event.Event.Action.Option = "opt_1"
	resp, err = bridge.HandleCardActionTrigger(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Content != "已选择 历史会话" {
		t.Fatalf("resume selection response = %#v", resp)
	}
	parts = launcher.terminals[0].writeParts()
	if len(parts) != 3 || parts[1] != "\x1b[B" || parts[2] != "\r" {
		t.Fatalf("resume selection should navigate then press enter, got %#v", parts)
	}
}
