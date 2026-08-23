//go:build !windows

package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const foregroundProcessPollInterval = 40 * time.Millisecond

type processTerminationStage struct {
	signal unix.Signal
	wait   time.Duration
}

var foregroundProcessTerminationStages = []processTerminationStage{
	{signal: unix.SIGINT, wait: 2 * time.Second},
	{signal: unix.SIGTERM, wait: 1500 * time.Millisecond},
	{signal: unix.SIGKILL, wait: time.Second},
}

func terminatePTYForegroundProcess(ctx context.Context, file *os.File, shellPID int) error {
	if file == nil || shellPID <= 0 {
		return errors.New("终端进程信息不可用")
	}
	shellGroup, err := unix.Getpgid(shellPID)
	if err != nil || shellGroup <= 0 {
		return fmt.Errorf("无法确认 Shell 进程组: %w", err)
	}
	foregroundGroup, err := unix.IoctlGetInt(int(file.Fd()), unix.TIOCGPGRP)
	if err != nil {
		return fmt.Errorf("无法读取终端前台进程组: %w", err)
	}
	if foregroundGroup <= 0 || foregroundGroup == shellGroup {
		return nil
	}
	for _, stage := range foregroundProcessTerminationStages {
		if err := signalProcessGroup(foregroundGroup, stage.signal); err != nil {
			return err
		}
		stopped, err := waitForForegroundProcessExit(ctx, file, shellGroup, foregroundGroup, stage.wait)
		if err != nil {
			return err
		}
		if stopped {
			return nil
		}
	}
	return errors.New("Agent 进程未完全退出，已取消重启")
}

func signalProcessGroup(processGroup int, signal unix.Signal) error {
	if processGroup <= 0 {
		return errors.New("Agent 进程组无效")
	}
	err := unix.Kill(-processGroup, signal)
	if err == nil || errors.Is(err, unix.ESRCH) {
		return nil
	}
	return fmt.Errorf("终止 Agent 进程失败: %w", err)
}

func waitForForegroundProcessExit(ctx context.Context, file *os.File, shellGroup, agentGroup int, timeout time.Duration) (bool, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(foregroundProcessPollInterval)
	defer ticker.Stop()
	for {
		returned, err := foregroundControlReturnedToShell(file, shellGroup, agentGroup)
		if err != nil {
			return false, err
		}
		if returned {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline.C:
			return false, nil
		case <-ticker.C:
		}
	}
}

func foregroundControlReturnedToShell(file *os.File, shellGroup, agentGroup int) (bool, error) {
	foregroundGroup, err := unix.IoctlGetInt(int(file.Fd()), unix.TIOCGPGRP)
	if err != nil {
		return false, fmt.Errorf("无法确认 Agent 退出状态: %w", err)
	}
	if foregroundGroup != shellGroup {
		return false, nil
	}
	err = unix.Kill(-agentGroup, 0)
	return errors.Is(err, unix.ESRCH), nil
}
