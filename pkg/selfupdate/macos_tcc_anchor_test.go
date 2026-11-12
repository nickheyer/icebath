package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Fake uv store holding a python3.11 binary and optionally a libpython
func tccBuildStore(t *testing.T, root, version string, withLibpython bool) string {
	t.Helper()
	store := filepath.Join(root, "uv-store", "uv", "python", "cpython-"+version+"-macos-aarch64-none")
	storeBin := filepath.Join(store, "bin")
	if err := os.MkdirAll(storeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeBin, "python3.11"), []byte("#!fake interpreter "+version), 0o755); err != nil {
		t.Fatal(err)
	}
	if withLibpython {
		dbgWrite(t, filepath.Join(store, "lib", "libpython3.11.dylib"), "fake dylib")
	}
	return storeBin
}

// Checkout whose venv symlinks its interpreter into the store
func tccBuildCheckout(t *testing.T, root, storeBin string) string {
	t.Helper()
	checkout := filepath.Join(root, "checkout")
	venvBin := filepath.Join(checkout, ".venv", "bin")
	if err := os.MkdirAll(venvBin, 0o755); err != nil {
		t.Fatal(err)
	}
	dbgWrite(t, filepath.Join(checkout, ".venv", "pyvenv.cfg"), "home = "+storeBin+"\n")
	for _, n := range []string{"python", "python3"} {
		if err := os.Symlink(filepath.Join(storeBin, "python3.11"), filepath.Join(venvBin, n)); err != nil {
			t.Fatal(err)
		}
	}
	return checkout
}

func TestUvStoreDetectionMatchesUvMacosStorePath(t *testing.T) {
	if !tccIsUvMacosStore("/Users/u/.local/share/uv/python/cpython-3.11.15-macos-aarch64-none/bin/python3.11") {
		t.Fatal("not matched")
	}
}

func TestUvStoreDetectionMatchesHermesRuntimeRepairGeneration(t *testing.T) {
	if !tccIsUvMacosStore("/Users/u/icebath-agent/.icebath-runtime/python/generation-a1b2c3/cpython-3.11.15-macos-aarch64-none/bin/python3.11") {
		t.Fatal("not matched")
	}
}

func TestUvStoreDetectionRejectsHomebrewInterpreter(t *testing.T) {
	if tccIsUvMacosStore("/opt/homebrew/Cellar/python@3.14/3.14.6/Frameworks/Python.framework/Versions/3.14/bin/python3.14") {
		t.Fatal("matched")
	}
}

func TestUvStoreDetectionRejectsLinuxInterpreter(t *testing.T) {
	if tccIsUvMacosStore("/usr/bin/python3") {
		t.Fatal("matched")
	}
}

func TestUvStoreDetectionRejectsUvStoreOnLinux(t *testing.T) {
	if tccIsUvMacosStore("/home/u/.local/share/uv/python/cpython-3.11.15-x86_64-unknown-linux-gnu/bin/python3.11") {
		t.Fatal("matched")
	}
}

