//go:build !windows

package gateway

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestReceiptFencesWrongSessionGenerationAndOwnerEpoch(t *testing.T) {
	ctx, db := newTestStateDB(t)
	for _, sid := range []string{"parent", "foreign"} {
		if err := db.CreateSession(state.NewSession{ID: sid, Source: "cli"}); err != nil {
			t.Fatal(err)
		}
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RegisterWorkerExecution(ctx, state.WorkerRegistration{Epoch: epoch, ExecutionID: "worker", SessionID: "parent",
		Kind: "compute", AdoptionSecret: "private"}); err != nil {
		t.Fatal(err)
	}
	birth := birthOf(t, os.Getpid())
	for _, tc := range []struct {
		override func(*state.WorkerReceipt)
		reason   string
	}{
		{func(r *state.WorkerReceipt) { r.SessionID = "foreign" }, "permission_denied"},
		{func(r *state.WorkerReceipt) { r.Generation = 1 }, "stale_generation"},
		{func(r *state.WorkerReceipt) { r.Epoch = epoch + 1 }, "stale_epoch"},
		{func(r *state.WorkerReceipt) { r.Operation = "execute_sql" }, "invalid_params"},
	} {
		receipt := state.WorkerReceipt{Epoch: epoch, ExecutionID: "worker", SessionID: "parent", Sequence: 1,
			Operation: "delegation.read", Payload: pyjson.ObjectOf("delegation_id", "none")}
		tc.override(&receipt)
		if _, err := persistWorkerDelegation(ctx, db, receipt, int64(os.Getpid()), birth); !isRuntimeRefusal(err, tc.reason) {
			t.Fatalf("want %s, got %v", tc.reason, err)
		}
	}
	var receipts int64
	if err := db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		row, err := state.ConnRow(ctx, conn, "SELECT COUNT(*) AS n FROM worker_receipts")
		if err == nil {
			receipts = row["n"].(int64)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if receipts != 0 {
		t.Fatalf("worker_receipts %d", receipts)
	}
}
