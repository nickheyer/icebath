package memory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/pyast"
	"github.com/nickheyer/icebath/pkg/internal/pyimport"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Entry point group pip installed memory providers declare
const MemoryProviderEntryPointsGroup = "icebath_agent.memory_providers"

// Modules a successful warm up import also loads in the plugin host
var nativeWarmImports = []string{"numpy"}

// Directory of the profile plugin tree, empty when it is not a directory
func userPluginsDir(ctx context.Context, env *tools.Env) string {
	d := filepath.Join(registryHome(ctx, env), "plugins")
	if st, err := os.Stat(d); err == nil && st.IsDir() {
		return d
	}
	return ""
}

// Working tree plugins directory, only when ICEBATH_ENABLE_PROJECT_PLUGINS is truthy and the directory exists
func projectPluginsDir() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ICEBATH_ENABLE_PROJECT_PLUGINS"))) {
	case "1", "true", "yes", "on":
	default:
		return ""
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	d := filepath.Join(cwd, ".icebath", "plugins")
	if st, err := os.Stat(d); err == nil && st.IsDir() {
		return d
	}
	return ""
}

// Package text decoded like read_text with utf-8-sig and errors replace
func readPackageText(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(pystr.TranslateNewlines(pystr.DecodeUTF8Replace(raw)), "\xef\xbb\xbf"), nil
}

// Whether the package init mentions the memory provider contract in its first 8192 characters or defines it further down
func isMemoryProviderDir(log *slog.Logger, path string) bool {
	initFile := filepath.Join(path, "__init__.py")
	if _, err := os.Stat(initFile); err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return false
		}
		log.Warn(fmt.Sprintf("Skipping unreadable plugin directory %s: %s", path, err))
		return false
	}
	source, err := readPackageText(initFile)
	if err != nil {
		log.Warn(fmt.Sprintf("Skipping unreadable plugin directory %s: %s", path, err))
		return false
	}
	if runes := []rune(source); len(runes) > 8192 {
		source = string(runes[:8192])
	}
	return strings.Contains(source, "register_memory_provider") || strings.Contains(source, "MemoryProvider") || definesMemoryProvider(path)
}

// Parses the top level modules for a register_memory_provider call or a MemoryProvider subclass, aliases included
func definesMemoryProvider(path string) bool {
	modules, _ := filepath.Glob(filepath.Join(path, "*.py"))
	sort.Strings(modules)
	for _, module := range modules {
		text, err := readPackageText(module)
		if err != nil {
			continue
		}
		tree, err := pyast.Parse(text)
		if err != nil {
			continue
		}
		nodes := pyast.Walk(tree)
		bases := map[string]bool{"MemoryProvider": true}
		for _, n := range nodes {
			if !n.Is("ImportFrom") {
				continue
			}
			for _, alias := range n.Children("names") {
				if name, _ := alias.Str("name"); name == "MemoryProvider" {
					if as, ok := alias.Str("asname"); ok && as != "" {
						bases[as] = true
					}
				}
			}
		}
		for _, n := range nodes {
			if n.Is("Call") {
				if f := n.Child("func"); f != nil {
					attr, _ := f.Str("attr")
					id, _ := f.Str("id")
					if attr == "register_memory_provider" || id == "register_memory_provider" {
						return true
					}
				}
			}
			if n.Is("ClassDef") {
				for _, b := range n.Children("bases") {
					name, _ := b.Str("attr")
					if name == "" {
						name, _ = b.Str("id")
					}
					if name != "" && bases[name] {
						return true
					}
				}
			}
		}
	}
	return false
}

// Whether a plugin directory defines a memory provider, the exported form plugins reach through Services
func DefinesMemoryProvider(path string) bool { return definesMemoryProvider(path) }

// One external provider root with the discovery source it stands for
type providerSourceDir struct{ dir, source string }

// User then project provider roots that exist, in precedence order
func externalSourceDirs(ctx context.Context, env *tools.Env) []providerSourceDir {
	var out []providerSourceDir
	if d := userPluginsDir(ctx, env); d != "" {
		out = append(out, providerSourceDir{d, SourceUser})
	}
	if d := projectPluginsDir(); d != "" {
		out = append(out, providerSourceDir{d, SourceProject})
	}
	return out
}

// Import path entry points are enumerated over, the one the plugin host process imports from
func entryPointImportPath() []string { return filepath.SplitList(os.Getenv("PYTHONPATH")) }

// Installed memory provider entry points, read from distribution metadata without importing anything
func iterEntryPoints() []pyimport.EntryPoint {
	return pyimport.EntryPoints(entryPointImportPath(), MemoryProviderEntryPointsGroup)
}

// Entry point a provider name resolves to, nil when none is installed
func FindProviderEntryPoint(name string) *pyimport.EntryPoint {
	for _, ep := range iterEntryPoints() {
		if ep.Name == name {
			return &ep
		}
	}
	return nil
}

// Package directory of an entry point module resolved without importing it, empty for a bare module
func entryPointPackageDir(ep *pyimport.EntryPoint) string {
	if ep == nil {
		return ""
	}
	module, _, _ := strings.Cut(ep.Value, ":")
	origin := pyimport.FindOrigin(entryPointImportPath(), strings.TrimSpace(module))
	if origin == "" || filepath.Base(origin) != "__init__.py" {
		return ""
	}
	return filepath.Dir(origin)
}

// Provider directory of a name, bundled, user, project, then an entry point package, empty when none
func FindProviderDir(ctx context.Context, env *tools.Env, name string) string {
	r := FindProvider(ctx, env, name)
	if r == nil || !hasProviderDir(r) {
		return ""
	}
	return r.Dir
}

