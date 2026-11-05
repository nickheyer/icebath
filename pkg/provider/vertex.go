package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

const (
	vertexDefaultRegion     = "global"
	vertexAnthropicVersion  = "vertex-2023-10-16"
	vertexTokenMintFailure  = "Vertex AI OAuth token could not be minted (see the Vertex credential settings)"
	vertexMultiplexADCSkips = "Vertex ADC skipped for this profile: GOOGLE_APPLICATION_CREDENTIALS is set in the process environment " +
		"(from another profile's .env) but not in this profile's own config. Set VERTEX_CREDENTIALS_PATH in this " +
		"profile's .env instead of relying on ADC."
)

var vertexCloudPlatformScopes = []string{gcpCloudPlatformScope}

// Profile view the Vertex settings resolve through, the config tree and the profile secrets
type vertexEnv struct {
	cfg     map[string]any
	secrets *config.Secrets
}

// Secret from the profile .env, then the active secret scope which fails closed when multiplexing unscoped
func (e vertexEnv) secret(ctx context.Context, name string) string {
	if e.secrets != nil {
		if v, ok := e.secrets.File()[name]; ok {
			return v
		}
	}
	return SecretScopeFrom(ctx).GetSecretStr(name, "")
}

func (e vertexEnv) section() map[string]any {
	if m, ok := e.cfg["vertex"].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Env or secret value first, then the vertex config section, empty when neither is set
func (e vertexEnv) envOrConfig(ctx context.Context, envVar, key string) string {
	if v := strings.TrimSpace(e.secret(ctx, envVar)); v != "" {
		return v
	}
	if v := e.section()[key]; v != nil {
		return strings.TrimSpace(fmt.Sprint(v))
	}
	return ""
}

// Explicit region, then VERTEX_REGION, then config, then global
func (e vertexEnv) region(ctx context.Context, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := e.envOrConfig(ctx, "VERTEX_REGION", "region"); v != "" {
		return v
	}
	return vertexDefaultRegion
}

// Project id override from VERTEX_PROJECT_ID or config, empty to use the credentials project
func (e vertexEnv) projectOverride(ctx context.Context) string {
	return e.envOrConfig(ctx, "VERTEX_PROJECT_ID", "project_id")
}

func vertexPathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Existing explicit path, then VERTEX_CREDENTIALS_PATH, then GOOGLE_APPLICATION_CREDENTIALS read through the profile scope
func vertexResolveCredentialsPath(ctx context.Context, e vertexEnv, explicit string) string {
	if explicit != "" && vertexPathExists(explicit) {
		return explicit
	}
	for _, name := range []string{"VERTEX_CREDENTIALS_PATH", "GOOGLE_APPLICATION_CREDENTIALS"} {
		if p := e.secret(ctx, name); p != "" && vertexPathExists(p) {
			return p
		}
	}
	return ""
}

var (
	vertexResolveCredentialsPathFn = vertexResolveCredentialsPath
	vertexADCFn                    = gcpDefaultCredentials
)

// Credential cache key, the ADC sentinel, an unreadable path, or a path with its content digest
type vertexCacheKey struct {
	path   string
	digest string
}

type vertexCacheEntry struct {
	creds     *gcpCredentials
	projectID string
}

var (
	vertexCredsMu    sync.Mutex
	vertexCredsCache = map[vertexCacheKey]*vertexCacheEntry{}
)

// Clears the per content credential cache
func resetVertexCredentialCache() {
	vertexCredsMu.Lock()
	vertexCredsCache = map[vertexCacheKey]*vertexCacheEntry{}
	vertexCredsMu.Unlock()
}

// File bytes and the content keyed cache entry for one credential attempt
func vertexSASnapshot(path string) ([]byte, vertexCacheKey) {
	if path == "" {
		return nil, vertexCacheKey{path: "__adc__"}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, vertexCacheKey{path: path}
	}
	sum := sha256.Sum256(raw)
	return raw, vertexCacheKey{path: path, digest: hex.EncodeToString(sum[:])}
}

// Credentials for a cache miss, nil without error when ADC must be refused under multiplexing
func vertexLoadCredentials(ctx context.Context, path string, raw []byte) (*vertexCacheEntry, error) {
	if path != "" {
		if raw == nil {
			b, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			raw = b
		}
		var info map[string]any
		if err := json.Unmarshal(raw, &info); err != nil {
			return nil, err
		}
		creds, err := gcpServiceAccountFromInfo(info, vertexCloudPlatformScopes)
		if err != nil {
			return nil, err
		}
		return &vertexCacheEntry{creds: creds, projectID: creds.projectID}, nil
	}
	if IsMultiplexActive(ctx) && os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") != "" {
		slog.Default().Warn(vertexMultiplexADCSkips)
		return nil, nil
	}
	creds, project, err := vertexADCFn(ctx, vertexCloudPlatformScopes)
	if err != nil {
		return nil, err
	}
	return &vertexCacheEntry{creds: creds, projectID: project}, nil
}

