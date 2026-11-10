package tools

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// The connector search route answers 7 use cases per request and fails on 8 or more
const (
	toolSearchMaxQueriesPerCall       = 7
	toolSearchMaxDescribeNamesPerCall = 10
)

// ConnectorBatchSentinel is the dispatch unit name a connector only tool_call batch resolves to
const ConnectorBatchSentinel = "connectors__execute"

// ConnectorNamePrefix marks hosted connector tool names
const ConnectorNamePrefix = "connectors__"

// IsConnectorName reports whether a tool name belongs to the hosted connector surface by prefix
func IsConnectorName(name string) bool {
	return strings.HasPrefix(name, ConnectorNamePrefix)
}

// ToolSearchConfig is the resolved and validated tool search configuration for one assembly
type ToolSearchConfig struct {
	Enabled            string
	ThresholdPct       float64
	SearchDefaultLimit int
	MaxSearchLimit     int
	Listing            string
	ListingMaxTokens   int
	// Nil keeps the curated default while an explicit set replaces it wholesale
	DeferTools map[string]bool
}

// EffectiveDeferTools returns the curated default set unless the config named its own list
func (c ToolSearchConfig) EffectiveDeferTools() map[string]bool {
	if c.DeferTools == nil {
		return DefaultDeferredTools()
	}
	return c.DeferTools
}

// DefaultDeferredTools is the curated deferral set kept in the config defaults so discovery and runtime agree
func DefaultDeferredTools() map[string]bool {
	out := map[string]bool{}
	for _, n := range config.AsStrings(config.Defaults().Get("tools.tool_search.defer")) {
		if strings.TrimSpace(n) != "" {
			out[strings.TrimSpace(n)] = true
		}
	}
	return out
}

var toolSearchTriStateAliases = map[string]string{
	"true": "on", "1": "on", "yes": "on", "false": "off", "0": "off", "no": "off",
}

// Normalizes an auto, on, or off setting accepting bool like aliases
func toolSearchTriState(value any) string {
	text := strings.ToLower(strings.TrimSpace(fmt.Sprint(value)))
	if v, ok := toolSearchTriStateAliases[text]; ok {
		return v
	}
	switch text {
	case "auto", "on", "off":
		return text
	}
	return "auto"
}

// Integer value or the fallback when unparseable, clamped to the range
func toolSearchClampedInt(value any, fallback, lo, hi int) int {
	v, ok := toolSearchAsInt(value)
	if !ok {
		v = fallback
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Python int() semantics over JSON and YAML scalars
func toolSearchAsInt(value any) (int, bool) {
	switch t := value.(type) {
	case nil:
		return 0, false
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return 0, false
		}
		return int(t), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		if err != nil {
			return 0, false
		}
		return int(n), true
	}
	return config.AsInt(value)
}

func toolSearchSafeFloat(value any, fallback float64) float64 {
	switch t := value.(type) {
	case nil:
		return fallback
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return fallback
		}
		return f
	}
	if f, ok := config.AsFloat(value); ok {
		return f
	}
	return fallback
}

// ToolSearchConfigFromRaw builds the config from a raw map, legacy bool, or nil with every field clamped
func ToolSearchConfigFromRaw(raw any) ToolSearchConfig {
	m, ok := raw.(map[string]any)
	if !ok {
		enabled := "auto"
		if b, isBool := raw.(bool); isBool && !b {
			enabled = "off"
		}
		m = map[string]any{"enabled": enabled}
	}
	maxSearchLimit := toolSearchClampedInt(m["max_search_limit"], 25, 1, 50)
	var deferTools map[string]bool
	if deferRaw, present := m["defer"]; present && deferRaw != nil {
		list, isList := toolSearchAsList(deferRaw)
		if !isList {
			slog.Warn(fmt.Sprintf("tools.tool_search.defer is %v, expected a YAML list of tool names (e.g. [todo_list, computer_use]; [] keeps every tool eager) - using the curated default set.", deferRaw))
		} else {
			deferTools = map[string]bool{}
			for _, n := range list {
				if s := strings.TrimSpace(fmt.Sprint(n)); s != "" {
					deferTools[s] = true
				}
			}
		}
	}
	enabledRaw, has := m["enabled"]
	if !has {
		enabledRaw = "auto"
	}
	listingRaw, has := m["listing"]
	if !has {
		listingRaw = "auto"
	}
	return ToolSearchConfig{
		Enabled:            toolSearchTriState(enabledRaw),
		ThresholdPct:       math.Max(0, math.Min(100, toolSearchSafeFloat(m["threshold_pct"], 5.0))),
		SearchDefaultLimit: toolSearchClampedInt(m["search_default_limit"], 5, 1, maxSearchLimit),
		MaxSearchLimit:     maxSearchLimit,
		Listing:            toolSearchTriState(listingRaw),
		ListingMaxTokens:   toolSearchClampedInt(m["listing_max_tokens"], 4000, 200, 60000),
		DeferTools:         deferTools,
	}
}

