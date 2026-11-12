package gatewayrun

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Models previewed per provider in the text listing of a bare model command
const textListingModels = 5

// Text listing body of a bare model command on platforms without a picker
func modelProviderListingLines(ctx context.Context, providers []map[string]any) []string {
	lines := []string{}
	for _, p := range providers {
		tag := ""
		if pyrepr.Truthy(p["is_current"]) {
			tag = i18n.T(ctx, "gateway.model.current_tag", "", nil)
		}
		lines = append(lines, fmt.Sprintf("**%s** `--provider %s`%s:", pyrepr.Str(p["name"]), pyrepr.Str(p["slug"]), tag))
		models := listingModels(p["models"])
		if len(models) > 0 {
			shown := models
			if len(shown) > textListingModels {
				shown = shown[:textListingModels]
			}
			quoted := make([]string, len(shown))
			for i, m := range shown {
				quoted[i] = "`" + pyrepr.Str(m) + "`"
			}
			hidden := listingTotal(p["total_models"]) - len(shown)
			extra := ""
			if hidden > 0 {
				extra = i18n.T(ctx, "gateway.model.more_models_suffix", "", map[string]any{"count": hidden})
			}
			lines = append(lines, "  "+strings.Join(quoted, ", ")+extra)
		} else if pyrepr.Truthy(p["api_url"]) {
			lines = append(lines, "  `"+pyrepr.Str(p["api_url"])+"`")
		}
		lines = append(lines, "")
	}
	return lines
}

// Model rows of one listed provider
func listingModels(v any) []any {
	switch rows := v.(type) {
	case []any:
		return rows
	case []string:
		out := make([]any, len(rows))
		for i, row := range rows {
			out[i] = row
		}
		return out
	}
	return nil
}

