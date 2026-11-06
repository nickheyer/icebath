package codeexec

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

var isWindows = runtime.GOOS == "windows"

var safeEnvPrefixes = []string{"PATH", "HOME", "USER", "LANG", "LC_", "TERM", "TMPDIR", "TMP", "TEMP", "SHELL",
	"LOGNAME", "XDG_", "PYTHONPATH", "VIRTUAL_ENV", "CONDA"}

// SecretSubstrings are the name fragments that mark an environment variable as a credential
var SecretSubstrings = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "PASSWD", "AUTH", "DSN",
	"WEBHOOK", "CREDS", "BEARER", "APIKEY", "COOKIE"}

// Reports whether a variable name contains a credential fragment in any letter case
func IsSecretEnvName(name string) bool {
	upper := pystr.Upper(name)
	for _, s := range SecretSubstrings {
		if strings.Contains(upper, s) {
			return true
		}
	}
	return false
}

const delegatedChildEnvMarker = "ICEBATH_DELEGATED_CHILD_CONTEXT"

var icebathChildAllowed = map[string]bool{
	"ICEBATH_HOME": true, "ICEBATH_PROFILE": true, "ICEBATH_CONFIG": true, "ICEBATH_ENV": true,
	delegatedChildEnvMarker: true,
}

var windowsEssentialEnvVars = map[string]bool{
	"SYSTEMROOT": true, "SYSTEMDRIVE": true, "WINDIR": true, "COMSPEC": true, "PATHEXT": true, "OS": true,
	"PROCESSOR_ARCHITECTURE": true, "NUMBER_OF_PROCESSORS": true, "PUBLIC": true, "ALLUSERSPROFILE": true,
	"PROGRAMDATA": true, "PROGRAMFILES": true, "PROGRAMFILES(X86)": true, "PROGRAMW6432": true,
	"APPDATA": true, "LOCALAPPDATA": true, "USERPROFILE": true, "USERDOMAIN": true, "USERNAME": true,
	"HOMEDRIVE": true, "HOMEPATH": true, "COMPUTERNAME": true,
}

type scrubOptions struct {
	isPassthrough func(string) bool
	isWindows     *bool
	toolchain     map[string]bool
}

func logger(env *tools.Env) *slog.Logger {
	if env != nil && env.Log != nil {
		return env.Log
	}
	return slog.Default()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Process environment as a map, keys uppercased on Windows like os.environ
func environMap() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if isWindows {
			k = strings.ToUpper(k)
		}
		out[k] = v
	}
	return out
}

func envList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, k+"="+m[k])
	}
	return out
}

// Environment the session's tool subprocesses start from, the bound client's toolchain when one is bound
func toolchainEnviron(env *tools.Env) map[string]string {
	base := environMap()
	if tools.SessionToolchainEnviron != nil {
		base = tools.SessionToolchainEnviron(env, base)
	}
	return base
}

// Toolchain values a bound local session's tool subprocesses start with, empty when unbound
func sessionToolEnv(env *tools.Env) map[string]string {
	if tools.SessionToolEnv == nil {
		return map[string]string{}
	}
	return tools.SessionToolEnv(env)
}

