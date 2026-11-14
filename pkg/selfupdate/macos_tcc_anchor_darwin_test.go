package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Checkout of the given store, anchored with a copied interpreter and marker when asked
func tccDarwinCheckout(t *testing.T, root, storeBin string, anchored bool) string {
	t.Helper()
	if !anchored {
		return tccBuildCheckout(t, root, storeBin)
	}
	checkout := filepath.Join(root, "checkout")
	venvBin := filepath.Join(checkout, ".venv", "bin")
	if err := os.MkdirAll(venvBin, 0o755); err != nil {
		t.Fatal(err)
	}
	dbgWrite(t, filepath.Join(checkout, ".venv", "pyvenv.cfg"), "home = "+storeBin+"\n")
	storePy := filepath.Join(storeBin, "python3.11")
	raw, _ := os.ReadFile(storePy)
	if err := os.WriteFile(filepath.Join(venvBin, "python"), raw, 0o755); err != nil {
		t.Fatal(err)
	}
	dbgWrite(t, filepath.Join(venvBin, ".tcc-anchor-source"), storePy)
	if err := os.Symlink(filepath.Join(venvBin, "python"), filepath.Join(venvBin, "python3")); err != nil {
		t.Fatal(err)
	}
	return checkout
}

// Checkout whose venv points at a homebrew interpreter
func tccHomebrewCheckout(t *testing.T, root string) string {
	t.Helper()
	brew := filepath.Join(root, "opt", "homebrew", "bin")
	brewPy := filepath.Join(brew, "python3.14")
	dbgWrite(t, brewPy, "#!homebrew")
	os.Chmod(brewPy, 0o755)
	checkout := filepath.Join(root, "checkout")
	venvBin := filepath.Join(checkout, ".venv", "bin")
	os.MkdirAll(venvBin, 0o755)
	dbgWrite(t, filepath.Join(checkout, ".venv", "pyvenv.cfg"), "home = "+brew+"\n")
	for _, n := range []string{"python", "python3"} {
		if err := os.Symlink(brewPy, filepath.Join(venvBin, n)); err != nil {
			t.Fatal(err)
		}
	}
	return checkout
}

