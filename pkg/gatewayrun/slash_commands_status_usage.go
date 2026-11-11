package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Unicode dash a phone keyboard substitutes for a double hyphen before an insights flag
var insightsDashFlagRe = regexp.MustCompile(`[\x{2012}\x{2013}\x{2014}\x{2015}](days|source)`)

// Seconds a portal fetch of the usage and topup replies may take
const accountFetchTimeout = 10 * time.Second

// Agents rows for one background delegation with its live per child activity
func agentsDelegationLines(ctx context.Context, d map[string]any) []string {
	goal := clipText(strings.Join(pystr.SplitWhitespace(pyrepr.Str(pyrepr.Or(d["goal"], "")), -1), " "), 70)
	status, ok := d["status"]
	if !ok {
		status = "?"
	}
	delegationID, ok := d["delegation_id"]
	if !ok {
		delegationID = "?"
	}
	row := fmt.Sprintf("- `%s` · %s", pyrepr.Str(delegationID), pyrepr.Str(status))
	quiet := d["stalled_after_quiet_seconds"]
	sinceProgress, _ := d["seconds_since_progress"].(float64)
	if status == "stalling" && quiet != nil {
		q, _ := quiet.(float64)
		row += i18n.T(ctx, "gateway.agents.no_progress", "", map[string]any{"seconds": fmt0(q)})
	} else if status != "stalling" && sinceProgress >= 60 {
		row += i18n.T(ctx, "gateway.agents.quiet_for", "", map[string]any{"seconds": fmt0(sinceProgress)})
	}
	if goal != "" {
		row += " · " + goal
	}
	lines := []string{row}
	children, _ := d["children_activity"].([]any)
	for i, raw := range children {
		child, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		doing := i18n.T(ctx, "gateway.agents.between_turns", "", nil)
		if tool := child["current_tool"]; pyrepr.Truthy(tool) {
			doing = "`" + pyrepr.Str(tool) + "`"
		}
		calls, ok := child["api_calls"]
		if !ok {
			calls = "?"
		}
		part := i18n.T(ctx, "gateway.agents.child_row", "", map[string]any{"index": i + 1, "calls": calls, "doing": doing})
		if idle, ok := child["seconds_since_activity"].(float64); ok {
			part += i18n.T(ctx, "gateway.agents.active_ago", "", map[string]any{"seconds": fmt0(idle)})
		}
		lines = append(lines, part)
	}
	return lines
}

// Usage session block for a live agent of rate limits, token breakdown, context window and compressions
func usageAgentStatsLines(ctx context.Context, a *agent.AIAgent) []string {
	var lines []string
	if rl := a.CreditsCapture().RateLimit(); rl.HasData() {
		lines = append(lines, i18n.T(ctx, "gateway.usage.rate_limits", "", map[string]any{"state": provider.FormatRateLimitCompact(rl, time.Now())}), "")
	}
	lines = append(lines, i18n.T(ctx, "gateway.usage.header_session", "", nil),
		i18n.T(ctx, "gateway.usage.label_model", "", map[string]any{"model": a.Model()}),
		i18n.T(ctx, "gateway.usage.label_input_tokens", "", map[string]any{"count": fmtThousands(a.SessionInputTokens())}),
		i18n.T(ctx, "gateway.usage.label_output_tokens", "", map[string]any{"count": fmtThousands(a.SessionOutputTokens())}),
		i18n.T(ctx, "gateway.usage.label_total", "", map[string]any{"count": fmtThousands(a.SessionTotalTokens())}),
		i18n.T(ctx, "gateway.usage.label_api_calls", "", map[string]any{"count": a.SessionAPICalls()}))
	engine := a.ContextCompressor()
	base := engine.Base()
	if base.LastPromptTokens > 0 {
		pct := occupancyPct(base.LastPromptTokens, base.ContextLength)
		mark := ""
		if agent.ContextDisplaySource(engine) != "provider_usage" {
			mark = "~"
		}
		lines = append(lines, i18n.T(ctx, "gateway.usage.label_context", "", map[string]any{"used": mark + fmtThousands(base.LastPromptTokens),
			"total": fmtThousands(base.ContextLength), "pct": mark + fmt0(pct)}))
	}
	if base.CompressionCount != 0 {
		lines = append(lines, i18n.T(ctx, "gateway.usage.label_compressions", "", map[string]any{"count": base.CompressionCount}))
	}
	return lines
}

