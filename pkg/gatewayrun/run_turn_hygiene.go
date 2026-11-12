package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Hygiene pre check outcome for one turn
type hygienePlan struct {
	needsCompress      bool
	approxTokens       int
	msgCount           int
	warnTokenThreshold int
}

// Resolved session hygiene configuration for one inbound turn, an empty provider, url or key standing for None
type hygieneSettings struct {
	model                  string
	thresholdPct           float64
	compressionEnabled     bool
	hardMsgLimit           int
	timeoutSeconds         float64
	totalCeilingSeconds    float64
	maxTurnHoldSeconds     float64
	failureCooldownSeconds float64
	configContextLength    *int
	provider               string
	baseURL                string
	apiKey                 string
	data                   map[string]any
}

// One detached hygiene compression attempt whose deferred flag the wait handlers set on raise paths
type hygieneAttempt struct {
	agent           *agent.AIAgent
	meta            map[string]any
	commitFence     *agent.CompressionCommitFence
	future          *gatewayFuture
	waitStarted     float64
	cleanupDeferred bool
	history         []map[string]any
	historyReplaced bool
}

// Idle or ceiling timeout of the hygiene wait, the bare TimeoutError of hermes
type hygieneWaitTimeout struct{}

func (hygieneWaitTimeout) Error() string { return "" }

// Python class name the wait timeout carries
func (hygieneWaitTimeout) PyTypeName() string { return "TimeoutError" }

// Applies the model and compression knobs of the gateway config, invalid values keeping the defaults
func hmwaHygieneReadConfig(hs *hygieneSettings, data map[string]any) {
	switch modelCfg := data["model"].(type) {
	case string:
		hs.model = modelCfg
	case map[string]any:
		if v := modelCfg["default"]; pyrepr.Truthy(v) {
			hs.model = pyrepr.Str(v)
		} else if v := modelCfg["model"]; pyrepr.Truthy(v) {
			hs.model = pyrepr.Str(v)
		}
		if raw := modelCfg["context_length"]; raw != nil {
			if n, err := pyint.Int64(raw); err == nil {
				value := int(n)
				hs.configContextLength = &value
			}
		}
		hs.provider, hs.baseURL = "", ""
		if v := modelCfg["provider"]; pyrepr.Truthy(v) {
			hs.provider = pyrepr.Str(v)
		}
		if v := modelCfg["base_url"]; pyrepr.Truthy(v) {
			hs.baseURL = pyrepr.Str(v)
		}
	}
	rawComp, present := data["compression"]
	if !present {
		rawComp = map[string]any{}
	}
	compCfg, ok := rawComp.(map[string]any)
	if !ok {
		return
	}
	enabled, present := compCfg["enabled"]
	if !present {
		enabled = true
	}
	switch strings.ToLower(pyrepr.Str(enabled)) {
	case "true", "1", "yes":
		hs.compressionEnabled = true
	default:
		hs.compressionEnabled = false
	}
	floatKnob := func(key string, current float64, allowZero bool) float64 {
		raw := compCfg[key]
		if raw == nil {
			return current
		}
		parsed, err := pyint.Float(raw)
		if err != nil {
			return current
		}
		if (allowZero && parsed >= 0) || (!allowZero && parsed > 0) {
			return parsed
		}
		return current
	}
	if raw := compCfg["hygiene_hard_message_limit"]; raw != nil {
		if n, err := pyint.Int64(raw); err == nil && n > 0 {
			hs.hardMsgLimit = int(n)
		}
	}
	hs.timeoutSeconds = floatKnob("hygiene_timeout_seconds", hs.timeoutSeconds, false)
	hs.totalCeilingSeconds = floatKnob("hygiene_total_ceiling_seconds", hs.totalCeilingSeconds, false)
	hs.totalCeilingSeconds = max(hs.totalCeilingSeconds, hs.timeoutSeconds)
	hs.maxTurnHoldSeconds = floatKnob("hygiene_max_turn_hold_seconds", hs.maxTurnHoldSeconds, false)
	hs.failureCooldownSeconds = floatKnob("hygiene_failure_cooldown_seconds", hs.failureCooldownSeconds, true)
}