func tccRead(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func tccEnsure(t *testing.T, root string) string {
	t.Helper()
	got, err := ensureMacosTCCAnchor(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestEnsureTccAnchorInstallSignsTheAnchorCopy(t *testing.T) {
	tmp := t.TempDir()
	saved := tccSignManagedPython
	t.Cleanup(func() { tccSignManagedPython = saved })
	var signed []string
	tccSignManagedPython = func(p string) bool { signed = append(signed, p); return true }
	root := tccBuildCheckout(t, tmp, tccBuildStore(t, tmp, "3.11.15", false))
	anchored := tccEnsure(t, root)
	if anchored == "" || len(signed) != 1 || filepath.Dir(signed[0]) != filepath.Dir(anchored) {
		t.Fatal(anchored, signed)
	}
}

func TestEnsureTccAnchorAnchorsRepairGenerationInterpreter(t *testing.T) {
	tmp := t.TempDir()
	storeBin := filepath.Join(tmp, "checkout", ".icebath-runtime", "python", "generation-a1b2c3", "cpython-3.11.15-macos-aarch64-none", "bin")
	storePy := filepath.Join(storeBin, "python3.11")
	dbgWrite(t, storePy, "#!fake generation interpreter")
	os.Chmod(storePy, 0o755)
	root := tccBuildCheckout(t, tmp, storeBin)
	venvPy := pm.VenvPython(filepath.Join(root, ".venv"))
	if !tccIsSymlink(venvPy) {
		t.Fatal("fixture venv is not a symlink")
	}
	if anchored := tccEnsure(t, root); anchored != venvPy || tccIsSymlink(venvPy) || tccRead(t, venvPy) != tccRead(t, storePy) {
		t.Fatal(anchored)
	}
}

func TestEnsureTccAnchorAnchorsUvManagedInterpreter(t *testing.T) {
	tmp := t.TempDir()
	storeBin := tccBuildStore(t, tmp, "3.11.15", false)
	root := tccBuildCheckout(t, tmp, storeBin)
	venvPy := pm.VenvPython(filepath.Join(root, ".venv"))
	if anchored := tccEnsure(t, root); anchored != venvPy || tccIsSymlink(venvPy) || tccRead(t, venvPy) != tccRead(t, filepath.Join(storeBin, "python3.11")) {
		t.Fatal(anchored)
	}
	if st, _ := os.Stat(venvPy); st.Mode()&0o111 == 0 {
		t.Fatal("anchor not executable")
	}
	if strings.TrimSpace(tccRead(t, filepath.Join(filepath.Dir(venvPy), ".tcc-anchor-source"))) != filepath.Join(storeBin, "python3.11") {
		t.Fatal("marker")
	}
	alias := filepath.Join(filepath.Dir(venvPy), "python3")
	if tccIsSymlink(alias) || tccRead(t, alias) != tccRead(t, venvPy) {
		t.Fatal("alias")
	}
}

func TestEnsureTccAnchorIdempotent(t *testing.T) {
	tmp := t.TempDir()
	root := tccDarwinCheckout(t, tmp, tccBuildStore(t, tmp, "3.11.15", false), true)
	venvPy := pm.VenvPython(filepath.Join(root, ".venv"))
	marker := filepath.Join(filepath.Dir(venvPy), ".tcc-anchor-source")
	before := tccRead(t, marker)
	if anchored := tccEnsure(t, root); anchored != venvPy || tccIsSymlink(venvPy) || tccRead(t, marker) != before {
		t.Fatal(anchored)
	}
}

func TestEnsureTccAnchorRepairsAliasSymlinksLeftByPredecessor(t *testing.T) {
	tmp := t.TempDir()
	root := tccDarwinCheckout(t, tmp, tccBuildStore(t, tmp, "3.11.15", false), true)
	venvBin := filepath.Join(root, ".venv", "bin")
	venvPy := filepath.Join(venvBin, "python")
	if anchored := tccEnsure(t, root); anchored != venvPy {
		t.Fatal(anchored)
	}
	alias := filepath.Join(venvBin, "python3")
	if tccIsSymlink(alias) || tccRead(t, alias) != tccRead(t, venvPy) {
		t.Fatal("alias")
	}
}

func TestEnsureTccAnchorReanchorsAfterPatchBump(t *testing.T) {
	tmp := t.TempDir()
	root := tccDarwinCheckout(t, tmp, tccBuildStore(t, tmp, "3.11.15", false), true)
	venvPy := pm.VenvPython(filepath.Join(root, ".venv"))
	newBin := tccBuildStore(t, tmp, "3.11.16", false)
	newPy := filepath.Join(newBin, "python3.11")
	os.Remove(venvPy)
	os.Symlink(newPy, venvPy)
	dbgWrite(t, filepath.Join(root, ".venv", "pyvenv.cfg"), "home = "+newBin+"\n")
	if anchored := tccEnsure(t, root); anchored != venvPy || tccIsSymlink(venvPy) || tccRead(t, venvPy) != tccRead(t, newPy) {
		t.Fatal(anchored)
	}
	if strings.TrimSpace(tccRead(t, filepath.Join(filepath.Dir(venvPy), ".tcc-anchor-source"))) != newPy {
		t.Fatal("marker")
	}
	alias := filepath.Join(filepath.Dir(venvPy), "python3")
	if tccIsSymlink(alias) || tccRead(t, alias) != tccRead(t, newPy) {
		t.Fatal("alias")
	}
}

func TestEnsureTccAnchorSkipsHomebrewInterpreter(t *testing.T) {
	root := tccHomebrewCheckout(t, t.TempDir())
	venvPy := pm.VenvPython(filepath.Join(root, ".venv"))
	if got := tccEnsure(t, root); got != "" || !tccIsSymlink(venvPy) {
		t.Fatal(got)
	}
}

func TestEnsureTccAnchorNoVenvReturnsNone(t *testing.T) {
	if got := tccEnsure(t, filepath.Join(t.TempDir(), "missing")); got != "" {
		t.Fatal(got)
	}
}

func TestEnsureTccAnchorPreservesStdlibSourceHome(t *testing.T) {
	tmp := t.TempDir()
	storeBin := tccBuildStore(t, tmp, "3.11.15", false)
	root := tccBuildCheckout(t, tmp, storeBin)
	tccEnsure(t, root)
	if !strings.Contains(tccRead(t, filepath.Join(root, ".venv", "pyvenv.cfg")), "home = "+storeBin) {
		t.Fatal("pyvenv.cfg home changed")
	}
}

func TestEnsureTccAnchorProvisionsLibpythonAsHardlinkWhenPresent(t *testing.T) {
	tmp := t.TempDir()
	storeBin := tccBuildStore(t, tmp, "3.11.15", true)
	root := tccBuildCheckout(t, tmp, storeBin)
	src := filepath.Join(filepath.Dir(storeBin), "lib", "libpython3.11.dylib")
	tccEnsure(t, root)
	dst := filepath.Join(root, ".venv", "lib", "libpython3.11.dylib")
	srcSt, _ := os.Stat(src)
	dstSt, err := os.Stat(dst)
	if err != nil || tccRead(t, dst) != tccRead(t, src) || srcSt.Sys().(*syscall.Stat_t).Ino != dstSt.Sys().(*syscall.Stat_t).Ino {
		t.Fatal(err)
	}
}

func TestEnsureTccAnchorBootGateRefusalLeavesVenvUntouched(t *testing.T) {
	tmp := t.TempDir()
	root := tccBuildCheckout(t, tmp, tccBuildStore(t, tmp, "3.11.15", false))
	venvPy := pm.VenvPython(filepath.Join(root, ".venv"))
	saved := tccPassesBootGate
	t.Cleanup(func() { tccPassesBootGate = saved })
	tccPassesBootGate = func(string, string) bool { return false }
	if got := tccEnsure(t, root); got != "" || !tccIsSymlink(venvPy) {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(venvPy), ".tcc-anchor-source")); err == nil {
		t.Fatal("marker written")
	}
}

// Captures the anchor module's warnings
func tccCaptureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	saved := tccLog
	t.Cleanup(func() { tccLog = saved })
	buf := &bytes.Buffer{}
	tccLog = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return buf
}

func TestEnsureTccAnchorAliasFailureLeavesAnchorUnmarked(t *testing.T) {
	tmp := t.TempDir()
	root := tccBuildCheckout(t, tmp, tccBuildStore(t, tmp, "3.11.15", false))
	venvPy := pm.VenvPython(filepath.Join(root, ".venv"))
	logs := tccCaptureLog(t)
	saved := tccCopyAlias
	tccCopyAlias = func(string, string, string) bool { return false }
	tccEnsure(t, root)
	if _, err := os.Stat(filepath.Join(filepath.Dir(venvPy), ".tcc-anchor-source")); err == nil {
		t.Fatal("marker written")
	}
	if status, _, _ := macosTCCAnchorState(context.Background(), root); status == "active" || !strings.Contains(logs.String(), "alias") {
		t.Fatal(status, logs.String())
	}
	tccCopyAlias = saved
	if got := tccEnsure(t, root); got == "" {
		t.Fatal("retry did not anchor")
	}
	if status, _, _ := macosTCCAnchorState(context.Background(), root); status != "active" {
		t.Fatal(status)
	}
}

func TestEnsureTccAnchorCopyAliasFailureWarnsAndCleansStaging(t *testing.T) {
	tmp := t.TempDir()
	anchor := filepath.Join(tmp, "anchor")
	dbgWrite(t, anchor, "#!anchor")
	os.Chmod(anchor, 0o755)
	logs := tccCaptureLog(t)
	saved := tccCopy2
	t.Cleanup(func() { tccCopy2 = saved })
	tccCopy2 = func(string, string) error {
		return &os.PathError{Op: "copy", Path: anchor, Err: errors.New("No space left on device")}
	}
	if tccCopyAlias(tmp, "python3", anchor) {
		t.Fatal("copied")
	}
	left, _ := filepath.Glob(filepath.Join(tmp, ".python3.tcc-*"))
	if !strings.Contains(logs.String(), "python3") || len(left) != 0 {
		t.Fatal(logs.String(), left)
	}
}

func TestTccAnchorStateStateActiveThroughUnpatchedHomeSymlink(t *testing.T) {
	tmp := t.TempDir()
	patched := tccBuildStore(t, tmp, "3.11.15", false)
	versionless := filepath.Join(filepath.Dir(filepath.Dir(patched)), "cpython-3.11-macos-aarch64-none")
	os.Symlink(filepath.Dir(patched), versionless)
	homeBin := filepath.Join(versionless, "bin")
	root := filepath.Join(tmp, "checkout")
	venvBin := filepath.Join(root, ".venv", "bin")
	os.MkdirAll(venvBin, 0o755)
	dbgWrite(t, filepath.Join(root, ".venv", "pyvenv.cfg"), "home = "+homeBin+"\n")
	os.Symlink(filepath.Join(homeBin, "python3.11"), filepath.Join(venvBin, "python"))
	tccEnsure(t, root)
	if status, _, _ := macosTCCAnchorState(context.Background(), root); status != "active" {
		t.Fatal(status)
	}
}

func TestTccAnchorStateStateMissingThenActive(t *testing.T) {
	tmp := t.TempDir()
	root := tccBuildCheckout(t, tmp, tccBuildStore(t, tmp, "3.11.15", false))
	status, detail, _ := macosTCCAnchorState(context.Background(), root)
	if status != "missing" || !strings.Contains(detail, pm.VenvPython(filepath.Join(root, ".venv"))) {
		t.Fatal(status, detail)
	}
	tccEnsure(t, root)
	if status, _, _ := macosTCCAnchorState(context.Background(), root); status != "active" {
		t.Fatal(status)
	}
}

func TestTccAnchorStateStateSkipForHomebrew(t *testing.T) {
	root := tccHomebrewCheckout(t, t.TempDir())
	if status, detail, _ := macosTCCAnchorState(context.Background(), root); status != "skip" || !strings.Contains(detail, "not uv-managed") {
		t.Fatal(status, detail)
	}
}

func TestTccAnchorStateStateStaleAfterPatchBump(t *testing.T) {
	tmp := t.TempDir()
	root := tccDarwinCheckout(t, tmp, tccBuildStore(t, tmp, "3.11.15", false), true)
	newBin := tccBuildStore(t, tmp, "3.11.16", false)
	dbgWrite(t, filepath.Join(root, ".venv", "pyvenv.cfg"), "home = "+newBin+"\n")
	if status, _, _ := macosTCCAnchorState(context.Background(), root); status != "stale" {
		t.Fatal(status)
	}
	venvPy := pm.VenvPython(filepath.Join(root, ".venv"))
	if anchored := tccEnsure(t, root); anchored != venvPy || tccRead(t, venvPy) != tccRead(t, filepath.Join(newBin, "python3.11")) {
		t.Fatal(anchored)
	}
	if status, _, _ := macosTCCAnchorState(context.Background(), root); status != "active" {
		t.Fatal(status)
	}
}
