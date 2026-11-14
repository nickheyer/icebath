package portal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/auth"
)

const (
	AccountSourceJWT          = "jwt"
	AccountSourceAPI          = "account_api"
	AccountSourceInferenceKey = "inference_key"
	AccountSourceNone         = "none"
	AccountSourceError        = "error"

	FreeTierNeedsAccount     = "This needs an Icebath Portal account. Run `icebath auth upgrade`."
	FreeTierNeedsAccountChat = "This needs an Icebath Portal account. Use /login to sign in."

	accountInfoCacheTTL = 60 * time.Second
	accountFetchTimeout = 8 * time.Second
)

var ToolCoverageCategories = []string{"firecrawl", "fal", "fal-video", "openai-audio", "browser-use", "modal"}

type SubscriptionInfo struct {
	Plan             string
	Tier             *int
	MonthlyCharge    *float64
	MonthlyCredits   *float64
	CurrentPeriodEnd string
	CreditsRemaining *float64
	RolloverCredits  *float64
}

type PaidAccessInfo struct {
	Allowed                      *bool
	PaidAccess                   *bool
	Reason                       string
	OrganisationID               string
	EffectiveAtMS                *int
	HasActiveSubscription        *bool
	ActiveSubscriptionIsPaid     *bool
	SubscriptionTier             *int
	SubscriptionMonthlyCharge    *float64
	SubscriptionCreditsRemaining *float64
	PurchasedCreditsRemaining    *float64
	TotalUsableCredits           *float64
	MemberSpendCapExceeded       *bool
	MemberSpendCapUSD            *float64
	MemberSpendUSD               *float64
	MemberSpendCapRemainingUSD   *float64
}

type UsageWindowInfo struct {
	Gauges             []WindowGauge
	FeaturedModelIDs   []string
	AutoTopUpEnabled   *bool
	PausedReason       string
	KeepGoingOnCredits *bool
}

type ToolAccessInfo struct {
	Enabled  bool
	Coverage map[string]bool
}

type AccountInfo struct {
	LoggedIn                   bool
	Source                     string
	Fresh                      bool
	UserID                     string
	OrgID                      string
	OrgSlug                    string
	OrgName                    string
	ClientID                   string
	ProductID                  string
	PortalClient               string
	PortalBaseURL              string
	InferenceBaseURL           string
	InferenceCredentialPresent bool
	CredentialSource           string
	ExpiresAt                  time.Time
	Email                      string
	PrivyDID                   string
	Subscription               *SubscriptionInfo
	PaidServiceAccess          *bool
	PaidServiceAccessInfo      *PaidAccessInfo
	ToolAccess                 *ToolAccessInfo
	UsageWindow                *UsageWindowInfo
	RawClaims                  json.RawMessage
	RawAccount                 json.RawMessage
	Error                      string
	AccountTier                string
	ManagedTools               *bool
}

func isTrue(b *bool) bool { return b != nil && *b }

func isFalse(b *bool) bool { return b != nil && !*b }

func (i *AccountInfo) IsPaid() bool { return isTrue(i.PaidServiceAccess) }

func (i *AccountInfo) IsAnonymousTier() bool {
	return i != nil && strings.ToLower(strings.TrimSpace(i.AccountTier)) == AnonAccountTier
}

func (i *AccountInfo) IsFreeTier() bool { return isFalse(i.PaidServiceAccess) }

func (i *AccountInfo) ToolGatewayEntitled() bool {
	return isTrue(i.PaidServiceAccess) || (i.ToolAccess != nil && i.ToolAccess.Enabled)
}

func (i *AccountInfo) ToolGatewayEntitledFor(category string) bool {
	return isTrue(i.PaidServiceAccess) || (i.ToolAccess != nil && i.ToolAccess.Enabled && i.ToolAccess.Coverage[category])
}

func (i *AccountInfo) ManagedToolsRolledOut() bool { return isTrue(i.ManagedTools) }

func BillingURL(info *AccountInfo) string {
	base := auth.DefaultPortalURL
	if info != nil && strings.TrimSpace(info.PortalBaseURL) != "" {
		base = info.PortalBaseURL
	}
	return strings.TrimRight(base, "/") + "/billing"
}

