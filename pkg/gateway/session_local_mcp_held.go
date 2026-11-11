package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Held editor connections and borrowed editor server lists of one authority, both keyed by session scope
type editorMCPState struct {
	mu       sync.Mutex
	held     map[string]*editorHeld
	borrowed map[string]any
}

// One session scope's editor connections, borrowed by running executions and kept between them only while viewed
type editorHeld struct {
	mu          sync.Mutex
	users       int
	fingerprint *string
}

// Ledger scope of one session's editor servers, never shared with another session or profile
func sessionScope(a *SessionAuthority, sid string) string {
	sum := sha256.Sum256([]byte(a.ProfileID + ":" + sid))
	return "editor-session:" + hex.EncodeToString(sum[:])
}

// First n hex digits of the sha256 of text
func shortDigest(text string, n int) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:n]
}

// Server specs of a validated editor server list
func editorSpecs(servers any) []map[string]any {
	list, _ := pyjson.Plain(servers).([]any)
	specs := make([]map[string]any, len(list))
	for i, item := range list {
		specs[i], _ = item.(map[string]any)
	}
	return specs
}

// Ledger name per server index, the editor's own name plus a per session tag
func editorServerNames(scope string, specs []map[string]any) map[int]string {
	names := make(map[int]string, len(specs))
	for i, spec := range specs {
		names[i] = fmt.Sprintf("editor_%s_%s", mcp.SanitizeNameComponent(pyrepr.Str(spec["name"])), shortDigest(fmt.Sprintf("%s:%d", scope, i), 8))
	}
	return names
}

// Names a session's manifest was frozen under, derived from its scope for a manifest that predates them
func frozenEditorNames(data map[string]any, specs []map[string]any) (map[int]string, error) {
	if raw, ok := data["names"].(map[string]any); ok {
		names := make(map[int]string, len(raw))
		for key, name := range raw {
			i, err := strconv.Atoi(key)
			if err != nil {
				return nil, &PyError{Kind: "ValueError", Msg: "invalid literal for int() with base 10: " + pyrepr.Repr(key)}
			}
			names[i] = pyrepr.Str(name)
		}
		return names, nil
	}
	prefix := shortDigest(pyrepr.Str(data["scope"]), 20)
	names := make(map[int]string, len(specs))
	for i := range specs {
		names[i] = fmt.Sprintf("editor_%s_%d", prefix, i)
	}
	return names, nil
}

// Whether a toolset is one of the editor toolsets frozen in a manifest
func frozenEditorToolset(data map[string]any, toolset string) bool {
	if len(data) == 0 {
		return false
	}
	if raw, ok := data["names"].(map[string]any); ok {
		for _, name := range raw {
			if toolset == "mcp-"+pyrepr.Str(name) {
				return true
			}
		}
		return false
	}
	return strings.HasPrefix(toolset, "mcp-editor_"+shortDigest(pyrepr.Str(data["scope"]), 20)+"_")
}

// Name and value pairs of an env or headers list as a mapping
func editorPairs(spec map[string]any, key string) map[string]any {
	out := map[string]any{}
	items, _ := spec[key].([]any)
	for _, item := range items {
		if pair, ok := item.(map[string]any); ok {
			out[pyrepr.Str(pair["name"])] = pair["value"]
		}
	}
	return out
}

// MCP server configs of the named editor servers with one bounded eager discovery each
func editorConfigs(specs []map[string]any, names map[int]string, cwd string) map[string]map[string]any {
	configs := make(map[string]map[string]any, len(names))
	for i, name := range names {
		spec := specs[i]
		kind, has := spec["type"]
		if !has {
			kind = "stdio"
		}
		var config map[string]any
		if kind == "stdio" {
			config = map[string]any{"command": spec["command"], "args": spec["args"], "env": editorPairs(spec, "env"), "cwd": cwd}
		} else {
			config = map[string]any{"url": spec["url"], "headers": editorPairs(spec, "headers")}
			if kind == "sse" {
				config["transport"] = "sse"
			}
		}
		config["connect_timeout"], config["tool_timeout"], config["lazy"] = 5, 30, false
		configs[name] = config
	}
	return configs
}

