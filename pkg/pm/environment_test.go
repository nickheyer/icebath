package pm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Removes every ambient uv and pip setting for one test and points pip at an empty config
func peCleanIndexEnv(t *testing.T) string {
	t.Helper()
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "UV_") || strings.HasPrefix(k, "PIP_") {
			t.Setenv(k, "")
			os.Unsetenv(k)
		}
	}
	t.Setenv("PIP_CONFIG_FILE", os.DevNull)
	return t.TempDir()
}

func TestEnvironmentPipIndexReachesUVButAmbientUVSelectionDoesNot(t *testing.T) {
	peCleanIndexEnv(t)
	for k, v := range map[string]string{"PIP_INDEX_URL": "https://mirror.example/simple", "PIP_TRUSTED_HOST": "mirror.example",
		"UV_HTTP_TIMEOUT": "300", "UV_INDEX_CORP_PASSWORD": "s3cret", "UV_PYTHON": "/poison/python",
		"UV_CACHE_DIR": "/poison/cache", "UV_PROJECT_ENVIRONMENT": "/poison/venv"} {
		t.Setenv(k, v)
	}
	env, err := BaseEnvironment(nil)
	if err != nil {
		t.Fatal(err)
	}
	if env["UV_INDEX_URL"] != "https://mirror.example/simple" || env["UV_INSECURE_HOST"] != "mirror.example" ||
		env["UV_HTTP_TIMEOUT"] != "300" || env["UV_INDEX_CORP_PASSWORD"] != "s3cret" {
		t.Fatalf("bridged %v", env)
	}
	for _, k := range []string{"UV_PYTHON", "UV_CACHE_DIR", "UV_PROJECT_ENVIRONMENT"} {
		if _, ok := env[k]; ok {
			t.Fatalf("%s leaked", k)
		}
	}
}

func TestEnvironmentPipConfIsBridgedOnlyWhenUVHasNoIndex(t *testing.T) {
	root := peCleanIndexEnv(t)
	conf := filepath.Join(root, "pip.conf")
	peWrite(t, conf, "[global]\nindex-url = https://user:p%40ss@mirror.example/simple\n")
	t.Setenv("PIP_CONFIG_FILE", conf)
	env, err := BaseEnvironment(nil)
	if err != nil || env["UV_INDEX_URL"] != "https://user:p%40ss@mirror.example/simple" {
		t.Fatalf("got %v %v", env["UV_INDEX_URL"], err)
	}
	t.Setenv("UV_DEFAULT_INDEX", "https://explicit.example/simple")
	env, err = BaseEnvironment(nil)
	if err != nil || env["UV_DEFAULT_INDEX"] != "https://explicit.example/simple" {
		t.Fatalf("got %v %v", env, err)
	}
	if _, ok := env["UV_INDEX_URL"]; ok {
		t.Fatal("pip.conf bridged although uv names an index")
	}
}

// A pip.conf that is not UTF-8 fails the base environment as Python's decode error does
func TestEnvironmentPipConfDecodeErrorPropagates(t *testing.T) {
	root := peCleanIndexEnv(t)
	conf := filepath.Join(root, "pip.conf")
	writeFile(t, conf, []byte("[global]\nindex-url = \xff\n"), 0o666)
	t.Setenv("PIP_CONFIG_FILE", conf)
	if _, err := BaseEnvironment(nil); err == nil || !strings.Contains(err.Error(), "codec can't decode") {
		t.Fatalf("got %v", err)
	}
	if env, err := BaseEnvironment(map[string]string{"PIP_CONFIG_FILE": conf, "PYTHONPATH": "x", "VIRTUAL_ENV": "v", "UV_PYTHON": "p", "UV_INDEX_URL": "i"}); err != nil ||
		!reflect.DeepEqual(env, map[string]string{"PIP_CONFIG_FILE": conf, "UV_INDEX_URL": "i"}) {
		t.Fatalf("explicit env %v %v", env, err)
	}
}

// Swaps a package variable for one test
func peSwap[T any](t *testing.T, target *T, value T) {
	t.Helper()
	original := *target
	*target = value
	t.Cleanup(func() { *target = original })
}

