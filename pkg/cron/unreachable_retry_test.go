package cron

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"
	"time"
)

func markUnreachable(t *testing.T, s *Store, id string) {
	t.Helper()
	ok, err := s.markJobRun(context.Background(), id, false, MarkOptions{Error: prompt("ConnectError: dns"), ModelUnreachable: true})
	if err != nil || !ok {
		t.Fatalf("markJobRun(%s) = %v %v", id, ok, err)
	}
}

func nextRunTime(t *testing.T, j *Job) time.Time {
	t.Helper()
	if j.NextRunAt == nil {
		t.Fatalf("job %s has no next_run_at", j.ID)
	}
	at, err := ParseISO(*j.NextRunAt)
	if err != nil {
		t.Fatal(err)
	}
	return at.Time
}

func TestUnreachableFailurePullsNextRunEarlierThenLadderExhausts(t *testing.T) {
	s := newClockStore(t, nil, time.UTC)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("nightly report"), Schedule: "every 24h"})
	now := time.Now()
	for i, delay := range UnreachableRetryDelaysSeconds {
		markUnreachable(t, s, job.ID)
		j := mustGet(t, s, job.ID)
		gap := nextRunTime(t, j).Sub(now)
		if gap <= 0 || gap > time.Duration(delay+120)*time.Second {
			t.Fatalf("attempt %d: expected retry ~%ds out, got %s", i, delay, gap)
		}
		if j.UnreachableRetry == nil || j.UnreachableRetry.Attempt != i+1 {
			t.Fatalf("attempt %d state = %+v", i, j.UnreachableRetry)
		}
	}
	markUnreachable(t, s, job.ID)
	j := mustGet(t, s, job.ID)
	if j.UnreachableRetry != nil {
		t.Fatalf("exhausted ladder kept state %+v", j.UnreachableRetry)
	}
	if nextRunTime(t, j).Sub(now) <= time.Hour {
		t.Fatal("exhausted ladder did not fall back to the schedule")
	}

	pinned := time.Date(2026, 9, 18, 12, 1, 0, 0, time.UTC)
	cur := pinned
	s.clk.nowFn = func() time.Time { return cur }
	weekly := mustCreate(t, s, CreateJobOptions{Prompt: prompt("weekly digest"), Schedule: "0 12 * * 5"})
	markUnreachable(t, s, weekly.ID)
	retryAt := nextRunTime(t, mustGet(t, s, weekly.ID))
	if want := pinned.Add(time.Duration(UnreachableRetryDelaysSeconds[0]) * time.Second); !retryAt.Equal(want) {
		t.Fatalf("retry at %s, want %s", retryAt, want)
	}
	cur = retryAt.Add(time.Second)
	if !dueIDs(t, s)[weekly.ID] {
		t.Fatal("ladder instant off the cron lattice was not due")
	}
	markUnreachable(t, s, weekly.ID)
	retryAt = nextRunTime(t, mustGet(t, s, weekly.ID))
	editJobs(t, s, func(j *Job) {
		if j.ID == weekly.ID {
			j.Schedule.Expr = "0 9 * * 1"
		}
	})
	cur = retryAt.Add(time.Second)
	if dueIDs(t, s)[weekly.ID] {
		t.Fatal("edited expression fired the parked ladder instant")
	}
}

func TestWillRetryMirrorsPlanRetryYield(t *testing.T) {
	s := newClockStore(t, nil, time.UTC)
	willRetry := func(j *Job) bool {
		t.Helper()
		ok, err := unreachableRetryWillRetry(s, j)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	fast := mustCreate(t, s, CreateJobOptions{Prompt: prompt("fast poll"), Schedule: "every 2m"})
	markUnreachable(t, s, fast.ID)
	j := mustGet(t, s, fast.ID)
	if j.UnreachableRetry != nil || willRetry(j) {
		t.Fatal("2m cadence beats the 5m rung: plan_retry yields")
	}
	slow := mustCreate(t, s, CreateJobOptions{Prompt: prompt("nightly report"), Schedule: "every 24h"})
	markUnreachable(t, s, slow.ID)
	js := mustGet(t, s, slow.ID)
	if js.UnreachableRetry == nil || js.UnreachableRetry.Attempt != 1 || !willRetry(js) {
		t.Fatal("5m rung beats the 24h cadence: re-run is scheduled")
	}
	mid := mustCreate(t, s, CreateJobOptions{Prompt: prompt("ten minute sync"), Schedule: "every 10m"})
	markUnreachable(t, s, mid.ID)
	jm := mustGet(t, s, mid.ID)
	if jm.UnreachableRetry == nil || jm.UnreachableRetry.Attempt != 1 || willRetry(jm) {
		t.Fatal("10m cadence beats the 15m rung: yielded, notice goes out")
	}
	last := mustCreate(t, s, CreateJobOptions{Prompt: prompt("final run"), Schedule: "every 24h", Repeat: 1})
	if willRetry(mustGet(t, s, last.ID)) {
		t.Fatal("final finite repeat completes the job")
	}
}

func TestReachingTheModelResetsLadderAndOneshotsNeverRetry(t *testing.T) {
	s := newClockStore(t, nil, time.UTC)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("hourly sync"), Schedule: "every 12h"})
	markUnreachable(t, s, job.ID)
	if st := mustGet(t, s, job.ID).UnreachableRetry; st == nil || st.Attempt != 1 {
		t.Fatalf("state = %+v", st)
	}
	mustMark(t, s, job.ID, false, MarkOptions{Error: prompt("agent error")})
	j := mustGet(t, s, job.ID)
	if j.UnreachableRetry != nil {
		t.Fatal("model reach kept the ladder")
	}
	if nextRunTime(t, j).Sub(time.Now()) <= 11*time.Hour {
		t.Fatal("model reach left the schedule off cadence")
	}
	once := mustCreate(t, s, CreateJobOptions{Prompt: prompt("one shot"), Schedule: FormatISO(time.Now().UTC().Add(time.Minute))})
	markUnreachable(t, s, once.ID)
	remaining, err := s.getJob(context.Background(), once.ID)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != nil && remaining.UnreachableRetry != nil {
		t.Fatal("one shot entered the ladder")
	}
}

