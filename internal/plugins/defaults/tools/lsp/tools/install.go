package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
	lspconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/lsp/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Ways a recipe installs its server
const (
	npmInstall    = "npm"
	goInstall     = "go"
	pipInstall    = "pip"
	manualInstall = "manual"
)

// How one server package installs and the binary it gives
type recipe struct {
	strategy string
	pkg      string
	bin      string
	extra    []string
}

// The last TypeScript line that ships a JavaScript SDK, which the servers that host TypeScript load
const typescriptSDKPkg = "typescript@6"

// Recipes by their key
var recipes = map[string]recipe{
	"pyright":                           {npmInstall, "pyright", "pyright-langserver", nil},
	"typescript-language-server":        {npmInstall, "typescript-language-server", "typescript-language-server", []string{typescriptSDKPkg}},
	"@vue/language-server":              {npmInstall, "@vue/language-server@2", "vue-language-server", []string{typescriptSDKPkg}},
	"svelte-language-server":            {npmInstall, "svelte-language-server", "svelteserver", nil},
	"@astrojs/language-server":          {npmInstall, "@astrojs/language-server", "astro-ls", nil},
	"yaml-language-server":              {npmInstall, "yaml-language-server", "yaml-language-server", nil},
	"bash-language-server":              {npmInstall, "bash-language-server", "bash-language-server", nil},
	"intelephense":                      {npmInstall, "intelephense", "intelephense", nil},
	"dockerfile-language-server-nodejs": {npmInstall, "dockerfile-language-server-nodejs", "docker-langserver", nil},
	"gopls":                             {goInstall, "golang.org/x/tools/gopls@latest", "gopls", nil},
	"rust-analyzer":                     {manualInstall, "", "rust-analyzer", nil},
	"clangd":                            {manualInstall, "", "clangd", nil},
	"lua-language-server":               {manualInstall, "", "lua-language-server", nil},
	"laravel-lsp":                       {manualInstall, "", "laravel-lsp", nil},
	"powershell":                        {manualInstall, "", "pwsh", nil},
}

// Recipe key of a server where the two differ
var recipeOf = map[string]string{"vue-language-server": "@vue/language-server", "astro-language-server": "@astrojs/language-server",
	"dockerfile-ls": "dockerfile-language-server-nodejs", "typescript": "typescript-language-server"}

func recipeKey(server string) string {
	if key, ok := recipeOf[server]; ok {
		return key
	}
	return server
}

// Binary a recipe key gives, the key itself when no recipe names one
func binOf(key string) string {
	if r, ok := recipes[key]; ok && r.bin != "" {
		return r.bin
	}
	return key
}

// Install states of a server
const (
	installed  = "installed"
	missing    = "missing"
	manualOnly = "manual-only"
)

// Folder the profile's language servers are installed in
func (m *Manager) dir(cfg *config.Config) string {
	return filepath.Join(cfg.ProfileDir(cfg.Profile()), "lsp")
}

// Folder the installed binaries are linked into, made when missing
func (m *Manager) binDir(cfg *config.Config) (string, error) {
	dir := filepath.Join(m.dir(cfg), "bin")
	return dir, os.MkdirAll(dir, 0o777)
}

// Names a staged binary may have on this machine, the wrappers Windows runs before the bare name
func candidates(base string) []string {
	if runtime.GOOS != "windows" {
		return []string{base}
	}
	return []string{base + ".cmd", base + ".exe", base + ".bat", base}
}

// Binary of a name that is there already: staged in the profile, npm's own bin folder on Windows, a Python server's environment, then PATH
func (m *Manager) existing(cfg *config.Config, name string) (string, error) {
	bin, err := m.binDir(cfg)
	if err != nil {
		return "", err
	}
	bases := []string{filepath.Join(bin, name)}
	if runtime.GOOS == "windows" {
		bases = append(bases, filepath.Join(m.dir(cfg), "node_modules", ".bin", name))
	}
	for _, base := range bases {
		for _, staged := range candidates(base) {
			if fsx.Exists(staged) && executable(staged) {
				return staged, nil
			}
		}
	}
	if pipBinary(name) {
		path, found, err := m.packages.PythonTool(cfg, "lsp-"+name, name)
		if err != nil {
			m.logf(slog.LevelWarn, "[install] cannot read Python server %s: %s", name, err)
		} else if found {
			return path, nil
		}
	}
	suffixes := []string{""}
	if runtime.GOOS == "windows" {
		suffixes = []string{".cmd", ".exe", ".bat", ""}
	}
	for _, suffix := range suffixes {
		if path := which(name + suffix); path != "" {
			return path, nil
		}
	}
	return "", nil
}

