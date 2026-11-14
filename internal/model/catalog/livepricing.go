package catalog

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/blake2b"

	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	failedCatalogTTL      = 120 * time.Second
	portalCatalogTTL      = 300 * time.Second
	pricingFetchTimeout   = 8 * time.Second
	pricingAuthKeyPrefix  = "\x00auth:"
	openRouterPricingBase = "https://openrouter.ai/api"
	fireworksPricingKey   = "models.dev/fireworks"
	portalInferenceOrigin = "https://inference-api.icebath.app"
	aiGatewayBaseURL      = "https://ai-gateway.vercel.sh/v1"
	novitaDefaultBaseURL  = "https://api.novita.ai/openai/v1"
	portalPolicyAppendMax = 64
	billingModeSubscriber = "subscription"
)

type PriceRates struct {
	Prompt     string
	Completion string
	CacheRead  string
	CacheWrite string
}

type PriceRow struct {
	PriceRates
	Original     *PriceRates
	Subscription bool
	TopTier      bool
	UsageLevel   string
	Name         string
	NoTools      bool
	Generation   bool
}

type PriceTable struct {
	ids  []string
	rows map[string]PriceRow
}

type pricingState struct {
	mu           sync.Mutex
	order        []string
	catalogs     map[string]*PriceTable
	retryAfter   map[string]time.Time
	providerKeys map[string]string
}

func newPriceTable() *PriceTable { return &PriceTable{rows: map[string]PriceRow{}} }

func (t *PriceTable) Len() int {
	if t == nil {
		return 0
	}
	return len(t.ids)
}

func (t *PriceTable) IDs() []string {
	if t == nil {
		return nil
	}
	return slices.Clone(t.ids)
}

func (t *PriceTable) Row(id string) (PriceRow, bool) {
	if t == nil {
		return PriceRow{}, false
	}
	row, ok := t.rows[id]
	return row, ok
}

func (t *PriceTable) put(id string, row PriceRow) {
	if _, ok := t.rows[id]; !ok {
		t.ids = append(t.ids, id)
	}
	t.rows[id] = row
}

func (t *PriceTable) clone() *PriceTable {
	out := newPriceTable()
	if t == nil {
		return out
	}
	for _, id := range t.ids {
		out.put(id, t.rows[id])
	}
	return out
}

func blake2bHex(data []byte, size int) string {
	hash, err := blake2b.New(size, nil)
	if err != nil {
		panic("catalog: a BLAKE2b digest of the requested size is unavailable: " + err.Error())
	}
	hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil))
}

func blake2bHex8(data []byte) string { return blake2bHex(data, 8) }

func pricingAuthFingerprint(apiKey string) string {
	if apiKey == "" {
		return ""
	}
	return pricingAuthKeyPrefix + blake2bHex8([]byte(apiKey))
}

func (st *pricingState) drop(key string) {
	delete(st.catalogs, key)
	delete(st.retryAfter, key)
	if i := slices.Index(st.order, key); i >= 0 {
		st.order = slices.Delete(st.order, i, i+1)
	}
}

func (st *pricingState) cachedLocked(key string) (*PriceTable, bool) {
	cached, ok := st.catalogs[key]
	if !ok {
		return nil, false
	}
	if deadline, timed := st.retryAfter[key]; timed && !time.Now().Before(deadline) {
		st.drop(key)
		return nil, false
	}
	return cached, true
}

func (c *Catalog) cachedCatalog(key string) (*PriceTable, bool) {
	st := &c.st.pricing
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.cachedLocked(key)
}

func (c *Catalog) cacheCatalog(key string, result *PriceTable, ttl time.Duration) *PriceTable {
	st := &c.st.pricing
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	for _, cached := range slices.Clone(st.order) {
		if deadline, timed := st.retryAfter[cached]; timed && !now.Before(deadline) {
			st.drop(cached)
		}
	}
	if _, ok := st.catalogs[key]; !ok {
		st.order = append(st.order, key)
	}
	st.catalogs[key] = result
	switch {
	case result.Len() == 0:
		st.retryAfter[key] = now.Add(failedCatalogTTL)
	case ttl > 0:
		st.retryAfter[key] = now.Add(ttl)
	default:
		delete(st.retryAfter, key)
	}
	return result
}

