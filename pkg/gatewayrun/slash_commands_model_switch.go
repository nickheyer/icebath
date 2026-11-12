package gatewayrun

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Reasoning display toggles of the reasoning command, the value the display flag takes
var reasoningDisplayToggles = gateway.ReasoningDisplayToggles

// Error reply refusing a model switch while the gateway runs code older than the checkout, empty when current
func modelSwitchSkewGuard(ctx context.Context) string {
	bootRev, diskRev, skewed := gateway.DetectCodeSkew()
	if !skewed {
		return ""
	}
	return i18n.T(ctx, "gateway.model.error_prefix", "", map[string]any{
		"error": i18n.T(ctx, "gateway.model.err_skew", "", map[string]any{"boot_rev": bootRev, "disk_rev": diskRev})})
}

// Writes a resolved switch through to the profile config at configPath
func persistModelSwitchToConfig(ctx context.Context, result provider.ModelSwitchResult, configPath string) error {
	return provider.PersistModelSelection(ctx, result, configPath)
}

// Custom provider rows of a switch context
func switchCustomProviders(v any) []any {
	switch rows := v.(type) {
	case []any:
		return rows
	case []map[string]any:
		out := make([]any, len(rows))
		for i, row := range rows {
			out[i] = row
		}
		return out
	}
	return nil
}

// Resolves a model switch, the result or the error reply text
func (r *GatewayRunner) performModelSwitch(ctx context.Context, sw *gateway.ModelSwitchContext, rawInput, explicitProvider string, source *gateway.SessionSource) (*provider.ModelSwitchResult, string, error) {
	if skewError := modelSwitchSkewGuard(ctx); skewError != "" {
		return nil, skewError, nil
	}
	userProviders, _ := sw.UserProviders.(map[string]any)
	result, err := provider.SwitchModel(ctx, provider.SwitchModelOptions{
		RawInput: rawInput, CurrentProvider: sw.CurrentProvider, CurrentBaseURL: sw.CurrentBaseURL,
		CurrentAPIKey: sw.CurrentAPIKey, IsGlobal: sw.PersistGlobal, ExplicitProvider: explicitProvider,
		UserProviders: userProviders, CustomProviders: switchCustomProviders(sw.CustomProviders),
	})
	if err != nil {
		return nil, "", err
	}
	if !result.Success {
		return nil, i18n.T(ctx, "gateway.model.error_prefix", "", map[string]any{"error": result.ErrorMessage}), nil
	}
	if err := r.enrichModelSwitchWarningsForGateway(ctx, &result, sw.SessionKey, source, switchCustomProviders(sw.CustomProviders)); err != nil {
		runLog().Debug("preflight-compression switch warning failed: " + pyrepr.ExceptionMessage(err))
	}
	return &result, "", nil
}

// Configured context length and route of the gateway config, all nil unless a context length is set
func configuredPreflightRoute(cfg map[string]any) (*int, agent.PreflightConfigured) {
	modelCfg, ok := cfg["model"].(map[string]any)
	if !ok || modelCfg["context_length"] == nil {
		return nil, agent.PreflightConfigured{}
	}
	length, err := pyint.Int64(modelCfg["context_length"])
	if err != nil {
		return nil, agent.PreflightConfigured{}
	}
	value := int(length)
	optional := func(v any) *string {
		if v == nil {
			return nil
		}
		text := pyrepr.Str(v)
		return &text
	}
	return &value, agent.PreflightConfigured{Model: optional(pyrepr.Or(modelCfg["default"], modelCfg["model"])),
		Provider: optional(modelCfg["provider"]), BaseURL: optional(modelCfg["base_url"])}
}

// Preflight compression warning from the cached agent and the session's stored messages
func (r *GatewayRunner) enrichModelSwitchWarningsForGateway(ctx context.Context, result *provider.ModelSwitchResult, sessionKey string, source *gateway.SessionSource, customProviders []any) error {
	cached := r.cachedAgentFor(sessionKey, false)
	if cached == nil {
		return nil
	}
	configContext, configured := configuredPreflightRoute(gateway.LoadGatewayConfigMap(ctx, ""))
	var messages []map[string]any
	if db := r.sessionDB(ctx); db != nil && r.sessionStore != nil {
		if entry, err := r.sessionStore.GetOrCreateSession(ctx, source, false, true); err == nil && entry != nil {
			if rows, err := db.DB().GetMessagesAsConversation(ctx, entry.SessionID, state.ConversationOptions{}); err == nil {
				messages = rows
			}
		}
	}
	cached.MergePreflightCompressionWarning(ctx, result, messages, customProviders, configContext, configured)
	return nil
}

