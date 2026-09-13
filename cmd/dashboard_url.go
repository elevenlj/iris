package main

import (
	"net"
	"strings"
)

func dashboardURLForConfig(cfg Config) string {
	if strings.TrimSpace(cfg.DashboardURL) != "" {
		return cfg.DashboardURL
	}
	return detectedDashboardURL(cfg)
}

func detectedDashboardURL(cfg Config) string {
	port := cfg.Port
	if port == "" {
		port = "8080"
	}
	interfaces, _ := net.Interfaces()
	var ipv6 net.IP
	// ponytail: choose the first usable IPv4 on multi-homed hosts; explicit
	// dashboard_url remains available when that interface is not reachable.
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
