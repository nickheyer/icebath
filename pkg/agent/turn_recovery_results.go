package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Base failed turn result shared by the two terminal paths
func failedTurnResult(finalResponse string, messages []*state.Message, apiCallCount int, errText string) map[string]any {
	return map[string]any{
		"final_response": finalResponse, "messages": messages, "api_calls": apiCallCount,
		"completed": false, "failed": true, "error": errText,
	}
}

// Prepends delivered partial text to a terminal error body, reporting whether the partial is kept
func withDeliveredPartial(finalResponse, errorSummary, delivered string) (string, bool) {
	d := pystr.Strip(delivered)
	if d == "" || d == pystr.Strip(errorSummary) {
		return finalResponse, false
	}
	return d + "\n\n" + finalResponse, true
}

// Epoch seconds the provider says its limit lifts, false when the error names none
func limitResetEpoch(apiError error) (epoch float64, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			epoch, ok = 0, false
		}
	}()
	return provider.ParseAbsoluteTimestamp(provider.ExtractAPIErrorContext(apiError)["reset_at"])
}

// Reasons a reset time explains
var limitReasons = map[provider.FailoverReason]bool{
	provider.ReasonRateLimit: true, provider.ReasonUpstreamRateLimit: true, provider.ReasonBilling: true,
}

// Stamps the limit reset epoch and appends its sentence to the chat text of a limit failure
func stampLimitReset(result map[string]any, apiError error, classified *provider.Classified) {
	if !limitReasons[classified.Reason] {
		return
	}
	resetsAt, ok := limitResetEpoch(apiError)
	if !ok {
		return
	}
	result["failure_resets_at"] = resetsAt
	if line := LimitResetCopy(resetsAt, nil); line != "" {
		result["final_response"] = fmt.Sprintf("%v\n\n%s", result["final_response"], line)
	}
}

// Structured free tier failure block a client renders its card from
func stampFreeTier(result map[string]any, kind, message string) map[string]any {
	if kind == "" {
		kind = "refused"
	}
	result["free_tier"] = map[string]any{"kind": kind, "message": message}
	return result
}

// Free model outage sentence for a transport or server failure on the Nous free tier, empty elsewhere
func welcomeOutageCopy(a *AIAgent, baseURL string, classified *provider.Classified) string {
	if provider.OnFreeModel(a.provider, a.apiKey, baseURL) && (classified.Reason == provider.ReasonTimeout ||
		classified.Reason == provider.ReasonOverloaded || classified.Reason == provider.ReasonServerError) {
		return provider.FreeTierOutageCopy
	}
	return ""
}

// Prints the actionable guidance for a terminal auth or billing error
func printNonretryableAuthGuidance(ctx context.Context, a *AIAgent, classified *provider.Classified, statusCode *int, prov, baseURL, model string) {
	if pyTruthy(classified.Context["usage_window"]) {
		return
	}
	if classified.Reason == provider.ReasonBilling && printBillingOrEntitlementGuidance(ctx, a, "model access", prov, baseURL, model, classified.BillingUnverified()) {
		return
	}
	if prov == "nous" && printNousEntitlementGuidance(ctx, a, "Nous model access") {
		return
	}
	is401 := statusCode != nil && *statusCode == 401
	if (prov == "openai-codex" || prov == "xai-oauth" || prov == "nous") && is401 {
		switch prov {
		case "openai-codex":
			vlines(a,
				"   💡 Codex OAuth token was rejected (HTTP 401). Your token may have been",
				"      refreshed by another client (Codex CLI, VS Code) or another Icebath profile.",
				fmt.Sprintf("      Sign this profile in again: `%s`", provider.OAuthReloginCommand(prov)))
		case "xai-oauth":
			vlines(a,
				"   💡 xAI OAuth token was rejected (HTTP 401). To fix:",
				"      re-authenticate with xAI Grok OAuth (SuperGrok / Premium+) from `icebath model`.")
		default:
			vlines(a,
				"   💡 Nous Portal OAuth token was rejected (HTTP 401). Your token may be",
				"      expired, revoked, or your account may be out of credits. To fix:",
				"      1. Re-authenticate: icebath portal",
				"      2. Check your portal account: https://portal.nousresearch.com")
			if strings.HasSuffix(model, ":free") {
				vlines(a,
					fmt.Sprintf("      ⚠️  Note: `%s` looks like an OpenRouter slug (`:free` suffix).", model),
					"         Nous Portal won't recognize that model name. Either switch to a",
					fmt.Sprintf("         Nous catalog model, or run `/model openrouter:%s` to use OpenRouter.", model))
			}
		}
		return
	}
	if is401 && provider.PluginRefreshHookExists(prov) {
		vlines(a, fmt.Sprintf("   💡 %s sign-in was rejected (HTTP 401). Sign in again: `%s`", prov, provider.OAuthReloginCommand(prov)))
		return
	}
	vlines(a,
		"   💡 Your API key was rejected by the provider. Check:",
		"      • Is the key valid? Run: icebath setup",
		fmt.Sprintf("      • Does your account have access to %s?", model))
	if matches, _ := pyurl.BaseURLHostMatches(baseURL, "openrouter.ai"); matches {
		vlines(a, "      • Check credits: https://openrouter.ai/settings/credits")
	}
}

