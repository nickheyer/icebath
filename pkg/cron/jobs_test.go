package cron

import (
	"context"
	"os"
	"reflect"
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

func mustParseDuration(t *testing.T, in string, want int) {
	t.Helper()
	if got, err := parseDuration(in); err != nil || got != want {
		t.Fatalf("parseDuration(%q) = %d, %v want %d", in, got, err, want)
	}
}

func mustReject(t *testing.T, s *Store, in string) {
	t.Helper()
	if sched, err := s.parseSchedule(in); err == nil {
		t.Fatalf("parseSchedule(%q) accepted as %+v", in, sched)
	}
}

func mustCronExpr(t *testing.T, s *Store, in, want string) {
	t.Helper()
	if sched := mustParse(t, s, in); sched.Kind != "cron" || sched.Expr != want {
		t.Fatalf("parseSchedule(%q) = %+v want cron %q", in, sched, want)
	}
}

func TestParseDurationMinutes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{{"30m", 30}, {"1min", 1}, {"5mins", 5}, {"10minute", 10}, {"120minutes", 120}} {
		mustParseDuration(t, tc.in, tc.want)
	}
}

func TestParseDurationBareUnitsDefaultToOne(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{{"hour", 60}, {"hr", 60}, {"h", 60}, {"minute", 1}, {"m", 1}, {"day", 1440}, {"d", 1440}} {
		mustParseDuration(t, tc.in, tc.want)
	}
}

func TestParseDurationEveryBareUnitSchedule(t *testing.T) {
	s, _ := newTestStore(t)
	if sched := mustParse(t, s, "every hour"); sched.Kind != "interval" || sched.Minutes != 60 {
		t.Fatalf("every hour = %+v", sched)
	}
	if sched := mustParse(t, s, "every day"); sched.Kind != "interval" || sched.Minutes != 1440 {
		t.Fatalf("every day = %+v", sched)
	}
}

func TestParseDurationInvalidRaises(t *testing.T) {
	for _, in := range []string{"abc", "hourx", "30x", "", "m30"} {
		if _, err := parseDuration(in); err == nil {
			t.Fatalf("parseDuration(%q) accepted", in)
		}
	}
}

func TestParseScheduleBareDurationBecomesRecurringInterval(t *testing.T) {
	s, _ := newTestStore(t)
	if sched := mustParse(t, s, "30m"); sched.Kind != "interval" || sched.Minutes != 30 || sched.RunAt != "" {
		t.Fatalf("bare duration = %+v", sched)
	}
}

func TestParseScheduleInDurationBecomesOnce(t *testing.T) {
	s, _ := newTestStore(t)
	sched := mustParse(t, s, "in 30m")
	runAt, err := ParseISO(sched.RunAt)
	now := time.Now()
	if sched.Kind != "once" || err != nil || !runAt.Time.After(now) || !runAt.Time.Before(now.Add(31*time.Minute)) {
		t.Fatalf("in 30m = %+v %v", sched, err)
	}
}

func TestParseScheduleEveryBecomesInterval(t *testing.T) {
	s, _ := newTestStore(t)
	if sched := mustParse(t, s, "every 2h"); sched.Kind != "interval" || sched.Minutes != 120 {
		t.Fatalf("every 2h = %+v", sched)
	}
}

func TestParseScheduleEveryWeekdayTimeBecomesCron(t *testing.T) {
	s, _ := newTestStore(t)
	if sched := mustParse(t, s, "every monday 9am"); sched.Kind != "cron" || sched.Expr != "0 9 * * 1" || sched.Display != "every monday 9am" {
		t.Fatalf("every monday 9am = %+v", sched)
	}
}

func TestParseScheduleEverySundayMapsToZero(t *testing.T) {
	s, _ := newTestStore(t)
	mustCronExpr(t, s, "every sunday 9am", "0 9 * * 0")
}

func TestParseScheduleEveryWeekdayAbbreviations(t *testing.T) {
	s, _ := newTestStore(t)
	mustCronExpr(t, s, "every mon 9am", "0 9 * * 1")
	mustCronExpr(t, s, "every fri 5pm", "0 17 * * 5")
}

