package provider

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Clock behind every catalog TTL in this slice
var modelsClock = time.Now

// Transport seam so catalog tests never open a socket
var modelsTransport http.RoundTripper

// Non 2xx catalog response
type modelsHTTPError struct {
	URL    string
	Status int
	Body   []byte
}

func (e *modelsHTTPError) Error() string {
	return fmt.Sprintf("HTTP Error %d for %s", e.Status, e.URL)
}

// GET a catalog URL and parse its JSON body, credentials dropped on a cross origin redirect
func modelsGetJSON(ctx context.Context, rawURL string, timeout time.Duration, headers map[string]string, tlsCfg *tls.Config) (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	var transport http.RoundTripper = &http.Transport{
		Proxy:               probeProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: timeout}).DialContext,
		TLSClientConfig:     tlsCfg,
		TLSHandshakeTimeout: timeout,
		DisableCompression:  true,
	}
	if modelsTransport != nil {
		transport = modelsTransport
	}
	client := &http.Client{
		Transport:     transport,
		Timeout:       timeout,
		CheckRedirect: credentialRedirectPolicy(rawURL),
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &modelsHTTPError{URL: rawURL, Status: resp.StatusCode, Body: body}
	}
	if strings.EqualFold(req.Header.Get("Accept-Encoding"), "gzip") && strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(strings.NewReader(string(body)))
		if err != nil {
			return nil, err
		}
		body, err = io.ReadAll(zr)
		if err != nil {
			return nil, err
		}
	}
	var out any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// True when the error is a connect or read timeout rather than a refusal or protocol failure
func modelsErrIsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

// JSON object cache file, nil when missing, unreadable or not an object
func modelsReadJSONCache(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	raw = []byte(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))
	var out any
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	m, _ := asMap(out)
	return m
}

// Atomically persists a cache file with the dump options, creating parents unless they lie in a dead profile
func modelsWriteJSONCache(ctx context.Context, path string, data any, opts ...pyjson.Option) error {
	if _, err := home.Mkdir(ctx, filepath.Dir(path)); err != nil {
		return err
	}
	return config.AtomicJSONWriteWith(ctx, path, data, opts...)
}

// Primary verbatim then the secondary entries whose key is new
func mergeUnique(primary, secondary []string, key func(string) string) []string {
	if key == nil {
		key = strings.ToLower
	}
	merged := append([]string{}, primary...)
	seen := map[string]bool{}
	for _, m := range primary {
		seen[key(m)] = true
	}
	for _, m := range secondary {
		k := key(m)
		if !seen[k] {
			seen[k] = true
			merged = append(merged, m)
		}
	}
	return merged
}

// ---- OpenRouter curated list, process slot plus disk mirror ----

var openrouterCatalogSlot = newProfileSlot[[]CatalogModel]()

// Same TTL as the catalog manifest the list is filtered from
func openrouterCatalogDiskTTL(ctx context.Context) time.Duration { return CatalogRefreshInterval(ctx) }

func openrouterCatalogDiskPath(ctx context.Context) string {
	return filepath.Join(home.CacheDir(home.Dir(ctx)), "openrouter_curated_catalog.json")
}

// Fresh curated catalog from disk, nil when missing, corrupt, expired or empty
func readOpenrouterCatalogDisk(ctx context.Context, allowStale bool) []CatalogModel {
	obj := modelsReadJSONCache(openrouterCatalogDiskPath(ctx))
	if obj == nil {
		return nil
	}
	if !allowStale {
		fetchedAt, ok := floatOf(obj["fetched_at"])
		if !ok {
			return nil
		}
		if float64(modelsClock().Unix())-fetchedAt > openrouterCatalogDiskTTL(ctx).Seconds() {
			return nil
		}
	}
	items, ok := asList(obj["curated"])
	if !ok {
		return nil
	}
	var out []CatalogModel
	for _, it := range items {
		pair, ok := asList(it)
		if !ok || len(pair) != 2 {
			continue
		}
		out = append(out, CatalogModel{ID: stringOf(pair[0]), Description: stringOf(pair[1])})
	}
	return out
}

