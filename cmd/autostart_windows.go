//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"strings"
)

func installAutoStart(spec autoStartSpec) error {
	command := `cmd.exe /D /S /C "set \"IRIS_HOME=` + spec.DataDir + `\" && ` + windowsQuote(spec.Binary) + " --no-open --port " + windowsQuote(spec.Port) + " --config-dir " + windowsQuote(spec.ConfigDir) + `"`
	output, err := exec.Command("schtasks", "/Create", "/TN", "Iris", "/SC", "ONLOGON", "/TR", command, "/F").CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks create: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func removeAutoStart() error {
	if err := exec.Command("schtasks", "/Query", "/TN", "Iris").Run(); err != nil {
		return nil
	}
	output, err := exec.Command("schtasks", "/Delete", "/TN", "Iris", "/F").CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks delete: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func windowsQuote(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}
