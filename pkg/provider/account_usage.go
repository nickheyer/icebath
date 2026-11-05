package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	accountUsageDepletedLine        = "Status: access depleted — top up to restore"
	accountUsageTopTierUsedUp       = "Monthly large-model usage is used up."
	accountUsagePausedLine          = "Status: included usage paused. Your subscription payment failed. Update your card to restore it. Until then, Hermes uses your credits."
	accountUsagePausedOtherLine     = "Status: included usage paused. See Billing in the Portal. Until then, Hermes uses your credits."
	accountUsageKeepGoingOffLine    = "Keep going on credits: off. Hermes waits for the reset (turn on in /topup)."
	accountUsageKeepGoingOnLine     = "Keep going on credits: on. Uses your credits when included usage runs out."
	accountUsageDevFixtureTail      = "(dev fixture — ICEBATH_DEV_CREDITS_FIXTURE)"
	codexWindowExhaustedPercent     = 100.0
	codexUsageRequestTimeout        = 15 * time.Second
	openrouterUsageRequestTimeout   = 10 * time.Second
	anthropicOAuthUsageURL          = "https://api.anthropic.com/api/oauth/usage"
	defaultCodexUsageBackendBaseURL = "https://chatgpt.com/backend-api/codex"
)

// Wall clock bound on a plugin profile account usage hook, shared by every usage surface
var PluginUsageHookDeadline = 10 * time.Second

// Seams over the clock, HTTP clients, credential reads and portal fetch, replaced in tests
var (
	accountUsageNow      = time.Now
	accountUsageClientFn = func(timeout time.Duration) *http.Client {
		return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	codexUsageResolveFn = func(env PoolEnv, force bool) (map[string]any, error) {
		return resolveCodexRuntimeCredentials(env, CodexResolveOptions{ForceRefresh: force})
	}
	codexUsageReadTokensFn = func(env PoolEnv) (map[string]any, error) {
		data, err := readCodexTokens(env, true)
		return data.tokens, err
	}
	codexUsageLoadPoolFn = func(env PoolEnv) codexUsagePool {
		if p := LoadPool(env, "openai-codex"); p != nil {
			return p
		}
		return nil
	}
	codexUsageClearCooldownsFn = func(env PoolEnv) { ClearCodexPoolQuotaCooldowns(env, "") }
	anthropicUsageTokenFn      = func(env PoolEnv) string { return ResolveAnthropicToken(env, "") }
	openrouterUsageRuntimeFn   = func(ctx context.Context, baseURL, apiKey string) (*Runtime, error) {
		return modelsResolver(ctx).ResolveRuntime("openrouter", apiKey, baseURL, nil, "")
	}
	accountUsageProfileFn   = GetProviderProfile
	portalAccountFetchFn    = GetNousPortalAccountInfo
	accountUsageLoggedInFn  = NousLoggedIn
	accountUsageFixtureFn   = DevFixtureCreditsState
	accountUsageEnvFn       = modelsPoolEnv
	errPortalAccountTimeout = errors.New("portal account fetch timed out")
)

// codexUsagePool is the credential pool surface the Codex usage reads need
type codexUsagePool interface {
	Select(model string) (PooledCredential, bool)
	TryRefreshMatching(apiKeyHint, credentialID string) (PooledCredential, bool)
}

// AccountUsageWindow is one quota window, scope account or model
type AccountUsageWindow struct {
	Label       string
	UsedPercent *float64
	ResetAt     *time.Time
	Detail      string
	Scope       string
}

// AccountUsageSnapshot is one provider account usage reading for usage surfaces and the picker
type AccountUsageSnapshot struct {
	Provider          string
	Source            string
	FetchedAt         time.Time
	Title             string
	Plan              string
	Windows           []AccountUsageWindow
	Details           []string
	UnavailableReason string
	Identity          string
	Raw               map[string]any
	UsageWindow       bool
}

// Available reports windows or details to show and no unavailable reason
func (s *AccountUsageSnapshot) Available() bool {
	return (len(s.Windows) > 0 || len(s.Details) > 0) && s.UnavailableReason == ""
}

// NewAccountUsageSnapshot stamps a snapshot fetched now with the default title
func NewAccountUsageSnapshot(provider, source string, windows []AccountUsageWindow, details []string) *AccountUsageSnapshot {
	return &AccountUsageSnapshot{Provider: provider, Source: source, FetchedAt: accountUsageNow().UTC(), Title: "Account limits",
		Windows: windows, Details: details}
}

func accountWindow(label string, used *float64, resetAt *time.Time, detail string) AccountUsageWindow {
	return AccountUsageWindow{Label: label, UsedPercent: used, ResetAt: resetAt, Detail: detail, Scope: "account"}
}

func titleCaseSlug(value any) string {
	cleaned := strings.TrimSpace(auStr(value))
	if cleaned == "" {
		return ""
	}
	return pyTitle(strings.NewReplacer("_", " ", "-", " ").Replace(cleaned))
}

// Reset time of epoch seconds or ISO text, nil for empty or unparseable text, an error for an out of range epoch
func parseUsageDT(value any) (*time.Time, error) {
	if value == nil || value == "" {
		return nil, nil
	}
	if auIsNum(value) {
		f, err := auFloat(value)
		if err != nil {
			return nil, err
		}
		if math.IsNaN(f) {
			return nil, errors.New("cannot convert float NaN to integer")
		}
		if math.IsInf(f, 0) || math.Abs(f) > 1e15 {
			return nil, errors.New("timestamp out of range for platform time_t")
		}
		sec, frac := math.Modf(f)
		t := time.Unix(int64(sec), int64(math.RoundToEven(frac*1e6))*1000).UTC()
		if t.Year() < 1 || t.Year() > 9999 {
			return nil, errors.New("year is out of range")
		}
		return &t, nil
	}
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return nil, nil
	}
	t, ok := parseISO(strings.TrimSpace(text))
	if !ok {
		return nil, nil
	}
	return &t, nil
}

// Local reset stamp with the time left, now when already past
func formatUsageReset(dt *time.Time) string {
	if dt == nil {
		return "unknown"
	}
	stamp := dt.Local().Format("2006-01-02 15:04 MST")
	total := int64(dt.Sub(accountUsageNow()).Seconds())
	if total <= 0 {
		return fmt.Sprintf("now (%s)", stamp)
	}
	hours, minutes := total/3600, (total%3600)/60
	if hours >= 24 {
		return fmt.Sprintf("in %dd %dh (%s)", hours/24, hours%24, stamp)
	}
	if hours > 0 {
		return fmt.Sprintf("in %dh %dm (%s)", hours, minutes, stamp)
	}
	return fmt.Sprintf("in %dm (%s)", minutes, stamp)
}

// Compact resets in phrase without a wall clock stamp, resets soon when unknown
func usageResetPhrase(dt *time.Time) string {
	if dt == nil {
		return "resets soon"
	}
	ms := dt.UnixMilli()
	return FormatResetsIn(&ms, nil)
}