func toolSearchAsList(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out, true
	case map[string]any:
		out := make([]any, 0, len(t))
		for _, k := range sortedKeys(t) {
			out = append(out, k)
		}
		return out, true
	}
	return nil, false
}

// LoadToolSearchConfig reads tools.tool_search from the session config with defaults on any failure
func LoadToolSearchConfig(env *Env) ToolSearchConfig {
	if env == nil || env.Config == nil {
		return ToolSearchConfigFromRaw(nil)
	}
	tools, ok := env.Config.Get("tools").(map[string]any)
	if !ok {
		return ToolSearchConfigFromRaw(nil)
	}
	return ToolSearchConfigFromRaw(tools["tool_search"])
}

// Names that never defer by default
func toolSearchCoreToolNames() map[string]bool {
	out := make(map[string]bool, len(CoreTools))
	for _, n := range CoreTools {
		out[n] = true
	}
	return out
}

// Session gated GUI toolsets stay direct once enabled unless the deferral list names them
func toolSearchDirectSurfaceToolset(toolset string) bool {
	if ClientSurfaceToolsets[toolset] {
		return true
	}
	_, gated := ToolsetSessionPlatforms[toolset]
	return gated
}

// IsDeferrableToolName reports whether a tool is eligible for deferral under the given defer set
func IsDeferrableToolName(ctx context.Context, env *Env, name string, deferTools map[string]bool) bool {
	if BridgeToolNames[name] {
		return false
	}
	if deferTools != nil && deferTools[name] {
		return true
	}
	if toolSearchCoreToolNames()[name] {
		return false
	}
	toolset, ok := newSearchScope(ctx, env).toolset(name)
	if !ok {
		return false
	}
	return strings.HasPrefix(toolset, "mcp-") || !toolSearchDirectSurfaceToolset(toolset)
}

// SessionToolNames returns every tool name in the session's tool defs
func SessionToolNames(toolDefs []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, td := range toolDefs {
		if n := toolDefName(td); n != "" {
			out[n] = true
		}
	}
	return out
}

// ClassifyTools splits tool defs into visible and deferrable with bridge tools dropped
func ClassifyTools(ctx context.Context, env *Env, toolDefs []map[string]any, deferTools map[string]bool) (visible, deferrable []map[string]any) {
	for _, td := range toolDefs {
		name := toolDefName(td)
		if BridgeToolNames[name] {
			continue
		}
		if IsDeferrableToolName(ctx, env, name, deferTools) {
			deferrable = append(deferrable, td)
		} else {
			visible = append(visible, td)
		}
	}
	return visible, deferrable
}

// Deferrable subset of pre assembly tool defs under the session config
func toolSearchDeferrableIn(ctx context.Context, env *Env, toolDefs []map[string]any) []map[string]any {
	_, deferrable := ClassifyTools(ctx, env, toolDefs, LoadToolSearchConfig(env).EffectiveDeferTools())
	return deferrable
}

// EstimateTokensFromSchemas estimates token cost through the chars per token rule
func EstimateTokensFromSchemas(toolDefs []map[string]any) int {
	chars := 0
	for _, td := range toolDefs {
		chars += utf8.RuneCountInString(jsonCompact(td))
	}
	return int(math.Ceil(float64(chars) / CharsPerToken))
}

