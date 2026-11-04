//go:build windows

package tools

import (
	"os"
	"sync"
	"testing"
)

type countingProber struct {
	processProber
	mu    sync.Mutex
	calls []int
}

func (c *countingProber) Zombie(pid int) bool {
	c.mu.Lock()
	c.calls = append(c.calls, pid)
	c.mu.Unlock()
	return c.processProber.Zombie(pid)
}

func TestWindowsStrictBranchSkipsStatus(t *testing.T) {
	counting := &countingProber{processProber: osProcessProber{}}
	prev := identityProber
	identityProber = counting
	t.Cleanup(func() { identityProber = prev })
	pid := os.Getpid()
	birth, err := ProcessBirth(pid)
	if err != nil {
		t.Fatal(err)
	}
	alive, known := pidAliveMatches(pid, &birth, true, nil)
	if !alive || !known || len(counting.calls) != 0 {
		t.Fatalf("alive=%v known=%v probes=%v", alive, known, counting.calls)
	}
}

func TestWindowsNeverReleases(t *testing.T) {
	calls := 0
	prev := releaseGroupsFn
	releaseGroupsFn = func([]int) { calls++ }
	t.Cleanup(func() { releaseGroupsFn = prev })
	if ReleaseProcessGroupOnceEmpty(os.Getpid(), nil, 0) || calls != 0 {
		t.Fatal("a Windows pid was released as a group")
	}
}
