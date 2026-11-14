package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/codeexec"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	delegateHeartbeatInterval     = 30 * time.Second
	delegateHeartbeatStaleIdle    = 15
	delegateHeartbeatStaleInTool  = 40
	delegateStaleResultGrace      = 2.0
	delegateLivenessPollSeconds   = 5.0
	delegateBudgetWarningFraction = 0.8
	errChildWaitTimeout           = errors.New("")
	delegateDiagChildAttrs        = []string{"model", "provider", "api_mode", "base_url", "max_iterations", "quiet_mode", "skip_memory", "skip_context_files", "platform", "_delegate_role", "_delegate_depth"}
	childRunNow                   = time.Now
)

// Reserves a queued child's session row without completing its first turn persistence
func reserveChildTranscript(child *AIAgent) error {
	db := child.sessionDB
	if db == nil || child.persistDisabled {
		return nil
	}
	if err := db.CreateSession(state.NewSession{ID: child.sessionID, Source: "subagent", ParentSessionID: child.parentSessionID}); err != nil {
		closeChild(child, "")
		return err
	}
	child.sessionDBReserved = true
	return nil
}

func strOrNone(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// Speed tier the child runs at, its own else the one its request settings carry
func tierInEffect(child *AIAgent) string {
	if child.serviceTier != "" {
		return child.serviceTier
	}
	overrides := child.requestOverrides
	extra, _ := overrides["extra_body"].(map[string]any)
	for _, scope := range []map[string]any{overrides, extra} {
		if v := scope["service_tier"]; pyrepr.Truthy(v) {
			return pyrepr.Str(v)
		}
		if scope["speed"] == "fast" {
			return "priority"
		}
	}
	return ""
}

// What a backend routed child actually ran on, nil for the default backend's unlabelled route
func routeReport(child *AIAgent) *pyjson.Object {
	if child == nil || child.delegateRoute == nil {
		return nil
	}
	out := pyjson.NewObject()
	for _, k := range child.delegateRoute.Keys {
		out.Set(k, child.delegateRoute.Vals[k])
	}
	var effort any
	if r := child.reasoningConfig; r != nil {
		enabled, has := r["enabled"]
		if !has || pyrepr.Truthy(enabled) {
			effort = r["effort"]
		} else {
			effort = "none"
		}
	}
	effortStr, _ := effort.(string)
	out.Set("provider", strOrNone(child.provider))
	out.Set("model", strOrNone(child.model))
	out.Set("reasoning_effort", strOrNone(effortStr))
	out.Set("service_tier", strOrNone(provider.ServiceTierWord(tierInEffect(child))))
	if child.fallbackActivated {
		out.Set("fallback_activated", true)
	}
	return out
}

func childRole(child *AIAgent) any {
	if child == nil || child.delegateRole == "" {
		return nil
	}
	return child.delegateRole
}

// Result entry of a child that raised, never finished, or was abandoned
func fabricatedEntry(idx int, status, errText string, child *AIAgent, duration float64) *pyjson.Object {
	e := pyjson.NewObject()
	e.Set("task_index", idx)
	e.Set("status", status)
	e.Set("summary", nil)
	e.Set("error", errText)
	e.Set("api_calls", 0)
	e.Set("duration_seconds", duration)
	e.Set("_child_role", childRole(child))
	if route := routeReport(child); route != nil {
		e.Set("route", route)
	}
	return e
}

// Records steer text that lost the race with the child's failure or timeout
func appendMissedSteer(entry *pyjson.Object, lateSteer string) {
	if lateSteer == "" {
		return
	}
	entry.Set("missed_steer", lateSteer)
	prev, _ := entry.Get("error")
	entry.Set("error", pyrepr.Str(prev)+" [steer did not land before the subagent stopped: "+lateSteer+"]")
}

// Closes the child's tool sandboxes, daemons and clients, logging any failure
func closeChild(child *AIAgent, logMessage string) {
	if child == nil {
		return
	}
	delegateQuiet(logMessage, func() {
		if err := child.Close(); err != nil && logMessage != "" {
			delegateLog().Debug(logMessage, "error", err)
		}
	})
}

// Registers the child for parent interrupt propagation, replaying a stop that already landed
func attachChild(parent, child *AIAgent) {
	if parent == nil {
		return
	}
	lock := parent.activeChildrenLock
	if lock != nil {
		lock.Lock()
	}
	parent.activeChildren = append(parent.activeChildren, child)
	if lock != nil {
		lock.Unlock()
	}
	if !parent.interruptRequested.Load() {
		return
	}
	message := parent.interruptMessage
	if parent.hardInterruptRequested == nil || parent.hardInterruptRequested.IsSet() {
		reason := message
		if reason == "" {
			reason = "parent agent interrupted"
		}
		toolReason := parent.toolInterruptReason
		if toolReason == "" {
			toolReason = "parent agent interrupted"
		}
		signalChildStop(child, reason, toolReason)
		return
	}
	delegateQuiet("Failed to propagate interrupt to late child: %v", func() { child.Interrupt(message) })
}

// Removes the child from parent interrupt propagation, a no op when absent
func detachChild(parent, child *AIAgent) {
	if parent == nil {
		return
	}
	lock := parent.activeChildrenLock
	if lock != nil {
		lock.Lock()
		defer lock.Unlock()
	}
	for i, c := range parent.activeChildren {
		if c == child {
			parent.activeChildren = append(parent.activeChildren[:i:i], parent.activeChildren[i+1:]...)
			return
		}
	}
	delegateLog().Debug("Could not remove child from active_children: list.remove(x): x not in list")
}

// Cooperative hard stop of a child with the fixed cause its tools see
func signalChildStop(child *AIAgent, reason, toolReason string) {
	if child == nil {
		return
	}
	if toolReason == "" {
		toolReason = "parent delegation ended"
	}
	delegateQuiet("", func() { RequestHardInterrupt(child, reason, toolReason) })
}

// One diagnostic section, a failing producer rendered as its labelled error line like the hermes except branch
func diagSection(label string, produce func() ([]string, error)) (lines []string) {
	defer func() {
		if r := recover(); r != nil {
			lines = []string{"  " + label + fmt.Sprint(r) + ">"}
		}
	}()
	lines, err := produce()
	if err != nil {
		return []string{"  " + label + err.Error() + ">"}
	}
	return lines
}

func diagSizes(child *AIAgent) []string {
	prompt := func() ([]string, error) {
		sys := child.ephemeralSystemPrompt
		if sys == "" {
			sys = child.cachedSystemPrompt
		}
		return []string{fmt.Sprintf("  system_prompt_bytes: %d", len(sys)), fmt.Sprintf("  system_prompt_chars: %d", pystr.Len(sys))}, nil
	}
	toolsFn := func() ([]string, error) {
		if child.tools == nil {
			return nil, nil
		}
		b, err := pyjson.EncodeWTF8PythonASCII(child.tools)
		if err != nil {
			return nil, err
		}
		return []string{fmt.Sprintf("  tool_schema_count: %d", len(child.tools)), fmt.Sprintf("  tool_schema_bytes: %d", len(b))}, nil
	}
	return append(append([]string{"## Prompt / schema sizes"}, diagSection("system_prompt: <error: ", prompt)...), diagSection("tool_schema: <error: ", toolsFn)...)
}

// Worker goroutine stack plus every other live goroutine, bounded to forty
func diagThreads(workerStack string) []string {
	lines := []string{"## Worker thread stack at timeout"}
	if workerStack == "" {
		lines = append(lines, "  <no worker thread handle>")
	} else {
		for _, l := range strings.Split(strings.TrimRight(workerStack, "\n"), "\n") {
			lines = append(lines, "  "+l)
		}
	}
	all := func() ([]string, error) {
		var out []string
		blocks := strings.Split(strings.TrimSpace(allGoroutineStacks()), "\n\n")
		for dumped, block := range blocks {
			if dumped >= 40 {
				out = append(out, fmt.Sprintf("  <%d more threads omitted>", len(blocks)-dumped-1))
				break
			}
			header, body, _ := strings.Cut(block, "\n")
			out = append(out, "  --- "+strings.TrimSuffix(header, ":")+" ---")
			for _, l := range strings.Split(body, "\n") {
				out = append(out, "    "+l)
			}
		}
		return out, nil
	}
	return append(append(lines, "", "## All thread stacks at timeout"), diagSection("<all-thread dump failed: ", all)...)
}

func diagAttr(child *AIAgent, attr string) any {
	switch attr {
	case "model":
		return strOrNone(child.model)
	case "provider":
		return strOrNone(child.provider)
	case "api_mode":
		return strOrNone(child.apiMode)
	case "base_url":
		return strOrNone(child.baseURL)
	case "max_iterations":
		return child.maxIterations
	case "quiet_mode":
		return child.quietMode
	case "skip_memory":
		return child.skipMemory
	case "skip_context_files":
		return child.skipContextFiles
	case "platform":
		return strOrNone(child.platform)
	case "_delegate_role":
		return childRole(child)
	case "_delegate_depth":
		return child.delegateDepth
	}
	return nil
}

// Diagnostic file of a subagent that timed out before any API call, empty on failure
func dumpSubagentTimeoutDiagnostic(ctx context.Context, child *AIAgent, taskIndex int, timeoutSeconds, durationSeconds float64, workerStack, goal string) string {
	logsDir := filepath.Join(home.Dir(ctx), "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return ""
	}
	subagentID := child.subagentID
	if subagentID == "" {
		subagentID = fmt.Sprintf("idx%d", taskIndex)
	}
	now := childRunNow()
	dumpPath := filepath.Join(logsDir, fmt.Sprintf("subagent-timeout-%s-%s.log", subagentID, now.Format("20060102_150405")))
	goalPreview := tools.Elide(pystr.Strip(goal), 1000)
	if goalPreview == "" {
		goalPreview = "(empty)"
	}
	lines := []string{"# Subagent timeout diagnostic — issue #14726", "# Generated: " + now.Format("2006-01-02T15:04:05.000000"), "",
		"## Timeout", fmt.Sprintf("  task_index:        %d", taskIndex), "  subagent_id:       " + subagentID,
		fmt.Sprintf("  configured_timeout: %ss", pyrepr.Repr(timeoutSeconds)), fmt.Sprintf("  actual_duration:   %.2fs", durationSeconds), "", "## Goal",
		goalPreview, "", "## Child config"}
	for _, attr := range delegateDiagChildAttrs {
		lines = append(lines, fmt.Sprintf("  %s: %s", attr, pyrepr.Repr(diagAttr(child, attr))))
	}
	var enabled any
	if child.enabledToolsets != nil {
		list := []any{}
		for _, t := range child.enabledToolsets {
			list = append(list, t)
		}
		enabled = list
	}
	lines = append(lines, "", "## Toolsets", "  enabled_toolsets:  "+pyrepr.Repr(enabled))
	if len(child.validToolNames) > 0 {
		names := make([]string, 0, len(child.validToolNames))
		for n := range child.validToolNames {
			names = append(names, n)
		}
		sort.Strings(names)
		list := []any{}
		for _, n := range names {
			list = append(list, n)
		}
		lines = append(lines, fmt.Sprintf("  loaded tool count: %d", len(names)), "  loaded tools:      "+pyrepr.Repr(list))
	}
	lines = append(append(lines, ""), diagSizes(child)...)
	lines = append(lines, "", "## Activity summary")
	lines = append(lines, diagSection("<get_activity_summary failed: ", func() ([]string, error) {
		summary := child.GetActivitySummary()
		keys := make([]string, 0, len(summary))
		for k := range summary {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var out []string
		for _, k := range keys {
			out = append(out, fmt.Sprintf("  %s: %s", k, pyrepr.Repr(summary[k])))
		}
		return out, nil
	})...)
	lines = append(append(lines, ""), diagThreads(workerStack)...)
	lines = append(lines, "", "## Notes",
		"  This file is written ONLY when a subagent times out with 0 API calls.",
		"  0-API-call timeouts mean the child never reached its first LLM request.",
		"  Common causes: oversized prompt rejected by provider, transport hang,",
		"  credential resolution stuck. See issue #14726 for context.")
	if err := os.WriteFile(dumpPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		delegateLog().Warn(fmt.Sprintf("Subagent timeout diagnostic dump failed: %v", err))
		return ""
	}
	return dumpPath
}

func budgetWarningText(idleSeconds, childTimeout float64) string {
	return fmt.Sprintf("[delegation budget warning] No progress signal for %.0fs of your %.0fs "+
		"inactivity window. Finish the current step and return your summary now — the work is discarded if the "+
		"window elapses.", idleSeconds, childTimeout)
}

// Queues the budget warning through the child's steer path
func warnChildBudget(child *AIAgent, idleSeconds, childTimeout float64) {
	delegateQuiet("budget warning steer failed: %v", func() { child.Steer(budgetWarningText(idleSeconds, childTimeout)) })
}

type activityFingerprint struct {
	apiCalls, tool, ts any
}

// Completed calls, current tool and activity clock of a child, the progress signals the stale verdict reads
func childActivityFingerprint(child *AIAgent) activityFingerprint {
	s := child.GetActivitySummary()
	return activityFingerprint{s["api_call_count"], s["current_tool"], s["last_activity_ts"]}
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case *float64:
		if n != nil {
			return *n, true
		}
	}
	return 0, false
}

func childNumInt(v any) int {
	f, _ := asFloat(v)
	return int(f)
}

// Seconds since the child's activity clock last ticked, nil when it exposes none
func childLastEventAge(child *AIAgent) any {
	ts, ok := asFloat(child.GetActivitySummary()["last_activity_ts"])
	if !ok {
		return nil
	}
	return math.Round(math.Max(0, float64(childRunNow().UnixNano())/1e9-ts)*100) / 100
}

// Parent activity heartbeat of one child on the shared periodic scheduler
type childHeartbeat struct {
	child, parent        *AIAgent
	taskIndex            int
	lastIter             int
	lastTool             any
	lastTS               *float64
	stale                int
	handle               *cron.ScheduledHandle
	settled              chan struct{}
	settleOnce           sync.Once
	mu                   sync.Mutex
	staleThresholdSecond *float64
}

func newChildHeartbeat(child, parent *AIAgent, taskIndex int) *childHeartbeat {
	return &childHeartbeat{child: child, parent: parent, taskIndex: taskIndex, settled: make(chan struct{})}
}

func (h *childHeartbeat) settle() { h.settleOnce.Do(func() { close(h.settled) }) }

func (h *childHeartbeat) start(ctx context.Context) {
	h.handle = cron.SchedulePeriodic(context.WithoutCancel(ctx), func(context.Context) bool { return h.tick() }, delegateHeartbeatInterval)
}

func (h *childHeartbeat) stop() {
	if h.handle != nil {
		h.handle.Cancel(5 * time.Second)
	}
}

func (h *childHeartbeat) staleThreshold() *float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.staleThresholdSecond
}

