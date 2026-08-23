package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/elevenlj/iris/internal/session"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

type wsBridge struct {
	rt         *session.RuntimeSession
	conn       *websocket.Conn
	headless   bool
	subscriber chan session.RuntimeEvent
}

type clientMessage struct {
	Type                      string `json:"type"`
	Data                      string `json:"data,omitempty"`
	Source                    string `json:"source,omitempty"`
	RequestID                 string `json:"request_id,omitempty"`
	ContinuityVersion         uint32 `json:"continuity_version,omitempty"`
	RenderEpoch               uint64 `json:"render_epoch,omitempty"`
	BufferType                string `json:"buffer_type,omitempty"`
	BufferAtCapacity          bool   `json:"buffer_at_capacity,omitempty"`
	AnchorGuardActive         bool   `json:"anchor_guard_active,omitempty"`
	AnchorGuardLine           int    `json:"anchor_guard_line,omitempty"`
	CursorLine                *int   `json:"cursor_line,omitempty"`
	BaselineSnapshot          string `json:"baseline_snapshot,omitempty"`
	BaselineSource            string `json:"baseline_source,omitempty"`
	BaselineContinuityVersion uint32 `json:"baseline_continuity_version,omitempty"`
	BaselineRenderEpoch       uint64 `json:"baseline_render_epoch,omitempty"`
	BaselineBufferType        string `json:"baseline_buffer_type,omitempty"`
	BaselineBufferAtCapacity  bool   `json:"baseline_buffer_at_capacity,omitempty"`
	BaselineAnchorGuardActive bool   `json:"baseline_anchor_guard_active,omitempty"`
	BaselineAnchorGuardLine   int    `json:"baseline_anchor_guard_line,omitempty"`
	BaselineCursorLine        *int   `json:"baseline_cursor_line,omitempty"`
	Cols                      uint16 `json:"cols,omitempty"`
	Rows                      uint16 `json:"rows,omitempty"`
}

func serveWS(w http.ResponseWriter, r *http.Request, rt *session.RuntimeSession) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	headless := r.URL.Query().Get("headless") == "1"
	b := &wsBridge{rt: rt, conn: conn, headless: headless}
	if headless {
		cols, rows := rt.TerminalSize()
		if err := b.writeTerminalResize(cols, rows); err != nil {
			_ = conn.Close()
			return
		}
	}
	_ = conn.WriteMessage(websocket.BinaryMessage, rt.OutputSnapshot())
	ch, cancel := rt.SubscribeWithMode(headless)
	b.subscriber = ch
	defer cancel()
	defer conn.Close()
	go b.readClient()
	for ev := range ch {
		switch ev.Type {
		case session.RuntimeEventSnapshotRequest:
			msg, _ := json.Marshal(snapshotRequestPayload(ev))
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case session.RuntimeEventTerminalResize:
			if err := b.writeTerminalResize(ev.Cols, ev.Rows); err != nil {
				return
			}
		default:
			if err := conn.WriteMessage(websocket.BinaryMessage, ev.Data); err != nil {
				return
			}
		}
	}
}

func snapshotRequestPayload(ev session.RuntimeEvent) map[string]string {
	payload := map[string]string{"type": "snapshot_request", "request_id": ev.RequestID}
	if purpose := strings.TrimSpace(ev.Purpose); purpose != "" {
		payload["purpose"] = purpose
	}
	return payload
}

func (b *wsBridge) writeTerminalResize(cols, rows uint16) error {
	msg, _ := json.Marshal(map[string]any{"type": "terminal_resize", "cols": cols, "rows": rows})
	return b.conn.WriteMessage(websocket.TextMessage, msg)
}

func (b *wsBridge) readClient() {
	for {
		_, data, err := b.conn.ReadMessage()
		if err != nil {
			return
		}
		var msg clientMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		b.handleClientMessage(msg)
	}
}

