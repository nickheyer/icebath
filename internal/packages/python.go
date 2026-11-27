package packages

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

var (
	simpleName     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	generationName = regexp.MustCompile(`^gen-[0-9a-f]{32}$`)
	separators     = regexp.MustCompile(`[-_.]+`)
)

const (
	defaultUVTimeout = 30 * time.Minute
	// Rounds of removing corrupt cached packages before an install gives up
	cacheRounds = 3
)

// Remedies the uv failures name
const (
	retryDownloadRemedy = "check that this machine reaches the address in the error, through its proxy when it uses one, then run the same command again"
	timeoutHint         = "uv timed out. If your network needs a package mirror, set index-url in pip.conf (bridged to uv automatically) or UV_INDEX_URL; " +
		"raise UV_HTTP_TIMEOUT for slow links."
	undatedHint = "the package index did not report upload times, which locking icebath's dependencies needs for its 3-day release-age check. " +
		"Use https://pypi.org/simple or a mirror that serves upload times (pip.conf index-url / UV_INDEX_URL), then retry."
	compilerRemedy = "%s: install a C/C++ compiler (Debian/Ubuntu/WSL: `sudo apt install build-essential`; Fedora: `sudo dnf install gcc gcc-c++`; " +
		"macOS: `xcode-select --install`) or point CC/CXX at one, then retry"
)

// Name a dependency set or extra is matched by: separator runs folded to one dash, lower case
func extraKey(name string) string {
	return strings.ToLower(separators.ReplaceAllString(name, "-"))
}

// Pinned uv and Python, installed first when realize is set; found false when they are not there and realize is not
func (h *Packages) toolchain(ctx context.Context, cfg *config.Config, realize, explicit bool, r Reporter) (string, string, bool, error) {
	if realize {
		for _, name := range []string{Python, UV} {
			bar := newProgress(r, name)
			_, err := h.Ensure(ctx, cfg, name, Ensuring{Explicit: explicit, Report: r, Step: func(_, stage string, done, total int64, label string) {
				bar.report(stage, done, total, label)
			}})
			bar.finish()
			if err != nil {
				return "", "", false, err
			}
			if bar.fetched {
				reporter(r).Line("  ✓ " + name)
			}
		}
	}
	binaries := map[string]string{}
	for _, name := range []string{UV, Python} {
		p, err := h.Package(name)
		if err != nil {
			return "", "", false, err
		}
		got, err := h.installed(p, false)
		if err != nil {
			return "", "", false, err
		}
		if got == nil || got.Binary == "" || !fsx.IsFile(got.Binary) {
			if !realize {
				return "", "", false, nil
			}
			return "", "", false, Failure(name, "pinned tool is unavailable", "run `icebath pm install`")
		}
		binaries[name] = got.Binary
	}
	return binaries[UV], binaries[Python], true, nil
}

// One environment uv builds and installs into on the package manager's toolchain
type pyenv struct {
	uv     string
	python string
	dest   string
	cache  string
	env    map[string]string
	report Reporter
}

// Environment at dest on the pinned toolchain, the pinned interpreter given build defaults this machine can run
func (h *Packages) pyenv(ctx context.Context, cfg *config.Config, dest string, base map[string]string, explicit bool, r Reporter) (*pyenv, error) {
	uvBin, python, found, err := h.toolchain(ctx, cfg, true, explicit, r)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, Failure("venv", "PM's pinned toolchain is unavailable", "")
	}
	env, err := uvBase(base)
	if err != nil {
		return nil, err
	}
	for k, v := range buildDefaults(python, env) {
		if _, set := env[k]; !set {
			env[k] = v
		}
	}
	return &pyenv{uv: uvBin, python: python, dest: dest, cache: h.uvCache(), env: env, report: r}, nil
}

