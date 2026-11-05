package provider

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/home"
)

// Env var naming a dev credits fixture state or a file holding that name
const DevCreditsFixtureEnv = "ICEBATH_DEV_CREDITS_FIXTURE"

// Credits notice policy constants shared by every surface
const (
	CreditsNoticeKind           = "sticky"
	CreditsRestoredTTLMs        = 8000
	CreditsUsageKey             = "credits.usage"
	GrantUnspentMinMicros       = 10_000
	CreditsWindowPaygKey        = "credits.window_payg"
	CreditsDepletedKey          = "credits.depleted"
	CreditsWindowLimitKey       = "credits.window_limit"
	CreditsRestoredKey          = "credits.restored"
	CreditsGrantSpentKey        = "credits.grant_spent"
	WindowApproachFraction      = 0.90
	CreditsWindowApproachPrefix = "credits.window."
	WindowApproachTTLMs         = 10_000
	creditsDepletedText         = "✕ Credit access paused · run /topup to top up"
	creditsPausedText           = "✕ Included usage paused · your subscription payment failed · update your card, or add credits in /topup to keep going"
	creditsPausedOtherText      = "✕ Included usage paused · see Billing in the Portal, or add credits in /topup to keep going"
	creditsTopTierLimit         = "You've hit your monthly large-model limit"
	creditsAutoTopUpSuffix      = " · auto top-up is on"
	creditsAddCreditsAction     = "/topup to add credits"
	seenUsageBandsMax           = 500
	creditsGaugesHeader         = "x-nous-credits-window-gauges"
)

// CreditsUsageBand is one escalating usage threshold with its notice level and label percent
type CreditsUsageBand struct {
	Threshold float64
	Level     string
	Pct       int
}

// Ascending usage bands, the highest reached is the one line shown
var CreditsUsageBands = []CreditsUsageBand{{0.50, "info", 50}, {0.75, "warn", 75}, {0.90, "warn", 90}}

var creditsStep = map[string]string{
	UseCredits:             "turn on Keep going in /topup",
	AddCredits:             creditsAddCreditsAction,
	AddCreditsAndKeepGoing: "add credits and turn on Keep going in /topup",
}

var creditsNow = time.Now

var creditsVersionWarned sync.Once

// CreditsState is the Nous credits snapshot parsed from x-nous-credits headers or seeded from the Portal
type CreditsState struct {
	Version                 int
	RemainingMicros         int64
	RemainingUSD            string
	SubscriptionMicros      int64
	SubscriptionUSD         string
	SubscriptionLimitMicros *int64
	SubscriptionLimitUSD    *string
	RolloverMicros          int64
	PurchasedMicros         int64
	PurchasedUSD            string
	ToolPoolMicros          int64
	ToolPoolGatedOff        bool
	DenominatorKind         string
	PaidAccess              bool
	DisabledReason          string
	AsOfMs                  int64
	CapturedAt              float64
	FromHeader              bool
	Gauges                  []WindowGauge
	SpendSource             string
	Unmetered               bool
	PausedReason            string
	AutoTopUpEnabled        bool
	KeepGoingOnCredits      *bool
}

// NewCreditsState returns a state with the dataclass defaults, no denominator and paid access
func NewCreditsState() *CreditsState {
	return &CreditsState{DenominatorKind: "none", PaidAccess: true}
}

// CreditsPay reports top-up credits covering usage once the included allowance is used up
func (s *CreditsState) CreditsPay() bool {
	return s.PurchasedMicros > 0 && (s.KeepGoingOnCredits == nil || *s.KeepGoingOnCredits)
}

// HasData reports a captured state
func (s *CreditsState) HasData() bool { return s.CapturedAt > 0 }

// AgeSeconds is the time since capture, infinite without data
func (s *CreditsState) AgeSeconds() float64 {
	if !s.HasData() {
		return math.Inf(1)
	}
	return float64(creditsNow().UnixNano())/1e9 - s.CapturedAt
}

// Depleted keys off paid access alone since a zero balance can still have live access
func (s *CreditsState) Depleted() bool { return !s.PaidAccess }

// IsUsageWindow reports a usage window plan with at least one allowance stopping every paid model
func (s *CreditsState) IsUsageWindow() bool {
	return s.DenominatorKind == "usage_window" && len(AllScope(s.Gauges)) > 0
}

// BindingGauge is the all scope allowance that binds on a usage window plan
func (s *CreditsState) BindingGauge() *WindowGauge {
	if !s.IsUsageWindow() {
		return nil
	}
	return BindingGauge(s.Gauges)
}

// TopTierGauge is the large model allowance on a usage window plan
func (s *CreditsState) TopTierGauge() *WindowGauge {
	if !s.IsUsageWindow() {
		return nil
	}
	return TopTierGauge(s.Gauges)
}

