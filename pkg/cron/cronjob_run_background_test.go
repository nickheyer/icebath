package cron

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Tool env bound to an approval session key like the Python contextvar
func bgEnv(s *Store, key string) *tools.Env {
	env := &tools.Env{Home: s.home, Values: &sync.Map{}}
	if key != "" {
		env.Values.Store("approval.session_key", key)
	}
	return env
}

type dispatchRecord struct {
	mu    sync.Mutex
	calls int
	req   tools.AsyncDelegationRequest
	out   chan *pyjson.Object
}

// Replaces the async registry with one that runs the runner on a goroutine and reports dispatched
func installDispatch(t *testing.T, status string) *dispatchRecord {
	t.Helper()
	rec := &dispatchRecord{out: make(chan *pyjson.Object, 1)}
	prev := dispatchAsyncDelegationSeam
	t.Cleanup(func() { dispatchAsyncDelegationSeam = prev })
	dispatchAsyncDelegationSeam = func(env *tools.Env, req tools.AsyncDelegationRequest) map[string]any {
		rec.mu.Lock()
		rec.calls++
		rec.req = req
		rec.mu.Unlock()
		if status != "dispatched" {
			return map[string]any{"status": status, "error": "capacity"}
		}
		go func() { rec.out <- req.Runner() }()
		return map[string]any{"status": "dispatched", "delegation_id": "deleg-" + req.Role}
	}
	return rec
}

// Replaces run_one_job with the body for the test
func installRunOneJob(t *testing.T, body func(ctx context.Context, s *Store, job *Job) bool) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	prev := runOneJobSeam
	t.Cleanup(func() { runOneJobSeam = prev })
	runOneJobSeam = func(ctx context.Context, s *Store, job *Job, o runOneJobOptions) (bool, error) {
		n.Add(1)
		return body(ctx, s, job), nil
	}
	return &n
}

func bgJob(t *testing.T, s *Store, name, deliver string) *Job {
	t.Helper()
	o := CreateJobOptions{Prompt: strPtr("hi"), Schedule: "0 9 * * *", Name: strPtr(name)}
	if deliver != "" {
		o.Deliver = strPtr(deliver)
	}
	return mustCreate(t, s, o)
}

func markOK(ctx context.Context, s *Store, job *Job) bool {
	s.markJobRun(ctx, job.ID, true, MarkOptions{})
	return true
}

func waitOut(t *testing.T, rec *dispatchRecord) *pyjson.Object {
	t.Helper()
	select {
	case o := <-rec.out:
		return o
	case <-time.After(10 * time.Second):
		t.Fatal("completion never produced")
	}
	return nil
}

func TestBackgroundDispatchReturnsHandleImmediately(t *testing.T) {
	s, _ := newTestStore(t)
	job := bgJob(t, s, "bg run", "")
	rec := installDispatch(t, "dispatched")
	started, release := make(chan struct{}), make(chan struct{})
	installRunOneJob(t, func(ctx context.Context, s *Store, job *Job) bool {
		close(started)
		<-release
		return markOK(ctx, s, job)
	})
	res := s.tryDispatchBackgroundRun(context.Background(), bgEnv(s, "agent:main:telegram:dm:123"), job, nil, nil)
	defer func() { close(release); waitOut(t, rec) }()
	if res == nil || !res.Claimed || res.Dispatched == nil || !*res.Dispatched || res.DelegationID == "" {
		t.Fatalf("result %+v", res)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("job never started in background")
	}
}

func TestBackgroundCompletionCarriesSessionAndOutcome(t *testing.T) {
	s, _ := newTestStore(t)
	job := bgJob(t, s, "bg run", "")
	rec := installDispatch(t, "dispatched")
	installRunOneJob(t, markOK)
	res := s.tryDispatchBackgroundRun(context.Background(), bgEnv(s, "agent:main:telegram:dm:777"), job, nil, nil)
	if res == nil || res.Dispatched == nil || !*res.Dispatched {
		t.Fatalf("result %+v", res)
	}
	out := waitOut(t, rec)
	if rec.req.SessionKey != "agent:main:telegram:dm:777" || rec.req.Role != "cron_run" {
		t.Fatalf("request %+v", rec.req)
	}
	summary, _ := out.Get("summary")
	if st, _ := out.Get("status"); st != "completed" || !strings.Contains(summary.(string), "bg run") {
		t.Fatalf("completion %v", out)
	}
}

func TestBackgroundFailedRunReportsErrorStatus(t *testing.T) {
	s, _ := newTestStore(t)
	job := bgJob(t, s, "bg run", "")
	rec := installDispatch(t, "dispatched")
	installRunOneJob(t, func(ctx context.Context, s *Store, job *Job) bool {
		s.markJobRun(ctx, job.ID, false, MarkOptions{Error: strPtr("provider exploded")})
		return true
	})
	s.tryDispatchBackgroundRun(context.Background(), bgEnv(s, "agent:main:telegram:dm:778"), job, nil, nil)
	out := waitOut(t, rec)
	st, _ := out.Get("status")
	errText, _ := out.Get("error")
	if st != "error" || !strings.Contains(pyStrOf(errText), "provider exploded") {
		t.Fatalf("completion %v", out)
	}
}

func pyStrOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func TestBackgroundClaimLostReportsWithoutDispatch(t *testing.T) {
	s, _ := newTestStore(t)
	job := bgJob(t, s, "bg run", "")
	if _, err := s.pauseJob(context.Background(), job.ID, nil); err != nil {
		t.Fatal(err)
	}
	rec := installDispatch(t, "dispatched")
	res := s.tryDispatchBackgroundRun(context.Background(), bgEnv(s, "agent:main:telegram:dm:123"), job, nil, nil)
	if res == nil || res.Claimed || !strings.Contains(pyStrOf(res.Error), "paused/disabled") || rec.calls != 0 {
		t.Fatalf("result %+v calls %d", res, rec.calls)
	}
}

func TestBackgroundSyncFallbacks(t *testing.T) {
	s, _ := newTestStore(t)
	job := bgJob(t, s, "bg run", "")
	if res := s.tryDispatchBackgroundRun(context.Background(), bgEnv(s, ""), job, nil, nil); res != nil {
		t.Fatalf("no session key %+v", res)
	}
	env := bgEnv(s, "agent:main:telegram:dm:123")
	env.Values.Store("session.async_delivery", false)
	if res := s.tryDispatchBackgroundRun(context.Background(), env, job, nil, nil); res != nil {
		t.Fatalf("async unsupported %+v", res)
	}
}

func TestBackgroundPoolAtCapacityRunsInline(t *testing.T) {
	s, _ := newTestStore(t)
	job := bgJob(t, s, "bg run", "")
	installDispatch(t, "rejected")
	runs := installRunOneJob(t, markOK)
	res := s.tryDispatchBackgroundRun(context.Background(), bgEnv(s, "agent:main:telegram:dm:123"), job, nil, nil)
	if res == nil || res.Dispatched == nil || *res.Dispatched || !res.Success || runs.Load() != 1 {
		t.Fatalf("result %+v runs %d", res, runs.Load())
	}
}

func TestRunClaimedJobInFlightDedupe(t *testing.T) {
	s, _ := newTestStore(t)
	job := bgJob(t, s, "bg run", "")
	if !s.tryRegisterRunningJob(job.ID, "test", nil) {
		t.Fatal("register")
	}
	runs := installRunOneJob(t, markOK)
	rec := installDispatch(t, "dispatched")
	res := s.runClaimedJob(context.Background(), job, nil)
	if res.Success || !strings.Contains(pyStrOf(res.Error), "already running") || runs.Load() != 0 {
		t.Fatalf("result %+v", res)
	}
	bg := s.tryDispatchBackgroundRun(context.Background(), bgEnv(s, "agent:main:telegram:dm:123"), job, nil, nil)
	if bg == nil || bg.Claimed || !strings.Contains(pyStrOf(bg.Error), "already running") || rec.calls != 0 {
		t.Fatalf("background %+v", bg)
	}
	s.releaseRunningJob(job.ID, s.home, "test")
}

func TestRunClaimedJobRegistersAndReleases(t *testing.T) {
	s, _ := newTestStore(t)
	job := bgJob(t, s, "bg run", "")
	var registered bool
	installRunOneJob(t, func(ctx context.Context, s *Store, j *Job) bool {
		registered = getRunningJobIDs()[job.ID]
		return markOK(ctx, s, j)
	})
	res := s.runClaimedJob(context.Background(), job, nil)
	if !res.Success || !registered || getRunningJobIDs()[job.ID] {
		t.Fatalf("result %+v registered %v", res, registered)
	}
}

func TestRunClaimedJobReportsExactUnknownExecution(t *testing.T) {
	s, _ := newTestStore(t)
	job := bgJob(t, s, "bg run", "")
	ctx := context.Background()
	l := openExecutionsLedger(s)
	row, err := l.Create(ctx, job.ID, "direct", nil)
	if err != nil {
		t.Fatal(err)
	}
	id := row["id"].(string)
	l.transaction(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "UPDATE executions SET status='unknown', error='worker owner exited' WHERE id=?", id)
		return err
	})
	installRunOneJob(t, func(ctx context.Context, s *Store, j *Job) bool {
		j.ExecutionID = &id
		return markOK(ctx, s, j)
	})
	res := s.runClaimedJob(ctx, job, nil)
	if res.Success || res.Error != "worker owner exited" {
		t.Fatalf("result %+v", res)
	}
}

