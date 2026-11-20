package state

import (
	"context"
	"math"
	"reflect"
	"sync"
	"testing"
)

const (
	crossIdleS      = 6 * 3600.0
	crossStalenessS = crossIdleS * 2
)

func crossSession(t *testing.T, db *SessionDB, id string, startedAt float64, messageAt *float64) {
	t.Helper()
	if err := db.CreateSession(NewSession{ID: id, Source: "tui"}); err != nil {
		t.Fatal(err)
	}
	if messageAt != nil {
		if _, err := db.AppendMessage(context.Background(), id, map[string]any{"role": "user", "content": "hello"}, AppendGuards{}); err != nil {
			t.Fatal(err)
		}
		profileExec(t, db, "UPDATE messages SET timestamp = ? WHERE session_id = ?", *messageAt, id)
	}
	profileExec(t, db, "UPDATE sessions SET started_at = ? WHERE id = ?", startedAt, id)
}

func crossHeartbeat(t *testing.T, db *SessionDB, id string, pid int, startedAt, last float64) {
	t.Helper()
	if err := db.RegisterBackendHeartbeat(context.Background(), BackendHeartbeat{BackendID: id, PID: pid, StartedAt: startedAt, LastHeartbeat: &last, Profile: "p", Host: "h"}); err != nil {
		t.Fatal(err)
	}
}

