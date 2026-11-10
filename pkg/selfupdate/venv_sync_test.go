package selfupdate

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io/fs"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Hidden first argument that routes a re-executed test binary to a venv sync child stand-in
const vsChildEntry = "__venv_sync_test_child__"

// Argv re-executing this test binary as the named venv sync child
func vsChildArgv(mode string, args ...string) []string {
	return append([]string{os.Args[0], vsChildEntry, mode}, args...)
}

// Dispatches a venv sync child stand-in, the completion tail or a relaunched launch
func vsChild(args []string) int {
	original := append([]string(nil), args...)
	args = TakeRelaunchMarker(args)
	ctx := context.Background()
	pm.SetCommandArgv(func(argv []string) []string { return argv })
	if len(args) > 1 && args[1] == SourceCompletionEntry {
		return vsTailStandIn(args[2:])
	}
	if len(args) > 2 && args[1] == "pm" && args[2] == "worker" {
		vsChildToolchain()
		return pm.WorkerMain()
	}
	if probe := os.Getenv("VS_BOOT_PROBE"); probe != "" && (len(args) < 2 || args[1] != vsChildEntry) {
		return vsBootProbe(ctx, args, probe)
	}
	if len(args) < 3 || args[1] != vsChildEntry {
		os.Stderr.WriteString("venv sync child without a mode\n")
		return 2
	}
	mode, rest := args[2], args[3:]
	switch mode {
	case "bare":
		return vsBareChild(ctx, rest[0])
	case "import":
		for _, entry := range pm.ImportPathEntries() {
			if _, err := os.Stat(filepath.Join(entry, rest[0], "__init__.py")); err == nil {
				return 0
			}
		}
		os.Stderr.WriteString("ModuleNotFoundError: No module named '" + rest[0] + "'\n")
		return 1
	case "main":
		return VenvSyncMain(ctx, rest, os.Stdout, os.Stderr)
	case "hold-checkout":
		lock := updaterecord.NewUpdateLock(rest[1], rest[0], true)
		if ok, err := lock.Acquire(ctx); err != nil || !ok {
			os.Stderr.WriteString("could not hold the checkout\n")
			return 1
		}
		os.Stdout.WriteString("held\n")
		time.Sleep(120 * time.Second)
		return 0
	case "launch":
		return vsLaunchChild(ctx, original, args, rest)
	case "probe":
		selected, err := pm.SelectedVenv(ctx, rest[0])
		if err != nil {
			os.Stderr.WriteString(err.Error() + "\n")
			return 1
		}
		site, err := pm.SitePackages(selected)
		if err != nil {
			os.Stderr.WriteString(err.Error() + "\n")
			return 1
		}
		data, err := os.ReadFile(filepath.Join(site, "selected_probe.py"))
		if err != nil {
			os.Stderr.WriteString(err.Error() + "\n")
			return 1
		}
		value := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(string(data)), "VALUE = '"), "'")
		encoded, _ := json.Marshal(map[string]any{"executable": args[0], "value": value})
		os.Stdout.Write(append(encoded, '\n'))
		return 0
	}
	os.Stderr.WriteString("unknown venv sync child mode " + mode + "\n")
	return 2
}

