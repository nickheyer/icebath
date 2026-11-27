package provider

import (
	"cmp"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials/aws"
	bedrockconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/bedrock/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

// OpenAI models Bedrock Mantle serves over the Responses API, by their bare in region ids
var MantleModels = []string{"openai.gpt-6-astra", "openai.gpt-6.1-sol", "openai.gpt-6-sol", "openai.gpt-6-luna", "openai.gpt-5.5", "openai.gpt-5.6-sol",
	"openai.gpt-5.6-terra", "openai.gpt-5.6-luna"}

// Claude on Bedrock in any regional or global profile form
var claudeModel = regexp.MustCompile(`(?i)^(?:(?:global|us|eu|apac|ap|au|jp|ca|sa|me|af)\.)?anthropic\.claude`)

// API and address a Bedrock request takes for its model: a bare OpenAI id goes to Mantle's Responses API in the region, Claude signed with
// AWS to the Anthropic Messages API of the runtime, every other model to Converse; the API the provider is set to when it is set, Mantle's
// address for the Responses API
func route(r provider.Route, settings config.View) provider.Route {
	if configured := settings.GetString(providerconfig.APIMode); provider.KnownMode(configured) {
		if r.Mode = provider.APIMode(configured); r.Mode == provider.ModeCodexResponses {
			r.Region = cmp.Or(r.Region, aws.EndpointRegion(r.BaseURL), "us-east-1")
			r.BaseURL = aws.MantleURL(r.Region)
		}
		return r
	}
	switch {
	case slices.Contains(MantleModels, strings.ToLower(strings.TrimSpace(r.Model))):
		r.Region = cmp.Or(r.Region, aws.EndpointRegion(r.BaseURL), "us-east-1")
		r.Mode, r.BaseURL = provider.ModeCodexResponses, aws.MantleURL(r.Region)
	case claudeModel.MatchString(r.Model) && r.Auth == provider.AuthAWSSigV4:
		r.Mode = provider.ModeAnthropicMessages
	default:
		r.Mode = provider.ModeBedrockConverse
	}
	return r
}

// The provider's guardrail, nil unless its id and version are both set
func configured(call *provider.Call) map[string]any {
	id, version := call.Settings.GetString(bedrockconfig.Guardrail), call.Settings.GetString(bedrockconfig.GuardrailVersion)
	if id == "" || version == "" {
		return nil
	}
	return map[string]any{"guardrailIdentifier": id, "guardrailVersion": version,
		"streamProcessingMode": call.Settings.GetString(bedrockconfig.GuardrailStreaming), "trace": call.Settings.GetString(bedrockconfig.GuardrailTrace)}
}

// Guardrail of a Converse request, in its body
func guardrail(call *provider.Call) (map[string]any, map[string]any) {
	if call.Route.Mode != provider.ModeBedrockConverse {
		return nil, nil
	}
	if g := configured(call); g != nil {
		return nil, map[string]any{"guardrailConfig": g}
	}
	return nil, nil
}

// Guardrail of an InvokeModel request of the Anthropic Messages API, in its headers, the only place that API takes it
func guardrailHeaders(call *provider.Call) map[string]string {
	g := configured(call)
	if call.Route.Mode != provider.ModeAnthropicMessages || g == nil {
		return nil
	}
	headers := map[string]string{"X-Amzn-Bedrock-GuardrailIdentifier": g["guardrailIdentifier"].(string),
		"X-Amzn-Bedrock-GuardrailVersion": g["guardrailVersion"].(string)}
	switch strings.ToLower(g["trace"].(string)) {
	case "enabled", "enabled_full", "true":
		headers["X-Amzn-Bedrock-Trace"] = "ENABLED"
	}
	return headers
}
