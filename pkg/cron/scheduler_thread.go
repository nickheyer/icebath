package cron

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Thread shaped handle over the in process ticker whose RestartIfDead respawns a ticker that crashed
type SupervisedTickerThread struct {
	target   func()
	stop     <-chan struct{}
	name     string
	log      *slog.Logger
	mu       sync.Mutex
	crashed  bool
	started  bool
	done     chan struct{}
	restarts int
}

// Supervisor for target with stop as its stop event, the thread named cron-scheduler when name is empty
func NewSupervisedTickerThread(target func(), stop <-chan struct{}, name string) *SupervisedTickerThread {
	if name == "" {
		name = "cron-scheduler"
	}
	return &SupervisedTickerThread{target: target, stop: stop, name: name, log: slog.Default()}
}

// Runs target once, marking the ticker crashed and printing the escaping panic as threading.excepthook does
func (t *SupervisedTickerThread) run(done chan struct{}) {
	defer close(done)
	defer func() {
		if r := recover(); r != nil {
			t.mu.Lock()
			t.crashed = true
			t.mu.Unlock()
			fmt.Fprintf(os.Stderr, "Exception in thread %s:\n%v\n%s", t.name, r, debug.Stack())
		}
	}()
	t.target()
}

// Launches a fresh ticker goroutine with the crashed flag cleared, the caller holding the lock
func (t *SupervisedTickerThread) spawnLocked() {
	t.crashed = false
	t.done = make(chan struct{})
	go t.run(t.done)
}

// Starts the ticker, an error when this handle was already started
func (t *SupervisedTickerThread) Start() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started {
		return errors.New("threads can only be started once")
	}
	t.started = true
	t.spawnLocked()
	return nil
}

// Whether the current ticker goroutine is running
func (t *SupervisedTickerThread) IsAlive() bool {
	t.mu.Lock()
	done := t.done
	t.mu.Unlock()
	if done == nil {
		return false
	}
	select {
	case <-done:
		return false
	default:
		return true
	}
}

// Waits for the current ticker to end, at most timeout when given, an error when never started
func (t *SupervisedTickerThread) Join(timeout *time.Duration) error {
	t.mu.Lock()
	done := t.done
	t.mu.Unlock()
	if done == nil {
		return errors.New("cannot join thread before it is started")
	}
	if timeout == nil {
		<-done
		return nil
	}
	timer := time.NewTimer(*timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
	return nil
}

// Respawns issued so far
func (t *SupervisedTickerThread) Restarts() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.restarts
}

// Respawns a ticker that crashed without a stop request, true when a restart happened
func (t *SupervisedTickerThread) RestartIfDead() bool {
	select {
	case <-t.stop:
		return false
	default:
	}
	if t.IsAlive() {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.crashed {
		return false
	}
	t.restarts++
	t.log.Error(fmt.Sprintf("Cron ticker thread %s died without a stop request; restarting (restart #%d). Scheduled jobs did not fire while it was down \u2014 check errors.log for the escaping exception.", pyrepr.StrRepr(t.name), t.restarts))
	t.spawnLocked()
	return true
}
