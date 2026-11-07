package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/plugins"
)

// Imports one home plugin dir in the plugin host and returns every profile it registers
func loadExternalProviderPlugin(ctx context.Context, dir, source, homeKey string) ([]Profile, error) {
	hosted, err := plugins.LoadProviderPluginProfiles(ctx, dir, userModuleName(dir, homeKey))
	return profilesFromHosted(ctx, hosted), err
}

// Every installed icebath_agent.plugins entry point with loaders that run its target in the plugin host
func installedProviderEntryPoints(ctx context.Context) ([]ProviderEntryPoint, error) {
	var out []ProviderEntryPoint
	for _, ep := range plugins.ProviderEntryPoints() {
		out = append(out, ProviderEntryPoint{Name: ep.Name, Value: ep.Value, Load: func(ctx context.Context) (ProviderEntryPointTarget, error) {
			loaded, err := plugins.LoadProviderEntryPoint(ctx, ep)
			return ProviderEntryPointTarget{Profiles: profilesFromHosted(ctx, loaded.Profiles), Callable: loaded.Callable, RequiresArguments: loaded.RequiresArguments,
				Invoke: func(ctx context.Context) ([]Profile, error) {
					hosted, err := plugins.InvokeProviderEntryPoint(ctx, ep)
					return profilesFromHosted(ctx, hosted), err
				}}, err
		}})
	}
	return out, nil
}

// Runs one hosted profile method or callable field in the plugin host, replaced in tests
var hostedProfileCallFn = func(ctx context.Context, h *plugins.HostedProfile, attr string, args []any, kwargs map[string]any) (any, error) {
	return h.Call(ctx, attr, args, kwargs)
}

// Builds a create_client profile's live client in the plugin host, replaced in tests
var hostedProfileCreateClientFn = func(ctx context.Context, h *plugins.HostedProfile, clientKwargs map[string]any) (hostedClientObject, error) {
	obj, err := h.CreateClient(ctx, nil, clientKwargs)
	if err != nil {
		return nil, err
	}
	return obj, nil
}

// Profiles whose data comes from the host extraction and whose overridden methods run in the plugin host
func profilesFromHosted(ctx context.Context, hosted []*plugins.HostedProfile) []Profile {
	if ctx == nil {
		ctx = context.Background()
	}
	out := make([]Profile, 0, len(hosted))
	for _, h := range hosted {
		out = append(out, profileFromHosted(context.WithoutCancel(ctx), h))
	}
	return out
}

// One hosted profile with the dataclass defaults, its extracted fields and a host hook per overridden call
func profileFromHosted(ctx context.Context, h *plugins.HostedProfile) Profile {
	p := Profile{Name: h.Name, Mode: ModeChatCompletions, AuthType: "api_key", HealthCheck: true, ModelListing: true, VisionToolMessages: true}
	names := make([]string, 0, len(h.Fields))
	for name := range h.Fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !applyHostedProfileField(&p, name, h.Fields[name]) {
			if p.Extra == nil {
				p.Extra = map[string]any{}
			}
			p.Extra[name] = h.Fields[name]
		}
	}
	p.Auth = AuthKindOf(p.AuthType)
	if p.ModelsDevID == "" {
		p.ModelsDevID = p.Name
	}
	if p.ModelsURL == "" && p.BaseURL != "" && p.ModelListing {
		p.ModelsURL = strings.TrimRight(p.BaseURL, "/") + "/models"
	}
	hc := hostedCalls{ctx: ctx, h: h}
	attrs := make([]string, 0, len(h.Calls))
	for attr := range h.Calls {
		attrs = append(attrs, attr)
	}
	sort.Strings(attrs)
	for _, attr := range attrs {
		hc.install(&p, attr)
	}
	if h.InProcess != "" {
		p.CreateClientHook = func(ctx context.Context, clientKwargs map[string]any) (Client, error) {
			obj, err := hostedProfileCreateClientFn(ctx, h, clientKwargs)
			if err != nil {
				return nil, err
			}
			return newHostedSDKClient(p, clientKwargs, obj), nil
		}
	}
	return p
}