func writeOpenrouterCatalogDisk(ctx context.Context, curated []CatalogModel) {
	rows := make([][]string, 0, len(curated))
	for _, c := range curated {
		rows = append(rows, []string{c.ID, c.Description})
	}
	if err := modelsWriteJSONCache(ctx, openrouterCatalogDiskPath(ctx), map[string]any{"fetched_at": modelsClock().Unix(), "curated": rows}, pyjson.Indent(2)); err != nil {
		modelsLog.Debug("openrouter curated catalog disk write failed", "error", err)
	}
}

// ---- Nous Portal tiers ----

// PartitionNousModelsByTier splits Nous models into selectable and unavailable rows for a free tier account
func PartitionNousModelsByTier(modelIDs []string, pricing PricingTable, freeTier bool) ([]string, []string) {
	if !freeTier || len(pricing) == 0 {
		return cloneStrings(modelIDs), []string{}
	}
	anyPricing := PricingTableAsAny(pricing)
	selectable, unavailable := []string{}, []string{}
	for _, mid := range modelIDs {
		if IsModelFree(mid, anyPricing) {
			selectable = append(selectable, mid)
		} else {
			unavailable = append(unavailable, mid)
		}
	}
	return selectable, unavailable
}

func clonePricing(pricing PricingTable) PricingTable {
	out := make(PricingTable, len(pricing))
	for k, v := range pricing {
		out[k] = v
	}
	return out
}

// Appends the Portal's tier recommendations missing from the curated ids
func unionWithPortalRecommendations(ctx context.Context, tierKey string, curated []string, pricing PricingTable, portalBaseURL string, forceRefresh, synthesizeFreePricing bool) ([]string, PricingTable) {
	payload := fetchNousRecommendedModelsFn(ctx, portalBaseURL, forceRefresh)
	entries, _ := asList(payload[tierKey])
	var portalIDs []string
	for _, entry := range entries {
		if name := extractModelName(entry); name != "" {
			portalIDs = append(portalIDs, name)
		}
	}
	if len(portalIDs) == 0 {
		return cloneStrings(curated), clonePricing(pricing)
	}
	augmented := clonePricing(pricing)
	if synthesizeFreePricing {
		for _, mid := range portalIDs {
			if _, ok := augmented[mid]; !ok {
				augmented[mid] = map[string]any{"prompt": "0", "completion": "0"}
			}
		}
	}
	seen := map[string]bool{}
	for _, mid := range curated {
		seen[mid] = true
	}
	out := cloneStrings(curated)
	for _, mid := range portalIDs {
		if !seen[mid] {
			out = append(out, mid)
		}
	}
	return out, augmented
}

// UnionWithPortalFreeRecommendations adds the Portal's free picks with a synthetic zero rate card
func UnionWithPortalFreeRecommendations(ctx context.Context, curated []string, pricing PricingTable, portalBaseURL string, forceRefresh bool) ([]string, PricingTable) {
	return unionWithPortalRecommendations(ctx, "freeRecommendedModels", curated, pricing, portalBaseURL, forceRefresh, true)
}

// UnionWithPortalPaidRecommendations adds the Portal's paid picks leaving pricing untouched
func UnionWithPortalPaidRecommendations(ctx context.Context, curated []string, pricing PricingTable, portalBaseURL string, forceRefresh bool) ([]string, PricingTable) {
	return unionWithPortalRecommendations(ctx, "paidRecommendedModels", curated, pricing, portalBaseURL, forceRefresh, false)
}

// UnionWithNousOnSaleModels appends every discounted paid chat model the gateway lists, deepest discount first
func UnionWithNousOnSaleModels(curated []string, pricing PricingTable) []string {
	seen := map[string]bool{}
	for _, mid := range curated {
		seen[mid] = true
	}
	type sale struct {
		negPercent int
		id         string
	}
	var onSale []sale
	for mid, entry := range pricing {
		if seen[mid] || entry == nil || entry["tools"] == false || truthy(entry["generation"]) {
			continue
		}
		disc, ok := ComputeSaleDiscount(stringOf(entry["prompt"]), stringOf(entry["completion"]), entry["original"])
		paid := false
		for _, key := range []string{"prompt", "completion"} {
			if rate, ok := priceFloat(entry[key], true); ok && !math.IsInf(rate, 0) && !math.IsNaN(rate) {
				paid = true
			}
		}
		if _, isMap := asMap(entry["original"]); ok && isMap && paid {
			onSale = append(onSale, sale{-disc.Percent, mid})
		}
	}
	sort.Slice(onSale, func(i, j int) bool {
		if onSale[i].negPercent != onSale[j].negPercent {
			return onSale[i].negPercent < onSale[j].negPercent
		}
		return onSale[i].id < onSale[j].id
	})
	out := cloneStrings(curated)
	for _, s := range onSale {
		out = append(out, s.id)
	}
	return out
}

