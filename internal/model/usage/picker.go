package usage

import (
	"context"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/model/catalog"
	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/providers"
)

const (
	barWidth = 10
	barFill  = "|"
	barEmpty = " "

	StyleGreen  = "green"
	StyleYellow = "yellow"
	StyleRed    = "red"

	LevelFree    = "free"
	LevelLow     = "low"
	LevelMedium  = "medium"
	LevelTopTier = "top_tier"

	TopTierFootnote    = "Large models also use your monthly large-model limit."
	SectionRecommended = "Recommended"
	SectionOther       = "Other models"

	gaugeToday   = "day"
	gaugeWeek    = "week"
	gaugeMonth   = "month"
	gaugeTopTier = "top_tier"

	planUsageKind = "usage_window"
)

func clampFraction(v float64) float64 {
	if v < 0 {
		return 0
	}
	return min(v, 1.0)
}

func gaugeStyle(left float64) string {
	left = clampFraction(left)
	switch {
	case left < 0.2:
		return StyleRed
	case left < 0.5:
		return StyleYellow
	}
	return StyleGreen
}

func filledCells(fraction float64, width int) int {
	value := clampFraction(fraction)
	filled := int(math.RoundToEven(value * float64(width)))
	if value > 0 && filled == 0 {
		filled = 1
	}
	if value < 1 && filled == width {
		filled = width - 1
	}
	return filled
}

func renderBar(fraction float64, width int) string {
	filled := filledCells(fraction, width)
	return "[" + strings.Repeat(barFill, filled) + strings.Repeat(barEmpty, width-filled) + "]"
}

func formatPercentUsed(used float64) string {
	bp := int(roundTo(clampFraction(used)*portal.GaugeFull, 6))
	switch {
	case bp <= 0:
		return "0%"
	case bp < 100:
		return "<1%"
	}
	return strconv.Itoa(bp/100) + "%"
}

func compactDuration(resetsAtMS *int64, now time.Time) string {
	if resetsAtMS == nil {
		return ""
	}
	delta := *resetsAtMS - now.UnixMilli()
	seconds := int64(0)
	if delta > 0 {
		seconds = delta / 1000
	}
	if seconds < 60 {
		return ""
	}
	days, rem := seconds/86_400, seconds%86_400
	hours, rem := rem/3_600, rem%3_600
	minutes := rem / 60
	if days != 0 {
		if hours != 0 {
			return strconv.FormatInt(days, 10) + "d " + strconv.FormatInt(hours, 10) + "h"
		}
		return strconv.FormatInt(days, 10) + "d"
	}
	if hours != 0 {
		return strconv.FormatInt(hours, 10) + "h"
	}
	return strconv.FormatInt(minutes, 10) + "m"
}

func formatResetsIn(resetsAtMS *int64, now time.Time) string {
	if d := compactDuration(resetsAtMS, now); d != "" {
		return "resets in " + d
	}
	return "resets soon"
}

type UsageGauge struct {
	Label        string
	LeftFraction float64
	ResetsAtMS   *int64
	Scope        string
	Title        string
}

func (g UsageGauge) UsedFraction() float64 { return 1.0 - clampFraction(g.LeftFraction) }

func (g UsageGauge) Style() string { return gaugeStyle(g.LeftFraction) }

func (g UsageGauge) Bar() string { return renderBar(g.UsedFraction(), barWidth) }

func (g UsageGauge) PercentUsed() string { return formatPercentUsed(g.UsedFraction()) }

type PickerUsage struct {
	Plan            string
	Gauges          []UsageGauge
	PausedReason    string
	Featured        []string
	TopTierIDs      map[string]bool
	FreeIDs         map[string]bool
	SubscriptionIDs map[string]bool
	CreditsPay      *bool
}

func (u *PickerUsage) FeaturedModelIDs() []string { return slices.Clone(u.Featured) }

func (u *PickerUsage) IsTopTier(modelID string) bool { return u.TopTierIDs[modelID] }

