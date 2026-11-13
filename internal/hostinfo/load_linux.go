package hostinfo

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Load reading state, empty because the kernel keeps the averages itself
type loadState struct{}

// Averages from the kernel load file
func (*loadState) read() ([3]float64, error) {
	var averages [3]float64
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return averages, fmt.Errorf("load average: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return averages, fmt.Errorf("load average: %d fields", len(fields))
	}
	for i := range averages {
		if averages[i], err = strconv.ParseFloat(fields[i], 64); err != nil {
			return averages, fmt.Errorf("load average: %w", err)
		}
	}
	return averages, nil
}