// ShouldActivate reports whether the bridge replaces deferrable tools for this assembly
func ShouldActivate(cfg ToolSearchConfig, deferrableTokens int, contextLength int, connectionsGranted bool) bool {
	if cfg.Enabled == "off" {
		return false
	}
	if deferrableTokens > 0 {
		return true
	}
	return connectionsGranted
}

// ListingTokenBudget is the smaller of the listing cap and the threshold share of the context
func ListingTokenBudget(cfg ToolSearchConfig, contextLength int) int {
	pctLeg := 10000
	if contextLength > 0 {
		pctLeg = int(float64(contextLength) * (cfg.ThresholdPct / 100.0))
	}
	budget := cfg.ListingMaxTokens
	if pctLeg < budget {
		budget = pctLeg
	}
	if budget < 0 {
		return 0
	}
	return budget
}

// One OpenAI style function schema for a bridge tool
func toolSearchBridgeSchema(name, description string, properties map[string]any, required []string) map[string]any {
	req := make([]any, len(required))
	for i, r := range required {
		req[i] = r
	}
	return map[string]any{"type": "function", "function": map[string]any{
		"name": name, "description": description,
		"parameters": map[string]any{"type": "object", "properties": properties, "required": req}}}
}

const toolSearchConnectionsHint = " Searches also cover the user's connected app accounts, whose tools are hosted and so " +
	"never listed here (results named `connectors__<app>__<tool>`); `manage_connections` checks " +
	"or connects an account."

// tool_search bridge description with the listing embedded and framed per listing form
func toolSearchDescription(deferredCount int, listing, listingForm string, connectionsGranted bool) string {
	var desc string
	if deferredCount > 0 {
		desc = fmt.Sprintf("Search %d additional tools that are loaded on demand. ", deferredCount)
	} else {
		desc = "Search remote connector tools (email, calendars, issue trackers, and more). "
	}
	desc += "Send one keyword query per capability you need; returns matching tool names " +
		fmt.Sprintf("and descriptions. Then `%s` loads schemas and `%s` ", ToolDescribeName, ToolCallName) +
		"invokes. Tools already in your tool list need no search."
	if connectionsGranted {
		desc += toolSearchConnectionsHint
	}
	if listing == "" {
		return desc
	}
	if listingForm == "groups" {
		return desc + "\n\nThe servers below are connected and their tools ARE available " +
			"through this bridge. For any request in these domains, search " +
			"here FIRST — do not claim the capability is unavailable and do " +
			"not substitute a generic tool (terminal/browser) without " +
			"searching.\n\n" + listing
	}
	desc += "\n\nEvery tool listed below exists: never call it unavailable, and go straight to " +
		fmt.Sprintf("`%s` when you see the exact name.", ToolDescribeName)
	if listingForm == "mixed" {
		desc += " For servers marked 'names not listed', the tools exist " +
			fmt.Sprintf("too — find them with `%s` before ", ToolSearchName) +
			"concluding anything is missing."
	}
	return desc + "\n\n" + listing
}

// BridgeToolSchemas returns the three bridge schemas injected in place of deferred tools
func BridgeToolSchemas(deferredCount int, listing, listingForm string, connectionsGranted bool) []map[string]any {
	return []map[string]any{
		toolSearchBridgeSchema(ToolSearchName,
			toolSearchDescription(deferredCount, listing, listingForm, connectionsGranted),
			map[string]any{
				"queries": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "One query per capability: service + action + object keywords (e.g. ['gmail send email', 'github create issue']), never a question; every word must match tool text. Searched in parallel.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Max matches per query (default 5).",
				},
			},
			[]string{"queries"}),
		toolSearchBridgeSchema(ToolDescribeName,
			fmt.Sprintf("Load the full JSON schemas for tools returned by `%s`. ", ToolSearchName)+
				fmt.Sprintf("Required before `%s` if a tool's parameters are unknown. ", ToolCallName)+
				"Batch every schema you need into one call.",
			map[string]any{
				"names": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Exact tool names (as returned by tool_search). A single string is accepted and treated as one name.",
				},
			},
			[]string{"names"}),
		toolSearchBridgeSchema(ToolCallName,
			"Invoke deferred tools: `calls` is an array of {name, arguments}, one entry per "+
				"invocation (batch independent calls; results come back in order). Arguments match "+
				fmt.Sprintf("the schema from `%s`; approvals and hooks apply as usual.", ToolDescribeName),
			map[string]any{
				"calls": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name":      map[string]any{"type": "string", "description": "Exact tool name to invoke."},
							"arguments": map[string]any{"type": "object", "description": "Arguments matching the tool schema."},
						},
						"required": []any{"name", "arguments"},
					},
					"description": "One or more independent deferred tool invocations.",
				},
			},
			[]string{"calls"}),
	}
}

