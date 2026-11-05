package pm

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Fixture tool whose env names its entry
type peCopyPackage struct{ BasePackage }

func (p *peCopyPackage) Env(entry, target string) EnvDiff { return EnvDiff{"COPY_ROOT": entry} }

// Every regular file under a root with its bytes
func peTreeBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			rel, _ := filepath.Rel(root, path)
			out[rel] = peRead(t, path)
		}
		return nil
	})
	return out
}

// Every path under a root
func peTreePaths(root string) []string {
	var out []string
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil {
			out = append(out, path)
		}
		return nil
	})
	return out
}

func TestBuildOperationsStageToolsCopiesVerifiedClosureWithoutAcquiringOrLiveState(t *testing.T) {
	tmp := isolateHome(t)
	t.Setenv(EnvRuntimeDir, filepath.Join(tmp, "store"))
	target, err := CurrentTarget()
	if err != nil {
		t.Fatal(err)
	}
	leaf := &peCopyPackage{BasePackage{Name: "copy-leaf"}}
	tool := &peCopyPackage{BasePackage{Name: "copy-tool", Deps: []string{"copy-leaf"}}}
	ieSwapRegistry(t, leaf, tool)
	lockPath := filepath.Join(tmp, "lock.json")
	ieEmptyLock(t, lockPath)
	peSwap(t, &lockfilePath, func() string { return lockPath })
	docroot := filepath.Join(tmp, "www")
	os.MkdirAll(docroot, 0o777)
	server := httptest.NewServer(http.FileServer(http.Dir(docroot)))
	t.Cleanup(server.Close)
	lock := NewLockfile(lockPath)
	for _, name := range []string{"copy-leaf", "copy-tool"} {
		data := makeTarGz(t, []member{{Name: "data", Data: []byte(name)}}, 0o755)
		writeFile(t, filepath.Join(docroot, name+".tgz"), data, 0o644)
		lock.SetPin(name, "1.0", map[string]any{target: map[string]any{"url": server.URL + "/" + name + ".tgz", "sha256": hexSHA256(data)}})
	}
	if err := lock.Save(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	source := filepath.Join(tmp, "canonical")
	got, err := prepareTools(ctx, PrepareToolsOptions{Names: []string{"copy-tool"}, Out: source, Target: target})
	if err != nil || got != resolvePath(source) {
		t.Fatalf("prepare %q %v", got, err)
	}
	facts, err := NewFacts(filepath.Join(source, "facts.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	before := peRead(t, filepath.Join(source, "facts.json"))
	homeBefore := peTreePaths(filepath.Join(tmp, ".icebath"))
	destination := filepath.Join(tmp, "payload-tools")
	if got, err := stageTools(ctx, StageToolsOptions{Names: []string{"copy-tool"}, SourceStore: source, Out: destination, Target: target}); err != nil || got != resolvePath(destination) {
		t.Fatalf("stage %q %v", got, err)
	}
	toolFact, _ := facts.Get("copy-tool")
	linked := filepath.Join(destination, pyStr(toolFact["entry"]), "data")
	os.Remove(linked)
	if err := os.Link(filepath.Join(source, pyStr(toolFact["entry"]), "data"), linked); err != nil {
		t.Fatal(err)
	}
	if _, err := stageTools(ctx, StageToolsOptions{Names: []string{"copy-tool"}, SourceStore: source, Out: destination, Target: target}); err != nil {
		t.Fatal(err)
	}
	staged, err := NewFacts(filepath.Join(destination, "facts.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"copy-leaf", "copy-tool"} {
		fact, _ := facts.Get(name)
		entry := pyStr(fact["entry"])
		copied, original := filepath.Join(destination, entry, "data"), filepath.Join(source, entry, "data")
		if peRead(t, copied) != peRead(t, original) {
			t.Fatalf("%s copy differs", name)
		}
		a, _ := os.Stat(copied)
		b, _ := os.Stat(original)
		if os.SameFile(a, b) {
			t.Fatalf("%s copy shares the original file", name)
		}
		peWrite(t, copied, "signed output")
		if peRead(t, original) != name {
			t.Fatalf("%s original changed", name)
		}
		stagedFact, _ := staged.Get(name)
		if stagedFact["digest"] != fact["digest"] || staged.EnvFor(name, destination)["COPY_ROOT"] != filepath.Join(destination, entry) {
			t.Fatalf("%s staged fact %v env %v", name, stagedFact, staged.EnvFor(name, destination))
		}
	}
	if peRead(t, filepath.Join(source, "facts.json")) != before || !reflect.DeepEqual(peTreePaths(filepath.Join(tmp, ".icebath")), homeBefore) ||
		existsOrLink(filepath.Join(tmp, ".icebath", "config.yaml")) || existsOrLink(filepath.Join(tmp, "store", "facts.json")) {
		t.Fatal("staging touched source facts, the home or the live store")
	}
	original := filepath.Join(source, pyStr(toolFact["entry"]), "data")
	peWrite(t, original, "corrupt")
	if _, err := stageTools(ctx, StageToolsOptions{Names: []string{"copy-tool"}, SourceStore: source, Out: destination, Target: target}); err == nil || !strings.Contains(err.Error(), "source failed verification") {
		t.Fatalf("corrupt source: %v", err)
	}
	peWrite(t, original, "copy-tool")
	lock = NewLockfile(lockPath)
	lock.SetPin("copy-tool", "1.0", map[string]any{target: map[string]any{"url": "https://invalid.test/repin", "sha256": strings.Repeat("b", 64)}})
	lock.Save()
	if _, err := stageTools(ctx, StageToolsOptions{Names: []string{"copy-tool"}, SourceStore: source, Out: destination, Target: target}); err == nil || !strings.Contains(err.Error(), "source failed verification") {
		t.Fatalf("repinned source: %v", err)
	}
	artifacts := toolFact["artifacts"].([]any)
	lock = NewLockfile(lockPath)
	lock.SetPin("copy-tool", "1.0", map[string]any{target: map[string]any{"url": "https://invalid.test/tool.tgz", "sha256": artifacts[0]}})
	lock.Save()
	external := filepath.Join(tmp, "external")
	peWrite(t, external, "shared")
	os.Remove(original)
	if err := os.Symlink(external, original); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Dir(original)
	digest, err := TreeDigest(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := facts.Record("copy-tool", "1.0", filepath.Base(entry), tool.Env(entry, target), source,
		FactRecord{Target: &target, Artifacts: []string{pyStr(artifacts[0])}, Digest: &digest}); err != nil {
		t.Fatal(err)
	}
	_, err = stageTools(ctx, StageToolsOptions{Names: []string{"copy-tool"}, SourceStore: source, Out: filepath.Join(tmp, "other-output"), Target: target})
	if err == nil || !(strings.Contains(err.Error(), "verification") || strings.Contains(err.Error(), "escapes")) {
		t.Fatalf("linked source: %v", err)
	}
}

func TestBuildOperationsVerifiedToolsAdmitsOnlyLockedEntriesWithoutExecution(t *testing.T) {
	for _, damage := range []string{"", "entry", "env", "bytes", "pin", "binary"} {
		t.Run(damage, func(t *testing.T) {
			tmp := t.TempDir()
			target, err := CurrentTarget()
			if err != nil {
				t.Fatal(err)
			}
			store := filepath.Join(tmp, "tools")
			lock := NewLockfile(filepath.Join(tmp, "lock.json"))
			p, err := GetPackage("python")
			if err != nil {
				t.Fatal(err)
			}
			entry := filepath.Join(store, p.StoreEntry("1.0", target))
			binary := p.Binary(entry, target)
			if binary == "" {
				t.Fatal("python has no binary")
			}
			writeFile(t, binary, []byte("already verified at publication"), 0o755)
			lock.SetPin("python", "1.0", map[string]any{target: map[string]any{"url": "https://invalid.test/python", "sha256": strings.Repeat("a", 64)}})
			facts, err := NewFacts(filepath.Join(store, "facts.json"), false)
			if err != nil {
				t.Fatal(err)
			}
			env := p.Env(entry, target)
			switch damage {
			case "entry":
				renamed := filepath.Join(filepath.Dir(entry), "noncanonical")
				os.Rename(entry, renamed)
				entry = renamed
				env = p.Env(entry, target)
			case "env":
				env["PATH"] = []string{filepath.Join(tmp, "unverified")}
			case "binary":
				os.Remove(binary)
			}
			digest, err := TreeDigest(entry)
			if err != nil {
				t.Fatal(err)
			}
			if err := facts.Record("python", "1.0", filepath.Base(entry), env, store, FactRecord{Target: &target, Artifacts: []string{strings.Repeat("a", 64)}, Digest: &digest}); err != nil {
				t.Fatal(err)
			}
			if damage == "bytes" {
				writeFile(t, binary, []byte("changed"), 0o755)
			}
			if damage == "pin" {
				lock.SetPin("python", "1.0", map[string]any{target: map[string]any{"url": "https://invalid.test/python", "sha256": strings.Repeat("b", 64)}})
			}
			before := peTreeBytes(t, store)
			peSwap(t, &runProcess, func(processSpec) (processResult, error) {
				t.Fatal("read-only admission executed a process")
				return processResult{}, nil
			})
			selected, err := ReadVerifiedTools([]string{"python"}, store, target, lock)
			if damage != "" {
				if err == nil || !strings.Contains(err.Error(), "source failed verification") {
					t.Fatalf("damage %s admitted: %v", damage, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				python := selected.Entries["python"]
				if python.Path != entry || python.Binary != binary {
					t.Fatalf("selected %+v", python)
				}
				if got := selected.Environment(map[string]string{"PATH": "inherited"})["PATH"]; got != filepath.Dir(binary)+string(os.PathListSeparator)+"inherited" {
					t.Fatalf("PATH %q", got)
				}
			}
			if !reflect.DeepEqual(peTreeBytes(t, store), before) {
				t.Fatal("admission wrote to the store")
			}
		})
	}
}

// Build tools environment and the isolated home build operations run against
func peBuildTools(t *testing.T) (string, map[string]string) {
	t.Helper()
	tmp := isolateHome(t)
	peToolchain(t, peUV(t), peHostPython(t))
	env := map[string]string{}
	for k, v := range pyEnviron() {
		if !strings.HasPrefix(k, "UV_") && !strings.HasPrefix(k, "PYTHON") && k != "VIRTUAL_ENV" {
			env[k] = v
		}
	}
	h := filepath.Join(tmp, "home")
	os.MkdirAll(h, 0o777)
	env["HOME"], env["USERPROFILE"], env["ICEBATH_HOME"] = h, h, filepath.Join(h, ".icebath")
	t.Setenv("ICEBATH_HOME", env["ICEBATH_HOME"])
	t.Setenv(EnvDisableLazyInstalls, "")
	os.Unsetenv(EnvDisableLazyInstalls)
	return tmp, env
}

// A locked offline source with an extra carrying a marker and a dev group
func peLockedSource(t *testing.T, tmp string, env map[string]string) string {
	t.Helper()
	wheels := filepath.Join(tmp, "wheelhouse")
	for _, name := range []string{"base_dep", "chosen_dep", "dev_dep"} {
		peWheel(t, wheels, name, "", nil, nil)
	}
	source := filepath.Join(tmp, "source with spaces")
	wheelsJSON, _ := json.Marshal(filepath.ToSlash(wheels))
	peWrite(t, filepath.Join(source, "pyproject.toml"), "[project]\nname=\"export-root\"\nversion=\"1\"\nrequires-python=\">=3.11\"\n"+
		"dependencies=[\"base-dep==1.0\"]\n[project.optional-dependencies]\nchosen=[\"chosen-dep==1.0; sys_platform == 'win32'\"]\n"+
		"[dependency-groups]\ndev=[\"dev-dep==1.0\"]\n[tool.uv]\npackage=false\nno-index=true\nfind-links=["+string(wheelsJSON)+"]\n")
	if err := LockProject(context.Background(), LockProjectOptions{Source: source, Python: peHostPython(t), Cache: filepath.Join(tmp, "cache"),
		Env: env, Offline: true, Explicit: true}); err != nil {
		t.Fatal(err)
	}
	return source
}

func TestBuildOperationsCheckLockNeverChangesSource(t *testing.T) {
	for _, state := range []string{"current", "stale", "missing"} {
		t.Run(state, func(t *testing.T) {
			tmp, env := peBuildTools(t)
			source := peLockedSource(t, tmp, env)
			switch state {
			case "stale":
				manifest := filepath.Join(source, "pyproject.toml")
				peWrite(t, manifest, strings.Replace(peRead(t, manifest), `version="1"`, `version="2"`, 1))
			case "missing":
				os.Remove(filepath.Join(source, "uv.lock"))
			}
			before := peTreeBytes(t, source)
			err := CheckProjectLock(context.Background(), CheckProjectLockOptions{Source: source, Python: peHostPython(t), Cache: filepath.Join(tmp, "cache"),
				Env: env, Offline: true, Explicit: true})
			var ie *InstallError
			if state == "current" && err != nil || state != "current" && !errors.As(err, &ie) {
				t.Fatalf("check %s: %v", state, err)
			}
			if !reflect.DeepEqual(peTreeBytes(t, source), before) || existsOrLink(filepath.Join(source, ".venv")) {
				t.Fatal("check changed the source")
			}
		})
	}
}

func TestBuildOperationsFrozenExportKeepsMarkersAndExcludesBuildMetadata(t *testing.T) {
	tmp, env := peBuildTools(t)
	source := peLockedSource(t, tmp, env)
	manifest := filepath.Join(source, "pyproject.toml")
	peWrite(t, manifest, strings.Replace(peRead(t, manifest), "base-dep==1.0", "base-dep==2.0", 1))
	before := peTreeBytes(t, source)
	out := filepath.Join(tmp, "export directory", "requirements.txt")
	if err := ExportRequirements(context.Background(), ExportRequirementsOptions{Source: source, Out: out, Extras: []string{"chosen", "chosen"},
		Python: peHostPython(t), Cache: filepath.Join(tmp, "cache"), Env: env, Explicit: true}); err != nil {
		t.Fatal(err)
	}
	text := peRead(t, out)
	requirements := map[string]*pyRequirement{}
	for _, line := range pySplitLines(text) {
		r, err := ParseRequirement(line)
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		requirements[r.Name] = r
	}
	if len(requirements) != 2 || requirements["base-dep"] == nil || requirements["chosen-dep"] == nil {
		t.Fatalf("requirements %q", text)
	}
	if spec, _ := formatSpecifierSet(requirements["base-dep"].Specifier); spec != "==1.0" {
		t.Fatalf("base spec %q", spec)
	}
	marker, _ := formatMarker(requirements["chosen-dep"].Marker, true)
	if marker != `sys_platform == "win32"` {
		t.Fatalf("marker %q", marker)
	}
	if strings.Contains(text, "#") || strings.Contains(text, "--hash") {
		t.Fatalf("export carries metadata: %q", text)
	}
	if !reflect.DeepEqual(peTreeBytes(t, source), before) || existsOrLink(filepath.Join(source, ".venv")) {
		t.Fatal("export changed the source")
	}
}

func TestBuildOperationsFrozenExportPreservesGitCommitPin(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("the git pin case needs git")
	}
	tmp, env := peBuildTools(t)
	source := peLockedSource(t, tmp, env)
	dependency := filepath.Join(tmp, "git-dependency")
	peWrite(t, filepath.Join(dependency, "pyproject.toml"), "[project]\nname=\"git-dep\"\nversion=\"1.0\"\nrequires-python=\">=3.11\"\ndependencies=[]\n")
	peRun(t, []string{git, "init", "--quiet"}, dependency, env)
	peRun(t, []string{git, "add", "pyproject.toml"}, dependency, env)
	peRun(t, []string{git, "-c", "user.name=PM test", "-c", "user.email=pm@example.invalid", "-c", "commit.gpgsign=false",
		"-c", "core.hooksPath=" + filepath.Join(tmp, "empty-hooks"), "commit", "--quiet", "-m", "fixture"}, dependency, env)
	revision := peRun(t, []string{git, "rev-parse", "HEAD"}, dependency, env)
	url := "git+file://" + filepath.ToSlash(dependency) + "@" + revision
	quoted, _ := json.Marshal("git-dep @ " + url)
	manifest := filepath.Join(source, "pyproject.toml")
	peWrite(t, manifest, strings.Replace(peRead(t, manifest), `"base-dep==1.0"`, string(quoted), 1))
	ctx := context.Background()
	if err := LockProject(ctx, LockProjectOptions{Source: source, Python: peHostPython(t), Cache: filepath.Join(tmp, "cache"), Env: env, Explicit: true}); err != nil {
		t.Fatal(err)
	}
	before := peRead(t, filepath.Join(source, "uv.lock"))
	out := filepath.Join(tmp, "git-requirements.txt")
	if err := ExportRequirements(ctx, ExportRequirementsOptions{Source: source, Out: out, Cache: filepath.Join(tmp, "cache"), Env: env, Explicit: true}); err != nil {
		t.Fatal(err)
	}
	r, err := ParseRequirement(strings.TrimSpace(peRead(t, out)))
	if err != nil || r.Name != "git-dep" || r.URL != url {
		t.Fatalf("export %q %v", peRead(t, out), err)
	}
	if peRead(t, filepath.Join(source, "uv.lock")) != before {
		t.Fatal("export rewrote the lock")
	}
}

// Python base prefix of the host interpreter
func peBasePrefix(t *testing.T, env map[string]string) string {
	return peRun(t, []string{peHostPython(t), "-I", "-c", "import sys; print(sys.base_prefix)"}, t.TempDir(), env)
}

func TestBuildOperationsRequirementsBuildInstallsOfflineMarkersAndSealsOnlyBuildPth(t *testing.T) {
	for _, sealed := range []bool{false, true} {
		t.Run(fmt.Sprint(sealed), func(t *testing.T) {
			tmp, env := peBuildTools(t)
			wheels := filepath.Join(tmp, "wheels with spaces")
			peWheel(t, wheels, "leaf_dep", "", nil, nil)
			peWheel(t, wheels, "app_dep", "", []string{"leaf-dep==1.0"}, map[string]string{"__editable__.app_dep-1.0.pth": "# build machine path\n", "app_dep.pth": "\n"})
			out := filepath.Join(tmp, "destination with spaces")
			peWrite(t, filepath.Join(tmp, "uv.toml"), "required-version=\"<0.1\"\n")
			poison := filepath.Join(tmp, "poison")
			buildEnv := map[string]string{}
			for k, v := range env {
				buildEnv[k] = v
			}
			for _, k := range []string{"UV_PROJECT_ENVIRONMENT", "VIRTUAL_ENV", "UV_PYTHON", "UV_CACHE_DIR"} {
				buildEnv[k] = poison
			}
			buildEnv["UV_SYSTEM_PYTHON"] = "true"
			before := os.Environ()
			executable, err := BuildRequirementsEnvironment(context.Background(), BuildRequirementsEnvironmentOptions{
				Requirements: []string{"app-dep==1.0; python_version >= '3'", "missing-dep==1.0; python_version < '2'"}, Out: out,
				Python: peHostPython(t), Wheelhouse: wheels, Cache: filepath.Join(tmp, "cache"), Env: buildEnv, Offline: true, Sealed: sealed, Explicit: true})
			if err != nil {
				t.Fatal(err)
			}
			got := peRun(t, []string{executable, "-I", "-c", "import sys, json, app_dep, leaf_dep, importlib.util; " +
				"print(json.dumps([app_dep.__version__, leaf_dep.__version__, sys.base_prefix, importlib.util.find_spec('missing_dep') is None]))"}, tmp, env)
			var result []any
			json.Unmarshal([]byte(got), &result)
			if !reflect.DeepEqual(result, []any{"1.0", "1.0", peBasePrefix(t, env), true}) {
				t.Fatalf("result %v", result)
			}
			if filepath.Dir(filepath.Dir(executable)) != out {
				t.Fatalf("destination %s", executable)
			}
			editable := false
			kept := false
			filepath.WalkDir(out, func(path string, d fs.DirEntry, err error) error {
				editable = editable || d.Name() == "__editable__.app_dep-1.0.pth"
				kept = kept || d.Name() == "app_dep.pth"
				return nil
			})
			if editable == sealed || !kept {
				t.Fatalf("editable %v kept %v sealed %v", editable, kept, sealed)
			}
			if existsOrLink(poison) || existsOrLink(env["ICEBATH_HOME"]) || !reflect.DeepEqual(os.Environ(), before) {
				t.Fatal("ambient settings escaped the explicit build")
			}
		})
	}
}

func TestBuildOperationsFailedRequirementBuildRemovesOnlyItsCandidate(t *testing.T) {
	for _, failure := range []string{"create", "install", "check"} {
		t.Run(failure, func(t *testing.T) {
			tmp, env := peBuildTools(t)
			wheels := filepath.Join(tmp, "wheels")
			wheel := peWheel(t, wheels, "app_dep", "", nil, nil)
			ctx := context.Background()
			previous := filepath.Join(tmp, "previous")
			build := func(out, python, cache string) (string, error) {
				return BuildRequirementsEnvironment(ctx, BuildRequirementsEnvironmentOptions{Requirements: []string{"app-dep==1.0"}, Out: out,
					Python: python, Wheelhouse: wheels, Env: env, Cache: cache, Offline: true, Explicit: true})
			}
			executable, err := build(previous, "", filepath.Join(tmp, "cache"))
			if err != nil {
				t.Fatal(err)
			}
			cfg := peRead(t, filepath.Join(previous, "pyvenv.cfg"))
			python := peHostPython(t)
			switch failure {
			case "create":
				python = filepath.Join(tmp, "missing-python")
			case "install":
				os.Remove(wheel)
			default:
				data, _ := os.ReadFile(wheel)
				reader, _ := zip.NewReader(bytes.NewReader(data), int64(len(data)))
				var buf bytes.Buffer
				w := zip.NewWriter(&buf)
				for _, f := range reader.File {
					if err := w.Copy(f); err != nil {
						t.Fatal(err)
					}
				}
				extra, _ := w.Create("app_dep-1.0.data/purelib/ghost-1.0.dist-info/METADATA")
				extra.Write([]byte("Metadata-Version: 2.1\nName: ghost\nVersion: 1.0\nRequires-Dist: missing-dep==1.0\n"))
				w.Close()
				os.WriteFile(wheel, buf.Bytes(), 0o644)
			}
			out := filepath.Join(tmp, "candidate")
			_, err = build(out, python, filepath.Join(tmp, "cold-cache"))
			var ie *InstallError
			if !errors.As(err, &ie) || failure == "check" && !strings.Contains(err.Error(), "dependency validation") {
				t.Fatalf("failed build: %v", err)
			}
			if existsOrLink(out) {
				t.Fatal("failed build left its candidate")
			}
			if _, err := build(previous, "", filepath.Join(tmp, "cache")); !errors.Is(err, fs.ErrExist) || !strings.Contains(err.Error(), "already exists") {
				t.Fatalf("existing destination: %v", err)
			}
			if peRead(t, filepath.Join(previous, "pyvenv.cfg")) != cfg {
				t.Fatal("previous environment changed")
			}
			if got := peRun(t, []string{executable, "-I", "-c", "import app_dep; print(app_dep.__version__)"}, tmp, env); got != "1.0" {
				t.Fatalf("previous import %q", got)
			}
		})
	}
}

func TestBuildOperationsWheelhouseCannotFallBackToIndexOrBuildSource(t *testing.T) {
	tmp, env := peBuildTools(t)
	index := filepath.Join(tmp, "index", "leaf-dep")
	wheel := peWheel(t, index, "leaf_dep", "", nil, nil)
	peWrite(t, filepath.Join(index, "index.html"), `<a href="`+filepath.Base(wheel)+`">`+filepath.Base(wheel)+`</a>`)
	wheels := filepath.Join(tmp, "wheelhouse")
	os.MkdirAll(wheels, 0o777)
	backend := "from pathlib import Path\nimport base64\n" +
		"def build_wheel(wheel_directory, config_settings=None, metadata_directory=None):\n" +
		"    Path(wheel_directory, " + pyRepr(filepath.Base(wheel)) + ").write_bytes(base64.b64decode(" +
		"b'" + base64.StdEncoding.EncodeToString(readFile(t, wheel)) + "'))\n" +
		"    return " + pyRepr(filepath.Base(wheel)) + "\n"
	entries := [][2]string{{"pyproject.toml", "[project]\nname=\"leaf-dep\"\nversion=\"1.0\"\n[build-system]\nrequires=[]\nbuild-backend=\"backend\"\nbackend-path=[\".\"]\n"},
		{"backend.py", backend}}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		tw.WriteHeader(&tar.Header{Name: "leaf_dep-1.0/" + e[0], Mode: 0o644, Size: int64(len(e[1])), Typeflag: tar.TypeReg})
		tw.Write([]byte(e[1]))
	}
	tw.Close()
	gz.Close()
	writeFile(t, filepath.Join(wheels, "leaf_dep-1.0.tar.gz"), buf.Bytes(), 0o644)
	indexEnv := map[string]string{}
	for k, v := range env {
		indexEnv[k] = v
	}
	indexEnv["UV_DEFAULT_INDEX"] = "file://" + filepath.ToSlash(filepath.Dir(index))
	ctx := context.Background()
	out := filepath.Join(tmp, "candidate")
	_, err := BuildRequirementsEnvironment(ctx, BuildRequirementsEnvironmentOptions{Requirements: []string{"leaf-dep==1.0"}, Out: out, Wheelhouse: wheels,
		Env: indexEnv, Cache: filepath.Join(tmp, "cache"), Offline: true, Explicit: true})
	var ie *InstallError
	if !errors.As(err, &ie) || existsOrLink(out) {
		t.Fatalf("wheelhouse build fell back: %v", err)
	}
	sourceEnv := map[string]string{}
	for k, v := range env {
		sourceEnv[k] = v
	}
	sourceEnv["UV_NO_INDEX"], sourceEnv["UV_FIND_LINKS"] = "1", wheels
	for name, settings := range map[string]map[string]string{"index": indexEnv, "source": sourceEnv} {
		executable, err := BuildRequirementsEnvironment(ctx, BuildRequirementsEnvironmentOptions{Requirements: []string{"leaf-dep==1.0"},
			Out: filepath.Join(tmp, "from-"+name), Env: settings, Cache: filepath.Join(tmp, name+"-cache"), Offline: true, Explicit: true})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := peRun(t, []string{executable, "-I", "-c", "import leaf_dep; print(leaf_dep.__version__)"}, tmp, env); got != "1.0" {
			t.Fatalf("%s import %q", name, got)
		}
	}
}

func TestBuildOperationsReadyToolsDoNotBypassDisabledLazyOperations(t *testing.T) {
	for _, operation := range []string{"check", "export", "build"} {
		t.Run(operation, func(t *testing.T) {
			tmp, env := peBuildTools(t)
			source := peLockedSource(t, tmp, env)
			t.Setenv(EnvDisableLazyInstalls, "1")
			ctx := context.Background()
			out := filepath.Join(tmp, "blocked-output")
			before := peTreeBytes(t, source)
			var err error
			switch operation {
			case "check":
				err = checkProjectLock(ctx, CheckProjectLockOptions{Source: source, Env: env, Cache: filepath.Join(tmp, "cache")})
			case "export":
				_, err = exportRequirements(ctx, ExportRequirementsOptions{Source: source, Out: out, Env: env, Cache: filepath.Join(tmp, "cache")})
			default:
				_, err = buildRequirementsEnvironment(ctx, BuildRequirementsEnvironmentOptions{Requirements: []string{"base-dep==1.0"}, Out: out, Env: env,
					Cache: filepath.Join(tmp, "cache"), Offline: true})
			}
			if err == nil || !strings.Contains(err.Error(), "lazy installs are disabled") {
				t.Fatalf("%s: %v", operation, err)
			}
			if existsOrLink(out) || !reflect.DeepEqual(peTreeBytes(t, source), before) {
				t.Fatal("refused operation wrote output")
			}
			if err := checkProjectLock(ctx, CheckProjectLockOptions{Source: source, Env: env, Cache: filepath.Join(tmp, "cache"), Offline: true}); err != nil {
				t.Fatalf("passive check: %v", err)
			}
		})
	}
}

func TestBuildOperationsPruneCacheDoesNotAcquireAMissingToolchain(t *testing.T) {
	tmp := isolateHome(t)
	store := filepath.Join(tmp, "empty-store")
	t.Setenv(EnvRuntimeDir, store)
	err := PruneCache(context.Background(), PruneCacheOptions{Cache: filepath.Join(tmp, "cache")})
	if err == nil || !strings.Contains(err.Error(), "pinned toolchain is unavailable") {
		t.Fatalf("got %v", err)
	}
	if existsOrLink(store) {
		t.Fatal("cache pruning acquired tools")
	}
}

func TestBuildOperationsPruneCachePreservesDownloadedWheelsUnlessCI(t *testing.T) {
	for _, ci := range []bool{false, true} {
		t.Run(fmt.Sprint(ci), func(t *testing.T) {
			tmp, env := peBuildTools(t)
			wheels := filepath.Join(tmp, "wheels")
			wheel := peWheel(t, wheels, "cached_dep", "", nil, nil)
			server := httptest.NewServer(http.FileServer(http.Dir(wheels)))
			cache := filepath.Join(tmp, "cache")
			requirement := "cached-dep @ " + server.URL + "/" + filepath.Base(wheel)
			ctx := context.Background()
			executable, err := BuildRequirementsEnvironment(ctx, BuildRequirementsEnvironmentOptions{Requirements: []string{requirement},
				Out: filepath.Join(tmp, "first"), Cache: cache, Env: env, Explicit: true})
			server.Close()
			if err != nil {
				t.Fatal(err)
			}
			if got := peRun(t, []string{executable, "-I", "-c", "import cached_dep; print(cached_dep.__version__)"}, tmp, env); got != "1.0" {
				t.Fatalf("first import %q", got)
			}
			if err := PruneCache(ctx, PruneCacheOptions{Cache: cache, CI: ci}); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(tmp, "second")
			second, err := BuildRequirementsEnvironment(ctx, BuildRequirementsEnvironmentOptions{Requirements: []string{requirement}, Out: out,
				Cache: cache, Env: env, Offline: true, Explicit: true})
			if ci {
				var ie *InstallError
				if !errors.As(err, &ie) || existsOrLink(out) {
					t.Fatalf("ci prune kept the wheel: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := peRun(t, []string{second, "-I", "-c", "import cached_dep; print(cached_dep.__version__)"}, tmp, env); got != "1.0" {
				t.Fatalf("second import %q", got)
			}
		})
	}
}
