package memory

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

var insightsTokenKeys = []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens"}

// Sub cent totals render at four places through the shared cost label
func insightsFmtEstCost(est float64) string {
	if math.IsInf(est, 1) {
		return "~$Infinity"
	}
	r, _ := new(big.Rat).SetString(pyrepr.Float(est))
	return provider.FormatCostLabel(r)
}

// USD estimate and pricing status of a token bucket through the provider pricing tables
func insightsEstimateCost(ctx context.Context, model string, counts map[string]any, prov, baseURL string) (float64, string, error) {
	var vals [4]int
	for i, k := range insightsTokenKeys {
		n, err := pyIntCell(or0(counts[k]))
		if err != nil {
			return 0, "", err
		}
		vals[i] = int(n)
	}
	usage := provider.CanonicalUsage{InputTokens: vals[0], OutputTokens: vals[1], CacheReadTokens: vals[2], CacheWriteTokens: vals[3], RequestCount: 1}
	res := provider.EstimateUsageCost(ctx, model, usage, prov, baseURL, "")
	amount := 0.0
	if res.AmountUSD != nil {
		amount, _ = res.AmountUSD.Float64()
	}
	return amount, res.Status, nil
}

// Block bars scaled to the peak value, empty for zero values
func insightsBarChart(values []int64, maxWidth int) []string {
	out := make([]string, len(values))
	var peak int64 = 1
	if len(values) > 0 {
		peak = values[0]
		for _, v := range values {
			peak = max(peak, v)
		}
	}
	if peak == 0 {
		return out
	}
	for i, v := range values {
		if v > 0 {
			out[i] = strings.Repeat("█", max(1, int(float64(v)/float64(peak)*float64(maxWidth))))
		}
	}
	return out
}

// Model name without its provider prefix, unknown when empty
func insightsShortModel(model string) string {
	if model == "" {
		model = "unknown"
	}
	return model[strings.LastIndex(model, "/")+1:]
}

func insightsDecode(raw any) any {
	if s, ok := raw.(string); ok {
		v, err := pyjson.Loads(s)
		if err != nil {
			return nil
		}
		return v
	}
	return raw
}

// Function entries of every dict in a tool_calls column, an empty dict for entries without one
func insightsFunctions(raw any) []any {
	calls, _ := insightsDecode(raw).([]any)
	var out []any
	for _, call := range calls {
		if obj, ok := call.(*pyjson.Object); ok {
			fn, found := obj.Get("function")
			if !found {
				fn = pyjson.NewObject()
			}
			out = append(out, fn)
		}
	}
	return out
}

func insightsAttrGet(fn any, key string) (any, error) {
	obj, ok := fn.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(fn))
	}
	v, _ := obj.Get(key)
	return v, nil
}

func insightsHour12(hr int) string {
	h := hr % 12
	if h == 0 {
		h = 12
	}
	if hr < 12 {
		return fmt.Sprintf("%dAM", h)
	}
	return fmt.Sprintf("%dPM", h)
}

// Month and day of a stored timestamp, a question mark when unset or corrupt
func insightsDay(ts any) string {
	if !pyrepr.Truthy(ts) {
		return "?"
	}
	c, ok := pydatetime.CoerceEpoch(ts, "", "")
	if !ok || c == 0 {
		return "?"
	}
	return pydatetime.Strftime(pydatetime.FromTimestamp(c), "%b %d")
}

func insightsLocalSurface(alias string) string {
	return " OR (" + alias + "source = 'local' AND " + alias + "created_source = ?)"
}

// Unfiltered and source filtered query pair sharing one body
func insightsScoped(before, after, alias string) [2]string {
	src := " AND (" + alias + "source = ?" + insightsLocalSurface(alias) + ")"
	return [2]string{before + after, before + src + after}
}

const (
	insightsSessionCols = "id, source, model, started_at, ended_at, " +
		"message_count, tool_call_count, input_tokens, output_tokens, " +
		"cache_read_tokens, cache_write_tokens, billing_provider, " +
		"billing_base_url, billing_mode, estimated_cost_usd, " +
		"actual_cost_usd, cost_status, cost_source, api_call_count"
	insightsAssistantCallsIndex = "idx_messages_assistant_calls_by_session"
	insightsAssistantCalls      = " FROM messages m INDEXED BY " + insightsAssistantCallsIndex +
		" JOIN sessions s ON s.id = m.session_id" +
		" WHERE s.started_at >= ?"
)