// Terminal result of a non retryable 4xx once fallback is exhausted, with its guidance printed and the session persisted
func nonretryableClientErrorResult(ctx context.Context, a *AIAgent, s *LoopState, e *apiErrorTurn, delivered string) (map[string]any, error) {
	classified, apiError := e.classified, e.apiError
	prov, baseURL, model := e.providerName, e.baseURL, e.model
	if s.APIKwargs != nil {
		a.dumpAPIRequestDebug(tools.WithEnvHome(ctx, a.env), s.APIKwargs, "non_retryable_client_error", apiError)
	}
	a.flushStatusBuffer()
	summary := a.summarizeAPIError(apiError)
	plabel := ProviderLabelFor(prov)
	label, labelled := provider.NonretryableLabel(classified.Reason)
	if !labelled {
		label = plabel + " rejected the request and retrying won't help"
	}
	a.emitDiagnosticStatus(fmt.Sprintf("❌ %s: %s", label, summary))
	if a.verboseLogging {
		vlines(a,
			fmt.Sprintf("   🔌 Provider: %s  Model: %s  (HTTP %s)", prov, model, pyStatus(e.statusCode)),
			"   🌐 Endpoint: "+baseURL)
	}
	welcomeHint := provider.WelcomeTierGuidance(classified, model, false, true)
	prefixSuggestion := missingVendorPrefixSuggestion(apiError, prov, model)
	if moved := provider.HostInstallHint(baseURL); moved != "" {
		vlines(a, "   💡 "+moved)
	}
	switch {
	case welcomeHint != "":
		vlines(a, "   💡 "+welcomeHint)
	case classified.IsAuth() || classified.Reason == provider.ReasonBilling:
		printNonretryableAuthGuidance(ctx, a, classified, e.statusCode, prov, baseURL, model)
	case classified.Reason == provider.ReasonModelNotFound:
		vlines(a, fmt.Sprintf("   💡 Model '%s' isn't available on %s. Pick another with /model.", model, plabel))
		if prefixSuggestion != "" {
			vlines(a, fmt.Sprintf("      Did you mean '%s'? It looks like the vendor prefix is missing.", prefixSuggestion))
		}
	case !labelled:
		vlines(a, fmt.Sprintf("   💡 Fix: pick another model (/model), or check `%s/logs/agent.log`.", home.Display(home.Dir(ctx))))
	}
	if classified.Reason == provider.ReasonUpstreamBlocked {
		vlines(a,
			"   💡 The endpoint's firewall/CDN blocked the request before it reached the model — your key",
			"      and model access are probably fine. Relays often reject the SDK's default User-Agent:",
			"      set `extra_headers: {User-Agent: IcebathAgent/1.0}` on the custom_providers entry,",
			"      or check the proxy/WAF rules and your network.")
	}
	if classified.Reason == provider.ReasonContentPolicyBlocked {
		vlines(a,
			"   💡 "+ContentPolicyNextSteps,
			"      To route future blocks to another provider automatically: icebath fallback add")
	}
	if classified.Reason == provider.ReasonSSLCertVerification {
		vlines(a,
			"   💡 Icebath couldn't verify the provider's security certificate. This fails the same",
			"      way on every retry — fix the environment, then try again:",
			"      • Corporate TLS-inspecting proxy? Ask your administrator to install",
			"        its root certificate in the operating system trust store.",
			"      • Missing/stale system CA store? Refresh the OS certificate store.",
			"        A provider-specific CA can also be configured with ssl_ca_cert.",
			"      • Self-signed local endpoint (llama.cpp, LM Studio, vLLM)? Use http://",
			"        for localhost, or add the server's cert to your trust store.")
	}
	conversationLoopLog().Error(fmt.Sprintf("%sNon-retryable client error: %s", a.logPrefix, apiError))
	if e.statusCode != nil && *e.statusCode == 400 && (s.ApproxTokens > 50000 || len(s.APIMessages) > 80) {
		vlines(a, "⚠️  Skipping session persistence for large failed session to prevent growth loop.")
	} else if err := a.persistSession(&s.Messages, s.ConversationHistory); err != nil {
		return nil, err
	}
	if classified.Reason == provider.ReasonContentPolicyBlocked {
		return contentPolicyBlockedResult(s.Messages, s.APICallCount, "⚠️ "+ContentPolicyCopy(plabel, summary), summary), nil
	}
	if classified.Reason == provider.ReasonBilling {
		return billingFailureResult(ctx, classified, summary, s.Messages, s.APICallCount, prov, baseURL, model, nil,
			usageWindowRefusal(ctx, a, classified, s.APIMessages)), nil
	}
	var finalResponse string
	if welcomeHint != "" {
		finalResponse = provider.WelcomeTierGuidance(classified, model, true, true)
	} else {
		copyText, err := NonretryableCopy(ctx, classified, prov, model, summary, prefixSuggestion)
		if err != nil {
			return nil, err
		}
		finalResponse = copyText
	}
	finalResponse, keepPartial := withDeliveredPartial(finalResponse, summary, delivered)
	result := failedTurnResult(finalResponse, s.Messages, s.APICallCount, summary)
	result["failure_reason"] = string(classified.Reason)
	result["failure_retryable"] = classified.Retryable
	if keepPartial {
		result["partial"] = true
	}
	stampLimitReset(result, apiError, classified)
	if welcomeHint != "" {
		if kind := provider.WelcomeSurfaceKind(classified); kind != "" {
			stampFreeTier(result, kind, provider.WelcomeTierGuidance(classified, model, true, false))
		}
	}
	return result, nil
}

