package catalog

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/wire"
)

type listing struct {
	ids      []string
	fallback bool
	native   bool
}

type compatTemplate struct {
	synthetic string
	templates []string
}

var defaultCodexModels = []string{
	"gpt-6-sol", "gpt-6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.4-mini", "gpt-5.4",
	"gpt-5.3-codex-spark",
}

var forwardCompatTemplates = []compatTemplate{
	{"gpt-6-sol", []string{"gpt-5.6-sol", "gpt-5.5"}},
	{"gpt-6-luna", []string{"gpt-5.6-luna", "gpt-5.5"}},
	{"gpt-5.6-sol", []string{"gpt-5.5", "gpt-5.4"}},
	{"gpt-5.6-terra", []string{"gpt-5.5", "gpt-5.4"}},
	{"gpt-5.6-luna", []string{"gpt-5.5", "gpt-5.4"}},
	{"gpt-5.5", []string{"gpt-5.4", "gpt-5.4-mini"}},
	{"gpt-5.3-codex-spark", []string{"gpt-5.4", "gpt-5.5"}},
}

func dedupe(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func addForwardCompatModels(ids []string) []string {
	ordered := dedupe(ids)
	for _, row := range forwardCompatTemplates {
		if slices.Contains(ordered, row.synthetic) {
			continue
		}
		if slices.ContainsFunc(row.templates, func(template string) bool { return slices.Contains(ordered, template) }) {
			ordered = append(ordered, row.synthetic)
		}
	}
	return ordered
}

func addContextVariants(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id)
		variant := id + CodexContextVariantSuffix
		if slices.Contains(ids, variant) || slices.Contains(out, variant) {
			continue
		}
		if IsCodex900kBase(id) {
			out = append(out, variant)
		}
	}
	return out
}

func finalizeCodexModels(ids []string) []string {
	return addContextVariants(addForwardCompatModels(ids))
}

func dropUndiscoveredAstra(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !wire.IsAstraModel(id) {
			out = append(out, id)
		}
	}
	return out
}

func jwtClaims(token auth.Secret) (wire.Value, bool) {
	raw, ok := auth.JWTClaims(token.Reveal())
	if !ok {
		return wire.Value{}, false
	}
	claims, err := wire.ParseValue(raw)
	if err != nil || !claims.IsObject() {
		return wire.Value{}, false
	}
	return claims, true
}

func codexTokenExpiring(token auth.Secret) bool {
	claims, ok := jwtClaims(token)
	if !ok {
		return false
	}
	expires := claims.Get("exp")
	if expires.Kind() != wire.KindNumber {
		return false
	}
	seconds, ok := expires.Float()
	return ok && seconds <= unixSeconds(time.Now())
}

func codexPrincipalIdentity(token auth.Secret) (accountID, subject string, ok bool) {
	claims, ok := jwtClaims(token)
	if !ok {
		return "", "", false
	}
	accountID, _ = claims.Path("https://api.openai.com/auth", "chatgpt_account_id").Str()
	subject, _ = claims.Get("sub").Str()
	accountID, subject = strings.TrimSpace(accountID), strings.TrimSpace(subject)
	return accountID, subject, accountID != "" && subject != ""
}

func (c *Catalog) codexCatalogCredentialIdentity(ctx context.Context) string {
	creds, err := c.auth.ResolveCodex(ctx, auth.ResolveOptions{ReadOnly: true})
	if err != nil || creds.APIKey.Empty() {
		return "missing"
	}
	route := NormalizeRouteBaseURL(strings.TrimSpace(creds.BaseURL))
	if codexTokenExpiring(creds.APIKey) {
		return "expired\n" + route
	}
	if accountID, subject, ok := codexPrincipalIdentity(creds.APIKey); ok {
		return accountID + "/" + subject + "\n" + route
	}
	return creds.APIKey.Reveal() + "\n" + route
}

