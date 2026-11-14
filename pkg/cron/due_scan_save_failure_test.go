package cron

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

var dueScanFixedNow = time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)

// Recurring job whose next run is the pinned instant, on the grid of its schedule
func gridDueJob(id string) map[string]any {
	return map[string]any{
		"id": id, "name": id, "prompt": "x", "schedule": map[string]any{"kind": "cron", "expr": "* * * * *", "display": "every minute"},
		"next_run_at": isoUTC(dueScanFixedNow), "last_run_at": nil, "enabled": true, "state": "active", "repeat": nil, "deliver": "local",
	}
}

// Enabled record carrying pause markers, the repair that makes the scan save on its way out
func halfPausedDueJob(id string) map[string]any {
	job := gridDueJob(id)
	job["paused_at"], job["paused_reason"] = isoUTC(dueScanFixedNow), "test"
	return job
}

func onceDueJob(id string) map[string]any {
	job := gridDueJob(id)
	job["schedule"] = map[string]any{"kind": "once", "run_at": isoUTC(dueScanFixedNow), "display": "once"}
	job["repeat"] = map[string]any{"times": 1, "completed": 0}
	return job
}

func jobFromRow(t *testing.T, row map[string]any) *Job {
	t.Helper()
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	job := &Job{}
	if err := json.Unmarshal(raw, job); err != nil {
		t.Fatal(err)
	}
	return job
}

type saveFailureClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *saveFailureClock) get() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *saveFailureClock) set(at time.Time) {
	c.mu.Lock()
	c.now = at
	c.mu.Unlock()
}

// Store on a pinned movable clock with a clean outage registry, its log captured
func saveFailureStore(t *testing.T) (*Store, *bytes.Buffer, *saveFailureClock, *float64, *error, *[]string) {
	t.Helper()
	_, mono, probe, events := freshStoreHealth(t)
	s, logs := newTestStore(t)
	clock := &saveFailureClock{now: dueScanFixedNow}
	s.clk.nowFn = clock.get
	s.clk.zoneFn = func() *time.Location { return time.UTC }
	storeHealthHooks.wall = func() float64 { return float64(clock.get().UnixNano()) / 1e9 }
	return s, logs, clock, mono, probe, events
}

func failStaging(t *testing.T, fail func() bool, writes *int) {
	t.Helper()
	prev := jobsPayloadStager
	t.Cleanup(func() { jobsPayloadStager = prev })
	jobsPayloadStager = func(s *Store, jobs []*Job) (string, error) {
		if writes != nil {
			*writes++
		}
		if fail() {
			return "", enospc
		}
		return prev(s, jobs)
	}
}

func warningLines(logs *bytes.Buffer) []string {
	var out []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") {
			out = append(out, line)
		}
	}
	return out
}

func TestDueJobsAreReturnedWhenTheStoreCannotBeSaved(t *testing.T) {
	s, logs, _, _, _, _ := saveFailureStore(t)
	seedJobs(t, s, gridDueJob("due-job"), halfPausedDueJob("half-paused"))
	failStaging(t, func() bool { return true }, nil)
	logs.Reset()
	for range 2 {
		if due := mustDue(t, s); !reflect.DeepEqual(jobIDs(due), []string{"due-job"}) {
			t.Fatalf("due %v", jobIDs(due))
		}
	}
	otherHome := filepath.Join(filepath.Dir(s.home), "other-profile")
	other := claimStoreAt(t, otherHome)
	other.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	storeHealthNoteUnwritable(other, enospc, "x", "scan")
	warnings := warningLines(logs)
	if len(warnings) != 2 || !strings.Contains(warnings[0], "due-scan repairs not persisted") || !strings.Contains(warnings[0], s.cronDir) ||
		!strings.Contains(warnings[1], otherHome) {
		t.Fatalf("warnings %q", warnings)
	}
}

