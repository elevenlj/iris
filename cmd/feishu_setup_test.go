package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elevenlj/iris/internal/httpapi"
	"github.com/gorilla/websocket"
)

// Exercise the production browser lifecycle without Feishu credentials or writes.
func TestFeishuBrowserLoginFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell launcher fixture")
	}
	dir := t.TempDir()
	launcher := filepath.Join(dir, "chrome")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexec \"$IRIS_BROWSER_TEST_BINARY\" -test.run=^TestFeishuFlowHelper$ -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHROME_BIN", launcher)
	t.Setenv("IRIS_BROWSER_TEST_BINARY", os.Args[0])
	t.Setenv("IRIS_BROWSER_FLOW_DIR", dir)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		app, err := runFeishuSetup(ctx, "", dir, "list", "")
		if err != nil || len(app.Apps) != 1 {
			t.Fatalf("launch %d: %+v, %v", i, app, err)
		}
	}
	launches, err := os.ReadFile(filepath.Join(dir, "launches"))
	if err != nil || string(launches) != "headless\nheaded\nheadless\n" {
		t.Fatalf("valid login must not open a second window: %q, %v", launches, err)
	}
	t.Setenv("IRIS_BROWSER_FLOW_NAV_ERROR", "1")
	if _, err := runFeishuSetup(ctx, "", dir, "list", ""); err == nil {
		t.Fatal("network failure reported as success")
	}
	launches, _ = os.ReadFile(filepath.Join(dir, "launches"))
	if string(launches) != "headless\nheaded\nheadless\nheadless\n" {
		t.Fatalf("network error must not trigger another login window: %q", launches)
	}
}

func TestFeishuFlowHelper(t *testing.T) {
	dir := os.Getenv("IRIS_BROWSER_FLOW_DIR")
	if dir == "" {
		return
	}
	headless, profile := false, ""
	for _, arg := range os.Args {
		if arg == "--headless=new" {
			headless = true
		}
		if strings.HasPrefix(arg, "--user-data-dir=") {
			profile = strings.TrimPrefix(arg, "--user-data-dir=")
		}
	}
	launch, _ := os.OpenFile(filepath.Join(dir, "launches"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if headless {
		_, _ = launch.WriteString("headless\n")
	} else {
		_, _ = launch.WriteString("headed\n")
	}
	_ = launch.Close()
	var server *httptest.Server
	var mu sync.Mutex
	closed, navigated := false, false
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json/list" {
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"id": "current", "type": "page", "url": "https://accounts.feishu.cn/accounts/page/login", "webSocketDebuggerUrl": strings.Replace(server.URL, "http", "ws", 1) + "/page"},
				{"id": "stale", "type": "page", "url": "https://accounts.feishu.cn/accounts/page/login"},
			})
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var request struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
				Params struct {
					Expression string `json:"expression"`
				} `json:"params"`
			}
			if conn.ReadJSON(&request) != nil {
				return
			}
			mu.Lock()
			result := any(map[string]any{})
			switch request.Method {
			case "Browser.close":
				os.Exit(0)
			case "Target.closeTarget":
				closed = true
			case "Page.navigate":
				navigated = true
				if os.Getenv("IRIS_BROWSER_FLOW_NAV_ERROR") == "1" {
					result = map[string]string{"errorText": "ERR_CONNECTION_REFUSED"}
				}
			case "Runtime.evaluate":
				if !closed || !navigated {
					os.Exit(2)
				}
				_, err := os.Stat(filepath.Join(dir, "logged-in"))
				ready := err == nil || !headless
				value := any(map[string]any{"ready": ready, "url": "https://accounts.feishu.cn/accounts/page/login"})
				if !headless {
					_ = os.WriteFile(filepath.Join(dir, "logged-in"), []byte("test"), 0600)
				}
				if strings.Contains(request.Params.Expression, "irisCreateFeishuApp") {
					value = map[string]any{"apps": []map[string]string{{"app_id": "cli_test", "name": "test"}}}
				}
				result = map[string]any{"result": map[string]any{"value": value}}
			}
			mu.Unlock()
			if conn.WriteJSON(map[string]any{"id": request.ID, "result": result}) != nil {
				return
			}
		}
	}))
	port := strings.TrimPrefix(server.URL, "http://127.0.0.1:")
	_ = os.WriteFile(filepath.Join(profile, "DevToolsActivePort"), []byte(port+"\n/devtools/browser/test"), 0600)
	select {}
}