// Sets the Go field of one ProviderProfile attribute, false for an attribute the struct has no field for
func applyHostedProfileField(p *Profile, name string, v any) bool {
	switch name {
	case "name":
		p.Name = hostedStr(v)
	case "api_mode":
		p.Mode = APIMode(hostedStr(v))
	case "aliases":
		p.Aliases = hostedStrs(v)
	case "display_name":
		p.DisplayName = hostedStr(v)
	case "description":
		p.Description = hostedStr(v)
	case "signup_url":
		p.SignupURL = hostedStr(v)
	case "hidden":
		p.Hidden = pyTruthy(v)
	case "env_vars":
		p.KeyEnv = hostedStrs(v)
	case "base_url":
		p.BaseURL = hostedStr(v)
	case "models_url":
		p.ModelsURL = hostedStr(v)
	case "auth_type":
		p.AuthType = hostedStr(v)
	case "supports_streaming":
		if v != nil {
			b := pyTruthy(v)
			p.SupportsStreaming = &b
		}
	case "supports_health_check":
		p.HealthCheck = pyTruthy(v)
	case "supports_model_listing":
		p.ModelListing = pyTruthy(v)
	case "supports_vision":
		p.Vision = pyTruthy(v)
	case "supports_vision_tool_messages":
		p.VisionToolMessages = pyTruthy(v)
	case "supports_prompt_cache_key":
		p.SupportsPromptCacheKey = pyTruthy(v)
	case "native_reasoning_details_type":
		p.NativeReasoningDetailsType = hostedStr(v)
	case "process_command":
		p.ProcessCommand = hostedStr(v)
	case "process_args":
		p.ProcessArgs = hostedStrs(v)
	case "process_command_env_vars":
		p.ProcessCommandEnvVars = hostedStrs(v)
	case "process_args_env_var":
		p.ProcessArgsEnvVar = hostedStr(v)
	case "fallback_models":
		p.FallbackModels = hostedStrs(v)
	case "model_aliases":
		p.ModelAliases = hostedStrMap(v)
	case "hostname":
		p.Hostname = hostedStr(v)
	case "default_headers":
		p.Headers = hostedStrMap(v)
	case "fixed_temperature":
		if f, ok := hostedFloat(v); ok {
			p.FixedTemperature = &f
		} else if v != nil {
			p.OmitTemperature = true
		}
	case "default_max_tokens":
		p.DefaultMaxTokens = hostedInt(v)
	case "unsupported_response_formats":
		p.UnsupportedResponseFormats = hostedStrs(v)
	case "default_aux_model":
		p.DefaultAuxModel = hostedStr(v)
	case "model_capabilities":
		caps := map[string]map[string]any{}
		for model, entry := range hostedMap(v) {
			caps[model] = hostedMap(entry)
		}
		p.ModelCapabilities = caps
	case "account_card":
		p.AccountCard = hostedStrMap(v)
	case "hostnames":
		p.Hostnames = hostedStrs(v)
	case "metadata_hostnames":
		p.MetadataHostnames = hostedStrs(v)
	case "models_dev_id":
		p.ModelsDevID = hostedStr(v)
	case "ambient_credentials":
		p.AmbientCredentials = pyTruthy(v)
	case "responses_replay_item_ids":
		p.ResponsesDropReplayItemIDs = !pyTruthy(v)
	case "responses_prompt_cache_key":
		p.ResponsesOmitPromptCacheKey = !pyTruthy(v)
	case "responses_issuer_kind":
		p.ResponsesIssuer = hostedStr(v)
	case "responses_trusts_completed_status":
		p.ResponsesDistrustCompletedStatus = !pyTruthy(v)
	case "responses_native_compaction":
		p.ResponsesNoNativeCompaction = !pyTruthy(v)
	case "responses_profile_reasoning":
		p.ResponsesReasoningFromProfile = pyTruthy(v)
	case "message_carrier_opaque":
		p.MessageCarrierOpaque = hostedStr(v)
	case "message_carrier_text":
		p.MessageCarrierText = hostedStr(v)
	case "replays_tool_call_signatures":
		p.ReplaysToolCallSignatures = pyTruthy(v)
	case "static_models":
		p.StaticModels = hostedStrs(v)
	case "catalog_credential_files":
		p.CatalogCredentialFiles = hostedStrs(v)
	case "picker_after":
		p.PickerAfter = hostedStr(v)
	case "picker_group":
		p.PickerGroup = hostedStrs(v)
	case "catalog_aliases":
		p.CatalogAliases = pyTruthy(v)
	case "aggregator":
		p.Aggregator = pyTruthy(v)
	case "provider_overlay":
		p.Overlay = hostedOverlay(hostedMap(v))
	case "overlay_aliases":
		p.OverlayAliases = hostedStrs(v)
	case "registry_auth_type":
		p.RegistryAuthType = hostedStr(v)
	case "base_url_env_var":
		p.BaseURLEnv = hostedStr(v)
	case "registry_after":
		p.RegistryAfter = hostedStr(v)
	default:
		return false
	}
	return true
}

// Overlay of a provider_overlay dict, nil when the dict is empty like the dataclass default
func hostedOverlay(m map[string]any) *IcebathOverlay {
	if len(m) == 0 {
		return nil
	}
	return &IcebathOverlay{Transport: hostedStr(m["transport"]), IsAggregator: pyTruthy(m["is_aggregator"]), AuthType: hostedStr(m["auth_type"]),
		ExtraEnvVars: hostedStrs(m["extra_env_vars"]), BaseURLOverride: hostedStr(m["base_url_override"]), BaseURLEnvVar: hostedStr(m["base_url_env_var"])}
}