// One heartbeat, false once the child looks stale so the scheduler drops it
func (h *childHeartbeat) tick() bool {
	if h.parent == nil {
		return true
	}
	desc := fmt.Sprintf("delegate_task: subagent %d working", h.taskIndex)
	func() {
		defer func() {
			if r := recover(); r != nil && isUserInterruptPanic(r) {
				panic(r)
			}
		}()
		s := h.child.GetActivitySummary()
		tool := s["current_tool"]
		iter := childNumInt(s["api_call_count"])
		maxIter := childNumInt(s["max_iterations"])
		ts, hasTS := asFloat(s["last_activity_ts"])
		advanced := hasTS && (h.lastTS == nil || ts > *h.lastTS)
		if iter > h.lastIter || !reflect.DeepEqual(tool, h.lastTool) || advanced {
			h.lastIter, h.lastTool, h.stale = iter, tool, 0
			if hasTS {
				h.lastTS = &ts
			}
		} else {
			h.stale++
		}
		toolName, _ := tool.(string)
		cycles := delegateHeartbeatStaleIdle
		if toolName != "" {
			cycles = delegateHeartbeatStaleInTool
		}
		if h.stale >= cycles {
			shown := toolName
			if shown == "" {
				shown = "<none>"
			}
			delegateLog().Warn(fmt.Sprintf("Subagent %d appears stale (no progress for %d heartbeat cycles, tool=%s) — abandoning its wait",
				h.taskIndex, h.stale, shown))
			threshold := float64(cycles) * delegateHeartbeatInterval.Seconds()
			h.mu.Lock()
			h.staleThresholdSecond = &threshold
			h.mu.Unlock()
			h.settle()
			desc = ""
			return
		}
		if toolName != "" {
			desc = fmt.Sprintf("delegate_task: subagent running %s (iteration %d/%d)", toolName, iter, maxIter)
		} else if d, _ := s["last_activity_desc"].(string); d != "" {
			desc = fmt.Sprintf("delegate_task: subagent %s (iteration %d/%d)", d, iter, maxIter)
		}
	}()
	if desc == "" {
		return false
	}
	delegateQuiet("", func() { childHeartbeatTouchFn(h.parent, desc) })
	return true
}

