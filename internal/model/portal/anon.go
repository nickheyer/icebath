package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth"
)

const (
	AnonAuthMethod   = "anonymous"
	AnonClientID     = "nas-anonymous"
	AnonAccountTier  = "anonymous"
	GuestModel       = "portal/welcome"
	GuestMintTimeout = 5 * time.Second

	FreeTierLabel       = "Icebath Portal · free tier"
	UpgradeHint         = "Run `icebath auth upgrade` to sign in with an Icebath Portal account, or /login inside a chat."
	FreeTierNotSignedIn = "You're not signed in. Free inference and connectors are always on. " +
		"Run `icebath auth` to sign in with an Icebath Portal account."

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

	GuestNoticeFlag    = "guest_notice_shown"
	UpgradedAuthMethod = "oauth_device_code"

	anonSecretHeader  = "x-anonymous-api-secret"
	purposeInference  = "inference"
	purposeConnectors = "connectors"
	signinIsFree      = "Signing in is free."
	mintPausedMinWait = 60 * time.Second
)

var mintRetryLadder = []time.Duration{15 * time.Second, 60 * time.Second, 300 * time.Second}

func (a *Account) GuestAllowed(ctx context.Context) bool { return a.guestAllowed(ctx) }

func (a *Account) State(ctx context.Context) (json.RawMessage, bool) {
	state := a.quietState(ctx)
	if state == nil {
		return nil, false
	}
	return state.raw(), true
}

func AnonTerminal(code string) bool {
	switch code {
	case AnonGateClosed, AnonPowRequired, AnonAccountLocked, AnonSigninRequired:
		return true
	}
	return false
}

func AnonUnreachableCode(code string) bool {
	return code == AnonUnreachable || code == AnonServerError
}

func anonFailureTemplate(code string) (string, bool) {
	switch code {
	case AnonGateClosed:
		return "This version can't be used without an Icebath Portal account. " + signinIsFree, true
	case AnonGatePaused:
		return "Using icebath without signing in is paused for a moment. " + signinIsFree, true
	case AnonRateLimited:
		return "Lots of people are getting started right now. Try again in {wait}. Signing in is free and skips the wait.", true
	case AnonPowRequired:
		return "The Icebath Portal server asked for a proof of work, but that isn't implemented in your " +
			"Agent yet. Sign in with an Icebath Portal account to continue.", true
	case AnonChallengeRequired:
		return "Finish the quick check in your browser, then try again.", true
	case AnonSigninRequired:
		return "Free guest access isn't available here. Sign in with an Icebath Portal account to continue.", true
	case AnonAccountLocked:
		return "This session can't continue without signing in. " + signinIsFree, true
	case AnonCredentialDead:
		return "Your session ended. A new one starts on its own.", true
	case AnonUnreachable:
		return "The Icebath Portal service couldn't be reached. Check your internet connection and try again.", true
	case AnonServerError:
		return "The Icebath Portal service had a hiccup. Try again in a moment.", true
	}
	return "", false
}

func FriendlyWait(wait time.Duration) string {
	s := max(0, wait.Seconds())
	switch {
	case s <= 15:
		return "a few seconds"
	case s < 90:
		return "about a minute"
	case s < 3600:
		return fmt.Sprintf("about %d minutes", int(math.RoundToEven(s/60)))
	}
	hours := int(math.RoundToEven(s / 3600))
	if hours <= 1 {
		return "about an hour"
	}
	return fmt.Sprintf("about %d hours", hours)
}

func AnonFailureCopy(code string, retryAfter time.Duration) string {
	template, ok := anonFailureTemplate(code)
	if !ok {
		template, _ = anonFailureTemplate(AnonServerError)
	}
	if retryAfter <= 0 {
		retryAfter = 60 * time.Second
	}
	return strings.ReplaceAll(template, "{wait}", FriendlyWait(retryAfter))
}

type CredentialDeadError struct {
	Err *auth.Error
}

func (e *CredentialDeadError) Error() string { return e.Err.Error() }

func (e *CredentialDeadError) Unwrap() error { return e.Err }

func anonErr(message, code string, retryAfter time.Duration) *auth.Error {
	return &auth.Error{Message: message, Code: code, RetryAfter: retryAfter, Retryable: boolPtr(!AnonTerminal(code))}
}

func isGuestState(state *object) bool {
	return state != nil && state.str("auth_method") == AnonAuthMethod
}

