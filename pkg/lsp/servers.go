package lsp

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// LSP languageId per language for didOpen, in Python dict order
var extsByLanguage = []struct {
	lang string
	exts []string
}{
	{"python", []string{".py", ".pyi"}},
	{"typescript", []string{".ts", ".mts", ".cts"}},
	{"typescriptreact", []string{".tsx"}},
	{"javascript", []string{".js", ".mjs", ".cjs"}},
	{"javascriptreact", []string{".jsx"}},
	{"vue", []string{".vue"}}, {"svelte", []string{".svelte"}}, {"astro", []string{".astro"}},
	{"go", []string{".go"}}, {"rust", []string{".rs"}},
	{"ruby", []string{".rb", ".rake", ".gemspec", ".ru"}},
	{"c", []string{".c", ".h"}},
	{"cpp", []string{".cc", ".cpp", ".cxx", ".hh", ".hpp", ".hxx"}},
	{"csharp", []string{".cs", ".csx"}}, {"fsharp", []string{".fs", ".fsi", ".fsx"}},
	{"swift", []string{".swift"}}, {"java", []string{".java"}}, {"kotlin", []string{".kt", ".kts"}},
	{"yaml", []string{".yaml", ".yml"}}, {"json", []string{".json"}}, {"jsonc", []string{".jsonc"}},
	{"lua", []string{".lua"}}, {"php", []string{".php"}}, {"blade", []string{".blade.php"}},
	{"prisma", []string{".prisma"}}, {"dart", []string{".dart"}},
	{"ocaml", []string{".ml", ".mli"}},
	{"shellscript", []string{".sh", ".bash", ".zsh"}},
	{"terraform", []string{".tf", ".tfvars"}},
	{"latex", []string{".tex"}}, {"bibtex", []string{".bib"}}, {"gleam", []string{".gleam"}},
	{"clojure", []string{".clj", ".cljc", ".edn"}}, {"clojurescript", []string{".cljs"}},
	{"nix", []string{".nix"}}, {"typst", []string{".typ", ".typc"}}, {"haskell", []string{".hs", ".lhs"}},
	{"julia", []string{".jl"}}, {"elixir", []string{".ex", ".exs"}}, {"zig", []string{".zig", ".zon"}},
	{"dockerfile", []string{".dockerfile"}},
	{"powershell", []string{".ps1", ".psm1", ".psd1"}},
}

// LanguageByExt maps an extension or basename to its didOpen languageId
var LanguageByExt = func() map[string]string {
	out := map[string]string{}
	for _, e := range extsByLanguage {
		for _, ext := range e.exts {
			out[ext] = e.lang
		}
	}
	return out
}()

// RootFn resolves the per server project root for a file in a workspace, empty to skip the file
type RootFn func(filePath, workspace string) string

// SpawnFn assembles the spawn command for a root, nil when the binary cannot be found
type SpawnFn func(root string, ctx ServerContext) (*SpawnSpec, error)

// SpawnSpec is the resolved spawn of a server for one root
type SpawnSpec struct {
	Command                    []string
	WorkspaceRoot              string
	Cwd                        string
	Env                        map[string]string
	InitializationOptions      map[string]any
	SeedDiagnosticsOnFirstPush bool
}

// ServerDef is one language server entry of the registry
type ServerDef struct {
	ServerID      string
	Extensions    []string
	ResolveRoot   RootFn
	BuildSpawn    SpawnFn
	SeedFirstPush bool
	Description   string
	MultiRoot     bool
	LanguageID    string
}

// Matches reports whether the server handles a file by extension or basename
func (s *ServerDef) Matches(filePath string) bool {
	key := fileExtOrBasename(filePath)
	for _, e := range s.Extensions {
		if e == key {
			return true
		}
	}
	return false
}

// Handles reports whether the server lists an extension
func (s *ServerDef) Handles(ext string) bool {
	for _, e := range s.Extensions {
		if e == ext {
			return true
		}
	}
	return false
}

// ServerContext is the user policy a spawn builder consults
type ServerContext struct {
	WorkspaceRoot   string
	InstallStrategy string
	BinaryOverrides map[string][]string
	EnvOverrides    map[string]map[string]string
	InitOverrides   map[string]map[string]any
	Trusted         bool
	Runtime         *Runtime
}

