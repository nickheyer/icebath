package gateway

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

var sessionControlKeys = map[string]bool{"busy": true, "yolo": true, "show_reasoning": true, "service_tier": true, "model": true}

var boolWords = map[string]bool{"1": true, "on": true, "true": true, "yes": true, "0": false, "off": false, "false": false, "no": false}

// Aliases whose answer reports the setting as on or off
var onOffSettings = map[string]bool{"density": true, "battery": true, "timestamps": true}

// Show and hide words of the reasoning command that toggle display rather than effort
var ReasoningDisplayToggles = map[string]bool{"show": true, "on": true, "hide": false, "off": false}

// Words of the reasoning command that show the whole thinking text or clamp it to a few lines
var ReasoningFullWords = map[string]bool{"full": true, "all": true, "clamp": false, "collapse": false, "short": false}

// Resolves a personality name against the config, assigned in cmd/icebath/wiring.go
var ResolvePersonality func(value any, cfg config.Tree) (string, string, error)

// Runner resolving the routed profile's tier, GatewayRunner._resolve_session_service_tier
type sessionTierRunner interface {
	ResolveSessionServiceTier(ctx context.Context, source *SessionSource, sessionKey string) (*string, error)
}

// Runner applying a reasoning pick to a route, GatewayRunner._apply_reasoning_selection
type reasoningSelectionRunner interface {
	ApplyReasoningSelection(ctx context.Context, sessionKey, platformKey, value string, persistGlobal bool) string
}

// Agent API key getattr reads for the ultrafast probe
type agentAPIKey interface{ APIKey() string }

// Lowercased stripped text of a value, empty for None
func settingWord(value any) string {
	if value == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(pyrepr.Str(value)))
}

// Refusal of a settings write: a bad value invalid_params, a pinned setting permission_denied, else storage_unavailable
func settingsRefusal(err error) error {
	var bad *settings.ValueError
	switch {
	case errors.As(err, &bad), errors.Is(err, settings.ErrUnknownKey), errors.Is(err, settings.ErrScope):
		return runtimeRefusal("invalid_params")
	case errors.Is(err, settings.ErrLocked):
		return runtimeRefusal("permission_denied")
	}
	return runtimeRefusal("storage_unavailable")
}

// Writes one setting of the owning profile through the settings writer
func setOwnerSetting(sctx context.Context, key string, value any) (settings.Change, error) {
	change, err := settings.Set(sctx, key, value, settings.Global)
	if err != nil {
		return settings.Change{}, settingsRefusal(err)
	}
	return change, nil
}

// Word a config.set answer reports for a setting's new value
func settingAnswerWord(key string, value any) any {
	if on, isBool := value.(bool); isBool && onOffSettings[key] {
		if on {
			return "on"
		}
		return "off"
	}
	if value == nil {
		return ""
	}
	if _, isBool := value.(bool); isBool {
		return value
	}
	return pyrepr.Str(value)
}

// Writes the setting a config.set key or alias names in the owning profile, returning the answer fields
func setProfileSetting(ctx context.Context, c *AuthorityConnection, key string, value any) (map[string]any, error) {
	sctx := profileRuntimeScope(ctx, c.authority.ProfileID, true)
	raw := settingWord(value)
	if section, ok := strings.CutPrefix(key, "details_mode."); ok {
		if !slices.Contains(settings.DetailSections, section) {
			return nil, runtimeRefusal("invalid_params")
		}
		if raw == "" {
			if _, err := settings.Reset(sctx, "display.sections."+section, settings.Global); err != nil {
				return nil, settingsRefusal(err)
			}
			return map[string]any{"value": ""}, nil
		}
		key = "display.sections." + section
	}
	if key == "skin" && raw == "" {
		return nil, runtimeRefusal("invalid_params")
	}
	e := settings.Find(key)
	if e == nil {
		return nil, runtimeRefusal("invalid_params")
	}
	next := value
	if raw == "" || raw == "toggle" {
		if flipped := settings.Flipped(sctx, e); flipped != nil {
			next = flipped
		}
	}
	change, err := setOwnerSetting(sctx, e.Info().Key, next)
	if err != nil {
		return nil, err
	}
	return map[string]any{"value": settingAnswerWord(key, change.New.Value)}, nil
}