func IsGuestState(state json.RawMessage) bool {
	parsed, ok := parseObject(state)
	return ok && isGuestState(parsed)
}

func IsAnonymousRequest(provider string, apiKey auth.Secret) bool {
	return provider == Provider && claimsOf(apiKey.Reveal()).str("account_tier") == AnonAccountTier
}

func (a *Account) HasGuest(ctx context.Context) bool {
	return isGuestState(a.quietState(ctx))
}

func (a *Account) freeInferenceActive(ctx context.Context, state *object) bool {
	return isGuestState(state) && state.get(auth.WelcomeClaimFlag) == true && a.guestAllowed(ctx)
}

func (a *Account) FreeInferenceActive(ctx context.Context) bool {
	return a.freeInferenceActive(ctx, a.quietState(ctx))
}

func (a *Account) TrialModel(ctx context.Context) string {
	if a.FreeInferenceActive(ctx) {
		return GuestModel
	}
	return ""
}

func (a *Account) FreeTierRoute(ctx context.Context) (bool, error) {
	ctx = a.bind(ctx)
	if !a.FreeInferenceActive(ctx) {
		return false, nil
	}
	provider, err := a.resolveProvider(ctx, false)
	if err != nil {
		return false, err
	}
	return provider == Provider, nil
}

func (a *Account) OnLocalDesktopBackend(ctx context.Context) bool {
	return a.localDesktop(a.bind(ctx))
}

func (a *Account) guestWithoutFreeTier(ctx context.Context, state *object) bool {
	return isGuestState(state) && !a.freeInferenceActive(ctx, state)
}

func anonSecret() string {
	return strings.TrimSpace(os.Getenv(EnvAnonSecret))
}

func (a *Account) anonHeaders(ctx context.Context) map[string]string {
	headers := map[string]string{"Content-Type": "application/json", "User-Agent": a.userAgent(ctx)}
	if secret := anonSecret(); secret != "" {
		headers[anonSecretHeader] = secret
	}
	return headers
}

type anonKey struct {
	status int
	code   string
	any    bool
}

type anonVerdict struct {
	dead bool
	code string
}

var anonRefusals = map[anonKey]anonVerdict{
	{status: 404, code: "unknown_token"}:               {dead: true, code: AnonCredentialDead},
	{status: 404, any: true}:                           {code: AnonGateClosed},
	{status: 401, code: "invalid_shared_secret"}:       {code: AnonGateClosed},
	{status: 401, any: true}:                           {dead: true, code: AnonCredentialDead},
	{status: 403, code: "account_locked"}:              {dead: true, code: AnonAccountLocked},
	{status: 403, code: "anonymous_accounts_disabled"}: {code: AnonGatePaused},
	{status: 403, code: "circuit_open"}:                {code: AnonGatePaused},
	{status: 428, any: true}:                           {code: AnonPowRequired},
	{status: 429, any: true}:                           {code: AnonRateLimited},
	{status: 503, code: "temporarily_disabled"}:        {code: AnonGatePaused},
}

type anonOverride func(body *object) error

func anonStatus(r reply, action string, overrides map[anonKey]anonOverride) (*object, error) {
	payload := r.object()
	code := textualOrEmpty(payload.get("error"))
	if r.status == 200 || r.status == 201 {
		return payload, nil
	}
	if strings.HasPrefix(code, "pow_") {
		code = "pow_"
	}
	if build, ok := overrides[anonKey{status: r.status, code: code}]; ok {
		return nil, build(payload)
	}
	if build, ok := overrides[anonKey{status: r.status, any: true}]; ok {
		return nil, build(payload)
	}
	verdict, ok := anonRefusals[anonKey{status: r.status, code: code}]
	if !ok {
		verdict, ok = anonRefusals[anonKey{status: r.status, any: true}]
	}
	if !ok {
		verdict = anonVerdict{code: AnonServerError}
		if code == "pow_" {
			verdict.code = AnonPowRequired
		}
	}
	if verdict.code == AnonServerError {
		suffix := ""
		if code != "" {
			suffix = ": " + code
		}
		slog.Info(fmt.Sprintf("Icebath Portal free tier %s failed (%d%s)", action, r.status, suffix))
	}
	retryAfter, _ := httpclient.RetryAfter(r.header, time.Now())
	failure := anonErr(AnonFailureCopy(verdict.code, retryAfter), verdict.code, retryAfter)
	if verdict.dead {
		return nil, &CredentialDeadError{Err: failure}
	}
	return nil, failure
}

