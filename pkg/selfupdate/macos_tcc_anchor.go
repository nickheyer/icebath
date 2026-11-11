package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

const tccMarkerName = ".tcc-anchor-source"

var (
	tccStoreCommonMarkers = []string{"cpython-", "-macos-"}
	tccStoreRootMarkers   = []string{"/uv/python/", "/.icebath-runtime/python/"}
	tccAliasPattern       = regexp.MustCompile(`^python3(\.\d+)?$`)
	tccLog                = slog.Default().With("logger", "hermes_cli.macos_tcc_anchor")
	tccIsMacOS            = func() bool { return runtime.GOOS == "darwin" }
	tccSignManagedPython  = pm.SignManagedPython
	tccCopy2              = pyshutil.Copy2
)

// Staged copy that refused to boot, the live venv staying untouched
type tccBootGateFailed struct{ msg string }

func (e *tccBootGateFailed) Error() string { return e.msg }

// Fully resolved path so symlinked spellings of one store binary compare equal
func tccMarkerValue(sourceFile string) string {
	if real, err := filepath.EvalSymlinks(sourceFile); err == nil {
		return real
	}
	abs, _ := filepath.Abs(sourceFile)
	return abs
}

// Whether a path is a uv managed macOS store interpreter
func tccIsUvMacosStore(path string) bool {
	normalized := strings.ReplaceAll(path, "\\", "/")
	for _, m := range tccStoreCommonMarkers {
		if !strings.Contains(normalized, m) {
			return false
		}
	}
	for _, m := range tccStoreRootMarkers {
		if strings.Contains(normalized, m) {
			return true
		}
	}
	return false
}

// Minor version of the interpreter a venv holds, the alias and store names derive from it
func tccPythonMinor(venvDir string) string {
	_, minor, ok, err := pm.VenvPythonVersion(venvDir)
	if err != nil || !ok {
		return ""
	}
	return minor
}

func tccAliasNames(minor string) []string {
	if minor == "" {
		return []string{"python3"}
	}
	return []string{"python3", "python3." + minor}
}

func tccStoreBinNames(minor string) []string {
	if minor == "" {
		return []string{"python3", "python"}
	}
	return []string{"python3." + minor, "python3", "python"}
}

