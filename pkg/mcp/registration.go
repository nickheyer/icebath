package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"weak"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// ErrEditorSchemaChanged is the state authority refusal when an editor session server changes its frozen manifest
var ErrEditorSchemaChanged = errors.New("acp_mcp_schema_changed")

const utilityOriginPrefix = "generated utility "

const editorSessionScopePrefix = "editor-session:"

// Reports whether a registry scope belongs to an editor session, raw or canonicalized as a scope key
func isEditorSessionScope(scope string) bool {
	return strings.HasPrefix(scope, editorSessionScopePrefix) || (scope != "" && strings.HasPrefix(scope, tools.ScopeKey(editorSessionScopePrefix)))
}

// Shared scope reconciliations serialize per scope on one of sixteen hashed locks
var scopeRefreshLocks [16]sync.Mutex

// Utility tool key to the handler factory taking the server name and tool timeout
var utilityHandlerFactories map[string]func(serverName string, toolTimeout float64) tools.Handler

// Fills the factory table at init so handlers that reach the manager form no initialization cycle
func init() {
	utilityHandlerFactories = map[string]func(serverName string, toolTimeout float64) tools.Handler{
		"list_resources": makeListResourcesHandler, "read_resource": makeReadResourceHandler,
		"list_prompts": makeListPromptsHandler, "get_prompt": makeGetPromptHandler,
		"list_event_types": makeListEventTypesHandler, "subscribe_event": makeSubscribeEventHandler,
		"unsubscribe_event": makeUnsubscribeEventHandler,
	}
}

// Frozen editor session manifests live exactly as long as their server task
var (
	editorManifestsMu sync.Mutex
	editorManifests   = map[weak.Pointer[serverTask]]string{}
)

// Reports whether an editor session already froze this server's tool manifest
func editorManifestFrozen(s *serverTask) bool {
	editorManifestsMu.Lock()
	defer editorManifestsMu.Unlock()
	_, ok := editorManifests[weak.Make(s)]
	return ok
}

// Returns the manifest already frozen for the task, freezing the given one on first use
func freezeEditorManifest(s *serverTask, manifest string) string {
	wp := weak.Make(s)
	editorManifestsMu.Lock()
	defer editorManifestsMu.Unlock()
	if frozen, ok := editorManifests[wp]; ok {
		return frozen
	}
	editorManifests[wp] = manifest
	runtime.AddCleanup(s, func(wp weak.Pointer[serverTask]) {
		editorManifestsMu.Lock()
		delete(editorManifests, wp)
		editorManifestsMu.Unlock()
	}, wp)
	return manifest
}

// True only for an exact boolean true readOnlyHint, unknown counts as write capable
func annotationReadOnlyHint(t *Tool) bool { return t.ReadOnly() }

// Stores or clears the declared cost policy under key, caller holds mcpMu
func recordCostPolicy(serverName string, cfg map[string]any, key ServerKey) error {
	policy, err := ConfigCostPolicy(serverName, cfg)
	if err != nil {
		return err
	}
	if policy == nil {
		delete(serverCostPolicies, key)
	} else {
		serverCostPolicies[key] = policy
	}
	return nil
}

// Records trust tier, cost policy and per tool read only hints of a server under its connection key
func recordToolTrustMetadata(ctx context.Context, serverName string, cfg map[string]any, list []*Tool, key *ServerKey) error {
	mcpMu.Lock()
	defer mcpMu.Unlock()
	k := serverKey(ctx, serverName, "", true)
	if key != nil {
		k = *key
	}
	serverTrustLevels[k] = normalizeServerTrust(cfg["trust"])
	if err := recordCostPolicy(serverName, cfg, k); err != nil {
		return err
	}
	hints := toolReadOnlyHints[k]
	if hints == nil {
		hints = map[string]bool{}
		toolReadOnlyHints[k] = hints
	}
	for _, t := range list {
		if t != nil && t.Name != "" {
			hints[t.Name] = annotationReadOnlyHint(t)
		}
	}
	return nil
}

// Warns when tool_costs names tools the live server does not offer
func warnUnknownCostKeys(serverName string, key ServerKey, toolNames []string) {
	mcpMu.Lock()
	policy := serverCostPolicies[key]
	mcpMu.Unlock()
	unknown := UnknownCostKeys(policy, toolNames)
	if len(unknown) == 0 {
		return
	}
	items := make([]any, len(unknown))
	for i, u := range unknown {
		items[i] = u
	}
	mcpLogger().Warn(fmt.Sprintf("MCP server '%s': tool_costs names %s, which the server does not offer, so those calls "+
		"carry no cost and no spend limit. Use the server's own tool names (without Icebath's '%s%s__' prefix).",
		serverName, pyRepr(items), ToolNamePrefix, serverName))
}

