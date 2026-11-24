package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

// Output cap leaving room for a reasoning model's first thinking tokens, timeout in seconds and prompt of one live probe
const (
	fallbackLiveMaxTokens = 512
	fallbackLiveTimeout   = 30.0
	fallbackLivePrompt    = "Reply with exactly: OK"
)

// Router meta models that answer under the model they picked, so the id they report proves nothing
const (
	fallbackRouterAutoModel    = "openrouter/auto"
	fallbackRouterPresetMarker = "@preset/"
)

// Publisher path Vertex may name a model by on its OpenAI compatible surface, project scoped or not
var fallbackVertexPublisherPathRe = regexp.MustCompile(`^(?:projects/[^/]+/locations/[^/]+/)?publishers/([^/]+)/models/`)

// Dated snapshot or latest pointer a provider may answer with for the requested alias, anything else is another model
var fallbackSnapshotSuffixRe = regexp.MustCompile(`^(?:-\p{Nd}{4}-?\p{Nd}{2}-?\p{Nd}{2}|-\p{Nd}{4}|-latest)?$`)

// Responses style terminal states that mean no answer even when partial text came back, incomplete still answered
var fallbackFailedStatuses = map[string]bool{"failed": true, "cancelled": true}

// Host labels of Azure endpoints, sovereign clouds included, which report the base model instead of the deployment name
var fallbackAzureHostLabels = []string{".openai.azure.", ".services.ai.azure.", ".cognitiveservices.azure."}

// FallbackEntryCheck is one fallback entry's verdict, InferenceReady and LatencyMs nil until a live probe ran
type FallbackEntryCheck struct {
	Provider       string
	Model          string
	BaseURL        string
	APIMode        string
	AuthSource     string
	Configured     bool
	AuthReady      bool
	InferenceReady *bool
	LatencyMs      *int
	ModelReturned  string
	ErrorClass     string
	Error          string
}

// Ready reports an entry that resolved with credentials and whose live probe, when one ran, answered
func (c *FallbackEntryCheck) Ready() bool {
	return c.Configured && c.AuthReady && (c.InferenceReady == nil || *c.InferenceReady)
}

// Status is ready after a live answer, ready (static) without a probe, else the error class every failed path sets
func (c *FallbackEntryCheck) Status() string {
	if !c.Ready() {
		return c.ErrorClass
	}
	if c.InferenceReady != nil && *c.InferenceReady {
		return "ready"
	}
	return "ready (static)"
}

// FallbackCheckRedactor scrubs one support text, keeping at most maxChars characters unless maxChars is negative
type FallbackCheckRedactor func(value any, maxChars int) string

// Text with the known credential values removed before the support redactor, whose patterns miss keys without a known prefix
func fallbackRedacted(redact FallbackCheckRedactor, text string, secrets []string, maxChars int) string {
	for _, secret := range secrets {
		text = strings.ReplaceAll(text, secret, "***")
	}
	return redact(text, maxChars)
}

// Candidates long enough to be a credential worth removing from a text
func fallbackSecretValues(candidates ...string) []string {
	var secrets []string
	for _, candidate := range candidates {
		if utf8.RuneCountInString(candidate) >= 8 {
			secrets = append(secrets, candidate)
		}
	}
	return secrets
}

// Reports whether a response model field can prove which model answered, true only for the SDK client off local, Azure, ARN and router meta routes
func fallbackReportedModelIsEvidence(ctx context.Context, provider, model string, client AuxClient) (bool, error) {
	baseURL := client.BaseURL()
	hostname, err := urlutil.BaseHost(baseURL)
	if err != nil {
		return false, err
	}
	host := "." + hostname + "."
	lowered := strings.ToLower(strings.TrimSpace(model))
	name := provider
	if profile := getProviderProfile(ctx, provider); profile != nil {
		name = profile.Name
	}
	isAzure := name == "azure-foundry"
	for _, label := range fallbackAzureHostLabels {
		isAzure = isAzure || strings.Contains(host, label)
	}
	_, isSDK := client.(*auxOpenAIClient)
	return isSDK && !IsLocalEndpoint(baseURL) && !isAzure && !strings.HasPrefix(lowered, "arn:") &&
		!strings.HasPrefix(lowered, fallbackRouterAutoModel) && !strings.Contains(lowered, fallbackRouterPresetMarker), nil
}

// Lowercased model id with a Vertex publisher path reduced to publisher and model
func fallbackCanonicalModelID(modelID string) string {
	return fallbackVertexPublisherPathRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(modelID)), "${1}/")
}

