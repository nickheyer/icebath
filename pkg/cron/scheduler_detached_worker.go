package cron

import (
	"context"
	"os/exec"

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

// True when the agent goroutine still runs and will finalize the session and tear the agent down itself
func deferTeardownToRunningWorker(s *Store, future *agentFuture, sessionDB *state.DB, a CronAgent, jobID, jobName, cronSessionID, workdir string) bool {
	if future == nil || future.Done() {
		return false
	}
	future.addDoneCallback(func() {
		defer s.teardownCronAgent(a, jobID, nil)
		if sessionDB != nil {
			s.finalizeCronSession(context.Background(), sessionDB, a, jobID, jobName, cronSessionID, workdir)
		}
	})
	return true
}

// Waits on an external worker in the background so it never lingers as a zombie
func reapTerminalWorkerInBackground(cmd *exec.Cmd) {
	go func() { _ = cmd.Wait() }()
}