// Records the consuming profile's own trust tier and cost policy for an adopted connection
func recordScopeTrust(ctx context.Context, serverName string, cfg map[string]any, scope string) error {
	mcpMu.Lock()
	defer mcpMu.Unlock()
	key := serverKey(ctx, serverName, scope, false)
	serverTrustLevels[key] = normalizeServerTrust(cfg["trust"])
	return recordCostPolicy(serverName, cfg, key)
}

// Remembers the raw server that registered a tool
func trackMCPToolServer(toolName, serverName string) {
	mcpMu.Lock()
	mcpToolServerNames[toolName] = serverName
	mcpMu.Unlock()
}

// Exported tool server tracking for packages that simulate a live MCP registration
func TrackMCPToolServer(toolName, serverName string) { trackMCPToolServer(toolName, serverName) }

// Forgets the server provenance of a deregistered tool
func forgetMCPToolServer(toolName string) {
	mcpMu.Lock()
	delete(mcpToolServerNames, toolName)
	mcpMu.Unlock()
}

// Exported provenance removal paired with the exported tracking
func ForgetMCPToolServer(toolName string) { forgetMCPToolServer(toolName) }

// Connection key of a live task found by identity, else the task name's key in the current scope
func serverKeyForTask(ctx context.Context, s *serverTask) ServerKey {
	mcpMu.Lock()
	defer mcpMu.Unlock()
	for key, live := range mcpServers {
		if live == s {
			return key
		}
	}
	return serverKey(ctx, s.name, "", true)
}

// Deregisters one tool of a live server from every profile overlay that owns it
func deregisterMCPToolAllScopes(ctx context.Context, s *serverTask, toolName string) {
	deregisterMCPToolForKey(ctx, serverKeyForTask(ctx, s), toolName)
}

// Deregisters one tool of the connection under key from every profile overlay that owns it
func deregisterMCPToolForKey(ctx context.Context, key ServerKey, toolName string) {
	mcpMu.Lock()
	var scopes []string
	for scope := range serverToolScopes[key] {
		scopes = append(scopes, scope)
	}
	if len(scopes) == 0 {
		scopes = []string{serverRegistryScope(ctx, key)}
	}
	mcpMu.Unlock()
	for _, scope := range scopes {
		_ = tools.Default.Deregister(toolName, scope, "")
	}
	forgetMCPToolServer(toolName)
	restoreServerToolsetAlias(key)
}

// Keeps the per name toolset alias while any profile still owns tools of a same named server
func restoreServerToolsetAlias(key ServerKey) {
	type owned struct {
		server *serverTask
		scopes []string
	}
	mcpMu.Lock()
	var list []owned
	for k, server := range mcpServers {
		if k.Name != key.Name {
			continue
		}
		o := owned{server: server}
		for scope := range serverToolScopes[k] {
			o.scopes = append(o.scopes, scope)
		}
		list = append(list, o)
	}
	mcpMu.Unlock()
	for _, o := range list {
		names := o.server.registeredToolNames()
		for _, scope := range o.scopes {
			for _, name := range names {
				if tools.Default.SnapshotRegistration(scope, name) != nil {
					tools.Default.RegisterToolsetAlias(key.Name, "mcp-"+key.Name)
					return
				}
			}
		}
	}
}

// Removes one profile's overlay of a shared live connection and its trust and cost records
func removeServerScope(ctx context.Context, key ServerKey, scope string) {
	for _, name := range tools.Default.ToolNamesForToolset(ctx, scope, "mcp-"+key.Name) {
		_ = tools.Default.Deregister(name, scope, "")
	}
	mcpMu.Lock()
	if scopes := serverToolScopes[key]; scopes != nil {
		delete(scopes, scope)
		if len(scopes) == 0 {
			delete(serverToolScopes, key)
		}
	}
	own := serverKey(ctx, key.Name, scope, false)
	delete(serverTrustLevels, own)
	delete(serverCostPolicies, own)
	mcpMu.Unlock()
	restoreServerToolsetAlias(key)
}

