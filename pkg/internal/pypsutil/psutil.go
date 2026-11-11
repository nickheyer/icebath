package pypsutil

import (
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
)

// Physical memory figures in bytes with the used share as a percentage rounded to one decimal
type VirtualMemoryStats struct {
	Total, Available, Used uint64
	Percent                float64
}

// Volume figures in bytes with the used share as a percentage rounded to one decimal
type DiskUsageStats struct {
	Total, Used, Free uint64
	Percent           float64
}

// Used over total as a percentage rounded to one decimal, zero for a zero total
func usagePercent(used, total float64) float64 {
	if total == 0 {
		return 0.0
	}
	return pyint.Round(used/total*100, 1)
}

// System wide CPU use percentage measured across a blocking interval, rounded to one decimal
func CPUPercent(interval time.Duration) (float64, error) {
	before, err := cpuTimes()
	if err != nil {
		return 0, err
	}
	time.Sleep(interval)
	after, err := cpuTimes()
	if err != nil {
		return 0, err
	}
	deltas := make([]float64, len(before))
	for i := range before {
		deltas[i] = max(0, after[i]-before[i])
	}
	all := cpuTotalTime(deltas)
	if all == 0 {
		return 0.0, nil
	}
	return pyint.Round(cpuBusyTime(deltas)/all*100, 1), nil
}

// Sum of every CPU time field
func sumTimes(times []float64) float64 {
	total := 0.0
	for _, t := range times {
		total += t
	}
	return total
}
