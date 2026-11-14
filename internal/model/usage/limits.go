package usage

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/catalog"
	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	NoteFree     = "free"
	NoteIncluded = "included"

	contextHeadroomTokens = 8_000
	maxSuggestions        = 2
)

type ModelSuggestion struct {
	Model    string `json:"model"`
	Label    string `json:"label"`
	Note     string `json:"note"`
	Provider string `json:"provider"`
}

type CreditsOption struct {
	Action string `json:"action"`
}

type LimitHit struct {
	GaugeID      string            `json:"gauge_id"`
	Scope        string            `json:"scope"`
	LimitName    string            `json:"limit_name"`
	ResetsAtMS   *int64            `json:"resets_at_ms"`
	Message      string            `json:"message"`
	Suggestions  []ModelSuggestion `json:"suggestions"`
	Credits      *CreditsOption    `json:"credits"`
	CurrentModel string            `json:"current_model"`
}

func (t *Tracker) cachedPricing(ctx context.Context) *catalog.PriceTable {
	cat := t.catalog()
	if cat == nil {
		return nil
	}
	return cat.PricingForProvider(ctx, providers.ProviderPortal, "", false, true)
}

func IsFree(model string, pricing *catalog.PriceTable) bool {
	if model == "" {
		return false
	}
	if strings.HasSuffix(model, ":free") {
		return true
	}
	return pricing.Len() > 0 && catalog.ModelIsFree(model, pricing)
}

func IsTopTier(model string, pricing *catalog.PriceTable) bool {
	return model != "" && UsageLevels([]string{model}, pricing)[model] == LevelTopTier
}

func ModelLabel(model string, pricing *catalog.PriceTable) string {
	if row, ok := pricing.Row(model); ok {
		if name := strings.TrimSpace(row.Name); name != "" {
			return name
		}
	}
	return model
}

func resetOrZero(g portal.WindowGauge) int64 {
	if g.ResetsAtMS == nil {
		return 0
	}
	return *g.ResetsAtMS
}

func BlockingGauge(gauges []portal.WindowGauge, free, topTier bool) (portal.WindowGauge, bool) {
	if free {
		return portal.WindowGauge{}, false
	}
	var best portal.WindowGauge
	found := false
	for _, g := range gauges {
		if !g.Full() || !(g.Scope == portal.ScopeAll || (g.Scope == portal.ScopeTopTier && topTier)) {
			continue
		}
		if !found || resetOrZero(g) > resetOrZero(best) {
			best, found = g, true
		}
	}
	return best, found
}

func topTierMark(model string, pricing *catalog.PriceTable) *bool {
	if _, listed := pricing.Row(model); !listed && !IsFree(model, pricing) {
		return nil
	}
	return new(IsTopTier(model, pricing))
}

func (t *Tracker) ModelIsTopTier(ctx context.Context, model string) *bool {
	return topTierMark(model, t.cachedPricing(t.bind(ctx)))
}

func IsBlocked(model string, gauges []portal.WindowGauge, pricing *catalog.PriceTable) bool {
	_, blocked := BlockingGauge(gauges, IsFree(model, pricing), IsTopTier(model, pricing))
	return blocked
}

