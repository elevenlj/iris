package session

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type codexCompletionMeta struct {
	Type    string `json:"type"`
	Payload struct {
		ID           string          `json:"id"`
		SessionID    string          `json:"session_id"`
		Source       json.RawMessage `json:"source"`
		ThreadSource json.RawMessage `json:"thread_source"`
		Parent       string          `json:"parent_thread_id"`
		ForkedFrom   string          `json:"forked_from_id"`
		Timestamp    time.Time       `json:"timestamp"`
	} `json:"payload"`
}

func (meta codexCompletionMeta) isRootThread(threadID string) bool {
	id := meta.Payload.ID
	if id == "" {
		id = meta.Payload.SessionID
	}
	source := strings.ToLower(string(meta.Payload.Source) + string(meta.Payload.ThreadSource))
	return meta.Type == "session_meta" && id == threadID && meta.Payload.Parent == "" && !strings.Contains(source, "subagent") && (strings.Contains(source, `"cli"`) || strings.Contains(source, `"user"`))
}

func openCodexCompletionRollout(home, threadID string) (*os.File, error) {
	if !validCodexThreadID(threadID) {
		return nil, errors.New("invalid thread identity")
	}
	files, err := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "rollout-*-"+threadID+".jsonl"))
	if err != nil || len(files) != 1 {
		return nil, errors.New("thread rollout unavailable or ambiguous")
	}
	return os.Open(files[0])
}

func validateCodexForkLineage(home, threadID, parent, pinned string) error {
	seen := map[string]bool{threadID: true}
	for parent != pinned {
		if seen[parent] {
			return errors.New("cyclic fork lineage")
		}
		seen[parent] = true
		f, err := openCodexCompletionRollout(home, parent)
		if err != nil {
			return err
		}
		var meta codexCompletionMeta
		err = json.NewDecoder(f).Decode(&meta)
		f.Close()
		if err != nil {
			return err
		}
		if !meta.isRootThread(parent) {
			return errors.New("fork ancestor is not a root CLI thread")
		}
		parent = meta.Payload.ForkedFrom
	}
	return nil
}

// Validate only the reported thread's rollout; never choose the newest session
// in a working directory, and never trust inherited parent session_id metadata.
func validateCodexCompletion(sess Session, threadID, turnID string, submitted time.Time, input string) error {
	home := sess.LastAgentHome
	if home == "" {
		home = defaultCodexHome()
	}
	f, err := openCodexCompletionRollout(home, threadID)
	if err != nil {
		return err
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	var meta codexCompletionMeta
	if err := decoder.Decode(&meta); err != nil {
		return err
	}
	if !meta.isRootThread(threadID) {
		return errors.New("completion is not from a root CLI thread")
	}
	pinned := codexResumeThreadID(sess.LastAgentResumeCommand)
	changingThread := pinned != "" && pinned != threadID
	if changingThread {
		if submitted.IsZero() || meta.Payload.Timestamp.IsZero() {
			return errors.New("fork submission or creation time unavailable")
		}
		if err := validateCodexForkLineage(home, threadID, meta.Payload.ForkedFrom, pinned); err != nil {
			return err
		}
	}
	latestTurn := ""
	var started time.Time
	consumedInput, ended := false, false
	// shortcut: stream this one rollout per completion, cache offsets if large logs become slow.
	for {
		var event struct {
			Timestamp time.Time `json:"timestamp"`
			Type      string    `json:"type"`
			Payload   struct {
				Type    string `json:"type"`
				TurnID  string `json:"turn_id"`
				Role    string `json:"role"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				InputMetadata struct {
					TurnID string `json:"turn_id"`
				} `json:"internal_chat_message_metadata_passthrough"`
				Item struct {
					Type    string `json:"type"`
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"item"`
			} `json:"payload"`
		}
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		if event.Type == "event_msg" && event.Payload.Type == "task_started" {
			latestTurn, started = event.Payload.TurnID, event.Timestamp
			consumedInput, ended = false, false
		} else if event.Type == "turn_context" && event.Payload.TurnID != "" && event.Payload.TurnID != latestTurn {
			latestTurn, started = event.Payload.TurnID, event.Timestamp
			consumedInput, ended = false, false
		} else if event.Type == "event_msg" && event.Payload.Type == "task_complete" && event.Payload.TurnID == latestTurn {
			ended = true
		} else if !ended && latestTurn == turnID && !event.Timestamp.Before(submitted) && strings.TrimSpace(input) != "" {
			// Steering is consumed within the existing turn, without another task_started.
			var text strings.Builder
			if event.Type == "response_item" && event.Payload.Type == "message" && event.Payload.Role == "user" && (event.Payload.InputMetadata.TurnID == "" || event.Payload.InputMetadata.TurnID == latestTurn) {
				for _, part := range event.Payload.Content {
					text.WriteString(part.Text)
				}
			} else if event.Type == "event_msg" && event.Payload.Type == "item_completed" && event.Payload.TurnID == latestTurn && event.Payload.Item.Type == "UserMessage" {
				for _, part := range event.Payload.Item.Content {
					text.WriteString(part.Text)
				}
			}
			if strings.TrimSpace(text.String()) == strings.TrimSpace(input) && (!changingThread || !event.Timestamp.Before(meta.Payload.Timestamp)) {
				consumedInput = true
			}
		}
	}
	if latestTurn != turnID || started.IsZero() || (started.Before(submitted) && !consumedInput) {
		return errors.New("completion does not belong to the current submitted turn")
	}
	// Forks carry copied history; ancestry alone cannot prove this Iris round was consumed.
	if changingThread && !consumedInput {
		return errors.New("fork has not consumed the current submitted input")
	}
	return nil
}
