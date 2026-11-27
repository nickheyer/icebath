package provider

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	githubcopilotconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/githubcopilot/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

var responsesFamily = regexp.MustCompile(`^gpt-(\d+)`)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := githubcopilotconfig.CopilotBaseURL
	acpBase := githubcopilotconfig.ACPBaseURL
	return []provider.Spec{{
		Name:               githubcopilotconfig.CopilotName,
		Shape:              shape,
		ReasoningPayload:   reasoningPayload,
		RequestHeaders:     headers,
		OutputField:        outputField,
		Replay:             replay,
		FunctionSignatures: functionSignatures,
		Native:             native,
		RejectionHint:      rejectionHint,
		RefreshNotice:      refreshNotice,
		DisplayName:        githubcopilotconfig.CopilotTitle,
		Rank:               5,
		Description:        "Models of your GitHub Copilot plan, by GitHub sign-in or token",
		Account: []config.Setting{{Key: providerconfig.APIKeyField, Title: "GitHub token", About: "Your GitHub token for an account with Copilot.",
			Help: "A GitHub token starts with gho_, github_pat_ or ghu_.", Default: "", Secret: true}},
		BaseURL:                   base,
		BaseURLEnv:                githubcopilotconfig.CopilotBaseURLEnv,
		ModelsURL:                 provider.ModelsURL(base),
		Mode:                      provider.ModeChatCompletions,
		Auth:                      provider.AuthAPIKey,
		AuthType:                  provider.AuthTypeAPIKey,
		KeyEnv:                    []string{githubcopilotconfig.CopilotTokenEnv, githubcopilotconfig.GHTokenEnv, githubcopilotconfig.GitHubTokenEnv},
		Aliases:                   []string{"github-copilot", "github-models", "github-model", "github"},
		DefaultModel:              githubcopilotconfig.CopilotDefaultModel,
		Aggregator:                true,
		ModelListing:              true,
		HealthCheck:               true,
		VisionToolMessages:        true,
		ModelsDevID:               "github-copilot",
		Hostnames:                 []string{"githubcopilot.com", "models.github.ai"},
		PerModelMode:              mode,
		AmbientCredentials:        true,
		AccountCard:               map[string]string{"name": "GitHub Copilot", "docs_url": "https://docs.github.com/en/copilot"},
		MetadataHostnames:         []string{"models.inference.ai.azure.com"},
		MessageCarrierOpaque:      "reasoning_opaque",
		MessageCarrierText:        "reasoning_text",
		ReplaysToolCallSignatures: true,
		StaticModels: []string{
			"gpt-5.4", "gpt-5.4-mini", "gpt-5-mini", "gpt-5.3-codex", "gpt-5.2-codex", "gpt-4.1", "gpt-4o", "gpt-4o-mini",
			"claude-sonnet-4.6", "claude-sonnet-5", "claude-sonnet-4", "claude-sonnet-4.5", "claude-haiku-4.5",
			"gemini-3.1-pro-preview", "gemini-3-pro-preview", "gemini-3-flash-preview", "gemini-2.5-pro",
		},
		PickerAfter:                      "nvidia",
		PickerGroup:                      pickerGroup(),
		CatalogAliases:                   true,
		Overlay:                          &provider.IcebathOverlay{ExtraEnvVars: []string{githubcopilotconfig.CopilotTokenEnv, githubcopilotconfig.GHTokenEnv}},
		OverlayAliases:                   []string{"github"},
		RegistryAuthType:                 provider.AuthTypeAPIKey,
		RegistryAfter:                    "lmstudio",
		ResponsesDropReplayItemIDs:       true,
		ResponsesOmitPromptCacheKey:      true,
		ResponsesIssuer:                  "github_responses",
		ResponsesDistrustCompletedStatus: true,
		ResponsesNoNativeCompaction:      true,
		ResponsesReasoningFromProfile:    true,
	}, {
		Name:                  githubcopilotconfig.ACPName,
		DisplayName:           githubcopilotconfig.ACPTitle,
		Description:           "Your Copilot plan through the Copilot CLI on this computer",
		BaseURL:               acpBase,
		BaseURLEnv:            githubcopilotconfig.ACPBaseURLEnv,
		ModelsURL:             provider.ModelsURL(acpBase),
		Mode:                  provider.ModeChatCompletions,
		Auth:                  provider.AuthOAuth,
		AuthType:              provider.AuthTypeExternalProcess,
		Aliases:               []string{"github-copilot-acp", "copilot-acp-agent"},
		DefaultModel:          githubcopilotconfig.ACPDefaultModel,
		ModelListing:          true,
		HealthCheck:           true,
		VisionToolMessages:    true,
		ProcessCommand:        "copilot",
		ProcessArgs:           []string{"--acp", "--stdio"},
		ProcessCommandEnvVars: []string{"ICEBATH_COPILOT_ACP_COMMAND", "COPILOT_CLI_PATH"},
		ProcessArgsEnvVar:     "ICEBATH_COPILOT_ACP_ARGS",
		AccountCard: map[string]string{"name": "GitHub Copilot (ACP)", "flow": "external", "cli_command": "copilot login",
			"docs_url": "https://docs.github.com/en/copilot"},
		StaticModels:   []string{"copilot-acp"},
		PickerAfter:    "copilot",
		PickerGroup:    pickerGroup(),
		CatalogAliases: true,
		Overlay: &provider.IcebathOverlay{Transport: "codex_responses", AuthType: provider.AuthTypeExternalProcess,
			BaseURLOverride: githubcopilotconfig.ACPMarkerURL, BaseURLEnvVar: githubcopilotconfig.ACPBaseURLEnv},
		OverlayAliases: []string{"github-copilot-acp"},
		RegistryAfter:  "copilot",
	}}
}

func mode(model string) provider.APIMode {
	match := responsesFamily.FindStringSubmatch(model)
	if match == nil {
		return provider.ModeChatCompletions
	}
	if version, err := strconv.Atoi(match[1]); err == nil && version >= 5 && !strings.HasPrefix(model, "gpt-5-mini") {
		return provider.ModeCodexResponses
	}
	return provider.ModeChatCompletions
}

func pickerGroup() []string {
	return []string{"copilot", "GitHub Copilot", "GitHub token API or copilot --acp process"}
}