// Parent activity touch of a heartbeat, swapped by tests that count the touches
var childHeartbeatTouchFn = func(parent *AIAgent, desc string) { parent.TouchActivity(desc) }

// Registers the live child in the subagent registry and returns its id, empty when it has none
func registerChild(ctx context.Context, child, parent *AIAgent, goal, ownerSessionID string, ownerTransport, ownerSessionRecord any) string {
	subagentID := child.subagentID
	if subagentID == "" {
		return ""
	}
	if ownerSessionID == "" {
		ownerSessionID = tools.GetSessionEnv(child.env, "ICEBATH_UI_SESSION_ID", "")
	}
	if ownerSessionID != "" && (ownerTransport == nil || ownerSessionRecord == nil) {
		ownerTransport, ownerSessionRecord = captureGatewaySteerAuthority(ctx, ownerSessionID)
	}
	ownerAgentSessionID := child.parentSessionID
	if ownerAgentSessionID == "" && parent != nil {
		ownerAgentSessionID = parent.sessionID
	}
	registerSubagent(map[string]any{
		"subagent_id": subagentID, "parent_id": strOrNone(child.parentSubagentID), "depth": max(0, child.delegateDepth-1),
		"goal": goal, "delegation_id": strOrNone(child.delegationID), "model": strOrNone(child.model),
		"started_at": float64(childRunNow().UnixNano()) / 1e9, "status": "running", "tool_count": 0, "agent": child,
		"owner_agent_session_id": strOrNone(ownerAgentSessionID), "owner_session_id": strOrNone(ownerSessionID),
		"owner_transport": ownerTransport, "owner_session_record": ownerSessionRecord,
	})
	return subagentID
}

// Isolated worktree creator of the child run, swapped by tests that seed a worktree
var createIsolatedWorktreeFn = createIsolatedWorktree

// Own git worktree for the child when isolation is on and the backend is local, nil otherwise
func createIsolatedWorktree(ctx context.Context, parent *AIAgent, parentTaskID, subagentID string) (info map[string]string) {
	if !tools.WorktreeIsolation(ctx) {
		return nil
	}
	delegateQuiet("worktree isolation setup failed: %v", func() {
		if !subagentLocalBackendActive(ctx) {
			delegateLog().Debug("worktree isolation skipped: non-local terminal backend")
			return
		}
		parentCwd, _ := tools.GetSessionCwd(parent.env, parentTaskID)
		if parentCwd == "" {
			parentCwd = resolveWorkspaceHint(parent)
		}
		info = createSubagentWorktree(ctx, parentCwd, subagentID)
	})
	return info
}

// Hands the child's close to the worker's exit and drains its pooled transports now and once more later
func deferCloseAfterTimeout(child *AIAgent, done <-chan struct{}) {
	go func() {
		<-done
		closeChild(child, "Failed to close timed-out child after worker exit")
	}()
	drainOnce := func(phase string) {
		delegateQuiet(fmt.Sprintf("Timed-out child transport drain (%s) failed", phase), func() {
			drainChildTransportsFn(child, "delegate_timeout_"+phase)
		})
	}
	drainOnce("immediate")
	time.AfterFunc(5*time.Second, func() {
		select {
		case <-done:
		default:
			drainOnce("resweep")
		}
	})
}

