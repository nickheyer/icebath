package provider

import (
	"context"
	"fmt"
	"net/http"
)

// Implemented by an SDK client whose chat completions expose with_raw_response
type chatCompletionRawCreator interface {
	CreateChatCompletionRaw(ctx context.Context, kwargs map[string]any) (any, http.Header, error)
}

// Blocking chat completion that feeds a nous usage window response's headers to the agent's credits capture
func ChatCompletionCapturingHeaders(ctx context.Context, agent ChatCompletionAgent, client any, apiKwargs map[string]any) (any, error) {
	creator, ok := client.(chatCompletionCreator)
	if !ok {
		return nil, fmt.Errorf("AttributeError: '%T' object has no attribute 'chat'", client)
	}
	credits := agent.CreditsCapture()
	var state *CreditsState
	if credits != nil {
		state = credits.Credits()
	}
	if agent.Provider() != "nous" || state == nil || !state.IsUsageWindow() {
		return creator.CreateChatCompletion(ctx, apiKwargs)
	}
	raw, ok := client.(chatCompletionRawCreator)
	if !ok {
		return creator.CreateChatCompletion(ctx, apiKwargs)
	}
	parsed, headers, err := raw.CreateChatCompletionRaw(ctx, apiKwargs)
	if err != nil {
		return nil, err
	}
	chatCompletionCapturingHeaders(agent.Provider(), credits, headers)
	return parsed, nil
}
