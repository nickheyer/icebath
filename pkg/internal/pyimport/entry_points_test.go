package pyimport

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func writeDist(t *testing.T, root, info, text string) {
	t.Helper()
	dir := filepath.Join(root, info)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "entry_points.txt"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEntryPointsSelectsGroupFirstDistributionWinsAndReadsZips(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	writeDist(t, a, "pip_mem-1.0.dist-info", "[icebath_agent.memory_providers]\npipmem = pipmem_pkg:register\n# c\n; c\n\n[console_scripts]\nx = y\n")
	writeDist(t, b, "Pip.Mem-2.0.dist-info", "[icebath_agent.memory_providers]\nshadowed = other\n")
	writeDist(t, b, "flat-0.1.egg-info", "[icebath_agent.memory_providers]\r\n  flatmem =  flatmem  \r\n")
	zpath := filepath.Join(t.TempDir(), "bundle.zip")
	f, err := os.Create(zpath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("zipmem-3.0.dist-info/entry_points.txt")
	w.Write([]byte("[icebath_agent.memory_providers]\nzipmem = zipmem_pkg\n"))
	zw.Close()
	f.Close()
	got := EntryPoints([]string{a, b, zpath, filepath.Join(a, "missing")}, "icebath_agent.memory_providers")
	want := []EntryPoint{
		{Name: "pipmem", Value: "pipmem_pkg:register", Group: "icebath_agent.memory_providers", Dist: filepath.Join(a, "pip_mem-1.0.dist-info")},
		{Name: "flatmem", Value: "flatmem", Group: "icebath_agent.memory_providers", Dist: filepath.Join(b, "flat-0.1.egg-info")},
		{Name: "zipmem", Value: "zipmem_pkg", Group: "icebath_agent.memory_providers", Dist: filepath.Join(zpath, "zipmem-3.0.dist-info")},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d: got %+v want %+v", i, got[i], want[i])
		}
	}
	if eps := EntryPoints([]string{a}, "console_scripts"); len(eps) != 1 || eps[0].Name != "x" || eps[0].Value != "y" {
		t.Fatalf("console_scripts: %+v", eps)
	}
}
