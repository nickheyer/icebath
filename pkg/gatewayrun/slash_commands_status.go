package gatewayrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Rows per agents section before the overflow line
const agentsListCap = 12

// Display format of status timestamps
const statusStampFormat = "%Y-%m-%d %H:%M"

// Stripped string value, empty for anything that is not a non blank string
func cleanStr(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return pystr.Strip(s)
}

// Integer coercion of a value, zero when it does not convert
func intValue(v any) int {
	if v == nil {
		return 0
	}
	if s, ok := v.(string); ok {
		n, ok := pystr.IntText(pystr.Strip(s))
		if !ok || !n.IsInt64() {
			return 0
		}
		return int(n.Int64())
	}
	n, err := pyint.Int64(v)
	if err != nil {
		if f, ok := v.(float64); ok && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return int(f)
		}
		return 0
	}
	return int(n)
}

// Counter read from the agent, zero when there is none
func agentCount(a *agent.AIAgent, read func(*agent.AIAgent) int) int {
	if a == nil {
		return 0
	}
	return read(a)
}

// Integer with thousands separators
func fmtThousands(n int) string { return pystr.GroupThousands(int64(n)) }

// Result of a call that may fail, the fallback when it does
func quiet[T any](call func() (T, error), fallback T) (out T) {
	defer func() {
		if recover() != nil {
			out = fallback
		}
	}()
	value, err := call()
	if err != nil {
		return fallback
	}
	return value
}

// Result of a call that reports no error, the fallback when it breaks
func quietSync[T any](call func() T, fallback T) (out T) {
	defer func() {
		if recover() != nil {
			out = fallback
		}
	}()
	return call()
}

// Occupancy percentage clamped to one hundred, zero for an unknown window
func occupancyPct(used, total int) float64 {
	if total == 0 {
		return 0
	}
	return math.Min(100, float64(used)/float64(total)*100)
}

// Text cut to limit characters with an ellipsis
func clipText(text string, limit int) string {
	if pystr.Len(text) > limit {
		return pystr.Slice(text, 0, limit-3) + "..."
	}
	return text
}

// Python format of a float with no decimals
func fmt0(f float64) string {
	s, _ := pyrepr.Format(f, ".0f")
	return s
}

// Approximate tokens and count of the user and assistant messages with content
func transcriptEstimate(ctx context.Context, history []map[string]any) (int, int) {
	var msgs []map[string]any
	for _, m := range history {
		if role := m["role"]; (role == "user" || role == "assistant") && pyrepr.Truthy(m["content"]) {
			msgs = append(msgs, m)
		}
	}
	return agent.EstimateMessagesTokensRough(ctx, msgs), len(msgs)
}

// Configured model provider of the gateway config, empty when unset
func configuredProvider(ctx context.Context) string {
	modelCfg, _ := gateway.LoadGatewayConfigMap(ctx, "")["model"].(map[string]any)
	if modelCfg == nil {
		return ""
	}
	return cleanStr(modelCfg["provider"])
}

// Context engine of an agent, nil without one
func agentContextEngine(a *agent.AIAgent) agent.ContextEngine {
	if a == nil || a == agentPendingSentinel {
		return nil
	}
	return a.ContextCompressor()
}

