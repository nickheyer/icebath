package models

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/models"
	openrouterconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openrouter/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// OpenRouter's public catalog: every model with its window, its most output and its prices, kept an hour in memory and on disk for each
// profile; the curated picker list cut down to the models it serves with tools; and the prices it lists for each key
type catalog struct {
	mu      sync.Mutex
	rows    map[string]directory
	curated map[string]picked
	prices  map[string]priced
}

type directory struct {
	rows map[string]models.Row
	at   time.Time
}

type picked struct {
	catalog models.Catalog
	at      time.Time
}

type priced struct {
	prices map[string]provider.Price
	// When they are read again, sooner when none came back
	until time.Time
}

const (
	directoryTTL = time.Hour
	pricesRetry  = 2 * time.Minute
	rowsCache    = "openrouter_model_metadata.json"
	curatedCache = "openrouter_curated_catalog.json"
)

func Register(registry *models.Registry) {
	registry.Register(&catalog{rows: map[string]directory{}, curated: map[string]picked{}, prices: map[string]priced{}})
}

func (c *catalog) Models(ctx context.Context, route provider.Route) ([]string, bool) {
	if route.Spec != openrouterconfig.Name {
		return nil, false
	}
	public := route
	public.APIKey = ""
	return models.List(ctx, public, openrouterconfig.ModelsURL)
}

// Public catalog of the profile: held in memory, else on disk while fresh, else fetched; the last copy of either when the fetch fails
func (c *catalog) directory(ctx context.Context, env *models.Env) map[string]models.Row {
	path := env.Cache(rowsCache)
	c.mu.Lock()
	defer c.mu.Unlock()
	held := c.rows[path]
	if !env.Refresh {
		if len(held.rows) > 0 && time.Since(held.at) < directoryTTL {
			return held.rows
		}
		if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) < directoryTTL {
			if disk := readRows(path); len(disk) > 0 {
				c.rows[path] = directory{disk, info.ModTime()}
				return disk
			}
		}
	}
	data, err := models.Request(ctx, "GET", openrouterconfig.ModelsURL, map[string]string{"Accept": "application/json", "User-Agent": provider.UserAgent("icebath-cli")}, nil,
		10*time.Second)
	if err == nil {
		rows := map[string]models.Row{}
		items, _ := data["data"].([]any)
		for _, item := range items {
			model, _ := item.(map[string]any)
			id, _ := model["id"].(string)
			if id == "" {
				continue
			}
			top, _ := model["top_provider"].(map[string]any)
			r := models.Row{Name: id, Context: number(model["context_length"], 128_000), MaxOutput: number(top["max_completion_tokens"], 4096),
				Pricing: map[string]string{}}
			if name, ok := model["name"].(string); ok {
				r.Name = name
			}
			pricing, _ := model["pricing"].(map[string]any)
			for key, value := range pricing {
				r.Pricing[key] = models.Scalar(value)
			}
			if parameters, ok := model["supported_parameters"].([]any); ok {
				r.Parameters = []string{}
				for _, parameter := range parameters {
					if name, ok := parameter.(string); ok {
						r.Parameters = append(r.Parameters, name)
					}
				}
			}
			if reasoning, ok := model["reasoning"].(map[string]any); ok {
				efforts, _ := reasoning["supported_efforts"].([]any)
				for _, effort := range efforts {
					if level := strings.ToLower(strings.TrimSpace(models.Scalar(effort))); level != "" && !slices.Contains(r.Efforts, level) {
						r.Efforts = append(r.Efforts, level)
					}
				}
				r.Mandatory = reasoning["mandatory"] == true
			}
			aliases := []string{id}
			if canonical, _ := model["canonical_slug"].(string); canonical != "" && canonical != id {
				aliases = append(aliases, canonical)
			}
			for _, alias := range aliases {
				rows[alias] = r
				if _, bare, ok := strings.Cut(alias, "/"); ok {
					if _, taken := rows[bare]; !taken {
						rows[bare] = r
					}
				}
			}
		}
		c.rows[path] = directory{rows, time.Now()}
		if raw, err := json.Marshal(rows); err == nil {
			if err := fsx.Write(path, raw, 0o600); err != nil {
				slog.Debug("failed to save OpenRouter model metadata disk cache", "error", err)
			}
		}
		slog.Debug("fetched OpenRouter model metadata", "models", len(rows))
		return rows
	}
	if ctx.Err() == nil {
		slog.Warn("Failed to fetch model metadata from OpenRouter", "error", err)
	}
	if len(held.rows) > 0 {
		return held.rows
	}
	if disk := readRows(path); len(disk) > 0 {
		c.rows[path] = directory{disk, time.Now().Add(-directoryTTL + time.Second)}
		return disk
	}
	return map[string]models.Row{}
}

