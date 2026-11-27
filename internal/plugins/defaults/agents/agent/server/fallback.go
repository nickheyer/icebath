package server

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	launchconfig "github.com/nickheyer/icebath/internal/launch/config"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/provider"
)

const (
	// How long the primary waits after a failure that is no rate limit walked the whole chain
	exhaustedCooldown = 5 * time.Second
	// Longest a rate limited primary is left before it is tried again
	maxCooldown = 4 * time.Hour
)

// Words of a refusal for want of credit
var billingWords = regexp.MustCompile(`(?i)credits|insufficient funds|insufficient_quota|billing|quota exceeded|payment required|can only afford`)

// Fallback state of a session: the chain entry it runs on in place of its main model, and when the main one may be tried again
type failover struct {
	// Entry of the chain tried next
	next int
	// Route the session runs on while a fallback is active, empty while the main model is
	active   provider.Route
	instance provider.Instance
	// The main model failed this way, which the notices name
	primary provider.Route
	// The main model is not tried again before then, after a rate limit
	until time.Time
	// Rate limits in a row, which double the wait
	backoff int
}

// Routes of the profile's fallback chain, each provider:model with the provider named as in the profile; an entry that names no provider
// of the profile, no model or one with no credential is left out, and the whole chain in safe mode
func (a *Agent) chain(settings *config.Config) []provider.Route {
	if settings.GetBool(launchconfig.SafeMode) {
		return nil
	}
	var routes []provider.Route
	seen := map[string]bool{}
	for _, entry := range settings.GetStringSlice(agentconfig.Fallbacks) {
		name, model, ok := strings.Cut(strings.TrimSpace(entry), ":")
		name, model = strings.TrimSpace(name), strings.TrimSpace(model)
		if !ok || name == "" || model == "" {
			continue
		}
		instance, found := a.provider(settings, name)
		if !found {
			continue
		}
		route, err := a.credentials.Route(instance, model)
		if err != nil || seen[routeKey(route)] {
			continue
		}
		seen[routeKey(route)] = true
		routes = append(routes, route)
	}
	return routes
}

func routeKey(route provider.Route) string {
	return strings.ToLower(route.Provider) + "\x00" + route.Model + "\x00" + strings.TrimRight(route.BaseURL, "/")
}

// Route the session's next model call takes: the active fallback, else the voice chat model of a turn the person spoke, else the main model.
// A turn the voice chat model runs sets the session's fallback state aside, so a fallback active here then is one the voice chat model fell to
func (a *Agent) current(r *run) (provider.Instance, provider.Route, error) {
	r.mu.Lock()
	f, v := r.failover, r.voice
	r.mu.Unlock()
	if f != nil && f.active.Model != "" {
		return f.instance, f.active, nil
	}
	if v != nil && v.route.Model != "" {
		return v.instance, v.route, nil
	}
	return a.route(r)
}

// Why a failure moves the session to its next fallback, empty when it doesn't: one retrying cannot fix, or one the retries did not
func fallbackReason(err error, exhausted bool) string {
	failure, ok := errors.AsType[*provider.APIError](err)
	if !ok {
		return ""
	}
	// A plugin's classification decides whether the session falls back, at once when it says so
	if failure.Fallback != nil && !*failure.Fallback {
		return ""
	}
	forced := failure.Fallback != nil
	switch {
	case forced && failure.Kind == provider.ErrRateLimit:
		return "rate limit"
	case forced && failure.Kind != provider.ErrQuota:
		return "plugin classification: " + failure.Reason
	case failure.StatusCode == 402 || failure.Kind == provider.ErrQuota || billingWords.MatchString(failure.Message):
		return billingReason
	case failure.Kind == provider.ErrAuth:
		return "authentication failed"
	case failure.Kind == provider.ErrModelNotFound:
		return "model not found"
	case !exhausted:
		return ""
	case failure.Kind == provider.ErrRateLimit:
		return "rate limit"
	case failure.Kind == provider.ErrServer:
		return "provider server error"
	case failure.Kind == provider.ErrNetwork:
		return "request timeout"
	case failure.Kind == provider.ErrEmptyResponse, failure.Kind == provider.ErrUnknown:
		return "provider failure"
	}
	return ""
}