// Model, provider, context used, context window and the winning route for the status reply
func statusModelRoute(ctx context.Context, statusAgent *agent.AIAgent, activeOverride map[string]any, persistedRoute, sessionRow *pyjson.Object, entry *gateway.SessionEntry) (string, string, int, int, map[string]any) {
	contextUsed, contextTotal := 0, 0
	type route struct {
		model, provider string
		extra           map[string]any
	}
	var routes []route
	if statusAgent != nil && statusAgent != agentPendingSentinel {
		routes = append(routes, route{pystr.Strip(statusAgent.Model()), pystr.Strip(statusAgent.Provider()),
			map[string]any{"base_url": pystr.Strip(statusAgent.BaseURL()), "api_key": pystr.Strip(statusAgent.APIKey())}})
		if engine := statusAgent.ContextCompressor(); engine != nil {
			contextUsed = max(0, engine.Base().LastPromptTokens)
			contextTotal = engine.Base().ContextLength
		}
	}
	routes = append(routes, route{cleanStr(activeOverride["model"]), cleanStr(activeOverride["provider"]),
		map[string]any{"base_url": cleanStr(activeOverride["base_url"]), "api_key": cleanStr(activeOverride["api_key"])}})
	get := func(o *pyjson.Object, key string) any {
		if o == nil {
			return nil
		}
		v, _ := o.Get(key)
		return v
	}
	routes = append(routes, route{cleanStr(get(persistedRoute, "model")), cleanStr(get(persistedRoute, "billing_provider")), map[string]any{}})
	winner := route{cleanStr(get(sessionRow, "model")), cleanStr(get(sessionRow, "billing_provider")), map[string]any{}}
	for _, rt := range routes {
		if rt.model != "" && rt.provider != "" {
			winner = rt
			break
		}
	}
	if contextUsed == 0 && entry != nil {
		contextUsed = int(entry.LastPromptTokens)
	}
	modelName, providerName := winner.model, winner.provider
	var userConfig map[string]any
	if modelName == "" || providerName == "" {
		userConfig = quietSync(func() map[string]any { return gateway.LoadGatewayConfigMap(ctx, "") }, map[string]any{})
	}
	modelCfg, _ := userConfig["model"].(map[string]any)
	if modelName == "" {
		modelName = gateway.ResolveGatewayModel(ctx, userConfig)
	}
	if providerName == "" && modelCfg != nil {
		providerName = cleanStr(modelCfg["provider"])
	}
	out := map[string]any{"provider": providerName}
	for k, v := range winner.extra {
		out[k] = v
	}
	return modelName, providerName, contextUsed, contextTotal, out
}

// Full context view lines of auto compression threshold, compression count and cumulative throughput
func contextCompressorLines(ctx context.Context, a *agent.AIAgent, engine agent.ContextEngine, used int) []string {
	var lines []string
	mark := ""
	if agent.ContextDisplaySource(engine) != "provider_usage" {
		mark = "~"
	}
	base := engine.Base()
	threshold := base.ThresholdTokens
	thresholdPct := fmt0(base.ThresholdPercent * 100)
	if threshold > 0 {
		if used >= threshold {
			lines = append(lines, i18n.T(ctx, "gateway.context.over_threshold", "", map[string]any{"threshold": fmtThousands(threshold), "threshold_pct": thresholdPct}))
		} else {
			lines = append(lines, i18n.T(ctx, "gateway.context.threshold", "", map[string]any{"threshold": fmtThousands(threshold),
				"threshold_pct": thresholdPct, "to_go": mark + fmtThousands(threshold-used)}))
		}
	}
	compressions := base.CompressionCount
	lines = append(lines, i18n.T(ctx, "gateway.context.compressions", "", map[string]any{"count": compressions}))
	if compressor, ok := engine.(*agent.ContextCompressor); ok && compressions != 0 {
		lines = append(lines, i18n.T(ctx, "gateway.context.last_savings", "", map[string]any{"savings": fmt0(compressor.LastCompressionSavingsPct())}))
	}
	calls, input, output := agentCount(a, (*agent.AIAgent).SessionAPICalls), agentCount(a, (*agent.AIAgent).SessionInputTokens), agentCount(a, (*agent.AIAgent).SessionOutputTokens)
	reasoning, total := agentCount(a, (*agent.AIAgent).SessionReasoningTokens), agentCount(a, (*agent.AIAgent).SessionTotalTokens)
	return append(lines, "",
		i18n.T(ctx, "gateway.context.totals_header", "", map[string]any{"calls": calls}),
		i18n.T(ctx, "gateway.context.totals_line", "", map[string]any{"input": fmtThousands(input), "output": fmtThousands(output), "reasoning": fmtThousands(reasoning)}),
		i18n.T(ctx, "gateway.context.total_billed", "", map[string]any{"total": fmtThousands(total)}),
		i18n.T(ctx, "gateway.context.throughput_note", "", nil))
}

