package providers

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/auth/azure"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	ProviderAzureFoundry      = "azure-foundry"
	azureFoundryKeyEnv        = "AZURE_FOUNDRY_API_KEY"
	azureAnthropicAPIVersion  = "2025-04-15"
	azureDetectErrorBodyLimit = 64 * 1024
	azureAnthropicProbeBody   = `{"model": "probe", "max_tokens": 1, "messages": [{"role": "user", "content": "ping"}]}`
	azureEntraSource          = "entra_id"
	azureEntraStatusHint      = "Entra ID sign-in is available; live credential validation is skipped here. Run `icebath doctor` to verify token acquisition."
)

var azureOpenAIProbeAPIVersions = []string{"2025-04-01-preview", "2024-10-21"}

type azureFoundryProfile struct {
	base
	d        Deps
	identity homeBound[*azure.Identity]
}

type EntraOwner interface {
	Identity(ctx context.Context) (*azure.Identity, error)
	EntraCredential(ctx context.Context, scope string) (auth.Credential, error)
	AuthStatus(ctx context.Context) (auth.Status, bool)
}

func newAzureFoundry(d Deps) Profile {
	return &azureFoundryProfile{d: d, base: base{spec: Spec{
		Name: ProviderAzureFoundry, Aliases: []string{"azure", "azure-ai-foundry", "azure-ai"}, DisplayName: "Azure Foundry",
		Description: "Microsoft Foundry - OpenAI-compatible endpoint (user-supplied base URL)",
		SignupURL:   "https://ai.azure.com/", EnvVars: []string{azureFoundryKeyEnv, "AZURE_FOUNDRY_BASE_URL"},
		AuthType: authTypeAPIKey,
	}}}
}

func (p *azureFoundryProfile) Identity(ctx context.Context) (*azure.Identity, error) {
	return p.identity.get(ctx, p.d, func(env func(name string) string, key homeKey) (*azure.Identity, error) {
		return azure.New(azure.Options{Secrets: env, Routed: key.routed, Multiplex: key.multiplex,
			HTTP: p.d.HTTP, Host: p.d.Host, Toolchain: p.d.Toolchain})
	})
}

func (p *azureFoundryProfile) EntraCredential(ctx context.Context, scope string) (auth.Credential, error) {
	id, err := p.Identity(ctx)
	if err != nil {
		return auth.Credential{}, err
	}
	tokens, err := id.TokenSource(azure.NewConfig(strings.TrimSpace(scope)))
	if err != nil {
		return auth.Credential{}, err
	}
	return auth.Credential{Kind: auth.CredToken, Tokens: tokens, Source: azureEntraSource}, nil
}

func EntraAuthorizer(tokens auth.TokenSource, sendsAsAPIKey func(token string) bool) wire.Authorizer {
	return azure.BearerAuthorizer{Tokens: tokens, SendsAsAPIKey: sendsAsAPIKey}
}

func (p *azureFoundryProfile) AuthStatus(ctx context.Context) (auth.Status, bool) {
	status := auth.Status{Provider: ProviderAzureFoundry}
	var model config.ModelConfig
	if p.d.Config != nil {
		if cfg, err := p.d.Config.LoadReadonly(ctx); err == nil {
			model = cfg.Model.ModelConfig
		}
	}
	mode := strings.ToLower(strings.TrimSpace(model.AuthMode))
	if mode == "" {
		mode = string(azure.ModeAPIKey)
	}
	status.AuthMode = mode
	status.BaseURL = strings.TrimSpace(model.BaseURL)
	if mode == string(azure.ModeEntraID) {
		entra := map[string]any{}
		if model.Entra != nil {
			entra["scope"] = model.Entra.Scope
		}
		status.LoggedIn = true
		status.Hint = azureEntraStatusHint
		status.Extra = map[string]string{
			"scope":               azure.ConfigFromMap(entra, azure.DefaultScope).Scope,
			"credential_probe":    "not_run",
			"credential_verified": "false",
		}
		return status, true
	}
	status.LoggedIn = auth.HasUsableSecret(p.storedKey(ctx), 4)
	return status, true
}

func (p *azureFoundryProfile) storedKey(ctx context.Context) string {
	if p.d.Config == nil {
		return os.Getenv(azureFoundryKeyEnv)
	}
	key, _, err := p.d.Config.Env().GetPreferFile(ctx, azureFoundryKeyEnv)
	if err != nil {
		return os.Getenv(azureFoundryKeyEnv)
	}
	return key
}

type AzureDetection struct {
	Mode          wire.Mode
	Models        []string
	Hostname      string
	Reason        string
	ModelsProbeOK bool
	Anthropic     bool
}

