package cron

import "github.com/nickheyer/icebath/pkg/home"

// Registers a run of the job in the home as in flight without running it, as a fire does first
func HoldInflightRun(jobID, h string) {
	inflight.Lock()
	defer inflight.Unlock()
	inflight.running[inflightKey{Home: home.Key(h), JobID: jobID}] = true
}