func TestParseScheduleEveryDayKeywordIsDaily(t *testing.T) {
	s, _ := newTestStore(t)
	mustCronExpr(t, s, "every day at 9am", "0 9 * * *")
	mustCronExpr(t, s, "every day 7am", "0 7 * * *")
}

func TestParseScheduleEveryWeekdayKeywordIsBusinessDays(t *testing.T) {
	s, _ := newTestStore(t)
	mustCronExpr(t, s, "every weekday at 9am", "0 9 * * 1-5")
}

func TestParseScheduleEveryWeekendKeyword(t *testing.T) {
	s, _ := newTestStore(t)
	mustCronExpr(t, s, "every weekend at 10am", "0 10 * * 0,6")
}

func TestParseScheduleNoEveryPrefixNaturalForms(t *testing.T) {
	s, _ := newTestStore(t)
	mustCronExpr(t, s, "weekdays at 9am", "0 9 * * 1-5")
	mustCronExpr(t, s, "monday at 9:30", "30 9 * * 1")
	mustCronExpr(t, s, "daily at 7am", "0 7 * * *")
}

func TestParseScheduleWeekdayListForms(t *testing.T) {
	s, _ := newTestStore(t)
	mustCronExpr(t, s, "Monday, Wednesday at 9am", "0 9 * * 1,3")
	mustCronExpr(t, s, "monday and friday at 5pm", "0 17 * * 1,5")
	mustCronExpr(t, s, "every tue, thu 8am", "0 8 * * 2,4")
}

func TestParseScheduleNaturalFormNegativesStillReject(t *testing.T) {
	s, _ := newTestStore(t)
	mustReject(t, s, "monday banana at 9am")
	mustReject(t, s, "funday at 9am")
}

func TestParseScheduleEveryTimeFormats(t *testing.T) {
	s, _ := newTestStore(t)
	mustCronExpr(t, s, "every monday 14:30", "30 14 * * 1")
	mustCronExpr(t, s, "every monday 9:05am", "5 9 * * 1")
	mustCronExpr(t, s, "every monday noon", "0 12 * * 1")
	mustCronExpr(t, s, "every monday midnight", "0 0 * * 1")
	mustCronExpr(t, s, "every monday at 7", "0 7 * * 1")
}

func TestParseScheduleEvery12HourBoundaries(t *testing.T) {
	s, _ := newTestStore(t)
	mustCronExpr(t, s, "every monday 12am", "0 0 * * 1")
	mustCronExpr(t, s, "every monday 12pm", "0 12 * * 1")
}

func TestParseScheduleEveryWeekdayTimeIsCaseInsensitive(t *testing.T) {
	s, _ := newTestStore(t)
	mustCronExpr(t, s, "Every Monday 9AM", "0 9 * * 1")
}

func TestParseScheduleEveryWeekdayScheduleComputesNextRun(t *testing.T) {
	s, _ := newTestStore(t)
	next, err := s.computeNextRun(mustParse(t, s, "every monday 9am"), nil, nil)
	if err != nil || next == nil {
		t.Fatalf("weekly next = %v %v", next, err)
	}
	dt, _ := ParseISO(*next)
	if dt.Time.Weekday() != time.Monday || dt.Time.Hour() != 9 || dt.Time.Minute() != 0 {
		t.Fatalf("weekly next = %s", *next)
	}
}

func TestParseScheduleEveryDurationStillInterval(t *testing.T) {
	s, _ := newTestStore(t)
	if sched := mustParse(t, s, "every 30m"); sched.Kind != "interval" {
		t.Fatalf("every 30m = %+v", sched)
	}
	if sched := mustParse(t, s, "every 1d"); sched.Minutes != 1440 {
		t.Fatalf("every 1d = %+v", sched)
	}
}

func TestParseScheduleEveryWeekdayWithoutTimeRaises(t *testing.T) {
	s, _ := newTestStore(t)
	mustReject(t, s, "every monday")
}