// ---- free tier detection cache, per profile ----

const freeTierCacheTTL = 180 * time.Second

type freeTierEntry struct {
	result bool
	at     time.Time
}

var (
	freeTierCacheMu sync.Mutex
	freeTierCache   = map[string]freeTierEntry{}
)

// GetCachedNousFreeTier returns this profile's live cached entitlement, false when unknown or expired
func GetCachedNousFreeTier(ctx context.Context) (bool, bool) {
	freeTierCacheMu.Lock()
	defer freeTierCacheMu.Unlock()
	cached, ok := freeTierCache[pricingProfileKey(ctx)]
	if !ok || modelsClock().Sub(cached.at) >= freeTierCacheTTL {
		return false, false
	}
	return cached.result, true
}

// CheckNousFreeTier is true only when the Portal user is known to be free tier, cached for three minutes
func CheckNousFreeTier(ctx context.Context, forceFresh, cachedOnly bool) bool {
	now := modelsClock()
	profileKey := pricingProfileKey(ctx)
	if !forceFresh {
		if cached, ok := GetCachedNousFreeTier(ctx); ok {
			return cached
		}
	}
	if cachedOnly {
		return false
	}
	result, err := nousPortalAccountIsFreeTierFn(ctx, forceFresh)
	if err != nil {
		result = false
	}
	freeTierCacheMu.Lock()
	freeTierCache[profileKey] = freeTierEntry{result: result, at: now}
	freeTierCacheMu.Unlock()
	return result
}

// ---- Nous Portal recommended models ----

// NousRecommendedModelsMirror is the R2 mirror of the Portal's public recommended models document
const NousRecommendedModelsMirror = "https://web-assets.nousresearch.com/dynamic"

const (
	nousMirrorTimeout       = 2 * time.Second
	nousRecommendedCacheTTL = 600 * time.Second
)

type nousRecommendedEntry struct {
	data map[string]any
	at   time.Time
}

type nousRecommendedKey struct{ profile, base string }

var (
	nousRecommendedMu    sync.Mutex
	nousRecommendedCache = map[nousRecommendedKey]nousRecommendedEntry{}
)

func nousRecommendedDiskPath(ctx context.Context) string {
	return filepath.Join(home.CacheDir(home.Dir(ctx)), "nous_recommended_cache.json")
}

// Last good payload for a Portal base plus its age
func readNousRecommendedDisk(ctx context.Context, base string) (map[string]any, time.Duration, bool) {
	blob := modelsReadJSONCache(nousRecommendedDiskPath(ctx))
	entry, _ := asMap(blob[base])
	data, _ := asMap(entry["data"])
	if len(data) == 0 {
		return nil, 0, false
	}
	age := time.Duration(math.Inf(1))
	if ts, ok := floatOf(entry["ts"]); ok {
		age = time.Duration((float64(modelsClock().UnixNano())/1e9 - ts) * float64(time.Second))
	}
	return data, age, true
}

// Merges a Portal payload into the per base disk map atomically
func writeNousRecommendedDisk(ctx context.Context, base string, data map[string]any) {
	if len(data) == 0 {
		return
	}
	path := nousRecommendedDiskPath(ctx)
	blob := modelsReadJSONCache(path)
	if blob == nil {
		blob = map[string]any{}
	}
	blob[base] = map[string]any{"data": data, "ts": float64(modelsClock().UnixNano()) / 1e9}
	if err := modelsWriteJSONCache(ctx, path, blob, pyjson.Indent(2)); err != nil {
		modelsLog.Debug("nous recommended-models disk cache write failed", "error", err)
	}
}

