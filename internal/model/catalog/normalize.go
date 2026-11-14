package catalog

import (
	"context"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/providers"
)

type vendorPrefix struct {
	prefix string
	vendor string
}

var vendorPrefixes = []vendorPrefix{
	{"claude", "anthropic"}, {"gpt", "openai"}, {"o1", "openai"}, {"o3", "openai"}, {"o4", "openai"},
	{"gemini", "google"}, {"gemma", "google"}, {"deepseek", "deepseek"}, {"glm", "z-ai"},
	{"kimi", "moonshotai"}, {"minimax", "minimax"}, {"grok", "x-ai"}, {"qwen", "qwen"},
	{"mimo", "xiaomi"}, {"trinity", "arcee-ai"}, {"nemotron", "nvidia"}, {"llama", "meta-llama"},
	{"step", "stepfun"},
}

var (
	slugAggregators         = []string{"openrouter", providers.ProviderPortal, "ai-gateway", "kilocode"}
	dotToHyphenProviders    = []string{"anthropic"}
	stripVendorProviders    = []string{"openai-codex"}
	matchingPrefixProviders = []string{
		"zai", "kimi-coding", "kimi-coding-cn", "minimax", "minimax-oauth", "minimax-cn", "alibaba", "qwen-oauth",
		"xiaomi", "arcee", "ollama-cloud", "nebius-token-factory", "custom", "gemini", "xai",
	}
	cataloguePrefixRepairProviders = []string{"nvidia"}
	lowercaseModelProviders        = []string{"xiaomi"}
	deepSeekRetiredAliases         = map[string]string{"deepseek-chat": "deepseek-flash", "deepseek-reasoner": "deepseek-flash"}
)

var modelSearchAliases = map[string][]string{
	"k3":               {"kimi-k3", "kimi"},
	"x-preview-f-free": {"ox-alpha", "ox"},
}

func modelAliasCanonical(model string) string {
	key := strings.ToLower(strings.TrimSpace(model))
	if aliases := modelSearchAliases[key]; len(aliases) > 0 {
		return strings.ToLower(aliases[0])
	}
	return key
}

func ModelSearchText(model string) string {
	id := strings.TrimSpace(model)
	if id == "" {
		return model
	}
	if aliases := modelSearchAliases[strings.ToLower(id)]; len(aliases) > 0 {
		return id + " " + strings.Join(aliases, " ")
	}
	return id
}

func afterVendor(model string) string {
	if _, rest, ok := strings.Cut(model, "/"); ok {
		return rest
	}
	return model
}

func normalizeForDeepSeek(model string) string {
	bare := strings.ToLower(afterVendor(model))
	if replacement, ok := deepSeekRetiredAliases[bare]; ok {
		return replacement
	}
	return bare
}

func (c *Catalog) normalizeProviderAlias(ctx context.Context, provider string) string {
	raw := strings.ToLower(strings.TrimSpace(provider))
	if raw == "" {
		return raw
	}
	return c.normalizeProvider(ctx, raw)
}

func (c *Catalog) stripMatchingProviderPrefix(ctx context.Context, model, target string) string {
	cut := -1
	for _, i := range []int{strings.Index(model, "/"), strings.Index(model, ":")} {
		if i >= 0 && (cut < 0 || i < cut) {
			cut = i
		}
	}
	if cut < 0 {
		return model
	}
	prefix, remainder := model[:cut], model[cut+1:]
	if strings.TrimSpace(prefix) == "" || strings.TrimSpace(remainder) == "" {
		return model
	}
	normalizedTarget := c.normalizeProviderAlias(ctx, target)
	if normalizedTarget == "custom" {
		if strings.ToLower(strings.TrimSpace(prefix)) == "custom" {
			return strings.TrimSpace(remainder)
		}
		return model
	}
	normalizedPrefix := c.normalizeProviderAlias(ctx, prefix)
	if normalizedPrefix != "" && normalizedPrefix == normalizedTarget {
		return strings.TrimSpace(remainder)
	}
	return model
}