// Multi part extensions naming another language than their last segment
var compoundExts = []string{".blade.php"}

// Lower cased extension with compound ones first, or the basename of an extensionless file
func fileExtOrBasename(path string) string {
	base := pypath.Basename(path)
	lower := strings.ToLower(base)
	for _, c := range compoundExts {
		if strings.HasSuffix(lower, c) {
			return c
		}
	}
	if _, ext := pypath.Splitext(base); ext != "" {
		return strings.ToLower(ext)
	}
	return base
}

// Python shutil.which for one command name
func pyWhich(name string) string {
	p, err := exec.LookPath(name)
	if err != nil && !errors.Is(err, exec.ErrDot) {
		return ""
	}
	if st, statErr := os.Stat(p); statErr != nil || st.IsDir() {
		return ""
	}
	return p
}

// Full path of the first command found on PATH, swapped by tests that stage binaries
var which = func(names ...string) string {
	for _, n := range names {
		if p := pyWhich(n); p != "" {
			return p
		}
	}
	return ""
}

// Marker root with the workspace as fallback, empty only when an exclude marker fired
func rootOrWorkspace(filePath, workspace string, markers, excludes []string) string {
	ceiling := ""
	if workspace != "" {
		ceiling = filepath.Dir(workspace)
	}
	found := NearestRoot(filePath, markers, excludes, ceiling)
	if found == "" && len(excludes) > 0 && NearestRoot(filePath, markers, nil, ceiling) != "" {
		return ""
	}
	if found != "" {
		return found
	}
	return workspace
}

// Root resolver over marker files, nil markers meaning always the workspace root
func markersRoot(markers, excludes []string) RootFn {
	if markers == nil {
		return func(_, ws string) string { return ws }
	}
	return func(fp, ws string) string { return rootOrWorkspace(fp, ws, markers, excludes) }
}

// Python os.path.exists
func osPathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Config override, then PATH, then an optional auto install, empty when nothing resolves
func findBinary(ctx ServerContext, serverID string, names []string, installPkg string) (string, error) {
	binPath := ""
	if override := ctx.BinaryOverrides[serverID]; len(override) > 0 && override[0] != "" && osPathExists(override[0]) {
		binPath = override[0]
	} else {
		binPath = which(names...)
	}
	if binPath == "" && installPkg != "" {
		return TryInstall(ctx.Runtime, installPkg, ctx.InstallStrategy)
	}
	return binPath, nil
}

// Spawn spec with config init overrides merged over a base and the managed Node env
func makeSpec(root string, ctx ServerContext, serverID string, command []string, baseInit map[string]any, seed bool) (*SpawnSpec, error) {
	init := maps.Clone(ctx.InitOverrides[serverID])
	if init == nil {
		init = map[string]any{}
	}
	if baseInit != nil {
		merged := maps.Clone(baseInit)
		maps.Copy(merged, ctx.InitOverrides[serverID])
		init = merged
	}
	env, err := ctx.Runtime.envFor([]string{"node"}, nil)
	if err != nil {
		return nil, err
	}
	env = maps.Clone(env)
	if env == nil {
		env = map[string]string{}
	}
	maps.Copy(env, ctx.EnvOverrides[serverID])
	return &SpawnSpec{Command: command, WorkspaceRoot: root, Cwd: root, Env: env, InitializationOptions: init,
		SeedDiagnosticsOnFirstPush: seed}, nil
}

// Spawn builder for the single binary server shape, untrusted init replacing base init off trust
func simpleSpawn(serverID string, names, args []string, installPkg string, baseInit map[string]any, seed bool, untrustedInit map[string]any) SpawnFn {
	return func(root string, ctx ServerContext) (*SpawnSpec, error) {
		binPath, err := findBinary(ctx, serverID, names, installPkg)
		if err != nil {
			return nil, err
		}
		init := baseInit
		if !ctx.Trusted && untrustedInit != nil {
			init = untrustedInit
		}
		if binPath == "" {
			return nil, nil
		}
		return makeSpec(root, ctx, serverID, append([]string{binPath}, args...), init, seed)
	}
}

