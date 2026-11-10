package hostinfo

import (
	"math"
	"strconv"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// BatteryStatus is one battery reading, the percent clamped to a hundred and Plugged nil when unknown
type BatteryStatus struct {
	Available bool
	Percent   *int
	Plugged   *bool
}

// Charging reports a battery on external power
func (s BatteryStatus) Charging() bool { return s.Plugged != nil && *s.Plugged }

const (
	BatteryCategoryGood     = "good"
	BatteryCategoryWarn     = "warn"
	BatteryCategoryBad      = "bad"
	BatteryCategoryCritical = "critical"
	BatteryCategoryDim      = "dim"

	batteryCacheTTLSeconds = 8.0
)

// sensorReading is what the platform battery probe reports, as psutil sensors_battery does
type sensorReading struct {
	percent *float64
	plugged *bool
}

var (
	batteryLevelCategories = []struct {
		bound    int
		category string
	}{{10, BatteryCategoryCritical}, {20, BatteryCategoryBad}, {50, BatteryCategoryWarn}}
	batteryMu      sync.Mutex
	batteryCached  *BatteryStatus
	batteryCacheAt float64
	sensorsBattery = platformSensorsBattery
)

// Reads the host battery without the cache, unavailable when the host has none or the probe fails
func readBatteryUncached() (status BatteryStatus) {
	defer func() {
		if recover() != nil {
			status = BatteryStatus{}
		}
	}()
	reading, ok := sensorsBattery()
	if !ok {
		return BatteryStatus{}
	}
	status = BatteryStatus{Available: true, Plugged: reading.plugged}
	if reading.percent != nil && !math.IsNaN(*reading.percent) && !math.IsInf(*reading.percent, 0) {
		percent := max(0, min(100, int(math.RoundToEven(*reading.percent))))
		status.Percent = &percent
	}
	return status
}

// ReadBattery returns the current battery status, memoised for a few seconds when useCache is set
func ReadBattery(useCache bool) BatteryStatus {
	batteryMu.Lock()
	defer batteryMu.Unlock()
	if useCache && batteryCached != nil && pythread.Monotonic()-batteryCacheAt < batteryCacheTTLSeconds {
		return *batteryCached
	}
	status := readBatteryUncached()
	batteryCached, batteryCacheAt = &status, pythread.Monotonic()
	return status
}

// ClearBatteryCache drops the memoised reading
func ClearBatteryCache() {
	batteryMu.Lock()
	batteryCached = nil
	batteryMu.Unlock()
}

// BatteryCategory buckets a reading into good, warn, bad, critical or dim for the status bar colour
func BatteryCategory(status BatteryStatus) string {
	if !status.Available || status.Percent == nil {
		return BatteryCategoryDim
	}
	if status.Charging() {
		return BatteryCategoryGood
	}
	for _, level := range batteryLevelCategories {
		if *status.Percent <= level.bound {
			return level.category
		}
	}
	return BatteryCategoryGood
}

// BatteryGlyph is a bolt while charging and a battery otherwise
func BatteryGlyph(status BatteryStatus) string {
	if status.Charging() {
		return "⚡"
	}
	return "\U0001f50b"
}

// FormatBattery renders the compact glyph and percent label, empty when no reading is available
func FormatBattery(status BatteryStatus) string {
	if !status.Available || status.Percent == nil {
		return ""
	}
	return BatteryGlyph(status) + " " + strconv.Itoa(*status.Percent) + "%"
}