func (u *PickerUsage) WithPricing(pricing *catalog.PriceTable, modelIDs []string) *PickerUsage {
	ids := modelIDs
	if ids == nil {
		ids = pricing.IDs()
	}
	if len(ids) == 0 {
		return u
	}
	out := *u
	out.TopTierIDs, out.FreeIDs, out.SubscriptionIDs = map[string]bool{}, map[string]bool{}, map[string]bool{}
	for id, level := range UsageLevels(ids, pricing) {
		switch level {
		case LevelFree:
			out.FreeIDs[id] = true
		case LevelTopTier:
			out.TopTierIDs[id] = true
		}
	}
	for _, id := range ids {
		if row, ok := pricing.Row(id); ok && row.Subscription {
			out.SubscriptionIDs[id] = true
		}
	}
	return &out
}

func latestGauge(gauges []UsageGauge, keep func(UsageGauge) bool) (UsageGauge, bool) {
	var best UsageGauge
	found := false
	for _, g := range gauges {
		if !keep(g) {
			continue
		}
		if !found || resetOf(g.ResetsAtMS) > resetOf(best.ResetsAtMS) {
			best, found = g, true
		}
	}
	return best, found
}

func resetOf(ms *int64) int64 {
	if ms == nil {
		return 0
	}
	return *ms
}

func (u *PickerUsage) GaugesForModel(modelID string) []UsageGauge {
	top := u.IsTopTier(modelID)
	var out []UsageGauge
	for _, g := range u.Gauges {
		if g.Scope == portal.ScopeAll || (g.Scope == portal.ScopeTopTier && top) {
			out = append(out, g)
		}
	}
	return out
}

func (u *PickerUsage) ExhaustedGauge(modelID string) (UsageGauge, bool) {
	if u.FreeIDs[modelID] || strings.HasSuffix(modelID, ":free") {
		return UsageGauge{}, false
	}
	return latestGauge(u.GaugesForModel(modelID), func(g UsageGauge) bool { return g.LeftFraction <= 0 })
}

func (u *PickerUsage) Blocks(modelID string) bool {
	_, exhausted := u.ExhaustedGauge(modelID)
	return !isTrue(u.CreditsPay) && exhausted
}

func planText(tr Translate, key string, args map[string]string) (string, error) {
	if tr == nil {
		return "", &UnfilledError{Name: "translator"}
	}
	return tr("plan_usage."+key, args), nil
}

func resetsText(tr Translate, resetsAtMS *int64) (string, error) {
	if d := compactDuration(resetsAtMS, time.Now()); d != "" {
		return planText(tr, "resets_in", map[string]string{"duration": d})
	}
	return planText(tr, "resets_soon", nil)
}

func limitName(tr Translate, g UsageGauge) (string, error) {
	switch g.Label {
	case gaugeToday, gaugeWeek, gaugeMonth, gaugeTopTier:
		return planText(tr, "limit_"+g.Label, nil)
	}
	if g.Title != "" {
		return g.Title, nil
	}
	return g.Label, nil
}

func capitalize(text string) string {
	r, size := utf8.DecodeRuneInString(text)
	if size == 0 {
		return text
	}
	return string(unicode.ToUpper(r)) + text[size:]
}

var _ catalog.PlanUsage = (*Tracker)(nil)

func (u *PickerUsage) LimitLine(tr Translate) (string, error) {
	if tr == nil {
		return "", &UnfilledError{Name: "translator"}
	}
	if u.PausedReason != "" {
		return "", nil
	}
	full, isFull := latestGauge(u.Gauges, func(g UsageGauge) bool { return g.Scope == portal.ScopeAll && g.LeftFraction <= 0 })
	g, found := full, isFull
	if !isFull {
		for _, candidate := range u.Gauges {
			if candidate.Scope == gaugeTopTier && candidate.LeftFraction <= 0 {
				g, found = candidate, true
				break
			}
		}
	}
	if !found {
		return "", nil
	}
	name, err := limitName(tr, g)
	if err != nil {
		return "", err
	}
	resets, err := resetsText(tr, g.ResetsAtMS)
	if err != nil {
		return "", err
	}
	head, err := planText(tr, "limit_reached", map[string]string{"name": name, "resets": resets})
	if err != nil {
		return "", err
	}
	head = capitalize(head)
	large := "_large"
	if isFull {
		large = ""
	}
	switch {
	case isTrue(u.CreditsPay):
		tail, err := planText(tr, "then_credits"+large, nil)
		return head + " · " + tail, err
	case isFalse(u.CreditsPay):
		tail, err := planText(tr, "then_wait"+large, nil)
		return head + " · " + tail, err
	}
	return head, nil
}