// Display language of the owning profile, a language one of its layers supplies, stored as its canonical id
func setDisplayLanguage(ctx context.Context, c *AuthorityConnection, value any) (map[string]any, error) {
	sctx := profileRuntimeScope(ctx, c.authority.ProfileID, true)
	i18n.ResetLanguageCache()
	id := i18n.ResolveLanguageID(sctx, settingWord(value), home.Dir(sctx))
	if id == "" {
		return nil, runtimeRefusal("invalid_params")
	}
	if _, err := setOwnerSetting(sctx, settings.Language.Key, id); err != nil {
		return nil, err
	}
	i18n.ResetLanguageCache()
	return map[string]any{"value": id}, nil
}

// Profile level settings authorization plus the control capability
func authorizeSettingsWrite(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (*LocalSessionPolicy, error) {
	policy, err := authorizeSettingsRead(ctx, c, ref, params)
	if err != nil {
		return nil, err
	}
	if !c.actor.Capabilities["session:control"] {
		return nil, runtimeRefusal("permission_denied")
	}
	return policy, nil
}

// Tier the session's next turn runs, its fast pin else its policy's or the profile's
func currentTier(ctx context.Context, c *AuthorityConnection, ref SessionRef, policy *LocalSessionPolicy) (*string, error) {
	live, err := liveSession(c.authority, ref.SessionID)
	if err != nil {
		return nil, err
	}
	if policy != nil {
		return policy.CurrentServiceTier(ctx, c.authority.Runner, live.Route)
	}
	runner, ok := c.authority.Runner.(sessionTierRunner)
	if !ok {
		return nil, runnerLacks("resolve the session service tier")
	}
	return runner.ResolveSessionServiceTier(ctx, live.Source, live.Route)
}

// Refusal of a runner without a capability the session controls call
func runnerLacks(capability string) error {
	return &runnerCapabilityError{capability}
}

// Runner without a capability the session controls call
type runnerCapabilityError struct{ capability string }

func (e *runnerCapabilityError) Error() string {
	return "the gateway runner cannot " + e.capability
}

// Fast mode pick, status saying the mode in effect and a bare or toggle value flipping it: pinned on the session after a check of its route,
// or at global scope written for every session and pinned on the session it came from
func setFast(ctx context.Context, c *AuthorityConnection, ref SessionRef, policy *LocalSessionPolicy, value any, scope string) (map[string]any, error) {
	if (scope != "session" && scope != "global") || (scope == "session" && ref.SessionID == "") {
		return nil, runtimeRefusal("invalid_params")
	}
	raw := settingWord(value)
	if raw == "" || raw == "toggle" || raw == "status" {
		current := ""
		if scope == "global" {
			current = provider.ParseServiceTier(settings.FastMode.Get(settings.WithoutSession(profileRuntimeScope(ctx, c.authority.ProfileID, true))))
		} else {
			pinned, err := currentTier(ctx, c, ref, policy)
			if err != nil {
				return nil, err
			}
			if pinned != nil {
				current = *pinned
			}
		}
		if raw == "status" {
			return map[string]any{"key": "fast", "value": provider.ServiceTierWord(current), "scope": scope}, nil
		}
		raw = "fast"
		if provider.StaticTiers[current] {
			raw = "normal"
		}
	}
	tier, err := provider.ParseExactServiceTier(raw)
	if err != nil {
		return nil, runtimeRefusal("invalid_params")
	}
	word := provider.ServiceTierWord(tier)
	if scope == "global" {
		if _, err := forwardOffThread(ctx, func() (settings.Change, error) {
			return setOwnerSetting(profileRuntimeScope(ctx, c.authority.ProfileID, true), settings.FastMode.Key, word)
		}); err != nil {
			return nil, err
		}
		if ref.SessionID == "" {
			return map[string]any{"key": "fast", "value": word, "scope": "global"}, nil
		}
	} else if tier != "" {
		agent, err := c.authority.agent(ref)
		if err != nil {
			return nil, err
		}
		model, prov, baseURL, apiKey := "", "", "", ""
		if a, ok := agent.(agentModel); ok {
			model = a.Model()
		}
		if a, ok := agent.(agentProvider); ok {
			prov = a.Provider()
		}
		if a, ok := agent.(agentBaseURL); ok {
			baseURL = a.BaseURL()
		}
		if a, ok := agent.(agentAPIKey); ok {
			apiKey = a.APIKey()
		}
		if policy != nil {
			if model == "" {
				picked, err := policy.currentModel(ctx, c.authority)
				if err != nil {
					return nil, err
				}
				if picked != nil {
					model = *picked
				}
			}
			if prov == "" && pyrepr.Truthy(policy.Provider()) {
				prov = pyrepr.Str(policy.Provider())
			}
			if baseURL == "" && pyrepr.Truthy(policy.BaseURL()) {
				baseURL = pyrepr.Str(policy.BaseURL())
			}
		}
		offered, err := forwardOffThread(ctx, func() (bool, error) {
			overrideTier := ""
			if tier == "ultrafast" {
				overrideTier = tier
			}
			if model == "" || provider.ResolveFastModeOverrides(model, prov, baseURL, overrideTier) == nil {
				return false, nil
			}
			if tier != "ultrafast" {
				return true, nil
			}
			return provider.UltrafastOffered(model, prov, baseURL, apiKey)
		})
		if err != nil {
			return nil, err
		}
		if !offered {
			return nil, runtimeRefusal("fast_unavailable")
		}
	}
	runner, ok := c.authority.Runner.(serviceTierRunner)
	if !ok {
		return nil, runnerLacks("set a session service tier override")
	}
	live, err := liveSession(c.authority, ref.SessionID)
	if err != nil {
		return nil, err
	}
	if err := storeSessionSetting(ctx, c.authority, ref.SessionID, settings.FastMode.Key, word); err != nil {
		return nil, err
	}
	var picked *string
	if tier != "" {
		picked = &tier
	}
	runner.SetSessionServiceTierOverride(live.Route, picked, false)
	return map[string]any{"key": "fast", "value": word, "scope": scope}, nil
}

// Validates and persists the profile personality the next turn of each session resolves
func setPersonality(ctx context.Context, c *AuthorityConnection, value any) (map[string]any, error) {
	sctx := profileRuntimeScope(ctx, c.authority.ProfileID, true)
	text := ""
	if pyrepr.Truthy(value) {
		text = pyrepr.Str(value)
	}
	cfg, err := config.LoadContext(sctx, home.Dir(sctx), nil)
	if err != nil {
		return nil, err
	}
	name, _, err := ResolvePersonality(text, cfg)
	if err != nil {
		return nil, runtimeRefusal("unknown_personality")
	}
	if _, err := setOwnerSetting(sctx, settings.Personality.Key, name); err != nil {
		return nil, err
	}
	reported := "none"
	if pyrepr.Truthy(value) {
		reported = pyrepr.Str(value)
	}
	return map[string]any{"key": "personality", "value": reported, "history_reset": false}, nil
}

// Sets the owning profile's approval mode when a value is given, answered with the mode in effect
func setApprovalMode(ctx context.Context, c *AuthorityConnection, value any) (map[string]any, error) {
	sctx := profileRuntimeScope(ctx, c.authority.ProfileID, true)
	if word := settingWord(value); word != "" {
		if _, err := setOwnerSetting(sctx, settings.ApprovalMode.Key, word); err != nil {
			return nil, err
		}
	}
	h := home.Dir(sctx)
	return map[string]any{"key": "approvals.mode", "value": tools.GetApprovalMode(tools.EnvForProfileHome(sctx, nil, h))}, nil
}

// Settings a slash command owns, reasoning and the global yolo zap, through that command's own code
func setCommandSetting(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (map[string]any, error) {
	scope := params.GetOr("scope", "session")
	if paramsOutside(params, "session_id", "profile", "key", "value", "scope") || ref.SessionID == "" ||
		(scope != "session" && scope != "global") {
		return nil, runtimeRefusal("invalid_params")
	}
	if _, err := authorizeSettingsWrite(ctx, c, ref, params); err != nil {
		return nil, err
	}
	if err := c.authority.Authorize(ctx, c.actor, ref, "session:control"); err != nil {
		return nil, err
	}
	rawValue, _ := params.Get("value")
	value := settingWord(rawValue)
	if key, _ := params.Get("key"); key == "yolo" {
		if scope != "global" {
			return nil, runtimeRefusal("invalid_params")
		}
		on, known := boolWords[value]
		switch value {
		case "1":
			on, known = true, true
		case "0":
			on, known = false, true
		}
		if !known && value != "" && value != "toggle" {
			return nil, runtimeRefusal("invalid_params")
		}
		enable, err := forwardOffThread(ctx, func() (bool, error) {
			enable := on
			if !known {
				sctx := profileRuntimeScope(ctx, c.authority.ProfileID, true)
				enable = tools.GetApprovalMode(tools.EnvForProfileHome(sctx, nil, home.Dir(sctx))) != "off"
			}
			mode := "manual"
			if enable {
				mode = "off"
			}
			_, err := setApprovalMode(ctx, c, mode)
			return enable, err
		})
		if err != nil {
			return nil, err
		}
		flag := "0"
		if enable {
			flag = "1"
		}
		return map[string]any{"key": "yolo", "value": flag, "scope": "global"}, nil
	}
	if value == "" {
		return nil, runtimeRefusal("invalid_params")
	}
	if full, known := ReasoningFullWords[value]; known {
		if _, err := forwardOffThread(ctx, func() (settings.Change, error) {
			return setOwnerSetting(profileRuntimeScope(ctx, c.authority.ProfileID, true), settings.FullThinking.Key, full)
		}); err != nil {
			return nil, err
		}
		reported := "clamp"
		if full {
			reported = "full"
		}
		return map[string]any{"key": "reasoning", "value": reported, "scope": scope}, nil
	}
	if _, toggle := ReasoningDisplayToggles[value]; !toggle && value != "reset" && config.ParseReasoningEffort(value) == nil {
		return nil, runtimeRefusal("invalid_params")
	}
	live, err := liveSession(c.authority, ref.SessionID)
	if err != nil {
		return nil, err
	}
	runner, ok := c.authority.Runner.(reasoningSelectionRunner)
	if !ok {
		return nil, runnerLacks("apply a reasoning selection")
	}
	global := scope == "global"
	if err := commitReasoningCommand(ctx, c.authority, c.actor, ref, value, global); err != nil {
		return nil, err
	}
	if _, err := forwardOffThread(ctx, func() (string, error) {
		return runner.ApplyReasoningSelection(profileRuntimeScope(ctx, c.authority.ProfileID, true), live.Route, "cli", value, global), nil
	}); err != nil {
		return nil, err
	}
	reported := value
	if show, toggle := ReasoningDisplayToggles[value]; toggle {
		reported = "hide"
		if show {
			reported = "show"
		}
	}
	return map[string]any{"key": "reasoning", "value": reported, "scope": scope}, nil
}

// Profile preference verbs of a connection keyed by method name
func tuiConfigHandlers(c *AuthorityConnection) map[string]controlHandler {
	return map[string]controlHandler{
		"config.set": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return configSet(ctx, c, ref, params)
		},
		"settings.groups": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			if _, err := authorizeSettingsRead(ctx, c, ref, params); err != nil {
				return nil, err
			}
			return tuigateway.Plain(map[string]any{"groups": settings.Groups()}), nil
		},
		"settings.list": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return settingsList(ctx, c, ref, params)
		},
		"settings.set": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return settingsWrite(ctx, c, ref, params, false)
		},
		"settings.reset": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return settingsWrite(ctx, c, ref, params, true)
		},
	}
}

