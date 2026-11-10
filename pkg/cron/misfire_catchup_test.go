package cron

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// External provider stand-in with the real base claim and a recorded fire_claimed
type misfireRecordingProvider struct {
	mu      sync.Mutex
	fired   []string
	done    chan struct{}
	once    sync.Once
	release chan struct{}
}

func newMisfireRecordingProvider() *misfireRecordingProvider {
	return &misfireRecordingProvider{done: make(chan struct{})}
}

func (p *misfireRecordingProvider) Name() string                                         { return "recording" }
func (p *misfireRecordingProvider) Start(context.Context, <-chan struct{}, StartOptions) {}

func (p *misfireRecordingProvider) FireClaimed(_ context.Context, job *Job, _ FireOptions) (bool, error) {
	if p.release != nil {
		select {
		case <-p.release:
		case <-time.After(10 * time.Second):
			return false, errors.New("release never set")
		}
	}
	p.mu.Lock()
	p.fired = append(p.fired, job.ID)
	p.mu.Unlock()
	p.once.Do(func() { close(p.done) })
	return true, nil
}

func (p *misfireRecordingProvider) waitFired(timeout time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (p *misfireRecordingProvider) firedIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.fired...)
}

// Rewinds a job's next run into the past, the missed fires of a parked external trigger
func parkInPast(t *testing.T, s *Store, jobID string, minutes float64) {
	t.Helper()
	jobs, err := s.LoadJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.ID == jobID {
			at := FormatISO(s.clk.now().Add(-time.Duration(minutes * float64(time.Minute))))
			j.NextRunAt = &at
		}
	}
	if err := s.SaveJobs(context.Background(), jobs, nil, false); err != nil {
		t.Fatal(err)
	}
}

// Store with one hourly job and a context bound to it
func misfireFixture(t *testing.T) (*Store, context.Context, *Job) {
	t.Helper()
	s, _ := newTestStore(t)
	job, err := s.createJob(context.Background(), CreateJobOptions{Prompt: strPtr("p"), Schedule: "every 1h"})
	if err != nil {
		t.Fatal(err)
	}
	return s, withCronStore(context.Background(), s), job
}

