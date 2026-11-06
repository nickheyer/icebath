package observability

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

// Collection policy config plus the capture with the real receipt reporter and purger restored
func installMarks(t *testing.T, sharedMetrics string) *markCapture {
	t.Helper()
	m := captureMarks(t)
	reportPendingInstalls, purgePendingInstalls = ReportPendingInstalls, PurgePendingInstalls
	saved := relayInstrumentationEnabled
	t.Cleanup(func() { relayInstrumentationEnabled = saved })
	relayInstrumentationEnabled = func(context.Context) bool { return true }
	writeSharedMetricsConfig(t, m.home, sharedMetrics)
	return m
}

// Writes the profile config.yaml with one telemetry.shared_metrics mapping
func writeSharedMetricsConfig(t *testing.T, h, sharedMetrics string) {
	t.Helper()
	mustMkdir(t, h)
	body := "telemetry:\n  shared_metrics: " + sharedMetrics + "\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// An in contract installer receipt finished now after a 200 second run
func installReceipt(overrides map[string]any) map[string]any {
	now := time.Now().Unix()
	r := map[string]any{"id": strings.Repeat("ab", 16), "installer": "install_sh", "outcome": "failed",
		"failed_stage": "repository", "failure_class": "git_clone_failed", "started_at": now - 200, "finished_at": now}
	for k, v := range overrides {
		r[k] = v
	}
	return r
}

// Parks one receipt under the pending installs directory as the installer would
func parkReceipt(t *testing.T, h string, receipt map[string]any) string {
	t.Helper()
	dir := PendingInstallsDir(h)
	mustMkdir(t, dir)
	path := filepath.Join(dir, receipt["id"].(string)+".json")
	writeJSON(t, path, receipt)
	return path
}

func TestInstallReceiptIsReportedOnceOutOfContractOnesDropped(t *testing.T) {
	m := installMarks(t, "{enabled: true}")
	ctx := context.Background()
	good := parkReceipt(t, m.home, installReceipt(nil))
	success := parkReceipt(t, m.home, installReceipt(map[string]any{"id": strings.Repeat("cd", 16),
		"installer": "install_ps1", "outcome": "success", "failed_stage": "none", "failure_class": "none"}))
	extra := installReceipt(map[string]any{"id": strings.Repeat("03", 16)})
	extra["reason"] = "free text"
	bad := []string{
		parkReceipt(t, m.home, installReceipt(map[string]any{"id": strings.Repeat("01", 16),
			"failure_class": "git clone of https://host/x failed"})),
		parkReceipt(t, m.home, installReceipt(map[string]any{"id": strings.Repeat("02", 16), "failed_stage": "/home/someone"})),
		parkReceipt(t, m.home, extra),
		parkReceipt(t, m.home, installReceipt(map[string]any{"id": strings.Repeat("04", 16), "outcome": "success"})),
	}
	m.setSaves(false)
	ReportPendingInstalls(ctx, m.home)
	if !exists(good) || !exists(success) {
		t.Fatal("in contract receipts were not kept for the next start")
	}
	for _, p := range bad {
		if exists(p) {
			t.Fatalf("%s survived", p)
		}
	}
	if len(m.rows) != 0 {
		t.Fatalf("rows %v", m.rows)
	}
	m.setSaves(true)
	ReportPendingInstalls(ctx, m.home)
	parkReceipt(t, m.home, installReceipt(nil))
	ReportPendingInstalls(ctx, m.home)
	rows := append([]Mark(nil), m.rows...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Data["outcome"] < rows[j].Data["outcome"] })
	want := []Mark{
		{InstallRunMark, map[string]string{"duration_bucket": "2m_to_5m", "failed_stage": "repository",
			"failure_class": "git_clone_failed", "installer": "install_sh", "outcome": "failed"}},
		{InstallRunMark, map[string]string{"duration_bucket": "2m_to_5m", "failed_stage": "none",
			"failure_class": "none", "installer": "install_ps1", "outcome": "success"}},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows %v", rows)
	}
	if entries, _ := os.ReadDir(PendingInstallsDir(m.home)); len(entries) != 0 {
		t.Fatalf("left %v", entries)
	}
	assertValid(t, rows)
}

func TestInstallStartWithCollectionOffPurgesReceiptsUnreported(t *testing.T) {
	m := installMarks(t, "{enabled: false}")
	receipt := parkReceipt(t, m.home, installReceipt(nil))
	resetProcessState(t)
	m.setPolicy(false)
	BeginProcess(context.Background(), "cli")
	if exists(filepath.Dir(receipt)) {
		t.Fatal("receipt directory survived an opted out start")
	}
	m.setPolicy(true)
	ReportPendingInstalls(context.Background(), m.home)
	if len(m.rows) != 0 {
		t.Fatalf("rows %v", m.rows)
	}
}

