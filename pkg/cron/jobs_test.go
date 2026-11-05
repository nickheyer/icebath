package cron

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Store in a temp home whose clock reads now in zone, a nil now keeping the real clock
func newClockStore(t *testing.T, now *time.Time, zone *time.Location) *Store {
	t.Helper()
	s, _ := newTestStore(t)
	if now != nil {
		at := *now
		s.clk.nowFn = func() time.Time { return at }
	}
	if zone != nil {
		s.clk.zoneFn = func() *time.Location { return zone }
	}
	return s
}

func mustCreate(t *testing.T, s *Store, o CreateJobOptions) *Job {
	t.Helper()
	job, err := s.createJob(o)
	if err != nil {
		t.Fatalf("createJob(%+v): %v", o, err)
	}
	return job
}

func prompt(p string) *string { return &p }

func mustParse(t *testing.T, s *Store, text string) *Schedule {
	t.Helper()
	sched, err := s.parseSchedule(text)
	if err != nil {
		t.Fatalf("parseSchedule(%q): %v", text, err)
	}
	return sched
}

func TestParseDurationForms(t *testing.T) {
	for in, want := range map[string]int{"30m": 30, "1min": 1, "5mins": 5, "10minute": 10, "120minutes": 120,
		"hour": 60, "hr": 60, "h": 60, "minute": 1, "m": 1, "day": 1440, "d": 1440} {
		if got, err := parseDuration(in); err != nil || got != want {
			t.Fatalf("parseDuration(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"abc", "hourx", "30x", "", "m30"} {
		if _, err := parseDuration(in); err == nil {
			t.Fatalf("parseDuration(%q) accepted", in)
		}
	}
	s, _ := newTestStore(t)
	for in, want := range map[string]int{"every hour": 60, "every day": 1440} {
		sched := mustParse(t, s, in)
		if sched.Kind != "interval" || sched.Minutes != want {
			t.Fatalf("%q = %+v", in, sched)
		}
	}
}

func TestParseScheduleForms(t *testing.T) {
	s, _ := newTestStore(t)
	if sched := mustParse(t, s, "30m"); sched.Kind != "interval" || sched.Minutes != 30 || sched.RunAt != "" {
		t.Fatalf("bare duration = %+v", sched)
	}
	sched := mustParse(t, s, "in 30m")
	runAt, err := ParseISO(sched.RunAt)
	now := time.Now()
	if sched.Kind != "once" || err != nil || !runAt.Time.After(now) || !runAt.Time.Before(now.Add(31*time.Minute)) {
		t.Fatalf("in 30m = %+v %v", sched, err)
	}
	if sched := mustParse(t, s, "every 2h"); sched.Kind != "interval" || sched.Minutes != 120 {
		t.Fatalf("every 2h = %+v", sched)
	}
	if sched := mustParse(t, s, "every monday 9am"); sched.Kind != "cron" || sched.Expr != "0 9 * * 1" || sched.Display != "every monday 9am" {
		t.Fatalf("every monday 9am = %+v", sched)
	}
	for in, want := range map[string]string{
		"every sunday 9am": "0 9 * * 0", "every mon 9am": "0 9 * * 1", "every fri 5pm": "0 17 * * 5",
		"every day at 9am": "0 9 * * *", "every day 7am": "0 7 * * *", "every weekday at 9am": "0 9 * * 1-5",
		"every weekend at 10am": "0 10 * * 0,6", "weekdays at 9am": "0 9 * * 1-5", "monday at 9:30": "30 9 * * 1",
		"daily at 7am": "0 7 * * *", "Monday, Wednesday at 9am": "0 9 * * 1,3", "monday and friday at 5pm": "0 17 * * 1,5",
		"every tue, thu 8am": "0 8 * * 2,4", "every monday 14:30": "30 14 * * 1", "every monday 9:05am": "5 9 * * 1",
		"every monday noon": "0 12 * * 1", "every monday midnight": "0 0 * * 1", "every monday at 7": "0 7 * * 1",
		"every monday 12am": "0 0 * * 1", "every monday 12pm": "0 12 * * 1", "Every Monday 9AM": "0 9 * * 1",
		"0 9 * * *": "0 9 * * *", "0 9 * * MON": "0 9 * * MON", "*/15 9-17 * * MON-FRI": "*/15 9-17 * * MON-FRI",
		"0 9 1 JAN *": "0 9 1 JAN *", "0 9 * * MON,WED,FRI": "0 9 * * MON,WED,FRI",
	} {
		if sched := mustParse(t, s, in); sched.Kind != "cron" || sched.Expr != want {
			t.Fatalf("%q = %+v, want %q", in, sched, want)
		}
	}
	for _, in := range []string{"monday banana at 9am", "funday at 9am", "every monday", "every monday 25am",
		"every monday 9pm pizza", "0 9 * * FUNDAY"} {
		if _, err := s.parseSchedule(in); err == nil {
			t.Fatalf("%q accepted", in)
		}
	}
	if sched := mustParse(t, s, "every 30m"); sched.Kind != "interval" {
		t.Fatal("every 30m is not an interval")
	}
	if sched := mustParse(t, s, "every 1d"); sched.Minutes != 1440 {
		t.Fatal("every 1d minutes")
	}
	if sched := mustParse(t, s, "2030-01-15T14:00:00"); sched.Kind != "once" || !strings.Contains(sched.RunAt, "2030-01-15") {
		t.Fatalf("iso = %+v", sched)
	}
	next, err := s.computeNextRun(mustParse(t, s, "every monday 9am"), nil, nil)
	if err != nil || next == nil {
		t.Fatalf("weekly next = %v %v", next, err)
	}
	dt, _ := ParseISO(*next)
	if dt.Time.Weekday() != time.Monday || dt.Time.Hour() != 9 || dt.Time.Minute() != 0 {
		t.Fatalf("weekly next = %s", *next)
	}
}

func TestNaiveISOAnchorsToConfiguredZone(t *testing.T) {
	kolkata := time.FixedZone("", 5*3600+1800)
	now := time.Date(2026, 6, 22, 20, 0, 0, 0, kolkata)
	s := newClockStore(t, &now, kolkata)
	sched := mustParse(t, s, "2026-06-22T20:07:00")
	if sched.Kind != "once" || sched.RunAt != "2026-06-22T20:07:00+05:30" {
		t.Fatalf("naive iso = %+v", sched)
	}
}

func TestRecentPastOneshotDueUnderDivergingZone(t *testing.T) {
	kolkata := time.FixedZone("", 5*3600+1800)
	now := time.Date(2026, 6, 22, 20, 7, 30, 0, kolkata)
	s := newClockStore(t, &now, kolkata)
	naive := now.Add(-30 * time.Second).Format("2006-01-02T15:04:05")
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("test message"), Schedule: naive, Deliver: prompt("local")})
	due, err := s.getDueJobs(context.Background())
	if err != nil || len(due) != 1 || due[0].ID != job.ID {
		t.Fatalf("one-shot should be due, next_run_at=%v due=%v err=%v", deref(job.NextRunAt), jobIDs(due), err)
	}
}

