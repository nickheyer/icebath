package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// webCallContext carries the request context and the session env through the web pipeline
type webCallContext struct {
	ctx context.Context
	env *Env
}

// Config aware env value, the secret scope then the process env, trimmed
func webEnvValue(env *Env, name string) string { return strings.TrimSpace(secretValue(env, name)) }

func webHasEnv(env *Env, name string) bool { return webEnvValue(env, name) != "" }

// Lower cased web.<key>, empty when unset
func configuredWebBackend(env *Env, key string) string { return webBackendKey(env, key) }

// Reports whether a provider is registered and the probe answered true, false when it panicked
func webProbe(env *Env, p WebProvider, keyless bool) bool {
	if p == nil {
		return false
	}
	if keyless {
		return providerKeylessAvailable(env, p)
	}
	return providerAvailable(env, p)
}

// Shared web backend name, a stored selection returned as is and a never configured install autodetected
func getWebBackend(env *Env) string {
	if configured := configuredWebBackend(env, "backend"); configured != "" {
		if configured == NousManagedProvider {
			return "firecrawl"
		}
		return configured
	}
	if ReadSelection(env, "web") != "" {
		return "firecrawl"
	}
	if backend := autodetectWebBackend(env); backend != "" {
		return backend
	}
	if backend := keylessWebBackend(env); backend != "" {
		return backend
	}
	return "firecrawl"
}

// Autodetect rungs above the keyless tier, explicit credentials beating the managed gateway probe
func autodetectWebBackend(env *Env) string {
	candidates := []struct {
		name      string
		available bool
	}{
		{"tavily", webHasEnv(env, "TAVILY_API_KEY")}, {"perplexity", webHasEnv(env, "PERPLEXITY_API_KEY")},
		{"exa", webHasEnv(env, "EXA_API_KEY")},
		{"parallel", webHasEnv(env, "PARALLEL_API_KEY")}, {"keenable", webHasEnv(env, "KEENABLE_API_KEY")},
		{"firecrawl", webHasEnv(env, "FIRECRAWL_API_KEY") || webHasEnv(env, "FIRECRAWL_API_URL")},
		{"firecrawl", firecrawlGatewayReady(env)}, {"searxng", webHasEnv(env, "SEARXNG_URL")},
		{"brave-free", webHasEnv(env, "BRAVE_SEARCH_API_KEY")}, {"ddgs", ddgsPackageImportable(context.Background())},
	}
	for _, c := range candidates {
		if c.available {
			return c.name
		}
	}
	for _, p := range listWebProviders(env) {
		if !legacyWebBackends[p.Name()] && webProbe(env, p, false) {
			return p.Name()
		}
	}
	return ""
}

// Keyless free tier backend name, strictly the last autodetect rung
func keylessWebBackend(env *Env) string {
	ensureWebPluginsLoaded(context.Background(), env)
	if !keylessEnabled(env) {
		return ""
	}
	for _, name := range keylessPreference() {
		if p := webProvider(env, name); p != nil && webProbe(env, p, true) {
			return name
		}
	}
	return ""
}

// True when web_search is on the managed Nous route
func managedWebSearch(env *Env) bool {
	if pin := configuredWebBackend(env, "search_backend"); pin != "" {
		return pin == NousManagedProvider
	}
	if selected := ReadSelection(env, "web"); selected != "" {
		return selected == NousManagedProvider
	}
	backend := autodetectWebBackend(env)
	if backend == "firecrawl" {
		return !(webHasEnv(env, "FIRECRAWL_API_KEY") || webHasEnv(env, "FIRECRAWL_API_URL")) && firecrawlGatewayReady(env)
	}
	if backend != "" {
		return false
	}
	gw, err := ResolveFreeSearchGateway(env, peekNousAccessToken)
	return err == nil && gw != nil
}

// Backend for web_search, the pin strictly, then the shared backend, managed search served by Perplexity
func getSearchBackend(env *Env) string {
	if pin := configuredWebBackend(env, "search_backend"); pin != "" && pin != NousManagedProvider {
		return pin
	}
	if managedWebSearch(env) {
		return "perplexity"
	}
	return getWebBackend(env)
}

