package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Replaces a package level seam for the test and restores it at cleanup
func swapSeam[T any](t *testing.T, seam *T, double T) {
	t.Helper()
	prev := *seam
	*seam = double
	t.Cleanup(func() { *seam = prev })
}

// Launcher scripts with their cmd spellings under root
func toolchainBin(t *testing.T, root string, names ...string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		for _, spelling := range []string{name, name + ".cmd"} {
			if err := os.WriteFile(filepath.Join(root, spelling), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

// Platform shaped executable, name.exe on Windows and an executable bare name elsewhere
func bareExe(t *testing.T, dir, name string) string {
	t.Helper()
	exe, body := filepath.Join(dir, name), "#!/bin/sh\nexit 0\n"
	if runtime.GOOS == "windows" {
		exe, body = exe+".exe", ""
	}
	if err := os.WriteFile(exe, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

// Package manager that ships npm and node in nodeDirs and uv beside uv, ensure always succeeding
func pmShips(t *testing.T, nodeDirs []string, uv string) {
	t.Helper()
	swapSeam(t, &pmMissingReason, func(string) (string, error) { return "", nil })
	swapSeam(t, &pmEnsure, func(context.Context, string) error { return nil })
	swapSeam(t, &pmNodeEnv, func(base map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		out["PATH"] = strings.Join(nodeDirs, string(os.PathListSeparator))
		return out
	})
	swapSeam(t, &pmUVLauncher, func(_ context.Context, name string) (string, error) {
		if uv == "" {
			return "", nil
		}
		return filepath.Join(filepath.Dir(uv), name), nil
	})
}

// Sealed payload npm and node dirs with a real node file, returning the npm dir and node
func sealedNpx(t *testing.T, dir string) (string, string) {
	t.Helper()
	npmBin := toolchainBin(t, filepath.Join(dir, "store", "npm"), "npx")
	cli := filepath.Join(npmBin, "node_modules", "npm", "bin")
	if err := os.MkdirAll(cli, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(cli, "npm-cli.js"), nil, 0o644)
	nodeBin := filepath.Join(dir, "store", "node")
	os.MkdirAll(nodeBin, 0o755)
	node := bareExe(t, nodeBin, "node")
	swapSeam(t, &pmUserFacingApplies, func(context.Context) (bool, error) { return true, nil })
	sealed := map[string][]string{"node": {nodeBin}, "npm": {npmBin}}
	swapSeam(t, &pmSealedDirs, func(_ context.Context, name string) ([]string, error) { return sealed[name], nil })
	return npmBin, node
}

// Test binary started in a fake subprocess mode
func fakeSubprocess(ctx context.Context, mode string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0])
	cmd.Env = append(os.Environ(), fakeSubprocessChild+"="+mode)
	return cmd
}

func TestBareNodeLaunchersResolvePmNodeAheadOfTheUsers(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	userBin := toolchainBin(t, filepath.Join(dir, "user-node"), "npx", "node")
	npmBin := toolchainBin(t, filepath.Join(dir, "store", "npm", "bin"), "npx", "npm")
	nodeBin := toolchainBin(t, filepath.Join(dir, "store", "node", "bin"), "node")
	pmShips(t, []string{npmBin, nodeBin}, "")
	sep := string(os.PathListSeparator)
	command, env, err := resolveStdioCommand(context.Background(), "npx", map[string]string{"PATH": userBin + sep + "/usr/bin"})
	if err != nil || filepath.Dir(command) != npmBin {
		t.Fatalf("%q %v", command, err)
	}
	if got := strings.Split(env["PATH"], sep); !slices.Equal(got, []string{npmBin, nodeBin, userBin, "/usr/bin"}) {
		t.Fatalf("PATH %q", got)
	}
}

func TestPackagedNpxRunsThePackageBinOnThePackagedNode(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	npmBin, node := sealedNpx(t, dir)
	var mu sync.Mutex
	var calls [][]string
	swapSeam(t, &npxInstallCommand, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		argv := append([]string{name}, args...)
		mu.Lock()
		calls = append(calls, argv)
		mu.Unlock()
		pkg := filepath.Join(argv[slices.Index(argv, "--prefix")+1], "node_modules", "@scope", "srv")
		os.MkdirAll(pkg, 0o755)
		os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"bin": {"srv": "dist/index.js"}}`), 0o644)
		return fakeSubprocess(ctx, "exit:0:")
	})
	ctx := context.Background()
	npx := filepath.Join(npmBin, "npx.cmd")
	entry := func(args []string) (string, []string, bool) {
		t.Helper()
		command, argv, ok, err := packagedNpxEntry(ctx, npx, args)
		if err != nil {
			t.Fatal(err)
		}
		return command, argv, ok
	}
	args := []string{"-y", "@scope/srv", "--port", "1"}
	command, argv, ok := entry(args)
	if !ok || !strings.EqualFold(filepath.Clean(command), filepath.Clean(node)) {
		t.Fatalf("command %q %v want %q", command, ok, node)
	}
	if !slices.Equal(argv[1:], []string{"--port", "1"}) || !strings.HasSuffix(argv[0], filepath.Join("@scope", "srv", "dist", "index.js")) {
		t.Fatalf("argv %q", argv)
	}
	if !slices.Contains(calls[0], "--ignore-scripts") || calls[0][0] != command {
		t.Fatalf("install %q", calls[0])
	}
	if c2, a2, _ := entry(args); c2 != command || !slices.Equal(a2, argv) || len(calls) != 2 {
		t.Fatalf("unpinned spec must reinstall %d", len(calls))
	}
	pinned := []string{"-y", "@scope/srv@1.2.3"}
	c1, a1, _ := entry(pinned)
	c2, a2, _ := entry(pinned)
	if c1 != c2 || !slices.Equal(a1, a2) || len(calls) != 3 {
		t.Fatalf("pinned spec reuse %d", len(calls))
	}
	if _, _, ok := entry([]string{"-y", "github:u/r"}); ok || len(calls) != 3 {
		t.Fatal("a git spec must stay plain npx without an install")
	}
	if _, _, ok := entry([]string{"-y", "@scope/srv", "--port", "1"}); !ok {
		t.Fatal("sealed npx lost its entry")
	}
	if _, _, ok, _ := packagedNpxEntry(ctx, filepath.Join(dir, "user-node", "npx.cmd"), args); ok {
		t.Fatal("a user npx is not the sealed payload's")
	}
}

func TestPackagedNpxInstallFailureDegradesToPlainNpx(t *testing.T) {
	failures := map[string]func(ctx context.Context, dir string) *exec.Cmd{
		"timeout": func(ctx context.Context, _ string) *exec.Cmd { return fakeSubprocess(ctx, "sleep") },
		"oserror": func(ctx context.Context, dir string) *exec.Cmd {
			return exec.CommandContext(ctx, filepath.Join(dir, "no-such-node"))
		},
		"nonzero-exit": func(ctx context.Context, _ string) *exec.Cmd { return fakeSubprocess(ctx, "exit:1:npm ERR!") },
	}
	for name, failure := range failures {
		t.Run(name, func(t *testing.T) {
			h := isolateHome(t)
			dir := t.TempDir()
			npmBin, _ := sealedNpx(t, dir)
			swapSeam(t, &npxInstallTimeout, 300*time.Millisecond)
			swapSeam(t, &npxInstallCommand, func(ctx context.Context, _ string, _ ...string) *exec.Cmd { return failure(ctx, dir) })
			npx := filepath.Join(npmBin, "npx.cmd")
			if _, _, ok, err := packagedNpxEntry(context.Background(), npx, []string{"-y", "@scope/srv"}); ok || err != nil {
				t.Fatalf("failed install must fall back to plain npx %v %v", ok, err)
			}
			sum := sha256.Sum256([]byte("@scope/srv"))
			pkg := filepath.Join(h, "cache", "mcp-npx", hex.EncodeToString(sum[:])[:16], "node_modules", "@scope", "srv")
			os.MkdirAll(pkg, 0o755)
			os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"bin": "cli.js"}`), 0o644)
			_, argv, ok, err := packagedNpxEntry(context.Background(), npx, []string{"-y", "@scope/srv"})
			if !ok || err != nil || !slices.Equal(argv, []string{filepath.Join(pkg, "cli.js")}) {
				t.Fatalf("an offline refresh must keep the earlier install %q %v %v", argv, ok, err)
			}
		})
	}
}

