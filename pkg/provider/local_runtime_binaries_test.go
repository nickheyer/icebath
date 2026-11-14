package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
)

func TestAutoBackendUsesOnlyCompatiblePins(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := []struct {
		target   string
		vendor   *string
		expected string
	}{
		{"linux-x64", str("nvidia"), "cuda"},
		{"linux-arm64", str("nvidia"), "cuda"},
		{"win32-arm64", str("nvidia"), "cuda"},
		{"win32-arm64", str("AMD Radeon"), "cpu"},
		{"win32-x64", str("AMD Radeon"), "vulkan"},
		{"win32-x64", nil, "cpu"},
		{"darwin-arm64", nil, "metal"},
	}
	for _, c := range cases {
		got, err := ResolveBackend("auto", c.vendor, c.target)
		if err != nil || got != c.expected {
			t.Errorf("ResolveBackend(auto, %v, %s) = %q, %v, want %q", c.vendor, c.target, got, err, c.expected)
		}
		var resolution *BinaryResolutionError
		if _, err := ResolveBackend("cuda", nil, "darwin-arm64"); !errors.As(err, &resolution) {
			t.Errorf("cuda on darwin-arm64 err = %v, want BinaryResolutionError", err)
		}
	}
}

func TestLinuxCudaRuntimeLibrariesLandBesideTheServer(t *testing.T) {
	p, err := pm.GetPackage("llamacpp-cuda")
	if err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(t.TempDir(), "tree")
	cudart := filepath.Join(staged, "cudart-llama-b1-bin-ubuntu-cuda-13.4-x64")
	gomp := filepath.Join(staged, "usr", "lib", "x86_64-linux-gnu")
	for _, dir := range []string{filepath.Join(staged, "llama-b1"), cudart, gomp} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writes := map[string]string{filepath.Join(staged, "llama-b1", "llama-server"): "engine",
		filepath.Join(cudart, "libcudart.so.13"): "rt", filepath.Join(gomp, "libgomp.so.1.0.0"): "omp"}
	for path, body := range writes {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("libgomp.so.1.0.0", filepath.Join(gomp, "libgomp.so.1")); err != nil {
		t.Fatal(err)
	}
	if err := p.Stage(nil, staged, "1", "linux-x64"); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(p.Binary(staged, "linux-x64")); err != nil || !st.Mode().IsRegular() {
		t.Fatalf("engine binary missing after stage: %v", err)
	}
	if st, err := os.Stat(filepath.Join(staged, "libcudart.so.13")); err != nil || !st.Mode().IsRegular() {
		t.Fatalf("cudart not beside the server: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(staged, "cudart-*")); len(left) != 0 {
		t.Fatalf("cudart dir left behind: %v", left)
	}
	if st, err := os.Lstat(filepath.Join(staged, "libgomp.so.1")); err != nil || st.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("libgomp.so.1 missing or still a symlink: %v", err)
	}
	if body, _ := os.ReadFile(filepath.Join(staged, "libgomp.so.1")); string(body) != "omp" {
		t.Fatalf("libgomp.so.1 = %q", body)
	}
	if _, err := os.Stat(filepath.Join(staged, "usr")); !os.IsNotExist(err) {
		t.Fatal("usr tree left behind")
	}
}

func TestMissingPinIsRefusedInsteadOfConstructingDownloadURL(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "lock.json")
	previous := llamaLockfilePath
	llamaLockfilePath = func() string { return lockPath }
	t.Cleanup(func() { llamaLockfilePath = previous })
	lock := pm.NewLockfile(lockPath)
	lock.SetPin("llamacpp-cpu", "123", map[string]any{})
	if err := lock.Save(); err != nil {
		t.Fatal(err)
	}
	if tag, err := PinnedTag("cpu"); err != nil || tag != "b123" {
		t.Fatalf("PinnedTag = %q, %v", tag, err)
	}
	target, err := pm.CurrentTarget()
	if err != nil {
		t.Fatal(err)
	}
	_, err = ResolveBackend("cpu", nil, target)
	var resolution *BinaryResolutionError
	if !errors.As(err, &resolution) || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("ResolveBackend err = %v", err)
	}
}