// UsedFraction is the consumed share of the cap or binding allowance, false without a denominator
func (s *CreditsState) UsedFraction() (float64, bool) {
	if s.IsUsageWindow() {
		return s.BindingGauge().UsedFraction(), true
	}
	if s.SubscriptionLimitMicros == nil || *s.SubscriptionLimitMicros <= 0 {
		return 0, false
	}
	lim := float64(*s.SubscriptionLimitMicros)
	return math.Max(0, math.Min(1, (lim-float64(s.SubscriptionMicros))/lim)), true
}

// WindowResetsAtMs is the reset instant of the binding allowance
func (s *CreditsState) WindowResetsAtMs() *int64 {
	if g := s.BindingGauge(); g != nil {
		return g.ResetsAtMs
	}
	return nil
}

// TopTierResetsAtMs is the reset instant of the large model allowance
func (s *CreditsState) TopTierResetsAtMs() *int64 {
	if g := s.TopTierGauge(); g != nil {
		return g.ResetsAtMs
	}
	return nil
}

// TopTierUsedBp is the large model allowance use in basis points
func (s *CreditsState) TopTierUsedBp() (int, bool) {
	if g := s.TopTierGauge(); g != nil {
		return g.UsedBp, true
	}
	return 0, false
}

// WindowExhausted reports a full allowance that stops every paid model
func (s *CreditsState) WindowExhausted() bool {
	if !s.IsUsageWindow() {
		return false
	}
	for _, g := range AllScope(s.Gauges) {
		if g.Full() {
			return true
		}
	}
	return false
}

// TopTierExhausted reports a full large model allowance
func (s *CreditsState) TopTierExhausted() bool {
	g := s.TopTierGauge()
	return g != nil && g.Full()
}

// AgentNotice is an out of band notice each driver renders its own way
type AgentNotice struct {
	Text  string
	Level string
	Kind  string
	TTLMs int
	Key   string
	ID    string
}

// NewCreditsLatch returns a fresh notice latch for the credits notice policy
func NewCreditsLatch() map[string]any {
	return map[string]any{"active": map[string]bool{}, "seen_below_90": false, "usage_band": nil, "seen_grant_unspent": false, "texts": map[string]string{}}
}

func latchActive(latch map[string]any) map[string]bool {
	active, ok := latch["active"].(map[string]bool)
	if !ok {
		active = map[string]bool{}
		latch["active"] = active
	}
	return active
}

func latchTexts(latch map[string]any) map[string]string {
	texts, ok := latch["texts"].(map[string]string)
	if !ok {
		texts = map[string]string{}
		latch["texts"] = texts
	}
	return texts
}

func latchWarned(latch map[string]any) map[string]*int64 {
	warned, ok := latch["window_warned"].(map[string]*int64)
	if !ok {
		warned = map[string]*int64{}
		latch["window_warned"] = warned
	}
	return warned
}

func latchBool(latch map[string]any, key string) bool {
	v, _ := latch[key].(bool)
	return v
}

// FormatWindowReset renders a reset as HH:MM within a day, weekday HH:MM within a week, else month and day
func FormatWindowReset(resetsAtMs, nowMs int64) string {
	if resetsAtMs <= 0 {
		return "the next reset"
	}
	if nowMs == 0 {
		nowMs = creditsNow().UnixMilli()
	}
	if resetsAtMs <= nowMs {
		return "shortly"
	}
	stamp := time.UnixMilli(resetsAtMs).Local()
	left := resetsAtMs - nowMs
	switch {
	case left >= 7*24*3600*1000:
		return fmt.Sprintf("%s %d", stamp.Format("Jan"), stamp.Day())
	case left < 24*3600*1000:
		return stamp.Format("15:04")
	}
	return stamp.Format("Mon 15:04")
}

func resetsSuffix(resetsAtMs *int64) string {
	if resetsAtMs == nil || *resetsAtMs <= 0 {
		return ""
	}
	return " · resets " + FormatWindowReset(*resetsAtMs, 0)
}

func spentWindow(state *CreditsState) (string, *int64) {
	if g := state.BindingGauge(); g != nil {
		return GaugeLimitName(*g), g.ResetsAtMs
	}
	return "daily", nil
}

// WindowApproachKey is the per allowance notice key a client replaces in place
func WindowApproachKey(gaugeID string) string { return CreditsWindowApproachPrefix + gaugeID }

func windowApproachText(g WindowGauge) string {
	pct := min(99, int(g.UsedFraction()*100))
	return fmt.Sprintf("⚠ %d%% of your %s limit used%s", pct, GaugeLimitName(g), resetsSuffix(g.ResetsAtMs))
}

// Full allowance behind the limit line, a nil tier counts the large model allowance as possibly stopping
func stopGauge(state *CreditsState, modelIsTopTier *bool) *WindowGauge {
	if !state.IsUsageWindow() {
		return nil
	}
	refused := state.FromHeader && state.SpendSource == ""
	g := BlockingGauge(state.Gauges, false, refused && (modelIsTopTier == nil || *modelIsTopTier))
	if g == nil && refused && state.TopTierExhausted() {
		return state.TopTierGauge()
	}
	return g
}