func TestParseScheduleEveryInvalidTimeRaises(t *testing.T) {
	s, _ := newTestStore(t)
	mustReject(t, s, "every monday 25am")
	mustReject(t, s, "every monday 9pm pizza")
}

func TestParseScheduleCronExpression(t *testing.T) {
	s, _ := newTestStore(t)
	mustCronExpr(t, s, "0 9 * * *", "0 9 * * *")
}

func TestParseScheduleCronNamedWeekdaysAndMonths(t *testing.T) {
	s, _ := newTestStore(t)
	for _, expr := range []string{"0 9 * * MON", "*/15 9-17 * * MON-FRI", "0 9 1 JAN *", "0 9 * * MON,WED,FRI"} {
		mustCronExpr(t, s, expr, expr)
	}
}

func TestParseScheduleInvalidNamedCronStillRejected(t *testing.T) {
	s, _ := newTestStore(t)
	mustReject(t, s, "0 9 * * FUNDAY")
}

func TestParseScheduleIsoTimestamp(t *testing.T) {
	s, _ := newTestStore(t)
	if sched := mustParse(t, s, "2030-01-15T14:00:00"); sched.Kind != "once" || !strings.Contains(sched.RunAt, "2030-01-15") {
		t.Fatalf("iso = %+v", sched)
	}
}

func TestParseScheduleNaiveIsoAnchorsToConfiguredTzNotServerLocal(t *testing.T) {
	kolkata := time.FixedZone("", 5*3600+1800)
	now := time.Date(2026, 6, 22, 20, 0, 0, 0, kolkata)
	s := newClockStore(t, &now, kolkata)
	sched := mustParse(t, s, "2026-06-22T20:07:00")
	parsed, err := ParseISO(sched.RunAt)
	if sched.Kind != "once" || err != nil {
		t.Fatalf("naive iso = %+v %v", sched, err)
	}
	if _, off := parsed.Time.Zone(); off != 5*3600+1800 {
		t.Fatalf("offset %d of %s", off, sched.RunAt)
	}
	if wall := parsed.Time.Format("2006-01-02T15:04:05"); wall != "2026-06-22T20:07:00" {
		t.Fatalf("wall clock %s", wall)
	}
}

func TestNaiveScheduleTimezoneDivergenceRecentPastOneshotIsDueUnderDivergingTz(t *testing.T) {
	kolkata := time.FixedZone("", 5*3600+1800)
	now := time.Date(2026, 6, 22, 20, 7, 30, 0, kolkata)
	s := newClockStore(t, &now, kolkata)
	naive := now.Add(-30 * time.Second).Format("2006-01-02T15:04:05")
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("test message"), Schedule: naive, Deliver: prompt("local")})
	due, err := s.getDueJobs(context.Background())
	if err != nil || !containsString(jobIDs(due), job.ID) {
		t.Fatalf("one-shot should be due; next_run_at=%v due=%v err=%v", deref(job.NextRunAt), jobIDs(due), err)
	}
}

func TestComputeNextRunOnceFutureReturnsTime(t *testing.T) {
	s, _ := newTestStore(t)
	future := time.Now().Add(time.Hour).Format("2006-01-02T15:04:05.000000")
	if got, err := s.computeNextRun(&Schedule{Kind: "once", RunAt: future}, nil, nil); err != nil || deref(got) != future {
		t.Fatalf("future once = %v %v", got, err)
	}
}

func TestComputeNextRunOnceRecentPastWithinGraceReturnsTime(t *testing.T) {
	now := time.Date(2026, 3, 18, 4, 22, 3, 0, time.UTC)
	s := newClockStore(t, &now, time.UTC)
	runAt := "2026-03-18T04:22:00+00:00"
	if got, err := s.computeNextRun(&Schedule{Kind: "once", RunAt: runAt}, nil, nil); err != nil || deref(got) != runAt {
		t.Fatalf("within grace = %v %v", got, err)
	}
}

