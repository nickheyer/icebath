package provider

import "strings"

// Providers whose OpenAI shaped endpoint expects Anthropic media blocks
var auxAnthropicCompatProviders = map[string]bool{"minimax": true, "minimax-oauth": true, "minimax-cn": true}

// True for known Anthropic compatible providers or any base URL with an anthropic path
func auxIsAnthropicCompatEndpoint(provider, baseURL string) bool {
	return auxAnthropicCompatProviders[provider] || strings.Contains(strings.ToLower(baseURL), "/anthropic")
}

// OpenAI media block type mapped to the Anthropic block type and the default data URL media type
var auxAnthropicMediaBlocks = map[string][2]string{"image_url": {"image", "image/png"}, "video_url": {"video", "video/mp4"}}

// OpenAI image and video URL blocks rewritten as Anthropic source blocks, other messages kept as is
func auxConvertOpenAIImagesToAnthropic(messages any) any {
	list, _ := auxListValue(messages)
	converted := make([]any, 0, len(list))
	for _, raw := range list {
		msg, _ := raw.(map[string]any)
		content, isList := auxListValue(msg["content"])
		if _, isStr := msg["content"].(string); isStr || !isList {
			converted = append(converted, raw)
			continue
		}
		newContent := make([]any, 0, len(content))
		changed := false
		for _, rawBlock := range content {
			block, _ := rawBlock.(map[string]any)
			blockType, _ := block["type"].(string)
			mapping, media := auxAnthropicMediaBlocks[blockType]
			if !media {
				newContent = append(newContent, rawBlock)
				continue
			}
			inner, _ := block[blockType].(map[string]any)
			url, _ := inner["url"].(string)
			anthType, mediaType := mapping[0], mapping[1]
			var source map[string]any
			if strings.HasPrefix(url, "data:") {
				header, b64data, _ := strings.Cut(url, ",")
				if strings.Contains(header, ":") && strings.Contains(header, ";") {
					_, afterColon, _ := strings.Cut(header, ":")
					mediaType, _, _ = strings.Cut(afterColon, ";")
				}
				source = map[string]any{"type": "base64", "media_type": mediaType, "data": b64data}
			} else {
				source = map[string]any{"type": "url", "url": url}
			}
			newContent = append(newContent, map[string]any{"type": anthType, "source": source})
			changed = true
		}
		if !changed {
			converted = append(converted, raw)
			continue
		}
		next := make(map[string]any, len(msg))
		for k, v := range msg {
			next[k] = v
		}
		next["content"] = newContent
		converted = append(converted, next)
	}
	return converted
}
