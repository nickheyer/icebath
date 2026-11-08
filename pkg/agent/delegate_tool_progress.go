package agent

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

var subagentFailureStatuses = map[string]bool{"failed": true, "error": true, "timeout": true}

// Runs fn swallowing any panic but a user interrupt, logging it at debug when a message is given
func delegateQuiet(logMessage string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			if isUserInterruptPanic(r) {
				panic(r)
			}
			if logMessage != "" {
				if strings.Contains(logMessage, "%") {
					delegateLog().Debug(fmt.Sprintf(logMessage, r))
				} else {
					delegateLog().Debug(logMessage, "error", r)
				}
			}
		}
	}()
	fn()
}

// Calls a progress callback of any accepted shape with the event, name, preview, args and keyword extras
func callProgressShape(cb any, event, name, preview string, args map[string]any, extra map[string]any) {
	switch fn := progressFn(cb).(type) {
	case toolProgressFn:
		fn(event, name, preview, args, extra)
	case func(event, name string, preview any, args any, extra map[string]any):
		fn(event, name, preview, args, extra)
	case codexProgressFn:
		fn(event, name, preview, args, codexProgressExtra(extra))
	}
}

// Invokes a child progress callback, relay failures never reach the run
func safeProgress(cb any, event, name, preview string, extra map[string]any) {
	if cb == nil || progressFn(cb) == nil {
		return
	}
	delegateQuiet(fmt.Sprintf("Progress callback %s failed: %%v", event), func() {
		callProgressShape(cb, event, name, preview, nil, extra)
	})
}

