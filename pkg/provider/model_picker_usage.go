package provider

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	BarWidth           = 10
	BarFill            = "|"
	BarEmpty           = " "
	gaugeYellowBelow   = 0.5
	gaugeRedBelow      = 0.2
	TopTierFootnote    = "Large models also use your monthly large-model limit."
	SectionRecommended = "Recommended"
	SectionOther       = "Other models"
	GaugeToday         = "day"
	GaugeWeek          = "week"
	GaugeMonth         = "month"
	GaugeTopTier       = "top_tier"
)

// Levels a picker row names, Large for its own monthly limit and Free for never running out
var shownUsageLevels = map[string]bool{"free": true, "top_tier": true}

// Catalog text lookup of the i18n slice, swappable so tests supply the English catalog
var pickerTranslateFn = i18nT

// Clock the reset durations read, swappable so tests pin the time
var pickerNow = time.Now

// Account snapshot readers the picker usage goes through, swappable so tests script the cache
var (
	pickerCachedAccountFn = CachedNousPortalAccountInfo
	pickerFetchAccountFn  = GetNousPortalAccountInfo
	pickerPricingFn       = GetPricingForProvider
)

// UsageGauge is one server allowance as the share left and when it refills
type UsageGauge struct {
	Label        string
	LeftFraction float64
	ResetsAtMs   *int64
	Scope        string
	Title        string
}

// UsedFraction is the consumed share every surface draws
func (g UsageGauge) UsedFraction() float64 { return 1.0 - clampFraction(g.LeftFraction) }

// Style is green under half used, yellow from half, red past four fifths
func (g UsageGauge) Style() string { return gaugeStyle(g.LeftFraction) }

// Bar is the used share drawn as a ten cell bar
func (g UsageGauge) Bar() string { return RenderBar(g.UsedFraction(), BarWidth) }

// PercentUsed is the used share as a whole percent by the portal rule
func (g UsageGauge) PercentUsed() string { return FormatPercentUsed(g.UsedFraction()) }

// PickerUsage is what the pickers draw for a usage window plan
type PickerUsage struct {
	Plan             string
	Gauges           []UsageGauge
	PausedReason     string
	FeaturedModelIDs []string
	TopTierIDs       map[string]bool
	FreeIDs          map[string]bool
	SubscriptionIDs  map[string]bool
	CreditsPay       *bool
}

// Gauge is the allowance with this server id, nil when the plan has none
func (u *PickerUsage) Gauge(label string) *UsageGauge {
	for _, g := range u.Gauges {
		if g.Label == label {
			return &g
		}
	}
	return nil
}

// IsTopTier reports a model the gateway marked top tier, unmarked models are not
func (u *PickerUsage) IsTopTier(modelID string) bool { return u.TopTierIDs[modelID] }

// WithPricing is a copy carrying the top tier, free and subscription marks of the catalog rows
func (u *PickerUsage) WithPricing(pricing map[string]any) *PickerUsage {
	if len(pricing) == 0 {
		return u
	}
	ids := make([]string, 0, len(pricing))
	for m := range pricing {
		ids = append(ids, m)
	}
	cp := *u
	cp.TopTierIDs, cp.FreeIDs, cp.SubscriptionIDs = map[string]bool{}, map[string]bool{}, map[string]bool{}
	for m, level := range UsageLevels(ids, pricing) {
		switch level {
		case "free":
			cp.FreeIDs[m] = true
		case "top_tier":
			cp.TopTierIDs[m] = true
		}
	}
	for m, row := range pricing {
		if IsSubscriptionBilled(row) {
			cp.SubscriptionIDs[m] = true
		}
	}
	return &cp
}

// Spent gauge among the candidates that frees up last, the first of equal resets winning
func lastFreeingSpent(gauges []UsageGauge, keep func(UsageGauge) bool) *UsageGauge {
	var best *UsageGauge
	var bestKey int64
	for _, g := range gauges {
		if !keep(g) || g.LeftFraction > 0.0 {
			continue
		}
		key := int64(0)
		if g.ResetsAtMs != nil {
			key = *g.ResetsAtMs
		}
		if best == nil || key > bestKey {
			g := g
			best, bestKey = &g, key
		}
	}
	return best
}

