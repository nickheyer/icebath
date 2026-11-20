package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

const (
	// Dump a person asked for with /heapdump or at the start
	dumpManual = "manual"
	// Dump the memory watchdog wrote at its high level
	dumpAutoHigh = "auto-high"
	// Dump the memory watchdog wrote before it ended the process
	dumpAutoCritical = "auto-critical"
	// Bytes the dump folder may hold before its oldest dumps go, unless the environment sets another cap
	heapDumpCap = 2 << 30
	// Shortest run of the UI a growth rate is worked out over, the memory it takes to start counting as growth before that
	growthBase = time.Minute
	// File endings of a heap profile and of the memory facts written beside it
	heapProfileSuffix, diagnosticsSuffix = ".pprof", ".diagnostics.json"
)

// Moment the package was initialised, standing in for the start of the process
var processStarted = time.Now()

// Resident size at the moment the terminal UI started, the base memory growth is measured from
var memoryBaseline struct {
	rss uint64
	at  time.Time
}

// Runtime metrics a memory sample reads, in the order sampleMemory takes them
var memoryMetricNames = []string{"/gc/heap/live:bytes", "/gc/cycles/total:gc-cycles", "/memory/classes/heap/objects:bytes", "/memory/classes/total:bytes", "/memory/classes/heap/released:bytes"}

// Memory the process holds at one moment
type memorySnapshot struct {
	// Bytes of heap objects the last collection found live, the allocated bytes before the first collection ran
	heap uint64
	// Bytes of heap objects allocated and not yet swept
	heapUsed uint64
	// Bytes the Go runtime holds from the system
	held uint64
	// Resident set size, the bytes the runtime holds standing in where the system reports none
	rss uint64
}

// Reads the heap and resident sizes without stopping the program
func sampleMemory() memorySnapshot {
	samples := make([]metrics.Sample, len(memoryMetricNames))
	for i, name := range memoryMetricNames {
		samples[i].Name = name
	}
	metrics.Read(samples)
	value := func(i int) uint64 {
		if samples[i].Value.Kind() != metrics.KindUint64 {
			return 0
		}
		return samples[i].Value.Uint64()
	}
	snap := memorySnapshot{heap: value(0), heapUsed: value(2), held: value(3) - min(value(4), value(3)), rss: residentBytes()}
	if value(1) == 0 {
		snap.heap = snap.heapUsed
	}
	if snap.rss == 0 {
		snap.rss = snap.held
	}
	return snap
}

// Notes the resident size the terminal UI starts with
func markMemoryBaseline() {
	memoryBaseline.rss, memoryBaseline.at = sampleMemory().rss, time.Now()
}

// Peak resident size in bytes and the CPU times in microseconds the system accounts to the process
type processUsage struct {
	MaxRSS        uint64 `json:"maxRSS"`
	UserCPUTime   int64  `json:"userCPUTime"`
	SystemCPUTime int64  `json:"systemCPUTime"`
}

// Heap and runtime sizes of a memory dump
type diagnosticsUsage struct {
	RSS         uint64 `json:"rss"`
	Anonymous   uint64 `json:"anonymous,omitempty"`
	HeapLive    uint64 `json:"heapLive"`
	HeapUsed    uint64 `json:"heapUsed"`
	HeapTotal   uint64 `json:"heapTotal"`
	HeapObjects uint64 `json:"heapObjects"`
	Stacks      uint64 `json:"stacks"`
	Runtime     uint64 `json:"runtime"`
	Released    uint64 `json:"released"`
}

// Growth of the resident size since the terminal UI started
type diagnosticsGrowth struct {
	BytesPerSecond float64 `json:"bytesPerSecond"`
	MBPerHour      float64 `json:"mbPerHour"`
}

// State of the garbage collector in a memory dump
type diagnosticsCollector struct {
	Cycles        uint32  `json:"cycles"`
	PauseTotalNs  uint64  `json:"pauseTotalNs"`
	CPUFraction   float64 `json:"cpuFraction"`
	NextCycleHeap uint64  `json:"nextCycleHeap"`
	Percent       int64   `json:"percent"`
}

// Limits the process runs under and the sizes the watchdog acts at
type diagnosticsLimits struct {
	GoMemoryLimit    uint64 `json:"goMemoryLimit,omitempty"`
	CgroupLimit      uint64 `json:"cgroupLimit,omitempty"`
	PhysicalMemory   uint64 `json:"physicalMemory,omitempty"`
	HeapBudget       uint64 `json:"heapBudget"`
	BudgetSource     string `json:"budgetSource"`
	WatchdogWarn     uint64 `json:"watchdogWarn"`
	WatchdogHigh     uint64 `json:"watchdogHigh"`
	WatchdogCritical uint64 `json:"watchdogCritical"`
}

