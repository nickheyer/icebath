package pm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func TestEnvironmentBuildPruneSitePthKeepsOnlyLoadBearingPth(t *testing.T) {
	root := t.TempDir()
	win := filepath.Join(root, "win-venv")
	winSite := filepath.Join(win, "Lib", "site-packages")
	posix := filepath.Join(root, "posix-venv")
	posixSite := filepath.Join(posix, "lib", "python3.14", "site-packages")
	os.MkdirAll(filepath.Join(win, "Scripts"), 0o777)
	os.MkdirAll(filepath.Join(posix, "bin"), 0o777)
	for _, site := range []string{winSite, posixSite} {
		peWrite(t, filepath.Join(site, "pywin32.pth"), "win32\nwin32\\lib\nimport pywin32_bootstrap\n")
		peWrite(t, filepath.Join(site, "_virtualenv.pth"), "import _virtualenv\n")
		peWrite(t, filepath.Join(site, "__editable__.icebath_agent-0.21.1.pth"), "import __editable___icebath_agent_0_21_1_finder\n")
	}
	PruneSitePth(win)
	PruneSitePth(posix)
	for _, site := range []string{winSite, posixSite} {
		if got := globUnder(site, "*.pth"); len(got) != 1 || filepath.Base(got[0]) != "pywin32.pth" {
			t.Fatalf("%s kept %v", site, got)
		}
	}
}

// A locked offline project with a workspace member and two extras
func peLockedProject(t *testing.T, root string) (string, string, map[string]string) {
	t.Helper()
	uv, python := peUV(t), peHostPython(t)
	source, wheels := filepath.Join(root, "source with spaces"), filepath.Join(root, "wheels")
	for _, name := range []string{"base_dep", "member_dep", "chosen_dep", "other_dep"} {
		peWheel(t, wheels, name, "", nil, nil)
	}
	wheelsJSON, _ := json.Marshal(filepath.ToSlash(wheels))
	peWrite(t, filepath.Join(source, "pyproject.toml"), "[project]\nname=\"construction-root\"\nversion=\"1\"\nrequires-python=\">=3.11\"\n"+
		"dependencies=[\"base-dep==1.0\"]\n[project.optional-dependencies]\nchosen=[\"chosen-dep==1.0\"]\nother=[\"other-dep==1.0\"]\n"+
		"[tool.uv]\npackage=false\nno-index=true\nfind-links=["+string(wheelsJSON)+"]\n[tool.uv.workspace]\nmembers=[\"member\"]\n")
	peWrite(t, filepath.Join(source, "member", "pyproject.toml"), "[project]\nname=\"plugin-member\"\nversion=\"1\"\nrequires-python=\">=3.11\"\n"+
		"dependencies=[\"member-dep==1.0\"]\n[tool.uv]\npackage=false\n")
	env := peIsolatedEnv(t, root)
	config := filepath.Join(root, "config")
	os.MkdirAll(config, 0o777)
	env["XDG_CONFIG_HOME"], env["XDG_CONFIG_DIRS"] = config, config
	env["UV_CACHE_DIR"], env["UV_PYTHON"], env["UV_OFFLINE"] = filepath.Join(root, "cache"), python, "1"
	peRun(t, []string{uv, "lock", "--python", python}, source, env)
	return source, uv, env
}

const peLocalBackend = `
from pathlib import Path
from zipfile import ZipFile

def build_wheel(wheel_directory, config_settings=None, metadata_directory=None):
    Path("root_app.py").read_text()
    name = "construction_root-1-py3-none-any.whl"
    dist = "construction_root-1.dist-info"
    entries = {
        "construction_root.pth": str(Path.cwd()) + "\n",
        "__editable__.construction_root-1.pth": "# build machine path\n",
        dist + "/METADATA": "Metadata-Version: 2.1\nName: construction-root\nVersion: 1\nRequires-Dist: base-dep==1.0\n",
        dist + "/WHEEL": "Wheel-Version: 1.0\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
    }
    entries[dist + "/RECORD"] = "".join(path + ",,\n" for path in entries)
    with ZipFile(Path(wheel_directory) / name, "w") as wheel:
        for path, body in entries.items():
            wheel.writestr(path, body)
    return name

build_editable = build_wheel
`

