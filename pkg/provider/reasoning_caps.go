package provider

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
)

// ReasoningCapabilities is one model's advertised reasoning support from an OpenRouter schema catalog
type ReasoningCapabilities struct {
	SupportsReasoning bool
	// Effort levels the route accepts, nil when the catalog lists none
	SupportedEfforts []string
	Mandatory        bool
}

func capabilitiesOf(caps reasoningCaps) ReasoningCapabilities {
	return ReasoningCapabilities{SupportsReasoning: caps.supports, SupportedEfforts: cloneStrings(caps.supportedEfforts), Mandatory: caps.mandatory}
}

// ReasoningCapsDiskTTL is how long a disk mirrored catalog counts as fresh
const ReasoningCapsDiskTTL = reasoningCapsDiskTTL

var reasoningCapsWarmWait sync.WaitGroup

// Background warm seam, tests may run the refresh inline or drop it
var reasoningCapsSpawn = func(refresh func()) {
	reasoningCapsWarmWait.Add(1)
	go func() {
		defer reasoningCapsWarmWait.Done()
		refresh()
	}()
}

// WaitReasoningCapsWarms blocks until every background capability refresh has finished
func WaitReasoningCapsWarms() { reasoningCapsWarmWait.Wait() }

// Runs a refresh in the background so the next call or process finds the mirror warm
func warmReasoningCapsAsync(refresh func()) { reasoningCapsSpawn(refresh) }

// Fetches one OpenRouter shaped catalog into the mirror, false when unreachable or empty
func fetchReasoningCapsCatalog(ctx context.Context, rawURL string, timeout time.Duration) bool {
	payload, err := modelsGetJSON(ctx, rawURL, timeout, pyhttpx.OrderedHeaderMap(pyhttpx.HeaderLine{Name: "Accept", Value: "application/json"}, pyhttpx.HeaderLine{Name: "User-Agent", Value: userAgent("icebath-cli")}), nil)
	if err != nil {
		return false
	}
	doc, _ := asMap(payload)
	return seedReasoningCaps(ctx, rawURL, doc["data"])
}

// Once per process guards of one catalog source
type capsSlots struct {
	catalog     *capsCatalog
	diskChecked bool
	warmStarted bool
}

// One catalog's slots plus how to name its URL
type capsSource struct {
	mu         sync.Mutex
	resolveURL func(ctx context.Context) string
	slot       *capsSlots
}

func newCapsSource(resolveURL func(ctx context.Context) string) *capsSource {
	return &capsSource{resolveURL: resolveURL, slot: &capsSlots{}}
}

func (s *capsSource) slots(context.Context) *capsSlots { return s.slot }

// The shared catalog for the resolved URL, named at most once per process
func (s *capsSource) catalog(ctx context.Context) *capsCatalog {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot := s.slots(ctx)
	if slot.catalog == nil {
		slot.catalog = catalogFor(s.resolveURL(ctx))
	}
	return slot.catalog
}

// Fetch plus cache of the source caps, false without poisoning the cache when unreachable
func (s *capsSource) fetchCaps(ctx context.Context, timeout time.Duration, force bool) (map[string]reasoningCaps, bool) {
	c := s.catalog(ctx)
	c.mu.Lock()
	cached, failedAt := c.cache, c.failedAt
	c.mu.Unlock()
	if cached != nil && !force {
		return cached, true
	}
	if !failedAt.IsZero() && modelsClock().Sub(failedAt) < time.Minute {
		return nil, false
	}
	if !fetchReasoningCapsCatalog(ctx, c.url, timeout) {
		c.mu.Lock()
		c.failedAt = modelsClock()
		c.mu.Unlock()
		return nil, false
	}
	c.mu.Lock()
	cached = c.cache
	c.mu.Unlock()
	return cached, cached != nil
}

// Cache only caps from memory else the disk mirror once per profile, a stale mirror queues a refresh
func (s *capsSource) cachedCaps(ctx context.Context) map[string]reasoningCaps {
	s.mu.Lock()
	slot := s.slots(ctx)
	if slot.catalog != nil {
		slot.catalog.mu.Lock()
		cache := slot.catalog.cache
		slot.catalog.mu.Unlock()
		if cache != nil {
			s.mu.Unlock()
			return cache
		}
	}
	if slot.diskChecked {
		s.mu.Unlock()
		return nil
	}
	slot.diskChecked = true
	if slot.catalog == nil {
		slot.catalog = catalogFor(s.resolveURL(ctx))
	}
	c := slot.catalog
	s.mu.Unlock()
	caps, age := c.loadDisk()
	if caps == nil {
		return nil
	}
	c.mu.Lock()
	c.cache = caps
	c.mu.Unlock()
	if age >= reasoningCapsDiskTTL {
		warmReasoningCapsAsync(func() { s.fetchCaps(ctx, 6*time.Second, true) })
	}
	return caps
}

// Tri state answer for one model, false when the catalog is unloaded or does not list it
func (s *capsSource) modelCaps(ctx context.Context, modelID string, timeout time.Duration, allowFetch bool) (ReasoningCapabilities, bool) {
	model := strings.TrimSpace(modelID)
	if model == "" {
		return ReasoningCapabilities{}, false
	}
	capsByID := s.cachedCaps(ctx)
	if capsByID == nil && allowFetch {
		capsByID, _ = s.fetchCaps(ctx, timeout, false)
	}
	if capsByID == nil {
		return ReasoningCapabilities{}, false
	}
	caps, ok := capsByID[model]
	if !ok || !caps.known {
		return ReasoningCapabilities{}, false
	}
	return capabilitiesOf(caps), true
}

// Starts one background warm per profile when nothing is cached
func (s *capsSource) warmAsync(ctx context.Context) {
	s.mu.Lock()
	started := s.slots(ctx).warmStarted
	s.mu.Unlock()
	if started || s.cachedCaps(ctx) != nil {
		return
	}
	s.mu.Lock()
	s.slots(ctx).warmStarted = true
	s.mu.Unlock()
	warmReasoningCapsAsync(func() { s.fetchCaps(ctx, 6*time.Second, true) })
}

var (
	openrouterCapsSource = newCapsSource(func(context.Context) string { return openRouterCatalogURL })
)

// RefreshReasoningCapsAsync forces a background refetch of a provider's capability catalog
func RefreshReasoningCapsAsync(ctx context.Context, provider string) {
	var src *capsSource
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openrouter":
		src = openrouterCapsSource
	default:
		return
	}
	warmReasoningCapsAsync(func() { src.fetchCaps(ctx, 6*time.Second, true) })
}

// OpenRouterModelReasoningCapabilities is the live catalog answer for an OpenRouter model, cache only by default
func OpenRouterModelReasoningCapabilities(ctx context.Context, modelID string, timeout time.Duration, allowFetch bool) (ReasoningCapabilities, bool) {
	if timeout <= 0 {
		timeout = 6 * time.Second
	}
	return openrouterCapsSource.modelCaps(ctx, modelID, timeout, allowFetch)
}

// WarmOpenRouterReasoningCapsAsync warms the OpenRouter capability cache in the background
func WarmOpenRouterReasoningCapsAsync(ctx context.Context) { openrouterCapsSource.warmAsync(ctx) }
