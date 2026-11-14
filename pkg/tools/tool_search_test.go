package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func decodeMap(raw string) map[string]any {
	var out map[string]any
	json.Unmarshal([]byte(raw), &out)
	return out
}

func toolDef(name, description string, properties map[string]any) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	return map[string]any{"type": "function", "function": map[string]any{"name": name, "description": description,
		"parameters": map[string]any{"type": "object", "properties": properties}}}
}

func searchEnv(t *testing.T) *Env {
	t.Helper()
	previous := MCPUnavailableDetails
	MCPUnavailableDetails = func(server string) (string, bool, bool) { return "", false, false }
	t.Cleanup(func() { MCPUnavailableDetails = previous })
	return &Env{Home: t.TempDir(), Config: config.Defaults()}
}

func registerSearchTool(t *testing.T, name, toolset string, params map[string]any) {
	t.Helper()
	if params == nil {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	Default.Register(Def{Name: name, Toolset: toolset, Schema: map[string]any{"name": name, "description": "desc " + name, "parameters": params},
		Handler: func(ctx context.Context, call *Call) string {
			return jsonCompact(map[string]any{"ok": true, "args": call.Args})
		}})
	t.Cleanup(func() { Default.Remove(name) })
}

func TestToolSearchConfigParsing(t *testing.T) {
	configured := DefaultDeferredTools()
	if len(configured) == 0 || !configured["todo_list"] {
		t.Fatalf("defaults %v", configured)
	}
	if got := ToolSearchConfigFromRaw(nil).EffectiveDeferTools(); len(got) != len(configured) {
		t.Fatal("nil raw not default")
	}
	if got := ToolSearchConfigFromRaw(map[string]any{"defer": []any{"terminal"}}).EffectiveDeferTools(); len(got) != 1 || !got["terminal"] {
		t.Fatal("user list did not replace")
	}
	if got := ToolSearchConfigFromRaw(map[string]any{"defer": []any{}}).EffectiveDeferTools(); len(got) != 0 {
		t.Fatal("empty list not kept")
	}
	if got := ToolSearchConfigFromRaw(map[string]any{"defer": "todo_list"}).EffectiveDeferTools(); len(got) != len(configured) {
		t.Fatal("scalar did not fall back")
	}
	if ToolSearchConfigFromRaw(true).Enabled != "auto" || ToolSearchConfigFromRaw(false).Enabled != "off" {
		t.Fatal("bool aliases")
	}
	cfg := ToolSearchConfigFromRaw(map[string]any{"search_default_limit": 100, "max_search_limit": 1000})
	if cfg.MaxSearchLimit != 50 || cfg.SearchDefaultLimit != 50 {
		t.Fatalf("clamp %+v", cfg)
	}
	cfg = ToolSearchConfigFromRaw(map[string]any{"search_default_limit": 0, "listing_max_tokens": 10})
	if cfg.SearchDefaultLimit != 1 || cfg.ListingMaxTokens != 200 {
		t.Fatalf("floors %+v", cfg)
	}
}

func TestToolSearchClassification(t *testing.T) {
	env := searchEnv(t)
	ctx := context.Background()
	registerSearchTool(t, "mcp__srv__thing", "mcp-srv", nil)
	registerSearchTool(t, "plugin_tool_x", "myplugin", nil)
	registerSearchTool(t, "preview_open_x", "desktop_ui", nil)
	defer_ := LoadToolSearchConfig(env).EffectiveDeferTools()
	for _, core := range []string{"terminal", "read_file", "write_file", "clarify"} {
		if IsDeferrableToolName(ctx, env, core, defer_) {
			t.Fatalf("core tool %s deferred", core)
		}
	}
	for _, bridge := range []string{ToolSearchName, ToolDescribeName, ToolCallName} {
		if IsDeferrableToolName(ctx, env, bridge, defer_) {
			t.Fatalf("bridge %s deferred", bridge)
		}
	}
	if !IsDeferrableToolName(ctx, env, "mcp__srv__thing", defer_) || !IsDeferrableToolName(ctx, env, "plugin_tool_x", defer_) {
		t.Fatal("mcp or plugin tool not deferrable")
	}
	if IsDeferrableToolName(ctx, env, "preview_open_x", defer_) {
		t.Fatal("gui surface tool deferred")
	}
	if !IsDeferrableToolName(ctx, env, "preview_open_x", map[string]bool{"preview_open_x": true}) {
		t.Fatal("defer override ignored")
	}
	if !IsDeferrableToolName(ctx, env, "todo_list", defer_) {
		t.Fatal("curated deferral ignored")
	}
	visible, deferrable := ClassifyTools(ctx, env, []map[string]any{toolDef("terminal", "", nil), toolDef("mcp__srv__thing", "", nil), toolDef("unknown_tool", "", nil), toolDef(ToolSearchName, "", nil)}, defer_)
	if len(visible) != 2 || len(deferrable) != 1 {
		t.Fatalf("visible %d deferrable %d", len(visible), len(deferrable))
	}
}

func TestThresholdGate(t *testing.T) {
	cfg := ToolSearchConfigFromRaw(map[string]any{"enabled": "off"})
	if ShouldActivate(cfg, 999999, 200000, false) {
		t.Fatal("off activated")
	}
	if !ShouldActivate(ToolSearchConfigFromRaw(nil), 1, 200000, false) {
		t.Fatal("deferrable tokens did not activate")
	}
	small := EstimateTokensFromSchemas([]map[string]any{toolDef("a", "x", nil)})
	large := EstimateTokensFromSchemas([]map[string]any{toolDef("a", strings.Repeat("x", 4000), nil)})
	if large <= small {
		t.Fatal("token estimate not proportional")
	}
	if ListingTokenBudget(ToolSearchConfigFromRaw(nil), 0) != 4000 || ListingTokenBudget(ToolSearchConfigFromRaw(nil), 20000) != 1000 {
		t.Fatal("listing budget")
	}
}

func fakeCatalog() []CatalogEntry {
	defs := []map[string]any{
		toolDef("github_create_issue", "Open a new issue in a GitHub repository", map[string]any{"title": map[string]any{"type": "string"}, "body": map[string]any{"type": "string"}}),
		toolDef("github_search_repos", "Search GitHub for matching repositories", map[string]any{"query": map[string]any{"type": "string"}}),
		toolDef("slack_send_message", "Post a message into a Slack channel", map[string]any{"channel": map[string]any{"type": "string"}, "text": map[string]any{"type": "string"}}),
		toolDef("calendar_create_event", "Add an event to the user's calendar", map[string]any{"title": map[string]any{"type": "string"}, "start": map[string]any{"type": "string"}}),
	}
	var catalog []CatalogEntry
	for _, d := range defs {
		catalog = append(catalog, CatalogEntry{Name: toolDefName(d), Description: toolDefDescription(d), Schema: d, Source: "mcp", SourceName: "mcp-test",
			Tokens: catalogTokenize(catalogEntrySearchText(d, ""))})
	}
	return catalog
}

func TestRetrieval(t *testing.T) {
	catalog := fakeCatalog()
	hits := SearchCatalog(catalog, "create github issue", 5, nil)
	if len(hits) == 0 || hits[0].Name != "github_create_issue" {
		t.Fatalf("hits %v", hits)
	}
	if len(SearchCatalog(catalog, "github", 1, nil)) != 1 {
		t.Fatal("limit ignored")
	}
	if hits := SearchCatalog(catalog, "issues", 5, nil); len(hits) == 0 || hits[0].Name != "github_create_issue" {
		t.Fatalf("stemming: %v", hits)
	}
	if len(SearchCatalog(catalog, "zebra", 5, nil)) != 0 {
		t.Fatal("unknown token returned results")
	}
}

func relevanceCatalog(t *testing.T) []CatalogEntry {
	t.Helper()
	defs := []map[string]any{
		toolDef("github_rerun_failed_workflow_run_jobs", "Re-run failed jobs in a workflow run", map[string]any{"run_id": map[string]any{"type": "string"}}),
		toolDef("github_create_issue", "Open a new issue in a GitHub repository", map[string]any{"title": map[string]any{"type": "string"}, "body": map[string]any{"type": "string"}}),
		toolDef("github_list_issues", "List issues in a repository", map[string]any{"repo": map[string]any{"type": "string"}}),
		toolDef("slack_send_message", "Post a message into a Slack channel", map[string]any{"channel": map[string]any{"type": "string"}, "text": map[string]any{"type": "string"}}),
		toolDef("gist_save_snippet", "Save a shell command snippet as a gist", map[string]any{"content": map[string]any{"type": "string"}}),
		toolDef("codeql_scan", "Scan code for vulnerabilities and execute analysis", map[string]any{"repo": map[string]any{"type": "string"}}),
	}
	return BuildCatalog(context.Background(), searchEnv(t), defs)
}

func hasHit(hits []CatalogEntry, name string) bool {
	for _, h := range hits {
		if h.Name == name {
			return true
		}
	}
	return false
}

func TestRelevanceFloor(t *testing.T) {
	catalog := relevanceCatalog(t)
	if hits := SearchCatalog(catalog, "run shell command execute code", 5, nil); len(hits) != 0 {
		t.Fatalf("incidental single term match admitted: %v", hits)
	}
	if !hasHit(SearchCatalog(catalog, "list issues", 5, nil), "github_list_issues") {
		t.Fatal("short query lost recall")
	}
	if !hasHit(SearchCatalog(catalog, "send message", 5, nil), "slack_send_message") {
		t.Fatal("short query lost recall for slack")
	}
	hits := SearchCatalog(catalog, "create issue github repository title", 5, nil)
	if len(hits) == 0 || hits[0].Name != "github_create_issue" || hasHit(hits, "github_rerun_failed_workflow_run_jobs") {
		t.Fatalf("long query with coverage: %v", hits)
	}
	if hits := SearchCatalog(catalog, "github_create_issue", 5, nil); len(hits) == 0 || hits[0].Name != "github_create_issue" {
		t.Fatal("exact name bypass")
	}
}

func TestPorter2Stemmer(t *testing.T) {
	cases := map[string]string{
		"issues": "issu", "running": "run", "generously": "generous", "agreed": "agre", "caresses": "caress", "ponies": "poni",
		"ties": "tie", "cats": "cat", "conditional": "condit", "hopping": "hop", "fizzed": "fizz", "relational": "relat",
		"skies": "sky", "news": "news", "happily": "happili", "cry": "cri", "say": "say", "rational": "ration",
	}
	for word, want := range cases {
		if got := porter2Stem(word); got != want {
			t.Errorf("%s: got %s want %s", word, got, want)
		}
	}
}

func TestAssembly(t *testing.T) {
	env := searchEnv(t)
	ctx := context.Background()
	defs := []map[string]any{toolDef("terminal", "run", nil), toolDef("read_file", "read", nil)}
	result := AssembleToolDefs(ctx, env, defs, 200000, nil)
	if result.Activated || len(result.ToolDefs) != 2 {
		t.Fatalf("no deferrable changed: %+v", result)
	}
	for i := 0; i < 3; i++ {
		registerSearchTool(t, fmt.Sprintf("mcp__asm__tool%d", i), "mcp-asm", nil)
		defs = append(defs, toolDef(fmt.Sprintf("mcp__asm__tool%d", i), "Deferred capability description.", nil))
	}
	first := AssembleToolDefs(ctx, env, defs, 200000, nil)
	if !first.Activated || first.DeferredCount != 3 || first.ListingForm != "full" {
		t.Fatalf("first %+v", first)
	}
	offCfg := ToolSearchConfigFromRaw(map[string]any{"enabled": "off"})
	stale := AssembleToolDefs(ctx, env, []map[string]any{toolDef("terminal", "Run shell", nil), toolDef(ToolSearchName, "old", nil)}, 200000, &offCfg)
	for _, td := range stale.ToolDefs {
		if toolDefName(td) == ToolSearchName {
			t.Fatal("stale bridge not stripped")
		}
	}
	second := AssembleToolDefs(ctx, env, append(append([]map[string]any{}, defs...), toolDef(ToolSearchName, "old", nil)), 200000, nil)
	names := map[string]int{}
	for _, td := range second.ToolDefs {
		names[toolDefName(td)]++
	}
	if names[ToolSearchName] != 1 || names[ToolCallName] != 1 || names[ToolDescribeName] != 1 || len(second.ToolDefs) != len(first.ToolDefs) {
		t.Fatalf("not idempotent: %v", names)
	}
	off := ToolSearchConfigFromRaw(map[string]any{"enabled": "on", "listing": "off"})
	bare := AssembleToolDefs(ctx, env, defs, 1000, &off)
	if !bare.Activated {
		t.Fatal("listing off did not activate")
	}
	for _, td := range bare.ToolDefs {
		if toolDefName(td) == ToolSearchName && strings.Contains(toolDefDescription(td), "mcp__asm__tool0") {
			t.Fatal("listing embedded while off")
		}
	}
}

func TestCatalogListingLayout(t *testing.T) {
	env := searchEnv(t)
	ctx := context.Background()
	var defs []map[string]any
	for i := 0; i < 800; i++ {
		name := fmt.Sprintf("layouthuge_op_%04d", i)
		registerSearchTool(t, name, "mcp-layouthuge", nil)
		defs = append(defs, toolDef(name, "Do a huge-server thing.", nil))
	}
	for _, pair := range [][2]string{{"layoutsmall_render", "Render the current scene to a file."}, {"layoutsmall_inspect", "Summarize the objects in the scene."}} {
		registerSearchTool(t, pair[0], "mcp-layoutsmall", nil)
		defs = append(defs, toolDef(pair[0], pair[1], nil))
	}
	text, form := BuildCatalogListingWithForm(ctx, env, defs, 1000)
	if form != "mixed" || !strings.Contains(text, "layouthuge (800 tools \u2014 names not listed") || strings.Contains(text, "layouthuge_op_0000") {
		t.Fatalf("form %s\n%s", form, text)
	}
	lineSet := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		lineSet[line] = true
	}
	if !lineSet["- layoutsmall_render: Render the current scene to a file."] {
		t.Fatalf("small server lost its descriptions:\n%s", text)
	}
	var pairDefs []map[string]any
	for _, pair := range [][2]string{{"layoutpair_b", "Second paired tool."}, {"layoutpair_a", "First paired tool."}} {
		registerSearchTool(t, pair[0], "mcp-layoutpair", nil)
		pairDefs = append(pairDefs, toolDef(pair[0], pair[1], nil))
	}
	registerSearchTool(t, "layoutsolo_only", "mcp-layoutsolo", nil)
	pairDefs = append(pairDefs, toolDef("layoutsolo_only", "The only solo tool.", nil))
	text, form = BuildCatalogListingWithForm(ctx, env, pairDefs, 4000)
	want := []string{"- layoutsolo_only: The only solo tool.", "layoutpair tools (2):", "- layoutpair_a: First paired tool.", "- layoutpair_b: Second paired tool."}
	if lines := strings.Split(text, "\n"); form != "full" || strings.Join(lines[1:], "|") != strings.Join(want, "|") {
		t.Fatalf("form %s\n%s", form, text)
	}
	if catalogShortDesc("Opens a page, e.g. the docs. Then more.", 80) != "Opens a page, e.g. the docs." {
		t.Fatal("abbreviation clipped")
	}
	if catalogShortDesc("Visit example.com/path for details. Next.", 80) != "Visit example.com/path for details." {
		t.Fatal("hostname clipped")
	}
	if got := catalogShortDesc(strings.Repeat("word ", 40), 80); !strings.HasSuffix(got, "\u2026") || len([]rune(got)) > 81 {
		t.Fatalf("long clip %q", got)
	}
}

