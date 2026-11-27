package packages

import (
	"archive/tar"
	"archive/zip"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
	"github.com/pelletier/go-toml/v2"
)

// One platform the extras resolve for: the marker uv names a failed split with and the targets it covers
type releaseEnvironment struct {
	marker  string
	targets []string
}

// Platforms icebath runs on, in the marker spelling uv reports
var releaseEnvironments = []releaseEnvironment{
	{"platform_machine == 'x86_64' and sys_platform == 'linux'", []string{"linux-x64", "linux-x64-musl"}},
	{"platform_machine == 'aarch64' and sys_platform == 'linux'", []string{"linux-arm64", "linux-arm64-musl", Bionic}},
	{"platform_machine == 'x86_64' and sys_platform == 'darwin'", []string{"darwin-x64"}},
	{"platform_machine == 'arm64' and sys_platform == 'darwin'", []string{"darwin-arm64"}},
	{"platform_machine == 'AMD64' and sys_platform == 'win32'", []string{"win32-x64"}},
	{"platform_machine == 'ARM64' and sys_platform == 'win32'", []string{"win32-arm64"}},
}

var (
	failedSplit  = regexp.MustCompile(`for split \(markers: ([^)]*)\)`)
	missingWheel = regexp.MustCompile("([A-Za-z0-9][A-Za-z0-9_.-]*)(?:\\{[^}]*\\})?((?:[<>=!~][^ ]*)?) has no (?:`[^`]*`-compatible wheels|wheels with a matching [a-zA-Z ]+tag)")
)

// Build tools a backend fetches from the index when the machine lacks them, kept off the PATH its build requirements are asked with
var indexBuildTools = map[string]bool{"cmake": true, "cmake3": true, "ninja": true, "ninja-build": true, "patchelf": true}

// Native sources and build files whose presence in an sdist means a compiler builds it
var (
	nativeSuffixes = []string{".c", ".cc", ".cpp", ".cxx", ".pyx", ".rs", ".cu", ".f", ".f90"}
	nativeFiles    = map[string]bool{"Cargo.toml": true, "CMakeLists.txt": true, "meson.build": true}
)

