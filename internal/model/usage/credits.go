package usage

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/providers"
)

const (
	creditsHeaderPrefix = "x-portal-credits-"
	gaugesHeader        = creditsHeaderPrefix + "window-gauges"

	kindSubscriptionCap = "subscription_cap"
	kindNone            = "none"
	kindUsageWindow     = "usage_window"
	spendIncluded       = "included"
	spendPAYG           = "payg"

	NoticeSticky = "sticky"
	NoticeTTL    = "ttl"

	NoticeKeyUsage          = "credits.usage"
	NoticeKeyWindowPAYG     = "credits.window_payg"
	NoticeKeyDepleted       = "credits.depleted"
	NoticeKeyWindowLimit    = "credits.window_limit"
	NoticeKeyRestored       = "credits.restored"
	NoticeKeyGrantSpent     = "credits.grant_spent"
	NoticeKeyWindowApproach = "credits.window."

	restoredTTLMS         = 8000
	windowApproachTTLMS   = 10_000
	windowApproachAt      = 0.90
	grantUnspentMinMicros = 10_000
	sessionMemoryMax      = 500
	planRetryAfter        = 120 * time.Second

	depletedText     = "✕ Credit access paused · run /topup to top up"
	pausedText       = "✕ Included usage paused · your subscription payment failed · update your card, or add credits in /topup to keep going"
	pausedOtherText  = "✕ Included usage paused · see Billing in the Portal, or add credits in /topup to keep going"
	topTierLimitText = "You've hit your monthly large-model limit"
	autoTopUpSuffix  = " · auto top-up is on"
	addCreditsStep   = "/topup to add credits"
	restoredText     = "✓ Credit access restored"
)

var usdPattern = regexp.MustCompile(`^-?\p{Nd}+\.\p{Nd}{2}\n?$`)

type usageBand struct {
	threshold float64
	level     string
	pct       int
}

var usageBands = [...]usageBand{{0.50, "info", 50}, {0.75, "warn", 75}, {0.90, "warn", 90}}

var windowNoticeKeys = [...]string{NoticeKeyWindowPAYG, NoticeKeyWindowLimit, NoticeKeyDepleted}

type CreditsState struct {
	Version                 int
	RemainingMicros         int64
	RemainingUSD            string
	SubscriptionMicros      int64
	SubscriptionUSD         string
	SubscriptionLimitMicros *int64
	SubscriptionLimitUSD    string
	RolloverMicros          int64
	PurchasedMicros         int64
	PurchasedUSD            string
	ToolPoolMicros          int64
	ToolPoolGatedOff        bool
	DenominatorKind         string
	PaidAccess              bool
	DisabledReason          string
	AsOfMS                  int64
	CapturedAt              time.Time
	FromHeader              bool
	Gauges                  []portal.WindowGauge
	SpendSource             string
	Unmetered               bool
	PausedReason            string
	AutoTopUpEnabled        bool
	KeepGoingOnCredits      *bool
}

func isFalse(b *bool) bool { return b != nil && !*b }

func isTrue(b *bool) bool { return b != nil && *b }

func (s *CreditsState) CreditsPay() bool {
	return s.PurchasedMicros > 0 && !isFalse(s.KeepGoingOnCredits)
}

func (s *CreditsState) Depleted() bool { return !s.PaidAccess }

func (s *CreditsState) IsUsageWindow() bool {
	return s.DenominatorKind == kindUsageWindow && len(portal.AllScope(s.Gauges)) > 0
}

func (s *CreditsState) BindingGauge() (portal.WindowGauge, bool) {
	if !s.IsUsageWindow() {
		return portal.WindowGauge{}, false
	}
	return portal.BindingGauge(s.Gauges)
}

func (s *CreditsState) TopTierGauge() (portal.WindowGauge, bool) {
	if !s.IsUsageWindow() {
		return portal.WindowGauge{}, false
	}
	return portal.TopTierGauge(s.Gauges)
}