// Environment of a uv child: no ambient interpreter, project or cache choice, the user's index and transport settings kept and pip's
// translated
func uvBase(base map[string]string) (map[string]string, error) {
	source := base
	if source == nil {
		source = environ()
	}
	env := map[string]string{}
	for k, v := range source {
		if !strings.HasPrefix(k, "PYTHON") && k != "VIRTUAL_ENV" && (!strings.HasPrefix(k, "UV_") || forwarded(k)) {
			env[k] = v
		}
	}
	if base == nil {
		bridged, err := bridgedIndex(environ())
		if err != nil {
			return nil, err
		}
		maps.Copy(env, bridged)
	}
	return env, nil
}

// uv settings that reach uv: where packages come from and how they travel
var uvForwarded = map[string]bool{
	"UV_INDEX_URL": true, "UV_EXTRA_INDEX_URL": true, "UV_DEFAULT_INDEX": true, "UV_INDEX": true, "UV_NO_INDEX": true, "UV_FIND_LINKS": true,
	"UV_INDEX_STRATEGY": true, "UV_KEYRING_PROVIDER": true, "UV_NATIVE_TLS": true, "UV_INSECURE_HOST": true, "UV_HTTP_TIMEOUT": true,
}

func forwarded(key string) bool { return uvForwarded[key] || strings.HasPrefix(key, "UV_INDEX_") }

// Settings that redirect where packages resolve from, left out of a relock so it resolves from the public index
func redirect(key string) bool {
	switch key {
	case "UV_NATIVE_TLS", "UV_INSECURE_HOST", "UV_HTTP_TIMEOUT":
		return false
	}
	return forwarded(key)
}

// uv's index settings from the ambient ones, pip's PIP_INDEX_URL and pip.conf index-url standing in when uv has none
func bridgedIndex(ambient map[string]string) (map[string]string, error) {
	settings := map[string]string{}
	for k, v := range ambient {
		if forwarded(k) {
			settings[k] = v
		}
	}
	if settings["UV_INDEX_URL"] == "" && settings["UV_DEFAULT_INDEX"] == "" && settings["UV_INDEX"] == "" {
		index := strings.TrimSpace(ambient["PIP_INDEX_URL"])
		if index == "" {
			var err error
			if index, err = pipConfIndex(ambient); err != nil {
				return nil, err
			}
		}
		if index != "" {
			settings["UV_INDEX_URL"] = index
		}
	}
	for _, pair := range [][2]string{{"PIP_EXTRA_INDEX_URL", "UV_EXTRA_INDEX_URL"}, {"PIP_TRUSTED_HOST", "UV_INSECURE_HOST"}} {
		if value := strings.TrimSpace(ambient[pair[0]]); value != "" && settings[pair[1]] == "" {
			settings[pair[1]] = value
		}
	}
	return settings, nil
}

// index-url of the global section of pip's config files, later files winning
func pipConfIndex(env map[string]string) (string, error) {
	home, _ := os.UserHomeDir()
	explicit := env["PIP_CONFIG_FILE"]
	if explicit == os.DevNull {
		return "", nil
	}
	var files []string
	switch runtime.GOOS {
	case "windows":
		programData := cmp.Or(env["ProgramData"], env["PROGRAMDATA"], `C:\ProgramData`)
		appData := cmp.Or(env["APPDATA"], filepath.Join(home, "AppData", "Roaming"))
		files = []string{filepath.Join(programData, "pip", "pip.ini"), filepath.Join(home, "pip", "pip.ini"), filepath.Join(appData, "pip", "pip.ini")}
	case "darwin":
		support := filepath.Join(home, "Library", "Application Support", "pip")
		if !fsx.IsDir(support) {
			support = filepath.Join(home, ".config", "pip")
		}
		files = []string{"/Library/Application Support/pip/pip.conf", filepath.Join(home, ".pip", "pip.conf"), filepath.Join(support, "pip.conf")}
	default:
		for _, dir := range strings.Split(cmp.Or(env["XDG_CONFIG_DIRS"], "/etc/xdg"), ":") {
			if dir != "" {
				files = append(files, filepath.Join(dir, "pip", "pip.conf"))
			}
		}
		files = append(files, "/etc/pip.conf", filepath.Join(home, ".pip", "pip.conf"),
			filepath.Join(cmp.Or(env["XDG_CONFIG_HOME"], filepath.Join(home, ".config")), "pip", "pip.conf"))
	}
	if explicit != "" {
		if fsx.IsFile(explicit) {
			files = slices.DeleteFunc(files, func(f string) bool { return strings.HasPrefix(f, home) })
		}
		files = append(files, explicit)
	}
	index := ""
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		section := ""
		scanner := bufio.NewScanner(bytes.NewReader(data))
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			switch {
			case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
			case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
				section = line[1 : len(line)-1]
			case section == "global":
				if key, value, ok := strings.Cut(line, "="); ok && strings.EqualFold(strings.TrimSpace(key), "index-url") {
					index = strings.TrimSpace(value)
				} else if key, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(key), "index-url") {
					index = strings.TrimSpace(value)
				}
			}
		}
	}
	return index, nil
}