func stripTrailingV1(baseURL string) string {
	return strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
}

func looksLikeAnthropicPath(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	path := strings.TrimRight(strings.ToLower(u.EscapedPath()), "/")
	return strings.HasSuffix(path, "/anthropic") || strings.Contains(path+"/", "/anthropic/")
}

func jsonValueSet(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

func azureModelIDs(payload map[string]any) []string {
	data, ok := payload["data"].([]any)
	if !ok {
		return nil
	}
	ids := []string{}
	for _, raw := range data {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"id", "model", "name"} {
			value := item[field]
			if !jsonValueSet(value) {
				continue
			}
			if id, ok := value.(string); ok {
				ids = append(ids, id)
			}
			break
		}
	}
	return ids
}

func ProbeAzureModels(ctx context.Context, n Net, baseURL string, cred auth.Credential) (bool, []string) {
	base := strings.TrimRight(baseURL, "/")
	candidates := []string{base + "/models"}
	for _, version := range azureOpenAIProbeAPIVersions {
		candidates = append(candidates, base+"/models?api-version="+version)
	}
	for _, target := range candidates {
		status, body := azure.GetJSON(ctx, n.Client, target, cred, azure.DetectTimeout)
		if status != http.StatusOK || body == nil {
			continue
		}
		if ids := azureModelIDs(body); len(ids) > 0 {
			slog.Info(fmt.Sprintf("azure_detect: /models probe OK at %s (%d models)", target, len(ids)))
			return true, ids
		}
		if _, ok := body["data"]; ok {
			return true, []string{}
		}
	}
	return false, []string{}
}

func probeAzureAnthropicMessages(ctx context.Context, n Net, baseURL string, cred auth.Credential) bool {
	ctx, cancel := context.WithTimeout(ctx, azure.DetectTimeout)
	defer cancel()
	target := stripTrailingV1(baseURL) + "/v1/messages?api-version=" + azureAnthropicAPIVersion
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(azureAnthropicProbeBody))
	if err != nil {
		return false
	}
	azure.AuthorizeDetection(ctx, req, cred)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	resp, err := n.Client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, azureDetectErrorBodyLimit+1))
	if err != nil || len(body) > azureDetectErrorBodyLimit {
		return false
	}
	lowered := strings.ToLower(strings.ToValidUTF8(string(body), "�"))
	if strings.Contains(lowered, "anthropic") || (strings.Contains(lowered, `"type"`) && strings.Contains(lowered, `"error"`)) {
		return true
	}
	return resp.StatusCode == http.StatusBadRequest && (strings.Contains(lowered, "messages") || strings.Contains(lowered, "model"))
}

func DetectAzure(ctx context.Context, n Net, baseURL string, cred auth.Credential) AzureDetection {
	result := AzureDetection{Models: []string{}}
	if u, err := url.Parse(baseURL); err == nil {
		result.Hostname = strings.ToLower(u.Hostname())
	}
	if looksLikeAnthropicPath(baseURL) {
		result.Anthropic = true
		result.Mode = wire.ModeAnthropic
		result.Reason = "URL path ends in /anthropic → Anthropic Messages API"
		return result
	}
	if ok, models := ProbeAzureModels(ctx, n, baseURL, cred); ok {
		result.ModelsProbeOK = true
		result.Models = models
		result.Mode = wire.ModeChat
		if len(models) > 0 {
			result.Reason = fmt.Sprintf("GET /models returned %d model(s) — OpenAI-style endpoint", len(models))
		} else {
			result.Reason = "GET /models returned an OpenAI-shaped empty list — OpenAI-style endpoint"
		}
		return result
	}
	if probeAzureAnthropicMessages(ctx, n, baseURL, cred) {
		result.Anthropic = true
		result.Mode = wire.ModeAnthropic
		result.Reason = "Endpoint accepts Anthropic Messages shape"
		return result
	}
	result.Reason = "Could not probe endpoint (private network, missing model list, or " +
		"non-standard path) — falling back to manual API-mode selection"
	return result
}

func LookupAzureContextLength(ctx context.Context, model, baseURL string, cred auth.Credential, fallback int,
	lookup func(ctx context.Context, model, baseURL string, apiKey auth.Secret) (int, error)) (int, bool) {
	id := strings.TrimSpace(model)
	if id == "" {
		return 0, false
	}
	token, _ := azure.ResolveCredential(ctx, cred)
	n, err := lookup(ctx, id, baseURL, token)
	if err != nil {
		slog.Debug(fmt.Sprintf("azure_detect: context length lookup failed: %v", err))
		return 0, false
	}
	if n <= 0 || n == fallback {
		return 0, false
	}
	return n, true
}
