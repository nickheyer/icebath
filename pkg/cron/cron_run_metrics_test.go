package cron

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

// Captures every relay process mark, returning a reader of the cron run rows after a drain
func cronRunRows(t *testing.T) func() []map[string]string {
	t.Helper()
	var mu sync.Mutex
	var got []struct {
		mark string
		data map[string]string
	}
	t.Cleanup(observability.SetRelayRecorder(func(context.Context) bool { return true }, func(_ context.Context, mark string, data map[string]string) {
		mu.Lock()
		defer mu.Unlock()
		copied := make(map[string]string, len(data))
		for k, v := range data {
			copied[k] = v
		}
		got = append(got, struct {
			mark string
			data map[string]string
		}{mark, copied})
	}))
	return func() []map[string]string {
		t.Helper()
		if err := observability.Drain(10 * time.Second); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		var found []map[string]string
		for _, r := range got {
			if r.mark != observability.CronRunMark {
				continue
			}
			dims := make(map[string]any, len(r.data))
			for k, v := range r.data {
				dims[k] = v
			}
			if !observability.CounterDimensionsAreValid(observability.CronRunMetric, dims) {
				t.Fatalf("invalid cron run dimensions %v", r.data)
			}
			found = append(found, r.data)
		}
		return found
	}
}

// Claims an attempt noted with its delivery, started unless the claim was lost before any side effect
func meteredExecution(t *testing.T, l *Ledger, jobID, deliver string, start bool) string {
	t.Helper()
	ctx := context.Background()
	row := mustLedger(t)(l.Create(ctx, jobID, "builtin", nil))
	id := recText(row, "id")
	observability.NoteCronExecution(map[string]any{"execution_id": id, "deliver": deliver})
	if start {
		mustLedger(t)(l.MarkRunning(ctx, id))
	}
	return id
}

func TestEachTerminalExecutionIsOneRowClassifiedWithoutJobDetails(t *testing.T) {
	l, _ := ledgerFixture(t)
	rows := cronRunRows(t)
	ctx := context.Background()
	delivered, suppressed := "delivered", "suppressed"
	ok := meteredExecution(t, l, "job1", "telegram:-100555", true)
	mustLedger(t)(l.Finish(ctx, ok, true, nil, &delivered, false))
	mustLedger(t)(l.Finish(ctx, ok, false, strPtr("late duplicate"), nil, false))
	mustLedger(t)(l.Finish(ctx, meteredExecution(t, l, "job2", "local", true), false, strPtr("boom"), nil, false))
	refused := meteredExecution(t, l, "job3", "webhook", false)
	mustLedger(t)(l.Finish(ctx, refused, false, strPtr("Fire claim lost; execution was not started."), nil, false))
	gated := meteredExecution(t, l, "job4", "origin", true)
	observability.NoteCronSkipped(map[string]any{"execution_id": gated})
	mustLedger(t)(l.Finish(ctx, gated, true, nil, &suppressed, false))
	var got [][2]string
	for _, r := range rows() {
		got = append(got, [2]string{r["outcome"], r["delivery_kind"]})
	}
	want := [][2]string{{"success", "platform"}, {"failed", "local"}, {"skipped", "webhook"}, {"skipped", "platform"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cron rows %v", got)
	}
}

func TestAnOccurrenceDroppedAfterDowntimeIsOneMissedRow(t *testing.T) {
	s, _ := newTestStore(t)
	rows := cronRunRows(t)
	if err := os.WriteFile(filepath.Join(s.Home(), "config.yaml"), []byte("cron:\n  catch_up_missed: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createJob(context.Background(), CreateJobOptions{Prompt: strPtr("secret prompt"), Schedule: "every 1h", Model: "fixture", Deliver: strPtr("local")}); err != nil {
		t.Fatal(err)
	}
	stored, err := s.LoadJobs(context.Background())
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored %v %v", stored, err)
	}
	stale := FormatISO(s.clk.now().Add(-4 * time.Hour))
	stored[0].NextRunAt = &stale
	if err := s.SaveJobs(context.Background(), stored, nil, false); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for range 2 {
		if due, err := s.getDueJobs(ctx); err != nil || len(due) != 0 {
			t.Fatalf("due %v %v", due, err)
		}
	}
	want := []map[string]string{{"delivery_kind": "local", "duration_bucket": "lt_1s", "outcome": "missed"}}
	if got := rows(); !reflect.DeepEqual(got, want) {
		t.Fatalf("cron rows %v", got)
	}
}

func TestAStoppedRunRowValidatesAgainstThePackageSchema(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	schemaPath, err := filepath.Abs(filepath.Join("..", "observability", "schemas", "icebath.shared_metrics.v4.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	resource := map[string]string{"architecture": "x86_64", "icebath_version": "test", "install_method": "git", "os_family": "linux"}
	store, err := observability.NewSharedMetricsStore(ctx, filepath.Join(dir, "metrics.sqlite3"), filepath.Join(dir, "outbox"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordCounter(ctx, observability.CronRunMetric, observability.CronRunFields("cancelled", "local", 3), resource, 1); err != nil {
		t.Fatal(err)
	}
	paths, err := store.CreateAndExportPackage(ctx)
	if err != nil || len(paths) != 1 {
		t.Fatalf("packages %v %v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var pkg any
	if err := json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &pkg); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	schema, err := compiler.Compile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(pkg); err != nil {
		t.Fatal(err)
	}
}
