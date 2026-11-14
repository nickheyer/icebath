package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/model/auth"
)

const (
	BrowserCapability = "browser-v1"
	ChallengeEvent    = "free_tier.challenge"
	ChallengeCopy     = "Icebath Portal needs to run a quick check before starting your free session."

	challengeWait       = 120 * time.Second
	challengePollMin    = time.Second
	challengePollMax    = 5 * time.Second
	challengeExpiresMin = 30.0
	challengeExpiresMax = 900.0
	challengeExpiresDef = 600.0
	challengePollClient = 10 * time.Second
	serverMessageMax    = 300
	gaveUpCooldown      = 30 * time.Second
	clientName          = "icebath"
)

type BrowserChallenge struct {
	URL       string
	Required  bool
	ExpiresIn time.Duration
	Interval  time.Duration
	Message   string
	Attempt   int
}

type ChallengeStatus struct {
	Type      string `json:"type"`
	URL       string `json:"url"`
	Required  bool   `json:"required"`
	ExpiresIn int    `json:"expires_in"`
	Message   string `json:"message"`
	Attempt   int    `json:"attempt"`
}

func (c BrowserChallenge) Status() ChallengeStatus {
	return ChallengeStatus{Type: "browser", URL: c.URL, Required: c.Required, ExpiresIn: int(c.ExpiresIn.Seconds()), Message: c.Message, Attempt: c.Attempt}
}

type ChallengeRequiredError struct {
	Err           *auth.Error
	Challenge     BrowserChallenge
	portalBaseURL string
	anonToken     auth.Secret
	tls           auth.TLSState
}

func (e *ChallengeRequiredError) Error() string { return e.Err.Error() }

func (e *ChallengeRequiredError) Unwrap() error { return e.Err }

type challengeRecord struct {
	pending     *BrowserChallenge
	deadline    time.Time
	outcome     *string
	gaveUpUntil time.Time
}

type backgroundKey struct{}

func WithBackground(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
}

func isBackground(ctx context.Context) bool {
	v, _ := ctx.Value(backgroundKey{}).(bool)
	return v
}

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
	if n := utf8.RuneCountInString(text); n == 0 || n > serverMessageMax {
		return ""
	}
	return text
}

func signinRequiredError(message any) *auth.Error {
	text := serverMessage(message)
	if text == "" {
		text = AnonFailureCopy(AnonSigninRequired, 0)
	}
	return &auth.Error{Message: text, Provider: Provider, Code: AnonSigninRequired, Retryable: boolPtr(false)}
}

func (a *Account) ClientSurface(ctx context.Context) Surface {
	return a.surface(a.bind(ctx))
}

func (a *Account) clientInfo(ctx context.Context) *object {
	return pairs("name", clientName, "version", a.version, "surface", string(a.surface(ctx)), "platform", platformName(),
		"capabilities", []any{BrowserCapability})
}

func (a *Account) userAgent(ctx context.Context) string {
	return fmt.Sprintf("%s/%s (%s; %s; %s)", clientName, a.version, a.surface(ctx), platformName(), a.machine())
}

func splitOrigin(raw string) (scheme, netloc string) {
	raw = strings.TrimLeftFunc(raw, func(r rune) bool { return r <= ' ' })
	i := strings.Index(raw, ":")
	if i <= 0 {
		return "", ""
	}
	for j, r := range raw[:i] {
		letter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !letter && (j == 0 || !((r >= '0' && r <= '9') || r == '+' || r == '-' || r == '.')) {
			return "", ""
		}
	}
	scheme = strings.ToLower(raw[:i])
	rest := raw[i+1:]
	if !strings.HasPrefix(rest, "//") {
		return scheme, ""
	}
	rest = rest[2:]
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		rest = rest[:end]
	}
	return scheme, rest
}

func sameOrigin(target, portalBaseURL string) bool {
	scheme, netloc := splitOrigin(target)
	portalScheme, portalNetloc := splitOrigin(portalBaseURL)
	return (scheme == "https" || scheme == "http") && scheme == portalScheme && netloc != "" &&
		strings.ToLower(netloc) == strings.ToLower(portalNetloc)
}

func finiteNumber(value any, def float64) float64 {
	if f, ok := numeric(value); ok {
		return f
	}
	return def
}

