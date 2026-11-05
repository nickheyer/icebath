package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
)

const (
	hwGiB                          = 1 << 30
	hwMarginFloor            int64 = 2 << 30
	hwMarginFraction               = 0.09
	hwLaunchHeadroom         int64 = 1 << 30
	hwUMAHeadroomFraction          = 0.20
	hwPromptCacheMax         int64 = 8 << 30
	hwPromptCacheMin         int64 = 512 << 20
	hwPoolDisagreement             = 1.5
	hwPoolRAMFraction              = 0.75
	hwCUDeviceIntegrated           = 18
	hwNvidiaVendorID               = 0x10DE
	hwPoolNegativeTTLSeconds       = 60.0
	hwGPUQueryTTLSeconds           = 4.0
	hwCSVFieldLimit                = 131072
	hwGPUQueryArg                  = "--query-gpu=memory.total,memory.free,name,pci.device_id,memory.used,utilization.gpu"
)

var (
	hwGPUEngineBackends = []string{"vulkan", "hip"}
	hwPySpace           = `[\t\n\v\f\r \x1c-\x1f\x{85}\p{Z}]`
	hwDeviceLineRE      = regexp.MustCompile(`CUDA\p{Nd}+:.*\((\p{Nd}+)` + hwPySpace + `*MiB,` + hwPySpace + `*\p{Nd}+` +
		hwPySpace + `*MiB free\)` + hwPySpace + `*$`)
	hwPageSizeRE = regexp.MustCompile(`page size of (\p{Nd}+)`)
	hwLinuxRAM   = map[string]bool{"MemTotal": true, "MemAvailable": true, "MemFree": true}
)

// Python exception that escaped every handler of the probe, Class names the exception type
type HardwareProbeError struct {
	Class string
	Msg   string
}

func (e *HardwareProbeError) Error() string { return e.Class + ": " + e.Msg }

// Exit status and decoded output of one probe subprocess
type hwRunResult struct {
	ReturnCode int
	Stdout     string
	Stderr     string
}

// Allocator side view of the first CUDA device, Integrated nil when the driver gave no verdict
type hwDevicePool struct {
	Total      int64
	Integrated *bool
}

// One nvidia-smi reading shared by the budget probe and the hardware endpoint
type NvidiaGPUQuery struct {
	GPUName        string
	TotalBytes     int64
	FreeBytes      int64
	UsedBytes      int64
	GPUUtilPercent int64
	GPUPCIID       *int64
}

// The physical memory counters GlobalMemoryStatusEx reports
type windowsMemoryStatusEx struct {
	TotalPhys     uint64
	AvailPhys     uint64
	TotalPageFile uint64
	AvailPageFile uint64
}

type hwTimedPool struct {
	stamp float64
	view  *hwDevicePool
}

type hwTimedDevice struct {
	stamp  float64
	device *GGMLDevice
}

type hwTimedQuery struct {
	stamp float64
	data  *NvidiaGPUQuery
}

var hwStart = time.Now()

// Seams the probe reads through so tests replace any one host answer
var (
	hwMonotonic           = func() float64 { return time.Since(hwStart).Seconds() }
	hwSysPlatform         = hostinfo.OSFamily
	hwWindowsMemoryStatus = windowsMemoryStatus
	hwLinuxMeminfoText    = linuxMeminfoText
	hwStdout              = hwRunStdout
	hwRun                 = hwRunCommand
	hwWhich               = exec.LookPath
	hwPathExists          = func(p string) bool { _, err := os.Stat(p); return err == nil }
	hwNvidiaSMIPath       = nvidiaSMIPath
	hwNvidiaVRAM          = nvidiaVRAM
	hwCudaDriverPool      = cudaDriverPool
	hwEngineDevicePool    = engineDevicePool
	hwDevicePoolView      = devicePoolView
	hwUnifiedPoolBytes    = unifiedPoolBytes
	hwRAMBytes            = ramBytes
	hwAcceleratorDevice   = acceleratorDevice
	hwConfiguredEngine    = configuredEngine
	hwServingEngine       = func() *Engine { return servingEngine() }
	hwInstalledEngine     = func(backend string) (*Engine, error) { return InstalledEngine(context.Background(), backend, true) }
	hwResolveBackend      = func(backend string) (string, error) { return ResolveBackend(backend, nil, "") }
	hwProbeDevices        = ProbeDevices
	hwWindowsGPUAdapters  = hostinfo.WindowsGPUAdapters
	hwGPUClass            = hostinfo.GPUClass
	hwLoadConfig          = func() map[string]any { return loadConfigTree(home.Process()) }
	hwCacheMu             sync.Mutex
	hwPoolProbeCache      *hwTimedPool
	hwAcceleratorCache    = map[string]hwTimedDevice{}
	hwGPUQueryCache       *hwTimedQuery
	hwSMIPathCache        *string
	hwSMIPathCached       bool
)

