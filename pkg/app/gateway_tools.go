package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Poll interval of the process and subagent views
const gatewayToolsPollSeconds = 1.0

// How long a finished process stays interesting to the dock
const gatewayToolsRetainSeconds = 60.0

// Process registry surface the dock, status bar, agents and stop commands read
type cliProcessRegistry interface {
	listSessions(ctx context.Context) []map[string]any
	get(id string) (tools.ProcessDockFields, bool)
	countRunning() int
	killProcess(ctx context.Context, id string) map[string]any
	killAll(ctx context.Context, source string) (int, error)
}

// Process registry of this process for a CLI whose gateway view is unbound or final
type localProcessRegistry struct{ env *tools.Env }

func (l localProcessRegistry) listSessions(ctx context.Context) []map[string]any {
	return tools.DefaultProcessRegistry().ListSessions(ctx, l.env, "", "", false)
}

func (l localProcessRegistry) get(id string) (tools.ProcessDockFields, bool) {
	proc := tools.DefaultProcessRegistry().Get(l.env, id)
	if proc == nil {
		return tools.ProcessDockFields{}, false
	}
	return proc.DockFields(), true
}

func (l localProcessRegistry) countRunning() int {
	return tools.DefaultProcessRegistry().CountRunning()
}

func (l localProcessRegistry) killProcess(ctx context.Context, id string) map[string]any {
	return tools.DefaultProcessRegistry().KillProcess(ctx, l.env, id, "process.kill", true)
}

func (l localProcessRegistry) killAll(ctx context.Context, source string) (int, error) {
	return tools.DefaultProcessRegistry().KillAll(ctx, l.env, "", nil, source, true), nil
}

// One gateway process row as the dock reads a process session
type gatewayProc struct {
	fields         tools.ProcessDockFields
	completionText string
	exited         bool
}

func newGatewayProc(row map[string]any) gatewayProc {
	started, _ := pyNumber(row["started_epoch"])
	exitedAt, _ := pyNumber(row["exited_at"])
	fields := tools.ProcessDockFields{ID: strOrEmpty(row["session_id"]), Command: strOrEmpty(row["command"]), StartedAt: started,
		ExitedAt: exitedAt, OutputBuffer: strOrEmpty(row["output_tail"])}
	if code, ok := pyIntValue(row["exit_code"]); ok {
		fields.ExitCode = &code
	}
	reason := "exited"
	if pyrepr.Truthy(row["completion_reason"]) {
		reason = pyrepr.Str(row["completion_reason"])
	}
	return gatewayProc{fields: fields, completionText: reason, exited: row["status"] == "exited"}
}

// Background processes and subagents of the CLI's gateway session, served by its process and subagent verbs
type GatewayProcessView struct {
	connection *gatewaycli.GatewayConnection
	sessionID  func() string

	mu                  sync.Mutex
	rows                []gatewayProc
	dirty               bool
	delegations         int
	polled              float64
	refreshing          sync.Mutex
	subagents           []map[string]any
	subagentsPolled     float64
	subagentsSeen       float64
	subagentsRefreshing sync.Mutex
	tail                *gatewayTail
	subagentsWatchUntil float64
}

// Cached transcript tail of one subagent
type gatewayTail struct {
	subagentID string
	at         float64
	text       *string
}

var (
	gatewayViewMu sync.Mutex
	gatewayView   *GatewayProcessView
)

// Binds the process view to a session id source on the gateway connection, the view every reader then uses
func bindGatewayView(connection *gatewaycli.GatewayConnection, sessionID func() string) *GatewayProcessView {
	gatewayViewMu.Lock()
	defer gatewayViewMu.Unlock()
	gatewayView = nil
	if connection != nil {
		gatewayView = &GatewayProcessView{connection: connection, sessionID: sessionID, dirty: true}
	}
	return gatewayView
}

// Binds the view to the CLI's gateway agent once it is built
func bindGatewayAgent(a *GatewayAgent) *GatewayProcessView {
	return bindGatewayView(a.connection, a.SessionID)
}

// Binds the view to the CLI's open gateway session before any agent exists, following the CLI's id
func bindGatewaySession(c *CLI, connection *gatewaycli.GatewayConnection) *GatewayProcessView {
	return bindGatewayView(connection, func() string {
		c.uiMu.Lock()
		defer c.uiMu.Unlock()
		return c.sessionID
	})
}

