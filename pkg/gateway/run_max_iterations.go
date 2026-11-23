package gateway

import (
	"context"
	"os"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
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

// Per turn iteration budget of the profile in ctx, .env reloaded first for rotated keys outside multiplex
func CurrentMaxIterations(ctx context.Context) int {
	if !provider.IsMultiplexActive(ctx) {
		_, _ = provider.LoadIcebathDotenv(ctx, provider.DotenvLoadOptions{Home: home.Dir(context.Background())})
	}
	var raw any
	if n, set := settings.MaxTurns.Lookup(ctx); set {
		raw = n
	}
	return config.ResolveTurnLimit(raw, config.TurnLimitUnlimited)
}
