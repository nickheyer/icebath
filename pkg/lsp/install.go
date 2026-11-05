package lsp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Recipe is how one server package installs and which binary it provides
type Recipe struct {
	Strategy  string
	Pkg       string
	Bin       string
	ExtraPkgs []string
}

// TypescriptSDKPkg pins the last TypeScript line that ships a JavaScript SDK
const TypescriptSDKPkg = "typescript@6"

func npmRecipe(pkg, bin string, extra ...string) Recipe {
	return Recipe{Strategy: "npm", Pkg: pkg, Bin: bin, ExtraPkgs: extra}
}

func manualRecipe(bin string) Recipe { return Recipe{Strategy: "manual", Bin: bin} }

// InstallRecipes maps a recipe key to its install strategy, package and binary
var InstallRecipes = map[string]Recipe{
	"pyright":                           npmRecipe("pyright", "pyright-langserver"),
	"typescript-language-server":        npmRecipe("typescript-language-server", "typescript-language-server", TypescriptSDKPkg),
	"@vue/language-server":              npmRecipe("@vue/language-server@2", "vue-language-server", TypescriptSDKPkg),
	"svelte-language-server":            npmRecipe("svelte-language-server", "svelteserver"),
	"@astrojs/language-server":          npmRecipe("@astrojs/language-server", "astro-ls"),
	"yaml-language-server":              npmRecipe("yaml-language-server", "yaml-language-server"),
	"bash-language-server":              npmRecipe("bash-language-server", "bash-language-server"),
	"intelephense":                      npmRecipe("intelephense", "intelephense"),
	"dockerfile-language-server-nodejs": npmRecipe("dockerfile-language-server-nodejs", "docker-langserver"),
	"gopls":                             {Strategy: "go", Pkg: "golang.org/x/tools/gopls@latest", Bin: "gopls"},
	"rust-analyzer":                     manualRecipe("rust-analyzer"),
	"clangd":                            manualRecipe("clangd"),
	"lua-language-server":               manualRecipe("lua-language-server"),
	"laravel-lsp":                       manualRecipe("laravel-lsp"),
	"powershell":                        manualRecipe("pwsh"),
}

// Binary name of a recipe key, the key itself when no recipe names one
func recipeBin(pkg string) string {
	if r, ok := InstallRecipes[pkg]; ok && r.Bin != "" {
		return r.Bin
	}
	return pkg
}

// One install outcome cached for the process
type installResult struct {
	path string
	err  error
}

var (
	installMeta    sync.Mutex
	installLocks   = map[string]*sync.Mutex{}
	installResults = map[string]installResult{}
)

var windowsWrapperSuffixes = []string{".cmd", ".exe", ".bat"}

func isWindowsHost() bool { return runtime.GOOS == "windows" }

// BinDir returns and creates the staging directory for LSP server binaries
func BinDir(rt *Runtime) (string, error) {
	p := filepath.Join(rt.home(), "lsp", "bin")
	if err := os.MkdirAll(p, 0o777); err != nil {
		return "", err
	}
	return p, nil
}

// Platform native executable candidates for a staged binary, wrappers before the bare name on Windows
func nativeBinaryCandidates(base string, windows bool) []string {
	if !windows {
		return []string{base}
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range []string{base + ".cmd", base + ".exe", base + ".bat", base} {
		if k := strings.ToLower(c); !seen[k] {
			seen[k] = true
			out = append(out, c)
		}
	}
	return out
}

// First platform native candidate of any base that exists on disk
func firstExisting(windows bool, bases ...string) string {
	for _, base := range bases {
		for _, c := range nativeBinaryCandidates(base, windows) {
			if osPathExists(c) {
				return c
			}
		}
	}
	return ""
}

// npm's own bin directory under the staging tree
func npmBinDir(rt *Runtime) (string, error) {
	bin, err := BinDir(rt)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(bin), "node_modules", ".bin"), nil
}

// ExistingBinary probes the staging dir, npm's bin dir on Windows, the Python tool envs, then PATH
func ExistingBinary(rt *Runtime, name string) (string, error) {
	return existingBinary(rt, name, isWindowsHost())
}

