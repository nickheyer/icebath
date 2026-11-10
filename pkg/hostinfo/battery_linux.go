package hostinfo

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

var powerSupplyPath = "/sys/class/power_supply"

// First readable file of the paths as an int, false when none exists or the value is not an int
func powerSupplyInt(paths ...string) (int64, bool) {
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		return n, err == nil
	}
	return 0, false
}

// Reads the first battery under the power supply tree the way psutil sensors_battery does on Linux
func platformSensorsBattery() (sensorReading, bool) {
	entries, err := os.ReadDir(powerSupplyPath)
	if err != nil {
		return sensorReading{}, false
	}
	var bats []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "BAT") || strings.Contains(strings.ToLower(e.Name()), "battery") {
			bats = append(bats, e.Name())
		}
	}
	if len(bats) == 0 {
		return sensorReading{}, false
	}
	root := filepath.Join(powerSupplyPath, slices.Min(bats))
	energyNow, hasNow := powerSupplyInt(filepath.Join(root, "energy_now"), filepath.Join(root, "charge_now"))
	energyFull, hasFull := powerSupplyInt(filepath.Join(root, "energy_full"), filepath.Join(root, "charge_full"))
	var percent float64
	if hasNow && hasFull {
		if energyFull != 0 {
			percent = 100.0 * float64(energyNow) / float64(energyFull)
		}
	} else {
		capacity, ok := powerSupplyInt(filepath.Join(root, "capacity"))
		if !ok || capacity == -1 {
			return sensorReading{}, false
		}
		percent = float64(capacity)
	}
	reading := sensorReading{percent: &percent}
	if online, ok := powerSupplyInt(filepath.Join(powerSupplyPath, "AC0", "online"), filepath.Join(powerSupplyPath, "AC", "online")); ok {
		plugged := online == 1
		reading.plugged = &plugged
	} else if raw, err := os.ReadFile(filepath.Join(root, "status")); err == nil {
		switch strings.ToLower(strings.TrimSpace(string(raw))) {
		case "discharging":
			plugged := false
			reading.plugged = &plugged
		case "charging", "full":
			plugged := true
			reading.plugged = &plugged
		}
	}
	return reading, true
}
