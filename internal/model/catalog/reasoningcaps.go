package catalog

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	reasoningCapsDiskTTL   = 24 * time.Hour
	reasoningCapsTimeout   = 6 * time.Second
	reasoningCapsRetryWait = 60 * time.Second
	openRouterCatalogURL   = "https://openrouter.ai/api/v1/models"
)

type modelCaps map[string]*providers.ReasoningCaps

type capsSlot struct {
	cache       modelCaps
	failedAt    time.Time
	diskChecked bool
	warmStarted bool
}

type capsState struct {
	mu         sync.Mutex
	openRouter capsSlot
	portal     capsSlot
}

func parseReasoningCapabilities(item wire.Value) *providers.ReasoningCaps {
	if !item.IsObject() {
		return nil
	}
	params := item.Get("supported_parameters")
	if !params.IsArray() {
		return nil
	}
	advertised := slices.ContainsFunc(params.Elems(), func(v wire.Value) bool {
		text, ok := v.Str()
		return ok && text == "reasoning"
	})
	if !advertised {
		return &providers.ReasoningCaps{}
	}
	caps := &providers.ReasoningCaps{Supports: true}
	reasoning := item.Get("reasoning")
	if !reasoning.IsObject() {
		return caps
	}
	if raw := reasoning.Get("supported_efforts"); raw.IsArray() {
		caps.EffortsDeclared = true
		caps.Efforts = []string{}
		for _, entry := range raw.Elems() {
			effort := strings.ToLower(strings.TrimSpace(entry.Display()))
			if effort != "" && !slices.Contains(caps.Efforts, effort) {
				caps.Efforts = append(caps.Efforts, effort)
			}
		}
	}
	caps.Mandatory = reasoning.Get("mandatory").True()
	return caps
}

func encodeCaps(caps *providers.ReasoningCaps) any {
	if caps == nil {
		return nil
	}
	if !caps.Supports {
		return struct {
			Supports bool `json:"supports_reasoning"`
		}{}
	}
	var efforts []string
	if caps.EffortsDeclared {
		efforts = append([]string{}, caps.Efforts...)
	}
	return struct {
		Supports  bool     `json:"supports_reasoning"`
		Efforts   []string `json:"supported_efforts"`
		Mandatory bool     `json:"mandatory"`
	}{true, efforts, caps.Mandatory}
}

func decodeCaps(raw wire.Value) *providers.ReasoningCaps {
	if !raw.IsObject() {
		return nil
	}
	supports, ok := raw.Get("supports_reasoning").Bool()
	if !ok {
		return nil
	}
	caps := &providers.ReasoningCaps{Supports: supports}
	if !supports {
		return caps
	}
	if efforts := raw.Get("supported_efforts"); efforts.IsArray() {
		caps.EffortsDeclared = true
		caps.Efforts = []string{}
		for _, entry := range efforts.Elems() {
			if text, ok := entry.Str(); ok {
				caps.Efforts = append(caps.Efforts, text)
			}
		}
	}
	caps.Mandatory = raw.Get("mandatory").True()
	return caps
}

func (c *Catalog) reasoningCapsPath() string { return home.ReasoningCaps.In(c.home) }

func (c *Catalog) loadReasoningCapsDisk(catalogURL string) (modelCaps, time.Duration, bool) {
	raw, ok := readJSONObject(c.reasoningCapsPath())[catalogURL]
	if !ok {
		return nil, 0, false
	}
	entry, err := wire.ParseValue(raw)
	if err != nil || !entry.IsObject() {
		return nil, 0, false
	}
	stored := entry.Get("caps")
	if !stored.IsObject() || len(stored.Fields()) == 0 {
		return nil, 0, false
	}
	age := reasoningCapsDiskTTL
	stamp := entry.Get("ts")
	switch {
	case !stamp.Truthy():
		age = time.Duration(unixSeconds(time.Now()) * float64(time.Second))
	default:
		if seconds, ok := parseFloat(stamp.Display()); ok {
			age = max(0, time.Duration((unixSeconds(time.Now())-seconds)*float64(time.Second)))
		}
	}
	caps := modelCaps{}
	for _, field := range stored.Fields() {
		caps[field.Key] = decodeCaps(field.Value)
	}
	return caps, age, true
}

func (c *Catalog) saveReasoningCapsDisk(ctx context.Context, catalogURL string, caps modelCaps) {
	path := c.reasoningCapsPath()
	data := readJSONObject(path)
	encodedCaps := make(map[string]any, len(caps))
	for id, entry := range caps {
		encodedCaps[id] = encodeCaps(entry)
	}
	encoded, err := json.Marshal(struct {
		Stamp json.RawMessage `json:"ts"`
		Caps  map[string]any  `json:"caps"`
	}{json.RawMessage(strconv.FormatFloat(unixSeconds(time.Now()), 'f', -1, 64)), encodedCaps})
	if err != nil {
		slog.Debug("failed to save reasoning-caps disk cache", "error", err)
		return
	}
	data[catalogURL] = encoded
	if err := fileutil.WriteJSON(ctx, path, data, fileutil.JSONStyle{Lines: true, Tight: true}); err != nil {
		slog.Debug("failed to save reasoning-caps disk cache", "error", err)
	}
}

func (c *Catalog) seedReasoningCaps(ctx context.Context, catalogURL string, items []wire.Value) modelCaps {
	caps := modelCaps{}
	for _, item := range items {
		if !item.IsObject() {
			continue
		}
		if id := strings.TrimSpace(itemID(item)); id != "" {
			caps[id] = parseReasoningCapabilities(item)
		}
	}
	if len(caps) == 0 {
		return nil
	}
	c.saveReasoningCapsDisk(ctx, catalogURL, caps)
	return caps
}

