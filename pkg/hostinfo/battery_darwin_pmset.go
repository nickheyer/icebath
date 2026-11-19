//go:build darwin && !cgo

package hostinfo

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	pmsetSourceRe  = regexp.MustCompile(`Now drawing from '([^']+)'`)
	pmsetPercentRe = regexp.MustCompile(`(\d+(?:\.\d+)?)%`)
)

// Reads the internal battery through pmset, the system tool every macOS ships, when the build has no cgo for IOKit
func platformSensorsBattery() (sensorReading, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/pmset", "-g", "batt").Output()
	if err != nil {
		return sensorReading{}, false
	}
	return parsePmsetBattery(string(out))
}

// Percent and power source of the InternalBattery row of pmset -g batt, false on a Mac without a battery
func parsePmsetBattery(text string) (sensorReading, bool) {
	source := ""
	if m := pmsetSourceRe.FindStringSubmatch(text); m != nil {
		source = m[1]
	}
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, "InternalBattery") {
			continue
		}
		m := pmsetPercentRe.FindStringSubmatch(line)
		if m == nil {
			return sensorReading{}, false
		}
		percent, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return sensorReading{}, false
		}
		plugged := source == "AC Power"
		return sensorReading{percent: &percent, plugged: &plugged}, true
	}
	return sensorReading{}, false
}
