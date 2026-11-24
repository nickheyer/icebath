package provider

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Copilot API host family covering the individual, business and enterprise subdomains
const copilotHost = "githubcopilot.com"

// Hosts whose first call of a user turn carries x-initiator user
var copilotInitiatorHosts = []string{"api.githubcopilot.com", "models.github.ai"}

// copilot-acp env names kept profile managed, scrubbed when inherited but absent from the profile env file
var copilotProfileEnv = []string{"ICEBATH_COPILOT_ACP_COMMAND", "ICEBATH_COPILOT_ACP_ARGS", "COPILOT_CLI_PATH", "COPILOT_ACP_BASE_URL"}

// Picker group both Copilot providers fold under
var copilotPickerGroup = []string{"copilot", "GitHub Copilot", "GitHub token API or copilot --acp process"}

// Profiles the built in github-copilot plugin registers, the Copilot API then the Copilot CLI over ACP
func copilotPluginProfiles() []Profile {
	return []Profile{copilotAPIProfile(), copilotACPProfile()}
}

// Copilot and GitHub Models over the Copilot API with editor headers and catalog gated reasoning
func copilotAPIProfile() Profile {
	p := bundledRowProfile(profileRow{name: "copilot", display: "GitHub Copilot",
		desc: "GitHub Copilot (Uses GITHUB_TOKEN or icebath auth add copilot)", base: copilotBaseURL, authType: "copilot",
		modelsDev: copilotModelsDevID, hostnames: append([]string(nil), copilotHostnames...), aggregator: true},
		"github-copilot", "github-models", "github-model", "github")
	p.KeyEnv = append([]string(nil), copilotEnvVars...)
	p.AmbientCredentials = true
	p.MetadataHostnames = append([]string(nil), copilotMetadataHostnames...)
	p.ResponsesDropReplayItemIDs = true
	p.ResponsesOmitPromptCacheKey = true
	p.ResponsesIssuer = "github_responses"
	p.ResponsesDistrustCompletedStatus = true
	p.ResponsesNoNativeCompaction = true
	p.ResponsesReasoningFromProfile = true
	p.MessageCarrierOpaque = "reasoning_opaque"
	p.MessageCarrierText = "reasoning_text"
	p.ReplaysToolCallSignatures = true
	p.StaticModels = append([]string(nil), copilotStaticModels...)
	p.PickerCatalog = copilotPickerCatalog
	p.PickerAfter = "nvidia"
	p.PickerGroup = append([]string(nil), copilotPickerGroup...)
	p.CatalogAliases = true
	p.Overlay = &IcebathOverlay{ExtraEnvVars: []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN"}}
	p.OverlayAliases = []string{"github"}
	p.RegistryAuthType = AuthTypeAPIKey
	p.BaseURLEnv = "COPILOT_API_BASE_URL"
	p.RegistryAfter = "lmstudio"
	p.AccountCard = map[string]string{"name": "GitHub Copilot", "docs_url": "https://docs.github.com/en/copilot"}
	p.SetupReasoningEfforts = copilotSetupReasoningEfforts
	p.AccountStatus = copilotAccountStatus
	p.BuildAPIKwargsExtrasHook = copilotBuildAPIKwargsExtras
	p.APIModeForModelHook = copilotAPIModeForModel
	p.ClientHeadersHook = copilotClientHeaders
	p.RequestHeadersHook = copilotRequestHeadersHook
	p.MaxTokensParamHook = copilotMaxTokensParam
	p.ReasoningPayloadHook = copilotReasoningPayload
	p.ReplayMessageCarriersHook = copilotReplayMessageCarriers
	p.ReplaysFunctionThoughtSignatureHook = copilotReplaysFunctionThoughtSignature
	p.LiveContextLengthHook = copilotLiveContextLength
	p.NormalizeModelIDHook = copilotNormalizeModelID
	p.ProbeModelsHook = copilotProbeModels
	p.ResolveAPIKeySecretHook = copilotResolveAPIKeySecret
	p.RuntimeBaseURLHook = copilotRuntimeBaseURL
	p.PreparePoolEntryHook = copilotPreparePoolEntry
	p.SeedPoolCredentialsHook = copilotSeedPoolCredentials
	p.RemoveCredentialSourceHook = copilotRemoveCredentialSource
	p.RefreshRuntimeCredentialsHook = copilotRefreshRuntimeCredentials
	p.CredentialRefreshNoticeHook = copilotCredentialRefreshNotice
	p.IsStaleCredentialErrorHook = copilotIsStaleCredentialError
	p.RequestRejectionHintHook = copilotRequestRejectionHint
	p.MissingCredentialsHintHook = copilotMissingCredentialsHint
	return p
}

// Reports a base url on the Copilot API host family
func isCopilotHost(baseURL string) bool { return HostMatches(baseURL, copilotHost) }

// Catalog gated GitHub Models reasoning in extra_body, medium when the caller set no reasoning config
func copilotBuildAPIKwargsExtras(reasoning *Reasoning, kw map[string]any) (extra *pyjson.Object, top map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			extra, top = pyjson.ObjectOf(), map[string]any{}
		}
	}()
	model := hookString(kw, "model")
	supports, _ := kw["supports_reasoning"].(bool)
	if !supports || model == "" {
		return pyjson.ObjectOf(), map[string]any{}
	}
	supported := githubModelReasoningEfforts(hookCtx(kw), model, nil, "")
	if len(supported) == 0 {
		return pyjson.ObjectOf(), map[string]any{}
	}
	cfg := reasoningConfigMap(reasoning)
	if len(cfg) == 0 {
		return pyjson.ObjectOf("reasoning", pyjson.ObjectOf("effort", "medium")), map[string]any{}
	}
	effort := clampGithubReasoningEffort(cpOrStr(cfg["effort"]), supported)
	return pyjson.ObjectOf("reasoning", pyjson.ObjectOf("effort", effort)), map[string]any{}
}