func TopupURL(info *AccountInfo) string {
	base := strings.TrimSuffix(BillingURL(info), "/billing")
	if info != nil && strings.TrimSpace(info.OrgSlug) != "" {
		return fmt.Sprintf("%s/orgs/%s/billing?topup=open", base, quotePathSegment(strings.TrimSpace(info.OrgSlug)))
	}
	return base + "/billing?topup=open"
}

func windowSeedReady(window *UsageWindowInfo) bool {
	return window != nil && len(window.Gauges) > 0 && len(AllScope(window.Gauges)) > 0
}

func UsageWindowBlockGuidance(info *AccountInfo) string {
	if info == nil || !windowSeedReady(info.UsageWindow) {
		return ""
	}
	window := info.UsageWindow
	if window.PausedReason != "" {
		fix := "Included usage is paused"
		if PausedForPayment(window.PausedReason) {
			fix = "Update your card to restore included usage"
		}
		return fmt.Sprintf("%s, or add credits to keep going now: %s", fix, BillingURL(info))
	}
	micros := int64(0)
	if access := info.PaidServiceAccessInfo; access != nil && access.PurchasedCreditsRemaining != nil {
		micros = int64(math.Trunc(*access.PurchasedCreditsRemaining * 1_000_000))
	}
	switch CreditsAction(window.KeepGoingOnCredits, micros) {
	case UseCredits:
		return "Turn on Keep going in /topup to use your credits now: " + BillingURL(info)
	case AddCreditsAndKeepGoing:
		return "Add credits and turn on Keep going in /topup to keep going now: " + BillingURL(info)
	}
	lines := []string{"Add credits to keep going now: " + TopupURL(info)}
	if isFalse(window.AutoTopUpEnabled) {
		lines = append(lines, "Turn on auto top-up in the same place so this doesn't stop you next time.")
	}
	return strings.Join(lines, "\n")
}

type EntitlementOptions struct {
	Capability       string
	NoRefreshHint    bool
	CoverageCategory string
	InChat           bool
}

func EntitlementMessage(info *AccountInfo, o EntitlementOptions) string {
	capability := o.Capability
	if capability == "" {
		capability = "this feature"
	}
	needsAccount := FreeTierNeedsAccount
	if o.InChat {
		needsAccount = FreeTierNeedsAccountChat
	}
	if info.IsAnonymousTier() {
		return needsAccount
	}
	billingURL := BillingURL(info)
	if info == nil {
		return fmt.Sprintf("icebath could not verify your Icebath Portal entitlement, so %s is unavailable. "+
			"Run `icebath model` to refresh your login, or check billing at %s.", capability, billingURL)
	}
	if o.CoverageCategory != "" {
		if info.ToolGatewayEntitledFor(o.CoverageCategory) {
			return ""
		}
		if info.ToolGatewayEntitled() {
			return fmt.Sprintf("%s isn't included with your current Icebath Portal access. "+
				"Add credits or a subscription to enable it at %s.", capability, billingURL)
		}
	} else if info.ToolGatewayEntitled() {
		return ""
	}
	if !info.LoggedIn {
		if info.InferenceCredentialPresent {
			return fmt.Sprintf("Icebath Portal inference credentials are configured, but icebath cannot verify your Icebath Portal "+
				"paid access for %s. Log in with `icebath model` to enable Portal-managed "+
				"features. Billing and credits are managed at %s.", capability, billingURL)
		}
		return fmt.Sprintf("Log in to Icebath Portal to use %s: run `icebath model`. "+
			"Billing and credits are managed at %s.", capability, billingURL)
	}
	if info.PaidServiceAccess == nil {
		detail := fmt.Sprintf("icebath could not verify your Icebath Portal paid access, so %s is unavailable.", capability)
		if info.Error != "" {
			detail += fmt.Sprintf(" Account lookup failed: %s.", info.Error)
		}
		if !o.NoRefreshHint {
			detail += " Run `icebath model` to refresh your session."
		}
		return detail + fmt.Sprintf(" Check billing at %s.", billingURL)
	}
	reason := ""
	if info.PaidServiceAccessInfo != nil {
		reason = info.PaidServiceAccessInfo.Reason
	}
	if reason == "account_missing" {
		return fmt.Sprintf("icebath could not find an Icebath Portal account or organisation for this login, so %s "+
			"is unavailable. Run `icebath model` to authenticate again; if the problem persists, contact Icebath Portal support.", capability)
	}
	if reason == "no_usable_credits" || isFalse(info.PaidServiceAccess) {
		message := noPaidAccessMessage(info, capability, billingURL, needsAccount)
		if !o.NoRefreshHint && !info.Fresh {
			message += " If you recently bought credits, run `icebath model` to refresh icebath."
		}
		return message
	}
	return fmt.Sprintf("Your Icebath Portal account does not currently have paid service access, "+
		"so %s is unavailable. Add credits or update billing at %s.", capability, billingURL)
}

