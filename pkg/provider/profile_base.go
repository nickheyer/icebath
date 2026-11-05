package provider

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Mutex the goroutine holding it may lock again, released after the matching number of unlocks
type reentrantMutex struct {
	mu    sync.Mutex
	owner atomic.Uint64
	depth int
}

func (m *reentrantMutex) Lock() {
	id := goroutineID()
	if m.owner.Load() == id {
		m.depth++
		return
	}
	m.mu.Lock()
	m.owner.Store(id)
	m.depth = 1
}

func (m *reentrantMutex) Unlock() {
	m.depth--
	if m.depth == 0 {
		m.owner.Store(0)
		m.mu.Unlock()
	}
}

// One resolver for every lazy header map, reentrant so a callback may read another lazy map
var lazyHeadersResolveLock reentrantMutex

// HeaderEntry is one default header, Fn computing the value on first read when set
type HeaderEntry struct {
	Key   string
	Value string
	Fn    func() string
}

// LazyHeaders keeps default headers in insertion order and resolves callable values on first read
type LazyHeaders struct {
	mu      sync.RWMutex
	keys    []string
	vals    map[string]string
	fns     map[string]func() string
	pending atomic.Bool
}

// NewLazyHeaders stores every key up front and defers callable values until a read
func NewLazyHeaders(entries ...HeaderEntry) *LazyHeaders {
	h := &LazyHeaders{vals: map[string]string{}, fns: map[string]func() string{}}
	for _, e := range entries {
		if _, seen := h.vals[e.Key]; !seen {
			if _, seenFn := h.fns[e.Key]; !seenFn {
				h.keys = append(h.keys, e.Key)
			}
		}
		if e.Fn != nil {
			h.fns[e.Key] = e.Fn
			delete(h.vals, e.Key)
		} else {
			h.vals[e.Key] = e.Value
			delete(h.fns, e.Key)
		}
	}
	h.pending.Store(len(h.fns) > 0)
	return h
}

// Runs every pending callable once under the shared reentrant lock
func (h *LazyHeaders) resolve() *LazyHeaders {
	if h.pending.Load() {
		lazyHeadersResolveLock.Lock()
		defer lazyHeadersResolveLock.Unlock()
		if h.pending.Load() {
			h.mu.RLock()
			keys := append([]string(nil), h.keys...)
			fns := make(map[string]func() string, len(h.fns))
			for k, fn := range h.fns {
				fns[k] = fn
			}
			h.mu.RUnlock()
			for _, k := range keys {
				if fn, ok := fns[k]; ok {
					v := fn()
					h.mu.Lock()
					if _, still := h.fns[k]; still {
						h.vals[k] = v
						delete(h.fns, k)
					}
					h.mu.Unlock()
				}
			}
			h.pending.Store(false)
		}
	}
	return h
}

// Len counts the stored keys without resolving any value
func (h *LazyHeaders) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.keys)
}

// Contains reports key membership without resolving any value
func (h *LazyHeaders) Contains(key string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, v := h.vals[key]
	_, f := h.fns[key]
	return v || f
}

// Get returns the resolved value of key
func (h *LazyHeaders) Get(key string) (string, bool) {
	h.resolve()
	h.mu.RLock()
	defer h.mu.RUnlock()
	v, ok := h.vals[key]
	return v, ok
}

// GetDefault returns the resolved value of key or def when absent
func (h *LazyHeaders) GetDefault(key, def string) string {
	if v, ok := h.Get(key); ok {
		return v
	}
	return def
}

// Keys returns the keys in insertion order after resolving
func (h *LazyHeaders) Keys() []string {
	h.resolve()
	h.mu.RLock()
	defer h.mu.RUnlock()
	return append([]string(nil), h.keys...)
}

// Reversed returns the keys in reverse insertion order after resolving
func (h *LazyHeaders) Reversed() []string {
	keys := h.Keys()
	for i, j := 0, len(keys)-1; i < j; i, j = i+1, j-1 {
		keys[i], keys[j] = keys[j], keys[i]
	}
	return keys
}

// Values returns the resolved values in insertion order
func (h *LazyHeaders) Values() []string {
	items := h.Items()
	out := make([]string, len(items))
	for i, kv := range items {
		out[i] = kv[1]
	}
	return out
}

// Items returns resolved key value pairs in insertion order
func (h *LazyHeaders) Items() [][2]string {
	h.resolve()
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([][2]string, 0, len(h.keys))
	for _, k := range h.keys {
		out = append(out, [2]string{k, h.vals[k]})
	}
	return out
}

