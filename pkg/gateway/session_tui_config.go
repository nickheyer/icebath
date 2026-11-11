package gateway

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var sessionControlKeys = map[string]bool{"busy": true, "verbose": true, "yolo": true, "show_reasoning": true, "service_tier": true, "model": true}

var detailSections = []string{"thinking", "tools", "subagents", "activity"}

var detailModes = map[string]bool{"hidden": true, "collapsed": true, "expanded": true}

var statusbarModes = []string{"off", "top", "bottom"}

var boolWords = map[string]bool{"on": true, "true": true, "yes": true, "off": false, "false": false, "no": false}

var displayToggleKeys = map[string]bool{"display.message_reactions": true, "display.in_app_tips": true, "display.in_app_tours": true}

var voiceChatModes = map[string]bool{"chained": true, "gpt-live": true}

// Show and hide words of the reasoning command that toggle display rather than effort
var ReasoningDisplayToggles = map[string]bool{"show": true, "on": true, "hide": false, "off": false}

// One read modify write of a profile config.yaml at a time
var profileConfigWriteLock sync.Mutex

// hermes_cli/personality.py resolve_personality and persist_personality, assigned in cmd/icebath/wiring.go
var (
	ResolvePersonality func(value any, cfg config.Tree) (string, string, error)
	PersistPersonality func(ctx context.Context, value any) bool
)

// hermes_cli/approval_mode.py run_approval_mode_command as ok and mode, assigned in cmd/icebath/wiring.go
var RunApprovalModeCommand func(ctx context.Context, h string, env *tools.Env, requestedMode string) (bool, string, error)

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
	return pystr.Lower(pystr.Strip(pyrepr.Str(value)))
}

// Status bar placement of a raw value, off for false and top for anything unknown
func statusbarPlacement(raw any) string {
	if raw == false {
		return "off"
	}
	if s, ok := raw.(string); ok && slices.Contains(statusbarModes, pystr.Lower(pystr.Strip(s))) {
		return pystr.Lower(pystr.Strip(s))
	}
	return "top"
}

// One display toggle, its field, accepted words, flipped value and reported word
type displayToggle struct {
	field   string
	aliases map[string]any
	flipped func(display map[string]any) any
	report  func(value any) string
}

// Display toggles keyed by setting, an empty word or toggle flipping the current value
func displayToggles() map[string]displayToggle {
	onOff := func(value any) string {
		if pyrepr.Truthy(value) {
			return "on"
		}
		return "off"
	}
	boolAliases := map[string]any{}
	for word, value := range boolWords {
		boolAliases[word] = value
	}
	statusAliases := map[string]any{"on": "top"}
	for _, mode := range statusbarModes {
		statusAliases[mode] = mode
	}
	mouseAliases := map[string]any{}
	for word, mode := range tuigateway.MouseTrackingAliases {
		mouseAliases[word] = mode
	}
	notFlag := func(field string) func(map[string]any) any {
		return func(d map[string]any) any { return !pyrepr.Truthy(d[field]) }
	}
	return map[string]displayToggle{
		"density":    {"tui_compact", map[string]any{"on": true, "off": false}, notFlag("tui_compact"), onOff},
		"battery":    {"battery", boolAliases, notFlag("battery"), onOff},
		"timestamps": {"timestamps", boolAliases, notFlag("timestamps"), onOff},
		"statusbar": {"tui_statusbar", statusAliases, func(d map[string]any) any {
			current, ok := d["tui_statusbar"]
			if !ok {
				current = "top"
			}
			if statusbarPlacement(current) == "off" {
				return "top"
			}
			return "off"
		}, func(value any) string { return pyrepr.Str(value) }},
		"mouse": {"mouse_tracking", mouseAliases, func(d map[string]any) any {
			if tuigateway.DisplayMouseTracking(d) == "off" {
				return "all"
			}
			return "off"
		}, func(value any) string { return pyrepr.Str(value) }},
	}
}