// Blocking copy for a usage window account or false when nothing blocks
func windowHardStop(state *CreditsState, modelIsFree bool, modelIsTopTier *bool) (string, bool) {
	purchased := state.PurchasedMicros
	if state.PausedReason != "" && purchased <= 0 {
		if PausedForPayment(state.PausedReason) {
			return creditsPausedText, true
		}
		return creditsPausedOtherText, true
	}
	if !state.PaidAccess {
		if modelIsFree {
			return "", false
		}
		return creditsDepletedText, true
	}
	if modelIsFree || state.CreditsPay() || state.Unmetered {
		return "", false
	}
	action := creditsStep[CreditsAction(state.KeepGoingOnCredits, purchased)]
	g := stopGauge(state, modelIsTopTier)
	if g == nil {
		return "", false
	}
	if g.Scope == ScopeTopTier {
		auto := ""
		if state.AutoTopUpEnabled && action == creditsAddCreditsAction {
			auto = creditsAutoTopUpSuffix
		}
		return fmt.Sprintf("✕ %s%s · %s%s", creditsTopTierLimit, resetsSuffix(g.ResetsAtMs), action, auto), true
	}
	return fmt.Sprintf("✕ You've hit your %s limit%s · %s", GaugeLimitName(*g), resetsSuffix(g.ResetsAtMs), action), true
}

// WindowStopResetsAtMs is when the allowance behind the limit line frees up
func WindowStopResetsAtMs(state *CreditsState, modelIsTopTier *bool) *int64 {
	if state.Unmetered {
		return nil
	}
	if g := stopGauge(state, modelIsTopTier); g != nil {
		return g.ResetsAtMs
	}
	return nil
}

func windowPaygText(state *CreditsState) (string, bool) {
	if state.SpendSource != "payg" || state.PurchasedMicros <= 0 {
		return "", false
	}
	var text string
	switch {
	case state.WindowExhausted():
		name, resets := spentWindow(state)
		text = fmt.Sprintf("• You've hit your %s limit%s · now using credits", name, resetsSuffix(resets))
	case state.TopTierExhausted():
		text = fmt.Sprintf("• %s%s · large models now use credits", creditsTopTierLimit, resetsSuffix(state.TopTierResetsAtMs()))
	default:
		text = "• Now using credits"
	}
	if state.AutoTopUpEnabled {
		text += creditsAutoTopUpSuffix
	}
	return text, true
}

func stickyNotice(text, level, key string) AgentNotice {
	return AgentNotice{Text: text, Level: level, Kind: CreditsNoticeKind, Key: key, ID: key}
}

func restoredNotice() AgentNotice {
	return AgentNotice{Text: "✓ Credit access restored", Level: "success", Kind: "ttl", TTLMs: CreditsRestoredTTLMs, Key: CreditsRestoredKey, ID: CreditsRestoredKey}
}

// Shows, replaces or clears one sticky line by key, a text change clears first
func reconcileSticky(key, text string, show bool, level string, latch map[string]any, toShow *[]AgentNotice, toClear *[]string) {
	active, texts := latchActive(latch), latchTexts(latch)
	prior, hasPrior := texts[key]
	switch {
	case show && (!active[key] || !hasPrior || prior != text):
		if active[key] {
			*toClear = append(*toClear, key)
		}
		*toShow = append(*toShow, stickyNotice(text, level, key))
		active[key] = true
		texts[key] = text
	case !show && active[key]:
		*toClear = append(*toClear, key)
		delete(active, key)
		delete(texts, key)
	}
}

func resetsEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Window plan policy, a heads up per allowance per reset, the limit or pause line, restored and payg lines
func evaluateWindowNotices(state *CreditsState, latch map[string]any, modelIsFree, modelIsTopTier bool) ([]AgentNotice, []string) {
	toShow, toClear := []AgentNotice{}, []string{}
	active := latchActive(latch)
	warned := latchWarned(latch)
	if active[CreditsUsageKey] {
		reconcileSticky(CreditsUsageKey, "", false, "info", latch, &toShow, &toClear)
		latch["usage_band"] = nil
	}
	for _, g := range state.Gauges {
		relevant := !modelIsFree && (g.Scope != ScopeTopTier || modelIsTopTier)
		if relevant && !g.Full() && g.UsedFraction() >= WindowApproachFraction && !resetsEqual(warned[g.ID], g.ResetsAtMs) {
			warned[g.ID] = g.ResetsAtMs
			key := WindowApproachKey(g.ID)
			toShow = append(toShow, AgentNotice{Text: windowApproachText(g), Level: "warn", Kind: "ttl", TTLMs: WindowApproachTTLMs, Key: key, ID: key})
		}
	}
	if active[CreditsGrantSpentKey] {
		toClear = append(toClear, CreditsGrantSpentKey)
		delete(active, CreditsGrantSpentKey)
	}
	tier := modelIsTopTier
	stop, stopped := windowHardStop(state, modelIsFree, &tier)
	limit := stopped && stop != creditsPausedText && stop != creditsPausedOtherText && stop != creditsDepletedText
	wasStopped := active[CreditsDepletedKey] || active[CreditsWindowLimitKey]
	reconcileSticky(CreditsWindowLimitKey, stop, limit, "error", latch, &toShow, &toClear)
	reconcileSticky(CreditsDepletedKey, stop, stopped && !limit, "error", latch, &toShow, &toClear)
	if wasStopped && !stopped && state.PaidAccess && !modelIsFree {
		toShow = append(toShow, restoredNotice())
	}
	payg, paygOK := "", false
	if !stopped {
		payg, paygOK = windowPaygText(state)
	}
	reconcileSticky(CreditsWindowPaygKey, payg, paygOK, "warn", latch, &toShow, &toClear)
	return toShow, toClear
}

