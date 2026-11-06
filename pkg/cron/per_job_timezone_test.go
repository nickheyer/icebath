package cron

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	weekday9AM = "0 9 * * 1-5"
	daily9AM   = "0 9 * * *"
)

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// Store pinned to the Los Angeles profile zone with a settable clock and its log captured
func laStore(t *testing.T) (*Store, *fakeClock, *bytes.Buffer) {
	t.Helper()
	t.Setenv("ICEBATH_TIMEZONE", "America/Los_Angeles")
	s, buf := newTestStore(t)
	la := mustZone(t, "America/Los_Angeles")
	fc := &fakeClock{t: time.Date(2026, 10, 22, 12, 0, 0, 0, mustZone(t, "America/New_York"))}
	s.clk.nowFn = fc.Now
	s.clk.zoneFn = func() *time.Location { return la }
	return s, fc, buf
}

// Creates a job through the public door then parks its instant where the test needs it
func addZonedJob(t *testing.T, s *Store, schedule string, timezone any, grace any, nextRunAt time.Time) string {
	t.Helper()
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("probe"), Schedule: schedule, Model: "fixture", Deliver: prompt("local"), Timezone: timezone, GraceMinutes: grace})
	editJobs(t, s, func(j *Job) {
		if j.ID == job.ID {
			j.NextRunAt = strPtr(FormatISO(nextRunAt))
		}
	})
	return job.ID
}

func storedJob(t *testing.T, s *Store, id string) *Job {
	t.Helper()
	jobs, err := s.LoadJobs()
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.ID == id {
			return j
		}
	}
	t.Fatalf("job %s not stored", id)
	return nil
}

func dueIDList(t *testing.T, s *Store) []string {
	t.Helper()
	return jobIDs(mustDue(t, s))
}

func parseInstant(t *testing.T, text *string) time.Time {
	t.Helper()
	if text == nil {
		t.Fatal("instant is null")
	}
	parsed, err := ParseISO(*text)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Time
}

func rawStoredRows(t *testing.T, s *Store) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(s.JobsFile())
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Jobs
}

func TestJobEastOfTheProfileZoneFiresOnTime(t *testing.T) {
	for _, zone := range []string{"America/New_York", "Asia/Hong_Kong"} {
		t.Run(zone, func(t *testing.T) {
			s, fc, buf := laStore(t)
			nine := time.Date(2026, 10, 22, 9, 0, 0, 0, mustZone(t, zone))
			fc.Set(nine)
			id := addZonedJob(t, s, weekday9AM, zone, nil, nine)
			if got := dueIDList(t, s); strings.Join(got, ",") != id {
				t.Fatalf("due %v want [%s]", got, id)
			}
			if got := deref(storedJob(t, s, id).NextRunAt); got != FormatISO(nine) {
				t.Fatalf("pinned instant rewritten to %s", got)
			}
			if strings.Contains(buf.String(), "Recomputing cron run") {
				t.Fatal("offset repair ran for a pinned job")
			}
		})
	}
}

func TestTwoPinnedJobsKeepTheirOwnNineAM(t *testing.T) {
	s, fc, _ := laStore(t)
	ny, hk := mustZone(t, "America/New_York"), mustZone(t, "Asia/Hong_Kong")
	nyJob := addZonedJob(t, s, weekday9AM, "America/New_York", nil, time.Date(2026, 10, 22, 9, 0, 0, 0, ny))
	hkJob := addZonedJob(t, s, weekday9AM, "Asia/Hong_Kong", nil, time.Date(2026, 10, 23, 9, 0, 0, 0, hk))
	fc.Set(time.Date(2026, 10, 22, 9, 0, 0, 0, ny))
	if got := dueIDList(t, s); strings.Join(got, ",") != nyJob {
		t.Fatalf("due %v want only the New York job", got)
	}
	if _, ok, err := s.claimJobForFire(context.Background(), nyJob, FireClaimOptions{}); err != nil || !ok {
		t.Fatalf("claim %v %v", ok, err)
	}
	fc.Set(time.Date(2026, 10, 23, 9, 0, 0, 0, hk))
	if got := dueIDList(t, s); strings.Join(got, ",") != hkJob {
		t.Fatalf("due %v want only the Hong Kong job", got)
	}
	if h := parseInstant(t, storedJob(t, s, nyJob).NextRunAt).In(ny).Hour(); h != 9 {
		t.Fatalf("New York job advanced to %d o'clock", h)
	}
}

