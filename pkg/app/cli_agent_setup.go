package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// turnRoute is the effective model and runtime one turn's agent is built from
type turnRoute struct {
	model            string
	runtime          *cliRuntime
	signature        string
	requestOverrides map[string]any
}

// Answers a clarify request immediately for a -q turn that has no user to ask
func singleQueryClarifyCallback(questions []any) map[string]any {
	return map[string]any{"answers": map[string]any{}, "outcome": "undelivered", "notice": "single-query mode: no user available to answer. Pick the best choices using your own " +
		"judgment, or make the most reasonable assumption you can, and continue."}
}

// Snapshots the CLI's resolved provider routing as the runtime an agent is built from
func currentRuntime(c *CLI) *cliRuntime {
	return &cliRuntime{APIKey: c.apiKey, BaseURL: c.baseURL, Provider: c.provider, RequestedProvider: c.requestedProvider,
		APIMode: c.apiMode, Command: c.acpCommand, Args: append([]string{}, c.acpArgs...), CredentialPool: c.credentialPool, Resolved: c.resolvedRuntime}
}

// Returns the identity of model and routing that tells when the agent must be rebuilt
func routeSignature(model string, rt *cliRuntime) string {
	return fmt.Sprintf("%q|%q|%q|%q|%q|%q|%q", model, rt.Provider, rt.RequestedProvider, rt.BaseURL, rt.APIMode, rt.Command, strings.Join(rt.Args, "\x00"))
}

// Names why a benched credential is cooling down from what the pool recorded
func cooldownCause(c *CLI, entry provider.PooledCredential) string {
	reason := strings.ToLower(entry.LastErrorReason)
	if entry.LastErrorCode == 402 || entry.LastErrorCode == 429 || strings.Contains(reason, "rate") || strings.Contains(reason, "quota") || strings.Contains(reason, "insufficient") {
		return i18n.T(c.ctx, "cli.startup.cooldown_cause_rate_limit", "", nil)
	}
	if entry.LastErrorCode == 0 || strings.Contains(reason, "refresh") {
		return i18n.T(c.ctx, "cli.startup.cooldown_cause_token_refresh", "", nil)
	}
	return i18n.T(c.ctx, "cli.startup.cooldown_cause_http", "", map[string]any{"code": entry.LastErrorCode})
}

// Credential pool surface the startup gate reads, the agent.credential_pool.CredentialPool methods it calls
type startupCredentialPool interface {
	HasCredentials() bool
	HasAvailable(model string) bool
	NextAvailableAt(model string) (time.Time, bool)
	Entries() []provider.PooledCredential
}

// Seams over the pool loader and the runtime resolver, the agent.credential_pool and runtime_provider names the first run tests patch
var (
	startupLoadPool = func(c *CLI, providerName string) startupCredentialPool {
		env := provider.HomePoolEnv(home.Dir(c.ctx))
		env.Ctx = c.ctx
		env.Config = func() map[string]any { return c.config }
		if pool := provider.LoadPool(env, providerName); pool != nil {
			return pool
		}
		return nil
	}
	resolveRuntimeProviderFn = provider.ResolveRuntimeProvider
)

// Explains why a provider's credential pool has nothing selectable, cooling first when a cooldown is live
func credentialPoolNotice(c *CLI, providerName string) (bool, []string) {
	pool := startupLoadPool(c, providerName)
	if pool == nil || !pool.HasCredentials() || pool.HasAvailable("") {
		return false, nil
	}
	nextAt, hasNext := pool.NextAvailableAt("")
	entries := pool.Entries()
	var lines []string
	if hasNext {
		minutes := max(1, int((time.Until(nextAt).Seconds()+59)/60))
		cause := i18n.T(c.ctx, "cli.startup.cooldown_cause_failed_request", "", nil)
		for _, e := range entries {
			if e.LastStatus == provider.StatusExhausted {
				cause = cooldownCause(c, e)
				break
			}
		}
		lines = append(lines, i18n.T(c.ctx, "cli.startup.credential_cooling_down", "", map[string]any{"provider": providerName, "cause": cause, "minutes": minutes}))
	}
	for _, e := range entries {
		if e.LastStatus == provider.StatusDead {
			reason := firstNonEmpty(e.LastErrorMessage, e.LastErrorReason, i18n.T(c.ctx, "cli.startup.sign_in_lost", "", nil))
			lines = append(lines, i18n.T(c.ctx, "cli.startup.sign_in_was_lost", "", map[string]any{"provider": providerName, "reason": reason}))
			break
		}
	}
	return hasNext, lines
}

// Returns the user and total message count suffix of the resumed session line
func resumeCounts(c *CLI, userCount, total int) string {
	key := "cli.resume.counts_other"
	if userCount == 1 {
		key = "cli.resume.counts_one"
	}
	return i18n.T(c.ctx, key, "", map[string]any{"count": userCount, "total": total})
}

// Reports a custom or local endpoint that may need no auth, never an OpenRouter base URL
func keylessCustomBase(baseURL string) bool {
	if baseURL == "" {
		return false
	}
	matches, err := pyurl.BaseURLHostMatches(baseURL, "openrouter.ai")
	return err == nil && !matches
}