// AnsweredByRequestedModel reports a provider reported model that is the requested one or a dated snapshot of it, an empty report matching
func AnsweredByRequestedModel(requested, returned string) bool {
	if returned == "" {
		return true
	}
	want, wantTag, _ := strings.Cut(fallbackCanonicalModelID(requested), ":")
	got, gotTag, _ := strings.Cut(fallbackCanonicalModelID(returned), ":")
	if gotTag != "" && gotTag != wantTag {
		return false
	}
	if !strings.Contains(want, "/") || !strings.Contains(got, "/") {
		want, got = want[strings.LastIndex(want, "/")+1:], got[strings.LastIndex(got, "/")+1:]
	}
	suffix, ok := strings.CutPrefix(got, want)
	return ok && fallbackSnapshotSuffixRe.MatchString(suffix)
}

// Client with SDK retries off so one probe is one request, the Codex Responses wrapper rebuilt around its retry free client
func fallbackWithoutSDKRetries(client AuxClient) AuxClient {
	switch c := client.(type) {
	case *auxOpenAIClient:
		return c.withMaxRetries(0)
	case *CodexAuxiliaryClient:
		if sdk, ok := c.realClient.(*auxOpenAIClient); ok {
			return NewCodexAuxiliaryClient(sdk.withMaxRetries(0), c.adapter.model)
		}
	}
	return client
}

// Runtime of one fallback entry resolved from its provider pin, model, base url and inline or env backed key
func resolveFallbackEntryRuntime(ctx context.Context, entry map[string]any) (*Runtime, error) {
	return ResolveRuntimeProvider(ctx, home.Dir(ctx), strings.TrimSpace(auxStrOr(entry["provider"])), strings.TrimSpace(auxStrOr(entry["model"])),
		auxStrOr(entry["base_url"]), ResolveFallbackEntryAPIKey(ctx, "", entry))
}

// CheckFallbackEntryStatic resolves an entry the way the fallback switch does without sending an inference request, an error only for a host abort or an unreadable config
func CheckFallbackEntryStatic(ctx context.Context, entry map[string]any, redact FallbackCheckRedactor) (*FallbackEntryCheck, error) {
	result := &FallbackEntryCheck{Provider: strings.ToLower(strings.TrimSpace(auxStrOr(entry["provider"]))), Model: auxStrOr(entry["model"])}
	var runtime *Runtime
	var err error
	if raised := auxRecoverRaise(func() { runtime, err = resolveFallbackEntryRuntime(ctx, entry) }); raised != nil {
		err = raised
	}
	if err != nil {
		if auxIsBaseException(err) {
			return nil, err
		}
		var authErr *AuthError
		if errors.As(err, &authErr) {
			result.Configured = authErr.Code != "invalid_provider"
			result.ErrorClass = "config"
			if result.Configured {
				result.ErrorClass = "auth"
			}
			result.Error = fallbackRedacted(redact, err.Error(), nil, -1)
			return result, nil
		}
		pkgLog().Debug(fmt.Sprintf("fallback check: %s/%s did not resolve", result.Provider, result.Model), "err", err)
		result.ErrorClass = "config"
		if result.Error = fallbackRedacted(redact, err.Error(), nil, -1); result.Error == "" {
			result.Error = auxPyExceptionName(err)
		}
		return result, nil
	}
	result.Configured = true
	result.BaseURL = fallbackRedacted(redact, runtime.BaseURL, nil, -1)
	baseURLHint := strings.TrimSpace(auxStrOr(entry["base_url"]))
	explicit, hinted, err := FallbackAPIModeHint(ctx, entry, result.Provider, &baseURLHint)
	if err != nil {
		return nil, err
	}
	result.APIMode = hinted
	if !explicit && hinted == string(ModeChatCompletions) && runtime.APIMode != "" {
		result.APIMode = string(runtime.APIMode)
	}
	result.AuthSource = runtime.Source
	result.AuthReady = RuntimeHasCredentials(runtime)
	if !result.AuthReady {
		result.ErrorClass = "auth"
		result.Error = "No usable credentials found for " + result.Provider + "." + missingCredentialsSuffix(ctx, result.Provider)
	}
	return result, nil
}