// In place swap on the cached agent, the error reply when the agent rolled back
func (r *GatewayRunner) switchCachedAgentModel(ctx context.Context, result *provider.ModelSwitchResult, sw *gateway.ModelSwitchContext, picker bool) string {
	cached := r.cachedAgentFor(sw.SessionKey, false)
	if cached == nil {
		return ""
	}
	if err := cached.SwitchModel(ctx, result.NewModel, result.TargetProvider, result.APIKey, result.BaseURL, result.APIMode,
		result.RuntimeCapabilities, false, false, ""); err != nil {
		label := "In-place"
		if picker {
			label = "Picker"
		}
		runLog().Warn(fmt.Sprintf("%s model switch failed for cached agent: %s", label, pyrepr.ExceptionMessage(err)))
		return i18n.T(ctx, "gateway.model.error_prefix", "", map[string]any{"error": i18n.T(ctx, "gateway.model.err_switch_failed", "",
			map[string]any{"model": result.NewModel, "error": pyrepr.ExceptionMessage(err), "current": sw.CurrentModel})})
	}
	return ""
}

// Persists a committed switch to the session store, notes, config and overrides, the global write warning when one failed
func (r *GatewayRunner) recordModelSwitch(ctx context.Context, result *provider.ModelSwitchResult, sw *gateway.ModelSwitchContext, source *gateway.SessionSource, oneTurn, picker bool) string {
	if db := r.sessionDB(ctx); db != nil {
		err := func() error {
			entry, err := r.sessionStore.GetOrCreateSession(ctx, source, false, true)
			if err != nil {
				return err
			}
			if !picker && entry.WasAutoReset {
				entry.WasAutoReset = false
			}
			return db.DB().SetSessionModel(ctx, entry.SessionID, result.NewModel, result.TargetProvider, result.BaseURL, result.APIMode)
		}()
		if err != nil {
			runLog().Debug("Failed to persist model switch to DB: " + pyrepr.ExceptionMessage(err))
		}
	}
	via := result.ProviderLabel
	if via == "" {
		via = result.TargetProvider
	}
	onceNote := ""
	if oneTurn {
		onceNote = "This override applies to the next turn only. "
	}
	r.stageSessionNote(&r.pendingModelNotes, sw.SessionKey, fmt.Sprintf("[Note: model was just switched from %s to %s via %s. %sAdjust your self-identification accordingly.]",
		provider.FormatModelForDisplay(sw.CurrentModel), provider.FormatModelForDisplay(result.NewModel), via, onceNote))
	requestOverrides := map[string]any{}
	for k, v := range result.RequestOverrides {
		requestOverrides[k] = v
	}
	capabilities := map[string]any{}
	for k, v := range result.RuntimeCapabilities {
		capabilities[k] = v
	}
	switched := map[string]any{
		"model": result.NewModel, "provider": result.TargetProvider, "api_key": result.APIKey,
		"base_url": result.BaseURL, "api_mode": result.APIMode, "request_overrides": requestOverrides, "capabilities": capabilities,
	}
	r.withSessionState(sw.SessionKey, func(s *SessionState) {
		s.conversation.modelOverride = switched
		if oneTurn {
			claimOneTurnRestoreIn(s, sw.RestoreSnapshot)
		} else if !picker {
			s.conversation.oneTurnRestore = nil
		}
	})
	globalError := ""
	if sw.PersistGlobal {
		if err := persistModelSwitchToConfig(ctx, *result, sw.ConfigPath); err != nil {
			runLog().Warn("Failed to persist model switch: " + pyrepr.ExceptionMessage(err))
			globalError = i18n.T(ctx, "gateway.model.err_config_not_updated", "", map[string]any{"error": errorTextOrType(err)})
		}
	}
	switch {
	case sw.PersistGlobal && globalError == "" && r.channelOverrideFor(source) == nil:
		if err := r.sessionStore.SetModelOverride(ctx, sw.SessionKey, nil); err != nil {
			runLog().Warn("Failed to clear persisted session model override: " + pyrepr.ExceptionMessage(err))
			globalError = i18n.T(ctx, "gateway.model.err_stale_override", "", map[string]any{"error": pyrepr.ExceptionMessage(err)})
		} else {
			r.sessionModelOverrides().Delete(sw.SessionKey)
		}
	case !oneTurn:
		override, _ := r.sessionModelOverrides().Get(sw.SessionKey)
		if err := r.sessionStore.SetModelOverride(ctx, sw.SessionKey, override); err != nil {
			runLog().Debug("Failed to persist session model override", "error", err)
		}
	}
	r.evictCachedAgent(ctx, sw.SessionKey, false, false)
	return globalError
}

