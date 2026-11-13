package hostinfo

import "errors"

// One reading of physical memory with Linux MemAvailable and swap, macOS purgeable pages, Windows commit
type Memory struct {
	Total           uint64
	Available       uint64
	Used            uint64
	Free            uint64
	KernelAvailable *uint64
	Purgeable       *uint64
	Swap            *Swap
	Commit          *Commit
}

// Size of the paging space and the part still free
type Swap struct {
	Total uint64
	Free  uint64
}

// Windows commit limit and the commit new allocations may still take
type Commit struct {
	Limit     uint64
	Available uint64
}

// Total and free physical memory in the kernel counters sysinfo reports, which a container meminfo view leaves unchanged
type KernelMemory struct {
	Total uint64
	Free  uint64
}

// Share of physical memory not available to new programs in percent, rounded to one decimal, zero without a total
func (m Memory) Percent() float64 {
	if m.Total == 0 {
		return 0
	}
	return roundDecimals((float64(m.Total)-float64(m.Available))/float64(m.Total)*100, 1)
}

// Physical memory this OS manages, read once
func (h *Host) RAMTotal() (uint64, error) { return h.ramTotal.get(physicalTotal) }

var errNoPhysicalMemory = errors.New("memory: the OS reports no physical memory")

// Memory controller state of one cgroup, each value nil when unreadable or unlimited
type CgroupMemory struct {
	High         *uint64
	Max          *uint64
	Current      *uint64
	Anon         *uint64
	InactiveFile *uint64
}
