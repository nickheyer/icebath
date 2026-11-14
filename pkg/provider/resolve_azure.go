package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Default Entra scope for Azure AI Foundry
const ScopeAIAzureDefault = "https://ai.azure.com/.default"

// Callable api key minting a fresh Entra JWT per request, its credential built for the context profile
func azureEntraCredentialsIn(ctx context.Context, cfgEntra map[string]any) (func(context.Context) (string, error), error) {
	scope := ""
	if raw := cfgEntra["scope"]; pyrepr.Truthy(raw) {
		scope = pystr.Strip(pyrepr.Str(raw))
	}
	if scope == "" {
		scope = ScopeAIAzureDefault
	}
	return entraTokenProviderIn(ctx, scope)
}

// Callable api key minting a fresh Entra JWT per request for the process profile
func azureEntraCredentials(cfgEntra map[string]any) (func(context.Context) (string, error), error) {
	return azureEntraCredentialsIn(context.Background(), cfgEntra)
}

// Azure Foundry API key from the explicit arg, the .env file, then the environment
func (r *resolver) azureFoundryAPIKey(explicitAPIKey string) (string, error) {
	if explicitAPIKey != "" {
		return explicitAPIKey, nil
	}
	apiKey := strings.TrimSpace(r.secret("AZURE_FOUNDRY_API_KEY"))
	if apiKey == "" {
		return "", newAuthErr("azure-foundry", "Azure Foundry requires an API key. Set AZURE_FOUNDRY_API_KEY in "+
			"~/.icebath/.env or run 'icebath model' to configure. To use "+
			"keyless Microsoft Entra ID auth instead, set "+
			"model.auth_mode: entra_id in config.yaml (or pick "+
			"'Microsoft Entra ID' in 'icebath model').", "", false)
	}
	return apiKey, nil
}

// Azure Foundry from model.base_url and model.api_mode or explicit overrides with an API key or Entra token
func (r *resolver) resolveAzureFoundryRuntime(requestedProvider string, modelCfg map[string]any, explicitAPIKey string, forwardedToken func(context.Context) (string, error), explicitBaseURL, targetModel string) (*Runtime, error) {
	explicitAPIKey = strings.TrimSpace(explicitAPIKey)
	if forwardedToken != nil {
		explicitAPIKey = ""
	}
	explicitBaseURLClean := strings.TrimRight(strings.TrimSpace(explicitBaseURL), "/")
	cfgBaseURL, cfgAPIMode, cfgAuthMode := "", ModeChatCompletions, "api_key"
	cfgEntra := map[string]any{}
	if cfgProviderOf(modelCfg) == "azure-foundry" {
		cfgBaseURL = configBaseURLForProvider(modelCfg, "azure-foundry")
		if parsed := r.parseAPIMode(modelCfg["api_mode"]); parsed != "" {
			cfgAPIMode = parsed
		}
		if mode := strings.ToLower(strings.TrimSpace(cfgString(modelCfg["auth_mode"]))); mode != "" {
			cfgAuthMode = mode
		}
		if entra, ok := modelCfg["entra"].(map[string]any); ok {
			cfgEntra = entra
		}
	}
	model := strings.TrimSpace(targetModel)
	if model == "" {
		model = strings.TrimSpace(cfgString(modelCfg["default"]))
	}
	cfgAPIMode = azureInferredAPIMode(model, cfgAPIMode)
	envBaseURL := strings.TrimRight(strings.TrimSpace(r.secret("AZURE_FOUNDRY_BASE_URL")), "/")
	baseURL := explicitBaseURLClean
	if baseURL == "" {
		baseURL = cfgBaseURL
	}
	if baseURL == "" {
		baseURL = envBaseURL
	}
	if baseURL == "" {
		return nil, newAuthErr("azure-foundry", "Azure Foundry requires a base URL. Set it via 'icebath model' or "+
			"the AZURE_FOUNDRY_BASE_URL environment variable.", "", false)
	}
	if cfgAPIMode == ModeAnthropicMessages {
		baseURL = v1TailRe.ReplaceAllString(baseURL, "")
	}
	if cfgAuthMode == "entra_id" {
		if explicitAPIKey != "" {
			rt := r.runtime("azure-foundry", cfgAPIMode, baseURL, explicitAPIKey, "explicit")
			rt.AuthMode = "api_key"
			rt.Entra = map[string]any{}
			rt.RequestedProvider = requestedProvider
			return rt, nil
		}
		scope := strings.TrimSpace(cfgString(cfgEntra["scope"]))
		src := forwardedToken
		if src == nil {
			var err error
			src, err = azureEntraCredentialsIn(r.ctx, cfgEntra)
			if err != nil {
				return nil, err
			}
		}
		rt := r.runtime("azure-foundry", cfgAPIMode, baseURL, "", "entra_id")
		rt.TokenSource = src
		rt.AuthMode = "entra_id"
		rt.Entra = map[string]any{}
		if scope != "" {
			rt.Entra["scope"] = scope
		}
		rt.RequestedProvider = requestedProvider
		return rt, nil
	}
	apiKey, err := r.azureFoundryAPIKey(explicitAPIKey)
	if err != nil {
		return nil, err
	}
	source := "config"
	if explicitAPIKey != "" || explicitBaseURL != "" {
		source = "explicit"
	}
	rt := r.runtime("azure-foundry", cfgAPIMode, baseURL, apiKey, source)
	rt.AuthMode = "api_key"
	rt.RequestedProvider = requestedProvider
	return rt, nil
}