// AssemblyResult is the outcome of one assembly for tests and observability
type AssemblyResult struct {
	ToolDefs        []map[string]any
	Activated       bool
	DeferredCount   int
	DeferredTokens  int
	ThresholdTokens int
	// 0 passthrough, 1 bridge with per tool listing, 2 bare bridge or server summary only
	Tier        int
	ListingForm string
}

// Reports whether manage_connections is in the session's tool defs
func connectionsInScope(toolDefs []map[string]any) bool {
	for _, td := range toolDefs {
		if toolDefName(td) == "manage_connections" {
			return true
		}
	}
	return false
}

// AssembleToolDefs replaces deferrable tools by the bridge tools when active, stripping stale bridges first
func AssembleToolDefs(ctx context.Context, env *Env, toolDefs []map[string]any, contextLength int, cfg *ToolSearchConfig) AssemblyResult {
	var c ToolSearchConfig
	if cfg != nil {
		c = *cfg
	} else {
		c = LoadToolSearchConfig(env)
	}
	incoming := make([]map[string]any, 0, len(toolDefs))
	for _, td := range toolDefs {
		if !BridgeToolNames[toolDefName(td)] {
			incoming = append(incoming, td)
		}
	}
	visible, deferrable := ClassifyTools(ctx, env, incoming, c.EffectiveDeferTools())
	connectionsGranted := connectionsInScope(incoming)
	if len(deferrable) == 0 {
		if ShouldActivate(c, 0, contextLength, connectionsGranted) {
			return AssemblyResult{ToolDefs: append(append([]map[string]any{}, incoming...), BridgeToolSchemas(0, "", "", connectionsGranted)...),
				Activated: true, Tier: 2, ListingForm: "none"}
		}
		return AssemblyResult{ToolDefs: incoming, ListingForm: "none"}
	}
	deferrableTokens := EstimateTokensFromSchemas(deferrable)
	if !ShouldActivate(c, deferrableTokens, contextLength, false) {
		return AssemblyResult{ToolDefs: incoming, DeferredCount: len(deferrable), DeferredTokens: deferrableTokens,
			ThresholdTokens: int(float64(contextLength) * (c.ThresholdPct / 100.0)), ListingForm: "none"}
	}
	listing, listingForm := "", "none"
	listingBudget := ListingTokenBudget(c, contextLength)
	if c.Listing != "off" {
		listing, listingForm = BuildCatalogListingWithForm(ctx, env, deferrable, listingBudget)
	}
	bridge := BridgeToolSchemas(len(deferrable), listing, listingForm, connectionsGranted)
	tier := 2
	switch listingForm {
	case "full", "names", "mixed":
		tier = 1
	}
	slog.Info(fmt.Sprintf("tool_search activated (tier %d): %d core/visible tools kept, %d deferred (~%d tokens), listing %s (budget ~%d tokens)",
		tier, len(visible), len(deferrable), deferrableTokens, listingForm, listingBudget))
	return AssemblyResult{ToolDefs: append(append([]map[string]any{}, visible...), bridge...), Activated: true,
		DeferredCount: len(deferrable), DeferredTokens: deferrableTokens, ThresholdTokens: listingBudget,
		Tier: tier, ListingForm: listingForm}
}

// IsBridgeTool reports whether the name is one of the three bridge tools
func IsBridgeTool(name string) bool { return BridgeToolNames[name] }

// Caps a record description marking the cut so it reads as deliberate
func toolSearchClipDescription(text string, cap int) string {
	if utf8.RuneCountInString(text) <= cap {
		return text
	}
	return string([]rune(text)[:cap]) + "…"
}

