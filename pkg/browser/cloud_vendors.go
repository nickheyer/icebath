package browser

import (
	"crypto/rand"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrequests"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Optional env knob as os.environ.get returns it
func osEnvOr(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return fallback
}

// Browserbase paid features dropped in order on a 402, with the warning each drop logs
var browserbasePaidFallbacks = [][2]string{
	{"keepAlive", "keepAlive may require paid plan (402), retrying without it. Sessions may timeout during long operations."},
	{"proxies", "Proxies unavailable (402), retrying without proxies. Bot detection may be less effective."},
}

// Browserbase cloud browser with direct BROWSERBASE_API_KEY and BROWSERBASE_PROJECT_ID only
type BrowserbaseBrowserProvider struct{ CloudBrowserProvider }

func newBrowserbaseProvider() *BrowserbaseBrowserProvider {
	p := &BrowserbaseBrowserProvider{CloudBrowserProvider{providerID: "browserbase", label: "Browserbase",
		module: "plugins.browser.browserbase.provider", releaseMethod: "post", releasePath: "/v1/sessions/{session_id}",
		missingCredentialsError: "Browserbase requires BROWSERBASE_API_KEY and BROWSERBASE_PROJECT_ID environment variables.",
		closeFailFmt:            "Failed to close session %s: HTTP %s - %s", setupTag: "Cloud browser with stealth and proxies",
		setupEnvVars: [][3]string{{"BROWSERBASE_API_KEY", "Browserbase API key", "https://browserbase.com"},
			{"BROWSERBASE_PROJECT_ID", "Browserbase project ID", ""}}}}
	p.hooks = p
	return p
}

func (p *BrowserbaseBrowserProvider) configOrNone(env *tools.Env) (*cloudConfig, error) {
	s := cfgOf(env)
	apiKey, projectID := s.rawSecret("BROWSERBASE_API_KEY"), s.rawSecret("BROWSERBASE_PROJECT_ID")
	if apiKey == "" || projectID == "" {
		return nil, nil
	}
	base := s.rawSecret("BROWSERBASE_BASE_URL")
	if base == "" {
		base = "https://api.browserbase.com"
	}
	return &cloudConfig{APIKey: apiKey, ProjectID: projectID, BaseURL: strings.TrimRight(base, "/")}, nil
}

func (p *BrowserbaseBrowserProvider) config(env *tools.Env) (*cloudConfig, error) {
	return p.baseConfig(env)
}

func (p *BrowserbaseBrowserProvider) headers(_ *tools.Env, c *cloudConfig) (map[string]string, error) {
	return map[string]string{"Content-Type": "application/json", "X-BB-API-Key": c.APIKey}, nil
}

func (p *BrowserbaseBrowserProvider) releaseHeaders(env *tools.Env, c *cloudConfig) (map[string]string, error) {
	return p.headers(env, c)
}

func (p *BrowserbaseBrowserProvider) releaseBody(c *cloudConfig) any {
	body := pyjson.NewObject()
	body.Set("projectId", c.ProjectID)
	body.Set("status", "REQUEST_RELEASE")
	return body
}

// Creates a session with the configured features, retrying without each paid feature a 402 refuses
func (p *BrowserbaseBrowserProvider) CreateSession(env *tools.Env, taskID string) (map[string]any, error) {
	c, err := p.config(env)
	if err != nil {
		return nil, err
	}
	log := p.log(env)
	proxies := strings.ToLower(osEnvOr("BROWSERBASE_PROXIES", "true")) != "false"
	stealth := strings.ToLower(osEnvOr("BROWSERBASE_ADVANCED_STEALTH", "false")) == "true"
	keepAlive := strings.ToLower(osEnvOr("BROWSERBASE_KEEP_ALIVE", "true")) != "false"
	customTimeout := os.Getenv("BROWSERBASE_SESSION_TIMEOUT")
	cfg := pyjson.NewObject()
	cfg.Set("projectId", c.ProjectID)
	if keepAlive {
		cfg.Set("keepAlive", true)
	}
	if customTimeout != "" {
		if n, ok := pyint.Parse(customTimeout); !ok {
			log.Warn("Invalid BROWSERBASE_SESSION_TIMEOUT value: " + customTimeout)
		} else if n.Sign() > 0 {
			cfg.Set("timeout", pyint.Native(n))
		}
	}
	if proxies {
		cfg.Set("proxies", true)
	}
	if stealth {
		cfg.Set("browserSettings", map[string]any{"advancedStealth": true})
	}
	url := c.BaseURL + "/v1/sessions"
	headers, _ := p.headers(env, c)
	resp, err := p.postCreate(url, headers, cfg, true)
	if err != nil {
		return nil, err
	}
	dropped := map[string]bool{}
	for _, fb := range browserbasePaidFallbacks {
		if _, held := cfg.Get(fb[0]); resp.Status == 402 && held {
			dropped[fb[0]] = true
			log.Warn(fb[1])
			cfg.Delete(fb[0])
			if resp, err = p.postCreate(url, headers, cfg, true); err != nil {
				return nil, err
			}
		}
	}
	if err := p.checkCreated(resp); err != nil {
		return nil, err
	}
	data, err := responseJSON(resp)
	if err != nil {
		return nil, err
	}
	name := cloudSessionName(taskID)
	_, timed := cfg.Get("timeout")
	order := []string{"basic_stealth", "proxies", "advanced_stealth", "keep_alive", "custom_timeout"}
	features := map[string]any{"basic_stealth": true, "proxies": proxies && !dropped["proxies"], "advanced_stealth": stealth,
		"keep_alive": keepAlive && !dropped["keepAlive"], "custom_timeout": customTimeout != "" && timed}
	var on []string
	for _, k := range order {
		if features[k].(bool) {
			on = append(on, k)
		}
	}
	log.Info(fmt.Sprintf("Created Browserbase session %s with features: %s", name, strings.Join(on, ", ")))
	id, err := jsonItem(data, "id")
	if err != nil {
		return nil, err
	}
	cdp, err := jsonItem(data, "connectUrl")
	if err != nil {
		return nil, err
	}
	return map[string]any{"session_name": name, "bb_session_id": id, "cdp_url": cdp, "features": features}, nil
}

const (
	browserUseBaseURL            = "https://api.browser-use.com/api/v3"
	browserUseManagedTimeoutMins = 5
	browserUseManagedProxyCC     = "us"
)

// Managed create idempotency keys per task, kept across a retryable failure
var (
	pendingCreateMu   sync.Mutex
	pendingCreateKeys = map[string]string{}
)

func pendingCreateKey(taskID string) string {
	pendingCreateMu.Lock()
	defer pendingCreateMu.Unlock()
	if existing := pendingCreateKeys[taskID]; existing != "" {
		return existing
	}
	buf := make([]byte, 16)
	rand.Read(buf)
	created := fmt.Sprintf("browser-use-session-create:%x", buf)
	pendingCreateKeys[taskID] = created
	return created
}

func clearPendingCreateKey(taskID string) {
	pendingCreateMu.Lock()
	delete(pendingCreateKeys, taskID)
	pendingCreateMu.Unlock()
}

// Retryable failure keeping the key, any 5xx or a 409 whose error message says the request is in progress
func shouldPreservePendingCreateKey(r *pyrequests.Response) bool {
	if r.Status >= 500 {
		return true
	}
	if r.Status != 409 {
		return false
	}
	payload, err := responseJSON(r)
	if err != nil {
		return false
	}
	errVal, _, _ := jsonGet(payload, "error")
	msg, _, isObject := jsonGet(errVal, "message")
	if !isObject {
		return false
	}
	if !pyrepr.Truthy(msg) {
		msg = ""
	}
	return strings.Contains(strings.ToLower(pyrepr.Str(msg)), "already in progress")
}

// Browser Use cloud browser, direct BROWSER_USE_API_KEY or the managed Nous tool gateway
type BrowserUseBrowserProvider struct{ CloudBrowserProvider }

func newBrowserUseProvider() *BrowserUseBrowserProvider {
	p := &BrowserUseBrowserProvider{CloudBrowserProvider{providerID: "browser-use", label: "Browser Use",
		module: "plugins.browser.browser_use.provider", releaseMethod: "patch", releasePath: "/browsers/{session_id}"}}
	p.hooks = p
	return p
}

// Availability without the synchronous OAuth refresh
func (p *BrowserUseBrowserProvider) IsAvailable(env *tools.Env) bool {
	c, err := p.configOrNoneRefresh(env, false)
	if err != nil {
		panic(err)
	}
	return c != nil
}

func (p *BrowserUseBrowserProvider) configOrNone(env *tools.Env) (*cloudConfig, error) {
	return p.configOrNoneRefresh(env, true)
}

// Strict selection, nous managed only, another stored vendor direct only, none stored direct then managed
func (p *BrowserUseBrowserProvider) configOrNoneRefresh(env *tools.Env, refreshToken bool) (*cloudConfig, error) {
	managed := func() (*cloudConfig, error) {
		var reader func(*tools.Env) string
		if !refreshToken {
			reader = tools.PeekNousAccessToken
		}
		gw, err := tools.ResolveManagedToolGateway(env, "browser-use", nil, reader)
		if err != nil || gw == nil {
			return nil, err
		}
		return &cloudConfig{APIKey: gw.NousUserToken, BaseURL: strings.TrimRight(gw.GatewayOrigin, "/"), ManagedMode: true}, nil
	}
	apiKey := cfgOf(env).rawSecret("BROWSER_USE_API_KEY")
	selected := tools.ReadSelection(env, "browser")
	var direct *cloudConfig
	if apiKey != "" {
		direct = &cloudConfig{APIKey: apiKey, BaseURL: browserUseBaseURL}
	}
	if selected == tools.NousManagedProvider {
		return managed()
	}
	if selected != "" || direct != nil {
		return direct, nil
	}
	return managed()
}

// Config or the ValueError naming why the selected or default route is unavailable
func (p *BrowserUseBrowserProvider) config(env *tools.Env) (*cloudConfig, error) {
	c, err := p.configOrNone(env)
	if err != nil || c != nil {
		return c, err
	}
	selected := tools.ReadSelection(env, "browser")
	if selected == tools.NousManagedProvider {
		return nil, valueError(tools.SelectionError("browser", tools.NousManagedProvider,
			"the Nous Tool Gateway is not available (not entitled or unreachable)"))
	}
	if selected != "" {
		return nil, valueError(tools.SelectionError("browser", selected, "BROWSER_USE_API_KEY is not set"))
	}
	if tools.ManagedNousToolsEnabled(env, false) {
		return nil, valueError("Browser Use requires either a direct BROWSER_USE_API_KEY credential or a managed Browser Use gateway configuration.")
	}
	return nil, valueError("Browser Use requires a direct BROWSER_USE_API_KEY credential.")
}

func (p *BrowserUseBrowserProvider) headers(_ *tools.Env, c *cloudConfig) (map[string]string, error) {
	return map[string]string{"Content-Type": "application/json", "X-Browser-Use-API-Key": c.APIKey}, nil
}

func (p *BrowserUseBrowserProvider) releaseHeaders(env *tools.Env, c *cloudConfig) (map[string]string, error) {
	return p.headers(env, c)
}

func (p *BrowserUseBrowserProvider) releaseBody(*cloudConfig) any {
	return map[string]any{"action": "stop"}
}

// Creates a browser, managed mode forwarding its idempotency key and propagating raw network errors
func (p *BrowserUseBrowserProvider) CreateSession(env *tools.Env, taskID string) (map[string]any, error) {
	c, err := p.config(env)
	if err != nil {
		return nil, err
	}
	headers, _ := p.headers(env, c)
	payload := pyjson.NewObject()
	if c.ManagedMode {
		headers["X-Idempotency-Key"] = pendingCreateKey(taskID)
		payload.Set("timeout", browserUseManagedTimeoutMins)
		payload.Set("proxyCountryCode", browserUseManagedProxyCC)
	}
	resp, err := p.postCreate(c.BaseURL+"/browsers", headers, payload, !c.ManagedMode)
	if err != nil {
		return nil, err
	}
	if !responseOK(resp) && c.ManagedMode && !shouldPreservePendingCreateKey(resp) {
		clearPendingCreateKey(taskID)
	}
	if err := p.checkCreated(resp); err != nil {
		return nil, err
	}
	data, err := responseJSON(resp)
	if err != nil {
		return nil, err
	}
	if c.ManagedMode {
		clearPendingCreateKey(taskID)
	}
	name := cloudSessionName(taskID)
	p.log(env).Info("Created Browser Use session " + name)
	id, err := jsonItem(data, "id")
	if err != nil {
		return nil, err
	}
	cdp, _, _ := jsonGet(data, "cdpUrl")
	if !pyrepr.Truthy(cdp) {
		cdp, _, _ = jsonGet(data, "connectUrl")
	}
	if !pyrepr.Truthy(cdp) {
		cdp = ""
	}
	expires, _, _ := jsonGet(data, "timeoutAt")
	var callID any
	if values := resp.Header.Values("x-external-call-id"); c.ManagedMode && len(values) > 0 {
		callID = strings.Join(values, ", ")
	}
	return map[string]any{"session_name": name, "bb_session_id": id, "cdp_url": cdp, "expires_at": expires,
		"features": map[string]any{"browser_use": true}, "external_call_id": callID}, nil
}

const firecrawlBaseURL = "https://api.firecrawl.dev"

// Firecrawl cloud browser on /v2/browser, explicit selection only
type FirecrawlBrowserProvider struct{ CloudBrowserProvider }

func newFirecrawlProvider() *FirecrawlBrowserProvider {
	p := &FirecrawlBrowserProvider{CloudBrowserProvider{providerID: "firecrawl", label: "Firecrawl",
		module: "plugins.browser.firecrawl.provider", releaseMethod: "delete", releasePath: "/v2/browser/{session_id}",
		createLabelSuffix: " browser", setupTag: "Cloud browser with remote execution",
		setupEnvVars: [][3]string{{"FIRECRAWL_API_KEY", "Firecrawl API key", "https://firecrawl.dev"}}}}
	p.hooks = p
	return p
}

func (p *FirecrawlBrowserProvider) apiURL(env *tools.Env) string {
	if v := cfgOf(env).rawSecret("FIRECRAWL_API_URL"); v != "" {
		return v
	}
	return firecrawlBaseURL
}

func (p *FirecrawlBrowserProvider) configOrNone(env *tools.Env) (*cloudConfig, error) {
	if cfgOf(env).rawSecret("FIRECRAWL_API_KEY") == "" {
		return nil, nil
	}
	return &cloudConfig{BaseURL: p.apiURL(env)}, nil
}

// Never raises, a missing key surfacing from the headers inside the request
func (p *FirecrawlBrowserProvider) config(env *tools.Env) (*cloudConfig, error) {
	return &cloudConfig{BaseURL: p.apiURL(env)}, nil
}

func (p *FirecrawlBrowserProvider) headers(env *tools.Env, _ *cloudConfig) (map[string]string, error) {
	key := cfgOf(env).rawSecret("FIRECRAWL_API_KEY")
	if key == "" {
		return nil, valueError("FIRECRAWL_API_KEY environment variable is required. Get your key at https://firecrawl.dev")
	}
	return map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + key}, nil
}

