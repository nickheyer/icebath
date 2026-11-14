package cron

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// Polls predicate until it holds or ten seconds pass, returning its last answer
func waitUntilTrue(predicate func() bool) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return predicate()
}

// A ticker that ended with an escaping panic is respawned once per check while a stopped one is left down
func TestHousekeepingRestartsADeadTicker(t *testing.T) {
	var mu sync.Mutex
	starts := 0
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return starts
	}
	stop := make(chan struct{})
	ticker := NewSupervisedTickerThread(func() {
		mu.Lock()
		starts++
		mu.Unlock()
		panic("escaped")
	}, stop, "")
	logs := &bytes.Buffer{}
	ticker.log = slog.New(slog.NewTextHandler(logs, nil))
	if err := ticker.Start(); err != nil {
		t.Fatal(err)
	}
	if err := ticker.Start(); err == nil || err.Error() != "threads can only be started once" {
		t.Fatalf("second start %v", err)
	}
	waitUntilTrue(func() bool { return !ticker.IsAlive() })
	if !ticker.RestartIfDead() {
		t.Fatal("crashed ticker not restarted")
	}
	waitUntilTrue(func() bool { return count() == 2 && !ticker.IsAlive() })
	if count() != 2 || ticker.Restarts() != 1 {
		t.Fatalf("starts %d restarts %d", count(), ticker.Restarts())
	}
	if !strings.Contains(logs.String(), "Cron ticker thread 'cron-scheduler' died without a stop request; restarting (restart #1).") {
		t.Fatalf("log %s", logs.String())
	}
	close(stop)
	if ticker.RestartIfDead() || count() != 2 {
		t.Fatal("a stopped ticker must not be respawned")
	}
}

// A provider start that returns by design is not a dead ticker and never reruns
func TestSupervisorLeavesAReturningExternalProviderAlone(t *testing.T) {
	var mu sync.Mutex
	starts := 0
	stop := make(chan struct{})
	ticker := NewSupervisedTickerThread(func() {
		mu.Lock()
		starts++
		mu.Unlock()
	}, stop, "")
	if ticker.IsAlive() || ticker.RestartIfDead() {
		t.Fatal("an unstarted ticker reads as alive or restartable")
	}
	if err := ticker.Start(); err != nil {
		t.Fatal(err)
	}
	waitUntilTrue(func() bool { return !ticker.IsAlive() })
	ticker.Join(nil)
	mu.Lock()
	defer mu.Unlock()
	if ticker.RestartIfDead() || starts != 1 || ticker.Restarts() != 0 {
		t.Fatalf("starts %d restarts %d", starts, ticker.Restarts())
	}
}
