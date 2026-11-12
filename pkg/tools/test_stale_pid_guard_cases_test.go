package tools

import (
	"slices"
	"testing"
	"time"
)

// Retained handle double counting its kills
type retainedTreeKillChild struct {
	pid   int
	kills int
}

func (c *retainedTreeKillChild) Pid() int                    { return c.pid }
func (c *retainedTreeKillChild) Kill() error                 { c.kills++; return nil }
func (c *retainedTreeKillChild) WaitExit(time.Duration) bool { return true }

func TestKillProcessTreeRetainedHandleIsTaskkilledWithoutProbe(t *testing.T) {
	prevWindows, prevRun := isWindowsHost, legacyTaskkillRun
	t.Cleanup(func() { isWindowsHost, legacyTaskkillRun = prevWindows, prevRun })
	isWindowsHost = true
	var calls [][]string
	legacyTaskkillRun = func(argv []string) { calls = append(calls, argv) }
	legacyKillChildTree(&retainedTreeKillChild{pid: 4321})
	if len(calls) != 1 {
		t.Fatalf("taskkill runs %v, want exactly one", calls)
	}
	argv := calls[0]
	if argv[0] != "taskkill" || !slices.Contains(argv, "/PID") || !slices.Contains(argv, "4321") {
		t.Fatalf("taskkill argv %v", argv)
	}
}
