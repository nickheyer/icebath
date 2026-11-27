package provider

import "github.com/nickheyer/icebath/internal/provider"

// Thinking config of the request in the snake case google block Vertex's OpenAI compatible surface takes
func extraBody(call *provider.Call) map[string]any {
	thinking := provider.GeminiSnake(provider.GeminiThinking(call.Model, call.Ask))
	if thinking == nil {
		return nil
	}
	return map[string]any{"extra_body": map[string]any{"google": map[string]any{"thinking_config": thinking}}}
}
