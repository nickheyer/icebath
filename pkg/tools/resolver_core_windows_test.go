package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsPathextIsHonoredWithoutMutatingEnviron(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "tool.cmd")
	if err := os.WriteFile(exe, []byte("@echo off\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, hadBefore := os.LookupEnv("PATHEXT")
	res := locateCommand("tool", &commandLookupContext{Path: strPtr(dir), PathExt: strPtr(".CMD")}, nil)
	if res.Kind != "path_executable" || !strings.EqualFold(res.Command()[0], exe) {
		t.Fatalf("%+v", res)
	}
	if after, hadAfter := os.LookupEnv("PATHEXT"); after != before || hadAfter != hadBefore {
		t.Fatalf("PATHEXT changed from %q to %q", before, after)
	}
}

func TestKnownDirVariantsReadAnEmptyPathextAsEmpty(t *testing.T) {
	t.Setenv("PATHEXT", "")
	if got := knownDirVariants(`C:\x\tool`); len(got) != 1 {
		t.Fatalf("%v", got)
	}
}