// Copy returns a plain resolved map
func (h *LazyHeaders) Copy() map[string]string {
	out := map[string]string{}
	for _, kv := range h.Items() {
		out[kv[0]] = kv[1]
	}
	return out
}

// Set stores a value after resolving, appending a new key at the end
func (h *LazyHeaders) Set(key, value string) {
	h.resolve()
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.vals[key]; !ok {
		h.keys = append(h.keys, key)
	}
	h.vals[key] = value
}

// Delete removes key after resolving, false when it was absent
func (h *LazyHeaders) Delete(key string) bool {
	_, ok := h.Pop(key)
	return ok
}

// Pop removes key and returns its resolved value
func (h *LazyHeaders) Pop(key string) (string, bool) {
	h.resolve()
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.vals[key]
	if !ok {
		return "", false
	}
	delete(h.vals, key)
	for i, k := range h.keys {
		if k == key {
			h.keys = append(h.keys[:i:i], h.keys[i+1:]...)
			break
		}
	}
	return v, true
}

// PopDefault removes key and returns its value, def when absent
func (h *LazyHeaders) PopDefault(key, def string) string {
	if v, ok := h.Pop(key); ok {
		return v
	}
	return def
}

// PopItem removes and returns the most recently inserted pair
func (h *LazyHeaders) PopItem() (string, string, bool) {
	h.resolve()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.keys) == 0 {
		return "", "", false
	}
	k := h.keys[len(h.keys)-1]
	v := h.vals[k]
	h.keys = h.keys[:len(h.keys)-1]
	delete(h.vals, k)
	return k, v, true
}

// SetDefault returns the value of key, storing def first when absent
func (h *LazyHeaders) SetDefault(key, def string) string {
	h.resolve()
	h.mu.Lock()
	defer h.mu.Unlock()
	if v, ok := h.vals[key]; ok {
		return v
	}
	h.keys = append(h.keys, key)
	h.vals[key] = def
	return def
}

// Update stores every pair in order after resolving
func (h *LazyHeaders) Update(pairs ...[2]string) {
	for _, kv := range pairs {
		h.Set(kv[0], kv[1])
	}
}

// Equal compares the resolved contents with another lazy or plain header map
func (h *LazyHeaders) Equal(other any) bool {
	var theirs map[string]string
	switch o := other.(type) {
	case *LazyHeaders:
		theirs = o.Copy()
	case map[string]string:
		theirs = o
	default:
		return false
	}
	mine := h.Copy()
	if len(mine) != len(theirs) {
		return false
	}
	for k, v := range mine {
		if tv, ok := theirs[k]; !ok || tv != v {
			return false
		}
	}
	return true
}

// Or returns a plain map of the resolved headers overlaid by other
func (h *LazyHeaders) Or(other map[string]string) map[string]string {
	out := h.Copy()
	for k, v := range other {
		out[k] = v
	}
	return out
}

// ROr returns a plain map of other overlaid by the resolved headers
func (h *LazyHeaders) ROr(other map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range other {
		out[k] = v
	}
	for k, v := range h.Copy() {
		out[k] = v
	}
	return out
}

// IOr merges other into the headers in place and returns them
func (h *LazyHeaders) IOr(other map[string]string) *LazyHeaders {
	for k, v := range other {
		h.Set(k, v)
	}
	return h
}

// String renders the resolved headers as a Python dict literal
func (h *LazyHeaders) String() string {
	var b strings.Builder
	b.WriteByte('{')
	for i, kv := range h.Items() {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(pyRepr(kv[0]) + ": " + pyRepr(kv[1]))
	}
	b.WriteByte('}')
	return b.String()
}

// Catalog probe user agent so WAF fronted endpoints do not reject the default client
func profileUserAgent() string {
	if strings.TrimSpace(Version) == "" {
		return "icebath-cli"
	}
	return "icebath-cli/" + Version
}

// Fixed temperature of the profile, omit true for the server managed sentinel
func (p Profile) TemperatureOverride() (value *float64, omit bool) {
	if p.OmitTemperature {
		return nil, true
	}
	return p.FixedTemperature, false
}

// Resolved default headers, the lazy map winning over the plain one when set
func (p Profile) DefaultHeaders() map[string]string {
	if p.LazyHeaders != nil {
		return p.LazyHeaders.Copy()
	}
	out := make(map[string]string, len(p.Headers))
	for k, v := range p.Headers {
		out[k] = v
	}
	return out
}

