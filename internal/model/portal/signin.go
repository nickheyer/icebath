package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/auth"
)

const (
	UpgradeStart              = "Sign in with an Icebath Portal account to unlock more models and tools."
	UpgradeAlreadySignedIn    = "Already signed in."
	UpgradeDoNotShare         = "Do not share this code."
	UpgradeTimedOut           = "That sign-in link has expired. Start again whenever you're ready."
	UpgradeNotCompleted       = "Sign-in didn't finish. Try again whenever you're ready."
	UpgradeServiceBusy        = "Signing in couldn't finish because the Icebath Portal service is busy. Try again in {wait}. Your session is still here in the meantime."
	UpgradeServiceUnreachable = "The Icebath Portal service couldn't be reached to finish signing you in. " +
		"Check your internet connection and try again. Your session is still here."
	UpgradeNoDefaultTerminal = "No default model is set yet; run `icebath model` to pick one."
	UpgradeNoDefaultChat     = "No default model is set yet; run /model to pick one."
	UpgradeWaiting           = "Waiting for sign-in..."
	UpgradeWaitingUpTo       = "Waiting for sign-in, up to {minutes}."
	UpgradeCancelled         = "\nSign-in cancelled."
	LoginCommand             = "/login"
	LoginStarting            = "Starting sign-in..."
	LoginDMOnly              = "Sign in from a direct message with icebath."
	LoginBusyElsewhere       = "Another sign-in is already running on this icebath. Try again in a few minutes."
	LoginNotAllowed          = "Only an operator of this icebath can sign it in."
	FreeTierRateLimitCard    = "You've used up the allowance for chatting without signing in. It refreshes in {reset}. " +
		"Sign in for a bigger allowance, it's free."
	FreeTierRateLimitChat = FreeTierRateLimitCard + " To sign in: /login."
)

func FormatWaitLine(expiresIn int) string {
	n := max(1, int(math.Ceil(float64(max(0, expiresIn))/60)))
	minutes := fmt.Sprintf("%d minutes", n)
	if n == 1 {
		minutes = "1 minute"
	}
	return strings.ReplaceAll(UpgradeWaitingUpTo, "{minutes}", minutes)
}

type SignInKind string

const (
	SignInCode            SignInKind = "code"
	SignInWaiting         SignInKind = "waiting"
	SignInCompleted       SignInKind = "completed"
	SignInCancelled       SignInKind = "cancelled"
	SignInTimedOut        SignInKind = "timed_out"
	SignInFailed          SignInKind = "failed"
	SignInAlreadySignedIn SignInKind = "already_signed_in"
)

type SignInState struct {
	Kind          SignInKind
	Link          string
	Code          string
	ExpiresIn     int
	Interval      int
	Email         string
	Model         string
	ModelChanged  bool
	Handoff       []string
	HandoffStatus *HandoffStatus
	Reason        string
	Detail        string
	RetryAfter    time.Duration
}

func (s SignInState) Terminal() bool {
	return s.Kind != SignInCode && s.Kind != SignInWaiting
}

func (s SignInState) OK() bool {
	return s.Kind == SignInCompleted || s.Kind == SignInAlreadySignedIn
}

func (s SignInState) Precondition() bool {
	return s.Kind == SignInAlreadySignedIn
}

func (s SignInState) Retryable() bool {
	return s.Kind == SignInFailed && strings.HasPrefix(s.Reason, "anon_") && !AnonTerminal(s.Reason)
}

func (s SignInState) completedLines(noDefault string) string {
	lines := []string{"Signed in."}
	if s.Email != "" {
		lines[0] = fmt.Sprintf("Signed in as %s.", s.Email)
	}
	lines = append(lines, s.Handoff...)
	if s.ModelChanged {
		if s.Model != "" {
			lines = append(lines, fmt.Sprintf("Default model is now %s.", s.Model))
		} else {
			lines = append(lines, noDefault)
		}
	}
	return strings.Join(lines, "\n")
}