func mintRequestBody() *object {
	raw := strings.TrimSpace(os.Getenv(EnvPreviewFullConnectors))
	switch raw {
	case "1", "true":
		return pairs("preview_full_connectors", true)
	case "0", "false":
		return pairs("preview_full_connectors", false)
	}
	return newObject()
}

func (a *Account) mintGuest(ctx context.Context, client *http.Client, portalBaseURL string, timeout time.Duration) (*object, error) {
	r, err := postJSON(ctx, client, strings.TrimRight(portalBaseURL, "/")+"/api/anonymous/create", a.anonHeaders(ctx), mintRequestBody(), timeout)
	if err != nil {
		return nil, err
	}
	payload, err := anonStatus(r, "sign-up", nil)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(payload.str("token"), "anon_") {
		slog.Info("Icebath Portal free tier sign-up returned no credential")
		return nil, anonErr(AnonFailureCopy(AnonServerError, 0), AnonServerError, 0)
	}
	return payload, nil
}

func isConnectorsOnly(token any) bool {
	claims := claimsOf(token)
	scopes := scopeValues(claims.get("scope"))
	for s := range scopeValues(claims.get("scp")) {
		scopes[s] = true
	}
	return len(scopes) > 0 && !scopes[auth.PortalInferenceInvokeScope]
}

func heldConnectorsToken(state *object, skew *time.Duration) string {
	if state == nil {
		return ""
	}
	slots := []*object{state}
	if isGuestState(state) {
		slots = append(slots, state.obj("connectors_token"))
	}
	type held struct {
		token     string
		expiresAt any
	}
	tokens := []held{}
	for _, slot := range slots {
		if token := slot.str("access_token"); token != "" {
			tokens = append(tokens, held{token: token, expiresAt: slot.get("expires_at")})
		}
	}
	margin := time.Duration(0)
	if skew != nil {
		margin = *skew
	}
	for _, h := range tokens {
		if !isExpiring(h.expiresAt, margin) {
			return h.token
		}
	}
	if len(tokens) > 0 && skew == nil {
		return tokens[0].token
	}
	return ""
}

func (a *Account) exchangeAnonJWT(ctx context.Context, client *http.Client, portalBaseURL, anonToken string, state *object, purpose string, timeout time.Duration) (*object, error) {
	body := pairs("token", anonToken, "client", a.clientInfo(ctx), "purpose", purpose)
	r, err := postJSON(ctx, client, strings.TrimRight(portalBaseURL, "/")+"/api/anonymous/token", a.anonHeaders(ctx), body, timeout)
	if err != nil {
		return nil, err
	}
	challenge := func(body *object) error {
		return challengeError(body, portalBaseURL, anonToken, auth.TLSStateFrom(state.raw()))
	}
	signin := func(body *object) error { return signinRequiredError(body.get("message")) }
	proofOfWork := func(*object) error { return anonErr(AnonFailureCopy(AnonPowRequired, 0), AnonPowRequired, 0) }
	payload, err := anonStatus(r, "token exchange", map[anonKey]anonOverride{
		{status: 428, code: "challenge_required"}: challenge, {status: 428, code: "pow_"}: proofOfWork, {status: 428, any: true}: signin,
		{status: 403, code: "signin_required"}: signin, {status: 403, code: "access_denied"}: signin,
	})
	if err != nil {
		return nil, err
	}
	if payload.str("access_token") == "" {
		slog.Info("Icebath Portal free tier token exchange returned no token")
		return nil, anonErr(AnonFailureCopy(AnonServerError, 0), AnonServerError, 0)
	}
	if !isConnectorsOnly(payload.get("access_token")) {
		a.noteOptionalChallenges(ctx, payload, portalBaseURL)
	}
	return payload, nil
}