func (u *PickerUsage) LargeModelNotice(modelID string, detail bool, tr Translate) (string, error) {
	if tr == nil {
		return "", &UnfilledError{Name: "translator"}
	}
	if detail && u.PausedReason == "" {
		if g, ok := u.ExhaustedGauge(modelID); ok {
			if isTrue(u.CreditsPay) {
				return planText(tr, "large_notice_credits", nil)
			}
			key := "large_notice_limit"
			if isFalse(u.CreditsPay) {
				key = "large_notice_limit_stops"
			}
			name, err := limitName(tr, g)
			if err != nil {
				return "", err
			}
			resets, err := resetsText(tr, g.ResetsAtMS)
			if err != nil {
				return "", err
			}
			return planText(tr, key, map[string]string{"name": name, "resets": resets})
		}
	}
	if detail {
		for _, g := range u.Gauges {
			if g.Scope == gaugeTopTier {
				return planText(tr, "large_notice_percent", map[string]string{"percent": g.PercentUsed()})
			}
		}
	}
	return planText(tr, "large_notice", nil)
}

func (u *PickerUsage) IncludedSummary(tr Translate) (string, error) {
	if u.PausedReason != "" {
		return "included usage paused", nil
	}
	line, err := u.LimitLine(tr)
	if err != nil || line != "" {
		return line, err
	}
	return "every model below is included", nil
}

type GaugePayload struct {
	Label        string  `json:"label"`
	LeftFraction float64 `json:"left_fraction"`
	Style        string  `json:"style"`
	ResetsAtMS   *int64  `json:"resets_at_ms"`
	Scope        string  `json:"scope"`
	Title        string  `json:"title"`
}

type PlanUsagePayload struct {
	Kind                 string         `json:"kind"`
	Plan                 *string        `json:"plan"`
	Gauges               []GaugePayload `json:"gauges"`
	TopTierModelIDs      []string       `json:"top_tier_model_ids"`
	SubscriptionModelIDs []string       `json:"subscription_model_ids"`
	FeaturedModelIDs     []string       `json:"featured_model_ids"`
	PausedReason         *string        `json:"paused_reason"`
	CreditsPay           *bool          `json:"credits_pay"`
}