// Session controls a session scoped settings write goes through, keyed by setting
var sessionControlOf = map[string]string{settings.BusyMode.Key: "busy", settings.ToolProgress.Key: "verbose",
	settings.ShowThinking.Key: "show_reasoning", settings.FastMode.Key: "service_tier"}

// Drops a session's fast pick from the runner so its next turn follows the profile again
func clearSessionTier(a *SessionAuthority, sessionID string) error {
	live, err := liveSession(a, sessionID)
	if err != nil {
		return err
	}
	runner, ok := a.Runner.(serviceTierRunner)
	if !ok {
		return runnerLacks("set a session service tier override")
	}
	runner.SetSessionServiceTierOverride(live.Route, nil, true)
	return nil
}

// Rows of the settings a query selects in the owning profile, session values included when a session is named
func settingsList(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	if paramsOutside(params, "session_id", "profile", "group", "prefix", "search", "keys", "advanced") {
		return nil, runtimeRefusal("invalid_params")
	}
	if _, err := authorizeSettingsRead(ctx, c, ref, params); err != nil {
		return nil, err
	}
	var q settings.Query
	group, _ := params.Get("group")
	prefix, _ := params.Get("prefix")
	search, _ := params.Get("search")
	q.Group, q.Prefix, q.Search = settings.GroupID(pyrepr.Str(pyrepr.Or(group, ""))), pyrepr.Str(pyrepr.Or(prefix, "")), pyrepr.Str(pyrepr.Or(search, ""))
	if advanced, _ := params.Get("advanced"); advanced == true {
		q.Advanced = true
	}
	if keys, _ := params.Get("keys"); keys != nil {
		q.Keys = config.AsStrings(keys)
	}
	return forwardOffThread(ctx, func() (any, error) {
		sctx := settings.WithSession(profileRuntimeScope(ctx, c.authority.ProfileID, true), ref.SessionID)
		rows := settings.Rows(sctx, q)
		if rows == nil {
			rows = []settings.Row{}
		}
		return tuigateway.Plain(map[string]any{"rows": rows}), nil
	})
}