// Inactivity cap of one child, swapped by tests that pin a cap below the configured floor
var childTimeoutFn = tools.ChildTimeout

// Transport drain of a timed out child, swapped by tests that settle a blocked worker read themselves
var drainChildTransportsFn = func(child *AIAgent, reason string) int { return child.drainTransportsAfterAbandonment(reason) }

// Child conversation and credential swap of the child run, swapped by tests that observe the scope they run in
var (
	runChildConversationFn = func(ctx context.Context, child *AIAgent, userMessage any, o RunConversationOptions) (map[string]any, error) {
		return child.RunConversationContent(ctx, userMessage, o)
	}
	swapChildCredentialFn = func(ctx context.Context, child *AIAgent, leased provider.PooledCredential) (bool, error) {
		return child.swapCredential(ctx, leased)
	}
)

// Leases and binds a credential serving the child's endpoint from its pool
func leaseChildCredential(ctx context.Context, child *AIAgent) (*provider.Pool, string) {
	pool := child.credentialPool
	if pool == nil {
		return nil, ""
	}
	scoped := childProfileScope(ctx, child)
	leasedID, ok := pool.AcquireLease("")
	if !ok || leasedID == "" {
		return pool, ""
	}
	delegateQuiet("Failed to bind child to leased credential: %v", func() {
		var leased *provider.PooledCredential
		for _, e := range pool.Entries() {
			if e.ID == leasedID {
				e := e
				leased = &e
				break
			}
		}
		if leased == nil || !provider.CredentialPoolEntryServesEndpoint(*leased, child.baseURL) {
			pool.ReleaseLease(leasedID)
			leased = nil
			for _, e := range pool.Entries() {
				if e.LastStatus != "dead" && provider.CredentialPoolEntryServesEndpoint(e, child.baseURL) {
					e := e
					leased = &e
					break
				}
			}
			leasedID = ""
			if leased != nil {
				if id, ok := pool.AcquireLease(leased.ID); ok {
					leasedID = id
				}
			}
		}
		if leased != nil {
			if _, err := swapChildCredentialFn(scoped, child, *leased); err != nil {
				panic(err)
			}
		}
	})
	return pool, leasedID
}

// Drains steer text accepted after the child's last turn into the result
func mergeLateSteer(result map[string]any, subagentID string, child *AIAgent) {
	if subagentID == "" {
		return
	}
	if late := closeSubagentSteering(subagentID, child); late != "" {
		if existing, ok := result["pending_steer"].(string); ok && existing != "" {
			result["pending_steer"] = existing + "\n" + late
		} else {
			result["pending_steer"] = late
		}
	}
}

type schemaOutcome struct {
	schema  any
	valid   *bool
	errors  []string
	retries int
}

// Validates the final answer against the output schema with one bounded retry turn
func validateChildOutputSchema(ctx context.Context, child *AIAgent, result map[string]any, taskIndex int, childTaskID string, relay func(string)) schemaOutcome {
	schema := child.delegateOutputSchema
	if _, ok := schema.(map[string]any); !ok {
		return schemaOutcome{schema: schema}
	}
	firstText, _ := result["final_response"].(string)
	valid, errs, _ := tools.ValidateOutput(firstText, schema)
	if valid || pystr.Strip(firstText) == "" || pyTruthy(result["interrupted"]) {
		return schemaOutcome{schema, boolPtr(valid), errs, 0}
	}
	var retry map[string]any
	func() {
		defer func() {
			if r := recover(); r != nil {
				if isUserInterruptPanic(r) {
					panic(r)
				}
				delegateLog().Warn(fmt.Sprintf("Subagent %d schema-retry turn failed: %v", taskIndex, r))
			}
		}()
		runCtx := tools.WithDelegatedChildContext(childProfileScope(ctx, child), true)
		childSessionID := child.sessionID
		defer tools.ScopedCurrentSessionID(child.env, &childSessionID)()
		out, err := runChildConversationFn(runCtx, child, tools.BuildRetryMessage(errs), RunConversationOptions{TaskID: childTaskID, StreamCallback: relay})
		if err != nil {
			delegateLog().Warn(fmt.Sprintf("Subagent %d schema-retry turn failed: %v", taskIndex, err))
			return
		}
		retry = out
	}()
	if retry != nil {
		retryText, _ := retry["final_response"].(string)
		if pystr.Strip(retryText) != "" {
			result["final_response"] = retryText
		}
		result["api_calls"] = childNumInt(result["api_calls"]) + childNumInt(retry["api_calls"])
		if rm, ok := retry["messages"].([]any); ok {
			if m, ok := result["messages"].([]any); ok {
				result["messages"] = append(append([]any{}, m...), rm...)
			}
		} else if rm, ok := retry["messages"].([]map[string]any); ok {
			if m, ok := result["messages"].([]map[string]any); ok {
				result["messages"] = append(append([]map[string]any{}, m...), rm...)
			}
		}
		valid, errs, _ = tools.ValidateOutput(retryText, schema)
	}
	return schemaOutcome{schema, boolPtr(valid), errs, 1}
}

func resultMessages(v any) []map[string]any {
	switch m := v.(type) {
	case []map[string]any:
		return m
	case []any:
		out := make([]map[string]any, 0, len(m))
		for _, x := range m {
			if d, ok := x.(map[string]any); ok {
				out = append(out, d)
			}
		}
		return out
	}
	return nil
}

// Tool trace of the child's messages, pairing parallel calls with results by tool call id
func buildToolTrace(env *tools.Env, messages any) []any {
	trace := []any{}
	byID := map[string]*pyjson.Object{}
	var last *pyjson.Object
	for _, msg := range resultMessages(messages) {
		switch msg["role"] {
		case "assistant":
			calls, _ := msg["tool_calls"].([]any)
			if typed, ok := msg["tool_calls"].([]map[string]any); ok {
				for _, c := range typed {
					calls = append(calls, c)
				}
			}
			for _, raw := range calls {
				tc, _ := pyjson.Dict(raw)
				fn, _ := pyjson.Dict(tc["function"])
				name := "unknown"
				if n, ok := fn["name"]; ok {
					name = pyrepr.Str(n)
				}
				arguments, _ := fn["arguments"].(string)
				entry := pyjson.NewObject()
				entry.Set("tool", name)
				entry.Set("args_bytes", pystr.Len(arguments))
				entry.Set("input_summary", summarizeToolArguments(env, fn["arguments"]))
				trace = append(trace, entry)
				last = entry
				if id, _ := tc["id"].(string); id != "" {
					byID[id] = entry
				}
			}
		case "tool":
			content := stringifyToolContent(msg["content"])
			status := "ok"
			if looksLikeErrorOutput(content) {
				status = "error"
			}
			id, _ := msg["tool_call_id"].(string)
			target := byID[id]
			if id == "" || target == nil {
				if id != "" || last == nil {
					continue
				}
				target = last
			}
			target.Set("result_bytes", pystr.Len(content))
			target.Set("status", status)
		}
	}
	return trace
}

