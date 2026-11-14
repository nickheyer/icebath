package catalog

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	freeTierCacheTTL          = 180 * time.Second
	recommendedCacheTTL       = 600 * time.Second
	recommendedTimeout        = 5 * time.Second
	recommendedMirrorTimeout  = 2 * time.Second
	recommendedModelsMirror   = "https://web-assets.icebath.app/dynamic"
	recommendedModelsEndpoint = "/api/portal/recommended-models"
)

type recommendedMemo struct {
	data wire.Value
	at   time.Time
}

type freeTierMemo struct {
	free bool
	at   time.Time
	set  bool
}

func rateIsZero(text string) bool {
	value, ok := parseFloat(text)
	return ok && value == 0
}

func rowIsZeroPriced(row PriceRow) bool {
	return rateIsZero(row.Prompt) && rateIsZero(row.Completion)
}

func liveRateIsZero(pricing wire.Value, key string) bool {
	raw := pricing.Get(key)
	switch raw.Kind() {
	case wire.KindNone:
		return true
	case wire.KindNumber, wire.KindString:
		value, ok := parseFloat(raw.Display())
		return ok && value == 0
	case wire.KindBool:
		return raw.False()
	}
	return false
}

func livePricingIsFree(pricing wire.Value, promptKey, completionKey string) bool {
	return pricing.IsObject() && liveRateIsZero(pricing, promptKey) && liveRateIsZero(pricing, completionKey)
}

func ModelIsFree(modelID string, pricing *PriceTable) bool {
	row, ok := pricing.Row(modelID)
	return ok && (row.Subscription || rowIsZeroPriced(row))
}

func PartitionPortalModelsByTier(modelIDs []string, pricing *PriceTable, freeTier bool) (selectable, unavailable []string) {
	if !freeTier || pricing.Len() == 0 {
		return modelIDs, []string{}
	}
	selectable, unavailable = []string{}, []string{}
	for _, id := range modelIDs {
		if ModelIsFree(id, pricing) {
			selectable = append(selectable, id)
		}
	}
	for _, id := range modelIDs {
		if !slices.Contains(selectable, id) {
			unavailable = append(unavailable, id)
		}
	}
	return selectable, unavailable
}

func extractModelName(entry wire.Value) (string, bool) {
	name, ok := entry.Get("modelName").Str()
	if !ok || !entry.IsObject() {
		return "", false
	}
	name = strings.TrimSpace(name)
	return name, name != ""
}

func (c *Catalog) unionWithPortalRecommendations(ctx context.Context, tierKey string, curated []string, pricing *PriceTable, portalBaseURL string, force, synthesizeFree bool) ([]string, *PriceTable) {
	payload := c.fetchPortalRecommendedModels(ctx, portalBaseURL, recommendedTimeout, force)
	var portalIDs []string
	if block := payload.Get(tierKey); block.IsArray() {
		for _, entry := range block.Elems() {
			if name, ok := extractModelName(entry); ok {
				portalIDs = append(portalIDs, name)
			}
		}
	}
	augmented := pricing.clone()
	if len(portalIDs) == 0 {
		return slices.Clone(curated), augmented
	}
	if synthesizeFree {
		for _, id := range portalIDs {
			if _, ok := augmented.Row(id); !ok {
				augmented.put(id, PriceRow{PriceRates: PriceRates{Prompt: "0", Completion: "0"}})
			}
		}
	}
	out := slices.Clone(curated)
	for _, id := range portalIDs {
		if !slices.Contains(curated, id) {
			out = append(out, id)
		}
	}
	return out, augmented
}

func (c *Catalog) UnionWithPortalFreeRecommendations(ctx context.Context, curated []string, pricing *PriceTable, portalBaseURL string, force bool) ([]string, *PriceTable) {
	return c.unionWithPortalRecommendations(c.scoped(ctx), "freeRecommendedModels", curated, pricing, portalBaseURL, force, true)
}