func (b *wsBridge) handleClientMessage(msg clientMessage) {
	switch msg.Type {
	case "input":
		if b.headless {
			return
		}
		filtered := filterTerminalResponses([]byte(msg.Data))
		if len(filtered) > 0 {
			b.rt.SetNotificationMentionOpenID("")
			if strings.ContainsAny(string(filtered), "\r\n") && (msg.BaselineSnapshot != "" || msg.BaselineSource != "") {
				_ = b.rt.WriteInputWithSnapshotBaselineFrom(string(filtered), msg.BaselineSnapshot, b.snapshotSource(msg.BaselineSource, msg.BaselineContinuityVersion, msg.BaselineRenderEpoch, msg.BaselineBufferType, msg.BaselineBufferAtCapacity, msg.BaselineAnchorGuardActive, msg.BaselineAnchorGuardLine, msg.BaselineCursorLine), b.subscriber)
				return
			}
			_ = b.rt.WriteInputFrom(string(filtered), b.subscriber)
		}
	case "submit":
		if b.headless {
			return
		}
		_ = session.SubmitStructuredInputFrom(b.rt, msg.Data, b.subscriber)
	case "resize":
		if b.headless {
			return
		}
		_ = b.rt.ResizeFrom(msg.Cols, msg.Rows, b.subscriber)
	case "snapshot":
		b.rt.SetVisibleSnapshotResponseFrom(msg.Data, b.snapshotSource(msg.Source, msg.ContinuityVersion, msg.RenderEpoch, msg.BufferType, msg.BufferAtCapacity, msg.AnchorGuardActive, msg.AnchorGuardLine, msg.CursorLine), msg.RequestID, b.subscriber)
	}
}

func (b *wsBridge) snapshotSource(source string, continuityVersion uint32, renderEpoch uint64, bufferType string, bufferAtCapacity bool, anchorGuardActive bool, anchorGuardLine int, cursorLine *int) string {
	source = strings.TrimSpace(source)
	if source == "" {
		source = "unknown"
	}
	prefix := "browser:"
	if b.headless {
		prefix = "headless:"
	}
	base := prefix + url.QueryEscape(source)
	bufferType = strings.TrimSpace(strings.ToLower(bufferType))
	if continuityVersion == 0 && renderEpoch == 0 && bufferType == "" && !bufferAtCapacity && !anchorGuardActive {
		// One-version compatibility for an already-open client running the old
		// app.js protocol.
		return base
	}
	cursor := -1
	if cursorLine != nil {
		cursor = *cursorLine
	}
	return fmt.Sprintf("%s;continuity_version=%d;render_epoch=%d;buffer_type=%s;buffer_at_capacity=%t;anchor_guard_active=%t;anchor_guard_line=%d;cursor_line=%d", base, continuityVersion, renderEpoch, url.QueryEscape(bufferType), bufferAtCapacity, anchorGuardActive, anchorGuardLine, cursor)
}

func filterTerminalResponses(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); {
		if data[i] == 0x1b && i+1 < len(data) {
			switch data[i+1] {
			case '[':
				if end, ok := terminalResponseCSIEnd(data, i+2); ok {
					i = end + 1
					continue
				}
			case ']', 'P', '^', '_':
				if end, ok := stringControlEnd(data, i+2); ok {
					i = end + 1
					continue
				}
			}
		}
		out = append(out, data[i])
		i++
	}
	return out
}

func terminalResponseCSIEnd(data []byte, start int) (int, bool) {
	if start >= len(data) || !isCSIParamByte(data[start]) {
		return 0, false
	}
	for i := start; i < len(data); i++ {
		b := data[i]
		if b >= 0x40 && b <= 0x7e {
			return i, isTerminalResponseCSIFinal(b)
		}
		if !isCSIParamByte(b) && !(b >= 0x20 && b <= 0x2f) {
			return 0, false
		}
	}
	return 0, false
}

func isCSIParamByte(b byte) bool {
	return b >= 0x30 && b <= 0x3f
}

func isTerminalResponseCSIFinal(b byte) bool {
	switch b {
	case 'R', 'c', 'n', 't', 'u':
		return true
	default:
		return false
	}
}

func stringControlEnd(data []byte, start int) (int, bool) {
	for i := start; i < len(data); i++ {
		if data[i] == 0x07 {
			return i, true
		}
		if data[i] == 0x1b && i+1 < len(data) && data[i+1] == '\\' {
			return i + 1, true
		}
	}
	return 0, false
}