func readRows(path string) map[string]models.Row {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var rows map[string]models.Row
	if json.Unmarshal(raw, &rows) != nil {
		return nil
	}
	return rows
}

// Whole number of a decoded value, the fallback when it is none
func number(v any, fallback int) int {
	if n, ok := v.(float64); ok {
		return int(n)
	}
	return fallback
}

// Row of any model in OpenRouter's public catalog
func (c *catalog) Lookup(ctx context.Context, env *models.Env, model string) (models.Row, bool) {
	r, ok := c.directory(ctx, env)[model]
	return r, ok
}

// Window OpenRouter lists for the route's model, not the 32K it lists for Kimi models that have more
func (c *catalog) ContextLength(ctx context.Context, env *models.Env) (int, bool) {
	if env.Route.Spec != openrouterconfig.Name {
		return 0, false
	}
	r, ok := c.directory(ctx, env)[env.Route.Model]
	lower := strings.ToLower(env.Route.Model)
	if !ok || r.Context <= 0 || r.Context == 32768 && (strings.HasPrefix(lower, "kimi") || strings.Contains(lower, "moonshot")) {
		return 0, false
	}
	return r.Context, true
}

// Prices OpenRouter lists for the key of the route, kept until the catalog is read again
func (c *catalog) Prices(ctx context.Context, env *models.Env) (map[string]provider.Price, bool) {
	if env.Route.Spec != openrouterconfig.Name {
		return nil, false
	}
	key := strings.TrimRight(env.Route.BaseURL, "/") + "\x00" + models.Identity(env.Route.APIKey)
	c.mu.Lock()
	held, ok := c.prices[key]
	c.mu.Unlock()
	if ok && !env.Refresh && time.Now().Before(held.until) {
		return held.prices, true
	}
	headers := map[string]string{"Accept": "application/json", "User-Agent": provider.UserAgent("icebath-cli")}
	if env.Route.APIKey != "" {
		headers["Authorization"] = "Bearer " + env.Route.APIKey
	}
	prices := map[string]provider.Price{}
	data, err := models.Request(ctx, "GET", openrouterconfig.ModelsURL, headers, nil, 8*time.Second)
	if err == nil {
		items, _ := data["data"].([]any)
		for _, item := range items {
			model, _ := item.(map[string]any)
			id, _ := model["id"].(string)
			listed, ok := model["pricing"].(map[string]any)
			if id == "" || !ok {
				continue
			}
			r := models.Row{Pricing: map[string]string{}}
			for name, value := range listed {
				r.Pricing[name] = models.Scalar(value)
			}
			if price, ok := r.Price(); ok {
				prices[id] = price
			}
		}
	}
	until := time.Now().Add(directoryTTL)
	if len(prices) == 0 {
		until = time.Now().Add(pricesRetry)
	}
	c.mu.Lock()
	c.prices[key] = priced{prices, until}
	c.mu.Unlock()
	return prices, true
}

