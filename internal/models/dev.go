package models

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	modelsconfig "github.com/nickheyer/icebath/internal/models/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Public registry of model details
const devURL = "https://models.dev/api.json"

const (
	// Age past which the registry is fetched again, still used meanwhile
	devTTL = 4 * time.Hour
	// Time a failed fetch is not tried again, and a stale copy is used before another try
	devRetry = 5 * time.Minute
	// Longest one fetch of the registry takes
	devDeadline = 15 * time.Second
)

// Copy of the models.dev registry a profile holds: what was read, when, and the fetch in flight
type registry struct {
	mu      sync.Mutex
	data    map[string]any
	fetched time.Time
	// No fetch before this, after one failed
	retry time.Time
	// A fetch runs, in the background or for a caller
	fetching bool
	// One fetch at a time
	fetch sync.Mutex
	// Values of model overrides already warned of as unusable
	warned map[string]bool
}

// Registry of the profile served from memory, then its disk copy of any age, then one fetch; a stale copy is used while a fetch in the
// background replaces it. Without the network only what is held or on disk is served
func (m *Models) registry(cfg *config.Config, network bool) map[string]any {
	s := m.of(cfg)
	r := &s.dev
	r.mu.Lock()
	if len(r.data) == 0 {
		if disk := s.loadDev(); len(disk) > 0 {
			r.data = disk
			if info, err := os.Stat(s.file(devCache)); err == nil && time.Since(info.ModTime()) >= 0 {
				r.fetched = info.ModTime()
			}
		}
	}
	data, fresh := r.data, time.Since(r.fetched) < devTTL
	if !network || (len(data) > 0 && fresh) || time.Now().Before(r.retry) {
		r.mu.Unlock()
		return data
	}
	if len(data) > 0 {
		// Stale: a five minute grace before the next try, and a fetch in the background
		if grace := time.Now().Add(devRetry - devTTL); grace.After(r.fetched) {
			r.fetched = grace
		}
		if !r.fetching {
			r.fetching = true
			go func() {
				r.fetch.Lock()
				defer r.fetch.Unlock()
				m.refreshDev(cfg, "background")
			}()
		}
		r.mu.Unlock()
		return data
	}
	r.fetching = true
	r.mu.Unlock()
	r.fetch.Lock()
	defer r.fetch.Unlock()
	r.mu.Lock()
	if len(r.data) > 0 {
		data := r.data
		r.fetching = false
		r.mu.Unlock()
		return data
	}
	r.mu.Unlock()
	return m.refreshDev(cfg, "foreground")
}

// Fetches the registry, asking only for a change when one is held, keeping the answer on disk and in memory; what is held after it
func (m *Models) refreshDev(cfg *config.Config, where string) map[string]any {
	s := m.of(cfg)
	r := &s.dev
	r.mu.Lock()
	held := r.data
	r.mu.Unlock()
	data, etag, err := fetchDev(cfg, s, len(held) > 0)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fetching = false
	switch {
	case err == nil:
		if err := writeJSON(s.file(devCache), data); err != nil {
			slog.Debug("Failed to save models.dev disk cache", "error", err)
		}
		if etag != "" {
			if err := fsx.Write(s.file(devTag), []byte(etag), 0o600); err != nil {
				slog.Debug("Failed to save models.dev ETag", "error", err)
			}
		}
		r.data, r.fetched, r.retry = data, time.Now(), time.Time{}
		count := 0
		for _, entry := range data {
			if p, ok := entry.(map[string]any); ok {
				models, _ := p["models"].(map[string]any)
				count += len(models)
			}
		}
		slog.Debug(fmt.Sprintf("Refreshed models.dev registry (%s): %d providers, %d total models", where, len(data), count))
	case errors.Is(err, errNotModified) && len(r.data) > 0:
		r.fetched, r.retry = time.Now(), time.Time{}
		slog.Debug(fmt.Sprintf("models.dev registry unchanged (304 Not Modified, %s); cache re-confirmed fresh", where))
	case errors.Is(err, errNotModified):
		os.Remove(s.file(devTag))
		r.retry = time.Now().Add(devRetry)
		slog.Warn(fmt.Sprintf("models.dev returned 304 but no cached registry is held (%s); cleared ETag sidecar, will refetch unconditionally", where))
	default:
		r.retry = time.Now().Add(devRetry)
		slog.Debug(fmt.Sprintf("models.dev refresh failed (%s); retry suppressed for %ds: %v", where, int(devRetry.Seconds()), err))
	}
	return r.data
}

const (
	devCache = "models_dev_cache.json"
	devTag   = "models_dev_cache.etag"
)