func existingBinary(rt *Runtime, name string, windows bool) (string, error) {
	bin, err := BinDir(rt)
	if err != nil {
		return "", err
	}
	bases := []string{filepath.Join(bin, name)}
	if windows {
		npmBin, err := npmBinDir(rt)
		if err != nil {
			return "", err
		}
		bases = append(bases, filepath.Join(npmBin, name))
	}
	for _, base := range bases {
		for _, staged := range nativeBinaryCandidates(base, windows) {
			if osPathExists(staged) && accessExec(staged) {
				return staged, nil
			}
		}
	}
	if hasPipRecipeFor(name) {
		if rt == nil || rt.PythonTool == nil {
			return "", missingHook("PythonTool")
		}
		type tool struct {
			bin string
			ok  bool
		}
		found, err := guard(func() (tool, error) {
			bin, ok, err := rt.PythonTool("lsp-"+name, name)
			return tool{bin, ok}, err
		})
		binary, ok := found.bin, found.ok
		if err != nil {
			rt.logf(slog.LevelWarn, "agent.lsp.install", "[install] cannot read Python server %s: %s", name, err)
		} else if ok {
			return binary, nil
		}
	}
	suffixes := []string{""}
	if windows {
		suffixes = append(append([]string{}, windowsWrapperSuffixes...), "")
	}
	for _, s := range suffixes {
		if p := pyWhich(name + s); p != "" {
			return p, nil
		}
	}
	return "", nil
}

// Whether a pip recipe provides the binary
func hasPipRecipeFor(name string) bool {
	for _, r := range InstallRecipes {
		if r.Strategy == "pip" && r.Bin == name {
			return true
		}
	}
	return false
}

// TryInstall installs a package under the auto strategy, else probes, returning the binary or empty
func TryInstall(rt *Runtime, pkg, strategy string) (string, error) {
	if strategy != "auto" {
		return ExistingBinary(rt, recipeBin(pkg))
	}
	installMeta.Lock()
	if res, ok := installResults[pkg]; ok {
		installMeta.Unlock()
		return res.path, res.err
	}
	lock := installLocks[pkg]
	if lock == nil {
		lock = &sync.Mutex{}
		installLocks[pkg] = lock
	}
	installMeta.Unlock()
	lock.Lock()
	defer lock.Unlock()
	installMeta.Lock()
	res, ok := installResults[pkg]
	installMeta.Unlock()
	if ok {
		return res.path, res.err
	}
	path, err := doInstall(rt, pkg)
	if err == nil {
		installMeta.Lock()
		installResults[pkg] = installResult{path: path}
		installMeta.Unlock()
	}
	return path, err
}

// Installs one package by its recipe, probing first and dispatching on the strategy
func doInstall(rt *Runtime, pkg string) (string, error) {
	recipe, ok := InstallRecipes[pkg]
	if !ok {
		return pyWhich(pkg), nil
	}
	strategy := recipe.Strategy
	if strategy == "" {
		strategy = "manual"
	}
	binName := recipe.Bin
	if binName == "" {
		binName = pkg
	}
	existing, err := ExistingBinary(rt, binName)
	if err != nil || existing != "" {
		return existing, err
	}
	switch strategy {
	case "manual":
		rt.logf(slog.LevelDebug, "agent.lsp.install", "[install] %s requires manual install (recipe=%s)", pkg, recipeRepr(recipe))
		return "", nil
	case "npm":
		return installNpm(rt, recipe.Pkg, binName, recipe.ExtraPkgs)
	case "go":
		return installGo(rt, recipe.Pkg, binName)
	case "pip":
		return installPip(rt, recipe.Pkg, binName)
	}
	rt.logf(slog.LevelWarn, "agent.lsp.install", "[install] unknown strategy %s for %s", pyStrRepr(strategy), pkg)
	return "", nil
}

// Python dict repr of a recipe
func recipeRepr(r Recipe) string {
	s := fmt.Sprintf("{'strategy': %s, 'pkg': %s, 'bin': %s", pyStrRepr(r.Strategy), pyStrRepr(r.Pkg), pyStrRepr(r.Bin))
	if r.ExtraPkgs != nil {
		s += ", 'extra_pkgs': " + pyListRepr(r.ExtraPkgs)
	}
	return s + "}"
}

// Python list repr of strings
func pyListRepr(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = pyStrRepr(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// Captured outcome of one installer child
type installRun struct {
	code           int
	stdout, stderr string
}

// Runs one installer child with captured output, closed stdin and a hidden window, swapped by tests
var runInstallCommand = func(argv []string, timeout time.Duration, env map[string]string) (*installRun, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if env != nil {
		cmd.Env = environList(env)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr, cmd.Stdin = &stdout, &stderr, nil
	cmd.WaitDelay = time.Second
	hideWindow(cmd)
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, &TimeoutError{msg: fmt.Sprintf("Command %s timed out after %d seconds", pyListRepr(argv), int(timeout/time.Second))}
	}
	res := &installRun{stdout: decodeUTF8Replace(stdout.Bytes()), stderr: decodeUTF8Replace(stderr.Bytes())}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return nil, pyOSError(err, argv[0])
		}
		res.code = ee.ExitCode()
	}
	return res, nil
}