func stripV1(rawURL string) string {
	if strings.HasSuffix(rawURL, "/v1") {
		return strings.TrimRight(rawURL[:len(rawURL)-3], "/")
	}
	return rawURL
}

func (c *Catalog) PeekCachedPricing(baseURL string) *PriceTable {
	root := stripV1(strings.TrimRight(baseURL, "/"))
	authedPrefix := root + pricingAuthKeyPrefix
	st := &c.st.pricing
	st.mu.Lock()
	defer st.mu.Unlock()
	keys := slices.Clone(st.order)
	for i := len(keys) - 1; i >= 0; i-- {
		if !strings.HasPrefix(keys[i], authedPrefix) {
			continue
		}
		if cached, ok := st.cachedLocked(keys[i]); ok && cached.Len() > 0 {
			return cached
		}
	}
	if cached, ok := st.cachedLocked(root); ok {
		return cached
	}
	return newPriceTable()
}

func (c *Catalog) PricingFetchSuppressed(baseURL string) bool {
	root := stripV1(strings.TrimRight(baseURL, "/"))
	st := &c.st.pricing
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	for _, key := range st.order {
		if key != root && !strings.HasPrefix(key, root+pricingAuthKeyPrefix) {
			continue
		}
		if st.catalogs[key].Len() == 0 && st.retryAfter[key].After(now) {
			return true
		}
	}
	return false
}

func FormatPricePerMTok(perToken string) string {
	value, ok := parseFloat(perToken)
	if !ok {
		return "?"
	}
	if value == 0 {
		return "free"
	}
	perM := value * 1_000_000
	text := strconv.FormatFloat(perM, 'f', 2, 64)
	if perM < 0.01 {
		precision := 3
		for precision < 12 {
			rounded, _ := strconv.ParseFloat(strconv.FormatFloat(perM, 'f', precision, 64), 64)
			if rounded != 0 {
				break
			}
			precision++
		}
		text = strconv.FormatFloat(perM, 'f', min(precision+1, 12), 64)
		if strings.Contains(text, ".") {
			text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
		}
	}
	return "$" + text
}

func priceFloat(raw string, positive bool) (float64, bool) {
	if raw == "" {
		return 0, false
	}
	value, ok := parseFloat(raw)
	if !ok || math.IsNaN(value) {
		return 0, false
	}
	if (positive && value <= 0) || (!positive && value < 0) {
		return 0, false
	}
	return value, true
}

func salePercent(current, original string) (int, bool) {
	cur, ok := priceFloat(current, true)
	if !ok {
		return 0, false
	}
	orig, ok := priceFloat(original, true)
	if !ok || cur >= orig {
		return 0, false
	}
	return int(math.RoundToEven((1.0 - cur/orig) * 100)), true
}

func computeSaleDiscount(prompt, completion string, original *PriceRates) (percent int, wasPrompt, wasCompletion string, ok bool) {
	if original != nil {
		wasPrompt, wasCompletion = original.Prompt, original.Completion
	}
	if value, set := priceFloat(prompt, false); set && value == 0 {
		if value, set := priceFloat(completion, false); !set || value == 0 {
			return 100, wasPrompt, wasCompletion, true
		}
	}
	if original == nil || (wasPrompt == "" && wasCompletion == "") {
		return 0, "", "", false
	}
	if pct, set := salePercent(prompt, wasPrompt); set {
		return pct, wasPrompt, wasCompletion, pct >= 1
	}
	if pct, set := salePercent(completion, wasCompletion); set {
		return pct, wasPrompt, wasCompletion, pct >= 1
	}
	return 0, "", "", false
}

func SaleDiscount(row PriceRow) (percent int, wasPrompt, wasCompletion string, ok bool) {
	return computeSaleDiscount(row.Prompt, row.Completion, row.Original)
}

