package provider

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

// Default bounded fast window length
const DefaultFastWindowSeconds = 60

// Service tiers that open a bounded window per user turn or per session start
var BoundedFastModes = map[string]bool{"auto": true, "cold": true}

// Tiers sent on every request of the session
var StaticTiers = map[string]bool{"priority": true, "ultrafast": true}

// Codex app server names for the wire tiers it accepts
var CodexTierWords = map[string]string{"priority": "fast"}

// Words that mean the normal tier
var NormalTierWords = map[string]bool{"": true, "normal": true, "default": true, "standard": true, "off": true, "none": true}

// User or config word to stored service tier
var ServiceTierWords = map[string]string{
	"fast": "priority", "priority": "priority", "on": "priority",
	"ultrafast": "ultrafast", "ultra": "ultrafast", "auto": "auto", "cold": "cold",
}

// Typed /fast shorthands to the canonical word
var FastArgAliases = map[string]string{"ultra": "ultrafast"}

// Fast mode rate limit headers whose zero value means no fast capacity for the model
var fastLimitHeaders = []string{"anthropic-fast-input-tokens-limit", "anthropic-fast-output-tokens-limit"}

// ParseServiceTier maps a user or config word to a stored tier, empty for normal and unknown words
func ParseServiceTier(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	if NormalTierWords[value] {
		return ""
	}
	return ServiceTierWords[value]
}

// ParseExactServiceTier pins normal for an empty word and rejects an unknown word instead of reading it as normal
func ParseExactServiceTier(raw string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if !NormalTierWords[value] {
		if _, ok := ServiceTierWords[value]; !ok {
			return "", fmt.Errorf("unknown service tier: %s", value)
		}
	}
	return ParseServiceTier(value), nil
}

// ServiceTierWord is the user facing word for a stored tier
func ServiceTierWord(tier string) string {
	switch tier {
	case "priority":
		return "fast"
	case "":
		return "normal"
	}
	return tier
}

// FastWindow holds a session's service tier, its bounded window, and the models that lack fast capacity
type FastWindow struct {
	mu sync.Mutex
	// Stored tier, empty for normal
	ServiceTier string
	// Window length for auto and cold
	AutoSeconds float64
	// Static per session request overrides the fast override is layered onto
	RequestOverrides map[string]any
	// Daybreak opt in forwarded to the Codex tier negotiation
	Daybreak    bool
	until       time.Time
	unavailable map[string]bool
	now         func() time.Time
}

// NewFastWindow returns a closed window for the tier with the default auto length
func NewFastWindow(serviceTier string, overrides map[string]any) *FastWindow {
	return &FastWindow{ServiceTier: serviceTier, AutoSeconds: DefaultFastWindowSeconds, RequestOverrides: overrides, unavailable: map[string]bool{}, now: time.Now}
}

func (f *FastWindow) clock() time.Time {
	if f.now == nil {
		return time.Now()
	}
	return f.now()
}

// BeginTurn opens the window at a user turn boundary, cold only when no history exists
func (f *FastWindow) BeginTurn(history []*state.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.until = time.Time{}
	if !BoundedFastModes[f.ServiceTier] {
		return
	}
	if f.ServiceTier == "cold" {
		for _, m := range history {
			if m != nil && (m.Role == "user" || m.Role == "assistant" || m.Role == "tool") {
				return
			}
		}
	}
	window := f.AutoSeconds
	if window < 0 {
		window = 0
	}
	f.until = f.clock().Add(time.Duration(window * float64(time.Second)))
}

// Open reports whether the bounded fast window is live
func (f *FastWindow) Open() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return BoundedFastModes[f.ServiceTier] && f.clock().Before(f.until)
}

// EffectiveRequestOverrides layers the fast override onto the static overrides while the window is open
func (f *FastWindow) EffectiveRequestOverrides(route Route, anthropicBaseURL string) map[string]any {
	f.mu.Lock()
	overrides := make(map[string]any, len(f.RequestOverrides)+1)
	for k, v := range f.RequestOverrides {
		overrides[k] = v
	}
	open := BoundedFastModes[f.ServiceTier] && f.clock().Before(f.until)
	unavailable := f.unavailable[route.Model]
	f.mu.Unlock()
	if open {
		baseURL := route.BaseURL
		if route.Mode == ModeAnthropicMessages && anthropicBaseURL != "" {
			baseURL = anthropicBaseURL
		}
		for k, v := range ResolveFastModeOverrides(route.Model, route.Provider, baseURL, "") {
			overrides[k] = v
		}
	}
	if _, has := overrides["speed"]; has && unavailable {
		delete(overrides, "speed")
	}
	NegotiateRequestTier(route, overrides)
	ApplyDaybreak(route, f.Daybreak, overrides)
	return overrides
}

// FastModeUnprovisioned reports a 429 on a speed fast request whose fast limit header is zero
func FastModeUnprovisioned(apiErr *APIError, extraBody map[string]any, headers http.Header) bool {
	if apiErr == nil || apiErr.StatusCode != 429 {
		return false
	}
	if speed, _ := extraBody["speed"].(string); speed != "fast" {
		return false
	}
	if headers == nil {
		return false
	}
	lowered := LowerHeaders(headers)
	for _, name := range fastLimitHeaders {
		if strings.TrimSpace(lowered[name]) == "0" {
			return true
		}
	}
	return false
}

// MarkFastModeUnavailable stops sending speed for the model this session, false when already marked
func (f *FastWindow) MarkFastModeUnavailable(model string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unavailable == nil {
		f.unavailable = map[string]bool{}
	}
	if model == "" || f.unavailable[model] {
		return false
	}
	f.unavailable[model] = true
	return true
}

// FastModeUnavailable reports whether the model was marked without fast capacity
func (f *FastWindow) FastModeUnavailable(model string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unavailable[model]
}
