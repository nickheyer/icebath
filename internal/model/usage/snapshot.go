package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/providers"
)

const (
	defaultTitle        = "Account limits"
	scopeAccount        = "account"
	scopeModel          = "model"
	usageFetchTimeout   = 15 * time.Second
	openRouterTimeout   = 10 * time.Second
	pluginHookDeadline  = 10 * time.Second
	anthropicUsageURL   = "https://api.anthropic.com/api/oauth/usage"
	codexEnvBaseURL     = "ICEBATH_CODEX_BASE_URL"
	codexExhaustedPct   = 100.0
	codexResetReset     = "reset"
	codexResetNothing   = "nothing_to_reset"
	codexResetNoCredit  = "no_credit"
	codexResetAlready   = "already_redeemed"
	codexResetNotFull   = "not_exhausted"
	codexResetNoBanked  = "no_credits_banked"
	codexResetUnusable  = "unavailable"
	maxValidUnixSeconds = 253402300799
)

var (
	errCodexSessionRefresh = errors.New("usage: could not refresh the Codex credential this session runs on")
	errCodexPoolEmpty      = errors.New("usage: no available openai-codex credential in credential pool")
)

type UsageQuery struct {
	Provider   string
	BaseURL    string
	APIKey     auth.Secret
	ReadOnly   bool
	IdentityID string
}

func newSnapshot(provider, source string, windows []providers.UsageWindow, details []string) *providers.AccountUsage {
	return &providers.AccountUsage{Provider: provider, Source: source, FetchedAt: time.Now().UTC(), Title: defaultTitle, Windows: windows, Details: details}
}

func (t *Tracker) FetchAccountUsage(ctx context.Context, q UsageQuery) (*providers.AccountUsage, error) {
	ctx = t.bind(ctx)
	slug := strings.ToLower(strings.TrimSpace(q.Provider))
	started := time.Now()
	var snap *providers.AccountUsage
	var err error
	switch slug {
	case auth.ProviderCodex:
		snap, err = t.codexUsage(ctx, q.BaseURL, q.APIKey, q.ReadOnly)
	case auth.ProviderAnthropic:
		snap, err = t.anthropicUsage(ctx, q.APIKey)
	case auth.ProviderOpenRoute:
		snap, err = t.openRouterUsage(ctx, q.BaseURL, q.APIKey)
	default:
		snap, err = t.pluginUsage(ctx, slug, q.BaseURL, q.APIKey)
	}
	if err != nil {
		return nil, err
	}
	t.remember(q.Provider, snap, q.IdentityID, q.BaseURL, started)
	return snap, nil
}

func builtinUsage(slug string) bool {
	return slug == auth.ProviderCodex || slug == auth.ProviderAnthropic || slug == auth.ProviderOpenRoute
}

