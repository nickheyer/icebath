package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Fields of a subagent record the subagent list projects
var ancillarySubagentFields = []string{"subagent_id", "parent_id", "depth", "goal", "delegation_id", "model",
	"started_at", "status", "tool_count", "last_tool", "accepting_steer"}

const (
	// Bytes of a child's live transcript the tail read returns
	ancillaryTailBytes = 16384
	// Characters of a process's output the dock tail view shows
	ancillaryProcessTailChars = 32768
)

// Agent side reads the ancillary projections need, assigned from pkg/agent in cmd/icebath/wiring.go
var AncillaryAgent struct {
	SubagentRecords func(ctx context.Context, parent any, sessionID string, sessionDB any, route any, includeFallbackEvicted bool) ([]map[string]any, error)
	SteerSubagent   func(subagentID string, child any, text string) bool
	TranscriptPath  func(agent any) string
	ControlSnapshot func(ctx context.Context, getMeta func(ctx context.Context, key string) (string, bool, error), target string) (*pyjson.Object, error)
	SetSpawnPaused  func(paused bool) bool
}

// Agent carrying the toolsets the session's runtime selected
type toolsetCarrier interface {
	EnabledToolsets() []string
	DisabledToolsets() []string
}

// Agent exposing its session id
type agentSessionIdentified interface{ SessionID() string }

// Desktop per child steer or stop, only for a child of this session's live agent
func subagentControl(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object, steer bool) (any, error) {
	fields := []string{"session_id", "profile", "subagent_id"}
	required := []string{"session_id", "subagent_id"}
	if steer {
		fields = append(fields, "text")
		required = append(required, "text")
	}
	invalid := paramsOutside(params, fields...) || ref.SessionID == ""
	for _, key := range required {
		if text, isStr := params.GetOr(key, nil).(string); !isStr || text == "" {
			invalid = true
		}
	}
	if invalid {
		return nil, runtimeRefusal("invalid_params")
	}
	a := c.authority
	live, ok := a.live(ref.SessionID)
	if !ok {
		return nil, runtimeRefusal("not_found")
	}
	if err := authorizeBusy(ctx, a, c.actor, ref, params, "session:control"); err != nil {
		return nil, err
	}
	sub := params.GetOr("subagent_id", "").(string)
	var child any
	err := func() error {
		live.EventStream.Lock.Lock()
		defer live.EventStream.Lock.Unlock()
		if err := requireCurrentRoute(ctx, a, ref); err != nil {
			return err
		}
		_, records, err := ownerObjects(ctx, a, ref, false)
		if err != nil {
			return err
		}
		for _, record := range records {
			if pyjson.Equal(record["subagent_id"], sub) {
				held, present := record["agent"]
				if !present {
					return &pyKeyError{"agent"}
				}
				child = held
				return nil
			}
		}
		return nil
	}()
	if err != nil {
		return nil, err
	}
	if !steer {
		found := !nilAgent(child) && hardInterruptWith(child, "Interrupted via Desktop ("+sub+")")
		return pyjson.ObjectOf("found", found, "subagent_id", sub), nil
	}
	text := params.GetOr("text", "").(string)
	status := "rejected"
	if AncillaryAgent.SteerSubagent(sub, child, text) {
		status = "queued"
	}
	return pyjson.ObjectOf("status", status, "subagent_id", sub, "text", text), nil
}

// Nil interface or nil pointer standing for a Python None agent
func nilAgent(agent any) bool {
	if agent == nil {
		return true
	}
	value := reflect.ValueOf(agent)
	return value.Kind() == reflect.Pointer && value.IsNil()
}

// Explicit stop of an agent with a message, the legacy interrupt as fallback and false when neither exists
func hardInterruptWith(agent any, message string) bool {
	if nilAgent(agent) {
		return false
	}
	switch target := agent.(type) {
	case hardInterruptWithReason:
		target.HardInterrupt(message, "")
	case hardInterruptMessageOnly:
		target.HardInterrupt(message)
	case interruptWithReason:
		target.Interrupt(message, "")
	case interruptMessageOnly:
		target.Interrupt(message)
	default:
		return false
	}
	return true
}

// Params outside the session read pair or a missing session id refuse the call
func sessionOnlyParams(params *pyjson.Object, ref SessionRef) error {
	if paramsOutside(params, "session_id", "profile") || ref.SessionID == "" {
		return runtimeRefusal("invalid_params")
	}
	return nil
}