func noPaidAccessMessage(info *AccountInfo, capability, billingURL, needsAccount string) string {
	if info.IsAnonymousTier() {
		return needsAccount
	}
	access := info.PaidServiceAccessInfo
	if access == nil {
		access = &PaidAccessInfo{}
	}
	parts := []string{}
	for _, labelled := range []struct {
		label  string
		amount *float64
	}{{"usable", access.TotalUsableCredits}, {"subscription", access.SubscriptionCreditsRemaining}, {"purchased", access.PurchasedCreditsRemaining}} {
		if labelled.amount != nil {
			parts = append(parts, fmt.Sprintf("%s $%.2f", labelled.label, *labelled.amount))
		}
	}
	creditDetail := ""
	if len(parts) > 0 {
		creditDetail = " (" + strings.Join(parts, ", ") + ")"
	}
	if isTrue(access.MemberSpendCapExceeded) {
		capDetail := ""
		if limit, spent := access.MemberSpendCapUSD, access.MemberSpendUSD; limit != nil && spent != nil {
			capDetail = fmt.Sprintf(" Your organisation's per-member spend cap is $%.2f and you've spent $%.2f of it.", *limit, *spent)
		} else if limit != nil {
			capDetail = fmt.Sprintf(" Your organisation's per-member spend cap is $%.2f.", *limit)
		}
		return fmt.Sprintf("Your Icebath Portal access is paused because you've exceeded the per-member spend cap set by "+
			"your organisation.%s%s Ask your organisation admin to raise the "+
			"member spend cap at %s, then run `icebath model` to refresh.", capDetail, creditDetail, billingURL)
	}
	active, paid := access.HasActiveSubscription, access.ActiveSubscriptionIsPaid
	switch {
	case isTrue(active) && isTrue(paid):
		return fmt.Sprintf("Your Icebath Portal credits are exhausted%s, so %s is unavailable. "+
			"Top up or renew credits at %s.", creditDetail, capability, billingURL)
	case isTrue(active) && isFalse(paid):
		return fmt.Sprintf("Your current Icebath Portal plan does not include paid service access, "+
			"so %s is unavailable. Upgrade or add credits at %s.", capability, billingURL)
	case isFalse(active):
		return fmt.Sprintf("Your Icebath Portal account has no active subscription or usable credits%s, "+
			"so %s is unavailable. Subscribe or add credits at %s.", creditDetail, capability, billingURL)
	}
	return fmt.Sprintf("Your Icebath Portal account has no usable paid credits%s, so "+
		"%s is unavailable. Add credits or update billing at %s.", creditDetail, capability, billingURL)
}

type accountCache struct {
	key      string
	at       time.Time
	info     *AccountInfo
	identity string
}

func (a *Account) ResetAccountInfoCache() {
	a.mu.Lock()
	a.accountMemo = nil
	a.mu.Unlock()
}

func errorInfo(cause string, loggedIn bool, portalBaseURL string, rawAccount json.RawMessage, tier string) *AccountInfo {
	return &AccountInfo{LoggedIn: loggedIn, Source: AccountSourceError, PortalBaseURL: portalBaseURL, RawAccount: rawAccount, Error: cause, AccountTier: tier}
}

func (a *Account) statePortalURL(ctx context.Context, state *object) string {
	value := a.portalOverride(ctx)
	if value == "" {
		value = state.str("portal_base_url")
	}
	return strings.TrimRight(strings.TrimSpace(value), "/")
}

type AccountOptions struct {
	ForceFresh bool
	MinJWTTTL  time.Duration
}