// ModelAliasMap returns the provider native model alias table
func (p Profile) ModelAliasMap() map[string]string { return p.ModelAliases }

// ResponsesReplayItemIDs reports whether replayed Responses message items keep their id
func (p Profile) ResponsesReplayItemIDs() bool { return !p.ResponsesDropReplayItemIDs }

// ResponsesPromptCacheKey reports whether the Responses body carries prompt_cache_key
func (p Profile) ResponsesPromptCacheKey() bool { return !p.ResponsesOmitPromptCacheKey }

// ResponsesIssuerKind is the persisted stamp for encrypted reasoning minted here, empty means URL derived
func (p Profile) ResponsesIssuerKind() string { return p.ResponsesIssuer }

// ResponsesTrustsCompletedStatus is false when a reasoning only completed response continues
func (p Profile) ResponsesTrustsCompletedStatus() bool { return !p.ResponsesDistrustCompletedStatus }

// ResponsesNativeCompaction is false when context_management must never be sent
func (p Profile) ResponsesNativeCompaction() bool { return !p.ResponsesNoNativeCompaction }

// ResponsesProfileReasoning is true when reasoning comes from the profile payload plus summary without include
func (p Profile) ResponsesProfileReasoning() bool { return p.ResponsesReasoningFromProfile }

// FetchAccountUsage returns the provider account usage snapshot, nil when the profile reports none
func (p Profile) FetchAccountUsage(ctx context.Context, baseURL, apiKey string) (*AccountUsageSnapshot, error) {
	if p.FetchAccountUsageHook != nil {
		return p.FetchAccountUsageHook(ctx, baseURL, apiKey)
	}
	return nil, nil
}

// ResolveAuxModel returns the cheap auxiliary model, empty for the generic choice
func (p Profile) ResolveAuxModel(vision bool) string {
	if p.ResolveAuxModelHook != nil {
		return p.ResolveAuxModelHook(vision)
	}
	return ""
}

// GetHostname returns the declared hostname, else the base url host, else empty
func (p Profile) GetHostname() string {
	if p.GetHostnameHook != nil {
		return p.GetHostnameHook()
	}
	if p.Hostname != "" {
		return p.Hostname
	}
	if p.BaseURL != "" {
		if u, err := url.Parse(p.BaseURL); err == nil {
			return strings.ToLower(u.Hostname())
		}
	}
	return ""
}

// PrepareMessages rewrites outgoing messages, unchanged by default
func (p Profile) PrepareMessages(messages []map[string]any) []map[string]any {
	if p.PrepareMessagesHook != nil {
		return p.PrepareMessagesHook(messages)
	}
	return messages
}

// BuildExtraBody returns provider specific extra_body fields, empty by default
func (p Profile) BuildExtraBody(sessionID string, context map[string]any) map[string]any {
	if p.BuildExtraBodyHook != nil {
		return p.BuildExtraBodyHook(sessionID, context)
	}
	return map[string]any{}
}

// DefaultReasoningConfig returns the reasoning config applied when the user set none
func (p Profile) DefaultReasoningConfig(model string) *Reasoning {
	if p.DefaultReasoningConfigHook != nil {
		return p.DefaultReasoningConfigHook(model)
	}
	return nil
}

// BuildAPIKwargsExtras returns extra_body and top level request fields, both empty by default
func (p Profile) BuildAPIKwargsExtras(reasoning *Reasoning, context map[string]any) (map[string]any, map[string]any) {
	if p.BuildAPIKwargsExtrasHook != nil {
		return p.BuildAPIKwargsExtrasHook(reasoning, context)
	}
	return map[string]any{}, map[string]any{}
}

// BuildClientKwargsExtras returns client constructor extras, empty by default
func (p Profile) BuildClientKwargsExtras(context map[string]any) map[string]any {
	if p.BuildClientKwargsExtrasHook != nil {
		return p.BuildClientKwargsExtrasHook(context)
	}
	return map[string]any{}
}

// DefaultVisionModel returns the provider vision model, empty for none
func (p Profile) DefaultVisionModel() string {
	if p.DefaultVisionModelHook != nil {
		return p.DefaultVisionModelHook()
	}
	return ""
}

