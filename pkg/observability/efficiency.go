package observability

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	tokenThresholds = []threshold{{2_000, "lt_2k"}, {10_000, "2k_to_10k"}, {50_000, "10k_to_50k"},
		{200_000, "50k_to_200k"}, {1_000_000, "200k_to_1m"}}
	activityThresholds = []threshold{{1, "0"}, {2, "1"}, {3, "2"}, {6, "3_to_5"}, {11, "6_to_10"},
		{26, "11_to_25"}, {51, "26_to_50"}, {101, "51_to_100"}}
	outputSizeThresholds = []threshold{{1_000, "lt_1k"}, {10_000, "1k_to_10k"}, {50_000, "10k_to_50k"},
		{100_000, "50k_to_100k"}, {500_000, "100k_to_500k"}}
	schemaTokenThresholds = []threshold{{1, "0"}, {2_000, "lt_2k"}, {5_000, "2k_to_5k"}, {10_000, "5k_to_10k"},
		{20_000, "10k_to_20k"}, {40_000, "20k_to_40k"}}
	taskCostOutcomes = map[string]string{"success": "completed", "cancelled": "interrupted"}
)

const (
	// CacheTTLSeconds is the provider prompt cache lifetime past which a cold read is plain expiry
	CacheTTLSeconds     = 300.0
	spentTurnsKept      = 64
	pendingOutputsKey   = "_shared_metrics_tool_outputs"
	pendingOutputsMax   = 256
	noticeMinChars      = 1_000
	anonymousOutputMark = "\x00anonymous-output-"
)

var toolTruncationNotice = regexp.MustCompile(`\[[A-Z_ ]{1,32} TRUNCATED - [\p{Nd},]{1,20} chars omitted out of ([\p{Nd},]{1,20}) total\]`)

// EstimateToolsTokensHook is the rough schema token estimate, set by pkg/provider to EstimateToolsTokens
var EstimateToolsTokensHook func(defs []map[string]any) int

// IdentityLineValueHook reads one identity line of a system prompt, set by pkg/agent from surface_switch
var IdentityLineValueHook func(prompt, label string) string

// Token bucket of a turn, unknown for nil
func TokensBucket(tokens *int) string {
	if tokens == nil {
		return "unknown"
	}
	return bucket(float64(max(0, *tokens)), tokenThresholds, "gte_1m")
}

// Bucket of a model or tool call count
func ActivityBucket(count int) string {
	return bucket(float64(max(0, count)), activityThresholds, "gte_101")
}

// Bucket of a tool output size in characters, unknown for nil
func OutputSizeBucket(size *int) string {
	if size == nil {
		return "unknown"
	}
	return bucket(float64(max(0, *size)), outputSizeThresholds, "gte_500k")
}

// Bucket of the estimated tool schema tokens a request carries
func SchemaTokensBucket(tokens int) string {
	return bucket(float64(max(0, tokens)), schemaTokenThresholds, "gte_40k")
}

// Truncated integer of a non negative number, nil otherwise
func nonNegativeInt(value any) *int {
	number, ok := nonNegativeNumber(value)
	if !ok {
		return nil
	}
	n := int(number)
	return &n
}

// Prompt plus completion tokens of one call, cache reads and writes included, nil without usage
func CallTokens(usage any) *int {
	d, ok := asDict(usage)
	if !ok {
		return nil
	}
	if total := nonNegativeInt(d["total_tokens"]); total != nil {
		return total
	}
	sum, any := 0, false
	for _, key := range []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens"} {
		if p := nonNegativeInt(d[key]); p != nil {
			sum, any = sum+*p, true
		}
	}
	if !any {
		return nil
	}
	return &sum
}

// A shipped toolset's own name, custom for MCP servers, plugins and user toolsets
func ToolsetMetricName(toolset any) string {
	if name, ok := toolset.(string); ok {
		if _, shipped := tools.BuiltinToolsetNames[name]; shipped {
			return name
		}
	}
	return "custom"
}

// Copy of a route, the unknown route for an empty one
func routeOrUnknown(route map[string]string) map[string]string {
	if len(route) > 0 {
		return maps.Clone(route)
	}
	return ModelRoute(nil, nil)
}

