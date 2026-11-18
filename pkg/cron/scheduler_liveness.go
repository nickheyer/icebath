package cron

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

// Wall time of each execution's last progress verdict, read by cron status for owner fenced rows
var ownerProgress = struct {
	sync.Mutex
	at map[string]float64
}{at: map[string]float64{}}

// Event the watchdog waits on between polls, true once a stop was requested
type watchdogStop interface {
	Wait(timeout time.Duration) bool
}

// Stop event over a channel closed when the run ends
type chanStop <-chan struct{}

// Blocks up to timeout and reports whether the channel closed
func (c chanStop) Wait(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-c:
		return true
	case <-timer.C:
		return false
	}
}

// True once idle time crosses the limit, false when stopped or the run finished first
func inactivityWatchdogLoop(getIdleSeconds func() (float64, error), limitS float64, poll time.Duration, stop watchdogStop, futureDone func() bool, meter *state.AwakeIdleMeter) bool {
	if meter == nil {
		meter = state.NewAwakeIdleMeter()
	}
	for !stop.Wait(poll) {
		if futureDone() {
			return false
		}
		idle, err := getIdleSeconds()
		if err != nil {
			idle = 0
		}
		if meter.Measure(idle) >= limitS {
			return true
		}
	}
	return false
}

// Ledger stamp of a running attempt this process owns, the touch_execution_progress name liveness imports
var touchExecutionProgress = func(ctx context.Context, s *Store, executionID string) (bool, error) {
	return openExecutionsLedger(s).TouchProgress(ctx, executionID)
}

// Refreshes the execution's progress stamp while the agent keeps showing activity
type executionProgressStamper struct {
	s           *Store
	executionID string
	jobName     string
	idleSeconds func() float64
	every       float64
	last        float64
}

func newExecutionProgressStamper(s *Store, executionID, jobName string, idleSeconds func() float64, everySeconds float64) *executionProgressStamper {
	return &executionProgressStamper{s: s, executionID: executionID, jobName: jobName, idleSeconds: idleSeconds, every: everySeconds, last: monoNow()}
}

// Stamps progress once per cadence unless the agent has been idle for a whole cadence
func (p *executionProgressStamper) tick(ctx context.Context) {
	if p.executionID == "" {
		return
	}
	now := monoNow()
	if now-p.last < p.every {
		return
	}
	if p.idleSeconds() >= p.every {
		return
	}
	p.last = now
	ownerProgress.Lock()
	ownerProgress.at[p.executionID] = epochNow()
	ownerProgress.Unlock()
	if _, err := touchExecutionProgress(ctx, p.s, p.executionID); err != nil {
		p.s.log.Debug(fmt.Sprintf("Job '%s': execution progress stamp failed", p.jobName), "error", err)
	}
}