// Map view of a tools config block, empty when absent or not a mapping
func toolsFilterBlock(cfg map[string]any) map[string]any {
	if m, ok := cfg["tools"].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Utility schemas the config enables and the server advertises, events only when opted in
func selectUtilitySchemas(serverName string, s *serverTask, cfg map[string]any) []UtilitySchema {
	filter := toolsFilterBlock(cfg)
	enabled := map[string]bool{
		"resources": parseBoolish(filter["resources"], true),
		"prompts":   parseBoolish(filter["prompts"], true),
	}
	s.mu.Lock()
	var advertised *ServerCapabilities
	if s.initializeResult != nil {
		advertised = s.initializeResult.Capabilities
	}
	hasInit := s.initializeResult != nil
	hasSession := s.session != nil
	s.mu.Unlock()
	skipReason := func(handlerKey string) string {
		family := utilityCapabilityAttrs[handlerKey]
		if family == "events" {
			if eventsEnabled(cfg) {
				return ""
			}
			return "events not enabled"
		}
		if !enabled[family] {
			return family + " disabled"
		}
		if hasInit && advertised != nil {
			if !advertised.Has(family) {
				return fmt.Sprintf("server does not advertise '%s' capability", family)
			}
			return ""
		}
		if hasSession {
			return ""
		}
		return "session lacks " + handlerKey
	}
	var selected []UtilitySchema
	for _, entry := range buildUtilitySchemas(serverName) {
		if reason := skipReason(entry.HandlerKey); reason != "" {
			mcpLogger().Debug(fmt.Sprintf("MCP server '%s': skipping utility '%s' (%s)", serverName, entry.HandlerKey, reason))
			continue
		}
		selected = append(selected, entry)
	}
	return selected
}

// Rows of utility schemas in the cache and candidate shape
func utilityRows(list []UtilitySchema) []any {
	out := make([]any, len(list))
	for i, u := range list {
		out[i] = map[string]any{"schema": u.Schema, "handler_key": u.HandlerKey}
	}
	return out
}

// Registered MCP tool names of every connected and lazily cached server visible to the current scope
func existingToolNames(ctx context.Context) []string {
	scope := mcpRegistryScope(ctx)
	if scope != "" {
		mcpMu.Lock()
		var servers []string
		for key := range mcpServers {
			if serverVisibleInScope(key, scope) {
				servers = append(servers, key.Name)
			}
		}
		for key := range lazyServerToolNames {
			if _, live := mcpServers[key]; !live && serverVisibleInScope(key, scope) {
				servers = append(servers, key.Name)
			}
		}
		mcpMu.Unlock()
		set := map[string]bool{}
		for _, server := range servers {
			for _, name := range tools.Default.ToolNamesForToolset(ctx, scope, "mcp-"+server) {
				set[name] = true
			}
		}
		out := make([]string, 0, len(set))
		for name := range set {
			out = append(out, name)
		}
		sort.Strings(out)
		return out
	}
	mcpMu.Lock()
	keys := make([]ServerKey, 0, len(mcpServers))
	for key := range mcpServers {
		keys = append(keys, key)
	}
	sortServerKeys(keys)
	live := make([]*serverTask, len(keys))
	for i, key := range keys {
		live[i] = mcpServers[key]
	}
	var lazyKeys []ServerKey
	for key := range lazyServerToolNames {
		if _, ok := mcpServers[key]; !ok {
			lazyKeys = append(lazyKeys, key)
		}
	}
	sortServerKeys(lazyKeys)
	var lazyNames []string
	for _, key := range lazyKeys {
		lazyNames = append(lazyNames, lazyServerToolNames[key]...)
	}
	mcpMu.Unlock()
	var names []string
	for _, s := range live {
		names = append(names, s.registeredToolNames()...)
	}
	return append(names, lazyNames...)
}

// Orders connection keys by scope then name
func sortServerKeys(keys []ServerKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Scope != keys[j].Scope {
			return keys[i].Scope < keys[j].Scope
		}
		return keys[i].Name < keys[j].Name
	})
}

// Include whitelist wins over exclude blacklist, entries exact names or globs, an empty include registers nothing
func makeToolFilter(serverName string, cfg map[string]any) func(string) bool {
	filter := toolsFilterBlock(cfg)
	includeRaw := filter["include"]
	include := normalizeNameFilter(includeRaw, fmt.Sprintf("mcp_servers.%s.tools.include", serverName))
	exclude := normalizeNameFilter(filter["exclude"], fmt.Sprintf("mcp_servers.%s.tools.exclude", serverName))
	switch includeRaw.(type) {
	case string, []any, []string:
		return func(name string) bool { return MatchesNameFilter(name, include) }
	}
	return func(name string) bool { return !(len(exclude) > 0 && MatchesNameFilter(name, exclude)) }
}

