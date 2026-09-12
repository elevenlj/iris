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
	"testing"
	"time"

	"github.com/elevenlj/iris/internal/httpapi"
	"github.com/gorilla/websocket"
)

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

func TestFeishuLoginProfileReusedAndIsolated(t *testing.T) {
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