// Returns the descendant holding a compression chain head's messages, empty when there is none
func compressionDescendant(ctx context.Context, db *gatewaycli.GatewaySessionStore, sessionID string) string {
	resolved, err := db.ResolveResumeSessionID(ctx, sessionID)
	if err != nil || resolved == "" || resolved == sessionID {
		return ""
	}
	return resolved
}

// Id the gateway answers for a physical row, its session.resume owner, unchanged off the gateway or on a refusal
func gatewayOwnedID(ctx context.Context, db *gatewaycli.GatewaySessionStore, sessionID string) string {
	owner, _ := gatewayOpen(ctx, db, sessionID)
	return owner
}

// Owner id and refusal code of one session.resume, the refusal set only when this viewer may not open it
func gatewayOpen(ctx context.Context, db *gatewaycli.GatewaySessionStore, sessionID string) (string, string) {
	if db == nil {
		return sessionID, ""
	}
	snapshot, err := db.Attach(ctx, sessionID, false)
	if err != nil {
		var refused *gateway.GatewayRPCError
		if errors.As(err, &refused) && gatewaycli.ResumeGone[refused.Error()] {
			return sessionID, refused.Error()
		}
		return sessionID, ""
	}
	if owner, _ := snapshot.Vals["stored_session_id"].(string); owner != "" {
		return owner, ""
	}
	return sessionID, ""
}

// Returns the recap text of a user row, multimodal lists becoming text parts and image markers
func userDisplayText(content any) string {
	if parts, ok := content.([]any); ok {
		var out []string
		for _, p := range parts {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			switch m["type"] {
			case "text":
				out = append(out, cliStr(m["text"], ""))
			case "image_url":
				out = append(out, "[image]")
			}
		}
		return strings.Join(out, " ")
	}
	if content == nil {
		return ""
	}
	return cliStr(content, "")
}