func TestSkippedOneshotSurvivesRecoveryByAnySave(t *testing.T) {
	s, _, clock, _, _, _ := saveFailureStore(t)
	once := onceDueJob("once")
	seedJobs(t, s, once)
	storeHealthNoteUnwritable(s, enospc, "x", "claim", jobFromRow(t, once))
	clock.set(dueScanFixedNow.Add(10 * time.Minute))
	appendAndSave := func(row map[string]any) {
		t.Helper()
		jobs := storedJobs(t, s)
		if row != nil {
			jobs = append(jobs, jobFromRow(t, row))
		}
		if err := s.SaveJobs(context.Background(), jobs, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	other := gridDueJob("other")
	other["next_run_at"] = isoUTC(clock.get().Add(time.Hour))
	appendAndSave(other)
	if DegradedRecord(s.cronDir) != nil {
		t.Fatal("a landed save left the store degraded")
	}
	failing := true
	failStaging(t, func() bool { return failing }, nil)
	if due := mustDue(t, s); !reflect.DeepEqual(jobIDs(due), []string{"once"}) {
		t.Fatalf("during the second failure due %v", jobIDs(due))
	}
	failing = false
	appendAndSave(nil)
	if due := mustDue(t, s); !reflect.DeepEqual(jobIDs(due), []string{"once"}) {
		t.Fatalf("after the second recovery due %v", jobIDs(due))
	}
	if due := mustDue(t, s); len(due) != 0 {
		t.Fatalf("the live run claim did not hold the one shot, due %v", jobIDs(due))
	}
	ttl := time.Duration(s.oneshotRunClaimTTL(context.Background()) * float64(time.Second))
	clock.set(clock.get().Add(ttl + 60*time.Second))
	if due := mustDue(t, s); !reflect.DeepEqual(jobIDs(due), []string{"once"}) {
		t.Fatalf("after the claim expired due %v", jobIDs(due))
	}
	if _, err := s.clearRunClaim(context.Background(), "once"); err != nil {
		t.Fatal(err)
	}
	if due := mustDue(t, s); !reflect.DeepEqual(jobIDs(due), []string{"once"}) {
		t.Fatalf("after the failed dispatch due %v", jobIDs(due))
	}
	stale := onceDueJob("stale")
	appendAndSave(stale)
	for _, id := range jobIDs(mustDue(t, s)) {
		if id == "stale" {
			t.Fatal("a one shot first scanned after the recovery scan fired")
		}
	}
	late := onceDueJob("late")
	late["next_run_at"] = isoUTC(clock.get().Add(time.Minute))
	appendAndSave(late)
	clock.set(clock.get().Add(10 * time.Minute))
	for _, id := range jobIDs(mustDue(t, s)) {
		if id == "late" {
			t.Fatal("a one shot missed while the store was writable fired")
		}
	}
	for _, id := range jobIDs(storedJobs(t, s)) {
		if id == "late" {
			t.Fatal("the missed one shot was not retired")
		}
	}
}

func saveFailureTick(t *testing.T, s *Store) int {
	t.Helper()
	n, err := schedulerTick(context.Background(), s, tickOptions{Verbose: false, Sync: true})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestUnwritableStoreDegradesOnceThrottlesAndCatchesUpOnce(t *testing.T) {
	s, _, clock, mono, _, _ := saveFailureStore(t)
	seedJobs(t, s, gridDueJob("due-job"), halfPausedDueJob("half-paused"), onceDueJob("once"))
	resetRan()
	t.Cleanup(resetRan)
	type transition struct {
		event   string
		skipped int
	}
	var events []transition
	SetStoreTransitionListener(func(event string, record *StoreDegraded) {
		events = append(events, transition{event, record.SkippedRuns()})
	})
	outage, writes, probes := true, 0, 0
	failStaging(t, func() bool { return outage }, &writes)
	storeHealthHooks.probe = func(string) error {
		probes++
		if outage {
			return enospc
		}
		return nil
	}
	var perTick []int
	for minute, at := range []float64{0, 30, 60, 90, 120} {
		clock.set(dueScanFixedNow.Add(time.Duration(minute) * time.Minute))
		*mono = at
		before := writes
		if n := saveFailureTick(t, s); n != 0 {
			t.Fatalf("tick %d dispatched %d", minute, n)
		}
		perTick = append(perTick, writes-before)
	}
	if len(ranJobs()) != 0 || probes != 0 {
		t.Fatalf("ran %v probes %d", ranJobs(), probes)
	}
	if !reflect.DeepEqual(perTick, []int{3, 1, 1, 1, 1}) {
		t.Fatalf("write attempts per tick %v", perTick)
	}
	if !reflect.DeepEqual(events, []transition{{"unwritable", 0}}) {
		t.Fatalf("events %v", events)
	}
	if record := DegradedRecord(s.cronDir); record == nil || record.SkippedRuns() != 2 {
		t.Fatalf("record %+v", record)
	}
	if n := s.CatchUpOccurrenceCount(); n != 0 {
		t.Fatalf("catch up count %d before the fast forward persisted", n)
	}
	outage = false
	clock.set(dueScanFixedNow.Add(5 * time.Minute))
	*mono = 180
	if n := saveFailureTick(t, s); n != 2 {
		t.Fatalf("recovery tick dispatched %d", n)
	}
	ran := ranJobs()
	sort.Strings(ran)
	if !reflect.DeepEqual(ran, []string{"due-job", "once"}) || len(events) != 2 || events[1].event != "recovered" {
		t.Fatalf("ran %v events %v", ran, events)
	}
	if DegradedRecord(s.cronDir) != nil {
		t.Fatal("the store stayed degraded after a landed save")
	}
	if n := s.CatchUpOccurrenceCount(); n != 1 {
		t.Fatalf("catch up count %d", n)
	}
	rows, err := openExecutionsLedger(s).List(context.Background(), strPtr("once"), 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	failed := 0
	for _, row := range rows {
		if row["status"] == "failed" {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("the one shot keeps %d failed rows in %v", failed, rows)
	}
}

func TestIdleTickReprobesADegradedStoreAndClearsIt(t *testing.T) {
	s, _, _, mono, probe, _ := saveFailureStore(t)
	*mono = 0
	*probe = enospc
	if err := s.SaveJobs(context.Background(), []*Job{}, nil, false); err != nil {
		t.Fatal(err)
	}
	storeHealthNoteUnwritable(s, enospc, "x", "scan")
	if n := saveFailureTick(t, s); n != 0 {
		t.Fatalf("idle tick dispatched %d", n)
	}
	*probe = nil
	*mono += 30
	saveFailureTick(t, s)
	if DegradedRecord(s.cronDir) == nil {
		t.Fatal("the re-probe was not throttled to once a minute")
	}
	failing := true
	failStaging(t, func() bool { return failing }, nil)
	*mono += 60
	saveFailureTick(t, s)
	if DegradedRecord(s.cronDir) == nil {
		t.Fatal("a passing probe ended the outage while the real save still fails")
	}
	failing = false
	*mono += 60
	saveFailureTick(t, s)
	if DegradedRecord(s.cronDir) != nil {
		t.Fatal("the store stayed degraded after the save landed")
	}
}