// Model, provider, context length and hygiene knobs of the session, a failure keeping the defaults
func (r *GatewayRunner) hmwaHygieneSettings(ctx context.Context, source *gateway.SessionSource, sessionKey string) *hygieneSettings {
	hs := &hygieneSettings{model: "anthropic/claude-sonnet-4.6", thresholdPct: 0.85, compressionEnabled: true,
		hardMsgLimit: 5000, timeoutSeconds: 30.0, totalCeilingSeconds: 600.0, maxTurnHoldSeconds: 10.0,
		failureCooldownSeconds: 300.0, data: map[string]any{}}
	func() {
		defer func() { _ = recover() }()
		hs.data = gateway.LoadGatewayConfigMap(ctx, "")
		if len(hs.data) > 0 {
			hmwaHygieneReadConfig(hs, hs.data)
		}
		configuredModel, configuredProvider, configuredBaseURL := hs.model, hs.provider, hs.baseURL
		if model, runtime, err := r.resolveSessionAgentRuntime(ctx, source, sessionKey, hs.data, true); err == nil {
			hs.model = model
			if v := runtime["provider"]; pyrepr.Truthy(v) {
				hs.provider = pyrepr.Str(v)
			}
			if v := runtime["base_url"]; pyrepr.Truthy(v) {
				hs.baseURL = pyrepr.Str(v)
			}
			if v := runtime["api_key"]; pyrepr.Truthy(v) {
				hs.apiKey = pyrepr.Str(v)
			}
		}
		if hs.configContextLength != nil && provider.ShouldClearContextPin(configuredModel, hs.model, configuredBaseURL, hs.baseURL, configuredProvider, hs.provider) {
			hs.configContextLength = nil
		}
		if hs.configContextLength == nil && hs.baseURL != "" {
			if n := provider.CustomProviderContextLength(hs.model, hs.baseURL, provider.CompatibleCustomProviders(hs.data), nil); n != 0 {
				hs.configContextLength = &n
			}
		}
	}()
	return hs
}

// Whether hygiene compression fires this turn from the token and message thresholds, cooldown and lock
func (r *GatewayRunner) hmwaHygienePlan(ctx context.Context, hs *hygieneSettings, history []map[string]any, entry *gateway.SessionEntry, sessionKey string) hygienePlan {
	pinned := 0
	if hs.configContextLength != nil {
		pinned = *hs.configContextLength
	}
	contextLength := provider.GetModelContextLength(ctx, provider.ContextLengthRequest{Model: hs.model, BaseURL: hs.baseURL,
		APIKey: hs.apiKey, ConfigContextLength: pinned, Provider: hs.provider})
	compressThreshold := int(float64(contextLength) * hs.thresholdPct)
	warnThreshold := int(float64(contextLength) * 0.95)
	msgCount := len(history)
	costCtx := provider.BindImageTokenCost(ctx, hs.model, hs.baseURL)
	var approxTokens int
	var tokenSource string
	anchored, haveAnchor := 0, false
	if entry.LastPromptTokens <= 0 {
		if db := r.SessionDB(ctx); db != nil {
			anchored, haveAnchor = provider.PersistedAnchorTokens(costCtx, db, entry.SessionID, agent.RowsToMessages(history), true)
		}
	}
	switch {
	case entry.LastPromptTokens > 0:
		approxTokens, tokenSource = int(entry.LastPromptTokens), "actual"
	case haveAnchor:
		approxTokens, tokenSource = anchored, "anchored"
	default:
		approxTokens, tokenSource = agent.EstimateMessagesTokensRough(costCtx, history), "estimated"
	}
	needsCompress := approxTokens >= compressThreshold || msgCount >= hs.hardMsgLimit
	if needsCompress {
		if db := r.SessionDB(ctx); db != nil {
			if cooldown, err := db.GetCompressionFailureCooldown(entry.SessionID); err == nil && cooldown != nil {
				remaining, _ := pyint.Float(cooldown.GetOr("remaining_seconds", 0))
				if remaining > 0 {
					runLog().Info(fmt.Sprintf("Session hygiene: skipping compression for %s; previous failure cooldown active for %.1fs", entry.SessionID, remaining))
					needsCompress = false
				}
			}
		}
	}
	if needsCompress && r.sessionHasCompressionInFlight(ctx, sessionKey) {
		runLog().Info(fmt.Sprintf("Session hygiene: skipping compression for %s; another compression is already in flight", entry.SessionID))
		needsCompress = false
	}
	if needsCompress {
		runLog().Info(fmt.Sprintf("Session hygiene: %d messages, ~%s tokens (%s) — auto-compressing (threshold: %d%% of %s = %s tokens)",
			msgCount, thousands(approxTokens), tokenSource, int(hs.thresholdPct*100), thousands(contextLength), thousands(compressThreshold)))
	}
	return hygienePlan{needsCompress: needsCompress, approxTokens: approxTokens, msgCount: msgCount, warnTokenThreshold: warnThreshold}
}