// Decoded stdout of argv run with a five second deadline
func hwRunStdout(argv ...string) (string, error) {
	res, err := hwRun(argv, 5*time.Second, "")
	if err != nil {
		return "", err
	}
	return res.Stdout, nil
}

// Runs argv with stdin closed and output captured, an OSError class error when it cannot start
func hwRunCommand(argv []string, timeout time.Duration, dir string) (hwRunResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	hideChildWindow(cmd)
	err := cmd.Run()
	if ctx.Err() != nil {
		return hwRunResult{}, &HardwareProbeError{Class: "TimeoutExpired",
			Msg: fmt.Sprintf("Command '%s' timed out after %d seconds", pyRepr(stringsAsAny(argv)), int(timeout.Seconds()))}
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return hwRunResult{}, &HardwareProbeError{Class: "OSError", Msg: err.Error()}
	}
	return hwRunResult{ReturnCode: cmd.ProcessState.ExitCode(),
		Stdout: loadDecodeReplace([]byte(stdout.String())), Stderr: loadDecodeReplace([]byte(stderr.String()))}, nil
}

func stringsAsAny(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

func hwIsClass(err error, classes ...string) bool {
	var probe *HardwareProbeError
	return errors.As(err, &probe) && slices.Contains(classes, probe.Class)
}

// Raw /proc/meminfo text, false when procfs cannot be read
func linuxMeminfoText() (string, bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return "", false
	}
	return string(data), true
}

// Total and available bytes from meminfo text, MemAvailable present even at zero beats MemFree
func linuxRAMFromMeminfo(text string) (int64, int64, bool) {
	fields := map[string]int64{}
	for _, line := range pySplitLines(text) {
		name, value, found := strings.Cut(line, ":")
		if !found || !hwLinuxRAM[name] {
			continue
		}
		parts := strings.FieldsFunc(value, pyIsSpace)
		if len(parts) != 2 || parts[1] != "kB" {
			continue
		}
		n, ok := pyIntText(parts[0])
		if !ok {
			return 0, 0, false
		}
		fields[name] = n * 1024
	}
	total, hasTotal := fields["MemTotal"]
	available, hasAvailable := fields["MemAvailable"]
	if !hasAvailable {
		available, hasAvailable = fields["MemFree"]
	}
	if !hasTotal || total <= 0 || !hasAvailable || available < 0 || available > total {
		return 0, 0, false
	}
	return total, available, true
}

// Python int() of probe output, the empty string standing for fallback as the or operator does
func hwIntOr(text string, fallback int64) (int64, error) {
	if text == "" {
		return fallback, nil
	}
	n, ok := pyIntText(text)
	if !ok {
		return 0, &HardwareProbeError{Class: "ValueError", Msg: fmt.Sprintf("invalid literal for int() with base 10: %s", pyRepr(text))}
	}
	return n, nil
}

// Total and available physical memory, zeros when the platform answer is unreadable
func ramBytes() (int64, int64, error) {
	if stat := hwWindowsMemoryStatus(); stat != nil {
		return int64(stat.TotalPhys), int64(stat.AvailPhys), nil
	}
	total, avail, err := ramBytesPOSIX()
	if hwIsClass(err, "OSError", "ValueError") {
		return 0, 0, nil
	}
	return total, avail, err
}

