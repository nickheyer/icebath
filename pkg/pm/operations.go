package pm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"

	"github.com/nickheyer/icebath/pkg/home"
)

var (
	simplePackageName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	generationName    = regexp.MustCompile(`^gen-[0-9a-f]{32}$`)
)

// DestinationExistsError is a build destination that already exists, a FileExistsError in Python
type DestinationExistsError struct{ Path string }

func (e *DestinationExistsError) Error() string {
	return "environment destination already exists: " + e.Path
}

func (e *DestinationExistsError) Is(target error) bool { return target == fs.ErrExist }

// Reports whether a path exists or is a symlink, dangling or not
func existsOrLink(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// Refuses an implicit dependency operation when lazy installs are disabled
func requireInstallAllowed(ctx context.Context, explicit bool) error {
	if !explicit && !LazyInstallsAllowedFor(ctx) {
		return RefuseLazyFor(ctx, "venv", "Python dependency operation requires an explicit request")
	}
	return nil
}

// BuildEnvironmentOptions are build_environment's keywords, nil Env for None and Frozen true for Python's default
type BuildEnvironmentOptions struct {
	Source           string
	Out              string
	Python           string
	Cache            string
	Env              map[string]string
	Extras           []string
	Groups           []string
	AllExtras        bool
	NoInstallProject bool
	Frozen           bool
	Sealed           bool
	Offline          bool
	Explicit         bool
	Timeout          time.Duration
}

// Builds and validates a fresh environment destination from a project, never overwriting an existing tree
func buildEnvironment(ctx context.Context, opts BuildEnvironmentOptions) (string, error) {
	source, out := absoluteUnresolved(opts.Source), absoluteUnresolved(opts.Out)
	if !isRegularFile(filepath.Join(source, "pyproject.toml")) {
		return "", NewInstallError("venv", "project manifest is missing: "+source, "")
	}
	lock := filepath.Join(source, "uv.lock")
	if opts.Frozen && !isRegularFile(lock) {
		return "", NewInstallError("venv", "frozen build requires a lock: "+lock, "")
	}
	if existsOrLink(out) {
		return "", &DestinationExistsError{Path: out}
	}
	if err := requireInstallAllowed(ctx, opts.Explicit); err != nil {
		return "", err
	}
	env := opts.Env
	if env == nil {
		var err error
		if env, err = sourceBuildEnvironment(ctx, source); err != nil {
			return "", err
		}
	}
	environment, err := ManagedEnvironment(ctx, out, ManagedEnvironmentOptions{Python: opts.Python, Cache: opts.Cache, Env: env,
		Offline: opts.Offline, Explicit: opts.Explicit, Output: os.Stderr})
	if err != nil {
		return "", err
	}
	err = freshBuild(ctx, environment, opts.Sealed, lock, func() error {
		return environment.Sync(ctx, source, SyncOptions{Extras: opts.Extras, Groups: opts.Groups, AllExtras: opts.AllExtras,
			NoInstallProject: opts.NoInstallProject, Resolve: !opts.Frozen, Timeout: opts.Timeout})
	})
	if err != nil {
		return "", err
	}
	return environment.Executable(), nil
}

// LockProjectOptions are lock_project's keywords, nil Env for None
type LockProjectOptions struct {
	Source   string
	Upgrade  bool
	Python   string
	Env      map[string]string
	Cache    string
	Offline  bool
	Explicit bool
}

// Refreshes a project's resolution without creating or selecting a venv
func lockProject(ctx context.Context, opts LockProjectOptions) error {
	source := absoluteUnresolved(opts.Source)
	if !isRegularFile(filepath.Join(source, "pyproject.toml")) {
		return NewInstallError("venv", "project manifest is missing: "+source, "")
	}
	if err := requireInstallAllowed(ctx, opts.Explicit); err != nil {
		return err
	}
	environment, err := ManagedEnvironment(ctx, filepath.Join(source, ".venv"), ManagedEnvironmentOptions{Python: opts.Python,
		Cache: opts.Cache, Env: opts.Env, Offline: opts.Offline, Explicit: opts.Explicit, Output: os.Stderr})
	if err != nil {
		return err
	}
	return environment.Lock(ctx, source, opts.Upgrade, 0)
}

// StageManagerRuntimeOptions are stage_manager_runtime's keywords
type StageManagerRuntimeOptions struct {
	Python      string
	Destination string
	Project     string
	Offline     bool
	Wheelhouse  string
	Cache       string
}

// Stages a manager runtime's independent dependency graph with tools staged beforehand
func stageManagerRuntime(ctx context.Context, opts StageManagerRuntimeOptions) (string, error) {
	destination, python := absoluteUnresolved(opts.Destination), absoluteUnresolved(opts.Python)
	if existsOrLink(destination) {
		return "", &DestinationExistsError{Path: destination}
	}
	uv, _, found, err := uvToolchain(ctx, false, false)
	if err != nil {
		return "", err
	}
	if !found {
		return "", NewInstallError("pm-runtime", "stage pinned tools before building the manager runtime", "")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o777); err != nil {
		return "", err
	}
	if err := os.Mkdir(destination, 0o777); err != nil {
		return "", err
	}
	executable, err := StageRuntime(ctx, uv, python, destination, StageRuntimeOptions{Project: opts.Project, Offline: opts.Offline,
		Wheelhouse: opts.Wheelhouse, Cache: opts.Cache})
	if err == nil {
		err = writeLockDocument(filepath.Join(destination, "pm-runtime.json"), map[string]any{})
	}
	if err != nil {
		removeTreeQuiet(destination)
		return "", err
	}
	return executable, nil
}

// Root of a named side environment, an explicit root or the home's environments directory
func environmentRoot(ctx context.Context, name, root string) (string, error) {
	if !simplePackageName.MatchString(name) {
		return "", &PyError{Class: "ValueError", Message: "environment name must be a simple package name"}
	}
	if root != "" {
		return absoluteUnresolved(root), nil
	}
	return filepath.Join(home.Dir(ctx), "environments", name), nil
}

// Selection record of a side environment root, empty when none was made
func environmentSelection(root string) (map[string]any, error) {
	data, err := os.ReadFile(filepath.Join(root, "active.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil
	}
	unreadable := NewInstallError("venv", "cannot read environment selection: "+root, "")
	if err != nil {
		return nil, unreadable
	}
	data = trimBOM(data)
	if utf8Strict(data) != nil {
		return nil, unreadable
	}
	v, err := loadJSON(data)
	if err != nil {
		return nil, unreadable
	}
	record, ok := v.(map[string]any)
	generation, isString := record["generation"].(string)
	if !ok || !isString || !generationName.MatchString(generation) {
		return nil, NewInstallError("venv", "invalid environment selection: "+root, "")
	}
	return record, nil
}

// Selected interpreter of a named side environment without acquiring tools or writing state, ok false when absent
func EnvironmentPython(name, root string) (string, bool, error) {
	return environmentPython(context.Background(), name, root)
}

// Selected interpreter of a side environment resolved against the context's home
func environmentPython(ctx context.Context, name, root string) (string, bool, error) {
	root, err := environmentRoot(ctx, name, root)
	if err != nil {
		return "", false, err
	}
	selected, err := environmentSelection(root)
	if err != nil {
		return "", false, err
	}
	if len(selected) == 0 {
		return "", false, nil
	}
	environment := filepath.Join(root, selected["generation"].(string), "venv")
	if !isRelativeTo(resolvePath(environment), resolvePath(root)) {
		return "", false, NewInstallError(name, "selected environment escapes its root", "")
	}
	python := VenvPython(environment)
	if isRegularFile(python) && isRegularFile(filepath.Join(environment, "pyvenv.cfg")) {
		return python, true, nil
	}
	return "", false, nil
}

// Entry point of a tool beside an interpreter, ok false when the environment does not provide it
func environmentTool(python, executable string) (string, bool, error) {
	if !simplePackageName.MatchString(executable) {
		return "", false, &PyError{Class: "ValueError", Message: "tool executable must be a filename, not a path"}
	}
	directory := filepath.Dir(python)
	suffixes := []string{""}
	if runtime.GOOS == "windows" {
		suffixes = []string{".exe", ".cmd", ""}
	}
	for _, suffix := range suffixes {
		if candidate := filepath.Join(directory, executable+suffix); isRegularFile(candidate) {
			return candidate, true, nil
		}
	}
	return "", false, nil
}

// Selected tool entry point of a named side environment, never an installer executable
func PythonTool(name, executable, root string) (string, bool, error) {
	python, ok, err := EnvironmentPython(name, root)
	if err != nil || !ok {
		return "", false, err
	}
	return environmentTool(python, executable)
}

// Requirements normalized the way packaging prints them, deduplicated and sorted
func normalizedRequirements(requirements []string) ([]string, error) {
	if len(requirements) == 0 {
		return nil, &PyError{Class: "ValueError", Message: "an isolated environment needs a nonempty sequence of requirements"}
	}
	seen := map[string]bool{}
	var out []string
	for _, requirement := range requirements {
		parsed, err := ParseRequirement(requirement)
		var invalid *InvalidRequirementError
		if errors.As(err, &invalid) {
			return nil, &PyError{Class: "ValueError", Message: "invalid environment requirement: " + invalid.Message}
		}
		if err != nil {
			return nil, err
		}
		text, err := parsed.String()
		if err != nil {
			return nil, err
		}
		if !seen[text] {
			seen[text] = true
			out = append(out, text)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Python json dumps of a value with default separators and ASCII escapes, lone surrogates escaped as Python keeps them
func pyJSONCompact(v any) string {
	var b strings.Builder
	var encode func(v any)
	encode = func(v any) {
		switch t := v.(type) {
		case nil:
			b.WriteString("null")
		case bool:
			if t {
				b.WriteString("true")
			} else {
				b.WriteString("false")
			}
		case string:
			pyJSONString(&b, t)
		case []string:
			b.WriteByte('[')
			for i, s := range t {
				if i > 0 {
					b.WriteString(", ")
				}
				pyJSONString(&b, s)
			}
			b.WriteByte(']')
		case []any:
			b.WriteByte('[')
			for i, e := range t {
				if i > 0 {
					b.WriteString(", ")
				}
				encode(e)
			}
			b.WriteByte(']')
		case map[string]string:
			m := map[string]any{}
			for k, s := range t {
				m[k] = s
			}
			encode(m)
		case map[string]any:
			b.WriteByte('{')
			for i, k := range sortedKeys(t) {
				if i > 0 {
					b.WriteString(", ")
				}
				pyJSONString(&b, k)
				b.WriteString(": ")
				encode(t[k])
			}
			b.WriteByte('}')
		default:
			b.WriteString(fmt.Sprint(t))
		}
	}
	encode(v)
	return b.String()
}

// Writes a JSON string literal escaped like Python ensure_ascii, generalized UTF-8 surrogates escaped individually
func pyJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c == 0xED && i+2 < len(s) && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2] >= 0x80 && s[i+2] <= 0xBF {
			fmt.Fprintf(b, `\u%04x`, 0xD000|rune(s[i+1]&0x3F)<<6|rune(s[i+2]&0x3F))
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20 || (r >= 0x7f && r <= 0xFFFF):
			fmt.Fprintf(b, `\u%04x`, r)
		case r > 0xFFFF:
			r -= 0x10000
			fmt.Fprintf(b, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

// EnsureEnvironmentOptions are ensure_environment's arguments, empty Root and Executable for None
type EnsureEnvironmentOptions struct {
	Name         string
	Requirements []string
	Root         string
	Explicit     bool
	Timeout      time.Duration
	Executable   string
}

// Makes an isolated dependency set current then selects it, an optional entry point validated before publication
func ensureEnvironment(ctx context.Context, opts EnsureEnvironmentOptions) (string, error) {
	root, err := environmentRoot(ctx, opts.Name, opts.Root)
	if err != nil {
		return "", err
	}
	requirements, err := normalizedRequirements(opts.Requirements)
	if err != nil {
		return "", err
	}
	if opts.Executable != "" {
		if _, _, err := environmentTool(filepath.Join("unused", "python"), opts.Executable); err != nil {
			return "", err
		}
	}
	build := func(generation, basePython string) (string, error) {
		manifest := "[project]\nname = \"icebath-side-environment\"\nversion = \"0\"\n" +
			"requires-python = \">=3.11\"\ndependencies = " + pyJSONCompact(requirements) + "\n[tool.uv]\npackage = false\n"
		if err := os.WriteFile(filepath.Join(generation, "pyproject.toml"), []byte(manifest), 0o666); err != nil {
			return "", err
		}
		previous, err := environmentSelection(root)
		if err != nil {
			return "", err
		}
		if len(previous) > 0 {
			if seed := filepath.Join(root, previous["generation"].(string), "uv.lock"); isRegularFile(seed) {
				if err := copyFileBytes(seed, filepath.Join(generation, "uv.lock")); err != nil {
					return "", err
				}
			}
		}
		return buildEnvironment(ctx, BuildEnvironmentOptions{Source: generation, Out: filepath.Join(generation, "venv"), Python: basePython,
			Explicit: opts.Explicit, Timeout: opts.Timeout})
	}
	return ensureGeneration(ctx, opts.Name, root, map[string]any{"requirements": requirements}, build,
		map[string]any{"requirements": requirements}, opts.Explicit, opts.Executable)
}

// EnsureProjectEnvironmentOptions are ensure_project_environment's arguments, empty Root for None
type EnsureProjectEnvironmentOptions struct {
	Name     string
	Project  string
	Extras   []string
	Groups   []string
	Root     string
	Explicit bool
	Timeout  time.Duration
}

// Makes an isolated environment of a project's locked dependencies current, keyed by its manifest and lock bytes
func ensureProjectEnvironment(ctx context.Context, opts EnsureProjectEnvironmentOptions) (string, error) {
	root, err := environmentRoot(ctx, opts.Name, opts.Root)
	if err != nil {
		return "", err
	}
	project := absoluteUnresolved(opts.Project)
	extras, groups := sortedSet(opts.Extras), sortedSet(opts.Groups)
	if extras == nil {
		extras = []string{}
	}
	if groups == nil {
		groups = []string{}
	}
	manifests := map[string]any{}
	for _, manifest := range []string{"pyproject.toml", "uv.lock"} {
		data, err := os.ReadFile(filepath.Join(project, manifest))
		if errors.Is(err, fs.ErrNotExist) {
			return "", NewInstallError("venv", "locked project environment needs "+filepath.Join(project, manifest), "")
		}
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(data)
		manifests[manifest] = hex.EncodeToString(sum[:])
	}
	build := func(generation, basePython string) (string, error) {
		return buildEnvironment(ctx, BuildEnvironmentOptions{Source: project, Out: filepath.Join(generation, "venv"), Python: basePython,
			Extras: extras, Groups: groups, NoInstallProject: true, Frozen: true, Explicit: opts.Explicit, Timeout: opts.Timeout})
	}
	return ensureGeneration(ctx, opts.Name, root, map[string]any{"manifests": manifests, "extras": extras, "groups": groups},
		build, map[string]any{"extras": extras, "groups": groups}, opts.Explicit, "")
}

// Pinned interpreter resolved through links, empty when the toolchain is not installed
func selectedPython(ctx context.Context) (string, error) {
	_, python, found, err := uvToolchain(ctx, false, false)
	if err != nil || !found {
		return "", err
	}
	return resolvePath(python), nil
}

// Selects the generation whose inputs match, building one at its final path only when none does
func ensureGeneration(ctx context.Context, name, root string, inputs map[string]any, build func(generation, basePython string) (string, error),
	record map[string]any, explicit bool, executable string) (string, error) {
	lock := NewLockfile(LockfilePath())
	target, err := CurrentTarget()
	if err != nil {
		return "", err
	}
	full := map[string]any{}
	for k, v := range inputs {
		full[k] = v
	}
	if version, ok := lock.Version("python"); ok {
		full["python"] = version
	} else {
		full["python"] = nil
	}
	full["target"] = target
	artifacts := []any{}
	for _, item := range lock.Artifacts("python", target) {
		artifacts = append(artifacts, item["sha256"])
	}
	full["artifacts"] = artifacts
	identity := func(basePython string) string {
		withInterpreter := map[string]any{}
		for k, v := range full {
			withInterpreter[k] = v
		}
		withInterpreter["interpreter"] = Native(basePython)
		sum := sha256.Sum256([]byte(pyJSONCompact(withInterpreter)))
		return hex.EncodeToString(sum[:])
	}
	current := func(basePython string) (string, bool, error) {
		if basePython == "" {
			return "", false, nil
		}
		selected, err := environmentSelection(root)
		if err != nil {
			return "", false, err
		}
		python, ok, err := environmentPython(ctx, name, root)
		if err != nil {
			return "", false, err
		}
		inputsValue, _ := selected["inputs"].(string)
		if _, present := selected["inputs"]; !present || inputsValue != identity(basePython) || !ok {
			return "", false, nil
		}
		if executable != "" {
			if _, has, err := environmentTool(python, executable); err != nil || !has {
				return "", false, err
			}
		}
		return python, true, nil
	}
	base, err := selectedPython(ctx)
	if err != nil {
		return "", err
	}
	if existing, ok, err := current(base); err != nil || ok {
		return existing, err
	}
	if !explicit && !LazyInstallsAllowedFor(ctx) {
		return "", RefuseLazyFor(ctx, name, "isolated Python environment is missing or outdated")
	}
	if err := os.MkdirAll(root, 0o777); err != nil {
		return "", err
	}
	mutex, err := os.OpenFile(filepath.Join(root, ".install.lock"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o666)
	if err != nil {
		return "", err
	}
	defer mutex.Close()
	if _, err := LockFD(mutex, true, WaitForever); err != nil {
		return "", err
	}
	if base, err = selectedPython(ctx); err != nil {
		return "", err
	}
	if existing, ok, err := current(base); err != nil || ok {
		return existing, err
	}
	if base == "" {
		_, python, found, err := uvToolchain(ctx, true, explicit)
		if err != nil {
			return "", err
		}
		if !found {
			return "", NewInstallError(name, "PM's pinned toolchain is unavailable", "")
		}
		base = resolvePath(python)
	}
	generation := filepath.Join(root, "gen-"+randomHex32())
	if err := os.Mkdir(generation, 0o777); err != nil {
		return "", err
	}
	python, err := func() (string, error) {
		python, err := build(generation, base)
		if err != nil {
			return "", err
		}
		if executable != "" {
			_, has, err := environmentTool(python, executable)
			if err != nil {
				return "", err
			}
			if !has {
				return "", NewInstallError(name, "installed requirements do not provide "+pyrepr.StrRepr(executable), "")
			}
		}
		if err := flushBeforeSelecting(); err != nil {
			return "", err
		}
		selection := map[string]any{"generation": filepath.Base(generation), "inputs": identity(base)}
		for k, v := range record {
			selection[k] = v
		}
		return python, writeLockDocument(filepath.Join(root, "active.json"), selection)
	}()
	if err != nil {
		removeTreeQuiet(generation)
		return "", err
	}
	return python, nil
}

// EnsurePythonToolOptions are ensure_python_tool's arguments, empty Root for None
type EnsurePythonToolOptions struct {
	Name         string
	Requirements []string
	Executable   string
	Root         string
	Explicit     bool
	Timeout      time.Duration
}

// Makes an isolated tool environment current and returns its validated entry point
func ensurePythonTool(ctx context.Context, opts EnsurePythonToolOptions) (string, error) {
	python, err := ensureEnvironment(ctx, EnsureEnvironmentOptions{Name: opts.Name, Requirements: opts.Requirements, Root: opts.Root,
		Explicit: opts.Explicit, Timeout: opts.Timeout, Executable: opts.Executable})
	if err != nil {
		return "", err
	}
	tool, ok, err := environmentTool(python, opts.Executable)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", &PyError{Class: "AssertionError"}
	}
	return tool, nil
}