// Marks the bound view stale after a tool may have started or stopped a process
func gatewayToolsMarkDirty() {
	if view := currentGatewayView(); view != nil {
		view.markDirty()
	}
}

func currentGatewayView() *GatewayProcessView {
	gatewayViewMu.Lock()
	defer gatewayViewMu.Unlock()
	return gatewayView
}

// Bound gateway view, nil when none is bound or its connection is final
func gatewayProcessView() *GatewayProcessView {
	view := currentGatewayView()
	if view == nil || view.connection.Final() {
		return nil
	}
	return view
}

// Process registry the CLI reads, the gateway view when bound and this process's own otherwise
func (c *CLI) processRegistry() cliProcessRegistry {
	if view := gatewayProcessView(); view != nil {
		return view
	}
	return localProcessRegistry{env: c.env}
}

// Running background processes and subagents for the status bar, cached and never an RPC
func gatewayBackgroundCounts() map[string]int {
	if view := gatewayProcessView(); view != nil {
		return map[string]int{"active_background_processes": view.countRunning(), "active_background_subagents": view.activeDelegations()}
	}
	return map[string]int{"active_background_processes": tools.DefaultProcessRegistry().CountRunning(),
		"active_background_subagents": tools.AsyncDelegationActiveCount()}
}

// Gateway session's tool catalog as definition rows and a name to toolset lookup, false off the gateway
func gatewaySessionTools(ctx context.Context) ([]map[string]any, map[string]any, bool, error) {
	view := gatewayProcessView()
	if view == nil {
		return nil, nil, false, nil
	}
	raw, err := view.connection.Call(ctx, "tools.show", map[string]any{"session_id": view.sessionID()})
	if err != nil {
		return nil, nil, true, err
	}
	result, _ := pyjson.Plain(raw).(map[string]any)
	var defs []map[string]any
	toolsetOf := map[string]any{}
	sections, _ := result["sections"].([]any)
	for _, rawSection := range sections {
		section, _ := rawSection.(map[string]any)
		rows, _ := section["tools"].([]any)
		for _, rawRow := range rows {
			row, _ := rawRow.(map[string]any)
			name := pyrepr.Str(requireKey(row, "name"))
			toolsetOf[name] = section["name"]
			defs = append(defs, map[string]any{"type": "function", "function": map[string]any{"name": name, "description": strOrEmpty(row["description"])}})
		}
	}
	return defs, toolsetOf, true, nil
}

// Rescans skills in the gateway session too, nil off the gateway or when the rescan failed
func reloadGatewaySkills(ctx context.Context) any {
	view := gatewayProcessView()
	if view == nil {
		return nil
	}
	raw, err := view.connection.Call(ctx, "skills.reload", map[string]any{"session_id": view.sessionID()})
	if err != nil {
		if !isGatewayClientError(err) {
			panic(err)
		}
		slog.Debug(fmt.Sprintf("skills.reload failed: %s", err.Error()))
		return nil
	}
	result, _ := pyjson.Plain(raw).(map[string]any)
	return result["result"]
}

// Runs the skills hub command on the local tree, then the gateway's rescan and a fresh read of its catalog
func handleGatewaySkillsSlash(c *CLI, cmd string, console *pyrich.Console) error {
	if err := skills.NewHubCLI(skillsScope(c.ctx)).HandleSkillsSlash(c.ctx, cmd, console); err != nil {
		return err
	}
	reloadGatewaySkills(c.ctx)
	invalidateGatewayCatalog(c)
	return nil
}

func (v *GatewayProcessView) markDirty() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.dirty = true
}

// Whether a listed process is running or exited within the retain window
func (v *GatewayProcessView) interestingLocked(now float64) bool {
	for _, p := range v.rows {
		if !p.exited || (p.fields.ExitedAt != 0 && now-p.fields.ExitedAt <= gatewayToolsRetainSeconds) {
			return true
		}
	}
	return false
}

