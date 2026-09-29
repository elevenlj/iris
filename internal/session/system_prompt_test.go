package session

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestSystemPromptLaunchAndFallback(t *testing.T) {
	prompt := "你是机器人的助理。\n保留 'quotes'、\"双引号\" 和 $(echo no)。"
	for _, command := range []string{CodexAgentCommand, AidenCodexAgentCommand, "codex resume thread", ClaudeAgentCommand, AidenClaudeAgentCommand, "claude --resume thread", AidenAgentCommand, TraeAgentCommand, "custom-agent"} {
		t.Run(command, func(t *testing.T) {
			m := NewManager(nil, nil)
			m.SetSystemPrompt(prompt)
			rt := &RuntimeSession{manager: m}
			launch, err := rt.agentLaunchCommand(command)
			if err != nil {
				t.Fatal(err)
			}
			kind := agentKindForCommand(command, "")
			if isAidenCodexCommand(command) {
				kind = "custom"
			}
			out, err := exec.Command("sh", "-c", "set -- "+launch+`; printf '%s\000' "$@"`).Output()
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
			switch kind {
			case "codex":
				var got string
				for _, arg := range args {
					if strings.HasPrefix(arg, "developer_instructions=") {
						err = json.Unmarshal([]byte(strings.TrimPrefix(arg, "developer_instructions=")), &got)
					}
				}
				if err != nil || got != prompt {
					t.Fatalf("prompt = %q, err=%v", got, err)
				}
			case "claude", "aiden":
				found := false
				for i, arg := range args {
					if arg == "--append-system-prompt" && i+1 < len(args) && args[i+1] == prompt {
						found = true
					}
				}
				if !found {
					t.Fatalf("missing prompt: %q", launch)
				}
			default:
				if launch != command || rt.pendingSystemPrompt != prompt {
					t.Fatal("missing fallback")
				}
			}
			m.SetSystemPrompt("")
			launch, err = rt.agentLaunchCommand(command)
			if err != nil || launch != command || rt.pendingSystemPrompt != "" {
				t.Fatal("clearing prompt changed launch or left stale fallback")
			}
		})
	}
}

func TestSystemPromptFallbackOnlyOnFirstRequest(t *testing.T) {
	launcher := &recordingLauncher{}
	m := NewManager(nil, launcher)
	sess, err := m.CreateSession(context.Background(), "prompt")
	if err != nil {
		t.Fatal(err)
	}
	defer m.DeleteSession(context.Background(), sess.ID)
	rt, _ := m.GetRuntime(sess.ID)
	m.SetSystemPrompt("机器人 A 的指令")
	if _, err := rt.agentLaunchCommand("custom-agent"); err != nil {
		t.Fatal(err)
	}
	m.SetSystemPrompt("下次启动才生效")
	for _, input := range []string{"/model", "1", "你好", "下一轮"} {
		if err := SubmitStructuredInputWithMention(rt, input, ""); err != nil {
			t.Fatal(err)
		}
	}
	writes := launcher.terminals[0].writes()
	if strings.Count(writes, "机器人 A 的指令") != 1 || strings.Contains(writes, "下次启动才生效") || !strings.Contains(writes, "【当前请求】\n你好") {
		t.Fatalf("incorrect fallback injection: %q", writes)
	}
	if rt.pendingSystemPrompt != "" {
		t.Fatal("prompt was not consumed")
	}
}

func TestSystemPromptAppliesOnCreateWithoutPersistingLaunchFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	launcher := &recordingLauncher{}
	m := NewManager(nil, launcher)
	m.SetSystemPrompt("isolated-bot-prompt")
	m.SetAgentConfig(AgentConfig{Kind: "codex", Command: CodexAgentCommand}, nil)
	sess, err := m.CreateSession(context.Background(), "native prompt")
	if err != nil {
		t.Fatal(err)
	}
	defer m.DeleteSession(context.Background(), sess.ID)
	if !strings.Contains(launcher.terminals[0].writes(), "developer_instructions=") {
		t.Fatal("create did not inject prompt")
	}
	if strings.Contains(sess.LastAgentStartCommand, "isolated-bot-prompt") || strings.Contains(sess.LastAgentResumeCommand, "isolated-bot-prompt") {
		t.Fatal("persisted stale prompt in recovery command")
	}
}
