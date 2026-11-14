package provider

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	cwNowMs      int64 = 1_800_000_000_000
	cwDayReset         = cwNowMs + 6*3600*1000
	cwWeekReset        = cwNowMs + 3*24*3600*1000
	cwMonthReset       = cwNowMs + 20*24*3600*1000
)

// Allowance list knobs, a negative top or month percent omits that allowance
type cwG struct {
	day, week, top, month                     int
	dayReset, weekReset, topReset, monthReset int64
}

func cwDefaults() cwG {
	return cwG{day: 1_200, week: 900, top: 0, month: -1, dayReset: cwDayReset, weekReset: cwWeekReset, monthReset: cwMonthReset}
}

func cwGaugeList(g cwG) []map[string]any {
	out := []map[string]any{
		{"id": "day", "scope": "all", "label": "Today", "used_bp": g.day, "resets_at_ms": g.dayReset},
		{"id": "week", "scope": "all", "label": "This week", "used_bp": g.week, "resets_at_ms": g.weekReset},
	}
	if g.month >= 0 {
		out = append(out, map[string]any{"id": "month", "scope": "all", "label": "This month", "used_bp": g.month, "resets_at_ms": g.monthReset})
	}
	if g.top >= 0 {
		reset := g.topReset
		if reset == 0 {
			reset = g.dayReset
		}
		out = append(out, map[string]any{"id": "top_tier", "scope": "top_tier", "label": "Large models (monthly)", "used_bp": g.top, "resets_at_ms": reset})
	}
	return out
}