// Schema cache rows as tools, rows without a mapping shape or a name dropped, bad annotations write capable
func cachedTools(raws []any) []*Tool {
	var out []*Tool
	for _, raw := range raws {
		row, ok := raw.(map[string]any)
		if !ok || !pyTruthy(row["name"]) {
			continue
		}
		t := &Tool{Name: pyStr(row["name"]), InputSchema: map[string]any{}}
		if pyTruthy(row["description"]) {
			t.Description = pyStr(row["description"])
		}
		if schema, ok := row["inputSchema"].(map[string]any); ok {
			t.InputSchema = schema
		}
		if ann, ok := row["annotations"].(map[string]any); ok {
			hint, present := ann["readOnlyHint"]
			if !present {
				hint = ann["read_only_hint"]
			}
			b, isBool := hint.(bool)
			readOnly := isBool && b
			t.Annotations = &ToolAnnotations{ReadOnlyHint: &readOnly}
		}
		out = append(out, t)
	}
	return out
}

// One registration attempt of a native tool or a generated utility with its provenance text
type candidate struct {
	registryName string
	origin       string
	schema       map[string]any
	handler      tools.Handler
}

// Reports whether the candidate is a generated utility rather than a server native tool
func (c candidate) isUtility() bool { return strings.HasPrefix(c.origin, utilityOriginPrefix) }

// Native tools to candidates after the config filter, the injection scan running on live and cached tools alike
func toolCandidates(serverName string, list []*Tool, shouldRegister func(string) bool, toolTimeout float64) []candidate {
	var out []candidate
	for _, t := range list {
		if !shouldRegister(t.Name) {
			mcpLogger().Debug(fmt.Sprintf("MCP server '%s': skipping tool '%s' (filtered by config)", serverName, t.Name))
			continue
		}
		scanDescription(serverName, t.Name, t.Description)
		schema := convertToolSchema(serverName, t)
		out = append(out, candidate{
			registryName: pyStr(schema["name"]), origin: "tool " + pyReprStr(t.Name), schema: schema,
			handler: makeToolHandler(serverName, t.Name, toolTimeout, true),
		})
	}
	return out
}

// Utility rows from live selection or cache to candidates, malformed rows dropped
func utilityCandidates(serverName string, entries []any, toolTimeout float64) []candidate {
	var out []candidate
	for _, raw := range entries {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		schema, ok := row["schema"].(map[string]any)
		key, isStr := row["handler_key"].(string)
		factory, known := utilityHandlerFactories[key]
		if !ok || !isStr || !known || !pyTruthy(schema["name"]) {
			continue
		}
		out = append(out, candidate{
			registryName: pyStr(schema["name"]), origin: utilityOriginPrefix + pyReprStr(key), schema: schema,
			handler: factory(serverName, toolTimeout),
		})
	}
	return out
}

// Drops exact duplicates, lets a native tool shadow a colliding utility and skips every other colliding name
func resolveNameCollisions(serverName string, candidates []candidate) []candidate {
	var unique []candidate
	originsByName := map[string]map[string]bool{}
	var order []string
	for _, c := range candidates {
		origins := originsByName[c.registryName]
		if origins == nil {
			origins = map[string]bool{}
			originsByName[c.registryName] = origins
			order = append(order, c.registryName)
		}
		if origins[c.origin] {
			mcpLogger().Debug(fmt.Sprintf("MCP server '%s': duplicate registration candidate %s for '%s'; keeping one", serverName, c.origin, c.registryName))
			continue
		}
		origins[c.origin] = true
		unique = append(unique, c)
	}
	ambiguous := map[string][]string{}
	shadowed := map[[2]string]bool{}
	for _, name := range order {
		origins := originsByName[name]
		if len(origins) <= 1 {
			continue
		}
		var utility, native []string
		for o := range origins {
			if strings.HasPrefix(o, utilityOriginPrefix) {
				utility = append(utility, o)
			} else {
				native = append(native, o)
			}
		}
		sort.Strings(utility)
		sort.Strings(native)
		if len(native) == 1 && len(utility) > 0 {
			for _, o := range utility {
				shadowed[[2]string{name, o}] = true
			}
			mcpLogger().Info(fmt.Sprintf("MCP server '%s': generated utility %s normalizes onto server-native %s \u2014 keeping the native tool "+
				"and dropping the utility (the utility only applies when the server has no such tool of its own)",
				serverName, strings.Join(utility, ", "), native[0]))
			continue
		}
		all := append(append([]string{}, utility...), native...)
		sort.Strings(all)
		ambiguous[name] = all
	}
	names := make([]string, 0, len(ambiguous))
	for name := range ambiguous {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		mcpLogger().Error(fmt.Sprintf("MCP server '%s': name normalization collision for '%s' from %s; skipping every colliding "+
			"entry instead of choosing an arbitrary handler", serverName, name, strings.Join(ambiguous[name], ", ")))
	}
	var out []candidate
	for _, c := range unique {
		if _, bad := ambiguous[c.registryName]; bad || shadowed[[2]string{c.registryName, c.origin}] {
			continue
		}
		out = append(out, c)
	}
	return out
}