// RenderAccountUsageLines renders a snapshot as usage lines, the stacked bar layout for usage window plans
func RenderAccountUsageLines(snapshot *AccountUsageSnapshot, markdown bool) []string {
	if snapshot == nil {
		return []string{}
	}
	if snapshot.UsageWindow {
		return renderUsageWindowLines(snapshot)
	}
	bold := ""
	if markdown {
		bold = "**"
	}
	plan := ""
	if snapshot.Plan != "" {
		plan = " (" + snapshot.Plan + ")"
	}
	lines := []string{"📈 " + bold + snapshot.Title + bold, "Provider: " + snapshot.Provider + plan}
	for _, w := range snapshot.Windows {
		var base string
		if w.UsedPercent == nil {
			base = w.Label + ": unavailable"
		} else {
			used := *w.UsedPercent
			base = fmt.Sprintf("%s: %d%% remaining (%d%% used)", w.Label, max(0, pyRoundInt(100-used)), max(0, pyRoundInt(used)))
		}
		if w.ResetAt != nil {
			base += " • resets " + formatUsageReset(w.ResetAt)
		} else if w.Detail != "" {
			base += " • " + w.Detail
		}
		lines = append(lines, base)
	}
	lines = append(lines, snapshot.Details...)
	if snapshot.UnavailableReason != "" {
		lines = append(lines, "Unavailable: "+snapshot.UnavailableReason)
	}
	return lines
}

func pyRoundInt(v float64) int { return int(math.RoundToEven(v)) }

// Plain stacked bar lines with no emoji, ANSI or markdown so terminal widths stay exact
func renderUsageWindowLines(snapshot *AccountUsageSnapshot) []string {
	var lines []string
	if snapshot.Plan != "" {
		lines = []string{snapshot.Plan}
	} else {
		lines = []string{snapshot.Title, "Provider: " + snapshot.Provider}
	}
	labelW := 0
	for _, w := range snapshot.Windows {
		labelW = max(labelW, utf8.RuneCountInString(w.Label))
	}
	for _, w := range snapshot.Windows {
		label := w.Label
		if labelW > 0 {
			label += strings.Repeat(" ", labelW-utf8.RuneCountInString(w.Label))
		}
		var base string
		if w.UsedPercent == nil {
			base = label + "  unavailable"
		} else {
			used := math.Max(0, math.Min(100, *w.UsedPercent)) / 100
			bar, pct := RenderBar(used, BarWidth), FormatPercentUsed(used)
			if bar != "" {
				base = label + "  " + bar + "  " + pct + " used"
			} else {
				base = label + "  " + pct + " used"
			}
		}
		if w.ResetAt != nil {
			base += "  ·  " + usageResetPhrase(w.ResetAt)
		} else if w.Detail != "" {
			base += "  ·  " + w.Detail
		}
		lines = append(lines, base)
	}
	lines = append(lines, snapshot.Details...)
	if snapshot.UnavailableReason != "" {
		lines = append(lines, "Unavailable: "+snapshot.UnavailableReason)
	}
	return lines
}

// Dollars with thousands separators and two decimals, sign after the dollar like Python formatting
func fmtUsageUSD(d float64) string {
	sign := ""
	if d < 0 || (d == 0 && math.Signbit(d)) {
		sign = "-"
		d = -d
	}
	text := strconv.FormatFloat(d, 'f', 2, 64)
	whole, cents, _ := strings.Cut(text, ".")
	var b strings.Builder
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return "$" + sign + b.String() + "." + cents
}

func finiteRef(v *float64) bool { return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) }

// Nous snapshot with tail lines appended, nil when there is nothing to show
func nousUsageSnapshot(windows []AccountUsageWindow, details, tail []string, source, plan string, usageWindow bool) *AccountUsageSnapshot {
	if len(windows) == 0 && len(details) == 0 {
		return nil
	}
	s := NewAccountUsageSnapshot("nous", source, windows, append(details, tail...))
	s.Title, s.Plan, s.UsageWindow = "Nous credits", plan, usageWindow
	return s
}

// One usage window gauge row from basis points and an epoch ms reset, unknown resets keep the row
func bpWindow(label string, usedBp int, resetsAtMs *int64) AccountUsageWindow {
	var resetAt *time.Time
	if resetsAtMs != nil {
		t := time.UnixMilli(*resetsAtMs).UTC()
		resetAt = &t
	}
	used := math.Max(0, math.Min(100, float64(usedBp)/BPFull*100))
	return accountWindow(label, &used, resetAt, "")
}

// Large model used up line with the local reset date, pointing at other included usage when keep going is off
func windowTopTierLine(resetAt *time.Time, keepGoing *bool) string {
	resets := ""
	if resetAt != nil {
		local := resetAt.Local()
		resets = fmt.Sprintf(" Resets %s %d.", local.Format("Jan"), local.Day())
	}
	tail := " Past this, large models use your credits."
	if keepGoing != nil && !*keepGoing {
		tail = " Your other included usage is still available."
	}
	return accountUsageTopTierUsedUp + resets + tail
}

// One row per server allowance in server order plus top tier, paused and keep going lines
func usageWindowRows(gauges []WindowGauge, pausedReason string, keepGoing *bool) ([]AccountUsageWindow, []string) {
	if len(gauges) == 0 || len(AllScope(gauges)) == 0 {
		return nil, nil
	}
	var out []AccountUsageWindow
	var topRow *AccountUsageWindow
	topFull := false
	for _, g := range gauges {
		row := bpWindow(GaugeTitle(g), g.UsedBp, g.ResetsAtMs)
		out = append(out, row)
		if g.Scope == ScopeTopTier {
			r := row
			topRow, topFull = &r, g.Full()
		}
	}
	var lines []string
	if topFull {
		var resetAt *time.Time
		if topRow != nil {
			resetAt = topRow.ResetAt
		}
		lines = append(lines, windowTopTierLine(resetAt, keepGoing))
	}
	switch {
	case pausedReason != "" && PausedForPayment(pausedReason):
		lines = append(lines, accountUsagePausedLine)
	case pausedReason != "":
		lines = append(lines, accountUsagePausedOtherLine)
	case keepGoing != nil && !*keepGoing:
		lines = append(lines, accountUsageKeepGoingOffLine)
	case keepGoing != nil:
		lines = append(lines, accountUsageKeepGoingOnLine)
	}
	return out, lines
}

