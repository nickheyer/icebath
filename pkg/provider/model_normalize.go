package provider

import (
	"strings"
	"sync"
)

// First hyphen token of a bare model name mapped onto the vendor slug aggregators expect
var vendorPrefixes = []struct{ prefix, vendor string }{
	{"claude", "anthropic"}, {"gpt", "openai"}, {"o1", "openai"}, {"o3", "openai"}, {"o4", "openai"},
	{"gemini", "google"}, {"gemma", "google"}, {"deepseek", "deepseek"}, {"glm", "z-ai"}, {"kimi", "moonshotai"},
	{"minimax", "minimax"}, {"grok", "x-ai"}, {"qwen", "qwen"}, {"mimo", "xiaomi"}, {"trinity", "arcee-ai"},
	{"nemotron", "nvidia"}, {"llama", "meta-llama"}, {"step", "stepfun"},
}

var aggregatorModelProviders = map[string]bool{"openrouter": true, "nous": true, "ai-gateway": true, "kilocode": true}

var dotToHyphenProviders = map[string]bool{"anthropic": true}

var stripVendorOnlyProviders = map[string]bool{"openai-codex": true}

var matchingPrefixStripProviders = map[string]bool{
	"zai": true, "kimi-coding": true, "kimi-coding-cn": true, "minimax": true, "minimax-oauth": true,
	"minimax-cn": true, "alibaba": true, "qwen-oauth": true, "xiaomi": true, "arcee": true, "ollama-cloud": true,
	"nebius-token-factory": true, "custom": true, "gemini": true, "xai": true,
}

var cataloguePrefixRepairProviders = map[string]bool{"nvidia": true}

var lowercaseModelProviders = map[string]bool{"xiaomi": true}

var deepseekRetiredAliases = map[string]string{"deepseek-chat": "deepseek-flash", "deepseek-reasoner": "deepseek-flash"}

// ModelIDNormalizer is a provider profile hook that returns the native id for a model or empty for generic rules
type ModelIDNormalizer func(model, apiKey string) string

var (
	modelIDNormalizersMu sync.RWMutex
	modelIDNormalizers   = map[string]ModelIDNormalizer{}
)

// RegisterModelIDNormalizer installs a profile owned id repair for one canonical provider
func RegisterModelIDNormalizer(provider string, fn ModelIDNormalizer) {
	modelIDNormalizersMu.Lock()
	defer modelIDNormalizersMu.Unlock()
	if fn == nil {
		delete(modelIDNormalizers, provider)
		return
	}
	modelIDNormalizers[provider] = fn
}

// Native id from the provider profile hook, empty when no hook exists or it declines
func profileNormalizeModelID(provider, model string) (out string) {
	modelIDNormalizersMu.RLock()
	fn := modelIDNormalizers[provider]
	modelIDNormalizersMu.RUnlock()
	if fn == nil {
		return ""
	}
	defer func() {
		if r := recover(); r != nil {
			out = ""
		}
	}()
	return fn(model, "")
}

// Folds retired DeepSeek aliases onto their replacement and lowercases everything else
func normalizeForDeepseek(model string) string {
	bare := strings.ToLower(stripVendorPrefix(model))
	if repl, ok := deepseekRetiredAliases[bare]; ok {
		return repl
	}
	return bare
}

// Removes a leading vendor slash segment when present
func stripVendorPrefix(model string) string {
	if i := strings.Index(model, "/"); i >= 0 {
		return model[i+1:]
	}
	return model
}

func dotsToHyphens(model string) string { return strings.ReplaceAll(model, ".", "-") }

// Canonical provider id for a raw name, empty stays empty
func normalizeProviderAlias(name string) string {
	raw := strings.ToLower(strings.TrimSpace(name))
	if raw == "" {
		return raw
	}
	return NormalizeProvider(raw)
}