// The locked project made installable through a local build backend
func peInstallableProject(t *testing.T, root string) (string, string, map[string]string) {
	t.Helper()
	source, uv, env := peLockedProject(t, root)
	manifest := filepath.Join(source, "pyproject.toml")
	peWrite(t, manifest, strings.Replace(peRead(t, manifest), "package=false", "package=true", 1)+
		"\n[build-system]\nrequires=[]\nbuild-backend=\"local_backend\"\nbackend-path=[\".\"]\n")
	peWrite(t, filepath.Join(source, "root_app.py"), "VALUE = 'installed from the explicit source'\n")
	peWrite(t, filepath.Join(source, "local_backend.py"), peLocalBackend)
	peRun(t, []string{uv, "lock", "--python", peHostPython(t)}, source, env)
	peToolchain(t, uv, peHostPython(t))
	return source, uv, env
}

// Site packages directory of a POSIX or Windows venv
func peSitePackages(t *testing.T, venv string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return filepath.Join(venv, "Lib", "site-packages")
	}
	matches, _ := filepath.Glob(filepath.Join(venv, "lib", "python*", "site-packages"))
	if len(matches) != 1 {
		t.Fatalf("site packages of %s: %v", venv, matches)
	}
	return matches[0]
}

func TestEnvironmentBuildPublicBuildInstallsAllExtrasAtExplicitDestination(t *testing.T) {
	for _, sealed := range []bool{false, true} {
		t.Run(fmt.Sprint(sealed), func(t *testing.T) {
			isolateHome(t)
			root := t.TempDir()
			source, _, env := peInstallableProject(t, root)
			peSwap(t, &repoRoot, func() string { return filepath.Join(root, "unrelated-project") })
			t.Setenv("UV_PYTHON", "/not-the-interpreter")
			t.Setenv("UV_PROJECT_ENVIRONMENT", filepath.Join(root, "wrong-environment"))
			t.Setenv("VIRTUAL_ENV", filepath.Join(root, "wrong-environment"))
			before := os.Environ()
			locked := peRead(t, filepath.Join(source, "uv.lock"))
			executable, err := BuildEnvironment(context.Background(), BuildEnvironmentOptions{Explicit: true, Source: source,
				Python: peHostPython(t), Out: filepath.Join(root, "native environment"), Cache: filepath.Join(root, "cache"), Env: env,
				AllExtras: true, Offline: true, Sealed: sealed, Frozen: true})
			if err != nil {
				t.Fatal(err)
			}
			venv := filepath.Dir(filepath.Dir(executable))
			site := peSitePackages(t, venv)
			compiled, _ := filepath.Glob(filepath.Join(site, "*", "__pycache__", "*.pyc"))
			if len(compiled) == 0 {
				t.Fatal("dependencies were installed without bytecode")
			}
			if got := peRun(t, []string{executable, "-I", "-c", "import root_app, member_dep, chosen_dep, other_dep; print(root_app.VALUE)"}, root, env); got != "installed from the explicit source" {
				t.Fatalf("import %q", got)
			}
			if venv != filepath.Join(root, "native environment") || existsOrLink(filepath.Join(root, "wrong-environment")) {
				t.Fatalf("destination %s", venv)
			}
			if existsOrLink(filepath.Join(site, "__editable__.construction_root-1.pth")) == sealed {
				t.Fatalf("editable pointer present %v while sealed %v", !sealed, sealed)
			}
			if !isRegularFile(filepath.Join(site, "construction_root.pth")) {
				t.Fatal("load-bearing .pth must survive sealing")
			}
			if peRead(t, filepath.Join(source, "uv.lock")) != locked || !reflect.DeepEqual(os.Environ(), before) || existsOrLink(env["ICEBATH_HOME"]) {
				t.Fatal("build touched the lock, the process environment or the home")
			}
		})
	}
}

