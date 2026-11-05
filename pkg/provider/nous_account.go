package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
)

// Tool categories a free tool pool may fund
var ToolCoverageCategories = []string{"firecrawl", "fal", "fal-video", "openai-audio", "browser-use", "modal"}

const (
	nousAccountInfoCacheTTL  = 60 * time.Second
	nousAnonAccountTier      = "anonymous"
	FreeTierNeedsAccount     = "This needs a Nous account. Run `icebath auth upgrade`."
	FreeTierNeedsAccountChat = "This needs a Nous account. Use /login to sign in."
	nousPolicyNoticeText     = "Your organization restricts which models are available — models outside its policy are not listed."
)

// NousPortalSubscriptionInfo is the Portal subscription block
type NousPortalSubscriptionInfo struct {
	Plan             *string
	Tier             *int64
	MonthlyCharge    *float64
	MonthlyCredits   *float64
	CurrentPeriodEnd *string
	CreditsRemaining *float64
	RolloverCredits  *float64
}

// NousPaidServiceAccessInfo is the Portal paid service access block
type NousPaidServiceAccessInfo struct {
	Allowed                      *bool
	PaidAccess                   *bool
	Reason                       string
	OrganisationID               string
	EffectiveAtMs                *int64
	HasActiveSubscription        *bool
	ActiveSubscriptionIsPaid     *bool
	SubscriptionTier             *int64
	SubscriptionMonthlyCharge    *float64
	SubscriptionCreditsRemaining *float64
	PurchasedCreditsRemaining    *float64
	TotalUsableCredits           *float64
	MemberSpendCapExceeded       *bool
	MemberSpendCapUSD            *float64
	MemberSpendUSD               *float64
	MemberSpendCapRemainingUSD   *float64
}

// NousUsageWindowInfo is a usage window plan's allowances, auto top up, pause reason and featured picks
type NousUsageWindowInfo struct {
	Gauges             []WindowGauge
	FeaturedModelIDs   []string
	AutoTopUpEnabled   *bool
	PausedReason       string
	KeepGoingOnCredits *bool
}

// NousToolAccessInfo is the free tool pool entitlement, coverage maps a category to whether the pool funds it
type NousToolAccessInfo struct {
	Enabled  bool
	Coverage map[string]bool
}

// NousPortalAccountInfo is the normalized Nous Portal account entitlement snapshot
type NousPortalAccountInfo struct {
	LoggedIn                   bool
	Source                     string
	Fresh                      bool
	UserID                     string
	OrgID                      string
	OrgSlug                    string
	OrgName                    string
	ClientID                   string
	ProductID                  string
	NousClient                 string
	PortalBaseURL              string
	InferenceBaseURL           string
	InferenceCredentialPresent bool
	CredentialSource           string
	ExpiresAt                  *time.Time
	Email                      string
	PrivyDID                   string
	Subscription               *NousPortalSubscriptionInfo
	PaidServiceAccess          *bool
	PaidServiceAccessInfo      *NousPaidServiceAccessInfo
	ToolAccess                 *NousToolAccessInfo
	UsageWindow                *NousUsageWindowInfo
	RawClaims                  map[string]any
	RawAccount                 map[string]any
	Error                      string
	AccountTier                string
	ManagedTools               *bool
}

// IsPaid reports a literal true paid service access
func (i *NousPortalAccountInfo) IsPaid() bool {
	return i != nil && i.PaidServiceAccess != nil && *i.PaidServiceAccess
}

// IsAnonymousTier reports the guest tier compared case and whitespace insensitively
func (i *NousPortalAccountInfo) IsAnonymousTier() bool {
	return i != nil && strings.ToLower(pyStripSpace(i.AccountTier)) == nousAnonAccountTier
}

// IsFreeTier reports a literal false paid service access
func (i *NousPortalAccountInfo) IsFreeTier() bool {
	return i != nil && i.PaidServiceAccess != nil && !*i.PaidServiceAccess
}

// ToolGatewayEntitled reports paid access or a live free tool pool
func (i *NousPortalAccountInfo) ToolGatewayEntitled() bool {
	return i.IsPaid() || (i != nil && i.ToolAccess != nil && i.ToolAccess.Enabled)
}

// ToolGatewayEntitledFor reports paid access or a live pool whose coverage funds the category
func (i *NousPortalAccountInfo) ToolGatewayEntitledFor(category string) bool {
	return i.IsPaid() || (i != nil && i.ToolAccess != nil && i.ToolAccess.Enabled && i.ToolAccess.Coverage[category])
}

// ManagedToolsRolledOut reports a literal true managed tools flag from the portal
func (i *NousPortalAccountInfo) ManagedToolsRolledOut() bool {
	return i != nil && i.ManagedTools != nil && *i.ManagedTools
}

// NousPortalBillingURL is the billing page of the portal, the default portal when blank
func NousPortalBillingURL(portalBaseURL string) string {
	base := portalBaseURL
	if pyStripSpace(base) == "" {
		base = DefaultNousPortalURL
	}
	return strings.TrimRight(base, "/") + "/billing"
}

