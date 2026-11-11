package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Fewest transcript messages a manual compress works on
const manualCompressMinMessages = 4

// Session store the async session store wraps
func (r *GatewayRunner) slashStore() *gateway.SessionStore { return r.AsyncSessionStore().Store() }

// Transcript entries as message dicts
func transcriptMaps(history []any) []map[string]any {
	out := make([]map[string]any, 0, len(history))
	for _, item := range history {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// Whether the gateway multiplexes profiles
func (r *GatewayRunner) multiplexProfiles() bool {
	return r.config != nil && r.config.MultiplexProfiles != nil && *r.config.MultiplexProfiles
}

// Manual compress confirmation lines surfacing summariser and auxiliary model failures with forced redaction
func manualCompressionReplyLines(ctx context.Context, summary *agent.ManualCompressionFeedback, compressor agent.ContextEngine, focusTopic *string) []string {
	lines := []string{"🗜️ " + summary.Headline}
	if focusTopic != nil && *focusTopic != "" {
		lines = append(lines, i18n.T(ctx, "gateway.compress.focus_line", "", map[string]any{"topic": *focusTopic}))
	}
	lines = append(lines, summary.TokenLine)
	if summary.Note != nil && *summary.Note != "" {
		lines = append(lines, *summary.Note)
	}
	builtin, _ := compressor.(*agent.ContextCompressor)
	summaryErr := ""
	auxFailModel, auxFailErr := "", ""
	aborted := false
	if builtin != nil {
		if v := builtin.LastSummaryErrorValue(); v != nil && *v != "" {
			summaryErr = tools.RedactSensitiveTextWith(&tools.Env{Home: home.Dir(ctx)}, *v, tools.RedactOptions{Force: true})
		}
		auxFailModel, auxFailErr = builtin.LastAuxModelFailureModel, builtin.LastAuxModelFailureError
		aborted = builtin.LastCompressAborted()
	}
	unknown := func() string { return i18n.T(ctx, "gateway.shared.unknown_error", "", nil) }
	if aborted {
		if summaryErr == "" {
			summaryErr = unknown()
		}
		lines = append(lines, i18n.T(ctx, "gateway.compress.aborted", "", map[string]any{"error": summaryErr}))
	} else if auxFailModel != "" {
		if auxFailErr == "" {
			auxFailErr = unknown()
		}
		lines = append(lines, i18n.T(ctx, "gateway.compress.aux_failed", "", map[string]any{"model": auxFailModel, "error": auxFailErr}))
	}
	return lines
}

// What a manual compress would remove, with no agent and no writes
func compressPreviewReply(ctx context.Context, history []map[string]any, partial bool, keepLast int, focusTopic *string, aggNote string) string {
	var pvMsgs []map[string]any
	for _, m := range history {
		role := m["role"]
		if (role == "user" || role == "assistant") && pyrepr.Truthy(m["content"]) {
			pvMsgs = append(pvMsgs, map[string]any{"role": role, "content": m["content"]})
		}
	}
	report := provider.SummarizeCompressPreview(pvMsgs, partial, keepLast, focusTopic, agent.SessionCompressAgent{}.RequestTokens(ctx, pvMsgs, "", nil))
	reportLines, _ := report["lines"].([]string)
	lines := make([]string, 0, len(reportLines)+1)
	for _, line := range reportLines {
		lines = append(lines, "🗜️ "+line)
	}
	if aggNote != "" {
		lines = append(lines, aggNote)
	}
	return strings.Join(lines, "\n")
}

// Manual compress under the routed profile's runtime scope when the gateway multiplexes profiles
func (r *GatewayRunner) handleCompressCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	if !r.multiplexProfiles() {
		return r.handleCompressCommandInner(ctx, event)
	}
	profileHome, err := r.ResolveProfileHomeForSource(ctx, event.Source)
	if err != nil {
		return "", err
	}
	return r.handleCompressCommandInner(gateway.ProfileRuntimeScope(ctx, profileHome, true), event)
}

// Forces compaction of the live cached agent's app server thread and keeps it cached
func (r *GatewayRunner) compressCodexAppServerSession(ctx context.Context, sessionKey, sessionID string) (string, error) {
	cached := r.cachedAgentFor(sessionKey, true)
	if cached == nil || cached == agentPendingSentinel || cached.CodexSession() == nil {
		return i18n.T(ctx, "gateway.compress.codex_nothing", "", nil), nil
	}
	countBefore, _ := cached.ContextCompressionCount()
	taskID := sessionID
	if taskID == "" {
		taskID = "default"
	}
	_, err := r.runInExecutorWithContext(ctx, func(workerCtx context.Context) (any, error) {
		_, _, err := cached.ForceCompressContext(workerCtx, []map[string]any{}, "", taskID)
		return nil, err
	})
	if err != nil {
		return i18n.T(ctx, "gateway.compress.failed", "", map[string]any{"error": pyrepr.ExceptionMessage(err)}), nil
	}
	if countAfter, _ := cached.ContextCompressionCount(); countAfter > countBefore {
		return i18n.T(ctx, "gateway.compress.codex_compacted", "", nil), nil
	}
	return i18n.T(ctx, "gateway.compress.codex_incomplete", "", nil), nil
}

// Manually compresses the conversation, a focus argument telling the summariser what to keep
func (r *GatewayRunner) handleCompressCommandInner(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	source := event.Source
	entry, err := r.slashStore().GetOrCreateSession(ctx, source, false, true)
	if err != nil {
		return "", err
	}
	raw, err := r.slashStore().LoadTranscript(ctx, entry.SessionID)
	if err != nil {
		var readErr *gateway.TranscriptReadError
		if errors.As(err, &readErr) {
			return historyUnreadable(ctx), nil
		}
		return "", err
	}
	history := transcriptMaps(raw)
	if len(history) == 0 || len(history) < manualCompressMinMessages {
		return i18n.T(ctx, "gateway.compress.not_enough", "", nil), nil
	}
	request := agent.ParseCompressArgs(event.GetCommandArgs())
	aggNote := ""
	if request.Aggressive {
		aggNote = i18n.T(ctx, "gateway.compress.aggressive_unsupported", "", nil)
	}
	if request.Aggressive && !request.Preview {
		return aggNote, nil
	}
	if request.Preview {
		return compressPreviewReply(ctx, history, request.Partial, request.KeepLast, request.FocusTopic, aggNote), nil
	}
	reply, err := r.runManualCompression(ctx, source, entry, history, request)
	if err != nil {
		runLog().Warn(fmt.Sprintf("Manual compress failed: %s", pyrepr.ExceptionMessage(err)))
		return i18n.T(ctx, "gateway.compress.failed", "", map[string]any{"error": pyrepr.ExceptionMessage(err)}), nil
	}
	return reply, nil
}

// Builds a temporary agent, runs the shared compress core, persists and describes the outcome
func (r *GatewayRunner) runManualCompression(ctx context.Context, source *gateway.SessionSource, entry *gateway.SessionEntry, history []map[string]any, request agent.CompressRequest) (string, error) {
	sessionKey, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return "", err
	}
	platformKey := ""
	if source.Platform != "" {
		platformKey = platformConfigKey(source.Platform)
	}
	model, runtimeKwargs, err := r.resolveSessionAgentRuntime(ctx, source, sessionKey, nil, true)
	if err != nil {
		return "", err
	}
	if pystr.Lower(pyrepr.Str(pyrepr.Or(runtimeKwargs["api_mode"], ""))) == "codex_app_server" {
		return r.compressCodexAppServerSession(ctx, sessionKey, entry.SessionID)
	}
	if !pyrepr.Truthy(runtimeKwargs["api_key"]) {
		return i18n.T(ctx, "gateway.compress.no_provider", "", nil), nil
	}
	var msgs []map[string]any
	for _, m := range history {
		if role := m["role"]; role == "user" || role == "assistant" || role == "tool" {
			msgs = append(msgs, m)
		}
	}
	reasoning, err := r.resolveSessionReasoningConfig(ctx, source, "", model)
	if err != nil {
		return "", err
	}
	tmpAgent, err := r.buildManualCompressionAgent(ctx, entry.SessionID, model, runtimeKwargs, platformKey, sessionKey, reasoning)
	if err != nil {
		return "", err
	}
	var compressor agent.ContextEngine
	var summary *agent.ManualCompressionFeedback
	reply, done, err := func() (string, bool, error) {
		defer func() {
			agent.FinalizeContextEngineCompressionNotification(tmpAgent, false)
			r.evictCachedAgent(ctx, sessionKey, false, false)
			r.cleanupAgentResourcesOffLoop(context.WithoutCancel(ctx), tmpAgent, "manual compression", "")
		}()
		taskID := entry.SessionID
		if taskID == "" {
			taskID = "default"
		}
		system := ""
		value, err := r.runInExecutorWithContext(ctx, func(workerCtx context.Context) (any, error) {
			return agent.CompressNow(workerCtx, tmpAgent, msgs, request, agent.CompressNowOptions{SystemMessage: &system, TaskID: taskID, SkipWithoutWindow: true})
		})
		if err != nil {
			return "", false, err
		}
		result := value.(agent.CompressResult)
		if result.Status == "nothing_to_do" {
			return i18n.T(ctx, "gateway.compress.nothing_to_do", "", nil), true, nil
		}
		if result.Status != "compressed" {
			return strings.Join(agent.RenderCompressResult(result, ""), "\n"), true, nil
		}
		if err := r.persistManualCompression(ctx, tmpAgent, entry, source, result.AfterMessages); err != nil {
			return "", false, err
		}
		agent.FinalizeContextEngineCompressionNotification(tmpAgent, true)
		compressor = tmpAgent.ContextCompressor()
		summary = result.Summary
		return "", false, nil
	}()
	if err != nil {
		return "", err
	}
	if done {
		return reply, nil
	}
	if summary == nil {
		return "", errors.New("TypeError: 'NoneType' object is not subscriptable")
	}
	return strings.Join(manualCompressionReplyLines(ctx, summary, compressor, request.FocusTopic), "\n"), nil
}

