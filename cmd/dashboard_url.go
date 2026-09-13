package main

import (
	"net"
	"net/url"
	"strings"

	"github.com/elevenlj/iris/internal/session"
)

func dashboardURLForConfig(cfg Config) string {
	if strings.TrimSpace(cfg.DashboardURL) != "" {
		return cfg.DashboardURL
	}
	return detectedDashboardURL(cfg)
}

func detectedDashboardURL(cfg Config) string {
	if cfg.DetectedDashboardURL != "" {
		return cfg.DetectedDashboardURL
	}
	port := cfg.Port
	if port == "" {
		port = "8080"
	}
	interfaces, _ := net.Interfaces()
	var ipv6 net.IP
	// ponytail: on multi-homed hosts this is a best-effort fallback; the
	// authenticated browser origin supersedes it once the dashboard is opened.
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if ip.To4() != nil {
				return "http://" + net.JoinHostPort(ip.String(), port)
			}
			if ipv6 == nil {
				ipv6 = ip
			}
		}
	}
	if ipv6 != nil {
		return "http://" + net.JoinHostPort(ipv6.String(), port)
	}
	return ""
}

// Called only for authenticated, same-origin browser requests. Learning the
// browser's origin preserves TLS and public proxy ports without trusting
// arbitrary X-Forwarded-* headers or headless loopback requests.
func (s *appConfigService) ObserveDashboardURL(raw string) error {
	raw, err := session.NormalizeDashboardURL(raw)
	if err != nil || raw == "" {
		return err
	}
	u, _ := url.Parse(raw)
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	ip := net.ParseIP(host)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		(ip != nil && (!ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast())) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.DetectedDashboardURL == raw {
		return nil
	}
	cfg := *s.cfg
	cfg.DetectedDashboardURL = raw
	if err := writeConfigFile(s.path, cfg); err != nil {
		return err
	}
	*s.cfg = cfg
	base := dashboardURLForConfig(cfg)
	if err := s.manager.SetDashboardURL(base); err != nil {
		return err
	}
	if s.bots != nil {
		for _, rt := range s.bots.runtimes {
			if err := rt.manager.SetDashboardURL(base); err != nil {
				return err
			}
		}
	}
	return nil
}
