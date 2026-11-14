package local

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/hostinfo"
	"github.com/nickheyer/icebath/internal/proc"
)

const (
	gibFloat               = float64(1 << 30)
	marginFloor            = int64(2 << 30)
	marginFraction         = 0.09
	launchHeadroom         = int64(1 << 30)
	umaHeadroomFraction    = 0.20
	promptCacheMax         = int64(8 << 30)
	promptCacheMin         = int64(512 << 20)
	poolDisagreementFactor = 1.5
	poolRAMFraction        = 0.75
	nvidiaVendorID         = 0x10DE
	poolNegativeTTL        = 60 * time.Second
	listDevicesTimeout     = 30 * time.Second
)

var deviceLinePattern = regexp.MustCompile(`CUDA\d+:.*\((\d+)\s*MiB,\s*\d+\s*MiB free\)\s*$`)

var gpuEngineBackends = map[string]bool{"vulkan": true, "hip": true}

type poolView struct {
	total      int64
	integrated *bool
}

type poolProbe struct {
	at   time.Time
	view *poolView
}

type accelProbe struct {
	at     time.Time
	device *Device
}

type smiVRAM struct {
	total, free int64
	name        string
	pciID       uint32
}

func (r *Runtime) RAM() (total, available int64) { return ramBytes(r.host) }

func (r *Runtime) nvidiaVRAM(ctx context.Context) *smiVRAM {
	reading, err := r.host.NvidiaSMI(ctx)
	if err != nil {
		return nil
	}
	v := &smiVRAM{total: int64(reading.TotalBytes), free: int64(reading.FreeBytes), name: reading.Name}
	if reading.PCIID != nil {
		v.pciID = *reading.PCIID
	}
	return v
}

func (r *Runtime) cudaDriverPool() *poolView {
	dev, err := r.host.CUDA()
	if err != nil || dev.TotalBytes == 0 {
		return nil
	}
	return &poolView{total: int64(dev.TotalBytes), integrated: dev.Integrated}
}

func (r *Runtime) configuredEngine(ctx context.Context) *Engine {
	if engine := r.ServingEngine(); engine != nil {
		return engine
	}
	backend, err := r.configuredBackend(ctx)
	if err != nil {
		return nil
	}
	engine, err := r.InstalledEngine(ctx, backend, true)
	if err != nil {
		return nil
	}
	return engine
}

func (r *Runtime) configuredBackend(ctx context.Context) (string, error) {
	cfg, err := r.config(ctx)
	if err != nil {
		return "", err
	}
	if backend := string(cfg.LocalRuntime.Backend); backend != "" {
		return backend, nil
	}
	return "auto", nil
}

func (r *Runtime) engineDevicePool(ctx context.Context) *poolView {
	engine := r.configuredEngine(ctx)
	if engine == nil {
		return nil
	}
	runCtx, cancel := context.WithTimeout(ctx, listDevicesTimeout)
	defer cancel()
	cmd := exec.Command(engine.Binary, "--list-devices")
	cmd.Dir = filepath.Dir(engine.Binary)
	proc.Hide(cmd)
	res, err := proc.Run(runCtx, cmd)
	if err != nil || !res.Exit.Success() {
		return nil
	}
	scanner := bufio.NewScanner(bytes.NewReader(append(append([]byte{}, res.Stdout...), res.Stderr...)))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		if m := deviceLinePattern.FindStringSubmatch(strings.TrimRight(scanner.Text(), "\r")); m != nil {
			mib, err := strconv.ParseInt(m[1], 10, 64)
			if err == nil {
				return &poolView{total: mib << 20}
			}
		}
	}
	return nil
}

func (r *Runtime) devicePoolView(ctx context.Context) *poolView {
	r.hwMu.Lock()
	cached := r.pool
	r.hwMu.Unlock()
	now := time.Now()
	if cached != nil && (cached.view != nil || now.Sub(cached.at) < poolNegativeTTL) {
		return cached.view
	}
	view := r.cudaDriverPool()
	if view == nil {
		view = r.engineDevicePool(ctx)
	}
	r.hwMu.Lock()
	r.pool = &poolProbe{at: now, view: view}
	r.hwMu.Unlock()
	return view
}

