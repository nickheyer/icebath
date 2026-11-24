package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

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
	return strings.TrimPrefix(pystr.TranslateNewlines(strings.ToValidUTF8(string(raw), "\uFFFD")), "\xef\xbb\xbf"), nil
}

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

// Provider directory of a name, bundled, user, then project, empty when none
func FindProviderDir(ctx context.Context, env *tools.Env, name string) string {
	r := FindProvider(ctx, env, name)
	if r == nil {
		return ""
	}
	return r.Dir
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
		New: func(ctx context.Context, env *tools.Env, opts LoadOptions) (Provider, error) {
			return loadProviderFromDir(ctx, dir, opts)
		}}
}

// Providers the directory scan finds that no explicit registration already stands for
func scannedRegistrations(ctx context.Context, env *tools.Env, explicit []*ProviderRegistration) []*ProviderRegistration {
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
			if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") || seen[name] || !plugins.GuestProvides(path, plugins.CategoryMemoryProvider) {
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
	return out
}

// Loads the memory provider a wasm or program plugin directory registers, its hooks and skills collected by the loader
func loadProviderFromDir(ctx context.Context, dir string, opts LoadOptions) (Provider, error) {
	obj, err := plugins.CollectMemoryProviderInHost(ctx, filepath.Base(dir), dir, opts.RegisterSkills, opts.TrackSkill)
	if err != nil || obj == nil {
		return nil, err
	}
	return newHostedProvider(ctx, obj)
}
