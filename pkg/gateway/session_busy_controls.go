package gateway

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

var busyModes = map[string]bool{"interrupt": true, "steer": true, "queue": true}

var verboseCycle = []string{"off", "new", "all", "verbose"}

var yoloWords = map[string]bool{"1": true, "on": true, "true": true, "0": false, "off": false, "false": false}

// Sources whose unset reasoning flag follows display.show_reasoning
var configDefaultSources = map[string]bool{"tui": true, "gui": true}

// Runner reading the routed profile busy input mode, GatewayRunner._effective_busy_input_mode
type busyModeRunner interface {
	EffectiveBusyInputMode(ctx context.Context, source *SessionSource) string
}

// Runner holding the session scoped /fast pick, GatewayRunner._set_session_service_tier_override
type serviceTierRunner interface {
	SetSessionServiceTierOverride(sessionKey string, serviceTier *string, clear bool)
}

// Param keys outside the allowed set
func paramsOutside(params *pyjson.Object, allowed ...string) bool {
	for _, key := range params.Keys {
		if !slices.Contains(allowed, key) {
			return true
		}
	}
	return false
}

// Live session of an id, a KeyError like the authority dict for an unknown one
func liveSession(a *SessionAuthority, sessionID string) (*LiveSession, error) {
	live, ok := a.live(sessionID)
	if !ok {
		return nil, &pyKeyError{sessionID}
	}
	return live, nil
}

// Checks the capability on the session and that a non null profile names this owner
func authorizeBusy(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef, params *pyjson.Object, capability string) error {
	if err := a.Authorize(ctx, actor, ref, capability); err != nil {
		return err
	}
	raw, _ := params.Get("profile")
	if raw == nil {
		return nil
	}
	profile, ok := raw.(string)
	if !ok {
		return runtimeRefusal("invalid_params")
	}
	if profile != "" && !profiles.ProfileMatchesHome(ctx, profile, a.ProfileID) {
		return runtimeRefusal("profile_mismatch")
	}
	return nil
}

// Capability a read or a write of a session preference needs
func preferenceCapability(write bool) string {
	if write {
		return "session:control"
	}
	return "session:read"
}

// Session scoped busy input mode and the verbose, reasoning, tier and yolo preferences it routes
func busyConfig(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef, params *pyjson.Object, write bool) (*pyjson.Object, error) {
	key, _ := params.Get("key")
	if write && key == "model" {
		return nil, runtimeRefusal("use_session_mutation_model")
	}
	allowed := []string{"session_id", "profile", "key"}
	if write {
		allowed = append(allowed, "value")
	}
	extra := paramsOutside(params, allowed...)
	switch {
	case key == "verbose" && !extra && ref.SessionID != "":
		return verboseConfig(ctx, a, actor, ref, params, write)
	case key == "show_reasoning" && !extra && ref.SessionID != "":
		return showReasoningConfig(ctx, a, actor, ref, params, write)
	case key == "service_tier" && write && !extra && ref.SessionID != "":
		return serviceTierConfig(ctx, a, actor, ref, params)
	case key == "yolo" && !extra && ref.SessionID != "":
		return yoloConfig(ctx, a, actor, ref, params, write)
	}
	value, _ := params.Get("value")
	mode, _ := value.(string)
	if extra || ref.SessionID == "" || key != "busy" || (write && !busyModes[mode]) {
		return nil, runtimeRefusal("invalid_params")
	}
	if err := authorizeBusy(ctx, a, actor, ref, params, preferenceCapability(write)); err != nil {
		return nil, err
	}
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return nil, err
	}
	if write {
		live.BusyInputMode = mode
	}
	current := live.BusyInputMode
	if current == "" {
		policy, err := policyForSource(ctx, a.Runner, live.Source)
		if err != nil {
			return nil, err
		}
		fallback := "interrupt"
		if policy != nil && policy.Source == "tui" {
			fallback = "queue"
		}
		if policy != nil {
			cfg, err := policy.config(ctx, nil, true)
			if err != nil {
				return nil, err
			}
			display, err := pyGet(cfg, "display", nil)
			if err != nil {
				return nil, err
			}
			if !pyrepr.Truthy(display) {
				display = map[string]any{}
			}
			chosen, err := pyGet(display, "busy_input_mode", nil)
			if err != nil {
				return nil, err
			}
			if !pyrepr.Truthy(chosen) {
				chosen = fallback
			}
			current, _ = chosen.(string)
		} else {
			runner, ok := a.Runner.(busyModeRunner)
			if !ok {
				return nil, errors.New("AttributeError: runner has no attribute '_effective_busy_input_mode'")
			}
			current = runner.EffectiveBusyInputMode(ctx, live.Source)
		}
		if !busyModes[current] {
			current = fallback
		}
	}
	return pyjson.ObjectOf("key", "busy", "value", current, "scope", "session"), nil
}