func TestUnpinnedJobStillFollowsTheProfileZone(t *testing.T) {
	s, fc, _ := laStore(t)
	nine := time.Date(2026, 10, 22, 9, 0, 0, 0, mustZone(t, "America/Los_Angeles"))
	fc.Set(nine)
	id := addZonedJob(t, s, weekday9AM, nil, nil, nine)
	if got := dueIDList(t, s); strings.Join(got, ",") != id {
		t.Fatalf("due %v", got)
	}
}

func TestNextRunLandsOnTheJobWallClock(t *testing.T) {
	s, _, _ := laStore(t)
	ny, la := mustZone(t, "America/New_York"), mustZone(t, "America/Los_Angeles")
	sched, err := s.parseSchedule(weekday9AM)
	if err != nil {
		t.Fatal(err)
	}
	base := FormatISO(time.Date(2026, 10, 22, 10, 0, 0, 0, ny))
	pinnedText, err := s.computeNextRun(sched, &base, strPtr("America/New_York"))
	if err != nil {
		t.Fatal(err)
	}
	inheritedText, err := s.computeNextRun(sched, &base, nil)
	if err != nil {
		t.Fatal(err)
	}
	pinned, inherited := parseInstant(t, pinnedText), parseInstant(t, inheritedText)
	if pinned.Hour() != 9 || pinned.Minute() != 0 {
		t.Fatalf("pinned wall clock %s", *pinnedText)
	}
	if y, m, d := pinned.In(ny).Date(); y != 2026 || m != 10 || d != 23 {
		t.Fatalf("pinned date %s", *pinnedText)
	}
	if inherited.In(la).Hour() != 9 {
		t.Fatalf("inherited %s is not 09:00 Los Angeles", *inheritedText)
	}
	if pinned.Equal(inherited) {
		t.Fatal("pinned and inherited instants coincide")
	}
}

func TestDSTTransitionsHoldTheJobWallClock(t *testing.T) {
	s, _, _ := laStore(t)
	ny := mustZone(t, "America/New_York")
	sched, err := s.parseSchedule(daily9AM)
	if err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"2026-03-07T09:00:00-05:00", "2026-10-31T09:00:00-04:00"} {
		next, err := s.computeNextRun(sched, &base, strPtr("America/New_York"))
		if err != nil {
			t.Fatal(err)
		}
		if at := parseInstant(t, next).In(ny); at.Hour() != 9 || at.Minute() != 0 {
			t.Fatalf("%s advanced to %s", base, *next)
		}
	}
}

func TestFallBackSecondOccurrenceIsStrictlyLater(t *testing.T) {
	s, _, _ := laStore(t)
	base := time.Date(2026, 11, 1, 6, 15, 0, 0, time.UTC).In(mustZone(t, "America/New_York"))
	baseText := FormatISO(base)
	if !strings.HasSuffix(baseText, "-05:00") {
		t.Fatalf("base %s is not the second 01:15", baseText)
	}
	next, err := s.computeNextRun(&Schedule{Kind: "cron", Expr: "30 1 * * *"}, &baseText, strPtr("America/New_York"))
	if err != nil {
		t.Fatal(err)
	}
	if !parseInstant(t, next).After(base) {
		t.Fatalf("next %s does not follow %s", *next, baseText)
	}
}

