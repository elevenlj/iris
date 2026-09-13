package main

import (
	"net"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/elevenlj/iris/internal/session"
)

func TestDashboardAddressAutoDetectionPersistsAndUpdatesAllBots(t *testing.T) {
	cfg := defaultConfig()
	root := session.NewManager(nil, nil, session.WithAgentTurnHookURL("http://127.0.0.1:8080"))
	other := session.NewManager(nil, nil, session.WithAgentTurnHookURL("http://127.0.0.1:8080/bots/other"))
	svc := &appConfigService{cfg: &cfg, path: filepath.Join(t.TempDir(), "config.json"), manager: root}
	svc.bots = &botService{runtimes: map[string]*botRuntime{"other": {manager: other}}}
	want := "https://dev.example.com:9443"
	if err := svc.ObserveDashboardURL(want); err != nil {
		t.Fatal(err)
	}
	for _, local := range []string{"http://localhost:8080", "http://localhost.:8080", "http://127.0.0.1:8080", "http://[::1]:8080", "http://0.0.0.0:8080", "http://[::]:8080"} {
		if err := svc.ObserveDashboardURL(local); err != nil {
			t.Fatal(err)
		}
	}
	if root.DashboardURL() != want || other.DashboardURL() != want+"/bots/other" || other.AgentTurnHookURL() != "http://127.0.0.1:8080/bots/other" {
		t.Fatalf("wrong public/private routing: %q / %q / %q", root.DashboardURL(), other.DashboardURL(), other.AgentTurnHookURL())
	}
	reloaded := loadConfig(svc.path)
	if reloaded.DetectedDashboardURL != want || dashboardURLForConfig(reloaded) != want || reloaded.DashboardURL != "" {
		t.Fatal("detected address did not survive restart or overwrote manual configuration")
	}
	cfg.DashboardURL = "https://manual.example.com"
	if err := svc.ObserveDashboardURL("https://new-dev.example.com"); err != nil {
		t.Fatal(err)
	}
	if root.DashboardURL() != cfg.DashboardURL || other.DashboardURL() != cfg.DashboardURL+"/bots/other" {
		t.Fatal("automatic detection overrode explicit configuration")
	}
	if err := svc.ObserveDashboardURL("javascript:alert(1)"); err == nil {
		t.Fatal("unsafe automatic address accepted")
	}
}

func TestDashboardMachineAddressFallback(t *testing.T) {
	raw := detectedDashboardURL(Config{Port: "8088"})
	if raw == "" {
		t.Skip("no usable network interface on this machine")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "http" || u.Port() != "8088" || ip == nil || ip.IsLoopback() || !ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast() {
		t.Fatalf("machine address must be a usable non-loopback IP: %q", raw)
	}
}
