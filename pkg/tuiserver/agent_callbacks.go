package tuiserver

import (
	"context"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Prefill message loaders of the CLI config package, assigned by the command wiring from the app package
var Prefill struct {
	ResolveFile func(ctx context.Context, cfg map[string]any) string
	Load        func(ctx context.Context, filePath, baseDir string) []any
}

// Agent turn limit, the env override first then agent max_turns then the top level key
func cfgMaxTurns(cfg map[string]any, fallback int) int {
	if envValue := os.Getenv("ICEBATH_TUI_MAX_TURNS"); envValue != "" {
		return config.ResolveTurnLimit(envValue, fallback)
	}
	agentCfg, _ := cfg["agent"].(map[string]any)
	raw := agentCfg["max_turns"]
	if raw == nil {
		raw = cfg["max_turns"]
	}
	if raw == nil {
		return fallback
	}
	return config.ResolveTurnLimit(raw, fallback)
}

// Skills named in ICEBATH_TUI_SKILLS, comma or newline separated, first occurrence kept
func parseTUISkillsEnv() []string {
	raw := strings.ReplaceAll(os.Getenv("ICEBATH_TUI_SKILLS"), "\n", ",")
	seen := map[string]bool{}
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" && !seen[part] {
			seen[part] = true
			out = append(out, part)
		}
	}
	return out
}

// Configured fallback chain, fallback providers first and the legacy fallback model merged after
func loadFallbackModel(ctx context.Context) []map[string]any {
	return provider.GetFallbackChain(loadCfg(ctx))
}

// Configured prefill messages resolved like the CLI, relative paths against the active profile home
func loadPrefillMessages(ctx context.Context) []any {
	return Prefill.Load(ctx, Prefill.ResolveFile(ctx, loadCfg(ctx)), home.Dir(ctx))
}
