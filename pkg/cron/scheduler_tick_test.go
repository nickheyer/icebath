package cron

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func makeTickDue(t *testing.T, s *Store, id string) string {
	t.Helper()
	due := FormatISO(s.clk.now().Add(-30 * time.Second))
	err := s.withJobsLock(context.Background(), func(tx *jobsTx) error {
		jobs, err := s.loadJobs(context.Background(), tx)
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if j.ID == id {
				j.NextRunAt = &due
			}
		}
		return s.saveJobsLocked(context.Background(), tx, jobs, nil, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	return due
}

func stubGateway(t *testing.T, state, reason string) *int {
	probes := 0
	discoverGatewayEndpoint = func(context.Context, string, time.Duration) gateway.ObservedEndpoint {
		probes++
		return gateway.ObservedEndpoint{State: state, ReasonCode: reason}
	}
	t.Cleanup(func() { discoverGatewayEndpoint = gateway.DiscoverGatewayEndpoint })
	return &probes
}

func tickStoredJob(t *testing.T, s *Store, id string) *Job {
	jobs, err := s.LoadJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.ID == id {
			return j
		}
	}
	t.Fatalf("job %s missing", id)
	return nil
}

func TestTickWithoutGatewaySkipsAgentJobsWithoutDrift(t *testing.T) {
	s, buf := newTestStore(t)
	stubGateway(t, "absent", "")
	resetRan()
	agent := mustCreate(t, s, CreateJobOptions{Prompt: prompt("summarise inbox"), Schedule: "every 1h", Name: prompt("agent"), Deliver: prompt("local")})
	other := mustCreate(t, s, CreateJobOptions{Prompt: prompt("digest"), Schedule: "every 1h", Name: prompt("agent-2"), Deliver: prompt("local")})
	dueA, dueB := makeTickDue(t, s, agent.ID), makeTickDue(t, s, other.ID)
	ran, err := schedulerTick(t.Context(), s, tickOptions{Headless: true, Sync: true})
	if err != nil || ran != 0 || len(ranJobs()) != 0 {
		t.Fatal(ran, err, ranJobs())
	}
	for id, due := range map[string]string{agent.ID: dueA, other.ID: dueB} {
		j := tickStoredJob(t, s, id)
		if j.LastRunAt != nil || j.NextRunAt == nil || *j.NextRunAt != due || j.PendingSlot != nil {
			t.Fatal(id, j.LastRunAt, j.NextRunAt, j.PendingSlot)
		}
		if j.LastFireError == nil || !strings.Contains(j.LastFireError.Detail, "icebath gateway start") {
			t.Fatal(j.LastFireError)
		}
	}
	if n := strings.Count(buf.String(), "not running"); n != 1 || !strings.Contains(buf.String(), agent.ID) {
		t.Fatal(buf.String())
	}
	testTick.Lock()
	reconciled := append([]bool{}, testTick.reconciled...)
	testTick.reconciled = nil
	testTick.Unlock()
	if len(reconciled) == 0 || reconciled[len(reconciled)-1] {
		t.Fatal("a headless tick reconciled with connects allowed", reconciled)
	}
}

func TestTickHoldsAgentJobsWhileGatewayIsStarting(t *testing.T) {
	s, _ := newTestStore(t)
	stubGateway(t, "starting", "runtime_lock")
	resetRan()
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 1h", Deliver: prompt("local")})
	due := makeTickDue(t, s, job.ID)
	if ran, err := schedulerTick(t.Context(), s, tickOptions{Headless: true, Sync: true}); err != nil || ran != 0 {
		t.Fatal(ran, err)
	}
	j := tickStoredJob(t, s, job.ID)
	if *j.NextRunAt != due || !strings.Contains(j.LastFireError.Detail, "the gateway is starting (runtime_lock)") {
		t.Fatal(j.NextRunAt, j.LastFireError)
	}
}

func TestReadyGatewayAndRemoteURLDispatchAgentJobs(t *testing.T) {
	s, _ := newTestStore(t)
	probes := stubGateway(t, "ready", "")
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 1h"})
	kept, err := refuseAgentJobsWithoutGateway(t.Context(), s, []*Job{job})
	if err != nil || len(kept) != 1 || *probes != 1 {
		t.Fatal(kept, err, *probes)
	}
	t.Setenv("ICEBATH_TUI_GATEWAY_URL", "ws://remote")
	if kept, _ = refuseAgentJobsWithoutGateway(t.Context(), s, []*Job{job}); len(kept) != 1 || *probes != 1 {
		t.Fatal("an explicit remote gateway was probed")
	}
}

func TestNoAgentJobsStillFireWithoutAGateway(t *testing.T) {
	s, _ := newTestStore(t)
	probes := stubGateway(t, "absent", "")
	resetRan()
	job := mustCreate(t, s, CreateJobOptions{Schedule: "every 1h", Name: prompt("script"), Script: "fire.sh", NoAgent: true, Deliver: prompt("local")})
	makeTickDue(t, s, job.ID)
	ran, err := schedulerTick(t.Context(), s, tickOptions{Headless: true, Sync: true})
	if err != nil || ran != 1 || len(ranJobs()) != 1 || ranJobs()[0] != job.ID || *probes != 0 {
		t.Fatal(ran, err, ranJobs(), *probes)
	}
	rec, err := openExecutionsLedger(s).Latest(t.Context(), job.ID)
	if err != nil || rec == nil || recText(rec, "source") != "builtin" {
		t.Fatal(rec, err)
	}
}

func TestTickSkipsWhenAnotherTickerHoldsTheLock(t *testing.T) {
	s, buf := newTestStore(t)
	_, lockFile := s.getLockPaths()
	holder, err := os.OpenFile(lockFile, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := tryFlockOnce(holder); err != nil {
		t.Fatal(err)
	}
	defer releaseFlock(holder)
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("x"), Schedule: "every 1h"})
	makeTickDue(t, s, job.ID)
	resetRan()
	if ran, err := schedulerTick(t.Context(), s, tickOptions{Sync: true}); err != nil || ran != 0 || len(ranJobs()) != 0 {
		t.Fatal(ran, err)
	}
	if !strings.Contains(buf.String(), "another instance holds the lock") {
		t.Fatal(buf.String())
	}
}