func TestOutsidePackageSwapsTheSealedDirsForPmCopies(t *testing.T) {
	dir := t.TempDir()
	sealed := filepath.Join(dir, "sealed")
	copyDir := toolchainBin(t, filepath.Join(dir, "copy"), "npx")
	managed := filepath.Join(dir, "sealed", "npx")
	swapSeam(t, &pmUserFacingCopies, func(context.Context, []string) ([]string, error) { return []string{copyDir}, nil })
	launcher, dirs, err := outsidePackage(context.Background(), "npx", managed, []string{sealed})
	if err != nil || !slices.Equal(dirs, []string{copyDir}) || filepath.Dir(launcher) != copyDir {
		t.Fatalf("%q %q %v", launcher, dirs, err)
	}
	pmUserFacingCopies = func(_ context.Context, d []string) ([]string, error) { return d, nil }
	launcher, dirs, err = outsidePackage(context.Background(), "npx", managed, []string{sealed})
	if err != nil || launcher != managed || !reflect.DeepEqual(dirs, []string{sealed}) {
		t.Fatalf("outside a payload %q %q %v", launcher, dirs, err)
	}
}

func TestBareUvxResolvesPmUvAndAnAbsoluteCommandStaysTheUsers(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	userBin := toolchainBin(t, filepath.Join(dir, "user", ".local", "bin"), "uvx")
	uvDir := toolchainBin(t, filepath.Join(dir, "store", "uv-0.1"), "uv", "uvx")
	pmShips(t, nil, filepath.Join(uvDir, "uv"))
	command, env, err := resolveStdioCommand(context.Background(), "uvx", map[string]string{"PATH": userBin})
	if err != nil || filepath.Dir(command) != uvDir || strings.Split(env["PATH"], string(os.PathListSeparator))[0] != uvDir {
		t.Fatalf("%q %q %v", command, env["PATH"], err)
	}
	explicit := filepath.Join(userBin, "uvx")
	if command, _, err = resolveStdioCommand(context.Background(), explicit, map[string]string{"PATH": "/usr/bin"}); err != nil || command != explicit {
		t.Fatalf("absolute command %q %v", command, err)
	}
}

