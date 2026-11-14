package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	maxSearchQueries  = 7
	maxDescribeNames  = 10
	MaxConnectorCalls = 10
	echoArgsMax       = 1500
	describeClip      = 500
)

type searchConfig struct {
	enabled          string
	listing          string
	thresholdPct     float64
	defaultLimit     int
	maxLimit         int
	listingMaxTokens int
	deferTools       map[string]bool
}

func triState(v string) string {
	t := strings.ToLower(trimWide(v))
	switch t {
	case "true", "1", "yes":
		return "on"
	case "false", "0", "no":
		return "off"
	case "auto", "on", "off":
		return t
	}
	return "auto"
}

func clampInt(v, lo, hi int) int {
	return max(lo, min(hi, v))
}

func (r *Registry) searchConfig(ctx context.Context) (searchConfig, error) {
	if r.config == nil {
		return searchConfig{}, &UnfilledError{Name: "config store"}
	}
	cfg, err := r.config.LoadReadonly(ctx)
	if err != nil {
		return searchConfig{}, err
	}
	ts := cfg.Tools.ToolSearch
	maxLimit := clampInt(ts.MaxSearchLimit, 1, 50)
	sc := searchConfig{
		enabled:          triState(ts.Enabled.String()),
		listing:          triState(ts.Listing.String()),
		thresholdPct:     math.Max(0, math.Min(100, ts.ThresholdPct)),
		defaultLimit:     clampInt(ts.SearchDefaultLimit, 1, maxLimit),
		maxLimit:         maxLimit,
		listingMaxTokens: clampInt(ts.ListingMaxTokens, 200, 60000),
		deferTools:       map[string]bool{},
	}
	list := ts.Defer
	if list == nil {
		list = config.Default().Tools.ToolSearch.Defer
	}
	for _, n := range list {
		if t := trimWide(n); t != "" {
			sc.deferTools[t] = true
		}
	}
	return sc, nil
}

func (r *Registry) directSurface(toolset string) bool {
	if slices.Contains(ClientSurfaceToolsets(), toolset) {
		return true
	}
	t, ok := r.staticToolset(toolset)
	return ok && len(t.Platforms) > 0
}

func (r *Registry) toolsetOf(name string, at Scope) (string, bool) {
	e, ok := r.Lookup(name, at)
	if !ok {
		return "", false
	}
	return e.Tool.Toolset, true
}

func (r *Registry) deferrable(name string, at Scope, deferTools map[string]bool) bool {
	if IsBridgeTool(name) {
		return false
	}
	if deferTools[name] {
		return true
	}
	if slices.Contains(CoreTools(), name) {
		return false
	}
	ts, ok := r.toolsetOf(name, at)
	return ok && (strings.HasPrefix(ts, "mcp-") || !r.directSurface(ts))
}

func (r *Registry) classifyTools(tools []wire.Tool, at Scope, deferTools map[string]bool) (visible, deferrable []wire.Tool) {
	for _, t := range tools {
		if IsBridgeTool(t.Name) {
			continue
		}
		if r.deferrable(t.Name, at, deferTools) {
			deferrable = append(deferrable, t)
		} else {
			visible = append(visible, t)
		}
	}
	return visible, deferrable
}

func toolNames(tools []wire.Tool) map[string]bool {
	out := make(map[string]bool, len(tools))
	for _, t := range tools {
		if t.Name != "" {
			out[t.Name] = true
		}
	}
	return out
}

func connectionsInScope(tools []wire.Tool) bool {
	for _, t := range tools {
		if t.Name == "manage_connections" {
			return true
		}
	}
	return false
}

func definitionChars(t wire.Tool) int {
	params := t.Parameters
	if len(params) == 0 {
		params = json.RawMessage("null")
	}
	n := utf8.RuneCountInString(`{"type":"function","function":{"name":,"description":,"parameters":}}`)
	return n + utf8.RuneCount(wire.Quote(t.Name)) + utf8.RuneCount(wire.Quote(t.Description)) + utf8.RuneCount(params)
}

func estimateTokens(tools []wire.Tool) int {
	total := 0
	for _, t := range tools {
		total += definitionChars(t)
	}
	return int(math.Ceil(float64(total) / charsPerToken))
}