func ramBytesPOSIX() (int64, int64, error) {
	platform := hwSysPlatform()
	if platform == "darwin" {
		return ramBytesDarwin()
	}
	if strings.HasPrefix(platform, "linux") {
		if text, ok := hwLinuxMeminfoText(); ok {
			if total, avail, ok := linuxRAMFromMeminfo(text); ok {
				return total, avail, nil
			}
		}
	}
	out, err := hwStdout("getconf", "PAGE_SIZE")
	if err != nil {
		return 0, 0, err
	}
	page, err := hwIntOr(out, 4096)
	if err != nil {
		return 0, 0, err
	}
	if out, err = hwStdout("getconf", "_PHYS_PAGES"); err != nil {
		return 0, 0, err
	}
	pages, err := hwIntOr(out, 0)
	if err != nil {
		return 0, 0, err
	}
	total := pages * page
	avail := pyFloorDiv(total, 2)
	out, err = hwStdout("getconf", "_AVPHYS_PAGES")
	if err == nil {
		var free int64
		if free, err = hwIntOr(out, 0); err == nil && free*page != 0 {
			avail = free * page
		}
	}
	if err != nil && !hwIsClass(err, "OSError", "ValueError") {
		return 0, 0, err
	}
	return total, avail, nil
}

func ramBytesDarwin() (int64, int64, error) {
	out, err := hwStdout("/usr/sbin/sysctl", "-n", "hw.memsize")
	if err != nil {
		return 0, 0, err
	}
	total, err := hwIntOr(strings.TrimFunc(out, pyIsSpace), 0)
	if err != nil || total <= 0 {
		return 0, 0, err
	}
	avail := pyFloorDiv(total, 2)
	out, err = hwStdout("/usr/bin/vm_stat")
	if err != nil {
		if hwIsClass(err, "OSError") {
			return total, avail, nil
		}
		return 0, 0, err
	}
	page := int64(16384)
	if m := hwPageSizeRE.FindStringSubmatch(out); m != nil {
		if page, err = hwIntOr(m[1], 0); err != nil {
			return total, avail, nil
		}
	}
	var pages int64
	for _, key := range []string{"Pages free", "Pages inactive", "Pages purgeable", "Pages speculative"} {
		re := regexp.MustCompile(regexp.QuoteMeta(key) + `:` + hwPySpace + `+(\p{Nd}+)\.`)
		if m := re.FindStringSubmatch(out); m != nil {
			n, err := hwIntOr(m[1], 0)
			if err != nil {
				return total, avail, nil
			}
			pages += n
		}
	}
	if pages > 0 {
		avail = pages * page
	}
	return total, avail, nil
}

// Absolute nvidia-smi path from PATH, then the Windows driver dirs or the WSL driver dir, cached
func nvidiaSMIPath() (string, bool) {
	hwCacheMu.Lock()
	if hwSMIPathCached {
		defer hwCacheMu.Unlock()
		if hwSMIPathCache == nil {
			return "", false
		}
		return *hwSMIPathCache, true
	}
	hwCacheMu.Unlock()
	var found *string
	if p, err := hwWhich("nvidia-smi"); err == nil {
		found = &p
	} else if hwSysPlatform() == "win32" {
		windir := hwGetenvOr("SystemRoot", `C:\Windows`)
		programFiles := hwGetenvOr("ProgramFiles", `C:\Program Files`)
		for _, c := range []string{filepath.Join(windir, "System32", "nvidia-smi.exe"),
			filepath.Join(programFiles, "NVIDIA Corporation", "NVSMI", "nvidia-smi.exe")} {
			if hwPathExists(c) {
				found = &c
				break
			}
		}
	} else if strings.HasPrefix(hwSysPlatform(), "linux") {
		if c := "/usr/lib/wsl/lib/nvidia-smi"; hwPathExists(c) {
			found = &c
		}
	}
	hwCacheMu.Lock()
	hwSMIPathCache, hwSMIPathCached = found, true
	hwCacheMu.Unlock()
	if found == nil {
		return "", false
	}
	return *found, true
}

func hwGetenvOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

// Total, free, name and PCI id from the shared nvidia-smi query, nil when it has no answer
func nvidiaVRAM() (*NvidiaGPUQuery, error) {
	return CachedNvidiaGPUQuery(hwGPUQueryTTLSeconds)
}

// One nvidia-smi read cached for ttl seconds, failures included, nil when smi is absent or fails
func CachedNvidiaGPUQuery(ttl float64) (*NvidiaGPUQuery, error) {
	now := hwMonotonic()
	hwCacheMu.Lock()
	if hwGPUQueryCache != nil && now-hwGPUQueryCache.stamp < ttl {
		data := hwGPUQueryCache.data
		hwCacheMu.Unlock()
		return data, nil
	}
	hwCacheMu.Unlock()
	data, err := nvidiaGPUQueryUncached()
	if err != nil {
		return nil, err
	}
	hwCacheMu.Lock()
	hwGPUQueryCache = &hwTimedQuery{stamp: now, data: data}
	hwCacheMu.Unlock()
	return data, nil
}

func nvidiaGPUQueryUncached() (*NvidiaGPUQuery, error) {
	exe, ok := hwNvidiaSMIPath()
	if !ok {
		return nil, nil
	}
	out, err := hwRun([]string{exe, hwGPUQueryArg, "--format=csv,noheader,nounits"}, 10*time.Second, "")
	if err != nil {
		return nil, nil
	}
	text := strings.TrimFunc(out.Stdout, pyIsSpace)
	if out.ReturnCode != 0 || text == "" {
		return nil, nil
	}
	row, err := pyCSVFirstRow(pySplitLines(text))
	if err != nil {
		return nil, err
	}
	if len(row) != 6 {
		return nil, nil
	}
	values := make([]int64, 0, 4)
	for _, raw := range []string{row[0], row[1], row[4], row[5]} {
		n, ok := pyIntText(raw)
		if !ok {
			return nil, nil
		}
		values = append(values, n)
	}
	var pciID *int64
	if id, ok := pyIntBase16(row[3]); ok {
		pciID = &id
	}
	return &NvidiaGPUQuery{GPUName: strings.TrimFunc(row[2], pyIsSpace), TotalBytes: values[0] << 20,
		FreeBytes: values[1] << 20, UsedBytes: values[2] << 20, GPUUtilPercent: values[3], GPUPCIID: pciID}, nil
}

// First record of Python csv.reader with skipinitialspace over lines, a csv.Error past the field limit
func pyCSVFirstRow(lines []string) ([]string, error) {
	var row []string
	var field []rune
	const (
		startField = iota
		inField
		inQuoted
		quoteInQuoted
	)
	state := startField
	finish := func() error {
		if len(field) > hwCSVFieldLimit {
			return &HardwareProbeError{Class: "Error", Msg: fmt.Sprintf("field larger than field limit (%d)", hwCSVFieldLimit)}
		}
		row = append(row, string(field))
		field = field[:0]
		return nil
	}
	for _, line := range lines {
		for _, c := range line {
			switch state {
			case startField:
				switch c {
				case '"':
					state = inQuoted
				case ' ':
				case ',':
					if err := finish(); err != nil {
						return nil, err
					}
				default:
					field = append(field, c)
					state = inField
				}
			case inField:
				if c == ',' {
					if err := finish(); err != nil {
						return nil, err
					}
					state = startField
				} else {
					field = append(field, c)
				}
			case inQuoted:
				if c == '"' {
					state = quoteInQuoted
				} else {
					field = append(field, c)
				}
			case quoteInQuoted:
				switch c {
				case '"':
					field = append(field, c)
					state = inQuoted
				case ',':
					if err := finish(); err != nil {
						return nil, err
					}
					state = startField
				default:
					field = append(field, c)
					state = inField
				}
			}
		}
		if state == inQuoted {
			continue
		}
		break
	}
	if err := finish(); err != nil {
		return nil, err
	}
	return row, nil
}

