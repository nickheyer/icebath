package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/packages"
	lspconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/lsp/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Language id each extension or file name is opened with
var languages = func() map[string]string {
	out := map[string]string{}
	for _, l := range []struct {
		id   string
		keys []string
	}{
		{"python", []string{".py", ".pyi"}}, {"typescript", []string{".ts", ".mts", ".cts"}}, {"typescriptreact", []string{".tsx"}},
		{"javascript", []string{".js", ".mjs", ".cjs"}}, {"javascriptreact", []string{".jsx"}}, {"vue", []string{".vue"}},
		{"svelte", []string{".svelte"}}, {"astro", []string{".astro"}}, {"go", []string{".go"}}, {"rust", []string{".rs"}},
		{"ruby", []string{".rb", ".rake", ".gemspec", ".ru"}}, {"c", []string{".c", ".h"}},
		{"cpp", []string{".cc", ".cpp", ".cxx", ".hh", ".hpp", ".hxx"}}, {"csharp", []string{".cs", ".csx"}},
		{"fsharp", []string{".fs", ".fsi", ".fsx"}}, {"swift", []string{".swift"}}, {"java", []string{".java"}}, {"kotlin", []string{".kt", ".kts"}},
		{"yaml", []string{".yaml", ".yml"}}, {"json", []string{".json"}}, {"jsonc", []string{".jsonc"}}, {"lua", []string{".lua"}},
		{"php", []string{".php"}}, {"blade", []string{".blade.php"}}, {"prisma", []string{".prisma"}}, {"dart", []string{".dart"}},
		{"ocaml", []string{".ml", ".mli"}}, {"shellscript", []string{".sh", ".bash", ".zsh"}}, {"terraform", []string{".tf", ".tfvars"}},
		{"latex", []string{".tex"}}, {"bibtex", []string{".bib"}}, {"gleam", []string{".gleam"}}, {"clojure", []string{".clj", ".cljc", ".edn"}},
		{"clojurescript", []string{".cljs"}}, {"nix", []string{".nix"}}, {"typst", []string{".typ", ".typc"}}, {"haskell", []string{".hs", ".lhs"}},
		{"julia", []string{".jl"}}, {"elixir", []string{".ex", ".exs"}}, {"zig", []string{".zig", ".zon"}}, {"dockerfile", []string{".dockerfile"}},
		{"powershell", []string{".ps1", ".psm1", ".psd1"}},
	} {
		for _, key := range l.keys {
			out[key] = l.id
		}
	}
	return out
}()

// Extensions of more than one part that name another language than their last one
var compound = []string{".blade.php"}

// Lower case extension of a file, compound ones first, or its name when it has none
func extOrName(path string) string {
	base := filepath.Base(path)
	lower := strings.ToLower(base)
	for _, c := range compound {
		if strings.HasSuffix(lower, c) {
			return c
		}
	}
	if ext := filepath.Ext(strings.TrimLeft(base, ".")); ext != "" {
		return strings.ToLower(ext)
	}
	return base
}

// Root of a file for a server within its workspace, empty to leave the file out
type rootFunc func(file, workspace string) string

// What a server is started with for a root, nil when it can't be found
type spec struct {
	command []string
	env     map[string]string
	// The package manager's Node goes on the server's PATH
	node  bool
	init  map[string]any
	seeds bool
}

// One language server icebath knows, or one the settings declare
type server struct {
	id         string
	extensions []string
	root       rootFunc
	spawn      func(b *build) (*spec, error)
	seeds      bool
	about      string
	multiRoot  bool
	language   string
	custom     bool
	// Names its program goes by on PATH, and the program a declared server runs as written
	names   []string
	program string
	// How a person installs it themselves, for one icebath can't install
	install string
}

// Whether the server takes the file, by its extension or its name
func (s *server) matches(path string) bool { return slices.Contains(s.extensions, extOrName(path)) }

