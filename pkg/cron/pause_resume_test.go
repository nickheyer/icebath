package cron

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func dueIDs(t *testing.T, s *Store) map[string]bool {
	t.Helper()
	due, err := s.getDueJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, j := range due {
		out[j.ID] = true
	}
	return out
}

func TestPauseSetsState(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Pause me"), Schedule: "every 1h"})
	paused, err := s.pauseJob(context.Background(), job.ID, prompt("user paused"))
	if err != nil || paused.Enabled || paused.State != "paused" || deref(paused.PausedReason) != "user paused" || deref(paused.PausedAt) == "" {
		t.Fatalf("paused = %+v %v", paused, err)
	}
}

func TestPauseResumeJobPauseIsAuthoritativeDueJobsDoNotFire(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("Must not fire while paused"), Schedule: "every 1h"})
	past := FormatISO(s.clk.now().Add(-2 * time.Hour))
	if updated, err := s.updateJob(ctx, job.ID, map[string]any{"next_run_at": past}); err != nil || !updated.Enabled {
		t.Fatalf("update = %+v %v", updated, err)
	}
	if !dueIDs(t, s)[job.ID] {
		t.Fatal("past due job not due")
	}
	paused, _ := s.pauseJob(ctx, job.ID, prompt("outage freeze"))
	if paused.Enabled || paused.State != "paused" || deref(paused.PausedAt) == "" {
		t.Fatalf("paused = %+v", paused)
	}
	if dueIDs(t, s)[job.ID] {
		t.Fatal("paused job is due")
	}
	before, _ := s.getJob(ctx, job.ID)
	if before.LastRunAt != nil && !ptrEqual(before.LastRunAt, job.LastRunAt) {
		t.Fatalf("pause ran the job %v", deref(before.LastRunAt))
	}
	if _, won, _ := s.claimJobForFire(ctx, job.ID, FireClaimOptions{}); won {
		t.Fatal("paused job was claimed for fire")
	}
	after, _ := s.getJob(ctx, job.ID)
	if !ptrEqual(after.LastRunAt, before.LastRunAt) || after.Enabled {
		t.Fatalf("claim changed the paused job: %+v", after)
	}
}

func TestPauseResumeJobContradictoryHalfPauseSelfDisablesAndDoesNotFire(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := s.clk.now()
	writeJobsFile(t, s, map[string]any{"jobs": []any{map[string]any{
		"id": "half-paused-1", "name": "half-paused", "prompt": "should never run",
		"schedule":         map[string]any{"kind": "interval", "minutes": 5, "display": "every 5m"},
		"schedule_display": "every 5m", "repeat": map[string]any{"times": nil, "completed": 0},
		"enabled": true, "state": "paused", "paused_at": FormatISO(now.Add(-20 * time.Hour)),
		"paused_reason": "operator thought this was frozen", "next_run_at": FormatISO(now.Add(-time.Hour)),
		"last_run_at": nil, "created_at": FormatISO(now.Add(-24 * time.Hour)), "deliver": "local",
	}}})
	raw, _ := s.LoadJobs(ctx)
	if effectiveJobState(raw[0]) != "scheduled" {
		t.Fatal("enabled half pause must read scheduled")
	}
	listed, _ := s.listJobs(ctx, true)
	var half *Job
	for _, j := range listed {
		if j.ID == "half-paused-1" {
			half = j
		}
	}
	if half == nil || !half.Enabled || half.State == "paused" {
		t.Fatalf("listed = %+v", half)
	}
	if _, won, _ := s.claimJobForFire(ctx, "half-paused-1", FireClaimOptions{}); won {
		t.Fatal("half paused job claimed")
	}
	if dueIDs(t, s)["half-paused-1"] {
		t.Fatal("half paused job due")
	}
	healed, _ := s.getJob(ctx, "half-paused-1")
	if healed.Enabled || healed.State != "paused" || deref(healed.PausedAt) == "" {
		t.Fatalf("healed = %+v", healed)
	}
	if dueIDs(t, s)["half-paused-1"] {
		t.Fatal("healed job due")
	}
}

func TestResumeRejectsPastOneshot(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	s := newClockStore(t, &now, time.UTC)
	writeJobsFile(t, s, map[string]any{"jobs": []any{map[string]any{
		"id": "test-resume-past", "name": "test-resume-past", "prompt": "Past one-shot",
		"schedule": map[string]any{"kind": "once", "run_at": FormatISO(now.Add(-5 * time.Minute)), "display": "once"},
		"repeat":   map[string]any{"times": 1, "completed": 0}, "enabled": false, "state": "paused",
		"paused_at": FormatISO(now), "paused_reason": "test", "next_run_at": nil,
		"created_at": FormatISO(now.Add(-time.Hour)), "deliver": "local",
	}}})
	if _, err := s.resumeJob(context.Background(), "test-resume-past"); err == nil || !strings.Contains(err.Error(), "in the past") {
		t.Fatalf("resume of past one-shot = %v", err)
	}
}

