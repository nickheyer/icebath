package gateway

import (
	"context"
	"math/big"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// MemAvailable floors of the critical and elevated tiers, critical doubling as the ledger OOM suspicion
const (
	criticalAvailableKiB      = 64 * 1024
	criticalAvailableFraction = 0.05
	elevatedAvailableKiB      = 128 * 1024
	elevatedAvailableFraction = 0.15
	heartbeatFreshTTLS        = 150.0
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

// KiB floored to MB, nil for anything but a non negative int
func kibToMB(kib any) any {
	n := nonnegInt(kib)
	if n == nil {
		return nil
	}
	return pyint.Value(new(big.Int).Quo(n, big.NewInt(1024)))
}

// fromisoformat of a non empty string with naive values taken as UTC, false when absent or invalid
func memoryParseISO(value any) (time.Time, bool) {
	s, ok := value.(string)
	if !ok || s == "" {
		return time.Time{}, false
	}
	t, aware, err := pydatetime.ParseISOFormat(s)
	if err != nil {
		return time.Time{}, false
	}
	if !aware {
		t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
	}
	return t, true
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

// Memory block of the status endpoint from the heartbeat and lifecycle sentinel, now defaulting to the current time
func CollectMemoryStatus(ctx context.Context, h string, now *time.Time) *pyjson.Object {
	moment := time.Now().UTC().Truncate(time.Microsecond)
	if now != nil {
		moment = *now
	}
	status := pyjson.NewObject()
	for _, k := range []string{"pressure", "gateway_rss_mb", "system_total_mb", "system_available_mb", "swap_used_mb",
		"sampled_at", "last_boot_unclean", "last_boot_suspected_oom", "boot_id"} {
		status.Set(k, nil)
	}
	status.Set("pressure", "unknown")
	status.Set("last_boot_unclean", false)
	status.Set("last_boot_suspected_oom", false)
	heartbeat := ledgerReadJSON(GetLoopHeartbeatPath(ctx, h))
	sentinel := ledgerReadJSON(GetLifecycleSentinelPath(ctx, h))
	if heartbeat != nil && heartbeat.Len() > 0 {
		sampledAt, sampled := memoryParseISO(recGet(heartbeat, "updated_at"))
		if mem, ok := recGet(heartbeat, "mem").(*pyjson.Object); ok {
			for _, pair := range [][2]string{{"gateway_rss_mb", "rss_kib"}, {"system_total_mb", "mem_total_kib"},
				{"system_available_mb", "mem_available_kib"}, {"swap_used_mb", "swap_used_kib"}} {
				status.Set(pair[0], kibToMB(recGet(mem, pair[1])))
			}
			if sampled {
				status.Set("sampled_at", pydatetime.ISOFormat(sampledAt))
				if age := moment.Sub(sampledAt).Seconds(); age >= 0 && age <= heartbeatFreshTTLS {
					status.Set("pressure", ClassifyPressure(recGet(mem, "mem_available_kib"), recGet(mem, "mem_total_kib")))
				}
			}
		}
	}
	if sentinel != nil && sentinel.Len() > 0 {
		status.Set("last_boot_unclean", pyrepr.Truthy(recGet(sentinel, "prior_unclean_exit")))
		status.Set("last_boot_suspected_oom", pyrepr.Truthy(recGet(sentinel, "prior_suspected_oom")))
		if startedAt, ok := recGet(sentinel, "started_at").(string); ok && startedAt != "" {
			status.Set("boot_id", startedAt)
		} else {
			status.Set("boot_id", nil)
		}
	}
	return status
}