// Integer with comma thousands separators, the Python format spec of a comma
func thousands(n int) string {
	text, _ := pyrepr.Format(n, ",")
	return text
}

// Progress aware wait for the detached compressor, the turn hold or idle and ceiling timeout raised for the handlers
func (r *GatewayRunner) hmwaHygieneWaitForSummary(ctx context.Context, attempt *hygieneAttempt, hs *hygieneSettings, entry *gateway.SessionEntry) ([]map[string]any, error) {
	fence := attempt.commitFence
	for {
		if fence.IsCancelled() {
			return nil, hygieneWaitTimeout{}
		}
		waited := pythread.Monotonic() - attempt.waitStarted
		ceiling := agent.EffectiveCeiling(fence, hs.totalCeilingSeconds, attempt.waitStarted)
		slice := min(max(hs.timeoutSeconds-fence.SecondsSinceProgress(), 0.005), max(ceiling-waited, 0.005))
		turnHoldRemaining := hs.maxTurnHoldSeconds - (pythread.Monotonic() - attempt.waitStarted)
		if turnHoldRemaining <= 0 {
			slice = 0.005
		} else {
			slice = min(slice, max(turnHoldRemaining, 0.005))
		}
		idleLeft := max(hs.timeoutSeconds-fence.SecondsSinceProgress(), 0.005)
		slice = min(slice, 0.25)
		waitCtx, cancel := context.WithTimeout(ctx, time.Duration(slice*float64(time.Second)))
		result, err := attempt.future.wait(waitCtx)
		cancel()
		if err == nil || attempt.future.isDone() {
			if attempt.future.isDone() {
				result, err = attempt.future.wait(context.WithoutCancel(ctx))
			}
			if err != nil {
				return nil, err
			}
			compressed, _ := result.([]map[string]any)
			return compressed, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if fence.IsCancelled() {
			return nil, hygieneWaitTimeout{}
		}
		waited = pythread.Monotonic() - attempt.waitStarted
		ceiling = agent.EffectiveCeiling(fence, hs.totalCeilingSeconds, attempt.waitStarted)
		idle := fence.SecondsSinceProgress()
		if waited >= hs.maxTurnHoldSeconds {
			runLog().Info(fmt.Sprintf("Session hygiene compression for session %s exceeded the turn-hold budget (%.1fs >= %.1fs) — abandoning inline wait, proceeding without compression this turn",
				entry.SessionID, waited, hs.maxTurnHoldSeconds))
			return nil, &hygieneTurnHoldExceeded{msg: fmt.Sprintf("turn-hold budget %.1fs elapsed after %.1fs", hs.maxTurnHoldSeconds, waited)}
		}
		if hygieneWaitShouldExtend(idle, hs.timeoutSeconds, waited, ceiling, fence.IsCancelled()) {
			if slice >= idleLeft-1e-9 {
				runLog().Info(fmt.Sprintf("Session hygiene compression for session %s still streaming after %.0fs (last progress %.1fs ago) — extending wait (ceiling %.0fs)",
					entry.SessionID, waited, idle, ceiling))
			}
			continue
		}
		return nil, hygieneWaitTimeout{}
	}
}

// Cancels the worker at the commit fence and defers its cleanup, or adopts the transcript of a worker already committing
func (r *GatewayRunner) hmwaHygieneCancelOrAdopt(ctx context.Context, attempt *hygieneAttempt, cleanupContext string) ([]map[string]any, bool, error) {
	fence := attempt.commitFence
	for !fence.CommitInFlight() {
		cancelled := fence.TryCancelBeforeCommit()
		if cancelled == nil {
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case <-time.After(25 * time.Millisecond):
			}
			continue
		}
		if *cancelled {
			fence.ReleaseCancelledCompressionLock()
			r.hmwaHygieneDeferCleanup(ctx, attempt, cleanupContext)
			return nil, false, nil
		}
		break
	}
	result, err := attempt.future.wait(ctx)
	if err != nil {
		return nil, false, err
	}
	compressed, _ := result.([]map[string]any)
	return compressed, true, nil
}