// Up to the list cap of rendered items plus an overflow line
func cappedRows[T any](ctx context.Context, items []T, render func(T) []string) []string {
	var lines []string
	for _, item := range items[:min(len(items), agentsListCap)] {
		lines = append(lines, render(item)...)
	}
	if len(items) > agentsListCap {
		lines = append(lines, i18n.T(ctx, "gateway.agents.more", "", map[string]any{"count": len(items) - agentsListCap}))
	}
	return lines
}

// Lists active agents, running processes, async jobs and background delegations
func (r *GatewayRunner) handleAgentsCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	now := float64(time.Now().UnixNano()) / 1e9
	currentKey, err := r.sessionKeyForSource(ctx, event.Source)
	if err != nil {
		return "", err
	}
	type agentRow struct {
		sessionKey, state, sessionID, model string
		elapsed                             int
	}
	started := r.runningAgentsTS()
	running := r.runningAgents()
	var rows []agentRow
	for _, key := range running.Keys() {
		a, _ := running.Get(key)
		pending := a == agentPendingSentinel
		start, ok := started.Get(key)
		if !ok {
			start = now
		}
		row := agentRow{sessionKey: key, elapsed: max(0, int(now-start)), state: i18n.T(ctx, "gateway.agents.state_running", "", nil)}
		if pending {
			row.state = i18n.T(ctx, "gateway.agents.state_starting", "", nil)
		} else if a != nil {
			row.sessionID, row.model = a.SessionID(), a.Model()
		}
		rows = append(rows, row)
	}
	slices.SortStableFunc(rows, func(x, y agentRow) int { return y.elapsed - x.elapsed })
	procs := func() (out []map[string]any) {
		defer func() {
			if recover() != nil {
				out = nil
			}
		}()
		return tools.DefaultProcessRegistry().ListSessions(ctx, slashToolsEnv(ctx), "", "", false)
	}()
	var runningProcs []map[string]any
	for _, p := range procs {
		if p["status"] == "running" {
			runningProcs = append(runningProcs, p)
		}
	}
	r.taskSetsMu.Lock()
	var backgroundTasks []*gatewayTask
	for task := range r.backgroundTasks {
		if !task.isDone() {
			backgroundTasks = append(backgroundTasks, task)
		}
	}
	r.taskSetsMu.Unlock()
	var delegations []map[string]any
	func() {
		defer func() { _ = recover() }()
		for _, d := range tools.ListAsyncDelegations() {
			if s := d["status"]; s == "running" || s == "stalling" || s == "finalizing" {
				delegations = append(delegations, d)
			}
		}
	}()
	type indexedRow struct {
		idx int
		row agentRow
	}
	indexed := make([]indexedRow, len(rows))
	for i, row := range rows {
		indexed[i] = indexedRow{i + 1, row}
	}
	lines := []string{i18n.T(ctx, "gateway.agents.header", "", nil), "", i18n.T(ctx, "gateway.agents.active_agents", "", map[string]any{"count": len(rows)})}
	lines = append(lines, cappedRows(ctx, indexed, func(ir indexedRow) []string {
		current := ""
		if ir.row.sessionKey == currentKey {
			current = i18n.T(ctx, "gateway.agents.this_chat", "", nil)
		}
		sid, model := "", ""
		if ir.row.sessionID != "" {
			sid = " · `" + ir.row.sessionID + "`"
		}
		if ir.row.model != "" {
			model = " · `" + ir.row.model + "`"
		}
		return []string{fmt.Sprintf("%d. `%s` · %s · %s%s%s%s", ir.idx, ir.row.sessionKey, ir.row.state, tools.FormatUptimeShort(ir.row.elapsed), sid, model, current)}
	})...)
	lines = append(lines, "", i18n.T(ctx, "gateway.agents.running_processes", "", map[string]any{"count": len(runningProcs)}))
	lines = append(lines, cappedRows(ctx, runningProcs, func(p map[string]any) []string {
		cmd := clipText(strings.Join(pystr.SplitWhitespace(pyrepr.Str(pyrepr.Or(p["command"], "")), -1), " "), 90)
		sessionID, ok := p["session_id"]
		if !ok {
			sessionID = "?"
		}
		return []string{fmt.Sprintf("- `%s` · %s · `%s`", pyrepr.Str(sessionID), tools.FormatUptimeShort(intValue(p["uptime_seconds"])), cmd)}
	})...)
	lines = append(lines, "", i18n.T(ctx, "gateway.agents.async_jobs", "", map[string]any{"count": len(backgroundTasks)}))
	if len(delegations) > 0 {
		lines = append(lines, "", i18n.T(ctx, "gateway.agents.background_delegations", "", map[string]any{"count": len(delegations)}))
		lines = append(lines, cappedRows(ctx, delegations, func(d map[string]any) []string { return agentsDelegationLines(ctx, d) })...)
	}
	if len(rows) == 0 && len(runningProcs) == 0 && len(backgroundTasks) == 0 && len(delegations) == 0 {
		lines = append(lines, "", i18n.T(ctx, "gateway.agents.none", "", nil))
	}
	return strings.Join(lines, "\n"), nil
}

