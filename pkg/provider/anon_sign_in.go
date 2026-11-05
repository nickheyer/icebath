package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"time"
)

const (
	UpgradeStart              = "Sign in with a Nous account to unlock more models and tools."
	UpgradeAlreadySignedIn    = "Already signed in."
	UpgradeDoNotShare         = "Do not share this code."
	UpgradeTimedOut           = "That sign-in link has expired. Start again whenever you're ready."
	UpgradeNotCompleted       = "Sign-in didn't finish. Try again whenever you're ready."
	UpgradeServiceBusy        = "Signing in couldn't finish because the Nous service is busy. Try again in {wait}. Your session is still here in the meantime."
	UpgradeServiceUnreachable = "The Nous service couldn't be reached to finish signing you in. Check your internet connection and try again. Your session is still here."
	UpgradeNoDefaultTerminal  = "No default model is set yet; run `icebath model` to pick one."
	UpgradeNoDefaultChat      = "No default model is set yet; run /model to pick one."
	UpgradeWaiting            = "Waiting for sign-in..."
	UpgradeWaitingUpTo        = "Waiting for sign-in, up to {minutes}."
	UpgradeCancelled          = "\nSign-in cancelled."
	LoginSlashCommand         = "/login"
	LoginStarting             = "Starting sign-in..."
	LoginDMOnly               = "Sign in from a direct message with Icebath."
	LoginBusyElsewhere        = "Another sign-in is already running on this Icebath. Try again in a few minutes."
	LoginNotAllowed           = "Only an operator of this Icebath can sign it in."
)

// The up to N minutes line with N the expiry rounded up to whole minutes, singular at one
func FormatWaitLine(expiresIn int) string {
	n := max(1, int(math.Ceil(float64(max(0, expiresIn))/60)))
	minutes := fmt.Sprintf("%d minutes", n)
	if n == 1 {
		minutes = "1 minute"
	}
	return strings.ReplaceAll(UpgradeWaitingUpTo, "{minutes}", minutes)
}

// SignInState is one step of a sign in carrying its own chat and terminal copy
type SignInState interface {
	Kind() string
	Terminal() bool
	OK() bool
	Precondition() bool
	Copy() string
	CopyTerminal() string
}

// SignInCode is the device page link and code with the clock the caller may show
type SignInCode struct {
	Link      string
	Code      string
	ExpiresIn int
	Interval  int
}

func (SignInCode) Kind() string           { return "code" }
func (SignInCode) Terminal() bool         { return false }
func (SignInCode) OK() bool               { return false }
func (SignInCode) Precondition() bool     { return false }
func (SignInCode) Copy() string           { return UpgradeDoNotShare }
func (s SignInCode) CopyTerminal() string { return s.Copy() }

// The do not share line followed by the honest wait line
func (s SignInCode) CopyWithWait() string {
	return UpgradeDoNotShare + " " + FormatWaitLine(s.ExpiresIn)
}

// SignInWaiting is the token poll in progress
type SignInWaiting struct{}

func (SignInWaiting) Kind() string           { return "waiting" }
func (SignInWaiting) Terminal() bool         { return false }
func (SignInWaiting) OK() bool               { return false }
func (SignInWaiting) Precondition() bool     { return false }
func (SignInWaiting) Copy() string           { return UpgradeWaiting }
func (s SignInWaiting) CopyTerminal() string { return s.Copy() }

// SignInCompleted is a saved sign in with the handoff lines and the settled default model
type SignInCompleted struct {
	Email         string
	Model         string
	ModelChanged  bool
	Handoff       []string
	HandoffStatus map[string]any
}

func (SignInCompleted) Kind() string           { return "completed" }
func (SignInCompleted) Terminal() bool         { return true }
func (SignInCompleted) OK() bool               { return true }
func (SignInCompleted) Precondition() bool     { return false }
func (s SignInCompleted) Copy() string         { return s.lines(UpgradeNoDefaultChat) }
func (s SignInCompleted) CopyTerminal() string { return s.lines(UpgradeNoDefaultTerminal) }

func (s SignInCompleted) lines(noDefault string) string {
	first := "Signed in."
	if s.Email != "" {
		first = fmt.Sprintf("Signed in as %s.", s.Email)
	}
	lines := append([]string{first}, s.Handoff...)
	if s.ModelChanged {
		if s.Model != "" {
			lines = append(lines, fmt.Sprintf("Default model is now %s.", s.Model))
		} else {
			lines = append(lines, noDefault)
		}
	}
	return strings.Join(lines, "\n")
}

// SignInCancelled is a sign in stopped from outside before anything was saved
type SignInCancelled struct{}

