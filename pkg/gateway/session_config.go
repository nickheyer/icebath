package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
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
)

// Settings verbs of a connection keyed by method name
func sessionConfigHandlers(c *AuthorityConnection) map[string]controlHandler {
	return map[string]controlHandler{
		"config.get": func(ctx context.Context, ref SessionRef, params *jsonutil.Object) (any, error) {
			return configGet(ctx, c, ref, params)
		},
		"model.options": func(ctx context.Context, ref SessionRef, params *jsonutil.Object) (any, error) {
			return modelOptions(ctx, c, ref, params)
		},
	}
}

// Read authorization of the settings verbs, the session's local policy when a session is named
func authorizeSettingsRead(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object) (*LocalSessionPolicy, error) {
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
	if profile, _ := params.Get("profile"); anyutil.IsTrue(profile) {
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
	text, err := jsonutil.Encode(sections, jsonutil.SortKeys())
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:12], nil
}

// Owner bound settings read, the session controls for their keys and the profile config for the rest
func configGet(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object) (any, error) {
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
func readSetting(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object, key string, policy *LocalSessionPolicy, agent any) (any, error) {
	profileHome := c.authority.ProfileID
	sctx := profileRuntimeScope(ctx, profileHome, true)
	cfg, err := config.LoadContext(sctx, home.Dir(sctx), nil)
	if err != nil {
		return nil, err
	}
	client := tuigateway.ClientConfigOf(settings.WithSession(sctx, ref.SessionID))
	displayValue := func(word string) (map[string]any, error) {
		return map[string]any{"value": word}, nil
	}
	configPath := filepath.Join(profileHome, "config.yaml")
	switch key {
	case "full":
		full := tuigateway.ClientConfigOf(settings.WithSession(sctx, ref.SessionID))
		return tuigateway.Plain(tuigateway.ConfigGetResult{Config: &full}), nil
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
		return reasoningSetting(sctx, c, ref, agent, policy)
	case "fast":
		return fastSetting(sctx, c, ref, policy)
	case "daybreak":
		return map[string]any{"value": settings.Daybreak.Get(sctx)}, nil
	case "skin":
		return displayValue(settings.Skin.Get(sctx))
	case "theme":
		return displayValue(string(client.Display.Theme))
	case "indicator":
		return displayValue(string(client.Display.Indicator))
	case "details_mode":
		return displayValue(string(client.Display.DetailsMode))
	case "approvals.mode", "approval_mode":
		return map[string]any{"value": tools.GetApprovalMode(tools.EnvForProfileHome(sctx, nil, profileHome))}, nil
	case "project":
		return projectSetting(sctx, params, policy)
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
		return "", runnerLacks("derive the voice key of a source")
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
				return nil, runnerLacks("derive the voice key of a source")
			}
			enabled = modes.VoiceModeFor(live.Source) != "off"
		}
		return &enabled, nil
	}
	return nil, nil
}

// The session's own voice mode so the TUI mirrors the gateway state
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

// Fast mode word of the session's next turn, else the mode every session starts with
func fastSetting(ctx context.Context, c *AuthorityConnection, ref SessionRef, policy *LocalSessionPolicy) (map[string]any, error) {
	if ref.SessionID == "" {
		return map[string]any{"value": settings.FastMode.Get(ctx)}, nil
	}
	current, err := currentTier(ctx, c, ref, policy)
	if err != nil {
		return nil, err
	}
	tier := ""
	if current != nil {
		tier = *current
	}
	return map[string]any{"value": provider.ServiceTierWord(tier)}, nil
}

// Reasoning effort the next turn uses and whether the local display shows it
func reasoningSetting(ctx context.Context, c *AuthorityConnection, ref SessionRef, agent any, policy *LocalSessionPolicy) (map[string]any, error) {
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
		value = settings.ReasoningConfig(ctx, "")
	}
	var effort any = "medium"
	if len(value) > 0 && value["enabled"] == false {
		effort = "none"
	} else if e, ok := value["effort"]; ok {
		effort = e
	}
	mode := "hide"
	if settings.ShowThinking.Get(settings.WithPlatform(ctx, settings.LocalPlatform)) {
		mode = "show"
	}
	return map[string]any{"value": effort, "display": mode}, nil
}

// Working directory a project read names and its git branch
func projectSetting(ctx context.Context, params *jsonutil.Object, policy *LocalSessionPolicy) (map[string]any, error) {
	cwd, _ := params.Get("cwd")
	if !anyutil.IsTrue(cwd) {
		if policy != nil {
			cwd = policy.Cwd
		} else {
			cwd = settings.WorkingFolder.Get(ctx)
		}
	}
	if !anyutil.IsTrue(cwd) {
		resolved, err := tools.ResolveAgentCwd(tools.EnvForProfileHome(ctx, nil, home.Dir(ctx)))
		if err != nil {
			return nil, err
		}
		cwd = resolved
	}
	dir := anyutil.String(cwd)
	return map[string]any{"cwd": cwd, "branch": tuihost.Branch(dir)}, nil
}

// Model picker inventory of the owning profile with the session's provider selected
func modelOptions(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object) (any, error) {
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
		if prov == "" && anyutil.IsTrue(policy.Provider()) {
			prov = anyutil.String(policy.Provider())
		}
		if model == "" && policy.Model != nil {
			model = *policy.Model
		}
		if baseURL == "" && anyutil.IsTrue(policy.BaseURL()) {
			baseURL = anyutil.String(policy.BaseURL())
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
