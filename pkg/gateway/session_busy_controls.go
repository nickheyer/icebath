package gateway

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

var busyModes = map[string]bool{"interrupt": true, "steer": true, "queue": true}

var verboseCycle = []string{"off", "new", "all", "verbose"}

var yoloWords = map[string]bool{"1": true, "on": true, "true": true, "0": false, "off": false, "false": false}

// Sources whose unset reasoning flag follows display.show_reasoning
var configDefaultSources = map[string]bool{"tui": true, "gui": true}

// Runner holding the session scoped /fast pick, GatewayRunner._set_session_service_tier_override
type serviceTierRunner interface {
	SetSessionServiceTierOverride(sessionKey string, serviceTier *string, clear bool)
}

// Param keys outside the allowed set
func paramsOutside(params *jsonutil.Object, allowed ...string) bool {
	for _, key := range params.Keys() {
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
func authorizeBusy(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef, params *jsonutil.Object, capability string) error {
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

// Context whose settings reads and session writes belong to one session of the authority's profile
func sessionSettingsCtx(ctx context.Context, a *SessionAuthority, sessionID string) context.Context {
	return settings.WithSession(profileRuntimeScope(ctx, a.ProfileID, true), sessionID)
}

// Records a session's own choice in the settings session layer, which validates it
func storeSessionSetting(ctx context.Context, a *SessionAuthority, sessionID, key string, value any) error {
	if _, err := settings.Set(sessionSettingsCtx(ctx, a, sessionID), key, value, settings.Session); err != nil {
		return settingsRefusal(err)
	}
	return nil
}

// Session a local live session runs, the id its source chat names
func liveSessionID(live *LiveSession) string {
	if live == nil || live.Source == nil {
		return ""
	}
	return live.Source.ChatID
}

// Value a local live session chose for a setting itself, false when it follows the profile
func SessionOwnSetting(ctx context.Context, live *LiveSession, key string) (any, bool) {
	sid := liveSessionID(live)
	if sid == "" {
		return nil, false
	}
	v, err := settings.Resolve(settings.WithSession(ctx, sid), key)
	if err != nil || v.Source != settings.SourceSession {
		return nil, false
	}
	return v.Value, true
}

// Capability a read or a write of a session preference needs
func preferenceCapability(write bool) string {
	if write {
		return "session:control"
	}
	return "session:read"
}

// Session scoped busy input mode and the verbose, reasoning, tier and yolo preferences it routes
func busyConfig(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef, params *jsonutil.Object, write bool) (*jsonutil.Object, error) {
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
	if _, err := liveSession(a, ref.SessionID); err != nil {
		return nil, err
	}
	if write {
		if err := storeSessionSetting(ctx, a, ref.SessionID, settings.BusyMode.Key, mode); err != nil {
			return nil, err
		}
	}
	current := settings.BusyMode.Get(sessionSettingsCtx(ctx, a, ref.SessionID))
	return jsonutil.ObjectOf("key", "busy", "value", current, "scope", "session"), nil
}

// Session scoped tool progress mode the next tool events render with, cycled or set
func verboseConfig(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef, params *jsonutil.Object, write bool) (*jsonutil.Object, error) {
	value, _ := params.Get("value")
	word, isStr := value.(string)
	if write && !(isStr && (slices.Contains(verboseCycle, word) || word == "cycle")) {
		return nil, runtimeRefusal("invalid_params")
	}
	if err := authorizeBusy(ctx, a, actor, ref, params, preferenceCapability(write)); err != nil {
		return nil, err
	}
	if _, err := liveSession(a, ref.SessionID); err != nil {
		return nil, err
	}
	current := settings.ToolProgress.Get(sessionSettingsCtx(ctx, a, ref.SessionID))
	if write {
		if word == "cycle" {
			index := slices.Index(verboseCycle, current)
			if index < 0 {
				return nil, fmt.Errorf("tool progress mode %s is not one of %s", current, strings.Join(verboseCycle, ", "))
			}
			current = verboseCycle[(index+1)%len(verboseCycle)]
		} else {
			current = word
		}
		if err := storeSessionSetting(ctx, a, ref.SessionID, settings.ToolProgress.Key, current); err != nil {
			return nil, err
		}
	}
	return jsonutil.ObjectOf("key", "verbose", "value", current, "scope", "session"), nil
}

// Session scoped reasoning display, the launch default when the session never set it
func showReasoningConfig(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef, params *jsonutil.Object, write bool) (*jsonutil.Object, error) {
	value, _ := params.Get("value")
	word := strings.ToLower(strings.TrimSpace(anyutil.String(value)))
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
		if err := storeSessionSetting(ctx, a, ref.SessionID, settings.ShowThinking.Key, setting); err != nil {
			return nil, err
		}
	}
	shown, err := sessionShowsReasoning(sessionSettingsCtx(ctx, a, ref.SessionID), a, live)
	if err != nil {
		return nil, err
	}
	flag := "0"
	if shown {
		flag = "1"
	}
	return jsonutil.ObjectOf("key", "show_reasoning", "value", flag, "scope", "session"), nil
}

// Whether a session shows the model's thinking: its own pick, else the profile's for a terminal UI launch, else hidden
func sessionShowsReasoning(sctx context.Context, a *SessionAuthority, live *LiveSession) (bool, error) {
	v, err := settings.Resolve(sctx, settings.ShowThinking.Key)
	if err != nil {
		return false, err
	}
	if v.Source == settings.SourceSession {
		on, _ := v.Value.(bool)
		return on, nil
	}
	policy, err := policyForSource(sctx, a.Runner, live.Source)
	if err != nil {
		return false, err
	}
	return launchShowsReasoning(sctx, policy), nil
}

// Whether a session that never picked shows reasoning, the profile's choice for a terminal UI launch
func launchShowsReasoning(ctx context.Context, policy *LocalSessionPolicy) bool {
	if policy == nil || !configDefaultSources[policy.Source] {
		return false
	}
	return settings.ShowThinking.Get(settings.WithPlatform(settings.WithoutSession(ctx), settings.LocalPlatform))
}

// Session scoped fast tier pick the next turn reads, normal pinning the normal tier
func serviceTierConfig(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef, params *jsonutil.Object) (*jsonutil.Object, error) {
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
		return nil, errors.New("the gateway runner cannot set a session service tier override")
	}
	if err := storeSessionSetting(ctx, a, ref.SessionID, settings.FastMode.Key, provider.ServiceTierWord(tier)); err != nil {
		return nil, err
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
	return jsonutil.ObjectOf("key", "service_tier", "value", word, "scope", "session"), nil
}

// Session scoped approval bypass, persisted on the route before the live flip
func yoloConfig(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef, params *jsonutil.Object, write bool) (*jsonutil.Object, error) {
	value, _ := params.Get("value")
	word := strings.ToLower(strings.TrimSpace(anyutil.String(value)))
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
	tools.RestoreGatewayYolo(ctx, live.Route, persistedYolo, policy != nil && policy.Yolo())
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
		enabled = tools.ToggleSessionYolo(ctx, live.Route, choice, persisted, persist)
	}
	flag := "0"
	if enabled {
		flag = "1"
	}
	return jsonutil.ObjectOf("key", "yolo", "value", flag, "scope", "session"), nil
}
