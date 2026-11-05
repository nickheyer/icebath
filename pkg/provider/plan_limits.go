package provider

import (
	"context"
	"fmt"
	"math"
	"strings"
)

// Plan limit vocabulary shared by the picker, the limit card and the switch button
const (
	PlanProvider              = "nous"
	planContextHeadroomTokens = 8_000
	planMaxSuggestions        = 2
	NoteFree                  = "free"
	NoteIncluded              = "included"
	UseCredits                = "use_credits"
	AddCredits                = "add_credits"
	AddCreditsAndKeepGoing    = "add_credits_and_keep_going"
	portalKeepGoingPhrase     = "turn on Keep going in the Portal"
	portalAddCreditsPhrase    = "add credits in the Portal"
)

// ModelSuggestion is a plan model that still works past a limit
type ModelSuggestion struct {
	Model    string
	Label    string
	Note     string
	Provider string
}

// ToPayload is the wire form of a suggestion
func (s ModelSuggestion) ToPayload() map[string]string {
	provider := s.Provider
	if provider == "" {
		provider = PlanProvider
	}
	return map[string]string{"model": s.Model, "label": s.Label, "note": s.Note, "provider": provider}
}

// CreditsOption is the credits step past a limit
type CreditsOption struct{ Action string }

// ToPayload is the wire form of a credits option
func (o CreditsOption) ToPayload() map[string]string { return map[string]string{"action": o.Action} }

// LimitHit describes which allowance stopped the current model, when it resets and what to offer
type LimitHit struct {
	GaugeID      string
	Scope        string
	LimitName    string
	ResetsAtMs   *int64
	Message      string
	Suggestions  []ModelSuggestion
	Credits      *CreditsOption
	CurrentModel string
}

// ToPayload is the wire form of a limit hit carried on the turn result
func (h LimitHit) ToPayload() map[string]any {
	suggestions := make([]any, 0, len(h.Suggestions))
	for _, s := range h.Suggestions {
		suggestions = append(suggestions, s.ToPayload())
	}
	var credits any
	if h.Credits != nil {
		credits = h.Credits.ToPayload()
	}
	var resets any
	if h.ResetsAtMs != nil {
		resets = *h.ResetsAtMs
	}
	return map[string]any{
		"gauge_id": h.GaugeID, "scope": h.Scope, "limit_name": h.LimitName, "resets_at_ms": resets, "message": h.Message,
		"suggestions": suggestions, "credits": credits, "current_model": h.CurrentModel,
	}
}

// BlockingGauge is the full gauge that frees up last among those stopping a paid model, nil while room remains
func BlockingGauge(gauges []WindowGauge, free, topTier bool) *WindowGauge {
	if free {
		return nil
	}
	var best *WindowGauge
	for i := range gauges {
		g := &gauges[i]
		if !g.Full() || !(g.Scope == ScopeAll || (g.Scope == ScopeTopTier && topTier)) {
			continue
		}
		if best == nil || resetOrZero(g.ResetsAtMs) > resetOrZero(best.ResetsAtMs) {
			best = g
		}
	}
	return best
}

