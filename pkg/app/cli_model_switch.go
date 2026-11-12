package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runtime fields a switch snapshots and a rollback or one turn restore puts back
var modelRuntimeFields = []string{"model", "provider", "requested_provider", "_explicit_api_key", "_explicit_base_url",
	"api_key", "base_url", "api_mode", "reasoning_config"}

// Current runtime route fields of the CLI keyed by their hermes attribute names
func (c *CLI) runtimeFields() map[string]any {
	var reasoning any
	if c.reasoningConfig != nil {
		reasoning = c.reasoningConfig
	}
	return map[string]any{"model": c.model, "provider": c.provider, "requested_provider": c.requestedProvider,
		"_explicit_api_key": c.explicitAPIKey, "_explicit_base_url": c.explicitBaseURL, "api_key": c.apiKey,
		"base_url": c.baseURL, "api_mode": c.apiMode, "reasoning_config": reasoning}
}

// Puts one runtime field back by its hermes attribute name
func (c *CLI) setRuntimeField(key string, value any) {
	text, _ := value.(string)
	switch key {
	case "model":
		c.model = text
	case "provider":
		c.provider = text
	case "requested_provider":
		c.requestedProvider = text
	case "_explicit_api_key":
		c.explicitAPIKey = text
	case "_explicit_base_url":
		c.explicitBaseURL = text
	case "api_key":
		c.apiKey = text
	case "base_url":
		c.baseURL = text
	case "api_mode":
		c.apiMode = text
	case "reasoning_config":
		cfg, _ := value.(map[string]any)
		c.reasoningConfig = cfg
	}
}

// Text of a snapshot field, empty when absent or not text
func snapshotText(snapshot map[string]any, key string) string {
	s, _ := snapshot[key].(string)
	return s
}

// Route a resumed session runs on, false when the stored model is absent or already current
func storedSessionRoute(c *CLI, sessionMeta map[string]any, currentModel, currentProvider string) (model, prov, baseURL, apiMode string, providerChanged, ok bool) {
	stored := pystr.Strip(cliStr(sessionMeta["model"], ""))
	if stored == "" {
		return "", "", "", "", false, false
	}
	runtime := state.SessionGatewayRuntime(objectFromPlain(sessionMeta))
	baseURL = cliStr(runtime.GetOr("base_url", ""), "")
	apiMode = cliStr(runtime.GetOr("api_mode", ""), "")
	storedProvider := cliStr(runtime.GetOr("provider", ""), "")
	prov = healBareCustomProvider(c, storedProvider, baseURL, stored)
	if prov != storedProvider {
		baseURL = ""
		if prov == "" {
			apiMode = ""
		}
	}
	providerChanged = prov != "" && prov != currentProvider
	if stored == currentModel && !providerChanged {
		return "", "", "", "", false, false
	}
	if provider.IsForeignProviderEndpoint(prov, baseURL) {
		baseURL, apiMode = "", ""
	}
	if derived, ok := provider.ModelDerivedAPIMode(c.ctx, prov, stored, ""); ok {
		apiMode = derived
		baseURL = provider.NormalizeOpenCodeBaseURL(prov, provider.APIMode(apiMode), baseURL)
	}
	return stored, prov, baseURL, apiMode, providerChanged, true
}

// Durable custom identity of a bare custom provider, empty when nothing heals it
func healBareCustomProvider(c *CLI, prov, baseURL, model string) (healed string) {
	if strings.ToLower(pystr.Strip(prov)) != "custom" {
		return prov
	}
	defer func() {
		if recover() != nil {
			healed = ""
		}
	}()
	return provider.CanonicalCustomIdentity(c.ctx, baseURL, "", model, "")
}