func (a *Account) AccountInfo(ctx context.Context, o AccountOptions) *AccountInfo {
	ctx = a.bind(ctx)
	minTTL := o.MinJWTTTL
	if minTTL == 0 {
		minTTL = 60 * time.Second
	}
	state, err := a.currentState(ctx)
	if err != nil {
		return errorInfo(err.Error(), false, "", nil, "")
	}
	if state == nil {
		state = newObject()
	}
	accessToken := ""
	if !a.guestWithoutFreeTier(ctx, state) {
		accessToken = heldConnectorsToken(state, nil)
	}
	portalBaseURL := a.statePortalURL(ctx, state)
	if strings.TrimSpace(accessToken) == "" {
		if info := a.infoFromOAuthPool(ctx, o.ForceFresh, minTTL, portalBaseURL); info != nil {
			return info
		}
		if info := a.infoFromInferenceKeyPool(ctx, portalBaseURL); info != nil {
			return info
		}
		return &AccountInfo{Source: AccountSourceNone, PortalBaseURL: portalBaseURL}
	}
	if !o.ForceFresh {
		if info := infoFromValidJWT(accessToken, state, portalBaseURL, minTTL); info != nil {
			return info
		}
	}
	return a.freshAccountInfo(ctx, state, o.ForceFresh, portalBaseURL)
}

func (a *Account) PolicyPresent(ctx context.Context) *bool {
	state := a.quietState(ctx)
	if !nonEmpty(state.get("access_token")) {
		return nil
	}
	return boolOf(claimsOf(state.get("access_token")).get("policy_present"))
}

func (a *Account) PolicyNotice(ctx context.Context, removed bool) string {
	if !removed || !isTrue(a.PolicyPresent(ctx)) {
		return ""
	}
	return "Your organization restricts which models are available — models outside its policy are not listed."
}

func accountIdentity(accessToken, portalBaseURL string) string {
	claims := claimsOf(accessToken)
	identity := ""
	if sub, org := claims.get("sub"), claims.get("org_id"); nonEmpty(sub) && nonEmpty(org) {
		identity = fmt.Sprintf("jwt:%s:%s", sub, org)
	} else {
		sum := sha256.Sum256([]byte(accessToken))
		identity = hex.EncodeToString(sum[:])
	}
	return portalBaseURL + ":" + identity
}

func (a *Account) CachedAccountInfo(ctx context.Context, anyAge bool) *AccountInfo {
	ctx = a.bind(ctx)
	a.mu.Lock()
	cached := a.accountMemo
	a.mu.Unlock()
	if cached == nil || (!anyAge && time.Since(cached.at) >= accountInfoCacheTTL) {
		return nil
	}
	state, err := a.currentState(ctx)
	if err != nil {
		slog.Debug("Cached Icebath Portal account info unreadable", "error", err)
		return nil
	}
	accessToken := state.str("access_token")
	if strings.TrimSpace(accessToken) == "" || cached.identity == "" || cached.identity != accountIdentity(accessToken, a.statePortalURL(ctx, state)) {
		return nil
	}
	return cached.info
}

func (a *Account) freshAccountInfo(ctx context.Context, state *object, forceFresh bool, portalBaseURL string) *AccountInfo {
	fail := func(err error) *AccountInfo {
		return errorInfo(err.Error(), truthy(state.get("access_token")), portalBaseURL, nil, state.str("account_tier"))
	}
	secret, err := a.AccessToken(ctx, AccessTokenOptions{})
	if err != nil {
		return fail(err)
	}
	accessToken := secret.Reveal()
	refreshed, err := a.currentState(ctx)
	if err != nil {
		return fail(err)
	}
	if refreshed.size() == 0 {
		refreshed = state
	}
	if fresh := a.statePortalURL(ctx, refreshed); fresh != "" {
		portalBaseURL = fresh
	}
	sum := sha256.Sum256([]byte(accessToken))
	cacheKey := portalBaseURL + ":" + hex.EncodeToString(sum[:])
	a.mu.Lock()
	cached := a.accountMemo
	a.mu.Unlock()
	if !forceFresh && cached != nil && cached.key == cacheKey && time.Since(cached.at) < accountInfoCacheTTL {
		return cached.info
	}
	info, err := a.infoFromFetchedAccount(ctx, accessToken, refreshed, portalBaseURL)
	if err != nil {
		return fail(err)
	}
	if info.Source != AccountSourceError {
		a.mu.Lock()
		a.accountMemo = &accountCache{key: cacheKey, at: time.Now(), info: info, identity: accountIdentity(accessToken, portalBaseURL)}
		a.mu.Unlock()
	}
	return info
}