func sortedKeys(set map[string]bool) []string {
	out := []string{}
	for k, v := range set {
		if v {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func optionalText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (u *PickerUsage) Payload() PlanUsagePayload {
	gauges := []GaugePayload{}
	for _, g := range u.Gauges {
		gauges = append(gauges, GaugePayload{
			Label: g.Label, LeftFraction: roundTo(g.LeftFraction, 4), Style: g.Style(), ResetsAtMS: g.ResetsAtMS, Scope: g.Scope, Title: g.Title,
		})
	}
	featured := append([]string{}, u.Featured...)
	return PlanUsagePayload{
		Kind: planUsageKind, Plan: optionalText(u.Plan), Gauges: gauges,
		TopTierModelIDs: sortedKeys(u.TopTierIDs), SubscriptionModelIDs: sortedKeys(u.SubscriptionIDs), FeaturedModelIDs: featured,
		PausedReason: optionalText(u.PausedReason), CreditsPay: u.CreditsPay,
	}
}

func UsageLevels(modelIDs []string, pricing *catalog.PriceTable) map[string]string {
	out := map[string]string{}
	for _, id := range modelIDs {
		row, _ := pricing.Row(id)
		switch {
		case IsFree(id, pricing):
			out[id] = LevelFree
		case row.UsageLevel == LevelFree || row.UsageLevel == LevelLow || row.UsageLevel == LevelMedium || row.UsageLevel == LevelTopTier:
			out[id] = row.UsageLevel
		case row.TopTier:
			out[id] = LevelTopTier
		}
	}
	return out
}

func UsageTagLevels(modelIDs []string, pricing *catalog.PriceTable) map[string]string {
	out := map[string]string{}
	for id, level := range UsageLevels(modelIDs, pricing) {
		if row, ok := pricing.Row(id); ok && row.Subscription {
			continue
		}
		out[id] = level
	}
	return out
}

func UsageLevelLabel(level string, tr Translate) (string, error) {
	if level != LevelFree && level != LevelTopTier {
		return "", nil
	}
	return planText(tr, "level_"+level, nil)
}

func pickerUsageFromAccount(info *portal.AccountInfo) *PickerUsage {
	if info == nil || !info.LoggedIn {
		return nil
	}
	window := info.UsageWindow
	if !windowSeedReady(window) {
		return nil
	}
	u := &PickerUsage{PausedReason: window.PausedReason, Featured: slices.Clone(window.FeaturedModelIDs)}
	if info.Subscription != nil {
		u.Plan = info.Subscription.Plan
	}
	for _, g := range window.Gauges {
		u.Gauges = append(u.Gauges, UsageGauge{Label: g.ID, LeftFraction: 1.0 - g.UsedFraction(), ResetsAtMS: g.ResetsAtMS, Scope: g.Scope, Title: g.Label})
	}
	switch {
	case isFalse(window.KeepGoingOnCredits):
		u.CreditsPay = new(false)
	case info.PaidServiceAccessInfo != nil && info.PaidServiceAccessInfo.PurchasedCreditsRemaining != nil:
		u.CreditsPay = new(*info.PaidServiceAccessInfo.PurchasedCreditsRemaining > 0)
	}
	return u
}

func (t *Tracker) PickerUsage(ctx context.Context, read catalog.PlanRead) (*PickerUsage, bool) {
	ctx = t.bind(ctx)
	if read == catalog.PlanWarm && t.portal.CachedAccountInfo(ctx, false) == nil {
		prior := t.portal.CachedAccountInfo(ctx, true)
		onWindow := prior != nil && windowSeedReady(prior.UsageWindow)
		t.portal.AccountInfo(ctx, portal.AccountOptions{ForceFresh: onWindow})
	}
	u := pickerUsageFromAccount(t.portal.CachedAccountInfo(ctx, read == catalog.PlanAnyAge))
	return u, u != nil
}

func (t *Tracker) Snapshot(ctx context.Context, read catalog.PlanRead) (catalog.PlanSnapshot, bool) {
	u, ok := t.PickerUsage(ctx, read)
	if !ok {
		return nil, false
	}
	return u, true
}

func (t *Tracker) WindowPlanDefault(ctx context.Context, modelIDs []string, pricing *catalog.PriceTable) string {
	u, ok := t.PickerUsage(ctx, catalog.PlanAnyAge)
	if !ok {
		return ""
	}
	recommended, _ := catalog.SplitRecommended(modelIDs, u.FeaturedModelIDs())
	for _, id := range recommended {
		row, _ := pricing.Row(id)
		if !row.TopTier && row.UsageLevel != LevelTopTier {
			return id
		}
	}
	return ""
}

func (t *Tracker) ModelSwitchNotice(ctx context.Context, model, provider string, fetch bool, tr Translate) (string, error) {
	if tr == nil {
		return "", &UnfilledError{Name: "translator"}
	}
	if strings.ToLower(strings.TrimSpace(provider)) != providers.ProviderPortal || model == "" {
		return "", nil
	}
	ctx = t.bind(ctx)
	fresh, _ := t.PickerUsage(ctx, catalog.PlanCached)
	u := fresh
	if u == nil {
		u, _ = t.PickerUsage(ctx, catalog.PlanAnyAge)
	}
	cat := t.catalog()
	if cat == nil {
		return "", &UnfilledError{Name: "catalog"}
	}
	rows := cat.PricingForProvider(ctx, providers.ProviderPortal, "", false, true)
	if rows.Len() == 0 && fetch && u != nil {
		rows = cat.PricingForProvider(ctx, providers.ProviderPortal, "", false, false)
	}
	if _, ok := rows.Row(model); !ok {
		return "", nil
	}
	if UsageLevels([]string{model}, rows)[model] != LevelTopTier {
		return "", nil
	}
	if u == nil {
		return planText(tr, "large_notice", nil)
	}
	return u.WithPricing(rows, []string{model}).LargeModelNotice(model, fresh != nil, tr)
}

func (t *Tracker) LargeModelNotice(ctx context.Context, model, provider string) string {
	notice, err := t.ModelSwitchNotice(ctx, model, provider, false, t.translate)
	if err != nil {
		return ""
	}
	return notice
}

func (t *Tracker) ModelNotes(ctx context.Context, modelIDs []string) (labels map[string]string, planLine string) {
	ctx = t.bind(ctx)
	fresh, _ := t.PickerUsage(ctx, catalog.PlanCached)
	u := fresh
	if u == nil {
		u, _ = t.PickerUsage(ctx, catalog.PlanAnyAge)
	}
	if u == nil {
		return nil, ""
	}
	labels = map[string]string{}
	for id, level := range UsageTagLevels(modelIDs, t.cachedPricing(ctx)) {
		if label, err := UsageLevelLabel(level, t.translate); err == nil && label != "" {
			labels[id] = label
		}
	}
	if fresh == nil {
		return labels, ""
	}
	line, err := fresh.LimitLine(t.translate)
	if err != nil {
		return labels, ""
	}
	return labels, line
}

func WindowRowLabels(modelIDs []string, levels map[string]string, subscribed []string, tr Translate) ([]string, error) {
	ids := slices.Clone(modelIDs)
	if levels == nil || len(ids) == 0 {
		return ids, nil
	}
	tags := make([]string, len(ids))
	tagged := false
	for i, id := range ids {
		level := levels[id]
		if slices.Contains(subscribed, id) {
			level = ""
		}
		tag, err := UsageLevelLabel(level, tr)
		if err != nil {
			return nil, err
		}
		tags[i] = tag
		tagged = tagged || tag != ""
	}
	if !tagged {
		return ids, nil
	}
	width := 0
	for _, id := range ids {
		width = max(width, runeLen(id))
	}
	width = min(width, 48)
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = strings.TrimRightFunc(padRight(id, width)+"  "+tags[i], unicode.IsSpace)
	}
	return out, nil
}

func (t *Tracker) AnnotateWindowPlanRows(ctx context.Context, slug string, modelIDs []string, levels map[string]string, subscribed []string) ([]string, string) {
	if strings.ToLower(slug) != providers.ProviderPortal {
		return slices.Clone(modelIDs), ""
	}
	u, _ := t.PickerUsage(ctx, catalog.PlanCached)
	labels, err := WindowRowLabels(modelIDs, levels, subscribed, t.translate)
	if err != nil {
		return slices.Clone(modelIDs), ""
	}
	if u == nil {
		return labels, ""
	}
	line, err := u.LimitLine(t.translate)
	if err != nil {
		return slices.Clone(modelIDs), ""
	}
	return labels, line
}

func gaugeName(g UsageGauge) string {
	title := g.Title
	if title == "" {
		title = g.Label
	}
	return portal.GaugeShort(portal.WindowGauge{ID: g.Label, Scope: g.Scope, Label: title})
}

func PlanGaugeLines(u *PickerUsage) []string {
	paused := ""
	if u.PausedReason != "" {
		paused = " (paused)"
		if portal.PausedForPayment(u.PausedReason) {
			paused = " (paused: renewal unpaid)"
		}
	}
	plan := u.Plan
	if plan == "" {
		plan = "icebath plan"
	}
	lines := []string{plan + paused}
	width := 4
	if len(u.Gauges) > 0 {
		width = 0
		for _, g := range u.Gauges {
			width = max(width, runeLen(gaugeName(g)))
		}
	}
	now := time.Now()
	for _, g := range u.Gauges {
		lines = append(lines, padRight(gaugeName(g), width)+"  "+g.Bar()+"  "+padRight(g.PercentUsed()+" used", 9)+"  "+formatResetsIn(g.ResetsAtMS, now))
	}
	return lines
}