// Nous balance with the portal top up link, never charging or tracking payment
func (r *GatewayRunner) handleTopupCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	view, ok := func() (v provider.CreditsView, ok bool) {
		defer func() {
			if recover() != nil {
				v, ok = provider.CreditsView{}, false
			}
		}()
		return provider.BuildCreditsView(ctx, true, accountFetchTimeout), true
	}()
	if !ok || !view.LoggedIn {
		return i18n.T(ctx, "gateway.credits.not_logged_in", "", nil), nil
	}
	lines := []string{i18n.T(ctx, "gateway.topup.header", "", nil)}
	for _, ln := range view.BalanceLines {
		if !strings.HasPrefix(pystr.Lstrip(ln), "📈") {
			lines = append(lines, ln)
		}
	}
	if view.IdentityLine != "" {
		lines = append(lines, "", view.IdentityLine)
	}
	if view.TopupURL != "" {
		lines = append(lines, "", i18n.T(ctx, "gateway.topup.portal_link", "", map[string]any{"url": view.TopupURL}), i18n.T(ctx, "gateway.topup.hint", "", nil))
	}
	return strings.Join(lines, "\n"), nil
}

// Token usage of the session plus account and credit blocks, reset redeeming a banked Codex reset credit
func (r *GatewayRunner) handleUsageCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	source := event.Source
	sessionKey, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return "", err
	}
	rawArgs := pystr.Strip(event.GetCommandArgs())
	var args []string
	for _, a := range pystr.SplitWhitespace(rawArgs, -1) {
		args = append(args, pystr.Lower(a))
	}
	wantsReset := len(args) > 0 && args[0] == "reset"
	if len(args) > 0 && !wantsReset {
		return i18n.T(ctx, "gateway.usage.unknown_subcommand", "", map[string]any{"args": rawArgs}), nil
	}
	resident := r.residentAgentFor(sessionKey)
	providerName, baseURL, apiKey := "", "", ""
	if resident != nil {
		providerName, baseURL, apiKey = resident.Provider(), resident.BaseURL(), resident.APIKey()
	}
	if providerName == "" && r.sessionDB(ctx) != nil {
		providerName, baseURL = r.persistedBillingRoute(ctx, source)
	}
	if providerName == "" {
		providerName = func() (p string) {
			defer func() {
				if recover() != nil {
					p = ""
				}
			}()
			return configuredProvider(ctx)
		}()
	}
	if wantsReset {
		if pystr.Lower(pystr.Strip(providerName)) != "openai-codex" {
			return i18n.T(ctx, "gateway.usage.reset_wrong_provider", "", nil), nil
		}
		return provider.RedeemCodexResetCredit(ctx, baseURL, apiKey, slices.Contains(args[1:], "--force")).Message, nil
	}
	var accountLines []string
	if providerName != "" {
		func() {
			defer func() { _ = recover() }()
			if snapshot := provider.FetchAccountUsage(ctx, providerName, provider.AccountUsageFetchOptions{BaseURL: baseURL, APIKey: apiKey}); snapshot != nil {
				accountLines = provider.RenderAccountUsageLines(snapshot, true)
			}
		}()
	}
	var creditsState *provider.CreditsState
	if resident != nil {
		creditsState = resident.CreditsCapture().Credits()
	}
	var creditsLines []string
	func() {
		defer func() { _ = recover() }()
		for _, line := range provider.NousCreditsLines(ctx, true, accountFetchTimeout, creditsState) {
			creditsLines = append(creditsLines, provider.PortalWording(line))
		}
	}()
	withAccountBlocks := func(lines []string) string {
		for _, block := range [][]string{accountLines, creditsLines} {
			if len(block) > 0 {
				if len(lines) > 0 {
					lines = append(lines, "")
				}
				lines = append(lines, block...)
			}
		}
		return strings.Join(lines, "\n")
	}
	if resident != nil && resident.SessionAPICalls() > 0 {
		lines := usageAgentStatsLines(ctx, resident)
		if breakdown := r.contextBreakdownLines(ctx, resident, source); len(breakdown) > 0 {
			lines = append(append(lines, ""), breakdown...)
		}
		return withAccountBlocks(lines), nil
	}
	entry, err := r.slashStore().GetOrCreateSession(ctx, source, false, true)
	if err != nil {
		return "", err
	}
	raw, err := r.slashStore().LoadTranscript(ctx, entry.SessionID)
	if err != nil {
		var readErr *gateway.TranscriptReadError
		if !errors.As(err, &readErr) {
			return "", err
		}
		return historyUnreadable(ctx), nil
	}
	if history := transcriptMaps(raw); len(history) > 0 {
		approx, count := transcriptEstimate(ctx, history)
		return withAccountBlocks([]string{i18n.T(ctx, "gateway.usage.header_session_info", "", nil),
			i18n.T(ctx, "gateway.usage.label_messages", "", map[string]any{"count": count}),
			i18n.T(ctx, "gateway.usage.label_estimated_context", "", map[string]any{"count": fmtThousands(approx)}),
			i18n.T(ctx, "gateway.usage.detailed_after_first", "", nil)}), nil
	}
	if len(accountLines) > 0 || len(creditsLines) > 0 {
		return withAccountBlocks(nil), nil
	}
	return i18n.T(ctx, "gateway.usage.no_data", "", nil), nil
}