func allPrintable(s string) bool {
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func parseBrowserChallenge(payload *object, portalBaseURL string) (BrowserChallenge, bool) {
	entries, ok := payload.get("challenges").([]any)
	if !ok {
		return BrowserChallenge{}, false
	}
	message := serverMessage(payload.get("message"))
	if message == "" {
		message = ChallengeCopy
	}
	for _, item := range entries {
		entry, ok := item.(*object)
		if !ok || entry.str("type") != "browser" {
			continue
		}
		target, isString := entry.get("url").(string)
		if !isString || !allPrintable(target) || !sameOrigin(target, portalBaseURL) {
			slog.Info("Icebath Portal free tier: ignoring a challenge URL outside the portal origin")
			continue
		}
		expires := min(challengeExpiresMax, max(challengeExpiresMin, finiteNumber(entry.get("expires_in"), challengeExpiresDef)))
		interval := min(challengePollMax.Seconds(), max(challengePollMin.Seconds(), finiteNumber(entry.get("interval"), 2.0)))
		return BrowserChallenge{
			URL: target, Required: entry.get("required") != false,
			ExpiresIn: time.Duration(expires * float64(time.Second)),
			Interval:  time.Duration(interval * float64(time.Second)),
			Message:   message,
		}, true
	}
	return BrowserChallenge{}, false
}

func challengeError(payload *object, portalBaseURL, anonToken string, tls auth.TLSState) error {
	challenge, ok := parseBrowserChallenge(payload, portalBaseURL)
	if !ok {
		return signinRequiredError(nil)
	}
	return &ChallengeRequiredError{
		Err:       &auth.Error{Message: challenge.Message, Provider: Provider, Code: AnonChallengeRequired, Retryable: boolPtr(true)},
		Challenge: challenge, portalBaseURL: portalBaseURL, anonToken: auth.NewSecret(anonToken), tls: tls,
	}
}

func (a *Account) liveChallenge() *BrowserChallenge {
	if a.challenge.pending != nil && !a.challenge.deadline.After(time.Now()) {
		a.challenge.pending, a.challenge.outcome = nil, nil
	}
	return a.challenge.pending
}

func (a *Account) PendingChallenge() (ChallengeStatus, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	challenge := a.liveChallenge()
	if challenge == nil {
		return ChallengeStatus{}, false
	}
	if outcome := a.challenge.outcome; outcome != nil && *outcome != "timeout" && *outcome != "error" {
		return ChallengeStatus{}, false
	}
	status := challenge.Status()
	status.ExpiresIn = int(math.Ceil(time.Until(a.challenge.deadline).Seconds()))
	return status, true
}

func (a *Account) clearPending() {
	a.mu.Lock()
	a.challenge.pending, a.challenge.outcome = nil, nil
	a.mu.Unlock()
}

func (a *Account) recordChallenge(challenge BrowserChallenge, newAttempt bool) BrowserChallenge {
	a.mu.Lock()
	defer a.mu.Unlock()
	previous := a.liveChallenge()
	deadline := time.Now().Add(challenge.ExpiresIn)
	recorded := challenge
	if previous == nil || previous.URL != challenge.URL {
		recorded.Attempt = 0
		a.challenge.pending, a.challenge.deadline, a.challenge.outcome = &recorded, deadline, nil
		return recorded
	}
	recorded.Attempt = previous.Attempt
	if newAttempt {
		recorded.Attempt++
	}
	if deadline.After(a.challenge.deadline) {
		deadline = a.challenge.deadline
	}
	a.challenge.pending, a.challenge.deadline = &recorded, deadline
	if newAttempt {
		a.challenge.outcome = nil
	}
	return recorded
}

func (a *Account) RecordHostOutcome(target string, attempt int, outcome string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	challenge := a.liveChallenge()
	if challenge == nil || challenge.URL != target || challenge.Attempt != attempt {
		return false
	}
	a.challenge.outcome = &outcome
	return true
}

func (a *Account) hostOutcome(challenge BrowserChallenge) *string {
	a.mu.Lock()
	defer a.mu.Unlock()
	pending := a.challenge.pending
	if pending == nil || pending.URL != challenge.URL || pending.Attempt != challenge.Attempt {
		return nil
	}
	return a.challenge.outcome
}

func (a *Account) giveUp() {
	a.mu.Lock()
	a.challenge.gaveUpUntil = time.Now().Add(gaveUpCooldown)
	a.mu.Unlock()
}

func (a *Account) gaveUpRecently() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Now().Before(a.challenge.gaveUpUntil)
}

func (a *Account) announce(challenge BrowserChallenge) bool {
	if a.events == nil {
		return false
	}
	payload, err := json.Marshal(challenge.Status())
	if err == nil {
		err = a.events.Broadcast(ChallengeEvent, payload)
	}
	if err != nil {
		slog.Debug(ChallengeEvent+" not broadcast", "error", err)
		return false
	}
	return true
}

func (a *Account) presentInTerminal(ctx context.Context, challenge BrowserChallenge) {
	a.mu.Lock()
	alreadyOpen := a.opened[challenge.URL]
	a.mu.Unlock()
	opened := false
	if !alreadyOpen && a.surface(ctx) != SurfaceGateway && !a.store.IsRemoteSession(ctx) && a.store.CanOpenGraphicalBrowser(ctx) {
		opened = a.openBrowser(ctx, challenge.URL)
		if opened {
			a.mu.Lock()
			a.opened[challenge.URL] = true
			a.mu.Unlock()
		}
	}
	a.notify("\n" + challenge.Message)
	a.notify("  Open: " + challenge.URL)
	if opened || alreadyOpen {
		a.notify("  (Opened in your browser.)\n")
	} else {
		a.notify("  (Open that link in any browser; this continues on its own.)\n")
	}
}

