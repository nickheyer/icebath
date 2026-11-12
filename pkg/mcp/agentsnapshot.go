package mcp

import (
	"context"
	"sort"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// ToolPinStore persists a session tools pin, nil pin clearing it
type ToolPinStore interface {
	UpdateSessionToolNames(sessionID string, pin any) error
}

// AgentToolSnapshot is the live agent surface the tool snapshot maintenance reads, publishes and delegates to
type AgentToolSnapshot interface {
	ToolDefs() []map[string]any
	SetToolSnapshot(defs []map[string]any, names map[string]bool)
	EnabledToolsets() []string
	DisabledToolsets() []string
	SetToolsets(enabled, disabled []string)
	SetConfiguredDisabledToolsets(disabled []string)
	ConfiguredDisabledToolsets() []string
	Platform() string
	SnapshotGeneration() (int64, bool)
	SnapshotSelection() (string, bool)
	RecordSnapshot(generation int64, selection string)
	PublishContextEngineToolNames(names map[string]bool)
	ToolPinStore() ToolPinStore
	SessionID() string
	DelegationBackend() string
	MemoryToolSchemas() func() ([]any, error)
	ContextEngineToolSchemas() func() ([]any, error)
	ToolEnv() *tools.Env
	ToolSurface(defs []map[string]any) []map[string]any
	ToolDrops() map[string]bool
	IsOneshot() bool
	MemoryProviderToolsEnabled(enabled, disabled []string, memoryToolPresent bool) bool
	SavedDelegationBackend() string
	AdoptBuiltBackend(defs []any, backend any)
	AdoptSessionDelegationMode()
	MessageAgentAuthorized() bool
	MessageAgentToolSchema() map[string]any
}

// RefreshOptions selects the toolset overrides and rebuild mode of one snapshot refresh, a nil override meaning none
type RefreshOptions struct {
	EnabledOverride  []string
	DisabledOverride []string
	Quiet            bool
	ContentAware     bool
	PreservePrefix   bool
}

const (
	messageAgentToolName = "message_agent"
	pinnedSnapshotsMax   = 4096
)

type pinnedKey struct{ home, session string }

type pinnedAgainst struct {
	generation int64
	selection  string
}

var (
	agentToolsLock       sync.Mutex
	pinnedSnapshots      = map[pinnedKey]pinnedAgainst{}
	pinnedSnapshotsOrder []pinnedKey
	agentToolRegistry    = func() *tools.Registry { return tools.Default }
	agentToolDefinitions = func(ctx context.Context, env *tools.Env, enabled, disabled []string, quiet bool) []map[string]any {
		return agentToolRegistry().ToolDefinitions(ctx, env, tools.DefinitionOptions{Enabled: enabled, Disabled: disabled, Quiet: quiet})
	}
	toolPinVersion          = defaultToolPinVersion
	persistAgentToolNamesFn = persistAgentToolNames
	reinjectPostBuildFn     = reinjectPostBuildTools
)

// Name of a tool definition, empty when the function block or its name is missing
func snapshotDefName(def map[string]any) string {
	fn, _ := pyjson.Dict(def["function"])
	name, _ := fn["name"].(string)
	return name
}

func agentToolDefs(agent AgentToolSnapshot) []map[string]any {
	return append([]map[string]any(nil), agent.ToolDefs()...)
}

func defNameSet(defs []map[string]any) map[string]bool {
	out := make(map[string]bool, len(defs))
	for _, d := range defs {
		out[snapshotDefName(d)] = true
	}
	return out
}

func sameNameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func defsAsAny(defs []map[string]any) []any {
	out := make([]any, len(defs))
	for i, d := range defs {
		out[i] = d
	}
	return out
}

// AgentToolNames lists the agent tool names in wire order skipping unnamed entries
func AgentToolNames(agent AgentToolSnapshot) []string {
	var out []string
	for _, d := range agentToolDefs(agent) {
		if name := snapshotDefName(d); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// Applies explicit toolset overrides to the agent selection and returns the selection the rebuild uses
func resolveRefreshToolsets(agent AgentToolSnapshot, enabledOverride, disabledOverride []string) ([]string, []string) {
	enabled, disabled := agent.EnabledToolsets(), agent.DisabledToolsets()
	if enabledOverride != nil || disabledOverride != nil {
		if enabledOverride != nil {
			enabled = enabledOverride
		}
		if disabledOverride != nil {
			agent.SetConfiguredDisabledToolsets(disabledOverride)
			disabled = tools.SessionDisabledToolsets(disabledOverride, agent.Platform(), agent.IsOneshot())
		}
		agent.SetToolsets(enabled, disabled)
	}
	return enabled, disabled
}

func dumpToolDefs(defs []map[string]any) (string, error) {
	return pyjson.Dumps(defsAsAny(defs), pyjson.SortKeys(true), pyjson.Separators(",", ":"),
		pyjson.Default(func(v any) (any, error) { return pyrepr.Str(v), nil }))
}

// Reports a serialized byte difference between the live and fresh arrays, false when either fails to serialize
func toolDefsContentChanged(agent AgentToolSnapshot, newDefs []map[string]any) bool {
	current, err := dumpToolDefs(agentToolDefs(agent))
	if err != nil {
		return false
	}
	fresh, err := dumpToolDefs(newDefs)
	if err != nil {
		return false
	}
	return current != fresh
}

// Removes the names the agent never carries from a merged array and its name set
func dropSessionTools(agent AgentToolSnapshot, newDefs []map[string]any, newNames map[string]bool) ([]map[string]any, map[string]bool) {
	drops := agent.ToolDrops()
	if len(drops) == 0 {
		return newDefs, newNames
	}
	kept := make([]map[string]any, 0, len(newDefs))
	for _, d := range newDefs {
		if !drops[snapshotDefName(d)] {
			kept = append(kept, d)
		}
	}
	names := map[string]bool{}
	for n := range newNames {
		if !drops[n] {
			names[n] = true
		}
	}
	return kept, names
}

// Names the freeze holds back, every registered tool plus the Tool Search bridge names
func frozenToolNames(registered map[string]bool) map[string]bool {
	out := make(map[string]bool, len(registered)+len(tools.BridgeToolNames))
	for n := range registered {
		out[n] = true
	}
	for n := range tools.BridgeToolNames {
		out[n] = true
	}
	return out
}

type snapshotPublish struct {
	snapshotGeneration int64
	builtGeneration    int64
	selection          string
	stagedEngineNames  map[string]bool
	contentAware       bool
	prefixRegistered   map[string]bool
}

// Diffs and publishes a rebuilt snapshot atomically, returning the added names or false when nothing was published
func publishToolSnapshot(agent AgentToolSnapshot, newDefs []map[string]any, newNames map[string]bool, p snapshotPublish) (map[string]bool, bool) {
	agentToolsLock.Lock()
	defer agentToolsLock.Unlock()
	publishedGen, ok := agent.SnapshotGeneration()
	if !ok {
		publishedGen = -1
	}
	if p.snapshotGeneration < publishedGen {
		return nil, false
	}
	currentDefs := agentToolDefs(agent)
	current := defNameSet(currentDefs)
	if p.prefixRegistered != nil {
		recorded, hasRecorded := agent.SnapshotSelection()
		frozen := p.builtGeneration == publishedGen && hasRecorded && p.selection == recorded
		held := map[string]bool{}
		if frozen {
			held = frozenToolNames(p.prefixRegistered)
		}
		newDefs, newNames = mergePreservingPrefix(currentDefs, newDefs, p.prefixRegistered, held)
	}
	newDefs, newNames = dropSessionTools(agent, newDefs, newNames)
	agent.RecordSnapshot(max(publishedGen, p.snapshotGeneration), p.selection)
	if sameNameSet(newNames, current) && !(p.contentAware && toolDefsContentChanged(agent, newDefs)) {
		return nil, false
	}
	agent.SetToolSnapshot(newDefs, newNames)
	agent.PublishContextEngineToolNames(p.stagedEngineNames)
	added := map[string]bool{}
	for n := range newNames {
		if !current[n] {
			added[n] = true
		}
	}
	return added, true
}

// Rebuilds a live agent tool snapshot from the registry and returns the newly added tool names
func RefreshAgentMCPTools(ctx context.Context, agent AgentToolSnapshot, opts RefreshOptions) map[string]bool {
	enabled, disabled := resolveRefreshToolsets(agent, opts.EnabledOverride, opts.DisabledOverride)
	reg := agentToolRegistry()
	snapshotGeneration := reg.Generation()
	builtBackend := agent.SavedDelegationBackend()
	env := agent.ToolEnv()
	newDefs := agent.ToolSurface(append([]map[string]any(nil), agentToolDefinitions(ctx, env, enabled, disabled, opts.Quiet)...))
	newNames := defNameSet(newDefs)
	stagedEngineNames := reinjectPostBuildFn(agent, &newDefs, newNames)
	reinjectAuthorizedDynamicTools(agent, &newDefs, newNames)
	contentAware := opts.ContentAware
	var prefixRegistered map[string]bool
	if opts.PreservePrefix {
		prefixRegistered = registeredToolNames(ctx, reg, env)
	} else {
		contentAware = true
	}
	builtGeneration := reg.Generation()
	added, published := publishToolSnapshot(agent, newDefs, newNames, snapshotPublish{
		snapshotGeneration: snapshotGeneration, builtGeneration: builtGeneration,
		selection: tools.ToolSelectionKey(enabled, disabled), stagedEngineNames: stagedEngineNames,
		contentAware: contentAware, prefixRegistered: prefixRegistered})
	backendBefore := agent.DelegationBackend()
	publishedGen, ok := agent.SnapshotGeneration()
	stale := ok && snapshotGeneration < publishedGen
	if prefixRegistered == nil && !stale {
		agent.AdoptBuiltBackend(defsAsAny(newDefs), builtBackend)
	}
	if !published {
		if agent.DelegationBackend() != backendBefore {
			persistAgentToolNamesFn(ctx, agent)
		}
		return map[string]bool{}
	}
	persistAgentToolNamesFn(ctx, agent)
	return added
}

// Registered tool names visible to the agent profile scope
func registeredToolNames(ctx context.Context, reg *tools.Registry, env *tools.Env) map[string]bool {
	scope := ""
	if env != nil {
		scope = tools.ScopeKey(env.Home)
	}
	out := map[string]bool{}
	for _, d := range reg.Snapshot(ctx, scope) {
		out[d.Name] = true
	}
	return out
}

// ReprobeToolAvailability drops the cached availability verdicts and the memoized tool definitions
func ReprobeToolAvailability() {
	tools.InvalidateCheckCache()
	tools.ClearToolDefinitionsCache()
}

// ToolPinVersion is the code identity a tools pin was built by, the build sha else the release version
func ToolPinVersion() string { return toolPinVersion() }

// Build sha or version of the build info identity, else the install stamp base version
func defaultToolPinVersion() string {
	identity := buildInfoCodeIdentity()
	for _, key := range []string{"sha", "version"} {
		if v := identity[key]; pyrepr.Truthy(v) {
			return pyrepr.Str(v)
		}
	}
	return cliDunderVersion()
}

// Code identity of the build info module, unknown until a relaunch certifies the checkout
func buildInfoCodeIdentity() map[string]any {
	return map[string]any{"sha": nil, "short_sha": nil, "version": nil, "source": "unknown"}
}

// Install stamp base version of the repo checkout, 0.0.0 when the stamp has none
func cliDunderVersion() string {
	v, _ := pm.ReadInstallStamp(pm.RepoRoot()).Get("baseVersion")
	if !pyrepr.Truthy(v) {
		return "0.0.0"
	}
	return pyrepr.Str(v)
}

// PersistAgentToolNames writes the agent tools pin to its session row and remembers what it was built against
func PersistAgentToolNames(ctx context.Context, agent AgentToolSnapshot) {
	persistAgentToolNamesFn(ctx, agent)
}

func persistAgentToolNames(ctx context.Context, agent AgentToolSnapshot) {
	db := agent.ToolPinStore()
	sessionID := agent.SessionID()
	if db == nil || sessionID == "" {
		return
	}
	pin := pyjson.NewObject()
	pin.Set("version", toolPinVersion())
	pin.Set("tools", defsAsAny(agentToolDefs(agent)))
	if backend := agent.DelegationBackend(); backend != "" {
		pin.Set("delegation_backend", backend)
	}
	if err := db.UpdateSessionToolNames(sessionID, pin); err != nil {
		mcpLogger().Debug("tool_names persist skipped", "error", err)
		return
	}
	recordPinnedSnapshot(ctx, agent, sessionID)
}

func pinnedSnapshotKey(ctx context.Context, sessionID string) pinnedKey {
	return pinnedKey{home: home.Key(home.Dir(ctx)), session: sessionID}
}

func dropPinnedOrder(key pinnedKey) {
	for i, k := range pinnedSnapshotsOrder {
		if k == key {
			pinnedSnapshotsOrder = append(pinnedSnapshotsOrder[:i:i], pinnedSnapshotsOrder[i+1:]...)
			return
		}
	}
}

// Remembers the generation and selection the pin just written was built against, oldest evicted first
func recordPinnedSnapshot(ctx context.Context, agent AgentToolSnapshot, sessionID string) {
	key := pinnedSnapshotKey(ctx, sessionID)
	generation, genOK := agent.SnapshotGeneration()
	selection, selOK := agent.SnapshotSelection()
	agentToolsLock.Lock()
	defer agentToolsLock.Unlock()
	if _, ok := pinnedSnapshots[key]; ok {
		delete(pinnedSnapshots, key)
		dropPinnedOrder(key)
	}
	if !genOK || !selOK {
		return
	}
	pinnedSnapshots[key] = pinnedAgainst{generation: generation, selection: selection}
	pinnedSnapshotsOrder = append(pinnedSnapshotsOrder, key)
	for len(pinnedSnapshotsOrder) > pinnedSnapshotsMax {
		delete(pinnedSnapshots, pinnedSnapshotsOrder[0])
		pinnedSnapshotsOrder = pinnedSnapshotsOrder[1:]
	}
}

// Whether this process wrote the session pin and the names a restore holds back when nothing moved since
func pinRestoreHeld(ctx context.Context, agent AgentToolSnapshot, registered map[string]bool) (bool, map[string]bool) {
	sessionID := agent.SessionID()
	if sessionID == "" {
		return false, map[string]bool{}
	}
	agentToolsLock.Lock()
	against, pinned := pinnedSnapshots[pinnedSnapshotKey(ctx, sessionID)]
	agentToolsLock.Unlock()
	selection, selOK := agent.SnapshotSelection()
	if !pinned || !selOK || against != (pinnedAgainst{generation: agentToolRegistry().Generation(), selection: selection}) {
		return pinned, map[string]bool{}
	}
	return true, frozenToolNames(registered)
}

// Tool names the agent toolset selection allows before availability checks, client surface toolsets included
func configPermittedNames(ctx context.Context, agent AgentToolSnapshot) map[string]bool {
	enabled := agent.EnabledToolsets()
	if enabled != nil {
		enabled = append(append([]string{}, enabled...), clientSurfaceToolsetNames()...)
	}
	disabled := agent.DisabledToolsets()
	if len(disabled) > 0 && agent.IsOneshot() {
		configured := map[string]bool{}
		for _, n := range agent.ConfiguredDisabledToolsets() {
			configured[n] = true
		}
		oneshotGated := map[string]bool{}
		for _, n := range tools.OneshotDisabledToolsets {
			oneshotGated[n] = true
		}
		kept := []string{}
		for _, n := range disabled {
			if !oneshotGated[n] || configured[n] {
				kept = append(kept, n)
			}
		}
		disabled = kept
	}
	names, _ := agentToolRegistry().SelectToolNames(ctx, agent.ToolEnv(), enabled, disabled, tools.SelectionContext{})
	return names
}

// Client surface toolset names in sorted order
func clientSurfaceToolsetNames() []string {
	out := make([]string, 0, len(tools.ClientSurfaceToolsets))
	for n := range tools.ClientSurfaceToolsets {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func hasFresh(fresh map[string]map[string]any, name string) bool {
	_, ok := fresh[name]
	return ok
}

// Pin in the decoded JSON model with typed Go lists of names or definitions turned into plain lists
func pinValue(v any) any {
	switch t := v.(type) {
	case []map[string]any:
		return pyjson.Plain(defsAsAny(t))
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out
	}
	return pyjson.Plain(v)
}

// Keeps a carried tool only when its dynamic schema gate still admits it against the merged array
func dropGatedCarriedTools(env *tools.Env, merged []map[string]any, carried map[string]bool) []map[string]any {
	available := defNameSet(merged)
	out := make([]map[string]any, 0, len(merged))
	for _, t := range merged {
		name := snapshotDefName(t)
		rewrite := tools.LookupSchemaRewriter(name)
		if !carried[name] || rewrite == nil || rewrite(env, t, available) != nil {
			out = append(out, t)
		}
	}
	return out
}

// Pinned items, whether the same code wrote them and their definitions in pin order
func pinnedToolDefsOf(ctx context.Context, env *tools.Env, saved any, fresh map[string]map[string]any, keepBytes bool) ([]any, bool, []map[string]any) {
	saved = pinValue(saved)
	var raw, version any
	if obj, ok := saved.(map[string]any); ok {
		raw, version = obj["tools"], obj["version"]
		if !pyrepr.Truthy(raw) {
			raw = []any{}
		}
	} else {
		raw = saved
	}
	pinned, ok := raw.([]any)
	if !ok {
		return []any{}, false, []map[string]any{}
	}
	sameCode := version != nil && pyrepr.Equal(version, toolPinVersion())
	reg := agentToolRegistry()
	defs := []map[string]any{}
	for _, item := range pinned {
		var name string
		hasName := false
		dict, isDict := item.(map[string]any)
		switch v := item.(type) {
		case string:
			name, hasName = v, true
		case map[string]any:
			name, hasName = snapshotDefName(v), true
		}
		var def map[string]any
		switch {
		case isDict && (sameCode || keepBytes):
			def = dict
		case hasName && hasFresh(fresh, name):
			def = fresh[name]
		case hasName && name != "":
			if entry := reg.Entry(ctx, env, name); entry != nil {
				fn := make(map[string]any, len(entry.Schema)+1)
				for k, v := range entry.Schema {
					fn[k] = v
				}
				fn["name"] = entry.Name
				def = map[string]any{"type": "function", "function": fn}
			}
		}
		if len(def) > 0 {
			defs = append(defs, def)
		}
	}
	return pinned, sameCode, defs
}

// PinnedToolDefs returns a pin definitions as a fresh agent restores them, keeping pinned bytes
func PinnedToolDefs(ctx context.Context, env *tools.Env, saved any) []map[string]any {
	_, _, defs := pinnedToolDefsOf(ctx, env, saved, map[string]map[string]any{}, true)
	return defs
}

func defsByName(defs []map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any, len(defs))
	for _, d := range defs {
		out[snapshotDefName(d)] = d
	}
	return out
}

// RestoreAgentToolPrefix folds a freshly built agent tools onto the session pin and reports a change
func RestoreAgentToolPrefix(ctx context.Context, agent AgentToolSnapshot, saved any) bool {
	env := agent.ToolEnv()
	freshDefs := agentToolDefs(agent)
	fresh := defsByName(freshDefs)
	pinned, sameCode, pinnedDefs := pinnedToolDefsOf(ctx, env, saved, fresh, false)
	if len(pinned) == 0 {
		return false
	}
	registered := registeredToolNames(ctx, agentToolRegistry(), env)
	carried := map[string]bool{}
	for _, d := range pinnedDefs {
		if name := snapshotDefName(d); !hasFresh(fresh, name) && registered[name] {
			carried[name] = true
		}
	}
	if len(carried) > 0 {
		permitted := configPermittedNames(ctx, agent)
		for name := range carried {
			if !permitted[name] {
				delete(carried, name)
			}
		}
	}
	pinnedHere, held := pinRestoreHeld(ctx, agent, registered)
	if pinnedHere {
		for _, d := range pinnedDefs {
			if name := snapshotDefName(d); tools.BridgeToolNames[name] {
				carried[name] = true
			}
		}
	}
	merged := []map[string]any{}
	for _, d := range pinnedDefs {
		if name := snapshotDefName(d); hasFresh(fresh, name) || carried[name] {
			merged = append(merged, d)
		}
	}
	pinnedNames := defNameSet(merged)
	for _, t := range freshDefs {
		if name := snapshotDefName(t); !pinnedNames[name] && !held[name] {
			merged = append(merged, t)
		}
	}
	merged = dropGatedCarriedTools(env, merged, carried)
	mergedNames := defNameSet(merged)
	reinjectAuthorizedDynamicTools(agent, &merged, mergedNames)
	merged, mergedNames = dropSessionTools(agent, merged, mergedNames)
	changed := !pyListsEqual(defsAsAny(merged), defsAsAny(freshDefs))
	if changed {
		agentToolsLock.Lock()
		agent.SetToolSnapshot(merged, mergedNames)
		agentToolsLock.Unlock()
	}
	if obj, ok := pinValue(saved).(map[string]any); ok && pyrepr.Truthy(obj["delegation_backend"]) {
		agent.AdoptBuiltBackend(pinned, obj["delegation_backend"])
	}
	agent.AdoptSessionDelegationMode()
	if !sameCode || !pyListsEqual(defsAsAny(merged), pinned) {
		persistAgentToolNamesFn(ctx, agent)
	}
	return changed
}

// Python list equality over JSON shaped values, numbers comparing numerically and Go slices as lists
func pyListsEqual(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !pyrepr.Equal(pyJSONValue(a[i]), pyJSONValue(b[i])) {
			return false
		}
	}
	return true
}

// Value in the decoded JSON model so typed Go slices and maps compare like their Python counterparts
func pyJSONValue(v any) any {
	text, err := pyjson.Dumps(v)
	if err != nil {
		return v
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return v
	}
	return pyjson.Plain(decoded)
}

// Folds a fresh snapshot into the live one keeping existing slots and bytes, appending new names unless held
func mergePreservingPrefix(currentDefs, newDefs []map[string]any, registered, held map[string]bool) ([]map[string]any, map[string]bool) {
	fresh := map[string]map[string]any{}
	var order []string
	for _, entry := range newDefs {
		name := snapshotDefName(entry)
		if name == "" {
			continue
		}
		if _, seen := fresh[name]; !seen {
			order = append(order, name)
		}
		fresh[name] = entry
	}
	merged := []map[string]any{}
	for _, entry := range currentDefs {
		name := snapshotDefName(entry)
		_, replaced := fresh[name]
		delete(fresh, name)
		if replaced || tools.BridgeToolNames[name] || (name != "" && registered[name]) {
			merged = append(merged, entry)
		}
	}
	for _, name := range order {
		if entry, ok := fresh[name]; ok && !held[name] {
			merged = append(merged, entry)
		}
	}
	return merged, defNameSet(merged)
}

// Scrubs any staged message_agent copy and re-adds it only when the live gate authorizes the agent
func reinjectAuthorizedDynamicTools(agent AgentToolSnapshot, toolsList *[]map[string]any, nameSet map[string]bool) {
	kept := (*toolsList)[:0:0]
	for _, entry := range *toolsList {
		if snapshotDefName(entry) != messageAgentToolName {
			kept = append(kept, entry)
		}
	}
	*toolsList = kept
	delete(nameSet, messageAgentToolName)
	if agent.MessageAgentAuthorized() {
		*toolsList = append(*toolsList, agent.MessageAgentToolSchema())
		nameSet[messageAgentToolName] = true
	}
}

// Appends memory provider and context engine tools onto the staged pair, returning the routing names it claimed
func reinjectPostBuildTools(agent AgentToolSnapshot, toolsList *[]map[string]any, nameSet map[string]bool) map[string]bool {
	add := func(schema any) (string, bool) {
		s, ok := pyjson.Plain(schema).(map[string]any)
		if !ok {
			return "", false
		}
		name, _ := s["name"].(string)
		if name == "" || nameSet[name] {
			return "", false
		}
		*toolsList = append(*toolsList, map[string]any{"type": "function", "function": s})
		nameSet[name] = true
		return name, true
	}
	enabled := agent.EnabledToolsets()
	if getMem := agent.MemoryToolSchemas(); getMem != nil {
		if agent.MemoryProviderToolsEnabled(enabled, agent.DisabledToolsets(), nameSet["memory"]) {
			if schemas, err := getMem(); err != nil {
				mcpLogger().Debug("Memory-provider tool re-injection skipped", "error", err)
			} else {
				for _, schema := range schemas {
					add(schema)
				}
			}
		}
	}
	staged := map[string]bool{}
	getSchemas := agent.ContextEngineToolSchemas()
	if (enabled == nil || containsString(enabled, "context_engine")) && getSchemas != nil {
		schemas, err := getSchemas()
		if err != nil {
			mcpLogger().Debug("Context-engine tool re-injection skipped", "error", err)
			return staged
		}
		for _, schema := range schemas {
			if name, ok := add(schema); ok {
				staged[name] = true
			}
		}
	}
	return staged
}
