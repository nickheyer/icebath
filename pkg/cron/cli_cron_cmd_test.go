package cron

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func cliCreate(t *testing.T, s *Store, o CreateJobOptions) *Job {
	t.Helper()
	job, err := s.createJob(o)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// Applies edit to the first stored job record of jobs.json like load_jobs, mutate, save_jobs
func editFirstStoredJob(t *testing.T, s *Store, edit func(*pyjson.Object)) {
	t.Helper()
	path := filepath.Join(s.cronDir, "jobs.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := pyjson.Loads(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	jobs, _ := doc.(*pyjson.Object).Get("jobs")
	edit(jobs.([]any)[0].(*pyjson.Object))
	out, err := pyjson.Dumps(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runCron(t *testing.T, s *Store, a *CronArgs) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	rc, err := s.cronCommand(context.Background(), a, &buf)
	if err != nil {
		t.Fatal(err)
	}
	return rc, buf.String()
}

func mustContain(t *testing.T, out string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(out, p) {
			t.Fatalf("output lacks %q:\n%s", p, out)
		}
	}
}

func TestCronRunsCLIPrintsExecutionHistory(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	l := openExecutionsLedger(s)
	row, err := l.Create(ctx, "cli-job", "builtin", nil)
	if err != nil {
		t.Fatal(err)
	}
	id := row["id"].(string)
	if _, err := l.Finish(ctx, id, false, strPtr("boom"), nil, false); err != nil {
		t.Fatal(err)
	}
	limit := 10
	_, out := runCron(t, s, &CronArgs{Command: "runs", JobID: "cli-job", Limit: &limit})
	mustContain(t, out, id, "failed", "boom")
}

func seedUnverified(t *testing.T, s *Store) *Job {
	job := cliCreate(t, s, CreateJobOptions{Prompt: strPtr("Nightly brief"), Schedule: "every 1h", Deliver: strPtr("slack:C0123456")})
	editFirstStoredJob(t, s, func(o *pyjson.Object) {
		o.Set("last_status", "ok")
		o.Set("last_delivery_unverified", []any{"slack:C0123456"})
	})
	return job
}

func TestCronListShowsUnverifiedDelivery(t *testing.T) {
	s, _ := newTestStore(t)
	job := seedUnverified(t, s)
	_, out := runCron(t, s, &CronArgs{Command: "list", All: true})
	mustContain(t, out, job.ID, "Delivery UNVERIFIED", "slack:C0123456", "without message_id/raw_response")
}

func TestCronListIsQuietWhenDeliveryWasVerified(t *testing.T) {
	s, _ := newTestStore(t)
	cliCreate(t, s, CreateJobOptions{Prompt: strPtr("Nightly brief"), Schedule: "every 1h", Deliver: strPtr("slack:C0123456")})
	if _, out := runCron(t, s, &CronArgs{Command: "list", All: true}); strings.Contains(out, "UNVERIFIED") {
		t.Fatal(out)
	}
}

func TestCronDoctorReportsUnverifiedDelivery(t *testing.T) {
	s, _ := newTestStore(t)
	job := seedUnverified(t, s)
	rc, out := runCron(t, s, &CronArgs{Command: "doctor"})
	if rc != 1 {
		t.Fatalf("rc %d", rc)
	}
	mustContain(t, out, job.ID, "last delivery unverified", "slack:C0123456")
}

func TestCronDoctorReportsCronHealthIssues(t *testing.T) {
	s, _ := newTestStore(t)
	job := cliCreate(t, s, CreateJobOptions{Prompt: strPtr("Daily digest"), Schedule: "every 1h", Script: "missing.py"})
	editFirstStoredJob(t, s, func(o *pyjson.Object) {
		o.Set("last_status", "error")
		o.Set("last_error", "Provider returned error")
		o.Set("last_delivery_error", "telegram timeout")
	})
	rc, out := runCron(t, s, &CronArgs{Command: "doctor"})
	if rc != 1 {
		t.Fatalf("rc %d", rc)
	}
	mustContain(t, out, "Cron doctor found 3 issue(s)", job.ID, "last run failed: Provider returned error", "was not delivered (telegram timeout)", "icebath cron edit", "script not found")
}

func TestCronDoctorReportsHealthyJobs(t *testing.T) {
	s, _ := newTestStore(t)
	scripts := filepath.Join(filepath.Dir(s.cronDir), "scripts")
	if err := os.MkdirAll(scripts, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "ok.py"), []byte("print('ok')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cliCreate(t, s, CreateJobOptions{Prompt: strPtr("Daily digest"), Schedule: "every 1h", Script: "ok.py"})
	rc, out := runCron(t, s, &CronArgs{Command: "doctor"})
	if rc != 0 {
		t.Fatalf("rc %d %s", rc, out)
	}
	mustContain(t, out, "✓ Cron doctor found no issues")
}

func TestCronDoctorReportsDeliveryFailureOnce(t *testing.T) {
	s, _ := newTestStore(t)
	cliCreate(t, s, CreateJobOptions{Prompt: strPtr("Daily digest"), Schedule: "every 1h"})
	editFirstStoredJob(t, s, func(o *pyjson.Object) {
		o.Set("last_status", "delivery_failed")
		o.Set("last_error", nil)
		o.Set("last_delivery_error", "telegram timeout")
	})
	rc, out := runCron(t, s, &CronArgs{Command: "doctor"})
	if rc != 1 || strings.Contains(out, "last run failed") || strings.Contains(out, "unknown error") {
		t.Fatalf("rc %d %s", rc, out)
	}
	mustContain(t, out, "was not delivered (telegram timeout)", "icebath cron edit")
}

func TestCronDoctorOverdueNextRun(t *testing.T) {
	for _, c := range []struct {
		ago time.Duration
		rc  int
		has []string
	}{{3 * time.Hour, 1, []string{"overdue", "not firing"}}, {5 * time.Minute, 0, []string{"✓ Cron doctor found no issues"}}} {
		s, _ := newTestStore(t)
		cliCreate(t, s, CreateJobOptions{Prompt: strPtr("Hourly ping"), Schedule: "every 1h"})
		editFirstStoredJob(t, s, func(o *pyjson.Object) { o.Set("next_run_at", FormatISO(time.Now().UTC().Add(-c.ago))) })
		rc, out := runCron(t, s, &CronArgs{Command: "doctor"})
		if rc != c.rc {
			t.Fatalf("ago %v rc %d %s", c.ago, rc, out)
		}
		mustContain(t, out, c.has...)
	}
}

func TestCronUnknownSubcommandExits(t *testing.T) {
	s, _ := newTestStore(t)
	var buf bytes.Buffer
	rc, err := s.cronCommand(context.Background(), &CronArgs{Command: "bogus"}, &buf)
	if rc != 1 || err == nil {
		t.Fatalf("rc %d err %v", rc, err)
	}
	mustContain(t, buf.String(), "Unknown cron command: bogus", "Usage: icebath cron [list|")
}

// Runs one cron subcommand returning its exit status, output and error result
func runCronErr(s *Store, a *CronArgs) (int, string, error) {
	var buf bytes.Buffer
	rc, err := s.cronCommand(context.Background(), a, &buf)
	return rc, buf.String(), err
}

func TestCronResumePrintsValueErrorAndPropagatesStoreFailure(t *testing.T) {
	s, _ := newTestStore(t)
	recurring := cliCreate(t, s, CreateJobOptions{Prompt: strPtr("hourly"), Schedule: "every 1h"})
	rc, out, err := runCronErr(s, &CronArgs{Command: "resume", JobID: recurring.ID, RunNow: true})
	if err != nil || rc != 1 {
		t.Fatalf("rc=%d err=%v", rc, err)
	}
	mustContain(t, out, "Failed to re-arm job: "+rearmRecurringError)

	once := cliCreate(t, s, CreateJobOptions{Prompt: strPtr("later"), Schedule: time.Now().Add(2 * time.Hour).Format("2006-01-02T15:04:05")})
	if once.Schedule == nil || once.Schedule.Kind != "once" {
		t.Fatalf("schedule %+v", once.Schedule)
	}
	failure := errors.New("database disk image is malformed")
	orig := cliRearmOneshot
	cliRearmOneshot = func(*Store, context.Context, string, any) (*Job, error) { return nil, failure }
	t.Cleanup(func() { cliRearmOneshot = orig })
	rc, out, err = runCronErr(s, &CronArgs{Command: "resume", JobID: once.ID, RunNow: true})
	if !errors.Is(err, failure) || out != "" {
		t.Fatalf("store failure printed instead of propagating rc=%d err=%v out=%q", rc, err, out)
	}
}

func TestCronNotepadPrintsValueErrorAndPropagatesDatabaseFailure(t *testing.T) {
	s, _ := newTestStore(t)
	key, value := strings.Repeat("k", NotepadMaxKeyChars+1), "v"
	rc, out, err := runCronErr(s, &CronArgs{Command: "notepad", JobID: "job1", NotepadAction: "set", Key: &key, Value: &value})
	if err != nil || rc != 1 {
		t.Fatalf("rc=%d err=%v", rc, err)
	}
	mustContain(t, out, fmt.Sprintf("Notepad error: key too long (max %d characters)", NotepadMaxKeyChars))

	if err := os.MkdirAll(filepath.Join(openNotepad(s).path, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
	short := "k"
	rc, out, err = runCronErr(s, &CronArgs{Command: "notepad", JobID: "job1", NotepadAction: "set", Key: &short, Value: &value})
	if err == nil || strings.Contains(out, "Notepad error") {
		t.Fatalf("database failure printed instead of propagating rc=%d err=%v out=%q", rc, err, out)
	}
}
