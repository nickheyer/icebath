package observability

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

func modelDims() map[string]string {
	return map[string]string{"model": "anthropic/claude-sonnet-4.6", "provider": "openrouter"}
}

func legacyModelDims() map[string]string {
	return map[string]string{"call_role": "primary", "locality": "remote", "model_family": "claude", "outcome": "success",
		"provider_family": "direct"}
}

func testResource() map[string]string { return resourceOf("test-version", "linux", "x86_64", "git") }

func resourceOf(version, osFamily, arch, method string) map[string]string {
	return map[string]string{"architecture": arch, "icebath_version": version, "install_method": method, "os_family": osFamily}
}

// Validator for one package schema version, Draft 2020-12 with format assertions
func schemaValidator(t *testing.T, version int) *jsonschema.Schema {
	t.Helper()
	path, _ := filepath.Abs(filepath.Join("schemas", "icebath.shared_metrics.v"+string(rune('0'+version))+".schema.json"))
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	compiler.AssertFormat = true
	schema, err := compiler.Compile(path)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func validatePackage(t *testing.T, version int, pkg any) {
	t.Helper()
	text, _ := json.Marshal(pkg)
	var doc any
	json.Unmarshal(text, &doc)
	if err := schemaValidator(t, version).Validate(doc); err != nil {
		t.Fatal(err)
	}
}

func openStore(t *testing.T, dir string) *SharedMetricsStore {
	t.Helper()
	s, err := NewSharedMetricsStore(context.Background(), filepath.Join(dir, "metrics.sqlite3"), filepath.Join(dir, "outbox"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func readPackage(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func mustExport(t *testing.T, s *SharedMetricsStore) []string {
	t.Helper()
	paths, err := s.CreateAndExportPackage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func mustDue(t *testing.T, s *SharedMetricsStore) []string {
	t.Helper()
	paths, err := s.CreateAndExportPackageIfDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func mustRecord(t *testing.T, s *SharedMetricsStore, metric string, dims, resource map[string]string) {
	t.Helper()
	if err := s.RecordCounter(context.Background(), metric, dims, resource, 1); err != nil {
		t.Fatal(err)
	}
}

func snapshot(t *testing.T, s *SharedMetricsStore) []CounterRow {
	t.Helper()
	rows, err := s.CounterSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	var u, v any
	json.Unmarshal(x, &u)
	json.Unmarshal(y, &v)
	return reflect.DeepEqual(u, v)
}

func outboxFiles(t *testing.T, dir string) int {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	return len(matches)
}

// Moves the store clock for one test
func setClock(t *testing.T, now *time.Time) {
	saved := utcNow
	utcNow = func() time.Time { return *now }
	t.Cleanup(func() { utcNow = saved })
}

func allPackaged(t *testing.T, s *SharedMetricsStore) bool {
	for _, r := range snapshot(t, s) {
		if r.Value != r.PackagedValue {
			return false
		}
	}
	return true
}

func TestModelCallCounterSurvivesRestartAndExportsOnlyNewDeltas(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	s.RecordModelCall(context.Background(), modelDims(), testResource())
	s.RecordModelCall(context.Background(), modelDims(), testResource())
	first := mustExport(t, s)
	if len(first) != 1 {
		t.Fatal(first)
	}
	firstPkg := readPackage(t, first[0])
	validatePackage(t, 4, firstPkg)
	if _, err := uuid.Parse(firstPkg["package_id"].(string)); err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(firstPkg["install_id"].(string)); err != nil {
		t.Fatal(err)
	}
	want := []any{map[string]any{"name": ModelRouteMetric, "type": "counter", "dimensions": modelDims(), "value": 2}}
	if firstPkg["schema_version"] != "icebath.shared_metrics.v4" || !jsonEqual(firstPkg["resource"], testResource()) ||
		!jsonEqual(firstPkg["metrics"], want) {
		t.Fatal(firstPkg)
	}
	restarted := openStore(t, dir)
	if r := snapshot(t, restarted)[0]; r.Value != 2 || r.PackagedValue != 2 {
		t.Fatalf("%+v", r)
	}
	if paths := mustExport(t, restarted); len(paths) != 0 || outboxFiles(t, filepath.Join(dir, "outbox")) != 1 {
		t.Fatal(paths)
	}
	restarted.RecordModelCall(context.Background(), modelDims(), testResource())
	second := mustExport(t, restarted)
	secondPkg := readPackage(t, second[0])
	if len(second) != 1 || secondPkg["package_id"] == firstPkg["package_id"] || secondPkg["install_id"] != firstPkg["install_id"] ||
		secondPkg["metrics"].([]any)[0].(map[string]any)["value"] != float64(1) {
		t.Fatal(secondPkg)
	}
	if r := snapshot(t, restarted)[0]; r.Value != 3 || r.PackagedValue != 3 {
		t.Fatalf("%+v", r)
	}
}

func TestV2PackagePreservesPendingV1ModelCounters(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	storeExec(t, s, `INSERT INTO counter_aggregates(period_start, metric_name, icebath_version, os_family, architecture,
		install_method, dimensions_json, value, packaged_value) VALUES (?, ?, ?, ?, ?, ?, ?, 3, 0)`,
		utcDate(utcNow()), LegacyModelCallMetric, "test-version", "unknown", "unknown", "unknown", compactJSON(anyDims(legacyModelDims())))
	s.RecordModelCall(context.Background(), modelDims(), resourceOf("test-version", "unknown", "unknown", "unknown"))
	paths := mustExport(t, s)
	pkg := readPackage(t, paths[0])
	validatePackage(t, 4, pkg)
	want := []any{
		map[string]any{"name": LegacyModelCallMetric, "type": "counter", "dimensions": legacyModelDims(), "value": 3},
		map[string]any{"name": ModelRouteMetric, "type": "counter", "dimensions": modelDims(), "value": 1},
	}
	if len(paths) != 1 || pkg["schema_version"] != "icebath.shared_metrics.v4" || !jsonEqual(pkg["metrics"], want) || !allPackaged(t, s) {
		t.Fatal(pkg)
	}
}

func TestV1OutboxPackageExportsUnchangedAfterUpgrade(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	packageID := uuid.NewString()
	payload := map[string]any{
		"schema_version": "icebath.shared_metrics.v1", "package_id": packageID, "install_id": uuid.NewString(),
		"period_start": "2026-07-28T00:00:00Z", "period_end": "2026-07-29T00:00:00Z", "generated_at": "2026-07-29T01:00:00Z",
		"resource": map[string]any{"icebath_version": "legacy-version"},
		"metrics":  []any{map[string]any{"name": LegacyModelCallMetric, "type": "counter", "dimensions": legacyModelDims(), "value": 2}},
	}
	payloadJSON := compactJSON(map[string]any(pyPlain(payload)))
	validatePackage(t, 1, payload)
	storeExec(t, s, "INSERT INTO package_outbox(package_id, period_start, period_end, payload_json, created_at) VALUES (?, ?, ?, ?, ?)",
		packageID, payload["period_start"], payload["period_end"], payloadJSON, payload["generated_at"])
	want := filepath.Join(dir, "outbox", packageID+".json")
	if paths := mustExport(t, s); len(paths) != 1 || paths[0] != want {
		t.Fatal(paths)
	}
	if !jsonEqual(readPackage(t, want), payload) {
		t.Fatal(readPackage(t, want))
	}
	if got := storeScalar[string](t, s, "SELECT payload_json FROM package_outbox WHERE package_id = ?", packageID); got != payloadJSON {
		t.Fatal(got)
	}
}

// Plain JSON round trip turning typed maps into map[string]any
func pyPlain(v any) map[string]any {
	text, _ := json.Marshal(v)
	var out map[string]any
	json.Unmarshal(text, &out)
	for _, m := range out["metrics"].([]any) {
		m.(map[string]any)["value"] = int64(m.(map[string]any)["value"].(float64))
	}
	return out
}

func TestDueExportRunsOncePerUTCDayAndCatchesUpPendingDeltas(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	setClock(t, &now)
	s := openStore(t, dir)
	s.RecordModelCall(context.Background(), modelDims(), testResource())
	if paths := mustDue(t, s); len(paths) != 1 {
		t.Fatal(paths)
	}
	now = time.Date(2026, 7, 28, 18, 0, 0, 0, time.UTC)
	s = openStore(t, dir)
	s.RecordModelCall(context.Background(), modelDims(), testResource())
	if paths := mustDue(t, s); len(paths) != 0 || outboxFiles(t, filepath.Join(dir, "outbox")) != 1 {
		t.Fatal(paths)
	}
	first := snapshot(t, s)[0]
	if first.PeriodStart != "2026-07-28" || first.MetricName != ModelRouteMetric || !jsonEqual(first.Resource, testResource()) ||
		!jsonEqual(first.Dimensions, modelDims()) || first.Value != 2 || first.PackagedValue != 1 {
		t.Fatalf("%+v", first)
	}
	now = time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
	s.RecordModelCall(context.Background(), modelDims(), testResource())
	if paths := mustDue(t, s); len(paths) != 2 || outboxFiles(t, filepath.Join(dir, "outbox")) != 3 || !allPackaged(t, s) {
		t.Fatal(paths)
	}
	s.RecordModelCall(context.Background(), modelDims(), testResource())
	if paths := mustDue(t, s); len(paths) != 0 || outboxFiles(t, filepath.Join(dir, "outbox")) != 3 {
		t.Fatal(paths)
	}
}

func activeRecorded(t *testing.T, s *SharedMetricsStore) bool {
	t.Helper()
	ok, err := s.RecordClientActive(context.Background(), testResource())
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestClientActiveLatch(t *testing.T) {
	t.Run("uses_a_transactional_rolling_24_hour_latch", func(t *testing.T) {
		s := openStore(t, t.TempDir())
		now := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
		setClock(t, &now)
		if !activeRecorded(t, s) || activeRecorded(t, s) {
			t.Fatal("latch did not hold")
		}
		now = now.Add(23*time.Hour + 59*time.Minute + 59*time.Second)
		if activeRecorded(t, s) {
			t.Fatal("recorded inside the window")
		}
		now = now.Add(time.Second)
		if !activeRecorded(t, s) {
			t.Fatal("not recorded after the window")
		}
		var periods []string
		for _, r := range snapshot(t, s) {
			if r.MetricName == ClientActiveMetric {
				if !jsonEqual(r.Dimensions, map[string]any{}) || r.Value != 1 {
					t.Fatalf("%+v", r)
				}
				periods = append(periods, r.PeriodStart)
			}
		}
		if strings.Join(periods, ",") != "2026-07-22,2026-07-23" {
			t.Fatal(periods)
		}
	})
	t.Run("recovers_from_an_invalid_latch_and_creates_identity", func(t *testing.T) {
		s := openStore(t, t.TempDir())
		storeExec(t, s, "INSERT INTO telemetry_state(key, value) VALUES (?, ?)", "client_active_recorded_at", "invalid-timestamp")
		now := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
		setClock(t, &now)
		if !activeRecorded(t, s) {
			t.Fatal("not recorded")
		}
		if _, err := uuid.Parse(storeScalar[string](t, s, "SELECT value FROM telemetry_state WHERE key = 'install_id'")); err != nil {
			t.Fatal(err)
		}
		if got := storeScalar[string](t, s, "SELECT value FROM telemetry_state WHERE key = 'client_active_recorded_at'"); got != "2026-07-22T10:00:00Z" {
			t.Fatal(got)
		}
	})
	t.Run("rebases_a_future_latch_without_double_counting", func(t *testing.T) {
		s := openStore(t, t.TempDir())
		now := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
		setClock(t, &now)
		if !activeRecorded(t, s) {
			t.Fatal("not recorded")
		}
		storeExec(t, s, "UPDATE telemetry_state SET value = ? WHERE key = ?", "2026-07-24T10:00:00Z", "client_active_recorded_at")
		if activeRecorded(t, s) {
			t.Fatal("future latch double counted")
		}
		if got := storeScalar[string](t, s, "SELECT value FROM telemetry_state WHERE key = 'client_active_recorded_at'"); got != "2026-07-22T10:00:00Z" {
			t.Fatal(got)
		}
		rows := snapshot(t, s)
		if len(rows) != 1 || rows[0].MetricName != ClientActiveMetric || rows[0].Value != 1 {
			t.Fatalf("%+v", rows)
		}
	})
	t.Run("package_uses_empty_dimensions_and_stable_install_id", func(t *testing.T) {
		s := openStore(t, t.TempDir())
		activeRecorded(t, s)
		paths := mustExport(t, s)
		pkg := readPackage(t, paths[0])
		validatePackage(t, 4, pkg)
		if _, err := uuid.Parse(pkg["install_id"].(string)); err != nil {
			t.Fatal(err)
		}
		want := []any{map[string]any{"name": ClientActiveMetric, "type": "counter", "dimensions": map[string]any{}, "value": 1}}
		if !jsonEqual(pkg["metrics"], want) {
			t.Fatal(pkg["metrics"])
		}
	})
	t.Run("deleting_local_metrics_state_resets_install_identity", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "shared-metrics")
		first := openStore(t, root)
		activeRecorded(t, first)
		firstPkg := readPackage(t, mustExport(t, first)[0])
		os.RemoveAll(root)
		reset := openStore(t, root)
		activeRecorded(t, reset)
		resetPkg := readPackage(t, mustExport(t, reset)[0])
		if resetPkg["install_id"] == firstPkg["install_id"] || resetPkg["metrics"].([]any)[0].(map[string]any)["name"] != ClientActiveMetric {
			t.Fatal(resetPkg)
		}
	})
}

func storeErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want %q, got %v", want, err)
	}
}

func TestStoreSchemaVersions(t *testing.T) {
	t.Run("rejects_an_unsupported_schema_version", func(t *testing.T) {
		dir := t.TempDir()
		raw := &SharedMetricsStore{DatabasePath: filepath.Join(dir, "metrics.sqlite3")}
		storeExec(t, raw, "CREATE TABLE telemetry_state (key TEXT PRIMARY KEY, value TEXT NOT NULL)")
		storeExec(t, raw, "INSERT INTO telemetry_state(key, value) VALUES ('schema_version', '999')")
		_, err := NewSharedMetricsStore(context.Background(), raw.DatabasePath, filepath.Join(dir, "outbox"))
		storeErr(t, err, "Unsupported shared-metrics store schema")
		if v := storeScalar[string](t, raw, "SELECT value FROM telemetry_state WHERE key = 'schema_version'"); v != "999" {
			t.Fatal(v)
		}
	})
	t.Run("migrates_v1_counters_with_unknown_client_dimensions", func(t *testing.T) {
		dir := t.TempDir()
		raw := &SharedMetricsStore{DatabasePath: filepath.Join(dir, "metrics.sqlite3")}
		id := uuid.NewString()
		storeExec(t, raw, "CREATE TABLE telemetry_state (key TEXT PRIMARY KEY, value TEXT NOT NULL)")
		storeExec(t, raw, "INSERT INTO telemetry_state(key, value) VALUES ('schema_version', '1'), ('install_id', ?)", id)
		storeExec(t, raw, `CREATE TABLE counter_aggregates (period_start TEXT NOT NULL, metric_name TEXT NOT NULL,
			icebath_version TEXT NOT NULL, dimensions_json TEXT NOT NULL, value INTEGER NOT NULL,
			packaged_value INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (period_start, metric_name, icebath_version, dimensions_json))`)
		storeExec(t, raw, `INSERT INTO counter_aggregates(period_start, metric_name, icebath_version, dimensions_json, value,
			packaged_value) VALUES (?, ?, ?, ?, 3, 1)`, "2026-07-21", LegacyModelCallMetric, "old-version",
			compactJSON(anyDims(legacyModelDims())))
		s := openStore(t, dir)
		rows := snapshot(t, s)
		if len(rows) != 1 || !jsonEqual(rows[0].Resource, resourceOf("old-version", "unknown", "unknown", "unknown")) ||
			rows[0].Value != 3 || rows[0].PackagedValue != 1 {
			t.Fatalf("%+v", rows)
		}
		pkg := readPackage(t, mustExport(t, s)[0])
		validatePackage(t, 4, pkg)
		if pkg["install_id"] != id || pkg["metrics"].([]any)[0].(map[string]any)["value"] != float64(2) {
			t.Fatal(pkg)
		}
	})
}

func TestStorePackaging(t *testing.T) {
	ctx := context.Background()
	t.Run("pending_metrics_keep_the_client_resource_recorded_at_event_time", func(t *testing.T) {
		s := openStore(t, t.TempDir())
		a := resourceOf("version-a", "linux", "arm64", "pip")
		b := resourceOf("version-a", "macos", "x86_64", "git")
		s.RecordModelCall(ctx, modelDims(), a)
		s.RecordModelCall(ctx, modelDims(), b)
		paths := mustExport(t, s)
		seen := map[string]bool{}
		for _, p := range paths {
			pkg := readPackage(t, p)
			text, _ := json.Marshal(pkg["resource"])
			seen[string(text)] = true
			if pkg["metrics"].([]any)[0].(map[string]any)["value"] != float64(1) {
				t.Fatal(pkg)
			}
		}
		ta, _ := json.Marshal(a)
		tb, _ := json.Marshal(b)
		if len(seen) != 2 || !seen[string(ta)] || !seen[string(tb)] {
			t.Fatal(seen)
		}
	})
	t.Run("exports_task_started_and_terminal_counters", func(t *testing.T) {
		s := openStore(t, t.TempDir())
		mustRecord(t, s, "icebath.task_run.started", map[string]string{"entrypoint": "interactive", "execution_surface": "cli"}, testResource())
		terminal := TaskTerminalFields(ctx, map[string]any{"platform": "cli", "completed": true, "turn_exit_reason": "text_response(stop)"},
			2000, 1, 2, 0)
		data := map[string]any{}
		for k, v := range terminal {
			data[k] = v
		}
		end := &RelayEvent{Kind: "scope", Category: "function", Name: "icebath.task_run", ScopeCategory: "end",
			Metadata: map[string]any{"icebath.metrics.schema_version": "icebath.metrics.event.v4"}, Data: data}
		for _, build := range []func(*RelayEvent) (Counter, bool){TaskCounter, TaskDurationCounter} {
			c, ok := build(end)
			if !ok {
				t.Fatal("no counter")
			}
			mustRecord(t, s, c.Metric, c.Dimensions, testResource())
		}
		pkg := readPackage(t, mustExport(t, s)[0])
		validatePackage(t, 4, pkg)
		if names := metricNames(pkg); names != "icebath.task_run.duration,icebath.task_run.finished,icebath.task_run.started" {
			t.Fatal(names)
		}
	})
	t.Run("v2_task_and_tool_rows_recorded_before_an_upgrade_still_package", func(t *testing.T) {
		s := openStore(t, t.TempDir())
		mustRecord(t, s, "icebath.task_run.finished", map[string]string{"duration_bucket": "1s_to_5s", "end_reason": "completed",
			"entrypoint": "interactive", "execution_surface": "cli", "model_call_count_bucket": "1", "outcome": "success",
			"retry_count_bucket": "0", "termination": "none", "tool_call_count_bucket": "2"}, testResource())
		mustRecord(t, s, "icebath.tool_call.count", map[string]string{"approval_outcome": "not_required", "latency_bucket": "lt_100ms",
			"outcome": "success", "retry_count_bucket": "0", "tool_category": "file"}, testResource())
		pkg := readPackage(t, mustExport(t, s)[0])
		validatePackage(t, 4, pkg)
		if names := metricNames(pkg); names != "icebath.task_run.finished,icebath.tool_call.count" {
			t.Fatal(names)
		}
	})
	t.Run("failure_class_rows_validate_and_v3_rows_still_drain", func(t *testing.T) {
		s := openStore(t, t.TempDir())
		mustRecord(t, s, "icebath.compression.count", map[string]string{"context_fill_bucket": "lt_50", "outcome": "failed", "trigger": "auto"}, testResource())
		mustRecord(t, s, "icebath.memory.op.count", map[string]string{"op": "add", "origin": "foreground", "outcome": "failed", "provider": "builtin"}, testResource())
		want := map[any]string{"exception:AcmeCorpSecretError": "exception", "rollback:KeyboardInterrupt": "rollback",
			"acme_engine_reason": "other", nil: "unknown"}
		for raw, class := range want {
			fields := CompressionFields("auto", "failed", 1, 10, raw)
			if fields["failure_class"] != class {
				t.Fatal(raw, fields)
			}
			mustRecord(t, s, "icebath.compression.count", fields, testResource())
		}
		marks := captureMarks(t)
		owner := new(int)
		BeginCompressionAttempt(owner, "auto", 1)
		FinishCompressionAttempt(ctx, owner, "aborted", "  Lock_Contended ", 10, nil)
		rows := marks.snapshot()
		if len(rows) != 1 || rows[0].Data["outcome"] != "skipped" || rows[0].Data["failure_class"] != "lock_contended" {
			t.Fatalf("%+v", rows)
		}
		path := mustExport(t, s)[0]
		pkg := readPackage(t, path)
		validatePackage(t, 4, pkg)
		text, _ := os.ReadFile(path)
		without := 0
		for _, m := range pkg["metrics"].([]any) {
			if _, ok := m.(map[string]any)["dimensions"].(map[string]any)["failure_class"]; !ok {
				without++
			}
		}
		if strings.Contains(string(text), "Acme") || without != 2 {
			t.Fatal(string(text))
		}
	})
	t.Run("sent_package_keeps_its_file_but_not_a_second_copy_in_the_database", func(t *testing.T) {
		s := openStore(t, t.TempDir())
		s.RecordModelCall(ctx, modelDims(), testResource())
		sent := mustExport(t, s)[0]
		storeExec(t, s, "UPDATE package_outbox SET send_state = 'sent'")
		s.RecordModelCall(ctx, modelDims(), testResource())
		pending := mustExport(t, s)[0]
		stem := func(p string) string { return strings.TrimSuffix(filepath.Base(p), ".json") }
		if body := storeScalar[string](t, s, "SELECT payload_json FROM package_outbox WHERE package_id = ?", stem(sent)); body != "" {
			t.Fatal(body)
		}
		var stored map[string]any
		json.Unmarshal([]byte(storeScalar[string](t, s, "SELECT payload_json FROM package_outbox WHERE package_id = ?", stem(pending))), &stored)
		if !jsonEqual(stored, readPackage(t, pending)) {
			t.Fatal(stored)
		}
		validatePackage(t, 4, readPackage(t, sent))
	})
	t.Run("package_schema_rejects_unknown_fields", func(t *testing.T) {
		s := openStore(t, t.TempDir())
		s.RecordModelCall(ctx, modelDims(), testResource())
		pkg := readPackage(t, mustExport(t, s)[0])
		pkg["prompt"] = "must-not-be-accepted"
		if err := schemaValidator(t, 4).Validate(pkg); err == nil {
			t.Fatal("schema accepted an unknown field")
		}
	})
}

func metricNames(pkg map[string]any) string {
	var names []string
	for _, m := range pkg["metrics"].([]any) {
		names = append(names, m.(map[string]any)["name"].(string))
	}
	sortStrings(names)
	return strings.Join(names, ",")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func TestStoreRejectsOffContractRows(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	storeErr(t, s.RecordCounter(ctx, LegacyModelCallMetric, legacyModelDims(), testResource(), 1), "Unsupported shared metric")
	storeErr(t, s.RecordCounter(ctx, ModelRouteMetric, map[string]string{"prompt": "must-not-be-persisted"}, testResource(), 1),
		"Unsupported dimensions")
	bad := testResource()
	bad["architecture"] = "privacy-architecture-canary"
	storeErr(t, s.RecordModelCall(ctx, modelDims(), bad), "Unsupported shared-metrics client resource")
	if rows := snapshot(t, s); len(rows) != 0 {
		t.Fatalf("%+v", rows)
	}
	for _, tamper := range []struct{ query, arg, want string }{
		{"UPDATE counter_aggregates SET dimensions_json = ?", `{"prompt": "must-not-be-exported"}`, "Unsupported dimensions"},
		{"UPDATE counter_aggregates SET os_family = ?", "privacy-os-canary", "Unsupported shared-metrics client resource"},
	} {
		dir := t.TempDir()
		s := openStore(t, dir)
		s.RecordModelCall(ctx, modelDims(), testResource())
		storeExec(t, s, tamper.query, tamper.arg)
		_, err := s.CreateAndExportPackage(ctx)
		storeErr(t, err, tamper.want)
		if outboxFiles(t, filepath.Join(dir, "outbox")) != 0 {
			t.Fatal("tampered row exported")
		}
	}
}

func TestSavedRowNamingARetiredToolOrToolsetStillPackages(t *testing.T) {
	cases := []struct {
		metric string
		dims   map[string]string
	}{
		{"icebath.tool_enabled_unused.count", map[string]string{"toolset": "start_chat", "used": "yes"}},
		{"icebath.tool.usage.count", map[string]string{"error_class": "none", "outcome": "success", "tool_name": "setup_choose"}},
		{"icebath.tool_enabled_unused.count", map[string]string{"toolset": "spotify", "used": "no"}},
		{"icebath.tool.usage.count", map[string]string{"error_class": "none", "outcome": "success", "tool_name": "spotify_search"}},
		{"icebath.tool_unavailable.count", map[string]string{"model": "custom", "provider": "custom", "tool_name": "spotify_playback"}},
		{"icebath.tool_unavailable.count", map[string]string{"model": "custom", "provider": "custom", "tool_name": "browser_vault_fill"}},
	}
	for _, c := range cases {
		s := openStore(t, t.TempDir())
		s.RecordModelCall(context.Background(), modelDims(), testResource())
		storeExec(t, s, "UPDATE counter_aggregates SET metric_name = ?, dimensions_json = ?", c.metric, compactJSON(anyDims(c.dims)))
		pkg := readPackage(t, mustExport(t, s)[0])
		metrics := pkg["metrics"].([]any)
		if len(metrics) != 1 || metrics[0].(map[string]any)["name"] != c.metric || !jsonEqual(metrics[0].(map[string]any)["dimensions"], c.dims) {
			t.Fatal(c.metric, metrics)
		}
		validatePackage(t, 4, pkg)
	}
}

func TestPackageBuilderRebucketsDrift(t *testing.T) {
	ctx := context.Background()
	t.Run("buckets_values_that_drifted_out_of_the_catalog", func(t *testing.T) {
		s := openStore(t, t.TempDir())
		setCatalog(t, "plugin_catalog_names", "later-removed-plugin")
		mustRecord(t, s, ExtensionInstallMetric, map[string]string{"kind": "plugin", "name": "later-removed-plugin", "outcome": "success",
			"source": "catalog"}, testResource())
		s.RecordModelCall(ctx, modelDims(), testResource())
		setCatalog(t, "plugin_catalog_names")
		pkg := readPackage(t, mustExport(t, s)[0])
		byName := map[string]any{}
		for _, m := range pkg["metrics"].([]any) {
			byName[m.(map[string]any)["name"].(string)] = m.(map[string]any)["dimensions"]
		}
		if !jsonEqual(byName[ExtensionInstallMetric], map[string]string{"kind": "plugin", "name": "custom", "outcome": "success", "source": "catalog"}) ||
			!jsonEqual(byName[ModelRouteMetric], modelDims()) {
			t.Fatal(byName)
		}
		if p, err := s.createPackage(ctx); err != nil || p != nil {
			t.Fatal(p, err)
		}
	})
	t.Run("settles_rows_an_earlier_build_stored_with_retired_values", func(t *testing.T) {
		s := openStore(t, t.TempDir())
		s.RecordModelCall(ctx, modelDims(), testResource())
		for _, toolset := range []string{"file", "terminal"} {
			mustRecord(t, s, ToolEnabledUnusedMetric, map[string]string{"toolset": toolset, "used": "yes"}, testResource())
		}
		unavailable := modelDims()
		unavailable["tool_name"] = "read_file"
		mustRecord(t, s, ToolUnavailableMetric, unavailable, testResource())
		storeExec(t, s, "UPDATE counter_aggregates SET dimensions_json = ? WHERE dimensions_json = ?",
			`{"toolset":"retired-toolset-a","used":"yes"}`, `{"toolset":"file","used":"yes"}`)
		storeExec(t, s, "UPDATE counter_aggregates SET dimensions_json = ? WHERE dimensions_json = ?",
			`{"toolset":"retired-toolset-b","used":"yes"}`, `{"toolset":"terminal","used":"yes"}`)
		storeExec(t, s, "UPDATE counter_aggregates SET dimensions_json = ? WHERE metric_name = ?",
			`{"model": "anthropic/claude-sonnet-4.6", "provider": "openrouter", "tool_name": "retired_tool"}`, ToolUnavailableMetric)
		path := mustExport(t, s)[0]
		text, _ := os.ReadFile(path)
		if strings.Contains(string(text), "retired") {
			t.Fatal(string(text))
		}
		found := map[string]bool{}
		for _, m := range readPackage(t, path)["metrics"].([]any) {
			mm := m.(map[string]any)
			switch {
			case mm["name"] == ToolEnabledUnusedMetric && jsonEqual(mm["dimensions"], map[string]string{"toolset": "custom", "used": "yes"}) && mm["value"] == float64(2):
				found["merged"] = true
			case mm["name"] == ToolUnavailableMetric:
				t.Fatal("retired tool row exported")
			case mm["name"] == ModelRouteMetric && jsonEqual(mm["dimensions"], modelDims()) && mm["value"] == float64(1):
				found["route"] = true
			}
		}
		if !found["merged"] || !found["route"] {
			t.Fatal(found)
		}
		if p, err := s.createPackage(ctx); err != nil || p != nil {
			t.Fatal(p, err)
		}
	})
}

// Installs a catalog source for one test, restoring the previous one after
func setCatalog(t *testing.T, name string, values ...string) {
	t.Helper()
	catalogMu.Lock()
	previous, had := catalogSources[name]
	catalogMu.Unlock()
	SetCatalogSource(name, func() ([]string, error) { return values, nil })
	ResetCatalogCaches()
	t.Cleanup(func() {
		catalogMu.Lock()
		if had {
			catalogSources[name] = previous
		} else {
			delete(catalogSources, name)
		}
		catalogMu.Unlock()
		ResetCatalogCaches()
	})
}

func TestStoreRetentionAndExportFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("retention_prunes_only_expired_exported_history", func(t *testing.T) {
		dir := t.TempDir()
		s := openStore(t, dir)
		s.RecordModelCall(ctx, modelDims(), resourceOf("expired-version", "linux", "x86_64", "git"))
		expired := mustExport(t, s)[0]
		s.RecordModelCall(ctx, modelDims(), resourceOf("current-version", "linux", "x86_64", "git"))
		current := mustExport(t, s)[0]
		s.RecordModelCall(ctx, modelDims(), resourceOf("pending-version", "linux", "x86_64", "git"))
		pending, err := s.createPackage(ctx)
		if err != nil || pending == nil {
			t.Fatal(err)
		}
		storeExec(t, s, "UPDATE counter_aggregates SET period_start = '2026-05-01' WHERE icebath_version = 'expired-version'")
		storeExec(t, s, `UPDATE package_outbox SET period_start = '2026-05-01T00:00:00Z', period_end = '2026-05-02T00:00:00Z',
			exported_at = '2026-05-02T00:00:00Z' WHERE package_id = ?`, strings.TrimSuffix(filepath.Base(expired), ".json"))
		if err := s.pruneExpiredHistory(ctx, time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)); err != nil {
			t.Fatal(err)
		}
		if exists(expired) || !exists(current) || exists(filepath.Join(dir, "outbox", pending["package_id"].(string)+".json")) {
			t.Fatal("wrong files pruned")
		}
		ids := strings.Join(storeColumn(t, s, "SELECT package_id FROM package_outbox ORDER BY package_id"), ",")
		want := []string{strings.TrimSuffix(filepath.Base(current), ".json"), pending["package_id"].(string)}
		sortStrings(want)
		if ids != strings.Join(want, ",") {
			t.Fatal(ids)
		}
		if n := storeScalar[int](t, s, "SELECT COUNT(*) FROM package_outbox WHERE package_id = ? AND exported_at IS NULL", pending["package_id"]); n != 1 {
			t.Fatal(n)
		}
		versions := storeColumn(t, s, "SELECT DISTINCT icebath_version FROM counter_aggregates ORDER BY icebath_version")
		if strings.Join(versions, ",") != "current-version,pending-version" {
			t.Fatal(versions)
		}
	})
	t.Run("retention_failure_does_not_fail_a_committed_export", func(t *testing.T) {
		s := openStore(t, t.TempDir())
		s.RecordModelCall(ctx, modelDims(), testResource())
		saved := pruneExpiredHistoryFn
		pruneExpiredHistoryFn = func(*SharedMetricsStore, context.Context, time.Time) error {
			return &os.PathError{Op: "prune", Path: "outbox", Err: os.ErrPermission}
		}
		defer func() { pruneExpiredHistoryFn = saved }()
		paths := mustExport(t, s)
		if len(paths) != 1 || !exists(paths[0]) || snapshot(t, s)[0].PackagedValue != 1 {
			t.Fatal(paths)
		}
	})
	t.Run("file_export_failure_retries_committed_outbox_without_duplicate_delta", func(t *testing.T) {
		dir := t.TempDir()
		s := openStore(t, dir)
		s.RecordModelCall(ctx, modelDims(), testResource())
		saved := writePackageFile
		writePackageFile = func(context.Context, string, any) error { return errors.New("simulated atomic export failure") }
		_, err := s.CreateAndExportPackage(ctx)
		writePackageFile = saved
		storeErr(t, err, "simulated atomic export failure")
		id := storeScalar[string](t, s, "SELECT package_id FROM package_outbox")
		if n := storeScalar[int](t, s, "SELECT COUNT(*) FROM package_outbox WHERE exported_at IS NULL"); n != 1 ||
			snapshot(t, s)[0].PackagedValue != 1 || outboxFiles(t, filepath.Join(dir, "outbox")) != 0 {
			t.Fatal("export failure left a file or lost the package")
		}
		if paths := mustExport(t, s); len(paths) != 1 || paths[0] != filepath.Join(dir, "outbox", id+".json") {
			t.Fatal(paths)
		}
		if outboxFiles(t, filepath.Join(dir, "outbox")) != 1 || len(mustExport(t, s)) != 0 {
			t.Fatal("duplicate export")
		}
	})
	t.Run("package_export_does_not_chase_concurrent_updates", func(t *testing.T) {
		s := openStore(t, t.TempDir())
		s.RecordModelCall(ctx, modelDims(), testResource())
		saved := createPackageFn
		calls := 0
		createPackageFn = func(store *SharedMetricsStore, ctx context.Context) (map[string]any, error) {
			calls++
			p, err := saved(store, ctx)
			if calls == 1 {
				store.RecordModelCall(ctx, modelDims(), testResource())
			}
			return p, err
		}
		paths := mustExport(t, s)
		createPackageFn = saved
		rows := snapshot(t, s)
		if calls != 1 || len(paths) != 1 || len(rows) != 1 || rows[0].Value != 2 || rows[0].PackagedValue != 1 {
			t.Fatalf("%d %v %+v", calls, paths, rows)
		}
		if paths := mustExport(t, s); len(paths) != 1 || snapshot(t, s)[0].PackagedValue != 2 {
			t.Fatal(paths)
		}
	})
}

func storeColumn(t *testing.T, s *SharedMetricsStore, query string) []string {
	t.Helper()
	var out []string
	err := s.connection(context.Background(), busyTimeoutMS, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(context.Background(), query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v string
			rows.Scan(&v)
			out = append(out, v)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Runs fn in n goroutines released together, retrying once on the store when a run reports lock contention
func concurrently(t *testing.T, n int, fn func() error, retry func()) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = fn()
		}(i)
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		if err == nil {
			continue
		}
		if !isContention(err) {
			t.Fatal(err)
		}
		retry()
	}
}

func TestStoreConcurrency(t *testing.T) {
	ctx := context.Background()
	t.Run("concurrent_package_builders_commit_one_delta", func(t *testing.T) {
		dir := t.TempDir()
		s := openStore(t, dir)
		s.RecordModelCall(ctx, modelDims(), testResource())
		concurrently(t, 2, func() error {
			_, err := openStore(t, dir).CreateAndExportPackage(ctx)
			return err
		}, func() { mustExport(t, s) })
		pkgs, _ := filepath.Glob(filepath.Join(dir, "outbox", "*.json"))
		if n := storeScalar[int](t, s, "SELECT COUNT(*) FROM package_outbox"); n != 1 || len(pkgs) != 1 ||
			readPackage(t, pkgs[0])["metrics"].([]any)[0].(map[string]any)["value"] != float64(1) || snapshot(t, s)[0].PackagedValue != 1 {
			t.Fatal(n, pkgs)
		}
	})
	t.Run("concurrent_due_exports_create_one_daily_package", func(t *testing.T) {
		dir := t.TempDir()
		s := openStore(t, dir)
		s.RecordModelCall(ctx, modelDims(), testResource())
		concurrently(t, 8, func() error {
			_, err := openStore(t, dir).CreateAndExportPackageIfDue(ctx)
			return err
		}, func() { mustDue(t, s) })
		if n := storeScalar[int](t, s, "SELECT COUNT(*) FROM package_outbox"); n != 1 || outboxFiles(t, filepath.Join(dir, "outbox")) != 1 ||
			snapshot(t, s)[0].PackagedValue != 1 {
			t.Fatal(n)
		}
	})
	t.Run("concurrent_model_call_updates_are_transactional", func(t *testing.T) {
		dir := t.TempDir()
		openStore(t, dir)
		var busy sync.Map
		concurrently(t, 2, func() error {
			s := openStore(t, dir)
			for i := 0; i < 10; i++ {
				if err := s.RecordModelCall(ctx, modelDims(), testResource()); err != nil {
					if !isContention(err) {
						return err
					}
					busy.Store(new(int), true)
				}
			}
			s.DrainDeferred(ctx, schemaBusyTimeoutMS)
			return nil
		}, func() {})
		restarted := openStore(t, dir)
		busy.Range(func(any, any) bool {
			restarted.RecordModelCall(ctx, modelDims(), testResource())
			return true
		})
		if v := snapshot(t, restarted)[0].Value; v != 20 {
			t.Fatal(v)
		}
	})
	t.Run("a_write_the_busy_store_cannot_take_is_deferred_not_lost", func(t *testing.T) {
		s := openStore(t, t.TempDir())
		blocker, err := state.ConnectSQLite(s.DatabasePath, state.SQLiteConfig{})
		if err != nil {
			t.Fatal(err)
		}
		blocker.ExecContext(ctx, "BEGIN IMMEDIATE")
		if err := s.RecordModelCall(ctx, modelDims(), testResource()); err != nil {
			t.Fatal(err)
		}
		blocker.ExecContext(ctx, "ROLLBACK")
		blocker.Close()
		if rows := snapshot(t, s); len(rows) != 0 {
			t.Fatalf("%+v", rows)
		}
		s.RecordModelCall(ctx, modelDims(), testResource())
		if v := snapshot(t, s)[0].Value; v != 2 {
			t.Fatal(v)
		}
	})
	t.Run("schema_initialization_waits_for_an_existing_writer", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "metrics.sqlite3")
		os.WriteFile(path, nil, 0o600)
		blocker, err := state.ConnectSQLite(path, state.SQLiteConfig{})
		if err != nil {
			t.Fatal(err)
		}
		blocker.ExecContext(ctx, "BEGIN IMMEDIATE")
		done := make(chan *SharedMetricsStore, 1)
		go func() {
			s, err := NewSharedMetricsStore(ctx, path, filepath.Join(dir, "outbox"))
			if err != nil {
				t.Error(err)
			}
			done <- s
		}()
		time.Sleep(400 * time.Millisecond)
		select {
		case <-done:
			t.Fatal("schema initialization did not wait for the writer")
		default:
		}
		blocker.ExecContext(ctx, "ROLLBACK")
		blocker.Close()
		select {
		case s := <-done:
			if s == nil || len(snapshot(t, s)) != 0 {
				t.Fatal("store not opened")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("schema initialization never finished")
		}
	})
	t.Run("store_and_export_are_owner_only", func(t *testing.T) {
		dir := t.TempDir()
		db := filepath.Join(dir, "private-store", "metrics.sqlite3")
		s, err := NewSharedMetricsStore(ctx, db, filepath.Join(dir, "private-outbox"))
		if err != nil {
			t.Fatal(err)
		}
		s.RecordModelCall(ctx, modelDims(), testResource())
		pkg := mustExport(t, s)[0]
		for path, mode := range map[string]os.FileMode{filepath.Dir(db): 0o700, s.OutboxDirectory: 0o700, db: 0o600, pkg: 0o600} {
			if info, err := os.Stat(path); err != nil || info.Mode().Perm() != mode {
				t.Fatal(path, err)
			}
		}
	})
}

// Records from a child copy of the test binary, the cross process half of the transactional tests
func TestStoreHelperProcess(t *testing.T) {
	dir, action := os.Getenv("ICEBATH_STORE_HELPER_DIR"), os.Getenv("ICEBATH_STORE_HELPER_ACTION")
	if dir == "" {
		t.Skip("helper process only")
	}
	s := openStore(t, dir)
	for !exists(filepath.Join(dir, "go")) {
		time.Sleep(5 * time.Millisecond)
	}
	ctx := context.Background()
	if action == "active" {
		if _, err := s.RecordClientActive(ctx, testResource()); err != nil {
			t.Fatal(err)
		}
		return
	}
	for i := 0; i < 10; i++ {
		if err := s.RecordModelCall(ctx, modelDims(), testResource()); err != nil {
			t.Fatal(err)
		}
	}
	drainDeferringStoresAtExit(ctx)
}

func TestStoreCrossProcess(t *testing.T) {
	for _, action := range []string{"calls", "active"} {
		t.Run(action, func(t *testing.T) {
			dir := t.TempDir()
			openStore(t, dir)
			var cmds []*exec.Cmd
			for i := 0; i < 2; i++ {
				cmd := exec.Command(os.Args[0], "-test.run", "^TestStoreHelperProcess$", "-test.count=1")
				cmd.Env = append(os.Environ(), "ICEBATH_STORE_HELPER_DIR="+dir, "ICEBATH_STORE_HELPER_ACTION="+action)
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				cmds = append(cmds, cmd)
			}
			time.Sleep(300 * time.Millisecond)
			os.WriteFile(filepath.Join(dir, "go"), nil, 0o600)
			for _, cmd := range cmds {
				if err := cmd.Wait(); err != nil {
					t.Fatal(err)
				}
			}
			rows := snapshot(t, openStore(t, dir))
			want := int64(20)
			if action == "active" {
				want = 1
				if len(rows) != 1 || rows[0].MetricName != ClientActiveMetric || !jsonEqual(rows[0].Dimensions, map[string]any{}) {
					t.Fatalf("%+v", rows)
				}
			}
			if rows[0].Value != want {
				t.Fatal(rows[0].Value)
			}
		})
	}
}