// Curated picker list of the online catalog, else the fallback models, cut down to the models OpenRouter serves with tools and noting the
// default, the free ones and the recommended first; kept as long as the online catalog
func (c *catalog) Catalog(ctx context.Context, env *models.Env) (models.Catalog, bool) {
	if env.Route.Spec != openrouterconfig.Name {
		return models.Catalog{}, false
	}
	path := env.Cache(curatedCache)
	ttl := env.Host.RefreshInterval(env.Config)
	c.mu.Lock()
	held, ok := c.curated[path]
	c.mu.Unlock()
	if ok && !env.Refresh && time.Since(held.at) < ttl {
		return held.catalog, true
	}
	if !env.Refresh {
		if disk, at, ok := readCurated(path); ok && time.Since(at) < ttl {
			c.remember(path, disk, at)
			return disk, true
		}
	}
	rows := env.Host.Manifest(env.Config, openrouterconfig.Name, env.Refresh)
	if len(rows) == 0 {
		for _, id := range env.Host.Spec(env.Route).StaticModels {
			note, ok := notes[id]
			if !ok && strings.HasSuffix(id, ":free") {
				note = "free"
			}
			rows = append(rows, models.ManifestModel{ID: id, Description: note})
		}
	}
	fallback := models.Catalog{Fallback: true, Notes: map[string]string{}}
	for _, row := range rows {
		fallback.IDs = append(fallback.IDs, row.ID)
		if row.Description != "" {
			fallback.Notes[row.ID] = row.Description
		}
	}
	live, err := models.Request(ctx, "GET", openrouterconfig.ModelsURL, map[string]string{"Accept": "application/json"}, nil, 8*time.Second)
	if err != nil {
		if ok {
			return held.catalog, true
		}
		return fallback, true
	}
	served := map[string]map[string]any{}
	items, _ := live["data"].([]any)
	for _, item := range items {
		if model, ok := item.(map[string]any); ok {
			if id, _ := model["id"].(string); strings.TrimSpace(id) != "" {
				served[strings.TrimSpace(id)] = model
			}
		}
	}
	silent := env.Host.ManifestDefault(env.Config, openrouterconfig.Name)
	if silent == "" {
		silent = env.Host.Spec(env.Route).CatalogDefault
	}
	curated := models.Catalog{Notes: map[string]string{}}
	for _, row := range rows {
		model, listed := served[row.ID]
		if !listed || !tools(model) {
			continue
		}
		curated.IDs = append(curated.IDs, row.ID)
		switch {
		case row.ID == silent:
			curated.Notes[row.ID] = "default"
		case free(model["pricing"]):
			curated.Notes[row.ID] = "free"
		}
	}
	if len(curated.IDs) == 0 {
		if ok {
			return held.catalog, true
		}
		return fallback, true
	}
	if curated.Notes[curated.IDs[0]] == "" {
		curated.Notes[curated.IDs[0]] = "recommended"
	}
	c.remember(path, curated, time.Now())
	saveCurated(path, curated)
	return curated, true
}

func (c *catalog) remember(path string, catalog models.Catalog, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.curated[path] = picked{catalog, at}
}

// Words shown beside models of the curated list when the online catalog can't be read
var notes = map[string]string{
	"anthropic/claude-opus-5-fast":      "2x price, higher output speed",
	"anthropic/claude-opus-4.8-fast":    "2x price, higher output speed",
	"deepseek/deepseek-v4-pro-0813":     "dated snapshot of v4-pro",
	"deepseek/deepseek-v4.1-flash-1017": "dated snapshot of v4.1-flash",
	"deepseek/deepseek-v4-flash-0731":   "dated snapshot of v4-flash",
	"moonshotai/kimi-k3":                "recommended",
	"z-ai/glm-5.3":                      "default",
	"z-ai/glm-5.3-flashx":               "high-speed tier of glm-5.3-flash",
	"openrouter/pareto-code":            "auto-routes to cheapest coder meeting openrouter.min_coding_score",
	"openai/gpt-6-astra-fast":           "2x price, priority tier",
	"openai/gpt-6-astra-flex":           "0.5x price, flex tier",
	"openai/gpt-6-astra-pro-fast":       "2x price, priority tier",
	"openai/gpt-6-astra-pro-flex":       "0.5x price, flex tier",
	"stealth/union-alpha":               "free, stealth model",
	"stealth/space-bunny-alpha":         "free, stealth model",
}