func TestNullAndAbsentTimezoneAreTheSameJob(t *testing.T) {
	s, _, _ := laStore(t)
	explicit := mustCreate(t, s, CreateJobOptions{Prompt: prompt("null"), Schedule: weekday9AM, Model: "fixture", Deliver: prompt("local"), Timezone: nil})
	absent := mustCreate(t, s, CreateJobOptions{Prompt: prompt("absent"), Schedule: weekday9AM, Model: "fixture", Deliver: prompt("local")})
	for _, row := range rawStoredRows(t, s) {
		if _, has := row["timezone"]; has {
			t.Fatalf("row %v stores a timezone", row["prompt"])
		}
	}
	if deref(explicit.NextRunAt) != deref(absent.NextRunAt) {
		t.Fatalf("null %s absent %s", deref(explicit.NextRunAt), deref(absent.NextRunAt))
	}
}

func TestClearingThePinThroughUpdateDropsTheKey(t *testing.T) {
	s, _, _ := laStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("p"), Schedule: weekday9AM, Model: "fixture", Deliver: prompt("local"), Timezone: "Asia/Hong_Kong"})
	if _, err := s.updateJob(job.ID, map[string]any{"timezone": ""}); err != nil {
		t.Fatal(err)
	}
	if _, has := rawStoredRows(t, s)[0]["timezone"]; has {
		t.Fatal("cleared pin still stored")
	}
}

func TestChangingThePinRecomputesTheStoredInstant(t *testing.T) {
	s, _, _ := laStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("p"), Schedule: weekday9AM, Model: "fixture", Deliver: prompt("local"), Timezone: "America/New_York"})
	before := deref(storedJob(t, s, job.ID).NextRunAt)
	if _, err := s.updateJob(job.ID, map[string]any{"timezone": "Asia/Hong_Kong"}); err != nil {
		t.Fatal(err)
	}
	after := storedJob(t, s, job.ID).NextRunAt
	if deref(after) == before {
		t.Fatal("instant of the old zone kept")
	}
	if h := parseInstant(t, after).In(mustZone(t, "Asia/Hong_Kong")).Hour(); h != 9 {
		t.Fatalf("Hong Kong hour %d", h)
	}
}

func TestACorruptStoredZoneDoesNotBreakTheScan(t *testing.T) {
	s, fc, _ := laStore(t)
	nine := time.Date(2026, 10, 22, 9, 0, 0, 0, mustZone(t, "America/Los_Angeles"))
	fc.Set(nine)
	id := addZonedJob(t, s, weekday9AM, nil, nil, nine)
	editJobs(t, s, func(j *Job) { j.Timezone = strPtr("Mars/Olympus_Mons") })
	if got := dueIDList(t, s); strings.Join(got, ",") != id {
		t.Fatalf("due %v", got)
	}
}

// Dispatch kind stamped on an hourly job 45 minutes late under the given grace override
func lateHourlyStamp(t *testing.T, grace any) string {
	s, fc, _ := laStore(t)
	la := mustZone(t, "America/Los_Angeles")
	fc.Set(time.Date(2026, 10, 22, 12, 45, 0, 0, la))
	id := addZonedJob(t, s, "0 * * * *", "America/Los_Angeles", grace, time.Date(2026, 10, 22, 12, 0, 0, 0, la))
	if got := dueIDList(t, s); strings.Join(got, ",") != id {
		t.Fatalf("due %v", got)
	}
	stamp := storedJob(t, s, id).LastDispatch
	if stamp == nil {
		t.Fatal("no dispatch stamp")
	}
	return stamp.Kind
}

func TestComputedWindowClassifiesA45MinLateHourlyRunAsCatchUp(t *testing.T) {
	if kind := lateHourlyStamp(t, nil); kind != "catch_up" {
		t.Fatalf("kind %s", kind)
	}
}

func TestGraceOverrideWidensTheWindowSoTheSameRunIsOnlyLate(t *testing.T) {
	if kind := lateHourlyStamp(t, 120); kind != "late" {
		t.Fatalf("kind %s", kind)
	}
}