func TestBridgeDispatch(t *testing.T) {
	env := searchEnv(t)
	ctx := context.Background()
	registerSearchTool(t, "mcp__bridge__send_email", "mcp-bridge", map[string]any{"type": "object", "properties": map[string]any{"to": map[string]any{"type": "string"}}, "required": []any{"to"}})
	registerSearchTool(t, "terminal", "terminal", nil)
	defs := []map[string]any{toolDef("terminal", "run", nil), toolDef("mcp__bridge__send_email", "Send an email through the bridge mail server", map[string]any{"to": map[string]any{"type": "string"}})}
	var parsed map[string]any
	parsed = decodeMap(DispatchToolSearch(ctx, env, map[string]any{}, defs, nil, nil))
	if !strings.Contains(parsed["error"].(string), "queries is required") {
		t.Fatalf("missing queries: %v", parsed)
	}
	parsed = decodeMap(DispatchToolSearch(ctx, env, map[string]any{"queries": []any{"", " "}}, defs, nil, nil))
	if !strings.Contains(parsed["error"].(string), "at least one non-empty string") {
		t.Fatalf("blank queries: %v", parsed)
	}
	many := make([]any, 8)
	for i := range many {
		many[i] = fmt.Sprintf("q%d", i)
	}
	parsed = decodeMap(DispatchToolSearch(ctx, env, map[string]any{"queries": many}, defs, nil, nil))
	if !strings.Contains(parsed["error"].(string), "too many queries") {
		t.Fatalf("overcap: %v", parsed)
	}
	raw := DispatchToolSearch(ctx, env, map[string]any{"queries": "send email"}, defs, nil, nil)
	parsed = decodeMap(raw)
	results, ok := parsed["results"].([]any)
	if !ok {
		t.Fatalf("search result: %s", raw)
	}
	if matches := results[0].(map[string]any)["matches"].([]any); len(matches) != 1 || matches[0] != "mcp__bridge__send_email" {
		t.Fatalf("search: %v", parsed)
	}
	record := parsed["tools"].(map[string]any)["mcp__bridge__send_email"].(map[string]any)
	if record["source"] != "mcp" || record["source_name"] != "mcp-bridge" {
		t.Fatalf("record %v", record)
	}
	parsed = decodeMap(DispatchToolSearch(ctx, env, map[string]any{"queries": []any{"zebra giraffe"}}, defs, nil, nil))
	empty := parsed["results"].([]any)[0].(map[string]any)
	if len(empty["matches"].([]any)) != 0 || empty["available_sources"] == nil || !strings.Contains(empty["hint"].(string), "sources above") {
		t.Fatalf("empty search hint: %v", empty)
	}
	parsed = decodeMap(DispatchToolDescribe(ctx, env, map[string]any{"names": "mcp__bridge__send_email"}, defs, nil, nil))
	described := parsed["tools"].(map[string]any)["mcp__bridge__send_email"].(map[string]any)
	if described["parameters"] == nil {
		t.Fatalf("describe: %v", parsed)
	}
	parsed = decodeMap(DispatchToolDescribe(ctx, env, map[string]any{"names": []any{"terminal", "nope_tool"}}, defs, nil, nil))
	if errs := parsed["errors"].(map[string]any); !strings.Contains(errs["terminal"].(string), "directly-listed") {
		t.Fatalf("describe errors: %v", parsed)
	}
	if nf := parsed["not_found"].([]any); len(nf) != 1 || nf[0] != "nope_tool" {
		t.Fatalf("not found: %v", parsed)
	}
}

