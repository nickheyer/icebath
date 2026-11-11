package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Notice key of the large model usage warning
const largeModelNoticeKey = "model.large_notice"

// Flags of one model switch, every caller stating pinning and counting explicitly
type modelSwitchOptions struct {
	confirmExpensiveModel bool
	pinSessionOverride    bool
	parsedFlags           *provider.ModelSwitchRequest
	persistOverride       *bool
	countSwitch           bool
}

// Normalized model input, explicit provider, once flag, global persistence and reasoning effort of a model request
func switchRequest(ctx context.Context, rawInput string, parsed *provider.ModelSwitchRequest, persistOverride *bool) (string, string, bool, bool, string, error) {
	f := parsed
	if f == nil {
		req := provider.ParseModelSwitchArgs(rawInput)
		f = &req
	}
	for _, code := range f.Errors {
		return "", "", false, false, "", errors.New(provider.ModelSwitchErrorText[code])
	}
	if f.IsGlobal && f.IsOnce {
		return "", "", false, false, "", errors.New(provider.ModelSwitchErrorText[provider.ModelSwitchErrOnceWithGlobal])
	}
	persist := false
	if persistOverride != nil {
		persist = *persistOverride
	} else {
		persist = provider.ResolvePersistBehavior(ctx, f.IsGlobal, f.IsSession, f.IsOnce, f.ExplicitProvider)
	}
	if f.ModelInput() == "" {
		return "", "", false, false, "", errors.New("model value required")
	}
	return f.ModelInput(), f.ExplicitProvider, f.IsOnce, persist, f.ReasoningEffort, nil
}

// Provider, model, base url and key a switch leaves, the live agent's else the configured runtime
func currentModelRuntime(ctx context.Context, a *agent.AIAgent, explicitProvider string) (string, string, string, string, error) {
	if a != nil {
		return a.Provider(), a.Model(), a.BaseURL(), a.APIKey(), nil
	}
	currentModel := resolveModel(ctx)
	if explicitProvider != "" {
		return pystr.Strip(explicitProvider), currentModel, "", "", nil
	}
	rt, err := provider.ResolveRuntimeProvider(ctx, home.Dir(ctx), "", currentModel, "", "")
	if err != nil {
		return "", "", "", "", err
	}
	return rt.Provider, currentModel, rt.BaseURL, rt.APIKey, nil
}

// Provider of the model a switch leaves, nil when only a credential resolve could tell
func switchAwayProvider(ctx context.Context, a *agent.AIAgent, explicitProvider, currentProvider string) any {
	if a != nil || explicitProvider == "" {
		return currentProvider
	}
	if envProvider := pystr.Strip(os.Getenv("ICEBATH_TUI_PROVIDER")); envProvider != "" {
		return envProvider
	}
	if envModelSeed() != "" {
		return nil
	}
	if _, configured := configModelTarget(ctx); configured != "" {
		return configured
	}
	return nil
}

// Folds the context compression preflight warning into the switch result, logging any failure
func mergePreflightWarning(ctx context.Context, result *provider.ModelSwitchResult, a *agent.AIAgent, session *tuigateway.Session, cfg map[string]any, customProvs []any) {
	if failure := catchPanic(func() {
		var cfgCtx *int
		if mc, isDict := cfg["model"].(map[string]any); isDict && mc["context_length"] != nil {
			n, err := pyint.Int64(mc["context_length"])
			if err != nil {
				panic(err)
			}
			value := int(n)
			cfgCtx = &value
		}
		history, _ := session.Get("history").([]any)
		a.MergePreflightCompressionWarning(ctx, result, dictList(history), customProvs, cfgCtx, agent.PreflightConfigured{})
	}); failure != nil {
		logger().Debug(fmt.Sprintf("preflight-compression switch warning failed: %s", panicMessage(failure)))
	}
}

// Selection guard context of a live agent from its last billed prompt, nil without a measurement
func selectionContextForAgent(a *agent.AIAgent) *provider.SelectionContext {
	return agent.SelectionContextForAgent(a)
}

