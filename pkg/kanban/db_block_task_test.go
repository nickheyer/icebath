package kanban

import (
	"context"
	"testing"
)

// A same kind re block reads an outside writer's empty block_recurrences as zero like the or 0 idiom
func TestBlockTaskReadsEmptyRecurrencesAsZero(t *testing.T) {
	conn := openTestBoard(t)
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "outside writer", Assignee: sp("worker")})
	if _, err := conn.ExecContext(ctx, "UPDATE tasks SET status = 'ready', block_kind = 'needs_input', block_recurrences = '' WHERE id = ?", tid); err != nil {
		t.Fatal(err)
	}
	ok, err := BlockTaskWithKind(ctx, conn, tid, sp("waiting on legal"), sp("needs_input"), nil)
	if err != nil || !ok {
		t.Fatal("block", ok, err)
	}
	got := mustTask(t, conn, tid)
	if got.Status != "blocked" || got.BlockKind == nil || *got.BlockKind != "needs_input" || got.BlockRecurrences != 1 {
		t.Fatal("blocked row", got.Status, got.BlockKind, got.BlockRecurrences)
	}
}
