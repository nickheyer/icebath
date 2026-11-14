package state

import (
	"context"
	"fmt"
	"testing"
)

func emptySeed(t *testing.T, db *SessionDB, sid string) {
	t.Helper()
	readSession(t, db, NewSession{ID: sid, Source: "desktop", Model: "test-model"})
	for i := range 4 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		readAppend(t, db, sid, map[string]any{"role": role, "content": fmt.Sprintf("turn %d", i)})
	}
}

func emptyCounts(t *testing.T, db *SessionDB, sid string) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	counter, err := db.readOne(ctx, "SELECT message_count FROM sessions WHERE id = ?", sid)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.readOne(ctx, "SELECT COUNT(*) AS n FROM messages WHERE session_id = ?", sid)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := rowValue(counter, "message_count").(int64)
	n, _ := rowValue(rows, "n").(int64)
	return c, n
}

func emptyRewind(t *testing.T, db *SessionDB, sid string) {
	t.Helper()
	if err := db.ReplaceMessages(context.Background(), sid, nil, ReplaceOptions{ArchiveDropped: true}); err != nil {
		t.Fatal(err)
	}
}

func emptyInactive(t *testing.T, db *SessionDB, sid string) int {
	t.Helper()
	rows, err := db.GetMessages(context.Background(), sid, GetMessagesOptions{IncludeInactive: true})
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

func emptySweep(t *testing.T, db *SessionDB, wantCount, wantDeleted int) {
	t.Helper()
	ctx := context.Background()
	if n, err := db.CountEmptySessions(ctx); err != nil || n != wantCount {
		t.Fatalf("count = %d, %v want %d", n, err, wantCount)
	}
	if n, err := db.DeleteEmptySessions(ctx, ""); err != nil || n != wantDeleted {
		t.Fatalf("delete = %d, %v want %d", n, err, wantDeleted)
	}
}

func TestSweepSparesARewoundSession(t *testing.T) {
	installRetirementHooks(t)
	db := leaseDB(t)
	emptySeed(t, db, "rewound")
	emptyRewind(t, db, "rewound")
	if err := db.EndSession("rewound", "ws_orphan_reap"); err != nil {
		t.Fatal(err)
	}
	emptySweep(t, db, 0, 0)
	if guardSession(t, db, "rewound") == nil || emptyInactive(t, db, "rewound") != 4 {
		t.Fatal("rewound session swept")
	}
}

func TestSweepSparesAnInPlaceCompactedSession(t *testing.T) {
	installRetirementHooks(t)
	db := leaseDB(t)
	emptySeed(t, db, "compacted")
	if _, err := db.ArchiveAndCompact(context.Background(), "compacted", nil, ArchiveCompactOptions{}); err != nil {
		t.Fatal(err)
	}
	if c, n := emptyCounts(t, db, "compacted"); c != 0 || n != 4 {
		t.Fatalf("counts %d %d", c, n)
	}
	if err := db.EndSession("compacted", "ws_orphan_reap"); err != nil {
		t.Fatal(err)
	}
	emptySweep(t, db, 0, 0)
	if guardSession(t, db, "compacted") == nil || emptyInactive(t, db, "compacted") != 4 {
		t.Fatal("compacted session swept")
	}
}

func TestGenuinelyEmptySessionsAreStillSwept(t *testing.T) {
	installRetirementHooks(t)
	db := leaseDB(t)
	readSession(t, db, NewSession{ID: "ghost", Source: "desktop"})
	if err := db.EndSession("ghost", "tui_close"); err != nil {
		t.Fatal(err)
	}
	emptySeed(t, db, "populated")
	if err := db.EndSession("populated", "tui_close"); err != nil {
		t.Fatal(err)
	}
	emptySweep(t, db, 1, 1)
	if guardSession(t, db, "ghost") != nil || guardSession(t, db, "populated") == nil {
		t.Fatal("sweep scope")
	}
}

func TestCountAndDeleteAgreeOnAMixedDatabase(t *testing.T) {
	installRetirementHooks(t)
	db := leaseDB(t)
	readSession(t, db, NewSession{ID: "ghost", Source: "desktop"})
	if err := db.EndSession("ghost", "tui_close"); err != nil {
		t.Fatal(err)
	}
	emptySeed(t, db, "rewound")
	emptyRewind(t, db, "rewound")
	if err := db.EndSession("rewound", "ws_orphan_reap"); err != nil {
		t.Fatal(err)
	}
	emptySeed(t, db, "live")
	emptySweep(t, db, 1, 1)
}