// Scrubbed child env applying passthrough, secret, safe prefix, control name and Windows essential rules
func scrubChildEnv(env *tools.Env, source map[string]string, opts scrubOptions) (map[string]string, error) {
	isPassthrough := opts.isPassthrough
	if isPassthrough == nil {
		isPassthrough = func(k string) bool { return tools.IsEnvPassthrough(env, k) }
	}
	windows := isWindows
	if opts.isWindows != nil {
		windows = *opts.isWindows
	}
	scrubbed := map[string]string{}
	var dropped []string
	for _, k := range sortedKeys(source) {
		v := source[k]
		if isPassthrough(k) {
			resolved, err := tools.ResolvePassthroughValue(env, k, &v)
			if err != nil {
				return nil, err
			}
			if resolved != nil {
				scrubbed[k] = *resolved
			}
			continue
		}
		if IsSecretEnvName(k) {
			continue
		}
		safe := false
		for _, p := range safeEnvPrefixes {
			if strings.HasPrefix(k, p) {
				safe = true
				break
			}
		}
		if safe || opts.toolchain[k] || icebathChildAllowed[k] || (windows && windowsEssentialEnvVars[pystr.Upper(k)]) {
			scrubbed[k] = v
		} else if strings.HasPrefix(k, "ICEBATH_") {
			dropped = append(dropped, k)
		}
	}
	present := map[string]bool{}
	for k := range scrubbed {
		present[k] = true
	}
	for k, v := range tools.ScopedPassthroughAdditions(env, present) {
		if isPassthrough(k) {
			scrubbed[k] = v
		}
	}
	if len(dropped) > 0 {
		logger(env).Debug("execute_code: dropped non-allowlisted ICEBATH_* var(s) from the sandbox child env. "+
			"This is intentional hardening (#27303); if a sandbox script legitimately needs one, declare it via "+
			"env_passthrough in the skill/config so it passes by explicit opt-in.",
			"count", len(dropped), "names", strings.Join(dropped, ", "))
	}
	scoped := tools.DelegatedChildSubprocessEnv(env, source)
	if scoped[delegatedChildEnvMarker] != "" {
		for _, key := range []string{delegatedChildEnvMarker, "ICEBATH_KANBAN_DB", "ICEBATH_KANBAN_BOARD"} {
			if v, ok := scoped[key]; ok {
				scrubbed[key] = v
			}
		}
	}
	return tools.DelegatedChildSubprocessEnv(env, scrubbed), nil
}

type childEnvSpec struct {
	RPCEndpoint string
	RPCToken    string
	TmpDir      string
	ChildPython string
}

// Configured IANA timezone handed to children as TZ, swapped by tests
var timezoneName = config.TimezoneName

// Scrubbed child environment both execution paths share, with RPC, UTF-8, TZ, home and PYTHONPATH rules
func buildChildEnv(ctx context.Context, env *tools.Env, spec childEnvSpec) (map[string]string, error) {
	toolchain := map[string]bool{}
	for k := range sessionToolEnv(env) {
		toolchain[k] = true
	}
	child, err := scrubChildEnv(env, toolchainEnviron(env), scrubOptions{toolchain: toolchain})
	if err != nil {
		return nil, err
	}
	child["ICEBATH_RPC_SOCKET"] = spec.RPCEndpoint
	child["ICEBATH_RPC_TOKEN"] = spec.RPCToken
	child["PYTHONDONTWRITEBYTECODE"] = "1"
	child["PYTHONIOENCODING"] = "utf-8"
	child["PYTHONUTF8"] = "1"
	if tz := timezoneName(ctx); tz != "" && !isWindows {
		child["TZ"] = tz
	}
	delete(child, "ICEBATH_TIMEZONE")
	home.ApplySubprocessHomeEnv(ctx, child)
	if override := home.Override(ctx); override != "" {
		child["ICEBATH_HOME"] = override
		home.ApplyScratchTmpEnv(child)
	}
	rt, err := runtimeInterpreter(ctx, env)
	if err != nil {
		return nil, err
	}
	sep := string(os.PathListSeparator)
	runtimePath := ""
	if spec.ChildPython == rt.Python {
		venv, ok, err := tools.ValidatedRuntimeVenv(child)
		if err != nil {
			return nil, err
		}
		if ok {
			candidate, err := pm.SitePackages(venv)
			if err != nil {
				return nil, err
			}
			for _, entry := range strings.Split(child["PYTHONPATH"], sep) {
				if entry != "" && tools.SamePath(entry, candidate) {
					runtimePath = candidate
					break
				}
			}
		}
	}
	if err := tools.StripIcebathOwnedPythonpath(child); err != nil {
		return nil, err
	}
	existing := child["PYTHONPATH"]
	parts := []string{spec.TmpDir}
	if runtimePath != "" {
		parts = append(parts, runtimePath)
	}
	uses, err := usesIcebathPythonEnvironment(ctx, env, spec.ChildPython)
	if err != nil {
		return nil, err
	}
	if uses {
		parts = append(parts, pm.RepoRoot())
	} else if markExternalLogged(spec.ChildPython) {
		logger(env).Info("execute_code: child interpreter is outside the Icebath environment; icebath root omitted from PYTHONPATH",
			"python", spec.ChildPython)
	}
	if existing != "" {
		parts = append(parts, existing)
	}
	child["PYTHONPATH"] = strings.Join(parts, sep)
	return child, nil
}

