package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func turnRecoveryCodexLog() *slog.Logger {
	return slog.Default().With("logger", "agent.turn_recovery_codex")
}

// Body or response of an API error as Python str renders it, cut to the first 200 characters
func apiErrorBodyOrResponse(err error) string {
	text := ""
	var apiErr *provider.APIError
	if errors.As(err, &apiErr) && apiErr.Body != "" {
		text = apiErrorBodyText(apiErr)
	} else if status := apiErrorStatus(err); status != nil {
		text = fmt.Sprintf("<Response [%d %s]>", *status, http.StatusText(*status))
	}
	if r := []rune(text); len(r) > 200 {
		text = string(r[:200])
	}
	return text
}

// Prints the Nous 401 guidance once a credential refresh could not lift it
func printNous401Diagnostics(ctx context.Context, a *AIAgent, apiError error) {
	bodyText := apiErrorBodyOrResponse(apiError)
	plines(a, "🔐 Nous 401 — Portal authentication failed.")
	if bodyText != "" {
		plines(a, "   Response: "+bodyText)
	}
	if provider.IsAnonymousRequest(a.provider, a.apiKey) {
		plines(a, "   Your session ended and Icebath couldn't start a new one.",
			"   Sign in with a Nous account (it's free), or switch providers with /model.")
		return
	}
	if !printNousEntitlementGuidance(ctx, a, "Nous model access") {
		plines(a, "   Most likely: Portal OAuth expired, account out of credits, or agent key revoked.")
	}
	plines(a,
		"   Troubleshooting:",
		"     • Re-authenticate: icebath auth add nous",
		"     • Check credits / billing: https://portal.nousresearch.com",
		fmt.Sprintf("     • Verify stored credentials: %s/auth.json", home.Display(home.Dir(ctx))),
		"     • Switch providers temporarily: /model <model> --provider openrouter",
	)
}

// Prints the Anthropic auth method and fixes once a credential refresh could not lift a 401
func printAnthropic401Diagnostics(ctx context.Context, a *AIAgent, key any) {
	plines(a, "🔐 Anthropic 401 — authentication failed.")
	if provider.IsTokenProvider(key) {
		plines(a,
			"   Auth method: Microsoft Entra ID (httpx event hook)",
			"   Run `icebath doctor` for credential-chain diagnostics, or",
			"   `az login` if your developer session expired.",
		)
	} else {
		text, _ := key.(string)
		authMethod := "x-api-key (API key)"
		if provider.IsOAuthToken(text) {
			authMethod = "Bearer (OAuth/setup-token)"
		}
		prefix := "   Token: (empty or short)"
		if r := []rune(text); len(r) > 12 {
			prefix = "   Token prefix: " + string(r[:12]) + "..."
		}
		plines(a, "   Auth method: "+authMethod, prefix)
	}
	dhh := home.Display(home.Dir(ctx))
	plines(a,
		"   Troubleshooting:",
		fmt.Sprintf("     • Check ANTHROPIC_TOKEN in %s/.env for Icebath-managed OAuth/setup tokens", dhh),
		fmt.Sprintf("     • Check ANTHROPIC_API_KEY in %s/.env for API keys or legacy token values", dhh),
		"     • For API keys: verify at https://platform.claude.com/settings/keys",
		"     • Icebath login (OAuth): run 'icebath auth add anthropic' to sign in again, then retry",
		"     • Inspect what Icebath holds: icebath auth list anthropic",
		"     • Legacy cleanup: icebath config set ANTHROPIC_TOKEN \"\"",
		"     • Clear stale keys: icebath config set ANTHROPIC_API_KEY \"\"",
	)
}