func nousAccountPortal(info *NousPortalAccountInfo) string {
	if info == nil {
		return ""
	}
	return info.PortalBaseURL
}

// NousPortalTopupURL opens the top up modal on the org pinned billing page, the plain billing page without a slug
func NousPortalTopupURL(info *NousPortalAccountInfo) string {
	base := strings.TrimSuffix(NousPortalBillingURL(nousAccountPortal(info)), "/billing")
	if info != nil && pyStripSpace(info.OrgSlug) != "" {
		return base + "/orgs/" + pyQuoteAll(pyStripSpace(info.OrgSlug)) + "/billing?topup=open"
	}
	return base + "/billing?topup=open"
}

// Percent encodes every byte outside letters, digits and underscore dot dash tilde
func pyQuoteAll(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_.-~", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// FormatUsageWindowBlockGuidance is the next step after a usage window 402, empty for accounts not on a window plan
func FormatUsageWindowBlockGuidance(info *NousPortalAccountInfo) string {
	if info == nil || !applyWindowSeedFields(info.UsageWindow, nil) {
		return ""
	}
	window := info.UsageWindow
	billing := NousPortalBillingURL(info.PortalBaseURL)
	if window.PausedReason != "" {
		fix := "Included usage is paused"
		if PausedForPayment(window.PausedReason) {
			fix = "Update your card to restore included usage"
		}
		return fmt.Sprintf("%s, or add credits to keep going now: %s", fix, billing)
	}
	micros := int64(0)
	if acc := info.PaidServiceAccessInfo; acc != nil && acc.PurchasedCreditsRemaining != nil && !math.IsNaN(*acc.PurchasedCreditsRemaining) && !math.IsInf(*acc.PurchasedCreditsRemaining, 0) {
		micros = int64(math.Trunc(*acc.PurchasedCreditsRemaining * 1_000_000))
	}
	switch CreditsAction(window.KeepGoingOnCredits, micros) {
	case UseCredits:
		return "Turn on Keep going in /topup to use your credits now: " + billing
	case AddCreditsAndKeepGoing:
		return "Add credits and turn on Keep going in /topup to keep going now: " + billing
	}
	lines := []string{"Add credits to keep going now: " + NousPortalTopupURL(info)}
	if window.AutoTopUpEnabled != nil && !*window.AutoTopUpEnabled {
		lines = append(lines, "Turn on auto top-up in the same place so this doesn't stop you next time.")
	}
	return strings.Join(lines, "\n")
}

// NousEntitlementOptions shape the entitlement copy, zero values give the default terminal message
type NousEntitlementOptions struct {
	Capability       string
	NoRefreshHint    bool
	CoverageCategory string
	InChat           bool
}

// FormatNousPortalEntitlementMessage is the missing entitlement guidance for a capability, empty when entitled
func FormatNousPortalEntitlementMessage(info *NousPortalAccountInfo, capability string, inChat bool) string {
	return FormatNousPortalEntitlementMessageWith(info, NousEntitlementOptions{Capability: capability, InChat: inChat})
}

// FormatNousPortalEntitlementMessageWith is the entitlement guidance scoped by coverage category and refresh hint
func FormatNousPortalEntitlementMessageWith(info *NousPortalAccountInfo, opts NousEntitlementOptions) string {
	capability := opts.Capability
	if capability == "" {
		capability = "this feature"
	}
	if info.IsAnonymousTier() {
		if opts.InChat {
			return FreeTierNeedsAccountChat
		}
		return FreeTierNeedsAccount
	}
	billing := NousPortalBillingURL(nousAccountPortal(info))
	if info != nil {
		if opts.CoverageCategory != "" {
			if info.ToolGatewayEntitledFor(opts.CoverageCategory) {
				return ""
			}
			if info.ToolGatewayEntitled() {
				return fmt.Sprintf("%s isn't included with your current Nous Portal access. Add credits or a subscription to enable it at %s.", capability, billing)
			}
		} else if info.ToolGatewayEntitled() {
			return ""
		}
	}
	if info == nil {
		return fmt.Sprintf("Icebath could not verify your Nous Portal entitlement, so %s is unavailable. Run `icebath model` to refresh your login, or check billing at %s.", capability, billing)
	}
	if !info.LoggedIn {
		if info.InferenceCredentialPresent {
			return fmt.Sprintf("Nous inference credentials are configured, but Icebath cannot verify your Nous Portal paid access for %s. Log in with `icebath model` to enable Portal-managed features. Billing and credits are managed at %s.", capability, billing)
		}
		return fmt.Sprintf("Log in to Nous Portal to use %s: run `icebath model`. Billing and credits are managed at %s.", capability, billing)
	}
	if info.PaidServiceAccess == nil {
		detail := fmt.Sprintf("Icebath could not verify your Nous Portal paid access, so %s is unavailable.", capability)
		if info.Error != "" {
			detail += fmt.Sprintf(" Account lookup failed: %s.", info.Error)
		}
		if !opts.NoRefreshHint {
			detail += " Run `icebath model` to refresh your session."
		}
		return detail + fmt.Sprintf(" Check billing at %s.", billing)
	}
	reason := ""
	if info.PaidServiceAccessInfo != nil {
		reason = info.PaidServiceAccessInfo.Reason
	}
	if reason == "account_missing" {
		return fmt.Sprintf("Icebath could not find a Nous Portal account or organisation for this login, so %s is unavailable. Run `icebath model` to authenticate again; if the problem persists, contact Nous support.", capability)
	}
	if reason == "no_usable_credits" || !*info.PaidServiceAccess {
		message := nousNoPaidAccessMessage(info, capability, billing, opts.InChat)
		if !opts.NoRefreshHint && !info.Fresh {
			message += " If you recently bought credits, run `icebath model` to refresh Icebath."
		}
		return message
	}
	return fmt.Sprintf("Your Nous Portal account does not currently have paid service access, so %s is unavailable. Add credits or update billing at %s.", capability, billing)
}

// Dollar amount with two decimals spelled as Python formats non finite values
func pyMoney2(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	return strconv.FormatFloat(f, 'f', 2, 64)
}

// Copy for an account without paid access naming the spend cap, subscription or credit state
func nousNoPaidAccessMessage(info *NousPortalAccountInfo, capability, billing string, inChat bool) string {
	if info.IsAnonymousTier() {
		if inChat {
			return FreeTierNeedsAccountChat
		}
		return FreeTierNeedsAccount
	}
	access := NousPaidServiceAccessInfo{}
	if info.PaidServiceAccessInfo != nil {
		access = *info.PaidServiceAccessInfo
	}
	var parts []string
	for _, l := range []struct {
		label  string
		amount *float64
	}{{"usable", access.TotalUsableCredits}, {"subscription", access.SubscriptionCreditsRemaining}, {"purchased", access.PurchasedCreditsRemaining}} {
		if l.amount != nil {
			parts = append(parts, fmt.Sprintf("%s $%s", l.label, pyMoney2(*l.amount)))
		}
	}
	creditDetail := ""
	if len(parts) > 0 {
		creditDetail = " (" + strings.Join(parts, ", ") + ")"
	}
	if access.MemberSpendCapExceeded != nil && *access.MemberSpendCapExceeded {
		capDetail := ""
		if access.MemberSpendCapUSD != nil && access.MemberSpendUSD != nil {
			capDetail = fmt.Sprintf(" Your organisation's per-member spend cap is $%s and you've spent $%s of it.", pyMoney2(*access.MemberSpendCapUSD), pyMoney2(*access.MemberSpendUSD))
		} else if access.MemberSpendCapUSD != nil {
			capDetail = fmt.Sprintf(" Your organisation's per-member spend cap is $%s.", pyMoney2(*access.MemberSpendCapUSD))
		}
		return fmt.Sprintf("Your Nous Portal access is paused because you've exceeded the per-member spend cap set by your organisation.%s%s Ask your organisation admin to raise the member spend cap at %s, then run `icebath model` to refresh.", capDetail, creditDetail, billing)
	}
	active, paid := access.HasActiveSubscription, access.ActiveSubscriptionIsPaid
	switch {
	case active != nil && *active && paid != nil && *paid:
		return fmt.Sprintf("Your Nous Portal credits are exhausted%s, so %s is unavailable. Top up or renew credits at %s.", creditDetail, capability, billing)
	case active != nil && *active && paid != nil && !*paid:
		return fmt.Sprintf("Your current Nous Portal plan does not include paid service access, so %s is unavailable. Upgrade or add credits at %s.", capability, billing)
	case active != nil && !*active:
		return fmt.Sprintf("Your Nous Portal account has no active subscription or usable credits%s, so %s is unavailable. Subscribe or add credits at %s.", creditDetail, capability, billing)
	}
	return fmt.Sprintf("Your Nous Portal account has no usable paid credits%s, so %s is unavailable. Add credits or update billing at %s.", creditDetail, capability, billing)
}

type nousAccountCacheEntry struct {
	key      string
	at       time.Time
	info     *NousPortalAccountInfo
	identity string
}

var (
	nousAccountCacheMu sync.Mutex
	nousAccountCache   = map[string]nousAccountCacheEntry{}
	nousAccountNow     = time.Now

	nousAccountAuthStateFn = func(env PoolEnv) (map[string]any, error) { return env.Store.GetProviderAuthState("nous") }
	nousAccountTokenFn     = func(env PoolEnv) (string, error) { return resolveNousAccessToken(env, NousAccessTokenOptions{}) }
	nousAccountLoadPoolFn  = func(env PoolEnv) *Pool { return LoadPool(env, "nous") }
	fetchNousAccountInfoFn = fetchNousAccountInfo
)

// ResetNousPortalAccountInfoCache clears every home's account snapshot
func ResetNousPortalAccountInfoCache() {
	nousAccountCacheMu.Lock()
	nousAccountCache = map[string]nousAccountCacheEntry{}
	nousAccountCacheMu.Unlock()
}

// Cache slot of the home the env reads its auth store from
func nousAccountHomeKey(env PoolEnv) string {
	if env.Store != nil {
		return home.Key(env.Store.Home())
	}
	return home.Key(home.Dir(env.ctx()))
}

// GetNousPortalAccountInfo is the account entitlement from a valid JWT or the account API, force fresh always calls the API
func GetNousPortalAccountInfo(ctx context.Context, forceFresh bool) *NousPortalAccountInfo {
	return getNousPortalAccountInfoIn(modelsPoolEnv(ctx), forceFresh, 60)
}

// Account entitlement for the env's home, a JWT needing at least minJWTTTL seconds left to serve as a snapshot
func getNousPortalAccountInfoIn(env PoolEnv, forceFresh bool, minJWTTTL int) *NousPortalAccountInfo {
	state, err := nousAccountAuthStateFn(env)
	if err != nil {
		return nousAccountErrorInfo(err.Error(), false, "", nil, "")
	}
	if state == nil {
		state = map[string]any{}
	}
	accessToken := ""
	if !guestWithoutFreeTierIn(env, state) {
		accessToken = heldConnectorsTokenFor(state, 0, false)
	}
	portal := nousAccountPortalBaseURL(env, state)
	if pyStripSpace(accessToken) == "" {
		if info := nousAccountFromOAuthPool(env, forceFresh, minJWTTTL, portal); info != nil {
			return info
		}
		if info := nousAccountFromInferenceKeyPool(env, portal); info != nil {
			return info
		}
		return &NousPortalAccountInfo{Source: "none", PortalBaseURL: portal}
	}
	if !forceFresh {
		if info := nousAccountFromValidJWT(accessToken, state, portal, minJWTTTL); info != nil {
			return info
		}
	}
	return nousFreshAccountInfo(env, state, forceFresh, portal)
}

// nousPortalAccountIsFreeTier reports an account known to lack paid access
func nousPortalAccountIsFreeTier(ctx context.Context, forceFresh bool) (bool, error) {
	return GetNousPortalAccountInfo(ctx, forceFresh).IsFreeTier(), nil
}

// Entitlement notice for the capability from fresh account data, empty when entitled
func nousPortalEntitlementMessage(env PoolEnv, capability string) (string, error) {
	return FormatNousPortalEntitlementMessage(getNousPortalAccountInfoIn(env, true, 60), capability, false), nil
}

// Org restrictive policy claim of the stored login, known false when the claim is absent or unreadable
func nousPolicyPresent(ctx context.Context) (present, known bool) {
	state, err := nousAccountAuthStateFn(modelsPoolEnv(ctx))
	if err != nil {
		return false, false
	}
	token, _ := state["access_token"].(string)
	if pyStripSpace(token) == "" {
		return false, false
	}
	claims := DecodeJWTClaims(token)
	if len(claims) == 0 {
		return false, false
	}
	flag := nousCoerceBool(claims["policy_present"])
	if flag == nil {
		return false, false
	}
	return *flag, true
}

// One line notice for a list the org policy narrowed, empty when nothing was removed or no policy is known
func nousPolicyNotice(env PoolEnv, removed bool) string {
	if !removed {
		return ""
	}
	if present, known := nousPolicyPresentFn(env.ctx()); !known || !present {
		return ""
	}
	return nousPolicyNoticeText
}

// CachedNousPortalAccountInfo is the cached snapshot of the current login, past the TTL only with anyAge, never a fetch
func CachedNousPortalAccountInfo(ctx context.Context, anyAge bool) *NousPortalAccountInfo {
	env := modelsPoolEnv(ctx)
	nousAccountCacheMu.Lock()
	entry, ok := nousAccountCache[nousAccountHomeKey(env)]
	nousAccountCacheMu.Unlock()
	if !ok || entry.info == nil {
		return nil
	}
	if !anyAge && nousAccountNow().Sub(entry.at) >= nousAccountInfoCacheTTL {
		return nil
	}
	current, ok := nousCurrentAccountIdentity(env)
	if entry.identity == "" || !ok || entry.identity != current {
		return nil
	}
	return entry.info
}

// PeekNousAccountInfoCache is the raw cached snapshot of the home, without an age or identity check
func PeekNousAccountInfoCache(ctx context.Context) *NousPortalAccountInfo {
	nousAccountCacheMu.Lock()
	defer nousAccountCacheMu.Unlock()
	return nousAccountCache[nousAccountHomeKey(modelsPoolEnv(ctx))].info
}

// Portal plus the token's user and org, an opaque token identified by its hash
func nousAccountIdentity(accessToken, portal string) string {
	claims := DecodeJWTClaims(accessToken)
	sub, _ := claims["sub"].(string)
	org, _ := claims["org_id"].(string)
	identity := ""
	if pyStripSpace(sub) != "" && pyStripSpace(org) != "" {
		identity = "jwt:" + sub + ":" + org
	} else {
		sum := sha256.Sum256([]byte(accessToken))
		identity = hex.EncodeToString(sum[:])
	}
	return portal + ":" + identity
}

// Identity of the persisted login without a refresh or network call, false when logged out
func nousCurrentAccountIdentity(env PoolEnv) (string, bool) {
	state, err := nousAccountAuthStateFn(env)
	if err != nil {
		return "", false
	}
	token, _ := state["access_token"].(string)
	if pyStripSpace(token) == "" {
		return "", false
	}
	return nousAccountIdentity(token, nousAccountPortalBaseURL(env, state)), true
}

// Account API snapshot of the refreshed login, served from the cache unless forced or expired
func nousFreshAccountInfo(env PoolEnv, state map[string]any, forceFresh bool, portal string) *NousPortalAccountInfo {
	fail := func(err error) *NousPortalAccountInfo {
		return nousAccountErrorInfo(err.Error(), pyTruthy(state["access_token"]), portal, nil, nousCoerceStr(state["account_tier"]))
	}
	accessToken, err := nousAccountTokenFn(env)
	if err != nil {
		return fail(err)
	}
	refreshed, err := nousAccountAuthStateFn(env)
	if err != nil {
		return fail(err)
	}
	if len(refreshed) == 0 {
		refreshed = state
	}
	if p := nousAccountPortalBaseURL(env, refreshed); p != "" {
		portal = p
	}
	sum := sha256.Sum256([]byte(accessToken))
	cacheKey := portal + ":" + hex.EncodeToString(sum[:])
	slot := nousAccountHomeKey(env)
	if !forceFresh {
		nousAccountCacheMu.Lock()
		entry, ok := nousAccountCache[slot]
		nousAccountCacheMu.Unlock()
		if ok && entry.key == cacheKey && nousAccountNow().Sub(entry.at) < nousAccountInfoCacheTTL {
			return entry.info
		}
	}
	info, err := nousAccountFromFetched(env, accessToken, refreshed, portal)
	if err != nil {
		return fail(err)
	}
	if info.Source != "error" {
		identity := nousAccountIdentity(accessToken, portal)
		nousAccountCacheMu.Lock()
		nousAccountCache[slot] = nousAccountCacheEntry{key: cacheKey, at: nousAccountNow(), info: info, identity: identity}
		nousAccountCacheMu.Unlock()
	}
	return info
}

// Unknown entitlement snapshot for an opaque Nous inference key in the pool
func nousAccountFromInferenceKeyPool(env PoolEnv, portal string) *NousPortalAccountInfo {
	entry := nousSelectPoolEntry(env)
	if entry == nil {
		return nil
	}
	key := entry.RuntimeAPIKey()
	if key == "" {
		key = entry.AccessToken
	}
	if pyStripSpace(key) == "" {
		return nil
	}
	if p := entry.ExtraString("portal_base_url"); p != "" {
		portal = p
	}
	return &NousPortalAccountInfo{Source: "inference_key", PortalBaseURL: portal, InferenceBaseURL: nousPoolEntryInferenceURL(*entry),
		InferenceCredentialPresent: true, CredentialSource: "pool:" + entry.Label, Error: "portal_oauth_missing"}
}

// Account snapshot of a portal OAuth pool row, its JWT first unless forced, then the account API
func nousAccountFromOAuthPool(env PoolEnv, forceFresh bool, minJWTTTL int, portal string) *NousPortalAccountInfo {
	entry := nousSelectPoolEntry(env)
	if entry == nil || !nousPoolEntryIsPortalOAuth(*entry) {
		return nil
	}
	entryPortal := portal
	if p := entry.ExtraString("portal_base_url"); p != "" {
		entryPortal = p
	}
	state := map[string]any{"access_token": entry.AccessToken, "client_id": entry.Extra["client_id"],
		"inference_base_url": nousPoolEntryInferenceURL(*entry), "agent_key": entry.AgentKey, "credential_source": "pool:" + entry.Label}
	if !forceFresh {
		if info := nousAccountFromValidJWT(entry.AccessToken, state, entryPortal, minJWTTTL); info != nil {
			return info
		}
	}
	info, err := nousAccountFromFetched(env, entry.AccessToken, state, entryPortal)
	if err != nil {
		return nousAccountErrorInfo(err.Error(), true, entryPortal, nil, "")
	}
	return info
}

// Account API call normalized, an empty or error payload becoming an error snapshot
func nousAccountFromFetched(env PoolEnv, accessToken string, state map[string]any, portal string) (*NousPortalAccountInfo, error) {
	payload, err := fetchNousAccountInfoFn(env, accessToken, portal)
	if err != nil {
		return nil, err
	}
	tier := nousCoerceStr(state["account_tier"])
	if len(payload) == 0 {
		return nousAccountErrorInfo("empty_account_response", true, portal, nil, tier), nil
	}
	if text, ok := payload["error"].(string); ok {
		if text == "" {
			text = "account_response_error"
		}
		return nousAccountErrorInfo(text, true, portal, payload, tier), nil
	}
	return nousAccountFromPayload(payload, state, portal)
}

func nousPoolEntryInferenceURL(entry PooledCredential) string {
	if entry.InferenceBaseURL != "" {
		return entry.InferenceBaseURL
	}
	if u := entry.RuntimeBaseURL(); u != "" {
		return u
	}
	return entry.BaseURL
}

// Pool row with the latest agent key expiry, then access expiry, then lowest priority
func nousSelectPoolEntry(env PoolEnv) *PooledCredential {
	pool := nousAccountLoadPoolFn(env)
	if pool == nil || !pool.HasCredentials() {
		return nil
	}
	entries := pool.Entries()
	if len(entries) == 0 {
		return nil
	}
	key := func(e PooledCredential) [3]float64 {
		return [3]float64{nousAccountISOTimestamp(e.AgentKeyExpiresAt), nousAccountISOTimestamp(e.ExpiresAt), -float64(e.Priority)}
	}
	best, bestKey := 0, key(entries[0])
	for i := 1; i < len(entries); i++ {
		k := key(entries[i])
		if k[0] > bestKey[0] || k[0] == bestKey[0] && (k[1] > bestKey[1] || k[1] == bestKey[1] && k[2] > bestKey[2]) {
			best, bestKey = i, k
		}
	}
	return &entries[best]
}

func nousPoolEntryIsPortalOAuth(entry PooledCredential) bool {
	if pyStripSpace(entry.AccessToken) == "" {
		return false
	}
	return strings.HasPrefix(strings.ToLower(pyStripSpace(entry.AuthType)), "oauth") || entry.RefreshToken != ""
}

// GETs the portal account endpoint with the bearer, a non object body reading as empty
func fetchNousAccountInfo(env PoolEnv, accessToken, portal string) (map[string]any, error) {
	base := portal
	if base == "" {
		base = DefaultNousPortalURL
	}
	req, err := http.NewRequestWithContext(env.ctx(), http.MethodGet, strings.TrimRight(base, "/")+"/api/oauth/account", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "icebath-agent")
	client := &http.Client{Timeout: 8 * time.Second}
	if env.HTTP != nil {
		c := *env.HTTP
		c.Timeout = 8 * time.Second
		client = &c
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)+" "))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(body) {
		return nil, fmt.Errorf("'utf-8' codec can't decode the account response")
	}
	payload, err := decodeExactJSON(string(body))
	if err != nil {
		return nil, err
	}
	obj, _ := payload.(map[string]any)
	if obj == nil {
		return map[string]any{}, nil
	}
	return obj, nil
}