// Per server execution timeout from the raw config timeout, zero when absent or not a number
func executionTimeout(raw any) time.Duration {
	if raw == nil {
		return 0
	}
	f, ok := config.AsFloat(raw)
	if !ok || math.IsNaN(f) || f <= 0 {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}

// Registers candidates under toolset mcp-name and returns the names that landed, the registry being the atomic gate
func registerCandidates(ctx context.Context, serverName string, candidates []candidate, check func(*tools.Env) bool,
	scope func() string, lazy bool, key *ServerKey, timeout any) []string {
	toolset := "mcp-" + serverName
	scopeValue := tools.ScopeKey(scope())
	k := serverKey(ctx, serverName, scopeValue, false)
	if key != nil {
		k = *key
	}
	var registered []string
	for _, c := range candidates {
		existing := tools.Default.ToolsetForTool(ctx, scopeValue, c.registryName)
		if existing != "" && existing != toolset {
			switch {
			case lazy:
				if !c.isUtility() {
					mcpLogger().Warn(fmt.Sprintf("MCP server '%s' (lazy): cached tool '%s' collides with toolset '%s' \u2014 skipping", serverName, c.registryName, existing))
				}
			case strings.HasPrefix(existing, "mcp-"):
				mcpLogger().Error(fmt.Sprintf("MCP server '%s': %s normalizes to '%s', already owned by MCP toolset '%s' \u2014 skipping to "+
					"preserve the existing owner", serverName, c.origin, c.registryName, existing))
			default:
				mcpLogger().Warn(fmt.Sprintf("MCP server '%s': %s (\u2192 '%s') collides with built-in tool in toolset '%s' \u2014 skipping to "+
					"preserve built-in", serverName, c.origin, c.registryName, existing))
			}
			continue
		}
		desc, _ := c.schema["description"].(string)
		err := tools.Default.RegisterScoped(scopeValue, tools.Def{
			Name: c.registryName, Toolset: toolset, Schema: c.schema, Handler: c.handler, Check: check,
			Description: desc, Timeout: executionTimeout(timeout),
		})
		if err == nil && tools.Default.ToolsetForTool(ctx, scopeValue, c.registryName) == toolset {
			trackMCPToolServer(c.registryName, serverName)
			if scopeValue != "" {
				mcpMu.Lock()
				scopes := serverToolScopes[k]
				if scopes == nil {
					scopes = map[string]bool{}
					serverToolScopes[k] = scopes
				}
				scopes[scopeValue] = true
				mcpMu.Unlock()
			}
			registered = append(registered, c.registryName)
		} else if !lazy {
			mcpLogger().Error(fmt.Sprintf("MCP server '%s': registration of %s as '%s' was rejected by the registry; "+
				"skipping provenance/count updates", serverName, c.origin, c.registryName))
		}
	}
	if len(registered) > 0 {
		tools.Default.RegisterToolsetAlias(serverName, toolset)
	}
	return registered
}

// Persists the filtered live manifest so the next startup can register the server lazily, failures only logged
func writeSchemaCache(ctx context.Context, serverName string, s *serverTask, cfg map[string]any, shouldRegister func(string) bool) {
	s.mu.Lock()
	list := append([]*Tool(nil), s.tools...)
	meta := s.listCacheMeta
	s.mu.Unlock()
	payload := []any{}
	for _, t := range list {
		if !shouldRegister(t.Name) {
			continue
		}
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{}
		}
		payload = append(payload, map[string]any{
			"name": t.Name, "description": t.Description, "inputSchema": schema,
			"annotations": map[string]any{"readOnlyHint": annotationReadOnlyHint(t)},
		})
	}
	utility := utilityRows(selectUtilitySchemas(serverName, s, cfg))
	if err := WriteCacheEntry(ctx, serverName, ConfigFingerprint(cfg), payload, utility, meta.TTLMs, meta.CacheScope); err != nil {
		mcpLogger().Debug(fmt.Sprintf("MCP schema cache write failed for '%s': %s", serverName, excStr(err)))
	}
}

