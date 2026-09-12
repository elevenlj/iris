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

	"github.com/elevenlj/iris/internal/httpapi"
	"github.com/gorilla/websocket"
)

//go:embed feishu_setup.js
var feishuSetupScript string

type createdFeishuApp struct {
	AppID      string              `json:"app_id"`
	AppSecret  string              `json:"app_secret"`
	AppName    string              `json:"app_name"`
	OwnerEmail string              `json:"owner_email"`
	Apps       []httpapi.FeishuApp `json:"apps,omitempty"`
}

// Reuse only Iris's own login profile, isolated per service data directory.
// Never read or export the user's normal browser profile.
func createFeishuApp(ctx context.Context, name, dataDir string) (createdFeishuApp, error) {
	return runFeishuSetup(ctx, name, dataDir, "create", "")
}

func runFeishuSetup(ctx context.Context, name, dataDir, mode, appID string) (createdFeishuApp, error) {
	result, err := runFeishuBrowser(ctx, name, dataDir, mode, appID, true)
	if errors.Is(err, errFeishuLoginRequired) {
		return runFeishuBrowser(ctx, name, dataDir, mode, appID, false)
	}
	return result, err
}

var errFeishuLoginRequired = errors.New("需要飞书登录")

func runFeishuBrowser(ctx context.Context, name, dataDir, mode, appID string, headless bool) (createdFeishuApp, error) {
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
	httpapi.ReportBotCreationProgress(ctx, "checking_login", "正在检查登录状态…", "")
	command := exec.Command(chrome, feishuChromeArgs(profile, headless)...)
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
	lastStage := "checking_login"
	reportLogin := func(pageURL string) error {
		stage, message := "checking_login", "正在检查登录状态…"
		if strings.HasPrefix(pageURL, "https://accounts.feishu.cn/") || strings.HasPrefix(pageURL, "https://accounts.larkoffice.com/") {
			stage, message = "login", "等待登录，请在飞书窗口完成登录…"
		} else if strings.HasPrefix(pageURL, "https://security.larkoffice.com/") || strings.HasPrefix(pageURL, "https://security.feishu.cn/") {
			stage, message = "device_auth", "等待设备授权，请在飞书客户端确认…"
		}
		if headless && stage != "checking_login" {
			return errFeishuLoginRequired
		}
		if stage != lastStage {
			lastStage = stage
			httpapi.ReportBotCreationProgress(ctx, stage, message, "")
		}
		return nil
	}
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
			ID     string `json:"id"`
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
			if page.Type == "page" {
				connection, _, err = websocket.DefaultDialer.DialContext(ctx, page.Socket, nil)
				if err == nil {
					// Session restore may bring back stale login tabs. Keep one Iris
					// tab and navigate afresh before judging the saved login state.
					for _, other := range pages {
						if other.Type == "page" && other.ID != page.ID {
							_ = connection.WriteJSON(map[string]any{"id": -2, "method": "Target.closeTarget", "params": map[string]string{"targetId": other.ID}})
						}
					}
					break
				}
			}
		}
	}
	defer connection.Close()
	if err := connection.WriteJSON(map[string]any{"id": -3, "method": "Page.navigate", "params": map[string]string{"url": "https://open.feishu.cn/app"}}); err != nil {
		return result, err
	}
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	// Wait for the new navigation to commit; the restored page can still show
	// a stale login URL while Page.navigate is in flight.
	for {
		var response struct {
			ID     int             `json:"id"`
			Error  json.RawMessage `json:"error"`
			Result struct {
				ErrorText string `json:"errorText"`
			} `json:"result"`
		}
		if err := connection.ReadJSON(&response); err != nil {
			return result, err
		}
		if response.ID == -3 {
			if response.Error != nil || response.Result.ErrorText != "" {
				return result, errors.New("飞书开放平台加载失败，请检查网络后重试")
			}
			break
		}
	}
	sequence := 0
	evaluate := func(expression string, out any) error {
		sequence++
		if err := connection.WriteJSON(map[string]any{"id": sequence, "method": "Runtime.evaluate", "params": map[string]any{"expression": expression, "awaitPromise": true, "returnByValue": true}}); err != nil {
			return err
		}
		for {
			var response struct {
				Method string `json:"method"`
				Params struct {
					Name    string `json:"name"`
					Payload string `json:"payload"`
				} `json:"params"`
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
			if response.Method == "Runtime.bindingCalled" && response.Params.Name == "irisSetupProgress" {
				var event httpapi.BotCreationProgress
				if json.Unmarshal([]byte(response.Params.Payload), &event) == nil {
					httpapi.ReportBotCreationProgress(ctx, event.Stage, event.Message, event.AppID)
				}
				continue
			}
			if response.ID != sequence {
				continue
			}
			if response.Error != nil {
				return errors.New("浏览器执行失败")
			}
			if response.Result.Exception != nil {
				return errors.New(strings.TrimPrefix(strings.SplitN(response.Result.Exception.Exception.Description, "\n", 2)[0], "Error: "))
			}
			return json.Unmarshal(response.Result.Result.Value, out)
		}
	}
	for {
		var state struct {
			Ready bool   `json:"ready"`
			URL   string `json:"url"`
		}
		if err := evaluate("({ready:['https://open.feishu.cn','https://open.larkoffice.com'].includes(location.origin) && Boolean(window.csrfToken && window.user && window.user.id),url:location.origin+location.pathname})", &state); err == nil {
			if state.Ready {
				break
			}
			if err := reportLogin(state.URL); err != nil {
				return result, err
			}
		}
		if err := wait(); err != nil {
			return result, err
		}
	}
	stage, message := "creating", "登录完成，正在创建应用…"
	if mode == "list" {
		stage, message = "listing", "登录完成，正在读取已有应用…"
	}
	if mode == "connect" {
		stage, message = "verifying", "登录完成，正在验证已有应用…"
	}
	httpapi.ReportBotCreationProgress(ctx, stage, message, "")
	if err := connection.WriteJSON(map[string]any{"id": -1, "method": "Runtime.addBinding", "params": map[string]string{"name": "irisSetupProgress"}}); err != nil {
		return result, err
	}
	encodedName, _ := json.Marshal(name)
	encodedMode, _ := json.Marshal(mode)
	encodedID, _ := json.Marshal(appID)
	err = evaluate(feishuSetupScript+"\nirisCreateFeishuApp("+string(encodedName)+","+string(encodedMode)+","+string(encodedID)+")", &result)
	return result, err
}

func feishuChromeArgs(profile string, headless bool) []string {
	// Chrome session cookies need session restoration, not just a retained directory.
	args := []string{"--user-data-dir=" + profile, "--restore-last-session", "--remote-debugging-address=127.0.0.1", "--remote-debugging-port=0", "--no-first-run", "--no-default-browser-check", "https://open.feishu.cn/app"}
	if headless {
		args = append([]string{"--headless=new"}, args...)
	}
	return args
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