// Tool catalog of the session as the classic CLI listed it, grouped by toolset with each first sentence
func showTools(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	if err := sessionOnlyParams(params, ref); err != nil {
		return nil, err
	}
	a := c.authority
	live, ok := a.live(ref.SessionID)
	if !ok {
		return nil, runtimeRefusal("not_found")
	}
	if err := authorizeBusy(ctx, a, c.actor, ref, params, "session:read"); err != nil {
		return nil, err
	}
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil {
		return nil, err
	}
	agent, err := a.agent(ref)
	if err != nil {
		return nil, err
	}
	return forwardOffThread(ctx, func() (*pyjson.Object, error) {
		scoped := ownerScope(ctx, a, false)
		var enabled, disabled []string
		if !nilAgent(agent) {
			if carrier, ok := agent.(toolsetCarrier); ok {
				enabled, disabled = carrier.EnabledToolsets(), carrier.DisabledToolsets()
			}
		} else {
			cfg := map[string]any{}
			if policy != nil {
				if cfg, err = policy.config(scoped, a, false); err != nil {
					return nil, err
				}
				if enabled, err = policy.currentToolsets(scoped, cfg); err != nil {
					return nil, err
				}
			}
			agentSection := cfg["agent"]
			if !pyrepr.Truthy(agentSection) {
				agentSection = map[string]any{}
			}
			raw, err := pyGet(agentSection, "disabled_toolsets", nil)
			if err != nil {
				return nil, err
			}
			platform := "cli"
			if policy != nil {
				platform = policy.Platform
			}
			disabled = tools.SessionDisabledToolsets(tools.ParseConfigStringList(raw), platform, policy != nil && policy.Source == "oneshot")
		}
		defs := tools.Default.ToolDefinitions(scoped, nil, tools.DefinitionOptions{Enabled: enabled, Disabled: disabled,
			Quiet: true, SkipToolSearchAssembly: true})
		names := make([]string, 0, len(defs))
		byName := map[string]map[string]any{}
		for _, def := range defs {
			function, _ := def["function"].(map[string]any)
			name, _ := function["name"].(string)
			names = append(names, name)
			byName[name] = function
		}
		sort.Strings(names)
		sections := map[string][]any{}
		for _, name := range names {
			description := byName[name]["description"]
			desc := ""
			if pyrepr.Truthy(description) {
				desc = pyrepr.Str(description)
			}
			desc = strings.SplitN(desc, "\n", 2)[0]
			if i := strings.Index(desc, ". "); i >= 0 {
				desc = desc[:i+1]
			}
			toolset := tools.Default.ToolsetForTool(scoped, "", name)
			if toolset == "" {
				toolset = "unknown"
			}
			sections[toolset] = append(sections[toolset], pyjson.ObjectOf("name", name, "description", desc))
		}
		order := make([]string, 0, len(sections))
		for name := range sections {
			order = append(order, name)
		}
		sort.Strings(order)
		rows := make([]any, 0, len(order))
		for _, name := range order {
			rows = append(rows, pyjson.ObjectOf("name", name, "tools", sections[name]))
		}
		return pyjson.ObjectOf("sections", rows, "total", len(defs)), nil
	})
}

// Rescans the skill dirs in the owner and renders the classic reload lines
func reloadSkills(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	if err := sessionOnlyParams(params, ref); err != nil {
		return nil, err
	}
	a := c.authority
	live, ok := a.live(ref.SessionID)
	if !ok {
		return nil, runtimeRefusal("not_found")
	}
	if err := authorizeBusy(ctx, a, c.actor, ref, params, "session:control"); err != nil {
		return nil, err
	}
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil {
		return nil, err
	}
	result, err := forwardOffThread(ctx, func() (map[string]any, error) {
		var rescanned map[string]any
		err := policyScope(ownerScope(ctx, a, false), policy, a, func(scoped context.Context) error {
			var err error
			rescanned, err = skills.NewScope(home.Dir(scoped), nil).ReloadSkills(scoped)
			return err
		})
		return rescanned, err
	})
	if err != nil {
		return nil, err
	}
	added, removed := skillRows(result["added"]), skillRows(result["removed"])
	lines := []string{"Reloading skills..."}
	if len(added) == 0 && len(removed) == 0 {
		lines = append(lines, "No new skills detected.")
	}
	for _, group := range []struct {
		label string
		items []map[string]any
	}{{"Added skills:", added}, {"Removed skills:", removed}} {
		if len(group.items) == 0 {
			continue
		}
		lines = append(lines, group.label)
		for _, item := range group.items {
			name, present := item["name"]
			if !present {
				name = ""
			}
			lines = append(lines, "  - "+pyrepr.Str(name))
		}
	}
	total := 0
	if pyrepr.Truthy(result["total"]) {
		total = int(anyInt64(result["total"]))
	}
	lines = append(lines, fmt.Sprintf("%d skill(s) available", total))
	return pyjson.ObjectOf("output", strings.Join(lines, "\n"), "result", result), nil
}