func DetectVendor(model string) (string, bool) {
	name := strings.TrimSpace(model)
	if name == "" {
		return "", false
	}
	if vendor, _, ok := strings.Cut(name, "/"); ok {
		vendor = strings.ToLower(vendor)
		return vendor, vendor != ""
	}
	lower := strings.ToLower(name)
	first, _, _ := strings.Cut(lower, "-")
	for _, row := range vendorPrefixes {
		if row.prefix == first {
			return row.vendor, true
		}
	}
	for _, row := range vendorPrefixes {
		if strings.HasPrefix(lower, row.prefix) {
			return row.vendor, true
		}
	}
	return "", false
}

func prependVendor(model string) string {
	if strings.Contains(model, "/") {
		return model
	}
	if vendor, ok := DetectVendor(model); ok {
		return vendor + "/" + model
	}
	return model
}

func (c *Catalog) repairPrefixFromCatalogue(ctx context.Context, model, provider string) string {
	if strings.Contains(model, "/") {
		return model
	}
	needle := strings.ToLower(strings.TrimSpace(model))
	catalogue, _ := c.builtinStaticRow(ctx, provider)
	var matches []string
	for _, entry := range catalogue {
		_, rest, ok := strings.Cut(entry, "/")
		if !ok || strings.ToLower(strings.TrimSpace(rest)) != needle || slices.Contains(matches, entry) {
			continue
		}
		matches = append(matches, entry)
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return model
}

func (c *Catalog) PrefixedModel(ctx context.Context, provider, model string) (string, bool) {
	ctx = c.scoped(ctx)
	name := strings.TrimSpace(model)
	if name == "" || strings.Contains(name, "/") {
		return "", false
	}
	repaired := c.repairPrefixFromCatalogue(ctx, name, c.normalizeProviderAlias(ctx, provider))
	return repaired, repaired != name
}

func dotsToHyphens(model string) string { return strings.ReplaceAll(model, ".", "-") }

func (c *Catalog) normalizeModelForProvider(ctx context.Context, modelInput, target string) string {
	name := strings.TrimSpace(modelInput)
	if name == "" {
		return name
	}
	provider := c.normalizeProviderAlias(ctx, target)
	if slices.Contains(slugAggregators, provider) {
		return prependVendor(name)
	}
	if family, ok := c.reg.OpenCodeFamily(ctx, provider); ok {
		if _, rest, cut := strings.Cut(name, "/"); cut {
			if trimmed := strings.TrimSpace(rest); trimmed != "" {
				name = trimmed
			}
		}
		if family == "opencode-zen" && strings.HasPrefix(strings.ToLower(name), "claude-") {
			return dotsToHyphens(name)
		}
		return name
	}
	if slices.Contains(dotToHyphenProviders, provider) {
		bare := c.stripMatchingProviderPrefix(ctx, name, provider)
		if strings.Contains(bare, "/") {
			return bare
		}
		return dotsToHyphens(bare)
	}
	if p, ok := c.reg.Get(ctx, provider); ok {
		if hook, ok := p.(providers.ModelNormalizer); ok {
			if normalized, ok := hook.NormalizeModel(ctx, name, auth.Secret{}); ok && normalized != "" {
				return normalized
			}
		}
	}
	if slices.Contains(stripVendorProviders, provider) {
		stripped := c.stripMatchingProviderPrefix(ctx, name, provider)
		if stripped == name && (strings.HasPrefix(name, "openai/") || strings.HasPrefix(name, "openai:")) {
			return name[len("openai/"):]
		}
		return stripped
	}
	if provider == "deepseek" {
		bare := c.stripMatchingProviderPrefix(ctx, name, provider)
		if strings.Contains(bare, "/") {
			return bare
		}
		return normalizeForDeepSeek(bare)
	}
	if slices.Contains(matchingPrefixProviders, provider) {
		result := c.stripMatchingProviderPrefix(ctx, name, provider)
		if slices.Contains(lowercaseModelProviders, provider) {
			return strings.ToLower(result)
		}
		return result
	}
	if slices.Contains(cataloguePrefixRepairProviders, provider) {
		return c.repairPrefixFromCatalogue(ctx, name, provider)
	}
	return name
}

func (c *Catalog) NormalizeModelForProvider(ctx context.Context, modelInput, target string) string {
	return c.normalizeModelForProvider(c.scoped(ctx), modelInput, target)
}
