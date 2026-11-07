package provider

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/home"
)

// Auxiliary client cache key, the hermes tuple with every composite slot rendered secret free
type auxClientCacheKey struct {
	Home     string
	Provider string
	Async    bool
	BaseURL  string
	APIKey   string
	APIMode  string
	Runtime  string
	Vision   bool
	Task     string
	PoolHint string
	Model    string
	Borrowed string
}

// Cached client with its default model and the event loop it was bound to
type auxCachedClient struct {
	Client       AuxClient
	DefaultModel string
	BoundLoop    any
	Retained     []any
}

// Oldest entries are evicted once the cache grows past this size
const auxClientCacheMaxSize = 64

var (
	auxClientCacheMu    sync.Mutex
	auxClientCache      = map[auxClientCacheKey]auxCachedClient{}
	auxClientCacheOrder []auxClientCacheKey
)

// Cache keys in insertion order, the caller holds the cache lock
func auxClientCacheKeysLocked() []auxClientCacheKey {
	return slices.Clone(auxClientCacheOrder)
}

// Stores an entry keeping the slot of an existing key, the caller holds the cache lock
func auxClientCacheSetLocked(key auxClientCacheKey, entry auxCachedClient) {
	if _, ok := auxClientCache[key]; !ok {
		auxClientCacheOrder = append(auxClientCacheOrder, key)
	}
	auxClientCache[key] = entry
}

// Removes an entry and its order slot, the caller holds the cache lock
func auxClientCacheDeleteLocked(key auxClientCacheKey) {
	if _, ok := auxClientCache[key]; !ok {
		return
	}
	delete(auxClientCache, key)
	if i := slices.Index(auxClientCacheOrder, key); i >= 0 {
		auxClientCacheOrder = slices.Delete(auxClientCacheOrder, i, i+1)
	}
}

// Identity comparison of two values, pointer identity for reference kinds and equality for comparable values
func auxSameObject(a, b any) bool {
	if a == nil || b == nil {
		return false
	}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if va.Type() != vb.Type() {
		return false
	}
	switch va.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Chan, reflect.Func, reflect.UnsafePointer, reflect.Slice:
		return va.Pointer() == vb.Pointer()
	}
	return va.Type().Comparable() && a == b
}

// Drops cache entries whose client or its real client is the target, true when any was dropped
func auxEvictCachedClientInstance(target any) bool {
	if target == nil {
		return false
	}
	if v := reflect.ValueOf(target); v.Kind() == reflect.Pointer && v.IsNil() {
		return false
	}
	evicted := false
	auxClientCacheMu.Lock()
	defer auxClientCacheMu.Unlock()
	for _, key := range auxClientCacheKeysLocked() {
		cached := auxClientCache[key].Client
		if cached == nil {
			continue
		}
		var real any
		if holder, ok := cached.(interface{ auxRealClient() AuxClient }); ok {
			if rc := holder.auxRealClient(); rc != nil {
				real = rc
			}
		}
		if auxSameObject(cached, target) || auxSameObject(real, target) {
			auxClientCacheDeleteLocked(key)
			evicted = true
		}
	}
	return evicted
}

// Event loop an async cached client is bound to, stale once it reports closed
type auxBoundLoop interface {
	IsClosed() bool
}

// Client whose close hands back an awaitable bound to the loop owning its transport
type auxAwaitableCloser interface {
	CloseAwaitable() func() error
}

// Client able to mark its transport closed without draining it on the owner loop
type auxTransportNeuterer interface {
	ForceCloseTransport()
}

// Marks the transport inside an async client closed so a dead loop is never asked to drain it
func auxForceCloseAsyncTransport(client AuxClient) {
	if n, ok := client.(auxTransportNeuterer); ok {
		_ = auxRecoverRaise(n.ForceCloseTransport)
	}
}

// Closes one cached client, awaiting an async close only when its owner loop is closed or current
func auxCloseCachedClient(client AuxClient, closeAsync bool) {
	if client == nil {
		return
	}
	if ac, ok := client.(auxAwaitableCloser); ok {
		var await func() error
		if err := auxRecoverRaise(func() { await = ac.CloseAwaitable() }); err != nil || await == nil {
			auxForceCloseAsyncTransport(client)
			return
		}
		if closeAsync {
			_ = auxRecoverRaise(func() { _ = await() })
		}
		auxForceCloseAsyncTransport(client)
		return
	}
	_ = auxRecoverRaise(func() { _ = client.Close() })
	auxForceCloseAsyncTransport(client)
}