func valueText(v wire.Value) string {
	switch v.Kind() {
	case wire.KindNone:
		return ""
	case wire.KindString:
		return v.Text()
	}
	return string(v.Raw())
}

func pricingRates(pricing wire.Value, promptKey, completionKey string) PriceRates {
	rates := PriceRates{Prompt: valueText(pricing.Get(promptKey)), Completion: valueText(pricing.Get(completionKey))}
	if read := pricing.Get("input_cache_read"); read.Truthy() {
		rates.CacheRead = valueText(read)
	}
	if write := pricing.Get("input_cache_write"); write.Truthy() {
		rates.CacheWrite = valueText(write)
	}
	return rates
}

func perToken(perMTok float64) string { return formatRate(perMTok / 1_000_000) }

func supportsTools(item wire.Value) bool {
	params := item.Get("supported_parameters")
	if !params.IsArray() {
		return true
	}
	return slices.ContainsFunc(params.Elems(), func(v wire.Value) bool {
		text, ok := v.Str()
		return ok && text == "tools"
	})
}

func presentText(v wire.Value) (string, bool) {
	if !v.Exists() || v.Kind() == wire.KindNull {
		return "", false
	}
	text := valueText(v)
	return text, text != ""
}

func (c *Catalog) fetchModelsWithPricing(ctx context.Context, apiKey, baseURL string, timeout time.Duration, force, saleOriginal bool, ttl time.Duration) *PriceTable {
	root := strings.TrimRight(baseURL, "/")
	cacheKey := root + pricingAuthFingerprint(apiKey)
	if !force {
		if cached, ok := c.cachedCatalog(cacheKey); ok {
			return cached
		}
	}
	rawURL := root + "/v1/models"
	header := bearer(http.Header{"Accept": {"application/json"}, "User-Agent": {c.userAgent()}}, apiKey)
	payload, err := c.getJSONObject(ctx, rawURL, header, "", timeout)
	if err != nil {
		return c.cacheCatalog(cacheKey, newPriceTable(), 0)
	}
	items, listed := dataItems(payload)
	if listed {
		c.seedReasoningCaps(ctx, rawURL, items)
	}
	result := newPriceTable()
	for _, item := range items {
		id, pricing := itemID(item), item.Get("pricing")
		if id == "" || !pricing.IsObject() {
			continue
		}
		row := PriceRow{PriceRates: pricingRates(pricing, "prompt", "completion")}
		if saleOriginal {
			if original := pricing.Get("original"); original.IsObject() {
				rates := PriceRates{}
				rates.Prompt, _ = presentText(original.Get("prompt"))
				rates.Completion, _ = presentText(original.Get("completion"))
				rates.CacheRead, _ = presentText(original.Get("input_cache_read"))
				rates.CacheWrite, _ = presentText(original.Get("input_cache_write"))
				if rates.Prompt != "" || rates.Completion != "" {
					row.Original = &rates
				}
			}
			if mode, ok := item.Get("billing_mode").Str(); ok && mode == billingModeSubscriber {
				row.Subscription = true
			}
			row.TopTier = item.Get("top_tier").True()
			if level, ok := item.Get("usage_level").Str(); ok {
				row.UsageLevel = level
			}
			if name, ok := item.Get("name").Str(); ok {
				if trimmed := strings.TrimSpace(name); trimmed != "" && trimmed != id {
					row.Name = trimmed
				}
			}
			row.NoTools = !supportsTools(item)
			row.Generation = CatalogItemIsGeneration(item)
		}
		result.put(id, row)
	}
	return c.cacheCatalog(cacheKey, result, ttl)
}

func (c *Catalog) FetchModelsWithPricing(ctx context.Context, apiKey auth.Secret, baseURL string, force bool) *PriceTable {
	ctx = c.scoped(ctx)
	if baseURL == "" {
		baseURL = openRouterPricingBase
	}
	return c.fetchModelsWithPricing(ctx, apiKey.Reveal(), baseURL, pricingFetchTimeout, force, false, 0)
}

