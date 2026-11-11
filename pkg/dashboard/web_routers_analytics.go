package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Raw config.yaml text of the requested profile plus the path it resolves to
func getConfigRaw(ctx context.Context, profile string) (*pyjson.Object, error) {
	supported, err := config.SupportsFileTooling(ctx)
	if err != nil {
		return nil, err
	}
	if !supported {
		return nil, NewHTTPException(409, "This config backend has no raw config file")
	}
	sctx, _, release, err := profileScope(ctx, profile)
	if err != nil {
		return nil, err
	}
	path := home.ConfigPath(home.Dir(sctx))
	release()
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return pyjson.ObjectOf("yaml", "", "path", path), nil
	}
	if err != nil {
		return nil, err
	}
	text, err := pystr.DecodeUTF8SigFile(raw)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("yaml", text, "path", path), nil
}

// Replaces the whole config of the requested profile with a YAML mapping, broadcasting an approvals mode change
func updateConfigRaw(ctx context.Context, body *pyjson.Object, profile string) (*pyjson.Object, error) {
	yamlText, _ := body.GetOr("yaml_text", "").(string)
	parsed, err := config.SafeLoad(yamlText)
	if err != nil {
		if config.IsYAMLError(err) {
			return nil, NewHTTPException(400, "Invalid YAML: "+err.Error())
		}
		return nil, err
	}
	doc, ok := parsed.(map[string]any)
	if !ok {
		return nil, NewHTTPException(400, "YAML must be a mapping")
	}
	target := profile
	if bodyProfile, _ := body.GetOr("profile", nil).(string); bodyProfile != "" {
		target = bodyProfile
	}
	changed, err := func() (bool, error) {
		sctx, _, release, err := profileScope(ctx, target)
		if err != nil {
			return false, err
		}
		defer release()
		existing, err := config.ReadRawConfig(sctx, home.Dir(sctx))
		if err != nil {
			return false, err
		}
		changed := approvalModeOf(doc) != approvalModeOf(existing)
		return changed, config.SaveConfig(sctx, home.Dir(sctx), doc, config.SaveOptions{MergeExisting: false})
	}()
	if err != nil {
		return nil, err
	}
	if changed {
		other, err := isOtherProfile(target)
		if err != nil {
			return nil, err
		}
		if !other {
			broadcastGatewaySessionInfo(ctx)
		}
	}
	return pyjson.ObjectOf("ok", true), nil
}

// Rows of a query bound to the cutoff
func analyticsRows(ctx context.Context, db *state.SessionDB, query string, cutoff float64) ([]*pyjson.Object, error) {
	return db.ReadAll(ctx, query, cutoff)
}

// Object rows as a JSON list
func objectList(rows []*pyjson.Object) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r
	}
	return out
}

// Cutoff epoch of a window of days ending now
func analyticsCutoff(days int64) float64 {
	return float64(time.Now().UnixNano())/1e9 - float64(days*86400)
}

// Tool and skill breakdown of the insights engine as the dicts agent insights returns
func usageBreakdownDict(b *memory.InsightsUsageBreakdown) (tools []any, skills *pyjson.Object) {
	var totalCalls int64
	for _, t := range b.Tools {
		totalCalls += t.Count
	}
	tools = make([]any, len(b.Tools))
	for i, t := range b.Tools {
		var pct any = int64(0)
		if totalCalls != 0 {
			pct = t.Percentage
		}
		tools[i] = pyjson.ObjectOf("tool", t.Tool, "count", t.Count, "percentage", pct)
	}
	top := make([]any, len(b.Skills.TopSkills))
	for i, s := range b.Skills.TopSkills {
		var pct any = int64(0)
		if b.Skills.TotalSkillActions != 0 {
			pct = s.Percentage
		}
		top[i] = pyjson.ObjectOf("skill", s.Skill, "view_count", s.ViewCount, "manage_count", s.ManageCount,
			"total_count", s.TotalCount, "percentage", pct, "last_used_at", s.LastUsedAt)
	}
	skills = pyjson.ObjectOf(
		"summary", pyjson.ObjectOf("total_skill_loads", b.Skills.TotalSkillLoads, "total_skill_edits", b.Skills.TotalSkillEdits,
			"total_skill_actions", b.Skills.TotalSkillActions, "distinct_skills_used", int64(b.Skills.DistinctSkillsUsed)),
		"top_skills", top)
	return tools, skills
}