// BuildNousCreditsSnapshot turns portal account info into the usage snapshot, nil when logged out or empty
func BuildNousCreditsSnapshot(info *NousPortalAccountInfo) *AccountUsageSnapshot {
	if info == nil || !info.LoggedIn {
		return nil
	}
	access, sub := info.PaidServiceAccessInfo, info.Subscription
	plan := ""
	if sub != nil && sub.Plan != nil {
		plan = *sub.Plan
	}
	var windows []AccountUsageWindow
	var details, windowLines []string
	if uw := info.UsageWindow; uw != nil {
		windows, windowLines = usageWindowRows(uw.Gauges, uw.PausedReason, uw.KeepGoingOnCredits)
	}
	if len(windows) > 0 {
		details = append(details, windowLines...)
		if access != nil && finiteRef(access.PurchasedCreditsRemaining) {
			details = append(details, "Top-up credits: "+fmtUsageUSD(*access.PurchasedCreditsRemaining))
		}
		if info.PaidServiceAccess != nil && !*info.PaidServiceAccess && len(windowLines) == 0 {
			details = append(details, accountUsageDepletedLine)
		}
		if sub != nil && sub.CurrentPeriodEnd != nil && *sub.CurrentPeriodEnd != "" {
			renews, ok := FormatRenews(*sub.CurrentPeriodEnd)
			if !ok || renews == "" {
				renews = *sub.CurrentPeriodEnd
			}
			details = append(details, "Renews: "+renews)
		}
		return nousUsageSnapshot(windows, details, []string{"Top up via /topup"}, "portal-account", plan, true)
	}
	if sub != nil {
		capV, remaining := sub.MonthlyCredits, sub.CreditsRemaining
		if finiteRef(capV) && *capV > 0 && finiteRef(remaining) && *remaining <= *capV {
			used := math.Max(0, math.Min(100, (*capV-*remaining) / *capV * 100))
			windows = append(windows, accountWindow("Subscription", &used, nil,
				fmtUsageUSD(*remaining)+" of "+fmtUsageUSD(*capV)+" left"))
		}
	}
	if access != nil {
		for _, f := range []struct {
			v     *float64
			label string
		}{{access.SubscriptionCreditsRemaining, "Subscription credits"}, {access.PurchasedCreditsRemaining, "Top-up credits"},
			{access.TotalUsableCredits, "Total usable"}} {
			if finiteRef(f.v) {
				details = append(details, f.label+": "+fmtUsageUSD(*f.v))
			}
		}
	}
	if sub != nil {
		if finiteRef(sub.RolloverCredits) && *sub.RolloverCredits > 0 {
			details = append(details, "Rollover: "+fmtUsageUSD(*sub.RolloverCredits))
		}
		if sub.CurrentPeriodEnd != nil && *sub.CurrentPeriodEnd != "" {
			details = append(details, "Renews: "+*sub.CurrentPeriodEnd)
		}
	}
	if info.PaidServiceAccess != nil && !*info.PaidServiceAccess {
		details = append(details, accountUsageDepletedLine)
	}
	return nousUsageSnapshot(windows, details, []string{"Top up: " + NousPortalTopupURL(info), "(or run /topup)"},
		"portal-account", plan, false)
}

// FetchPortalAccount fetches the portal account fresh, releasing the caller at timeout while the worker runs on
func FetchPortalAccount(ctx context.Context, timeout time.Duration) (*NousPortalAccountInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	fetch := portalAccountFetchFn
	type result struct {
		info *NousPortalAccountInfo
		err  error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- result{err: fmt.Errorf("portal account fetch failed: %v", r)}
			}
		}()
		done <- result{info: fetch(ctx, true)}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.info, r.err
	case <-timer.C:
		return nil, errPortalAccountTimeout
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func headerGaugePercent(bp int, ok bool) *float64 {
	if !ok {
		return nil
	}
	v := math.Max(0, math.Min(100, float64(bp)/BPFull*100))
	return &v
}

// ApplyHeaderGauge raises window percents to the last response headers when those run ahead of the portal
func ApplyHeaderGauge(snapshot *AccountUsageSnapshot, state *CreditsState) *AccountUsageSnapshot {
	if snapshot == nil || !snapshot.UsageWindow || state == nil || !state.FromHeader || !state.IsUsageWindow() {
		return snapshot
	}
	type headerGauge struct {
		bp      int
		resetMs *int64
	}
	mapping := map[string]headerGauge{}
	for _, g := range state.Gauges {
		mapping[GaugeTitle(g)] = headerGauge{g.UsedBp, g.ResetsAtMs}
	}
	nowMs := accountUsageNow().UnixMilli()
	windows := make([]AccountUsageWindow, 0, len(snapshot.Windows))
	changed := false
	for _, w := range snapshot.Windows {
		hg, ok := mapping[w.Label]
		stale := ok && hg.resetMs != nil && nowMs >= *hg.resetMs
		header := headerGaugePercent(hg.bp, ok && !stale)
		if header == nil || w.UsedPercent == nil || *header <= *w.UsedPercent {
			windows = append(windows, w)
			continue
		}
		changed = true
		w.UsedPercent = header
		windows = append(windows, w)
	}
	if !changed {
		return snapshot
	}
	details := append([]string(nil), snapshot.Details...)
	if top := state.TopTierGauge(); top != nil {
		title := GaugeTitle(*top)
		for _, w := range windows {
			if w.Label != title {
				continue
			}
			if w.UsedPercent != nil && *w.UsedPercent >= 100 {
				already := false
				for _, line := range details {
					already = already || strings.HasPrefix(line, accountUsageTopTierUsedUp)
				}
				if !already {
					details = append(details, windowTopTierLine(w.ResetAt, state.KeepGoingOnCredits))
				}
			}
			break
		}
	}
	out := *snapshot
	out.Windows, out.Details = windows, details
	return &out
}

// NousCreditsLines is the rendered Nous credits usage block, empty when there is nothing to show
func NousCreditsLines(ctx context.Context, markdown bool, timeout time.Duration, state *CreditsState) []string {
	lines, _ := NousCreditsView(ctx, markdown, timeout, state)
	return lines
}

// NousCreditsView is the credits block plus whether it uses the usage window layout, failing open to nothing
func NousCreditsView(ctx context.Context, markdown bool, timeout time.Duration, state *CreditsState) (lines []string, isWindow bool) {
	if fixture := safeDevFixture(); fixture != nil {
		snapshot := snapshotFromCreditsState(fixture)
		return RenderAccountUsageLines(snapshot, markdown), snapshot != nil && snapshot.UsageWindow
	}
	if !accountUsageLoggedInFn(ctx) {
		return []string{}, false
	}
	defer func() {
		if r := recover(); r != nil {
			accountUsageLog().Debug("credits ▸ /usage portal fetch/render failed (fail-open)", "panic", r)
			lines, isWindow = []string{}, false
		}
	}()
	info, err := FetchPortalAccount(ctx, timeout)
	if err != nil {
		accountUsageLog().Debug("credits ▸ /usage portal fetch/render failed (fail-open)", "err", err)
		return []string{}, false
	}
	snapshot := ApplyHeaderGauge(BuildNousCreditsSnapshot(info), state)
	return RenderAccountUsageLines(snapshot, markdown), snapshot != nil && snapshot.UsageWindow
}

func safeDevFixture() (state *CreditsState) {
	defer func() {
		if r := recover(); r != nil {
			accountUsageLog().Debug("Dev credits fixture unavailable", "panic", r)
			state = nil
		}
	}()
	return accountUsageFixtureFn()
}

