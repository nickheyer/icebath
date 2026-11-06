package gateway

import (
	"context"
	"fmt"
	"math/big"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const bytesPerMB = 1024 * 1024

var diskPressureTiers = []struct {
	level        string
	freeFloor    int64
	percentFloor float64
	headroom     int64
}{
	{"critical", 256, 95.0, 1024},
	{"elevated", 512, 85.0, 4096},
}

var diskUsage = pyfs.DiskUsage

// ok, elevated or critical from free and total MB, unknown for a missing or malformed sample
func ClassifyDiskPressure(freeMB, totalMB any) string {
	free, total := nonnegInt(freeMB), nonnegInt(totalMB)
	if free == nil || total == nil || total.Sign() == 0 {
		return "unknown"
	}
	ratio, _ := new(big.Rat).SetFrac(free, total).Float64()
	usedPercent := (1 - ratio) * 100.0
	for _, tier := range diskPressureTiers {
		if free.Cmp(big.NewInt(tier.freeFloor)) < 0 || (usedPercent >= tier.percentFloor && free.Cmp(big.NewInt(tier.headroom)) < 0) {
			return tier.level
		}
	}
	return "ok"
}

// Disk block of the status API for the home volume, pressure unknown when the filesystem cannot be read
func CollectDiskStatus(ctx context.Context, h string) *pyjson.Object {
	status := pyjson.NewObject()
	status.Set("pressure", "unknown")
	status.Set("total_mb", nil)
	status.Set("free_mb", nil)
	status.Set("used_percent", nil)
	total, used, free, err := func() (total, used, free uint64, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		if h == "" {
			h = home.Dir(ctx)
		}
		return diskUsage(h)
	}()
	if err != nil || total == 0 {
		return status
	}
	totalMB, freeMB := pyint.Value(new(big.Int).SetUint64(total/bytesPerMB)), pyint.Value(new(big.Int).SetUint64(free/bytesPerMB))
	ratio, _ := new(big.Rat).SetFrac(new(big.Int).SetUint64(used), new(big.Int).SetUint64(total)).Float64()
	status.Set("total_mb", totalMB)
	status.Set("free_mb", freeMB)
	status.Set("used_percent", pyint.Round(ratio*100, 1))
	status.Set("pressure", ClassifyDiskPressure(freeMB, totalMB))
	return status
}