// Cached credentials for the key, loading on a miss and dropping older entries of the same file
func vertexCachedCredentials(ctx context.Context, path string, raw []byte, key vertexCacheKey) (*vertexCacheEntry, error) {
	vertexCredsMu.Lock()
	cached := vertexCredsCache[key]
	vertexCredsMu.Unlock()
	if cached != nil {
		return cached, nil
	}
	loaded, err := vertexLoadCredentials(ctx, path, raw)
	if err != nil || loaded == nil {
		return nil, err
	}
	vertexCredsMu.Lock()
	defer vertexCredsMu.Unlock()
	vertexCredsCache[key] = loaded
	for k := range vertexCredsCache {
		if k != key && k.path == key.path {
			delete(vertexCredsCache, k)
		}
	}
	return loaded, nil
}

func vertexDropCached(key vertexCacheKey) {
	vertexCredsMu.Lock()
	delete(vertexCredsCache, key)
	vertexCredsMu.Unlock()
}

// True with no token, an expired token, or one within five minutes of expiry
func vertexNeedsRefresh(c *gcpCredentials) bool {
	return c.token == "" || c.Expired() || (!c.expiry.IsZero() && c.expiry.Sub(gcpNow()) < 300*time.Second)
}

// Fresh access token and project, retrying a failed ADC attempt with a service account file added later
func vertexGetCredentials(ctx context.Context, e vertexEnv, explicitPath string) (string, string) {
	path := vertexResolveCredentialsPathFn(ctx, e, explicitPath)
	raw, key := vertexSASnapshot(path)
	token, project, err := func() (string, string, error) {
		cached, err := vertexCachedCredentials(ctx, path, raw, key)
		if err != nil || cached == nil {
			return "", "", err
		}
		cached.creds.mu.Lock()
		defer cached.creds.mu.Unlock()
		if vertexNeedsRefresh(cached.creds) {
			if err := cached.creds.Refresh(ctx); err != nil {
				return "", "", err
			}
		}
		project := e.projectOverride(ctx)
		if project == "" {
			project = cached.projectID
		}
		return cached.creds.token, project, nil
	}()
	if err == nil {
		return token, project
	}
	slog.Default().Error(fmt.Sprintf("Failed to resolve Vertex AI credentials: %v", err))
	vertexDropCached(key)
	if path == "" {
		if saPath := vertexResolveCredentialsPathFn(ctx, e, explicitPath); saPath != "" {
			slog.Default().Info("ADC failed, retrying with service account: " + saPath)
			return vertexGetCredentials(ctx, e, saPath)
		}
	}
	return "", ""
}

// OpenAI compatible Vertex base URL, the bare host for the global region
func buildVertexBaseURL(projectID, region string) string {
	host := region + "-aiplatform.googleapis.com"
	if region == "global" {
		host = "aiplatform.googleapis.com"
	}
	return fmt.Sprintf("https://%s/v1beta1/projects/%s/locations/%s/endpoints/openapi", host, projectID, region)
}

// Access token and base URL, both empty when either cannot be resolved
func vertexGetConfig(ctx context.Context, e vertexEnv, explicitPath, region string) (string, string) {
	token, project := vertexGetCredentials(ctx, e, explicitPath)
	if token == "" || project == "" {
		return "", ""
	}
	return token, buildVertexBaseURL(project, e.region(ctx, region))
}

