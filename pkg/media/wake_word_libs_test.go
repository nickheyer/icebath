package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pvporcupine"
	"github.com/nickheyer/icebath/pkg/internal/pyopenwakeword"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Empty install tree with a venv record, returning the context and the site packages directory PM selects in it
func wakeLibsInstall(t *testing.T) (context.Context, string) {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(pm.PatchRepoRoot(root))
	ctx := home.WithOverride(context.Background(), t.TempDir())
	venv, err := pm.SelectedVenv(ctx, root)
	if err != nil || venv == "" {
		t.Fatalf("selected venv %q %v", venv, err)
	}
	if err := os.MkdirAll(venv, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venv, "pyvenv.cfg"), []byte("home = /usr/bin\nversion = 3.13.1\nversion_info = 3.13.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	site, err := pm.SitePackages(venv)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, site
}

func wakeLibsTouch(t *testing.T, parts ...string) {
	t.Helper()
	path := filepath.Join(parts...)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWakeLibraryImportsWithoutTheDistributions(t *testing.T) {
	ctx, _ := wakeLibsInstall(t)
	check := func(name string, err error) {
		t.Helper()
		var we *WakeWordError
		if !errors.As(err, &we) || we.Class != "ModuleNotFoundError" || we.Msg != "No module named '"+name+"'" {
			t.Fatalf("import of %s gave %v", name, err)
		}
	}
	_, err := importPyOpenWakeWord(ctx)
	check("pyopen_wakeword", err)
	_, err = importPvporcupine(ctx)
	check("pvporcupine", err)
	_, err = importSherpaOnnx(ctx)
	check("sherpa_onnx", err)
}

func TestWakeLibraryImportsFromTheSelectedEnvironment(t *testing.T) {
	ctx, site := wakeLibsInstall(t)
	wakeLibsTouch(t, site, "pvporcupine", "__init__.py")
	for _, platform := range []string{"linux", "mac", "windows", "raspberry-pi"} {
		wakeLibsTouch(t, site, "pvporcupine", "resources", "keyword_files", platform, "jarvis_"+platform+".ppn")
	}
	wakeLibsTouch(t, site, "pyopen_wakeword", "__init__.py")
	wakeLibsTouch(t, site, "sherpa_onnx", "__init__.py")

	porcupine, err := importPvporcupine(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pvErr *pvporcupine.PyError
	if _, err := porcupine.Create("key", nil, []string{"computer"}, []float64{0.5}); !errors.As(err, &pvErr) || pvErr.Class != "ValueError" ||
		pvErr.Msg != `One or more keywords are not available by default. Available default keywords are:\njarvis` {
		t.Fatalf("unknown keyword gave %v", err)
	}
	if _, err := porcupine.Create("key", nil, []string{"jarvis"}, []float64{0.5}); !errors.As(err, &pvErr) || pvErr.Class != "OSError" {
		t.Fatalf("tree without the dynamic library gave %v", err)
	}
	if _, err := porcupine.Create("key", []string{"custom.ppn"}, nil, []float64{0.5, 0.5}); !errors.As(err, &pvErr) ||
		pvErr.Msg != "Number of keywords does not match the number of sensitivities." {
		t.Fatalf("mismatched sensitivities gave %v", err)
	}

	oww, err := importPyOpenWakeWord(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var owwErr *pyopenwakeword.PyError
	if _, err := oww.FeaturesFromBuiltin(); !errors.As(err, &owwErr) || owwErr.Class != "ValueError" {
		t.Fatalf("features without the TFLite library gave %v", err)
	}
	if _, err := oww.ModelFromModel(filepath.Join(site, "hey_hermes.tflite")); !errors.As(err, &owwErr) || owwErr.Class != "ValueError" {
		t.Fatalf("classifier without the TFLite library gave %v", err)
	}

	var we *WakeWordError
	if _, err := importSherpaOnnx(ctx); !errors.As(err, &we) || we.Class != "ImportError" || we.Msg == "" {
		t.Fatalf("sherpa_onnx without its native library gave %v", err)
	}
}