// Pyright pointed at the project interpreter when trusted, the operator's otherwise
func spawnPyright(root string, ctx ServerContext) (*SpawnSpec, error) {
	binPath, err := findBinary(ctx, "pyright", []string{"pyright-langserver", "pyright"}, "pyright")
	if err != nil || binPath == "" {
		return nil, err
	}
	stem, suffix := pypath.Splitext(pypath.Basename(binPath))
	if stem == "pyright" {
		sibling := filepath.Join(filepath.Dir(binPath), "pyright-langserver"+suffix)
		if osPathExists(sibling) {
			binPath = sibling
		}
	}
	projectRoot := ""
	if ctx.Trusted {
		projectRoot = root
	}
	py, err := detectPython(ctx.Runtime, projectRoot)
	if err != nil {
		return nil, err
	}
	base := map[string]any{}
	if py != "" {
		base = map[string]any{"python": map[string]any{"pythonPath": py}}
	}
	return makeSpec(root, ctx, "pyright", []string{binPath, "--stdio"}, base, false)
}

// Interpreter for pyright, the operator venv then the project venv then the managed Python
func detectPython(rt *Runtime, root string) (string, error) {
	var venvs []string
	if v := os.Getenv("VIRTUAL_ENV"); v != "" {
		venvs = append(venvs, v)
	}
	if root != "" {
		venvs = append(venvs, filepath.Join(root, ".venv"), filepath.Join(root, "venv"))
	}
	for _, v := range venvs {
		for _, sub := range []string{"bin/python", "bin/python3", "Scripts/python.exe"} {
			if p := pypath.Join(v, sub); osPathExists(p) {
				return p, nil
			}
		}
	}
	bin, ok, err := rt.installedBinary("python")
	if err != nil || !ok {
		return "", err
	}
	return bin, nil
}

var (
	warnedMu   sync.Mutex
	warnedOnce = map[string]bool{}
)

// Logs a warning the first time its key is seen in this process
func warnOnce(rt *Runtime, key, message string) {
	warnedMu.Lock()
	seen := warnedOnce[key]
	warnedOnce[key] = true
	warnedMu.Unlock()
	if !seen {
		rt.logf(slog.LevelWarn, "agent.lsp.servers", "%s", message)
	}
}

// bash-language-server, warning once when shellcheck is missing
func spawnBashLS(root string, ctx ServerContext) (*SpawnSpec, error) {
	binPath, err := findBinary(ctx, "bash-language-server", []string{"bash-language-server"}, "bash-language-server")
	if err != nil || binPath == "" {
		return nil, err
	}
	if which("shellcheck") == "" {
		warnOnce(ctx.Runtime, "shellcheck", "bash-language-server: shellcheck not found on PATH — diagnostics will be empty "+
			"until shellcheck is installed (apt: shellcheck, brew: shellcheck, scoop: shellcheck).")
	}
	return makeSpec(root, ctx, "bash-language-server", []string{binPath, "start"}, nil, false)
}

const vueReinstall = "delete <ICEBATH_HOME>/lsp/node_modules/@vue and <ICEBATH_HOME>/lsp/bin/vue-language-server*, " +
	"then run: icebath lsp install vue-language-server"

const vueTunnelMsg = "vue-language-server: the installed @vue/language-server is 3.x, which only works behind a client-hosted " +
	"tsserver tunnel Icebath does not run — no diagnostics will arrive. Reinstall the self-hosting 2.x line: " + vueReinstall

const vueTsdkMsg = "vue-language-server: no JavaScript TypeScript SDK (typescript/lib/typescript.js) next to the server or under " +
	"the project's node_modules — diagnostics are skipped. Reinstall (the recipe co-installs one): " + vueReinstall