// ---- Azure Foundry endpoint auto detection (hermes_cli/azure_detect.py) ----

// Azure OpenAI api-version fallbacks for pre v1 resources, probed after the versionless GA path
var azureOpenAIProbeAPIVersions = []string{"2025-04-01-preview", "2024-10-21"}

const (
	azureAnthropicAPIVersion     = "2025-04-15"
	azureDetectJSONBodyMaxBytes  = 1024 * 1024
	azureDetectErrorBodyMaxBytes = 64 * 1024
	azureDetectTimeout           = 6 * time.Second
)

// AzureDetection is everything auto detection could gather from a base URL and credential
type AzureDetection struct {
	APIMode       APIMode
	Models        []string
	Hostname      string
	Reason        string
	ModelsProbeOK bool
	IsAnthropic   bool
}

// AzureDetector probes an Azure endpoint for its transport and model list
type AzureDetector struct {
	Client    *http.Client
	UserAgent string
}

// Sends through the credential safe opener, an injected client standing as the installed policy
func (d *AzureDetector) open(req *http.Request) (*http.Response, error) {
	if d != nil && d.Client != nil {
		opener, err := secureOpenerFromPolicy(&Opener{Transport: d.Client.Transport}, req.URL.String(), nil)
		if err != nil {
			return nil, err
		}
		return opener.Open(req, azureDetectTimeout)
	}
	return OpenCredentialedURL(req, azureDetectTimeout, nil, nil)
}

func (d *AzureDetector) userAgent() string {
	if d != nil && d.UserAgent != "" {
		return d.UserAgent
	}
	return "icebath/azure-detect"
}

// Token and credential mode, entra_id when a token source was supplied else api_key
func resolveAzureCredential(ctx context.Context, apiKey string, tokenProvider func(context.Context) (string, error)) (string, string) {
	if tokenProvider != nil {
		token, err := tokenProvider(ctx)
		if err != nil {
			slog.Default().Debug(fmt.Sprintf("azure_detect: token_provider failed: %v", err))
			return "", "entra_id"
		}
		return token, "entra_id"
	}
	if apiKey != "" {
		return apiKey, "api_key"
	}
	return "", "api_key"
}

