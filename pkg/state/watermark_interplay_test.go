package state

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func interplayDB(t *testing.T, sid string, contents ...string) *SessionDB {
	t.Helper()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: sid, Source: "cli"})
	for _, c := range contents {
		readAppend(t, db, sid, map[string]any{"role": "user", "content": c})
	}
	return db
}

func interplayHits(t *testing.T, db *SessionDB, sid, query string) int {
	t.Helper()
	hits, err := db.SearchMessages(context.Background(), query, NewSearchMessagesOptions())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, h := range hits {
		if h.Vals["session_id"] == sid {
			n++
		}
	}
	return n
}

func TestRewindWalkBoundedAtWatermark(t *testing.T) {
	sid := "s-wm-bound"
	db := interplayDB(t, sid, "seen-0", "seen-1", "seen-2", "seen-3")
	watermark := archiveWatermark(t, db, sid)
	readAppend(t, db, sid, map[string]any{"role": "user", "content": "concurrent-append"})
	archiveCompact(t, db, sid, []map[string]any{{"role": "user", "content": "SUMMARY"}, {"role": "user", "content": "seen-2"}, {"role": "user", "content": "seen-3"}},
		ArchiveCompactOptions{Watermark: watermark, TailCount: 2})
	for _, content := range []string{"seen-2", "seen-3"} {
		var originals, live int
		for _, r := range archiveCopies(t, db, sid, content) {
			if r.active == 0 {
				originals++
				if r.compacted != 0 {
					t.Fatalf("%s original stamped compacted=1", content)
				}
			} else if r.active == 1 {
				live++
			}
		}
		if originals == 0 || live != 1 {
			t.Fatalf("%s originals %d live %d", content, originals, live)
		}
	}
	for _, content := range []string{"seen-0", "seen-1"} {
		rows := archiveCopies(t, db, sid, content)
		if len(rows) != 1 || rows[0].active != 0 || rows[0].compacted != 1 {
			t.Fatalf("%s rows %+v", content, rows)
		}
	}
}

func TestConcurrentTailOriginalTakesRewindFlags(t *testing.T) {
	sid := "s-wm-clone"
	db := interplayDB(t, sid, "old-0", "old-1", "old-2")
	watermark := archiveWatermark(t, db, sid)
	readAppend(t, db, sid, map[string]any{"role": "user", "content": "mid-flight zqx-token"})
	archiveCompact(t, db, sid, []map[string]any{{"role": "user", "content": "SUMMARY"}}, ArchiveCompactOptions{Watermark: watermark})
	copies := archiveCopies(t, db, sid, "mid-flight zqx-token")
	if len(copies) != 2 {
		t.Fatalf("copies = %+v", copies)
	}
	for _, r := range copies {
		if r.compacted != 0 {
			t.Fatalf("concurrent tail copy stamped compacted=1: %+v", r)
		}
	}
	if copies[0].active == copies[1].active {
		t.Fatalf("want one original and one clone: %+v", copies)
	}
	if n := interplayHits(t, db, sid, "zqx-token"); n != 1 {
		t.Fatalf("hits = %d", n)
	}
}

func TestRecallStableAcrossGenerations(t *testing.T) {
	sid := "s-gen"
	var turns []string
	for i := range 5 {
		turns = append(turns, fmt.Sprintf("turn-%d gentok-%d", i, i))
	}
	db := interplayDB(t, sid, turns...)
	var counts []int
	for range 3 {
		payload := []map[string]any{{"role": "user", "content": "SUMMARY"}, {"role": "user", "content": "turn-3 gentok-3"}, {"role": "user", "content": "turn-4 gentok-4"}}
		archiveCompact(t, db, sid, payload, ArchiveCompactOptions{TailCount: 2})
		counts = append(counts, interplayHits(t, db, sid, "gentok-3"))
	}
	if fmt.Sprint(counts) != "[1 1 1]" {
		t.Fatalf("counts = %v", counts)
	}
}

func TestRewindToEmptyDriftsTheCounterToZero(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "rewound", Source: "desktop", Model: "test-model"})
	for i := range 4 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		readAppend(t, db, "rewound", map[string]any{"role": role, "content": fmt.Sprintf("turn %d", i)})
	}
	counts := func() string {
		row, err := db.readOne(ctx, "SELECT message_count, (SELECT COUNT(*) FROM messages WHERE session_id = ?) AS n FROM sessions WHERE id = ?", "rewound", "rewound")
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(row.Vals["message_count"], " ", row.Vals["n"])
	}
	if got := counts(); got != "4 4" {
		t.Fatalf("seeded counts %s", got)
	}
	if err := db.ReplaceMessages(ctx, "rewound", []map[string]any{}, ReplaceOptions{ArchiveDropped: true}); err != nil {
		t.Fatal(err)
	}
	if got := counts(); got != "0 4" {
		t.Fatalf("counts after the rewind %s", got)
	}
	rows, err := db.GetMessages(ctx, "rewound", GetMessagesOptions{IncludeInactive: true})
	if err != nil || len(rows) != 4 || !strings.HasPrefix(pyStr(rows[0].Vals["content"]), "turn") {
		t.Fatalf("inactive rows %d err %v", len(rows), err)
	}
}