func TestEnvironmentBuildPublicDependencyOnlyBuildNeedsNoApplicationSource(t *testing.T) {
	for i, c := range []struct {
		extras    []string
		allExtras bool
		expected  [2]bool
	}{{[]string{"chosen", "chosen"}, false, [2]bool{true, false}}, {nil, true, [2]bool{true, true}}, {nil, false, [2]bool{false, false}}} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			isolateHome(t)
			root := t.TempDir()
			source, _, env := peInstallableProject(t, root)
			os.Remove(filepath.Join(source, "root_app.py"))
			os.Remove(filepath.Join(source, "local_backend.py"))
			locked := peRead(t, filepath.Join(source, "uv.lock"))
			executable, err := BuildEnvironment(context.Background(), BuildEnvironmentOptions{Explicit: true, Source: source,
				Python: peHostPython(t), Out: filepath.Join(root, "docker env"), Cache: filepath.Join(root, "cache"), Env: env,
				NoInstallProject: true, Offline: true, Extras: c.extras, AllExtras: c.allExtras, Frozen: true})
			if err != nil {
				t.Fatal(err)
			}
			out := peRun(t, []string{executable, "-I", "-c", "import json, importlib.util, importlib.metadata, base_dep; " +
				"print(json.dumps([importlib.util.find_spec('chosen_dep') is not None, importlib.util.find_spec('other_dep') is not None, " +
				"'construction-root' in [d.metadata['Name'] for d in importlib.metadata.distributions()]]))"}, root, env)
			var got []bool
			json.Unmarshal([]byte(out), &got)
			if !reflect.DeepEqual(got, []bool{c.expected[0], c.expected[1], false}) {
				t.Fatalf("got %v", got)
			}
			if peRead(t, filepath.Join(source, "uv.lock")) != locked || existsOrLink(env["ICEBATH_HOME"]) {
				t.Fatal("build touched the lock or the home")
			}
		})
	}
}

// Log that touches a release file once every marker has been written
type peAcknowledgingLog struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	markers []string
	release string
}

func (l *peAcknowledgingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, _ := l.buf.Write(p)
	all := true
	for _, m := range l.markers {
		all = all && strings.Contains(l.buf.String(), m)
	}
	if all {
		os.WriteFile(l.release, nil, 0o666)
	}
	return n, nil
}