// Completion tail stand in recording its arguments and the run, exiting with the requested status
func vsTailStandIn(argv []string) int {
	if calls := os.Getenv("VS_COMPLETION_CALLS"); calls != "" {
		encoded, _ := json.Marshal(argv)
		f, err := os.OpenFile(calls, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
		if err == nil {
			f.Write(append(encoded, '\n'))
			f.Close()
		}
	}
	if started := os.Getenv("VS_TAIL_STARTED"); started != "" {
		os.WriteFile(started, nil, 0o666)
	}
	code, _ := strconv.Atoi(os.Getenv("VS_COMPLETION_EXIT"))
	return code
}

// Passive paths of a bare process, the PM engine never entered
func vsBareChild(ctx context.Context, root string) int {
	engine := func() { os.Stderr.WriteString("the PM engine was entered\n"); os.Exit(3) }
	venvIsCurrent = func(context.Context, string) (bool, error) { engine(); return false, nil }
	syncVenv = func(context.Context, pm.SyncVenvOptions) error { engine(); return nil }
	ensureToolsForSync = func(context.Context) error { engine(); return nil }
	if result, err := SyncTree(ctx, root, true); err != nil || result.State != "failed" {
		os.Stderr.WriteString("check of an empty tree did not fail\n")
		return 1
	}
	if err := os.WriteFile(filepath.Join(root, "install-stamp.json"), []byte(`{"updateMechanism":"external"}`), 0o666); err != nil {
		return 1
	}
	if result, err := SyncTree(ctx, root, false); err != nil || result != (VenvSyncResult{State: "sealed", OK: true}) {
		os.Stderr.WriteString("an external stamp did not read as sealed\n")
		return 1
	}
	if binary, err := PrepareLaunch(ctx, root, nil); err != nil || binary != "" {
		os.Stderr.WriteString("an external tree prepared a launch\n")
		return 1
	}
	return 0
}

// Environment of a bare child, Python interpreter variables and the dependency site dropped
func vsBareEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "PYTHON") || strings.HasPrefix(kv, "VIRTUAL_ENV=") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

// Runs a bare child the way the hermes harness runs python -I -S
func vsRunBare(t *testing.T, mode string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], vsChildArgv(mode, args...)[1:]...)
	cmd.Env = vsBareEnv()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), stderr.String()
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, stderr.String()
}

// Isolated data root for one test with the CLI splitter installed
func vsHome(t *testing.T, dir string) string {
	t.Helper()
	home := filepath.Join(dir, "home")
	t.Setenv("ICEBATH_HOME", home)
	t.Setenv(pm.EnvDisableLazyInstalls, "")
	os.Unsetenv(pm.EnvDisableLazyInstalls)
	pm.SetCommandArgv(func(argv []string) []string { return argv })
	return home
}

// Swaps a package seam for one test
func vsSwap[T any](t *testing.T, target *T, value T) {
	t.Helper()
	previous := *target
	*target = value
	t.Cleanup(func() { *target = previous })
}

func vsWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o666); err != nil {
		t.Fatal(err)
	}
}

// A git checkout with a project and a lock
func vsCheckout(t *testing.T, tmp, name string) string {
	t.Helper()
	root := filepath.Join(tmp, name)
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o777); err != nil {
		t.Fatal(err)
	}
	vsWrite(t, filepath.Join(root, "pyproject.toml"), "[project]\nname='x'\n")
	vsWrite(t, filepath.Join(root, "uv.lock"), "lock-v1\n")
	return root
}

type vsSyncCall struct {
	root     string
	explicit bool
}

// Answers currency with a constant and records each sync, failing with an install error when asked
func vsWirePM(t *testing.T, current bool, failure string) *[]vsSyncCall {
	t.Helper()
	calls := &[]vsSyncCall{}
	vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return current, nil })
	vsSwap(t, &syncVenv, func(_ context.Context, opts pm.SyncVenvOptions) error {
		if !opts.EvictIncompatiblePlugins {
			t.Fatal("an update sync must disable misfit plugins, not fail")
		}
		*calls = append(*calls, vsSyncCall{opts.ProjectRoot, opts.Explicit})
		if failure != "" {
			return pm.NewInstallError("venv", failure, "")
		}
		return nil
	})
	return calls
}

// Every path under a tree
func vsTree(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && path != root {
			out[path] = true
		}
		return nil
	})
	return out
}

func TestBareImportAndPassivePaths(t *testing.T) {
	tmp := t.TempDir()
	vsHome(t, tmp)
	if code, stderr := vsRunBare(t, "bare", tmp); code != 0 {
		t.Fatalf("bare passive paths exited %d: %s", code, stderr)
	}
}

func TestBareHarnessRejectsThirdPartyImport(t *testing.T) {
	site := t.TempDir()
	vsWrite(t, filepath.Join(site, "requests", "__init__.py"), "")
	t.Setenv("PYTHONPATH", site)
	code, stderr := vsRunBare(t, "import", "requests")
	if code == 0 || !strings.Contains(stderr, "ModuleNotFoundError") {
		t.Fatalf("bare harness imported a third party module: %d %s", code, stderr)
	}
}

