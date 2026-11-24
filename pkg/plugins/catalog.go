package plugins

import (
	"bytes"
	"context"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Catalog badges, official first
var CatalogTiers = []string{"official", "verified", "community"}

// Tiers whose entries may be marked default and installed in every home
var DefaultCatalogTiers = map[string]bool{"official": true, "verified": true}

// Browse shelves a catalog entry may name
var CatalogCategories = []string{"memory", "platform", "web", "tools", "voice", "automation", "models", "general"}

// Hosts catalog images and screenshots may come from
var ImageHosts = []string{"raw.githubusercontent.com", "github.com"}

// Host suffix every GitHub user content host shares
const ImageHostSuffix = ".githubusercontent.com"

var (
	catalogNameRe    = regexp.MustCompile(`^[a-z0-9_-]{1,64}\n?$`)
	catalogShaRe     = regexp.MustCompile(`^[0-9a-f]{40}\n?$`)
	catalogVersionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,31}\n?$`)
)

// Whether a URL is https on a GitHub image host
func IsAllowedImageURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme != "https" || host == "" {
		return false
	}
	for _, h := range ImageHosts {
		if host == h {
			return true
		}
	}
	return strings.HasSuffix(host, ImageHostSuffix)
}

// Tools, hooks, middleware and env vars a catalog entry declares
type CatalogCapabilities struct {
	ProvidesTools      []string
	ProvidesHooks      []string
	ProvidesMiddleware []string
	RequiresEnv        []string
}

// One curated catalog entry pinned to an exact commit
type PluginCatalogEntry struct {
	Name            string
	Repo            string
	Sha             string
	Description     string
	Maintainer      string
	Tier            string
	Category        string
	RequiresIcebath string
	Subdir          string
	DocsURL         string
	Version         string
	Image           string
	Screenshots     []string
	Readme          bool
	Platforms       []string
	Title           string
	Onboarding      bool
	// Installed and enabled once in every home, official and verified entries only
	Default      bool
	Capabilities CatalogCapabilities
	KnownIssues  []string
}

// Installer identifier, repo#subdir for monorepo entries
func (e *PluginCatalogEntry) InstallIdentifier() string {
	if e.Subdir != "" {
		return e.Repo + "#" + e.Subdir
	}
	return e.Repo
}

// Ordered mapping of every field the JSON output renders
func (e *PluginCatalogEntry) ToDict() map[string]any {
	caps := e.Capabilities
	capabilities := map[string]any{
		"provides_tools": stringsAny(caps.ProvidesTools), "provides_hooks": stringsAny(caps.ProvidesHooks),
		"provides_middleware": stringsAny(caps.ProvidesMiddleware), "requires_env": stringsAny(caps.RequiresEnv),
	}
	config.SetMappingKeys(capabilities, []string{"provides_tools", "provides_hooks", "provides_middleware", "requires_env"})
	out := map[string]any{
		"name": e.Name, "repo": e.Repo, "sha": e.Sha, "description": e.Description,
		"maintainer": e.Maintainer, "tier": e.Tier, "category": e.Category,
		"requires_icebath": e.RequiresIcebath, "subdir": e.Subdir, "docs_url": e.DocsURL, "version": e.Version,
		"image": e.Image, "screenshots": stringsAny(e.Screenshots), "readme": e.Readme,
		"platforms": stringsAny(e.Platforms), "title": e.Title, "onboarding": e.Onboarding,
		"default":      e.Default,
		"capabilities": capabilities,
		"known_issues": stringsAny(e.KnownIssues),
	}
	config.SetMappingKeys(out, []string{"name", "repo", "sha", "description", "maintainer", "tier", "category", "requires_icebath",
		"subdir", "docs_url", "version", "image", "screenshots", "readme", "platforms", "title", "onboarding", "default", "capabilities", "known_issues"})
	return out
}

// Copied string slice as a list value
func stringsAny(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

// Text of every string, boolean or number item of a list, empty for a non list
func catalogStrList(raw any) []string {
	list, ok := raw.([]any)
	if !ok {
		return []string{}
	}
	out := []string{}
	for _, x := range list {
		switch x.(type) {
		case string, bool, int, int64, float64:
			out = append(out, anyutil.String(x))
		default:
			if _, isInt := anyutil.Int(x); isInt {
				out = append(out, anyutil.String(x))
			}
		}
	}
	return out
}

// Text of a mapping value, the default when the value is empty or false
func catalogField(data map[string]any, key, def string) string {
	v := data[key]
	if !jsonTruthy(v) {
		return def
	}
	return anyutil.String(v)
}

// Whether a mapping value is exactly the boolean want
func isPyBool(v any, want bool) bool {
	b, ok := v.(bool)
	return ok && b == want
}

// Validated entry of one catalog mapping, nil with a warning on any failure
func EntryFromMapping(raw any, label string) *PluginCatalogEntry {
	data, ok := asStringMap(raw)
	if !ok {
		pkgLog().Warn("Plugin catalog: " + label + ": entry must be a mapping")
		return nil
	}
	name := catalogField(data, "name", "")
	repo := catalogField(data, "repo", "")
	sha := strings.ToLower(strings.TrimSpace(catalogField(data, "sha", "")))
	tier := catalogField(data, "tier", "community")
	category := catalogField(data, "category", "general")
	problem := ""
	switch {
	case !catalogNameRe.MatchString(name):
		problem = "invalid name " + errchain.Show(name) + " (must match [a-z0-9_-]{1,64})"
	case !strings.HasPrefix(repo, "https://"):
		problem = "repo must be an https:// URL (got " + errchain.Show(repo) + ")"
	case !catalogShaRe.MatchString(sha):
		problem = "sha must be a full 40-character hex commit SHA (got " + errchain.Show(data["sha"]) + ")"
	case !containsString(CatalogTiers, tier):
		problem = "tier must be one of " + strings.Join(CatalogTiers, "/") + " (got " + errchain.Show(tier) + ")"
	case !containsString(CatalogCategories, category):
		problem = "category must be one of " + strings.Join(CatalogCategories, "/") + " (got " + errchain.Show(category) + ")"
	}
	if problem != "" {
		pkgLog().Warn("Plugin catalog: " + label + ": " + problem)
		return nil
	}
	caps, _ := asStringMap(data["capabilities"])
	version := strings.TrimSpace(catalogField(data, "version", ""))
	if version != "" && !catalogVersionRe.MatchString(version) {
		pkgLog().Warn("Plugin catalog: " + label + ": ignoring version " + errchain.Show(version) + " (max 32 chars of [A-Za-z0-9._+-])")
		version = ""
	}
	image := strings.TrimSpace(catalogField(data, "image", ""))
	if image != "" && !IsAllowedImageURL(image) {
		pkgLog().Warn("Plugin catalog: " + label + ": ignoring image " + errchain.Show(image) + " (must be https on a GitHub host)")
		image = ""
	}
	isDefault := isPyBool(data["default"], true)
	if isDefault && !DefaultCatalogTiers[tier] {
		pkgLog().Warn("Plugin catalog: " + label + ": ignoring default: true (tier " + tier + "; only official/verified entries ship by default)")
		isDefault = false
	}
	declared := catalogStrList(data["screenshots"])
	screenshots := []string{}
	for _, s := range declared {
		if IsAllowedImageURL(s) {
			screenshots = append(screenshots, s)
		}
	}
	if len(screenshots) != len(declared) {
		pkgLog().Warn("Plugin catalog: " + label + ": ignoring screenshots off GitHub hosts")
	}
	requires := data["requires_icebath"]
	requiresText := ""
	if jsonTruthy(requires) {
		requiresText = strings.TrimSpace(anyutil.String(requires))
	}
	return &PluginCatalogEntry{
		Name: name, Repo: repo, Sha: sha,
		Description: strings.TrimSpace(catalogField(data, "description", "")),
		Maintainer:  strings.TrimSpace(catalogField(data, "maintainer", "")), Tier: tier, Category: category,
		RequiresIcebath: requiresText,
		Subdir:          strings.TrimSpace(catalogField(data, "subdir", "")), DocsURL: strings.TrimSpace(catalogField(data, "docs_url", "")),
		Version: version, Image: image, Screenshots: screenshots, Readme: !isPyBool(data["readme"], false),
		Platforms: catalogStrList(data["platforms"]),
		Title:     strings.TrimSpace(catalogField(data, "title", "")), Onboarding: isPyBool(data["onboarding"], true),
		Default:     isDefault,
		KnownIssues: catalogStrList(data["known_issues"]),
		Capabilities: CatalogCapabilities{
			ProvidesTools: catalogStrList(caps["provides_tools"]), ProvidesHooks: catalogStrList(caps["provides_hooks"]),
			ProvidesMiddleware: catalogStrList(caps["provides_middleware"]), RequiresEnv: catalogStrList(caps["requires_env"]),
		},
	}
}

// String keyed view of a decoded mapping
func asStringMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case map[any]any:
		return asMap(m), true
	}
	return nil, false
}

// Whether items holds s
func containsString(items []string, s string) bool {
	for _, item := range items {
		if item == s {
			return true
		}
	}
	return false
}

// Parsed YAML of one catalog file read as utf-8-sig, nil with a warning when unreadable or malformed
func readCatalogYAML(raw []byte, readErr error, label string) any {
	if readErr != nil {
		pkgLog().Warn("Plugin catalog: failed to read " + label + ": " + errchain.OSText(readErr))
		return nil
	}
	if err := textutil.ValidateUTF8(raw); err != nil {
		pkgLog().Warn("Plugin catalog: failed to read " + label + ": " + err.Error())
		return nil
	}
	doc, err := config.FastSafeLoad(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))
	if err != nil {
		pkgLog().Warn("Plugin catalog: failed to read " + label + ": " + err.Error())
		return nil
	}
	if !jsonTruthy(doc) {
		return map[string]any{}
	}
	return doc
}

// Catalog files of a directory sorted by name, the embedded in-tree catalog when dir is empty
func catalogFiles(dir string) ([]string, func(string) ([]byte, error), error) {
	if dir == "" {
		dir = catalogDirOverride
	}
	if dir == "" {
		names, err := fs.Glob(inTreeCatalog, "catalog/*.yaml")
		sort.Strings(names)
		return names, inTreeCatalog.ReadFile, err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, os.ReadFile, nil
	}
	names, err := filepath.Glob(filepath.Join(globEscape(dir), "*.yaml"))
	sort.Strings(names)
	return names, os.ReadFile, err
}

// Directory path with glob metacharacters escaped
func globEscape(dir string) string {
	var b strings.Builder
	for _, r := range dir {
		if strings.ContainsRune(`*?[\`, r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Every valid entry of a catalog directory except removed.yaml, error only when the directory cannot be listed
func loadCatalog(dir string) ([]*PluginCatalogEntry, error) {
	names, read, err := catalogFiles(dir)
	if err != nil {
		return nil, err
	}
	entries := []*PluginCatalogEntry{}
	for _, path := range names {
		if filepath.Base(path) == "removed.yaml" {
			continue
		}
		raw, err := read(path)
		data := readCatalogYAML(raw, err, path)
		if data == nil {
			continue
		}
		if entry := EntryFromMapping(data, path); entry != nil {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

// Every valid in-tree or directory catalog entry sorted by file name, malformed ones skipped with a warning
func LoadCatalog(dir string) []*PluginCatalogEntry {
	entries, err := loadCatalog(dir)
	if err != nil {
		pkgLog().Warn("Plugin catalog: failed to list the catalog", "error", err)
	}
	return entries
}

// In-tree catalog entries the live merge compares against, swapped by tests
var inTreeCatalogEntries = func() []*PluginCatalogEntry { return LoadCatalog("") }

// In-tree catalog entry of a name, nil when the catalog lacks it
func GetCatalogEntry(name, dir string) *PluginCatalogEntry {
	for _, e := range LoadCatalog(dir) {
		if e.Name == name {
			return e
		}
	}
	return nil
}

// In-tree catalog entry of a name, nil when absent, an error when the shipped catalog cannot be listed
func GetPluginCatalogEntry(name string) (*PluginCatalogEntry, error) {
	entries, err := loadCatalog("")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Name == name {
			return e, nil
		}
	}
	return nil, nil
}

// Entries whose name, description or declared tools contain the query case insensitively, all for an empty query
func FilterEntries(entries []*PluginCatalogEntry, query string) []*PluginCatalogEntry {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return entries
	}
	out := []*PluginCatalogEntry{}
	for _, e := range entries {
		hay := append([]string{e.Name, e.Description}, e.Capabilities.ProvidesTools...)
		for _, h := range hay {
			if strings.Contains(strings.ToLower(h), q) {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// Kill list entry matching a catalog name or repo, the in-tree list united with the live one when dir is empty
func FindRemoved(ctx context.Context, nameOrRepo, dir string) *RemovedEntry {
	if nameOrRepo == "" {
		return nil
	}
	entries := loadRemovedList(dir)
	if dir == "" {
		entries = append(entries, LiveRemovedList(ctx)...)
	}
	return matchRemoved(nameOrRepo, entries)
}

// Full kill list in one resolution, the in-tree list united with the live one
func ResolvedRemovedEntries(ctx context.Context) []RemovedEntry {
	return append(loadRemovedList(""), LiveRemovedList(ctx)...)
}

// One candidate against a resolved kill list by exact name or normalized repo
func MatchRemoved(candidate string, entries []RemovedEntry) *RemovedEntry {
	return matchRemoved(candidate, entries)
}

// One paragraph of what installing or enabling an entry grants
func EntryCapabilitySummary(e *PluginCatalogEntry) string {
	caps := e.Capabilities
	var parts []string
	for _, pair := range []struct {
		label string
		items []string
	}{{"registers tool(s):", caps.ProvidesTools}, {"hook(s):", caps.ProvidesHooks},
		{"middleware:", caps.ProvidesMiddleware}, {"requires env var(s):", caps.RequiresEnv}} {
		if len(pair.items) > 0 {
			parts = append(parts, pair.label+" "+strings.Join(pair.items, ", "))
		}
	}
	bits := []string{e.Name + " (" + e.Tier + ", maintained by " + e.Maintainer + ")"}
	if e.Description != "" {
		bits = append(bits, e.Description)
	}
	grants := "declares no tools, hooks, middleware, or env vars"
	if len(parts) > 0 {
		grants = strings.Join(parts, "; ")
	}
	bits = append(bits, "This plugin "+grants+".")
	if len(e.Platforms) > 0 {
		bits = append(bits, "Platforms: "+strings.Join(e.Platforms, ", ")+".")
	}
	if e.RequiresIcebath != "" {
		bits = append(bits, "Requires Icebath "+e.RequiresIcebath+".")
	}
	if len(e.KnownIssues) > 0 {
		bits = append(bits, "Known issues: "+strings.Join(e.KnownIssues, "; ")+".")
	}
	return strings.Join(bits, " ")
}