func cwDumps(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func cwGauges(g cwG) []WindowGauge { return ParseGauges(cwDumps(cwGaugeList(g))) }

func cwGauge(s *CreditsState, id string) *WindowGauge {
	for i := range s.Gauges {
		if s.Gauges[i].ID == id {
			return &s.Gauges[i]
		}
	}
	return nil
}

// Header knobs, an empty source or paused omits the header
type cwH struct {
	day, week, top int
	purchased      float64
	source, paused string
	unpaid         bool
	extra          map[string]string
}

func cwHeaders(o cwH) http.Header {
	g := cwDefaults()
	g.day, g.week, g.top = o.day, o.week, o.top
	h := http.Header{}
	for k, v := range map[string]string{
		"x-nous-credits-version":             "1",
		"x-nous-credits-remaining-micros":    ctMicros(o.purchased),
		"x-nous-credits-remaining-usd":       fmt.Sprintf("%.2f", o.purchased),
		"x-nous-credits-subscription-micros": "0", "x-nous-credits-subscription-usd": "0.00", "x-nous-credits-rollover-micros": "0",
		"x-nous-credits-purchased-micros": ctMicros(o.purchased), "x-nous-credits-purchased-usd": fmt.Sprintf("%.2f", o.purchased),
		"x-nous-credits-paid-access": fmt.Sprint(!o.unpaid), "x-nous-credits-as-of-ms": fmt.Sprint(cwNowMs),
		"x-nous-credits-denominator-kind": "usage_window", "x-nous-credits-window-gauges": cwDumps(cwGaugeList(g)),
	} {
		h.Set(k, v)
	}
	if o.source != "" {
		h.Set("x-nous-credits-spend-source", o.source)
	}
	if o.paused != "" {
		h.Set("x-nous-credits-window-paused-reason", o.paused)
	}
	for k, v := range o.extra {
		h.Set(k, v)
	}
	return h
}

func cwDefaultH() cwH { return cwH{day: 1_200, week: 900, source: "included"} }

func cwWith(h http.Header, kv ...string) http.Header {
	out := h.Clone()
	for i := 0; i < len(kv); i += 2 {
		out.Set(kv[i], kv[i+1])
	}
	return out
}

// Header shaped window state like a parse result, the gauge knobs building the server list
func cwState(g cwG, mods ...func(*CreditsState)) *CreditsState {
	s := NewCreditsState()
	s.Version, s.DenominatorKind, s.PaidAccess, s.PurchasedUSD, s.FromHeader = 1, "usage_window", true, "0.00", true
	s.CapturedAt, s.SpendSource, s.Gauges = float64(time.Now().UnixNano())/1e9, "included", cwGauges(g)
	for _, m := range mods {
		m(s)
	}
	return s
}

func cwDay(day, week int) cwG {
	g := cwDefaults()
	g.day, g.week = day, week
	return g
}

func cwTop(top int) cwG {
	g := cwDefaults()
	g.top = top
	return g
}

func cwPurchased(usd string) func(*CreditsState) {
	return func(s *CreditsState) {
		s.PurchasedUSD = usd
		s.PurchasedMicros = int64(math.Round(cwFloat(usd) * 1e6))
	}
}

func cwFloat(s string) float64 {
	var f float64
	fmt.Sscanf(s, "%g", &f)
	return f
}

func cwSource(src string) func(*CreditsState) { return func(s *CreditsState) { s.SpendSource = src } }

func cwKeepGoing(v *bool) func(*CreditsState) {
	return func(s *CreditsState) { s.KeepGoingOnCredits = v }
}

type cwShown struct{ key, level, text string }

func cwRun(s *CreditsState, latch map[string]any, free, top bool) ([]cwShown, []string) {
	show, clear := EvaluateCreditsNotices(s, latch, free, top)
	out := []cwShown{}
	for _, n := range show {
		out = append(out, cwShown{n.Key, n.Level, n.Text})
	}
	if clear == nil {
		clear = []string{}
	}
	return out, clear
}

func cwKeys(show []cwShown) []string {
	out := []string{}
	for _, s := range show {
		out = append(out, s.key)
	}
	return out
}

func cwHasKey(show []cwShown, key string) bool {
	for _, s := range show {
		if s.key == key {
			return true
		}
	}
	return false
}

func cwText(show []cwShown, key string) string {
	for _, s := range show {
		if s.key == key {
			return s.text
		}
	}
	return ""
}

func cwSeenLatch() map[string]any {
	l := NewCreditsLatch()
	l["seen_below_90"] = true
	return l
}

func cwReset(ms int64) string { return FormatWindowReset(ms, 0) }

func cwParse(t *testing.T, h http.Header) *CreditsState {
	t.Helper()
	s := ParseCreditsHeaders(h, "nous")
	if s == nil {
		t.Fatalf("headers did not parse")
	}
	return s
}

func TestCreditsWindowParserFullWindowNoteParses(t *testing.T) {
	s := cwParse(t, cwHeaders(cwH{day: 7_600, week: 4_000, top: 2_500, purchased: 5, source: "payg"}))
	if !s.FromHeader || s.DenominatorKind != "usage_window" || !s.IsUsageWindow() {
		t.Fatalf("%+v", s)
	}
	var got []string
	for _, g := range s.Gauges {
		got = append(got, fmt.Sprintf("%s/%s/%d", g.ID, g.Scope, g.UsedBp))
	}
	if strings.Join(got, ",") != "day/all/7600,week/all/4000,top_tier/top_tier/2500" {
		t.Fatalf("gauges %v", got)
	}
	if *cwGauge(s, "day").ResetsAtMs != cwDayReset || *cwGauge(s, "week").ResetsAtMs != cwWeekReset {
		t.Fatal("resets")
	}
	if s.SpendSource != "payg" || s.PurchasedMicros != 5_000_000 || s.PausedReason != "" || s.AutoTopUpEnabled {
		t.Fatalf("%+v", s)
	}
}

func TestCreditsWindowParserNoteWithoutReadableGaugeListIsAMiss(t *testing.T) {
	h := cwHeaders(cwDefaultH())
	h.Del("x-nous-credits-window-gauges")
	if ParseCreditsHeaders(h, "nous") != nil {
		t.Fatal("missing list must miss")
	}
	for _, bad := range []string{"not json", "{}", "[]"} {
		if ParseCreditsHeaders(cwWith(h, "x-nous-credits-window-gauges", bad), "nous") != nil {
			t.Fatalf("%s must miss", bad)
		}
	}
	topOnly := []map[string]any{}
	for _, g := range cwGaugeList(cwDefaults()) {
		if g["scope"] == "top_tier" {
			topOnly = append(topOnly, g)
		}
	}
	if ParseCreditsHeaders(cwWith(h, "x-nous-credits-window-gauges", cwDumps(topOnly)), "nous") != nil {
		t.Fatal("top tier only must miss")
	}
}

func TestCreditsWindowParserServerListDecidesWhichAllowancesExist(t *testing.T) {
	gauges := []map[string]any{{"id": "five_hour", "scope": "all", "label": "Next 5 hours", "used_bp": 10_000, "resets_at_ms": cwNowMs + 3600_000}}
	gauges = append(gauges, cwGaugeList(cwDefaults())...)
	gauges = append(gauges, map[string]any{"id": "week", "scope": "all", "label": "dup", "used_bp": 1, "resets_at_ms": 1},
		map[string]any{"id": "x", "scope": "per_model", "label": "x", "used_bp": 1, "resets_at_ms": 1},
		map[string]any{"id": "y", "scope": "all", "label": "y", "used_bp": 10_001, "resets_at_ms": 1})
	s := cwParse(t, cwWith(cwHeaders(cwDefaultH()), "x-nous-credits-window-gauges", cwDumps(gauges)))
	var ids []string
	for _, g := range s.Gauges {
		ids = append(ids, g.ID)
	}
	if strings.Join(ids, ",") != "five_hour,day,week,top_tier" || !s.WindowExhausted() || s.BindingGauge().ID != "five_hour" ||
		*s.WindowResetsAtMs() != cwNowMs+3600_000 || cwGauge(s, "week").Label != "This week" {
		t.Fatalf("ids %v", ids)
	}
	st := cwState(cwDefaults(), cwSource(""))
	st.Gauges = s.Gauges
	show, _ := cwRun(st, NewCreditsLatch(), false, false)
	if !strings.HasPrefix(show[len(show)-1].text, "✕ You've hit your Next 5 hours limit") {
		t.Fatalf("%v", show)
	}
}

func TestCreditsWindowParserMonthAndTopTierResetComeFromTheList(t *testing.T) {
	topReset := cwNowMs + 20*86_400_000
	g := cwDefaults()
	g.month, g.topReset = 3_300, topReset
	s := cwParse(t, cwWith(cwHeaders(cwDefaultH()), "x-nous-credits-window-gauges", cwDumps(cwGaugeList(g))))
	if cwGauge(s, "month").UsedBp != 3_300 || *s.TopTierResetsAtMs() != topReset {
		t.Fatal("month and top tier reset")
	}
	old := cwParse(t, cwHeaders(cwDefaultH()))
	if !old.IsUsageWindow() || cwGauge(old, "month") != nil {
		t.Fatal("no month gauge")
	}
}

func TestCreditsWindowParserListWithoutTopTierReadsAsNotUsed(t *testing.T) {
	s := cwParse(t, cwHeaders(cwH{day: 1_200, week: 900, top: -1}))
	if _, ok := s.TopTierUsedBp(); ok || s.TopTierExhausted() || s.SpendSource != "" {
		t.Fatalf("%+v", s)
	}
	s = cwParse(t, cwWith(cwHeaders(cwDefaultH()), "x-nous-credits-spend-source", "wallet"))
	if s.SpendSource != "" {
		t.Fatal("outside the vocabulary is absent")
	}
}

func TestCreditsWindowParserPausedReasonHeader(t *testing.T) {
	s := cwParse(t, cwHeaders(cwH{day: 1_200, week: 900, unpaid: true, paused: "payment_past_due"}))
	if s.PausedReason != "payment_past_due" || s.PaidAccess {
		t.Fatalf("%+v", s)
	}
}

func TestCreditsWindowParserTopUpSettingHeader(t *testing.T) {
	read := func(v string) *CreditsState {
		o := cwH{day: 1_200, week: 900, purchased: 5, source: "included"}
		if v != "" {
			o.extra = map[string]string{"x-nous-credits-keep-going-on-credits": v}
		}
		return cwParse(t, cwHeaders(o))
	}
	on, off, old, bad := read("on"), read("OFF"), read(""), read("maybe")
	if on.KeepGoingOnCredits == nil || !*on.KeepGoingOnCredits || off.KeepGoingOnCredits == nil || *off.KeepGoingOnCredits ||
		old.KeepGoingOnCredits != nil || bad.KeepGoingOnCredits != nil {
		t.Fatal("keep going values")
	}
	if !on.CreditsPay() || !old.CreditsPay() || off.CreditsPay() {
		t.Fatal("credits pay")
	}
}

func TestCreditsWindowKeepGoingOffWithABalanceStopsAndNamesTheSetting(t *testing.T) {
	no, yes := false, true
	s := cwState(cwDay(10_000, 900), cwPurchased("5.00"), cwSource(""), cwKeepGoing(&no))
	show, _ := cwRun(s, cwSeenLatch(), false, false)
	var stops []string
	for _, n := range show {
		if n.key == CreditsWindowLimitKey {
			stops = append(stops, n.text)
		}
	}
	if want := "✕ You've hit your daily limit · resets " + cwReset(cwDayReset) + " · turn on Keep going in /topup"; len(stops) != 1 || stops[0] != want {
		t.Fatalf("%v", stops)
	}
	for _, kg := range []*bool{&yes, nil} {
		show, _ := cwRun(cwState(cwDay(10_000, 900), cwPurchased("5.00"), cwSource("payg"), cwKeepGoing(kg)), cwSeenLatch(), false, false)
		if cwHasKey(show, CreditsWindowLimitKey) {
			t.Fatalf("keep going %v must not stop", kg)
		}
	}
}

func TestCreditsWindowKeepGoingOffTopTierRefusalNamesTheSetting(t *testing.T) {
	no := false
	show, _ := cwRun(cwState(cwTop(10_000), cwPurchased("5.00"), cwSource(""), cwKeepGoing(&no)), NewCreditsLatch(), false, false)
	stop := cwText(show, CreditsWindowLimitKey)
	if !strings.HasPrefix(stop, "✕ You've hit your monthly large-model limit") || !strings.HasSuffix(stop, " · turn on Keep going in /topup") {
		t.Fatalf("%q", stop)
	}
}

func cwMicros(n int64) func(*CreditsState) { return func(s *CreditsState) { s.PurchasedMicros = n } }

func cwPaused(reason string) func(*CreditsState) {
	return func(s *CreditsState) { s.PausedReason = reason }
}

func cwUnpaid(s *CreditsState) { s.PaidAccess = false }

func cwAutoTopUp(s *CreditsState) { s.AutoTopUpEnabled = true }

func cwEqualStrings(a, b []string) bool {
	return strings.Join(a, "\x00") == strings.Join(b, "\x00") && len(a) == len(b)
}

func TestCreditsWindowParserHeaderNamesAreCaseInsensitive(t *testing.T) {
	upper := http.Header{}
	for k, v := range cwHeaders(cwDefaultH()) {
		upper[strings.ToUpper(k)] = v
	}
	if ParseCreditsHeaders(upper, "nous") == nil {
		t.Fatal("upper case names must parse")
	}
}

func TestCreditsWindowParserLegacyNotesIgnoreWindowHeaders(t *testing.T) {
	s := cwParse(t, cwWith(cwHeaders(cwDefaultH()), "x-nous-credits-denominator-kind", "none"))
	if s.IsUsageWindow() || len(s.Gauges) != 0 || s.SpendSource != "" {
		t.Fatalf("%+v", s)
	}
}

func TestCreditsWindowParserUnknownKindIsStillAMiss(t *testing.T) {
	if ParseCreditsHeaders(cwWith(cwHeaders(cwDefaultH()), "x-nous-credits-denominator-kind", "usage_windows"), "nous") != nil {
		t.Fatal("unknown kind must miss")
	}
}

func TestCreditsWindowParserNoteMayDropTheCreditGrantFields(t *testing.T) {
	h := cwHeaders(cwH{day: 1_200, week: 900, purchased: 5, source: "included"})
	for _, name := range []string{"remaining-micros", "remaining-usd", "subscription-micros", "subscription-usd", "rollover-micros", "as-of-ms"} {
		h.Del("x-nous-credits-" + name)
	}
	s := cwParse(t, h)
	if !s.IsUsageWindow() || cwGauge(s, "day").UsedBp != 1_200 || s.PurchasedMicros != 5_000_000 || s.RemainingMicros != 0 || s.SubscriptionUSD != "" {
		t.Fatalf("%+v", s)
	}
	if ParseCreditsHeaders(cwWith(h, "x-nous-credits-denominator-kind", "none"), "nous") != nil {
		t.Fatal("a credit note without grant fields is a miss")
	}
	bare := h.Clone()
	for k := range bare {
		if strings.HasPrefix(strings.ToLower(k), "x-nous-credits-purchased-") {
			bare.Del(k)
		}
	}
	if s := cwParse(t, bare); !s.IsUsageWindow() || s.PurchasedMicros != 0 {
		t.Fatalf("%+v", s)
	}
	if ParseCreditsHeaders(cwWith(bare, "x-nous-credits-denominator-kind", "none"), "nous") != nil {
		t.Fatal("bare credit note is a miss")
	}
	if ParseCreditsHeaders(cwWith(h, "x-nous-credits-rollover-micros", "-1"), "nous") != nil {
		t.Fatal("negative rollover is a miss")
	}
}

func TestCreditsWindowDerivedUsedFractionIsTheFullerWindow(t *testing.T) {
	for _, c := range []struct {
		day, week int
		want      float64
	}{{3_000, 9_100, 0.91}, {7_600, 4_000, 0.76}} {
		if f, ok := cwState(cwDay(c.day, c.week)).UsedFraction(); !ok || math.Abs(f-c.want) > 1e-9 {
			t.Fatalf("%v %v", f, ok)
		}
	}
}

func TestCreditsWindowDerivedBindingWindowAndItsReset(t *testing.T) {
	day := cwState(cwDay(7_600, 4_000))
	week := cwState(cwDay(3_000, 9_100))
	if day.BindingGauge().ID != "day" || *day.WindowResetsAtMs() != cwDayReset || week.BindingGauge().ID != "week" ||
		*week.WindowResetsAtMs() != cwWeekReset || cwState(cwDay(5_000, 5_000)).BindingGauge().ID != "day" {
		t.Fatal("binding gauge")
	}
}

func TestCreditsWindowDerivedExhaustedFlags(t *testing.T) {
	if !cwState(cwDay(10_000, 900)).WindowExhausted() || !cwState(cwDay(1_200, 10_000)).WindowExhausted() ||
		cwState(cwDay(9_999, 900)).WindowExhausted() || !cwState(cwTop(10_000)).TopTierExhausted() ||
		cwState(cwTop(-1)).TopTierExhausted() || cwState(cwDefaults(), cwPaused("payment_past_due")).WindowExhausted() {
		t.Fatal("exhausted flags")
	}
}

func TestCreditsWindowDerivedImplausibleResetStampReadsAsUnknown(t *testing.T) {
	far := time.Now().UnixMilli() + 10*366*86_400_000
	gauges := ParseGauges(cwDumps([]map[string]any{{"id": "day", "scope": "all", "used_bp": 100, "resets_at_ms": far},
		{"id": "week", "scope": "all", "used_bp": 100, "resets_at_ms": cwWeekReset}}))
	if gauges[0].ResetsAtMs != nil || *gauges[1].ResetsAtMs != cwWeekReset {
		t.Fatalf("%+v", gauges)
	}
}

func TestCreditsWindowDerivedNoGaugesIsNotAUsageWindow(t *testing.T) {
	s := cwState(cwDefaults())
	s.Gauges = nil
	if _, ok := s.UsedFraction(); s.IsUsageWindow() || ok || s.BindingGauge() != nil {
		t.Fatal("no gauges")
	}
}

func TestCreditsWindowDerivedDepletedStillKeysOffPaidAccess(t *testing.T) {
	if cwState(cwDay(10_000, 900)).Depleted() || !cwState(cwDefaults(), cwUnpaid).Depleted() {
		t.Fatal("depleted")
	}
}

func TestCreditsWindowFormatReset(t *testing.T) {
	in5h := cwNowMs + 5*3600*1000
	if got := FormatWindowReset(in5h, cwNowMs); got != time.UnixMilli(in5h).Local().Format("15:04") {
		t.Fatalf("clock %q", got)
	}
	if got := FormatWindowReset(cwWeekReset, cwNowMs); got != time.UnixMilli(cwWeekReset).Local().Format("Mon 15:04") {
		t.Fatalf("weekday %q", got)
	}
	top := cwNowMs + 20*86_400_000
	stamp := time.UnixMilli(top).Local()
	if got := FormatWindowReset(top, cwNowMs); got != fmt.Sprintf("%s %d", stamp.Format("Jan"), stamp.Day()) {
		t.Fatalf("date %q", got)
	}
	if FormatWindowReset(int64(math.MaxInt64), cwNowMs) != "the next reset" || FormatWindowReset(cwNowMs-1, cwNowMs) != "shortly" ||
		FormatWindowReset(0, 0) != "the next reset" {
		t.Fatal("implausible, passed and unknown")
	}
}

func TestCreditsWindowApproachHealthyAnd75AreSilent(t *testing.T) {
	latch := NewCreditsLatch()
	for _, s := range []*CreditsState{cwState(cwDefaults()), cwState(cwDay(7_600, 8_900))} {
		if show, clear := cwRun(s, latch, false, false); len(show) != 0 || len(clear) != 0 {
			t.Fatalf("%v %v", show, clear)
		}
	}
}

func TestCreditsWindowApproachEachAllowanceWarnsAt90WithItsOwnKeyAndReset(t *testing.T) {
	show, _ := cwRun(cwState(cwDay(9_100, 9_300)), NewCreditsLatch(), false, false)
	if !cwEqualStrings(cwKeys(show), []string{WindowApproachKey("day"), WindowApproachKey("week")}) {
		t.Fatalf("%v", show)
	}
	if show[0].text != "⚠ 91% of your daily limit used · resets "+cwReset(cwDayReset) ||
		show[1].text != "⚠ 93% of your weekly limit used · resets "+cwReset(cwWeekReset) || strings.Contains(show[0].text, "$") {
		t.Fatalf("%v", show)
	}
	for _, s := range show {
		if s.level != "warn" {
			t.Fatalf("%v", s)
		}
	}
}

func TestCreditsWindowApproachOncePerResetNotPerRequest(t *testing.T) {
	latch := NewCreditsLatch()
	cwRun(cwState(cwDay(9_100, 900)), latch, false, false)
	if show, clear := cwRun(cwState(cwDay(9_600, 900)), latch, false, false); len(show)+len(clear) != 0 {
		t.Fatalf("%v %v", show, clear)
	}
	g := cwDay(9_100, 900)
	g.dayReset = cwDayReset + 86_400_000
	if show, _ := cwRun(cwState(g), latch, false, false); !cwEqualStrings(cwKeys(show), []string{WindowApproachKey("day")}) {
		t.Fatalf("%v", show)
	}
}

func TestCreditsWindowApproachTTLHeadsUpNeverNeedsAClear(t *testing.T) {
	latch := NewCreditsLatch()
	notices, _ := EvaluateCreditsNotices(cwState(cwDay(9_100, 900)), latch, false, false)
	if notices[0].Kind != "ttl" || notices[0].TTLMs == 0 {
		t.Fatalf("%+v", notices[0])
	}
	show, clear := cwRun(cwState(cwDay(10_000, 900), cwSource("")), latch, false, false)
	if len(clear) != 0 || !cwEqualStrings(cwKeys(show), []string{CreditsWindowLimitKey}) {
		t.Fatalf("%v %v", show, clear)
	}
}

func TestCreditsWindowApproachTopTierWarnsOnlyOnATopTierModel(t *testing.T) {
	s := cwState(cwTop(9_200))
	if show, clear := cwRun(s, NewCreditsLatch(), false, false); len(show)+len(clear) != 0 {
		t.Fatalf("%v", show)
	}
	show, _ := cwRun(s, NewCreditsLatch(), false, true)
	if show[0].key != WindowApproachKey("top_tier") || !strings.HasPrefix(show[0].text, "⚠ 92% of your monthly large-model limit used") {
		t.Fatalf("%v", show)
	}
}

func TestCreditsWindowApproachFreeModelWarnsAboutNothing(t *testing.T) {
	if show, clear := cwRun(cwState(cwDay(9_500, 900)), NewCreditsLatch(), true, false); len(show)+len(clear) != 0 {
		t.Fatalf("%v", show)
	}
}

func TestCreditsWindowApproachNewAllowanceWarnsWithTheServerLabel(t *testing.T) {
	list := append([]map[string]any{{"id": "five_hour", "scope": "all", "label": "5-hour", "used_bp": 9_400, "resets_at_ms": cwDayReset}},
		cwGaugeList(cwDefaults())...)
	s := cwState(cwDefaults())
	s.Gauges = ParseGauges(cwDumps(list))
	show, _ := cwRun(s, NewCreditsLatch(), false, false)
	if show[0].key != WindowApproachKey("five_hour") || !strings.Contains(show[0].text, "of your 5-hour limit used") {
		t.Fatalf("%v", show)
	}
}

func TestCreditsWindowApproachShowsEvenWithTopUpCredits(t *testing.T) {
	show, _ := cwRun(cwState(cwDay(9_000, 900), cwPurchased("5.00")), NewCreditsLatch(), false, false)
	if !cwEqualStrings(cwKeys(show), []string{WindowApproachKey("day")}) {
		t.Fatalf("%v", show)
	}
}

func TestCreditsWindowApproachStaleDollarBandIsClearedOnAWindowPlan(t *testing.T) {
	latch := NewCreditsLatch()
	latch["active"].(map[string]bool)[CreditsUsageKey] = true
	latch["texts"].(map[string]string)[CreditsUsageKey] = "x"
	if _, clear := cwRun(cwState(cwDefaults()), latch, false, false); !cwEqualStrings(clear, []string{CreditsUsageKey}) {
		t.Fatalf("%v", clear)
	}
}

func TestCreditsWindowApproachGrantSpentNeverFiresOnAWindow(t *testing.T) {
	latch := NewCreditsLatch()
	latch["seen_grant_unspent"] = true
	if show, _ := cwRun(cwState(cwDay(10_000, 900), cwMicros(5_000_000), cwSource("payg")), latch, false, false); cwHasKey(show, CreditsGrantSpentKey) {
		t.Fatalf("%v", show)
	}
}

func TestCreditsWindowPaygWindowExhaustedWithTopUp(t *testing.T) {
	latch := NewCreditsLatch()
	cwRun(cwState(cwDefaults()), latch, false, false)
	show, _ := cwRun(cwState(cwDay(10_000, 900), cwPurchased("5.00"), cwSource("payg")), latch, false, false)
	if !cwHasKey(show, CreditsWindowPaygKey) || cwHasKey(show, CreditsWindowLimitKey) {
		t.Fatalf("%v", show)
	}
	if text := cwText(show, CreditsWindowPaygKey); text != "• You've hit your daily limit · resets "+cwReset(cwDayReset)+" · now using credits" {
		t.Fatalf("%q", text)
	}
}

func TestCreditsWindowPaygAutoTopUpIsAppendedWhenOn(t *testing.T) {
	show, _ := cwRun(cwState(cwDay(10_000, 900), cwMicros(5_000_000), cwSource("payg"), cwAutoTopUp), NewCreditsLatch(), false, false)
	if !strings.HasSuffix(cwText(show, CreditsWindowPaygKey), " · now using credits · auto top-up is on") {
		t.Fatalf("%v", show)
	}
}

func TestCreditsWindowPaygTopTierSliceHasItsOwnCopy(t *testing.T) {
	top := cwNowMs + 20*86_400_000
	g := cwDay(4_000, 900)
	g.top, g.topReset = 10_000, top
	show, _ := cwRun(cwState(g, cwMicros(5_000_000), cwSource("payg")), NewCreditsLatch(), false, false)
	if want := "• You've hit your monthly large-model limit · resets " + cwReset(top) + " · large models now use credits"; cwText(show, CreditsWindowPaygKey) != want {
		t.Fatalf("%v", show)
	}
}

func TestCreditsWindowPaygOneShotDoesNotRepeatAndClearsWhenIncludedAgain(t *testing.T) {
	latch := NewCreditsLatch()
	payg := cwState(cwDay(10_000, 900), cwMicros(5_000_000), cwSource("payg"))
	cwRun(payg, latch, false, false)
	if show, clear := cwRun(payg, latch, false, false); len(show)+len(clear) != 0 {
		t.Fatalf("%v %v", show, clear)
	}
	show, clear := cwRun(cwState(cwDay(500, 500), cwMicros(5_000_000), cwSource("included")), latch, false, false)
	if !strings.Contains(strings.Join(clear, ","), CreditsWindowPaygKey) || cwHasKey(show, CreditsWindowPaygKey) {
		t.Fatalf("%v %v", show, clear)
	}
}

func TestCreditsWindowPaygWithoutTopUpIsNotPayg(t *testing.T) {
	if show, _ := cwRun(cwState(cwDay(10_000, 900), cwSource("payg")), NewCreditsLatch(), false, false); cwHasKey(show, CreditsWindowPaygKey) {
		t.Fatalf("%v", show)
	}
}

func cwFromHeader(v bool) func(*CreditsState) { return func(s *CreditsState) { s.FromHeader = v } }

func TestCreditsWindowHardStopExhaustedWithEmptyWallets(t *testing.T) {
	show, _ := cwRun(cwState(cwDay(10_000, 6_000), cwSource("included")), NewCreditsLatch(), false, false)
	last := show[len(show)-1]
	if last.key != CreditsWindowLimitKey || last.level != "error" || cwHasKey(show, CreditsWindowPaygKey) ||
		last.text != "✕ You've hit your daily limit · resets "+cwReset(cwDayReset)+" · /topup to add credits" {
		t.Fatalf("%v", show)
	}
}

func TestCreditsWindowHardStopNamesTheMonthAndItsResetWhenItBinds(t *testing.T) {
	g := cwDay(10_000, 6_000)
	g.month = 10_000
	s := cwState(g, cwSource(""))
	if s.BindingGauge().ID != "month" || *s.WindowResetsAtMs() != cwMonthReset {
		t.Fatal("month binds")
	}
	show, _ := cwRun(s, NewCreditsLatch(), false, false)
	if !strings.HasPrefix(show[len(show)-1].text, "✕ You've hit your monthly limit · resets "+cwReset(cwMonthReset)+" · ") {
		t.Fatalf("%v", show)
	}
	g = cwDay(10_000, 900)
	g.month = 4_000
	if cwState(g).BindingGauge().ID != "day" {
		t.Fatal("day binds")
	}
}

func TestCreditsWindowHardStopNamesTheWeekWhenItBinds(t *testing.T) {
	for _, g := range []cwG{cwDay(6_000, 10_000), cwDay(10_000, 10_000)} {
		show, _ := cwRun(cwState(g, cwSource("")), NewCreditsLatch(), false, false)
		if !strings.HasPrefix(show[len(show)-1].text, "✕ You've hit your weekly limit · resets "+cwReset(cwWeekReset)+" · ") {
			t.Fatalf("%v", show)
		}
	}
}

func TestCreditsWindowHardStopPaymentPausedOffersTheCardAndCredits(t *testing.T) {
	show, _ := cwRun(cwState(cwDefaults(), cwUnpaid, cwPaused("payment_past_due"), cwSource("")), NewCreditsLatch(), false, false)
	text := show[0].text
	if !cwEqualStrings(cwKeys(show), []string{CreditsDepletedKey}) || !strings.Contains(text, "payment failed") ||
		!strings.Contains(text, "update your card") || !strings.Contains(text, "add credits in /topup") || strings.Contains(text, "Keep going on credits") {
		t.Fatalf("%v", show)
	}
}

func TestCreditsWindowHardStopPauseForAnotherReasonNeverClaimsAPaymentFailed(t *testing.T) {
	show, _ := cwRun(cwState(cwDefaults(), cwUnpaid, cwPaused("unknown"), cwSource("")), NewCreditsLatch(), false, false)
	if !cwEqualStrings(cwKeys(show), []string{CreditsDepletedKey}) || strings.Contains(show[0].text, "payment") ||
		!strings.Contains(show[0].text, "add credits in /topup") {
		t.Fatalf("%v", show)
	}
	no, paid, zero := false, true, 0.0
	info := &NousPortalAccountInfo{LoggedIn: true, PaidServiceAccess: &paid,
		UsageWindow:           &NousUsageWindowInfo{Gauges: cwGauges(cwDay(100, 50)), AutoTopUpEnabled: &no, PausedReason: "unknown"},
		PaidServiceAccessInfo: &NousPaidServiceAccessInfo{PurchasedCreditsRemaining: &zero}}
	lines := strings.Join(RenderAccountUsageLines(BuildNousCreditsSnapshot(info), false), "\n")
	if !strings.Contains(lines, accountUsagePausedOtherLine) || strings.Contains(lines, accountUsagePausedLine) {
		t.Fatalf("%s", lines)
	}
	if g := FormatUsageWindowBlockGuidance(info); !strings.HasPrefix(g, "Included usage is paused, or add credits") {
		t.Fatalf("%q", g)
	}
}

func TestCreditsWindowHardStopPaymentPausedWinsOverExhaustedAndSurvivesAFreeModel(t *testing.T) {
	s := cwState(cwDay(10_000, 900), cwUnpaid, cwPaused("payment_past_due"), cwSource(""))
	for _, free := range []bool{false, true} {
		if show, _ := cwRun(s, NewCreditsLatch(), free, false); !strings.Contains(show[0].text, "payment failed") {
			t.Fatalf("free %v: %v", free, show)
		}
	}
}

func TestCreditsWindowHardStopPausedWithTopUpIsNotAStop(t *testing.T) {
	show, _ := cwRun(cwState(cwDefaults(), cwPaused("payment_past_due"), cwMicros(5_000_000), cwSource("payg")), NewCreditsLatch(), false, false)
	if cwHasKey(show, CreditsDepletedKey) || !cwHasKey(show, CreditsWindowPaygKey) {
		t.Fatalf("%v", show)
	}
}

func TestCreditsWindowHardStopTopTierBlockedOnA402(t *testing.T) {
	top := cwNowMs + 20*86_400_000
	g := cwDay(4_000, 900)
	g.top, g.topReset = 10_000, top
	show, _ := cwRun(cwState(g, cwSource("")), NewCreditsLatch(), false, false)
	last := show[len(show)-1]
	if last.key != CreditsWindowLimitKey || last.text != "✕ You've hit your monthly large-model limit · resets "+cwReset(top)+" · /topup to add credits" {
		t.Fatalf("%v", show)
	}
	g.topReset = 0
	old, _ := cwRun(cwState(g, cwSource("")), NewCreditsLatch(), false, false)
	if !strings.Contains(old[len(old)-1].text, "resets "+cwReset(cwDayReset)+" ·") {
		t.Fatalf("%v", old)
	}
}

func TestCreditsWindowHardStopTopTierBlockedMentionsAutoTopUp(t *testing.T) {
	g := cwDay(4_000, 900)
	g.top = 10_000
	show, _ := cwRun(cwState(g, cwSource(""), cwAutoTopUp), NewCreditsLatch(), false, false)
	if !strings.HasSuffix(show[len(show)-1].text, " · /topup to add credits · auto top-up is on") {
		t.Fatalf("%v", show)
	}
}

func TestCreditsWindowHardStopTopTierUsedButAdmittedOrSeededIsNotAStop(t *testing.T) {
	g := cwDay(4_000, 900)
	g.top = 10_000
	for _, s := range []*CreditsState{cwState(g, cwSource("included")), cwState(g, cwSource(""), cwFromHeader(false))} {
		if show, _ := cwRun(s, NewCreditsLatch(), false, false); cwHasKey(show, CreditsWindowLimitKey) {
			t.Fatalf("%v", show)
		}
	}
}

func TestCreditsWindowHardStopPortalPaidAccessFalseWithoutAPauseIsTheLegacyDepletedLine(t *testing.T) {
	if show, _ := cwRun(cwState(cwDefaults(), cwUnpaid, cwSource("")), NewCreditsLatch(), false, false); show[0].text != "✕ Credit access paused · run /topup to top up" {
		t.Fatalf("%v", show)
	}
}

func TestCreditsWindowHardStopFreeModelSuppressesTheWindowStopWithoutRestored(t *testing.T) {
	latch := NewCreditsLatch()
	cwRun(cwState(cwDay(10_000, 900), cwSource("")), latch, false, false)
	show, clear := cwRun(cwState(cwDay(10_000, 900), cwSource("")), latch, true, false)
	if !cwEqualStrings(clear, []string{CreditsWindowLimitKey}) || len(show) != 0 {
		t.Fatalf("%v %v", show, clear)
	}
}

func TestCreditsWindowHardStopResetRestores(t *testing.T) {
	latch := NewCreditsLatch()
	cwRun(cwState(cwDay(10_000, 900), cwSource("")), latch, false, false)
	show, clear := cwRun(cwState(cwDay(100, 100), cwSource("included")), latch, false, false)
	var restored []cwShown
	for _, n := range show {
		if n.key == CreditsRestoredKey {
			restored = append(restored, n)
		}
	}
	if !strings.Contains(strings.Join(clear, ","), CreditsWindowLimitKey) || len(restored) != 1 || restored[0].level != "success" {
		t.Fatalf("%v %v", show, clear)
	}
}

func TestCreditsWindowHardStopTopUpMidBlockRestoresThenPayg(t *testing.T) {
	latch := NewCreditsLatch()
	cwRun(cwState(cwDay(10_000, 900), cwSource("")), latch, false, false)
	show, clear := cwRun(cwState(cwDay(10_000, 900), cwMicros(5_000_000), cwSource("payg")), latch, false, false)
	if !strings.Contains(strings.Join(clear, ","), CreditsWindowLimitKey) || !cwEqualStrings(cwKeys(show), []string{CreditsRestoredKey, CreditsWindowPaygKey}) {
		t.Fatalf("%v %v", show, clear)
	}
}

func TestCreditsWindowHardStopStopTextChangeReEmits(t *testing.T) {
	latch := NewCreditsLatch()
	cwRun(cwState(cwDay(10_000, 900), cwSource("")), latch, false, false)
	show, clear := cwRun(cwState(cwDay(10_000, 900), cwUnpaid, cwPaused("payment_past_due"), cwSource("")), latch, false, false)
	if !cwEqualStrings(clear, []string{CreditsWindowLimitKey}) || show[0].key != CreditsDepletedKey || !strings.Contains(show[0].text, "payment failed") ||
		cwHasKey(show, CreditsRestoredKey) {
		t.Fatalf("%v %v", show, clear)
	}
}

func TestCreditsWindowPlanFlipWindowPolicyClearsAStaleGrantLine(t *testing.T) {
	latch := NewCreditsLatch()
	latch["active"].(map[string]bool)[CreditsGrantSpentKey] = true
	_, clear := cwRun(cwState(cwDefaults()), latch, false, false)
	if !strings.Contains(strings.Join(clear, ","), CreditsGrantSpentKey) || latch["active"].(map[string]bool)[CreditsGrantSpentKey] {
		t.Fatalf("%v", clear)
	}
}

func cwLegacy(kind string) *CreditsState {
	s := NewCreditsState()
	s.Version, s.DenominatorKind, s.PaidAccess, s.FromHeader, s.CapturedAt = 1, kind, true, true, float64(time.Now().Unix())
	return s
}

func TestCreditsWindowPlanFlipLegacyPolicyClearsAStalePaygLine(t *testing.T) {
	latch := NewCreditsLatch()
	latch["active"].(map[string]bool)[CreditsWindowPaygKey] = true
	if _, clear := EvaluateCreditsNotices(cwLegacy("none"), latch, false, false); !strings.Contains(strings.Join(clear, ","), CreditsWindowPaygKey) {
		t.Fatalf("%v", clear)
	}
}

func TestCreditsWindowPlanFlipLegacyPathIsUnchangedForCreditGrantStates(t *testing.T) {
	s := cwLegacy("subscription_cap")
	limit, limitUSD := int64(20_000_000), "20.00"
	s.SubscriptionMicros, s.SubscriptionUSD, s.SubscriptionLimitMicros, s.SubscriptionLimitUSD = 2_000_000, "2.00", &limit, &limitUSD
	show, _ := EvaluateCreditsNotices(s, cwSeenLatch(), false, false)
	if len(show) != 1 || show[0].Key != CreditsUsageKey || show[0].Text != "⚠ You've used $18.00 of your $20.00 cap" {
		t.Fatalf("%+v", show)
	}
}

func cwSeedInfo(uw *NousUsageWindowInfo, paid bool, purchased float64) *NousPortalAccountInfo {
	zero, zeroSub := 0.0, 0.0
	return &NousPortalAccountInfo{PaidServiceAccess: &paid,
		PaidServiceAccessInfo: &NousPaidServiceAccessInfo{TotalUsableCredits: &purchased, SubscriptionCreditsRemaining: &zero, PurchasedCreditsRemaining: &purchased},
		Subscription:          &NousPortalSubscriptionInfo{MonthlyCredits: &zeroSub, RolloverCredits: &zeroSub}, UsageWindow: uw}
}

func TestCreditsWindowSeedAccountUsageWindowSeedsAWindowState(t *testing.T) {
	on := true
	g := cwDay(7_600, 4_000)
	g.top = 10_000
	s := creditsStateFromAccount(cwSeedInfo(&NousUsageWindowInfo{Gauges: cwGauges(g), AutoTopUpEnabled: &on}, true, 5))
	f, _ := s.UsedFraction()
	top, ok := s.TopTierUsedBp()
	if !s.IsUsageWindow() || s.FromHeader || math.Abs(f-0.76) > 1e-9 || !ok || top != 10_000 || !s.AutoTopUpEnabled ||
		s.PurchasedMicros != 5_000_000 || s.SubscriptionMicros != 0 || s.SubscriptionLimitMicros != nil || s.SpendSource != "" ||
		s.PausedReason != "" || *s.TopTierResetsAtMs() != cwDayReset {
		t.Fatalf("%+v", s)
	}
}

func TestCreditsWindowSeedCarriesTheMonthlyTopTierResetAndCap(t *testing.T) {
	top := cwNowMs + 20*86_400_000
	g := cwDay(0, 0)
	g.topReset = top
	if s := creditsStateFromAccount(cwSeedInfo(&NousUsageWindowInfo{Gauges: cwGauges(g)}, true, 5)); *s.TopTierResetsAtMs() != top {
		t.Fatal("top tier reset")
	}
	g = cwDay(0, 0)
	g.month = 10_000
	if s := creditsStateFromAccount(cwSeedInfo(&NousUsageWindowInfo{Gauges: cwGauges(g)}, true, 5)); !s.WindowExhausted() || *s.WindowResetsAtMs() != cwMonthReset {
		t.Fatal("monthly cap")
	}
}

func TestCreditsWindowSeedPaused(t *testing.T) {
	g := cwDay(0, 0)
	g.top = -1
	s := creditsStateFromAccount(cwSeedInfo(&NousUsageWindowInfo{Gauges: cwGauges(g), PausedReason: "payment_past_due"}, false, 0))
	if _, ok := s.TopTierUsedBp(); s.PausedReason != "payment_past_due" || s.PaidAccess || ok || s.TopTierExhausted() {
		t.Fatalf("%+v", s)
	}
}

func TestCreditsWindowSeedWithoutAGaugeListFallsBackToTheLegacyMapping(t *testing.T) {
	for _, uw := range []*NousUsageWindowInfo{{Gauges: nil}, {Gauges: []WindowGauge{}}} {
		if s := creditsStateFromAccount(cwSeedInfo(uw, true, 5)); s == nil || s.DenominatorKind != "none" || s.IsUsageWindow() {
			t.Fatalf("%+v", s)
		}
	}
	info := cwSeedInfo(nil, true, 5)
	twenty, zero := 20.0, 0.0
	info.Subscription = &NousPortalSubscriptionInfo{MonthlyCredits: &twenty, RolloverCredits: &zero}
	if s := creditsStateFromAccount(info); s.DenominatorKind != "subscription_cap" || s.SubscriptionLimitMicros == nil || *s.SubscriptionLimitMicros != 20_000_000 {
		t.Fatalf("%+v", s)
	}
}

func TestCreditsWindowSeedSessionOpeningPast90GetsTheHeadsUp(t *testing.T) {
	no := false
	state := creditsStateFromAccount(cwSeedInfo(&NousUsageWindowInfo{Gauges: cwGauges(cwDay(9_200, 4_000)), AutoTopUpEnabled: &no}, true, 5))
	c := NewCreditsCapture(nil, "nous", "", "", "cw-seed-"+t.Name())
	var emitted []AgentNotice
	c.Notice = func(n AgentNotice) { emitted = append(emitted, n) }
	hydrateSeedState(c, state)
	if len(emitted) != 1 || emitted[0].Key != WindowApproachKey("day") || !strings.Contains(emitted[0].Text, "92%") {
		t.Fatalf("%+v", emitted)
	}
}

func cwPlanState(dayBp, weekBp, topBp int, purchased, source string, autoTopUp bool, paused string, paid bool, keepGoing *bool) *CreditsState {
	now := time.Now().UnixMilli()
	dayReset, weekReset := now+6*3600*1000, now+3*24*3600*1000
	micros := MustDecimal(purchased)
	scaled, _ := micros.rescale(-6).coef.Int64(), 0
	s := NewCreditsState()
	s.Version, s.DenominatorKind, s.PaidAccess = 1, "usage_window", paid
	s.RemainingMicros, s.RemainingUSD, s.PurchasedMicros, s.PurchasedUSD = scaled, purchased, scaled, purchased
	s.SubscriptionUSD = "0.00"
	s.Gauges = []WindowGauge{{ID: "day", Scope: ScopeAll, Label: "Today", UsedBp: dayBp, ResetsAtMs: &dayReset},
		{ID: "week", Scope: ScopeAll, Label: "This week", UsedBp: weekBp, ResetsAtMs: &weekReset},
		{ID: "top_tier", Scope: ScopeTopTier, Label: "Large models (monthly)", UsedBp: topBp, ResetsAtMs: &dayReset}}
	s.SpendSource, s.AutoTopUpEnabled, s.PausedReason, s.KeepGoingOnCredits = source, autoTopUp, paused, keepGoing
	s.FromHeader, s.CapturedAt = true, float64(time.Now().UnixNano())/1e9
	return s
}

func TestCreditsWindowPlanStatesEveryWindowStateDrivesItsNotice(t *testing.T) {
	no := false
	states := map[string]func() *CreditsState{
		"healthy":                    func() *CreditsState { return cwPlanState(1_200, 900, 0, "0.00", "included", false, "", true, nil) },
		"75pct":                      func() *CreditsState { return cwPlanState(7_600, 4_000, 0, "0.00", "included", false, "", true, nil) },
		"week_90pct":                 func() *CreditsState { return cwPlanState(3_000, 9_100, 0, "0.00", "included", false, "", true, nil) },
		"keep_going_off":             func() *CreditsState { return cwPlanState(10_000, 6_000, 0, "5.00", "", false, "", true, &no) },
		"keep_going_off_no_balance":  func() *CreditsState { return cwPlanState(10_000, 6_000, 0, "0.00", "", false, "", true, &no) },
		"payg":                       func() *CreditsState { return cwPlanState(10_000, 6_000, 0, "5.00", "payg", false, "", true, nil) },
		"payg_autotopup":             func() *CreditsState { return cwPlanState(10_000, 6_000, 0, "5.00", "payg", true, "", true, nil) },
		"payg_top_tier":              func() *CreditsState { return cwPlanState(4_000, 2_000, 10_000, "5.00", "payg", false, "", true, nil) },
		"top_tier_blocked":           func() *CreditsState { return cwPlanState(4_000, 2_000, 10_000, "0.00", "", false, "", true, nil) },
		"top_tier_blocked_autotopup": func() *CreditsState { return cwPlanState(4_000, 2_000, 10_000, "0.00", "", true, "", true, nil) },
		"exhausted":                  func() *CreditsState { return cwPlanState(10_000, 6_000, 0, "0.00", "", false, "", true, nil) },
		"paused": func() *CreditsState {
			return cwPlanState(2_000, 1_000, 0, "0.00", "", false, "payment_past_due", false, nil)
		},
	}
	for _, c := range []struct{ name, key, contains string }{
		{"healthy", "", ""}, {"75pct", "", ""}, {"week_90pct", WindowApproachKey("week"), "91% of your weekly limit used"},
		{"payg", CreditsWindowPaygKey, "now using credits"}, {"payg_autotopup", CreditsWindowPaygKey, "auto top-up is on"},
		{"payg_top_tier", CreditsWindowPaygKey, "You've hit your monthly large-model limit · resets"},
		{"top_tier_blocked", CreditsWindowLimitKey, "You've hit your monthly large-model limit · resets"},
		{"top_tier_blocked_autotopup", CreditsWindowLimitKey, "auto top-up is on"},
		{"exhausted", CreditsWindowLimitKey, "/topup to add credits"}, {"keep_going_off", CreditsWindowLimitKey, "turn on Keep going in /topup"},
		{"keep_going_off_no_balance", CreditsWindowLimitKey, "add credits and turn on Keep going in /topup"},
		{"paused", CreditsDepletedKey, "payment failed"},
	} {
		state := states[c.name]()
		if !state.IsUsageWindow() || !state.FromHeader || state.Version != 1 {
			t.Fatalf("%s state", c.name)
		}
		show, _ := EvaluateCreditsNotices(state, cwSeenLatch(), false, false)
		var keys []string
		found := c.contains == ""
		for _, n := range show {
			keys = append(keys, n.Key)
			found = found || strings.Contains(n.Text, c.contains)
			if strings.Contains(n.Text, "$") {
				t.Fatalf("%s: dollar text %q", c.name, n.Text)
			}
		}
		want := []string{}
		if c.key != "" {
			want = []string{c.key}
		}
		if !cwEqualStrings(keys, want) || !found {
			t.Fatalf("%s: keys %v want %v", c.name, keys, want)
		}
	}
}

func TestCreditsWindowPlanStatesLegacyFixturesStillResolve(t *testing.T) {
	t.Setenv(DevCreditsEnv, "1")
	t.Setenv(DevCreditsFixtureEnv, "depleted")
	if s := DevFixtureCreditsState(); s == nil || s.IsUsageWindow() || s.PaidAccess {
		t.Fatalf("%+v", s)
	}
}

func TestCreditsWindowPlanStatesWindowFixtureNamesAreNotDevFixtures(t *testing.T) {
	t.Setenv(DevCreditsEnv, "1")
	t.Setenv(DevCreditsFixtureEnv, "window_exhausted")
	if s := DevFixtureCreditsState(); s != nil {
		t.Fatalf("%+v", s)
	}
}

func cwF(v float64) *float64 { return &v }

func cwStr(v string) *string { return &v }

func cwWindowInfo(g cwG, keepGoing *bool) *NousPortalAccountInfo {
	no, paid := false, true
	return &NousPortalAccountInfo{LoggedIn: true, PaidServiceAccess: &paid, PortalBaseURL: "https://portal.example",
		PaidServiceAccessInfo: &NousPaidServiceAccessInfo{PurchasedCreditsRemaining: cwF(5), SubscriptionCreditsRemaining: cwF(0), TotalUsableCredits: cwF(5)},
		Subscription: &NousPortalSubscriptionInfo{Plan: cwStr("Window Plan"), MonthlyCredits: cwF(0), CreditsRemaining: cwF(0), RolloverCredits: cwF(0),
			CurrentPeriodEnd: cwStr("2026-10-01")},
		UsageWindow: &NousUsageWindowInfo{Gauges: cwGauges(g), AutoTopUpEnabled: &no, KeepGoingOnCredits: keepGoing}}
}

func cwPercents(s *AccountUsageSnapshot) []int {
	var out []int
	for _, w := range s.Windows {
		out = append(out, int(math.RoundToEven(*w.UsedPercent)))
	}
	return out
}

func cwLabels(s *AccountUsageSnapshot) string {
	var out []string
	for _, w := range s.Windows {
		out = append(out, w.Label)
	}
	return strings.Join(out, ",")
}

func cwLinesHasPrefix(lines []string, prefix string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

func cwLinesHas(lines []string, line string) bool {
	for _, l := range lines {
		if l == line {
			return true
		}
	}
	return false
}

func TestCreditsWindowUsagePortalWindowAccountRendersTodayWeekAndTopTier(t *testing.T) {
	now := time.Now().UnixMilli()
	top := now + 20*86_400_000
	g := cwDay(7_600, 4_000)
	g.top, g.dayReset, g.weekReset, g.topReset = 10_000, now+3600_000, now+3*86_400_000, top
	snap := BuildNousCreditsSnapshot(cwWindowInfo(g, nil))
	if snap == nil || snap.Plan != "Window Plan" || cwLabels(snap) != "Today,This week,Large models (monthly)" {
		t.Fatalf("%+v", snap)
	}
	topLocal := time.UnixMilli(top).Local()
	if !snap.Windows[2].ResetAt.Equal(time.UnixMilli(top)) {
		t.Fatal("top reset")
	}
	if want := fmt.Sprintf("Monthly large-model usage is used up. Resets %s %d. Past this, large models use your credits.", topLocal.Format("Jan"), topLocal.Day()); !cwLinesHas(snap.Details, want) {
		t.Fatalf("%v", snap.Details)
	}
	if fmt.Sprint(cwPercents(snap)) != "[76 40 100]" {
		t.Fatalf("%v", cwPercents(snap))
	}
	lines := RenderAccountUsageLines(snap, false)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "76% used") || !strings.Contains(joined, "[") || !strings.Contains(joined, "40% used") ||
		!strings.Contains(joined, "100% used") || strings.Count(strings.Split(strings.Split(joined, "Today")[1], "This week")[0], "[") != 1 {
		t.Fatalf("%s", joined)
	}
	if !cwLinesHasPrefix(lines, accountUsageTopTierUsedUp) || !cwLinesHas(lines, "Top-up credits: $5.00") || !cwLinesHas(lines, "Renews: Oct 1, 2026") ||
		cwLinesHasPrefix(lines, "Subscription credits") || cwLinesHasPrefix(lines, "Total usable") || cwLinesHasPrefix(lines, "Subscription:") ||
		cwLinesHas(lines, accountUsageDepletedLine) {
		t.Fatalf("%s", joined)
	}
}

func TestCreditsWindowUsagePortalRowsFollowTheServerList(t *testing.T) {
	now := time.Now().UnixMilli()
	g := cwDay(100, 200)
	g.dayReset, g.weekReset, g.topReset, g.monthReset = now+3600_000, now+3*86_400_000, now+20*86_400_000, now+20*86_400_000
	paid := true
	info := &NousPortalAccountInfo{LoggedIn: true, PaidServiceAccess: &paid, UsageWindow: &NousUsageWindowInfo{Gauges: cwGauges(g)}}
	if cwLabels(BuildNousCreditsSnapshot(info)) != "Today,This week,Large models (monthly)" {
		t.Fatal("no month row")
	}
	g.month = 3_300
	info.UsageWindow.Gauges = cwGauges(g)
	snap := BuildNousCreditsSnapshot(info)
	if cwLabels(snap) != "Today,This week,This month,Large models (monthly)" || cwPercents(snap)[2] != 33 {
		t.Fatalf("%v", cwLabels(snap))
	}
	g.month = -1
	info.UsageWindow.Gauges = ParseGauges(cwDumps(append([]map[string]any{{"id": "five_hour", "scope": "all", "label": "Next 5 hours",
		"used_bp": 5_000, "resets_at_ms": now + 3600_000}}, cwGaugeList(g)...)))
	if BuildNousCreditsSnapshot(info).Windows[0].Label != "Next 5 hours" {
		t.Fatal("new allowance first")
	}
}

func TestCreditsWindowUsageUnknownResetKeepsTheAllowanceRow(t *testing.T) {
	paid := true
	info := &NousPortalAccountInfo{LoggedIn: true, PaidServiceAccess: &paid, UsageWindow: &NousUsageWindowInfo{Gauges: ParseGauges(cwDumps(
		[]map[string]any{{"id": "day", "scope": "all", "label": "Today", "used_bp": 4_200},
			{"id": "week", "scope": "all", "label": "This week", "used_bp": 1_000, "resets_at_ms": "soon"}}))}}
	snap := BuildNousCreditsSnapshot(info)
	if !snap.UsageWindow || cwLabels(snap) != "Today,This week" || fmt.Sprint(cwPercents(snap)) != "[42 10]" || snap.Windows[0].ResetAt != nil || snap.Windows[1].ResetAt != nil {
		t.Fatalf("%+v", snap)
	}
	for _, line := range RenderAccountUsageLines(snap, false) {
		if strings.Contains(line, "resets") {
			t.Fatalf("%q", line)
		}
	}
}

func TestCreditsWindowUsageHeaderGaugeRaisesStalePortalPercents(t *testing.T) {
	now := time.Now().UnixMilli()
	g := cwDay(2_200, 1_100)
	g.top, g.dayReset, g.weekReset = 3_400, now+3600_000, now+3*86_400_000
	snap := BuildNousCreditsSnapshot(cwWindowInfo(g, nil))
	future := now + 3600_000
	hg := cwDay(4_000, 1_600)
	hg.top, hg.dayReset, hg.weekReset = 10_000, future, future
	if raised := ApplyHeaderGauge(snap, cwState(hg)); fmt.Sprint(cwPercents(raised)) != "[40 16 100]" {
		t.Fatalf("%v", cwPercents(raised))
	}
	hg = cwDay(100, 100)
	hg.top, hg.dayReset, hg.weekReset = 100, future, future
	if ApplyHeaderGauge(snap, cwState(hg)) != snap {
		t.Fatal("behind header leaves the snapshot")
	}
	past := time.Now().UnixMilli() - 1
	hg = cwDay(10_000, 1_600)
	hg.top, hg.dayReset, hg.weekReset = -1, past, past+3*86_400_000
	if got := cwPercents(ApplyHeaderGauge(snap, cwState(hg))); fmt.Sprint(got[:2]) != "[22 16]" {
		t.Fatalf("%v", got)
	}
}

func TestCreditsWindowUsagePortalPausedWindowAccount(t *testing.T) {
	now := time.Now().UnixMilli()
	g := cwDay(0, 0)
	g.dayReset, g.weekReset = now+1, now+2
	no, unpaid := false, false
	info := &NousPortalAccountInfo{LoggedIn: true, PaidServiceAccess: &unpaid, PaidServiceAccessInfo: &NousPaidServiceAccessInfo{PurchasedCreditsRemaining: cwF(0)},
		UsageWindow: &NousUsageWindowInfo{Gauges: cwGauges(g), AutoTopUpEnabled: &no, PausedReason: "payment_past_due"}}
	lines := RenderAccountUsageLines(BuildNousCreditsSnapshot(info), false)
	if !cwLinesHas(lines, accountUsagePausedLine) || cwLinesHas(lines, accountUsageDepletedLine) {
		t.Fatalf("%v", lines)
	}
}

func TestCreditsWindowUsagePortalKeepGoingOffSaysSoAndNeverSaysAddCredits(t *testing.T) {
	now := time.Now().UnixMilli()
	g := cwDay(7_600, 4_000)
	g.top, g.dayReset, g.weekReset, g.topReset = 10_000, now+3600_000, now+3*86_400_000, now+20*86_400_000
	off := false
	info := cwWindowInfo(g, &off)
	info.Subscription = nil
	lines := RenderAccountUsageLines(BuildNousCreditsSnapshot(info), false)
	if !cwLinesHas(lines, accountUsageKeepGoingOffLine) {
		t.Fatalf("%v", lines)
	}
	tail := false
	for _, l := range lines {
		tail = tail || strings.HasSuffix(l, "Your other included usage is still available.")
		if strings.Contains(l, "use your credits") {
			t.Fatalf("%q", l)
		}
	}
	guidance := FormatUsageWindowBlockGuidance(info)
	if !tail || !strings.Contains(guidance, NousPortalBillingURL(info.PortalBaseURL)) || strings.Contains(guidance, "topup=open") || !strings.Contains(guidance, "/topup") {
		t.Fatalf("%v %q", tail, guidance)
	}
	for _, text := range append(lines, guidance) {
		if strings.Contains(text, "—") {
			t.Fatalf("em dash in %q", text)
		}
	}
	on := true
	for _, kg := range []*bool{&on, nil} {
		info.UsageWindow.KeepGoingOnCredits = kg
		lines := RenderAccountUsageLines(BuildNousCreditsSnapshot(info), false)
		if cwLinesHas(lines, accountUsageKeepGoingOffLine) || cwLinesHas(lines, accountUsageKeepGoingOnLine) != (kg != nil) {
			t.Fatalf("%v", lines)
		}
		past := false
		for _, l := range lines {
			past = past || strings.HasSuffix(l, "Past this, large models use your credits.")
		}
		if !past || !strings.Contains(FormatUsageWindowBlockGuidance(info), NousPortalTopupURL(info)) {
			t.Fatalf("%v", lines)
		}
	}
}

func TestCreditsWindowUsagePortalLegacyAccountIsUnchanged(t *testing.T) {
	paid := true
	info := &NousPortalAccountInfo{LoggedIn: true, PaidServiceAccess: &paid,
		PaidServiceAccessInfo: &NousPaidServiceAccessInfo{PurchasedCreditsRemaining: cwF(1), SubscriptionCreditsRemaining: cwF(18), TotalUsableCredits: cwF(19)},
		Subscription:          &NousPortalSubscriptionInfo{Plan: cwStr("Credit Plan"), MonthlyCredits: cwF(20), CreditsRemaining: cwF(18), RolloverCredits: cwF(0)}}
	snap := BuildNousCreditsSnapshot(info)
	if snap == nil || snap.UsageWindow {
		t.Fatalf("%+v", snap)
	}
	lines := RenderAccountUsageLines(snap, false)
	if lines[0] != "📈 Nous credits" || lines[1] != "Provider: nous (Credit Plan)" || !cwLinesHasPrefix(lines, "Subscription: 90% remaining (10% used)") ||
		!cwLinesHas(lines, "Subscription credits: $18.00") || !cwLinesHas(lines, "Top up: "+NousPortalTopupURL(info)) || !cwLinesHas(lines, "(or run /topup)") {
		t.Fatalf("%v", lines)
	}
	for _, l := range lines {
		if strings.Contains(l, "% used  ") || strings.Contains(l, "Top up via") {
			t.Fatalf("%q", l)
		}
	}
}

func TestCreditsWindowUsageFixtureWindowStateRenders(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv(DevCreditsEnv, "1")
	t.Setenv(DevCreditsFixtureEnv, "")
	prev := accountUsageFixtureFn
	t.Cleanup(func() { accountUsageFixtureFn = prev })
	accountUsageFixtureFn = func() *CreditsState { return cwPlanState(4_000, 2_000, 10_000, "0.00", "", false, "", true, nil) }
	lines := NousCreditsLines(t.Context(), false, time.Second, nil)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "40% used") || !strings.Contains(joined, "This week") || !strings.Contains(joined, "[") ||
		!cwLinesHasPrefix(lines, accountUsageTopTierUsedUp) || !cwLinesHas(lines, "Top-up credits: $0.00") ||
		!cwLinesHas(lines, accountUsageDevFixtureTail) {
		t.Fatalf("%s", joined)
	}
}