// Interpreter of the managed runtime and its resolved prefix, the counterpart of sys.executable and sys.prefix
type interpreter struct {
	Python string
	Prefix string
}

// Managed runtime interpreter resolver, swapped by tests for a host python
var runtimeInterpreter = resolveRuntimeInterpreter

// Selected managed venv python, else the pinned toolchain python with its probed prefix
func resolveRuntimeInterpreter(ctx context.Context, env *tools.Env) (interpreter, error) {
	venv, err := pm.SelectedVenv(ctx, pm.RepoRoot())
	if err != nil {
		return interpreter{}, err
	}
	if python := pm.VenvPython(venv); isRegularFile(python) {
		prefix, err := pypath.Realpath(venv, pypath.Lax)
		if err != nil {
			return interpreter{}, err
		}
		return interpreter{Python: python, Prefix: prefix}, nil
	}
	_, pinned, found, err := pm.Toolchain(ctx, false, false)
	if err != nil {
		return interpreter{}, err
	}
	if !found {
		return interpreter{}, errors.New("no Python interpreter is installed for execute_code")
	}
	return interpreter{Python: pinned, Prefix: pythonEnvironmentPrefix(env, pinned)}, nil
}

func isRegularFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

const probeCacheMax = 32

// Bounded insertion ordered cache evicting the oldest entry at the cap
type probeCache[V any] struct {
	mu    sync.Mutex
	order []string
	items map[string]V
}

func (c *probeCache[V]) get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.items[key]
	return v, ok
}

func (c *probeCache[V]) put(key string, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = map[string]V{}
	}
	if _, ok := c.items[key]; !ok {
		if len(c.order) >= probeCacheMax {
			delete(c.items, c.order[0])
			c.order = c.order[1:]
		}
		c.order = append(c.order, key)
	}
	c.items[key] = value
}

func (c *probeCache[V]) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order, c.items = nil, nil
}

var (
	usablePythonCache   probeCache[bool]
	pythonPrefixCache   probeCache[string]
	externalEnvLoggedMu sync.Mutex
	externalEnvLogged   = map[string]bool{}
)

// Records a child interpreter as logged and reports whether this is its first notice
func markExternalLogged(python string) bool {
	externalEnvLoggedMu.Lock()
	defer externalEnvLoggedMu.Unlock()
	if externalEnvLogged[python] {
		return false
	}
	externalEnvLogged[python] = true
	return true
}

type probeResult struct {
	returnCode int
	stdout     string
}

// Runs python -c code with a five second timeout, false when missing, unspawnable or timed out
var probePython = func(env *tools.Env, python, code string) (probeResult, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", code)
	cmd.Env = envList(tools.DelegatedChildSubprocessEnv(env, environMap()))
	hideConsoleWindow(cmd)
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return probeResult{}, false
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return probeResult{}, false
	}
	return probeResult{returnCode: cmd.ProcessState.ExitCode(), stdout: string(out)}, true
}

// Reports whether the interpreter is Python 3.8 or newer, caching success and failure but never a failed probe
func isUsablePython(env *tools.Env, python string) bool {
	if cached, ok := usablePythonCache.get(python); ok {
		return cached
	}
	result, ok := probePython(env, python, "import sys; sys.exit(0 if sys.version_info >= (3, 8) else 1)")
	if !ok {
		return false
	}
	usable := result.returnCode == 0
	usablePythonCache.put(python, usable)
	return usable
}