// Reads process.list when forced or when a tool finished or a listed process is interesting, once a second
func (v *GatewayProcessView) refresh(ctx context.Context, force bool) {
	now := nowSeconds()
	v.mu.Lock()
	skip := !force && (now-v.polled < gatewayToolsPollSeconds || !(v.dirty || v.interestingLocked(now)))
	v.mu.Unlock()
	if skip || !v.refreshing.TryLock() {
		return
	}
	defer v.refreshing.Unlock()
	v.mu.Lock()
	v.polled, v.dirty = now, false
	v.mu.Unlock()
	raw, err := v.connection.CallWith(ctx, "process.list", map[string]any{"session_id": v.sessionID()}, gatewaycli.CallOptions{Timeout: 5 * time.Second})
	if err != nil {
		slog.Debug(fmt.Sprintf("process.list failed: %s", err.Error()))
		return
	}
	result, _ := pyjson.Plain(raw).(map[string]any)
	listed, _ := result["processes"].([]any)
	rows := make([]gatewayProc, 0, len(listed))
	for _, row := range anyRows(listed) {
		rows = append(rows, newGatewayProc(row))
	}
	v.mu.Lock()
	v.rows = rows
	v.mu.Unlock()
}

func (v *GatewayProcessView) listSessions(ctx context.Context) []map[string]any {
	v.refresh(ctx, false)
	now := nowSeconds()
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]map[string]any, 0, len(v.rows))
	for _, p := range v.rows {
		status, reason := "running", any(nil)
		if p.exited {
			status, reason = "exited", p.completionText
		}
		uptime := 0
		if p.fields.StartedAt != 0 {
			uptime = int(now - p.fields.StartedAt)
		}
		var exitCode any
		if p.fields.ExitCode != nil {
			exitCode = *p.fields.ExitCode
		}
		out = append(out, map[string]any{"session_id": p.fields.ID, "command": pystr.Slice(p.fields.Command, 0, 200), "status": status,
			"exit_code": exitCode, "completion_reason": reason, "uptime_seconds": uptime})
	}
	return out
}

func (v *GatewayProcessView) get(id string) (tools.ProcessDockFields, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, p := range v.rows {
		if p.fields.ID == id {
			return p.fields, true
		}
	}
	return tools.ProcessDockFields{}, false
}

// Running processes from the cache, the status bar never waiting on an RPC
func (v *GatewayProcessView) countRunning() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	n := 0
	for _, p := range v.rows {
		if !p.exited {
			n++
		}
	}
	return n
}

func (v *GatewayProcessView) killProcess(ctx context.Context, id string) map[string]any {
	raw, err := v.connection.Call(ctx, "process.kill", map[string]any{"session_id": v.sessionID(), "process_id": id})
	if err != nil {
		if !isGatewayClientError(err) {
			panic(err)
		}
		return map[string]any{"status": "error", "error": err.Error()}
	}
	v.refresh(ctx, true)
	result, _ := pyjson.Plain(raw).(map[string]any)
	return result
}

// Stops the session's processes and background delegations, returning the processes killed
func (v *GatewayProcessView) killAll(ctx context.Context, source string) (int, error) {
	raw, err := v.connection.Call(ctx, "process.stop", map[string]any{"session_id": v.sessionID()})
	if err != nil {
		return 0, err
	}
	result, _ := pyjson.Plain(raw).(map[string]any)
	delegations, _ := pyIntValue(orZero(result["delegations"]))
	v.mu.Lock()
	v.delegations = delegations
	v.mu.Unlock()
	v.refresh(ctx, true)
	killed, _ := pyIntValue(orZero(result["killed"]))
	return killed, nil
}