func resetOrZero(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// CurrentModelIsTopTier reads the capture model tier from the cached catalog, nil when the catalog has no row
func CurrentModelIsTopTier(c *CreditsCapture) *bool {
	c.mu.Lock()
	model, baseURL := c.Model, c.BaseURL
	c.mu.Unlock()
	return topTierMark(model, CachedPricing(baseURL))
}

func topTierMark(model string, pricing map[string]any) *bool {
	if _, listed := pricing[model]; !listed && !IsPlanFree(model, pricing) {
		return nil
	}
	mark := IsTopTier(model, pricing)
	return &mark
}

// IsBlocked reports a model a full gauge stops
func IsBlocked(model string, gauges []WindowGauge, pricing map[string]any) bool {
	return BlockingGauge(gauges, IsPlanFree(model, pricing), IsTopTier(model, pricing)) != nil
}

// Portal free picks this profile already holds in process or on disk, never fetched
func portalFreeModels(ctx context.Context) []string {
	base := resolveNousPortalURL(ctx)
	nousRecommendedMu.Lock()
	entry, ok := nousRecommendedCache[nousRecommendedKey{pricingProfileKey(ctx), base}]
	nousRecommendedMu.Unlock()
	payload := entry.data
	if !ok || len(payload) == 0 {
		payload = nil
		if disk, _, found := readNousRecommendedDisk(ctx, base); found {
			payload = disk
		}
	}
	entries, _ := payload["freeRecommendedModels"].([]any)
	var out []string
	for _, e := range entries {
		if name := extractModelName(e); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func fitsPlanContext(ctx context.Context, model string, neededTokens int, baseURL string) bool {
	if neededTokens <= 0 {
		return true
	}
	window := GetModelContextLength(ctx, ContextLengthRequest{Model: model, BaseURL: baseURL, Provider: PlanProvider})
	return window == 0 || window >= neededTokens+planContextHeadroomTokens
}

// SuggestModels lists recommended then Portal free models the full gauges leave open, never the current one
func SuggestModels(ctx context.Context, currentModel string, gauges []WindowGauge, pricing map[string]any, featured []string, neededTokens int, baseURL string, limit int) []ModelSuggestion {
	var pool []string
	seen := map[string]bool{}
	for _, m := range featured {
		if m != "" {
			pool = append(pool, m)
			seen[m] = true
		}
	}
	for _, m := range portalFreeModels(ctx) {
		if !seen[m] {
			pool = append(pool, m)
			seen[m] = true
		}
	}
	var out []ModelSuggestion
	for _, model := range pool {
		if model == currentModel || IsSubscriptionBilled(pricing[model]) {
			continue
		}
		if IsBlocked(model, gauges, pricing) || !fitsPlanContext(ctx, model, neededTokens, baseURL) {
			continue
		}
		note := NoteIncluded
		if IsPlanFree(model, pricing) {
			note = NoteFree
		}
		out = append(out, ModelSuggestion{Model: model, Label: PlanModelLabel(model, pricing), Note: note, Provider: PlanProvider})
		if len(out) >= limit {
			break
		}
	}
	return out
}

// CreditsAction is the credits step past a limit that every surface copy reads
func CreditsAction(keepGoingOnCredits *bool, purchasedMicros int64) string {
	if keepGoingOnCredits != nil && !*keepGoingOnCredits {
		if purchasedMicros > 0 {
			return UseCredits
		}
		return AddCreditsAndKeepGoing
	}
	return AddCredits
}

// CreditsOptionFor is how credits could carry on past the limit, nil while the plan is paused
func CreditsOptionFor(state *CreditsState) *CreditsOption {
	if state == nil || state.PausedReason != "" {
		return nil
	}
	return &CreditsOption{Action: CreditsAction(state.KeepGoingOnCredits, state.PurchasedMicros)}
}

// LimitHitFor names the allowance that refused the current model with its reset and the ways on
func LimitHitFor(ctx context.Context, c *CreditsCapture, message string, neededTokens int) *LimitHit {
	c.mu.Lock()
	state, model, baseURL := c.credits, c.Model, c.BaseURL
	c.mu.Unlock()
	if state == nil || !state.IsUsageWindow() {
		return nil
	}
	pricing := CachedPricing(baseURL)
	gauges := state.Gauges
	mark := topTierMark(model, pricing)
	g := BlockingGauge(gauges, IsPlanFree(model, pricing), mark == nil || *mark)
	if g == nil {
		if top := TopTierGauge(gauges); top != nil && top.Full() {
			g = top
		}
	}
	if g == nil {
		return nil
	}
	var featured []string
	if usage := NousPickerUsage(false, true); usage != nil {
		featured = usage.FeaturedModelIDs
	}
	return &LimitHit{
		GaugeID: g.ID, Scope: g.Scope, LimitName: GaugeLimitName(*g), ResetsAtMs: g.ResetsAtMs,
		Message: message, CurrentModel: model, Credits: CreditsOptionFor(state),
		Suggestions: SuggestModels(ctx, model, gauges, pricing, featured, neededTokens, baseURL, planMaxSuggestions),
	}
}

// PortalWording points the credits steps of a refusal line at the Portal
func PortalWording(text string) string {
	text = strings.ReplaceAll(text, "turn on Keep going in /topup", portalKeepGoingPhrase)
	text = strings.ReplaceAll(text, "(turn on in /topup)", "(turn on in the Portal)")
	text = strings.ReplaceAll(text, "/topup to add credits", portalAddCreditsPhrase)
	return strings.ReplaceAll(text, "add credits in /topup", portalAddCreditsPhrase)
}

// ChatRefusalWording points credits at the Portal and reads the reset as a duration for a chat
func ChatRefusalWording(text string, resetsAt float64) string {
	text = PortalWording(text)
	if resetsAt > 0 && !math.IsInf(resetsAt, 0) && !math.IsNaN(resetsAt) {
		ms := int64(math.RoundToEven(resetsAt * 1000))
		if left := CompactDuration(ms, 0); left != "" {
			text = strings.ReplaceAll(text, "resets "+FormatWindowReset(ms, 0), "resets in "+left)
		}
	}
	return text
}

// SuggestionPhrase renders a suggestion as its label with the note in parentheses
func SuggestionPhrase(s ModelSuggestion) string { return fmt.Sprintf("%s (%s)", s.Label, s.Note) }

// NextStepsText is one line on how to keep going for a surface without buttons
func NextStepsText(hit LimitHit, modelCommand, topupCommand string, portal bool) string {
	var parts []string
	if len(hit.Suggestions) > 0 {
		parts = append(parts, fmt.Sprintf("Switch to %s with %s", SuggestionPhrase(hit.Suggestions[0]), modelCommand))
	} else {
		parts = append(parts, "Pick another model with "+modelCommand)
	}
	if hit.Credits != nil {
		where := topupCommand
		if portal || topupCommand == "" {
			where = "the Portal"
		}
		switch hit.Credits.Action {
		case UseCredits:
			parts = append(parts, fmt.Sprintf("turn on Keep going in %s to use credits", where))
		case AddCreditsAndKeepGoing:
			parts = append(parts, fmt.Sprintf("add credits and turn on Keep going in %s", where))
		default:
			prep := "with"
			if where == "the Portal" {
				prep = "in"
			}
			parts = append(parts, fmt.Sprintf("add credits %s %s", prep, where))
		}
	}
	return strings.Join(parts, ", or ") + "."
}

func payloadText(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyStr(v)
}

// LimitNextStepsFromPayload renders the next steps line from a limit hit payload, markup bolds the commands
func LimitNextStepsFromPayload(limit map[string]any, modelCommand, topupCommand string, portal, markup bool) string {
	var sug []ModelSuggestion
	rawSug, _ := limit["suggestions"].([]any)
	for _, raw := range rawSug {
		s, ok := raw.(map[string]any)
		if !ok || !pyTruthy(s["model"]) {
			continue
		}
		label := s["label"]
		if !pyTruthy(label) {
			label = s["model"]
		}
		note := s["note"]
		if !pyTruthy(note) {
			note = NoteIncluded
		}
		sug = append(sug, ModelSuggestion{Model: payloadText(s["model"]), Label: payloadText(label), Note: payloadText(note)})
	}
	var credits *CreditsOption
	if c, ok := limit["credits"].(map[string]any); ok && len(c) > 0 && pyTruthy(c["action"]) {
		credits = &CreditsOption{Action: payloadText(c["action"])}
	}
	hit := LimitHit{
		GaugeID: payloadText(orNil(limit["gauge_id"])), Scope: payloadText(orNil(limit["scope"])),
		LimitName: payloadText(orNil(limit["limit_name"])), Message: payloadText(orNil(limit["message"])),
		Suggestions: sug, Credits: credits,
	}
	if markup {
		modelCommand = "[bold]" + modelCommand + "[/]"
		if topupCommand != "" {
			topupCommand = "[bold]" + topupCommand + "[/]"
		}
	}
	return NextStepsText(hit, modelCommand, topupCommand, portal)
}

func orNil(v any) any {
	if !pyTruthy(v) {
		return nil
	}
	return v
}
