package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
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
	return &cliRuntime{BaseURL: c.baseURL, Provider: c.provider, RequestedProvider: c.requestedProvider,
		APIMode: c.apiMode, Command: c.acpCommand, Args: append([]string{}, c.acpArgs...), CredentialPool: c.credentialPool, Resolved: c.resolvedRuntime}
}

// Returns the identity of model and routing that tells when the agent must be rebuilt
func routeSignature(model string, rt *cliRuntime) string {
	return fmt.Sprintf("%q|%q|%q|%q|%q|%q|%q", model, rt.Provider, rt.RequestedProvider, rt.BaseURL, rt.APIMode, rt.Command, strings.Join(rt.Args, "\x00"))
}

// Seams over the pool loader and the runtime resolver, the agent.credential_pool and runtime_provider names the first run tests patch
var (
	resolveRuntimeProviderFn = provider.ResolveRuntimeProvider
)

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

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
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
	// The gateway session runs the turn and mints its own short lived OAuth token, this process only routes
	runtime, primaryErr := resolveRuntimeProviderFn(provider.WithDeferredTokenMint(c.ctx), home.Dir(c.ctx), c.requestedProvider, c.model, c.explicitBaseURL, c.explicitAPIKey)
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
			pkgLog().Debug(fmt.Sprintf("No API key for custom endpoint %s (source=%s), using placeholder — local servers typically ignore auth", baseURL, runtime.Source))
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
	c.apiKey = apiKey
	c.baseURL = baseURL
	c.resolvedRuntime = runtime
	if runtime.Model != "" && (c.model == "" || c.model == c.provider || c.model == runtime.RequestedProvider) {
		c.model = runtime.Model
	}
	if c.model == "" && resolvedProvider != "" {
		if def := provider.GetDefaultModelForProvider(c.ctx, resolvedProvider); def != "" {
			c.model = def
			pkgLog().Info(fmt.Sprintf("No model configured — defaulting to %s for provider %s", def, resolvedProvider))
		}
	}
	modelChanged := c.normalizeModelForProvider(resolvedProvider)
	if c.model != modelAtEntry && c.explicitReasoningConfig == nil {
		c.resolveCLIReasoning()
		pkgLog().Info(fmt.Sprintf("Model moved to %s: reasoning_config resolved: %v", c.model, c.reasoningConfig))
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
		adopt(provider.NormalizeModelForProviderCtx(c.ctx, currentModel, resolvedProvider), normalizedNotice)
	}
	if profile, ok := provider.GetProviderProfile(c.ctx, resolvedProvider); ok {
		if nativeID := profile.NormalizeModelID(c.ctx, currentModel, c.apiKey); nativeID != "" {
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
		runtime, err := provider.ResolveRuntimeProvider(provider.WithDeferredTokenMint(c.ctx), home.Dir(c.ctx), fbProvider, fbModel, cliStr(fb["base_url"], ""), provider.AuxFallbackEntryAPIKey(c.ctx, fb))
		if err != nil || runtime == nil {
			continue
		}
		whyLog, why := provider.PrimaryFailureWording(authErr)
		pkgLog().Warn(fmt.Sprintf("Primary provider %s (%v). Falling through to fallback: %s/%s", whyLog, primaryErr, fbProvider, fbModel))
		_, _ = config.RenderNotification(c.ctx, func() error {
			cprint(i18n.T(c.ctx, "cli.startup.switching_to_fallback", "", map[string]any{"reason": why, "provider": fbProvider, "model": fbModel}))
			return nil
		}, "cli", true)
		c.requestedProvider = fbProvider
		c.model = fbModel
		return runtime
	}
	return nil
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
	if c.env != nil {
		c.env.Ask = singleQueryAsker{}
	}
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
	o := gatewayAgentOptions{SessionDB: c.sessionDB, SessionID: c.sessionID, Model: effectiveModel, Provider: rt.Provider, BaseURL: rt.BaseURL,
		APIMode: rt.APIMode, MaxIterations: c.maxTurns, ToolProgressMode: c.toolProgressMode,
		ReasoningConfig: c.reasoningConfig, ServiceTier: c.serviceTier,
		ReasoningCallback: c.currentReasoningCallback(), ThinkingCallback: c.onThinking, ToolProgressCallback: c.onToolProgress,
		NoticeCallback: c.onNotice, NoticeClearCallback: c.onNoticeClear, ToolEnv: c.env, ClarifyCallback: singleQueryClarifyCallback}
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
		return false
	}
	c.setAgent(a)
	setActiveAgent(a)
	a.SetPrintFn(c.agentStatusPrint)
	c.activeAgentRouteSignature = routeSignature(effectiveModel, rt)
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
		pkgLog().Warn(fmt.Sprintf("Resume safety check failed for %s (proceeding without guard): %v", c.sessionID, err))
	}
	return ""
}
