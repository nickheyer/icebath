package pypsutil

import (
	"os"
	"testing"
	"time"
)

func TestVirtualMemory(t *testing.T) {
	mem, err := VirtualMemory()
	if err != nil {
		t.Fatal(err)
	}
	if mem.Total == 0 || mem.Available == 0 || mem.Available > mem.Total || mem.Used > mem.Total {
		t.Fatalf("memory %+v", mem)
	}
	if mem.Percent < 0 || mem.Percent > 100 {
		t.Fatalf("percent %v", mem.Percent)
	}
}

func TestCPUPercent(t *testing.T) {
	for range 3 {
		percent, err := CPUPercent(1 * time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if percent < 0 || percent > 100 {
			t.Fatalf("percent %v", percent)
		}
	}
}

func TestDiskUsage(t *testing.T) {
	usage, err := DiskUsage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if usage.Total == 0 || usage.Used > usage.Total || usage.Free > usage.Total || usage.Percent < 0 || usage.Percent > 100 {
		t.Fatalf("usage %+v", usage)
	}
	if _, err := DiskUsage(os.DevNull + "-absent-volume"); err == nil {
		t.Fatal("a missing path must fail")
	}
}

func TestGetloadavg(t *testing.T) {
	load, err := Getloadavg()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range load {
		if v < 0 {
			t.Fatalf("load %v", load)
		}
	}
}

func TestUsagePercent(t *testing.T) {
	if got := usagePercent(1, 3); got != 33.3 {
		t.Fatalf("got %v", got)
	}
	if got := usagePercent(5, 0); got != 0 {
		t.Fatalf("got %v", got)
	}
}