// Hands the agent cleanup to the still running worker future and marks it deferred
func (r *GatewayRunner) hmwaHygieneDeferCleanup(ctx context.Context, attempt *hygieneAttempt, cleanupContext string) {
	r.deferAgentCleanupUntilFutureDone(ctx, attempt.future, attempt.agent, cleanupContext)
	attempt.cleanupDeferred = true
}

// Warning on the hygiene thread through the delivery adapter, a failed send only logged
func (r *GatewayRunner) hmwaHygieneNotify(ctx context.Context, source *gateway.SessionSource, meta map[string]any, message, what string) {
	err := func() (err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%s", pyrepr.Str(rec))
			}
		}()
		adapter, err := r.deliveryAdapterFor(ctx, source)
		if err != nil {
			return err
		}
		if adapter != nil && source.ChatID != "" {
			_, err = adapter.EmitWarning(ctx, source.ChatID, message, "", meta, source.Platform)
		}
		return err
	}()
	if err != nil {
		runLog().Warn(fmt.Sprintf("Failed to deliver %s to user: %s", what, pyrepr.ExceptionMessage(err)))
	}
}

// Escalates the failure streak and persists the cooldown when cooldowns are enabled
func (r *GatewayRunner) hmwaHygieneRecordFailureCooldown(ctx context.Context, hs *hygieneSettings, sessionKey, sessionID string, reason *string) {
	if hs.failureCooldownSeconds < 0 {
		return
	}
	recordHygieneCooldown(ctx, r, sessionID, hygieneCooldownForFailure(ctx, r, sessionKey, hs.failureCooldownSeconds), reason)
}

// Keeps or cancels the worker commit admission after the turn hold budget, notifies and re raises unless already committing
func (r *GatewayRunner) hmwaHygieneOnTurnHold(ctx context.Context, attempt *hygieneAttempt, holdErr error, entry *gateway.SessionEntry, sessionKey string, source *gateway.SessionSource) ([]map[string]any, error) {
	fence := attempt.commitFence
	logSuffix := ""
	if fence.CommitWatermarkFenced() && !fence.IsCancelled() {
		r.hmwaHygieneDeferCleanup(ctx, attempt, "session hygiene turn-hold")
		sid, skey, hygAgent := entry.SessionID, sessionKey, attempt.agent
		bg := context.WithoutCancel(ctx)
		attempt.future.addDoneCallback(func(done *gatewayFuture) {
			_, err := done.wait(bg)
			committed := err == nil && (hygAgent.LastCompactionInPlace() || hygAgent.SessionID() != sid)
			if committed {
				runLog().Info(fmt.Sprintf("Session hygiene compression for session %s finished after the turn-hold was released — summary adopted at the watermark-fenced commit boundary (#97963)", sid))
				resetHygieneFailureStreak(bg, r, skey)
				return
			}
			reason := "hygiene compression deferred: turn-hold budget expired and the detached attempt did not commit"
			recordHygieneCooldown(bg, r, sid, hygieneTurnholdRetrySecond, &reason)
		})
		logSuffix = " — the watermark-fenced worker keeps its commit admission and the summary will be adopted when it finishes"
	} else {
		adopted, ok, err := r.hmwaHygieneCancelOrAdopt(ctx, attempt, "session hygiene turn-hold")
		if err != nil {
			return nil, err
		}
		if ok {
			return adopted, nil
		}
		reason := "hygiene compression deferred: turn-hold budget expired while the summary was still streaming"
		recordHygieneCooldown(ctx, r, entry.SessionID, hygieneTurnholdRetrySecond, &reason)
	}
	stampHygieneCompressionProvenance(attempt.agent, "session hygiene compression turn-hold",
		string(state.ActivityProvenanceAgentCompressionTurnhold), "hygiene compression turn-hold activity stamp failed")
	runLog().Info(fmt.Sprintf("Session hygiene compression for session %s exceeded turn-hold budget (%.1fs); proceeding without compression this turn%s",
		entry.SessionID, pythread.Monotonic()-attempt.waitStarted, logSuffix))
	r.hmwaHygieneNotify(ctx, source, attempt.meta, i18n.T(ctx, "gateway.compress.turnhold_deferred", "", nil), "compression-turnhold notice")
	return nil, holdErr
}

