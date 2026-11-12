package hostinfo

import (
	"runtime"
	"strings"
	"testing"
)

func TestNormalizeAndWindowsNativeArch(t *testing.T) {
	if NormalizeArch(" X86_64 ") != "amd64" || NormalizeArch("AARCH64") != "arm64" || NormalizeArch("i686") != "x86" || NormalizeArch("riscv") != "unknown" {
		t.Fatal("normalize")
	}
	if WindowsNativeArch(0xAA64, "AMD64") != "arm64" || WindowsNativeArch(0, "AMD64") != "amd64" || WindowsNativeArch(7, "zz") != "unknown" {
		t.Fatal("windows native")
	}
}

func TestParseCPUInfo(t *testing.T) {
	text := "processor\t: 0\nvendor_id\t: GenuineIntel\nmodel name\t: Intel(R) Core(TM) i9\n"
	if ParseCPUInfo(text, "") != "Intel(R) Core(TM) i9" || ParseCPUInfoVendor(text) != "GenuineIntel" {
		t.Fatal("x86")
	}
	arm := "processor\t: 0\nCPU implementer\t: 0x4e\nHardware\t: NVIDIA GB10\n"
	if ParseCPUInfo(arm, "") != "NVIDIA GB10" || ParseCPUInfoVendor(arm) != "NVIDIA" {
		t.Fatal("arm")
	}
	if ParseCPUInfo("", "NVIDIA_DGX\x00") != "NVIDIA DGX" || ParseCPUInfoVendor("CPU implementer: zz") != "" {
		t.Fatal("device tree")
	}
	if ParseMeminfoTotal("MemTotal:       16384 kB\n") != 16384*1024 || ParseMeminfoTotal("x") != 0 {
		t.Fatal("meminfo")
	}
}

func TestGPUClassification(t *testing.T) {
	if ClassifyGPUVendors(nil) != "none" || ClassifyGPUVendors([]string{"", " "}) != "unknown" {
		t.Fatal("empty")
	}
	if ClassifyGPUVendors([]string{"0x8086", "10DE"}) != "nvidia" || ClassifyGPUVendors([]string{"8086", "1002"}) != "amd" || ClassifyGPUVendors([]string{"ffff"}) != "none" {
		t.Fatal("priority")
	}
	adapters := ParseWindowsDisplayAdapters([][3]string{{"0000", "PCI\\VEN_10DE&DEV_2E03&SUBSYS", "NVIDIA N1X"}, {"0001", "", "Basic"}, {"Properties", "", "x"}, {"0002", "ROOT\\DISPLAY", "Remote"}})
	if len(adapters) != 2 || adapters[0].VendorID != "10DE" || adapters[0].DeviceID != "2E03" || adapters[1].VendorID != "" {
		t.Fatalf("%+v", adapters)
	}
	gpus := NvidiaGPUsFromAdapters(adapters)
	if len(gpus) != 1 || gpus[0].Name != "NVIDIA N1X" || !IsNvidiaN1XPCIID(gpus[0].PCIID) {
		t.Fatalf("%+v", gpus)
	}
	if ParseNvidiaGPUModel("Model:\t NVIDIA GB10\nIRQ: 1") != "NVIDIA GB10" {
		t.Fatal("model")
	}
}

func TestClassifyNvidiaPlatform(t *testing.T) {
	if ClassifyNvidiaPlatform("win32", "arm64", "Snapdragon", "nvidia", []NvidiaGPU{{"x", 0x2E03<<16 | 0x10DE}}) != "rtx_spark" {
		t.Fatal("n1x by pci id")
	}
	if ClassifyNvidiaPlatform("win32", "arm64", "NVIDIA N1X", "none", nil) != "rtx_spark" {
		t.Fatal("n1x by cpu")
	}
	if ClassifyNvidiaPlatform("linux", "arm64", "NVIDIA_GB10", "nvidia", nil) != "dgx_spark" || ClassifyNvidiaPlatform("linux", "arm64", "x", "nvidia", []NvidiaGPU{{"NVIDIA GB10", 0}}) != "dgx_spark" {
		t.Fatal("dgx")
	}
	if ClassifyNvidiaPlatform("linux", "amd64", "x", "nvidia", nil) != "discrete" || ClassifyNvidiaPlatform("linux", "amd64", "x", "amd", nil) != "" {
		t.Fatal("discrete")
	}
	if MachineKind("darwin", "") != "Mac" || MachineKind("win32", "rtx_spark") != "Spark" || MachineKind("linux", "discrete") != "computer" {
		t.Fatal("kind")
	}
}

func TestFullNameAndLocale(t *testing.T) {
	if FullNameFrom("nick", "Nicholas  Heyer") != "Nicholas Heyer" {
		t.Fatal("full name")
	}
	for _, bad := range []string{"", "n", "root", "nick", "p14", "CD_01.05", "lowercase name", "a@b"} {
		if FullNameFrom("nick", bad) != "" {
			t.Fatalf("%q accepted", bad)
		}
	}
	if LinuxLocaleFrom("# x\nLANG=\"de_DE.UTF-8\"\n") != "de-DE" || LinuxLocaleFrom("LC_ALL=C\n") != "" {
		t.Fatal("locale")
	}
	if roundHalfEvenAdjust(62.5) != -1 || roundHalfEvenAdjust(63.5) != 0 || roundHalfEvenAdjust(63.2) != 0 {
		t.Fatal("rounding")
	}
}

func TestRuntimePredicatesAndSummary(t *testing.T) {
	if rootMountHasMarker("/nonexistent", []string{"x"}) {
		t.Fatal("missing file")
	}
	s := HostSummary()
	if s.MachineKind == "" || s.Machine["os_family"] == nil || s.Machine["native_arch"] == nil {
		t.Fatalf("%+v", s)
	}
	m := s.AsMap()
	if _, ok := m["machine"]; !ok || m["machine_kind"] != s.MachineKind {
		t.Fatal(m)
	}
	ClearCaches()
	if NativeArch() == "" {
		t.Fatal("arch after cache clear")
	}
}

func TestGoVersionIsTheReleaseNumber(t *testing.T) {
	got := GoVersion()
	if got == "" || strings.HasPrefix(got, "go") || strings.ContainsAny(got, " \t") && !strings.HasPrefix(got, "devel ") {
		t.Fatalf("GoVersion %q", got)
	}
	if first := strings.Fields(runtime.Version())[0]; first != "devel" && "go"+got != first {
		t.Fatalf("GoVersion %q for runtime %q", got, runtime.Version())
	}
}

func TestModuleVersionOfAnUnlinkedModuleIsEmpty(t *testing.T) {
	if got := ModuleVersion("example.invalid/not-linked"); got != "" {
		t.Fatalf("ModuleVersion %q", got)
	}
}
