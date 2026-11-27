package models

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/models"
	routerconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/router/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// File of the profile's cache the efforts of Router's models are kept in
const effortsCache = "router_catalog.json"

// Router's catalog: its models, and the reasoning efforts each publishes, by the cache file of the profile it was read for
type catalog struct {
	mu      sync.Mutex
	efforts map[string]map[string][]string
	warming map[string]bool
}

func Register(registry *models.Registry) {
	registry.Register(&catalog{efforts: map[string]map[string][]string{}, warming: map[string]bool{}})
}

// Models of Router's catalog, in its order
func (c *catalog) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	if route.Spec != routerconfig.Name {
		return nil, false
	}
	items, ok := fetch(ctx, route)
	if !ok {
		return nil, false
	}
	ids := []string{}
	for _, item := range items {
		row, _ := item.(map[string]any)
		if id, _ := row["id"].(string); id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids, len(ids) > 0
}

// Items of Router's models listing
func fetch(ctx context.Context, route provider.Route) ([]any, bool) {
	headers := map[string]string{"Accept": "application/json", "User-Agent": provider.UserAgent("icebath-cli")}
	if route.APIKey != "" {
		headers["Authorization"] = "Bearer " + route.APIKey
	}
	data, err := models.GetJSON(ctx, strings.TrimRight(route.BaseURL, "/")+"/models", 8*time.Second, headers)
	if err != nil {
		slog.Debug("router: catalog fetch failed", "error", err)
		return nil, false
	}
	items := data
	if document, ok := data.(map[string]any); ok {
		if items, ok = document["data"]; !ok {
			items = []any{}
		}
	}
	list, ok := items.([]any)
	return list, ok
}

// Efforts each model of the listing publishes, empty for one that takes no reasoning; levels off the ladder left out
func efforts(items []any) map[string][]string {
	out := map[string][]string{}
	for _, item := range items {
		row, _ := item.(map[string]any)
		id, _ := row["id"].(string)
		router, _ := row["router"].(map[string]any)
		capabilities, _ := router["capabilities"].(map[string]any)
		reasoning, ok := capabilities["reasoning"].(map[string]any)
		if id = strings.TrimSpace(id); id == "" || !ok {
			continue
		}
		if reasoning["supported"] == false {
			out[id] = []string{}
			continue
		}
		var known, unknown []string
		levels, _ := reasoning["efforts"].([]any)
		for _, level := range levels {
			entry, _ := level.(map[string]any)
			value, _ := entry["value"].(string)
			switch value = strings.TrimSpace(value); {
			case value == "":
			case provider.Rung(value) >= 0:
				known = append(known, value)
			default:
				unknown = append(unknown, value)
			}
		}
		if len(unknown) > 0 {
			slog.Info(fmt.Sprintf("router: model %s publishes unrecognized reasoning effort level(s) %v; ignoring them", id, unknown))
		}
		if len(known) > 0 {
			out[id] = known
		}
	}
	return out
}

// Efforts the route's model publishes in Router's catalog, as last read: from memory, else the profile's cache, else unknown while the
// catalog is read in the background
func (c *catalog) Thinking(ctx context.Context, env *models.Env) (provider.Thinking, bool) {
	if env.Route.Spec != routerconfig.Name || strings.TrimSpace(env.Route.Model) == "" {
		return provider.Thinking{}, false
	}
	path := env.Cache(effortsCache)
	c.mu.Lock()
	held, ok := c.efforts[path]
	c.mu.Unlock()
	if !ok {
		if held = load(path); held != nil {
			c.mu.Lock()
			c.efforts[path] = held
			c.mu.Unlock()
		} else {
			c.warm(ctx, env.Route, path)
			return provider.Thinking{}, false
		}
	}
	levels, listed := held[strings.TrimSpace(env.Route.Model)]
	if !listed {
		return provider.Thinking{}, false
	}
	return provider.Thinking{Takes: len(levels) > 0, Efforts: slices.Clone(levels), Known: true}, true
}

// Reads Router's catalog in the background once for the profile, keeping its efforts in memory and in the profile's cache
func (c *catalog) warm(ctx context.Context, route provider.Route, path string) {
	c.mu.Lock()
	if c.warming[path] || route.APIKey == "" {
		c.mu.Unlock()
		return
	}
	c.warming[path] = true
	c.mu.Unlock()
	go func() {
		items, ok := fetch(context.WithoutCancel(ctx), route)
		if !ok {
			return
		}
		found := efforts(items)
		if len(found) == 0 {
			return
		}
		c.mu.Lock()
		c.efforts[path] = found
		c.mu.Unlock()
		raw, err := json.Marshal(map[string]any{"ts": time.Now().Unix(), "efforts": found})
		if err == nil {
			err = fsx.Write(path, raw, 0o600)
		}
		if err != nil {
			slog.Debug("router: caps disk mirror write failed", "error", err)
		}
	}()
}

// Efforts kept in the profile's cache, nil when none can be read
func load(path string) map[string][]string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var kept struct {
		Efforts map[string][]string `json:"efforts"`
	}
	if json.Unmarshal(raw, &kept) != nil || len(kept.Efforts) == 0 {
		return nil
	}
	return kept.Efforts
}
