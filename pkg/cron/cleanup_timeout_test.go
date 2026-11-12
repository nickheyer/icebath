package cron

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

type finalizeCall struct {
	db                                     cronSessionDB
	agent                                  CronAgent
	jobID, jobName, cronSessionID, workdir string
}

type teardownCall struct {
	agent CronAgent
	jobID string
}

func TestDetachedWorkerTeardownWaitsForFuture(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	s, _ := newTestStore(t)
	future := newAgentFuture()
	fakeDB := &state.DB{}
	agent := &fakeCronAgent{}
	var finalized []finalizeCall
	var tornDown []teardownCall
	prevFinalize, prevTeardown := finalizeCronSessionSeam, teardownCronAgentSeam
	finalizeCronSessionSeam = func(_ *Store, _ context.Context, db cronSessionDB, a CronAgent, jobID, jobName, cronSessionID, workdir string) {
		finalized = append(finalized, finalizeCall{db, a, jobID, jobName, cronSessionID, workdir})
	}
	teardownCronAgentSeam = func(_ *Store, a CronAgent, jobID string, _ *float64) {
		tornDown = append(tornDown, teardownCall{a, jobID})
	}
	func() {
		defer func() { finalizeCronSessionSeam, teardownCronAgentSeam = prevFinalize, prevTeardown }()
		if !deferTeardownToRunningWorker(s, future, fakeDB, agent, "detached-worker", "detached worker", "cron_detached-worker", "") {
			t.Fatal("running worker did not take the teardown")
		}
		if len(finalized) != 0 || len(tornDown) != 0 {
			t.Fatalf("teardown ran before the future completed: %v %v", finalized, tornDown)
		}
		future.finish(map[string]any{"final_response": "late"}, nil)
		want := finalizeCall{fakeDB, agent, "detached-worker", "detached worker", "cron_detached-worker", ""}
		if len(finalized) != 1 || finalized[0] != want {
			t.Fatalf("finalize calls %v", finalized)
		}
		if len(tornDown) != 1 || tornDown[0] != (teardownCall{agent, "detached-worker"}) {
			t.Fatalf("teardown calls %v", tornDown)
		}
	}()
	if deferTeardownToRunningWorker(s, future, fakeDB, agent, "detached-worker", "detached worker", "cron_detached-worker", "") {
		t.Fatal("finished worker still took the teardown")
	}
}
