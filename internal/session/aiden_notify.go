package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func aidenCheckpointAssistantMessage(dir, sessionID string) (string, error) {
	if !validCodexThreadID(sessionID) || !filepath.IsAbs(dir) || filepath.Base(filepath.Clean(dir)) != sessionID {
		return "", errors.New("Aiden transcript does not match the event session")
	}
	var latest struct {
		Latest string `json:"latest"`
	}
	if err := readAidenCheckpointJSON(filepath.Join(dir, "latest.json"), 4096, &latest); err != nil {
		return "", err
	}
	if !validCodexThreadID(latest.Latest) {
		return "", errors.New("invalid Aiden checkpoint ID")
	}
	var state struct {
		Config struct {
			Configurable struct {
				ThreadID string `json:"thread_id"`
			} `json:"configurable"`
		} `json:"config"`
		Checkpoint struct {
			ID            string `json:"id"`
			ChannelValues struct {
				Messages []struct {
					Type      string            `json:"type"`
					Content   json.RawMessage   `json:"content"`
					ToolCalls []json.RawMessage `json:"tool_calls"`
				} `json:"messages"`
			} `json:"channel_values"`
		} `json:"checkpoint"`
	}
	if err := readAidenCheckpointJSON(filepath.Join(dir, latest.Latest+".json"), 64<<20, &state); err != nil {
		return "", err
	}
	if state.Config.Configurable.ThreadID != sessionID || state.Checkpoint.ID != latest.Latest {
		return "", errors.New("Aiden checkpoint identity mismatch")
	}
	messages := state.Checkpoint.ChannelValues.Messages
	if len(messages) == 0 {
		return "", errors.New("Aiden checkpoint has no messages")
	}
	last := messages[len(messages)-1]
	if last.Type != "ai" || len(last.ToolCalls) != 0 {
		return "", errors.New("Aiden checkpoint has no final assistant reply")
	}
	var content string
	if err := json.Unmarshal(last.Content, &content); err != nil {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(last.Content, &blocks); err != nil {
			return "", fmt.Errorf("decode Aiden assistant content: %w", err)
		}
		var parts []string
		for _, block := range blocks {
			if block.Type == "text" && block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
		content = strings.Join(parts, "\n")
	}
	if strings.TrimSpace(content) == "" {
		return "", errors.New("Aiden final assistant reply is empty")
	}
	return content, nil
}

func readAidenCheckpointJSON(path string, maxBytes int64, target any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxBytes {
		return errors.New("Aiden checkpoint is not a regular file or exceeds size limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > maxBytes {
		return errors.New("Aiden checkpoint exceeds size limit")
	}
	return json.Unmarshal(data, target)
}
