package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Runner holding the per chat voice modes, GatewayRunner._voice_mode with _voice_key_for_source
type voiceModeRunner interface {
	VoiceModeFor(source *SessionSource) string
	StoreVoiceMode(source *SessionSource, mode string)
}

// Agent attributes the settings reads take through getattr with a None default
type (
	agentProvider          interface{ Provider() string }
	agentModel             interface{ Model() string }
	agentBaseURL           interface{ BaseURL() string }
	agentRequestedProvider interface{ RequestedProvider() string }
	agentReasoningConfig   interface{ ReasoningConfig() map[string]any }
	agentServiceTier       interface{ ServiceTier() string }
)

// Settings verbs of a connection keyed by method name
func sessionConfigHandlers(c *AuthorityConnection) map[string]controlHandler {
	return map[string]controlHandler{
		"config.get": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return configGet(ctx, c, ref, params)
		},
		"model.options": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return modelOptions(ctx, c, ref, params)
		},
	}
}

// Read authorization of the settings verbs, the session's local policy when a session is named
func authorizeSettingsRead(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (*LocalSessionPolicy, error) {
	a, actor := c.authority, c.actor
	if actor.ProfileID != a.ProfileID {
		return nil, runtimeRefusal("profile_mismatch")
	}
	if !actor.Capabilities["session:read"] {
		return nil, runtimeRefusal("permission_denied")
	}
	for _, name := range []string{"profile", "session_id"} {
		if value, ok := params.Get(name); ok {
			if _, isStr := value.(string); !isStr {
				return nil, runtimeRefusal("invalid_params")
			}
		}
	}
	if profile, _ := params.Get("profile"); pyrepr.Truthy(profile) {
		if !profiles.ProfileMatchesHome(ctx, profile.(string), a.ProfileID) {
			return nil, runtimeRefusal("profile_mismatch")
		}
	}
	if ref.SessionID == "" {
		return nil, nil
	}
	if err := a.Authorize(ctx, actor, ref, "session:read"); err != nil {
		return nil, err
	}
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return nil, err
	}
	return policyForSource(ctx, a.Runner, live.Source)
}

// Twelve hex digest of the MCP relevant sections, read from the frozen policy when the session has one
func mcpRevision(ctx context.Context, policy *LocalSessionPolicy, cfg map[string]any) (string, error) {
	source := cfg
	if policy != nil {
		frozen, err := policy.config(ctx, nil, true)
		if err != nil {
			return "", err
		}
		source = frozen
	}
	sections := map[string]any{}
	for _, key := range []string{"mcp", "mcp_servers", "tools"} {
		sections[key] = source[key]
	}
	text, err := pyjson.Dumps(sections, pyjson.SortKeys(true))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:12], nil
}

// Presentation only projection of the user config, redacted, with the local display keys winning
func clientConfig(cfg map[string]any) map[string]any {
	picked := map[string]any{}
	for _, key := range []string{"display", "approvals", "paste_collapse_threshold", "paste_collapse_char_threshold"} {
		if value, ok := cfg[key]; ok {
			picked[key] = value
		}
	}
	result, _ := config.RedactConfigValue(picked).(map[string]any)
	if result == nil {
		result = map[string]any{}
	}
	if display, ok := result["display"]; ok {
		result["display"] = config.LocalDisplayConfig(display)
	}
	voice, _ := cfg["voice"].(map[string]any)
	projected := map[string]any{}
	for _, key := range []string{"record_key", "submit_mode"} {
		if value, ok := voice[key]; ok {
			projected[key] = value
		}
	}
	result["voice"] = projected
	return result
}

// Owner bound settings read, the session controls for their keys and the profile config for the rest
func configGet(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	if paramsOutside(params, "session_id", "profile", "key", "cwd") {
		return nil, runtimeRefusal("invalid_params")
	}
	rawKey, _ := params.Get("key")
	key, ok := rawKey.(string)
	if !ok {
		return nil, runtimeRefusal("invalid_params")
	}
	switch key {
	case "busy", "verbose", "yolo", "show_reasoning":
		return busyConfig(ctx, c.authority, c.actor, ref, params, false)
	}
	policy, err := authorizeSettingsRead(ctx, c, ref, params)
	if err != nil {
		return nil, err
	}
	if cwd, has := params.Get("cwd"); has {
		if _, isStr := cwd.(string); key != "project" || !isStr {
			return nil, runtimeRefusal("invalid_params")
		}
	}
	if key == "voice" {
		return voiceSetting(c, ref)
	}
	var agent any
	if ref.SessionID != "" {
		if agent, err = c.authority.agent(ref); err != nil {
			return nil, err
		}
	}
	return forwardOffThread(ctx, func() (any, error) {
		return readSetting(ctx, c, ref, params, key, policy, agent)
	})
}