func (s *CreditsState) UsedFraction() (float64, bool) {
	if s.IsUsageWindow() {
		g, _ := s.BindingGauge()
		return g.UsedFraction(), true
	}
	if s.SubscriptionLimitMicros == nil || *s.SubscriptionLimitMicros <= 0 {
		return 0, false
	}
	limit := *s.SubscriptionLimitMicros
	return max(0.0, min(1.0, float64(limit-s.SubscriptionMicros)/float64(limit))), true
}

func (s *CreditsState) WindowExhausted() bool {
	if !s.IsUsageWindow() {
		return false
	}
	return slices.ContainsFunc(portal.AllScope(s.Gauges), portal.WindowGauge.Full)
}

func (s *CreditsState) TopTierExhausted() bool {
	g, ok := s.TopTierGauge()
	return ok && g.Full()
}

type Notice struct {
	Text  string `json:"text"`
	Level string `json:"level"`
	Kind  string `json:"kind"`
	TTLMS int    `json:"ttl_ms,omitempty"`
	Key   string `json:"key,omitempty"`
	ID    string `json:"id,omitempty"`
}

func stickyNotice(text, level, key string) Notice {
	return Notice{Text: text, Level: level, Kind: NoticeSticky, Key: key, ID: key}
}

func ttlNotice(text, level, key string, ttl int) Notice {
	return Notice{Text: text, Level: level, Kind: NoticeTTL, TTLMS: ttl, Key: key, ID: key}
}

type resetMark struct {
	known bool
	ms    int64
}

func markOf(ms *int64) resetMark {
	if ms == nil {
		return resetMark{}
	}
	return resetMark{known: true, ms: *ms}
}

type CreditsLatch struct {
	active           map[string]bool
	texts            map[string]string
	seenBelow90      bool
	usageBand        int
	seenGrantUnspent bool
	windowWarned     map[string]resetMark
	windowRestored   bool
}

func NewCreditsLatch() *CreditsLatch {
	return &CreditsLatch{active: map[string]bool{}, texts: map[string]string{}, windowWarned: map[string]resetMark{}}
}

func formatWindowReset(resetsAtMS int64, now time.Time) string {
	if resetsAtMS <= 0 {
		return "the next reset"
	}
	nowMS := now.UnixMilli()
	if resetsAtMS <= nowMS {
		return "shortly"
	}
	stamp := time.UnixMilli(resetsAtMS).Local()
	left := resetsAtMS - nowMS
	if left >= 7*24*3600*1000 {
		return stamp.Format("Jan") + " " + strconv.Itoa(stamp.Day())
	}
	if left < 24*3600*1000 {
		return stamp.Format("15:04")
	}
	return stamp.Format("Mon 15:04")
}

func resetsSuffix(resetsAtMS *int64) string {
	if resetsAtMS == nil || *resetsAtMS <= 0 {
		return ""
	}
	return " · resets " + formatWindowReset(*resetsAtMS, time.Now())
}

func spentWindow(s *CreditsState) (string, *int64) {
	if g, ok := s.BindingGauge(); ok {
		return portal.GaugeLimitName(g), g.ResetsAtMS
	}
	return "daily", nil
}

func windowApproachText(g portal.WindowGauge) string {
	pct := min(99, int(g.UsedFraction()*100))
	return "⚠ " + strconv.Itoa(pct) + "% of your " + portal.GaugeLimitName(g) + " limit used" + resetsSuffix(g.ResetsAtMS)
}

func stopGauge(s *CreditsState, modelIsTopTier *bool) (portal.WindowGauge, bool) {
	if !s.IsUsageWindow() {
		return portal.WindowGauge{}, false
	}
	refused := s.FromHeader && s.SpendSource == ""
	g, ok := BlockingGauge(s.Gauges, false, refused && !isFalse(modelIsTopTier))
	if !ok && refused && s.TopTierExhausted() {
		return s.TopTierGauge()
	}
	return g, ok
}

func creditsStep(action string) string {
	switch action {
	case portal.UseCredits:
		return "turn on Keep going in /topup"
	case portal.AddCreditsAndKeepGoing:
		return "add credits and turn on Keep going in /topup"
	}
	return addCreditsStep
}