// Per model wire, the raw id pattern without a key and the normalized id with any key, empty included
func copilotAPIModeForModel(model string, apiKey *string) APIMode {
	if apiKey == nil {
		if copilotRequiresResponsesAPI(model) {
			return ModeCodexResponses
		}
		return ModeChatCompletions
	}
	return copilotModelAPIMode(context.Background(), model, nil, *apiKey)
}

// Editor attribution headers for a Copilot host, nil elsewhere
func copilotClientHeaders(baseURL, _ string, hasImages bool) map[string]string {
	if !isCopilotHost(baseURL) {
		return nil
	}
	return copilotRequestHeaders(true, hasImages)
}

// x-initiator user on the first call of a user turn at the initiator hosts, nothing otherwise
func copilotRequestHeadersHook(baseURL, _ string, userInitiated bool) map[string]string {
	if userInitiated {
		for _, h := range copilotInitiatorHosts {
			if HostMatches(baseURL, h) {
				return map[string]string{"x-initiator": "user"}
			}
		}
	}
	return map[string]string{}
}

// max_completion_tokens on a Copilot subdomain, the transport default elsewhere
func copilotMaxTokensParam(baseURL, _ string) string {
	if strings.HasSuffix(Hostname(baseURL), "."+copilotHost) {
		return "max_completion_tokens"
	}
	return ""
}

// Catalog clamped reasoning effort payload, nil for a model without efforts or reasoning disabled
func copilotReasoningPayload(model string, r Reasoning, _ string) map[string]any {
	supported := githubModelReasoningEfforts(context.Background(), model, nil, "")
	if len(supported) == 0 {
		return nil
	}
	if r.Enabled != nil && !*r.Enabled {
		return nil
	}
	return map[string]any{"effort": clampGithubReasoningEffort(r.Effort, supported)}
}

// Opaque reasoning blob with its text replayed only on a Copilot host
func copilotReplayMessageCarriers(values map[string]any, _, baseURL string) map[string]any {
	if !isCopilotHost(baseURL) || !cpTruthy(values["reasoning_opaque"]) {
		return map[string]any{}
	}
	out := map[string]any{"reasoning_opaque": values["reasoning_opaque"]}
	if cpTruthy(values["reasoning_text"]) {
		out["reasoning_text"] = values["reasoning_text"]
	}
	return out
}

// Copilot's Gemini variant expects function thought signatures back on its own host
func copilotReplaysFunctionThoughtSignature(_, baseURL string) bool { return isCopilotHost(baseURL) }

// max_prompt_tokens of the model in the account catalog
func copilotLiveContextLength(ctx context.Context, model, apiKey, _ string) int {
	return getCopilotModelContext(ctx, model, apiKey)
}

// Copilot native id of a model, the catalog consulted when a key is given
func copilotNormalizeModelID(ctx context.Context, model, apiKey string) string {
	return normalizeCopilotModelID(ctx, model, nil, apiKey)
}