func TestCheckoutSyncEachRootUsesThePublicPMTransaction(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(strconv.FormatBool(foreign), func(t *testing.T) {
			tmp := t.TempDir()
			vsHome(t, tmp)
			root := vsCheckout(t, tmp, "co")
			vsSwap(t, &venvSyncProjectRoot, func() string { return root })
			calls := vsWirePM(t, false, "")
			arg := ""
			if foreign {
				arg = root
			}
			result, err := SyncTree(context.Background(), arg, false)
			if err != nil || result != (VenvSyncResult{State: "synced", OK: true}) {
				t.Fatalf("sync %+v %v", result, err)
			}
			if !reflect.DeepEqual(*calls, []vsSyncCall{{root, true}}) {
				t.Fatalf("calls %+v", *calls)
			}
		})
	}
}

func TestCheckoutSyncCheckIsPassive(t *testing.T) {
	tmp := t.TempDir()
	vsHome(t, tmp)
	root := vsCheckout(t, tmp, "co")
	calls := vsWirePM(t, false, "")
	before := vsTree(t, tmp)
	result, err := SyncTree(context.Background(), root, true)
	if err != nil || result != (VenvSyncResult{State: "would-sync", OK: true}) {
		t.Fatalf("check %+v %v", result, err)
	}
	if len(*calls) != 0 {
		t.Fatalf("check synced %+v", *calls)
	}
	if after := vsTree(t, tmp); !reflect.DeepEqual(after, before) {
		t.Fatalf("check wrote files: %v", after)
	}
}

func TestCheckoutSyncAFailedSyncIsReportedAndRetried(t *testing.T) {
	tmp := t.TempDir()
	vsHome(t, tmp)
	root := vsCheckout(t, tmp, "co")
	calls := vsWirePM(t, false, "resolution failed")
	for range 2 {
		out, err := SyncTree(context.Background(), root, false)
		if err != nil || out.State != "failed" || out.OK || !strings.Contains(out.Detail, "resolution failed") {
			t.Fatalf("failed sync %+v %v", out, err)
		}
	}
	if !reflect.DeepEqual(*calls, []vsSyncCall{{root, true}, {root, true}}) {
		t.Fatalf("calls %+v", *calls)
	}
}

func TestSealedTreesASealedTreeIsACleanNoop(t *testing.T) {
	tmp := t.TempDir()
	vsHome(t, tmp)
	root := filepath.Join(tmp, "sealed")
	vsWrite(t, filepath.Join(root, "install-stamp.json"), `{"commit": "abc123", "payload": "full", "updateMechanism": "electron-updater"}`)
	calls := vsWirePM(t, false, "")
	out, err := SyncTree(context.Background(), root, false)
	if err != nil || out != (VenvSyncResult{State: "sealed", OK: true}) {
		t.Fatalf("sealed %+v %v", out, err)
	}
	if len(*calls) != 0 {
		t.Fatalf("sealed tree synced %+v", *calls)
	}
}

func TestSealedTreesADevTreeWithBothStampAndGitIsACheckout(t *testing.T) {
	tmp := t.TempDir()
	vsHome(t, tmp)
	root := vsCheckout(t, tmp, "co")
	vsWrite(t, filepath.Join(root, "install-stamp.json"), `{"commit": "abc", "updateMechanism": "electron-updater"}`)
	vsWirePM(t, false, "")
	if out, err := SyncTree(context.Background(), root, false); err != nil || out.State != "synced" {
		t.Fatalf("dev tree %+v %v", out, err)
	}
}

func TestSealedTreesAStampWithoutUpdateMechanismIsABuildLaneBug(t *testing.T) {
	tmp := t.TempDir()
	vsHome(t, tmp)
	root := filepath.Join(tmp, "sealed")
	vsWrite(t, filepath.Join(root, "install-stamp.json"), `{"commit": "abc"}`)
	_, err := SyncTree(context.Background(), root, false)
	if err == nil || pm.ErrorClass(err) != "RuntimeError" || !strings.Contains(err.Error(), "updateMechanism") {
		t.Fatalf("stamp without a mechanism %v", err)
	}
}