// Registers a connected server's tools and utilities, refusing a changed manifest inside an editor session scope
func registerServerTools(ctx context.Context, name string, s *serverTask, cfg map[string]any) ([]string, error) {
	shouldRegister := makeToolFilter(name, cfg)
	key := serverKeyForTask(ctx, s)
	s.mu.Lock()
	list := append([]*Tool(nil), s.tools...)
	toolTimeout := s.toolTimeout
	s.mu.Unlock()
	if err := recordToolTrustMetadata(ctx, name, cfg, list, &key); err != nil {
		return nil, err
	}
	var names []string
	for _, t := range list {
		if t != nil && t.Name != "" {
			names = append(names, t.Name)
		}
	}
	warnUnknownCostKeys(name, key, names)
	candidates := toolCandidates(name, list, shouldRegister, toolTimeout)
	candidates = append(candidates, utilityCandidates(name, utilityRows(selectUtilitySchemas(name, s, cfg)), toolTimeout)...)
	ownerScope := func() string {
		mcpMu.Lock()
		defer mcpMu.Unlock()
		return serverRegistryScope(ctx, key)
	}
	if isEditorSessionScope(ownerScope()) {
		manifest := map[string]any{}
		for _, c := range candidates {
			manifest[c.registryName] = c.schema
		}
		text := pyDumpsSorted(manifest)
		if freezeEditorManifest(s, text) != text {
			return nil, ErrEditorSchemaChanged
		}
	}
	registered := registerCandidates(ctx, name, resolveNameCollisions(name, candidates), makeCheckFn(name),
		ownerScope, false, &key, cfg["timeout"])
	if len(registered) > 0 && !isEditorSessionScope(ownerScope()) {
		writeSchemaCache(ctx, name, s, cfg, shouldRegister)
	}
	return registered, nil
}

// Python json.dumps with sort_keys, default separators, ensure_ascii and str for unknown values
func pyDumpsSorted(v any) string {
	var b strings.Builder
	writePyJSON(&b, v)
	return b.String()
}

// Writes one value in Python json.dumps form
func writePyJSON(b *strings.Builder, v any) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(pyJSONCompactASCII(k))
			b.WriteString(": ")
			writePyJSON(b, x[k])
		}
		b.WriteByte('}')
	case map[string]string:
		m := make(map[string]any, len(x))
		for k, s := range x {
			m[k] = s
		}
		writePyJSON(b, m)
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			writePyJSON(b, item)
		}
		b.WriteByte(']')
	case []string:
		items := make([]any, len(x))
		for i, s := range x {
			items[i] = s
		}
		writePyJSON(b, items)
	case json.Number:
		if i, err := strconv.ParseInt(string(x), 10, 64); err == nil {
			b.WriteString(strconv.FormatInt(i, 10))
		} else if f, err := x.Float64(); err == nil {
			b.WriteString(pyJSONCompactASCII(f))
		} else {
			b.WriteString(pyJSONCompactASCII(string(x)))
		}
	case int32:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case uint:
		b.WriteString(strconv.FormatUint(uint64(x), 10))
	case uint32:
		b.WriteString(strconv.FormatUint(uint64(x), 10))
	case float32:
		b.WriteString(pyJSONCompactASCII(float64(x)))
	default:
		b.WriteString(pyJSONCompactASCII(v))
	}
}

// Python equality class of a scalar config value, booleans and numbers comparing numerically
func identityScalar(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "n:1.0"
		}
		return "n:0.0"
	case string:
		return "s:" + x
	}
	if f, ok := pyFloat(v); ok {
		return "n:" + pyFloatRepr(f)
	}
	return "r:" + pyDumpsSorted(v)
}

// Value of a config entry frozen for identity comparison, falsy values reading as an empty mapping
func frozenIdentityValue(v any) string {
	if !pyTruthy(v) {
		return "{}"
	}
	return pyDumpsSorted(v)
}

// Normalised auth type of a server config
func authType(cfg map[string]any) string {
	if !pyTruthy(cfg["auth"]) {
		return ""
	}
	return pyStrip(strings.ToLower(pyStr(cfg["auth"])))
}