// Daily, per model, per task and total token usage of the window with the tool and skill breakdown
func usageAnalytics(ctx context.Context, days int64, profile *string) (*pyjson.Object, error) {
	db, err := openSessionDBForProfile(ctx, profile, true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	cutoff := analyticsCutoff(days)
	daily, err := analyticsRows(ctx, db, `
            SELECT date(started_at, 'unixepoch', 'localtime') as day,
                   SUM(input_tokens) as input_tokens,
                   SUM(output_tokens) as output_tokens,
                   SUM(cache_read_tokens) as cache_read_tokens,
                   SUM(reasoning_tokens) as reasoning_tokens,
                   COALESCE(SUM(estimated_cost_usd), 0) as estimated_cost,
                   COALESCE(SUM(actual_cost_usd), 0) as actual_cost,
                   COUNT(*) as sessions,
                   SUM(COALESCE(api_call_count, 0)) as api_calls
            FROM sessions WHERE started_at > ?
            GROUP BY day ORDER BY day
        `, cutoff)
	if err != nil {
		return nil, err
	}
	byModel, err := analyticsRows(ctx, db, `
            SELECT model,
                   SUM(input_tokens) as input_tokens,
                   SUM(output_tokens) as output_tokens,
                   COALESCE(SUM(estimated_cost_usd), 0) as estimated_cost,
                   COUNT(*) as sessions,
                   SUM(COALESCE(api_call_count, 0)) as api_calls
            FROM sessions WHERE started_at > ? AND model IS NOT NULL
            GROUP BY model ORDER BY SUM(input_tokens) + SUM(output_tokens) DESC
        `, cutoff)
	if err != nil {
		return nil, err
	}
	auxRows := auxUsageRows(ctx, db, cutoff)
	byModel = mergeAuxIntoByModel(byModel, auxRows)
	totals, err := analyticsRows(ctx, db, `
            SELECT SUM(input_tokens) as total_input,
                   SUM(output_tokens) as total_output,
                   SUM(cache_read_tokens) as total_cache_read,
                   SUM(reasoning_tokens) as total_reasoning,
                   COALESCE(SUM(estimated_cost_usd), 0) as total_estimated_cost,
                   COALESCE(SUM(actual_cost_usd), 0) as total_actual_cost,
                   COUNT(*) as total_sessions,
                   SUM(COALESCE(api_call_count, 0)) as total_api_calls
            FROM sessions WHERE started_at > ?
        `, cutoff)
	if err != nil {
		return nil, err
	}
	engine, err := memory.OpenInsightsEngine(db)
	if err != nil {
		return nil, err
	}
	defer engine.Close()
	usage, err := engine.GetUsageBreakdown(ctx, int(days), "")
	if err != nil {
		return nil, err
	}
	tools, skills := usageBreakdownDict(usage)
	return pyjson.ObjectOf("daily", objectList(daily), "by_model", objectList(byModel),
		"by_task", objectList(auxTaskSummary(auxRows)), "totals", totals[0], "period_days", days,
		"skills", skills, "tools", tools), nil
}

// Usage analytics of a window clamped to 1 to 365 days, a corrupt store answered as a status payload
func getUsageAnalytics(ctx context.Context, days int64, profile *string) (any, error) {
	dbPath, err := sessionDBPathForProfile(ctx, profile)
	if err != nil {
		return nil, err
	}
	out, err := usageAnalytics(ctx, days, profile)
	if err != nil {
		return nil, corruptStoreAsStatus(dbPath, err)
	}
	return out, nil
}

var usageKeys = []string{"input_tokens", "output_tokens", "cache_read_tokens", "reasoning_tokens",
	"estimated_cost", "actual_cost", "api_calls", "tool_calls"}

// Whether a row carries any nonzero usage counter
func hasUsage(row *pyjson.Object) bool {
	for _, key := range usageKeys {
		if pyrepr.Truthy(row.GetOr(key, nil)) {
			return true
		}
	}
	return false
}

// Larger of two numbers, the first on a tie, the Python max of two counters
func numMax(a, b any) any {
	if counterFloat(b) > counterFloat(a) {
		return b
	}
	return a
}

// Truthy value of a key or the fallback, the row.get(key) or fallback read
func valueOr(row *pyjson.Object, key string, fallback any) any {
	return pyrepr.Or(row.GetOr(key, nil), fallback)
}

// Ordered groups of rows by a key, the setdefault append of a dict of lists
type rowGroups struct {
	keys   []string
	groups map[string][]*pyjson.Object
}

func (g *rowGroups) add(key string, row *pyjson.Object) {
	if g.groups == nil {
		g.groups = map[string][]*pyjson.Object{}
	}
	if _, held := g.groups[key]; !held {
		g.keys = append(g.keys, key)
	}
	g.groups[key] = append(g.groups[key], row)
}

// Key of a model and billing provider pair compared the way a Python tuple is
func pairKey(model, billingProvider any) string {
	return pyrepr.Repr(model) + "\x00" + pyrepr.Repr(billingProvider)
}

// Folds usage free rows without a billing provider into the single provider row of their model
func foldSessionOnlyRows(rawRows []*pyjson.Object) []*pyjson.Object {
	var byModel rowGroups
	for _, row := range rawRows {
		byModel.add(pyrepr.Repr(valueOr(row, "model", "")), row)
	}
	rows := []*pyjson.Object{}
	for _, key := range byModel.keys {
		modelRows := byModel.groups[key]
		var providerRows []*pyjson.Object
		for _, r := range modelRows {
			if pyrepr.Truthy(r.GetOr("billing_provider", nil)) {
				providerRows = append(providerRows, r)
			}
		}
		if len(providerRows) != 1 {
			rows = append(rows, modelRows...)
			continue
		}
		target := providerRows[0]
		for _, row := range modelRows {
			if row == target || pyrepr.Truthy(row.GetOr("billing_provider", nil)) || hasUsage(row) {
				continue
			}
			target.Set("sessions", addCounter(counterOr0(target, "sessions"), counterOr0(row, "sessions")))
			target.Set("last_used_at", numMax(counterOr0(target, "last_used_at"), counterOr0(row, "last_used_at")))
			totalTokens := addCounter(counterOr0(target, "input_tokens"), counterOr0(target, "output_tokens"))
			sessions := counterOr0(target, "sessions")
			if pyrepr.Truthy(sessions) {
				target.Set("avg_tokens_per_session", counterFloat(totalTokens)/counterFloat(sessions))
			} else {
				target.Set("avg_tokens_per_session", int64(0))
			}
		}
		rows = append(rows, target)
		for _, r := range modelRows {
			if r != target && (pyrepr.Truthy(r.GetOr("billing_provider", nil)) || hasUsage(r)) {
				rows = append(rows, r)
			}
		}
	}
	return rows
}

// Capability metadata of a model card from the models.dev cache, empty when unknown
func modelCapabilities(ctx context.Context, billingProvider, model string) *pyjson.Object {
	mc, found := provider.ModelsDevCapabilities(ctx, billingProvider, model, false)
	if !found {
		return pyjson.NewObject()
	}
	var vision, reasoning, maxOutput any
	if mc.SupportsVision != nil {
		vision = *mc.SupportsVision
	}
	if mc.SupportsReasoning != nil {
		reasoning = *mc.SupportsReasoning
	}
	if mc.MaxOutputTokens > 0 {
		maxOutput = int64(mc.MaxOutputTokens)
	}
	return pyjson.ObjectOf("supports_tools", mc.SupportsTools, "supports_vision", vision, "supports_reasoning", reasoning,
		"context_window", int64(mc.ContextWindow), "max_output_tokens", maxOutput, "model_family", mc.ModelFamily)
}

var auxSummedKeys = []string{"input_tokens", "output_tokens", "cache_read_tokens", "reasoning_tokens", "estimated_cost", "api_calls"}

// Adds auxiliary usage onto the matching model and provider row, an aux only pair getting its own row
func mergeAuxIntoRows(rawRows []*pyjson.Object, auxRows []*pyjson.Object) []*pyjson.Object {
	index := map[string]*pyjson.Object{}
	for _, row := range rawRows {
		index[pairKey(valueOr(row, "model", ""), valueOr(row, "billing_provider", ""))] = row
	}
	for _, aux := range auxRows {
		model, billing := valueOr(aux, "model", "unknown"), valueOr(aux, "billing_provider", "")
		key := pairKey(model, billing)
		target := index[key]
		if target == nil {
			target = pyjson.ObjectOf("model", model, "billing_provider", billing)
			for _, k := range auxSummedKeys {
				target.Set(k, int64(0))
			}
			target.Set("actual_cost", int64(0))
			target.Set("sessions", counterOr0(aux, "sessions"))
			target.Set("tool_calls", int64(0))
			target.Set("last_used_at", nil)
			target.Set("avg_tokens_per_session", int64(0))
			index[key] = target
			rawRows = append(rawRows, target)
		}
		for _, k := range auxSummedKeys {
			target.Set(k, addCounter(counterOr0(target, k), counterOr0(aux, k)))
		}
		if last := aux.GetOr("last_used_at", nil); last != nil {
			target.Set("last_used_at", numMax(counterOr0(target, "last_used_at"), last))
		}
		if sessions := counterOr0(target, "sessions"); pyrepr.Truthy(sessions) {
			tokens := addCounter(counterOr0(target, "input_tokens"), counterOr0(target, "output_tokens"))
			target.Set("avg_tokens_per_session", counterFloat(tokens)/counterFloat(sessions))
		}
	}
	return rawRows
}

var modelCardKeys = []string{"input_tokens", "output_tokens", "cache_read_tokens", "reasoning_tokens",
	"estimated_cost", "actual_cost", "sessions", "api_calls", "tool_calls", "last_used_at", "avg_tokens_per_session"}

// Session level tool call totals attributed to the card of each model and provider pair, else of the model
func attachToolCalls(ctx context.Context, db *state.SessionDB, cutoff float64, rawRows []*pyjson.Object) error {
	pairRows, err := analyticsRows(ctx, db, `
        SELECT model, billing_provider, SUM(COALESCE(tool_call_count, 0)) as tool_calls
        FROM sessions WHERE started_at > ? AND model IS NOT NULL AND model != ''
        GROUP BY model, billing_provider
    `, cutoff)
	if err != nil {
		return err
	}
	type pair struct {
		model any
		calls any
	}
	pairKeys := []string{}
	byPair := map[string]pair{}
	for _, r := range pairRows {
		model := valueOr(r, "model", "")
		key := pairKey(model, valueOr(r, "billing_provider", ""))
		if _, held := byPair[key]; !held {
			pairKeys = append(pairKeys, key)
		}
		byPair[key] = pair{model, counterOr0(r, "tool_calls")}
	}
	index := map[string]*pyjson.Object{}
	var byModel rowGroups
	for _, row := range rawRows {
		row.Set("tool_calls", int64(0))
		model := row.GetOr("model", nil)
		key := pairKey(model, valueOr(row, "billing_provider", ""))
		if _, held := index[key]; !held {
			index[key] = row
		}
		byModel.add(pyrepr.Repr(model), row)
	}
	for _, key := range pairKeys {
		p := byPair[key]
		target := index[key]
		if target == nil {
			if rows := byModel.groups[pyrepr.Repr(p.model)]; len(rows) > 0 {
				target = rows[0]
			}
		}
		if target != nil {
			target.Set("tool_calls", addCounter(target.GetOr("tool_calls", nil), p.calls))
		}
	}
	return nil
}

// Per model token, cost and session breakdown of the window with models.dev capability metadata
func modelsAnalytics(ctx context.Context, days int64, profile *string) (*pyjson.Object, error) {
	db, err := openSessionDBForProfile(ctx, profile, true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	cutoff := analyticsCutoff(days)
	rawRows, err := analyticsRows(ctx, db, `
                SELECT u.model,
                       u.billing_provider,
                       SUM(u.input_tokens) as input_tokens,
                       SUM(u.output_tokens) as output_tokens,
                       SUM(u.cache_read_tokens) as cache_read_tokens,
                       SUM(u.reasoning_tokens) as reasoning_tokens,
                       COALESCE(SUM(u.estimated_cost_usd), 0) as estimated_cost,
                       COALESCE(SUM(u.actual_cost_usd), 0) as actual_cost,
                       COUNT(DISTINCT u.session_id) as sessions,
                       SUM(COALESCE(u.api_call_count, 0)) as api_calls,
                       MAX(u.last_seen) as last_used_at,
                       AVG(u.input_tokens + u.output_tokens) as avg_tokens_per_session
                FROM session_model_usage u
                JOIN sessions s ON s.id = u.session_id
                WHERE s.started_at > ? AND u.model IS NOT NULL AND u.model != ''
                      AND u.task = ''
                GROUP BY u.model, u.billing_provider
                ORDER BY SUM(u.input_tokens) + SUM(u.output_tokens) DESC
            `, cutoff)
	if err != nil {
		if class, ok := state.SQLiteClassOf(err); !ok || class != state.SQLiteOperationalError {
			return nil, err
		}
		rawRows = nil
	}
	if len(rawRows) == 0 {
		rawRows, err = analyticsRows(ctx, db, `
                SELECT model,
                       billing_provider,
                       SUM(input_tokens) as input_tokens,
                       SUM(output_tokens) as output_tokens,
                       SUM(cache_read_tokens) as cache_read_tokens,
                       SUM(reasoning_tokens) as reasoning_tokens,
                       COALESCE(SUM(estimated_cost_usd), 0) as estimated_cost,
                       COALESCE(SUM(actual_cost_usd), 0) as actual_cost,
                       COUNT(*) as sessions,
                       SUM(COALESCE(api_call_count, 0)) as api_calls,
                       SUM(tool_call_count) as tool_calls,
                       MAX(started_at) as last_used_at,
                       AVG(input_tokens + output_tokens) as avg_tokens_per_session
                FROM sessions WHERE started_at > ? AND model IS NOT NULL AND model != ''
                GROUP BY model, billing_provider
                ORDER BY SUM(input_tokens) + SUM(output_tokens) DESC
            `, cutoff)
		if err != nil {
			return nil, err
		}
	} else if err := attachToolCalls(ctx, db, cutoff, rawRows); err != nil {
		return nil, err
	}
	rawRows = mergeAuxIntoRows(rawRows, auxUsageRows(ctx, db, cutoff))
	rows := foldSessionOnlyRows(rawRows)
	sort.SliceStable(rows, func(i, j int) bool { return tokenVolume(rows[i]) > tokenVolume(rows[j]) })
	models := make([]any, 0, len(rows))
	distinct := map[string]bool{}
	for _, row := range rows {
		model, ok := row.Get("model")
		if !ok {
			return nil, fmt.Errorf("KeyError: 'model'")
		}
		billing := valueOr(row, "billing_provider", "")
		card := pyjson.ObjectOf("model", model, "provider", billing)
		for _, key := range modelCardKeys {
			v, ok := row.Get(key)
			if !ok {
				return nil, fmt.Errorf("KeyError: %s", pyrepr.Repr(key))
			}
			card.Set(key, v)
		}
		card.Set("capabilities", modelCapabilities(ctx, pyrepr.Str(billing), pyrepr.Str(model)))
		models = append(models, card)
		distinct[pyrepr.Repr(model)] = true
	}
	totals, err := analyticsRows(ctx, db, `
            SELECT SUM(input_tokens) as total_input,
                   SUM(output_tokens) as total_output,
                   SUM(cache_read_tokens) as total_cache_read,
                   SUM(reasoning_tokens) as total_reasoning,
                   COALESCE(SUM(estimated_cost_usd), 0) as total_estimated_cost,
                   COALESCE(SUM(actual_cost_usd), 0) as total_actual_cost,
                   COUNT(*) as total_sessions,
                   SUM(COALESCE(api_call_count, 0)) as total_api_calls
            FROM sessions WHERE started_at > ? AND model IS NOT NULL AND model != ''
        `, cutoff)
	if err != nil {
		return nil, err
	}
	total := totals[0]
	total.Set("distinct_models", int64(len(distinct)))
	return pyjson.ObjectOf("models", models, "totals", total, "period_days", days), nil
}

// Model analytics of a window clamped to 1 to 365 days, a corrupt store answered as a status payload
func getModelsAnalytics(ctx context.Context, days int64, profile *string) (any, error) {
	dbPath, err := sessionDBPathForProfile(ctx, profile)
	if err != nil {
		return nil, err
	}
	out, err := modelsAnalytics(ctx, days, profile)
	if err != nil {
		return nil, corruptStoreAsStatus(dbPath, err)
	}
	return out, nil
}

// Router of the raw config editor and the usage and model analytics
func analyticsRouter() *APIRouter {
	r := NewAPIRouter("")
	profileParam := QueryParam("profile", Optional(Str), nil)
	days := QueryParam("days", IntRange(Bound(1), Bound(365)), int64(30))
	r.Get("/api/config/raw", &Endpoint{
		Name:   "get_config_raw",
		Params: []Param{profileParam},
		Handle: func(ctx context.Context, c *Call) (any, error) { return getConfigRaw(ctx, c.Str("profile")) },
	})
	r.Put("/api/config/raw", &Endpoint{
		Name:   "update_config_raw",
		Params: []Param{profileParam},
		Body:   []BodyParam{Body("body", rawConfigUpdateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return updateConfigRaw(ctx, c.Object("body"), c.Str("profile"))
		},
	})
	r.Get("/api/analytics/usage", &Endpoint{
		Name:   "get_usage_analytics",
		Params: []Param{days, profileParam},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return getUsageAnalytics(ctx, c.Int("days"), optionalStr(c, "profile"))
		},
	})
	r.Get("/api/analytics/models", &Endpoint{
		Name:   "get_models_analytics",
		Params: []Param{days, profileParam},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return getModelsAnalytics(ctx, c.Int("days"), optionalStr(c, "profile"))
		},
	})
	return r
}
