package gateway

import (
	"math/big"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
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

// The value when it is a non negative int and not a bool, nil otherwise
func nonnegInt(value any) *big.Int {
	if _, isBool := value.(bool); isBool || value == nil {
		return nil
	}
	if _, isFloat := value.(float64); isFloat {
		return nil
	}
	n, ok := pyrepr.Int(value)
	if !ok || n.Sign() < 0 {
		return nil
	}
	return n
}

// ok, elevated or critical from MemAvailable and MemTotal, unknown when the sample is missing or malformed
func ClassifyPressure(availableKiB, totalKiB any) string {
	available, total := nonnegInt(availableKiB), nonnegInt(totalKiB)
	if available == nil {
		return "unknown"
	}
	var fraction *float64
	if total != nil && total.Sign() != 0 {
		f, _ := new(big.Rat).SetFrac(available, total).Float64()
		fraction = &f
	}
	for _, tier := range pressureTiers {
		if available.Cmp(big.NewInt(tier.kibFloor)) < 0 || (fraction != nil && *fraction < tier.frac) {
			return tier.level
		}
	}
	return "ok"
}