func insightsBaseQueries() map[string][2]string {
	return map[string][2]string{
		"sessions": insightsScoped("SELECT "+insightsSessionCols+" FROM sessions WHERE started_at >= ?", " ORDER BY started_at DESC", ""),
		"tool_calls": insightsScoped("SELECT m.session_id, m.tool_calls"+insightsAssistantCalls,
			" AND m.role = 'assistant' AND m.tool_calls IS NOT NULL", "s."),
		"skill_calls": insightsScoped("SELECT m.tool_calls, m.timestamp"+insightsAssistantCalls,
			" AND m.role = 'assistant' AND m.tool_calls IS NOT NULL"+
				" AND (instr(m.tool_calls, 'skill_view') > 0"+
				" OR instr(m.tool_calls, 'skill_manage') > 0)", "s."),
		"tool_names": insightsScoped(`SELECT m.session_id, m.tool_name, COUNT(*) as count
                   FROM messages m
                   JOIN sessions s ON s.id = m.session_id
                   WHERE s.started_at >= ?`, `
                     AND m.role = 'tool' AND m.tool_name IS NOT NULL
                   GROUP BY m.session_id, m.tool_name`, "s."),
		"message_stats": insightsScoped(`SELECT
                     COUNT(*) as total_messages,
                     SUM(CASE WHEN m.role = 'user' THEN 1 ELSE 0 END) as user_messages,
                     SUM(CASE WHEN m.role = 'assistant' THEN 1 ELSE 0 END) as assistant_messages,
                     SUM(CASE WHEN m.role = 'tool' THEN 1 ELSE 0 END) as tool_messages
                   FROM messages m
                   JOIN sessions s ON s.id = m.session_id
                   WHERE s.started_at >= ?`, "", "s."),
		"model_usage": insightsScoped("SELECT u.session_id, u.model, u.billing_provider, u.billing_base_url,"+
			" u.api_call_count, u.input_tokens, u.output_tokens,"+
			" u.cache_read_tokens, u.cache_write_tokens, u.reasoning_tokens,"+
			" u.estimated_cost_usd, u.actual_cost_usd, u.cost_status,"+
			" u.cost_source, u.billing_mode"+
			" FROM session_model_usage u"+
			" JOIN sessions s ON s.id = u.session_id"+
			" WHERE s.started_at >= ?", "", "s."),
	}
}

// Session history analytics over the session store
type InsightsEngine struct {
	db                     any
	conn                   *sql.DB
	owned                  bool
	hasAssistantCallsIndex bool
	localSurfaces          bool
	queries                map[string][2]string
}

// Engine over an open SQLite connection, unpinning the partial index and the local surface clause when the schema lacks them
func NewInsightsEngine(conn *sql.DB) *InsightsEngine {
	e := &InsightsEngine{conn: conn, queries: insightsBaseQueries()}
	var one int
	e.hasAssistantCallsIndex = conn.QueryRow("SELECT 1 FROM sqlite_master WHERE type='index' AND name=?", insightsAssistantCallsIndex).Scan(&one) == nil
	if !e.hasAssistantCallsIndex {
		strip := " INDEXED BY " + insightsAssistantCallsIndex
		for _, base := range []string{"tool_calls", "skill_calls"} {
			q := e.queries[base]
			e.queries[base] = [2]string{strings.ReplaceAll(q[0], strip, ""), strings.ReplaceAll(q[1], strip, "")}
		}
	}
	if rows, err := conn.Query("PRAGMA table_info(sessions)"); err == nil {
		for rows.Next() {
			var cid, notnull, pk any
			var name, typ, dflt any
			if rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk) == nil && insightsCellStr(name) == "created_source" {
				e.localSurfaces = true
			}
		}
		if rows.Err() != nil {
			e.localSurfaces = false
		}
		rows.Close()
	}
	if !e.localSurfaces {
		for base, q := range e.queries {
			sql := q[1]
			for _, alias := range []string{"", "s."} {
				sql = strings.ReplaceAll(sql, insightsLocalSurface(alias), "")
			}
			e.queries[base] = [2]string{q[0], sql}
		}
	}
	return e
}

// Engine on a read connection of the session store file that Close releases
func OpenInsightsEngine(db *state.DB) (*InsightsEngine, error) {
	conn, err := sql.Open("sqlite", "file:"+db.Path()+"?_pragma=busy_timeout(1000)&mode=ro")
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	e := NewInsightsEngine(conn)
	e.db, e.owned = db, true
	return e, nil
}

// Releases the read connection the engine opened itself
func (e *InsightsEngine) Close() error {
	if e.owned {
		return e.conn.Close()
	}
	return nil
}

func insightsCellStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	}
	return pyrepr.Str(v)
}

