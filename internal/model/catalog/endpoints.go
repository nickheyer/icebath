package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/local"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	openRouterModelsURL  = "https://openrouter.ai/api/v1/models"
	modelCacheTTL        = time.Hour
	endpointModelTTL     = 300 * time.Second
	endpointProbeTTL     = time.Hour
	endpointProbeMissTTL = 300 * time.Second
	blackholeTTL         = 30 * time.Second
	localProbeDiskTTL    = 300 * time.Second
	localContextProbeTTL = 30 * time.Second
	perMillionQuoteMin   = 0.001
	llamaCppChildLimit   = 16
)

var errUnexpectedShape = errors.New("catalog: a listing reply does not have the expected shape")

type windowState uint8

const (
	windowAbsent windowState = iota
	windowUnknown
	windowKnown
)

type ModelMetadata struct {
	Name                string
	ContextLength       int
	MaxCompletionTokens int
	Pricing             map[string]string
	window              windowState
}

type ModelSet struct {
	order   []string
	items   map[string]*ModelMetadata
	allKeys bool
}

func newModelSet() *ModelSet { return &ModelSet{items: map[string]*ModelMetadata{}} }

func (s *ModelSet) Len() int {
	if s == nil {
		return 0
	}
	return len(s.order)
}

func (s *ModelSet) IDs() []string {
	if s == nil {
		return nil
	}
	return slices.Clone(s.order)
}

func (s *ModelSet) put(id string, m *ModelMetadata) {
	if _, ok := s.items[id]; !ok {
		s.order = append(s.order, id)
	}
	s.items[id] = m
}

func (s *ModelSet) addAliases(id string, m *ModelMetadata) {
	s.put(id, m)
	if _, tail, ok := strings.Cut(id, "/"); ok {
		if _, exists := s.items[tail]; !exists {
			s.put(tail, m)
		}
	}
}

type storedModel struct {
	ContextLength       *int              `json:"context_length,omitempty"`
	MaxCompletionTokens *int              `json:"max_completion_tokens,omitempty"`
	Name                string            `json:"name"`
	Pricing             map[string]string `json:"pricing,omitempty"`
}

type completeStoredModel struct {
	ContextLength       *int              `json:"context_length"`
	MaxCompletionTokens *int              `json:"max_completion_tokens"`
	Name                string            `json:"name"`
	Pricing             map[string]string `json:"pricing"`
}

