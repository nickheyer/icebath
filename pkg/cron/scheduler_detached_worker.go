package cron

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

// Closes a session store whose acquisition finished after the run gave up waiting for it
func closeLateSessionDBResult(result func() (*state.DB, error)) {
	defer func() { _ = recover() }()
	db, err := result()
	if err == nil && db != nil {
		state.ReleaseOrClose(db)
	}
}

// Session finalization and agent teardown the scheduler looks up at call time, swapped by tests
var (
	finalizeCronSessionSeam = (*Store).finalizeCronSession
	teardownCronAgentSeam   = (*Store).teardownCronAgent
)

// Teardowns that agent goroutines took over from runs that returned first, waited on by tests before the home is removed
var detachedTeardowns sync.WaitGroup

// True when the agent goroutine still runs and will finalize the session and tear the agent down itself
func deferTeardownToRunningWorker(s *Store, future *agentFuture, sessionDB *state.DB, a CronAgent, jobID, jobName, cronSessionID, workdir string) bool {
	if future == nil || future.Done() {
		return false
	}
	detachedTeardowns.Add(1)
	future.addDoneCallback(func() {
		defer detachedTeardowns.Done()
		defer teardownCronAgentSeam(s, a, jobID, nil)
		if sessionDB != nil {
			finalizeCronSessionSeam(s, context.Background(), sessionDB, a, jobID, jobName, cronSessionID, workdir)
		}
	})
	return true
}

// Holds the wait on a terminal worker still in teardown after the waiter returned, ending with its exit
func reapTerminalWorkerInBackground(proc workerProcess) {
	go func() { _, _ = proc.Wait(time.Duration(math.MaxInt64)) }()
}
