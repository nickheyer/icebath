package provider

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
)

// Max tokens kwarg for the auxiliary provider, max_completion_tokens for direct OpenAI, declaring profiles and newer OpenAI models
func AuxiliaryMaxTokensParam(ctx context.Context, value int, model string) map[string]any {
	customBase := auxCurrentCustomBaseURL(ctx)
	openrouterKey, _ := scopedKeyEnv(ctx, "OPENROUTER_API_KEY")
	direct := openrouterKey == "" && auxReadNousAuth(ctx) == nil &&
		(baseURLHostname(customBase) == "api.openai.com" ||
			routeAnswer(ctx, "max_tokens_param", "", nil, map[string]any{"base_url": customBase, "model": model}) == "max_completion_tokens")
	if direct || config.ModelForcesMaxCompletionTokens(model) {
		return map[string]any{"max_completion_tokens": value}
	}
	return map[string]any{"max_tokens": value}
}