func (c *Catalog) UnionWithPortalPaidRecommendations(ctx context.Context, curated []string, pricing *PriceTable, portalBaseURL string, force bool) ([]string, *PriceTable) {
	return c.unionWithPortalRecommendations(c.scoped(ctx), "paidRecommendedModels", curated, pricing, portalBaseURL, force, false)
}

func UnionWithPortalOnSaleModels(curated []string, pricing *PriceTable) []string {
	type sale struct {
		percent int
		id      string
	}
	var onSale []sale
	for _, id := range pricing.IDs() {
		row, _ := pricing.Row(id)
		if slices.Contains(curated, id) || row.NoTools || row.Generation {
			continue
		}
		percent, _, _, discounted := computeSaleDiscount(row.Prompt, row.Completion, row.Original)
		paid := false
		for _, rate := range []string{row.Prompt, row.Completion} {
			if value, ok := priceFloat(rate, true); ok && !math.IsInf(value, 0) {
				paid = true
			}
		}
		if discounted && row.Original != nil && paid {
			onSale = append(onSale, sale{percent, id})
		}
	}
	sort.SliceStable(onSale, func(i, j int) bool {
		if onSale[i].percent != onSale[j].percent {
			return onSale[i].percent > onSale[j].percent
		}
		return onSale[i].id < onSale[j].id
	})
	out := slices.Clone(curated)
	for _, row := range onSale {
		out = append(out, row.id)
	}
	return out
}

func (c *Catalog) CachedPortalFreeTier() (free, known bool) {
	st := &c.st.lists
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.freeTier.set || time.Since(st.freeTier.at) >= freeTierCacheTTL {
		return false, false
	}
	return st.freeTier.free, true
}

func (c *Catalog) checkPortalFreeTier(ctx context.Context, forceFresh, cachedOnly bool) bool {
	if !forceFresh {
		if free, known := c.CachedPortalFreeTier(); known {
			return free
		}
	}
	if cachedOnly {
		return false
	}
	now := time.Now()
	info := c.portal.AccountInfo(ctx, portal.AccountOptions{ForceFresh: forceFresh})
	result := info != nil && info.IsFreeTier()
	st := &c.st.lists
	st.mu.Lock()
	st.freeTier = freeTierMemo{free: result, at: now, set: true}
	st.mu.Unlock()
	return result
}

func (c *Catalog) CheckPortalFreeTier(ctx context.Context, forceFresh, cachedOnly bool) bool {
	return c.checkPortalFreeTier(c.scoped(ctx), forceFresh, cachedOnly)
}

func (c *Catalog) recommendedDiskPath() string { return home.PortalRecommendedCache.In(c.home) }

func (c *Catalog) readRecommendedDisk(base string) (wire.Value, time.Duration, bool) {
	raw, ok := readJSONObject(c.recommendedDiskPath())[base]
	if !ok {
		return wire.Value{}, 0, false
	}
	entry, err := wire.ParseValue(raw)
	if err != nil || !entry.IsObject() {
		return wire.Value{}, 0, false
	}
	data := entry.Get("data")
	if !data.IsObject() || len(data.Fields()) == 0 {
		return wire.Value{}, 0, false
	}
	age := time.Duration(math.MaxInt64)
	stamp := entry.Get("ts")
	switch stamp.Kind() {
	case wire.KindNone:
		age = time.Duration(unixSeconds(time.Now()) * float64(time.Second))
	case wire.KindNumber, wire.KindString:
		if seconds, ok := parseFloat(stamp.Display()); ok && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) {
			if delta := (unixSeconds(time.Now()) - seconds) * float64(time.Second); math.Abs(delta) < math.MaxInt64 {
				age = time.Duration(delta)
			}
		}
	}
	return data, age, true
}