func listingBudget(cfg searchConfig, contextLength int) int {
	pct := 10000
	if contextLength > 0 {
		pct = int(float64(contextLength) * (cfg.thresholdPct / 100.0))
	}
	return max(0, min(cfg.listingMaxTokens, pct))
}

func (r *Registry) classifySource(name string, at Scope) (string, string) {
	ts, ok := r.toolsetOf(name, at)
	if !ok {
		return "other", ""
	}
	if strings.HasPrefix(ts, "mcp-") {
		return "mcp", ts
	}
	return "plugin", ts
}

func (r *Registry) hiddenSources(ctx context.Context, pass *checkPass, at Scope) ([]hiddenSource, error) {
	r.mu.RLock()
	entries := r.merged(at)
	r.mu.RUnlock()
	grouped := map[string][]*Entry{}
	for _, e := range entries {
		if server, ok := strings.CutPrefix(e.Tool.Toolset, "mcp-"); ok {
			grouped[server] = append(grouped[server], e)
		}
	}
	servers := make([]string, 0, len(grouped))
	for s := range grouped {
		servers = append(servers, s)
	}
	sort.Strings(servers)
	var rows []hiddenSource
	for _, server := range servers {
		live := false
		for _, e := range grouped[server] {
			if e.Tool.Check == nil || pass.ok(ctx, e.Tool.Check) {
				live = true
				break
			}
		}
		if live {
			continue
		}
		if r.mcp == nil {
			return nil, &UnfilledError{Name: "MCP server declarations"}
		}
		sentence, ok := r.mcp.Unavailable(ctx, server)
		if !ok {
			continue
		}
		rows = append(rows, hiddenSource{name: server, toolCount: len(grouped[server]), sentence: sentence})
	}
	return rows, nil
}

func (r *Registry) listing(ctx context.Context, pass *checkPass, at Scope, deferrable []wire.Tool, budget int) (string, string, error) {
	groups := map[string][]listingLine{}
	for _, t := range deferrable {
		if t.Name == "" {
			continue
		}
		_, source := r.classifySource(t.Name, at)
		label := listingLabel(source)
		groups[label] = append(groups[label], listingLine{t.Name, shortDescription(t.Description, catalogSummaryMax)})
	}
	hidden, err := r.hiddenSources(ctx, pass, at)
	if err != nil {
		return "", "", err
	}
	text, form := buildListing(groups, hidden, budget)
	return text, form, nil
}

const connectionsHint = " Searches also cover the user's connected app accounts, whose tools are hosted and so never listed here (results named `connectors__<app>__<tool>`); `manage_connections` checks or connects an account."

func searchDescription(deferred int, listing, form string, granted bool) string {
	var b strings.Builder
	if deferred > 0 {
		fmt.Fprintf(&b, "Search %d additional tools that are loaded on demand. ", deferred)
	} else {
		b.WriteString("Search remote connector tools (email, calendars, issue trackers, and more). ")
	}
	fmt.Fprintf(&b, "Send one keyword query per capability you need; returns matching tool names and descriptions. Then `%s` loads schemas and `%s` invokes. Tools already in your tool list need no search.", describeToolName, callToolName)
	if granted {
		b.WriteString(connectionsHint)
	}
	if listing == "" {
		return b.String()
	}
	if form == "groups" {
		b.WriteString("\n\nThe servers below are connected and their tools ARE available through this bridge. For any request in these domains, search here FIRST — do not claim the capability is unavailable and do not substitute a generic tool (terminal/browser) without searching.\n\n")
		b.WriteString(listing)
		return b.String()
	}
	fmt.Fprintf(&b, "\n\nEvery tool listed below exists: never call it unavailable, and go straight to `%s` when you see the exact name.", describeToolName)
	if form == "mixed" {
		fmt.Fprintf(&b, " For servers marked 'names not listed', the tools exist too — find them with `%s` before concluding anything is missing.", searchToolName)
	}
	b.WriteString("\n\n")
	b.WriteString(listing)
	return b.String()
}