// Local snapshot from an OAuth JWT with more than minJWTTTL seconds left, nil otherwise
func nousAccountFromValidJWT(token string, state map[string]any, portal string, minJWTTTL int) *NousPortalAccountInfo {
	claims := DecodeJWTClaims(token)
	if len(claims) == 0 {
		return nil
	}
	exp, err := nousCoerceFloat(claims["exp"])
	if err != nil || exp == nil || *exp <= float64(nousAccountNow().UnixNano())/1e9+float64(max(0, minJWTTTL)) {
		return nil
	}
	tier, _ := nousCoerceInt(claims["subscription_tier"])
	paid := nousCoerceBool(claims["paid_access"])
	sec, frac := math.Modf(*exp)
	expiresAt := time.Unix(int64(sec), int64(math.Round(frac*1e6))*1000).UTC()
	credentialSource := nousCoerceStr(state["credential_source"])
	if credentialSource == "" {
		credentialSource = "auth_store"
	}
	accountTier := nousCoerceStr(claims["account_tier"])
	if accountTier == "" {
		accountTier = nousCoerceStr(state["account_tier"])
	}
	clientID := claims["client_id"]
	if !nousTruthy(clientID) {
		clientID = state["client_id"]
	}
	raw := make(map[string]any, len(claims))
	for k, v := range claims {
		raw[k] = v
	}
	return &NousPortalAccountInfo{
		LoggedIn: true, Source: "jwt",
		UserID: nousCoerceStr(claims["sub"]), OrgID: nousCoerceStr(claims["org_id"]), ClientID: nousCoerceStr(clientID),
		ProductID: nousCoerceStr(claims["product_id"]), NousClient: nousCoerceStr(claims["nous_client"]),
		PortalBaseURL: portal, InferenceBaseURL: nousCoerceStr(state["inference_base_url"]), InferenceCredentialPresent: true,
		CredentialSource: credentialSource, ExpiresAt: &expiresAt, PaidServiceAccess: paid,
		PaidServiceAccessInfo: &NousPaidServiceAccessInfo{Allowed: paid, PaidAccess: paid, OrganisationID: nousCoerceStr(claims["org_id"]), SubscriptionTier: tier},
		ToolAccess:            nousToolAccessFromValue(claims["tool_access"]), RawClaims: raw, AccountTier: accountTier,
		ManagedTools: nousCoerceBool(claims["managed_tools"]),
	}
}