func (l *peAcknowledgingLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func TestEnvironmentBuildBuildBackendOutputIsStreamedBeforeBuildFinishes(t *testing.T) {
	for _, operation := range []string{"sync", "requirements"} {
		t.Run(operation, func(t *testing.T) {
			isolateHome(t)
			t.Setenv("ICEBATH_VERBOSE", "1")
			root := t.TempDir()
			source, uv, env := peInstallableProject(t, root)
			release := filepath.Join(root, "release-build")
			stdoutMarker, stderrMarker := "construction-root: backend stdout", "construction-root: backend stderr"
			backend := filepath.Join(source, "local_backend.py")
			peWrite(t, backend, peRead(t, backend)+fmt.Sprintf(`
import sys
import time

original_build = build_wheel

def build_wheel(wheel_directory, config_settings=None, metadata_directory=None):
    print(%s, flush=True)
    print(%s, file=sys.stderr, flush=True)
    release = Path(%s)
    deadline = time.monotonic() + 10
    while not release.exists() and time.monotonic() < deadline:
        time.sleep(.01)
    assert release.exists(), "backend output was hidden until build exit"
    return original_build(wheel_directory, config_settings, metadata_directory)

build_editable = build_wheel
`, pyrepr.StrRepr(stdoutMarker), pyrepr.StrRepr(stderrMarker), pyrepr.StrRepr(release)))
			output := &peAcknowledgingLog{markers: []string{stdoutMarker, stderrMarker}, release: release}
			childEnv := map[string]string{}
			for k, v := range env {
				childEnv[k] = v
			}
			childEnv["UV_NO_INDEX"], childEnv["UV_FIND_LINKS"] = "1", filepath.Join(root, "wheels")
			e := &PythonEnvironment{UV: uv, Python: peHostPython(t), Destination: filepath.Join(root, "built"),
				Cache: filepath.Join(root, "build-cache"), Offline: true, Output: output, Env: childEnv}
			ctx := context.Background()
			if err := e.Create(ctx); err != nil {
				t.Fatal(err)
			}
			var err error
			if operation == "sync" {
				err = e.Sync(ctx, source, SyncOptions{Timeout: 30 * time.Second})
			} else {
				err = e.InstallRequirements(ctx, []string{"file://" + filepath.ToSlash(source)}, "", "")
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Check(ctx, ""); err != nil {
				t.Fatal(err)
			}
			if !isRegularFile(release) {
				t.Fatal("both backend streams must arrive during the build")
			}
			if got := peRun(t, []string{e.Executable(), "-I", "-c", "import root_app; print(root_app.VALUE)"}, root, env); got != "installed from the explicit source" {
				t.Fatalf("import %q", got)
			}
		})
	}
}

func TestEnvironmentBuildStreamingBoundsMemoryWithoutLosingFailureClass(t *testing.T) {
	python := peHostPython(t)
	for _, diagnostic := range []string{"No solution found", "Connection timed out", "Failed to build wheel", "The build backend returned an error"} {
		t.Run(diagnostic, func(t *testing.T) {
			root := t.TempDir()
			script := fmt.Sprintf("import os, time; os.write(2, %s); time.sleep(.15); os.write(2, %s); "+
				"[os.write(2, b'x' * 65536) for _ in range(128)]; os.write(2, b'final diagnostic'); raise SystemExit(1)",
				"b"+pyrepr.StrRepr(diagnostic[:4]), "b"+pyrepr.StrRepr(diagnostic[4:]))
			e := &PythonEnvironment{UV: python, Python: python, Destination: filepath.Join(root, "venv"), Cache: filepath.Join(root, "cache"),
				Env: pyEnviron(), Output: io.Discard}
			previous := debug.SetGCPercent(1)
			defer debug.SetGCPercent(previous)
			runtime.GC()
			var base runtime.MemStats
			runtime.ReadMemStats(&base)
			var peak atomic.Uint64
			done := make(chan struct{})
			go func() {
				var m runtime.MemStats
				for {
					select {
					case <-done:
						return
					case <-time.After(2 * time.Millisecond):
						runtime.ReadMemStats(&m)
						if m.HeapAlloc > peak.Load() {
							peak.Store(m.HeapAlloc)
						}
					}
				}
			}()
			result, err := e.run(context.Background(), []string{"-c", script}, root, 30*time.Second)
			close(done)
			if err != nil {
				t.Fatal(err)
			}
			if result.Code != 1 || len([]rune(result.Stderr)) > 2000 {
				t.Fatalf("code %d tail %d", result.Code, len([]rune(result.Stderr)))
			}
			if grown := int64(peak.Load()) - int64(base.HeapAlloc); grown > 2*1024*1024 {
				t.Fatalf("stream capture retained %d bytes", grown)
			}
			actual := ClassifyUVFailure("sync", result.Code, result.Stderr)
			if reflect.TypeOf(actual) != reflect.TypeOf(ClassifyUVFailure("sync", 1, diagnostic)) || !strings.Contains(actual.Error(), "final diagnostic") {
				t.Fatalf("classified %T %v", actual, actual)
			}
		})
	}
}

func TestEnvironmentBuildChildOutputIsLiveAndKeepsExplicitIndexCredentials(t *testing.T) {
	t.Setenv("ICEBATH_VERBOSE", "1")
	root := t.TempDir()
	python := peHostPython(t)
	released := filepath.Join(root, "release-child")
	output := &peAcknowledgingLog{markers: []string{"child-started"}, release: released}
	childEnv := pyEnviron()
	for k, v := range map[string]string{"UV_INDEX_PRIVATE_USERNAME": "fixture-user", "UV_INDEX_PRIVATE_PASSWORD": "fixture-secret",
		"UV_DEFAULT_INDEX": "https://fixture.invalid/simple", "UV_PROJECT": "/poison/project", "UV_VENV_SEED": "true",
		"UV_SYSTEM_PYTHON": "true", "UV_PYTHON": "/poison/python", "UV_CACHE_DIR": "/poison/cache"} {
		childEnv[k] = v
	}
	cache := filepath.Join(root, "cache")
	e := &PythonEnvironment{UV: python, Python: python, Destination: filepath.Join(root, "venv"), Cache: cache, Env: childEnv, Output: output}
	script := "import os, time; from pathlib import Path; print('child-started', flush=True); " +
		"release = Path(" + pyrepr.StrRepr(released) + "); deadline = time.monotonic() + 5\n" +
		"while not release.exists() and time.monotonic() < deadline: time.sleep(.01)\n" +
		"assert release.exists(), 'log was hidden until process exit'\n" +
		"assert os.environ['UV_INDEX_PRIVATE_USERNAME'] == 'fixture-user'\n" +
		"assert os.environ['UV_INDEX_PRIVATE_PASSWORD'] == 'fixture-secret'\n" +
		"assert os.environ['UV_DEFAULT_INDEX'] == 'https://fixture.invalid/simple'\n" +
		"assert not {'UV_PROJECT', 'UV_VENV_SEED', 'UV_SYSTEM_PYTHON'} & os.environ.keys()\n" +
		"assert os.environ['UV_PYTHON'] == " + pyrepr.StrRepr(python) + "\n" +
		"assert os.environ['UV_CACHE_DIR'] == " + pyrepr.StrRepr(cache) + "\n" +
		"print('child-complete', flush=True)\n"
	result, err := e.run(context.Background(), []string{"-c", script}, root, 10*time.Second)
	if err != nil || result.Code != 0 {
		t.Fatalf("%v %d %s", err, result.Code, result.Stderr)
	}
	if !strings.Contains(output.String(), "child-started") || !strings.Contains(output.String(), "child-complete") || !strings.Contains(result.Stderr, "child-complete") {
		t.Fatalf("output %q tail %q", output.String(), result.Stderr)
	}
}

// Streaming runner of the environment engine or the update command's live runner, with its timeout error check
func peStreamingRunners(t *testing.T, root string) map[string]func(script string, timeout time.Duration) (*bytes.Buffer, error) {
	python := peHostPython(t)
	return map[string]func(string, time.Duration) (*bytes.Buffer, error){
		"environment": func(script string, timeout time.Duration) (*bytes.Buffer, error) {
			output := &bytes.Buffer{}
			e := &PythonEnvironment{UV: python, Python: python, Destination: filepath.Join(root, "venv"), Cache: filepath.Join(root, "cache"),
				Env: pyEnviron(), Output: &syncWriter{w: output}}
			_, err := e.run(context.Background(), []string{"-c", script}, root, timeout)
			var ie *InstallError
			if !errors.As(err, &ie) {
				t.Fatalf("environment timeout raised %v", err)
			}
			return output, err
		},
		"cli": func(script string, timeout time.Duration) (*bytes.Buffer, error) {
			output := &bytes.Buffer{}
			_, _, err := runLive(context.Background(), &syncWriter{w: output}, []string{python, "-c", script}, root, pyEnviron(), timeout)
			var py *PyError
			if !errors.As(err, &py) || py.Class != "RuntimeError" {
				t.Fatalf("live runner timeout raised %v", err)
			}
			return output, err
		},
	}
}

// Writer safe for concurrent use
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func TestEnvironmentBuildStreamingDeadlineIncludesInheritedStdout(t *testing.T) {
	t.Setenv("ICEBATH_VERBOSE", "1")
	for name := range peStreamingRunners(t, t.TempDir()) {
		for _, parentExits := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s-%v", name, parentExits), func(t *testing.T) {
				root := t.TempDir()
				run := peStreamingRunners(t, root)[name]
				release, finished := filepath.Join(root, "release-descendant"), filepath.Join(root, "descendant-finished")
				descendant := "import time; from pathlib import Path; print('inherited-output', flush=True); " +
					"release = Path(" + pyrepr.StrRepr(release) + "); deadline = time.monotonic() + 10\n" +
					"while not release.exists() and time.monotonic() < deadline: time.sleep(.01)\n" +
					"Path(" + pyrepr.StrRepr(finished) + ").touch()\n"
				tail := "child.wait()"
				if parentExits {
					tail = "sys.exit(0)"
				}
				parent := "import subprocess, sys; child = subprocess.Popen([sys.executable, '-c', " + pyrepr.StrRepr(descendant) + "]); " + tail
				goroutines := runtime.NumGoroutine()
				started := time.Now()
				output, _ := run(parent, 2*time.Second)
				elapsed := time.Since(started)
				os.WriteFile(release, nil, 0o666)
				deadline := time.Now().Add(5 * time.Second)
				for !isRegularFile(finished) && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				if !isRegularFile(finished) {
					t.Fatal("descendant did not acknowledge cleanup")
				}
				if elapsed > 4*time.Second {
					t.Fatalf("draining stdout restarted the timeout: %v", elapsed)
				}
				if !strings.Contains(output.String(), "inherited-output") {
					t.Fatalf("output %q", output.String())
				}
				settle := time.Now().Add(2 * time.Second)
				for runtime.NumGoroutine() > goroutines && time.Now().Before(settle) {
					time.Sleep(10 * time.Millisecond)
				}
				if runtime.NumGoroutine() > goroutines {
					t.Fatalf("timeout leaked an output reader: %d goroutines, %d before", runtime.NumGoroutine(), goroutines)
				}
			})
		}
	}
}