// Deferred confirm response when the selection guards flag the target, nil otherwise
func expensiveModelConfirm(ctx context.Context, result *provider.ModelSwitchResult, currentBaseURL, currentAPIKey string, a *agent.AIAgent) map[string]any {
	var warning *provider.SelectionWarning
	if catchPanic(func() {
		baseURL, apiKey := result.BaseURL, result.APIKey
		if baseURL == "" {
			baseURL = currentBaseURL
		}
		if apiKey == "" {
			apiKey = currentAPIKey
		}
		warning = provider.CombinedSelectionWarning(ctx, provider.SelectionRequest{Model: result.NewModel, Provider: result.TargetProvider,
			BaseURL: baseURL, APIKey: apiKey, ModelInfo: result.ModelInfo, Context: selectionContextForAgent(a)})
	}) != nil {
		warning = nil
	}
	if warning == nil {
		return nil
	}
	msg := warning.Message
	if result.WarningMessage != "" {
		msg = warning.Message + "\n\n" + result.WarningMessage
	}
	return map[string]any{"value": result.NewModel, "warning": msg, "confirm_required": true, "confirm_message": msg}
}

// Swaps the live agent in place then restarts, persists, marks and announces, a failed swap aborting the commit
func commitAgentSwitch(ctx context.Context, sid string, session *tuigateway.Session, a *agent.AIAgent, result *provider.ModelSwitchResult, currentModel string, snapshot map[string]any) error {
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = errors.New(panicMessage(r))
			}
		}()
		return a.SwitchModel(ctx, result.NewModel, result.TargetProvider, result.APIKey, result.BaseURL, result.APIMode,
			result.RuntimeCapabilities, false, false, "")
	}()
	if err != nil {
		logger().Warn(fmt.Sprintf("In-place model switch failed for TUI agent: %s", err))
		staying := a.Model()
		if staying == "" {
			staying = currentModel
		}
		return fmt.Errorf("Model switch to %s failed (%s); staying on %s.", result.NewModel, err, staying)
	}
	restartSlashWorker(ctx, sid, session)
	persistLiveSessionRuntime(ctx, session)
	persistLiveSessionSystemPrompt(ctx, session)
	appendModelSwitchMarker(ctx, session, result.NewModel, result.TargetProvider)
	emitSessionInfo(ctx, sid, session)
	if snapshot != nil {
		session.Set("one_turn_model_restore", snapshot)
	} else {
		session.Pop("one_turn_model_restore")
	}
	return nil
}