// Python int(text, 16) with whitespace, sign, an optional 0x prefix and digit underscores
func pyIntBase16(text string) (int64, bool) {
	s := strings.TrimFunc(text, pyIsSpace)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg, s = s[0] == '-', s[1:]
	}
	prefixed := len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X')
	if prefixed {
		s = s[2:]
		s = strings.TrimPrefix(s, "_")
	}
	if s == "" || strings.HasPrefix(s, "_") || strings.HasSuffix(s, "_") || strings.Contains(s, "__") {
		return 0, false
	}
	var n uint64
	for _, r := range strings.ReplaceAll(s, "_", "") {
		var d uint64
		switch {
		case r >= '0' && r <= '9':
			d = uint64(r - '0')
		case r >= 'a' && r <= 'f':
			d = uint64(r-'a') + 10
		case r >= 'A' && r <= 'F':
			d = uint64(r-'A') + 10
		case pyIsDigit(r):
			d = uint64(unicodeDigitValue(r))
		default:
			return 0, false
		}
		if n > (math.MaxInt64-d)/16 {
			return 0, false
		}
		n = n*16 + d
	}
	if neg {
		return -int64(n), true
	}
	return int64(n), true
}

// Allocator pool and integrated verdict from the CUDA driver API of the first device, nil without a driver
func cudaDriverPool() *hwDevicePool {
	var lib *nativeLibrary
	for _, name := range []string{"nvcuda.dll", "libcuda.so.1", "libcuda.so"} {
		if l, err := openNativeLibrary(name, false); err == nil {
			lib = l
			break
		}
	}
	if lib == nil {
		return nil
	}
	cuInit, err := lib.sym("cuInit")
	if err != nil || int32(callNative(cuInit, 0)) != 0 {
		return nil
	}
	cuDeviceGet, err := lib.sym("cuDeviceGet")
	var dev int32
	if err != nil || callNativeIntOut(cuDeviceGet, &dev, 0) != 0 {
		return nil
	}
	getter, err := lib.sym("cuDeviceTotalMem_v2")
	if err != nil {
		if getter, err = lib.sym("cuDeviceTotalMem"); err != nil {
			return nil
		}
	}
	var total uint64
	if callNativeSizeOut(getter, &total, uintptr(dev)) != 0 || total == 0 {
		return nil
	}
	getAttribute, err := lib.sym("cuDeviceGetAttribute")
	if err != nil {
		return nil
	}
	pool := &hwDevicePool{Total: saturatingInt64(total)}
	var attr int32
	if callNativeIntOut(getAttribute, &attr, hwCUDeviceIntegrated, uintptr(dev)) == 0 {
		integrated := attr != 0
		pool.Integrated = &integrated
	}
	return pool
}

// The local_runtime config section, an AttributeError class error when it is truthy but not a mapping
func hwLocalRuntimeSection() (map[string]any, error) {
	raw := hwLoadConfig()["local_runtime"]
	if !pyTruthy(raw) {
		return map[string]any{}, nil
	}
	section, ok := raw.(map[string]any)
	if !ok {
		return nil, &HardwareProbeError{Class: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyTypeName(raw))}
	}
	return section, nil
}

// The backend local_runtime.backend names, auto when unset or falsy
func hwConfiguredBackend(section map[string]any) (string, error) {
	raw := section["backend"]
	if !pyTruthy(raw) {
		return "auto", nil
	}
	backend, ok := raw.(string)
	if !ok {
		return "", &HardwareProbeError{Class: "TypeError", Msg: fmt.Sprintf("backend must be a string, not %s", pyTypeName(raw))}
	}
	return backend, nil
}

