//go:build !windows

package session

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPTYForegroundTerminationEscalatesAndReturnsControlToShell(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handle, err := (ShellLauncher{Command: bash, Args: []string{"--noprofile", "--norc", "-i"}, Dir: t.TempDir()}).Launch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	terminal, ok := handle.Terminal().(*ptyTerminal)
	if !ok {
		t.Fatalf("terminal = %T", handle.Terminal())
	}
	defer terminal.Close()
	shellGroup, err := unix.Getpgid(terminal.shellPID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.Write([]byte("sh -c 'trap \"\" INT TERM; while :; do :; done'\r")); err != nil {
		t.Fatal(err)
	}
	agentGroup := waitForDifferentForegroundGroup(t, terminal, shellGroup)
	t.Cleanup(func() {
		_ = unix.Kill(-agentGroup, unix.SIGKILL)
	})
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer stopCancel()
	started := time.Now()
	if err := terminal.TerminateForegroundProcess(stopCtx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 3*time.Second {
		t.Fatalf("signal-ignoring Agent should require escalation, elapsed=%s", elapsed)
	}
	foregroundGroup, err := unix.IoctlGetInt(int(terminal.file.Fd()), unix.TIOCGPGRP)
	if err != nil {
		t.Fatal(err)
	}
	if foregroundGroup != shellGroup {
		t.Fatalf("foreground group = %d, want shell group %d", foregroundGroup, shellGroup)
	}
}

func waitForDifferentForegroundGroup(t *testing.T, terminal *ptyTerminal, shellGroup int) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		foregroundGroup, err := unix.IoctlGetInt(int(terminal.file.Fd()), unix.TIOCGPGRP)
		if err == nil && foregroundGroup > 0 && foregroundGroup != shellGroup {
			return foregroundGroup
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Agent never became the PTY foreground process group")
	return 0
}