// Message of an error, its type name when the message is empty
func errorTextOrType(err error) string {
	if text := pyrepr.ExceptionMessage(err); text != "" {
		return text
	}
	return pyrepr.TypeName(err)
}

// Confirmation of a committed switch with context window, output cap, capabilities and scope
func (r *GatewayRunner) modelSwitchConfirmation(ctx context.Context, result *provider.ModelSwitchResult, sw *gateway.ModelSwitchContext, oneTurn, picker bool, globalError string) string {
	via := result.ProviderLabel
	if via == "" {
		via = result.TargetProvider
	}
	lines := []string{
		i18n.T(ctx, "gateway.model.switched", "", map[string]any{"model": provider.FormatModelForDisplay(result.NewModel)}),
		i18n.T(ctx, "gateway.model.provider_label", "", map[string]any{"provider": via}),
	}
	mi := result.ModelInfo
	modelCfg, _ := gateway.LoadGatewayConfigMap(ctx, "")["model"].(map[string]any)
	var configCtx *int
	if modelCfg != nil && modelCfg["context_length"] != nil {
		if length, err := pyint.Int64(modelCfg["context_length"]); err == nil {
			value := int(length)
			configCtx = &value
		}
	}
	baseURL := result.BaseURL
	if baseURL == "" {
		baseURL = sw.CurrentBaseURL
	}
	apiKey := result.APIKey
	if apiKey == "" {
		apiKey = sw.CurrentAPIKey
	}
	var providers []map[string]any
	for _, row := range switchCustomProviders(sw.CustomProviders) {
		if m, ok := row.(map[string]any); ok {
			providers = append(providers, m)
		}
	}
	ctxLen, ok := provider.ResolveDisplayContextLength(ctx, provider.DisplayContextRequest{
		Model: result.NewModel, Provider: result.TargetProvider, BaseURL: baseURL, APIKey: apiKey, ModelInfo: mi,
		CustomProviders: providers, ConfigContextLength: configCtx,
		ConfiguredModel:    pyrepr.Str(pyrepr.Or(pyrepr.Or(modelCfg["default"], modelCfg["model"]), "")),
		ConfiguredProvider: pyrepr.Str(pyrepr.Or(modelCfg["provider"], "")), ConfiguredBaseURL: pyrepr.Str(pyrepr.Or(modelCfg["base_url"], "")),
	})
	if ok && ctxLen != 0 {
		lines = append(lines, i18n.T(ctx, "gateway.model.context_label", "", map[string]any{"tokens": pystr.GroupThousands(int64(ctxLen))}))
	}
	if mi != nil && mi.MaxOutput != 0 {
		lines = append(lines, i18n.T(ctx, "gateway.model.max_output_label", "", map[string]any{"tokens": pystr.GroupThousands(int64(mi.MaxOutput))}))
	}
	if mi != nil {
		lines = append(lines, i18n.T(ctx, "gateway.model.capabilities_label", "", map[string]any{"capabilities": mi.FormatCapabilities()}))
	}
	hostMatches, _ := pyurl.BaseURLHostMatches(result.BaseURL, "openrouter.ai")
	openrouterClaude := hostMatches && strings.Contains(strings.ToLower(result.NewModel), "claude")
	if !picker && (openrouterClaude || result.APIMode == "anthropic_messages") {
		lines = append(lines, i18n.T(ctx, "gateway.model.prompt_caching_enabled", "", nil))
	}
	if result.WarningMessage != "" {
		lines = append(lines, i18n.T(ctx, "gateway.model.warning_prefix", "", map[string]any{"warning": result.WarningMessage}))
	}
	if result.UsageNotice != "" {
		lines = append(lines, "⚠️ "+result.UsageNotice)
	}
	switch {
	case sw.PersistGlobal && globalError != "":
		lines = append(lines, i18n.T(ctx, "gateway.model.warning_prefix", "", map[string]any{"warning": globalError}),
			i18n.T(ctx, "gateway.model.session_only_hint", "", nil))
	case sw.PersistGlobal:
		lines = append(lines, i18n.T(ctx, "gateway.model.saved_global", "", nil))
	case oneTurn:
		lines = append(lines, i18n.T(ctx, "gateway.model.next_turn_only", "", nil))
	default:
		lines = append(lines, i18n.T(ctx, "gateway.model.session_only_hint", "", nil))
	}
	return strings.Join(lines, "\n")
}