// One record for the shared tools map carrying required names for trivial calls
func toolSearchSharedToolRecord(entry CatalogEntry) *pyjson.Object {
	required := []any{}
	if fn, ok := entry.Schema["function"].(map[string]any); ok {
		if params, ok := fn["parameters"].(map[string]any); ok {
			if list, ok := params["required"].([]any); ok {
				for _, r := range list {
					s, ok := r.(string)
					if !ok {
						continue
					}
					if runes := pystr.Runes(s); len(runes) > 64 {
						s = pystr.FromRunes(runes[:64])
					}
					required = append(required, s)
					if len(required) == 32 {
						break
					}
				}
			}
		}
	}
	return Dict("source", entry.Source, "source_name", entry.SourceName,
		"description", toolSearchClipDescription(entry.Description, 500), "required", required)
}

// Deterministic summaries of connected and declared unavailable sources
func toolSearchAvailableSourceSummary(ctx context.Context, env *Env, catalog []CatalogEntry) []*pyjson.Object {
	counts := map[string]int{}
	for _, entry := range catalog {
		counts[catalogListingGroupLabel(entry.SourceName)]++
	}
	rows := make([]*pyjson.Object, 0, len(counts))
	for _, name := range sortedStringKeys(counts) {
		rows = append(rows, Dict("name", name, "tool_count", counts[name]))
	}
	for _, hidden := range HiddenDeclaredSources(ctx, env) {
		rows = append(rows, Dict("name", hidden.Name, "tool_count", hidden.ToolCount, "unavailable", hidden.Unavailable))
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Vals["name"].(string) < rows[j].Vals["name"].(string) })
	return rows
}

func sortedStringKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Reads a list of strings bridge argument accepting a bare string as a one item list
func toolSearchStringListArg(args map[string]any, key string, dedupe bool, maxItems int, retryHint string) ([]string, string) {
	var raw []any
	switch t := args[key].(type) {
	case string:
		raw = []any{t}
	case []any:
		raw = t
	case []string:
		raw, _ = toolSearchAsList(t)
	default:
		return nil, toolErrorJSON(fmt.Sprintf("%s is required and must be an array of strings", key), nil)
	}
	var out []string
	for _, item := range raw {
		text := ""
		if item != nil {
			text = strings.TrimSpace(fmt.Sprint(item))
		}
		if text == "" {
			continue
		}
		if dedupe {
			dup := false
			for _, existing := range out {
				if existing == text {
					dup = true
					break
				}
			}
			if dup {
				continue
			}
		}
		out = append(out, text)
	}
	if len(out) == 0 {
		return nil, toolErrorJSON(fmt.Sprintf("%s is required and must contain at least one non-empty string", key), nil)
	}
	if len(out) > maxItems {
		return nil, toolErrorJSON(fmt.Sprintf("too many %s: %d > max %d. %s", key, len(out), maxItems, retryHint), nil)
	}
	return out, ""
}

