package gateway

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// One minute load average from /proc/loadavg, the os.getloadavg of Linux
func loadAverage1m() (float64, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, errors.New("Load averages are unobtainable")
	}
	return strconv.ParseFloat(fields[0], 64)
}