func (c *Catalog) fetchAIGatewayPricing(ctx context.Context, timeout time.Duration, force bool) *PriceTable {
	cacheKey := strings.TrimRight(aiGatewayBaseURL, "/")
	if !force {
		if cached, ok := c.cachedCatalog(cacheKey); ok {
			return cached
		}
	}
	payload, err := c.getJSONObject(ctx, cacheKey+"/models", http.Header{"Accept": {"application/json"}}, "", timeout)
	if err != nil {
		return c.cacheCatalog(cacheKey, newPriceTable(), 0)
	}
	result := newPriceTable()
	for _, item := range objectItems(payload) {
		id, pricing := itemID(item), item.Get("pricing")
		if id != "" && pricing.IsObject() {
			result.put(id, PriceRow{PriceRates: pricingRates(pricing, "input", "output")})
		}
	}
	return c.cacheCatalog(cacheKey, result, 0)
}

func (c *Catalog) portalInferenceOverride(ctx context.Context) string {
	return auth.OptionalBaseURL(c.env(ctx, portal.EnvInferenceBaseURL))
}

func (c *Catalog) portalPricingCredentials(ctx context.Context) (apiKey, baseURL string) {
	envBase := c.portalInferenceOverride(ctx)
	credsBase := ""
	if creds, err := c.portal.RuntimeCredentials(portal.WithBackground(ctx), portal.ResolveOptions{}); err == nil {
		apiKey = creds.APIKey.Reveal()
		credsBase = strings.TrimSpace(creds.BaseURL)
	}
	baseURL = envBase
	if baseURL == "" {
		baseURL = credsBase
	}
	if baseURL == "" {
		baseURL = portalInferenceOrigin
	}
	return apiKey, strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
}

func (c *Catalog) fetchPortalPricing(ctx context.Context, apiKey, baseURL string, force bool) *PriceTable {
	return c.fetchModelsWithPricing(ctx, apiKey, baseURL, pricingFetchTimeout, force, true, portalCatalogTTL)
}

func (c *Catalog) portalPolicyAllowedIDs(ctx context.Context, force bool) map[string]bool {
	if present := c.portal.PolicyPresent(ctx); present == nil || !*present {
		return nil
	}
	apiKey, baseURL := c.portalPricingCredentials(ctx)
	if apiKey == "" || baseURL == "" {
		return nil
	}
	table := c.fetchPortalPricing(ctx, apiKey, baseURL, force)
	if table.Len() == 0 {
		return nil
	}
	allowed := make(map[string]bool, table.Len())
	for _, id := range table.ids {
		allowed[id] = true
	}
	return allowed
}

func (c *Catalog) PortalPolicyAllowedIDs(ctx context.Context, force bool) map[string]bool {
	return c.portalPolicyAllowedIDs(c.scoped(ctx), force)
}

func RestrictToPortalPolicy(modelIDs []string, allowed map[string]bool, rescueEmpty bool) []string {
	if len(allowed) == 0 {
		return slices.Clone(modelIDs)
	}
	kept := []string{}
	for _, id := range modelIDs {
		base, _, _ := strings.Cut(id, ":")
		if allowed[id] || allowed[base] {
			kept = append(kept, id)
		}
	}
	if rescueEmpty && len(kept) == 0 && len(allowed) <= portalPolicyAppendMax {
		for id := range allowed {
			kept = append(kept, id)
		}
		sort.Strings(kept)
	}
	return kept
}

func (c *Catalog) rememberProviderCacheKey(provider, cacheKey string) {
	st := &c.st.pricing
	st.mu.Lock()
	st.providerKeys[provider] = cacheKey
	st.mu.Unlock()
}

func (c *Catalog) providerCacheKey(provider string) (string, bool) {
	st := &c.st.pricing
	st.mu.Lock()
	defer st.mu.Unlock()
	key, ok := st.providerKeys[provider]
	return key, ok
}