func (e *InsightsEngine) query(ctx context.Context, base string, cutoff float64, source string) ([]map[string]any, error) {
	q := e.queries[base]
	sqlText, args := q[0], []any{cutoff}
	if source != "" {
		sqlText = q[1]
		args = append(args, source)
		if e.localSurfaces {
			args = append(args, source)
		}
	}
	rows, err := e.conn.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = vals[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func insightsCutoff(days int) float64 {
	return float64(time.Now().UnixNano())/1e9 - float64(days*86400)
}

// Complete report for the last days days, optionally filtered by source platform
func (e *InsightsEngine) Generate(ctx context.Context, days int, source string) (*InsightsReport, error) {
	cutoff := insightsCutoff(days)
	if flusher, ok := e.db.(interface{ FlushTokenCounts(timeoutS float64) bool }); ok {
		flusher.FlushTokenCounts(5.0)
	}
	sessions, err := e.getSessions(ctx, cutoff, source)
	if err != nil {
		return nil, err
	}
	toolUsage, err := e.getToolUsage(ctx, cutoff, source)
	if err != nil {
		return nil, err
	}
	skillUsage, err := e.getSkillUsage(ctx, cutoff, source)
	if err != nil {
		return nil, err
	}
	messageStats, err := e.getMessageStats(ctx, cutoff, source)
	if err != nil {
		return nil, err
	}
	report := &InsightsReport{Days: days, SourceFilter: source, Models: []InsightsModel{}, Platforms: []InsightsPlatform{},
		Tools: []InsightsTool{}, TopSessions: []InsightsTopSession{}}
	if len(sessions) == 0 {
		report.Empty = true
		report.Skills, err = insightsSkillBreakdown(nil)
		return report, err
	}
	if report.Models, err = e.computeModelBreakdown(ctx, sessions, cutoff, source); err != nil {
		return nil, err
	}
	report.GeneratedAt = float64(time.Now().UnixNano()) / 1e9
	if report.Overview, err = insightsOverview(ctx, sessions, messageStats, report.Models); err != nil {
		return nil, err
	}
	if report.Platforms, err = insightsPlatformBreakdown(sessions); err != nil {
		return nil, err
	}
	report.Tools = insightsToolBreakdown(toolUsage)
	if report.Skills, err = insightsSkillBreakdown(skillUsage); err != nil {
		return nil, err
	}
	report.Activity = insightsActivityPatterns(sessions)
	if report.TopSessions, err = insightsTopSessions(sessions); err != nil {
		return nil, err
	}
	return report, nil
}

// Tool and skill analytics without a full report
func (e *InsightsEngine) GetUsageBreakdown(ctx context.Context, days int, source string) (*InsightsUsageBreakdown, error) {
	cutoff := insightsCutoff(days)
	tools, err := e.getToolUsage(ctx, cutoff, source)
	if err != nil {
		return nil, err
	}
	skills, err := e.getSkillUsage(ctx, cutoff, source)
	if err != nil {
		return nil, err
	}
	breakdown, err := insightsSkillBreakdown(skills)
	if err != nil {
		return nil, err
	}
	return &InsightsUsageBreakdown{Tools: insightsToolBreakdown(tools), Skills: breakdown}, nil
}

// Session rows with started_at and ended_at coerced so one corrupt cell degrades to unknown
func (e *InsightsEngine) getSessions(ctx context.Context, cutoff float64, source string) ([]map[string]any, error) {
	rows, err := e.query(ctx, "sessions", cutoff, source)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		for _, col := range []string{"started_at", "ended_at"} {
			if ts, ok := pydatetime.CoerceEpoch(row[col], insightsCellStr(row["id"]), col); ok {
				row[col] = ts
			} else {
				row[col] = nil
			}
		}
	}
	return rows, nil
}

type insightsToolKey struct {
	session string
	kind    string
	name    string
}

type insightsToolCount struct {
	name  any
	count int64
}

// Tool call counts reconciled per session between tool rows and assistant tool_calls, then summed
func (e *InsightsEngine) getToolUsage(ctx context.Context, cutoff float64, source string) ([]insightsToolCount, error) {
	var order []insightsToolKey
	names := map[insightsToolKey]any{}
	seen := func(k insightsToolKey, name any) {
		if _, ok := names[k]; !ok {
			names[k] = name
			order = append(order, k)
		}
	}
	bySessionTool := map[insightsToolKey]int64{}
	rows, err := e.query(ctx, "tool_names", cutoff, source)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		name := row["tool_name"]
		k := insightsToolKey{insightsCellStr(row["session_id"]), pyrepr.TypeName(name), insightsCellStr(name)}
		seen(k, name)
		n, _ := row["count"].(int64)
		bySessionTool[k] += n
	}
	callsBySessionTool := map[insightsToolKey]int64{}
	rows, err = e.query(ctx, "tool_calls", cutoff, source)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		for _, fn := range insightsFunctions(row["tool_calls"]) {
			name, err := insightsAttrGet(fn, "name")
			if err != nil {
				break
			}
			if !pyrepr.Truthy(name) {
				continue
			}
			if _, isList := name.([]any); isList {
				break
			}
			if _, isDict := name.(*pyjson.Object); isDict {
				break
			}
			k := insightsToolKey{insightsCellStr(row["session_id"]), pyrepr.TypeName(name), pyrepr.Repr(name)}
			if s, ok := name.(string); ok {
				k.name = s
			}
			seen(k, name)
			callsBySessionTool[k]++
		}
	}
	type total struct {
		name  any
		count int64
	}
	var toolOrder []string
	totals := map[string]*total{}
	for _, k := range order {
		a, aok := bySessionTool[k]
		b, bok := callsBySessionTool[k]
		if !aok && !bok {
			continue
		}
		id := k.kind + "\x00" + k.name
		t, ok := totals[id]
		if !ok {
			t = &total{name: names[k]}
			totals[id] = t
			toolOrder = append(toolOrder, id)
		}
		t.count += max(a, b)
	}
	out := make([]insightsToolCount, 0, len(toolOrder))
	for _, id := range toolOrder {
		out = append(out, insightsToolCount{totals[id].name, totals[id].count})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].count > out[j].count })
	return out, nil
}