func TestComputeNextRunOnceAndInterval(t *testing.T) {
	s, _ := newTestStore(t)
	future := time.Now().Add(time.Hour).Format("2006-01-02T15:04:05.000000")
	if got, _ := s.computeNextRun(&Schedule{Kind: "once", RunAt: future}, nil, nil); deref(got) != future {
		t.Fatalf("future once = %v", got)
	}
	now := time.Date(2026, 3, 18, 4, 22, 3, 0, time.UTC)
	s = newClockStore(t, &now, time.UTC)
	runAt := "2026-03-18T04:22:00+00:00"
	if got, _ := s.computeNextRun(&Schedule{Kind: "once", RunAt: runAt}, nil, nil); deref(got) != runAt {
		t.Fatalf("within grace = %v", got)
	}
	last := FormatISO(now)
	if got, _ := s.computeNextRun(&Schedule{Kind: "once", RunAt: runAt}, &last, nil); got != nil {
		t.Fatalf("already run = %v", *got)
	}
	s, _ = newTestStore(t)
	got, _ := s.computeNextRun(&Schedule{Kind: "interval", Minutes: 60, HasMinutes: true}, nil, nil)
	next, _ := ParseISO(deref(got))
	if !next.Time.After(time.Now().Add(59 * time.Minute)) {
		t.Fatalf("interval first run = %v", got)
	}
}