func (p *FirecrawlBrowserProvider) releaseHeaders(env *tools.Env, c *cloudConfig) (map[string]string, error) {
	return p.headers(env, c)
}

func (p *FirecrawlBrowserProvider) releaseBody(*cloudConfig) any { return nil }

// Creates a browser with FIRECRAWL_BROWSER_TTL seconds of life, 300 when unset or unparseable
func (p *FirecrawlBrowserProvider) CreateSession(env *tools.Env, taskID string) (map[string]any, error) {
	var ttl any = 300
	if n, ok := pyint.Parse(osEnvOr("FIRECRAWL_BROWSER_TTL", "300")); ok {
		ttl = pyint.Native(n)
	}
	headers, err := p.headers(env, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.postCreate(p.apiURL(env)+"/v2/browser", headers, map[string]any{"ttl": ttl}, true)
	if err != nil {
		return nil, err
	}
	if err := p.checkCreated(resp); err != nil {
		return nil, err
	}
	data, err := responseJSON(resp)
	if err != nil {
		return nil, err
	}
	name := cloudSessionName(taskID)
	p.log(env).Info("Created Firecrawl browser session " + name)
	id, err := jsonItem(data, "id")
	if err != nil {
		return nil, err
	}
	cdp, err := jsonItem(data, "cdpUrl")
	if err != nil {
		return nil, err
	}
	return map[string]any{"session_name": name, "bb_session_id": id, "cdp_url": cdp, "features": map[string]any{"firecrawl": true}}, nil
}

// One bundled cloud browser backend plugin registering its provider
type browserBackendPlugin struct {
	dir, description string
	provider         func() BrowserProvider
}

func (b browserBackendPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: "browser-" + strings.ReplaceAll(b.dir, "_", "-"), Version: "1.0.0", Description: b.description,
		Author: "NousResearch", Kind: "backend", Source: "bundled", Key: "browser/" + b.dir}
}