// Runs the module entry in a child and returns its status and stdout
func vsRunMain(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], vsChildArgv("main", args...)[1:]...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), stdout.String(), stderr.String()
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, stdout.String(), stderr.String()
}

func TestCliContractJSONOutputAndExitCodes(t *testing.T) {
	tmp := t.TempDir()
	vsHome(t, tmp)
	root := filepath.Join(tmp, "sealed")
	vsWrite(t, filepath.Join(root, "install-stamp.json"), `{"commit": "x", "updateMechanism": "electron-updater"}`)
	code, stdout, stderr := vsRunMain(t, "--project-root", root, "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if stdout != "{\"state\": \"sealed\", \"ok\": true}\n" {
		t.Fatalf("stdout %q", stdout)
	}
}

func TestCliContractFailureExitsNonzero(t *testing.T) {
	tmp := t.TempDir()
	vsHome(t, tmp)
	code, stdout, _ := vsRunMain(t, "--project-root", tmp, "--json")
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	doc, err := pyjson.Loads(strings.TrimSpace(stdout))
	obj, ok := doc.(*pyjson.Object)
	if err != nil || !ok {
		t.Fatalf("stdout %q", stdout)
	}
	if state, _ := obj.Get("state"); state != "failed" {
		t.Fatalf("state %v", state)
	}
}

// Starts a process holding the checkout lock of root under a marker elsewhere and waits until it holds
func vsHoldCheckout(t *testing.T, root, marker string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], vsChildArgv("hold-checkout", root, marker)[1:]...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	line, _ := bufio.NewReader(stdout).ReadString('\n')
	if strings.TrimSpace(line) != "held" {
		t.Fatalf("holder said %q", line)
	}
}

func TestLaunchRefusesWhileAnotherProcessHoldsTheCheckout(t *testing.T) {
	tmp := t.TempDir()
	home := vsHome(t, tmp)
	root := filepath.Join(tmp, "checkout")
	vsWrite(t, filepath.Join(root, ".git", "HEAD"), "")
	os.Remove(filepath.Join(root, ".git", "HEAD"))
	vsWrite(t, filepath.Join(root, "pyproject.toml"), "[project]\nname='example'\n")
	vsWrite(t, filepath.Join(root, "install-stamp.json"), `{"updateMechanism": "self"}`)
	vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return false, nil })
	var finished []string
	vsSwap(t, &finishSourceUpdate, func(_ context.Context, root string, _ bool, _ string) error {
		finished = append(finished, root)
		return nil
	})
	vsHoldCheckout(t, root, filepath.Join(tmp, "gone"))
	_, err := PrepareLaunch(context.Background(), root, nil)
	if err == nil || !strings.Contains(err.Error(), "an update is still running") {
		t.Fatalf("launch beside a held checkout %v", err)
	}
	if len(finished) != 0 {
		t.Fatal("a second completion tail ran beside the live update tree")
	}
	if _, err := os.Stat(filepath.Join(home, updaterecord.MarkerName)); err == nil {
		t.Fatal("a refused launch left a claim")
	}
}

// A checkout owing the tail whose completion child only records that it ran
func vsTailRoot(t *testing.T, tmp string) (string, string) {
	t.Helper()
	root := filepath.Join(tmp, "checkout")
	if err := os.MkdirAll(root, 0o777); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(tmp, "tail-ran")
	t.Setenv("VS_TAIL_STARTED", started)
	vsHome(t, tmp)
	vsSwap(t, &treeMatchesCompletedStamp, func(context.Context, string) (bool, error) { return false, nil })
	vsSwap(t, &activationEnvironment, func(context.Context, string) ([]string, error) { return nil, nil })
	return root, started
}