// Restores the model and provider recorded on the session row unless the launch named an explicit model
func (c *CLI) restoreSessionModel(sessionMeta map[string]any, quiet bool) {
	if !cliTruthy(sessionMeta["model"]) || c.explicitModelOverride {
		return
	}
	storedModel, storedProvider, storedBaseURL, storedAPIMode, providerChanged, ok := storedSessionRoute(c, sessionMeta, c.model, c.provider)
	if !ok {
		return
	}
	managed := provider.LlamacppAliases[strings.ToLower(pystr.Strip(storedProvider))]
	c.model = storedModel
	if storedProvider != "" {
		c.provider = storedProvider
		c.requestedProvider = storedProvider
		if storedBaseURL != "" && !managed {
			c.baseURL = storedBaseURL
		}
		if storedAPIMode != "" {
			c.apiMode = storedAPIMode
		}
	}
	switch {
	case managed && !(c.explicitBaseURL != "" && !providerChanged):
		c.explicitAPIKey, c.explicitBaseURL = "", ""
		rt, err := provider.ResolveRuntimeProvider(c.ctx, home.Dir(c.ctx), storedProvider, c.model, "", "")
		if err != nil {
			if storedBaseURL != "" {
				c.baseURL = storedBaseURL
			}
			cliLogger.Debug(fmt.Sprintf("Credential re-resolution for resumed session provider %s failed; keeping ambient credentials", storedProvider))
			break
		}
		if rt.APIKey != "" {
			c.apiKey, c.credentialPool = rt.APIKey, rt.CredentialPool
		}
		if rt.BaseURL != "" {
			c.baseURL = rt.BaseURL
		}
		if storedAPIMode == "" && rt.APIMode != "" {
			c.apiMode = string(rt.APIMode)
		}
	case providerChanged:
		c.explicitAPIKey, c.explicitBaseURL = "", storedBaseURL
		rt, err := provider.ResolveRuntimeProvider(c.ctx, home.Dir(c.ctx), storedProvider, c.model, "", "")
		if err != nil {
			cliLogger.Debug(fmt.Sprintf("Credential re-resolution for resumed session provider %s failed; keeping ambient credentials", storedProvider))
			break
		}
		if rt.APIKey != "" {
			c.apiKey, c.credentialPool = rt.APIKey, rt.CredentialPool
		}
		if storedBaseURL == "" && rt.BaseURL != "" {
			c.baseURL = rt.BaseURL
		}
		if storedAPIMode == "" && rt.APIMode != "" {
			c.apiMode = string(rt.APIMode)
		}
	}
	c.resolveCLIReasoning()
	if c.agent != nil {
		if _, err := c.agent.SwitchModel(c.model, c.provider, false, false, ""); err != nil {
			cliLogger.Debug("In-place agent model swap on resume failed")
		}
	}
	msg := c.t("cli.model.restored_from_session", map[string]any{"model": storedModel})
	if storedProvider != "" {
		msg += " (" + storedProvider + ")"
	}
	if quiet {
		fmt.Fprintln(stderrWriter(), msg)
		return
	}
	c.consolePrint("[dim]" + pyrich.Escape(msg) + "[/dim]")
}

// Captures the CLI and agent model runtime for a one turn restore
func (c *CLI) snapshotModelRuntime() map[string]any {
	snapshot := c.runtimeFields()
	snapshot["reasoning_config"] = pycopy.DeepCopy(snapshot["reasoning_config"])
	snapshot["agent_primary_runtime"] = nil
	return snapshot
}

// Restores the runtime captured before a one turn override, keeping the override when that route fails
func (c *CLI) restoreModelRuntimeSnapshot(snapshot map[string]any) {
	if len(snapshot) == 0 {
		return
	}
	oneTurn := c.runtimeFields()
	for _, key := range modelRuntimeFields {
		if value, ok := snapshot[key]; ok {
			c.setRuntimeField(key, value)
		}
	}
	a := c.agent
	if a == nil {
		return
	}
	restoreReasoning := func(value any) {
		cfg, _ := value.(map[string]any)
		a.RestoreReasoning(cfg)
	}
	if value, ok := snapshot["reasoning_config"]; ok {
		restoreReasoning(value)
	}
	restored, err := a.SwitchModel(snapshotText(snapshot, "model"), snapshotText(snapshot, "provider"), true, true, "")
	if err != nil {
		cliLogger.Warn(fmt.Sprintf("CLI one-turn model restore failed: %v", err))
	}
	if err != nil || !restored {
		for key, value := range oneTurn {
			c.setRuntimeField(key, value)
		}
		notice := c.t("cli.model.switch_cancelled", nil)
		if err != nil {
			notice = c.t("cli.model.switch_failed", map[string]any{"model": snapshotText(snapshot, "model"), "error": err, "old_model": a.Model()})
		}
		cprint("  " + notice)
		return
	}
	if value, ok := snapshot["reasoning_config"]; ok {
		restoreReasoning(value)
	}
}

// Value and label rows of the picker effort step, the canonical ladder then off then keep current
func pickerReasoningRows() [][2]string {
	ctx := context.Background()
	rows := make([][2]string, 0, len(config.ValidReasoningEfforts)+2)
	for _, level := range config.ValidReasoningEfforts {
		rows = append(rows, [2]string{level, level})
	}
	rows = append(rows, [2]string{"none", i18n.T(ctx, "cli.model.effort_none", "", nil)}, [2]string{"", i18n.T(ctx, "cli.model.effort_keep_current", "", nil)})
	return rows
}

// Reports a picked model the inventory does not mark as lacking a reasoning control
func pickerOffersReasoning(providerData map[string]any, model string) bool {
	caps, ok := providerData["capabilities"].(map[string]any)
	if !ok {
		return true
	}
	entry, ok := caps[model].(map[string]any)
	return !(ok && entry["reasoning"] == false)
}

// Opens the prompt native model picker on its provider stage
func (c *CLI) openModelPicker(providers []map[string]any, currentModel, currentProvider string, userProvs map[string]any, customProvs []any) {
	c.captureModalInputSnapshot()
	selected := 0
	for i, p := range providers {
		if cliTruthy(p["is_current"]) {
			selected = i
			break
		}
	}
	state := map[string]any{"stage": "provider", "providers": providers, "selected": selected, "current_model": currentModel,
		"current_provider": currentProvider, "user_provs": userProvs, "custom_provs": customProvs, "filter": ""}
	c.uiMu.Lock()
	c.modelPickerState = state
	c.uiMu.Unlock()
	_ = c.invalidateWithin(0)
}

