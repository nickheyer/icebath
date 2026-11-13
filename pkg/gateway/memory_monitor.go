package gateway

import (
	"fmt"
	"runtime"
	"runtime/metrics"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/logging"
)

const memoryBytesToMB = 1024 * 1024

var (
	memoryMonitorLogger = logging.GetLogger("gateway.memory_monitor")
	memoryMonitorMu     sync.Mutex
	memoryMonitorStop   chan struct{}
	memoryMonitorDone   chan struct{}
	memoryMonitorStart  atomic.Pointer[time.Time]
)

// Process RSS high water mark in MB, false when the process memory cannot be read
var processRSSMB = readProcessRSSMB

// Completed Go garbage collection cycles of the process
func gcCycles() uint64 {
	sample := []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}}
	metrics.Read(sample)
	if sample[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return sample[0].Value.Uint64()
}

// Logs one MEMORY line with RSS, GC cycles, goroutines and uptime since the last start
func LogMemoryUsage(prefix string) {
	rss := "unavailable"
	if mb, ok := processRSSMB(); ok {
		rss = fmt.Sprintf("%dMB", mb)
	}
	if prefix != "" {
		prefix += " "
	}
	uptime := 0
	if start := memoryMonitorStart.Load(); start != nil {
		uptime = int(time.Since(*start).Seconds())
	}
	memoryMonitorLogger.Info(fmt.Sprintf("[MEMORY] %srss=%s gc=%d goroutines=%d uptime=%ds", prefix, rss, gcCycles(), runtime.NumGoroutine(), uptime))
}

// Logs every interval until stop closes, a failed iteration logged at debug and never ending the loop
func memoryMonitorLoop(stop <-chan struct{}, done chan<- struct{}, interval time.Duration) {
	defer close(done)
	for {
		timer := time.NewTimer(interval)
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					memoryMonitorLogger.Debug(fmt.Sprintf("Memory monitor iteration failed: %v", r))
				}
			}()
			LogMemoryUsage("")
		}()
	}
}

// Whether the monitor goroutine of the last start is still running
func memoryMonitorAlive() bool {
	if memoryMonitorDone == nil {
		return false
	}
	select {
	case <-memoryMonitorDone:
		return false
	default:
		return true
	}
}

// Starts periodic logging after a baseline line, false when already running or RSS cannot be read
func StartMemoryMonitoring(intervalSeconds float64) bool {
	memoryMonitorMu.Lock()
	defer memoryMonitorMu.Unlock()
	if memoryMonitorAlive() {
		return false
	}
	if _, ok := processRSSMB(); !ok {
		memoryMonitorLogger.Warning("[MEMORY] Memory monitoring unavailable: neither getrusage nor GetProcessMemoryInfo could read process RSS " +
			"— skipping periodic logging.")
		return false
	}
	now := time.Now()
	memoryMonitorStart.Store(&now)
	memoryMonitorStop = make(chan struct{})
	LogMemoryUsage("baseline")
	memoryMonitorDone = make(chan struct{})
	go memoryMonitorLoop(memoryMonitorStop, memoryMonitorDone, time.Duration(intervalSeconds*float64(time.Second)))
	memoryMonitorLogger.Info(fmt.Sprintf("[MEMORY] Periodic memory monitoring started (interval: %ds)", int(intervalSeconds)))
	return true
}

// Stops the monitor after a shutdown line and waits up to timeout for it, nothing when never started
func StopMemoryMonitoring(timeoutSeconds float64) {
	memoryMonitorMu.Lock()
	if memoryMonitorStop == nil || memoryMonitorDone == nil {
		memoryMonitorMu.Unlock()
		return
	}
	func() {
		defer func() { _ = recover() }()
		LogMemoryUsage("shutdown")
	}()
	close(memoryMonitorStop)
	done := memoryMonitorDone
	memoryMonitorStop, memoryMonitorDone = nil, nil
	memoryMonitorMu.Unlock()
	timer := time.NewTimer(time.Duration(timeoutSeconds * float64(time.Second)))
	select {
	case <-done:
	case <-timer.C:
	}
	timer.Stop()
	memoryMonitorLogger.Info("[MEMORY] Periodic memory monitoring stopped")
}

// Whether the background monitor is running
func MemoryMonitorRunning() bool {
	memoryMonitorMu.Lock()
	defer memoryMonitorMu.Unlock()
	return memoryMonitorAlive()
}