// Rows of a reload diff list, empty for a missing one
func skillRows(value any) []map[string]any {
	switch rows := value.(type) {
	case []map[string]any:
		return rows
	case []any:
		out := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			if item, ok := row.(map[string]any); ok {
				out = append(out, item)
			}
		}
		return out
	}
	return nil
}

// Desktop approval replay and card ack over the owner's generation bound prompt projection
func approvals(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object, ack bool) (any, error) {
	fields := []string{"session_id", "profile"}
	if ack {
		fields = append(fields, "request_id")
	}
	requestID, isStr := params.GetOr("request_id", nil).(string)
	if paramsOutside(params, fields...) || ref.SessionID == "" || (ack && (!isStr || requestID == "")) {
		return nil, runtimeRefusal("invalid_params")
	}
	a := c.authority
	live, ok := a.live(ref.SessionID)
	if !ok {
		return nil, runtimeRefusal("not_found")
	}
	if err := authorizeBusy(ctx, a, c.actor, ref, params, "session:read"); err != nil {
		return nil, err
	}
	pending := []pendingPrompt{}
	live.EventStream.Lock.Lock()
	controls := live.pendingControls()
	for _, promptID := range controls.order {
		held := controls.pending[promptID]
		if pyjson.Equal(held.prompt.GetOr("kind", nil), "approval") {
			pending = append(pending, held)
		}
	}
	live.EventStream.Lock.Unlock()
	if ack {
		for _, held := range pending {
			if pyjson.Equal(held.prompt.GetOr("prompt_id", nil), requestID) {
				return pyjson.ObjectOf("acknowledged", held.route != nil && tools.AckGatewayApproval(pyrepr.Str(held.route), requestID)), nil
			}
		}
		return pyjson.ObjectOf("acknowledged", false), nil
	}
	rows := make([]any, 0, len(pending))
	for _, held := range pending {
		choices, err := pyIterable(held.prompt.GetOr("choices", nil))
		if err != nil {
			return nil, err
		}
		permanent := false
		for _, choice := range choices {
			if pyjson.Equal(choice, "always") {
				permanent = true
			}
		}
		rows = append(rows, pyjson.ObjectOf("request_id", held.prompt.GetOr("prompt_id", nil), "command", held.prompt.GetOr("command", nil),
			"description", held.prompt.GetOr("description", nil), "choices", choices, "allow_permanent", permanent))
	}
	return pyjson.ObjectOf("approvals", rows), nil
}

// One of the session control, process, subagent and tail projections, read off the owner loop
func ancillaryRead(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object, kind string) (any, error) {
	fields := []string{"session_id", "profile"}
	if kind == "tail" {
		fields = append(fields, "subagent_id")
	}
	subagentID, isStr := params.GetOr("subagent_id", nil).(string)
	if paramsOutside(params, fields...) || ref.SessionID == "" || (kind == "tail" && (!isStr || subagentID == "")) {
		return nil, runtimeRefusal("invalid_params")
	}
	a := c.authority
	live, ok := a.live(ref.SessionID)
	if !ok {
		return nil, runtimeRefusal("not_found")
	}
	if err := authorizeBusy(ctx, a, c.actor, ref, params, "session:read"); err != nil {
		return nil, err
	}
	return forwardOffThread(ctx, func() (*pyjson.Object, error) { return readProjection(ctx, a, live, ref, params, kind) })
}