func (c *Catalog) writeRecommendedDisk(ctx context.Context, base string, data wire.Value) {
	if !data.IsObject() || len(data.Fields()) == 0 {
		return
	}
	path := c.recommendedDiskPath()
	blob := readJSONObject(path)
	encoded, err := json.Marshal(struct {
		Data  json.RawMessage `json:"data"`
		Stamp json.RawMessage `json:"ts"`
	}{data.Raw(), json.RawMessage(strconv.FormatFloat(unixSeconds(time.Now()), 'f', -1, 64))})
	if err != nil {
		slog.Debug("portal recommended-models disk cache write failed", "error", err)
		return
	}
	blob[base] = encoded
	if err := fileutil.WriteJSON(ctx, path, blob, fileutil.JSONStyle{Indent: 2}); err != nil {
		slog.Debug("portal recommended-models disk cache write failed", "error", err)
	}
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Unmap().IsLoopback()
}

func (c *Catalog) fetchPortalRecommendedModels(ctx context.Context, portalBaseURL string, timeout time.Duration, force bool) wire.Value {
	base := portalBaseURL
	if base == "" {
		base = auth.DefaultPortalURL
	}
	base = strings.TrimRight(base, "/")
	now := time.Now()
	st := &c.st.lists
	st.mu.Lock()
	cached, hit := st.recommended[base]
	st.mu.Unlock()
	if !force && hit && now.Sub(cached.at) < recommendedCacheTTL {
		return cached.data
	}
	disk, age, onDisk := c.readRecommendedDisk(base)
	if !force && onDisk && age >= 0 && age < recommendedCacheTTL {
		st.mu.Lock()
		st.recommended[base] = recommendedMemo{data: disk, at: now.Add(-age)}
		st.mu.Unlock()
		return disk
	}
	type origin struct {
		url     string
		timeout time.Duration
	}
	var origins []origin
	if parts, err := url.Parse(base); err != nil || !isLoopbackHost(parts.Hostname()) {
		netloc := ""
		if err == nil {
			netloc = parts.Host
			if parts.User != nil {
				netloc = parts.User.String() + "@" + netloc
			}
		}
		origins = append(origins, origin{recommendedModelsMirror + "/" + netloc, min(timeout, recommendedMirrorTimeout)})
	}
	origins = append(origins, origin{base, timeout})
	data, fetched := wire.Value{}, false
	for _, source := range origins {
		payload, err := c.getJSON(ctx, source.url+recommendedModelsEndpoint, http.Header{"Accept": {"application/json"}}, "", source.timeout)
		if err != nil || !payload.IsObject() || len(payload.Fields()) == 0 {
			continue
		}
		c.writeRecommendedDisk(ctx, base, payload)
		data, fetched = payload, true
		break
	}
	if !fetched && onDisk {
		data = disk
	}
	st.mu.Lock()
	st.recommended[base] = recommendedMemo{data: data, at: now}
	st.mu.Unlock()
	return data
}

func (c *Catalog) FetchPortalRecommendedModels(ctx context.Context, portalBaseURL string, force bool) wire.Value {
	return c.fetchPortalRecommendedModels(c.scoped(ctx), portalBaseURL, recommendedTimeout, force)
}

func (c *Catalog) portalStateURL(ctx context.Context) string {
	raw, ok := c.portal.State(ctx)
	if !ok {
		return ""
	}
	var state struct {
		PortalBaseURL string `json:"portal_base_url"`
	}
	if json.Unmarshal(raw, &state) != nil {
		return ""
	}
	return state.PortalBaseURL
}

func (c *Catalog) resolvePortalURL(ctx context.Context) string {
	base := strings.TrimSpace(c.portalStateURL(ctx))
	if base == "" {
		base = auth.DefaultPortalURL
	}
	return strings.TrimRight(base, "/")
}

func (c *Catalog) PortalRecommendedAuxModel(ctx context.Context, vision bool) string {
	ctx = c.scoped(ctx)
	payload := c.fetchPortalRecommendedModels(ctx, c.resolvePortalURL(ctx), recommendedTimeout, false)
	if !payload.IsObject() || len(payload.Fields()) == 0 {
		return ""
	}
	kind := "Compaction"
	if vision {
		kind = "Vision"
	}
	tiers := []string{"paid", "free"}
	if c.checkPortalFreeTier(ctx, false, false) {
		tiers = []string{"free"}
	}
	for _, tier := range tiers {
		if name, ok := extractModelName(payload.Get(tier + "Recommended" + kind + "Model")); ok {
			return name
		}
	}
	return ""
}