func (c *Catalog) kiloPricingScope(ctx context.Context) string {
	baseURL := c.env(ctx, "KILOCODE_BASE_URL")
	if baseURL == "" {
		if p, ok := c.reg.Get(ctx, "kilocode"); ok {
			baseURL = p.Spec().BaseURL
		}
	}
	return strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
}

func (c *Catalog) novitaPricingScope(ctx context.Context) string {
	base := c.env(ctx, "NOVITA_BASE_URL")
	if base == "" {
		base = novitaDefaultBaseURL
	}
	return strings.TrimRight(base, "/")
}

func (c *Catalog) fetchKilocodePricing(ctx context.Context, timeout time.Duration, force bool) *PriceTable {
	cacheKey := c.kiloPricingScope(ctx)
	if cacheKey == "" {
		return newPriceTable()
	}
	if !force {
		if cached, ok := c.cachedCatalog(cacheKey); ok {
			return cached
		}
	}
	header := bearer(http.Header{"Accept": {"application/json"}, "User-Agent": {c.userAgent()}}, c.env(ctx, "KILOCODE_API_KEY"))
	payload, err := c.getJSONObject(ctx, cacheKey+"/models", header, "", timeout)
	if err != nil {
		return c.cacheCatalog(cacheKey, newPriceTable(), 0)
	}
	result := newPriceTable()
	for _, item := range objectItems(payload) {
		id, pricing := itemID(item), item.Get("pricing")
		if id == "" || !pricing.IsObject() {
			continue
		}
		rates := pricingRates(pricing, "prompt", "completion")
		if item.Get("isFree").Truthy() {
			rates.Prompt, rates.Completion, rates.CacheRead = "0", "0", ""
		}
		if _, ok := priceFloat(rates.Prompt, false); !ok {
			continue
		}
		if _, ok := priceFloat(rates.Completion, false); !ok {
			continue
		}
		result.put(id, PriceRow{PriceRates: rates})
	}
	return c.cacheCatalog(cacheKey, result, 0)
}

func (c *Catalog) fireworksPricingFromModelsDev(ctx context.Context, force bool) *PriceTable {
	if !force {
		if cached, ok := c.cachedCatalog(fireworksPricingKey); ok {
			return cached
		}
	}
	result := newPriceTable()
	if models := c.providerModels(ctx, "fireworks", false, nil); models != nil {
		for _, id := range models.order {
			entry := models.models[id]
			if entry == nil || entry.cost == nil {
				continue
			}
			input, hasInput := entry.cost["input"]
			output, hasOutput := entry.cost["output"]
			if !hasInput && !hasOutput {
				continue
			}
			row := PriceRow{PriceRates: PriceRates{Prompt: perToken(input), Completion: perToken(output)}}
			if read := entry.cost["cache_read"]; read != 0 {
				row.CacheRead = perToken(read)
			}
			result.put(id, row)
		}
	}
	return c.cacheCatalog(fireworksPricingKey, result, 0)
}

func (c *Catalog) fetchNovitaPricing(ctx context.Context, timeout time.Duration, force bool) *PriceTable {
	apiKey := c.env(ctx, "NOVITA_API_KEY")
	if apiKey == "" {
		return newPriceTable()
	}
	cacheKey := c.novitaPricingScope(ctx)
	if !force {
		if cached, ok := c.cachedCatalog(cacheKey); ok {
			return cached
		}
	}
	header := http.Header{"Authorization": {"Bearer " + apiKey}, "Accept": {"application/json"}, "User-Agent": {c.userAgent()}}
	payload, err := c.getJSONObject(ctx, cacheKey+"/models", header, "", timeout)
	if err != nil {
		return c.cacheCatalog(cacheKey, newPriceTable(), 0)
	}
	result := newPriceTable()
	for _, item := range objectItems(payload) {
		id := itemID(item)
		input, output := item.Get("input_token_price_per_m"), item.Get("output_token_price_per_m")
		absent := func(v wire.Value) bool { return !v.Exists() || v.Kind() == wire.KindNull }
		if id == "" || (absent(input) && absent(output)) {
			continue
		}
		in, _ := scalarFloat(input)
		out, _ := scalarFloat(output)
		result.put(id, PriceRow{PriceRates: PriceRates{Prompt: formatRate(in / 10_000 / 1_000_000), Completion: formatRate(out / 10_000 / 1_000_000)}})
	}
	return c.cacheCatalog(cacheKey, result, 0)
}