// Signs of a leak found in the numbers of a memory dump
type diagnosticsAnalysis struct {
	PotentialLeaks []string `json:"potentialLeaks"`
	Recommendation string   `json:"recommendation"`
}

// Memory facts written beside a heap profile, the numbers that explain a dump when the profile itself cannot be written
type memoryDiagnostics struct {
	Timestamp           string               `json:"timestamp"`
	Trigger             string               `json:"trigger"`
	Platform            string               `json:"platform"`
	GoVersion           string               `json:"goVersion"`
	PID                 int                  `json:"pid"`
	UptimeSeconds       float64              `json:"uptimeSeconds"`
	MemoryUsage         diagnosticsUsage     `json:"memoryUsage"`
	MemoryGrowthRate    diagnosticsGrowth    `json:"memoryGrowthRate"`
	Goroutines          int                  `json:"goroutines"`
	OpenFileDescriptors *int                 `json:"openFileDescriptors,omitempty"`
	ResourceUsage       *processUsage        `json:"resourceUsage,omitempty"`
	SmapsRollup         string               `json:"smapsRollup,omitempty"`
	Collector           diagnosticsCollector `json:"collector"`
	Limits              diagnosticsLimits    `json:"limits"`
	Analysis            diagnosticsAnalysis  `json:"analysis"`
}

// Gathers the memory facts of the process for a dump
func captureMemoryDiagnostics(trigger string) memoryDiagnostics {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	snap, now := sampleMemory(), time.Now()
	thresholds := resolveMemoryThresholds()
	d := memoryDiagnostics{
		Timestamp: now.UTC().Format("2006-01-02T15:04:05.000Z"), Trigger: trigger, Platform: runtime.GOOS + "/" + runtime.GOARCH, GoVersion: runtime.Version(),
		PID: os.Getpid(), UptimeSeconds: now.Sub(processStarted).Seconds(), Goroutines: runtime.NumGoroutine(), SmapsRollup: smapsRollup(),
		MemoryUsage: diagnosticsUsage{RSS: snap.rss, Anonymous: anonymousBytes(), HeapLive: snap.heap, HeapUsed: stats.HeapAlloc, HeapTotal: stats.HeapSys - stats.HeapReleased,
			HeapObjects: stats.HeapObjects, Stacks: stats.StackSys, Runtime: stats.Sys, Released: stats.HeapReleased},
		Collector: diagnosticsCollector{Cycles: stats.NumGC, PauseTotalNs: stats.PauseTotalNs, CPUFraction: stats.GCCPUFraction, NextCycleHeap: stats.NextGC, Percent: thresholds.percent},
		Limits: diagnosticsLimits{GoMemoryLimit: thresholds.goLimit, CgroupLimit: thresholds.cgroup, PhysicalMemory: thresholds.physical, HeapBudget: thresholds.budget,
			BudgetSource: thresholds.source, WatchdogWarn: thresholds.warn, WatchdogHigh: thresholds.high, WatchdogCritical: thresholds.critical},
	}
	if n, ok := openDescriptors(); ok {
		d.OpenFileDescriptors = &n
	}
	if usage, ok := resourceUsage(); ok {
		d.ResourceUsage = &usage
	}
	// Growth since the UI started, never the lifetime average of resident size over uptime, which reads as growth for a steady process,
	// and none before the UI ran long enough for its start to stop weighing on the rate
	if elapsed := now.Sub(memoryBaseline.at); !memoryBaseline.at.IsZero() && elapsed >= growthBase {
		d.MemoryGrowthRate.BytesPerSecond = (float64(snap.rss) - float64(memoryBaseline.rss)) / elapsed.Seconds()
		d.MemoryGrowthRate.MBPerHour = d.MemoryGrowthRate.BytesPerSecond * 3600 / (1 << 20)
	}
	leaks := []string{}
	if d.Goroutines > 1000 {
		leaks = append(leaks, fmt.Sprintf("%d goroutines — possible goroutine leak", d.Goroutines))
	}
	// Only memory no file backs is compared, the pages of the program file itself being resident without being held
	if anon, held := anonymousBytes(), stats.Sys-stats.HeapReleased; anon > held && anon-held > stats.HeapAlloc {
		leaks = append(leaks, "Memory outside the Go runtime > heap — the growth may be in mappings or C allocations")
	}
	if d.MemoryGrowthRate.MBPerHour > 100 {
		leaks = append(leaks, fmt.Sprintf("High memory growth rate: %.1f MB/hour", d.MemoryGrowthRate.MBPerHour))
	}
	if d.OpenFileDescriptors != nil && *d.OpenFileDescriptors > 500 {
		leaks = append(leaks, fmt.Sprintf("%d open FDs — possible file/socket leak", *d.OpenFileDescriptors))
	}
	d.Analysis = diagnosticsAnalysis{PotentialLeaks: leaks, Recommendation: "No obvious leak indicators. Inspect the heap profile for retained objects."}
	if len(leaks) > 0 {
		d.Analysis.Recommendation = fmt.Sprintf("WARNING: %d potential leak indicator(s). See potentialLeaks.", len(leaks))
	}
	return d
}