// One clean line of an error payload, the traceback's last line or the first non empty line, capped
func cleanErrorText(errText any, maxChars int) string {
	s := ""
	if pyrepr.Truthy(errText) {
		s = pyrepr.Str(errText)
	}
	var lines []string
	for _, ln := range pystr.Splitlines(pystr.Strip(tools.RedactChildText(nil, s, false)), false) {
		if t := pystr.Strip(ln); t != "" {
			lines = append(lines, t)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	line := lines[0]
	if strings.HasPrefix(lines[0], "Traceback") {
		line = lines[len(lines)-1]
	}
	if pystr.Len(line) > maxChars {
		return pystr.Slice(line, 0, maxChars-3) + "..."
	}
	return line
}

// Plain language reason of a failed child, the classified gloss else its own error reduced to one line
func describeSubagentFailure(failureReason, errText any, maxChars int) string {
	if gloss, ok := tools.FailureCauseGloss(failureReason, "it", "its"); ok && gloss != "" {
		return gloss
	}
	return cleanErrorText(errText, maxChars)
}

func formatFailureDuration(seconds any) string {
	var v float64
	switch n := seconds.(type) {
	case int:
		v = float64(n)
	case int64:
		v = float64(n)
	case float64:
		v = n
	default:
		return ""
	}
	if v <= 0 {
		return ""
	}
	if v >= 120 {
		return fmt.Sprintf("%d min", int(math.RoundToEven(v/60)))
	}
	return fmt.Sprintf("%ds", int(math.RoundToEven(v)))
}

// Human readable line about a failed or timed out subagent with the next step for the user
func formatSubagentFailureLine(goal, status string, errText, durationSeconds, failureReason any) string {
	goalLabel := strings.ReplaceAll(pystr.Strip(goal), "\n", " ")
	if pystr.Len(goalLabel) > 60 {
		goalLabel = pystr.Slice(goalLabel, 0, 57) + "..."
	}
	goalPart := ""
	if goalLabel != "" {
		goalPart = fmt.Sprintf(` — "%s"`, goalLabel)
	}
	elapsed := formatFailureDuration(durationSeconds)
	if status == "timeout" {
		after := ""
		if elapsed != "" {
			after = " after " + elapsed
		}
		return fmt.Sprintf("⚠️ Subagent timed out%s%s without finishing. I will carry on without it; ask me to "+
			"retry it, or raise delegation.child_timeout_seconds in config.yaml if these tasks legitimately "+
			"take longer.", goalPart, after)
	}
	line := "⚠️ Subagent failed" + goalPart
	if elapsed != "" {
		line += " after " + elapsed
	}
	if reason := describeSubagentFailure(failureReason, errText, 200); reason != "" {
		line += ": " + strings.TrimRight(reason, ".")
	}
	return line + ". Details: /agents, or ask me to retry with a smaller task."
}

const (
	delegateEventTaskSpawned       = "delegate.task_spawned"
	delegateEventTaskProgress      = "delegate.task_progress"
	delegateEventTaskCompleted     = "delegate.task_completed"
	delegateEventTaskFailed        = "delegate.task_failed"
	delegateEventTaskThinking      = "delegate.task_thinking"
	delegateEventTaskToolStarted   = "delegate.tool_started"
	delegateEventTaskToolCompleted = "delegate.tool_completed"
)

var (
	delegateEvents = map[string]bool{
		delegateEventTaskSpawned: true, delegateEventTaskProgress: true, delegateEventTaskCompleted: true,
		delegateEventTaskFailed: true, delegateEventTaskThinking: true, delegateEventTaskToolStarted: true,
		delegateEventTaskToolCompleted: true,
	}
	legacyDelegateEvents = map[string]string{
		"_thinking": delegateEventTaskThinking, "reasoning.available": delegateEventTaskThinking,
		"tool.started": delegateEventTaskToolStarted, "tool.completed": delegateEventTaskToolCompleted,
		"subagent_progress": delegateEventTaskProgress,
	}
	delegateLifecycleEvents = map[string]bool{
		"subagent.spawn_requested": true, "subagent.start": true, "subagent.complete": true, "subagent.text": true,
	}
)

// Dispatch key of a lifecycle, delegate or legacy event string, empty for an unknown event
func normalizeDelegateEvent(event string) string {
	if delegateLifecycleEvents[event] {
		return event
	}
	if mapped, ok := legacyDelegateEvents[event]; ok {
		return mapped
	}
	if delegateEvents[event] {
		return event
	}
	return ""
}

const (
	childContextFilesIntro = "\nThe workspace's project context files are reproduced below. Their conventions and invariants are binding for " +
		"your work in this workspace.\n\n"
	childCompletionInstructions = "\nComplete this task using the tools available to you. When finished, provide a clear, concise summary of:\n" +
		"- What you did\n- What you found or accomplished\n- Any files you created or modified\n- Any issues encountered\n\n" +
		"Important workspace rule: Never assume a repository lives at /workspace/... or any other container-style path " +
		"unless the task/context explicitly gives that path. If no exact local path is provided, discover it first before " +
		"issuing git/workdir-specific commands.\n\n" +
		"Keep your final summary tight: lead with outcomes, prefer bullet points over paragraphs, and don't replay your " +
		"whole process. Your response is returned to the parent agent as a summary, and overlong summaries crowd out the " +
		"parent's context window."
	childOrchestratorBlock = "\n## Subagent Spawning (Orchestrator Role)\n" +
		"You have access to the `delegate_task` tool and CAN spawn your own subagents to parallelize independent work.\n\n" +
		"WHEN to delegate:\n" +
		"- The goal decomposes into 2+ independent subtasks that can run in parallel (e.g. research A and B simultaneously).\n" +
		"- A subtask is reasoning-heavy and would flood your context with intermediate data.\n\n" +
		"WHEN NOT to delegate:\n" +
		"- Single-step mechanical work — do it directly.\n" +
		"- Trivial tasks you can execute in one or two tool calls.\n" +
		"- Re-delegating your entire assigned goal to one worker (that's just pass-through with no value added).\n\n" +
		"Coordinate your workers' results and synthesize them before reporting back to your parent. You are responsible " +
		"for the final summary, not your workers.\n\n"
	childLeafChildrenNote = "Your own children MUST be leaves (cannot delegate further) because they will reach the maximum nesting depth. " +
		"The system manages this automatically; no parameter override is available."
	childNestedChildrenNote = "Your own children can themselves delegate because depth remains. The system determines this automatically " +
		"from the nesting depth; children at the maximum depth cannot delegate further. No parameter override is available."
)

// Focused system prompt of a child with its context, workspace files and the orchestrator block by role
func buildChildSystemPrompt(ctx context.Context, env *tools.Env, contextText, workspacePath, role string, maxSpawnDepth, childDepth int) string {
	parts := []string{"You are a focused subagent working on a specific delegated task."}
	if contextText != "" && pystr.Strip(contextText) != "" {
		parts = append(parts, "\nCONTEXT:\n"+contextText)
	}
	if workspacePath != "" && pystr.Strip(workspacePath) != "" {
		parts = append(parts, "\nWORKSPACE PATH:\n"+workspacePath+"\n"+
			"Use this exact path for local repository/workdir operations unless the task explicitly says otherwise.")
		ctxFiles := ""
		delegateQuiet("subagent: workspace context-files load failed", func() {
			cwd := workspacePath
			out, err := BuildContextFilesPrompt(ctx, env, ContextFilesOptions{Cwd: &cwd, SkipSoul: true})
			if err != nil {
				delegateLog().Debug("subagent: workspace context-files load failed", "error", err)
				return
			}
			ctxFiles = out
		})
		if pystr.Strip(ctxFiles) != "" {
			parts = append(parts, childContextFilesIntro+pystr.Strip(ctxFiles))
		}
	}
	parts = append(parts, childCompletionInstructions)
	if role == "orchestrator" {
		note := childNestedChildrenNote
		if childDepth+1 >= maxSpawnDepth {
			note = childLeafChildrenNote
		}
		parts = append(parts, childOrchestratorBlock+
			fmt.Sprintf("NOTE: You are at depth %d. The delegation tree is capped at max_spawn_depth=%d. ", childDepth, maxSpawnDepth)+note)
	}
	return strings.Join(parts, "\n")
}

// Concrete absolute local directory for the child prompt, empty when none is known
func resolveWorkspaceHint(parent *AIAgent) string {
	var candidates []string
	if cwd, err := tools.ScopeTerminalCwd(parent.env); err == nil {
		candidates = append(candidates, cwd)
	}
	if hints, ok := parent.subdirectoryHints.(*tools.SubdirectoryHintTracker); ok && hints != nil {
		candidates = append(candidates, hints.WorkingDir())
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		text, err := filepath.Abs(expandUserPath(candidate))
		if err != nil || !filepath.IsAbs(text) {
			continue
		}
		if st, err := os.Stat(text); err == nil && st.IsDir() {
			return text
		}
	}
	return ""
}

// Tilde expansion of a leading home reference like os.path.expanduser
func expandUserPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return h + p[1:]
		}
	}
	return p
}

