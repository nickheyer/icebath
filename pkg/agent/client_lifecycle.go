package agent

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyplatform"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const (
	qwenCodeVersion = "0.14.1"
	openAISlot      = "_request_client_cache"
	anthropicSlot   = "_request_anthropic_client_cache"
	noSocketsSuffix = " — no sockets found; in-flight request may keep running until the provider finishes"
)

// Closes from the owning worker after a response, the only closes that keep a pooled client warm
var requestClientReuseReasons = map[string]bool{"request_complete": true, "stream_request_complete": true}

// Socket shutdown seam over the provider pool sweep
var forceCloseTCPSockets = provider.ForceCloseTCPSockets

var clientLockInitMu sync.Mutex

// User agent RouterMint needs to avoid Cloudflare 1010 blocks
func routermintHeaders() (map[string]string, error) {
	ua, err := updaterecord.UserAgent("IcebathAgent")
	if err != nil {
		return nil, err
	}
	return map[string]string{"User-Agent": ua}, nil
}

// QwenCode CLI identity headers the Qwen Portal expects
func qwenPortalHeaders() map[string]string {
	ua := fmt.Sprintf("QwenCode/%s (%s; %s)", qwenCodeVersion, strings.ToLower(pyplatform.System()), pyplatform.Machine())
	return map[string]string{"User-Agent": ua, "X-DashScope-CacheControl": "enable", "X-DashScope-UserAgent": ua, "X-DashScope-AuthType": "qwen-oauth"}
}

// Empties a request slot, keeping it reserved when in use is set
func resetSlot(cache map[string]any, inUse bool) {
	cache["client"], cache["key"], cache["poisoned"], cache["in_use"] = nil, nil, false, inUse
}

// True when both the key and the base url are non blank strings
func validCredentialPair(apiKey, baseURL any) bool {
	k, kok := apiKey.(string)
	b, bok := baseURL.(string)
	return kok && bok && strings.TrimSpace(k) != "" && strings.TrimSpace(b) != ""
}