// Applies a resolved switch under the runner wide model switch lock and returns the confirmation
func (r *GatewayRunner) commitModelSwitch(ctx context.Context, result *provider.ModelSwitchResult, sw *gateway.ModelSwitchContext, source *gateway.SessionSource, picker bool) (string, error) {
	lock := r.modelSwitchLock()
	lock.Lock()
	defer lock.Unlock()
	return r.commitModelSwitchLocked(ctx, result, sw, source, picker)
}

// Channel override entry of this chat, nil when none applies
func (r *GatewayRunner) channelOverrideFor(source *gateway.SessionSource) *gateway.ChannelOverride {
	if r.config == nil || source == nil {
		return nil
	}
	optional := func(v *string) *string {
		if v == nil || *v == "" {
			return nil
		}
		return v
	}
	return getChannelOverride(r.config, source.Platform, source.ChatID, optional(source.ThreadID), optional(source.ParentChatID))
}

// Runner wide lock over a model command's read, resolve and commit
func (r *GatewayRunner) modelSwitchLock() *sync.Mutex {
	return &r.modelSwitchLockObj
}

// Records the switch metric row under the owning profile home of a multiplexed runner
func (r *GatewayRunner) recordSwitchMetrics(ctx context.Context, result *provider.ModelSwitchResult, sw *gateway.ModelSwitchContext, source *gateway.SessionSource) {
	scoped := ctx
	if r.config != nil && r.config.MultiplexProfiles != nil && *r.config.MultiplexProfiles {
		if profileHome, err := r.resolveProfileHomeForSource(ctx, source); err == nil && profileHome != "" {
			scoped = home.WithOverride(ctx, profileHome)
		}
	}
	var sessionID any
	if cached := r.cachedAgentFor(sw.SessionKey, false); cached != nil {
		sessionID = cached.SessionID()
	}
	observability.RecordModelSwitch(scoped, sw.RouteProvider, result.TargetProvider, "gateway", sw.CurrentModel, sessionID)
}

// Commit body under the switch lock, a local session deferring the pick to its next boundary
func (r *GatewayRunner) commitModelSwitchLocked(ctx context.Context, result *provider.ModelSwitchResult, sw *gateway.ModelSwitchContext, source *gateway.SessionSource, picker bool) (string, error) {
	oneTurn := !picker && sw.OneTurn
	authority, live := gateway.LocalAuthorityLive(r, source)
	level := ""
	globalError := ""
	if live != nil {
		if oneTurn {
			return i18n.T(ctx, "gateway.model.error_prefix", "", map[string]any{"error": i18n.T(ctx, "gateway.model.err_once_local", "", nil)}), nil
		}
		effort := strings.ToLower(pystr.Strip(sw.ReasoningEffort))
		if config.ParseReasoningEffort(effort) != nil {
			level = effort
		}
		if _, err := gateway.DeferLocalModelPick(ctx, authority, live, *result, level); err != nil {
			return "", err
		}
		r.recordSwitchMetrics(ctx, result, sw, source)
		if sw.PersistGlobal {
			if err := persistModelSwitchToConfig(ctx, *result, sw.ConfigPath); err != nil {
				runLog().Warn("Failed to persist model switch", "error", err)
				globalError = i18n.T(ctx, "gateway.model.err_config_not_updated", "", map[string]any{"error": errorTextOrType(err)})
			}
		}
	} else {
		if errorReply := r.switchCachedAgentModel(ctx, result, sw, picker); errorReply != "" {
			return errorReply, nil
		}
		r.recordSwitchMetrics(ctx, result, sw, source)
		globalError = r.recordModelSwitch(ctx, result, sw, source, oneTurn, picker)
	}
	reply := r.modelSwitchConfirmation(ctx, result, sw, oneTurn, picker, globalError)
	if level != "" {
		saved := sw.PersistGlobal && globalError == "" && r.saveGatewayConfigKey(ctx, "agent.reasoning_effort", level)
		key := "gateway.reasoning.set_session"
		if saved {
			key = "gateway.reasoning.set_global"
		}
		reply += "\n" + i18n.T(ctx, key, "", map[string]any{"effort": level})
	} else if sw.ReasoningEffort != "" && !oneTurn {
		reply += "\n" + r.applyReasoningSelection(ctx, sw.SessionKey, platformConfigKey(source.Platform), sw.ReasoningEffort,
			sw.PersistGlobal && globalError == "")
	}
	if live != nil {
		reply += "\n" + i18n.T(ctx, "gateway.model.local_next_turn", "", nil)
	}
	return reply, nil
}

