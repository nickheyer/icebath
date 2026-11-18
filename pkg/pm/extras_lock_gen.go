//go:build !icebath_core && !icebath_noextras

package pm

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Extras lock of a source checkout, the file pm embeds
func ExtrasLockPath(root string) string {
	return filepath.Join(root, "pkg", "pm", "extras_lock.json")
}

// Source checkout holding the working directory, found by its go.mod beside an extras lock, empty outside one
func extrasCheckout() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if isRegularFile(filepath.Join(dir, "go.mod")) && isRegularFile(ExtrasLockPath(dir)) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// One platform the extras resolve for, the uv environment marker and the pm targets it covers
type releaseEnvironment struct {
	marker  string
	targets []string
}

// Platforms icebath releases on in the marker spelling uv reports a failed split with
var releaseEnvironments = []releaseEnvironment{
	{"platform_machine == 'x86_64' and sys_platform == 'linux'", []string{"linux-x64", "linux-x64-musl"}},
	{"platform_machine == 'aarch64' and sys_platform == 'linux'", []string{"linux-arm64", "linux-arm64-musl", "linux-arm64-bionic"}},
	{"platform_machine == 'x86_64' and sys_platform == 'darwin'", []string{"darwin-x64"}},
	{"platform_machine == 'arm64' and sys_platform == 'darwin'", []string{"darwin-arm64"}},
	{"platform_machine == 'AMD64' and sys_platform == 'win32'", []string{"win32-x64"}},
	{"platform_machine == 'ARM64' and sys_platform == 'win32'", []string{"win32-arm64"}},
}

var (
	failedSplit  = regexp.MustCompile(`for split \(markers: ([^)]*)\)`)
	missingWheel = regexp.MustCompile("([A-Za-z0-9][A-Za-z0-9_.-]*)(?:\\{[^}]*\\})?((?:[<>=!~][^ ]*)?) has no (?:`[^`]*`-compatible wheels|wheels with a matching [a-zA-Z ]+tag)")
)

// Re-resolves every extra of a checkout's lock with the pinned uv and writes the lock back when it changed
func relockExtras(ctx context.Context, root string) (int, error) {
	out := SyncOutput(ctx)
	if root == "" {
		return 0, NewInstallError("extras", "the working directory is not inside a source checkout holding pkg/pm/extras_lock.json", "run `icebath pm lock` from a source checkout")
	}
	path := ExtrasLockPath(root)
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	lock, err := parseExtrasLock(data)
	if err != nil {
		return 0, err
	}
	toolchainCtx, cache, found, err := runtimeToolchainContext(ctx, ExtrasRoot(ctx))
	if err != nil {
		return 0, err
	}
	if !found {
		if err := ensureRuntimeToolchain(toolchainCtx, out); err != nil {
			return 0, err
		}
	}
	uv, python, found, err := uvToolchain(toolchainCtx, false, true)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, NewInstallError("extras", "PM's pinned toolchain is unavailable", "run `icebath pm install uv`")
	}
	if cache == "" {
		if cache, err = UvCacheDir(ctx); err != nil {
			return 0, err
		}
	}
	short, _, err := extrasPythonVersion()
	if err != nil {
		return 0, err
	}
	env, err := BaseEnvironment(nil)
	if err != nil {
		return 0, err
	}
	for k := range env {
		if IsIndexRedirect(k) {
			delete(env, k)
		}
	}
	env["UV_PYTHON"], env["UV_CACHE_DIR"], env["UV_PYTHON_DOWNLOADS"] = Native(python), Native(cache), "never"
	scratch, err := os.MkdirTemp("", "pm-extras-lock-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(scratch)
	run := &lockRun{ctx: ctx, uv: uv, python: python, env: env, short: short, dir: scratch, sdists: map[string]string{}, requires: map[string][]string{}}
	lock.Schema, lock.Python = 1, short
	for _, name := range sortedKeys(lock.Extras) {
		fmt.Fprintf(out, "→ Resolving %s...\n", name)
		if err := run.lockExtra(name, lock.Extras[name]); err != nil {
			return 0, fmt.Errorf("extra %s: %w", name, err)
		}
		entry := lock.Extras[name]
		fmt.Fprintf(out, "✓ %s: %d packages", name, len(entry.Packages))
		if len(entry.BuildDeps) > 0 {
			fmt.Fprintf(out, ", %d build requirements", len(entry.BuildDeps))
		}
		if len(entry.Unsupported) > 0 {
			fmt.Fprintf(out, ", none for %s", strings.Join(sortedKeys(entry.Unsupported), " "))
		}
		fmt.Fprintln(out)
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", " ")
	if err := encoder.Encode(lock); err != nil {
		return 0, err
	}
	encoded := buf.Bytes()
	if string(encoded) == string(data) {
		fmt.Fprintln(out, "extras lock is current: "+path)
		return 0, nil
	}
	if err := DurableWriteBytes(path, encoded); err != nil {
		return 0, err
	}
	fmt.Fprintln(out, "wrote "+path+"; rebuild icebath to embed it")
	return 0, nil
}

// Pinned uv and interpreter one lock run resolves and probes with, and the sdists it fetched
type lockRun struct {
	ctx      context.Context
	uv       string
	python   string
	env      map[string]string
	short    string
	dir      string
	sdists   map[string]string
	requires map[string][]string
	shadow   string
}

// Build tools a backend fetches from the index when this machine lacks them
var indexBuildTools = map[string]bool{"cmake": true, "cmake3": true, "ninja": true, "ninja-build": true, "patchelf": true}

// Directory linking every program on PATH except the build tools a backend may fetch, made once per lock run
func (r *lockRun) shadowPath() (string, error) {
	if r.shadow != "" {
		return r.shadow, nil
	}
	dir := filepath.Join(r.dir, "shadow-path")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	for _, bin := range filepath.SplitList(r.env["PATH"]) {
		entries, err := os.ReadDir(bin)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			stem := strings.TrimSuffix(strings.ToLower(name), ".exe")
			link := filepath.Join(dir, name)
			if _, taken := os.Lstat(link); indexBuildTools[stem] || e.IsDir() || taken == nil {
				continue
			}
			if err := os.Symlink(filepath.Join(bin, name), link); err != nil {
				return "", err
			}
		}
	}
	r.shadow = dir
	return dir, nil
}