// GetModelContextLength returns the provider declared context window, zero for unknown
func (p Profile) GetModelContextLength(model string) int {
	if p.GetModelContextLengthHook != nil {
		return p.GetModelContextLengthHook(model)
	}
	return 0
}

// GetUsageCost prices usage with the provider's own table, false for the generic pricing
func (p Profile) GetUsageCost(model string, usage CanonicalUsage) (CostResult, bool) {
	if p.GetUsageCostHook != nil {
		return p.GetUsageCostHook(model, usage)
	}
	return CostResult{}, false
}

// GetMaxTokens returns the output cap for a model, the profile default when not overridden
func (p Profile) GetMaxTokens(model string) int {
	if p.GetMaxTokensHook != nil {
		return p.GetMaxTokensHook(model)
	}
	return p.DefaultMaxTokens
}

// SupportedReasoningEfforts returns the efforts the model accepts, false when the profile declares none
func (p Profile) SupportedReasoningEfforts(model string) ([]string, bool) {
	if p.SupportedReasoningEffortsHook != nil {
		return p.SupportedReasoningEffortsHook(model)
	}
	return nil, false
}

// APIModeForModel returns the per model wire, empty for the profile mode
func (p Profile) APIModeForModel(model, apiKey string) APIMode {
	if p.APIModeForModelHook != nil {
		return p.APIModeForModelHook(model, apiKey)
	}
	if p.PerModelMode != nil {
		return p.PerModelMode(model)
	}
	return ""
}

// ClientHeaders returns client headers for the endpoint, nil for the default headers
func (p Profile) ClientHeaders(baseURL, apiKey string, hasImages bool) map[string]string {
	if p.ClientHeadersHook != nil {
		return p.ClientHeadersHook(baseURL, apiKey, hasImages)
	}
	return nil
}

// RequestHeaders returns per request headers, empty by default
func (p Profile) RequestHeaders(baseURL, model string, userInitiated bool) map[string]string {
	if p.RequestHeadersHook != nil {
		return p.RequestHeadersHook(baseURL, model, userInitiated)
	}
	return map[string]string{}
}

// MaxTokensParam names the output cap field, empty for the transport default
func (p Profile) MaxTokensParam(baseURL, model string) string {
	if p.MaxTokensParamHook != nil {
		return p.MaxTokensParamHook(baseURL, model)
	}
	return ""
}

// Listed reports whether discovery surfaces may offer the provider, hidden ones once a pool row exists
func (p Profile) Listed(ctx context.Context) (bool, error) {
	if p.ListedHook != nil {
		return p.ListedHook(ctx)
	}
	if !p.Hidden {
		return true, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := NewStore(home.Dir(ctx)).ReadCredentialPool(p.Name)
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}

// CreateClient returns a provider specific client, nil for the standard construction path
func (p Profile) CreateClient(ctx context.Context, clientKwargs map[string]any) (Client, error) {
	if p.CreateClientHook != nil {
		return p.CreateClientHook(ctx, clientKwargs)
	}
	return nil, nil
}

// SetupStatus returns the external process login status, nil when there is nothing to report
func (p Profile) SetupStatus(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
	if p.SetupStatusHook != nil {
		return p.SetupStatusHook(ctx, kwargs)
	}
	return nil, nil
}

// DiscoverModels returns the account's live picker rows, nil to fall back to the fallback models
func (p Profile) DiscoverModels(ctx context.Context, kwargs map[string]any) ([]map[string]any, error) {
	if p.DiscoverModelsHook != nil {
		return p.DiscoverModelsHook(ctx, kwargs)
	}
	return nil, nil
}

// FetchModels lists live model ids from the models endpoint, false when the fetch failed or listing is off
func (p Profile) FetchModels(ctx context.Context, apiKey, baseURL string, timeout time.Duration) ([]string, bool) {
	if p.FetchModelsHook != nil {
		return p.FetchModelsHook(ctx, apiKey, baseURL, timeout)
	}
	if !p.ModelListing {
		return nil, false
	}
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	callerBase := strings.TrimSpace(baseURL)
	effectiveBase := callerBase
	if effectiveBase == "" {
		effectiveBase = p.BaseURL
	}
	customBase := callerBase != "" && strings.TrimRight(callerBase, "/") != strings.TrimRight(p.BaseURL, "/")
	var target string
	if customBase {
		target = strings.TrimRight(callerBase, "/") + "/models"
	} else {
		target = strings.TrimSpace(p.ModelsURL)
		if target == "" {
			if effectiveBase == "" {
				return nil, false
			}
			target = strings.TrimRight(effectiveBase, "/") + "/models"
		}
	}
	headers := map[string]string{}
	if apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}
	headers["Accept"] = "application/json"
	headers["User-agent"] = profileUserAgent()
	for k, v := range p.DefaultHeaders() {
		headers[canonicalHeaderKey(k)] = v
	}
	data, err := modelsGetJSON(ctx, target, timeout, headers, nil)
	if err != nil {
		slog.Debug("fetch_models(" + p.Name + "): " + err.Error())
		return nil, false
	}
	items := data
	if obj, ok := data.(map[string]any); ok {
		items, ok = obj["data"]
		if !ok {
			items = []any{}
		}
	} else if _, ok := data.([]any); !ok {
		slog.Debug("fetch_models(" + p.Name + "): payload is neither a list nor an object")
		return nil, false
	}
	return ChatCatalogIDs(items), true
}

