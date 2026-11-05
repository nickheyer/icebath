package pm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Stand in binary tool whose bin dir lands on PATH, the pm core FakeTool
type ieFakeTool struct {
	BinaryPackage
	stages     *atomic.Int32
	unpackHook func(archive, staged, target string) error
	verifyHook func(entry, target string) string
}

// Unpacks through the default extractor, then the test's fault hook
func (f *ieFakeTool) Unpack(archive, staged, target string) error {
	if err := f.BinaryPackage.Unpack(archive, staged, target); err != nil {
		return err
	}
	if f.unpackHook != nil {
		return f.unpackHook(archive, staged, target)
	}
	return nil
}

// Verifies through the test's fault hook first, then the binary package
func (f *ieFakeTool) Verify(entry, target string) string {
	if f.verifyHook != nil {
		if reason := f.verifyHook(entry, target); reason != "" {
			return reason
		}
	}
	return f.BinaryPackage.Verify(entry, target)
}

// Counts stages the way the single flight test watches them
func (f *ieFakeTool) Stage(store *Store, staged, version, target string) error {
	if f.stages != nil {
		f.stages.Add(1)
	}
	return f.BinaryPackage.Stage(store, staged, version, target)
}

// Dependency tool that also exports a variable, the pm core DepTool
type ieDepTool struct{ BinaryPackage }

// Adds the marker variable to the binary dir PATH diff
func (d *ieDepTool) Env(entry, target string) EnvDiff {
	diff := d.BinaryPackage.Env(entry, target)
	diff["DEPTOOL_SEEN"] = "1"
	return diff
}

// Binary package named name whose binary is bin/<rel>
func ieBinary(name, rel string, deps ...string) BinaryPackage {
	return BinaryPackage{BasePackage: BasePackage{Name: name, Deps: deps}, NoProbe: true,
		BinaryRel: map[string]string{"win32": rel, "posix": rel}}
}

// Loopback store, lockfile and registry of the pm core fixture
type ieEnv struct {
	tmp      string
	lockPath string
	runtime  string
	docroot  string
	baseURL  string
	fake     *ieFakeTool
	digest   string
}

// Builds the pm core fixture, faketool 1.0 pinned and served from a loopback docroot
func newIEEnv(t *testing.T) *ieEnv {
	t.Helper()
	tmp := isolateHome(t)
	e := &ieEnv{tmp: tmp, runtime: filepath.Join(tmp, "runtime"), docroot: filepath.Join(tmp, "www")}
	t.Setenv(EnvRuntimeDir, e.runtime)
	t.Setenv(EnvDisableLazyInstalls, "")
	os.Unsetenv(EnvDisableLazyInstalls)
	if err := os.MkdirAll(e.docroot, 0o777); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.FileServer(http.Dir(e.docroot)))
	t.Cleanup(server.Close)
	e.baseURL = server.URL
	e.fake = &ieFakeTool{BinaryPackage: ieBinary("faketool", "bin/faketool"), stages: &atomic.Int32{}}
	multi := ieBinary("multitool", "bin/multitool")
	multi.NoFlatten = true
	top := ieBinary("toptool", "bin/faketool", "deptool")
	ieSwapRegistry(t, e.fake, &ieDepTool{BinaryPackage: ieBinary("deptool", "bin/faketool")}, &ieFakeTool{BinaryPackage: top}, &multi, NewVenv(""))
	e.lockPath = filepath.Join(tmp, "repo-lock", "lock.json")
	if err := os.MkdirAll(filepath.Dir(e.lockPath), 0o777); err != nil {
		t.Fatal(err)
	}
	ieEmptyLock(t, e.lockPath)
	previous := lockfilePath
	lockfilePath = func() string { return e.lockPath }
	t.Cleanup(func() { lockfilePath = previous })
	e.digest = e.tar(t, "faketool-1.0.tar.gz", map[string]string{"bin/faketool": "#!x"})
	e.pin(t, "faketool", "1.0", e.digest)
	return e
}

// Writes an empty lockfile so a swapped lockfile path never answers the embedded pins
func ieEmptyLock(t *testing.T, path string) {
	t.Helper()
	writeFile(t, path, []byte(`{"schema": 1, "packages": {}}`), 0o644)
}

// Replaces the registry with exactly these packages for the test
func ieSwapRegistry(t *testing.T, list ...Package) {
	t.Helper()
	registryMu.Lock()
	saved := packages
	packages = map[string]Package{}
	registryMu.Unlock()
	for _, p := range list {
		if err := Register(p); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		registryMu.Lock()
		packages = saved
		registryMu.Unlock()
	})
}

// Writes a gzipped tar of 0755 files into the docroot and returns its sha256
func (e *ieEnv) tar(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, rel := range sortedKeys(files) {
		data := []byte(files[rel])
		if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(data)
	}
	tw.Close()
	gz.Close()
	writeFile(t, filepath.Join(e.docroot, name), buf.Bytes(), 0o644)
	return hexSHA256(buf.Bytes())
}

// Pins name at version to its docroot archive of the same name
func (e *ieEnv) pin(t *testing.T, name, version, digest string) {
	t.Helper()
	lock := NewLockfile(e.lockPath)
	lock.SetPin(name, version, map[string]any{"any": map[string]any{"url": e.baseURL + "/" + name + "-" + version + ".tar.gz", "sha256": digest}})
	if err := lock.Save(); err != nil {
		t.Fatal(err)
	}
}

// Engine ensure with an empty base env
func (e *ieEnv) ensure(t *testing.T, name string, opts EnsureOptions) (*Runner, error) {
	t.Helper()
	if opts.BaseEnv == nil {
		opts.BaseEnv = map[string]string{}
	}
	return ensure(context.Background(), name, opts)
}

// Store facts of the fixture's runtime store
func (e *ieEnv) facts(t *testing.T) *Facts {
	t.Helper()
	facts, err := NewFacts(filepath.Join(e.runtime, "facts.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

// Recorded entry name of an installed package
func (e *ieEnv) entry(t *testing.T, name string) string {
	t.Helper()
	fact, ok := e.facts(t).Get(name)
	if !ok {
		t.Fatalf("%s has no fact", name)
	}
	return pyStr(fact["entry"])
}

// Reports whether the engine considers a package installed
func ieInstalled(t *testing.T, name string) bool {
	t.Helper()
	ok, err := IsInstalled(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// Fails unless err is an install error whose text contains want
func ieWantInstallError(t *testing.T, err error, want string) *InstallError {
	t.Helper()
	var ie *InstallError
	if !errors.As(err, &ie) {
		t.Fatalf("got %v (%T), want an install error containing %q", err, err, want)
	}
	if !strings.Contains(ie.Error(), want) {
		t.Fatalf("install error %q lacks %q", ie.Error(), want)
	}
	return ie
}

// Names in a directory starting with prefix
func ieGlob(t *testing.T, dir, prefix string) []string {
	t.Helper()
	items, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var out []string
	for _, item := range items {
		if strings.HasPrefix(item.Name(), prefix) {
			out = append(out, item.Name())
		}
	}
	return out
}
