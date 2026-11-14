package proc

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

func RunAttached(ctx context.Context, cmd *exec.Cmd) (Exit, error) {
	if err := ctx.Err(); err != nil {
		return Exit{}, err
	}
	if err := cmd.Start(); err != nil {
		return Exit{}, err
	}
	stop := context.AfterFunc(ctx, func() { _ = cmd.Process.Kill() })
	err := cmd.Wait()
	stop()
	exit := ExitOf(cmd.ProcessState)
	if cause := context.Cause(ctx); cause != nil {
		return exit, fmt.Errorf("command %q stopped: %w", strings.Join(cmd.Args, " "), cause)
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return exit, err
	}
	return exit, nil
}