// CheckFallbackEntryLive sends one minimal completion for a statically ready entry on the client the swap builds and records the verdict, an error only for a host abort
func CheckFallbackEntryLive(ctx context.Context, entry map[string]any, result *FallbackEntryCheck, redact FallbackCheckRedactor) error {
	provider := result.Provider
	secrets := fallbackSecretValues(ResolveFallbackEntryAPIKey(ctx, "", entry))
	var started time.Time
	probe := func() error {
		route, err := ResolveRouteEntryClient(ctx, ResolveProviderClient, entry, provider, result.Model, false)
		if err != nil {
			return err
		}
		client, model := route.Client, route.Model
		if client == nil {
			result.InferenceReady, result.ErrorClass = new(false), "auth"
			result.Error = "the provider router built no client for this entry (no usable credential)"
			return nil
		}
		client = fallbackWithoutSDKRetries(client)
		secrets = append(secrets, fallbackSecretValues(client.APIKey())...)
		baseURL := client.BaseURL()
		timeout, configured := GetProviderRequestTimeout(ctx, provider, model)
		if !configured {
			timeout = fallbackLiveTimeout
		}
		kwargs, err := auxBuildCallKwargs(ctx, provider, model, []any{map[string]any{"role": "user", "content": fallbackLivePrompt}},
			auxCallKwargsOptions{MaxTokens: fallbackLiveMaxTokens, Timeout: timeout, BaseURL: baseURL})
		if err != nil {
			return err
		}
		started = time.Now()
		raw, err := auxCreateWithProgressOnce(ctx, client, kwargs, "", auxProviderRequiresStream(ctx, baseURL))
		if err != nil {
			return err
		}
		result.LatencyMs = new(auxElapsedMs(started, time.Now()))
		return fallbackJudgeAnswer(ctx, result, raw, model, client, secrets, redact)
	}
	var err error
	if raised := auxRecoverRaise(func() { err = probe() }); raised != nil {
		err = raised
	}
	if err == nil {
		return nil
	}
	if auxIsBaseException(err) {
		return err
	}
	if !started.IsZero() && result.LatencyMs == nil {
		result.LatencyMs = new(auxElapsedMs(started, time.Now()))
	}
	verdict := ClassifyError(auxClassifyInput(err, provider, result.Model))
	result.InferenceReady, result.ErrorClass = new(false), "live_"+string(verdict.Reason)
	if result.Error = fallbackRedacted(redact, err.Error(), secrets, 300); result.Error == "" {
		result.Error = auxPyExceptionName(err)
	}
	pkgLog().Debug(fmt.Sprintf("fallback check: live probe of %s/%s failed: %s", result.Provider, result.Model, result.Error))
	return nil
}

// Provider error and failed status of an answered body, read from the body, a gateway data envelope and the first choice before any unwrapping drops them
func fallbackBodyFailure(raw any) (map[string]any, bool) {
	layers := []any{raw}
	if data, ok := auxField(raw, "data", nil).(map[string]any); ok {
		layers = append(layers, data)
	}
	if choices, ok := auxField(raw, "choices", nil).([]any); ok && len(choices) > 0 {
		layers = append(layers, choices[0])
	}
	var embedded map[string]any
	failed := false
	for _, layer := range layers {
		if candidate := auxEmbeddedProviderError(layer); embedded == nil && fallbackAnyTruthy(candidate) {
			embedded = candidate
		}
		failed = failed || fallbackFailedStatuses[strings.ToLower(auxStrOr(auxField(layer, "status", nil)))]
	}
	return embedded, failed
}

// Reports an error object with at least one non empty field, one whose fields are all empty is no failure
func fallbackAnyTruthy(errObj map[string]any) bool {
	for _, value := range errObj {
		if anyutil.IsTrue(value) {
			return true
		}
	}
	return false
}

// Reports a Responses style output no text can be read from, a message whose content is a bare number or true or a part whose type is a list or an object
func fallbackOutputMalformed(response any) bool {
	if text, ok := auxField(response, "output_text", nil).(string); ok && strings.TrimSpace(text) != "" {
		return false
	}
	output, _ := auxField(response, "output", nil).([]any)
	for _, item := range output {
		if itemType := auxField(item, "type", nil); anyutil.IsTrue(itemType) && itemType != "message" {
			continue
		}
		switch content := auxField(item, "content", nil).(type) {
		case []any:
			for _, part := range content {
				switch auxField(part, "type", nil).(type) {
				case []any, map[string]any:
					return true
				}
			}
		case nil, string, map[string]any:
		default:
			if anyutil.IsTrue(content) {
				return true
			}
		}
	}
	return false
}

