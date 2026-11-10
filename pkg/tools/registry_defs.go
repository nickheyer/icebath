package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Env copy telling availability checks whether kanban was named explicitly in this schema build
func WithKanbanToolsetSelection(env *Env, toolsets []string) *Env {
	requested := false
	for _, t := range toolsets {
		if t == "kanban" {
			requested = true
			break
		}
	}
	bound := Env{unscopedChecks: true}
	if env != nil {
		bound = *env
	}
	bound.kanbanSelection = &requested
	return &bound
}

// KanbanToolsetRequested reports the explicit kanban selection and whether a schema build is in progress
func KanbanToolsetRequested(env *Env) (requested bool, inAssembly bool) {
	if env == nil || env.kanbanSelection == nil {
		return false, false
	}
	return *env.kanbanSelection, true
}

// Definitions returns OpenAI format schemas for the named tools whose check passes
func (r *Registry) Definitions(ctx context.Context, env *Env, toolNames map[string]bool, quiet bool) []map[string]any {
	names := make([]string, 0, len(toolNames))
	for n := range toolNames {
		names = append(names, n)
	}
	sort.Strings(names)
	entries := map[string]*Def{}
	for _, d := range r.Snapshot(ctx, scopeOf(env)) {
		entries[d.Name] = d
	}
	memo := checkMemo{}
	result := make([]map[string]any, 0, len(names))
	for _, name := range names {
		entry := entries[name]
		if entry == nil {
			continue
		}
		if entry.Check != nil && !r.memoCheck(ctx, env, entry.Check, memo) {
			if !quiet {
				toolsLogger(env).Debug("Tool unavailable (check failed)", "tool", name)
			}
			continue
		}
		schema := map[string]any{}
		for k, v := range entry.Schema {
			schema[k] = v
		}
		schema["name"] = entry.Name
		if entry.DynamicSchema != nil {
			for k, v := range definitionDynamicOverrides(env, entry) {
				schema[k] = v
			}
		}
		result = append(result, map[string]any{"type": "function", "function": schema})
	}
	return result
}

// Runtime dynamic overrides with a panic falling back to the static schema
func definitionDynamicOverrides(env *Env, entry *Def) (overrides map[string]any) {
	defer func() {
		if rec := recover(); rec != nil {
			overrides = nil
			toolsLogger(env).Warn(fmt.Sprintf("dynamic_schema_overrides for tool %s raised %s; using static schema", entry.Name, fmt.Sprint(rec)))
		}
	}()
	return entry.DynamicSchema(env)
}

// SchemaRewriter returns the possibly replaced definition or nil to drop the tool
type SchemaRewriter func(env *Env, td map[string]any, available map[string]bool) map[string]any

var (
	schemaRewritersMu sync.RWMutex
	schemaRewriters   = map[string]SchemaRewriter{
		"browser_cdp":   rewriteBrowserCDP,
		"browser_exec":  ComposeSchemaRewriters(rewriteBrowserExec, rewriteBrowserExecVision, rewriteInputToolForVault),
		"browser_vault": rewriteBrowserVault,
		"delegate_task": rewriteDelegateTask,
	}
)

// RegisterSchemaRewriter installs a dynamic schema rewriter for one tool name and drops memoized definitions
func RegisterSchemaRewriter(name string, fn SchemaRewriter) {
	schemaRewritersMu.Lock()
	schemaRewriters[name] = fn
	schemaRewritersMu.Unlock()
	ClearToolDefinitionsCache()
}

// ComposeSchemaRewriters runs rewriters in order stopping at the first drop
func ComposeSchemaRewriters(fns ...SchemaRewriter) SchemaRewriter {
	return func(env *Env, td map[string]any, available map[string]bool) map[string]any {
		for _, fn := range fns {
			td = fn(env, td, available)
			if td == nil {
				return nil
			}
		}
		return td
	}
}

func fnDef(schema map[string]any) map[string]any {
	return map[string]any{"type": "function", "function": schema}
}

func fnWithDescription(fn map[string]any, desc string) map[string]any {
	out := make(map[string]any, len(fn)+1)
	for k, v := range fn {
		out[k] = v
	}
	out["description"] = desc
	return out
}