// CleanupStaleAsyncClients drops and closes every cached async client whose bound loop has closed
func CleanupStaleAsyncClients() {
	auxClientCacheMu.Lock()
	var stale []AuxClient
	for _, k := range auxClientCacheKeysLocked() {
		entry := auxClientCache[k]
		if loop, ok := entry.BoundLoop.(auxBoundLoop); ok && loop.IsClosed() {
			stale = append(stale, entry.Client)
			auxClientCacheDeleteLocked(k)
		}
	}
	auxClientCacheMu.Unlock()
	for _, client := range stale {
		auxCloseCachedClient(client, true)
	}
}

// Closure identity of a callable credential, false for anything not a func
func auxCallableIdentity(v any) (uintptr, bool) {
	if v == nil || reflect.TypeOf(v).Kind() != reflect.Func || reflect.ValueOf(v).IsNil() {
		return 0, false
	}
	return (*[2]uintptr)(unsafe.Pointer(&v))[1], true
}

// Secret safe cache key slot of a runtime field, callables keyed by identity and string keys by digest
func auxRuntimeCacheDiscriminator(field string, value any) (string, []any) {
	if field == "api_key" {
		if id, ok := auxCallableIdentity(value); ok {
			return "<callable-api-key>#" + strconv.FormatUint(uint64(id), 16), []any{value}
		}
		if s, ok := value.(string); ok && s != "" {
			return "api-key-digest:" + hex.EncodeToString(blake2bSum([]byte(s), 16)), nil
		}
	}
	switch v := value.(type) {
	case nil:
		return "None", nil
	case string:
		return strconv.Quote(v), nil
	}
	return fmt.Sprintf("%#v", value), nil
}

// Tuple rendering of cache key slots
func auxCacheTuple(parts ...string) string {
	return "(" + strings.Join(parts, ",") + ")"
}

// Client cache key of a request with the callables its slots must keep alive while cached
func auxClientCacheKeyFor(ctx context.Context, provider string, asyncMode bool, baseURL string, apiKey any, apiMode string, mainRuntime map[string]any, isVision bool, task, model string) (auxClientCacheKey, []any, error) {
	runtime := auxNormalizeMainRuntime(ctx, mainRuntime)
	var retained []any
	key := auxClientCacheKey{Home: home.Key(home.Dir(ctx)), Provider: provider, Async: asyncMode, BaseURL: baseURL, APIMode: apiMode, Vision: isVision}
	key.Runtime = auxCacheTuple()
	if provider == "auto" {
		var parts []string
		for _, f := range auxMainRuntimeFields {
			v, ok := runtime[f]
			if !ok {
				v = ""
			}
			slot, keep := auxRuntimeCacheDiscriminator(f, v)
			parts, retained = append(parts, slot), append(retained, keep...)
		}
		key.Runtime = auxCacheTuple(parts...)
		fast, err := auxTaskPrefersFastModel(ctx, task)
		if err != nil {
			return auxClientCacheKey{}, nil, err
		}
		key.Task = auxCacheTuple(strconv.Quote(task), auxPyBool(fast))
	}
	key.PoolHint = auxPoolCacheHint(ctx, provider, mainRuntime)
	key.Model = model
	if key.Model == "" {
		key.Model, _ = runtime["model"].(string)
	}
	keyArg := apiKey
	if !auxTruthy(keyArg) {
		keyArg = ""
	}
	slot, keep := auxRuntimeCacheDiscriminator("api_key", keyArg)
	key.APIKey, retained = slot, append(retained, keep...)
	borrowed, keep, err := auxBorrowedMainCredentialKey(ctx, provider, baseURL, apiKey, runtime)
	if err != nil {
		return auxClientCacheKey{}, nil, err
	}
	key.Borrowed, retained = borrowed, append(retained, keep...)
	return key, retained, nil
}

// Python bool spelling
func auxPyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