// Held connection record of a scope, created on first use
func heldEditorMCP(a *SessionAuthority, scope string) *editorHeld {
	a.editorMCP.mu.Lock()
	defer a.editorMCP.mu.Unlock()
	if a.editorMCP.held == nil {
		a.editorMCP.held = map[string]*editorHeld{}
	}
	held := a.editorMCP.held[scope]
	if held == nil {
		held = &editorHeld{}
		a.editorMCP.held[scope] = held
	}
	return held
}

// Borrowed server list of a scope and whether one is held
func borrowedEditorMCP(a *SessionAuthority, scope string) (any, bool) {
	a.editorMCP.mu.Lock()
	defer a.editorMCP.mu.Unlock()
	servers, ok := a.editorMCP.borrowed[scope]
	return servers, ok
}

// Records or drops the borrowed server list of a scope
func setBorrowedEditorMCP(a *SessionAuthority, scope string, servers any, drop bool) {
	a.editorMCP.mu.Lock()
	defer a.editorMCP.mu.Unlock()
	if drop {
		delete(a.editorMCP.borrowed, scope)
		return
	}
	if a.editorMCP.borrowed == nil {
		a.editorMCP.borrowed = map[string]any{}
	}
	a.editorMCP.borrowed[scope] = servers
}

// Whether any session of the scope has a viewer
func viewedEditorScope(a *SessionAuthority, scope string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for sid, live := range a.Sessions {
		if len(live.Subscribers) > 0 && sessionScope(a, sid) == scope {
			return true
		}
	}
	return false
}

// Closes a scope's connections and forgets their fingerprint, caller holds the held lock
func disconnectEditorMCP(ctx context.Context, held *editorHeld, scope string) {
	mcp.DisconnectScope(ctx, scope)
	held.fingerprint = nil
}

// Runs body with the schemas and connected indexes of the scope's servers, reusing a held connection
func borrowEditorMCP(ctx context.Context, a *SessionAuthority, scope string, specs []map[string]any, names map[int]string, cwd string, partial bool,
	body func(schemas map[string]any, connected []int) error) error {
	held := heldEditorMCP(a, scope)
	list := make([]any, len(specs))
	for i, spec := range specs {
		list[i] = spec
	}
	wanted, err := editorMCPFingerprint([]any{list, cwd})
	if err != nil {
		return err
	}
	scoped := tools.WithSessionToolScope(ctx, scope)
	held.mu.Lock()
	if held.fingerprint != nil && *held.fingerprint != wanted {
		if held.users > 0 {
			held.mu.Unlock()
			return runtimeRefusal("session_busy")
		}
		disconnectEditorMCP(ctx, held, scope)
	}
	connected := mcp.EditorConnected(scope, names)
	if len(connected) != len(names) {
		env := &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}
		if _, err := mcp.RegisterRawServers(scoped, env, editorConfigs(specs, names, cwd)); err != nil {
			held.mu.Unlock()
			return err
		}
		connected = mcp.EditorConnected(scope, names)
	}
	held.fingerprint = &wanted
	held.users++
	held.mu.Unlock()
	defer func() {
		held.mu.Lock()
		defer held.mu.Unlock()
		held.users--
		if held.users == 0 && !viewedEditorScope(a, scope) {
			disconnectEditorMCP(ctx, held, scope)
		}
	}()
	if len(connected) != len(names) && !partial {
		return runtimeRefusal("acp_mcp_discovery_failed")
	}
	toolsets := map[string]bool{}
	for _, i := range connected {
		toolsets["mcp-"+names[i]] = true
	}
	schemas := map[string]any{}
	for _, name := range tools.Default.AllToolNames(scoped, "") {
		if toolsets[tools.Default.ToolsetForTool(scoped, "", name)] {
			schemas[name] = tools.Default.Schema(scoped, "", name)
		}
	}
	plain, err := editorMCPRoundTrip(schemas)
	if err != nil {
		return err
	}
	return body(plain.(map[string]any), connected)
}

// Closes the held editor connections of sessions whose last viewer left, leaving one an execution still borrows
func releaseEditorMCP(ctx context.Context, a *SessionAuthority, sessionIDs []string) {
	for _, sid := range sessionIDs {
		scope := sessionScope(a, sid)
		a.editorMCP.mu.Lock()
		held := a.editorMCP.held[scope]
		a.editorMCP.mu.Unlock()
		if held == nil {
			continue
		}
		held.mu.Lock()
		if held.users == 0 && held.fingerprint != nil && !viewedEditorScope(a, scope) {
			disconnectEditorMCP(ctx, held, scope)
		}
		held.mu.Unlock()
	}
}