func TestResolveUnderlyingCall(t *testing.T) {
	env := searchEnv(t)
	ctx := context.Background()
	registerSearchTool(t, "mcp__res__probe", "mcp-res", nil)
	name, args, errMsg := ResolveUnderlyingCall(ctx, env, map[string]any{"name": "mcp__res__probe", "arguments": map[string]any{"x": 1.0}}, nil)
	if errMsg != "" || name != "mcp__res__probe" || args["x"] != 1.0 {
		t.Fatalf("object args: %s %v %s", name, args, errMsg)
	}
	for _, raw := range []any{nil, "", "   "} {
		name, args, errMsg = ResolveUnderlyingCall(ctx, env, map[string]any{"calls": []any{map[string]any{"name": "mcp__res__probe", "arguments": raw}}}, nil)
		if errMsg != "" || len(args) != 0 {
			t.Fatalf("blank arguments %v: %v %s", raw, args, errMsg)
		}
	}
	_, _, errMsg = ResolveUnderlyingCall(ctx, env, map[string]any{"name": ToolCallName, "arguments": map[string]any{}}, nil)
	if !strings.Contains(errMsg, "cannot invoke") {
		t.Fatalf("recursion: %s", errMsg)
	}
	_, _, errMsg = ResolveUnderlyingCall(ctx, env, map[string]any{"name": "terminal", "arguments": map[string]any{}}, nil)
	if !strings.Contains(errMsg, "directly-listed") {
		t.Fatalf("core tool: %s", errMsg)
	}
	_, _, errMsg = ResolveUnderlyingCall(ctx, env, map[string]any{"name": "terminal", "arguments": map[string]any{}}, map[string]bool{"read_file": true})
	if !strings.Contains(errMsg, "not available in this execution context") {
		t.Fatalf("scoped core tool: %s", errMsg)
	}
	_, _, errMsg = ResolveUnderlyingCall(ctx, env, map[string]any{"name": "probe", "arguments": map[string]any{}}, nil)
	if !strings.Contains(errMsg, "Did you mean 'mcp__res__probe'") {
		t.Fatalf("suffix hint: %s", errMsg)
	}
	entries, errMsg := NormalizeToolCallEntries(map[string]any{"calls": `[{"name":"mcp__res__probe","arguments":"{\"a\":1,\"b\":1.0}"}]`})
	if errMsg != "" || len(entries) != 1 || entries[0].Arguments.Vals["a"] != int64(1) || entries[0].Arguments.Vals["b"] != 1.0 || !slices.Equal(entries[0].Arguments.Keys, []string{"a", "b"}) {
		t.Fatalf("json envelope: %v %s", entries, errMsg)
	}
	if handler := entries[0].HandlerArgs(); handler["a"] != 1.0 || handler["b"] != 1.0 {
		t.Fatalf("handler args: %v", handler)
	}
	_, errMsg = NormalizeToolCallEntries(map[string]any{"calls": []any{map[string]any{"name": "a", "arguments": "not json"}}})
	if errMsg != "tool_call calls[0].arguments is not valid JSON: Expecting value: line 1 column 1 (char 0)" {
		t.Fatalf("invalid json: %s", errMsg)
	}
	batchErr := LocalBatchError([]ToolCallEntry{{Name: "a", Arguments: pyjson.ObjectOf("y", 1.0, "x", int64(1))}, {Name: "b", Arguments: pyjson.NewObject()}})
	if batchErr != `tool_call takes exactly one entry for local tools; you sent 2. Retry with only: {"calls":[{"name":"a","arguments":{"y":1.0,"x":1}}]} then issue the remaining 1 call(s) as separate tool_call invocations. Only connectors__ names may be batched together.` {
		t.Fatalf("batch error: %s", batchErr)
	}
}

