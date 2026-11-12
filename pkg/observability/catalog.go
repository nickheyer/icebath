package observability

import (
	"context"

	"errors"
	"github.com/nickheyer/icebath/pkg/home"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Custom is the public name every user named identity reads as
const Custom = "custom"

// errCatalogUnavailable is the failure of a catalog whose owning package installed no source
var errCatalogUnavailable = errors.New("catalog source not installed")

var (
	catalogMu      sync.Mutex
	catalogSources = map[string]func() ([]string, error){"display_languages": displayLanguages, "bundled_plugin_names": bundledPluginNames}
	catalogCache   = map[string]ValueSet{}
	ownerCache     = map[[2]string]ownerResult{}
	modelsDevCache = map[string]bool{}
)

// ownerResult is one cached catalog platform owner lookup
type ownerResult struct {
	name string
	ok   bool
}

// Installs the loader of one public name catalog and drops its cached names
func SetCatalogSource(name string, load func() ([]string, error)) {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	catalogSources[name] = load
	delete(catalogCache, name)
}

// Clears every cached catalog, platform owner and models.dev answer
func ResetCatalogCaches() {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	catalogCache = map[string]ValueSet{}
	ownerCache = map[[2]string]ownerResult{}
	modelsDevCache = map[string]bool{}
}

// Names of one catalog, cached after a successful load and empty when the loader fails
func catalogSafe(name string) ValueSet {
	catalogMu.Lock()
	cached, ok := catalogCache[name]
	load := catalogSources[name]
	catalogMu.Unlock()
	if ok {
		return cached
	}
	if load == nil {
		slog.Debug("Shared-metrics catalog "+name+" unavailable", "error", errCatalogUnavailable)
		return ValueSet{}
	}
	names, err := load()
	if err != nil {
		slog.Debug("Shared-metrics catalog "+name+" unavailable", "error", err)
		return ValueSet{}
	}
	out := Set(names...)
	catalogMu.Lock()
	catalogCache[name] = out
	catalogMu.Unlock()
	return out
}

// Lower case stems of the shipped locale catalogs
func displayLanguages() ([]string, error) {
	locales := i18n.LocalesDir()
	matches, err := fs.Glob(locales.FS, "*.yaml")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, pystr.Lower(strings.TrimSuffix(path.Base(m), ".yaml")))
	}
	return out, nil
}

// Stripped lower case text of a string, empty for anything else
func catalogNorm(value any) string {
	s, ok := value.(string)
	if !ok {
		return ""
	}
	return pystr.Lower(pystr.Strip(s))
}

// Public name of a messaging platform, core and bundled names kept, proven catalog installs by entry name, else plugin
func PlatformMetricName(ctx context.Context, raw any, core ValueSet) string {
	name := catalogNorm(enumText(raw))
	if core.Has(name) || catalogSafe("bundled_platform_names").Has(name) {
		return name
	}
	if name == "" {
		return "plugin"
	}
	if owner, ok := catalogPlatformOwner(home.Dir(ctx), name); ok {
		return owner
	}
	return "plugin"
}

// Catalog entry that installed the plugin registering a platform in this profile, cached per home and platform
func catalogPlatformOwner(homeDir, platform string) (string, bool) {
	key := [2]string{homeDir, platform}
	catalogMu.Lock()
	cached, hit := ownerCache[key]
	catalogMu.Unlock()
	if hit {
		return cached.name, cached.ok
	}
	result := ownerResult{}
	if row, ok := CatalogPlatformOwnerHook(homeDir, platform); ok {
		name := catalogNorm(row)
		if catalogSafe("catalog_platform_names").Has(name) {
			result = ownerResult{name: name, ok: true}
		}
	}
	catalogMu.Lock()
	if len(ownerCache) >= 64 {
		ownerCache = map[[2]string]ownerResult{}
	}
	ownerCache[key] = result
	catalogMu.Unlock()
	return result.name, result.ok
}