// One shot per provider credential refresh on a 401, true when a refresh succeeded and the call retries
func refreshCredentialsAfter401(ctx context.Context, a *AIAgent, apiError error, retry *TurnRetryState, statusCode *int) (bool, error) {
	if statusCode == nil || *statusCode != 401 {
		return false, nil
	}
	if a.apiMode == "codex_responses" && (a.provider == "openai-codex" || a.provider == "xai-oauth") && !retry.CodexAuthRetryAttempted {
		retry.CodexAuthRetryAttempted = true
		if a.tryRefreshCodexClientCredentials(ctx, true) {
			label := "Codex"
			if a.provider == "xai-oauth" {
				label = "xAI OAuth"
			}
			a.bufferVprint(fmt.Sprintf("🔐 %s auth refreshed after 401. Retrying request...", label))
			return true, nil
		}
	}
	if a.apiMode == "chat_completions" && a.provider == "vertex" && !retry.VertexAuthRetryAttempted {
		retry.VertexAuthRetryAttempted = true
		if a.tryRefreshVertexClientCredentials(ctx) {
			a.bufferVprint("🔐 Vertex AI token refreshed after 401. Retrying request...")
			return true, nil
		}
	}
	if (a.apiMode == "chat_completions" || a.apiMode == "anthropic_messages") && a.provider == "nous" && !retry.NousAuthRetryAttempted {
		retry.NousAuthRetryAttempted = true
		if !provider.NousPoolRefreshedThisTurn(poolAuthAgent{a: a, ctx: ctx}) {
			ok, err := a.tryRefreshNousClientCredentials(ctx, true, nil)
			if err != nil {
				return false, err
			}
			if ok {
				a.bufferVprint("🔐 Nous agent key refreshed after 401. Retrying request...")
				return true, nil
			}
		}
		printNous401Diagnostics(ctx, a, apiError)
	}
	if !retry.ProviderAuthRefreshAttempted {
		if profile := credentialRefreshProfile(ctx, a); profile != nil {
			retry.ProviderAuthRefreshAttempted = true
			ok, err := a.tryRefreshProviderRuntimeCredentials(ctx, "unauthorized")
			if err != nil {
				return false, err
			}
			if ok {
				notice, _ := provider.AskOnErrorPath(ctx, profile, "credential_refresh_notice", "", map[string]any{"reason": "unauthorized"}).(string)
				if notice == "" {
					name := profile.DisplayName
					if name == "" {
						name = profile.Name
					}
					notice = name + " credentials refreshed after 401. Retrying request..."
				}
				a.bufferVprint("🔐 " + notice)
				return true, nil
			}
		}
	}
	if a.apiMode == "anthropic_messages" && !retry.AnthropicAuthRetryAttempted {
		retry.AnthropicAuthRetryAttempted = true
		ok, err := a.tryRefreshAnthropicClientCredentials(ctx)
		if err != nil {
			return false, err
		}
		if ok {
			plines(a, "🔐 Anthropic credentials refreshed after 401. Retrying request...")
			return true, nil
		}
		var key any = a.anthropicAPIKey
		if a.anthropicTokenSource != nil {
			key = a.anthropicTokenSource
		}
		printAnthropic401Diagnostics(ctx, a, key)
	}
	return false, nil
}

// Indexes and maps of the dict rows in a request copy, the shared rows the in place repairs rewrite
func requestDictRows(apiMessages []any) ([]int, []map[string]any) {
	var idx []int
	var rows []map[string]any
	for i, m := range apiMessages {
		if d, ok := m.(map[string]any); ok {
			idx = append(idx, i)
			rows = append(rows, d)
		}
	}
	return idx, rows
}