// FetchNousRecommendedModels reads the Portal's public recommended models from its mirror then the Portal, cached ten minutes
func FetchNousRecommendedModels(ctx context.Context, portalBaseURL string, timeout time.Duration, forceRefresh bool) map[string]any {
	base := strings.TrimRight(portalBaseURL, "/")
	if base == "" {
		base = "https://portal.nousresearch.com"
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	now := modelsClock()
	key := nousRecommendedKey{pricingProfileKey(ctx), base}
	nousRecommendedMu.Lock()
	cached, ok := nousRecommendedCache[key]
	nousRecommendedMu.Unlock()
	if !forceRefresh && ok && now.Sub(cached.at) < nousRecommendedCacheTTL {
		return cached.data
	}
	diskData, diskAge, diskOK := readNousRecommendedDisk(ctx, base)
	if !forceRefresh && diskOK && diskAge >= 0 && diskAge < nousRecommendedCacheTTL {
		nousRecommendedMu.Lock()
		nousRecommendedCache[key] = nousRecommendedEntry{data: diskData, at: now.Add(-diskAge)}
		nousRecommendedMu.Unlock()
		return diskData
	}
	type origin struct {
		url     string
		timeout time.Duration
	}
	var origins []origin
	if parsed, err := url.Parse(base); err == nil && !IsLoopbackHost(parsed.Hostname()) {
		mirrorTimeout := timeout
		if nousMirrorTimeout < mirrorTimeout {
			mirrorTimeout = nousMirrorTimeout
		}
		origins = append(origins, origin{NousRecommendedModelsMirror + "/" + parsed.Host, mirrorTimeout})
	}
	origins = append(origins, origin{base, timeout})
	var data map[string]any
	for _, o := range origins {
		payload, err := modelsGetJSON(ctx, o.url+"/api/nous/recommended-models", o.timeout, map[string]string{"Accept": "application/json", "Accept-Encoding": "gzip"}, nil)
		if err != nil {
			continue
		}
		if m, ok := asMap(payload); ok && len(m) > 0 {
			data = m
			writeNousRecommendedDisk(ctx, base, data)
			break
		}
	}
	if data == nil {
		data = map[string]any{}
		if diskOK {
			data = diskData
		}
	}
	nousRecommendedMu.Lock()
	nousRecommendedCache[key] = nousRecommendedEntry{data: data, at: now}
	nousRecommendedMu.Unlock()
	return data
}

// Seam form the catalog refresh command drives
func fetchNousRecommendedModels(ctx context.Context, portalURL string, forceRefresh bool) map[string]any {
	return FetchNousRecommendedModels(ctx, portalURL, 5*time.Second, forceRefresh)
}

// Portal base URL the profile is authed against, the default Portal otherwise
func resolveNousPortalURL(ctx context.Context) string {
	store := NewStore(home.Dir(ctx))
	data, err := store.Load()
	if err != nil {
		return "https://portal.nousresearch.com"
	}
	state := store.ProviderState(data, "nous")
	portal := strings.TrimSpace(stringOf(state["portal_base_url"]))
	if portal == "" {
		portal = DefaultNousPortalURL
	}
	return strings.TrimRight(portal, "/")
}

// ResolveNousPortalURL is the exported form of the Portal base URL lookup
func ResolveNousPortalURL(ctx context.Context) string { return resolveNousPortalURL(ctx) }

// The modelName field of a recommended model entry, empty otherwise
func extractModelName(entry any) string {
	m, ok := asMap(entry)
	if !ok {
		return ""
	}
	name, ok := m["modelName"].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(name)
}

// GetNousRecommendedAuxModel is the Portal's compaction or vision pick for the account's tier
func GetNousRecommendedAuxModel(ctx context.Context, vision bool, freeTier *bool, portalBaseURL string, forceRefresh bool) string {
	base := portalBaseURL
	if base == "" {
		base = resolveNousPortalURL(ctx)
	}
	payload := fetchNousRecommendedModelsFn(ctx, base, forceRefresh)
	if len(payload) == 0 {
		return ""
	}
	free := false
	if freeTier == nil {
		free = checkNousFreeTierFn(ctx, false, false)
	} else {
		free = *freeTier
	}
	kind := "Compaction"
	if vision {
		kind = "Vision"
	}
	tiers := []string{"paid", "free"}
	if free {
		tiers = []string{"free"}
	}
	for _, t := range tiers {
		if name := extractModelName(payload[t+"Recommended"+kind+"Model"]); name != "" {
			return name
		}
	}
	return ""
}

// ---- OpenRouter and AI Gateway live catalogs ----

func openrouterModelIsFree(pricing any) bool {
	return zeroPriced(pricing, [2]string{"prompt", "completion"}, "0")
}

// True when supported_parameters advertises tools, permissive when the field is absent or malformed
func openrouterModelSupportsTools(item any) bool {
	m, ok := asMap(item)
	if !ok {
		return true
	}
	params, ok := asList(m["supported_parameters"])
	if !ok {
		return true
	}
	for _, p := range params {
		if p == "tools" {
			return true
		}
	}
	return false
}

// ClampReasoningEffortToSupported keeps a supported level verbatim else the nearest weaker supported level
func ClampReasoningEffortToSupported(effort string, supported []string) string {
	return clampEffort(effort, supported, nil)
}

// GET an OpenAI style models listing as the raw data array and an id index
func fetchLiveCatalogIndex(ctx context.Context, rawURL string, timeout time.Duration) ([]any, map[string]map[string]any, bool) {
	payload, err := modelsGetJSON(ctx, rawURL, timeout, map[string]string{"Accept": "application/json"}, nil)
	if err != nil {
		return nil, nil, false
	}
	doc, ok := asMap(payload)
	if !ok {
		return nil, nil, false
	}
	items, ok := asList(doc["data"])
	if !ok {
		return nil, nil, false
	}
	byID := map[string]map[string]any{}
	for _, it := range items {
		m, ok := asMap(it)
		if !ok {
			continue
		}
		if mid := strings.TrimSpace(stringOf(m["id"])); mid != "" {
			byID[mid] = m
		}
	}
	return items, byID, true
}

// FetchModelsOptions tune one picker list fetch
type FetchModelsOptions struct {
	Timeout      time.Duration
	ForceRefresh bool
	CacheOnly    bool
}

func cloneCatalogModels(rows []CatalogModel) []CatalogModel {
	return append([]CatalogModel{}, rows...)
}

// FetchOpenRouterModels returns the curated OpenRouter picker list refreshed from the live catalog when possible
func FetchOpenRouterModels(ctx context.Context, opts FetchModelsOptions) []CatalogModel {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	cached, haveCached := openrouterCatalogSlot.get(ctx)
	if haveCached && !opts.ForceRefresh {
		return cloneCatalogModels(cached)
	}
	if !opts.ForceRefresh {
		if disk := readOpenrouterCatalogDisk(ctx, opts.CacheOnly); len(disk) > 0 {
			if !opts.CacheOnly {
				openrouterCatalogSlot.set(ctx, disk)
			}
			return cloneCatalogModels(disk)
		}
	}
	if opts.CacheOnly {
		return cloneCatalogModels(OpenRouterModels)
	}
	fallback := curatedOpenRouterModelsFn(ctx)
	if len(fallback) == 0 {
		fallback = cloneCatalogModels(OpenRouterModels)
	}
	liveItems, liveByID, ok := fetchLiveCatalogIndexFn(ctx, openRouterCatalogURL, timeout)
	if !ok {
		if haveCached {
			return cloneCatalogModels(cached)
		}
		return cloneCatalogModels(fallback)
	}
	seedReasoningCaps(ctx, openRouterCatalogURL, liveItems)
	var curated []CatalogModel
	silentDefault := preferredSilentDefaultFn(ctx, "openrouter")
	for _, row := range fallback {
		liveItem, ok := liveByID[row.ID]
		if !ok || !openrouterModelSupportsTools(liveItem) {
			continue
		}
		desc := ""
		if row.ID == silentDefault {
			desc = "default"
		} else if openrouterModelIsFree(liveItem["pricing"]) {
			desc = "free"
		}
		curated = append(curated, CatalogModel{ID: row.ID, Description: desc})
	}
	if len(curated) == 0 {
		if haveCached {
			return cloneCatalogModels(cached)
		}
		return cloneCatalogModels(fallback)
	}
	if curated[0].Description == "" {
		curated[0].Description = "recommended"
	}
	openrouterCatalogSlot.set(ctx, curated)
	writeOpenrouterCatalogDisk(ctx, curated)
	return cloneCatalogModels(curated)
}

// Seam form the catalog refresh command drives
func fetchOpenRouterModels(ctx context.Context, forceRefresh bool) []CatalogModel {
	return FetchOpenRouterModels(ctx, FetchModelsOptions{ForceRefresh: forceRefresh})
}

// OpenRouterModelIDs returns just the OpenRouter model id strings
func OpenRouterModelIDs(ctx context.Context, forceRefresh bool) []string {
	return CatalogModelIDs(FetchOpenRouterModels(ctx, FetchModelsOptions{ForceRefresh: forceRefresh}))
}

// GetCuratedNousModelIDs is the remote manifest's Nous row else the in repo snapshot
func GetCuratedNousModelIDs(ctx context.Context) []string {
	if remote := CuratedNousModels(ctx); len(remote) > 0 {
		return cloneStrings(remote)
	}
	return staticCatalogModels("nous")
}

func aiGatewayModelIsFree(pricing any) bool {
	return zeroPriced(pricing, [2]string{"input", "output"}, "0")
}

var (
	aiGatewayCatalogMu    sync.Mutex
	aiGatewayCatalogCache []CatalogModel
)

// FetchAIGatewayModels returns the curated AI Gateway picker list refreshed from the live catalog when possible
func FetchAIGatewayModels(ctx context.Context, timeout time.Duration, forceRefresh bool) []CatalogModel {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	aiGatewayCatalogMu.Lock()
	cached := aiGatewayCatalogCache
	aiGatewayCatalogMu.Unlock()
	if cached != nil && !forceRefresh {
		return cloneCatalogModels(cached)
	}
	fallback := cloneCatalogModels(VercelAIGatewayModels)
	_, liveByID, ok := fetchLiveCatalogIndex(ctx, strings.TrimRight(AIGatewayBaseURL, "/")+"/models", timeout)
	if !ok {
		if cached != nil {
			return cloneCatalogModels(cached)
		}
		return fallback
	}
	var curated []CatalogModel
	for _, row := range fallback {
		item, ok := liveByID[row.ID]
		if !ok {
			continue
		}
		desc := ""
		if aiGatewayModelIsFree(item["pricing"]) {
			desc = "free"
		}
		curated = append(curated, CatalogModel{ID: row.ID, Description: desc})
	}
	if len(curated) == 0 {
		if cached != nil {
			return cloneCatalogModels(cached)
		}
		return fallback
	}
	freeMoonshot := ""
	for _, mid := range sortedStringKeys(liveByID) {
		if strings.HasPrefix(mid, "moonshotai/") && aiGatewayModelIsFree(liveByID[mid]["pricing"]) {
			freeMoonshot = mid
			break
		}
	}
	if freeMoonshot != "" {
		promoted := []CatalogModel{{ID: freeMoonshot, Description: "recommended"}}
		for _, row := range curated {
			if row.ID != freeMoonshot {
				promoted = append(promoted, row)
			}
		}
		curated = promoted
	} else {
		curated[0].Description = "recommended"
	}
	aiGatewayCatalogMu.Lock()
	aiGatewayCatalogCache = curated
	aiGatewayCatalogMu.Unlock()
	return cloneCatalogModels(curated)
}

// AIGatewayModelIDs returns just the AI Gateway model id strings
func AIGatewayModelIDs(ctx context.Context, forceRefresh bool) []string {
	return CatalogModelIDs(FetchAIGatewayModels(ctx, 0, forceRefresh))
}

// Live AI Gateway language models with tool use, false without a key or on failure
func fetchAIGatewayModelsLive(ctx context.Context, timeout time.Duration) ([]string, bool) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	apiKey := strings.TrimSpace(GetSecretStr(ctx, "AI_GATEWAY_API_KEY", ""))
	if apiKey == "" {
		return nil, false
	}
	baseURL := strings.TrimSpace(GetSecretStr(ctx, "AI_GATEWAY_BASE_URL", ""))
	if baseURL == "" {
		baseURL = AIGatewayBaseURL
	}
	headers := map[string]string{"Authorization": "Bearer " + apiKey, "User-Agent": userAgent("icebath-cli")}
	payload, err := modelsGetJSON(ctx, strings.TrimRight(baseURL, "/")+"/models", timeout, headers, nil)
	if err != nil {
		return nil, false
	}
	doc, ok := asMap(payload)
	if !ok {
		return nil, false
	}
	items, _ := asList(doc["data"])
	out := []string{}
	for _, it := range items {
		m, ok := asMap(it)
		if !ok || stringOf(m["id"]) == "" || m["type"] != "language" {
			continue
		}
		tags, _ := asList(m["tags"])
		for _, t := range tags {
			if t == "tool-use" {
				out = append(out, stringOf(m["id"]))
				break
			}
		}
	}
	return out, true
}