// Display section of a raw config, created when absent or not a dict
func displaySection(cfg map[string]any) map[string]any {
	display, ok := cfg["display"].(map[string]any)
	if !ok {
		display = map[string]any{}
		cfg["display"] = display
	}
	return display
}

// Detail sections of a display block, created when absent or not a dict
func displaySections(display map[string]any) map[string]any {
	sections, ok := display["sections"].(map[string]any)
	if !ok {
		sections = map[string]any{}
		display["sections"] = sections
	}
	return sections
}

// One display preference on the raw config with main's semantics, the result extras returned
func setDisplay(cfg map[string]any, key string, value any) (map[string]any, error) {
	display, raw := displaySection(cfg), settingWord(value)
	if toggle, ok := displayToggles()[key]; ok {
		var next any
		if raw == "" || raw == "toggle" {
			next = toggle.flipped(display)
		} else if alias, known := toggle.aliases[raw]; known {
			next = alias
		}
		if next == nil {
			return nil, runtimeRefusal("invalid_params")
		}
		display[toggle.field] = next
		return map[string]any{"value": toggle.report(next)}, nil
	}
	words := map[string]struct {
		field   string
		allowed []string
	}{"theme": {"tui_theme", []string{"auto", "light", "dark"}}, "indicator": {"tui_status_indicator", home.IndicatorStyles}}
	if entry, ok := words[key]; ok {
		if !slices.Contains(entry.allowed, raw) {
			return nil, runtimeRefusal("invalid_params")
		}
		display[entry.field] = raw
		return map[string]any{"value": raw}, nil
	}
	if key == "details_mode" {
		if !detailModes[raw] {
			return nil, runtimeRefusal("invalid_params")
		}
		display["details_mode"] = raw
		sections := displaySections(display)
		for _, section := range detailSections {
			sections[section] = raw
		}
		return map[string]any{"value": raw}, nil
	}
	if section, ok := strings.CutPrefix(key, "details_mode."); ok {
		if !slices.Contains(detailSections, section) || (raw != "" && !detailModes[raw]) {
			return nil, runtimeRefusal("invalid_params")
		}
		if raw != "" {
			displaySections(display)[section] = raw
		} else {
			delete(displaySections(display), section)
		}
		return map[string]any{"value": raw}, nil
	}
	if key == "focus" {
		return setFocus(display, raw)
	}
	if key == "skin" {
		text, ok := value.(string)
		if !ok || pystr.Strip(text) == "" {
			return nil, runtimeRefusal("invalid_params")
		}
		display["skin"] = pystr.Strip(text)
		return map[string]any{"value": pystr.Strip(text)}, nil
	}
	return nil, runtimeRefusal("invalid_params")
}

// Focus on stashes the configured tool progress mode and pins it off, off restores it
func setFocus(display map[string]any, raw string) (map[string]any, error) {
	current := pyrepr.Truthy(display["focus_view"])
	progress, ok := display["tool_progress"]
	if !ok {
		progress = "all"
	}
	configured := tuigateway.NormalizeToolProgressMode(progress, "all")
	action, target := tuigateway.ResolveFocusArg(raw, current)
	if action == "usage" {
		return nil, runtimeRefusal("invalid_params")
	}
	onOff := func(on bool) string {
		if on {
			return "on"
		}
		return "off"
	}
	if action == "status" || target == nil {
		return map[string]any{"value": onOff(current), "tool_progress": configured}, nil
	}
	var effective string
	if *target {
		var saved any = configured
		if stashed := display["focus_saved_tool_progress"]; current && pyrepr.Truthy(stashed) {
			saved = stashed
		}
		display["focus_saved_tool_progress"] = tuigateway.NormalizeToolProgressMode(saved, "all")
		effective = tuigateway.FocusToolProgressMode
	} else {
		var saved any = "all"
		if stashed := display["focus_saved_tool_progress"]; pyrepr.Truthy(stashed) {
			saved = stashed
		}
		effective = tuigateway.NormalizeToolProgressMode(saved, "all")
	}
	display["tool_progress"], display["focus_view"] = effective, *target
	return map[string]any{"value": onOff(*target), "tool_progress": effective}, nil
}