func (s *ModelSet) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, id := range s.order {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := json.Marshal(id)
		if err != nil {
			return nil, err
		}
		m := s.items[id]
		row := storedModel{Name: m.Name, Pricing: m.Pricing}
		if m.ContextLength != 0 || m.window == windowKnown {
			row.ContextLength = &m.ContextLength
		}
		if m.MaxCompletionTokens != 0 {
			row.MaxCompletionTokens = &m.MaxCompletionTokens
		}
		var stored any = row
		if s.allKeys {
			if row.Pricing == nil {
				row.Pricing = map[string]string{}
			}
			stored = completeStoredModel(row)
		}
		value, err := json.Marshal(stored)
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func storedModelSet(root wire.Value) *ModelSet {
	set := newModelSet()
	for _, field := range root.Fields() {
		if !field.Value.IsObject() {
			continue
		}
		m := &ModelMetadata{Name: field.Value.Get("name").Text(), Pricing: pricingStrings(field.Value.Get("pricing"))}
		if window := field.Value.Get("context_length"); window.Exists() {
			m.window = windowUnknown
			if n, ok := jsonInt(window); ok {
				m.ContextLength, m.window = n, windowKnown
			}
		}
		if n, ok := jsonInt(field.Value.Get("max_completion_tokens")); ok {
			m.MaxCompletionTokens = n
		}
		set.put(field.Key, m)
	}
	return set
}

func jsonInt(v wire.Value) (int, bool) {
	if v.Kind() != wire.KindNumber {
		return 0, false
	}
	n, err := strconv.ParseInt(string(v.Raw()), 10, 64)
	if err != nil {
		return 0, false
	}
	return int(n), true
}

func positiveNumber(v wire.Value) (int, bool) {
	n, ok := v.Int()
	if !ok || n == 0 {
		return 0, false
	}
	if f, _ := v.Float(); f == 0 {
		return 0, false
	}
	return int(n), true
}

func scalarText(v wire.Value) (string, bool) {
	switch v.Kind() {
	case wire.KindString:
		text := v.Text()
		return text, text != ""
	case wire.KindNumber:
		return string(v.Raw()), true
	}
	return "", false
}

func scalarFloat(v wire.Value) (float64, bool) {
	switch v.Kind() {
	case wire.KindNumber:
		return v.Float()
	case wire.KindString:
		f, err := strconv.ParseFloat(strings.TrimSpace(v.Text()), 64)
		return f, err == nil
	}
	return 0, false
}

func formatRate(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func pricingStrings(v wire.Value) map[string]string {
	if !v.IsObject() {
		return nil
	}
	out := map[string]string{}
	for _, field := range v.Fields() {
		if text, ok := scalarText(field.Value); ok {
			out[field.Key] = text
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func walkObjects(v wire.Value, visit func(wire.Value) bool) bool {
	switch v.Kind() {
	case wire.KindObject:
		if visit(v) {
			return true
		}
		for _, field := range v.Fields() {
			if walkObjects(field.Value, visit) {
				return true
			}
		}
	case wire.KindArray:
		for _, item := range v.Elems() {
			if walkObjects(item, visit) {
				return true
			}
		}
	}
	return false
}

func coerceReasonableInt(v wire.Value) (int, bool) {
	var n int64
	switch v.Kind() {
	case wire.KindNumber:
		got, ok := v.Int()
		if !ok {
			return 0, false
		}
		n = got
	case wire.KindString:
		got, err := strconv.ParseInt(strings.ReplaceAll(strings.TrimSpace(v.Text()), ",", ""), 10, 64)
		if err != nil {
			return 0, false
		}
		n = got
	default:
		return 0, false
	}
	if n < 1024 || n > 10_000_000 {
		return 0, false
	}
	return int(n), true
}

func extractFirstInt(payload wire.Value, keys []string) (int, bool) {
	found, ok := 0, false
	walkObjects(payload, func(obj wire.Value) bool {
		for _, field := range obj.Fields() {
			if slices.Contains(keys, strings.ToLower(field.Key)) {
				if n, hit := coerceReasonableInt(field.Value); hit {
					found, ok = n, true
					return true
				}
			}
		}
		return false
	})
	return found, ok
}

func contextLengthFromModelPayload(payload wire.Value) (int, bool) {
	if !payload.IsObject() {
		return 0, false
	}
	for _, key := range contextLengthKeys {
		if n, ok := coerceReasonableInt(payload.Get(key)); ok {
			return n, true
		}
	}
	if n, ok := payload.Get("max_tokens").Int(); ok && n > 0 {
		return int(n), true
	}
	return 0, false
}

var pricingUnitDivisors = map[string]float64{
	"per_token": 1, "per_1k_tokens": 1_000, "per_thousand_tokens": 1_000,
	"per_1m_tokens": 1_000_000, "per_million_tokens": 1_000_000,
}

var tokenRateFields = []string{"prompt", "completion", "cache_read", "cache_write"}

type pricingAlias struct {
	target  string
	aliases []string
}

var pricingAliases = []pricingAlias{
	{"prompt", []string{"prompt", "input", "input_cost_per_token", "prompt_token_cost"}},
	{"completion", []string{"completion", "output", "output_cost_per_token", "completion_token_cost"}},
	{"request", []string{"request", "request_cost"}},
	{"cache_read", []string{"cache_read", "cached_prompt", "input_cache_read", "cache_read_cost_per_token"}},
	{"cache_write", []string{"cache_write", "cache_creation", "input_cache_write", "cache_write_cost_per_token"}},
}

func perTokenRates(source wire.Value, fields [][2]string, scale func(float64) float64) map[string]string {
	out := map[string]string{}
	for _, field := range fields {
		if rate, ok := scalarFloat(source.Get(field[1])); ok {
			out[field[0]] = formatRate(scale(rate))
		}
	}
	return out
}

func normalizeTokenRates(pricing map[string]string, unit wire.Value) map[string]string {
	rates := map[string]float64{}
	for _, key := range tokenRateFields {
		if text, ok := pricing[key]; ok {
			if rate, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil {
				rates[key] = rate
			}
		}
	}
	unitText := ""
	if unit.Truthy() {
		unitText = strings.ToLower(strings.TrimSpace(unit.Display()))
	}
	divisor, known := pricingUnitDivisors[unitText]
	if !known {
		divisor = 1
		for _, rate := range rates {
			if rate >= perMillionQuoteMin {
				divisor = 1_000_000
			}
		}
	}
	if divisor != 1 {
		for key, rate := range rates {
			pricing[key] = formatRate(rate / divisor)
		}
	}
	return pricing
}

func extractPricing(payload wire.Value) map[string]string {
	novita := [][2]string{{"prompt", "input_token_price_per_m"}, {"completion", "output_token_price_per_m"}}
	if payload.Get(novita[0][1]).Kind() > wire.KindNull || payload.Get(novita[1][1]).Kind() > wire.KindNull {
		return perTokenRates(payload, novita, func(v float64) float64 { return v / 10_000 / 1_000_000 })
	}
	deepInfra := [][2]string{{"prompt", "input_tokens"}, {"completion", "output_tokens"}, {"cache_read", "cache_read_tokens"}}
	if quoted := payload.Path("metadata", "pricing"); payload.Get("metadata").IsObject() && quoted.IsObject() &&
		slices.ContainsFunc(deepInfra, func(f [2]string) bool { return quoted.Get(f[1]).Exists() }) {
		return perTokenRates(quoted, deepInfra, func(v float64) float64 { return v / 1_000_000 })
	}
	var found map[string]string
	walkObjects(payload, func(obj wire.Value) bool {
		normalized := map[string]wire.Value{}
		for _, field := range obj.Fields() {
			normalized[strings.ToLower(field.Key)] = field.Value
		}
		pricing := map[string]string{}
		for _, row := range pricingAliases {
			for _, alias := range row.aliases {
				if text, ok := scalarText(normalized[alias]); ok {
					pricing[row.target] = text
					break
				}
			}
		}
		if len(pricing) == 0 {
			return false
		}
		found = normalizeTokenRates(pricing, normalized["unit"])
		return true
	})
	return found
}

func endpointModelEntry(model wire.Value, id string, contextLength int) *ModelMetadata {
	m := &ModelMetadata{Name: id, ContextLength: contextLength}
	if name := model.Get("name"); name.Exists() {
		m.Name = name.Text()
	}
	if n, ok := extractFirstInt(model, maxCompletionKeys); ok {
		m.MaxCompletionTokens = n
	}
	if pricing := extractPricing(model); len(pricing) > 0 {
		m.Pricing = pricing
	}
	return m
}

func (c *Catalog) noteBlackholed(baseURL string) {
	key, ok := endpointHostKey(baseURL)
	if !ok {
		return
	}
	c.mu.Lock()
	c.blackholes[key] = time.Now()
	c.mu.Unlock()
	slog.Debug("endpoint timed out connecting, skipping further probes", "endpoint", key, "seconds", blackholeTTL.Seconds())
}

func (c *Catalog) blackholed(baseURL string) bool {
	key, ok := endpointHostKey(baseURL)
	if !ok {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	seen, ok := c.blackholes[key]
	if !ok {
		return false
	}
	if time.Since(seen) >= blackholeTTL {
		delete(c.blackholes, key)
		return false
	}
	return true
}

func (c *Catalog) noteIfConnectTimeout(err error, baseURL string) {
	if isConnectTimeout(err) {
		c.noteBlackholed(baseURL)
	}
}

func (c *Catalog) openRouterCachePath() string { return home.OpenRouterModelMetadata.In(c.home) }

func (c *Catalog) openRouterDiskAge() (time.Duration, bool) {
	info, err := os.Stat(c.openRouterCachePath())
	if err != nil {
		return 0, false
	}
	age := time.Since(info.ModTime())
	return age, age >= 0
}

func (c *Catalog) loadOpenRouterDisk() *ModelSet {
	var raw json.RawMessage
	if err := fileutil.ReadJSON(c.openRouterCachePath(), &raw); err != nil {
		slog.Debug("failed to load OpenRouter model metadata disk cache", "error", err)
		return newModelSet()
	}
	root, err := wire.ParseValue(raw)
	if err != nil || !root.IsObject() {
		return newModelSet()
	}
	set := storedModelSet(root)
	set.allKeys = true
	return set
}

func (c *Catalog) fetchOpenRouter(ctx context.Context) (*ModelSet, error) {
	resp, err := c.get(ctx, openRouterModelsURL, nil, "", fetchPhases)
	if err != nil {
		return nil, err
	}
	if !resp.success() {
		return nil, &httpclient.StatusError{Code: resp.status, Status: http.StatusText(resp.status), Method: http.MethodGet, Host: "openrouter.ai"}
	}
	payload, err := resp.value()
	if err != nil {
		return nil, err
	}
	if !payload.IsObject() {
		return nil, errUnexpectedShape
	}
	set := newModelSet()
	set.allKeys = true
	for _, model := range payload.Get("data").Elems() {
		if !model.IsObject() {
			return nil, errUnexpectedShape
		}
		id := model.Get("id").Text()
		entry := &ModelMetadata{Name: id, ContextLength: 128000, MaxCompletionTokens: 4096, Pricing: pricingStrings(model.Get("pricing")), window: windowKnown}
		if window := model.Get("context_length"); window.Exists() {
			entry.ContextLength, entry.window = 0, windowUnknown
			if n, ok := jsonInt(window); ok {
				entry.ContextLength, entry.window = n, windowKnown
			}
		}
		if top := model.Get("top_provider"); top.Exists() {
			if !top.IsObject() {
				return nil, errUnexpectedShape
			}
			if limit := top.Get("max_completion_tokens"); limit.Exists() {
				entry.MaxCompletionTokens, _ = jsonInt(limit)
			}
		}
		if name := model.Get("name"); name.Exists() {
			entry.Name = name.Text()
		}
		set.addAliases(id, entry)
		if canonical := model.Get("canonical_slug").Text(); canonical != "" && canonical != id {
			set.addAliases(canonical, entry)
		}
	}
	return set, nil
}

func (c *Catalog) openRouterModels(ctx context.Context) *ModelSet {
	c.mu.Lock()
	held, at := c.openRouter, c.openRouterAt
	c.mu.Unlock()
	if held.Len() > 0 && time.Since(at) < modelCacheTTL {
		return held
	}
	if age, ok := c.openRouterDiskAge(); ok && age < modelCacheTTL {
		if disk := c.loadOpenRouterDisk(); disk.Len() > 0 {
			c.mu.Lock()
			c.openRouter, c.openRouterAt = disk, time.Now().Add(-age)
			c.mu.Unlock()
			return disk
		}
	}
	set, err := c.fetchOpenRouter(ctx)
	if err == nil {
		c.mu.Lock()
		c.openRouter, c.openRouterAt = set, time.Now()
		c.mu.Unlock()
		if err := fileutil.WriteJSON(ctx, c.openRouterCachePath(), set, fileutil.JSONStyle{Lines: true, Tight: true}); err != nil {
			slog.Debug("failed to save OpenRouter model metadata disk cache", "error", err)
		}
		slog.Debug("fetched model metadata from OpenRouter", "models", set.Len())
		return set
	}
	slog.Warn("failed to fetch model metadata from OpenRouter", "error", httpclient.Message(err))
	c.mu.Lock()
	cached := c.openRouter
	c.mu.Unlock()
	if cached.Len() > 0 {
		return cached
	}
	disk := c.loadOpenRouterDisk()
	if disk.Len() == 0 {
		return newModelSet()
	}
	staleBy := modelCacheTTL - time.Second
	if age, ok := c.openRouterDiskAge(); ok {
		staleBy = min(age, modelCacheTTL)
	}
	c.mu.Lock()
	c.openRouter, c.openRouterAt = disk, time.Now().Add(-staleBy)
	c.mu.Unlock()
	return disk
}

type endpointKey struct {
	baseURL     string
	fingerprint string
}

type endpointMemo struct {
	models *ModelSet
	at     time.Time
}

func endpointMemoKey(normalized string, key auth.Credential) endpointKey {
	if key.Tokens != nil {
		return endpointKey{baseURL: normalized}
	}
	return endpointKey{baseURL: normalized, fingerprint: auth.FingerprintSecret(key.Key.Reveal())}
}

func (c *Catalog) rememberEndpointModels(key endpointKey, set *ModelSet) *ModelSet {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, memo := range c.endpoints {
		if now.Sub(memo.at) >= endpointModelTTL {
			delete(c.endpoints, k)
		}
	}
	c.endpoints[key] = endpointMemo{models: set, at: now}
	return set
}

func (c *Catalog) endpointDiskGet(normalized string) (*ModelSet, bool) {
	raw, ok := memoGet(home.EndpointModelMetadata.In(c.home), normalized, endpointModelTTL, "at", "models")
	if !ok {
		return nil, false
	}
	root, err := wire.ParseValue(raw)
	if err != nil || !root.IsObject() {
		return nil, false
	}
	return storedModelSet(root), true
}

func parseModelsPayload(payload wire.Value) (*ModelSet, error) {
	if !payload.IsObject() {
		return nil, errUnexpectedShape
	}
	set := newModelSet()
	for _, model := range payload.Get("data").Elems() {
		if !model.IsObject() {
			continue
		}
		id := model.Get("id").Text()
		if id == "" {
			continue
		}
		window, _ := extractFirstInt(model, contextLengthKeys)
		set.addAliases(id, endpointModelEntry(model, id, window))
	}
	return set, nil
}

func lmStudioLoadedContext(model wire.Value) (int, bool) {
	for _, inst := range model.Get("loaded_instances").Elems() {
		if n, ok := jsonInt(inst.Path("config", "context_length")); ok && inst.IsObject() && inst.Get("config").IsObject() && n > 0 {
			return n, true
		}
	}
	return 0, false
}

func (c *Catalog) lmStudioNativeModels(ctx context.Context, normalized string, header http.Header) (*ModelSet, error) {
	target := strings.TrimRight(lmStudioServerRoot(normalized), "/") + "/api/v1/models"
	resp, err := c.get(ctx, target, header, normalized, fetchPhases)
	if err != nil {
		return nil, err
	}
	if !resp.success() {
		return nil, &httpclient.StatusError{Code: resp.status, Status: http.StatusText(resp.status), Method: http.MethodGet, Host: endpointLabel(normalized)}
	}
	payload, err := resp.value()
	if err != nil {
		return nil, err
	}
	if !payload.IsObject() {
		return nil, errUnexpectedShape
	}
	set := newModelSet()
	for _, model := range payload.Get("models").Elems() {
		if !model.IsObject() {
			continue
		}
		id := model.Get("key").Text()
		if id == "" {
			id = model.Get("id").Text()
		}
		if id == "" {
			continue
		}
		window, _ := lmStudioLoadedContext(model)
		entry := endpointModelEntry(model, id, window)
		set.addAliases(id, entry)
		if alt := model.Get("id").Text(); alt != "" && alt != id {
			set.addAliases(alt, entry)
		}
	}
	return set, nil
}

func propsContext(props wire.Value) (int, bool) {
	return positiveNumber(props.Path("default_generation_settings", "n_ctx"))
}

func (c *Catalog) llamaCppProps(ctx context.Context, base string, query url.Values, header http.Header, tlsURL string) (*reply, error) {
	suffix := ""
	if len(query) > 0 {
		suffix = "?" + query.Encode()
	}
	resp, err := c.get(ctx, base+"/v1/props"+suffix, header, tlsURL, propsPhases)
	if err != nil {
		return nil, err
	}
	if resp.failed() {
		return c.get(ctx, base+"/props"+suffix, header, tlsURL, propsPhases)
	}
	return resp, nil
}

func (c *Catalog) applyLlamaCppProps(ctx context.Context, set *ModelSet, requestCandidate string, header http.Header, tlsURL string) error {
	base := strings.ReplaceAll(strings.TrimRight(requestCandidate, "/"), "/v1", "")
	resp, err := c.llamaCppProps(ctx, base, nil, header, tlsURL)
	if err != nil {
		return err
	}
	if !resp.failed() {
		props, err := resp.value()
		if err != nil {
			return err
		}
		alias := props.Get("model_alias").Text()
		if window, ok := propsContext(props); ok && alias != "" {
			if entry, known := set.items[alias]; known {
				entry.ContextLength = window
			}
		}
		return nil
	}
	native, err := c.get(ctx, base+"/models", header, tlsURL, propsPhases)
	if err != nil {
		return err
	}
	if native.failed() {
		return nil
	}
	listing, err := native.value()
	if err != nil {
		return err
	}
	children := listing.Get("data").Elems()
	if len(children) > llamaCppChildLimit {
		children = children[:llamaCppChildLimit]
	}
	for _, child := range children {
		id := child.Get("id").Text()
		entry, known := set.items[id]
		state := child.Path("status", "value").Text()
		if id == "" || !known || (state != "loaded" && state != "ready") {
			continue
		}
		childResp, err := c.llamaCppProps(ctx, base, url.Values{"model": {id}, "autoload": {"false"}}, header, tlsURL)
		if err != nil {
			return err
		}
		if childResp.failed() {
			continue
		}
		props, err := childResp.value()
		if err != nil {
			return err
		}
		if window, ok := propsContext(props); ok {
			entry.ContextLength = window
		}
	}
	return nil
}

func (c *Catalog) endpointModels(ctx context.Context, baseURL string, key auth.Credential) *ModelSet {
	normalized := normalizeBaseURL(baseURL)
	if normalized == "" || providers.HostMatches(normalized, "openrouter.ai") {
		return newModelSet()
	}
	isLocal := IsLocalEndpoint(normalized)
	memoKey := endpointMemoKey(normalized, key)
	c.mu.Lock()
	memo, ok := c.endpoints[memoKey]
	c.mu.Unlock()
	if ok && time.Since(memo.at) < endpointModelTTL {
		return memo.models
	}
	if !isLocal {
		if disk, ok := c.endpointDiskGet(normalized); ok {
			return c.rememberEndpointModels(memoKey, disk)
		}
	}
	if c.blackholed(normalized) {
		return newModelSet()
	}
	candidates := []string{normalized}
	alternate := normalized + "/v1"
	if strings.HasSuffix(normalized, "/v1") {
		alternate = strings.TrimRight(normalized[:len(normalized)-3], "/")
	}
	if alternate != normalized {
		candidates = append(candidates, alternate)
	}
	header := bearerHeader(ctx, key)
	var lastErr error
	if isLocal {
		if kind, ok := c.detectLocalServerType(ctx, normalized, key); ok && kind == ServerLMStudio {
			set, err := c.lmStudioNativeModels(ctx, normalized, header)
			if err == nil {
				return c.rememberEndpointModels(memoKey, set)
			}
			lastErr = err
			c.noteIfConnectTimeout(err, normalized)
		}
	}
	for _, candidate := range candidates {
		if c.blackholed(normalized) {
			break
		}
		requestCandidate := localhostToIPv4(candidate)
		target := strings.TrimRight(requestCandidate, "/") + "/models"
		resp, err := c.get(ctx, target, header, normalized, fetchPhases)
		if err != nil {
			lastErr = err
			c.noteIfConnectTimeout(err, normalized)
			continue
		}
		if resp.status == http.StatusUnauthorized || resp.status == http.StatusForbidden {
			slog.Debug("model metadata probe was refused, stopping candidate probing", "status", resp.status, "endpoint", endpointLabel(target))
			break
		}
		if !resp.success() {
			lastErr = &httpclient.StatusError{Code: resp.status, Status: http.StatusText(resp.status), Method: http.MethodGet, Host: endpointLabel(target)}
			continue
		}
		payload, err := resp.value()
		if err != nil {
			lastErr = err
			continue
		}
		set, err := parseModelsPayload(payload)
		if err != nil {
			lastErr = err
			continue
		}
		if slices.ContainsFunc(payload.Get("data").Elems(), func(m wire.Value) bool { return m.IsObject() && m.Get("owned_by").Text() == "llamacpp" }) {
			if err := c.applyLlamaCppProps(ctx, set, requestCandidate, header, normalized); err != nil {
				slog.Debug("llama.cpp props lookup failed", "error", httpclient.Message(err))
			}
		}
		if set.Len() > 0 && !isLocal {
			memoPut(ctx, home.EndpointModelMetadata.In(c.home), normalized, set, endpointModelTTL, "at", "models", "endpoint model metadata disk cache")
		}
		return c.rememberEndpointModels(memoKey, set)
	}
	if lastErr != nil {
		slog.Debug("failed to fetch model metadata", "endpoint", endpointLabel(normalized), "error", httpclient.Message(lastErr))
	}
	return c.rememberEndpointModels(memoKey, newModelSet())
}

func (c *Catalog) endpointContextLength(ctx context.Context, model, baseURL string, key auth.Credential) (int, bool) {
	set := c.endpointModels(ctx, baseURL, key)
	matched, ok := set.items[model]
	if !ok && set.Len() == 1 {
		matched, ok = set.items[set.order[0]], true
	} else if !ok && model != "" {
		for _, id := range set.order {
			if strings.Contains(id, model) || strings.Contains(model, id) {
				matched, ok = set.items[id], true
				break
			}
		}
	}
	if !ok || matched.ContextLength == 0 {
		return 0, false
	}
	return matched.ContextLength, true
}

type ServerType string

const (
	ServerOllama   ServerType = "ollama"
	ServerLMStudio ServerType = "lm-studio"
	ServerVLLM     ServerType = "vllm"
	ServerLlamaCpp ServerType = "llamacpp"
)

type serverVerdict struct {
	kind ServerType
	at   time.Time
}

type prober struct {
	client *http.Client
	header http.Header
	total  time.Duration
}

func (c *Catalog) prober(ctx context.Context, tlsURL string, key auth.Credential, p phases) (*prober, error) {
	cl, err := c.client(ctx, tlsURL, p)
	if err != nil {
		return nil, err
	}
	return &prober{client: cl, header: bearerHeader(ctx, key), total: p.total}, nil
}

func (p *prober) get(ctx context.Context, rawURL string) (*reply, error) {
	bounded, cancel := context.WithTimeout(ctx, p.total)
	defer cancel()
	return send(bounded, p.client, http.MethodGet, rawURL, p.header, nil, nil)
}

func (p *prober) post(ctx context.Context, rawURL string, payload any) (*reply, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	bounded, cancel := context.WithTimeout(ctx, p.total)
	defer cancel()
	return send(bounded, p.client, http.MethodPost, rawURL, p.header, body, nil)
}

type serverLeg struct {
	kind  ServerType
	urls  []string
	check func(*reply) (bool, error)
}

func replyHasKey(key string) func(*reply) (bool, error) {
	return func(r *reply) (bool, error) {
		v, err := r.value()
		if err != nil {
			return false, err
		}
		if v.IsObject() {
			return v.Get(key).Exists(), nil
		}
		return slices.ContainsFunc(v.Elems(), func(item wire.Value) bool { return item.IsString() && item.Text() == key }), nil
	}
}

func (c *Catalog) detectLocalServerType(ctx context.Context, baseURL string, key auth.Credential) (ServerType, bool) {
	normalized := localhostToIPv4(normalizeBaseURL(baseURL))
	if inferred, ok := c.InferProviderFromURL(ctx, normalized); ok && inferred != "ollama-cloud" {
		return "", false
	}
	server := serverRoot(normalized)
	lmStudio := lmStudioServerRoot(normalized)
	c.mu.Lock()
	cached, ok := c.verdicts[server]
	c.mu.Unlock()
	if ok {
		ttl := endpointProbeMissTTL
		if cached.kind != "" {
			ttl = endpointProbeTTL
		}
		if time.Since(cached.at) < ttl {
			return cached.kind, cached.kind != ""
		}
	}
	if c.blackholed(server) {
		return "", false
	}
	if raw, ok := c.localProbeGet("server_type", server); ok {
		var kind string
		if json.Unmarshal(raw, &kind) == nil && json.Valid(raw) && bytes.HasPrefix(bytes.TrimSpace(raw), []byte(`"`)) {
			c.mu.Lock()
			c.verdicts[server] = serverVerdict{kind: ServerType(kind), at: time.Now()}
			c.mu.Unlock()
			return ServerType(kind), true
		}
	}
	legs := []serverLeg{
		{ServerLMStudio, []string{lmStudio + "/api/v1/models"}, func(*reply) (bool, error) { return true, nil }},
		{ServerOllama, []string{server + "/api/tags"}, replyHasKey("models")},
		{ServerLlamaCpp, []string{server + "/v1/props", server + "/props"}, func(r *reply) (bool, error) {
			return bytes.Contains(r.body, []byte("default_generation_settings")), nil
		}},
		{ServerVLLM, []string{server + "/version"}, replyHasKey("version")},
	}
	var result ServerType
	if p, err := c.prober(ctx, baseURL, key, detectPhases); err == nil {
	waterfall:
		for _, leg := range legs {
			var last *reply
			for _, target := range leg.urls {
				resp, err := p.get(ctx, target)
				if err != nil {
					last = nil
					if isConnectTimeout(err) {
						c.noteBlackholed(server)
						break waterfall
					}
					break
				}
				last = resp
				if resp.status == http.StatusOK {
					break
				}
			}
			if last == nil || last.status != http.StatusOK {
				continue
			}
			if hit, err := leg.check(last); err == nil && hit {
				result = leg.kind
				break
			}
		}
	}
	c.mu.Lock()
	c.verdicts[server] = serverVerdict{kind: result, at: time.Now()}
	c.mu.Unlock()
	if result != "" {
		c.localProbePut(ctx, "server_type", server, string(result))
	}
	return result, result != ""
}

func (c *Catalog) DetectLocalServerType(ctx context.Context, baseURL string, key auth.Credential) (ServerType, bool) {
	return c.detectLocalServerType(c.scoped(ctx), baseURL, key)
}

func (c *Catalog) ollamaShow(ctx context.Context, server string, key auth.Credential, model string, p phases, noteBlackhole bool) (wire.Value, bool) {
	pr, err := c.prober(ctx, server, key, p)
	if err != nil {
		return wire.Value{}, false
	}
	resp, err := pr.post(ctx, server+"/api/show", map[string]string{"name": model})
	if err != nil {
		if noteBlackhole {
			c.noteIfConnectTimeout(err, server)
		}
		return wire.Value{}, false
	}
	if resp.status != http.StatusOK {
		return wire.Value{}, false
	}
	data, err := resp.value()
	if err != nil {
		return wire.Value{}, false
	}
	return data, true
}

func ollamaShowContext(data wire.Value, ggufFirst bool, minimum int) (int, bool) {
	numCtx := func() []int {
		var out []int
		for _, line := range strings.Split(data.Get("parameters").Text(), "\n") {
			parts := strings.Fields(line)
			if strings.Contains(line, "num_ctx") && len(parts) >= 2 {
				if n, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
					out = append(out, n)
				}
			}
		}
		return out
	}
	gguf := func() []int {
		var out []int
		for _, field := range data.Get("model_info").Fields() {
			if n, ok := field.Value.Int(); ok && strings.Contains(field.Key, "context_length") {
				out = append(out, int(n))
			}
		}
		return out
	}
	readers := []func() []int{numCtx, gguf}
	if ggufFirst {
		readers = []func() []int{gguf, numCtx}
	}
	for _, reader := range readers {
		for _, window := range reader() {
			if window >= minimum {
				return window, true
			}
		}
	}
	return 0, false
}

func (c *Catalog) isOllamaServer(ctx context.Context, baseURL string, key auth.Credential) bool {
	kind, ok := c.detectLocalServerType(ctx, baseURL, key)
	return ok && kind == ServerOllama
}

func (c *Catalog) OllamaNumCtx(ctx context.Context, model, baseURL string, key auth.Credential) (int, bool) {
	ctx = c.scoped(ctx)
	bare, server := c.StripProviderPrefix(ctx, model), serverRoot(baseURL)
	if !c.isOllamaServer(ctx, baseURL, key) {
		return 0, false
	}
	diskKey := server + "|" + bare
	if raw, ok := c.localProbeGet("ollama_num_ctx", diskKey); ok {
		if v, err := wire.ParseValue(raw); err == nil {
			if n, ok := jsonInt(v); ok && n > 0 {
				return n, true
			}
		}
	}
	data, ok := c.ollamaShow(ctx, server, key, bare, localPhases, false)
	if !ok {
		return 0, false
	}
	window, ok := ollamaShowContext(data, false, math.MinInt)
	if ok {
		c.localProbePut(ctx, "ollama_num_ctx", diskKey, window)
	}
	return window, ok
}

func (c *Catalog) OllamaSupportsVision(ctx context.Context, model, baseURL string, key auth.Credential) (supports, known bool) {
	ctx = c.scoped(ctx)
	bare := c.StripProviderPrefix(ctx, model)
	if bare == "" || baseURL == "" || !c.isOllamaServer(ctx, baseURL, key) {
		return false, false
	}
	data, ok := c.ollamaShow(ctx, serverRoot(baseURL), key, bare, localPhases, false)
	if !ok {
		return false, false
	}
	if caps := data.Get("capabilities"); caps.IsArray() && len(caps.Elems()) > 0 {
		return slices.ContainsFunc(caps.Elems(), func(v wire.Value) bool { return strings.ToLower(v.Display()) == "vision" }), true
	}
	for _, field := range data.Get("model_info").Fields() {
		if strings.Contains(strings.ToLower(field.Key), "vision.block_count") {
			return true, true
		}
	}
	return false, false
}

type localProbeKey struct {
	kind    string
	model   string
	baseURL string
}

type localProbe struct {
	window int
	at     time.Time
}

func (c *Catalog) memoLocalProbe(key localProbeKey, probe func() (int, bool)) (int, bool) {
	now := time.Now()
	c.mu.Lock()
	cached, ok := c.localProbes[key]
	c.mu.Unlock()
	if ok && now.Sub(cached.at) < localContextProbeTTL {
		return cached.window, true
	}
	window, ok := probe()
	if ok && window != 0 {
		c.mu.Lock()
		c.localProbes[key] = localProbe{window: window, at: now}
		c.mu.Unlock()
	}
	return window, ok
}

func (c *Catalog) ollamaAPIShow(ctx context.Context, model, baseURL string, key auth.Credential) (int, bool) {
	memoKey := localProbeKey{kind: "ollama_show", model: c.StripProviderPrefix(ctx, model), baseURL: strings.TrimRight(baseURL, "/")}
	return c.memoLocalProbe(memoKey, func() (int, bool) {
		server := serverRoot(baseURL)
		if c.blackholed(server) {
			return 0, false
		}
		data, ok := c.ollamaShow(ctx, server, key, model, propsPhases, true)
		if !ok {
			return 0, false
		}
		return ollamaShowContext(data, true, 1024)
	})
}

func pathSegments(model string) string {
	parts := strings.Split(model, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func lmStudioContext(ctx context.Context, p *prober, lmStudio, model string) (int, bool, error) {
	resp, err := p.get(ctx, lmStudio+"/api/v1/models")
	if err != nil {
		return 0, false, err
	}
	if resp.status != http.StatusOK {
		return 0, false, nil
	}
	payload, err := resp.value()
	if err != nil {
		return 0, false, err
	}
	for _, m := range payload.Get("models").Elems() {
		if !ModelIDMatches(m.Get("key").Text(), model) && !ModelIDMatches(m.Get("id").Text(), model) {
			continue
		}
		for _, inst := range m.Get("loaded_instances").Elems() {
			if n, ok := positiveNumber(inst.Path("config", "context_length")); ok {
				return n, true, nil
			}
		}
		return 0, false, nil
	}
	return 0, false, nil
}

func openAIModelsListContext(ctx context.Context, p *prober, server, model string) (int, bool, error) {
	resp, err := p.get(ctx, server+"/v1/models")
	if err != nil {
		return 0, false, err
	}
	if resp.status != http.StatusOK {
		return 0, false, nil
	}
	payload, err := resp.value()
	if err != nil {
		return 0, false, err
	}
	models := payload.Get("data").Elems()
	var matched wire.Value
	for _, m := range models {
		if m.IsObject() && ModelIDMatches(m.Get("id").Text(), model) {
			matched = m
			break
		}
	}
	if !matched.Exists() && len(models) == 1 {
		matched = models[0]
		if !matched.IsObject() {
			return 0, false, errUnexpectedShape
		}
	}
	if !matched.Exists() {
		return 0, false, nil
	}
	sources := []wire.Value{matched}
	if meta := matched.Get("meta"); meta.IsObject() {
		sources = append(sources, meta)
	}
	for _, source := range sources {
		if n, ok := positiveNumber(source.Get("n_ctx")); ok {
			return n, true, nil
		}
	}
	for _, source := range sources {
		if n, ok := contextLengthFromModelPayload(source); ok {
			return n, true, nil
		}
	}
	return 0, false, nil
}

func (c *Catalog) localContextLengthUncached(ctx context.Context, model, baseURL string, key auth.Credential) (int, bool) {
	model = c.StripProviderPrefix(ctx, model)
	server := serverRoot(baseURL)
	lmStudio := localhostToIPv4(lmStudioServerRoot(baseURL))
	if c.blackholed(server) {
		return 0, false
	}
	kind, detected := c.detectLocalServerType(ctx, baseURL, key)
	p, err := c.prober(ctx, baseURL, key, localPhases)
	if err != nil {
		return 0, false
	}
	var probes []func() (int, bool, error)
	switch {
	case detected && kind == ServerOllama:
		probes = append(probes, func() (int, bool, error) {
			resp, err := p.post(ctx, server+"/api/show", map[string]string{"name": model})
			if err != nil {
				return 0, false, err
			}
			if resp.status != http.StatusOK {
				return 0, false, nil
			}
			data, err := resp.value()
			if err != nil {
				return 0, false, err
			}
			window, ok := ollamaShowContext(data, false, math.MinInt)
			return window, ok, nil
		})
	case detected && kind == ServerLMStudio:
		probes = append(probes, func() (int, bool, error) { return lmStudioContext(ctx, p, lmStudio, model) })
	case detected && kind == ServerLlamaCpp:
		probes = append(probes, func() (int, bool, error) {
			window, ok := local.LlamaCppContext(ctx, p.client, p.header, server, model)
			return window, ok, nil
		})
	}
	probes = append(probes, func() (int, bool, error) {
		if !detected {
			return 0, false, nil
		}
		resp, err := p.get(ctx, server+"/v1/models/"+pathSegments(model))
		if err != nil {
			return 0, false, err
		}
		if resp.status != http.StatusOK {
			return 0, false, nil
		}
		payload, err := resp.value()
		if err != nil {
			return 0, false, err
		}
		window, ok := contextLengthFromModelPayload(payload)
		return window, ok, nil
	}, func() (int, bool, error) { return openAIModelsListContext(ctx, p, server, model) })
	for _, probe := range probes {
		window, ok, err := probe()
		if err != nil {
			c.noteIfConnectTimeout(err, server)
			return 0, false
		}
		if ok {
			return window, true
		}
	}
	return 0, false
}

func (c *Catalog) localContextLength(ctx context.Context, model, baseURL string, key auth.Credential) (int, bool) {
	memoKey := localProbeKey{model: c.StripProviderPrefix(ctx, model), baseURL: strings.TrimRight(baseURL, "/")}
	return c.memoLocalProbe(memoKey, func() (int, bool) { return c.localContextLengthUncached(ctx, model, baseURL, key) })
}