// Host call context of one hosted profile, the load context carrying the plugin manager for hooks without one
type hostedCalls struct {
	ctx context.Context
	h   *plugins.HostedProfile
}

// Runs the attribute in the host, raising its exception as a panic for hooks that return no error
func (hc hostedCalls) must(ctx context.Context, attr string, args []any, kwargs map[string]any) any {
	out, err := hc.call(ctx, attr, args, kwargs)
	if err != nil {
		panic(err)
	}
	return out
}

func (hc hostedCalls) call(ctx context.Context, attr string, args []any, kwargs map[string]any) (any, error) {
	if ctx == nil {
		ctx = hc.ctx
	}
	return hostedProfileCallFn(ctx, hc.h, attr, args, kwargs)
}

// Installs the Go hook of one overridden ProviderProfile method or callable field
func (hc hostedCalls) install(p *Profile, attr string) {
	bg := hc.ctx
	switch attr {
	case "auth_handler":
		p.AuthHandler = func(action string, args []string, provider string) (bool, error) {
			out, err := hc.call(bg, attr, []any{action, hostedAuthNamespace{Provider: provider, Argv: args}}, nil)
			return pyTruthy(out), err
		}
	case "refresh_credential":
		p.RefreshCredential = func(entry PooledCredential) (map[string]any, error) {
			out, err := hc.call(bg, attr, []any{entry}, nil)
			return hostedMapOrNil(out), err
		}
	case "classify_api_error":
		p.ClassifyAPIError = func(in ClassifyInput, status int, errorType, errorCode, message string, body map[string]any) *Verdict {
			kwargs := map[string]any{"status_code": hostedOptInt(status), "error_code": hostedOptStr(errorCode), "message": message, "body": body, "model": in.Model}
			return hostedVerdict(hc.must(bg, attr, []any{in.Err}, kwargs))
		}
	case "account_status":
		p.AccountStatus = func(ctx context.Context) (any, error) { return hc.call(ctx, attr, nil, nil) }
	case "start_device_login":
		p.StartDeviceLogin = func(ctx context.Context) (any, error) { return hc.call(ctx, attr, nil, nil) }
	case "poll_device_login":
		p.PollDeviceLogin = func(ctx context.Context, state any) (any, error) { return hc.call(ctx, attr, []any{state}, nil) }
	case "save_device_login":
		p.SaveDeviceLogin = func(ctx context.Context, result any) error {
			_, err := hc.call(ctx, attr, []any{result}, nil)
			return err
		}
	case "model_setup_flow":
		p.ModelSetupFlow = func(ctx context.Context, cfg any, currentModel string, args any) (any, error) {
			return hc.call(ctx, attr, []any{cfg, currentModel, args}, nil)
		}
	case "setup_reasoning_efforts":
		p.SetupReasoningEfforts = func(model string) any { return hc.must(bg, attr, []any{model}, nil) }
	case "picker_catalog":
		p.PickerCatalog = func(ctx context.Context, forceRefresh bool) ([]string, bool) {
			out := hc.must(ctx, attr, []any{forceRefresh}, nil)
			return hostedStrs(out), out != nil
		}
	case "fetch_account_usage":
		p.FetchAccountUsageHook = func(ctx context.Context, baseURL, apiKey string) (*AccountUsageSnapshot, error) {
			out, err := hc.call(ctx, attr, nil, map[string]any{"base_url": hostedOptStr(baseURL), "api_key": hostedOptStr(apiKey)})
			if err != nil || out == nil {
				return nil, err
			}
			return hostedUsageSnapshot(hostedMap(out)), nil
		}
	case "resolve_aux_model":
		p.ResolveAuxModelHook = func(ctx context.Context, vision bool) string {
			return hostedStr(hc.must(ctx, attr, nil, map[string]any{"vision": vision}))
		}
	case "get_hostname":
		p.GetHostnameHook = func() string { return hostedStr(hc.must(bg, attr, nil, nil)) }
	case "prepare_messages":
		p.PrepareMessagesHook = func(messages []map[string]any) []map[string]any {
			return hostedDictList(hc.must(bg, attr, []any{messages}, nil))
		}
	case "build_extra_body":
		p.BuildExtraBodyHook = func(sessionID string, context map[string]any) map[string]any {
			kwargs := hostedKwargs(context)
			kwargs["session_id"] = hostedOptStr(sessionID)
			return hostedMap(hc.must(bg, attr, nil, kwargs))
		}
	case "default_reasoning_config":
		p.DefaultReasoningConfigHook = func(model string) *Reasoning {
			return reasoningPtrOfDict(hostedMapOrNil(hc.must(bg, attr, []any{hostedOptStr(model)}, nil)))
		}
	case "build_api_kwargs_extras":
		p.BuildAPIKwargsExtrasHook = func(reasoning *Reasoning, context map[string]any) (map[string]any, map[string]any) {
			kwargs := hostedKwargs(context)
			kwargs["reasoning_config"] = hostedReasoningArg(reasoning)
			pair := hostedList(hc.must(bg, attr, nil, kwargs))
			if len(pair) != 2 {
				panic(fmt.Errorf("%s.build_api_kwargs_extras returned %d values, expected 2", hc.h.Name, len(pair)))
			}
			return hostedMap(pair[0]), hostedMap(pair[1])
		}
	case "build_client_kwargs_extras":
		p.BuildClientKwargsExtrasHook = func(context map[string]any) map[string]any {
			return hostedMap(hc.must(bg, attr, nil, hostedKwargs(context)))
		}
	case "default_vision_model":
		p.DefaultVisionModelHook = func(ctx context.Context) string { return hostedStr(hc.must(ctx, attr, nil, nil)) }
	case "get_model_context_length":
		p.GetModelContextLengthHook = func(model string) int { return hostedInt(hc.must(bg, attr, []any{model}, nil)) }
	case "get_usage_cost":
		p.GetUsageCostHook = func(model string, usage CanonicalUsage) (CostResult, bool) {
			out := hc.must(bg, attr, []any{model, usage}, nil)
			if out == nil {
				return CostResult{}, false
			}
			return hostedCostResult(hostedMap(out)), true
		}
	case "get_max_tokens":
		p.GetMaxTokensHook = func(model string) int { return hostedInt(hc.must(bg, attr, []any{hostedOptStr(model)}, nil)) }
	case "supported_reasoning_efforts":
		p.SupportedReasoningEffortsHook = func(ctx context.Context, model string) ([]string, bool, error) {
			out, err := hc.call(ctx, attr, []any{hostedOptStr(model)}, nil)
			if err != nil || out == nil {
				return nil, false, err
			}
			return hostedStrs(out), true, nil
		}
	case "api_mode_for_model":
		p.APIModeForModelHook = func(model, apiKey string) APIMode {
			return APIMode(hostedStr(hc.must(bg, attr, []any{model}, map[string]any{"api_key": hostedOptStr(apiKey)})))
		}
	case "client_headers":
		p.ClientHeadersHook = func(baseURL, apiKey string, hasImages bool) map[string]string {
			out := hc.must(bg, attr, nil, map[string]any{"base_url": baseURL, "api_key": apiKey, "has_images": hasImages})
			if out == nil {
				return nil
			}
			return hostedStrMap(out)
		}
	case "request_headers":
		p.RequestHeadersHook = func(baseURL, model string, userInitiated bool) map[string]string {
			return hostedStrMap(hc.must(bg, attr, nil, map[string]any{"base_url": baseURL, "model": model, "user_initiated": userInitiated}))
		}
	case "max_tokens_param":
		p.MaxTokensParamHook = func(baseURL, model string) string {
			return hostedStr(hc.must(bg, attr, nil, map[string]any{"base_url": baseURL, "model": model}))
		}
	case "listed":
		p.ListedHook = func(ctx context.Context) (bool, error) {
			out, err := hc.call(ctx, attr, nil, nil)
			return pyTruthy(out), err
		}
	case "setup_status":
		p.SetupStatusHook = func(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
			out, err := hc.call(ctx, attr, nil, hostedKwargs(kwargs))
			return hostedMapOrNil(out), err
		}
	case "discover_models":
		p.DiscoverModelsHook = func(ctx context.Context, kwargs map[string]any) ([]map[string]any, error) {
			out, err := hc.call(ctx, attr, nil, hostedKwargs(kwargs))
			if err != nil || out == nil {
				return nil, err
			}
			return hostedDictList(out), nil
		}
	case "fetch_models":
		p.FetchModelsHook = func(ctx context.Context, apiKey, baseURL string, timeout time.Duration) ([]string, bool) {
			out := hc.must(ctx, attr, nil, map[string]any{"api_key": hostedOptStr(apiKey), "base_url": hostedOptStr(baseURL), "timeout": timeout.Seconds()})
			return hostedStrs(out), out != nil
		}
	case "live_context_length":
		p.LiveContextLengthHook = func(ctx context.Context, model, apiKey, baseURL string) int {
			return hostedInt(hc.must(ctx, attr, []any{model}, map[string]any{"api_key": apiKey, "base_url": baseURL}))
		}
	case "normalize_model_id":
		p.NormalizeModelIDHook = func(model, apiKey string) string {
			return hostedStr(hc.must(bg, attr, []any{model}, map[string]any{"api_key": apiKey}))
		}
	case "resolve_api_key_secret":
		p.ResolveAPIKeySecretHook = func() (string, string, bool) {
			pair := hostedList(hc.must(bg, attr, nil, nil))
			if len(pair) < 2 {
				return "", "", false
			}
			return hostedStr(pair[0]), hostedStr(pair[1]), true
		}
	case "runtime_base_url":
		p.RuntimeBaseURLHook = func(apiKey, defaultURL, envURL string) string {
			return hostedStr(hc.must(bg, attr, nil, map[string]any{"api_key": apiKey, "default": defaultURL, "env_url": envURL}))
		}
	case "prepare_pool_entry":
		p.PreparePoolEntryHook = func(entry *PooledCredential, apiKey string) string {
			return hostedStr(hc.must(bg, attr, []any{entry, apiKey}, nil))
		}
	case "seed_pool_credentials":
		p.SeedPoolCredentialsHook = func(suppressedSources map[string]bool, explicitlyConfigured bool) []map[string]any {
			sources := []string{}
			for s, on := range suppressedSources {
				if on {
					sources = append(sources, s)
				}
			}
			sort.Strings(sources)
			out := hc.must(bg, attr, nil, map[string]any{"suppressed_sources": sources, "explicitly_configured": explicitlyConfigured})
			if out == nil {
				return nil
			}
			return hostedDictList(out)
		}
	case "remove_credential_source":
		p.RemoveCredentialSourceHook = func(source string) ([]string, []string, bool) {
			out := hc.must(bg, attr, []any{source}, nil)
			pair := hostedList(out)
			if out == nil || len(pair) < 2 {
				return nil, nil, false
			}
			return hostedStrs(pair[0]), hostedStrs(pair[1]), true
		}
	case "refresh_runtime_credentials":
		p.RefreshRuntimeCredentialsHook = func(ctx context.Context, reason string) (string, string, bool, error) {
			out, err := hc.call(ctx, attr, nil, map[string]any{"reason": reason})
			pair := hostedList(out)
			if err != nil || out == nil || len(pair) < 2 {
				return "", "", false, err
			}
			return hostedStr(pair[0]), hostedStr(pair[1]), true, nil
		}
	case "is_stale_credential_error":
		p.IsStaleCredentialErrorHook = func(statusCode int, message string) bool {
			return pyTruthy(hc.must(bg, attr, nil, map[string]any{"status_code": hostedOptInt(statusCode), "message": message}))
		}
	case "credential_refresh_notice":
		p.CredentialRefreshNoticeHook = func(reason string) string {
			return hostedStr(hc.must(bg, attr, nil, map[string]any{"reason": reason}))
		}
	case "request_rejection_hint":
		p.RequestRejectionHintHook = func(statusCode int, baseURL string) []string {
			return hostedStrs(hc.must(bg, attr, nil, map[string]any{"status_code": hostedOptInt(statusCode), "base_url": baseURL}))
		}
	case "external_auth_evidence":
		p.ExternalAuthEvidenceHook = func() (bool, string, bool) {
			out := hc.must(bg, attr, nil, nil)
			pair := hostedList(out)
			if out == nil || len(pair) < 2 {
				return false, "", false
			}
			return pyTruthy(pair[0]), hostedStr(pair[1]), true
		}
	case "missing_credentials_hint":
		p.MissingCredentialsHintHook = func() any { return hc.must(bg, attr, nil, nil) }
	case "reasoning_payload":
		p.ReasoningPayloadHook = func(model string, r Reasoning, baseURL string) map[string]any {
			return hostedMapOrNil(hc.must(bg, attr, []any{hostedOptStr(model), reasoningConfigMap(&r)}, map[string]any{"base_url": baseURL}))
		}
	case "replay_message_carriers":
		p.ReplayMessageCarriersHook = func(values map[string]any, model, baseURL string) map[string]any {
			return hostedMap(hc.must(bg, attr, []any{values}, map[string]any{"model": hostedOptStr(model), "base_url": baseURL}))
		}
	case "replays_function_thought_signature":
		p.ReplaysFunctionThoughtSignatureHook = func(model, baseURL string) bool {
			return pyTruthy(hc.must(bg, attr, nil, map[string]any{"model": hostedOptStr(model), "base_url": baseURL}))
		}
	case "probe_models":
		p.ProbeModelsHook = func(ctx context.Context, baseURL, apiKey string, timeout time.Duration) *ModelsProbe {
			out := hc.must(ctx, attr, nil, map[string]any{"base_url": baseURL, "api_key": hostedOptStr(apiKey), "timeout": timeout.Seconds()})
			triple := hostedList(out)
			if out == nil || len(triple) < 3 {
				return nil
			}
			return &ModelsProbe{IDs: hostedStrs(triple[0]), Listed: triple[0] != nil, ProbedURL: hostedStr(triple[1]), ResolvedBaseURL: hostedStr(triple[2])}
		}
	case "invalidate_catalog_cache":
		p.InvalidateCatalogCacheHook = func() { hc.must(bg, attr, nil, nil) }
	default:
		if p.Extra == nil {
			p.Extra = map[string]any{}
		}
		p.Extra[attr] = func(ctx context.Context, args []any, kwargs map[string]any) (any, error) {
			return hc.call(ctx, attr, args, kwargs)
		}
	}
}