// Error text markers of an SSE stream a proxy or CDN cut mid response
var streamDropMarkers = []string{
	"connection lost", "connection reset", "connection closed", "network connection",
	"network error", "terminated",
}

// Seconds the classifier's reset_at lies ahead of now, nil when it names none
func resetSecondsFrom(classified *provider.Classified) *float64 {
	var at float64
	switch v := classified.Context["reset_at"].(type) {
	case float64:
		at = v
	case int:
		at = float64(v)
	case int64:
		at = float64(v)
	}
	if at == 0 {
		return nil
	}
	secs := at - float64(time.Now().UnixNano())/1e9
	return &secs
}

// Terminal result once retries, transport recovery and fallback all failed, guidance printed and the session persisted
func maxRetriesExhaustedResult(ctx context.Context, a *AIAgent, s *LoopState, e *apiErrorTurn, attempts int, delivered string) (map[string]any, error) {
	classified, apiError := e.classified, e.apiError
	prov, baseURL, model := e.providerName, e.baseURL, e.model
	a.flushStatusBuffer()
	summary := a.summarizeAPIError(apiError)
	billingGuidance := ""
	isBilling := classified.Reason == provider.ReasonBilling
	switch {
	case isBilling:
		if classified.BillingUnverified() {
			a.emitDiagnosticStatus("❌ Provider reported usage/credit exhaustion " +
				fmt.Sprintf("(unverified — may be a content-filter rejection) — %s", summary))
		} else {
			a.emitDiagnosticStatus("❌ Billing or credits exhausted — " + summary)
		}
		if !pyTruthy(classified.Context["usage_window"]) {
			billingGuidance = billingOrEntitlementMessage(ctx, "model access", prov, baseURL, model, classified.BillingUnverified())
			printBillingOrEntitlementGuidance(ctx, a, "model access", prov, baseURL, model, classified.BillingUnverified())
		}
	case e.isRateLimited:
		word := "retries"
		if attempts == 1 {
			word = "attempt"
		}
		line := fmt.Sprintf("❌ Rate limited after %d %s — %s", attempts, word, summary)
		if reset := provider.ResetHint(apiError, time.Now()); reset != "" {
			line += fmt.Sprintf(" (resets in %s)", reset)
		}
		a.emitDiagnosticStatus(line)
	default:
		a.emitDiagnosticStatus(fmt.Sprintf("❌ API failed after %d retries — %s", attempts, summary))
	}
	vlines(a, "   💀 Final error: "+summary)
	welcomeHint := provider.WelcomeTierGuidance(classified, model, false, true)
	if welcomeHint != "" {
		vlines(a, "   💡 "+welcomeHint)
	}

	isStreamDrop := false
	if apiErrorStatus(apiError) == nil {
		for _, marker := range streamDropMarkers {
			if strings.Contains(e.errorMsg, marker) {
				isStreamDrop = true
				break
			}
		}
	}
	if isStreamDrop {
		vlines(a,
			"   💡 The provider's stream connection keeps dropping. This often happens "+
				"when the model tries to write a very large file in a single tool call.",
			"      Try asking the model to use execute_code with Python's open() for "+
				"large files, or to write the file in smaller sections.")
	}
	errorMsg := e.errorMsg
	isThinkingTimeout := IsThinkingTimeout(classified, model, &errorMsg)
	if isThinkingTimeout {
		vlines(a, "   💡 "+pystr.Strip(BuildThinkingTimeoutGuidance(ctx, prov, model, "")))
	}

	conversationLoopLog().Error(fmt.Sprintf("%sAPI call failed after %d retries. %s | provider=%s model=%s msgs=%d tokens=~%s",
		a.logPrefix, attempts, summary, prov, model, len(s.APIMessages), pystr.GroupThousands(int64(s.ApproxTokens))))
	if s.APIKwargs != nil {
		a.dumpAPIRequestDebug(tools.WithEnvHome(ctx, a.env), s.APIKwargs, "max_retries_exhausted", apiError)
	}
	if err := a.persistSession(&s.Messages, s.ConversationHistory); err != nil {
		return nil, err
	}
	var billingBlock map[string]any
	billingUnverified := false
	freeTierKind := ""
	var finalResponse string
	if isBilling {
		billingUnverified = classified.BillingUnverified()
		finalResponse = billingTerminalLabel(summary, billingUnverified)
		if billingGuidance != "" {
			finalResponse += "\n\n" + billingGuidance
		}
		billingBlock = billingBlockDict(prov, baseURL, model, billingGuidance, billingUnverified)
	} else {
		finalResponse = ExhaustedCopy(string(classified.Reason), ProviderLabelFor(prov), attempts, summary, resetSecondsFrom(classified), apiError)
		if welcomeHint != "" {
			finalResponse = provider.WelcomeTierGuidance(classified, model, true, true)
			freeTierKind = provider.WelcomeSurfaceKind(classified)
		} else if outage := welcomeOutageCopy(a, baseURL, classified); outage != "" {
			finalResponse, freeTierKind = outage, "outage"
		}
	}
	if isThinkingTimeout {
		finalResponse += "\n\n" + BuildThinkingTimeoutGuidance(ctx, prov, model, "")
	} else if isStreamDrop {
		finalResponse += "\n\nThe connection kept dropping while the model was writing — this often " +
			"happens when it writes a very large file in one go. Ask me to write the file in " +
			"smaller sections (or via execute_code with Python's open())."
	}
	result := failedTurnResult(finalResponse, s.Messages, s.APICallCount, summary)
	result["failure_reason"] = string(classified.Reason)
	result["failure_retryable"] = classified.Retryable
	result["billing_unverified"] = billingUnverified
	if billingBlock != nil {
		result["billing_block"] = billingBlock
	} else {
		result["billing_block"] = nil
	}
	finalResponse, keepPartial := withDeliveredPartial(finalResponse, summary, delivered)
	if keepPartial {
		result["final_response"] = finalResponse
		result["partial"] = true
	}
	stampLimitReset(result, apiError, classified)
	if freeTierKind != "" {
		message := finalResponse
		if welcomeHint != "" {
			message = provider.WelcomeTierGuidance(classified, model, true, false)
		}
		stampFreeTier(result, freeTierKind, message)
	}
	return result, nil
}