func TestCreateJobRoundTripsNonASCII(t *testing.T) {
	s, _ := newTestStore(t)
	name := "日次レポート 🎉 café"
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Summarize " + name), Schedule: "30m", Name: &name})
	fetched, _ := s.getJob(job.ID)
	if fetched.Name != name || !strings.Contains(fetched.Prompt, name) {
		t.Fatalf("fetched = %+v", fetched)
	}
	raw, _ := os.ReadFile(s.jobsFile)
	if !strings.Contains(string(raw), "日次レポート") || !strings.Contains(string(raw), "🎉") || strings.Contains(string(raw), "\\"+"u65e5") {
		t.Fatalf("jobs.json escapes non ASCII: %s", raw)
	}
}

func TestCreateGetListRemove(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Check server status"), Schedule: "30m"})
	if job.ID == "" || job.Prompt != "Check server status" || !job.Enabled || job.Schedule.Kind != "interval" {
		t.Fatalf("created = %+v", job)
	}
	if fetched, _ := s.getJob(job.ID); fetched == nil || fetched.Prompt != "Check server status" {
		t.Fatal("get lost the job")
	}
	mustCreate(t, s, CreateJobOptions{Prompt: prompt("Job 2"), Schedule: "every 2h"})
	if jobs, _ := s.listJobs(false); len(jobs) != 2 {
		t.Fatalf("list = %d", len(jobs))
	}
	if ok, err := s.removeJob(context.Background(), job.ID); !ok || err != nil {
		t.Fatalf("remove = %v %v", ok, err)
	}
	if fetched, _ := s.getJob(job.ID); fetched != nil {
		t.Fatal("removed job still present")
	}
	once := mustCreate(t, s, CreateJobOptions{Prompt: prompt("One-shot"), Schedule: "in 1h"})
	if once.Repeat.Times == nil || *once.Repeat.Times != 1 {
		t.Fatal("one-shot repeat must default to 1")
	}
	origin := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Test"), Schedule: "30m",
		Origin: map[string]any{"platform": "telegram", "chat_id": "123"}})
	if deref(origin.Deliver) != "origin" {
		t.Fatalf("deliver = %v", deref(origin.Deliver))
	}
}

func TestRepeatStringFormsCoerced(t *testing.T) {
	s, _ := newTestStore(t)
	for in, want := range map[string]*int{"forever": nil, "once": intPtr(1), "3": intPtr(3)} {
		job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Str"), Schedule: "every 1h", Repeat: in})
		if (want == nil) != (job.Repeat.Times == nil) || (want != nil && *want != *job.Repeat.Times) {
			t.Fatalf("repeat %q = %v", in, job.Repeat.Times)
		}
	}
	if _, err := s.createJob(CreateJobOptions{Prompt: prompt("Bad"), Schedule: "every 1h", Repeat: "banana"}); err == nil || !strings.Contains(err.Error(), "Invalid repeat") {
		t.Fatalf("banana accepted: %v", err)
	}
	ctx := context.Background()
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("t"), Schedule: "every 1h", Repeat: 2})
	s.markJobRun(ctx, job.ID, true, MarkOptions{})
	updated, err := s.updateJob(job.ID, map[string]any{"repeat": "forever"})
	if err != nil || updated.Repeat.Times != nil || updated.Repeat.Completed != 1 {
		t.Fatalf("update forever = %+v %v", updated.Repeat, err)
	}
	if ok, err := s.markJobRun(ctx, job.ID, true, MarkOptions{}); !ok || err != nil {
		t.Fatalf("mark after forever = %v %v", ok, err)
	}
	updated, err = s.updateJob(job.ID, map[string]any{"repeat": map[string]any{"times": "3"}})
	if err != nil || *updated.Repeat.Times != 3 || updated.Repeat.Completed != 2 {
		t.Fatalf("update dict = %+v %v", updated.Repeat, err)
	}
	if _, err := s.updateJob(job.ID, map[string]any{"repeat": "banana"}); err == nil || !strings.Contains(err.Error(), "Invalid repeat") {
		t.Fatalf("update banana accepted: %v", err)
	}
}