// ---- DeepInfra tagged catalog ----

var deepinfraExcludeRe = regexp.MustCompile(`(?i)(embed|rerank|whisper|stable-diffusion|flux|sdxl|tts|bark|speech|image-gen|clip|vit-|dpt-)`)

var deepinfraSurfaceTags = map[string]bool{"chat": true, "embed": true, "image-gen": true, "tts": true, "stt": true, "video-gen": true}

const (
	// DeepinfraDefaultBaseURL is the OpenAI compatible DeepInfra root
	DeepinfraDefaultBaseURL = "https://api.deepinfra.com/v1/openai"
	deepinfraModelsQuery    = "filter=true&sort_by=hermes"
	deepinfraCatalogNegTTL  = 60 * time.Second
)

var (
	deepinfraCatalogMu       sync.Mutex
	deepinfraCatalogCache    = map[string][]map[string]any{}
	deepinfraCatalogNegCache = map[string]time.Time{}
)

// Pool environment for one profile home so credential reads follow the profile .env and scope
func modelsPoolEnv(ctx context.Context) PoolEnv {
	if ctx == nil {
		ctx = context.Background()
	}
	h := home.Dir(ctx)
	return PoolEnv{Store: NewStore(h), Secrets: config.NewSecrets(h), Scope: SecretScopeFrom(ctx), Config: ConfigLoader(h), Log: modelsLog, Ctx: ctx}
}