func bridgeTools(deferred int, listing, form string, granted bool) []wire.Tool {
	return []wire.Tool{
		{
			Name:        searchToolName,
			Description: searchDescription(deferred, listing, form, granted),
			Parameters:  json.RawMessage(`{"type":"object","properties":{"queries":{"type":"array","items":{"type":"string"},"description":"One query per capability: service + action + object keywords (e.g. ['gmail send email', 'github create issue']), never a question; every word must match tool text. Searched in parallel."},"limit":{"type":"integer","description":"Max matches per query (default 5)."}},"required":["queries"]}`),
		},
		{
			Name:        describeToolName,
			Description: fmt.Sprintf("Load the full JSON schemas for tools returned by `%s`. Required before `%s` if a tool's parameters are unknown. Batch every schema you need into one call.", searchToolName, callToolName),
			Parameters:  json.RawMessage(`{"type":"object","properties":{"names":{"type":"array","items":{"type":"string"},"description":"Exact tool names (as returned by tool_search). A single string is accepted and treated as one name."}},"required":["names"]}`),
		},
		{
			Name:        callToolName,
			Description: fmt.Sprintf("Invoke deferred tools: `calls` is an array of {name, arguments}, one entry per invocation (batch independent calls; results come back in order). Arguments match the schema from `%s`; approvals and hooks apply as usual.", describeToolName),
			Parameters:  json.RawMessage(`{"type":"object","properties":{"calls":{"type":"array","items":{"type":"object","properties":{"name":{"type":"string","description":"Exact tool name to invoke."},"arguments":{"type":"object","description":"Arguments matching the tool schema."}},"required":["name","arguments"]},"description":"One or more independent deferred tool invocations."}},"required":["calls"]}`),
		},
	}
}

func (r *Registry) assemble(ctx context.Context, pass *checkPass, at Scope, tools []wire.Tool, contextLength int, cfg searchConfig) ([]wire.Tool, Assembly, error) {
	var incoming []wire.Tool
	for _, t := range tools {
		if !IsBridgeTool(t.Name) {
			incoming = append(incoming, t)
		}
	}
	visible, deferrable := r.classifyTools(incoming, at, cfg.deferTools)
	granted := connectionsInScope(incoming)
	if len(deferrable) == 0 {
		if cfg.enabled != "off" && granted {
			return append(incoming, bridgeTools(0, "", "", granted)...), Assembly{Activated: true, Tier: 2, Listing: "none"}, nil
		}
		return incoming, Assembly{Listing: "none"}, nil
	}
	tokens := estimateTokens(deferrable)
	if cfg.enabled == "off" {
		return incoming, Assembly{Deferred: len(deferrable), DeferredTokens: tokens, Listing: "none"}, nil
	}
	listing, form := "", "none"
	budget := listingBudget(cfg, contextLength)
	if cfg.listing != "off" {
		var err error
		listing, form, err = r.listing(ctx, pass, at, deferrable, budget)
		if err != nil {
			return nil, Assembly{}, err
		}
	}
	tier := 2
	if form == "full" || form == "names" || form == "mixed" {
		tier = 1
	}
	slog.Info(fmt.Sprintf("tool_search activated (tier %d): %d core/visible tools kept, %d deferred (~%d tokens), listing %s (budget ~%d tokens)",
		tier, len(visible), len(deferrable), tokens, form, budget))
	out := append(slices.Clone(visible), bridgeTools(len(deferrable), listing, form, granted)...)
	return out, Assembly{Activated: true, Tier: tier, Deferred: len(deferrable), DeferredTokens: tokens, Listing: form}, nil
}

func (r *Registry) catalogOf(tools []wire.Tool, at Scope) []catalogEntry {
	var out []catalogEntry
	for _, t := range tools {
		if t.Name == "" {
			continue
		}
		source, sourceName := r.classifySource(t.Name, at)
		label := ""
		if sourceName != "" {
			label = listingLabel(sourceName)
		}
		params, err := parseNode(t.Parameters)
		if err != nil {
			params = newObj()
		}
		out = append(out, newCatalogEntry(t.Name, t.Description, params, source, sourceName, searchText(t.Name, t.Description, params, label)))
	}
	return out
}

func connectorEntry(t ConnectorTool) catalogEntry {
	params, err := parseNode(t.Parameters)
	if err != nil {
		params = newObj()
	}
	text := t.Connector + " " + strings.ReplaceAll(t.Slug, "_", " ") + " " + t.Description
	return newCatalogEntry(t.Name, t.Description, params, "connectors", t.Connector, text)
}

func clipDescription(text string) string {
	if utf8.RuneCountInString(text) <= describeClip {
		return text
	}
	return runePrefix(text, describeClip) + "…"
}