// Route fingerprint plus everything that authenticates or secures a connection
func connectionIdentity(cfg map[string]any) [8]string {
	sslVerify, ok := cfg["ssl_verify"]
	if !ok {
		sslVerify = true
	}
	strict := "False"
	if pyTruthy(cfg["strict_redirect_headers"]) {
		strict = "True"
	}
	return [8]string{
		ConfigFingerprint(cfg), frozenIdentityValue(cfg["env"]), frozenIdentityValue(cfg["headers"]), authType(cfg),
		frozenIdentityValue(cfg["client_cert"]), frozenIdentityValue(cfg["client_key"]), identityScalar(sslVerify), strict,
	}
}

// Hash of a connection's resolved inputs, the inputs themselves carry secrets
func identityDigest(inputs []any) string {
	sum := sha256.Sum256([]byte(pyDumpsSorted(inputs)))
	return hex.EncodeToString(sum[:])
}

// Digest of what this profile would connect with, false when it cannot connect or a resolver fails
func adopterIdentityDigest(ctx context.Context, env *tools.Env, serverName string, r *Rendered) (string, bool) {
	cfg := r.Config
	if _, hasURL := cfg["url"]; !hasURL && !pyTruthy(cfg["command"]) {
		return "", false
	}
	inputs, _, err := connectInputs(ctx, env, serverName, r)
	if err != nil {
		var unavailable *LiveEndpointUnavailableError
		if !errors.As(err, &unavailable) {
			mcpLogger().Warn(fmt.Sprintf("MCP server '%s': cannot resolve this profile's connection identity (%s); "+
				"not adopting another profile's connection", serverName, excStr(err)))
		}
		return "", false
	}
	return identityDigest(inputs), true
}

// Whether a live server matches a config, across profiles also its resolved identity and never under OAuth
func sameServerRoute(s *serverTask, cfg map[string]any, crossProfile bool, resolved string, resolvedOK bool) bool {
	s.mu.Lock()
	own := s.config
	recorded := s.resolvedIdentity
	s.mu.Unlock()
	if own == nil {
		own = map[string]any{}
	}
	if connectionIdentity(own) != connectionIdentity(cfg) {
		return false
	}
	if !crossProfile {
		return true
	}
	return authType(cfg) != "oauth" && recorded != "" && resolvedOK && recorded == resolved
}

// Serializes shared scope reconciliation and registration for one discovery pass
func registerConnectedIntoCurrentScope(ctx context.Context, env *tools.Env, servers map[string]*Rendered) (int, error) {
	scope := mcpRegistryScope(ctx)
	if scope == "" {
		return 0, nil
	}
	h := fnv.New32a()
	h.Write([]byte(scope))
	lock := &scopeRefreshLocks[h.Sum32()%uint32(len(scopeRefreshLocks))]
	lock.Lock()
	defer lock.Unlock()
	return healConnectedIntoCurrentScope(ctx, env, servers)
}