// Legacy web_search backend probe for env
func GetSearchBackend(env *Env) string { return getSearchBackend(env) }

// Backend for web_extract, the pin strictly, a nous pin being managed Firecrawl
func getExtractBackend(env *Env) string {
	pin := configuredWebBackend(env, "extract_backend")
	if pin == NousManagedProvider {
		return "firecrawl"
	}
	if pin != "" {
		return pin
	}
	return getWebBackend(env)
}

// Cheap xAI credential probe, env key then the auth store tokens, no network
func hasXAICredentials(env *Env) bool {
	if secretValue(env, "XAI_API_KEY") != "" {
		return true
	}
	h := home.Process()
	if env != nil && env.Home != "" {
		h = env.Home
	}
	data, err := os.ReadFile(home.AuthPath(h))
	if err != nil {
		return false
	}
	var store map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(string(data), "\xEF\xBB\xBF")), &store); err != nil {
		return false
	}
	tokens := asMap(asMap(asMap(store["providers"])["xai-oauth"])["tokens"])
	if strings.TrimSpace(asStr(tokens["access_token"])) != "" {
		return true
	}
	for _, e := range toAnyList(asMap(store["credential_pool"])["xai-oauth"]) {
		if strings.TrimSpace(asStr(asMap(e)["access_token"])) != "" {
			return true
		}
	}
	return false
}

// Cheap xAI credential probe over the env key and the auth store tokens without any network call
func HasXAICredentials(env *Env) bool { return hasXAICredentials(env) }

// Built in backends and their cheap availability probes
var builtinWebAvailability = map[string]func(env *Env) bool{
	"exa":       func(env *Env) bool { return webHasEnv(env, "EXA_API_KEY") },
	"parallel":  func(env *Env) bool { return webHasEnv(env, "PARALLEL_API_KEY") },
	"keenable":  func(env *Env) bool { return webHasEnv(env, "KEENABLE_API_KEY") },
	"firecrawl": func(env *Env) bool { return CheckFirecrawlAPIKey(env) },
	"tavily": func(env *Env) bool {
		if webHasEnv(env, "TAVILY_API_KEY") {
			return true
		}
		for _, k := range []string{"backend", "search_backend", "extract_backend"} {
			if configuredWebBackend(env, k) == "tavily" {
				return true
			}
		}
		return false
	},
	"perplexity": func(env *Env) bool { return webHasEnv(env, "PERPLEXITY_API_KEY") || managedWebSearch(env) },
	"searxng":    func(env *Env) bool { return webHasEnv(env, "SEARXNG_URL") },
	"brave-free": func(env *Env) bool { return webHasEnv(env, "BRAVE_SEARCH_API_KEY") },
	"ddgs":       func(env *Env) bool { return ddgsPackageImportable(context.Background()) },
	"xai":        func(env *Env) bool { return hasXAICredentials(env) },
}

// Built in backend names, the keys of builtinWebAvailability spelled out so initialization has no cycle
var legacyWebBackends = setOf("exa", "parallel", "keenable", "firecrawl", "tavily", "perplexity", "searxng", "brave-free", "ddgs", "xai")

// The single availability chokepoint, plugin names through the registry and built ins through cheap probes
func isWebBackendAvailable(env *Env, backend string) bool {
	backend = strings.ToLower(strings.TrimSpace(backend))
	if !legacyWebBackends[backend] {
		if p := webProvider(env, backend); p != nil {
			return webProbe(env, p, false)
		}
	}
	probe := builtinWebAvailability[backend]
	return probe != nil && probe(env)
}

// Tool registry metadata env vars for the web backends
func webRequiresEnv() []string {
	return []string{"EXA_API_KEY", "PARALLEL_API_KEY", "TAVILY_API_KEY", "PERPLEXITY_API_KEY", "KEENABLE_API_KEY", "FIRECRAWL_API_KEY",
		"FIRECRAWL_API_URL", "FIRECRAWL_GATEWAY_URL", "PERPLEXITY_GATEWAY_URL", "TOOL_GATEWAY_DOMAIN", "TOOL_GATEWAY_SCHEME", "TOOL_GATEWAY_USER_TOKEN"}
}

var (
	webDebugOnce sync.Once
	webDebug     *DebugSession
)