func (t *Tracker) pluginUsage(ctx context.Context, slug, baseURL string, key auth.Secret) (*providers.AccountUsage, error) {
	if slug == "" {
		return nil, nil
	}
	profile, ok := t.registry.Get(ctx, slug)
	if !ok || profile == nil {
		return nil, nil
	}
	fetcher, ok := profile.(providers.UsageFetcher)
	if !ok {
		return nil, nil
	}
	hctx, cancel := context.WithTimeout(ctx, pluginHookDeadline)
	defer cancel()
	type result struct {
		snap *providers.AccountUsage
		err  error
	}
	done := make(chan result, 1)
	go func() {
		snap, err := fetcher.AccountUsage(hctx, baseURL, key)
		done <- result{snap, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return nil, r.err
		}
		return normalizePluginSnapshot(r.snap), nil
	case <-hctx.Done():
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
}

func normalizePluginSnapshot(snap *providers.AccountUsage) *providers.AccountUsage {
	if snap == nil {
		return nil
	}
	out := *snap
	if out.Title == "" {
		out.Title = defaultTitle
	}
	if out.FetchedAt.IsZero() {
		out.FetchedAt = time.Now().UTC()
	}
	out.Windows = make([]providers.UsageWindow, len(snap.Windows))
	for i, w := range snap.Windows {
		if w.Scope == "" {
			w.Scope = scopeAccount
		}
		out.Windows[i] = w
	}
	out.Details = append([]string(nil), snap.Details...)
	return &out
}

func parseDT(v any) (time.Time, bool, error) {
	switch x := v.(type) {
	case nil:
		return time.Time{}, false, nil
	case string:
		text := strings.TrimSpace(x)
		if text == "" {
			return time.Time{}, false, nil
		}
		parsed, ok := parseISO(text)
		return parsed, ok, nil
	case json.Number, bool:
		seconds, _ := number(x)
		if math.Abs(seconds) > maxValidUnixSeconds {
			return time.Time{}, false, errMalformed
		}
		whole, frac := math.Modf(seconds)
		return time.Unix(int64(whole), int64(math.Round(frac*1e6))*1000).UTC(), true, nil
	}
	return time.Time{}, false, nil
}

type windowSpec struct{ key, label string }

func usageWindows(source map[string]any, mapping []windowSpec, usedKey, resetKey string, fraction bool, modelScoped map[string]bool) ([]providers.UsageWindow, error) {
	var windows []providers.UsageWindow
	for _, spec := range mapping {
		window, err := objectAt(source, spec.key)
		if err != nil {
			return nil, err
		}
		raw, present := window[usedKey]
		if !present || raw == nil {
			continue
		}
		used, err := toFloat(raw)
		if err != nil {
			return nil, err
		}
		if fraction && used <= 1 {
			used *= 100
		}
		reset, _, err := parseDT(window[resetKey])
		if err != nil {
			return nil, err
		}
		scope := scopeAccount
		if modelScoped[spec.key] {
			scope = scopeModel
		}
		windows = append(windows, providers.UsageWindow{Label: spec.label, UsedPercent: &used, ResetAt: reset, Scope: scope})
	}
	return windows, nil
}

type codexCred struct {
	token     string
	baseURL   string
	accountID string
}

type codexEndpoints struct{ usage, consume string }

func codexURLs(baseURL string) codexEndpoints {
	normalized := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if normalized == "" {
		normalized = auth.DefaultCodexBaseURL
	}
	normalized = strings.TrimSuffix(normalized, "/codex")
	prefix := normalized + "/api/codex"
	if strings.Contains(normalized, "/backend-api") {
		prefix = normalized + "/wham"
	}
	return codexEndpoints{usage: prefix + "/usage", consume: prefix + "/rate-limit-reset-credits/consume"}
}

func codexHeaders(c codexCred) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+c.token)
	h.Set("Accept", "application/json")
	h.Set("User-Agent", "codex-cli")
	for k, v := range auth.CodexAccountHeaders(c.token) {
		h.Set(k, v)
	}
	if c.accountID != "" {
		h.Set("ChatGPT-Account-ID", c.accountID)
	}
	return h
}

func (t *Tracker) codexSingleton(ctx context.Context) (access, accountID string, ok bool) {
	raw, found, err := t.auth.State(ctx, auth.ProviderCodex)
	if err != nil || !found {
		return "", "", false
	}
	var state struct {
		Tokens map[string]any `json:"tokens"`
	}
	if json.Unmarshal(raw, &state) != nil || state.Tokens == nil {
		return "", "", false
	}
	access, _ = state.Tokens["access_token"].(string)
	refresh, _ := state.Tokens["refresh_token"].(string)
	access = strings.TrimSpace(access)
	if access == "" || strings.TrimSpace(refresh) == "" {
		return "", "", false
	}
	if id, isText := state.Tokens["account_id"].(string); isText {
		accountID = strings.TrimSpace(id)
	}
	return access, accountID, true
}

