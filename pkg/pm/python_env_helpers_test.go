package pm

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tarzst"
)

// Recorded hermes tree archive the pm cases read, resolved before any test changes directory
var peArchive, _ = filepath.Abs(filepath.Join("testdata", "hermes.tar.zst"))

// Private copy of the named recorded hermes members, a slash path or a directory prefix ending in a slash
func peReferences(t *testing.T, members ...string) string {
	t.Helper()
	archive, err := os.ReadFile(peArchive)
	if err != nil {
		t.Fatal(err)
	}
	files, err := tarzst.Decode(archive)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, member := range members {
		found := false
		for _, f := range files {
			if f.Path != member && !(strings.HasSuffix(member, "/") && strings.HasPrefix(f.Path, member)) {
				continue
			}
			found = true
			target := filepath.Join(root, filepath.FromSlash(f.Path))
			if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, f.Data, f.Mode); err != nil {
				t.Fatal(err)
			}
		}
		if !found {
			t.Fatalf("recorded hermes tree has no %s", member)
		}
	}
	return root
}

// User cache directory read before any test moves HOME
var peCacheRoot, _ = os.UserCacheDir()

var (
	peUVOnce sync.Once
	peUVPath string
	peUVErr  error
)

// Real uv for environment tests, from PATH or the pinned release cached outside the repository
func peUV(t *testing.T) string {
	t.Helper()
	peUVOnce.Do(func() { peUVPath, peUVErr = peFetchUV() })
	if peUVErr != nil {
		t.Fatalf("environment tests need real uv: %v", peUVErr)
	}
	return peUVPath
}

// Downloads and unpacks the lockfile's uv for this host once into the user cache
func peFetchUV() (string, error) {
	if found, err := exec.LookPath("uv"); err == nil {
		return found, nil
	}
	name := "uv"
	if runtime.GOOS == "windows" {
		name = "uv.exe"
	}
	return peFetchPinned("uv", name)
}

var (
	pePythonOnce sync.Once
	pePythonPath string
	pePythonErr  error
)

// Self contained pinned interpreter for tests that record it as PM's Python entry
func pePinnedPython(t *testing.T) string {
	t.Helper()
	pePythonOnce.Do(func() {
		name := "python3"
		if runtime.GOOS == "windows" {
			name = "python.exe"
		}
		pePythonPath, pePythonErr = peFetchPinned("python", name)
	})
	if pePythonErr != nil {
		t.Fatalf("tests need the pinned Python: %v", pePythonErr)
	}
	return pePythonPath
}

// Downloads and unpacks a package's embedded lock artifact for this host once, returning the named file inside it
func peFetchPinned(pkg, binaryName string) (string, error) {
	target, err := CurrentTarget()
	if err != nil {
		return "", err
	}
	doc, err := loadJSON(embeddedLock)
	if err != nil {
		return "", err
	}
	row, _ := doc.(map[string]any)["packages"].(map[string]any)[pkg].(map[string]any)
	pin := row["artifacts"].(map[string]any)[target]
	if list, ok := pin.([]any); ok && len(list) > 0 {
		pin = list[0]
	}
	artifact, ok := pin.(map[string]any)
	if !ok {
		return "", &fs.PathError{Op: "pin", Path: pkg + " " + target, Err: fs.ErrNotExist}
	}
	url, sha := pyrepr.Str(artifact["url"]), pyrepr.Str(artifact["sha256"])
	if peCacheRoot == "" {
		return "", &fs.PathError{Op: "cache", Path: "user cache directory", Err: fs.ErrNotExist}
	}
	root := filepath.Join(peCacheRoot, "icebath-pm-tests", pkg+"-"+sha[:16])
	find := func() string {
		found := ""
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && d.Name() == binaryName && found == "" {
				found = path
			}
			return nil
		})
		return found
	}
	if binary := find(); binary != "" {
		return binary, nil
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != sha {
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
	if err := Extract(archive, filepath.Join(staging, "tree")); err != nil {
		return "", err
	}
	os.Remove(archive)
	if err := os.Rename(staging, root); err != nil {
		return "", err
	}
	binary := find()
	if binary == "" {
		return "", &fs.PathError{Op: "find", Path: root, Err: fs.ErrNotExist}
	}
	return binary, nil
}

// Interpreter the tests build environments with, the host Python standing in for sys executable
func peHostPython(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"python3", "python"} {
		if found, err := exec.LookPath(name); err == nil {
			return found
		}
	}
	t.Fatal("environment tests need a host Python")
	return ""
}

