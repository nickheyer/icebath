package state

import (
	"context"
	"reflect"
	"testing"
)

func flagSeed(t *testing.T, db *SessionDB, sid string, archived bool) {
	t.Helper()
	readSession(t, db, NewSession{ID: sid, Source: "cli"})
	readAppend(t, db, sid, map[string]any{"role": "user", "content": "hello from " + sid})
	if archived {
		setArchived(t, db, sid, true)
	}
}

func flagIdle(t *testing.T, db *SessionDB, sid string, daysIdle float64) {
	t.Helper()
	readSession(t, db, NewSession{ID: sid, Source: "cli"})
	readAppend(t, db, sid, map[string]any{"role": "user", "content": "msg " + sid})
	old := ago(daysIdle * 24)
	profileExec(t, db, "UPDATE sessions SET started_at = ? WHERE id = ?", old, sid)
	profileExec(t, db, "UPDATE messages SET timestamp = ? WHERE session_id = ?", old, sid)
}

func flagMust(t *testing.T, ok bool, err error) {
	t.Helper()
	if err != nil || !ok {
		t.Fatalf("flag write %v %v", ok, err)
	}
}

func TestSetSessionArchivedRoundtrip(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	flagSeed(t, db, "s1", false)
	for _, want := range []bool{true, false} {
		ok, err := db.SetSessionArchived(ctx, "s1", want, true)
		flagMust(t, ok, err)
		if got := autoField(t, db, "s1", "archived"); got != int64(boolInt(want)) {
			t.Fatalf("archived %v want %v", got, want)
		}
	}
}

func TestArchivedExcludedByDefault(t *testing.T) {
	db := usageDB(t)
	flagSeed(t, db, "live", false)
	flagSeed(t, db, "hidden", true)
	if ids := projIDs(projList(t, db, ListSessionsRichOptions{})); !reflect.DeepEqual(ids, map[string]bool{"live": true}) {
		t.Fatalf("listed %v", ids)
	}
	if n, err := db.SessionCount(context.Background(), SessionCountOptions{}); err != nil || n != 1 {
		t.Fatalf("count %d %v", n, err)
	}
}

func TestSetSessionPinnedRoundtrip(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "s1", Source: "cli"})
	for _, want := range []bool{true, false} {
		ok, err := db.SetSessionPinned(ctx, "s1", want)
		flagMust(t, ok, err)
		if got := autoField(t, db, "s1", "pinned"); got != int64(boolInt(want)) {
			t.Fatalf("pinned %v want %v", got, want)
		}
	}
}

func TestPinningAHiddenSessionMakesItListable(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "s1", Source: "cli"})
	readAppend(t, db, "s1", map[string]any{"role": "user", "content": "hi"})
	ok, err := db.SetSessionHidden(ctx, "s1", true)
	flagMust(t, ok, err)
	ok, err = db.SetSessionPinned(ctx, "s1", true)
	flagMust(t, ok, err)
	if got := autoField(t, db, "s1", "hidden"); got != int64(0) {
		t.Fatalf("hidden %v", got)
	}
	if ids := projIDs(projList(t, db, ListSessionsRichOptions{MinMessageCount: 1})); !ids["s1"] {
		t.Fatalf("listed %v", ids)
	}
}

func TestPinningTheCanonicalBotChatLeavesItHidden(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "bot1", Source: "desktop"})
	if _, err := db.SetSessionTitle(ctx, "bot1", CanonicalBotChatTitle); err != nil {
		t.Fatal(err)
	}
	ok, err := db.SetSessionHidden(ctx, "bot1", true)
	flagMust(t, ok, err)
	ok, err = db.SetSessionPinned(ctx, "bot1", true)
	flagMust(t, ok, err)
	if got := autoField(t, db, "bot1", "hidden"); got != int64(1) {
		t.Fatalf("hidden %v", got)
	}
}

func TestPinnedSessionSurvivesTheLimitWindow(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	for i, sid := range []string{"s0", "s1", "s2", "s3", "s4", "s5"} {
		flagIdle(t, db, sid, float64(6-i))
	}
	ok, err := db.SetSessionPinned(ctx, "s0", true)
	flagMust(t, ok, err)
	ids := func(includePinned bool) []any {
		limit := 3
		var out []any
		for _, r := range projList(t, db, ListSessionsRichOptions{Limit: &limit, MinMessageCount: 1, OrderByLastActive: true, IncludePinned: includePinned}) {
			out = append(out, rowValue(r, "id"))
		}
		return out
	}
	page, withPins := ids(false), ids(true)
	for _, id := range page {
		if id == "s0" {
			t.Fatalf("pin on page %v", page)
		}
	}
	if len(withPins) != len(page)+1 || !reflect.DeepEqual(withPins[:3], page) || withPins[3] != "s0" {
		t.Fatalf("page %v with pins %v", page, withPins)
	}
}

func TestPinnedSessionsAreSpared(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	flagIdle(t, db, "keep", 10)
	ok, err := db.SetSessionPinned(ctx, "keep", true)
	flagMust(t, ok, err)
	if n, err := db.ArchiveStaleSessions(ctx, 3, true); err != nil || n != 0 || autoField(t, db, "keep", "archived") != int64(0) {
		t.Fatalf("archived %d %v", n, err)
	}
	if n, err := db.ArchiveStaleSessions(ctx, 3, false); err != nil || n != 1 || autoField(t, db, "keep", "archived") != int64(1) {
		t.Fatalf("archived %d %v", n, err)
	}
}

func TestSetSessionReadMarksTheLineage(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "root", Source: "cli"})
	profileExec(t, db, "UPDATE sessions SET ended_at = 1, end_reason = 'compression' WHERE id = 'root'")
	readSession(t, db, NewSession{ID: "tip", Source: "cli", ParentSessionID: "root"})
	ok, err := db.SetSessionRead(ctx, "tip", false)
	flagMust(t, ok, err)
	for _, sid := range []string{"root", "tip"} {
		if got := autoField(t, db, sid, "last_read_at"); got != 0.0 {
			t.Fatalf("%s last read %v", sid, got)
		}
	}
	ok, err = db.SetSessionRead(ctx, "tip", true)
	flagMust(t, ok, err)
	if got, _ := pyFloatOf(autoField(t, db, "root", "last_read_at")); got < now()-60 {
		t.Fatalf("root last read %v", got)
	}
}