func (b browserBackendPlugin) Register(ctx *plugins.Context) error {
	_, err := ctx.RegisterBrowserProvider(b.provider())
	return err
}

// The bundled cloud browser plugins in plugins/browser directory order
var bundledBrowserPlugins = []browserBackendPlugin{
	{dir: "browser_use", description: "Browser Use (https://browser-use.com) cloud browser backend. Supports both direct BROWSER_USE_API_KEY and the managed Nous tool gateway. Also powers the 'Nous Subscription' UX flow that bills usage to a Nous subscription.",
		provider: func() BrowserProvider { return newBrowserUseProvider() }},
	{dir: "browserbase", description: "Browserbase (https://browserbase.com) cloud browser backend. Requires BROWSERBASE_API_KEY + BROWSERBASE_PROJECT_ID. Supports stealth, proxies, and keep-alive sessions; auto-falls-back when paid features are unavailable.",
		provider: func() BrowserProvider { return newBrowserbaseProvider() }},
	{dir: "firecrawl", description: "Firecrawl (https://firecrawl.dev) cloud browser backend. Requires FIRECRAWL_API_KEY. Distinct from the firecrawl WEB search/extract plugin — the two share an API key but operate on different endpoints.",
		provider: func() BrowserProvider { return newFirecrawlProvider() }},
}

func init() {
	for _, p := range bundledBrowserPlugins {
		plugins.RegisterBundled(p)
	}
}