func TestPauseResumeJobResumeKeepsSlotThatElapsedWhilePausedDue(t *testing.T) {
	now := time.Date(2026, 9, 16, 17, 0, 0, 0, time.UTC)
	s := newClockStore(t, &now, time.UTC)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("daily pipeline"), Schedule: "30 1 * * *", Deliver: prompt("local")})
	slot := FormatISO(time.Date(2026, 9, 16, 1, 30, 0, 0, time.UTC))
	stored, _ := s.LoadJobs(context.Background())
	stored[0].NextRunAt = &slot
	if err := s.SaveJobs(context.Background(), stored, nil, false); err != nil {
		t.Fatal(err)
	}
	s.pauseJob(context.Background(), job.ID, prompt("ops audit"))
	if dueIDs(t, s)[job.ID] {
		t.Fatal("paused job due")
	}
	if got, _ := s.getJob(context.Background(), job.ID); deref(got.NextRunAt) != slot {
		t.Fatalf("pause moved the slot to %v", deref(got.NextRunAt))
	}
	resumed, err := s.resumeJob(context.Background(), job.ID)
	if err != nil || deref(resumed.NextRunAt) != slot {
		t.Fatalf("resume = %v %v", deref(resumed.NextRunAt), err)
	}
	if !dueIDs(t, s)[job.ID] {
		t.Fatal("elapsed slot not due after resume")
	}
}

func TestPauseResumeJobResumeRecomputesFutureOrMissingSlotFromNow(t *testing.T) {
	now := time.Date(2026, 9, 16, 17, 0, 0, 0, time.UTC)
	s := newClockStore(t, &now, time.UTC)
	ahead := mustCreate(t, s, CreateJobOptions{Prompt: prompt("daily"), Schedule: "30 1 * * *", Deliver: prompt("local")})
	s.pauseJob(context.Background(), ahead.ID, nil)
	canary := mustCreate(t, s, CreateJobOptions{Prompt: prompt("canary"), Schedule: "0 9 * * *", Deliver: prompt("local"), Paused: true})
	if got, _ := s.getJob(context.Background(), canary.ID); got.NextRunAt != nil {
		t.Fatalf("paused create stored next_run_at %s", *got.NextRunAt)
	}
	for _, id := range []string{ahead.ID, canary.ID} {
		resumed, err := s.resumeJob(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		next, _ := ParseISO(deref(resumed.NextRunAt))
		if !next.Time.After(now) || dueIDs(t, s)[id] {
			t.Fatalf("resume %s = %v", id, deref(resumed.NextRunAt))
		}
	}
}

func TestResolveJobRefResolveByExactId(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("A"), Schedule: "1h", Name: prompt("alpha")})
	if got, err := s.resolveJobRef(context.Background(), job.ID); err != nil || got == nil || got.ID != job.ID {
		t.Fatalf("exact id did not resolve %v", err)
	}
}

func TestResolveJobRefMutationsRefuseAmbiguousName(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	mustCreate(t, s, CreateJobOptions{Prompt: prompt("A"), Schedule: "1h", Name: prompt("dup")})
	mustCreate(t, s, CreateJobOptions{Prompt: prompt("B"), Schedule: "1h", Name: prompt("dup")})
	var amb *AmbiguousJobReferenceError
	checks := map[string]func() error{
		"pause":   func() error { _, err := s.pauseJob(ctx, "dup", nil); return err },
		"resume":  func() error { _, err := s.resumeJob(ctx, "dup"); return err },
		"trigger": func() error { _, err := s.triggerJob(ctx, "dup", ""); return err },
		"remove":  func() error { _, err := s.removeJob(ctx, "dup"); return err },
	}
	for name, fn := range checks {
		if err := fn(); !errors.As(err, &amb) {
			t.Fatalf("%s on an ambiguous name = %v", name, err)
		}
	}
}

func TestExportedResolveJobRefUsesTheGivenOrAmbientHome(t *testing.T) {
	s, _ := newTestStore(t)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("A"), Schedule: "1h", Name: prompt("Alpha")})
	for _, h := range []string{s.home, ""} {
		if got, err := ResolveJobRef(context.Background(), h, "alpha"); err != nil || got == nil || got.ID != job.ID {
			t.Fatal(h, got, err)
		}
	}
	mustCreate(t, s, CreateJobOptions{Prompt: prompt("B"), Schedule: "1h", Name: prompt("Alpha")})
	var amb *AmbiguousJobReferenceError
	if _, err := ResolveJobRef(context.Background(), "", "alpha"); !errors.As(err, &amb) {
		t.Fatal(err)
	}
	if got, err := ResolveJobRef(context.Background(), t.TempDir(), job.ID); got != nil || err != nil {
		t.Fatal("another home resolved the job", got, err)
	}
}