func (a *Account) bestPoolEntry(ctx context.Context, skipAnonymous bool) (*auth.Entry, bool) {
	pool, err := a.store.Pool(ctx, Provider)
	if err != nil || !pool.HasCredentials() {
		return nil, false
	}
	entries := pool.Entries()
	best := -1
	var bestKey [3]float64
	for i := range entries {
		e := &entries[i]
		if skipAnonymous && e.ExtraString("auth_method") == AnonAuthMethod {
			continue
		}
		key := [3]float64{stampEpoch(e.AgentKeyExpiresAt), stampEpoch(e.ExpiresAt), -float64(e.Priority)}
		if best < 0 || keyGreater(key, bestKey) {
			best, bestKey = i, key
		}
	}
	if best < 0 {
		return nil, false
	}
	return &entries[best], true
}

func entryInferenceURL(ctx context.Context, e *auth.Entry) string {
	return firstText(e.InferenceBaseURL, e.RuntimeBaseURL(ctx), e.BaseURL)
}

func entryLabel(e *auth.Entry) string {
	if e.Label == "" {
		return "unknown"
	}
	return e.Label
}

func (a *Account) infoFromInferenceKeyPool(ctx context.Context, portalBaseURL string) *AccountInfo {
	entry, ok := a.bestPoolEntry(ctx, false)
	if !ok {
		return nil
	}
	if strings.TrimSpace(firstText(entry.RuntimeAPIKey().Reveal(), entry.AccessToken.Reveal())) == "" {
		return nil
	}
	return &AccountInfo{
		Source: AccountSourceInferenceKey, PortalBaseURL: firstText(entry.ExtraString("portal_base_url"), portalBaseURL),
		InferenceBaseURL: entryInferenceURL(ctx, entry), InferenceCredentialPresent: true,
		CredentialSource: "pool:" + entryLabel(entry), Error: "portal_oauth_missing",
	}
}

func (a *Account) infoFromOAuthPool(ctx context.Context, forceFresh bool, minTTL time.Duration, portalBaseURL string) *AccountInfo {
	entry, ok := a.bestPoolEntry(ctx, false)
	if !ok || strings.TrimSpace(entry.AccessToken.Reveal()) == "" {
		return nil
	}
	authType := strings.ToLower(strings.TrimSpace(entry.AuthType))
	if !strings.HasPrefix(authType, "oauth") && entry.RefreshToken.Empty() {
		return nil
	}
	accessToken := entry.AccessToken.Reveal()
	entryPortalURL := firstText(entry.ExtraString("portal_base_url"), portalBaseURL)
	state := pairs("access_token", accessToken, "client_id", nullable(entry.ExtraString("client_id")),
		"inference_base_url", nullable(entryInferenceURL(ctx, entry)), "agent_key", nullable(entry.AgentKey.Reveal()),
		"credential_source", "pool:"+entryLabel(entry))
	if !forceFresh {
		if info := infoFromValidJWT(accessToken, state, entryPortalURL, minTTL); info != nil {
			return info
		}
	}
	info, err := a.infoFromFetchedAccount(ctx, accessToken, state, entryPortalURL)
	if err != nil {
		return errorInfo(err.Error(), true, entryPortalURL, nil, state.str("account_tier"))
	}
	return info
}

func (a *Account) fetchAccount(ctx context.Context, accessToken, portalBaseURL string) (*object, error) {
	base := portalBaseURL
	if base == "" {
		base = auth.DefaultPortalURL
	}
	client, _, err := a.verifiedClient(ctx, nil, "", nil)
	if err != nil {
		return nil, err
	}
	r, err := exchange(ctx, client, http.MethodGet, strings.TrimRight(base, "/")+"/api/oauth/account",
		map[string]string{"Authorization": "Bearer " + accessToken}, nil, accountFetchTimeout)
	if err != nil {
		return nil, err
	}
	if r.status < 200 || r.status >= 300 {
		return nil, fmt.Errorf("HTTP Error %d: %s", r.status, http.StatusText(r.status))
	}
	value, err := parseValue(r.body)
	if err != nil {
		return nil, errors.New("account endpoint answered without JSON")
	}
	payload, _ := value.(*object)
	if payload == nil {
		payload = newObject()
	}
	return payload, nil
}