func WindowHardStop(s *CreditsState, modelIsFree bool, modelIsTopTier *bool) (string, bool) {
	if s == nil {
		return "", false
	}
	purchased := s.PurchasedMicros
	if s.PausedReason != "" && purchased <= 0 {
		if portal.PausedForPayment(s.PausedReason) {
			return pausedText, true
		}
		return pausedOtherText, true
	}
	if !s.PaidAccess {
		if modelIsFree {
			return "", false
		}
		return depletedText, true
	}
	if modelIsFree || s.CreditsPay() || s.Unmetered {
		return "", false
	}
	action := creditsStep(portal.CreditsAction(s.KeepGoingOnCredits, purchased))
	g, ok := stopGauge(s, modelIsTopTier)
	if !ok {
		return "", false
	}
	if g.Scope == portal.ScopeTopTier {
		auto := ""
		if s.AutoTopUpEnabled && action == addCreditsStep {
			auto = autoTopUpSuffix
		}
		return "✕ " + topTierLimitText + resetsSuffix(g.ResetsAtMS) + " · " + action + auto, true
	}
	return "✕ You've hit your " + portal.GaugeLimitName(g) + " limit" + resetsSuffix(g.ResetsAtMS) + " · " + action, true
}

func WindowStopResetsAt(s *CreditsState, modelIsTopTier *bool) (time.Time, bool) {
	if s == nil || s.Unmetered {
		return time.Time{}, false
	}
	g, ok := stopGauge(s, modelIsTopTier)
	if !ok || g.ResetsAtMS == nil {
		return time.Time{}, false
	}
	return time.UnixMilli(*g.ResetsAtMS), true
}

func windowPAYGText(s *CreditsState) string {
	if s.SpendSource != spendPAYG || s.PurchasedMicros <= 0 {
		return ""
	}
	var text string
	switch {
	case s.WindowExhausted():
		name, resets := spentWindow(s)
		text = "• You've hit your " + name + " limit" + resetsSuffix(resets) + " · now using credits"
	case s.TopTierExhausted():
		g, _ := s.TopTierGauge()
		text = "• " + topTierLimitText + resetsSuffix(g.ResetsAtMS) + " · large models now use credits"
	default:
		text = "• Now using credits"
	}
	if s.AutoTopUpEnabled {
		text += autoTopUpSuffix
	}
	return text
}

func (l *CreditsLatch) reconcile(key, text, level string, show *[]Notice, clear *[]string) {
	if text != "" && (!l.active[key] || l.texts[key] != text) {
		if l.active[key] {
			*clear = append(*clear, key)
		}
		*show = append(*show, stickyNotice(text, level, key))
		l.active[key] = true
		l.texts[key] = text
	} else if text == "" && l.active[key] {
		*clear = append(*clear, key)
		delete(l.active, key)
		delete(l.texts, key)
	}
}

