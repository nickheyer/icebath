package tui

import (
	"math"
	"time"

	tea "charm.land/bubbletea/v2"
)

const batteryPollInterval = 30 * time.Second

var batteryCategories = map[string]bool{"bad": true, "critical": true, "dim": true, "good": true, "warn": true}

// Battery reading the status rule shows, its category, percent and charger state when known
type batteryInfo struct {
	Available bool
	Category  string
	Percent   *int
	Plugged   *bool
}

// Polls system.battery while the battery indicator is on, one generation per enable
type batteryPoller struct {
	enabled bool
	started bool
	gen     int
}

// Interval tick of the battery poll of one generation
type batteryPollMsg struct{ gen int }

// Battery info of a system.battery answer, nil for a missing answer
func toBatteryInfo(r map[string]any) *batteryInfo {
	if r == nil {
		return nil
	}
	info := &batteryInfo{Available: truthyJS(r["available"]), Category: "dim"}
	if c, isStr := r["category"].(string); isStr && batteryCategories[c] {
		info.Category = c
	}
	if p, isNum := r["percent"].(float64); isNum && !math.IsInf(p, 0) && !math.IsNaN(p) {
		n := int(math.Max(0, math.Min(100, jsRound(p))))
		info.Percent = &n
	}
	if plugged, isBool := r["plugged"].(bool); isBool {
		info.Plugged = &plugged
	}
	return info
}

// Restarts polling after the battery setting changed, clearing the reading when it turned off
func (b *batteryPoller) sync(store *tuiStore, gw turnGateway) tea.Cmd {
	if b.started && b.enabled == store.ui.Battery {
		return nil
	}
	b.started, b.enabled = true, store.ui.Battery
	b.gen++
	if !b.enabled {
		store.ui.BatteryStatus = nil
		return nil
	}
	return tea.Batch(b.poll(store, gw), b.interval())
}

func (b *batteryPoller) interval() tea.Cmd {
	gen := b.gen
	return tea.Tick(batteryPollInterval, func(time.Time) tea.Msg { return batteryPollMsg{gen: gen} })
}

// One system.battery request whose answer lands while the generation is current, failures ignored
func (b *batteryPoller) poll(store *tuiStore, gw turnGateway) tea.Cmd {
	gen := b.gen
	return gatewayCall(gw, "system.battery", map[string]any{}, func(v any, err error) tea.Cmd {
		if err == nil && gen == b.gen {
			store.ui.BatteryStatus = toBatteryInfo(asRPCResult(v))
		}
		return nil
	})
}

// Next poll and interval of a tick from the current generation
func (b *batteryPoller) onTick(m batteryPollMsg, store *tuiStore, gw turnGateway) tea.Cmd {
	if m.gen != b.gen || !b.enabled {
		return nil
	}
	return tea.Batch(b.poll(store, gw), b.interval())
}