var (
	batchOrdinals   = map[string]map[string]int{}
	batchOrdinalsMu sync.Mutex
)

// Short per conversation tag of a delegation batch like set 2, empty without an id
func formatBatchTag(delegationID string, parent *AIAgent) string {
	if delegationID == "" {
		return ""
	}
	scope := ""
	if parent != nil {
		scope = parent.sessionID
	}
	batchOrdinalsMu.Lock()
	defer batchOrdinalsMu.Unlock()
	ordinals := batchOrdinals[scope]
	if ordinals == nil {
		ordinals = map[string]int{}
		batchOrdinals[scope] = ordinals
	}
	n, ok := ordinals[delegationID]
	if !ok {
		n = len(ordinals) + 1
		ordinals[delegationID] = n
	}
	return fmt.Sprintf("set %d", n)
}

// Bracketed slot prefix of a batch child line, the bare tag for a lone child
func batchPrefix(delegationID string, taskIndex, taskCount int, parent *AIAgent) string {
	tag := formatBatchTag(delegationID, parent)
	if taskCount > 1 {
		inner := fmt.Sprintf("%d/%d", taskIndex+1, taskCount)
		if tag != "" {
			inner = fmt.Sprintf("%s · %d/%d", tag, taskIndex+1, taskCount)
		}
		return "[" + inner + "] "
	}
	if tag != "" {
		return "[" + tag + "] "
	}
	return ""
}

// Prints a progress line through the parent's safe printer so headless hosts can redirect it
func emitParentConsole(parent *AIAgent, line string) {
	if parent != nil {
		printed := false
		delegateQuiet("", func() {
			parent.safePrint(line, false)
			printed = true
		})
		if printed {
			return
		}
	}
	fmt.Println(line)
}

