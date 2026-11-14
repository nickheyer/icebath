package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/hostinfo"
)

const (
	lr2GiB        int64 = 1 << 30
	lr2UMASmi     int64 = 16320 << 20
	lr2UMAPool    int64 = 46464 << 20
	lr2UMARAM     int64 = 48 * lr2GiB
	lr2NoEngineID       = "no engine"
)

// Restores every hardware seam and empties the probe caches around one test
func lr2Isolate(t *testing.T) {
	t.Helper()
	saved := []func(){}
	keep := func(restore func()) { saved = append(saved, restore) }
	m, p, w, l, s, r := hwMonotonic, hwSysPlatform, hwWindowsMemoryStatus, hwLinuxMeminfoText, hwStdout, hwRun
	keep(func() {
		hwMonotonic, hwSysPlatform, hwWindowsMemoryStatus, hwLinuxMeminfoText, hwStdout, hwRun = m, p, w, l, s, r
	})
	wh, pe, sp, nv, cu, ed := hwWhich, hwPathExists, hwNvidiaSMIPath, hwNvidiaVRAM, hwCudaDriverPool, hwEngineDevicePool
	keep(func() {
		hwWhich, hwPathExists, hwNvidiaSMIPath, hwNvidiaVRAM, hwCudaDriverPool, hwEngineDevicePool = wh, pe, sp, nv, cu, ed
	})
	dv, up, rb, ad, ce, se := hwDevicePoolView, hwUnifiedPoolBytes, hwRAMBytes, hwAcceleratorDevice, hwConfiguredEngine, hwServingEngine
	keep(func() {
		hwDevicePoolView, hwUnifiedPoolBytes, hwRAMBytes, hwAcceleratorDevice, hwConfiguredEngine, hwServingEngine = dv, up, rb, ad, ce, se
	})
	ie, rv, pd, wa, gc, lc := hwInstalledEngine, hwResolveBackend, hwProbeDevices, hwWindowsGPUAdapters, hwGPUClass, hwLoadConfig
	keep(func() {
		hwInstalledEngine, hwResolveBackend, hwProbeDevices, hwWindowsGPUAdapters, hwGPUClass, hwLoadConfig = ie, rv, pd, wa, gc, lc
	})
	lr2ResetCaches()
	hwWindowsMemoryStatus = func() *windowsMemoryStatusEx { return nil }
	hwWindowsGPUAdapters = func() []hostinfo.GPUAdapter { return nil }
	hwServingEngine = func() *Engine { return nil }
	t.Cleanup(func() {
		for _, restore := range saved {
			restore()
		}
		lr2ResetCaches()
	})
}

func lr2ResetCaches() {
	hwCacheMu.Lock()
	defer hwCacheMu.Unlock()
	hwPoolProbeCache, hwGPUQueryCache, hwSMIPathCache, hwSMIPathCached = nil, nil, nil, false
	hwAcceleratorCache = map[string]hwTimedDevice{}
}

func lr2Pool(total int64, integrated *bool) func() *hwDevicePool {
	return func() *hwDevicePool { return &hwDevicePool{Total: total, Integrated: integrated} }
}

func lr2Bool(b bool) *bool { return &b }

func lr2VRAM(total, free int64, name string, pci *int64) func() (*NvidiaGPUQuery, error) {
	return func() (*NvidiaGPUQuery, error) {
		return &NvidiaGPUQuery{TotalBytes: total, FreeBytes: free, GPUName: name, GPUPCIID: pci}, nil
	}
}

func lr2RAM(total, avail int64) func() (int64, int64, error) {
	return func() (int64, int64, error) { return total, avail, nil }
}

func lr2NoVRAM() (*NvidiaGPUQuery, error) { return nil, nil }

func lr2Probe(t *testing.T, planning bool) HardwareBudget {
	t.Helper()
	b, err := ProbeBudget(planning)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	return b
}