var errNotModified = errors.New("models.dev registry not modified")

// One download of the registry, conditional on the ETag kept when asked
func fetchDev(cfg *config.Config, s *state, conditional bool) (map[string]any, string, error) {
	target := cmp.Or(strings.TrimSpace(cfg.GetString(modelsconfig.DevURL)), devURL)
	headers := map[string]string{"Accept": "application/json", "User-Agent": provider.UserAgent("icebath-cli")}
	if conditional {
		if tag, err := os.ReadFile(s.file(devTag)); err == nil && strings.TrimSpace(string(tag)) != "" {
			headers["If-None-Match"] = strings.TrimSpace(strings.TrimPrefix(string(tag), "\xef\xbb\xbf"))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), devDeadline)
	defer cancel()
	reply, err := send(ctx, "GET", target, headers, nil, 10*time.Second, true)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", fmt.Errorf("models.dev fetch exceeded %s", devDeadline)
		}
		return nil, "", err
	}
	if reply.status == 304 {
		return nil, "", errNotModified
	}
	if reply.status >= 400 {
		return nil, "", fmt.Errorf("models.dev returned HTTP %d", reply.status)
	}
	data, ok := reply.object(), true
	if len(data) == 0 {
		ok = false
	}
	if !ok {
		return nil, "", errors.New("models.dev returned an empty or invalid registry")
	}
	return data, reply.header.Get("ETag"), nil
}

// Disk copy of the registry, a corrupt or empty one set aside so it is fetched again rather than served empty
func (s *state) loadDev() map[string]any {
	path := s.file(devCache)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	var data map[string]any
	if err == nil {
		err = json.Unmarshal([]byte(strings.TrimPrefix(string(raw), "\xef\xbb\xbf")), &data)
	}
	if err == nil && len(data) > 0 {
		return data
	}
	slog.Warn("models.dev disk cache is corrupt or empty; quarantining (will refetch from network)", "error", err)
	if err := os.Rename(path, path+".corrupt"); err != nil {
		slog.Debug("Could not quarantine corrupt models.dev cache", "error", err)
	}
	os.Remove(s.file(devTag))
	return nil
}

// models.dev provider id of the route: its kind's, or for an endpoint of its own the one its catalog_provider setting names, which may be
// a kind's name or a models.dev id the registry has
func (m *Models) devID(cfg *config.Config, route provider.Route) string {
	if spec, ok := m.providers.Spec(route.Spec); ok && spec.ModelsDevID != "" {
		return spec.ModelsDevID
	}
	instance, ok := m.providers.Instance(cfg, route.Provider)
	if !ok {
		return ""
	}
	named := strings.TrimSpace(instance.Settings.GetString(providerconfig.CatalogProvider))
	if named == "" {
		return ""
	}
	if spec, ok := m.providers.Spec(named); ok && spec.ModelsDevID != "" {
		return spec.ModelsDevID
	}
	for _, spec := range m.providers.Specs() {
		if spec.ModelsDevID == named {
			return named
		}
	}
	if _, ok := m.registry(cfg, false)[named]; ok {
		return named
	}
	s := m.of(cfg)
	s.dev.mu.Lock()
	first := !s.dev.warned["catalog:"+route.Provider+"\x00"+named]
	s.dev.warned["catalog:"+route.Provider+"\x00"+named] = true
	s.dev.mu.Unlock()
	if first {
		slog.Warn(fmt.Sprintf("providers.%s: catalog_provider %q is neither an Icebath provider id nor a models.dev id; ignoring", route.Provider, named))
	}
	return ""
}

// Models of the route's provider in the registry, nil when it has none
func (m *Models) devModels(cfg *config.Config, route provider.Route, network bool) map[string]any {
	id := m.devID(cfg, route)
	if id == "" {
		return nil
	}
	p, _ := m.registry(cfg, network)[id].(map[string]any)
	models, _ := p["models"].(map[string]any)
	return models
}

// Whether the route goes through OpenRouter, whose routing variants after a colon name no model of their own
func routed(route provider.Route, spec provider.Spec) bool {
	return provider.BaseHostMatches(cmp.Or(route.BaseURL, spec.BaseURL), "openrouter.ai")
}