// Above spinner line when a spinner exists, else the console line or the line indented two spaces
func printCompletionLine(parent *AIAgent, spinner tools.Spinner, line string, consoleLine *string) {
	if p, ok := spinner.(interface{ PrintAbove(string) }); ok && spinner != nil {
		printed := false
		delegateQuiet("", func() {
			p.PrintAbove(line)
			printed = true
		})
		if printed {
			return
		}
	}
	if consoleLine != nil {
		emitParentConsole(parent, *consoleLine)
		return
	}
	emitParentConsole(parent, "  "+line)
}

func shortText(text string, n int) string {
	if pystr.Len(text) > n {
		return pystr.Slice(text, 0, n) + "..."
	}
	return text
}

// Relays one child's events to the parent spinner and progress callback with its identity fields
type childProgressRelay struct {
	taskIndex, taskCount int
	goalLabel            string
	spinner              tools.Spinner
	parentCB             any
	sessionRef           map[string]any
	sessionRefMu         *sync.Mutex
	subagentID, parentID string
	depth                *int
	model                string
	toolsets             []string
	batch                []string
	parentScope          *AIAgent
	toolCount            int
	mu                   sync.Mutex
}

const childProgressBatchSize = 5

func (r *childProgressRelay) refString(key string) string {
	r.sessionRefMu.Lock()
	defer r.sessionRefMu.Unlock()
	return pyrepr.Str(r.sessionRef[key])
}

func (r *childProgressRelay) prefix() string {
	r.sessionRefMu.Lock()
	id, _ := r.sessionRef["delegation_id"].(string)
	scope, _ := r.sessionRef["_parent_scope"].(*AIAgent)
	r.sessionRefMu.Unlock()
	return batchPrefix(id, r.taskIndex, r.taskCount, scope)
}

func (r *childProgressRelay) identityKwargs() map[string]any {
	kw := map[string]any{"task_index": r.taskIndex, "task_count": r.taskCount, "goal": r.goalLabel}
	if r.subagentID != "" {
		kw["subagent_id"] = r.subagentID
	}
	if r.parentID != "" {
		kw["parent_id"] = r.parentID
	}
	if r.depth != nil {
		kw["depth"] = *r.depth
	}
	if r.model != "" {
		kw["model"] = r.model
	}
	if r.toolsets != nil {
		kw["toolsets"] = append([]string{}, r.toolsets...)
	}
	r.sessionRefMu.Lock()
	for _, pair := range [][2]string{{"session_id", "child_session_id"}, {"delegation_id", "delegation_id"}} {
		if v := r.sessionRef[pair[0]]; pyrepr.Truthy(v) {
			kw[pair[1]] = pyrepr.Str(v)
		}
	}
	r.sessionRefMu.Unlock()
	kw["tool_count"] = r.toolCount
	return kw
}

func (r *childProgressRelay) relay(event, name, preview string, args map[string]any, kwargs map[string]any) {
	if r.parentCB == nil {
		return
	}
	delegateQuiet("Parent callback failed: %v", func() {
		merged := r.identityKwargs()
		for k, v := range kwargs {
			merged[k] = v
		}
		callProgressShape(r.parentCB, event, name, preview, args, merged)
	})
}

func (r *childProgressRelay) treeLine(text string) {
	p, ok := r.spinner.(interface{ PrintAbove(string) })
	if r.spinner == nil || !ok {
		return
	}
	delegateQuiet("Spinner print_above failed: %v", func() { p.PrintAbove(" " + r.prefix() + "├─ " + text) })
}

// Sends any batched tool names to the parent progress callback
func (r *childProgressRelay) flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushLocked()
}

func (r *childProgressRelay) flushLocked() {
	if r.parentCB != nil && len(r.batch) > 0 {
		r.relay("subagent.progress", "", "🔀 "+r.prefix()+strings.Join(r.batch, ", "), nil, nil)
		r.batch = r.batch[:0]
	}
}