func TestLocalRuntimeLinuxMeminfoParsing(t *testing.T) {
	cases := []struct {
		text         string
		total, avail int64
		ok           bool
	}{
		{"MemTotal:       67108864 kB\nMemFree:        12582912 kB\nMemAvailable:   56623104 kB\nCached:         44040192 kB\n", 64 * lr2GiB, 54 * lr2GiB, true},
		{"MemTotal:       8388608 kB\nMemFree:        2097152 kB\nMemAvailable:         0 kB\n", 8 * lr2GiB, 0, true},
		{"MemTotal:       8388608 kB\nMemFree:        2097152 kB\n", 8 * lr2GiB, 2 * lr2GiB, true},
		{"MemAvailable: 1048576 kB\n", 0, 0, false},
		{"MemTotal: 1048576 kB\nMemAvailable: 2097152 kB\n", 0, 0, false},
	}
	for i, c := range cases {
		total, avail, ok := linuxRAMFromMeminfo(c.text)
		if total != c.total || avail != c.avail || ok != c.ok {
			t.Errorf("case %d: got (%d, %d, %t)", i, total, avail, ok)
		}
	}
}

func lr2AsLinux(t *testing.T, meminfo string, haveMeminfo bool, stdout func(...string) (string, error)) {
	lr2Isolate(t)
	hwSysPlatform = func() string { return "linux" }
	hwLinuxMeminfoText = func() (string, bool) { return meminfo, haveMeminfo }
	hwStdout = stdout
}

func TestLocalRuntimeRAMBytesReadsMeminfoBeforeGetconf(t *testing.T) {
	refuse := func(...string) (string, error) { t.Fatal("getconf must not run"); return "", nil }
	lr2AsLinux(t, "MemTotal:       8388608 kB\nMemFree:        2097152 kB\nMemAvailable:         0 kB\n", true, refuse)
	if total, avail, err := ramBytes(); total != 8*lr2GiB || avail != 0 || err != nil {
		t.Fatalf("zero MemAvailable: %d %d %v", total, avail, err)
	}
	lr2AsLinux(t, "MemTotal:       8388608 kB\nMemFree:        2097152 kB\n", true, refuse)
	if total, avail, _ := ramBytes(); total != 8*lr2GiB || avail != 2*lr2GiB {
		t.Fatalf("MemFree fallback: %d %d", total, avail)
	}
	values := map[string]string{"PAGE_SIZE": "4096\n", "_PHYS_PAGES": "2097152\n", "_AVPHYS_PAGES": "524288\n"}
	lr2AsLinux(t, "", false, func(argv ...string) (string, error) { return values[argv[len(argv)-1]], nil })
	if total, avail, _ := ramBytes(); total != 8*lr2GiB || avail != 2*lr2GiB {
		t.Fatalf("getconf fallback: %d %d", total, avail)
	}
}

func TestLocalRuntimeUnifiedPoolGates(t *testing.T) {
	lr2Isolate(t)
	cases := []struct {
		view          func() *hwDevicePool
		smi, ram, out int64
	}{
		{lr2Pool(lr2UMAPool, lr2Bool(true)), lr2UMASmi, lr2UMARAM, lr2UMAPool},
		{lr2Pool(lr2UMAPool, lr2Bool(false)), lr2UMASmi, lr2UMARAM, 0},
		{lr2Pool(lr2UMAPool, nil), lr2UMASmi, lr2UMARAM, lr2UMAPool},
		{lr2Pool(24*lr2GiB+(200<<20), nil), 24 * lr2GiB, 24 * lr2GiB, 0},
		{lr2Pool(24*lr2GiB+(200<<20), nil), 24 * lr2GiB, 256 * lr2GiB, 0},
		{lr2Pool(48*lr2GiB, nil), 48 * lr2GiB, 48 * lr2GiB, 0},
		{lr2Pool(32*lr2GiB, nil), 16 * lr2GiB, 128 * lr2GiB, 0},
		{func() *hwDevicePool { return nil }, lr2UMASmi, lr2UMARAM, 0},
	}
	for i, c := range cases {
		hwDevicePoolView = c.view
		got := unifiedPoolBytes(c.smi, c.ram)
		if (got == nil) != (c.out == 0) || (got != nil && *got != c.out) {
			t.Errorf("case %d: got %v", i, got)
		}
	}
}

func lr2UMAMachine(t *testing.T, view func() *hwDevicePool) {
	lr2Isolate(t)
	hwNvidiaVRAM = lr2VRAM(lr2UMASmi, 14848<<20, "", nil)
	hwRAMBytes = lr2RAM(lr2UMARAM, 32*lr2GiB)
	hwDevicePoolView = view
}