func cwClassify(status int, code, message, provider, model, baseURL string, body map[string]any) *Classified {
	if body == nil {
		body = map[string]any{"error": map[string]any{"code": code, "message": message}}
	}
	return ClassifyError(ClassifyInput{Err: fmt.Errorf("%s", message), Status: status, Body: cwDumps(body), Headers: http.Header{},
		Provider: provider, Model: model, BaseURL: baseURL})
}

func TestCreditsWindowClassifierWindow402IsBillingAndNotRetryable(t *testing.T) {
	c := cwClassify(402, "usage_window_and_credits_exhausted", "Your included Hermes usage for today is used up and you have no top-up credits. It resets at 18:00.",
		"nous", "example/model-c", "", nil)
	if c.Reason != ReasonBilling || c.Retryable || c.Context["usage_window"] != true || c.ShouldFallback {
		t.Fatalf("%+v", c)
	}
}

func TestCreditsWindowClassifierOther402BillingCodesKeepMainTextRules(t *testing.T) {
	c := cwClassify(402, "insufficient_quota", "quota exceeded, please retry after the window resets", "example", "example/model-a", "", nil)
	if c.Reason != ReasonRateLimit || c.Context["usage_window"] == true {
		t.Fatalf("%+v", c)
	}
}

func TestCreditsWindowClassifierWindow402CarriesTheServerWordingWhenSent(t *testing.T) {
	if c := cwClassify(402, "usage_window_and_credits_exhausted", "Your included Hermes usage for today is used up.", "nous", "m", "", nil); c.Context["display_message"] != nil {
		t.Fatalf("%+v", c.Context)
	}
	c := cwClassify(402, "", "Your included Hermes usage for today is used up.", "nous", "m", "", map[string]any{"status": 402,
		"code": "usage_window_and_credits_exhausted", "message": "m", "display_message": "Daily limit hit. Back at 4pm."})
	if len(c.Context) != 2 || c.Context["usage_window"] != true || c.Context["display_message"] != "Daily limit hit. Back at 4pm." {
		t.Fatalf("%+v", c.Context)
	}
	other := cwClassify(402, "", "no credits", "nous", "m", "", map[string]any{"error": map[string]any{"code": "insufficient_credit", "message": "no credits"},
		"display_message": "x"})
	if other.Context["display_message"] != nil {
		t.Fatalf("%+v", other.Context)
	}
}

