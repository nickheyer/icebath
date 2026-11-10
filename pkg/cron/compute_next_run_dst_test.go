package cron

import (
	"testing"
	"time"
)

var morningSchedule = &Schedule{Kind: "cron", Expr: "0 9 * * *"}

func mustLoadZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// Store whose zone is configured through ICEBATH_TIMEZONE, the production resolution path
func torontoStore(t *testing.T) (*Store, *time.Location) {
	t.Helper()
	t.Setenv("ICEBATH_TIMEZONE", "America/Toronto")
	s, _ := newTestStore(t)
	return s, mustLoadZone(t, "America/Toronto")
}

func mustNext(t *testing.T, s *Store, sched *Schedule, last string) time.Time {
	t.Helper()
	next, err := s.computeNextRun(sched, &last, nil)
	if err != nil || next == nil {
		t.Fatalf("compute_next_run(%+v, %s) = %v %v", sched, last, next, err)
	}
	at, err := ParseISO(*next)
	if err != nil {
		t.Fatal(err)
	}
	return at.Time
}

// Instant of a wall clock in loc, fold one picking the second occurrence of a repeated hour
func wallFold(t *testing.T, loc *time.Location, wall string, fold int) time.Time {
	t.Helper()
	naive, err := time.Parse("2006-01-02T15:04:05", wall)
	if err != nil {
		t.Fatal(err)
	}
	first, second := wallInZone(naive, loc)
	if fold == 1 {
		return second
	}
	return first
}

func TestCronNextRunHoldsConfiguredWallClockDstTransitionDaysFireAt9amLocal(t *testing.T) {
	s, toronto := torontoStore(t)
	spring := mustNext(t, s, morningSchedule, "2026-03-07T09:00:00-05:00").In(toronto)
	fall := mustNext(t, s, morningSchedule, "2026-10-31T09:00:00-04:00").In(toronto)
	if spring.Hour() != 9 || spring.Minute() != 0 || fall.Hour() != 9 || fall.Minute() != 0 {
		t.Fatalf("spring %s fall %s", spring, fall)
	}
}

func TestCronNextRunHoldsConfiguredWallClockFullYearWalkNeverDrifts(t *testing.T) {
	s, toronto := torontoStore(t)
	last := time.Date(2026, 1, 1, 9, 0, 0, 0, toronto)
	for i := 0; i < 365; i++ {
		next := mustNext(t, s, morningSchedule, FormatISO(last))
		if wall := next.In(toronto); wall.Hour() != 9 || wall.Minute() != 0 {
			t.Fatalf("drift at %s: %s", FormatISO(last), FormatISO(wall))
		}
		last = next
	}
}

func TestFallBackStrictlyAfterContractBaseInSecondOccurrenceGetsFutureInstant(t *testing.T) {
	s, toronto := torontoStore(t)
	base := wallFold(t, toronto, "2026-11-01T01:15:00", 1)
	next := mustNext(t, s, &Schedule{Kind: "cron", Expr: "30 1 * * *"}, FormatISO(base))
	if !next.After(base) {
		t.Fatalf("next %s not after base %s", FormatISO(next), FormatISO(base))
	}
	if wall := next.In(toronto); wall.Hour() != 1 || wall.Minute() != 30 {
		t.Fatalf("wall %s", FormatISO(wall))
	}
}

func TestFallBackStrictlyAfterContractEveryMinuteNeverReturnsPast(t *testing.T) {
	s, toronto := torontoStore(t)
	base := wallFold(t, toronto, "2026-11-01T01:59:00", 0)
	if next := mustNext(t, s, &Schedule{Kind: "cron", Expr: "* * * * *"}, FormatISO(base)); !next.After(base) {
		t.Fatalf("next %s not after base %s", FormatISO(next), FormatISO(base))
	}
}

func TestCronComputeNextRunUsesLastRunAtCronUsesLastRunAtForEvery6hSchedule(t *testing.T) {
	morocco := mustLoadZone(t, "Africa/Casablanca")
	now := time.Date(2026, 4, 10, 22, 0, 0, 0, morocco)
	s := newClockStore(t, &now, morocco)
	last := time.Date(2026, 4, 6, 14, 10, 0, 0, morocco)
	next := mustNext(t, s, &Schedule{Kind: "cron", Expr: "0 */6 * * *"}, FormatISO(last))
	if next.Format("2006-01-02") != "2026-04-06" || next.Hour() != 18 {
		t.Fatalf("Expected next run on Apr 6 (from last_run_at), got %s", FormatISO(next))
	}
}

// Start instants of the duration DST cases, each a zone, a wall clock and its fold, the patched now carrying the zone
var durationStarts = []struct {
	zone, wall string
	fold       int
}{
	{"America/Toronto", "2026-03-08T01:30:00", 0},
	{"America/Toronto", "2026-11-01T00:30:00", 0},
	{"America/Toronto", "2026-11-01T01:30:00", 1},
	{"Australia/Lord_Howe", "2026-04-05T01:45:00", 0},
	{"Australia/Lord_Howe", "2026-10-04T01:45:00", 0},
	{"UTC", "2026-03-08T01:30:00", 0},
}

// Run instant two hours after start in elapsed time whose offset is the zone's own offset at that instant
func checkElapsedTwoHours(t *testing.T, runAt string, start time.Time) {
	t.Helper()
	at, err := ParseISO(runAt)
	if err != nil {
		t.Fatal(err)
	}
	if got := at.Time.UTC().Sub(start.UTC()); got != 2*time.Hour {
		t.Fatalf("%s is %s after %s", runAt, got, FormatISO(start))
	}
	_, off := at.Time.Zone()
	if _, want := at.Time.In(start.Location()).Zone(); off != want {
		t.Fatalf("%s offset %d want %d", runAt, off, want)
	}
}

func TestOneShotDelayPreservesElapsedDuration(t *testing.T) {
	for _, tc := range durationStarts {
		t.Run(tc.zone+"_"+tc.wall, func(t *testing.T) {
			start := wallFold(t, mustLoadZone(t, tc.zone), tc.wall, tc.fold)
			s := newClockStore(t, &start, start.Location())
			checkElapsedTwoHours(t, mustParse(t, s, "in 2h").RunAt, start)
		})
	}
}

func TestIntervalPreservesElapsedDuration(t *testing.T) {
	for _, tc := range durationStarts {
		for _, resume := range []bool{false, true} {
			t.Run(tc.zone+"_"+tc.wall+map[bool]string{true: "_resume", false: ""}[resume], func(t *testing.T) {
				start := wallFold(t, mustLoadZone(t, tc.zone), tc.wall, tc.fold)
				now := start
				var last *string
				if resume {
					now = start.Add(48 * time.Hour)
					last = strPtr(FormatISO(start))
				}
				s := newClockStore(t, &now, start.Location())
				next, err := s.computeNextRun(mustParse(t, s, "every 2h"), last, nil)
				if err != nil || next == nil {
					t.Fatalf("next %v %v", next, err)
				}
				checkElapsedTwoHours(t, *next, start)
			})
		}
	}
}
