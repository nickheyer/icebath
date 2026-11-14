package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Reset windows shorter than this are upstream jitter rather than a breaker worthy exhaustion
const minResetForBreaker = 60 * time.Second

// WelcomeLongWait splits a welcome tier rate_limited refusal into exhausted allowance or a quiet wait
const WelcomeLongWait = 20 * time.Second

// Guest model served by the Nous welcome host
const GuestModel = "nous/welcome"

// AnonAccountTier is the JWT account_tier claim of a welcome tier credential
const AnonAccountTier = "anonymous"

// ExtraWelcomeHostsEnv names comma separated extra hostnames that count as the welcome host
const ExtraWelcomeHostsEnv = "ICEBATH_EXTRA_WELCOME_HOSTS"

var welcomeHosts = map[string]bool{"welcome-api.nousresearch.com": true}

// WelcomeHosts returns the stock welcome host plus any env configured extras lowercased
func WelcomeHosts() map[string]bool {
	out := map[string]bool{}
	for h := range welcomeHosts {
		out[h] = true
	}
	for _, part := range strings.Split(os.Getenv(ExtraWelcomeHostsEnv), ",") {
		if p := strings.ToLower(strings.TrimSpace(part)); p != "" {
			out[p] = true
		}
	}
	return out
}

// RouteIsWelcomeHost reports whether a base URL points at the free tier host
func RouteIsWelcomeHost(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	return WelcomeHosts()[strings.ToLower(u.Hostname())]
}

// IsAnonymousRequest reports a Nous request sent with a welcome tier JWT
func IsAnonymousRequest(provider, apiKey string) bool {
	if provider != "nous" || apiKey == "" {
		return false
	}
	tier, _ := DecodeJWTClaims(apiKey)["account_tier"].(string)
	return tier == AnonAccountTier
}

// Fairshare refusal reasons the welcome tier answers with
var WelcomeRefusalReasons = setOf("model_not_free", "feature_not_free", "at_capacity", "admission_closed", "rate_limited")

// Reasons meaning never on this tier so only a sign in or another provider helps
var WelcomeTierGateReasons = setOf("model_not_free", "feature_not_free")

// WelcomeRefusal is the structured welcome tier refusal in a gateway 429 body
type WelcomeRefusal struct {
	Reason     string   `json:"reason"`
	RetryAfter int      `json:"retry_after"`
	Alternates []string `json:"alternates"`
	UpgradeURL string   `json:"upgrade_url"`
}

// ParseWelcomeRefusal reads a structured welcome tier refusal body or reports false for other shapes
func ParseWelcomeRefusal(body map[string]any) (WelcomeRefusal, bool) {
	reason, ok := body["reason"].(string)
	if !ok || !WelcomeRefusalReasons[reason] {
		return WelcomeRefusal{}, false
	}
	out := WelcomeRefusal{Reason: reason, Alternates: []string{}}
	switch r := body["retry_after"].(type) {
	case float64:
		if r > 0 {
			out.RetryAfter = int(r)
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(r), 64); err == nil && f > 0 {
			out.RetryAfter = int(f)
		}
	}
	if alts, ok := body["alternates"].([]any); ok {
		for _, a := range alts {
			if s, ok := a.(string); ok && s != "" {
				out.Alternates = append(out.Alternates, s)
			}
		}
	}
	out.UpgradeURL, _ = body["upgrade_url"].(string)
	return out, true
}

var welcomeRouteRefusals = []struct{ needle, kind string }{
	{"anonymous accounts must use", "anon_on_paid_host"},
	{"serves anonymous icebath agent accounts only", "named_on_welcome_host"},
	{"serves anonymous hermes agent accounts only", "named_on_welcome_host"},
	{"anonymous accounts are not accepted", "tier_disabled"},
	{"invalid jwt", "session_expired"},
}

// WelcomeRouteRefusal names the host cross refusal of a gateway 400 or 403 or returns empty
func WelcomeRouteRefusal(status int, message, baseURL string) string {
	if status != 400 && status != 403 {
		return ""
	}
	text := strings.ToLower(message)
	for _, r := range welcomeRouteRefusals {
		if strings.Contains(text, r.needle) {
			return r.kind
		}
	}
	if status == 403 && baseURL != "" && RouteIsWelcomeHost(baseURL) {
		return "tier_disabled"
	}
	return ""
}

const (
	signinChat        = "To sign in: /login."
	signinTerminal    = "To sign in: `icebath auth upgrade`."
	modelHintChat     = "Run /model and pick the Nous row again."
	modelHintTerminal = "Run `icebath model` and pick the Nous row again."
)

// FreeTierOutageCopy is the terminal copy once free model retries are spent
const FreeTierOutageCopy = "The free model is having trouble responding right now. Try sending your message again in a minute."