func TestLocalRuntimeBudgetUnifiedPool(t *testing.T) {
	lr2UMAMachine(t, lr2Pool(lr2UMAPool, lr2Bool(true)))
	b := lr2Probe(t, true)
	if !b.UMA || b.RAMAvailableBytes != 0 || b.TotalDeviceBytes != lr2UMAPool ||
		b.UsableVRAMBytes != int64(lr2F(lr2UMAPool)*0.8) || b.UsableVRAMBytes <= 2*lr2UMASmi {
		t.Fatalf("planning %+v", b)
	}
	live := lr2Probe(t, false)
	if want := int64(lr2F(min(lr2UMAPool, (14848<<20)+32*lr2GiB)) * 0.8); !live.UMA || live.UsableVRAMBytes != want {
		t.Fatalf("live %+v", live)
	}
	hwNvidiaVRAM = lr2NoVRAM
	if b := lr2Probe(t, true); !b.UMA || b.TotalDeviceBytes != lr2UMAPool || b.UsableVRAMBytes != int64(lr2F(lr2UMAPool)*0.8) {
		t.Fatalf("no smi planning %+v", b)
	}
	if b := lr2Probe(t, false); b.UsableVRAMBytes != int64(lr2F(32*lr2GiB)*0.8) {
		t.Fatalf("no smi live %+v", b)
	}
}

func TestLocalRuntimeBudgetDiscreteWhenPoolSaysDiscreteOrIsMissing(t *testing.T) {
	lr2UMAMachine(t, lr2Pool(lr2UMAPool, lr2Bool(false)))
	b := lr2Probe(t, true)
	margin := max(hwMarginFloor, int64(lr2F(lr2UMASmi)*hwMarginFraction))
	if b.UMA || b.TotalDeviceBytes != lr2UMASmi || b.UsableVRAMBytes != lr2UMASmi-margin || b.RAMAvailableBytes != lr2UMARAM {
		t.Fatalf("discrete %+v", b)
	}
	lr2UMAMachine(t, func() *hwDevicePool { return nil })
	if b := lr2Probe(t, true); b.UMA || b.RAMAvailableBytes != lr2UMARAM {
		t.Fatalf("unavailable %+v", b)
	}
}

func TestLocalRuntimeEngineFallbackWithoutSmiStaysConservative(t *testing.T) {
	lr2Isolate(t)
	hwNvidiaVRAM = lr2NoVRAM
	hwRAMBytes = lr2RAM(lr2UMARAM, 32*lr2GiB)
	hwDevicePoolView = lr2Pool(lr2UMAPool, nil)
	hwConfiguredEngine = func() (*Engine, error) { return nil, nil }
	hwResolveBackend = func(string) (string, error) { return "cpu", nil }
	if b := lr2Probe(t, true); !b.UMA || b.TotalDeviceBytes != lr2UMARAM {
		t.Fatalf("%+v", b)
	}
}

func TestLocalRuntimeVulkanDeviceTypeDecidesDiscreteVsUnified(t *testing.T) {
	for _, c := range []struct {
		kind     int
		discrete bool
	}{{GGMLDeviceGPU, true}, {GGMLDeviceIGPU, false}} {
		lr2Isolate(t)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("local_runtime:\n  backend: vulkan\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ICEBATH_HOME", dir)
		total := int64(16304 << 20)
		var requested []string
		hwInstalledEngine = func(backend string) (*Engine, error) {
			requested = append(requested, backend)
			return &Engine{Backend: "vulkan", Tag: "b10964", Binary: filepath.Join(dir, "engine", "llama-server")}, nil
		}
		hwProbeDevices = func(directory, backend string) []GGMLDevice {
			return []GGMLDevice{{Description: "AMD Radeon RX 9060 XT", Type: c.kind, Free: total, Total: total}}
		}
		hwNvidiaVRAM = lr2NoVRAM
		hwDevicePoolView = func() *hwDevicePool { return nil }
		hwRAMBytes = lr2RAM(32*lr2GiB, 20*lr2GiB)
		b := lr2Probe(t, true)
		if len(requested) != 1 || requested[0] != "vulkan" || b.UMA == c.discrete {
			t.Fatalf("type %d: requested %v budget %+v", c.kind, requested, b)
		}
		if c.discrete && (b.TotalDeviceBytes != total || b.UsableVRAMBytes != total-hwMarginFloor || b.RAMAvailableBytes != 32*lr2GiB) {
			t.Fatalf("discrete %+v", b)
		}
		if !c.discrete && (b.TotalDeviceBytes != 32*lr2GiB || b.RAMAvailableBytes != 0) {
			t.Fatalf("integrated %+v", b)
		}
	}
}