func overdueFired(t *testing.T, ctx context.Context, p CronScheduler) int {
	t.Helper()
	n, err := fireOverdueJobs(ctx, p, FireOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestFireOverdueJobs(t *testing.T) {
	t.Run("test_noop_for_builtin_provider", func(t *testing.T) {
		s, ctx, job := misfireFixture(t)
		parkInPast(t, s, job.ID, 60)
		if n := overdueFired(t, ctx, &InProcessCronScheduler{}); n != 0 {
			t.Fatal(n)
		}
	})
	t.Run("test_fires_job_past_grace", func(t *testing.T) {
		s, ctx, job := misfireFixture(t)
		parkInPast(t, s, job.ID, 30)
		provider := newMisfireRecordingProvider()
		if n := overdueFired(t, ctx, provider); n != 1 {
			t.Fatal(n)
		}
		if !provider.waitFired(5*time.Second) || !reflect.DeepEqual(provider.firedIDs(), []string{job.ID}) {
			t.Fatalf("fired %v", provider.firedIDs())
		}
	})
	t.Run("test_respects_grace_window", func(t *testing.T) {
		s, ctx, job := misfireFixture(t)
		parkInPast(t, s, job.ID, 5)
		provider := newMisfireRecordingProvider()
		if n := overdueFired(t, ctx, provider); n != 0 || len(provider.firedIDs()) != 0 {
			t.Fatal(n, provider.firedIDs())
		}
	})
	t.Run("test_grace_zero_disables", func(t *testing.T) {
		s, ctx, job := misfireFixture(t)
		providerConfig(t, s, "cron:\n  misfire_grace_minutes: 0.0\n")
		parkInPast(t, s, job.ID, 600)
		if n := overdueFired(t, ctx, newMisfireRecordingProvider()); n != 0 {
			t.Fatal(n)
		}
	})
	t.Run("test_future_job_not_fired", func(t *testing.T) {
		_, ctx, _ := misfireFixture(t)
		if n := overdueFired(t, ctx, newMisfireRecordingProvider()); n != 0 {
			t.Fatal(n)
		}
	})
	t.Run("test_paused_job_not_fired", func(t *testing.T) {
		s, ctx, job := misfireFixture(t)
		parkInPast(t, s, job.ID, 30)
		if _, err := s.pauseJob(ctx, job.ID, nil); err != nil {
			t.Fatal(err)
		}
		if n := overdueFired(t, ctx, newMisfireRecordingProvider()); n != 0 {
			t.Fatal(n)
		}
	})
	t.Run("test_fresh_external_claim_wins", func(t *testing.T) {
		s, ctx, job := misfireFixture(t)
		parkInPast(t, s, job.ID, 30)
		if _, ok, err := s.claimJobForFire(ctx, job.ID, FireClaimOptions{}); err != nil || !ok {
			t.Fatal(ok, err)
		}
		parkInPast(t, s, job.ID, 30)
		provider := newMisfireRecordingProvider()
		if n := overdueFired(t, ctx, provider); n != 0 || len(provider.firedIDs()) != 0 {
			t.Fatal(n, provider.firedIDs())
		}
	})
	t.Run("test_claim_advances_next_run_no_refire", func(t *testing.T) {
		s, ctx, job := misfireFixture(t)
		parkInPast(t, s, job.ID, 30)
		provider := newMisfireRecordingProvider()
		if n := overdueFired(t, ctx, provider); n != 1 || !provider.waitFired(5*time.Second) {
			t.Fatal(n)
		}
		stamped, err := s.getJob(ctx, job.ID)
		if err != nil || stamped == nil || stamped.NextRunAt == nil || *stamped.NextRunAt <= FormatISO(s.clk.now()) {
			t.Fatalf("stamped %+v %v", stamped, err)
		}
		if n := overdueFired(t, ctx, newMisfireRecordingProvider()); n != 0 {
			t.Fatal(n)
		}
	})
	t.Run("test_dispatch_is_nonblocking", func(t *testing.T) {
		s, ctx, job := misfireFixture(t)
		parkInPast(t, s, job.ID, 30)
		provider := newMisfireRecordingProvider()
		provider.release = make(chan struct{})
		n := overdueFired(t, ctx, provider)
		blocked := provider.firedIDs()
		close(provider.release)
		if n != 1 || len(blocked) != 0 {
			t.Fatal(n, blocked)
		}
		if !provider.waitFired(10*time.Second) || !reflect.DeepEqual(provider.firedIDs(), []string{job.ID}) {
			t.Fatalf("fired %v", provider.firedIDs())
		}
	})
	t.Run("test_estop_skips_sweep_and_next_sweep_after_resume_catches_up", func(t *testing.T) {
		s, ctx, job := misfireFixture(t)
		patchChronos(t, &estopCheckPaused, func(component string, log *slog.Logger) bool {
			return tools.CheckPaused(context.Background(), component, log)
		})
		parkInPast(t, s, job.ID, 30)
		parked, err := s.getJob(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		provider := newMisfireRecordingProvider()
		tools.Engage(context.Background(), "runaway fan-out")
		t.Cleanup(func() { tools.Disengage(context.Background()) })
		if n := overdueFired(t, ctx, provider); n != 0 || len(provider.firedIDs()) != 0 {
			t.Fatal(n, provider.firedIDs())
		}
		if after, _ := s.getJob(ctx, job.ID); deref(after.NextRunAt) != deref(parked.NextRunAt) {
			t.Fatalf("next run moved %v %v", deref(after.NextRunAt), deref(parked.NextRunAt))
		}
		tools.Disengage(context.Background())
		if n := overdueFired(t, ctx, provider); n != 1 {
			t.Fatal(n)
		}
		if !provider.waitFired(5*time.Second) || !reflect.DeepEqual(provider.firedIDs(), []string{job.ID}) {
			t.Fatalf("fired %v", provider.firedIDs())
		}
	})
}

// Provider double that records claim attempts and declines every claim
type misfireClaimRecorder struct {
	minimalProvider
	claimed []string
}

func (p *misfireClaimRecorder) ClaimFire(_ context.Context, jobID string, _, _ bool) (*Job, error) {
	p.claimed = append(p.claimed, jobID)
	return nil, nil
}

func (p *misfireClaimRecorder) FireClaimed(context.Context, *Job, FireOptions) (bool, error) {
	panic("fire_claimed must not run when claim declined")
}

// Stored job record of one schedule kind due at the instant
func misfireJobRecord(id, kind string, runAt time.Time) map[string]any {
	schedule := map[string]any{"kind": kind}
	times := any(nil)
	if kind == "once" {
		schedule["run_at"] = FormatISO(runAt)
		times = 1
	} else {
		schedule["expr"] = "*/5 * * * *"
	}
	return map[string]any{
		"id": id, "name": id, "prompt": "x", "schedule": schedule, "next_run_at": FormatISO(runAt),
		"last_run_at": nil, "enabled": true, "state": "scheduled",
		"repeat": map[string]any{"times": times, "completed": 0}, "deliver": "local",
	}
}

// Runs the backstop over the stored jobs under the misfire grace, returning the provider and the fired count
func runBackstop(t *testing.T, s *Store, grace string, jobs ...map[string]any) (*misfireClaimRecorder, int) {
	t.Helper()
	writeJobsFile(t, s, map[string]any{"jobs": jobs})
	providerConfig(t, s, "cron:\n  misfire_grace_minutes: "+grace+"\n")
	provider := &misfireClaimRecorder{minimalProvider: minimalProvider{"recording"}}
	n, err := fireOverdueJobs(withCronStore(context.Background(), s), provider, FireOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return provider, n
}

func TestMisfireBackstopOneShotGrace(t *testing.T) {
	t.Run("test_stale_oneshot_beyond_grace_is_not_fired", func(t *testing.T) {
		s, _ := newTestStore(t)
		provider, fired := runBackstop(t, s, "5.0", misfireJobRecord("stale-once", "once", s.clk.now().Add(-2*time.Hour)))
		if fired != 0 || len(provider.claimed) != 0 {
			t.Fatal(fired, provider.claimed)
		}
	})
	t.Run("test_overdue_recurring_job_still_fires", func(t *testing.T) {
		s, _ := newTestStore(t)
		provider, _ := runBackstop(t, s, "5.0", misfireJobRecord("overdue-cron", "cron", s.clk.now().Add(-2*time.Hour)))
		if !reflect.DeepEqual(provider.claimed, []string{"overdue-cron"}) {
			t.Fatal(provider.claimed)
		}
	})
	t.Run("test_oneshot_within_oneshot_grace_but_past_misfire_grace", func(t *testing.T) {
		if oneshotGraceSeconds <= 60 {
			t.Fatal(oneshotGraceSeconds)
		}
		s, _ := newTestStore(t)
		provider, _ := runBackstop(t, s, "0.5", misfireJobRecord("recent-once", "once", s.clk.now().Add(-60*time.Second)))
		if !reflect.DeepEqual(provider.claimed, []string{"recent-once"}) {
			t.Fatal(provider.claimed)
		}
	})
}
