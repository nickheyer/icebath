package observability

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Commit counter of the store as another connection sees it, PRAGMA data_version
func storeDataVersion(t *testing.T, conn state.SQLiteConn) int64 {
	t.Helper()
	var version int64
	if err := conn.QueryRowContext(context.Background(), "PRAGMA data_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func TestFlushCommitsTheCountersItDeliversInOneTransaction(t *testing.T) {
	fake, ctx := directRuntime(t)
	rt := getRuntime(ctx, true, nil)
	if rt == nil {
		t.Fatal("no shared metrics runtime")
	}
	joinRuntimeWorkers(t, rt)
	observer, err := state.ConnectSQLite(rt.Subscriber.Store.DatabasePath, state.SQLiteConfig{Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	before := storeDataVersion(t, observer)
	const delivered = 12
	var during []int64
	fake.flushHook = func() error {
		for range delivered {
			fake.emit(&tools.RelayEventView{Kind: "mark", Name: ToolEnabledUnusedMark, Metadata: rt.eventMetadata(),
				Data: map[string]any{"toolset": "web", "used": "no"}})
			during = append(during, storeDataVersion(t, observer))
		}
		return nil
	}
	rt.flushAndExport(ctx, "flush failed")
	fake.flushHook = nil
	for i, version := range during {
		if version != before {
			t.Fatalf("event %d committed inside the flush (data_version %d, before %d)", i, version, before)
		}
	}
	if storeDataVersion(t, observer) == before {
		t.Fatal("the flush committed nothing")
	}
	var total int64
	if err := observer.QueryRowContext(ctx, "SELECT COALESCE(SUM(value), 0) FROM counter_aggregates WHERE metric_name = ?",
		ToolEnabledUnusedMetric).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != delivered {
		t.Fatalf("stored %d of %d counted events", total, delivered)
	}
}

func TestBatchCommitsEveryWriteExceptOneThatFails(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	store, err := NewSharedMetricsStore(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	resource := map[string]string{"icebath_version": "1", "os_family": "linux", "architecture": "x86_64", "install_method": "git"}
	var failure error
	err = store.Batch(ctx, func() error {
		if err := store.RecordCounter(ctx, ToolEnabledUnusedMetric, map[string]string{"toolset": "web", "used": "no"}, resource, 1); err != nil {
			return err
		}
		failure = store.writeOrDefer(ctx, func(ctx context.Context, conn state.SQLiteConn) error {
			_, err := conn.ExecContext(ctx, "INSERT INTO no_such_table VALUES (1)")
			return err
		})
		return store.RecordCounter(ctx, ToolEnabledUnusedMetric, map[string]string{"toolset": "web", "used": "no"}, resource, 1)
	})
	if err != nil {
		t.Fatal(err)
	}
	if failure == nil || !strings.Contains(failure.Error(), "no_such_table") {
		t.Fatalf("the failing write answered its caller %v", failure)
	}
	rows, err := store.CounterSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, row := range rows {
		if row.MetricName == ToolEnabledUnusedMetric {
			total += row.Value
		}
	}
	if total != 2 {
		t.Fatalf("stored %d, want the two writes around the failing one (%+v)", total, rows)
	}
}
