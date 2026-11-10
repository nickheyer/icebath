package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// One declarable capability and the legacy config gate it maps to
type CapabilitySpec struct {
	ID          string
	LegacyPath  []string
	Description string
}

// Every capability with an enforcing surface, keyed by id
var CapabilityRegistry = map[string]CapabilitySpec{
	"tools.override":           {"tools.override", []string{"allow_tool_override"}, "Replace built-in tools (e.g. shell_exec, write_file) — an override can intercept everything routed through that tool"},
	"llm.provider_override":    {"llm.provider_override", []string{"llm", "allow_provider_override"}, "Run host-owned LLM calls against a provider other than your active one (uses your credentials)"},
	"llm.model_override":       {"llm.model_override", []string{"llm", "allow_model_override"}, "Choose which model host-owned LLM calls use (spend follows the chosen model)"},
	"llm.agent_id_override":    {"llm.agent_id_override", []string{"llm", "allow_agent_id_override"}, "Attribute its LLM calls to a different agent id"},
	"llm.profile_override":     {"llm.profile_override", []string{"llm", "allow_profile_override"}, "Run LLM calls under a different auth profile"},
	"llm.task_override":        {"llm.task_override", []string{"llm", "allow_task_override"}, "Route its LLM calls through the host's built-in auxiliary task lanes"},
	"llm.side_question":        {"llm.side_question", nil, "Ask your agent a short question after each turn. It sees the whole session, including memory, and spends your main model's tokens."},
	"gateway.platform_actions": {"gateway.platform_actions", []string{"allow_platform_actions"}, "Act on connected chat platforms as the gateway bot (add reactions, rename threads) via ctx.platform_actions"},
}

// Config keys under plugins.entries.<id> holding consent state
const (
	GrantedCapabilitiesKey = "granted_capabilities"
	CapabilitiesConsentKey = "capabilities_consent"
)

// Sorted ids of every known capability
func ValidCapabilityIDs() []string {
	out := make([]string, 0, len(CapabilityRegistry))
	for id := range CapabilityRegistry {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Known capability ids of a manifest capabilities value, unknown ids dropped with a warning
func ParseDeclaredCapabilities(raw any, pluginName string) []string {
	if raw == nil {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		slog.Warn("plugin manifest capabilities must be a list, ignoring", "plugin", pluginName)
		return nil
	}
	var out []string
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			slog.Warn("plugin ignoring non-string capability entry", "plugin", pluginName, "entry", item)
			continue
		}
		cap := strings.TrimSpace(s)
		if _, known := CapabilityRegistry[cap]; !known {
			slog.Warn("plugin declares unknown capability, ignoring", "plugin", pluginName, "capability", cap, "known", strings.Join(ValidCapabilityIDs(), ", "))
			continue
		}
		out = appendUnique(out, cap)
	}
	return out
}

// Deduplicated known subset of a capability list in first seen order
func knownCapabilities(caps []string) []string {
	var out []string
	for _, c := range caps {
		if _, ok := CapabilityRegistry[c]; ok {
			out = appendUnique(out, c)
		}
	}
	return out
}

