package gateway

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Handoff verbs of a local session, queueing it for a messaging platform's home channel and polling the row
func handoffHandlers(c *AuthorityConnection) map[string]controlHandler {
	return map[string]controlHandler{
		"handoff.request": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return requestHandoff(ctx, c, ref, params)
		},
		"handoff.state": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return handoffState(ctx, c, ref, params)
		},
		"handoff.fail": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return failHandoff(ctx, c, ref, params)
		},
	}
}

// Refusal carrying on one line the handoff text the reference CLI printed for the same outcome
func handoffRefusal(ctx context.Context, reason, key string, kwargs map[string]any, more ...string) error {
	lines := []string{i18n.T(ctx, "cli.commands.handoff."+key, "", kwargs)}
	for _, k := range more {
		lines = append(lines, i18n.T(ctx, "cli.commands.handoff."+k, "", kwargs))
	}
	detail := strings.Join(lines, " ")
	return state.NewRuntimeStoreError(reason, nil, &detail)
}

// Home channel of a platform the gateway delivers to, refused with the reference CLI text when it cannot
func handoffDestination(ctx context.Context, platformName string) (*HomeChannel, error) {
	kwargs := map[string]any{"platform": platformName}
	platform, err := ParsePlatform(ctx, platformName)
	if err != nil {
		return nil, handoffRefusal(ctx, "invalid_params", "unknown_platform", kwargs)
	}
	cfg, err := LoadGatewayConfig(ctx)
	if err != nil {
		return nil, handoffRefusal(ctx, "invalid_params", "config_load_failed", map[string]any{"error": err.Error()})
	}
	if pcfg := cfg.Platforms[platform]; pcfg == nil || !pcfg.Enabled {
		fronted := false
		if relay := cfg.Platforms[PlatformRelay]; relay != nil && relay.Enabled {
			for _, identity := range RelayPlatformIdentities() {
				fronted = fronted || identity[0] == platformName
			}
		}
		if !fronted {
			return nil, handoffRefusal(ctx, "invalid_params", "platform_not_enabled", kwargs, "enable_platform_hint")
		}
	}
	home := cfg.GetHomeChannel(platform)
	if home == nil || home.ChatID == "" {
		return nil, handoffRefusal(ctx, "invalid_params", "no_home_channel", kwargs, "set_home_hint")
	}
	return home, nil
}

// Live local session the caller controls, refused when it is unknown or the caller lacks the capability
func handoffSession(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object, capability string, extra ...string) error {
	if paramsOutside(params, append([]string{"session_id", "profile"}, extra...)...) || ref.SessionID == "" {
		return runtimeRefusal("invalid_params")
	}
	if _, ok := c.authority.live(ref.SessionID); !ok {
		return runtimeRefusal("not_found")
	}
	return authorizeBusy(ctx, c.authority, c.actor, ref, params, capability)
}

// Queues the idle session for the platform's home channel, the gateway handoff watcher claiming it next
func requestHandoff(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	raw, isStr := params.GetOr("platform", nil).(string)
	if !isStr {
		return nil, runtimeRefusal("invalid_params")
	}
	if err := handoffSession(ctx, c, ref, params, "session:control", "platform"); err != nil {
		return nil, err
	}
	platformName := strings.ToLower(strings.TrimSpace(raw))
	if platformName == "" {
		return nil, handoffRefusal(ctx, "invalid_params", "usage", nil, "usage_detail_1")
	}
	home, err := handoffDestination(ctx, platformName)
	if err != nil {
		return nil, err
	}
	a := c.authority
	handle, err := a.handle(ctx, ref)
	if err != nil {
		return nil, err
	}
	if handle.ExecutionState != "idle" {
		detail := i18n.T(ctx, "cli.commands.shared.agent_busy", "", map[string]any{"command": "/handoff"})
		return nil, state.NewRuntimeStoreError("session_busy", nil, &detail)
	}
	queued, err := forwardOffThread(ctx, func() (bool, error) {
		row, err := a.DB.GetSession(ctx, ref.SessionID)
		if err != nil {
			return false, err
		}
		if row == nil || row.Len() == 0 {
			if _, err := a.DB.SetSessionTitle(ctx, ref.SessionID, "handoff-"+pystr.Slice(ref.SessionID, 0, 8)); err != nil {
				return false, err
			}
		}
		return a.DB.RequestHandoff(ctx, ref.SessionID, platformName)
	})
	if err != nil {
		return nil, err
	}
	if !queued {
		return nil, handoffRefusal(ctx, "session_busy", "already_in_flight", nil)
	}
	return pyjson.ObjectOf("queued", true, "session_key", ref.SessionID, "platform", platformName, "home_name", home.Name), nil
}

// Field of a handoff record as text, empty when unset
func handoffText(record *pyjson.Object, key string) string {
	if record == nil {
		return ""
	}
	value, _ := record.Get(key)
	if !pyrepr.Truthy(value) {
		return ""
	}
	return pyrepr.Str(value)
}

// State, platform and error of the session's handoff row, empty fields when none was requested
func handoffState(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	if err := handoffSession(ctx, c, ref, params, "session:read"); err != nil {
		return nil, err
	}
	record := c.authority.DB.GetHandoffState(ctx, ref.SessionID)
	return pyjson.ObjectOf("state", handoffText(record, "state"), "platform", handoffText(record, "platform"), "error", handoffText(record, "error")), nil
}

// Fails a handoff the watcher has not claimed yet, reporting the state it holds otherwise
func failHandoff(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	if err := handoffSession(ctx, c, ref, params, "session:control", "error"); err != nil {
		return nil, err
	}
	reason := "handoff failed"
	if raw := params.GetOr("error", nil); pyrepr.Truthy(raw) {
		reason = strings.TrimSpace(pyrepr.Str(raw))
	}
	if utf8.RuneCountInString(reason) > 500 {
		reason = string([]rune(reason)[:500])
	}
	db := c.authority.DB
	failed, err := db.FailHandoff(ctx, ref.SessionID, reason, []string{"pending"})
	if err != nil {
		return nil, err
	}
	current := "failed"
	if !failed {
		current = handoffText(db.GetHandoffState(ctx, ref.SessionID), "state")
	}
	return pyjson.ObjectOf("failed", failed, "state", current), nil
}
