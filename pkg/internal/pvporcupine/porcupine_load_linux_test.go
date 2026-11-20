//go:build linux && !386 && !arm && !faketime

package pvporcupine

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyctypes"
)

func systemMathLibrary(t *testing.T) string {
	t.Helper()
	for _, pattern := range []string{"/usr/lib/libm.so.6", "/lib/libm.so.6", "/lib64/libm.so.6", "/usr/lib64/libm.so.6", "/lib/*-linux-gnu/libm.so.6", "/usr/lib/*-linux-gnu/libm.so.6"} {
		if hits, _ := filepath.Glob(pattern); len(hits) > 0 {
			return hits[0]
		}
	}
	t.Fatal("no system libm.so.6 found to load")
	return ""
}

func TestPorcupineConstructorValidationOrder(t *testing.T) {
	lib := systemMathLibrary(t)
	dir := t.TempDir()
	model, keyword := filepath.Join(dir, "params.pv"), filepath.Join(dir, "k.ppn")
	for _, path := range []string{model, keyword} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := NewPorcupine("key", lib, filepath.Join(dir, "absent.pv"), "best", []string{keyword}, []float64{0.5})
	wantPyError(t, err, "OSError", "Couldn't find model file at '"+filepath.Join(dir, "absent.pv")+"'.")
	_, err = NewPorcupine("key", lib, model, "", []string{keyword}, []float64{0.5})
	wantPyError(t, err, "ValueError", "`device` should be a non-empty string.")
	_, err = NewPorcupine("key", lib, model, "best", []string{keyword}, []float64{0.5, 0.5})
	wantPyError(t, err, "ValueError", "Number of keywords does not match the number of sensitivities.")
	_, err = NewPorcupine("key", lib, model, "best", []string{"~/icebath-absent-keyword.ppn"}, []float64{0.5})
	wantPyError(t, err, "OSError", "Couldn't find keyword file at '~/icebath-absent-keyword.ppn'.")
	for _, bad := range []float64{1.5, -0.1, math.NaN()} {
		_, err = NewPorcupine("key", lib, model, "best", []string{keyword}, []float64{bad})
		wantPyError(t, err, "ValueError", "A sensitivity value should be within [0, 1].")
	}
	var missing *pyctypes.AttributeError
	if _, err = NewPorcupine("key", lib, model, "best", []string{keyword}, []float64{0, 1}[:1]); !errors.As(err, &missing) ||
		!strings.Contains(missing.Msg, "undefined symbol: pv_set_sdk") {
		t.Fatalf("library without the engine symbols gave %v", err)
	}
	if _, err = ListHardwareDevices(lib); !errors.As(err, &missing) || !strings.Contains(missing.Msg, "undefined symbol: pv_porcupine_list_hardware_devices") {
		t.Fatalf("device listing on a library without the engine symbols gave %v", err)
	}
	var loadErr *pyctypes.OSError
	if _, err = NewPorcupine("key", model, model, "best", []string{keyword}, []float64{0.5}); !errors.As(err, &loadErr) {
		t.Fatalf("unloadable library gave %v", err)
	}
}
