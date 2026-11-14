package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pywebbrowser"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	BrowserCapability   = "browser-v1"
	ChallengeEvent      = "free_tier.challenge"
	ChallengeCopy       = "Nous needs to run a quick check before starting your free session."
	challengePollMin    = time.Second
	challengePollMax    = 5 * time.Second
	challengeExpiresMin = 30 * time.Second
	challengeExpiresMax = 900 * time.Second
	challengeExpiresDef = 600.0
	challengeMessageMax = 300
	challengeGaveUpFor  = 30 * time.Second
)

// BrowserChallenge is one browser-v1 check the account service put in front of a token exchange
type BrowserChallenge struct {
	URL       string
	Required  bool
	ExpiresIn time.Duration
	Interval  time.Duration
	Message   string
	Attempt   int
}

// Wire payload of the challenge event and status field
func (c BrowserChallenge) AsPayload() map[string]any {
	return map[string]any{"type": "browser", "url": c.URL, "required": c.Required,
		"expires_in": int(c.ExpiresIn.Seconds()), "message": c.Message, "attempt": c.Attempt}
}

// AnonChallengeRequiredError carries the challenge plus the portal, credential and auth state to poll with
type AnonChallengeRequiredError struct {
	Auth          *AuthError
	Challenge     BrowserChallenge
	PortalBaseURL string
	AnonToken     string
	AuthState     map[string]any
}

func (e *AnonChallengeRequiredError) Error() string { return e.Auth.Message }

func (e *AnonChallengeRequiredError) Unwrap() error { return e.Auth }

// FreeTierGatewayHooks are the gateway runtime probes, a nil field meaning that gateway module is not loaded
type FreeTierGatewayHooks struct {
	OwnsRuntimeLock   func() bool
	BroadcastGlobal   func(event string, payload map[string]any) error
	StdioIsRPCChannel func() bool
}

var (
	freeTierGatewayMu sync.RWMutex
	freeTierGateway   FreeTierGatewayHooks
)

// SetFreeTierGatewayHooks installs the messaging gateway and TUI gateway probes for this process
func SetFreeTierGatewayHooks(h FreeTierGatewayHooks) {
	freeTierGatewayMu.Lock()
	defer freeTierGatewayMu.Unlock()
	freeTierGateway = h
}

func freeTierGatewayHooks() FreeTierGatewayHooks {
	freeTierGatewayMu.RLock()
	defer freeTierGatewayMu.RUnlock()
	return freeTierGateway
}

// Terminal sign in error for a client the service refuses without an account
func signinRequiredError(message any) *AuthError {
	text := serverMessage(message)
	if text == "" {
		text = AnonFailureCopy[AnonSigninRequired]
	}
	retryable := false
	return &AuthError{Message: text, Provider: "nous", Code: AnonSigninRequired, Retryable: &retryable}
}

// Service copy when it is a short printable single paragraph, else empty
func serverMessage(value any) string {
	s, ok := value.(string)
	if !ok {
		return ""
	}
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return ' '
	}, s)
	text := strings.Join(strings.Fields(cleaned), " ")
	if n := utf8.RuneCountInString(text); n > 0 && n <= challengeMessageMax {
		return text
	}
	return ""
}

