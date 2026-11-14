//go:build linux && !386 && !arm && !faketime

package pyopenwakeword

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyctypes"
)

func TestBindingRunsTheStreamingPipelineThroughTheLibrary(t *testing.T) {
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Fatalf("a C compiler is needed to build the test library: %v", err)
	}
	pkg := filepath.Join(t.TempDir(), "pyopen_wakeword")
	if err := os.MkdirAll(filepath.Join(pkg, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(pkg, "lib", "libtensorflowlite_c.so")
	if msg, err := exec.Command(cc, "-shared", "-fPIC", "-O0", "-ffp-contract=off", "-o", lib, filepath.Join("testdata", "fake_tensorflowlite_c.c")).CombinedOutput(); err != nil {
		t.Fatalf("building the test library: %v\n%s", err, msg)
	}
	m := Import(pkg)
	features, err := m.FeaturesFromBuiltin(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	model, err := m.FromModel(filepath.Join(pkg, "models", "hey_hermes.tflite"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if features.LibtensorflowliteCPath != lib || model.TfliteModel != filepath.Join(pkg, "models", "hey_hermes.tflite") {
		t.Fatalf("library %q model %q", features.LibtensorflowliteCPath, model.TfliteModel)
	}
	replayNumpySimulation(t, features, model)
	var pe *PyError
	if _, err := m.FromBuiltin("absent", nil, nil); !errors.As(err, &pe) || pe.Msg != "TfLiteModelCreateFromFile returned NULL" {
		t.Fatalf("model the library cannot open gave %v", err)
	}
	features.Close()
	model.Close()
	cdll, err := pyctypes.LoadLibrary(lib, true)
	if err != nil {
		t.Fatal(err)
	}
	var deletedModels, deletedInterpreters func() int32
	for name, fptr := range map[string]any{"fake_deleted_models": &deletedModels, "fake_deleted_interpreters": &deletedInterpreters} {
		addr, err := cdll.Symbol(name)
		if err != nil {
			t.Fatal(err)
		}
		pyctypes.RegisterFunc(fptr, addr)
	}
	if deletedModels() != 3 || deletedInterpreters() != 3 {
		t.Fatalf("library released %d models and %d interpreters, want 3 and 3", deletedModels(), deletedInterpreters())
	}
}