func TestLocalRuntimeSmiResolverUsesWSLDriverPath(t *testing.T) {
	lr2Isolate(t)
	hwSysPlatform = func() string { return "linux" }
	hwWhich = func(string) (string, error) { return "", os.ErrNotExist }
	hwPathExists = func(p string) bool { return p == "/usr/lib/wsl/lib/nvidia-smi" }
	if got, ok := nvidiaSMIPath(); !ok || got != "/usr/lib/wsl/lib/nvidia-smi" {
		t.Fatalf("got %q %t", got, ok)
	}
}

func lr2SmiOutput(t *testing.T, output string, calls *int) {
	hwNvidiaSMIPath = func() (string, bool) { return "nvidia-smi", true }
	hwRun = func(argv []string, timeout time.Duration, dir string) (hwRunResult, error) {
		*calls++
		if argv[0] != "nvidia-smi" || argv[1] != hwGPUQueryArg {
			t.Fatalf("argv %v", argv)
		}
		return hwRunResult{Stdout: output}, nil
	}
}

func TestLocalRuntimeMemoryProbeCarriesIdentityWithoutAnotherProcess(t *testing.T) {
	lr2Isolate(t)
	name := "NVIDIA RTX Spark N1X (5120-core Blackwell RTX GPU)"
	calls := 0
	lr2SmiOutput(t, fmt.Sprintf("32704, 31423, %s, 0x2E0310DE, 2048, 7\n", name), &calls)
	hwRAMBytes = lr2RAM(64*lr2GiB, 22*lr2GiB)
	hwDevicePoolView = lr2Pool(lr2UMAPool, lr2Bool(true))
	b := lr2Probe(t, true)
	if b.GPUName != name || b.GPUPCIID == nil || *b.GPUPCIID != 0x2E0310DE || b.Platform != hostinfo.OSFamily() ||
		b.TotalDeviceBytes != lr2UMAPool || b.UsableVRAMBytes != int64(lr2F(lr2UMAPool)*.8) || calls != 1 {
		t.Fatalf("%+v calls %d", b, calls)
	}
}

