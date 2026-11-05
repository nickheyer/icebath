package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const nousManagedProvider = NousManagedProvider

const firecrawlCloudAPIURL = "https://api.firecrawl.dev"

const (
	firecrawlScrapeTimeoutMsg = "Scrape timed out after 60s — page may be too large or unresponsive. Try browser_exec instead."
	firecrawlUnsafeRedirect   = "Blocked: URL targets a private or internal network address"
)

// firecrawlClient is a minimal REST client for the keyed, self hosted, managed, and keyless Firecrawl routes
type firecrawlClient struct {
	apiURL string
	apiKey string
}

func newKeylessFirecrawlClient(apiURL string) *firecrawlClient {
	return &firecrawlClient{apiURL: strings.TrimRight(apiURL, "/")}
}

func newFirecrawlClient(apiURL, apiKey string) *firecrawlClient {
	if apiURL == "" {
		apiURL = firecrawlCloudAPIURL
	}
	return &firecrawlClient{apiURL: strings.TrimRight(apiURL, "/"), apiKey: apiKey}
}

func (c *firecrawlClient) post(ctx context.Context, path string, payload map[string]any) (any, error) {
	headers := map[string]string{"Content-Type": "application/json"}
	if c.apiKey != "" {
		headers["Authorization"] = "Bearer " + c.apiKey
	}
	status, body, _, err := postJSON(ctx, webHTTPClient(60*time.Second), c.apiURL+path, payload, headers)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		text := strings.TrimSpace(body)
		if len([]rune(text)) > 300 {
			text = string([]rune(text)[:300])
		}
		return nil, fmt.Errorf("HTTP %d: %s", status, text)
	}
	return decodeJSON(body)
}

func (c *firecrawlClient) search(ctx context.Context, query string, limit int) (any, error) {
	if limit <= 0 {
		limit = 5
	}
	return c.post(ctx, "/v2/search", map[string]any{"query": query, "limit": limit})
}

func (c *firecrawlClient) scrape(ctx context.Context, u string, formats []string, timeoutMs int) (any, error) {
	payload := map[string]any{"url": u, "formats": formats}
	if timeoutMs > 0 {
		payload["timeout"] = timeoutMs
	}
	return c.post(ctx, "/v2/scrape", payload)
}

func normalizeFirecrawlRows(values any) []WebSearchHit {
	var out []WebSearchHit
	for _, v := range asList(values) {
		row := asMap(v)
		if row == nil {
			continue
		}
		out = append(out, WebSearchHit{URL: asStr(row["url"]), Title: asStr(row["title"]), Description: asStr(row["description"]), Position: int(asFloat(row["position"]))})
	}
	return out
}

func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

// Search rows across the REST, SDK, and gateway response shapes
func firecrawlWebSearchResults(resp any) []WebSearchHit {
	plain := asMap(resp)
	if plain == nil {
		return nil
	}
	if data, ok := plain["data"].([]any); ok {
		return normalizeFirecrawlRows(data)
	}
	var candidates []any
	if data := asMap(plain["data"]); data != nil {
		candidates = append(candidates, data["web"], data["results"])
	}
	candidates = append(candidates, plain["web"], plain["results"])
	for _, c := range candidates {
		if rows := normalizeFirecrawlRows(c); len(rows) > 0 {
			return rows
		}
	}
	return nil
}

// Scrape payload body, the data member when it is an object
func firecrawlScrapePayload(raw any) map[string]any {
	plain := asMap(raw)
	if plain == nil {
		return map[string]any{}
	}
	if data := asMap(plain["data"]); data != nil {
		return data
	}
	return plain
}

// The URL Firecrawl actually fetched after redirects
func firecrawlFinalURL(metadata map[string]any, requested string) string {
	if u := asStr(metadata["url"]); u != "" {
		return u
	}
	return requested
}

// Flags a scrape whose target answered 400 or above from either status key spelling
func firecrawlWithScrapeStatus(p *WebPage, metadata map[string]any) *WebPage {
	if metadata == nil {
		return p
	}
	status, ok := metadata["statusCode"]
	if !ok {
		status = metadata["status_code"]
	}
	return withOriginStatus(p, status, metadata["error"])
}

type firecrawlConfig struct {
	mode   string
	apiURL string
	apiKey string
	key    string
}

func firecrawlEnv(env *Env, name string) string {
	return strings.TrimSpace(secretValue(env, name))
}

