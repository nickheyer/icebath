package gatewayrun

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Personality catalog operations of the commands package, assigned in cmd/icebath wiring
var (
	ActivePersonalityName  func(cfg map[string]any) string
	AvailablePersonalities func(cfg map[string]any) map[string]any
	DescribePersonality    func(value any, width int) string
	PersistPersonality     func(ctx context.Context, value any) bool
	ResolvePersonality     func(value any, cfg map[string]any) (string, string, error)
)

// Shows or switches the codex runtime, evicting the cached agent when the change needs a new session
func (r *GatewayRunner) handleCodexRuntimeCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	newValue, problems := provider.CodexRuntimeParseArgs(pystr.Strip(event.GetCommandArgs()))
	if len(problems) > 0 {
		return "❌ " + strings.Join(problems, "\n❌ "), nil
	}
	h := home.Dir(ctx)
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return i18n.T(ctx, "gateway.codex_runtime.config_load_failed", "", map[string]any{"error": err.Error()}), nil
	}
	var persist func(map[string]any) error
	if newValue != nil {
		persist = func(updated map[string]any) error { return config.SaveConfig(ctx, h, updated, config.SaveOptions{}) }
	}
	result, err := provider.ApplyCodexRuntime(ctx, map[string]any(cfg), newValue, persist)
	if err != nil {
		return "", err
	}
	if result.Success && newValue != nil && result.RequiresNewSession {
		if sessionKey, err := r.sessionKeyForSource(ctx, event.Source); err != nil {
			runLog().Debug("could not evict cached agent after codex-runtime change", "exc_info", err)
		} else {
			r.evictCachedAgent(ctx, sessionKey, false, false)
		}
	}
	mark := "✗"
	if result.Success {
		mark = "✓"
	}
	return mark + " " + result.Message, nil
}

// Lists the personalities or persists the selected one for the next turn to resolve
func (r *GatewayRunner) handlePersonalityCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	args := pystr.Strip(event.GetCommandArgs())
	cfg := gateway.LoadGatewayConfigMap(ctx, "")
	if cfg == nil {
		cfg = map[string]any{}
	}
	personalities := AvailablePersonalities(cfg)
	names := config.MappingKeys(personalities)
	if args == "" {
		current := ActivePersonalityName(cfg)
		lines := []string{i18n.T(ctx, "gateway.personality.header", "", nil), i18n.T(ctx, "gateway.personality.none_option", "", nil)}
		for _, name := range names {
			marker := ""
			if name == current {
				marker = " ✓"
			}
			lines = append(lines, i18n.T(ctx, "gateway.personality.item", "", map[string]any{"name": name + marker,
				"preview": DescribePersonality(personalities[name], 50)}))
		}
		lines = append(lines, i18n.T(ctx, "gateway.personality.usage", "", nil))
		return strings.Join(lines, "\n"), nil
	}
	name, _, err := ResolvePersonality(args, cfg)
	if err != nil {
		if !isValueError(err) {
			return "", err
		}
		quoted := make([]string, len(names))
		for i, n := range names {
			quoted[i] = "`" + n + "`"
		}
		return i18n.T(ctx, "gateway.personality.unknown", "", map[string]any{"name": pystr.Lower(args),
			"available": "`none`, " + strings.Join(quoted, ", ")}), nil
	}
	if !PersistPersonality(ctx, name) {
		return i18n.T(ctx, "gateway.personality.save_failed", "", map[string]any{"error": i18n.T(ctx, "gateway.personality.err_config_write", "", nil)}), nil
	}
	if name == "" {
		return i18n.T(ctx, "gateway.personality.cleared", "", nil), nil
	}
	return i18n.T(ctx, "gateway.personality.set_to", "", map[string]any{"name": name}), nil
}