func (t *Tracker) codexRouteBase(ctx context.Context, entryBase string) string {
	base := strings.TrimRight(strings.TrimSpace(entryBase), "/")
	if value, _, err := t.cfg.Env().Lookup(ctx, codexEnvBaseURL); err == nil {
		if override := strings.TrimRight(strings.TrimSpace(value), "/"); override != "" {
			return override
		}
	}
	if base == "" || base == auth.DefaultCodexBaseURL {
		if cfg, err := t.cfg.LoadReadonly(ctx); err == nil && !cfg.Model.Bare && strings.ToLower(strings.TrimSpace(cfg.Model.Provider)) == auth.ProviderCodex {
			if configured := strings.TrimRight(strings.TrimSpace(cfg.Model.BaseURL), "/"); configured != "" {
				base = configured
			}
		}
	}
	if base == "" {
		return auth.DefaultCodexBaseURL
	}
	return base
}

func (t *Tracker) codexPoolCred(ctx context.Context, entry auth.Entry, fallbackBase string) codexCred {
	base := entry.RuntimeBaseURL(ctx)
	if base == "" {
		base = fallbackBase
	}
	key := entry.RuntimeAPIKey()
	return codexCred{token: key.Reveal(), baseURL: t.codexRouteBase(ctx, base)}
}

func (t *Tracker) codexCredentials(ctx context.Context, baseURL string, explicit auth.Secret, force bool) (codexCred, error) {
	explicitKey := strings.TrimSpace(explicit.Reveal())
	if explicitKey != "" && !force {
		return codexCred{token: explicitKey, baseURL: strings.TrimSpace(baseURL)}, nil
	}
	if explicitKey != "" {
		singleton, _, _ := t.codexSingleton(ctx)
		if singleton != explicitKey {
			pool, err := t.auth.Pool(ctx, auth.ProviderCodex)
			if err != nil {
				return codexCred{}, err
			}
			entry, ok, err := pool.TryRefreshMatching(ctx, auth.NewSecret(explicitKey), "")
			if err != nil {
				return codexCred{}, err
			}
			if !ok {
				return codexCred{}, errCodexSessionRefresh
			}
			return t.codexPoolCred(ctx, entry, baseURL), nil
		}
	}
	creds, err := t.auth.ResolveCodex(ctx, auth.ResolveOptions{ForceRefresh: force})
	if err == nil {
		_, accountID, _ := t.codexSingleton(ctx)
		return codexCred{token: creds.APIKey.Reveal(), baseURL: strings.TrimSpace(creds.BaseURL), accountID: accountID}, nil
	}
	var authErr *auth.Error
	if !errors.As(err, &authErr) {
		return codexCred{}, err
	}
	pool, err := t.auth.Pool(ctx, auth.ProviderCodex)
	if err != nil {
		return codexCred{}, err
	}
	entry, ok, err := pool.Select(ctx, "")
	if err != nil {
		return codexCred{}, err
	}
	if !ok {
		return codexCred{}, errCodexPoolEmpty
	}
	return t.codexPoolCred(ctx, entry, baseURL), nil
}

func codexBankedResets(payload map[string]any) (int, error) {
	credits, err := objectAt(payload, "rate_limit_reset_credits")
	if err != nil {
		return 0, err
	}
	if n, ok := number(credits["available_count"]); ok {
		return int(n), nil
	}
	return 0, nil
}

func codexWindowLabels(rateLimit map[string]any) []windowSpec {
	bySeconds := map[int]string{18000: "Session", 604800: "Weekly"}
	var specs []windowSpec
	for _, positional := range []windowSpec{{"primary_window", "Session"}, {"secondary_window", "Weekly"}} {
		label := positional.label
		if window, ok := rateLimit[positional.key].(map[string]any); ok {
			if seconds, isNum := number(window["limit_window_seconds"]); isNum {
				if named, known := bySeconds[int(seconds)]; known {
					label = named
				}
			}
		}
		specs = append(specs, windowSpec{positional.key, label})
	}
	return specs
}