var gccFamily = map[string]string{"clang": "gcc", "clang++": "g++"}

// Build variables the pinned interpreter recorded at its own build that this machine lacks, replaced by what it has
func buildDefaults(python string, env map[string]string) map[string]string {
	switch runtime.GOOS {
	case "darwin":
		return map[string]string{"AR": "/usr/bin/ar"}
	case "linux":
	default:
		return map[string]string{}
	}
	out, err := run(context.Background(), command{argv: []string{python, "-I", "-c", "import json, sysconfig; print(json.dumps({k: sysconfig.get_config_var(k) or '' " +
		"for k in ('CC', 'CXX', 'LDSHARED', 'LDCXXSHARED', 'AR')}))"}, timeout: 30 * time.Second})
	recorded := map[string]string{}
	if err != nil || out.code != 0 || json.Unmarshal([]byte(out.stdout), &recorded) != nil {
		return map[string]string{}
	}
	path := env[pathKey(env)]
	defaults := map[string]string{}
	for _, pair := range [][2]string{{"CC", "LDSHARED"}, {"CXX", "LDCXXSHARED"}} {
		compiler, linker := pair[0], pair[1]
		recordedCommand := recorded[compiler]
		program, _, _ := strings.Cut(recordedCommand, " ")
		substitute, known := gccFamily[program]
		if _, set := env[compiler]; set || !known || Which(program, path) != "" || Which(substitute, path) == "" {
			continue
		}
		defaults[compiler] = substitute + recordedCommand[len(program):]
		if first, _, _ := strings.Cut(recorded[linker], " "); first == program {
			if _, set := env[linker]; !set {
				defaults[linker] = substitute + recorded[linker][len(program):]
			}
		}
	}
	if ar := recorded["AR"]; ar != "" && Which(ar, path) == "" && Which("ar", path) != "" {
		defaults["AR"] = "ar"
	}
	return defaults
}

// What a uv run said and how it ended
type uvResult struct {
	code   int
	output string
}

// Labels of the uv steps a person watches
var uvLabels = map[string]string{"sync": "Installing Python dependencies", "lock": "Resolving Python dependencies", "pip install": "Installing Python packages",
	"cache": "Pruning the uv cache"}

