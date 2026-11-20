//go:build linux && !386 && !arm && !faketime

package pyopenwakeword

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyctypes"
)

func TestLibraryLoadErrors(t *testing.T) {
	var loadErr *pyctypes.OSError
	absent := filepath.Join(t.TempDir(), "libtensorflowlite_c.so")
	if _, err := newTfLiteWakeWord(absent); !errors.As(err, &loadErr) || !strings.Contains(loadErr.Msg, "cannot open shared object file") {
		t.Fatalf("absent library gave %v", err)
	}
	m := Import(t.TempDir())
	if _, err := m.FromModel("x.tflite", &absent); !errors.As(err, &loadErr) {
		t.Fatalf("classifier over an absent library gave %v", err)
	}
	if _, err := m.FeaturesFromBuiltin(nil, &absent); !errors.As(err, &loadErr) {
		t.Fatalf("features over an absent library gave %v", err)
	}
	var pe *PyError
	if _, err := m.FromBuiltin(ModelOkayNabu, nil, nil); !errors.As(err, &pe) || pe.Msg != "Failed to find tensorflowlite_c library" {
		t.Fatalf("builtin classifier without a library gave %v", err)
	}
	for _, pattern := range []string{"/usr/lib/libm.so.6", "/lib/libm.so.6", "/lib64/libm.so.6", "/usr/lib64/libm.so.6", "/lib/*-linux-gnu/libm.so.6", "/usr/lib/*-linux-gnu/libm.so.6"} {
		if hits, _ := filepath.Glob(pattern); len(hits) > 0 {
			var missing *pyctypes.AttributeError
			if _, err := newTfLiteWakeWord(hits[0]); !errors.As(err, &missing) || !strings.Contains(missing.Msg, "undefined symbol: TfLiteModelCreateFromFile") {
				t.Fatalf("library without the TFLite symbols gave %v", err)
			}
			return
		}
	}
	t.Fatal("no system libm.so.6 found to load")
}