func TestCreditsWindowClassifierWindowCodeFromAnotherUpstreamKeepsMainRules(t *testing.T) {
	c := cwClassify(402, "usage_window_and_credits_exhausted", "quota exceeded, retry after the window resets", "example", "m", "https://example.invalid/v1", nil)
	if c.Reason != ReasonRateLimit || c.Context["usage_window"] == true {
		t.Fatalf("%+v", c)
	}
	for _, pb := range [][2]string{{"example", "https://example.invalid/v1"}, {"custom", "https://custom.example.invalid/v1"}} {
		c := cwClassify(429, "usage_window_and_credits_exhausted", "Too many requests", pb[0], "m", pb[1], nil)
		if c.Reason != ReasonRateLimit || !c.Retryable || c.Context["usage_window"] == true {
			t.Fatalf("%s: %+v", pb[0], c)
		}
	}
}

func TestCreditsWindowClassifierPlain403StillClassifiesAsBefore(t *testing.T) {
	if c := cwClassify(403, "forbidden", "Forbidden", "nous", "m", "", nil); c.Reason == ReasonModelNotFound {
		t.Fatalf("%+v", c)
	}
}

func cwCapture(t *testing.T, provider, model, baseURL string) (*CreditsCapture, *[]AgentNotice) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv(DevCreditsFixtureEnv, "")
	c := NewCreditsCapture(nil, provider, model, baseURL, "cw-"+t.Name())
	var emitted []AgentNotice
	c.Notice = func(n AgentNotice) { emitted = append(emitted, n) }
	t.Cleanup(func() { ForgetWindowNotices("cw-" + t.Name()) })
	return c, &emitted
}

