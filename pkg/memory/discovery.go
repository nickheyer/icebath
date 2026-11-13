package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
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

// Whether a plugin directory holds a runtime plugin whose manifest provides a memory provider
func DefinesMemoryProvider(path string) bool {
	mf := plugins.GuestManifest(path)
	return mf != nil && mf.Supplies("memory")
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

// Provider directory of a name, bundled, user then project, empty when none
func FindProviderDir(ctx context.Context, env *tools.Env, name string) string {
	r := FindProvider(ctx, env, name)
	if r == nil {
		return ""
	}
	return r.Dir
}

// Raw plugin.yaml text of a provider directory and the manifest name the disabled list may use
func providerManifest(dir string) (string, []string) {
	raw, _ := os.ReadFile(filepath.Join(dir, "plugin.yaml"))
	text := strings.TrimPrefix(strings.ToValidUTF8(string(raw), "\uFFFD"), "\xef\xbb\xbf")
	var spellings []string
	if file, err := pm.NativeManifestFile(dir); err == nil && file != "" {
		if manifest, err := pm.ReadNativeManifest(file); err == nil && pyrepr.Truthy(manifest["name"]) {
			spellings = append(spellings, pyrepr.Str(manifest["name"]))
		}
	}
	return text, spellings
}

// Registration of a guest provider directory found by the scan
func dirRegistration(name, dir, source, home string) *ProviderRegistration {
	manifest, spellings := providerManifest(dir)
	return &ProviderRegistration{Name: name, Source: source, Dir: dir, Home: home, Manifest: manifest, Spellings: spellings,
		ConfigSchema: guestConfigSchema(dir),
		New: func(ctx context.Context, env *tools.Env, opts LoadOptions) (Provider, error) {
			return loadProviderFromDir(ctx, dir, opts)
		}}
}

// Guest providers the directory scan finds that no explicit registration already stands for
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
		for _, path := range plugins.GuestProviderDirs(src.dir, "memory") {
			name := filepath.Base(path)
			if seen[name] {
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

// Loads the memory provider a guest directory registers, its other registrations collected for the provider
func loadProviderFromDir(ctx context.Context, dir string, opts LoadOptions) (Provider, error) {
	obj, err := plugins.LoadGuestMemoryProvider(ctx, filepath.Base(dir), dir, opts.RegisterSkills, opts.TrackSkill)
	if err != nil || obj == nil {
		return nil, err
	}
	return newHostedProvider(ctx, obj)
}

// Settings schema a guest provider declared with pluginsdk.DeclareConfigSchema, nil when it declared none
func guestConfigSchema(dir string) func(ctx context.Context) (*ProviderConfigSchema, error) {
	return func(ctx context.Context) (*ProviderConfigSchema, error) {
		record, err := plugins.GuestConfigSchema(ctx, dir)
		if err != nil || len(record) == 0 {
			return nil, err
		}
		return ProviderConfigSchemaFromRecord(record)
	}
}

// Starts the configured or named provider ahead of construction, false when it is unknown or its guest fails to start
func ImportMemoryProviderModule(ctx context.Context, env *tools.Env, name string) bool {
	if name == "" {
		name = ActiveMemoryProvider(ctx, env)
	}
	if name == "" {
		return false
	}
	r := FindProvider(ctx, env, name)
	if r == nil {
		return false
	}
	if r.Source == SourceBundled {
		return true
	}
	if err := plugins.StartGuestPlugin(ctx, r.Dir); err != nil {
		envLog(env).Debug(fmt.Sprintf("memory provider '%s' guest did not start: %v", name, err))
		return false
	}
	return true
}
