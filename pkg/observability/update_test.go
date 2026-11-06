package observability

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// updateMarks captures update rows with a switchable collection policy and store
type updateMarks struct {
	mu    sync.Mutex
	on    bool
	saves bool
	rows  []Mark
	home  string
}

// Captures marks at the relay boundary with the config gate following the policy
func captureUpdates(t *testing.T) *updateMarks {
	t.Helper()
	m := &updateMarks{on: true, saves: true, home: filepath.Join(t.TempDir(), "home")}
	t.Setenv(home.EnvHome, m.home)
	savedEnabled, savedRecord, savedSaved, savedOn := relayEnabled, recordProcessMark, recordProcessMarksSaved, collectionOn
	t.Cleanup(func() {
		relayEnabled, recordProcessMark, recordProcessMarksSaved, collectionOn = savedEnabled, savedRecord, savedSaved, savedOn
	})
	policy := func(context.Context) bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.on
	}
	relayEnabled, collectionOn = policy, policy
	recordProcessMark = func(_ context.Context, mark string, data map[string]string) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.rows = append(m.rows, Mark{mark, data})
	}
	recordProcessMarksSaved = func(_ context.Context, rows []Mark) int {
		m.mu.Lock()
		defer m.mu.Unlock()
		if !m.saves {
			return 0
		}
		m.rows = append(m.rows, rows...)
		return len(rows)
	}
	return m
}

// Rows of one mark name
func (m *updateMarks) named(name string) []map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []map[string]string
	for _, r := range m.rows {
		if r.Name == name {
			out = append(out, r.Data)
		}
	}
	return out
}

// Asserts every captured row is a valid counter of its metric
func (m *updateMarks) assertValid(t *testing.T) {
	t.Helper()
	for _, r := range m.rows {
		if !CounterDimensionsAreValid(r.Name, anyDims(r.Data)) {
			t.Fatalf("invalid row %s %v", r.Name, r.Data)
		}
	}
}

// A receipt decoded from JSON the way the receipt writer leaves it
func receiptJSON(t *testing.T, text string) *pyjson.Object {
	t.Helper()
	v, err := pyjson.Loads(text)
	if err != nil {
		t.Fatal(err)
	}
	return v.(*pyjson.Object)
}

// ISO timestamp of an offset from now in UTC
func isoAgo(d time.Duration) string {
	return time.Now().Add(-d).UTC().Format("2006-01-02T15:04:05.000000+00:00")
}

func TestFailedUpdateReportsTheStageItNeverReached(t *testing.T) {
	m := captureUpdates(t)
	start := isoAgo(10 * time.Second)
	receipt := receiptJSON(t, `{"schema": 1, "update_id": "`+strings.Repeat("ab", 8)+`", "initiator": "cli",
		"started_at": "`+start+`", "finished_at": "`+isoAgo(0)+`", "outcome": "failed", "exit_code": 1,
		"stop_reason": "completion exited 1", "pre_update": {"commit_date": "`+isoAgo(10*24*time.Hour)+`"},
		"stages": [{"name": "plan", "outcome": "success", "at": "`+isoAgo(9*time.Second)+`"},
		{"name": "snapshot", "outcome": "skipped", "at": "`+isoAgo(8*time.Second)+`"},
		{"name": "apply", "outcome": "success", "mode": "git", "at": "`+isoAgo(7*time.Second)+`"}]}`)
	if !RecordUpdateReceipt(context.Background(), receipt, false) {
		t.Fatal("not recorded")
	}
	runs := m.named(UpdateRunMark)
	want := []map[string]string{{"apply_mode": "git", "duration_bucket": "lt_30s", "failed_stage": "deps",
		"from_version_age_bucket": "7d_to_30d", "kind": "cli", "outcome": "failed", "failure_class": "deps_failed"}}
	if !reflect.DeepEqual(runs, want) {
		t.Fatalf("runs %v", runs)
	}
	var stages [][2]string
	for _, s := range m.named(UpdateStageMark) {
		stages = append(stages, [2]string{s["stage"], s["outcome"]})
	}
	if !reflect.DeepEqual(stages, [][2]string{{"plan", "success"}, {"snapshot", "skipped"}, {"apply", "success"}, {"deps", "failed"}}) {
		t.Fatalf("stages %v", stages)
	}
	m.assertValid(t)
	if RecordUpdateReceipt(context.Background(), receipt, false); len(m.named(UpdateRunMark)) != 1 {
		t.Fatal("one update id counted twice")
	}
}

