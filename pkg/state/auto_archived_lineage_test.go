package state

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func sweepArchiveDB(t *testing.T) *SessionDB {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(dir, "home"))
	return usageOpen(t, filepath.Join(dir, "state.db"))
}

func sidebarIDs(t *testing.T, db *SessionDB) map[string]bool {
	t.Helper()
	limit := 50
	rows, err := db.ListSessionsRich(context.Background(), ListSessionsRichOptions{Limit: &limit, MinMessageCount: 1,
		OrderByLastActive: true, CompactRows: true, IncludePinned: true})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, r := range rows {
		out[pyStr(rowValue(r, "id"))] = true
	}
	return out
}

func archiveCompress(t *testing.T, db *SessionDB, parent, child string) {
	t.Helper()
	holder := "holder-" + child
	if !db.TryAcquireCompressionLock(parent, holder, 60) {
		t.Fatalf("lock %s", parent)
	}
	p := NewCompressionChildPublish(parent, child, "telegram", []map[string]any{{"role": "user", "content": "summary for " + child}})
	prompt := "p"
	p.SystemPrompt, p.CompressionLockHolder = &prompt, &holder
	if err := db.PublishCompressionChild(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

type archiveFlag struct{ archived, auto any }

func archiveFlags(t *testing.T, db *SessionDB, ids ...string) map[string]archiveFlag {
	t.Helper()
	out := map[string]archiveFlag{}
	for _, id := range ids {
		row, err := db.GetSession(context.Background(), id)
		if err != nil || row == nil {
			t.Fatalf("session %s %v", id, err)
		}
		out[id] = archiveFlag{rowValue(row, "archived"), rowValue(row, "auto_archived")}
	}
	return out
}

func sameFlags(ids []string, archived, auto int64) map[string]archiveFlag {
	out := map[string]archiveFlag{}
	for _, id := range ids {
		out[id] = archiveFlag{archived, auto}
	}
	return out
}

func archiveLineage(t *testing.T, db *SessionDB) {
	t.Helper()
	readSession(t, db, NewSession{ID: "root", Source: "telegram"})
	wmAppend(t, db, "root", map[string]any{"role": "user", "content": "hello"}, AppendGuards{})
	archiveCompress(t, db, "root", "mid")
	archiveCompress(t, db, "mid", "tip")
	wmAppend(t, db, "tip", map[string]any{"role": "user", "content": "latest"}, AppendGuards{})
}

func archiveSweep(t *testing.T, db *SessionDB) int {
	t.Helper()
	time.Sleep(20 * time.Millisecond)
	n, err := db.ArchiveStaleSessions(context.Background(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func setArchived(t *testing.T, db *SessionDB, id string, archived bool) {
	t.Helper()
	if ok, err := db.SetSessionArchived(context.Background(), id, archived, true); !ok || err != nil {
		t.Fatalf("archive %s %v %v", id, ok, err)
	}
}

func TestCompressionAfterAutoArchiveUnhidesTheLiveChat(t *testing.T) {
	db := sweepArchiveDB(t)
	archiveLineage(t, db)
	if n := archiveSweep(t, db); n != 1 {
		t.Fatalf("swept %d", n)
	}
	three := []string{"root", "mid", "tip"}
	if got := archiveFlags(t, db, three...); !reflect.DeepEqual(got, sameFlags(three, 1, 1)) {
		t.Fatalf("flags %v", got)
	}
	if ids := sidebarIDs(t, db); len(ids) != 0 {
		t.Fatalf("sidebar %v", ids)
	}
	archiveCompress(t, db, "tip", "tip2")
	four := []string{"root", "mid", "tip", "tip2"}
	if got := archiveFlags(t, db, four...); !reflect.DeepEqual(got, sameFlags(four, 0, 0)) {
		t.Fatalf("flags %v", got)
	}
	if ids := sidebarIDs(t, db); !reflect.DeepEqual(ids, map[string]bool{"tip2": true}) {
		t.Fatalf("sidebar %v", ids)
	}
}

func TestCompressionKeepsAManuallyArchivedChatHidden(t *testing.T) {
	db := sweepArchiveDB(t)
	archiveLineage(t, db)
	setArchived(t, db, "tip", true)
	archiveCompress(t, db, "tip", "tip2")
	four := []string{"root", "mid", "tip", "tip2"}
	if got := archiveFlags(t, db, four...); !reflect.DeepEqual(got, sameFlags(four, 1, 0)) {
		t.Fatalf("flags %v", got)
	}
	if ids := sidebarIDs(t, db); len(ids) != 0 {
		t.Fatalf("sidebar %v", ids)
	}
	if n := archiveSweep(t, db); n != 0 {
		t.Fatalf("swept %d", n)
	}
	archiveCompress(t, db, "tip2", "tip3")
	if ids := sidebarIDs(t, db); len(ids) != 0 {
		t.Fatalf("sidebar %v", ids)
	}
}

func TestManualArchiveOverridesSweepProvenance(t *testing.T) {
	db := sweepArchiveDB(t)
	archiveLineage(t, db)
	if n := archiveSweep(t, db); n != 1 {
		t.Fatalf("swept %d", n)
	}
	setArchived(t, db, "root", true)
	three := []string{"root", "mid", "tip"}
	if got := archiveFlags(t, db, three...); !reflect.DeepEqual(got, sameFlags(three, 1, 0)) {
		t.Fatalf("flags %v", got)
	}
	archiveCompress(t, db, "tip", "tip2")
	if ids := sidebarIDs(t, db); len(ids) != 0 {
		t.Fatalf("sidebar %v", ids)
	}
}

func TestSweepDoesNotRelabelAManuallyArchivedAncestor(t *testing.T) {
	db := sweepArchiveDB(t)
	archiveLineage(t, db)
	profileExec(t, db, "UPDATE sessions SET archived = 1 WHERE id IN ('root', 'mid')")
	if n := archiveSweep(t, db); n != 1 {
		t.Fatalf("swept %d", n)
	}
	want := map[string]archiveFlag{"root": {int64(1), int64(0)}, "mid": {int64(1), int64(0)}, "tip": {int64(1), int64(1)}}
	if got := archiveFlags(t, db, "root", "mid", "tip"); !reflect.DeepEqual(got, want) {
		t.Fatalf("flags %v", got)
	}
	archiveCompress(t, db, "tip", "tip2")
	if ids := sidebarIDs(t, db); len(ids) != 0 {
		t.Fatalf("sidebar %v", ids)
	}
	if got := archiveFlags(t, db, "root"); got["root"].archived != int64(1) {
		t.Fatalf("root %v", got)
	}
}

func TestResumeReopenUnhidesAutoArchivedButNotManual(t *testing.T) {
	ctx := context.Background()
	db := sweepArchiveDB(t)
	for _, id := range []string{"auto", "manual"} {
		readSession(t, db, NewSession{ID: id, Source: "cli"})
		wmAppend(t, db, id, map[string]any{"role": "user", "content": id[:1]}, AppendGuards{})
		if err := db.EndSession(id, "cli_close"); err != nil {
			t.Fatal(err)
		}
	}
	setArchived(t, db, "manual", true)
	if n := archiveSweep(t, db); n != 1 {
		t.Fatalf("swept %d", n)
	}
	want := map[string]archiveFlag{"auto": {int64(1), int64(1)}, "manual": {int64(1), int64(0)}}
	if got := archiveFlags(t, db, "auto", "manual"); !reflect.DeepEqual(got, want) {
		t.Fatalf("flags %v", got)
	}
	for _, id := range []string{"auto", "manual"} {
		if err := db.ReopenSession(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	want["auto"] = archiveFlag{int64(0), int64(0)}
	if got := archiveFlags(t, db, "auto", "manual"); !reflect.DeepEqual(got, want) {
		t.Fatalf("flags %v", got)
	}
	if ids := sidebarIDs(t, db); !reflect.DeepEqual(ids, map[string]bool{"auto": true}) {
		t.Fatalf("sidebar %v", ids)
	}
}

func TestUnarchiveClearsSweepProvenance(t *testing.T) {
	db := sweepArchiveDB(t)
	archiveLineage(t, db)
	if n := archiveSweep(t, db); n != 1 {
		t.Fatalf("swept %d", n)
	}
	setArchived(t, db, "tip", false)
	three := []string{"root", "mid", "tip"}
	if got := archiveFlags(t, db, three...); !reflect.DeepEqual(got, sameFlags(three, 0, 0)) {
		t.Fatalf("flags %v", got)
	}
}

func TestOwnerStartListsALegacyCompressedChainOnce(t *testing.T) {
	ctx := context.Background()
	for _, rootCreated := range []any{nil, "unknown", "local"} {
		db := usageDB(t)
		mid := createdCompress(t, db, rootCreated, "mid")
		createdPublish(t, db, mid, "tip", "local")
		profileExec(t, db, "UPDATE sessions SET created_source=? WHERE id=?", rootCreated, mid)
		profileExec(t, db, "UPDATE sessions SET created_source='cli' WHERE id='tip'")
		listed := func() []string {
			rows, err := db.ListSessionsRich(ctx, ListSessionsRichOptions{Sources: []string{"cli", "local"}, LocalCreatedSources: []string{"cli"}})
			if err != nil {
				t.Fatal(err)
			}
			ids := []string{}
			for _, r := range rows {
				ids = append(ids, pyStr(rowValue(r, "id")))
			}
			return ids
		}
		if got := listed(); len(got) != 0 {
			t.Fatalf("root %v listed before repair %v", rootCreated, got)
		}
		if n, err := db.StampLegacyCompressionChains(ctx); n != 2 || err != nil {
			t.Fatalf("stamped %d err %v", n, err)
		}
		if got := createdOf(t, db, "parent-mid", mid, "tip"); !reflect.DeepEqual(got, []any{"cli", "cli", "cli"}) {
			t.Fatalf("stamps %v", got)
		}
		if got := listed(); !reflect.DeepEqual(got, []string{"tip"}) {
			t.Fatalf("listed after repair %v", got)
		}
		if n, err := db.StampLegacyCompressionChains(ctx); n != 0 || err != nil {
			t.Fatalf("second start stamped %d err %v", n, err)
		}
	}
}
