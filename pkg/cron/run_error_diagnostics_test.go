package cron

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

func diagnosticJob(t *testing.T, s *Store) *Job {
	t.Helper()
	prompt, deliver := "Check status", "local"
	job, err := s.createJob(context.Background(), CreateJobOptions{Prompt: &prompt, Schedule: "every 1h", Model: "local-probe", Deliver: &deliver})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func failSetup(*Store, context.Context, *Job, string, string, cronJobConfig) (*cronAgentSetup, error) {
	cause := raiseTraced("ConnectionError", "https://user:password@localhost/api?token=secret-token", nil)
	return nil, raiseTraced("RuntimeError", "Connection error.", cause)
}

func TestRunErrorPersistsRedactedCauseButReturnsSummary(t *testing.T) {
	s, _ := newTestStore(t)
	resetDeliver(t)
	prev := resolveCronAgentSetupSeam
	resolveCronAgentSetupSeam = failSetup
	t.Cleanup(func() { resolveCronAgentSetupSeam = prev })
	job := diagnosticJob(t, s)
	db, err := state.Acquire(context.Background(), home.StatePath(s.home))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.ReleaseOrClose(db) })
	execID := "diagnostic-fire"
	ctx := gateway.BindCronExecution(context.Background(), gateway.CronOwnerExecution{
		DB: db, DBPath: home.StatePath(s.home), SessionID: "diagnostic-session", JobID: job.ID, ExecutionID: execID,
	})
	out, err := s.runJob(ctx, job, runJobOptions{ExecutionID: &execID})
	if err != nil {
		t.Fatal(err)
	}
	path, err := s.SaveJobOutput(ctx, job.ID, out.Output)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	saved := string(raw)
	if out.Success || out.FinalResponse != "" || out.Error == nil || *out.Error != "RuntimeError: Connection error." {
		t.Fatalf("outcome %+v", out)
	}
	for _, want := range []string{"Traceback (most recent call last)", "failSetup", "ConnectionError", "localhost"} {
		if !strings.Contains(saved, want) {
			t.Fatalf("saved output lacks %q:\n%s", want, saved)
		}
	}
	if strings.Contains(saved, "password") || strings.Contains(saved, "secret-token") {
		t.Fatalf("saved output leaks credentials:\n%s", saved)
	}
}

func TestListExposesRunErrorAndClearsItAfterSuccess(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	job := diagnosticJob(t, s)
	reason := "RuntimeError: https://user:password@localhost/api?token=secret-token"
	if _, err := s.markJobRun(ctx, job.ID, false, MarkOptions{Error: &reason}); err != nil {
		t.Fatal(err)
	}
	displayed := s.formatJob(mustJob(t, s, job.ID))
	lastError, _ := field(displayed, "last_error", nil).(string)
	if !strings.HasPrefix(lastError, "RuntimeError:") || !strings.Contains(lastError, "localhost") {
		t.Fatalf("last_error %q", lastError)
	}
	if strings.Contains(lastError, "password") || strings.Contains(lastError, "secret-token") {
		t.Fatalf("last_error leaks %q", lastError)
	}
	for _, k := range []string{"last_delivery_error", "last_fire_error"} {
		if v, ok := displayed.Get(k); !ok || v != nil {
			t.Fatalf("%s = %v %v", k, v, ok)
		}
	}
	console := strings.Join(s.cronListLines(ctx, false), "\n")
	if !strings.Contains(console, "error: "+lastError) {
		t.Fatalf("console lacks the error:\n%s", console)
	}
	if strings.Contains(console, "password") || strings.Contains(console, "secret-token") {
		t.Fatalf("console leaks:\n%s", console)
	}
	if _, err := s.markJobRun(ctx, job.ID, true, MarkOptions{}); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.formatJob(mustJob(t, s, job.ID)).Get("last_error"); v != nil {
		t.Fatalf("last_error kept %v", v)
	}
}
