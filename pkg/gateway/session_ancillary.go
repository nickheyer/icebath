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
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

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
	ControlSnapshot func(ctx context.Context, getMeta func(ctx context.Context, key string) (string, bool, error), target string) (*jsonutil.Object, error)
	SetSpawnPaused  func(paused bool) bool
}

// Agent carrying the toolsets the session's runtime selected
type toolsetCarrier interface {
	EnabledToolsets() []string
	DisabledToolsets() []string
}

// Agent exposing its session id
type agentSessionIdentified interface{ SessionID() string }

// Per child steer or stop, only for a child of this session's live agent
func subagentControl(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object, steer bool) (any, error) {
	fields := []string{"session_id", "profile", "subagent_id"}
	required := []string{"session_id", "subagent_id"}
	if steer {
		fields = append(fields, "text")
		required = append(required, "text")
	}
	invalid := paramsOutside(params, fields...) || ref.SessionID == ""
	for _, key := range required {
		if text, isStr := params.ValueOr(key, nil).(string); !isStr || text == "" {
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
	sub := params.ValueOr("subagent_id", "").(string)
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
			if anyutil.Equal(record["subagent_id"], sub) {
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
		found := !nilAgent(child) && hardInterruptWith(child, "Interrupted via TUI ("+sub+")")
		return tuigateway.Plain(tuigateway.SubagentInterruptResult{Found: found, SubagentID: sub}), nil
	}
	text := params.ValueOr("text", "").(string)
	status := tuigateway.SteerStatus("rejected")
	if AncillaryAgent.SteerSubagent(sub, child, text) {
		status = "queued"
	}
	return tuigateway.Plain(tuigateway.SubagentSteerResult{Status: status, SubagentID: sub, Text: text}), nil
}

// Typed value of a generic row a verb projects, a misfit reported against the method
func canonicalRow[T any](method string, raw any) T {
	var row T
	if err := tuigateway.Strict(raw, &row); err != nil {
		tuigateway.ReportMisfit("result of", method, err)
	}
	return row
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
func sessionOnlyParams(params *jsonutil.Object, ref SessionRef) error {
	if paramsOutside(params, "session_id", "profile") || ref.SessionID == "" {
		return runtimeRefusal("invalid_params")
	}
	return nil
}

// Tool catalog of the session as the reference CLI listed it, grouped by toolset with each first sentence
func showTools(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object) (any, error) {
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
	return forwardOffThread(ctx, func() (*jsonutil.Object, error) {
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
			if !anyutil.IsTrue(agentSection) {
				agentSection = map[string]any{}
			}
			raw, err := pyGet(agentSection, "disabled_toolsets", nil)
			if err != nil {
				return nil, err
			}
			disabled = tools.SessionDisabledToolsets(tools.ParseConfigStringList(raw), policy != nil && policy.Source == "oneshot")
		}
		defs := tools.Default.ToolDefinitions(scoped, nil, tools.DefinitionOptions{Enabled: enabled, Disabled: disabled,
			Quiet: true, SkipToolSearchAssembly: true})
		names := make([]string, 0, len(defs))
		byName := map[string]map[string]any{}
		for _, def := range defs {
			function, _ := jsonutil.AsMap(def["function"])
			name, _ := function["name"].(string)
			names = append(names, name)
			byName[name] = function
		}
		sort.Strings(names)
		sections := map[string][]any{}
		for _, name := range names {
			description := byName[name]["description"]
			desc := ""
			if anyutil.IsTrue(description) {
				desc = anyutil.String(description)
			}
			desc = strings.SplitN(desc, "\n", 2)[0]
			if i := strings.Index(desc, ". "); i >= 0 {
				desc = desc[:i+1]
			}
			toolset := tools.Default.ToolsetForTool(scoped, "", name)
			if toolset == "" {
				toolset = "unknown"
			}
			sections[toolset] = append(sections[toolset], jsonutil.ObjectOf("name", name, "description", desc))
		}
		order := make([]string, 0, len(sections))
		for name := range sections {
			order = append(order, name)
		}
		sort.Strings(order)
		rows := make([]any, 0, len(order))
		for _, name := range order {
			rows = append(rows, jsonutil.ObjectOf("name", name, "tools", sections[name]))
		}
		return jsonutil.ObjectOf("sections", rows, "total", len(defs)), nil
	})
}

// Rescans the skill dirs in the owner and renders the classic reload lines
func reloadSkills(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object) (any, error) {
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
			lines = append(lines, "  - "+anyutil.String(name))
		}
	}
	total := 0
	if anyutil.IsTrue(result["total"]) {
		total = int(anyInt64(result["total"]))
	}
	lines = append(lines, fmt.Sprintf("%d skill(s) available", total))
	return jsonutil.ObjectOf("output", strings.Join(lines, "\n"), "result", result), nil
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

// Control, process, subagent or tail projection read off the owner loop, a tail restoring a stored session
func ancillaryRead(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object, kind string) (any, error) {
	fields := []string{"session_id", "profile"}
	if kind == "tail" {
		fields = append(fields, "subagent_id")
	}
	subagentID, isStr := params.ValueOr("subagent_id", nil).(string)
	if paramsOutside(params, fields...) || ref.SessionID == "" || (kind == "tail" && (!isStr || subagentID == "")) {
		return nil, runtimeRefusal("invalid_params")
	}
	a := c.authority
	if kind == "tail" {
		if err := authorizeBusy(ctx, a, c.actor, ref, params, "session:read"); err != nil {
			return nil, err
		}
	}
	live, ok := a.live(ref.SessionID)
	if !ok {
		return nil, runtimeRefusal("not_found")
	}
	if kind != "tail" {
		if err := authorizeBusy(ctx, a, c.actor, ref, params, "session:read"); err != nil {
			return nil, err
		}
	}
	return forwardOffThread(ctx, func() (any, error) { return readProjection(ctx, a, live, ref, params, kind) })
}

// Projection under the session's event lock, a session popped meanwhile reading as not found
func readProjection(ctx context.Context, a *SessionAuthority, live *LiveSession, ref SessionRef, params *jsonutil.Object, kind string) (any, error) {
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
func project(ctx context.Context, a *SessionAuthority, ref SessionRef, params *jsonutil.Object, kind string) (any, error) {
	if kind == "control" {
		snapshot, err := controlSnapshot(ctx, a, ref)
		if err != nil {
			return nil, err
		}
		return jsonutil.ObjectOf("control", snapshot), nil
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
		failed, err := failedDelegations(ctx, a, ref)
		if err != nil {
			return nil, err
		}
		result := tuigateway.SubagentListResult{Subagents: []tuigateway.SubagentSnapshot{}, Delegations: failed}
		for _, record := range records {
			row := map[string]any{}
			for _, key := range tuigateway.WireNames[tuigateway.SubagentSnapshot]() {
				if value, present := record[key]; present {
					row[key] = value
				}
			}
			result.Subagents = append(result.Subagents, canonicalRow[tuigateway.SubagentSnapshot]("subagent.list", row))
		}
		return tuigateway.Plain(result), nil
	case "tail":
		subagentID := params.ValueOr("subagent_id", "").(string)
		path := ""
		for _, record := range records {
			if anyutil.Equal(record["subagent_id"], subagentID) {
				path = AncillaryAgent.TranscriptPath(record["agent"])
				break
			}
		}
		if path == "" {
			target, err := physicalTarget(ctx, a, ref)
			if err != nil {
				return nil, err
			}
			if path, err = tools.FinishedSubagentTranscript(ctx, a.DB, target, subagentID); err != nil {
				return nil, err
			}
		}
		return tuigateway.Plain(subagentTail(path, subagentID)), nil
	}
	snapshot, err := processSnapshot(ctx, a, ref, agent, records)
	if err != nil {
		return nil, err
	}
	return tuigateway.Plain(tuigateway.ProcessListResult{Processes: snapshot}), nil
}

// Session's own processes a selector keeps, read under the event lock against the current route
func ownedTargets(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object, keep func(view tools.ProcessView) bool) ([]string, error) {
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

// Per process stop of one process this session owns, any other id refused as process_not_found
func killProcess(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object) (any, error) {
	processID, isStr := params.ValueOr("process_id", nil).(string)
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
	return forwardOffThread(ctx, func() (any, error) {
		killed := tools.DefaultProcessRegistry().KillProcess(ctx, scopedToolEnv(ctx), ids[0], "process.kill", true)
		return tuigateway.Plain(canonicalRow[tuigateway.ProcessKillResult]("process.kill", killed)), nil
	})
}

// Runner that refreshes the cached agents of a profile after an MCP reload
type mcpReloadRefresher interface {
	MCPReloadRefreshCachedAgents(multiplex bool, profile string)
}

// Brings this profile's live MCP servers in step with its config and commits the calling session's selection
func reloadMCP(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object) (any, error) {
	if paramsOutside(params, "session_id", "confirm", "rev", "profile") {
		return nil, runtimeRefusal("invalid_params")
	}
	if !c.actor.Capabilities["session:control"] {
		return nil, runtimeRefusal("permission_denied")
	}
	if params.ValueOr("confirm", nil) != true {
		return jsonutil.ObjectOf("status", "confirm_required", "message", "Reloading MCP refreshes every chat of this profile."), nil
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
		return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("the gateway runner (%T) cannot refresh cached agents after an MCP reload", a.Runner)}
	}
	refresher.MCPReloadRefreshCachedAgents(a.Runner.MultiplexProfiles(), servedProfileName(a.ProfileID))
	session, err := commitSessionMCP(ctx, c, ref, params)
	if err != nil {
		return nil, err
	}
	heldRev := ""
	if anyutil.IsTrue(params.ValueOr("rev", nil)) {
		if heldRev, err = heldMCPRev(ctx, c, ref); err != nil {
			return nil, err
		}
	}
	if len(outcome.failed) > 0 && session != "applied" && len(outcome.result.Removed) == 0 && len(added) == 0 &&
		!anyutil.Equal(params.ValueOr("rev", nil), heldRev) {
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
	reply := jsonutil.ObjectOf("status", "reloaded", "message", "MCP servers reconciled ("+changed+")"+note+unavailable)
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
func commitSessionMCP(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object) (string, error) {
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
func stopProcesses(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object) (any, error) {
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
	return tuigateway.Plain(tuigateway.ProcessStopResult{Killed: int64(killed), Delegations: int64(delegations)}), nil
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

// Recently failed async delegations the session or its conversation dispatched, read from the profile's ledger
func failedDelegations(ctx context.Context, a *SessionAuthority, ref SessionRef) ([]tuigateway.FailedDelegation, error) {
	target, err := physicalTarget(ctx, a, ref)
	if err != nil {
		return nil, err
	}
	rows, err := tools.FailedDelegationsForSession(scopedToolEnv(ctx), ref.SessionID, target, 20, nil)
	if err != nil {
		return nil, err
	}
	out := []tuigateway.FailedDelegation{}
	for _, row := range rows {
		out = append(out, canonicalRow[tuigateway.FailedDelegation]("subagent.list", row))
	}
	return out, nil
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
func controlSnapshot(ctx context.Context, a *SessionAuthority, ref SessionRef) (*jsonutil.Object, error) {
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

// Last bytes of a child's transcript file, available false when the child recorded none or the file is gone
func subagentTail(path, subagentID string) tuigateway.SubagentTailResult {
	result := func() tuigateway.SubagentTailResult { return tuigateway.SubagentTailResult{SubagentID: subagentID} }
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
	out.Available, out.Text, out.Truncated = true, strings.ToValidUTF8(string(data[:n]), ""), size > ancillaryTailBytes
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
func processSnapshot(ctx context.Context, a *SessionAuthority, ref SessionRef, agent any, records []map[string]any) ([]tuigateway.ProcessEntry, error) {
	keys, owners, err := processOwnership(ctx, a, ref, agent, records)
	if err != nil {
		return nil, err
	}
	rows := []tuigateway.ProcessEntry{}
	nowEpoch := float64(time.Now().UnixNano()) / 1e9
	for _, view := range ownedViews(tools.DefaultProcessRegistry().ProcessViews(ancillaryProcessTailChars), keys, owners) {
		command := []rune(view.Command)
		if len(command) > 200 {
			command = command[:200]
		}
		uptime, preview := int64(nowEpoch-view.StartedAt), []rune(view.OutputTail)
		if len(preview) > 200 {
			preview = preview[len(preview)-200:]
		}
		previewText := string(preview)
		row := tuigateway.ProcessEntry{SessionID: view.ID, Command: string(command), Status: "running", StartedEpoch: &view.StartedAt,
			UptimeSeconds: &uptime, OutputPreview: &previewText, OutputTail: &view.OutputTail}
		if view.Exited {
			row.Status, row.CompletionReason = "exited", &view.CompletionReason
		}
		if view.ExitCode != nil {
			exitCode := int64(*view.ExitCode)
			row.ExitCode = &exitCode
		}
		if view.ExitedAt != 0 {
			row.ExitedAt = &view.ExitedAt
		}
		rows = append(rows, row)
	}
	return rows, nil
}