// Main runtime credential a keyless custom route borrows when its client is built, an empty tuple otherwise
func auxBorrowedMainCredentialKey(ctx context.Context, provider, baseURL string, apiKey any, runtime map[string]any) (string, []any, error) {
	if auxNormalizeAuxProvider(ctx, provider) != "custom" {
		return auxCacheTuple(), nil, nil
	}
	if baseURL == "" {
		base, ok := runtime["base_url"]
		if !ok {
			base = ""
		}
		key, ok := runtime["api_key"]
		if !ok {
			key = ""
		}
		baseSlot, _ := auxRuntimeCacheDiscriminator("base_url", base)
		keySlot, keep := auxRuntimeCacheDiscriminator("api_key", key)
		return auxCacheTuple(baseSlot, keySlot), keep, nil
	}
	if auxTruthy(auxNormalizeAPIKey(apiKey)) {
		return auxCacheTuple(), nil, nil
	}
	borrowed, err := auxReadMainAPIKeyIfSameOrigin(ctx, strings.TrimSpace(auxToOpenAIBaseURL(baseURL)))
	if err != nil {
		return "", nil, err
	}
	slot, keep := auxRuntimeCacheDiscriminator("api_key", borrowed)
	return auxCacheTuple(slot), keep, nil
}

type auxEventLoopKey struct{}

// WithAuxiliaryEventLoop binds the event loop async auxiliary clients built under the context belong to
func WithAuxiliaryEventLoop(ctx context.Context, loop interface{ IsClosed() bool }) context.Context {
	return context.WithValue(ctx, auxEventLoopKey{}, loop)
}

// Event loop bound to the context, nil when none is bound
func auxCurrentEventLoop(ctx context.Context) any {
	if ctx == nil {
		return nil
	}
	if loop, ok := ctx.Value(auxEventLoopKey{}).(auxBoundLoop); ok && loop != nil {
		return loop
	}
	return nil
}

// Stores a client under a key closing the different client it replaces, probe clients never cached
func auxStoreCachedClient(key auxClientCacheKey, client AuxClient, defaultModel string, boundLoop any, retained []any) {
	if _, probe := client.(*auxProbeClient); probe {
		return
	}
	auxClientCacheMu.Lock()
	defer auxClientCacheMu.Unlock()
	if old, ok := auxClientCache[key]; ok && !auxSameObject(old.Client, client) {
		auxCloseCachedClient(old.Client, false)
	}
	auxClientCacheSetLocked(key, auxCachedClient{Client: client, DefaultModel: defaultModel, BoundLoop: boundLoop, Retained: retained})
}

// Nous refresh request, the lookup model and task naming the cache entry the stale client came from
type auxNousRefreshRequest struct {
	CacheProvider string
	Model         string
	AsyncMode     bool
	BaseURL       string
	APIKey        any
	APIMode       string
	MainRuntime   map[string]any
	IsVision      bool
	LookupModel   string
	LookupTask    string
}

// Refreshes Nous runtime credentials and replaces the stale cache entry with a rebuilt client
func auxRefreshNousAuxiliaryClient(ctx context.Context, req auxNousRefreshRequest) (AuxClient, string, error) {
	stale, _ := req.APIKey.(string)
	freshKey, freshBaseURL, ok := auxResolveNousRuntimeAPIFn(ctx, true, stale)
	if !ok {
		return nil, req.Model, nil
	}
	syncClient := auxCreateOpenAIClient(ctx, freshKey, freshBaseURL, auxOpenAIOptions{})
	var currentLoop any
	client, finalModel := syncClient, req.Model
	if req.AsyncMode {
		currentLoop = auxCurrentEventLoop(ctx)
		client, finalModel = auxToAsyncClient(ctx, syncClient, req.Model, req.IsVision)
	}
	key, retained, err := auxClientCacheKeyFor(ctx, req.CacheProvider, req.AsyncMode, req.BaseURL, req.APIKey, req.APIMode, req.MainRuntime, req.IsVision, req.LookupTask, req.LookupModel)
	if err != nil {
		return nil, "", err
	}
	auxStoreCachedClient(key, client, finalModel, currentLoop, retained)
	return client, finalModel, nil
}

// ShutdownCachedClients closes and clears every cached client outside the lock, a live foreign loop's clients only neutered
func ShutdownCachedClients(ctx context.Context) {
	auxClientCacheMu.Lock()
	var entries []auxCachedClient
	for _, k := range auxClientCacheKeysLocked() {
		if e := auxClientCache[k]; e.Client != nil {
			entries = append(entries, e)
		}
	}
	clear(auxClientCache)
	auxClientCacheOrder = nil
	auxClientCacheMu.Unlock()
	running := auxCurrentEventLoop(ctx)
	for _, e := range entries {
		loop, _ := e.BoundLoop.(auxBoundLoop)
		auxCloseCachedClient(e.Client, loop != nil && (loop.IsClosed() || auxSameObject(e.BoundLoop, running)))
	}
}