// Projection under the session's event lock, a session popped meanwhile reading as not found
func readProjection(ctx context.Context, a *SessionAuthority, live *LiveSession, ref SessionRef, params *pyjson.Object, kind string) (*pyjson.Object, error) {
	live.EventStream.Lock.Lock()
	defer live.EventStream.Lock.Unlock()
	projected, err := project(ctx, a, ref, params, kind)
	var missing *pyKeyError
	if err != nil && errors.As(err, &missing) {
		if current, ok := a.live(ref.SessionID); !ok || current != live {
			return nil, runtimeRefusal("not_found")
		}
	}
	return projected, err
}

// Control, subagent, tail or process projection of a session
func project(ctx context.Context, a *SessionAuthority, ref SessionRef, params *pyjson.Object, kind string) (*pyjson.Object, error) {
	if kind == "control" {
		snapshot, err := controlSnapshot(ctx, a, ref)
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("control", snapshot), nil
	}
	if err := requireCurrentRoute(ctx, a, ref); err != nil {
		return nil, err
	}
	agent, records, err := ownerObjects(ctx, a, ref, kind == "subagents")
	if err != nil {
		return nil, err
	}
	switch kind {
	case "subagents":
		rows := make([]any, 0, len(records))
		for _, record := range records {
			row := pyjson.NewObject()
			for _, key := range ancillarySubagentFields {
				row.Set(key, record[key])
			}
			rows = append(rows, row)
		}
		return pyjson.ObjectOf("subagents", rows, "delegations", []any{}), nil
	case "tail":
		return subagentTail(records, params.GetOr("subagent_id", "").(string)), nil
	}
	snapshot, err := processSnapshot(ctx, a, ref, agent, records)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("processes", snapshot), nil
}

// Session's own processes a selector keeps, read under the event lock against the current route
func ownedTargets(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object, keep func(view tools.ProcessView) bool) ([]string, error) {
	a := c.authority
	live, ok := a.live(ref.SessionID)
	if !ok {
		return nil, runtimeRefusal("not_found")
	}
	if err := authorizeBusy(ctx, a, c.actor, ref, params, "session:control"); err != nil {
		return nil, err
	}
	live.EventStream.Lock.Lock()
	defer live.EventStream.Lock.Unlock()
	if err := requireCurrentRoute(ctx, a, ref); err != nil {
		return nil, err
	}
	agent, records, err := ownerObjects(ctx, a, ref, false)
	if err != nil {
		return nil, err
	}
	keys, owners, err := processOwnership(ctx, a, ref, agent, records)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, view := range ownedViews(tools.DefaultProcessRegistry().ProcessViews(0), keys, owners) {
		if keep(view) {
			ids = append(ids, view.ID)
		}
	}
	return ids, nil
}

// Tool environment of the owner scope a registry read runs in
func scopedToolEnv(ctx context.Context) *tools.Env {
	return &tools.Env{Home: home.Dir(ctx)}
}

// Desktop per process stop of one process this session owns, any other id refused as process_not_found
func killProcess(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	processID, isStr := params.GetOr("process_id", nil).(string)
	if paramsOutside(params, "session_id", "process_id", "profile") || ref.SessionID == "" || !isStr || processID == "" {
		return nil, runtimeRefusal("invalid_params")
	}
	ids, err := ownedTargets(ctx, c, ref, params, func(view tools.ProcessView) bool { return view.ID == processID })
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, runtimeRefusal("process_not_found")
	}
	return forwardOffThread(ctx, func() (map[string]any, error) {
		return tools.DefaultProcessRegistry().KillProcess(ctx, scopedToolEnv(ctx), ids[0], "process.kill", true), nil
	})
}

// Runner that refreshes the cached agents of a profile after an MCP reload
type mcpReloadRefresher interface {
	MCPReloadRefreshCachedAgents(multiplex bool, profile string)
}