// The serving engine when one runs or boots, else the installed engine for local_runtime.backend
func configuredEngine() (*Engine, error) {
	if engine := hwServingEngine(); engine != nil {
		return engine, nil
	}
	section, err := hwLocalRuntimeSection()
	if err != nil {
		return nil, err
	}
	backend, err := hwConfiguredBackend(section)
	if err != nil {
		return nil, err
	}
	return hwInstalledEngine(backend)
}

// Engine pool from the installed runtime's own list-devices output, nil on any miss
func engineDevicePool() (pool *hwDevicePool) {
	defer func() {
		if recover() != nil {
			pool = nil
		}
	}()
	engine, err := hwConfiguredEngine()
	if err != nil || engine == nil {
		return nil
	}
	out, err := hwRun([]string{engine.Binary, "--list-devices"}, 30*time.Second, filepath.Dir(engine.Binary))
	if err != nil || out.ReturnCode != 0 {
		return nil
	}
	for _, line := range pySplitLines(out.Stdout + out.Stderr) {
		if m := hwDeviceLineRE.FindStringSubmatch(line); m != nil {
			mib, ok := pythonUnicodeIntText(m[1])
			if !ok {
				return nil
			}
			return &hwDevicePool{Total: mib << 20}
		}
	}
	return nil
}

// Allocator side view cached for the process once it answers, a miss retried after the negative TTL
func devicePoolView() *hwDevicePool {
	now := hwMonotonic()
	hwCacheMu.Lock()
	if cached := hwPoolProbeCache; cached != nil && (cached.view != nil || now-cached.stamp < hwPoolNegativeTTLSeconds) {
		hwCacheMu.Unlock()
		return cached.view
	}
	hwCacheMu.Unlock()
	view := hwCudaDriverPool()
	if view == nil {
		view = hwEngineDevicePool()
	}
	hwCacheMu.Lock()
	hwPoolProbeCache = &hwTimedPool{stamp: now, view: view}
	hwCacheMu.Unlock()
	return view
}

// Device a Vulkan or HIP engine places layers on, discrete first then largest, cached per engine binary
func acceleratorDevice(fresh bool) (device *GGMLDevice) {
	defer func() {
		if recover() != nil {
			device = nil
		}
	}()
	engine, err := hwConfiguredEngine()
	if err != nil || engine == nil || !slices.Contains(hwGPUEngineBackends, engine.Backend) {
		return nil
	}
	key, now := engine.Binary, hwMonotonic()
	hwCacheMu.Lock()
	cached, hit := hwAcceleratorCache[key]
	hwCacheMu.Unlock()
	if !fresh && hit && (cached.device != nil || now-cached.stamp < hwPoolNegativeTTLSeconds) {
		return cached.device
	}
	for _, d := range hwProbeDevices(filepath.Dir(engine.Binary), engine.Backend) {
		if device == nil || hwDeviceRanksAbove(d, *device) {
			picked := d
			device = &picked
		}
	}
	hwCacheMu.Lock()
	hwAcceleratorCache[key] = hwTimedDevice{stamp: now, device: device}
	hwCacheMu.Unlock()
	return device
}

func hwDeviceRanksAbove(a, b GGMLDevice) bool {
	aGPU, bGPU := a.Type == GGMLDeviceGPU, b.Type == GGMLDeviceGPU
	if aGPU != bGPU {
		return aGPU
	}
	return a.Total > b.Total
}

// Real pool size when this NVIDIA device is unified memory behind a carve out, nil otherwise
func unifiedPoolBytes(smiTotal, ramTotal int64) *int64 {
	view := hwDevicePoolView()
	if view == nil {
		return nil
	}
	pool := view.Total
	if view.Integrated != nil {
		if *view.Integrated {
			return &pool
		}
		return nil
	}
	if smiTotal > 0 && pool >= int64(float64(smiTotal)*hwPoolDisagreement) &&
		ramTotal > 0 && pool >= int64(float64(ramTotal)*hwPoolRAMFraction) {
		return &pool
	}
	return nil
}