// Thinking signature, stale reasoning, native compaction and llama.cpp grammar one shot repairs, true when retrying
func recoverFormatErrors(a *AIAgent, apiError error, classified *provider.Classified, retry *TurnRetryState, messages []*state.Message, apiMessages []any) (bool, error) {
	if classified.Reason == provider.ReasonThinkingSignature && !retry.ThinkingSigRetryAttempted {
		retry.ThinkingSigRetryAttempted = true
		removed := 0
		var detail string
		if a.rejectedThinkingTracker().TracksRejectedThinking() {
			idx, rows := requestDictRows(apiMessages)
			removed = rememberRejectedThinking(a, rows)
			for i, r := range rows {
				apiMessages[idx[i]] = r
			}
			detail = "suppressed rejected Anthropic replay blocks"
		} else {
			for _, m := range apiMessages {
				if d, ok := m.(map[string]any); ok {
					if _, has := d["reasoning_details"]; has {
						delete(d, "reasoning_details")
						removed++
					}
				}
			}
			detail = "stripped reasoning_details"
		}
		vlines(a, fmt.Sprintf("⚠️  Thinking block signature invalid, %s and retrying...", detail))
		conversationLoopLog().Warn(fmt.Sprintf("%sThinking block signature recovery: %s from %d carrier/message(s) (canonical messages unchanged)", a.logPrefix, detail, removed))
		return true, nil
	}

	if classified.Reason == provider.ReasonInvalidEncryptedContent && recoverStaleCodexReasoning(a, retry, messages, apiMessages) {
		return true, nil
	}

	if a.apiMode == "codex_responses" && !retry.NativeCompactionRejectRetryAttempted && a.codexResponsesNativeCompaction {
		status := 0
		if s := apiErrorStatus(apiError); s != nil {
			status = *s
		}
		if provider.IsNativeCompactionRejection(apiError.Error(), status) {
			retry.NativeCompactionRejectRetryAttempted = true
			a.codexResponsesNativeCompaction = false
			vlines(a, "⚠️  Provider rejected native compaction (context_management) — disabled for this session, "+
				"local compression stays active. Retrying...")
			conversationLoopLog().Warn(fmt.Sprintf("%sNative compaction rejection recovery: disabled codex_responses_native for this session and retrying", a.logPrefix))
			return true, nil
		}
	}

	if classified.Reason == provider.ReasonLlamaCppGrammarPattern && !retry.LlamaCppGrammarRetryAttempted {
		retry.LlamaCppGrammarRetryAttempted = true
		schemas := make([]map[string]any, 0, len(a.tools))
		for _, tool := range a.tools {
			if d, ok := tool.(map[string]any); ok {
				schemas = append(schemas, d)
			}
		}
		stripped := 0
		if len(schemas) > 0 {
			_, stripped = tools.StripPatternAndFormat(schemas)
		}
		if stripped > 0 {
			vlines(a, fmt.Sprintf("⚠️  llama.cpp rejected tool schema grammar — stripped %d pattern/format keyword(s), retrying...", stripped))
			conversationLoopLog().Warn(fmt.Sprintf("%sllama.cpp grammar recovery: stripped %d pattern/format keyword(s) from tool schemas", a.logPrefix, stripped))
			return true, nil
		}
		conversationLoopLog().Warn(fmt.Sprintf("%sllama.cpp grammar error but no pattern/format keywords to strip — falling through to normal retry", a.logPrefix))
	}
	return false, nil
}

// Chat copy naming each healed Nous free tier route
var welcomeRouteHealCopy = map[string]string{
	"anon_on_paid_host":     "Reconnected to the free model's own route.",
	"named_on_welcome_host": "Reconnected to your Nous account's own route.",
	"session_expired":       "Renewed the connection to the free model.",
}

// Moves a model_not_free refusal to the gateway's alternate, or heals a stale free tier route, once each
func recoverWelcomeTier(ctx context.Context, a *AIAgent, classified *provider.Classified, retry *TurnRetryState) bool {
	refusal, _ := classified.Context["welcome_refusal"].(map[string]any)
	if typed, ok := classified.Context["welcome_refusal"].(provider.WelcomeRefusal); ok {
		refusal = map[string]any{"reason": typed.Reason, "alternates": typed.Alternates}
	}
	if refusal != nil && refusal["reason"] == "model_not_free" && !retry.WelcomeModelSwitchAttempted {
		retry.WelcomeModelSwitchAttempted = true
		var alternates []string
		if list, ok := refusal["alternates"].([]any); ok {
			for _, alt := range list {
				if s, ok := alt.(string); ok && s != "" {
					alternates = append(alternates, s)
				}
			}
		} else if list, ok := refusal["alternates"].([]string); ok {
			for _, s := range list {
				if s != "" {
					alternates = append(alternates, s)
				}
			}
		}
		requested := a.model
		if len(alternates) > 0 && alternates[0] != requested {
			target := alternates[0]
			a.model, a.modelNone = target, false
			a.nousModelSwitch = &[2]string{requested, target}
			vlines(a, fmt.Sprintf("↪️  %s isn't available without signing in; using %s for now. Retrying...", requested, target))
			conversationLoopLog().Info(fmt.Sprintf("%sNous free tier: moved %s -> %s after model_not_free", a.logPrefix, requested, target))
			return true
		}
	}
	route, _ := classified.Context["welcome_route"].(string)
	if copyText, known := welcomeRouteHealCopy[route]; known && !retry.WelcomeRouteHealAttempted {
		retry.WelcomeRouteHealAttempted = true
		healed, err := a.tryRefreshNousClientCredentials(ctx, true, nil)
		if err == nil && healed {
			vlines(a, fmt.Sprintf("🔐 %s Retrying request...", copyText))
			return true
		}
	}
	return false
}

