package gateway

import (
	"encoding/binary"
	"errors"

	"golang.org/x/sys/unix"
)

// One minute load average from the vm.loadavg sysctl, the getloadavg of macOS
func loadAverage1m() (float64, error) {
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil {
		return 0, err
	}
	if len(raw) < 24 {
		return 0, errors.New("Load averages are unobtainable")
	}
	scale := binary.LittleEndian.Uint64(raw[16:24])
	if scale == 0 {
		return 0, errors.New("Load averages are unobtainable")
	}
	return float64(binary.LittleEndian.Uint32(raw[0:4])) / float64(scale), nil
}