type insightsSkillCount struct {
	skill       string
	viewCount   int64
	manageCount int64
	lastUsedAt  any
}

// Per skill view and manage counts from assistant skill_view and skill_manage calls
func (e *InsightsEngine) getSkillUsage(ctx context.Context, cutoff float64, source string) ([]*insightsSkillCount, error) {
	rows, err := e.query(ctx, "skill_calls", cutoff, source)
	if err != nil {
		return nil, err
	}
	var out []*insightsSkillCount
	bySkill := map[string]*insightsSkillCount{}
	for _, row := range rows {
		timestamp := row["timestamp"]
		for _, fn := range insightsFunctions(row["tool_calls"]) {
			toolName, err := insightsAttrGet(fn, "name")
			if err != nil {
				return nil, err
			}
			switch toolName.(type) {
			case []any, *pyjson.Object:
				return nil, fmt.Errorf("unhashable type: '%s'", pyrepr.TypeName(toolName))
			}
			if toolName != "skill_view" && toolName != "skill_manage" {
				continue
			}
			argsVal, _ := insightsAttrGet(fn, "arguments")
			args, _ := insightsDecode(argsVal).(*pyjson.Object)
			var nameVal any
			if args != nil {
				nameVal, _ = args.Get("name")
			}
			skillName, ok := nameVal.(string)
			if !ok || strings.TrimSpace(skillName) == "" {
				continue
			}
			entry, ok := bySkill[skillName]
			if !ok {
				entry = &insightsSkillCount{skill: skillName}
				bySkill[skillName] = entry
				out = append(out, entry)
			}
			if toolName == "skill_view" {
				entry.viewCount++
			} else {
				entry.manageCount++
			}
			if timestamp != nil {
				newer := entry.lastUsedAt == nil
				if !newer {
					c, err := pyCompare(timestamp, entry.lastUsedAt)
					if err != nil {
						return nil, err
					}
					newer = c > 0
				}
				if newer {
					entry.lastUsedAt = timestamp
				}
			}
		}
	}
	return out, nil
}

// Message totals by role, zeros when the query returns no row
func (e *InsightsEngine) getMessageStats(ctx context.Context, cutoff float64, source string) (map[string]any, error) {
	rows, err := e.query(ctx, "message_stats", cutoff, source)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return map[string]any{"total_messages": int64(0), "user_messages": int64(0), "assistant_messages": int64(0), "tool_messages": int64(0)}, nil
	}
	return rows[0], nil
}

// Per model usage rows, none when the table is missing so the caller falls back to session aggregates
func (e *InsightsEngine) getModelUsage(ctx context.Context, cutoff float64, source string) ([]map[string]any, error) {
	rows, err := e.query(ctx, "model_usage", cutoff, source)
	if err != nil {
		if class, ok := state.SQLiteClassOf(err); ok && class == state.SQLiteOperationalError {
			return nil, nil
		}
		return nil, err
	}
	return rows, nil
}

func insightsTruthyTS(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok && f != 0
}

func insightsSum(rows []map[string]any, key string) (any, error) {
	var total any = int64(0)
	for _, r := range rows {
		var err error
		if total, err = pyArith("+", total, or0(r[key])); err != nil {
			return nil, err
		}
	}
	return total, nil
}

func insightsDiv(a any, n int) float64 {
	return numOf(a).Value() / float64(n)
}