// Request carrying the right auth headers for the credential mode
func (d *AzureDetector) authedRequest(ctx context.Context, method, target, apiKey string, tokenProvider func(context.Context) (string, error), body []byte) (*http.Request, error) {
	token, mode := resolveAzureCredential(ctx, apiKey, tokenProvider)
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	if token != "" {
		if mode != "entra_id" {
			req.Header.Set("api-key", token)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("User-Agent", d.userAgent())
	return req, nil
}

// Body up to a limit, erroring when the response exceeds it
func readLimitedBody(body io.Reader, limit int, label string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("%s exceeded %d bytes", label, limit)
	}
	return data, nil
}

// GET with auth headers returning the status and parsed JSON object, zero status when the call failed
func (d *AzureDetector) httpGetJSON(ctx context.Context, target, apiKey string, tokenProvider func(context.Context) (string, error)) (int, map[string]any) {
	req, err := d.authedRequest(ctx, http.MethodGet, target, apiKey, tokenProvider, nil)
	if err != nil {
		return 0, nil
	}
	resp, err := d.open(req)
	if err != nil {
		slog.Default().Debug(fmt.Sprintf("azure_detect: GET %s failed: %v", target, err))
		return 0, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return resp.StatusCode, nil
	}
	data, err := readLimitedBody(resp.Body, azureDetectJSONBodyMaxBytes, "Azure detection JSON response body")
	if err != nil {
		return resp.StatusCode, nil
	}
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, payload
}

// Strips a trailing /v1 so sub paths can be built
func stripTrailingV1(raw string) string {
	return v1TailRe.ReplaceAllString(strings.TrimRight(raw, "/"), "")
}

// True when the path ends in /anthropic or contains an /anthropic/ segment
func looksLikeAnthropicPath(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	path := strings.TrimRight(strings.ToLower(parsed.Path), "/")
	return strings.HasSuffix(path, "/anthropic") || strings.Contains(path+"/", "/anthropic/")
}

// Model ids from an OpenAI shaped models response, empty on any shape mismatch
func extractModelIDs(payload map[string]any) []string {
	data, ok := payload["data"].([]any)
	if !ok {
		return nil
	}
	var ids []string
	for _, item := range data {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if mid := firstTruthyString(m["id"], m["model"], m["name"]); mid != "" {
			ids = append(ids, mid)
		}
	}
	return ids
}

// Probes base/models for an OpenAI shaped response, versionless first then explicit api-versions
func (d *AzureDetector) probeOpenAIModels(ctx context.Context, baseURL, apiKey string, tokenProvider func(context.Context) (string, error)) (bool, []string) {
	baseURL = strings.TrimRight(baseURL, "/")
	candidates := []string{baseURL + "/models"}
	for _, v := range azureOpenAIProbeAPIVersions {
		candidates = append(candidates, baseURL+"/models?api-version="+v)
	}
	for _, target := range candidates {
		status, body := d.httpGetJSON(ctx, target, apiKey, tokenProvider)
		if status != 200 || body == nil {
			continue
		}
		if ids := extractModelIDs(body); len(ids) > 0 {
			slog.Default().Info(fmt.Sprintf("azure_detect: /models probe OK at %s (%d models)", target, len(ids)))
			return true, ids
		}
		if _, has := body["data"]; has {
			return true, nil
		}
	}
	return false, nil
}

// Zero token POST to base/v1/messages asking whether the endpoint recognises the Anthropic shape
func (d *AzureDetector) probeAnthropicMessages(ctx context.Context, baseURL, apiKey string, tokenProvider func(context.Context) (string, error)) bool {
	target := stripTrailingV1(baseURL) + "/v1/messages?api-version=" + azureAnthropicAPIVersion
	payload, _ := json.Marshal(map[string]any{"model": "probe", "max_tokens": 1, "messages": []map[string]any{{"role": "user", "content": "ping"}}})
	req, err := d.authedRequest(ctx, http.MethodPost, target, apiKey, tokenProvider, payload)
	if err != nil {
		return false
	}
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	resp, err := d.open(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 400 {
		return resp.StatusCode < 500
	}
	if resp.StatusCode >= 500 {
		return false
	}
	body, err := readLimitedBody(resp.Body, azureDetectErrorBodyMaxBytes, "Azure Anthropic probe error response body")
	if err != nil {
		return false
	}
	lowered := strings.ToLower(string(body))
	if strings.Contains(lowered, "anthropic") || (strings.Contains(lowered, `"type"`) && strings.Contains(lowered, `"error"`)) {
		return true
	}
	return resp.StatusCode == 400 && (strings.Contains(lowered, "messages") || strings.Contains(lowered, "model"))
}

// Detect inspects an Azure endpoint and describes its transport and models, advisory when api mode stays empty
func (d *AzureDetector) Detect(ctx context.Context, baseURL, apiKey string, tokenProvider func(context.Context) (string, error)) AzureDetection {
	result := AzureDetection{Hostname: strings.ToLower(Hostname(baseURL))}
	if looksLikeAnthropicPath(baseURL) {
		result.IsAnthropic = true
		result.APIMode = ModeAnthropicMessages
		result.Reason = "URL path ends in /anthropic → Anthropic Messages API"
		return result
	}
	if ok, models := d.probeOpenAIModels(ctx, baseURL, apiKey, tokenProvider); ok {
		result.ModelsProbeOK = true
		result.Models = models
		result.APIMode = ModeChatCompletions
		if len(models) > 0 {
			result.Reason = fmt.Sprintf("GET /models returned %d model(s) — OpenAI-style endpoint", len(models))
		} else {
			result.Reason = "GET /models returned an OpenAI-shaped empty list — OpenAI-style endpoint"
		}
		return result
	}
	if d.probeAnthropicMessages(ctx, baseURL, apiKey, tokenProvider) {
		result.IsAnthropic = true
		result.APIMode = ModeAnthropicMessages
		result.Reason = "Endpoint accepts Anthropic Messages shape"
		return result
	}
	result.Reason = "Could not probe endpoint (private network, missing model list, or " +
		"non-standard path) — falling back to manual API-mode selection"
	return result
}

// DetectAzureEndpoint runs auto detection with the default client
func DetectAzureEndpoint(ctx context.Context, baseURL, apiKey string, tokenProvider func(context.Context) (string, error)) AzureDetection {
	return (&AzureDetector{}).Detect(ctx, baseURL, apiKey, tokenProvider)
}

// AzureLookupContextLength returns the known context window for a model on an Azure endpoint or 0 when only a guess exists
func AzureLookupContextLength(ctx context.Context, model, baseURL, apiKey string, tokenProvider func(context.Context) (string, error)) int {
	modelID := strings.TrimSpace(model)
	if modelID == "" {
		return 0
	}
	token, _ := resolveAzureCredential(ctx, apiKey, tokenProvider)
	n := modelContextLength(ctx, modelID, baseURL, token)
	if n <= 0 || n == defaultFallbackContext {
		return 0
	}
	return n
}