// Base URL from loadable credentials and a resolvable project without minting a token
func vertexGetBaseURL(ctx context.Context, e vertexEnv, explicitPath, region string) string {
	path := vertexResolveCredentialsPathFn(ctx, e, explicitPath)
	raw, key := vertexSASnapshot(path)
	cached, err := vertexCachedCredentials(ctx, path, raw, key)
	if err != nil {
		slog.Default().Debug(fmt.Sprintf("Vertex AI credentials could not be loaded: %v", err))
		vertexDropCached(key)
		return ""
	}
	project := e.projectOverride(ctx)
	if project == "" && cached != nil {
		project = cached.projectID
	}
	if cached == nil || project == "" {
		return ""
	}
	return buildVertexBaseURL(project, e.region(ctx, region))
}

// Fresh Vertex token minted on first use and reused until near expiry
func vertexAccessToken(ctx context.Context, e vertexEnv) (string, error) {
	token, _ := vertexGetCredentials(ctx, e, "")
	if token == "" {
		return "", errors.New(vertexTokenMintFailure)
	}
	return token, nil
}

// True with a resolvable service account path or an explicit project id, no network
func hasVertexCredentials(ctx context.Context, e vertexEnv) bool {
	return vertexResolveCredentialsPathFn(ctx, e, "") != "" || e.projectOverride(ctx) != ""
}

// True only for icebath scoped Vertex signals, an explicit project or a readable VERTEX_CREDENTIALS_PATH
func hasExplicitVertexConfig(env PoolEnv) bool {
	e := vertexEnv{cfg: env.config(), secrets: env.Secrets}
	ctx := env.ctx()
	if e.projectOverride(ctx) != "" {
		return true
	}
	saPath := e.secret(ctx, "VERTEX_CREDENTIALS_PATH")
	if saPath == "" {
		return false
	}
	st, err := os.Stat(saPath)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(saPath)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

type deferredTokenMintKey struct{}

// WithDeferredTokenMint resolves routes without exchanging short lived OAuth tokens up front
func WithDeferredTokenMint(ctx context.Context) context.Context {
	return context.WithValue(ctx, deferredTokenMintKey{}, true)
}

func deferredTokenMint(ctx context.Context) bool {
	v, _ := ctx.Value(deferredTokenMintKey{}).(bool)
	return v
}

// Vertex token source and base URL for the profile, minting once up front unless the mint is deferred
func vertexRuntimeConfig(ctx context.Context, cfg map[string]any, secrets *config.Secrets, h string) (func(context.Context) (string, error), string, bool) {
	if secrets == nil {
		secrets = config.NewSecrets(h)
	}
	if cfg == nil {
		tree, err := config.Load(h, secrets.Lookup())
		if err != nil {
			tree = config.Defaults()
		}
		cfg = map[string]any(tree)
	}
	e := vertexEnv{cfg: cfg, secrets: secrets}
	src := TagCredentialSource(func(c context.Context) (string, error) { return vertexAccessToken(c, e) }, CredentialSourceVertexOAuth).Func()
	if deferredTokenMint(ctx) {
		base := vertexGetBaseURL(ctx, e, "", "")
		return src, base, base != ""
	}
	token, base := vertexGetConfig(ctx, e, "", "")
	if token == "" || base == "" {
		return nil, "", false
	}
	return src, base, true
}

// ---- Anthropic on Vertex AI ----

// Claude on Vertex through publishers/anthropic rawPredict and streamRawPredict
type anthVertexTransport struct {
	route Route
}

func vertexTransport(route Route) (anthTransport, error) {
	return &anthVertexTransport{route: route}, nil
}

// Profile Vertex view loaded from the home the context routes to
func vertexHomeEnv(ctx context.Context) vertexEnv {
	h := home.Dir(ctx)
	secrets := config.NewSecrets(h)
	tree, err := config.Load(h, secrets.Lookup())
	if err != nil {
		tree = config.Defaults()
	}
	return vertexEnv{cfg: map[string]any(tree), secrets: secrets}
}

// Project and region named by a Vertex URL path segment pair projects/P/locations/R
func vertexProjectRegionFromURL(raw string) (string, string) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	project, region := "", ""
	for i := 0; i+1 < len(parts); i++ {
		switch parts[i] {
		case "projects":
			project = parts[i+1]
		case "locations":
			region = parts[i+1]
		}
	}
	return project, region
}

