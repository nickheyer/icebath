package settings

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
)

type platformCtxKey struct{}

// Context whose reads resolve for one chat platform, its display.platforms overrides and tier defaults applying
func WithPlatform(ctx context.Context, platform string) context.Context {
	if platform == "" {
		return ctx
	}
	return context.WithValue(ctx, platformCtxKey{}, strings.ToLower(platform))
}

// Chat platform a context resolves for, empty for the terminal UI
func PlatformFrom(ctx context.Context) string {
	p, _ := ctx.Value(platformCtxKey{}).(string)
	return p
}

// Platform key of the terminal UI, whose overrides live under display.platforms.cli
const LocalPlatform = "cli"

// Display defaults each kind of chat platform starts from, keyed by tier then setting
var platformTiers = func() map[string]map[string]any {
	high := map[string]any{"display.tool_progress": "all", "display.show_reasoning": false, "display.tool_preview_length": 40,
		"display.interim_assistant_messages": true, "display.long_running_notifications": true, "display.busy_ack_detail": true}
	medium := withDefaults(high, map[string]any{"display.tool_progress": "new"})
	low := withDefaults(high, map[string]any{"display.tool_progress": "off", "display.streaming": false,
		"display.interim_assistant_messages": false, "display.long_running_notifications": false, "display.busy_ack_detail": false})
	minimal := withDefaults(low, map[string]any{"display.tool_preview_length": 0})
	return map[string]map[string]any{"high": high, "medium": medium, "low": low, "minimal": minimal}
}()

// Defaults every chat platform takes where the terminal UI's default does not suit a chat
var chatDefaults = map[string]any{"display.show_reasoning": false}

// Tier and overrides of the platforms the core ships, other platforms asking PlatformTier
var corePlatforms = map[string]map[string]any{
	"webhook":    platformTiers["minimal"],
	"api_server": withDefaults(platformTiers["high"], map[string]any{"display.tool_preview_length": 0}),
}

// Defaults single chat platforms take over their tier's
var platformOwnDefaults = map[string]map[string]any{
	"telegram": {"display.streaming": true},
	"discord":  {"display.streaming": false},
	"slack":    {"display.streaming": false},
	"wecom":    {"display.streaming": true},
}

// Copy of base with over on top
func withDefaults(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// Answers a registered chat platform's display tier and its own display defaults keyed by setting leaf
type PlatformTierFunc func(ctx context.Context, platform string) (tier string, overrides map[string]any)

// Registered platform tier lookup
var platformTierLookup struct {
	sync.RWMutex
	fn PlatformTierFunc
}

// Registers how a chat platform names its display tier, set once by the platform registry
func RegisterPlatformTiers(fn PlatformTierFunc) {
	platformTierLookup.Lock()
	platformTierLookup.fn = fn
	platformTierLookup.Unlock()
}

// Display defaults of a chat platform keyed by setting
func platformDefaults(ctx context.Context, platform string) map[string]any {
	if d, ok := corePlatforms[platform]; ok {
		return d
	}
	platformTierLookup.RLock()
	fn := platformTierLookup.fn
	platformTierLookup.RUnlock()
	if fn == nil {
		return nil
	}
	tier, overrides := fn(ctx, platform)
	d := platformTiers[strings.ToLower(tier)]
	if len(overrides) == 0 {
		return d
	}
	out := withDefaults(d, nil)
	for leaf, v := range overrides {
		out["display."+leaf] = v
	}
	return out
}

// Leaf of a display key under display.platforms.<p>, false for keys outside display
func platformLeaf(key string) (string, bool) {
	return strings.CutPrefix(key, "display.")
}

// Value a platform's own override holds for a setting that takes overrides
func platformValue(ctx context.Context, l *layers, m *Meta, platform string) (any, bool) {
	if platform == "" || !m.Platforms {
		return nil, false
	}
	leaf, ok := platformLeaf(m.Key)
	if !ok {
		return nil, false
	}
	v, set := storedValue(ctx, l.doc, "display.platforms."+platform+"."+leaf)
	if !set || v == nil {
		return nil, false
	}
	return v, true
}

// Default a chat platform's tier gives a setting, false for the terminal UI or a setting its tier leaves alone
func platformDefault(ctx context.Context, m *Meta, platform string) (any, bool) {
	if platform == "" || platform == LocalPlatform || !m.Platforms {
		return nil, false
	}
	if v, ok := platformOwnDefaults[platform][m.Key]; ok {
		return v, true
	}
	if v, ok := platformDefaults(ctx, platform)[m.Key]; ok {
		return v, true
	}
	v, ok := chatDefaults[m.Key]
	return v, ok
}

// Copy of an entry under another key, used as a field of the per platform overrides
func withKey(e Entry, key string) Entry {
	switch t := e.(type) {
	case *Bool:
		c := *t
		c.Key = key
		return &c
	case *Int:
		c := *t
		c.Key = key
		return &c
	case *Float:
		c := *t
		c.Key = key
		return &c
	case *Duration:
		c := *t
		c.Key = key
		return &c
	case *Enum:
		c := *t
		c.Key = key
		return &c
	case *String:
		c := *t
		c.Key = key
		return &c
	case *List:
		c := *t
		c.Key = key
		return &c
	}
	panic("settings: " + e.Info().Key + " cannot be overridden per platform")
}

// Fields of one platform's display overrides, filled once every setting is registered
var platformFields = &Object{Meta: Meta{Key: "display.platforms.*"}}

// Per platform display overrides, each platform's block holding the settings that take one
var PlatformDisplay = Register(&Map{Meta: Meta{Key: "display.platforms", Label: "Chat app display",
	Short: "Display settings one chat app uses instead of the global ones.",
	Help:  "Keyed by chat app. Only the settings that say so take an override here.",
	Group: GroupChatApps, Order: 110, Advanced: true},
	Value: platformFields})

func init() {
	for _, e := range All() {
		if leaf, ok := platformLeaf(e.Info().Key); ok && e.Info().Platforms {
			f := withKey(e, leaf)
			f.Info().Derive, f.Info().Companions = nil, nil
			platformFields.Fields = append(platformFields.Fields, f)
		}
	}
}

// Every platform's own value of a setting that takes overrides, keyed by platform
func PlatformOverrides(ctx context.Context, e Entry) map[string]any {
	m := e.Info()
	leaf, ok := platformLeaf(m.Key)
	if !m.Platforms || !ok || defaultsOnly(ctx) {
		return nil
	}
	out := map[string]any{}
	platforms, _ := lookupPath(layersOf(ctx, homeDir(ctx)).doc, PlatformDisplay.Key)
	for name, block := range config.AsMap(platforms) {
		if v, set := lookupPath(config.AsMap(block), leaf); set && v != nil {
			out[name] = v
		}
	}
	return out
}

func init() {
	config.SuppressWarningsOn = func(ctx context.Context, platform string) bool {
		return DisplaySuppressWarningNotifications.Get(WithPlatform(ctx, platform))
	}
}

// Chat platforms whose display defaults the core declares, in name order
func CorePlatforms() []string {
	out := make([]string, 0, len(corePlatforms))
	for name := range corePlatforms {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}