// Summarizes tool calls as a count with up to four distinct names
func toolCallsSummary(c *CLI, toolCalls []any) string {
	var names []string
	for _, tc := range toolCalls {
		name := "unknown"
		if m, ok := pyjson.Dict(tc); ok {
			if fn, ok := pyjson.Dict(m["function"]); ok {
				name = cliStr(fn["name"], "unknown")
			}
		}
		if !containsString(names, name) {
			names = append(names, name)
		}
	}
	shown := names
	if len(shown) > 4 {
		shown = shown[:4]
	}
	namesStr := strings.Join(shown, ", ")
	if len(names) > 4 {
		namesStr += ", ..."
	}
	key := "cli.resume.tool_calls_other"
	if len(toolCalls) == 1 {
		key = "cli.resume.tool_calls_one"
	}
	return i18n.T(c.ctx, key, "", map[string]any{"count": len(toolCalls), "names": namesStr})
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

var resumeEventKeys = map[string]string{
	"model_switch": "cli.resume.event_model_changed", "async_delegation_complete": "cli.resume.event_delegation_completed",
	"process_complete": "cli.resume.event_process_finished", "auto_continue": "cli.resume.event_resumed_interrupted",
}

// resumeEntry is one displayable recap row of a resumed session
type resumeEntry struct {
	Role string
	Text string
}

// Builds the recap entries of stored history, truncated per display.resume_* with system and tool rows skipped
func collectResumeEntries(c *CLI, displayHistory []map[string]any, disp map[string]any, cleanAssistant func(string) string) ([]resumeEntry, int, string) {
	maxUserLen := intOr(mapValueOr(disp, "resume_max_user_chars", 300), 300)
	maxAsstLen := intOr(mapValueOr(disp, "resume_max_assistant_chars", 200), 200)
	maxAsstLines := intOr(mapValueOr(disp, "resume_max_assistant_lines", 3), 3)
	skipToolOnly := displayFlag(disp, "resume_skip_tool_only", true)
	var entries []resumeEntry
	lastAsstIdx := -1
	lastAsstFull := ""
	for _, msg := range displayHistory {
		role := cliStr(msg["role"], "")
		displayKind := cliStr(msg["display_kind"], "")
		content := msg["content"]
		toolCalls, _ := msg["tool_calls"].([]any)
		if displayKind == "hidden" {
			continue
		}
		if key, ok := resumeEventKeys[displayKind]; ok {
			metadata, _ := msg["display_metadata"].(map[string]any)
			label := ""
			if displayKind == "async_delegation_complete" || displayKind == "process_complete" {
				label = cliStr(metadata["display_text"], "")
			}
			if label == "" {
				label = i18n.T(c.ctx, key, "", nil)
			}
			entries = append(entries, resumeEntry{"event", tools.SanitizeDisplayText(label)})
			continue
		}
		switch role {
		case "user":
			text, _ := state.WithheldPromptText(msg)
			if text == "" {
				text = userDisplayText(content)
			}
			text = tools.SanitizeDisplayText(text)
			if len([]rune(text)) > maxUserLen {
				text = string([]rune(text)[:maxUserLen]) + "..."
			}
			entries = append(entries, resumeEntry{"user", text})
		case "assistant":
			text := ""
			if content != nil {
				text = cleanAssistant(cliStr(content, ""))
			}
			var parts, fullParts []string
			if text != "" {
				fullParts = append(fullParts, text)
				lines := strings.Split(text, "\n")
				if len(lines) > maxAsstLines {
					text = strings.Join(lines[:maxAsstLines], "\n") + " ..."
				}
				if len([]rune(text)) > maxAsstLen {
					text = string([]rune(text)[:maxAsstLen]) + "..."
				}
				parts = append(parts, text)
			}
			if len(toolCalls) > 0 {
				summary := toolCallsSummary(c, toolCalls)
				parts = append(parts, summary)
				fullParts = append(fullParts, summary)
			}
			if text == "" && (skipToolOnly || len(toolCalls) == 0) {
				continue
			}
			entries = append(entries, resumeEntry{"assistant", strings.Join(parts, " ")})
			lastAsstIdx = len(entries) - 1
			lastAsstFull = strings.Join(fullParts, " ")
		}
	}
	return entries, lastAsstIdx, lastAsstFull
}

var resumeSkinColors = [][2]string{{"banner_text", "#FFF8DC"}, {"session_label", "#DAA520"}, {"session_border", "#8B8682"}, {"ui_ok", "#8FBC8F"}}

// Returns the active skin's resume panel colors, the defaults when no skin loads
func resumePanelColors(c *CLI) [4]string {
	var out [4]string
	skin := config.GetActiveSkin(c.ctx)
	for i, kv := range resumeSkinColors {
		out[i] = kv[1]
		if skin != nil {
			out[i] = skin.GetColor(kv[0], kv[1])
		}
	}
	return out
}

// Drops the agent so the next turn rebuilds it, releasing the LLM clients of an agent that holds its own first
func retireAgent(c *CLI) {
	if holder, ok := c.agentRef().(interface{ releaseClients() }); ok {
		holder.releaseClients()
	}
	c.setAgent(nil)
}

// Re-resolves provider credentials before agent use, false on an auth failure
func (c *CLI) ensureRuntimeCredentials() bool {
	modelAtEntry := c.model
	c.credentialsRateLimited = false
	c.credentialsTerminal = false
	runtime, primaryErr := resolveRuntimeProviderFn(c.ctx, home.Dir(c.ctx), c.requestedProvider, c.model, c.explicitBaseURL, c.explicitAPIKey)
	if primaryErr != nil {
		runtime = nil
		if fb := c.resolveFallbackRuntime(primaryErr); fb != nil {
			runtime = fb
			primaryErr = nil
		}
	}
	if runtime == nil {
		var authErr *provider.AuthError
		c.credentialsRateLimited = primaryErr != nil && provider.IsRateLimitedAuthError(primaryErr)
		c.credentialsTerminal = errors.As(primaryErr, &authErr) && authErr.ReloginRequired
		message := i18n.T(c.ctx, "cli.startup.provider_resolution_failed", "", nil)
		if primaryErr != nil {
			message = provider.FormatRuntimeProviderError(primaryErr)
		}
		if c.toolProgressMode == "off" {
			fmt.Fprintln(stderrWriter(), message)
		} else {
			chatConsolePrint(fmt.Sprintf("[bold red]%s[/]", message))
		}
		return false
	}
	apiKey := runtime.APIKey
	baseURL := runtime.BaseURL
	resolvedProvider := runtime.Provider
	if resolvedProvider == "" {
		resolvedProvider = "openrouter"
	}
	apiMode := string(runtime.APIMode)
	if apiMode == "" {
		apiMode = c.apiMode
	}
	resolvedArgs := append([]string{}, runtime.Args...)
	if runtime.TokenSource == nil && apiKey == "" {
		if keylessCustomBase(baseURL) {
			apiKey = "no-key-required"
			cliLogger.Debug(fmt.Sprintf("No API key for custom endpoint %s (source=%s), using placeholder — local servers typically ignore auth", baseURL, runtime.Source))
		} else {
			prov := strings.TrimSpace(firstNonEmpty(resolvedProvider, c.requestedProvider))
			if prov != "" && prov != "auto" {
				fmt.Fprintf(c.console.Out, "\n%s\n", i18n.T(c.ctx, "cli.startup.no_api_key_for_provider", "", map[string]any{"provider": prov}))
			} else {
				fmt.Fprintf(c.console.Out, "\n%s\n", i18n.T(c.ctx, "cli.startup.no_provider_configured", "", nil))
			}
			fmt.Fprintf(c.console.Out, "   %s\n", i18n.T(c.ctx, "cli.startup.run_model_or_setup", "", nil))
			return false
		}
	}
	if baseURL == "" {
		fmt.Fprintf(c.console.Out, "\n%s\n", i18n.T(c.ctx, "cli.startup.empty_base_url", "", nil))
		return false
	}
	credentialsChanged := apiKey != c.apiKey || baseURL != c.baseURL
	routingChanged := resolvedProvider != c.provider || apiMode != c.apiMode || runtime.Command != c.acpCommand || strings.Join(resolvedArgs, "\x00") != strings.Join(c.acpArgs, "\x00")
	c.provider, c.apiMode, c.acpCommand, c.acpArgs = resolvedProvider, apiMode, runtime.Command, resolvedArgs
	c.credentialPool = runtime.CredentialPool
	c.providerSource = runtime.Source
	c.apiKey = apiKey
	c.baseURL = baseURL
	c.resolvedRuntime = runtime
	if runtime.Model != "" && (c.model == "" || c.model == c.provider || c.model == runtime.RequestedProvider) {
		c.model = runtime.Model
	}
	if c.model == "" && resolvedProvider != "" {
		if def := provider.GetDefaultModelForProvider(c.ctx, resolvedProvider); def != "" {
			c.model = def
			cliLogger.Info(fmt.Sprintf("No model configured — defaulting to %s for provider %s", def, resolvedProvider))
		}
	}
	modelChanged := c.normalizeModelForProvider(resolvedProvider)
	if c.model != modelAtEntry && c.explicitReasoningConfig == nil {
		c.resolveCLIReasoning()
		cliLogger.Info(fmt.Sprintf("Model moved to %s: reasoning_config resolved: %v", c.model, c.reasoningConfig))
	}
	if (credentialsChanged || routingChanged || modelChanged) && c.agentRef() != nil {
		retireAgent(c)
		c.activeAgentRouteSignature = ""
	}
	return true
}

// Re-resolves the CLI level reasoning config for the current model through the shared chokepoint
func (c *CLI) resolveCLIReasoning() {
	c.reasoningConfig = config.ResolveReasoningConfig(c.config, c.model)
	c.reasoningUserSet = false
}

// Normalizes provider specific model ids and routing, true when the model or api mode changed
func (c *CLI) normalizeModelForProvider(resolvedProvider string) bool {
	currentModel := strings.TrimSpace(c.model)
	changed := false
	adopt := func(canonical string, notice func(string) string) {
		if canonical != "" && canonical != currentModel {
			if !c.modelIsDefault {
				c.consolePrint(fmt.Sprintf("[yellow]⚠️  %s[/]", notice(canonical)))
			}
			c.model = canonical
			currentModel = canonical
			changed = true
		}
	}
	adoptWithMode := func(normalize func(string) string, apiModeOf func(string) string, notice func(string) string) bool {
		adopt(normalize(currentModel), notice)
		if resolvedMode := apiModeOf(currentModel); resolvedMode != c.apiMode {
			c.apiMode = resolvedMode
			changed = true
		}
		return changed
	}
	normalizedNotice := func(newModel string) string {
		return i18n.T(c.ctx, "cli.model.normalized_model", "", map[string]any{"old": currentModel, "new": newModel, "provider": resolvedProvider})
	}
	strippedNotice := func(providerLabel string) func(string) string {
		return func(newModel string) string {
			return i18n.T(c.ctx, "cli.model.stripped_provider_prefix", "", map[string]any{"old": currentModel, "new": newModel, "provider": providerLabel})
		}
	}
	if !containsString(provider.AggregatorProviders(), resolvedProvider) {
		adopt(provider.NormalizeModelForProvider(currentModel, resolvedProvider), normalizedNotice)
	}
	if profile, ok := provider.GetProviderProfile(c.ctx, resolvedProvider); ok {
		if nativeID := profile.NormalizeModelID(currentModel, c.apiKey); nativeID != "" {
			return adoptWithMode(func(string) string { return nativeID }, func(m string) string {
				if mode, ok := provider.ModelDerivedAPIMode(c.ctx, resolvedProvider, m, c.apiKey); ok && mode != "" {
					return mode
				}
				return c.apiMode
			}, normalizedNotice)
		}
	}
	if provider.OpenCodeFamily(resolvedProvider) != "" {
		return adoptWithMode(func(m string) string { return provider.NormalizeOpenCodeModelID(resolvedProvider, m) },
			func(m string) string { return string(provider.OpenCodeModelMode(resolvedProvider, m)) }, strippedNotice(resolvedProvider))
	}
	if resolvedProvider != "openai-codex" {
		return changed
	}
	if i := strings.Index(currentModel, "/"); i >= 0 {
		slug := currentModel[i+1:]
		if !c.modelIsDefault {
			c.consolePrint("[yellow]⚠️  " + pyrich.Escape(strippedNotice("OpenAI Codex")(slug)) + "[/]")
		}
		c.model = slug
		currentModel = slug
		changed = true
	}
	if c.modelIsDefault {
		fallbackModel := provider.DefaultCodexModels[0]
		if available, ok := provider.GetCodexModelIDs(c.ctx, c.apiKey, c.baseURL); ok && len(available) > 0 {
			fallbackModel = available[0]
		}
		if currentModel != fallbackModel {
			c.model = fallbackModel
			changed = true
		}
	}
	return changed
}

// Tries each fallback entry after an auth failure of the primary provider, switching the CLI to the first that resolves
func (c *CLI) resolveFallbackRuntime(primaryErr error) *provider.Runtime {
	var authErr *provider.AuthError
	if !errors.As(primaryErr, &authErr) {
		return nil
	}
	for _, fb := range c.fallbackModel {
		fbProvider := strings.ToLower(strings.TrimSpace(cliStr(fb["provider"], "")))
		fbModel := strings.TrimSpace(cliStr(fb["model"], ""))
		if fbProvider == "" || fbModel == "" {
			continue
		}
		runtime, err := provider.ResolveRuntimeProvider(c.ctx, home.Dir(c.ctx), fbProvider, fbModel, cliStr(fb["base_url"], ""), provider.AuxFallbackEntryAPIKey(c.ctx, fb))
		if err != nil || runtime == nil {
			continue
		}
		whyLog, why := provider.PrimaryFailureWording(authErr)
		cliLogger.Warn(fmt.Sprintf("Primary provider %s (%v). Falling through to fallback: %s/%s", whyLog, primaryErr, fbProvider, fbModel))
		_, _ = config.RenderNotification(c.ctx, func() error {
			cprint(i18n.T(c.ctx, "cli.startup.switching_to_fallback", "", map[string]any{"reason": why, "provider": fbProvider, "model": fbModel}))
			return nil
		}, "cli", true, nil)
		c.requestedProvider = fbProvider
		c.model = fbModel
		return runtime
	}
	return nil
}

// Silently probes whether any inference provider can be resolved
func (c *CLI) runtimeCredentialsReady() bool {
	ready, _ := c.probeRuntimeCredentials()
	return ready
}

// Reports readiness and the error that stopped resolution, never printing or mutating CLI state
func (c *CLI) probeRuntimeCredentials() (bool, error) {
	runtime, err := resolveRuntimeProviderFn(c.ctx, home.Dir(c.ctx), c.requestedProvider, "", c.explicitBaseURL, c.explicitAPIKey)
	if err != nil {
		return false, err
	}
	if runtime == nil {
		return false, nil
	}
	if runtime.TokenSource != nil || runtime.APIKey != "" {
		return runtime.BaseURL != "", nil
	}
	var authErr error
	if runtime.AuthError != nil {
		authErr = runtime.AuthError
	}
	return keylessCustomBase(runtime.BaseURL), authErr
}

// Routes a blank install to the provider wizard, a benched or signed out profile to its reason
func (c *CLI) maybeOfferFirstRunSetup() {
	if !c.console.StdinIsTTY() {
		return
	}
	ready, err := c.probeRuntimeCredentials()
	if !ready && !c.explainUnusableCredentials(err) {
		c.offerFirstRunSetup()
	}
}

// Prints why a configured credential is unusable, false when nothing is configured at all
func (c *CLI) explainUnusableCredentials(err error) bool {
	if err == nil {
		return false
	}
	var authErr *provider.AuthError
	isAuth := errors.As(err, &authErr)
	if isAuth && authErr.Code == "no_provider_configured" {
		return false
	}
	providerName := c.requestedProvider
	if isAuth && authErr.Provider != "" {
		providerName = authErr.Provider
	}
	cooling, lines := false, []string{}
	if providerName != "" && providerName != "auto" {
		cooling, lines = credentialPoolNotice(c, providerName)
	}
	cprint("")
	if cooling {
		cprint("⚠️  " + pyrich.Escape(lines[0]))
		lines = lines[1:]
		cprint("  " + pyrich.Escape(err.Error()))
	} else if isAuth {
		cprint("⚠️  " + pyrich.Escape(provider.FormatAuthError(authErr)))
	} else {
		cprint("⚠️  " + pyrich.Escape(err.Error()))
	}
	for _, line := range lines {
		cprint("  " + pyrich.Escape(line))
	}
	return true
}

// Offers the provider picker when no provider is configured, true when one was configured
func (c *CLI) offerFirstRunSetup() bool {
	cprint("")
	cprint(i18n.T(c.ctx, "cli.startup.first_run_no_provider", "", nil))
	cprint("  " + i18n.T(c.ctx, "cli.startup.first_run_pick_provider", "", nil))
	answer, err := c.console.Input("  " + i18n.T(c.ctx, "cli.startup.first_run_prompt", "", nil) + " ")
	if err != nil {
		fmt.Fprintln(c.console.Out)
		answer = "n"
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer == "n" || answer == "no" {
		cprint("  " + i18n.T(c.ctx, "cli.startup.first_run_skipped", "", nil))
		return false
	}
	setupErr := observability.ProviderSetupSurface(c.ctx, "cli_setup", func(ctx context.Context) error {
		return selectProviderAndModel(ctx)
	})
	if setupErr != nil {
		if errors.Is(setupErr, ErrSetupCancelled) {
			fmt.Fprintln(c.console.Out)
			cprint("  " + i18n.T(c.ctx, "cli.startup.first_run_cancelled", "", nil))
			return false
		}
		cliLogger.Debug(fmt.Sprintf("first-run provider setup failed: %v", setupErr))
		cprint("  " + i18n.T(c.ctx, "cli.startup.provider_setup_failed", "", map[string]any{"error": setupErr}))
		cprint("  " + i18n.T(c.ctx, "cli.startup.run_model_to_retry", "", nil))
		return false
	}
	if cfg, lerr := config.Load(home.Dir(c.ctx), nil); lerr == nil {
		if modelCfg, ok := cfg["model"].(map[string]any); ok {
			if p := strings.TrimSpace(cliStr(modelCfg["provider"], "")); p != "" {
				c.requestedProvider = p
			}
			newModel := strings.TrimSpace(cliStr(modelCfg["default"], cliStr(modelCfg["model"], "")))
			if newModel != "" {
				c.model = newModel
				if c.explicitReasoningConfig == nil {
					c.resolveCLIReasoning()
				}
			}
		}
	} else {
		cliLogger.Debug(fmt.Sprintf("first-run config re-sync failed: %v", lerr))
	}
	retireAgent(c)
	c.activeAgentRouteSignature = ""
	if c.runtimeCredentialsReady() {
		cprint("  " + i18n.T(c.ctx, "cli.startup.provider_configured", "", nil))
		return true
	}
	cprint("  " + i18n.T(c.ctx, "cli.startup.provider_setup_incomplete", "", nil))
	return false
}

// Returns the effective model and runtime for one turn, attaching static fast tier request overrides
func (c *CLI) resolveTurnAgentConfig(userMessage any) turnRoute {
	rt := currentRuntime(c)
	route := turnRoute{model: c.model, runtime: rt, signature: routeSignature(c.model, rt)}
	if provider.StaticTiers[c.serviceTier] {
		route.requestOverrides = provider.ResolveFastModeOverrides(route.model, rt.Provider, rt.BaseURL, c.serviceTier)
	}
	return route
}

// Switches a resumed empty compression chain head to the descendant holding the messages
func (c *CLI) followCompressionChain(sessionMeta map[string]any, announce func(string)) map[string]any {
	c.resumeReadID = c.sessionID
	if resolvedID := compressionDescendant(c.ctx, c.sessionDB, c.sessionID); resolvedID != "" {
		if meta := c.sessionMeta(resolvedID); meta != nil {
			sessionMeta = meta
		}
		c.resumeReadID = resolvedID
		if owner := gatewayOwnedID(c.ctx, c.sessionDB, resolvedID); owner != resolvedID {
			c.sessionID = owner
		} else {
			announce(resolvedID)
			c.sessionID = resolvedID
		}
	}
	return sessionMeta
}

// Returns a session row as a plain map, nil when the store has no such row
func (c *CLI) sessionMeta(sessionID string) map[string]any {
	meta, err := c.sessionDB.GetSession(c.ctx, sessionID)
	if err != nil || len(meta) == 0 {
		return nil
	}
	return meta
}

// Restores cwd, yolo and model from the resumed session's metadata
func (c *CLI) restoreSessionState(sessionMeta map[string]any, quiet bool) {
	c.restoreSessionCwd(sessionMeta, quiet)
	c.restoreSessionYolo(sessionMeta, quiet)
	c.restoreSessionModel(sessionMeta, quiet)
}

// Clears ended_at so the resumed session is active again
func (c *CLI) reopenSession() { c.sessionDB.ReopenSession(c.ctx, c.sessionID) }

// Validates a late resume and loads its history from the DB, false when the resume must abort
func (c *CLI) loadResumedHistoryLate() bool {
	sessionMeta := c.sessionMeta(c.sessionID)
	quietMode := c.toolProgressMode == "off"
	say := func(plain, rich string) {
		if quietMode {
			fmt.Fprintln(stderrWriter(), plain)
		} else {
			chatConsolePrint(rich)
		}
	}
	if sessionMeta == nil {
		hint := i18n.T(c.ctx, "cli.resume.session_id_hint", "", nil)
		notFound := i18n.T(c.ctx, "cli.resume.session_not_found", "", map[string]any{"session_id": c.sessionID})
		if quietMode {
			fmt.Fprintln(stderrWriter(), notFound)
			fmt.Fprintln(stderrWriter(), hint)
		} else {
			cprint("\033[1;31m" + notFound + "\033[0m")
			cprint("\033[2m" + hint + "\033[0m")
		}
		return false
	}
	sessionMeta = c.followCompressionChain(sessionMeta, func(rid string) {
		chatConsolePrint(fmt.Sprintf("[dim]%s[/dim]", pyrich.Escape(i18n.T(c.ctx, "cli.resume.compressed_into", "", map[string]any{"session_id": c.sessionID, "descendant": rid}))))
	})
	if c.resumeHistoryError != "" {
		return false
	}
	if limitErr := c.resumeHistoryLimitError(true); limitErr != "" {
		c.resumeHistoryError = limitErr
		say(i18n.T(c.ctx, "cli.resume.cannot_resume", "", map[string]any{"error": limitErr}),
			fmt.Sprintf("[bold red]%s[/] %s", pyrich.Escape(i18n.T(c.ctx, "cli.resume.cannot_resume_prefix", "", nil)), pyrich.Escape(limitErr)))
		return false
	}
	restored, err := c.sessionDB.GetMessagesAsConversation(c.ctx, c.resumeReadID, state.ConversationOptions{RepairAlternation: true})
	if err != nil {
		restored = nil
	}
	if len(restored) > 0 {
		filtered := make([]map[string]any, 0, len(restored))
		msgCount := 0
		for _, m := range restored {
			if m["role"] == "session_meta" {
				continue
			}
			if m["role"] == "user" {
				msgCount++
			}
			filtered = append(filtered, m)
		}
		c.conversationHistory = filtered
		titlePart := ""
		if title := cliStr(sessionMeta["title"], ""); title != "" {
			titlePart = fmt.Sprintf(" %q", title)
		}
		counts := resumeCounts(c, msgCount, len(filtered))
		say(fmt.Sprintf("%s %s%s %s", i18n.T(c.ctx, "cli.resume.resumed_session", "", nil), c.sessionID, titlePart, counts),
			fmt.Sprintf("[bold %s]%s[/] [bold]%s[/][bold %s]%s[/] %s", accentHex(), pyrich.Escape(i18n.T(c.ctx, "cli.resume.resumed_session", "", nil)),
				pyrich.Escape(c.sessionID), accentHex(), pyrich.Escape(titlePart), counts))
		c.restoreSessionState(sessionMeta, quietMode)
	} else {
		noMessages := i18n.T(c.ctx, "cli.resume.session_no_messages", "", map[string]any{"session_id": c.sessionID})
		say(noMessages, fmt.Sprintf("[bold %s]%s[/]", accentHex(), pyrich.Escape(noMessages)))
	}
	c.reopenSession()
	return true
}

// Builds the agent on first use, restoring history from SQLite when resuming, true on success
func (c *CLI) initAgent(modelOverride string, runtimeOverride *cliRuntime, requestOverrides map[string]any) bool {
	if c.agentRef() != nil {
		return true
	}
	if err := c.finalizePreloadedSkills(); err != nil {
		chatConsolePrint(fmt.Sprintf("[bold red]%s[/]", pyrich.Escape(agentInitFailureMessage(err))))
		return false
	}
	c.installToolCallbacks()
	if !ensureTurnCredentials(c) {
		return false
	}
	if c.sessionDB == nil {
		c.sessionDB = gatewaycli.SessionStore()
	}
	if err := c.ensureGatewaySession(); err != nil {
		raiseSystemExit(ExitOnResumeRefusal(err))
		chatConsolePrint(fmt.Sprintf("[bold red]%s[/]", pyrich.Escape(agentInitFailureMessage(err))))
		return false
	}
	if c.resumed && c.sessionDB != nil && len(c.conversationHistory) == 0 && !c.loadResumedHistoryLate() {
		return false
	}
	rt := runtimeOverride
	if rt == nil {
		rt = currentRuntime(c)
	}
	effectiveModel := firstNonEmpty(modelOverride, c.model)
	pendingOneTurn := c.pendingOneTurnModelRestore
	oneTurn := pendingOneTurn != nil
	o := gatewayAgentOptions{SessionDB: c.sessionDB, SessionID: c.sessionID, Model: effectiveModel, Provider: rt.Provider, BaseURL: rt.BaseURL,
		APIMode: rt.APIMode, MaxIterations: c.maxTurns, QuietMode: !c.verbose, ToolProgressMode: c.toolProgressMode,
		ReasoningConfig: c.reasoningConfig, ServiceTier: c.serviceTier, PushReasoning: c.reasoningUserSet && !oneTurn, OneTurn: oneTurn,
		ReasoningCallback: c.currentReasoningCallback(), ThinkingCallback: c.onThinking, ToolProgressCallback: c.onToolProgress,
		NoticeCallback: c.onNotice, NoticeClearCallback: c.onNoticeClear, ReactionCallback: c.onReaction, ToolEnv: c.env,
		ApprovalCallback: c.approvalCallback, SudoPasswordCallback: c.sudoPasswordCallback}
	if oneTurn {
		o.OneTurnReasoning = strOrEmpty(pendingOneTurn["one_turn_reasoning"])
		o.OneTurnAlias = strOrEmpty(pendingOneTurn["one_turn_alias"])
	}
	if c.singleQueryMode {
		o.ClarifyCallback = singleQueryClarifyCallback
	} else {
		o.ClarifyCallback = c.clarifyCallback
		o.ConnectionCallback = c.connectionCallback
		o.ConnectionUpdateCallback = c.connectionOnChange
		o.ModelConfirmCallback = c.confirmOwnerModelSwitch
		o.ModalWithdrawCallback = c.withdrawGatewayModal
	}
	if c.inlineDiffsEnabled {
		o.ToolCompleteCallback = c.onToolComplete
	}
	if c.streamingEnabled {
		o.StreamDeltaCallback = c.streamDelta
		o.ToolGenCallback = c.onToolGenStart
	}
	a, err := newGatewayAgent(c.ctx, o)
	if err != nil {
		chatConsolePrint(fmt.Sprintf("[bold red]%s[/]", pyrich.Escape(agentInitFailureMessage(err))))
		for _, line := range home.PartialUpdateHint(err) {
			chatConsolePrint(line)
		}
		return false
	}
	c.setAgent(a)
	setActiveAgent(a)
	a.SetControlUpdateCallback(c.markAutomationStale)
	a.SetPrintFn(c.agentStatusPrint)
	a.SetFinite(c.singleQueryMode)
	bindGatewayAgent(a)
	provider.SeedCreditsAtSessionStart(a.CreditsCapture())
	c.activeAgentRouteSignature = routeSignature(effectiveModel, rt)
	if a.ReasoningPushed() {
		c.reasoningUserSet = false
	}
	if c.pendingTitle != "" && c.sessionDB != nil {
		a.ensureDBSession()
		if _, terr := c.sessionDB.SetSessionTitle(c.ctx, c.sessionID, c.pendingTitle); terr != nil {
			cprint("  " + i18n.T(c.ctx, "cli.resume.title_apply_failed", "", map[string]any{"error": terr}))
		} else {
			cprint("  " + i18n.T(c.ctx, "cli.resume.title_applied", "", map[string]any{"title": c.pendingTitle}))
			c.pendingTitle = ""
		}
	}
	return true
}

// Raises the system exit a refusal printed, nothing for any other error
func raiseSystemExit(err error) {
	var exit *pm.SystemExit
	if errors.As(err, &exit) {
		panic(singleQueryExit{code: exit.Code})
	}
}

// Returns a safe resume error without materializing transcript rows, empty when resuming is allowed
func (c *CLI) resumeHistoryLimitError(tipOnly bool) string {
	if c.sessionDB == nil {
		return ""
	}
	readID := firstNonEmpty(c.resumeReadID, c.sessionID)
	if _, err := c.sessionDB.AssertResumeSafe(c.ctx, readID, nil, tipOnly); err != nil {
		var tooLarge *state.SessionResumeTooLargeError
		if errors.As(err, &tooLarge) {
			return tooLarge.Error()
		}
		cliLogger.Warn(fmt.Sprintf("Resume safety check failed for %s (proceeding without guard): %v", c.sessionID, err))
	}
	return ""
}

// Loads a resumed session's history early so run can display it, true when history loaded
func (c *CLI) preloadResumedSession() bool {
	if !c.resumed || c.sessionDB == nil {
		return false
	}
	sessionMeta := c.sessionMeta(c.sessionID)
	if sessionMeta == nil {
		c.consolePrint(fmt.Sprintf("[bold red]%s[/]", pyrich.Escape(i18n.T(c.ctx, "cli.resume.session_not_found", "", map[string]any{"session_id": c.sessionID}))))
		c.consolePrint(fmt.Sprintf("[dim]%s[/]", pyrich.Escape(i18n.T(c.ctx, "cli.resume.session_id_hint", "", nil))))
		return false
	}
	sessionMeta = c.followCompressionChain(sessionMeta, func(rid string) {
		c.consolePrint(fmt.Sprintf("[dim]%s[/]", pyrich.Escape(i18n.T(c.ctx, "cli.resume.compressed_into", "", map[string]any{"session_id": c.sessionID, "descendant": rid}))))
	})
	if refusal := kanban.KanbanResumeRefusal(c.ctx, c.sessionDB, c.resumeReadID); refusal != "" {
		c.consolePrint(fmt.Sprintf("[bold red]%s[/]", pyrich.Escape(refusal)))
		c.resumed = false
		c.sessionID = newSessionID(c.sessionStart)
		return false
	}
	if limitErr := c.resumeHistoryLimitError(false); limitErr != "" {
		c.resumeHistoryError = limitErr
		c.consolePrint(fmt.Sprintf("[bold red]%s[/] %s", pyrich.Escape(i18n.T(c.ctx, "cli.resume.cannot_resume_prefix", "", nil)), limitErr))
		return false
	}
	restored, displayHistory, err := c.sessionDB.GetResumeConversations(c.ctx, c.resumeReadID)
	accent := accentHex()
	if err != nil || len(restored) == 0 {
		c.consolePrint(fmt.Sprintf("[%s]%s[/]", accent, pyrich.Escape(i18n.T(c.ctx, "cli.resume.session_no_messages", "", map[string]any{"session_id": c.sessionID}))))
		return false
	}
	c.conversationHistory = withoutSessionMeta(restored)
	c.resumeDisplayHistory = withoutSessionMeta(displayHistory)
	msgCount := 0
	for _, m := range c.resumeDisplayHistory {
		if agent.IsUserOriginatedTurn(m) {
			msgCount++
		}
	}
	titlePart := ""
	if title := cliStr(sessionMeta["title"], ""); title != "" {
		titlePart = fmt.Sprintf(" %q", title)
	}
	c.consolePrint(fmt.Sprintf("[%s]%s [bold]%s[/bold]%s %s[/]", accent, pyrich.Escape(i18n.T(c.ctx, "cli.resume.resumed_session", "", nil)),
		c.sessionID, titlePart, resumeCounts(c, msgCount, len(c.conversationHistory))))
	c.restoreSessionState(sessionMeta, false)
	c.reopenSession()
	return true
}

// Drops session_meta rows from a stored conversation
func withoutSessionMeta(rows []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, m := range rows {
		if m["role"] != "session_meta" {
			out = append(out, m)
		}
	}
	return out
}

// Renders the dim recap panel of the previous conversation capped at the last resume_exchanges exchanges
func (c *CLI) displayResumedHistory() {
	displayHistory := c.resumeDisplayHistory
	if displayHistory == nil {
		displayHistory = c.conversationHistory
	}
	if len(displayHistory) == 0 || c.resumeDisplay == "minimal" {
		return
	}
	disp := cliMap(c.config, "display")
	entries, lastAsstIdx, lastAsstFull := collectResumeEntries(c, displayHistory, disp, func(t string) string {
		return tools.SanitizeDisplayText(stripReasoningTags(t))
	})
	if len(entries) == 0 {
		return
	}
	skipped := max(0, len(entries)-intOr(mapValueOr(disp, "resume_exchanges", 10), 10)*2)
	entries = entries[skipped:]
	if lastAsstIdx >= 0 && lastAsstFull != "" {
		if adj := lastAsstIdx - skipped; adj >= 0 && adj < len(entries) {
			entries[adj] = resumeEntry{"assistant_last", lastAsstFull}
		}
	}
	paintResumeHistoryPanel(c, entries, skipped, resumePanelColors(c))
}
