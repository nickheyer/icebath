package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/nickheyer/icebath/internal/proc"
)

func signalQuietly(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
		_ = p.Release()
	}
}

func terminateTree(ctx context.Context, pid int, _ time.Duration) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	proc.Hide(cmd)
	err := cmd.Run()
	var exit *exec.ExitError
	if err == nil || errors.As(err, &exit) && ctx.Err() == nil {
		return
	}
	signalQuietly(pid)
}
