package hostinfo

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	cpuInfoPath      = "/proc/cpuinfo"
	cpuInfoLimit     = 65536
	deviceTreeModel  = "/proc/device-tree/model"
	deviceTreeLimit  = 512
	onlineCPUsPath   = "/sys/devices/system/cpu/online"
	procStatPath     = "/proc/stat"
	cpuStatFields    = 10
	cpuStatIdle      = 3
	cpuStatIOWait    = 4
	cpuStatGuest     = 8
	cpuStatGuestNice = 9
)

var armImplementers = map[int64]string{
	0x41: "ARM", 0x42: "Broadcom", 0x43: "Cavium", 0x46: "Fujitsu", 0x48: "HiSilicon", 0x4E: "NVIDIA",
	0x50: "Ampere", 0x51: "Qualcomm", 0x61: "Apple", 0x69: "Intel", 0x6D: "Microsoft", 0xC0: "Ampere",
}

// Model name or Hardware line of cpuinfo, else the device tree model
func cpuModel() string {
	text := readText(cpuInfoPath, cpuInfoLimit)
	for _, key := range []string{"model name", "hardware"} {
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(strings.ToLower(line), key) && strings.Contains(line, ":") {
				_, value, _ := strings.Cut(line, ":")
				return strings.TrimSpace(value)
			}
		}
	}
	model := readText(deviceTreeModel, deviceTreeLimit)
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(model, "\x00", ""), "_", " "))
}

// Vendor id on x86 or the named implementer of arm cores from cpuinfo, the last entry winning
func cpuVendor(*Host) string {
	fields := map[string]string{}
	for _, line := range strings.Split(readText(cpuInfoPath, cpuInfoLimit), "\n") {
		if key, value, found := strings.Cut(line, ":"); found {
			fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	if fields["vendor_id"] != "" {
		return fields["vendor_id"]
	}
	raw := strings.TrimPrefix(strings.TrimPrefix(fields["CPU implementer"], "0x"), "0X")
	code, err := strconv.ParseInt(raw, 16, 64)
	if err != nil {
		return ""
	}
	return armImplementers[code]
}

// Logical processors the kernel has online, from the online list or the per CPU lines of the stat file
func CPUCount() (int, error) {
	if data, err := os.ReadFile(onlineCPUsPath); err == nil {
		if count, ok := countCPUList(strings.TrimSpace(string(data))); ok {
			return count, nil
		}
	}
	data, err := os.ReadFile(procStatPath)
	if err != nil {
		return 0, fmt.Errorf("online processors: %w", err)
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if name, _, _ := strings.Cut(line, " "); len(name) > 3 && strings.HasPrefix(name, "cpu") {
			if _, err := strconv.Atoi(name[3:]); err == nil {
				count++
			}
		}
	}
	if count == 0 {
		return 0, errors.New("online processors: stat file lists no processor")
	}
	return count, nil
}

// Number of processors in a kernel CPU list such as 0-3,8-11, false when it does not parse
func countCPUList(list string) (int, bool) {
	count := 0
	for _, part := range strings.Split(list, ",") {
		lo, hi, ranged := strings.Cut(part, "-")
		first, err := strconv.Atoi(lo)
		if err != nil {
			return 0, false
		}
		last := first
		if ranged {
			if last, err = strconv.Atoi(hi); err != nil || last < first {
				return 0, false
			}
		}
		count += last - first + 1
	}
	return count, count > 0
}

// Processors this process may run on under its affinity mask, at least one
func UsableCPUs() int {
	for size := 1024; size <= 1<<20; size *= 2 {
		set := unix.NewCPUSet(size)
		err := unix.SchedGetaffinityDynamic(0, set)
		if err == nil {
			return max(1, set.Count())
		}
		if !errors.Is(err, unix.EINVAL) {
			break
		}
	}
	count, err := CPUCount()
	if err != nil {
		return 1
	}
	return max(1, count)
}

// Seconds of the aggregate line of the stat file from user through guest_nice
func cpuTimes() ([]float64, error) {
	data, err := os.ReadFile(procStatPath)
	if err != nil {
		return nil, fmt.Errorf("cpu times: %w", err)
	}
	line, _, _ := strings.Cut(string(data), "\n")
	fields := strings.Fields(line)
	if len(fields) < cpuStatFields+1 || fields[0] != "cpu" {
		return nil, fmt.Errorf("cpu times: aggregate line has %d fields", len(fields))
	}
	times := make([]float64, cpuStatFields)
	for i := range times {
		if times[i], err = strconv.ParseFloat(fields[i+1], 64); err != nil {
			return nil, fmt.Errorf("cpu times: %w", err)
		}
	}
	return times, nil
}

// Total time less guest time already counted in user, busy time less idle and iowait
func cpuTotalBusy(d []float64) (total, busy float64) {
	for _, v := range d {
		total += v
	}
	total -= d[cpuStatGuest] + d[cpuStatGuestNice]
	return total, total - d[cpuStatIdle] - d[cpuStatIOWait]
}