func (a *Account) applyExchangeToState(ctx context.Context, state, exchanged *object) error {
	accessToken := exchanged.str("access_token")
	claims := claimsOf(accessToken)
	now := time.Now()
	nowEpoch := epochOf(now)
	expiresEpoch := nowEpoch
	if exp, ok := claimNumber(claims.get("exp")); ok {
		expiresEpoch = exp
	} else {
		ttl := int64(900)
		if v := exchanged.get("expires_in"); truthy(v) {
			parsed, ok := intOf(v)
			if !ok {
				return errors.New("portal: the token exchange answered with a non-numeric expires_in")
			}
			ttl = parsed
		}
		expiresEpoch = nowEpoch + float64(ttl)
	}
	expiresAt := isoFromEpoch(expiresEpoch)
	if isConnectorsOnly(accessToken) {
		state.set("connectors_token", pairs("access_token", accessToken, "expires_at", expiresAt))
		return nil
	}
	inferenceURL := a.validateInferenceURL(ctx, exchanged.get("inference_base_url"))
	if inferenceURL == "" {
		inferenceURL = auth.DefaultPortalWelcomeURL
	}
	var scope any = claims.get("scope")
	if !truthy(scope) {
		scope = claims.get("scp")
	}
	if !truthy(scope) {
		scope = state.get("scope")
	}
	if items, ok := scope.([]any); ok {
		parts := make([]string, len(items))
		for i, item := range items {
			parts[i] = textual(item)
		}
		scope = strings.Join(parts, " ")
	}
	tier := AnonAccountTier
	if v := claims.get("account_tier"); truthy(v) {
		tier = textual(v)
	}
	state.set("access_token", accessToken)
	state.set("token_type", "Bearer")
	state.set("scope", scope)
	state.set("obtained_at", isoUTC(now))
	state.set("expires_at", expiresAt)
	state.set("expires_in", max(0, int64(math.Trunc(expiresEpoch-nowEpoch))))
	state.set("account_tier", tier)
	state.set("inference_base_url", inferenceURL)
	for _, k := range []string{"user_id", "org_id"} {
		if v := exchanged.get(k); truthy(v) {
			state.set(k, cloneValue(v))
		}
	}
	state.del("refresh_token")
	return nil
}

func (a *Account) canonicalPortalURL(ctx context.Context) string {
	if override := a.portalOverride(ctx); override != "" {
		return strings.TrimRight(override, "/")
	}
	return auth.DefaultPortalURL
}

func sharedIdentityKey(state *object) any {
	if state == nil {
		return nil
	}
	if isGuestState(state) {
		return state.get("anon_token")
	}
	return state.get("refresh_token")
}

func (a *Account) saveLocal(tx *auth.Tx, state *object) error {
	return tx.SaveLocal(state.raw())
}

func (a *Account) restoreActive(ctx context.Context, prior string) error {
	return a.store.RestoreActive(ctx, prior)
}

func (a *Account) mintLocked(ctx context.Context, tx *auth.Tx, client *http.Client, portalBaseURL string, welcomeClaim bool, timeout time.Duration) (*object, error) {
	minted, err := a.mintGuest(ctx, client, portalBaseURL, timeout)
	if err != nil {
		return nil, err
	}
	state := pairs(
		"auth_method", AnonAuthMethod, "account_tier", AnonAccountTier,
		"anon_token", minted.get("token"), "client_id", AnonClientID,
		"portal_base_url", strings.TrimRight(portalBaseURL, "/"),
		"user_id", minted.get("user_id"), "org_id", minted.get("org_id"),
		"idle_ttl_days", minted.get("idle_ttl_days"),
		auth.WelcomeClaimFlag, welcomeClaim,
	)
	if err := a.saveLocal(tx, state); err != nil {
		return nil, err
	}
	a.writeShared(ctx, state, false)
	slog.Info("Icebath Portal free tier ready (identity minted)")
	return state, nil
}

type MintFailure struct {
	Code       string
	Message    string
	Retryable  bool
	RetryAfter time.Duration
	Attempts   int
	notBefore  time.Time
}

type MintFailureStatus struct {
	ErrorCode  string `json:"error_code"`
	Error      string `json:"error"`
	Retryable  bool   `json:"retryable"`
	RetryAfter int    `json:"retry_after"`
}

func (f *MintFailure) quiet(now time.Time) bool {
	return !f.Retryable || now.Before(f.notBefore)
}

func (f *MintFailure) Remaining() time.Duration {
	if !f.Retryable {
		return 0
	}
	return max(0, time.Until(f.notBefore).Round(time.Millisecond))
}

func (f *MintFailure) Status() MintFailureStatus {
	out := MintFailureStatus{ErrorCode: f.Code, Error: f.Message, Retryable: f.Retryable}
	if f.Retryable {
		out.RetryAfter = int(math.Ceil(f.Remaining().Seconds()))
	}
	return out
}

