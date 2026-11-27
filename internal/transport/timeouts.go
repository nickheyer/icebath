package transport

import (
	"cmp"
	"context"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	transportconfig "github.com/nickheyer/icebath/internal/transport/config"
)

// A wait with no limit
const Forever = time.Duration(math.MaxInt64)

// What the waits of one request are worked out from
type Limits struct {
	// Settings of the session, and of the request's provider in its profile
	Config   *config.Config
	Settings config.View
	Model    string
	// The provider is on this machine or its network
	Local bool
	// Rough tokens of the request's context
	Tokens int
	// When the run the request is part of must end, zero for none
	Deadline time.Time
	// Model id a slow reasoning model's floor is looked up by, such as a Bedrock id without its region and vendor; Model when empty
	Family string
}

// Model id the reasoning floor is looked up by
func (l Limits) family() string {
	if l.Family != "" {
		return l.Family
	}
	return l.Model
}

// How long one request may take and stay silent
type Timeouts struct {
	// The whole request, and whether the provider's own setting gave it
	Request    time.Duration
	Configured bool
	Connect    time.Duration
	// Longest the connection of a stream may stay quiet before its read gives up
	Read time.Duration
	// Longest the reply may stay silent before it is dropped and asked again, Forever for no limit
	Stale time.Duration
	// Longest one reply may keep streaming, 0 for no limit
	Lifetime time.Duration
}

// Seconds as a wait, Forever for one too long to hold
func Seconds(f float64) time.Duration {
	if math.IsNaN(f) || math.IsInf(f, 1) || f*float64(time.Second) >= math.MaxInt64 {
		return Forever
	}
	return time.Duration(f * float64(time.Second))
}

// Wait of a silence setting in seconds, Forever when it is 0 and so sets no limit
func silence(seconds float64) time.Duration {
	if seconds <= 0 {
		return Forever
	}
	return Seconds(seconds)
}

func (l Limits) number(key string) float64 {
	return cast.ToFloat64(l.Config.Get(key))
}

// Whether the setting was given in the file or the environment rather than left at its default
func (l Limits) set(key string) bool {
	_, setting, ok := l.Config.Setting(key)
	if !ok {
		return false
	}
	source, _ := l.Config.Source(setting)
	return source != config.FromDefault
}

// Seconds of the provider's own limit: the model's row of its limits by model, else its own; false when neither sets one
func (l Limits) provider(modelKey, providerKey string) (float64, bool) {
	if l.Model != "" {
		if row, ok := l.Settings.Get(providerconfig.ModelTimeouts).(map[string]any); ok {
			if model, ok := row[l.Model].(map[string]any); ok {
				if seconds := cast.ToFloat64(model[modelKey]); seconds > 0 {
					return seconds, true
				}
			}
		}
	}
	if seconds := cast.ToFloat64(l.Settings.Get(providerKey)); seconds > 0 {
		return seconds, true
	}
	return 0, false
}

// Whole request limit and whether the provider's own setting gave it: else transports.api_timeout, never under a second
func (l Limits) request() (time.Duration, bool) {
	if seconds, ok := l.provider(providerconfig.ModelRequestTimeout, providerconfig.RequestTimeout); ok {
		return Seconds(seconds), true
	}
	return Seconds(max(l.number(transportconfig.APITimeout), 1)), false
}

// Stale limit of a stream: the provider's own; a local server's own when the silence limit isn't set; else the silence limit, raised for a
// large context and to the floor of a slow reasoning model, and capped at half what is left of the run
func (l Limits) stale(ctx context.Context) time.Duration {
	if seconds, ok := l.provider(providerconfig.ModelStaleTimeout, providerconfig.StaleTimeout); ok {
		return Seconds(seconds)
	}
	set := l.set(transportconfig.StreamStaleTimeout)
	if !set && l.Local {
		return silence(l.number(transportconfig.LocalStaleTimeout))
	}
	timeout := silence(l.number(transportconfig.StreamStaleTimeout))
	if timeout == Forever {
		return Forever
	}
	timeout = scaled(timeout, l.Tokens)
	if floor, ok := ReasoningFloor(l.family()); ok && floor > timeout {
		timeout = floor
	}
	if set {
		return timeout
	}
	return l.budgeted(ctx, timeout)
}

// Stale patience raised for a large context
func scaled(timeout time.Duration, tokens int) time.Duration {
	switch {
	case tokens > 100_000 && timeout < 300*time.Second:
		return 300 * time.Second
	case tokens > 50_000 && timeout < 240*time.Second:
		return 240 * time.Second
	}
	return timeout
}

// Implicit stale limit capped at half the time left of the run, the request's deadline or the context's, never under a minute
func (l Limits) budgeted(ctx context.Context, timeout time.Duration) time.Duration {
	end := l.Deadline
	if deadline, ok := ctx.Deadline(); ok && (end.IsZero() || deadline.Before(end)) {
		end = deadline
	}
	if end.IsZero() {
		return timeout
	}
	return min(timeout, max(time.Minute, time.Until(end)/2))
}