// Runs one uv command isolated from the user's uv config, its output behind a status line, streamed as it is when verbose
func (e *pyenv) run(ctx context.Context, args []string, dir string, timeout time.Duration) (uvResult, error) {
	env := maps.Clone(e.env)
	env["UV_PYTHON"], env["UV_PROJECT_ENVIRONMENT"], env["UV_CACHE_DIR"], env["UV_PYTHON_DOWNLOADS"] = e.python, e.dest, e.cache, "never"
	if _, set := env["CMAKE_POLICY_VERSION_MINIMUM"]; !set {
		env["CMAKE_POLICY_VERSION_MINIMUM"] = "3.5"
	}
	isolated, err := os.MkdirTemp("", "pm-uv-config-")
	if err != nil {
		return uvResult{}, err
	}
	defer os.RemoveAll(isolated)
	env["XDG_CONFIG_HOME"], env["XDG_CONFIG_DIRS"] = isolated, isolated
	argv := append([]string{e.uv}, args...)
	label := uvLabels[args[0]]
	if len(args) > 1 {
		label = cmp.Or(uvLabels[args[0]+" "+args[1]], label)
	}
	c := command{argv: argv, dir: dir, env: env, timeout: timeout}
	var out strings.Builder
	var tail *liveTail
	partial := ""
	switch {
	case e.report != nil && verbose():
		c.argv = append(c.argv, "--verbose")
		if _, set := env["RUST_LOG"]; !set {
			env["RUST_LOG"] = "uv_build_frontend=debug"
		}
		c.stream = func(text string) {
			out.WriteString(text)
			lines := strings.Split(partial+text, "\n")
			partial = lines[len(lines)-1]
			for _, line := range lines[:len(lines)-1] {
				e.report.Line(strings.TrimRight(line, "\r"))
			}
		}
	case e.report != nil:
		tail = newLiveTail(e.report, label, "  ")
		c.stream = func(text string) { out.WriteString(text); tail.write(text) }
	}
	r, err := run(ctx, c)
	if partial != "" {
		e.report.Line(partial)
	}
	if err == errTimedOut {
		if tail != nil {
			tail.close(false)
		}
		return uvResult{}, Failure("venv", fmt.Sprintf("uv %s timed out after %ds", args[0], int(timeout.Seconds())), timeoutHint)
	}
	if err != nil {
		if tail != nil {
			tail.close(false)
		}
		return uvResult{}, err
	}
	if c.stream == nil {
		out.WriteString(r.stderr)
		out.WriteString(r.stdout)
	}
	result := uvResult{code: r.code, output: strs.Tail(out.String(), 20000)}
	if tail != nil {
		if slices.Contains(args, "--compile-bytecode") && bytecodeFailure(result) {
			tail.discard()
		} else {
			tail.close(r.code == 0)
		}
	}
	return result, nil
}

// A failure only of the optional bytecode compile after an install
func bytecodeFailure(r uvResult) bool {
	return r.code != 0 && strings.Contains(strings.ToLower(r.output), "failed to bytecode-compile")
}

// Interpreter inside the environment
func (e *pyenv) executable() string { return venvPython(e.dest) }

// Interpreter of a venv laid out for this platform
func venvPython(venv string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(venv, "Scripts", "python.exe")
	}
	return filepath.Join(venv, "bin", "python")
}

// Creates the venv at its final place, which nothing moves later
func (e *pyenv) create(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(e.dest), 0o777); err != nil {
		return err
	}
	r, err := e.run(ctx, []string{"venv", "--relocatable", "--no-project", "--no-config", "--python", e.python, e.dest}, filepath.Dir(e.dest), 120*time.Second)
	if err != nil {
		return err
	}
	if r.code != 0 {
		return Failure("venv", "uv venv failed: "+strs.Tail(r.output, 600), "")
	}
	return nil
}

// Resolves a project's lock, every pin moved to its newest allowed when upgrading
func (e *pyenv) lock(ctx context.Context, project string, upgrade bool, timeout time.Duration) error {
	args := []string{"lock", "--python", e.python}
	if upgrade {
		args = append(args, "--upgrade")
	}
	r, err := e.run(ctx, args, project, cmp.Or(timeout, defaultUVTimeout))
	if err != nil {
		return err
	}
	if r.code != 0 {
		return classify("lock", r.code, r.output)
	}
	return nil
}

// Installs a locked project into the environment, once more without bytecode compiling when only that failed
func (e *pyenv) sync(ctx context.Context, project string, timeout time.Duration) error {
	args := []string{"sync", "--frozen", "--all-packages", "--python", e.python, "--compile-bytecode"}
	r, err := e.recovering(ctx, "sync", func() (uvResult, error) {
		r, err := e.run(ctx, args, project, cmp.Or(timeout, defaultUVTimeout))
		if err != nil || !bytecodeFailure(r) {
			return r, err
		}
		reporter(e.report).Line("Bytecode compile failed; retrying the install without it. Modules will compile on first import.")
		return e.run(ctx, slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "--compile-bytecode" }), project, cmp.Or(timeout, defaultUVTimeout))
	})
	if err != nil {
		return err
	}
	if r.code != 0 {
		return classify("sync", r.code, r.output)
	}
	return nil
}