func TestEnsureTccAnchorNonMacosNoopOnNonMacos(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("runs on hosts that are not macOS")
	}
	root := t.TempDir()
	venvPy := pm.VenvPython(filepath.Join(root, "checkout", ".venv"))
	if err := os.MkdirAll(filepath.Dir(venvPy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(venvPy, []byte("#!fake interpreter"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := ensureMacosTCCAnchor(context.Background(), filepath.Join(root, "checkout"))
	if raw, _ := os.ReadFile(venvPy); err != nil || got != "" || string(raw) != "#!fake interpreter" {
		t.Fatal(got, err, string(raw))
	}
}

// Pins the boot probe to the given outcome, recording the env it received
func tccPinProbe(t *testing.T, stdout string, code int, err error) *[]string {
	t.Helper()
	saved := tccRunProbe
	t.Cleanup(func() { tccRunProbe = saved })
	var seen []string
	tccRunProbe = func(_ []string, env []string, _ time.Duration) (string, int, error) {
		seen = env
		return stdout, code, err
	}
	return &seen
}

func TestBootGateRefusesNonzeroExit(t *testing.T) {
	tmp := t.TempDir()
	tccPinProbe(t, "", 1, nil)
	if tccPassesBootGate(filepath.Join(tmp, "staged"), filepath.Join(tmp, "venv")) {
		t.Fatal("passed")
	}
}

func TestBootGateRefusesWrongPrefix(t *testing.T) {
	tmp := t.TempDir()
	tccPinProbe(t, "/install\n", 0, nil)
	if tccPassesBootGate(filepath.Join(tmp, "staged"), filepath.Join(tmp, "venv")) {
		t.Fatal("passed")
	}
}

func TestBootGateRefusesTimeout(t *testing.T) {
	tmp := t.TempDir()
	tccPinProbe(t, "", -1, context.DeadlineExceeded)
	if tccPassesBootGate(filepath.Join(tmp, "staged"), filepath.Join(tmp, "venv")) {
		t.Fatal("passed")
	}
}

func TestBootGateSkipsUnexecutableStaging(t *testing.T) {
	tmp := t.TempDir()
	tccPinProbe(t, "", -1, &os.PathError{Op: "fork/exec", Path: "staged", Err: syscall.ENOENT})
	if !tccPassesBootGate(filepath.Join(tmp, "staged"), filepath.Join(tmp, "venv")) {
		t.Fatal("refused")
	}
}

func TestBootGateRefusesEaccesAfterChmod(t *testing.T) {
	tmp := t.TempDir()
	tccPinProbe(t, "", -1, &os.PathError{Op: "fork/exec", Path: "staged", Err: syscall.EACCES})
	if tccPassesBootGate(filepath.Join(tmp, "staged"), filepath.Join(tmp, "venv")) {
		t.Fatal("passed")
	}
}

func TestBootGateScrubsPythonhomePythonpath(t *testing.T) {
	tmp := t.TempDir()
	venv := filepath.Join(tmp, "venv")
	os.MkdirAll(venv, 0o755)
	seen := tccPinProbe(t, venv+"\n", 0, nil)
	t.Setenv("PYTHONHOME", "/poison")
	t.Setenv("PYTHONPATH", "/poison")
	if !tccPassesBootGate(filepath.Join(tmp, "staged"), venv) {
		t.Fatal("refused")
	}
	if *seen == nil {
		t.Fatal("gate must pass an explicit scrubbed env")
	}
	for _, kv := range *seen {
		if strings.HasPrefix(kv, "PYTHONHOME=") || strings.HasPrefix(kv, "PYTHONPATH=") {
			t.Fatal(kv)
		}
	}
}

func TestBootGateAcceptsMatchingPrefix(t *testing.T) {
	tmp := t.TempDir()
	venv := filepath.Join(tmp, "venv")
	os.MkdirAll(venv, 0o755)
	tccPinProbe(t, venv+"\n", 0, nil)
	if !tccPassesBootGate(filepath.Join(tmp, "staged"), venv) {
		t.Fatal("refused")
	}
}

func TestTccAnchorStateNonMacosStateSkipOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("runs on Linux")
	}
	tmp := t.TempDir()
	root := tccBuildCheckout(t, tmp, tccBuildStore(t, tmp, "3.11.15", false))
	status, detail, err := macosTCCAnchorState(context.Background(), root)
	if err != nil || status != "skip" || detail != "not macOS" {
		t.Fatal(status, detail, err)
	}
}

// Doctor with its output buffer and the anchor state pinned
func tccDoctor(t *testing.T, state func(context.Context, string) (string, string, error)) (*Doctor, *bytes.Buffer) {
	t.Helper()
	saved := macosTCCAnchorState
	t.Cleanup(func() { macosTCCAnchorState = saved })
	macosTCCAnchorState = state
	out := &bytes.Buffer{}
	return &Doctor{W: out, ProjectRoot: t.TempDir()}, out
}

func TestDoctorCheckSkipIsSilentOnNonMacos(t *testing.T) {
	d, out := tccDoctor(t, func(context.Context, string) (string, string, error) { return "skip", "not macOS", nil })
	if err := doctorCheckMacosTCCAnchor(context.Background(), d, false); err != nil || out.String() != "" {
		t.Fatal(err, out.String())
	}
}

func TestDoctorCheckNeverCrashesOnException(t *testing.T) {
	d, out := tccDoctor(t, func(context.Context, string) (string, string, error) { return "", "", errors.New("tccd down") })
	if err := doctorCheckMacosTCCAnchor(context.Background(), d, false); err != nil || !strings.Contains(out.String(), "macOS TCC anchor check failed") {
		t.Fatal(err, out.String())
	}
}

func TestEnsureTccAnchorMarkerWrittenAtomically(t *testing.T) {
	tmp := t.TempDir()
	source := filepath.Join(tmp, "store", "python3.11")
	dbgWrite(t, source, "#!store")
	venvBin := filepath.Join(tmp, "bin")
	os.MkdirAll(venvBin, 0o755)
	if err := tccWriteMarker(context.Background(), venvBin, source); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(venvBin, ".tcc-anchor-source"))
	left, _ := filepath.Glob(filepath.Join(venvBin, ".tcc-anchor-source.*"))
	if string(raw) != tccMarkerValue(source) || len(left) != 0 {
		t.Fatal(string(raw), left)
	}
}