func isExplicitFirecrawlSelection(env *Env) bool {
	return webConfigSelects(env, "firecrawl")
}

// Direct Firecrawl config, keyed or self hosted through the SDK route else keyless on explicit selection
func directFirecrawlConfig(env *Env) *firecrawlConfig {
	apiKey := firecrawlEnv(env, "FIRECRAWL_API_KEY")
	apiURL := strings.TrimRight(firecrawlEnv(env, "FIRECRAWL_API_URL"), "/")
	if apiKey != "" || apiURL != "" {
		return &firecrawlConfig{mode: "sdk", apiURL: apiURL, apiKey: apiKey, key: "direct|" + apiURL + "|" + apiKey}
	}
	if isExplicitFirecrawlSelection(env) {
		return &firecrawlConfig{mode: "keyless", apiURL: firecrawlCloudAPIURL, key: "direct-keyless|" + firecrawlCloudAPIURL}
	}
	return nil
}

func firecrawlGatewayReady(env *Env) bool {
	return IsManagedToolGatewayReady(env, "firecrawl", nil, peekNousAccessToken)
}

// Routes through the keyless ring only without credentials and when the managed gateway is not selected
func firecrawlUseKeylessRing(env *Env, capability string) bool {
	if firecrawlEnv(env, "FIRECRAWL_API_KEY") != "" || firecrawlEnv(env, "FIRECRAWL_API_URL") != "" {
		return false
	}
	if ReadWebCapabilitySelection(env, capability) == NousManagedProvider {
		return false
	}
	if firecrawlGatewayReady(env) && !isExplicitFirecrawlSelection(env) {
		return false
	}
	return useKeyless(env, "firecrawl", "")
}

// CheckFirecrawlAPIKey reports whether the selected or any Firecrawl route is usable
func CheckFirecrawlAPIKey(env *Env) bool {
	usable := func(selected string) bool {
		if selected == NousManagedProvider {
			return firecrawlGatewayReady(env)
		}
		return directFirecrawlConfig(env) != nil || (selected == "" && firecrawlGatewayReady(env))
	}
	for _, cap := range []string{"search", "extract"} {
		if usable(ReadWebCapabilitySelection(env, cap)) {
			return true
		}
	}
	return false
}

// FirecrawlIsManagedRoute reports whether calls for a capability go through the Nous Tool Gateway
func FirecrawlIsManagedRoute(env *Env, capability string) bool {
	selected := ReadWebCapabilitySelection(env, capability)
	if selected != "" || isExplicitFirecrawlSelection(env) {
		return selected == NousManagedProvider
	}
	return directFirecrawlConfig(env) == nil && firecrawlGatewayReady(env)
}

var firecrawlClients = struct {
	sync.Mutex
	byHome map[string]struct {
		key    string
		client *firecrawlClient
	}
}{byHome: map[string]struct {
	key    string
	client *firecrawlClient
}{}}

func firecrawlUnconfiguredMessage(env *Env) string {
	message := "Web tools are not configured. Set FIRECRAWL_API_KEY for cloud Firecrawl or set FIRECRAWL_API_URL for a self-hosted Firecrawl instance."
	if managedNousToolsEnabled(env, false) {
		return message + " With your Nous subscription you can also use the Tool Gateway. run `icebath tools` and select Nous Subscription as the web provider."
	}
	return message + " " + nousToolGatewayUnavailableMessage(env, "managed Firecrawl web tools", false)
}

