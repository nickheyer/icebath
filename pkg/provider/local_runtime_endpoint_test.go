package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/hostinfo"
)

var (
	lr2RAMWin  = int64(31.5 * lr2F(lr2GiB))
	lr2B390    = hostinfo.GPUAdapter{Description: "Intel(R) Arc(TM) B390 GPU", VendorID: 0x8086, DedicatedBytes: 128 << 20, SharedBytes: 18*lr2GiB - (128 << 20)}
	lr2Adreno  = hostinfo.GPUAdapter{Description: "Qualcomm(R) Adreno(TM) X1-85 GPU", VendorID: 0x4D4F4351, SharedBytes: 15 * lr2GiB}
	lr2SW      = hostinfo.GPUAdapter{Description: "Microsoft Basic Render Driver", VendorID: 0x1414, SharedBytes: 16 * lr2GiB, Software: true}
	lr2EngB390 = &GGMLDevice{Description: lr2B390.Description, Type: GGMLDeviceIGPU, Total: 34 * lr2GiB, Free: 30 * lr2GiB}
)

func lr2IGPUMachine(t *testing.T, adapters []hostinfo.GPUAdapter, device *GGMLDevice, installed, planned string) {
	lr2Isolate(t)
	hwNvidiaVRAM = lr2NoVRAM
	hwDevicePoolView = func() *hwDevicePool { return nil }
	hwRAMBytes = lr2RAM(lr2RAMWin, 20*lr2GiB)
	hwAcceleratorDevice = func(bool) *GGMLDevice { return device }
	hwWindowsGPUAdapters = func() []hostinfo.GPUAdapter { return adapters }
	hwConfiguredEngine = func() (*Engine, error) {
		if installed == "" {
			return nil, nil
		}
		return &Engine{Backend: installed, Tag: "b11370", Binary: "llama-server"}, nil
	}
	hwResolveBackend = func(string) (string, error) { return planned, nil }
}

func TestLocalRuntimeIntegratedGPUGetsWhatWindowsLetsItAllocate(t *testing.T) {
	for _, c := range []struct {
		device    *GGMLDevice
		installed string
	}{{lr2EngB390, "vulkan"}, {nil, ""}} {
		lr2IGPUMachine(t, []hostinfo.GPUAdapter{lr2B390, lr2SW}, c.device, c.installed, "vulkan")
		capacity, live := lr2Probe(t, true), lr2Probe(t, false)
		if !capacity.UMA || capacity.TotalDeviceBytes != lr2B390.MemoryBytes() || live.UsableVRAMBytes > capacity.UsableVRAMBytes {
			t.Fatalf("%+v %+v", capacity, live)
		}
		if choice := SelectVariant(CatalogByID()["qwen3.8-27b"], capacity); choice != nil {
			t.Fatalf("the 27B fits an 18 GiB integrated pool %+v", choice)
		}
	}
	discrete := hostinfo.GPUAdapter{Description: "Intel(R) Arc(TM) A380 Graphics", VendorID: 0x8086, DedicatedBytes: 6 * lr2GiB, SharedBytes: 15 * lr2GiB}
	lr2IGPUMachine(t, []hostinfo.GPUAdapter{discrete, lr2B390, lr2SW}, lr2EngB390, "vulkan", "vulkan")
	if b := lr2Probe(t, true); b.TotalDeviceBytes != lr2B390.MemoryBytes() {
		t.Fatalf("named %+v", b)
	}
	lr2IGPUMachine(t, []hostinfo.GPUAdapter{discrete, lr2B390, lr2SW}, nil, "vulkan", "vulkan")
	if b := lr2Probe(t, true); b.TotalDeviceBytes != lr2RAMWin {
		t.Fatalf("ambiguous %+v", b)
	}
	for _, c := range []struct {
		adapter            hostinfo.GPUAdapter
		installed, planned string
	}{{lr2B390, "cpu", "cpu"}, {lr2Adreno, "", "cpu"}} {
		lr2IGPUMachine(t, []hostinfo.GPUAdapter{c.adapter, lr2SW}, nil, c.installed, c.planned)
		if b := lr2Probe(t, true); b.TotalDeviceBytes != lr2RAMWin {
			t.Fatalf("cpu engine %+v", b)
		}
	}
	for _, adapters := range [][]hostinfo.GPUAdapter{{lr2SW},
		{{Description: "AMD Radeon(TM) 8060S Graphics", VendorID: 0x1002, DedicatedBytes: 96 * lr2GiB, SharedBytes: 16 * lr2GiB}, lr2SW}, {}} {
		lr2IGPUMachine(t, adapters, nil, "vulkan", "vulkan")
		if b := lr2Probe(t, true); b.TotalDeviceBytes != lr2RAMWin {
			t.Fatalf("never past ram %+v", b)
		}
	}
}

