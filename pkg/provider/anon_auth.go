package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	AnonAuthMethod           = "anonymous"
	AnonClientID             = "nas-anonymous"
	AnonSecretHeader         = "x-anonymous-api-secret"
	AnonSecretEnv            = "ICEBATH_ANON_API_SECRET"
	PreviewFullConnectorsEnv = "ICEBATH_PREVIEW_FULL_CONNECTORS"
	GuestMintTimeout         = 5 * time.Second
	FreeTierLabel            = "Nous · free tier"
	UpgradeHint              = "Run `icebath auth upgrade` to sign in with a Nous account, or /login inside a chat."
	FreeTierNotSignedIn      = "You're not signed in. Free inference and connectors are always on. Run `icebath auth` to sign in with a Nous account."
	GuestNoticeFlag          = "guest_notice_shown"
	UpgradedAuthMethod       = "oauth_device_code"
)

const (
	AnonGateClosed        = "anon_gate_closed"
	AnonGatePaused        = "anon_gate_paused"
	AnonRateLimited       = "anon_rate_limited"
	AnonPowRequired       = "anon_pow_required"
	AnonAccountLocked     = "anon_account_locked"
	AnonCredentialDead    = "anon_credential_dead"
	AnonUnreachable       = "anon_unreachable"
	AnonServerError       = "anon_server_error"
	AnonChallengeRequired = "anon_challenge_required"
	AnonSigninRequired    = "anon_signin_required"
)

// Codes a later attempt cannot fix for this process and version
var AnonTerminalCodes = setOf(AnonGateClosed, AnonPowRequired, AnonAccountLocked, AnonSigninRequired)

// Codes meaning the account service itself is not answering so a sign in cannot help either
var AnonUnreachableCodes = setOf(AnonUnreachable, AnonServerError)

const anonSigninIsFree = "Signing in is free."

// User copy per free tier failure code, what happened then the one honest way forward
var AnonFailureCopy = map[string]string{
	AnonGateClosed:        "This version can't be used without a Nous account. " + anonSigninIsFree,
	AnonGatePaused:        "Using Icebath without signing in is paused for a moment. " + anonSigninIsFree,
	AnonRateLimited:       "Lots of people are getting started right now. Try again in {wait}. Signing in is free and skips the wait.",
	AnonPowRequired:       "The Nous server asked for a proof of work, but that isn't implemented in your Agent yet. Sign in with a Nous account to continue.",
	AnonChallengeRequired: "Finish the quick check in your browser, then try again.",
	AnonSigninRequired:    "Free guest access isn't available here. Sign in with a Nous account to continue.",
	AnonAccountLocked:     "This session can't continue without signing in. " + anonSigninIsFree,
	AnonCredentialDead:    "Your session ended. A new one starts on its own.",
	AnonUnreachable:       "The Nous service couldn't be reached. Check your internet connection and try again.",
	AnonServerError:       "The Nous service had a hiccup. Try again in a moment.",
}

// AnonCredentialDeadError means the account service no longer knows the anon credential, reaped or claimed
type AnonCredentialDeadError struct{ Auth *AuthError }

func (e *AnonCredentialDeadError) Error() string { return e.Auth.Message }

func (e *AnonCredentialDeadError) Unwrap() error { return e.Auth }

// Free tier failure error whose terminal flag follows the code
func anonErr(message, code string, retryAfter time.Duration, hasRetryAfter bool) *AuthError {
	retryable := !AnonTerminalCodes[code]
	return &AuthError{Message: message, Code: code, RetryAfter: retryAfter, HasRetryAfter: hasRetryAfter, Retryable: &retryable}
}

// Spoken duration for user copy from a few seconds up to hours, never a raw second count
func friendlyWaitSeconds(seconds float64) string {
	if seconds < 0 || math.IsNaN(seconds) {
		seconds = 0
	}
	if seconds <= 15 {
		return "a few seconds"
	}
	if seconds < 90 {
		return "about a minute"
	}
	if seconds < 3600 {
		return fmt.Sprintf("about %d minutes", int(math.RoundToEven(seconds/60)))
	}
	hours := int(math.RoundToEven(seconds / 3600))
	if hours <= 1 {
		return "about an hour"
	}
	return fmt.Sprintf("about %d hours", hours)
}

// Surface agnostic sentence for a free tier failure code with the wait filled in
func anonFailureCopy(code string, retryAfter time.Duration, hasRetryAfter bool) string {
	template, ok := AnonFailureCopy[code]
	if !ok || template == "" {
		template = AnonFailureCopy[AnonServerError]
	}
	wait := 60.0
	if hasRetryAfter && retryAfter > 0 {
		wait = retryAfter.Seconds()
	}
	return strings.ReplaceAll(template, "{wait}", friendlyWaitSeconds(wait))
}

