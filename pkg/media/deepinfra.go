package media

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Default DeepInfra OpenAI compatible origin
const DefaultDeepInfraBaseURL = "https://api.deepinfra.com/v1/openai"

const deepInfraModelsQuery = "filter=true&sort_by=icebath"

var deepInfraSurfaceTags = map[string]bool{"chat": true, "embed": true, "image-gen": true, "tts": true, "stt": true, "video-gen": true}

var deepInfraExcludeRe = regexp.MustCompile(`(?i)(embed|rerank|whisper|stable-diffusion|flux|sdxl|tts|bark|speech|image-gen|clip|vit-|dpt-)`)

var deepInfraCatalogCache = struct {
	sync.Mutex
	entries map[string][]map[string]any
	failed  map[string]time.Time
}{entries: map[string][]map[string]any{}, failed: map[string]time.Time{}}

const deepInfraCatalogNegTTL = 60 * time.Second

// DeepInfra base URL from a config section, then the env, then the default
func deepInfraBaseURL(env *tools.Env, sec config.Tree) string {
	if v := cfgString(sec, "base_url"); v != "" {
		return strings.TrimRight(v, "/")
	}
	if v := secret(env, "DEEPINFRA_BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return DefaultDeepInfraBaseURL
}

// Raw tagged DeepInfra catalog, cached per base URL and key, nil on failure
func fetchDeepInfraCatalog(ctx context.Context, env *tools.Env) []map[string]any {
	base := deepInfraBaseURL(env, nil)
	key := base + "#" + secret(env, "DEEPINFRA_API_KEY")
	deepInfraCatalogCache.Lock()
	if entries, ok := deepInfraCatalogCache.entries[key]; ok {
		deepInfraCatalogCache.Unlock()
		return entries
	}
	if failedAt, ok := deepInfraCatalogCache.failed[key]; ok && time.Since(failedAt) < deepInfraCatalogNegTTL {
		deepInfraCatalogCache.Unlock()
		return nil
	}
	deepInfraCatalogCache.Unlock()
	headers := map[string]string{}
	if apiKey := secret(env, "DEEPINFRA_API_KEY"); apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}
	payload, _, err := getJSON(ctx, base+"/models?"+deepInfraModelsQuery, headers, 5*time.Second)
	deepInfraCatalogCache.Lock()
	defer deepInfraCatalogCache.Unlock()
	if err != nil {
		deepInfraCatalogCache.failed[key] = time.Now()
		return nil
	}
	data, ok := payload["data"].([]any)
	if !ok {
		deepInfraCatalogCache.failed[key] = time.Now()
		return nil
	}
	var entries []map[string]any
	for _, item := range data {
		if m, ok := item.(map[string]any); ok {
			entries = append(entries, m)
		}
	}
	deepInfraCatalogCache.entries[key] = entries
	delete(deepInfraCatalogCache.failed, key)
	return entries
}

// DeepInfra catalog items whose surface tag matches, nil on network failure
func deepInfraModelsByTag(ctx context.Context, env *tools.Env, tag string) []map[string]any {
	data := fetchDeepInfraCatalog(ctx, env)
	if data == nil {
		return nil
	}
	matched := []map[string]any{}
	for _, item := range data {
		mid := stripAt(item, "id")
		rawMeta, present := item["metadata"]
		if mid == "" || !present || rawMeta == nil {
			continue
		}
		meta, _ := rawMeta.(map[string]any)
		if meta == nil {
			meta = map[string]any{}
		}
		tags := stringList(meta["tags"])
		hasSurface := false
		hit := false
		for _, t := range tags {
			if deepInfraSurfaceTags[t] {
				hasSurface = true
			}
			if t == tag {
				hit = true
			}
		}
		if !hasSurface {
			hit = tag == "chat" && !deepInfraExcludeRe.MatchString(mid)
		}
		if hit {
			matched = append(matched, map[string]any{"id": mid, "metadata": meta})
		}
	}
	return matched
}

// DeepInfra model ids carrying a surface tag, empty on failure
func deepInfraModelIDs(ctx context.Context, env *tools.Env, tag string) []string {
	var ids []string
	for _, item := range deepInfraModelsByTag(ctx, env, tag) {
		ids = append(ids, stripAt(item, "id"))
	}
	return ids
}
