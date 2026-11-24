package gateway

import (
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// MemAvailable floors of the critical and elevated tiers, critical doubling as the ledger OOM suspicion
const (
	criticalAvailableKiB      = 64 * 1024
	criticalAvailableFraction = 0.05
	elevatedAvailableKiB      = 128 * 1024
	elevatedAvailableFraction = 0.15
)

var pressureTiers = []struct {
	level    string
	kibFloor int64
	frac     float64
}{
	{"critical", criticalAvailableKiB, criticalAvailableFraction},
	{"elevated", elevatedAvailableKiB, elevatedAvailableFraction},
}

// The value when it is a non negative integer, false for bools, floats and every other type
func nonnegInt(value any) (int64, bool) {
	n, ok := anyutil.Int(value)
	return n, ok && n >= 0
}

// ok, elevated or critical from MemAvailable and MemTotal, unknown when the sample is missing or malformed
func ClassifyPressure(availableKiB, totalKiB any) string {
	available, ok := nonnegInt(availableKiB)
	if !ok {
		return "unknown"
	}
	var fraction *float64
	if total, ok := nonnegInt(totalKiB); ok && total != 0 {
		f := float64(available) / float64(total)
		fraction = &f
	}
	for _, tier := range pressureTiers {
		if available < tier.kibFloor || (fraction != nil && *fraction < tier.frac) {
			return tier.level
		}
	}
	return "ok"
}