// Parsed CLI namespace an auth handler reads, the provider it was invoked for and the raw arguments
type hostedAuthNamespace struct {
	Provider string
	Argv     []string
}

// Verdict of a classify_api_error mapping, nil when the hook declined or named no known reason
func hostedVerdict(out any) *Verdict {
	m, ok := out.(map[string]any)
	if !ok {
		if r, isRecord := out.(plugins.Record); isRecord {
			m = map[string]any(r)
		} else {
			return nil
		}
	}
	raw, ok := m["reason"].(string)
	if !ok {
		return nil
	}
	reason := FailoverReason(strings.ToLower(strings.TrimSpace(raw)))
	if !slices.Contains(failoverReasons, reason) {
		return nil
	}
	vd := &Verdict{Reason: reason}
	flags := map[string]**bool{"retryable": &vd.Retryable, "should_compress": &vd.ShouldCompress,
		"should_rotate_credential": &vd.ShouldRotateCredential, "should_fallback": &vd.ShouldFallback}
	for key, dst := range flags {
		if v, present := m[key]; present {
			*dst = bptr(pyTruthy(v))
		}
	}
	if ctxMap, ok := m["error_context"].(map[string]any); ok {
		vd.Context = ctxMap
	}
	return vd
}

// Usage snapshot of an AccountUsageSnapshot record
func hostedUsageSnapshot(m map[string]any) *AccountUsageSnapshot {
	snap := &AccountUsageSnapshot{Provider: hostedStr(m["provider"]), Source: hostedStr(m["source"]), Title: hostedStr(m["title"]),
		Plan: hostedStr(m["plan"]), Details: hostedStrs(m["details"]), UnavailableReason: hostedStr(m["unavailable_reason"]),
		Identity: hostedStr(m["identity"]), Raw: hostedMapOrNil(m["raw"]), UsageWindow: pyTruthy(m["usage_window"])}
	if t, ok := hostedDatetime(m["fetched_at"]); ok {
		snap.FetchedAt = t
	}
	for _, raw := range hostedList(m["windows"]) {
		w := hostedMap(raw)
		win := AccountUsageWindow{Label: hostedStr(w["label"]), Detail: hostedStr(w["detail"]), Scope: hostedStr(w["scope"])}
		if f, ok := hostedFloat(w["used_percent"]); ok {
			win.UsedPercent = &f
		}
		if t, ok := hostedDatetime(w["reset_at"]); ok {
			win.ResetAt = &t
		}
		snap.Windows = append(snap.Windows, win)
	}
	return snap
}