func accountUsageLog() *slog.Logger { return modelsLog }

// NousCreditsUsageFields is the session usage credits_lines and credits_usage_window fields from live header state
func NousCreditsUsageFields(ctx context.Context, state *CreditsState) map[string]any {
	lines, isWindow := NousCreditsView(ctx, false, 10*time.Second, state)
	if len(lines) == 0 {
		return map[string]any{}
	}
	out := map[string]any{"credits_lines": lines}
	if isWindow {
		out["credits_usage_window"] = true
	}
	return out
}

// Header shaped dev fixture state as a usage snapshot in the same shape as the portal path
func snapshotFromCreditsState(state *CreditsState) *AccountUsageSnapshot {
	if state == nil {
		return nil
	}
	var windows []AccountUsageWindow
	var details []string
	if state.IsUsageWindow() {
		var windowLines []string
		windows, windowLines = usageWindowRows(state.Gauges, state.PausedReason, state.KeepGoingOnCredits)
		details = append(details, windowLines...)
		if state.PurchasedUSD != "" {
			details = append(details, "Top-up credits: $"+state.PurchasedUSD)
		}
		if !state.PaidAccess && len(windowLines) == 0 {
			details = append(details, accountUsageDepletedLine)
		}
		return nousUsageSnapshot(windows, details, []string{accountUsageDevFixtureTail}, "dev-fixture", "", true)
	}
	capUSD := ""
	if state.SubscriptionLimitUSD != nil {
		capUSD = *state.SubscriptionLimitUSD
	}
	if uf, ok := state.UsedFraction(); ok && !math.IsNaN(uf) && !math.IsInf(uf, 0) {
		used := math.Max(0, math.Min(100, uf*100))
		detail := ""
		if state.SubscriptionUSD != "" && capUSD != "" {
			detail = "$" + state.SubscriptionUSD + " of $" + capUSD + " left"
		}
		windows = append(windows, accountWindow("Subscription", &used, nil, detail))
	}
	for _, f := range [][2]string{{state.SubscriptionUSD, "Subscription credits"}, {state.PurchasedUSD, "Top-up credits"},
		{state.RemainingUSD, "Total usable"}} {
		if f[0] != "" {
			details = append(details, f[1]+": $"+f[0])
		}
	}
	if !state.PaidAccess {
		details = append(details, accountUsageDepletedLine)
	}
	return nousUsageSnapshot(windows, details, []string{accountUsageDevFixtureTail}, "dev-fixture", "", false)
}

// CreditsView is the topup balance view shared by every money surface
type CreditsView struct {
	LoggedIn     bool
	BalanceLines []string
	IdentityLine string
	TopupURL     string
	Depleted     bool
}

// BuildCreditsView builds the topup balance block, identity line and top up URL from one portal fetch
func BuildCreditsView(ctx context.Context, markdown bool, timeout time.Duration) CreditsView {
	if !accountUsageLoggedInFn(ctx) {
		return CreditsView{}
	}
	account, err := FetchPortalAccount(ctx, timeout)
	if err != nil {
		accountUsageLog().Debug("credits ▸ /topup portal fetch failed (fail-open)", "err", err)
		return CreditsView{}
	}
	if account == nil || !account.LoggedIn {
		return CreditsView{}
	}
	balance := []string{}
	for _, line := range RenderAccountUsageLines(BuildNousCreditsSnapshot(account), markdown) {
		trimmed := strings.TrimLeft(line, " \t\n\r\v\f")
		if strings.HasPrefix(trimmed, "Top up:") || strings.HasPrefix(trimmed, "Top up via") || strings.HasPrefix(trimmed, "(or run") {
			continue
		}
		balance = append(balance, line)
	}
	var who []string
	if account.Email != "" {
		who = append(who, account.Email)
	}
	if account.OrgName != "" {
		who = append(who, "org "+account.OrgName)
	}
	identity := ""
	if len(who) > 0 {
		identity = "Topping up as " + strings.Join(who, " / ")
	}
	return CreditsView{LoggedIn: true, BalanceLines: balance, IdentityLine: identity, TopupURL: NousPortalTopupURL(account),
		Depleted: account.PaidServiceAccess != nil && !*account.PaidServiceAccess}
}

// Codex usage, reset credit list and consume endpoints by the Codex CLI path style split
func codexBackendURLs(baseURL string) (string, string, string) {
	normalized := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if normalized == "" {
		normalized = defaultCodexUsageBackendBaseURL
	}
	normalized = strings.TrimSuffix(normalized, "/codex")
	prefix := normalized + "/api/codex"
	if strings.Contains(normalized, "/backend-api") {
		prefix = normalized + "/wham"
	}
	return prefix + "/usage", prefix + "/rate-limit-reset-credits", prefix + "/rate-limit-reset-credits/consume"
}

type codexUsageCreds struct {
	token, baseURL, accountID string
}

func isAuthErr(err error) bool {
	var ae *AuthError
	return errors.As(err, &ae)
}

// Codex quota credentials from explicit live agent creds, then the native resolver, then a direct pool select
func resolveCodexUsageCredentials(ctx context.Context, baseURL, apiKey string, forceRefresh bool) (codexUsageCreds, error) {
	env := accountUsageEnvFn(ctx)
	explicit := strings.TrimSpace(apiKey)
	if explicit != "" && !forceRefresh {
		return codexUsageCreds{explicit, strings.TrimSpace(baseURL), ""}, nil
	}
	if explicit != "" {
		singleton := ""
		tokens, err := codexUsageReadTokensFn(env)
		if err != nil && !isAuthErr(err) {
			return codexUsageCreds{}, err
		}
		if err == nil {
			singleton = strings.TrimSpace(auStr(tokens["access_token"]))
		}
		if singleton != explicit {
			var entry PooledCredential
			ok := false
			if pool := codexUsageLoadPoolFn(env); pool != nil {
				entry, ok = pool.TryRefreshMatching(explicit, "")
			}
			if !ok {
				return codexUsageCreds{}, errors.New("Could not refresh the Codex credential this session runs on")
			}
			return codexUsageCreds{entry.RuntimeAPIKey(), codexPoolRouteBaseURL(env, usageFirstNonEmpty(entry.RuntimeBaseURL(), baseURL)), ""}, nil
		}
	}
	creds, err := codexUsageResolveFn(env, forceRefresh)
	if err == nil {
		accountID := ""
		tokens, terr := codexUsageReadTokensFn(env)
		switch {
		case terr == nil:
			accountID = strings.TrimSpace(auStr(tokens["account_id"]))
		case isAuthErr(terr):
			accountUsageLog().Debug("codex ▸ /usage account_id read failed (best-effort)", "err", terr)
		default:
			return codexUsageCreds{}, terr
		}
		key, ok := creds["api_key"].(string)
		if !ok {
			return codexUsageCreds{}, errors.New("'api_key'")
		}
		return codexUsageCreds{key, strings.TrimSpace(auStr(creds["base_url"])), accountID}, nil
	}
	if !isAuthErr(err) {
		return codexUsageCreds{}, err
	}
	accountUsageLog().Debug("codex ▸ /usage runtime resolver returned no creds; trying pool", "err", err)
	var entry PooledCredential
	ok := false
	if pool := codexUsageLoadPoolFn(env); pool != nil {
		entry, ok = pool.Select("")
	}
	if !ok {
		return codexUsageCreds{}, errors.New("No available openai-codex credential in credential pool")
	}
	return codexUsageCreds{entry.RuntimeAPIKey(), codexPoolRouteBaseURL(env, usageFirstNonEmpty(entry.RuntimeBaseURL(), baseURL)), ""}, nil
}

func usageFirstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func codexBankedResets(payload map[string]any) (int64, error) {
	credits, err := auDictOr(payload["rate_limit_reset_credits"])
	if err != nil {
		return 0, err
	}
	raw := credits["available_count"]
	if !auIsNum(raw) {
		return 0, nil
	}
	return auInt(raw)
}

// Codex request headers, the auth.json account id winning over the JWT claim
func codexUsageHeaders(token, accountID string) map[string]string {
	h := map[string]string{"Authorization": "Bearer " + token, "Accept": "application/json", "User-Agent": "codex-cli"}
	for k, v := range codexAccountHeaders(token) {
		h[k] = v
	}
	if accountID != "" {
		h["ChatGPT-Account-ID"] = accountID
	}
	return h
}

// accountUsageHTTPError is a non success HTTP status as httpx raise_for_status reports it
type accountUsageHTTPError struct {
	Status int
	URL    string
}

func (e *accountUsageHTTPError) Error() string {
	return fmt.Sprintf("HTTP %d for url '%s'", e.Status, e.URL)
}

func httpStatusOf(err error) int {
	var he *accountUsageHTTPError
	if errors.As(err, &he) {
		return he.Status
	}
	return 0
}

// One request decoded as Python json, non success statuses raising, falsy bodies reading as an empty dict
func accountUsageDo(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body []byte) (map[string]any, string, error) {
	var rdr io.Reader
	if body != nil {
		rdr = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", &accountUsageHTTPError{Status: resp.StatusCode, URL: url}
	}
	v, repr, err := pyJSONLoads(raw)
	if err != nil {
		return nil, "", err
	}
	if !auTruthy(v) {
		return map[string]any{}, "{}", nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, "", fmt.Errorf("'%s' object has no attribute 'get'", auTypeName(v))
	}
	return m, repr, nil
}

func accountUsageGetJSON(ctx context.Context, url string, headers map[string]string, timeout time.Duration) (map[string]any, error) {
	m, _, err := accountUsageDo(ctx, accountUsageClientFn(timeout), http.MethodGet, url, headers, nil)
	return m, err
}

// Windows from source key used fields, fraction scaling values at most one, model scoped keys capping one family
func usageWindowsFrom(source map[string]any, mapping [][2]string, usedKey, resetKey string, fraction bool, modelScoped map[string]bool) ([]AccountUsageWindow, error) {
	var windows []AccountUsageWindow
	for _, m := range mapping {
		window, err := auDictOr(source[m[0]])
		if err != nil {
			return nil, err
		}
		raw, present := window[usedKey]
		if !present || raw == nil {
			continue
		}
		used, err := auFloat(raw)
		if err != nil {
			return nil, err
		}
		if fraction && used <= 1 {
			used *= 100
		}
		resetAt, err := parseUsageDT(window[resetKey])
		if err != nil {
			return nil, err
		}
		w := accountWindow(m[1], &used, resetAt, "")
		if modelScoped[m[0]] {
			w.Scope = "model"
		}
		windows = append(windows, w)
	}
	return windows, nil
}

var codexWindowLabelsBySeconds = map[int64]string{18000: "Session", 604800: "Weekly"}

// Codex window labels by published duration, positional labels when the duration is missing or unknown
func codexWindowLabels(rateLimit map[string]any) ([][2]string, error) {
	var labels [][2]string
	for _, pos := range [][2]string{{"primary_window", "Session"}, {"secondary_window", "Weekly"}} {
		label := pos[1]
		if window, ok := rateLimit[pos[0]].(map[string]any); ok && auIsNum(window["limit_window_seconds"]) {
			seconds, err := auInt(window["limit_window_seconds"])
			if err != nil {
				return nil, err
			}
			if l, ok := codexWindowLabelsBySeconds[seconds]; ok {
				label = l
			}
		}
		labels = append(labels, [2]string{pos[0], label})
	}
	return labels, nil
}

func usagePlural(count int64) string {
	if count != 1 {
		return "s"
	}
	return ""
}

func fetchCodexAccountUsage(ctx context.Context, baseURL, apiKey string) (*AccountUsageSnapshot, error) {
	return fetchCodexAccountUsageImpl(ctx, baseURL, apiKey, false)
}

// Picker variant where a 401 is reported and never repaired by refreshing or rotating a credential
func fetchCodexAccountUsageReadOnly(ctx context.Context, baseURL, apiKey string) (*AccountUsageSnapshot, error) {
	return fetchCodexAccountUsageImpl(ctx, baseURL, apiKey, true)
}

func fetchCodexAccountUsageImpl(ctx context.Context, baseURL, apiKey string, readOnly bool) (*AccountUsageSnapshot, error) {
	creds, err := resolveCodexUsageCredentials(ctx, baseURL, apiKey, false)
	if err != nil {
		return nil, err
	}
	usageURL, _, _ := codexBackendURLs(creds.baseURL)
	payload, err := accountUsageGetJSON(ctx, usageURL, codexUsageHeaders(creds.token, creds.accountID), codexUsageRequestTimeout)
	if err != nil {
		if httpStatusOf(err) != 401 || readOnly {
			return nil, err
		}
		if creds, err = resolveCodexUsageCredentials(ctx, baseURL, apiKey, true); err != nil {
			return nil, err
		}
		usageURL, _, _ = codexBackendURLs(creds.baseURL)
		if payload, err = accountUsageGetJSON(ctx, usageURL, codexUsageHeaders(creds.token, creds.accountID), codexUsageRequestTimeout); err != nil {
			return nil, err
		}
	}
	rateLimit, err := auDictOr(payload["rate_limit"])
	if err != nil {
		return nil, err
	}
	labels, err := codexWindowLabels(rateLimit)
	if err != nil {
		return nil, err
	}
	windows, err := usageWindowsFrom(rateLimit, labels, "used_percent", "reset_at", false, nil)
	if err != nil {
		return nil, err
	}
	var details []string
	count, err := codexBankedResets(payload)
	if err != nil {
		return nil, err
	}
	if count > 0 {
		details = append(details, fmt.Sprintf("You have %d reset%s banked - use /usage reset to activate", count, usagePlural(count)))
	}
	credits, err := auDictOr(payload["credits"])
	if err != nil {
		return nil, err
	}
	if auTruthy(credits["has_credits"]) && auIsNum(credits["balance"]) {
		bal, err := auFloat(credits["balance"])
		if err != nil {
			return nil, err
		}
		details = append(details, "Credits balance: $"+pyFixed2(bal))
	} else if auTruthy(credits["has_credits"]) && auTruthy(credits["unlimited"]) {
		details = append(details, "Credits balance: unlimited")
	}
	s := NewAccountUsageSnapshot("openai-codex", "usage_api", windows, details)
	s.Plan, s.Identity, s.Raw = titleCaseSlug(payload["plan_type"]), codexSnapshotIdentity(creds.token), payload
	return s, nil
}