func TestComputeNextRunOnceWithLastRunReturnsNoneEvenWithinGrace(t *testing.T) {
	now := time.Date(2026, 3, 18, 4, 22, 3, 0, time.UTC)
	s := newClockStore(t, &now, time.UTC)
	last := FormatISO(now)
	if got, err := s.computeNextRun(&Schedule{Kind: "once", RunAt: "2026-03-18T04:22:00+00:00"}, &last, nil); err != nil || got != nil {
		t.Fatalf("already run = %v %v", got, err)
	}
}

func TestComputeNextRunIntervalFirstRun(t *testing.T) {
	s, _ := newTestStore(t)
	got, err := s.computeNextRun(&Schedule{Kind: "interval", Minutes: 60, HasMinutes: true}, nil, nil)
	next, perr := ParseISO(deref(got))
	if err != nil || perr != nil || !next.Time.After(time.Now().Add(59*time.Minute)) {
		t.Fatalf("interval first run = %v %v", got, err)
	}
}

func TestJobCRUDCjkAndEmojiRoundTripReadableInJobsJson(t *testing.T) {
	s, _ := newTestStore(t)
	name := "日次レポート 🎉 café"
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Summarize " + name), Schedule: "30m", Name: &name})
	fetched := mustGet(t, s, job.ID)
	if fetched.Name != name || !strings.Contains(fetched.Prompt, name) {
		t.Fatalf("fetched = %+v", fetched)
	}
	raw, _ := os.ReadFile(s.jobsFile)
	if !strings.Contains(string(raw), "日次レポート") || !strings.Contains(string(raw), "🎉") || strings.Contains(string(raw), "\\"+"u65e5") {
		t.Fatalf("jobs.json escapes non ASCII: %s", raw)
	}
}

func TestJobCRUDCreateAndGet(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Check server status"), Schedule: "30m"})
	if job.ID == "" || job.Prompt != "Check server status" || !job.Enabled || job.Schedule.Kind != "interval" {
		t.Fatalf("created = %+v", job)
	}
	if fetched, _ := s.getJob(job.ID); fetched == nil || fetched.Prompt != "Check server status" {
		t.Fatal("get lost the job")
	}
}

func TestJobCRUDListJobs(t *testing.T) {
	s, _ := newTestStore(t)
	mustCreate(t, s, CreateJobOptions{Prompt: prompt("Job 1"), Schedule: "every 1h"})
	mustCreate(t, s, CreateJobOptions{Prompt: prompt("Job 2"), Schedule: "every 2h"})
	if jobs, err := s.listJobs(false); err != nil || len(jobs) != 2 {
		t.Fatalf("list = %d %v", len(jobs), err)
	}
}

func TestJobCRUDRemoveJob(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Temp job"), Schedule: "30m"})
	if ok, err := s.removeJob(context.Background(), job.ID); !ok || err != nil {
		t.Fatalf("remove = %v %v", ok, err)
	}
	if fetched, _ := s.getJob(job.ID); fetched != nil {
		t.Fatal("removed job still present")
	}
}

func TestJobCRUDAutoRepeatForOnce(t *testing.T) {
	s, _ := newTestStore(t)
	once := mustCreate(t, s, CreateJobOptions{Prompt: prompt("One-shot"), Schedule: "in 1h"})
	if once.Repeat.Times == nil || *once.Repeat.Times != 1 {
		t.Fatalf("one-shot repeat = %+v", once.Repeat)
	}
}

func TestJobCRUDRepeatStringFormsCoerced(t *testing.T) {
	s, _ := newTestStore(t)
	for _, tc := range []struct {
		in   string
		want *int
	}{{"forever", nil}, {"once", intPtr(1)}, {"3", intPtr(3)}} {
		job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Str"), Schedule: "every 1h", Repeat: tc.in})
		if (tc.want == nil) != (job.Repeat.Times == nil) || (tc.want != nil && *tc.want != *job.Repeat.Times) {
			t.Fatalf("repeat %q = %v", tc.in, job.Repeat.Times)
		}
	}
	if _, err := s.createJob(CreateJobOptions{Prompt: prompt("Bad"), Schedule: "every 1h", Repeat: "banana"}); err == nil || !strings.Contains(err.Error(), "Invalid repeat") {
		t.Fatalf("banana accepted: %v", err)
	}
}

