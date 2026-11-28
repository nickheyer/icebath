package models

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Structured catalog types that mean the row is not a chat completion
var generationTypes = map[string]bool{
	"image": true, "image_generation": true, "image-generation": true, "image-gen": true,
	"video": true, "video_generation": true, "video-generation": true, "video-gen": true,
}

var generationOutputs = map[string]bool{"image": true, "video": true}

var chatOutputs = map[string]bool{"text": true, "chat": true}

// Separator qualified generation tokens, vision and vl stay chat
var generationIDRe = regexp2.MustCompile(
	`(?i)(?:^|[/_.-])(?:image(?:[/_.-]gen(?:eration)?)?|video(?:[/_.-]gen(?:eration)?)?|text[/_.-]to[/_.-](?:image|video)|stable-diffusion|sdxl|flux)(?=$|[/_.-])`,
	regexp2.None)

var (
	seenGenerationMu  sync.Mutex
	seenGenerationIDs = map[string]bool{}
)

// NoteGenerationModel remembers an id a live catalog row proved to be generation only
func NoteGenerationModel(modelID any) {
	key := strings.ToLower(strings.TrimSpace(stringOf(modelID)))
	if key == "" {
		return
	}
	seenGenerationMu.Lock()
	seenGenerationIDs[key] = true
	seenGenerationMu.Unlock()
}

// ModelIDIsGeneration reports whether the id names an image or video generation surface
func ModelIDIsGeneration(modelID any) bool {
	m, _ := generationIDRe.MatchString(strings.TrimSpace(stringOf(modelID)))
	return m
}

func tagListIsGeneration(tags any) bool {
	list, ok := tags.([]any)
	if !ok {
		return false
	}
	for _, tag := range list {
		if generationTypes[strings.ToLower(strings.TrimSpace(stringOf(tag)))] {
			return true
		}
	}
	return false
}

func outputModalitiesAreGeneration(outputs any) bool {
	list, ok := outputs.([]any)
	if !ok || len(list) == 0 {
		return false
	}
	kinds := map[string]bool{}
	for _, item := range list {
		if s := strings.ToLower(strings.TrimSpace(stringOf(item))); s != "" {
			kinds[s] = true
		}
	}
	hasGen, hasChat := false, false
	for k := range kinds {
		hasGen = hasGen || generationOutputs[k]
		hasChat = hasChat || chatOutputs[k]
	}
	return hasGen && !hasChat
}

// CatalogItemIsGeneration fails open unless the row's own type or name proves it is generation only
func CatalogItemIsGeneration(item any) bool {
	row, ok := item.(map[string]any)
	if !ok {
		return false
	}
	if ModelIDIsGeneration(row["id"]) {
		return true
	}
	if caps, ok := row["capabilities"].(map[string]any); ok {
		if generationTypes[strings.ToLower(strings.TrimSpace(stringOf(caps["type"])))] {
			return true
		}
		if tagListIsGeneration(caps["tags"]) {
			return true
		}
	}
	if topType, ok := row["type"].(string); ok && generationTypes[strings.ToLower(strings.TrimSpace(topType))] {
		return true
	}
	if tagListIsGeneration(row["tags"]) {
		return true
	}
	if arch, ok := row["architecture"].(map[string]any); ok && outputModalitiesAreGeneration(arch["output_modalities"]) {
		return true
	}
	if meta, ok := row["metadata"].(map[string]any); ok && tagListIsGeneration(meta["tags"]) {
		return true
	}
	return false
}

// NoteCatalogItem remembers a generation row and reports whether it must leave the chat catalog
func NoteCatalogItem(item any) bool {
	if !CatalogItemIsGeneration(item) {
		return false
	}
	if row, ok := item.(map[string]any); ok {
		NoteGenerationModel(row["id"])
	}
	return true
}

// IsKnownNonChatModel is true when the name shape or an already parsed catalog item says not chat
func IsKnownNonChatModel(model string) bool {
	mid := strings.TrimSpace(model)
	if ModelIDIsGeneration(mid) {
		return true
	}
	seenGenerationMu.Lock()
	defer seenGenerationMu.Unlock()
	return seenGenerationIDs[strings.ToLower(mid)]
}