// Unified memory budget from base less the UMA headroom
func umaBudget(base, total int64, gpuName string, gpuPCIID *int64, lazyReads bool) HardwareBudget {
	usable := max(0, int64(float64(base)*(1-hwUMAHeadroomFraction)))
	return HardwareBudget{UsableVRAMBytes: usable, TotalDeviceBytes: total, UMA: true, GPUName: gpuName,
		Platform: hwSysPlatform(), GPUPCIID: gpuPCIID, LazyReads: lazyReads}
}

// Commit the managed server may take on Windows now with ownBytes counted as freed, nil off Windows
func windowsCommitRoom(ownBytes int64) *int64 {
	stat := hwWindowsMemoryStatus()
	if stat == nil || stat.TotalPageFile == 0 {
		return nil
	}
	margin := max(hwMarginFloor, int64(float64(stat.TotalPhys)*hwMarginFraction))
	room := max(0, int64(stat.AvailPageFile)+max(0, ownBytes)-margin)
	return &room
}

// What Windows charges to commit for a model estimated at modelBytes of device memory
func ModelCommitBytes(modelBytes int64) int64 {
	return int64(float64(modelBytes) / (1 - hwUMAHeadroomFraction))
}

// Prompt cache size beside a model of modelBytes per resident model, nil when the engine default applies
func PromptCacheBytes(live *HardwareBudget, modelBytes int64, residents int) *int64 {
	if live == nil || live.CommitRoomBytes == nil || modelBytes <= 0 {
		return nil
	}
	n := int64(max(1, residents))
	cache := min(hwPromptCacheMax, pyFloorDiv(max(0, *live.CommitRoomBytes-n*ModelCommitBytes(modelBytes)), n))
	if cache < hwPromptCacheMin {
		cache = 0
	}
	return &cache
}

// Device memory a model may plan with on Windows unified memory leaving each resident the smallest cache
func UsableBesidePromptCache(live HardwareBudget, residents int) (int64, error) {
	if live.CommitRoomBytes == nil {
		return 0, &HardwareProbeError{Class: "TypeError", Msg: "unsupported operand type(s) for //: 'NoneType' and 'int'"}
	}
	share := pyFloorDiv(*live.CommitRoomBytes, int64(max(1, residents))) - hwPromptCacheMin
	return min(live.UsableVRAMBytes, max(0, int64(float64(share)*(1-hwUMAHeadroomFraction)))), nil
}

// Whether a Vulkan or HIP engine runs the model, the configured or planned one, false when unresolved
func gpuEngineRunsTheModel() (runs bool) {
	defer func() {
		if recover() != nil {
			runs = false
		}
	}()
	engine, err := hwConfiguredEngine()
	if err != nil {
		return false
	}
	if engine != nil {
		return slices.Contains(hwGPUEngineBackends, engine.Backend)
	}
	section, err := hwLocalRuntimeSection()
	if err != nil {
		return false
	}
	backend, err := hwConfiguredBackend(section)
	if err != nil {
		return false
	}
	resolved, err := hwResolveBackend(backend)
	return err == nil && slices.Contains(hwGPUEngineBackends, resolved)
}

// What Windows lets the integrated GPU allocate, the engine named adapter or the only non NVIDIA one
func windowsIGPUBytes(device *GGMLDevice) *int64 {
	var hardware, named, others []hostinfo.GPUAdapter
	for _, a := range hwWindowsGPUAdapters() {
		if !a.Software {
			hardware = append(hardware, a)
		}
	}
	for _, a := range hardware {
		if device != nil && a.Description == device.Description {
			named = append(named, a)
		}
		if a.VendorID != hwNvidiaVendorID {
			others = append(others, a)
		}
	}
	candidates := named
	if len(candidates) == 0 {
		candidates = others
	}
	if len(candidates) != 1 || (device == nil && !gpuEngineRunsTheModel()) {
		return nil
	}
	bytes := candidates[0].MemoryBytes()
	return &bytes
}