func (a *Account) present(ctx context.Context, challenge BrowserChallenge) {
	if a.surface(ctx) == SurfaceDesktop && a.announce(challenge) {
		return
	}
	if !challenge.Required || isBackground(ctx) {
		return
	}
	if a.events != nil && a.events.StdioRPC() && a.announce(challenge) {
		return
	}
	a.presentInTerminal(ctx, challenge)
}

func (a *Account) pollChallengeStatus(ctx context.Context, client *http.Client, portalBaseURL string, anonToken auth.Secret) string {
	r, err := postJSON(ctx, client, strings.TrimRight(portalBaseURL, "/")+"/api/anonymous/challenge/status", a.anonHeaders(ctx),
		pairs("token", anonToken.Reveal()), challengePollClient)
	if err != nil {
		slog.Debug("challenge status poll failed", "error", fmt.Sprintf("%T", err))
		return "pending"
	}
	if r.status >= 400 && r.status < 500 && r.status != 429 {
		return "none"
	}
	if r.status != 200 {
		return "pending"
	}
	body, ok := parseObject(r.body)
	if !ok {
		return "pending"
	}
	status, ok := body.get("status").(string)
	if !ok {
		return "pending"
	}
	return status
}

func challengeWaiting(status string) bool {
	return status == "pending" || status == "needs_interaction"
}

func (a *Account) waitForChallenge(ctx context.Context, exc *ChallengeRequiredError) (bool, error) {
	if a.gaveUpRecently() {
		return false, nil
	}
	challenge := exc.Challenge
	deadline := time.Now().Add(min(challengeWait, challenge.ExpiresIn))
	client, _, err := a.store.VerifiedClient(ctx, nil, "", exc.tls)
	if err != nil {
		return false, err
	}
	if !challengeWaiting(a.pollChallengeStatus(ctx, client, exc.portalBaseURL, exc.anonToken)) {
		return true, nil
	}
	challenge = a.recordChallenge(challenge, true)
	exc.Challenge = challenge
	a.present(ctx, challenge)
	toldInteractive := false
	for time.Now().Before(deadline) {
		if a.hostOutcome(challenge) != nil {
			return true, nil
		}
		timer := time.NewTimer(challenge.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
		status := a.pollChallengeStatus(ctx, client, exc.portalBaseURL, exc.anonToken)
		if !challengeWaiting(status) {
			return true, nil
		}
		if status == "needs_interaction" && !toldInteractive {
			toldInteractive = true
			if a.surface(ctx) != SurfaceDesktop {
				a.notify("  Finish the quick check in your browser to continue.")
			}
		}
	}
	a.giveUp()
	return false, nil
}

func (a *Account) stillPending(cause *ChallengeRequiredError) error {
	message := AnonFailureCopy(AnonChallengeRequired, 0)
	if outcome := a.hostOutcome(cause.Challenge); outcome != nil && *outcome != "" {
		message = "The quick check couldn't finish. Try again."
	}
	return &auth.Error{Message: message, Provider: Provider, Code: AnonChallengeRequired, Retryable: boolPtr(true), Cause: cause}
}

func runWithChallenge[T any](ctx context.Context, a *Account, exchange func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	out, err := exchange(ctx)
	var first *ChallengeRequiredError
	if !errors.As(err, &first) {
		return out, err
	}
	if isBackground(ctx) || a.surface(ctx) == SurfaceGateway {
		a.present(ctx, a.recordChallenge(first.Challenge, false))
		return zero, a.stillPending(first)
	}
	a.workMu.Lock()
	_, werr := a.waitForChallenge(ctx, first)
	a.workMu.Unlock()
	if werr != nil {
		return zero, werr
	}
	out, err = exchange(ctx)
	var again *ChallengeRequiredError
	if errors.As(err, &again) {
		again.Challenge = a.recordChallenge(again.Challenge, false)
		a.giveUp()
		return zero, a.stillPending(again)
	}
	if ae, ok := authErrorOf(err); ok && !retryableOf(ae) {
		a.clearPending()
	}
	return out, err
}

func (a *Account) noteOptionalChallenges(ctx context.Context, payload *object, portalBaseURL string) {
	challenge, ok := parseBrowserChallenge(payload, portalBaseURL)
	if !ok {
		a.clearPending()
		return
	}
	if !challenge.Required && a.surface(ctx) == SurfaceDesktop {
		a.announce(a.recordChallenge(challenge, false))
	}
}