func TestInvalidRepeatCompletedIsNormalized(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("t"), Schedule: "every 1h", Repeat: 3})
	completedRe := regexp.MustCompile(`"completed": [^\n}]*`)
	setCompleted := func(value string) {
		raw, _ := os.ReadFile(s.jobsFile)
		os.WriteFile(s.jobsFile, completedRe.ReplaceAll(raw, []byte(`"completed": `+value)), 0o600)
	}
	completed := func() int {
		j, _ := s.getJob(job.ID)
		return j.Repeat.Completed
	}
	setCompleted("null")
	s.markJobRun(ctx, job.ID, true, MarkOptions{})
	if completed() != 1 {
		t.Fatalf("null completed then run = %d", completed())
	}
	setCompleted("null")
	if u, _ := s.updateJob(job.ID, map[string]any{"repeat": map[string]any{"times": 5}}); u.Repeat.Completed != 0 {
		t.Fatalf("update over null completed = %d", u.Repeat.Completed)
	}
	for value, want := range map[string]int{`"2"`: 3, "1.0": 2, `"junk"`: 1, "-2": 1, "Infinity": 1} {
		setCompleted(value)
		s.markJobRun(ctx, job.ID, true, MarkOptions{})
		if got := completed(); got != want {
			t.Fatalf("completed %s then run = %d, want %d", value, got, want)
		}
	}
}

func TestScheduleKindFlipRederivesRepeat(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("poll"), Schedule: "in 15m"})
	updated, err := s.updateJob(job.ID, map[string]any{"schedule": "every 15m"})
	if err != nil || updated.Schedule.Kind != "interval" || updated.Repeat.Times != nil {
		t.Fatalf("once to recurring = %+v %v", updated, err)
	}
	explicit := mustCreate(t, s, CreateJobOptions{Prompt: prompt("poll"), Schedule: "in 15m"})
	if u, _ := s.updateJob(explicit.ID, map[string]any{"schedule": "every 15m", "repeat": 3}); *u.Repeat.Times != 3 {
		t.Fatal("explicit repeat lost")
	}
	rec := mustCreate(t, s, CreateJobOptions{Prompt: prompt("poll"), Schedule: "every 15m"})
	u, err := s.updateJob(rec.ID, map[string]any{"schedule": "in 15m"})
	if err != nil || u.Schedule.Kind != "once" || u.Repeat.Times == nil || *u.Repeat.Times != 1 {
		t.Fatalf("recurring to once = %+v %v", u, err)
	}
}

func TestRejectsStalePastOneshotAtCreation(t *testing.T) {
	now := time.Date(2026, 3, 18, 4, 30, 0, 0, time.UTC)
	s := newClockStore(t, &now, time.UTC)
	_, err := s.createJob(CreateJobOptions{Prompt: prompt("Too late"), Schedule: FormatISO(now.Add(-5 * time.Minute))})
	if err == nil || !strings.Contains(err.Error(), "past and cannot be scheduled") {
		t.Fatalf("stale one-shot accepted: %v", err)
	}
	if jobs, _ := s.LoadJobs(); len(jobs) != 0 {
		t.Fatal("rejected job was stored")
	}
}

func TestUpdateJobName(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Check server status"), Schedule: "every 1h", Name: prompt("Old Name")})
	updated, err := s.updateJob(job.ID, map[string]any{"name": "New Name"})
	if err != nil || updated.Name != "New Name" || updated.Prompt != "Check server status" || updated.ID != job.ID ||
		updated.Schedule.Expr != job.Schedule.Expr || updated.Schedule.Minutes != job.Schedule.Minutes {
		t.Fatalf("updated = %+v %v", updated, err)
	}
	if fetched, _ := s.getJob(job.ID); fetched.Name != "New Name" {
		t.Fatal("rename not persisted")
	}
}