func TestParkedReceiptIsReportedOnceByTheNextStart(t *testing.T) {
	m := captureUpdates(t)
	dir := PendingUpdatesDir(m.home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	parked := filepath.Join(dir, "0123456789abcdef.json")
	body := `{"update_id": "0123456789abcdef", "started_at": "` + isoAgo(time.Minute) + `", "finished_at": "` + isoAgo(0) +
		`", "outcome": "failed", "stages": [{"name": "plan", "outcome": "success", "at": "` + isoAgo(50*time.Second) + `"}]}`
	for name, text := range map[string]string{"0123456789abcdef.json": "\ufeff" + body, ".hidden.json": body, "notes.txt": body} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reportPendingUpdates(context.Background())
	reportPendingUpdates(context.Background())
	runs := m.named(UpdateRunMark)
	if len(runs) != 1 || runs[0]["failure_class"] != "other" || runs[0]["failed_stage"] != "snapshot" {
		t.Fatalf("runs %v", runs)
	}
	if _, err := os.Stat(parked); !os.IsNotExist(err) {
		t.Fatalf("parked receipt kept %v", err)
	}
	for _, name := range []string{".hidden.json", "notes.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s touched %v", name, err)
		}
	}
	m.assertValid(t)
}

func TestAParkedReceiptTheStoreRefusedIsKeptAndItsLatchReleased(t *testing.T) {
	m := captureUpdates(t)
	m.saves = false
	dir := PendingUpdatesDir(m.home)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	parked := filepath.Join(dir, "fedcba9876543210.json")
	body := `{"update_id": "fedcba9876543210", "started_at": 100, "finished_at": 160, "outcome": "success"}`
	if err := os.WriteFile(parked, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	reportPendingUpdates(context.Background())
	if _, err := os.Stat(parked); err != nil {
		t.Fatalf("refused receipt not handed back %v", err)
	}
	latch := filepath.Join(m.home, "telemetry", "shared_metrics", RecordedUpdatesDirname, "fedcba9876543210")
	if _, err := os.Stat(latch); !os.IsNotExist(err) {
		t.Fatalf("latch kept %v", err)
	}
	m.saves = true
	reportPendingUpdates(context.Background())
	if runs := m.named(UpdateRunMark); len(runs) != 1 || runs[0]["outcome"] != "success" || runs[0]["duration_bucket"] != "30s_to_2m" {
		t.Fatalf("runs %v", runs)
	}
}

func TestDisabledCollectionRecordsNoUpdateRows(t *testing.T) {
	m := captureUpdates(t)
	m.on = false
	receipt := receiptJSON(t, `{"started_at": 1, "finished_at": 2, "outcome": "success", "stages": [{"name": "plan", "outcome": "success"}]}`)
	if !RecordUpdateReceipt(context.Background(), receipt, true) {
		t.Fatal("off is nothing to record")
	}
	RecordDesktopUpdate(context.Background(), map[string]any{"outcome": "success", "failed_stage": nil, "duration_ms": 1, "mechanism": "app-installer"})
	if len(m.rows) != 0 {
		t.Fatalf("rows %v", m.rows)
	}
}

func TestDesktopUpdateRowsAndKeywordMismatch(t *testing.T) {
	m := captureUpdates(t)
	ctx := context.Background()
	RecordDesktopUpdate(ctx, map[string]any{"outcome": " FAILED ", "failed_stage": "Download", "duration_ms": 1, "mechanism": "posix-handoff",
		"from_commit_date": float64(time.Now().Add(-48 * time.Hour).Unix())})
	RecordDesktopUpdate(ctx, map[string]any{"outcome": "success", "failed_stage": nil, "duration_ms": 1})
	RecordDesktopUpdate(ctx, map[string]any{"outcome": "success", "failed_stage": nil, "duration_ms": 1, "mechanism": nil, "extra": 1})
	RecordDesktopUpdate(ctx, map[string]any{"outcome": "weird", "failed_stage": "nowhere", "duration_ms": "x", "mechanism": "Microsoft-Store"})
	want := []map[string]string{
		{"apply_mode": "git", "duration_bucket": "lt_30s", "failed_stage": "download", "from_version_age_bucket": "lt_7d",
			"kind": "desktop", "outcome": "failed", "failure_class": "unknown"},
		{"apply_mode": "package", "duration_bucket": "lt_30s", "failed_stage": "other", "from_version_age_bucket": "unknown",
			"kind": "desktop", "outcome": "failed", "failure_class": "unknown"},
	}
	if got := m.named(UpdateRunMark); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows %v", got)
	}
	m.assertValid(t)
}