func evaluateWindowNotices(s *CreditsState, l *CreditsLatch, modelIsFree, modelIsTopTier bool) ([]Notice, []string) {
	var show []Notice
	var clear []string
	if l.active[NoticeKeyUsage] {
		l.reconcile(NoticeKeyUsage, "", "info", &show, &clear)
		l.usageBand = 0
	}
	for _, g := range s.Gauges {
		relevant := !modelIsFree && (g.Scope != portal.ScopeTopTier || modelIsTopTier)
		if relevant && !g.Full() && g.UsedFraction() >= windowApproachAt && l.windowWarned[g.ID] != markOf(g.ResetsAtMS) {
			l.windowWarned[g.ID] = markOf(g.ResetsAtMS)
			key := NoticeKeyWindowApproach + g.ID
			show = append(show, ttlNotice(windowApproachText(g), "warn", key, windowApproachTTLMS))
		}
	}
	if l.active[NoticeKeyGrantSpent] {
		clear = append(clear, NoticeKeyGrantSpent)
		delete(l.active, NoticeKeyGrantSpent)
	}
	topTier := modelIsTopTier
	stop, stopped := WindowHardStop(s, modelIsFree, &topTier)
	limit := stopped && stop != pausedText && stop != pausedOtherText && stop != depletedText
	wasStopped := l.active[NoticeKeyDepleted] || l.active[NoticeKeyWindowLimit]
	limitText, depletedLine := "", stop
	if limit {
		limitText, depletedLine = stop, ""
	}
	l.reconcile(NoticeKeyWindowLimit, limitText, "error", &show, &clear)
	l.reconcile(NoticeKeyDepleted, depletedLine, "error", &show, &clear)
	if wasStopped && !stopped && s.PaidAccess && !modelIsFree {
		show = append(show, ttlNotice(restoredText, "success", NoticeKeyRestored, restoredTTLMS))
	}
	payg := ""
	if !stopped {
		payg = windowPAYGText(s)
	}
	l.reconcile(NoticeKeyWindowPAYG, payg, "warn", &show, &clear)
	return show, clear
}

func EvaluateCreditsNotices(s *CreditsState, l *CreditsLatch, modelIsFree, modelIsTopTier bool) (show []Notice, clear []string) {
	if s.IsUsageWindow() {
		return evaluateWindowNotices(s, l, modelIsFree, modelIsTopTier)
	}
	for _, key := range []string{NoticeKeyWindowPAYG, NoticeKeyWindowLimit} {
		if l.active[key] {
			clear = append(clear, key)
			delete(l.active, key)
			delete(l.texts, key)
		}
	}
	uf, known := s.UsedFraction()
	if known && uf < usageBands[0].threshold {
		l.seenBelow90 = true
	}
	if known && uf < 1.0 && s.SubscriptionMicros >= grantUnspentMinMicros {
		l.seenGrantUnspent = true
	}
	var current *usageBand
	if known && s.PurchasedMicros <= 0 {
		for i := len(usageBands) - 1; i >= 0; i-- {
			if uf >= usageBands[i].threshold {
				current = &usageBands[i]
				break
			}
		}
	}
	target := 0
	if current != nil && l.seenBelow90 {
		target = current.pct
	}
	if target != l.usageBand {
		if l.active[NoticeKeyUsage] {
			clear = append(clear, NoticeKeyUsage)
			delete(l.active, NoticeKeyUsage)
		}
		if target != 0 {
			limit := int64(0)
			if s.SubscriptionLimitMicros != nil {
				limit = *s.SubscriptionLimitMicros
			}
			usedUSD := "?"
			if limit != 0 {
				usedUSD = fixed(float64(max(0, min(limit, limit-s.SubscriptionMicros)))/1_000_000, 2)
			}
			mark := "•"
			if current.level == "warn" {
				mark = "⚠"
			}
			capUSD := s.SubscriptionLimitUSD
			if capUSD == "" {
				capUSD = "?"
			}
			show = append(show, stickyNotice(mark+" You've used $"+usedUSD+" of your $"+capUSD+" cap", current.level, NoticeKeyUsage))
			l.active[NoticeKeyUsage] = true
		}
		l.usageBand = target
	}
	grant := s.DenominatorKind == kindSubscriptionCap && known && uf >= 1.0 && s.PurchasedMicros > 0
	if grant && !l.active[NoticeKeyGrantSpent] && l.seenGrantUnspent {
		show = append(show, stickyNotice("• Grant spent · $"+s.PurchasedUSD+" top-up left", "info", NoticeKeyGrantSpent))
		l.active[NoticeKeyGrantSpent] = true
		l.seenGrantUnspent = false
	} else if l.active[NoticeKeyGrantSpent] && !grant {
		clear = append(clear, NoticeKeyGrantSpent)
		delete(l.active, NoticeKeyGrantSpent)
	}
	showDepleted := !s.PaidAccess && !modelIsFree
	if showDepleted && !l.active[NoticeKeyDepleted] {
		show = append(show, stickyNotice(depletedText, "error", NoticeKeyDepleted))
		l.active[NoticeKeyDepleted] = true
	} else if l.active[NoticeKeyDepleted] && !showDepleted {
		clear = append(clear, NoticeKeyDepleted)
		delete(l.active, NoticeKeyDepleted)
		if s.PaidAccess {
			show = append(show, ttlNotice(restoredText, "success", NoticeKeyRestored, restoredTTLMS))
		}
	}
	return show, clear
}

func lowerHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		out[strings.ToLower(k)] = strings.Join(vs, ", ")
	}
	return out
}

func safeInt(raw string) (int64, bool) {
	text := strings.TrimSpace(raw)
	if strings.Contains(text, "_") {
		for i, r := range text {
			if r != '_' {
				continue
			}
			before, _ := utf8.DecodeLastRuneInString(text[:i])
			after, _ := utf8.DecodeRuneInString(text[i+1:])
			if !unicode.IsDigit(before) || !unicode.IsDigit(after) {
				return 0, false
			}
		}
		text = strings.ReplaceAll(text, "_", "")
	}
	v, err := strconv.ParseInt(text, 10, 64)
	return v, err == nil
}

type headerParse struct {
	h      map[string]string
	window bool
	bad    bool
}

func (p *headerParse) micros(header string, signed, windowOptional bool) int64 {
	raw, ok := p.h[header]
	if !ok {
		if !(p.window && windowOptional) {
			p.bad = true
		}
		return 0
	}
	v, valid := safeInt(raw)
	if !valid || (!signed && v < 0) {
		p.bad = true
	}
	return v
}

func (p *headerParse) usd(header string, windowOptional bool) string {
	raw, ok := p.h[header]
	if !ok {
		if !(p.window && windowOptional) {
			p.bad = true
		}
		return ""
	}
	if !usdPattern.MatchString(raw) {
		p.bad = true
	}
	return raw
}

func (p *headerParse) flag(header string, def bool) bool {
	raw, ok := p.h[header]
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true":
		return true
	case "false":
		return false
	}
	p.bad = true
	return def
}

func gaugesFromHeader(value string, present bool) []portal.WindowGauge {
	if !present {
		return nil
	}
	quoted, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return portal.ParseGauges(quoted)
}

