package provider

// Profiles the bundled qwen-oauth plugin registers
func qwenPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "qwen-oauth", keys: []string{"QWEN_API_KEY"}, base: "https://portal.qwen.ai/v1",
		authType: AuthTypeOAuthExternal, maxTokens: 65536}, "qwen", "qwen-portal", "qwen-cli")
	p.PrepareMessagesHook = qwenPrepareMessages
	p.BuildExtraBodyHook = func(string, map[string]any) map[string]any { return map[string]any{"vl_high_resolution_images": true} }
	p.BuildAPIKwargsExtrasHook = func(_ *Reasoning, kw map[string]any) (map[string]any, map[string]any) {
		if md := kw["qwen_session_metadata"]; pyTruthy(md) {
			return map[string]any{}, map[string]any{"metadata": md}
		}
		return map[string]any{}, map[string]any{}
	}
	return []Profile{p}
}

// List content as dict parts with strings wrapped and image urls copied, nil when nothing changed
func qwenNormalizeParts(content []any) []any {
	var parts []any
	changed := false
	for _, part := range content {
		switch v := part.(type) {
		case string:
			parts = append(parts, map[string]any{"type": "text", "text": v})
			changed = true
		case map[string]any:
			if img, ok := v["image_url"].(map[string]any); ok {
				v = copyDict(v)
				v["image_url"] = copyDict(img)
				changed = true
			}
			parts = append(parts, v)
		default:
			changed = true
		}
	}
	if len(parts) > 0 && changed {
		return parts
	}
	return nil
}

// Content normalized to dict parts with cache control on the first system message, copying only touched messages
func qwenPrepareMessages(messages []map[string]any) []map[string]any {
	if len(messages) == 0 {
		return []map[string]any{}
	}
	prepared := append([]map[string]any(nil), messages...)
	systemIdx := -1
	for idx, msg := range messages {
		if msg == nil {
			continue
		}
		if systemIdx < 0 && msg["role"] == "system" {
			systemIdx = idx
		}
		switch content := msg["content"].(type) {
		case string:
			next := copyDict(msg)
			next["content"] = []any{map[string]any{"type": "text", "text": content}}
			prepared[idx] = next
		case []any:
			if parts := qwenNormalizeParts(content); parts != nil {
				next := copyDict(msg)
				next["content"] = parts
				prepared[idx] = next
			}
		}
	}
	if systemIdx >= 0 {
		msg := prepared[systemIdx]
		if content, ok := msg["content"].([]any); ok && len(content) > 0 {
			if last, ok := content[len(content)-1].(map[string]any); ok {
				contentCopy := append([]any(nil), content...)
				last = copyDict(last)
				last["cache_control"] = map[string]any{"type": "ephemeral"}
				contentCopy[len(contentCopy)-1] = last
				next := copyDict(msg)
				next["content"] = contentCopy
				prepared[systemIdx] = next
			}
		}
	}
	return prepared
}
