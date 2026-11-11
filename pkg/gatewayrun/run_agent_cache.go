package gatewayrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Override fields layered onto runtime kwargs when set so partial overrides keep the defaults
var overrideApplyKeys = []string{
	"provider", "requested_provider", "api_key", "base_url", "api_mode", "credential_pool", "capabilities", "max_tokens",
}

var (
	memoryIdentityProviderMu   sync.Mutex
	memoryIdentityProviderMemo = map[string]memory.Provider{}
)

// Config values whose change rebuilds the cached agent keyed section dot key, with registry generation and memory identity
func extractCacheBustingConfig(ctx context.Context, userConfig map[string]any) map[string]any {
	out := map[string]any{}
	cfg := userConfig
	if cfg == nil {
		cfg = map[string]any{}
	}
	defaults := map[string]any(config.Defaults())
	for _, pair := range cacheBustingConfigKeys {
		section, key := pair[0], pair[1]
		def := cfgGetDefault(defaults, nil, section, key)
		if b, ok := cfg[section].(bool); ok && section == "checkpoints" {
			if key == "enabled" {
				out[section+"."+key] = b
			} else {
				out[section+"."+key] = def
			}
			continue
		}
		out[section+"."+key] = cfgGetDefault(cfg, def, section, key)
	}
	out["tools.registry_generation"] = tools.Default.Generation()
	for key, value := range memoryProviderIdentitySignature(ctx, cfgGetDefault(cfg, nil, "memory", "provider")) {
		out["memory."+key] = value
	}
	return out
}

// Nested lookup through mappings returning the default on any miss and an explicit null as nil
func cfgGetDefault(cfg map[string]any, def any, keys ...string) any {
	if cfg == nil {
		return def
	}
	var node any = cfg
	for _, key := range keys {
		m := config.AsMap(node)
		if m == nil {
			return def
		}
		v, ok := m[key]
		if !ok {
			return def
		}
		node = v
	}
	return node
}

// Identity signature of the named memory provider, empty when none, unloadable or its hook panics
func memoryProviderIdentitySignature(ctx context.Context, providerName any) (out map[string]any) {
	name, ok := providerName.(string)
	if !ok || strings.TrimSpace(name) == "" {
		return map[string]any{}
	}
	name = strings.TrimSpace(name)
	defer func() {
		if recover() != nil {
			out = map[string]any{}
		}
	}()
	memoryIdentityProviderMu.Lock()
	instance := memoryIdentityProviderMemo[name]
	memoryIdentityProviderMu.Unlock()
	if instance == nil {
		env := &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}
		instance = memory.LoadMemoryProviderWithSkills(ctx, env, name, false)
		if instance == nil {
			return map[string]any{}
		}
		memoryIdentityProviderMu.Lock()
		memoryIdentityProviderMemo[name] = instance
		memoryIdentityProviderMu.Unlock()
	}
	signature := instance.IdentitySignature(ctx)
	if signature == nil {
		return map[string]any{}
	}
	return maps.Clone(signature)
}

// Stable sixteen hex key of the agent config, a change rebuilding the cached agent and a match reusing it
func agentConfigSignature(model string, runtime map[string]any, enabledToolsets []string, ephemeralPrompt string,
	cacheKeys map[string]any, userID, userIDAlt string, skipContextFiles bool) string {
	apiKey := ""
	if v := runtime["api_key"]; pyrepr.Truthy(v) {
		apiKey = pyrepr.Str(v)
	}
	keyHash := ""
	if apiKey != "" {
		sum := sha256.Sum256([]byte(apiKey))
		keyHash = hex.EncodeToString(sum[:])
	}
	toolsets := []any{}
	if len(enabledToolsets) > 0 {
		sorted := slices.Clone(enabledToolsets)
		sort.Strings(sorted)
		for _, t := range sorted {
			toolsets = append(toolsets, t)
		}
	}
	blob, _ := pyjson.Dumps([]any{
		model, keyHash,
		runtimeGet(runtime, "base_url"), runtimeGet(runtime, "provider"),
		runtimeGet(runtime, "requested_provider"), runtimeGet(runtime, "api_mode"),
		sortedItems(capabilityMap(runtime["capabilities"])),
		toolsets,
		ephemeralPrompt,
		sortedItems(cacheKeys),
		userID, userIDAlt,
		skipContextFiles,
	}, pyjson.SortKeys(true), pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
	sum := sha256.Sum256([]byte(blob))
	return hex.EncodeToString(sum[:])[:16]
}

// Runtime value at key with an empty string for a missing key and nil for an explicit null
func runtimeGet(runtime map[string]any, key string) any {
	v, ok := runtime[key]
	if !ok {
		return ""
	}
	return v
}

// Capabilities mapping of a runtime as string keyed values, empty when unset or falsy
func capabilityMap(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case map[string]bool:
		out := make(map[string]any, len(m))
		for k, b := range m {
			out[k] = b
		}
		return out
	}
	return nil
}

// Key value pairs of a map ordered by key as two element lists
func sortedItems(m map[string]any) []any {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, []any{k, m[k]})
	}
	return out
}

// Hash of the exact inputs the session context renderer reads so any rendered change changes the key
func ephemeralChangeKey(ctx context.Context, sc *gateway.SessionContext, redactPII bool) string {
	src := sc.Source
	s := func(v *string) string {
		if v == nil {
			return ""
		}
		return *v
	}
	discordIDs := pyrepr.Tuple{}
	discordTools, slackTools := "", ""
	if src.Platform == gateway.PlatformDiscord {
		discordTools = "0"
		if gateway.DiscordToolsLoaded(ctx) {
			discordTools = "1"
		}
		discordIDs = pyrepr.Tuple{s(src.GuildID), s(src.ParentChatID), s(src.ThreadID), src.ChatID}
	}
	if src.Platform == gateway.PlatformSlack {
		slackTools = "0"
		if gateway.SlackToolsLoaded(ctx) {
			slackTools = "1"
		}
	}
	connected := pyrepr.Tuple{}
	for _, p := range sc.ConnectedPlatforms {
		connected = append(connected, string(p))
	}
	homes := pyrepr.Tuple{}
	if sc.HomeChannels != nil {
		for p, hc := range sc.HomeChannels.All() {
			name, chatID := "", ""
			if hc != nil {
				if pyrepr.Truthy(hc.Name) {
					name = pyrepr.Str(hc.Name)
				}
				chatID = hc.ChatID
			}
			homes = append(homes, pyrepr.Tuple{string(p), name, chatID})
		}
	}
	key := pyrepr.Tuple{
		string(src.Platform),
		src.ChatID, s(src.ThreadID), src.ChatType, s(src.ChatName), s(src.ChatTopic),
		s(src.UserName), s(src.UserID), s(src.Profile),
		sc.SharedMultiUserSession, discordIDs, discordTools, slackTools,
		connected, homes,
		redactPII, home.Display(home.Dir(ctx)),
	}
	sum := sha256.Sum256([]byte(pyrepr.Repr(key)))
	return fmt.Sprintf("%x", sum[:])
}