// Heals the current profile's overlay from shared live connections with the same route and credentials
func healConnectedIntoCurrentScope(ctx context.Context, env *tools.Env, servers map[string]*Rendered) (int, error) {
	scope := mcpRegistryScope(ctx)
	if scope == "" {
		return 0, nil
	}
	mcpMu.Lock()
	omitted := map[string]bool{}
	for key, scopes := range serverToolScopes {
		if _, named := servers[key.Name]; scopes[scope] && !named {
			omitted[key.Name] = true
		}
	}
	foreign := map[string]bool{}
	for key := range mcpServers {
		if key.Scope != scope {
			foreign[key.Name] = true
		}
	}
	mcpMu.Unlock()
	ownerCtx := installOwnerSecretScope(ctx)
	ownEnv := ownerEnv(ctx, env)
	profileServers := map[string]*Rendered{}
	if len(omitted) > 0 {
		profileServers = LoadServerConfigs(ownerCtx, ownEnv)
	}
	judged := map[string]*Rendered{}
	for name := range omitted {
		judged[name] = profileServers[name]
	}
	for name, r := range servers {
		judged[name] = r
	}
	type digest struct {
		value string
		ok    bool
	}
	resolved := map[string]digest{}
	for name, r := range judged {
		if foreign[name] && r != nil && ServerEnabled(r.Config) {
			v, ok := adopterIdentityDigest(ownerCtx, ownEnv, name, r)
			resolved[name] = digest{v, ok}
		}
	}
	type candidateKey struct {
		key    ServerKey
		server *serverTask
		cfg    *Rendered
	}
	mcpMu.Lock()
	var judging []candidateKey
	for key, scopes := range serverToolScopes {
		if !scopes[scope] {
			continue
		}
		r, ok := judged[key.Name]
		if !ok {
			continue
		}
		judging = append(judging, candidateKey{key, mcpServers[key], r})
	}
	mcpMu.Unlock()
	var stale []ServerKey
	for _, j := range judging {
		d := resolved[j.key.Name]
		if j.cfg == nil || !ServerEnabled(j.cfg.Config) || j.server == nil || j.server.currentSession() == nil ||
			!sameServerRoute(j.server, j.cfg.Config, j.key.Scope != scope, d.value, d.ok) {
			stale = append(stale, j.key)
		}
	}
	for _, key := range stale {
		removeServerScope(ctx, key, scope)
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	registeredServers := 0
	for _, name := range names {
		r := servers[name]
		if r == nil || !ServerEnabled(r.Config) {
			continue
		}
		cfg := r.Config
		mcpMu.Lock()
		_, own := mcpServers[serverKey(ctx, name, scope, false)]
		var live []candidateKey
		if !own {
			for key, server := range mcpServers {
				if key.Name == name {
					live = append(live, candidateKey{key: key, server: server})
				}
			}
		}
		mcpMu.Unlock()
		if own {
			continue
		}
		sort.Slice(live, func(i, j int) bool { return live[i].key.Scope < live[j].key.Scope })
		d := resolved[name]
		var shared *candidateKey
		for i := range live {
			if live[i].server.currentSession() != nil && sameServerRoute(live[i].server, cfg, true, d.value, d.ok) {
				shared = &live[i]
				break
			}
		}
		if shared == nil {
			continue
		}
		mcpMu.Lock()
		scopes := serverToolScopes[shared.key]
		if scopes == nil {
			scopes = map[string]bool{}
			serverToolScopes[shared.key] = scopes
		}
		scopes[scope] = true
		mcpMu.Unlock()
		if err := recordScopeTrust(ctx, name, cfg, scope); err != nil {
			return registeredServers, err
		}
		if len(tools.Default.ToolNamesForToolset(ctx, scope, "mcp-"+name)) > 0 {
			continue
		}
		server := shared.server
		server.mu.Lock()
		list := append([]*Tool(nil), server.tools...)
		toolTimeout := server.toolTimeout
		server.mu.Unlock()
		candidates := toolCandidates(name, list, makeToolFilter(name, cfg), toolTimeout)
		candidates = append(candidates, utilityCandidates(name, utilityRows(selectUtilitySchemas(name, server, cfg)), toolTimeout)...)
		key := shared.key
		landed := registerCandidates(ctx, name, resolveNameCollisions(name, candidates), makeCheckFn(name),
			func() string { return scope }, false, &key, cfg["timeout"])
		if len(landed) == 0 {
			continue
		}
		registeredServers++
		server.mu.Lock()
		union := map[string]bool{}
		for _, n := range server.toolNames {
			union[n] = true
		}
		for _, n := range landed {
			union[n] = true
		}
		merged := make([]string, 0, len(union))
		for n := range union {
			merged = append(merged, n)
		}
		sort.Strings(merged)
		server.toolNames = merged
		server.mu.Unlock()
	}
	return registeredServers, nil
}

// Registers a server from its cached manifest without spawning it, trust recorded first so gating matches live
func registerFromCacheSync(ctx context.Context, env *tools.Env, name string, cfg map[string]any, entry map[string]any) ([]string, error) {
	var tree config.Tree
	if env != nil {
		tree = env.Config
	}
	toolTimeout := resolveToolTimeout(cfg, tree)
	cached := cachedTools(ToolsFromCacheEntry(entry))
	if err := recordToolTrustMetadata(ctx, name, cfg, cached, nil); err != nil {
		return nil, err
	}
	candidates := toolCandidates(name, cached, makeToolFilter(name, cfg), toolTimeout)
	candidates = append(candidates, utilityCandidates(name, UtilityToolsFromCacheEntry(entry), toolTimeout)...)
	registered := registerCandidates(ctx, name, candidates, makeCheckFn(name),
		func() string { return mcpRegistryScope(ctx) }, true, nil, cfg["timeout"])
	if len(registered) > 0 {
		mcpMu.Lock()
		key := serverKey(ctx, name, "", true)
		copied := make(map[string]any, len(cfg))
		for k, v := range cfg {
			copied[k] = v
		}
		lazyServerConfigs[key] = copied
		lazyServerFingerprints[key] = ConfigFingerprint(cfg)
		lazyServerToolNames[key] = append([]string(nil), registered...)
		mcpMu.Unlock()
		mcpLogger().Info(fmt.Sprintf("MCP server '%s' (lazy): registered %d tool(s) from schema cache", name, len(registered)))
	}
	return registered, nil
}
