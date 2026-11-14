package state

import (
	"context"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func autoDB(t *testing.T) *SessionDB {
	t.Helper()
	installRetirementHooks(t)
	return leaseDB(t)
}

func autoPrune(t *testing.T, db *SessionDB, minIntervalHours float64) AutoPruneResult {
	t.Helper()
	o := NewAutoPruneOptions()
	retention := 90.0
	o.RetentionDays, o.MinIntervalHours, o.Vacuum = &retention, minIntervalHours, false
	return db.MaybeAutoPruneAndVacuum(context.Background(), o)
}

func autoSession(t *testing.T, db *SessionDB, sid string) *pyjson.Object {
	t.Helper()
	row, err := db.GetSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func autoStale(t *testing.T, db *SessionDB, sid, source string, at float64) {
	t.Helper()
	sweepMake(t, db, sid, source, at, &at)
	profileExec(t, db, "UPDATE sessions SET last_activity_at = ? WHERE id = ?", at, sid)
}

func autoField(t *testing.T, db *SessionDB, sid, field string) any {
	t.Helper()
	row := autoSession(t, db, sid)
	if row == nil {
		t.Fatalf("session %s missing", sid)
	}
	return rowValue(row, field)
}

func autoLease(t *testing.T, db *SessionDB, sid, holder string) {
	t.Helper()
	if ok, err := db.TryAcquireSessionTurnLease(sid, holder, 300, nil); err != nil || !ok {
		t.Fatalf("lease %s %v %v", sid, ok, err)
	}
}

func TestAutoPruneClosesStaleStateOwnedRowsButSparesLiveTurns(t *testing.T) {
	ctx := context.Background()
	db := autoDB(t)
	stale, recent := ago(100*24), ago(24)
	for _, s := range [][2]string{{"orphan", "cli"}, {"live-turn", "cli"}, {"stale-cron", "cron"}, {"runtime-owned-ui", "tui"}} {
		autoStale(t, db, s[0], s[1], stale)
	}
	autoStale(t, db, "recent-orphan", "cli", recent)
	readSession(t, db, NewSession{ID: "keyed", Source: "telegram", SessionKey: "telegram:chat:1"})
	profileExec(t, db, "UPDATE sessions SET started_at = ? WHERE id = ?", stale, "keyed")
	readSession(t, db, NewSession{ID: "unkeyed-gateway", Source: "telegram"})
	profileExec(t, db, "UPDATE sessions SET started_at = ?, last_activity_at = ? WHERE id = ?", stale, stale, "unkeyed-gateway")
	autoLease(t, db, "live-turn", "external-turn")
	beat := now()
	if err := db.RegisterBackendHeartbeat(ctx, BackendHeartbeat{BackendID: "unrelated-dashboard", PID: 12345, StartedAt: beat, LastHeartbeat: &beat}); err != nil {
		t.Fatal(err)
	}
	if first := autoPrune(t, db, 0); first.Pruned != 0 || first.Error != nil {
		t.Fatalf("first %+v", first)
	}
	for _, sid := range []string{"orphan", "stale-cron"} {
		if got := autoField(t, db, sid, "end_reason"); got != "startup_orphan_reap" {
			t.Fatalf("%s end reason %v", sid, got)
		}
	}
	for _, sid := range []string{"live-turn", "recent-orphan", "runtime-owned-ui", "keyed", "unkeyed-gateway"} {
		if got := autoField(t, db, sid, "ended_at"); got != nil {
			t.Fatalf("%s ended %v", sid, got)
		}
	}
	if second := autoPrune(t, db, 0); second.Pruned != 0 || autoSession(t, db, "orphan") == nil || autoSession(t, db, "stale-cron") == nil {
		t.Fatalf("second %+v", second)
	}
	profileExec(t, db, "UPDATE sessions SET ended_at = ? WHERE id IN (?, ?)", stale, "orphan", "stale-cron")
	if third := autoPrune(t, db, 0); third.Pruned != 2 || autoSession(t, db, "orphan") != nil || autoSession(t, db, "stale-cron") != nil {
		t.Fatalf("third %+v", third)
	}
}

func TestFailedMaintenanceMarkerKeepsNewlySweptRowRecoverable(t *testing.T) {
	db := autoDB(t)
	autoStale(t, db, "recoverable", "cli", ago(100*24))
	profileExec(t, db, "CREATE TRIGGER fail_marker BEFORE INSERT ON state_meta WHEN NEW.key = 'last_auto_prune' "+
		"BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END")
	first := autoPrune(t, db, 0)
	profileExec(t, db, "DROP TRIGGER fail_marker")
	retry := autoPrune(t, db, 0)
	if first.Error == nil || !strings.Contains(*first.Error, "injected marker failure") {
		t.Fatalf("first %+v", first)
	}
	if retry.Pruned != 0 || retry.Error != nil {
		t.Fatalf("retry %+v", retry)
	}
	if got := autoField(t, db, "recoverable", "end_reason"); got != "startup_orphan_reap" {
		t.Fatalf("end reason %v", got)
	}
}

func TestConcurrentAutoMaintenancePreservesTheRecoveryWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	t.Setenv("ICEBATH_HOME", filepath.Join(filepath.Dir(path), "home"))
	installRetirementHooks(t)
	db, peer := usageOpen(t, path), usageOpen(t, path)
	autoStale(t, db, "concurrent", "cli", ago(100*24))
	oneDone := make(chan struct{})
	var holdOnce, doneOnce sync.Once
	saved := reportStartupProgress
	reportStartupProgress = func(expectedS float64, phase string) {
		if phase == "state_db_auto_prune" {
			holdOnce.Do(func() {
				select {
				case <-oneDone:
				case <-time.After(5 * time.Second):
				}
			})
		}
	}
	t.Cleanup(func() { reportStartupProgress = saved })
	var wg sync.WaitGroup
	results := make([]AutoPruneResult, 2)
	for i, inst := range []*SessionDB{db, peer} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer doneOnce.Do(func() { close(oneDone) })
			results[i] = autoPrune(t, inst, 24)
		}()
	}
	wg.Wait()
	skipped, pruned := 0, 0
	for _, r := range results {
		if r.Error != nil {
			t.Fatalf("maintenance error %s", *r.Error)
		}
		if r.Skipped {
			skipped++
		}
		pruned += r.Pruned
	}
	if skipped != 1 || pruned != 0 {
		t.Fatalf("results %+v", results)
	}
	if got := autoField(t, db, "concurrent", "end_reason"); got != "startup_orphan_reap" {
		t.Fatalf("end reason %v", got)
	}
}

