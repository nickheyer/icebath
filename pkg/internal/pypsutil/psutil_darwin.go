package pypsutil

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
)

const (
	hostCPULoadInfo      = 3
	hostCPULoadInfoCount = 4
	hostVMInfo64         = 4
	hostVMInfo64Count    = 38
	clockTicks           = 100.0
	cpuStateUser         = 0
	cpuStateSystem       = 1
	cpuStateIdle         = 2
	cpuStateNice         = 3
	cpuIdle              = 3
)

// Fills buf through a mach host statistics call on a fresh host port that is released afterwards
func hostStatistics(symbol string, flavor int32, buf []byte, count uint32) error {
	hostSelf, err := purego.Dlsym(purego.RTLD_DEFAULT, "mach_host_self")
	if err != nil {
		return err
	}
	call, err := purego.Dlsym(purego.RTLD_DEFAULT, symbol)
	if err != nil {
		return err
	}
	deallocate, err := purego.Dlsym(purego.RTLD_DEFAULT, "mach_port_deallocate")
	if err != nil {
		return err
	}
	taskSelf, err := purego.Dlsym(purego.RTLD_DEFAULT, "mach_task_self_")
	if err != nil {
		return err
	}
	port, _, _ := purego.SyscallN(hostSelf)
	if uint32(port) == 0 {
		return errors.New("mach_host_self() returned MACH_PORT_NULL")
	}
	ret, _, _ := purego.SyscallN(call, port, uintptr(flavor), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&count)))
	purego.SyscallN(deallocate, uintptr(**(**uint32)(unsafe.Pointer(&taskSelf))), port)
	runtime.KeepAlive(buf)
	if int32(ret) != 0 {
		return fmt.Errorf("%s syscall failed: kern_return %d", symbol, int32(ret))
	}
	return nil
}

// Seconds of the host CPU load in the order user nice system idle
func cpuTimes() ([]float64, error) {
	buf := make([]byte, 4*hostCPULoadInfoCount)
	if err := hostStatistics("host_statistics", hostCPULoadInfo, buf, hostCPULoadInfoCount); err != nil {
		return nil, err
	}
	tick := func(state int) float64 { return float64(binary.LittleEndian.Uint32(buf[4*state:])) / clockTicks }
	return []float64{tick(cpuStateUser), tick(cpuStateNice), tick(cpuStateSystem), tick(cpuStateIdle)}, nil
}

// Total CPU time of a delta, every field counted
func cpuTotalTime(times []float64) float64 { return sumTimes(times) }

// Busy CPU time of a delta, the total less idle
func cpuBusyTime(times []float64) float64 { return cpuTotalTime(times) - times[cpuIdle] }

// Physical memory of the host from hw.memsize and the mach VM page counts the way psutil reads them
func VirtualMemory() (VirtualMemoryStats, error) {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return VirtualMemoryStats{}, err
	}
	buf := make([]byte, 4*hostVMInfo64Count)
	if err := hostStatistics("host_statistics64", hostVMInfo64, buf, hostVMInfo64Count); err != nil {
		return VirtualMemoryStats{}, err
	}
	pagesize := uint64(os.Getpagesize())
	pages := func(offset int) uint64 { return uint64(binary.LittleEndian.Uint32(buf[offset:])) * pagesize }
	free, active, inactive, wired := pages(0), pages(4), pages(8), pages(12)
	avail := inactive + free
	return VirtualMemoryStats{Total: total, Available: avail, Used: active + wired,
		Percent: usagePercent(float64(total)-float64(avail), float64(total))}, nil
}

// One, five and fifteen minute load averages from the vm.loadavg sysctl
func Getloadavg() ([3]float64, error) {
	var out [3]float64
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil || len(raw) < 24 {
		return out, errors.New("Load averages are unobtainable")
	}
	scale := binary.LittleEndian.Uint64(raw[16:24])
	if scale == 0 {
		return out, errors.New("Load averages are unobtainable")
	}
	for i := range out {
		out[i] = float64(binary.LittleEndian.Uint32(raw[4*i:])) / float64(scale)
	}
	return out, nil
}

type volumeAttrList struct {
	bitmapCount uint16
	reserved    uint16
	commonAttr  uint32
	volAttr     uint32
	dirAttr     uint32
	fileAttr    uint32
	forkAttr    uint32
}

// Used bytes of the volume holding path from its space used attribute, fallback when the volume has none
func diskUsageUsed(path string, fallback uint64) uint64 {
	p, err := unix.BytePtrFromString(path)
	if err != nil {
		return fallback
	}
	attrs := volumeAttrList{bitmapCount: unix.ATTR_BIT_MAP_COUNT, volAttr: unix.ATTR_VOL_INFO | unix.ATTR_VOL_SPACEUSED}
	var buf [12]byte
	_, _, errno := unix.Syscall6(unix.SYS_GETATTRLIST, uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&attrs)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0)
	runtime.KeepAlive(p)
	if errno != 0 {
		return fallback
	}
	return binary.LittleEndian.Uint64(buf[4:])
}

// Size of the volume holding path with the share a user can no longer fill as the percentage
func DiskUsage(path string) (DiskUsageStats, error) {
	total, used, free, err := pyfs.DiskUsage(path)
	if err != nil {
		return DiskUsageStats{}, err
	}
	used = diskUsageUsed(path, used)
	return DiskUsageStats{Total: total, Used: used, Free: free, Percent: usagePercent(float64(used), float64(used+free))}, nil
}

// Logical CPUs the kernel has active, the os.cpu_count of CPython
func CPUCount() int {
	if n, err := unix.SysctlUint32("hw.activecpu"); err == nil && n > 0 {
		return int(n)
	}
	return runtime.NumCPU()
}
