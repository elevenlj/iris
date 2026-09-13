package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/elevenlj/iris/internal/httpapi"
)

func TestLarkSavedCredentialsIgnoreCallerEnvironment(t *testing.T) {
	t.Setenv("LARK_APP_ID", "caller-app")
	t.Setenv("LARK_APP_SECRET", "caller-secret")
	t.Setenv("LARK_NOTIFY_RECEIVE_ID", "caller-user")
	for _, tc := range []struct {
		name, config, app, secret, receiver string
	}{
		{"legacy", `{"lark_app_id":"saved-app","lark_app_secret":"saved-secret","lark_notify_receive_id":"saved-user"}`, "saved-app", "saved-secret", "saved-user"},
		{"partial", `{"lark_app_id":"saved-app"}`, "saved-app", "", ""},
		{"bootstrap", `{}`, "caller-app", "caller-secret", "caller-user"},
		{"default bot", `{"lark_app_id":"stale-app","lark_app_secret":"stale-secret","bots":[{"id":"default","app_id":"bot-app","app_secret":"bot-secret","receive_id":"bot-user"}]}`, "bot-app", "bot-secret", "bot-user"},
		{"no default bot", `{"lark_app_id":"stale-app","bots":[{"id":"bot-other","app_id":"other-app","app_secret":"other-secret"}]}`, "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.local.json")
			if err := os.WriteFile(path, []byte(tc.config), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := loadConfig(path)
			if cfg.LarkAppID != tc.app || cfg.LarkAppSecret != tc.secret || cfg.LarkNotifyReceiveID != tc.receiver {
				t.Fatal("credentials did not follow saved bot > saved legacy > first-use environment priority")
			}
			if err := writeConfigFile(path, cfg); err != nil {
				t.Fatal(err)
			}
			t.Setenv("LARK_APP_ID", "another-caller")
			t.Setenv("LARK_APP_SECRET", "another-secret")
			loaded := loadConfig(path)
			if loaded.LarkAppID != tc.app || loaded.LarkAppSecret != tc.secret {
				t.Fatal("saved credentials changed after reload")
			}
		})
	}
}

func TestRuntimeRestartFiltersOnlyLarkCredentials(t *testing.T) {
	for _, key := range []string{"LARK_APP_ID", "LARK_APP_SECRET", "LARK_NOTIFY_RECEIVE_ID"} {
		t.Setenv(key, "caller-value")
	}
	t.Setenv("IRIS_TEST_KEEP_ENV", "keep")
	t.Setenv("HTTPS_PROXY", "http://proxy.example:8080")
	cmd := runtimeProcessCommand(runtimeRecord{Executable: "/opt/iris", Port: "8088", ConfigDir: "/data/iris/conf"})
	if !slices.Equal(cmd.Args, []string{"/opt/iris", "--port", "8088", "--config-dir", "/data/iris/conf"}) {
		t.Fatal("restart changed target service")
	}
	for _, entry := range cmd.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key == "LARK_APP_ID" || key == "LARK_APP_SECRET" || key == "LARK_NOTIFY_RECEIVE_ID" {
			t.Fatal("restart inherited caller credentials")
		}
	}
	if !slices.Contains(cmd.Env, "IRIS_TEST_KEEP_ENV=keep") || !slices.Contains(cmd.Env, "HTTPS_PROXY=http://proxy.example:8080") {
		t.Fatal("restart lost unrelated environment or network configuration")
	}
}

func TestRuntimeStatusSeparatesServiceAndBots(t *testing.T) {
	var out bytes.Buffer
	printRuntimeStatus(&out, []runtimeRecord{{Port: "8080", Version: "test", Bots: []httpapi.BotConnectionStatus{
		{ID: "default", Name: "本地助手", Status: "connected"},
		{ID: "bot-other", Name: "另一个助手", Status: "reconnecting"},
	}}}, "8080")
	for _, want := range []string{"VERSION", "飞书连接", "本地助手\tconnected", "另一个助手\treconnecting"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in status: %s", want, out.String())
		}
	}
}