func sharedRecord(e catalogEntry) json.RawMessage {
	required := newArr()
	if req := e.params.get("required"); req.isArr() {
		for _, r := range req.arr {
			s, ok := r.text()
			if !ok {
				continue
			}
			if len(required.arr) == 32 {
				break
			}
			required.arr = append(required.arr, newStr(runePrefix(s, 64)))
		}
	}
	return newObj().with("source", newStr(e.source)).with("source_name", newStr(e.sourceName)).
		with("description", newStr(clipDescription(e.description))).with("required", required).compact()
}

func valueText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if !t {
			return ""
		}
		return "true"
	case json.Number:
		if f, err := t.Float64(); err == nil && f == 0 {
			return ""
		}
		return string(t)
	}
	n := nodeOf(v)
	if !n.truthy() {
		return ""
	}
	return string(n.compact())
}

func stringListArg(args Args, key string, dedupe bool, limit int, hint string) ([]string, *Result) {
	raw := args[key]
	if s, ok := raw.(string); ok {
		raw = []any{s}
	}
	list, ok := raw.([]any)
	if !ok {
		f := Failure(key + " is required and must be an array of strings")
		return nil, &f
	}
	var out []string
	for _, item := range list {
		text := trimWide(valueText(item))
		if text != "" && (!dedupe || !slices.Contains(out, text)) {
			out = append(out, text)
		}
	}
	if len(out) == 0 {
		f := Failure(key + " is required and must contain at least one non-empty string")
		return nil, &f
	}
	if len(out) > limit {
		f := Failure(fmt.Sprintf("too many %s: %d > max %d. %s", key, len(out), limit, hint))
		return nil, &f
	}
	return out, nil
}

func intArg(v any, fallback int) int {
	switch t := v.(type) {
	case json.Number:
		if i, err := strconv.Atoi(string(t)); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
			return int(f)
		}
	case float64:
		if !math.IsInf(t, 0) && !math.IsNaN(t) {
			return int(t)
		}
	case int:
		return t
	case bool:
		if t {
			return 1
		}
		return 0
	case string:
		if i, err := strconv.Atoi(strings.ReplaceAll(trimWide(t), "_", "")); err == nil {
			return i
		}
	}
	return fallback
}

func searchScope(local, hosted bool) string {
	switch {
	case local && hosted:
		return "both"
	case hosted:
		return "connector"
	}
	return "local"
}

func (d *Dispatcher) recordSearch(ctx context.Context, local, hosted bool, hits int, outcome string) error {
	if d.metrics == nil {
		return &UnfilledError{Name: "tool search metrics"}
	}
	d.metrics.ToolSearch(ctx, searchScope(local, hosted), hits, outcome)
	return nil
}

func (r *Registry) availableSources(ctx context.Context, pass *checkPass, at Scope, catalog []catalogEntry) (json.RawMessage, error) {
	counts := map[string]int{}
	for _, e := range catalog {
		counts[listingLabel(e.sourceName)]++
	}
	type row struct {
		name string
		raw  *jnode
	}
	var rows []row
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		rows = append(rows, row{n, newObj().with("name", newStr(n)).with("tool_count", newNum(strconv.Itoa(counts[n])))})
	}
	hidden, err := r.hiddenSources(ctx, pass, at)
	if err != nil {
		return nil, err
	}
	for _, h := range hidden {
		rows = append(rows, row{h.name, newObj().with("name", newStr(h.name)).with("tool_count", newNum(strconv.Itoa(h.toolCount))).with("unavailable", newStr(h.sentence))})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	if len(rows) == 0 {
		return nil, nil
	}
	list := newArr()
	for _, r := range rows {
		list.arr = append(list.arr, r.raw)
	}
	return list.compact(), nil
}

func failureOf(err error) Result {
	return Failure(err.Error())
}