// Retries a credit limited 402 once with the output cap lowered to the affordable budget
func clampToAffordableBudget(a *AIAgent, apiError error, classified *provider.Classified, retry *TurnRetryState) bool {
	if classified.Reason != provider.ReasonBilling || retry.Affordable402ClampAttempted {
		return false
	}
	affordable, ok := provider.AffordableMaxTokensFromError(apiError)
	if !ok {
		return false
	}
	retry.Affordable402ClampAttempted = true
	a.ephemeralMaxOutputTokens = affordable
	vlines(a, fmt.Sprintf("💳 Credit balance covers fewer output tokens — retrying with max_tokens=%s...", pystr.GroupThousands(int64(affordable))))
	conversationLoopLog().Info(fmt.Sprintf("%sCredit-limited 402: retrying once with max_tokens=%d", a.logPrefix, affordable))
	return true
}

// Codex replay controls of the live transport, else of the live client
func codexReplayControl(a *AIAgent) provider.CodexReplayControl {
	if c, ok := a.getTransport("").(provider.CodexReplayControl); ok && c != nil {
		return c
	}
	if c, ok := a.client.(provider.CodexReplayControl); ok && c != nil {
		return c
	}
	return nil
}

// Omits the all turns reasoning context after a 400 naming it, true when the transport applied it
func rejectAllTurns(a *AIAgent, apiError error) bool {
	if c := codexReplayControl(a); c != nil {
		return c.RejectAllTurns(apiError)
	}
	return false
}

// Error context the credential pool sees, a usage window 402 benching the refused model until its reset
func poolErrorContext(a *AIAgent, classified *provider.Classified, errorContext map[string]any) map[string]any {
	if !pyTruthy(classified.Context["usage_window"]) {
		return errorContext
	}
	out := maps.Clone(errorContext)
	if out == nil {
		out = map[string]any{}
	}
	out["usage_window"] = true
	maps.Copy(out, usageWindowResetContext(a))
	return out
}

// Epoch seconds the refused usage window frees up, empty when the credits state names none
func usageWindowResetContext(a *AIAgent) (out map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			conversationLoopLog().Debug("usage-window reset unavailable for the pool", "error", fmt.Sprint(r))
			out = map[string]any{}
		}
	}()
	capture := a.creditsCapture()
	st := capture.Credits()
	if st == nil {
		return map[string]any{}
	}
	resetsMs := provider.WindowStopResetsAtMs(st, provider.CurrentModelIsTopTier(capture))
	if resetsMs == nil || *resetsMs <= 0 {
		return map[string]any{}
	}
	return map[string]any{"reset_at": float64(*resetsMs) / 1000}
}

// Refreshes Nous runtime credentials after a fresh account check confirms paid access
func tryRefreshNousPaidEntitlementCredentials(ctx context.Context, a *AIAgent) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()
	info := provider.GetNousPortalAccountInfo(ctx, true)
	if info == nil || info.PaidServiceAccess == nil || !*info.PaidServiceAccess {
		return false
	}
	refreshed, err := a.tryRefreshNousClientCredentials(ctx, true, nil)
	return err == nil && refreshed
}

// 401 token_expired from the Codex backend, the stale replayed reasoning blob signature
func isCodexTokenExpired(apiError error) bool {
	if status := apiErrorStatus(apiError); status == nil || *status != 401 {
		return false
	}
	reason, ok := provider.ExtractAPIErrorContext(apiError)["reason"].(string)
	return ok && pystr.Lower(pystr.Strip(reason)) == "token_expired"
}