// Every rune of s is printable the way Python str.isprintable reads it
func isPrintableString(s string) bool {
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// Which surface this process is, desktop then gateway then the desktop spawned backend else cli
func clientSurface(env PoolEnv) string {
	if tools.ServesLocalDesktopNow(env.ctx()) {
		return "desktop"
	}
	if hooks := freeTierGatewayHooks(); hooks.OwnsRuntimeLock != nil && hooks.OwnsRuntimeLock() {
		return "gateway"
	}
	if tools.IsDesktopOwnedBackend(nil) {
		return "desktop"
	}
	return "cli"
}

// Base version of the install stamp or build identity, unknown when neither carries one
func anonBaseVersion() string {
	info, err := updaterecord.GetVersionInfo()
	if err != nil {
		panic(err)
	}
	return info.BaseVersion
}

// Machine name the way uname reports it for the running binary's architecture
func anonMachine() string {
	switch runtime.GOARCH {
	case "amd64":
		if runtime.GOOS == "windows" {
			return "AMD64"
		}
		return "x86_64"
	case "arm64":
		switch runtime.GOOS {
		case "windows":
			return "ARM64"
		case "linux", "android":
			return "aarch64"
		}
		return "arm64"
	case "386":
		if runtime.GOOS == "windows" {
			return "x86"
		}
		return "i686"
	case "":
		return "unknown"
	}
	return runtime.GOARCH
}

// Self reported client block on a token exchange
func clientInfo(env PoolEnv) map[string]any {
	return map[string]any{"name": "icebath-agent", "version": anonBaseVersion(), "surface": clientSurface(env),
		"platform": hostinfo.OSFamily(), "capabilities": []string{BrowserCapability}}
}

// User agent naming the version, surface, platform and machine
func anonUserAgent(env PoolEnv) string {
	return fmt.Sprintf("icebath-agent/%s (%s; %s; %s)", anonBaseVersion(), clientSurface(env), hostinfo.OSFamily(), anonMachine())
}

// Network location including any userinfo, the way urlparse reports netloc
func urlNetloc(u *url.URL) string {
	if u.User != nil {
		return u.User.String() + "@" + u.Host
	}
	return u.Host
}

// A challenge URL is only opened on the portal origin the exchange itself went to
func sameOrigin(raw, portalBaseURL string) bool {
	target, err := url.Parse(raw)
	if err != nil {
		return false
	}
	portal, err := url.Parse(portalBaseURL)
	if err != nil {
		return false
	}
	return (target.Scheme == "https" || target.Scheme == "http") && target.Scheme == portal.Scheme &&
		urlNetloc(target) != "" && strings.EqualFold(urlNetloc(target), urlNetloc(portal))
}

// Finite JSON number else the default, booleans refused
func challengeNumber(v any, def float64) float64 {
	n, ok := jsonNumber(v)
	if !ok || math.IsInf(n, 0) || math.IsNaN(n) {
		return def
	}
	return n
}

// Seconds clamped into the bounds as a duration
func clampSeconds(v float64, lo, hi time.Duration) time.Duration {
	return time.Duration(math.Min(hi.Seconds(), math.Max(lo.Seconds(), v)) * float64(time.Second))
}

// First browser challenge in the payload this client can run, nil when none
func parseBrowserChallenge(env PoolEnv, payload map[string]any, portalBaseURL string) *BrowserChallenge {
	entries, ok := payload["challenges"].([]any)
	if !ok {
		return nil
	}
	message := serverMessage(payload["message"])
	if message == "" {
		message = ChallengeCopy
	}
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok || entry["type"] != "browser" {
			continue
		}
		target, ok := entry["url"].(string)
		if !ok || !isPrintableString(target) || !sameOrigin(target, portalBaseURL) {
			env.log().Info("Nous free tier: ignoring a challenge URL outside the portal origin")
			continue
		}
		return &BrowserChallenge{
			URL: target, Required: entry["required"] != false,
			ExpiresIn: clampSeconds(challengeNumber(entry["expires_in"], challengeExpiresDef), challengeExpiresMin, challengeExpiresMax),
			Interval:  clampSeconds(challengeNumber(entry["interval"], 2.0), challengePollMin, challengePollMax),
			Message:   message,
		}
	}
	return nil
}

// Error for a 428 challenge_required, the challenge to work or the sign in fallback
func challengeError(env PoolEnv, payload map[string]any, portalBaseURL, anonToken string, authState map[string]any) error {
	challenge := parseBrowserChallenge(env, payload, portalBaseURL)
	if challenge == nil {
		return signinRequiredError(nil)
	}
	retryable := true
	return &AnonChallengeRequiredError{
		Auth:      &AuthError{Message: challenge.Message, Provider: "nous", Code: AnonChallengeRequired, Retryable: &retryable},
		Challenge: *challenge, PortalBaseURL: portalBaseURL, AnonToken: anonToken, AuthState: authState,
	}
}

