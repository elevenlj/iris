package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

type LarkNotifier struct {
	WebhookURL string
	MentionAll bool
}

func (n *LarkNotifier) Available() bool {
	return n != nil && n.WebhookURL != ""
}

func (n *LarkNotifier) NotifyWaiting(note WaitingNotification) (WaitingNotificationResult, error) {
	if !n.Available() {
		return WaitingNotificationResult{}, errors.New("lark webhook is not configured")
	}
	content := larkTerminalPlainText(note.Content)
	elements := []map[string]any{}
	if n.MentionAll {
		elements = append(elements, map[string]any{"tag": "markdown", "content": "<at id=all></at>"})
	}
	elements = append(elements, map[string]any{"tag": "div", "text": map[string]any{"tag": "plain_text", "content": content}})
	elements = append(elements, larkShortcutActionElements(note.SessionID, note.UpdateNo, false, note.DeveloperModeEnabled)...)
	payload := map[string]any{
		"msg_type": "interactive",
		"card": map[string]any{
			"schema": "2.0",
			"header": map[string]any{"template": "blue", "title": map[string]any{"tag": "plain_text", "content": note.Name}},
			"body":   map[string]any{"elements": elements},
		},
	}
	b, _ := json.Marshal(payload)
	resp, err := http.Post(n.WebhookURL, "application/json", bytes.NewReader(b))
	if err != nil {
		return WaitingNotificationResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return WaitingNotificationResult{}, fmt.Errorf("%s: %s", resp.Status, string(body))
	}
	defaultLarkMessageRegistry.rememberLatest(note.SessionID)
	return WaitingNotificationResult{}, nil
}