// Decoded JWT principal of a Codex credential, never the token, empty when it cannot be decoded
func codexSnapshotIdentity(token string) string {
	if account, sub, ok := codexPrincipalIdentity(token); ok {
		return "codex:" + account + ":" + sub
	}
	return ""
}

// CodexResetRedeemResult is the outcome of a usage reset attempt against the Codex backend
type CodexResetRedeemResult struct {
	Status         string
	Message        string
	AvailableCount int64
	WindowsReset   int64
}

// Redeemed reports a reset that went through
func (r CodexResetRedeemResult) Redeemed() bool { return r.Status == "reset" }

func codexResetUnavailable(message string) CodexResetRedeemResult {
	return CodexResetRedeemResult{Status: "unavailable", Message: message}
}

// Refusal for a redemption that would be wasted, nil when it may proceed
func codexResetGuard(payload map[string]any, available int64, force bool) (*CodexResetRedeemResult, error) {
	if available <= 0 {
		return &CodexResetRedeemResult{Status: "no_credits_banked", Message: "No banked reset credits on this account — nothing to redeem."}, nil
	}
	rateLimit, err := auDictOr(payload["rate_limit"])
	if err != nil {
		return nil, err
	}
	var worst *float64
	for _, k := range []string{"primary_window", "secondary_window"} {
		window, err := auDictOr(rateLimit[k])
		if err != nil {
			return nil, err
		}
		if u := window["used_percent"]; auIsNum(u) {
			f, err := auFloat(u)
			if err != nil {
				return nil, err
			}
			if worst == nil {
				zero := 0.0
				worst = &zero
			}
			if f > *worst {
				*worst = f
			}
		}
	}
	if force || (worst != nil && *worst >= codexWindowExhaustedPercent) {
		return nil, nil
	}
	note := "your current usage could not be confirmed as exhausted"
	if worst != nil {
		note = fmt.Sprintf("your busiest window is only %s%% used", pyFixed0(*worst))
	}
	return &CodexResetRedeemResult{Status: "not_exhausted", AvailableCount: available, Message: fmt.Sprintf(
		"⚠️ Not redeeming: %s. A banked reset restores your FULL 5h + weekly limits, so spending it now would waste most of it. "+
			"You have %d reset%s banked. Use `/usage reset --force` to redeem anyway.", note, available, usagePlural(available))}, nil
}

// Consume response code mapped to a result, a reset also lifting persisted pool cooldowns
func codexResetOutcome(ctx context.Context, body map[string]any, bodyRepr string, available int64) (CodexResetRedeemResult, error) {
	code := strings.ToLower(strings.TrimSpace(auStr(body["code"])))
	remaining := max(0, available-1)
	outcomes := map[string]struct {
		msg   string
		count int64
	}{
		"reset": {fmt.Sprintf("✅ Reset redeemed — your usage limits have been reset. %d banked reset%s remaining.", remaining,
			usagePlural(remaining)), remaining},
		"nothing_to_reset": {"Backend reports nothing to reset — your limits aren't exhausted. The credit was NOT spent.", available},
		"no_credit":        {"Backend reports no available reset credit on this account.", 0},
		"already_redeemed": {"This redemption was already processed — no additional credit was spent.", remaining},
	}
	out, ok := outcomes[code]
	if !ok {
		return codexResetUnavailable("Unexpected response from the Codex backend: " + bodyRepr), nil
	}
	var windowsReset int64
	if code == "reset" {
		func() {
			defer func() {
				if r := recover(); r != nil {
					accountUsageLog().Debug("Failed to clear Codex pool cooldowns after reset redemption", "panic", r)
				}
			}()
			codexUsageClearCooldownsFn(accountUsageEnvFn(ctx))
		}()
		if raw := body["windows_reset"]; auIsNum(raw) {
			n, err := auInt(raw)
			if err != nil {
				return CodexResetRedeemResult{}, err
			}
			windowsReset = n
		}
	}
	return CodexResetRedeemResult{Status: code, Message: out.msg, AvailableCount: out.count, WindowsReset: windowsReset}, nil
}

// RedeemCodexResetCredit redeems one banked Codex rate limit reset credit, every failure returned as a result
func RedeemCodexResetCredit(ctx context.Context, baseURL, apiKey string, force bool) CodexResetRedeemResult {
	if ctx == nil {
		ctx = context.Background()
	}
	creds, err := resolveCodexUsageCredentials(ctx, baseURL, apiKey, false)
	if err != nil {
		return codexResetUnavailable("No Codex credentials available. Run `icebath auth` to sign in with your ChatGPT account.")
	}
	redeemID := uuid.NewString()
	var body map[string]any
	var bodyRepr string
	var available int64
	for attempt := 0; attempt < 2; attempt++ {
		usageURL, _, consumeURL := codexBackendURLs(creds.baseURL)
		headers := codexUsageHeaders(creds.token, creds.accountID)
		client := accountUsageClientFn(codexUsageRequestTimeout)
		var refused *CodexResetRedeemResult
		err = func() error {
			payload, _, err := accountUsageDo(ctx, client, http.MethodGet, usageURL, headers, nil)
			if err != nil {
				return err
			}
			if available, err = codexBankedResets(payload); err != nil {
				return err
			}
			if refused, err = codexResetGuard(payload, available, force); err != nil || refused != nil {
				return err
			}
			consumeHeaders := map[string]string{"Content-Type": "application/json"}
			for k, v := range headers {
				consumeHeaders[k] = v
			}
			reqBody, _ := json.Marshal(map[string]string{"redeem_request_id": redeemID})
			body, bodyRepr, err = accountUsageDo(ctx, client, http.MethodPost, consumeURL, consumeHeaders, reqBody)
			return err
		}()
		if refused != nil {
			return *refused
		}
		if err == nil {
			break
		}
		if httpStatusOf(err) != 401 || attempt > 0 {
			break
		}
		fresh, rerr := resolveCodexUsageCredentials(ctx, baseURL, apiKey, true)
		if rerr != nil {
			break
		}
		creds = fresh
	}
	if err != nil {
		if code := httpStatusOf(err); code != 0 {
			if code == 401 || code == 403 {
				return codexResetUnavailable(fmt.Sprintf("Codex backend rejected the request (HTTP %d). Reset credits require ChatGPT-account "+
					"(OAuth) auth — run `icebath auth` and sign in with your ChatGPT account.", code))
			}
			return codexResetUnavailable(fmt.Sprintf("Codex backend error (HTTP %d) — try again shortly.", code))
		}
		return codexResetUnavailable("Could not reach the Codex backend: " + err.Error())
	}
	res, err := codexResetOutcome(ctx, body, bodyRepr, available)
	if err != nil {
		return codexResetUnavailable("Could not reach the Codex backend: " + err.Error())
	}
	return res
}