func (a *Account) infoFromFetchedAccount(ctx context.Context, accessToken string, state *object, portalBaseURL string) (*AccountInfo, error) {
	payload, err := a.fetchAccount(ctx, accessToken, portalBaseURL)
	if err != nil {
		return nil, err
	}
	tier := state.str("account_tier")
	if payload.size() == 0 {
		return errorInfo("empty_account_response", true, portalBaseURL, nil, tier), nil
	}
	if code, ok := payload.get("error").(string); ok {
		if code == "" {
			code = "account_response_error"
		}
		return errorInfo(code, true, portalBaseURL, payload.raw(), tier), nil
	}
	return infoFromAccountPayload(payload, state, portalBaseURL), nil
}

func boolOf(v any) *bool {
	b, ok := v.(bool)
	if !ok {
		return nil
	}
	return &b
}

func intPtr(v any) *int {
	if _, isBool := v.(bool); isBool {
		return nil
	}
	n, ok := intOf(v)
	if !ok {
		return nil
	}
	out := int(n)
	return &out
}

func floatPtr(v any) *float64 {
	if _, isBool := v.(bool); isBool {
		return nil
	}
	f, ok := floatOf(v)
	if !ok {
		return nil
	}
	return &f
}

func infoFromValidJWT(token string, state *object, portalBaseURL string, minTTL time.Duration) *AccountInfo {
	claims := claimsOf(token)
	if claims.size() == 0 {
		return nil
	}
	exp := floatPtr(claims.get("exp"))
	if exp == nil || *exp <= epochOf(time.Now())+max(0, minTTL.Truncate(time.Second)).Seconds() {
		return nil
	}
	paid := boolOf(claims.get("paid_access"))
	clientID := claims.str("client_id")
	if !truthy(claims.get("client_id")) {
		clientID = state.str("client_id")
	}
	sec, frac := math.Modf(*exp)
	return &AccountInfo{
		LoggedIn: true, Source: AccountSourceJWT,
		UserID: claims.str("sub"), OrgID: claims.str("org_id"), ClientID: clientID, ProductID: claims.str("product_id"),
		PortalClient: claims.str("portal_client"), PortalBaseURL: portalBaseURL, InferenceBaseURL: state.str("inference_base_url"),
		InferenceCredentialPresent: true, CredentialSource: firstText(state.str("credential_source"), "auth_store"),
		ExpiresAt:         time.Unix(int64(sec), int64(frac*1e9)).UTC(),
		PaidServiceAccess: paid,
		PaidServiceAccessInfo: &PaidAccessInfo{Allowed: paid, PaidAccess: paid, OrganisationID: claims.str("org_id"),
			SubscriptionTier: intPtr(claims.get("subscription_tier"))},
		ToolAccess: toolAccessFrom(claims.get("tool_access")), RawClaims: claims.raw(),
		AccountTier: firstText(claims.str("account_tier"), state.str("account_tier")), ManagedTools: boolOf(claims.get("managed_tools")),
	}
}

func paidAccessFrom(value any) *PaidAccessInfo {
	raw, ok := value.(*object)
	if !ok || raw == nil {
		return nil
	}
	return &PaidAccessInfo{
		Allowed: boolOf(raw.get("allowed")), PaidAccess: boolOf(raw.get("paid_access")), Reason: raw.str("reason"),
		OrganisationID: raw.str("organisation_id"), EffectiveAtMS: intPtr(raw.get("effective_at_ms")),
		HasActiveSubscription: boolOf(raw.get("has_active_subscription")), ActiveSubscriptionIsPaid: boolOf(raw.get("active_subscription_is_paid")),
		SubscriptionTier: intPtr(raw.get("subscription_tier")), SubscriptionMonthlyCharge: floatPtr(raw.get("subscription_monthly_charge")),
		SubscriptionCreditsRemaining: floatPtr(raw.get("subscription_credits_remaining")),
		PurchasedCreditsRemaining:    floatPtr(raw.get("purchased_credits_remaining")), TotalUsableCredits: floatPtr(raw.get("total_usable_credits")),
		MemberSpendCapExceeded: boolOf(raw.get("member_spend_cap_exceeded")), MemberSpendCapUSD: floatPtr(raw.get("member_spend_cap_usd")),
		MemberSpendUSD: floatPtr(raw.get("member_spend_usd")), MemberSpendCapRemainingUSD: floatPtr(raw.get("member_spend_cap_remaining_usd")),
	}
}