// Cancels at the commit fence after a timeout, records the cooldown, warns and re raises unless already committing
func (r *GatewayRunner) hmwaHygieneOnTimeout(ctx context.Context, attempt *hygieneAttempt, hs *hygieneSettings, timeoutErr error, entry *gateway.SessionEntry, sessionKey string, source *gateway.SessionSource) ([]map[string]any, error) {
	fence := attempt.commitFence
	totalExhausted := fence.DeadlineExceeded()
	if totalExhausted {
		fence.RetainCompressionLockUntilWorkerDone()
	}
	fenceCancelled := fence.IsCancelled()
	adopted, ok, err := r.hmwaHygieneCancelOrAdopt(ctx, attempt, "session hygiene timeout")
	if err != nil {
		return nil, err
	}
	if ok {
		return adopted, nil
	}
	reason := "session hygiene compression timed out with no output from the summary model"
	switch {
	case fenceCancelled:
		reason = "session hygiene compression cancelled at commit fence"
	case totalExhausted:
		reason = "session hygiene compression total ceiling exhausted"
	}
	r.hmwaHygieneRecordFailureCooldown(ctx, hs, sessionKey, entry.SessionID, &reason)
	desc := "session hygiene compression timed out"
	if fenceCancelled {
		desc = "session hygiene compression cancelled at commit fence"
	}
	stampHygieneCompressionProvenance(attempt.agent, desc, string(state.ActivityProvenanceAgentCompressionTimeout), "hygiene compression timeout activity stamp failed")
	if fenceCancelled {
		runLog().Warn(fmt.Sprintf("Session hygiene compression for session %s was cancelled at the commit fence; continuing without compression", entry.SessionID))
		return nil, timeoutErr
	}
	elapsed := pythread.Monotonic() - attempt.waitStarted
	if totalExhausted {
		runLog().Warn(fmt.Sprintf("Session hygiene compression for session %s reached its total ceiling after %.1fs (progress observed=%s); continuing without compression",
			entry.SessionID, elapsed, pyrepr.Str(fence.ProgressObserved())))
	} else {
		runLog().Warn(fmt.Sprintf("Session hygiene compression for session %s made no progress for %.1fs (total wait %.1fs, ceiling %.1fs); continuing without compression",
			entry.SessionID, fence.SecondsSinceProgress(), elapsed, hs.totalCeilingSeconds))
	}
	r.hmwaHygieneNotify(ctx, source, attempt.meta, hygieneCompressionTimeoutMessage(ctx, totalExhausted), "compression-timeout warning")
	return nil, timeoutErr
}

// Revokes commit admission before the host unwinds, defers cleanup and records a cooldown
func (r *GatewayRunner) hmwaHygieneOnUnwind(ctx context.Context, attempt *hygieneAttempt, hs *hygieneSettings, entry *gateway.SessionEntry, sessionKey string) {
	attempt.commitFence.RevokeCommitAdmission()
	if !attempt.cleanupDeferred {
		r.hmwaHygieneDeferCleanup(ctx, attempt, "session hygiene unwind")
	}
	if hs.failureCooldownSeconds >= 0 {
		bg := context.WithoutCancel(ctx)
		reason := "session hygiene compression cancelled at commit fence"
		recordHygieneCooldown(bg, r, entry.SessionID, hygieneCooldownForFailure(bg, r, sessionKey, hs.failureCooldownSeconds), &reason)
	}
}