// Whether a pip recipe gives the binary
func pipBinary(name string) bool {
	for _, r := range recipes {
		if r.strategy == pipInstall && r.bin == name {
			return true
		}
	}
	return false
}

// Install state of a recipe key, nothing started
func (m *Manager) status(cfg *config.Config, key string) (string, error) {
	bin, err := m.existing(cfg, binOf(key))
	if err != nil {
		return "", err
	}
	if bin != "" {
		return installed, nil
	}
	if r, ok := recipes[key]; ok && r.strategy == manualInstall {
		return manualOnly, nil
	}
	return missing, nil
}

// Installs done by key, each tried once a process unless a person asks again
type installs struct {
	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	results map[string]string
}

// Binary of a recipe: installed under the auto strategy, else only looked for; explicit when a person asked, which tries again what failed
// before and installs the package manager's npm whatever lazy installs allow
func (m *Manager) install(ctx context.Context, cfg *config.Config, key, strategy string, explicit bool) (string, error) {
	if strategy != lspconfig.InstallAuto {
		return m.existing(cfg, binOf(key))
	}
	id := cfg.Profile() + "\x00" + key
	m.installs.mu.Lock()
	if path, ok := m.installs.results[id]; ok && (!explicit || path != "") {
		m.installs.mu.Unlock()
		return path, nil
	}
	lock := m.installs.locks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		m.installs.locks[id] = lock
	}
	m.installs.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()
	m.installs.mu.Lock()
	path, ok := m.installs.results[id]
	m.installs.mu.Unlock()
	if ok && (!explicit || path != "") {
		return path, nil
	}
	path, err := m.installRecipe(ctx, cfg, key, explicit)
	if err == nil {
		m.installs.mu.Lock()
		m.installs.results[id] = path
		m.installs.mu.Unlock()
	}
	return path, err
}

// Installs one recipe after looking for what is there, by its strategy
func (m *Manager) installRecipe(ctx context.Context, cfg *config.Config, key string, explicit bool) (string, error) {
	r, ok := recipes[key]
	if !ok {
		return which(key), nil
	}
	strategy := r.strategy
	if strategy == "" {
		strategy = manualInstall
	}
	name := r.bin
	if name == "" {
		name = key
	}
	if found, err := m.existing(cfg, name); err != nil || found != "" {
		return found, err
	}
	switch strategy {
	case manualInstall:
		m.installf(ctx, slog.LevelDebug, "%s requires manual install (recipe=%s)", key, describe(r))
		return "", nil
	case npmInstall:
		return m.installNode(ctx, cfg, r.pkg, name, r.extra, explicit)
	case goInstall:
		return m.installGo(ctx, cfg, r.pkg, name)
	case pipInstall:
		return m.installPython(ctx, cfg, r.pkg, name, explicit)
	}
	m.installf(ctx, slog.LevelWarn, "There is no way to install %s that icebath knows, as its install method “%s” is unknown.", key, strategy)
	return "", nil
}

// Recipe as compact JSON for the log
func describe(r recipe) string {
	fields := map[string]any{"strategy": r.strategy, "pkg": r.pkg, "bin": r.bin}
	if r.extra != nil {
		fields["extra_pkgs"] = r.extra
	}
	data, _ := json.Marshal(fields)
	return string(data)
}

// Asks the context of an install to tell its lines to the person who asked for it, rather than the log
func withSay(ctx context.Context, say func(text string)) context.Context {
	return context.WithValue(ctx, sayKey{}, say)
}

type sayKey struct{}

// Line of an install, told to the person who asked for it unless it is a detail, else logged
func (m *Manager) installf(ctx context.Context, level slog.Level, format string, args ...any) {
	if say, ok := ctx.Value(sayKey{}).(func(text string)); ok && level >= slog.LevelInfo {
		say(fmt.Sprintf(format, args...))
		return
	}
	m.logf(level, "[install] "+format, args...)
}