func TestCreditsWindowBlockedNous402RefreshesTheCreditsState(t *testing.T) {
	c, emitted := cwCapture(t, "nous", "example/model-c", "https://inference-api.nousresearch.com/v1")
	pricingWarmFlights.Store(c, true)
	t.Cleanup(func() { pricingWarmFlights.Delete(c) })
	c.CaptureCredits(cwHeaders(cwH{day: 4_000, week: 2_000, top: 10_000}), true)
	if s := c.Credits(); s == nil || !s.TopTierExhausted() {
		t.Fatalf("%+v", s)
	}
	if len(*emitted) != 1 || (*emitted)[0].Key != CreditsWindowLimitKey || !strings.Contains((*emitted)[0].Text, "You've hit your monthly large-model limit") {
		t.Fatalf("%+v", *emitted)
	}
}

func TestCreditsWindowBlockedResponseIsUsageWindow(t *testing.T) {
	if !ResponseIsUsageWindow(cwHeaders(cwDefaultH())) {
		t.Fatal("window headers")
	}
	partial := cwHeaders(cwDefaultH())
	partial.Del("x-nous-credits-window-gauges")
	for _, h := range []http.Header{{}, cwWith(cwHeaders(cwDefaultH()), "x-nous-credits-denominator-kind", "subscription_cap"), partial, nil} {
		if ResponseIsUsageWindow(h) {
			t.Fatalf("%v", h)
		}
	}
}