func lr2Stat(visible, limit, free float64) *windowsMemoryStatusEx {
	g := lr2F(lr2GiB)
	return &windowsMemoryStatusEx{TotalPhys: uint64(visible * g), AvailPhys: uint64(visible * g / 2),
		TotalPageFile: uint64(limit * g), AvailPageFile: uint64(free * g)}
}

func lr2Spark(t *testing.T) **windowsMemoryStatusEx {
	lr2Isolate(t)
	stat := lr2Stat(30.2, 61.2, 48.4)
	hwWindowsMemoryStatus = func() *windowsMemoryStatusEx { return stat }
	hwUnifiedPoolBytes = func(int64, int64) *int64 { return ptrInt64(int64(50.6 * lr2F(lr2GiB))) }
	hwNvidiaVRAM = lr2VRAM(32*lr2GiB, 31*lr2GiB, "NVIDIA RTX Spark N1X", nil)
	hwRAMBytes = lr2RAM(int64(30.2*lr2F(lr2GiB)), 15*lr2GiB)
	return &stat
}

func lr2Near(a, b, rel float64) bool { return math.Abs(a-b) <= rel*math.Abs(b) }

func TestLocalRuntimeWindowsCommitBudget(t *testing.T) {
	g := lr2F(lr2GiB)
	stat := lr2Spark(t)
	before := windowsCommitRoom(0)
	if !lr2Near(lr2F(*before), (48.4-2.718)*g, 1e-3) {
		t.Fatalf("margin %d", *before)
	}
	*stat = lr2Stat(30.2, 61.2, 48.4-37.5)
	if got := windowsCommitRoom(int64(37.5 * g)); !lr2Near(lr2F(*got), lr2F(*before), 1e-6) {
		t.Fatalf("own bytes %d", *got)
	}
	lr2Isolate(t)
	capacity := HardwareBudget{UsableVRAMBytes: 16 * lr2GiB, TotalDeviceBytes: 50 * lr2GiB, UMA: true, LazyReads: true}
	if windowsCommitRoom(0) != nil {
		t.Fatal("off windows room")
	}
	if live, err := LaunchBudget(capacity, 0); live != nil || err != nil {
		t.Fatal("off windows launch budget")
	}
	stat = lr2Spark(t)
	idle := lr2Probe(t, true)
	*stat = lr2Stat(30.2, 61.2, 12.0)
	if busy := lr2Probe(t, true); busy.UsableVRAMBytes != idle.UsableVRAMBytes || busy.CommitRoomBytes != nil {
		t.Fatalf("busy %+v", busy)
	}
	*stat = lr2Stat(22.2, 44.4, 31.6)
	live, _ := LaunchBudget(idle, 0)
	if *live.CommitRoomBytes != *windowsCommitRoom(0) || live.UsableVRAMBytes != int64(lr2F(*live.CommitRoomBytes)*0.8) ||
		live.UsableVRAMBytes >= idle.UsableVRAMBytes {
		t.Fatalf("narrow %+v", live)
	}
	*stat = lr2Stat(94, 120, 106)
	capacity = lr2Probe(t, true)
	live, _ = LaunchBudget(capacity, 0)
	if live.UsableVRAMBytes != capacity.UsableVRAMBytes || capacity.UsableVRAMBytes != int64(50.6*g*0.8) {
		t.Fatalf("plentiful %+v", live)
	}
	*stat = lr2Stat(30.2, 61.2, 48.4)
	capacity = lr2Probe(t, true)
	*stat = lr2Stat(30.2, 61.2, 48.4-25.0)
	live, _ = LaunchBudget(capacity, 20*lr2GiB)
	if !lr2Near(lr2F(*live.CommitRoomBytes), (48.4-2.718)*g, 1e-3) {
		t.Fatalf("outgoing %+v", live)
	}
}