// Resolves the Firecrawl client for a capability under strict stored selection semantics
func firecrawlClientFor(env *Env, capability string) (*firecrawlClient, error) {
	selected := ReadWebCapabilitySelection(env, capability)
	direct := directFirecrawlConfig(env)
	managed := func() (*firecrawlConfig, error) {
		gw, err := ResolveManagedToolGateway(env, "firecrawl", nil, readNousAccessToken)
		if err != nil || gw == nil {
			return nil, err
		}
		return &firecrawlConfig{mode: "sdk", apiURL: gw.GatewayOrigin, apiKey: gw.NousUserToken, key: "tool-gateway|" + gw.GatewayOrigin + "|" + gw.NousUserToken}, nil
	}
	var resolved *firecrawlConfig
	var logMsg string
	var message func() string
	var err error
	switch {
	case selected == NousManagedProvider:
		resolved, err = managed()
		logMsg = "the Nous Subscription web selection is stored but the tool gateway is unavailable."
		message = func() string {
			return SelectionError("web", NousManagedProvider, "the Nous Tool Gateway is not available (not entitled or unreachable)")
		}
	case selected != "" || isExplicitFirecrawlSelection(env):
		resolved = direct
		logMsg = "direct Firecrawl selected but FIRECRAWL_API_KEY/FIRECRAWL_API_URL is not set."
		name := selected
		if name == "" {
			name = "firecrawl"
		}
		message = func() string {
			return SelectionError("web", name, "neither FIRECRAWL_API_KEY nor FIRECRAWL_API_URL is set")
		}
	case direct != nil:
		resolved = direct
	default:
		resolved, err = managed()
		logMsg = "missing direct config and tool-gateway auth."
		message = func() string { return firecrawlUnconfiguredMessage(env) }
	}
	if err != nil {
		return nil, err
	}
	if resolved == nil {
		envLog(env).Error("Firecrawl client initialization failed", "reason", logMsg)
		return nil, verbatim(message())
	}
	homeKey := envIdentity(env)
	firecrawlClients.Lock()
	defer firecrawlClients.Unlock()
	if cached, ok := firecrawlClients.byHome[homeKey]; ok && cached.key == resolved.key {
		return cached.client, nil
	}
	var client *firecrawlClient
	if resolved.mode == "keyless" {
		client = newKeylessFirecrawlClient(resolved.apiURL)
	} else {
		client = newFirecrawlClient(resolved.apiURL, resolved.apiKey)
	}
	firecrawlClients.byHome[homeKey] = struct {
		key    string
		client *firecrawlClient
	}{resolved.key, client}
	return client, nil
}

// ResetFirecrawlClients drops cached clients so the next call re resolves credentials
func ResetFirecrawlClients() {
	firecrawlClients.Lock()
	defer firecrawlClients.Unlock()
	firecrawlClients.byHome = map[string]struct {
		key    string
		client *firecrawlClient
	}{}
}

func firecrawlErrorEntry(u, errText, title string, raw bool, blocked *WebsiteBlock) *WebPage {
	p := &WebPage{URL: u, Title: title, Error: errText, HasRaw: raw}
	if blocked != nil {
		p.BlockedByPolicy = blockedByPolicy(blocked)
	}
	return p
}

// Scrapes one URL under a 60s deadline and re checks SSRF and policy against the final URL
func firecrawlScrapeOne(ctx context.Context, env *Env, u string, formats []string, format string) *WebPage {
	if blocked := checkWebsiteAccess(env, u); blocked != nil {
		envLog(env).Info("Blocked web_extract", "host", blocked.Host, "rule", blocked.Rule)
		return firecrawlErrorEntry(u, blocked.Message, "", false, blocked)
	}
	client, err := firecrawlClientFor(env, "extract")
	if err != nil {
		return firecrawlErrorEntry(u, err.Error(), "", true, nil)
	}
	envLog(env).Info("Firecrawl scraping", "url", u)
	scrapeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	raw, err := client.scrape(scrapeCtx, u, formats, 60000)
	if err != nil {
		if errors.Is(scrapeCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			envLog(env).Warn("Firecrawl scrape timed out", "url", u)
			return firecrawlErrorEntry(u, firecrawlScrapeTimeoutMsg, "", false, nil)
		}
		envLog(env).Debug("Firecrawl scrape failed", "url", u, "error", err)
		return firecrawlErrorEntry(u, err.Error(), "", true, nil)
	}
	payload := firecrawlScrapePayload(raw)
	metadata := asMap(payload["metadata"])
	if metadata == nil {
		metadata = map[string]any{}
	}
	if _, ok := metadata["sourceURL"]; !ok {
		if su := asStr(metadata["source_url"]); su != "" {
			metadata["sourceURL"] = su
		} else {
			metadata["sourceURL"] = u
		}
	}
	title := asStr(metadata["title"])
	finalURL := firecrawlFinalURL(metadata, u)
	if !isSafeURL(env, finalURL) {
		envLog(env).Info("Blocked redirected web_extract for unsafe final URL", "url", finalURL)
		return firecrawlErrorEntry(finalURL, firecrawlUnsafeRedirect, title, true, nil)
	}
	if blocked := checkWebsiteAccess(env, finalURL); blocked != nil {
		envLog(env).Info("Blocked redirected web_extract", "host", blocked.Host, "rule", blocked.Rule)
		return firecrawlErrorEntry(finalURL, blocked.Message, title, true, blocked)
	}
	markdown, html := asStr(payload["markdown"]), asStr(payload["html"])
	content := ""
	switch {
	case format == "markdown" || (format == "" && markdown != ""):
		content = markdown
	case html != "":
		content = html
	default:
		content = markdown
	}
	entry := &WebPage{URL: finalURL, Title: title, Content: content, RawContent: content, HasRaw: true, Metadata: metadata}
	return firecrawlWithScrapeStatus(entry, metadata)
}