// Adopts a finished compression by rotation or in place and publishes the transcript to continue with
func (r *GatewayRunner) hmwaHygieneAdoptTranscript(ctx context.Context, attempt *hygieneAttempt, compressed, history []map[string]any, plan hygienePlan, entry *gateway.SessionEntry, source *gateway.SessionSource, quickKey string, runGeneration int) (bool, bool, int, int) {
	hygAgent := attempt.agent
	newSID := hygAgent.SessionID()
	rotated := newSID != entry.SessionID
	inPlace := hygAgent.LastCompactionInPlace()
	inToks := agent.EstimateMessagesTokensRough(ctx, history)
	outToks := agent.EstimateMessagesTokensRough(ctx, compressed)
	if rotated && outToks > inToks {
		runLog().Warn(fmt.Sprintf("Gateway hygiene compression for session %s would grow transcript (~%s -> ~%s tokens); keeping the original transcript unchanged",
			entry.SessionID, thousands(inToks), thousands(outToks)))
		rotated = false
		compressed = history
	}
	if rotated {
		if !r.AsyncSessionStore().Store().PersistRotatedCompressionChild(ctx, entry.SessionID, newSID, compressed) {
			runLog().Error(fmt.Sprintf("Session hygiene: failed to persist compressed transcript for rotated session %s → %s; keeping the live entry on the original session so the conversation is not dropped",
				entry.SessionID, newSID))
			rotated, inPlace = false, false
		} else {
			entry.SessionID = newSID
			r.rebindTurnLease(quickKey, runGeneration, newSID)
			if err := r.AsyncSessionStore().Store().Save(ctx); err != nil {
				runLog().Warn(fmt.Sprintf("Session hygiene: session index save failed: %s", pyrepr.ExceptionMessage(err)))
			}
			r.syncTelegramTopicBinding(ctx, source, entry, "hygiene-compression")
		}
	}
	var newCount, newTokens int
	if rotated || inPlace {
		entry.LastPromptTokens = 0
		attempt.history, attempt.historyReplaced = compressed, !isSameRowList(compressed, history)
		newCount = len(compressed)
		newTokens = agent.EstimateMessagesTokensRough(ctx, compressed)
	} else {
		newCount, newTokens = plan.msgCount, plan.approxTokens
		runLog().Warn(fmt.Sprintf("Gateway hygiene compression for session %s did not rotate or compact in place (%s) — preserving the original transcript instead of overwriting it with the summary (#21301).",
			entry.SessionID, hygieneNoCommitReason(hygAgent)))
	}
	runLog().Info(fmt.Sprintf("Session hygiene: compressed %d → %d msgs, ~%s → ~%s tokens", plan.msgCount, newCount, thousands(plan.approxTokens), thousands(newTokens)))
	if newTokens >= plan.warnTokenThreshold {
		runLog().Warn(fmt.Sprintf("Session hygiene: still ~%s tokens after compression", thousands(newTokens)))
	}
	return rotated, inPlace, newCount, newTokens
}

// Adopts the finished compression, records streak or cooldown and warns the user on an abort
func (r *GatewayRunner) hmwaHygieneApplyResult(ctx context.Context, attempt *hygieneAttempt, hs *hygieneSettings, compressed, history []map[string]any, plan hygienePlan, entry *gateway.SessionEntry, sessionKey string, source *gateway.SessionSource, quickKey string, runGeneration int) {
	rotated, inPlace, newCount, newTokens := r.hmwaHygieneAdoptTranscript(ctx, attempt, compressed, history, plan, entry, source, quickKey, runGeneration)
	comp, _ := attempt.agent.ContextCompressor().(*agent.ContextCompressor)
	aborted := comp != nil && comp.LastCompressAborted()
	fenceCancelled := attempt.commitFence.IsCancelled() && !rotated && !inPlace
	if fenceCancelled {
		aborted = true
	}
	if !aborted && hygieneCompactionRecovered(aborted, rotated, inPlace, plan.msgCount, newCount, plan.approxTokens, newTokens) {
		resetHygieneFailureStreak(ctx, r, sessionKey)
	}
	if aborted {
		var reason *string
		if fenceCancelled {
			text := "session hygiene compression cancelled at commit fence"
			reason = &text
		} else if comp != nil {
			reason = comp.LastSummaryErrorValue()
		}
		r.hmwaHygieneRecordFailureCooldown(ctx, hs, sessionKey, entry.SessionID, reason)
		stampHygieneCompressionProvenance(attempt.agent, "session hygiene compression aborted",
			string(state.ActivityProvenanceAgentCompressionCooldown), "hygiene compression abort activity stamp failed")
		if !fenceCancelled {
			summaryErr := ""
			if comp != nil {
				summaryErr = comp.LastSummaryError()
			}
			if summaryErr == "" {
				summaryErr = i18n.T(ctx, "gateway.shared.unknown_error", "", nil)
			}
			redacted := tools.RedactSensitiveTextWith(&tools.Env{Home: home.Dir(ctx)}, summaryErr, tools.RedactOptions{Force: true})
			runLog().Warn("Session hygiene compression aborted: " + redacted)
			r.hmwaHygieneNotify(ctx, source, attempt.meta, i18n.T(ctx, "gateway.compress.hygiene_failed", "", nil), "compression-failure warning")
		}
		return
	}
	if comp != nil && comp.LastAuxModelFailureModel != "" {
		auxErr := comp.LastAuxModelFailureError
		if auxErr == "" {
			auxErr = i18n.T(ctx, "gateway.shared.unknown_error", "", nil)
		}
		r.hmwaHygieneNotify(ctx, source, attempt.meta,
			i18n.T(ctx, "gateway.compress.aux_failed", "", map[string]any{"model": comp.LastAuxModelFailureModel, "error": auxErr}), "aux-model-fallback notice")
	}
}