// Profile scoped .env then environ read of a DeepInfra setting
func deepinfraEnv(ctx context.Context, key string) string {
	return strings.TrimSpace(GetEnvPreferDotenv(modelsPoolEnv(ctx), key))
}

// Cache key carrying the api key fingerprint plus the full catalog URL
func deepinfraCatalogURLIn(ctx context.Context) (string, string) {
	base := deepinfraEnv(ctx, "DEEPINFRA_BASE_URL")
	if base == "" {
		base = DeepinfraDefaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	fp := secretFingerprintOf(deepinfraEnv(ctx, "DEEPINFRA_API_KEY"))
	if fp == "" {
		fp = "anon"
	}
	return base + "#" + fp, base + "/models?" + deepinfraModelsQuery
}

// Seam form the pricing scope reads, resolved against the process home
func deepinfraCatalogURL() (string, string) { return deepinfraCatalogURLIn(context.Background()) }

// True when the tagged catalog for a cache key is resident in this process
func deepinfraCatalogCached(cacheKey string) bool {
	deepinfraCatalogMu.Lock()
	defer deepinfraCatalogMu.Unlock()
	_, ok := deepinfraCatalogCache[cacheKey]
	return ok
}

// Raw DeepInfra catalog list cached per base URL and key, false on failure
func fetchDeepinfraCatalog(ctx context.Context, timeout time.Duration, forceRefresh bool) ([]map[string]any, bool) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	cacheKey, rawURL := deepinfraCatalogURLIn(ctx)
	if !forceRefresh {
		deepinfraCatalogMu.Lock()
		cached, ok := deepinfraCatalogCache[cacheKey]
		lastFail, failed := deepinfraCatalogNegCache[cacheKey]
		deepinfraCatalogMu.Unlock()
		if ok {
			return cached, true
		}
		if failed && modelsClock().Sub(lastFail) < deepinfraCatalogNegTTL {
			return nil, false
		}
	}
	headers := map[string]string{"User-Agent": userAgent("icebath-cli")}
	if apiKey := deepinfraEnv(ctx, "DEEPINFRA_API_KEY"); apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}
	payload, err := modelsGetJSON(ctx, rawURL, timeout, headers, nil)
	doc, _ := asMap(payload)
	items, ok := asList(doc["data"])
	if err != nil || !ok {
		deepinfraCatalogMu.Lock()
		deepinfraCatalogNegCache[cacheKey] = modelsClock()
		deepinfraCatalogMu.Unlock()
		return nil, false
	}
	data := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if m, ok := asMap(it); ok {
			data = append(data, m)
		}
	}
	deepinfraCatalogMu.Lock()
	deepinfraCatalogCache[cacheKey] = data
	delete(deepinfraCatalogNegCache, cacheKey)
	deepinfraCatalogMu.Unlock()
	return data, true
}

