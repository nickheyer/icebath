package pyhost

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Names the interpreter a matrix child process starts instead of the pinned one
const matrixEnv = "ICEBATH_PYHOST_TEST_PYTHON"

// Interpreter versions the matrix runs beside the pinned one, the PyConfig layout path, free threading and the newest
var matrixVersions = []string{"3.11", "3.13", "3.13t", "3.15"}

var (
	testHost *Host
	testErr  error
)

// Provisions interpreters before the network is blocked, starts one on the main thread, runs the suite and finalizes
func TestMain(m *testing.M) {
	exe := os.Getenv(matrixEnv)
	if exe == "" {
		exe, testErr = pinnedPython()
		if testErr == nil {
			testErr = provisionMatrix()
		}
	}
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	if testErr == nil {
		var in *Interpreter
		if in, testErr = Discover(exe); testErr == nil {
			testHost, testErr = Start(Options{Interpreter: in, ExtraPaths: []string{filepath.Join(os.TempDir(), "pyhost-extra")}})
		}
	}
	code := m.Run()
	if testHost != nil {
		rc, err := testHost.Finalize(20 * time.Second)
		if err != nil || rc != 0 {
			fmt.Fprintf(os.Stderr, "Py_FinalizeEx returned %d: %v\n", rc, err)
			code = 1
		}
	}
	os.Exit(code)
}

// Started host of the test process, failing with the locator message when no interpreter could be started
func needHost(t *testing.T) *Host {
	t.Helper()
	if testErr != nil {
		t.Fatalf("no embeddable interpreter: %v", testErr)
	}
	return testHost
}

// Shared cache the pm suite unpacks pinned tools into, outside the home the gate hides
func testCacheRoot() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "icebath-pm-tests"), nil
}

// Interpreter the product hosts plugins with, the lockfile's pinned Python for this host
func pinnedPython() (string, error) {
	name := "python3"
	if runtime.GOOS == "windows" {
		name = "python.exe"
	}
	return fetchPinned("python", name)
}

// Downloads, verifies and unpacks a pinned lockfile artifact once into the shared cache, returning the named file
func fetchPinned(pkg, binaryName string) (string, error) {
	target, err := pm.CurrentTarget()
	if err != nil {
		return "", err
	}
	artifacts := pm.NewLockfile(pm.LockfilePath()).Artifacts(pkg, target)
	cache, err := testCacheRoot()
	if err != nil {
		return "", err
	}
	if len(artifacts) == 0 {
		return "", &fs.PathError{Op: "pin", Path: pkg + " " + target, Err: fs.ErrNotExist}
	}
	url, sha := pyrepr.Str(artifacts[0]["url"]), pyrepr.Str(artifacts[0]["sha256"])
	root := filepath.Join(cache, pkg+"-"+sha[:16])
	if binary := findFile(root, binaryName); binary != "" {
		return binary, nil
	}
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Get(url)
	if err != nil {
		return "", fmt.Errorf("%s is not unpacked under %s and the download failed: %w", pkg, root, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != sha {
		return "", &fs.PathError{Op: "sha256", Path: url, Err: fs.ErrInvalid}
	}
	staging := root + ".partial"
	os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o777); err != nil {
		return "", err
	}
	archive := filepath.Join(staging, filepath.Base(url))
	if err := os.WriteFile(archive, data, 0o666); err != nil {
		return "", err
	}
	if err := pm.Extract(archive, filepath.Join(staging, "tree")); err != nil {
		return "", err
	}
	os.Remove(archive)
	if err := os.Rename(staging, root); err != nil {
		return "", err
	}
	if binary := findFile(root, binaryName); binary != "" {
		return binary, nil
	}
	return "", &fs.PathError{Op: "find", Path: filepath.Join(root, binaryName), Err: fs.ErrNotExist}
}

// First regular file or link named name under root in lexical walk order
func findFile(root, name string) string {
	found := ""
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == name && found == "" {
			found = path
		}
		return nil
	})
	return found
}

// Install directory of the matrix interpreters, a uv managed Python directory in the shared cache
func matrixDir() (string, error) {
	cache, err := testCacheRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "pyhost-python"), nil
}

// Installs every matrix version missing from the matrix directory with the pinned uv
func provisionMatrix() error {
	dir, err := matrixDir()
	if err != nil {
		return err
	}
	var missing []string
	for _, v := range matrixVersions {
		if _, err := matrixPython(dir, v); err != nil {
			missing = append(missing, v)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	name := "uv"
	if runtime.GOOS == "windows" {
		name = "uv.exe"
	}
	uv, err := fetchPinned("uv", name)
	if err != nil {
		return err
	}
	cmd := exec.Command(uv, append([]string{"python", "install"}, missing...)...)
	cmd.Env = append(os.Environ(), "UV_PYTHON_INSTALL_DIR="+dir, "UV_CACHE_DIR="+filepath.Join(dir, ".uv-cache"))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("UV_PYTHON_INSTALL_DIR=%s %s python install %s failed: %v\n%s", dir, uv, strings.Join(missing, " "), err, out)
	}
	return nil
}

// Executable of one matrix version under the uv install directory, 3.13t naming the free threaded build
func matrixPython(dir, version string) (string, error) {
	minor, threaded := strings.CutSuffix(version, "t")
	pattern := "cpython-" + minor + ".*-" + runtime.GOOS + "-*"
	if threaded {
		pattern = "cpython-" + minor + ".*+freethreaded-" + runtime.GOOS + "-*"
	}
	matches, _ := filepath.Glob(filepath.Join(dir, pattern))
	sort.Strings(matches)
	for i := len(matches) - 1; i >= 0; i-- {
		if !threaded && strings.Contains(filepath.Base(matches[i]), "+freethreaded") {
			continue
		}
		exe := filepath.Join(matches[i], "bin", "python"+version)
		if runtime.GOOS == "windows" {
			exe = filepath.Join(matches[i], "python.exe")
			if threaded {
				exe = filepath.Join(matches[i], "python"+version+".exe")
			}
		}
		if isFile(exe) {
			return exe, nil
		}
	}
	return "", fmt.Errorf("no Python %s under %s, install it with `UV_PYTHON_INSTALL_DIR=%s uv python install %s`", version, dir, dir, version)
}
