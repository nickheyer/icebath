package gateway

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Store at the path, failing the test when it cannot open
func openRunIdempotencyStore(t *testing.T, path string) *RunIdempotencyStore {
	t.Helper()
	store, err := NewRunIdempotencyStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// Outcome of a reserve with no owner and no retention horizon
func reserveRun(t *testing.T, store *RunIdempotencyStore, scope, key, fingerprint, runID string, status *pyjson.Object, retention float64) (string, *RunIdempotencyRecord) {
	t.Helper()
	outcome, record, err := store.Reserve(context.Background(), scope, key, fingerprint, runID, status, 0, 0, retention)
	if err != nil {
		t.Fatal(err)
	}
	return outcome, record
}

// Pins the store clock for one test
func pinAPIClock(t *testing.T, now *float64) {
	prev := apiNow
	apiNow = func() float64 { return *now }
	t.Cleanup(func() { apiNow = prev })
}

func TestRestartDurabilityAndTerminalSemantics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idem.db")
	for _, terminal := range []string{"completed", "failed", "cancelled"} {
		first := openRunIdempotencyStore(t, path)
		runID := "run_" + terminal
		if outcome, _ := reserveRun(t, first, "tenant", terminal, "fp", runID, pyjson.ObjectOf("run_id", runID, "status", terminal), 0); outcome != "created" {
			t.Fatalf("%s: %s", terminal, outcome)
		}
		first.Close()
		restarted := openRunIdempotencyStore(t, path)
		outcome, record := reserveRun(t, restarted, "tenant", terminal, "fp", "run_new", pyjson.ObjectOf("status", "queued"), 0)
		if outcome != "reused" || *record.RunID != runID || record.Status.(*pyjson.Object).GetOr("status", nil) != terminal {
			t.Fatalf("%s: %s %+v", terminal, outcome, record)
		}
		restarted.Close()
	}
}

func TestTenantIsolationAndRetention(t *testing.T) {
	store := openRunIdempotencyStore(t, filepath.Join(t.TempDir(), "idem.db"))
	defer store.Close()
	if outcome, _ := reserveRun(t, store, "tenant-a", "key", "fp-a", "run_a", pyjson.ObjectOf("status", "queued"), 0); outcome != "created" {
		t.Fatal(outcome)
	}
	if outcome, _ := reserveRun(t, store, "tenant-b", "key", "fp-b", "run_b", pyjson.ObjectOf("status", "queued"), 0); outcome != "created" {
		t.Fatal(outcome)
	}
}

func TestRetentionNeverReleasesAnActiveIdempotencyReservation(t *testing.T) {
	store := openRunIdempotencyStore(t, filepath.Join(t.TempDir(), "idem.db"))
	defer store.Close()
	now := 100.0
	pinAPIClock(t, &now)
	if outcome, _ := reserveRun(t, store, "tenant", "active-key", "active-fingerprint", "run-active", pyjson.ObjectOf("status", "running"), 0); outcome != "created" {
		t.Fatal(outcome)
	}
	if outcome, _ := reserveRun(t, store, "tenant", "done-key", "done-fingerprint", "run-done", pyjson.ObjectOf("status", "completed"), 0); outcome != "created" {
		t.Fatal(outcome)
	}
	now = 100 + RunIdempotencyRetentionSeconds + 1
	active, activeRecord, err := store.Lookup(context.Background(), "tenant", "active-key", "active-fingerprint", 0)
	if err != nil {
		t.Fatal(err)
	}
	done, doneRecord, err := store.Lookup(context.Background(), "tenant", "done-key", "done-fingerprint", 0)
	if err != nil {
		t.Fatal(err)
	}
	if active != "reused" || *activeRecord.RunID != "run-active" || done != "missing" || doneRecord != nil {
		t.Fatalf("%s %+v %s %+v", active, activeRecord, done, doneRecord)
	}
}

func TestRoomTerminalReceiptSurvivesOfflineHomeUntilGrantHorizon(t *testing.T) {
	now := 100.0
	pinAPIClock(t, &now)
	store := openRunIdempotencyStore(t, filepath.Join(t.TempDir(), "idem.db"))
	defer store.Close()
	horizon := now + 30*24*60*60
	if outcome, _ := reserveRun(t, store, "room-scope", "room:task-1:1", "room-fingerprint", "run-room",
		pyjson.ObjectOf("run_id", "run-room", "status", "completed"), horizon); outcome != "created" {
		t.Fatal(outcome)
	}
	now += RunIdempotencyRetentionSeconds + 1
	reserveRun(t, store, "other-scope", "other-key", "other-fingerprint", "run-other", pyjson.ObjectOf("run_id", "run-other", "status", "queued"), 0)
	outcome, record, err := store.Lookup(context.Background(), "room-scope", "room:task-1:1", "room-fingerprint", 0)
	if err != nil || outcome != "reused" || *record.RunID != "run-room" {
		t.Fatalf("%s %+v %v", outcome, record, err)
	}
	now = horizon + 1
	reserveRun(t, store, "third-scope", "third-key", "third-fingerprint", "run-third", pyjson.ObjectOf("run_id", "run-third", "status", "queued"), 0)
	outcome, record, err = store.Lookup(context.Background(), "room-scope", "room:task-1:1", "room-fingerprint", 0)
	if err != nil || outcome != "missing" || record != nil {
		t.Fatalf("%s %+v %v", outcome, record, err)
	}
}