// DeepInfra id and metadata items whose surface tags include the tag, untagged ids fall to the regex
func fetchDeepinfraModelsByTagIn(ctx context.Context, tag string, timeout time.Duration, forceRefresh bool) ([]map[string]any, bool) {
	data, ok := fetchDeepinfraCatalog(ctx, timeout, forceRefresh)
	if !ok {
		return nil, false
	}
	matched := []map[string]any{}
	for _, item := range data {
		mid := stringOf(item["id"])
		rawMetadata, present := item["metadata"]
		if mid == "" || !present || rawMetadata == nil {
			continue
		}
		metadata, _ := asMap(rawMetadata)
		if metadata == nil {
			metadata = map[string]any{}
		}
		tags, _ := asList(metadata["tags"])
		surface, hit := false, false
		for _, t := range tags {
			if s, ok := t.(string); ok && deepinfraSurfaceTags[s] {
				surface = true
			}
			if t == tag {
				hit = true
			}
		}
		if !surface {
			hit = tag == "chat" && !deepinfraExcludeRe.MatchString(mid)
		}
		if hit {
			matched = append(matched, map[string]any{"id": mid, "metadata": metadata})
		}
	}
	return matched, true
}

// Seam form the pricing fetcher drives, nil on network failure
func fetchDeepinfraModelsByTag(tag string, timeout time.Duration, forceRefresh bool) []map[string]any {
	items, ok := fetchDeepinfraModelsByTagIn(context.Background(), tag, timeout, forceRefresh)
	if !ok {
		return nil
	}
	return items
}