// MarkRow is one process mark a runtime helper hands back for the caller to record
type MarkRow struct {
	Mark string
	Dims map[string]string
}

// TurnCost is the token spend of one task, user turn marking a turn a user message started
type TurnCost struct {
	UserTurn bool
	Tokens   int
	Known    bool
	Route    map[string]string
}

// Adds one call's tokens and remembers its route
func (c *TurnCost) AddCall(route map[string]string, usage any) {
	c.Route = route
	if tokens := CallTokens(usage); tokens != nil {
		c.Tokens += *tokens
		c.Known = true
	}
}

// Token bucket of the task, unknown when no call reported usage
func (c *TurnCost) Bucket() string {
	if !c.Known {
		return TokensBucket(nil)
	}
	return TokensBucket(&c.Tokens)
}

// SpentTurn is one finished user turn an undo or retry can still throw away
type SpentTurn struct {
	Route        map[string]string
	TokensBucket string
	Wasted       bool
}

// Wasted tokens row for one thrown away turn
func wastedRow(reason string, turn *SpentTurn) MarkRow {
	dims := maps.Clone(turn.Route)
	dims["reason"], dims["tokens_bucket"] = reason, turn.TokensBucket
	return MarkRow{WastedTokensMark, dims}
}

// ToolUsage is what the session paid to carry tool definitions and which shipped toolsets it touched
type ToolUsage struct {
	Observed        bool
	Surface         string
	EnabledToolsets map[string]bool
	EnabledCount    int
	SchemaTokens    int
	Names           []string
	HasNames        bool
	Used            map[string]bool
}

// Empty tool usage on the unknown surface
func NewToolUsage() *ToolUsage {
	return &ToolUsage{Surface: "unknown", EnabledToolsets: map[string]bool{}, Used: map[string]bool{}}
}

// Records the tools one request sent and reports whether a later request changed the array
func (u *ToolUsage) Observe(names []string, toolsets map[string]bool, enabledCount, schemaTokens int) bool {
	changed := u.HasNames && !slices.Equal(names, u.Names)
	u.Observed, u.Names, u.HasNames = true, slices.Clone(names), true
	for ts := range toolsets {
		u.EnabledToolsets[ts] = true
	}
	u.EnabledCount, u.SchemaTokens = max(u.EnabledCount, enabledCount), schemaTokens
	return changed
}

// Folds a child session's observed tool usage into this one
func (u *ToolUsage) Absorb(other *ToolUsage) {
	if other == nil || !other.Observed {
		return
	}
	u.Observed, u.Surface = true, other.Surface
	for ts := range other.EnabledToolsets {
		u.EnabledToolsets[ts] = true
	}
	u.EnabledCount = max(u.EnabledCount, other.EnabledCount)
	u.SchemaTokens = max(u.SchemaTokens, other.SchemaTokens)
	for name := range other.Used {
		u.Used[name] = true
	}
}

// Tool overhead row and one enabled unused row per enabled shipped toolset, nothing when unobserved
func (u *ToolUsage) Rows() []MarkRow {
	if !u.Observed {
		return nil
	}
	rows := []MarkRow{{ToolOverheadMark, map[string]string{
		"enabled_tool_count_bucket": SizeBucket(u.EnabledCount),
		"execution_surface":         u.Surface,
		"tool_schema_tokens_bucket": SchemaTokensBucket(u.SchemaTokens),
	}}}
	for _, name := range slices.Sorted(maps.Keys(u.EnabledToolsets)) {
		used := "no"
		if u.Used[name] {
			used = "yes"
		}
		rows = append(rows, MarkRow{ToolEnabledUnusedMark, map[string]string{"toolset": name, "used": used}})
	}
	return rows
}

// SessionEfficiency is the per session spend, tool overhead and prompt cache state of the relay runtime
type SessionEfficiency struct {
	Spent        *SpentTurns
	Tools        *ToolUsage
	ToolsKey     any
	CacheRead    int
	CacheRoute   map[string]string
	CacheEndedNS int64
}