func TestEnvironmentBuildStreamingEOFDoesNotRestartProcessWaitTimeout(t *testing.T) {
	t.Setenv("ICEBATH_VERBOSE", "1")
	for name := range peStreamingRunners(t, t.TempDir()) {
		t.Run(name, func(t *testing.T) {
			run := peStreamingRunners(t, t.TempDir())[name]
			script := "import os, time; print('before-eof', flush=True); time.sleep(3); os.close(1); os.close(2); time.sleep(10)"
			started := time.Now()
			output, _ := run(script, 4*time.Second)
			if time.Since(started) > 6*time.Second {
				t.Fatalf("EOF restarted the process wait budget: %v", time.Since(started))
			}
			if !strings.Contains(output.String(), "before-eof") {
				t.Fatalf("output %q", output.String())
			}
		})
	}
}

func TestEnvironmentBuildFailedBuildRemovesOnlyItsCandidate(t *testing.T) {
	for _, damage := range []string{"source", "check", "lock"} {
		t.Run(damage, func(t *testing.T) {
			isolateHome(t)
			root := t.TempDir()
			source, _, env := peInstallableProject(t, root)
			python := peHostPython(t)
			ctx := context.Background()
			previous := filepath.Join(root, "previous")
			build := func(out, cache string) (string, error) {
				return BuildEnvironment(ctx, BuildEnvironmentOptions{Explicit: true, Source: source, Python: python, Out: out, Env: env,
					Cache: cache, Offline: true, Frozen: true})
			}
			executable, err := build(previous, filepath.Join(root, "cache"))
			if err != nil {
				t.Fatal(err)
			}
			cfg := peRead(t, filepath.Join(previous, "pyvenv.cfg"))
			sourceLock := peRead(t, filepath.Join(source, "uv.lock"))
			if _, err := build(previous, filepath.Join(root, "cache")); !errors.Is(err, fs.ErrExist) || !strings.Contains(err.Error(), "already exists") {
				t.Fatalf("existing destination: %v", err)
			}
			switch damage {
			case "source":
				os.Remove(filepath.Join(source, "root_app.py"))
			case "check":
				backend := filepath.Join(source, "local_backend.py")
				peWrite(t, backend, strings.Replace(peRead(t, backend), "base-dep==1.0", "base-dep==2.0", 1))
				os.RemoveAll(filepath.Join(source, "__pycache__"))
			default:
				os.Remove(filepath.Join(source, "uv.lock"))
			}
			candidate := filepath.Join(root, "candidate")
			_, err = build(candidate, filepath.Join(root, "cold-cache"))
			var ie *InstallError
			want := "uv sync"
			if damage == "check" {
				want = "dependency validation"
			}
			if !errors.As(err, &ie) || !(strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "frozen build requires a lock")) {
				t.Fatalf("damaged build: %v", err)
			}
			if existsOrLink(candidate) || peRead(t, filepath.Join(previous, "pyvenv.cfg")) != cfg {
				t.Fatal("failed build touched more than its candidate")
			}
			if got := peRun(t, []string{executable, "-I", "-c", "import base_dep; print(base_dep.__version__)"}, root, env); got != "1.0" {
				t.Fatalf("previous %q", got)
			}
			if damage != "lock" && peRead(t, filepath.Join(source, "uv.lock")) != sourceLock {
				t.Fatal("source lock changed")
			}
			if damage == "lock" && existsOrLink(filepath.Join(source, "uv.lock")) {
				t.Fatal("frozen builds must not manufacture a lock")
			}
		})
	}
}