// The process wide web tools debug session
func webDebugSession(env *Env) *DebugSession {
	webDebugOnce.Do(func() { webDebug = NewDebugSession(env, "web_tools", "WEB_TOOLS_DEBUG") })
	return webDebug
}

// Logs the call into the debug session, with an error recording it and returning its tool error envelope
func finishWebDebug(env *Env, callName string, data map[string]any, errMsg string) string {
	if errMsg != "" {
		envLog(env).Debug(errMsg)
		data["error"] = errMsg
	}
	d := webDebugSession(env)
	d.LogCall(callName, data)
	d.Save(env)
	if errMsg == "" {
		return ""
	}
	return toolErrorJSON(errMsg, nil)
}

// True when the provider is keyed available or keyless capable without raising
func providerIsReady(env *Env, p WebProvider) bool {
	if p == nil {
		return false
	}
	ready, panicked := probeAvailable(env, p)
	if panicked {
		return false
	}
	return ready || webProbe(env, p, true)
}

// Availability probe reporting a panic separately from a false answer
func probeAvailable(env *Env, p WebProvider) (ok bool, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			envLog(env).Debug("web provider is_available raised during readiness check", "provider", p.Name(), "panic", fmt.Sprint(r))
			ok, panicked = false, true
		}
	}()
	return p.Available(env), false
}

// Credential probes that back other tools but serve no registered web backend
var webCheckSkip = map[string]bool{"xai": true}

// CheckWebAPIKey is the check gate for web_search and web_extract, any usable backend lighting the tools
func CheckWebAPIKey(env *Env) bool {
	var candidates []string
	if c := configuredWebBackend(env, "backend"); c != "" {
		candidates = append(candidates, c)
	}
	for name := range legacyWebBackends {
		if !webCheckSkip[name] {
			candidates = append(candidates, name)
		}
	}
	for _, backend := range candidates {
		if isWebBackendAvailable(env, backend) {
			return true
		}
	}
	ensureWebPluginsLoaded(context.Background(), env)
	for _, p := range []WebProvider{activeSearchProvider(env), activeExtractProvider(env)} {
		if p != nil && webCheckSkip[p.Name()] {
			if keylessWebBackend(env) != "" {
				return true
			}
			continue
		}
		if providerIsReady(env, p) {
			return true
		}
	}
	return false
}

// Error text when the configured backend's bundled plugin is disabled in config
func disabledPluginError(capability, disabledKey string) string {
	vendor := disabledKey
	if i := strings.Index(disabledKey, "/"); i >= 0 {
		vendor = disabledKey[i+1:]
	}
	return fmt.Sprintf("web.%s_backend is set to '%s', but its plugin ('%s') is disabled in config. Re-enable it with `icebath plugins enable %s` (or remove it from plugins.disabled).",
		capability, vendor, disabledKey, disabledKey)
}

// Error when no provider resolved, pointing at a disabled bundled plugin when that is the real cause
func noProviderError(env *Env, capability, fallback string) string {
	if key := disabledWebPluginFor(env, "", capability); key != "" {
		return disabledPluginError(capability, key)
	}
	return fallback
}

// Error for a stored but unregistered backend
func strictSelectionError(env *Env, capability, backend string) string {
	failure := "no registered web " + capability + " provider has that name"
	return noProviderError(env, capability, SelectionError("web", "'"+backend+"'", failure))
}

// Runs the paid search with the managed fallback and the keyless rescue around it, rescued true when served elsewhere
func paidSearch(ctx webCallContext, provider WebProvider, query string, fetchLimit int) (*WebSearchResponse, bool, error) {
	servedAfterFailure := func(errText string) (*WebSearchResponse, error) {
		if fallback := managedSearchFallback(ctx, provider, errText, query, fetchLimit); fallback != nil {
			return fallback, nil
		}
		if rescueEligible(ctx.env, provider) {
			return rescueSearch(ctx, provider.Name(), errText, query, fetchLimit)
		}
		return nil, nil
	}
	resp, err := provider.Search(ctx.ctx, ctx.env, query, fetchLimit)
	if err != nil {
		served, serr := servedAfterFailure(err.Error())
		if serr != nil {
			return nil, false, serr
		}
		if served != nil {
			return served, true, nil
		}
		return nil, false, err
	}
	if resp == nil {
		resp = searchFail("")
	}
	if !resp.Success {
		served, serr := servedAfterFailure(resp.Error)
		if serr != nil {
			return nil, false, serr
		}
		if served != nil {
			return served, true, nil
		}
	}
	return resp, false, nil
}