// Scratch project resolving one extra for the given platforms, its requirements and index sources
func extraProject(name, short string, entry *ExtraEntry, environments []releaseEnvironment) string {
	var b strings.Builder
	b.WriteString("[project]\nname = " + tomlQuote("icebath-extra-"+name) + "\nversion = \"0\"\nrequires-python = " + tomlQuote("=="+short+".*") + "\ndependencies = [\n")
	for _, r := range entry.Requires {
		b.WriteString("  " + tomlQuote(r) + ",\n")
	}
	b.WriteString("]\n\n[tool.uv]\npackage = false\nenvironments = [\n")
	for _, e := range environments {
		b.WriteString("  " + tomlQuote(e.marker) + ",\n")
	}
	b.WriteString("]\n")
	indexes := map[string]bool{}
	for _, s := range entry.Sources {
		indexes[s.Index] = true
	}
	names := map[string]string{}
	for i, index := range sortedKeys(indexes) {
		names[index] = fmt.Sprintf("extra-index-%d", i)
		b.WriteString("\n[[tool.uv.index]]\nname = " + tomlQuote(names[index]) + "\nurl = " + tomlQuote(index) + "\nexplicit = true\n")
	}
	if len(entry.Sources) > 0 {
		b.WriteString("\n[tool.uv.sources]\n")
		for _, pkg := range sortedKeys(entry.Sources) {
			s := entry.Sources[pkg]
			b.WriteString(tomlQuote(pkg) + " = [{ index = " + tomlQuote(names[s.Index]))
			if s.Marker != "" {
				b.WriteString(", marker = " + tomlQuote(s.Marker))
			}
			b.WriteString(" }]\n")
		}
	}
	return b.String()
}