// FriendlyWait renders a wait as a rounded spoken duration, never a raw second count
func FriendlyWait(seconds float64) string {
	s := math.Max(0, seconds)
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

// WelcomeRefusalCopy is the user sentence for a structured welcome tier refusal
func WelcomeRefusalCopy(r WelcomeRefusal, model string, inChat, door bool) string {
	signin := ""
	if door {
		signin = signinTerminal
		if inChat {
			signin = signinChat
		}
	}
	serves := GuestModel
	if len(r.Alternates) > 0 {
		serves = r.Alternates[0]
	}
	wait := "a little while"
	if r.RetryAfter > 0 {
		wait = FriendlyWait(float64(r.RetryAfter))
	}
	var s string
	switch r.Reason {
	case "model_not_free":
		what := "That model isn't"
		if model != "" {
			what = model + " isn't"
		}
		s = fmt.Sprintf("%s available without signing in, so Icebath uses %s instead. Sign in for more models. %s", what, serves, signin)
	case "feature_not_free":
		s = "That isn't available without signing in. Sign in to use it, it's free. " + signin
	case "at_capacity":
		s = fmt.Sprintf("Chatting without signing in is really busy right now. Sign in to skip the queue, it's free, or try again in %s. %s", wait, signin)
	case "admission_closed":
		s = fmt.Sprintf("Chatting without signing in is full right now. Sign in to keep going, it's free, or try again in %s. %s", wait, signin)
	case "rate_limited":
		s = fmt.Sprintf("You've used up the allowance for chatting without signing in. It refreshes in %s. Sign in for a bigger allowance, it's free. %s", wait, signin)
	default:
		s = "Icebath couldn't send that without signing in. Signing in is free. " + signin
	}
	return strings.TrimRight(s, " ")
}

var welcomeRouteCopy = map[string]string{
	"anon_on_paid_host":     "This install is set to use a different Nous server (NOUS_INFERENCE_BASE_URL). Unset it to use the free model, or sign in. {signin}",
	"named_on_welcome_host": "This Nous account needs to reconnect. {model_hint}",
	"tier_disabled":         "Using Icebath without signing in is switched off right now. Sign in to keep chatting, it's free. {signin}",
	"session_expired":       "Icebath couldn't renew its connection to the free model. Send your message again, or sign in to keep chatting, it's free. {signin}",
}

// WelcomeRouteRefusalCopy is the user sentence for a host cross refusal kind
func WelcomeRouteRefusalCopy(kind string, inChat, door bool) string {
	tmpl, ok := welcomeRouteCopy[kind]
	if !ok {
		tmpl = "Icebath couldn't reach the free model on this route."
	}
	signin, hint := "", modelHintTerminal
	if inChat {
		hint = modelHintChat
	}
	if door {
		signin = signinTerminal
		if inChat {
			signin = signinChat
		}
	}
	r := strings.NewReplacer("{signin}", signin, "{model_hint}", hint, "{host}", DefaultNousWelcomeURL)
	return strings.TrimRight(r.Replace(tmpl), " ")
}

// NousRateGuard shares Nous rate limit state across sessions through a file under the home
type NousRateGuard struct {
	Home      string
	Anonymous bool
	Now       func() time.Time
}

func (g *NousRateGuard) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *NousRateGuard) statePath() string {
	name := "nous.json"
	if g.Anonymous {
		name = "nous-anonymous.json"
	}
	return filepath.Join(g.Home, "rate_limits", name)
}

type nousRateState struct {
	ResetAt      float64 `json:"reset_at"`
	RecordedAt   float64 `json:"recorded_at"`
	ResetSeconds float64 `json:"reset_seconds"`
}

func parseResetSeconds(h http.Header) (time.Duration, bool) {
	lowered := LowerHeaders(h)
	for _, key := range []string{"x-ratelimit-reset-requests-1h", "x-ratelimit-reset-requests"} {
		if f, ok := safeFloat(lowered[key]); ok && f > 0 {
			return time.Duration(f * float64(time.Second)), true
		}
	}
	if d, ok := ParseRetryAfter(lowered["retry-after"]); ok && d > 0 {
		return d, true
	}
	return 0, false
}

// Record writes the shared cooldown from the body reset, then headers, then the default cooldown
func (g *NousRateGuard) Record(h http.Header, resetAt time.Time, defaultCooldown time.Duration) error {
	now := g.now()
	at := time.Time{}
	if !resetAt.IsZero() && resetAt.After(now) {
		at = resetAt
	}
	if at.IsZero() {
		if d, ok := parseResetSeconds(h); ok {
			at = now.Add(d)
		}
	}
	if at.IsZero() {
		at = now.Add(defaultCooldown)
	}
	st := nousRateState{ResetAt: float64(at.UnixNano()) / 1e9, RecordedAt: float64(now.UnixNano()) / 1e9, ResetSeconds: at.Sub(now).Seconds()}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(g.statePath()), 0o700); err != nil {
		return err
	}
	if err := config.AtomicWrite(g.statePath(), raw, 0o600); err != nil {
		return err
	}
	nousRateGuardLog().Info(fmt.Sprintf("Nous rate limit recorded: resets in %.0fs (at %.0f)", st.ResetSeconds, st.ResetAt))
	return nil
}

func nousRateGuardLog() *slog.Logger { return slog.Default().With("logger", "agent.nous_rate_guard") }

