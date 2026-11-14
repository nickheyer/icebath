package portal

import (
	"encoding/json"
	"strings"
	"time"
	"unicode"
)

const (
	GaugeFull     = 10000
	ScopeAll      = "all"
	ScopeTopTier  = "top_tier"
	resetMaxAhead = 366 * 86400000

	UseCredits             = "use_credits"
	AddCredits             = "add_credits"
	AddCreditsAndKeepGoing = "add_credits_and_keep_going"
)

type WindowGauge struct {
	ID         string
	Scope      string
	Label      string
	UsedBP     int
	ResetsAtMS *int64
}

func (g WindowGauge) Full() bool { return g.UsedBP >= GaugeFull }

func (g WindowGauge) UsedFraction() float64 {
	return min(1.0, max(0.0, float64(g.UsedBP)/GaugeFull))
}

func PausedForPayment(reason string) bool { return reason == "payment_past_due" }

func wholeNumber(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	if err != nil || strings.ContainsAny(n.String(), ".eE") {
		return 0, false
	}
	return i, true
}

func ParseGauges(raw json.RawMessage) []WindowGauge {
	value, err := parseValue(raw)
	if err != nil {
		return nil
	}
	return parseGauges(value)
}

func parseGauges(value any) []WindowGauge {
	if text, ok := value.(string); ok {
		if strings.HasPrefix(text, string(rune(0xFEFF))) {
			return nil
		}
		parsed, err := parseValue([]byte(text))
		if err != nil {
			return nil
		}
		value = parsed
	}
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	var out []WindowGauge
	seen := map[string]bool{}
	for _, item := range items {
		entry, ok := item.(*object)
		if !ok {
			continue
		}
		id, scope := entry.str("id"), entry.str("scope")
		if id == "" || seen[id] || (scope != ScopeAll && scope != ScopeTopTier) {
			continue
		}
		bp, ok := wholeNumber(entry.get("used_bp"))
		if !ok || bp < 0 || bp > GaugeFull {
			continue
		}
		gauge := WindowGauge{ID: id, Scope: scope, Label: id, UsedBP: int(bp)}
		if reset, ok := wholeNumber(entry.get("resets_at_ms")); ok && reset > 0 && reset <= time.Now().UnixMilli()+resetMaxAhead {
			gauge.ResetsAtMS = &reset
		}
		if label := strings.TrimFunc(entry.str("label"), func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }); label != "" {
			gauge.Label = label
		}
		seen[id] = true
		out = append(out, gauge)
	}
	return out
}

func AllScope(gauges []WindowGauge) []WindowGauge {
	var out []WindowGauge
	for _, g := range gauges {
		if g.Scope == ScopeAll {
			out = append(out, g)
		}
	}
	return out
}

func TopTierGauge(gauges []WindowGauge) (WindowGauge, bool) {
	for _, g := range gauges {
		if g.Scope == ScopeTopTier {
			return g, true
		}
	}
	return WindowGauge{}, false
}

func BindingGauge(gauges []WindowGauge) (WindowGauge, bool) {
	all := AllScope(gauges)
	if len(all) == 0 {
		return WindowGauge{}, false
	}
	resetOf := func(g WindowGauge) int64 {
		if g.ResetsAtMS == nil {
			return 0
		}
		return *g.ResetsAtMS
	}
	var best *WindowGauge
	for i := range all {
		if all[i].Full() && (best == nil || resetOf(all[i]) > resetOf(*best)) {
			best = &all[i]
		}
	}
	if best != nil {
		return *best, true
	}
	best = &all[0]
	for i := range all {
		if all[i].UsedBP > best.UsedBP {
			best = &all[i]
		}
	}
	return *best, true
}

func gaugeWords(id string) ([4]string, bool) {
	switch id {
	case "day":
		return [4]string{"Today", "today", "daily", "today"}, true
	case "week":
		return [4]string{"This week", "this week", "weekly", "week"}, true
	case "month":
		return [4]string{"This month", "this month", "monthly", "month"}, true
	case "top_tier":
		return [4]string{"Large models (monthly)", "your monthly large-model usage", "monthly large-model", "large models (monthly)"}, true
	}
	return [4]string{}, false
}

func GaugeTitle(g WindowGauge) string {
	if w, ok := gaugeWords(g.ID); ok {
		return w[0]
	}
	return g.Label
}

func GaugeLimitName(g WindowGauge) string {
	if w, ok := gaugeWords(g.ID); ok {
		return w[2]
	}
	return g.Label
}

func GaugeShort(g WindowGauge) string {
	if w, ok := gaugeWords(g.ID); ok {
		return w[3]
	}
	return g.Label
}

func CreditsAction(keepGoingOnCredits *bool, purchasedMicros int64) string {
	if keepGoingOnCredits != nil && !*keepGoingOnCredits {
		if purchasedMicros > 0 {
			return UseCredits
		}
		return AddCreditsAndKeepGoing
	}
	return AddCredits
}