func subscriptionFrom(value any) *SubscriptionInfo {
	raw, ok := value.(*object)
	if !ok || raw == nil {
		return nil
	}
	return &SubscriptionInfo{
		Plan: raw.str("plan"), Tier: intPtr(raw.get("tier")), MonthlyCharge: floatPtr(raw.get("monthly_charge")),
		MonthlyCredits: floatPtr(raw.get("monthly_credits")), CurrentPeriodEnd: raw.str("current_period_end"),
		CreditsRemaining: floatPtr(raw.get("credits_remaining")), RolloverCredits: floatPtr(raw.get("rollover_credits")),
	}
}

func usageWindowFrom(value any) *UsageWindowInfo {
	raw, ok := value.(*object)
	if !ok || raw == nil {
		return nil
	}
	out := &UsageWindowInfo{
		Gauges: parseGauges(raw.get("gauges")), AutoTopUpEnabled: boolOf(raw.get("auto_top_up_enabled")),
		PausedReason: raw.str("paused_reason"), KeepGoingOnCredits: boolOf(raw.get("keep_going_on_credits")),
	}
	if items, ok := raw.get("featured_model_ids").([]any); ok {
		out.FeaturedModelIDs = []string{}
		for _, item := range items {
			if s, ok := item.(string); ok && s != "" {
				out.FeaturedModelIDs = append(out.FeaturedModelIDs, s)
			}
		}
	}
	return out
}

func toolAccessFrom(value any) *ToolAccessInfo {
	raw, ok := value.(*object)
	if !ok || raw == nil {
		return nil
	}
	coverage := map[string]bool{}
	if entries := raw.obj("coverage"); entries != nil {
		for _, k := range entries.keys {
			coverage[k] = entries.vals[k] == true
		}
	}
	return &ToolAccessInfo{Enabled: raw.get("enabled") == true, Coverage: coverage}
}

func infoFromAccountPayload(payload, state *object, portalBaseURL string) *AccountInfo {
	user, organisation := payload.obj("user"), payload.obj("organisation")
	access := paidAccessFrom(payload.get("paid_service_access"))
	var paid *bool
	orgID := organisation.str("id")
	if access != nil {
		paid = access.Allowed
		if paid == nil {
			paid = access.PaidAccess
		}
		orgID = firstText(orgID, access.OrganisationID)
	}
	return &AccountInfo{
		LoggedIn: true, Source: AccountSourceAPI, Fresh: true,
		OrgID: orgID, OrgSlug: organisation.str("slug"), OrgName: organisation.str("name"), ClientID: state.str("client_id"),
		PortalBaseURL: portalBaseURL, InferenceBaseURL: state.str("inference_base_url"),
		InferenceCredentialPresent: truthy(state.get("access_token")) || truthy(state.get("agent_key")),
		CredentialSource:           firstText(state.str("credential_source"), "auth_store"),
		Email:                      user.str("email"), PrivyDID: user.str("privy_did"),
		Subscription: subscriptionFrom(payload.get("subscription")), PaidServiceAccess: paid, PaidServiceAccessInfo: access,
		ToolAccess: toolAccessFrom(payload.get("tool_access")), UsageWindow: usageWindowFrom(payload.get("usage_window")),
		RawAccount:   payload.raw(),
		AccountTier:  firstText(payload.str("account_tier"), user.str("account_tier"), state.str("account_tier")),
		ManagedTools: boolOf(payload.get("managed_tools")),
	}
}

func (a *Account) ResolvedPortalBaseURL(ctx context.Context) string {
	ctx = a.bind(ctx)
	state := a.quietState(ctx)
	if state == nil {
		state = newObject()
	}
	return a.portalBaseURL(ctx, state)
}

func (a *Account) PortalEntitlementMessage(ctx context.Context, capability string) string {
	return EntitlementMessage(a.AccountInfo(ctx, AccountOptions{ForceFresh: true}), EntitlementOptions{Capability: capability})
}

func (a *Account) FormatEntitlementAuthError(ctx context.Context, err error) string {
	if message := a.PortalEntitlementMessage(ctx, "Icebath Portal model access"); message != "" {
		return message
	}
	return err.Error() + " Check credits or billing in Icebath Portal, then retry."
}
