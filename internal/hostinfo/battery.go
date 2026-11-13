package hostinfo

import (
	"context"
	"math"
	"strconv"
	"time"
)

// One reading of the host battery with its charge in percent, Plugged nil when the OS cannot tell
type Battery struct {
	Percent int
	Plugged *bool
}

// Bucket of a reading that picks the color of the status bar indicator
type BatteryCategory string

const (
	BatteryGood     BatteryCategory = "good"
	BatteryWarn     BatteryCategory = "warn"
	BatteryBad      BatteryCategory = "bad"
	BatteryCritical BatteryCategory = "critical"
	BatteryDim      BatteryCategory = "dim"
)

const batteryTTL = 8 * time.Second

var batteryLevels = []struct {
	bound    int
	category BatteryCategory
}{{10, BatteryCritical}, {20, BatteryBad}, {50, BatteryWarn}}

// Raw battery figures as the OS reports them
type batteryReading struct {
	percent float64
	plugged *bool
}

// Reports a battery on external power
func (b Battery) Charging() bool { return b.Plugged != nil && *b.Plugged }

// Good on external power or above half, warn to fifty, bad to twenty, critical to ten
func (b Battery) Category() BatteryCategory {
	if b.Charging() {
		return BatteryGood
	}
	for _, level := range batteryLevels {
		if b.Percent <= level.bound {
			return level.category
		}
	}
	return BatteryGood
}

// Bolt while charging and battery otherwise, then the percent
func (b Battery) Label() string {
	glyph := "\U0001F50B"
	if b.Charging() {
		glyph = "⚡"
	}
	return glyph + " " + strconv.Itoa(b.Percent) + "%"
}

// Host battery memoised for eight seconds, ErrAbsent when the host has no battery
func (h *Host) Battery() (Battery, error) {
	return h.battery.get(context.Background(), batteryTTL, false, readBattery)
}

// Host battery read now and memoised for the next eight seconds
func (h *Host) RefreshBattery() (Battery, error) {
	return h.battery.get(context.Background(), batteryTTL, true, readBattery)
}

// Battery read from the OS now with its charge rounded and clamped to the percent range
func readBattery(context.Context) (Battery, error) {
	r, err := sensorsBattery()
	if err != nil {
		return Battery{}, err
	}
	return Battery{Percent: max(0, min(100, int(math.RoundToEven(r.percent)))), Plugged: r.plugged}, nil
}