func (t *Tracker) ParseCreditsHeaders(h http.Header) *CreditsState {
	lowered := lowerHeaders(h)
	versionRaw, ok := lowered[creditsHeaderPrefix+"version"]
	if !ok {
		return nil
	}
	version, ok := safeInt(versionRaw)
	if !ok {
		return nil
	}
	if version != 1 {
		if version > 1 && t.versionWarned.CompareAndSwap(false, true) {
			slog.Warn("credits header version unsupported, ignoring — update icebath", "version", version)
		}
		return nil
	}
	kind, present := lowered[creditsHeaderPrefix+"denominator-kind"]
	if !present {
		kind = kindNone
	}
	p := &headerParse{h: lowered, window: kind == kindUsageWindow}
	s := &CreditsState{
		Version:            1,
		RemainingMicros:    p.micros(creditsHeaderPrefix+"remaining-micros", false, true),
		SubscriptionMicros: p.micros(creditsHeaderPrefix+"subscription-micros", true, true),
		RolloverMicros:     p.micros(creditsHeaderPrefix+"rollover-micros", false, true),
		PurchasedMicros:    p.micros(creditsHeaderPrefix+"purchased-micros", false, true),
		AsOfMS:             p.micros(creditsHeaderPrefix+"as-of-ms", false, true),
		RemainingUSD:       p.usd(creditsHeaderPrefix+"remaining-usd", true),
		SubscriptionUSD:    p.usd(creditsHeaderPrefix+"subscription-usd", true),
		PurchasedUSD:       p.usd(creditsHeaderPrefix+"purchased-usd", true),
		PaidAccess:         p.flag(creditsHeaderPrefix+"paid-access", true),
		ToolPoolGatedOff:   p.flag("x-portal-tool-pool-gated-off", false),
		DenominatorKind:    kind,
	}
	if _, has := lowered["x-portal-tool-pool-micros"]; has {
		s.ToolPoolMicros = p.micros("x-portal-tool-pool-micros", false, false)
	}
	_, hasLimit := lowered[creditsHeaderPrefix+"subscription-limit-micros"]
	_, hasLimitUSD := lowered[creditsHeaderPrefix+"subscription-limit-usd"]
	if hasLimit && hasLimitUSD {
		limit := p.micros(creditsHeaderPrefix+"subscription-limit-micros", false, false)
		s.SubscriptionLimitMicros = &limit
		s.SubscriptionLimitUSD = p.usd(creditsHeaderPrefix+"subscription-limit-usd", false)
	}
	if p.bad || (kind != kindSubscriptionCap && kind != kindNone && kind != kindUsageWindow) {
		return nil
	}
	if p.window {
		gaugesRaw, has := lowered[gaugesHeader]
		gauges := gaugesFromHeader(gaugesRaw, has)
		if len(gauges) == 0 || len(portal.AllScope(gauges)) == 0 {
			return nil
		}
		s.Gauges = gauges
		if source := lowered[creditsHeaderPrefix+"spend-source"]; source == spendIncluded || source == spendPAYG {
			s.SpendSource = source
		}
		s.PausedReason = strings.TrimSpace(lowered[creditsHeaderPrefix+"window-paused-reason"])
		switch strings.ToLower(strings.TrimSpace(lowered[creditsHeaderPrefix+"keep-going-on-credits"])) {
		case "on":
			s.KeepGoingOnCredits = new(true)
		case "off":
			s.KeepGoingOnCredits = new(false)
		}
	}
	s.DisabledReason = lowered[creditsHeaderPrefix+"disabled-reason"]
	s.CapturedAt = time.Now()
	s.FromHeader = true
	return s
}

func (t *Tracker) ResponseIsUsageWindow(h http.Header) bool {
	if len(h) == 0 {
		return false
	}
	s := t.ParseCreditsHeaders(h)
	return s != nil && s.IsUsageWindow()
}

func HasPurchasedHeader(h http.Header) bool {
	_, ok := lowerHeaders(h)[creditsHeaderPrefix+"purchased-micros"]
	return ok
}

func windowSeedReady(window *portal.UsageWindowInfo) bool {
	return window != nil && len(portal.AllScope(window.Gauges)) > 0
}

func money(dollars *float64) (int64, string) {
	if dollars == nil {
		return 0, ""
	}
	return int64(math.RoundToEven(*dollars * 1_000_000)), fixed(*dollars, 2)
}

func creditsStateFromAccount(info *portal.AccountInfo) *CreditsState {
	if info == nil {
		return nil
	}
	access, sub := info.PaidServiceAccessInfo, info.Subscription
	var total, subRemaining, purchased, monthly, rollover *float64
	if access != nil {
		total, subRemaining, purchased = access.TotalUsableCredits, access.SubscriptionCreditsRemaining, access.PurchasedCreditsRemaining
	}
	if sub != nil {
		monthly, rollover = sub.MonthlyCredits, sub.RolloverCredits
	}
	s := &CreditsState{PaidAccess: true, CapturedAt: time.Now(), DenominatorKind: kindNone}
	s.RemainingMicros, s.RemainingUSD = money(total)
	s.SubscriptionMicros, s.SubscriptionUSD = money(subRemaining)
	s.PurchasedMicros, s.PurchasedUSD = money(purchased)
	s.RolloverMicros, _ = money(rollover)
	if info.PaidServiceAccess != nil {
		s.PaidAccess = *info.PaidServiceAccess
	}
	if windowSeedReady(info.UsageWindow) {
		window := info.UsageWindow
		s.SubscriptionMicros, s.SubscriptionUSD = 0, "0.00"
		s.DenominatorKind = kindUsageWindow
		s.Gauges = slices.Clone(window.Gauges)
		s.AutoTopUpEnabled = isTrue(window.AutoTopUpEnabled)
		s.KeepGoingOnCredits = window.KeepGoingOnCredits
		s.PausedReason = window.PausedReason
		return s
	}
	if monthly != nil && *monthly > 0 {
		limit, limitUSD := money(monthly)
		s.SubscriptionLimitMicros, s.SubscriptionLimitUSD = &limit, limitUSD
		s.DenominatorKind = kindSubscriptionCap
	}
	return s
}