// Strips a provider slash or colon prefix only when it names the target provider
func stripMatchingProviderPrefix(model, target string) string {
	cut := -1
	for _, sep := range []string{"/", ":"} {
		if i := strings.Index(model, sep); i >= 0 && (cut < 0 || i < cut) {
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
	target = normalizeProviderAlias(target)
	if target == "custom" {
		if strings.ToLower(strings.TrimSpace(prefix)) == "custom" {
			return strings.TrimSpace(remainder)
		}
		return model
	}
	normalizedPrefix := normalizeProviderAlias(prefix)
	if normalizedPrefix != "" && normalizedPrefix == target {
		return strings.TrimSpace(remainder)
	}
	return model
}

// DetectVendor returns the vendor slug implied by a model name or empty when none is recognisable
func DetectVendor(model string) string {
	name := strings.TrimSpace(model)
	if name == "" {
		return ""
	}
	if i := strings.Index(name, "/"); i >= 0 {
		return strings.ToLower(name[:i])
	}
	lower := strings.ToLower(name)
	first := strings.SplitN(lower, "-", 2)[0]
	for _, vp := range vendorPrefixes {
		if vp.prefix == first {
			return vp.vendor
		}
	}
	for _, vp := range vendorPrefixes {
		if strings.HasPrefix(lower, vp.prefix) {
			return vp.vendor
		}
	}
	return ""
}

// Prepends the detected vendor slug for aggregator APIs, leaving slashed or unknown names alone
func prependVendor(model string) string {
	if strings.Contains(model, "/") {
		return model
	}
	if v := DetectVendor(model); v != "" {
		return v + "/" + model
	}
	return model
}

// Restores a dropped vendor prefix only when exactly one curated catalogue entry ends in the bare id
func repairPrefixFromCatalogue(model, provider string) string {
	if strings.Contains(model, "/") {
		return model
	}
	needle := strings.ToLower(strings.TrimSpace(model))
	matches := map[string]bool{}
	for _, entry := range staticCatalogModels(provider) {
		i := strings.Index(entry, "/")
		if i < 0 {
			continue
		}
		if strings.ToLower(strings.TrimSpace(entry[i+1:])) == needle {
			matches[entry] = true
		}
	}
	if len(matches) != 1 {
		return model
	}
	for m := range matches {
		return m
	}
	return model
}

// SuggestPrefixedModelID returns the unambiguous catalogue id for a bare model name or empty
func SuggestPrefixedModelID(provider, model string) string {
	name := strings.TrimSpace(model)
	if name == "" || strings.Contains(name, "/") {
		return ""
	}
	repaired := repairPrefixFromCatalogue(name, normalizeProviderAlias(provider))
	if repaired == name {
		return ""
	}
	return repaired
}

// NormalizeModelForProvider rewrites a bare, vendor prefixed, or native model name into the target provider's form
func NormalizeModelForProvider(model, targetProvider string) string {
	name := strings.TrimSpace(model)
	if name == "" {
		return name
	}
	provider := normalizeProviderAlias(targetProvider)
	if aggregatorModelProviders[provider] {
		return prependVendor(name)
	}
	if family := OpenCodeFamily(provider); family != "" {
		if i := strings.Index(name, "/"); i >= 0 {
			if bare := strings.TrimSpace(name[i+1:]); bare != "" {
				name = bare
			}
		}
		if family == "opencode-zen" && strings.HasPrefix(strings.ToLower(name), "claude-") {
			return dotsToHyphens(name)
		}
		return name
	}
	if dotToHyphenProviders[provider] {
		bare := stripMatchingProviderPrefix(name, provider)
		if strings.Contains(bare, "/") {
			return bare
		}
		return dotsToHyphens(bare)
	}
	if native := profileNormalizeModelID(provider, name); native != "" {
		return native
	}
	if stripVendorOnlyProviders[provider] {
		stripped := stripMatchingProviderPrefix(name, provider)
		if stripped == name && (strings.HasPrefix(name, "openai/") || strings.HasPrefix(name, "openai:")) {
			return name[len("openai/"):]
		}
		return stripped
	}
	if provider == "deepseek" {
		bare := stripMatchingProviderPrefix(name, provider)
		if strings.Contains(bare, "/") {
			return bare
		}
		return normalizeForDeepseek(bare)
	}
	if matchingPrefixStripProviders[provider] {
		result := stripMatchingProviderPrefix(name, provider)
		if lowercaseModelProviders[provider] {
			return strings.ToLower(result)
		}
		return result
	}
	if cataloguePrefixRepairProviders[provider] {
		return repairPrefixFromCatalogue(name, provider)
	}
	return name
}