// ExhaustedGauge is the spent allowance that stops a model, nil while it has room or the model is free
func (u *PickerUsage) ExhaustedGauge(modelID string) *UsageGauge {
	if u.FreeIDs[modelID] || strings.HasSuffix(modelID, ":free") {
		return nil
	}
	return lastFreeingSpent(u.GaugesForModel(modelID), func(UsageGauge) bool { return true })
}

// Blocks reports a full allowance stopping the model while top up credits do not pay
func (u *PickerUsage) Blocks(modelID string) bool {
	return !(u.CreditsPay != nil && *u.CreditsPay) && u.ExhaustedGauge(modelID) != nil
}

func (u *PickerUsage) fullAllGauge() *UsageGauge {
	return lastFreeingSpent(u.Gauges, func(g UsageGauge) bool { return g.Scope == ScopeAll })
}

// LimitLine says a full allowance and what happens next, empty while nothing is full or the plan is paused
func (u *PickerUsage) LimitLine(lang string) string {
	if u.PausedReason != "" {
		return ""
	}
	full := u.fullAllGauge()
	g := full
	if full == nil {
		for _, cand := range u.Gauges {
			if cand.Scope == GaugeTopTier && cand.LeftFraction <= 0.0 {
				g = &cand
				break
			}
		}
	}
	if g == nil {
		return ""
	}
	head := pyCapitalizeFirst(pickerT("limit_reached", lang, map[string]any{
		"name": pickerLimitName(*g, lang), "resets": pickerResets(g.ResetsAtMs, lang)}))
	large := ""
	if full == nil {
		large = "_large"
	}
	if u.CreditsPay != nil && *u.CreditsPay {
		return head + " · " + pickerT("then_credits"+large, lang, nil)
	}
	if u.CreditsPay != nil {
		return head + " · " + pickerT("then_wait"+large, lang, nil)
	}
	return head
}

// LargeModelNotice is the warning for a switch to a Large model, detail adds the allowance state
func (u *PickerUsage) LargeModelNotice(modelID string, detail bool, lang string) string {
	var g *UsageGauge
	if detail && u.PausedReason == "" {
		g = u.ExhaustedGauge(modelID)
	}
	if g != nil {
		if u.CreditsPay != nil && *u.CreditsPay {
			return pickerT("large_notice_credits", lang, nil)
		}
		key := "large_notice_limit"
		if u.CreditsPay != nil {
			key = "large_notice_limit_stops"
		}
		return pickerT(key, lang, map[string]any{"name": pickerLimitName(*g, lang), "resets": pickerResets(g.ResetsAtMs, lang)})
	}
	if detail {
		for _, top := range u.Gauges {
			if top.Scope == GaugeTopTier {
				return pickerT("large_notice_percent", lang, map[string]any{"percent": top.PercentUsed()})
			}
		}
	}
	return pickerT("large_notice", lang, nil)
}

// IncludedSummary is the header suffix, every row included unless an allowance is full or the plan paused
func (u *PickerUsage) IncludedSummary() string {
	if u.PausedReason != "" {
		return "included usage paused"
	}
	if line := u.LimitLine("en"); line != "" {
		return line
	}
	return "every model below is included"
}

// GaugesForModel is every all scope allowance plus the top tier ones when the model draws on them
func (u *PickerUsage) GaugesForModel(modelID string) []UsageGauge {
	top := u.IsTopTier(modelID)
	var out []UsageGauge
	for _, g := range u.Gauges {
		if g.Scope == ScopeAll || (g.Scope == ScopeTopTier && top) {
			out = append(out, g)
		}
	}
	return out
}