// Managed bin under an overridden home and a user bin outside it, both holding python3
func managedAndUserBins(t *testing.T, userHasPython bool) (context.Context, string, string, string, string) {
	t.Helper()
	isolateHome(t)
	dir := t.TempDir()
	managedBin := filepath.Join(dir, "home", "bin")
	userBin := filepath.Join(dir, "user-bin")
	os.MkdirAll(managedBin, 0o755)
	os.MkdirAll(userBin, 0o755)
	managedExe, userExe := bareExe(t, managedBin, "python3"), ""
	if userHasPython {
		userExe = bareExe(t, userBin, "python3")
	}
	return home.WithOverride(context.Background(), filepath.Join(dir, "home")), managedBin, userBin, managedExe, userExe
}

func TestBarePython3StepsPastTheManagedRuntimeToTheUserHit(t *testing.T) {
	ctx, managedBin, userBin, _, userExe := managedAndUserBins(t, true)
	path := managedBin + string(os.PathListSeparator) + userBin
	command, env, err := resolveStdioCommand(ctx, "python3", map[string]string{"PATH": path})
	if err != nil || command != userExe || env["PATH"] != path {
		t.Fatalf("%q %q %v", command, env["PATH"], err)
	}
}

func TestBarePython3KeepsTheManagedHitWhenTheUserHasNone(t *testing.T) {
	ctx, managedBin, userBin, managedExe, _ := managedAndUserBins(t, false)
	command, _, err := resolveStdioCommand(ctx, "python3", map[string]string{"PATH": managedBin + string(os.PathListSeparator) + userBin})
	if err != nil || command != managedExe {
		t.Fatalf("%q %v", command, err)
	}
}

