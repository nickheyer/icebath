package executil

import (
	"fmt"
	"time"
)

// Non-zero exit of a command run with RequireSuccess, carrying its exit code and both captured streams
type StatusError struct {
	Args     []string
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// The command and how it ended, as in command ["git" "fetch"]: exit status 128
func (e *StatusError) Error() string {
	return fmt.Sprintf("command %q: %s", e.Args, statusText(e.ExitCode))
}

// Command killed for outliving its timeout, carrying the timeout and the output read before the kill
type TimeoutError struct {
	Args    []string
	Timeout time.Duration
	Stdout  []byte
	Stderr  []byte
}

// The command and its timeout, as in command ["sleep" "5"] timed out after 1s
func (e *TimeoutError) Error() string {
	return fmt.Sprintf("command %q timed out after %s", e.Args, e.Timeout)
}
