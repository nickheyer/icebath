package gateway

import (
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Window plan allowance for the status bar, a full binding one over today's, nil off a window plan or past its reset
func PlanGauge(state *provider.CreditsState) map[string]any {
	if state == nil || !state.IsUsageWindow() {
		return nil
	}
	binding := state.BindingGauge()
	var day *provider.WindowGauge
	for i := range state.Gauges {
		if state.Gauges[i].ID == "day" && state.Gauges[i].Scope == "all" {
			day = &state.Gauges[i]
			break
		}
	}
	gauge := binding
	if binding == nil || !binding.Full() {
		if day != nil {
			gauge = day
		}
	}
	if gauge == nil {
		return nil
	}
	if reset := gauge.ResetsAtMs; reset != nil && *reset > 0 && *reset <= time.Now().UnixMilli() {
		return nil
	}
	var resets any
	if gauge.ResetsAtMs != nil {
		resets = *gauge.ResetsAtMs
	}
	return map[string]any{"id": gauge.ID, "label": gauge.Label, "used_bp": gauge.UsedBp, "resets_at_ms": resets}
}
