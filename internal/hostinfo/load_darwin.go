package hostinfo

import (
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// Load reading state, empty because the kernel keeps the averages itself
type loadState struct{}

// Averages from the fixed point loadavg sysctl and its scale
func (*loadState) read() ([3]float64, error) {
	var averages [3]float64
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil {
		return averages, fmt.Errorf("load average: %w", err)
	}
	if len(raw) < 24 {
		return averages, fmt.Errorf("load average: sysctl returned %d bytes", len(raw))
	}
	scale := binary.LittleEndian.Uint64(raw[16:24])
	if scale == 0 {
		return averages, errors.New("load average: zero scale")
	}
	for i := range averages {
		averages[i] = float64(binary.LittleEndian.Uint32(raw[4*i:])) / float64(scale)
	}
	return averages, nil
}