func (c *Catalog) fetchDeepInfraPricing(ctx context.Context, timeout time.Duration, force bool) *PriceTable {
	items, _ := c.deepInfraModelsByTag(ctx, "chat", timeout, force)
	result := newPriceTable()
	for _, item := range items {
		pricing := item.metadata.Get("pricing")
		if !pricing.IsObject() {
			continue
		}
		rates, any := PriceRates{}, false
		for _, field := range []struct {
			theirs string
			ours   *string
		}{{"input_tokens", &rates.Prompt}, {"output_tokens", &rates.Completion}, {"cache_read_tokens", &rates.CacheRead}} {
			raw := pricing.Get(field.theirs)
			if !raw.Exists() || raw.Kind() == wire.KindNull {
				continue
			}
			if value, ok := scalarFloat(raw); ok {
				*field.ours, any = perToken(value), true
			}
		}
		if any {
			result.put(item.id, PriceRow{PriceRates: rates})
		}
	}
	return result
}

func (c *Catalog) cachedPortalInferenceBaseURL(ctx context.Context) string {
	raw, ok := c.portal.State(ctx)
	if !ok {
		return ""
	}
	var state struct {
		InferenceBaseURL string `json:"inference_base_url"`
	}
	if json.Unmarshal(raw, &state) != nil {
		return ""
	}
	stored := auth.OptionalBaseURL(state.InferenceBaseURL)
	if stored == "" {
		return ""
	}
	trimmed := strings.TrimRight(strings.TrimSpace(stored), "/")
	if c.portal.InferenceBaseURL(ctx, stored) != trimmed {
		return ""
	}
	return strings.TrimSuffix(trimmed, "/v1")
}

func (c *Catalog) resolvePricingProvider(ctx context.Context, provider, baseURL string) string {
	normalized := c.normalizeProvider(ctx, provider)
	if !strings.HasPrefix(normalized, "custom:") {
		return normalized
	}
	if baseURL == "" {
		return ""
	}
	inferred, ok := c.InferProviderFromURL(ctx, baseURL)
	if !ok || inferred == "" {
		return ""
	}
	hostname := providers.URLHostname(baseURL)
	trusted := false
	if p, ok := c.reg.Get(ctx, inferred); ok {
		spec := p.Spec()
		trusted = slices.Contains(spec.Hostnames, hostname) || slices.Contains(spec.MetadataHostnames, hostname)
	}
	if !trusted {
		trusted = slices.ContainsFunc(urlProviders, func(row hostRow) bool {
			return row.provider == inferred && !strings.HasPrefix(row.part, ".") && providers.HostMatches(baseURL, row.part)
		})
	}
	if !trusted {
		return ""
	}
	return c.normalizeProvider(ctx, inferred)
}

func (c *Catalog) ResolvePricingProvider(ctx context.Context, provider, baseURL string) string {
	return c.resolvePricingProvider(c.scoped(ctx), provider, baseURL)
}

func (c *Catalog) staticPricingScope(ctx context.Context, normalized string) (string, bool) {
	switch normalized {
	case "openrouter":
		return openRouterPricingBase, true
	case "ai-gateway":
		return strings.TrimRight(aiGatewayBaseURL, "/"), true
	case "novita":
		return c.novitaPricingScope(ctx), true
	case "kilocode":
		return c.kiloPricingScope(ctx), true
	case "fireworks":
		return fireworksPricingKey, true
	}
	return "", false
}

