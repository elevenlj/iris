package session

import (
	"errors"
	"reflect"
	"testing"
)

func TestTerminalEnvironmentReplacesMissingOrDumbTerm(t *testing.T) {
	for _, input := range [][]string{{"PATH=/bin"}, {"PATH=/bin", "TERM=dumb"}} {
		env := terminalEnvironment(input)
		if got := environmentValue(env, "TERM"); got != "xterm-256color" {
			t.Fatalf("TERM = %q for %#v", got, input)
		}
	}
	input := []string{"PATH=/bin", "TERM=screen-256color"}
	if got := environmentValue(terminalEnvironment(input), "TERM"); got != "screen-256color" {
		t.Fatalf("existing capable TERM should be preserved, got %q", got)
	}
}

func environmentValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if len(entry) >= len(prefix) && entry[:len(prefix)] == prefix {
			return entry[len(prefix):]
		}
	}
	return ""
}

func TestResolveInteractiveShellDoesNotRequireZsh(t *testing.T) {
	tests := []struct {
		name      string
		goos      string
		env       map[string]string
		available map[string]string
		want      string
	}{
		{
			name: "configured shell", goos: "linux",
			env:       map[string]string{"SHELL": "/usr/local/bin/fish"},
			available: map[string]string{"/usr/local/bin/fish": "/usr/local/bin/fish"}, want: "/usr/local/bin/fish",
		},
		{
			name: "bash fallback without zsh", goos: "linux",
			available: map[string]string{"bash": "/usr/bin/bash", "sh": "/bin/sh"}, want: "/usr/bin/bash",
		},
		{
			name: "portable sh fallback", goos: "linux",
			available: map[string]string{"sh": "/bin/sh"}, want: "/bin/sh",
		},
		{
			name: "windows comspec", goos: "windows",
			env:       map[string]string{"COMSPEC": `C:\\Windows\\System32\\cmd.exe`},
			available: map[string]string{`C:\\Windows\\System32\\cmd.exe`: `C:\\Windows\\System32\\cmd.exe`}, want: `C:\\Windows\\System32\\cmd.exe`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(key string) string { return tt.env[key] }
			lookPath := func(command string) (string, error) {
				if path := tt.available[command]; path != "" {
					return path, nil
				}
				return "", errors.New("not found")
			}
			if got := resolveInteractiveShell(getenv, lookPath, tt.goos); got != tt.want {
				t.Fatalf("shell = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInteractiveShellArgsArePortable(t *testing.T) {
	for _, tt := range []struct {
		command string
		want    []string
	}{
		{"/bin/bash", []string{"-i"}},
		{"/usr/bin/fish", []string{"-i"}},
		{"pwsh.exe", []string{"-NoLogo"}},
		{"cmd.exe", nil},
	} {
		if got := interactiveShellArgs(tt.command); !reflect.DeepEqual(got, tt.want) {
			t.Fatalf("args for %s = %#v, want %#v", tt.command, got, tt.want)
		}
	}
}
