package gateway

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const diskGB = 1024

func TestClassifyDiskPressure(t *testing.T) {
	cases := []struct {
		free, total any
		want        string
	}{
		{40 * diskGB, 100 * diskGB, "ok"},
		{200, 500 * diskGB, "critical"},
		{800, 20 * diskGB, "critical"},
		{20 * diskGB, 500 * diskGB, "ok"},
		{400, 4 * diskGB, "elevated"},
		{2400, 20 * diskGB, "elevated"},
		{50 * diskGB, 500 * diskGB, "ok"},
		{nil, nil, "unknown"},
		{"lots", 100, "unknown"},
		{true, 100, "unknown"},
		{-5, 100, "unknown"},
		{100, 0, "unknown"},
	}
	for _, c := range cases {
		if got := ClassifyDiskPressure(c.free, c.total); got != c.want {
			t.Errorf("ClassifyDiskPressure(%v, %v) = %s want %s", c.free, c.total, got, c.want)
		}
	}
}

func withDiskUsage(t *testing.T, fn func(string) (uint64, uint64, uint64, error)) {
	prev := diskUsage
	diskUsage = fn
	t.Cleanup(func() { diskUsage = prev })
}

func TestCollectDiskStatusReportsRealUsage(t *testing.T) {
	status := CollectDiskStatus(context.Background(), t.TempDir())
	p, _ := status.Get("pressure")
	if p != "ok" && p != "elevated" && p != "critical" {
		t.Fatalf("pressure %v", p)
	}
	total, _ := status.Get("total_mb")
	free, _ := status.Get("free_mb")
	used, _ := status.Get("used_percent")
	if n, ok := pyrepr.Int(total); !ok || n.Sign() <= 0 {
		t.Fatalf("total %v", total)
	}
	if n, ok := pyrepr.Int(free); !ok || n.Sign() < 0 {
		t.Fatalf("free %v", free)
	}
	if f, ok := used.(float64); !ok || f < 0 || f > 100 {
		t.Fatalf("used %v", used)
	}
}

func TestCollectDiskStatusDegradesToUnknown(t *testing.T) {
	withDiskUsage(t, func(string) (uint64, uint64, uint64, error) { return 0, 0, 0, errors.New("statvfs failed") })
	status := CollectDiskStatus(context.Background(), t.TempDir())
	if got, want := pyDumps(t, status), `{"pressure": "unknown", "total_mb": null, "free_mb": null, "used_percent": null}`; got != want {
		t.Fatalf("unreadable status %s", got)
	}
	withDiskUsage(t, func(string) (uint64, uint64, uint64, error) { return 0, 0, 0, nil })
	status = CollectDiskStatus(context.Background(), t.TempDir())
	if p, _ := status.Get("pressure"); p != "unknown" {
		t.Fatalf("zero total pressure %v", p)
	}
	if v, _ := status.Get("total_mb"); v != nil {
		t.Fatalf("zero total total_mb %v", v)
	}
	withDiskUsage(t, func(string) (uint64, uint64, uint64, error) { return 0, 0, 0, fs.ErrPermission })
	if p, _ := CollectDiskStatus(context.Background(), "").Get("pressure"); p != "unknown" {
		t.Fatalf("homeless pressure %v", p)
	}
}

func TestCollectDiskStatusSyntheticFullVolumeIsCritical(t *testing.T) {
	total, free := uint64(10*1024*1024*1024), uint64(100*1024*1024)
	withDiskUsage(t, func(string) (uint64, uint64, uint64, error) { return total, total - free, free, nil })
	status := CollectDiskStatus(context.Background(), t.TempDir())
	if got, want := pyDumps(t, status), `{"pressure": "critical", "total_mb": 10240, "free_mb": 100, "used_percent": 99.0}`; got != want {
		t.Fatalf("full volume %s", got)
	}
}

func pyDumps(t *testing.T, v any) string {
	t.Helper()
	s, err := pyjson.Dumps(v)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