// One profile's challenge state, its work lock serialising waits per profile
type profileChallenge struct {
	workLock    sync.Mutex
	pending     *BrowserChallenge
	deadline    time.Time
	outcome     string
	gaveUpUntil time.Time
}

var (
	challengeStateMu    sync.Mutex
	challengeProfiles             = map[string]*profileChallenge{}
	challengeOpened               = map[string]bool{}
	challengeSleep                = time.Sleep
	challengeWait                 = 120 * time.Second
	presentChallengeFn            = presentChallenge
	announceChallengeFn           = announceChallenge
	presentInTerminalFn           = presentChallengeInTerminal
	challengeStderr     io.Writer = os.Stderr
)

type backgroundCallerKey struct{}

// Marks the enclosed token reads as background work that announces a challenge but never waits on it
func withBackgroundCaller(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, backgroundCallerKey{}, true)
}

func isBackgroundCaller(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(backgroundCallerKey{}).(bool)
	return v
}

// This profile's challenge record
func challengeProfile(env PoolEnv) *profileChallenge {
	key := mintMemoKey(env)
	challengeStateMu.Lock()
	defer challengeStateMu.Unlock()
	record, ok := challengeProfiles[key]
	if !ok {
		record = &profileChallenge{}
		challengeProfiles[key] = record
	}
	return record
}

// Pending challenge still inside its deadline, expiring it otherwise, called under the state lock
func (r *profileChallenge) live() *BrowserChallenge {
	if r.pending != nil && !anonMonotonic().Before(r.deadline) {
		r.pending, r.outcome = nil, ""
	}
	return r.pending
}

// PendingChallenge is the challenge this profile waits on for a status read, nil once settled or closed
func PendingChallenge(env PoolEnv) map[string]any {
	record := challengeProfile(env)
	challengeStateMu.Lock()
	defer challengeStateMu.Unlock()
	challenge := record.live()
	if challenge == nil || (record.outcome != "" && record.outcome != "timeout" && record.outcome != "error") {
		return nil
	}
	payload := challenge.AsPayload()
	payload["expires_in"] = int(math.Ceil(record.deadline.Sub(anonMonotonic()).Seconds()))
	return payload
}

func clearPendingChallenge(env PoolEnv) {
	record := challengeProfile(env)
	challengeStateMu.Lock()
	defer challengeStateMu.Unlock()
	record.pending, record.outcome = nil, ""
}

// Records the profile's pending challenge, a replay keeping its deadline and a new attempt reopening it
func recordChallenge(env PoolEnv, challenge BrowserChallenge, newAttempt bool) BrowserChallenge {
	record := challengeProfile(env)
	challengeStateMu.Lock()
	defer challengeStateMu.Unlock()
	previous := record.live()
	deadline := anonMonotonic().Add(challenge.ExpiresIn)
	if previous == nil || previous.URL != challenge.URL {
		fresh := challenge
		fresh.Attempt = 0
		record.pending, record.deadline, record.outcome = &fresh, deadline, ""
	} else {
		attempt := previous.Attempt
		if newAttempt {
			attempt++
		}
		next := challenge
		next.Attempt = attempt
		record.pending = &next
		if deadline.Before(record.deadline) {
			record.deadline = deadline
		}
		if newAttempt {
			record.outcome = ""
		}
	}
	return *record.pending
}

// RecordHostOutcome stores how the host window ended for the attempt that opened it
func RecordHostOutcome(env PoolEnv, rawURL string, attempt int, outcome string) bool {
	record := challengeProfile(env)
	challengeStateMu.Lock()
	defer challengeStateMu.Unlock()
	challenge := record.live()
	if challenge == nil || challenge.URL != rawURL || challenge.Attempt != attempt {
		return false
	}
	record.outcome = outcome
	return true
}

