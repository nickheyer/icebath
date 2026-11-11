package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Budget of bot authored inbound messages per conversation and the cooldown once it trips
type BotLoopGuardSettings struct {
	Enabled         bool
	MaxEvents       int
	WindowSeconds   float64
	CooldownSeconds float64
}

// Default bot loop guard budget of 20 messages per 300 seconds with a 600 second cooldown
func DefaultBotLoopGuardSettings() BotLoopGuardSettings {
	return BotLoopGuardSettings{Enabled: true, MaxEvents: 20, WindowSeconds: 300.0, CooldownSeconds: 600.0}
}

// Boolean of a config value, the default for anything that is not a recognised token
func botGuardBool(raw any, def bool) bool {
	if b, ok := raw.(bool); ok {
		return b
	}
	text := ""
	if raw != nil {
		text = pystr.Lower(pystr.Strip(pyrepr.Str(raw)))
	}
	switch text {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	}
	return def
}

// Positive float of a config value, the default for a bool, an unparseable or a non positive value
func botGuardPositive(raw any, def float64) float64 {
	if _, isBool := raw.(bool); isBool {
		return def
	}
	value, err := pyint.Float(raw)
	if err != nil || !(value > 0) {
		return def
	}
	return value
}

// Positive whole number of a config value, the default unless it is an integral value of at least one
func botGuardPositiveInt(raw any, def int) int {
	value := botGuardPositive(raw, 0.0)
	if value >= 1 && value == float64(int64(value)) {
		return int(value)
	}
	return def
}

// Settings of the gateway.bot_loop_guard config block, an unusable value keeping its default
func BotLoopGuardSettingsFromConfig(cfg map[string]any) BotLoopGuardSettings {
	defaults := DefaultBotLoopGuardSettings()
	raw, _ := config.CfgGet(cfg, "gateway", "bot_loop_guard")
	block, ok := raw.(map[string]any)
	if !ok {
		return defaults
	}
	return BotLoopGuardSettings{
		Enabled:         botGuardBool(block["enabled"], defaults.Enabled),
		MaxEvents:       botGuardPositiveInt(block["max_events"], defaults.MaxEvents),
		WindowSeconds:   botGuardPositive(block["window_seconds"], defaults.WindowSeconds),
		CooldownSeconds: botGuardPositive(block["cooldown_seconds"], defaults.CooldownSeconds),
	}
}

// Settings of the live config.yaml, the defaults when it cannot be read
func LoadBotLoopGuardSettings(ctx context.Context) BotLoopGuardSettings {
	cfg, err := config.LoadReadonly(ctx, home.Dir(ctx), nil)
	if err != nil {
		return DefaultBotLoopGuardSettings()
	}
	return BotLoopGuardSettingsFromConfig(cfg)
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
