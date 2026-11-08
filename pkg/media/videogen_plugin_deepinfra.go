package media

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/plugins"
)

// Picker rows of the video-gen tagged live DeepInfra catalog, empty when it is unreachable
func deepInfraVideoModels(ctx context.Context) []map[string]any {
	rows := []map[string]any{}
	for _, item := range deepInfraModelsByTag(ctx, pluginEnv(), "video-gen") {
		id, _ := item["id"].(string)
		if !pyTruthy(item["id"]) {
			continue
		}
		meta, _ := item["metadata"].(map[string]any)
		desc, _ := meta["description"].(string)
		if r := []rune(desc); len(r) > 80 {
			desc = string(r[:80])
		}
		rows = append(rows, map[string]any{"id": id, "display": id[strings.LastIndex(id, "/")+1:], "strengths": desc})
	}
	return rows
}

// Text and image to video through the DeepInfra OpenAI compatible videos endpoint
func newDeepInfraVideoGenProvider() *openAICompatibleVideoGenProvider {
	return newOpenAICompatibleVideoGenProvider(openAICompatibleVideoGenProvider{
		providerID: "deepinfra", label: "DeepInfra", envKey: "DEEPINFRA_API_KEY",
		defaultBaseURL: "https://api.deepinfra.com/v1/openai", models: deepInfraVideoModels,
		caps: func() map[string]any {
			return map[string]any{"modalities": []any{"text", "image"}, "aspect_ratios": []any{"16:9", "9:16", "1:1"},
				"resolutions": []any{"480p", "720p", "1080p"}, "max_duration": 10, "min_duration": 1, "supports_audio": false,
				"supports_negative_prompt": true, "supports_seed": true, "supports_upscale": false, "max_reference_images": 0}
		},
		setup: func() map[string]any {
			return map[string]any{"name": "DeepInfra", "badge": "paid",
				"tag":      "Wan, p-video, … — live catalog from api.deepinfra.com; text-to-video & image-to-video",
				"env_vars": []any{map[string]any{"key": "DEEPINFRA_API_KEY", "prompt": "DeepInfra API key", "url": "https://deepinfra.com/dash/api_keys"}}}
		},
	})
}

func init() {
	plugins.RegisterBundled(videoGenBackendPlugin{name: "deepinfra", author: "Georgi Atsev", requiresEnv: []string{"DEEPINFRA_API_KEY"},
		description: "DeepInfra video generation (text-to-video & image-to-video) via the OpenAI-compatible /v1/openai/videos endpoint. Catalog discovered live from api.deepinfra.com.",
		provider:    func() VideoGenProvider { return newDeepInfraVideoGenProvider() }})
}