// Empty session efficiency state
func NewSessionEfficiency() *SessionEfficiency {
	return &SessionEfficiency{Tools: NewToolUsage(), Spent: &SpentTurns{}}
}

// Newest spent turns one conversation keeps, shared by every segment compression rotated
type SpentTurns struct {
	mu    sync.Mutex
	Items []*SpentTurn
}

// Task cost row for one user turn the user saw end, an interrupt also counting its tokens wasted
func (e *SessionEfficiency) FinishTurn(cost *TurnCost, terminal, route map[string]string, modelCalls, toolCalls int) []MarkRow {
	r := cost.Route
	if len(r) == 0 {
		r = route
	}
	turn := &SpentTurn{Route: routeOrUnknown(r), TokensBucket: cost.Bucket()}
	outcome, ok := taskCostOutcomes[terminal["outcome"]]
	if !ok {
		outcome = "failed"
	}
	dims := maps.Clone(turn.Route)
	dims["api_calls_bucket"], dims["outcome"] = ActivityBucket(modelCalls), outcome
	dims["tokens_bucket"], dims["tool_calls_bucket"] = turn.TokensBucket, ActivityBucket(toolCalls)
	rows := []MarkRow{{TaskCostMark, dims}}
	if terminal["end_reason"] == "user_cancelled" {
		turn.Wasted = true
		rows = append(rows, wastedRow("interrupt", turn))
	}
	e.Spent.mu.Lock()
	defer e.Spent.mu.Unlock()
	e.Spent.Items = append(e.Spent.Items, turn)
	if len(e.Spent.Items) > spentTurnsKept {
		e.Spent.Items = e.Spent.Items[len(e.Spent.Items)-spentTurnsKept:]
	}
	return rows
}

// Wasted rows for the newest turns an undo or retry threw away, skipping turns already counted
func (e *SessionEfficiency) DiscardTurns(reason string, turns int, fallbackRoute map[string]string) []MarkRow {
	var rows []MarkRow
	e.Spent.mu.Lock()
	defer e.Spent.mu.Unlock()
	for range max(1, min(turns, spentTurnsKept)) {
		var turn *SpentTurn
		if n := len(e.Spent.Items); n > 0 {
			turn, e.Spent.Items = e.Spent.Items[n-1], e.Spent.Items[:n-1]
		} else {
			turn = &SpentTurn{Route: routeOrUnknown(fallbackRoute), TokensBucket: "unknown"}
		}
		if !turn.Wasted {
			rows = append(rows, wastedRow(reason, turn))
		}
	}
	return rows
}

// Cause of an unannounced cold primary read after a warm one on the same route, false otherwise
func (e *SessionEfficiency) ObserveCache(route map[string]string, usage any, startedNS, endedNS int64, expected bool) (string, bool) {
	d, ok := asDict(usage)
	if !ok {
		return "", false
	}
	read := nonNegativeInt(d["cache_read_tokens"])
	if read == nil {
		return "", false
	}
	prevRead, prevRoute, prevEnded := e.CacheRead, e.CacheRoute, e.CacheEndedNS
	e.CacheRead, e.CacheRoute, e.CacheEndedNS = *read, route, endedNS
	if expected || *read != 0 || prevRead == 0 || prevRoute == nil || !maps.Equal(prevRoute, route) {
		return "", false
	}
	if float64(startedNS-prevEnded)/1e9 >= CacheTTLSeconds {
		return "cache_expired", true
	}
	return "provider_reported_miss", true
}

// Cache break row for a cause on a route
func CacheBreakRow(cause string, route map[string]string) MarkRow {
	dims := routeOrUnknown(route)
	dims["cause"] = cause
	return MarkRow{CacheBreakMark, dims}
}

// Reports whether the agent is an icebath owned fork sharing the user's session id
func internalAgent(agent MetricsAgent) bool {
	return agent != nil && agent.MemoryWriteOrigin() == "background_review"
}

// Catalog route of the agent's provider and model
func agentRoute(agent MetricsAgent) map[string]string {
	return ModelRoute(agentProviderModel(agent))
}