func (c *Catalog) preferredSilentDefault(provider string) string {
	if labeled := c.defaultModelFromCache(provider); labeled != "" {
		return labeled
	}
	return PreferredSilentDefaultModel
}

func (c *Catalog) PreferredSilentDefaultModel(provider string) string {
	if provider == "" {
		provider = "openrouter"
	}
	return c.preferredSilentDefault(provider)
}

func bareLower(modelID string) string {
	raw := strings.ToLower(strings.TrimSpace(modelID))
	if _, rest, ok := strings.Cut(raw, "/"); ok {
		return rest
	}
	return raw
}

func isAnthropicFrontierTier(modelID string) bool {
	base, _, _ := strings.Cut(bareLower(modelID), ":")
	if !strings.HasPrefix(base, "claude-") {
		return false
	}
	return strings.Contains(base, "opus-") || strings.Contains(base, "fable-")
}

func (c *Catalog) pickSilentDefault(modelIDs []string, provider string) string {
	if len(modelIDs) == 0 {
		return ""
	}
	if preferred := c.preferredSilentDefault(provider); slices.Contains(modelIDs, preferred) {
		return preferred
	}
	for _, id := range modelIDs {
		if !isAnthropicFrontierTier(id) {
			return id
		}
	}
	return modelIDs[0]
}

func (c *Catalog) PickSilentDefaultModel(modelIDs []string, provider string) string {
	if provider == "" {
		provider = "openrouter"
	}
	return c.pickSilentDefault(modelIDs, provider)
}

func (c *Catalog) curatedPortalModelIDs(ctx context.Context) []string {
	if remote := c.curatedPortalModels(ctx); len(remote) > 0 {
		return remote
	}
	row, _ := c.builtinStaticRow(ctx, providers.ProviderPortal)
	return slices.Clone(row)
}

func (c *Catalog) CuratedPortalModelIDs(ctx context.Context) []string {
	return c.curatedPortalModelIDs(c.scoped(ctx))
}

func (c *Catalog) RecommendedPortalDefault(ctx context.Context) (model string, freeTier bool) {
	ctx = c.scoped(ctx)
	modelIDs := c.curatedPortalModelIDs(ctx)
	pricing := c.pricingForProvider(ctx, providers.ProviderPortal, "", false, false)
	freeTier = c.checkPortalFreeTier(ctx, true, false)
	portalURL := c.portalStateURL(ctx)
	allowed := c.portalPolicyAllowedIDs(ctx, false)
	tierKey := "paidRecommendedModels"
	if freeTier {
		tierKey = "freeRecommendedModels"
	}
	modelIDs, pricing = c.unionWithPortalRecommendations(ctx, tierKey, modelIDs, pricing, portalURL, false, freeTier)
	modelIDs = RestrictToPortalPolicy(modelIDs, allowed, true)
	if freeTier {
		modelIDs, _ = PartitionPortalModelsByTier(modelIDs, pricing, true)
		var owned []string
		for _, id := range modelIDs {
			if row, ok := pricing.Row(id); !ok || !row.Subscription {
				owned = append(owned, id)
			}
		}
		if len(owned) > 0 {
			modelIDs = owned
		}
	} else {
		model = c.plan.WindowPlanDefault(ctx, modelIDs, pricing)
	}
	if model == "" {
		model = c.pickSilentDefault(modelIDs, providers.ProviderPortal)
	}
	return model, freeTier
}

func (c *Catalog) defaultModelForProvider(ctx context.Context, provider string) string {
	models := c.staticCatalog(ctx, provider)
	if slices.Contains(silentDefaultProviders, provider) {
		preferred := c.preferredSilentDefault(provider)
		if preferred != "" && (slices.Contains(models, preferred) || len(models) == 0) {
			return preferred
		}
	}
	if len(models) > 0 {
		return models[0]
	}
	return ""
}

func (c *Catalog) DefaultModelForProvider(ctx context.Context, provider string) string {
	return c.defaultModelForProvider(c.scoped(ctx), provider)
}