func TestAutoPruneSparesCompressionRootOfLiveTurn(t *testing.T) {
	db := autoDB(t)
	stale := ago(100 * 24)
	sweepMake(t, db, "root", "cli", stale, &stale)
	if err := db.EndSession("root", "compression"); err != nil {
		t.Fatal(err)
	}
	readSession(t, db, NewSession{ID: "tip", Source: "cli", ParentSessionID: "root"})
	readAppend(t, db, "tip", map[string]any{"role": "user", "content": "continued"})
	profileExec(t, db, "UPDATE messages SET timestamp = ? WHERE session_id = ?", stale, "tip")
	profileExec(t, db, "UPDATE sessions SET started_at = ?, last_activity_at = ? WHERE id = ?", stale, stale, "tip")
	autoLease(t, db, "tip", "external-turn")
	if r := autoPrune(t, db, 0); r.Pruned != 0 || autoSession(t, db, "root") == nil || autoField(t, db, "tip", "ended_at") != nil {
		t.Fatalf("result %+v", r)
	}
}

func TestAutoPruneSparesPriorSweepRowWithNewTurnLease(t *testing.T) {
	db := autoDB(t)
	autoStale(t, db, "racy", "cli", ago(100*24))
	if err := db.EndSession("racy", "startup_orphan_reap"); err != nil {
		t.Fatal(err)
	}
	autoLease(t, db, "racy", "arriving-turn")
	if r := autoPrune(t, db, 0); r.Pruned != 0 || autoSession(t, db, "racy") == nil {
		t.Fatalf("result %+v", r)
	}
}