func (c *Catalog) fetchReasoningCapsCatalog(ctx context.Context, catalogURL string, timeout time.Duration) modelCaps {
	header := http.Header{"Accept": {"application/json"}, "User-Agent": {c.userAgent()}}
	payload, err := c.getJSONObject(ctx, catalogURL, header, "", timeout)
	if err != nil {
		return nil
	}
	items, listed := dataItems(payload)
	if !listed {
		return nil
	}
	return c.seedReasoningCaps(ctx, catalogURL, items)
}

func (c *Catalog) capsSlot(portalSource bool) *capsSlot {
	if portalSource {
		return &c.st.caps.portal
	}
	return &c.st.caps.openRouter
}

func (c *Catalog) portalCatalogURL(ctx context.Context) string {
	_, base := c.portalPricingCredentials(ctx)
	return base + "/v1/models"
}

func (c *Catalog) capsURL(ctx context.Context, portalSource bool) string {
	if portalSource {
		return c.portalCatalogURL(ctx)
	}
	return openRouterCatalogURL
}

func (c *Catalog) fetchCaps(ctx context.Context, portalSource bool, timeout time.Duration, force bool) modelCaps {
	st, slot := &c.st.caps, c.capsSlot(portalSource)
	st.mu.Lock()
	cached, failedAt := slot.cache, slot.failedAt
	st.mu.Unlock()
	if cached != nil && !force {
		return cached
	}
	if !failedAt.IsZero() && time.Since(failedAt) < reasoningCapsRetryWait {
		return nil
	}
	fetched := c.fetchReasoningCapsCatalog(ctx, c.capsURL(ctx, portalSource), timeout)
	st.mu.Lock()
	defer st.mu.Unlock()
	if fetched == nil {
		slot.failedAt = time.Now()
		return nil
	}
	slot.cache = fetched
	return fetched
}

func (c *Catalog) warmCaps(ctx context.Context, portalSource bool) {
	c.background(ctx, func(bg context.Context) { c.fetchCaps(bg, portalSource, reasoningCapsTimeout, true) })
}

func (c *Catalog) capsCached(ctx context.Context, portalSource bool) modelCaps {
	st, slot := &c.st.caps, c.capsSlot(portalSource)
	st.mu.Lock()
	if slot.cache != nil || slot.diskChecked {
		cached := slot.cache
		st.mu.Unlock()
		return cached
	}
	slot.diskChecked = true
	st.mu.Unlock()
	caps, age, ok := c.loadReasoningCapsDisk(c.capsURL(ctx, portalSource))
	if !ok {
		return nil
	}
	if age >= reasoningCapsDiskTTL {
		c.warmCaps(ctx, portalSource)
	}
	st.mu.Lock()
	if slot.cache == nil {
		slot.cache = caps
	}
	caps = slot.cache
	st.mu.Unlock()
	return caps
}

func (c *Catalog) modelReasoningCaps(ctx context.Context, portalSource bool, modelID string, allowFetch bool) (providers.ReasoningCaps, bool) {
	model := strings.TrimSpace(modelID)
	if model == "" {
		return providers.ReasoningCaps{}, false
	}
	caps := c.capsCached(ctx, portalSource)
	if caps == nil && allowFetch {
		caps = c.fetchCaps(ctx, portalSource, reasoningCapsTimeout, false)
	}
	entry := caps[model]
	if entry == nil {
		return providers.ReasoningCaps{}, false
	}
	return providers.ReasoningCaps{Supports: entry.Supports, Mandatory: entry.Mandatory, Efforts: slices.Clone(entry.Efforts), EffortsDeclared: entry.EffortsDeclared}, true
}

func (c *Catalog) OpenRouterReasoningCaps(ctx context.Context, modelID string, allowFetch bool) (providers.ReasoningCaps, bool) {
	return c.modelReasoningCaps(c.scoped(ctx), false, modelID, allowFetch)
}

func (c *Catalog) PortalReasoningCaps(ctx context.Context, modelID string, allowFetch bool) (providers.ReasoningCaps, bool) {
	return c.modelReasoningCaps(c.scoped(ctx), true, modelID, allowFetch)
}

func (c *Catalog) warmCapsOnce(ctx context.Context, portalSource bool) {
	st, slot := &c.st.caps, c.capsSlot(portalSource)
	st.mu.Lock()
	started := slot.warmStarted
	st.mu.Unlock()
	if started || c.capsCached(ctx, portalSource) != nil {
		return
	}
	st.mu.Lock()
	started = slot.warmStarted
	slot.warmStarted = true
	st.mu.Unlock()
	if !started {
		c.warmCaps(ctx, portalSource)
	}
}

func (c *Catalog) WarmOpenRouterReasoningCaps(ctx context.Context) {
	c.warmCapsOnce(c.scoped(ctx), false)
}

func (c *Catalog) WarmPortalReasoningCaps(ctx context.Context) {
	c.warmCapsOnce(c.scoped(ctx), true)
}

func (c *Catalog) RefreshReasoningCaps(ctx context.Context, provider string) {
	ctx = c.scoped(ctx)
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providers.ProviderPortal, "icebath-portal":
		c.warmCaps(ctx, true)
	case "openrouter":
		c.warmCaps(ctx, false)
	}
}