// EvaluateCreditsNotices reconciles credits notices against the latch in place, callers clear before showing
func EvaluateCreditsNotices(state *CreditsState, latch map[string]any, modelIsFree, modelIsTopTier bool) ([]AgentNotice, []string) {
	if state.IsUsageWindow() {
		return evaluateWindowNotices(state, latch, modelIsFree, modelIsTopTier)
	}
	toShow, toClear := []AgentNotice{}, []string{}
	uf, ufOK := state.UsedFraction()
	active := latchActive(latch)
	for _, key := range []string{CreditsWindowPaygKey, CreditsWindowLimitKey} {
		if active[key] {
			toClear = append(toClear, key)
			delete(active, key)
			if texts, ok := latch["texts"].(map[string]string); ok {
				delete(texts, key)
			}
		}
	}
	if ufOK && uf < CreditsUsageBands[0].Threshold {
		latch["seen_below_90"] = true
	}
	if ufOK && uf < 1.0 && state.SubscriptionMicros >= GrantUnspentMinMicros {
		latch["seen_grant_unspent"] = true
	}
	var current *CreditsUsageBand
	if ufOK && state.PurchasedMicros <= 0 {
		for i := len(CreditsUsageBands) - 1; i >= 0; i-- {
			if uf >= CreditsUsageBands[i].Threshold {
				current = &CreditsUsageBands[i]
				break
			}
		}
	}
	var target any
	if current != nil && latchBool(latch, "seen_below_90") {
		target = current.Pct
	}
	if target != latch["usage_band"] {
		if active[CreditsUsageKey] {
			toClear = append(toClear, CreditsUsageKey)
			delete(active, CreditsUsageKey)
		}
		if target != nil {
			var lim int64
			if state.SubscriptionLimitMicros != nil {
				lim = *state.SubscriptionLimitMicros
			}
			usedUSD := "?"
			if lim != 0 {
				usedUSD = fmt.Sprintf("%.2f", float64(max(0, min(lim, lim-state.SubscriptionMicros)))/1_000_000)
			}
			limUSD := "?"
			if state.SubscriptionLimitUSD != nil && *state.SubscriptionLimitUSD != "" {
				limUSD = *state.SubscriptionLimitUSD
			}
			mark := "•"
			if current.Level == "warn" {
				mark = "⚠"
			}
			toShow = append(toShow, stickyNotice(fmt.Sprintf("%s You've used $%s of your $%s cap", mark, usedUSD, limUSD), current.Level, CreditsUsageKey))
			active[CreditsUsageKey] = true
		}
		latch["usage_band"] = target
	}
	grantCond := state.DenominatorKind == "subscription_cap" && ufOK && uf >= 1.0 && state.PurchasedMicros > 0
	if grantCond && !active[CreditsGrantSpentKey] && latchBool(latch, "seen_grant_unspent") {
		toShow = append(toShow, stickyNotice(fmt.Sprintf("• Grant spent · $%s top-up left", state.PurchasedUSD), "info", CreditsGrantSpentKey))
		active[CreditsGrantSpentKey] = true
		latch["seen_grant_unspent"] = false
	} else if active[CreditsGrantSpentKey] && !grantCond {
		toClear = append(toClear, CreditsGrantSpentKey)
		delete(active, CreditsGrantSpentKey)
	}
	depletedCond := !state.PaidAccess
	showDepleted := depletedCond && !modelIsFree
	if showDepleted && !active[CreditsDepletedKey] {
		toShow = append(toShow, stickyNotice(creditsDepletedText, "error", CreditsDepletedKey))
		active[CreditsDepletedKey] = true
	} else if active[CreditsDepletedKey] && !showDepleted {
		toClear = append(toClear, CreditsDepletedKey)
		delete(active, CreditsDepletedKey)
		if !depletedCond {
			toShow = append(toShow, restoredNotice())
		}
	}
	return toShow, toClear
}