// Checks the installed dependencies agree with each other
func (e *pyenv) check(ctx context.Context) error {
	quiet := *e
	quiet.report = nil
	r, err := quiet.run(ctx, []string{"pip", "check", "--no-config", "--color", "never", "--python", e.executable()}, filepath.Dir(e.dest), 60*time.Second)
	if err != nil {
		return err
	}
	if r.code != 0 {
		return Failure("venv", "dependency validation failed: "+strs.Tail(r.output, 600), "")
	}
	return nil
}

var (
	resolverMarkers = []string{"no solution found", "conflicting requirements", "conflicting urls", "because only the following versions",
		"and your pyproject depends on"}
	networkMarkers   = []string{"failed to fetch", "error sending request"}
	missingCompilers = []*regexp.Regexp{
		regexp.MustCompile(`No such file or directory: '([^']+)'`),
		regexp.MustCompile(`command '([^']+)' failed: No such file or directory`),
		regexp.MustCompile(`No CMAKE_(?:C|CXX)_COMPILER could be found`),
	}
	compilerName = regexp.MustCompile(`^(?:clang(?:\+\+)?|g?cc|g\+\+|c\+\+)(?:-[0-9.]+)?$`)
)

// A resolution that has no solution
type ConflictError struct{ *InstallError }

func (e *ConflictError) Unwrap() error { return e.InstallError }

// Install error of a failed uv step: no solution, a build that failed, the network, or the index missing upload times
func classify(stage string, code int, output string) error {
	tail := strings.TrimSpace(output)
	if trimmed := strs.Tail(tail, 600); trimmed != tail {
		if _, rest, ok := strings.Cut(trimmed, "\n"); ok {
			trimmed = rest
		}
		tail = trimmed
	}
	cause := fmt.Sprintf("uv %s exited %d: %s", stage, code, tail)
	lowered := strings.ToLower(output)
	contains := func(markers []string) bool {
		return slices.ContainsFunc(markers, func(m string) bool { return strings.Contains(lowered, m) })
	}
	switch {
	case strings.Contains(lowered, "has no publish time") ||
		(strings.Contains(lowered, "is missing an upload date") && strings.Contains(lowered, "was filtered by `exclude-newer")):
		return Failure("venv", cause, undatedHint)
	case contains(resolverMarkers):
		return &ConflictError{Failure("venv", cause, "")}
	case strings.Contains(lowered, "the build backend returned an error"):
		return Failure("venv", cause, compilerHint(output))
	case contains(networkMarkers):
		return Failure("venv", cause, retryDownloadRemedy)
	}
	return Failure("venv", cause, "")
}

// Remedy of a build that found no compiler, empty when that is not what failed
func compilerHint(output string) string {
	for _, pattern := range missingCompilers {
		for _, m := range pattern.FindAllStringSubmatch(output, -1) {
			if len(m) == 1 {
				return fmt.Sprintf(compilerRemedy, "this package compiles native code and no compiler was found")
			}
			if compilerName.MatchString(filepath.Base(m[1])) {
				return fmt.Sprintf(compilerRemedy, "the build needs `"+m[1]+"`, which is not on PATH")
			}
		}
	}
	return ""
}