// Compacts the codex app server thread of the cached agent instead of the transcript mirror and keeps it cached
func (r *GatewayRunner) hmwaHygieneCodexCompaction(ctx context.Context, hs *hygieneSettings, plan hygienePlan, history []map[string]any, entry *gateway.SessionEntry, sessionKey string) error {
	autoMode := "native"
	if compCfg, ok := hs.data["compression"].(map[string]any); ok {
		raw, present := compCfg["codex_app_server_auto"]
		if !present {
			raw = "native"
		}
		autoMode = "native"
		if pyrepr.Truthy(raw) {
			autoMode = pyrepr.Str(raw)
		}
	}
	outcome, err := runCodexHygieneCompaction(ctx, r, sessionKey, entry.SessionID, autoMode, history, plan.approxTokens, hs.totalCeilingSeconds, hs.failureCooldownSeconds)
	if err != nil {
		return err
	}
	runLog().Info(fmt.Sprintf("Session hygiene (codex app-server): %s (session=%s, mode=%s, ~%s tokens)", outcome, entry.SessionID, autoMode, thousands(plan.approxTokens)))
	return nil
}

// Detached hygiene agent seeded with the live session's system prompt and its state database
func (r *GatewayRunner) hmwaHygieneBuildAgent(ctx context.Context, model string, runtime map[string]any, entry *gateway.SessionEntry) (*agent.AIAgent, *state.SessionDB, error) {
	db := r.SessionDB(ctx)
	var sessionRow *pyjson.Object
	err := func() (err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%s", pyrepr.Str(rec))
			}
		}()
		if db == nil {
			return errors.New("'NoneType' object has no attribute 'get_session'")
		}
		sessionRow, err = db.GetSession(ctx, entry.SessionID)
		return err
	}()
	if err != nil {
		sessionRow = nil
		runLog().Warn(fmt.Sprintf("Session hygiene could not restore the system prompt for session %s: %s. Preserving an empty prompt so the live turn rebuilds it with its configured providers.",
			entry.SessionID, pyrepr.ExceptionMessage(err)))
	}
	checkpointRequired := false
	if cfg, err := config.LoadContext(ctx, home.Dir(ctx), nil); err == nil {
		if comp, ok := cfg["compression"].(map[string]any); ok {
			checkpointRequired = provider.IsTruthyValue(comp["checkpoint_required"])
		}
	}
	opts := agent.AIAgentOptions{Model: model, MaxIterations: 4, QuietMode: true, SkipMemory: !checkpointRequired,
		EnabledToolsets: []string{"memory"}, SessionID: entry.SessionID, SessionDB: db}
	applyRuntimeKwargs(&opts, runtime)
	hygAgent, err := newAIAgent(ctx, opts)
	if err != nil {
		return nil, nil, err
	}
	seedHygieneSystemPrompt(hygAgent, sessionRow)
	hygAgent.SetPlatform(gatewayHygienePlatform)
	return hygAgent, db, nil
}

// One detached hygiene compression end to end, the transcript to continue with published on the attempt
func (r *GatewayRunner) hmwaHygieneDetachedAttempt(ctx context.Context, attempt *hygieneAttempt, hs *hygieneSettings, plan hygienePlan, history, hygMsgs []map[string]any, model string, runtime map[string]any,
	source *gateway.SessionSource, entry *gateway.SessionEntry, sessionKey, quickKey string, runGeneration int) (err error) {
	hygAgent, hygDB, err := r.hmwaHygieneBuildAgent(ctx, model, runtime, entry)
	if err != nil {
		return err
	}
	attempt.agent = hygAgent
	defer func() {
		r.evictCachedAgent(ctx, sessionKey, false, false)
		if !attempt.cleanupDeferred {
			r.cleanupAgentResourcesOffLoop(context.WithoutCancel(ctx), hygAgent, "session hygiene", "")
		}
	}()
	hygAgent.SetCompressionInPlace(true)
	if comp, ok := hygAgent.ContextCompressor().(*agent.ContextCompressor); ok && comp != nil {
		_ = comp.BindSessionState(hygDB, entry.SessionID)
	}
	hygAgent.SetEndSessionOnClose(false)
	hygAgent.SetPrintFn(func(string) {})
	fence, err := agent.NewCompressionCommitFenceWithCeiling(hs.totalCeilingSeconds)
	if err != nil {
		return err
	}
	attempt.commitFence = fence
	taskID := entry.SessionID
	if taskID == "" {
		taskID = "default"
	}
	attempt.future = runInDefaultExecutor(ctx, func(workerCtx context.Context) (any, error) {
		return hygAgent.CompressContextUnderFence(workerCtx, hygMsgs, plan.approxTokens, fence, taskID, "gateway_hygiene")
	})
	r.trackDeferredAgentWorker(attempt.future, hygAgent)
	attempt.waitStarted = pythread.Monotonic()
	compressed, waitErr := r.hmwaHygieneWaitForSummary(ctx, attempt, hs, entry)
	if waitErr != nil {
		var hold *hygieneTurnHoldExceeded
		var timeout hygieneWaitTimeout
		switch {
		case errors.As(waitErr, &hold):
			compressed, waitErr = r.hmwaHygieneOnTurnHold(ctx, attempt, waitErr, entry, sessionKey, source)
		case errors.As(waitErr, &timeout):
			compressed, waitErr = r.hmwaHygieneOnTimeout(ctx, attempt, hs, waitErr, entry, sessionKey, source)
		default:
			r.hmwaHygieneOnUnwind(ctx, attempt, hs, entry, sessionKey)
			return waitErr
		}
		if waitErr != nil {
			return waitErr
		}
	}
	r.hmwaHygieneApplyResult(ctx, attempt, hs, compressed, history, plan, entry, sessionKey, source, quickKey, runGeneration)
	return nil
}