// Session id, title, model, provider and timestamps of the status reply preformatted for display
func gatewayStatusFields(sessionID string, sessionRow *pyjson.Object, title *string, model, provider string, entry *gateway.SessionEntry, tokens int, agentRunning bool) map[string]any {
	rowTitle := ""
	if title != nil {
		rowTitle = *title
	} else if sessionRow != nil {
		v, _ := sessionRow.Get("title")
		if s, ok := v.(string); ok {
			rowTitle = s
		}
	}
	return map[string]any{"session_id": sessionID, "title": pystr.Strip(rowTitle), "model": model, "provider": provider,
		"created": pydatetime.Strftime(entry.CreatedAt.T, statusStampFormat), "last_activity": pydatetime.Strftime(entry.UpdatedAt.T, statusStampFormat),
		"tokens": fmtThousands(tokens), "agent_running": agentRunning}
}

// Adapter exposing its Matrix session scope
type matrixScopeAdapter interface {
	MatrixSessionScope() string
}

// Session summary of model, context, tokens, queue and connected platforms
func (r *GatewayRunner) handleStatusCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	source := event.Source
	entry, err := r.slashStore().GetOrCreateSession(ctx, source, false, true)
	if err != nil {
		return "", err
	}
	sessionKey := entry.SessionKey
	running, _ := r.runningAgents().Get(sessionKey)
	isRunning := running != nil && running != agentPendingSentinel
	var adapter gateway.PlatformAdapter
	if source != nil && r.adapters != nil {
		adapter, _ = r.adapters.Get(source.Platform)
	}
	queueDepth := r.queueDepth(sessionKey, adapter)
	title, sessionRow, dbTotalTokens, persistedRoute := r.statusSessionDBFacts(ctx, entry.SessionID)
	statusAgent := running
	if !isRunning {
		statusAgent = r.cachedAgentFor(sessionKey, false)
	}
	r.rehydrateSessionModelOverride(ctx, sessionKey)
	activeOverride := r.sessionModelOverride(sessionKey)
	if activeOverride == nil {
		activeOverride = map[string]any{}
	}
	modelName, providerName, contextUsed, contextTotal, route := statusModelRoute(ctx, statusAgent, activeOverride, persistedRoute, sessionRow, entry)
	if contextTotal == 0 && modelName != "" {
		if resolved, ok := r.resolveRouteContext(ctx, source, modelName, route); ok && resolved.contextSource != "default" {
			contextTotal = resolved.contextLength
		}
	}
	fields := gatewayStatusFields(entry.SessionID, sessionRow, title, modelName, providerName, entry, dbTotalTokens, isRunning)
	lines := []string{i18n.T(ctx, "gateway.status.header", "", nil), "", i18n.T(ctx, "gateway.status.session_id", "", map[string]any{"session_id": fields["session_id"]})}
	if fields["title"] != "" {
		lines = append(lines, i18n.T(ctx, "gateway.status.title", "", map[string]any{"title": fields["title"]}))
	}
	lines = append(lines, i18n.T(ctx, "gateway.status.created", "", map[string]any{"timestamp": fields["created"]}),
		i18n.T(ctx, "gateway.status.last_activity", "", map[string]any{"timestamp": fields["last_activity"]}))
	if modelName != "" && providerName != "" {
		lines = append(lines, i18n.T(ctx, "gateway.status.model_provider", "", map[string]any{"model": modelName, "provider": providerName}))
	} else if modelName != "" {
		lines = append(lines, i18n.T(ctx, "gateway.status.model", "", map[string]any{"model": modelName}))
	}
	mark := ""
	if agent.ContextDisplaySource(agentContextEngine(statusAgent)) != "provider_usage" {
		mark = "~"
	}
	if contextTotal != 0 {
		pct := min(100, int(math.RoundToEven(float64(contextUsed)/float64(contextTotal)*100)))
		lines = append(lines, i18n.T(ctx, "gateway.status.context", "", map[string]any{"used": mark + fmtThousands(contextUsed),
			"total": fmtThousands(contextTotal), "pct": fmt.Sprintf("%s%d", mark, pct)}))
	} else if contextUsed != 0 {
		lines = append(lines, i18n.T(ctx, "gateway.status.context_used", "", map[string]any{"used": mark + fmtThousands(contextUsed)}))
	}
	stateKey := "gateway.status.state_no"
	if isRunning {
		stateKey = "gateway.status.state_yes"
	}
	lines = append(lines, i18n.T(ctx, "gateway.status.tokens", "", map[string]any{"tokens": fields["tokens"]}),
		i18n.T(ctx, "gateway.status.agent_running", "", map[string]any{"state": i18n.T(ctx, stateKey, "", nil)}))
	if queueDepth != 0 {
		lines = append(lines, i18n.T(ctx, "gateway.status.queued", "", map[string]any{"count": queueDepth}))
	}
	if source.Platform == gateway.PlatformMatrix {
		scope, present := os.LookupEnv("MATRIX_SESSION_SCOPE")
		if !present {
			scope = "auto"
		}
		if r.adapters != nil {
			if matrix, ok := r.adapters.Get(gateway.PlatformMatrix); ok {
				if scoped, has := matrix.(matrixScopeAdapter); has {
					scope = scoped.MatrixSessionScope()
				}
			}
		}
		threadID := sattr(source.ThreadID)
		if threadID == "" {
			threadID = i18n.T(ctx, "gateway.shared.none_value", "", nil)
		}
		lines = append(lines, "", i18n.T(ctx, "gateway.status.matrix_scope_header", "", nil),
			i18n.T(ctx, "gateway.status.matrix_scope_room", "", map[string]any{"room": chatNameOrID(source)}),
			i18n.T(ctx, "gateway.status.matrix_scope_room_id", "", map[string]any{"room_id": source.ChatID}),
			i18n.T(ctx, "gateway.status.matrix_scope_thread", "", map[string]any{"thread_id": threadID}),
			i18n.T(ctx, "gateway.status.matrix_scope_mode", "", map[string]any{"scope": scope}),
			i18n.T(ctx, "gateway.status.matrix_scope_key", "", map[string]any{"session_key": redactMatrixSessionKey(sessionKey)}))
	}
	var platforms []string
	if r.adapters != nil {
		for _, p := range r.adapters.Keys() {
			platforms = append(platforms, string(p))
		}
	}
	lines = append(lines, "", i18n.T(ctx, "gateway.status.platforms", "", map[string]any{"platforms": strings.Join(platforms, ", ")}))
	return strings.Join(lines, "\n"), nil
}

