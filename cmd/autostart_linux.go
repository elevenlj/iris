//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func autoStartDefinitionPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", "iris.service"), nil
}

func installAutoStart(spec autoStartSpec) error {
	path, err := autoStartDefinitionPath()
	if err != nil {
		return err
	}
	args := []string{spec.Binary, "--no-open", "--port", spec.Port, "--config-dir", spec.ConfigDir}
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, systemdQuote(arg))
	}
	unit := fmt.Sprintf(`[Unit]
Description=Iris local AI assistant

[Service]
Type=simple
Environment=%s
ExecStart=%s

[Install]
WantedBy=default.target
`, systemdQuote("IRIS_HOME="+spec.DataDir), strings.Join(quoted, " "))
	if err := writeFileAtomic(path, []byte(unit), 0o644); err != nil {
		return err
	}
	if output, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if output, err := exec.Command("systemctl", "--user", "enable", "iris.service").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl enable: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func systemdQuote(value string) string {
	value = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`).Replace(value)
	return `"` + value + `"`
}

func removeAutoStart() error {
	path, err := autoStartDefinitionPath()
	if err != nil {
		return err
	}
	_, _ = exec.Command("systemctl", "--user", "disable", "iris.service").CombinedOutput()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	_, _ = exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput()
	return nil
}
