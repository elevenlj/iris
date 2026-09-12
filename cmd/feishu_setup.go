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

// Reuse only Iris's own login profile, isolated per service data directory.
// Never read or export the user's normal browser profile.
func createFeishuApp(ctx context.Context, name, dataDir string) (createdFeishuApp, error) {
	var result createdFeishuApp
	chrome := findChrome()
	if chrome == "" {
		return result, errors.New("自动创建需要本机安装 Chrome")
	}
	profile, err := feishuLoginProfile(dataDir)
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	command := exec.Command(chrome, "--user-data-dir="+profile, "--remote-debugging-address=127.0.0.1", "--remote-debugging-port=0", "--no-first-run", "--no-default-browser-check", "https://open.feishu.cn/app")
	if err := command.Start(); err != nil {
		return result, err
	}
	done := make(chan struct{})
	go func() { _ = command.Wait(); close(done) }()
	defer closeFeishuBrowser(command, done, profile)
	wait := func() error {
		select {
		case <-done:
			return errors.New("飞书登录窗口已关闭或启动失败，请重新创建；已保存的登录状态会保留")
		case <-ctx.Done():
			return fmt.Errorf("飞书登录或创建超时/取消；如页面提示设备授权，请先在飞书客户端完成授权：%w", ctx.Err())
		case <-time.After(time.Second):
			return nil
		}
	}
	client := &http.Client{Timeout: 3 * time.Second}
	var connection *websocket.Conn
	for connection == nil {
		if err := wait(); err != nil {
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
		if err := evaluate("['https://open.feishu.cn','https://open.larkoffice.com'].includes(location.origin) && Boolean(window.csrfToken && window.user && window.user.id)", &ready); err == nil && ready {
			break
		}
		if err := wait(); err != nil {
			return result, err
		}
	}
	encodedName, _ := json.Marshal(name)
	err = evaluate(feishuSetupScript+"\nirisCreateFeishuApp("+string(encodedName)+")", &result)
	return result, err
}

func feishuLoginProfile(dataDir string) (string, error) {
	if dataDir == "" {
		return "", errors.New("缺少飞书登录数据目录")
	}
	profile, err := filepath.Abs(filepath.Join(dataDir, "feishu-login"))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(profile, 0700); err != nil {
		return "", err
	}
	if err := os.Chmod(profile, 0700); err != nil {
		return "", err
	}
	// This is a transient discovery file, not login data. Chrome writes a fresh
	// endpoint on every launch; never connect to a previous process's port.
	if err := os.Remove(filepath.Join(profile, "DevToolsActivePort")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return profile, nil
}

// Let Chrome flush its login state before falling back to killing a stuck
// process. The normal user browser is never addressed by this endpoint.
func closeFeishuBrowser(command *exec.Cmd, done <-chan struct{}, profile string) {
	select {
	case <-done:
		return
	default:
	}
	data, err := os.ReadFile(filepath.Join(profile, "DevToolsActivePort"))
	parts := strings.Split(strings.TrimSpace(string(data)), "\n")
	if err == nil && len(parts) == 2 && strings.HasPrefix(parts[1], "/devtools/browser/") {
		if _, err := strconv.ParseUint(parts[0], 10, 16); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			conn, _, err := websocket.DefaultDialer.DialContext(ctx, "ws://127.0.0.1:"+parts[0]+parts[1], nil)
			if err == nil {
				_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
				_ = conn.WriteJSON(map[string]any{"id": 1, "method": "Browser.close"})
				_ = conn.Close()
			}
			cancel()
		}
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = command.Process.Kill()
		<-done
	}
}