func (d *Dispatcher) dispatchSearch(ctx context.Context, c *Call, args Args, defs []wire.Tool) Result {
	at := c.Scope
	cfg, err := d.r.searchConfig(ctx)
	if err != nil {
		return failureOf(err)
	}
	hosted := connectionsInScope(defs)
	_, deferrable := d.r.classifyTools(defs, at, cfg.deferTools)
	queries, bad := stringListArg(args, "queries", false, maxSearchQueries, "Retry with fewer, more targeted queries.")
	if bad != nil {
		if err := d.recordSearch(ctx, len(deferrable) > 0, hosted, 0, "error"); err != nil {
			return failureOf(err)
		}
		return *bad
	}
	limit := cfg.defaultLimit
	if raw, ok := args["limit"]; ok && raw != nil {
		limit = clampInt(intArg(raw, cfg.defaultLimit), 1, cfg.maxLimit)
	}
	catalog := d.r.catalogOf(deferrable, at)
	remote := make([][]catalogEntry, len(queries))
	failure := ""
	if hosted {
		if d.connectors == nil {
			return failureOf(&UnfilledError{Name: "connectors"})
		}
		hits, f := d.connectors.Search(ctx, c, queries)
		failure = f
		if failure == "" {
			for i := range queries {
				if i < len(hits) {
					for _, t := range hits[i] {
						remote[i] = append(remote[i], connectorEntry(t))
					}
				}
			}
		}
	}
	pass := d.r.newPass(at, nil)
	sources, err := d.r.availableSources(ctx, pass, at, catalog)
	if err != nil {
		return failureOf(err)
	}
	tools := newObj()
	results := newArr()
	for i, query := range queries {
		corpus := append(slices.Clone(catalog), remote[i]...)
		hits := searchCatalog(corpus, query, limit)
		matches := newArr()
		for _, h := range hits {
			if !tools.has(h.name) {
				rec, _ := parseNode(sharedRecord(h))
				tools.set(h.name, rec)
			}
			matches.arr = append(matches.arr, newStr(h.name))
		}
		group := newObj().with("query", newStr(query)).with("matches", matches)
		if len(hits) == 0 && sources != nil {
			src, _ := parseNode(sources)
			group.set("available_sources", src)
			group.set("hint", newStr("This query returned no lexical matches, but the sources above are connected and their tools remain available. Retry tool_search with the service name plus a concrete action or object before concluding the capability is unavailable."))
		}
		results.arr = append(results.arr, group)
	}
	remoteCount := 0
	for _, name := range tools.keys {
		if strings.HasPrefix(name, ConnectorPrefix) {
			remoteCount++
		}
	}
	qs := newArr()
	for _, q := range queries {
		qs.arr = append(qs.arr, newStr(q))
	}
	payload := newObj().with("queries", qs).with("total_available", newNum(strconv.Itoa(len(catalog)+remoteCount))).
		with("results", results).with("tools", tools)
	if failure != "" {
		u, err := parseNode(d.connectors.Unavailable(ctx, failure, "searched", nil))
		if err != nil {
			return failureOf(fmt.Errorf("connector unavailability payload: %w", err))
		}
		payload.set("connectors", u)
	}
	outcome := "no_hits"
	switch {
	case failure != "":
		outcome = "connector_unavailable"
	case len(tools.keys) > 0:
		outcome = "hits"
	}
	if err := d.recordSearch(ctx, len(catalog) > 0, hosted, len(tools.keys), outcome); err != nil {
		return failureOf(err)
	}
	return Result{Text: payload.spaced()}
}