func (t *Tracker) SeedCredits(ctx context.Context) *CreditsState {
	ctx = t.bind(ctx)
	t.warmPortalPricing(ctx)
	return creditsStateFromAccount(t.portal.AccountInfo(ctx, portal.AccountOptions{ForceFresh: true}))
}

func (t *Tracker) warmPortalPricing(ctx context.Context) {
	if cat := t.catalog(); cat != nil {
		cat.PricingForProvider(ctx, providers.ProviderPortal, "", false, false)
	}
}

func (t *Tracker) PrimeSeedLatch(sessionID string, l *CreditsLatch, s *CreditsState) {
	if l == nil || s == nil {
		return
	}
	if _, known := s.UsedFraction(); !known {
		return
	}
	l.seenBelow90 = true
	if sessionID == "" {
		return
	}
	t.mu.Lock()
	prior, ok := t.shownBands.get(sessionID)
	t.mu.Unlock()
	if ok && prior != 0 {
		l.usageBand = prior
		l.active[NoticeKeyUsage] = true
	}
}

func (t *Tracker) RememberShownBand(sessionID string, l *CreditsLatch) {
	if sessionID == "" || l == nil {
		return
	}
	t.mu.Lock()
	t.shownBands.put(sessionID, l.usageBand)
	t.mu.Unlock()
}

type windowMemory struct {
	texts  map[string]string
	warned map[string]resetMark
}

func (t *Tracker) RestoreWindowNotices(sessionID string, l *CreditsLatch) {
	if sessionID == "" || l == nil || l.windowRestored {
		return
	}
	l.windowRestored = true
	t.mu.Lock()
	saved, ok := t.windowNotices.get(sessionID)
	t.mu.Unlock()
	if !ok {
		return
	}
	for key, text := range saved.texts {
		if !l.active[key] {
			l.active[key] = true
			l.texts[key] = text
		}
	}
	for id, mark := range saved.warned {
		l.windowWarned[id] = mark
	}
}

func (t *Tracker) RememberWindowNotices(sessionID string, l *CreditsLatch) {
	if sessionID == "" || l == nil {
		return
	}
	entry := windowMemory{texts: map[string]string{}, warned: map[string]resetMark{}}
	for _, key := range windowNoticeKeys {
		if text, ok := l.texts[key]; ok && l.active[key] {
			entry.texts[key] = text
		}
	}
	for id, mark := range l.windowWarned {
		entry.warned[id] = mark
	}
	t.mu.Lock()
	t.windowNotices.put(sessionID, entry)
	t.mu.Unlock()
}

func (t *Tracker) ForgetWindowNotices(sessionID string) {
	t.mu.Lock()
	t.windowNotices.del(sessionID)
	t.mu.Unlock()
}

func (t *Tracker) IsFreeTierModel(model, baseURL string) bool {
	if model == "" {
		return false
	}
	if strings.HasSuffix(model, ":free") || strings.HasPrefix(model, "stealth/") {
		return true
	}
	if baseURL == "" {
		return false
	}
	if portal.RouteIsWelcomeHost(baseURL) {
		return true
	}
	cat := t.catalog()
	if cat == nil {
		return false
	}
	pricing := cat.PeekCachedPricing(baseURL)
	return pricing.Len() > 0 && IsFree(model, pricing)
}

type PricingWarm struct {
	running atomic.Bool
}