// Replaces the pinned toolchain with real uv and the host interpreter for one test
func peToolchain(t *testing.T, uv, python string) {
	t.Helper()
	original := uvToolchain
	uvToolchain = func(context.Context, bool, bool) (string, string, bool, error) { return uv, python, true, nil }
	t.Cleanup(func() { uvToolchain = original })
}

// Builds a wheel whose bytes depend only on its arguments
func peWheel(t *testing.T, dir, name, version string, requirements []string, extra map[string]string) string {
	t.Helper()
	if version == "" {
		version = "1.0"
	}
	metadata := name + "-" + version + ".dist-info"
	var paths []string
	entries := map[string]string{}
	add := func(path, body string) {
		paths = append(paths, path)
		entries[path] = body
	}
	add(name+"/__init__.py", "__version__ = "+pyrepr.StrRepr(version)+"\n")
	meta := "Metadata-Version: 2.1\nName: " + name + "\nVersion: " + version + "\n"
	for _, r := range requirements {
		meta += "Requires-Dist: " + r + "\n"
	}
	add(metadata+"/METADATA", meta)
	add(metadata+"/WHEEL", "Wheel-Version: 1.0\nRoot-Is-Purelib: true\nTag: py3-none-any\n")
	for _, k := range sortedKeys(extra) {
		add(k, extra[k])
	}
	var record strings.Builder
	for _, p := range paths {
		record.WriteString(p + ",,\n")
	}
	add(metadata+"/RECORD", record.String())
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range paths {
		header := &zip.FileHeader{Name: p, Method: zip.Deflate, Modified: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)}
		header.SetMode(0o644)
		w, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, entries[p])
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	wheel := filepath.Join(dir, name+"-"+version+"-py3-none-any.whl")
	if err := os.WriteFile(wheel, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return wheel
}

// Runs a command that must succeed and returns its stripped stdout
func peRun(t *testing.T, argv []string, cwd string, env map[string]string) string {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = EnvList(env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v failed: %v\n%s", argv, err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// Process environment without interpreter, project and uv selection, homes moved under a test directory
func peIsolatedEnv(t *testing.T, root string) map[string]string {
	t.Helper()
	env := map[string]string{}
	for k, v := range pyEnviron() {
		if !strings.HasPrefix(k, "UV_") && !strings.HasPrefix(k, "PYTHON") && k != "VIRTUAL_ENV" {
			env[k] = v
		}
	}
	h := filepath.Join(root, "isolated-home")
	if err := os.MkdirAll(h, 0o777); err != nil {
		t.Fatal(err)
	}
	env["HOME"], env["USERPROFILE"], env["ICEBATH_HOME"] = h, h, filepath.Join(h, ".icebath")
	return env
}

// Writes a text file, creating its directory
func peWrite(t *testing.T, path, text string) {
	t.Helper()
	writeFile(t, path, []byte(text), 0o666)
}

// Reads a text file
func peRead(t *testing.T, path string) string {
	t.Helper()
	return string(readFile(t, path))
}

// Walks a tree calling visit for every entry with whether it is a regular file, stopping when visit answers false
func filepathWalkStop(root string, visit func(path string, regular bool) bool) {
	stop := false
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if stop || err != nil {
			return nil
		}
		if !d.IsDir() && !visit(path, d.Type().IsRegular()) {
			stop = true
			return filepath.SkipAll
		}
		return nil
	})
}

// Reports whether this process can open a file for reading
func canRead(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	f.Close()
	return true
}
