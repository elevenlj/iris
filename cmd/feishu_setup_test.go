package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/elevenlj/iris/internal/httpapi"
)

// Explicit opt-in: opens an isolated login window and creates one real app.
func TestFeishuCreationLive(t *testing.T) {
	if os.Getenv("IRIS_FEISHU_LIVE_TEST") != "1" {
		t.Skip("requires user login and creates a real Feishu app")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	app, err := createFeishuApp(ctx, "Iris 验证")
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
