package provider

import (
	"maps"
	"runtime"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/provider"
)

// Version of Qwen Code whose identity the Qwen Portal expects, and the host of the portal
const (
	qwenCode   = "0.14.1"
	portalHost = "portal.qwen.ai"
)

// Content of every message as parts, strings wrapped as text parts, with the cache marked at the end of the first system message
func messages(_ *provider.Call, messages []map[string]any) []map[string]any {
	prepared := make([]map[string]any, len(messages))
	system := -1
	for i, m := range messages {
		prepared[i] = m
		if m == nil {
			continue
		}
		if system < 0 && m["role"] == "system" {
			system = i
		}
		switch content := m["content"].(type) {
		case string:
			next := maps.Clone(m)
			next["content"] = []any{map[string]any{"type": "text", "text": content}}
			prepared[i] = next
		case []any:
			if parts := normalized(content); parts != nil {
				next := maps.Clone(m)
				next["content"] = parts
				prepared[i] = next
			}
		}
	}
	if system < 0 {
		return prepared
	}
	content, ok := prepared[system]["content"].([]any)
	if !ok || len(content) == 0 {
		return prepared
	}
	last, ok := content[len(content)-1].(map[string]any)
	if !ok {
		return prepared
	}
	content = append([]any(nil), content...)
	last = maps.Clone(last)
	last["cache_control"] = map[string]any{"type": "ephemeral"}
	content[len(content)-1] = last
	next := maps.Clone(prepared[system])
	next["content"] = content
	prepared[system] = next
	return prepared
}

// Parts with strings wrapped as text parts and image parts copied, nil when none changed
func normalized(content []any) []any {
	var parts []any
	changed := false
	for _, part := range content {
		switch p := part.(type) {
		case string:
			parts = append(parts, map[string]any{"type": "text", "text": p})
			changed = true
		case map[string]any:
			if image, ok := p["image_url"].(map[string]any); ok {
				p = maps.Clone(p)
				p["image_url"] = maps.Clone(image)
				changed = true
			}
			parts = append(parts, p)
		default:
			changed = true
		}
	}
	if len(parts) > 0 && changed {
		return parts
	}
	return nil
}

// Pictures the portal reads at full resolution
func extraBody(*provider.Call) map[string]any {
	return map[string]any{"vl_high_resolution_images": true}
}

// Session metadata the portal groups a conversation's requests under
func shape(call *provider.Call) (map[string]any, map[string]any) {
	if !provider.BaseHostMatches(call.Route.BaseURL, portalHost) {
		return nil, nil
	}
	session := call.SessionID
	if session == "" {
		session = "icebath"
	}
	return nil, map[string]any{"metadata": map[string]any{"sessionId": session, "promptId": uuid.NewString()}}
}

// Qwen Code's identity, which the portal takes requests from
func headers(call *provider.Call) map[string]string {
	if !provider.BaseHostMatches(call.Route.BaseURL, portalHost) {
		return nil
	}
	agent := "QwenCode/" + qwenCode + " (" + runtime.GOOS + "; " + machine() + ")"
	return map[string]string{"User-Agent": agent, "X-DashScope-CacheControl": "enable", "X-DashScope-UserAgent": agent, "X-DashScope-AuthType": "qwen-oauth"}
}

// Hardware name of this machine as its operating system gives it
func machine() string {
	switch runtime.GOARCH {
	case "amd64":
		if runtime.GOOS == "windows" {
			return "AMD64"
		}
		return "x86_64"
	case "arm64":
		if runtime.GOOS == "linux" {
			return "aarch64"
		}
		return "arm64"
	case "386":
		return "x86"
	}
	return runtime.GOARCH
}