func TestATailChildCustodyRefusesNeverRunsAndStaysOwed(t *testing.T) {
	root, started := vsTailRoot(t, t.TempDir())
	vsSwap(t, &completionCustodyRun, func(ctx context.Context, argv []string, _ bool, _ ChildOptions) (*CompletedProcess, error) {
		return nil, updaterecord.RefuseCustodyChild(ctx, argv, &os.SyscallError{Syscall: "OpenProcess", Err: errors.New("Access is denied")}, false)
	})
	err := finishSourceUpdate(context.Background(), root, true, filepath.Join(root, ".update-pending"))
	if err == nil || !strings.Contains(err.Error(), "source update completion failed") {
		t.Fatalf("refused tail %v", err)
	}
	if _, err := os.Stat(started); err == nil {
		t.Fatal("the tail child ran outside the update's custody")
	}
}

// Launch fixture whose store binary spelling and running executable the case chooses
func vsCurrentLaunch(t *testing.T) (string, string, func(spelling, executable string) (string, []string)) {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "checkout")
	vsWrite(t, filepath.Join(root, ".git", "config"), "")
	vsWrite(t, filepath.Join(root, "pyproject.toml"), "[project]\nname='example'\n")
	vsWrite(t, filepath.Join(root, "install-stamp.json"), `{"updateMechanism": "self"}`)
	vsHome(t, tmp)
	vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return true, nil })
	store := filepath.Join(tmp, "store", "cpython-3.14", "bin", "python3")
	vsWrite(t, store, "#!/bin/sh\n")
	var published []string
	vsSwap(t, &publishLaunchers, func(_ context.Context, root string) error { published = append(published, root); return nil })
	run := func(spelling, executable string) (string, []string) {
		vsSwap(t, &resolveStorePython, func(string) string { return spelling })
		vsSwap(t, &runningExecutable, func() (string, error) { return executable, nil })
		target, err := PrepareLaunch(context.Background(), root, nil)
		if err != nil {
			t.Fatal(err)
		}
		return target, published
	}
	return tmp, store, run
}

func TestStorePythonSpelledThroughDotdotHomeIsCurrent(t *testing.T) {
	tmp, store, run := vsCurrentLaunch(t)
	relative, _ := filepath.Rel(tmp, store)
	dotted := filepath.Join(tmp, "work") + string(filepath.Separator) + ".." + string(filepath.Separator) + relative
	if err := os.Mkdir(filepath.Join(tmp, "work"), 0o777); err != nil {
		t.Fatal(err)
	}
	if target, published := run(dotted, store); target != "" || len(published) != 0 {
		t.Fatalf("current store binary relaunched %q %v", target, published)
	}
}

func TestSealedStampReaderHonorsExternalInstallRoot(t *testing.T) {
	tmp := t.TempDir()
	stamped := filepath.Join(tmp, "payload")
	if err := os.Mkdir(stamped, 0o777); err != nil {
		t.Fatal(err)
	}
	vsWrite(t, filepath.Join(tmp, "install-stamp.json"), `{"updateMechanism": "external"}`)
	t.Setenv(pm.EnvInstallRoot, tmp)
	t.Cleanup(pm.PatchRepoRoot(stamped))
	if sealed, err := pm.IsSealedTree(stamped); err != nil || !sealed {
		t.Fatalf("sealed %v %v", sealed, err)
	}
}

func TestDeveloperCheckoutSkipsManagedRuntimeWarning(t *testing.T) {
	checkout := filepath.Join(t.TempDir(), "checkout")
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o777); err != nil {
		t.Fatal(err)
	}
	vsSwap(t, &checkRuntimeActivate, func(context.Context, bool) ([]string, error) {
		t.Fatal("dev checkout activated managed runtime")
		return nil, nil
	})
	if problem, err := CheckRuntime(context.Background(), checkout); problem != "" || err != nil {
		t.Fatalf("dev checkout %q %v", problem, err)
	}
}