func TestBackendSelection(t *testing.T) {
	cases := []struct{ vendor, osName, expected string }{
		{"NVIDIA GeForce RTX 5090", "win", "cuda"},
		{"nvidia", "ubuntu", "cuda"},
		{"AMD Radeon RX 7900", "win", "vulkan"},
		{"intel", "win", "vulkan"},
		{"", "win", "cpu"},
		{"", "ubuntu", "cpu"},
		{"nvidia", "macos", "metal"},
		{"", "macos", "metal"},
	}
	for _, c := range cases {
		if got, err := SelectBackend(c.vendor, c.osName); err != nil || got != c.expected {
			t.Errorf("SelectBackend(%q, %q) = %q, %v, want %q", c.vendor, c.osName, got, err, c.expected)
		}
	}
}

func TestRuntimesRootResolvesToTheSharedRootFromANamedProfile(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".icebath")
	profile := filepath.Join(root, "profiles", "coder")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, profile)
	runtimes := RuntimesRoot()
	if runtimes != filepath.Join(root, "runtimes", "llamacpp") {
		t.Fatalf("runtimes dir leaked into the profile: %s", runtimes)
	}
	if strings.Contains(runtimes, string(filepath.Separator)+"profiles"+string(filepath.Separator)) {
		t.Fatalf("runtimes path carries a profiles part: %s", runtimes)
	}
}

func TestRuntimesRootDefaultProfilePathsUnchanged(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, root)
	if got := RuntimesRoot(); got != filepath.Join(root, "runtimes", "llamacpp") {
		t.Fatalf("RuntimesRoot = %s", got)
	}
}

type lr3LegacyEnv struct {
	home, store, target, backend, version, older string
	pkg                                          pm.Package
	pinned                                       []string
}

// Runs fn once per backend with a disposable home and store against the shipped PM pin of that backend
func lr3ForLegacyEnv(t *testing.T, fn func(t *testing.T, env lr3LegacyEnv)) {
	for _, backend := range []string{"cpu", "cuda"} {
		t.Run(backend, func(t *testing.T) {
			name := BackendPackages[backend]
			target, err := pm.CurrentTarget()
			if err != nil {
				t.Fatal(err)
			}
			p, err := pm.GetPackage(name)
			if err != nil {
				t.Fatal(err)
			}
			if reason := p.MissingReason(target); reason != "" {
				t.Skipf("%s has no build for %s", name, target)
			}
			lock := pm.NewLockfile(pm.LockfilePath())
			version, _ := lock.Version(name)
			number, err := strconv.Atoi(version)
			if err != nil {
				t.Fatalf("pinned version %q of %s", version, name)
			}
			var pinned []string
			for _, artifact := range lock.Artifacts(name, target) {
				sha, _ := artifact["sha256"].(string)
				pinned = append(pinned, sha)
			}
			root := filepath.Join(t.TempDir(), "home")
			store := filepath.Join(root, "tools")
			t.Setenv(home.EnvHome, root)
			t.Setenv(pm.EnvRuntimeDir, store)
			previousVerify := llamaVerify
			llamaVerify = func(pm.Package, string, string) string { return "" }
			legacyLeftInPlaceM.Lock()
			legacyLeftInPlace = map[string]bool{}
			legacyLeftInPlaceM.Unlock()
			t.Cleanup(func() { llamaVerify = previousVerify })
			fn(t, lr3LegacyEnv{home: root, store: store, target: target, backend: backend, version: version,
				older: strconv.Itoa(number - 285), pkg: p, pinned: pinned})
		})
	}
}

// The digests a pre PM manifest records, the engine release assets and never PM's extra archives
func (e lr3LegacyEnv) legacyDigests(t *testing.T) []string {
	names, err := llamaAssetNames(e.pkg, e.version, e.target)
	if err != nil {
		t.Fatal(err)
	}
	return append([]string(nil), e.pinned[:len(names)]...)
}

func (e lr3LegacyEnv) install(t *testing.T, version string, digests []string, verified bool) string {
	t.Helper()
	tag := "b" + version
	dir := filepath.Join(e.home, "runtimes", "llamacpp", tag, e.backend)
	binary := e.pkg.Binary(dir, e.target)
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	lr3Must(t, os.WriteFile(binary, []byte("engine "+tag), 0o644))
	lr3Must(t, os.WriteFile(filepath.Join(dir, "ggml-base.dll"), []byte("dll"), 0o644))
	names, err := llamaAssetNames(e.pkg, version, e.target)
	if err != nil {
		t.Fatal(err)
	}
	assets := map[string]any{}
	for i, name := range names {
		if i < len(digests) {
			assets[name] = digests[i]
		}
	}
	manifest := map[string]any{"tag": tag, "backend": e.backend, "assets": assets}
	if verified {
		manifest["verified_version"] = fmt.Sprintf("version: 0.4.1-dev (build %s, commit 0)", version)
	}
	raw, _ := json.Marshal(manifest)
	lr3Must(t, os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o644))
	return dir
}

