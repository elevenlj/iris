//go:build windows

package session

import (
	"context"
	"os"
)

func terminatePTYForegroundProcess(context.Context, *os.File, int) error {
	return errForegroundProcessControlUnavailable
}