type creditsHeaderField struct {
	name, kind string
	def        any
}

var creditsHeaderFields = []creditsHeaderField{
	{"remaining_micros", "micros", nil}, {"subscription_micros", "signed", nil}, {"rollover_micros", "micros", nil},
	{"purchased_micros", "micros", nil}, {"as_of_ms", "micros", nil}, {"tool_pool_micros", "micros", int64(0)},
	{"remaining_usd", "usd", nil}, {"subscription_usd", "usd", nil}, {"purchased_usd", "usd", nil},
	{"paid_access", "bool", true}, {"tool_pool_gated_off", "bool", false},
}

var creditsWindowOptionalFields = setOf("remaining_micros", "remaining_usd", "subscription_micros", "subscription_usd", "rollover_micros", "as_of_ms", "purchased_micros", "purchased_usd")

var creditsUSDRe = regexp.MustCompile(`^-?\p{Nd}+\.\p{Nd}{2}\n?$`)

func creditsHeaderName(field string) string {
	prefix := "x-nous-credits-"
	if strings.HasPrefix(field, "tool_pool_") {
		prefix = "x-nous-"
	}
	return prefix + strings.ReplaceAll(field, "_", "-")
}

// Decimal digit value of any Unicode Nd rune, every Nd run starts at a zero
func unicodeDigitValue(r rune) int {
	start := r
	for start > 0 && unicode.Is(unicode.Nd, start-1) {
		start--
	}
	return int(r-start) % 10
}

// Exact integer text as Python int() reads it, signs, surrounding whitespace, single underscores between digits
func pyIntText(raw string) (int64, bool) {
	s := strings.TrimFunc(raw, unicode.IsSpace)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	if s == "" {
		return 0, false
	}
	n := new(big.Int)
	ten := big.NewInt(10)
	prevDigit := false
	for _, r := range s {
		if r == '_' {
			if !prevDigit {
				return 0, false
			}
			prevDigit = false
			continue
		}
		if !unicode.Is(unicode.Nd, r) {
			return 0, false
		}
		n.Mul(n, ten).Add(n, big.NewInt(int64(unicodeDigitValue(r))))
		prevDigit = true
	}
	if !prevDigit {
		return 0, false
	}
	if neg {
		n.Neg(n)
	}
	if !n.IsInt64() {
		return 0, false
	}
	return n.Int64(), true
}

// One header value to a field value, the default when absent, false on a contract violation
func parseCreditsField(kind string, raw string, present bool, def any) (any, bool) {
	if !present {
		return def, def != nil
	}
	switch kind {
	case "micros", "signed", "bp":
		v, ok := pyIntText(raw)
		if !ok || (kind != "signed" && v < 0) || (kind == "bp" && v > BPFull) {
			return nil, false
		}
		return v, true
	case "usd":
		if !creditsUSDRe.MatchString(raw) {
			return nil, false
		}
		return raw, true
	}
	flag := strings.ToLower(strings.TrimFunc(raw, unicode.IsSpace))
	if flag != "true" && flag != "false" {
		return nil, false
	}
	return flag == "true", true
}

func lookupHeader(lowered map[string]string, key string) (string, bool) {
	v, ok := lowered[key]
	return v, ok
}

