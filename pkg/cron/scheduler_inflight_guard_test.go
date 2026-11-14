package cron

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Store with an empty registry and a 30 minute floor, every claim it seeds removed on cleanup
func sweepStore(t *testing.T) *Store {
	t.Helper()
	s, _ := newTestStore(t)
	s.loadConfig = func() (config.Tree, error) { return config.Tree{}, nil }
	t.Cleanup(func() {
		inflight.Lock()
		for k := range inflight.running {
			if k.Home == home.Key(s.home) {
				delete(inflight.running, k)
				delete(inflight.since, k)
				delete(inflight.futures, k)
			}
		}
		inflightGuard.forcedReleaseCount = 0
		inflightGuard.forcedReleases = nil
		inflight.Unlock()
	})
	return s
}

func seedClaim(s *Store, jobID string, ageSeconds float64, fut *poolFuture, withSince bool) inflightKey {
	k := s.inflightKey(jobID, s.home)
	inflight.Lock()
	inflight.running[k] = true
	if withSince {
		inflight.since[k] = inflightEpoch() - ageSeconds
	}
	if fut != nil {
		inflight.futures[k] = fut
	}
	inflight.Unlock()
	return k
}

func sweepIntervalJob(id string, minutes int) *Job {
	return &Job{ID: id, Name: id, Schedule: &Schedule{Kind: "interval", Minutes: minutes, HasMinutes: true}}
}

func TestStaleInflightSweep(t *testing.T) {
	t.Run("allowance is at least two intervals", func(t *testing.T) {
		for _, j := range []*Job{sweepIntervalJob("j1", 360), sweepIntervalJob("guide-curator", 4320)} {
			s := sweepStore(t)
			seedClaim(s, j.ID, 4*3600, nil, true)
			if got := s.sweepStaleInflight([]*Job{j}); len(got) != 0 || !isJobRunning(s, j.ID) {
				t.Fatal(j.ID, got)
			}
		}
	})
	t.Run("cron allowance not clipped to floor", func(t *testing.T) {
		s := sweepStore(t)
		j := &Job{ID: "weekly", Schedule: &Schedule{Kind: "cron", Expr: "0 9 * * 1"}}
		seedClaim(s, j.ID, 24*3600, nil, true)
		if got := s.sweepStaleInflight([]*Job{j}); len(got) != 0 {
			t.Fatal(got)
		}
	})
	t.Run("live future is never released", func(t *testing.T) {
		s := sweepStore(t)
		j := sweepIntervalJob("j1", 60)
		fut := &poolFuture{done: make(chan struct{})}
		seedClaim(s, j.ID, 10*3600, fut, true)
		if got := s.sweepStaleInflight([]*Job{j}); len(got) != 0 {
			t.Fatal(got)
		}
		close(fut.done)
		if got := s.sweepStaleInflight([]*Job{j}); len(got) != 1 || got[0] != "j1" {
			t.Fatal(got)
		}
	})
	t.Run("pending sentinel released when submit hung and young one kept", func(t *testing.T) {
		s := sweepStore(t)
		created, err := s.createJob(context.Background(), CreateJobOptions{Prompt: strp("p"), Schedule: "every 60m", Name: strp("hung")})
		if err != nil {
			t.Fatal(err)
		}
		seedClaim(s, created.ID, 5*3600, futurePending, true)
		seedClaim(s, "young", 60, futurePending, true)
		got := s.sweepStaleInflight([]*Job{created, sweepIntervalJob("young", 60)})
		if len(got) != 1 || got[0] != created.ID || !isJobRunning(s, "young") {
			t.Fatal(got)
		}
		after, _ := s.getJob(context.Background(), created.ID)
		if after == nil || after.LastStatus == nil || *after.LastStatus != "error" {
			t.Fatal("forced release must fail the run", after)
		}
	})
	t.Run("finite repeat job released without mark job run", func(t *testing.T) {
		s := sweepStore(t)
		created, err := s.createJob(context.Background(), CreateJobOptions{Prompt: strp("p"), Schedule: "every 60m", Name: strp("finite"), Repeat: 1})
		if err != nil {
			t.Fatal(err)
		}
		seedClaim(s, created.ID, 5*3600, nil, true)
		if got := s.sweepStaleInflight([]*Job{created}); len(got) != 1 || isJobRunning(s, created.ID) {
			t.Fatal(got)
		}
		after, _ := s.getJob(context.Background(), created.ID)
		if after.LastStatus != nil {
			t.Fatal("finite repeat job was marked")
		}
		if st := getInflightGuardStats(); st.ForcedReleases != 1 || len(st.RecentForcedReleases) != 1 {
			t.Fatal(st)
		}
	})
	t.Run("claim without timestamp is adopted then swept", func(t *testing.T) {
		s := sweepStore(t)
		j := sweepIntervalJob("j1", 60)
		k := seedClaim(s, j.ID, 0, nil, false)
		if got := s.sweepStaleInflight([]*Job{j}); len(got) != 0 {
			t.Fatal(got)
		}
		inflight.Lock()
		_, adopted := inflight.since[k]
		inflight.since[k] -= 5 * 3600
		inflight.Unlock()
		if !adopted {
			t.Fatal("claim not adopted")
		}
		if got := s.sweepStaleInflight([]*Job{j}); len(got) != 1 {
			t.Fatal(got)
		}
	})
}

func TestInterruptedFlags(t *testing.T) {
	s := sweepStore(t)
	k := seedClaim(s, "j-int", 0, nil, true)
	owner := "owner-1"
	inflight.Lock()
	inflight.fireOwners[k] = map[any]fireOwnerEntry{"tok": {Owner: &owner, Home: s.home}}
	inflight.Unlock()
	t.Cleanup(func() {
		inflight.Lock()
		delete(inflight.fireOwners, k)
		inflightGuard.interrupted = map[any]bool{}
		inflight.Unlock()
	})
	if got := markRunningJobsInterrupted(context.Background(), "Interrupted", map[OwnerPair]bool{{"j-int", "other"}: true}); len(got) != 0 {
		t.Fatal(got)
	}
	markRunningJobsInterrupted(context.Background(), "Interrupted", nil)
	if !s.isInterrupted("j-int", "tok") || s.isInterrupted("j-int", "other-token") {
		t.Fatal("token scope")
	}
	if !s.consumeInterruptedFlag("j-int", "tok") || s.consumeInterruptedFlag("j-int", "tok") {
		t.Fatal("consume")
	}
}
