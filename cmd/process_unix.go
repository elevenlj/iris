//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

func interruptSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}

func configureDetachedCommand(cmd *exec.Cmd) {
	if cmd != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
}

func terminateHeadlessProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if pid <= 0 {
		return
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = cmd.Process.Kill()
		return
	}
	go func() {
		time.Sleep(2 * time.Second)
		if cmd.ProcessState == nil {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}()
}