// Manifest JSON of a policy as a mapping, nil when the policy has none
func policyEditorManifest(policy *LocalSessionPolicy) (map[string]any, error) {
	if policy.EditorMCPJSON == nil || *policy.EditorMCPJSON == "" {
		return nil, nil
	}
	loaded, err := pyjson.Loads(*policy.EditorMCPJSON)
	if err != nil {
		return nil, err
	}
	data, _ := pyjson.Plain(loaded).(map[string]any)
	return data, nil
}

// Policy with its editor selection freshly discovered from the servers under the session's own scope and whether the manifest changed
func rebindEditorMCP(ctx context.Context, a *SessionAuthority, sid string, policy *LocalSessionPolicy, servers any) (*LocalSessionPolicy, bool, error) {
	old, err := policyEditorManifest(policy)
	if err != nil {
		return nil, false, err
	}
	toolsets := map[string]bool{}
	for _, name := range policy.Toolsets {
		if !frozenEditorToolset(old, name) {
			toolsets[name] = true
		}
	}
	var data map[string]any
	if pyTruthy(servers) {
		specs := editorSpecs(servers)
		scope := sessionScope(a, sid)
		held := heldEditorMCP(a, scope)
		held.mu.Lock()
		if held.users > 0 {
			held.mu.Unlock()
			return nil, false, runtimeRefusal("session_busy")
		}
		disconnectEditorMCP(ctx, held, scope)
		held.mu.Unlock()
		names := editorServerNames(scope, specs)
		var schemas map[string]any
		var connected []int
		err := borrowEditorMCP(ctx, a, scope, specs, names, policy.Cwd, true, func(s map[string]any, c []int) error {
			schemas, connected = s, c
			missing := map[int]bool{}
			for i := range names {
				missing[i] = true
			}
			for _, i := range c {
				delete(missing, i)
			}
			var order []int
			for i := range missing {
				order = append(order, i)
			}
			sort.Ints(order)
			for _, i := range order {
				slog.Warn(fmt.Sprintf("Editor MCP server %s failed discovery; session %s continues without it", pyrepr.Repr(specs[i]["name"]), sid))
			}
			return nil
		})
		if err != nil {
			return nil, false, err
		}
		kept := map[string]any{}
		for _, i := range connected {
			kept[strconv.Itoa(i)] = names[i]
			toolsets["mcp-"+names[i]] = true
		}
		digest, err := editorMCPFingerprint(servers)
		if err != nil {
			return nil, false, err
		}
		data = map[string]any{"scope": scope, "fingerprint": digest, "schemas": schemas, "names": kept}
	}
	rebound := *policy
	rebound.EditorMCPJSON = nil
	if len(data) > 0 {
		text, err := editorMCPJSON(data)
		if err != nil {
			return nil, false, err
		}
		rebound.EditorMCPJSON = &text
	}
	rebound.Toolsets = make([]string, 0, len(toolsets))
	for name := range toolsets {
		rebound.Toolsets = append(rebound.Toolsets, name)
	}
	sort.Strings(rebound.Toolsets)
	var oldSchemas, newSchemas any = map[string]any{}, map[string]any{}
	if s, ok := old["schemas"]; ok {
		oldSchemas = s
	}
	if s, ok := data["schemas"]; ok {
		newSchemas = s
	}
	changed := !pyjson.Equal(pyjson.Plain(oldSchemas), pyjson.Plain(newSchemas)) || !slices.Equal(rebound.Toolsets, policy.Toolsets)
	return &rebound, changed, nil
}

// Lends the servers to the policy's editor scope once their fingerprint matches its frozen manifest
func attachEditorMCP(ctx context.Context, a *SessionAuthority, policy *LocalSessionPolicy, servers any) error {
	if err := validateServers(servers); err != nil {
		return err
	}
	data, err := policyEditorManifest(policy)
	if err != nil {
		return err
	}
	digest, err := editorMCPFingerprint(servers)
	if err != nil {
		return err
	}
	if len(data) == 0 || !hmac.Equal([]byte(pyrepr.Str(data["fingerprint"])), []byte(digest)) {
		return runtimeRefusal("acp_mcp_policy_conflict")
	}
	copied, err := editorMCPRoundTrip(servers)
	if err != nil {
		return err
	}
	setBorrowedEditorMCP(a, pyrepr.Str(data["scope"]), copied, false)
	return nil
}