// Brings this profile's live MCP servers in step with its config and commits the calling session's selection
func reloadMCP(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	if paramsOutside(params, "session_id", "confirm", "rev", "profile") {
		return nil, runtimeRefusal("invalid_params")
	}
	if !c.actor.Capabilities["session:control"] {
		return nil, runtimeRefusal("permission_denied")
	}
	if params.GetOr("confirm", nil) != true {
		return pyjson.ObjectOf("status", "confirm_required", "message", "Reloading MCP refreshes every chat of this profile."), nil
	}
	type reconciled struct {
		result mcp.ReconcileResult
		failed []string
		ready  []string
	}
	outcome, err := forwardOffThread(ctx, func() (reconciled, error) {
		scoped := mcp.SuppressInteractiveOAuth(ctx)
		result, err := mcp.Reconcile(scoped, nil)
		if err != nil {
			return reconciled{}, err
		}
		return reconciled{result: result, failed: mcp.ServersNotConnected(scoped, nil), ready: mcp.ServersReady(scoped, nil)}, nil
	})
	if err != nil {
		return nil, err
	}
	failed := map[string]bool{}
	for _, name := range outcome.failed {
		failed[name] = true
	}
	added := []string{}
	for _, name := range outcome.result.Added {
		if !failed[name] {
			added = append(added, name)
		}
	}
	a := c.authority
	refresher, ok := a.Runner.(mcpReloadRefresher)
	if !ok {
		return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("'%T' object has no attribute '_mcp_reload_refresh_cached_agents'", a.Runner)}
	}
	refresher.MCPReloadRefreshCachedAgents(a.Runner.MultiplexProfiles(), servedProfileName(a.ProfileID))
	session, err := commitSessionMCP(ctx, c, ref, params)
	if err != nil {
		return nil, err
	}
	heldRev := ""
	if pyrepr.Truthy(params.GetOr("rev", nil)) {
		if heldRev, err = heldMCPRev(ctx, c, ref); err != nil {
			return nil, err
		}
	}
	if len(outcome.failed) > 0 && session != "applied" && len(outcome.result.Removed) == 0 && len(added) == 0 &&
		!pyjson.Equal(params.GetOr("rev", nil), heldRev) {
		field, detail := "mcp_servers", "did not connect: "+strings.Join(outcome.failed, ", ")
		return nil, state.NewRuntimeStoreError("mcp_servers_unavailable", &field, &detail)
	}
	changes := []string{}
	for _, group := range []struct {
		key   string
		names []string
	}{{"removed", outcome.result.Removed}, {"added", added}, {"pending", outcome.result.Pending}} {
		if len(group.names) > 0 {
			changes = append(changes, group.key+": "+strings.Join(group.names, ", "))
		}
	}
	changed := strings.Join(changes, ", ")
	if changed == "" {
		changed = "no server changes"
	}
	note := map[string]string{"applied": "; this chat's tools changed for its next turn",
		"busy": "; this chat is busy: its new tools apply once the turn finishes and reload runs again or the chat is reopened"}[session]
	unavailable := ""
	if len(outcome.failed) > 0 {
		unavailable = "; did not connect: " + strings.Join(outcome.failed, ", ")
	}
	reply := pyjson.ObjectOf("status", "reloaded", "message", "MCP servers reconciled ("+changed+")"+note+unavailable)
	if heldRev != "" {
		reply.Set("loaded_rev", heldRev)
	}
	if session != "" {
		reply.Set("session", session)
	}
	if len(outcome.failed) > 0 {
		reply.Set("unavailable", outcome.failed)
	}
	if session != "" {
		servers, err := forwardOffThread(ctx, func() ([]string, error) { return sessionServers(ctx, a, ref.SessionID, outcome.ready) })
		if err != nil {
			return nil, err
		}
		reply.Set("session_servers", servers)
	}
	return reply, nil
}

// MCP revision the session's committed policy already holds, empty when the call names no live local session
func heldMCPRev(ctx context.Context, c *AuthorityConnection, ref SessionRef) (string, error) {
	if ref.SessionID == "" {
		return "", nil
	}
	live, ok := c.authority.live(ref.SessionID)
	if !ok || live.Source == nil {
		return "", nil
	}
	policy, err := policyForSource(ctx, c.authority.Runner, live.Source)
	if err != nil {
		return "", err
	}
	return forwardOffThread(ctx, func() (string, error) { return mcpRevisionInScope(ctx, c.authority.ProfileID, policy) })
}

// MCP revision of a policy against the profile config read inside its runtime scope
func mcpRevisionInScope(ctx context.Context, profileHome string, policy *LocalSessionPolicy) (string, error) {
	scoped := profileRuntimeScope(ctx, profileHome, true)
	cfg, err := config.LoadContext(scoped, home.Dir(scoped), nil)
	if err != nil {
		return "", err
	}
	return mcpRevision(scoped, policy, cfg)
}

