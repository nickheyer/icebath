package hostinfo

import (
	"encoding/binary"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

const cpuStateIdle = 2

// CPU brand string the kernel reports
func cpuModel() string {
	brand, err := unix.Sysctl("machdep.cpu.brand_string")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(brand)
}

// Apple on Apple silicon, else the vendor string of the Intel CPU
func cpuVendor(h *Host) string {
	if h.NativeArch() == ArchARM64 {
		return "Apple"
	}
	vendor, err := unix.Sysctl("machdep.cpu.vendor")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(vendor)
}

// Logical processors the kernel has active
func CPUCount() (int, error) {
	count, err := unix.SysctlUint32("hw.activecpu")
	if err != nil {
		return 0, fmt.Errorf("hw.activecpu: %w", err)
	}
	return int(count), nil
}

// Active processors, since macOS has no affinity mask, at least one
func UsableCPUs() int {
	count, err := CPUCount()
	if err != nil {
		return 1
	}
	return max(1, count)
}

// Ticks of the host CPU load in the order user system idle nice
func cpuTimes() ([]float64, error) {
	buf, err := hostStatistics("host_statistics", hostCPULoadInfo, hostCPULoadInfoCount)
	if err != nil {
		return nil, fmt.Errorf("cpu times: %w", err)
	}
	if len(buf) < 4*hostCPULoadInfoCount {
		return nil, fmt.Errorf("cpu times: host returned %d bytes", len(buf))
	}
	ticks := make([]float64, hostCPULoadInfoCount)
	for i := range ticks {
		ticks[i] = float64(binary.LittleEndian.Uint32(buf[4*i:]))
	}
	return ticks, nil
}

// Every state counted in the total, busy time less idle
func cpuTotalBusy(d []float64) (total, busy float64) {
	for _, v := range d {
		total += v
	}
	return total, total - d[cpuStateIdle]
}
