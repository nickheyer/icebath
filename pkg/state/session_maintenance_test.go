package state

import (
	"context"
	"reflect"
	"testing"
	"time"
)

const sweepIdleS = 6 * 3600

func sweepMake(t *testing.T, db *SessionDB, sid, source string, startedAt float64, messageAt *float64) {
	t.Helper()
	readSession(t, db, NewSession{ID: sid, Source: source})
	if messageAt != nil {
		readAppend(t, db, sid, map[string]any{"role": "user", "content": "hello"})
		profileExec(t, db, "UPDATE messages SET timestamp = ? WHERE session_id = ?", *messageAt, sid)
	}
	profileExec(t, db, "UPDATE sessions SET started_at = ? WHERE id = ?", startedAt, sid)
}

func sweepRun(t *testing.T, db *SessionDB, mutate func(*SweepOrphanedOptions)) []string {
	t.Helper()
	o := NewSweepOrphanedOptions(sweepIdleS)
	if mutate != nil {
		mutate(&o)
	}
	got, err := db.SweepOrphanedSessions(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func sweepWant(t *testing.T, got []string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("swept %v want %v", got, want)
	}
}

func sweepCLI(o *SweepOrphanedOptions) { o.Sources = []string{"cli"} }

func ago(hours float64) float64 { return float64(time.Now().UnixNano())/1e9 - hours*3600 }

func TestSweepOrphanedSessionsStaleSourcesSwept(t *testing.T) {
	for _, source := range []string{"tui", "desktop", "subagent"} {
		db := leaseDB(t)
		stale := ago(8)
		sweepMake(t, db, "stale-"+source, source, stale, &stale)
		sweepWant(t, sweepRun(t, db, nil), "stale-"+source)
		row := guardSession(t, db, "stale-"+source)
		if row["ended_at"] == nil || row["end_reason"] != "startup_orphan_reap" {
			t.Fatalf("row %v", row)
		}
	}
}

func TestSweepOrphanedSessionsSparesRecentActivity(t *testing.T) {
	db := leaseDB(t)
	stale, fresh, old := ago(48), ago(0), ago(8)
	sweepMake(t, db, "active", "tui", stale, &fresh)
	sweepMake(t, db, "active-heartbeat", "tui", stale, &stale)
	profileExec(t, db, "UPDATE sessions SET last_activity_at = ? WHERE id = ?", ago(0), "active-heartbeat")
	sweepMake(t, db, "fresh-child", "tui", ago(0), &old)
	sweepMake(t, db, "tg-sess", "telegram", old, &old)
	sweepWant(t, sweepRun(t, db, nil))
	for _, sid := range []string{"active", "active-heartbeat", "fresh-child", "tg-sess"} {
		if row := guardSession(t, db, sid); row["ended_at"] != nil {
			t.Fatalf("%s swept %v", sid, row)
		}
	}
}

func TestSweepOrphanedSessionsAlreadyEndedUntouched(t *testing.T) {
	db := leaseDB(t)
	stale := ago(8)
	sweepMake(t, db, "done", "tui", stale, &stale)
	if err := db.EndSession("done", "user_exit"); err != nil {
		t.Fatal(err)
	}
	sweepWant(t, sweepRun(t, db, nil))
	if row := guardSession(t, db, "done"); row["end_reason"] != "user_exit" {
		t.Fatalf("row %v", row)
	}
}

func TestSweepOrphanedSessionsStaleEmptySweptFreshSpared(t *testing.T) {
	db := leaseDB(t)
	sweepMake(t, db, "stale-empty", "tui", ago(8), nil)
	sweepMake(t, db, "fresh-empty", "tui", ago(0), nil)
	sweepWant(t, sweepRun(t, db, nil), "stale-empty")
	if guardSession(t, db, "stale-empty")["end_reason"] != "startup_orphan_reap" || guardSession(t, db, "fresh-empty")["ended_at"] != nil {
		t.Fatal("empty sweep scope")
	}
}

func TestSweepOrphanedSessionsExcludeIDsSparesLiveRow(t *testing.T) {
	db := leaseDB(t)
	stale := ago(8)
	sweepMake(t, db, "live-tui", "tui", stale, &stale)
	sweepMake(t, db, "dead-tui", "tui", stale, &stale)
	sweepWant(t, sweepRun(t, db, func(o *SweepOrphanedOptions) { o.ExcludeIDs = []string{"live-tui"} }), "dead-tui")
	if guardSession(t, db, "live-tui")["ended_at"] != nil || guardSession(t, db, "dead-tui")["end_reason"] != "startup_orphan_reap" {
		t.Fatal("exclude ids")
	}
}

func TestSweepOrphanedSessionsCustomSourcesRespected(t *testing.T) {
	db := leaseDB(t)
	stale := ago(8)
	sweepMake(t, db, "stale-cli", "cli", stale, &stale)
	sweepMake(t, db, "stale-tui", "tui", stale, &stale)
	sweepWant(t, sweepRun(t, db, sweepCLI), "stale-cli")
	if guardSession(t, db, "stale-cli")["end_reason"] != "startup_orphan_reap" || guardSession(t, db, "stale-tui")["ended_at"] != nil {
		t.Fatal("custom sources")
	}
}

func TestSweepOrphanedSessionsExplicitSourceScopeSparesGatewaySessions(t *testing.T) {
	db := leaseDB(t)
	stale := ago(8)
	sweepMake(t, db, "stale-cron", "cron", stale, &stale)
	for sid, key := range map[string]string{"keyed-telegram": "telegram:chat:1", "unkeyed-telegram": ""} {
		readSession(t, db, NewSession{ID: sid, Source: "telegram", SessionKey: key})
		readAppend(t, db, sid, map[string]any{"role": "user", "content": "hello"})
		profileExec(t, db, "UPDATE messages SET timestamp = ? WHERE session_id = ?", stale, sid)
		profileExec(t, db, "UPDATE sessions SET started_at = ? WHERE id = ?", stale, sid)
	}
	sweepWant(t, sweepRun(t, db, func(o *SweepOrphanedOptions) { o.Sources = []string{"cron"} }), "stale-cron")
	if guardSession(t, db, "keyed-telegram")["ended_at"] != nil || guardSession(t, db, "unkeyed-telegram")["ended_at"] != nil {
		t.Fatal("gateway sessions swept")
	}
}

func TestSweepOrphanedSessionsAutomaticSourceScopeSparesPinnedSession(t *testing.T) {
	db := leaseDB(t)
	stale := ago(8)
	sweepMake(t, db, "pinned", "cli", stale, &stale)
	if err := db.SetPinned("pinned", true); err != nil {
		t.Fatal(err)
	}
	sweepWant(t, sweepRun(t, db, func(o *SweepOrphanedOptions) { sweepCLI(o); o.ExcludePinned = true }))
	if guardSession(t, db, "pinned")["ended_at"] != nil {
		t.Fatal("pinned swept")
	}
}

func TestSweepOrphanedSessionsLiveTurnLeaseOnCompressionLineageSparesSession(t *testing.T) {
	db := leaseDB(t)
	stale := ago(8)
	sweepMake(t, db, "root", "cli", stale, &stale)
	leaseEnd(t, db, "root")
	readSession(t, db, NewSession{ID: "tip", Source: "cli", ParentSessionID: "root"})
	readAppend(t, db, "tip", map[string]any{"role": "user", "content": "continued"})
	profileExec(t, db, "UPDATE messages SET timestamp = ? WHERE session_id = ?", stale, "tip")
	profileExec(t, db, "UPDATE sessions SET started_at = ? WHERE id = ?", stale, "tip")
	if !leaseTry(t, db, "tip", "external-turn", 300) {
		t.Fatal("lease")
	}
	sweepWant(t, sweepRun(t, db, sweepCLI))
	if guardSession(t, db, "tip")["ended_at"] != nil {
		t.Fatal("leased tip swept")
	}
}

func TestSweepOrphanedSessionsActiveCompressionLockSparesAndExpiryFencesOwner(t *testing.T) {
	db := leaseDB(t)
	stale := ago(8)
	sweepMake(t, db, "compressing", "cli", stale, &stale)
	if !db.TryAcquireCompressionLock("compressing", "compressor", 300) {
		t.Fatal("lock")
	}
	sweepWant(t, sweepRun(t, db, sweepCLI))
	profileExec(t, db, "UPDATE compression_locks SET expires_at = ? WHERE session_id = ?", ago(0)-1, "compressing")
	sweepWant(t, sweepRun(t, db, sweepCLI), "compressing")
	if holder, err := db.GetCompressionLockHolder("compressing"); err != nil || holder != nil {
		t.Fatalf("holder %v, %v", holder, err)
	}
	if db.RefreshCompressionLock("compressing", "compressor", 300) {
		t.Fatal("fenced owner refreshed")
	}
}

func TestSweepOrphanedSessionsExpiredTurnLeaseDoesNotBlockSweep(t *testing.T) {
	db := leaseDB(t)
	stale := ago(8)
	sweepMake(t, db, "expired", "cli", stale, &stale)
	if !leaseTry(t, db, "expired", "expired-turn", 300) {
		t.Fatal("lease")
	}
	profileExec(t, db, "UPDATE session_turn_leases SET expires_at = ? WHERE conversation_id = ?", ago(0)-1, "expired")
	sweepWant(t, sweepRun(t, db, sweepCLI), "expired")
	if guardSession(t, db, "expired")["end_reason"] != "startup_orphan_reap" {
		t.Fatal("expired lease row")
	}
	if ok, err := db.RefreshSessionTurnLease("expired", "expired-turn", 300, nil); err != nil || ok {
		t.Fatalf("fenced turn refreshed %v, %v", ok, err)
	}
}

func TestSweepOrphanedSessionsReturnsEmptyOnEmptyDBAndZeroTTL(t *testing.T) {
	db := leaseDB(t)
	sweepWant(t, sweepRun(t, db, nil))
	stale := ago(8)
	sweepMake(t, db, "stale", "tui", stale, &stale)
	sweepWant(t, sweepRun(t, db, func(o *SweepOrphanedOptions) { o.MaxIdleSeconds = 0 }))
	if guardSession(t, db, "stale")["ended_at"] != nil {
		t.Fatal("zero ttl swept")
	}
}