func TestCompletionRetryBackoffAndCap(t *testing.T) {
	tmp := t.TempDir()
	vsSwap(t, &completionPendingPath, func(context.Context, string) string { return filepath.Join(tmp, "source-completion-pending") })
	record := filepath.Join(tmp, "source-completion-attempts")
	ctx := context.Background()
	state := func(now func() float64) (bool, int64, int64) {
		may, attempts, backoff := CompletionRetryState(ctx, tmp, now)
		return may, attempts.Int64(), backoff
	}
	if may, attempts, backoff := state(nil); !may || attempts != 0 || backoff != 0 {
		t.Fatalf("no record %v %d %d", may, attempts, backoff)
	}
	vsWrite(t, record, "1\n")
	if may, attempts, backoff := state(nil); !may || attempts != 1 || backoff != 0 {
		t.Fatalf("one failure %v %d %d", may, attempts, backoff)
	}
	vsWrite(t, record, "2\n")
	if may, attempts, _ := state(nil); may || attempts != 2 {
		t.Fatalf("two failures %v %d", may, attempts)
	}
	past := 12345.0
	old := past - CompletionRetryBackoffSeconds - 1
	stamp := time.Unix(int64(old), 0)
	if err := os.Chtimes(record, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if may, attempts, _ := state(func() float64 { return past }); !may || attempts != 2 {
		t.Fatalf("after the window %v %d", may, attempts)
	}
	vsWrite(t, record, strconv.Itoa(CompletionRetryMaxAttempts)+"\n")
	for _, now := range []float64{past, past + 10*CompletionRetryBackoffSeconds} {
		if may, attempts, _ := state(func() float64 { return now }); may || attempts != CompletionRetryMaxAttempts {
			t.Fatalf("at the cap %v %d", may, attempts)
		}
	}
	vsWrite(t, record, "not a number\n")
	if may, attempts, backoff := CompletionRetryState(ctx, tmp, nil); !may || attempts.Cmp(new(big.Int)) != 0 || backoff != 0 {
		t.Fatalf("corrupt record %v %v %d", may, attempts, backoff)
	}
}

// Digest of every file a publication wrote, keyed by name
func vsPublished(t *testing.T, written []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, path := range written {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(path)] = string(sha256Sum(data))
	}
	return out
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

func TestTempHomePrepareLaunchRelaunchesWithoutRebinding(t *testing.T) {
	tmp := t.TempDir()
	ctx := context.Background()
	repo := filepath.Join(tmp, "owner's checkout")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o777); err != nil {
		t.Fatal(err)
	}
	vsWrite(t, filepath.Join(repo, "pyproject.toml"), "[project]\nname = \"x\"\nversion = \"0\"\n")
	vsWrite(t, filepath.Join(repo, "install-stamp.json"), `{"updateMechanism": "self"}`)
	binary := vsInstallBinary(t, repo)
	vsRecordStorePython(t, filepath.Join(tmp, "owner's home", "tools"), "python-owner's home")
	vsRecordStorePython(t, filepath.Join(tmp, "temp", "tools"), "python-temp")
	isolate := func(h string) {
		t.Setenv("HOME", tmp)
		t.Setenv("ICEBATH_HOME", h)
		t.Setenv(pm.EnvRuntimeDir, "")
		os.Unsetenv(pm.EnvRuntimeDir)
		t.Setenv(pm.EnvDisableLazyInstalls, "")
		os.Unsetenv(pm.EnvDisableLazyInstalls)
	}
	isolate(filepath.Join(tmp, "owner's home"))
	pm.SetCommandArgv(func(argv []string) []string { return argv })
	local := filepath.Join(repo, ".icebath", "bin")
	written, err := pm.EnsureInstallLaunchers(ctx, repo, local)
	if err != nil || len(written) != pm.EntryPointCount() {
		t.Fatalf("publication %v %v", written, err)
	}
	before := vsPublished(t, written)
	isolate(filepath.Join(tmp, "temp"))
	vsSwap(t, &venvIsCurrent, func(context.Context, string) (bool, error) { return true, nil })
	target, err := PrepareLaunch(ctx, repo, nil)
	if err != nil || target == "" {
		t.Fatalf("target %q %v", target, err)
	}
	resolvedTarget, _ := filepath.EvalSymlinks(target)
	resolvedBinary, _ := filepath.EvalSymlinks(binary)
	if resolvedTarget != resolvedBinary {
		t.Fatalf("target %s, want %s", target, binary)
	}
	for name, digest := range before {
		data, err := os.ReadFile(filepath.Join(local, name))
		if err != nil || string(sha256Sum(data)) != digest {
			t.Fatalf("launcher %s rebound by a borrowing launch", name)
		}
	}
}