// Environment map as a KEY=VALUE list
func environList(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// First n code points of a string
func runePrefix(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// Runs one install child, logging and returning false on a non zero exit or an error
func runInstaller(rt *Runtime, tool, pkg string, cmd []string, timeout time.Duration, env map[string]string) bool {
	res, err := runInstallCommand(cmd, timeout, env)
	if err != nil {
		rt.logf(slog.LevelWarn, "agent.lsp.install", "[install] %s install errored for %s: %s", tool, pkg, errText(err))
		return false
	}
	if res.code != 0 {
		detail := pyStrip(res.stderr)
		if detail == "" {
			detail = pyStrip(res.stdout)
		}
		rt.logf(slog.LevelWarn, "agent.lsp.install", "[install] %s install failed for %s: %s", tool, pkg, runePrefix(detail, 500))
		return false
	}
	return true
}

// Python shutil.copy2 of one file onto a destination path
func copyFile2(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(dst, st.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(dst, st.ModTime(), st.ModTime())
}

// Symlinks or copies a target into the staging bin directory and returns the path to use
func linkIntoBin(rt *Runtime, target string) (string, error) {
	bin, err := BinDir(rt)
	if err != nil {
		return "", err
	}
	link := filepath.Join(bin, filepath.Base(target))
	if !osPathExists(link) {
		if err := os.Symlink(target, link); err != nil {
			if err := copyFile2(target, link); err != nil {
				return target, nil
			}
		}
	}
	if osPathExists(link) {
		return link, nil
	}
	return target, nil
}

// Installer argv per Node package manager, every one installing into the staging directory
var nodePMArgv = map[string]func(staging string) []string{
	"npm": func(staging string) []string {
		return []string{"install", "--prefix", staging, "--silent", "--no-fund", "--no-audit"}
	},
	"pnpm": func(staging string) []string { return []string{"add", "--dir", staging} },
	"yarn": func(staging string) []string { return []string{"--cwd", staging, "add"} },
}

// Configured Node package manager, empty for an unknown value so installs fail closed
func nodePackageManager(rt *Runtime) string {
	pm := "npm"
	if v := rt.lspConfig()["package_manager"]; pyTruthy(v) {
		pm = strings.ToLower(pyStrip(pyStr(v)))
	}
	if _, ok := nodePMArgv[pm]; !ok {
		rt.logf(slog.LevelWarn, "agent.lsp.install", "[install] lsp.package_manager=%s is not one of %s; skipping install",
			pyStrRepr(pm), pyListRepr([]string{"npm", "pnpm", "yarn"}))
		return ""
	}
	return pm
}

// Windows shim names per Node command
var windowsNodeShims = map[string][]string{
	"npm": {"npm.cmd", "npm.exe", "npm"}, "npx": {"npx.cmd", "npx.exe", "npx"}, "node": {"node.exe", "node"},
}

// Command names a Node tool may carry on this platform
func candidateNodeCommandNames(command string) []string {
	base := filepath.Base(command)
	if !isWindowsHost() || strings.Contains(base, ".") {
		return []string{base}
	}
	if names, ok := windowsNodeShims[strings.ToLower(base)]; ok {
		return names
	}
	return []string{base + ".cmd", base + ".exe", base}
}

// Managed node, npm or npx, or a user tool on PATH, empty when absent, swapped by tests
var findNodeExecutable = func(rt *Runtime, command string) (string, error) {
	if strings.ContainsAny(command, `/\`) {
		if isWindowsHost() {
			if isFile(command) {
				return command, nil
			}
			return "", nil
		}
		return pyWhich(command), nil
	}
	base := strings.ToLower(command)
	for _, suffix := range []string{".cmd", ".exe", ".ps1"} {
		base = strings.TrimSuffix(base, suffix)
	}
	pkgName := map[string]string{"node": "node", "npm": "npm", "npx": "npm"}[base]
	if pkgName != "" {
		binary, ok, err := rt.installedBinary(pkgName)
		if err != nil || !ok {
			return "", err
		}
		if base != "npx" {
			return binary, nil
		}
		for _, name := range candidateNodeCommandNames("npx") {
			if c := filepath.Join(filepath.Dir(binary), name); isFile(c) {
				return c, nil
			}
		}
		return "", nil
	}
	if !isWindowsHost() {
		return pyWhich(command), nil
	}
	for _, name := range candidateNodeCommandNames(command) {
		for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
			if dir == "" {
				continue
			}
			if c := filepath.Join(dir, name); isFile(c) {
				return c, nil
			}
		}
	}
	return "", nil
}

// Installs with the configured Node package manager into the staging tree and links its binary
func installNpm(rt *Runtime, pkg, binName string, extraPkgs []string) (string, error) {
	pm := nodePackageManager(rt)
	if pm == "" {
		return "", nil
	}
	pmBin, err := findNodeExecutable(rt, pm)
	if err != nil {
		return "", err
	}
	if pmBin == "" && pm == "npm" {
		if rt == nil || rt.EnsureEnv == nil {
			return "", missingHook("EnsureEnv")
		}
		_, err := guard(func() (map[string]string, error) { return rt.EnsureEnv("npm", nil) })
		if err == nil {
			pmBin, err = findNodeExecutable(rt, "npm")
		}
		if err != nil {
			rt.logf(slog.LevelWarn, "agent.lsp.install", "[install] cannot install %s: managed npm unavailable (%s)", pkg, errText(err))
			return "", nil
		}
	}
	if pmBin == "" {
		rt.logf(slog.LevelWarn, "agent.lsp.install", "[install] cannot install %s: lsp.package_manager is %s but no usable %s was found "+
			"(install it, or set lsp.package_manager: npm)", pkg, pyStrRepr(pm), pm)
		return "", nil
	}
	bin, err := BinDir(rt)
	if err != nil {
		return "", err
	}
	staging := filepath.Dir(bin)
	cmd := append([]string{pmBin}, nodePMArgv[pm](staging)...)
	cmd = append(cmd, pkg)
	cmd = append(cmd, extraPkgs...)
	rt.logf(slog.LevelInfo, "agent.lsp.install", "[install] %s %s", pm, strings.Join(cmd[1:], " "))
	base, err := rt.subprocessEnv()
	if err != nil {
		return "", err
	}
	env, err := rt.envFor([]string{"npm"}, base)
	if err != nil {
		return "", err
	}
	if !runInstaller(rt, pm, pkg, cmd, 300*time.Second, env) {
		return "", nil
	}
	found := firstExisting(isWindowsHost(), filepath.Join(staging, "node_modules", ".bin", binName))
	if found != "" {
		if ext := strings.ToLower(filepath.Ext(found)); isWindowsHost() && (ext == ".cmd" || ext == ".bat") {
			return found, nil
		}
		return linkIntoBin(rt, found)
	}
	rt.logf(slog.LevelWarn, "agent.lsp.install", "[install] %s install for %s succeeded but bin %s not found", pm, pkg, binName)
	return "", nil
}

// Installs a Go module with GOBIN pointed at the staging directory
func installGo(rt *Runtime, pkg, binName string) (string, error) {
	goBin := pyWhich("go")
	if goBin == "" {
		rt.logf(slog.LevelInfo, "agent.lsp.install", "[install] cannot install %s: go not on PATH", pkg)
		return "", nil
	}
	staging, err := BinDir(rt)
	if err != nil {
		return "", err
	}
	rt.logf(slog.LevelInfo, "agent.lsp.install", "[install] go install %s (GOBIN=%s)", pkg, staging)
	env, err := rt.subprocessEnv()
	if err != nil {
		return "", err
	}
	env["GOBIN"] = staging
	if !runInstaller(rt, "go", pkg, []string{goBin, "install", pkg}, 600*time.Second, env) {
		return "", nil
	}
	binPath := filepath.Join(staging, binName)
	if isWindowsHost() {
		root, _ := pySplitext(binPath)
		binPath = root + ".exe"
	}
	if osPathExists(binPath) {
		return binPath, nil
	}
	rt.logf(slog.LevelWarn, "agent.lsp.install", "[install] go install for %s succeeded but bin %s not found", pkg, binName)
	return "", nil
}

// Provisions a Python server in its own managed environment
func installPip(rt *Runtime, pkg, binName string) (string, error) {
	if rt == nil || rt.EnsurePythonTool == nil {
		return "", missingHook("EnsurePythonTool")
	}
	path, err := guard(func() (string, error) {
		return rt.EnsurePythonTool("lsp-"+binName, []string{pkg}, binName, 300*time.Second)
	})
	if err != nil {
		rt.logf(slog.LevelWarn, "agent.lsp.install", "[install] Python server install failed for %s: %s", pkg, errText(err))
		return "", nil
	}
	return path, nil
}

// DetectStatus reports installed, missing or manual-only for a recipe key without spawning anything
func DetectStatus(rt *Runtime, pkg string) (string, error) {
	recipe, ok := InstallRecipes[pkg]
	bin, err := ExistingBinary(rt, recipeBin(pkg))
	if err != nil {
		return "", err
	}
	if bin != "" {
		return "installed", nil
	}
	if ok && recipe.Strategy == "manual" {
		return "manual-only", nil
	}
	return "missing", nil
}