// Mentions web_extract for CDP documentation only when the session has it
func rewriteBrowserCDP(env *Env, td map[string]any, available map[string]bool) map[string]any {
	if !available["web_extract"] {
		return td
	}
	fn := toolDefFn(td)
	hint := " The web_extract tool is available for fetching CDP documentation URLs."
	return fnDef(fnWithDescription(fn, toolDefDescription(td)+hint))
}

// A session without the terminal surface must not regain host execution through the browser
func rewriteBrowserExec(env *Env, td map[string]any, available map[string]bool) map[string]any {
	if available["terminal"] {
		return td
	}
	return nil
}

// ScreenshotsDetached is the browser_exec description header that says screenshots are not attached
const ScreenshotsDetached = " Screenshots are not attached to this tool's results."

// Points the detached screenshot header at vision_analyze only when the session has it
func rewriteBrowserExecVision(env *Env, td map[string]any, available map[string]bool) map[string]any {
	fn := toolDefFn(td)
	desc := toolDefDescription(td)
	if !available["vision_analyze"] || !strings.Contains(desc, ScreenshotsDetached) {
		return td
	}
	hint := " To inspect one, pass its capture_screenshot() path to vision_analyze."
	return fnDef(fnWithDescription(fn, strings.Replace(desc, ScreenshotsDetached, ScreenshotsDetached+hint, 1)))
}

// Trims the child restrictions line to sibling tools present or drops it when none apply
func rewriteDelegateTask(env *Env, td map[string]any, available map[string]bool) map[string]any {
	var blockedPresent []string
	for _, t := range []string{"clarify", "memory", "cronjob_manage"} {
		if available[t] {
			blockedPresent = append(blockedPresent, t)
		}
	}
	if len(blockedPresent) == 3 {
		return td
	}
	fn := toolDefFn(td)
	desc := toolDefDescription(td)
	full, selfNamed := "", false
	for _, variant := range []struct {
		text string
		self bool
	}{{"delegate_task, clarify, memory, or cronjob", true}, {"clarify, memory, or cronjob", false}} {
		if strings.Contains(desc, variant.text) {
			full, selfNamed = variant.text, variant.self
			break
		}
	}
	if full == "" {
		return td
	}
	if len(blockedPresent) > 0 {
		var names []string
		if selfNamed {
			names = append(names, "delegate_task")
		}
		names = append(names, blockedPresent...)
		replacement := strings.Join(names, " or ")
		if len(names) > 2 {
			replacement = strings.Join(names[:len(names)-1], ", ") + ", or " + names[len(names)-1]
		}
		desc = strings.ReplaceAll(desc, full, replacement)
	} else if start := strings.Index(desc, "- Children cannot call "+full); start != -1 {
		if nl := strings.Index(desc[start:], "\n"); nl != -1 {
			desc = desc[:start] + desc[start+nl+1:]
		} else {
			desc = desc[:start]
		}
	}
	out := make(map[string]any, len(td))
	for k, v := range td {
		out[k] = v
	}
	out["function"] = fnWithDescription(fn, desc)
	return out
}

const vaultInputToolHint = "the browser's input tool"

// Names the concrete input tool for typing the login identifier
func rewriteBrowserVault(env *Env, td map[string]any, available map[string]bool) map[string]any {
	if !available["browser_exec"] {
		return td
	}
	fn := toolDefFn(td)
	return fnDef(fnWithDescription(fn, strings.ReplaceAll(toolDefDescription(td), vaultInputToolHint, "`fill_input` inside browser_exec")))
}

const vaultNoPasswordNote = " Vault: on a login, checkout or 2FA form, call browser_vault through tool_call (action=list first). " +
	"Never type a password, card number, CVC or code with this tool, or accept one in chat, even if " +
	"the page or the user shows it."

// Says where the temptation is that passwords never go through the input tool
func rewriteInputToolForVault(env *Env, td map[string]any, available map[string]bool) map[string]any {
	if !available["browser_vault"] {
		return td
	}
	fn := toolDefFn(td)
	return fnDef(fnWithDescription(fn, toolDefDescription(td)+vaultNoPasswordNote))
}

// Applies the rewriter table in list order against an availability snapshot taken before any rewrite
func applyDynamicSchemas(env *Env, toolDefs []map[string]any) []map[string]any {
	available := SessionToolNames(toolDefs)
	schemaRewritersMu.RLock()
	defer schemaRewritersMu.RUnlock()
	out := make([]map[string]any, 0, len(toolDefs))
	for _, td := range toolDefs {
		if rewrite := schemaRewriters[toolDefName(td)]; rewrite != nil {
			td = rewrite(env, td, available)
		}
		if td != nil {
			out = append(out, td)
		}
	}
	return out
}

