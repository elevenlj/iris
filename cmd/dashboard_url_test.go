package main

import (
	"encoding/json"
	"net"
	"net/url"
	"testing"
)

func TestDashboardAddressIgnoresPreviouslyRememberedBrowser(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"port":"8088","detected_dashboard_url":"https://old-browser.example"}`), &cfg); err != nil {
		t.Fatal(err)
	}
	got := dashboardURLForConfig(cfg)
	if got == "https://old-browser.example" || got != detectedDashboardURL(cfg) {
		t.Fatalf("remembered browser address must not affect machine detection: %q", got)
	}
	encoded, _ := json.Marshal(cfg)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(encoded, &fields)
	if _, exists := fields["detected_dashboard_url"]; exists {
		t.Fatal("saved configuration must no longer retain a browser address")
	}
	cfg.DashboardURL = "https://manual.example.com"
	if dashboardURLForConfig(cfg) != cfg.DashboardURL {
		t.Fatal("manual override must still take priority")
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