// TTL memo and single flight around the paid vendor call, bucketed count fetched and sliced for the caller
func memoizedSearch(ctx webCallContext, provider WebProvider, query string, limit int) (*WebSearchResponse, error) {
	env := ctx.env
	name := provider.Name()
	response := searchMemo.Lookup(env, name, query, limit)
	if response == nil {
		lock := searchMemo.FlightLock(name, query, limit)
		lock.Lock()
		response = searchMemo.Lookup(env, name, query, limit)
		if response == nil {
			resp, rescued, err := paidSearch(ctx, provider, query, BucketLimit(limit))
			if err != nil {
				lock.Unlock()
				return nil, err
			}
			if !rescued {
				searchMemo.Store(env, name, query, limit, resp)
			}
			response = resp
		}
		lock.Unlock()
	}
	return SliceSearchResponse(response, limit), nil
}

// WebSearchTool searches the web through the configured backend and returns the result JSON
func WebSearchTool(ctx context.Context, env *Env, query string, limitArg any) string {
	limit := 5
	if n, ok := pyIntValue(limitArg); ok {
		if s, isString := limitArg.(string); !isString || pyIntLiteralRe.MatchString(strings.TrimSpace(s)) {
			limit = min(max(n, 1), 100)
		}
	}
	debug := map[string]any{"parameters": map[string]any{"query": query, "limit": limit}, "error": nil, "results_count": 0, "original_response_size": 0, "final_response_size": 0}
	if IsInterrupted(ctx) {
		return toolErrorJSON("Interrupted", map[string]any{"success": false})
	}
	call := webCallContext{ctx, env}
	ensureWebPluginsLoaded(ctx, env)
	backend := getSearchBackend(env)
	var provider WebProvider
	if backend != "" {
		provider = webProvider(env, backend)
	}
	if provider == nil || !provider.SupportsSearch() {
		if provider == nil && backend != "" && SelectionExists(env, "web") {
			errText := strictSelectionError(env, "search", backend)
			debug["error"] = errText
			finishWebDebug(env, "web_search_tool", debug, "")
			return jsonIndent(map[string]any{"success": false, "error": errText})
		}
		provider = activeSearchProvider(env)
	}
	var response any
	if provider == nil {
		response = map[string]any{"success": false, "error": noProviderError(env, "search", "No web search provider configured. Run `icebath tools` to set one up.")}
	} else {
		envLog(env).Info("Web search", "provider", provider.Name(), "query", query, "limit", limit)
		resp, err := memoizedSearch(call, provider, query, limit)
		if err != nil {
			return finishWebDebug(env, "web_search_tool", debug, "Error searching web: "+err.Error())
		}
		if resp.Data != nil {
			debug["results_count"] = len(resp.Data.Web)
		}
		response = resp
	}
	resultJSON := jsonIndent(response)
	debug["final_response_size"] = len([]rune(resultJSON))
	finishWebDebug(env, "web_search_tool", debug, "")
	return resultJSON
}

const (
	noResultError       = "Extract backend returned no result for this URL"
	extractBackendsHint = "firecrawl, tavily, keenable, exa, or parallel."
	invalidItemError    = "Invalid URL item at index %d: expected a URL string or an object with a string 'url' or 'href' field"
	defaultExtractTO    = 120.0
)