func (s SignInState) Copy() string {
	switch s.Kind {
	case SignInCode:
		return UpgradeDoNotShare
	case SignInWaiting:
		return UpgradeWaiting
	case SignInCompleted:
		return s.completedLines(UpgradeNoDefaultChat)
	case SignInCancelled:
		return strings.TrimSpace(UpgradeCancelled)
	case SignInTimedOut:
		return UpgradeTimedOut
	case SignInAlreadySignedIn:
		return UpgradeAlreadySignedIn
	case SignInFailed:
		if AnonUnreachableCode(s.Reason) {
			return UpgradeServiceUnreachable
		}
		if s.Reason == AnonRateLimited || s.Reason == AnonGatePaused {
			wait := s.RetryAfter
			if wait <= 0 {
				wait = 60 * time.Second
			}
			return strings.ReplaceAll(UpgradeServiceBusy, "{wait}", FriendlyWait(wait))
		}
		if _, ok := anonFailureTemplate(s.Reason); ok {
			return AnonFailureCopy(s.Reason, s.RetryAfter)
		}
		return UpgradeNotCompleted
	}
	return ""
}

func (s SignInState) CopyTerminal() string {
	switch s.Kind {
	case SignInCompleted:
		return s.completedLines(UpgradeNoDefaultTerminal)
	case SignInFailed:
		if text := s.Copy(); s.Reason != "" && text != UpgradeNotCompleted {
			return text
		}
		if s.Detail != "" {
			return "Sign-in failed: " + s.Detail
		}
		return UpgradeNotCompleted
	}
	return s.Copy()
}

func (s SignInState) CopyWithWait() string {
	return UpgradeDoNotShare + " " + FormatWaitLine(s.ExpiresIn)
}

func failedFromError(err error) SignInState {
	classified := classifyMintError(err)
	ae, _ := authErrorOf(classified)
	_, wasAuth := authErrorOf(err)
	if ae.Code == AnonServerError && !wasAuth {
		return SignInState{Kind: SignInFailed, Detail: err.Error()}
	}
	return SignInState{Kind: SignInFailed, Reason: ae.Code, Detail: err.Error(), RetryAfter: ae.RetryAfter}
}

func accountEmail(state *object, fallback string) string {
	if email := claimsOf(state.get("access_token")).get("email"); truthy(email) {
		return strings.TrimSpace(textual(email))
	}
	return strings.TrimSpace(fallback)
}

type SignInOptions struct {
	Timeout        time.Duration
	Cancelled      func() bool
	ReplaceAccount bool
	PersistGuard   func(save func() error) (bool, error)
}

func (a *Account) RunSignIn(ctx context.Context, o SignInOptions) iter.Seq[SignInState] {
	return func(yield func(SignInState) bool) {
		ctx := a.bind(ctx)
		cancelled := o.Cancelled
		if cancelled == nil {
			cancelled = func() bool { return false }
		}
		timeout := o.Timeout
		if timeout <= 0 {
			timeout = defaultTimeout
		}
		state := a.quietState(ctx)
		lookup := state
		if lookup == nil {
			lookup = newObject()
		}
		portalBaseURL := a.portalBaseURL(ctx, lookup)
		if state.size() > 0 && !isGuestState(state) && !o.ReplaceAccount {
			yield(SignInState{Kind: SignInAlreadySignedIn})
			return
		}
		accountState, stopped, final := a.signInGrant(ctx, portalBaseURL, timeout, cancelled, yield)
		if stopped {
			return
		}
		if final != nil {
			yield(*final)
			return
		}
		guard := o.PersistGuard
		if guard == nil {
			guard = func(save func() error) (bool, error) {
				if cancelled() {
					return false, nil
				}
				return true, save()
			}
		}
		completed, err := a.signInPersist(ctx, guard, accountState, state)
		if err != nil {
			yield(SignInState{Kind: SignInFailed, Detail: err.Error()})
			return
		}
		if completed == nil {
			yield(SignInState{Kind: SignInCancelled})
			return
		}
		yield(*completed)
	}
}