// Explicit opt-in: opens an isolated login window and creates one real app.
func TestFeishuCreationLive(t *testing.T) {
	if os.Getenv("IRIS_FEISHU_LIVE_TEST") != "1" {
		t.Skip("requires user login and creates a real Feishu app")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	app, err := createFeishuApp(ctx, "Iris 验证", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Retain the new application's credentials privately so a later permission
	// failure does not force the developer to create another application.
	f, err := os.CreateTemp("", "iris-feishu-verified-*.json")
	if err != nil {
		t.Fatal(err)
	}
	err = json.NewEncoder(f).Encode(app)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("created app %s; private credential file: %s", app.AppID, f.Name())
	owner, err := httpapi.ResolveLarkOwner(ctx, app.AppID, app.AppSecret, app.OwnerEmail)
	if err != nil {
		t.Fatal(err)
	}
	result := httpapi.TestLarkConfig(httpapi.RuntimeConfig{LarkAppID: app.AppID, LarkAppSecret: app.AppSecret, LarkNotifyReceiveID: owner})
	if !result.OK {
		t.Fatalf("connection/permission verification failed: %+v", result.Steps)
	}
	t.Log("PASS: real app creation, permissions, publication, developer identity, test card and group-message permission")
}

// Read-only live check of Iris's real browser lifecycle; never creates an app.
func TestFeishuExistingAppLive(t *testing.T) {
	if os.Getenv("IRIS_FEISHU_READONLY_TEST") != "1" {
		t.Skip("requires an authenticated Iris browser profile")
	}
	dataDir, appID := os.Getenv("IRIS_FEISHU_TEST_DATA_DIR"), os.Getenv("IRIS_FEISHU_TEST_APP")
	if dataDir == "" || appID == "" {
		t.Fatal("explicit test data directory and existing app required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	listed, err := runFeishuSetup(ctx, "", dataDir, "list", "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, app := range listed.Apps {
		if app.AppID == appID {
			found = true
		}
	}
	if !found {
		t.Fatal("reported app missing from list")
	}
	// No headed fallback on the second launch: repeated login must fail this check.
	linked, err := runFeishuBrowser(ctx, "existing app verification", dataDir, "connect", appID, true)
	if err != nil {
		t.Fatal(err)
	}
	if linked.AppID != appID || linked.AppSecret == "" || linked.OwnerEmail == "" {
		t.Fatal("existing app connection data incomplete")
	}
	t.Log("PASS: Iris browser restarted without login, existing app listed, published state and credentials verified read-only")
}

func TestFeishuLoginProfileReusedAndIsolated(t *testing.T) {
	for _, headless := range []bool{true, false} {
		args := strings.Join(feishuChromeArgs("test-profile", headless), " ")
		if !strings.Contains(args, "--restore-last-session") {
			t.Fatal("retaining the directory alone loses session-only login cookies")
		}
		if strings.Contains(args, "--headless=new") != headless {
			t.Fatal("valid login must be checked without opening a window")
		}
	}
	base := t.TempDir()
	first, err := feishuLoginProfile(filepath.Join(base, "service-1"))
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(first, "login-state-test")
	if err := os.WriteFile(marker, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	endpoint := filepath.Join(first, "DevToolsActivePort")
	if err := os.WriteFile(endpoint, []byte("1234\n/devtools/browser/stale"), 0600); err != nil {
		t.Fatal(err)
	}
	again, err := feishuLoginProfile(filepath.Join(base, "service-1"))
	if err != nil || again != first {
		t.Fatalf("profile not reused: %q, %v", again, err)
	}
	if value, err := os.ReadFile(marker); err != nil || string(value) != "preserve" {
		t.Fatalf("login state lost: %q, %v", value, err)
	}
	if _, err := os.Stat(endpoint); !os.IsNotExist(err) {
		t.Fatalf("stale browser endpoint retained: %v", err)
	}
	other, err := feishuLoginProfile(filepath.Join(base, "service-2"))
	if err != nil || other == first {
		t.Fatalf("services share login profile: %q, %v", other, err)
	}
	info, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0700 {
		t.Fatalf("login directory is not private: %v", info.Mode())
	}
	if _, err := feishuLoginProfile(""); err == nil {
		t.Fatal("empty data directory accepted")
	}
}

func TestFeishuBrowserHelperProcess(t *testing.T) {
	if os.Getenv("IRIS_FEISHU_BROWSER_HELPER") != "1" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func TestFeishuBrowserClosesGracefully(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestFeishuBrowserHelperProcess$")
	command.Env = append(os.Environ(), "IRIS_FEISHU_BROWSER_HELPER=1")
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = command.Wait(); close(done) }()
	t.Cleanup(func() { _ = stdin.Close(); _ = command.Process.Kill(); <-done })
	closed := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var request struct{ Method string }
		if conn.ReadJSON(&request) == nil {
			closed <- request.Method
			_ = stdin.Close()
		}
	}))
	defer server.Close()
	profile := t.TempDir()
	port := strings.TrimPrefix(server.URL, "http://127.0.0.1:")
	if err := os.WriteFile(filepath.Join(profile, "DevToolsActivePort"), []byte(port+"\n/devtools/browser/test"), 0600); err != nil {
		t.Fatal(err)
	}
	closeFeishuBrowser(command, done, profile)
	select {
	case method := <-closed:
		if method != "Browser.close" || !command.ProcessState.Success() {
			t.Fatalf("browser killed without flushing login state: %q, %v", method, command.ProcessState)
		}
	default:
		t.Fatal("browser was not closed through its own endpoint")
	}
}