// DispatchToolSearch answers a tool_search bridge call over the session's deferrable catalog
func DispatchToolSearch(ctx context.Context, env *Env, args map[string]any, currentToolDefs []map[string]any, cfg *ToolSearchConfig, call *Call) string {
	var c ToolSearchConfig
	if cfg != nil {
		c = *cfg
	} else {
		c = LoadToolSearchConfig(env)
	}
	queries, errJSON := toolSearchStringListArg(args, "queries", false, toolSearchMaxQueriesPerCall, "Retry with fewer, more targeted queries.")
	if errJSON != "" {
		toolSearchRecordMetric(ctx, env, currentToolDefs, nil, 0, "error")
		return errJSON
	}
	limit := c.SearchDefaultLimit
	if rawLimit, has := args["limit"]; has && rawLimit != nil {
		limit = toolSearchClampedInt(rawLimit, c.SearchDefaultLimit, 1, c.MaxSearchLimit)
	}
	catalog := BuildCatalog(ctx, env, toolSearchDeferrableIn(ctx, env, currentToolDefs))
	remoteEntries := make([][]CatalogEntry, len(queries))
	hostedFailure := ""
	if connectionsInScope(currentToolDefs) {
		remoteEntries, hostedFailure = connectorEntriesByGroup(ctx, env, queries, call)
	}
	results := make([]*pyjson.Object, 0, len(queries))
	toolsMap := Dict()
	availableSources := toolSearchAvailableSourceSummary(ctx, env, catalog)
	for position, query := range queries {
		corpus := append(append([]CatalogEntry{}, catalog...), remoteEntries[position]...)
		hits := SearchCatalog(corpus, query, limit, nil)
		matches := make([]any, 0, len(hits))
		for _, h := range hits {
			if _, seen := toolsMap.Get(h.Name); !seen {
				toolsMap.Set(h.Name, toolSearchSharedToolRecord(h))
			}
			matches = append(matches, h.Name)
		}
		group := Dict("query", query, "matches", matches)
		if len(matches) == 0 && len(availableSources) > 0 {
			group.Set("available_sources", availableSources)
			group.Set("hint", "This query returned no lexical matches, but the sources above "+
				"are connected and their tools remain available. Retry "+
				"tool_search with the service name plus a concrete action or "+
				"object before concluding the capability is unavailable.")
		}
		results = append(results, group)
	}
	remoteCount := 0
	for _, name := range toolsMap.Keys {
		if IsConnectorName(name) {
			remoteCount++
		}
	}
	payload := Dict("queries", queries, "total_available", len(catalog)+remoteCount,
		"results", results, "tools", toolsMap)
	if hostedFailure != "" {
		payload.Set("connectors", connectorsUnavailable(env, hostedFailure, "searched", nil))
	}
	outcome := "no_hits"
	if hostedFailure != "" {
		outcome = "connector_unavailable"
	} else if toolsMap.Len() > 0 {
		outcome = "hits"
	}
	toolSearchRecordMetric(ctx, env, currentToolDefs, catalog, toolsMap.Len(), outcome)
	return JSON(payload)
}

// Emits one icebath.tool_search.count metric event naming the corpora read, never the query
func toolSearchRecordMetric(ctx context.Context, env *Env, currentToolDefs []map[string]any, catalog []CatalogEntry, hits int, outcome string) {
	local := len(catalog) > 0
	if catalog == nil {
		local = len(toolSearchDeferrableIn(ctx, env, currentToolDefs)) > 0
	}
	hosted := connectionsInScope(currentToolDefs)
	scope := "local"
	if local && hosted {
		scope = "both"
	} else if hosted {
		scope = "connector"
	}
	env.Emit(Event{Kind: "metric", Tool: ToolSearchName, Data: map[string]any{
		"metric": "icebath.tool_search.count", "scope": scope, "hits": hits, "outcome": outcome}})
}

// DispatchToolDescribe answers a tool_describe bridge call with full schemas for deferred tools
func DispatchToolDescribe(ctx context.Context, env *Env, args map[string]any, currentToolDefs []map[string]any, cfg *ToolSearchConfig, call *Call) string {
	names, errJSON := toolSearchStringListArg(args, "names", true, toolSearchMaxDescribeNamesPerCall, "Retry with fewer names per call.")
	if errJSON != "" {
		return errJSON
	}
	deferrable := toolSearchDeferrableIn(ctx, env, currentToolDefs)
	byName := map[string]map[string]any{}
	for _, td := range deferrable {
		if n := toolDefName(td); n != "" {
			byName[n] = toolDefFn(td)
		}
	}
	remoteSchemas, hostedFailure := connectorRemoteSchemasFor(ctx, env, names, currentToolDefs, call)
	tools := Dict()
	var notFound, undescribed []string
	errors := Dict()
	sessionNames := SessionToolNames(currentToolDefs)
	deferTools := LoadToolSearchConfig(env).EffectiveDeferTools()
	scope := newSearchScope(ctx, env)
	for _, name := range names {
		fn, local := byName[name]
		remoteFn, remote := remoteSchemas[name]
		switch {
		case local:
			params, _ := fn["parameters"]
			if params == nil {
				params = map[string]any{}
			}
			desc, _ := fn["description"].(string)
			tools.Set(name, Dict("description", desc, "parameters", params))
		case remote && remoteFn != nil:
			params, _ := remoteFn["parameters"]
			if params == nil {
				params = map[string]any{}
			}
			desc := ""
			if d, ok := remoteFn["description"]; ok && d != nil {
				desc = fmt.Sprint(d)
			}
			tools.Set(name, Dict("description", desc, "parameters", params))
		case IsConnectorName(name):
			if hostedFailure != "" {
				undescribed = append(undescribed, name)
			} else {
				notFound = append(notFound, name)
			}
		default:
			if unavailable := SessionUnavailableError(ctx, env, name, sessionNames); unavailable != "" {
				errors.Set(name, unavailable)
			} else if scope.entry(name) != nil && !IsDeferrableToolName(ctx, env, name, deferTools) {
				errors.Set(name, NotDeferrableError(ctx, env, name, sessionNames))
			} else {
				notFound = append(notFound, name)
			}
		}
	}
	result := Dict("tools", tools)
	if len(notFound) > 0 {
		result.Set("not_found", notFound)
		result.Set("hint", "Names in not_found are not currently available. Re-run tool_search to refresh.")
	}
	if errors.Len() > 0 {
		result.Set("errors", errors)
	}
	if hostedFailure != "" {
		result.Set("connectors", connectorsUnavailable(env, hostedFailure, "described", undescribed))
	}
	return JSON(result)
}