// Header key in the capitalized form urllib stores so a profile header replaces the generic one
func canonicalHeaderKey(k string) string {
	if k == "" {
		return k
	}
	return strings.ToUpper(k[:1]) + strings.ToLower(k[1:])
}

// LiveContextLength returns the context window from the live credentialed catalog, zero for none
func (p Profile) LiveContextLength(ctx context.Context, model, apiKey, baseURL string) int {
	if p.LiveContextLengthHook != nil {
		return p.LiveContextLengthHook(ctx, model, apiKey, baseURL)
	}
	return 0
}

// NormalizeModelID returns the provider native id, empty for the generic normalization
func (p Profile) NormalizeModelID(model, apiKey string) string {
	if p.NormalizeModelIDHook != nil {
		return p.NormalizeModelIDHook(model, apiKey)
	}
	return ""
}

// ResolveAPIKeySecret returns the provider owned runtime key and source, false for the generic ladder
func (p Profile) ResolveAPIKeySecret() (string, string, bool) {
	if p.ResolveAPIKeySecretHook != nil {
		return p.ResolveAPIKeySecretHook()
	}
	return "", "", false
}

// RuntimeBaseURL returns the credential derived base url, empty for the env or default url
func (p Profile) RuntimeBaseURL(apiKey, def, envURL string) string {
	if p.RuntimeBaseURLHook != nil {
		return p.RuntimeBaseURLHook(apiKey, def, envURL)
	}
	return ""
}

// PreparePoolEntry turns the selected pool row secret into the runtime key, unchanged by default
func (p Profile) PreparePoolEntry(entry *PooledCredential, apiKey string) string {
	if p.PreparePoolEntryHook != nil {
		return p.PreparePoolEntryHook(entry, apiKey)
	}
	return apiKey
}

// SeedPoolCredentials returns rows the provider seeds itself, nil keeps generic env seeding
func (p Profile) SeedPoolCredentials(suppressedSources map[string]bool, explicitlyConfigured bool) []map[string]any {
	if p.SeedPoolCredentialsHook != nil {
		return p.SeedPoolCredentialsHook(suppressedSources, explicitlyConfigured)
	}
	return nil
}

// RemoveCredentialSource returns sources to suppress and hints, false for the generic removal
func (p Profile) RemoveCredentialSource(source string) ([]string, []string, bool) {
	if p.RemoveCredentialSourceHook != nil {
		return p.RemoveCredentialSourceHook(source)
	}
	return nil, nil, false
}

// RefreshRuntimeCredentials re-mints the live client key and base url, false when nothing fresher exists
func (p Profile) RefreshRuntimeCredentials(ctx context.Context, reason string) (string, string, bool, error) {
	if p.RefreshRuntimeCredentialsHook != nil {
		return p.RefreshRuntimeCredentialsHook(ctx, reason)
	}
	return "", "", false, nil
}

// IsStaleCredentialError reports whether a non auth error means the credential went stale
func (p Profile) IsStaleCredentialError(statusCode int, message string) bool {
	if p.IsStaleCredentialErrorHook != nil {
		return p.IsStaleCredentialErrorHook(statusCode, message)
	}
	return false
}

// CredentialRefreshNotice returns the status line after a refresh, empty for the generic wording
func (p Profile) CredentialRefreshNotice(reason string) string {
	if p.CredentialRefreshNoticeHook != nil {
		return p.CredentialRefreshNoticeHook(reason)
	}
	return ""
}