// Whether a listed model takes tools: it says so, or says nothing of what it takes
func tools(model map[string]any) bool {
	parameters, ok := model["supported_parameters"].([]any)
	if !ok {
		return true
	}
	for _, parameter := range parameters {
		if parameter == "tools" {
			return true
		}
	}
	return false
}

// Whether both a listed model's prompt and completion cost nothing
func free(pricing any) bool {
	listed, ok := pricing.(map[string]any)
	if !ok {
		return false
	}
	for _, key := range []string{"prompt", "completion"} {
		value, present := listed[key]
		if !present {
			value = "0"
		}
		if value == nil {
			return false
		}
		if n, err := strconv.ParseFloat(strings.TrimSpace(models.Scalar(value)), 64); err != nil || n != 0 {
			return false
		}
	}
	return true
}

func readCurated(path string) (models.Catalog, time.Time, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return models.Catalog{}, time.Time{}, false
	}
	var kept struct {
		At      float64    `json:"fetched_at"`
		Curated [][]string `json:"curated"`
	}
	if json.Unmarshal(raw, &kept) != nil || len(kept.Curated) == 0 {
		return models.Catalog{}, time.Time{}, false
	}
	catalog := models.Catalog{Notes: map[string]string{}}
	for _, pair := range kept.Curated {
		if len(pair) == 2 {
			catalog.IDs = append(catalog.IDs, pair[0])
			if pair[1] != "" {
				catalog.Notes[pair[0]] = pair[1]
			}
		}
	}
	return catalog, time.Unix(0, int64(kept.At*1e9)), true
}

func saveCurated(path string, catalog models.Catalog) {
	rows := make([][]string, 0, len(catalog.IDs))
	for _, id := range catalog.IDs {
		rows = append(rows, []string{id, catalog.Notes[id]})
	}
	raw, err := json.MarshalIndent(map[string]any{"fetched_at": float64(time.Now().UnixNano()) / 1e9, "curated": rows}, "", "  ")
	if err == nil {
		err = fsx.Write(path, raw, 0o600)
	}
	if err != nil {
		slog.Debug("openrouter curated catalog disk write failed", "error", err)
	}
}

// Model families of the catalog that reason, guessed by their ids while the catalog can't be read
var reasoningFamilies = []string{"deepseek/", "anthropic/", "openai/", "x-ai/", "google/gemini-2", "google/gemma-4", "qwen/qwen3", "tencent/hy", "xiaomi/"}

// Whether the route's model takes reasoning, at which efforts and whether it can't be turned off, as OpenRouter's catalog lists; a guess
// by its family while the catalog can't be read, and nothing at Mistral's address
func (c *catalog) Thinking(ctx context.Context, env *models.Env) (provider.Thinking, bool) {
	if env.Route.Spec != openrouterconfig.Name {
		return provider.Thinking{}, false
	}
	if provider.BaseHostMatches(env.Route.BaseURL, "api.mistral.ai") {
		return provider.Thinking{Known: true}, true
	}
	r, ok := c.directory(ctx, env)[strings.TrimSpace(env.Route.Model)]
	if !ok || r.Parameters == nil {
		return provider.Thinking{Takes: provider.HasPrefix(strings.ToLower(env.Route.Model), reasoningFamilies...)}, true
	}
	return provider.Thinking{Takes: slices.Contains(r.Parameters, "reasoning"), Efforts: r.Efforts, Mandatory: r.Mandatory, Known: true}, true
}