// pendingOutputs is the ordered set of committed tool results awaiting their batch's turn budget
type pendingOutputs struct {
	mu      sync.Mutex
	keys    []string
	entries map[string]*pendingOutput
	anon    int
}

type pendingOutput struct {
	tool      string
	size      *int
	truncated bool
}

// Reports whether two values are the same object, strings and scalars comparing by value
func sameObject(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if va.Type() != vb.Type() {
		return false
	}
	switch va.Kind() {
	case reflect.Map, reflect.Pointer, reflect.Slice, reflect.Func, reflect.Chan:
		return va.Pointer() == vb.Pointer() && (va.Kind() != reflect.Slice || va.Len() == vb.Len())
	}
	return pyrepr.Equal(a, b)
}

// Remembers one committed tool result until its batch's turn budget has run
func NoteToolResult(ctx context.Context, agent MetricsAgent, toolName, toolCallID, raw, persisted any) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Shared-metrics tool output not noted", "error", r)
		}
	}()
	if internalAgent(agent) || !relayEnabled(ctx) {
		return
	}
	value, _ := agent.MetricsScratch().LoadOrStore(pendingOutputsKey, &pendingOutputs{entries: map[string]*pendingOutput{}})
	pending := value.(*pendingOutputs)
	pending.mu.Lock()
	defer pending.mu.Unlock()
	if len(pending.entries) >= pendingOutputsMax {
		return
	}
	size, cutInTool := resultSize(raw)
	key := pyText(toolCallID)
	if key == "" {
		pending.anon++
		key = fmt.Sprintf("%s%d", anonymousOutputMark, pending.anon)
	}
	if _, seen := pending.entries[key]; !seen {
		pending.keys = append(pending.keys, key)
	}
	pending.entries[key] = &pendingOutput{tool: ToolMetricName(map[string]any{"tool_name": toolName}), size: size,
		truncated: cutInTool || !sameObject(persisted, raw)}
}

// Characters a tool produced and whether the tool itself already truncated them
func resultSize(result any) (*int, bool) {
	if d, ok := asDict(result); ok {
		parts, _ := d["content"].([]any)
		total := 0
		for _, p := range parts {
			part, ok := asDict(p)
			if !ok || part["type"] != "text" {
				continue
			}
			switch text := part["text"].(type) {
			case string:
				total += pystr.Len(text)
			case []any:
				total += len(text)
			}
		}
		return &total, false
	}
	s, ok := result.(string)
	if !ok {
		return nil, false
	}
	length := pystr.Len(s)
	var m []string
	if length >= noticeMinChars {
		m = toolTruncationNotice.FindStringSubmatch(s)
	}
	if m == nil {
		return &length, false
	}
	stated := 0
	for _, r := range m[1] {
		if digit, ok := pystr.DecimalValue(r); ok {
			stated = stated*10 + digit
		}
	}
	size := max(length, stated)
	return &size, true
}

// One truncation row per tool result of the batch, truncated when a cap or the turn budget replaced it
func RecordToolBatch(ctx context.Context, agent MetricsAgent, batch []map[string]any, contentsBefore []any) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Shared-metrics tool output truncation not recorded", "error", r)
		}
	}()
	value, loaded := agent.MetricsScratch().LoadAndDelete(pendingOutputsKey)
	if !loaded {
		return
	}
	pending := value.(*pendingOutputs)
	pending.mu.Lock()
	defer pending.mu.Unlock()
	if len(pending.entries) == 0 || !relayEnabled(ctx) {
		return
	}
	for i := 0; i < len(batch) && i < len(contentsBefore); i++ {
		entry := pending.entries[pyText(batch[i]["tool_call_id"])]
		if entry != nil && !sameObject(batch[i]["content"], contentsBefore[i]) {
			entry.truncated = true
		}
	}
	for _, key := range pending.keys {
		entry := pending.entries[key]
		truncated := "no"
		if entry.truncated {
			truncated = "yes"
		}
		recordProcessMark(ctx, ToolOutputTruncationMark, map[string]string{
			"original_size_bucket": OutputSizeBucket(entry.size), "tool": entry.tool, "truncated": truncated})
	}
}