// Entries of the registry the model may go by in order: exactly, ignoring case, as its cloud variant, then the base of an OpenRouter
// routing variant
func devEntries(models map[string]any, model string, variants bool, aggregator bool) []devMatch {
	names := []string{model}
	if variants {
		names = append(names, model+":cloud", model+"-cloud")
	}
	// A picker only 900k variant is its base model
	if base, ok := strings.CutSuffix(model, "-900k"); ok {
		names = append(names, base)
	}
	keys := slices.Sorted(maps.Keys(models))
	var out []devMatch
	for _, name := range names {
		if entry, ok := models[name].(map[string]any); ok {
			out = append(out, devMatch{name, entry})
		}
		for _, id := range keys {
			if strings.EqualFold(id, name) {
				if entry, ok := models[id].(map[string]any); ok {
					out = append(out, devMatch{id, entry})
				}
			}
		}
	}
	if base := variantBase(model); aggregator && base != "" {
		out = append(out, devEntries(models, base, variants, false)...)
	}
	return out
}

type devMatch struct {
	id    string
	entry map[string]any
}

// Routing variants OpenRouter takes after a colon, which name no model of their own
var routingVariants = []string{"nitro", "floor", "exacto", "online"}

// Model id without an OpenRouter routing variant, empty when it has none
func variantBase(model string) string {
	i := strings.LastIndex(model, ":")
	if i <= 0 || !slices.Contains(routingVariants, strings.ToLower(model[i+1:])) {
		return ""
	}
	return model[:i]
}

// Positive limit of an entry, such as its context or output, 0 when not known
func limit(entry map[string]any, key string) int {
	limits, _ := entry["limit"].(map[string]any)
	if n, ok := limits[key].(float64); ok && n > 0 {
		return int(n)
	}
	return 0
}

// What models.dev keeps of a model, the overrides of the profile and the kind's own declarations applied
type Info struct {
	ID               string
	Name             string
	Family           string
	Provider         string
	Reasoning        bool
	Tools            bool
	Attachment       bool
	Temperature      bool
	StructuredOutput bool
	OpenWeights      bool
	Input            []string
	Output           []string
	Context          int
	MaxOutput        int
	MaxInput         int
	// USD for a million tokens; nil cache prices are not known
	CostInput      float64
	CostOutput     float64
	CostCacheRead  *float64
	CostCacheWrite *float64
	Knowledge      string
	Released       string
	Status         string
}

// Whether either price of a token is known
func (i Info) Priced() bool {
	return i.CostInput > 0 || i.CostOutput > 0
}

// Whether the model takes images
func (i Info) Vision() bool {
	return i.Attachment || slices.Contains(i.Input, "image")
}

// Abilities of the model as a list, basic when it has none of note
func (i Info) Abilities() string {
	var out []string
	for _, flag := range []struct {
		on    bool
		label string
	}{{i.Reasoning, "reasoning"}, {i.Tools, "tools"}, {i.Vision(), "vision"}, {slices.Contains(i.Input, "pdf"), "PDF"},
		{slices.Contains(i.Input, "audio"), "audio"}, {i.StructuredOutput, "structured output"}, {i.OpenWeights, "open weights"}} {
		if flag.on {
			out = append(out, flag.label)
		}
	}
	if len(out) == 0 {
		return "basic"
	}
	return strings.Join(out, ", ")
}

// Details of the route's model from the registry as already held, the profile's overrides applied; false when nothing knows the model
func (m *Models) Info(cfg *config.Config, route provider.Route) (Info, bool) {
	id := m.devID(cfg, route)
	models := m.devModels(cfg, route, false)
	model := route.Model
	var entry map[string]any
	for _, match := range devEntries(models, route.Model, false, routed(route, m.Spec(route))) {
		model, entry = match.id, match.entry
		break
	}
	raw := m.patched(cfg, route, entry)
	if raw == nil {
		return Info{}, false
	}
	cost, _ := raw["cost"].(map[string]any)
	modalities, _ := raw["modalities"].(map[string]any)
	optional := func(key string) *float64 {
		if v, ok := cost[key].(float64); ok {
			return &v
		}
		return nil
	}
	name, _ := raw["name"].(string)
	info := Info{ID: model, Name: cmp.Or(name, model), Family: text(raw["family"]), Provider: id, Reasoning: config.Truthy(raw["reasoning"]),
		Tools: config.Truthy(raw["tool_call"]), Attachment: config.Truthy(raw["attachment"]), Temperature: config.Truthy(raw["temperature"]),
		StructuredOutput: config.Truthy(raw["structured_output"]), OpenWeights: config.Truthy(raw["open_weights"]), Input: texts(modalities["input"]),
		Output: texts(modalities["output"]), Context: limit(raw, "context"), MaxOutput: limit(raw, "output"), MaxInput: limit(raw, "input"),
		CostCacheRead: optional("cache_read"), CostCacheWrite: optional("cache_write"), Knowledge: text(raw["knowledge"]),
		Released: text(raw["release_date"]), Status: text(raw["status"])}
	info.CostInput, _ = cost["input"].(float64)
	info.CostOutput, _ = cost["output"].(float64)
	return info, true
}