func (t *Tracker) codexUsage(ctx context.Context, baseURL string, key auth.Secret, readOnly bool) (*providers.AccountUsage, error) {
	cred, err := t.codexCredentials(ctx, baseURL, key, false)
	if err != nil {
		return nil, err
	}
	payload, raw, err := t.requestJSON(ctx, http.MethodGet, codexURLs(cred.baseURL).usage, codexHeaders(cred), nil, usageFetchTimeout)
	var status *httpclient.StatusError
	if err != nil && errors.As(err, &status) && status.Code == http.StatusUnauthorized && !readOnly {
		if cred, err = t.codexCredentials(ctx, baseURL, key, true); err != nil {
			return nil, err
		}
		payload, raw, err = t.requestJSON(ctx, http.MethodGet, codexURLs(cred.baseURL).usage, codexHeaders(cred), nil, usageFetchTimeout)
	}
	if err != nil {
		return nil, err
	}
	rateLimit, err := objectAt(payload, "rate_limit")
	if err != nil {
		return nil, err
	}
	windows, err := usageWindows(rateLimit, codexWindowLabels(rateLimit), "used_percent", "reset_at", false, nil)
	if err != nil {
		return nil, err
	}
	var details []string
	count, err := codexBankedResets(payload)
	if err != nil {
		return nil, err
	}
	if count > 0 {
		details = append(details, fmt.Sprintf("You have %d reset%s banked - use /usage reset to activate", count, plural(count)))
	}
	credits, err := objectAt(payload, "credits")
	if err != nil {
		return nil, err
	}
	if balance, ok := number(credits["balance"]); !falsy(credits["has_credits"]) && ok {
		details = append(details, "Credits balance: $"+fixed(balance, 2))
	} else if !falsy(credits["has_credits"]) && !falsy(credits["unlimited"]) {
		details = append(details, "Credits balance: unlimited")
	}
	snap := newSnapshot(auth.ProviderCodex, "usage_api", windows, details)
	if plan := payload["plan_type"]; !falsy(plan) {
		snap.Plan = titleCaseSlug(display(plan))
	}
	snap.Identity = codexIdentity(cred.token)
	snap.Raw = raw
	return snap, nil
}

func codexPrincipal(token string) (account, subject string, ok bool) {
	raw, decoded := auth.JWTClaims(token)
	if !decoded {
		return "", "", false
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return "", "", false
	}
	authClaims, _ := claims["https://api.openai.com/auth"].(map[string]any)
	account, _ = authClaims["chatgpt_account_id"].(string)
	subject, _ = claims["sub"].(string)
	account, subject = strings.TrimSpace(account), strings.TrimSpace(subject)
	return account, subject, account != "" && subject != ""
}

func codexIdentity(token string) string {
	account, subject, ok := codexPrincipal(token)
	if !ok {
		return ""
	}
	return "codex:" + account + ":" + subject
}

type CodexResetResult struct {
	Status         string `json:"status"`
	Message        string `json:"message"`
	AvailableCount int    `json:"available_count"`
	WindowsReset   int    `json:"windows_reset"`
}

func (r CodexResetResult) Redeemed() bool { return r.Status == codexResetReset }

func resetUnavailable(message string) CodexResetResult {
	return CodexResetResult{Status: codexResetUnusable, Message: message}
}

func codexResetGuard(payload map[string]any, available int, force bool) (*CodexResetResult, error) {
	if available <= 0 {
		return &CodexResetResult{Status: codexResetNoBanked, Message: "No banked reset credits on this account — nothing to redeem."}, nil
	}
	rateLimit, err := objectAt(payload, "rate_limit")
	if err != nil {
		return nil, err
	}
	worst, known := 0.0, false
	for _, key := range []string{"primary_window", "secondary_window"} {
		window, err := objectAt(rateLimit, key)
		if err != nil {
			return nil, err
		}
		if used, ok := number(window["used_percent"]); ok {
			worst, known = max(worst, used), true
		}
	}
	if force || (known && worst >= codexExhaustedPct) {
		return nil, nil
	}
	note := "your current usage could not be confirmed as exhausted"
	if known {
		note = "your busiest window is only " + fixed(worst, 0) + "% used"
	}
	return &CodexResetResult{
		Status: codexResetNotFull, AvailableCount: available,
		Message: string(rune(0x26A0)) + string(rune(0xFE0F)) + " Not redeeming: " + note + ". A banked reset restores your FULL 5h + weekly limits, so spending it " +
			"now would waste most of it. You have " + strconv.Itoa(available) + " reset" + plural(available) + " banked. " +
			"Use `/usage reset --force` to redeem anyway.",
	}, nil
}