func (a *Account) signInGrant(ctx context.Context, portalBaseURL string, timeout time.Duration, cancelled func() bool, yield func(SignInState) bool) (*object, bool, *SignInState) {
	fail := func(err error) (*object, bool, *SignInState) {
		var expired *auth.DeviceCodeExpiredError
		var final SignInState
		switch {
		case errors.Is(err, auth.ErrDeviceFlowCancelled):
			final = SignInState{Kind: SignInCancelled}
		case errors.As(err, &expired) && expired.Cause != nil:
			final = SignInState{Kind: SignInFailed, Reason: AnonUnreachable, Detail: expired.Cause.Error()}
		case errors.As(err, &expired), auth.IsDeviceCodeExpired(err):
			final = SignInState{Kind: SignInTimedOut, Detail: err.Error()}
		default:
			final = failedFromError(err)
		}
		return nil, false, &final
	}
	clientID, scope := auth.DefaultPortalClientID, auth.DefaultPortalScope
	client, tls, err := a.verifiedClient(ctx, nil, "", nil)
	if err != nil {
		return fail(err)
	}
	deviceRaw, err := a.store.RequestPortalDeviceCode(ctx, client, portalBaseURL, clientID, scope, timeout)
	if err != nil {
		return fail(err)
	}
	device := objectOrEmpty(deviceRaw)
	expires, ok := intOf(device.get("expires_in"))
	if !ok {
		return fail(errors.New("Device code response carried a non-numeric expires_in"))
	}
	expiresIn, interval := int(expires), 5
	if v := device.get("interval"); truthy(v) {
		every, ok := intOf(v)
		if !ok {
			return fail(errors.New("Device code response carried a non-numeric interval"))
		}
		interval = int(every)
	}
	deadline := time.Now().Add(time.Duration(max(1, expiresIn)) * time.Second)
	for _, key := range []string{"verification_uri_complete", "user_code"} {
		if !device.has(key) {
			return fail(fmt.Errorf("Device code response carried no %s", key))
		}
	}
	if !yield(SignInState{Kind: SignInCode, Link: textual(device.get("verification_uri_complete")), Code: textual(device.get("user_code")),
		ExpiresIn: expiresIn, Interval: interval}) {
		return nil, true, nil
	}
	if cancelled() {
		return nil, false, &SignInState{Kind: SignInCancelled}
	}
	if !yield(SignInState{Kind: SignInWaiting}) {
		return nil, true, nil
	}
	if !device.has("device_code") {
		return fail(errors.New("Device code response carried no device_code"))
	}
	remaining := max(1, int(time.Until(deadline).Seconds()))
	tokenRaw, err := a.store.PollPortalToken(ctx, client, portalBaseURL, clientID, textual(device.get("device_code")),
		time.Duration(remaining)*time.Second, time.Duration(interval)*time.Second, timeout, cancelled)
	if err != nil {
		return fail(err)
	}
	tokenData := objectOrEmpty(tokenRaw)
	if !tokenData.has("access_token") {
		return fail(errors.New("Token response carried no access_token"))
	}
	accountState, err := a.accountStateFromToken(ctx, tokenData, portalBaseURL, clientID, scope, tls, timeout)
	if err != nil {
		return fail(err)
	}
	return accountState, false, nil
}

func (a *Account) signInPersist(ctx context.Context, guard func(save func() error) (bool, error), accountState, replaced *object) (*SignInState, error) {
	saved, err := guard(func() error {
		_, _, err := a.persistCredentials(ctx, accountState, "")
		return err
	})
	if err != nil || !saved {
		return nil, err
	}
	handoff, err := a.Drain(ctx)
	if err != nil {
		return nil, err
	}
	model, changed := a.settleAfterUpgrade(ctx, accountState)
	completed := &SignInState{Kind: SignInCompleted, Model: model, ModelChanged: changed, Handoff: []string{}}
	fallbackEmail := ""
	for _, view := range handoff {
		completed.Handoff = append(completed.Handoff, a.ResultLines(view)...)
		if fallbackEmail == "" {
			fallbackEmail = view.AccountEmail
		}
	}
	switch {
	case len(handoff) > 0:
		wire := a.WireStatus(handoff[len(handoff)-1])
		completed.HandoffStatus = &wire
	case isGuestState(replaced) && replaced.str("anon_token") != "":
		status, ok, err := a.HandoffRecordStatus(auth.NewSecret(replaced.str("anon_token")))
		if err != nil {
			return nil, err
		}
		if ok {
			wire := a.WireStatus(status)
			completed.HandoffStatus = &wire
		}
	}
	completed.Email = accountEmail(accountState, fallbackEmail)
	return completed, nil
}