// Total model count of one listed provider
func listingTotal(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// Listing arguments of a bare model command, the read path that never stalls on one degraded provider
type modelListingArgs struct {
	currentProvider string
	currentBaseURL  string
	currentModel    string
	userProviders   map[string]any
	customProviders []any
	excluded        []string
}

// Listing arguments from the current route of a switch context
func modelListingArgsFor(sw *gateway.ModelSwitchContext) modelListingArgs {
	userProviders, _ := sw.UserProviders.(map[string]any)
	excluded := make([]string, 0, len(sw.ExcludedProvider))
	for _, slug := range sw.ExcludedProvider {
		excluded = append(excluded, pyrepr.Str(slug))
	}
	return modelListingArgs{currentProvider: sw.CurrentProvider, currentBaseURL: sw.CurrentBaseURL, currentModel: sw.CurrentModel,
		userProviders: userProviders, customProviders: switchCustomProviders(sw.CustomProviders), excluded: excluded}
}

// Sends the interactive model picker, false when nothing was sent and the text listing follows
func (r *GatewayRunner) sendModelPicker(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, picker gateway.ModelPickerSender,
	sessionKey string, args modelListingArgs, onModelSelected func(ctx context.Context, chatID, modelID, providerSlug string) (string, error)) (bool, error) {
	maxModels := 50
	providers, err := provider.ListPickerProviders(ctx, provider.PickerProvidersOptions{
		CurrentProvider: args.currentProvider, CurrentBaseURL: args.currentBaseURL, CurrentModel: args.currentModel,
		UserProviders: args.userProviders, CustomProviders: args.customProviders, ExcludedProviders: args.excluded,
		MaxModels: &maxModels, IncludeMoA: true, NonBlockingCatalogs: true, SkipCustomProviderProbe: true, ProbeCurrentCustomProvider: true,
	})
	if err != nil {
		providers = nil
	}
	if len(providers) == 0 {
		return false, nil
	}
	result, err := picker.SendModelPicker(ctx, source.ChatID, providers, args.currentModel, args.currentProvider, sessionKey,
		onModelSelected, r.threadMetadataForSource(source, gateway.ReplyAnchorForEvent(event)))
	if err != nil {
		return false, err
	}
	return result.Success, nil
}

// Bare model command reply, the interactive picker where the adapter has one and else the text listing
func (r *GatewayRunner) modelListingReply(ctx context.Context, event *gateway.MessageEvent, sw *gateway.ModelSwitchContext, profileHome string) (*string, error) {
	args := modelListingArgsFor(sw)
	adapter, err := r.deliveryAdapterFor(ctx, sw.Source)
	if err != nil {
		return nil, err
	}
	if picker, ok := adapter.(gateway.ModelPickerSender); adapter != nil && ok {
		pickerSwitch := func(switchCtx context.Context, modelID, providerSlug string) (string, error) {
			result, errorText, err := r.performModelSwitch(switchCtx, sw, modelID, providerSlug, event.Source)
			if err != nil {
				return "", err
			}
			if result == nil {
				return errorText, nil
			}
			return r.commitModelSwitch(switchCtx, result, sw, event.Source, true)
		}
		onModelSelected := func(selectCtx context.Context, _ string, modelID, providerSlug string) (string, error) {
			if profileHome == "" {
				return pickerSwitch(selectCtx, modelID, providerSlug)
			}
			return pickerSwitch(gateway.ProfileRuntimeScope(selectCtx, profileHome, true), modelID, providerSlug)
		}
		sent, err := r.sendModelPicker(ctx, event, sw.Source, picker, sw.SessionKey, args, onModelSelected)
		if err != nil {
			return nil, err
		}
		if sent {
			return nil, nil
		}
	}
	model := sw.CurrentModel
	if model == "" {
		model = i18n.T(ctx, "gateway.shared.unknown_value", "", nil)
	}
	lines := []string{i18n.T(ctx, "gateway.model.current_label", "", map[string]any{"model": model,
		"provider": provider.GetProviderLabel(ctx, sw.CurrentProvider)}), ""}
	maxModels := textListingModels
	providers, err := provider.ListAuthenticatedProviders(ctx, provider.AuthenticatedProvidersOptions{
		CurrentProvider: args.currentProvider, CurrentBaseURL: args.currentBaseURL, CurrentModel: args.currentModel,
		UserProviders: args.userProviders, CustomProviders: args.customProviders, ExcludedProviders: args.excluded,
		MaxModels: &maxModels, NonBlockingCatalogs: true, SkipCustomProviderProbe: true, ProbeCurrentCustomProvider: true,
	})
	if err == nil {
		lines = append(lines, modelProviderListingLines(ctx, providers)...)
	}
	for _, key := range []string{"gateway.model.usage_switch_model", "gateway.model.usage_switch_provider", "gateway.model.usage_persist", "gateway.model.usage_reset"} {
		lines = append(lines, i18n.T(ctx, key, "", nil))
	}
	reply := strings.Join(lines, "\n")
	return &reply, nil
}

// Combined selection guard warning of a resolved switch, nil when no guard fires or the check itself fails
func (r *GatewayRunner) modelSelectionWarning(ctx context.Context, sw *gateway.ModelSwitchContext, result *provider.ModelSwitchResult) (warning *provider.SelectionWarning) {
	defer func() {
		if recover() != nil {
			warning = nil
		}
	}()
	baseURL := result.BaseURL
	if baseURL == "" {
		baseURL = sw.CurrentBaseURL
	}
	apiKey := result.APIKey
	if apiKey == "" {
		apiKey = sw.CurrentAPIKey
	}
	return provider.CombinedSelectionWarning(ctx, provider.SelectionRequest{Model: result.NewModel, Provider: result.TargetProvider,
		BaseURL: baseURL, APIKey: apiKey, ModelInfo: result.ModelInfo, Context: agent.SelectionContextForAgent(r.cachedAgentFor(sw.SessionKey, false))})
}

// Typed path confirmation when a selection guard flags the target, whether it fired and the reply to send
func (r *GatewayRunner) modelSelectionGuardReply(ctx context.Context, event *gateway.MessageEvent, sw *gateway.ModelSwitchContext, result *provider.ModelSwitchResult) (bool, *string, error) {
	warning := r.modelSelectionWarning(ctx, sw, result)
	if warning == nil {
		return false, nil, nil
	}
	onCostConfirm := func(confirmCtx context.Context, choice string) (string, error) {
		if choice == "cancel" {
			model := sw.CurrentModel
			if model == "" {
				model = i18n.T(confirmCtx, "gateway.shared.unknown_value", "", nil)
			}
			return i18n.T(confirmCtx, "gateway.model.switch_cancelled", "", map[string]any{"model": model}), nil
		}
		return r.commitModelSwitch(confirmCtx, result, sw, sw.Source, false)
	}
	prefix := r.typedCommandPrefixFor(event.Source.Platform)
	message := i18n.T(ctx, "gateway.model.guard_confirm", "", map[string]any{"title": warning.Title, "message": warning.Message, "prefix": prefix})
	reply, err := r.requestSlashConfirm(ctx, event, "model", warning.Title, message, onCostConfirm)
	if err != nil {
		return true, nil, err
	}
	return true, reply, nil
}

// Switches the model, taking the switch lock before anything else so concurrent commands commit in issue order
func (r *GatewayRunner) handleModelCommand(ctx context.Context, event *gateway.MessageEvent) (*string, error) {
	lock := r.modelSwitchLock()
	lock.Lock()
	defer lock.Unlock()
	return r.handleModelCommandLocked(ctx, event)
}

// Model command body under the switch lock, parse then reset, listing, or resolve guard and commit
func (r *GatewayRunner) handleModelCommandLocked(ctx context.Context, event *gateway.MessageEvent) (*string, error) {
	profileHome := ""
	if r.config != nil && r.config.MultiplexProfiles != nil && *r.config.MultiplexProfiles {
		resolved, err := r.resolveProfileHomeForSource(ctx, event.Source)
		if err != nil {
			return nil, err
		}
		profileHome = resolved
	}
	request := provider.ParseModelSwitchArgs(strings.TrimSpace(event.GetCommandArgs()))
	if len(request.Errors) > 0 {
		reply := "❌ " + request.ErrorMessages()[0]
		return &reply, nil
	}
	if request.ForceRefresh {
		func() {
			defer func() { _ = recover() }()
			provider.ClearAllProviderModelsCache(ctx)
		}()
	}
	source := r.normalizeSourceForSessionKey(ctx, event.Source)
	sessionKey, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return nil, err
	}
	configHome := profileHome
	if configHome == "" {
		configHome = home.Dir(context.Background())
	}
	sw := &gateway.ModelSwitchContext{
		SessionKey: sessionKey, Source: source, ConfigPath: filepath.Join(configHome, "config.yaml"),
		PersistGlobal:   provider.ResolvePersistBehavior(ctx, request.IsGlobal, request.IsSession, request.IsOnce, request.ExplicitProvider),
		OneTurn:         request.IsOnce,
		ReasoningEffort: request.ReasoningEffort,
		CurrentProvider: "openrouter",
	}
	if request.IsOnce {
		sw.RestoreSnapshot = r.snapshotSessionModelOverride(sessionKey)
	}
	sw.ReadConfig(ctx)
	if request.IsReset() {
		return r.handleModelReset(ctx, event, sw)
	}
	override, _ := r.sessionModelOverrides().Get(sessionKey)
	sw.ApplyOverride(override)
	if request.Target == "" && request.ExplicitProvider == "" {
		return r.modelListingReply(ctx, event, sw, profileHome)
	}
	result, errorText, err := r.performModelSwitch(ctx, sw, request.Target, request.ExplicitProvider, source)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return &errorText, nil
	}
	fired, guardReply, err := r.modelSelectionGuardReply(ctx, event, sw, result)
	if err != nil {
		return nil, err
	}
	if fired {
		return guardReply, nil
	}
	reply, err := r.commitModelSwitchLocked(ctx, result, sw, source, false)
	if err != nil {
		return nil, err
	}
	return &reply, nil
}