// Reset a 429 context carries as a Python int or float or as the classifier instant, zero for any other value
func ContextResetAt(errorContext map[string]any) time.Time {
	if at, ok := errorContext["reset_at"].(time.Time); ok {
		return at
	}
	epoch, ok := pyNumber(errorContext["reset_at"])
	if !ok {
		return time.Time{}
	}
	sec, frac := math.Modf(epoch)
	return time.Unix(int64(sec), int64(frac*1e9))
}

// Logs a cooldown write that failed, the way the guard swallows it
func LogNousRateLimitWriteFailure(err error) {
	nousRateGuardLog().Debug(fmt.Sprintf("Failed to write Nous rate limit state: %s", err))
}

// Remaining returns the time until the shared reset and removes an expired file
func (g *NousRateGuard) Remaining() (time.Duration, bool) {
	raw, err := os.ReadFile(g.statePath())
	if err != nil {
		return 0, false
	}
	var st nousRateState
	if err := json.Unmarshal([]byte(strings.TrimPrefix(string(raw), "\xef\xbb\xbf")), &st); err != nil {
		return 0, false
	}
	remaining := time.Unix(0, int64(st.ResetAt*1e9)).Sub(g.now())
	if remaining > 0 {
		return remaining, true
	}
	_ = os.Remove(g.statePath())
	return 0, false
}

// Clear removes the shared state after a successful Nous request, logging and returning any other failure
func (g *NousRateGuard) Clear() error {
	err := os.Remove(g.statePath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		nousRateGuardLog().Debug(fmt.Sprintf("Failed to clear Nous rate limit state: %s", pypath.OSErrorText(err)+": "+pyrepr.StrRepr(g.statePath())))
		return err
	}
	return nil
}

func isExhausted(remaining int, hasRemaining bool, reset time.Duration, hasReset bool) bool {
	return hasRemaining && remaining <= 0 && hasReset && reset >= minResetForBreaker
}

type bucketProbe struct {
	remaining    int
	hasRemaining bool
	reset        time.Duration
	hasReset     bool
}

func parseBucketsFromHeaders(h http.Header) map[string]bucketProbe {
	lowered := LowerHeaders(h)
	out := map[string]bucketProbe{}
	if !HasRateLimitHeaders(lowered) {
		return out
	}
	for _, bt := range bucketTags {
		remaining, hasRemaining := safeInt(lowered["x-ratelimit-remaining-"+bt.tag])
		resetF, hasReset := safeFloat(lowered["x-ratelimit-reset-"+bt.tag])
		if hasRemaining || hasReset {
			out[bt.tag] = bucketProbe{remaining, hasRemaining, time.Duration(resetF * float64(time.Second)), hasReset}
		}
	}
	return out
}

func hasExhaustedBucket(buckets map[string]bucketProbe) bool {
	for _, b := range buckets {
		if isExhausted(b.remaining, b.hasRemaining, b.reset, b.hasReset) {
			return true
		}
	}
	return false
}

func hasExhaustedBucketInState(s *RateLimitState, now time.Time) bool {
	if s == nil {
		return false
	}
	for _, b := range s.buckets() {
		if b.Limit <= 0 {
			continue
		}
		if isExhausted(b.Remaining, true, time.Duration(b.RemainingSecondsNow(now)*float64(time.Second)), true) {
			return true
		}
	}
	return false
}

// IsGenuineNousRateLimit reports an exhausted bucket in the 429 headers or the last known state
func IsGenuineNousRateLimit(h http.Header, lastKnown *RateLimitState, now time.Time) bool {
	if hasExhaustedBucket(parseBucketsFromHeaders(h)) {
		return true
	}
	return hasExhaustedBucketInState(lastKnown, now)
}

// IsLongWelcomeRateLimit reports a rate_limited welcome refusal whose reset marks an exhausted allowance
func IsLongWelcomeRateLimit(ctx map[string]any) bool {
	r, ok := ctx["welcome_refusal"].(WelcomeRefusal)
	return ok && r.Reason == "rate_limited" && time.Duration(r.RetryAfter)*time.Second >= WelcomeLongWait
}

// WelcomeRefusalFromHeaders reads a reason less free tier 429 into the refusal shape from its headers
func WelcomeRefusalFromHeaders(h http.Header, bodyWait time.Duration) WelcomeRefusal {
	buckets := parseBucketsFromHeaders(h)
	var exhaustedWait time.Duration
	empty := false
	for _, b := range buckets {
		if b.hasRemaining && b.remaining <= 0 {
			empty = true
			if b.hasReset && b.reset > exhaustedWait {
				exhaustedWait = b.reset
			}
		}
	}
	wait := exhaustedWait
	if wait <= 0 {
		wait, _ = ParseRetryAfter(LowerHeaders(h)["retry-after"])
	}
	if wait <= 0 {
		wait = bodyWait
	}
	if wait < 0 {
		wait = 0
	}
	reason := "at_capacity"
	if wait >= WelcomeLongWait || empty {
		reason = "rate_limited"
	}
	return WelcomeRefusal{Reason: reason, RetryAfter: int(wait / time.Second), Alternates: []string{}}
}
