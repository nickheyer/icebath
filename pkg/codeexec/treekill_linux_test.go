//go:build linux

package codeexec

import (
	"slices"
	"syscall"
	"testing"
	"time"
)

type treeCall struct {
	pid int
	sig syscall.Signal
}

func TestEscalateWaitsThenSigkillsTree(t *testing.T) {
	var calls []treeCall
	var waits []time.Duration
	prevTree, prevWait := killProcessTree, waitExited
	killProcessTree = func(pid int, sig syscall.Signal) bool { calls = append(calls, treeCall{pid, sig}); return true }
	waitExited = func(_ <-chan struct{}, d time.Duration) bool { waits = append(waits, d); return false }
	t.Cleanup(func() { killProcessTree, waitExited = prevTree, prevWait })
	killProcessGroup(6666, func() error { return nil }, make(chan struct{}), true)
	if !slices.Equal(calls, []treeCall{{6666, syscall.SIGTERM}, {6666, syscall.SIGKILL}}) || !slices.Equal(waits, []time.Duration{5 * time.Second}) {
		t.Fatalf("calls %v waits %v", calls, waits)
	}
}