func TestCreditsWindowPickerAccountPayloadUsageWindowIsParsed(t *testing.T) {
	payload := bvJSON(t, cwDumps(map[string]any{"user": map[string]any{"email": "x@y.z"}, "organisation": map[string]any{"id": "o1"},
		"paid_service_access": map[string]any{"allowed": true},
		"usage_window": map[string]any{"gauges": cwGaugeList(cwDefaults()), "auto_top_up_enabled": false, "paused_reason": "payment_past_due",
			"day_used_bp": 9999, "week_used_bp": 9999}}))
	info, err := nousAccountFromPayload(payload, map[string]any{}, "")
	if err != nil || info.UsageWindow == nil || info.UsageWindow.PausedReason != "payment_past_due" {
		t.Fatalf("%+v %v", info, err)
	}
	var got []string
	for _, g := range info.UsageWindow.Gauges {
		got = append(got, fmt.Sprintf("%s:%d", g.ID, g.UsedBp))
	}
	if strings.Join(got, ",") != "day:1200,week:900,top_tier:0" || *info.UsageWindow.Gauges[1].ResetsAtMs != cwWeekReset {
		t.Fatalf("%v", got)
	}
	legacy, err := nousAccountFromPayload(map[string]any{"user": map[string]any{}, "paid_service_access": map[string]any{"allowed": true}}, map[string]any{}, "")
	if err != nil || legacy.UsageWindow != nil {
		t.Fatalf("%+v %v", legacy, err)
	}
}