// node_modules trees that may hold a server and its TypeScript SDK, launcher tree first
func nodeModulesTrees(rt *Runtime, binPath, root string) ([]string, error) {
	binDir, err := BinDir(rt)
	if err != nil {
		return nil, err
	}
	trees := []string{filepath.Join(filepath.Dir(binDir), "node_modules")}
	if root != "" {
		trees = append(trees, filepath.Join(root, "node_modules"))
	}
	real, err := pypath.Realpath(binPath, pypath.Lax)
	if err != nil {
		return nil, err
	}
	marker := string(filepath.Separator) + "node_modules" + string(filepath.Separator)
	if idx := strings.LastIndex(real, marker); idx >= 0 {
		trees = append([]string{real[:idx+len(marker)-1]}, trees...)
	}
	return trees, nil
}

// Major version of the first @vue/language-server found, zero when unreadable
func vueServerMajor(rt *Runtime, trees []string) (int, error) {
	for _, tree := range trees {
		raw, err := os.ReadFile(filepath.Join(tree, "@vue", "language-server", "package.json"))
		if err != nil {
			continue
		}
		if !utf8.Valid(raw) {
			continue
		}
		v, err := rt.jsonDecoder()(string(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))))
		if err != nil {
			var jde *JSONDecodeError
			var pe *pyExc
			if errors.As(err, &jde) || (errors.As(err, &pe) && pe.typ == "ValueError") {
				continue
			}
			return 0, err
		}
		m, ok := v.(map[string]any)
		if !ok {
			return 0, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(v))
		}
		version, ok := m["version"]
		if !ok {
			version = ""
		}
		head, _, _ := strings.Cut(pyrepr.Str(version), ".")
		n, err := pyInt(head)
		if err != nil {
			continue
		}
		return int(n), nil
	}
	return 0, nil
}

// First typescript lib directory holding a JavaScript typescript.js
func typescriptSDKDir(trees []string) string {
	for _, tree := range trees {
		c := filepath.Join(tree, "typescript", "lib")
		if st, err := os.Stat(filepath.Join(c, "typescript.js")); err == nil && st.Mode().IsRegular() {
			return c
		}
	}
	return ""
}

// @vue/language-server 2.x self hosting TypeScript with hybrid mode off and an explicit SDK
func spawnVue(root string, ctx ServerContext) (*SpawnSpec, error) {
	binPath, err := findBinary(ctx, "vue-language-server", []string{"vue-language-server"}, "@vue/language-server")
	if err != nil || binPath == "" {
		return nil, err
	}
	trees, err := nodeModulesTrees(ctx.Runtime, binPath, root)
	if err != nil {
		return nil, err
	}
	major, err := vueServerMajor(ctx.Runtime, trees)
	if err != nil {
		return nil, err
	}
	if major >= 3 {
		warnOnce(ctx.Runtime, "vue-tunnel", vueTunnelMsg)
		return nil, nil
	}
	tsdk := typescriptSDKDir(trees)
	if tsdk == "" {
		warnOnce(ctx.Runtime, "vue-tsdk", vueTsdkMsg)
		return nil, nil
	}
	return makeSpec(root, ctx, "vue-language-server", []string{binPath, "--stdio"},
		map[string]any{"typescript": map[string]any{"tsdk": tsdk}, "vue": map[string]any{"hybridMode": false}}, false)
}

const tsUntrustedMsg = "typescript-language-server: no TypeScript SDK next to the server, and this workspace is untrusted so its " +
	"own node_modules/typescript is not loaded — diagnostics are skipped. Reinstall: icebath lsp install " +
	"typescript-language-server, or list the workspace under lsp.trusted_workspaces."

// typescript-language-server pinned to the staged SDK in an untrusted workspace
func spawnTypescript(root string, ctx ServerContext) (*SpawnSpec, error) {
	binPath, err := findBinary(ctx, "typescript", []string{"typescript-language-server"}, "typescript-language-server")
	if err != nil || binPath == "" {
		return nil, err
	}
	base := map[string]any{}
	if !ctx.Trusted {
		trees, err := nodeModulesTrees(ctx.Runtime, binPath, "")
		if err != nil {
			return nil, err
		}
		sdk := typescriptSDKDir(trees)
		if sdk == "" {
			warnOnce(ctx.Runtime, "ts-untrusted", tsUntrustedMsg)
			return nil, nil
		}
		base = map[string]any{"tsserver": map[string]any{"path": sdk}}
	}
	return makeSpec(root, ctx, "typescript", []string{binPath, "--stdio"}, base, true)
}