// Parent visible result entry with status, exit reason, tool trace, tokens and cost
func buildResultEntry(child *AIAgent, result map[string]any, taskIndex int, duration float64, schema schemaOutcome) *pyjson.Object {
	summary, _ := result["final_response"].(string)
	usable := summary != "" && pystr.Strip(summary) != "(empty)"
	interruptNote := ""
	var status, exitReason string
	switch {
	case pyTruthy(result["interrupted"]):
		status, exitReason = "interrupted", "interrupted"
		sentinels := map[string]bool{"": true, pystr.Strip(summary): true, "Operation interrupted.": true}
		msgs := resultMessages(result["messages"])
		partial := ""
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i]["role"] != "assistant" {
				continue
			}
			if t := pystr.Strip(state.FlattenMessageText(msgs[i]["content"], "\n")); !sentinels[t] {
				partial = t
				break
			}
		}
		if partial != "" {
			interruptNote, summary = pystr.Strip(summary), partial
		}
	case pyTruthy(result["failed"]) || pyTruthy(result["error"]):
		status, exitReason = "failed", "error"
	default:
		exitReason = "max_iterations"
		if pyTruthy(result["completed"]) {
			exitReason = "completed"
		}
		status = "failed"
		if usable {
			status = "completed"
		}
	}
	apiCalls, ok := result["api_calls"]
	if !ok {
		apiCalls = 0
	}
	e := pyjson.NewObject()
	e.Set("task_index", taskIndex)
	e.Set("status", status)
	e.Set("summary", summary)
	e.Set("api_calls", apiCalls)
	e.Set("duration_seconds", duration)
	e.Set("model", strOrNone(child.model))
	e.Set("exit_reason", exitReason)
	e.Set("truncated", exitReason == "max_iterations")
	tokens := pyjson.NewObject()
	tokens.Set("input", child.sessionPromptTokens)
	tokens.Set("output", child.sessionCompletionTokens)
	e.Set("tokens", tokens)
	e.Set("tool_trace", buildToolTrace(child.env, result["messages"]))
	e.Set("_child_role", childRole(child))
	cost := child.sessionEstimatedCostUSD
	e.Set("_child_cost_usd", cost)
	e.Set("cost_usd", math.Round(cost*1e6)/1e6)
	costStatus := child.sessionCostStatus
	if costStatus == "" {
		costStatus = "unknown"
	}
	e.Set("cost_status", costStatus)
	if route := routeReport(child); route != nil {
		e.Set("route", route)
	}
	if status == "failed" {
		errV, ok := result["error"]
		if !ok {
			errV = "Subagent did not produce a response."
		}
		e.Set("error", errV)
		if fr, _ := result["failure_reason"].(string); fr != "" {
			e.Set("failure_reason", fr)
		}
	} else if interruptNote != "" {
		e.Set("error", interruptNote)
	}
	if _, ok := schema.schema.(map[string]any); ok {
		valid := schema.valid != nil && *schema.valid
		e.Set("schema_valid", valid)
		if schema.retries != 0 {
			e.Set("schema_retries", schema.retries)
		}
		if !valid && len(schema.errors) > 0 {
			errs := make([]any, len(schema.errors))
			for i, s := range schema.errors {
				errs[i] = s
			}
			e.Set("schema_errors", errs)
		}
		if schema.valid != nil && !*schema.valid && usable {
			retryNote := ""
			if schema.retries != 0 {
				retryNote = " (after 1 retry)"
			}
			e.Set("schema_note", "Final answer does not satisfy the declared output_schema"+retryNote+
				"; `summary` is the child's raw, UNVALIDATED final text — extract what you need from it "+
				"yourself (see schema_errors) rather than re-running the task.")
		}
	}
	if missed, ok := result["pending_steer"].(string); ok && pystr.Strip(missed) != "" {
		e.Set("missed_steer", missed)
		note := "[steer did not land — the subagent finished before it could be delivered: " + missed + "]"
		if summary != "" {
			e.Set("summary", summary+"\n\n"+note)
		} else {
			e.Set("summary", note)
		}
	}
	return e
}

func isImageURL(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") || strings.HasPrefix(ref, "data:image/")
}

// Image input mode of the child route, swapped by tests that force native or text forwarding
var decideImageInputModeFn = tools.DecideImageInputMode

// First user message of a child forwarding images, content parts for a vision child else hint lines
func buildChildGoalMessage(ctx context.Context, goal string, images []string, child *AIAgent) (msg any) {
	defer func() {
		if r := recover(); r != nil {
			if isUserInterruptPanic(r) {
				panic(r)
			}
			delegateLog().Warn("delegate_task: image forwarding failed; sending text-only goal", "error", r)
			msg = goal
		}
	}()
	var dataURLs, urls, paths []string
	for _, s := range images {
		if strings.HasPrefix(s, "data:image/") {
			dataURLs = append(dataURLs, s)
		}
	}
	for _, s := range images {
		if isImageURL(s) && !strings.HasPrefix(s, "data:image/") {
			urls = append(urls, s)
		} else if !isImageURL(s) {
			paths = append(paths, s)
		}
	}
	mode := decideImageInputModeFn(ctx, child.env, child.provider, child.model, child.requestedProvider)
	if mode == "native" {
		parts, skipped, err := tools.BuildNativeContentParts(child.env, goal, paths, urls)
		if err != nil {
			panic(err)
		}
		if len(skipped) > 0 {
			shown := skipped
			if len(shown) > 3 {
				shown = shown[:3]
			}
			delegateLog().Warn(fmt.Sprintf("delegate_task: skipped %d unreadable image(s) for subagent: %s", len(skipped), strings.Join(shown, ", ")))
		}
		if len(dataURLs) > 0 {
			if len(parts) == 0 {
				parts = []any{map[string]any{"type": "text", "text": goal}}
			}
			for _, u := range dataURLs {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
			}
		}
		for _, p := range parts {
			if m, ok := p.(map[string]any); ok && m["type"] == "image_url" {
				return parts
			}
		}
		return goal
	}
	if len(dataURLs) > 0 {
		delegateLog().Warn(fmt.Sprintf("delegate_task: %d inline data-URL image(s) dropped for a non-vision subagent", len(dataURLs)))
	}
	var hints []string
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			hints = append(hints, "[Image attached at: "+p+"]")
		} else {
			delegateLog().Warn("delegate_task: image path not found, not forwarded: " + p)
		}
	}
	for _, u := range urls {
		hints = append(hints, "[Image attached: "+u+"]")
	}
	if len(hints) == 0 {
		return goal
	}
	return goal + "\n\n" + strings.Join(hints, "\n") + "\nUse vision_analyze to inspect these images."
}

