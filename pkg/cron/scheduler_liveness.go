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

// True once idle time crosses the limit, false when stopped or the run finished first
func inactivityWatchdogLoop(getIdleSeconds func() (float64, error), limitS float64, poll time.Duration, stop <-chan struct{}, futureDone func() bool, meter *state.AwakeIdleMeter) bool {
	if meter == nil {
		meter = state.NewAwakeIdleMeter()
	}
	for {
		select {
		case <-stop:
			return false
		case <-time.After(poll):
		}
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
	if _, err := openExecutionsLedger(p.s).TouchProgress(ctx, p.executionID); err != nil {
		p.s.log.Debug(fmt.Sprintf("Job '%s': execution progress stamp failed", p.jobName), "error", err)
	}
}
