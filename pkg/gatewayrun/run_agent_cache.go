package gatewayrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Override fields layered onto runtime kwargs when set so partial overrides keep the defaults
var overrideApplyKeys = []string{
	"provider", "requested_provider", "api_key", "base_url", "api_mode", "credential_pool", "capabilities", "max_tokens",
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