// Session scoped tool progress mode the next tool events render with, cycled or set
func verboseConfig(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef, params *pyjson.Object, write bool) (*pyjson.Object, error) {
	value, _ := params.Get("value")
	word, isStr := value.(string)
	if write && !(isStr && (slices.Contains(verboseCycle, word) || word == "cycle")) {
		return nil, runtimeRefusal("invalid_params")
	}
	if err := authorizeBusy(ctx, a, actor, ref, params, preferenceCapability(write)); err != nil {
		return nil, err
	}
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return nil, err
	}
	current := live.ToolProgressMode
	if current == "" {
		current = "all"
	}
	if write {
		if word == "cycle" {
			index := slices.Index(verboseCycle, current)
			if index < 0 {
				return nil, fmt.Errorf("ValueError: %s is not in tuple", pyrepr.Repr(current))
			}
			current = verboseCycle[(index+1)%4]
		} else {
			current = word
		}
		live.ToolProgressMode = current
	}
	return pyjson.ObjectOf("key", "verbose", "value", current, "scope", "session"), nil
}

// Session scoped reasoning display, the launch default when the session never set it
func showReasoningConfig(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef, params *pyjson.Object, write bool) (*pyjson.Object, error) {
	value, _ := params.Get("value")
	word := pystr.Lower(pystr.Strip(pyrepr.Str(value)))
	setting, known := yoloWords[word]
	if write && !known {
		return nil, runtimeRefusal("invalid_params")
	}
	if err := authorizeBusy(ctx, a, actor, ref, params, preferenceCapability(write)); err != nil {
		return nil, err
	}
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return nil, err
	}
	if write {
		live.ShowReasoning = &setting
	}
	var shown bool
	if live.ShowReasoning != nil {
		shown = *live.ShowReasoning
	} else {
		policy, err := policyForSource(ctx, a.Runner, live.Source)
		if err != nil {
			return nil, err
		}
		if shown, err = launchShowsReasoning(ctx, policy, a); err != nil {
			return nil, err
		}
	}
	flag := "0"
	if shown {
		flag = "1"
	}
	return pyjson.ObjectOf("key", "show_reasoning", "value", flag, "scope", "session"), nil
}

// Whether an unset reasoning flag shows reasoning, display.show_reasoning for an Ink or Desktop launch
func launchShowsReasoning(ctx context.Context, policy *LocalSessionPolicy, a *SessionAuthority) (bool, error) {
	if policy == nil || !configDefaultSources[policy.Source] {
		return false, nil
	}
	cfg, err := policy.config(ctx, a, false)
	if err != nil {
		return false, err
	}
	display := config.LocalDisplayConfig(cfg["display"])
	value, ok := display["show_reasoning"]
	if !ok {
		return true, nil
	}
	return pyrepr.Truthy(value), nil
}

// Session scoped fast tier pick the next turn reads, normal pinning the normal tier
func serviceTierConfig(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef, params *pyjson.Object) (*pyjson.Object, error) {
	value, _ := params.Get("value")
	raw, isStr := value.(string)
	if !isStr {
		return nil, runtimeRefusal("invalid_params")
	}
	tier, err := provider.ParseExactServiceTier(raw)
	if err != nil {
		return nil, runtimeRefusal("invalid_params")
	}
	if err := authorizeBusy(ctx, a, actor, ref, params, "session:control"); err != nil {
		return nil, err
	}
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return nil, err
	}
	runner, ok := a.Runner.(serviceTierRunner)
	if !ok {
		return nil, errors.New("AttributeError: runner has no attribute '_set_session_service_tier_override'")
	}
	var picked *string
	if tier != "" {
		picked = &tier
	}
	runner.SetSessionServiceTierOverride(live.Route, picked, false)
	word := tier
	if word == "" {
		word = "normal"
	}
	return pyjson.ObjectOf("key", "service_tier", "value", word, "scope", "session"), nil
}

// Session scoped approval bypass, persisted on the route before the live flip
func yoloConfig(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef, params *pyjson.Object, write bool) (*pyjson.Object, error) {
	value, _ := params.Get("value")
	word := pystr.Lower(pystr.Strip(pyrepr.Str(value)))
	if _, known := yoloWords[word]; write && value != nil && !known {
		return nil, runtimeRefusal("invalid_params")
	}
	if err := authorizeBusy(ctx, a, actor, ref, params, preferenceCapability(write)); err != nil {
		return nil, err
	}
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return nil, err
	}
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil {
		return nil, err
	}
	store := a.Runner.SessionStore()
	var persistedYolo *bool
	if store != nil {
		if entry := store.LookupBySessionKey(ctx, live.Route); entry != nil {
			persistedYolo = entry.Yolo
		}
	}
	tools.RestoreGatewayYolo(live.Route, persistedYolo, policy != nil && policy.Yolo())
	persisted := persistedYolo != nil && *persistedYolo
	enabled := tools.IsSessionYoloEnabled(live.Route) || persisted
	if write {
		var persist func(on bool) error
		if store != nil {
			persist = func(on bool) error {
				_, err := store.SetSessionYolo(ctx, live.Route, &on)
				return err
			}
		}
		var choice *bool
		if value != nil {
			pick := yoloWords[word]
			choice = &pick
		}
		enabled = tools.ToggleSessionYolo(live.Route, choice, persisted, persist)
	}
	flag := "0"
	if enabled {
		flag = "1"
	}
	return pyjson.ObjectOf("key", "yolo", "value", flag, "scope", "session"), nil
}
