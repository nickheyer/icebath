package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const batteryPollInterval = 30 * time.Second

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
func toBatteryInfo(r *tuigateway.SystemBatteryResult) *batteryInfo {
	if r == nil {
		return nil
	}
	info := &batteryInfo{Available: r.Available, Category: string(r.Category), Plugged: r.Plugged}
	if r.Percent != nil {
		n := int(max(0, min(100, *r.Percent)))
		info.Percent = &n
	}
	return info
}

// Restarts polling after the battery setting changed, clearing the reading when it turned off
func (b *batteryPoller) sync(ui *sharedUI, gw turnGateway) tea.Cmd {
	if b.started && b.enabled == ui.Battery {
		return nil
	}
	b.started, b.enabled = true, ui.Battery
	b.gen++
	if !b.enabled {
		ui.BatteryStatus = nil
		return nil
	}
	return tea.Batch(b.poll(ui, gw), b.interval())
}

func (b *batteryPoller) interval() tea.Cmd {
	gen := b.gen
	return tea.Tick(batteryPollInterval, func(time.Time) tea.Msg { return batteryPollMsg{gen: gen} })
}

// One system.battery request whose answer lands while the generation is current, failures ignored
func (b *batteryPoller) poll(ui *sharedUI, gw turnGateway) tea.Cmd {
	gen := b.gen
	return call(gw, tuigateway.SystemBattery, tuigateway.ProfileParams{}, func(r *tuigateway.SystemBatteryResult, err error) tea.Cmd {
		if err == nil && gen == b.gen {
			ui.BatteryStatus = toBatteryInfo(r)
		}
		return nil
	})
}

// Next poll and interval of a tick from the current generation
func (b *batteryPoller) onTick(m batteryPollMsg, ui *sharedUI, gw turnGateway) tea.Cmd {
	if m.gen != b.gen || !b.enabled {
		return nil
	}
	return tea.Batch(b.poll(ui, gw), b.interval())
}
