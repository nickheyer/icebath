package cron

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type slotFixture struct {
	s       *Store
	jobID   string
	slot    string
	counter string
}

// Store with a script job whose hourly slot fell due a minute ago, each fire appending one line
func newSlotFixture(t *testing.T) *slotFixture {
	t.Helper()
	s, _ := newTestStore(t)
	for _, dir := range []string{filepath.Join("cron", "output"), "scripts"} {
		if err := os.MkdirAll(filepath.Join(s.home, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", filepath.Dir(s.home))
	t.Setenv("ICEBATH_MACHINE_ID", "")
	os.Unsetenv("ICEBATH_MACHINE_ID")
	counter := filepath.Join(s.home, "fires.txt")
	script := filepath.Join(s.home, "scripts", "fire.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho fired >> "+counter+"\necho fired\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	useRealTickSeams(t)
	useRealDelivery(t)
	t.Cleanup(shutdownParallelPools)
	job := mustCreate(t, s, CreateJobOptions{Schedule: "every 1h", Name: strPtr("slot"), Script: "fire.sh", NoAgent: true, Deliver: strPtr("local")})
	slot := FormatISO(s.clk.now().Add(-time.Minute).Truncate(time.Second))
	editJobs(t, s, func(j *Job) {
		if j.ID == job.ID {
			j.NextRunAt = &slot
		}
	})
	return &slotFixture{s: s, jobID: job.ID, slot: slot, counter: counter}
}

func (f *slotFixture) fires(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile(f.counter)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(raw), "\n")
}

// A separate process whose tick advances the schedule and dies before any fire claim
func (f *slotFixture) crashBeforeDispatch(t *testing.T) {
	t.Helper()
	cmd := ledgerChildCommand(f.s.home, "", "crash-before-dispatch", "")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 137 {
		t.Fatalf("crash child ended with %v: %s", err, out)
	}
}

func (f *slotFixture) nextRunInFuture(t *testing.T, job *Job) bool {
	t.Helper()
	return inFuture(t, f.s, job.NextRunAt)
}

func forgetRunningJobs() {
	inflight.Lock()
	defer inflight.Unlock()
	clear(inflight.running)
	clear(inflight.since)
	clear(inflight.futures)
	clear(inflight.owners)
}

func TestMissedWindowCatchUpSlotLostBeforeDispatchFiresOnceAfterRestart(t *testing.T) {
	f := newSlotFixture(t)
	f.crashBeforeDispatch(t)
	if afterCrash := mustGet(t, f.s, f.jobID); !f.nextRunInFuture(t, afterCrash) {
		t.Fatalf("the crashed tick did not advance the schedule, next run %v", deref(afterCrash.NextRunAt))
	}
	if n := f.fires(t); n != 0 {
		t.Fatalf("fired %d times before the restart", n)
	}
	tickOnce(t, f.s)
	if n := f.fires(t); n != 1 {
		t.Fatalf("occurrence lost in the restart gap must fire once, fired %d", n)
	}
	row := latestExecution(t, f.s, f.jobID)
	if row == nil || row["status"] != "completed" {
		t.Fatalf("execution %v", row)
	}
	if want := scheduledInstant(&f.slot); want == nil || row["scheduled_instant"] != *want {
		t.Fatalf("restored slot lost its identity: %v want %v", row["scheduled_instant"], want)
	}
	tickOnce(t, f.s)
	if n := f.fires(t); n != 1 {
		t.Fatalf("fired %d times after a second tick", n)
	}
	record := mustGet(t, f.s, f.jobID)
	if record.PendingSlot != nil || !f.nextRunInFuture(t, record) {
		t.Fatalf("pending slot %+v next run %v", record.PendingSlot, deref(record.NextRunAt))
	}
}

func TestMissedWindowCatchUpFiredSlotIsNotReplayedAfterRestart(t *testing.T) {
	f := newSlotFixture(t)
	tickOnce(t, f.s)
	if n := f.fires(t); n != 1 {
		t.Fatalf("fired %d times", n)
	}
	forgetRunningJobs()
	tickOnce(t, f.s)
	if n := f.fires(t); n != 1 {
		t.Fatalf("a fired slot was replayed, fired %d", n)
	}
	if record := mustGet(t, f.s, f.jobID); record.PendingSlot != nil {
		t.Fatalf("pending slot %+v", record.PendingSlot)
	}
}