// Canonical registry name of a command or alias, skill and plugin commands staying anonymous
func SlashCommandMetricName(ctx context.Context, raw any) string {
	name := strings.SplitN(strings.TrimLeft(catalogNorm(raw), "/"), " ", 2)[0]
	if name == "" {
		return "unknown"
	}
	if hook := ResolveCommandHook; hook != nil {
		if canonical, ok := hook(name); ok && catalogSafe("slash_command_names").Has(canonical) {
			return canonical
		}
	}
	if skill, err := ResolveSkillCommandKeyHook(ctx, name); err != nil {
		return "unknown"
	} else if skill {
		return "skill"
	}
	if PluginCommandHandlerHook(ctx, name) {
		return "plugin"
	}
	return "unknown"
}

// A bundled or optional skill's public name, custom otherwise
func SkillMetricName(raw any) string {
	name := catalogNorm(raw)
	if catalogSafe("bundled_skill_names").Has(name) {
		return name
	}
	return Custom
}

var extensionCatalogs = map[string]string{
	"skill": "bundled_skill_names", "mcp_server": "mcp_catalog_names", "plugin": "plugin_catalog_names",
}

// A catalog entry's public name, custom for URL and local installs of anything
func ExtensionMetricName(kind string, raw any) string {
	loader, ok := extensionCatalogs[kind]
	name := catalogNorm(raw)
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if ok && catalogSafe(loader).Has(name) {
		return name
	}
	return Custom
}

// A built in auxiliary task's name, none when unset and other when unknown
func AuxTaskMetricName(raw any) string {
	name := catalogNorm(raw)
	if name == "" {
		return "none"
	}
	if catalogSafe("aux_task_names").Has(name) {
		return name
	}
	return "other"
}

// A shipped provider id, user named providers and local server aliases of custom reading custom
func ProviderMetricName(raw any) string {
	name := metricIdentifier(raw, ProviderIdentifierMaxLength)
	if name == "unknown" {
		return name
	}
	if strings.HasPrefix(name, Custom) || catalogSafe("custom_provider_aliases").Has(name) {
		return Custom
	}
	if catalogSafe("provider_names").Has(name) || modelsDevProvider(name) {
		return name
	}
	return Custom
}

// A public models.dev provider id from the local cache only, cached per name
func modelsDevProvider(name string) bool {
	catalogMu.Lock()
	cached, hit := modelsDevCache[name]
	catalogMu.Unlock()
	if hit {
		return cached
	}
	known := false
	if hook := ModelsDevProviderHook; hook != nil {
		known = hook(name)
	}
	catalogMu.Lock()
	if len(modelsDevCache) >= 256 {
		modelsDevCache = map[string]bool{}
	}
	modelsDevCache[name] = known
	catalogMu.Unlock()
	return known
}

// Model id of a shipped remote provider, custom for user named, unknown provider, location shaped or private Azure ids
func ModelMetricName(raw any, provider string, maxLength int) string {
	if catalogSafe("user_named_model_providers").Has(provider) {
		return Custom
	}
	model := metricIdentifier(raw, maxLength)
	if model == "unknown" {
		return model
	}
	if provider == "unknown" || locationModel.MatchString(model) {
		return Custom
	}
	if strings.HasPrefix(provider, "azure") && !catalogSafe("public_model_ids").Has(model) {
		return Custom
	}
	return model
}

// A URL or path, weight file, loopback or IPv4 host, host with a port or AWS ARN, never a public model id
var locationModel = regexp.MustCompile(`^(?:arn:|.*:/|.*\.(?:gguf|bin|safetensors)$|(?:localhost|\d{1,3}(?:\.\d{1,3}){3})(?:[:/]|$)|[^/:]+:\d{2,5}(?:/|$))`)

// A shipped locale's name, en when unset and other when unknown
func DisplayLanguageMetricName(raw any) string {
	name := catalogNorm(raw)
	if name == "" {
		name = "en"
	}
	if catalogSafe("display_languages").Has(name) {
		return name
	}
	return "other"
}