// URL from a model supplied extract item, string or object with url or href, empty when unusable
func webExtractURL(value any) string {
	if m, ok := value.(map[string]any); ok {
		value = m["url"]
		if !truthy(value) {
			value = m["href"]
		}
	}
	s, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// validatedExtract is the normalized URL list with positions, invalid rows, and a whole call refusal
type validatedExtract struct {
	urls    []string
	indices []int
	invalid map[int]*WebPage
	blocked string
}

// Normalizes items and refuses the whole call when any URL carries a secret
func validateExtractURLs(items []any) validatedExtract {
	out := validatedExtract{invalid: map[int]*WebPage{}}
	for index, item := range items {
		u := webExtractURL(item)
		if u == "" {
			out.invalid[index] = pageError("", fmt.Sprintf(invalidItemError, index))
			continue
		}
		normalized := normalizeURLForRequest(u)
		for _, candidate := range []string{u, unquoteLoose(u), normalized, unquoteLoose(normalized)} {
			if prefixSearch(candidate) {
				out.blocked = jsonCompact(map[string]any{"success": false, "error": "Blocked: URL contains what appears to be an API key or token. Secrets must not be sent in URLs."})
				return out
			}
		}
		out.urls = append(out.urls, normalized)
		out.indices = append(out.indices, index)
	}
	return out
}

// Percent decodes the way urllib.parse.unquote does, leaving bad escapes as they are
func unquoteLoose(s string) string {
	if decoded, err := url.PathUnescape(strings.ReplaceAll(s, "+", "%2B")); err == nil {
		return decoded
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if decoded, err := url.PathUnescape(s[i : i+3]); err == nil {
				b.WriteString(decoded)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Rebuilds a total long result list from fixed entries by position and fetched results at their positions
func mergeInOrder(total int, fixed map[int]*WebPage, positions []int, urls []string, results []*WebPage) []*WebPage {
	merged := map[int]*WebPage{}
	for k, v := range fixed {
		merged[k] = v
	}
	for pos, position := range positions {
		if pos < len(results) {
			merged[position] = results[pos]
		} else {
			merged[position] = pageError(urls[pos], noResultError)
		}
	}
	out := make([]*WebPage, total)
	for i := range out {
		out[i] = merged[i]
	}
	return out
}

// Resolves the extract provider for a backend, the error JSON when it cannot serve
func resolveExtractProvider(env *Env, backend string) (WebProvider, string) {
	var provider WebProvider
	if backend != "" {
		provider = webProvider(env, backend)
	}
	if provider != nil && provider.SupportsExtract() {
		return provider, ""
	}
	if provider != nil {
		return nil, jsonCompact(map[string]any{"success": false, "error": provider.DisplayName() + " is a search-only backend and cannot extract URL content. Set web.extract_backend to " + extractBackendsHint})
	}
	if backend != "" && SelectionExists(env, "web") {
		return nil, jsonCompact(map[string]any{"success": false, "error": strictSelectionError(env, "extract", backend)})
	}
	provider = activeExtractProvider(env)
	if provider == nil {
		return nil, jsonCompact(map[string]any{"success": false, "error": noProviderError(env, "extract", "No web extract provider configured. Set web.extract_backend to "+extractBackendsHint)})
	}
	return provider, ""
}

// Wall clock cap for one provider extract dispatch, web.extract_timeout, zero or negative disabling it
func extractTimeout(env *Env) time.Duration {
	v, ok := webCfg(env).Lookup("extract_timeout")
	seconds := defaultExtractTO
	if ok && v != nil {
		if f, ok := pyFloatValue(v); ok {
			seconds = f
		} else if s, isString := v.(string); isString {
			if parsed, err := parseFloatText(s); err == nil {
				seconds = parsed
			}
		}
	}
	return time.Duration(seconds * float64(time.Second))
}

// Reports whether every entry failed and none of the failures is the page's own HTTP answer
func backendFailedWholeBatch(results []*WebPage) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range results {
		if r.Error == "" || isOriginHTTPError(r.Error) {
			return false
		}
	}
	return true
}

// Calls the provider's extract under the timeout with one shot keyless rescue, rescued true when the ring served
func fetchWithRescue(call webCallContext, provider WebProvider, urls []string, format string) ([]*WebPage, bool, error) {
	env := call.env
	timeout := extractTimeout(env)
	runCtx, cancel := call.ctx, func() {}
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(call.ctx, timeout)
	}
	defer cancel()
	type outcome struct {
		results []*WebPage
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		results, err := provider.Extract(runCtx, env, urls, format)
		done <- outcome{results, err}
	}()
	var out outcome
	select {
	case out = <-done:
	case <-runCtx.Done():
		if call.ctx.Err() != nil {
			return nil, false, call.ctx.Err()
		}
		envLog(env).Warn("web_extract provider timed out", "provider", provider.Name(), "seconds", int(timeout.Seconds()), "urls", len(urls))
		failed := make([]*WebPage, 0, len(urls))
		for _, u := range urls {
			failed = append(failed, pageError(u, fmt.Sprintf("Extract timed out after %ds via %s", int(timeout.Seconds()), provider.Name())))
		}
		if !rescueEligible(env, provider) {
			return failed, false, nil
		}
		return rescuedExtract(rescueExtract(call, provider.Name(), urls, failed))
	}
	if out.err != nil {
		if !rescueEligible(env, provider) {
			return nil, false, out.err
		}
		failed := make([]*WebPage, 0, len(urls))
		for _, u := range urls {
			failed = append(failed, pageError(u, out.err.Error()))
		}
		return rescuedExtract(rescueExtract(call, provider.Name(), urls, failed))
	}
	if backendFailedWholeBatch(out.results) && rescueEligible(env, provider) {
		return rescuedExtract(rescueExtract(call, provider.Name(), urls, out.results))
	}
	return out.results, false, nil
}

// Rescue outcome as the extract result, a raised ring error returned in place of the rescue
func rescuedExtract(pages []*WebPage, err error) ([]*WebPage, bool, error) {
	if err != nil {
		return nil, false, err
	}
	return pages, true, nil
}

// Refusal entry when a page's reported URL fails the website policy or SSRF gate, nil when it passes
func finalURLRefusal(env *Env, fetched *WebPage) *WebPage {
	finalURL, title := fetched.URL, fetched.Title
	refusal := func(errText string, blocked *WebsiteBlock) *WebPage {
		p := &WebPage{URL: finalURL, Title: title, HasRaw: true, Error: errText}
		if blocked != nil {
			p.BlockedByPolicy = blockedByPolicy(blocked)
		}
		return p
	}
	blocked, err := extractWebsiteAccess(env, finalURL, "")
	if err != nil {
		envLog(env).Warn("Website policy check failed for redirect target", "url", finalURL, "error", err)
		return refusal("Website policy check failed for redirect target: "+err.Error(), nil)
	}
	if blocked != nil {
		envLog(env).Info("Blocked redirected web_extract", "host", blocked.Host, "rule", blocked.Rule)
		return refusal(blocked.Message, blocked)
	}
	if !extractURLSafe(env, finalURL) {
		envLog(env).Info("Blocked redirected web_extract for unsafe final URL", "url", finalURL)
		return refusal("Blocked: URL targets a private or internal network address", nil)
	}
	return nil
}

// Re gates every page reported under a URL that was not requested
func gateFinalURLs(env *Env, results []*WebPage, requested map[string]bool) []*WebPage {
	out := make([]*WebPage, 0, len(results))
	for _, fetched := range results {
		if fetched.URL != "" && !requested[fetched.URL] && fetched.Error == "" {
			if refused := finalURLRefusal(env, fetched); refused != nil {
				fetched = refused
			}
		}
		out = append(out, fetched)
	}
	return out
}

// Fetches with rescue, re gates redirect targets, then caches the passes under their requested URL
func dispatchExtract(call webCallContext, provider WebProvider, urls []string, format string) ([]*WebPage, error) {
	env := call.env
	results, rescued, err := fetchWithRescue(call, provider, urls, format)
	if err != nil {
		return nil, err
	}
	requested := map[string]bool{}
	for _, u := range urls {
		requested[u] = true
	}
	results = gateFinalURLs(env, results, requested)
	if rescued {
		return results, nil
	}
	for _, fetched := range results {
		source := asStr(asMap(fetched.Metadata)["sourceURL"])
		u := ""
		for _, candidate := range []string{fetched.URL, source} {
			if requested[candidate] {
				u = candidate
				break
			}
		}
		content := fetched.body()
		if u != "" && content != "" && fetched.Error == "" {
			stored := ExtractCachePut(env, u, content, fetched.Title, format, provider.Name())
			if stored && fetched.URL != "" && fetched.URL != u {
				ExtractCacheRecordFinalURL(env, u, fetched.URL, content, format, provider.Name())
			}
		}
	}
	return results, nil
}

// Gates a cache hit on the URL its content came from, consuming the final url
func cachedFinalURLAllowed(env *Env, hit *WebPage, requested string) bool {
	finalURL := hit.FinalURL
	hit.FinalURL = ""
	if finalURL == "" || finalURL == requested {
		return true
	}
	blocked, err := extractWebsiteAccess(env, finalURL, "")
	if err != nil {
		envLog(env).Warn("Website policy check failed for cached final URL", "url", finalURL, "error", err)
		return false
	}
	if blocked != nil {
		return false
	}
	return extractURLSafe(env, finalURL)
}

// Serves cache hits, fetches the rest, and merges back in order
func extractSafeURLs(call webCallContext, provider WebProvider, safeURLs []string, format string) ([]*WebPage, error) {
	env := call.env
	cached := map[int]*WebPage{}
	var fetchURLs []string
	var fetchPositions []int
	for position, u := range safeURLs {
		blocked, err := extractWebsiteAccess(env, u, "")
		var hit *WebPage
		if err == nil && blocked == nil {
			hit = ExtractCacheGet(env, u, format, provider.Name())
		}
		if hit != nil && !cachedFinalURLAllowed(env, hit, u) {
			hit = nil
		}
		if hit != nil {
			cached[position] = hit
		} else {
			fetchURLs = append(fetchURLs, u)
			fetchPositions = append(fetchPositions, position)
		}
	}
	if len(fetchURLs) == 0 {
		out := make([]*WebPage, len(safeURLs))
		for i := range out {
			out[i] = cached[i]
		}
		return out, nil
	}
	envLog(env).Info("Web extract", "provider", provider.Name(), "urls", len(fetchURLs))
	results, err := dispatchExtract(call, provider, fetchURLs, format)
	if err != nil {
		return nil, err
	}
	if len(cached) == 0 {
		return results, nil
	}
	return mergeInOrder(len(safeURLs), cached, fetchPositions, fetchURLs, results), nil
}

// WebExtractTool extracts clean page content from URLs through the configured backend
func WebExtractTool(ctx context.Context, env *Env, items []any, format string, charLimit any) string {
	validated := validateExtractURLs(items)
	if validated.blocked != "" {
		return validated.blocked
	}
	debug := map[string]any{"parameters": map[string]any{"urls": validated.urls, "format": emptyAsNull(format), "char_limit": charLimit}, "error": nil,
		"pages_extracted": 0, "pages_truncated": 0, "original_response_size": 0, "final_response_size": 0, "truncation_metrics": []any{}, "processing_applied": []any{}}
	envLog(env).Info("Extracting content", "urls", len(validated.urls))
	call := webCallContext{ctx, env}
	var safeURLs []string
	var safeIndices []int
	ssrfBlocked := map[int]*WebPage{}
	for i, u := range validated.urls {
		index := validated.indices[i]
		if isSafeURL(env, u) {
			safeURLs = append(safeURLs, u)
			safeIndices = append(safeIndices, index)
		} else {
			ssrfBlocked[index] = pageError(u, "Blocked: URL targets a private or internal network address")
		}
	}
	var results []*WebPage
	if len(safeURLs) > 0 {
		backend := getExtractBackend(env)
		ensureWebPluginsLoaded(ctx, env)
		provider, errJSON := resolveExtractProvider(env, backend)
		if errJSON != "" {
			return errJSON
		}
		var err error
		results, err = extractSafeURLs(call, provider, safeURLs, format)
		if err != nil {
			return finishWebDebug(env, "web_extract_tool", debug, "Error extracting content: "+err.Error())
		}
	}
	if len(validated.invalid) > 0 || len(ssrfBlocked) > 0 {
		fixed := map[int]*WebPage{}
		for k, v := range ssrfBlocked {
			fixed[k] = v
		}
		for k, v := range validated.invalid {
			fixed[k] = v
		}
		results = mergeInOrder(len(items), fixed, safeIndices, safeURLs, results)
	}
	envLog(env).Info("Extracted content", "pages", len(results))
	debug["pages_extracted"] = len(results)
	debug["original_response_size"] = len(jsonCompact(map[string]any{"results": trimResults(results)}))
	debug["processing_applied"] = append(debug["processing_applied"].([]any), "truncate_and_store")
	truncateResults(env, results, effectiveCharLimit(env, charLimit), debug)
	trimmed := trimResults(results)
	resultJSON := toolErrorJSON("Content was inaccessible or not found", nil)
	if len(trimmed) > 0 {
		resultJSON = jsonIndent(map[string]any{"results": trimmed})
	}
	cleaned := ConvertBase64ImagesToLinks(resultJSON)
	debug["final_response_size"] = len([]rune(cleaned))
	debug["processing_applied"] = append(debug["processing_applied"].([]any), "base64_image_conversion")
	finishWebDebug(env, "web_extract_tool", debug, "")
	return cleaned
}

var webSearchSchema = map[string]any{
	"name":        "web_search",
	"description": "Search the web: returns titles, URLs and snippets (5 by default). Read pages with web_extract. The query is passed through to the configured backend, so operators such as site:domain, filetype:pdf, intitle:word, -term, and \"exact phrase\" may work when the backend supports them.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "The search query to look up on the web. You may include backend-supported operators such as site:example.com, filetype:pdf, intitle:word, -term, or \"exact phrase\"."},
			"limit": map[string]any{"type": "integer", "description": "Maximum number of results to return. Defaults to 5.", "minimum": 1, "maximum": 100, "default": 5},
		},
		"required": []any{"query"},
	},
}

