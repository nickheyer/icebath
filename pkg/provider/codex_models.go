package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultCodexModels is the curated offline Codex catalog, only slugs the ChatGPT OAuth backend accepts
var DefaultCodexModels = []string{
	"gpt-6-sol",
	"gpt-6-luna",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"gpt-5.5",
	"gpt-5.4-mini",
	"gpt-5.4",
	"gpt-5.3-codex-spark",
}

type codexForwardCompat struct {
	synthetic string
	templates []string
}

// Newer slugs surfaced when an older compatible template is present in live discovery
var codexForwardCompatTemplates = []codexForwardCompat{
	{"gpt-6-sol", []string{"gpt-5.6-sol", "gpt-5.5"}},
	{"gpt-6-luna", []string{"gpt-5.6-luna", "gpt-5.5"}},
	{"gpt-5.6-sol", []string{"gpt-5.5", "gpt-5.4"}},
	{"gpt-5.6-terra", []string{"gpt-5.5", "gpt-5.4"}},
	{"gpt-5.6-luna", []string{"gpt-5.5", "gpt-5.4"}},
	{"gpt-5.5", []string{"gpt-5.4", "gpt-5.4-mini"}},
	{"gpt-5.3-codex-spark", []string{"gpt-5.4", "gpt-5.5"}},
}