// WithoutGenerationModels drops generation ids and returns the same slice when none were dropped
func WithoutGenerationModels(models []string) []string {
	if len(models) == 0 {
		return models
	}
	kept := make([]string, 0, len(models))
	for _, m := range models {
		if !IsKnownNonChatModel(m) {
			kept = append(kept, m)
		}
	}
	if len(kept) == len(models) {
		return models
	}
	return kept
}

// ChatCatalogIDs lists the model ids of a models payload minus generation rows
func ChatCatalogIDs(items any) []string {
	list, ok := items.([]any)
	if !ok {
		return []string{}
	}
	ids := []string{}
	for _, item := range list {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, has := row["id"]
		if !has {
			continue
		}
		if NoteCatalogItem(item) {
			continue
		}
		ids = append(ids, stringOf(id))
	}
	return ids
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// Models a provider offers: their ids, whether they stand in for a list it could not give, and words shown beside some
type Catalog struct {
	IDs      []string
	Fallback bool
	// Such as recommended, free or default, by model id
	Notes map[string]string
	// Why the provider gave no list of its own, nil when it gave one or was not asked
	Unlisted *Unlisted
}

const (
	catalogCache = "provider_models_cache.json"
	// Age past which a kept catalog is fetched again, still offered meanwhile up to a week; a stand in kept only a minute
	catalogTTL      = time.Hour
	catalogStaleMax = 7 * 24 * time.Hour
	catalogStandIn  = time.Minute
)

// Models the route's provider offers, kept on disk for the credential it was read with: a fresh copy as it is, a stale one while a refresh
// runs in the background, else read now. Refresh reads it now whatever is kept
func (m *Models) Catalog(ctx context.Context, cfg *config.Config, route provider.Route, refresh bool) Catalog {
	s := m.of(cfg)
	key, fp := route.Provider, fingerprint(route.APIKey+"\x00"+strings.TrimRight(route.BaseURL, "/"))
	kept, keptOK := s.keptCatalog(key, fp)
	if !refresh && keptOK {
		limit := catalogTTL
		if kept.Fallback {
			limit = catalogStandIn
		}
		age := time.Since(kept.at)
		if age < limit {
			return kept.Catalog
		}
		if !kept.Fallback && age < catalogStaleMax {
			s.background("catalog:"+key, func() {
				if live := m.live(context.WithoutCancel(ctx), cfg, route, true); len(live.IDs) > 0 {
					s.keepCatalog(key, fp, live)
				}
			})
			return kept.Catalog
		}
	}
	live := m.live(ctx, cfg, route, refresh)
	if len(live.IDs) == 0 {
		if keptOK {
			return kept.Catalog
		}
		return live
	}
	if live.Fallback && keptOK && !kept.Fallback {
		return kept.Catalog
	}
	s.keepCatalog(key, fp, live)
	return live
}

type keptCatalog struct {
	Catalog
	at time.Time
}

func (s *state) keptCatalog(key, fp string) (keptCatalog, bool) {
	entry, ok := readJSON(s.file(catalogCache))[key].(map[string]any)
	if !ok || entry["fp"] != fp {
		return keptCatalog{}, false
	}
	at, ok := entry["at"].(float64)
	ids := texts(entry["models"])
	if !ok || len(ids) == 0 {
		return keptCatalog{}, false
	}
	notes := map[string]string{}
	if raw, ok := entry["notes"].(map[string]any); ok {
		for id, note := range raw {
			notes[id] = text(note)
		}
	}
	return keptCatalog{Catalog{IDs: WithoutGenerationModels(ids), Fallback: config.Truthy(entry["fallback"]), Notes: notes},
		time.Unix(0, int64(at*1e9))}, true
}

func (s *state) keepCatalog(key, fp string, catalog Catalog) {
	s.catalogs.Lock()
	defer s.catalogs.Unlock()
	path := s.file(catalogCache)
	release, err := fsx.Lock(context.Background(), path+".lock", 5*time.Second)
	if err != nil {
		slog.Debug("Skipping model list cache update", "error", err)
		return
	}
	defer release()
	data := readJSON(path)
	if data == nil {
		data = map[string]any{}
	}
	entry := map[string]any{"fp": fp, "at": float64(time.Now().UnixNano()) / 1e9, "models": catalog.IDs}
	if catalog.Fallback {
		entry["fallback"] = true
	}
	if len(catalog.Notes) > 0 {
		entry["notes"] = catalog.Notes
	}
	data[key] = entry
	if err := writeJSON(path, data); err != nil {
		slog.Debug("Failed to save model list cache", "error", err)
	}
}

// Models the route's provider offers now: what its plugin makes of them, else what it lists merged with its curated models, else the
// curated ones with what models.dev knows of the kind and why the provider listed none, as its requests met it; generation models left out
func (m *Models) live(ctx context.Context, cfg *config.Config, route provider.Route, refresh bool) Catalog {
	spec := m.Spec(route)
	unlisted := &Unlisted{}
	ctx = noting(ctx, unlisted)
	if catalog, ok := m.Registry.curated(ctx, m.env(cfg, route, refresh)); ok {
		catalog.IDs = WithoutGenerationModels(catalog.IDs)
		if len(catalog.IDs) == 0 && unlisted.URL != "" {
			catalog.Unlisted = unlisted
		}
		return catalog
	}
	listed, ok := m.Registry.Models(ctx, route)
	if !ok || len(listed) == 0 {
		catalog := m.Curated(cfg, route)
		if unlisted.URL != "" {
			catalog.Unlisted = unlisted
		}
		return catalog
	}
	curated := spec.StaticModels
	if len(curated) == 0 {
		curated = spec.FallbackModels
	}
	ids := listed
	switch {
	case len(curated) == 0:
	case spec.LiveFirst:
		ids = Merge(listed, curated)
	default:
		ids = Merge(curated, listed)
	}
	return Catalog{IDs: WithoutGenerationModels(delisted(spec, ids))}
}

// Models the route's kind offers without asking its provider: its curated ones, after those models.dev knows when the registry describes
// the kind better, a stand in for a list it could not give
func (m *Models) Curated(cfg *config.Config, route provider.Route) Catalog {
	spec := m.Spec(route)
	ids := slices.Clone(spec.StaticModels)
	if len(ids) == 0 {
		ids = slices.Clone(spec.FallbackModels)
	}
	if spec.ModelsDevPreferred {
		ids = Merge(m.Agentic(cfg, route, true), ids)
	}
	return Catalog{IDs: WithoutGenerationModels(delisted(spec, ids)), Fallback: true}
}

// Ids of the first list as they are, then those of the second not already there, case aside
func Merge(first, second []string) []string {
	out := slices.Clone(first)
	seen := map[string]bool{}
	for _, id := range first {
		seen[strings.ToLower(id)] = true
	}
	for _, id := range second {
		if !seen[strings.ToLower(id)] {
			seen[strings.ToLower(id)] = true
			out = append(out, id)
		}
	}
	return out
}

// Ids without those the kind still lists but no longer serves
func delisted(spec provider.Spec, ids []string) []string {
	if len(spec.Delisted) == 0 {
		return ids
	}
	return slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return slices.Contains(spec.Delisted, strings.ToLower(id)) })
}

// Model a route of the kind lands on when none is picked: the catalog's default for a kind that names one, else the first it curates
func (m *Models) DefaultModel(cfg *config.Config, route provider.Route) string {
	if labeled := m.ManifestDefault(cfg, route.Spec); labeled != "" {
		return labeled
	}
	spec := m.Spec(route)
	if spec.CatalogDefault != "" {
		return spec.CatalogDefault
	}
	if len(spec.StaticModels) > 0 {
		return spec.StaticModels[0]
	}
	return ""
}