// Closes the model picker and restores the draft it hid
func (c *CLI) closeModelPicker() {
	c.uiMu.Lock()
	c.modelPickerState = nil
	c.uiMu.Unlock()
	c.restoreModalInputSnapshot()
	_ = c.invalidateWithin(0)
}

// Original index and label pairs of entries matching a case insensitive subsequence query, all of them for an empty query
func (c *CLI) filterModelPickerEntries(modelList []string, query string) []pickerEntry {
	pairs := make([]pickerEntry, 0, len(modelList))
	q := pystr.Lower(pystr.Strip(query))
	for i, entry := range modelList {
		if q == "" || isSubsequence(q, pystr.Lower(entry)) {
			pairs = append(pairs, pickerEntry{Index: i, Model: entry})
		}
	}
	return pairs
}

// pickerEntry is a model picker row with the index it has in the unfiltered list
type pickerEntry struct {
	Index int
	Model string
}

// Scroll offset and visible row count of the picker that keep the selection on screen
func computeModelPickerViewport(selected, scrollOffset, nItems, termRows, panelChrome int) (int, int) {
	const reservedBelow, minVisible = 6, 3
	maxVisible := max(minVisible, termRows-reservedBelow-panelChrome)
	if nItems <= maxVisible {
		return 0, nItems
	}
	visible := maxVisible
	if selected < scrollOffset {
		scrollOffset = selected
	} else if selected >= scrollOffset+visible {
		scrollOffset = selected - visible + 1
	}
	return max(0, min(scrollOffset, nItems-visible)), visible
}

// Switch result of a raw model pick resolved against this CLI's live route
func (c *CLI) switchModelFrom(rawInput string, isGlobal bool, explicitProvider string, userProviders map[string]any, customProviders []any) (*provider.ModelSwitchResult, error) {
	result, err := switchModelResolve(c.ctx, provider.SwitchModelOptions{RawInput: rawInput, CurrentProvider: c.provider,
		CurrentBaseURL: c.baseURL, CurrentAPIKey: c.apiKey, IsGlobal: isGlobal, ExplicitProvider: explicitProvider,
		UserProviders: userProviders, CustomProviders: customProviders})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// Switch resolver and worker launcher of the model switch paths, replaced by tests the way the hermes cases patch them
var (
	switchModelResolve = provider.SwitchModel
	startSwitchWorker  = func(target func()) { go target() }
)

// Runs a confirm and apply sequence on a worker while the TUI is live so its modal can render
func (c *CLI) runConfirmAndApply(target func()) {
	if c.app != nil {
		startSwitchWorker(target)
		return
	}
	target()
}

// Asks whether to switch anyway past a model guard, true only on an explicit yes
func (c *CLI) askSwitchAnyway(title, detail string) bool {
	choices := []slashConfirmChoice{
		{Value: "once", Label: c.t("cli.model.choice_switch_anyway", nil), Desc: c.t("cli.model.desc_switch_anyway", nil)},
		{Value: "cancel", Label: c.t("cli.model.choice_cancel", nil), Desc: c.t("cli.model.desc_keep_current_model", nil)}}
	raw, _ := c.promptTextInputModal("!!! "+title+" !!!", detail, choices, 120)
	return c.normalizeSlashConfirmChoice(raw, choices) == "once"
}

// Asks before applying a costly switch, true when the switch may go ahead
func (c *CLI) confirmExpensiveModelSwitch(result *provider.ModelSwitchResult) bool {
	c.confirmedSwitchWarning = ""
	if !result.Success {
		return true
	}
	selection := c.oneTurnSelectionContext
	if selection == nil {
		selection = selectionContextForAgent(agentGateway(c.agent))
	}
	baseURL, apiKey := result.BaseURL, result.APIKey
	if baseURL == "" {
		baseURL = c.baseURL
	}
	if apiKey == "" {
		apiKey = c.apiKey
	}
	warning := combinedSelectionWarning(c, provider.SelectionRequest{Model: result.NewModel, Provider: result.TargetProvider,
		BaseURL: baseURL, APIKey: apiKey, ModelInfo: result.ModelInfo, Context: selection})
	if warning == nil {
		return true
	}
	if !c.askSwitchAnyway(warning.Title, warning.Message) {
		return false
	}
	c.confirmedSwitchWarning = warning.Message
	return true
}

// Combined guard warning of a selection, nil when the guard itself fails
func combinedSelectionWarning(c *CLI, req provider.SelectionRequest) (warning *provider.SelectionWarning) {
	defer func() {
		if recover() != nil {
			warning = nil
		}
	}()
	return provider.CombinedSelectionWarning(c.ctx, req)
}

// Folds the preflight compression warning into a switch result
func (c *CLI) mergePreflightWarning(result *provider.ModelSwitchResult, customProviders []any) {
	if c.agent == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			cliLogger.Debug(fmt.Sprintf("preflight-compression switch warning failed: %v", r))
		}
	}()
	var providers []map[string]any
	for _, p := range customProviders {
		if m, ok := p.(map[string]any); ok {
			providers = append(providers, m)
		}
	}
	mergeGatewayPreflightWarning(c.ctx, agentGateway(c.agent), result, append([]map[string]any(nil), c.conversationHistory...), providers, c.agent.ConfigContextLength())
}

