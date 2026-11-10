package cron

import (
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

func timezoneStore(t *testing.T, zone string) *Store {
	t.Helper()
	config.ResetTimezoneCache()
	t.Cleanup(config.ResetTimezoneCache)
	t.Setenv("ICEBATH_TIMEZONE", zone)
	s, _ := newTestStore(t)
	config.ResetTimezoneCache()
	return s
}

func awareISO(t *testing.T, text string) ISOTime {
	t.Helper()
	parsed, err := ParseISO(text)
	if err != nil {
		t.Fatalf("%q is not ISO text: %v", text, err)
	}
	return parsed
}

func TestCronTimezoneParseScheduleOneShotDurationUsesTzAwareNow(t *testing.T) {
	s := timezoneStore(t, "Asia/Kolkata")
	parsed, err := s.parseSchedule("in 30m")
	if err != nil {
		t.Fatal(err)
	}
	if runAt := awareISO(t, parsed.RunAt); !runAt.Aware {
		t.Fatalf("run_at %q is naive", parsed.RunAt)
	}
}

func TestCronTimezoneEnsureAwareNaivePreservesAbsoluteTime(t *testing.T) {
	s := timezoneStore(t, "Asia/Kolkata")
	naive := awareISO(t, "2026-03-11T12:00:00")
	if naive.Aware {
		t.Fatal("fixture parsed as aware")
	}
	result := s.clk.ensureAware(naive, nil)
	if _, offset := result.Zone(); offset != 5*3600+30*60 {
		t.Fatalf("result %v is not in the Kolkata zone", result)
	}
	_, systemOffset := time.Now().Zone()
	expected := time.Date(2026, 3, 11, 12, 0, 0, 0, time.FixedZone("system", systemOffset))
	if !result.Equal(expected) {
		t.Fatalf("Absolute time shifted: expected %v, got %v", expected.UTC(), result.UTC())
	}
}

func TestCronTimezoneGetDueJobsNaiveCrossTimezone(t *testing.T) {
	s := timezoneStore(t, "Pacific/Midway")
	mustCreate(t, s, CreateJobOptions{Prompt: prompt("Cross-tz job"), Schedule: "every 1h"})
	naivePast := time.Now().Add(-30 * time.Second).Format("2006-01-02T15:04:05.000000")
	editJobs(t, s, func(j *Job) { j.NextRunAt = &naivePast })
	if due := mustDue(t, s); len(due) != 1 {
		t.Fatalf("Naive past timestamp should be due regardless of the configured timezone, got %v", jobIDs(due))
	}
}

func TestCronTimezoneCreateJobStoresTzAwareTimestamps(t *testing.T) {
	s := timezoneStore(t, "US/Eastern")
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("TZ test"), Schedule: "every 2h"})
	if created := awareISO(t, job.CreatedAt); !created.Aware {
		t.Fatalf("created_at %q is naive", job.CreatedAt)
	}
	if nextRun := awareISO(t, deref(job.NextRunAt)); !nextRun.Aware {
		t.Fatalf("next_run_at %q is naive", deref(job.NextRunAt))
	}
}