func (r *Runtime) acceleratorDevice(ctx context.Context, fresh bool) *Device {
	engine := r.configuredEngine(ctx)
	if engine == nil || !gpuEngineBackends[engine.Backend] {
		return nil
	}
	key, now := engine.Binary, time.Now()
	r.hwMu.Lock()
	cached, ok := r.accel[key]
	r.hwMu.Unlock()
	if !fresh && ok && (cached.device != nil || now.Sub(cached.at) < poolNegativeTTL) {
		return cached.device
	}
	var best *Device
	for _, d := range r.probeDevices(ctx, filepath.Dir(engine.Binary), engine.Backend) {
		if best == nil || deviceRanksAbove(d, *best) {
			pick := d
			best = &pick
		}
	}
	r.hwMu.Lock()
	r.accel[key] = accelProbe{at: now, device: best}
	r.hwMu.Unlock()
	return best
}

func deviceRanksAbove(a, b Device) bool {
	aGPU, bGPU := a.Type == ggmlDeviceGPU, b.Type == ggmlDeviceGPU
	if aGPU != bGPU {
		return aGPU
	}
	return a.Total > b.Total
}

func (r *Runtime) unifiedPoolBytes(ctx context.Context, smiTotal, ramTotal int64) (int64, bool) {
	view := r.devicePoolView(ctx)
	if view == nil {
		return 0, false
	}
	if view.integrated != nil {
		return view.total, *view.integrated
	}
	if smiTotal > 0 && view.total >= int64(float64(smiTotal)*poolDisagreementFactor) &&
		ramTotal > 0 && view.total >= int64(float64(ramTotal)*poolRAMFraction) {
		return view.total, true
	}
	return 0, false
}

func umaBudget(base, total int64, gpuName string, pciID uint32, lazyReads bool) HardwareBudget {
	return HardwareBudget{
		UsableVRAMBytes: max(0, int64(float64(base)*(1-umaHeadroomFraction))), TotalDeviceBytes: total,
		UMA: true, GPUName: gpuName, Platform: string(hostinfo.OSFamily), GPUPCIID: pciID, LazyReads: lazyReads,
	}
}

func windowsCommitRoom(ownBytes int64) *int64 {
	m, err := hostinfo.ReadMemory()
	if err != nil || m.Commit == nil || m.Commit.Limit == 0 {
		return nil
	}
	margin := max(marginFloor, int64(float64(m.Total)*marginFraction))
	room := max(0, int64(m.Commit.Available)+max(0, ownBytes)-margin)
	return &room
}

func ModelCommitBytes(modelBytes int64) int64 {
	return int64(float64(modelBytes) / (1 - umaHeadroomFraction))
}

func PromptCacheBytes(live *HardwareBudget, modelBytes int64, residents int) *int64 {
	if live == nil || live.CommitRoomBytes == nil || modelBytes <= 0 {
		return nil
	}
	residents = max(1, residents)
	room := *live.CommitRoomBytes
	cache := min(promptCacheMax, max(0, room-int64(residents)*ModelCommitBytes(modelBytes))/int64(residents))
	if cache < promptCacheMin {
		cache = 0
	}
	return &cache
}

func UsableBesidePromptCache(live HardwareBudget, residents int) int64 {
	var room int64
	if live.CommitRoomBytes != nil {
		room = *live.CommitRoomBytes
	}
	share := room/int64(max(1, residents)) - promptCacheMin
	return min(live.UsableVRAMBytes, max(0, int64(float64(share)*(1-umaHeadroomFraction))))
}

func (r *Runtime) gpuEngineRunsTheModel(ctx context.Context) bool {
	if engine := r.configuredEngine(ctx); engine != nil {
		return gpuEngineBackends[engine.Backend]
	}
	backend, err := r.configuredBackend(ctx)
	if err != nil {
		return false
	}
	resolved, err := r.ResolveBackend(ctx, backend)
	if err != nil {
		return false
	}
	return gpuEngineBackends[resolved]
}

