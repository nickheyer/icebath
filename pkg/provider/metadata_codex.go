package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Codex OAuth context windows, lower than the direct API for the same slugs, longest key first
var codexOAuthContextFallback = []contextEntry{
	{"gpt-6-astra", 272_000}, {"gpt-6.1-sol", 272_000}, {"gpt-6-sol", 272_000}, {"gpt-6-luna", 272_000},
	{"gpt-5.1-codex-max", 272_000}, {"gpt-5.1-codex-mini", 272_000}, {"gpt-5.3-codex", 272_000},
	{"gpt-5.3-codex-spark", 128_000}, {"gpt-5.2-codex", 272_000}, {"gpt-5.4-mini", 272_000},
	{"gpt-5.6-sol", 272_000}, {"gpt-5.6-terra", 272_000}, {"gpt-5.6-luna", 272_000}, {"gpt-daybreak-blue-latest", 272_000},
	{"gpt-5.5", 272_000}, {"gpt-5.4", 272_000}, {"gpt-5.2", 272_000}, {"gpt-5", 272_000},
}

// Families Codex advertises at 272K but accepts far above, opted in through the 900k picker variants
var codexOAuthVerifiedAboveAdvertisedPrefixes = []contextEntry{
	{"gpt-5.6", 900_000}, {"gpt-6-sol", 900_000}, {"gpt-6-luna", 900_000},
}

var codexOAuthVerifiedAboveAdvertisedExact = map[string]int{
	"gpt-5.4": 900_000, "gpt-daybreak-blue-latest": 900_000,
	"gpt-6-astra": 900_000,
	"gpt-6.1-sol": 900_000,
}

// The only advertised value the bump may override
const codexOAuthStaleAdvertisedCtx = 272_000

// CodexContextVariantSuffix is the picker only opt in suffix never sent on the wire
const CodexContextVariantSuffix = "-900k"

var codex900kSnapshotBases = []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-sol", "gpt-6-luna"}

var codex900kEligibleBases = map[string]bool{
	"gpt-5.6-sol": true, "gpt-5.6-terra": true, "gpt-5.6-luna": true, "gpt-6-sol": true, "gpt-6-luna": true,
	"gpt-5.4": true, "gpt-daybreak-blue-latest": true, "gpt-6-astra": true, "gpt-6.1-sol": true,
}

var codex900kSnapshotRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// Lowercased slug without a vendor namespace
func bareCodexSlug(model string) string {
	return lastPathSegment(strings.ToLower(strings.TrimSpace(model)))
}

// IsCodex900kBase is the single source of truth for 900k eligibility
func IsCodex900kBase(model string) bool {
	slug := bareCodexSlug(model)
	if slug == "" || strings.HasSuffix(slug, CodexContextVariantSuffix) {
		return false
	}
	if codex900kEligibleBases[slug] {
		return true
	}
	for _, base := range codex900kSnapshotBases {
		if strings.HasPrefix(slug, base+"-") && codex900kSnapshotRe.MatchString(slug[len(base)+1:]) {
			return true
		}
	}
	return false
}

// Lowercased eligible base of a valid 900k variant, empty otherwise
func codexVariantBase(model string) string {
	slug := bareCodexSlug(model)
	if !strings.HasSuffix(slug, CodexContextVariantSuffix) {
		return ""
	}
	base := slug[:len(slug)-len(CodexContextVariantSuffix)]
	if IsCodex900kBase(base) {
		return base
	}
	return ""
}

// IsCodexContextVariant requires both the suffix and an eligible base
func IsCodexContextVariant(model string) bool { return codexVariantBase(model) != "" }

// StripCodexContextVariantSuffix removes a valid 900k suffix, an ineligible alias is returned unchanged
func StripCodexContextVariantSuffix(model string) string {
	raw := strings.TrimSpace(model)
	if codexVariantBase(raw) != "" {
		return raw[:len(raw)-len(CodexContextVariantSuffix)]
	}
	return raw
}

// HasCodexContextVariant is the picker side alias of the eligibility check
func HasCodexContextVariant(modelBare string) bool { return IsCodex900kBase(modelBare) }