// DeepInfra chat model ids, false on failure or when no chat id exists
func fetchDeepinfraModels(ctx context.Context, timeout time.Duration, forceRefresh bool) ([]string, bool) {
	items, ok := fetchDeepinfraModelsByTagIn(ctx, "chat", timeout, forceRefresh)
	if !ok {
		return nil, false
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, stringOf(it["id"]))
	}
	return ids, len(ids) > 0
}

// DeepinfraModelIDs returns the DeepInfra ids carrying a surface tag, empty on failure
func DeepinfraModelIDs(ctx context.Context, tag string, forceRefresh bool) []string {
	items, ok := fetchDeepinfraModelsByTagIn(ctx, tag, 5*time.Second, forceRefresh)
	if !ok {
		return []string{}
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, stringOf(it["id"]))
	}
	return ids
}

// DeepinfraBaseURL is the config section base_url, then the env override, then the default
func DeepinfraBaseURL(ctx context.Context, section map[string]any) string {
	value := strings.TrimSpace(stringOf(section["base_url"]))
	if value == "" {
		value = deepinfraEnv(ctx, "DEEPINFRA_BASE_URL")
	}
	if value == "" {
		value = DeepinfraDefaultBaseURL
	}
	return strings.TrimRight(strings.TrimSpace(value), "/")
}

// ResetModelsFetchCaches clears every process resident picker cache of this slice
func ResetModelsFetchCaches() {
	openrouterCatalogSlot.reset()
	aiGatewayCatalogMu.Lock()
	aiGatewayCatalogCache = nil
	aiGatewayCatalogMu.Unlock()
	nousRecommendedMu.Lock()
	nousRecommendedCache = map[nousRecommendedKey]nousRecommendedEntry{}
	nousRecommendedMu.Unlock()
	freeTierCacheMu.Lock()
	freeTierCache = map[string]freeTierEntry{}
	freeTierCacheMu.Unlock()
	deepinfraCatalogMu.Lock()
	deepinfraCatalogCache = map[string][]map[string]any{}
	deepinfraCatalogNegCache = map[string]time.Time{}
	deepinfraCatalogMu.Unlock()
}