// Runs an install and again after removing from the cache the packages whose cached copy it found corrupt
func (e *pyenv) recovering(ctx context.Context, stage string, attempt func() (uvResult, error)) (uvResult, error) {
	purged := map[string]bool{}
	r, err := attempt()
	if err != nil {
		return r, err
	}
	for range cacheRounds {
		if r.code == 0 {
			return r, nil
		}
		failed := failedCached(r.output)
		if len(failed) == 0 || subset(failed, purged) {
			break
		}
		union := maps.Clone(failed)
		for name := range corruptCached(e.cache) {
			union[name] = true
		}
		for name := range purged {
			delete(union, name)
		}
		names := slices.Sorted(maps.Keys(union))
		reporter(e.report).Line(fmt.Sprintf("The cached copy of %s is corrupt; removing it from the uv cache and retrying the install.", strings.Join(names, ", ")))
		if err := e.purge(ctx, names); err != nil {
			if ie, ok := errors.AsType[*InstallError](err); ok {
				return r, Failure(ie.Package, fmt.Sprintf("%s; uv %s had failed with: %s", ie.Cause, stage, strs.Tail(strings.TrimSpace(r.output), 600)), ie.Remedy)
			}
			return r, err
		}
		for _, name := range names {
			purged[name] = true
		}
		if r, err = attempt(); err != nil {
			return r, err
		}
	}
	if r.code == 0 || len(purged) == 0 {
		return r, nil
	}
	if still := failedCached(r.output); len(still) > 0 {
		remedy := "run `icebath pm doctor --repair`, then retry"
		if subset(still, purged) {
			remedy = "the package source is serving a broken wheel; retry later or pin another version"
		}
		return r, Failure("venv", fmt.Sprintf("uv %s exited %d: the cached copy of %s is still invalid after removing %s from the uv cache at %s: %s", stage, r.code,
			strings.Join(slices.Sorted(maps.Keys(still)), ", "), strings.Join(slices.Sorted(maps.Keys(purged)), ", "), e.cache,
			strs.Tail(strings.TrimSpace(r.output), 600)), remedy)
	}
	return r, nil
}

func subset(a, b map[string]bool) bool {
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// Removes packages from the uv cache through uv, then deletes what uv left that still fails its checks
func (e *pyenv) purge(ctx context.Context, names []string) error {
	quiet := *e
	quiet.report = nil
	r, err := quiet.run(ctx, append([]string{"cache", "clean"}, names...), filepath.Dir(e.cache), 900*time.Second)
	if err != nil {
		if ie, ok := errors.AsType[*InstallError](err); ok {
			return Failure("uv", fmt.Sprintf("could not clean the uv cache at %s: %s", e.cache, ie.Cause), "stop other icebath installs and retry")
		}
		return err
	}
	if r.code != 0 {
		return Failure("uv", fmt.Sprintf("could not remove %s from the uv cache at %s: %s", strings.Join(names, ", "), e.cache, strs.Tail(r.output, 600)), "")
	}
	return removeCorrupt(e.cache, names)
}

// Selection of a generation root: the generation in use, the digest of what built it, and what it holds
type selection map[string]any

// Selection of a root, empty when none was made, refusing a damaged one
func selected(root string) (selection, error) {
	data, err := os.ReadFile(filepath.Join(root, "active.json"))
	if os.IsNotExist(err) {
		return selection{}, nil
	}
	unreadable := Failure("venv", "cannot read environment selection: "+root, "")
	if err != nil {
		return nil, unreadable
	}
	var record selection
	if json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &record) != nil {
		return nil, unreadable
	}
	if generation, ok := record["generation"].(string); !ok || !generationName.MatchString(generation) {
		return nil, Failure("venv", "invalid environment selection: "+root, "")
	}
	return record, nil
}

// Interpreter of the selected generation of a root, false when none is selected or it is gone
func selectedPython(name, root string) (string, bool, error) {
	record, err := selected(root)
	if err != nil || len(record) == 0 {
		return "", false, err
	}
	venv := filepath.Join(root, record["generation"].(string), "venv")
	if !fsx.Within(resolved(venv), resolved(root)) {
		return "", false, Failure(name, "selected environment escapes its root", "")
	}
	python := venvPython(venv)
	return python, fsx.IsFile(python) && fsx.IsFile(filepath.Join(venv, "pyvenv.cfg")), nil
}

// Program a named side environment of the profile installed beside the interpreter of its selected generation, never installing; false
// when the environment or the program is not there
func (h *Packages) PythonTool(cfg *config.Config, name, executable string) (string, bool, error) {
	root, err := environmentRoot(cfg, name)
	if err != nil {
		return "", false, err
	}
	python, found, err := selectedPython(name, root)
	if err != nil || !found {
		return "", false, err
	}
	return tool(python, executable)
}

