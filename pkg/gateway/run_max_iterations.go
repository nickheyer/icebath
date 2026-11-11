package gateway

import (
	"context"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Session config keys bridged to their env variables each turn
var sessionsEnvBridge = [][2]string{{"cjk_fts", "ICEBATH_CJK_FTS"}, {"search_slow_ms", "ICEBATH_SEARCH_SLOW_MS"}}

// Exports every present key of a config section as its str value
func bridgeSectionToEnv(section any, mapping [][2]string) {
	m, ok := section.(map[string]any)
	if !ok {
		return
	}
	for _, pair := range mapping {
		if v, present := m[pair[0]]; present {
			os.Setenv(pair[1], pyrepr.Str(v))
		}
	}
}

// Bridges agent.max_turns in its raw spelling, a null clearing a stale bridge
func bridgeMaxTurnsToEnv(agentCfg any) {
	m, ok := agentCfg.(map[string]any)
	if !ok {
		return
	}
	raw, present := m["max_turns"]
	if !present {
		return
	}
	if raw != nil {
		os.Setenv("ICEBATH_MAX_ITERATIONS", pyrepr.Str(raw))
		return
	}
	os.Unsetenv("ICEBATH_MAX_ITERATIONS")
}

// Effective user config for the presence sensitive env bridge, a bypass worker's frozen snapshot when set
func loadBridgeConfig(configPath string) (map[string]any, error) {
	if snapshot := config.WorkerConfigSnapshot(); snapshot != nil {
		return snapshot, nil
	}
	return config.LoadUserEffective(configPath, nil)
}

// Re-bridges agent.max_turns and sessions settings from the home config, skipped inside a served secondary
func bridgeMaxTurnsFromConfig(ctx context.Context, h string) {
	if profileScoped(ctx) {
		return
	}
	configPath := filepath.Join(h, "config.yaml")
	exists, err := config.ConfigExists(ctx, configPath)
	if err != nil || !exists {
		return
	}
	cfg, err := loadBridgeConfig(configPath)
	if err != nil {
		return
	}
	agentCfg, ok := cfg["agent"]
	if !ok {
		agentCfg = map[string]any{}
	}
	bridgeMaxTurnsToEnv(agentCfg)
	sessions, ok := cfg["sessions"]
	if !ok {
		sessions = map[string]any{}
	}
	bridgeSectionToEnv(sessions, sessionsEnvBridge)
}

// Reloads .env for rotated keys outside multiplex while config.yaml stays authoritative for budgets
func reloadRuntimeEnvPreservingConfigAuthority(ctx context.Context) {
	processHome := home.Dir(context.Background())
	if !provider.IsMultiplexActive(ctx) {
		_, _ = provider.LoadIcebathDotenv(ctx, provider.DotenvLoadOptions{Home: processHome})
	}
	bridgeMaxTurnsFromConfig(ctx, processHome)
}

// Per turn iteration budget after the env refresh, a routed profile reading its own agent.max_turns
func CurrentMaxIterations(ctx context.Context) int {
	reloadRuntimeEnvPreservingConfigAuthority(ctx)
	if override := home.Override(ctx); override != "" {
		configPath := filepath.Join(override, "config.yaml")
		cfg := map[string]any{}
		if exists, err := config.ConfigExists(ctx, configPath); err == nil && exists {
			if loaded, err := loadBridgeConfig(configPath); err == nil {
				cfg = loaded
			}
		}
		var maxTurns any
		if agentCfg, ok := cfg["agent"].(map[string]any); ok {
			maxTurns = agentCfg["max_turns"]
		}
		return config.ResolveTurnLimit(maxTurns, config.TurnLimitUnlimited)
	}
	var raw any
	if v, ok := os.LookupEnv("ICEBATH_MAX_ITERATIONS"); ok {
		raw = v
	}
	return config.ResolveTurnLimit(raw, config.TurnLimitUnlimited)
}
