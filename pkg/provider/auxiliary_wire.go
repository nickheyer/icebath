package provider

import "context"

// Sanitizes Chat Completions SDK requests at the resolved client boundary, leaving native adapter replay untouched
func auxPrepareChatMessages(ctx context.Context, client AuxClient, kwargs map[string]any) map[string]any {
	sdk, ok := client.(*auxOpenAIClient)
	if !ok || sdk == nil {
		return kwargs
	}
	raw, has := kwargs["messages"]
	if !has {
		return kwargs
	}
	model, _ := kwargs["model"].(string)
	baseURL := sdk.BaseURL()
	messages := chatCompletionsConvertMessages(shapeWireCarriers(ctx, auxMessageList(raw), model, baseURL, nil), model, baseURL)
	out := make(map[string]any, len(kwargs))
	for k, v := range kwargs {
		out[k] = v
	}
	out["messages"] = messages
	return out
}

// Message maps of a kwargs messages value, accepting typed and untyped slices
func auxMessageList(raw any) []map[string]any {
	switch v := raw.(type) {
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// Relay metadata for one auxiliary attempt, always decoded as a chat completions request
func auxRelayChatMetadata(metadata map[string]any) map[string]any {
	out := make(map[string]any, len(metadata)+1)
	for k, v := range metadata {
		out[k] = v
	}
	out["api_mode"] = "chat_completions"
	return out
}