// Lends a resuming editor's servers to the session, an empty or omitted list keeping the current ones
func resumeEditorMCP(ctx context.Context, a *SessionAuthority, ref SessionRef, editor any) error {
	if editor == nil {
		return nil
	}
	var servers any
	if fields, ok := pyjson.Plain(editor).(map[string]any); ok {
		servers = fields["mcp_servers"]
	}
	if list, ok := servers.([]any); ok && len(list) == 0 {
		return nil
	}
	live, ok := a.live(ref.SessionID)
	if !ok {
		return &pyKeyError{ref.SessionID}
	}
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil {
		return err
	}
	if policy == nil {
		return runtimeRefusal("acp_mcp_policy_conflict")
	}
	if err := validateEditor(policy.Source, editor); err != nil {
		return err
	}
	if pyTruthy(servers) {
		return attachEditorMCP(ctx, a, policy, servers)
	}
	return nil
}

// Closes a session's held editor connections at once after an empty selection committed
func retireEditorMCP(ctx context.Context, a *SessionAuthority, sid string) {
	scope := sessionScope(a, sid)
	held := heldEditorMCP(a, scope)
	held.mu.Lock()
	defer held.mu.Unlock()
	disconnectEditorMCP(ctx, held, scope)
}

// Makes a session's borrowed servers follow its committed policy after a change
func installEditorMCP(ctx context.Context, a *SessionAuthority, sid string, servers any) error {
	receipt, err := a.DB.LocalReceipt(ctx, sid)
	if err != nil {
		return err
	}
	raw, err := pyItem(receipt, "policy")
	if err != nil {
		return err
	}
	policy, err := restorePolicy(raw)
	if err != nil {
		return err
	}
	data, err := policyEditorManifest(policy)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		setBorrowedEditorMCP(a, sessionScope(a, sid), nil, true)
		return nil
	}
	digest, err := editorMCPFingerprint(servers)
	if err != nil {
		return err
	}
	if hmac.Equal([]byte(pyrepr.Str(data["fingerprint"])), []byte(digest)) {
		copied, err := editorMCPRoundTrip(servers)
		if err != nil {
			return err
		}
		setBorrowedEditorMCP(a, pyrepr.Str(data["scope"]), copied, false)
	}
	return nil
}

// Publishes one tools changed event with server names and generation when a change altered the manifest
func announceToolsChanged(live *LiveSession, sessionID string, result *pyjson.Object) {
	if live == nil || result == nil {
		return
	}
	changed, _ := result.Get("tools_changed")
	if !pyTruthy(changed) {
		return
	}
	servers, _ := result.Get("mcp_servers")
	generation, _ := result.Get("execution_generation")
	live.EventStream.Publish(sessionID, pyjson.ObjectOf("mcp_servers", servers, "execution_generation", generation), "session.tools_changed", nil)
}