// Stages a switch onto the CLI fields then swaps the live agent, rolling the CLI back when the swap fails
func (c *CLI) stageAndSwapModel(result *provider.ModelSwitchResult, oldModel string) bool {
	if c.agent == nil && c.gatewaySessionOpen && !c.initAgent("", nil, nil) {
		c.confirmedSwitchWarning = ""
		return false
	}
	snapshot := c.runtimeFields()
	c.model, c.provider, c.requestedProvider = result.NewModel, result.TargetProvider, result.TargetProvider
	c.explicitAPIKey, c.explicitBaseURL = result.APIKey, result.BaseURL
	if result.APIKey != "" {
		c.apiKey = result.APIKey
	}
	if result.BaseURL != "" {
		c.baseURL = result.BaseURL
	}
	if result.APIMode != "" {
		c.apiMode = result.APIMode
	}
	c.resolveCLIReasoning()
	if c.agent == nil {
		return true
	}
	defer func() { c.confirmedSwitchWarning = "" }()
	switched, err := c.agent.SwitchModel(result.NewModel, result.TargetProvider, c.modelSwitchOneTurn, false, result.ResolvedViaAlias)
	if err == nil && switched {
		return true
	}
	for key, value := range snapshot {
		c.setRuntimeField(key, value)
	}
	if err == nil {
		cprint("  " + c.t("cli.model.switch_cancelled", nil))
		return false
	}
	if err.Error() == "launch_credentials_unavailable" {
		cprint("  " + refusalText(c.ctx, err))
		return false
	}
	cprint("  " + c.t("cli.model.switch_failed", map[string]any{"model": result.NewModel, "error": err, "old_model": oldModel}))
	return false
}

// Thousands grouped token count
func groupedTokens(n int) string { return pystr.GroupThousands(int64(n)) }

// Records the next turn switch note and prints the model switched block
func (c *CLI) printSwitchSummary(result *provider.ModelSwitchResult, oldModel string, oneTurn, strictContext bool) error {
	displayOld, displayNew := provider.FormatModelForDisplay(oldModel), provider.FormatModelForDisplay(result.NewModel)
	label := result.ProviderLabel
	if label == "" {
		label = result.TargetProvider
	}
	once := ""
	if oneTurn {
		once = "This override applies to the next turn only. "
	}
	c.pendingModelSwitchNote = fmt.Sprintf("[Note: model was just switched from %s to %s via %s. %sAdjust your self-identification accordingly.]",
		displayOld, displayNew, label, once)
	cprint("  " + c.t("cli.model.switched", map[string]any{"model": displayNew}))
	cprint("    " + c.t("gateway.model.provider_label", map[string]any{"provider": label}))
	if result.TargetProvider == "moa" {
		presets, _ := provider.NormalizeMoAConfig(c.config["moa"])["presets"].(map[string]any)
		preset, _ := presets[result.NewModel].(map[string]any)
		if agg, ok := preset["aggregator"].(map[string]any); ok && len(agg) > 0 {
			cprint("    " + c.t("cli.model.acting_model", map[string]any{"model": fmt.Sprintf("%v:%v", pyStrNone(agg["provider"]), pyStrNone(agg["model"]))}))
		}
	}
	var configContext *int
	var customProviders []map[string]any
	if c.agent != nil {
		configContext = c.agent.ConfigContextLength()
	}
	baseURL, apiKey := result.BaseURL, result.APIKey
	if baseURL == "" {
		baseURL = c.baseURL
	}
	if apiKey == "" {
		apiKey = c.apiKey
	}
	ctxLen, _, ctxErr := provider.ResolveDisplayContextLengthAsync(c.ctx, provider.DisplayContextRequest{Model: result.NewModel,
		Provider: result.TargetProvider, BaseURL: baseURL, APIKey: apiKey, ModelInfo: result.ModelInfo,
		ConfigContextLength: configContext, CustomProviders: customProviders})
	if ctxErr != nil {
		if strictContext {
			return ctxErr
		}
		ctxLen = 0
	}
	if ctxLen != 0 {
		var configured any
		if configContext != nil {
			configured = *configContext
		}
		cprint("    " + c.t("gateway.model.context_label", map[string]any{"tokens": groupedTokens(ctxLen)}) + agent.ContextPinSuffix(ctxLen, configured))
	}
	if mi := result.ModelInfo; mi != nil {
		if mi.MaxOutput != 0 {
			cprint("    " + c.t("gateway.model.max_output_label", map[string]any{"tokens": groupedTokens(mi.MaxOutput)}))
		}
		cprint("    " + c.t("gateway.model.capabilities_label", map[string]any{"capabilities": mi.FormatCapabilities()}))
	}
	onOpenRouter, _ := pyurl.BaseURLHostMatches(result.BaseURL, "openrouter.ai")
	if (onOpenRouter && strings.Contains(pystr.Lower(result.NewModel), "claude")) || result.APIMode == "anthropic_messages" {
		cprint("    " + c.t("gateway.model.prompt_caching_enabled", nil))
	}
	if result.WarningMessage != "" {
		cprint("    ⚠ " + result.WarningMessage)
	}
	if result.UsageNotice != "" {
		cprint("    ⚠ " + result.UsageNotice)
	}
	return nil
}

