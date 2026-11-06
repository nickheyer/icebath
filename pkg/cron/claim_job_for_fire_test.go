package cron

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

func tryClaimFire(t *testing.T, s *Store, id string, o FireClaimOptions) (*Job, bool) {
	t.Helper()
	job, ok, err := s.claimJobForFire(context.Background(), id, o)
	if err != nil {
		t.Fatalf("claimJobForFire(%s): %v", id, err)
	}
	return job, ok
}

func setNow(s *Store, at time.Time) { s.clk.nowFn = func() time.Time { return at } }

func TestClaimSucceedsOnceThenBlocks(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 5m", Name: prompt("t")})
	before := deref(mustGet(t, s, job.ID).NextRunAt)
	if _, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{}); !ok {
		t.Fatal("first claim lost")
	}
	if _, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{}); ok {
		t.Fatal("second claim for the same fire won")
	}
	if deref(mustGet(t, s, job.ID).NextRunAt) == before {
		t.Fatal("claim did not advance next_run_at")
	}
}

func TestClaimOneshotCannotBeDoubleClaimed(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "in 30m", Name: prompt("o")})
	if _, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{}); !ok {
		t.Fatal("first claim lost")
	}
	if _, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{}); ok {
		t.Fatal("one shot claimed twice")
	}
}

func TestClaimUnknownAndPausedJobs(t *testing.T) {
	s, _ := newTestStore(t)
	if _, ok := tryClaimFire(t, s, "nope-does-not-exist", FireClaimOptions{}); ok {
		t.Fatal("unknown job claimed")
	}
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 5m", Name: prompt("p")})
	if _, err := s.pauseJob(job.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{}); ok {
		t.Fatal("paused job claimed")
	}
	if _, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{Manual: true}); ok {
		t.Fatal("manual claim resumed a paused job")
	}
	if mustGet(t, s, job.ID).PausedAt == nil {
		t.Fatal("manual claim cleared paused_at")
	}
}

func TestForcedClaimAtomicallyResumesPausedJob(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 5m", Name: prompt("manual")})
	if _, err := s.pauseJob(job.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{Force: true}); !ok {
		t.Fatal("forced claim lost")
	}
	claimed := mustGet(t, s, job.ID)
	if !claimed.Enabled || claimed.State != "scheduled" || claimed.PausedAt != nil || claimed.PausedReason != nil || claimed.FireClaim == nil {
		t.Fatalf("forced claim left %+v", claimed)
	}
}

func TestStaleClaimIsReclaimable(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 5m", Name: prompt("s")})
	if _, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{}); !ok {
		t.Fatal("first claim lost")
	}
	zero := 0.0
	if _, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{TTLSeconds: &zero}); !ok {
		t.Fatal("a zero TTL did not make the claim stale")
	}
}

func TestMarkJobRunClearsClaim(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 5m", Name: prompt("c")})
	if _, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{}); !ok || mustGet(t, s, job.ID).FireClaim == nil {
		t.Fatal("claim not stamped")
	}
	mustMark(t, s, job.ID, true, MarkOptions{})
	if mustGet(t, s, job.ID).FireClaim != nil {
		t.Fatal("mark_job_run kept the claim")
	}
	if _, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{}); !ok {
		t.Fatal("rearmed job not claimable")
	}
}

func TestFireClaimHeartbeatRefreshesOnlyExpectedOwner(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 5m", Name: prompt("heartbeat")})
	tryClaimFire(t, s, job.ID, FireClaimOptions{})
	claimed := *mustGet(t, s, job.ID).FireClaim
	setNow(s, parseInstant(t, &claimed.At).Add(30*time.Second))
	if ok, err := s.heartbeatFireClaim(job.ID, claimed.By); err != nil || !ok {
		t.Fatalf("owner heartbeat %v %v", ok, err)
	}
	refreshed := mustGet(t, s, job.ID).FireClaim
	if refreshed.At == claimed.At || refreshed.By != claimed.By {
		t.Fatalf("refreshed %+v from %+v", refreshed, claimed)
	}
	if ok, err := s.heartbeatFireClaim(job.ID, "replacement-owner"); err != nil || ok {
		t.Fatalf("foreign heartbeat %v %v", ok, err)
	}
}