func TestLocalRuntimePromptCacheShares(t *testing.T) {
	g := lr2F(lr2GiB)
	live := &HardwareBudget{UMA: true, CommitRoomBytes: ptrInt64(int64(45.7 * g))}
	if got := PromptCacheBytes(live, 20*lr2GiB, 1); *got != 8*lr2GiB {
		t.Fatalf("max %d", *got)
	}
	if got := PromptCacheBytes(live, 31*lr2GiB, 1); !lr2Near(lr2F(*got), 45.7*g-31*g/0.8, 1e-6) {
		t.Fatalf("leftover %d", *got)
	}
	if got := PromptCacheBytes(live, int64(36.5*g), 1); *got != 0 {
		t.Fatalf("off %d", *got)
	}
	if PromptCacheBytes(nil, 10*lr2GiB, 1) != nil {
		t.Fatal("no live reading")
	}
	each := PromptCacheBytes(live, 14*lr2GiB, 2)
	if !lr2Near(lr2F(*each), (45.7-2*17.5)*g/2, 1e-6) || 2*(lr2F(*each)+17.5*g) > 45.7*g {
		t.Fatalf("residents %d", *each)
	}
	if got := PromptCacheBytes(live, 14*lr2GiB, 1); *got != 8*lr2GiB {
		t.Fatalf("one resident %d", *got)
	}
	if usable, err := UsableBesidePromptCache(HardwareBudget{UsableVRAMBytes: 40 * lr2GiB, CommitRoomBytes: ptrInt64(20 * lr2GiB)}, 2); err != nil ||
		usable != int64(lr2F(10*lr2GiB-hwPromptCacheMin)*0.8) {
		t.Fatalf("usable %d %v", usable, err)
	}
	if _, err := UsableBesidePromptCache(HardwareBudget{}, 1); err == nil {
		t.Fatal("missing commit room must raise")
	}
}

func TestLocalRuntimeLaunchBudget(t *testing.T) {
	cardTotal := int64(31.84 * lr2F(lr2GiB))
	capacity := HardwareBudget{UsableVRAMBytes: cardTotal - max(hwMarginFloor, int64(lr2F(cardTotal)*hwMarginFraction)),
		TotalDeviceBytes: cardTotal, RAMAvailableBytes: 256 * lr2GiB, LazyReads: true}
	card := func(others float64) {
		hwNvidiaVRAM = lr2VRAM(cardTotal, cardTotal-int64(others*lr2F(lr2GiB)), "NVIDIA GeForce RTX 5090", nil)
	}
	lr2Isolate(t)
	card(5.4)
	if b, _ := LaunchBudget(capacity, 0); b.UsableVRAMBytes != cardTotal-int64(5.4*lr2F(lr2GiB))-hwLaunchHeadroom ||
		b.TotalDeviceBytes != cardTotal || b.UMA {
		t.Fatalf("subtracts %+v", b)
	}
	card(0)
	if b, _ := LaunchBudget(capacity, 0); b.UsableVRAMBytes != capacity.UsableVRAMBytes {
		t.Fatalf("capacity cap %+v", b)
	}
	card(5.0 + 24.6)
	if b, _ := LaunchBudget(capacity, int64(24.6*lr2F(lr2GiB))); math.Abs(lr2F(b.UsableVRAMBytes-(cardTotal-5*lr2GiB-hwLaunchHeadroom))) > 1<<20 {
		t.Fatalf("own server %+v", b)
	}
	held := 0.0
	hwNvidiaVRAM = lr2NoVRAM
	hwConfiguredEngine = func() (*Engine, error) { return &Engine{Backend: "vulkan", Binary: "/x/llama-server"}, nil }
	hwProbeDevices = func(string, string) []GGMLDevice {
		return []GGMLDevice{{Description: "AMD Radeon RX 9060 XT", Type: 1, Total: cardTotal, Free: cardTotal - int64(held*lr2F(lr2GiB))}}
	}
	acceleratorDevice(false)
	held = 5.4
	if b, _ := LaunchBudget(capacity, 0); b.UsableVRAMBytes != cardTotal-int64(5.4*lr2F(lr2GiB))-hwLaunchHeadroom {
		t.Fatalf("vulkan %+v", b)
	}
	uma := capacity
	uma.UMA = true
	hwNvidiaVRAM = func() (*NvidiaGPUQuery, error) { t.Fatal("UMA must not probe"); return nil, nil }
	if b, _ := LaunchBudget(uma, 0); b != nil {
		t.Fatal("uma off windows")
	}
	hwNvidiaVRAM = lr2NoVRAM
	hwConfiguredEngine = func() (*Engine, error) { return nil, nil }
	if b, _ := LaunchBudget(capacity, 0); b != nil {
		t.Fatal("failed probe")
	}
}