func lr3Must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (e lr3LegacyEnv) engine(t *testing.T, allowOutdated bool) *Engine {
	t.Helper()
	engine, err := InstalledEngine(context.Background(), e.backend, allowOutdated)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func (e lr3LegacyEnv) fact(t *testing.T) (map[string]any, bool) {
	facts, err := pm.NewFacts(filepath.Join(e.store, "facts.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	return facts.Get(e.pkg.Base().Name)
}

func lr3ExistsPath(path string) bool { _, err := os.Lstat(path); return err == nil }

func TestMatchingLegacyEngineMovesIntoTheStoreAsCurrent(t *testing.T) {
	lr3ForLegacyEnv(t, func(t *testing.T, e lr3LegacyEnv) {
		legacy := e.legacyDigests(t)
		install := e.install(t, e.version, legacy, true)
		engine := e.engine(t, true)
		entry := filepath.Join(e.store, e.pkg.StoreEntry(e.version, e.target))
		want := Engine{Backend: e.backend, Tag: "b" + e.version, Binary: e.pkg.Binary(entry, e.target)}
		if engine == nil || *engine != want {
			t.Fatalf("engine = %+v, want %+v", engine, want)
		}
		if body, _ := os.ReadFile(engine.Binary); string(body) != "engine b"+e.version {
			t.Fatalf("binary = %q", body)
		}
		if lr3ExistsPath(install) || lr3ExistsPath(filepath.Dir(install)) {
			t.Fatal("legacy install or its tag dir left behind")
		}
		fact, ok := e.fact(t)
		if !ok || fact["version"] != e.version || !reflect.DeepEqual(lr3Strings(fact["artifacts"]), legacy) {
			t.Fatalf("fact = %v", fact)
		}
		current := e.engine(t, false)
		if (current != nil && *current == *engine) != reflect.DeepEqual(legacy, e.pinned) {
			t.Fatalf("current engine %+v with legacy %v pinned %v", current, legacy, e.pinned)
		}
	})
}

func lr3Strings(v any) []string {
	var out []string
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		for _, item := range t {
			s, _ := item.(string)
			out = append(out, s)
		}
	}
	return out
}

func TestOlderLegacyEngineCountsAsInstalledButOutdated(t *testing.T) {
	lr3ForLegacyEnv(t, func(t *testing.T, e lr3LegacyEnv) {
		e.install(t, e.older, append([]string{strings.Repeat("d", 64)}, e.legacyDigests(t)[1:]...), true)
		if engine := e.engine(t, true); engine == nil || engine.Tag != "b"+e.older {
			t.Fatalf("engine = %+v", engine)
		}
		if found, err := pm.Installed(context.Background(), e.pkg.Base().Name, false); err != nil || found != nil {
			t.Fatalf("pm current install = %+v, %v", found, err)
		}
		if engine := e.engine(t, false); engine != nil {
			t.Fatalf("outdated engine counted as current: %+v", engine)
		}
	})
}

func TestNewestLegacyEngineWinsAndOlderStays(t *testing.T) {
	lr3ForLegacyEnv(t, func(t *testing.T, e lr3LegacyEnv) {
		old := e.install(t, e.older, append([]string{strings.Repeat("d", 64)}, e.legacyDigests(t)[1:]...), true)
		e.install(t, e.version, e.legacyDigests(t), true)
		if engine := e.engine(t, true); engine == nil || engine.Tag != "b"+e.version {
			t.Fatalf("engine = %+v", engine)
		}
		if st, err := os.Stat(old); err != nil || !st.IsDir() {
			t.Fatal("older legacy install was moved")
		}
	})
}

func TestUnverifiedOrDamagedLegacyEngineIsLeftAlone(t *testing.T) {
	for _, damage := range []string{"unverified", "missing_digest", "wrong_backend", "tag_mismatch"} {
		t.Run(damage, func(t *testing.T) {
			lr3ForLegacyEnv(t, func(t *testing.T, e lr3LegacyEnv) {
				legacy := e.legacyDigests(t)
				if damage == "missing_digest" {
					legacy = append([]string{"not-a-digest"}, legacy[1:]...)
				}
				install := e.install(t, e.version, legacy, damage != "unverified")
				path := filepath.Join(install, "manifest.json")
				raw, _ := os.ReadFile(path)
				var manifest map[string]any
				lr3Must(t, json.Unmarshal(raw, &manifest))
				if damage == "wrong_backend" {
					manifest["backend"] = "vulkan"
				}
				if damage == "tag_mismatch" {
					manifest["tag"] = "b" + e.older
				}
				raw, _ = json.Marshal(manifest)
				lr3Must(t, os.WriteFile(path, raw, 0o644))
				if engine := e.engine(t, true); engine != nil {
					t.Fatalf("damaged install adopted: %+v", engine)
				}
				if st, err := os.Stat(install); err != nil || !st.IsDir() {
					t.Fatal("damaged install moved")
				}
				if fact, ok := e.fact(t); ok {
					t.Fatalf("fact recorded: %v", fact)
				}
			})
		})
	}
}

func TestEngineThatFailsVerificationStaysAndIsNotRetried(t *testing.T) {
	lr3ForLegacyEnv(t, func(t *testing.T, e lr3LegacyEnv) {
		install := e.install(t, e.version, e.legacyDigests(t), true)
		var probes []string
		llamaVerify = func(_ pm.Package, entry, _ string) string {
			probes = append(probes, entry)
			return "llama-server --version exited 1"
		}
		if e.engine(t, true) != nil || e.engine(t, true) != nil {
			t.Fatal("engine that failed verification was adopted")
		}
		if st, err := os.Stat(install); err != nil || !st.IsDir() || !reflect.DeepEqual(probes, []string{install}) {
			t.Fatalf("install kept %v, probes %v", err, probes)
		}
	})
}

func TestExistingPMEngineIsNeverReplaced(t *testing.T) {
	lr3ForLegacyEnv(t, func(t *testing.T, e lr3LegacyEnv) {
		entryName := e.pkg.StoreEntry(e.version, e.target)
		entry := filepath.Join(e.store, entryName)
		binary := e.pkg.Binary(entry, e.target)
		lr3Must(t, os.MkdirAll(filepath.Dir(binary), 0o755))
		lr3Must(t, os.WriteFile(binary, []byte("pm engine"), 0o644))
		digest, err := pm.TreeDigest(entry)
		lr3Must(t, err)
		facts, err := pm.NewFacts(filepath.Join(e.store, "facts.json"), false)
		lr3Must(t, err)
		target := e.target
		lr3Must(t, facts.Record(e.pkg.Base().Name, e.version, entryName, pm.EnvDiff{}, e.store,
			pm.FactRecord{Target: &target, Artifacts: e.pinned, Digest: &digest}))
		install := e.install(t, e.version, e.legacyDigests(t), true)
		engine := e.engine(t, true)
		if engine == nil {
			t.Fatal("no engine")
		}
		if body, _ := os.ReadFile(engine.Binary); string(body) != "pm engine" {
			t.Fatalf("binary = %q", body)
		}
		if st, err := os.Stat(install); err != nil || !st.IsDir() {
			t.Fatal("legacy install moved over an existing PM engine")
		}
	})
}

func TestBusyStoreLeavesTheEngineForALaterCall(t *testing.T) {
	lr3ForLegacyEnv(t, func(t *testing.T, e lr3LegacyEnv) {
		install := e.install(t, e.version, e.legacyDigests(t), true)
		lr3Must(t, os.MkdirAll(e.store, 0o755))
		held, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
		go func() {
			defer close(done)
			f, err := os.OpenFile(filepath.Join(e.store, ".install.lock"), os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				return
			}
			defer f.Close()
			if ok, err := pm.LockFD(f, true, pm.WaitForever); ok && err == nil {
				close(held)
				select {
				case <-release:
				case <-time.After(30 * time.Second):
				}
			}
		}()
		select {
		case <-held:
		case <-time.After(10 * time.Second):
			t.Fatal("holder never took the store lock")
		}
		engine := e.engine(t, true)
		_, statErr := os.Stat(install)
		close(release)
		<-done
		if engine != nil || statErr != nil {
			t.Fatalf("busy store adopted the engine %+v or moved the install %v", engine, statErr)
		}
		if engine := e.engine(t, true); engine == nil || engine.Tag != "b"+e.version {
			t.Fatalf("later call engine = %+v", engine)
		}
	})
}