func TestJobCRUDUpdateRepeatStringFormsCoerced(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("t"), Schedule: "every 1h", Repeat: 2})
	mustMark(t, s, job.ID, true, MarkOptions{})
	updated, err := s.updateJob(job.ID, map[string]any{"repeat": "forever"})
	if err != nil || updated.Repeat.Times != nil || updated.Repeat.Completed != 1 {
		t.Fatalf("update forever = %+v %v", updated.Repeat, err)
	}
	if _, err := s.markJobRun(ctx, job.ID, true, MarkOptions{}); err != nil {
		t.Fatalf("mark after forever: %v", err)
	}
	updated, err = s.updateJob(job.ID, map[string]any{"repeat": map[string]any{"times": "3"}})
	if err != nil || updated.Repeat.Times == nil || *updated.Repeat.Times != 3 || updated.Repeat.Completed != 2 {
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
	for _, tc := range []struct {
		value string
		want  int
	}{{`"2"`, 3}, {"1.0", 2}, {`"junk"`, 1}, {"-2", 1}, {"Infinity", 1}} {
		setCompleted(tc.value)
		s.markJobRun(ctx, job.ID, true, MarkOptions{})
		if got := completed(); got != tc.want {
			t.Fatalf("completed %s then run = %d, want %d", tc.value, got, tc.want)
		}
	}
}

func TestJobCRUDOneshotTurnedRecurringBecomesForever(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("poll"), Schedule: "in 15m"})
	if job.Repeat.Times == nil || *job.Repeat.Times != 1 {
		t.Fatalf("one-shot repeat = %+v", job.Repeat)
	}
	updated, err := s.updateJob(job.ID, map[string]any{"schedule": "every 15m"})
	if err != nil || updated.Schedule.Kind != "interval" || updated.Repeat.Times != nil {
		t.Fatalf("once to recurring = %+v %v", updated, err)
	}
	explicit := mustCreate(t, s, CreateJobOptions{Prompt: prompt("poll"), Schedule: "in 15m"})
	if u, err := s.updateJob(explicit.ID, map[string]any{"schedule": "every 15m", "repeat": 3}); err != nil || u.Repeat.Times == nil || *u.Repeat.Times != 3 {
		t.Fatalf("explicit repeat lost %v", err)
	}
}

func TestJobCRUDRecurringTurnedOneshotBecomesOnce(t *testing.T) {
	s, _ := newTestStore(t)
	rec := mustCreate(t, s, CreateJobOptions{Prompt: prompt("poll"), Schedule: "every 15m"})
	if rec.Repeat.Times != nil {
		t.Fatalf("recurring repeat = %+v", rec.Repeat)
	}
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

func TestJobCRUDDefaultDeliveryOrigin(t *testing.T) {
	s, _ := newTestStore(t)
	origin := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Test"), Schedule: "30m",
		Origin: map[string]any{"platform": "telegram", "chat_id": "123"}})
	if deref(origin.Deliver) != "origin" {
		t.Fatalf("deliver = %v", deref(origin.Deliver))
	}
}

func TestUpdateJobUpdateName(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Check server status"), Schedule: "every 1h", Name: prompt("Old Name")})
	if job.Name != "Old Name" {
		t.Fatalf("created name %q", job.Name)
	}
	updated, err := s.updateJob(job.ID, map[string]any{"name": "New Name"})
	if err != nil || updated == nil || updated.Name != "New Name" || updated.Prompt != "Check server status" || updated.ID != job.ID ||
		!reflect.DeepEqual(updated.Schedule, job.Schedule) {
		t.Fatalf("updated = %+v %v", updated, err)
	}
	if fetched := mustGet(t, s, job.ID); fetched.Name != "New Name" {
		t.Fatal("rename not persisted")
	}
}
