// Process groups, process table, identity and liveness, signals, tree kill, contained runs and owner death supervision
package proc

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

var (
	// The pid names no live process
	ErrNotFound = errors.New("no such process")
	// The operating system refused access to another process
	ErrAccessDenied = errors.New("process access denied")
	// A recorded pid now names a different process than the one recorded
	ErrReused = errors.New("process identity changed")
	// The tree was signalled but its listing could not be completed, so a descendant may have escaped
	ErrIncomplete = errors.New("process tree not fully captured")
)

// Wraps a permission failure so errors.Is matches both ErrAccessDenied and the system error
func denied(pid int, err error) error {
	return fmt.Errorf("pid %d: %w: %w", pid, ErrAccessDenied, err)
}

// Scheduling state of a process as the kernel reports it, zero when the state letter is unknown
type Status uint8

// Process states, each printing as the lower case word the dashboards show
const (
	Running Status = iota + 1
	Sleeping
	DiskSleep
	Stopped
	TracingStop
	Zombie
	Dead
	WakeKill
	Waking
	Parked
	Idle
)

var statusWords = [...]string{"?", "running", "sleeping", "disk-sleep", "stopped", "tracing-stop", "zombie", "dead", "wake-kill", "waking", "parked", "idle"}

// Lower case word for the state, a question mark when unknown
func (s Status) String() string {
	if int(s) < len(statusWords) {
		return statusWords[s]
	}
	return "?"
}

// True for a process that has exited and only waits for its parent to collect it
func (s Status) Exited() bool { return s == Zombie || s == Dead }

// Fires after the pause between liveness probes, growing from 10ms to half a second
func afterDelay(attempt int) <-chan time.Time {
	return time.After(min(10*time.Millisecond<<min(attempt, 6), 500*time.Millisecond))
}

func itoa(n int) string { return strconv.Itoa(n) }