// Cost of a CostResult record, the decimal amount kept exact
func hostedCostResult(m map[string]any) CostResult {
	out := CostResult{Status: hostedStr(m["status"]), Source: hostedStr(m["source"]), Label: hostedStr(m["label"]),
		PricingVersion: hostedStr(m["pricing_version"]), Notes: hostedStrs(m["notes"])}
	switch amount := m["amount_usd"].(type) {
	case nil:
	case *plugins.Opaque:
		if mm := decimalRepr.FindStringSubmatch(amount.Text); mm != nil {
			out.AmountUSD, _ = new(big.Rat).SetString(mm[1])
		}
	default:
		out.AmountUSD, _ = new(big.Rat).SetString(pyStr(amount))
	}
	if t, ok := hostedDatetime(m["fetched_at"]); ok {
		out.FetchedAt = t
	}
	return out
}

var decimalRepr = regexp.MustCompile(`^Decimal\('([^']*)'\)$`)

var datetimeRepr = regexp.MustCompile(`^datetime\.datetime\((\d+), (\d+), (\d+)(?:, (\d+))?(?:, (\d+))?(?:, (\d+))?(?:, (\d+))?(?:, tzinfo=(.*))?\)$`)

var zoneinfoRepr = regexp.MustCompile(`ZoneInfo\(key='([^']+)'\)`)