// Resolves requirements in a scratch project for every release platform, dropping each one uv cannot solve with its reason
func (r *lockRun) resolve(name string, entry *ExtraEntry) ([]LockedPackage, map[string]string, error) {
	scratch, err := os.MkdirTemp(r.dir, "resolve-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(scratch)
	run := func(args ...string) (uvResult, error) {
		return runCaptured(r.ctx, append([]string{Native(r.uv)}, append(args, "--no-config")...), scratch, r.env, 30*time.Minute)
	}
	environments := append([]releaseEnvironment{}, releaseEnvironments...)
	unsupported := map[string]string{}
	for {
		if err := os.WriteFile(filepath.Join(scratch, "pyproject.toml"), []byte(extraProject(name, r.short, entry, environments)), 0o666); err != nil {
			return nil, nil, err
		}
		result, err := run("lock")
		if err != nil {
			return nil, nil, err
		}
		if result.Code == 0 {
			break
		}
		stderr := strings.Join(strings.Fields(result.Stderr), " ")
		m := failedSplit.FindStringSubmatch(stderr)
		if m == nil {
			return nil, nil, fmt.Errorf("uv lock failed: %s", strings.TrimSpace(result.Stderr))
		}
		kept := environments[:0]
		dropped := false
		for _, e := range environments {
			if e.marker == m[1] {
				dropped = true
				reason := "no version of its requirements installs on this platform"
				if all := missingWheel.FindAllStringSubmatch(stderr, -1); len(all) > 0 {
					last := all[len(all)-1]
					reason = last[1] + last[2] + " publishes no wheel for it under Python " + r.short
				}
				for _, target := range e.targets {
					unsupported[target] = reason
				}
				continue
			}
			kept = append(kept, e)
		}
		if !dropped || len(kept) == 0 {
			return nil, nil, fmt.Errorf("uv lock failed: %s", strings.TrimSpace(result.Stderr))
		}
		environments = kept
	}
	result, err := run("export", "--frozen", "--format", "pylock.toml", "--no-emit-project", "--no-header", "-o", "pylock.toml")
	if err != nil {
		return nil, nil, err
	}
	if result.Code != 0 {
		return nil, nil, fmt.Errorf("uv export failed: %s", strings.TrimSpace(result.Stderr))
	}
	text, err := os.ReadFile(filepath.Join(scratch, "pylock.toml"))
	if err != nil {
		return nil, nil, err
	}
	packages, err := readPylock(string(text))
	return packages, unsupported, err
}

// Packages a target installs from their sdist because no wheel of them serves it, a package with neither marking the target unsupported
func sourceBuilds(packages []LockedPackage, target string, unsupported map[string]string) ([]LockedPackage, error) {
	var built []LockedPackage
	for _, p := range packages {
		admitted, err := markerAdmits(p.Marker, target)
		if err != nil {
			return nil, err
		}
		if !admitted {
			continue
		}
		served := false
		for _, w := range p.Wheels {
			served = served || containsString(wheelTargets(w.URL), target)
		}
		switch {
		case served:
		case p.Sdist != nil:
			built = append(built, p)
		default:
			unsupported[target] = p.Name + " " + p.Version + " publishes no wheel for it and no source"
		}
	}
	return built, nil
}

// Resolves one extra, records which targets compile which packages, then pins by hash what every source build installs
func (r *lockRun) lockExtra(name string, entry *ExtraEntry) error {
	packages, unsupported, err := r.resolve(name, entry)
	if err != nil {
		return err
	}
	entry.Packages = packages
	entry.Build = map[string][]string{}
	compiled := map[string]bool{}
	built := map[string]LockedPackage{}
	builtOn := map[string][]string{}
	for _, target := range AllTargets {
		if _, gone := unsupported[target]; gone {
			continue
		}
		sources, err := sourceBuilds(packages, target, unsupported)
		if err != nil {
			return err
		}
		if _, gone := unsupported[target]; gone {
			continue
		}
		for _, p := range sources {
			built[p.Name] = p
			builtOn[target] = append(builtOn[target], p.Name)
			native, seen := compiled[p.Name]
			if !seen {
				archive, err := r.fetchSdist(*p.Sdist)
				if err != nil {
					return err
				}
				if native, err = sdistCompiles(archive); err != nil {
					return err
				}
				compiled[p.Name] = native
			}
			if native {
				entry.Build[target] = append(entry.Build[target], p.Name)
			}
		}
	}
	entry.BuildDeps = nil
	if len(built) > 0 {
		deps, gone, err := r.lockBuildDeps(name, built)
		if err != nil {
			return err
		}
		entry.BuildDeps = deps
		for target, reason := range gone {
			if len(builtOn[target]) > 0 {
				unsupported[target] = "the source build of " + strings.Join(builtOn[target], ", ") + " needs " + reason
			}
		}
	}
	for target := range unsupported {
		delete(entry.Build, target)
	}
	entry.Unsupported = unsupported
	if len(entry.Build) == 0 {
		entry.Build = nil
	}
	if len(entry.Unsupported) == 0 {
		entry.Unsupported = nil
	}
	return nil
}

// Hash pinned closure of the build requirements of the source built packages, the builds of that closure's own sdists included
func (r *lockRun) lockBuildDeps(name string, built map[string]LockedPackage) ([]LockedPackage, map[string]string, error) {
	requires := map[string]bool{}
	probed := map[string]bool{}
	pending := built
	for {
		for _, key := range sortedKeys(pending) {
			p := pending[key]
			probed[p.Name+"=="+p.Version] = true
			reqs, err := r.sdistBuildRequires(*p.Sdist)
			if err != nil {
				return nil, nil, fmt.Errorf("build requirements of %s %s: %w", p.Name, p.Version, err)
			}
			for _, req := range reqs {
				requires[req] = true
			}
		}
		closure, unsupported, err := r.resolve(name+"-build", &ExtraEntry{Requires: sortedKeys(requires)})
		if err != nil {
			return nil, nil, err
		}
		pending = map[string]LockedPackage{}
		for _, target := range AllTargets {
			if _, gone := unsupported[target]; gone {
				continue
			}
			sources, err := sourceBuilds(closure, target, unsupported)
			if err != nil {
				return nil, nil, err
			}
			for _, p := range sources {
				if !probed[p.Name+"=="+p.Version] {
					pending[p.Name] = p
				}
			}
		}
		if len(pending) == 0 {
			return closure, unsupported, nil
		}
	}
}

// Script asking a build backend for the requirements of a wheel build, run from the unpacked source tree
const buildRequiresScript = `import importlib, json, sys
sys.path[:0] = sys.argv[2:]
module, _, attrs = sys.argv[1].partition(":")
backend = importlib.import_module(module)
for attr in filter(None, attrs.split(".")):
    backend = getattr(backend, attr)
hook = getattr(backend, "get_requires_for_build_wheel", None)
print(json.dumps(list(hook({}) if hook else [])))
`

// Static and backend reported build requirements of an sdist, the backend asked with none of the index build tools on PATH
func (r *lockRun) sdistBuildRequires(f LockedFile) ([]string, error) {
	if cached, ok := r.requires[f.URL]; ok {
		return cached, nil
	}
	archive, err := r.fetchSdist(f)
	if err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(r.dir, "build-requires-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	source := filepath.Join(work, "source")
	if err := Extract(archive, source); err != nil {
		return nil, err
	}
	if entries, err := os.ReadDir(source); err == nil && len(entries) == 1 && entries[0].IsDir() {
		source = filepath.Join(source, entries[0].Name())
	}
	requires := []string{"setuptools>=40.8.0"}
	backend := "setuptools.build_meta:__legacy__"
	var backendPath []string
	if text, err := os.ReadFile(filepath.Join(source, "pyproject.toml")); err == nil {
		doc, err := tomlLoads(string(trimBOM(text)))
		if err != nil {
			return nil, err
		}
		if system, ok := doc.Values["build-system"].(*OrderedObject); ok {
			if listed, ok := tomlStringList(system.Values["requires"]); ok {
				requires = listed
			}
			if named, ok := system.Values["build-backend"].(string); ok && named != "" {
				backend = named
			}
			if paths, ok := tomlStringList(system.Values["backend-path"]); ok {
				for _, rel := range paths {
					backendPath = append(backendPath, filepath.Join(source, filepath.FromSlash(rel)))
				}
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	venv := filepath.Join(work, "venv")
	uvRun := func(args ...string) error {
		result, err := runCaptured(r.ctx, append([]string{Native(r.uv)}, append(args, "--no-config")...), work, r.env, 30*time.Minute)
		if err != nil {
			return err
		}
		if result.Code != 0 {
			return fmt.Errorf("uv %s exited %d: %s", args[0], result.Code, strings.TrimSpace(result.Stderr))
		}
		return nil
	}
	if err := uvRun("venv", "--python", Native(r.python), Native(venv)); err != nil {
		return nil, err
	}
	if len(requires) > 0 {
		if err := uvRun(append([]string{"pip", "install", "--python", Native(VenvPython(venv))}, requires...)...); err != nil {
			return nil, err
		}
	}
	hookEnv := map[string]string{}
	for k, v := range r.env {
		hookEnv[k] = v
	}
	shadow, err := r.shadowPath()
	if err != nil {
		return nil, err
	}
	hookEnv["PATH"] = filepath.Dir(VenvPython(venv)) + string(os.PathListSeparator) + shadow
	command := append([]string{Native(VenvPython(venv)), "-I", "-c", buildRequiresScript, backend}, backendPath...)
	result, err := runCaptured(r.ctx, command, source, hookEnv, 10*time.Minute)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	var dynamic []string
	if result.Code != 0 || json.Unmarshal([]byte(lines[len(lines)-1]), &dynamic) != nil {
		return nil, fmt.Errorf("%s get_requires_for_build_wheel exited %d: %s", backend, result.Code, lastChars(strings.TrimSpace(result.Stdout+"\n"+result.Stderr), 2000))
	}
	all := append(append([]string{}, requires...), dynamic...)
	r.requires[f.URL] = all
	return all, nil
}

// Suffixes and file names of native sources whose presence in an sdist means a compiler builds it
var (
	nativeSourceSuffixes = []string{".c", ".cc", ".cpp", ".cxx", ".pyx", ".rs", ".cu", ".f", ".f90"}
	nativeBuildFiles     = map[string]bool{"Cargo.toml": true, "CMakeLists.txt": true, "meson.build": true}
)

// Local copy of an sdist fetched once per lock run, its digest checked against the pin
func (r *lockRun) fetchSdist(f LockedFile) (string, error) {
	if path, ok := r.sdists[f.URL]; ok {
		return path, nil
	}
	base := filepath.Base(strings.SplitN(f.URL, "?", 2)[0])
	path := filepath.Join(r.dir, fmt.Sprintf("sdist-%d-%s", len(r.sdists), base))
	if err := fetchPinnedFile(f, path); err != nil {
		return "", err
	}
	r.sdists[f.URL] = path
	return path, nil
}

// Whether an sdist archive carries native sources, read from its member names
func sdistCompiles(path string) (bool, error) {
	native := func(name string) bool {
		base := filepath.Base(name)
		if nativeBuildFiles[base] {
			return true
		}
		for _, suffix := range nativeSourceSuffixes {
			if strings.HasSuffix(strings.ToLower(base), suffix) {
				return true
			}
		}
		return false
	}
	scratch, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer scratch.Close()
	if strings.HasSuffix(path, ".zip") {
		info, err := scratch.Stat()
		if err != nil {
			return false, err
		}
		archive, err := zip.NewReader(scratch, info.Size())
		if err != nil {
			return false, err
		}
		for _, member := range archive.File {
			if native(member.Name) {
				return true, nil
			}
		}
		return false, nil
	}
	gz, err := gzip.NewReader(scratch)
	if err != nil {
		return false, err
	}
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if native(header.Name) {
			return true, nil
		}
	}
}

// Packages of an exported pylock document, each keeping only wheels some pm target installs
func readPylock(text string) ([]LockedPackage, error) {
	doc, err := tomlLoads(text)
	if err != nil {
		return nil, err
	}
	rows, err := tomlIter(doc.Values["packages"])
	if err != nil {
		return nil, err
	}
	file := func(v any) (*LockedFile, error) {
		table, ok := v.(*OrderedObject)
		if !ok {
			return nil, fmt.Errorf("pylock artifact is not a table")
		}
		hashes, _ := table.Values["hashes"].(*OrderedObject)
		if hashes == nil {
			return nil, fmt.Errorf("pylock artifact %s has no hashes", tomlText(table.Values["url"]))
		}
		return &LockedFile{URL: tomlText(table.Values["url"]), SHA256: tomlText(hashes.Values["sha256"])}, nil
	}
	var out []LockedPackage
	for _, row := range rows {
		table, ok := row.(*OrderedObject)
		if !ok {
			return nil, fmt.Errorf("pylock package is not a table")
		}
		p := LockedPackage{Name: tomlText(table.Values["name"]), Version: tomlText(table.Values["version"])}
		if v, ok := table.Values["marker"]; ok {
			p.Marker = tomlText(v)
		}
		if v, ok := table.Values["index"]; ok && tomlText(v) != "https://pypi.org/simple" {
			p.Index = tomlText(v)
		}
		if v, ok := table.Values["sdist"]; ok {
			if p.Sdist, err = file(v); err != nil {
				return nil, err
			}
		}
		if v, ok := table.Values["archive"]; ok {
			f, err := file(v)
			if err != nil {
				return nil, err
			}
			if strings.HasSuffix(f.URL, ".whl") {
				p.Wheels = append(p.Wheels, *f)
			} else {
				p.Sdist = f
			}
		}
		if v, ok := table.Values["wheels"]; ok {
			wheels, err := tomlIter(v)
			if err != nil {
				return nil, err
			}
			for _, w := range wheels {
				f, err := file(w)
				if err != nil {
					return nil, err
				}
				if len(wheelTargets(f.URL)) > 0 {
					p.Wheels = append(p.Wheels, *f)
				}
			}
			sort.Slice(p.Wheels, func(i, j int) bool { return p.Wheels[i].URL < p.Wheels[j].URL })
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
