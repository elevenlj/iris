//go:build windows

package main

import (
	"os"
	"os/exec"
)

func interruptSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

func configureDetachedCommand(cmd *exec.Cmd) {
}

func terminateHeadlessProcess(cmd *exec.Cmd, done <-chan struct{}) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