// Slash bearing model kept only for clients that accept vendor prefixed ids, else the cached default
func auxCompatModel(client AuxClient, model, cachedDefault string) string {
	if model != "" && strings.Contains(model, "/") {
		accepts := false
		objs := []AuxClient{client}
		if holder, ok := client.(interface{ auxRealClient() AuxClient }); ok {
			objs = append(objs, holder.auxRealClient())
		}
		for _, obj := range objs {
			if obj != nil && !(reflect.ValueOf(obj).Kind() == reflect.Pointer && reflect.ValueOf(obj).IsNil()) && baseURLHostMatches(obj.BaseURL(), "openrouter.ai") {
				accepts = true
			}
		}
		if !accepts && !strings.Contains(cachedDefault, "/") {
			return cachedDefault
		}
	}
	if model != "" {
		return model
	}
	return cachedDefault
}

// Cached client request, empty strings for None
type auxCachedClientRequest struct {
	Provider    string
	Model       string
	AsyncMode   bool
	BaseURL     string
	APIKey      any
	APIMode     string
	MainRuntime map[string]any
	IsVision    bool
	Task        string
}

// Cached client acquisition, swapped by tests the way hermes tests patch the module function
var auxGetCachedClientFn = auxGetCachedClient

// Cached client for a provider config built on a miss, async hits valid only on the open loop they were built on
func auxGetCachedClient(ctx context.Context, req auxCachedClientRequest) (AuxClient, string, error) {
	provider, baseURL, apiKey := req.Provider, req.BaseURL, req.APIKey
	keyText, _ := apiKey.(string)
	localBase, localKey, isLocal, err := auxBareLlamacppEndpoint(ctx, provider, baseURL, keyText)
	if err != nil {
		return nil, "", err
	}
	if isLocal {
		if localBase == "" {
			return nil, "", nil
		}
		baseURL, apiKey = localBase, localKey
	}
	var currentLoop any
	if req.AsyncMode {
		currentLoop = auxCurrentEventLoop(ctx)
	}
	runtime := auxNormalizeMainRuntime(ctx, req.MainRuntime)
	key, retained, err := auxClientCacheKeyFor(ctx, provider, req.AsyncMode, baseURL, apiKey, req.APIMode, req.MainRuntime, req.IsVision, req.Task, req.Model)
	if err != nil {
		return nil, "", err
	}
	auxClientCacheMu.Lock()
	if entry, ok := auxClientCache[key]; ok {
		loop, _ := entry.BoundLoop.(auxBoundLoop)
		if !req.AsyncMode || (loop != nil && auxSameObject(entry.BoundLoop, currentLoop) && !loop.IsClosed()) {
			auxClientCacheMu.Unlock()
			return entry.Client, auxCompatModel(entry.Client, req.Model, entry.DefaultModel), nil
		}
		auxCloseCachedClient(entry.Client, loop != nil && loop.IsClosed())
		auxClientCacheDeleteLocked(key)
	}
	auxClientCacheMu.Unlock()
	effectiveKey := apiKey
	if !auxTruthy(effectiveKey) {
		pool, perr := auxCachedClientPool(ctx, provider)
		if perr != nil {
			return nil, "", perr
		}
		if pool != nil {
			if entry := auxPeekPoolEntry(ctx, provider, pool); entry != nil {
				if k := auxPoolRuntimeAPIKey(entry); k != "" {
					effectiveKey = k
				}
			}
		}
	}
	client, defaultModel, err := auxResolveProviderClientFn(ctx, provider, req.Model, ProviderClientOptions{
		AsyncMode: req.AsyncMode, ExplicitBaseURL: baseURL, ExplicitAPIKey: effectiveKey, APIMode: req.APIMode,
		MainRuntime: runtime, IsVision: req.IsVision, Task: req.Task,
	})
	if err != nil {
		return nil, "", err
	}
	if client != nil && auxProbeActive(ctx) {
		return client, auxFirstNonEmpty(req.Model, defaultModel), nil
	}
	if client != nil {
		auxClientCacheMu.Lock()
		if entry, ok := auxClientCache[key]; !ok {
			for len(auxClientCache) >= auxClientCacheMaxSize {
				auxClientCacheDeleteLocked(auxClientCacheOrder[0])
			}
			auxClientCacheSetLocked(key, auxCachedClient{Client: client, DefaultModel: defaultModel, BoundLoop: currentLoop, Retained: retained})
		} else {
			built := client
			client, defaultModel = entry.Client, entry.DefaultModel
			auxCloseCachedClient(built, req.AsyncMode)
		}
		auxClientCacheMu.Unlock()
	}
	return client, auxCompatModel(client, req.Model, defaultModel), nil
}