// Title, row, summed token totals and recent route of the session, each failing open
func (r *GatewayRunner) statusSessionDBFacts(ctx context.Context, sessionID string) (*string, *pyjson.Object, int, *pyjson.Object) {
	asyncDB := r.sessionDB(ctx)
	if asyncDB == nil {
		return nil, pyjson.NewObject(), 0, pyjson.NewObject()
	}
	db := asyncDB.DB()
	title := quiet(func() (*string, error) {
		text, has, err := db.GetSessionTitle(ctx, sessionID)
		if err != nil || !has {
			return nil, err
		}
		return &text, nil
	}, nil)
	sessionRow := quiet(func() (*pyjson.Object, error) { return db.GetSession(ctx, sessionID) }, nil)
	if sessionRow == nil {
		sessionRow = pyjson.NewObject()
	}
	total := 0
	for _, k := range []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens"} {
		v, _ := sessionRow.Get(k)
		total += intValue(v)
	}
	route := quiet(func() (*pyjson.Object, error) { return db.GetRecentSessionModelRoute(ctx, sessionID) }, nil)
	if route == nil {
		route = pyjson.NewObject()
	}
	return title, sessionRow, total, route
}

// Stable sha256 fingerprint of a Matrix session key for shared room status
func redactMatrixSessionKey(sessionKey string) string {
	digest := sha256.Sum256([]byte(sessionKey))
	return "sha256:" + hex.EncodeToString(digest[:])[:12]
}