func TestLocalRuntimeGGMLDevicesFilterAndChildFailure(t *testing.T) {
	got := ggmlDevicesFromJSON([]any{
		map[string]any{"description": "dGPU", "type": 1, "free": 5, "total": 10},
		map[string]any{"description": "iGPU", "type": 2.0, "free": 1, "total": 2},
		map[string]any{"description": "cpu", "type": 0, "free": 1, "total": 2},
		map[string]any{"description": "zero", "type": 1, "free": 0, "total": 0},
		map[string]any{"description": "float", "type": 1, "free": 0, "total": 2.0},
		map[string]any{"description": 5, "type": 1, "total": 2}, "not a dict"})
	if len(got) != 2 || got[0].Description != "dGPU" || got[1].Type != GGMLDeviceIGPU {
		t.Fatalf("%+v", got)
	}
	if devices := ProbeDevices(t.TempDir(), "vulkan"); len(devices) != 0 {
		t.Fatalf("missing libraries %+v", devices)
	}
	var out, errOut bytes.Buffer
	if code := ggmlDevicesMain(t.TempDir(), "vulkan", &out, &errOut); code != 1 || out.Len() != 0 {
		t.Fatalf("exit %d out %q", code, out.String())
	}
}

func lr2LlamaServer(t *testing.T, propsStatus int, props, models any) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/props":
			w.WriteHeader(propsStatus)
			_ = json.NewEncoder(w).Encode(props)
		case "/models":
			if models == nil {
				w.WriteHeader(404)
				return
			}
			_ = json.NewEncoder(w).Encode(models)
		}
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	return n
}

func TestLocalRuntimeDetectServerFingerprintsLlamaServer(t *testing.T) {
	prev := DefaultProbePorts
	DefaultProbePorts = nil
	t.Cleanup(func() { DefaultProbePorts = prev })
	router := lr2LlamaServer(t, 200, map[string]any{"build_info": "b10290-c8e03ce81", "model_path": "/m.gguf",
		"default_generation_settings": map[string]any{"n_ctx": 4096}}, map[string]any{"data": []any{}})
	hit, err := DetectServer([]int{router, router})
	if err != nil || hit == nil || hit.BuildInfo != "b10290-c8e03ce81" || hit.ModelPath != "/m.gguf" || hit.NCtx != 4096 ||
		!hit.RouterMode || hit.AuthRequired || hit.BaseURL != "http://127.0.0.1:"+strconv.Itoa(router)+"/v1" {
		t.Fatalf("%+v %v", hit, err)
	}
	locked := lr2LlamaServer(t, 401, nil, nil)
	if hit, _ := ProbePort(locked); hit == nil || !hit.AuthRequired || hit.BuildInfo != "" {
		t.Fatalf("auth %+v", hit)
	}
	other := lr2LlamaServer(t, 200, map[string]any{"version": "ollama"}, nil)
	if hit, _ := ProbePort(other); hit != nil {
		t.Fatalf("non llama %+v", hit)
	}
	plain := lr2LlamaServer(t, 200, map[string]any{"build_info": "b1"}, nil)
	if hit, _ := DetectServer([]int{other, plain}); hit == nil || hit.RouterMode || hit.NCtx != nil {
		t.Fatalf("plain %+v", hit)
	}
}