// Python str of a value with None for nil
func pyStrNone(v any) string {
	if v == nil {
		return "None"
	}
	return fmt.Sprint(v)
}

// Applies a reasoning level picked with a model after the swap, session scoped unless the pick persists
func (c *CLI) applyReasoningAfterSwitch(effort string, persistGlobal, oneTurn bool) {
	parsed := parseReasoningConfig(effort)
	if parsed == nil {
		return
	}
	c.reasoningConfig = parsed
	if oneTurn {
		if c.pendingOneTurnModelRestore != nil {
			c.pendingOneTurnModelRestore["one_turn_reasoning"] = effort
		}
		if c.agent != nil {
			c.agent.StageTurnReasoning(parsed)
		}
		cprint("    " + c.t("cli.model.reasoning_effort", map[string]any{"effort": effort}))
		return
	}
	c.reasoningUserSet = true
	if c.agent != nil {
		c.agent.PinReasoning(parsed)
	}
	saved := persistGlobal && saveConfigValue(c.ctx, "agent.reasoning_effort", effort)
	if saved {
		cfg := CLIConfig(c.ctx)
		agentCfg, ok := cfg["agent"].(map[string]any)
		if !ok {
			agentCfg = map[string]any{}
			cfg["agent"] = agentCfg
		}
		agentCfg["reasoning_effort"] = effort
	}
	key := "cli.model.reasoning_effort"
	if saved {
		key = "cli.model.reasoning_effort_saved"
	}
	cprint("    " + c.t(key, map[string]any{"effort": effort}))
}

// Session row writes a session scoped model switch records
type modelSwitchWriter interface {
	UpdateSessionModel(ctx context.Context, sessionID, model string) error
	PatchSessionModelConfig(ctx context.Context, sessionID string, patch map[string]any) error
}

var _ modelSwitchWriter = (*gatewaycli.GatewaySessionStore)(nil)

// Persists a session scoped switch to the session row with the route in both reader shapes
func (c *CLI) persistModelSwitchToSession(result *provider.ModelSwitchResult) {
	if c.sessionDB == nil {
		return
	}
	persistModelSwitch(c, c.sessionDB, result)
}

// Writes the model column and both route shapes through the store, a failing store logged and never raised
func persistModelSwitch(c *CLI, db modelSwitchWriter, result *provider.ModelSwitchResult) {
	if c.sessionID == "" {
		return
	}
	patch := modelSwitchSessionPatch(c, result)
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = asError(r)
			}
		}()
		if err := db.UpdateSessionModel(c.ctx, c.sessionID, result.NewModel); err != nil {
			return err
		}
		return db.PatchSessionModelConfig(c.ctx, c.sessionID, patch)
	}()
	if err != nil {
		cliLogger.Debug("Failed to persist model switch to session DB", "error", err)
	}
}

// Model config patch of a session scoped switch, the route nested and top level with stale keys set to None
func modelSwitchSessionPatch(c *CLI, result *provider.ModelSwitchResult) map[string]any {
	route := map[string]any{"provider": noneIfEmpty(healBareCustomProvider(c, result.TargetProvider, result.BaseURL, result.NewModel)),
		"base_url": noneIfEmpty(result.BaseURL), "api_mode": noneIfEmpty(result.APIMode)}
	patch := map[string]any{"gateway_runtime": route}
	for k, v := range route {
		patch[k] = v
	}
	return patch
}