// Discovers the new editor list and freezes it into the policy the next turn runs under
func prepareEditorMCP(ctx context.Context, a *SessionAuthority, live *LiveSession, payload *pyjson.Object, prepared *pyjson.Object, servers any) (*pyjson.Object, error) {
	old, err := preparedPolicy(prepared)
	if err != nil {
		return nil, err
	}
	if old.Source != "acp" {
		return nil, runtimeRefusal("invalid_params")
	}
	sid, err := preparedSessionID(prepared)
	if err != nil {
		return nil, err
	}
	policy, changed, err := rebindEditorMCP(ctx, a, sid, old, servers)
	if err != nil {
		return nil, err
	}
	data, err := policyEditorManifest(policy)
	if err != nil {
		return nil, err
	}
	names, _ := data["names"].(map[string]any)
	indexes := make([]int, 0, len(names))
	for key := range names {
		i, err := strconv.Atoi(key)
		if err != nil {
			return nil, &PyError{Kind: "ValueError", Msg: "invalid literal for int() with base 10: " + pyrepr.Repr(key)}
		}
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	specs := editorSpecs(servers)
	picked := make([]any, 0, len(indexes))
	for _, i := range indexes {
		picked = append(picked, specs[i]["name"])
	}
	return rebindPrepared(ctx, a, prepared, old, policy, "tools_changed", changed, "mcp_servers", picked)
}

// Runs body inside the policy's editor scope, refusing schemas that drifted from its frozen manifest
func editorMCPScope(ctx context.Context, a *SessionAuthority, policy *LocalSessionPolicy, body func() error) error {
	data, err := policyEditorManifest(policy)
	if err != nil {
		return err
	}
	if policy.EditorMCPJSON == nil || *policy.EditorMCPJSON == "" {
		return body()
	}
	scope := pyrepr.Str(data["scope"])
	servers, ok := borrowedEditorMCP(a, scope)
	if !ok {
		return runtimeRefusal("launch_credentials_unavailable")
	}
	specs := editorSpecs(servers)
	names, err := frozenEditorNames(data, specs)
	if err != nil {
		return err
	}
	return borrowEditorMCP(ctx, a, scope, specs, names, policy.Cwd, false, func(schemas map[string]any, _ []int) error {
		if !pyjson.Equal(schemas, pyjson.Plain(data["schemas"])) {
			return runtimeRefusal("acp_mcp_schema_changed")
		}
		return body()
	})
}

// Copy of the parent's frozen secrets referenced under the child's policy identity
func childSecretRef(ctx context.Context, a *SessionAuthority, child string, parent, policy *LocalSessionPolicy) (*LocalSessionPolicy, error) {
	if parent.ConfigSecretRef == nil || !strings.HasPrefix(*parent.ConfigSecretRef, configSecretPrefix) {
		return policy, nil
	}
	secrets, err := recoverConfigSecretsSkipping(ctx, a, parent, map[string]bool{})
	if err != nil {
		return nil, err
	}
	ref, err := configReference(ctx, a, child, policy, secrets)
	if err != nil {
		return nil, err
	}
	out := *policy
	out.ConfigSecretRef = &ref
	return &out, nil
}

// Prepared branch fields and servers of a fork of an editor session under the child's own scope, nil when none are involved
func prepareBranchEditor(ctx context.Context, a *SessionAuthority, ref SessionRef, servers any, gate func() (*pyjson.Object, error)) (*pyjson.Object, any, error) {
	saved, err := a.DB.LocalReceipt(ctx, ref.SessionID)
	if err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	raw, err := pyItem(saved, "policy")
	if err != nil {
		return nil, nil, err
	}
	parent, err := restorePolicy(raw)
	if err != nil {
		return nil, nil, err
	}
	if servers == nil {
		data, err := policyEditorManifest(parent)
		if err != nil {
			return nil, nil, err
		}
		if parent.EditorMCPJSON == nil || *parent.EditorMCPJSON == "" {
			return nil, nil, nil
		}
		held, ok := borrowedEditorMCP(a, pyrepr.Str(data["scope"]))
		if !ok {
			return nil, nil, runtimeRefusal("launch_credentials_unavailable")
		}
		if servers, err = editorMCPRoundTrip(held); err != nil {
			return nil, nil, err
		}
	} else if parent.Source != "acp" {
		return nil, nil, runtimeRefusal("invalid_params")
	}
	gated, err := gate()
	if err != nil {
		return nil, nil, err
	}
	if _, has := gated.Get("snapshot"); !has {
		return gated, servers, nil
	}
	requestID := "branch:" + strings.ReplaceAll(uuid.NewString(), "-", "")
	profileID, _ := saved.Get("profile_id")
	principalID, _ := saved.Get("principal_id")
	child, err := state.LocalIdentity(profileID, principalID, requestID)
	if err != nil {
		return nil, nil, err
	}
	policy, _, err := rebindEditorMCP(ctx, a, child, parent, servers)
	if err != nil {
		return nil, nil, err
	}
	asked := *policy
	asked.EditApprovalPolicy = "ask"
	if policy, err = childSecretRef(ctx, a, child, parent, &asked); err != nil {
		return nil, nil, err
	}
	toolsets := make([]any, len(policy.Toolsets))
	for i, name := range policy.Toolsets {
		toolsets[i] = name
	}
	return preparedWith(gated, "request_id", requestID, "child", child, "parent_editor_mcp_json", optValue(parent.EditorMCPJSON),
		"editor_mcp_json", optValue(policy.EditorMCPJSON), "toolsets", toolsets, "config_secret_ref", optValue(policy.ConfigSecretRef)), servers, nil
}