func TestEnvironmentBuildLockUpgradeAndGroupSelectionUseTheSameEnvironment(t *testing.T) {
	root := t.TempDir()
	source, uv, env := peLockedProject(t, root)
	manifest := filepath.Join(source, "pyproject.toml")
	peWrite(t, manifest, strings.Replace(peRead(t, manifest), "base-dep==1.0", "base-dep>=1,<2", 1)+"\n[dependency-groups]\nqa=[\"other-dep==1.0\"]\n")
	e := &PythonEnvironment{UV: uv, Python: peHostPython(t), Destination: filepath.Join(root, "candidate"), Cache: filepath.Join(root, "cache"), Env: env, Offline: true}
	ctx := context.Background()
	if err := e.Lock(ctx, source, false, 60*time.Second); err != nil {
		t.Fatal(err)
	}
	peWheel(t, filepath.Join(root, "wheels"), "base_dep", "1.1", nil, nil)
	if err := e.Lock(ctx, source, false, 60*time.Second); err != nil {
		t.Fatal(err)
	}
	baseVersion := func() string {
		doc, err := tomlLoads(peRead(t, filepath.Join(source, "uv.lock")))
		if err != nil {
			t.Fatal(err)
		}
		rows, _ := doc.Get("package")
		for _, row := range rows.([]any) {
			if obj := row.(*OrderedObject); obj.Values["name"] == "base-dep" {
				return obj.Values["version"].(string)
			}
		}
		return ""
	}
	if got := baseVersion(); got != "1.0" {
		t.Fatalf("plain lock moved base-dep to %s", got)
	}
	if err := e.Lock(ctx, source, true, 60*time.Second); err != nil {
		t.Fatal(err)
	}
	locked := peRead(t, filepath.Join(source, "uv.lock"))
	if err := e.Create(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(ctx, source, SyncOptions{Groups: []string{"qa", "qa"}, Timeout: 60 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if got := peRun(t, []string{e.Executable(), "-I", "-c", "import base_dep, other_dep; print(base_dep.__version__, other_dep.__version__)"}, root, env); got != "1.1 1.0" {
		t.Fatalf("installed %q", got)
	}
	if peRead(t, filepath.Join(source, "uv.lock")) != locked {
		t.Fatal("frozen sync rewrote the lock")
	}
}

func TestEnvironmentBuildExplicitEnvironmentInstallsLockedMembersWithoutLiveSelection(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	source, uv, env := peLockedProject(t, root)
	beforeLock := peRead(t, filepath.Join(source, "uv.lock"))
	beforeProcess := os.Environ()
	beforeEnv := map[string]string{}
	for k, v := range env {
		beforeEnv[k] = v
	}
	peSwap(t, &repoRoot, func() string { t.Fatal("implicit source lookup"); return "" })
	python := peHostPython(t)
	e := &PythonEnvironment{UV: uv, Python: python, Destination: filepath.Join(root, "candidate"), Cache: filepath.Join(root, "cache"), Env: env}
	ctx := context.Background()
	if err := e.Create(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Sync(ctx, source, SyncOptions{Extras: []string{"chosen"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Check(ctx, ""); err != nil {
		t.Fatal(err)
	}
	out := peRun(t, []string{e.Executable(), "-I", "-c", "import sys, json, base_dep, member_dep, chosen_dep, importlib.util; " +
		"print(json.dumps([sys.base_prefix, member_dep.__version__, importlib.util.find_spec('other_dep') is None]))"}, root, env)
	basePrefix := peRun(t, []string{python, "-I", "-c", "import sys; print(sys.base_prefix)"}, root, env)
	var got []any
	json.Unmarshal([]byte(out), &got)
	if !reflect.DeepEqual(got, []any{basePrefix, "1.0", true}) {
		t.Fatalf("got %v", got)
	}
	if peRead(t, filepath.Join(source, "uv.lock")) != beforeLock || existsOrLink(filepath.Join(source, ".venv")) || existsOrLink(env["ICEBATH_HOME"]) ||
		!reflect.DeepEqual(env, beforeEnv) || !reflect.DeepEqual(os.Environ(), beforeProcess) {
		t.Fatal("explicit environment touched live state")
	}
}

func TestEnvironmentBuildNoConfigDropsBridgedIndexSettingsFromChildEnv(t *testing.T) {
	root := t.TempDir()
	python := peHostPython(t)
	ambient := pyEnviron()
	ambient["UV_INDEX_URL"], ambient["UV_HTTP_TIMEOUT"] = "https://fixture-mirror.invalid/simple", "99"
	child := "import os, sys\nif 'UV_INDEX_URL' in os.environ:\n    sys.exit('leaked index redirect: UV_INDEX_URL')\n" +
		"if 'UV_HTTP_TIMEOUT' not in os.environ:\n    sys.exit('transport knob dropped: UV_HTTP_TIMEOUT')\n"
	run := func(noConfig bool) uvResult {
		e := &PythonEnvironment{UV: python, Python: python, Destination: filepath.Join(root, "venv"), Cache: filepath.Join(root, "cache"),
			Env: ambient, Output: &bytes.Buffer{}, NoConfig: noConfig}
		result, err := e.run(context.Background(), []string{"-c", child}, root, 60*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if isolated := run(true); isolated.Code != 0 {
		t.Fatalf("isolation wrong: %s", isolated.Stderr)
	}
	if bridged := run(false); bridged.Code != 1 || !strings.Contains(bridged.Stderr, "UV_INDEX_URL") {
		t.Fatalf("default path must keep the bridge: %d %s", bridged.Code, bridged.Stderr)
	}
}