// Observes the tool definitions one primary request carries
func ObserveRequestTools(ctx context.Context, agent MetricsAgent, toolsForAPI []map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Shared-metrics tool overhead not observed", "error", r)
		}
	}()
	if agent.PersistDisabled() || internalAgent(agent) || !relayEnabled(ctx) {
		return
	}
	if toolsForAPI == nil {
		toolsForAPI = []map[string]any{}
	}
	RecordSessionTools(ctx, agent.SessionID(), agent, toolsForAPI)
}

// Sent tool names, enabled shipped toolsets, enabled tool count and estimated schema tokens of a request
func ToolSnapshot(ctx context.Context, agent MetricsAgent, toolsForAPI []map[string]any) ([]string, map[string]bool, int, int, error) {
	names := toolNames(toolsForAPI)
	source := toolsForAPI
	var extra map[string]bool
	if agent != nil {
		if defs := agent.ToolDefinitions(); len(defs) > 0 {
			source = defs
		}
		extra = agent.ValidToolNames()
	}
	enabledNames := map[string]bool{}
	for _, n := range toolNames(source) {
		enabledNames[n] = true
	}
	for n := range extra {
		enabledNames[n] = true
	}
	toolsets := map[string]bool{}
	for n := range enabledNames {
		toolsets[ToolsetMetricName(tools.Default.ToolsetForTool(ctx, "", n))] = true
	}
	schemaTokens := 0
	if len(toolsForAPI) > 0 {
		if EstimateToolsTokensHook == nil {
			return nil, nil, 0, 0, fmt.Errorf("cannot import name '_estimate_tools_tokens_rough'")
		}
		schemaTokens = EstimateToolsTokensHook(toolsForAPI)
	}
	return names, toolsets, len(enabledNames), schemaTokens, nil
}

// Names of tool definitions in the function wrapped or flat form
func toolNames(defs []map[string]any) []string {
	var names []string
	for _, tool := range defs {
		var name any
		if fn, ok := asDict(tool["function"]); ok {
			name = fn["name"]
		} else {
			name = tool["name"]
		}
		if s, ok := name.(string); ok {
			names = append(names, s)
		}
	}
	return names
}

// Counts icebath itself invalidating the conversation's cached prefix
func RecordCacheBreak(ctx context.Context, agent MetricsAgent, cause string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Shared-metrics cache break not recorded", "error", r)
		}
	}()
	if !CacheBreakCauses.Has(cause) || (agent != nil && agent.PersistDisabled()) {
		return
	}
	if internalAgent(agent) || !relayEnabled(ctx) {
		return
	}
	sessionID := ""
	if agent != nil {
		sessionID = agent.SessionID()
	}
	RecordKnownCacheBreak(ctx, cause, agentRoute(agent), sessionID)
}

// Counts a continuing conversation rebuilding its system prompt instead of replaying the stored bytes
func RecordPromptRebuild(ctx context.Context, agent MetricsAgent, storedPrompt any, storedState string, rebuilt any) {
	if storedState == "null" || storedState == "empty" {
		recordCacheBreak(ctx, agent, "system_prompt_rebuild")
		return
	}
	if storedState != "stale_runtime" || !pyrepr.Truthy(storedPrompt) || pyrepr.Equal(rebuilt, storedPrompt) {
		return
	}
	switched := func() (out bool) {
		defer func() {
			if r := recover(); r != nil {
				out = false
			}
		}()
		if IdentityLineValueHook == nil {
			return false
		}
		prompt := pyrepr.Str(storedPrompt)
		for _, pair := range [][2]string{{"Model", "model"}, {"Provider", "provider"}} {
			stored := IdentityLineValueHook(prompt, pair[0])
			current := ""
			if agent != nil {
				current = agent.Model()
				if pair[1] == "provider" {
					current = agent.Provider()
				}
			}
			if stored != "" && stored != pystr.Strip(current) {
				return true
			}
		}
		return false
	}()
	cause := "system_prompt_rebuild"
	if switched {
		cause = "model_switch"
	}
	recordCacheBreak(ctx, agent, cause)
}

// Cache break counter the prompt rebuild check calls, replaceable like the Python module attribute
var recordCacheBreak = RecordCacheBreak