// ParseCreditsHeaders reads x-nous-credits headers into a state, nil on any contract violation
func ParseCreditsHeaders(h http.Header, provider string) (state *CreditsState) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("credits ▸ parse_credits_headers raised (fail-open miss)", "provider", provider, "panic", r)
			state = nil
		}
	}()
	found := false
	for k := range h {
		if strings.ToLower(k) == "x-nous-credits-version" {
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	lowered := LowerHeaders(h)
	version, ok := pyIntText(lowered["x-nous-credits-version"])
	if !ok {
		return nil
	}
	if version != 1 {
		if version > 1 {
			creditsVersionWarned.Do(func() {
				slog.Warn(fmt.Sprintf("credits header version %d unsupported, ignoring — update icebath", version), "provider", provider)
			})
		}
		return nil
	}
	kind, hasKind := lookupHeader(lowered, "x-nous-credits-denominator-kind")
	if !hasKind {
		kind = "none"
	}
	windowNote := kind == "usage_window"
	out := NewCreditsState()
	out.Version = 1
	valid := true
	for _, f := range creditsHeaderFields {
		raw, present := lookupHeader(lowered, creditsHeaderName(f.name))
		if !present && windowNote && creditsWindowOptionalFields[f.name] {
			continue
		}
		v, ok := parseCreditsField(f.kind, raw, present, f.def)
		if !ok {
			valid = false
			continue
		}
		assignCreditsField(out, f.name, v)
	}
	limMicros, hasLimMicros := lookupHeader(lowered, "x-nous-credits-subscription-limit-micros")
	limUSD, hasLimUSD := lookupHeader(lowered, "x-nous-credits-subscription-limit-usd")
	if hasLimMicros && hasLimUSD {
		m, okM := parseCreditsField("micros", limMicros, true, nil)
		u, okU := parseCreditsField("usd", limUSD, true, nil)
		if !okM || !okU {
			valid = false
		} else {
			micros, usd := m.(int64), u.(string)
			out.SubscriptionLimitMicros, out.SubscriptionLimitUSD = &micros, &usd
		}
	}
	if !valid || (kind != "subscription_cap" && kind != "none" && kind != "usage_window") {
		return nil
	}
	out.DenominatorKind = kind
	if windowNote && !parseWindowFields(lowered, out) {
		return nil
	}
	out.DisabledReason = lowered["x-nous-credits-disabled-reason"]
	out.CapturedAt = float64(creditsNow().UnixNano()) / 1e9
	out.FromHeader = true
	return out
}

func assignCreditsField(s *CreditsState, name string, v any) {
	switch name {
	case "remaining_micros":
		s.RemainingMicros = v.(int64)
	case "subscription_micros":
		s.SubscriptionMicros = v.(int64)
	case "rollover_micros":
		s.RolloverMicros = v.(int64)
	case "purchased_micros":
		s.PurchasedMicros = v.(int64)
	case "as_of_ms":
		s.AsOfMs = v.(int64)
	case "tool_pool_micros":
		s.ToolPoolMicros = v.(int64)
	case "remaining_usd":
		s.RemainingUSD = v.(string)
	case "subscription_usd":
		s.SubscriptionUSD = v.(string)
	case "purchased_usd":
		s.PurchasedUSD = v.(string)
	case "paid_access":
		s.PaidAccess = v.(bool)
	case "tool_pool_gated_off":
		s.ToolPoolGatedOff = v.(bool)
	}
}

// Usage window fields from lowercased headers, false when the gauge list is absent or unreadable
func parseWindowFields(lowered map[string]string, s *CreditsState) bool {
	raw, ok := lookupHeader(lowered, creditsGaugesHeader)
	if !ok {
		return false
	}
	gauges := ParseGauges(raw)
	if gauges == nil || len(AllScope(gauges)) == 0 {
		return false
	}
	s.Gauges = gauges
	if source := lowered["x-nous-credits-spend-source"]; source == "included" || source == "payg" {
		s.SpendSource = source
	}
	s.PausedReason = strings.TrimFunc(lowered["x-nous-credits-window-paused-reason"], unicode.IsSpace)
	switch strings.ToLower(strings.TrimFunc(lowered["x-nous-credits-keep-going-on-credits"], unicode.IsSpace)) {
	case "on":
		on := true
		s.KeepGoingOnCredits = &on
	case "off":
		off := false
		s.KeepGoingOnCredits = &off
	}
	return true
}

// ResponseIsUsageWindow reports response headers carrying complete usage window credits headers
func ResponseIsUsageWindow(h http.Header) bool {
	if len(h) == 0 {
		return false
	}
	state := ParseCreditsHeaders(h, "")
	return state != nil && state.IsUsageWindow()
}

type creditsFixture struct {
	remaining, subscription, limit, purchased string
	paid                                      bool
	reason                                    string
}

var devCreditsFixtures = map[string]creditsFixture{
	"healthy":         {"30.34", "18.00", "20.00", "12.34", true, ""},
	"sub_50pct":       {"10.00", "10.00", "20.00", "", true, ""},
	"sub_75pct":       {"5.00", "5.00", "20.00", "", true, ""},
	"sub_90pct":       {"2.00", "2.00", "20.00", "", true, ""},
	"grant_exhausted": {"12.34", "0.00", "20.00", "12.34", true, ""},
	"depleted":        {"0.00", "0.00", "", "0.00", false, "out_of_credits"},
	"debt":            {"0.00", "-5.00", "20.00", "0.00", false, "out_of_credits"},
}

// Exact micros of a dollar string truncated toward zero
func fixtureMicros(usd string) int64 {
	r, _ := new(big.Rat).SetString(usd)
	r.Mul(r, big.NewRat(1_000_000, 1))
	return new(big.Int).Quo(r.Num(), r.Denom()).Int64()
}

// DevFixtureCreditsState returns the fixture named by the fixture env only while dev credits is on
func DevFixtureCreditsState() *CreditsState {
	name := strings.TrimSpace(os.Getenv(DevCreditsFixtureEnv))
	if name == "" || !envTruthy(DevCreditsEnv) {
		return nil
	}
	if strings.ContainsRune(name, os.PathSeparator) || strings.Contains(name, "/") {
		data, err := os.ReadFile(name)
		if err != nil {
			return nil
		}
		name = strings.TrimSpace(strings.TrimPrefix(string(data), "\uFEFF"))
	}
	spec, ok := devCreditsFixtures[strings.ToLower(name)]
	if !ok {
		return nil
	}
	s := NewCreditsState()
	s.Version = 1
	s.RemainingMicros, s.RemainingUSD = fixtureMicros(spec.remaining), spec.remaining
	s.SubscriptionMicros, s.SubscriptionUSD = fixtureMicros(spec.subscription), spec.subscription
	s.PurchasedUSD = "0.00"
	if spec.purchased != "" {
		s.PurchasedMicros, s.PurchasedUSD = fixtureMicros(spec.purchased), spec.purchased
	}
	if spec.limit != "" {
		lim, usd := fixtureMicros(spec.limit), spec.limit
		s.SubscriptionLimitMicros, s.SubscriptionLimitUSD = &lim, &usd
		s.DenominatorKind = "subscription_cap"
	}
	s.PaidAccess = spec.paid
	s.DisabledReason = spec.reason
	s.FromHeader = true
	s.CapturedAt = float64(creditsNow().UnixNano()) / 1e9
	return s
}

func accountMoney(dollars *float64) (int64, string) {
	if dollars == nil {
		return 0, ""
	}
	return int64(math.RoundToEven(*dollars * 1_000_000)), fmt.Sprintf("%.2f", *dollars)
}

// Maps a Portal account snapshot into a header shaped seed state, nil without an account
func creditsStateFromAccount(info *NousPortalAccountInfo) *CreditsState {
	if info == nil {
		return nil
	}
	s := NewCreditsState()
	if acc := info.PaidServiceAccessInfo; acc != nil {
		s.RemainingMicros, s.RemainingUSD = accountMoney(acc.TotalUsableCredits)
		s.SubscriptionMicros, s.SubscriptionUSD = accountMoney(acc.SubscriptionCreditsRemaining)
		s.PurchasedMicros, s.PurchasedUSD = accountMoney(acc.PurchasedCreditsRemaining)
	}
	if sub := info.Subscription; sub != nil {
		if sub.MonthlyCredits != nil && *sub.MonthlyCredits > 0 {
			lim, usd := accountMoney(sub.MonthlyCredits)
			s.SubscriptionLimitMicros, s.SubscriptionLimitUSD = &lim, &usd
		}
		s.RolloverMicros, _ = accountMoney(sub.RolloverCredits)
	}
	if info.PaidServiceAccess != nil {
		s.PaidAccess = *info.PaidServiceAccess
	}
	if applyWindowSeedFields(info.UsageWindow, s) {
		s.SubscriptionMicros, s.SubscriptionUSD = 0, "0.00"
		s.SubscriptionLimitMicros, s.SubscriptionLimitUSD = nil, nil
		s.DenominatorKind = "usage_window"
	} else if s.SubscriptionLimitMicros != nil {
		s.DenominatorKind = "subscription_cap"
	}
	s.CapturedAt = float64(creditsNow().UnixNano()) / 1e9
	return s
}

// Copies Portal usage window fields onto a state, false unless the gauges carry an all scope allowance
func applyWindowSeedFields(uw *NousUsageWindowInfo, s *CreditsState) bool {
	if uw == nil {
		return false
	}
	gauges := ParseGauges(uw.Gauges)
	if gauges == nil || len(AllScope(gauges)) == 0 {
		return false
	}
	if s != nil {
		s.Gauges = gauges
		s.AutoTopUpEnabled = uw.AutoTopUpEnabled != nil && *uw.AutoTopUpEnabled
		s.KeepGoingOnCredits = uw.KeepGoingOnCredits
		s.PausedReason = uw.PausedReason
	}
	return true
}

// Bounded most recently used map keyed by session id
type sessionMRU[V any] struct {
	mu    sync.Mutex
	order []string
	items map[string]V
}

func (m *sessionMRU[V]) put(key string, v V) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.items == nil {
		m.items = map[string]V{}
	}
	m.dropLocked(key)
	m.items[key] = v
	m.order = append(m.order, key)
	if len(m.order) > seenUsageBandsMax {
		delete(m.items, m.order[0])
		m.order = m.order[1:]
	}
}