func (SignInCancelled) Kind() string           { return "cancelled" }
func (SignInCancelled) Terminal() bool         { return true }
func (SignInCancelled) OK() bool               { return false }
func (SignInCancelled) Precondition() bool     { return false }
func (SignInCancelled) Copy() string           { return strings.TrimSpace(UpgradeCancelled) }
func (s SignInCancelled) CopyTerminal() string { return s.Copy() }

// SignInTimedOut is a code that ran out, Detail holding the enriched device auth guidance
type SignInTimedOut struct{ Detail string }

func (SignInTimedOut) Kind() string           { return "timed_out" }
func (SignInTimedOut) Terminal() bool         { return true }
func (SignInTimedOut) OK() bool               { return false }
func (SignInTimedOut) Precondition() bool     { return false }
func (SignInTimedOut) Copy() string           { return UpgradeTimedOut }
func (s SignInTimedOut) CopyTerminal() string { return s.Copy() }

// SignInFailed keeps the service verdict code, the raw detail and the wait it asked for in seconds
type SignInFailed struct {
	Reason     string
	Detail     string
	RetryAfter float64
}

func (SignInFailed) Kind() string       { return "failed" }
func (SignInFailed) Terminal() bool     { return true }
func (SignInFailed) OK() bool           { return false }
func (SignInFailed) Precondition() bool { return false }

// A later attempt can succeed when the verdict is a non terminal free tier code
func (s SignInFailed) Retryable() bool {
	return strings.HasPrefix(s.Reason, "anon_") && !AnonTerminalCodes[s.Reason]
}

func (s SignInFailed) Copy() string {
	if AnonUnreachableCodes[s.Reason] {
		return UpgradeServiceUnreachable
	}
	if s.Reason == AnonRateLimited || s.Reason == AnonGatePaused {
		wait := s.RetryAfter
		if wait == 0 {
			wait = 60
		}
		return strings.ReplaceAll(UpgradeServiceBusy, "{wait}", friendlyWaitSeconds(wait))
	}
	if _, ok := AnonFailureCopy[s.Reason]; ok {
		return anonFailureCopy(s.Reason, time.Duration(s.RetryAfter*float64(time.Second)), true)
	}
	return UpgradeNotCompleted
}

func (s SignInFailed) CopyTerminal() string {
	if s.Reason != "" && s.Copy() != UpgradeNotCompleted {
		return s.Copy()
	}
	if s.Detail != "" {
		return "Sign-in failed: " + s.Detail
	}
	return UpgradeNotCompleted
}

// SignInAlreadySignedIn is the precondition refusal for an install already signed in
type SignInAlreadySignedIn struct{}

func (SignInAlreadySignedIn) Kind() string           { return "already_signed_in" }
func (SignInAlreadySignedIn) Terminal() bool         { return true }
func (SignInAlreadySignedIn) OK() bool               { return true }
func (SignInAlreadySignedIn) Precondition() bool     { return true }
func (SignInAlreadySignedIn) Copy() string           { return UpgradeAlreadySignedIn }
func (s SignInAlreadySignedIn) CopyTerminal() string { return s.Copy() }

// A failure keeping the service code and wait, an unnamed local failure left on the generic copy
func signInFailedFromError(err error) SignInFailed {
	classified := anonAuthErrorOf(classifyMintError(err))
	reason := ""
	retryAfter := 0.0
	if classified != nil {
		reason = classified.Code
		if classified.HasRetryAfter {
			retryAfter = classified.RetryAfter.Seconds()
		}
	}
	var authErr *AuthError
	if reason == AnonServerError && anonAuthErrorOf(err) == nil && !errors.As(err, &authErr) {
		return SignInFailed{Detail: err.Error()}
	}
	return SignInFailed{Reason: reason, Detail: err.Error(), RetryAfter: retryAfter}
}

// The state a device flow or token grant error ends the sign in with
func signInStateFromError(err error) SignInState {
	var cancelled *DeviceFlowCancelledError
	if errors.As(err, &cancelled) {
		return SignInCancelled{}
	}
	var expired *DeviceCodeExpiredError
	if errors.As(err, &expired) {
		if expired.Cause != nil {
			return SignInFailed{Reason: AnonUnreachable, Detail: expired.Cause.Error()}
		}
		return SignInTimedOut{Detail: err.Error()}
	}
	if IsDeviceCodeExpired(err) {
		return SignInTimedOut{Detail: err.Error()}
	}
	return signInFailedFromError(err)
}

// The signed in account's email from its own access token, else the fallback
func signInAccountEmail(accountState map[string]any, fallback string) string {
	email := DecodeJWTClaims(anyString(accountState["access_token"]))["email"]
	if pyTruthy(email) {
		return strings.TrimSpace(fmt.Sprint(email))
	}
	return strings.TrimSpace(fallback)
}

func signInHandoffLines(results []map[string]any) []string {
	lines := []string{}
	for _, view := range results {
		lines = append(lines, guestHandoffResultLines(view)...)
	}
	return lines
}