type firecrawlProvider struct{ baseWebProvider }

func (p *firecrawlProvider) Available(env *Env) bool { return CheckFirecrawlAPIKey(env) }

// Search through the keyless ring or the resolved client, pre flight errors propagating
func (p *firecrawlProvider) Search(ctx context.Context, env *Env, query string, limit int) (*WebSearchResponse, error) {
	if IsInterrupted(ctx) {
		return searchFail("Interrupted"), nil
	}
	if firecrawlUseKeylessRing(env, "search") {
		envLog(env).Info("Firecrawl keyless search", "query", query, "limit", limit)
		return keylessSearchWithFailover(ctx, env, "firecrawl", query, limit), nil
	}
	envLog(env).Info("Firecrawl search", "query", query, "limit", limit)
	client, err := firecrawlClientFor(env, "search")
	if err != nil {
		return nil, err
	}
	resp, err := client.search(ctx, query, limit)
	if err != nil {
		envLog(env).Warn("Firecrawl search error", "error", err)
		return searchFail(fmt.Sprintf("Firecrawl search failed: %v", err)), nil
	}
	rows := firecrawlWebSearchResults(resp)
	envLog(env).Info("Firecrawl search results", "count", len(rows))
	return searchOK(rows), nil
}

// Extract scrapes each URL in turn, failures becoming entries with an error field
func (p *firecrawlProvider) Extract(ctx context.Context, env *Env, urls []string, format string) ([]*WebPage, error) {
	if IsInterrupted(ctx) {
		return extractInterrupted(urls), nil
	}
	if firecrawlUseKeylessRing(env, "extract") {
		envLog(env).Info("Firecrawl keyless extract", "urls", len(urls))
		return keylessExtractWithFailover(ctx, env, "firecrawl", urls), nil
	}
	formats := []string{"markdown", "html"}
	if format == "markdown" || format == "html" {
		formats = []string{format}
	}
	out := make([]*WebPage, 0, len(urls))
	for _, u := range urls {
		if IsInterrupted(ctx) {
			out = append(out, &WebPage{URL: u, Error: "Interrupted"})
			continue
		}
		out = append(out, firecrawlScrapeOne(ctx, env, u, formats, format))
	}
	return out, nil
}

// SetupSchema is the icebath tools picker entry
func (p *firecrawlProvider) SetupSchema() map[string]any {
	return setupSchema("Firecrawl", "keyless/paid · optional gateway",
		"Full search + extract; supports keyless cloud, direct API, and Nous tool-gateway routing.",
		"FIRECRAWL_API_KEY", "Firecrawl API key (optional; blank = keyless cloud or self-hosted)", "https://docs.firecrawl.dev/introduction", nil)
}

// Picker entry for a web provider with optional key env
func setupSchema(name, badge, tag, keyEnv, prompt, url string, extra map[string]any) map[string]any {
	envVars := []any{}
	if keyEnv != "" {
		envVars = append(envVars, map[string]any{"key": keyEnv, "prompt": prompt, "url": url})
	}
	out := map[string]any{"name": name, "badge": badge, "tag": tag, "env_vars": envVars}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// Picker entry for a keyless ring vendor with a paid variant
func keylessVariantSchema(display, keyEnv, keyURL, freeTag, paidTag string) map[string]any {
	paid := setupSchema(display+" · Paid (API key)", "paid", paidTag, keyEnv, display+" API key", keyURL, map[string]any{"web_tier": "paid"})
	return setupSchema(display+" · Free (keyless)", "free · no key", freeTag, "", "", "", map[string]any{"web_tier": "free", "variants": []any{paid}})
}

func init() {
	RegisterWebProvider(&firecrawlProvider{baseWebProvider{name: "firecrawl", display: "Firecrawl", keyEnv: "FIRECRAWL_API_KEY", extract: true, keyless: true}})
}