// Python os.path.isfile
func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// PowerShellEditorServices bundle from the command override, init override, env, then the staged copy
func findPSESBundle(ctx ServerContext) string {
	var candidates []string
	if override := ctx.BinaryOverrides["powershell"]; len(override) > 0 {
		candidates = append(candidates, override[0])
	}
	if init := ctx.InitOverrides["powershell"]; init != nil && pyrepr.Truthy(init["bundlePath"]) {
		candidates = append(candidates, pyrepr.Str(init["bundlePath"]))
	}
	candidates = append(candidates, os.Getenv("PSES_BUNDLE_PATH"),
		filepath.Join(ctx.Runtime.home(), "lsp", "PowerShellEditorServices"))
	for _, cand := range candidates {
		if cand == "" {
			continue
		}
		if isFile(filepath.Join(cand, "PowerShellEditorServices", "Start-EditorServices.ps1")) {
			return cand
		}
		if isFile(filepath.Join(cand, "Start-EditorServices.ps1")) {
			return filepath.Dir(cand)
		}
	}
	return ""
}

const psesMissingMsg = "powershell: pwsh found but the PowerShellEditorServices bundle is missing. Download the release zip from " +
	"https://github.com/PowerShell/PowerShellEditorServices/releases, extract it, and either set " +
	"lsp.servers.powershell.command to the bundle path or unzip it to <ICEBATH_HOME>/lsp/PowerShellEditorServices."