// Compresses a pathologically large transcript before the agent starts, the model payload bounded when hygiene does not land
func (r *GatewayRunner) hmwaRunSessionHygiene(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, entry *gateway.SessionEntry, sessionKey string, history []map[string]any, quickKey string, runGeneration int) ([]map[string]any, error) {
	if len(history) < 4 {
		return history, nil
	}
	hs := r.hmwaHygieneSettings(ctx, source, sessionKey)
	if !hs.compressionEnabled {
		return boundHygienePayload(history, hs, entry), nil
	}
	plan := r.hmwaHygienePlan(ctx, hs, history, entry, sessionKey)
	if !plan.needsCompress {
		return boundHygienePayload(history, hs, entry), nil
	}
	attempt := &hygieneAttempt{meta: r.eventThreadMetadata(event, source), history: history}
	err := func() error {
		model, runtime, err := r.resolveSessionAgentRuntime(ctx, source, sessionKey, hs.data, true)
		if err != nil {
			return err
		}
		if strings.ToLower(noneText(runtime["api_mode"])) == "codex_app_server" {
			return r.hmwaHygieneCodexCompaction(ctx, hs, plan, history, entry, sessionKey)
		}
		if !pyrepr.Truthy(runtime["api_key"]) {
			return nil
		}
		hygMsgs := make([]map[string]any, 0, len(history))
		for _, m := range history {
			switch m["role"] {
			case "user", "assistant", "tool":
				hygMsgs = append(hygMsgs, m)
			}
		}
		if len(hygMsgs) < 4 {
			return nil
		}
		return r.hmwaHygieneDetachedAttempt(ctx, attempt, hs, plan, history, hygMsgs, model, runtime, source, entry, sessionKey, quickKey, runGeneration)
	}()
	var hold *hygieneTurnHoldExceeded
	switch {
	case err == nil, errors.As(err, &hold):
	case ctx.Err() != nil && errors.Is(err, ctx.Err()):
		return nil, err
	default:
		runLog().Warn("Session hygiene auto-compress failed: " + pyrepr.ExceptionMessage(err))
	}
	if !attempt.historyReplaced {
		return boundHygienePayload(history, hs, entry), nil
	}
	return attempt.history, nil
}

// Model payload bounded to the hard message limit with one log line when the cut is real
func boundHygienePayload(history []map[string]any, hs *hygieneSettings, entry *gateway.SessionEntry) []map[string]any {
	bounded := boundModelInputWithoutHygiene(history, hs.hardMsgLimit)
	if len(bounded) != len(history) {
		runLog().Info(fmt.Sprintf("Session hygiene did not land for %s: bounding the model payload to %d of %d messages (hard limit %d) — the stored transcript is unchanged",
			entry.SessionID, len(bounded), len(history), hs.hardMsgLimit))
	}
	return bounded
}

// Whether two row slices are the same list object, the identity check of hermes
func isSameRowList(a, b []map[string]any) bool {
	return len(a) == len(b) && len(a) > 0 && &a[0] == &b[0]
}