// Ready profile MCP servers the session's committed policy runs
func sessionServers(ctx context.Context, a *SessionAuthority, sessionID string, ready []string) ([]string, error) {
	receipt, err := a.DB.LocalReceipt(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	stored, ok := receipt.Get("policy")
	if !ok {
		return nil, &pyKeyError{"policy"}
	}
	policy, err := restorePolicy(stored)
	if err != nil {
		return nil, err
	}
	cfg, err := loadsPolicyMap(policy.ConfigJSON)
	if err != nil {
		return nil, err
	}
	enabled := tools.EnabledMCPServerNames(config.Tree(cfg), nil)
	readySet := map[string]bool{}
	for _, name := range ready {
		readySet[name] = true
	}
	names := map[string]bool{}
	for _, name := range policy.Toolsets {
		if enabled[name] && readySet[name] {
			names[name] = true
		}
	}
	return sortedNames(names), nil
}

// The calling local session's MCP servers committed as a new policy generation, applied unchanged or busy
func commitSessionMCP(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (string, error) {
	if ref.SessionID == "" {
		return "", nil
	}
	a := c.authority
	live, ok := a.live(ref.SessionID)
	if !ok || live.Source == nil || live.Source.Platform != PlatformLocal {
		return "", nil
	}
	if err := authorizeBusy(ctx, a, c.actor, ref, params, "session:control"); err != nil {
		return "", err
	}
	committed, err := commitRefresh(ctx, a, ref, true, false)
	if err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) && (refusal.Reason == "session_busy" || refusal.Reason == "unknown_execution" || refusal.Reason == "revision_conflict") {
			return "busy", nil
		}
		return "", err
	}
	if committed {
		return "applied", nil
	}
	return "unchanged", nil
}

// Kills the background processes this session owns and interrupts the delegations it dispatched
func stopProcesses(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	if err := sessionOnlyParams(params, ref); err != nil {
		return nil, err
	}
	targets, err := ownedTargets(ctx, c, ref, params, func(view tools.ProcessView) bool { return !view.Exited || view.ScopeStopPending })
	if err != nil {
		return nil, err
	}
	killed := 0
	if len(targets) > 0 {
		if killed, err = forwardOffThread(ctx, func() (int, error) {
			count := 0
			for _, id := range targets {
				result := tools.DefaultProcessRegistry().KillProcess(ctx, scopedToolEnv(ctx), id, "process.stop", false)
				_, scopeFailed := result["scope_stop_failed"]
				if status := result["status"]; (status == "killed" || status == "already_exited") && !scopeFailed {
					count++
				}
			}
			return count, nil
		}); err != nil {
			return nil, err
		}
	}
	delegations, err := forwardOffThread(ctx, func() (int, error) { return interruptDelegations(ctx, c.authority, ref) })
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("killed", killed, "delegations", delegations), nil
}

// Interrupts the background delegations this session dispatched by its route or durable id
func interruptDelegations(ctx context.Context, a *SessionAuthority, ref SessionRef) (int, error) {
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return 0, err
	}
	target, err := physicalTarget(ctx, a, ref)
	if err != nil {
		return 0, err
	}
	return tools.InterruptAsyncDelegationsForSession(live.Route, "", target, "/stop"), nil
}

// Refuses a route that no longer serves this conversation
func requireCurrentRoute(ctx context.Context, a *SessionAuthority, ref SessionRef) error {
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return err
	}
	target, err := physicalTarget(ctx, a, ref)
	if err != nil {
		return err
	}
	store := a.Runner.SessionStore()
	store.mu.Lock()
	entry := store.entries.get(live.Route)
	store.mu.Unlock()
	if entry == nil || entry.SessionID != target {
		return runtimeRefusal("stale_generation")
	}
	return nil
}

// Agent and subagent records of this in process session, a managed worker refused as unsupported
func ownerObjects(ctx context.Context, a *SessionAuthority, ref SessionRef, includeFallbackEvicted bool) (any, []map[string]any, error) {
	managed, err := managedPolicy(ctx, a, ref)
	if err != nil {
		return nil, nil, err
	}
	if managed != nil {
		return nil, nil, runtimeRefusal("unsupported_projection")
	}
	agent, err := a.agent(ref)
	if err != nil {
		return nil, nil, err
	}
	sessionID := ""
	var route, sessionDB any
	if includeFallbackEvicted {
		if sessionID, err = physicalTarget(ctx, a, ref); err != nil {
			return nil, nil, err
		}
		live, err := liveSession(a, ref.SessionID)
		if err != nil {
			return nil, nil, err
		}
		route = live.Route
		if a.DB != nil {
			sessionDB = a.DB
		}
	}
	records, err := AncillaryAgent.SubagentRecords(ctx, agent, sessionID, sessionDB, route, includeFallbackEvicted)
	return agent, records, err
}

