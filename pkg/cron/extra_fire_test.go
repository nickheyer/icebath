package cron

import (
	"testing"
	"time"
)

func TestExtraFireSettleRecomputesInTheJobZone(t *testing.T) {
	la, _ := time.LoadLocation("America/Los_Angeles")
	ny, _ := time.LoadLocation("America/New_York")
	hk, _ := time.LoadLocation("Asia/Hong_Kong")
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, ny)
	s := newClockStore(t, &now, la)
	job := &Job{Schedule: mustParse(t, s, "0 9 * * *"), Timezone: strPtr("Asia/Hong_Kong"),
		NextRunAt: strPtr(FormatISO(time.Date(2026, 10, 22, 9, 0, 0, 0, hk)))}
	if err := extraFireSettle(s, job, s.clk.nowISO()); err != nil {
		t.Fatal(err)
	}
	got, _ := ParseISO(*job.NextRunAt)
	if !got.Time.Equal(time.Date(2026, 10, 23, 9, 0, 0, 0, hk)) {
		t.Fatalf("settled to %s", *job.NextRunAt)
	}
}

func TestExtraFireMarkersAndPendingSlot(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s := newClockStore(t, &now, time.UTC)
	trigger := FormatISO(now)
	future := FormatISO(now.Add(time.Hour))
	job := &Job{NextRunAt: strPtr(trigger), ManualRunAt: strPtr(trigger), ManualRunRestoreAt: &NullString{Valid: true, S: future},
		ManualRunPrompt: strPtr("again")}
	if !extraFireQueued(job) || deref(extraFireRestoreTarget(job)) != future {
		t.Fatal("queued trigger not recognised")
	}
	held := extraFireHeldMarkers(job)
	if held["manual_run_at"] != trigger || held[restoreKey] != future || held["manual_run_prompt"] != "again" {
		t.Fatalf("held %v", held)
	}
	if slot, ok := extraFirePendingSlot(s, job, now, false); !ok || slot != future {
		t.Fatalf("restored slot %q %v", slot, ok)
	}
	job.FireClaim = &Claim{By: "me"}
	extraFireStampClaim(job, future)
	if !extraFireIsExtra(job) || deref(job.NextRunAt) != future || job.ManualRunRestoreAt != nil || extraFireQueued(job) {
		t.Fatalf("stamped %+v", job)
	}
	if len(extraFireHeldMarkers(job)) != 0 || deref(extraFireRestoreTarget(job)) != future {
		t.Fatal("unqueued job held markers")
	}
	plain := &Job{NextRunAt: strPtr(future)}
	if _, ok := extraFirePendingSlot(s, plain, now, false); ok {
		t.Fatal("scheduled fire took a pending slot")
	}
	if slot, ok := extraFirePendingSlot(s, plain, now, true); !ok || slot != future {
		t.Fatal("manual fire lost the future slot")
	}
	extraFireReschedule(plain, trigger)
	if deref(plain.NextRunAt) != trigger || plain.ManualRunAt != nil {
		t.Fatalf("rescheduled %+v", plain)
	}
	once := &Job{Schedule: &Schedule{Kind: "once"}, NextRunAt: strPtr(FormatISO(now.Add(-time.Hour)))}
	extraFireSettle(s, once, trigger)
	if deref(once.NextRunAt) != trigger {
		t.Fatalf("one shot settled to %s", deref(once.NextRunAt))
	}
}