// Edits the owning profile's config.yaml exactly as written, saving only when the edit changed it
func writeOwnerProfileConfig(ctx context.Context, c *AuthorityConnection, edit func(cfg map[string]any) (map[string]any, error)) (map[string]any, error) {
	sctx := profileRuntimeScope(ctx, c.authority.ProfileID, true)
	profileConfigWriteLock.Lock()
	defer profileConfigWriteLock.Unlock()
	h := home.Dir(sctx)
	path := home.ConfigPath(h)
	cfg, err := config.ReadUserConfigRaw(sctx, path)
	if err != nil {
		return nil, runtimeRefusal("storage_unavailable")
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	before := pycopy.DeepCopy(cfg)
	result, err := edit(cfg)
	if err != nil {
		return nil, err
	}
	if !pyrepr.Equal(cfg, before) {
		if err := config.AtomicConfigReplace(sctx, h, path, cfg, ""); err != nil {
			return nil, runtimeRefusal("storage_unavailable")
		}
	}
	return result, nil
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
		return nil, attributeError("_resolve_session_service_tier")
	}
	return runner.ResolveSessionServiceTier(ctx, live.Source, live.Route)
}

// AttributeError a getattr on the runner raises for a method it lacks
func attributeError(name string) error {
	return &pyAttributeError{name}
}

// Python AttributeError of a runner without a method the session controls call
type pyAttributeError struct{ name string }

func (e *pyAttributeError) Error() string {
	return "AttributeError: 'GatewayRunner' object has no attribute " + pyrepr.Repr(e.name)
}

// Session scoped fast pick validated against the session's route, never a config write
func setFast(ctx context.Context, c *AuthorityConnection, ref SessionRef, policy *LocalSessionPolicy, value any) (map[string]any, error) {
	if ref.SessionID == "" {
		return nil, runtimeRefusal("invalid_params")
	}
	raw := settingWord(value)
	current, err := currentTier(ctx, c, ref, policy)
	if err != nil {
		return nil, err
	}
	currentWord := ""
	if current != nil {
		currentWord = *current
	}
	if raw == "status" {
		return map[string]any{"key": "fast", "value": provider.ServiceTierWord(currentWord)}, nil
	}
	if raw == "" || raw == "toggle" {
		raw = "fast"
		if current != nil && provider.StaticTiers[*current] {
			raw = "normal"
		}
	}
	tier, err := provider.ParseExactServiceTier(raw)
	if err != nil {
		return nil, runtimeRefusal("invalid_params")
	}
	if provider.StaticTiers[tier] {
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
		return nil, attributeError("_set_session_service_tier_override")
	}
	live, err := liveSession(c.authority, ref.SessionID)
	if err != nil {
		return nil, err
	}
	var picked *string
	if tier != "" {
		picked = &tier
	}
	runner.SetSessionServiceTierOverride(live.Route, picked, false)
	return map[string]any{"key": "fast", "value": provider.ServiceTierWord(tier)}, nil
}

// Validates and persists the profile personality the next turn of each session resolves
func setPersonality(ctx context.Context, c *AuthorityConnection, value any) (map[string]any, error) {
	sctx := profileRuntimeScope(ctx, c.authority.ProfileID, true)
	profileConfigWriteLock.Lock()
	defer profileConfigWriteLock.Unlock()
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
	if !PersistPersonality(sctx, name) {
		return nil, runtimeRefusal("storage_unavailable")
	}
	reported := "none"
	if pyrepr.Truthy(value) {
		reported = pyrepr.Str(value)
	}
	return map[string]any{"key": "personality", "value": reported, "history_reset": false}, nil
}

