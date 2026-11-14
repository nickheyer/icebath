package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	deepInfraDefaultBaseURL = "https://api.deepinfra.com/v1/openai"
	deepInfraModelsQuery    = "filter=true"
	deepInfraFailureTTL     = 60 * time.Second
	deepInfraTimeout        = 5 * time.Second
)

var deepInfraExclude = regexp.MustCompile(`(?i)(embed|rerank|whisper|stable-diffusion|flux|sdxl|tts|bark|speech|image-gen|clip|vit-|dpt-)`)

var deepInfraSurfaceTags = []string{"chat", "embed", "image-gen", "tts", "stt", "video-gen"}

type deepInfraState struct {
	mu       sync.Mutex
	catalogs map[string][]wire.Value
	failures map[string]time.Time
}

type deepInfraItem struct {
	id       string
	metadata wire.Value
}

func (c *Catalog) deepInfraCatalogURL(ctx context.Context) (cacheKey, rawURL string) {
	base := c.env(ctx, "DEEPINFRA_BASE_URL")
	if base == "" {
		base = deepInfraDefaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	fingerprint := auth.FingerprintSecret(c.env(ctx, "DEEPINFRA_API_KEY"))
	if fingerprint == "" {
		fingerprint = "anon"
	}
	return base + "#" + fingerprint, base + "/models?" + deepInfraModelsQuery
}

func (c *Catalog) deepInfraCached(cacheKey string) bool {
	st := &c.st.deepInfra
	st.mu.Lock()
	defer st.mu.Unlock()
	_, ok := st.catalogs[cacheKey]
	return ok
}

func (c *Catalog) fetchDeepInfraCatalog(ctx context.Context, timeout time.Duration, force bool) ([]wire.Value, bool) {
	cacheKey, rawURL := c.deepInfraCatalogURL(ctx)
	st := &c.st.deepInfra
	if !force {
		st.mu.Lock()
		if data, ok := st.catalogs[cacheKey]; ok {
			st.mu.Unlock()
			return data, true
		}
		failedAt, failed := st.failures[cacheKey]
		st.mu.Unlock()
		if failed && time.Since(failedAt) < deepInfraFailureTTL {
			return nil, false
		}
	}
	header := bearer(http.Header{"User-Agent": {c.userAgent()}}, c.env(ctx, "DEEPINFRA_API_KEY"))
	payload, err := c.getJSONObject(ctx, rawURL, header, "", timeout)
	data, listed := dataItems(payload)
	st.mu.Lock()
	defer st.mu.Unlock()
	if err != nil || !listed {
		st.failures[cacheKey] = time.Now()
		return nil, false
	}
	st.catalogs[cacheKey] = data
	delete(st.failures, cacheKey)
	return data, true
}

func (c *Catalog) deepInfraModelsByTag(ctx context.Context, tag string, timeout time.Duration, force bool) ([]deepInfraItem, bool) {
	data, ok := c.fetchDeepInfraCatalog(ctx, timeout, force)
	if !ok {
		return nil, false
	}
	matched := []deepInfraItem{}
	for _, item := range data {
		if !item.IsObject() {
			continue
		}
		id := itemID(item)
		rawMetadata := item.Get("metadata")
		if id == "" || !rawMetadata.Exists() || rawMetadata.Kind() == wire.KindNull {
			continue
		}
		metadata := wire.Value{}
		if rawMetadata.IsObject() {
			metadata = rawMetadata
		}
		var tags []string
		if rawTags := metadata.Get("tags"); rawTags.IsArray() {
			for _, entry := range rawTags.Elems() {
				if text, ok := entry.Str(); ok {
					tags = append(tags, text)
				}
			}
		}
		hit := tag == "chat" && !deepInfraExclude.MatchString(id)
		if slices.ContainsFunc(tags, func(t string) bool { return slices.Contains(deepInfraSurfaceTags, t) }) {
			hit = slices.Contains(tags, tag)
		}
		if hit {
			matched = append(matched, deepInfraItem{id: id, metadata: metadata})
		}
	}
	return matched, true
}

func (c *Catalog) DeepInfraModelsByTag(ctx context.Context, tag string) ([]wire.Value, bool) {
	ctx = c.scoped(ctx)
	items, ok := c.deepInfraModelsByTag(ctx, tag, deepInfraTimeout, false)
	if !ok {
		return nil, false
	}
	out := make([]wire.Value, 0, len(items))
	for _, item := range items {
		metadata := json.RawMessage("{}")
		if item.metadata.IsObject() {
			metadata = item.metadata.Raw()
		}
		encoded, err := json.Marshal(struct {
			ID       string          `json:"id"`
			Metadata json.RawMessage `json:"metadata"`
		}{item.id, metadata})
		if err != nil {
			return nil, false
		}
		value, err := wire.ParseValue(encoded)
		if err != nil {
			return nil, false
		}
		out = append(out, value)
	}
	return out, true
}

func (c *Catalog) fetchDeepInfraModels(ctx context.Context, timeout time.Duration, force bool) ([]string, bool) {
	items, ok := c.deepInfraModelsByTag(ctx, "chat", timeout, force)
	if !ok || len(items) == 0 {
		return nil, false
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.id)
	}
	return ids, true
}

func (c *Catalog) DeepInfraModelIDs(ctx context.Context, tag string, force bool) []string {
	ctx = c.scoped(ctx)
	items, _ := c.deepInfraModelsByTag(ctx, tag, deepInfraTimeout, force)
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.id)
	}
	return ids
}

func (c *Catalog) DeepInfraBaseURL(ctx context.Context, sectionBaseURL string) string {
	ctx = c.scoped(ctx)
	value := sectionBaseURL
	if value == "" {
		value = c.env(ctx, "DEEPINFRA_BASE_URL")
	}
	if value == "" {
		value = deepInfraDefaultBaseURL
	}
	return strings.TrimRight(strings.TrimSpace(value), "/")
}