// Program an environment installed beside its interpreter, false when it has none of the name
func tool(python, name string) (string, bool, error) {
	if !simpleName.MatchString(name) {
		return "", false, invalid("tool executable must be a filename, not a path")
	}
	suffixes := []string{""}
	if runtime.GOOS == "windows" {
		suffixes = []string{".exe", ".cmd", ""}
	}
	for _, suffix := range suffixes {
		if candidate := filepath.Join(filepath.Dir(python), name+suffix); fsx.IsFile(candidate) {
			return candidate, true, nil
		}
	}
	return "", false, nil
}

// Selects the generation of a root whose inputs match, building one at its final place only when none does
func (h *Packages) generation(ctx context.Context, cfg *config.Config, name, root string, inputs, record map[string]any,
	build func(generation, python string) (string, error), explicit bool, executable string, r Reporter) (string, error) {
	l := h.lock()
	target := CurrentTarget()
	full := maps.Clone(inputs)
	full["python"], _ = l.version(Python)
	full["target"] = target
	var digests []string
	for _, a := range l.artifacts(Python, target) {
		digests = append(digests, a.SHA256)
	}
	full["artifacts"] = digests
	identity := func(python string) string {
		withInterpreter := maps.Clone(full)
		withInterpreter["interpreter"] = python
		data, _ := json.Marshal(withInterpreter)
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:])
	}
	current := func(python string) (string, bool, error) {
		if python == "" {
			return "", false, nil
		}
		record, err := selected(root)
		if err != nil {
			return "", false, err
		}
		got, ok, err := selectedPython(name, root)
		if err != nil || !ok || record["inputs"] != identity(python) {
			return "", false, err
		}
		if executable != "" {
			if _, has, err := tool(got, executable); err != nil || !has {
				return "", false, err
			}
		}
		return got, true, nil
	}
	base := func() (string, error) {
		_, python, found, err := h.toolchain(ctx, cfg, false, false, nil)
		if err != nil || !found {
			return "", err
		}
		return resolved(python), nil
	}
	python, err := base()
	if err != nil {
		return "", err
	}
	if existing, ok, err := current(python); err != nil || ok {
		return existing, err
	}
	if !explicit && !h.LazyAllowed(cfg) {
		return "", refuseLazy(ctx, name, "isolated Python environment is missing or outdated")
	}
	release, err := fsx.Lock(ctx, filepath.Join(root, ".install.lock"), -1)
	if err != nil {
		return "", err
	}
	defer release()
	if python, err = base(); err != nil {
		return "", err
	}
	if existing, ok, err := current(python); err != nil || ok {
		return existing, err
	}
	if python == "" {
		_, bin, _, err := h.toolchain(ctx, cfg, true, explicit, r)
		if err != nil {
			return "", err
		}
		python = resolved(bin)
	}
	generation := filepath.Join(root, "gen-"+strs.Random(16))
	if err := os.Mkdir(generation, 0o777); err != nil {
		return "", err
	}
	got, err := func() (string, error) {
		got, err := build(generation, python)
		if err != nil {
			return "", err
		}
		if executable != "" {
			if _, has, err := tool(got, executable); err != nil {
				return "", err
			} else if !has {
				return "", Failure(name, "installed requirements do not provide "+executable, "")
			}
		}
		selection := map[string]any{"generation": filepath.Base(generation), "inputs": identity(python)}
		maps.Copy(selection, record)
		return got, writeJSON(filepath.Join(root, "active.json"), selection)
	}()
	if err != nil {
		removeQuiet(generation)
		return "", err
	}
	return got, nil
}

// A request for an environment that can't be made as asked
type invalid string

func (e invalid) Error() string { return string(e) }

// What a side environment is to hold and where
type EnvironmentOptions struct {
	// Simple name of the environment, a folder under the profile's environments
	Name string
	// Bounded requirements to install, such as "pypdf==6.21.2"
	Requirements []string
	// A person asked for it, so the lazy install setting does not apply
	Explicit bool
	// Longest the resolution and install may take, half an hour when zero
	Timeout time.Duration
	// Program the requirements must install beside the interpreter, checked before the generation is selected
	Executable string
	Report     Reporter
}

