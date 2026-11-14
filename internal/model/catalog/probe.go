package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	probeFailureTTL         = 60 * time.Second
	apiModelsTimeout        = 5 * time.Second
	openRouterListTimeout   = 8 * time.Second
	anthropicModelsMaxPages = 20
	anthropicAPIVersion     = "2023-06-01"
	anthropicDefaultBaseURL = "https://api.anthropic.com"
	ModeAnthropicMessages   = "anthropic_messages"
)

var (
	anthropicCommonBetas = []string{"interleaved-thinking-2025-05-14", "fine-grained-tool-streaming-2025-05-14"}
	anthropicOAuthBetas  = []string{"claude-code-20250219", "oauth-2025-04-20"}
	anthropicContext1M   = "context-1m-2025-08-07"
)

type ProbeResult struct {
	Models           []string
	Listed           bool
	ProbedURL        string
	ResolvedBaseURL  string
	SuggestedBaseURL string
	UsedFallback     bool
}

type listState struct {
	mu                sync.Mutex
	freeTier          freeTierMemo
	recommended       map[string]recommendedMemo
	probeFailures     map[string]time.Time
	openRouterCurated []ModelChoice
	aiGatewayCurated  []ModelChoice
}

type liveIndex struct {
	items []wire.Value
	order []string
	byID  map[string]wire.Value
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func probeFailureKey(baseURL string) (string, bool) {
	_, host, port := urlOrigin(baseURL)
	if host == "" {
		return "", false
	}
	return host + ":" + strconv.Itoa(port), true
}

func (c *Catalog) probeAPIModels(ctx context.Context, apiKey auth.Secret, baseURL string, timeout time.Duration, mode string, requestHeaders map[string]string) ProbeResult {
	normalized := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if normalized == "" {
		return ProbeResult{}
	}
	if owner, ok := c.reg.ForHost(ctx, normalized, true); ok {
		if prober, ok := owner.(providers.ModelProber); ok {
			if owned, ok := prober.ProbeModels(ctx, normalized, apiKey, timeout); ok {
				return ProbeResult{Models: owned.Models, Listed: owned.Listed, ProbedURL: owned.URL, ResolvedBaseURL: owned.BaseURL}
			}
		}
	}
	alternate := normalized + "/v1"
	if strings.HasSuffix(normalized, "/v1") {
		alternate = strings.TrimRight(normalized[:len(normalized)-3], "/")
	}
	suggested := ""
	if alternate != normalized {
		suggested = alternate
	}
	candidates := []string{normalized}
	if alternate != "" && alternate != normalized {
		candidates = append(candidates, alternate)
	}
	st := &c.st.lists
	failureKey, keyed := probeFailureKey(normalized)
	if keyed {
		st.mu.Lock()
		seen, failed := st.probeFailures[failureKey]
		st.mu.Unlock()
		if failed && time.Since(seen) < probeFailureTTL {
			return ProbeResult{ProbedURL: normalized + "/models", ResolvedBaseURL: normalized, SuggestedBaseURL: suggested}
		}
	}
	header := http.Header{"User-Agent": {c.userAgent()}}
	if providers.URLHostname(normalized) == "generativelanguage.googleapis.com" {
		header.Set("X-Goog-Api-Client", strings.TrimSuffix("icebath/"+c.version, "/"))
	}
	if key := apiKey.Reveal(); key != "" {
		if mode == ModeAnthropicMessages {
			header.Set("x-api-key", key)
			header.Set("anthropic-version", anthropicAPIVersion)
		} else {
			header.Set("Authorization", "Bearer "+key)
		}
	}
	for name, value := range requestHeaders {
		if name != "" {
			header.Set(name, value)
		}
	}
	allTimedOut := true
	for i, candidate := range candidates {
		probed := strings.TrimRight(candidate, "/") + "/models"
		payload, err := c.getJSONObject(ctx, probed, header, normalized, timeout)
		if err != nil {
			allTimedOut = allTimedOut && isTimeout(err)
			continue
		}
		if keyed {
			st.mu.Lock()
			delete(st.probeFailures, failureKey)
			st.mu.Unlock()
		}
		ids := []string{}
		items, _ := dataItems(payload)
		for _, item := range items {
			if !item.IsObject() || c.NoteCatalogItem(item) {
				continue
			}
			ids = append(ids, item.Get("id").Display())
		}
		result := ProbeResult{Models: ids, Listed: true, ProbedURL: probed, ResolvedBaseURL: strings.TrimRight(candidate, "/"), UsedFallback: i > 0}
		if alternate != candidate {
			result.SuggestedBaseURL = alternate
		} else {
			result.SuggestedBaseURL = normalized
		}
		return result
	}
	if keyed && allTimedOut {
		st.mu.Lock()
		st.probeFailures[failureKey] = time.Now()
		st.mu.Unlock()
	}
	return ProbeResult{ProbedURL: candidates[0] + "/models", ResolvedBaseURL: normalized, SuggestedBaseURL: suggested}
}

func (c *Catalog) ProbeAPIModels(ctx context.Context, key auth.Credential, baseURL string, timeout time.Duration, mode string, headers map[string]string) ProbeResult {
	ctx = c.scoped(ctx)
	if timeout <= 0 {
		timeout = apiModelsTimeout
	}
	return c.probeAPIModels(ctx, key.ProbeKey(ctx), baseURL, timeout, mode, headers)
}

func (c *Catalog) fetchAPIModels(ctx context.Context, apiKey auth.Secret, baseURL string, timeout time.Duration, mode string, headers map[string]string) ([]string, bool) {
	result := c.probeAPIModels(ctx, apiKey, baseURL, timeout, mode, headers)
	return result.Models, result.Listed
}

func (c *Catalog) FetchAPIModels(ctx context.Context, key auth.Credential, baseURL string, timeout time.Duration, mode string, headers map[string]string) ([]string, bool) {
	result := c.ProbeAPIModels(ctx, key, baseURL, timeout, mode, headers)
	return result.Models, result.Listed
}

func (c *Catalog) ListAPIModels(ctx context.Context, apiKey, baseURL string, headers map[string]string, timeout time.Duration) ([]string, bool) {
	ctx = c.scoped(ctx)
	if timeout <= 0 {
		timeout = apiModelsTimeout
	}
	return c.fetchAPIModels(ctx, auth.NewSecret(apiKey), baseURL, timeout, "", headers)
}

func baseURLLooksLikeAnthropicMessages(baseURL string) bool {
	normalized := strings.TrimRight(strings.ToLower(strings.TrimSpace(baseURL)), "/")
	if normalized == "" {
		return false
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return false
	}
	path := strings.TrimRight(parsed.Path, "/")
	return strings.HasSuffix(path, "/anthropic") || strings.HasSuffix(path, "/anthropic/v1")
}

func anthropicModelsURL(baseURL, afterID string) string {
	endpoint := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if endpoint == "" {
		endpoint = anthropicDefaultBaseURL
	}
	target := endpoint + "/v1/models"
	if strings.HasSuffix(endpoint, "/v1") {
		target = endpoint + "/models"
	}
	query := "limit=1000"
	if afterID != "" {
		query += "&after_id=" + url.QueryEscape(afterID)
	}
	if strings.Contains(target, "?") {
		return target + "&" + query
	}
	return target + "?" + query
}

func anthropicNextCursor(page wire.Value, seen map[string]bool) (string, bool) {
	if !page.IsObject() || !page.Get("has_more").True() {
		return "", false
	}
	lastID, ok := page.Get("last_id").Str()
	if !ok || lastID == "" || seen[lastID] {
		return "", false
	}
	seen[lastID] = true
	return lastID, true
}

func (c *Catalog) anthropicPoolCatalogCredentials(ctx context.Context) (auth.Secret, string) {
	pool, err := c.auth.Pool(ctx, "anthropic")
	if err != nil {
		return auth.Secret{}, ""
	}
	for _, entry := range pool.Entries() {
		if entry.AuthType != auth.AuthTypeAPIKey {
			continue
		}
		if token := strings.TrimSpace(entry.AccessToken.Reveal()); token != "" {
			base := entry.BaseURL
			if base == "" {
				base = entry.InferenceBaseURL
			}
			return auth.NewSecret(token), strings.TrimSpace(base)
		}
	}
	return auth.Secret{}, ""
}

func anthropicPageIDs(page wire.Value) []string {
	var ids []string
	for _, item := range objectItems(page) {
		if id := item.Get("id"); id.Truthy() {
			ids = append(ids, id.Display())
		}
	}
	return ids
}

func (c *Catalog) fetchAnthropicModels(ctx context.Context, timeout time.Duration, baseURL string, apiKey auth.Secret) ([]string, bool) {
	resolvedBase := baseURL
	token := strings.TrimSpace(apiKey.Reveal())
	if token == "" {
		if resolved, ok := c.auth.ResolveAnthropicToken(ctx, ""); ok {
			token = resolved.Reveal()
		}
	}
	if token == "" {
		var pooled auth.Secret
		pooled, resolvedBase = c.anthropicPoolCatalogCredentials(ctx)
		token = pooled.Reveal()
	}
	if token == "" {
		return nil, false
	}
	header := http.Header{"anthropic-version": {anthropicAPIVersion}}
	oauth := auth.IsAnthropicOAuthToken(token)
	if oauth {
		header.Set("Authorization", "Bearer "+token)
		header.Set("anthropic-beta", strings.Join(append(slices.Clone(anthropicCommonBetas), anthropicOAuthBetas...), ","))
	} else {
		header.Set("x-api-key", token)
	}
	first := anthropicModelsURL(resolvedBase, "")
	page, err := c.getJSONObject(ctx, first, header, "", timeout)
	var status *statusError
	if err != nil && oauth && errors.As(err, &status) && status.status == http.StatusBadRequest {
		body := strings.ToLower(string(status.body))
		if strings.Contains(body, "long context beta") && strings.Contains(body, "not yet available") {
			var betas []string
			for _, beta := range anthropicCommonBetas {
				if beta != anthropicContext1M {
					betas = append(betas, beta)
				}
			}
			header.Set("anthropic-beta", strings.Join(append(betas, anthropicOAuthBetas...), ","))
			page, err = c.getJSONObject(ctx, first, header, "", timeout)
		}
	}
	if err != nil {
		slog.Debug("failed to fetch Anthropic models", "error", err)
		return nil, false
	}
	models := anthropicPageIDs(page)
	seen := map[string]bool{}
	for range anthropicModelsMaxPages {
		cursor, more := anthropicNextCursor(page, seen)
		if !more {
			break
		}
		if page, err = c.getJSONObject(ctx, anthropicModelsURL(resolvedBase, cursor), header, "", timeout); err != nil {
			slog.Debug("failed to fetch Anthropic models", "error", err)
			return nil, false
		}
		models = append(models, anthropicPageIDs(page)...)
	}
	models = dedupe(models)
	tier := func(id string) int {
		switch {
		case strings.Contains(id, "opus"):
			return 0
		case strings.Contains(id, "sonnet"):
			return 1
		case strings.Contains(id, "haiku"):
			return 2
		}
		return 3
	}
	sort.SliceStable(models, func(i, j int) bool {
		if a, b := tier(models[i]), tier(models[j]); a != b {
			return a < b
		}
		return models[i] < models[j]
	})
	return models, true
}

func (c *Catalog) fetchAIGatewayLanguageModels(ctx context.Context, timeout time.Duration) ([]string, bool) {
	apiKey := c.env(ctx, "AI_GATEWAY_API_KEY")
	if apiKey == "" {
		return nil, false
	}
	baseURL := c.env(ctx, "AI_GATEWAY_BASE_URL")
	if baseURL == "" {
		baseURL = aiGatewayBaseURL
	}
	header := http.Header{"Authorization": {"Bearer " + apiKey}, "User-Agent": {c.userAgent()}}
	payload, err := c.getJSONObject(ctx, strings.TrimRight(baseURL, "/")+"/models", header, "", timeout)
	if err != nil {
		return nil, false
	}
	ids := []string{}
	for _, item := range objectItems(payload) {
		id := item.Get("id")
		kind, _ := item.Get("type").Str()
		tooled := slices.ContainsFunc(item.Get("tags").Elems(), func(tag wire.Value) bool {
			text, ok := tag.Str()
			return ok && text == "tool-use"
		})
		if id.Truthy() && kind == "language" && tooled {
			ids = append(ids, id.Display())
		}
	}
	return ids, true
}

func (c *Catalog) fetchLiveCatalogIndex(ctx context.Context, rawURL string, timeout time.Duration) (*liveIndex, bool) {
	payload, err := c.getJSONObject(ctx, rawURL, http.Header{"Accept": {"application/json"}}, "", timeout)
	if err != nil {
		return nil, false
	}
	data := payload.Get("data")
	if data.Exists() && !data.IsArray() {
		return nil, false
	}
	index := &liveIndex{items: data.Elems(), byID: map[string]wire.Value{}}
	for _, item := range index.items {
		if !item.IsObject() {
			continue
		}
		id := strings.TrimSpace(itemID(item))
		if id == "" {
			continue
		}
		if _, ok := index.byID[id]; !ok {
			index.order = append(index.order, id)
		}
		index.byID[id] = item
	}
	return index, true
}

func (c *Catalog) openRouterCuratedPath() string { return home.OpenRouterCuratedCatalog.In(c.home) }

func (c *Catalog) readOpenRouterCuratedDisk(ctx context.Context, allowStale bool) []ModelChoice {
	raw := readJSONObject(c.openRouterCuratedPath())
	if !allowStale {
		fetchedAt := 0.0
		if stamp, ok := raw["fetched_at"]; ok {
			value, err := wire.ParseValue(stamp)
			if err != nil {
				return nil
			}
			seconds, ok := parseFloat(value.Display())
			if !ok {
				return nil
			}
			fetchedAt = seconds
		}
		if unixSeconds(time.Now())-fetchedAt > c.ManifestRefreshInterval(ctx).Seconds() {
			return nil
		}
	}
	items, err := wire.ParseValue(raw["curated"])
	if err != nil || !items.IsArray() {
		return nil
	}
	var out []ModelChoice
	for _, item := range items.Elems() {
		if pair := item.Elems(); item.IsArray() && len(pair) == 2 {
			out = append(out, ModelChoice{ID: pair[0].Display(), Description: pair[1].Display()})
		}
	}
	return out
}

func (c *Catalog) writeOpenRouterCuratedDisk(ctx context.Context, curated []ModelChoice) {
	pairs := make([][2]string, 0, len(curated))
	for _, choice := range curated {
		pairs = append(pairs, [2]string{choice.ID, choice.Description})
	}
	payload := struct {
		FetchedAt json.RawMessage `json:"fetched_at"`
		Curated   [][2]string     `json:"curated"`
	}{json.RawMessage(strconv.FormatFloat(unixSeconds(time.Now()), 'f', -1, 64)), pairs}
	if err := fileutil.WriteJSON(ctx, c.openRouterCuratedPath(), payload, fileutil.JSONStyle{Indent: 2}); err != nil {
		slog.Debug("openrouter curated catalog disk write failed", "error", err)
	}
}

func (c *Catalog) fetchOpenRouterModels(ctx context.Context, timeout time.Duration, force, cacheOnly bool) []ModelChoice {
	st := &c.st.lists
	st.mu.Lock()
	cached := st.openRouterCurated
	st.mu.Unlock()
	if cached != nil && !force {
		return slices.Clone(cached)
	}
	if !force {
		if disk := c.readOpenRouterCuratedDisk(ctx, cacheOnly); len(disk) > 0 {
			if !cacheOnly {
				st.mu.Lock()
				st.openRouterCurated = disk
				st.mu.Unlock()
			}
			return slices.Clone(disk)
		}
	}
	if cacheOnly {
		return slices.Clone(openRouterSnapshot)
	}
	fallback := c.curatedOpenRouterModels(ctx)
	if len(fallback) == 0 {
		fallback = slices.Clone(openRouterSnapshot)
	}
	degraded := fallback
	if len(cached) > 0 {
		degraded = slices.Clone(cached)
	}
	live, ok := c.fetchLiveCatalogIndex(ctx, openRouterCatalogURL, timeout)
	if !ok {
		return degraded
	}
	if seeded := c.seedReasoningCaps(ctx, openRouterCatalogURL, live.items); seeded != nil {
		caps := &c.st.caps
		caps.mu.Lock()
		if caps.openRouter.cache == nil {
			caps.openRouter.cache = seeded
		}
		caps.mu.Unlock()
	}
	curated := []ModelChoice{}
	silentDefault := c.preferredSilentDefault("openrouter")
	for _, preferred := range fallback {
		item, listed := live.byID[preferred.ID]
		if !listed || !supportsTools(item) {
			continue
		}
		desc := ""
		switch {
		case preferred.ID == silentDefault:
			desc = "default"
		case livePricingIsFree(item.Get("pricing"), "prompt", "completion"):
			desc = "free"
		}
		curated = append(curated, ModelChoice{ID: preferred.ID, Description: desc})
	}
	if len(curated) == 0 {
		return degraded
	}
	if curated[0].Description == "" {
		curated[0].Description = "recommended"
	}
	st.mu.Lock()
	st.openRouterCurated = curated
	st.mu.Unlock()
	c.writeOpenRouterCuratedDisk(ctx, curated)
	return slices.Clone(curated)
}

func (c *Catalog) FetchOpenRouterModels(ctx context.Context, force, cacheOnly bool) []ModelChoice {
	return c.fetchOpenRouterModels(c.scoped(ctx), openRouterListTimeout, force, cacheOnly)
}

func (c *Catalog) OpenRouterModelIDs(ctx context.Context, force bool) []string {
	return choiceIDs(c.fetchOpenRouterModels(c.scoped(ctx), openRouterListTimeout, force, false))
}

func (c *Catalog) fetchAIGatewayModels(ctx context.Context, timeout time.Duration, force bool) []ModelChoice {
	st := &c.st.lists
	st.mu.Lock()
	cached := st.aiGatewayCurated
	st.mu.Unlock()
	if cached != nil && !force {
		return slices.Clone(cached)
	}
	degraded := slices.Clone(aiGatewaySnapshot)
	if len(cached) > 0 {
		degraded = slices.Clone(cached)
	}
	live, ok := c.fetchLiveCatalogIndex(ctx, strings.TrimRight(aiGatewayBaseURL, "/")+"/models", timeout)
	if !ok {
		return degraded
	}
	free := func(id string) bool { return livePricingIsFree(live.byID[id].Get("pricing"), "input", "output") }
	curated := []ModelChoice{}
	for _, preferred := range aiGatewaySnapshot {
		if _, listed := live.byID[preferred.ID]; !listed {
			continue
		}
		desc := ""
		if free(preferred.ID) {
			desc = "free"
		}
		curated = append(curated, ModelChoice{ID: preferred.ID, Description: desc})
	}
	if len(curated) == 0 {
		return degraded
	}
	freeMoonshot := ""
	for _, id := range live.order {
		if strings.HasPrefix(id, "moonshotai/") && free(id) {
			freeMoonshot = id
			break
		}
	}
	if freeMoonshot != "" {
		promoted := []ModelChoice{{ID: freeMoonshot, Description: "recommended"}}
		for _, choice := range curated {
			if choice.ID != freeMoonshot {
				promoted = append(promoted, choice)
			}
		}
		curated = promoted
	} else {
		curated[0].Description = "recommended"
	}
	st.mu.Lock()
	st.aiGatewayCurated = curated
	st.mu.Unlock()
	return slices.Clone(curated)
}

func (c *Catalog) AIGatewayModelIDs(ctx context.Context, force bool) []string {
	return choiceIDs(c.fetchAIGatewayModels(c.scoped(ctx), openRouterListTimeout, force))
}