// One settings key read inside the owning profile's runtime scope
func readSetting(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object, key string, policy *LocalSessionPolicy, agent any) (any, error) {
	profileHome := c.authority.ProfileID
	sctx := profileRuntimeScope(ctx, profileHome, true)
	cfg, err := config.LoadContext(sctx, home.Dir(sctx), nil)
	if err != nil {
		return nil, err
	}
	rawDisplay := cfg["display"]
	if !pyrepr.Truthy(rawDisplay) {
		rawDisplay = map[string]any{}
	}
	displayValue := func(name string, def any) (map[string]any, error) {
		value, err := pyGet(rawDisplay, name, def)
		if err != nil {
			return nil, err
		}
		return map[string]any{"value": value}, nil
	}
	configPath := filepath.Join(profileHome, "config.yaml")
	switch key {
	case "full":
		user, err := config.LoadUserEffective(configPath, config.EnvRefLookupContext(sctx))
		if err != nil {
			return nil, err
		}
		return map[string]any{"config": clientConfig(user)}, nil
	case "mtime":
		version, err := config.ConfigVersionOrNone(sctx, configPath)
		if err != nil {
			return nil, err
		}
		revision, err := mcpRevision(sctx, policy, cfg)
		if err != nil {
			return nil, err
		}
		return map[string]any{"mtime": config.VersionToken(version), "mcp_rev": revision}, nil
	case "reasoning":
		return reasoningSetting(sctx, c, ref, agent, policy, cfg)
	case "fast":
		return fastSetting(sctx, c, ref, policy, agent, cfg)
	case "daybreak":
		section, err := pyGet(cfg, "agent", nil)
		if err != nil {
			return nil, err
		}
		if !pyrepr.Truthy(section) {
			section = map[string]any{}
		}
		value, err := pyGet(section, "daybreak", nil)
		if err != nil {
			return nil, err
		}
		return map[string]any{"value": value == true}, nil
	case "skin":
		return displayValue("skin", "default")
	case "theme":
		return displayValue("tui_theme", "auto")
	case "indicator":
		return displayValue("tui_status_indicator", home.DefaultIndicatorStyle)
	case "details_mode":
		return displayValue("details_mode", "collapsed")
	case "approvals.mode", "approval_mode":
		return map[string]any{"value": tools.GetApprovalMode(tools.EnvForProfileHome(sctx, nil, profileHome))}, nil
	case "project":
		return projectSetting(sctx, params, policy, cfg)
	case "profile":
		return map[string]any{"home": profileHome, "display": profileHome}, nil
	}
	return nil, runtimeRefusal("invalid_params")
}

// The session's voice mode as the messaging handler stored it, off for a history mount
func sessionVoiceMode(c *AuthorityConnection, ref SessionRef) (string, error) {
	live, err := liveSession(c.authority, ref.SessionID)
	if err != nil {
		return "", err
	}
	if live.Source == nil {
		return "off", nil
	}
	runner, ok := c.authority.Runner.(voiceModeRunner)
	if !ok {
		return "", attributeError("_voice_key_for_source")
	}
	return runner.VoiceModeFor(live.Source), nil
}

// Voice state of a local session the mounted owner holds, nil when no owner here holds it
func LocalVoiceEnabled(sessionID any) (*bool, error) {
	runner := ActiveAPIRunner()
	id, ok := sessionID.(string)
	if runner == nil || !ok || id == "" {
		return nil, nil
	}
	for _, a := range AllAuthorities(runner) {
		live, held := a.live(id)
		if !held {
			continue
		}
		enabled := false
		if live.Source != nil {
			modes, ok := runner.(voiceModeRunner)
			if !ok {
				return nil, attributeError("_voice_key_for_source")
			}
			enabled = modes.VoiceModeFor(live.Source) != "off"
		}
		return &enabled, nil
	}
	return nil, nil
}

// The session's own voice mode so Ink mirrors the gateway state
func voiceSetting(c *AuthorityConnection, ref SessionRef) (map[string]any, error) {
	if ref.SessionID == "" {
		return nil, runtimeRefusal("invalid_params")
	}
	mode, err := sessionVoiceMode(c, ref)
	if err != nil {
		return nil, err
	}
	return map[string]any{"value": mode}, nil
}

// Fast tier word of the session's next turn, else the profile's configured tier
func fastSetting(ctx context.Context, c *AuthorityConnection, ref SessionRef, policy *LocalSessionPolicy, agent any, cfg map[string]any) (map[string]any, error) {
	var tier string
	if ref.SessionID != "" {
		current, err := currentTier(ctx, c, ref, policy)
		if err != nil {
			return nil, err
		}
		if current != nil {
			tier = *current
		}
	} else {
		if a, ok := agent.(agentServiceTier); ok {
			tier = a.ServiceTier()
		}
		if tier == "" {
			section, err := pyGet(cfg, "agent", map[string]any{})
			if err != nil {
				return nil, err
			}
			raw, err := pyGet(section, "service_tier", nil)
			if err != nil {
				return nil, err
			}
			word := ""
			if pyrepr.Truthy(raw) {
				word = pyrepr.Str(raw)
			}
			tier = provider.ParseServiceTier(word)
		}
	}
	word := "normal"
	switch tier {
	case "priority":
		word = "fast"
	case "ultrafast":
		word = "ultrafast"
	}
	return map[string]any{"value": word}, nil
}