// Applies a model request to a session, pinning, persisting, counting and confirming as its flags ask
func applyModelSwitch(ctx context.Context, sid string, session *tuigateway.Session, rawInput string, o modelSwitchOptions) (map[string]any, error) {
	parsed := o.parsedFlags
	if parsed == nil {
		req := provider.ParseModelSwitchArgs(rawInput)
		parsed = &req
	}
	if parsed.IsReset() {
		return resetSessionModelOverride(ctx, sid, session)
	}
	modelInput, explicitProvider, oneTurn, persistGlobal, reasoningEffort, err := switchRequest(ctx, rawInput, parsed, o.persistOverride)
	if err != nil {
		return nil, err
	}
	a := sessionAgent(session)
	if oneTurn && a == nil {
		return nil, errors.New("/model --once requires a live session")
	}
	currentProvider, currentModel, currentBaseURL, currentAPIKey, err := currentModelRuntime(ctx, a, explicitProvider)
	if err != nil {
		return nil, err
	}
	var cfg map[string]any
	var userProvs map[string]any
	var customProvs []any
	suppress(func() {
		loaded, loadErr := loadConfig(ctx)
		if loadErr != nil {
			panic(loadErr)
		}
		cfg = loaded
		userProvs, _ = cfg["providers"].(map[string]any)
		for _, entry := range provider.CompatibleCustomProviders(cfg) {
			customProvs = append(customProvs, entry)
		}
	})
	result, err := provider.SwitchModel(ctx, provider.SwitchModelOptions{RawInput: modelInput, CurrentProvider: currentProvider,
		CurrentBaseURL: currentBaseURL, CurrentAPIKey: currentAPIKey, IsGlobal: persistGlobal, ExplicitProvider: explicitProvider,
		UserProviders: userProvs, CustomProviders: customProvs})
	if err != nil {
		return nil, err
	}
	if !result.Success {
		if result.ErrorMessage != "" {
			return nil, errors.New(result.ErrorMessage)
		}
		return nil, errors.New("model switch failed")
	}
	var restoreSnapshot map[string]any
	if oneTurn && a != nil {
		restoreSnapshot = snapshotAgentModelRuntime(a)
	}
	if a != nil {
		mergePreflightWarning(ctx, &result, a, session, cfg, customProvs)
	}
	if !o.confirmExpensiveModel {
		if confirm := expensiveModelConfirm(ctx, &result, currentBaseURL, currentAPIKey, a); confirm != nil {
			return confirm, nil
		}
	}
	recordsComposerOverride := o.pinSessionOverride && !oneTurn && !persistGlobal && truthy(session.Get("follow_profile_config"))
	previousComposerProfile, hadComposerProfile := session.Lookup("composer_override_profile")
	if recordsComposerOverride {
		profileModel, profileProvider := configModelTarget(ctx)
		session.Set("composer_override_profile", map[string]any{"model": profileModel, "provider": profileProvider})
	}
	if a != nil {
		if err := commitAgentSwitch(ctx, sid, session, a, &result, currentModel, restoreSnapshot); err != nil {
			if recordsComposerOverride {
				if hadComposerProfile {
					session.Set("composer_override_profile", previousComposerProfile)
				} else {
					session.Pop("composer_override_profile")
				}
			}
			return nil, err
		}
	}
	if o.pinSessionOverride && !oneTurn {
		session.Set("model_override", map[string]any{"model": result.NewModel, "provider": result.TargetProvider,
			"base_url": result.BaseURL, "api_key": result.APIKey, "api_mode": result.APIMode})
	}
	if persistGlobal {
		if err := provider.PersistModelSelection(ctx, result, ""); err != nil {
			return nil, err
		}
	}
	if reasoningEffort != "" {
		if err := applySwitchReasoning(ctx, sid, session, a, reasoningEffort, persistGlobal, oneTurn); err != nil {
			return nil, err
		}
	}
	if o.countSwitch {
		var sessionID any
		if a != nil && a.SessionID() != "" {
			sessionID = a.SessionID()
		}
		observability.RecordModelSwitch(ctx, switchAwayProvider(ctx, a, explicitProvider, currentProvider), result.TargetProvider,
			sessionSource(session), currentModel, sessionID)
	}
	if result.UsageNotice != "" && sid != "" {
		tuigateway.Emit(ctx, "notification.show", sid, map[string]any{"text": "⚠ " + result.UsageNotice, "level": "warn",
			"kind": "ttl", "ttl_ms": 8000, "key": largeModelNoticeKey, "id": largeModelNoticeKey})
	}
	scope := "session"
	if oneTurn {
		scope = "once"
	} else if persistGlobal {
		scope = "global"
	}
	return map[string]any{"value": result.NewModel, "warning": result.WarningMessage, "confirm_required": false, "scope": scope}, nil
}

// Drops the session pin and armed restore, adopts the configured model unpinned and persists the row
func resetSessionModelOverride(ctx context.Context, sid string, session *tuigateway.Session) (map[string]any, error) {
	hadOverride := truthy(session.Get("model_override"))
	session.Pop("model_override")
	session.Pop("one_turn_model_restore")
	session.Pop("config_model_seen")
	if _, present := session.Lookup("composer_override_profile"); present {
		session.Set("composer_override_profile", nil)
	}
	model, providerName := configModelTarget(ctx)
	a := sessionAgent(session)
	if model != "" && a != nil && (model != a.Model() || (providerName != "" && providerName != a.Provider())) {
		raw := model
		if providerName != "" {
			raw = model + " --provider " + providerName
		}
		persist := false
		if _, err := applyModelSwitch(ctx, sid, session, raw, modelSwitchOptions{confirmExpensiveModel: true, persistOverride: &persist}); err != nil {
			return nil, err
		}
	}
	persistLiveSessionRuntime(ctx, session)
	emitSessionInfo(ctx, sid, session)
	shown := model
	if shown == "" && a != nil {
		shown = a.Model()
	}
	if shown == "" {
		shown = i18n.T(ctx, "gateway.shared.unknown_value", "", nil)
	}
	key := "gateway.model.reset_none"
	if hadOverride {
		key = "gateway.model.reset_done"
	}
	return map[string]any{"value": shown, "warning": i18n.T(ctx, key, "", map[string]any{"model": shown}), "confirm_required": false,
		"scope": "session"}, nil
}