func TestAutoPruneSparesPriorSweepRowWithNewCompressionLock(t *testing.T) {
	db := autoDB(t)
	autoStale(t, db, "racy-compression", "cli", ago(100*24))
	if err := db.EndSession("racy-compression", "startup_orphan_reap"); err != nil {
		t.Fatal(err)
	}
	if !db.TryAcquireCompressionLock("racy-compression", "arriving-compressor", 300) {
		t.Fatal("compression lock")
	}
	if r := autoPrune(t, db, 0); r.Pruned != 0 || autoSession(t, db, "racy-compression") == nil {
		t.Fatalf("result %+v", r)
	}
}

func TestAutoPruneReportsClosedCountAndDeletesAfterSecondWindow(t *testing.T) {
	db := autoDB(t)
	stale := ago(200 * 24)
	for _, s := range [][2]string{{"cron-0", "cron"}, {"kanban-1", "kanban"}, {"subagent-2", "subagent"}, {"telegram-3", "telegram"}} {
		autoStale(t, db, s[0], s[1], stale)
	}
	if first := autoPrune(t, db, 0); first.Closed != 3 || first.Pruned != 0 {
		t.Fatalf("first %+v", first)
	}
	for _, sid := range []string{"cron-0", "kanban-1", "subagent-2"} {
		if got := autoField(t, db, sid, "end_reason"); got != "startup_orphan_reap" {
			t.Fatalf("%s end reason %v", sid, got)
		}
	}
	if got := autoField(t, db, "telegram-3", "ended_at"); got != nil {
		t.Fatalf("telegram ended %v", got)
	}
	profileExec(t, db, "UPDATE sessions SET ended_at = ended_at - 91 * 86400 WHERE end_reason = 'startup_orphan_reap'")
	if second := autoPrune(t, db, 0); second.Closed != 0 || second.Pruned != 3 {
		t.Fatalf("second %+v", second)
	}
	rows, err := db.readAll(context.Background(), "SELECT id FROM sessions")
	if err != nil || len(rows) != 1 || rowValue(rows[0], "id") != "telegram-3" {
		t.Fatalf("remaining %v %v", rows, err)
	}
}

func TestAutoPruneClosesAndLaterDeletesRecoveredRows(t *testing.T) {
	db := autoDB(t)
	autoStale(t, db, "recovered-0", "recovered", ago(200*24))
	if first := autoPrune(t, db, 0); first.Closed != 1 || autoField(t, db, "recovered-0", "end_reason") != "startup_orphan_reap" {
		t.Fatalf("first %+v", first)
	}
	profileExec(t, db, "UPDATE sessions SET ended_at = ended_at - 91 * 86400 WHERE id = 'recovered-0'")
	if second := autoPrune(t, db, 0); second.Pruned != 1 || autoSession(t, db, "recovered-0") != nil {
		t.Fatalf("second %+v", second)
	}
}

func pruneDays(days float64) *float64 { return &days }