// Deep context window view with gauge, compression state, throughput and an optional breakdown
func (r *GatewayRunner) handleContextCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	source := event.Source
	entry, err := r.slashStore().GetOrCreateSession(ctx, source, false, true)
	if err != nil {
		return "", err
	}
	expanded := false
	switch pystr.Lower(pystr.Strip(event.GetCommandArgs())) {
	case "all", "full", "details":
		expanded = true
	}
	sessionKey, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return "", err
	}
	resident := r.residentAgentFor(sessionKey)
	engine := agentContextEngine(resident)
	used, contextLength, modelName := r.resolveContextFigures(ctx, resident, engine, entry, source)
	mark := ""
	if agent.ContextDisplaySource(engine) != "provider_usage" {
		mark = "~"
	}
	if used > 0 && contextLength > 0 {
		pct := occupancyPct(used, contextLength)
		filled := int(math.RoundToEven(pct / 100 * 24))
		shownModel := modelName
		if shownModel == "" {
			shownModel = "?"
		}
		lines := []string{i18n.T(ctx, "gateway.context.header", "", nil), "",
			i18n.T(ctx, "gateway.context.model", "", map[string]any{"model": shownModel}),
			i18n.T(ctx, "gateway.context.window", "", map[string]any{"total": fmtThousands(contextLength)}),
			i18n.T(ctx, "gateway.context.in_use", "", map[string]any{"used": mark + fmtThousands(used), "total": fmtThousands(contextLength), "pct": mark + fmt0(pct)}),
			i18n.T(ctx, "gateway.context.bar", "", map[string]any{"bar": strings.Repeat("█", max(0, filled)) + strings.Repeat("░", max(0, 24-filled))}),
			i18n.T(ctx, "gateway.context.headroom", "", map[string]any{"headroom": mark + fmtThousands(max(0, contextLength-used))}),
			""}
		if engine != nil {
			lines = append(lines, contextCompressorLines(ctx, resident, engine, used)...)
		} else {
			lines = append(lines, i18n.T(ctx, "gateway.context.detail_after_first", "", nil))
		}
		if resident != nil {
			if breakdown := r.contextBreakdownBlock(ctx, resident, source, expanded); len(breakdown) > 0 {
				lines = append(append(lines, ""), breakdown...)
			}
		}
		return strings.Join(lines, "\n"), nil
	}
	raw, err := r.slashStore().LoadTranscript(ctx, entry.SessionID)
	if err != nil {
		var readErr *gateway.TranscriptReadError
		if !errors.As(err, &readErr) {
			return "", err
		}
		return historyUnreadable(ctx), nil
	}
	history := transcriptMaps(raw)
	if len(history) == 0 {
		return i18n.T(ctx, "gateway.context.no_data", "", nil), nil
	}
	approx, count := transcriptEstimate(ctx, history)
	return strings.Join([]string{i18n.T(ctx, "gateway.context.header", "", nil), "",
		i18n.T(ctx, "gateway.context.estimated", "", map[string]any{"count": fmtThousands(approx), "messages": count}),
		i18n.T(ctx, "gateway.context.detail_after_first", "", nil)}, "\n"), nil
}

// Context used, window and model, the compressor first, then the session row, the gateway route and model metadata
func (r *GatewayRunner) resolveContextFigures(ctx context.Context, a *agent.AIAgent, engine agent.ContextEngine, entry *gateway.SessionEntry, source *gateway.SessionSource) (int, int, string) {
	used, contextLength := 0, 0
	if engine != nil {
		used, contextLength = max(0, engine.Base().LastPromptTokens), engine.Base().ContextLength
	}
	if used == 0 {
		used = max(0, int(entry.LastPromptTokens))
	}
	modelName := ""
	if a != nil {
		modelName = pystr.Strip(a.Model())
	}
	if modelName == "" {
		if asyncDB := r.sessionDB(ctx); asyncDB != nil {
			if row := quiet(func() (*pyjson.Object, error) { return asyncDB.DB().GetSession(ctx, entry.SessionID) }, nil); row != nil {
				v, _ := row.Get("model")
				modelName = cleanStr(v)
			}
		}
	}
	if contextLength == 0 {
		if resolved, ok := r.resolveRouteContext(ctx, source, modelName, nil); ok {
			if modelName == "" {
				modelName = resolved.model
			}
			contextLength = resolved.contextLength
		}
	}
	if contextLength == 0 && modelName != "" {
		contextLength = quietSync(func() int {
			return provider.GetModelContextLength(ctx, provider.ContextLengthRequest{Model: modelName})
		}, 0)
	}
	return used, contextLength, modelName
}