// Applies the reasoning effort riding a model pick in the pick's own scope
func applySwitchReasoning(ctx context.Context, sid string, session *tuigateway.Session, a *agent.AIAgent, effort string, persistGlobal, oneTurn bool) error {
	parsed := config.ParseReasoningEffort(effort)
	if parsed == nil {
		return nil
	}
	if a != nil {
		a.SetReasoningConfig(parsed)
	}
	if oneTurn || session == nil {
		return nil
	}
	if persistGlobal {
		if err := writeConfigKey(ctx, "agent.reasoning_effort", effort); err != nil {
			return err
		}
		session.Pop("create_reasoning_override")
	} else {
		session.Set("create_reasoning_override", parsed)
	}
	if a != nil {
		persistLiveSessionRuntime(ctx, session)
		emitSessionInfo(ctx, sid, session)
	}
	return nil
}

// Adopts a config.yaml model change at turn start, sessions pinned by a live pick keeping their choice
func syncAgentModelWithConfig(ctx context.Context, sid string, session *tuigateway.Session) {
	a := sessionAgent(session)
	if a == nil {
		return
	}
	model, providerName := configModelTarget(ctx)
	if model == "" {
		return
	}
	target := [2]string{model, providerName}
	if seen, isPair := session.Get("config_model_seen").([2]string); isPair && seen == target {
		return
	}
	superseded := false
	if truthy(session.Get("model_override")) {
		composer, isDict := session.Get("composer_override_profile").(map[string]any)
		if !isDict {
			return
		}
		pinned := [2]string{pystr.Strip(strOr(composer["model"])), pystr.Strip(strOr(composer["provider"]))}
		if pinned == target {
			return
		}
		session.Pop("model_override")
		session.Set("composer_override_profile", nil)
		superseded = true
	}
	session.Set("config_model_seen", target)
	if model == a.Model() && (providerName == "" || providerName == a.Provider()) {
		if superseded {
			persistLiveSessionRuntime(ctx, session)
		}
		return
	}
	raw := model
	if providerName != "" {
		raw = model + " --provider " + providerName
	}
	persist := false
	_, err := func() (out map[string]any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = errors.New(panicMessage(r))
			}
		}()
		return applyModelSwitch(ctx, sid, session, raw, modelSwitchOptions{confirmExpensiveModel: true, persistOverride: &persist})
	}()
	if err == nil {
		return
	}
	logger().Warn(fmt.Sprintf("Configured model %s could not be adopted for session %s: %s", model, sid, err))
	var userConfig map[string]any
	if current := sessionAgent(session); current != nil {
		userConfig = current.NotificationConfig()
	}
	_, _ = config.RenderNotification(ctx, func() error {
		tuigateway.Emit(ctx, "error", sid, map[string]any{"message": fmt.Sprintf("Could not switch to configured model %s: %s", model, err)})
		return nil
	}, "tui", true, userConfig)
}

// Selection guard message for a model queued mid turn, empty when none applies
func pendingSwitchSelectionWarning(ctx context.Context, model, providerName string, a *agent.AIAgent) string {
	if model == "" {
		return ""
	}
	var warning *provider.SelectionWarning
	if catchPanic(func() {
		warning = provider.CombinedSelectionWarning(ctx, provider.SelectionRequest{Model: model, Provider: providerName,
			Context: selectionContextForAgent(a)})
	}) != nil || warning == nil {
		return ""
	}
	return warning.Message
}