// ToPayload is the model options wire shape of the plan usage
func (u *PickerUsage) ToPayload() map[string]any {
	gauges := make([]any, 0, len(u.Gauges))
	for _, g := range u.Gauges {
		var resets any
		if g.ResetsAtMs != nil {
			resets = *g.ResetsAtMs
		}
		gauges = append(gauges, map[string]any{"label": g.Label, "left_fraction": pyRoundPlaces(g.LeftFraction, 4),
			"style": g.Style(), "resets_at_ms": resets, "scope": g.Scope, "title": g.Title})
	}
	featured := make([]any, 0, len(u.FeaturedModelIDs))
	for _, m := range u.FeaturedModelIDs {
		featured = append(featured, m)
	}
	return map[string]any{
		"kind": "usage_window", "plan": noneIfEmpty(u.Plan), "gauges": gauges,
		"top_tier_model_ids": sortedSetList(u.TopTierIDs), "subscription_model_ids": sortedSetList(u.SubscriptionIDs),
		"featured_model_ids": featured, "paused_reason": noneIfEmpty(u.PausedReason), "credits_pay": boolOrNone(u.CreditsPay),
	}
}

func noneIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolOrNone(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

func sortedSetList(set map[string]bool) []any {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]any, len(keys))
	for i, k := range keys {
		out[i] = k
	}
	return out
}

// Rounds to the given decimal places the way Python round does on the exact binary value
func pyRoundPlaces(v float64, places int) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return v
	}
	r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', places, 64), 64)
	return r
}

func clampFraction(v float64) float64 {
	if v < 0.0 {
		return 0.0
	}
	if 1.0 < v {
		return 1.0
	}
	return v
}

func gaugeStyle(leftFraction float64) string {
	left := clampFraction(leftFraction)
	if left < gaugeRedBelow {
		return "red"
	}
	if left < gaugeYellowBelow {
		return "yellow"
	}
	return "green"
}

func filledCells(fraction float64, width int) int {
	value := clampFraction(fraction)
	filled := int(math.RoundToEven(value * float64(width)))
	if value > 0.0 && filled == 0 {
		filled = 1
	}
	if value < 1.0 && filled == width {
		filled = width - 1
	}
	return filled
}

// RenderBar draws a bracketed bar whose filled cells show the fraction, never empty above zero or full below one
func RenderBar(fraction float64, width int) string {
	filled := filledCells(fraction, width)
	return "[" + strings.Repeat(BarFill, filled) + strings.Repeat(BarEmpty, width-filled) + "]"
}

// Plan featured picks first in plan order and only those listed, then the rest unchanged
func featuredFirst(ids, featured []string) []string {
	head, rest := splitRecommended(ids, featured)
	return append(head, rest...)
}

// Listed featured picks in plan order, then every other listed model in catalog order
func splitRecommended(ids, featured []string) ([]string, []string) {
	listed := make(map[string]bool, len(ids))
	for _, m := range ids {
		listed[m] = true
	}
	head := []string{}
	seen := map[string]bool{}
	for _, m := range featured {
		if listed[m] && !seen[m] {
			seen[m] = true
			head = append(head, m)
		}
	}
	rest := []string{}
	for _, m := range ids {
		if !seen[m] {
			rest = append(rest, m)
		}
	}
	return head, rest
}

// Usage levels a picker row shows a tag for, subscription billed rows left untagged
func usageTagLevels(ids []string, pricing map[string]any) map[string]string {
	out := map[string]string{}
	for m, level := range UsageLevels(ids, pricing) {
		if !IsSubscriptionBilled(pricing[m]) {
			out[m] = level
		}
	}
	return out
}

// IsFreeModel reports a catalog row priced at zero in and zero out, a row without pricing is not free
func IsFreeModel(row map[string]any) bool {
	if len(row) == 0 {
		return false
	}
	in, okIn := priceFloat(row["prompt"], false)
	out, okOut := priceFloat(row["completion"], false)
	return okIn && okOut && in <= 0.0 && out <= 0.0
}