// Goal, loop and heartbeat cards of the conversation's current compression segment
func controlSnapshot(ctx context.Context, a *SessionAuthority, ref SessionRef) (*pyjson.Object, error) {
	target, err := physicalTarget(ctx, a, ref)
	if err != nil {
		return nil, err
	}
	tip, err := a.DB.GetCompressionTip(ctx, target)
	if err != nil {
		return nil, err
	}
	if tip != "" {
		target = tip
	}
	return AncillaryAgent.ControlSnapshot(ctx, a.DB.GetMeta, target)
}

// Last bytes of a child's live transcript, unavailable when it has none or it cannot be read
func subagentTail(records []map[string]any, subagentID string) *pyjson.Object {
	result := func() *pyjson.Object {
		return pyjson.ObjectOf("subagent_id", subagentID, "available", false, "text", "", "truncated", false)
	}
	var path string
	for _, record := range records {
		if pyjson.Equal(record["subagent_id"], subagentID) {
			path = AncillaryAgent.TranscriptPath(record["agent"])
			break
		}
	}
	if path == "" {
		return result()
	}
	stream, err := os.Open(path)
	if err != nil {
		return result()
	}
	defer stream.Close()
	size, err := stream.Seek(0, io.SeekEnd)
	if err != nil {
		return result()
	}
	if _, err := stream.Seek(max(0, size-ancillaryTailBytes), io.SeekStart); err != nil {
		return result()
	}
	data := make([]byte, ancillaryTailBytes)
	n, err := io.ReadFull(stream, data)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return result()
	}
	out := result()
	out.Set("available", true)
	out.Set("text", strings.ToValidUTF8(string(data[:n]), ""))
	out.Set("truncated", size > ancillaryTailBytes)
	return out
}

// Routing keys and owners a registry process must match to belong to this session
func processOwnership(ctx context.Context, a *SessionAuthority, ref SessionRef, agent any, records []map[string]any) (map[string]bool, map[string]bool, error) {
	target, err := physicalTarget(ctx, a, ref)
	if err != nil {
		return nil, nil, err
	}
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return nil, nil, err
	}
	keys, owners := map[string]bool{live.Route: true, target: true}, map[string]bool{target: true}
	if held, ok := agent.(agentSessionIdentified); ok && !nilAgent(agent) {
		owners[held.SessionID()] = true
	}
	for _, record := range records {
		if id, isStr := record["subagent_id"].(string); isStr {
			owners[id] = true
		}
	}
	delete(owners, "")
	return keys, owners, nil
}

// Processes whose routing key and owner task match this session
func ownedViews(views []tools.ProcessView, keys, owners map[string]bool) []tools.ProcessView {
	out := []tools.ProcessView{}
	for _, view := range views {
		owner := view.OwnerTaskID
		if owner == "" {
			owner = view.TaskID
		}
		if keys[view.SessionKey] && owners[owner] {
			out = append(out, view)
		}
	}
	return out
}

// Dock rows of the processes this session owns with their elapsed, exit and tail fields
func processSnapshot(ctx context.Context, a *SessionAuthority, ref SessionRef, agent any, records []map[string]any) ([]any, error) {
	keys, owners, err := processOwnership(ctx, a, ref, agent, records)
	if err != nil {
		return nil, err
	}
	rows := []any{}
	for _, view := range ownedViews(tools.DefaultProcessRegistry().ProcessViews(ancillaryProcessTailChars), keys, owners) {
		command := []rune(view.Command)
		if len(command) > 200 {
			command = command[:200]
		}
		status := "running"
		var completion any
		if view.Exited {
			status, completion = "exited", view.CompletionReason
		}
		var exitCode, exitedAt any
		if view.ExitCode != nil {
			exitCode = *view.ExitCode
		}
		if view.ExitedAt != 0 {
			exitedAt = view.ExitedAt
		}
		rows = append(rows, pyjson.ObjectOf("session_id", view.ID, "command", string(command), "status", status,
			"exit_code", exitCode, "started_epoch", view.StartedAt, "exited_at", exitedAt,
			"completion_reason", completion, "output_tail", view.OutputTail))
	}
	return rows, nil
}