// Sets or resets one setting of the owning profile or session, session controls going through their own handler
func settingsWrite(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object, reset bool) (any, error) {
	if paramsOutside(params, "session_id", "profile", "key", "value", "scope") {
		return nil, runtimeRefusal("invalid_params")
	}
	if _, err := authorizeSettingsWrite(ctx, c, ref, params); err != nil {
		return nil, err
	}
	rawKey, _ := params.Get("key")
	e := settings.Find(pyrepr.Str(pyrepr.Or(rawKey, "")))
	if e == nil {
		return nil, runtimeRefusal("invalid_params")
	}
	key := e.Info().Key
	rawScope, _ := params.Get("scope")
	scopeWord := settingWord(rawScope)
	scope := settings.ParseScope(scopeWord)
	if scope == 0 && scopeWord != "" {
		return nil, runtimeRefusal("invalid_params")
	}
	raw, _ := params.Get("value")
	// A list or mapping arrives as wire objects; the registry checks plain values
	value := pyjson.Plain(raw)
	if control, ok := sessionControlOf[key]; ok && scope == settings.Session && !reset {
		if ref.SessionID == "" {
			return nil, runtimeRefusal("invalid_params")
		}
		word := settingWord(value)
		if on, isBool := value.(bool); isBool {
			word = map[bool]string{true: "1", false: "0"}[on]
		}
		if _, err := busyConfig(ctx, c.authority, c.actor, ref, pyjson.ObjectOf("session_id", ref.SessionID, "key", control, "value", word), true); err != nil {
			return nil, err
		}
	} else {
		_, err := forwardOffThread(ctx, func() (settings.Change, error) {
			sctx := settings.WithSession(profileRuntimeScope(ctx, c.authority.ProfileID, true), ref.SessionID)
			var change settings.Change
			var err error
			if reset {
				change, err = settings.Reset(sctx, key, scope)
			} else {
				change, err = settings.Set(sctx, key, value, scope)
			}
			if err != nil {
				return change, settingsRefusal(err)
			}
			return change, nil
		})
		if err != nil {
			return nil, err
		}
		if key == settings.FastMode.Key && scope == settings.Session && reset {
			if err := clearSessionTier(c.authority, ref.SessionID); err != nil {
				return nil, err
			}
		}
		if key == settings.Skin.Key {
			if err := broadcastSkin(ctx, c); err != nil {
				return nil, err
			}
		}
	}
	return forwardOffThread(ctx, func() (any, error) {
		sctx := settings.WithSession(profileRuntimeScope(ctx, c.authority.ProfileID, true), ref.SessionID)
		return tuigateway.Plain(map[string]any{"row": settings.Describe(sctx, e), "apply": e.Info().Apply.String()}), nil
	})
}