// Live budget or capacity budget when planning, err for the exceptions Python lets escape the probe
func ProbeBudget(planning bool) (HardwareBudget, error) {
	ramTotal, ramAvail, err := hwRAMBytes()
	if err != nil {
		return HardwareBudget{}, err
	}
	vram, err := hwNvidiaVRAM()
	if err != nil {
		return HardwareBudget{}, err
	}
	var smiTotal int64
	if vram != nil {
		smiTotal = vram.TotalBytes
	}
	if unified := hwUnifiedPoolBytes(smiTotal, ramTotal); unified != nil {
		carve := "unavailable"
		if vram != nil {
			carve = fmt.Sprintf("%.1f GiB", float64(vram.TotalBytes)/hwGiB)
		}
		slog.Info(fmt.Sprintf("unified-memory NVIDIA device: allocator pool %.1f GiB (nvidia-smi carve-out: %s); budgeting from the pool",
			float64(*unified)/hwGiB, carve))
		base := *unified
		if !planning {
			live := ramAvail
			if vram != nil {
				live = vram.FreeBytes + ramAvail
			}
			base = min(*unified, live)
		}
		if vram == nil {
			return umaBudget(base, *unified, "", nil, true), nil
		}
		return umaBudget(base, *unified, vram.GPUName, vram.GPUPCIID, true), nil
	}
	if vram == nil {
		device := hwAcceleratorDevice(false)
		if device != nil && device.Type == GGMLDeviceGPU {
			total := device.Total
			margin := max(hwMarginFloor, int64(float64(total)*hwMarginFraction))
			ram := ramAvail
			if planning {
				ram = ramTotal
			}
			return HardwareBudget{UsableVRAMBytes: max(0, total-margin), TotalDeviceBytes: total, RAMAvailableBytes: ram,
				GPUName: device.Description, Platform: hwSysPlatform(), LazyReads: true}, nil
		}
		pool := ramTotal
		if igpu := windowsIGPUBytes(device); igpu != nil && *igpu != 0 {
			pool = min(ramTotal, *igpu)
		}
		base := pool
		if !planning {
			base = min(pool, ramAvail)
		}
		class := hwGPUClass()
		return umaBudget(base, pool, "", nil, class != "amd" && class != "intel"), nil
	}
	margin := max(hwMarginFloor, int64(float64(vram.TotalBytes)*hwMarginFraction))
	usable, ram := vram.FreeBytes, ramAvail
	if planning {
		usable, ram = vram.TotalBytes, ramTotal
	}
	return HardwareBudget{UsableVRAMBytes: max(0, usable-margin), TotalDeviceBytes: vram.TotalBytes, RAMAvailableBytes: ram,
		GPUName: vram.GPUName, Platform: hwSysPlatform(), GPUPCIID: vram.GPUPCIID, LazyReads: true}, nil
}

// Capacity less what other programs hold on the card now, nil when no live reading applies
func LaunchBudget(capacity HardwareBudget, ownBytes int64) (*HardwareBudget, error) {
	if capacity.UMA {
		var charged int64
		if ownBytes > 0 {
			charged = ModelCommitBytes(ownBytes)
		}
		room := windowsCommitRoom(charged)
		if room == nil {
			return nil, nil
		}
		out := capacity
		out.UsableVRAMBytes = min(capacity.UsableVRAMBytes, int64(float64(*room)*(1-hwUMAHeadroomFraction)))
		out.CommitRoomBytes = room
		return &out, nil
	}
	vram, err := hwNvidiaVRAM()
	if err != nil {
		return nil, err
	}
	var total, free int64
	if vram != nil {
		total, free = vram.TotalBytes, vram.FreeBytes
	} else {
		device := hwAcceleratorDevice(true)
		if device == nil || device.Type != GGMLDeviceGPU {
			return nil, nil
		}
		total, free = device.Total, device.Free
	}
	others := max(0, total-free-max(0, ownBytes))
	out := capacity
	out.UsableVRAMBytes = min(capacity.UsableVRAMBytes, max(0, total-others-hwLaunchHeadroom))
	return &out, nil
}