// Host outcome recorded for exactly this challenge attempt, empty when none
func challengeHostOutcome(env PoolEnv, challenge BrowserChallenge) string {
	record := challengeProfile(env)
	challengeStateMu.Lock()
	defer challengeStateMu.Unlock()
	pending := record.pending
	if pending != nil && pending.URL == challenge.URL && pending.Attempt == challenge.Attempt {
		return record.outcome
	}
	return ""
}

func challengeGiveUp(env PoolEnv) {
	record := challengeProfile(env)
	challengeStateMu.Lock()
	defer challengeStateMu.Unlock()
	record.gaveUpUntil = anonMonotonic().Add(challengeGaveUpFor)
}

func challengeGaveUpRecently(env PoolEnv) bool {
	record := challengeProfile(env)
	challengeStateMu.Lock()
	defer challengeStateMu.Unlock()
	return anonMonotonic().Before(record.gaveUpUntil)
}

func resetChallengeForTests() {
	challengeStateMu.Lock()
	defer challengeStateMu.Unlock()
	challengeProfiles = map[string]*profileChallenge{}
	challengeOpened = map[string]bool{}
}

// Broadcasts the challenge to connected desktop clients, false when no gateway server is loaded or it failed
func announceChallenge(env PoolEnv, challenge BrowserChallenge) bool {
	hooks := freeTierGatewayHooks()
	if hooks.BroadcastGlobal == nil {
		return false
	}
	if err := hooks.BroadcastGlobal(ChallengeEvent, challenge.AsPayload()); err != nil {
		env.log().Debug(fmt.Sprintf("%s not broadcast: %v", ChallengeEvent, err))
		return false
	}
	return true
}

// Prints the challenge link and opens one browser tab per ticket where a graphical browser exists
func presentChallengeInTerminal(env PoolEnv, challenge BrowserChallenge) {
	challengeStateMu.Lock()
	alreadyOpen := challengeOpened[challenge.URL]
	challengeStateMu.Unlock()
	opened := false
	if !alreadyOpen && clientSurface(env) != "gateway" && !isRemoteSession() && canOpenGraphicalBrowser() {
		var browserErr *pywebbrowser.Error
		switch err := openBrowserFn(challenge.URL); {
		case err == nil:
			opened = true
		case errors.Is(err, errBrowserNotOpened):
		case errors.As(err, &browserErr), pypath.IsOSError(err):
			env.log().Debug(fmt.Sprintf("could not open the challenge in a browser: %v", err))
		default:
			panic(err)
		}
		if opened {
			challengeStateMu.Lock()
			challengeOpened[challenge.URL] = true
			challengeStateMu.Unlock()
		}
	}
	fmt.Fprintf(challengeStderr, "\n%s\n", challenge.Message)
	fmt.Fprintf(challengeStderr, "  Open: %s\n", challenge.URL)
	if opened || alreadyOpen {
		fmt.Fprint(challengeStderr, "  (Opened in your browser.)\n\n")
	} else {
		fmt.Fprint(challengeStderr, "  (Open that link in any browser; this continues on its own.)\n\n")
	}
}

// Gets the challenge URL in front of something that can load it for the current surface
func presentChallenge(env PoolEnv, challenge BrowserChallenge) {
	if clientSurface(env) == "desktop" && announceChallengeFn(env, challenge) {
		return
	}
	if !challenge.Required || isBackgroundCaller(env.ctx()) {
		return
	}
	hooks := freeTierGatewayHooks()
	if hooks.BroadcastGlobal != nil && hooks.StdioIsRPCChannel != nil && hooks.StdioIsRPCChannel() && announceChallengeFn(env, challenge) {
		return
	}
	presentInTerminalFn(env, challenge)
}

// One status read, only a 200 naming a status being a verdict and a refused poll ending the wait
func pollChallengeStatus(env PoolEnv, client *http.Client, portalBaseURL, anonToken string) string {
	resp, err := anonPost(env, client, strings.TrimRight(portalBaseURL, "/")+"/api/anonymous/challenge/status", map[string]any{"token": anonToken})
	if err != nil {
		env.log().Debug(fmt.Sprintf("challenge status poll failed: %v", err))
		return "pending"
	}
	if resp.Status >= 400 && resp.Status < 500 && resp.Status != 429 {
		return "none"
	}
	if resp.Status != 200 {
		return "pending"
	}
	if status, ok := resp.Payload["status"].(string); ok {
		return status
	}
	return "pending"
}