// One section leaf of the profile config, a voice chat mode or a boolean word
func setProfileLeaf(cfg map[string]any, key string, value any) (map[string]any, error) {
	section, leaf, _ := strings.Cut(key, ".")
	var next any
	if key == "voice.voice_chat_mode" {
		word := settingWord(value)
		if !voiceChatModes[word] {
			return nil, runtimeRefusal("invalid_params")
		}
		next = word
	} else {
		word := settingWord(value)
		on, known := boolWords[word]
		switch word {
		case "1":
			on, known = true, true
		case "0":
			on, known = false, true
		}
		if !known {
			return nil, runtimeRefusal("invalid_params")
		}
		next = on
	}
	block, ok := cfg[section].(map[string]any)
	if !ok {
		block = map[string]any{}
		cfg[section] = block
	}
	block[leaf] = next
	return map[string]any{"value": next}, nil
}

// The approvals command's own write of approvals.mode for this profile
func setApprovalMode(ctx context.Context, c *AuthorityConnection, value any) (map[string]any, error) {
	sctx := profileRuntimeScope(ctx, c.authority.ProfileID, true)
	profileConfigWriteLock.Lock()
	h := home.Dir(sctx)
	ok, mode, err := RunApprovalModeCommand(sctx, h, tools.EnvForProfileHome(sctx, nil, h), settingWord(value))
	profileConfigWriteLock.Unlock()
	if err != nil {
		return nil, err
	}
	if !ok {
		switch settingWord(value) {
		case "manual", "smart", "off":
			return nil, runtimeRefusal("storage_unavailable")
		}
		return nil, runtimeRefusal("invalid_params")
	}
	return map[string]any{"key": "approvals.mode", "value": mode}, nil
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
	if _, toggle := ReasoningDisplayToggles[value]; !toggle && value != "reset" && config.ParseReasoningEffort(value) == nil {
		return nil, runtimeRefusal("invalid_params")
	}
	live, err := liveSession(c.authority, ref.SessionID)
	if err != nil {
		return nil, err
	}
	runner, ok := c.authority.Runner.(reasoningSelectionRunner)
	if !ok {
		return nil, attributeError("_apply_reasoning_selection")
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
	}
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
	if paramsOutside(params, "session_id", "profile", "key", "value") {
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
		return setFast(ctx, c, ref, policy, value)
	case key == "personality":
		return forwardOffThread(ctx, func() (map[string]any, error) { return setPersonality(ctx, c, value) })
	case key == "approvals.mode" || key == "approval_mode":
		return forwardOffThread(ctx, func() (map[string]any, error) { return setApprovalMode(ctx, c, value) })
	case displayToggleKeys[key] || key == "voice.voice_chat_mode":
		return keyed(forwardOffThread(ctx, func() (map[string]any, error) {
			return writeOwnerProfileConfig(ctx, c, func(cfg map[string]any) (map[string]any, error) { return setProfileLeaf(cfg, key, value) })
		}))
	case key == "daybreak":
		return keyed(forwardOffThread(ctx, func() (map[string]any, error) {
			return writeOwnerProfileConfig(ctx, c, func(cfg map[string]any) (map[string]any, error) {
				return setProfileLeaf(cfg, "agent.daybreak", value)
			})
		}))
	}
	result, err := forwardOffThread(ctx, func() (map[string]any, error) {
		return writeOwnerProfileConfig(ctx, c, func(cfg map[string]any) (map[string]any, error) { return setDisplay(cfg, key, value) })
	})
	if err != nil {
		return nil, err
	}
	if progress, ok := result["tool_progress"]; key == "focus" && ref.SessionID != "" && ok {
		live, err := liveSession(c.authority, ref.SessionID)
		if err != nil {
			return nil, err
		}
		live.ToolProgressMode = pyrepr.Str(progress)
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
		payload := tuigateway.SkinPayload(sctx, cfg)
		tuigateway.NoteSkinBroadcast(sctx)
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