// Throwaway agent performing a manual compress rewrite of the session with its live system prompt restored
func (r *GatewayRunner) buildManualCompressionAgent(ctx context.Context, sessionID, model string, runtimeKwargs map[string]any, platformKey, sessionKey string, reasoning map[string]any) (*agent.AIAgent, error) {
	asyncDB := r.sessionDB(ctx)
	var sessionRow *pyjson.Object
	if asyncDB != nil {
		row, err := state.AsyncCall(ctx, asyncDB, func(d *state.SessionDB) (*pyjson.Object, error) { return d.GetSession(ctx, sessionID) })
		if err != nil {
			runLog().Warn(fmt.Sprintf("Manual compression could not restore the system prompt for session %s: %s. "+
				"Preserving an empty prompt so the live turn rebuilds it with its configured providers.", sessionID, pyrepr.ExceptionMessage(err)),
				"exc_info", err)
		} else {
			sessionRow = row
		}
	}
	checkpointRequired := false
	if cfg, err := config.LoadContext(ctx, home.Dir(ctx), nil); err == nil && cfg != nil {
		if comp, ok := cfg["compression"].(map[string]any); ok {
			checkpointRequired = provider.IsTruthyValue(comp["checkpoint_required"])
		}
	}
	opts := agent.AIAgentOptions{Model: model, MaxIterations: 4, QuietMode: true, SkipMemory: !checkpointRequired,
		EnabledToolsets: []string{"memory"}, SessionID: sessionID, Platform: platformKey, GatewaySessionKey: sessionKey,
		ReasoningConfig: reasoning}
	if asyncDB != nil {
		opts.SessionDB = asyncDB.DB()
	}
	applyRuntimeKwargs(&opts, runtimeKwargs)
	tmpAgent, err := agent.NewAIAgent(ctx, opts)
	if err != nil {
		return nil, err
	}
	seedHygieneSystemPrompt(tmpAgent, sessionRow)
	tmpAgent.SetPlatform(gatewayHygienePlatform)
	tmpAgent.SetPrintFn(func(string) {})
	tmpAgent.SetEndSessionOnClose(false)
	return tmpAgent, nil
}

// Commits a manual compress result, a rotated child made durable before the entry is repointed
func (r *GatewayRunner) persistManualCompression(ctx context.Context, tmpAgent *agent.AIAgent, entry *gateway.SessionEntry, source *gateway.SessionSource, compressed []map[string]any) error {
	store := r.slashStore()
	newSessionID := tmpAgent.SessionID()
	if newSessionID != entry.SessionID {
		if !store.PersistRotatedCompressionChild(ctx, entry.SessionID, newSessionID, compressed) {
			return fmt.Errorf("RuntimeError: failed to persist compressed transcript for session %s", newSessionID)
		}
		entry.SessionID = newSessionID
		if err := store.Save(ctx); err != nil {
			return err
		}
		r.syncTelegramTopicBinding(ctx, source, entry, "compress-command")
	} else if !tmpAgent.LastCompactionInPlace() {
		runLog().Warn("Manual /compress: session rotation did not occur (session_id unchanged) and in-place " +
			"mode is off — preserving original transcript instead of overwriting it (#44794).")
	}
	zero := int64(0)
	return store.UpdateSession(ctx, entry.SessionKey, &zero, true)
}