// Nil for an empty string, the or None of a route field
func noneIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Stages and swaps a switch, prints its summary and persists it to the session row or config
func (c *CLI) commitModelSwitch(result *provider.ModelSwitchResult, persistGlobal, oneTurn, picker bool, reasoningEffort string) error {
	oldModel, oldProvider := c.model, c.provider
	var snapshot map[string]any
	if oneTurn {
		source := c.pendingOneTurnModelRestore
		if len(source) == 0 {
			source = c.snapshotModelRuntime()
		}
		snapshot = map[string]any{}
		for k, v := range source {
			if k != "one_turn_reasoning" && k != "one_turn_alias" {
				snapshot[k] = v
			}
		}
	}
	c.modelSwitchOneTurn = oneTurn
	staged := c.stageAndSwapModel(result, oldModel)
	c.modelSwitchOneTurn = false
	if !staged {
		return nil
	}
	var sessionID any = c.sessionID
	if c.agent != nil && c.agent.SessionID() != "" {
		sessionID = c.agent.SessionID()
	} else if c.sessionID == "" {
		sessionID = nil
	}
	if !c.slashWorker {
		observability.RecordModelSwitch(c.ctx, oldProvider, result.TargetProvider, "cli", oldModel, sessionID)
	}
	if !picker {
		c.pendingOneTurnModelRestore = snapshot
		if snapshot != nil && result.ResolvedViaAlias != "" {
			snapshot["one_turn_alias"] = result.ResolvedViaAlias
		}
	}
	if err := c.printSwitchSummary(result, oldModel, oneTurn, !picker); err != nil {
		return err
	}
	if reasoningEffort != "" {
		c.applyReasoningAfterSwitch(reasoningEffort, persistGlobal && !oneTurn, oneTurn)
	}
	switch {
	case persistGlobal:
		if err := provider.PersistModelSelection(c.ctx, *result, ""); err != nil {
			return err
		}
		key := "cli.model.saved"
		if picker {
			key = "cli.model.saved_global"
		}
		cprint("    " + c.t(key, nil))
	case oneTurn:
		cprint("    " + c.t("cli.model.next_turn_only", nil))
	default:
		cprint("    " + c.t("cli.model.session_only", nil))
	}
	if !oneTurn {
		c.persistModelSwitchToSession(result)
	}
	return nil
}

// Picker path commit of a switch result
func (c *CLI) applyModelSwitchResult(result *provider.ModelSwitchResult, persistGlobal bool, customProviders []any, reasoningEffort string) error {
	if !result.Success {
		cprint("  ✗ " + result.ErrorMessage)
		return nil
	}
	c.mergePreflightWarning(result, customProviders)
	return c.commitModelSwitch(result, persistGlobal, false, true, reasoningEffort)
}

// Confirms a costly picked switch and applies it, printing any failure
func (c *CLI) confirmAndApplyModelSwitchResult(result *provider.ModelSwitchResult, persistGlobal bool, customProviders []any, reasoningEffort string) {
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		if result.Success && !c.confirmExpensiveModelSwitch(result) {
			cprint("  " + c.t("cli.model.switch_cancelled", nil))
			return nil
		}
		return c.applyModelSwitchResult(result, persistGlobal, customProviders, reasoningEffort)
	}()
	if err != nil {
		cprint("  " + c.t("cli.model.selection_failed", map[string]any{"error": err}))
	}
}

// Closes the picker and runs the confirm and apply sequence of a picked result
func (c *CLI) commitPickerResult(result *provider.ModelSwitchResult, persistGlobal bool, reasoningEffort string) {
	state := c.modelPicker()
	customProvs, _ := state["custom_provs"].([]any)
	c.closeModelPicker()
	c.runConfirmAndApply(func() { c.confirmAndApplyModelSwitchResult(result, persistGlobal, customProvs, reasoningEffort) })
}

// Model ids a picked provider row offers, the cached catalog when its curated list is empty
func (c *CLI) pickerModelList(providerData map[string]any) []string {
	var raw []any
	switch models := providerData["models"].(type) {
	case []any:
		raw = models
	case []string:
		for _, m := range models {
			raw = append(raw, m)
		}
	}
	if len(raw) == 0 {
		func() {
			defer func() { _ = recover() }()
			slug, _ := providerData["slug"].(string)
			for _, id := range provider.CachedProviderModelIDs(c.ctx, slug, provider.CachedModelsOptions{}) {
				raw = append(raw, id)
			}
		}()
	}
	slug, _ := providerData["slug"].(string)
	apiURL, _ := providerData["api_url"].(string)
	var out []string
	for _, id := range provider.OfferedModelIDs(raw, slug, apiURL) {
		out = append(out, fmt.Sprint(id))
	}
	return out
}