// Pool a cached client miss derives its key from, entry scoped for a pinned custom name
func auxCachedClientPool(ctx context.Context, provider string) (*Pool, error) {
	var cfg map[string]any
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(provider)), "custom:") {
		tree, err := auxLoadConfigFn(ctx)
		if err != nil {
			return nil, err
		}
		cfg = tree
	}
	return PoolForAuxProvider(provider, auxNormalizeAuxProvider(ctx, provider), cfg, func(k string) *Pool {
		return auxLoadPoolWithCredentials(ctx, k, " (peek)")
	}), nil
}

// Explicit provider moa resolved to its preset aggregator slot, unchanged when the preset does not resolve
func auxUnwrapMoaProvider(ctx context.Context, prov, mdl string) (string, string) {
	if strings.ToLower(strings.TrimSpace(prov)) != "moa" {
		return prov, mdl
	}
	if aggProvider, aggModel := auxResolveMoaAggregator(ctx, mdl); aggProvider != "" && aggModel != "" {
		return aggProvider, aggModel
	}
	return prov, mdl
}

// True when a first class provider keeps its identity alongside an explicit base URL
func auxPreserveProviderWithBaseURL(ctx context.Context, prov string) bool {
	normalized := strings.ToLower(strings.TrimSpace(prov))
	if normalized == "" || normalized == "auto" || normalized == "custom" || strings.HasPrefix(normalized, "custom:") {
		return false
	}
	if localServerAliases[normalized] {
		return true
	}
	return auxBuiltinProviderPresent(ctx, normalized) || auxNamedCustomProviderPresent(ctx, normalized)
}

// Catalog lookup seam of the builtin provider check, a raised error reading as an unavailable catalog
var auxGetProviderDefFn = func(ctx context.Context, name string) *ProviderDef { return GetProviderDef(ctx, name, true) }

// True for a builtin provider, falling back to the provider backed routes when the catalog raises
func auxBuiltinProviderPresent(ctx context.Context, name string) bool {
	var def *ProviderDef
	if err := auxRecoverRaise(func() { def = auxGetProviderDefFn(ctx, name) }); err != nil {
		switch name {
		case "anthropic", "minimax-oauth", "nous", "openai-codex", "qwen-oauth", "xai-oauth":
			return true
		}
		return auxProfileCredentialsBypassCustom(ctx, name)
	}
	return def != nil
}

// Profile lookup seam of the credential bypass check
var auxGetProviderProfileFn = GetProviderProfile

// True for a canonical profile whose ambient or external process credentials custom cannot carry
func auxProfileCredentialsBypassCustom(ctx context.Context, name string) bool {
	var profile Profile
	var found bool
	if err := auxRecoverRaise(func() { profile, found = auxGetProviderProfileFn(ctx, name) }); err != nil {
		slog.Debug(fmt.Sprintf("provider profile lookup for %s failed", name), "error", err)
		return false
	}
	return found && profile.Name == name && (profile.AmbientCredentials || profile.AuthType == "external_process")
}

// Named custom lookup seam, a raised config load failing closed
var auxNamedCustomProviderFn = func(ctx context.Context, name string) bool {
	tree, err := auxLoadConfigFn(ctx)
	if err != nil {
		panic(err)
	}
	return newResolver(ctx, tree, nil, "", nil).HasNamedCustomProvider(name)
}

// True when the user defined providers section names the provider, false when config cannot load
func auxNamedCustomProviderPresent(ctx context.Context, name string) bool {
	present := false
	if err := auxRecoverRaise(func() { present = auxNamedCustomProviderFn(ctx, name) }); err != nil {
		return false
	}
	return present
}