// Read only config for the free tier readers, an error when config.yaml exists but cannot be read
func anonConfigReadonly(env PoolEnv) (map[string]any, error) {
	if FrozenRuntimeConfig(env.ctx()) == nil {
		path := home.ConfigPath(envHome(env))
		if _, err := os.Stat(path); err == nil {
			if _, err := config.ReadUser(envHome(env)); err != nil {
				return nil, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	cfg := env.config()
	if cfg == nil {
		cfg = LoadConfigSafe(env.ctx(), envHome(env))
	}
	return cfg, nil
}

// True unless nous.guest switched guests off, false when the config cannot be read
func guestAllowed(env PoolEnv) bool {
	cfg, err := anonConfigReadonly(env)
	if err != nil {
		env.log().Debug("guest: config unreadable, treating nous.guest as false", "err", err)
		return false
	}
	nousCfg, ok := cfg["nous"].(map[string]any)
	if !ok {
		return true
	}
	value, present := nousCfg["guest"]
	if !present {
		return true
	}
	return pyTruthy(value)
}

// Nous state reader of every caller, replaced by a case as the hermes cases patch current_nous_state
var currentNousState = readCurrentNousState

// The profile's providers.nous state without locking or network, nil when the store is unreadable
func readCurrentNousState(env PoolEnv) map[string]any {
	data, err := env.Store.Load()
	if err != nil {
		env.log().Debug(fmt.Sprintf("guest: auth store unreadable: %v", err))
		return nil
	}
	return env.Store.ProviderState(data, "nous")
}

// The profile holds a guest identity
func hasGuest(env PoolEnv) bool { return isGuestState(currentNousState(env)) }

// HasGuest reports whether the stored Nous state is a guest session
func HasGuest(env PoolEnv) bool { return hasGuest(env) }

// The activity is work for the Desktop app on this machine
func onLocalDesktopBackend(env PoolEnv) bool { return tools.ServesLocalDesktopNow(env.ctx()) }

// A guest stamped with the welcome trial claim while guests stay allowed carries inference
func freeInferenceActive(env PoolEnv, state map[string]any) bool {
	return isGuestState(state) && state[WelcomeClaimFlag] == true && guestAllowed(env)
}

// The profile state's free inference flag, read from the store
func profileFreeInferenceActive(env PoolEnv) bool {
	return freeInferenceActive(env, currentNousState(env))
}

// Last rung of model resolution, nous/welcome while the welcome trial carries inference else empty
func trialModel(env PoolEnv) string {
	if profileFreeInferenceActive(env) {
		return GuestModel
	}
	return ""
}

// Guest trial model while the home's Nous identity carries free inference, else empty
func TrialModel(ctx context.Context, h string) string {
	return trialModel(modelsPoolEnv(ctxForHome(ctx, h)))
}

// The profile's Nous identity is a guest that carries inference here
func hasFreeTierAccount(env PoolEnv) bool { return profileFreeInferenceActive(env) }

// HasFreeTierAccount reports whether the signed in Nous account has free inference active
func HasFreeTierAccount(env PoolEnv) bool { return hasFreeTierAccount(env) }

// A guest that serves connectors and managed tools only, so inference readers treat it as signed out
func guestWithoutFreeTier(state map[string]any) bool {
	return guestWithoutFreeTierIn(nousProcessEnv(), state)
}

// Guest without the free tier, the opt out read from the env's config
func guestWithoutFreeTierIn(env PoolEnv, state map[string]any) bool {
	return isGuestState(state) && !freeInferenceActive(env, state)
}

// Profile copy telling a connectors only guest that it is not signed in
func freeTierNotSignedIn() string { return FreeTierNotSignedIn }

// The free tier carries inference and the auto resolver lands on Nous
func freeTierRoute(env PoolEnv) bool {
	if !hasFreeTierAccount(env) {
		return false
	}
	resolved, err := resolveProvider(env, "auto")
	return err == nil && resolved == "nous"
}

// Model at agent start, nous/welcome on the Nous welcome host and the caller's model elsewhere
func PinModelForRoute(provider, baseURL, model string) string {
	if provider == "nous" && RouteIsWelcomeHost(baseURL) {
		if model != "" && model != GuestModel {
			slog.Info(fmt.Sprintf("Nous free tier: using %s instead of configured model %s", GuestModel, model))
		}
		return GuestModel
	}
	return model
}

// A credential rotation may land here, the welcome host serving only nous/welcome conversations
func routeCanServeModel(provider, baseURL, model string) bool {
	if provider != "nous" || !RouteIsWelcomeHost(baseURL) {
		return true
	}
	return model == "" || model == GuestModel
}

// RouteCanServeModel reports whether the route may carry the model, false only for a welcome host off the guest model
func RouteCanServeModel(provider, baseURL, model string) bool {
	return routeCanServeModel(provider, baseURL, model)
}

// The request went to the free tier host on a free tier credential
func OnFreeModel(provider, apiKey, baseURL string) bool {
	return RouteIsWelcomeHost(baseURL) && IsAnonymousRequest(provider, apiKey)
}

// Shared secret gating the anonymous surface, read from the process environment only
func anonSecret() string { return strings.TrimSpace(os.Getenv(AnonSecretEnv)) }

// Headers for every anonymous surface call, the shared secret only when set
func anonHeaders(env PoolEnv) map[string]string {
	headers := map[string]string{"content-type": "application/json", "user-agent": anonUserAgent(env)}
	if secret := anonSecret(); secret != "" {
		headers[AnonSecretHeader] = secret
	}
	return headers
}

type anonRefusalKey struct {
	status int
	err    string
	any    bool
}

type anonRefusal struct {
	dead bool
	code string
}

// Account service status and error pairs to verdicts, a wildcard entry matching any error string
var anonNASRefusals = map[anonRefusalKey]anonRefusal{
	{404, "unknown_token", false}:               {true, AnonCredentialDead},
	{404, "", true}:                             {false, AnonGateClosed},
	{401, "invalid_shared_secret", false}:       {false, AnonGateClosed},
	{401, "", true}:                             {true, AnonCredentialDead},
	{403, "account_locked", false}:              {true, AnonAccountLocked},
	{403, "anonymous_accounts_disabled", false}: {false, AnonGatePaused},
	{403, "circuit_open", false}:                {false, AnonGatePaused},
	{428, "", true}:                             {false, AnonPowRequired},
	{429, "", true}:                             {false, AnonRateLimited},
	{503, "temporarily_disabled", false}:        {false, AnonGatePaused},
}

type anonVerdict func(payload map[string]any) error

// One fully read anonymous surface response
type anonResponse struct {
	Status  int
	Header  http.Header
	Payload map[string]any
}

// Posts JSON to an anonymous surface path with the anonymous headers and reads the JSON object back
func anonPost(env PoolEnv, client *http.Client, target string, body any) (*anonResponse, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(env.ctx(), http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	for k, v := range anonHeaders(env) {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	out := &anonResponse{Status: resp.StatusCode, Header: resp.Header, Payload: map[string]any{}}
	var decoded any
	if json.Unmarshal(data, &decoded) == nil {
		if m, ok := decoded.(map[string]any); ok {
			out.Payload = m
		}
	}
	return out, nil
}

// Returns the success payload or the coded refusal, endpoint overrides winning over the shared table
func raiseForAnonStatus(env PoolEnv, resp *anonResponse, action string, overrides map[anonRefusalKey]anonVerdict) (map[string]any, error) {
	payload := resp.Payload
	errName := ""
	if pyTruthy(payload["error"]) {
		errName = fmt.Sprint(payload["error"])
	}
	status := resp.Status
	if status == 200 || status == 201 {
		return payload, nil
	}
	if strings.HasPrefix(errName, "pow_") {
		errName = "pow_"
	}
	if overrides != nil {
		build, ok := overrides[anonRefusalKey{status, errName, false}]
		if !ok {
			build, ok = overrides[anonRefusalKey{status, "", true}]
		}
		if ok {
			return nil, build(payload)
		}
	}
	verdict, ok := anonNASRefusals[anonRefusalKey{status, errName, false}]
	if !ok {
		verdict, ok = anonNASRefusals[anonRefusalKey{status, "", true}]
	}
	if !ok {
		if errName == "pow_" {
			verdict = anonRefusal{false, AnonPowRequired}
		} else {
			verdict = anonRefusal{false, AnonServerError}
		}
	}
	if verdict.code == AnonServerError {
		suffix := ""
		if errName != "" {
			suffix = ": " + errName
		}
		env.log().Info(fmt.Sprintf("Nous free tier %s failed (%d%s)", action, status, suffix))
	}
	seconds, has := parseRetryAfterSeconds(resp.Header)
	retryAfter := time.Duration(seconds) * time.Second
	authErr := anonErr(anonFailureCopy(verdict.code, retryAfter, has), verdict.code, retryAfter, has)
	if verdict.dead {
		return nil, &AnonCredentialDeadError{Auth: authErr}
	}
	return nil, authErr
}

// The create body, preview_full_connectors only for a recognised true or false spelling
func mintRequestBody() map[string]any {
	raw := strings.TrimSpace(os.Getenv(PreviewFullConnectorsEnv))
	switch raw {
	case "1", "true":
		return map[string]any{"preview_full_connectors": true}
	case "0", "false":
		return map[string]any{"preview_full_connectors": false}
	}
	return map[string]any{}
}

// Creates an anonymous account and returns its one time anon credential payload
func mintGuest(env PoolEnv, client *http.Client, portalBaseURL string) (map[string]any, error) {
	resp, err := anonPost(env, client, strings.TrimRight(portalBaseURL, "/")+"/api/anonymous/create", mintRequestBody())
	if err != nil {
		return nil, err
	}
	payload, err := raiseForAnonStatus(env, resp, "sign-up", nil)
	if err != nil {
		return nil, err
	}
	token, ok := payload["token"].(string)
	if !ok || !strings.HasPrefix(token, "anon_") {
		env.log().Info("Nous free tier sign-up returned no credential")
		return nil, anonErr(AnonFailureCopy[AnonServerError], AnonServerError, 0, false)
	}
	return payload, nil
}

// The token carries a scope claim lacking inference invoke, a scopeless JWT reading as an inference one
func isConnectorsOnly(token string) bool {
	claims := DecodeJWTClaims(token)
	scopes := scopeValues(claims["scope"])
	for k := range scopeValues(claims["scp"]) {
		scopes[k] = true
	}
	return len(scopes) > 0 && !scopes[NousInferenceInvokeScope]
}

// IsConnectorsOnly reports whether the token carries only the connectors scope
func IsConnectorsOnly(token string) bool { return isConnectorsOnly(token) }

// Bearer for a connectors call, the profile token then a guest's connectors token, preferring one outliving skew
func heldConnectorsToken(state map[string]any, skew time.Duration) string {
	return heldConnectorsTokenFor(state, skew, true)
}

// Held connectors bearer, an expiring one accepted only when no skew was asked for
func heldConnectorsTokenFor(state map[string]any, skew time.Duration, hasSkew bool) string {
	if state == nil {
		return ""
	}
	slots := []map[string]any{state}
	if isGuestState(state) {
		if connectors, ok := state["connectors_token"].(map[string]any); ok {
			slots = append(slots, connectors)
		}
	}
	type heldToken struct {
		token   string
		expires any
	}
	var held []heldToken
	for _, slot := range slots {
		if token, ok := slot["access_token"].(string); ok && token != "" {
			held = append(held, heldToken{token, slot["expires_at"]})
		}
	}
	for _, h := range held {
		if !IsExpiring(h.expires, skew) {
			return h.token
		}
	}
	if len(held) > 0 && !hasSkew {
		return held[0].token
	}
	return ""
}

// Exchanges the anon credential for a short lived JWT, a challenge or sign in refusal surfacing as its error
func exchangeAnonJWT(env PoolEnv, client *http.Client, portalBaseURL, anonToken string, authState map[string]any, purpose string) (map[string]any, error) {
	body := map[string]any{"token": anonToken, "client": clientInfo(env), "purpose": purpose}
	resp, err := anonPost(env, client, strings.TrimRight(portalBaseURL, "/")+"/api/anonymous/token", body)
	if err != nil {
		return nil, err
	}
	challenge := func(payload map[string]any) error {
		return challengeError(env, payload, portalBaseURL, anonToken, authState)
	}
	signin := func(payload map[string]any) error { return signinRequiredError(payload["message"]) }
	proofOfWork := func(map[string]any) error {
		return anonErr(AnonFailureCopy[AnonPowRequired], AnonPowRequired, 0, false)
	}
	payload, err := raiseForAnonStatus(env, resp, "token exchange", map[anonRefusalKey]anonVerdict{
		{428, "challenge_required", false}: challenge, {428, "pow_", false}: proofOfWork, {428, "", true}: signin,
		{403, "signin_required", false}: signin, {403, "access_denied", false}: signin})
	if err != nil {
		return nil, err
	}
	accessToken, ok := payload["access_token"].(string)
	if !ok || accessToken == "" {
		env.log().Info("Nous free tier token exchange returned no token")
		return nil, anonErr(AnonFailureCopy[AnonServerError], AnonServerError, 0, false)
	}
	if !isConnectorsOnly(accessToken) {
		noteOptionalChallenges(env, payload, portalBaseURL)
	}
	return payload, nil
}

// Writes an exchange result into a guest state, a connectors only token beside it in connectors_token
func applyExchangeToState(state, exchanged map[string]any) {
	accessToken := anyString(exchanged["access_token"])
	claims := DecodeJWTClaims(accessToken)
	now := time.Now().UTC()
	var expiresAt time.Time
	if exp, ok := jsonNumber(claims["exp"]); ok {
		sec := math.Floor(exp)
		expiresAt = time.Unix(int64(sec), int64((exp-sec)*1e9)).UTC()
	} else {
		ttl := 900
		if v, ok := jsonNumber(exchanged["expires_in"]); ok && v != 0 {
			ttl = int(v)
		}
		expiresAt = now.Add(time.Duration(ttl) * time.Second)
	}
	if isConnectorsOnly(accessToken) {
		state["connectors_token"] = map[string]any{"access_token": accessToken, "expires_at": nousISO(expiresAt)}
		return
	}
	inferenceURL := validateNousInferenceURLFromNetwork(anyString(exchanged["inference_base_url"]))
	if inferenceURL == "" {
		inferenceURL = DefaultNousWelcomeURL
	}
	scope := claims["scope"]
	if !pyTruthy(scope) {
		scope = claims["scp"]
	}
	if !pyTruthy(scope) {
		scope = state["scope"]
	}
	if list, ok := scope.([]any); ok {
		parts := make([]string, 0, len(list))
		for _, s := range list {
			parts = append(parts, fmt.Sprint(s))
		}
		scope = strings.Join(parts, " ")
	}
	expiresIn := int(expiresAt.Sub(now).Seconds())
	if expiresIn < 0 {
		expiresIn = 0
	}
	tier := AnonAccountTier
	if t := claims["account_tier"]; pyTruthy(t) {
		tier = fmt.Sprint(t)
	}
	state["access_token"] = accessToken
	state["token_type"] = "Bearer"
	state["scope"] = scope
	state["obtained_at"] = nousISO(now)
	state["expires_at"] = nousISO(expiresAt)
	state["expires_in"] = expiresIn
	state["account_tier"] = tier
	state["inference_base_url"] = inferenceURL
	for _, key := range []string{"user_id", "org_id"} {
		if pyTruthy(exchanged[key]) {
			state[key] = exchanged[key]
		}
	}
	delete(state, "refresh_token")
}

// Numeric JSON value as float64, booleans and strings refused
func jsonNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// Portal base URL for minting, the operator override else the default
func anonPortalBaseURL(env PoolEnv) string {
	portal := nousPortalEnvOverride(env)
	if portal == "" {
		portal = DefaultNousPortalURL
	}
	return strings.TrimRight(portal, "/")
}

// Stable identity of a Nous credential, the anon token for a guest and the refresh token for an account
func sharedIdentityKey(state map[string]any) any {
	if state == nil {
		return nil
	}
	if isGuestState(state) {
		return state["anon_token"]
	}
	return state["refresh_token"]
}

// Mints under the caller's locks, persisting the identity before any exchange with the claim stamped
func mintLocked(env PoolEnv, client *http.Client, portal string, authStore AuthData, welcomeClaim bool) (map[string]any, error) {
	minted, err := mintGuest(env, client, portal)
	if err != nil {
		return nil, err
	}
	state := map[string]any{
		"auth_method": AnonAuthMethod, "account_tier": AnonAccountTier,
		"anon_token": minted["token"], "client_id": AnonClientID,
		"portal_base_url": strings.TrimRight(portal, "/"),
		"user_id":         minted["user_id"], "org_id": minted["org_id"],
		"idle_ttl_days":  minted["idle_ttl_days"],
		WelcomeClaimFlag: welcomeClaim,
	}
	if err := env.Store.StoreProviderState(authStore, "nous", state, false); err != nil {
		return nil, err
	}
	if err := env.Store.Save(authStore); err != nil {
		return nil, err
	}
	writeSharedNousState(env, state, false)
	env.log().Info("Nous free tier ready (identity minted)")
	return state, nil
}

var (
	mintRetryLadder    = []time.Duration{15 * time.Second, 60 * time.Second, 300 * time.Second}
	mintPausedMinWait  = 60 * time.Second
	anonMonotonic      = time.Now
	mintFailuresMu     sync.Mutex
	mintFailures       = map[string]*MintFailure{}
	anonForeverInstant = time.Unix(1<<62, 0)
)

// MintFailure is why the last mint for a profile failed and when the next one may run
type MintFailure struct {
	Code       string
	Message    string
	Retryable  bool
	RetryAfter time.Duration
	NotBefore  time.Time
	Attempts   int
}

// Wait still to run before the next attempt, zero for a terminal code
func (f *MintFailure) Remaining() time.Duration {
	if !f.Retryable {
		return 0
	}
	left := f.NotBefore.Sub(anonMonotonic()).Round(time.Millisecond)
	if left < 0 {
		return 0
	}
	return left
}

// Status RPC shape with retry_after in whole seconds still to wait
func (f *MintFailure) AsPayload() map[string]any {
	retryAfter := 0
	if f.Retryable {
		retryAfter = int(math.Ceil(f.Remaining().Seconds()))
	}
	return map[string]any{"error_code": f.Code, "error": f.Message, "retryable": f.Retryable, "retry_after": retryAfter}
}

// Per profile memo key, empty for the unscoped launch profile
func mintMemoKey(env PoolEnv) string {
	override := home.Override(env.ctx())
	if override == "" {
		return ""
	}
	return home.Key(override)
}

func mintFailureForProfile(env PoolEnv) *MintFailure {
	mintFailuresMu.Lock()
	defer mintFailuresMu.Unlock()
	return mintFailures[mintMemoKey(env)]
}

func clearMintFailure(env PoolEnv) {
	mintFailuresMu.Lock()
	defer mintFailuresMu.Unlock()
	delete(mintFailures, mintMemoKey(env))
}

func resetMintMemoForTests() {
	mintFailuresMu.Lock()
	defer mintFailuresMu.Unlock()
	mintFailures = map[string]*MintFailure{}
}

// Latest mint failure payload for the profile, nil when none is recorded
func lastMintFailure(env PoolEnv) map[string]any {
	if failure := mintFailureForProfile(env); failure != nil {
		return failure.AsPayload()
	}
	return nil
}

// Coded auth error carried directly by err, the anon wrappers unwrapped one level
func anonAuthErrorOf(err error) *AuthError {
	switch e := err.(type) {
	case *AuthError:
		return e
	case *AnonCredentialDeadError:
		return e.Auth
	case *AnonChallengeRequiredError:
		return e.Auth
	}
	return nil
}

// Every mint failure as a coded error, transport failures unreachable and anything else a server error
func classifyMintError(err error) error {
	if ae := anonAuthErrorOf(err); ae != nil && ae.Code != "" {
		return err
	}
	code := AnonServerError
	var netErr net.Error
	var urlErr *url.Error
	var opErr *net.OpError
	if errors.As(err, &netErr) || errors.As(err, &urlErr) || errors.As(err, &opErr) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) {
		code = AnonUnreachable
	}
	var retryAfter time.Duration
	has := false
	if ae := anonAuthErrorOf(err); ae != nil && ae.HasRetryAfter {
		retryAfter, has = ae.RetryAfter, true
	}
	wrapped := anonErr(AnonFailureCopy[code], code, retryAfter, has)
	wrapped.Err = err
	return wrapped
}

// Records a mint failure with its cooldown, climbing the retry ladder for repeats of one code
func noteMintFailure(env PoolEnv, err error) *MintFailure {
	ae := anonAuthErrorOf(err)
	code := AnonServerError
	if ae != nil && ae.Code != "" {
		code = ae.Code
	}
	previous := mintFailureForProfile(env)
	attempts := 1
	if previous != nil && previous.Code == code {
		attempts = previous.Attempts + 1
	}
	retryable := !AnonTerminalCodes[code]
	var wait time.Duration
	notBefore := anonForeverInstant
	if retryable {
		if ae != nil && ae.HasRetryAfter && ae.RetryAfter > 0 {
			wait = ae.RetryAfter
		} else {
			wait = mintRetryLadder[min(attempts, len(mintRetryLadder))-1]
		}
		if code == AnonGatePaused && wait < mintPausedMinWait {
			wait = mintPausedMinWait
		}
		if wait < time.Second {
			wait = time.Second
		}
		notBefore = anonMonotonic().Add(wait)
	}
	failure := &MintFailure{Code: code, Message: err.Error(), Retryable: retryable, RetryAfter: wait, NotBefore: notBefore, Attempts: attempts}
	mintFailuresMu.Lock()
	mintFailures[mintMemoKey(env)] = failure
	mintFailuresMu.Unlock()
	return failure
}

// Adopts the shared identity, keeps the profile's own, or mints under the profile then shared locks
func reconcileAndProvision(env PoolEnv, timeout time.Duration, force, welcomeClaim bool) (map[string]any, *MintFailure, error) {
	portal := anonPortalBaseURL(env)
	unlock, err := env.Store.Lock(AuthLockTimeout)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	authStore, err := env.Store.Load()
	if err != nil {
		return nil, nil, err
	}
	profileState := env.Store.ProviderState(authStore, "nous")
	sharedTimeout := max(timeout, 5*time.Second)
	unlockShared, err := nousSharedStoreLock(env, sharedTimeout)
	if err != nil {
		return nil, nil, err
	}
	defer unlockShared()
	shared, _ := readSharedNousState(env)
	if len(shared) > 0 && sharedIdentityKey(shared) != sharedIdentityKey(profileState) {
		state := map[string]any{}
		for k, v := range shared {
			if k != nousSharedRenewalKey {
				state[k] = v
			}
		}
		if err := env.Store.StoreProviderState(authStore, "nous", state, !isGuestState(state)); err != nil {
			return nil, nil, err
		}
		if err := env.Store.Save(authStore); err != nil {
			return nil, nil, err
		}
		env.log().Debug("Nous identity adopted from the shared store")
		return state, nil, nil
	}
	if len(profileState) > 0 {
		if len(shared) == 0 {
			writeSharedNousState(env, profileState, false)
		}
		return profileState, nil, nil
	}
	if failure := mintFailureForProfile(env); failure != nil && !force && anonMonotonic().Before(failure.NotBefore) {
		return nil, nil, nil
	}
	verify := resolveOAuthVerify(env, nil, "", nil)
	client := nousHTTPClientFn(env, timeout, verify)
	state, err := mintLocked(env, client, portal, authStore, welcomeClaim)
	if err != nil {
		classified := classifyMintError(err)
		return nil, noteMintFailure(env, classified), classified
	}
	return state, nil, nil
}

// PortalIdentityOptions are the inputs of an explicit Nous identity provision
type PortalIdentityOptions struct {
	Explicit     bool
	Timeout      time.Duration
	Force        bool
	WelcomeClaim bool
}

// EnsurePortalIdentity makes sure the profile has a Nous identity, minting a guest only when no store holds one
func EnsurePortalIdentity(env PoolEnv, opts PortalIdentityOptions) (map[string]any, error) {
	if !opts.Explicit {
		return nil, errors.New("ensure_portal_identity: only explicit creators may call this (explicit=True)")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = GuestMintTimeout
	}
	if !guestAllowed(env) {
		return nil, nil
	}
	if failure := mintFailureForProfile(env); failure != nil && !opts.Force && len(currentNousState(env)) == 0 && anonMonotonic().Before(failure.NotBefore) {
		return nil, nil
	}
	state, noted, err := reconcileAndProvision(env, opts.Timeout, opts.Force, opts.WelcomeClaim)
	if err != nil {
		classified := classifyMintError(err)
		if noted == nil {
			noted = noteMintFailure(env, classified)
		}
		next := ", not retried"
		if noted.Retryable {
			next = fmt.Sprintf(", next try in %.0fs", noted.RetryAfter.Seconds())
		}
		env.log().Info(fmt.Sprintf("Nous free tier not set up (%s, attempt %d%s)", noted.Code, noted.Attempts, next))
		return nil, classified
	}
	if state != nil {
		clearMintFailure(env)
		if _, err := nousGuestHandoffDrainFn(env); err != nil {
			return nil, err
		}
	}
	return state, nil
}

// Re exchanges the guest's anon credential in place for the named purpose and returns the minted token
func refreshGuestState(env PoolEnv, state map[string]any, client *http.Client, purpose string) (string, error) {
	if purpose == "inference" && !freeInferenceActive(env, state) {
		return "", nousErr("Icebath is not logged into Nous Portal.", "nous_auth_missing", false)
	}
	anonToken, ok := state["anon_token"].(string)
	if !ok || anonToken == "" {
		return "", &AnonCredentialDeadError{Auth: anonErr(AnonFailureCopy[AnonCredentialDead], AnonCredentialDead, 0, false)}
	}
	exchanged, err := exchangeAnonJWT(env, client, nousPortalBaseURLIn(env, state), anonToken, state, purpose)
	if err != nil {
		return "", err
	}
	applyExchangeToState(state, exchanged)
	return anyString(exchanged["access_token"]), nil
}

// Code of a dead anon credential error and whether err is one
func anonCredentialDeadCode(err error) (string, bool) {
	var dead *AnonCredentialDeadError
	if !errors.As(err, &dead) {
		return "", false
	}
	return dead.Auth.Code, true
}

// Drops the dead guest that failed so the next need re mints, a sibling's newer identity left alone
func clearDeadGuest(env PoolEnv, code, deadToken string) error {
	reason := code
	err := func() error {
		unlock, err := env.Store.Lock(AuthLockTimeout)
		if err != nil {
			return err
		}
		defer unlock()
		token := deadToken
		authStore, err := env.Store.Load()
		if err != nil {
			return err
		}
		state := env.Store.ProviderState(authStore, "nous")
		if isGuestState(state) {
			if token == "" {
				token = anyString(state["anon_token"])
			}
			if anyString(state["anon_token"]) == token {
				delete(section(authStore, "providers"), "nous")
				delete(section(authStore, "credential_pool"), "nous")
				if authStore["active_provider"] == "nous" {
					authStore["active_provider"] = nil
				}
				if err := env.Store.Save(authStore); err != nil {
					return err
				}
			}
		}
		unlockShared, err := nousSharedStoreLock(env, AuthLockTimeout)
		if err != nil {
			return err
		}
		defer unlockShared()
		shared, _ := readSharedNousState(env)
		if token != "" && isGuestState(shared) && anyString(shared["anon_token"]) == token {
			clearSharedNousState(env, reason)
		}
		return nil
	}()
	if err != nil {
		return err
	}
	clearMintFailure(env)
	env.log().Info(fmt.Sprintf("Nous free-tier identity retired (%s); a new one is set up on next use", reason))
	return nil
}

// Moves the config default off a requested model the gateway switched to its backing model
func ApplyModelSwitchConfig(env PoolEnv, baseURL, requested, backing string) bool {
	cfg, err := anonConfigReadonly(env)
	if err != nil {
		env.log().Debug(fmt.Sprintf("model switch: config default left as is: %v", err))
		return false
	}
	modelCfg := anonModelSection(cfg)
	if strings.TrimSpace(anyString(modelCfg["default"])) != requested {
		return false
	}
	if _, err := updateConfigForProvider(env, "nous", baseURL, backing, false); err != nil {
		env.log().Debug(fmt.Sprintf("model switch: config default left as is: %v", err))
		return false
	}
	env.log().Info(fmt.Sprintf("Config default model moved %s -> %s", requested, backing))
	return true
}

// The model section as a mapping, a bare string model read as its default
func anonModelSection(cfg map[string]any) map[string]any {
	switch raw := cfg["model"].(type) {
	case map[string]any:
		return raw
	case string:
		return map[string]any{"default": raw}
	}
	return map[string]any{}
}

// A guest that carries inference has not yet shown the one time availability notice
func guestNoticePending(env PoolEnv) bool {
	state := currentNousState(env)
	return freeInferenceActive(env, state) && !pyTruthy(state[GuestNoticeFlag])
}

// GuestNoticePending reports whether a guest carrying inference still owes the one time availability notice
func GuestNoticePending(env PoolEnv) bool { return guestNoticePending(env) }

// Persists the notice flag on the guest state, false when there is no guest to mark
func markGuestNoticeShown(env PoolEnv) (bool, error) {
	state, err := setGuestFlag(env, GuestNoticeFlag)
	if err != nil {
		return false, err
	}
	return state != nil, nil
}

// Sets flag on the guest's providers.nous state in whichever store holds it, nil when there is no guest
func setGuestFlag(env PoolEnv, flag string) (map[string]any, error) {
	var out map[string]any
	err := env.Store.ProviderTransaction("nous", AuthLockTimeout, func(tx *ProviderTxn) error {
		if !isGuestState(tx.State) || tx.SourcePath == "" {
			return nil
		}
		if pyTruthy(tx.State[flag]) {
			out = tx.State
			return nil
		}
		next := make(map[string]any, len(tx.State)+1)
		for k, v := range tx.State {
			next[k] = v
		}
		next[flag] = true
		if err := saveGuestState(env, tx.Data, next, tx.SourcePath); err != nil {
			return err
		}
		out = next
		return nil
	})
	return out, err
}

// Writes the guest's providers.nous back to the store it was read from under the caller's lock
func saveGuestState(env PoolEnv, authStore AuthData, state map[string]any, sourcePath string) error {
	if samePath(sourcePath, env.Store.Path()) {
		section(authStore, "providers")["nous"] = state
		return env.Store.Save(authStore)
	}
	sourceStore, err := env.Store.LoadFile(sourcePath)
	if err != nil {
		return err
	}
	section(sourceStore, "providers")["nous"] = state
	return env.Store.SaveTo(sourceStore, sourcePath)
}

// The providers.nous shape for a signed in account from a token grant, refreshed to an invoke JWT
func accountStateFromToken(env PoolEnv, tokenData map[string]any, portalBaseURL, clientID string, scope any, verify oauthVerify, timeout time.Duration) (map[string]any, error) {
	now := time.Now().UTC()
	ttl := coerceTTLSeconds(nousOr(tokenData["expires_in"], 0))
	inferenceURL := optionalBaseURL(tokenData["inference_base_url"])
	if inferenceURL == "" {
		inferenceURL = strings.TrimRight(DefaultNousInferenceURL, "/")
	}
	tokenScope := tokenData["scope"]
	if !pyTruthy(tokenScope) {
		tokenScope = scope
	}
	state := map[string]any{
		"portal_base_url": portalBaseURL, "inference_base_url": inferenceURL,
		"client_id": clientID, "scope": tokenScope,
		"token_type":   nousOr(tokenData["token_type"], "Bearer"),
		"access_token": tokenData["access_token"], "refresh_token": tokenData["refresh_token"],
		"obtained_at": nousISO(now), "expires_at": nousISOAfter(now, ttl), "expires_in": ttl,
		"tls": tlsStateFromVerify(verify.Insecure, verify.CABundle),
	}
	if _, ok := tokenData["token_type"]; !ok {
		state["token_type"] = "Bearer"
	}
	for k, v := range nousEmptyAgentKeyFields {
		state[k] = v
	}
	refreshed, err := refreshNousOAuthFromStateFn(env, state, timeout, false, nil)
	if err != nil {
		return nil, err
	}
	refreshed["auth_method"] = UpgradedAuthMethod
	return refreshed, nil
}

// Moves the config off the free tier route after a sign in persisted the account
func settleAfterUpgrade(env PoolEnv, state map[string]any) {
	settleAfterUpgradeResult(env, state)
}

// Settles the config after a sign in and returns the default it carries and whether this call wrote it
func settleAfterUpgradeResult(env PoolEnv, accountState map[string]any) (string, bool) {
	cfg, err := anonConfigReadonly(env)
	if err != nil {
		env.log().Warn(fmt.Sprintf("sign-in completion: config unreadable, default model left as is: %v", err))
		return "", false
	}
	modelCfg := anonModelSection(cfg)
	current := strings.TrimSpace(anyString(modelCfg["default"]))
	if modelCfg["default"] == nil {
		current = ""
	}
	onWelcomeModel := current == GuestModel
	onWelcomeHost := RouteIsWelcomeHost(anyString(modelCfg["base_url"]))
	if !onWelcomeModel && !onWelcomeHost {
		return current, false
	}
	model := current
	if onWelcomeModel {
		model = RecommendedNousDefaultModel(env.ctx()).Model
	}
	defaultModel := ""
	if onWelcomeModel {
		defaultModel = model
	}
	if _, err := updateConfigForProvider(env, "nous", anyString(accountState["inference_base_url"]), defaultModel, onWelcomeModel && model == ""); err != nil {
		env.log().Warn(fmt.Sprintf("sign-in completion: could not update the default model: %v", err))
		return current, false
	}
	return model, true
}