// Verdict of an answered request, a provider error in the body, no usable completion, another model, or ready
func fallbackJudgeAnswer(ctx context.Context, result *FallbackEntryCheck, raw any, model string, client AuxClient, secrets []string, redact FallbackCheckRedactor) error {
	provider := result.Provider
	embedded, failed := fallbackBodyFailure(raw)
	var response any
	if embedded == nil && !failed {
		unwrapped, err := auxUnwrapDataEnvelope(raw, "")
		if err != nil {
			return err
		}
		response = unwrapped
		if !anyutil.IsTrue(auxField(response, "choices", nil)) {
			if fallbackOutputMalformed(response) {
				response = nil
			} else if recovered := auxRecoverAuxResponseMessage(response); recovered != nil {
				response = recovered
			}
		}
	}
	evidence, err := fallbackReportedModelIsEvidence(ctx, provider, model, client)
	if err != nil {
		return err
	}
	returned := ""
	if evidence {
		returned = auxStrOr(auxField(response, "model", nil))
	}
	result.ModelReturned = fallbackRedacted(redact, returned, secrets, -1)
	choices, _ := auxField(response, "choices", nil).([]any)
	switch {
	case embedded != nil:
		var shown any = embedded
		if message := embedded["message"]; anyutil.IsTrue(message) {
			shown = message
		}
		relayed := &AuxRuntimeError{Message: anyutil.String(shown), ProviderError: embedded, Fault: AuxReplyMalformed}
		result.InferenceReady = new(false)
		result.ErrorClass = "live_" + auxErrorClass(relayed, provider, result.Model)
		result.Error = fallbackRedacted(redact, relayed.Error(), secrets, 300)
	case failed || len(choices) == 0 || auxField(choices[0], "message", nil) == nil ||
		strings.ToLower(auxStrOr(auxField(choices[0], "finish_reason", nil))) == "error" ||
		auxIsRouterTimeoutShimResponse(response) || ExtractContentOrReasoning(response, -1) == "":
		result.InferenceReady, result.ErrorClass = new(false), "live_response"
		result.Error = "the provider answered without a usable completion"
	case !AnsweredByRequestedModel(model, returned):
		result.InferenceReady, result.ErrorClass = new(false), "model_mismatch"
		result.Error = fallbackRedacted(redact, "requested "+anyutil.Quote(model)+" but "+anyutil.Quote(returned)+" answered", secrets, -1)
	default:
		result.InferenceReady = new(true)
	}
	return nil
}

// RouteEntryClient is what the fallback swap builds for an entry, a nil Client when the provider has no usable one and Model normalized for the provider
type RouteEntryClient struct {
	Client          AuxClient
	Model           string
	APIModeExplicit bool
	APIMode         string
}

// ProviderClientResolver builds the client and model of a provider, the router a route entry resolves through
type ProviderClientResolver func(ctx context.Context, provider, model string, opts ProviderClientOptions) (AuxClient, string, error)

// ResolveRouteEntryClient builds the client the fallback swap binds for an entry, shared with the live check, the chat completions adapter of a Responses provider when rawCodex is off
func ResolveRouteEntryClient(ctx context.Context, resolve ProviderClientResolver, entry map[string]any, provider, model string, rawCodex bool) (RouteEntryClient, error) {
	baseURLHint := strings.TrimSpace(auxStrOr(entry["base_url"]))
	apiKeyHint := ResolveFallbackEntryAPIKey(ctx, "", entry)
	explicit, apiMode, err := FallbackAPIModeHint(ctx, entry, provider, &baseURLHint)
	if err != nil {
		return RouteEntryClient{}, err
	}
	if baseURLHint != "" {
		ollamaCloud, err := urlutil.BaseHostMatches(baseURLHint, "ollama.com")
		if err != nil {
			return RouteEntryClient{}, err
		}
		if ollamaCloud && apiKeyHint == "" {
			apiKeyHint = GetSecretStr(ctx, "OLLAMA_API_KEY", "")
		}
	}
	var keyHint any
	if apiKeyHint != "" {
		keyHint = apiKeyHint
	}
	client, _, err := resolve(ctx, provider, model, ProviderClientOptions{RawCodex: rawCodex, ExplicitBaseURL: baseURLHint, ExplicitAPIKey: keyHint, APIMode: apiMode})
	if err != nil {
		return RouteEntryClient{}, err
	}
	if client != nil && provider != "moa" {
		raw := model
		if normErr := auxRecoverRaise(func() { model = NormalizeModelForProviderCtx(ctx, raw, provider) }); normErr != nil {
			pkgLog().Warn(fmt.Sprintf("Could not normalize fallback model %s for provider %s: %v", anyutil.Quote(raw), anyutil.Quote(provider), normErr))
		}
	}
	return RouteEntryClient{Client: client, Model: model, APIModeExplicit: explicit, APIMode: apiMode}, nil
}