func (m *sessionMRU[V]) get(key string) (V, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.items[key]
	return v, ok
}

func (m *sessionMRU[V]) drop(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropLocked(key)
}

func (m *sessionMRU[V]) dropLocked(key string) {
	if _, ok := m.items[key]; !ok {
		return
	}
	delete(m.items, key)
	for i, k := range m.order {
		if k == key {
			m.order = append(m.order[:i:i], m.order[i+1:]...)
			break
		}
	}
}

type windowNoticeMemory struct {
	texts  map[string]string
	warned map[string]*int64
}

var (
	seenUsageBands     sessionMRU[any]
	seenWindowNotices  sessionMRU[windowNoticeMemory]
	windowNoticeKeys   = []string{CreditsWindowPaygKey, CreditsWindowLimitKey, CreditsDepletedKey}
	pricingWarmFlights sync.Map
)

// RememberShownBand records the usage band last evaluated for a session so a rebuilt capture keeps it
func RememberShownBand(sessionID string, band any) {
	if sessionID == "" {
		return
	}
	seenUsageBands.put(sessionID, band)
}

// RestoreWindowNotices loads the window lines this session already showed into a fresh latch once
func RestoreWindowNotices(sessionID string, latch map[string]any) {
	if sessionID == "" || latchBool(latch, "window_restored") {
		return
	}
	latch["window_restored"] = true
	saved, ok := seenWindowNotices.get(sessionID)
	if !ok || (len(saved.texts) == 0 && len(saved.warned) == 0) {
		return
	}
	active, texts := latchActive(latch), latchTexts(latch)
	for key, text := range saved.texts {
		if !active[key] {
			active[key] = true
			texts[key] = text
		}
	}
	warned := latchWarned(latch)
	for id, reset := range saved.warned {
		warned[id] = reset
	}
}