func TestReclaimedFireUsesNewOwnerToken(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 5m", Name: prompt("reclaim")})
	tryClaimFire(t, s, job.ID, FireClaimOptions{})
	original := *mustGet(t, s, job.ID).FireClaim
	setNow(s, parseInstant(t, &original.At).Add(301*time.Second))
	if _, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{}); !ok {
		t.Fatal("expired claim not reclaimed")
	}
	replacement := *mustGet(t, s, job.ID).FireClaim
	if replacement.By == original.By {
		t.Fatal("reclaim reused the owner token")
	}
	if ok, _ := s.heartbeatFireClaim(job.ID, original.By); ok {
		t.Fatal("old owner refreshed the replacement claim")
	}
	if got := *mustGet(t, s, job.ID).FireClaim; got != replacement {
		t.Fatalf("claim changed to %+v", got)
	}
}

func TestStaleFireOwnerCannotMarkReplacementRun(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 5m", Name: prompt("fenced")})
	tryClaimFire(t, s, job.ID, FireClaimOptions{})
	original := *mustGet(t, s, job.ID).FireClaim
	editJobs(t, s, func(j *Job) { j.FireClaim = &Claim{At: original.At, By: "replacement"} })
	ok, err := s.markJobRun(context.Background(), job.ID, true, MarkOptions{ExpectedFireOwner: &original.By})
	if err != nil || ok {
		t.Fatalf("stale owner marked %v %v", ok, err)
	}
	persisted := mustGet(t, s, job.ID)
	if persisted.FireClaim.By != "replacement" || persisted.LastRunAt != nil {
		t.Fatalf("persisted %+v", persisted)
	}
}

func TestFireClaimFenceSerializesTerminalRevocation(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 5m", Name: prompt("fenced-side-effect")})
	claimed, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{})
	if !ok || claimed == nil {
		t.Fatal("claim lost")
	}
	owner := claimed.FireClaim.By
	var done atomic.Bool
	finished := make(chan struct{})
	err := s.fireClaimFence(context.Background(), job.ID, owner, func(ctx context.Context, owns bool) error {
		if !owns {
			return fmt.Errorf("owner does not own its claim")
		}
		go func() {
			s.markJobRun(context.Background(), job.ID, true, MarkOptions{ExpectedFireOwner: &owner})
			done.Store(true)
			close(finished)
		}()
		time.Sleep(50 * time.Millisecond)
		if done.Load() {
			return fmt.Errorf("terminal revocation ran inside the fence")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("terminal revocation never ran")
	}
}

func TestFireClaimFenceRejectsStaleOwner(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 5m", Name: prompt("stale-fence")})
	tryClaimFire(t, s, job.ID, FireClaimOptions{})
	var owns bool
	if err := s.fireClaimFence(context.Background(), job.ID, "stale", func(_ context.Context, o bool) error { owns = o; return nil }); err != nil {
		t.Fatal(err)
	}
	if owns {
		t.Fatal("stale owner owns the claim")
	}
}

func shortLockTimeout(t *testing.T, d time.Duration) {
	prev := jobsLockTimeout
	jobsLockTimeout = d
	t.Cleanup(func() { jobsLockTimeout = prev })
}

func TestSameProcessFireFenceRefusesSecondClaimAfterTimeout(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 5m", Name: prompt("local-fence-timeout")})
	shortLockTimeout(t, 100*time.Millisecond)
	type outcome struct {
		ok  bool
		err error
	}
	result := make(chan outcome, 1)
	err := s.fireJobLock(context.Background(), job.ID, func(_ context.Context, acquired bool) error {
		if !acquired {
			return fmt.Errorf("fence not acquired")
		}
		go func() {
			_, ok, err := s.claimJobForFire(context.Background(), job.ID, FireClaimOptions{})
			result <- outcome{ok, err}
		}()
		select {
		case r := <-result:
			if r.err != nil || r.ok {
				return fmt.Errorf("second claimant won %v %v", r.ok, r.err)
			}
		case <-time.After(2 * time.Second):
			return fmt.Errorf("same process claimant waited past the fire fence timeout")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{}); !ok {
		t.Fatal("claim after release lost")
	}
}

func TestSameContextFireFenceReentrancyPreservesOwnership(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 5m", Name: prompt("local-fence-reentrant")})
	done := make(chan [2]bool, 1)
	go func() {
		var got [2]bool
		s.fireJobLock(context.Background(), job.ID, func(ctx context.Context, outer bool) error {
			got[0] = outer
			return s.fireJobLock(ctx, job.ID, func(_ context.Context, inner bool) error {
				got[1] = inner
				return nil
			})
		})
		done <- got
	}()
	select {
	case got := <-done:
		if got != [2]bool{true, true} {
			t.Fatalf("outer and inner %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nested fire fence did not return")
	}
}

func TestManualClaimDoesNotStampAFutureOccurrence(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 5m", Name: prompt("m")})
	claimed, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{Manual: true})
	if !ok || claimed.ScheduledInstant != nil {
		t.Fatalf("manual fire stamped %v", claimed.ScheduledInstant)
	}
}

func TestUnclassifiedOffTickClaimDoesNotStampAFutureOccurrence(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 5m", Name: prompt("off-tick")})
	setNow(s, parseInstant(t, mustGet(t, s, job.ID).NextRunAt).Add(-time.Minute))
	claimed, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{})
	if !ok || claimed.ScheduledInstant != nil {
		t.Fatalf("off tick claim stamped %v", claimed.ScheduledInstant)
	}
}

func TestClaimSecondsBeforeTheSlotOwnsItOnce(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "0 19 * * *", Name: prompt("skew")})
	slot := mustGet(t, s, job.ID).NextRunAt
	slotAt := parseInstant(t, slot)
	setNow(s, slotAt.Add(-2*time.Second))
	claimed, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{})
	if !ok || deref(claimed.ScheduledInstant) != deref(scheduledInstant(s, slot)) {
		t.Fatalf("early fire carried %v want %v", claimed.ScheduledInstant, scheduledInstant(s, slot))
	}
	ctx := context.Background()
	ledger := openExecutionsLedger(s)
	row, err := ledger.Create(ctx, job.ID, "chronos", claimed.ScheduledInstant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Finish(ctx, row["id"].(string), true, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	mustMark(t, s, job.ID, true, MarkOptions{})
	setNow(s, slotAt.UTC().Add(11*time.Minute))
	if _, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{}); ok {
		t.Fatal("misfire backstop re ran the slot a skewed early fire already completed")
	}
	if !parseInstant(t, mustGet(t, s, job.ID).NextRunAt).After(slotAt) {
		t.Fatal("next_run_at did not move past the completed slot")
	}
}