func TestCronjobRunActionReturnsBackgroundNote(t *testing.T) {
	s, _ := newTestStore(t)
	job := bgJob(t, s, "bg run", "")
	rec := installDispatch(t, "dispatched")
	installRunOneJob(t, markOK)
	out := runCronjob(t, s, &cronjobArgs{Action: "run", JobID: job.ID, env: bgEnv(s, "agent:main:telegram:dm:123")})
	waitOut(t, rec)
	j, _ := out["job"].(map[string]any)
	if out["success"] != true || j["executed"] != true || j["execution_mode"] != "background" || j["delegation_id"] == "" {
		t.Fatalf("out %v", out)
	}
}

func TestOneShotCliPathReapsDeadOwnerRowBeforeReturningSync(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	l := openExecutionsLedger(s)
	row, err := l.Create(ctx, "dead-owner-job", "direct", nil)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	id := row["id"].(string)
	l.transaction(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "UPDATE executions SET status='running', process_id='other-process', pid=?, process_started_at=1 WHERE id=?", cmd.Process.Pid, id)
		return err
	})
	env := bgEnv(s, "")
	env.Values.Store("session.async_delivery", false)
	deliver := "local"
	if res := s.tryDispatchBackgroundRun(ctx, env, &Job{ID: "dead-owner-job", Name: "dead owner job", Deliver: &deliver}, nil, nil); res != nil {
		t.Fatalf("result %+v", res)
	}
	got, _ := l.Get(ctx, id)
	if got["status"] != "unknown" {
		t.Fatalf("row %v", got)
	}
}

func TestManualRunDeliveryNote(t *testing.T) {
	want := " (output saved locally only)"
	if manualRunDeliveryNote("local", &Job{}) != want || manualRunDeliveryNote("local", &Job{LastDeliveryError: strPtr("telegram 400")}) != want {
		t.Fatal("local note")
	}
	note := manualRunDeliveryNote(" ", &Job{LastDeliveryError: strPtr("no target")})
	if !strings.Contains(note, "delivery FAILED") || !strings.Contains(note, "no target") {
		t.Fatalf("whitespace note %q", note)
	}
}

func TestRunnerSummaryDeliveryWiring(t *testing.T) {
	s, _ := newTestStore(t)
	job := bgJob(t, s, "dn run 1", "telegram")
	rec := installDispatch(t, "dispatched")
	installRunOneJob(t, func(ctx context.Context, s *Store, j *Job) bool {
		s.markJobRun(ctx, j.ID, true, MarkOptions{DeliveryError: strPtr("telegram send failed: 400"), Status: strPtr("delivery_failed")})
		return true
	})
	s.tryDispatchBackgroundRun(context.Background(), bgEnv(s, "agent:main:telegram:dm:83993"), job, nil, nil)
	summary, _ := waitOut(t, rec).Get("summary")
	sum := pyStrOf(summary)
	for _, want := range []string{"Delivery target: telegram", "delivery FAILED", "telegram send failed: 400", "Result: FAILED"} {
		if !strings.Contains(sum, want) {
			t.Fatalf("summary %q lacks %q", sum, want)
		}
	}
	if strings.Contains(sum, "delivered there by the job itself") || strings.Contains(sum, "Result: ok") {
		t.Fatalf("summary %q", sum)
	}
	local := bgJob(t, s, "dn run 3", "")
	local.Deliver = strPtr("")
	rec2 := installDispatch(t, "dispatched")
	installRunOneJob(t, markOK)
	s.tryDispatchBackgroundRun(context.Background(), bgEnv(s, "agent:main:telegram:dm:86622"), local, nil, nil)
	summary, _ = waitOut(t, rec2).Get("summary")
	if sum := pyStrOf(summary); !strings.Contains(sum, "Delivery target: local (output saved locally only)") || strings.Contains(sum, "delivered there by the job itself") {
		t.Fatalf("local summary %q", sum)
	}
}

func TestRunNoticeOmitsUsageNoRunSessionRecorded(t *testing.T) {
	s, _ := newTestStore(t)
	for _, c := range []struct {
		model   string
		success bool
	}{{"", true}, {"vendor/pinned-model", false}} {
		job := mustCreate(t, s, CreateJobOptions{Prompt: strPtr("report"), Schedule: "every 1h", Deliver: strPtr("local"), Model: c.model})
		rec := installDispatch(t, "dispatched")
		installRunOneJob(t, func(ctx context.Context, s *Store, j *Job) bool {
			if c.success {
				s.markJobRun(ctx, j.ID, true, MarkOptions{})
				return true
			}
			s.markJobRun(ctx, j.ID, false, MarkOptions{Error: strPtr("refused")})
			return false
		})
		s.tryDispatchBackgroundRun(context.Background(), bgEnv(s, "agent:main:telegram:dm:4242"), job, nil, nil)
		out := waitOut(t, rec)
		model, _ := out.Get("model")
		if _, ok := out.Get("api_calls"); ok || pyStrOf(model) != "" {
			t.Fatalf("completion %v", out)
		}
	}
}