// Reasoning effort the next turn uses and whether the local display shows it
func reasoningSetting(ctx context.Context, c *AuthorityConnection, ref SessionRef, agent any, policy *LocalSessionPolicy, cfg map[string]any) (map[string]any, error) {
	var value map[string]any
	if a, ok := agent.(agentReasoningConfig); ok {
		value = a.ReasoningConfig()
	}
	if value == nil && policy != nil {
		live, err := liveSession(c.authority, ref.SessionID)
		if err != nil {
			return nil, err
		}
		if value, err = policy.CurrentReasoningConfig(ctx, c.authority.Runner, live.Route); err != nil {
			return nil, err
		}
	} else if value == nil {
		value = config.ResolveReasoningConfig(cfg, "")
	}
	var effort any = "medium"
	if len(value) > 0 && value["enabled"] == false {
		effort = "none"
	} else if e, ok := value["effort"]; ok {
		effort = e
	}
	shown := true
	if v, ok := config.LocalDisplayConfig(cfg["display"])["show_reasoning"]; ok {
		shown = pyrepr.Truthy(v)
	}
	mode := "hide"
	if shown {
		mode = "show"
	}
	return map[string]any{"value": effort, "display": mode}, nil
}

// Working directory a project read names and its git branch
func projectSetting(ctx context.Context, params *pyjson.Object, policy *LocalSessionPolicy, cfg map[string]any) (map[string]any, error) {
	cwd, _ := params.Get("cwd")
	if !pyrepr.Truthy(cwd) {
		if policy != nil {
			cwd = policy.Cwd
		} else {
			terminal, err := pyGet(cfg, "terminal", map[string]any{})
			if err != nil {
				return nil, err
			}
			if cwd, err = pyGet(terminal, "cwd", nil); err != nil {
				return nil, err
			}
		}
	}
	if !pyrepr.Truthy(cwd) {
		resolved, err := tools.ResolveAgentCwd(tools.EnvForProfileHome(ctx, nil, home.Dir(ctx)))
		if err != nil {
			return nil, err
		}
		cwd = resolved
	}
	dir := pyrepr.Str(cwd)
	return map[string]any{"cwd": cwd, "branch": tuigateway.Branch(dir)}, nil
}

// Model picker inventory of the owning profile with the session's provider selected
func modelOptions(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	flags := []string{"explicit_only", "include_unconfigured", "refresh"}
	if paramsOutside(params, append([]string{"profile", "session_id"}, flags...)...) {
		return nil, runtimeRefusal("invalid_params")
	}
	set := map[string]bool{}
	for _, flag := range flags {
		if value, ok := params.Get(flag); ok {
			b, isBool := value.(bool)
			if !isBool {
				return nil, runtimeRefusal("invalid_params")
			}
			set[flag] = b
		}
	}
	policy, err := authorizeSettingsRead(ctx, c, ref, params)
	if err != nil {
		return nil, err
	}
	var agent any
	if ref.SessionID != "" {
		if agent, err = c.authority.agent(ref); err != nil {
			return nil, err
		}
	}
	prov, model, baseURL, requested := "", "", "", ""
	if a, ok := agent.(agentProvider); ok {
		prov = a.Provider()
	}
	if a, ok := agent.(agentModel); ok {
		model = a.Model()
	}
	if a, ok := agent.(agentBaseURL); ok {
		baseURL = a.BaseURL()
	}
	if a, ok := agent.(agentRequestedProvider); ok {
		requested = a.RequestedProvider()
	}
	if policy != nil {
		if prov == "" && pyrepr.Truthy(policy.Provider()) {
			prov = pyrepr.Str(policy.Provider())
		}
		if model == "" && policy.Model != nil {
			model = *policy.Model
		}
		if baseURL == "" && pyrepr.Truthy(policy.BaseURL()) {
			baseURL = pyrepr.Str(policy.BaseURL())
		}
	}
	return forwardOffThread(ctx, func() (any, error) {
		sctx := profileRuntimeScope(ctx, c.authority.ProfileID, true)
		picker, err := provider.LoadPickerContext(sctx)
		if err != nil {
			return nil, err
		}
		selected := prov
		if selected == "custom" {
			if identity := provider.CanonicalCustomIdentity(sctx, baseURL, picker.CurrentProvider, model, requested); identity != "" {
				selected = identity
			}
		}
		return provider.BuildModelOptionsPayload(sctx, picker.WithOverrides(selected, model, baseURL),
			set["explicit_only"], set["include_unconfigured"], set["refresh"])
	})
}