func (t *Tracker) codexResetOutcome(ctx context.Context, body map[string]any, raw json.RawMessage, available int) CodexResetResult {
	code := strings.ToLower(strings.TrimSpace(display(body["code"])))
	if falsy(body["code"]) {
		code = ""
	}
	remaining := max(0, available-1)
	var message string
	count := remaining
	switch code {
	case codexResetReset:
		message = "✅ Reset redeemed — your usage limits have been reset. " + strconv.Itoa(remaining) + " banked reset" + plural(remaining) + " remaining."
	case codexResetNothing:
		message, count = "Backend reports nothing to reset — your limits aren't exhausted. The credit was NOT spent.", available
	case codexResetNoCredit:
		message, count = "Backend reports no available reset credit on this account.", 0
	case codexResetAlready:
		message = "This redemption was already processed — no additional credit was spent."
	default:
		return resetUnavailable("Unexpected response from the Codex backend: " + string(raw))
	}
	windowsReset := 0
	if code == codexResetReset {
		t.auth.ClearCodexPoolQuotaCooldowns(ctx, "")
		if n, ok := number(body["windows_reset"]); ok {
			windowsReset = int(n)
		}
	}
	return CodexResetResult{Status: code, Message: message, AvailableCount: count, WindowsReset: windowsReset}
}

func (t *Tracker) redeemOnce(ctx context.Context, cred codexCred, requestID string, force bool) (*CodexResetResult, map[string]any, json.RawMessage, int, error) {
	urls := codexURLs(cred.baseURL)
	headers := codexHeaders(cred)
	payload, _, err := t.requestJSON(ctx, http.MethodGet, urls.usage, headers, nil, usageFetchTimeout)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	available, err := codexBankedResets(payload)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	refused, err := codexResetGuard(payload, available, force)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	if refused != nil {
		return refused, nil, nil, available, nil
	}
	consume := headers.Clone()
	consume.Set("Content-Type", "application/json")
	request, err := json.Marshal(map[string]string{"redeem_request_id": requestID})
	if err != nil {
		return nil, nil, nil, 0, err
	}
	body, raw, err := t.requestJSON(ctx, http.MethodPost, urls.consume, consume, request, usageFetchTimeout)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	return nil, body, raw, available, nil
}

func (t *Tracker) RedeemCodexReset(ctx context.Context, baseURL string, apiKey auth.Secret, force bool) CodexResetResult {
	ctx = t.bind(ctx)
	cred, err := t.codexCredentials(ctx, baseURL, apiKey, false)
	if err != nil {
		return resetUnavailable("No Codex credentials available. Run `icebath auth` to sign in with your ChatGPT account.")
	}
	requestID := uuid.NewString()
	for attempt := 0; ; attempt++ {
		refused, body, raw, available, err := t.redeemOnce(ctx, cred, requestID, force)
		if err == nil {
			if refused != nil {
				return *refused
			}
			return t.codexResetOutcome(ctx, body, raw, available)
		}
		var status *httpclient.StatusError
		if !errors.As(err, &status) || status.Code != http.StatusUnauthorized || attempt > 0 {
			return redeemFailure(err)
		}
		refreshed, rerr := t.codexCredentials(ctx, baseURL, apiKey, true)
		if rerr != nil {
			return redeemFailure(err)
		}
		cred = refreshed
	}
}

func redeemFailure(err error) CodexResetResult {
	var status *httpclient.StatusError
	if errors.As(err, &status) {
		if status.Code == http.StatusUnauthorized || status.Code == http.StatusForbidden {
			return resetUnavailable(fmt.Sprintf("Codex backend rejected the request (HTTP %d). Reset credits require ChatGPT-account "+
				"(OAuth) auth — run `icebath auth` and sign in with your ChatGPT account.", status.Code))
		}
		return resetUnavailable(fmt.Sprintf("Codex backend error (HTTP %d) — try again shortly.", status.Code))
	}
	return resetUnavailable("Could not reach the Codex backend: " + err.Error())
}

