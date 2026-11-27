package models

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/models"
	deepinfraconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/deepinfra/config"
	"github.com/nickheyer/icebath/internal/provider"
)

// DeepInfra's tagged catalog for each address and key, a failed read not tried again for a minute
type catalog struct {
	mu     sync.Mutex
	items  map[string][]map[string]any
	failed map[string]time.Time
}

const failedTTL = time.Minute

// Tags of the surfaces DeepInfra serves, a model with none of them being chat unless its id says otherwise
var (
	surfaces = []string{"chat", "embed", "image-gen", "tts", "stt", "video-gen"}
	notChat  = regexp.MustCompile(`(?i)(embed|rerank|whisper|stable-diffusion|flux|sdxl|tts|bark|speech|image-gen|clip|vit-|dpt-)`)
)

func Register(registry *models.Registry) {
	registry.Register(&catalog{items: map[string][]map[string]any{}, failed: map[string]time.Time{}})
}

// Answered by the catalog below
func (c *catalog) Models(context.Context, provider.Route) ([]string, bool) {
	return nil, false
}

// Models of the catalog with their metadata, read once for each address and key; false when it can't be read
func (c *catalog) read(ctx context.Context, route provider.Route, refresh bool) ([]map[string]any, bool) {
	base := strings.TrimRight(route.BaseURL, "/")
	key := base + "#" + models.Identity(route.APIKey)
	c.mu.Lock()
	held, ok := c.items[key]
	failed, failing := c.failed[key]
	c.mu.Unlock()
	if !refresh && ok {
		return held, true
	}
	if !refresh && failing && time.Since(failed) < failedTTL {
		return nil, false
	}
	headers := map[string]string{"User-Agent": provider.UserAgent("icebath-cli")}
	if route.APIKey != "" {
		headers["Authorization"] = "Bearer " + route.APIKey
	}
	data, err := models.Request(ctx, "GET", base+"/models?filter=true&sort_by=icebath", headers, nil, 5*time.Second)
	listed, isList := data["data"].([]any)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil || !isList {
		c.failed[key] = time.Now()
		return nil, false
	}
	items := make([]map[string]any, 0, len(listed))
	for _, item := range listed {
		if row, ok := item.(map[string]any); ok {
			items = append(items, row)
		}
	}
	c.items[key] = items
	delete(c.failed, key)
	return items, true
}

// Models whose surface tags include the tag, a chat model also being one with no surface tag whose id is not of another surface
func (c *catalog) tagged(ctx context.Context, route provider.Route, tag string, refresh bool) ([]map[string]any, bool) {
	items, ok := c.read(ctx, route, refresh)
	if !ok {
		return nil, false
	}
	var out []map[string]any
	for _, item := range items {
		id, _ := item["id"].(string)
		metadata, present := item["metadata"]
		if id == "" || !present || metadata == nil {
			continue
		}
		fields, _ := metadata.(map[string]any)
		tags, _ := fields["tags"].([]any)
		surface := slices.ContainsFunc(tags, func(t any) bool { s, _ := t.(string); return slices.Contains(surfaces, s) })
		hit := slices.Contains(tags, any(tag))
		if !surface {
			hit = tag == "chat" && !notChat.MatchString(id)
		}
		if hit {
			out = append(out, map[string]any{"id": id, "metadata": fields})
		}
	}
	return out, true
}

// Models of a surface the catalog tags, such as tts, stt, image-gen or video-gen, for the services of DeepInfra beside chat; none when it
// can't be read
func (c *catalog) Tagged(ctx context.Context, env *models.Env, tag string) ([]map[string]any, bool) {
	if env.Route.Spec != deepinfraconfig.Name {
		return nil, false
	}
	items, _ := c.tagged(ctx, env.Route, tag, env.Refresh)
	return items, true
}

// Chat models of the catalog; none when it can't be read, as it has no curated list
func (c *catalog) Catalog(ctx context.Context, env *models.Env) (models.Catalog, bool) {
	if env.Route.Spec != deepinfraconfig.Name {
		return models.Catalog{}, false
	}
	items, _ := c.tagged(ctx, env.Route, "chat", env.Refresh)
	ids := []string{}
	for _, item := range items {
		ids = append(ids, item["id"].(string))
	}
	return models.Catalog{IDs: ids}, true
}

// First chat model the catalog tags as seeing images, for side jobs on images; none without a key or when it can't be read, as the model
// DeepInfra runs side jobs on can't see
func (c *catalog) VisionModel(ctx context.Context, env *models.Env) (string, bool) {
	if env.Route.Spec != deepinfraconfig.Name {
		return "", false
	}
	if strings.TrimSpace(env.Route.APIKey) == "" {
		return "", true
	}
	items, _ := c.tagged(ctx, env.Route, "chat", false)
	for _, item := range items {
		metadata, _ := item["metadata"].(map[string]any)
		if tags, _ := metadata["tags"].([]any); slices.Contains(tags, any("vision")) {
			return item["id"].(string), true
		}
	}
	return "", true
}

// Prices the catalog gives its chat models, for a million tokens
func (c *catalog) Prices(ctx context.Context, env *models.Env) (map[string]provider.Price, bool) {
	if env.Route.Spec != deepinfraconfig.Name {
		return nil, false
	}
	items, _ := c.tagged(ctx, env.Route, "chat", env.Refresh)
	prices := map[string]provider.Price{}
	for _, item := range items {
		metadata, _ := item["metadata"].(map[string]any)
		listed, ok := metadata["pricing"].(map[string]any)
		if !ok {
			continue
		}
		price := provider.Price{Input: models.Scalar(listed["input_tokens"]), Output: models.Scalar(listed["output_tokens"]), CacheRead: models.Scalar(listed["cache_read_tokens"])}
		if price.Input != "" || price.Output != "" || price.CacheRead != "" {
			prices[item["id"].(string)] = price
		}
	}
	return prices, true
}