// Outcome of a memory dump, the profile path empty when an automatic dump wrote the facts alone
type heapDumpResult struct {
	heapPath   string
	diagPath   string
	suppressed bool
	err        error
}

// Folder the memory dumps go to, the one the environment names or the heapdumps folder of the home
func heapDumpDir() string {
	if dir := strings.TrimSpace(os.Getenv("ICEBATH_HEAPDUMP_DIR")); dir != "" {
		return dir
	}
	return filepath.Join(home.Process(), "heapdumps")
}

// Whether the environment allows the watchdog to write whole heap profiles and not the facts alone
func autoHeapDumpEnabled() bool {
	return trueRe.MatchString(strings.ToLower(strings.TrimSpace(os.Getenv("ICEBATH_AUTO_HEAPDUMP"))))
}

// Writes the memory facts and then the heap profile of the process, an automatic dump stopping after the facts unless the environment asks for profiles
func performHeapDump(trigger string) heapDumpResult {
	// The facts go first: collecting a very large heap for its profile can fail, and the facts are what explains the dump then
	diagnostics := captureMemoryDiagnostics(trigger)
	dir := heapDumpDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return heapDumpResult{err: err}
	}
	stamp := strings.NewReplacer(":", "-", ".", "-").Replace(diagnostics.Timestamp)
	base := filepath.Join(dir, fmt.Sprintf("icebath-%s-%d-%s", stamp, os.Getpid(), trigger))
	heapPath, diagPath := base+heapProfileSuffix, base+diagnosticsSuffix
	body, err := json.MarshalIndent(diagnostics, "", "  ")
	if err == nil {
		err = os.WriteFile(diagPath, body, 0o600)
	}
	if err != nil {
		return heapDumpResult{err: err}
	}
	// Profiles written at every threshold crossing can fill a disk, so the watchdog writes them only when asked to
	if trigger != dumpManual && !autoHeapDumpEnabled() {
		pruneHeapDumps(dir)
		return heapDumpResult{diagPath: diagPath, suppressed: true}
	}
	if err := writeHeapProfile(heapPath); err != nil {
		return heapDumpResult{err: err}
	}
	pruneHeapDumps(dir)
	return heapDumpResult{heapPath: heapPath, diagPath: diagPath}
}

// Writes the heap profile of the process after a collection, so it shows what is live and not what waits to be swept
func writeHeapProfile(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	runtime.GC()
	return errors.Join(pprof.WriteHeapProfile(file), file.Close())
}

// Whether a file of the dump folder is a profile or a facts file this program wrote
func isDumpArtifact(name string) bool {
	return strings.HasPrefix(name, "icebath-") && (strings.HasSuffix(name, heapProfileSuffix) || strings.HasSuffix(name, diagnosticsSuffix))
}

// Bytes the dump folder may hold, the cap of the environment when it names a positive number
func heapDumpLimit() int64 {
	n, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv("ICEBATH_HEAPDUMP_MAX_BYTES")), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
		return heapDumpCap
	}
	return int64(min(n, math.MaxInt64/2))
}

// Deletes the oldest dumps of the folder until what is left fits the cap, the newest always kept and files of others never touched
func pruneHeapDumps(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type dumpFile struct {
		path     string
		size     int64
		modified time.Time
	}
	var files []dumpFile
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || !isDumpArtifact(entry.Name()) {
			continue
		}
		files = append(files, dumpFile{path: filepath.Join(dir, entry.Name()), size: info.Size(), modified: info.ModTime()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modified.After(files[j].modified) })
	for limit := heapDumpLimit(); total > limit && len(files) > 1; files = files[:len(files)-1] {
		oldest := files[len(files)-1]
		// A failed delete is passed over, the loop going on to the next oldest dump
		_ = os.Remove(oldest.path)
		total -= oldest.size
	}
}
