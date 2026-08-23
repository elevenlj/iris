package session

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/creack/pty"
)

const (
	defaultTerminalCols uint16 = 120
	defaultTerminalRows uint16 = 36
)

type Terminal interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
	Resize(cols, rows uint16) error
}

type ForegroundProcessController interface {
	TerminateForegroundProcess(context.Context) error
}

var errForegroundProcessControlUnavailable = errors.New("foreground process control unavailable")

type Launcher interface {
	Launch(ctx context.Context) (ProcessHandle, error)
}

type ProcessHandle interface {
	Terminal() Terminal
	Process() Waiter
}

type Waiter interface {
	Wait() error
}

type ShellLauncher struct {
	Command string
	Args    []string
	Dir     string
}

func (l ShellLauncher) Launch(ctx context.Context) (ProcessHandle, error) {
	command := l.Command
	if command == "" {
		command = defaultInteractiveShell()
	}
	args := l.Args
	if len(args) == 0 {
		args = interactiveShellArgs(command)
	}
	dir := l.Dir
	if dir == "" {
		dir = os.Getenv("TERMINAL_WORKING_DIR")
	}
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = dir
	cmd.Env = terminalEnvironment(os.Environ())
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: defaultTerminalRows, Cols: defaultTerminalCols})
	if err != nil {
		return nil, err
	}
	return &shellHandle{term: &ptyTerminal{file: f, shellPID: cmd.Process.Pid}, process: cmd}, nil
}

func defaultInteractiveShell() string {
	return resolveInteractiveShell(os.Getenv, exec.LookPath, runtime.GOOS)
}

func resolveInteractiveShell(getenv func(string) string, lookPath func(string) (string, error), goos string) string {
	candidates := make([]string, 0, 6)
	if shell := strings.TrimSpace(getenv("SHELL")); shell != "" {
		candidates = append(candidates, shell)
	}
	if goos == "windows" {
		if shell := strings.TrimSpace(getenv("COMSPEC")); shell != "" {
			candidates = append(candidates, shell)
		}
		candidates = append(candidates, "pwsh", "powershell", "cmd.exe")
	} else {
		candidates = append(candidates, "bash", "sh", "zsh", "fish")
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		if resolved, err := lookPath(candidate); err == nil && strings.TrimSpace(resolved) != "" {
			return resolved
		}
	}
	if goos == "windows" {
		return "cmd.exe"
	}
	return "/bin/sh"
}

func interactiveShellArgs(command string) []string {
	name := strings.ToLower(filepath.Base(strings.TrimSpace(command)))
	name = strings.TrimSuffix(name, ".exe")
	switch name {
	case "cmd":
		return nil
	case "powershell", "pwsh":
		return []string{"-NoLogo"}
	default:
		return []string{"-i"}
	}
}

func terminalEnvironment(current []string) []string {
	term := ""
	for _, entry := range current {
		if strings.HasPrefix(entry, "TERM=") {
			term = strings.TrimSpace(strings.TrimPrefix(entry, "TERM="))
			break
		}
	}
	if term != "" && !strings.EqualFold(term, "dumb") {
		return append([]string(nil), current...)
	}
	out := make([]string, 0, len(current)+1)
	for _, entry := range current {
		if !strings.HasPrefix(entry, "TERM=") {
			out = append(out, entry)
		}
	}
	return append(out, "TERM=xterm-256color")
}

type shellHandle struct {
	term    Terminal
	process *exec.Cmd
}

func (h *shellHandle) Terminal() Terminal { return h.term }
func (h *shellHandle) Process() Waiter    { return h.process }

type ptyTerminal struct {
	file     *os.File
	shellPID int
}

func (t *ptyTerminal) Read(p []byte) (int, error)  { return t.file.Read(p) }
func (t *ptyTerminal) Write(p []byte) (int, error) { return t.file.Write(p) }
func (t *ptyTerminal) Close() error                { return t.file.Close() }
func (t *ptyTerminal) Resize(cols, rows uint16) error {
	return pty.Setsize(t.file, &pty.Winsize{Cols: cols, Rows: rows})
}
func (t *ptyTerminal) TerminateForegroundProcess(ctx context.Context) error {
	return terminatePTYForegroundProcess(ctx, t.file, t.shellPID)
}

type ScreenLauncher struct {
	SessionName string
	Command     string
	Args        []string
	Dir         string
}

func (l ScreenLauncher) Launch(ctx context.Context) (ProcessHandle, error) {
	name := l.SessionName
	if name == "" {
		name = "easy-terminal"
	}
	command := l.Command
	if command == "" {
		command = defaultInteractiveShell()
	}
	args := []string{"-S", name, "-dm", command}
	shellArgs := l.Args
	if len(shellArgs) == 0 {
		shellArgs = interactiveShellArgs(command)
	}
	args = append(args, shellArgs...)
	cmd := exec.CommandContext(ctx, "screen", args...)
	if l.Dir != "" {
		cmd.Dir = l.Dir
	}
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return &screenHandle{term: screenTerminal{name: name}, process: screenWaiter{}}, nil
}

type screenHandle struct {
	term    Terminal
	process Waiter
}

func (h *screenHandle) Terminal() Terminal { return h.term }
func (h *screenHandle) Process() Waiter    { return h.process }

type screenTerminal struct {
	name string
}

func (t screenTerminal) Read([]byte) (int, error) {
	return 0, os.ErrInvalid
}

func (t screenTerminal) Write(p []byte) (int, error) {
	cmd := exec.Command("screen", "-S", t.name, "-X", "stuff", string(p))
	if err := cmd.Run(); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (t screenTerminal) Close() error {
	return exec.Command("screen", "-S", t.name, "-X", "quit").Run()
}

func (t screenTerminal) Resize(cols, rows uint16) error {
	return nil
}

type screenWaiter struct{}

func (screenWaiter) Wait() error { return nil }