// Non interactive approval asker answering through the subagent approval callback
type subagentApprovalAsker struct {
	cb func(command, description string, kwargs map[string]any) string
}

func (s subagentApprovalAsker) Ask(_ context.Context, qs []tools.Question) (tools.FormResult, error) {
	var answers []tools.Answer
	for _, q := range qs {
		description := strings.TrimPrefix(q.Header, "⚠️  DANGEROUS COMMAND: ")
		answers = append(answers, tools.Answer{Status: "answered", Selected: []string{s.cb(q.Text, description, nil)}})
	}
	return tools.FormResult{Outcome: "answered", Answers: answers}, nil
}

// State of one child run shared by every phase of runSingleChild
type childRun struct {
	ctx                 context.Context
	child, parent       *AIAgent
	taskIndex           int
	goal                string
	subagentID          string
	progressCB          any
	heartbeat           *childHeartbeat
	childStart          time.Time
	worktreeInfo        map[string]string
	childTaskID         string
	parentTaskID        string
	wallStart           float64
	parentReadsSnapshot []string
}

func (r *childRun) elapsed() float64 {
	return math.Round(time.Since(r.childStart).Seconds()*100) / 100
}

// Streams the child's reply text up the progress relay
func (r *childRun) relayText(delta string) {
	if delta != "" {
		safeProgress(r.progressCB, "subagent.text", "", delta, nil)
	}
}

// Inspects and prunes the child worktree into the entry, then applies the parent copy redaction
func (r *childRun) attachWorktree(entry *pyjson.Object) *pyjson.Object {
	if r.worktreeInfo != nil {
		payload, err := finalizeSubagentWorktree(r.worktreeInfo, true)
		if err != nil {
			delegateLog().Warn(fmt.Sprintf("worktree finalize failed: %v", err))
			payload = unprovenWorktreePayload(r.worktreeInfo, fmt.Sprintf("finalize raised: %v", err))
		}
		entry.Set("worktree", payload)
	}
	return redactedEntry(r.child, entry)
}

func redactedEntry(child *AIAgent, entry *pyjson.Object) *pyjson.Object {
	out, ok := tools.RedactChildResult(child.env, entry, child.delegateOutputSchema, false).(*pyjson.Object)
	if !ok {
		return entry
	}
	return out
}

