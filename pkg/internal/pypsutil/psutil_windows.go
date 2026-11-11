package pypsutil

import (
	"errors"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
)

const (
	hiT                                 = 429.4967296
	loT                                 = 1e-7
	cpuIdle                             = 2
	systemProcessorPerformanceInfoClass = 8
	pdhFmtDouble                        = 0x00000200
	loadavgFactor1                      = 0.9200444146293232478931553241
	loadavgFactor5                      = 0.9834714538216174894737477501
	loadavgFactor15                     = 0.9944598480048967508795473394
	loadavgSamplingSeconds              = 5
	allProcessorGroups                  = 0xffff
)

var (
	modKernel32                     = windows.NewLazySystemDLL("kernel32.dll")
	modPsapi                        = windows.NewLazySystemDLL("psapi.dll")
	modPdh                          = windows.NewLazySystemDLL("pdh.dll")
	procGetSystemTimes              = modKernel32.NewProc("GetSystemTimes")
	procGetPerformanceInfo          = modPsapi.NewProc("GetPerformanceInfo")
	procPdhOpenQueryW               = modPdh.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounterW       = modPdh.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryDataEx       = modPdh.NewProc("PdhCollectQueryDataEx")
	procPdhGetFormattedCounterValue = modPdh.NewProc("PdhGetFormattedCounterValue")
)

type performanceInformation struct {
	cb                uint32
	commitTotal       uintptr
	commitLimit       uintptr
	commitPeak        uintptr
	physicalTotal     uintptr
	physicalAvailable uintptr
	systemCache       uintptr
	kernelTotal       uintptr
	kernelPaged       uintptr
	kernelNonpaged    uintptr
	pageSize          uintptr
	handleCount       uint32
	processCount      uint32
	threadCount       uint32
}

type processorPerformanceInformation struct {
	idleTime, kernelTime, userTime, dpcTime, interruptTime int64
	interruptCount                                         uint32
}

// Seconds of a 100 nanosecond tick count split into its high and low halves like psutil
func ticksSeconds(high int32, low uint32) float64 { return hiT*float64(high) + loT*float64(low) }

func filetimeSeconds(ft windows.Filetime) float64 {
	return hiT*float64(ft.HighDateTime) + loT*float64(ft.LowDateTime)
}

// Seconds of the host CPU in the order user system idle interrupt dpc, kernel time taken less idle
func cpuTimes() ([]float64, error) {
	var idle, kernel, user windows.Filetime
	if r, _, err := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user))); r == 0 {
		return nil, err
	}
	ncpus := windows.GetActiveProcessorCount(allProcessorGroups)
	if ncpus == 0 {
		return nil, errors.New("no logical CPUs detected")
	}
	infos := make([]processorPerformanceInformation, ncpus)
	size := uint32(uintptr(ncpus) * unsafe.Sizeof(processorPerformanceInformation{}))
	if err := windows.NtQuerySystemInformation(systemProcessorPerformanceInfoClass, unsafe.Pointer(&infos[0]), size, nil); err != nil {
		return nil, err
	}
	interrupt, dpc := 0.0, 0.0
	for _, info := range infos {
		interrupt += ticksSeconds(int32(info.interruptTime>>32), uint32(info.interruptTime))
		dpc += ticksSeconds(int32(info.dpcTime>>32), uint32(info.dpcTime))
	}
	idleSeconds := filetimeSeconds(idle)
	return []float64{filetimeSeconds(user), filetimeSeconds(kernel) - idleSeconds, idleSeconds, interrupt, dpc}, nil
}

// Total CPU time of a delta, every field counted
func cpuTotalTime(times []float64) float64 { return sumTimes(times) }

// Busy CPU time of a delta, the total less idle
func cpuBusyTime(times []float64) float64 { return cpuTotalTime(times) - times[cpuIdle] }

