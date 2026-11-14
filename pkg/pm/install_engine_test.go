package pm

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBadHashRejected(t *testing.T) {
	e := newIEEnv(t)
	e.pin(t, "faketool", "1.0", strings.Repeat("0", 64))
	_, err := e.ensure(t, "faketool", EnsureOptions{})
	ieWantInstallError(t, err, "install failed")
}

func TestDepsComposeDependentsWin(t *testing.T) {
	e := newIEEnv(t)
	e.pin(t, "deptool", "1.0", e.tar(t, "deptool-1.0.tar.gz", map[string]string{"bin/faketool": "y"}))
	e.pin(t, "toptool", "1.0", e.tar(t, "toptool-1.0.tar.gz", map[string]string{"bin/faketool": "z"}))
	runner, err := e.ensure(t, "toptool", EnsureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if runner.Env["DEPTOOL_SEEN"] != "1" {
		t.Fatalf("env %v", runner.Env)
	}
	path := runner.Env["PATH"]
	top, dep := strings.Index(path, "toptool-1.0"), strings.Index(path, "deptool-1.0")
	if top < 0 || dep < 0 || top >= dep {
		t.Fatalf("dependent does not win PATH: %s", path)
	}
}

func TestStandaloneWarmEnsureDoesNotWaitForUnrelatedWriter(t *testing.T) {
	e := newIEEnv(t)
	if _, err := ensure(context.Background(), "faketool", EnsureOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	release, err := NewStore(e.runtime).InstallLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	done := make(chan *Runner, 1)
	go func() {
		runner, err := e.ensure(t, "faketool", EnsureOptions{Explicit: true})
		if err != nil {
			t.Error(err)
		}
		done <- runner
	}()
	select {
	case runner := <-done:
		if runner == nil || !strings.Contains(runner.Env["PATH"], "faketool-1.0") {
			t.Fatalf("runner %v", runner)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("warm ensure waited for an unrelated writer")
	}
}

func TestVersionBumpSelectsTheNewTool(t *testing.T) {
	e := newIEEnv(t)
	if _, err := e.ensure(t, "faketool", EnsureOptions{}); err != nil {
		t.Fatal(err)
	}
	e.pin(t, "faketool", "2.0", e.tar(t, "faketool-2.0.tar.gz", map[string]string{"bin/faketool": "#!2"}))
	runner, err := e.ensure(t, "faketool", EnsureOptions{})
	if err != nil || !strings.Contains(runner.Env["PATH"], "faketool-2.0") {
		t.Fatalf("runner %v err %v", runner, err)
	}
}

func TestLazyInstallsDisabled(t *testing.T) {
	newIEEnv(t)
	_, err := ensure(withLazyInstallsDisabled(context.Background()), "faketool", EnsureOptions{BaseEnv: map[string]string{}})
	ieWantInstallError(t, err, "lazy installs are disabled")
	t.Setenv(EnvDisableLazyInstalls, "yes")
	_, err = ensure(context.Background(), "faketool", EnsureOptions{BaseEnv: map[string]string{}})
	ieWantInstallError(t, err, "lazy installs are disabled")
}

func TestMissingPlatformIsDeclared(t *testing.T) {
	e := newIEEnv(t)
	target, err := CurrentTarget()
	if err != nil {
		t.Fatal(err)
	}
	e.fake.Gaps = map[string]string{target: "no artifact for this platform"}
	defer func() { e.fake.Gaps = nil }()
	_, err = e.ensure(t, "faketool", EnsureOptions{})
	ieWantInstallError(t, err, "no artifact")
}

func TestCorruptFactsDegradesToEmpty(t *testing.T) {
	e := newIEEnv(t)
	if _, err := e.ensure(t, "faketool", EnsureOptions{}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(e.runtime, "facts.json"), []byte("{ not json"), 0o644)
	if ieInstalled(t, "faketool") {
		t.Fatal("corrupt facts still report an install")
	}
	if !isRegularFile(filepath.Join(e.runtime, "facts.corrupt")) {
		t.Fatal("unparsable bytes were discarded")
	}
}

func TestConcurrentInstallsDoNotClobber(t *testing.T) {
	e := newIEEnv(t)
	e.pin(t, "deptool", "1.0", e.tar(t, "deptool-1.0.tar.gz", map[string]string{"bin/faketool": "y"}))
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, name := range []string{"faketool", "deptool"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			if _, err := ensure(context.Background(), name, EnsureOptions{BaseEnv: map[string]string{}}); err != nil {
				errs <- err
			}
		}(name)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if !ieInstalled(t, "faketool") || !ieInstalled(t, "deptool") {
		t.Fatal("a concurrent install clobbered the other")
	}
}

// Writer counting the store lock wait notices
type ieWaitCounter struct {
	mu    sync.Mutex
	lines int
}

func (w *ieWaitCounter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.lines += bytes.Count(p, []byte("\n"))
	w.mu.Unlock()
	return len(p), nil
}

func (w *ieWaitCounter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lines
}

func TestSingleFlightOneStoreEntry(t *testing.T) {
	e := newIEEnv(t)
	waiting := &ieWaitCounter{}
	previous := installLockWaitOutput
	installLockWaitOutput = waiting
	defer func() { installLockWaitOutput = previous }()
	archive := readFile(t, filepath.Join(e.docroot, "faketool-1.0.tar.gz"))
	release := make(chan struct{})
	var mu sync.Mutex
	var bodies []string
	server := ieGatedServer(t, func(path string, rangeHeader string) []byte {
		if rangeHeader != "bytes=0-0" {
			mu.Lock()
			bodies = append(bodies, path)
			mu.Unlock()
			<-release
		}
		return archive
	})
	lock := NewLockfile(e.lockPath)
	lock.SetPin("faketool", "1.0", map[string]any{"any": map[string]any{"url": server + "/faketool-1.0.tar.gz", "sha256": e.digest}})
	if err := lock.Save(); err != nil {
		t.Fatal(err)
	}
	runners := make([]*Runner, 6)
	var wg sync.WaitGroup
	for i := range runners {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runner, err := ensure(context.Background(), "faketool", EnsureOptions{BaseEnv: map[string]string{}})
			if err != nil {
				t.Error(err)
			}
			runners[i] = runner
		}(i)
	}
	deadline := time.Now().Add(15 * time.Second)
	for waiting.count() < 5 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	close(release)
	wg.Wait()
	if waiting.count() < 5 {
		t.Fatalf("only %d contenders queued on the store lock", waiting.count())
	}
	if len(bodies) != 1 || bodies[0] != "/faketool-1.0.tar.gz" {
		t.Fatalf("bodies %v", bodies)
	}
	if got := e.fake.stages.Load(); got != 1 {
		t.Fatalf("staged %d times", got)
	}
	for _, runner := range runners {
		if runner == nil || !reflect.DeepEqual(runner.Env, runners[0].Env) {
			t.Fatalf("runners disagree %v", runners)
		}
	}
	entry := e.entry(t, "faketool")
	if !strings.Contains(runners[0].Env["PATH"], entry) || string(readFile(t, filepath.Join(e.runtime, entry, "bin/faketool"))) != "#!x" {
		t.Fatal("published entry does not match the runner")
	}
}

func TestUnremovableReplacedEntryOnPosixIsSetAsideWithoutTouchingOutside(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("POSIX directory modes as a non root user")
	}
	e := newIEEnv(t)
	if _, err := e.ensure(t, "faketool", EnsureOptions{}); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(e.runtime, e.entry(t, "faketool"))
	outside := filepath.Join(e.tmp, "outside.txt")
	writeFile(t, outside, []byte("keep"), 0o644)
	for _, dir := range []string{"sealed", "pinned"} {
		if err := os.Mkdir(filepath.Join(entry, dir), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(entry, "pinned", "link")); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(entry, "sealed"), 0)
	os.Chmod(filepath.Join(entry, "pinned"), 0o500)
	defer func() {
		for _, prefix := range []string{".reclaim-", ".previous-"} {
			for _, held := range ieGlob(t, e.runtime, prefix) {
				items, _ := os.ReadDir(filepath.Join(e.runtime, held))
				for _, item := range items {
					os.Chmod(filepath.Join(e.runtime, held, item.Name()), 0o700)
				}
			}
		}
	}()
	if err := os.Remove(filepath.Join(entry, "bin/faketool")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ensure(t, "faketool", EnsureOptions{}); err != nil {
		t.Fatal(err)
	}
	if string(readFile(t, filepath.Join(entry, "bin/faketool"))) != "#!x" {
		t.Fatal("entry was not republished")
	}
	if st, _ := os.Stat(outside); st.Mode().Perm() != 0o644 {
		t.Fatalf("outside mode changed to %v", st.Mode().Perm())
	}
	if len(ieGlob(t, e.runtime, ".reclaim-")) == 0 {
		t.Fatal("replaced entry was not set aside")
	}
}

func TestEnvForNeverInstalls(t *testing.T) {
	e := newIEEnv(t)
	env := EnvFor([]string{"faketool"}, map[string]string{})
	if strings.Contains(env["PATH"], "faketool-1.0") {
		t.Fatalf("env %v", env)
	}
	if len(ieGlob(t, e.runtime, "faketool-")) > 0 {
		t.Fatal("env_for installed")
	}
}

func TestInstallVerifyFailureReportsReason(t *testing.T) {
	e := newIEEnv(t)
	e.pin(t, "faketool", "2.0", e.tar(t, "faketool-2.0.tar.gz", map[string]string{"nope/x": "y"}))
	_, err := e.ensure(t, "faketool", EnsureOptions{Explicit: true})
	ie := ieWantInstallError(t, err, "failed verification")
	if !strings.Contains(ie.Error(), "bin/faketool") || !strings.Contains(ie.Error(), "missing") {
		t.Fatalf("verification failure hides its reason: %s", ie.Error())
	}
}

func TestStorePathDirsIncludeNodeNpmWhenInstalled(t *testing.T) {
	tmp := isolateHome(t)
	runtimeDir := filepath.Join(tmp, "runtime")
	t.Setenv(EnvRuntimeDir, runtimeDir)
	lockPath := filepath.Join(tmp, "repo-lock", "lock.json")
	ieEmptyLock(t, lockPath)
	previous := lockfilePath
	lockfilePath = func() string { return lockPath }
	defer func() { lockfilePath = previous }()
	lock := NewLockfile(lockPath)
	for _, name := range []string{"node", "npm"} {
		lock.SetPin(name, "1", map[string]any{"any": map[string]any{"url": "x", "sha256": strings.Repeat("0", 64)}})
	}
	if err := lock.Save(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, _ := StoreRoot(ctx)
	factsPath, _ := FactsPath(ctx)
	writeFile(t, factsPath, []byte(`{"schema": 1, "packages": {}}`), 0o644)
	facts, err := NewFacts(factsPath, false)
	if err != nil {
		t.Fatal(err)
	}
	target, _ := CurrentTarget()
	for _, name := range []string{"node", "npm"} {
		entry := name + "-1"
		p, err := GetPackage(name)
		if err != nil {
			t.Fatal(err)
		}
		binary := p.Binary(filepath.Join(store, entry), target)
		writeFile(t, binary, []byte("installed tool"), 0o755)
		if err := facts.Record(name, "1", entry, EnvDiff{"PATH": []string{"{store}/" + entry}}, store,
			FactRecord{Target: &target, Artifacts: []string{strings.Repeat("0", 64)}}); err != nil {
			t.Fatal(err)
		}
	}
	dirs, err := StorePathDirs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	index := func(suffix string) int {
		for i, d := range dirs {
			if strings.HasSuffix(d, suffix) {
				return i
			}
		}
		return -1
	}
	if index("node-1") < 0 || index("npm-1") < 0 || index("npm-1") > index("node-1") {
		t.Fatalf("dirs %v", dirs)
	}
	os.MkdirAll(filepath.Join(store, "uv-1"), 0o777)
	if err := facts.Record("uv", "1", "uv-1", EnvDiff{"PATH": []string{"{store}/uv-1"}}, store, FactRecord{}); err != nil {
		t.Fatal(err)
	}
	if dirs, _ = StorePathDirs(ctx); index("uv-1") >= 0 {
		t.Fatalf("internal uv reached PATH %v", dirs)
	}
}

func TestInstalledLookupPrefersCurrentPinThenRecordedFallback(t *testing.T) {
	e := newIEEnv(t)
	ctx := context.Background()
	if _, err := e.ensure(t, "faketool", EnsureOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	original, err := Installed(ctx, "faketool", false)
	if err != nil || original == nil || !isRegularFile(original.Binary) {
		t.Fatalf("installed %v %v", original, err)
	}
	lock := NewLockfile(e.lockPath)
	lock.SetPin("faketool", "2.0", map[string]any{"any": map[string]any{"url": e.baseURL + "/faketool-2.0.tar.gz", "sha256": strings.Repeat("b", 64)}})
	lock.Save()
	if got, _ := Installed(ctx, "faketool", false); got != nil {
		t.Fatalf("outdated pin answered %v", got)
	}
	if got, _ := Installed(ctx, "faketool", true); got == nil || *got != *original {
		t.Fatalf("fallback %v", got)
	}
	writable := filepath.Join(e.tmp, ".icebath", "tools")
	if err := os.Rename(e.runtime, writable); err != nil {
		t.Fatal(err)
	}
	sealed := filepath.Join(e.tmp, "sealed", "tools")
	os.MkdirAll(sealed, 0o777)
	writeFile(t, filepath.Join(e.tmp, "sealed", "manifest.json"), []byte("{}"), 0o644)
	t.Setenv(EnvRuntimeDir, sealed)
	got, err := Installed(ctx, "faketool", true)
	if err != nil || got == nil || got.Version != original.Version || !strings.HasPrefix(got.Binary, writable) {
		t.Fatalf("shipped then writable lookup %v %v", got, err)
	}
	os.Remove(got.Binary)
	if got, _ := Installed(ctx, "faketool", true); got != nil {
		t.Fatalf("missing binary answered %v", got)
	}
}

func TestInstalledStateFactInSharedFactsFileIsNotAStoreInstall(t *testing.T) {
	e := newIEEnv(t)
	if _, err := e.ensure(t, "faketool", EnsureOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	facts := e.facts(t)
	if err := facts.RecordState("venv", "stamp", []string{"all"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if facts.Installed("venv", nil, e.runtime, nil) {
		t.Fatal("a state fact counted as a store install")
	}
	composed := EnvFor(append([]string{"venv", "faketool"}, AllPackages()...), nil)
	if composed["PATH"] != EnvFor([]string{"faketool"}, nil)["PATH"] {
		t.Fatal("the state fact changed the composed env")
	}
}

func TestInstalledEntryStagedWithoutExecBitIsUsableAgain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX exec bit")
	}
	e := newIEEnv(t)
	ctx := context.Background()
	if _, err := e.ensure(t, "faketool", EnsureOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	first, _ := Installed(ctx, "faketool", false)
	os.Chmod(first.Binary, 0o644)
	healed, err := Installed(ctx, "faketool", false)
	if err != nil || healed == nil || healed.Binary != first.Binary || !canExecute(first.Binary) {
		t.Fatalf("healed %v %v", healed, err)
	}
}

func TestInstallUvLauncherIsReadOnlyAndNamesOnlyUv(t *testing.T) {
	newIEEnv(t)
	if _, err := UvLauncher(context.Background(), "python"); err == nil || errorClass(err) != "ValueError" {
		t.Fatalf("got %v", err)
	}
	ieSwapRegistry(t, &ieFakeTool{BinaryPackage: ieBinary("uv", "bin/uv")})
	if launcher, err := UvLauncher(context.Background(), "uvx"); err != nil || launcher != "" {
		t.Fatalf("uninstalled uv answered %q %v", launcher, err)
	}
}

// Loopback server answering archive bytes from a body function that may block on the body request
func ieGatedServer(t *testing.T, body func(path, rangeHeader string) []byte) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := body(r.URL.Path, r.Header.Get("Range"))
		http.ServeContent(w, r, r.URL.Path, time.Unix(0, 0), bytes.NewReader(payload))
	}))
	t.Cleanup(server.Close)
	return server.URL
}
