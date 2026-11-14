package cron

import (
	"bytes"
	"context"
	"reflect"
	"testing"
)

func TestPausedCreationIsInertUntilOperatorAction(t *testing.T) {
	s, _ := newTestStore(t)
	var registered []string
	provider := &registeringProvider{name: "registering", register: func(job *Job) error {
		registered = append(registered, job.ID)
		return nil
	}}
	patchChronos(t, &schedulerResolver, func(*Store) CronScheduler { return provider })
	var writes [][]*Job
	patchChronos(t, &jobsPayloadStager, func(st *Store, rows []*Job) (string, error) {
		snapshot := make([]*Job, len(rows))
		for i, row := range rows {
			snapshot[i] = row.Clone()
		}
		writes = append(writes, snapshot)
		return st.stageJobsPayload(rows)
	})
	create := func(extra map[string]any) map[string]any {
		args := map[string]any{"action": "create", "schedule": "every 1h", "prompt": "canary"}
		for k, v := range extra {
			args[k] = v
		}
		return dispatchCronjob(t, s, args)
	}
	active := create(nil)
	if active["success"] != true || active["job"].(map[string]any)["enabled"] != true {
		t.Fatalf("active create %v", active)
	}
	paused := create(map[string]any{"paused": true})
	if paused["success"] != true {
		t.Fatalf("paused create %v", paused)
	}
	jobID := paused["job_id"].(string)
	var first *Job
	for _, rows := range writes {
		for _, row := range rows {
			if first == nil && row.ID == jobID {
				first = row
			}
		}
	}
	if first == nil || first.Enabled || first.State != "paused" || deref(first.PausedAt) == "" || deref(first.PausedReason) == "" || first.NextRunAt != nil {
		t.Fatalf("first write of the paused job %+v", first)
	}
	if !reflect.DeepEqual(registered, []string{active["job_id"].(string)}) {
		t.Fatalf("registered %v", registered)
	}
	if containsString(jobIDs(mustDue(t, s)), jobID) {
		t.Fatal("paused creation is due")
	}
	if _, won, err := s.claimJobForFire(context.Background(), jobID, FireClaimOptions{}); won || err != nil {
		t.Fatalf("paused creation claimed %v %v", won, err)
	}
	resumed, err := s.resumeJob(context.Background(), jobID)
	if err != nil || !resumed.Enabled || resumed.NextRunAt == nil || resumed.PausedReason != nil {
		t.Fatalf("resumed %+v %v", resumed, err)
	}
	forced := create(map[string]any{"paused": true, "paused_reason": "canary review"})
	if _, won, err := s.claimJobForFire(context.Background(), forced["job_id"].(string), FireClaimOptions{Force: true}); !won || err != nil {
		t.Fatalf("forced claim %v %v", won, err)
	}
}

func TestInvalidCreationIsRejectedWithoutWrites(t *testing.T) {
	s, _ := newTestStore(t)
	for _, flags := range []map[string]any{{"paused": "yes"}, {"paused": nil}, {"paused": 1},
		{"paused_reason": "orphan"}, {"paused": true, "paused_reason": 123}} {
		args := map[string]any{"action": "create", "schedule": "every 1h", "prompt": "invalid"}
		for k, v := range flags {
			args[k] = v
		}
		if result := dispatchCronjob(t, s, args); result["success"] != false {
			t.Fatalf("%v accepted %v", flags, result)
		}
		if jobs, _ := s.LoadJobs(context.Background()); len(jobs) != 0 {
			t.Fatalf("%v wrote %v", flags, jobIDs(jobs))
		}
	}
	var out bytes.Buffer
	reason := "orphan"
	code, err := s.cronCommand(context.Background(), &CronArgs{Command: "create", Schedule: prompt("every 1h"), Prompt: prompt("invalid"),
		PausedReason: &reason}, &out)
	if err != nil || code != 1 {
		t.Fatalf("cmd_cron create = %d %v %s", code, err, out.String())
	}
	if jobs, _ := s.LoadJobs(context.Background()); len(jobs) != 0 {
		t.Fatalf("cmd_cron wrote %v", jobIDs(jobs))
	}
}
