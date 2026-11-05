package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func pickerLabelTail(label, mid string) string {
	_, tail, _ := strings.Cut(label, mid)
	return strings.TrimSpace(tail)
}

func TestPickerRenderBarFillsWithTheFractionGiven(t *testing.T) {
	if RenderBar(1.0, BarWidth) != "[||||||||||]" || RenderBar(0.0, BarWidth) != "[          ]" {
		t.Fatal("full or empty bar wrong")
	}
	if b := RenderBar(0.01, BarWidth); !strings.Contains(b, "|") || b == "[||||||||||]" {
		t.Fatalf("sliver bar %q", b)
	}
	if RenderBar(0.999, BarWidth) == "[||||||||||]" {
		t.Fatal("almost full reads full")
	}
}

func TestPickerPercentUsedMatchesThePortalRule(t *testing.T) {
	for in, want := range map[float64]string{0.0: "0%", 0.0001: "<1%", 0.0047: "<1%", 0.01: "1%", 0.996: "99%", 0.9999: "99%", 1.0: "100%"} {
		if got := FormatPercentUsed(in); got != want {
			t.Fatalf("%v: %q want %q", in, got, want)
		}
	}
}

func pickerLineWith(lines []string, prefix string) string {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

func TestPickerWindowGaugesReadAsUsedAndFillUp(t *testing.T) {
	usage := pickerWindowUsage(pickerKW{"day_used_bp": 47, "week_used_bp": 0, "top_tier_used_bp": 10_000})
	lines := PlanGaugeLines(usage)
	today, week, top := pickerLineWith(lines, "today"), pickerLineWith(lines, "week"), pickerLineWith(lines, "large models")
	if !strings.Contains(today, "[|         ]") || !strings.Contains(today, "<1% used") {
		t.Fatalf("today %q", today)
	}
	if !strings.Contains(week, "[          ]") || !strings.Contains(week, "0% used") {
		t.Fatalf("week %q", week)
	}
	if !strings.Contains(top, "[||||||||||]") || !strings.Contains(top, "100% used") {
		t.Fatalf("top %q", top)
	}
	if usage.Gauge(GaugeToday).Style() != "green" || usage.Gauge("top_tier").Style() != "red" {
		t.Fatal("styles")
	}
}

func TestPickerUsageLevelIsTheGatewayWordNeverComputedHere(t *testing.T) {
	pricing := map[string]any{
		"small":     map[string]any{"prompt": "0.00000014", "completion": "0.00000028", "usage_level": "low"},
		"medium":    map[string]any{"prompt": "0.000001", "completion": "0.0000032", "usage_level": "medium"},
		"large":     map[string]any{"prompt": "0.000005", "completion": "0.000025", "usage_level": "top_tier", "top_tier": true},
		"gift:free": map[string]any{"prompt": "0", "completion": "0", "usage_level": "free"},
	}
	ids := []string{"small", "medium", "large", "gift:free"}
	if got := UsageLevels(ids, pricing); !reflect.DeepEqual(got, map[string]string{"small": "low", "medium": "medium", "large": "top_tier", "gift:free": "free"}) {
		t.Fatalf("levels %v", got)
	}
	bare := map[string]any{}
	for m, p := range pricing {
		row := map[string]any{}
		for k, v := range p.(map[string]any) {
			if k != "usage_level" {
				row[k] = v
			}
		}
		bare[m] = row
	}
	if got := UsageLevels(ids, bare); !reflect.DeepEqual(got, map[string]string{"large": "top_tier", "gift:free": "free"}) {
		t.Fatalf("bare %v", got)
	}
	if got := UsageLevels([]string{"u", "x"}, map[string]any{"u": map[string]any{}, "x": map[string]any{"usage_level": "huge"}}); len(got) != 0 {
		t.Fatalf("unknown %v", got)
	}
	if got := UsageLevels([]string{"large"}, pricing); !reflect.DeepEqual(got, map[string]string{"large": "top_tier"}) {
		t.Fatalf("one %v", got)
	}
}

func TestPickerWindowPickerShowsTheUsageWordNotPricesOrBars(t *testing.T) {
	installPickerEnglish(t)
	usage := pickerUsageFromAccount(pickerWindowAccount("Window Plan", pickerKW{"day_used_bp": 2000, "week_used_bp": 1000, "top_tier_used_bp": 0, "day_resets_at_ms": 1, "week_resets_at_ms": 2}))
	if usage == nil {
		t.Fatal("no usage")
	}
	rows := newModelPickerRows([]string{"cheap/m", "pricey/m"}, PricingTable{
		"cheap/m":  {"prompt": "0.00000006", "completion": "0.00000012", "usage_level": "low"},
		"pricey/m": {"prompt": "0.000003", "completion": "0.000015", "usage_level": "top_tier", "top_tier": true},
	}, "", true, usage, nil)
	cheap, pricey := rows.label("cheap/m"), rows.label("pricey/m")
	for _, l := range []string{cheap, pricey} {
		if strings.Contains(l, "$") || strings.Contains(l, "[") {
			t.Fatalf("row %q", l)
		}
	}
	if strings.TrimSpace(cheap) != "cheap/m" || !strings.HasSuffix(strings.TrimRight(pricey, " "), "Large") || strings.HasPrefix(pricey, "★") {
		t.Fatalf("rows %q %q", cheap, pricey)
	}
	if len([]rune(strings.TrimRight(cheap, " "))) > len([]rune(pricey)) {
		t.Fatal("column not aligned")
	}
	desc := rows.descriptionLines()
	header := strings.Join(desc, "\n")
	if !strings.Contains(header, "Usage") || strings.Contains(header, "bar") || strings.Contains(header, "★") {
		t.Fatalf("header %q", header)
	}
	if rows.footnote() != TopTierFootnote {
		t.Fatal("footnote")
	}
	find := func(word string) string {
		for _, l := range desc {
			if strings.Contains(strings.ToLower(l), word) {
				return l
			}
		}
		return ""
	}
	todayLine, weekLine, topLine := find("today"), find("week"), find("large models")
	if todayLine == weekLine || weekLine == topLine {
		t.Fatalf("gauge lines %q %q %q", todayLine, weekLine, topLine)
	}
	if !strings.Contains(desc[0], "Window Plan") || !strings.Contains(desc[0], "included") {
		t.Fatalf("head %q", desc[0])
	}
}

func TestPickerFootnoteOnlyWhenATopTierRowIsListed(t *testing.T) {
	rows := newModelPickerRows([]string{"cheap/m"}, PricingTable{"cheap/m": {"prompt": "0.0000001", "completion": "0.0000002", "usage_level": "low"}}, "", true, pickerWindowUsage(nil), nil)
	if rows.footnote() != "" {
		t.Fatal("footnote without a Large row")
	}
	if newModelPickerRows([]string{"x"}, PricingTable{"x": {"top_tier": true}}, "", true, nil, nil).footnote() != "" {
		t.Fatal("credit grant footnote")
	}
}

func TestPickerCreditGrantAccountGetsNoPlanLine(t *testing.T) {
	plan := "Credit Plan"
	if pickerUsageFromAccount(&NousPortalAccountInfo{LoggedIn: true, Subscription: &NousPortalSubscriptionInfo{Plan: &plan}}) != nil {
		t.Fatal("credit grant got plan chrome")
	}
}

func TestPickerFreeOrPaygAccountHasNoPickerUsage(t *testing.T) {
	if pickerUsageFromAccount(&NousPortalAccountInfo{LoggedIn: true}) != nil || pickerUsageFromAccount(nil) != nil {
		t.Fatal("usage without a window")
	}
}

func TestPickerWindowPlanGaugesStackOnSeparateLines(t *testing.T) {
	stacked := PlanGaugeLines(pickerUsageFromAccount(pickerWindowAccount("Window Plan", pickerKW{})))
	if stacked[0] != "Window Plan" || len(stacked) != 4 {
		t.Fatalf("stacked %q", stacked)
	}
	for _, p := range []string{"today", "week", "large models"} {
		if pickerLineWith(stacked, p) == "" {
			t.Fatalf("missing %s in %q", p, stacked)
		}
	}
}

func pickerGaugeIDs(gs []UsageGauge) []string {
	out := []string{}
	for _, g := range gs {
		out = append(out, g.Label)
	}
	return out
}

func TestPickerWindowGaugesForModelOmitTopTierOnRegularIDs(t *testing.T) {
	usage := pickerUsageFromAccount(pickerWindowAccount("Window Plan", pickerKW{"top_tier_used_bp": 5000, "day_resets_at_ms": 1, "week_resets_at_ms": 2})).
		WithPricing(map[string]any{"example/model-b": map[string]any{"prompt": "0.000005", "completion": "0.000025", "top_tier": true}})
	if got := pickerGaugeIDs(usage.GaugesForModel("example/model-a")); !reflect.DeepEqual(got, []string{GaugeToday, GaugeWeek}) {
		t.Fatalf("regular %v", got)
	}
	if got := pickerGaugeIDs(usage.GaugesForModel("example/model-b")); got[len(got)-1] != "top_tier" {
		t.Fatalf("top %v", got)
	}
}

func TestPickerMonthRowOnlyWhenTheServerListsIt(t *testing.T) {
	installPickerEnglish(t)
	if pickerWindowUsage(nil).Gauge(GaugeMonth) != nil {
		t.Fatal("month row without a month allowance")
	}
	usage := pickerWindowUsage(pickerKW{"month_used_bp": 10_000, "month_resets_at_ms": 333, "top_tier_used_bp": 2_000})
	if got := pickerGaugeIDs(usage.Gauges); !reflect.DeepEqual(got, []string{GaugeToday, GaugeWeek, GaugeMonth, "top_tier"}) {
		t.Fatalf("gauges %v", got)
	}
	if *usage.Gauge(GaugeMonth).ResetsAtMs != 333 {
		t.Fatal("month reset")
	}
	var firsts []string
	for _, l := range PlanGaugeLines(usage)[1:] {
		firsts = append(firsts, strings.Fields(l)[0])
	}
	if !reflect.DeepEqual(firsts, []string{"today", "week", "month", "large"}) {
		t.Fatalf("names %v", firsts)
	}
	if !strings.HasPrefix(usage.IncludedSummary(), "Monthly limit reached") || !usage.Blocks("any/model") {
		t.Fatalf("summary %q", usage.IncludedSummary())
	}
}

func TestPickerDrawsWhateverAllowancesTheServerLists(t *testing.T) {
	installPickerEnglish(t)
	info := pickerWindowAccount("Window Plan", pickerKW{})
	info.UsageWindow.Gauges = append([]WindowGauge{{ID: "five_hour", Scope: "all", Label: "Next 5 hours", UsedBp: 10_000, ResetsAtMs: pickerI64(pickerFar)}}, info.UsageWindow.Gauges...)
	usage := pickerUsageFromAccount(info)
	if usage.Gauges[0].Label != "five_hour" || !strings.HasPrefix(PlanGaugeLines(usage)[1], "Next 5 hours") {
		t.Fatalf("lines %q", PlanGaugeLines(usage))
	}
	if !strings.HasPrefix(usage.IncludedSummary(), "Next 5 hours limit reached") || !usage.Blocks("any/model") {
		t.Fatalf("summary %q", usage.IncludedSummary())
	}
}

func TestPickerTopTierGaugeIsLabelledMonthlyAndResetsOnItsOwnClock(t *testing.T) {
	usage := pickerWindowUsage(pickerKW{"top_tier_used_bp": 2_000, "day_resets_at_ms": 111, "top_tier_resets_at_ms": 222})
	if *usage.Gauge("top_tier").ResetsAtMs != 222 || pickerLineWith(PlanGaugeLines(usage), "large models (monthly)  ") == "" {
		t.Fatalf("top %q", PlanGaugeLines(usage))
	}
	if *pickerWindowUsage(pickerKW{"top_tier_used_bp": 2_000, "day_resets_at_ms": 111}).Gauge("top_tier").ResetsAtMs != 111 {
		t.Fatal("top tier did not fall back to the day reset")
	}
}

func TestPickerBlocksOnlyModelsWhoseAllowanceIsEmpty(t *testing.T) {
	installPickerEnglish(t)
	usage := pickerWindowUsage(pickerKW{"top_tier_used_bp": 10_000})
	if !usage.Blocks("example/model-b") || usage.Blocks("example/model-a") {
		t.Fatal("blocks")
	}
	if !strings.HasPrefix(usage.IncludedSummary(), "Monthly large-model limit reached") {
		t.Fatalf("summary %q", usage.IncludedSummary())
	}
	if pickerWindowUsage(nil).IncludedSummary() != "every model below is included" {
		t.Fatal("quiet summary")
	}
}

func TestPickerBlocksEveryPaidModelWhenTheDayIsSpent(t *testing.T) {
	installPickerEnglish(t)
	usage := pickerWindowUsage(pickerKW{"day_used_bp": 10_000})
	if !usage.Blocks("example/model-a") || !strings.HasPrefix(usage.IncludedSummary(), "Daily limit reached") {
		t.Fatalf("summary %q", usage.IncludedSummary())
	}
}

func TestPickerPayAsYouGoWordingWhenTopUpCreditsPay(t *testing.T) {
	installPickerEnglish(t)
	usage := pickerWithCredits(pickerWindowUsage(pickerKW{"day_used_bp": 10_000}), pickerBool(true))
	if usage.Blocks("example/model-a") {
		t.Fatal("credits pay row blocked")
	}
	line := usage.LimitLine("")
	if !strings.HasPrefix(line, "Daily limit reached · resets ") || !strings.HasSuffix(line, " · paid models now use your credits (pay as you go)") {
		t.Fatalf("line %q", line)
	}
	stops := pickerWithCredits(usage, pickerBool(false))
	if !stops.Blocks("example/model-a") || !strings.HasSuffix(stops.LimitLine(""), " · only Free models until then") {
		t.Fatalf("stops %q", stops.LimitLine(""))
	}
}

func TestPickerLargeModelLimitBlocksOnlyLargeRows(t *testing.T) {
	installPickerEnglish(t)
	usage := pickerWithCredits(pickerWindowUsage(pickerKW{"top_tier_used_bp": 10_000}), pickerBool(true))
	if usage.Blocks("example/model-b") || usage.Blocks("example/model-a") {
		t.Fatal("credits pay blocked")
	}
	stops := pickerWithCredits(usage, pickerBool(false))
	if !stops.Blocks("example/model-b") || stops.Blocks("example/model-a") {
		t.Fatal("stops blocks")
	}
	if l := usage.LimitLine(""); !strings.HasPrefix(l, "Monthly large-model limit reached") || !strings.HasSuffix(l, " · Large models now use your credits (pay as you go)") {
		t.Fatalf("line %q", l)
	}
	if !strings.HasSuffix(stops.LimitLine(""), " · Large models paused until then") {
		t.Fatalf("stops %q", stops.LimitLine(""))
	}
}

func TestPickerCreditsPayReadsKeepGoingAndTheTopUpBalance(t *testing.T) {
	on, off := true, false
	window := &NousUsageWindowInfo{KeepGoingOnCredits: &on}
	balance := func(v float64) *NousPortalAccountInfo {
		return &NousPortalAccountInfo{PaidServiceAccessInfo: &NousPaidServiceAccessInfo{PurchasedCreditsRemaining: &v}}
	}
	if p := pickerCreditsPay(balance(12.5), window); p == nil || !*p {
		t.Fatal("balance")
	}
	if p := pickerCreditsPay(balance(0.0), window); p == nil || *p {
		t.Fatal("empty")
	}
	if pickerCreditsPay(&NousPortalAccountInfo{}, window) != nil {
		t.Fatal("unknown")
	}
	if p := pickerCreditsPay(balance(12.5), &NousUsageWindowInfo{KeepGoingOnCredits: &off}); p == nil || *p {
		t.Fatal("keep going off")
	}
}

func TestPickerAFreeModelIsNeverUsedUp(t *testing.T) {
	pricing := map[string]any{
		"gift/m:free": map[string]any{"prompt": "0", "completion": "0"},
		"zero/m":      map[string]any{"prompt": "0", "completion": "0"},
		"paid/m":      map[string]any{"prompt": "0.000001", "completion": "0.000002"},
	}
	usage := pickerWindowUsage(pickerKW{"day_used_bp": 10_000, "top_tier_used_bp": 10_000}).WithPricing(pricing)
	if usage.Blocks("gift/m:free") || usage.Blocks("zero/m") || !usage.Blocks("paid/m") {
		t.Fatal("free rows")
	}
	if pickerWindowUsage(pickerKW{"day_used_bp": 10_000}).Blocks("other/x:free") {
		t.Fatal(":free id blocked")
	}
}

func TestPickerTheNoteNamesTheAllowanceThatFreesUpLast(t *testing.T) {
	usage := pickerWindowUsage(pickerKW{"day_used_bp": 10_000, "week_used_bp": 10_000, "day_resets_at_ms": 1_000, "week_resets_at_ms": pickerFar})
	if g := usage.ExhaustedGauge("paid/m"); g == nil || g.Label != GaugeWeek {
		t.Fatalf("gauge %v", g)
	}
}

func TestPickerWindowPickerLabelsFreeModelsFromTheCatalogPrice(t *testing.T) {
	installPickerEnglish(t)
	pricing := PricingTable{
		"cheap/m":     {"prompt": "0.00000006", "completion": "0.00000012", "usage_level": "low"},
		"gift/m:free": {"prompt": "0", "completion": "0"},
		"unpriced/m":  {},
	}
	rows := newModelPickerRows([]string{"cheap/m", "gift/m:free", "unpriced/m"}, pricing, "", true, pickerWindowUsage(nil), nil)
	free := rows.label("gift/m:free")
	if pickerLabelTail(free, "gift/m:free") != "Free" || pickerLabelTail(rows.label("unpriced/m"), "unpriced/m") != "" {
		t.Fatalf("labels %q %q", free, rows.label("unpriced/m"))
	}
	if len([]rune(free)) != len([]rune(rows.label("cheap/m"))) {
		t.Fatal("column not aligned")
	}
}

func TestPickerCreditPickerFreeRowsAreUnchanged(t *testing.T) {
	rows := newModelPickerRows([]string{"gift/m:free"}, PricingTable{"gift/m:free": {"prompt": "0", "completion": "0"}}, "", true, nil, nil)
	if rows.windowMode || !strings.Contains(rows.label("gift/m:free"), "free") {
		t.Fatalf("row %q", rows.label("gift/m:free"))
	}
}

var pickerPrices = map[string]any{
	"example/model-a": map[string]any{"prompt": "0.0000000108", "completion": "0.00000128"},
	"example/model-b": map[string]any{"prompt": "0.00000041", "completion": "0.00000399"},
	"example/model-c": map[string]any{"prompt": "0.000005", "completion": "0.000025", "top_tier": true},
}

func TestPickerTopTierComesFromTheGatewayMarks(t *testing.T) {
	marked := pickerWindowUsage(pickerKW{"featured_model_ids": []string{"example/model-a", "example/model-b"}}).WithPricing(pickerPrices)
	if !marked.IsTopTier("example/model-c") || marked.IsTopTier("example/model-b") {
		t.Fatal("marks")
	}
	payload := marked.ToPayload()
	if !reflect.DeepEqual(payload["top_tier_model_ids"], []any{"example/model-c"}) || !reflect.DeepEqual(payload["featured_model_ids"], []any{"example/model-a", "example/model-b"}) {
		t.Fatalf("payload %v", payload)
	}
}

func TestPickerNoMarksMeansNothingIsTopTier(t *testing.T) {
	unmarked := map[string]any{}
	for k, v := range pickerPrices {
		row := map[string]any{}
		for kk, vv := range v.(map[string]any) {
			if kk != "top_tier" {
				row[kk] = vv
			}
		}
		unmarked[k] = row
	}
	usage := pickerWindowUsage(nil).WithPricing(unmarked)
	if usage.IsTopTier("example/model-b") || usage.IsTopTier("example/model-c") {
		t.Fatal("unmarked top tier")
	}
	bare := pickerUsageFromAccount(pickerWindowAccount("Window Plan", pickerKW{"top_tier_used_bp": 10_000}))
	if bare == nil || bare.IsTopTier("example/model-b") || bare.Blocks("example/model-b") {
		t.Fatal("bare")
	}
	if !reflect.DeepEqual(bare.ToPayload()["top_tier_model_ids"], []any{}) {
		t.Fatalf("payload %v", bare.ToPayload()["top_tier_model_ids"])
	}
}

func TestPickerFeaturedFirstPutsPlanPicksFirstAndKeepsTheRestInOrder(t *testing.T) {
	listed := []string{"a/x", "example/model-b", "b/y", "example/model-a"}
	featured := []string{"example/model-a", "example/model-b", "not/listed"}
	if got := featuredFirst(listed, featured); !reflect.DeepEqual(got, []string{"example/model-a", "example/model-b", "a/x", "b/y"}) {
		t.Fatalf("first %v", got)
	}
	if got := featuredFirst(listed, nil); !reflect.DeepEqual(got, listed) {
		t.Fatalf("none %v", got)
	}
	head, rest := splitRecommended(listed, featured)
	if !reflect.DeepEqual(head, []string{"example/model-a", "example/model-b"}) || !reflect.DeepEqual(rest, []string{"a/x", "b/y"}) {
		t.Fatalf("split %v %v", head, rest)
	}
	head, rest = splitRecommended(listed, nil)
	if len(head) != 0 || !reflect.DeepEqual(rest, listed) {
		t.Fatalf("split none %v %v", head, rest)
	}
}

func TestPickerCLIRowsMarkTopTierFromTheGatewayWord(t *testing.T) {
	installPickerEnglish(t)
	prices := PricingTable{}
	for k, v := range pickerPrices {
		prices[k] = v.(map[string]any)
	}
	prices["example/model-c"] = map[string]any{"prompt": "0.000005", "completion": "0.000025", "top_tier": true, "usage_level": "top_tier"}
	rows := newModelPickerRows([]string{"example/model-a", "example/model-b", "example/model-c"}, prices, "", true, pickerWindowUsage(nil), nil)
	if !rows.hasTopTier || !strings.HasSuffix(rows.label("example/model-c"), "Large") || !rows.usage.IsTopTier("example/model-c") || rows.usage.IsTopTier("example/model-b") {
		t.Fatalf("rows %q", rows.label("example/model-c"))
	}
}

func TestPickerWarmPickerRefetchesAKnownWindowPlanPastTheCacheTTL(t *testing.T) {
	window := pickerWindowAccount("Window Plan", pickerKW{"day_used_bp": 100, "week_used_bp": 200})
	calls := scriptPickerAccounts(t, window, window)
	usage := nousPickerUsageIn(context.Background(), true, false)
	if !reflect.DeepEqual(*calls, []bool{true}) || usage == nil || usage.Gauge(GaugeToday) == nil {
		t.Fatalf("calls %v usage %v", *calls, usage)
	}
}

func TestPickerWarmPickerNeverForcesAFetchForANonWindowAccount(t *testing.T) {
	plan := "Credit Plan"
	credit := &NousPortalAccountInfo{LoggedIn: true, Subscription: &NousPortalSubscriptionInfo{Plan: &plan}}
	for _, stale := range []*NousPortalAccountInfo{credit, nil} {
		calls := scriptPickerAccounts(t, stale, credit)
		if nousPickerUsageIn(context.Background(), true, false) != nil || !reflect.DeepEqual(*calls, []bool{false}) {
			t.Fatalf("calls %v", *calls)
		}
	}
}

func TestPickerTheInChatCLIPickerSaysTheLimitOnceOnNousOnly(t *testing.T) {
	installPickerEnglish(t)
	servePickerSnapshot(t, pickerWindowAccount("Window Plan", pickerKW{"day_used_bp": 10_000}))
	rows, line := AnnotateWindowPlanRows(context.Background(), map[string]any{"slug": "nous"}, []string{"paid/m", "gift/m:free"})
	if !reflect.DeepEqual(rows, []string{"paid/m", "gift/m:free"}) || !strings.HasPrefix(line, "Daily limit reached") {
		t.Fatalf("rows %v line %q", rows, line)
	}
	if rows, line := AnnotateWindowPlanRows(context.Background(), map[string]any{"slug": "example"}, []string{"paid/m"}); !reflect.DeepEqual(rows, []string{"paid/m"}) || line != "" {
		t.Fatalf("other %v %q", rows, line)
	}
	servePickerSnapshot(t, nil)
	if rows, line := AnnotateWindowPlanRows(context.Background(), map[string]any{"slug": "nous"}, []string{"paid/m"}); !reflect.DeepEqual(rows, []string{"paid/m"}) || line != "" {
		t.Fatalf("no snapshot %v %q", rows, line)
	}
}

func pickerFields(rows []string) [][]string {
	out := [][]string{}
	for _, r := range rows {
		out = append(out, strings.Fields(r))
	}
	return out
}

func TestPickerTheInChatCLIPickerTagsLargeAndFreeRowsOnWindowPlansOnly(t *testing.T) {
	installPickerEnglish(t)
	servePickerSnapshot(t, nil)
	window := map[string]any{"slug": "nous", "plan_usage": map[string]any{"kind": "usage_window"},
		"usage_levels": map[string]any{"example/model-b": "top_tier", "example/model-a": "low", "example/model-free:free": "free"}}
	ids := []string{"example/model-b", "example/model-a", "example/model-free:free"}
	rows, _ := AnnotateWindowPlanRows(context.Background(), window, ids)
	if want := [][]string{{"example/model-b", "Large"}, {"example/model-a"}, {"example/model-free:free", "Free"}}; !reflect.DeepEqual(pickerFields(rows), want) {
		t.Fatalf("rows %q", rows)
	}
	plain := map[string]any{"slug": "nous", "plan_usage": nil, "usage_levels": window["usage_levels"]}
	if rows, _ := AnnotateWindowPlanRows(context.Background(), plain, ids); !reflect.DeepEqual(rows, ids) {
		t.Fatalf("plain %q", rows)
	}
}

var pickerLevels = PricingTable{"example/model-b": {"usage_level": "top_tier", "top_tier": true}, "example/model-a": {"usage_level": "low"}}

func TestPickerLargeModelNoticeWindowPlansOnly(t *testing.T) {
	installPickerEnglish(t)
	ctx := context.Background()
	servePickerCatalog(t, pickerLevels)
	servePickerSnapshot(t, pickerWindowAccount("Window Plan", pickerKW{}))
	note := LargeModelNoticeFor(ctx, "example/model-b", "nous", false, "")
	if !strings.HasPrefix(note, "Large model: uses more of your plan's usage per message.") || !strings.Contains(note, "Monthly large-model limit:") {
		t.Fatalf("note %q", note)
	}
	if LargeModelNoticeFor(ctx, "example/model-a", "nous", false, "") != "" || LargeModelNoticeFor(ctx, "example/model-b", "example", false, "") != "" {
		t.Fatal("notice for a non Large or non Nous switch")
	}
	servePickerSnapshot(t, pickerWindowAccount("Window Plan", pickerKW{"top_tier_used_bp": 10_000, "keep_going_on_credits": true, "purchased_credits_remaining": 5.0}))
	if got := LargeModelNoticeFor(ctx, "example/model-b", "nous", false, ""); got != "Large model: uses more credits per message (pay as you go)." {
		t.Fatalf("credits %q", got)
	}
	servePickerSnapshot(t, nil)
	if got := LargeModelNoticeFor(ctx, "example/model-b", "nous", false, ""); got != "Large model: uses more of your plan's usage per message." {
		t.Fatalf("cold %q", got)
	}
	seen := servePickerCatalog(t, PricingTable{"example/model-b": {"prompt": "0.000001"}})
	if LargeModelNoticeFor(ctx, "example/model-b", "nous", true, "") != "" || !reflect.DeepEqual(*seen, []bool{true}) {
		t.Fatalf("fetched %v", *seen)
	}
}

func TestPickerCatalogReadsNeverAnswerFromAnotherProfilesCache(t *testing.T) {
	servePickerCatalog(t, PricingTable{})
	servePickerSnapshot(t, nil)
	if LargeModelNoticeFor(context.Background(), "example/model-b", "nous", false, "") != "" {
		t.Fatal("notice from another profile's catalog")
	}
}

var pickerSubscriptionBilled = map[string]any{
	"example/model-sub": map[string]any{"prompt": "0.000005", "completion": "0.00004", "billing_mode": "subscription"},
	"gift/m:free":       map[string]any{"prompt": "0", "completion": "0"},
	"example/model-b":   map[string]any{"prompt": "0.000005", "completion": "0.000025", "usage_level": "top_tier", "top_tier": true},
}

var pickerSubscriptionIDs = []string{"example/model-sub", "gift/m:free", "example/model-b"}

func TestPickerSubscriptionBilledRowsDrawNothingButCarryNoFreeTag(t *testing.T) {
	if UsageLevels(pickerSubscriptionIDs, pickerSubscriptionBilled)["example/model-sub"] != "free" {
		t.Fatal("subscription row not free to the window")
	}
	if got := usageTagLevels(pickerSubscriptionIDs, pickerSubscriptionBilled); !reflect.DeepEqual(got, map[string]string{"gift/m:free": "free", "example/model-b": "top_tier"}) {
		t.Fatalf("tags %v", got)
	}
	usage := pickerWindowUsage(pickerKW{"day_used_bp": 10_000, "top_tier_used_bp": 10_000}).WithPricing(pickerSubscriptionBilled)
	if usage.Blocks("example/model-sub") || usage.Blocks("gift/m:free") || !usage.Blocks("example/model-b") {
		t.Fatal("blocks")
	}
	if !reflect.DeepEqual(usage.ToPayload()["subscription_model_ids"], []any{"example/model-sub"}) || !reflect.DeepEqual(pickerWindowUsage(nil).ToPayload()["subscription_model_ids"], []any{}) {
		t.Fatal("payload subscription ids")
	}
}

func TestPickerWindowPickerRowsLeaveSubscriptionBilledRowsUntagged(t *testing.T) {
	installPickerEnglish(t)
	prices := PricingTable{}
	for k, v := range pickerSubscriptionBilled {
		prices[k] = v.(map[string]any)
	}
	rows := newModelPickerRows(pickerSubscriptionIDs, prices, "", true, pickerWindowUsage(pickerKW{"day_used_bp": 10_000}), nil)
	if pickerLabelTail(rows.label("example/model-sub"), "example/model-sub") != "" || pickerLabelTail(rows.label("gift/m:free"), "gift/m:free") != "Free" || pickerLabelTail(rows.label("example/model-b"), "example/model-b") != "Large" {
		t.Fatal("tags")
	}
	if !rows.hasTopTier || rows.usage.Blocks("example/model-sub") || !rows.usage.Blocks("example/model-b") {
		t.Fatal("dimming")
	}
}

func TestPickerTheInChatCLIPickerLeavesSubscriptionBilledRowsUntagged(t *testing.T) {
	installPickerEnglish(t)
	servePickerSnapshot(t, nil)
	row := map[string]any{"slug": "nous", "plan_usage": map[string]any{"kind": "usage_window", "subscription_model_ids": []any{"example/model-sub"}},
		"usage_levels": map[string]any{"example/model-sub": "free", "gift/m:free": "free"}}
	rows, _ := AnnotateWindowPlanRows(context.Background(), row, []string{"example/model-sub", "gift/m:free"})
	if !reflect.DeepEqual(pickerFields(rows), [][]string{{"example/model-sub"}, {"gift/m:free", "Free"}}) {
		t.Fatalf("rows %q", rows)
	}
	old := map[string]any{"slug": "nous", "plan_usage": map[string]any{"kind": "usage_window"}, "usage_levels": row["usage_levels"]}
	if rows, _ := AnnotateWindowPlanRows(context.Background(), old, []string{"example/model-sub"}); !reflect.DeepEqual(pickerFields(rows), [][]string{{"example/model-sub", "Free"}}) {
		t.Fatalf("old %q", rows)
	}
}

func TestPickerWindowPlanDefaultIsThePlansFirstListedRecommendation(t *testing.T) {
	ids := []string{"example/model-c", "example/model-b", "example/model-a"}
	servePickerSnapshot(t, pickerWindowAccount("Window Plan", pickerKW{"featured_model_ids": []string{"not/listed", "example/model-a"}}))
	if got := windowPlanDefault(ids, map[string]any{}); got != "example/model-a" {
		t.Fatalf("default %q", got)
	}
	servePickerSnapshot(t, pickerWindowAccount("Window Plan", pickerKW{"featured_model_ids": []string{"example/model-c", "example/model-a"}}))
	if got := windowPlanDefault(ids, map[string]any{"example/model-c": map[string]any{"usage_level": "top_tier"}}); got != "example/model-a" {
		t.Fatalf("large default %q", got)
	}
	servePickerSnapshot(t, nil)
	if got := windowPlanDefault(ids, nil); got != "" {
		t.Fatalf("no plan %q", got)
	}
}

func TestPickerCompactDurationAndResetsIn(t *testing.T) {
	now := int64(1_000_000_000)
	cases := map[int64]string{now + 59_000: "", now - 5_000: "", now + 12*60_000: "12m", now + 5*3_600_000: "5h", now + 2*86_400_000 + 3*3_600_000: "2d 3h", now + 2*86_400_000: "2d"}
	for at, want := range cases {
		if got := CompactDuration(at, now); got != want {
			t.Fatalf("%d: %q want %q", at-now, got, want)
		}
	}
	if FormatResetsIn(pickerI64(now+5*3_600_000), &now) != "resets in 5h" || FormatResetsIn(nil, &now) != "resets soon" {
		t.Fatal("resets in")
	}
}