func TestFreshClaimFromADeadSameHostOwnerIsReclaimable(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 5m", Name: prompt("s")})
	tryClaimFire(t, s, job.ID, FireClaimOptions{})
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	editJobs(t, s, func(j *Job) { j.FireClaim.By = fmt.Sprintf("%s:%d:tok", host, os.Getpid()) })
	if _, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{}); ok {
		t.Fatal("live same host owner's fresh claim was taken")
	}
	child := exec.Command(os.Args[0], "-test.run=^$")
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	editJobs(t, s, func(j *Job) { j.FireClaim.By = fmt.Sprintf("%s:%d:tok", host, child.Process.Pid) })
	if _, ok := tryClaimFire(t, s, job.ID, FireClaimOptions{}); !ok {
		t.Fatal("dead owner's fresh claim blocked the run")
	}
}

func TestHeartbeatDoesNotWaitOnTheFenceItsOwnRunHolds(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 5m", Name: prompt("long-run")})
	tryClaimFire(t, s, job.ID, FireClaimOptions{})
	owner := mustGet(t, s, job.ID).FireClaim.By
	shortLockTimeout(t, 200*time.Millisecond)
	held, release, owned := make(chan struct{}), make(chan struct{}), make(chan bool, 1)
	go func() {
		s.fireClaimFence(context.Background(), job.ID, owner, func(_ context.Context, owns bool) error {
			owned <- owns
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	ok, err := s.heartbeatFireClaim(job.ID, owner)
	foreign, ferr := s.heartbeatFireClaim(job.ID, "replacement-owner")
	close(release)
	if err != nil || !ok {
		t.Fatalf("owner heartbeat while fenced %v %v", ok, err)
	}
	if ferr != nil || foreign {
		t.Fatalf("takeover heartbeat %v %v", foreign, ferr)
	}
	if !<-owned {
		t.Fatal("fence holder did not own its claim")
	}
}
