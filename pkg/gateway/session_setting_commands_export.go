package gateway

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Runner reading the busy input mode a messaging source runs under
type busyInputModeRunner interface {
	EffectiveBusyInputMode(ctx context.Context, source *SessionSource) string
}

// Live local session the source addresses, nil for every other route
func LocalLive(runner any, source *SessionSource) *LiveSession {
	return localLive(runner, source)
}

// Busy mode the local session's next input runs under, its own pin, else its policy, else the runner's
func SessionBusyMode(ctx context.Context, runner AuthorityRunner, live *LiveSession) (string, error) {
	valid := func(v any) bool { return v == "interrupt" || v == "steer" || v == "queue" }
	if valid(live.BusyInputMode) {
		return live.BusyInputMode, nil
	}
	policy, err := policyForSource(ctx, runner, live.Source)
	if err != nil {
		return "", err
	}
	fallback := "interrupt"
	if policy != nil && policy.Source == "tui" {
		fallback = "queue"
	}
	if policy == nil {
		busy, ok := runner.(busyInputModeRunner)
		if !ok {
			return "", fmt.Errorf("AttributeError: %s object has no attribute '_effective_busy_input_mode'", pyrepr.TypeName(runner))
		}
		return busy.EffectiveBusyInputMode(ctx, live.Source), nil
	}
	cfg, _, err := policy.EffectiveFor(ctx, nil, true)
	if err != nil {
		return "", err
	}
	var value any
	if display := cfg["display"]; pyrepr.Truthy(display) {
		section, ok := display.(map[string]any)
		if !ok {
			return "", fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(display))
		}
		value = section["busy_input_mode"]
	}
	if !pyrepr.Truthy(value) {
		return fallback, nil
	}
	if valid(value) {
		return value.(string), nil
	}
	return fallback, nil
}
