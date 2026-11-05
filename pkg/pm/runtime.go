package pm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// PMRuntimeImports is the probe statement proving a PM runtime generation imports its own dependencies
const PMRuntimeImports = "import packaging, tomli_w, truststore; from ruamel.yaml import YAML"

// Environment of an isolated runtime child, ambient interpreter selection stripped and the home and store pinned
func RuntimeEnvironment(ctx context.Context) (map[string]string, error) {
	env, err := BaseEnvironment(nil)
	if err != nil {
		return nil, err
	}
	store, err := StoreRoot(ctx)
	if err != nil {
		return nil, err
	}
	env[home.EnvHome] = home.Dir(ctx)
	env[EnvRuntimeDir] = Native(store)
	return env, nil
}

// Identity of a runtime's inputs, its project lock and the interpreter reached through resolved directories
func runtimeInputs(project, python string, asSpelled bool) (string, error) {
	h := sha256.New()
	for _, name := range []string{"pyproject.toml", "uv.lock"} {
		data, err := os.ReadFile(filepath.Join(project, name))
		if err != nil {
			return "", err
		}
		h.Write(data)
		h.Write([]byte{0})
	}
	python = absoluteUnresolved(Native(python))
	if asSpelled {
		h.Write([]byte(python))
	} else {
		h.Write([]byte(filepath.Join(resolvePath(filepath.Dir(python)), filepath.Base(python))))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Absolute spelling of a path without collapsing dot dot or resolving links, like Path absolute
func absoluteUnresolved(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	wd, err := os.Getwd()
	if err != nil {
		return path
	}
	return wd + string(filepath.Separator) + path
}

// Reports whether an interpreter prefix is a published PM runtime generation
func IsRuntimePrefix(prefix string) bool {
	return isRegularFile(filepath.Join(prefix, "pm-runtime.json"))
}

// ResidentRuntime is a packaged runtime's interpreter and its site packages
type ResidentRuntime struct {
	Python       string
	SitePackages string
}

// Packaged runtime a sealed payload or a nix or docker stamp ships, nil when this install has none
func FindResidentRuntime() (*ResidentRuntime, error) {
	project := RepoRoot()
	payload := ""
	if isRegularFile(filepath.Join(filepath.Dir(project), "manifest.json")) {
		payload = filepath.Dir(project)
	}
	var runtimeDir string
	if payload != "" {
		runtimeDir = filepath.Join(payload, "pm-runtime")
	} else {
		data, err := os.ReadFile(InstallStampPath(project))
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		var stamp map[string]any
		if err == nil {
			var v any
			v, err = loadJSON(data)
			if err == nil {
				var ok bool
				if stamp, ok = v.(map[string]any); !ok {
					err = errors.New("install stamp is not an object")
				}
			}
		}
		if err != nil {
			return nil, NewInstallError("pm-runtime", "invalid package install stamp", "reinstall this application")
		}
		if d, _ := stamp["distribution"].(string); d != "nix" && d != "docker" {
			return nil, nil
		}
		value, ok := stamp["pmRuntime"].(string)
		if !ok || !filepath.IsAbs(value) {
			return nil, NewInstallError("pm-runtime", "packaged PM runtime is missing", "reinstall this application")
		}
		runtimeDir = value
	}
	invalid := NewInstallError("pm-runtime", "packaged PM runtime is missing or invalid", "reinstall this application")
	data, err := os.ReadFile(filepath.Join(runtimeDir, "pm-runtime.json"))
	if err != nil {
		return nil, invalid
	}
	v, err := loadJSON(data)
	if err != nil {
		return nil, invalid
	}
	marker, ok := v.(map[string]any)
	pythonRel, okPython := marker["python"].(string)
	siteRel, okSite := marker["sitePackages"].(string)
	if !ok || !okPython || !okSite {
		return nil, invalid
	}
	python := resolvePath(pypath.Join(runtimeDir, pythonRel))
	site := resolvePath(pypath.Join(runtimeDir, siteRel))
	siteInfo, siteErr := os.Stat(site)
	if !isRegularFile(python) || siteErr != nil || !siteInfo.IsDir() ||
		(payload != "" && (!isRelativeTo(python, payload) || !isRelativeTo(site, runtimeDir))) {
		return nil, NewInstallError("pm-runtime", "packaged PM paths are missing or escape the payload", "reinstall this application")
	}
	return &ResidentRuntime{Python: python, SitePackages: site}, nil
}

// Empty when the generation's interpreter imports what imports names, else why it cannot
var validateRuntime = func(ctx context.Context, python, imports string, env map[string]string) string {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-I", "-B", "-c", imports)
	cmd.Env = EnvList(env)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("Command '%s' timed out after 30 seconds", pyrepr.Repr(cmd.Args))
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if tail := strings.TrimSpace(stderr.String()); tail != "" {
			return tail
		}
		return fmt.Sprintf("exit %d", exitErr.ExitCode())
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

var (
	heldMu sync.Mutex
	held   = map[string]func(){}
)

// Leases a generation once for this process so children resolved to it outlive a later collection
var holdForChildren = func(environment string) error {
	heldMu.Lock()
	defer heldMu.Unlock()
	if _, ok := held[environment]; ok {
		return nil
	}
	release, err := LeaseDirectory(environment)
	if err != nil {
		return err
	}
	held[environment] = release
	return nil
}

// RuntimeOptions select the locked project a runtime generation is built from and how
type RuntimeOptions struct {
	Project     string
	Imports     string
	Offline     bool
	NoBootstrap bool
	Cache       string
}

// Publishes or reuses an immutable locked runtime generation for a project and returns its interpreter
func PrepareRuntime(ctx context.Context, uv, python, root string, opts RuntimeOptions) (string, error) {
	identity, err := runtimeInputs(opts.Project, python, false)
	if err != nil {
		return "", err
	}
	spelled, err := runtimeInputs(opts.Project, python, true)
	if err != nil {
		return "", err
	}
	env, err := RuntimeEnvironment(ctx)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o777); err != nil {
		return "", err
	}
	lock, err := os.OpenFile(filepath.Join(root, ".prepare.lock"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o666)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if _, err := LockFD(lock, true, WaitForever); err != nil {
		return "", err
	}
	selected := filepath.Join(root, "selected.json")
	fact := map[string]any{}
	if data, err := os.ReadFile(selected); err == nil {
		v, err := loadJSON(data)
		if err != nil {
			return "", err
		}
		m, ok := v.(map[string]any)
		if !ok {
			return "", fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(v))
		}
		fact = m
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if inputs, ok := fact["inputs"].(string); ok && (inputs == identity || inputs == spelled) {
		value, present := fact["generation"]
		if !present {
			return "", errors.New("KeyError: 'generation'")
		}
		environment, err := generationPath(root, value)
		if err != nil {
			return "", err
		}
		if IsRuntimePrefix(environment) && validateRuntime(ctx, VenvPython(environment), opts.Imports, env) == "" {
			if err := holdForChildren(environment); err != nil {
				return "", err
			}
			return VenvPython(environment), nil
		}
	}
	if opts.NoBootstrap {
		return "", NewInstallError("pm-runtime", "not installed or outdated and lazy installs are disabled",
			"run `icebath pm install` to prepare the independent PM runtime")
	}
	generation := "generations/" + randomHex32()
	environment := filepath.Join(root, filepath.FromSlash(generation))
	executable, err := publishRuntimeGeneration(ctx, uv, python, environment, selected, identity, generation, opts)
	if err != nil {
		removeTreeQuiet(environment)
		return "", err
	}
	if err := holdForChildren(environment); err != nil {
		return "", err
	}
	return executable, nil
}

// Builds a locked runtime environment at its final path
var stageRuntime = StageRuntime

// Generation directory a selection record names, joined the way pathlib joins and refusing a non string
func generationPath(root string, value any) (string, error) {
	generation, ok := value.(string)
	if !ok {
		kind := "PosixPath"
		if runtime.GOOS == "windows" {
			kind = "WindowsPath"
		}
		return "", fmt.Errorf("unsupported operand type(s) for /: '%s' and '%s'", kind, pyrepr.TypeName(value))
	}
	return filepath.Clean(pypath.Join(root, generation)), nil
}

// Stages a generation at its final path then writes its marker and the selection record
func publishRuntimeGeneration(ctx context.Context, uv, python, environment, selected, identity, generation string, opts RuntimeOptions) (string, error) {
	fmt.Fprintln(os.Stderr, "Preparing the isolated icebath runtime…")
	executable, err := stageRuntime(ctx, uv, python, environment, StageRuntimeOptions{Project: opts.Project, Offline: opts.Offline, Cache: opts.Cache, Imports: opts.Imports})
	if err != nil {
		return "", err
	}
	if err := touchFile(filepath.Join(environment, ".lease-managed")); err != nil {
		return "", err
	}
	if err := writeLockDocument(filepath.Join(environment, "pm-runtime.json"), map[string]any{"inputs": identity}); err != nil {
		return "", err
	}
	if err := writeLockDocument(selected, map[string]any{"inputs": identity, "generation": generation}); err != nil {
		return "", err
	}
	return executable, nil
}

// Creates a file or bumps its mtime
func touchFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	now := time.Now()
	return os.Chtimes(path, now, now)
}

// Pins the runtime generation an interpreter prefix belongs to so the collector leaves it alone
func LeaseCurrentRuntime(prefix string) error {
	if !IsRuntimePrefix(prefix) {
		return nil
	}
	_, err := LeaseDirectory(prefix)
	return err
}

// Removes runtime generations nothing can run from, skipping while a stage holds the prepare lock
func CollectRuntimeGenerations(root string) ([]string, error) {
	generations := filepath.Join(root, "generations")
	if st, err := os.Stat(generations); err != nil || !st.IsDir() {
		return nil, nil
	}
	lock, err := os.OpenFile(filepath.Join(root, ".prepare.lock"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o666)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	locked, err := LockFD(lock, false, 0)
	if err != nil || !locked {
		return nil, err
	}
	var selectedValue any = ""
	if data, err := os.ReadFile(filepath.Join(root, "selected.json")); err == nil {
		v, err := loadJSON(data)
		if err != nil {
			return nil, err
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(v))
		}
		if g, has := m["generation"]; has {
			selectedValue = g
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	selected, err := generationPath(root, selectedValue)
	if err != nil {
		return nil, err
	}
	children, err := sortedChildren(generations)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, generation := range children {
		st, err := os.Lstat(generation)
		if err != nil || st.Mode()&fs.ModeSymlink != 0 || !st.IsDir() || generation == selected {
			continue
		}
		if IsRuntimePrefix(generation) {
			if !isRegularFile(filepath.Join(generation, ".lease-managed")) {
				continue
			}
			leased, err := LeasesHeld(generation)
			if err != nil {
				return nil, err
			}
			if leased {
				continue
			}
		}
		if err := RemoveTree(generation); err != nil {
			return nil, err
		}
		removed = append(removed, generation)
	}
	return removed, nil
}

// Resolves a runtime interpreter for a project without selecting or repairing anything else
func RuntimePython(ctx context.Context, opts RuntimeOptions) (string, error) {
	resident, err := FindResidentRuntime()
	if err != nil {
		return "", err
	}
	if resident != nil {
		return resident.Python, nil
	}
	uv, python, found, err := Toolchain(ctx, false, false)
	if err != nil {
		return "", err
	}
	if !found {
		if opts.NoBootstrap {
			return "", NewInstallError("pm-runtime", "not installed and lazy installs are disabled",
				"run `icebath pm install` to prepare the independent PM runtime")
		}
		uv, python, found, err = Toolchain(ctx, true, true)
		if err != nil {
			return "", err
		}
	}
	if !found {
		return "", NewInstallError("pm-runtime", "pinned uv and Python are unavailable", "")
	}
	return PrepareRuntime(ctx, uv, python, filepath.Join(InstallStateDir(ctx, RepoRoot()), "pm-runtime"), opts)
}

// Launch argv running a script in the isolated runtime, a resident payload loading its own site packages
func RuntimeCommand(ctx context.Context, script string, args []string, opts RuntimeOptions) ([]string, error) {
	resident, err := FindResidentRuntime()
	if err != nil {
		return nil, err
	}
	if resident == nil {
		python, err := RuntimePython(ctx, opts)
		if err != nil {
			return nil, err
		}
		return append([]string{python, "-I", "-B", script}, args...), nil
	}
	launcher := "import runpy,sys; sys.path.insert(0,sys.argv.pop(1)); " +
		"script=sys.argv.pop(1); sys.argv[0]=script; runpy.run_path(script,run_name='__main__')"
	return append([]string{resident.Python, "-I", "-S", "-B", "-c", launcher, resident.SitePackages, script}, args...), nil
}

// Runs a script in the isolated runtime with inherited stdio and returns its exit code
func RunRuntimeScript(ctx context.Context, script string, argv []string, opts RuntimeOptions) (int, error) {
	command, err := RuntimeCommand(ctx, script, argv, opts)
	if err != nil {
		return 0, err
	}
	env, err := RuntimeEnvironment(ctx)
	if err != nil {
		return 0, err
	}
	return (&Runner{Name: "pm-runtime", Env: env}).Run(ctx, command[0], command[1:]...)
}