func TestUnrepresentableTimestampsReadAsUnknown(t *testing.T) {
	huge := new(big.Int).Exp(big.NewInt(10), big.NewInt(400), nil)
	if _, ok := epoch(huge); ok {
		t.Fatal("10**400 read as a time")
	}
	for _, v := range []any{true, nil, "yesterday", []any{}} {
		if _, ok := epoch(v); ok {
			t.Fatalf("%v read as a time", v)
		}
	}
	naive, ok := epoch("2024-01-02T03:04:05")
	if want := time.Date(2024, 1, 2, 3, 4, 5, 0, time.Local); !ok || naive != float64(want.Unix()) {
		t.Fatalf("naive %v %v", naive, ok)
	}
	if v, ok := epoch("2024-01-02T03:04:05.5+01:00"); !ok || v != float64(time.Date(2024, 1, 2, 2, 4, 5, 0, time.UTC).Unix())+0.5 {
		t.Fatalf("aware %v", v)
	}
	if elapsedMs(10, 5) != nil || elapsedMs(5, 10) != float64(5000) {
		t.Fatal("elapsed")
	}
}

func TestUpdateFailureClassReadsOnlyTheFinalReceipt(t *testing.T) {
	base := `"schema": 1, "started_at": 100, "finished_at": 200`
	for _, c := range []struct{ body, run, failed string }{
		{`"outcome": "failed", "steps": [{"name": "admission", "ok": false}]`, "managed_install", "other"},
		{`"outcome": "failed", "steps": [{"name": "admission", "ok": false}], "stop_reason": "x"`, "managed_install", ""},
		{`"outcome": "failed", "exit_code": 130`, "interrupted", ""},
		{`"outcome": "failed", "stop_reason": "KeyboardInterrupt:\n"`, "interrupted", ""},
		{`"outcome": "refused", "exit_code": 2`, "lock_held", ""},
		{`"outcome": "refused", "exit_code": 3`, "other", ""},
		{`"outcome": "failed", "stop_class": "branch_missing"`, "branch_missing", ""},
		{`"outcome": "failed", "stop_class": "branch_missing", "stages": [{"name": "apply", "outcome": "success"}, {"name": "deps", "outcome": "success"}, {"name": "build", "outcome": "failed"}]`, "build_failed", "build"},
		{`"outcome": "failed", "fleet": [{"state": "down"}], "stages": [{"name": "apply"}]`, "fleet_stale", "verify"},
		{`"outcome": "failed", "gateway_restart": {"incomplete": true}`, "restart_failed", ""},
		{`"outcome": "failed", "gateway_restart": {"incomplete": 1}`, "git_failed", ""},
		{`"outcome": "failed", "runtime_outcomes": [{"outcome": "failed"}]`, "restart_failed", ""},
		{`"outcome": "failed", "stages": [{"name": "restart", "outcome": "skipped"}]`, "restart_failed", "restart"},
		{`"outcome": "failed", "stop_reason": "Windows gateway recovery failed: x"`, "restart_failed", ""},
		{`"outcome": "failed", "stop_reason": "OSError: [Errno 28] No space left"`, "disk_full", ""},
		{`"outcome": "failed", "stop_reason": "PermissionError: [Errno 13] denied"`, "permission_denied", ""},
		{`"outcome": "failed", "stop_reason": "HashError: bad"`, "deps_failed", ""},
		{`"outcome": "failed", "stop_reason": "FileNotFoundError: gone"`, "os_error", ""},
		{`"outcome": "failed", "stop_reason": "TimeoutExpired: slow"`, "subprocess_failed", ""},
		{`"outcome": "failed", "stop_reason": "ValueError: no"`, "exception", ""},
		{`"outcome": "failed", "stages": [{"name": "apply"}, {"name": "deps"}]`, "build_failed", ""},
		{`"outcome": "failed", "stages": [{"name": "apply"}, {"name": "deps"}, {"name": "build"}]`, "other", ""},
		{`"outcome": "failed", "stop_reason": "sys.exit(1)"`, "aborted_before_apply", ""},
		{`"outcome": "partial", "user_action": "stash", "stages": [{"name": "apply", "outcome": "success"}]`, "local_changes_parked", "apply"},
		{`"outcome": "partial", "fleet": [{"state": "current"}], "stages": [{"name": "apply", "outcome": "success"}]`, "fleet_unverified", "verify"},
		{`"outcome": "success", "stages": [{"name": "apply", "outcome": "skipped"}]`, "none", "none"},
	} {
		fields, err := UpdateReceiptFields(receiptJSON(t, "{"+base+", "+c.body+"}"))
		if err != nil || fields == nil {
			t.Fatalf("%s: %v", c.body, err)
		}
		if fields.Run["failure_class"] != c.run || (c.failed != "" && fields.Run["failed_stage"] != c.failed) {
			t.Fatalf("%s: run %v", c.body, fields.Run)
		}
		if !CounterDimensionsAreValid(UpdateRunMetric, anyDims(fields.Run)) {
			t.Fatalf("%s: invalid %v", c.body, fields.Run)
		}
	}
	noSchema, _ := UpdateReceiptFields(receiptJSON(t, `{"started_at": 1, "finished_at": 2, "outcome": "failed"}`))
	if noSchema.Run["failure_class"] != "other" {
		t.Fatalf("parked copy %v", noSchema.Run)
	}
	if f, err := UpdateReceiptFields(receiptJSON(t, `{"outcome": "failed"}`)); f != nil || err != nil {
		t.Fatal("unfinished receipt derived")
	}
	if _, err := UpdateReceiptFields(receiptJSON(t, `{"finished_at": 1, "stages": 5}`)); err == nil {
		t.Fatal("non iterable stages accepted")
	}
	if _, err := UpdateReceiptFields(receiptJSON(t, `{"finished_at": 1, "stages": [{"name": ["apply"]}]}`)); err == nil {
		t.Fatal("unhashable stage name accepted")
	}
	captureUpdates(t)
	if RecordUpdateReceipt(context.Background(), receiptJSON(t, `{"finished_at": 1, "pre_update": "x"}`), true) {
		t.Fatal("malformed receipt reported settled")
	}
}

func TestUpdateLatchesKeepOnlyTheNewest(t *testing.T) {
	m := captureUpdates(t)
	ctx := context.Background()
	dir := filepath.Join(m.home, "telemetry", "shared_metrics", RecordedUpdatesDirname)
	for i := 0; i < recordedUpdatesKeep+3; i++ {
		id := strings.Repeat("0", 6) + string("0123456789abcdef"[i%16]) + string("0123456789abcdef"[i/16])
		if first, _ := claimUpdateID(ctx, id); !first {
			t.Fatalf("%s already claimed", id)
		}
		past := time.Now().Add(time.Duration(i-100) * time.Minute)
		_ = os.Chtimes(filepath.Join(dir, id), past, past)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != recordedUpdatesKeep {
		t.Fatalf("kept %d", len(entries))
	}
	if _, err := os.Stat(filepath.Join(dir, "00000000")); !os.IsNotExist(err) {
		t.Fatal("oldest latch kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "00000024")); err != nil {
		t.Fatalf("newest latch pruned %v", err)
	}
	if first, latch := claimUpdateID(ctx, "XYZ"); !first || latch != "" {
		t.Fatal("unstable id deduped")
	}
}