// Equality of two cache keys as Python compares them, callables by identity
func slotKeyEqual(x, y any) bool {
	if x == nil || y == nil {
		return x == nil && y == nil
	}
	vx, vy := reflect.ValueOf(x), reflect.ValueOf(y)
	if vx.Kind() == reflect.Func || vy.Kind() == reflect.Func {
		return vx.Kind() == vy.Kind() && vx.Pointer() == vy.Pointer()
	}
	switch tx := x.(type) {
	case map[string]any:
		ty, ok := y.(map[string]any)
		if !ok || len(tx) != len(ty) {
			return false
		}
		for k, v := range tx {
			w, present := ty[k]
			if !present || !slotKeyEqual(v, w) {
				return false
			}
		}
		return true
	case []any:
		ty, ok := y.([]any)
		if !ok || len(tx) != len(ty) {
			return false
		}
		for i := range tx {
			if !slotKeyEqual(tx[i], ty[i]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(x, y)
}

// Goroutine, provider, base url and model of the calling worker for client log lines
func (a *AIAgent) ClientLogContext() string {
	id := pythread.GoroutineID()
	return fmt.Sprintf("thread=goroutine-%d:%d provider=%s base_url=%s model=%s", id, id, a.provider, a.baseURL, a.model)
}

// Provider and model for Anthropic client log lines
func (a *AIAgent) anthropicLogContext() string {
	return fmt.Sprintf("provider=%s model=%s", a.provider, a.model)
}

// Reentrant client lock, created on first use
func (a *AIAgent) openaiClientLock() *pythread.RLock {
	clientLockInitMu.Lock()
	defer clientLockInitMu.Unlock()
	if a.clientLock == nil {
		a.clientLock = &pythread.RLock{}
	}
	return a.clientLock
}

// True when the client or its HTTP client reports itself closed
func isOpenAIClientClosed(client any) bool {
	if c, ok := client.(interface{ IsClosed() bool }); ok && c.IsClosed() {
		return true
	}
	if holder, ok := client.(interface{ HTTPClient() any }); ok {
		if hc, ok := holder.HTTPClient().(interface{ IsClosed() bool }); ok {
			return hc.IsClosed()
		}
	}
	return false
}

// Shared keepalive HTTP client used by the main and auxiliary paths
func (a *AIAgent) buildKeepaliveHTTPClient(baseURL string, verify provider.TLSVerify) *http.Client {
	return provider.BuildKeepaliveHTTPClient(baseURL, false, verify)
}

// Force closes the client sockets then closes it gracefully, logging the outcome
func (a *AIAgent) closeOpenAIClient(client any, reason string, shared bool) {
	if client == nil {
		return
	}
	logCtx := a.ClientLogContext()
	forceClosed := forceCloseTCPSockets(client)
	if err := closeClient(client); err != nil {
		runAgentLog().Debug(fmt.Sprintf("OpenAI client close failed (%s, shared=%s) %s error=%v", reason, pyBoolStr(shared), logCtx, err))
		return
	}
	runAgentLog().Info(fmt.Sprintf("OpenAI client closed (%s, shared=%s, tcp_force_closed=%d) %s", reason, pyBoolStr(shared), forceClosed, logCtx))
}

// Close of a client exposing Close, a panic surfacing as the close error
func closeClient(client any) (err error) {
	c, ok := client.(interface{ Close() error })
	if !ok {
		return fmt.Errorf("AttributeError: '%T' object has no attribute 'close'", client)
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return c.Close()
}

// Shuts the sockets of a replaced shared client down and leaves descriptor release to the collector
func (a *AIAgent) retireSharedOpenAIClient(client any, reason string) {
	if client == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			runAgentLog().Debug(fmt.Sprintf("Shared OpenAI client retire failed (%s) %s error=%v", reason, a.ClientLogContext(), r))
		}
	}()
	shutdown := forceCloseTCPSockets(client)
	runAgentLog().Info(fmt.Sprintf("Shared OpenAI client retired (%s, tcp_shutdown=%d, fd_release=deferred_to_gc) %s", reason, shutdown, a.ClientLogContext()))
}

// Socket shutdown of every client an abandoned worker may block on, returning the sockets shut
func (a *AIAgent) drainTransportsAfterAbandonment(reason string) int {
	drained := 0
	guard := func(label string, fn func()) {
		defer func() {
			if r := recover(); r != nil {
				runAgentLog().Debug("Abandoned-worker drain: "+label, "error", r)
			}
		}()
		fn()
	}
	guard("shared client sweep failed", func() {
		if client := a.client; client != nil {
			drained += forceCloseTCPSockets(client)
		}
	})
	for _, slot := range []struct {
		attr, label string
		abort       func(client any, reason string)
	}{{openAISlot, "request", a.abortRequestOpenAIClient}, {anthropicSlot, "anthropic", a.abortRequestAnthropicClient}} {
		guard(slot.label+" client abort failed", func() {
			lock := a.openaiClientLock()
			lock.Lock()
			var cached any
			if cache := *a.requestSlotRef(slot.attr); cache != nil {
				cached = cache["client"]
			}
			lock.Unlock()
			if cached != nil {
				slot.abort(cached, reason)
			}
		})
	}
	guard("codex interrupt failed", func() {
		if a.codexSession != nil {
			a.codexSession.RequestInterrupt()
		}
	})
	guard("active request abort failed", func() {
		a.abortActiveRequest(reason)
	})
	runAgentLog().Info(fmt.Sprintf("Abandoned-worker transports drained (%s, tcp_shutdown=%d, fd_release=deferred_to_worker) %s", reason, drained, a.ClientLogContext()))
	return drained
}

// Rebuilds the shared primary client under the client lock and retires the replaced one
func (a *AIAgent) replacePrimaryOpenAIClient(reason string) bool {
	ctx := a.homeContext()
	lock := a.openaiClientLock()
	lock.Lock()
	oldClient := a.client
	var newClient any
	var err error
	if strings.ToLower(strings.TrimSpace(a.provider)) == "moa" {
		newClient = buildMoaFacade(ctx, a, a.model)
	} else {
		newClient, err = a.createOpenAIClient(ctx, a.clientKwargs, reason, true)
	}
	if err != nil {
		lock.Unlock()
		runAgentLog().Warn(fmt.Sprintf("Failed to rebuild shared primary client (%s) %s error=%v", reason, a.ClientLogContext(), err))
		return false
	}
	a.client = newClient
	lock.Unlock()
	a.retireSharedOpenAIClient(oldClient, "replace:"+reason)
	return true
}

// Shared primary client, recreated first when it reports itself closed
func (a *AIAgent) ensurePrimaryOpenAIClient(ctx context.Context, reason string) (any, error) {
	lock := a.openaiClientLock()
	lock.Lock()
	client := a.client
	if client != nil && !isOpenAIClientClosed(client) {
		lock.Unlock()
		return client, nil
	}
	newClient, err := a.createOpenAIClient(ctx, a.clientKwargs, reason, true)
	if err != nil {
		lock.Unlock()
		runAgentLog().Warn(fmt.Sprintf("Failed to recreate closed OpenAI client (%s) %s error=%v", reason, a.ClientLogContext(), err))
		return nil, fmt.Errorf("Failed to recreate closed OpenAI client: %w", err)
	}
	a.client = newClient
	lock.Unlock()
	runAgentLog().Warn(fmt.Sprintf("Detected closed shared OpenAI client; recreated before use (%s) %s", reason, a.ClientLogContext()))
	a.closeOpenAIClient(client, "replace:"+reason, true)
	return newClient, nil
}

func (a *AIAgent) EnsurePrimaryOpenAIClient(ctx context.Context, reason string) (any, error) {
	return a.ensurePrimaryOpenAIClient(ctx, reason)
}

// True when the chat messages or Responses input still carry a native image part
func apiKwargsHaveImageParts(apiKwargs map[string]any) bool {
	var containsImage func(v any) bool
	containsImage = func(v any) bool {
		switch t := v.(type) {
		case map[string]any:
			if typ, _ := t["type"].(string); typ == "image_url" || typ == "input_image" {
				return true
			}
			for _, inner := range t {
				if containsImage(inner) {
					return true
				}
			}
		case []any:
			for _, inner := range t {
				if containsImage(inner) {
					return true
				}
			}
		case []map[string]any:
			for _, inner := range t {
				if containsImage(inner) {
					return true
				}
			}
		}
		return false
	}
	for _, field := range []string{"messages", "input"} {
		switch items := apiKwargs[field].(type) {
		case []any, []map[string]any:
			if containsImage(items) {
				return true
			}
		}
	}
	return false
}

// Field holding the named request slot
func (a *AIAgent) requestSlotRef(slotAttr string) *map[string]any {
	if slotAttr == anthropicSlot {
		return &a.requestAnthropicClientCache
	}
	return &a.requestClientCache
}

// Named request slot, created empty on first use, caller holds the client lock
func (a *AIAgent) requestSlot(slotAttr string) map[string]any {
	ref := a.requestSlotRef(slotAttr)
	if *ref == nil {
		*ref = map[string]any{}
		resetSlot(*ref, false)
	}
	return *ref
}

// Reusable cached client or the stale one to close, at most one non nil
func (a *AIAgent) checkoutRequestSlot(slotAttr string, key any) (any, any) {
	lock := a.openaiClientLock()
	lock.Lock()
	defer lock.Unlock()
	cache := a.requestSlot(slotAttr)
	cached := cache["client"]
	if cached == nil || cache["in_use"] == true {
		return nil, nil
	}
	if cache["poisoned"] != true && slotKeyEqual(cache["key"], key) && !isOpenAIClientClosed(cached) {
		cache["in_use"] = true
		return cached, nil
	}
	resetSlot(cache, false)
	return nil, cached
}

// Caches a fresh client as checked out unless a concurrent call already holds the slot
func (a *AIAgent) storeRequestSlot(slotAttr string, client, key any) {
	lock := a.openaiClientLock()
	lock.Lock()
	defer lock.Unlock()
	cache := a.requestSlot(slotAttr)
	if cache["client"] == nil {
		cache["client"], cache["key"], cache["poisoned"], cache["in_use"] = client, key, false, true
	}
}

// Owner release, true when the client stays cached after a clean unpoisoned finish
func (a *AIAgent) releaseRequestSlot(slotAttr string, client any, reason string) bool {
	lock := a.openaiClientLock()
	lock.Lock()
	defer lock.Unlock()
	cache := a.requestSlot(slotAttr)
	if sameClient(cache["client"], client) {
		if requestClientReuseReasons[reason] && cache["poisoned"] != true {
			cache["in_use"] = false
			return true
		}
		resetSlot(cache, false)
	}
	return false
}

// Identity comparison of two client values as the Python is operator compares them
func sameClient(x, y any) bool {
	if x == nil || y == nil {
		return x == nil && y == nil
	}
	vx, vy := reflect.ValueOf(x), reflect.ValueOf(y)
	if vx.Type() != vy.Type() {
		return false
	}
	switch vx.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Chan, reflect.Func, reflect.UnsafePointer, reflect.Slice:
		return vx.Pointer() == vy.Pointer()
	}
	return vx.Type().Comparable() && x == y
}

// Teardown take of the slot client and whether a worker held it
func (a *AIAgent) takeRequestSlot(slotAttr string) (any, bool) {
	lock := a.openaiClientLock()
	lock.Lock()
	defer lock.Unlock()
	cache := *a.requestSlotRef(slotAttr)
	if cache == nil {
		return nil, false
	}
	client, inUse := cache["client"], cache["in_use"] == true
	resetSlot(cache, false)
	return client, inUse
}

// Stranger goroutine abort that shuts sockets down without releasing descriptors and poisons the slot
func (a *AIAgent) abortRequestSlotClient(slotAttr string, client any, reason string) {
	if client == nil {
		return
	}
	label, logCtx := "OpenAI", a.ClientLogContext()
	if slotAttr == anthropicSlot {
		label, logCtx = "Anthropic", a.anthropicLogContext()
	}
	lock := a.openaiClientLock()
	lock.Lock()
	if cache := a.requestSlot(slotAttr); sameClient(cache["client"], client) {
		cache["poisoned"] = true
	}
	lock.Unlock()
	defer func() {
		if r := recover(); r != nil {
			runAgentLog().Debug(fmt.Sprintf("%s client abort failed (%s, shared=False) %s error=%v", label, reason, logCtx, r))
		}
	}()
	switch c := client.(type) {
	case interface{ Cancel() error }:
		if err := c.Cancel(); err != nil {
			runAgentLog().Debug(fmt.Sprintf("%s client abort failed (%s, shared=False) %s error=%v", label, reason, logCtx, err))
		}
		return
	case interface{ Cancel() }:
		c.Cancel()
		return
	}
	shutdown := forceCloseTCPSockets(client)
	msg := fmt.Sprintf("%s client aborted (%s, shared=False, tcp_force_closed=%d, deferred_close=stranger_thread) %s", label, reason, shutdown, logCtx)
	if shutdown == 0 {
		runAgentLog().Warn(msg + noSocketsSuffix)
		return
	}
	runAgentLog().Info(msg)
}

// Request scoped OpenAI client reusing the warm slot, image requests keyed apart by their route headers
func (a *AIAgent) createRequestOpenAIClient(ctx context.Context, reason string, apiKwargs map[string]any) (any, error) {
	primary, err := a.ensurePrimaryOpenAIClient(ctx, reason)
	if err != nil {
		return nil, err
	}
	if a.provider == "moa" {
		return primary, nil
	}
	lock := a.openaiClientLock()
	lock.Lock()
	requestKwargs := maps.Clone(a.clientKwargs)
	lock.Unlock()
	if requestKwargs == nil {
		requestKwargs = map[string]any{}
	}
	requestKwargs["max_retries"] = 0
	if apiKwargsHaveImageParts(apiKwargs) {
		imageHeaders := stringHeaders(provider.RouteAnswer(ctx, "client_headers", a.provider, a, map[string]any{
			"base_url": kwargText(requestKwargs["base_url"]), "api_key": kwargText(requestKwargs["api_key"]), "has_images": true,
		}))
		if len(imageHeaders) > 0 {
			merged := stringHeaders(requestKwargs["default_headers"])
			if merged == nil {
				merged = map[string]string{}
			}
			maps.Copy(merged, imageHeaders)
			requestKwargs["default_headers"] = merged
		}
	}
	cached, stale := a.checkoutRequestSlot(openAISlot, requestKwargs)
	if cached != nil {
		return cached, nil
	}
	if stale != nil {
		a.closeOpenAIClient(stale, "reuse_evict:"+reason, false)
	}
	client, err := a.createOpenAIClient(ctx, requestKwargs, reason, false)
	if err != nil {
		return nil, err
	}
	snapshot := make(map[string]any, len(requestKwargs))
	for k, v := range requestKwargs {
		switch inner := v.(type) {
		case map[string]any:
			snapshot[k] = maps.Clone(inner)
		case map[string]string:
			snapshot[k] = maps.Clone(inner)
		default:
			snapshot[k] = v
		}
	}
	a.storeRequestSlot(openAISlot, client, snapshot)
	return client, nil
}

// Owner close that keeps a cleanly finished client cached, otherwise closes it
func (a *AIAgent) closeRequestOpenAIClient(client any, reason string) {
	if !a.releaseRequestSlot(openAISlot, client, reason) {
		a.closeOpenAIClient(client, reason, false)
	}
}

// Teardown close of the cached request client, aborted instead while a worker holds it
func (a *AIAgent) closeCachedRequestOpenAIClient(reason string) {
	client, inUse := a.takeRequestSlot(openAISlot)
	if client == nil {
		return
	}
	if inUse {
		a.abortRequestOpenAIClient(client, reason+"_in_flight")
		return
	}
	a.closeOpenAIClient(client, reason, false)
}

// Stranger abort of a request OpenAI client
func (a *AIAgent) abortRequestOpenAIClient(client any, reason string) {
	a.abortRequestSlotClient(openAISlot, client, reason)
}

// Request timeout of the route as the cache key element, nil when unset
func (a *AIAgent) requestTimeoutKey(ctx context.Context) any {
	if t, ok := provider.GetProviderRequestTimeout(ctx, a.provider, a.model); ok {
		return t
	}
	return nil
}

// Cache key over the credential, base url or region, timeout and the 1M beta flag
func (a *AIAgent) requestAnthropicClientKey(ctx context.Context) []any {
	if a.provider == "bedrock" {
		region := a.bedrockRegion
		if region == "" {
			region = "us-east-1"
		}
		return []any{"bedrock", region, a.requestTimeoutKey(ctx)}
	}
	return []any{"direct", a.anthropicKeyValue(), a.anthropicBaseURL, a.requestTimeoutKey(ctx), a.oauth1MBetaDisabled}
}

// Bedrock or direct Anthropic client for a request cache key
func (a *AIAgent) buildAnthropicClientForKey(ctx context.Context, key []any) (any, error) {
	if key[0] == "bedrock" {
		timeout, _ := key[2].(float64)
		return provider.BuildAnthropicBedrockClient(ctx, key[1].(string), timeout)
	}
	timeout, _ := key[3].(float64)
	baseURL, _ := key[2].(string)
	return anthropicClientBuilder(ctx, key[1], baseURL, timeout, key[4] == true)
}

// Request local Anthropic client for one in flight call, reusing the warm slot
func (a *AIAgent) createRequestAnthropicClient(ctx context.Context, reason string) (any, error) {
	if a.apiMode == "anthropic_messages" {
		if _, err := a.tryRefreshAnthropicClientCredentials(ctx); err != nil {
			return nil, err
		}
	}
	key := a.requestAnthropicClientKey(ctx)
	cached, stale := a.checkoutRequestSlot(anthropicSlot, key)
	if cached != nil {
		return cached, nil
	}
	if stale != nil {
		a.closeRequestAnthropicClient(stale, "reuse_evict:"+reason)
	}
	client, err := a.buildAnthropicClientForKey(ctx, key)
	if err != nil {
		return nil, err
	}
	runAgentLog().Debug(fmt.Sprintf("Anthropic request client created (%s, shared=False) %s", reason, a.anthropicLogContext()))
	a.storeRequestSlot(anthropicSlot, client, key)
	return client, nil
}

// Owner close keeping a clean finish warm, otherwise force closing the sockets and the client
func (a *AIAgent) closeRequestAnthropicClient(client any, reason string) {
	if client == nil || a.releaseRequestSlot(anthropicSlot, client, reason) {
		return
	}
	forceCloseTCPSockets(client)
	if err := closeClient(client); err != nil {
		runAgentLog().Debug(fmt.Sprintf("Anthropic client close failed (%s, shared=False) %s error=%v", reason, a.anthropicLogContext(), err))
		return
	}
	runAgentLog().Info(fmt.Sprintf("Anthropic client closed (%s, shared=False) %s", reason, a.anthropicLogContext()))
}

// Teardown close of the cached Anthropic request client, aborted instead while a worker holds it
func (a *AIAgent) closeCachedRequestAnthropicClient(reason string) {
	client, inUse := a.takeRequestSlot(anthropicSlot)
	if client == nil {
		return
	}
	if inUse {
		a.abortRequestAnthropicClient(client, reason+"_in_flight")
		return
	}
	func() {
		defer func() { _ = recover() }()
		forceCloseTCPSockets(client)
		_ = closeClient(client)
	}()
}

// Stranger abort of a request Anthropic client
func (a *AIAgent) abortRequestAnthropicClient(client any, reason string) {
	a.abortRequestSlotClient(anthropicSlot, client, reason)
}

// Mirrors the agent key and base url into the OpenAI style client kwargs
func (a *AIAgent) syncClientKwargsCredentials() {
	if a.clientKwargs == nil {
		a.clientKwargs = map[string]any{}
	}
	a.clientKwargs["api_key"], a.clientKwargs["base_url"] = a.apiKey, a.baseURL
}

// Applies a fresh key and base url to the client kwargs and rebuilds the shared client
func (a *AIAgent) adoptOpenAICredentials(apiKey, baseURL, reason string) bool {
	a.apiKey = strings.TrimSpace(apiKey)
	a.setBaseURL(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	a.syncClientKwargsCredentials()
	return a.replacePrimaryOpenAIClient(reason)
}

// Rebuilds the Anthropic client for the current request key after an interrupted or stale call
func (a *AIAgent) rebuildAnthropicClient(ctx context.Context) error {
	client, err := a.buildAnthropicClientForKey(ctx, a.requestAnthropicClientKey(ctx))
	if err != nil {
		return err
	}
	a.anthropicClient = client
	return nil
}

// ChatCompletionAgent request client surface over the request slot methods
func (a *AIAgent) CreateRequestOpenAIClient(reason string, apiKwargs map[string]any) (any, error) {
	return a.createRequestOpenAIClient(a.homeContext(), reason, apiKwargs)
}

func (a *AIAgent) AbortRequestOpenAIClient(client any, reason string) error {
	a.abortRequestOpenAIClient(client, reason)
	return nil
}

func (a *AIAgent) CloseRequestOpenAIClient(client any, reason string) error {
	a.closeRequestOpenAIClient(client, reason)
	return nil
}

func (a *AIAgent) CreateRequestAnthropicClient(reason string) (any, error) {
	return a.createRequestAnthropicClient(a.homeContext(), reason)
}

func (a *AIAgent) AbortRequestAnthropicClient(client any, reason string) error {
	a.abortRequestAnthropicClient(client, reason)
	return nil
}

func (a *AIAgent) CloseRequestAnthropicClient(client any, reason string) error {
	a.closeRequestAnthropicClient(client, reason)
	return nil
}