func (t *Tracker) portalFreeModels(ctx context.Context) []string {
	base := auth.DefaultPortalURL
	if raw, ok := t.portal.State(ctx); ok {
		var state struct {
			PortalBaseURL string `json:"portal_base_url"`
		}
		if json.Unmarshal(raw, &state) == nil && strings.TrimSpace(state.PortalBaseURL) != "" {
			base = strings.TrimSpace(state.PortalBaseURL)
		}
	}
	base = strings.TrimRight(base, "/")
	var disk map[string]json.RawMessage
	if fileutil.ReadJSON(home.PortalRecommendedCache.In(t.home), &disk) != nil {
		return nil
	}
	entry, err := wire.ParseValue(disk[base])
	if err != nil || !entry.IsObject() {
		return nil
	}
	var names []string
	for _, item := range entry.Get("data").Get("freeRecommendedModels").Elems() {
		name, ok := item.Get("modelName").Str()
		if !ok || !item.IsObject() {
			continue
		}
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func (t *Tracker) fitsContext(ctx context.Context, model string, neededTokens int, baseURL string) bool {
	if neededTokens <= 0 {
		return true
	}
	cat := t.catalog()
	if cat == nil {
		return true
	}
	window, known := cat.ContextLength(ctx, catalog.ContextQuery{Model: model, BaseURL: baseURL, Provider: providers.ProviderPortal})
	return !known || window == 0 || window >= neededTokens+contextHeadroomTokens
}

func (t *Tracker) suggestModels(ctx context.Context, current string, gauges []portal.WindowGauge, pricing *catalog.PriceTable, featured []string, neededTokens int, baseURL string) []ModelSuggestion {
	var pool []string
	for _, id := range featured {
		if id != "" {
			pool = append(pool, id)
		}
	}
	head := len(pool)
	for _, id := range t.portalFreeModels(ctx) {
		if !containsString(pool[:head], id) {
			pool = append(pool, id)
		}
	}
	out := []ModelSuggestion{}
	for _, id := range pool {
		row, _ := pricing.Row(id)
		if id == current || row.Subscription {
			continue
		}
		if IsBlocked(id, gauges, pricing) || !t.fitsContext(ctx, id, neededTokens, baseURL) {
			continue
		}
		note := NoteIncluded
		if IsFree(id, pricing) {
			note = NoteFree
		}
		out = append(out, ModelSuggestion{Model: id, Label: ModelLabel(id, pricing), Note: note, Provider: providers.ProviderPortal})
		if len(out) >= maxSuggestions {
			break
		}
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func creditsOptionFor(s *CreditsState) *CreditsOption {
	if s == nil || s.PausedReason != "" {
		return nil
	}
	return &CreditsOption{Action: portal.CreditsAction(s.KeepGoingOnCredits, s.PurchasedMicros)}
}

type LimitQuery struct {
	State        *CreditsState
	Model        string
	BaseURL      string
	Message      string
	NeededTokens int
}

func (t *Tracker) LimitHit(ctx context.Context, q LimitQuery) *LimitHit {
	s := q.State
	if s == nil || !s.IsUsageWindow() {
		return nil
	}
	ctx = t.bind(ctx)
	pricing := t.cachedPricing(ctx)
	g, ok := BlockingGauge(s.Gauges, IsFree(q.Model, pricing), !isFalse(topTierMark(q.Model, pricing)))
	if !ok {
		if top, has := portal.TopTierGauge(s.Gauges); has && top.Full() {
			g, ok = top, true
		}
	}
	if !ok {
		return nil
	}
	var featured []string
	if plan, has := t.PickerUsage(ctx, catalog.PlanAnyAge); has {
		featured = plan.FeaturedModelIDs()
	}
	return &LimitHit{
		GaugeID: g.ID, Scope: g.Scope, LimitName: portal.GaugeLimitName(g), ResetsAtMS: g.ResetsAtMS, Message: q.Message,
		CurrentModel: q.Model, Credits: creditsOptionFor(s),
		Suggestions: t.suggestModels(ctx, q.Model, s.Gauges, pricing, featured, q.NeededTokens, q.BaseURL),
	}
}

func PortalWording(text string) string {
	for _, pair := range [...][2]string{
		{"turn on Keep going in /topup", "turn on Keep going in the Portal"},
		{"(turn on in /topup)", "(turn on in the Portal)"},
		{"/topup to add credits", "add credits in the Portal"},
		{"add credits in /topup", "add credits in the Portal"},
	} {
		text = strings.ReplaceAll(text, pair[0], pair[1])
	}
	return text
}

func ChatRefusalWording(text string, resetsAt time.Time) string {
	text = PortalWording(text)
	if resetsAt.IsZero() || resetsAt.UnixMilli() <= 0 {
		return text
	}
	ms := resetsAt.UnixMilli()
	now := time.Now()
	if left := compactDuration(&ms, now); left != "" {
		text = strings.ReplaceAll(text, "resets "+formatWindowReset(ms, now), "resets in "+left)
	}
	return text
}

func SuggestionPhrase(s ModelSuggestion) string { return s.Label + " (" + s.Note + ")" }

type NextSteps struct {
	ModelCommand string
	TopupCommand string
	Portal       bool
}

func DefaultNextSteps() NextSteps { return NextSteps{ModelCommand: "/model", TopupCommand: "/topup"} }

func NextStepsText(hit LimitHit, o NextSteps) string {
	var parts []string
	if len(hit.Suggestions) > 0 {
		parts = append(parts, "Switch to "+SuggestionPhrase(hit.Suggestions[0])+" with "+o.ModelCommand)
	} else {
		parts = append(parts, "Pick another model with "+o.ModelCommand)
	}
	if hit.Credits != nil {
		where := o.TopupCommand
		if o.Portal || o.TopupCommand == "" {
			where = "the Portal"
		}
		switch hit.Credits.Action {
		case portal.UseCredits:
			parts = append(parts, "turn on Keep going in "+where+" to use credits")
		case portal.AddCreditsAndKeepGoing:
			parts = append(parts, "add credits and turn on Keep going in "+where)
		default:
			preposition := "with"
			if where == "the Portal" {
				preposition = "in"
			}
			parts = append(parts, "add credits "+preposition+" "+where)
		}
	}
	return strings.Join(parts, ", or ") + "."
}

func LimitNextStepsFromPayload(raw json.RawMessage, o NextSteps, markup bool) (string, error) {
	var limit map[string]any
	if err := json.Unmarshal(raw, &limit); err != nil {
		return "", err
	}
	hit := LimitHit{}
	if items, ok := limit["suggestions"].([]any); ok {
		for _, item := range items {
			s, isObj := item.(map[string]any)
			if !isObj || falsy(s["model"]) {
				continue
			}
			label := s["label"]
			if falsy(label) {
				label = s["model"]
			}
			note := NoteIncluded
			if !falsy(s["note"]) {
				note = display(s["note"])
			}
			hit.Suggestions = append(hit.Suggestions, ModelSuggestion{Model: display(s["model"]), Label: display(label), Note: note, Provider: providers.ProviderPortal})
		}
	}
	if credits, ok := limit["credits"].(map[string]any); ok && !falsy(credits["action"]) {
		hit.Credits = &CreditsOption{Action: display(credits["action"])}
	}
	if markup {
		o.ModelCommand = "[bold]" + o.ModelCommand + "[/]"
		if o.TopupCommand != "" {
			o.TopupCommand = "[bold]" + o.TopupCommand + "[/]"
		}
	}
	return NextStepsText(hit, o), nil
}