// Walks or creates nested dicts along keys, replacing any non dict value on the path
func nestedDict(root map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		child, ok := root[key].(map[string]any)
		if !ok {
			child = map[string]any{}
			root[key] = child
		}
		root = child
	}
	return root
}

// Saves a dot separated key to the gateway config file, false when the write failed
func (r *GatewayRunner) saveGatewayConfigKey(ctx context.Context, keyPath string, value any) bool {
	configHome := home.Dir(ctx)
	configPath := filepath.Join(configHome, "config.yaml")
	err := func() error {
		userConfig, err := config.ReadUserConfigRaw(ctx, configPath)
		if err != nil {
			return err
		}
		if userConfig == nil {
			userConfig = map[string]any{}
		}
		parts := strings.Split(keyPath, ".")
		nestedDict(userConfig, parts[:len(parts)-1]...)[parts[len(parts)-1]] = value
		return config.AtomicConfigWrite(ctx, configHome, configPath, userConfig, "")
	}()
	if err != nil {
		runLog().Error(fmt.Sprintf("Failed to save config key %s: %s", keyPath, pyrepr.ExceptionMessage(err)))
		return false
	}
	return true
}

// Stores or clears the session reasoning override and drops the cached agent
func (r *GatewayRunner) setReasoningOverride(ctx context.Context, sessionKey string, value map[string]any) {
	r.setSessionReasoningOverride(sessionKey, value)
	r.evictCachedAgent(ctx, sessionKey, false, false)
}

// Applies a typed or picked reasoning argument and returns the reply
func (r *GatewayRunner) applyReasoningSelection(ctx context.Context, sessionKey, platformKey, value string, persistGlobal bool) string {
	value = strings.ToLower(pystr.Strip(value))
	live := gateway.LocalLiveForRoute(r, sessionKey)
	if show, ok := reasoningDisplayToggles[value]; ok {
		r.showReasoning = show
		if live != nil {
			flag := show
			live.ShowReasoning = &flag
		}
		r.saveGatewayConfigKey(ctx, "display.platforms."+platformKey+".show_reasoning", show)
		key := "gateway.reasoning.display_set_off"
		if show {
			key = "gateway.reasoning.display_set_on"
		}
		return i18n.T(ctx, key, "", map[string]any{"platform": platformKey})
	}
	if value == "reset" {
		if persistGlobal {
			return i18n.T(ctx, "gateway.reasoning.reset_global_unsupported", "", nil)
		}
		r.setSessionReasoningOverride(sessionKey, nil)
		r.reasoningConfig = loadReasoningConfig(ctx, "")
		r.evictCachedAgent(ctx, sessionKey, false, false)
		return i18n.T(ctx, "gateway.reasoning.reset_done", "", nil)
	}
	parsed := config.ParseReasoningEffort(value)
	if parsed == nil {
		return i18n.T(ctx, "gateway.reasoning.unknown_arg", "", map[string]any{"arg": value})
	}
	r.reasoningConfig = parsed
	if persistGlobal {
		if r.saveGatewayConfigKey(ctx, "agent.reasoning_effort", value) {
			var override map[string]any
			if live != nil {
				override = parsed
			}
			r.setReasoningOverride(ctx, sessionKey, override)
			return i18n.T(ctx, "gateway.reasoning.set_global", "", map[string]any{"effort": value})
		}
		r.setReasoningOverride(ctx, sessionKey, parsed)
		return i18n.T(ctx, "gateway.reasoning.set_global_save_failed", "", map[string]any{"effort": value})
	}
	r.setReasoningOverride(ctx, sessionKey, parsed)
	return i18n.T(ctx, "gateway.reasoning.set_session", "", map[string]any{"effort": value})
}