// Live verified cap for a valid 900k variant only
func verifiedCodexCtxForSlug(modelBare string) (int, bool) {
	base := codexVariantBase(modelBare)
	if base == "" {
		return 0, false
	}
	if exact, ok := codexOAuthVerifiedAboveAdvertisedExact[base]; ok {
		return exact, true
	}
	for _, e := range codexOAuthVerifiedAboveAdvertisedPrefixes {
		if base == e.key || strings.HasPrefix(base, e.key+"-") || strings.HasPrefix(base, e.key+".") {
			return e.ctx, true
		}
	}
	return 0, false
}

type codexContextCacheEntry struct {
	lengths map[string]int
	at      time.Time
}

var (
	codexOAuthContextCache    = map[string]codexContextCacheEntry{}
	codexOAuthMaxContextCache = map[string]map[string]int{}
)

const codexOAuthContextCacheTTL = 3600 * time.Second

// A probe that found no catalog is retried after this interval
const codexOAuthContextNegativeTTL = 300 * time.Second

// CodexModelsCatalogEndpoint is the hard coded Codex catalog
const CodexModelsCatalogEndpoint = "https://chatgpt.com/backend-api/codex/models"

const CodexNewestClientVersion = "99.0.0"

const CodexUngatedClientVersion = "0.0.0"

// CodexCatalogURLs builds the catalog urls against a gateway base or the hard coded endpoint
func CodexCatalogURLs(baseURL string) []string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	endpoint := CodexModelsCatalogEndpoint
	if base != "" {
		endpoint = base + "/models"
	}
	return []string{endpoint + "?client_version=" + CodexNewestClientVersion, endpoint + "?client_version=" + CodexUngatedClientVersion}
}

// Whether a catalog probe may carry the token to the base url, chatgpt.com only takes JWTs
func codexCatalogProbeAllowed(accessToken, baseURL string) bool {
	if accessToken == "" {
		return false
	}
	base := strings.TrimSpace(baseURL)
	if base == "" {
		base = CodexModelsCatalogEndpoint
	}
	if !HostMatches(base, "chatgpt.com") {
		return true
	}
	return len(DecodeJWTClaims(accessToken)) > 0
}

// FetchCodexCatalogEntries returns the models of the first catalog url answering 200 with a non empty list
func FetchCodexCatalogEntries(get func(string) (*probeResponse, error), baseURL string) ([]any, int, error) {
	status := 0
	for _, target := range CodexCatalogURLs(baseURL) {
		resp, err := get(target)
		if err != nil {
			return nil, status, err
		}
		status = resp.StatusCode
		if status != 200 {
			continue
		}
		if entries, ok := resp.JSON()["models"].([]any); ok && len(entries) > 0 {
			return entries, status, nil
		}
	}
	return []any{}, status, nil
}

// Non secret cache key for a Codex token and the base it was probed against
func codexOAuthTokenFingerprint(accessToken, baseURL string) string {
	sum := sha256.Sum256([]byte(accessToken + "\n" + strings.TrimRight(strings.TrimSpace(baseURL), "/")))
	return hex.EncodeToString(sum[:])[:16]
}

// Remembers a probe that found no catalog so the shared freshness check expires it after the negative TTL
func rememberNoCodexCatalog(cacheKey string) {
	memoLock.Lock()
	defer memoLock.Unlock()
	now := wallNow()
	if current, ok := codexOAuthContextCache[cacheKey]; ok && len(current.lengths) > 0 && now.Sub(current.at) < codexOAuthContextCacheTTL {
		return
	}
	boundCodexCaches(cacheKey)
	codexOAuthContextCache[cacheKey] = codexContextCacheEntry{lengths: map[string]int{}, at: now.Add(-codexOAuthContextCacheTTL + codexOAuthContextNegativeTTL)}
}