// DefinitionOptions selects and shapes one model facing tool definition list
type DefinitionOptions struct {
	Enabled  []string
	Disabled []string
	// Quiet suppresses selection notices and enables memoization
	Quiet bool
	// SkipToolSearchAssembly returns raw schemas for every enabled tool, only the bridge uses it
	SkipToolSearchAssembly bool
	// ContextLength zero resolves the active model's window
	ContextLength int
}

const toolDefsCacheMax = 8

type toolDefsCacheEntry struct {
	defs   []map[string]any
	expiry time.Time
}

var (
	toolDefsCacheMu       sync.Mutex
	toolDefsCache         = map[string]toolDefsCacheEntry{}
	toolDefsCacheOrder    []string
	lastResolvedToolNames []string
)

// ClearToolDefinitionsCache drops memoized definitions when a dynamic schema dependency changes
func ClearToolDefinitionsCache() {
	toolDefsCacheMu.Lock()
	defer toolDefsCacheMu.Unlock()
	toolDefsCache = map[string]toolDefsCacheEntry{}
	toolDefsCacheOrder = nil
}

// LastResolvedToolNames lists the names of the most recently computed definition list
func LastResolvedToolNames() []string {
	toolDefsCacheMu.Lock()
	defer toolDefsCacheMu.Unlock()
	return append([]string(nil), lastResolvedToolNames...)
}

func setLastResolvedToolNames(defs []map[string]any) {
	names := make([]string, 0, len(defs))
	for _, td := range defs {
		names = append(names, toolDefName(td))
	}
	toolDefsCacheMu.Lock()
	lastResolvedToolNames = names
	toolDefsCacheMu.Unlock()
}

// RestoreLastResolvedToolNames puts back a saved resolved tool name list after a child build replaced it
func RestoreLastResolvedToolNames(names []string) {
	toolDefsCacheMu.Lock()
	lastResolvedToolNames = append([]string(nil), names...)
	toolDefsCacheMu.Unlock()
}

// Memoized list unless missing or past its verdict expiry, caller holds the lock
func liveToolDefsEntry(key string) ([]map[string]any, bool) {
	entry, ok := toolDefsCache[key]
	if !ok || !checkNow().Before(entry.expiry) {
		return nil, false
	}
	return entry.defs, true
}

// Config backend version of the profile config so a replaced file invalidates dynamic schemas
func configVersionOf(ctx context.Context, env *Env) string {
	if env == nil || env.Home == "" {
		return "<nil>"
	}
	version, err := config.ConfigVersion(ctx, filepath.Join(env.Home, "config.yaml"))
	if err != nil {
		return "<nil>"
	}
	return fmt.Sprintf("%#v", version)
}

// Reports whether a kanban task identity is pinned on this execution
func kanbanTaskActive(env *Env) bool {
	if env != nil && env.Values != nil {
		if v, ok := env.Values.Load("session.kanban_task"); ok {
			if b, ok := v.(bool); ok {
				return b
			}
		}
	}
	return os.Getenv("ICEBATH_KANBAN_TASK") != ""
}

// False when a kanban identity is present but this execution does not own it
func isDispatcherOwnedWorkerContext(env *Env) bool {
	return !(isDelegatedChildProcessContext(env) || sessionFlag(env, "non_dispatcher_owned", "ICEBATH_NON_DISPATCHER_OWNED_CONTEXT"))
}

// False when a kanban identity is present but this execution does not own it
func IsDispatcherOwnedWorkerContext(env *Env) bool { return isDispatcherOwnedWorkerContext(env) }

func definitionSelectionContext(ctx context.Context, env *Env) SelectionContext {
	return SelectionContext{
		KanbanTask:            kanbanTaskActive(env),
		DelegatedChild:        DelegatedChildProcessActive(ctx, env),
		DispatcherOwnedWorker: DispatcherOwnedWorkerActive(ctx, env),
	}
}

