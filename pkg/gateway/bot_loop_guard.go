package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/settings"
)

// Budget of bot authored inbound messages per conversation and the cooldown once it trips
type BotLoopGuardSettings struct {
	Enabled         bool
	MaxEvents       int
	WindowSeconds   float64
	CooldownSeconds float64
}

// Bot loop guard budget the registry gives by default
func DefaultBotLoopGuardSettings() BotLoopGuardSettings {
	return BotLoopGuardSettings{Enabled: settings.GatewayBotLoopGuardEnabled.Default, MaxEvents: settings.GatewayBotLoopGuardMaxEvents.Default,
		WindowSeconds: settings.GatewayBotLoopGuardWindowSeconds.Default.Seconds(), CooldownSeconds: settings.GatewayBotLoopGuardCooldownSeconds.Default.Seconds()}
}

// Bot loop guard budget of the profile in ctx, a non positive count or span keeping its default
func LoadBotLoopGuardSettings(ctx context.Context) BotLoopGuardSettings {
	out := DefaultBotLoopGuardSettings()
	out.Enabled = settings.GatewayBotLoopGuardEnabled.Get(ctx)
	if n := settings.GatewayBotLoopGuardMaxEvents.Get(ctx); n > 0 {
		out.MaxEvents = n
	}
	if d := settings.GatewayBotLoopGuardWindowSeconds.Get(ctx); d > 0 {
		out.WindowSeconds = d.Seconds()
	}
	if d := settings.GatewayBotLoopGuardCooldownSeconds.Get(ctx); d > 0 {
		out.CooldownSeconds = d.Seconds()
	}
	return out
}

var botGuardEpoch = time.Now()

// Monotonic seconds of the bot loop guard clock
func botGuardMonotonic() float64 { return time.Since(botGuardEpoch).Seconds() }

// Per conversation sliding window of admitted bot messages with a cooldown once the budget trips
type BotLoopGuard struct {
	settings      func(ctx context.Context) BotLoopGuardSettings
	clock         func() float64
	mu            sync.Mutex
	events        map[any][]float64
	cooldownUntil map[any]float64
	lastSweep     float64
}

// Guard reading its settings per call under the caller's scope, the live config and monotonic time when nil
func NewBotLoopGuard(settings func(ctx context.Context) BotLoopGuardSettings, clock func() float64) *BotLoopGuard {
	if settings == nil {
		settings = LoadBotLoopGuardSettings
	}
	if clock == nil {
		clock = botGuardMonotonic
	}
	return &BotLoopGuard{settings: settings, clock: clock, events: map[any][]float64{}, cooldownUntil: map[any]float64{}}
}

// Number of conversations holding a window
func (g *BotLoopGuard) TrackedConversations() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.events)
}

// Whether the conversation is cooling down, a pure read
func (g *BotLoopGuard) Blocked(ctx context.Context, conversation any) bool {
	if !g.settings(ctx).Enabled {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cooldownUntil[conversation] > g.clock()
}

// Counts one admitted bot message, its verdict and the disabled, ok, tripped or cooldown state
func (g *BotLoopGuard) Admit(ctx context.Context, conversation any) (bool, string) {
	settings := g.settings(ctx)
	if !settings.Enabled {
		return true, "disabled"
	}
	now := g.clock()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweep(now, settings)
	if g.cooldownUntil[conversation] > now {
		return false, "cooldown"
	}
	events := g.events[conversation]
	cutoff := now - settings.WindowSeconds
	for len(events) > 0 && events[0] <= cutoff {
		events = events[1:]
	}
	if len(events) >= settings.MaxEvents {
		g.cooldownUntil[conversation] = now + settings.CooldownSeconds
		g.events[conversation] = nil
		return false, "tripped"
	}
	g.events[conversation] = append(events, now)
	return true, "ok"
}

// Drops idle conversations at most once per window, the caller holding the lock
func (g *BotLoopGuard) sweep(now float64, settings BotLoopGuardSettings) {
	if now-g.lastSweep < settings.WindowSeconds {
		return
	}
	g.lastSweep = now
	idleCutoff := now - settings.WindowSeconds
	for key, events := range g.events {
		if len(events) == 0 || events[len(events)-1] <= idleCutoff {
			delete(g.events, key)
		}
	}
	for key, until := range g.cooldownUntil {
		if until <= now {
			delete(g.cooldownUntil, key)
		}
	}
}