// Abilities of a model as the catalog and overrides know them; nil abilities are not known
type Capabilities struct {
	Tools     bool
	Vision    *bool
	Reasoning *bool
	Context   int
	MaxOutput int
	Family    string
}

// Abilities of the route's model with overrides applied, the registry fetched when the network is allowed; false when nothing knows it
func (m *Models) Capabilities(cfg *config.Config, route provider.Route, network bool) (Capabilities, bool) {
	var entry map[string]any
	for _, match := range devEntries(m.devModels(cfg, route, network), route.Model, true, routed(route, m.Spec(route))) {
		entry = match.entry
		break
	}
	unknown := entry == nil && m.builtin(route) == nil
	raw := m.patched(cfg, route, entry)
	if raw == nil {
		return Capabilities{}, false
	}
	caps := Capabilities{Tools: config.Truthy(raw["tool_call"]), Context: limit(raw, "context"), MaxOutput: limit(raw, "output"), Family: text(raw["family"])}
	if caps.Context == 0 {
		caps.Context = 200_000
	}
	_, attachment := raw["attachment"]
	_, modalities := raw["modalities"]
	if !unknown || attachment || modalities {
		caps.Vision = new(sees(raw))
	}
	if _, reasoning := raw["reasoning"]; !unknown || reasoning {
		caps.Reasoning = new(config.Truthy(raw["reasoning"]))
	}
	return caps, true
}

// Whether an entry takes images: its input modalities when it lists them, else its attachment flag
func sees(entry map[string]any) bool {
	modalities, _ := entry["modalities"].(map[string]any)
	if input, ok := modalities["input"].([]any); ok {
		return slices.Contains(input, any("image"))
	}
	return config.Truthy(entry["attachment"])
}

// Context window of the route's model in the registry as held, explicit overrides first and the _default override filling what it misses
func (m *Models) devContext(cfg *config.Config, route provider.Route) (int, bool) {
	if n, ok := m.overrideInt(cfg, m.explicit(cfg, route), "context_window"); ok {
		return n, true
	}
	if n, ok := m.overrideInt(cfg, m.Spec(route).ModelCapabilities[route.Model], "context_window"); ok {
		return n, true
	}
	for _, match := range devEntries(m.devModels(cfg, route, false), route.Model, true, routed(route, m.Spec(route))) {
		if n := limit(match.entry, "context"); n > 0 {
			return n, true
		}
	}
	return m.overrideInt(cfg, m.fallback(cfg, route), "context_window")
}

// Ids not meant for agents: speech, embeddings, dated previews and image only models
var devNoise = regexp.MustCompile(`(?i)-tts\b|embedding|live-|-(preview|exp)-\d{2,4}[-_]|-image\b|-image-preview\b|-customtools\b`)

// Ids Google's catalog lists that the picker leaves out
var googleHidden = []string{"gemma-4-31b-it", "gemma-4-26b-it", "gemma-4-26b-a4b-it", "gemma-3-1b", "gemma-3-1b-it", "gemma-3-2b", "gemma-3-2b-it",
	"gemma-3-4b", "gemma-3-4b-it", "gemma-3-12b", "gemma-3-12b-it", "gemma-3-27b", "gemma-3-27b-it", "gemini-1.5-flash", "gemini-1.5-pro",
	"gemini-1.5-flash-8b", "gemini-2.0-flash", "gemini-2.0-flash-lite"}

// Ids of the route's provider in the registry, without those the picker leaves out
func (m *Models) DevIDs(cfg *config.Config, route provider.Route, network bool) []string {
	models := m.devModels(cfg, route, network)
	out := []string{}
	for _, id := range slices.Sorted(maps.Keys(models)) {
		if m.devID(cfg, route) != "google" || !slices.Contains(googleHidden, strings.ToLower(id)) {
			out = append(out, id)
		}
	}
	return out
}

// Ids of the route's provider in the registry that call tools, without the hidden and noise ones
func (m *Models) Agentic(cfg *config.Config, route provider.Route, network bool) []string {
	models := m.devModels(cfg, route, network)
	out := []string{}
	for _, id := range slices.Sorted(maps.Keys(models)) {
		entry, ok := models[id].(map[string]any)
		if !ok || m.devID(cfg, route) == "google" && slices.Contains(googleHidden, strings.ToLower(id)) || !config.Truthy(entry["tool_call"]) || devNoise.MatchString(id) {
			continue
		}
		out = append(out, id)
	}
	return out
}