// The latest handoff this run attempted, else the replaced free tier's saved record, else nil
func signInHandoffStatus(env PoolEnv, results []map[string]any, replaced map[string]any) (map[string]any, error) {
	if len(results) > 0 {
		return guestHandoffWireStatus(results[len(results)-1]), nil
	}
	if !isGuestState(replaced) {
		return nil, nil
	}
	anonToken, _ := replaced["anon_token"].(string)
	if anonToken == "" {
		return nil, nil
	}
	status, err := guestHandoffRecordStatus(env, anonToken)
	if err != nil || status == nil {
		return nil, err
	}
	return guestHandoffWireStatus(status), nil
}

// SignInOptions are the cancel hook, the account replacement switch and the surface seams of one sign in
type SignInOptions struct {
	Timeout        time.Duration
	Cancelled      func() bool
	ReplaceAccount bool
	// Runs body with whether the save may happen, under the surface's own lock
	PersistGuard func(body func(mayPersist bool) error) error
	// Entered around the precondition and the persist, never across a yield
	Scope         func(body func() error) error
	ClientFactory func(timeout time.Duration, verify oauthVerify) *http.Client
}

// Settles the config after a completed sign in, replaced in tests
var signInSettleFn = settleAfterUpgradeResult

// Runs body inside the scope, a panic from the scope, guard, persist or settle becoming its error
func signInGuarded(scope func(body func() error) error, body func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return scope(body)
}

// Signs in with a Nous account through the device code flow, ending with exactly one terminal state
func RunSignIn(env PoolEnv, opts SignInOptions) iter.Seq[SignInState] {
	return func(yield func(SignInState) bool) {
		timeout := opts.Timeout
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		isCancelled := opts.Cancelled
		if isCancelled == nil {
			isCancelled = func() bool { return false }
		}
		scope := opts.Scope
		if scope == nil {
			scope = func(body func() error) error { return body() }
		}
		var state map[string]any
		var portal string
		if err := signInGuarded(scope, func() error {
			state = currentNousState(env)
			portal = nousPortalBaseURLIn(env, state)
			return nil
		}); err != nil {
			yield(SignInFailed{Detail: err.Error()})
			return
		}
		if len(state) > 0 && !isGuestState(state) && !opts.ReplaceAccount {
			yield(SignInAlreadySignedIn{})
			return
		}
		clientID, scopeStr := DefaultNousClientID, DefaultNousScope
		verify := resolveOAuthVerify(env, nil, "", nil)
		factory := opts.ClientFactory
		if factory == nil {
			factory = func(t time.Duration, v oauthVerify) *http.Client { return nousHTTPClientFn(env, t, v) }
		}
		client := factory(timeout, verify)
		ctx := env.ctx()
		device, err := requestDeviceCode(ctx, client, portal, clientID, scopeStr)
		if err == nil {
			err = signInDeviceFields(device)
		}
		if err != nil {
			yield(signInStateFromError(err))
			return
		}
		expiresIn, _ := anyInt(device["expires_in"])
		interval, _ := anyInt(device["interval"])
		if interval == 0 {
			interval = 5
		}
		deadline := time.Now().Add(time.Duration(max(1, expiresIn)) * time.Second)
		if !yield(SignInCode{Link: fmt.Sprint(device["verification_uri_complete"]), Code: fmt.Sprint(device["user_code"]), ExpiresIn: expiresIn, Interval: interval}) {
			return
		}
		if isCancelled() {
			yield(SignInCancelled{})
			return
		}
		if !yield(SignInWaiting{}) {
			return
		}
		remaining := max(1, int(time.Until(deadline).Seconds()))
		tokenData, err := pollForToken(ctx, client, portal, clientID, fmt.Sprint(device["device_code"]), remaining, interval, isCancelled)
		var accountState map[string]any
		if err == nil {
			accountState, err = accountStateFromToken(env, tokenData, portal, clientID, scopeStr, verify, timeout)
		}
		if err != nil {
			yield(signInStateFromError(err))
			return
		}
		guard := opts.PersistGuard
		if guard == nil {
			guard = func(body func(bool) error) error { return body(!isCancelled()) }
		}
		var mayPersist bool
		var handoff []map[string]any
		var model string
		var changed bool
		var handoffStatus map[string]any
		if err := signInGuarded(scope, func() error {
			if err := guard(func(may bool) error {
				mayPersist = may
				if !may {
					return nil
				}
				_, _, err := persistNousCredentials(env, accountState, "")
				return err
			}); err != nil {
				return err
			}
			if !mayPersist {
				return nil
			}
			var err error
			if handoff, err = guestHandoffDrain(env); err != nil {
				return err
			}
			model, changed = signInSettleFn(env, accountState)
			handoffStatus, err = signInHandoffStatus(env, handoff, state)
			return err
		}); err != nil {
			yield(SignInFailed{Detail: err.Error()})
			return
		}
		if !mayPersist {
			yield(SignInCancelled{})
			return
		}
		fallback := ""
		for _, view := range handoff {
			if pyTruthy(view["account_email"]) {
				fallback = fmt.Sprint(view["account_email"])
				break
			}
		}
		yield(SignInCompleted{Email: signInAccountEmail(accountState, fallback), Model: model, ModelChanged: changed,
			Handoff: signInHandoffLines(handoff), HandoffStatus: handoffStatus})
	}
}