func TestFirstUserWhichHitFollowsTheChildEnvPathext(t *testing.T) {
	userBin := t.TempDir()
	bat, wrapper := filepath.Join(userBin, "srv.BAT"), filepath.Join(userBin, "wrap.cmd")
	os.WriteFile(bat, []byte("@echo off\r\n"), 0o755)
	os.WriteFile(wrapper, []byte("@echo off\r\n"), 0o755)
	pathext := map[string]string{"PATHEXT": ".COM;.EXE;.BAT;.CMD"}
	ctx := context.Background()
	if got := firstUserWhichHit(ctx, "srv", userBin, pathext, true); got != bat {
		t.Fatalf("srv %q", got)
	}
	if got := firstUserWhichHit(ctx, "wrap.cmd", userBin, pathext, true); got != wrapper {
		t.Fatalf("wrap.cmd %q", got)
	}
	if got := firstUserWhichHit(ctx, "missing", userBin, pathext, true); got != "" {
		t.Fatalf("missing %q", got)
	}
}

func TestBareLauncherCommandsKeepTheManagedFirstResolution(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	userBin := toolchainBin(t, filepath.Join(dir, "user-bin"), "uvx")
	uvDir := toolchainBin(t, filepath.Join(dir, "store", "uv-0.1"), "uv", "uvx")
	pmShips(t, nil, filepath.Join(uvDir, "uv"))
	command, _, err := resolveStdioCommand(context.Background(), "uvx", map[string]string{"PATH": userBin})
	if err != nil || command != filepath.Join(uvDir, "uvx") {
		t.Fatalf("%q %v", command, err)
	}
}

func TestTailServerStderrScopesToTheNamedServer(t *testing.T) {
	ctx := home.WithOverride(context.Background(), isolateHome(t))
	closeStderrLogs("")
	t.Cleanup(func() { closeStderrLogs("") })
	writeStderrLogHeader(ctx, "beta")
	fh := getStderrLog(ctx)
	fh.WriteString("beta noise\n")
	writeStderrLogHeader(ctx, "alpha")
	fh.WriteString("ModuleNotFoundError: No module named 'requests'\n")
	alpha := tailServerStderr(ctx, "alpha", 8192, 8)
	if !strings.Contains(alpha, "ModuleNotFoundError") || strings.Contains(alpha, "beta noise") {
		t.Fatalf("alpha tail %q", alpha)
	}
	if got := tailServerStderr(ctx, "never-started", 8192, 8); got != "" {
		t.Fatalf("never started %q", got)
	}
}

// Malware check double that sleeps then answers, with the npx cache swap disabled
func slowMalwareCheck(t *testing.T, delay time.Duration, verdict string) {
	t.Helper()
	swapSeam(t, &malwareCheck, func(string, []string) (string, error) { time.Sleep(delay); return verdict, nil })
	swapSeam(t, &npxCacheLookup, func([]string) (string, []string, bool, error) { return "", nil, false, nil })
	swapSeam(t, &pmUserFacingApplies, func(context.Context) (bool, error) { return false, nil })
}

func TestRunStdioMalwareCheckDoesNotBlockEventLoop(t *testing.T) {
	isolateHome(t)
	startTestLoop(t)
	slowMalwareCheck(t, 300*time.Millisecond, "")
	var ticks atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 20 {
			time.Sleep(10 * time.Millisecond)
			if loop := runningLoop(); loop != nil {
				loop.callSoon(func() { ticks.Add(1) })
			}
		}
	}()
	if _, _, err := preflightStdioCommand(context.Background(), "srv", "npx", []string{"-y", "pkg"}); err != nil {
		t.Fatal(err)
	}
	during := ticks.Load()
	<-done
	flushLoop(t)
	if during < 3 {
		t.Fatalf("event loop appeared blocked (ticks=%d)", during)
	}
}

func TestRunStdioMalwareCheckTimesOutFailOpen(t *testing.T) {
	isolateHome(t)
	slowMalwareCheck(t, 500*time.Millisecond, "MALWARE")
	swapSeam(t, &osvMalwareCheckTimeout, 200*time.Millisecond)
	start := time.Now()
	_, _, err := preflightStdioCommand(context.Background(), "srv", "npx", []string{"-y", "pkg"})
	if elapsed := time.Since(start); err != nil || elapsed >= time.Second {
		t.Fatalf("startup did not fail open promptly %v %v", elapsed, err)
	}
}