func (t *Tracker) anthropicUsage(ctx context.Context, key auth.Secret) (*providers.AccountUsage, error) {
	token := strings.TrimSpace(key.Reveal())
	if token == "" {
		if resolved, ok := t.auth.ResolveAnthropicToken(ctx, ""); ok {
			token = strings.TrimSpace(resolved.Reveal())
		}
	}
	if token == "" {
		return nil, nil
	}
	if !auth.IsAnthropicOAuthToken(token) {
		snap := newSnapshot(auth.ProviderAnthropic, "oauth_usage_api", nil, nil)
		snap.UnavailableReason = "Anthropic account limits are only available for OAuth-backed Claude accounts."
		return snap, nil
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	h.Set("anthropic-beta", "oauth-2025-04-20")
	h.Set("User-Agent", "claude-code/2.1.0")
	payload, _, err := t.requestJSON(ctx, http.MethodGet, anthropicUsageURL, h, nil, usageFetchTimeout)
	if err != nil {
		return nil, err
	}
	windows, err := usageWindows(payload, []windowSpec{
		{"five_hour", "Current session"}, {"seven_day", "Current week"}, {"seven_day_opus", "Opus week"}, {"seven_day_sonnet", "Sonnet week"},
	}, "utilization", "resets_at", true, map[string]bool{"seven_day_opus": true, "seven_day_sonnet": true})
	if err != nil {
		return nil, err
	}
	var details []string
	extra, err := objectAt(payload, "extra_usage")
	if err != nil {
		return nil, err
	}
	used, usedOK := number(extra["used_credits"])
	limit, limitOK := number(extra["monthly_limit"])
	if !falsy(extra["is_enabled"]) && usedOK && limitOK {
		currency := "USD"
		if !falsy(extra["currency"]) {
			currency = display(extra["currency"])
		}
		details = append(details, "Extra usage: "+fixed(used, 2)+" / "+fixed(limit, 2)+" "+currency)
	}
	return newSnapshot(auth.ProviderAnthropic, "oauth_usage_api", windows, details), nil
}

func (t *Tracker) openRouterData(ctx context.Context, base, path string, h http.Header) (map[string]any, error) {
	body, _, err := t.requestJSON(ctx, http.MethodGet, base+"/"+path, h, nil, openRouterTimeout)
	if err != nil {
		return nil, err
	}
	return objectAt(body, "data")
}

func optionalFloat(v any) (float64, error) {
	if falsy(v) {
		return 0, nil
	}
	return toFloat(v)
}

func (t *Tracker) openRouterUsage(ctx context.Context, baseURL string, key auth.Secret) (*providers.AccountUsage, error) {
	rt, err := t.resolve(ctx, auth.ProviderOpenRoute, baseURL, key)
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(rt.APIKey.Reveal())
	if token == "" {
		return nil, nil
	}
	base := strings.TrimRight(rt.BaseURL, "/")
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Accept", "application/json")
	credits, err := t.openRouterData(ctx, base, "credits", h)
	if err != nil {
		return nil, err
	}
	keyData, err := t.openRouterData(ctx, base, "key", h)
	if err != nil {
		keyData = map[string]any{}
	}
	total, err := optionalFloat(credits["total_credits"])
	if err != nil {
		return nil, err
	}
	spent, err := optionalFloat(credits["total_usage"])
	if err != nil {
		return nil, err
	}
	details := []string{"Credits balance: $" + fixed(max(0.0, total-spent), 2)}
	var windows []providers.UsageWindow
	limitReset := ""
	if !falsy(keyData["limit_reset"]) {
		limitReset = strings.TrimSpace(display(keyData["limit_reset"]))
	}
	limit, limitOK := number(keyData["limit"])
	remaining, remainingOK := number(keyData["limit_remaining"])
	if limitOK && limit > 0 && remainingOK && remaining >= 0 && remaining <= limit {
		parts := []string{"$" + fixed(remaining, 2) + " of $" + fixed(limit, 2) + " remaining"}
		if limitReset != "" {
			parts = append(parts, "resets "+limitReset)
		}
		used := (limit - remaining) / limit * 100
		windows = append(windows, providers.UsageWindow{Label: "API key quota", UsedPercent: &used, Detail: strings.Join(parts, " • "), Scope: scopeAccount})
	}
	if usage, ok := number(keyData["usage"]); ok {
		parts := []string{"API key usage: $" + fixed(usage, 2) + " total"}
		for _, period := range []windowSpec{{"usage_daily", "today"}, {"usage_weekly", "this week"}, {"usage_monthly", "this month"}} {
			if value, isNum := number(keyData[period.key]); isNum && value > 0 {
				parts = append(parts, "$"+fixed(value, 2)+" "+period.label)
			}
		}
		details = append(details, strings.Join(parts, " • "))
	}
	return newSnapshot(auth.ProviderOpenRoute, "credits_api", windows, details), nil
}

func formatReset(at time.Time, now time.Time) string {
	if at.IsZero() {
		return "unknown"
	}
	stamp := at.Local().Format("2006-01-02 15:04 MST")
	total := int64(at.Sub(now).Seconds())
	if total <= 0 {
		return "now (" + stamp + ")"
	}
	hours, rem := total/3600, total%3600
	minutes := rem / 60
	if hours >= 24 {
		return fmt.Sprintf("in %dd %dh (%s)", hours/24, hours%24, stamp)
	}
	if hours != 0 {
		return fmt.Sprintf("in %dh %dm (%s)", hours, minutes, stamp)
	}
	return fmt.Sprintf("in %dm (%s)", minutes, stamp)
}

func RenderAccountUsageLines(snap *providers.AccountUsage, markdown bool) []string {
	if snap == nil {
		return nil
	}
	if snap.UsageWindow {
		return renderUsageWindowLines(snap)
	}
	bold, plan := "", ""
	if markdown {
		bold = "**"
	}
	if snap.Plan != "" {
		plan = " (" + snap.Plan + ")"
	}
	lines := []string{"\U0001f4c8 " + bold + snap.Title + bold, "Provider: " + snap.Provider + plan}
	now := time.Now()
	for _, w := range snap.Windows {
		var base string
		if w.UsedPercent == nil {
			base = w.Label + ": unavailable"
		} else {
			used := *w.UsedPercent
			base = fmt.Sprintf("%s: %d%% remaining (%d%% used)", w.Label, max(0, int64(math.RoundToEven(100-used))), max(0, int64(math.RoundToEven(used))))
		}
		if !w.ResetAt.IsZero() {
			base += " • resets " + formatReset(w.ResetAt, now)
		} else if w.Detail != "" {
			base += " • " + w.Detail
		}
		lines = append(lines, base)
	}
	lines = append(lines, snap.Details...)
	if snap.UnavailableReason != "" {
		lines = append(lines, "Unavailable: "+snap.UnavailableReason)
	}
	return lines
}

func renderUsageWindowLines(snap *providers.AccountUsage) []string {
	var lines []string
	if snap.Plan != "" {
		lines = []string{snap.Plan}
	} else {
		lines = []string{snap.Title, "Provider: " + snap.Provider}
	}
	width := 0
	for _, w := range snap.Windows {
		width = max(width, runeLen(w.Label))
	}
	now := time.Now()
	for _, w := range snap.Windows {
		label := w.Label
		if width > 0 {
			label = padRight(label, width)
		}
		var base string
		if w.UsedPercent == nil {
			base = label + "  unavailable"
		} else {
			used := max(0.0, min(100.0, *w.UsedPercent)) / 100.0
			base = label + "  " + renderBar(used, barWidth) + "  " + formatPercentUsed(used) + " used"
		}
		if !w.ResetAt.IsZero() {
			ms := w.ResetAt.UnixMilli()
			base += "  ·  " + formatResetsIn(&ms, now)
		} else if w.Detail != "" {
			base += "  ·  " + w.Detail
		}
		lines = append(lines, base)
	}
	lines = append(lines, snap.Details...)
	if snap.UnavailableReason != "" {
		lines = append(lines, "Unavailable: "+snap.UnavailableReason)
	}
	return lines
}