func TestInstallReceiptIsRecordedOnlyOnADayTheSenderCanEverSend(t *testing.T) {
	m := installMarks(t, "{enabled: true, send: true}")
	ctx := context.Background()
	t0 := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	now := t0.Add(9 * time.Hour)
	savedNow := utcNow
	t.Cleanup(func() { utcNow = savedNow })
	utcNow = func() time.Time { return now }
	store, err := NewSharedMetricsStore(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	observe := func() {
		t.Helper()
		err := store.connection(ctx, busyTimeoutMS, func(conn state.SQLiteConn) error {
			return state.WriteTxn(ctx, conn, func() error { return ReconcileSendConsent(ctx, conn, true, now) })
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	recordProcessMarksSaved = func(_ context.Context, rows []Mark) int {
		for _, r := range rows {
			if err := store.RecordCounter(ctx, decisionMarkMetrics[r.Name], r.Data, testResource(), 1); err != nil {
				t.Fatal(err)
			}
		}
		return len(rows)
	}
	receipt := parkReceipt(t, m.home, installReceipt(map[string]any{"finished_at": now.Unix() - 60, "started_at": now.Unix() - 400}))
	observe()
	now = now.Add(time.Hour)
	ReportPendingInstalls(ctx, m.home)
	if snap, _ := store.CounterSnapshot(ctx); !exists(receipt) || len(snap) != 0 {
		t.Fatalf("opt in day recorded the receipt %v", snap)
	}
	now = t0.Add(32 * time.Hour)
	observe()
	ReportPendingInstalls(ctx, m.home)
	if exists(receipt) {
		t.Fatal("receipt waited past a consented day")
	}
	now = t0.Add(49 * time.Hour)
	observe()
	if _, err := store.CreateAndExportPackage(ctx); err != nil {
		t.Fatal(err)
	}
	var carrying []bool
	err = store.connection(ctx, busyTimeoutMS, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(ctx, "SELECT payload_json, "+ConsentGateSQL+" FROM package_outbox")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var body string
			var ok bool
			if err := rows.Scan(&body, &ok); err != nil {
				return err
			}
			var payload struct {
				Metrics []struct{ Name string } `json:"metrics"`
			}
			if err := json.Unmarshal([]byte(body), &payload); err != nil {
				return err
			}
			for _, metric := range payload.Metrics {
				if metric.Name == InstallRunMetric {
					carrying = append(carrying, ok)
					break
				}
			}
		}
		return rows.Err()
	})
	if err != nil || !reflect.DeepEqual(carrying, []bool{true}) {
		t.Fatalf("carrying %v err %v", carrying, err)
	}
}

func TestInstallReceiptWaitsWithoutAnOpenSendWindowAndIsRecordedAtOnceWhenKeptLocal(t *testing.T) {
	m := installMarks(t, "{enabled: true, send: true}")
	receipt := parkReceipt(t, m.home, installReceipt(nil))
	ReportPendingInstalls(context.Background(), m.home)
	if !exists(receipt) || len(m.rows) != 0 {
		t.Fatalf("receipt recorded without a send window %v", m.rows)
	}
	writeSharedMetricsConfig(t, m.home, "{enabled: true, send: false}")
	ReportPendingInstalls(context.Background(), m.home)
	if exists(receipt) || len(m.rows) != 1 {
		t.Fatalf("local only receipt not recorded %v", m.rows)
	}
}

func TestUnreadableAndWeekOldReceiptsAreDeletedUnreported(t *testing.T) {
	m := installMarks(t, "{enabled: true}")
	dir := PendingInstallsDir(m.home)
	mustMkdir(t, dir)
	bad := []string{filepath.Join(dir, strings.Repeat("c", 32)+".json"), filepath.Join(dir, strings.Repeat("d", 32)+".json")}
	if err := os.WriteFile(bad[0], []byte(`{"id":"`+strings.Repeat("c", 32)+`","started_at":,"finished_at":}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad[1], []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Unix() - MaxReceiptAgeSeconds - 3600
	stale := parkReceipt(t, m.home, installReceipt(map[string]any{"id": strings.Repeat("e", 32), "started_at": old - 100, "finished_at": old}))
	ReportPendingInstalls(context.Background(), m.home)
	for _, p := range append(bad, stale) {
		if exists(p) {
			t.Fatalf("%s survived", p)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 || len(m.rows) != 0 {
		t.Fatalf("left %v rows %v", entries, m.rows)
	}
}

func TestRelayInstrumentationOffKeepsTheReceiptInsteadOfLatchingItUnrecorded(t *testing.T) {
	m := installMarks(t, "{enabled: true}")
	relayInstrumentationEnabled = func(context.Context) bool { return false }
	receipt := parkReceipt(t, m.home, installReceipt(nil))
	ReportPendingInstalls(context.Background(), m.home)
	if !exists(receipt) {
		t.Fatal("receipt dropped while relay instrumentation was off")
	}
	if exists(recordedLatch(m.home, strings.TrimSuffix(filepath.Base(receipt), ".json"))) {
		t.Fatal("receipt latched unrecorded")
	}
}