func (t *Tracker) RewarmBeforeDepletedNotice(ctx context.Context, w *PricingWarm, provider, baseURL string, rerun func()) bool {
	if provider != providers.ProviderPortal || baseURL == "" || w == nil {
		return false
	}
	cat := t.catalog()
	if cat == nil || cat.PeekCachedPricing(baseURL).Len() > 0 || cat.PricingFetchSuppressed(baseURL) {
		return false
	}
	if !w.running.CompareAndSwap(false, true) {
		return false
	}
	if !t.spawn(ctx, func(bg context.Context) {
		defer w.running.Store(false)
		t.warmPortalPricing(bg)
		if rerun != nil {
			rerun()
		}
	}) {
		w.running.Store(false)
		return false
	}
	return true
}

func (t *Tracker) KeepGoingChanged(ctx context.Context, s *CreditsState, enabled bool) (*CreditsState, bool) {
	if s == nil || !s.IsUsageWindow() {
		return nil, false
	}
	next := *s
	next.KeepGoingOnCredits = new(enabled)
	t.spawn(ctx, func(bg context.Context) {
		t.portal.AccountInfo(bg, portal.AccountOptions{ForceFresh: true})
	})
	return &next, true
}

type planAttempt struct {
	seen string
	at   time.Time
}

func fullIDs(gauges []portal.WindowGauge) string {
	var ids []string
	for _, g := range gauges {
		if g.Full() {
			ids = append(ids, g.ID)
		}
	}
	slices.Sort(ids)
	return strings.Join(slices.Compact(ids), "\x00")
}

func snapshotFullIDs(info *portal.AccountInfo) (string, bool) {
	if info == nil || !windowSeedReady(info.UsageWindow) {
		return "", false
	}
	return fullIDs(info.UsageWindow.Gauges), true
}

func planSnapshotIsStale(s *CreditsState, info *portal.AccountInfo) bool {
	if s == nil || info == nil || info.Source == portal.AccountSourceError {
		return false
	}
	onWindow := s.IsUsageWindow()
	ids, isWindow := snapshotFullIDs(info)
	if onWindow != isWindow {
		return true
	}
	if !onWindow {
		return false
	}
	return fullIDs(s.Gauges) != ids
}

func (t *Tracker) clearPlanAttempt() {
	t.mu.Lock()
	t.planLast = nil
	t.mu.Unlock()
}

func (t *Tracker) SyncPlanSnapshot(ctx context.Context, s *CreditsState) bool {
	if s == nil {
		return false
	}
	ctx = t.bind(ctx)
	onWindow := s.IsUsageWindow()
	if !onWindow {
		if _, cachedWindow := snapshotFullIDs(t.portal.CachedAccountInfo(ctx, true)); !cachedWindow {
			t.clearPlanAttempt()
			return false
		}
	}
	snapshot := t.portal.CachedAccountInfo(ctx, true)
	if !planSnapshotIsStale(s, snapshot) {
		t.clearPlanAttempt()
		return false
	}
	seen := strconv.FormatBool(onWindow) + "|" + fullIDs(s.Gauges)
	_, snapshotWindow := snapshotFullIDs(snapshot)
	planChanged := onWindow != snapshotWindow
	now := time.Now()
	t.mu.Lock()
	if t.planInflight || (t.planLast != nil && t.planLast.seen == seen && now.Sub(t.planLast.at) < planRetryAfter) {
		t.mu.Unlock()
		return false
	}
	t.planInflight = true
	t.planLast = &planAttempt{seen: seen, at: now}
	t.mu.Unlock()
	started := t.spawn(ctx, func(bg context.Context) {
		defer func() {
			t.mu.Lock()
			t.planInflight = false
			t.mu.Unlock()
		}()
		if info := t.portal.AccountInfo(bg, portal.AccountOptions{ForceFresh: true}); info.Source == portal.AccountSourceError {
			t.clearPlanAttempt()
		}
		if planChanged {
			if cat := t.catalog(); cat != nil {
				cat.PricingForProvider(bg, providers.ProviderPortal, "", true, false)
			}
		}
	})
	if !started {
		t.mu.Lock()
		t.planInflight = false
		t.mu.Unlock()
	}
	return started
}
