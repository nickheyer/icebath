package provider

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// FallbackAPIModeAgent is the agent half of fallback wire detection, its Azure, direct OpenAI and Responses predicates
type FallbackAPIModeAgent interface {
	IsAzureOpenAIURL(baseURL string) bool
	IsDirectOpenAIURL(baseURL string) bool
	ProviderModelRequiresResponsesAPI(model, providerName string) bool
}

// Messages only host match shared with api mode detection, Anthropic, an anthropic suffix or Kimi Code
func isAnthropicWireURL(url string) bool {
	return HostMandatedMode(url) == ModeAnthropicMessages
}

// FallbackAPIModeHint is the explicit flag and api mode of a fallback entry read from its original base url
func FallbackAPIModeHint(ctx context.Context, fb map[string]any, fbProvider string, fbBaseURLHint *string) (bool, string, error) {
	if explicit := (*resolver)(nil).parseAPIMode(pyOr(fb["api_mode"], fb["transport"])); explicit != "" {
		return true, string(explicit), nil
	}
	if fbProvider != "" && fbProvider != "custom" && fbProvider != "moa" {
		entry, err := NamedCustomProvider(ctx, "", fbProvider)
		if err != nil {
			return false, "", err
		}
		if declared := entry["api_mode"]; pyTruthy(declared) {
			return true, cfgString(declared), nil
		}
	}
	if fbProvider == "anthropic" || (fbBaseURLHint != nil && *fbBaseURLHint != "" && isAnthropicWireURL(*fbBaseURLHint)) {
		return false, string(ModeAnthropicMessages), nil
	}
	return false, string(ModeChatCompletions), nil
}

// FallbackAPIModeResolved re-detects the wire from provider, resolved base url and model after the hint chose chat
func FallbackAPIModeResolved(ctx context.Context, agent FallbackAPIModeAgent, fbProvider, fbModel, fbBaseURL string) (string, error) {
	if fbProvider == "openai-codex" {
		return string(ModeCodexResponses), nil
	}
	if family := opencodeFamilyForCustom(fbProvider, fbBaseURL); family != "" {
		return string(OpenCodeModelMode(family, fbModel)), nil
	}
	switch fbProvider {
	case "nous", "nous-portal", "nousresearch":
		return string(NousMode(fbModel, modelsResolver(ctx).nousAnthropicWire())), nil
	}
	if fbBaseURL != "" {
		if _, err := pyurl.BaseURLHostname(fbBaseURL); err != nil {
			return "", err
		}
	}
	if isAnthropicWireURL(fbBaseURL) {
		return string(ModeAnthropicMessages), nil
	}
	if agent.IsAzureOpenAIURL(fbBaseURL) {
		return string(ModeChatCompletions), nil
	}
	if agent.IsDirectOpenAIURL(fbBaseURL) || agent.ProviderModelRequiresResponsesAPI(fbModel, fbProvider) {
		return string(ModeCodexResponses), nil
	}
	host, _ := pyurl.BaseURLHostname(fbBaseURL)
	if fbProvider == "bedrock" || (strings.HasPrefix(host, "bedrock-runtime.") && baseURLHostMatches(fbBaseURL, "amazonaws.com")) {
		return string(modeBedrockConverse), nil
	}
	return string(ModeChatCompletions), nil
}