func (d *Dispatcher) dispatchDescribe(ctx context.Context, c *Call, args Args, defs []wire.Tool) Result {
	at := c.Scope
	cfg, err := d.r.searchConfig(ctx)
	if err != nil {
		return failureOf(err)
	}
	names, bad := stringListArg(args, "names", true, maxDescribeNames, "Retry with fewer names per call.")
	if bad != nil {
		return *bad
	}
	_, deferrable := d.r.classifyTools(defs, at, cfg.deferTools)
	byName := map[string]wire.Tool{}
	for _, t := range deferrable {
		if t.Name != "" {
			byName[t.Name] = t
		}
	}
	var connectorNames []string
	for _, n := range names {
		if strings.HasPrefix(n, ConnectorPrefix) {
			connectorNames = append(connectorNames, n)
		}
	}
	remote := map[string]ConnectorTool{}
	failure := ""
	if len(connectorNames) > 0 && connectionsInScope(defs) {
		if d.connectors == nil {
			return failureOf(&UnfilledError{Name: "connectors"})
		}
		remote, failure = d.connectors.Describe(ctx, c, connectorNames)
	}
	session := toolNames(defs)
	tools := newObj()
	var notFound, undescribed []string
	errs := newObj()
	for _, name := range names {
		if t, ok := byName[name]; ok {
			params, err := parseNode(t.Parameters)
			if err != nil || len(t.Parameters) == 0 {
				params = newObj()
			}
			tools.set(name, newObj().with("description", newStr(t.Description)).with("parameters", params))
			continue
		}
		if t, ok := remote[name]; ok {
			params, err := parseNode(t.Parameters)
			if err != nil || len(t.Parameters) == 0 {
				params = newObj()
			}
			tools.set(name, newObj().with("description", newStr(t.Description)).with("parameters", params))
			continue
		}
		if strings.HasPrefix(name, ConnectorPrefix) {
			if failure != "" {
				undescribed = append(undescribed, name)
			} else {
				notFound = append(notFound, name)
			}
			continue
		}
		if reason := d.r.sessionUnavailable(name, at, session); reason != "" {
			errs.set(name, newStr(reason))
			continue
		}
		if _, registered := d.r.Lookup(name, at); registered && !d.r.deferrable(name, at, cfg.deferTools) {
			errs.set(name, newStr(d.r.notDeferrable(name, at, session)))
			continue
		}
		notFound = append(notFound, name)
	}
	result := newObj().with("tools", tools)
	if len(notFound) > 0 {
		result.set("not_found", nodeOf(notFound))
		result.set("hint", newStr("Names in not_found are not currently available. Re-run tool_search to refresh."))
	}
	if len(errs.keys) > 0 {
		result.set("errors", errs)
	}
	if failure != "" {
		u, err := parseNode(d.connectors.Unavailable(ctx, failure, "described", undescribed))
		if err != nil {
			return failureOf(fmt.Errorf("connector unavailability payload: %w", err))
		}
		result.set("connectors", u)
	}
	return Result{Text: result.spaced()}
}

func unavailableInSession(name string) string {
	return fmt.Sprintf("'%s' is not available in this execution context. Don't retry it — state the assumption and proceed with the work.", name)
}

func (r *Registry) outOfScope(name string, at Scope) string {
	if ts, ok := r.toolsetOf(name, at); ok && r.directSurface(ts) {
		return fmt.Sprintf("'%s' needs a desktop-app session with a GUI surface (preview/terminal panes). This session has none: use it only from the icebath desktop app, not via tool_search.", name)
	}
	return ""
}

func (r *Registry) sessionUnavailable(name string, at Scope, session map[string]bool) string {
	if session == nil || session[name] {
		return ""
	}
	if _, ok := r.Lookup(name, at); !ok || !slices.Contains(CoreTools(), name) {
		return ""
	}
	if reason := r.outOfScope(name, at); reason != "" {
		return reason
	}
	return unavailableInSession(name)
}

func (r *Registry) notDeferrable(name string, at Scope, session map[string]bool) string {
	_, registered := r.Lookup(name, at)
	if slices.Contains(CoreTools(), name) || registered {
		if session != nil && !session[name] {
			return unavailableInSession(name)
		}
		return fmt.Sprintf("'%s' is a directly-listed tool, not a deferred one. Call it directly instead of via tool_call.", name)
	}
	var candidates []string
	for _, e := range r.Entries(at) {
		if strings.HasSuffix(e.Tool.Name, "__"+name) {
			candidates = append(candidates, e.Tool.Name)
		}
	}
	sort.Strings(candidates)
	hint := " Use tool_search to find the exact name."
	if len(candidates) > 0 {
		quoted := make([]string, len(candidates))
		for i, c := range candidates {
			quoted[i] = "'" + c + "'"
		}
		hint = " Did you mean " + strings.Join(quoted, ", ") + "?"
	}
	return fmt.Sprintf("'%s' is not a known tool name. Deferred tools must be invoked through tool_call by the exact name tool_search returns (e.g. mcp__<server>__{tool}).%s", name, hint)
}

func (r *Registry) scopeBlock(name string, at Scope) string {
	if reason := r.outOfScope(name, at); reason != "" {
		return reason
	}
	if _, ok := r.Lookup(name, at); ok && slices.Contains(CoreTools(), name) {
		return unavailableInSession(name)
	}
	return fmt.Sprintf("'%s' is not available in this session. Use tool_search to find tools you can call.", name)
}

type callEntry struct {
	name string
	args *jnode
}