// Physical memory of the host from the performance information page counts the way psutil reads them
func VirtualMemory() (VirtualMemoryStats, error) {
	var info performanceInformation
	info.cb = uint32(unsafe.Sizeof(info))
	if r, _, err := procGetPerformanceInfo.Call(uintptr(unsafe.Pointer(&info)), uintptr(info.cb)); r == 0 {
		return VirtualMemoryStats{}, err
	}
	total := uint64(info.physicalTotal) * uint64(info.pageSize)
	avail := uint64(info.physicalAvailable) * uint64(info.pageSize)
	return VirtualMemoryStats{Total: total, Available: avail, Used: total - avail,
		Percent: usagePercent(float64(total)-float64(avail), float64(total))}, nil
}

// Size of the volume holding path with the used share of the whole volume as the percentage
func DiskUsage(path string) (DiskUsageStats, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return DiskUsageStats{}, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return DiskUsageStats{}, err
	}
	used := total - free
	return DiskUsageStats{Total: total, Used: used, Free: free, Percent: usagePercent(float64(used), float64(total))}, nil
}

var (
	loadavgMu     sync.Mutex
	loadavgInit   bool
	loadavgValues [3]float64
)

// Folds the current processor queue length into the three exponentially damped averages
func loadavgSample(counter uintptr) {
	var value struct {
		status uint32
		_      uint32
		double float64
	}
	if r, _, _ := procPdhGetFormattedCounterValue.Call(counter, pdhFmtDouble, 0, uintptr(unsafe.Pointer(&value))); r != 0 {
		return
	}
	loadavgMu.Lock()
	defer loadavgMu.Unlock()
	loadavgValues[0] = loadavgValues[0]*loadavgFactor1 + value.double*(1.0-loadavgFactor1)
	loadavgValues[1] = loadavgValues[1]*loadavgFactor5 + value.double*(1.0-loadavgFactor5)
	loadavgValues[2] = loadavgValues[2]*loadavgFactor15 + value.double*(1.0-loadavgFactor15)
}

// Starts the processor queue length counter that feeds the emulated load averages every five seconds
func initLoadavgCounter() error {
	var query, counter uintptr
	if r, _, _ := procPdhOpenQueryW.Call(0, 0, uintptr(unsafe.Pointer(&query))); r != 0 {
		return errors.New("PdhOpenQueryW failed")
	}
	path, err := windows.UTF16PtrFromString(`\System\Processor Queue Length`)
	if err != nil {
		return err
	}
	if r, _, _ := procPdhAddEnglishCounterW.Call(query, uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&counter))); r != 0 {
		return errors.New("PdhAddEnglishCounterW failed. Performance counters may be disabled.")
	}
	name, err := windows.UTF16PtrFromString("LoadUpdateEvent")
	if err != nil {
		return err
	}
	event, err := windows.CreateEvent(nil, 0, 0, name)
	if event == 0 {
		return err
	}
	if r, _, _ := procPdhCollectQueryDataEx.Call(query, loadavgSamplingSeconds, uintptr(event)); r != 0 {
		return errors.New("PdhCollectQueryDataEx failed")
	}
	go func() {
		for {
			if _, err := windows.WaitForSingleObject(event, windows.INFINITE); err != nil {
				return
			}
			loadavgSample(counter)
		}
	}()
	return nil
}

// Emulated one, five and fifteen minute load averages rounded to two decimals, the sampler started on first use
func Getloadavg() ([3]float64, error) {
	loadavgMu.Lock()
	if !loadavgInit {
		if err := initLoadavgCounter(); err != nil {
			loadavgMu.Unlock()
			return [3]float64{}, err
		}
		loadavgInit = true
	}
	values := loadavgValues
	loadavgMu.Unlock()
	for i := range values {
		values[i] = pyint.Round(values[i], 2)
	}
	return values, nil
}

// Logical CPUs active across every processor group, the os.cpu_count of CPython
func CPUCount() int {
	if n := windows.GetActiveProcessorCount(allProcessorGroups); n > 0 {
		return int(n)
	}
	return runtime.NumCPU()
}