// Normalized account API payload, a numeric overflow in a coerced field returned as the error
func nousAccountFromPayload(payload map[string]any, state map[string]any, portal string) (*NousPortalAccountInfo, error) {
	user, _ := payload["user"].(map[string]any)
	org, _ := payload["organisation"].(map[string]any)
	access, err := nousCoercePaidServiceAccess(payload["paid_service_access"])
	if err != nil {
		return nil, err
	}
	subscription, err := nousCoerceSubscription(payload["subscription"])
	if err != nil {
		return nil, err
	}
	var paid *bool
	orgID := nousCoerceStr(org["id"])
	if access != nil {
		paid = access.Allowed
		if paid == nil {
			paid = access.PaidAccess
		}
		if orgID == "" {
			orgID = access.OrganisationID
		}
	}
	credentialSource := nousCoerceStr(state["credential_source"])
	if credentialSource == "" {
		credentialSource = "auth_store"
	}
	accountTier := nousCoerceStr(payload["account_tier"])
	for _, alt := range []any{user["account_tier"], state["account_tier"]} {
		if accountTier == "" {
			accountTier = nousCoerceStr(alt)
		}
	}
	raw := make(map[string]any, len(payload))
	for k, v := range payload {
		raw[k] = v
	}
	return &NousPortalAccountInfo{
		LoggedIn: true, Source: "account_api", Fresh: true,
		OrgID: orgID, OrgSlug: nousCoerceStr(org["slug"]), OrgName: nousCoerceStr(org["name"]),
		ClientID: nousCoerceStr(state["client_id"]), PortalBaseURL: portal, InferenceBaseURL: nousCoerceStr(state["inference_base_url"]),
		InferenceCredentialPresent: nousTruthy(state["access_token"]) || nousTruthy(state["agent_key"]),
		CredentialSource:           credentialSource, Email: nousCoerceStr(user["email"]), PrivyDID: nousCoerceStr(user["privy_did"]),
		Subscription: subscription, PaidServiceAccess: paid, PaidServiceAccessInfo: access,
		ToolAccess: nousToolAccessFromValue(payload["tool_access"]), UsageWindow: nousCoerceUsageWindow(payload["usage_window"]),
		RawAccount: raw, AccountTier: accountTier, ManagedTools: nousCoerceBool(payload["managed_tools"]),
	}, nil
}