var webExtractSchema = map[string]any{
	"name":        "web_extract",
	"description": "Extract content from web page URLs. Returns clean page content in markdown/text (no LLM summarization — fast). Also works with PDF URLs (arxiv papers, documents) — pass the PDF link directly. Pages within the char budget (default 15000) return whole; larger pages return a head+tail window with a footer telling you the full text's saved file path and the read_file call to page through the omitted middle. Inline images appear as [IMAGE: alt] placeholders; real image URLs are kept as links. If a URL fails, times out, or needs JS, login or clicks, use a browser tool instead.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"urls":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "List of URLs to extract content from (max 5 URLs per call)", "maxItems": 5},
			"char_limit": map[string]any{"type": "integer", "description": "Optional per-page character budget sent back (default 15000). Pages larger than this are head+tail truncated with the full text stored to disk. Raise it when you need more of a long page inline.", "minimum": 2000},
		},
		"required": []any{"urls"},
	},
}

// Registers web_search and web_extract in the web toolset
func registerWebTools() {
	Register(Def{Name: "web_search", Toolset: "web", Schema: webSearchSchema, Check: CheckWebAPIKey, RequiresEnv: webRequiresEnv(), Emoji: "\U0001F50D", MaxResultChars: 100_000,
		Handler: func(ctx context.Context, call *Call) string {
			limit := call.Args["limit"]
			if limit == nil {
				limit = 5
			}
			return WebSearchTool(ctx, call.Env, argS(call.Args, "query"), limit)
		}})
	Register(Def{Name: "web_extract", Toolset: "web", Schema: webExtractSchema, Check: CheckWebAPIKey, RequiresEnv: webRequiresEnv(), Emoji: "\U0001F4C4", MaxResultChars: 100_000,
		Handler: func(ctx context.Context, call *Call) string {
			var items []any
			if list, ok := call.Args["urls"].([]any); ok {
				items = list
				if len(items) > 5 {
					items = items[:5]
				}
			}
			return WebExtractTool(ctx, call.Env, items, "markdown", call.Args["char_limit"])
		}})
}

// EnsurePluginsDiscovered runs idempotent plugin discovery for the profile home of a ctx, installed by the plugins package
var EnsurePluginsDiscovered func(ctx context.Context) error

var errPluginDiscoveryUnwired = errors.New("plugin discovery is not installed")

// Discovers plugins so plugin registered web providers exist before dispatch, a failure logged as a warning
func ensureWebPluginsLoaded(ctx context.Context, env *Env) {
	err := errPluginDiscoveryUnwired
	if EnsurePluginsDiscovered != nil {
		err = EnsurePluginsDiscovered(visionCtx(ctx, env))
	}
	if err != nil {
		envLog(env).Warn("Web plugin discovery failed (non-fatal): " + err.Error())
	}
}