func rankedSlugs(entries []wire.Value) []string {
	type ranked struct {
		rank int
		slug string
	}
	var sortable []ranked
	for _, item := range entries {
		if !item.IsObject() {
			continue
		}
		slug, ok := item.Get("slug").Str()
		if !ok || strings.TrimSpace(slug) == "" {
			continue
		}
		if visibility, ok := item.Get("visibility").Str(); ok {
			if v := strings.ToLower(strings.TrimSpace(visibility)); v == "hide" || v == "hidden" {
				continue
			}
		}
		rank := 10_000
		if priority := item.Get("priority"); priority.Kind() == wire.KindNumber {
			if value, ok := priority.Float(); ok {
				rank = int(math.Trunc(value))
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
	for _, row := range sortable {
		slugs = append(slugs, row.slug)
	}
	return dedupe(slugs)
}

func (c *Catalog) codexBaseURL(ctx context.Context) string {
	if override := strings.TrimRight(c.env(ctx, "ICEBATH_CODEX_BASE_URL"), "/"); override != "" {
		return override
	}
	return auth.DefaultCodexBaseURL
}

func (c *Catalog) fetchCodexModelsFromAPI(ctx context.Context, token auth.Secret, baseURL string) []string {
	catalogBase := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if catalogBase == "" {
		catalogBase = c.codexBaseURL(ctx)
	}
	if !CodexCatalogProbeAllowed(token, catalogBase) {
		return nil
	}
	header := http.Header{"Authorization": {"Bearer " + token.Reveal()}}
	for name, value := range auth.CodexAccountHeaders(token.Reveal()) {
		header.Set(name, value)
	}
	entries, _, err := c.fetchCodexCatalogEntries(ctx, header, catalogBase)
	if err != nil {
		slog.Debug("failed to fetch Codex models from the API", "error", err)
		return nil
	}
	return finalizeCodexModels(rankedSlugs(entries))
}

func readCodexDefaultModel(codexHome string) string {
	data, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		return ""
	}
	var payload map[string]any
	if toml.Unmarshal(withoutBOM(data), &payload) != nil {
		return ""
	}
	model, _ := payload["model"].(string)
	return strings.TrimSpace(model)
}

func readCodexCacheModels(codexHome string) []string {
	data, err := os.ReadFile(filepath.Join(codexHome, "models_cache.json"))
	if err != nil {
		return nil
	}
	root, err := wire.ParseValue(withoutBOM(data))
	if err != nil {
		return nil
	}
	entries := root.Get("models")
	if !entries.IsArray() {
		return nil
	}
	return rankedSlugs(entries.Elems())
}

func (c *Catalog) codexHome(ctx context.Context) string {
	if configured := c.env(ctx, "CODEX_HOME"); configured != "" {
		return home.ExpandUser(configured)
	}
	return home.ExpandUser("~/.codex")
}

func (c *Catalog) codexModelIDs(ctx context.Context, token auth.Secret, baseURL string) listing {
	if !token.Empty() {
		if live := c.fetchCodexModelsFromAPI(ctx, token, baseURL); len(live) > 0 {
			return listing{ids: finalizeCodexModels(live)}
		}
	}
	codexHome := c.codexHome(ctx)
	var hints []string
	if model := readCodexDefaultModel(codexHome); model != "" {
		hints = append(hints, model)
	}
	hints = append(hints, readCodexCacheModels(codexHome)...)
	hints = append(hints, defaultCodexModels...)
	return listing{ids: finalizeCodexModels(dropUndiscoveredAstra(dedupe(hints))), fallback: true}
}

func (c *Catalog) CodexModelIDs(ctx context.Context, token auth.Secret, baseURL string) []string {
	return c.codexModelIDs(c.scoped(ctx), token, baseURL).ids
}

func (c *Catalog) codexCatalog(ctx context.Context) listing {
	var token auth.Secret
	baseURL := ""
	if creds, err := c.auth.ResolveCodex(ctx, auth.ResolveOptions{ReadOnly: true}); err == nil {
		token, baseURL = creds.APIKey, creds.BaseURL
		if codexTokenExpiring(token) {
			token = auth.Secret{}
		}
	}
	return c.codexModelIDs(ctx, token, baseURL)
}
