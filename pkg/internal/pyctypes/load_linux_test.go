//go:build linux && !386 && !arm && !faketime

package pyctypes

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func systemLibrary(t *testing.T, name string) string {
	t.Helper()
	for _, dir := range []string{"/usr/lib", "/lib", "/lib64", "/usr/lib64", "/lib/*-linux-gnu", "/usr/lib/*-linux-gnu"} {
		if hits, _ := filepath.Glob(filepath.Join(dir, name)); len(hits) > 0 {
			return hits[0]
		}
	}
	t.Fatalf("no system %s found to load", name)
	return ""
}

func TestLoadLibraryCallsSystemFunctions(t *testing.T) {
	if !Supported {
		t.Fatal("linux build must load shared libraries")
	}
	for _, global := range []bool{false, true} {
		libc, err := LoadLibrary(systemLibrary(t, "libc.so.6"), global)
		if err != nil {
			t.Fatal(err)
		}
		addr, err := libc.Symbol("strlen")
		if err != nil {
			t.Fatal(err)
		}
		var strlen func(s *byte) uintptr
		RegisterFunc(&strlen, addr)
		if n := strlen(&CString("porcupine")[0]); n != 9 {
			t.Fatalf("strlen gave %d", n)
		}
		var abs func(v int32) int32
		if addr, err = libc.Symbol("abs"); err != nil {
			t.Fatal(err)
		}
		RegisterFunc(&abs, addr)
		if abs(-1280) != 1280 {
			t.Fatal("abs gave a wrong value")
		}
		var missing *AttributeError
		if _, err := libc.Symbol("icebath_no_such_symbol"); !errors.As(err, &missing) || !strings.Contains(missing.Msg, "undefined symbol: icebath_no_such_symbol") {
			t.Fatalf("missing symbol gave %v", err)
		}
	}
	var loadErr *OSError
	absent := filepath.Join(t.TempDir(), "libabsent.so")
	if _, err := LoadLibrary(absent, false); !errors.As(err, &loadErr) || !strings.Contains(loadErr.Msg, "cannot open shared object file: No such file or directory") {
		t.Fatalf("absent library gave %v", err)
	}
}
