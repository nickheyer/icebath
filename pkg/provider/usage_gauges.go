package provider

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	BPFull       = 10_000
	ScopeAll     = "all"
	ScopeTopTier = "top_tier"
)

var gaugeScopes = map[string]bool{ScopeAll: true, ScopeTopTier: true}

// Pause reasons that mean the subscription payment failed, any other reason is still a pause
var paymentPauseReasons = map[string]bool{"payment_past_due": true}

// A reset more than a year ahead is a bad stamp and reads as unknown
const gaugeResetMaxAheadMs = 366 * 86_400_000

// WindowGauge is one usage window allowance as the server sends it
type WindowGauge struct {
	ID         string
	Scope      string
	Label      string
	UsedBp     int
	ResetsAtMs *int64
}

// Full reports an allowance used up to the whole basis point budget
func (g WindowGauge) Full() bool { return g.UsedBp >= BPFull }

// UsedFraction is the consumed share clamped to zero through one
func (g WindowGauge) UsedFraction() float64 {
	return math.Min(1.0, math.Max(0.0, float64(g.UsedBp)/BPFull))
}

// PausedForPayment reports a pause reason that means the subscription payment failed
func PausedForPayment(reason string) bool { return paymentPauseReasons[reason] }

// Gauge words for known ids as row title, limit name and picker short name
var gaugeWords = map[string][3]string{
	"day":      {"Today", "daily", "today"},
	"week":     {"This week", "weekly", "week"},
	"month":    {"This month", "monthly", "month"},
	"top_tier": {"Large models (monthly)", "monthly large-model", "large models (monthly)"},
}

// GaugeTitle is the usage row title, the server label for an unknown id
func GaugeTitle(g WindowGauge) string {
	if w, ok := gaugeWords[g.ID]; ok {
		return w[0]
	}
	return g.Label
}

// GaugeLimitName names the limit in a hit message, the server label for an unknown id
func GaugeLimitName(g WindowGauge) string {
	if w, ok := gaugeWords[g.ID]; ok {
		return w[1]
	}
	return g.Label
}

// GaugeShort is the picker summary row name, the server label for an unknown id
func GaugeShort(g WindowGauge) string {
	if w, ok := gaugeWords[g.ID]; ok {
		return w[2]
	}
	return g.Label
}

var errExactJSONTrailingData = errors.New("trailing data after the JSON value")

// Decodes JSON text keeping integer literals distinct from floats, error on trailing data
func decodeExactJSON(text string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errExactJSONTrailingData
	}
	return v, nil
}

// Exact integer of a decoded wire value, false for floats with a fraction, bools and non numbers
func gaugeInt(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := strconv.ParseInt(n.String(), 10, 64)
		return i, err == nil
	case float64:
		if math.IsInf(n, 0) || math.IsNaN(n) || n != math.Trunc(n) || math.Abs(n) > 1<<62 {
			return 0, false
		}
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	case int32:
		return int64(n), true
	}
	return 0, false
}

// ParseGauges turns the wire list or its header string into gauges in order, nil when absent unreadable or empty
func ParseGauges(value any) []WindowGauge {
	switch v := value.(type) {
	case []WindowGauge:
		if len(v) == 0 {
			return nil
		}
		return v
	case string:
		decoded, err := decodeExactJSON(v)
		if err != nil {
			return nil
		}
		value = decoded
	}
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	var out []WindowGauge
	seen := map[string]bool{}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		gid, _ := item["id"].(string)
		scope, _ := item["scope"].(string)
		if gid == "" || seen[gid] || !gaugeScopes[scope] {
			continue
		}
		bp, ok := gaugeInt(item["used_bp"])
		if !ok || bp < 0 || bp > BPFull {
			continue
		}
		label := gid
		if s, ok := item["label"].(string); ok && pyStripSpace(s) != "" {
			label = pyStripSpace(s)
		}
		seen[gid] = true
		out = append(out, WindowGauge{ID: gid, Scope: scope, Label: label, UsedBp: int(bp), ResetsAtMs: plausibleResetMs(item["resets_at_ms"])})
	}
	return out
}

// Reset stamp when positive and no more than a year ahead, nil otherwise
func plausibleResetMs(value any) *int64 {
	ms, ok := gaugeInt(value)
	if !ok || ms <= 0 || float64(ms) > float64(time.Now().UnixMilli())+gaugeResetMaxAheadMs {
		return nil
	}
	return &ms
}

// AllScope keeps the allowances that stop every paid model
func AllScope(gauges []WindowGauge) []WindowGauge {
	var out []WindowGauge
	for _, g := range gauges {
		if g.Scope == ScopeAll {
			out = append(out, g)
		}
	}
	return out
}

// TopTierGauge is the first allowance scoped to top tier models, nil without one
func TopTierGauge(gauges []WindowGauge) *WindowGauge {
	for _, g := range gauges {
		if g.Scope == ScopeTopTier {
			return &g
		}
	}
	return nil
}

// BindingGauge is the full all scope allowance freeing up last, else the fullest with ties to the first listed
func BindingGauge(gauges []WindowGauge) *WindowGauge {
	alls := AllScope(gauges)
	if len(alls) == 0 {
		return nil
	}
	var best *WindowGauge
	bestReset := int64(0)
	for i := range alls {
		if !alls[i].Full() {
			continue
		}
		reset := int64(0)
		if alls[i].ResetsAtMs != nil {
			reset = *alls[i].ResetsAtMs
		}
		if best == nil || reset > bestReset {
			best, bestReset = &alls[i], reset
		}
	}
	if best != nil {
		return best
	}
	best = &alls[0]
	for i := range alls[1:] {
		if alls[i+1].UsedBp > best.UsedBp {
			best = &alls[i+1]
		}
	}
	return best
}
