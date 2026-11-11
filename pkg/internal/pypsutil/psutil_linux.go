package pypsutil

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
)

const (
	cpuFieldCount = 10
	cpuIdle       = 3
	cpuIOWait     = 4
	cpuGuest      = 8
	cpuGuestNice  = 9
)

// Seconds of the aggregate cpu line of /proc/stat in the order user nice system idle iowait irq softirq steal guest guest_nice
func cpuTimes() ([]float64, error) {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil, err
	}
	line, _, _ := bytes.Cut(raw, []byte("\n"))
	values := strings.Fields(string(line))
	if len(values) < cpuFieldCount+1 {
		return nil, fmt.Errorf("TypeError: scputimes.__new__() missing %d required positional arguments", cpuFieldCount+1-len(values))
	}
	ticks := proc.ClockTicks()
	out := make([]float64, cpuFieldCount)
	for i := range out {
		v, err := strconv.ParseFloat(values[i+1], 64)
		if err != nil {
			return nil, err
		}
		out[i] = v / ticks
	}
	return out, nil
}

// Total CPU time of a delta, guest time left out because user time already holds it
func cpuTotalTime(times []float64) float64 {
	return sumTimes(times) - times[cpuGuest] - times[cpuGuestNice]
}

// Busy CPU time of a delta, the total less idle and iowait
func cpuBusyTime(times []float64) float64 {
	return cpuTotalTime(times) - times[cpuIdle] - times[cpuIOWait]
}

// Estimate of available memory from the page cache and reclaimable slab less the low watermarks of every zone
func calculateAvailVMem(mems map[string]int64) int64 {
	free := mems["MemFree:"]
	fallback := free + mems["Cached:"]
	activeFile, okActive := mems["Active(file):"]
	inactiveFile, okInactive := mems["Inactive(file):"]
	slabReclaimable, okSlab := mems["SReclaimable:"]
	if !okActive || !okInactive || !okSlab {
		return fallback
	}
	f, err := os.Open("/proc/zoneinfo")
	if err != nil {
		return fallback
	}
	defer f.Close()
	var watermarkLow int64
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if strings.HasPrefix(line, "low") {
			if fields := strings.Fields(line); len(fields) > 1 {
				n, _ := strconv.ParseInt(fields[1], 10, 64)
				watermarkLow += n
			}
		}
	}
	low := float64(watermarkLow * int64(os.Getpagesize()))
	avail := float64(free) - low
	pagecache := float64(activeFile + inactiveFile)
	pagecache -= min(pagecache/2, low)
	avail += pagecache
	avail += float64(slabReclaimable) - min(float64(slabReclaimable)/2.0, low)
	return int64(avail)
}

// Physical memory of the host from /proc/meminfo, matching the free command the way psutil does
func VirtualMemory() (VirtualMemoryStats, error) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return VirtualMemoryStats{}, err
	}
	mems := map[string]int64{}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		n, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return VirtualMemoryStats{}, err
		}
		mems[fields[0]] = n * 1024
	}
	total, okTotal := mems["MemTotal:"]
	free, okFree := mems["MemFree:"]
	if !okTotal || !okFree {
		return VirtualMemoryStats{}, errors.New("KeyError: b'MemTotal:'")
	}
	var missing []string
	if _, ok := mems["Buffers:"]; !ok {
		missing = append(missing, "buffers")
	}
	if _, ok := mems["Cached:"]; !ok {
		missing = append(missing, "cached")
	}
	_, hasShmem := mems["Shmem:"]
	_, hasMemShared := mems["MemShared:"]
	if !hasShmem && !hasMemShared {
		missing = append(missing, "shared")
	}
	if _, ok := mems["Active:"]; !ok {
		missing = append(missing, "active")
	}
	if _, ok := mems["Inactive:"]; !ok {
		_, dirty := mems["Inact_dirty:"]
		_, clean := mems["Inact_clean:"]
		_, laundry := mems["Inact_laundry:"]
		if !dirty || !clean || !laundry {
			missing = append(missing, "inactive")
		}
	}
	avail, ok := mems["MemAvailable:"]
	if !ok || avail == 0 {
		avail = calculateAvailVMem(mems)
	}
	if avail < 0 {
		avail = 0
		missing = append(missing, "available")
	} else if avail > total {
		avail = free
	}
	if len(missing) > 0 {
		verb := "were"
		if len(missing) == 1 {
			verb = "was"
		}
		slog.Default().Warn(fmt.Sprintf("%s memory stats couldn't be determined and %s set to 0", strings.Join(missing, ", "), verb))
	}
	return VirtualMemoryStats{Total: uint64(total), Available: uint64(avail), Used: uint64(total - avail),
		Percent: usagePercent(float64(total-avail), float64(total))}, nil
}

// One, five and fifteen minute load averages from /proc/loadavg
func Getloadavg() ([3]float64, error) {
	var out [3]float64
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return out, errors.New("Load averages are unobtainable")
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 3 {
		return out, errors.New("Load averages are unobtainable")
	}
	for i := range out {
		if out[i], err = strconv.ParseFloat(fields[i], 64); err != nil {
			return out, errors.New("Load averages are unobtainable")
		}
	}
	return out, nil
}

// Size of the volume holding path with the share a user can no longer fill as the percentage
func DiskUsage(path string) (DiskUsageStats, error) {
	total, used, free, err := pyfs.DiskUsage(path)
	if err != nil {
		return DiskUsageStats{}, err
	}
	return DiskUsageStats{Total: total, Used: used, Free: free, Percent: usagePercent(float64(used), float64(used+free))}, nil
}

// Logical CPUs the kernel has online, the os.cpu_count of CPython
func CPUCount() int {
	raw, err := os.ReadFile("/sys/devices/system/cpu/online")
	if err != nil {
		return runtime.NumCPU()
	}
	count := 0
	for _, part := range strings.Split(strings.TrimSpace(string(raw)), ",") {
		lo, hi, ranged := strings.Cut(part, "-")
		first, err := strconv.Atoi(lo)
		if err != nil {
			return runtime.NumCPU()
		}
		last := first
		if ranged {
			if last, err = strconv.Atoi(hi); err != nil {
				return runtime.NumCPU()
			}
		}
		count += last - first + 1
	}
	if count <= 0 {
		return runtime.NumCPU()
	}
	return count
}