var timedeltaRepr = regexp.MustCompile(`datetime\.timedelta\((?:days=(-?\d+))?(?:, )?(?:seconds=(\d+))?`)

// Time of a datetime crossing the host as its repr, a naive datetime read as local time
func hostedDatetime(v any) (time.Time, bool) {
	o, ok := v.(*plugins.Opaque)
	if !ok {
		if s, isStr := v.(string); isStr {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				return t, true
			}
		}
		return time.Time{}, false
	}
	m := datetimeRepr.FindStringSubmatch(o.Text)
	if m == nil {
		return time.Time{}, false
	}
	n := make([]int, 7)
	for i := range n {
		n[i], _ = strconv.Atoi(m[i+1])
	}
	loc := time.Local
	if tz := m[8]; tz != "" {
		loc = time.UTC
		if z := zoneinfoRepr.FindStringSubmatch(tz); z != nil {
			zone, err := time.LoadLocation(z[1])
			if err != nil {
				return time.Time{}, false
			}
			loc = zone
		} else if d := timedeltaRepr.FindStringSubmatch(tz); d != nil && (d[1] != "" || d[2] != "") {
			days, _ := strconv.Atoi(d[1])
			secs, _ := strconv.Atoi(d[2])
			loc = time.FixedZone("", days*86400+secs)
		}
	}
	return time.Date(n[0], time.Month(n[1]), n[2], n[3], n[4], n[5], n[6]*1000, loc), true
}