func TestToolCallBatchErrorEchoesModelNumbersAsPythonDoes(t *testing.T) {
	env := searchEnv(t)
	registerSearchTool(t, "mcp__res__probe", "mcp-res", nil)
	registerSearchTool(t, "mcp__res__other", "mcp-res", nil)
	want := `Retry with only: {"calls":[{"name":"mcp__res__probe","arguments":{"y":1.0,"x":1,"big":12345678901234567890}}]} then issue`
	for _, raw := range []string{
		`{"calls": [{"name": "mcp__res__probe", "arguments": {"y": 1.0, "x": 1, "big": 12345678901234567890}}, {"name": "mcp__res__other", "arguments": {}}]}`,
		`{"calls": "[{\"name\": \"mcp__res__probe\", \"arguments\": {\"y\": 1.0, \"x\": 1, \"big\": 12345678901234567890}}, {\"name\": \"mcp__res__other\"}]"}`,
		`{"calls": [{"name": "mcp__res__probe", "arguments": "{\"y\": 1.0, \"x\": 1, \"big\": 12345678901234567890}"}, {"name": "mcp__res__other"}]}`,
	} {
		args, parseErr := ParseToolArguments(raw)
		if parseErr != "" {
			t.Fatalf("parse %s: %s", raw, parseErr)
		}
		out := NewDispatcher(Default).Dispatch(context.Background(), &Call{Name: ToolCallName, Args: args, RawArgs: raw, Env: env})
		decoded, err := pyjson.Loads(out)
		if err != nil {
			t.Fatalf("result %s: %v", out, err)
		}
		msg, _ := decoded.(*pyjson.Object).Vals["error"].(string)
		if !strings.Contains(msg, want) {
			t.Fatalf("echo for %s: %s", raw, msg)
		}
	}
}