func tccPresent(path string) bool {
	st, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if st.Mode()&fs.ModeSymlink != 0 {
		return true
	}
	st, err = os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

func tccIsFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

func tccIsSymlink(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.Mode()&fs.ModeSymlink != 0
}

// First venv or .venv of the checkout holding an interpreter, empty when neither does
func tccVenvDir(projectRoot string) string {
	if projectRoot == "" {
		projectRoot = config.GetProjectRoot()
	}
	for _, n := range []string{"venv", ".venv"} {
		if tccPresent(pm.VenvPython(filepath.Join(projectRoot, n))) {
			return filepath.Join(projectRoot, n)
		}
	}
	return ""
}

// Interpreter binary at or inside src, empty when there is none
func tccInterpreterFile(src, minor string) string {
	st, err := os.Stat(src)
	if err == nil && st.Mode().IsRegular() {
		return src
	}
	if err != nil || !st.IsDir() {
		return ""
	}
	var candidates []string
	for _, n := range tccStoreBinNames(minor) {
		candidates = append(candidates, filepath.Join(src, n))
	}
	matches, err := filepath.Glob(filepath.Join(src, "python3.*"))
	if err != nil {
		return ""
	}
	sort.Strings(matches)
	for _, c := range matches {
		if !strings.HasSuffix(c, ".dSYM") && !strings.HasSuffix(c, ".txt") {
			candidates = append(candidates, c)
		}
	}
	for _, c := range candidates {
		if tccIsFile(c) {
			return c
		}
	}
	return ""
}

// Interpreter file the venv resolves to, its symlink target or the pyvenv.cfg home, empty when unknown
func tccInterpreterSource(venvDir, minor string) string {
	venvPy := pm.VenvPython(venvDir)
	if tccIsSymlink(venvPy) {
		if resolved, err := pypath.Realpath(venvPy, pypath.Lax); err == nil {
			return resolved
		}
		return ""
	}
	cfg := filepath.Join(venvDir, "pyvenv.cfg")
	if !tccIsFile(cfg) {
		return ""
	}
	raw, err := os.ReadFile(cfg)
	if err != nil {
		return ""
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return ""
	}
	home := ""
	for _, l := range pystr.Splitlines(text, false) {
		if strings.HasPrefix(strings.ToLower(l), "home") {
			_, after, _ := strings.Cut(l, "=")
			home = pystr.Strip(after)
			break
		}
	}
	if home == "" {
		return ""
	}
	return tccInterpreterFile(home, minor)
}

// Venv dir, venv interpreter and source of a uv managed macOS venv, or the skip reason
func tccManagedVenv(projectRoot string) (venvDir, venvPy, source, skip string) {
	if !tccIsMacOS() {
		return "", "", "", "not macOS"
	}
	venvDir = tccVenvDir(projectRoot)
	if venvDir == "" {
		return "", "", "", "no venv interpreter"
	}
	source = tccInterpreterSource(venvDir, tccPythonMinor(venvDir))
	if source == "" || !tccIsUvMacosStore(source) {
		return "", "", "", "interpreter not uv-managed (stable path)"
	}
	return venvDir, pm.VenvPython(venvDir), source, ""
}

func tccAnchorMarker(venvBin string) string { return filepath.Join(venvBin, tccMarkerName) }

func tccMarkerMatches(venvBin, expected string) bool {
	marker := tccAnchorMarker(venvBin)
	if !tccIsFile(marker) {
		return false
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		return false
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	return err == nil && pystr.Strip(text) == expected
}

// Writes the marker atomically so a concurrent ensure never reads a torn one
func tccWriteMarker(ctx context.Context, venvBin, sourceFile string) error {
	return pyfs.AtomicWriteText(ctx, tccAnchorMarker(venvBin), tccMarkerValue(sourceFile), tccMarkerName+".")
}

// Store root two levels above the resolved store interpreter
func tccStoreRoot(sourceFile string) string {
	resolved, err := pypath.Realpath(sourceFile, pypath.Lax)
	if err != nil {
		resolved = sourceFile
	}
	return filepath.Dir(filepath.Dir(resolved))
}

// Hardlinks, else copies, the store libpython files into the venv lib dir
func tccProvisionLibpython(venvDir, sourceFile string, refresh bool) {
	srcLib := filepath.Join(tccStoreRoot(sourceFile), "lib")
	if st, err := os.Stat(srcLib); err != nil || !st.IsDir() {
		return
	}
	dstLib := filepath.Join(venvDir, "lib")
	if err := os.MkdirAll(dstLib, 0o777); err != nil {
		tccLog.Debug("libpython provision skipped", "err", err)
		return
	}
	matches, err := filepath.Glob(filepath.Join(srcLib, "libpython*"))
	if err != nil {
		tccLog.Debug("libpython provision skipped", "err", err)
		return
	}
	for _, src := range matches {
		if !tccIsFile(src) {
			continue
		}
		dst := filepath.Join(dstLib, filepath.Base(src))
		if _, err := os.Lstat(dst); err == nil {
			if !refresh {
				continue
			}
			if os.Remove(dst) != nil {
				continue
			}
		}
		if os.Link(src, dst) != nil {
			if err := pyshutil.Copy2(src, dst); err != nil {
				tccLog.Debug(fmt.Sprintf("libpython provision failed for %s", src), "err", err)
			}
		}
	}
}

// Unique executable staging copy of source inside the venv bin dir
var tccStageCopy = func(venvBin, prefix, source string) (string, error) {
	f, err := os.CreateTemp(venvBin, prefix+"*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	f.Close()
	if err := tccCopy2(source, tmp); err != nil {
		tccDiscard(tmp)
		return "", err
	}
	st, err := os.Stat(source)
	if err == nil {
		err = os.Chmod(tmp, st.Mode().Perm()|0o111)
	}
	if err != nil {
		tccDiscard(tmp)
		return "", err
	}
	return tmp, nil
}

func tccDiscard(tmp string) {
	if tmp != "" {
		os.Remove(tmp)
	}
}

// Materializes one alias as a real file copy of the anchor, false with a warning on failure
var tccCopyAlias = func(venvBin, name, anchor string) bool {
	tmp, err := tccStageCopy(venvBin, "."+name+".tcc-", anchor)
	if err == nil {
		if err = os.Rename(tmp, filepath.Join(venvBin, name)); err == nil {
			return true
		}
	}
	tccLog.Warn(fmt.Sprintf("TCC anchor alias %s not materialized: %s", name, pyfs.OSErrorText(err)))
	tccDiscard(tmp)
	return false
}

// Copies every uv alias name as a real file of the anchor, true only when each needed one succeeded
func tccMaterializeAliases(venvBin, anchor, minor string, refresh bool) bool {
	names := map[string]bool{}
	for _, n := range tccAliasNames(minor) {
		names[n] = true
	}
	if matches, err := filepath.Glob(filepath.Join(venvBin, "python3*")); err == nil {
		for _, p := range matches {
			if tccAliasPattern.MatchString(filepath.Base(p)) {
				names[filepath.Base(p)] = true
			}
		}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	ok := true
	for _, name := range sorted {
		alias := filepath.Join(venvBin, name)
		_, statErr := os.Stat(alias)
		if refresh || tccIsSymlink(alias) || statErr != nil {
			ok = tccCopyAlias(venvBin, name, anchor) && ok
		}
	}
	return ok
}

// Output and exit code of the boot probe, an exec error when the binary could not start
var tccRunProbe = func(argv []string, env []string, timeout time.Duration) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return "", -1, context.DeadlineExceeded
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return pystr.DecodeUTF8Replace(out), ee.ExitCode(), nil
	}
	if err != nil {
		return "", -1, err
	}
	return pystr.DecodeUTF8Replace(out), 0, nil
}

// Launches the staged interpreter and demands encodings and the venv prefix, an unrunnable binary passing
var tccPassesBootGate = func(staged, venvDir string) bool {
	env := []string{}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "PYTHONHOME", "PYTHONPATH", "PYTHONSTARTUP", "__PYVENV_LAUNCHER__":
			continue
		}
		env = append(env, kv)
	}
	stdout, code, err := tccRunProbe([]string{staged, "-c", "import encodings, sys; print(sys.prefix)"}, env, 30*time.Second)
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOEXEC) || errors.Is(err, exec.ErrNotFound) {
			tccLog.Debug(fmt.Sprintf("boot gate skipped: cannot execute %s (%s)", staged, pyfs.OSErrorText(err)))
			return true
		}
		tccLog.Warn("boot gate: staged copy not executable: " + pyfs.OSErrorText(err))
		return false
	}
	if code != 0 {
		return false
	}
	printed := pystr.Splitlines(pystr.Strip(stdout), false)
	if len(printed) == 0 {
		return false
	}
	last := printed[len(printed)-1]
	got, err1 := pypath.Realpath(last, pypath.Lax)
	want, err2 := pypath.Realpath(venvDir, pypath.Lax)
	if err1 != nil || err2 != nil {
		return strings.Contains(last, venvDir)
	}
	return got == want
}

