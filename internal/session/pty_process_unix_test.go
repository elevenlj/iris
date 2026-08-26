//go:build !windows

package session

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
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
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	readyFile := t.TempDir() + "/ready"
	command := "IRIS_TEST_IGNORE_SIGNALS=1 IRIS_TEST_SIGNAL_READY=" + shellQuote(readyFile) + " " + shellQuote(testBinary) + " -test.run '^TestPTYSignalIgnoringHelper$'"
	if _, err := terminal.Write([]byte(command + "\r")); err != nil {
		t.Fatal(err)
	}
	agentGroup := waitForDifferentForegroundGroup(t, terminal, shellGroup)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("signal-ignoring helper did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
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

func TestPTYSignalIgnoringHelper(t *testing.T) {
	if os.Getenv("IRIS_TEST_IGNORE_SIGNALS") != "1" {
		return
	}
	signal.Ignore(unix.SIGINT, unix.SIGTERM)
	if err := os.WriteFile(os.Getenv("IRIS_TEST_SIGNAL_READY"), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {}
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