func TestLocalRuntimeChildQueryAndManagedGetJSON(t *testing.T) {
	if got := childQuery("Qwen 3/8B&x"); got != "model=Qwen%203/8B%26x&autoload=false" {
		t.Fatalf("%q", got)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"modalities": {"vision": true}}`))
	}))
	defer srv.Close()
	body, err := managedGetJSON(context.Background(), srv.URL, "k", "/props", time.Second)
	if err != nil || body.(map[string]any)["modalities"].(map[string]any)["vision"] != true {
		t.Fatalf("%v %v", body, err)
	}
	if _, err := managedGetJSON(context.Background(), srv.URL, "wrong", "/props", time.Second); err == nil {
		t.Fatal("401 must raise")
	}
}

func TestLocalRuntimeResolveEndpointDetectsThenWaitsOnlyWhenBooting(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	prevDetect, prevEngine, prevEnsure, prevState := detectServerFn, endpointInstalledEngine, ensureLocalRuntimeFn, lrStatePath
	t.Cleanup(func() {
		detectServerFn, endpointInstalledEngine, ensureLocalRuntimeFn, lrStatePath = prevDetect, prevEngine, prevEnsure, prevState
	})
	lrStatePath = func() string { return filepath.Join(dir, "runtimes", "llamacpp", "server.json") }
	var probed []int
	detectServerFn = func(ports []int) (*DetectedServer, error) {
		probed = ports
		return &DetectedServer{BaseURL: "http://127.0.0.1:9/v1"}, nil
	}
	cfg := map[string]any{"local_runtime": map[string]any{"detect_ports": []any{"8081", 8082.0, true}}}
	ep, err := ResolveLlamacppEndpoint(context.Background(), cfg, dir, 0)
	if err != nil || ep == nil || ep.BaseURL != "http://127.0.0.1:9/v1" || ep.APIKey != "" || len(probed) != 3 || probed[0] != 8081 || probed[2] != 1 {
		t.Fatalf("%+v %v %v", ep, err, probed)
	}
	if _, err := ResolveLlamacppEndpoint(context.Background(), map[string]any{"local_runtime": map[string]any{"detect_ports": []any{"x"}}}, dir, 0); err == nil {
		t.Fatal("bad port must raise")
	}
	detectServerFn = func([]int) (*DetectedServer, error) { return &DetectedServer{AuthRequired: true}, nil }
	booted := make(chan bool, 1)
	ensureLocalRuntimeFn = func(ctx context.Context, cfg map[string]any) (*LlamaServerSupervisor, error) {
		booted <- true
		return nil, nil
	}
	endpointInstalledEngine = func(context.Context, string) (*Engine, error) { return nil, nil }
	enabled := map[string]any{"local_runtime": map[string]any{"enabled": true}}
	if ep, _ := ResolveLlamacppEndpoint(context.Background(), enabled, dir, 50*time.Millisecond); ep != nil {
		t.Fatalf("no engine %+v", ep)
	}
	endpointInstalledEngine = func(context.Context, string) (*Engine, error) { return &Engine{Backend: "cpu"}, nil }
	if ep, _ := ResolveLlamacppEndpoint(context.Background(), enabled, dir, 50*time.Millisecond); ep != nil {
		t.Fatalf("boot never wrote state %+v", ep)
	}
	select {
	case <-booted:
	case <-time.After(5 * time.Second):
		t.Fatal("kick never booted")
	}
	if _, _, ok := managedLocalStateEndpoint(filepath.Join(t.TempDir(), "elsewhere")); ok {
		t.Fatal("foreign installation endpoint")
	}
	if endpointPIDAlive(0) || endpointPIDAlive(-1) || !endpointPIDAlive(os.Getpid()) {
		t.Fatal("pid liveness")
	}
	_, routerPid := lr4Sleeper(t)
	record := lr4ModernRecord(t, routerPid)
	record["base_url"], record["api_key"] = "http://127.0.0.1:18001/v1", "key-a"
	_ = os.MkdirAll(filepath.Dir(lrStatePath()), 0o700)
	lr4WriteState(t, lrStatePath(), record)
	if base, key, ok := managedLocalStateEndpoint(dir); !ok || base != "http://127.0.0.1:18001/v1" || key != "key-a" {
		t.Fatalf("managed state %q %q %t", base, key, ok)
	}
	if base, key, ok := managedRoot(context.Background()); !ok || base != "http://127.0.0.1:18001" || key != "key-a" {
		t.Fatalf("managed root %q %q %t", base, key, ok)
	}
	if ep, _ := ResolveLlamacppEndpoint(context.Background(), nil, dir, 0); ep == nil || ep.APIKey != "key-a" {
		t.Fatalf("managed first %+v", ep)
	}
	_ = os.Remove(lrStatePath())
	if _, _, ok := managedLocalStateEndpoint(dir); ok {
		t.Fatal("server off must not resolve")
	}
}

func TestLocalRuntimeManagedCapabilities(t *testing.T) {
	prevURL, prevStaged, prevProps, prevEntry, prevAssets := capIsManagedURL, capStagedModelIDs, capPropsModalities, capEntryForModel, capAssetsDir
	t.Cleanup(func() {
		capIsManagedURL, capStagedModelIDs, capPropsModalities, capEntryForModel, capAssetsDir = prevURL, prevStaged, prevProps, prevEntry, prevAssets
	})
	capIsManagedURL = func(u string) bool { return u == "http://127.0.0.1:1/v1" }
	if !IsManagedProvider(" Llama.CPP ", "") || IsManagedProvider("custom", "") || !IsManagedProvider("custom", "http://127.0.0.1:1/v1") ||
		IsManagedProvider("custom", "http://other/v1") || IsManagedProvider("openrouter", "http://127.0.0.1:1/v1") {
		t.Fatal("managed provider")
	}
	assets := t.TempDir()
	capAssetsDir = func() string { return assets }
	capStagedModelIDs = func() []string { return []string{"Vis-Q4"} }
	capPropsModalities = func(context.Context, string) *bool { return nil }
	capEntryForModel = func(string) *CatalogEntry {
		return &CatalogEntry{MMProj: &AssetFile{Path: "sub/mmproj-BF16.gguf", Local: "vis-mmproj.gguf"}}
	}
	ctx := context.Background()
	if ManagedModelSupportsVision(ctx, "gpt-4o") != nil || ManagedModelSupportsVision(ctx, "") != nil {
		t.Fatal("not ours")
	}
	if got := ManagedModelSupportsVision(ctx, "Vis-Q4"); got == nil || *got {
		t.Fatal("missing projector must be blind")
	}
	_ = os.WriteFile(filepath.Join(assets, "vis-mmproj.gguf"), []byte("GGUF mmproj"), 0o600)
	if got := ManagedModelSupportsVision(ctx, "Vis-Q4"); got == nil || !*got {
		t.Fatal("projector on disk")
	}
	_ = os.Remove(filepath.Join(assets, "vis-mmproj.gguf"))
	capPropsModalities = func(context.Context, string) *bool { return lr2Bool(true) }
	if got := ManagedModelSupportsVision(ctx, "Vis-Q4"); got == nil || !*got {
		t.Fatal("live props beat catalog")
	}
	if !AcceptedImageMIMEs["image/png"] || AcceptedImageMIMEs["image/webp"] {
		t.Fatal("mimes")
	}
}