// Vertex API origin for a region, multi region rep hosts for us and eu
func vertexAnthropicOrigin(region string) string {
	switch region {
	case "global":
		return "https://aiplatform.googleapis.com"
	case "us", "eu":
		return "https://aiplatform." + region + ".rep.googleapis.com"
	}
	return "https://" + region + "-aiplatform.googleapis.com"
}

// Origin, project and region from the route base URL, completed from the profile Vertex settings
func (t *anthVertexTransport) target(ctx context.Context) (string, string, string, error) {
	origin, project, region := "", "", ""
	if base := strings.TrimSpace(t.route.BaseURL); base != "" {
		if u, err := url.Parse(base); err == nil && u.Scheme != "" && u.Host != "" {
			origin = u.Scheme + "://" + u.Host
			project, region = vertexProjectRegionFromURL(base)
		}
	}
	if project == "" || region == "" {
		e := vertexHomeEnv(ctx)
		if project == "" {
			project, region = vertexProjectRegionFromURL(vertexGetBaseURL(ctx, e, "", region))
		}
		if region == "" {
			region = e.region(ctx, "")
		}
	}
	if project == "" {
		return "", "", "", errors.New("provider: no Vertex AI project id resolves; set VERTEX_PROJECT_ID or vertex.project_id, or use credentials that embed one")
	}
	if origin == "" {
		origin = vertexAnthropicOrigin(region)
	}
	return origin, project, region, nil
}

// Bearer from the route token source, the route key, else the profile Vertex credentials
func (t *anthVertexTransport) bearer(ctx context.Context) (string, error) {
	if t.route.TokenSource != nil {
		token, err := t.route.TokenSource(ctx)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(token) == "" {
			return "", errors.New("token provider returned empty value")
		}
		return strings.TrimSpace(token), nil
	}
	if key := strings.TrimSpace(t.route.APIKey); key != "" {
		return key, nil
	}
	return vertexAccessToken(ctx, vertexHomeEnv(ctx))
}

// Request posts the body at the model rawPredict or streamRawPredict path, or the count-tokens model
func (t *anthVertexTransport) Request(ctx context.Context, op anthOperation, body map[string]any, headers map[string]string) (*http.Request, error) {
	origin, project, region, err := t.target(ctx)
	if err != nil {
		return nil, err
	}
	payload := make(map[string]any, len(body)+1)
	for k, v := range body {
		payload[k] = v
	}
	if _, ok := payload["anthropic_version"]; !ok {
		payload["anthropic_version"] = vertexAnthropicVersion
	}
	prefix := fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/anthropic/models/", origin, project, region)
	var target string
	if op == anthOpCountTokens {
		target = prefix + "count-tokens:rawPredict"
	} else {
		model, _ := payload["model"].(string)
		delete(payload, "model")
		if model == "" {
			model = t.route.Model
		}
		specifier := "rawPredict"
		if stream, _ := payload["stream"].(bool); stream {
			specifier = "streamRawPredict"
		}
		target = prefix + model + ":" + specifier
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("provider: encode vertex anthropic body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if stream, _ := payload["stream"].(bool); stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	for k, v := range headers {
		if strings.EqualFold(k, "anthropic-version") || strings.EqualFold(k, "x-api-key") || strings.EqualFold(k, "authorization") {
			continue
		}
		req.Header.Set(k, v)
	}
	token, err := t.bearer(ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return req, nil
}

func (t *anthVertexTransport) Events(resp *http.Response) anthEventSource {
	return newAnthSSEEvents(resp.Body, resp.Header.Clone())
}

// StreamUnavailable reports a Vertex endpoint that rejects streams or emits events out of order
func (t *anthVertexTransport) StreamUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "stream") && strings.Contains(msg, "not supported") {
		return true
	}
	return strings.Contains(msg, "unexpected event order")
}

func (t *anthVertexTransport) GuardrailIntervened(map[string]any) bool { return false }

func (t *anthVertexTransport) ResolveProfileARN(string) string { return "" }

func (t *anthVertexTransport) Betas(dropContext1M bool) []string {
	base := t.route.BaseURL
	if !isVertexAIEndpoint(base) {
		base = "https://aiplatform.googleapis.com"
	}
	return anthCommonBetasForBaseURL(base, dropContext1M)
}
