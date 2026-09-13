package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

type larkBridgeWSClient struct {
	appID       string
	appSecret   string
	handler     *dispatcher.EventDispatcher
	cardHandler func(context.Context, []byte) (*callback.CardActionTriggerResponse, error)
	conn        *websocket.Conn
	serviceID   string
	pingEvery   time.Duration
	retryEvery  time.Duration
	connURL     func(context.Context) (string, error)
	writeMu     sync.Mutex
	status      atomic.Value // string; independent of the socket's lifetime
}

func newLarkBridgeWSClient(appID, appSecret string, handler *dispatcher.EventDispatcher, cardHandler func(context.Context, []byte) (*callback.CardActionTriggerResponse, error)) *larkBridgeWSClient {
	return &larkBridgeWSClient{
		appID:       appID,
		appSecret:   appSecret,
		handler:     handler,
		cardHandler: cardHandler,
		pingEvery:   2 * time.Minute,
		retryEvery:  2 * time.Second,
	}
}

func (c *larkBridgeWSClient) Start(ctx context.Context) error {
	c.status.Store("connecting")
	defer c.status.Store("stopped")
	for {
		if err := c.connect(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("lark bridge ws connect failed: %v", err)
			c.status.Store("reconnecting")
			if err := c.waitBeforeReconnect(ctx); err != nil {
				return err
			}
			continue
		}
		connCtx, cancelPing := context.WithCancel(ctx)
		go c.pingLoop(connCtx)
		if err := c.receiveLoop(ctx); err != nil {
			log.Printf("lark bridge ws disconnected: %v", err)
		}
		cancelPing()
		_ = c.Close()
		c.status.Store("reconnecting")
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := c.waitBeforeReconnect(ctx); err != nil {
			return err
		}
	}
}

func (c *larkBridgeWSClient) waitBeforeReconnect(ctx context.Context) error {
	delay := c.retryEvery
	if delay <= 0 {
		delay = 2 * time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *larkBridgeWSClient) Close() error {
	c.status.Store("disconnected")
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

func (c *larkBridgeWSClient) connect(ctx context.Context) error {
	getConnURL := c.connURL
	if getConnURL == nil {
		getConnURL = c.getConnURL
	}
	connURL, err := getConnURL(ctx)
	if err != nil {
		return err
	}
	u, err := url.Parse(connURL)
	if err != nil {
		return err
	}
	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, connURL, nil)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			_ = resp.Body.Close()
			return fmt.Errorf("lark ws dial failed: %s %s", resp.Status, string(body))
		}
		return err
	}
	c.conn = conn
	c.serviceID = u.Query().Get(larkws.ServiceID)
	c.status.Store("connected")
	log.Printf("lark bridge ws connected")
	return nil
}

func (c *larkBridgeWSClient) getConnURL(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]string{"AppID": c.appID, "AppSecret": c.appSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, lark.FeishuBaseUrl+larkws.GenEndpointUri, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Add("locale", "zh")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("lark ws endpoint returned %s: %s", resp.Status, string(body))
	}
	var endpoint struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			URL          string `json:"URL"`
			ClientConfig *struct {
				PingInterval int `json:"PingInterval"`
			} `json:"ClientConfig"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&endpoint); err != nil {
		return "", err
	}
	if endpoint.Code != 0 {
		return "", fmt.Errorf("lark ws endpoint returned code %d: %s", endpoint.Code, endpoint.Msg)
	}
	if endpoint.Data.URL == "" {
		return "", fmt.Errorf("lark ws endpoint returned empty url")
	}
	if endpoint.Data.ClientConfig != nil && endpoint.Data.ClientConfig.PingInterval > 0 {
		c.pingEvery = time.Duration(endpoint.Data.ClientConfig.PingInterval) * time.Second
	}
	return endpoint.Data.URL, nil
}

func (c *larkBridgeWSClient) receiveLoop(ctx context.Context) error {
	for {
		mt, msg, err := c.conn.ReadMessage()
		if err != nil {
			return err
		}
		if mt != websocket.BinaryMessage {
			continue
		}
		go c.handleMessage(ctx, msg)
	}
}

func (c *larkBridgeWSClient) handleMessage(ctx context.Context, msg []byte) {
	var frame larkws.Frame
	if err := frame.Unmarshal(msg); err != nil {
		log.Printf("lark bridge ws unmarshal frame failed: %v", err)
		return
	}
	if frame.Method == int32(larkws.FrameTypeControl) {
		return
	}
	if frame.Method != int32(larkws.FrameTypeData) {
		return
	}
	hs := larkws.Headers(frame.Headers)
	msgType := hs.GetString(larkws.HeaderType)
	start := time.Now()
	var rsp interface{}
	var err error
	switch larkws.MessageType(msgType) {
	case larkws.MessageTypeEvent:
		if c.handler != nil {
			rsp, err = c.handler.Do(ctx, frame.Payload)
		}
	case larkws.MessageTypeCard:
		if c.cardHandler != nil {
			rsp, err = c.cardHandler(ctx, frame.Payload)
		}
	default:
		return
	}
	hs.Add(larkws.HeaderBizRt, strconv.FormatInt(time.Since(start).Milliseconds(), 10))
	resp := larkBridgeWSResponse{Code: http.StatusOK}
	if err != nil {
		log.Printf("lark bridge ws handle %s failed: %v", msgType, err)
		resp = larkBridgeWSResponse{Code: http.StatusInternalServerError}
	} else if rsp != nil {
		if resp.Data, err = json.Marshal(rsp); err != nil {
			log.Printf("lark bridge ws marshal response failed: %v", err)
			resp = larkBridgeWSResponse{Code: http.StatusInternalServerError}
		}
	}
	payload, _ := json.Marshal(resp)
	if larkws.MessageType(msgType) == larkws.MessageTypeCard {
		log.Printf("lark bridge ws card response payload=%s", payload)
	}
	frame.Payload = payload
	frame.Headers = hs
	out, err := frame.Marshal()
	if err != nil {
		log.Printf("lark bridge ws marshal frame failed: %v", err)
		return
	}
	if err := c.writeMessage(out); err != nil {
		log.Printf("lark bridge ws write response failed: %v", err)
	}
}

type larkBridgeWSResponse struct {
	Code    int               `json:"code"`
	Headers map[string]string `json:"headers"`
	Data    []byte            `json:"data"`
}

func (c *larkBridgeWSClient) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(c.pingEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if c.conn == nil {
			continue
		}
		serviceID, _ := strconv.ParseInt(c.serviceID, 10, 32)
		frame := larkws.NewPingFrame(int32(serviceID))
		msg, err := frame.Marshal()
		if err != nil {
			continue
		}
		if err := c.writeMessage(msg); err != nil {
			log.Printf("lark bridge ws ping failed: %v", err)
			return
		}
	}
}

func (c *larkBridgeWSClient) writeMessage(msg []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("connection is closed")
	}
	return c.conn.WriteMessage(websocket.BinaryMessage, msg)
}