func (a *Account) mintFailureMemo() *MintFailure {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mintFailure
}

func (a *Account) clearMintFailure() {
	a.mu.Lock()
	a.mintFailure = nil
	a.mu.Unlock()
}

func (a *Account) LastMintFailure() (MintFailureStatus, bool) {
	failure := a.mintFailureMemo()
	if failure == nil {
		return MintFailureStatus{}, false
	}
	return failure.Status(), true
}

func isTransportError(err error) bool {
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	var sysErr *os.SyscallError
	var errno syscall.Errno
	return auth.IsNetworkError(err) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &sysErr) || errors.As(err, &errno)
}

func classifyMintError(err error) error {
	prior, isAuth := authErrorOf(err)
	if isAuth && prior.Code != "" {
		return err
	}
	code := AnonServerError
	if isTransportError(err) {
		code = AnonUnreachable
	}
	retryAfter := time.Duration(0)
	if isAuth {
		retryAfter = prior.RetryAfter
	}
	wrapped := anonErr(AnonFailureCopy(code, 0), code, retryAfter)
	wrapped.Cause = err
	return wrapped
}

func (a *Account) noteMintFailure(err error) *MintFailure {
	ae, _ := authErrorOf(err)
	code := AnonServerError
	if ae != nil && ae.Code != "" {
		code = ae.Code
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	attempts := 1
	if previous := a.mintFailure; previous != nil && previous.Code == code {
		attempts = previous.Attempts + 1
	}
	failure := &MintFailure{Code: code, Message: err.Error(), Retryable: !AnonTerminal(code), Attempts: attempts}
	if failure.Retryable {
		wait := mintRetryLadder[min(attempts, len(mintRetryLadder))-1]
		if ae != nil && ae.RetryAfter > 0 {
			wait = ae.RetryAfter
		}
		if code == AnonGatePaused {
			wait = max(wait, mintPausedMinWait)
		}
		wait = max(time.Second, wait)
		failure.RetryAfter = wait
		failure.notBefore = time.Now().Add(wait)
	}
	a.mintFailure = failure
	return failure
}

type mintError struct {
	err     error
	failure *MintFailure
}

func (e *mintError) Error() string { return e.err.Error() }

func (e *mintError) Unwrap() error { return e.err }

func (a *Account) reconcileAndProvision(ctx context.Context, timeout time.Duration, force, welcomeClaim bool) (*object, error) {
	portalBaseURL := a.canonicalPortalURL(ctx)
	var out *object
	err := a.store.Transact(ctx, Provider, auth.LockTimeout, func(tx *auth.Tx) error {
		tctx := tx.Context()
		profileState := a.txState(tx)
		if profileState.size() == 0 {
			profileState = nil
		}
		lctx, unlock, err := a.lockShared(tctx, max(timeout, 5*time.Second))
		if err != nil {
			return err
		}
		defer unlock()
		shared := a.readShared()
		if shared != nil && !sameValue(sharedIdentityKey(shared), sharedIdentityKey(profileState)) {
			state := shared.without(sharedRenewalKey)
			if isGuestState(state) {
				err = a.saveLocal(tx, state)
			} else {
				err = tx.SaveActive(state.raw())
			}
			if err != nil {
				return err
			}
			slog.Debug("Icebath Portal identity adopted from the shared store")
			out = state
			return nil
		}
		if profileState != nil {
			if shared == nil {
				a.writeShared(lctx, profileState, false)
			}
			out = profileState
			return nil
		}
		if failure := a.mintFailureMemo(); failure != nil && !force && failure.quiet(time.Now()) {
			return nil
		}
		minted, err := func() (*object, error) {
			client, _, err := a.verifiedClient(lctx, nil, "", nil)
			if err != nil {
				return nil, err
			}
			return a.mintLocked(lctx, tx, client, portalBaseURL, welcomeClaim, timeout)
		}()
		if err != nil {
			classified := classifyMintError(err)
			return &mintError{err: classified, failure: a.noteMintFailure(classified)}
		}
		out = minted
		return nil
	})
	return out, err
}

type EnsureOptions struct {
	Timeout      time.Duration
	Force        bool
	WelcomeClaim bool
}

func (a *Account) EnsureIdentity(ctx context.Context, o EnsureOptions) (json.RawMessage, error) {
	ctx = a.bind(ctx)
	if !a.guestAllowed(ctx) {
		return nil, nil
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = GuestMintTimeout
	}
	if failure := a.mintFailureMemo(); failure != nil && !o.Force && a.quietState(ctx).size() == 0 && failure.quiet(time.Now()) {
		return nil, nil
	}
	state, err := a.reconcileAndProvision(ctx, timeout, o.Force, o.WelcomeClaim)
	if err != nil {
		var noted *MintFailure
		var minted *mintError
		if errors.As(err, &minted) {
			err, noted = minted.err, minted.failure
		} else {
			err = classifyMintError(err)
			noted = a.noteMintFailure(err)
		}
		next := ", not retried"
		if noted.Retryable {
			next = fmt.Sprintf(", next try in %.0fs", noted.RetryAfter.Seconds())
		}
		slog.Info(fmt.Sprintf("Icebath Portal free tier not set up (%s, attempt %d%s)", noted.Code, noted.Attempts, next))
		return nil, err
	}
	if state == nil {
		return nil, nil
	}
	a.clearMintFailure()
	if _, err := a.Drain(ctx); err != nil {
		return nil, err
	}
	return state.raw(), nil
}

func (a *Account) refreshGuestState(ctx context.Context, state *object, client *http.Client, purpose string, timeout time.Duration) (string, error) {
	if purpose == purposeInference && !a.freeInferenceActive(ctx, state) {
		return "", portalErr(notLoggedIn, "portal_auth_missing", false)
	}
	anonToken := state.str("anon_token")
	if anonToken == "" {
		return "", &CredentialDeadError{Err: &auth.Error{Message: AnonFailureCopy(AnonCredentialDead, 0), Code: AnonCredentialDead}}
	}
	exchanged, err := a.exchangeAnonJWT(ctx, client, a.portalBaseURL(ctx, state), anonToken, state, purpose, timeout)
	if err != nil {
		return "", err
	}
	if err := a.applyExchangeToState(ctx, state, exchanged); err != nil {
		return "", err
	}
	return exchanged.str("access_token"), nil
}

func (a *Account) ClearDeadGuest(ctx context.Context, reason string, deadToken auth.Secret) error {
	return a.clearDeadGuest(a.bind(ctx), reason, deadToken.Reveal())
}

func (a *Account) clearDeadGuest(ctx context.Context, reason, deadToken string) error {
	err := a.store.Transact(ctx, Provider, auth.LockTimeout, func(tx *auth.Tx) error {
		tctx := tx.Context()
		state := a.txState(tx)
		token := deadToken
		if isGuestState(state) {
			if token == "" {
				token = state.str("anon_token")
			}
			if state.str("anon_token") == token {
				if _, err := a.store.Clear(tctx, Provider); err != nil {
					return err
				}
			}
		}
		lctx, unlock, err := a.lockShared(tctx, auth.LockTimeout)
		if err != nil {
			return err
		}
		defer unlock()
		shared := a.readShared()
		if token != "" && isGuestState(shared) && shared.str("anon_token") == token {
			a.clearShared(lctx, reason)
		}
		return nil
	})
	if err != nil {
		return err
	}
	a.clearMintFailure()
	slog.Info(fmt.Sprintf("Icebath Portal free-tier identity retired (%s); a new one is set up on next use", reason))
	return nil
}

func (a *Account) GuestNoticePending(ctx context.Context) bool {
	state := a.quietState(ctx)
	return a.freeInferenceActive(ctx, state) && !truthy(state.get(GuestNoticeFlag))
}

func (a *Account) MarkGuestNoticeShown(ctx context.Context) (bool, error) {
	state, err := a.SetGuestFlag(ctx, GuestNoticeFlag)
	return state != nil, err
}

func (a *Account) SetGuestFlag(ctx context.Context, flag string) (json.RawMessage, error) {
	var out json.RawMessage
	err := a.store.Transact(a.bind(ctx), Provider, auth.LockTimeout, func(tx *auth.Tx) error {
		state := a.txState(tx)
		if !isGuestState(state) || tx.Source() == "" {
			return nil
		}
		if truthy(state.get(flag)) {
			out = state.raw()
			return nil
		}
		state.set(flag, true)
		if err := tx.Save(state.raw()); err != nil {
			return err
		}
		out = state.raw()
		return nil
	})
	return out, err
}
