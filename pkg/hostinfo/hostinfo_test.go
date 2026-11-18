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

func TestRuntimePredicatesAndCacheReset(t *testing.T) {
	if rootMountHasMarker("/nonexistent", []string{"x"}) {
		t.Fatal("missing file")
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