func TestCreditsWindowRefusalOnlyABillingRefusalWithTheWindowMarkBenchesOneModel(t *testing.T) {
	window := map[string]any{"usage_window": true}
	if PoolFailureReason(ReasonBilling, window, true) != FailureReasonUsageWindow || PoolFailureReason(ReasonRateLimit, window, false) != string(ReasonRateLimit) ||
		PoolFailureReason(ReasonBilling, map[string]any{}, true) != FailureReasonBillingUnverified || PoolFailureReason(ReasonBilling, nil, false) != string(ReasonBilling) {
		t.Fatal("pool failure reasons")
	}
}

func TestCreditsWindowRefusalKeepGoingOffNoBalanceGuidanceNamesBothSteps(t *testing.T) {
	no, paid := false, true
	window := &NousUsageWindowInfo{Gauges: cwGauges(cwDay(100, 50)), AutoTopUpEnabled: &no, KeepGoingOnCredits: &no}
	for _, purchased := range []*float64{cwF(0), nil} {
		info := &NousPortalAccountInfo{LoggedIn: true, PaidServiceAccess: &paid, UsageWindow: window,
			PaidServiceAccessInfo: &NousPaidServiceAccessInfo{PurchasedCreditsRemaining: purchased}}
		if g := FormatUsageWindowBlockGuidance(info); !strings.HasPrefix(g, "Add credits and turn on Keep going in /topup") {
			t.Fatalf("%q", g)
		}
	}
}

