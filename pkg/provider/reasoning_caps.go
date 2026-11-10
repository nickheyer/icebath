package provider

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
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

// ParseOpenRouterReasoningCapabilities normalizes one catalog entry, false when supported_parameters is missing or malformed
func ParseOpenRouterReasoningCapabilities(item any) (ReasoningCapabilities, bool) {
	m, ok := asMap(item)
	if !ok {
		return ReasoningCapabilities{}, false
	}
	caps, known := parseReasoningCaps(m)
	if !known {
		return ReasoningCapabilities{}, false
	}
	return capabilitiesOf(caps), true
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
	payload, err := modelsGetJSON(ctx, rawURL, timeout, map[string]string{"Accept": "application/json", "User-Agent": userAgent("icebath-cli")}, nil)
	if err != nil {
		return false
	}
	doc, _ := asMap(payload)
	return seedReasoningCaps(ctx, rawURL, doc["data"])
}

// Once per process guards of one catalog source for one profile
type capsSlots struct {
	catalog     *capsCatalog
	diskChecked bool
	warmStarted bool
}

// One catalog's per profile slots plus how to name its URL
type capsSource struct {
	mu         sync.Mutex
	resolveURL func(ctx context.Context) string
	perProfile bool
	byKey      map[string]*capsSlots
}

func newCapsSource(resolveURL func(ctx context.Context) string, perProfile bool) *capsSource {
	return &capsSource{resolveURL: resolveURL, perProfile: perProfile, byKey: map[string]*capsSlots{}}
}

// Slot key, the module slot without a home override and the home key under one
func (s *capsSource) key(ctx context.Context) string {
	if !s.perProfile || home.Override(ctx) == "" {
		return ""
	}
	return home.Key(home.Dir(ctx))
}

func (s *capsSource) slots(ctx context.Context) *capsSlots {
	key := s.key(ctx)
	slot := s.byKey[key]
	if slot == nil {
		slot = &capsSlots{}
		s.byKey[key] = slot
	}
	return slot
}

// The shared catalog for the resolved URL, named at most once per profile
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

// Drops every profile's slots so the next read starts cold
func (s *capsSource) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byKey = map[string]*capsSlots{}
}

// NousCatalogURL is the Portal models URL for the endpoint the profile actually talks to
func NousCatalogURL(ctx context.Context) string {
	_, base := resolveNousPricingCredentials(ctx)
	return base + "/v1/models"
}

// Catalog URL seam for the Nous source, tests count how often the Portal is named
var nousCatalogURLFn = NousCatalogURL

var (
	openrouterCapsSource = newCapsSource(func(context.Context) string { return openRouterCatalogURL }, false)
	nousCapsSource       = newCapsSource(func(ctx context.Context) string { return nousCatalogURLFn(ctx) }, true)
)

// RefreshReasoningCapsAsync forces a background refetch of a provider's capability catalog
func RefreshReasoningCapsAsync(ctx context.Context, provider string) {
	var src *capsSource
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "nous", "nous-portal", "nousresearch":
		src = nousCapsSource
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

// NousModelReasoningCapabilities is the Portal counterpart of the OpenRouter lookup
func NousModelReasoningCapabilities(ctx context.Context, modelID string, timeout time.Duration, allowFetch bool) (ReasoningCapabilities, bool) {
	if timeout <= 0 {
		timeout = 6 * time.Second
	}
	return nousCapsSource.modelCaps(ctx, modelID, timeout, allowFetch)
}

// WarmOpenRouterReasoningCapsAsync warms the OpenRouter capability cache in the background
func WarmOpenRouterReasoningCapsAsync(ctx context.Context) { openrouterCapsSource.warmAsync(ctx) }

// WarmNousReasoningCapsAsync warms the Portal capability cache in the background
func WarmNousReasoningCapsAsync(ctx context.Context) { nousCapsSource.warmAsync(ctx) }

// ResetReasoningCapsCatalogs drops every in process capability catalog and source slot
func ResetReasoningCapsCatalogs() {
	openrouterCapsSource.reset()
	nousCapsSource.reset()
	capsCatalogsMu.Lock()
	defer capsCatalogsMu.Unlock()
	capsCatalogs = map[string]*capsCatalog{}
}