func TestPruneSessionsRespectsTouchSessionActivity(t *testing.T) {
	ctx := context.Background()
	db := autoDB(t)
	oldTime, recentTime := ago(100*24), ago(2*24)
	for _, s := range [][2]string{{"active_by_heartbeat", "hello"}, {"truly_old", "old message"}} {
		readSession(t, db, NewSession{ID: s[0], Source: "cli"})
		readAppend(t, db, s[0], map[string]any{"role": "user", "content": s[1], "timestamp": oldTime})
		if err := db.EndSession(s[0], "done"); err != nil {
			t.Fatal(err)
		}
		profileExec(t, db, "UPDATE sessions SET started_at = ?, ended_at = ? WHERE id = ?", oldTime, oldTime, s[0])
		if s[0] == "active_by_heartbeat" {
			if err := db.TouchSessionActivity(s[0], &recentTime, "", ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	candidates, err := db.ListPruneCandidates(ctx, pruneDays(10), "", false, PruneFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if ids := projIDs(candidates); ids["active_by_heartbeat"] || !ids["truly_old"] {
		t.Fatalf("candidates %v", ids)
	}
	all, err := db.ListPruneCandidates(ctx, nil, "", false, PruneFilters{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := pyFloatOf(rowValue(projRow(t, all, "active_by_heartbeat"), "last_active"))
	if err != nil || math.Abs(got-recentTime) > 1.0 {
		t.Fatalf("last active %v want %v %v", got, recentTime, err)
	}
	pruned, err := db.PruneSessions(ctx, PruneSessionsOptions{OlderThanDays: pruneDays(10)})
	if err != nil || pruned != 1 || autoSession(t, db, "truly_old") != nil || autoSession(t, db, "active_by_heartbeat") == nil {
		t.Fatalf("pruned %d %v", pruned, err)
	}
}

func pruneRotate(t *testing.T, db *SessionDB, parent, child string, at float64) {
	t.Helper()
	p := NewCompressionChildPublish(parent, child, "cli", []map[string]any{{"role": "user", "content": "[summary]", "timestamp": at}})
	p.RequireCompressionLease = false
	if err := db.PublishCompressionChild(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	profileExec(t, db, "UPDATE sessions SET ended_at = ? WHERE id = ?", at, parent)
	profileExec(t, db, "UPDATE sessions SET started_at = ? WHERE id = ?", at, child)
}

func pruneLineage(t *testing.T, db *SessionDB, sid string) []string {
	t.Helper()
	got, err := db.GetCompressionLineage(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPruneKeepsTheCompressedSegmentsOfAChatStillInUse(t *testing.T) {
	ctx := context.Background()
	db := autoDB(t)
	old := ago(120 * 24)
	for _, chat := range []string{"live", "idle"} {
		readSession(t, db, NewSession{ID: chat, Source: "cli"})
		readAppend(t, db, chat, map[string]any{"role": "user", "content": chat + " early detail", "timestamp": old})
		profileExec(t, db, "UPDATE sessions SET started_at = ? WHERE id = ?", old, chat)
		pruneRotate(t, db, chat, chat+"-2", old+60)
		pruneRotate(t, db, chat+"-2", chat+"-3", old+120)
	}
	readAppend(t, db, "live-3", map[string]any{"role": "user", "content": "still talking today"})
	if err := db.EndSession("idle-3", "done"); err != nil {
		t.Fatal(err)
	}
	profileExec(t, db, "UPDATE sessions SET ended_at = ? WHERE id = 'idle-3'", old+180)
	idle := map[string]bool{"idle": true, "idle-2": true, "idle-3": true}
	profileExec(t, db, "UPDATE sessions SET pinned = 1 WHERE id = 'idle-3'")
	if n, err := db.CountPruneMatches(ctx, pruneDays(90), "", true, PruneFilters{}); err != nil || n != 1 {
		t.Fatalf("pinned matches %d %v", n, err)
	}
	profileExec(t, db, "UPDATE sessions SET pinned = 0 WHERE id = 'idle-3'")
	candidates, err := db.ListPruneCandidates(ctx, pruneDays(90), "", true, PruneFilters{})
	if err != nil || !reflect.DeepEqual(projIDs(candidates), idle) {
		t.Fatalf("candidates %v %v", projIDs(candidates), err)
	}
	if n, err := db.PruneSessions(ctx, PruneSessionsOptions{OlderThanDays: pruneDays(90)}); err != nil || n != len(idle) {
		t.Fatalf("pruned %d %v", n, err)
	}
	if got := pruneLineage(t, db, "live-3"); !reflect.DeepEqual(got, []string{"live", "live-2", "live-3"}) {
		t.Fatalf("lineage %v", got)
	}
	for sid := range idle {
		if autoSession(t, db, sid) != nil {
			t.Fatalf("%s survived", sid)
		}
	}
}

func pinnedThenRotated(t *testing.T, db *SessionDB) {
	t.Helper()
	old := ago(120 * 24)
	readSession(t, db, NewSession{ID: "keep", Source: "cli"})
	readAppend(t, db, "keep", map[string]any{"role": "user", "content": "early detail", "timestamp": old})
	if ok, err := db.SetSessionPinned(context.Background(), "keep", true); err != nil || !ok {
		t.Fatalf("pin %v %v", ok, err)
	}
	p := NewCompressionChildPublish("keep", "keep-2", "cli", []map[string]any{{"role": "user", "content": "[summary]", "timestamp": old + 60}})
	p.RequireCompressionLease = false
	if err := db.PublishCompressionChild(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession("keep-2", "done"); err != nil {
		t.Fatal(err)
	}
	profileExec(t, db, "UPDATE sessions SET started_at = ?, ended_at = ? WHERE id IN ('keep', 'keep-2')", old, old+120)
}

func TestAPinFollowsTheChatThroughCompressionAndBulkCleanupSparesIt(t *testing.T) {
	ctx := context.Background()
	db := autoDB(t)
	pinnedThenRotated(t, db)
	if a, b := autoField(t, db, "keep-2", "pinned"), autoField(t, db, "keep", "pinned"); a != int64(1) || b != int64(1) {
		t.Fatalf("pinned %v %v", a, b)
	}
	profileExec(t, db, "UPDATE sessions SET pinned = 0 WHERE id = 'keep-2'")
	if c, err := db.ListPruneCandidates(ctx, pruneDays(90), "", true, PruneFilters{}); err != nil || len(c) != 0 {
		t.Fatalf("candidates %v %v", c, err)
	}
	if n, err := db.CountPruneMatches(ctx, pruneDays(90), "", true, PruneFilters{}); err != nil || n != 1 {
		t.Fatalf("pinned matches %d %v", n, err)
	}
	if n, err := db.PruneSessions(ctx, PruneSessionsOptions{OlderThanDays: pruneDays(90)}); err != nil || n != 0 {
		t.Fatalf("pruned %d %v", n, err)
	}
	stale, err := db.ArchiveStaleSessions(ctx, 3, true)
	if err != nil {
		t.Fatal(err)
	}
	bulk, err := db.ArchiveSessions(ctx, pruneDays(90), "", PruneFilters{})
	if err != nil || stale != 0 || bulk != 0 {
		t.Fatalf("archived %d %d %v", stale, bulk, err)
	}
	if got := pruneLineage(t, db, "keep"); !reflect.DeepEqual(got, []string{"keep", "keep-2"}) {
		t.Fatalf("lineage %v", got)
	}
	if pyTruthy(autoField(t, db, "keep", "archived")) {
		t.Fatal("keep archived")
	}
	if n, err := db.PruneSessions(ctx, PruneSessionsOptions{OlderThanDays: pruneDays(90), Filters: PruneFilters{IncludePinned: true}}); err != nil || n != 2 {
		t.Fatalf("pruned pinned %d %v", n, err)
	}
}

func TestOrphanSweepSparesTheOpenTipOfAChatPinnedBeforeItRotated(t *testing.T) {
	db := autoDB(t)
	pinnedThenRotated(t, db)
	profileExec(t, db, "UPDATE sessions SET pinned = 0, ended_at = NULL, end_reason = NULL, last_activity_at = started_at WHERE id = 'keep-2'")
	sweep := func(excludePinned bool) []string {
		o := NewSweepOrphanedOptions(90 * 86400)
		o.Sources, o.RespectGatewayHeartbeats, o.ExcludePinned = []string{"cli"}, false, excludePinned
		got, err := db.SweepOrphanedSessions(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	sweepWant(t, sweep(true))
	if got := autoField(t, db, "keep-2", "ended_at"); got != nil {
		t.Fatalf("keep-2 ended %v", got)
	}
	sweepWant(t, sweep(false), "keep-2")
}