// Order preserving dedupe
func codexDedupe(modelIDs []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(modelIDs))
	for _, m := range modelIDs {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// Appends synthetic newer slugs whose template is present
func addCodexForwardCompatModels(modelIDs []string) []string {
	ordered := codexDedupe(modelIDs)
	seen := map[string]bool{}
	for _, m := range ordered {
		seen[m] = true
	}
	for _, fc := range codexForwardCompatTemplates {
		if seen[fc.synthetic] {
			continue
		}
		for _, template := range fc.templates {
			if seen[template] {
				ordered = append(ordered, fc.synthetic)
				seen[fc.synthetic] = true
				break
			}
		}
	}
	return ordered
}

// Inserts the 900k picker variant after each eligible base slug
func addCodexContextVariants(modelIDs []string) []string {
	present := map[string]bool{}
	for _, m := range modelIDs {
		present[m] = true
	}
	out := make([]string, 0, len(modelIDs)*2)
	emitted := map[string]bool{}
	for _, m := range modelIDs {
		out = append(out, m)
		emitted[m] = true
		variant := m + CodexContextVariantSuffix
		if present[variant] || emitted[variant] {
			continue
		}
		if HasCodexContextVariant(m) {
			out = append(out, variant)
			emitted[variant] = true
		}
	}
	return out
}

// Forward compat synthesis then large context variant synthesis
func finalizeCodexModels(modelIDs []string) []string {
	return addCodexContextVariants(addCodexForwardCompatModels(modelIDs))
}

// Curated openai-codex row of the static catalog
func codexCuratedModels() []string {
	return finalizeCodexModels(cloneStrings(DefaultCodexModels))
}

// Astra only enters a catalog through live account scoped discovery
func dropUndiscoveredAstra(modelIDs []string) []string {
	out := make([]string, 0, len(modelIDs))
	for _, m := range modelIDs {
		if !isAstraModel(m) {
			out = append(out, m)
		}
	}
	return out
}

// CodexCatalogCredentialIdentity names the principal and route live discovery would use for the cache key
func CodexCatalogCredentialIdentity(ctx context.Context) string {
	token, route := "", ""
	if creds, err := resolveCodexRuntimeCredentialsReadOnlyFn(modelsPoolEnv(ctx)); err == nil {
		token = stringOf(creds["api_key"])
		route = NormalizeRouteBaseURL(strings.TrimSpace(stringOf(creds["base_url"])))
	}
	if token == "" {
		return "missing"
	}
	if codexAccessTokenIsExpiring(token, 0) {
		return "expired\n" + route
	}
	if accountID, subject, ok := codexPrincipalIdentity(token); ok {
		return accountID + "/" + subject + "\n" + route
	}
	return token + "\n" + route
}

// Visible slugs of a Codex catalog models list sorted by priority then slug
func codexRankedSlugs(entries []any) []string {
	type ranked struct {
		rank int
		slug string
	}
	var sortable []ranked
	for _, item := range entries {
		m, ok := asMap(item)
		if !ok {
			continue
		}
		slug, ok := m["slug"].(string)
		if !ok || strings.TrimSpace(slug) == "" {
			continue
		}
		if visibility, ok := m["visibility"].(string); ok {
			switch strings.ToLower(strings.TrimSpace(visibility)) {
			case "hide", "hidden":
				continue
			}
		}
		rank := 10_000
		if p, ok := floatOf(m["priority"]); ok {
			if _, isBool := m["priority"].(bool); !isBool {
				rank = int(p)
			}
		}
		sortable = append(sortable, ranked{rank, strings.TrimSpace(slug)})
	}
	sort.SliceStable(sortable, func(i, j int) bool {
		if sortable[i].rank != sortable[j].rank {
			return sortable[i].rank < sortable[j].rank
		}
		return sortable[i].slug < sortable[j].slug
	})
	slugs := make([]string, 0, len(sortable))
	for _, r := range sortable {
		slugs = append(slugs, r.slug)
	}
	return codexDedupe(slugs)
}

// Default Codex host honoring the profile override
func codexDiscoveryBaseURL(ctx context.Context) string {
	if override := strings.TrimRight(strings.TrimSpace(GetSecretStr(ctx, "ICEBATH_CODEX_BASE_URL", "")), "/"); override != "" {
		return override
	}
	return DefaultCodexBaseURL
}

// Visible models from the Codex backend at the credential's own host, empty when refused or unreachable
func fetchCodexModelsFromAPI(ctx context.Context, accessToken, baseURL string) []string {
	catalogBase := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if catalogBase == "" {
		catalogBase = codexDiscoveryBaseURL(ctx)
	}
	if !codexCatalogProbeAllowed(accessToken, catalogBase) {
		return []string{}
	}
	headers := map[string]string{"Authorization": "Bearer " + accessToken}
	for k, v := range codexAccountHeaders(accessToken) {
		headers[k] = v
	}
	client := newProbeClient(ctx, catalogBase, headers, 10*time.Second, 10*time.Second)
	entries, _, err := FetchCodexCatalogEntries(func(rawURL string) (*probeResponse, error) {
		return client.Get(rawURL, nil)
	}, catalogBase)
	if err != nil {
		modelsLog.Debug("Failed to fetch Codex models from API", "error", err)
		return []string{}
	}
	return finalizeCodexModels(codexRankedSlugs(entries))
}

// Top level model key of a TOML document, empty when absent
func tomlTopLevelString(text, key string) string {
	for _, line := range strings.Split(strings.TrimPrefix(text, "\xef\xbb\xbf"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			return ""
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		name, value, ok := strings.Cut(trimmed, "=")
		if !ok || strings.TrimSpace(name) != key {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '"' {
			if unquoted, err := strconv.Unquote(value[:strings.LastIndex(value, `"`)+1]); err == nil {
				return unquoted
			}
			return ""
		}
		if len(value) >= 2 && value[0] == '\'' {
			end := strings.Index(value[1:], "'")
			if end < 0 {
				return ""
			}
			return value[1 : end+1]
		}
		return ""
	}
	return ""
}

// The model pinned in the Codex CLI config
func readCodexDefaultModel(codexHome string) string {
	raw, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(tomlTopLevelString(string(raw), "model"))
}

// Ranked slugs of the Codex CLI models cache
func readCodexCacheModels(codexHome string) []string {
	raw, err := os.ReadFile(filepath.Join(codexHome, "models_cache.json"))
	if err != nil {
		return []string{}
	}
	var doc any
	if json.Unmarshal([]byte(strings.TrimPrefix(string(raw), "\xef\xbb\xbf")), &doc) != nil {
		return []string{}
	}
	m, _ := asMap(doc)
	entries, _ := asList(m["models"])
	return codexRankedSlugs(entries)
}

// Codex CLI home from CODEX_HOME else the user's .codex directory
func codexHomeDir() string {
	if explicit := strings.TrimSpace(os.Getenv("CODEX_HOME")); explicit != "" {
		return modelsExpandUser(explicit)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		userHome = "~"
	}
	return filepath.Join(userHome, ".codex")
}

// GetCodexModelIDs is live discovery with a token, else config default, local cache and defaults as a curated fallback
func GetCodexModelIDs(ctx context.Context, accessToken, baseURL string) ([]string, bool) {
	codexHome := codexHomeDir()
	if accessToken != "" {
		if apiModels := fetchCodexModelsFromAPIFn(ctx, accessToken, baseURL); len(apiModels) > 0 {
			return finalizeCodexModels(apiModels), false
		}
	}
	var ids []string
	if defaultModel := readCodexDefaultModel(codexHome); defaultModel != "" {
		ids = append(ids, defaultModel)
	}
	ids = append(ids, readCodexCacheModels(codexHome)...)
	ids = append(ids, DefaultCodexModels...)
	return finalizeCodexModels(dropUndiscoveredAstra(codexDedupe(ids))), true
}
