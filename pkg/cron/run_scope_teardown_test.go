package cron

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type boundedFakeDB struct {
	release chan struct{}
	ended   []string
}

func (f *boundedFakeDB) CompressionTip(id string) (string, error) { panic("tip lookup exploded") }
func (f *boundedFakeDB) SetTitle(sessionID, title string) (bool, error) {
	return false, errors.New("title rejected")
}
func (f *boundedFakeDB) EndSession(id, reason string) error {
	<-f.release
	f.ended = append(f.ended, id)
	return nil
}
func (f *boundedFakeDB) UpdateSessionCwd(id, cwd, repoRoot, branch string) error { return nil }
func (f *boundedFakeDB) SessionLifecycleStatuses(ctx context.Context, ids []string) (map[string]string, error) {
	return map[string]string{"s1": "ended"}, nil
}

func TestBoundedSessionDBRaisesCallFailuresAndDisablesAfterATimeout(t *testing.T) {
	s, _ := newTestStore(t)
	if err := os.WriteFile(filepath.Join(s.home, "config.yaml"), []byte("cron:\n  cleanup_timeout_seconds: 0.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db := &boundedFakeDB{release: make(chan struct{})}
	b := newBoundedCronSessionDB(s, db, "job-1")
	if _, err := b.CompressionTip("s1"); err == nil || err.Error() != "tip lookup exploded" {
		t.Fatalf("panicking call gave %v", err)
	}
	if _, err := b.SetSessionTitle(context.Background(), "s1", "t"); err == nil || err.Error() != "title rejected" {
		t.Fatalf("failing call gave %v", err)
	}
	statuses, err := b.sessionLifecycleStatuses(context.Background(), []string{"s1"})
	if err != nil || statuses["s1"] != "ended" {
		t.Fatalf("statuses %v %v", statuses, err)
	}
	if err := b.EndSession("s1", "cron_complete"); err == nil || err.Error() != "session finalization method end_session timed out" {
		t.Fatalf("hung call gave %v", err)
	}
	close(db.release)
	if err := b.UpdateSessionCwd("s1", "/tmp"); err == nil || err.Error() != "session finalization disabled after prior cleanup failure" {
		t.Fatalf("call after timeout gave %v", err)
	}
}

func TestRunJobFailsOnANonMappingProviderRouting(t *testing.T) {
	s, _ := newTestStore(t)
	resetDeliver(t)
	if err := os.WriteFile(filepath.Join(s.home, "config.yaml"), []byte("provider_routing: fastest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	job := mustCreate(t, s, CreateJobOptions{Prompt: strPtr("hi"), Schedule: "every 1h", Name: strPtr("routed"), Model: "m1"})
	fake := &fakeCronAgent{result: map[string]any{"final_response": "never", "completed": true}}
	ctx, execID := ownerRun(t, s, job, fake)
	out, err := s.runJob(ctx, job, runJobOptions{ExecutionID: &execID})
	text := deref(out.Error)
	if err != nil {
		text = err.Error()
	}
	if out.Success || !strings.Contains(text, "'str' object has no attribute 'get'") || fake.spec.Platform != "" {
		t.Fatalf("outcome %+v err %v spec %+v", out, err, fake.spec)
	}
}

type reapRecorder struct {
	mu       sync.Mutex
	timeouts []time.Duration
	waited   chan struct{}
}

func (r *reapRecorder) Wait(timeout time.Duration) (*int, bool) {
	r.mu.Lock()
	r.timeouts = append(r.timeouts, timeout)
	r.mu.Unlock()
	if timeout == time.Duration(math.MaxInt64) {
		close(r.waited)
	}
	return nil, false
}
func (r *reapRecorder) Poll() *int { return nil }

func TestTerminalWorkerStillInTeardownIsWaitedInTheBackground(t *testing.T) {
	s, _ := newTestStore(t)
	prev := getExecution
	t.Cleanup(func() { getExecution = prev })
	getExecution = func(context.Context, *Store, string) (map[string]any, error) {
		return map[string]any{"id": "exec-9", "status": "completed"}, nil
	}
	proc := &reapRecorder{waited: make(chan struct{})}
	ok, err := s.waitForExternalCronWorkerBody(context.Background(), proc, "exec-9", nil)
	if err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	select {
	case <-proc.waited:
	case <-time.After(5 * time.Second):
		t.Fatalf("no background wait, waits %v", proc.timeouts)
	}
}

func TestCanonicalRunRefusesAJournalWithoutParams(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: strPtr("hi"), Schedule: "every 1h", Name: strPtr("canon")})
	requestID := "req-1"
	journal := s.journalPath(job.ID, requestID)
	if err := os.MkdirAll(filepath.Dir(journal), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte(`{"receipt": null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.runCanonicalJob(context.Background(), job, nil, nil, &requestID); err == nil || err.Error() != "KeyError: 'params'" {
		t.Fatalf("err %v", err)
	}
}

type stoppingProvider struct{ stopped int }

func (p *stoppingProvider) Name() string                                         { return "stopping" }
func (p *stoppingProvider) Start(context.Context, <-chan struct{}, StartOptions) {}
func (p *stoppingProvider) Stop()                                                { p.stopped++ }

type plainProvider struct{}

func (plainProvider) Name() string                                         { return "plain" }
func (plainProvider) Start(context.Context, <-chan struct{}, StartOptions) {}

func TestSchedulerStopCallsOnlyAnOfferedTeardown(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	p := &stoppingProvider{}
	SchedulerStop(p)
	SchedulerStop(plainProvider{})
	if p.stopped != 1 {
		t.Fatalf("stopped %d", p.stopped)
	}
}

func TestParallelPoolShutdownWaitsForQueuedWorkThenForgetsEveryPool(t *testing.T) {
	s, _ := newTestStore(t)
	one := 1
	pool := s.getParallelPool(&one)
	var mu sync.Mutex
	ran := 0
	for range 3 {
		if _, err := pool.Submit(func() { time.Sleep(20 * time.Millisecond); mu.Lock(); ran++; mu.Unlock() }); err != nil {
			t.Fatal(err)
		}
	}
	shutdownParallelPools()
	mu.Lock()
	defer mu.Unlock()
	parallelPools.Lock()
	defer parallelPools.Unlock()
	if ran != 3 || len(parallelPools.pools) != 0 || len(parallelPools.max) != 0 {
		t.Fatalf("ran %d pools %d", ran, len(parallelPools.pools))
	}
}
