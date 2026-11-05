package gateway

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var truthyTokens = []string{"1", "on", "true", "yes"}
var falsyTokens = []string{"0", "false", "no", "off"}

// Python str of the value parsed as a recognized truthy or falsy token, ok false otherwise
func boolToken(value any) (bool, bool) {
	if s, ok := value.(string); ok {
		return pystr.BoolToken(s)
	}
	return pystr.BoolToken(pyrepr.Str(value))
}

// Bool-ish config value with nil keeping the default and unrecognized strings keeping it too
func coerceBool(value any, def bool) bool {
	if value == nil {
		return def
	}
	if s, ok := value.(string); ok {
		parsed, known := boolToken(s)
		if !known {
			return def
		}
		return parsed
	}
	return pyrepr.Truthy(value)
}

// GATEWAY_MULTIPLEX_PROFILES operator override, nil when unset, blank or unrecognized
func envMultiplexProfilesOverride() *bool {
	raw := os.Getenv("GATEWAY_MULTIPLEX_PROFILES")
	if pystr.Strip(raw) == "" {
		return nil
	}
	parsed, known := boolToken(raw)
	if !known {
		slog.Warn(fmt.Sprintf("Ignoring unrecognized GATEWAY_MULTIPLEX_PROFILES=%s (expected one of %s or %s); falling back to config.yaml.",
			pyrepr.Repr(raw), pyrepr.Repr(anyStrings(truthyTokens)), pyrepr.Repr(anyStrings(falsyTokens))))
		return nil
	}
	return &parsed
}

func anyStrings(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// Policies for the runner when the last messaging adapter goes down
var OnAllAdaptersDownPolicies = []string{"exit", "stay_alive"}

// GATEWAY_ON_ALL_ADAPTERS_DOWN operator override, empty when unset, blank or unrecognized
func envOnAllAdaptersDownOverride() string {
	raw := os.Getenv("GATEWAY_ON_ALL_ADAPTERS_DOWN")
	if pystr.Strip(raw) == "" {
		return ""
	}
	token := pystr.Lower(pystr.Strip(raw))
	for _, p := range OnAllAdaptersDownPolicies {
		if token == p {
			return token
		}
	}
	slog.Warn(fmt.Sprintf("Ignoring unrecognized GATEWAY_ON_ALL_ADAPTERS_DOWN=%s (expected one of %s); falling back to config.yaml.",
		pyrepr.Repr(raw), pyrepr.Repr(anyStrings(OnAllAdaptersDownPolicies))))
	return ""
}

// Canonical streaming transport token where YAML booleans map to auto or off and blanks to auto
func normalizeTransportToken(value any) string {
	switch v := value.(type) {
	case nil:
		return "auto"
	case bool:
		if v {
			return "auto"
		}
		return "off"
	}
	if t := pystr.Lower(pystr.Strip(pyrepr.Str(value))); t != "" {
		return t
	}
	return "auto"
}

// Mapping value or a fresh empty mapping for anything else
func coerceDict(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Unauthorized DM behaviors, pair sends a code, ignore drops and decline refuses once
var UnauthorizedDMBehaviors = map[string]bool{"pair": true, "ignore": true, "decline": true}

// Refusal text the decline behavior sends to an unauthorized sender
const DefaultUnauthorizedDMDeclineMessage = "Hi! I'm a personal assistant and can only chat with my owner, so I can't help you directly. Sorry!"

// Lower cased stripped string when it is one of the choices, else the default
func normalizeChoice(value any, choices map[string]bool, def string) string {
	s, ok := value.(string)
	if !ok {
		return def
	}
	if n := pystr.Lower(pystr.Strip(s)); choices[n] {
		return n
	}
	return def
}

// Get or create the key as a mapping, replacing a non mapping value with an empty one
func dictSlot(container map[string]any, key string) map[string]any {
	if m, ok := container[key].(map[string]any); ok {
		return m
	}
	m := map[string]any{}
	container[key] = m
	return m
}

// Platforms whose adapter binds a listening port
var PortBindingPlatformValues = map[string]bool{
	"webhook": true, "api_server": true, "msgraph_webhook": true, "feishu": true, "wecom_callback": true,
	"bluebubbles": true, "sms": true, "whatsapp_cloud": true, "line": true, "teams": true,
}

// Connection mode a platform must use before it binds a port
var PortBindingConditionalModes = map[string]string{"feishu": "webhook"}

// Platforms whose listener the shared ingress mirrors
var SharedListenerMirrorPlatforms = map[string]bool{"api_server": true, "webhook": true}

// Mount path of each mirrored shared listener
var SharedListenerMirrorPaths = map[string]string{"api_server": "/v1", "webhook": "/webhooks/<route>"}

// True when the platform actually binds a port for its extra config
func PlatformBindsPort(platformValue string, extra map[string]any) bool {
	if !PortBindingPlatformValues[platformValue] {
		return false
	}
	expected, conditional := PortBindingConditionalModes[platformValue]
	if !conditional {
		return true
	}
	mode, ok := extra["connection_mode"]
	if !ok {
		mode = "websocket"
	}
	return pystr.Lower(pystr.Strip(pyrepr.Str(mode))) == expected
}

var discordChannelLinkRE = regexp.MustCompile(`^https://(?:(?:ptb|canary)\.)?discord(?:app)?\.com/channels/(?:[0-9]+|@me)/([0-9]+)/?$`)

// Channel id from a pasted Discord channel link, ok false for message links and anything else
func DiscordChannelIDFromLink(value string) (string, bool) {
	m := discordChannelLinkRE.FindStringSubmatch(value)
	if m == nil {
		return "", false
	}
	return m[1], true
}