// The device answer must carry every field the sign in reads
func signInDeviceFields(device map[string]any) error {
	for _, key := range []string{"expires_in", "verification_uri_complete", "user_code", "device_code"} {
		if _, ok := device[key]; !ok {
			return fmt.Errorf("device code response is missing %q", key)
		}
	}
	if _, ok := anyInt(device["expires_in"]); !ok {
		return fmt.Errorf("device code response has a non numeric expires_in %v", device["expires_in"])
	}
	return nil
}

// Prints the link, the code and the do not share line of a code state
func RenderSignInCLICode(out io.Writer, state SignInCode, openBrowser, chat bool) error {
	if err := printDeviceCodeInstructions(out, state.Link, state.Code, openBrowser, "--", true); err != nil {
		return err
	}
	copy := state.Copy()
	if chat {
		copy = state.CopyWithWait()
	}
	_, err := fmt.Fprintf(out, "  %s\n", copy)
	return err
}

// Runs states to the terminal one and returns its copy, an erroring terminal hook never losing it
func DrainSignInCopy(states iter.Seq[SignInState], chat bool, onTerminal func(SignInState) error) string {
	copy := ""
	for state := range states {
		if !state.Terminal() {
			continue
		}
		if chat {
			copy = state.Copy()
		} else {
			copy = state.CopyTerminal()
		}
		if onTerminal != nil {
			func() {
				defer func() { _ = recover() }()
				_ = onTerminal(state)
			}()
		}
	}
	return copy
}

// RenderSignInOptions are the terminal sign in renderer's switches and seams
type RenderSignInOptions struct {
	Timeout     time.Duration
	OpenBrowser bool
	Chat        bool
	// A partially drained sign in to finish instead of starting one
	States iter.Seq[SignInState]
	Out    io.Writer
}

// Interrupt source for the terminal renderer, replaced in tests
var signInInterrupts = func() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	return ch, func() { signal.Stop(ch) }
}

// Prints a sign in and returns the exit code, 0 ok, 1 not, 130 interrupted
func RenderSignInCLI(env PoolEnv, opts RenderSignInOptions) int {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	printer := func(line string) { fmt.Fprintln(out, line) }
	var interrupted atomic.Bool
	parent := env.ctx()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	signals, stopSignals := signInInterrupts()
	defer stopSignals()
	go func() {
		select {
		case <-signals:
			interrupted.Store(true)
			cancel()
		case <-ctx.Done():
		}
	}()
	states := opts.States
	if states == nil {
		scoped := env
		scoped.Ctx = ctx
		states = RunSignIn(scoped, SignInOptions{Timeout: opts.Timeout, Cancelled: interrupted.Load})
	}
	started := false
	code := 1
	for state := range states {
		if interrupted.Load() {
			break
		}
		if !started && !state.Precondition() {
			printer(UpgradeStart)
			started = true
		}
		if codeState, ok := state.(SignInCode); ok {
			if err := RenderSignInCLICode(out, codeState, opts.OpenBrowser, opts.Chat); err != nil {
				printer(err.Error())
			}
			continue
		}
		if state.Kind() == "waiting" {
			if !opts.Chat {
				printer(state.Copy())
			}
			continue
		}
		if state.Terminal() {
			if opts.Chat {
				printer(state.Copy())
			} else {
				printer(state.CopyTerminal())
			}
			if state.OK() {
				code = 0
			}
			break
		}
	}
	if interrupted.Load() {
		printer(UpgradeCancelled)
		return 130
	}
	return code
}

// UpgradeGuestArgs are the auth upgrade command's flags
type UpgradeGuestArgs struct {
	Timeout   time.Duration
	NoBrowser bool
}

// Signs in with a Nous account after retrying any unfinished handoff, returning the exit code
func UpgradeGuest(env PoolEnv, args UpgradeGuestArgs, out io.Writer) (int, error) {
	if _, err := guestHandoffDrain(env); err != nil {
		return 1, err
	}
	timeout := args.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	openBrowser := !args.NoBrowser && !isRemoteSession()
	return RenderSignInCLI(env, RenderSignInOptions{Timeout: timeout, OpenBrowser: openBrowser, Out: out}), nil
}