// Resolved sys.prefix the interpreter reports, empty on failure and failures are never cached
func pythonEnvironmentPrefix(env *tools.Env, python string) string {
	if cached, ok := pythonPrefixCache.get(python); ok {
		return cached
	}
	result, ok := probePython(env, python, "import sys; print(sys.prefix)")
	if ok && result.returnCode == 0 && pystr.Strip(result.stdout) != "" {
		prefix, err := pypath.Realpath(pystr.Strip(result.stdout), pypath.Lax)
		if err != nil {
			return ""
		}
		pythonPrefixCache.put(python, prefix)
		return prefix
	}
	return ""
}

// Reports whether the interpreter belongs to the managed runtime environment, by path first and then by prefix
func usesIcebathPythonEnvironment(ctx context.Context, env *tools.Env, python string) (bool, error) {
	rt, err := runtimeInterpreter(ctx, env)
	if err != nil {
		return false, err
	}
	if python == rt.Python {
		return true, nil
	}
	resolved, err := pypath.Realpath(python, pypath.Lax)
	if err != nil {
		return false, err
	}
	runtimeResolved, err := pypath.Realpath(rt.Python, pypath.Lax)
	if err != nil {
		return false, err
	}
	if resolved == runtimeResolved {
		return true, nil
	}
	prefix, err := pypath.Realpath(rt.Prefix, pypath.Lax)
	if err != nil {
		return false, err
	}
	return pythonEnvironmentPrefix(env, python) == prefix, nil
}

// Child interpreter, the runtime one in strict mode and the active venv or conda python in project mode
func resolveChildPython(ctx context.Context, env *tools.Env, mode string) (string, error) {
	rt, err := runtimeInterpreter(ctx, env)
	if err != nil {
		return "", err
	}
	if mode != "project" {
		return rt.Python, nil
	}
	environ := toolchainEnviron(env)
	subdir, exeNames := "bin", []string{"python", "python3"}
	if isWindows {
		subdir, exeNames = "Scripts", []string{"python.exe", "python3.exe"}
	}
	for _, name := range []string{"VIRTUAL_ENV", "CONDA_PREFIX"} {
		root := pystr.Strip(environ[name])
		if root == "" {
			continue
		}
		for _, exe := range exeNames {
			candidate := filepath.Join(root, subdir, exe)
			if !isRegularFile(candidate) || !isExecutable(candidate) {
				continue
			}
			if isUsablePython(env, candidate) {
				return candidate, nil
			}
			logger(env).Info("execute_code: skipping interpreter (Python version < 3.8 or broken). Using the runtime interpreter instead.",
				"var", name, "python", candidate)
			return rt.Python, nil
		}
	}
	return rt.Python, nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Child cwd, the staging dir in strict mode and the terminal cwd ladder in project mode
func resolveChildCwd(env *tools.Env, mode, stagingDir, taskID string) (string, error) {
	if mode != "project" {
		return stagingDir, nil
	}
	if taskID != "" {
		if recorded, ok := tools.GetSessionCwd(env, taskID); ok && recorded != "" && isDir(recorded) {
			return recorded, nil
		}
		if sessionCwd := tools.RegisteredTaskCwdOverride(env, taskID); sessionCwd != "" && isDir(sessionCwd) {
			return sessionCwd, nil
		}
	}
	raw, err := tools.ScopeTerminalCwd(env)
	if err != nil {
		return "", err
	}
	raw = pystr.Strip(raw)
	candidates := []string{""}
	if raw != "" {
		candidates[0] = pypath.Expanduser(raw)
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	candidates = append(candidates, wd)
	for _, c := range candidates {
		if c != "" && isDir(c) {
			return c, nil
		}
	}
	return stagingDir, nil
}