// Anthropic OAuth account windows for the explicit credential or the ambient OAuth token
func fetchAnthropicAccountUsage(ctx context.Context, baseURL, apiKey string) (*AccountUsageSnapshot, error) {
	token := strings.TrimSpace(apiKey)
	if token == "" {
		token = strings.TrimSpace(anthropicUsageTokenFn(accountUsageEnvFn(ctx)))
	}
	if token == "" {
		return nil, nil
	}
	if !isOAuthToken(token) {
		s := NewAccountUsageSnapshot("anthropic", "oauth_usage_api", nil, nil)
		s.UnavailableReason = "Anthropic account limits are only available for OAuth-backed Claude accounts."
		return s, nil
	}
	headers := map[string]string{"Authorization": "Bearer " + token, "Accept": "application/json", "Content-Type": "application/json",
		"anthropic-beta": "oauth-2025-04-20", "User-Agent": "claude-code/2.1.0"}
	payload, err := accountUsageGetJSON(ctx, anthropicOAuthUsageURL, headers, codexUsageRequestTimeout)
	if err != nil {
		return nil, err
	}
	windows, err := usageWindowsFrom(payload, [][2]string{{"five_hour", "Current session"}, {"seven_day", "Current week"},
		{"seven_day_opus", "Opus week"}, {"seven_day_sonnet", "Sonnet week"}}, "utilization", "resets_at", true,
		map[string]bool{"seven_day_opus": true, "seven_day_sonnet": true})
	if err != nil {
		return nil, err
	}
	var details []string
	extra, err := auDictOr(payload["extra_usage"])
	if err != nil {
		return nil, err
	}
	used, limit := extra["used_credits"], extra["monthly_limit"]
	if auTruthy(extra["is_enabled"]) && auIsNum(used) && auIsNum(limit) {
		u, err := auFloat(used)
		if err != nil {
			return nil, err
		}
		l, err := auFloat(limit)
		if err != nil {
			return nil, err
		}
		currency := "USD"
		if c := extra["currency"]; auTruthy(c) {
			currency = auStr(c)
		}
		details = append(details, fmt.Sprintf("Extra usage: %s / %s %s", pyFixed2(u), pyFixed2(l), currency))
	}
	return NewAccountUsageSnapshot("anthropic", "oauth_usage_api", windows, details), nil
}

// OpenRouter credit balance plus the API key quota window and usage totals
func fetchOpenRouterAccountUsage(ctx context.Context, baseURL, apiKey string) (*AccountUsageSnapshot, error) {
	runtime, err := openrouterUsageRuntimeFn(ctx, baseURL, apiKey)
	if err != nil {
		return nil, err
	}
	token := ""
	normalized := ""
	if runtime != nil {
		token, normalized = strings.TrimSpace(runtime.APIKey), strings.TrimRight(runtime.BaseURL, "/")
	}
	if token == "" {
		return nil, nil
	}
	headers := map[string]string{"Authorization": "Bearer " + token, "Accept": "application/json"}
	client := accountUsageClientFn(openrouterUsageRequestTimeout)
	data := func(path string) (any, error) {
		body, _, err := accountUsageDo(ctx, client, http.MethodGet, normalized+"/"+path, headers, nil)
		if err != nil {
			return nil, err
		}
		if v := body["data"]; auTruthy(v) {
			return v, nil
		}
		return map[string]any{}, nil
	}
	rawCredits, err := data("credits")
	if err != nil {
		return nil, err
	}
	rawKey, err := data("key")
	if err != nil {
		rawKey = map[string]any{}
	}
	credits, err := auDictOr(rawCredits)
	if err != nil {
		return nil, err
	}
	keyData, err := auDictOr(rawKey)
	if err != nil {
		return nil, err
	}
	total, err := auFloatOrZero(credits["total_credits"])
	if err != nil {
		return nil, err
	}
	usage, err := auFloatOrZero(credits["total_usage"])
	if err != nil {
		return nil, err
	}
	details := []string{"Credits balance: $" + pyFixed2(pyMaxZero(total-usage))}
	var windows []AccountUsageWindow
	limit, limitRemaining, keyUsage := keyData["limit"], keyData["limit_remaining"], keyData["usage"]
	limitReset := strings.TrimSpace(auStr(keyData["limit_reset"]))
	if auIsNum(limit) && auIsNum(limitRemaining) {
		l, lerr := auFloat(limit)
		r, rerr := auFloat(limitRemaining)
		if lerr != nil {
			return nil, lerr
		}
		if l > 0 && rerr != nil {
			return nil, rerr
		}
		if l > 0 && 0 <= r && r <= l {
			parts := []string{fmt.Sprintf("$%s of $%s remaining", pyFixed2(r), pyFixed2(l))}
			if limitReset != "" {
				parts = append(parts, "resets "+limitReset)
			}
			used := ((l - r) / l) * 100
			windows = append(windows, accountWindow("API key quota", &used, nil, strings.Join(parts, " • ")))
		}
	}
	if auIsNum(keyUsage) {
		u, err := auFloat(keyUsage)
		if err != nil {
			return nil, err
		}
		parts := []string{"API key usage: $" + pyFixed2(u) + " total"}
		for _, kv := range [][2]string{{"usage_daily", "today"}, {"usage_weekly", "this week"}, {"usage_monthly", "this month"}} {
			if v := keyData[kv[0]]; auIsNum(v) {
				f, err := auFloat(v)
				if err != nil {
					return nil, err
				}
				if f > 0 {
					parts = append(parts, "$"+pyFixed2(f)+" "+kv[1])
				}
			}
		}
		details = append(details, strings.Join(parts, " • "))
	}
	return NewAccountUsageSnapshot("openrouter", "credits_api", windows, details), nil
}

// accountUsageFetcher reads one provider account usage snapshot
type accountUsageFetcher func(ctx context.Context, baseURL, apiKey string) (*AccountUsageSnapshot, error)

// Built in usage fetchers by provider, replaced in tests
var accountUsageFetchers = map[string]accountUsageFetcher{
	"openai-codex": fetchCodexAccountUsage, "anthropic": fetchAnthropicAccountUsage, "openrouter": fetchOpenRouterAccountUsage,
}

// Per credential picker variants that never rotate or refresh a credential on failure
var readOnlyAccountUsageFetchers = map[string]accountUsageFetcher{"openai-codex": fetchCodexAccountUsageReadOnly}