// One relock: the pinned uv and interpreter it resolves and probes with, and the sdists it fetched
type relocking struct {
	h        *Packages
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

// Resolves every declared extra again with the pinned uv for every platform and keeps the resolution in the user's extras lock
func (h *Packages) relock(ctx context.Context, cfg *config.Config, r Reporter) (int, error) {
	uvBin, python, _, err := h.toolchain(ctx, cfg, true, true, r)
	if err != nil {
		return 0, err
	}
	short, _, err := pinnedPython(h.lock())
	if err != nil {
		return 0, err
	}
	env, err := uvBase(nil)
	if err != nil {
		return 0, err
	}
	maps.DeleteFunc(env, func(k, _ string) bool { return redirect(k) })
	env["UV_PYTHON"], env["UV_CACHE_DIR"], env["UV_PYTHON_DOWNLOADS"] = python, h.uvCache(), "never"
	scratch, err := os.MkdirTemp("", "pm-extras-lock-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(scratch)
	run := &relocking{h: h, ctx: ctx, uv: uvBin, python: python, env: env, short: short, dir: scratch, sdists: map[string]string{},
		requires: map[string][]string{}}
	doc := extrasDoc{Schema: 1, Python: short, Extras: map[string]*Extra{}}
	_, declared := h.index()
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		entry := *declared[name]
		r.Line(fmt.Sprintf("→ Resolving %s...", name))
		if err := run.lockExtra(name, &entry); err != nil {
			return 0, fmt.Errorf("extra %s: %w", name, err)
		}
		line := fmt.Sprintf("✓ %s: %d packages", name, len(entry.Packages))
		if len(entry.BuildDeps) > 0 {
			line += fmt.Sprintf(", %d build requirements", len(entry.BuildDeps))
		}
		if len(entry.Unsupported) > 0 {
			line += ", none for " + strings.Join(slices.Sorted(maps.Keys(entry.Unsupported)), " ")
		}
		r.Line(line)
		doc.Extras[name] = &entry
	}
	path := filepath.Join(h.stateDir(), "extras.json")
	encoded, err := json.MarshalIndent(doc, "", " ")
	if err != nil {
		return 0, err
	}
	encoded = append(encoded, '\n')
	if current, err := os.ReadFile(path); err == nil && string(current) == string(encoded) {
		r.Line("extras lock is current: " + path)
		return 0, nil
	}
	if err := fsx.Write(path, encoded, 0o600); err != nil {
		return 0, err
	}
	r.Line("wrote " + path + "; extras provisioned from now on install its pins")
	return 0, nil
}

// Scratch project resolving one extra for the platforms given, with its index sources
func extraProject(name, short string, e *Extra, environments []releaseEnvironment) string {
	var b strings.Builder
	b.WriteString("[project]\nname = " + tomlQuote("icebath-extra-"+name) + "\nversion = \"0\"\nrequires-python = " + tomlQuote("=="+short+".*") + "\ndependencies = [\n")
	for _, req := range e.Requires {
		b.WriteString("  " + tomlQuote(req) + ",\n")
	}
	b.WriteString("]\n\n[tool.uv]\npackage = false\nenvironments = [\n")
	for _, env := range environments {
		b.WriteString("  " + tomlQuote(env.marker) + ",\n")
	}
	b.WriteString("]\n")
	indexes := map[string]string{}
	urls := map[string]bool{}
	for _, s := range e.Sources {
		urls[s.Index] = true
	}
	for i, index := range slices.Sorted(maps.Keys(urls)) {
		indexes[index] = fmt.Sprintf("extra-index-%d", i)
		b.WriteString("\n[[tool.uv.index]]\nname = " + tomlQuote(indexes[index]) + "\nurl = " + tomlQuote(index) + "\nexplicit = true\n")
	}
	if len(e.Sources) > 0 {
		b.WriteString("\n[tool.uv.sources]\n")
		for _, pkg := range slices.Sorted(maps.Keys(e.Sources)) {
			s := e.Sources[pkg]
			b.WriteString(tomlQuote(pkg) + " = [{ index = " + tomlQuote(indexes[s.Index]))
			if s.Marker != "" {
				b.WriteString(", marker = " + tomlQuote(s.Marker))
			}
			b.WriteString(" }]\n")
		}
	}
	return b.String()
}

// Runs uv in a folder with the relock's environment, refusing user config
func (r *relocking) uvRun(dir string, timeout time.Duration, args ...string) (result, error) {
	return run(r.ctx, command{argv: append(append([]string{r.uv}, args...), "--no-config"), dir: dir, env: r.env, timeout: timeout})
}

// Resolves requirements for every platform, each platform uv can't solve dropped with why
func (r *relocking) resolve(name string, e *Extra) ([]Locked, map[string]string, error) {
	scratch, err := os.MkdirTemp(r.dir, "resolve-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(scratch)
	environments := slices.Clone(releaseEnvironments)
	unsupported := map[string]string{}
	for {
		if err := os.WriteFile(filepath.Join(scratch, "pyproject.toml"), []byte(extraProject(name, r.short, e, environments)), 0o666); err != nil {
			return nil, nil, err
		}
		out, err := r.uvRun(scratch, 30*time.Minute, "lock")
		if err != nil {
			return nil, nil, err
		}
		if out.code == 0 {
			break
		}
		stderr := strings.Join(strings.Fields(out.stderr), " ")
		m := failedSplit.FindStringSubmatch(stderr)
		if m == nil {
			return nil, nil, fmt.Errorf("uv lock failed: %s", strings.TrimSpace(out.stderr))
		}
		kept := environments[:0]
		dropped := false
		for _, env := range environments {
			if env.marker != m[1] {
				kept = append(kept, env)
				continue
			}
			dropped = true
			reason := "no version of its requirements installs on this platform"
			if all := missingWheel.FindAllStringSubmatch(stderr, -1); len(all) > 0 {
				last := all[len(all)-1]
				reason = last[1] + last[2] + " publishes no wheel for it under Python " + r.short
			}
			for _, target := range env.targets {
				unsupported[target] = reason
			}
		}
		if !dropped || len(kept) == 0 {
			return nil, nil, fmt.Errorf("uv lock failed: %s", strings.TrimSpace(out.stderr))
		}
		environments = kept
	}
	out, err := r.uvRun(scratch, 30*time.Minute, "export", "--frozen", "--format", "pylock.toml", "--no-emit-project", "--no-header", "-o", "pylock.toml")
	if err != nil {
		return nil, nil, err
	}
	if out.code != 0 {
		return nil, nil, fmt.Errorf("uv export failed: %s", strings.TrimSpace(out.stderr))
	}
	text, err := os.ReadFile(filepath.Join(scratch, "pylock.toml"))
	if err != nil {
		return nil, nil, err
	}
	packages, err := readPylock(text, r.short)
	return packages, unsupported, err
}

// Packages a target builds from their sdist, none of their wheels serving it; one with neither marks the target unsupported
func (r *relocking) sourceBuilds(packages []Locked, target string, unsupported map[string]string) ([]Locked, error) {
	_, full, err := pinnedPython(r.h.lock())
	if err != nil {
		return nil, err
	}
	env, err := markerEnv(target, r.short, full)
	if err != nil {
		return nil, err
	}
	var built []Locked
	for _, p := range packages {
		admitted, err := evalMarker(p.Marker, env)
		if err != nil {
			return nil, err
		}
		if !admitted {
			continue
		}
		served := slices.ContainsFunc(p.Wheels, func(w LockedFile) bool { return slices.Contains(wheelTargets(w.URL, r.short), target) })
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
func (r *relocking) lockExtra(name string, e *Extra) error {
	packages, unsupported, err := r.resolve(name, e)
	if err != nil {
		return err
	}
	e.Packages, e.Build = packages, map[string][]string{}
	compiled := map[string]bool{}
	built := map[string]Locked{}
	builtOn := map[string][]string{}
	for _, target := range Targets {
		if _, gone := unsupported[target]; gone {
			continue
		}
		sources, err := r.sourceBuilds(packages, target, unsupported)
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
				e.Build[target] = append(e.Build[target], p.Name)
			}
		}
	}
	e.BuildDeps = nil
	if len(built) > 0 {
		deps, gone, err := r.lockBuildDeps(name, built)
		if err != nil {
			return err
		}
		e.BuildDeps = deps
		for target, reason := range gone {
			if len(builtOn[target]) > 0 {
				unsupported[target] = "the source build of " + strings.Join(builtOn[target], ", ") + " needs " + reason
			}
		}
	}
	for target := range unsupported {
		delete(e.Build, target)
	}
	e.Unsupported = unsupported
	if len(e.Build) == 0 {
		e.Build = nil
	}
	if len(e.Unsupported) == 0 {
		e.Unsupported = nil
	}
	return nil
}

// Hash pinned closure of the build requirements of the source built packages, the builds of that closure's own sdists included
func (r *relocking) lockBuildDeps(name string, built map[string]Locked) ([]Locked, map[string]string, error) {
	requires, probed := map[string]bool{}, map[string]bool{}
	pending := built
	for {
		for _, key := range slices.Sorted(maps.Keys(pending)) {
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
		closure, unsupported, err := r.resolve(name+"-build", &Extra{Requires: slices.Sorted(maps.Keys(requires))})
		if err != nil {
			return nil, nil, err
		}
		pending = map[string]Locked{}
		for _, target := range Targets {
			if _, gone := unsupported[target]; gone {
				continue
			}
			sources, err := r.sourceBuilds(closure, target, unsupported)
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

// Asks a build backend for the requirements of a wheel build, run from the unpacked source tree
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
func (r *relocking) sdistBuildRequires(f LockedFile) ([]string, error) {
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
		var project struct {
			BuildSystem struct {
				Requires    *[]string `toml:"requires"`
				Backend     string    `toml:"build-backend"`
				BackendPath []string  `toml:"backend-path"`
			} `toml:"build-system"`
		}
		if err := toml.Unmarshal(text, &project); err != nil {
			return nil, err
		}
		if project.BuildSystem.Requires != nil {
			requires = *project.BuildSystem.Requires
		}
		backend = cmp.Or(project.BuildSystem.Backend, backend)
		for _, rel := range project.BuildSystem.BackendPath {
			backendPath = append(backendPath, filepath.Join(source, filepath.FromSlash(rel)))
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	venv := filepath.Join(work, "venv")
	uvStep := func(args ...string) error {
		out, err := r.uvRun(work, 30*time.Minute, args...)
		if err != nil {
			return err
		}
		if out.code != 0 {
			return fmt.Errorf("uv %s exited %d: %s", args[0], out.code, strings.TrimSpace(out.stderr))
		}
		return nil
	}
	if err := uvStep("venv", "--python", r.python, venv); err != nil {
		return nil, err
	}
	if len(requires) > 0 {
		if err := uvStep(append([]string{"pip", "install", "--python", venvPython(venv)}, requires...)...); err != nil {
			return nil, err
		}
	}
	shadow, err := r.shadowPath()
	if err != nil {
		return nil, err
	}
	hookEnv := maps.Clone(r.env)
	hookEnv[pathKey(hookEnv)] = filepath.Dir(venvPython(venv)) + string(os.PathListSeparator) + shadow
	out, err := run(r.ctx, command{argv: append([]string{venvPython(venv), "-I", "-c", buildRequiresScript, backend}, backendPath...), dir: source, env: hookEnv,
		timeout: 10 * time.Minute})
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(out.stdout), "\n")
	var dynamic []string
	if out.code != 0 || json.Unmarshal([]byte(lines[len(lines)-1]), &dynamic) != nil {
		return nil, fmt.Errorf("%s get_requires_for_build_wheel exited %d: %s", backend, out.code, strs.Tail(strings.TrimSpace(out.stdout+"\n"+out.stderr), 2000))
	}
	all := append(slices.Clone(requires), dynamic...)
	r.requires[f.URL] = all
	return all, nil
}

// Folder linking every program on PATH but the build tools a backend may fetch, made once per relock
func (r *relocking) shadowPath() (string, error) {
	if r.shadow != "" {
		return r.shadow, nil
	}
	dir := filepath.Join(r.dir, "shadow-path")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	for _, bin := range filepath.SplitList(r.env[pathKey(r.env)]) {
		entries, err := os.ReadDir(bin)
		if err != nil {
			continue
		}
		for _, e := range entries {
			link := filepath.Join(dir, e.Name())
			if indexBuildTools[strings.TrimSuffix(strings.ToLower(e.Name()), ".exe")] || e.IsDir() || fsx.Lexists(link) {
				continue
			}
			if err := os.Symlink(filepath.Join(bin, e.Name()), link); err != nil {
				return "", err
			}
		}
	}
	r.shadow = dir
	return dir, nil
}

// Local copy of an sdist fetched once per relock, its digest checked against the pin
func (r *relocking) fetchSdist(f LockedFile) (string, error) {
	if path, ok := r.sdists[f.URL]; ok {
		return path, nil
	}
	path := filepath.Join(r.dir, fmt.Sprintf("sdist-%d-%s", len(r.sdists), filepath.Base(strings.SplitN(f.URL, "?", 2)[0])))
	if _, err := r.h.Download([]Source{{URL: f.URL, Dest: path, SHA256: f.SHA256}}).Run(r.ctx, nil); err != nil {
		return "", err
	}
	r.sdists[f.URL] = path
	return path, nil
}

// Whether an sdist carries native sources, read from its member names
func sdistCompiles(path string) (bool, error) {
	native := func(name string) bool {
		base := filepath.Base(name)
		return nativeFiles[base] || slices.ContainsFunc(nativeSuffixes, func(s string) bool { return strings.HasSuffix(strings.ToLower(base), s) })
	}
	if strings.HasSuffix(path, ".zip") {
		zr, err := zip.OpenReader(path)
		if err != nil {
			return false, err
		}
		defer zr.Close()
		return slices.ContainsFunc(zr.File, func(f *zip.File) bool { return native(f.Name) }), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return false, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if native(h.Name) {
			return true, nil
		}
	}
}

// Packages of an exported pylock document, each keeping only the wheels some target installs
func readPylock(text []byte, short string) ([]Locked, error) {
	type artifact struct {
		URL    string            `toml:"url"`
		Hashes map[string]string `toml:"hashes"`
	}
	var doc struct {
		Packages []struct {
			Name    string     `toml:"name"`
			Version string     `toml:"version"`
			Marker  string     `toml:"marker"`
			Index   string     `toml:"index"`
			Sdist   *artifact  `toml:"sdist"`
			Archive *artifact  `toml:"archive"`
			Wheels  []artifact `toml:"wheels"`
		} `toml:"packages"`
	}
	if err := toml.Unmarshal(text, &doc); err != nil {
		return nil, err
	}
	file := func(a *artifact) (*LockedFile, error) {
		if a.Hashes["sha256"] == "" {
			return nil, fmt.Errorf("pylock artifact %s has no hashes", a.URL)
		}
		return &LockedFile{URL: a.URL, SHA256: a.Hashes["sha256"]}, nil
	}
	var out []Locked
	for _, row := range doc.Packages {
		p := Locked{Name: row.Name, Version: row.Version, Marker: row.Marker}
		if row.Index != "https://pypi.org/simple" {
			p.Index = row.Index
		}
		if row.Sdist != nil {
			sdist, err := file(row.Sdist)
			if err != nil {
				return nil, err
			}
			p.Sdist = sdist
		}
		if row.Archive != nil {
			f, err := file(row.Archive)
			if err != nil {
				return nil, err
			}
			if strings.HasSuffix(f.URL, ".whl") {
				p.Wheels = append(p.Wheels, *f)
			} else {
				p.Sdist = f
			}
		}
		for i := range row.Wheels {
			f, err := file(&row.Wheels[i])
			if err != nil {
				return nil, err
			}
			if len(wheelTargets(f.URL, short)) > 0 {
				p.Wheels = append(p.Wheels, *f)
			}
		}
		slices.SortFunc(p.Wheels, func(a, b LockedFile) int { return strings.Compare(a.URL, b.URL) })
		out = append(out, p)
	}
	slices.SortStableFunc(out, func(a, b Locked) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}
