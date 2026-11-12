package pm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

// Runs the cli install of names with progress off a terminal, returning the failure count and what it printed
func ieInstallNames(t *testing.T, names []string, verify bool) (int, string) {
	t.Helper()
	var out bytes.Buffer
	peSwap(t, &stderrStream, func() io.Writer { return &bytes.Buffer{} })
	failed, err := installNamesFor(withSyncOutput(context.Background(), &out), names, nil, verify)
	if err != nil {
		t.Fatal(err)
	}
	return failed, out.String()
}

// Runs pm gc and fails the test on an error escaping it
func ieGC(t *testing.T) {
	t.Helper()
	var out bytes.Buffer
	if _, err := CmdGC(withSyncOutput(context.Background(), &out)); err != nil {
		t.Fatalf("gc %v: %s", err, out.String())
	}
}

// Models a Windows hold, removal under these prefixes failing past every retry while rename still works
func ieHold(t *testing.T, prefixes ...string) *atomic.Bool {
	t.Helper()
	held := &atomic.Bool{}
	held.Store(true)
	real := removeEntry
	peSwap(t, &removeEntry, func(store *Store, name string, attempts int) error {
		if held.Load() && slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(name, prefix) }) {
			return &fs.PathError{Op: "remove", Path: store.Entry(name), Err: syscall.EACCES}
		}
		return real(store, name, attempts)
	})
	return held
}

// Installed binary of a package in the fixture store
func (e *ieEnv) binary(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(e.runtime, e.entry(t, name), "bin", "faketool")
}

func TestMultiArchiveMergesIntoOneEntry(t *testing.T) {
	e := newIEEnv(t)
	digestA := e.tar(t, "multitool-1.0-a.tar.gz", map[string]string{"bin/multitool": "#!a"})
	digestB := e.tar(t, "multitool-1.0-b.tar.gz", map[string]string{"lib/extra.so": "y"})
	lock := NewLockfile(e.lockPath)
	lock.SetPin("multitool", "1.0", map[string]any{"any": []any{
		map[string]any{"url": e.baseURL + "/multitool-1.0-a.tar.gz", "sha256": digestA},
		map[string]any{"url": e.baseURL + "/multitool-1.0-b.tar.gz", "sha256": digestB},
	}})
	if err := lock.Save(); err != nil {
		t.Fatal(err)
	}
	failed, out := ieInstallNames(t, []string{"multitool"}, true)
	if failed != 0 {
		t.Fatalf("install failed: %s", out)
	}
	for _, want := range []string{"✓ multitool", "multitool: 100.0%", "multitool: unpacking 1/2", "multitool: unpacking 2/2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("install output lacks %q: %s", want, out)
		}
	}
	if !ieInstalled(t, "multitool") {
		t.Fatal("multitool is not installed")
	}
	entries := ieGlob(t, e.runtime, "multitool-")
	if len(entries) != 1 {
		t.Fatalf("expected one merged entry, got %v", entries)
	}
	entry := filepath.Join(e.runtime, entries[0])
	if !isRegularFile(filepath.Join(entry, "bin", "multitool")) || !isRegularFile(filepath.Join(entry, "lib", "extra.so")) {
		t.Fatal("the archives did not merge into one entry")
	}
	oldFacts := readFile(t, filepath.Join(e.runtime, "facts.json"))
	good := e.tar(t, "multitool-2.0-a.tar.gz", map[string]string{"bin/multitool": "#!new"})
	e.tar(t, "multitool-2.0-b.tar.gz", map[string]string{"lib/extra.so": "z"})
	lock = NewLockfile(e.lockPath)
	lock.SetPin("multitool", "2.0", map[string]any{"any": []any{
		map[string]any{"url": e.baseURL + "/multitool-2.0-a.tar.gz", "sha256": good},
		map[string]any{"url": e.baseURL + "/multitool-2.0-b.tar.gz", "sha256": strings.Repeat("0", 64)},
	}})
	if err := lock.Save(); err != nil {
		t.Fatal(err)
	}
	_, err := e.ensure(t, "multitool", EnsureOptions{})
	ie := ieWantInstallError(t, err, "")
	if !regexp.MustCompile(`sha256 mismatch.*multitool-2.0-b`).MatchString(ie.Error()) {
		t.Fatalf("install error %q does not name the bad archive", ie.Error())
	}
	if !bytes.Equal(readFile(t, filepath.Join(e.runtime, "facts.json")), oldFacts) {
		t.Fatal("a failed archive changed facts")
	}
	if string(readFile(t, filepath.Join(entry, "bin/multitool"))) != "#!a" || string(readFile(t, filepath.Join(entry, "lib/extra.so"))) != "y" {
		t.Fatal("a failed archive changed the live entry")
	}
	if left := ieGlob(t, e.runtime, "multitool-2.0"); len(left) > 0 {
		t.Fatalf("the failed pin left %v", left)
	}
}