func writeCatchUpOff(t *testing.T, s *Store) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.Home(), "config.yaml"), []byte("cron:\n  catch_up_missed: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestZeroGraceSkipsOutrightWhenCatchUpIsDisabled(t *testing.T) {
	s, fc, _ := laStore(t)
	writeCatchUpOff(t, s)
	la := mustZone(t, "America/Los_Angeles")
	fc.Set(time.Date(2026, 10, 22, 12, 0, 0, 0, la))
	instant := time.Date(2026, 10, 22, 11, 0, 0, 0, la)
	never := addZonedJob(t, s, "0 11 * * *", "America/Los_Angeles", 0, instant)
	tolerant := addZonedJob(t, s, "0 11 * * *", "America/Los_Angeles", 120, instant)
	due := map[string]bool{}
	for _, id := range dueIDList(t, s) {
		due[id] = true
	}
	if !due[tolerant] {
		t.Fatal("a 2h window covers a 1h late run")
	}
	if due[never] {
		t.Fatal("grace 0 with catch up disabled must skip the miss")
	}
}

func TestGraceIsRefusedOnANonCronSchedule(t *testing.T) {
	s, _, _ := laStore(t)
	_, err := s.createJob(CreateJobOptions{Prompt: prompt("p"), Schedule: "every 5m", Model: "fixture", Deliver: prompt("local"), GraceMinutes: 30})
	if err == nil || !strings.Contains(err.Error(), "grace_minutes only applies to cron") {
		t.Fatalf("err %v", err)
	}
}

func TestStoredGraceRoundTrips(t *testing.T) {
	for _, tc := range []struct{ minutes, expected int }{{0, 120}, {45, 45 * 60}} {
		s, _, _ := laStore(t)
		job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("p"), Schedule: weekday9AM, Model: "fixture", Deliver: prompt("local"), GraceMinutes: tc.minutes})
		stored := storedJob(t, s, job.ID)
		if stored.GraceMinutes == nil || *stored.GraceMinutes != tc.minutes {
			t.Fatalf("grace %v want %d", stored.GraceMinutes, tc.minutes)
		}
		if got := s.computeGraceSeconds(stored.Schedule, stored.GraceMinutes, *stored.GraceMinutes); got != tc.expected {
			t.Fatalf("grace seconds %d want %d", got, tc.expected)
		}
	}
}

func TestOnTimeTickFiresAsScheduledWithZeroGrace(t *testing.T) {
	for _, catchUpDisabled := range []bool{false, true} {
		s, fc, _ := laStore(t)
		if catchUpDisabled {
			writeCatchUpOff(t, s)
		}
		ny := mustZone(t, "America/New_York")
		fc.Set(time.Date(2026, 10, 22, 9, 0, 20, 0, ny))
		id := addZonedJob(t, s, daily9AM, "America/New_York", 0, time.Date(2026, 10, 22, 9, 0, 0, 0, ny))
		if got := dueIDList(t, s); strings.Join(got, ",") != id {
			t.Fatalf("catch up disabled=%v due %v", catchUpDisabled, got)
		}
		if stamp := storedJob(t, s, id).LastDispatch; stamp == nil || stamp.Kind == "catch_up" {
			t.Fatalf("catch up disabled=%v stamp %+v", catchUpDisabled, stamp)
		}
	}
}

func TestExplicitGraceSkipsARealMissEvenWithDefaultCatchUp(t *testing.T) {
	s, fc, _ := laStore(t)
	ny := mustZone(t, "America/New_York")
	fc.Set(time.Date(2026, 10, 22, 10, 0, 0, 0, ny))
	id := addZonedJob(t, s, daily9AM, "America/New_York", 0, time.Date(2026, 10, 22, 9, 0, 0, 0, ny))
	if got := dueIDList(t, s); len(got) != 0 {
		t.Fatalf("due %v", got)
	}
	next := parseInstant(t, storedJob(t, s, id).NextRunAt)
	if !next.Equal(time.Date(2026, 10, 23, 9, 0, 0, 0, ny)) {
		t.Fatalf("next %v", next)
	}
}