func TestLocalRuntimePCIIdentityParsingKeepsMemory(t *testing.T) {
	n1x := "NVIDIA RTX Spark N1X"
	cases := []struct {
		pci, name string
		want      *int64
		matches   bool
	}{
		{"0x2E0010DE", "Renamed, GPU", ptrInt64(0x2E0010DE), true}, {"0x2e0310de", "", ptrInt64(0x2E0310DE), true},
		{"0x2E0610DE", "Renamed GPU", ptrInt64(0x2E0610DE), true}, {"0x2E1310DE", "Renamed GPU", ptrInt64(0x2E1310DE), true},
		{"0x2E2A10DE", "Renamed GPU", ptrInt64(0x2E2A10DE), true}, {"0x2E3F10DE", "Renamed GPU", ptrInt64(0x2E3F10DE), true},
		{"0x2DFF10DE", n1x, ptrInt64(0x2DFF10DE), false}, {"0x2E4010DE", n1x, ptrInt64(0x2E4010DE), false},
		{"0x2E8610DE", n1x, ptrInt64(0x2E8610DE), false}, {"0x2E031002", n1x, ptrInt64(0x2E031002), false},
		{"0x00000000", n1x, ptrInt64(0), false}, {"-0x2E0310DE", n1x, ptrInt64(-0x2E0310DE), false},
		{"0x12E0310DE", n1x, ptrInt64(0x12E0310DE), false}, {"N/A", n1x + " (updated description)", nil, true},
		{"[Not Supported]", n1x, nil, true}, {"malformed", "Renamed GPU", nil, false},
		{"0x2E0310DE00", "Renamed GPU", ptrInt64(0x2E0310DE00), false},
	}
	entry := CatalogByID()["qwen3.8-27b"]
	variant := entry.Variants[0]
	recommended := func(b HardwareBudget) string {
		e, reason := RecommendedEntry(b, nil, "auto")
		if e == nil {
			return "none " + reason
		}
		return e.ID + " " + reason
	}
	for _, c := range cases {
		lr2Isolate(t)
		calls := 0
		lr2SmiOutput(t, fmt.Sprintf("32704, 31423, \"%s\", %s, 2048, 7\n32704, 31423, NVIDIA RTX Spark N1X, 0x2E0310DE, 2048, 7\n", c.name, c.pci), &calls)
		hwRAMBytes = lr2RAM(64<<30, 22<<30)
		hwDevicePoolView = lr2Pool(48<<30, lr2Bool(true))
		b := lr2Probe(t, true)
		if b.GPUName != c.name || (b.GPUPCIID == nil) != (c.want == nil) || (c.want != nil && *b.GPUPCIID != *c.want) ||
			b.TotalDeviceBytes != 48<<30 || b.UsableVRAMBytes != int64(lr2F(48<<30)*.8) || calls != 1 {
			t.Errorf("%s: %+v", c.pci, b)
		}
		// The measured tables key on win32, the host this identity check runs on
		b.Platform = "win32"
		reference, generic := b, b
		reference.GPUName, reference.GPUPCIID = n1x, nil
		generic.GPUName, generic.GPUPCIID = "", nil
		expected := generic
		if c.matches {
			expected = reference
		}
		if recommended(b) != recommended(expected) {
			t.Errorf("%s: recommended %s want %s", c.pci, recommended(b), recommended(expected))
		}
		if PredictedDecodeTokS(entry, variant, reference, false, "auto") == PredictedDecodeTokS(entry, variant, generic, false, "auto") ||
			PredictedDecodeTokS(entry, variant, b, false, "auto") != PredictedDecodeTokS(entry, variant, expected, false, "auto") {
			t.Errorf("%s: predicted decode", c.pci)
		}
		for _, backend := range []string{"cpu", "vulkan"} {
			if PredictedDecodeTokS(entry, variant, b, false, backend) != PredictedDecodeTokS(entry, variant, generic, false, backend) {
				t.Errorf("%s: identity overrode %s eligibility", c.pci, backend)
			}
		}
		if PredictedDecodeTokS(entry, variant, b, true, "auto") != PredictedDecodeTokS(entry, variant, generic, true, "auto") {
			t.Errorf("%s: identity overrode placement", c.pci)
		}
	}
	for _, integrated := range []bool{false, true} {
		lr2Isolate(t)
		calls := 0
		lr2SmiOutput(t, "32704, 31423, NVIDIA RTX Spark N1X, N/A, 2048, 7\n", &calls)
		hwRAMBytes = lr2RAM(64<<30, 22<<30)
		hwDevicePoolView = lr2Pool(48<<30, lr2Bool(integrated))
		b := lr2Probe(t, true)
		total := int64(32704 << 20)
		usable := total - max(2<<30, int64(lr2F(total)*.09))
		if integrated {
			total = 48 << 30
			usable = int64(lr2F(total) * .8)
		}
		if b.GPUPCIID != nil || b.UMA != integrated || b.TotalDeviceBytes != total || b.UsableVRAMBytes != usable || calls != 1 {
			t.Errorf("integrated %t: %+v", integrated, b)
		}
	}
}

func ptrInt64(v int64) *int64 { return &v }

// Runtime float conversion so test arithmetic rounds the way the probe does
func lr2F(v int64) float64 { return float64(v) }

func TestLocalRuntimePoolProbeMissRetriesAfterTTL(t *testing.T) {
	lr2Isolate(t)
	hwCudaDriverPool = func() *hwDevicePool { return nil }
	answers := []*hwDevicePool{nil, {Total: lr2UMAPool}}
	hwEngineDevicePool = func() *hwDevicePool { a := answers[0]; answers = answers[1:]; return a }
	now := 1000.0
	hwMonotonic = func() float64 { return now }
	if devicePoolView() != nil {
		t.Fatal("first probe")
	}
	now += 1
	if devicePoolView() != nil {
		t.Fatal("cached miss")
	}
	now += hwPoolNegativeTTLSeconds + 1
	if v := devicePoolView(); v == nil || v.Total != lr2UMAPool || v.Integrated != nil || len(answers) != 0 {
		t.Fatalf("retry %+v", v)
	}
}

func TestLocalRuntimeDeviceLineRegexHandlesParenthesizedNames(t *testing.T) {
	m := hwDeviceLineRE.FindStringSubmatch("  CUDA0: NVIDIA Example Device (1234-core Example GPU) (46464 MiB, 46284 MiB free)")
	if m == nil || m[1] != "46464" {
		t.Fatalf("%v", m)
	}
}