// Environment over placeholder tools inside a test directory
func peFakeEnvironment(root string, output *bytes.Buffer) *PythonEnvironment {
	e := &PythonEnvironment{UV: filepath.Join(root, "uv"), Python: filepath.Join(root, "python"), Destination: filepath.Join(root, "venv"),
		Cache: filepath.Join(root, "cache"), Env: map[string]string{}}
	if output != nil {
		e.Output = output
	}
	return e
}

func TestEnvironmentStreamedRunsDoNotRequestUVDebugOutput(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_VERBOSE", "1")
	var commands [][]string
	var envs []map[string]string
	peSwap(t, &uvStreamRunner, func(ctx context.Context, command []string, cwd string, env map[string]string, timeout time.Duration, write func(string)) (uvResult, error) {
		commands, envs = append(commands, command), append(envs, env)
		return uvResult{}, nil
	})
	if _, err := peFakeEnvironment(root, &bytes.Buffer{}).run(context.Background(), []string{"sync"}, root, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || !containsString(commands[0], "--verbose") || envs[0]["RUST_LOG"] != "uv_build_frontend=debug" {
		t.Fatalf("commands %v env %v", commands, envs)
	}
}

func TestEnvironmentUVTimeoutNamesTheMirrorKnobs(t *testing.T) {
	root := t.TempDir()
	peSwap(t, &uvCaptureRunner, func(ctx context.Context, command []string, cwd string, env map[string]string, timeout time.Duration) (uvResult, error) {
		return uvResult{}, &uvTimeoutError{Argv: command, Timeout: timeout}
	})
	_, err := peFakeEnvironment(root, nil).run(context.Background(), []string{"sync"}, root, 7*time.Second)
	var ie *InstallError
	if !errors.As(err, &ie) || !strings.Contains(err.Error(), "UV_INDEX_URL") || !strings.Contains(err.Error(), "timed out after 7s") {
		t.Fatalf("got %v", err)
	}
}

// Records the environment of every captured uv spawn and reports success
func peRecordUVEnvs(t *testing.T) *[]map[string]string {
	t.Helper()
	seen := &[]map[string]string{}
	peSwap(t, &uvCaptureRunner, func(ctx context.Context, command []string, cwd string, env map[string]string, timeout time.Duration) (uvResult, error) {
		*seen = append(*seen, env)
		return uvResult{}, nil
	})
	return seen
}

func TestEnvironmentBothInstallTiersReceiveTheCMakePolicyFloor(t *testing.T) {
	root := t.TempDir()
	seen := peRecordUVEnvs(t)
	ctx := context.Background()
	e := &PythonEnvironment{UV: filepath.Join(root, "uv"), Python: peHostPython(t), Destination: filepath.Join(root, "venv"),
		Cache: filepath.Join(root, "cache"), Env: map[string]string{}}
	peWrite(t, filepath.Join(root, "pyproject.toml"), "[project]\nname = \"x\"\n")
	if err := e.Sync(ctx, root, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := e.InstallRequirements(ctx, []string{"python-olm==3.2.16"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 2 || (*seen)[0]["CMAKE_POLICY_VERSION_MINIMUM"] != "3.5" || (*seen)[1]["CMAKE_POLICY_VERSION_MINIMUM"] != "3.5" {
		t.Fatalf("envs %v", *seen)
	}
}

func TestEnvironmentAmbientCMakePolicyVersionIsNotOverwritten(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CMAKE_POLICY_VERSION_MINIMUM", "3.10")
	peToolchain(t, filepath.Join(root, "uv"), peHostPython(t))
	seen := peRecordUVEnvs(t)
	ctx := context.Background()
	e, err := ManagedEnvironment(ctx, filepath.Join(root, "candidate"), ManagedEnvironmentOptions{Cache: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(ctx, root, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if (*seen)[0]["CMAKE_POLICY_VERSION_MINIMUM"] != "3.10" {
		t.Fatalf("env %v", (*seen)[0])
	}
}

func TestEnvironmentStreamedSyncRetriesWithoutBytecodeCompileAndReportsNoFailure(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_VERBOSE", "0")
	fake := filepath.Join(root, "fake_uv.py")
	peWrite(t, fake, "import sys\nif '--compile-bytecode' in sys.argv:\n    print('error: Failed to bytecode-compile Python file in: venv')\n    print('N' * 4000)\n    raise SystemExit(1)\n")
	python := peHostPython(t)
	var calls [][]string
	peSwap(t, &uvStreamRunner, func(ctx context.Context, command []string, cwd string, env map[string]string, timeout time.Duration, write func(string)) (uvResult, error) {
		calls = append(calls, command[1:])
		return runStreaming(ctx, append([]string{python, fake}, command[1:]...), cwd, env, timeout, write)
	})
	notes := &bytes.Buffer{}
	if err := peFakeEnvironment(root, notes).Sync(context.Background(), root, SyncOptions{Extras: []string{"web"}}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || !containsString(calls[0], "--compile-bytecode") {
		t.Fatalf("calls %v", calls)
	}
	var fallback []string
	for _, a := range calls[0] {
		if a != "--compile-bytecode" {
			fallback = append(fallback, a)
		}
	}
	if !reflect.DeepEqual(calls[1], fallback) || strings.Contains(notes.String(), "✗") {
		t.Fatalf("retry %v notes %q", calls[1], notes.String())
	}
}

func TestEnvironmentSyncDoesNotRetryAnUnrelatedUVFailure(t *testing.T) {
	root := t.TempDir()
	var calls [][]string
	peSwap(t, &uvRunHook, func(e *PythonEnvironment, ctx context.Context, args []string, cwd string, timeout time.Duration) (uvResult, error) {
		calls = append(calls, args)
		return uvResult{Code: 1, Stderr: "error: No solution found for the requested dependencies"}, nil
	})
	err := peFakeEnvironment(root, nil).Sync(context.Background(), root, SyncOptions{})
	var ie *InstallError
	if !errors.As(err, &ie) || len(calls) != 1 || !containsString(calls[0], "--compile-bytecode") {
		t.Fatalf("err %v calls %v", err, calls)
	}
}

func TestEnvironmentOtherFailuresAreNotCacheCorruption(t *testing.T) {
	for _, output := range []string{
		"error: No solution found when resolving dependencies:\n  Because alpha==2 was not found ...",
		"error: Failed to fetch: `https://pypi.org/simple/alpha/`\n  Caused by: Request failed after 3 retries",
		"error: Failed to install: alpha-1.0-py3-none-any.whl (alpha==1.0)\n  cause: failed to hardlink file from /c/archive-v0/abc/alpha/__init__.py to /v/lib/alpha/__init__.py: No space left on device (os error 28)",
		"error: Failed to install: alpha-1.0-py3-none-any.whl (alpha==1.0)\n  cause: failed to create directory `/v/lib/alpha`: Permission denied (os error 13)",
		"  Caused by: The wheel is invalid: Invalid Wheel-Version",
		"error: Failed to install: --force-1.0-py3-none-any.whl (x==1)\n  cause: The wheel is invalid: x",
	} {
		if got := FailedCachedPackages(output); len(got) != 0 {
			t.Errorf("%q: %v", output, got)
		}
	}
}

// Fills a file with NUL bytes of its own size the way a crash leaves it
func peNulFill(t *testing.T, path string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, max(st.Size(), 1)), 0o666); err != nil {
		t.Fatal(err)
	}
}

func TestEnvironmentSyncRecoversFromNulFilledCacheEntries(t *testing.T) {
	root := t.TempDir()
	uv, python := peUV(t), peHostPython(t)
	source, wheels := filepath.Join(root, "source"), filepath.Join(root, "wheels")
	for _, name := range []string{"alpha_dep", "beta_dep"} {
		peWheel(t, wheels, name, "", nil, nil)
	}
	wheelsJSON, _ := json.Marshal(filepath.ToSlash(wheels))
	peWrite(t, filepath.Join(source, "pyproject.toml"), "[project]\nname=\"cache-root\"\nversion=\"1\"\nrequires-python=\">=3.11\"\n"+
		"dependencies=[\"alpha-dep==1.0\", \"beta-dep==1.0\"]\n[tool.uv]\npackage=false\nno-index=true\nfind-links=["+string(wheelsJSON)+"]\n")
	env := peIsolatedEnv(t, root)
	env["XDG_CONFIG_HOME"], env["XDG_CONFIG_DIRS"] = env["HOME"], env["HOME"]
	cache := filepath.Join(root, "cache")
	environment := func(name string, output *bytes.Buffer) *PythonEnvironment {
		e := &PythonEnvironment{UV: uv, Python: python, Destination: filepath.Join(root, name, "venv"), Cache: cache, Env: env, Offline: true}
		if output != nil {
			e.Output = output
		}
		return e
	}
	ctx := context.Background()
	first := environment("first", nil)
	if err := first.Lock(ctx, source, false, 120*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := first.Create(ctx); err != nil {
		t.Fatal(err)
	}
	if err := first.Sync(ctx, source, SyncOptions{Timeout: 120 * time.Second}); err != nil {
		t.Fatal(err)
	}
	cached := func(name string) string {
		matches, _ := filepath.Glob(filepath.Join(cache, "archive-v0", "*", name+"-1.0.dist-info"))
		if len(matches) != 1 {
			t.Fatalf("cached %s: %v", name, matches)
		}
		return matches[0]
	}
	peNulFill(t, filepath.Join(cached("alpha_dep"), "WHEEL"))
	parts, _ := os.ReadDir(cached("beta_dep"))
	betaDir := cached("beta_dep")
	for _, part := range parts {
		peNulFill(t, filepath.Join(betaDir, part.Name()))
	}
	if got := CorruptCachedPackages(cache); !reflect.DeepEqual(got, map[string]bool{"alpha-dep": true, "beta-dep": true}) {
		t.Fatalf("corrupt %v", got)
	}
	var calls [][]string
	peSwap(t, &uvRunHook, func(e *PythonEnvironment, ctx context.Context, args []string, cwd string, timeout time.Duration) (uvResult, error) {
		calls = append(calls, args)
		return e.runUV(ctx, args, cwd, timeout)
	})
	notes := &bytes.Buffer{}
	second := environment("second", notes)
	if err := second.Create(ctx); err != nil {
		t.Fatal(err)
	}
	if err := second.Sync(ctx, source, SyncOptions{Timeout: 120 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if got := peRun(t, []string{second.Executable(), "-I", "-c", "import alpha_dep, beta_dep; print(alpha_dep.__version__)"}, root, env); got != "1.0" {
		t.Fatalf("installed %q", got)
	}
	cleans, syncs := 0, 0
	var cleaned []string
	for _, c := range calls {
		if len(c) >= 2 && c[0] == "cache" && c[1] == "clean" {
			cleans++
			cleaned = append([]string{}, c[2:]...)
		}
		if len(c) >= 2 && c[0] == "cache" && c[1] == "prune" {
			t.Fatal("prune breaks PM's seeded cache")
		}
		if c[0] == "sync" {
			syncs++
		}
	}
	if cleans != 1 || syncs != 2 || !reflect.DeepEqual(sortedSet(cleaned), []string{"alpha-dep", "beta-dep"}) {
		t.Fatalf("calls %v", calls)
	}
	if got := CorruptCachedPackages(cache); len(got) != 0 {
		t.Fatalf("still corrupt %v", got)
	}
	if !strings.Contains(notes.String(), "alpha-dep, beta-dep is corrupt") {
		t.Fatalf("notes %q", notes.String())
	}
}

func TestEnvironmentRebuiltWheelIsByteIdenticalAcrossAClockTick(t *testing.T) {
	root := t.TempDir()
	first := peWheel(t, filepath.Join(root, "a"), "member_dep", "", nil, nil)
	time.Sleep(2100 * time.Millisecond)
	second := peWheel(t, filepath.Join(root, "b"), "member_dep", "", nil, nil)
	if !bytes.Equal(readFile(t, first), readFile(t, second)) {
		t.Fatal("rebuilt wheel bytes differ")
	}
}