func (a *Account) accountStateFromToken(ctx context.Context, tokenData *object, portalBaseURL, clientID, scope string, tls auth.TLSState, timeout time.Duration) (*object, error) {
	state := a.loginState(tokenData, portalBaseURL, strings.TrimRight(auth.DefaultPortalInferenceURL, "/"), clientID, scope, tls)
	state, err := a.refreshFromState(ctx, state, timeout, false, nil)
	if err != nil {
		return nil, err
	}
	state.set("auth_method", UpgradedAuthMethod)
	return state, nil
}

func (a *Account) loginState(tokenData *object, portalBaseURL, requestedInferenceURL, clientID, scope string, tls auth.TLSState) *object {
	now := time.Now()
	ttl := int64(0)
	if tokenData.has("expires_in") {
		ttl = coerceTTL(tokenData.get("expires_in"))
	}
	inferenceURL := optionalBaseURL(tokenData.get("inference_base_url"))
	if inferenceURL == "" {
		inferenceURL = requestedInferenceURL
	}
	var grantedScope any = scope
	if v := tokenData.get("scope"); truthy(v) {
		grantedScope = v
	}
	var tokenType any = "Bearer"
	if tokenData.has("token_type") {
		tokenType = tokenData.get("token_type")
	}
	state := pairs(
		"portal_base_url", portalBaseURL, "inference_base_url", inferenceURL,
		"client_id", clientID, "scope", grantedScope, "token_type", tokenType,
		"access_token", tokenData.get("access_token"), "refresh_token", tokenData.get("refresh_token"),
		"obtained_at", isoUTC(now), "expires_at", isoAfter(now, ttl), "expires_in", ttl,
		"tls", tlsObject(tls),
	)
	for _, k := range emptyAgentKeyFields {
		state.set(k, nil)
	}
	return state
}

func (a *Account) PersistCredentials(ctx context.Context, creds json.RawMessage, label string) (auth.Entry, bool, error) {
	state, ok := parseObject(creds)
	if !ok {
		return auth.Entry{}, false, errors.New("portal: credentials to persist are not a JSON object")
	}
	return a.persistCredentials(a.bind(ctx), state, label)
}

func (a *Account) persistCredentials(ctx context.Context, creds *object, label string) (auth.Entry, bool, error) {
	state := creds.clone()
	if strings.TrimSpace(label) != "" {
		state.set("label", strings.TrimSpace(label))
	}
	if err := a.store.SaveActive(ctx, Provider, state.raw()); err != nil {
		return auth.Entry{}, false, err
	}
	a.writeShared(ctx, state, false)
	pool, err := a.store.Pool(ctx, Provider)
	if err != nil {
		return auth.Entry{}, false, err
	}
	for _, e := range pool.Entries() {
		if e.Source == auth.PortalDeviceCodeSource {
			return e, true, nil
		}
	}
	return auth.Entry{}, false, nil
}

func (a *Account) SettleAfterUpgrade(ctx context.Context, accountState json.RawMessage) (model string, changed bool) {
	return a.settleAfterUpgrade(a.bind(ctx), objectOrEmpty(accountState))
}

func (a *Account) settleAfterUpgrade(ctx context.Context, accountState *object) (string, bool) {
	cfg, err := a.cfg.LoadReadonly(ctx)
	if err != nil {
		slog.Warn("sign-in completion: config unreadable, default model left as is", "error", err)
		return "", false
	}
	current := strings.TrimSpace(cfg.Model.Default.ID)
	onWelcomeModel := current == GuestModel
	onWelcomeHost := RouteIsWelcomeHost(cfg.Model.BaseURL)
	if !onWelcomeModel && !onWelcomeHost {
		return current, false
	}
	model := current
	if onWelcomeModel {
		model, err = a.recommended(ctx)
		if err != nil {
			slog.Debug("sign-in completion: recommended default unavailable", "error", err)
			model = ""
		}
	}
	update := ConfigUpdate{Provider: Provider, BaseURL: textualOrEmpty(accountState.get("inference_base_url"))}
	if onWelcomeModel {
		update.DefaultModel, update.ClearDefault = model, model == ""
	}
	if _, err := a.update(ctx, update); err != nil {
		slog.Warn("sign-in completion: could not update the default model", "error", err)
		return current, false
	}
	return model, true
}
