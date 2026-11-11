package gateway

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// Hidden entry of the install binary that runs the Windows update with its output and exit code captured
const GatewayUpdateHelperEntry = "gateway-update-helper"

// Runs the update helper entry and exits when this process was launched as one
func DispatchUpdateHelperEntry(ctx context.Context) {
	if len(os.Args) > 1 && os.Args[1] == GatewayUpdateHelperEntry {
		os.Exit(UpdateHelperMain(ctx, os.Args[2:], os.Stderr))
	}
}

// Runs the command with both outputs in the output file, then writes its exit code through a temp file and rename
func UpdateHelperMain(ctx context.Context, args []string, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "IndexError: list index out of range")
		return 1
	}
	outputPath, exitCodePath, command := args[0], args[1], args[2:]
	if len(command) == 0 {
		fmt.Fprintln(stderr, "IndexError: list index out of range")
		return 1
	}
	out, err := os.Create(outputPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer out.Close()
	runCtx, cancel := context.WithTimeout(ctx, 3600*time.Second)
	defer cancel()
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Env = append(os.Environ(), "PYTHONUNBUFFERED=1")
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-runCtx.Done():
		fmt.Fprintf(stderr, "subprocess.TimeoutExpired: Command %q timed out after 3600 seconds\n", command)
		return 1
	}
	rc := 0
	if waitErr != nil {
		exitErr, ok := waitErr.(*exec.ExitError)
		if !ok {
			fmt.Fprintln(stderr, waitErr)
			return 1
		}
		rc = exitErr.ExitCode()
	}
	tmp := exitCodePath + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(rc)), 0o644); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := os.Rename(tmp, exitCodePath); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
