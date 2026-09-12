package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed feishu_setup.js
var feishuSetupScript string

type createdFeishuApp struct {
	AppID      string `json:"app_id"`
	AppSecret  string `json:"app_secret"`
	AppName    string `json:"app_name"`
	OwnerEmail string `json:"owner_email"`
}

// A dedicated, temporary browser profile keeps login cookies out of Iris config
// and never reads or exports the user's normal browser profile.
func createFeishuApp(ctx context.Context, name string) (createdFeishuApp, error) {
	var result createdFeishuApp
	chrome := findChrome()
	if chrome == "" {
		return result, errors.New("自动创建需要本机安装 Chrome")
	}
	profile, err := os.MkdirTemp("", "iris-feishu-setup-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(profile)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, chrome, "--user-data-dir="+profile, "--remote-debugging-address=127.0.0.1", "--remote-debugging-port=0", "--no-first-run", "--no-default-browser-check", "https://open.feishu.cn/app")
	if err := command.Start(); err != nil {
		return result, err
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	client := &http.Client{Timeout: 3 * time.Second}
	var connection *websocket.Conn
	for connection == nil {
		if err := pauseSetup(ctx); err != nil {
			return result, err
		}
		data, err := os.ReadFile(filepath.Join(profile, "DevToolsActivePort"))
		if err != nil {
			continue
		}
		port := strings.Split(string(data), "\n")[0]
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			continue
		}
		response, err := client.Get("http://127.0.0.1:" + port + "/json/list")
		if err != nil {
			continue
		}
		var pages []struct {
			Type   string `json:"type"`
			URL    string `json:"url"`
			Socket string `json:"webSocketDebuggerUrl"`
		}
		err = json.NewDecoder(response.Body).Decode(&pages)
		response.Body.Close()
		if err != nil {
			continue
		}
		for _, page := range pages {
			if page.Type == "page" && (strings.HasPrefix(page.URL, "https://open.feishu.cn/") || strings.HasPrefix(page.URL, "https://open.larkoffice.com/")) {
				connection, _, err = websocket.DefaultDialer.DialContext(ctx, page.Socket, nil)
				if err == nil {
					break
				}
			}
		}
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	sequence := 0
	evaluate := func(expression string, out any) error {
		sequence++
		if err := connection.WriteJSON(map[string]any{"id": sequence, "method": "Runtime.evaluate", "params": map[string]any{"expression": expression, "awaitPromise": true, "returnByValue": true}}); err != nil {
			return err
		}
		for {
			var response struct {
				ID     int             `json:"id"`
				Error  json.RawMessage `json:"error"`
				Result struct {
					Result struct {
						Value json.RawMessage `json:"value"`
					} `json:"result"`
					Exception *struct {
						Text      string `json:"text"`
						Exception struct {
							Description string `json:"description"`
						} `json:"exception"`
					} `json:"exceptionDetails"`
				} `json:"result"`
			}
			if err := connection.ReadJSON(&response); err != nil {
				return err
			}
			if response.ID != sequence {
				continue
			}
			if response.Error != nil {
				return errors.New("浏览器执行失败")
			}
			if response.Result.Exception != nil {
				return fmt.Errorf("%s", response.Result.Exception.Exception.Description)
			}
			return json.Unmarshal(response.Result.Result.Value, out)
		}
	}
	for {
		var ready bool
		if err := evaluate("Boolean(window.csrfToken && window.user && window.user.id)", &ready); err == nil && ready {
			break
		}
		if err := pauseSetup(ctx); err != nil {
			return result, err
		}
	}
	encodedName, _ := json.Marshal(name)
	err = evaluate(feishuSetupScript+"\nirisCreateFeishuApp("+string(encodedName)+")", &result)
	return result, err
}

func pauseSetup(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Second):
		return nil
	}
}