func orString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (r *childProgressRelay) onComplete(preview string, kwargs map[string]any) {
	status, _ := kwargs["status"].(string)
	if subagentFailureStatuses[status] {
		parent := r.parentScope
		platform := "cli"
		var userConfig map[string]any
		if parent != nil {
			platform = parent.platform
			if parent.notificationPlatform != nil {
				platform = *parent.notificationPlatform
			}
			userConfig = parent.notificationConfig
		}
		errText := kwargs["summary"]
		if !pyrepr.Truthy(errText) {
			errText = preview
		}
		_, _ = config.RenderNotification(context.Background(), func() error {
			r.treeLine(formatSubagentFailureLine(r.goalLabel, status, errText, kwargs["duration_seconds"], kwargs["failure_reason"]))
			return nil
		}, platform, true, userConfig)
	}
	r.relay("subagent.complete", "", preview, nil, kwargs)
}

func (r *childProgressRelay) onToolStarted(name, preview string, args map[string]any) {
	r.toolCount++
	if r.subagentID != "" {
		activeSubagentsMu.Lock()
		if rec := activeSubagents.get(r.subagentID); rec != nil {
			rec["tool_count"] = r.toolCount
			rec["last_tool"] = name
		}
		activeSubagentsMu.Unlock()
	}
	if r.spinner != nil {
		line := tools.GetToolEmoji(nil, name, "⚡") + " " + name
		short := ""
		if preview != "" {
			short = shortText(preview, 35)
		}
		if short != "" {
			r.treeLine(fmt.Sprintf(`%s  "%s"`, line, short))
		} else {
			r.treeLine(line)
		}
	}
	if r.parentCB != nil {
		r.relay("subagent.tool", name, preview, args, nil)
		r.batch = append(r.batch, name)
		if len(r.batch) >= childProgressBatchSize {
			r.flushLocked()
		}
	}
}

// Dispatches one child event to its handler by the normalized event key
func (r *childProgressRelay) call(event, name, preview string, args map[string]any, kwargs map[string]any) {
	key := normalizeDelegateEvent(event)
	if key == "" || key == delegateEventTaskToolCompleted {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch key {
	case "subagent.spawn_requested":
		r.relay("subagent.spawn_requested", "", orString(preview, r.goalLabel), nil, kwargs)
	case "subagent.start":
		if r.goalLabel != "" {
			r.treeLine("🔀 " + shortText(r.goalLabel, 55))
		}
		r.relay("subagent.start", "", orString(preview, r.goalLabel), nil, kwargs)
	case "subagent.complete":
		r.onComplete(preview, kwargs)
	case "subagent.text":
		r.relay("subagent.text", "", preview, nil, nil)
	case delegateEventTaskThinking:
		text := orString(preview, name)
		r.treeLine(fmt.Sprintf(`💭 "%s"`, shortText(text, 55)))
		r.relay("subagent.thinking", "", text, nil, nil)
	case delegateEventTaskProgress:
		summary := orString(name, preview)
		if summary != "" {
			r.treeLine("🔀 " + summary)
		}
		if r.parentCB != nil {
			delegateQuiet("Parent callback relay failed: %v", func() {
				callProgressShape(r.parentCB, "subagent_progress", r.prefix()+summary, "", nil, nil)
			})
		}
	default:
		r.onToolStarted(name, preview, args)
	}
}

// Relay of one child's events, nil when the parent has neither a spinner nor a progress callback
func buildChildProgressCallback(taskIndex int, goal string, parent *AIAgent, taskCount int, subagentID, parentID string,
	depth *int, model string, toolsets []string, sessionRef map[string]any, sessionRefMu *sync.Mutex) any {
	spinner := parent.delegateSpinner
	parentCB := parent.toolProgressCallback
	if spinner == nil && parentCB == nil {
		return nil
	}
	if sessionRef == nil {
		sessionRef, sessionRefMu = map[string]any{}, &sync.Mutex{}
	} else {
		sessionRefMu.Lock()
		sessionRef["_parent_scope"] = parent
		sessionRefMu.Unlock()
	}
	r := &childProgressRelay{
		taskIndex: taskIndex, taskCount: taskCount, goalLabel: pystr.Strip(goal), spinner: spinner, parentCB: parentCB,
		sessionRef: sessionRef, sessionRefMu: sessionRefMu, subagentID: subagentID, parentID: parentID, depth: depth,
		model: model, toolsets: toolsets, parentScope: parent,
	}
	return &flushingProgressCallback{call: toolProgressFn(r.call), flush: r.flush}
}