// Model and provider this chat runs with no session override, channel overrides first and else the profile config
func (r *GatewayRunner) channelDefaultRoute(ctx context.Context, source *gateway.SessionSource, configPath string) (string, string) {
	cfg := gateway.LoadGatewayConfigMap(ctx, configPath)
	if cfg == nil {
		cfg = map[string]any{}
	}
	providerName := ""
	if modelCfg, ok := cfg["model"].(map[string]any); ok {
		providerName = pyrepr.Str(pyrepr.Or(modelCfg["provider"], ""))
	}
	if channel := r.channelOverrideFor(source); channel != nil && pyrepr.Truthy(channel.Model) {
		return pyrepr.Str(channel.Model), pyrepr.Str(pyrepr.Or(channel.Provider, ""))
	}
	if strings.ToLower(providerName) == "auto" {
		providerName = ""
	}
	return gateway.ResolveGatewayModel(ctx, cfg), providerName
}

// Drops this session's model override so the next turn resolves channel overrides then config again, history kept
func (r *GatewayRunner) handleModelReset(ctx context.Context, event *gateway.MessageEvent, sw *gateway.ModelSwitchContext) (*string, error) {
	source, sessionKey := sw.Source, sw.SessionKey
	defaultModel, defaultProvider := r.channelDefaultRoute(ctx, source, sw.ConfigPath)
	if _, live := gateway.LocalAuthorityLive(r, source); live != nil {
		sw.PersistGlobal = false
		result, errorText, err := r.performModelSwitch(ctx, sw, defaultModel, defaultProvider, source)
		if err != nil {
			return nil, err
		}
		if result == nil {
			return &errorText, nil
		}
		reply, err := r.commitModelSwitchLocked(ctx, result, sw, source, false)
		if err != nil {
			return nil, err
		}
		return &reply, nil
	}
	r.rehydrateSessionModelOverride(ctx, sessionKey)
	var previous map[string]any
	had := false
	r.withPeekedSessionState(sessionKey, func(s *SessionState) {
		previous, had = s.conversation.modelOverride, s.conversation.modelOverride != nil
		s.conversation.modelOverride, s.conversation.oneTurnRestore = nil, nil
	})
	if err := r.slashStore().SetModelOverride(ctx, sessionKey, nil); err != nil {
		runLog().Warn("Failed to clear persisted session model override", "error", err)
	}
	shown := defaultModel
	if shown == "" {
		shown = i18n.T(ctx, "gateway.shared.unknown_value", "", nil)
	}
	if !had || previous == nil {
		reply := i18n.T(ctx, "gateway.model.reset_none", "", map[string]any{"model": shown})
		return &reply, nil
	}
	if db := r.sessionDB(ctx); db != nil {
		err := func() error {
			entry, err := r.slashStore().GetOrCreateSession(ctx, source, false, true)
			if err != nil {
				return err
			}
			return db.DB().SetSessionModel(ctx, entry.SessionID, defaultModel, defaultProvider, "", "")
		}()
		if err != nil {
			runLog().Debug("Failed to persist model reset to DB", "error", err)
		}
	}
	r.stageSessionNote(&r.pendingModelNotes, sessionKey, fmt.Sprintf("[Note: the session model override was cleared; model changed from %s back to the default %s. Adjust your self-identification accordingly.]",
		provider.FormatModelForDisplay(pyrepr.Str(pyrepr.Or(previous["model"], ""))), provider.FormatModelForDisplay(defaultModel)))
	r.evictCachedAgent(ctx, sessionKey, false, false)
	reply := i18n.T(ctx, "gateway.model.reset_done", "", map[string]any{"model": provider.FormatModelForDisplay(shown)})
	return &reply, nil
}