func hostedStr(v any) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	}
	return pyStr(v)
}

func hostedOptStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func hostedOptInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func hostedInt(v any) int {
	if n, ok := anyInt(v); ok {
		return n
	}
	return 0
}

func hostedFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	}
	return 0, false
}

func hostedList(v any) []any {
	l, _ := v.([]any)
	return l
}

func hostedStrs(v any) []string {
	l := hostedList(v)
	if l == nil {
		return nil
	}
	out := make([]string, 0, len(l))
	for _, item := range l {
		out = append(out, hostedStr(item))
	}
	return out
}

func hostedMapOrNil(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case plugins.Record:
		return map[string]any(m)
	}
	return nil
}

func hostedMap(v any) map[string]any {
	if m := hostedMapOrNil(v); m != nil {
		return m
	}
	return map[string]any{}
}

func hostedStrMap(v any) map[string]string {
	out := map[string]string{}
	for k, item := range hostedMap(v) {
		out[k] = hostedStr(item)
	}
	return out
}

func hostedDictList(v any) []map[string]any {
	l := hostedList(v)
	out := make([]map[string]any, 0, len(l))
	for _, item := range l {
		out = append(out, hostedMap(item))
	}
	return out
}

// Keyword arguments copied so a hook never writes into the caller's context map
func hostedKwargs(context map[string]any) map[string]any {
	out := make(map[string]any, len(context)+1)
	for k, v := range context {
		out[k] = v
	}
	return out
}

func hostedReasoningArg(r *Reasoning) any {
	if r == nil {
		return nil
	}
	return reasoningConfigMap(r)
}

// Live SDK client object of the plugin host, its attributes and method calls crossing the host boundary
type hostedClientObject interface {
	Get(ctx context.Context, name string) (any, error)
	Call(ctx context.Context, method string, args []any, kwargs map[string]any) (any, error)
}

// Client over a create_client override's live SDK client, the request built for the chat completions wire
type hostedSDKClient struct {
	obj hostedClientObject
	cc  *chatCompletionsClient
}

func newHostedSDKClient(p Profile, clientKwargs map[string]any, obj hostedClientObject) *hostedSDKClient {
	route := Route{Provider: p.Name, BaseURL: hostedStr(clientKwargs["base_url"]), APIKey: hostedStr(clientKwargs["api_key"]),
		Mode: ModeChatCompletions, Auth: AuthKindOf(p.AuthType), Headers: p.Headers}
	return &hostedSDKClient{obj: obj, cc: &chatCompletionsClient{route: route, cc: newCCRoute(route), llamacpp: streamsLlamaCpp(route)}}
}

func (c *hostedSDKClient) Route() Route { return c.cc.route }

// Closes the live client in the host when it has a close method
func (c *hostedSDKClient) Close() error {
	if o, ok := c.obj.(interface{ HasMethod(name string) bool }); ok && !o.HasMethod("close") {
		return nil
	}
	_, err := c.obj.Call(context.Background(), "close", nil, nil)
	return err
}

// Chat completion kwargs for client.chat.completions.create, body keys as kwargs and the rest under extra_body
func (c *hostedSDKClient) prepare(ctx context.Context, req *Request, stream bool) (*ccPrepared, map[string]any, error) {
	settings := loadWireSettings(ctx)
	build := c.cc.cc.build(ctx, req, settings)
	if build.err != nil {
		return nil, nil, build.err
	}
	kwargs := map[string]any{}
	for _, k := range build.body.keys {
		kwargs[k] = build.body.vals[k]
	}
	if len(build.extraBody.keys) > 0 {
		kwargs["extra_body"] = build.extraBody.Map()
	}
	if len(build.headers) > 0 {
		kwargs["extra_headers"] = build.headers
	}
	delete(kwargs, "stream")
	delete(kwargs, "stream_options")
	c.cc.mu.Lock()
	rejected := c.cc.streamOptionsRejected
	c.cc.mu.Unlock()
	if stream {
		kwargs["stream"] = true
		if c.cc.includeUsage(req, rejected) {
			kwargs["stream_options"] = map[string]any{"include_usage": true}
		}
	}
	wire, err := hostedJSONMap(kwargs)
	if err != nil {
		return nil, nil, err
	}
	return &ccPrepared{build: build, wire: wire, settings: settings, estTokens: EstimateRequestContextTokens(ctx, wire)}, kwargs, nil
}