// Drops replayed blobs minted elsewhere, else strips every cached reasoning item once, disabling replay on a repeat
func recoverStaleCodexReasoning(a *AIAgent, retry *TurnRetryState, messages []*state.Message, apiMessages []any) bool {
	if retry.InvalidEncryptedContentRetryAttempted || a.apiMode != "codex_responses" || !a.codexReasoningReplayEnabled {
		return false
	}
	carries := false
	for _, m := range messages {
		if m == nil || m.Role != "assistant" {
			continue
		}
		if items, ok := state.MessageToDict(m)["codex_reasoning_items"].([]any); ok && len(items) > 0 {
			carries = true
			break
		}
	}
	if !carries {
		return false
	}
	if c := codexReplayControl(a); c != nil {
		idx, rows := requestDictRows(apiMessages)
		wire := make([]*state.Message, len(rows))
		for i, r := range rows {
			wire[i] = state.MessageFromDict(r)
		}
		removed := c.DropUnverifiedReplay(messages, wire)
		for i, r := range rows {
			if items, has := state.MessageToDict(wire[i])["codex_reasoning_items"]; has {
				r["codex_reasoning_items"] = items
			} else {
				delete(r, "codex_reasoning_items")
			}
			apiMessages[idx[i]] = r
		}
		if removed > 0 {
			vlines(a, fmt.Sprintf("⚠️  Encrypted reasoning replay was rejected — dropped %d item(s) minted elsewhere, retrying...", removed))
			return true
		}
	}
	retry.InvalidEncryptedContentRetryAttempted = true
	keepReplay := !a.codexReasoningReplayRejected
	a.codexReasoningReplayRejected = true
	rows := messagesToRows(messages)
	stats := a.disableCodexReasoningReplay(rows, keepReplay)
	for i, r := range rows {
		if r == nil {
			continue
		}
		if _, has := r["codex_reasoning_items"]; !has && messages[i].CodexReasoningItems != nil {
			*messages[i] = *state.MessageFromDict(r)
		}
	}
	for _, m := range apiMessages {
		if d, ok := m.(map[string]any); ok {
			delete(d, "codex_reasoning_items")
		}
	}
	action := "disabled replay for this session and stripped"
	if keepReplay {
		action = "stripped stale"
	}
	vlines(a, fmt.Sprintf("⚠️  Encrypted reasoning replay was rejected by the provider — %s %d item(s) from %d message(s), retrying...", action, stats["items"], stats["messages"]))
	turnRecoveryCodexLog().Warn(fmt.Sprintf("%sInvalid encrypted reasoning recovery: %s %d items from %d messages", a.logPrefix, action, stats["items"], stats["messages"]))
	return true
}