// Folder of a profile's side environments
func environmentsDir(cfg *config.Config) string {
	return filepath.Join(cfg.ProfileDir(cfg.Profile()), "environments")
}

// Root of a named side environment
func environmentRoot(cfg *config.Config, name string) (string, error) {
	if !simpleName.MatchString(name) {
		return "", invalid("environment name must be a simple package name")
	}
	return filepath.Join(environmentsDir(cfg), name), nil
}

// Makes a named isolated environment of the profile hold the requirements, reusing the generation that already does, and returns its
// interpreter
func (h *Packages) Environment(ctx context.Context, cfg *config.Config, opts EnvironmentOptions) (string, error) {
	root, err := environmentRoot(cfg, opts.Name)
	if err != nil {
		return "", err
	}
	if len(opts.Requirements) == 0 {
		return "", invalid("an isolated environment needs a nonempty sequence of requirements")
	}
	var requirements []string
	for _, req := range opts.Requirements {
		if req = strings.Join(strings.Fields(req), " "); req != "" && !slices.Contains(requirements, req) {
			requirements = append(requirements, req)
		}
	}
	slices.Sort(requirements)
	if opts.Executable != "" {
		if _, _, err := tool(filepath.Join("unused", "python"), opts.Executable); err != nil {
			return "", err
		}
	}
	build := func(generation, python string) (string, error) {
		deps, _ := json.Marshal(requirements)
		manifest := "[project]\nname = \"icebath-side-environment\"\nversion = \"0\"\nrequires-python = \">=3.11\"\ndependencies = " + string(deps) +
			"\n[tool.uv]\npackage = false\n"
		if err := os.WriteFile(filepath.Join(generation, "pyproject.toml"), []byte(manifest), 0o666); err != nil {
			return "", err
		}
		if previous, err := selected(root); err == nil && len(previous) > 0 {
			if seed := filepath.Join(root, previous["generation"].(string), "uv.lock"); fsx.IsFile(seed) {
				if data, err := os.ReadFile(seed); err == nil {
					_ = os.WriteFile(filepath.Join(generation, "uv.lock"), data, 0o666)
				}
			}
		}
		return h.buildProject(ctx, cfg, generation, python, opts.Explicit, opts.Timeout, false, opts.Report)
	}
	inputs := map[string]any{"requirements": requirements}
	return h.generation(ctx, cfg, opts.Name, root, inputs, maps.Clone(inputs), build, opts.Explicit, opts.Executable, opts.Report)
}

// Builds the project of a generation into its venv: resolved, synced and checked, the venv removed again when any step fails
func (h *Packages) buildProject(ctx context.Context, cfg *config.Config, project, python string, explicit bool, timeout time.Duration, upgrade bool,
	r Reporter) (string, error) {
	venv := filepath.Join(project, "venv")
	if fsx.Lexists(venv) {
		return "", fmt.Errorf("environment destination already exists: %s", venv)
	}
	e, err := h.pyenv(ctx, cfg, venv, nil, explicit, r)
	if err != nil {
		return "", err
	}
	e.python = python
	err = func() error {
		if err := e.create(ctx); err != nil {
			return err
		}
		if err := e.lock(ctx, project, upgrade, timeout); err != nil {
			return err
		}
		if err := e.sync(ctx, project, timeout); err != nil {
			return err
		}
		return e.check(ctx)
	}()
	if err != nil {
		removeQuiet(venv)
		return "", err
	}
	return e.executable(), nil
}

// Whether a venv still runs: its interpreter, its config and the interpreter it was made from are there
func healthy(venv string) bool {
	if !fsx.IsFile(venvPython(venv)) {
		return false
	}
	data, err := os.ReadFile(filepath.Join(venv, "pyvenv.cfg"))
	if err != nil {
		return false
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		if key, value, ok := strings.Cut(scanner.Text(), "="); ok && strings.TrimSpace(key) == "home" {
			return fsx.IsDir(strings.TrimSpace(value))
		}
	}
	return true
}
