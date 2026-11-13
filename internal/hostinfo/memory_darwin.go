package hostinfo

import (
	"encoding/binary"
	"fmt"

	"golang.org/x/sys/unix"
)

const (
	vmFreeOffset        = 0
	vmActiveOffset      = 4
	vmInactiveOffset    = 8
	vmWiredOffset       = 12
	vmPurgeableOffset   = 88
	vmSpeculativeOffset = 92
)

// Physical memory from hw.memsize and the mach VM page counts, free less speculative pages
func ReadMemory() (Memory, error) {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return Memory{}, fmt.Errorf("hw.memsize: %w", err)
	}
	page, err := kernelPageSize()
	if err != nil {
		return Memory{}, fmt.Errorf("memory: %w", err)
	}
	buf, err := hostStatistics("host_statistics64", hostVMInfo64, hostVMInfo64Count)
	if err != nil {
		return Memory{}, fmt.Errorf("memory: %w", err)
	}
	if len(buf) < vmSpeculativeOffset+4 {
		return Memory{}, fmt.Errorf("memory: host returned %d bytes", len(buf))
	}
	pages := func(offset int) uint64 { return uint64(binary.LittleEndian.Uint32(buf[offset:])) * page }
	free, speculative, purgeable := pages(vmFreeOffset), pages(vmSpeculativeOffset), pages(vmPurgeableOffset)
	return Memory{
		Total:     total,
		Available: free + pages(vmInactiveOffset),
		Used:      pages(vmActiveOffset) + pages(vmWiredOffset),
		Free:      free - min(speculative, free),
		Purgeable: &purgeable,
	}, nil
}

// Physical memory size the kernel reports in hw.memsize
func physicalTotal() (uint64, error) {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, fmt.Errorf("hw.memsize: %w", err)
	}
	if total == 0 {
		return 0, errNoPhysicalMemory
	}
	return total, nil
}