// Language id the file is opened with, a custom server's own winning
func (s *server) languageFor(path string) string {
	if s != nil && s.language != "" {
		return s.language
	}
	if id, ok := languages[extOrName(path)]; ok {
		return id
	}
	return "plaintext"
}

// What a spawn builder reads: the service it starts for, the root and whether it is trusted
type build struct {
	s       *service
	root    string
	trusted bool
}

// Servers that start in a workspace the user has not trusted, as they run none of its code
var untrustedSafe = map[string]bool{"pyright": true, "typescript": true, "svelte-language-server": true, "bash-language-server": true,
	"yaml-language-server": true, "dockerfile-ls": true, "intelephense": true, "clangd": true}

// Folders a walk up from a file goes through at most
const maxWalk = 64

// Folder of the start and its parents up to the top, at most maxWalk of them
func walkUp(start string) []string {
	var out []string
	for dir := start; len(out) < maxWalk; {
		out = append(out, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return out
}

func present(dir string, names []string) bool {
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// First folder up from the file holding a marker, a package folder of Python never one; empty past the ceiling or once an exclude marker
// is met
func nearestRoot(file string, markers, excludes []string, ceiling string) string {
	start, err := filepath.Abs(fsx.ExpandHome(file))
	if err != nil {
		return ""
	}
	if st, err := os.Stat(start); err == nil && st.Mode().IsRegular() {
		start = filepath.Dir(start)
	}
	if ceiling != "" {
		if abs, err := filepath.Abs(fsx.ExpandHome(ceiling)); err == nil {
			ceiling = abs
		}
	}
	for _, dir := range walkUp(start) {
		if present(dir, excludes) {
			return ""
		}
		if !present(dir, []string{"__init__.py"}) && present(dir, markers) {
			return dir
		}
		if dir == ceiling {
			return ""
		}
	}
	return ""
}

// Root under the markers with the workspace standing in, empty only when an exclude marker turned the server off
func markersRoot(markers, excludes []string) rootFunc {
	if markers == nil {
		return func(_, workspace string) string { return workspace }
	}
	return func(file, workspace string) string {
		ceiling := ""
		if workspace != "" {
			ceiling = filepath.Dir(workspace)
		}
		found := nearestRoot(file, markers, excludes, ceiling)
		if found == "" && len(excludes) > 0 && nearestRoot(file, markers, nil, ceiling) != "" {
			return ""
		}
		if found != "" {
			return found
		}
		return workspace
	}
}

// Full path of the first command of the names this process finds
func which(names ...string) string {
	for _, name := range names {
		if path, err := exec.LookPath(name); err == nil || errors.Is(err, exec.ErrDot) {
			if st, err := os.Stat(path); err == nil && !st.IsDir() {
				return path
			}
		}
	}
	return ""
}

// Binary of a server: the command the settings give when it is there, else the names on PATH, else an install of the recipe
func (b *build) binary(id string, names []string, recipe string) (string, error) {
	if command := b.s.settings.commands[id]; len(command) > 0 && command[0] != "" {
		if _, err := os.Stat(command[0]); err == nil {
			return command[0], nil
		}
	}
	if found := which(names...); found != "" || recipe == "" {
		return found, nil
	}
	return b.s.m.install(b.s.m.ctx, b.s.cfg, recipe, b.s.settings.auto, false)
}

// Spec of a command with the settings' initialization options over the base ones, their variables and the package manager's Node
func (b *build) spec(id string, command []string, base map[string]any, seeds bool) *spec {
	init := maps.Clone(base)
	if init == nil {
		init = map[string]any{}
	}
	maps.Copy(init, b.s.settings.inits[id])
	return &spec{command: command, env: maps.Clone(b.s.settings.envs[id]), node: true, init: init, seeds: seeds}
}

// Spawn of a server that is one binary with fixed arguments; off trust the untrusted options replace the base ones
func simple(id string, names, args []string, recipe string, base map[string]any, seeds bool, untrusted map[string]any) func(b *build) (*spec, error) {
	return func(b *build) (*spec, error) {
		bin, err := b.binary(id, names, recipe)
		if err != nil || bin == "" {
			return nil, err
		}
		init := base
		if !b.trusted && untrusted != nil {
			init = untrusted
		}
		return b.spec(id, append([]string{bin}, args...), init, seeds), nil
	}
}

// Pyright on the project's interpreter in a trusted workspace, on the user's otherwise
func spawnPyright(b *build) (*spec, error) {
	bin, err := b.binary("pyright", []string{"pyright-langserver", "pyright"}, "pyright")
	if err != nil || bin == "" {
		return nil, err
	}
	name := filepath.Base(bin)
	if ext := filepath.Ext(name); strings.TrimSuffix(name, ext) == "pyright" {
		if sibling := filepath.Join(filepath.Dir(bin), "pyright-langserver"+ext); fsx.Exists(sibling) {
			bin = sibling
		}
	}
	project := ""
	if b.trusted {
		project = b.root
	}
	base := map[string]any{}
	if python := b.s.m.python(project); python != "" {
		base = map[string]any{"python": map[string]any{"pythonPath": python}}
	}
	return b.spec("pyright", []string{bin, "--stdio"}, base, false), nil
}

// Interpreter pyright checks against: the user's active virtualenv, the project's own, then the package manager's Python
func (m *Manager) python(project string) string {
	var venvs []string
	if v := os.Getenv("VIRTUAL_ENV"); v != "" {
		venvs = append(venvs, v)
	}
	if project != "" {
		venvs = append(venvs, filepath.Join(project, ".venv"), filepath.Join(project, "venv"))
	}
	for _, venv := range venvs {
		for _, sub := range []string{"bin/python", "bin/python3", "Scripts/python.exe"} {
			if p := filepath.Join(venv, filepath.FromSlash(sub)); fsx.Exists(p) {
				return p
			}
		}
	}
	if installed, err := m.packages.Installed(packages.Python, false); err == nil && installed != nil {
		return installed.Binary
	}
	return ""
}

// bash-language-server, warning once that its diagnostics stay empty without shellcheck
func spawnBash(b *build) (*spec, error) {
	bin, err := b.binary("bash-language-server", []string{"bash-language-server"}, "bash-language-server")
	if err != nil || bin == "" {
		return nil, err
	}
	if which("shellcheck") == "" {
		b.s.m.warnOnce("shellcheck", "bash-language-server: shellcheck not found on PATH — diagnostics will be empty until shellcheck is installed "+
			"(apt: shellcheck, brew: shellcheck, scoop: shellcheck).")
	}
	return b.spec("bash-language-server", []string{bin, "start"}, nil, false), nil
}

// How to install the Vue server again once its install is of no use
func vueReinstall(dir string) string {
	return "delete " + filepath.Join(dir, "node_modules", "@vue") + " and " + filepath.Join(dir, "bin", "vue-language-server") +
		"*, then run: icebath lsp install vue-language-server"
}

// node_modules folders that may hold a server and its TypeScript SDK, the one the binary lives in first
func (s *service) nodeTrees(bin, root string) []string {
	trees := []string{filepath.Join(s.m.dir(s.cfg), "node_modules")}
	if root != "" {
		trees = append(trees, filepath.Join(root, "node_modules"))
	}
	real, err := filepath.EvalSymlinks(bin)
	if err != nil {
		real = bin
	}
	if abs, err := filepath.Abs(real); err == nil {
		real = abs
	}
	marker := string(filepath.Separator) + "node_modules" + string(filepath.Separator)
	if i := strings.LastIndex(real, marker); i >= 0 {
		trees = append([]string{real[:i+len(marker)-1]}, trees...)
	}
	return trees
}

// Major version of the first @vue/language-server of the trees, zero when none reads
func vueMajor(trees []string) int {
	for _, tree := range trees {
		raw, err := os.ReadFile(filepath.Join(tree, "@vue", "language-server", "package.json"))
		if err != nil {
			continue
		}
		var manifest struct {
			Version string `json:"version"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(string(raw), "\uFEFF")), &manifest) != nil {
			continue
		}
		head, _, _ := strings.Cut(manifest.Version, ".")
		if major, err := strconv.Atoi(strings.TrimSpace(head)); err == nil {
			return major
		}
	}
	return 0
}

// First lib folder of TypeScript under the trees that holds a JavaScript typescript.js
func typescriptSDK(trees []string) string {
	for _, tree := range trees {
		lib := filepath.Join(tree, "typescript", "lib")
		if fsx.IsFile(filepath.Join(lib, "typescript.js")) {
			return lib
		}
	}
	return ""
}

// @vue/language-server 2, which hosts TypeScript itself with hybrid mode off and the SDK named
func spawnVue(b *build) (*spec, error) {
	bin, err := b.binary("vue-language-server", []string{"vue-language-server"}, "@vue/language-server")
	if err != nil || bin == "" {
		return nil, err
	}
	trees := b.s.nodeTrees(bin, b.root)
	dir := b.s.m.dir(b.s.cfg)
	if vueMajor(trees) >= 3 {
		b.s.m.warnOnce("vue-tunnel", "vue-language-server: the installed @vue/language-server is 3.x, which only works behind a client-hosted tsserver "+
			"tunnel icebath does not run — no diagnostics will arrive. Reinstall the self-hosting 2.x line: "+vueReinstall(dir))
		return nil, nil
	}
	sdk := typescriptSDK(trees)
	if sdk == "" {
		b.s.m.warnOnce("vue-tsdk", "vue-language-server: no JavaScript TypeScript SDK (typescript/lib/typescript.js) next to the server or under the "+
			"project's node_modules — diagnostics are skipped. Reinstall (the recipe co-installs one): "+vueReinstall(dir))
		return nil, nil
	}
	return b.spec("vue-language-server", []string{bin, "--stdio"}, map[string]any{"typescript": map[string]any{"tsdk": sdk},
		"vue": map[string]any{"hybridMode": false}}, false), nil
}

// typescript-language-server, held to the SDK installed beside it in a workspace not trusted to load its own
func spawnTypescript(b *build) (*spec, error) {
	bin, err := b.binary("typescript", []string{"typescript-language-server"}, "typescript-language-server")
	if err != nil || bin == "" {
		return nil, err
	}
	base := map[string]any{}
	if !b.trusted {
		sdk := typescriptSDK(b.s.nodeTrees(bin, ""))
		if sdk == "" {
			b.s.m.warnOnce("ts-untrusted", "typescript-language-server: no TypeScript SDK next to the server, and this workspace is untrusted so its own "+
				"node_modules/typescript is not loaded — diagnostics are skipped. Reinstall: icebath lsp install typescript-language-server, or list the "+
				"workspace under "+toolsconfig.TrustedWorkspaces+".")
			return nil, nil
		}
		base = map[string]any{"tsserver": map[string]any{"path": sdk}}
	}
	return b.spec("typescript", []string{bin, "--stdio"}, base, true), nil
}

// PowerShellEditorServices bundle: from the command the settings give, the bundlePath option, the bundle setting, then the profile's copy
func (b *build) psesBundle() string {
	var candidates []string
	if command := b.s.settings.commands["powershell"]; len(command) > 0 {
		candidates = append(candidates, command[0])
	}
	if path, ok := b.s.settings.inits["powershell"]["bundlePath"].(string); ok && path != "" {
		candidates = append(candidates, path)
	}
	candidates = append(candidates, b.s.settings.bundle, filepath.Join(b.s.m.dir(b.s.cfg), "PowerShellEditorServices"))
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		candidate = fsx.ExpandHome(candidate)
		if fsx.IsFile(filepath.Join(candidate, "PowerShellEditorServices", "Start-EditorServices.ps1")) {
			return candidate
		}
		if fsx.IsFile(filepath.Join(candidate, "Start-EditorServices.ps1")) {
			return filepath.Dir(candidate)
		}
	}
	return ""
}

// PowerShellEditorServices on pwsh with its module bundle
func spawnPowershell(b *build) (*spec, error) {
	pwsh := which("pwsh", "powershell")
	if pwsh == "" {
		return nil, nil
	}
	bundle := b.psesBundle()
	if bundle == "" {
		b.s.m.warnOnce("pses-bundle", "powershell: pwsh found but the PowerShellEditorServices bundle is missing. Download the release zip from "+
			"https://github.com/PowerShell/PowerShellEditorServices/releases, extract it, and either set "+lspconfig.Servers+".powershell.command to the "+
			"bundle path, set "+lspconfig.PowerShellBundle+", or unzip it to "+filepath.Join(b.s.m.dir(b.s.cfg), "PowerShellEditorServices")+".")
		return nil, nil
	}
	sessions := filepath.Join(b.s.m.dir(b.s.cfg), "pses")
	if err := os.MkdirAll(sessions, 0o777); err != nil {
		return nil, err
	}
	start := filepath.Join(bundle, "PowerShellEditorServices", "Start-EditorServices.ps1")
	inner := "& '" + start + "' -BundledModulesPath '" + bundle + "' -LogPath '" + filepath.Join(sessions, "pses.log") + "' -SessionDetailsPath '" +
		filepath.Join(sessions, fmt.Sprintf("pses-session-%d.json", os.Getpid())) + "' -FeatureFlags @() -AdditionalModules @() -HostName Icebath " +
		"-HostProfileId icebath -HostVersion 1.0.0 -Stdio -LogLevel Normal"
	init := map[string]any{}
	for k, v := range b.s.settings.inits["powershell"] {
		if k != "bundlePath" {
			init[k] = v
		}
	}
	return &spec{command: []string{pwsh, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", inner},
		env: maps.Clone(b.s.settings.envs["powershell"]), init: init}, nil
}

// Options of one entry of the table
type entryOptions struct {
	markers   []string
	excludes  []string
	root      rootFunc
	spawn     func(b *build) (*spec, error)
	names     []string
	args      []string
	recipe    string
	init      map[string]any
	seeds     bool
	untrusted map[string]any
	multiRoot bool
	install   string
}

// Entry of the table, rooted by markers and spawned as one binary unless it says otherwise
func entry(id string, extensions []string, about string, o entryOptions) *server {
	root := o.root
	if root == nil {
		root = markersRoot(o.markers, o.excludes)
	}
	names := o.names
	if len(names) == 0 {
		names = []string{id}
	}
	spawn := o.spawn
	if spawn == nil {
		spawn = simple(id, names, o.args, o.recipe, o.init, o.seeds, o.untrusted)
	}
	return &server{id: id, extensions: extensions, root: root, spawn: spawn, seeds: o.seeds, about: about, multiRoot: o.multiRoot, names: names,
		install: o.install}
}

var (
	jsMarkers      = []string{"package-lock.json", "bun.lockb", "bun.lock", "pnpm-lock.yaml", "yarn.lock", "package.json", "tsconfig.json"}
	rootTypescript = markersRoot(jsMarkers, []string{"deno.json", "deno.jsonc"})
)

// Servers icebath knows, looked through in order
var builtins = []*server{
	entry("pyright", []string{".py", ".pyi"}, "Python — Microsoft pyright", entryOptions{
		markers: []string{"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "Pipfile", "pyrightconfig.json"}, spawn: spawnPyright, multiRoot: true,
		names: []string{"pyright-langserver", "pyright"}}),
	entry("typescript", []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts"}, "JavaScript/TypeScript — typescript-language-server",
		entryOptions{root: rootTypescript, spawn: spawnTypescript, seeds: true, names: []string{"typescript-language-server"}}),
	entry("vue-language-server", []string{".vue"}, "Vue.js — @vue/language-server", entryOptions{root: rootTypescript, spawn: spawnVue,
		names: []string{"vue-language-server"}}),
	entry("svelte-language-server", []string{".svelte"}, "Svelte — svelte-language-server", entryOptions{root: rootTypescript,
		names: []string{"svelteserver", "svelte-language-server"}, args: []string{"--stdio"}, recipe: "svelte-language-server",
		untrusted: map[string]any{"isTrusted": false}}),
	entry("astro-language-server", []string{".astro"}, "Astro — @astrojs/language-server", entryOptions{root: rootTypescript,
		names: []string{"astro-ls", "astro-language-server"}, args: []string{"--stdio"}, recipe: "@astrojs/language-server"}),
	entry("gopls", []string{".go"}, "Go — gopls", entryOptions{markers: []string{"go.work", "go.mod", "go.sum"}, recipe: "gopls"}),
	entry("rust-analyzer", []string{".rs"}, "Rust — rust-analyzer", entryOptions{markers: []string{"Cargo.toml", "Cargo.lock"}, recipe: "rust-analyzer",
		install: "Install rust-analyzer with rustup component add rust-analyzer."}),
	entry("clangd", []string{".c", ".cpp", ".cc", ".cxx", ".h", ".hh", ".hpp", ".hxx"}, "C/C++ — clangd", entryOptions{
		markers: []string{"compile_commands.json", "compile_flags.txt", ".clangd"}, args: []string{"--background-index", "--clang-tidy"}, recipe: "clangd",
		install: "Install clangd from the packages of your system, such as apt install clangd or brew install llvm."}),
	entry("bash-language-server", []string{".sh", ".bash", ".zsh", ".ksh"}, "Bash — bash-language-server", entryOptions{spawn: spawnBash, names: []string{"bash-language-server"}}),
	entry("yaml-language-server", []string{".yaml", ".yml"}, "YAML — yaml-language-server", entryOptions{args: []string{"--stdio"},
		recipe: "yaml-language-server"}),
	entry("lua-language-server", []string{".lua"}, "Lua — lua-language-server", entryOptions{
		markers: []string{".luarc.json", ".luarc.jsonc", ".luacheckrc", ".stylua.toml", "stylua.toml", "selene.toml", "selene.yml"},
		recipe:  "lua-language-server", install: "Install lua-language-server from the packages of your system, such as brew install lua-language-server."}),
	entry("laravel-lsp", []string{".blade.php"}, "Laravel Blade — laravel-lsp, installed with composer global require laravel/lsp", entryOptions{
		markers: []string{"artisan", "composer.json", "composer.lock"}, args: []string{"lsp"}, install: "Install laravel-lsp with composer global require laravel/lsp."}),
	entry("intelephense", []string{".php"}, "PHP — intelephense", entryOptions{markers: []string{"composer.json", "composer.lock", ".php-version"},
		args: []string{"--stdio"}, recipe: "intelephense", init: map[string]any{"telemetry": map[string]any{"enabled": false}}}),
	entry("ocaml-lsp", []string{".ml", ".mli"}, "OCaml — ocaml-lsp", entryOptions{markers: []string{"dune-project", "dune-workspace", ".merlin", "opam"},
		names: []string{"ocamllsp"}, install: "Install ocaml-lsp with opam install ocaml-lsp-server."}),
	entry("dockerfile-ls", []string{".dockerfile", "Dockerfile"}, "Dockerfile — dockerfile-language-server-nodejs", entryOptions{
		names: []string{"docker-langserver"}, args: []string{"--stdio"}, recipe: "dockerfile-language-server-nodejs"}),
	entry("terraform-ls", []string{".tf", ".tfvars"}, "Terraform — terraform-ls", entryOptions{markers: []string{".terraform.lock.hcl", "terraform.tfstate"},
		args: []string{"serve"}, init: map[string]any{"experimentalFeatures": map[string]any{"prefillRequiredFields": true, "validateOnSave": true}},
		install: "Install terraform-ls from HashiCorp, such as brew install hashicorp/tap/terraform-ls."}),
	entry("dart", []string{".dart"}, "Dart — built-in language server", entryOptions{markers: []string{"pubspec.yaml", "analysis_options.yaml"},
		args: []string{"language-server", "--lsp"}, install: "Install the Dart SDK, which brings the dart language server."}),
	entry("haskell-language-server", []string{".hs", ".lhs"}, "Haskell — haskell-language-server", entryOptions{
		markers: []string{"stack.yaml", "cabal.project", "hie.yaml"}, names: []string{"haskell-language-server-wrapper", "haskell-language-server"},
		args: []string{"--lsp"}, install: "Install haskell-language-server with ghcup install hls."}),
	entry("julia", []string{".jl"}, "Julia — LanguageServer.jl", entryOptions{markers: []string{"Project.toml", "Manifest.toml"},
		args:    []string{"--startup-file=no", "--history-file=no", "-e", "using LanguageServer; runserver()"},
		install: "Install Julia, then add the Julia package LanguageServer."}),
	entry("clojure-lsp", []string{".clj", ".cljs", ".cljc", ".edn"}, "Clojure — clojure-lsp", entryOptions{
		markers: []string{"deps.edn", "project.clj", "shadow-cljs.edn", "bb.edn", "build.boot"}, args: []string{"listen"},
		install: "Install clojure-lsp with brew install clojure-lsp/brew/clojure-lsp."}),
	entry("nixd", []string{".nix"}, "Nix — nixd", entryOptions{root: func(file, workspace string) string {
		if root := nearestRoot(file, []string{"flake.nix"}, nil, ""); root != "" {
			return root
		}
		return workspace
	}, install: "Install nixd with nix profile install nixpkgs#nixd."}),
	entry("zls", []string{".zig", ".zon"}, "Zig — zls", entryOptions{markers: []string{"build.zig"}, install: "Install zls from the packages of your system, such as brew install zls."}),
	entry("gleam", []string{".gleam"}, "Gleam — built-in language server", entryOptions{markers: []string{"gleam.toml"}, args: []string{"lsp"}, install: "Install Gleam, which brings the gleam language server."}),
	entry("elixir-ls", []string{".ex", ".exs"}, "Elixir — elixir-ls", entryOptions{markers: []string{"mix.exs", "mix.lock"},
		names: []string{"elixir-ls", "language_server.sh"}, install: "Install elixir-ls with brew install elixir-ls, or from the elixir-ls releases on GitHub."}),
	entry("prisma", []string{".prisma"}, "Prisma — built-in language server", entryOptions{markers: []string{"schema.prisma", "prisma/schema.prisma"},
		args: []string{"language-server"}, install: "Install the Prisma command line, which brings the prisma language server, with npm install -g prisma."}),
	entry("kotlin-language-server", []string{".kt", ".kts"}, "Kotlin — kotlin-language-server", entryOptions{
		markers: []string{"settings.gradle", "settings.gradle.kts", "build.gradle", "build.gradle.kts", "pom.xml"},
		install: "Install kotlin-language-server with brew install kotlin-language-server."}),
	entry("jdtls", []string{".java"}, "Java — Eclipse JDT Language Server", entryOptions{
		markers: []string{"pom.xml", "build.gradle", "build.gradle.kts", ".project", ".classpath", "settings.gradle"}, install: "Install jdtls with brew install jdtls."}),
	entry("powershell", []string{".ps1", ".psm1", ".psd1"}, "PowerShell — PowerShellEditorServices (manual bundle)", entryOptions{
		markers: []string{"PSScriptAnalyzerSettings.psd1"}, spawn: spawnPowershell, names: []string{"pwsh", "powershell"},
		install: "Install PowerShell, then unzip PowerShellEditorServices from GitHub into the lsp folder of the profile."}),
}

// First of the servers that takes the file
func serverOf(path string, servers []*server) *server {
	for _, s := range servers {
		if s.matches(path) {
			return s
		}
	}
	return nil
}

// Whether a built in server takes the extension
func builtinHandles(extension string) bool {
	for _, s := range builtins {
		if slices.Contains(s.extensions, extension) {
			return true
		}
	}
	return false
}

func builtin(id string) bool {
	return slices.ContainsFunc(builtins, func(s *server) bool { return s.id == id })
}

// Spawn of a server the settings declare, its program a path or a name on PATH; one with no command yet starts nothing
func spawnCustom(id string, command []string) func(b *build) (*spec, error) {
	return func(b *build) (*spec, error) {
		if len(command) == 0 {
			b.s.m.warnOnce("custom:"+id, fmt.Sprintf("%s.%s: the server has no command — server skipped", lspconfig.Servers, id))
			return nil, nil
		}
		bin := which(fsx.ExpandHome(command[0]))
		if bin == "" {
			b.s.m.warnOnce("custom:"+id, fmt.Sprintf("%s.%s: command %q not found on PATH — server skipped", lspconfig.Servers, id, command[0]))
			return nil, nil
		}
		return b.spec(id, append([]string{bin}, command[1:]...), nil, false), nil
	}
}

// Non empty list of non empty texts, false for any other shape
func texts(v any) ([]string, bool) {
	var list []any
	switch held := v.(type) {
	case []any:
		list = held
	case []string:
		list = anyList(held)
	}
	if len(list) == 0 {
		return nil, false
	}
	out := make([]string, len(list))
	for i, item := range list {
		s, ok := item.(string)
		if !ok || s == "" {
			return nil, false
		}
		out[i] = s
	}
	return out, true
}

// File types an entry a person wrote by hand names, read as kindly as they can be: a list of them, or one line of them
func endings(v any) []string {
	switch held := v.(type) {
	case string:
		return fileTypes(held)
	case []string:
		return fileTypes(strings.Join(held, " "))
	case []any:
		var words []string
		for _, item := range held {
			if item != nil {
				words = append(words, fmt.Sprint(item))
			}
		}
		return fileTypes(strings.Join(words, " "))
	}
	return nil
}

// Servers the settings declare: entries with extensions under a name no built in server has, in the order they are written. One whose
// entry lacks a command or file types, which only a file written by hand holds, is a server all the same, so its page can give it what
// it lacks: it starts nothing until then
func (m *Manager) customs(entries []serverEntry) []*server {
	var out []*server
	for _, e := range entries {
		if builtin(e.id) {
			continue
		}
		if _, has := e.raw[lspconfig.Extensions]; !has {
			continue
		}
		command := commandOf(e.raw[lspconfig.Command])
		extensions := endings(e.raw[lspconfig.Extensions])
		if len(command) == 0 || len(extensions) == 0 {
			m.logf(slog.LevelWarn, "%s.%s: custom server needs a command and its file types — it checks nothing until it has both", lspconfig.Servers, e.id)
		}
		program := ""
		if len(command) > 0 {
			program = command[0]
		}
		var markers []string
		if list, ok := e.raw[lspconfig.RootMarkers].([]any); ok {
			for _, item := range list {
				if marker := fmt.Sprint(item); item != nil && marker != "" {
					markers = append(markers, marker)
				}
			}
		}
		about := "A language server you added"
		if len(extensions) == 0 {
			about = "A language server you added, which checks no file until it has file types"
		}
		if text, ok := e.raw[lspconfig.Description].(string); ok && strings.TrimSpace(text) != "" {
			about = strings.TrimSpace(text)
		}
		language, _ := e.raw[lspconfig.LanguageID].(string)
		out = append(out, &server{id: e.id, extensions: extensions, root: markersRoot(markers, nil), spawn: spawnCustom(e.id, command), about: about,
			language: language, custom: true, names: []string{program}, program: program})
	}
	return out
}