func TestTickRunsDueJobsAndCountsResults(t *testing.T) {
	s, _ := newTestStore(t)
	a := mustCreate(t, s, CreateJobOptions{Prompt: prompt("a"), Schedule: "every 1h"})
	b := mustCreate(t, s, CreateJobOptions{Prompt: prompt("b"), Schedule: "every 1h"})
	makeTickDue(t, s, a.ID)
	makeTickDue(t, s, b.ID)
	resetRan()
	testRun.runOne = func(_ context.Context, _ *Store, job *Job, o runOneJobOptions) (bool, error) {
		if job.ExecutionID == nil || *job.ExecutionID == "" {
			t.Error("claim snapshot lost its execution id")
		}
		return job.ID == a.ID, nil
	}
	t.Cleanup(func() { testRun.runOne = nil })
	t.Setenv("ICEBATH_CRON_MAX_PARALLEL", "1")
	ran, err := schedulerTick(t.Context(), s, tickOptions{Sync: true})
	got := ranJobs()
	sort.Strings(got)
	want := []string{a.ID, b.ID}
	sort.Strings(want)
	if err != nil || ran != 1 || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatal(ran, err, got)
	}
	if tickStoredJob(t, s, a.ID).NextRunAt == nil {
		t.Fatal("recurring advance missing")
	}
	if n := s.resolveMaxParallelWorkers(t.Context()); n == nil || *n != 1 {
		t.Fatal(n)
	}
	t.Setenv("ICEBATH_CRON_MAX_PARALLEL", "0")
	if n := s.resolveMaxParallelWorkers(t.Context()); n != nil {
		t.Fatal(*n)
	}
}

func TestTickYieldLogsOncePerEpisode(t *testing.T) {
	s, buf := newTestStore(t)
	lastYieldLog.Lock()
	lastYieldLog.set = false
	lastYieldLog.Unlock()
	s.logTickYieldOnce("boot=a disk=b")
	s.logTickYieldOnce("boot=a disk=b")
	s.logTickYieldOnce("boot=a disk=c")
	if n := strings.Count(buf.String(), "Cron tick yielded"); n != 2 {
		t.Fatal(n, buf.String())
	}
}

func TestProfilesSharingARepoPruneItOncePerInterval(t *testing.T) {
	s, _ := newTestStore(t)
	base := t.TempDir()
	shared, private := filepath.Join(base, "shared"), filepath.Join(base, "private")
	for _, r := range []string{shared, private} {
		if err := os.MkdirAll(filepath.Join(r, ".worktrees"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	lastWorktreePrune.Lock()
	lastWorktreePrune.at = map[string]float64{}
	lastWorktreePrune.Unlock()
	testTick.Lock()
	testTick.pruned = nil
	testTick.Unlock()
	discovered := map[string][]string{"default": {shared}, "ops": {shared}, "lab": {shared, private}}
	for _, name := range []string{"default", "ops", "lab"} {
		repos := discovered[name]
		s.runWorktreeSweep(t.Context(), func(context.Context) []string { return repos }, worktreeMaintenanceIntervalSec, nil)
	}
	testTick.Lock()
	pruned := append([]string{}, testTick.pruned...)
	testTick.Unlock()
	sort.Strings(pruned)
	if strings.Join(pruned, ",") != strings.Join([]string{private, shared}, ",") {
		t.Fatal(pruned)
	}
}

func TestASweepWaitsInsteadOfRunningBesideTheRunningOne(t *testing.T) {
	s, buf := newTestStore(t)
	if !claimSweepSlot(nil) {
		t.Fatal("slot busy")
	}
	zero := 0.0
	called := false
	s.runWorktreeSweep(t.Context(), func(context.Context) []string { called = true; return nil }, 1, &zero)
	if called || !strings.Contains(buf.String(), "another sweep is still running") {
		t.Fatal(called, buf.String())
	}
	done := make(chan struct{})
	wait := 5.0
	go func() {
		s.runWorktreeSweep(t.Context(), func(context.Context) []string { return nil }, 1, &wait)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	releaseSweepSlot()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting sweep never ran")
	}
}