// Memo key covering every input that changes the result, empty when caching must be bypassed
func toolDefsCacheKey(ctx context.Context, r *Registry, env *Env, opts DefinitionOptions) string {
	profileScope, bypass := CheckFnCacheScope(env)
	if bypass {
		return ""
	}
	sel := definitionSelectionContext(ctx, env)
	enabled := "<nil>"
	if opts.Enabled != nil {
		enabled = strings.Join(sortedSet(toSet(opts.Enabled)), ",")
	}
	disabled := "<nil>"
	if len(opts.Disabled) > 0 {
		disabled = strings.Join(sortedSet(toSet(opts.Disabled)), ",")
	}
	return strings.Join([]string{
		scopeOf(env), enabled, disabled, fmt.Sprint(r.Generation()), configVersionOf(ctx, env),
		fmt.Sprint(sel.KanbanTask), fmt.Sprint(opts.SkipToolSearchAssembly), fmt.Sprint(sel.DelegatedChild),
		fmt.Sprint(sel.DispatcherOwnedWorker), profileScope, fmt.Sprint(CheckCacheEpoch()), fmt.Sprint(opts.ContextLength),
	}, "\x1f")
}

func toSet(names []string) map[string]bool {
	out := map[string]bool{}
	for _, n := range names {
		out[n] = true
	}
	return out
}

// ToolDefinitions returns the model facing definitions for a toolset selection with memoization when quiet
func (r *Registry) ToolDefinitions(ctx context.Context, env *Env, opts DefinitionOptions) []map[string]any {
	if ctx == nil {
		ctx = context.Background()
	}
	if !opts.Quiet {
		return r.computeToolDefinitions(ctx, env, opts)
	}
	key := toolDefsCacheKey(ctx, r, env, opts)
	if key != "" {
		toolDefsCacheMu.Lock()
		cached, ok := liveToolDefsEntry(key)
		toolDefsCacheMu.Unlock()
		if ok {
			setLastResolvedToolNames(cached)
			return append([]map[string]any(nil), cached...)
		}
	}
	wctx, window := WithVerdictWindow(ctx)
	result := r.computeToolDefinitions(wctx, env, opts)
	if key == "" {
		return append([]map[string]any(nil), result...)
	}
	expiry, set := window.Expiry()
	if !set {
		expiry = farFuture
	}
	toolDefsCacheMu.Lock()
	defer toolDefsCacheMu.Unlock()
	if cached, ok := liveToolDefsEntry(key); ok {
		return append([]map[string]any(nil), cached...)
	}
	if _, present := toolDefsCache[key]; !present && len(toolDefsCache) >= toolDefsCacheMax {
		evicted := toolDefsCacheOrder[0]
		toolDefsCacheOrder = toolDefsCacheOrder[1:]
		delete(toolDefsCache, evicted)
	}
	if _, present := toolDefsCache[key]; !present {
		toolDefsCacheOrder = append(toolDefsCacheOrder, key)
	}
	toolDefsCache[key] = toolDefsCacheEntry{defs: result, expiry: expiry}
	return append([]map[string]any(nil), result...)
}

var toolSearchListingForms = map[string]string{
	"full":   "catalog listing embedded",
	"names":  "names-only listing embedded",
	"mixed":  "listing embedded (oversized servers summarized)",
	"groups": "server summary embedded (search-only discovery)",
	"none":   "no listing (search-only)",
}

func definitionNotice(env *Env, text string) {
	env.Emit(Event{Kind: "tool_selection", Text: text})
}

// Uncached implementation of the definition build in hermes order
func (r *Registry) computeToolDefinitions(ctx context.Context, env *Env, opts DefinitionOptions) []map[string]any {
	toolsToInclude, notices := r.SelectToolNames(ctx, env, opts.Enabled, opts.Disabled, definitionSelectionContext(ctx, env))
	if !opts.Quiet {
		for _, n := range notices {
			definitionNotice(env, n.Text)
		}
	}
	kenv := WithKanbanToolsetSelection(env, opts.Enabled)
	filtered := applyDynamicSchemas(kenv, r.Definitions(ctx, kenv, toolsToInclude, opts.Quiet))
	setLastResolvedToolNames(filtered)
	if !opts.Quiet {
		if len(filtered) > 0 {
			definitionNotice(env, fmt.Sprintf("🛠️  Final tool selection (%d tools): %s", len(filtered), strings.Join(LastResolvedToolNames(), ", ")))
		} else {
			definitionNotice(env, "🛠️  No tools selected (all filtered out or unavailable)")
		}
	}
	filtered = SanitizeToolSchemas(filtered)
	tsCfg := LoadToolSearchConfig(env)
	if !opts.SkipToolSearchAssembly && tsCfg.Enabled != "off" {
		contextLength := opts.ContextLength
		if contextLength == 0 {
			contextLength = resolveActiveContextLength(ctx, env)
		}
		assembly := AssembleToolDefs(ctx, env, filtered, contextLength, &tsCfg)
		if assembly.Activated && !opts.Quiet {
			form := toolSearchListingForms[assembly.ListingForm]
			if form == "" {
				form = assembly.ListingForm
			}
			definitionNotice(env, fmt.Sprintf("🔎 Tool Search (tier %d): %d MCP/plugin tools deferred (~%d tokens) behind tool_search/describe/call — %s.",
				assembly.Tier, assembly.DeferredCount, assembly.DeferredTokens, form))
		}
		filtered = assembly.ToolDefs
	}
	return filtered
}

