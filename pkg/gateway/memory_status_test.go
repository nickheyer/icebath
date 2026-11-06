package gateway

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

var memoryNow = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

func writeMemoryHeartbeat(t *testing.T, h string, updatedAt time.Time, mem *pyjson.Object) {
	t.Helper()
	payload := rec("pid", int64(12345), "updated_at", pydatetime.ISOFormat(updatedAt), "monotonic", 1.0)
	if mem != nil {
		payload.Set("mem", mem)
	}
	writeRecord(t, GetLoopHeartbeatPath(context.Background(), h), payload)
}

func collectStatus(h string) *pyjson.Object {
	return CollectMemoryStatus(context.Background(), h, &memoryNow)
}

func TestClassifyPressure(t *testing.T) {
	cases := []struct {
		name         string
		avail, total any
		want         string
	}{
		{"plentiful_memory_is_ok", int64(1024 * 1024), int64(2048 * 1024), "ok"},
		{"low_absolute_available_is_critical", int64(32 * 1024), int64(8 * 1024 * 1024), "critical"},
		{"low_fraction_is_critical", int64(300 * 1024), int64(8 * 1024 * 1024), "critical"},
		{"elevated_band", int64(100 * 1024), int64(1024 * 1024), "elevated"},
		{"missing_sample_is_unknown", nil, nil, "unknown"},
		{"bool_is_not_an_int", true, int64(2048 * 1024), "unknown"},
		{"absolute_floor_without_total_critical", int64(32 * 1024), nil, "critical"},
		{"absolute_floor_without_total_ok", int64(1024 * 1024), nil, "ok"},
	}
	for _, c := range cases {
		if got := ClassifyPressure(c.avail, c.total); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCollectMemoryStatusNoFilesYieldsUnknown(t *testing.T) {
	status := collectStatus(t.TempDir())
	if recGet(status, "pressure") != "unknown" || recGet(status, "gateway_rss_mb") != nil ||
		recGet(status, "last_boot_unclean") != false || recGet(status, "last_boot_suspected_oom") != false {
		t.Fatalf("%v", status)
	}
}

func TestCollectMemoryStatusFreshHeartbeatReportsPressureAndNumbers(t *testing.T) {
	h := t.TempDir()
	writeMemoryHeartbeat(t, h, memoryNow.Add(-30*time.Second), rec("rss_kib", int64(400*1024), "mem_total_kib", int64(1024*1024),
		"mem_available_kib", int64(50*1024), "swap_used_kib", int64(200*1024)))
	status := collectStatus(h)
	if recGet(status, "pressure") != "critical" || recGet(status, "gateway_rss_mb") != int64(400) ||
		recGet(status, "system_total_mb") != int64(1024) || recGet(status, "system_available_mb") != int64(50) ||
		recGet(status, "swap_used_mb") != int64(200) || recGet(status, "sampled_at") != "2026-08-13T11:59:30+00:00" {
		t.Fatalf("%v", status)
	}
}

func TestCollectMemoryStatusStaleHeartbeatKeepsNumbersButUnknownPressure(t *testing.T) {
	h := t.TempDir()
	writeMemoryHeartbeat(t, h, memoryNow.Add(-2*time.Hour), rec("rss_kib", int64(400*1024), "mem_total_kib", int64(1024*1024),
		"mem_available_kib", int64(10*1024)))
	status := collectStatus(h)
	if recGet(status, "pressure") != "unknown" || recGet(status, "system_available_mb") != int64(10) || recGet(status, "sampled_at") == nil {
		t.Fatalf("%v", status)
	}
}

func TestCollectMemoryStatusFutureHeartbeatIsTreatedAsStale(t *testing.T) {
	h := t.TempDir()
	writeMemoryHeartbeat(t, h, memoryNow.Add(time.Hour), rec("mem_total_kib", int64(1024*1024), "mem_available_kib", int64(10*1024)))
	if status := collectStatus(h); recGet(status, "pressure") != "unknown" {
		t.Fatalf("%v", status)
	}
}

func TestCollectMemoryStatusSentinelFlagsSurface(t *testing.T) {
	h := t.TempDir()
	writeRecord(t, GetLifecycleSentinelPath(context.Background(), h), rec("phase", "running", "pid", int64(999),
		"started_at", "2026-08-13T01:00:00+00:00", "prior_unclean_exit", true, "prior_suspected_oom", true))
	status := collectStatus(h)
	if recGet(status, "last_boot_unclean") != true || recGet(status, "last_boot_suspected_oom") != true ||
		recGet(status, "boot_id") != "2026-08-13T01:00:00+00:00" {
		t.Fatalf("%v", status)
	}
}

func TestCollectMemoryStatusBootIDAbsentOrMalformedStaysNone(t *testing.T) {
	h := t.TempDir()
	writeRecord(t, GetLifecycleSentinelPath(context.Background(), h), rec("phase", "running", "pid", int64(999), "started_at", int64(12345)))
	if status := collectStatus(h); recGet(status, "boot_id") != nil {
		t.Fatalf("%v", status)
	}
	if status := collectStatus(filepath.Join(h, "nohome")); recGet(status, "boot_id") != nil {
		t.Fatalf("%v", status)
	}
}

func TestCollectMemoryStatusCleanSentinelHasNoFlags(t *testing.T) {
	h := t.TempDir()
	writeRecord(t, GetLifecycleSentinelPath(context.Background(), h), rec("phase", "exited", "pid", int64(999), "exit_reason", "graceful_shutdown"))
	status := collectStatus(h)
	if recGet(status, "last_boot_unclean") != false || recGet(status, "last_boot_suspected_oom") != false {
		t.Fatalf("%v", status)
	}
}

func TestCollectMemoryStatusCorruptFilesNeverRaise(t *testing.T) {
	h := t.TempDir()
	writeLedgerFile(t, GetLoopHeartbeatPath(context.Background(), h), "{not json")
	writeLedgerFile(t, GetLifecycleSentinelPath(context.Background(), h), "[]")
	if status := collectStatus(h); recGet(status, "pressure") != "unknown" {
		t.Fatalf("%v", status)
	}
}