// Totals, averages, cost buckets and date range across the sessions, tokens summed from the per model table when present
func insightsOverview(ctx context.Context, sessions []map[string]any, messageStats map[string]any, models []InsightsModel) (*InsightsOverview, error) {
	var tok [4]int64
	for i, k := range insightsTokenKeys {
		if len(models) > 0 {
			for _, m := range models {
				n, err := pyIntCell(or0(m.tokens()[i]))
				if err != nil {
					return nil, err
				}
				tok[i] += n
			}
			continue
		}
		for _, s := range sessions {
			n, err := pyIntCell(or0(s[k]))
			if err != nil {
				return nil, err
			}
			tok[i] += n
		}
	}
	totalTokens := tok[0] + tok[1] + tok[2] + tok[3]
	toolCalls, err := insightsSum(sessions, "tool_call_count")
	if err != nil {
		return nil, err
	}
	messages, err := insightsSum(sessions, "message_count")
	if err != nil {
		return nil, err
	}
	totalCost := 0.0
	var actual any = 0.0
	withPricing, withoutPricing := map[string]bool{}, map[string]bool{}
	statusCounts := map[string]int{}
	for _, s := range sessions {
		model := insightsCellStr(s["model"])
		estimated, status, err := insightsEstimateCost(ctx, model, s, insightsCellStr(s["billing_provider"]), insightsCellStr(s["billing_base_url"]))
		if err != nil {
			return nil, err
		}
		totalCost += estimated
		var cell any = 0.0
		if pyrepr.Truthy(s["actual_cost_usd"]) {
			cell = s["actual_cost_usd"]
		}
		if actual, err = pyArith("+", actual, cell); err != nil {
			return nil, err
		}
		statusCounts[status]++
		if provider.HasKnownPricing(ctx, model, insightsCellStr(s["billing_provider"]), insightsCellStr(s["billing_base_url"]), "") {
			withPricing[insightsShortModel(model)] = true
		} else {
			withoutPricing[insightsShortModel(model)] = true
		}
	}
	if len(models) > 0 {
		totalCost = 0
		for _, m := range models {
			totalCost += m.Cost
		}
	}
	var durations []float64
	var started []float64
	for _, s := range sessions {
		st, sok := insightsTruthyTS(s["started_at"])
		en, eok := insightsTruthyTS(s["ended_at"])
		if sok && eok && en > st {
			durations = append(durations, en-st)
		}
		if sok {
			started = append(started, st)
		}
	}
	n := len(sessions)
	o := &InsightsOverview{
		TotalSessions: n, TotalMessages: numOf(messages), TotalToolCalls: numOf(toolCalls),
		TotalInputTokens: tok[0], TotalOutputTokens: tok[1], TotalCacheReadTokens: tok[2], TotalCacheWriteTokens: tok[3],
		TotalTokens: totalTokens, EstimatedCost: totalCost, ActualCost: numOf(actual).Value(),
		AvgMessagesPerSession: insightsDiv(messages, n), AvgTokensPerSession: float64(totalTokens) / float64(n),
		UnknownCostSessions: statusCounts["unknown"], IncludedCostSessions: statusCounts["included"],
		ModelsWithPricing: insightsSortedKeys(withPricing), ModelsWithoutPricing: insightsSortedKeys(withoutPricing),
	}
	for _, f := range []struct {
		key string
		dst *InsightsNum
	}{{"user_messages", &o.UserMessages}, {"assistant_messages", &o.AssistantMessages}, {"tool_messages", &o.ToolMessages}} {
		*f.dst = numOf(or0(messageStats[f.key]))
	}
	if len(durations) > 0 {
		sum := 0.0
		for _, d := range durations {
			sum += d
		}
		o.TotalHours = sum / 3600
		o.AvgSessionDuration = sum / float64(len(durations))
	}
	if len(started) > 0 {
		lo, hi := started[0], started[0]
		for _, s := range started {
			lo, hi = min(lo, s), max(hi, s)
		}
		o.DateRangeStart, o.DateRangeEnd = &lo, &hi
	}
	return o, nil
}

func insightsSortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type insightsModelAcc struct {
	model      string
	sessions   map[string]bool
	tokens     [4]any
	reasoning  any
	total      any
	apiCalls   any
	toolCalls  any
	cost       float64
	actualCost float64
	costStatus string
	hasStatus  bool
	hasPricing bool
	pricingSet bool
}

var insightsCountKeys = append(append([]string{}, insightsTokenKeys...), "reasoning_tokens", "api_call_count")

func insightsPyMax0(v any) (any, error) {
	c, err := pyCompare(v, int64(0))
	if err != nil {
		return nil, err
	}
	if c > 0 {
		return v, nil
	}
	return int64(0), nil
}