// Moves the session onto the next usable entry of its fallback chain after the failure, telling its windows; false when the chain is spent.
// A rate limited main model waits out its reset, or a doubling wait, before it is tried again
func (a *Agent) failover(r *run, settings *config.Config, failed provider.Route, err error, reason string) bool {
	chain := a.chain(settings)
	r.mu.Lock()
	f := r.failover
	if f == nil {
		f = &failover{}
		r.failover = f
	}
	limited := reason == "rate limit"
	if f.active.Model == "" {
		f.primary = failed
	}
	cooled := ""
	if limited && f.active.Model == "" {
		reset, known := resetDelay(err)
		if stay := time.Duration(max(settings.GetInt(agentconfig.FallbackStayPut), 0)) * time.Second; known && reset < stay {
			slog.Info(fmt.Sprintf("Rate limit resets in %.0f s (< %s=%.0f): staying on the primary", reset.Seconds(), agentconfig.FallbackStayPut,
				stay.Seconds()))
			r.mu.Unlock()
			return false
		}
		wait := time.Duration(math.Min(60*math.Pow(2, float64(f.backoff)), maxCooldown.Seconds())) * time.Second
		if known {
			wait = reset
		}
		f.backoff++
		f.until = time.Now().Add(wait)
		cooled = fmt.Sprintf(" Primary retry eligible in ~%d s; recovery is not guaranteed.", int(math.Ceil(wait.Seconds())))
	}
	for f.next < len(chain) {
		next := chain[f.next]
		f.next++
		if routeKey(next) == routeKey(failed) {
			continue
		}
		instance, found := a.provider(settings, next.Provider)
		if !found {
			continue
		}
		f.active, f.instance = next, instance
		a.notify(r, fmt.Sprintf("⚠️ Model fallback: %s via %s unavailable (%s); using %s via %s.%s", failed.Model, failed.Provider, reason, next.Model,
			next.Provider, cooled))
		r.mu.Unlock()
		a.changed(r)
		return true
	}
	if len(chain) > 0 && !limited {
		f.until = laterOf(f.until, time.Now().Add(exhaustedCooldown))
	}
	r.mu.Unlock()
	return false
}

// When the provider says the rate limit that refused the request ends: the wait it gave with the failure, else the one its message names
func resetDelay(err error) (time.Duration, bool) {
	if failure, ok := errors.AsType[*provider.APIError](err); ok && failure.RetryAfter > 0 {
		return failure.RetryAfter, true
	}
	if delay, ok := credentials.Delay(err.Error()); ok && delay > 0 {
		return delay, true
	}
	return 0, false
}

// Puts the session back on its main model as a turn starts, once a fallback ran and any wait for the main one's rate limit is over
func (a *Agent) restore(r *run) {
	r.mu.Lock()
	f := r.failover
	if f == nil || time.Now().Before(f.until) {
		r.mu.Unlock()
		return
	}
	r.failover = nil
	r.mu.Unlock()
	if f.active.Model == "" {
		return
	}
	_, primary, err := a.route(r)
	if err != nil {
		return
	}
	r.mu.Lock()
	a.notify(r, fmt.Sprintf("✅ Primary model restored: %s via %s; fallback %s via %s is no longer active.", primary.Model, primary.Provider,
		f.active.Model, f.active.Provider))
	r.mu.Unlock()
	a.changed(r)
}

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

var (
	modelLine    = regexp.MustCompile(`(?m)^Model: .*$`)
	providerLine = regexp.MustCompile(`(?m)^Provider: .*$`)
)

// System prompt built for the main model with its last Model and Provider lines naming the route a fallback runs on, the rest unchanged
// so the provider's cache keeps it
func renamed(prompt string, route provider.Route) string {
	for _, line := range []struct {
		re    *regexp.Regexp
		label string
		value string
	}{{modelLine, "Model", route.Model}, {providerLine, "Provider", route.Provider}} {
		if line.value == "" {
			continue
		}
		if found := line.re.FindAllStringIndex(prompt, -1); len(found) > 0 {
			last := found[len(found)-1]
			prompt = prompt[:last[0]] + line.label + ": " + line.value + prompt[last[1]:]
		}
	}
	return prompt
}