// Owner config.set, the session controls for their keys and the owning profile's config.yaml for the rest
func configSet(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	rawKey, _ := params.Get("key")
	_, scoped := params.Get("scope")
	if rawKey == "reasoning" || (rawKey == "yolo" && scoped) {
		return setCommandSetting(ctx, c, ref, params)
	}
	key, isStr := rawKey.(string)
	if sessionControlKeys[key] || !isStr {
		return busyConfig(ctx, c.authority, c.actor, ref, params, true)
	}
	allowed := []string{"session_id", "profile", "key", "value"}
	if key == "fast" {
		allowed = append(allowed, "scope")
	}
	if paramsOutside(params, allowed...) {
		return nil, runtimeRefusal("invalid_params")
	}
	policy, err := authorizeSettingsWrite(ctx, c, ref, params)
	if err != nil {
		return nil, err
	}
	value, _ := params.Get("value")
	keyed := func(result map[string]any, err error) (any, error) {
		if err != nil {
			return nil, err
		}
		out := map[string]any{"key": key}
		for k, v := range result {
			out[k] = v
		}
		return out, nil
	}
	switch {
	case key == "fast":
		return setFast(ctx, c, ref, policy, value, settingWord(params.GetOr("scope", "session")))
	case key == "personality":
		return forwardOffThread(ctx, func() (map[string]any, error) { return setPersonality(ctx, c, value) })
	case key == "approvals.mode":
		return forwardOffThread(ctx, func() (map[string]any, error) { return setApprovalMode(ctx, c, value) })
	case key == "language":
		return keyed(forwardOffThread(ctx, func() (map[string]any, error) { return setDisplayLanguage(ctx, c, value) }))
	}
	result, err := forwardOffThread(ctx, func() (map[string]any, error) { return setProfileSetting(ctx, c, key, value) })
	if err != nil {
		return nil, err
	}
	if key == "skin" {
		if err := broadcastSkin(ctx, c); err != nil {
			return nil, err
		}
	}
	return keyed(result, nil)
}

// Repaints every attached session of the profile with a skin.changed event
func broadcastSkin(ctx context.Context, c *AuthorityConnection) error {
	payload, err := forwardOffThread(ctx, func() (map[string]any, error) {
		sctx := profileRuntimeScope(ctx, c.authority.ProfileID, true)
		cfg, err := config.LoadContext(sctx, home.Dir(sctx), nil)
		if err != nil {
			return nil, err
		}
		payload := tuihost.SkinPayload(sctx, cfg)
		tuihost.NoteSkinBroadcast(sctx)
		return payload, nil
	})
	if err != nil {
		return err
	}
	c.authority.mu.Lock()
	sessions := make(map[string]*LiveSession, len(c.authority.Sessions))
	for id, live := range c.authority.Sessions {
		sessions[id] = live
	}
	c.authority.mu.Unlock()
	for sessionID, live := range sessions {
		if len(live.Subscribers) > 0 {
			live.EventStream.Publish(sessionID, payload, "skin.changed", nil)
		}
	}
	return nil
}
