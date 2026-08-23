package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

func TestLarkBridgeWSResponseEncodesEmptyAckLikeOfficialSDK(t *testing.T) {
	resp := larkBridgeWSResponse{Code: http.StatusOK}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	official, err := json.Marshal(larkws.NewResponseByCode(http.StatusOK))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(official) {
		t.Fatalf("empty card response should match official SDK, got %s want %s", data, official)
	}
}

func TestLarkBridgeWSResponseEncodesCardDataLikeOfficialSDK(t *testing.T) {
	resp := larkBridgeWSResponse{Code: http.StatusOK, Data: []byte(`{"toast":{"type":"info","content":"刷新成功"}}`)}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	officialResp := larkws.NewResponseByCode(http.StatusOK)
	officialResp.Data = []byte(`{"toast":{"type":"info","content":"刷新成功"}}`)
	official, err := json.Marshal(officialResp)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(official) {
		t.Fatalf("card response should match official SDK, got %s want %s", data, official)
	}
	var got struct {
		Code int    `json:"code"`
		Data []byte `json:"data"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Code != http.StatusOK || string(got.Data) != `{"toast":{"type":"info","content":"刷新成功"}}` {
		t.Fatalf("card response should round-trip binary data, got code=%d data=%q raw=%s", got.Code, string(got.Data), data)
	}
}

func TestLarkBridgeWSCardHandlerExecutesShortcutWithEmptyAck(t *testing.T) {
	resetLarkRegistryForTest()
	launcher := &recordingLauncher{}
	manager := NewManager(nil, launcher)
	bridge := NewLarkReplyBridge("app", "secret", manager, t.TempDir())
	sess, err := manager.CreateSession(context.Background(), "Shortcut")
	if err != nil {
		t.Fatal(err)
	}
	client := newLarkBridgeWSClient("app", "secret", nil, bridge.handleCardActionPayload)
	frame := larkws.Frame{
		Method:  int32(larkws.FrameTypeData),
		Payload: []byte(`{"open_message_id":"bot-card","action":{"value":{"iris_action":"shortcut","session_id":"` + sess.ID + `","key":"ctrl_c"}}}`),
		Headers: []larkws.Header{
			{Key: larkws.HeaderType, Value: string(larkws.MessageTypeCard)},
		},
	}
	hs := larkws.Headers(frame.Headers)
	rsp, err := client.cardHandler(context.Background(), frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(larkBridgeWSResponse{Code: http.StatusOK})
	if err != nil {
		t.Fatal(err)
	}
	if hs.GetString(larkws.HeaderType) != "card" {
		t.Fatalf("test frame should be card")
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if rsp != nil {
		t.Fatalf("shortcut success should use empty ack, got %#v", rsp)
	}
	if _, ok := decoded["data"]; !ok {
		t.Fatalf("card frame response should include official data field for empty ack, got %s", out)
	}
	parts := launcher.terminals[0].writeParts()
	if len(parts) == 0 || parts[len(parts)-1] != "\x03" {
		t.Fatalf("card payload should send Ctrl-C, got %#v", parts)
	}
}

func TestLarkBridgeWSStartRetriesEndpointErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := newLarkBridgeWSClient("app", "secret", nil, nil)
	client.retryEvery = time.Millisecond
	var attempts atomic.Int32
	client.connURL = func(context.Context) (string, error) {
		if attempts.Add(1) >= 3 {
			cancel()
		}
		return "", errors.New("temporary endpoint failure")
	}
	err := client.Start(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start error = %v, want context.Canceled", err)
	}
	if got := attempts.Load(); got < 3 {
		t.Fatalf("endpoint should be retried, attempts=%d", got)
	}
}