// Account scoped catalog context lengths and whether this was a fresh authenticated probe
func fetchCodexOAuthContextLengthsWithSource(ctx context.Context, accessToken, baseURL string) (map[string]int, bool) {
	if !codexCatalogProbeAllowed(accessToken, baseURL) {
		return map[string]int{}, false
	}
	now := wallNow()
	cacheKey := codexOAuthTokenFingerprint(accessToken, baseURL)
	memoLock.Lock()
	cached, ok := codexOAuthContextCache[cacheKey]
	memoLock.Unlock()
	if ok && now.Sub(cached.at) < codexOAuthContextCacheTTL {
		return cached.lengths, false
	}
	headers := map[string]string{"Authorization": "Bearer " + accessToken}
	for k, v := range codexAccountHeaders(accessToken) {
		headers[k] = v
	}
	client := newProbeClient(ctx, "", headers, 5*time.Second, 10*time.Second)
	entries, status, err := FetchCodexCatalogEntries(func(u string) (*probeResponse, error) { return client.Get(u, nil) }, baseURL)
	if err != nil {
		metadataLog.Debug("Codex /models probe failed", "error", err)
		rememberNoCodexCatalog(cacheKey)
		return map[string]int{}, false
	}
	if status != 200 {
		metadataLog.Debug("Codex /models probe returned non 200, falling back to hardcoded defaults", "status", status)
		rememberNoCodexCatalog(cacheKey)
		return map[string]int{}, false
	}
	rememberCodexCatalog(cacheKey, entries)
	result, maxResult := map[string]int{}, map[string]int{}
	for _, raw := range entries {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		slug, isString := item["slug"].(string)
		ctxLen, hasCtx := intValue(item["context_window"])
		if !isString || !hasCtx || ctxLen <= 0 {
			continue
		}
		result[strings.TrimSpace(slug)] = ctxLen
		if maxCtx, ok := intValue(item["max_context_window"]); ok && maxCtx > 0 {
			maxResult[strings.TrimSpace(slug)] = maxCtx
		}
	}
	if len(result) == 0 {
		rememberNoCodexCatalog(cacheKey)
		return map[string]int{}, false
	}
	memoLock.Lock()
	boundCodexCaches(cacheKey)
	codexOAuthMaxContextCache[cacheKey] = maxResult
	codexOAuthContextCache[cacheKey] = codexContextCacheEntry{lengths: result, at: now}
	memoLock.Unlock()
	return result, true
}

// Catalog key for the slug, exact then case insensitive
func codexCatalogKey(keys map[string]int, slug string) (string, bool) {
	if _, ok := keys[slug]; ok {
		return slug, true
	}
	for _, k := range sortedStringKeys(keys) {
		if strings.EqualFold(k, slug) {
			return k, true
		}
	}
	return "", false
}

// Max context window the catalog behind the token and base last published for the slug
func cachedCodexCatalogMax(accessToken, baseURL, slug string) int {
	memoLock.Lock()
	published := codexOAuthMaxContextCache[codexOAuthTokenFingerprint(accessToken, baseURL)]
	memoLock.Unlock()
	if key, ok := codexCatalogKey(published, slug); ok {
		return published[key]
	}
	return 0
}

// Context length and source for a Codex OAuth slug, live, memory, fallback or empty
func resolveCodexOAuthContextLengthWithSource(ctx context.Context, model, accessToken, baseURL string) (int, string) {
	modelBare := strings.TrimSpace(stripProviderPrefix(model))
	if modelBare == "" {
		return 0, ""
	}
	applyVerifiedBump := func(n int, source string, catalogMax int) (int, string) {
		bumped, ok := verifiedCodexCtxForSlug(modelBare)
		if ok && n == codexOAuthStaleAdvertisedCtx {
			if catalogMax > 0 && catalogMax < bumped {
				bumped = catalogMax
			}
			metadataLog.Debug(fmt.Sprintf("Codex OAuth context for %s: advertised %d raised to live-verified %d", modelBare, n, bumped))
			return bumped, source
		}
		return n, source
	}
	lookupBare := bareCodexSlug(StripCodexContextVariantSuffix(modelBare))
	catalogMax := 0
	if accessToken != "" {
		live, fresh := fetchCodexOAuthContextLengthsWithSource(ctx, accessToken, baseURL)
		catalogMax = cachedCodexCatalogMax(accessToken, baseURL, lookupBare)
		if slug, ok := codexCatalogKey(live, lookupBare); ok {
			source := "memory"
			if fresh {
				source = "live"
			}
			return applyVerifiedBump(live[slug], source, catalogMax)
		}
	}
	if hit, ok := longestKeyMatch(codexOAuthContextFallback, strings.ToLower(lookupBare)); ok {
		return applyVerifiedBump(hit.ctx, "fallback", catalogMax)
	}
	return 0, ""
}
