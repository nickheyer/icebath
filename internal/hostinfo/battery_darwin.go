package hostinfo

import (
	"errors"
	"fmt"
)

// First IOKit power source with its current capacity and power source state
func sensorsBattery() (batteryReading, error) {
	f, err := loadFrameworks()
	if err != nil {
		return batteryReading{}, fmt.Errorf("battery: %w", err)
	}
	info := f.call("IOPSCopyPowerSourcesInfo")
	if info == 0 {
		return batteryReading{}, errors.New("battery: IOKit returned no power source information")
	}
	defer f.release(info)
	list := f.call("IOPSCopyPowerSourcesList", info)
	if list == 0 {
		return batteryReading{}, errors.New("battery: IOKit returned no power source list")
	}
	defer f.release(list)
	if f.call("CFArrayGetCount", list) == 0 {
		return batteryReading{}, fmt.Errorf("battery: %w", ErrAbsent)
	}
	description := f.call("IOPSGetPowerSourceDescription", info, f.call("CFArrayGetValueAtIndex", list, 0))
	if description == 0 {
		return batteryReading{}, errors.New("battery: power source has no description")
	}
	keys := map[string]uintptr{}
	for _, name := range []string{"Current Capacity", "Power Source State", "AC Power"} {
		keys[name] = f.cfString(name)
		defer f.release(keys[name])
	}
	value := func(key string) uintptr { return f.call("CFDictionaryGetValue", description, keys[key]) }
	current, ok := f.number(value("Current Capacity"))
	if !ok {
		return batteryReading{}, errors.New("battery: power source reports no capacity")
	}
	r := batteryReading{percent: float64(current)}
	if state := value("Power Source State"); state != 0 {
		plugged := int(f.call("CFStringCompare", state, keys["AC Power"], 0)) == 0
		r.plugged = &plugged
	}
	return r, nil
}