func cwEmit(sessionID string, latch map[string]any, s *CreditsState) ([]cwShown, []string) {
	RestoreWindowNotices(sessionID, latch)
	show, clear := cwRun(s, latch, false, false)
	RememberWindowNotices(sessionID, latch)
	return show, clear
}

func cwSession(t *testing.T, suffix string) string {
	sid := "cw-rebuild-" + t.Name() + suffix
	ForgetWindowNotices(sid)
	t.Cleanup(func() { ForgetWindowNotices(sid) })
	return sid
}

func cwSpent() *CreditsState {
	return cwState(cwDay(10_000, 900), cwMicros(5_000_000), cwSource("payg"))
}

func TestCreditsWindowRebuildSameLineNotReshown(t *testing.T) {
	sid := cwSession(t, "")
	if show, _ := cwEmit(sid, NewCreditsLatch(), cwSpent()); !cwHasKey(show, CreditsWindowPaygKey) {
		t.Fatalf("%v", show)
	}
	if show, clear := cwEmit(sid, NewCreditsLatch(), cwSpent()); len(show)+len(clear) != 0 {
		t.Fatalf("%v %v", show, clear)
	}
}

func TestCreditsWindowRebuildChangedLineStillShown(t *testing.T) {
	sid := cwSession(t, "")
	cwEmit(sid, NewCreditsLatch(), cwSpent())
	if show, _ := cwEmit(sid, NewCreditsLatch(), cwState(cwDay(10_000, 900), cwMicros(0), cwSource("payg"))); !cwHasKey(show, CreditsWindowLimitKey) {
		t.Fatalf("%v", show)
	}
}

func TestCreditsWindowRebuildRecoveryClears(t *testing.T) {
	sid := cwSession(t, "")
	cwEmit(sid, NewCreditsLatch(), cwSpent())
	if _, clear := cwEmit(sid, NewCreditsLatch(), cwState(cwDay(500, 900))); !strings.Contains(strings.Join(clear, ","), CreditsWindowPaygKey) {
		t.Fatalf("%v", clear)
	}
}

func TestCreditsWindowRebuildApproachWarningNotReshown(t *testing.T) {
	sid := cwSession(t, "")
	near := cwState(cwDay(9_200, 900))
	show, _ := cwEmit(sid, NewCreditsLatch(), near)
	if len(show) == 0 || !strings.HasPrefix(show[0].key, "credits.window.") {
		t.Fatalf("%v", show)
	}
	if show, _ := cwEmit(sid, NewCreditsLatch(), near); len(show) != 0 {
		t.Fatalf("%v", show)
	}
}

func TestCreditsWindowRebuildOtherSessionUnaffected(t *testing.T) {
	cwEmit(cwSession(t, "-a"), NewCreditsLatch(), cwSpent())
	if show, _ := cwEmit(cwSession(t, "-b"), NewCreditsLatch(), cwSpent()); !cwHasKey(show, CreditsWindowPaygKey) {
		t.Fatalf("%v", show)
	}
}

func TestCreditsWindowRefusedRequestKeepsTheLastKnownTopUpBalance(t *testing.T) {
	c, _ := cwCapture(t, "nous", "", "")
	c.Notice = nil
	c.credits = cwParse(t, cwHeaders(cwH{day: 9_800, week: 500, purchased: 8.3, source: "included"}))
	refused := cwHeaders(cwH{day: 10_000, week: 500})
	for k := range refused {
		if strings.HasPrefix(strings.ToLower(k), "x-nous-credits-purchased-") {
			refused.Del(k)
		}
	}
	c.CaptureCredits(refused, false)
	if s := c.Credits(); !s.WindowExhausted() || s.PurchasedMicros != 8_300_000 || s.PurchasedUSD != "8.30" {
		t.Fatalf("%+v", s)
	}
}

func TestCreditsWindowLeavingAWindowPlanClearsItsLimitLine(t *testing.T) {
	latch := NewCreditsLatch()
	cwRun(cwState(cwDay(10_000, 900)), latch, false, false)
	if !latch["active"].(map[string]bool)[CreditsWindowLimitKey] {
		t.Fatal("limit line active")
	}
	plan := cwLegacy("subscription_cap")
	limit, limitUSD := int64(10_000_000), "10.00"
	plan.PurchasedUSD, plan.RemainingMicros, plan.SubscriptionMicros, plan.SubscriptionLimitMicros, plan.SubscriptionLimitUSD = "0.00", 5_000_000, 5_000_000, &limit, &limitUSD
	_, clear := cwRun(plan, latch, false, false)
	if !strings.Contains(strings.Join(clear, ","), CreditsWindowLimitKey) || latch["active"].(map[string]bool)[CreditsWindowLimitKey] {
		t.Fatalf("%v", clear)
	}
}

func TestCreditsWindowHeadersFromAnotherUpstreamAreIgnored(t *testing.T) {
	c, _ := cwCapture(t, "example", "", "https://example.invalid/v1")
	c.CaptureCredits(cwHeaders(cwH{day: 10_000, week: 500, source: "included"}), false)
	if c.Credits() != nil {
		t.Fatalf("%+v", c.Credits())
	}
}

func TestCreditsWindowUnmeteredReplyRaisesNoLimitStop(t *testing.T) {
	c, _ := cwCapture(t, "nous", "", "")
	c.Notice = nil
	c.CaptureCredits(cwHeaders(cwH{day: 4_000, week: 2_000, top: 10_000}), false)
	s := c.Credits()
	if _, stop := windowHardStop(s, false, nil); !s.Unmetered || stop {
		t.Fatalf("%+v", s)
	}
	c.CaptureCredits(cwHeaders(cwH{day: 4_000, week: 2_000, top: 10_000}), true)
	if text, stop := windowHardStop(c.Credits(), false, nil); !stop || !strings.Contains(text, "large-model limit") {
		t.Fatalf("%q", text)
	}
}