func TestScopedDeferrableNames(t *testing.T) {
	env := searchEnv(t)
	ctx := context.Background()
	registerSearchTool(t, "mcp__scope__a", "mcp-scope", nil)
	registerSearchTool(t, "mcp__other__b", "mcp-other", nil)
	scoped := ScopedDeferrableNames(ctx, env, []map[string]any{toolDef("mcp__scope__a", "", nil), toolDef("terminal", "", nil)})
	if !scoped["mcp__scope__a"] || scoped["mcp__other__b"] || scoped["terminal"] {
		t.Fatalf("scoped %v", scoped)
	}
	catalog := BuildCatalog(ctx, env, toolSearchDeferrableIn(ctx, env, []map[string]any{toolDef("mcp__scope__a", "alpha", nil)}))
	if len(catalog) != 1 || catalog[0].Name != "mcp__scope__a" {
		t.Fatalf("catalog not scoped: %v", catalog)
	}
	if !strings.Contains(ScopeBlockReason(ctx, env, "mcp__other__b"), "not available in this session") {
		t.Fatal("scope block reason")
	}
}

func TestDeferredCallSchemaProbe(t *testing.T) {
	env := searchEnv(t)
	ctx := context.Background()
	registerSearchTool(t, "mcp_probe_docs_get", "mcp-probe", map[string]any{"type": "object",
		"properties": map[string]any{"document_id": map[string]any{"type": "string", "description": "Doc id"}, "format": map[string]any{"type": "string"}},
		"required":   []any{"document_id"}})
	var parsed map[string]any
	errJSON := ValidateDeferredCallArgs(ctx, env, "mcp_probe_docs_get", map[string]any{})
	parsed = decodeMap(errJSON)
	if !strings.Contains(parsed["error"].(string), "document_id") || parsed["parameters"].(map[string]any)["required"].([]any)[0] != "document_id" {
		t.Fatalf("missing required: %s", errJSON)
	}
	if ValidateDeferredCallArgs(ctx, env, "mcp_no_such_tool_xyz", map[string]any{}) != "" {
		t.Fatal("unknown tool blocked")
	}
	if ValidateDeferredCallArgs(ctx, env, "mcp_probe_docs_get", map[string]any{"document_id": "abc"}) != "" {
		t.Fatal("valid call blocked")
	}
	registerSearchTool(t, "mcp_probe_enum", "mcp-probe-enum", map[string]any{"type": "object",
		"properties": map[string]any{"priority": map[string]any{"type": "string", "enum": []any{"low", "high"}}}, "required": []any{"priority"}})
	parsed = decodeMap(ValidateDeferredCallArgs(ctx, env, "mcp_probe_enum", map[string]any{"priority": "urgent"}))
	if parsed["path"] != "arguments.priority" || parsed["constraint"] != "enum" {
		t.Fatalf("enum: %v", parsed)
	}
	nested := map[string]any{"type": "object", "properties": map[string]any{"options": map[string]any{"type": "object",
		"properties": map[string]any{"count": map[string]any{"type": "integer"}}, "required": []any{"count"}, "additionalProperties": false}},
		"required": []any{"options"}}
	registerSearchTool(t, "mcp_probe_nested", "mcp-probe-nested", nested)
	for _, tc := range []struct {
		args       map[string]any
		path, kind string
	}{
		{map[string]any{"options": map[string]any{"count": "not-an-integer"}}, "arguments.options.count", "type"},
		{map[string]any{"options": map[string]any{}}, "arguments.options", "required"},
		{map[string]any{"options": map[string]any{"count": 1.0, "extra": true}}, "arguments.options", "additionalProperties"},
	} {
		parsed = decodeMap(ValidateDeferredCallArgs(ctx, env, "mcp_probe_nested", tc.args))
		if parsed["path"] != tc.path || parsed["constraint"] != tc.kind {
			t.Fatalf("nested %v: %v", tc.args, parsed)
		}
	}
	registerSearchTool(t, "mcp_probe_coerce", "mcp-probe-coerce", map[string]any{"type": "object", "properties": map[string]any{"count": map[string]any{"type": "integer"}}, "required": []any{"count"}})
	if ValidateDeferredCallArgs(ctx, env, "mcp_probe_coerce", map[string]any{"count": "42"}) != "" {
		t.Fatal("coercible argument blocked")
	}
	registerSearchTool(t, "mcp_probe_nullable", "mcp-probe-nullable", map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string", "nullable": true}}, "required": []any{"value"}})
	if ValidateDeferredCallArgs(ctx, env, "mcp_probe_nullable", map[string]any{"value": nil}) != "" {
		t.Fatal("nullable rejected")
	}
	literal := map[string]any{"nullable": true, "$ref": "literal-not-a-schema"}
	registerSearchTool(t, "mcp_probe_literal", "mcp-probe-literal", map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"enum": []any{literal}}}, "required": []any{"value"}})
	if ValidateDeferredCallArgs(ctx, env, "mcp_probe_literal", map[string]any{"value": literal}) != "" {
		t.Fatal("literal enum object normalized")
	}
	registerSearchTool(t, "mcp_probe_malformed", "mcp-probe-malformed", map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "not-a-json-schema-type"}}})
	if ValidateDeferredCallArgs(ctx, env, "mcp_probe_malformed", map[string]any{"value": "kept"}) != "" {
		t.Fatal("malformed schema did not fail open")
	}
	registerSearchTool(t, "mcp_probe_external", "mcp-probe-external", map[string]any{"type": "object", "properties": map[string]any{"payload": map[string]any{"$ref": "https://example.invalid/schema.json"}}})
	if ValidateDeferredCallArgs(ctx, env, "mcp_probe_external", map[string]any{"payload": map[string]any{"anything": true}}) != "" {
		t.Fatal("external ref did not fail open")
	}
}

