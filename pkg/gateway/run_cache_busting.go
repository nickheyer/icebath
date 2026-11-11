package gateway

import (
	"context"
	"maps"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Section and key config values baked into the agent at construction whose change invalidates the cached agent
var cacheBustingConfigKeys = [][2]string{
	{"model", "context_length"}, {"compression", "enabled"},
	{"compression", "progress_notices"}, {"compression", "threshold"},
	{"compression", "model_thresholds"}, {"compression", "threshold_tokens"},
	{"compression", "codex_gpt55_autoraise"}, {"compression", "codex_app_server_auto"},
	{"compression", "codex_responses_native"},
	{"compression", "codex_responses_compact_threshold"}, {"compression", "in_place"},
	{"compression", "checkpoint_required"}, {"compression", "micro_compact"},
	{"compression", "micro_compact_every_n_turns"},
	{"compression", "micro_compact_defrag_threshold_tokens"}, {"compression", "target_ratio"},
	{"compression", "tail_mode"}, {"compression", "protect_last_n"},
	{"compression", "proactive_prune_tokens"},
	{"compression", "proactive_prune_min_result_chars"},
	{"compression", "proactive_prune_min_reclaim_tokens"},
	{"compression", "min_tail_user_messages"}, {"agent", "disabled_toolsets"},
	{"memory", "provider"}, {"checkpoints", "enabled"}, {"checkpoints", "max_snapshots"},
	{"checkpoints", "max_total_size_mb"}, {"checkpoints", "max_file_size_mb"},
}

var (
	memoryIdentityProviderMu   sync.Mutex
	memoryIdentityProviderMemo = map[string]memory.Provider{}
)

// Config values whose change rebuilds the cached agent keyed section dot key, with registry generation and memory identity
func ExtractCacheBustingConfig(ctx context.Context, userConfig map[string]any) map[string]any {
	out := map[string]any{}
	cfg := userConfig
	if cfg == nil {
		cfg = map[string]any{}
	}
	defaults := map[string]any(config.Defaults())
	for _, pair := range cacheBustingConfigKeys {
		section, key := pair[0], pair[1]
		def := cacheCfgGetDefault(defaults, nil, section, key)
		if b, ok := cfg[section].(bool); ok && section == "checkpoints" {
			if key == "enabled" {
				out[section+"."+key] = b
			} else {
				out[section+"."+key] = def
			}
			continue
		}
		out[section+"."+key] = cacheCfgGetDefault(cfg, def, section, key)
	}
	out["tools.registry_generation"] = tools.Default.Generation()
	for key, value := range memoryProviderIdentitySignature(ctx, cacheCfgGetDefault(cfg, nil, "memory", "provider")) {
		out["memory."+key] = value
	}
	return out
}

// Nested lookup through mappings returning the default on any miss and an explicit null as nil
func cacheCfgGetDefault(cfg map[string]any, def any, keys ...string) any {
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