// Runs client.chat.completions.create in the plugin host
func (c *hostedSDKClient) create(ctx context.Context, kwargs map[string]any) (any, error) {
	chat, err := c.obj.Get(ctx, "chat")
	if err != nil {
		return nil, err
	}
	chatObj, ok := chat.(hostedClientObject)
	if !ok {
		return nil, fmt.Errorf("provider: hosted client attribute chat is %T, not a live object", chat)
	}
	completions, err := chatObj.Get(ctx, "completions")
	if err != nil {
		return nil, err
	}
	compObj, ok := completions.(hostedClientObject)
	if !ok {
		return nil, fmt.Errorf("provider: hosted client attribute chat.completions is %T, not a live object", completions)
	}
	return compObj.Call(ctx, "create", nil, kwargs)
}

func (c *hostedSDKClient) Complete(ctx context.Context, req *Request) (*Response, error) {
	p, kwargs, err := c.prepare(ctx, req, false)
	if err != nil {
		return nil, err
	}
	stale := computeNonStreamStaleTimeout(ctx, p.settings, c.cc.cc.provider, req, c.cc.cc.local, false, p.estTokens, map[string]any{"extra_body": geminiPlain(p.build.extraBody)})
	return runNonStreamRequest(ctx, req.Model, p.estTokens, stale, func(ctx context.Context) (*Response, error) {
		start := time.Now()
		out, err := c.create(ctx, kwargs)
		if err != nil {
			return nil, Classify(c.cc.route.Provider, req.Model, 0, "", err)
		}
		return c.normalize(out, time.Since(start))
	})
}

// Response of a completion object the host returned as data
func (c *hostedSDKClient) normalize(out any, latency time.Duration) (*Response, error) {
	payload, err := hostedJSONMap(out)
	if err != nil {
		return nil, err
	}
	return c.cc.normalizeResponse(payload, 200, nil, latency)
}

// Streams through the live client, a final completion object switching the client to non streaming
func (c *hostedSDKClient) Stream(ctx context.Context, req *Request, fn StreamFunc) (*Response, error) {
	c.cc.mu.Lock()
	latched := c.cc.nonStreaming
	c.cc.mu.Unlock()
	if latched {
		resp, err := c.Complete(ctx, req)
		if err != nil {
			return nil, err
		}
		return resp, c.cc.replay(resp, fn)
	}
	p, kwargs, err := c.prepare(ctx, req, true)
	if err != nil {
		return nil, err
	}
	t := c.cc.streamTimeouts(req, p.settings, p.estTokens)
	s := c.cc.newStreamState(ctx, req, p, fn, t)
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	out, err := c.create(attemptCtx, kwargs)
	if err != nil {
		return nil, Classify(c.cc.route.Provider, req.Model, 0, "", err)
	}
	iter, live := out.(*plugins.HostedObject)
	if !live {
		catalogLog.Info(fmt.Sprintf("Streaming request returned a final response object instead of an iterator; switching %s/%s to non-streaming for this session.", orUnknown2(c.cc.route.Provider), orUnknown2(req.Model)))
		c.cc.mu.Lock()
		c.cc.nonStreaming = true
		c.cc.mu.Unlock()
		final, err := c.normalize(out, time.Since(s.started))
		if err != nil {
			return nil, err
		}
		return final, c.cc.replay(final, fn)
	}
	s.claimWriter(ctx)
	return c.cc.consumeStream(ctx, attemptCtx, cancel, &ccKill{}, &hostedChunkSource{ctx: attemptCtx, iter: iter}, nil, s, t)
}

func (c *hostedSDKClient) CountTokens(ctx context.Context, req *Request) (int, error) {
	p, _, err := c.prepare(ctx, req, req != nil && req.Stream)
	if err != nil {
		return 0, err
	}
	return p.estTokens, nil
}

// Frame source over the host side stream iterator, each chunk object one data frame
type hostedChunkSource struct {
	ctx  context.Context
	iter *plugins.HostedObject
}

func (s *hostedChunkSource) Next() (*sseFrame, error) {
	item, ok, err := s.iter.Next(s.ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, io.EOF
	}
	data, err := json.Marshal(hostedPlain(item))
	if err != nil {
		return nil, fmt.Errorf("provider: encode hosted stream chunk: %w", err)
	}
	return &sseFrame{Data: string(data)}, nil
}

// JSON shaped map of a host value, numbers and nesting exactly as a decoded HTTP body has them
func hostedJSONMap(v any) (map[string]any, error) {
	data, err := json.Marshal(hostedPlain(v))
	if err != nil {
		return nil, fmt.Errorf("provider: encode hosted completion: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, errors.New("provider: hosted client returned a completion that is not an object")
	}
	return out, nil
}

// Plain maps and lists of a host value, records unwrapped at every depth
func hostedPlain(v any) any {
	switch x := v.(type) {
	case plugins.Record:
		return hostedPlain(map[string]any(x))
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = hostedPlain(item)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = hostedPlain(item)
		}
		return out
	case *omap:
		return hostedPlain(x.Map())
	case *plugins.Opaque:
		return x.Text
	}
	return v
}