// One shot recovery chain after classification, reporting a retry and whether the credential pool recovered
func recoverAfterClassification(ctx context.Context, a *AIAgent, s *LoopState, e *apiErrorTurn) (bool, bool, error) {
	retry, classified, apiError := s.Retry, e.classified, e.apiError
	if recoverWelcomeTier(ctx, a, classified, retry) || clampToAffordableBudget(a, apiError, classified, retry) ||
		(a.apiMode == "codex_responses" && rejectAllTurns(a, apiError)) {
		return true, false, nil
	}

	if isCodexTokenExpired(apiError) && recoverStaleCodexReasoning(a, retry, s.Messages, s.APIMessages) {
		return true, false, nil
	}

	if classified.Reason == provider.ReasonBilling && !pyTruthy(classified.Context["usage_window"]) &&
		isNousInferenceRoute(a.provider, a.baseURL) && !retry.NousPaidEntitlementRefreshAttempted {
		retry.NousPaidEntitlementRefreshAttempted = true
		if tryRefreshNousPaidEntitlementCredentials(ctx, a) {
			vlines(a, "🔐 Nous paid access verified — refreshed runtime credentials and retrying request...")
			return true, false, nil
		}
	}

	recoveredWithPool, retried429, err := a.recoverWithCredentialPool(ctx, e.statusCode, retry.HasRetried429, classified.Reason,
		poolErrorContext(a, classified, e.errorContext), classified.BillingUnverified())
	if err != nil {
		return false, false, err
	}
	retry.HasRetried429 = retried429
	if recoveredWithPool {
		return true, true, nil
	}

	if classified.Reason == provider.ReasonImageTooLarge && !retry.ImageShrinkRetryAttempted {
		retry.ImageShrinkRetryAttempted = true
		maxDimension := 8000
		if d := imageErrorMaxDimension(apiError); d != nil && *d != 0 {
			maxDimension = *d
		}
		if tryShrinkImagePartsInMessages(s.APIMessages, maxDimension) {
			vlines(a, "📐 Image(s) exceeded provider size limit — shrank and retrying...")
			return true, false, nil
		}
		conversationLoopLog().Info("image-shrink recovery: no data-URL image parts found or shrink didn't reduce size; surfacing original error.")
	}

	if classified.Reason == provider.ReasonMultimodalToolContent && !retry.MultimodalToolContentRetryAttempted {
		retry.MultimodalToolContentRetryAttempted = true
		if a.tryStripImagePartsFromToolMessages(s.APIMessages, true) {
			vlines(a, "📐 Provider rejected list-type tool content — downgraded screenshots to text and retrying...")
			return true, false, nil
		}
		conversationLoopLog().Info("multimodal-tool-content recovery: no list-type tool messages with image parts found; surfacing original error.")
	}

	if classified.Reason == provider.ReasonReasoningMandatory && !retry.ReasoningMandatoryRetryAttempted {
		retry.ReasoningMandatoryRetryAttempted = true
		if sent := a.wireReasoningConfig; sent != nil && sent["enabled"] != false && sent["effort"] != nil && sent["effort"] != "none" {
			a.reasoningEffortRejected = true
			vlines(a, fmt.Sprintf("⚠️  %s rejects reasoning effort %s — using the route's default for this session, retrying...", a.model, fmt.Sprint(sent["effort"])))
			conversationLoopLog().Warn(fmt.Sprintf("%sReasoning-effort recovery: dropping reasoning config for %s", a.logPrefix, a.model))
			return true, false, nil
		}
		a.reasoningDisableRejected = true
		a.reasoningFloorRequired = provider.IsReasoningRequiredRejection(apiError.Error())
		func() {
			defer func() { _ = recover() }()
			provider.RefreshReasoningCapsAsync(ctx, a.provider)
		}()
		if a.reasoningFloorRequired {
			vlines(a, fmt.Sprintf("⚠️  %s cannot disable reasoning — using effort=%s for this session, retrying...", a.model, provider.ReasoningFloorEffort))
			conversationLoopLog().Warn(fmt.Sprintf("%sReasoning-disable recovery: stepping reasoning up to %s for %s", a.logPrefix, provider.ReasoningFloorEffort, a.model))
		} else {
			vlines(a, fmt.Sprintf("⚠️  %s rejects disabling reasoning — using the route's default for this session, retrying...", a.model))
			conversationLoopLog().Warn(fmt.Sprintf("%sReasoning-disable recovery: dropping reasoning disable for %s", a.logPrefix, a.model))
		}
		return true, false, nil
	}

	if classified.Reason == provider.ReasonImageCorrupt {
		if stripRequestImagesAndRetry(a, &s.APIMessages) {
			return true, false, nil
		}
		conversationLoopLog().Info("image-corrupt recovery: no image parts found to strip; surfacing original error.")
	}

	if classified.Reason == provider.ReasonOAuthLongContextBetaForbid && a.apiMode == "anthropic_messages" &&
		a.isAnthropicOAuth && !retry.OAuth1MBetaRetryAttempted {
		retry.OAuth1MBetaRetryAttempted = true
		if !a.oauth1MBetaDisabled {
			a.oauth1MBetaDisabled = true
			if c, ok := a.anthropicClient.(interface{ Close() error }); ok && c != nil {
				_ = c.Close()
			}
			if err := a.rebuildAnthropicClient(ctx); err != nil {
				return false, false, err
			}
			vlines(a, "🔕 OAuth subscription doesn't support the 1M-context beta — disabled for this session and retrying...")
			return true, false, nil
		}
	}

	refreshed, err := refreshCredentialsAfter401(ctx, a, apiError, retry, e.statusCode)
	if err != nil || refreshed {
		return refreshed, false, err
	}
	repaired, err := recoverFormatErrors(a, apiError, classified, retry, s.Messages, s.APIMessages)
	return repaired, false, err
}