// Tokens and cost per model from session_model_usage, sessions without per model rows falling back to their aggregate
func (e *InsightsEngine) computeModelBreakdown(ctx context.Context, sessions []map[string]any, cutoff float64, source string) ([]InsightsModel, error) {
	var order []string
	data := map[string]*insightsModelAcc{}
	entry := func(short string) *insightsModelAcc {
		d, ok := data[short]
		if !ok {
			d = &insightsModelAcc{model: short, sessions: map[string]bool{}, reasoning: int64(0), total: int64(0), apiCalls: int64(0), toolCalls: int64(0)}
			for i := range d.tokens {
				d.tokens[i] = int64(0)
			}
			data[short] = d
			order = append(order, short)
		}
		return d
	}
	accumulate := func(model, prov, baseURL, sessionID string, counts map[string]any, stored any, hasStored bool, actualCost any, costStatus string) error {
		if model == "" {
			model = "unknown"
		}
		d := entry(insightsShortModel(model))
		d.sessions[sessionID] = true
		var err error
		for i, k := range insightsTokenKeys {
			if d.tokens[i], err = pyArith("+=", d.tokens[i], counts[k]); err != nil {
				return err
			}
		}
		if d.reasoning, err = pyArith("+=", d.reasoning, counts["reasoning_tokens"]); err != nil {
			return err
		}
		var sum any = int64(0)
		for _, k := range insightsTokenKeys {
			if sum, err = pyArith("+", sum, counts[k]); err != nil {
				return err
			}
		}
		if d.total, err = pyArith("+=", d.total, sum); err != nil {
			return err
		}
		if d.apiCalls, err = pyArith("+=", d.apiCalls, counts["api_call_count"]); err != nil {
			return err
		}
		var estimate float64
		status := costStatus
		if !hasStored {
			if estimate, status, err = insightsEstimateCost(ctx, model, counts, prov, baseURL); err != nil {
				return err
			}
		} else {
			if pyrepr.Truthy(stored) {
				if estimate, err = pyFloat(stored); err != nil {
					return err
				}
			}
			if status == "" {
				status = "unknown"
			}
		}
		d.cost += estimate
		if pyrepr.Truthy(actualCost) {
			f, err := pyFloat(actualCost)
			if err != nil {
				return err
			}
			d.actualCost += f
		}
		d.costStatus, d.hasStatus = status, true
		d.hasPricing = provider.HasKnownPricing(ctx, model, prov, baseURL, "") || (d.pricingSet && d.hasPricing)
		d.pricingSet = true
		return nil
	}
	type usageTotals struct {
		counts    map[string]any
		estimated any
		actual    any
	}
	totalsBySession := map[string]*usageTotals{}
	totalsFor := func(id string) *usageTotals {
		t, ok := totalsBySession[id]
		if !ok {
			t = &usageTotals{counts: map[string]any{}, estimated: 0.0, actual: 0.0}
			for _, k := range insightsCountKeys {
				t.counts[k] = int64(0)
			}
			totalsBySession[id] = t
		}
		return t
	}
	usage, err := e.getModelUsage(ctx, cutoff, source)
	if err != nil {
		return nil, err
	}
	for _, r := range usage {
		sid := insightsCellStr(r["session_id"])
		totals := totalsFor(sid)
		counts := map[string]any{}
		for _, k := range insightsCountKeys {
			counts[k] = or0(r[k])
		}
		for _, k := range insightsCountKeys {
			if totals.counts[k], err = pyArith("+=", totals.counts[k], counts[k]); err != nil {
				return nil, err
			}
		}
		if totals.estimated, err = pyArith("+=", totals.estimated, insightsOr0f(r["estimated_cost_usd"])); err != nil {
			return nil, err
		}
		if totals.actual, err = pyArith("+=", totals.actual, insightsOr0f(r["actual_cost_usd"])); err != nil {
			return nil, err
		}
		hasStored := pyrepr.Truthy(r["cost_status"]) || pyrepr.Truthy(r["cost_source"])
		if err := accumulate(insightsCellStr(r["model"]), insightsCellStr(r["billing_provider"]), insightsCellStr(r["billing_base_url"]), sid, counts,
			r["estimated_cost_usd"], hasStored, r["actual_cost_usd"], insightsCellStr(r["cost_status"])); err != nil {
			return nil, err
		}
	}
	for _, s := range sessions {
		sid := insightsCellStr(s["id"])
		totals := totalsFor(sid)
		residual := map[string]any{"reasoning_tokens": int64(0)}
		anyResidual := false
		for _, k := range append(append([]string{}, insightsTokenKeys...), "api_call_count") {
			diff, err := pyArith("-", or0(s[k]), totals.counts[k])
			if err != nil {
				return nil, err
			}
			if residual[k], err = insightsPyMax0(diff); err != nil {
				return nil, err
			}
			anyResidual = anyResidual || pyrepr.Truthy(residual[k])
		}
		residualCost, err := insightsResidualFloat(s["estimated_cost_usd"], totals.estimated)
		if err != nil {
			return nil, err
		}
		residualActual, err := insightsResidualFloat(s["actual_cost_usd"], totals.actual)
		if err != nil {
			return nil, err
		}
		if anyResidual || residualCost != 0 || residualActual != 0 {
			if err := accumulate(insightsCellStr(s["model"]), insightsCellStr(s["billing_provider"]), insightsCellStr(s["billing_base_url"]), sid, residual,
				residualCost, true, residualActual, insightsCellStr(s["cost_status"])); err != nil {
				return nil, err
			}
		}
	}
	for _, s := range sessions {
		if pyrepr.Truthy(s["tool_call_count"]) {
			d := entry(insightsShortModel(insightsCellStr(s["model"])))
			if d.toolCalls, err = pyArith("+=", d.toolCalls, s["tool_call_count"]); err != nil {
				return nil, err
			}
		}
	}
	out := make([]InsightsModel, 0, len(order))
	for _, name := range order {
		d := data[name]
		m := InsightsModel{Model: name, Sessions: len(d.sessions), InputTokens: numOf(d.tokens[0]), OutputTokens: numOf(d.tokens[1]),
			CacheReadTokens: numOf(d.tokens[2]), CacheWriteTokens: numOf(d.tokens[3]), ReasoningTokens: numOf(d.reasoning),
			TotalTokens: numOf(d.total), APICalls: numOf(d.apiCalls), ToolCalls: numOf(d.toolCalls), Cost: d.cost,
			ActualCost: d.actualCost, CostStatus: "unknown", HasPricing: d.hasPricing}
		if d.hasStatus {
			m.CostStatus = d.costStatus
		}
		out = append(out, m)
	}
	var sortErr error
	sort.SliceStable(out, func(i, j int) bool {
		c, err := pyCompare(out[i].TotalTokens, out[j].TotalTokens)
		if err != nil && sortErr == nil {
			sortErr = err
		}
		if c != 0 {
			return c > 0
		}
		return out[i].Sessions > out[j].Sessions
	})
	return out, sortErr
}