func challengeWaiting(status string) bool {
	return status == "pending" || status == "needs_interaction"
}

// Presents the challenge and polls until a verdict or host hint ends the wait, false when patience ran out
func waitForChallenge(env PoolEnv, exc *AnonChallengeRequiredError) bool {
	if challengeGaveUpRecently(env) {
		return false
	}
	challenge := exc.Challenge
	deadline := anonMonotonic().Add(min(challengeWait, challenge.ExpiresIn))
	verify := resolveOAuthVerify(env, nil, "", exc.AuthState)
	client := nousHTTPClientFn(env, 10*time.Second, verify)
	toldInteractive := false
	if !challengeWaiting(pollChallengeStatus(env, client, exc.PortalBaseURL, exc.AnonToken)) {
		return true
	}
	challenge = recordChallenge(env, challenge, true)
	exc.Challenge = challenge
	presentChallengeFn(env, challenge)
	for anonMonotonic().Before(deadline) {
		if challengeHostOutcome(env, challenge) != "" {
			return true
		}
		challengeSleep(challenge.Interval)
		status := pollChallengeStatus(env, client, exc.PortalBaseURL, exc.AnonToken)
		if !challengeWaiting(status) {
			return true
		}
		if status == "needs_interaction" && !toldInteractive {
			toldInteractive = true
			if clientSurface(env) != "desktop" {
				fmt.Fprintln(challengeStderr, "  Finish the quick check in your browser to continue.")
			}
		}
	}
	challengeGiveUp(env)
	return false
}

// Retryable error for a challenge still pending, naming a failed check when the host reported one
func challengeStillPending(env PoolEnv, cause *AnonChallengeRequiredError) error {
	message := AnonFailureCopy[AnonChallengeRequired]
	if challengeHostOutcome(env, cause.Challenge) != "" {
		message = "The quick check couldn't finish. Try again."
	}
	retryable := true
	return &AuthError{Message: message, Provider: "nous", Code: AnonChallengeRequired, Retryable: &retryable, Err: cause}
}

// Runs the exchange, working one browser challenge outside every lock and exchanging once more
func runWithChallenge(env PoolEnv, fn func() (string, error)) (string, error) {
	out, err := fn()
	var first *AnonChallengeRequiredError
	if err == nil || !errors.As(err, &first) {
		return out, err
	}
	if isBackgroundCaller(env.ctx()) || clientSurface(env) == "gateway" {
		presentChallengeFn(env, recordChallenge(env, first.Challenge, false))
		return "", challengeStillPending(env, first)
	}
	record := challengeProfile(env)
	record.workLock.Lock()
	waitForChallenge(env, first)
	record.workLock.Unlock()
	out, err = fn()
	if err == nil {
		return out, nil
	}
	var again *AnonChallengeRequiredError
	if errors.As(err, &again) {
		again.Challenge = recordChallenge(env, again.Challenge, false)
		challengeGiveUp(env)
		return "", challengeStillPending(env, again)
	}
	var authErr *AuthError
	if errors.As(err, &authErr) && (authErr.Retryable == nil || !*authErr.Retryable) {
		clearPendingChallenge(env)
	}
	return "", err
}

// Announces an optional challenge of a successful exchange to a desktop client, clearing a settled one
func noteOptionalChallenges(env PoolEnv, payload map[string]any, portalBaseURL string) {
	challenge := parseBrowserChallenge(env, payload, portalBaseURL)
	if challenge == nil {
		clearPendingChallenge(env)
		return
	}
	if !challenge.Required && clientSurface(env) == "desktop" {
		announceChallengeFn(env, recordChallenge(env, *challenge, false))
	}
}