// Waits of a streamed request: its whole limit, its connect and read limits, its stale limit and how long it may stream
func (l Limits) Stream(ctx context.Context) Timeouts {
	request, configured := l.request()
	stale := l.stale(ctx)
	t := Timeouts{Request: request, Configured: configured, Stale: stale, Lifetime: lifetime(l.number(transportconfig.StreamMaxLifetime), stale)}
	if configured {
		t.Read, t.Connect = request, min(request, time.Minute)
		return t
	}
	t.Connect = 30 * time.Second
	t.Read = Seconds(l.number(transportconfig.StreamReadTimeout))
	if !l.set(transportconfig.StreamReadTimeout) {
		switch {
		case l.Local:
			t.Read = request
		case stale != Forever && stale > t.Read:
			t.Read = stale
		}
	}
	return t
}

// Longest a reply may keep streaming, 0 when off: the setting, raised to a finite stale limit
func lifetime(seconds float64, stale time.Duration) time.Duration {
	if seconds <= 0 || math.IsInf(seconds, 0) || math.IsNaN(seconds) {
		return 0
	}
	if stale != Forever && stale > Seconds(seconds) {
		return stale
	}
	return Seconds(seconds)
}

// Silence a request asking high effort or more is waited through, as long as a slow reasoning model takes
const highEffortFloor = 300 * time.Second

// Silence the request's effort is waited through at least: a slow reasoning model's patience from high effort up, none below it
func EffortFloor(effort string) time.Duration {
	if effort != "" && provider.Rung(effort) >= provider.Rung("high") {
		return highEffortFloor
	}
	return 0
}

// Stale limit of a request that isn't streamed: the provider's own, transports.api_call_stale_timeout, a slow reasoning model's floor, else
// 90 seconds (none for a server on this network); an implicit one raised for a large context and a high effort, scaled by the hops of a
// long decode, and capped at half what is left of the run
func (l Limits) Call(ctx context.Context, effort string, hops int) time.Duration {
	base, implicit := 90*time.Second, true
	if seconds, ok := l.provider(providerconfig.ModelStaleTimeout, providerconfig.StaleTimeout); ok {
		base, implicit = Seconds(seconds), false
	} else if text := strings.TrimSpace(cast.ToString(l.Config.Get(transportconfig.CallStaleTimeout))); text != "" {
		base, implicit = silence(cast.ToFloat64(text)), false
	} else if floor, ok := ReasoningFloor(l.family()); ok {
		base, implicit = floor, false
	}
	if implicit && l.Local {
		return Forever
	}
	if base == Forever {
		return Forever
	}
	timeout := base
	switch {
	case l.Tokens > 100_000:
		timeout = max(base, 240*time.Second)
	case l.Tokens > 50_000:
		timeout = max(base, 150*time.Second)
	}
	_, configured := l.provider(providerconfig.ModelStaleTimeout, providerconfig.StaleTimeout)
	if configured || strings.TrimSpace(cast.ToString(l.Config.Get(transportconfig.CallStaleTimeout))) != "" {
		return timeout
	}
	timeout = max(timeout, EffortFloor(effort))
	timeout *= time.Duration(max(hops, 1))
	return l.budgeted(ctx, timeout)
}

// Stale limits of slow reasoning models by the slugs their ids start with
var reasoningFloors = map[time.Duration][]string{
	600 * time.Second: {"nemotron-3-ultra", "nemotron-3-super", "nemotron-3.5-super-vl", "deepseek-r1", "deepseek-reasoner", "deepseek-flash",
		"deepseek-v4-flash", "deepseek-v4.1-flash", "deepseek-v4-pro", "o1", "o1-mini", "o1-pro", "o1-preview", "o3", "o3-pro", "gpt-5.6", "gpt-6",
		"claude-fable", "gemini-4"},
	300 * time.Second: {"nemotron-3-nano", "nemotron-3.5-lightning", "qwq-32b", "o3-mini", "o4-mini", "grok-4-fast-reasoning", "grok-4.20-reasoning",
		"grok-4.5", "grok-4.6", "ox-alpha", "x-preview-f-free", "inkling", "minimax-m2"},
	240 * time.Second: {"claude-opus-4", "claude-opus-5"},
	180 * time.Second: {"qwen3", "claude-sonnet-5", "claude-sonnet-4.5", "claude-sonnet-4.6", "grok-4-fast-non-reasoning"},
}

type floor struct {
	slug    string
	timeout time.Duration
	match   *regexp.Regexp
}

// Floors longest slug first, so the most specific one wins
var floors = func() []floor {
	var out []floor
	for timeout, slugs := range reasoningFloors {
		for _, slug := range slugs {
			out = append(out, floor{slug, timeout, regexp.MustCompile(`^` + regexp.QuoteMeta(slug) + `(?:$|[\-._:])`)})
		}
	}
	slices.SortFunc(out, func(a, b floor) int { return cmp.Or(len(b.slug)-len(a.slug), strings.Compare(a.slug, b.slug)) })
	return out
}()

// Stale limit a slow reasoning model gets at least, by its id without any vendor prefix
func ReasoningFloor(model string) (time.Duration, bool) {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return 0, false
	}
	name = name[strings.LastIndex(name, "/")+1:]
	for _, f := range floors {
		if f.match.MatchString(name) {
			return f.timeout, true
		}
	}
	return 0, false
}