// Delegation half of stop, already run by killAll when processes were live
func (v *GatewayProcessView) interruptDelegations(ctx context.Context) (int, error) {
	v.mu.Lock()
	count := v.delegations
	v.delegations = 0
	v.mu.Unlock()
	if count != 0 {
		return count, nil
	}
	if _, err := v.killAll(ctx, ""); err != nil {
		return 0, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	count, v.delegations = v.delegations, 0
	return count, nil
}

// Running background subagents the gateway's session usage counts
func (v *GatewayProcessView) activeDelegations() int {
	state := v.connection.State(v.sessionID())
	state.Lock()
	defer state.Unlock()
	n, _ := pyIntValue(state.Usage["active_subagents"])
	return n
}

// Polls the subagent roster for a while even with no turn running
func (v *GatewayProcessView) watchSubagents(seconds float64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.subagentsWatchUntil = nowSeconds() + seconds
	v.subagentsPolled = 0
}

// Live children of the session, read at most once a second and only while there is a reason to
func (v *GatewayProcessView) subagentRows(busy bool) []map[string]any {
	now := nowSeconds()
	v.mu.Lock()
	wanted := busy || len(v.subagents) > 0 || now < v.subagentsWatchUntil
	polled := v.subagentsPolled
	v.mu.Unlock()
	wanted = wanted || v.activeDelegations() != 0
	if wanted && now-polled >= gatewayToolsPollSeconds && v.subagentsRefreshing.TryLock() {
		func() {
			defer v.subagentsRefreshing.Unlock()
			v.mu.Lock()
			v.subagentsPolled = now
			v.mu.Unlock()
			raw, err := v.connection.CallWith(context.Background(), "subagent.list", map[string]any{"session_id": v.sessionID()},
				gatewaycli.CallOptions{Timeout: 5 * time.Second})
			v.mu.Lock()
			defer v.mu.Unlock()
			if err != nil {
				slog.Debug(fmt.Sprintf("subagent.list failed: %s", err.Error()))
				if now-v.subagentsSeen > gatewayToolsRetainSeconds {
					v.subagents = nil
				}
				return
			}
			result, _ := pyjson.Plain(raw).(map[string]any)
			listed, _ := result["subagents"].([]any)
			v.subagents = anyRows(listed)
			v.subagentsSeen = now
		}()
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]map[string]any, len(v.subagents))
	for i, row := range v.subagents {
		out[i] = copyMap(row)
	}
	return out
}

// Child's live transcript tail from the cache, nil when the gateway has none, fetching at most once a second
func (v *GatewayProcessView) subagentTail(subagentID string, fetch bool) *string {
	now := nowSeconds()
	v.mu.Lock()
	var cached *gatewayTail
	if v.tail != nil && v.tail.subagentID == subagentID {
		cached = v.tail
	}
	v.mu.Unlock()
	if !fetch || (cached != nil && now-cached.at < gatewayToolsPollSeconds) {
		if cached != nil {
			return cached.text
		}
		return nil
	}
	var text *string
	raw, err := v.connection.CallWith(context.Background(), "subagent.tail", map[string]any{"session_id": v.sessionID(), "subagent_id": subagentID},
		gatewaycli.CallOptions{Timeout: 5 * time.Second})
	if err != nil {
		slog.Debug(fmt.Sprintf("subagent.tail failed: %s", err.Error()))
	} else {
		result, _ := pyjson.Plain(raw).(map[string]any)
		if got, isStr := result["text"].(string); pyrepr.Truthy(result["available"]) && isStr {
			var b strings.Builder
			for _, r := range got {
				if pystr.IsPrintable(r) || r == '\n' || r == '\t' {
					b.WriteRune(r)
				}
			}
			filtered := b.String()
			text = &filtered
		}
	}
	v.mu.Lock()
	v.tail = &gatewayTail{subagentID: subagentID, at: now, text: text}
	v.mu.Unlock()
	return text
}

// Steers or stops one child of the session, answering a note or an error like the local registry
func (v *GatewayProcessView) subagentControl(action, subagentID string, message any) (map[string]any, error) {
	status, note, failure, known := agent.SubagentControlOutcome(action)
	if !known {
		panic(fmt.Sprintf("KeyError: %s", pyrepr.Repr(action)))
	}
	var ok bool
	var raw any
	var err error
	if action == "steer" {
		raw, err = v.connection.CallWith(context.Background(), "subagent.steer", map[string]any{"session_id": v.sessionID(), "subagent_id": subagentID,
			"text": message}, gatewaycli.CallOptions{Timeout: 5 * time.Second})
		result, _ := pyjson.Plain(raw).(map[string]any)
		ok = result["status"] == "queued"
	} else {
		raw, err = v.connection.CallWith(context.Background(), "subagent.interrupt", map[string]any{"session_id": v.sessionID(), "subagent_id": subagentID},
			gatewaycli.CallOptions{Timeout: 5 * time.Second})
		result, _ := pyjson.Plain(raw).(map[string]any)
		ok = pyrepr.Truthy(result["found"])
	}
	if err != nil {
		if !isGatewayClientError(err) {
			return nil, err
		}
		return map[string]any{"error": err.Error()}, nil
	}
	v.mu.Lock()
	v.subagentsPolled = 0
	v.mu.Unlock()
	if ok {
		return map[string]any{"action": action, "subagent_id": subagentID, "status": status, "note": note}, nil
	}
	return map[string]any{"error": fmt.Sprintf(failure, subagentID)}, nil
}