// FormatPercentUsed floors the used share to a whole percent, a sliver reading below one percent
func FormatPercentUsed(usedFraction float64) string {
	bp := int64(pyRoundPlaces(clampFraction(usedFraction)*BPFull, 6))
	if bp <= 0 {
		return "0%"
	}
	if bp < 100 {
		return "<1%"
	}
	return fmt.Sprintf("%d%%", bp/100)
}

// Time until the reset as days and hours, hours, or minutes, empty when past, under a minute or unknown
func compactDurationAt(resetsAtMs, nowMs *int64) string {
	if resetsAtMs == nil {
		return ""
	}
	now := pickerNow().UnixMilli()
	if nowMs != nil {
		now = *nowMs
	}
	seconds := max(0, floorDivInt64(*resetsAtMs-now, 1000))
	if seconds < 60 {
		return ""
	}
	days, rem := seconds/86_400, seconds%86_400
	hours, rem := rem/3_600, rem%3_600
	minutes := rem / 60
	if days != 0 {
		if hours != 0 {
			return fmt.Sprintf("%dd %dh", days, hours)
		}
		return fmt.Sprintf("%dd", days)
	}
	if hours != 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dm", minutes)
}

func floorDivInt64(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// CompactDuration is the time until a reset in milliseconds, a zero now reading the clock
func CompactDuration(resetsAtMs, nowMs int64) string {
	if nowMs == 0 {
		return compactDurationAt(&resetsAtMs, nil)
	}
	return compactDurationAt(&resetsAtMs, &nowMs)
}

// FormatResetsIn reads the reset as resets in a duration, or resets soon when past or unknown
func FormatResetsIn(resetsAtMs, nowMs *int64) string {
	if d := compactDurationAt(resetsAtMs, nowMs); d != "" {
		return "resets in " + d
	}
	return "resets soon"
}

// Plan usage catalog text in the display language or the given one
func pickerT(key, lang string, kwargs map[string]any) string {
	return pickerTranslateFn("plan_usage."+key, lang, kwargs)
}

func pickerResets(resetsAtMs *int64, lang string) string {
	if d := compactDurationAt(resetsAtMs, nil); d != "" {
		return pickerT("resets_in", lang, map[string]any{"duration": d})
	}
	return pickerT("resets_soon", lang, nil)
}

// Uppercases the first character the way Python upper does, the rest unchanged
func pyCapitalizeFirst(text string) string {
	r, size := utf8.DecodeRuneInString(text)
	if size == 0 {
		return text
	}
	return strings.ToUpper(string(r)) + text[size:]
}

// Limit name for the stop line, the server title or id for an unknown allowance
func pickerLimitName(g UsageGauge, lang string) string {
	switch g.Label {
	case GaugeToday, GaugeWeek, GaugeMonth, GaugeTopTier:
		return pickerT("limit_"+g.Label, lang, nil)
	}
	return firstNonEmpty(g.Title, g.Label)
}

// UsageLevelLabel is the row tag Large or Free in the catalog language, empty for every other level
func UsageLevelLabel(level, lang string) string {
	if shownUsageLevels[level] {
		return pickerT("level_"+level, lang, nil)
	}
	return ""
}

// Picker usage of a Portal account snapshot, nil for anything but a usage window plan with an all scope allowance
func pickerUsageFromAccount(info *NousPortalAccountInfo) *PickerUsage {
	if info == nil || !info.LoggedIn || info.UsageWindow == nil {
		return nil
	}
	window := info.UsageWindow
	parsed := ParseGauges(window.Gauges)
	if len(parsed) == 0 || len(AllScope(parsed)) == 0 {
		return nil
	}
	u := &PickerUsage{PausedReason: window.PausedReason, CreditsPay: pickerCreditsPay(info, window)}
	if info.Subscription != nil && info.Subscription.Plan != nil {
		u.Plan = *info.Subscription.Plan
	}
	for _, g := range parsed {
		u.Gauges = append(u.Gauges, UsageGauge{Label: g.ID, LeftFraction: 1.0 - g.UsedFraction(), ResetsAtMs: g.ResetsAtMs, Scope: g.Scope, Title: g.Label})
	}
	for _, m := range window.FeaturedModelIDs {
		if m != "" {
			u.FeaturedModelIDs = append(u.FeaturedModelIDs, m)
		}
	}
	return u
}

// Top up credits pay past a full allowance when keep going is not off and a balance is left, nil when unknown
func pickerCreditsPay(info *NousPortalAccountInfo, window *NousUsageWindowInfo) *bool {
	if window.KeepGoingOnCredits != nil && !*window.KeepGoingOnCredits {
		f := false
		return &f
	}
	if info.PaidServiceAccessInfo == nil || info.PaidServiceAccessInfo.PurchasedCreditsRemaining == nil {
		return nil
	}
	pays := *info.PaidServiceAccessInfo.PurchasedCreditsRemaining > 0
	return &pays
}

// Picker usage from the cached account snapshot, warm fetching once so a cold start knows a window plan
func nousPickerUsageIn(ctx context.Context, warm, anyAge bool) *PickerUsage {
	if warm && pickerCachedAccountFn(ctx, false) == nil {
		var window *NousUsageWindowInfo
		if stale := pickerCachedAccountFn(ctx, true); stale != nil {
			window = stale.UsageWindow
		}
		pickerFetchAccountFn(ctx, applyWindowSeedFields(window, nil))
	}
	return pickerUsageFromAccount(pickerCachedAccountFn(ctx, anyAge))
}

// NousPickerUsage is the current profile's plan usage, anyAge reading a snapshot past the cache TTL
func NousPickerUsage(warm, anyAge bool) *PickerUsage {
	return nousPickerUsageIn(context.Background(), warm, anyAge)
}

// Plan usage of the env's login with its featured picks, false for every account without a window plan
func nousPickerUsage(env PoolEnv, warm bool) (any, []string, bool) {
	u := nousPickerUsageIn(env.ctx(), warm, false)
	if u == nil {
		return nil, nil, false
	}
	return u, u.FeaturedModelIDs, true
}

// This profile's Nous catalog rows from its resident cache, fetched once only when asked
func nousCatalogRows(ctx context.Context, fetch bool) PricingTable {
	rows := pickerPricingFn(ctx, "nous", "", false, true)
	if len(rows) == 0 && fetch {
		rows = pickerPricingFn(ctx, "nous", "", false, false)
	}
	return rows
}

// LargeModelNoticeFor warns on a switch to a gateway marked Large Nous model, empty for every other switch
func LargeModelNoticeFor(ctx context.Context, modelID, provider string, fetch bool, lang string) string {
	if strings.ToLower(pyStripSpace(provider)) != "nous" || modelID == "" {
		return ""
	}
	fresh := nousPickerUsageIn(ctx, false, false)
	usage := fresh
	if usage == nil {
		usage = nousPickerUsageIn(ctx, false, true)
	}
	row := nousCatalogRows(ctx, fetch && usage != nil)[modelID]
	if len(row) == 0 {
		return ""
	}
	one := map[string]any{modelID: row}
	if UsageLevels([]string{modelID}, one)[modelID] != "top_tier" {
		return ""
	}
	if usage == nil {
		return pickerT("large_notice", lang, nil)
	}
	return usage.WithPricing(one).LargeModelNotice(modelID, fresh != nil, lang)
}

// Large model warning for the env's login without a catalog fetch
func largeModelNotice(env PoolEnv, model, provider string) string {
	return LargeModelNoticeFor(env.ctx(), model, provider, false, "")
}

// Pads to the width in characters with trailing spaces
func pickerLjust(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

// Pads to the width in characters with leading spaces
func pickerRjust(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return strings.Repeat(" ", width-n) + s
	}
	return s
}

func stringSetOf(v any) map[string]bool {
	out := map[string]bool{}
	switch items := v.(type) {
	case []string:
		for _, s := range items {
			out[s] = true
		}
	case []any:
		for _, s := range items {
			if str, ok := s.(string); ok {
				out[str] = true
			}
		}
	}
	return out
}

// WindowRowLabels tags CLI picker rows Large or Free on a window plan row, the bare ids otherwise
func WindowRowLabels(providerRow any, modelIDs []string) []string {
	ids := append([]string(nil), modelIDs...)
	row, _ := providerRow.(map[string]any)
	plan, planOK := row["plan_usage"].(map[string]any)
	var levels map[string]any
	levelsOK := false
	if planOK && plan["kind"] == "usage_window" {
		levels, levelsOK = row["usage_levels"].(map[string]any)
	}
	if !levelsOK {
		return ids
	}
	subscribed := stringSetOf(plan["subscription_model_ids"])
	tags := make([]string, len(ids))
	anyTag := false
	for i, m := range ids {
		level := ""
		if !subscribed[m] {
			level, _ = levels[m].(string)
		}
		tags[i] = UsageLevelLabel(level, "")
		anyTag = anyTag || tags[i] != ""
	}
	if !anyTag {
		return ids
	}
	width := 0
	for _, m := range ids {
		width = max(width, utf8.RuneCountInString(m))
	}
	width = min(width, 48)
	out := make([]string, len(ids))
	for i, m := range ids {
		out[i] = strings.TrimRightFunc(pickerLjust(m, width)+"  "+tags[i], unicode.IsSpace)
	}
	return out
}

// PickerRowSuffix is the Large or Free label stamped on one messaging picker row, empty otherwise
func PickerRowSuffix(providerRow any, modelID string) string {
	row, ok := providerRow.(map[string]any)
	if !ok {
		return ""
	}
	labels, _ := row["model_labels"].(map[string]any)
	if v := labels[modelID]; truthy(v) {
		return pyStr(v)
	}
	return ""
}

func pickerGaugeName(g UsageGauge) string {
	return GaugeShort(WindowGauge{ID: g.Label, Scope: g.Scope, Label: firstNonEmpty(g.Title, g.Label)})
}

// PlanGaugeLines is the plan head then one line per server allowance in server order
func PlanGaugeLines(u *PickerUsage) []string {
	paused := ""
	if u.PausedReason != "" {
		paused = " (paused)"
		if PausedForPayment(u.PausedReason) {
			paused = " (paused: renewal unpaid)"
		}
	}
	labelW := 4
	for i, g := range u.Gauges {
		if n := utf8.RuneCountInString(pickerGaugeName(g)); i == 0 || n > labelW {
			labelW = n
		}
	}
	lines := []string{firstNonEmpty(u.Plan, "Icebath plan") + paused}
	for _, g := range u.Gauges {
		lines = append(lines, fmt.Sprintf("%s  %s  %s  %s", pickerLjust(pickerGaugeName(g), labelW), g.Bar(),
			pickerLjust(g.PercentUsed()+" used", 9), FormatResetsIn(g.ResetsAtMs, nil)))
	}
	return lines
}

// First listed featured pick of a window plan that is not Large, empty for every other account
func windowPlanDefault(modelIDs []string, pricing map[string]any) string {
	usage := NousPickerUsage(false, true)
	if usage == nil {
		return ""
	}
	recommended, _ := splitRecommended(modelIDs, usage.FeaturedModelIDs)
	for _, mid := range recommended {
		row := dictOrEmpty(pricing[mid])
		if !truthy(row["top_tier"]) && row["usage_level"] != "top_tier" {
			return mid
		}
	}
	return ""
}

// AnnotateWindowPlanRows is the model stage labels and the limit line on a Nous window plan, bare ids otherwise
func AnnotateWindowPlanRows(ctx context.Context, providerData map[string]any, modelIDs []string) ([]string, string) {
	slug := ""
	if v := providerData["slug"]; truthy(v) {
		slug = pyStr(v)
	}
	if strings.ToLower(slug) != "nous" {
		return append([]string(nil), modelIDs...), ""
	}
	usage := nousPickerUsageIn(ctx, false, false)
	line := ""
	if usage != nil {
		line = usage.LimitLine("")
	}
	return WindowRowLabels(providerData, modelIDs), line
}