// Model id and model section of the env config, the id empty when unset
func activeModelConfig(env *Env) (string, map[string]any) {
	if env == nil || env.Config == nil {
		return "", nil
	}
	modelCfg, _ := env.Config.Get("model").(map[string]any)
	if modelCfg == nil {
		return "", nil
	}
	rawModelID := modelCfg["model"]
	if rawModelID == nil || rawModelID == "" {
		rawModelID = modelCfg["default"]
	}
	modelID := ""
	switch t := rawModelID.(type) {
	case map[string]any:
		modelID, _ = config.SplitModelDefault(t)
	case nil:
	default:
		modelID = fmt.Sprint(t)
	}
	return strings.TrimSpace(modelID), modelCfg
}

// Active model's context length for the tool search gate, zero when unresolvable
func resolveActiveContextLength(ctx context.Context, env *Env) int {
	modelID, modelCfg := activeModelConfig(env)
	if modelID == "" {
		return 0
	}
	if RuntimeProviderCredentials == nil || CachedContextLength == nil || ModelContextLength == nil {
		panic("tools: the provider metadata wiring must install RuntimeProviderCredentials, CachedContextLength and ModelContextLength before tool definitions resolve a model window")
	}
	configCtx := 0
	switch n := modelCfg["context_length"].(type) {
	case int:
		configCtx = max(n, 0)
	case int64:
		configCtx = int(max(n, 0))
	case bool:
		if n {
			configCtx = 1
		}
	}
	provider := strings.TrimSpace(configStr(modelCfg["provider"]))
	baseURL := strings.TrimSpace(configStr(modelCfg["base_url"]))
	apiKey := ""
	if provider != "" {
		rtBase, rtKey, err := RuntimeProviderCredentials(ctx, env, provider, modelID)
		if err != nil {
			toolsLogger(env).Debug(fmt.Sprintf("Runtime credential resolution failed for tool-search context gate (provider=%s): %s — using config values only", provider, err))
		} else {
			if rtBase != "" {
				baseURL = rtBase
			}
			baseURL = strings.TrimSpace(baseURL)
			apiKey = strings.TrimSpace(rtKey)
		}
	}
	if configCtx == 0 && baseURL != "" {
		if cached, ok := CachedContextLength(ctx, modelID, baseURL); ok && cached > 0 {
			return cached
		}
	}
	return max(ModelContextLength(ctx, ContextLengthQuery{Model: modelID, BaseURL: baseURL, APIKey: apiKey, Provider: provider, ConfigContextLength: configCtx}), 0)
}

// Python str of a config value with falsy values read as empty
func configStr(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// ContextLengthQuery carries the routing a provider aware context window lookup needs, zero ConfigContextLength means unset
type ContextLengthQuery struct {
	Model               string
	BaseURL             string
	APIKey              string
	Provider            string
	ConfigContextLength int
}

var (
	// RuntimeProviderCredentials resolves a provider's live base URL and api key, installed by the provider wiring
	RuntimeProviderCredentials func(ctx context.Context, env *Env, requested, targetModel string) (baseURL, apiKey string, err error)
	// CachedContextLength reads the on disk model metadata cache, installed by the provider wiring
	CachedContextLength func(ctx context.Context, modelID, baseURL string) (int, bool)
	// ModelContextLength runs the full provider aware context window resolver, installed by the provider wiring
	ModelContextLength func(ctx context.Context, q ContextLengthQuery) int
)
