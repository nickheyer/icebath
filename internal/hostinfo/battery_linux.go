package hostinfo

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const powerSupplyRoot = "/sys/class/power_supply"

// Integer in the first readable file of paths, numeric false for other text, present false when none is readable
func supplyValue(paths ...string) (n int64, numeric, present bool) {
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		return n, err == nil, true
	}
	return 0, false, false
}

// First battery of the power supply class with its charge and power source
func sensorsBattery() (batteryReading, error) {
	entries, err := os.ReadDir(powerSupplyRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return batteryReading{}, fmt.Errorf("battery: %w", ErrAbsent)
	}
	if err != nil {
		return batteryReading{}, fmt.Errorf("battery: %w", err)
	}
	var batteries []string
	for _, entry := range entries {
		if name := entry.Name(); strings.HasPrefix(name, "BAT") || strings.Contains(strings.ToLower(name), "battery") {
			batteries = append(batteries, name)
		}
	}
	if len(batteries) == 0 {
		return batteryReading{}, fmt.Errorf("battery: %w", ErrAbsent)
	}
	name := slices.Min(batteries)
	root := filepath.Join(powerSupplyRoot, name)
	percent, err := supplyPercent(root)
	if err != nil {
		return batteryReading{}, fmt.Errorf("battery %s: %w", name, err)
	}
	return batteryReading{percent: percent, plugged: supplyPlugged(filepath.Join(root, "status"))}, nil
}

// Charge from the energy or charge counters, zero for a zero full counter, else from the capacity file
func supplyPercent(root string) (float64, error) {
	file := func(name string) string { return filepath.Join(root, name) }
	now, nowNumeric, hasNow := supplyValue(file("energy_now"), file("charge_now"))
	full, fullNumeric, hasFull := supplyValue(file("energy_full"), file("charge_full"))
	if hasNow && hasFull {
		switch {
		case !nowNumeric || !fullNumeric:
			return 0, errors.New("charge counters hold no integer")
		case full == 0:
			return 0, nil
		}
		return 100 * float64(now) / float64(full), nil
	}
	capacity, numeric, present := supplyValue(file("capacity"))
	switch {
	case !present || (numeric && capacity == -1):
		return 0, fmt.Errorf("charge: %w", ErrAbsent)
	case !numeric:
		return 0, errors.New("capacity holds no integer")
	}
	return float64(capacity), nil
}

// External power from the AC adapter online flag, else from the battery status, nil when neither tells
func supplyPlugged(statusPath string) *bool {
	if online, numeric, present := supplyValue(filepath.Join(powerSupplyRoot, "AC0", "online"), filepath.Join(powerSupplyRoot, "AC", "online")); present {
		plugged := numeric && online == 1
		return &plugged
	}
	data, err := os.ReadFile(statusPath)
	if err != nil {
		return nil
	}
	var plugged bool
	switch strings.ToLower(strings.TrimSpace(string(data))) {
	case "discharging":
		plugged = false
	case "charging", "full":
		plugged = true
	default:
		return nil
	}
	return &plugged
}
