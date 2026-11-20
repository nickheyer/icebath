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
	if !IsNvidiaN1XPCIID(0x2E03<<16 | 0x10DE) {
		t.Fatal("N1X PCI id")
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