func TestUnreachableRetryDisabledByConfig(t *testing.T) {
	if !unreachableRetryEnabledIn(map[string]any{}) || !unreachableRetryEnabledIn(map[string]any{"cron": "x"}) {
		t.Fatal("missing or malformed cron section disables the ladder")
	}
	if !unreachableRetryEnabledIn(map[string]any{"cron": map[string]any{"retry_unreachable": 0}}) {
		t.Fatal("only false disables the ladder")
	}
	if unreachableRetryEnabledIn(map[string]any{"cron": map[string]any{"retry_unreachable": false}}) {
		t.Fatal("false kept the ladder on")
	}
	s := newClockStore(t, nil, time.UTC)
	if err := os.WriteFile(s.Home()+"/config.yaml", []byte("cron:\n  retry_unreachable: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("nightly report"), Schedule: "every 24h"})
	markUnreachable(t, s, job.ID)
	if mustGet(t, s, job.ID).UnreachableRetry != nil {
		t.Fatal("disabled ladder parked a retry")
	}
}

func TestModelUnreachableFailureClassifier(t *testing.T) {
	transient := []error{
		&url.Error{Op: "Post", URL: "https://api", Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("boom")}},
		&net.DNSError{Err: "no such host", Name: "api.example", IsNotFound: true},
		fmt.Errorf("resolve: %w", &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}),
		errors.New("Name or service not known"),
		&url.Error{Op: "Get", URL: "https://api", Err: errors.New("request timed out")},
	}
	for _, err := range transient {
		if !isModelUnreachableFailure(err, 0) {
			t.Errorf("%v not classified transient", err)
		}
	}
	if isModelUnreachableFailure(transient[0], 1) {
		t.Error("a run with a model call is not unreachable")
	}
	for _, err := range []error{errors.New("model said no"), errors.New("connection refused by policy")} {
		if isModelUnreachableFailure(err, 0) {
			t.Errorf("%v classified transient", err)
		}
	}
}

func TestFoldOneIntervalIsNotDueDuringFoldZero(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("zoneinfo unavailable")
	}
	now := time.Date(2026, 11, 1, 5, 1, 0, 0, time.UTC).In(ny)
	scheduled := time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC).In(ny)
	if scheduled.Sub(now) != 59*time.Minute {
		t.Fatal("fixture instants")
	}
	s := newClockStore(t, &now, ny)
	seedJobs(t, s, dstJob(scheduled))
	if due := dueIDs(t, s); len(due) != 0 {
		t.Fatalf("fold one interval due during fold zero: %v", due)
	}
}

func TestFoldHourTimersMeasureRealElapsedTime(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("zoneinfo unavailable")
	}
	now := time.Date(2026, 11, 1, 6, 2, 0, 0, time.UTC).In(ny)
	claimed := time.Date(2026, 11, 1, 5, 58, 0, 0, time.UTC).In(ny)
	s := newClockStore(t, &now, ny)
	if !claimIsLive(s, &Claim{At: FormatISO(claimed), By: "peer-host:1"}, now, 300) {
		t.Fatal("claim four real minutes old read as expired")
	}
	job, err := jobFromMap(dstJob(time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC).In(ny)))
	if err != nil {
		t.Fatal(err)
	}
	if !unreachableRetryPlanRetry(s, job) {
		t.Fatal("plan_retry parked nothing")
	}
	if gap := nextRunTime(t, job).Sub(now); gap != time.Duration(UnreachableRetryDelaysSeconds[0])*time.Second {
		t.Fatalf("retry gap = %s", gap)
	}
}

func dstJob(next time.Time) map[string]any {
	return map[string]any{
		"id": "dst-job", "name": "dst-job", "prompt": "fixture",
		"schedule":    map[string]any{"kind": "interval", "minutes": 60},
		"next_run_at": FormatISO(next), "last_run_at": nil, "enabled": true, "state": "scheduled",
		"repeat": map[string]any{"times": nil, "completed": 0}, "deliver": "local",
	}
}