// Billing provider and base url of the session row or its most recent route, empty when none resolves
func (r *GatewayRunner) persistedBillingRoute(ctx context.Context, source *gateway.SessionSource) (string, string) {
	persisted, recent := pyjson.NewObject(), pyjson.NewObject()
	func() {
		defer func() { _ = recover() }()
		entry, err := r.slashStore().GetOrCreateSession(ctx, source, false, true)
		if err != nil {
			return
		}
		db := r.sessionDB(ctx).DB()
		row, err := db.GetSession(ctx, entry.SessionID)
		if err != nil {
			return
		}
		route, err := db.GetRecentSessionModelRoute(ctx, entry.SessionID)
		if err != nil {
			return
		}
		if row != nil {
			persisted = row
		}
		if route != nil {
			recent = route
		}
	}()
	row := persisted
	if v, _ := recent.Get("billing_provider"); pyrepr.Truthy(v) {
		row = recent
	}
	providerValue, _ := row.Get("billing_provider")
	baseValue, _ := row.Get("billing_base_url")
	providerName, baseURL := "", ""
	if providerValue != nil {
		providerName = pyrepr.Str(providerValue)
	}
	if baseValue != nil {
		baseURL = pyrepr.Str(baseValue)
	}
	return providerName, baseURL
}

// Usage insights and analytics over the last N days, optionally for one source
func (r *GatewayRunner) handleInsightsCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	args := insightsDashFlagRe.ReplaceAllString(pystr.Strip(event.GetCommandArgs()), "--$1")
	days, source := 30, ""
	parts := pystr.SplitWhitespace(args, -1)
	for i := 0; i < len(parts); {
		flag := parts[i]
		hasValue := i+1 < len(parts)
		switch {
		case flag == "--days" && hasValue:
			n, ok := pystr.IntText(parts[i+1])
			if !ok || !n.IsInt64() {
				return i18n.T(ctx, "gateway.insights.invalid_days", "", map[string]any{"value": parts[i+1]}), nil
			}
			days = int(n.Int64())
			i += 2
		case flag == "--source" && hasValue:
			source = parts[i+1]
			i += 2
		default:
			if pystr.StrIsDigit(flag) {
				if n, ok := pystr.IntText(flag); ok && n.IsInt64() {
					days = int(n.Int64())
				}
			}
			i++
		}
	}
	value, err := r.runInExecutorWithContext(ctx, func(workerCtx context.Context) (any, error) {
		db, err := state.Acquire(workerCtx, "")
		if err != nil {
			return nil, err
		}
		defer state.ReleaseOrClose(db)
		engine, err := memory.OpenInsightsEngine(db)
		if err != nil {
			return nil, err
		}
		defer engine.Close()
		report, err := engine.Generate(workerCtx, days, source)
		if err != nil {
			return nil, err
		}
		return engine.FormatGateway(report), nil
	})
	if err != nil {
		runLog().Error(fmt.Sprintf("Insights command error: %s", pyrepr.ExceptionMessage(err)), "exc_info", err)
		return i18n.T(ctx, "gateway.insights.error", "", map[string]any{"error": pyrepr.ExceptionMessage(err)}), nil
	}
	return value.(string), nil
}