// Seeds the child's cwd record and container alias from the parent and engages worktree isolation
func (r *childRun) seedWorkspace() {
	r.childTaskID = r.subagentID
	if r.childTaskID == "" {
		r.childTaskID = fmt.Sprintf("subagent-%d-%s", r.taskIndex, strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
	}
	r.parentTaskID = r.parent.currentTaskID
	delegateQuiet("Child cwd seed failed: %v", func() {
		parentCwd, _ := tools.GetSessionCwd(r.parent.env, r.parentTaskID)
		tools.RecordSessionCwd(r.child.env, r.childTaskID, parentCwd)
		tools.RegisterContainerAlias(r.childTaskID, r.parentTaskID)
	})
	r.worktreeInfo = createIsolatedWorktreeFn(r.ctx, r.parent, r.parentTaskID, r.subagentID)
	if r.worktreeInfo != nil {
		delegateQuiet("worktree cwd seed failed: %v", func() {
			tools.RecordSessionCwd(r.child.env, r.childTaskID, r.worktreeInfo["path"])
		})
		r.goal += buildWorktreeContextNote(r.worktreeInfo)
	}
	r.wallStart = float64(childRunNow().UnixNano()) / 1e9
	if r.parentTaskID != "" {
		r.parentReadsSnapshot = tools.KnownReads(r.parentTaskID)
	}
}

// Shared failure tail emitting subagent.complete, noting a late steer and reporting the worktree
func (r *childRun) finishFailed(entry *pyjson.Object, lateSteer, preview, summary, status string) *pyjson.Object {
	if status == "" {
		s, _ := entry.Get("status")
		status = pyrepr.Str(s)
	}
	duration, _ := entry.Get("duration_seconds")
	safeProgress(r.progressCB, "subagent.complete", "", tools.RedactChildText(r.child.env, preview, false), map[string]any{
		"status": status, "duration_seconds": duration, "summary": tools.RedactChildText(r.child.env, summary, false),
	})
	appendMissedSteer(entry, lateSteer)
	return r.attachWorktree(entry)
}

func (r *childRun) closeSteering() string {
	if r.subagentID == "" {
		return ""
	}
	return closeSubagentSteering(r.subagentID, r.child)
}

// Waits for the worker or the stale verdict, the child timeout bounding inactivity rather than runtime
func (r *childRun) waitLivenessAware(settled <-chan struct{}, childTimeout *float64) {
	if childTimeout == nil {
		<-settled
		return
	}
	budget := *childTimeout
	deadline := time.Now().Add(time.Duration(budget * float64(time.Second)))
	warnAt := deadline.Add(-time.Duration(budget * (1 - delegateBudgetWarningFraction) * float64(time.Second)))
	warned := false
	fingerprint := childActivityFingerprint(r.child)
	for {
		now := time.Now()
		remaining := deadline.Sub(now)
		if remaining <= 0 {
			return
		}
		wait := min(time.Duration(delegateLivenessPollSeconds*float64(time.Second)), remaining)
		if !warned {
			wait = min(wait, max(warnAt.Sub(now), 0))
		}
		select {
		case <-settled:
			return
		case <-time.After(wait):
		}
		current := childActivityFingerprint(r.child)
		if !reflect.DeepEqual(current, fingerprint) {
			fingerprint = current
			deadline = time.Now().Add(time.Duration(budget * float64(time.Second)))
			warnAt = deadline.Add(-time.Duration(budget * (1 - delegateBudgetWarningFraction) * float64(time.Second)))
			warned = false
		} else if !warned && !time.Now().Before(warnAt) {
			warned = true
			warnChildBudget(r.child, budget-time.Until(deadline).Seconds(), budget)
		}
	}
}

type childOutcome struct {
	result map[string]any
	err    error
	stack  string
}

// Runs the child's conversation on a worker goroutine, the result or a failure entry with its deferred close flag
func (r *childRun) awaitChild() (map[string]any, *pyjson.Object, bool) {
	child, taskIndex := r.child, r.taskIndex
	childTimeout := childTimeoutFn(r.ctx)
	approval := tools.SubagentApprovalCallback(r.ctx)
	var userMessage any = r.goal
	if len(child.delegateImages) > 0 {
		userMessage = buildChildGoalMessage(r.ctx, r.goal, append([]string{}, child.delegateImages...), child)
	}
	done := make(chan struct{})
	var outcome childOutcome
	workerStack := make(chan string, 1)
	go func() {
		defer close(done)
		defer func() {
			if rec := recover(); rec != nil {
				outcome.err = fmt.Errorf("%v", rec)
			}
		}()
		buf := make([]byte, 64<<10)
		workerStack <- string(buf[:runtime.Stack(buf, false)])
		saved := child.env
		if saved != nil {
			runEnv := *saved
			runEnv.Ask = subagentApprovalAsker{cb: approval}
			child.env = &runEnv
			defer func() { child.env = saved }()
		}
		runCtx := tools.WithDelegatedChildContext(childProfileScope(context.WithoutCancel(r.ctx), child), true)
		childSessionID := child.sessionID
		defer tools.ScopedCurrentSessionID(child.env, &childSessionID)()
		outcome.result, outcome.err = runChildConversationFn(runCtx, child, userMessage, RunConversationOptions{TaskID: r.childTaskID, StreamCallback: r.relayText})
	}()
	settled := r.heartbeat.settled
	go func() {
		<-done
		r.heartbeat.settle()
	}()
	var staleAfter *float64
	waitErr := func() error {
		r.waitLivenessAware(settled, childTimeout)
		select {
		case <-done:
			return outcome.err
		default:
		}
		staleAfter = r.heartbeat.staleThreshold()
		if staleAfter == nil {
			return errChildWaitTimeout
		}
		signalChildStop(child, "", "")
		select {
		case <-done:
			delegateLog().Info(fmt.Sprintf("Subagent %d settled inside the %.1fs stale-result grace window — collecting its real result", taskIndex, delegateStaleResultGrace))
			return outcome.err
		case <-time.After(time.Duration(delegateStaleResultGrace * float64(time.Second))):
			return errChildWaitTimeout
		}
	}()
	if waitErr == nil {
		return outcome.result, nil, false
	}
	lateSteer := r.closeSteering()
	signalChildStop(child, "", "")
	isTimeout := waitErr == errChildWaitTimeout
	var timeoutCause *float64
	if staleAfter != nil {
		timeoutCause = staleAfter
	} else {
		timeoutCause = childTimeout
	}
	duration := r.elapsed()
	what := fmt.Sprintf("raised %s", pyrepr.TypeName(waitErr))
	if isTimeout {
		what = "timed out"
	}
	delegateLog().Warn(fmt.Sprintf("Subagent %d %s after %.1fs", taskIndex, what, duration))
	childAPICalls := childNumInt(child.GetActivitySummary()["api_call_count"])
	beforeFirstCall := isTimeout && childAPICalls == 0
	diagnosticPath := ""
	if beforeFirstCall {
		cause := 0.0
		if timeoutCause != nil {
			cause = *timeoutCause
		}
		stack := ""
		select {
		case stack = <-workerStack:
		default:
		}
		diagnosticPath = dumpSubagentTimeoutDiagnostic(r.ctx, child, taskIndex, cause, duration, stack, r.goal)
		if diagnosticPath != "" {
			delegateLog().Warn(fmt.Sprintf("Subagent %d 0-API-call timeout — diagnostic written to %s", taskIndex, diagnosticPath))
		}
	}
	var errText string
	timeoutText := "None"
	if childTimeout != nil {
		timeoutText = pyrepr.Repr(*childTimeout)
	}
	switch {
	case !isTimeout:
		errText = waitErr.Error()
	case staleAfter != nil:
		errText = fmt.Sprintf("Subagent stopped making progress after %d API call(s) — no activity for "+
			"%ss (heartbeat stale threshold); the pending worker was abandoned.", childAPICalls, pyFormatG(*staleAfter))
	case beforeFirstCall:
		errText = fmt.Sprintf("Subagent timed out after %ss without making any API call — the child never reached its "+
			"first LLM request (prompt construction, credential resolution, or transport may be stuck).", timeoutText)
	default:
		errText = fmt.Sprintf("Subagent timed out after %ss with %d API call(s) completed — no "+
			"progress in that window (no completed call, tool change, or activity-clock tick): a stalled "+
			"provider request or an unresponsive network request.", timeoutText, childAPICalls)
	}
	if diagnosticPath != "" {
		errText += " Diagnostic: " + diagnosticPath
	}
	status := "error"
	if isTimeout {
		status = "timeout"
	}
	e := pyjson.NewObject()
	e.Set("task_index", taskIndex)
	e.Set("status", status)
	e.Set("summary", nil)
	e.Set("error", errText)
	e.Set("exit_reason", status)
	e.Set("api_calls", childAPICalls)
	e.Set("duration_seconds", duration)
	var timeoutSeconds, timedOutAfter, phase, lastAge any
	if isTimeout {
		if timeoutCause != nil {
			timeoutSeconds = *timeoutCause
		}
		timedOutAfter, lastAge = duration, childLastEventAge(child)
		phase = "after_llm_calls"
		if beforeFirstCall {
			phase = "before_first_llm_call"
		}
	}
	e.Set("timeout_seconds", timeoutSeconds)
	e.Set("timed_out_after_seconds", timedOutAfter)
	e.Set("timeout_phase", phase)
	e.Set("last_event_age", lastAge)
	e.Set("_child_role", childRole(child))
	e.Set("diagnostic_path", strOrNone(diagnosticPath))
	preview := errText
	if isTimeout {
		preview = fmt.Sprintf("Timed out after %ss", pyrepr.Repr(duration))
	} else {
		preview = waitErr.Error()
	}
	e = r.finishFailed(e, lateSteer, preview, "", "")
	closeDeferred := false
	if isTimeout {
		select {
		case <-done:
		default:
			closeDeferred = true
		}
	}
	if closeDeferred {
		deferCloseAfterTimeout(child, done)
	}
	return nil, e, closeDeferred
}

// Python general format of a float, the shortest of fixed or exponent notation
func pyFormatG(v float64) string {
	return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%g", v), ".0"), "+00")
}

// Warns the parent when the child wrote files the parent had already read
func (r *childRun) appendSiblingWriteReminder(entry *pyjson.Object) {
	if r.parentTaskID == "" || len(r.parentReadsSnapshot) == 0 {
		return
	}
	delegateQuiet("file_state sibling-write check failed", func() {
		writes := tools.WritesSince(r.parentTaskID, r.wallStart, r.parentReadsSnapshot)
		set := map[string]bool{}
		for _, paths := range writes {
			for _, p := range paths {
				set[p] = true
			}
		}
		if len(set) == 0 {
			return
		}
		modPaths := make([]string, 0, len(set))
		for p := range set {
			modPaths = append(modPaths, p)
		}
		sort.Strings(modPaths)
		shown := modPaths
		if len(shown) > 8 {
			shown = shown[:8]
		}
		reminder := "\n\n[NOTE: subagent modified files the parent previously read — re-read before editing: " + strings.Join(shown, ", ")
		if len(modPaths) > 8 {
			reminder += fmt.Sprintf(" (+%d more)", len(modPaths)-8)
		}
		reminder += "]"
		if s, _ := entry.Get("summary"); pyrepr.Truthy(s) {
			entry.Set("summary", pyrepr.Str(s)+reminder)
		} else {
			list := make([]any, len(modPaths))
			for i, p := range modPaths {
				list[i] = p
			}
			entry.Set("stale_paths", list)
		}
	})
}

// Names the child's handed off, still running and unread background processes before cleanup kills them
func (r *childRun) accountBackgroundProcesses(entry *pyjson.Object) {
	if len(r.child.handedOffProcesses) > 0 {
		handed := make([]any, len(r.child.handedOffProcesses))
		for i, p := range r.child.handedOffProcesses {
			handed[i] = p
		}
		entry.Set("handed_off_processes", handed)
	}
	delegateQuiet("", func() {
		reg := tools.DefaultProcessRegistry()
		now := float64(childRunNow().UnixNano()) / 1e9
		if leftover := reg.RunningOwnedBy(r.childTaskID); len(leftover) > 0 {
			list := []any{}
			for _, s := range leftover {
				o := pyjson.NewObject()
				o.Set("session_id", s.ID)
				o.Set("command", pystr.Slice(tools.RedactChildText(r.child.env, s.Command, false), 0, 200))
				o.Set("runtime_seconds", int(math.RoundToEven(now-s.StartedAt)))
				list = append(list, o)
			}
			entry.Set("orphaned_processes", list)
		}
		if unread := reg.UnreadCompletionsOwnedBy(r.childTaskID); len(unread) > 0 {
			list := []any{}
			for _, s := range unread {
				o := pyjson.NewObject()
				o.Set("session_id", s.ID)
				o.Set("command", pystr.Slice(tools.RedactChildText(r.child.env, s.Command, false), 0, 200))
				var code any
				if s.ExitCode != nil {
					code = *s.ExitCode
				}
				o.Set("exit_code", code)
				tail := tools.RedactChildText(r.child.env, tools.OutputTail(s, pystr.Len(s.OutputBuffer)), false)
				o.Set("output_tail", pystr.Slice(tail, max(0, pystr.Len(tail)-600), pystr.Len(tail)))
				list = append(list, o)
			}
			entry.Set("unread_completions", list)
		}
	})
}

// Fires subagent.complete with the per branch tokens, cost, files touched and tool output tail
func (r *childRun) emitComplete(result map[string]any, entry *pyjson.Object, duration float64) {
	if r.progressCB == nil {
		return
	}
	child := r.child
	redacted := redactedEntry(child, entry)
	for _, k := range redacted.Keys {
		entry.Set(k, redacted.Vals[k])
	}
	summaryV, _ := entry.Get("summary")
	summary, _ := summaryV.(string)
	errV, _ := entry.Get("error")
	errText := ""
	if errV != nil {
		errText = pyrepr.Str(errV)
	}
	var filesRead []any
	delegateQuiet("", func() {
		reads := tools.KnownReads(r.childTaskID)
		for i, p := range reads {
			if i >= 40 {
				break
			}
			filesRead = append(filesRead, p)
		}
	})
	written := map[string]bool{}
	delegateQuiet("", func() {
		for tid, paths := range tools.WritesSince("", r.wallStart, nil) {
			if tid == r.childTaskID {
				for _, p := range paths {
					written[p] = true
				}
			}
		}
	})
	writtenList := make([]string, 0, len(written))
	for p := range written {
		writtenList = append(writtenList, p)
	}
	sort.Strings(writtenList)
	if len(writtenList) > 40 {
		writtenList = writtenList[:40]
	}
	preview, longSummary := errText, errText
	if summary != "" {
		preview, longSummary = pystr.Slice(summary, 0, 160), pystr.Slice(summary, 0, 500)
	}
	status, _ := entry.Get("status")
	apiCalls, _ := entry.Get("api_calls")
	kwargs := pyjson.NewObject()
	kwargs.Set("status", status)
	kwargs.Set("duration_seconds", duration)
	kwargs.Set("summary", longSummary)
	kwargs.Set("input_tokens", child.sessionPromptTokens)
	kwargs.Set("output_tokens", child.sessionCompletionTokens)
	kwargs.Set("reasoning_tokens", child.sessionReasoningTokens)
	kwargs.Set("api_calls", childNumInt(apiCalls))
	kwargs.Set("files_read", filesRead)
	kwargs.Set("files_written", writtenList)
	kwargs.Set("output_tail", extractOutputTail(r.child.env, result, 8, 600))
	if fr, _ := entry.Get("failure_reason"); pyrepr.Truthy(fr) {
		kwargs.Set("failure_reason", fr)
	}
	kwargs.Set("cost_usd", child.sessionEstimatedCostUSD)
	cleaned, ok := tools.RedactChildResult(r.child.env, kwargs, nil, false).(*pyjson.Object)
	if !ok {
		cleaned = kwargs
	}
	extra := map[string]any{}
	for _, k := range cleaned.Keys {
		extra[k] = cleaned.Vals[k]
	}
	safeProgress(r.progressCB, "subagent.complete", "", preview, extra)
}

// Idempotent finally path tearing down the heartbeat, registry entry, lease, parent link, child and relay scope
func (r *childRun) cleanup(pool *provider.Pool, leasedID string, closeDeferred bool) {
	child := r.child
	r.heartbeat.stop()
	if r.subagentID != "" {
		unregisterSubagent(r.subagentID, child)
	}
	if pool != nil && leasedID != "" {
		delegateQuiet("Failed to release credential lease: %v", func() { pool.ReleaseLease(leasedID) })
	}
	if child.delegateSavedToolNames != nil {
		tools.RestoreLastResolvedToolNames(append([]string{}, child.delegateSavedToolNames...))
	}
	detachChild(r.parent, child)
	if !closeDeferred {
		closeChild(child, "Failed to close child agent after delegation")
	}
	delegateQuiet("Failed to dispose child execute_code kernels: %v", func() {
		codeexec.ShutdownKernelsForDelegatedChild(child.sessionID)
	})
	delegateQuiet("Failed to close child Relay session after delegation", func() {
		profileKey := tools.CurrentRelayProfileKey(r.ctx)
		relay := tools.GetRelayRuntime(r.ctx, false, profileKey)
		active := tools.RelayCoordinator.HasActiveTurn(profileKey, child.sessionID)
		if relay != nil && child.sessionID != "" && !active {
			relay.UnregisterSubagent(r.ctx, map[string]any{"child_session_id": child.sessionID})
		}
	})
}