// PowerShellEditorServices on a pwsh host with the module bundle
func spawnPowershellES(root string, ctx ServerContext) (*SpawnSpec, error) {
	pwsh := which("pwsh", "powershell")
	if pwsh == "" {
		return nil, nil
	}
	bundle := findPSESBundle(ctx)
	if bundle == "" {
		warnOnce(ctx.Runtime, "pses-bundle", psesMissingMsg)
		return nil, nil
	}
	startScript := filepath.Join(bundle, "PowerShellEditorServices", "Start-EditorServices.ps1")
	sessionDir, err := SessionDir(ctx.Runtime)
	if err != nil {
		return nil, err
	}
	inner := "& '" + startScript + "' -BundledModulesPath '" + bundle + "' " +
		"-LogPath '" + filepath.Join(sessionDir, "pses.log") + "' " +
		"-SessionDetailsPath '" + filepath.Join(sessionDir, "pses-session-"+strconv.Itoa(os.Getpid())+".json") + "' " +
		"-FeatureFlags @() -AdditionalModules @() " +
		"-HostName Icebath -HostProfileId icebath -HostVersion 1.0.0 -Stdio -LogLevel Normal"
	env := maps.Clone(ctx.EnvOverrides["powershell"])
	if env == nil {
		env = map[string]string{}
	}
	init := map[string]any{}
	for k, v := range ctx.InitOverrides["powershell"] {
		if k != "bundlePath" {
			init[k] = v
		}
	}
	return &SpawnSpec{
		Command:       []string{pwsh, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", inner},
		WorkspaceRoot: root, Cwd: root, Env: env, InitializationOptions: init,
	}, nil
}

// SessionDir returns and creates the directory for PowerShellEditorServices session and log files
func SessionDir(rt *Runtime) (string, error) {
	d := filepath.Join(rt.home(), "lsp", "pses")
	if err := os.MkdirAll(d, 0o777); err != nil {
		return "", err
	}
	return d, nil
}

// UntrustedSafeServers are the only servers that start in a workspace the operator has not trusted
var UntrustedSafeServers = map[string]bool{
	"pyright":                true,
	"typescript":             true,
	"svelte-language-server": true,
	"bash-language-server":   true,
	"yaml-language-server":   true,
	"dockerfile-ls":          true,
	"intelephense":           true,
	"clangd":                 true,
}

var (
	jsMarkers      = []string{"package-lock.json", "bun.lockb", "bun.lock", "pnpm-lock.yaml", "yarn.lock", "package.json", "tsconfig.json"}
	denoExcludes   = []string{"deno.json", "deno.jsonc"}
	rootTypescript = markersRoot(jsMarkers, denoExcludes)
)

// Options of one registry entry
type serverOpts struct {
	markers       []string
	excludes      []string
	resolveRoot   RootFn
	buildSpawn    SpawnFn
	which         []string
	args          []string
	installPkg    string
	baseInit      map[string]any
	seed          bool
	untrustedInit map[string]any
	multiRoot     bool
}

// Registry entry defaulting to a marker based root and a single binary spawn
func server(serverID string, extensions []string, description string, o serverOpts) *ServerDef {
	resolve := o.resolveRoot
	if resolve == nil {
		resolve = markersRoot(o.markers, o.excludes)
	}
	build := o.buildSpawn
	if build == nil {
		names := o.which
		if len(names) == 0 {
			names = []string{serverID}
		}
		build = simpleSpawn(serverID, names, o.args, o.installPkg, o.baseInit, o.seed, o.untrustedInit)
	}
	return &ServerDef{ServerID: serverID, Extensions: extensions, ResolveRoot: resolve, BuildSpawn: build,
		SeedFirstPush: o.seed, Description: description, MultiRoot: o.multiRoot}
}

// Servers is the built in registry, searched in order
var Servers = []*ServerDef{
	server("pyright", []string{".py", ".pyi"}, "Python — Microsoft pyright", serverOpts{
		markers:    []string{"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "Pipfile", "pyrightconfig.json"},
		buildSpawn: spawnPyright, multiRoot: true}),
	server("typescript", []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts"},
		"JavaScript/TypeScript — typescript-language-server", serverOpts{resolveRoot: rootTypescript,
			buildSpawn: spawnTypescript, seed: true}),
	server("vue-language-server", []string{".vue"}, "Vue.js — @vue/language-server", serverOpts{resolveRoot: rootTypescript,
		buildSpawn: spawnVue}),
	server("svelte-language-server", []string{".svelte"}, "Svelte — svelte-language-server", serverOpts{resolveRoot: rootTypescript,
		which: []string{"svelteserver", "svelte-language-server"}, args: []string{"--stdio"}, installPkg: "svelte-language-server",
		untrustedInit: map[string]any{"isTrusted": false}}),
	server("astro-language-server", []string{".astro"}, "Astro — @astrojs/language-server", serverOpts{resolveRoot: rootTypescript,
		which: []string{"astro-ls", "astro-language-server"}, args: []string{"--stdio"}, installPkg: "@astrojs/language-server"}),
	server("gopls", []string{".go"}, "Go — gopls", serverOpts{markers: []string{"go.work", "go.mod", "go.sum"}, installPkg: "gopls"}),
	server("rust-analyzer", []string{".rs"}, "Rust — rust-analyzer", serverOpts{markers: []string{"Cargo.toml", "Cargo.lock"},
		installPkg: "rust-analyzer"}),
	server("clangd", []string{".c", ".cpp", ".cc", ".cxx", ".h", ".hh", ".hpp", ".hxx"}, "C/C++ — clangd", serverOpts{
		markers: []string{"compile_commands.json", "compile_flags.txt", ".clangd"},
		args:    []string{"--background-index", "--clang-tidy"}, installPkg: "clangd"}),
	server("bash-language-server", []string{".sh", ".bash", ".zsh", ".ksh"}, "Bash — bash-language-server",
		serverOpts{buildSpawn: spawnBashLS}),
	server("yaml-language-server", []string{".yaml", ".yml"}, "YAML — yaml-language-server", serverOpts{
		args: []string{"--stdio"}, installPkg: "yaml-language-server"}),
	server("lua-language-server", []string{".lua"}, "Lua — lua-language-server", serverOpts{
		markers:    []string{".luarc.json", ".luarc.jsonc", ".luacheckrc", ".stylua.toml", "stylua.toml", "selene.toml", "selene.yml"},
		installPkg: "lua-language-server"}),
	server("laravel-lsp", []string{".blade.php"}, "Laravel Blade — laravel-lsp (manual: composer global require laravel/lsp)",
		serverOpts{markers: []string{"artisan", "composer.json", "composer.lock"}, args: []string{"lsp"}}),
	server("intelephense", []string{".php"}, "PHP — intelephense", serverOpts{markers: []string{"composer.json", "composer.lock", ".php-version"},
		args: []string{"--stdio"}, installPkg: "intelephense", baseInit: map[string]any{"telemetry": map[string]any{"enabled": false}}}),
	server("ocaml-lsp", []string{".ml", ".mli"}, "OCaml — ocaml-lsp", serverOpts{markers: []string{"dune-project", "dune-workspace", ".merlin", "opam"},
		which: []string{"ocamllsp"}}),
	server("dockerfile-ls", []string{".dockerfile", "Dockerfile"}, "Dockerfile — dockerfile-language-server-nodejs", serverOpts{
		which: []string{"docker-langserver"}, args: []string{"--stdio"}, installPkg: "dockerfile-language-server-nodejs"}),
	server("terraform-ls", []string{".tf", ".tfvars"}, "Terraform — terraform-ls", serverOpts{markers: []string{".terraform.lock.hcl", "terraform.tfstate"},
		args: []string{"serve"}, baseInit: map[string]any{"experimentalFeatures": map[string]any{"prefillRequiredFields": true, "validateOnSave": true}}}),
	server("dart", []string{".dart"}, "Dart — built-in language server", serverOpts{markers: []string{"pubspec.yaml", "analysis_options.yaml"},
		args: []string{"language-server", "--lsp"}}),
	server("haskell-language-server", []string{".hs", ".lhs"}, "Haskell — haskell-language-server", serverOpts{
		markers: []string{"stack.yaml", "cabal.project", "hie.yaml"},
		which:   []string{"haskell-language-server-wrapper", "haskell-language-server"}, args: []string{"--lsp"}}),
	server("julia", []string{".jl"}, "Julia — LanguageServer.jl", serverOpts{markers: []string{"Project.toml", "Manifest.toml"},
		args: []string{"--startup-file=no", "--history-file=no", "-e", "using LanguageServer; runserver()"}}),
	server("clojure-lsp", []string{".clj", ".cljs", ".cljc", ".edn"}, "Clojure — clojure-lsp", serverOpts{
		markers: []string{"deps.edn", "project.clj", "shadow-cljs.edn", "bb.edn", "build.boot"}, args: []string{"listen"}}),
	server("nixd", []string{".nix"}, "Nix — nixd", serverOpts{resolveRoot: func(fp, ws string) string {
		if r := NearestRoot(fp, []string{"flake.nix"}, nil, ""); r != "" {
			return r
		}
		return ws
	}}),
	server("zls", []string{".zig", ".zon"}, "Zig — zls", serverOpts{markers: []string{"build.zig"}}),
	server("gleam", []string{".gleam"}, "Gleam — built-in language server", serverOpts{markers: []string{"gleam.toml"}, args: []string{"lsp"}}),
	server("elixir-ls", []string{".ex", ".exs"}, "Elixir — elixir-ls", serverOpts{markers: []string{"mix.exs", "mix.lock"},
		which: []string{"elixir-ls", "language_server.sh"}}),
	server("prisma", []string{".prisma"}, "Prisma — built-in language server", serverOpts{markers: []string{"schema.prisma", "prisma/schema.prisma"},
		args: []string{"language-server"}}),
	server("kotlin-language-server", []string{".kt", ".kts"}, "Kotlin — kotlin-language-server", serverOpts{
		markers: []string{"settings.gradle", "settings.gradle.kts", "build.gradle", "build.gradle.kts", "pom.xml"}}),
	server("jdtls", []string{".java"}, "Java — Eclipse JDT Language Server", serverOpts{
		markers: []string{"pom.xml", "build.gradle", "build.gradle.kts", ".project", ".classpath", "settings.gradle"}}),
	server("powershell", []string{".ps1", ".psm1", ".psd1"}, "PowerShell — PowerShellEditorServices (manual bundle)", serverOpts{
		markers: []string{"PSScriptAnalyzerSettings.psd1"}, buildSpawn: spawnPowershellES}),
}

// FindServerForFile returns the first entry of servers, the built in registry when nil, handling the file
func FindServerForFile(filePath string, servers []*ServerDef) *ServerDef {
	if servers == nil {
		servers = Servers
	}
	for _, srv := range servers {
		if srv.Matches(filePath) {
			return srv
		}
	}
	return nil
}

// StaticRegistryHandles reports whether a built in server lists the extension
func StaticRegistryHandles(ext string) bool {
	for _, srv := range Servers {
		if srv.Handles(ext) {
			return true
		}
	}
	return false
}

// LanguageIDFor returns the didOpen languageId, a custom server's own id winning
func LanguageIDFor(path string, srv *ServerDef) string {
	if srv != nil && srv.LanguageID != "" {
		return srv.LanguageID
	}
	if lang, ok := LanguageByExt[fileExtOrBasename(path)]; ok {
		return lang
	}
	return "plaintext"
}

// Spawn builder for a config declared server, the first command element a path or PATH lookup
func customSpawn(serverID string, command []string) SpawnFn {
	return func(root string, ctx ServerContext) (*SpawnSpec, error) {
		binPath := which(pypath.Expanduser(command[0]))
		if binPath == "" {
			warnOnce(ctx.Runtime, "custom:"+serverID, fmt.Sprintf("lsp.servers.%s: command %s not found on PATH — server skipped",
				serverID, pyrepr.StrRepr(command[0])))
			return nil, nil
		}
		return makeSpec(root, ctx, serverID, append([]string{binPath}, command[1:]...), nil, false)
	}
}

// Non empty list of non empty strings, false for any other shape
func stringList(v any) ([]string, bool) {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return nil, false
	}
	out := make([]string, len(list))
	for i, e := range list {
		s, ok := e.(string)
		if !ok || s == "" {
			return nil, false
		}
		out[i] = s
	}
	return out, true
}

// CustomServers turns config entries declaring extensions and no built in id into user declared servers
func CustomServers(rt *Runtime, serversCfg any) []*ServerDef {
	cfg, ok := serversCfg.(map[string]any)
	if !ok {
		return nil
	}
	builtin := map[string]bool{}
	for _, s := range Servers {
		builtin[s.ServerID] = true
	}
	var ids []string
	listed := map[string]bool{}
	if rt != nil && rt.ServerOrder != nil {
		for _, id := range rt.ServerOrder() {
			if _, declared := cfg[id]; declared && !listed[id] {
				listed[id] = true
				ids = append(ids, id)
			}
		}
	}
	var rest []string
	for id := range cfg {
		if !listed[id] {
			rest = append(rest, id)
		}
	}
	sort.Strings(rest)
	ids = append(ids, rest...)
	var out []*ServerDef
	for _, serverID := range ids {
		entry, ok := cfg[serverID].(map[string]any)
		if builtin[serverID] || !ok {
			continue
		}
		if _, has := entry["extensions"]; !has {
			continue
		}
		command, okCmd := stringList(entry["command"])
		exts, okExt := stringList(entry["extensions"])
		if !okCmd || !okExt {
			rt.logf(slog.LevelWarn, "agent.lsp.servers",
				"lsp.servers.%s: custom server needs command: [bin, ...args] and extensions: [.ext, ...] — ignored", serverID)
			continue
		}
		for i, e := range exts {
			if strings.HasPrefix(e, ".") {
				exts[i] = strings.ToLower(e)
			}
		}
		var markers []string
		if list, ok := entry["root_markers"].([]any); ok && len(list) > 0 {
			for _, m := range list {
				markers = append(markers, pyrepr.Str(m))
			}
		}
		description := serverID + " — custom (lsp.servers)"
		if d := entry["description"]; pyrepr.Truthy(d) {
			description = pyrepr.Str(d)
		}
		languageID := ""
		if l := entry["language_id"]; pyrepr.Truthy(l) {
			languageID = pyrepr.Str(l)
		}
		out = append(out, &ServerDef{ServerID: serverID, Extensions: exts, ResolveRoot: markersRoot(markers, nil),
			BuildSpawn: customSpawn(serverID, command), Description: description, LanguageID: languageID})
	}
	return out
}