// ScopedDeferrableNames is the universe tool_call may reach in this session's pre assembly defs
func ScopedDeferrableNames(ctx context.Context, env *Env, toolDefs []map[string]any) map[string]bool {
	deferTools := LoadToolSearchConfig(env).EffectiveDeferTools()
	out := map[string]bool{}
	for _, td := range toolDefs {
		n := toolDefName(td)
		if n != "" && IsDeferrableToolName(ctx, env, n, deferTools) {
			out[n] = true
		}
	}
	return out
}

// OutOfScopeReason names the missing GUI surface for a session gated tool outside this session
func OutOfScopeReason(ctx context.Context, env *Env, name string) string {
	if toolset, ok := newSearchScope(ctx, env).toolset(name); ok && toolSearchDirectSurfaceToolset(toolset) {
		return fmt.Sprintf("'%s' needs a desktop-app session with a GUI surface (preview/terminal panes). ", name) +
			"This session has none: use it only from the icebath desktop app, not via tool_search."
	}
	return ""
}

// ScopeBlockReason is the block message for a tool_call naming a tool outside the session's scope
func ScopeBlockReason(ctx context.Context, env *Env, name string) string {
	if reason := OutOfScopeReason(ctx, env, name); reason != "" {
		return reason
	}
	if toolSearchCoreToolNames()[name] && newSearchScope(ctx, env).entry(name) != nil {
		return UnavailableInSessionReason(name)
	}
	return fmt.Sprintf("'%s' is not available in this session. Use tool_search to find tools you can call.", name)
}

// ResolveUnderlyingCall parses tool_call args holding Python values into the underlying name, its handler args, and error
func ResolveUnderlyingCall(ctx context.Context, env *Env, args map[string]any, sessionToolNames map[string]bool) (string, map[string]any, string) {
	entries, errMsg := NormalizeToolCallEntries(args)
	if errMsg != "" {
		return "", map[string]any{}, errMsg
	}
	if len(entries) > 1 {
		for _, e := range entries {
			if !IsConnectorName(e.Name) {
				return "", map[string]any{}, LocalBatchError(entries)
			}
		}
	}
	if IsConnectorName(entries[0].Name) {
		calls := make([]any, 0, len(entries))
		for _, e := range entries {
			calls = append(calls, map[string]any{"name": e.Name, "arguments": e.HandlerArgs()})
		}
		return ConnectorBatchSentinel, map[string]any{"calls": calls}, ""
	}
	name := entries[0].Name
	rawArgs := entries[0].HandlerArgs()
	if unavailable := SessionUnavailableError(ctx, env, name, sessionToolNames); unavailable != "" {
		return "", map[string]any{}, unavailable
	}
	if !IsDeferrableToolName(ctx, env, name, LoadToolSearchConfig(env).EffectiveDeferTools()) {
		return "", map[string]any{}, NotDeferrableError(ctx, env, name, sessionToolNames)
	}
	return name, rawArgs, ""
}
