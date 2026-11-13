package hostinfo

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// Output of a program found on PATH or at the first existing fixed path, ErrAbsent when none exists
func runProgram(ctx context.Context, name string, fixed []string, args ...string) (stdout, stderr []byte, err error) {
	path, err := exec.LookPath(name)
	if err != nil {
		path = ""
		for _, candidate := range fixed {
			if found, lookErr := exec.LookPath(candidate); lookErr == nil {
				path = found
				break
			}
		}
	}
	if path == "" {
		return nil, nil, fmt.Errorf("%s: %w", name, ErrAbsent)
	}
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	hideConsole(cmd)
	err = cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, nil, ctxErr
	}
	return out.Bytes(), errOut.Bytes(), err
}
