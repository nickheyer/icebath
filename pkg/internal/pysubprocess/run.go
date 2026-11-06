package pysubprocess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Failure subprocess.run raises with check=True when the command exits nonzero
type CalledProcessError struct {
	Cmd        []string
	Returncode int
	Stdout     []byte
	Stderr     []byte
}

func (e *CalledProcessError) Error() string {
	if e.Returncode < 0 {
		if name, ok := signalName(-e.Returncode); ok {
			return fmt.Sprintf("Command '%s' died with <Signals.%s: %d>.", cmdRepr(e.Cmd), name, -e.Returncode)
		}
		return fmt.Sprintf("Command '%s' died with unknown signal %d.", cmdRepr(e.Cmd), -e.Returncode)
	}
	return fmt.Sprintf("Command '%s' returned non-zero exit status %d.", cmdRepr(e.Cmd), e.Returncode)
}

// Failure subprocess.run raises after killing a command that outlived its timeout
type TimeoutExpired struct {
	Cmd     []string
	Timeout int
	Stdout  []byte
	Stderr  []byte
}

func (e *TimeoutExpired) Error() string {
	return fmt.Sprintf("Command '%s' timed out after %s seconds", cmdRepr(e.Cmd), strconv.Itoa(e.Timeout))
}

func cmdRepr(cmd []string) string {
	items := make([]any, len(cmd))
	for i, s := range cmd {
		items[i] = s
	}
	return pyrepr.Repr(items)
}

// Runs argv with captured output and a whole second timeout, failing like check=True on a nonzero exit
func Run(argv []string, timeoutSeconds int, check bool) (stdout, stderr []byte, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return out.Bytes(), errOut.Bytes(), &TimeoutExpired{Cmd: argv, Timeout: timeoutSeconds, Stdout: out.Bytes(), Stderr: errOut.Bytes()}
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		if !check {
			return out.Bytes(), errOut.Bytes(), nil
		}
		return out.Bytes(), errOut.Bytes(), &CalledProcessError{Cmd: argv, Returncode: returncode(exitErr), Stdout: out.Bytes(), Stderr: errOut.Bytes()}
	}
	return out.Bytes(), errOut.Bytes(), runErr
}