func (r *Runtime) windowsIGPUBytes(ctx context.Context, device *Device) int64 {
	adapters, err := r.host.DXGIAdapters()
	if err != nil {
		return 0
	}
	var hardware, named, others []hostinfo.DXGIAdapter
	for _, a := range adapters {
		if !a.Software {
			hardware = append(hardware, a)
		}
	}
	for _, a := range hardware {
		if device != nil && a.Description == device.Description {
			named = append(named, a)
		}
		if a.VendorID != nvidiaVendorID {
			others = append(others, a)
		}
	}
	candidates := named
	if len(candidates) == 0 {
		candidates = others
	}
	if len(candidates) != 1 || (device == nil && !r.gpuEngineRunsTheModel(ctx)) {
		return 0
	}
	return int64(candidates[0].MemoryBytes())
}

func (r *Runtime) ProbeBudget(ctx context.Context, planning bool) HardwareBudget {
	ramTotal, ramAvail := ramBytes(r.host)
	vram := r.nvidiaVRAM(ctx)
	var smiTotal int64
	if vram != nil {
		smiTotal = vram.total
	}
	if unified, ok := r.unifiedPoolBytes(ctx, smiTotal, ramTotal); ok {
		carveOut := "unavailable"
		if vram != nil {
			carveOut = fmt.Sprintf("%.1f GiB", float64(vram.total)/gibFloat)
		}
		slog.Info(fmt.Sprintf("unified-memory NVIDIA device: allocator pool %.1f GiB (nvidia-smi carve-out: %s); budgeting from the pool",
			float64(unified)/gibFloat, carveOut))
		base := unified
		if !planning {
			live := ramAvail
			if vram != nil {
				live = vram.free + ramAvail
			}
			base = min(unified, live)
		}
		if vram != nil {
			return umaBudget(base, unified, vram.name, vram.pciID, true)
		}
		return umaBudget(base, unified, "", 0, true)
	}
	if vram == nil {
		device := r.acceleratorDevice(ctx, false)
		if device != nil && device.Type == ggmlDeviceGPU {
			margin := max(marginFloor, int64(float64(device.Total)*marginFraction))
			ram := ramAvail
			if planning {
				ram = ramTotal
			}
			return HardwareBudget{
				UsableVRAMBytes: max(0, device.Total-margin), TotalDeviceBytes: device.Total, RAMAvailableBytes: ram,
				GPUName: device.Description, Platform: string(hostinfo.OSFamily), LazyReads: true,
			}
		}
		pool := ramTotal
		if igpu := r.windowsIGPUBytes(ctx, device); igpu != 0 {
			pool = min(ramTotal, igpu)
		}
		base := min(pool, ramAvail)
		if planning {
			base = pool
		}
		class := r.host.GPUClass()
		return umaBudget(base, pool, "", 0, class != hostinfo.GPUAMD && class != hostinfo.GPUIntel)
	}
	margin := max(marginFloor, int64(float64(vram.total)*marginFraction))
	usable, ram := vram.free, ramAvail
	if planning {
		usable, ram = vram.total, ramTotal
	}
	return HardwareBudget{
		UsableVRAMBytes: max(0, usable-margin), TotalDeviceBytes: vram.total, RAMAvailableBytes: ram,
		GPUName: vram.name, Platform: string(hostinfo.OSFamily), GPUPCIID: vram.pciID, LazyReads: true,
	}
}

func (r *Runtime) LaunchBudget(ctx context.Context, capacity HardwareBudget, ownBytes int64) *HardwareBudget {
	if capacity.UMA {
		var own int64
		if ownBytes > 0 {
			own = ModelCommitBytes(ownBytes)
		}
		room := windowsCommitRoom(own)
		if room == nil {
			return nil
		}
		out := capacity
		out.UsableVRAMBytes = min(capacity.UsableVRAMBytes, int64(float64(*room)*(1-umaHeadroomFraction)))
		out.CommitRoomBytes = room
		return &out
	}
	var total, free int64
	if vram := r.nvidiaVRAM(ctx); vram != nil {
		total, free = vram.total, vram.free
	} else {
		device := r.acceleratorDevice(ctx, true)
		if device == nil || device.Type != ggmlDeviceGPU {
			return nil
		}
		total, free = device.Total, device.Free
	}
	others := max(0, total-free-max(0, ownBytes))
	out := capacity
	out.UsableVRAMBytes = min(capacity.UsableVRAMBytes, max(0, total-others-launchHeadroom))
	return &out
}