// Order insensitive sha256 of a capability set
func CapabilitySetHash(caps []string) string {
	set := map[string]bool{}
	for _, c := range caps {
		set[c] = true
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return hex.EncodeToString(sum[:])
}

// The plugins.entries.<id> mapping of a config document, empty when absent or malformed
func pluginEntry(cfg map[string]any, pluginID string) map[string]any {
	entries := asMap(asMap(cfg["plugins"])["entries"])
	if e := asMap(entries[pluginID]); e != nil {
		return e
	}
	return map[string]any{}
}

// Capabilities the user granted the plugin in the given config document
func GrantedCapabilities(cfg map[string]any, pluginID string) map[string]bool {
	out := map[string]bool{}
	for _, c := range knownCapabilities(stringList(pluginEntry(cfg, pluginID)[GrantedCapabilitiesKey])) {
		out[strings.TrimSpace(c)] = true
	}
	return out
}

// Reports whether the deprecated allow key of a capability is truthy in an entry
func legacyGateSet(entry map[string]any, spec CapabilitySpec) bool {
	if len(spec.LegacyPath) == 0 {
		return false
	}
	var node any = entry
	for _, part := range spec.LegacyPath {
		m := asMap(node)
		if m == nil {
			return false
		}
		node = m[part]
	}
	return truthy(node)
}

// Reports whether a capability is live for a plugin through a grant or the legacy allow key
func CapabilityGranted(cfg map[string]any, pluginID, capability string) bool {
	spec, ok := CapabilityRegistry[capability]
	if !ok {
		slog.Debug("capability check for unknown id denied", "capability", capability, "plugin", pluginID)
		return false
	}
	entry := pluginEntry(cfg, pluginID)
	allowed, evidence := false, "not granted"
	if GrantedCapabilities(cfg, pluginID)[capability] {
		allowed, evidence = true, GrantedCapabilitiesKey
	} else if legacyGateSet(entry, spec) {
		allowed, evidence = true, "legacy key plugins.entries."+pluginID+"."+strings.Join(spec.LegacyPath, ".")+" (deprecated)"
	}
	decision := "deny"
	if allowed {
		decision = "allow"
	}
	slog.Info("capability_check", "plugin", pluginID, "capability", capability, "decision", decision, "evidence", evidence)
	return allowed
}

// Child mapping of parent at key, replacing a non mapping value
func childMap(parent map[string]any, key string) map[string]any {
	if m := asMap(parent[key]); m != nil {
		parent[key] = m
		return m
	}
	m := map[string]any{}
	parent[key] = m
	return m
}

// Persists a consent decision, granting the union and mirroring every grant into its legacy gate
func RecordConsent(ctx context.Context, h, pluginID string, granted, declared []string) error {
	cfg, err := readUserConfig(h)
	if err != nil {
		return err
	}
	entry := childMap(childMap(childMap(cfg, "plugins"), "entries"), pluginID)
	merged := append(stringList(entry[GrantedCapabilitiesKey]), knownCapabilities(granted)...)
	known := knownCapabilities(merged)
	sort.Strings(known)
	grants := make([]any, 0, len(known))
	for _, c := range known {
		grants = append(grants, c)
	}
	entry[GrantedCapabilitiesKey] = grants
	entry[CapabilitiesConsentKey] = map[string]any{
		"hash":       CapabilitySetHash(knownCapabilities(declared)),
		"granted_at": time.Now().UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	for _, c := range known {
		spec := CapabilityRegistry[c]
		if len(spec.LegacyPath) == 0 {
			continue
		}
		node := entry
		for _, part := range spec.LegacyPath[:len(spec.LegacyPath)-1] {
			node = childMap(node, part)
		}
		node[spec.LegacyPath[len(spec.LegacyPath)-1]] = true
	}
	if err := saveUserConfig(ctx, h, cfg); err != nil {
		return err
	}
	slog.Info("capability_consent", "plugin", pluginID, "granted", strings.Join(known, ","), "declared_hash", CapabilitySetHash(knownCapabilities(declared))[:12])
	return nil
}

// Stored consent hash of a plugin, empty when absent
func ConsentHash(cfg map[string]any, pluginID string) string {
	consent := asMap(pluginEntry(cfg, pluginID)[CapabilitiesConsentKey])
	h, _ := consent["hash"].(string)
	return h
}

// Declared capabilities the user has not granted yet
func PendingCapabilities(cfg map[string]any, pluginID string, declared []string) []string {
	granted := GrantedCapabilities(cfg, pluginID)
	var out []string
	for _, c := range knownCapabilities(declared) {
		if !granted[c] {
			out = append(out, c)
		}
	}
	return out
}

// Reports whether the declared set differs from the one the user consented to
func DeclaredSetChanged(cfg map[string]any, pluginID string, declared []string) bool {
	stored := ConsentHash(cfg, pluginID)
	return stored == "" || stored != CapabilitySetHash(knownCapabilities(declared))
}

// Reports whether a config value reads as true
func truthy(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		switch strings.ToLower(strings.TrimSpace(b)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	case int:
		return b != 0
	case int64:
		return b != 0
	case float64:
		return b != 0
	}
	return false
}

// Human readable capability line for a consent screen
func capabilityLine(id string) string {
	spec, ok := CapabilityRegistry[id]
	if !ok {
		return id
	}
	return fmt.Sprintf("%s — %s", id, spec.Description)
}