func TestCliEnvReportsOnlyPackageExports(t *testing.T) {
	e := newIEEnv(t)
	e.pin(t, "deptool", "1.0", e.tar(t, "deptool-1.0.tar.gz", map[string]string{"bin/faketool": "y"}))
	if _, err := ensure(context.Background(), "deptool", EnsureOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_API_KEY", "never-print-this-secret")
	t.Setenv("PATH", "inherited-path-is-not-a-pm-export")
	var out bytes.Buffer
	code, err := CmdEnv(withSyncOutput(context.Background(), &out), []string{"deptool"})
	if code != 0 || err != nil {
		t.Fatalf("env %d %v", code, err)
	}
	output := out.String()
	if strings.Contains(output, "never-print-this-secret") || strings.Contains(output, "inherited-path-is-not-a-pm-export") {
		t.Fatalf("env leaked the process environment: %s", output)
	}
	var env map[string]any
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env["DEPTOOL_SEEN"] != "1" || !strings.Contains(env["PATH"].(string), "deptool-1.0") {
		t.Fatalf("env %v", env)
	}
}

func TestActivationTrustsARecordedEntryADeliberateInstallRepairs(t *testing.T) {
	e := newIEEnv(t)
	e.pin(t, "faketool", "1.0", e.tar(t, "faketool-1.0.tar.gz", map[string]string{"bin/faketool": "good"}))
	if failed, out := ieInstallNames(t, []string{"faketool"}, true); failed != 0 {
		t.Fatalf("install failed: %s", out)
	}
	binary := e.binary(t, "faketool")
	if err := os.WriteFile(binary, []byte("corrupt"), 0o755); err != nil {
		t.Fatal(err)
	}
	var checked []string
	original := entryVerified
	peSwap(t, &entryVerified, func(p Package, fact map[string]any, store *Store, target string) bool {
		checked = append(checked, p.Base().Name)
		return original(p, fact, store, target)
	})
	if failed, out := ieInstallNames(t, []string{"faketool"}, false); failed != 0 {
		t.Fatalf("trusted install failed: %s", out)
	}
	if len(checked) != 0 || string(readFile(t, binary)) != "corrupt" {
		t.Fatalf("a trusted install re-hashed %v", checked)
	}
	if failed, out := ieInstallNames(t, []string{"faketool"}, true); failed != 0 {
		t.Fatalf("deliberate install failed: %s", out)
	}
	if !slices.Contains(checked, "faketool") || string(readFile(t, binary)) != "good" {
		t.Fatalf("a deliberate install kept the corruption, checked %v", checked)
	}
}

func TestWarmInstallVerifiesSharedDependenciesOnceUnderLock(t *testing.T) {
	e := newIEEnv(t)
	for _, name := range []string{"deptool", "toptool"} {
		e.pin(t, name, "1.0", e.tar(t, name+"-1.0.tar.gz", map[string]string{"bin/faketool": name}))
	}
	if failed, out := ieInstallNames(t, []string{"deptool", "toptool"}, true); failed != 0 {
		t.Fatalf("install failed: %s", out)
	}
	checked := map[string]int{}
	var locked []bool
	original := entryVerified
	peSwap(t, &entryVerified, func(p Package, fact map[string]any, store *Store, target string) bool {
		f, err := os.OpenFile(filepath.Join(store.Root, ".install.lock"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		held, err := LockFD(f, false, 0)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		locked = append(locked, !held)
		checked[p.Base().Name]++
		return original(p, fact, store, target)
	})
	if failed, out := ieInstallNames(t, []string{"deptool", "toptool"}, true); failed != 0 {
		t.Fatalf("warm install failed: %s", out)
	}
	if !reflect.DeepEqual(checked, map[string]int{"deptool": 1, "toptool": 1}) {
		t.Fatalf("checked %v", checked)
	}
	for _, held := range locked {
		if !held {
			t.Fatal("validation must share the publication lock")
		}
	}
	binary := e.binary(t, "deptool")
	release, err := NewStore(e.runtime).InstallLock()
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(binary, []byte("corrupt"), 0o755)
	release()
	if failed, out := ieInstallNames(t, []string{"toptool"}, true); failed != 0 {
		t.Fatalf("install after the writer failed: %s", out)
	}
	if string(readFile(t, binary)) != "deptool" {
		t.Fatal("validity was reused across a writer's mutation")
	}
}

func TestInstallForgetsVerificationWhenStateOperationReleasesLock(t *testing.T) {
	e := newIEEnv(t)
	for _, name := range []string{"deptool", "toptool"} {
		e.pin(t, name, "1.0", e.tar(t, name+"-1.0.tar.gz", map[string]string{"bin/faketool": name}))
	}
	if failed, out := ieInstallNames(t, []string{"toptool"}, true); failed != 0 {
		t.Fatalf("install failed: %s", out)
	}
	binary := e.binary(t, "deptool")
	peSwap(t, &installSyncVenv, func(context.Context, SyncVenvOptions) error {
		f, err := os.OpenFile(filepath.Join(e.runtime, ".install.lock"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		held, err := LockFD(f, false, 0)
		if err != nil || !held {
			t.Fatalf("tool lock leaked into the state operation %v", err)
		}
		return os.WriteFile(binary, []byte("corrupt"), 0o755)
	})
	if failed, out := ieInstallNames(t, []string{"deptool", "venv", "toptool"}, true); failed != 0 {
		t.Fatalf("install failed: %s", out)
	}
	if string(readFile(t, binary)) != "deptool" {
		t.Fatal("a verification survived the state operation's release of the lock")
	}
}

func TestGcKeepsUsedRemovesOrphans(t *testing.T) {
	e := newIEEnv(t)
	if _, err := e.ensure(t, "faketool", EnsureOptions{}); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(e.runtime, "orphan-9.9-nowhere")
	for _, dir := range []string{orphan, filepath.Join(e.runtime, ".staging-abandoned", "tree"), filepath.Join(e.runtime, ".previous-faketool-1.0")} {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	ieGC(t)
	if lexists(orphan) || lexists(filepath.Join(e.runtime, ".staging-abandoned")) {
		t.Fatal("gc kept an orphan")
	}
	if !isDirectory(filepath.Join(e.runtime, ".previous-faketool-1.0")) {
		t.Fatal("gc removed a killed installer's restore point")
	}
	if len(ieGlob(t, e.runtime, "faketool-1.0")) == 0 {
		t.Fatal("gc removed the used entry")
	}
}

func TestHeldReplacedEntryDoesNotFailReinstallAndIsReclaimed(t *testing.T) {
	e := newIEEnv(t)
	if _, err := e.ensure(t, "faketool", EnsureOptions{}); err != nil {
		t.Fatal(err)
	}
	entryName := e.entry(t, "faketool")
	binary := filepath.Join(e.runtime, entryName, "bin", "faketool")
	held := ieHold(t, ".previous-", ".reclaim-")
	for range 2 {
		if err := os.Remove(binary); err != nil {
			t.Fatal(err)
		}
		runner, err := e.ensure(t, "faketool", EnsureOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(runner.Env["PATH"], entryName) || string(readFile(t, binary)) != "#!x" {
			t.Fatal("the reinstall did not republish the entry")
		}
		if lexists(filepath.Join(e.runtime, ".previous-"+entryName)) {
			t.Fatal("the held replaced entry blocks the next publish")
		}
	}
	if got := ieGlob(t, e.runtime, ".reclaim-"); len(got) != 2 {
		t.Fatalf("set aside %v", got)
	}
	held.Store(false)
	ieGC(t)
	if got := ieGlob(t, e.runtime, ".reclaim-"); len(got) != 0 || !isRegularFile(binary) {
		t.Fatalf("gc left %v", got)
	}
}

func TestHeldDisplacementDoesNotFailRestoreAndGcSparesInterruptedOne(t *testing.T) {
	e := newIEEnv(t)
	if _, err := e.ensure(t, "faketool", EnsureOptions{}); err != nil {
		t.Fatal(err)
	}
	entryName := e.entry(t, "faketool")
	store := NewStore(e.runtime)
	entry := store.Entry(entryName)
	if err := copyTreeLike(entry, store.Entry(".previous-"+entryName), false, nil); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(entry, "corrupt.txt"), []byte("bad publish"), 0o644)
	interrupted := store.Entry(".displaced-" + strings.Repeat("f", 32))
	if err := os.Mkdir(interrupted, 0o777); err != nil {
		t.Fatal(err)
	}
	held := ieHold(t, ".displaced-", ".reclaim-")
	if _, err := e.ensure(t, "faketool", EnsureOptions{Explicit: true}); err != nil {
		t.Fatal(err)
	}
	if lexists(filepath.Join(entry, "corrupt.txt")) || lexists(store.Entry(".previous-"+entryName)) {
		t.Fatal("the restore did not land")
	}
	if got := ieGlob(t, e.runtime, ".reclaim-"); len(got) != 1 {
		t.Fatalf("set aside %v", got)
	}
	held.Store(false)
	ieGC(t)
	if got := ieGlob(t, e.runtime, ".reclaim-"); len(got) != 0 {
		t.Fatalf("gc left %v", got)
	}
	if !isDirectory(interrupted) || !isRegularFile(filepath.Join(entry, "bin", "faketool")) {
		t.Fatal("gc removed an interrupted restore's displacement or the live entry")
	}
}

func TestGcRemovesFetchCacheArchives(t *testing.T) {
	e := newIEEnv(t)
	if _, err := e.ensure(t, "faketool", EnsureOptions{}); err != nil {
		t.Fatal(err)
	}
	target, err := CurrentTarget()
	if err != nil {
		t.Fatal(err)
	}
	artifact := NewLockfile(e.lockPath).Artifacts("faketool", target)[0]
	store := NewStore(e.runtime)
	scratch, cleanup, err := store.Scratch()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Fetch(artifact["url"].(string), artifact["sha256"].(string), scratch, nil, nil); err != nil {
		t.Fatal(err)
	}
	cleanup()
	if len(ieGlob(t, e.runtime, "fetch-")) == 0 {
		t.Fatal("the fetch left no archive cache")
	}
	ieGC(t)
	if left := ieGlob(t, e.runtime, "fetch-"); len(left) > 0 {
		t.Fatalf("gc must remove the fetch-<sha> archive cache, left %v", left)
	}
	if len(ieGlob(t, e.runtime, "faketool-1.0")) == 0 {
		t.Fatal("the live package entry survives gc")
	}
}

func TestDoctorFlagsTamperedEntryBytes(t *testing.T) {
	e := newIEEnv(t)
	if _, err := e.ensure(t, "faketool", EnsureOptions{}); err != nil {
		t.Fatal(err)
	}
	if code, out := ieDoctor(t); code != 0 {
		t.Fatalf("doctor %d on a clean install: %s", code, out)
	}
	binary := e.binary(t, "faketool")
	original := readFile(t, binary)
	os.WriteFile(binary, append(append([]byte{}, original...), "tampered"...), 0o755)
	if code, out := ieDoctor(t); code != 1 || !strings.Contains(out, "realized bytes do not match recorded digest") {
		t.Fatalf("doctor %d on tampered bytes: %s", code, out)
	}
	os.WriteFile(binary, original, 0o755)
	if code, out := ieDoctor(t); code != 0 {
		t.Fatalf("doctor %d after restoring the bytes: %s", code, out)
	}
}

func TestPythonPackageDoesNotSignNonMacosRuntime(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS signs its managed Python")
	}
	peSwap(t, &runProcess, func(processSpec) (processResult, error) {
		t.Fatal("codesign must not run outside macOS")
		return processResult{}, nil
	})
	if SignManagedPython(filepath.Join(t.TempDir(), "python")) {
		t.Fatal("a non macOS runtime was signed")
	}
}

// Runs a codesign command and returns its combined output, failing the test on a nonzero exit
func ieCodesign(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("codesign", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("codesign %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestPythonPackageStablySignsMacosRuntime(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("codesign identities exist only on macOS")
	}
	python, err := GetPackage("python")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	staged := filepath.Join(tmp, "staged")
	binary := filepath.Join(staged, "python", "bin", "python3")
	for _, dir := range []string{filepath.Dir(binary), filepath.Join(staged, "python", "lib")} {
		if err := os.MkdirAll(dir, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	interpreter, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	if interpreter, err = filepath.EvalSymlinks(interpreter); err != nil {
		t.Fatal(err)
	}
	if err := copy2(interpreter, binary); err != nil {
		t.Fatal(err)
	}
	ieCodesign(t, "--force", "--sign", "-", "--timestamp=none", "--identifier", "test.icebath.downloaded",
		"--requirements", `=designated => identifier "test.icebath.downloaded"`, binary)
	target, err := CurrentTarget()
	if err != nil {
		t.Fatal(err)
	}
	if err := python.Stage(NewStore(filepath.Join(tmp, "store")), staged, "fixture", target); err != nil {
		t.Fatal(err)
	}
	binary = python.Binary(staged, target)
	ieCodesign(t, "--verify", "--deep", "--strict", binary)
	identity := ieCodesign(t, "-d", "-r-", binary)
	if !strings.Contains(identity, `designated => identifier "ai.icebath.managed-python"`) {
		t.Fatalf("identity %s", identity)
	}
}