func TestBridgeThroughDispatcher(t *testing.T) {
	env := searchEnv(t)
	env.Enabled = []string{"mcp-dispatch"}
	ctx := context.Background()
	registerSearchTool(t, "mcp_dispatch_valid_op", "mcp-dispatch", map[string]any{"type": "object", "properties": map[string]any{"document_id": map[string]any{"type": "string"}}, "required": []any{"document_id"}})
	d := NewDispatcher(Default)
	var posts []PostToolCallEvent
	d.PostToolCall = append(d.PostToolCall, func(ctx context.Context, env *Env, ev PostToolCallEvent) { posts = append(posts, ev) })
	result := d.Dispatch(ctx, &Call{Name: ToolCallName, Args: map[string]any{"name": "mcp_dispatch_valid_op", "arguments": map[string]any{"document_id": "abc"}}, ToolCallID: "c1", Env: env})
	var parsed map[string]any
	parsed = decodeMap(result)
	if parsed["ok"] != true {
		t.Fatalf("dispatch: %s", result)
	}
	if len(posts) != 1 || posts[0].ToolName != "mcp_dispatch_valid_op" {
		t.Fatalf("post hooks %v", posts)
	}
	blocked := d.Dispatch(ctx, &Call{Name: ToolCallName, Args: map[string]any{"name": "mcp_dispatch_valid_op", "arguments": map[string]any{}}, ToolCallID: "c2", Env: env})
	parsed = decodeMap(blocked)
	if !strings.Contains(parsed["error"].(string), "missing required argument") {
		t.Fatalf("probe: %s", blocked)
	}
	loop := d.Dispatch(ctx, &Call{Name: "todo_list", Args: map[string]any{}, Env: env})
	if !strings.Contains(loop, "must be handled by the agent loop") {
		t.Fatalf("agent loop tool: %s", loop)
	}
}
