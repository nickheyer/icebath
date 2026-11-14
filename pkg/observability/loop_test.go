package observability

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

const enabledProfileYAML = "telemetry:\n  shared_metrics:\n    enabled: true\n"

// Flushed rows of one metric in the context profile as sorted dimension keys and values, nil without a store
func loopRows(t *testing.T, ctx context.Context, metric string) []string {
	t.Helper()
	settleStores(t, ctx)
	if _, err := os.Stat(filepath.Join(metricsRoot(ctx), "metrics.sqlite3")); err != nil {
		return nil
	}
	var out []string
	for _, v := range storedValues(t, ctx, metric) {
		out = append(out, fmt.Sprintf("%s|%d", dimsKey(v.dims), v.value))
	}
	sort.Strings(out)
	return out
}

func loopRow(value int, dims map[string]any) string {
	return fmt.Sprintf("%s|%d", dimsKey(dims), value)
}

func TestDelegationFanoutSplitIntoUnitsIsOneRowInTheParentProfile(t *testing.T) {
	_, ctx := directRuntime(t)
	call := "task-a|task-b|task-c"
	BeginDelegationRun(ctx, call, 3, 2)
	other := filepath.Join(t.TempDir(), "unit-thread-profile")
	writeProfileConfig(t, other, enabledProfileYAML)
	unitCtx := home.WithOverride(ctx, other)
	for _, results := range [][]any{
		{map[string]any{"status": "completed"}},
		{map[string]any{"status": "completed"}, map[string]any{"status": "failed"}},
	} {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			FinishDelegationUnit(unitCtx, call, results, true)
		}()
		wg.Wait()
	}
	if rows := loopRows(t, unitCtx, "icebath.delegation.run.count"); len(rows) != 0 {
		t.Fatalf("unit profile rows %v", rows)
	}
	want := []string{loopRow(1, map[string]any{"depth": "2", "mode": "background", "outcome": "partial", "subagent_count_bucket": "3_to_5"})}
	if rows := loopRows(t, ctx, "icebath.delegation.run.count"); !slices.Equal(rows, want) {
		t.Fatalf("parent rows %v want %v", rows, want)
	}
}

func TestBrowserBackendIsNotResolvedWhileCollectionIsOff(t *testing.T) {
	_, ctx := directRuntime(t)
	writeProfileConfig(t, home.Dir(ctx), "telemetry:\n  shared_metrics:\n    enabled: false\n")
	resolved := 0
	_, _ = RecordBrowserCall(ctx, nil, func() (any, error) { return `{"success": true}`, nil },
		func() (string, error) { resolved++; return "local", nil })
	if resolved != 0 {
		t.Fatalf("backend resolved %d times while collection is off", resolved)
	}
}

func TestBrowserCallsNameTheBackendThatServedThem(t *testing.T) {
	_, ctx := directRuntime(t)
	browserbase := func() (string, error) { return "browserbase", nil }
	served, err := RecordBrowserCall(ctx, nil, func() (any, error) { return `{"success": true}`, nil }, browserbase)
	if err != nil || served != `{"success": true}` {
		t.Fatalf("served %v %v", served, err)
	}
	_, _ = RecordBrowserCall(ctx, nil, func() (any, error) { return `{"success": false, "error": "x"}`, nil }, browserbase)
	if _, err := RecordBrowserCall(ctx, nil, func() (any, error) { return nil, errors.New("boom") },
		func() (string, error) { return "some-private-provider", nil }); err == nil {
		t.Fatal("raised call returned no error")
	}
	settleStores(t, ctx)
	var got []string
	for _, v := range storedValues(t, ctx, "icebath.execution_backend.count") {
		got = append(got, fmt.Sprintf("%v %v %v", v.dims["backend"], v.dims["outcome"], v.dims["error_class"]))
	}
	sort.Strings(got)
	want := []string{"browserbase failed tool_error", "browserbase success none", "other failed exception"}
	if !slices.Equal(got, want) {
		t.Fatalf("rows %v want %v", got, want)
	}
}

func TestTerminalBackendEventsCountTheirResultAndSkipReviewForks(t *testing.T) {
	_, ctx := directRuntime(t)
	env := &tools.Env{Home: home.Dir(ctx)}
	event := func(env *tools.Env, result, errorClass string) {
		RecordToolMetricEvent(ctx, env, tools.Event{Kind: "metric", Tool: "terminal", Data: map[string]any{
			"name": ExecutionBackendMark, "kind": "terminal", "backend": "local", "result": result, "error_class": errorClass}})
	}
	event(env, `{"exit_code": 0, "error": null}`, "")
	event(env, `{"exit_code": 3, "error": null}`, "")
	event(env, `{"exit_code": 124, "error": null}`, "")
	event(env, `{"output": "", "error": "gone"}`, "tool_error")
	review := &tools.Env{Home: home.Dir(ctx), Values: &sync.Map{}}
	review.Values.Store("write_origin", "background_review")
	event(review, `{"exit_code": 0, "error": null}`, "")
	want := []string{
		loopRow(1, map[string]any{"backend": "local", "error_class": "timeout", "kind": "terminal", "outcome": "failed"}),
		loopRow(1, map[string]any{"backend": "local", "error_class": "tool_error", "kind": "terminal", "outcome": "failed"}),
		loopRow(2, map[string]any{"backend": "local", "error_class": "none", "kind": "terminal", "outcome": "success"}),
	}
	sort.Strings(want)
	if rows := loopRows(t, ctx, "icebath.execution_backend.count"); !slices.Equal(rows, want) {
		t.Fatalf("rows %v want %v", rows, want)
	}
}