// Tool pool entitlement of an object, only literal true counting for enabled and each coverage entry
func nousToolAccessFromValue(value any) *NousToolAccessInfo {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	coverage := map[string]bool{}
	if cov, ok := obj["coverage"].(map[string]any); ok {
		for k, v := range cov {
			b, _ := v.(bool)
			coverage[k] = b
		}
	}
	enabled, _ := obj["enabled"].(bool)
	return &NousToolAccessInfo{Enabled: enabled, Coverage: coverage}
}

func nousCoercePaidServiceAccess(value any) (*NousPaidServiceAccessInfo, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, nil
	}
	out := &NousPaidServiceAccessInfo{Allowed: nousCoerceBool(obj["allowed"]), PaidAccess: nousCoerceBool(obj["paid_access"]),
		Reason: nousCoerceStr(obj["reason"]), OrganisationID: nousCoerceStr(obj["organisation_id"]),
		HasActiveSubscription: nousCoerceBool(obj["has_active_subscription"]), ActiveSubscriptionIsPaid: nousCoerceBool(obj["active_subscription_is_paid"]),
		MemberSpendCapExceeded: nousCoerceBool(obj["member_spend_cap_exceeded"])}
	effective, err := nousCoerceInt(obj["effective_at_ms"])
	if err != nil {
		return nil, err
	}
	out.EffectiveAtMs = effective
	tier, err := nousCoerceInt(obj["subscription_tier"])
	if err != nil {
		return nil, err
	}
	out.SubscriptionTier = tier
	for _, f := range []struct {
		key string
		dst **float64
	}{{"subscription_monthly_charge", &out.SubscriptionMonthlyCharge}, {"subscription_credits_remaining", &out.SubscriptionCreditsRemaining},
		{"purchased_credits_remaining", &out.PurchasedCreditsRemaining}, {"total_usable_credits", &out.TotalUsableCredits},
		{"member_spend_cap_usd", &out.MemberSpendCapUSD}, {"member_spend_usd", &out.MemberSpendUSD},
		{"member_spend_cap_remaining_usd", &out.MemberSpendCapRemainingUSD}} {
		if *f.dst, err = nousCoerceFloat(obj[f.key]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func nousCoerceSubscription(value any) (*NousPortalSubscriptionInfo, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, nil
	}
	out := &NousPortalSubscriptionInfo{Plan: nousCoerceStrPtr(obj["plan"]), CurrentPeriodEnd: nousCoerceStrPtr(obj["current_period_end"])}
	tier, err := nousCoerceInt(obj["tier"])
	if err != nil {
		return nil, err
	}
	out.Tier = tier
	for _, f := range []struct {
		key string
		dst **float64
	}{{"monthly_charge", &out.MonthlyCharge}, {"monthly_credits", &out.MonthlyCredits}, {"credits_remaining", &out.CreditsRemaining}, {"rollover_credits", &out.RolloverCredits}} {
		if *f.dst, err = nousCoerceFloat(obj[f.key]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func nousCoerceUsageWindow(value any) *NousUsageWindowInfo {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return &NousUsageWindowInfo{Gauges: ParseGauges(obj["gauges"]), FeaturedModelIDs: nousCoerceStrList(obj["featured_model_ids"]),
		AutoTopUpEnabled: nousCoerceBool(obj["auto_top_up_enabled"]), PausedReason: nousCoerceStr(obj["paused_reason"]),
		KeepGoingOnCredits: nousCoerceBool(obj["keep_going_on_credits"])}
}

func nousAccountErrorInfo(errText string, loggedIn bool, portal string, raw map[string]any, tier string) *NousPortalAccountInfo {
	return &NousPortalAccountInfo{LoggedIn: loggedIn, Source: "error", PortalBaseURL: portal, RawAccount: raw, Error: errText, AccountTier: tier}
}

// ResolveNousPortalBaseURL is the portal the stored login talks to, the operator override first
func ResolveNousPortalBaseURL(ctx context.Context) (string, error) {
	env := modelsPoolEnv(ctx)
	state, err := nousAccountAuthStateFn(env)
	if err != nil {
		return "", err
	}
	return nousPortalBaseURLIn(env, state), nil
}

// Operator override or stored portal URL trimmed of trailing slashes, empty when blank
func nousAccountPortalBaseURL(env PoolEnv, state map[string]any) string {
	value := nousPortalEnvOverride(env)
	if value == "" {
		value, _ = state["portal_base_url"].(string)
	}
	if pyStripSpace(value) == "" {
		return ""
	}
	return strings.TrimRight(pyStripSpace(value), "/")
}

var nousNaiveISORe = regexp.MustCompile(`(?:Z|[+-]\d{2}(?::?\d{2}(?::?\d{2}(?:\.\d+)?)?)?)$`)

// Epoch seconds of an ISO timestamp, naive values read as local time, zero when unparseable
func nousAccountISOTimestamp(value string) float64 {
	text := pyStripSpace(value)
	if text == "" {
		return 0
	}
	t, ok := parseISO(text)
	if !ok {
		return 0
	}
	if len(text) > 10 && !nousNaiveISORe.MatchString(text[10:]) {
		t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
	} else if len(text) <= 10 {
		t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
	}
	return float64(t.UnixNano()) / 1e9
}

// Python truthiness that also reads decoded JSON numbers
func nousTruthy(v any) bool {
	if n, ok := v.(json.Number); ok {
		f, err := n.Float64()
		return err != nil || f != 0
	}
	return pyTruthy(v)
}

func nousCoerceStr(v any) string {
	s, _ := v.(string)
	return s
}

func nousCoerceStrPtr(v any) *string {
	if s, ok := v.(string); ok && s != "" {
		return &s
	}
	return nil
}

func nousCoerceBool(v any) *bool {
	if b, ok := v.(bool); ok {
		return &b
	}
	return nil
}

// Non empty strings of a JSON array in order, nil for anything but an array
func nousCoerceStrList(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := []string{}
	for _, item := range items {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

var pyFloatTextRe = regexp.MustCompile(`(?i)^[+-]?(?:inf|infinity|nan|(?:\d(?:_?\d)*(?:\.(?:\d(?:_?\d)*)?)?|\.\d(?:_?\d)*)(?:e[+-]?\d(?:_?\d)*)?)$`)

// Python float of a string, false when float would raise ValueError
func nousFloatText(s string) (float64, bool) {
	t := pyStripSpace(s)
	if !pyFloatTextRe.MatchString(t) {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(t, "_", ""), 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return f, true
		}
		return 0, false
	}
	return f, true
}

var errNousIntTooLarge = fmt.Errorf("int too large to convert to float")

// Python float of a payload value, nil for None bool and unconvertible values, an error on overflow
func nousCoerceFloat(v any) (*float64, error) {
	var f float64
	switch n := v.(type) {
	case json.Number:
		text := n.String()
		parsed, err := strconv.ParseFloat(text, 64)
		if err != nil {
			if !strings.ContainsAny(text, ".eE") {
				return nil, errNousIntTooLarge
			}
			if ne, ok := err.(*strconv.NumError); !ok || ne.Err != strconv.ErrRange {
				return nil, nil
			}
		}
		f = parsed
	case float64:
		f = n
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case string:
		parsed, ok := nousFloatText(n)
		if !ok {
			return nil, nil
		}
		f = parsed
	default:
		return nil, nil
	}
	return &f, nil
}

var errNousFloatToInt = fmt.Errorf("cannot convert float infinity to integer")

// Python int of a payload value truncating floats, nil for None bool and unconvertible values, an error for infinity
func nousCoerceInt(v any) (*int64, error) {
	var f float64
	switch n := v.(type) {
	case json.Number:
		text := n.String()
		if !strings.ContainsAny(text, ".eE") {
			i, err := strconv.ParseInt(text, 10, 64)
			if err != nil {
				return nil, nil
			}
			return &i, nil
		}
		parsed, err := strconv.ParseFloat(text, 64)
		if err != nil {
			if ne, ok := err.(*strconv.NumError); !ok || ne.Err != strconv.ErrRange {
				return nil, nil
			}
		}
		f = parsed
	case float64:
		f = n
	case int:
		i := int64(n)
		return &i, nil
	case int64:
		return &n, nil
	case string:
		i, ok := pyIntText(n)
		if !ok {
			return nil, nil
		}
		return &i, nil
	default:
		return nil, nil
	}
	if math.IsInf(f, 0) {
		return nil, errNousFloatToInt
	}
	if math.IsNaN(f) || math.Abs(f) >= 1<<63 {
		return nil, nil
	}
	i := int64(math.Trunc(f))
	return &i, nil
}