// RequestRejectionHint returns lines explaining an endpoint's hard rejection of any route
func (p Profile) RequestRejectionHint(statusCode int, baseURL string) []string {
	if p.RequestRejectionHintHook != nil {
		return p.RequestRejectionHintHook(statusCode, baseURL)
	}
	return nil
}

// ExternalAuthEvidence returns subprocess free login evidence, false when the binary decides
func (p Profile) ExternalAuthEvidence() (bool, string, bool) {
	if p.ExternalAuthEvidenceHook != nil {
		return p.ExternalAuthEvidenceHook()
	}
	return false, "", false
}

// MissingCredentialsHint returns the sentence appended to missing credential messages, empty for none
func (p Profile) MissingCredentialsHint() string {
	if p.MissingCredentialsHintHook != nil {
		return p.MissingCredentialsHintHook()
	}
	return ""
}

// ReasoningPayload returns the verbatim reasoning object the endpoint takes, nil for none
func (p Profile) ReasoningPayload(model string, r Reasoning, baseURL string) map[string]any {
	if p.ReasoningPayloadHook != nil {
		return p.ReasoningPayloadHook(model, r, baseURL)
	}
	return nil
}

// ReplayMessageCarriers returns captured carriers to put back on a replayed assistant message
func (p Profile) ReplayMessageCarriers(values map[string]any, model, baseURL string) map[string]any {
	if p.ReplayMessageCarriersHook != nil {
		return p.ReplayMessageCarriersHook(values, model, baseURL)
	}
	return map[string]any{}
}

// ReplaysFunctionThoughtSignature reports whether replayed tool calls keep function thought signatures
func (p Profile) ReplaysFunctionThoughtSignature(model, baseURL string) bool {
	if p.ReplaysFunctionThoughtSignatureHook != nil {
		return p.ReplaysFunctionThoughtSignatureHook(model, baseURL)
	}
	return false
}

// ProbeModels answers a models probe from the provider catalog, nil for the generic HTTP probe
func (p Profile) ProbeModels(ctx context.Context, baseURL, apiKey string, timeout time.Duration) *ModelsProbe {
	if p.ProbeModelsHook != nil {
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		return p.ProbeModelsHook(ctx, baseURL, apiKey, timeout)
	}
	return nil
}

// InvalidateCatalogCache drops in process catalog memos, nothing by default
func (p Profile) InvalidateCatalogCache() {
	if p.InvalidateCatalogCacheHook != nil {
		p.InvalidateCatalogCacheHook()
	}
}

// Profiles overriding runtime credential refresh, the one named by the route provider then the one serving its host
func refreshRouteProfiles(ctx context.Context, provider, baseURL string) []*Profile {
	var out []*Profile
	named := (*Profile)(nil)
	if provider = strings.ToLower(strings.TrimSpace(provider)); provider != "" {
		named = getProviderProfile(ctx, provider)
	}
	if named != nil && named.RefreshRuntimeCredentialsHook != nil {
		out = append(out, named)
	}
	if baseURL != "" {
		if hosted := profileForHostCtx(ctx, baseURL, false); hosted != nil && hosted.RefreshRuntimeCredentialsHook != nil &&
			(named == nil || hosted.Name != named.Name) {
			out = append(out, hosted)
		}
	}
	return out
}

var errorPathWarned sync.Map

// Asks the route's credential refresh profile whether a non auth error means a stale credential, a panicking hook answers false
func routeProfileIsStaleCredentialError(ctx context.Context, route Route, status int, message string) (stale bool) {
	profiles := refreshRouteProfiles(ctx, route.Provider, route.BaseURL)
	if len(profiles) == 0 {
		return false
	}
	p := profiles[0]
	defer func() {
		if r := recover(); r != nil {
			if _, seen := errorPathWarned.LoadOrStore([2]string{p.Name, "is_stale_credential_error"}, true); !seen {
				slog.Warn(fmt.Sprintf("provider %s: %s hook failed; handling the API error without it", p.Name, "is_stale_credential_error"), "panic", r)
			}
			stale = false
		}
	}()
	return p.IsStaleCredentialError(status, message)
}

// RequestRejectionHints gathers every registered profile's rejection lines for an endpoint whatever route reached it
func RequestRejectionHints(ctx context.Context, statusCode int, baseURL string) []string {
	var out []string
	for _, p := range ListProviders(ctx) {
		out = append(out, p.RequestRejectionHint(statusCode, baseURL)...)
	}
	return out
}