// Advances the picker on Enter through its provider, model and effort stages
func (c *CLI) handleModelPickerSelection(persistGlobal bool) error {
	state := c.modelPicker()
	if state == nil {
		return nil
	}
	selected := stateInt(state, "selected", 0)
	stage := cliStr(state["stage"], "")
	switch stage {
	case "provider":
		providers, _ := state["providers"].([]map[string]any)
		if selected >= len(providers) {
			c.closeModelPicker()
			return nil
		}
		providerData := providers[selected]
		modelList := c.pickerModelList(providerData)
		c.uiMu.Lock()
		state["stage"], state["provider_data"], state["model_list"] = "model", providerData, modelList
		state["selected"], state["filter"], state["_filtered_pairs"] = 0, "", nil
		c.uiMu.Unlock()
		_ = c.invalidateWithin(0)
		return nil
	case "model":
		providerData, _ := state["provider_data"].(map[string]any)
		modelList, _ := state["model_list"].([]string)
		pairs, ok := state["_filtered_pairs"].([]pickerEntry)
		if !ok {
			pairs = c.filterModelPickerEntries(modelList, "")
		}
		backIdx := len(pairs)
		if selected == backIdx {
			providers, _ := state["providers"].([]map[string]any)
			back := 0
			for i, p := range providers {
				if p["slug"] == providerData["slug"] {
					back = i
					break
				}
			}
			c.uiMu.Lock()
			state["stage"], state["filter"], state["_filtered_pairs"], state["selected"] = "provider", "", nil, back
			c.uiMu.Unlock()
			_ = c.invalidateWithin(0)
			return nil
		}
		if selected > backIdx || selected < 0 {
			c.closeModelPicker()
			return nil
		}
		userProvs, _ := state["user_provs"].(map[string]any)
		customProvs, _ := state["custom_provs"].([]any)
		slug, _ := providerData["slug"].(string)
		result, err := c.switchModelFrom(pairs[selected].Model, persistGlobal, slug, userProvs, customProvs)
		if err != nil {
			return err
		}
		if result.Success && pickerOffersReasoning(providerData, result.NewModel) {
			c.uiMu.Lock()
			state["stage"], state["switch_result"], state["selected"], state["_scroll_offset"] = "reasoning", result, 0, 0
			c.uiMu.Unlock()
			_ = c.invalidateWithin(0)
			return nil
		}
		c.commitPickerResult(result, persistGlobal, "")
		return nil
	case "reasoning":
		rows := pickerReasoningRows()
		result, _ := state["switch_result"].(*provider.ModelSwitchResult)
		if selected == len(rows) {
			c.uiMu.Lock()
			state["stage"], state["selected"], state["_scroll_offset"], state["switch_result"] = "model", 0, 0, nil
			c.uiMu.Unlock()
			_ = c.invalidateWithin(0)
			return nil
		}
		if selected > len(rows) || result == nil {
			c.closeModelPicker()
			return nil
		}
		c.commitPickerResult(result, persistGlobal, rows[selected][0])
	}
	return nil
}

// Reports a model command typed without images, which runs at once on the UI thread
func (c *CLI) shouldHandleModelCommandInline(text string, hasImages bool) bool {
	if text == "" || hasImages || !looksLikeSlashCommand(text) {
		return false
	}
	words := pystr.SplitWhitespace(text, 1)
	cmd := commands.Resolve(strings.TrimLeft(pystr.Lower(words[0]), "/"))
	return cmd != nil && cmd.Name == "model"
}

// Usage rows of the model command, the invocation form and its catalog description key
var modelUsageRows = [][2]string{
	{"/model <name>", "cli.model.usage_switch"},
	{"/model <name> --global", "cli.model.usage_switch_global"},
	{"/model <name> --once", "cli.model.usage_switch_once"},
	{"/model <name> --session", "cli.model.usage_switch_session"},
	{"/model <name> --provider <slug>", "cli.model.usage_switch_provider_model"},
	{"/model <name> --reasoning <level>", "cli.model.usage_switch_reasoning"},
	{"/model --provider <slug>", "cli.model.usage_switch_provider"},
	{"/model --refresh", "cli.model.usage_refresh"},
}

const modelUsageCol = 36

// Model and provider config.yaml names now, the reset target, auto standing for no provider
func (c *CLI) configuredDefaultRoute() (string, string) {
	cfg, err := config.LoadReadonly(c.ctx, home.Dir(c.ctx), nil)
	if err != nil {
		return "", ""
	}
	modelCfg, ok := cfg["model"].(map[string]any)
	if !ok {
		if s, isStr := cfg["model"].(string); isStr {
			return pystr.Strip(s), ""
		}
		return "", ""
	}
	prov := pystr.Strip(cliStr(modelCfg["provider"], ""))
	model := cliStr(modelCfg["default"], "")
	if model == "" {
		model = cliStr(modelCfg["model"], "")
	}
	if pystr.Lower(prov) == "auto" {
		prov = ""
	}
	return pystr.Strip(model), prov
}

// Context the large context guard would see for a once switch, the live measurement else the persisted anchor
func (c *CLI) oneTurnSelectionContextFor() *provider.SelectionContext {
	if c.agent != nil {
		c.agent.refreshStats(nil)
	}
	if live := selectionContextForAgent(agentGateway(c.agent)); live != nil {
		return live
	}
	if c.sessionDB == nil || c.sessionID == "" {
		return nil
	}
	messages, err := c.sessionDB.GetMessagesAsConversation(c.ctx, c.sessionID, state.ConversationOptions{})
	if err != nil {
		cliLogger.Debug("one-turn selection context unavailable")
		return nil
	}
	reader := c.sessionDB.Reader(c.ctx)
	if reader == nil {
		return nil
	}
	tokens, ok := provider.PersistedAnchorTokens(c.ctx, reader.DB(), c.sessionID, agent.RowsToMessages(messages), true)
	if !ok || tokens == 0 {
		return nil
	}
	return &provider.SelectionContext{ContextTokens: tokens, CurrentModel: c.model}
}