func entryName(n *jnode) string {
	if !n.truthy() {
		return ""
	}
	switch n.kind {
	case jStr:
		return n.str
	case jNum:
		return n.num
	case jBool:
		return "true"
	}
	return string(n.compact())
}

func BridgeEntryNames(arguments string) ([]string, bool) {
	args, err := parseNode([]byte(arguments))
	if err != nil || !args.isObj() {
		return nil, false
	}
	entries, failure := normalizeEntries(args)
	if failure != "" {
		return nil, false
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.name
	}
	return names, true
}

func normalizeEntries(args *jnode) ([]callEntry, string) {
	raw := args.get("calls")
	if raw == nil || raw.kind == jNull {
		if trimWide(entryName(args.get("name"))) == "" {
			return nil, "tool_call requires 'calls' (an array of {name, arguments})"
		}
		single := newObj().with("name", args.get("name"))
		a := args.get("arguments")
		if a == nil {
			a = &jnode{kind: jNull}
		}
		single.set("arguments", a)
		raw = newArr(single)
	}
	if s, ok := raw.text(); ok {
		parsed, err := parseNode([]byte(s))
		if err != nil {
			return nil, fmt.Sprintf("tool_call 'calls' is not valid JSON: %v", err)
		}
		raw = parsed
	}
	if raw.isObj() {
		raw = newArr(raw)
	}
	if !raw.isArr() || len(raw.arr) == 0 {
		return nil, "tool_call 'calls' must be a non-empty array of {name, arguments}"
	}
	var entries []callEntry
	for i, item := range raw.arr {
		if !item.isObj() {
			return nil, fmt.Sprintf("tool_call calls[%d] must be an object with 'name' and 'arguments'", i)
		}
		name := trimWide(entryName(item.get("name")))
		if name == "" {
			return nil, fmt.Sprintf("tool_call calls[%d] requires a 'name'", i)
		}
		if IsBridgeTool(name) {
			return nil, fmt.Sprintf("tool_call cannot invoke '%s' (it is itself a bridge tool)", name)
		}
		a := item.get("arguments")
		if s, ok := a.text(); a == nil || a.kind == jNull || ok && trimWide(s) == "" {
			a = newObj()
		} else if ok {
			parsed, err := parseNode([]byte(s))
			if err != nil {
				return nil, fmt.Sprintf("tool_call calls[%d].arguments is not valid JSON: %v", i, err)
			}
			a = parsed
		}
		if !a.isObj() {
			return nil, fmt.Sprintf("tool_call calls[%d].arguments must be an object", i)
		}
		entries = append(entries, callEntry{name: name, args: a})
	}
	return entries, ""
}

func localBatchError(entries []callEntry) string {
	first := entries[0]
	args := string(first.args.compact())
	if utf8.RuneCountInString(args) > echoArgsMax {
		args = "{...}"
	}
	retry := `{"calls":[{"name":` + string(wire.Quote(first.name)) + `,"arguments":` + args + `}]}`
	remaining := ""
	if len(entries) > 1 {
		remaining = fmt.Sprintf(" then issue the remaining %d call(s) as separate tool_call invocations", len(entries)-1)
	}
	return fmt.Sprintf("tool_call takes exactly one entry for local tools; you sent %d. Retry with only: %s%s. Only connectors__ names may be batched together.", len(entries), retry, remaining)
}

type underlyingCall struct {
	name    string
	args    *jnode
	entries []callEntry
}

func (r *Registry) resolveUnderlying(args *jnode, at Scope, session map[string]bool, deferTools map[string]bool) (underlyingCall, string) {
	entries, msg := normalizeEntries(args)
	if msg != "" {
		return underlyingCall{}, msg
	}
	local := false
	for _, e := range entries {
		if !strings.HasPrefix(e.name, ConnectorPrefix) {
			local = true
		}
	}
	if len(entries) > 1 && local {
		return underlyingCall{}, localBatchError(entries)
	}
	if strings.HasPrefix(entries[0].name, ConnectorPrefix) {
		return underlyingCall{name: ConnectorBatch, entries: entries}, ""
	}
	name := entries[0].name
	if reason := r.sessionUnavailable(name, at, session); reason != "" {
		return underlyingCall{}, reason
	}
	if !r.deferrable(name, at, deferTools) {
		return underlyingCall{}, r.notDeferrable(name, at, session)
	}
	return underlyingCall{name: name, args: entries[0].args}, ""
}
