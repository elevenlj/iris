//go:build darwin

package main

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
)

func autoStartDefinitionPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", "com.elevenlj.iris.plist"), nil
}

func installAutoStart(spec autoStartSpec) error {
	path, err := autoStartDefinitionPath()
	if err != nil {
		return err
	}
	logPath := filepath.Join(instanceDataDir(spec.DataDir, spec.Port), "log", "autostart.log")
	args := []string{spec.Binary, "--port", spec.Port, "--config-dir", spec.ConfigDir}
	argXML := ""
	for _, arg := range args {
		argXML += "\n      <string>" + html.EscapeString(arg) + "</string>"
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.elevenlj.iris</string>
  <key>ProgramArguments</key>
  <array>%s
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>IRIS_HOME</key>
    <string>%s</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, argXML, html.EscapeString(spec.DataDir), html.EscapeString(logPath), html.EscapeString(logPath))
	return writeFileAtomic(path, []byte(plist), 0o644)
}

func removeAutoStart() error {
	path, err := autoStartDefinitionPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