// Opens the picker over the authenticated providers, else prints the model usage block
func (c *CLI) showModelPicker(pc *provider.PickerConfigContext, forceRefresh bool) {
	var providers []map[string]any
	if pc != nil {
		fast := false
		payload, err := provider.BuildModelsPayload(c.ctx, *pc, provider.ModelsPayloadOptions{ProbeCustomProviders: forceRefresh,
			ProbeCurrentCustomProvider: !forceRefresh, Capabilities: true, ForPicker: true, FastCustomProbe: &fast})
		if err == nil {
			rows, _ := payload["providers"].([]any)
			for _, r := range rows {
				if m, ok := r.(map[string]any); ok {
					providers = append(providers, m)
				}
			}
		}
	}
	if len(providers) == 0 {
		cprint("  " + c.t("cli.model.no_authenticated_providers", nil))
		cprint("")
		for _, row := range modelUsageRows {
			cprint("  " + pystr.Ljust(row[0], modelUsageCol) + " " + c.t(row[1], nil))
		}
		return
	}
	currentModel, currentProvider := c.model, "unknown"
	if currentModel == "" {
		currentModel = "unknown"
	}
	if c.provider != "" {
		currentProvider = provider.GetProviderLabel(c.ctx, c.provider)
	}
	var userProvs map[string]any
	var customProvs []any
	if pc != nil {
		userProvs, customProvs = pc.UserProviders, pc.CustomProviders
	}
	c.openModelPicker(providers, currentModel, currentProvider, userProvs, customProvs)
}

// Handles the typed model command, switching for the session, the next turn or globally, or opening the picker
func (c *CLI) handleModelSwitch(cmdOriginal string) {
	parts := pystr.SplitWhitespace(cmdOriginal, 1)
	rest := ""
	if len(parts) > 1 {
		rest = pystr.Strip(parts[1])
	}
	request := provider.ParseModelSwitchArgs(rest)
	if len(request.Errors) > 0 {
		cprint("  ✗ " + request.ErrorMessages()[0])
		return
	}
	target, explicitProvider := request.Target, request.ExplicitProvider
	if request.IsReset() {
		target, explicitProvider = c.configuredDefaultRoute()
		if target == "" {
			cprint("  ✗ " + c.t("cli.model.reset_no_default", nil))
			return
		}
		if target == c.model && (explicitProvider == "" || explicitProvider == c.provider) {
			c.pendingOneTurnModelRestore = nil
			cprint("  " + c.t("cli.model.reset_none", map[string]any{"model": target}))
			return
		}
	}
	oneTurn := request.IsOnce
	persistGlobal := !request.IsReset() && provider.ResolvePersistBehavior(c.ctx, request.IsGlobal, request.IsSession, oneTurn, request.ExplicitProvider)
	if request.ForceRefresh {
		provider.ClearAllProviderModelsCache(c.ctx)
		cprint("  " + c.t("cli.model.cleared_picker_cache", nil))
	}
	var pc *provider.PickerConfigContext
	if loaded, err := provider.LoadPickerContext(c.ctx); err == nil {
		overridden := loaded.WithOverrides(c.provider, c.model, c.baseURL)
		pc = &overridden
	}
	var userProvs map[string]any
	var customProvs []any
	if pc != nil {
		userProvs, customProvs = pc.UserProviders, pc.CustomProviders
	}
	if request.Target == "" && request.ExplicitProvider == "" {
		c.showModelPicker(pc, request.ForceRefresh)
		return
	}
	result, err := c.switchModelFrom(target, persistGlobal, explicitProvider, userProvs, customProvs)
	if err != nil {
		panic(err)
	}
	if !result.Success {
		cprint("  ✗ " + result.ErrorMessage)
		return
	}
	c.mergePreflightWarning(result, customProvs)
	effort := request.ReasoningEffort
	c.runConfirmAndApply(func() { c.confirmAndApplyCLIModelSwitch(result, persistGlobal, oneTurn, customProvs, effort) })
}

// Confirms a costly typed switch and commits it, the once path judged against the persisted anchor
func (c *CLI) confirmAndApplyCLIModelSwitch(result *provider.ModelSwitchResult, persistGlobal, oneTurn bool, customProvs []any, reasoningEffort string) {
	c.oneTurnSelectionContext = nil
	if oneTurn {
		c.oneTurnSelectionContext = c.oneTurnSelectionContextFor()
	}
	defer func() { c.oneTurnSelectionContext = nil }()
	if !c.confirmExpensiveModelSwitch(result) {
		cprint("  " + c.t("cli.model.switch_cancelled", nil))
		return
	}
	if err := c.commitModelSwitch(result, persistGlobal, oneTurn, false, reasoningEffort); err != nil {
		panic(err)
	}
}