func insightsOr0f(v any) any {
	if pyrepr.Truthy(v) {
		return v
	}
	return 0.0
}

// max(0.0, float(cell or 0.0) - attributed) of a session cost column
func insightsResidualFloat(cell, attributed any) (float64, error) {
	f := 0.0
	if pyrepr.Truthy(cell) {
		var err error
		if f, err = pyFloat(cell); err != nil {
			return 0, err
		}
	}
	diff, err := pyArith("-", f, attributed)
	if err != nil {
		return 0, err
	}
	return max(0.0, numOf(diff).Value()), nil
}

// Sessions, messages, tokens and tool calls per source platform, busiest first
func insightsPlatformBreakdown(sessions []map[string]any) ([]InsightsPlatform, error) {
	type acc struct {
		sessions int
		messages any
		tokens   [4]any
		total    any
		tools    any
	}
	var order []string
	data := map[string]*acc{}
	for _, s := range sessions {
		platform := insightsCellStr(s["source"])
		if !pyrepr.Truthy(s["source"]) {
			platform = "unknown"
		}
		d, ok := data[platform]
		if !ok {
			d = &acc{messages: int64(0), tokens: [4]any{int64(0), int64(0), int64(0), int64(0)}, total: int64(0), tools: int64(0)}
			data[platform] = d
			order = append(order, platform)
		}
		d.sessions++
		var err error
		if d.messages, err = pyArith("+=", d.messages, or0(s["message_count"])); err != nil {
			return nil, err
		}
		for i, k := range insightsTokenKeys {
			if d.tokens[i], err = pyArith("+=", d.tokens[i], or0(s[k])); err != nil {
				return nil, err
			}
			if d.total, err = pyArith("+=", d.total, or0(s[k])); err != nil {
				return nil, err
			}
		}
		if d.tools, err = pyArith("+=", d.tools, or0(s["tool_call_count"])); err != nil {
			return nil, err
		}
	}
	out := make([]InsightsPlatform, 0, len(order))
	for _, p := range order {
		d := data[p]
		out = append(out, InsightsPlatform{Platform: p, Sessions: d.sessions, Messages: numOf(d.messages), InputTokens: numOf(d.tokens[0]),
			OutputTokens: numOf(d.tokens[1]), CacheReadTokens: numOf(d.tokens[2]), CacheWriteTokens: numOf(d.tokens[3]),
			TotalTokens: numOf(d.total), ToolCalls: numOf(d.tools)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Sessions > out[j].Sessions })
	return out, nil
}

// Ranked tool list with each tool's share of all calls
func insightsToolBreakdown(usage []insightsToolCount) []InsightsTool {
	var total int64
	for _, t := range usage {
		total += t.count
	}
	out := make([]InsightsTool, 0, len(usage))
	for _, t := range usage {
		pct := 0.0
		if total != 0 {
			pct = float64(t.count) / float64(total) * 100
		}
		out = append(out, InsightsTool{Tool: t.name, Count: t.count, Percentage: pct})
	}
	return out
}

// Skill load and edit summary plus the ranked skill list
func insightsSkillBreakdown(usage []*insightsSkillCount) (InsightsSkills, error) {
	var loads, edits int64
	for _, s := range usage {
		loads += s.viewCount
		edits += s.manageCount
	}
	actions := loads + edits
	top := make([]InsightsSkill, 0, len(usage))
	for _, s := range usage {
		total := s.viewCount + s.manageCount
		pct := 0.0
		if actions != 0 {
			pct = float64(total) / float64(actions) * 100
		}
		top = append(top, InsightsSkill{Skill: s.skill, ViewCount: s.viewCount, ManageCount: s.manageCount, TotalCount: total,
			Percentage: pct, LastUsedAt: s.lastUsedAt})
	}
	var sortErr error
	sort.SliceStable(top, func(i, j int) bool {
		a, b := top[i], top[j]
		if a.TotalCount != b.TotalCount {
			return a.TotalCount > b.TotalCount
		}
		if a.ViewCount != b.ViewCount {
			return a.ViewCount > b.ViewCount
		}
		if a.ManageCount != b.ManageCount {
			return a.ManageCount > b.ManageCount
		}
		var la, lb any = a.LastUsedAt, b.LastUsedAt
		if !pyrepr.Truthy(la) {
			la = int64(0)
		}
		if !pyrepr.Truthy(lb) {
			lb = int64(0)
		}
		c, err := pyCompare(la, lb)
		if err != nil {
			if sortErr == nil {
				sortErr = err
			}
			return false
		}
		if c != 0 {
			return c > 0
		}
		return a.Skill > b.Skill
	})
	if sortErr != nil {
		return InsightsSkills{}, sortErr
	}
	return InsightsSkills{TotalSkillLoads: loads, TotalSkillEdits: edits, TotalSkillActions: actions, DistinctSkillsUsed: len(usage), TopSkills: top}, nil
}

var insightsDayNames = []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

// Sessions by weekday and hour plus the longest run of consecutive active days
func insightsActivityPatterns(sessions []map[string]any) *InsightsActivity {
	var dayCounts [7]int
	var hourCounts [24]int
	daily := map[string]int{}
	for _, s := range sessions {
		ts, ok := insightsTruthyTS(s["started_at"])
		if !ok {
			continue
		}
		dt := pydatetime.FromTimestamp(ts)
		dayCounts[(int(dt.Weekday())+6)%7]++
		hourCounts[dt.Hour()]++
		daily[dt.Format("2006-01-02")]++
	}
	a := &InsightsActivity{ActiveDays: len(daily)}
	for i, name := range insightsDayNames {
		a.ByDay = append(a.ByDay, InsightsDayCount{Day: name, Count: dayCounts[i]})
	}
	for i := range 24 {
		a.ByHour = append(a.ByHour, InsightsHourCount{Hour: i, Count: hourCounts[i]})
	}
	a.BusiestDay, a.BusiestHour = a.ByDay[0], a.ByHour[0]
	for _, d := range a.ByDay {
		if d.Count > a.BusiestDay.Count {
			a.BusiestDay = d
		}
	}
	for _, h := range a.ByHour {
		if h.Count > a.BusiestHour.Count {
			a.BusiestHour = h
		}
	}
	if len(daily) > 0 {
		keys := make([]string, 0, len(daily))
		for k := range daily {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		current := 1
		a.MaxStreak = 1
		for i := 1; i < len(keys); i++ {
			prev, _ := time.Parse("2006-01-02", keys[i-1])
			cur, _ := time.Parse("2006-01-02", keys[i])
			if int(cur.Sub(prev).Hours()/24) == 1 {
				current++
			} else {
				current = 1
			}
			a.MaxStreak = max(a.MaxStreak, current)
		}
	}
	return a
}

func insightsRuneSlice(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// Longest session and the sessions with the most messages, tokens and tool calls
func insightsTopSessions(sessions []map[string]any) ([]InsightsTopSession, error) {
	top := []InsightsTopSession{}
	var longest map[string]any
	var longestDur float64
	for _, s := range sessions {
		st, sok := insightsTruthyTS(s["started_at"])
		en, eok := insightsTruthyTS(s["ended_at"])
		if sok && eok && (longest == nil || en-st > longestDur) {
			longest, longestDur = s, en-st
		}
	}
	if longest != nil {
		top = append(top, InsightsTopSession{Label: "Longest session", SessionID: insightsRuneSlice(insightsCellStr(longest["id"]), 16),
			Value: provider.FormatDurationCompact(longestDur), Date: insightsDay(longest["started_at"])})
	}
	metrics := []struct {
		label  string
		metric func(map[string]any) (any, error)
		format func(any) string
	}{
		{"Most messages", func(s map[string]any) (any, error) { return or0(s["message_count"]), nil }, func(v any) string { return pyrepr.Str(v) + " msgs" }},
		{"Most tokens", func(s map[string]any) (any, error) {
			return pyArith("+", or0(s["input_tokens"]), or0(s["output_tokens"]))
		},
			func(v any) string { return pyFormat(v, ",") + " tokens" }},
		{"Most tool calls", func(s map[string]any) (any, error) { return or0(s["tool_call_count"]), nil }, func(v any) string { return pyrepr.Str(v) + " calls" }},
	}
	for _, m := range metrics {
		var best map[string]any
		var bestVal any
		for _, s := range sessions {
			v, err := m.metric(s)
			if err != nil {
				return nil, err
			}
			if best == nil {
				best, bestVal = s, v
				continue
			}
			c, err := pyCompare(v, bestVal)
			if err != nil {
				return nil, err
			}
			if c > 0 {
				best, bestVal = s, v
			}
		}
		if c, err := pyCompare(bestVal, int64(0)); err != nil {
			return nil, err
		} else if c > 0 {
			top = append(top, InsightsTopSession{Label: m.label, SessionID: insightsRuneSlice(insightsCellStr(best["id"]), 16), Value: m.format(bestVal),
				Date: insightsDay(best["started_at"])})
		}
	}
	return top, nil
}