// Replaces the venv interpreter with a signed copy of the store binary, gated on a real boot
func tccInstallAnchor(ctx context.Context, venvDir, sourceFile string) error {
	venvPy := pm.VenvPython(venvDir)
	venvBin := filepath.Dir(venvPy)
	if err := os.MkdirAll(venvBin, 0o777); err != nil {
		return err
	}
	tccProvisionLibpython(venvDir, sourceFile, true)
	tmp, err := tccStageCopy(venvBin, ".python-tcc-", sourceFile)
	if err != nil {
		return err
	}
	if err := func() error {
		defer func() {
			if r := recover(); r != nil {
				tccLog.Debug("anchor copy signing skipped", "err", fmt.Sprint(r))
			}
		}()
		tccSignManagedPython(tmp)
		return nil
	}(); err != nil {
		return err
	}
	if !tccPassesBootGate(tmp, venvDir) {
		tccDiscard(tmp)
		return &tccBootGateFailed{fmt.Sprintf("staged copy at %s failed encodings/prefix probe", tmp)}
	}
	if err := os.Rename(tmp, venvPy); err != nil {
		tccDiscard(tmp)
		return err
	}
	if tccMaterializeAliases(venvBin, venvPy, tccPythonMinor(venvDir), true) {
		if err := tccWriteMarker(ctx, venvBin, sourceFile); err != nil {
			return err
		}
	} else {
		tccLog.Warn("TCC anchor installed but alias materialization was incomplete; leaving anchor unmarked so the next run retries")
	}
	return nil
}

// Pins a dylib complete interpreter anchor for macOS TCC, the anchored interpreter or empty when not applicable or refused
func ensureMacosTCCAnchor(ctx context.Context, projectRoot string) (string, error) {
	venvDir, venvPy, source, skip := tccManagedVenv(projectRoot)
	if skip != "" {
		return "", nil
	}
	minor := tccPythonMinor(venvDir)
	sourceFile := tccInterpreterFile(source, minor)
	if sourceFile == "" {
		return "", nil
	}
	if !tccIsSymlink(venvPy) && tccMarkerMatches(filepath.Dir(venvPy), tccMarkerValue(sourceFile)) {
		tccProvisionLibpython(venvDir, sourceFile, false)
		if tccPassesBootGate(venvPy, venvDir) {
			tccMaterializeAliases(filepath.Dir(venvPy), venvPy, minor, false)
			return venvPy, nil
		}
	}
	if err := tccInstallAnchor(ctx, venvDir, sourceFile); err != nil {
		var gate *tccBootGateFailed
		if errors.As(err, &gate) {
			tccLog.Warn("macOS TCC anchor boot-gate refused install: " + gate.msg)
		} else {
			tccLog.Warn("macOS TCC anchor install failed: " + pyfs.OSErrorText(err))
		}
		return "", nil
	}
	return venvPy, nil
}

// Anchor state for doctor, skip, active, stale or missing with its detail
var macosTCCAnchorState = func(ctx context.Context, projectRoot string) (string, string, error) {
	venvDir, venvPy, source, skip := tccManagedVenv(projectRoot)
	if skip != "" {
		return "skip", skip, nil
	}
	if tccIsSymlink(venvPy) {
		return "missing", venvPy, nil
	}
	expected := source
	if sourceFile := tccInterpreterFile(source, tccPythonMinor(venvDir)); sourceFile != "" {
		expected = tccMarkerValue(sourceFile)
	}
	if tccMarkerMatches(filepath.Dir(venvPy), expected) {
		return "active", venvPy, nil
	}
	return "stale", venvPy, nil
}