// ForgetWindowNotices drops the saved window lines of a session that left its window plan
func ForgetWindowNotices(sessionID string) { seenWindowNotices.drop(sessionID) }

// RememberWindowNotices records the window lines and heads ups this session shows now
func RememberWindowNotices(sessionID string, latch map[string]any) {
	if sessionID == "" {
		return
	}
	active := latchActive(latch)
	texts, _ := latch["texts"].(map[string]string)
	entry := windowNoticeMemory{texts: map[string]string{}, warned: map[string]*int64{}}
	for _, key := range windowNoticeKeys {
		if text, ok := texts[key]; ok && active[key] {
			entry.texts[key] = text
		}
	}
	if warned, ok := latch["window_warned"].(map[string]*int64); ok {
		for id, reset := range warned {
			entry.warned[id] = reset
		}
	}
	seenWindowNotices.put(sessionID, entry)
}

func creditsCtx() context.Context {
	return home.WithOverride(context.Background(), home.Process())
}

// ApplyKeepGoingChange adopts a new Keep going setting now, reruns the notices and refreshes the account
func ApplyKeepGoingChange(c *CreditsCapture, enabled bool) {
	c.mu.Lock()
	state := c.credits
	if state == nil || !state.IsUsageWindow() {
		c.mu.Unlock()
		return
	}
	next := *state
	next.KeepGoingOnCredits = &enabled
	c.credits = &next
	c.mu.Unlock()
	c.EmitCreditsNotices()
	ctx := creditsCtx()
	go GetNousPortalAccountInfo(ctx, true)
}

// Installs a seed state, primes the crossing gate, restores the band this session showed, reruns notices
func hydrateSeedState(c *CreditsCapture, state *CreditsState) {
	c.mu.Lock()
	c.credits = state
	if c.sessionStartMicros == nil {
		v := state.RemainingMicros
		c.sessionStartMicros = &v
	}
	if c.latch == nil {
		c.latch = NewCreditsLatch()
	}
	if _, ok := state.UsedFraction(); ok {
		c.latch["seen_below_90"] = true
		if prior, ok := seenUsageBands.get(c.SessionID); ok && prior != nil {
			c.latch["usage_band"] = prior
			latchActive(c.latch)[CreditsUsageKey] = true
		}
	}
	c.mu.Unlock()
	c.EmitCreditsNotices()
}

func warmNousPricingCache(ctx context.Context) {
	GetPricingForProvider(ctx, "nous", "", false, false)
}

// RewarmPricingBeforeDepletedNotice starts a catalog warm that reruns the policy and defers the depleted decision
func RewarmPricingBeforeDepletedNotice(c *CreditsCapture) bool {
	c.mu.Lock()
	provider, baseURL := c.Provider, c.BaseURL
	c.mu.Unlock()
	if provider != "nous" || baseURL == "" {
		return false
	}
	if len(PeekCachedPricing(baseURL)) > 0 || PricingFetchSuppressed(baseURL) {
		return false
	}
	if _, loaded := pricingWarmFlights.LoadOrStore(c, true); loaded {
		return false
	}
	ctx := creditsCtx()
	go func() {
		defer pricingWarmFlights.Delete(c)
		warmNousPricingCache(ctx)
		c.EmitCreditsNotices()
	}()
	return true
}

// SeedCreditsAtSessionStart hydrates the credits state from a fixture or the Portal so notices show at open
func SeedCreditsAtSessionStart(c *CreditsCapture) bool {
	c.mu.Lock()
	skip := c.Provider != "nous" || c.credits != nil
	c.mu.Unlock()
	if skip {
		return false
	}
	if fixture := DevFixtureCreditsState(); fixture != nil {
		hydrateSeedState(c, fixture)
		return true
	}
	ctx := creditsCtx()
	go func() {
		warmNousPricingCache(ctx)
		info := GetNousPortalAccountInfo(ctx, true)
		if c.Credits() != nil {
			c.EmitCreditsNotices()
			return
		}
		if state := creditsStateFromAccount(info); state != nil {
			hydrateSeedState(c, state)
		}
	}()
	return true
}