func crossSweep(t *testing.T, db *SessionDB, o SweepOrphanedOptions) []string {
	t.Helper()
	swept, err := db.SweepOrphanedSessions(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return swept
}

func crossField(t *testing.T, db *SessionDB, id, field string) any {
	t.Helper()
	row, err := db.GetSession(context.Background(), id)
	if err != nil || row == nil {
		t.Fatal(row, err)
	}
	v, _ := row.Get(field)
	return v
}

func TestOtherBackendsLiveHeartbeatSparesSession(t *testing.T) {
	db := usageDB(t)
	at := now()
	stale := at - 2*3600
	crossSession(t, db, "b-session", stale, &stale)
	crossHeartbeat(t, db, "backend-B", 4242, at-3600, at)
	if swept := crossSweep(t, db, NewSweepOrphanedOptions(crossIdleS)); len(swept) != 0 {
		t.Fatal(swept)
	}
	if crossField(t, db, "b-session", "ended_at") != nil || crossField(t, db, "b-session", "end_reason") != nil {
		t.Fatal("live backend row was reaped")
	}
}

func TestTrulyDeadBackendSessionsStillReaped(t *testing.T) {
	db := usageDB(t)
	stale := now() - 8*3600
	crossSession(t, db, "truly-dead", stale, &stale)
	if swept := crossSweep(t, db, NewSweepOrphanedOptions(crossIdleS)); !reflect.DeepEqual(swept, []string{"truly-dead"}) {
		t.Fatal(swept)
	}
	if got := crossField(t, db, "truly-dead", "end_reason"); got != "startup_orphan_reap" {
		t.Fatal(got)
	}
}

func TestStaleHeartbeatDoesNotProtectRow(t *testing.T) {
	db := usageDB(t)
	at := now()
	started := at - 8*3600
	crossSession(t, db, "long-dead", started, &started)
	crossHeartbeat(t, db, "backend-dead", 1111, at-24*3600, at-(crossStalenessS+600))
	if swept := crossSweep(t, db, NewSweepOrphanedOptions(crossIdleS)); !reflect.DeepEqual(swept, []string{"long-dead"}) {
		t.Fatal(swept)
	}
	if got := crossField(t, db, "long-dead", "end_reason"); got != "startup_orphan_reap" {
		t.Fatal(got)
	}
}

func TestMixedLiveAndDeadBackendsOnlyReapsDead(t *testing.T) {
	db := usageDB(t)
	at := now()
	aAt, bAt := at-4*3600, at-30*3600
	crossSession(t, db, "a-row", aAt, &aAt)
	crossSession(t, db, "b-row", bAt, &bAt)
	crossHeartbeat(t, db, "A", 100, at-5*3600, at)
	crossHeartbeat(t, db, "B", 200, at-24*3600, at-(crossStalenessS+600))
	o := NewSweepOrphanedOptions(crossIdleS)
	grace := 0.0
	o.HeartbeatOwnershipGraceSeconds = &grace
	if swept := crossSweep(t, db, o); !reflect.DeepEqual(swept, []string{"b-row"}) {
		t.Fatal(swept)
	}
	if crossField(t, db, "a-row", "ended_at") != nil || crossField(t, db, "b-row", "end_reason") != "startup_orphan_reap" {
		t.Fatal("ownership inference reaped the wrong row")
	}
}

func TestExcludeIDsStillWinsOverLiveHeartbeats(t *testing.T) {
	db := usageDB(t)
	stale := now() - 8*3600
	crossSession(t, db, "resumed", stale, &stale)
	o := NewSweepOrphanedOptions(crossIdleS)
	o.ExcludeIDs = []string{"resumed"}
	if swept := crossSweep(t, db, o); len(swept) != 0 {
		t.Fatal(swept)
	}
	if crossField(t, db, "resumed", "ended_at") != nil {
		t.Fatal("excluded row was reaped")
	}
}

func TestHeartbeatPredicateHandlesMessageLessRow(t *testing.T) {
	db := usageDB(t)
	stale := now() - 8*3600
	crossSession(t, db, "no-msg", stale, nil)
	crossHeartbeat(t, db, "B", 1, stale-60, now())
	if swept := crossSweep(t, db, NewSweepOrphanedOptions(crossIdleS)); len(swept) != 0 {
		t.Fatal(swept)
	}
	if crossField(t, db, "no-msg", "ended_at") != nil {
		t.Fatal("message less row was reaped under a live heartbeat")
	}
}

func crossHeartbeatIDs(t *testing.T, db *SessionDB) []string {
	t.Helper()
	rows, err := db.ListBackendHeartbeats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, r := range rows {
		id, _ := r.Get("backend_id")
		ids = append(ids, id.(string))
	}
	return ids
}

func TestRegisterThenRefreshOverwritesInPlace(t *testing.T) {
	db := usageDB(t)
	at := now()
	crossHeartbeat(t, db, "B", 1, at-60, at)
	crossHeartbeat(t, db, "B", 1, at-60, at+5)
	rows, err := db.ListBackendHeartbeats(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	id, _ := rows[0].Get("backend_id")
	last, _ := rows[0].Get("last_heartbeat")
	if id != "B" || math.Abs(last.(float64)-(at+5)) > 0.01 {
		t.Fatal(id, last)
	}
}

func TestClearBackendHeartbeatRemovesOnlySelf(t *testing.T) {
	db := usageDB(t)
	crossHeartbeat(t, db, "A", 1, now(), now())
	crossHeartbeat(t, db, "B", 2, now(), now())
	if _, err := db.ClearBackendHeartbeat(context.Background(), "A"); err != nil {
		t.Fatal(err)
	}
	if ids := crossHeartbeatIDs(t, db); !reflect.DeepEqual(ids, []string{"B"}) {
		t.Fatal(ids)
	}
}

func TestPruneStaleHeartbeatsDropsOnlyExpired(t *testing.T) {
	db := usageDB(t)
	at := now()
	crossHeartbeat(t, db, "alive", 1, at, at)
	crossHeartbeat(t, db, "dead", 2, at-24*3600, at-(crossStalenessS+600))
	pruned, err := db.PruneStaleHeartbeats(context.Background(), crossStalenessS)
	if err != nil || !reflect.DeepEqual(pruned, []string{"dead"}) {
		t.Fatal(pruned, err)
	}
	if ids := crossHeartbeatIDs(t, db); !reflect.DeepEqual(ids, []string{"alive"}) {
		t.Fatal(ids)
	}
}

func TestHeartbeatIsSweptAtomicallyWithEndSession(t *testing.T) {
	db := usageDB(t)
	stale := now() - 8*3600
	crossSession(t, db, "racy", stale, &stale)
	crossHeartbeat(t, db, "B", 1, now(), now())
	var wg sync.WaitGroup
	var swept []string
	var err error
	wg.Add(1)
	go func() {
		defer wg.Done()
		swept, err = db.SweepOrphanedSessions(context.Background(), NewSweepOrphanedOptions(crossIdleS))
	}()
	wg.Wait()
	if err == nil && len(swept) != 0 {
		t.Fatal(swept)
	}
	if crossField(t, db, "racy", "ended_at") != nil {
		t.Fatal("row reaped under a live heartbeat")
	}
}
