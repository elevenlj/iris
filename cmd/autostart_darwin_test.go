//go:build darwin

package main

import (
	"os"
	"strings"
	"testing"
)

func TestAutoStartDoesNotSuppressDashboard(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := installAutoStart(autoStartSpec{Binary: "/tmp/iris", Port: "8081", ConfigDir: "/tmp/config", DataDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	path, err := autoStartDefinitionPath()
	if err != nil {
		t.Fatal(err)
	}
	plist, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plist), "--no-open") || !strings.Contains(string(plist), "<string>8081</string>") {
		t.Fatalf("incorrect autostart arguments: %s", plist)
	}
}