// Module name a provider package loads under, the synthetic user namespace keyed by the resolved directory
func providerDirModuleName(dir, name string) string {
	return providerModuleName(&ProviderRegistration{Name: name, Source: SourceUser, Dir: dir})
}

// Raw plugin.yaml text of a provider directory and the manifest name the disabled list may use
func providerManifest(dir string) (string, []string) {
	text, _ := readPackageText(filepath.Join(dir, "plugin.yaml"))
	var spellings []string
	if file, err := pm.NativeManifestFile(dir); err == nil && file != "" {
		if manifest, err := pm.ReadNativeManifest(file); err == nil && pyrepr.Truthy(manifest["name"]) {
			spellings = append(spellings, pyrepr.Str(manifest["name"]))
		}
	}
	return text, spellings
}

// Registration of a provider directory found by the scan, loaded in the plugin host
func dirRegistration(name, dir, source, home string) *ProviderRegistration {
	manifest, spellings := providerManifest(dir)
	return &ProviderRegistration{Name: name, Source: source, Dir: dir, Home: home, Manifest: manifest, Spellings: spellings,
		SchemaFile: filepath.Join(dir, "config_schema.py"), ConfigSchema: hostedConfigSchema(filepath.Join(dir, "config_schema.py")),
		New: func(ctx context.Context, env *tools.Env, opts LoadOptions) (Provider, error) {
			return loadProviderFromDir(ctx, dir, opts)
		}}
}

// Registration of an installed entry point, its package directory loading like a user directory
func entryPointRegistration(ep pyimport.EntryPoint) *ProviderRegistration {
	dir := entryPointPackageDir(&ep)
	name := ep.Name
	r := &ProviderRegistration{Name: name, Source: SourceEntryPoint, Dir: dir}
	if dir != "" {
		r.Manifest, r.Spellings = providerManifest(dir)
		r.SchemaFile = filepath.Join(dir, "config_schema.py")
		r.ConfigSchema = hostedConfigSchema(r.SchemaFile)
	}
	r.New = func(ctx context.Context, env *tools.Env, opts LoadOptions) (Provider, error) {
		if dir != "" {
			return loadProviderFromDir(ctx, dir, opts)
		}
		if refusal := plugins.InProcessImportRefusal(ctx, fmt.Sprintf("pip-installed memory provider %s", pyRepr(name)), SourceEntryPoint, nil); refusal != "" {
			envLog(env).Warn(refusal)
			return nil, nil
		}
		return loadProviderFromEntryPoint(ctx, name, opts)
	}
	return r
}

// Providers the directory and entry point scan finds that no explicit registration already stands for
func scannedRegistrations(ctx context.Context, env *tools.Env, explicit []*ProviderRegistration) []*ProviderRegistration {
	log := envLog(env)
	h := registryHome(ctx, env)
	seen := map[string]bool{}
	taken := map[string]bool{}
	for _, r := range explicit {
		taken[r.Source+"\x00"+r.Name] = true
		if r.Source == SourceBundled {
			seen[r.Name] = true
		}
	}
	var out []*ProviderRegistration
	for _, src := range externalSourceDirs(ctx, env) {
		children, err := os.ReadDir(src.dir)
		if err != nil {
			continue
		}
		for _, child := range children {
			name := child.Name()
			path := filepath.Join(src.dir, name)
			if st, err := os.Stat(path); err != nil || !st.IsDir() {
				continue
			}
			if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") || seen[name] || !isMemoryProviderDir(log, path) {
				continue
			}
			seen[name] = true
			if taken[src.source+"\x00"+name] {
				continue
			}
			home := ""
			if src.source == SourceUser {
				home = h
			}
			out = append(out, dirRegistration(name, path, src.source, home))
		}
	}
	for _, ep := range iterEntryPoints() {
		if taken[SourceEntryPoint+"\x00"+ep.Name] {
			continue
		}
		taken[SourceEntryPoint+"\x00"+ep.Name] = true
		out = append(out, entryPointRegistration(ep))
	}
	return out
}

// Loads a directory provider in the plugin host, the register(ctx) capture first and a top level subclass after
func loadProviderFromDir(ctx context.Context, dir string, opts LoadOptions) (Provider, error) {
	name := filepath.Base(dir)
	obj, err := plugins.CollectMemoryProviderInHost(ctx, name, dir, providerDirModuleName(dir, name), opts.RegisterSkills, opts.TrackSkill)
	if err != nil || obj == nil {
		return nil, err
	}
	return newHostedProvider(ctx, obj)
}

// Loads a bare module entry point in the plugin host as an instance, subclass, register module, factory or namespace
func loadProviderFromEntryPoint(ctx context.Context, name string, opts LoadOptions) (Provider, error) {
	obj, err := plugins.CollectMemoryProviderEntryPoint(ctx, name, opts.RegisterSkills, opts.TrackSkill)
	if err != nil || obj == nil {
		return nil, err
	}
	return newHostedProvider(ctx, obj)
}

// Imports the configured or named provider ahead of construction, false when unknown, failing or left to the host
func ImportMemoryProviderModule(ctx context.Context, env *tools.Env, name string) bool {
	if name == "" {
		name = ActiveMemoryProvider(ctx, env)
	}
	if name == "" {
		return false
	}
	r := FindProvider(ctx, env, name)
	if plugins.IsolationMode(ctx, nil) == plugins.IsolationHost && (r == nil || r.Source != SourceBundled) {
		return false
	}
	if r == nil {
		return false
	}
	if r.Source == SourceBundled {
		return true
	}
	dir, module := r.Dir, ""
	if hasProviderDir(r) {
		module = providerDirModuleName(dir, name)
	}
	return plugins.WarmMemoryProviderImport(ctx, name, dir, module, nativeWarmImports)
}