func (c *Catalog) PricingCacheScope(ctx context.Context, provider, baseURL, currentProvider, currentBaseURL string) string {
	ctx = c.scoped(ctx)
	normalized := c.resolvePricingProvider(ctx, provider, baseURL)
	if scope, ok := c.staticPricingScope(ctx, normalized); ok && scope != "" {
		return scope
	}
	switch normalized {
	case "deepinfra":
		key, _ := c.deepInfraCatalogURL(ctx)
		return key
	case providers.ProviderPortal:
		if envBase := c.portalInferenceOverride(ctx); envBase != "" {
			return strings.TrimSuffix(strings.TrimRight(envBase, "/"), "/v1")
		}
		if c.normalizeProvider(ctx, currentProvider) == providers.ProviderPortal && currentBaseURL != "" {
			return strings.TrimSuffix(strings.TrimRight(currentBaseURL, "/"), "/v1")
		}
		if persisted := c.cachedPortalInferenceBaseURL(ctx); persisted != "" {
			return persisted
		}
		if key, ok := c.providerCacheKey(normalized); ok {
			return key
		}
		return portalInferenceOrigin
	}
	return ""
}

func (c *Catalog) cachedOnlyPricing(ctx context.Context, normalized string) *PriceTable {
	if normalized == "deepinfra" {
		if key, _ := c.deepInfraCatalogURL(ctx); c.deepInfraCached(key) {
			return c.fetchDeepInfraPricing(ctx, deepInfraTimeout, false)
		}
		return newPriceTable()
	}
	cacheKey, ok := c.providerCacheKey(normalized)
	if !ok && (normalized == "openrouter" || normalized == "ai-gateway" || normalized == "fireworks") {
		cacheKey, _ = c.staticPricingScope(ctx, normalized)
	}
	if cacheKey == "" {
		return newPriceTable()
	}
	if cached, ok := c.cachedCatalog(cacheKey); ok {
		return cached
	}
	return newPriceTable()
}

func (c *Catalog) pricingForProvider(ctx context.Context, provider, baseURL string, force, cachedOnly bool) *PriceTable {
	normalized := c.resolvePricingProvider(ctx, provider, baseURL)
	if cachedOnly {
		return c.cachedOnlyPricing(ctx, normalized)
	}
	switch normalized {
	case "openrouter":
		c.rememberProviderCacheKey("openrouter", openRouterPricingBase)
		return c.fetchModelsWithPricing(ctx, c.env(ctx, "OPENROUTER_API_KEY"), openRouterPricingBase, pricingFetchTimeout, force, false, 0)
	case "ai-gateway":
		c.rememberProviderCacheKey("ai-gateway", strings.TrimRight(aiGatewayBaseURL, "/"))
		return c.fetchAIGatewayPricing(ctx, pricingFetchTimeout, force)
	case "novita":
		c.rememberProviderCacheKey("novita", c.novitaPricingScope(ctx))
		return c.fetchNovitaPricing(ctx, pricingFetchTimeout, force)
	case "deepinfra":
		return c.fetchDeepInfraPricing(ctx, deepInfraTimeout, force)
	case "fireworks":
		c.rememberProviderCacheKey("fireworks", fireworksPricingKey)
		return c.fireworksPricingFromModelsDev(ctx, force)
	case "kilocode":
		c.rememberProviderCacheKey("kilocode", c.kiloPricingScope(ctx))
		return c.fetchKilocodePricing(ctx, pricingFetchTimeout, force)
	case providers.ProviderPortal:
		apiKey, base := c.portalPricingCredentials(ctx)
		if base == "" {
			return newPriceTable()
		}
		c.rememberProviderCacheKey(providers.ProviderPortal, strings.TrimRight(base, "/")+pricingAuthFingerprint(apiKey))
		return c.fetchPortalPricing(ctx, apiKey, base, force)
	}
	return newPriceTable()
}

func (c *Catalog) PricingForProvider(ctx context.Context, provider, baseURL string, force, cachedOnly bool) *PriceTable {
	return c.pricingForProvider(c.scoped(ctx), provider, baseURL, force, cachedOnly)
}