// Profile hook under the shared deadline, nil past it, its error or panic returned to the caller
func callPluginUsageHook(ctx context.Context, profile Profile, baseURL, apiKey string) (*AccountUsageSnapshot, error) {
	if profile.FetchAccountUsageHook == nil {
		return nil, nil
	}
	type result struct {
		snap *AccountUsageSnapshot
		err  error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- result{err: fmt.Errorf("plugin account usage hook panicked: %v", r)}
			}
		}()
		snap, err := profile.FetchAccountUsage(ctx, baseURL, apiKey)
		done <- result{snap, err}
	}()
	timer := time.NewTimer(PluginUsageHookDeadline)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.snap, r.err
	case <-timer.C:
		return nil, nil
	}
}

// AccountUsageFetchOptions selects the credential a usage fetch reads and the cache slot it lands in
type AccountUsageFetchOptions struct {
	BaseURL    string
	APIKey     string
	ReadOnly   bool
	IdentityID string
}

// FetchAccountUsage fetches a provider account usage snapshot and remembers it, nil on any failure
func FetchAccountUsage(ctx context.Context, provider string, opts AccountUsageFetchOptions) *AccountUsageSnapshot {
	if ctx == nil {
		ctx = context.Background()
	}
	slug := strings.ToLower(strings.TrimSpace(provider))
	var fetcher accountUsageFetcher
	if opts.ReadOnly {
		fetcher = readOnlyAccountUsageFetchers[slug]
	}
	if fetcher == nil {
		fetcher = accountUsageFetchers[slug]
	}
	started := accountUsageMonotonic()
	snapshot, ok := func() (snap *AccountUsageSnapshot, ok bool) {
		defer func() {
			if r := recover(); r != nil {
				snap, ok = nil, false
			}
		}()
		var err error
		if fetcher != nil {
			snap, err = fetcher(ctx, opts.BaseURL, opts.APIKey)
		} else if profile, found := accountUsageProfileFn(ctx, slug); found {
			snap, err = callPluginUsageHook(ctx, profile, opts.BaseURL, opts.APIKey)
		}
		return snap, err == nil
	}()
	if !ok {
		return nil
	}
	RememberAccountUsage(ctx, provider, snapshot, opts.IdentityID, opts.BaseURL, started)
	return snapshot
}

// Python truthiness of a decoded JSON value, zero json numbers falsy
func auTruthy(v any) bool {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return err != nil || f != 0
	case map[string]any:
		return len(t) > 0
	}
	return pyTruthy(v)
}

func auTypeName(v any) string {
	switch t := v.(type) {
	case json.Number:
		if strings.ContainsAny(string(t), ".eE") {
			return "float"
		}
		return "int"
	case map[string]any:
		return "dict"
	case int, int64:
		return "int"
	}
	return pyTypeName(v)
}

// Python str of a value treated as empty when falsy
func auStr(v any) string {
	if !auTruthy(v) {
		return ""
	}
	switch t := v.(type) {
	case bool:
		return "True"
	case float64:
		return pyFloatRepr(t)
	case map[string]any, []any:
		return fmt.Sprint(t)
	}
	return pyStr(v)
}

// Dict of a falsy or dict value, an error for any other truthy shape as Python attribute access raises
func auDictOr(v any) (map[string]any, error) {
	if !auTruthy(v) {
		return map[string]any{}, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", auTypeName(v))
	}
	return m, nil
}

// Python isinstance int or float of a decoded value, bools included
func auIsNum(v any) bool {
	switch v.(type) {
	case bool, json.Number, float64, float32, int, int64:
		return true
	}
	return false
}

// Python float of a decoded value, strings parsed as float does
func auFloat(v any) (float64, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case float64:
		return t, nil
	case float32:
		return float64(t), nil
	case int:
		return float64(t), nil
	case int64:
		return float64(t), nil
	case json.Number:
		f, err := strconv.ParseFloat(string(t), 64)
		if err != nil && !strings.ContainsAny(string(t), ".eE") {
			return 0, errors.New("int too large to convert to float")
		}
		return f, nil
	case string:
		return pyFloatString(t)
	}
	return 0, fmt.Errorf("float() argument must be a string or a real number, not '%s'", auTypeName(v))
}

// Python float of a string, whitespace, single underscores between digits, Unicode digits and inf or nan words
func pyFloatString(s string) (float64, error) {
	text := strings.TrimSpace(s)
	bad := fmt.Errorf("could not convert string to float: %s", pyStringRepr(s))
	var b strings.Builder
	runes := []rune(text)
	for i, r := range runes {
		switch {
		case r == '_':
			if i == 0 || i == len(runes)-1 || !isDigitRune(runes[i-1]) || !isDigitRune(runes[i+1]) {
				return 0, bad
			}
		case r > 0x7f && isDigitRune(r):
			b.WriteByte(byte('0' + unicodeDigitValue(r)))
		case r == 'x' || r == 'X' || r == 'p' || r == 'P':
			return 0, bad
		default:
			b.WriteRune(r)
		}
	}
	f, err := strconv.ParseFloat(b.String(), 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, bad
	}
	return f, nil
}

func isDigitRune(r rune) bool {
	return (r >= '0' && r <= '9') || (r > 0x7f && unicode.Is(unicode.Nd, r))
}

// Python float of a value or zero for a falsy value
func auFloatOrZero(v any) (float64, error) {
	if !auTruthy(v) {
		return 0, nil
	}
	return auFloat(v)
}

// Python int of a numeric value, floats truncated, infinities and nan rejected
func auInt(v any) (int64, error) {
	if n, ok := v.(json.Number); ok && !strings.ContainsAny(string(n), ".eE") {
		i, err := strconv.ParseInt(string(n), 10, 64)
		if err != nil {
			if strings.HasPrefix(string(n), "-") {
				return math.MinInt64, nil
			}
			return math.MaxInt64, nil
		}
		return i, nil
	}
	f, err := auFloat(v)
	if err != nil {
		return 0, err
	}
	if math.IsInf(f, 0) {
		return 0, errors.New("cannot convert float infinity to integer")
	}
	if math.IsNaN(f) {
		return 0, errors.New("cannot convert float NaN to integer")
	}
	return int64(math.Trunc(f)), nil
}

// Python max of zero and a value, zero when the value is nan
func pyMaxZero(v float64) float64 {
	if v > 0 {
		return v
	}
	return 0
}

// Fixed two decimals as Python formats them, inf and nan spelled lowercase
func pyFixed2(v float64) string { return pyFixedN(v, 2) }

func pyFixed0(v float64) string { return pyFixedN(v, 0) }

func pyFixedN(v float64, n int) string {
	switch {
	case math.IsInf(v, 1):
		return "inf"
	case math.IsInf(v, -1):
		return "-inf"
	case math.IsNaN(v):
		return "nan"
	}
	return strconv.FormatFloat(v, 'f', n, 64)
}