// Gateway model context of a non resident session inside the profile serving the source, false on any failure
func (r *GatewayRunner) resolveRouteContext(ctx context.Context, source *gateway.SessionSource, modelName string, route map[string]any) (gatewayModelContext, bool) {
	resolved := quiet(func() (*gatewayModelContext, error) {
		scoped := ctx
		if r.multiplexProfiles() {
			profileHome, err := r.ResolveProfileHomeForSource(ctx, source)
			if err != nil {
				return nil, err
			}
			scoped = gateway.ProfileRuntimeScope(ctx, profileHome, true)
		}
		found := resolveGatewayModelContext(scoped, modelName, route)
		return &found, nil
	}, nil)
	if resolved == nil {
		return gatewayModelContext{}, false
	}
	return *resolved, true
}

// Per category context block with optional expanded listings, empty on any rendering failure
func (r *GatewayRunner) contextBreakdownBlock(ctx context.Context, a *agent.AIAgent, source *gateway.SessionSource, expanded bool) (lines []string) {
	defer func() {
		if recover() != nil {
			lines = nil
		}
	}()
	payload, err := r.sessionContextBreakdown(ctx, a, source)
	if err != nil {
		var readErr *gateway.TranscriptReadError
		if errors.As(err, &readErr) {
			return []string{historyUnreadable(ctx)}
		}
		return nil
	}
	if len(payload.Categories) == 0 {
		return nil
	}
	var details *agent.ContextDetails
	if expanded {
		d, err := agent.ComputeContextDetails(ctx, a)
		if err != nil {
			d = agent.ContextDetails{}
		}
		details = &d
	}
	var fileLines []string
	if sources, err := agent.ContextFileSourcesForAgent(ctx, a); err == nil {
		fileLines = agent.RenderContextFileLines(sources)
	}
	lines = agent.RenderContextBreakdownLines(ctx, payload, details, false)
	if len(fileLines) > 0 {
		lines = append(append(lines, ""), fileLines...)
	}
	return lines
}

// Per category context estimate of the agent over the session transcript, a read failure surfacing as its error
func (r *GatewayRunner) sessionContextBreakdown(ctx context.Context, a *agent.AIAgent, source *gateway.SessionSource) (agent.ContextBreakdown, error) {
	var history []map[string]any
	if entry, err := r.sessionStore.GetOrCreateSession(ctx, source, false, true); err == nil {
		raw, err := r.sessionStore.LoadTranscript(ctx, entry.SessionID)
		if err != nil {
			var readErr *gateway.TranscriptReadError
			if errors.As(err, &readErr) {
				return agent.ContextBreakdown{}, err
			}
		} else {
			history = transcriptMaps(raw)
		}
	}
	return agent.ComputeSessionContextBreakdown(ctx, a, history)
}

// Usage per category breakdown lines, empty when nothing is estimated or rendering fails
func (r *GatewayRunner) contextBreakdownLines(ctx context.Context, a *agent.AIAgent, source *gateway.SessionSource) (out []string) {
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	payload, err := r.sessionContextBreakdown(ctx, a, source)
	if err != nil {
		var readErr *gateway.TranscriptReadError
		if errors.As(err, &readErr) {
			return []string{historyUnreadable(ctx)}
		}
		return nil
	}
	if len(payload.Categories) == 0 {
		return nil
	}
	total := payload.EstimatedTotal
	out = []string{i18n.T(ctx, "gateway.usage.breakdown_header", "", nil)}
	for _, cat := range payload.Categories {
		if cat.Tokens <= 0 {
			continue
		}
		label := i18n.T(ctx, "gateway.usage.breakdown_cat_"+cat.ID, "", nil)
		if strings.HasSuffix(label, "breakdown_cat_"+cat.ID) {
			label = cat.Label
			if label == "" {
				label = cat.ID
			}
		}
		pct := 0
		if total != 0 {
			pct = int(math.RoundToEven(float64(cat.Tokens) / float64(total) * 100))
		}
		out = append(out, i18n.T(ctx, "gateway.usage.breakdown_line", "", map[string]any{"label": label, "count": fmtThousands(cat.Tokens), "pct": fmt.Sprintf("~%d", pct)}))
	}
	if len(out) > 1 {
		return out
	}
	return nil
}