// Runs one installer child with the runner's environment, its output captured and no input, false after logging when it fails
func (m *Manager) run(ctx context.Context, runner *packages.Runner, pkg string, timeout time.Duration, name string, args ...string) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := runner.Command(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, &stdout, &stderr
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	tool := runner.Name
	if ctx.Err() == context.DeadlineExceeded {
		m.installf(ctx, slog.LevelWarn, "Installing %s with %s took longer than %s, so it was stopped.", pkg, tool, strs.Span(timeout))
		return false
	}
	if err != nil {
		if _, exited := errors.AsType[*exec.ExitError](err); !exited {
			m.installf(ctx, slog.LevelWarn, "Could not install %s with %s: %s.", pkg, tool, err)
			return false
		}
		detail := strings.TrimSpace(strings.ToValidUTF8(stderr.String(), "\uFFFD"))
		if detail == "" {
			detail = strings.TrimSpace(strings.ToValidUTF8(stdout.String(), "\uFFFD"))
		}
		if runes := []rune(detail); len(runes) > 500 {
			detail = string(runes[:500])
		}
		m.installf(ctx, slog.LevelWarn, "Could not install %s with %s. It said: %s", pkg, tool, detail)
		return false
	}
	return true
}

// Copy of a file with its mode and times
func copyFile(src, dst string) error {
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

// Links or copies an installed binary into the profile's bin folder, the path to run it by
func (m *Manager) link(cfg *config.Config, target string) (string, error) {
	bin, err := m.binDir(cfg)
	if err != nil {
		return "", err
	}
	link := filepath.Join(bin, filepath.Base(target))
	if !fsx.Exists(link) {
		if os.Symlink(target, link) != nil && copyFile(target, link) != nil {
			return target, nil
		}
	}
	if fsx.Exists(link) {
		return link, nil
	}
	return target, nil
}

// Arguments each Node package manager installs into the profile's folder with
var nodeArgs = map[string]func(dir string) []string{
	"npm": func(dir string) []string {
		return []string{"install", "--prefix", dir, "--silent", "--no-fund", "--no-audit"}
	},
	"pnpm": func(dir string) []string { return []string{"add", "--dir", dir} },
	"yarn": func(dir string) []string { return []string{"--cwd", dir, "add"} },
}

// Node package manager the settings name, empty for one that isn't known so installs fail closed
func (m *Manager) nodeManager(ctx context.Context, cfg *config.Config) string {
	pm := strings.ToLower(strings.TrimSpace(cfg.GetString(lspconfig.PackageManager)))
	if pm == "" {
		pm = "npm"
	}
	if _, ok := nodeArgs[pm]; !ok {
		m.installf(ctx, slog.LevelWarn, "The Node package manager is set to “%s”, which is not npm, pnpm or yarn, so nothing was installed. Choose one "+
			"in /settings under Tools › Language servers › Node package manager.", pm)
		return ""
	}
	return pm
}

// Node, npm or npx of the package manager, or another tool found on PATH; empty when it is not there
func (m *Manager) nodeTool(command string) (string, error) {
	if strings.ContainsAny(command, `/\`) {
		if runtime.GOOS == "windows" {
			if fsx.IsFile(command) {
				return command, nil
			}
			return "", nil
		}
		return which(command), nil
	}
	base := strings.ToLower(command)
	for _, suffix := range []string{".cmd", ".exe", ".ps1"} {
		base = strings.TrimSuffix(base, suffix)
	}
	pkg := map[string]string{"node": packages.Node, "npm": packages.NPM, "npx": packages.NPM}[base]
	if pkg == "" {
		return which(command), nil
	}
	got, err := m.packages.Installed(pkg, false)
	if err != nil || got == nil || got.Binary == "" {
		return "", err
	}
	if base != "npx" {
		return got.Binary, nil
	}
	names := []string{"npx"}
	if runtime.GOOS == "windows" {
		names = []string{"npx.cmd", "npx.exe", "npx"}
	}
	for _, name := range names {
		if candidate := filepath.Join(filepath.Dir(got.Binary), name); fsx.IsFile(candidate) {
			return candidate, nil
		}
	}
	return "", nil
}

// Installs a Node server with the package manager the settings name into the profile's folder and links its binary
func (m *Manager) installNode(ctx context.Context, cfg *config.Config, pkg, name string, extra []string, explicit bool) (string, error) {
	pm := m.nodeManager(ctx, cfg)
	if pm == "" {
		return "", nil
	}
	tool, err := m.nodeTool(pm)
	if err != nil {
		return "", err
	}
	if tool == "" && pm == "npm" {
		if _, err := m.packages.Ensure(ctx, cfg, packages.NPM, packages.Ensuring{Explicit: explicit}); err != nil {
			m.installf(ctx, slog.LevelWarn, "Could not install %s, because npm could not be set up: %s", pkg, err)
			return "", nil
		}
		if tool, err = m.nodeTool("npm"); err != nil {
			return "", err
		}
	}
	if tool == "" {
		m.installf(ctx, slog.LevelWarn, "Could not install %s, because %s was not found. Install %s, or choose npm in /settings under "+
			"Tools › Language servers › Node package manager.", pkg, pm, pm)
		return "", nil
	}
	if _, err := m.binDir(cfg); err != nil {
		return "", err
	}
	dir := m.dir(cfg)
	args := append(append(nodeArgs[pm](dir), pkg), extra...)
	m.installf(ctx, slog.LevelInfo, "Running %s %s", pm, strings.Join(args, " "))
	runner := &packages.Runner{Name: pm, Env: m.packages.EnvFor([]string{packages.NPM}, m.terminal.ChildEnv(cfg, nil))}
	if !m.run(ctx, runner, pkg, 300*time.Second, tool, args...) {
		return "", nil
	}
	if pm == "npm" {
		if err := m.packages.RecordTree(dir); err != nil {
			m.installf(ctx, slog.LevelDebug, "%s could not be recorded for pm update --npm: %s", dir, err)
		}
	}
	for _, found := range candidates(filepath.Join(dir, "node_modules", ".bin", name)) {
		if !fsx.Exists(found) {
			continue
		}
		if ext := strings.ToLower(filepath.Ext(found)); runtime.GOOS == "windows" && (ext == ".cmd" || ext == ".bat") {
			return found, nil
		}
		return m.link(cfg, found)
	}
	m.installf(ctx, slog.LevelWarn, "%s installed %s, but its program %s was not found.", pm, pkg, name)
	return "", nil
}

// Installs a Go module with GOBIN at the profile's bin folder
func (m *Manager) installGo(ctx context.Context, cfg *config.Config, pkg, name string) (string, error) {
	goBin := which("go")
	if goBin == "" {
		m.installf(ctx, slog.LevelInfo, "Could not install %s, because Go is not installed or not on your PATH.", pkg)
		return "", nil
	}
	bin, err := m.binDir(cfg)
	if err != nil {
		return "", err
	}
	m.installf(ctx, slog.LevelInfo, "Running go install %s into %s", pkg, bin)
	env := m.terminal.ChildEnv(cfg, nil)
	env["GOBIN"] = bin
	if !m.run(ctx, &packages.Runner{Name: "go", Env: env}, pkg, 600*time.Second, goBin, "install", pkg) {
		return "", nil
	}
	path := filepath.Join(bin, name)
	if runtime.GOOS == "windows" {
		path = strings.TrimSuffix(path, filepath.Ext(path)) + ".exe"
	}
	if fsx.Exists(path) {
		return path, nil
	}
	m.installf(ctx, slog.LevelWarn, "Go installed %s, but its program %s was not found.", pkg, name)
	return "", nil
}

// Installs a Python server in an environment of its own the package manager keeps
func (m *Manager) installPython(ctx context.Context, cfg *config.Config, pkg, name string, explicit bool) (string, error) {
	if _, err := m.packages.Environment(ctx, cfg, packages.EnvironmentOptions{Name: "lsp-" + name, Requirements: []string{pkg}, Explicit: explicit,
		Timeout: 300 * time.Second, Executable: name}); err != nil {
		m.installf(ctx, slog.LevelWarn, "Could not install %s: %s", pkg, err)
		return "", nil
	}
	path, found, err := m.packages.PythonTool(cfg, "lsp-"+name, name)
	if err != nil || !found {
		return "", err
	}
	return path, nil
}

// Servers that spawn but stay silent for want of a companion tool
func (m *Manager) warnings(cfg *config.Config) ([]string, error) {
	bash, err := m.existing(cfg, "bash-language-server")
	if err != nil {
		return nil, err
	}
	if bash != "" && which("shellcheck") == "" {
		return []string{"bash-language-server is installed, but shellcheck is missing, so shell scripts get no checks. Install shellcheck with " +
			"apt, brew or scoop."}, nil
	}
	return nil, nil
}

// Built in servers with a recipe that installs by itself, those with a manual one too when asked
func recipeServers(manual bool) []*server {
	var out []*server
	for _, s := range builtins {
		if r, ok := recipes[recipeKey(s.id)]; ok && (manual || r.strategy != manualInstall) {
			out = append(out, s)
		}
	}
	return out
}
