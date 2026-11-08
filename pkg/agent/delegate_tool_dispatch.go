package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	syncNoteNoAsync = "background=true is not available in this session — it cannot " +
		"receive a detached subagent result after the turn ends (a " +
		"finite chat using -Q, --oneshot, or non-TTY stdio, `icebath -z`, a /bg background task, a cron " +
		"job, a Kanban worker, or a stateless HTTP endpoint). The subagent(s) ran SYNCHRONOUSLY and the result is included above."
	syncNoteAtCapacity = "The background delegation pool was at capacity (delegation.max_concurrent_children), so the subagent(s) ran " +
		"SYNCHRONOUSLY and the result is included above. Raise " +
		"delegation.max_concurrent_children in config.yaml to allow more concurrent background delegations."
	backgroundNoteOne = "Subagent is running in the background; its full result re-enters the conversation as a new message when it " +
		"finishes. Results are delivered only after you END YOUR TURN: do anything that does not depend on it, then " +
		"stop with a one-line status. Do not poll its transcript or artifacts to wait for it."
	backgroundNoteMany = "%d subagents are running in parallel in the background as %d completion unit(s); each unit's results " +
		"re-enter the conversation as their own new message when THAT unit finishes. Results are delivered only " +
		"after you END YOUR TURN: do anything that does not depend on them, then stop with a one-line status. Do not " +
		"poll transcripts or artifacts to wait for them."
	backgroundControlHint = "While a child runs you can orchestrate it live with this same tool: delegate_task(action='list') to see live " +
		"children, action='steer' with subagent_id + message to redirect one, action='stop' with subagent_id to end " +
		"one early."
	backgroundLiveTranscriptsHint = "Each subagent streams a human-readable transcript of its operations to the file listed above (append-only, " +
		"one per task). Read or `tail -f` these paths at any time to watch a child work while it runs."
)

var delegatePollInterval = 500 * time.Millisecond

func goalOf(task map[string]any) string { return pyrepr.Str(task["goal"]) }

// Runs one child of the batch under the originating session's steer and stop authority
func (b *delegateBatch) runChild(ctx context.Context, c delegateChild) *pyjson.Object {
	return runSingleChildFn(ctx, c.Index, goalOf(c.Task), c.Child, b.Parent, b.Origin.UISessionID, b.Origin.OwnerTransport, b.Origin.OwnerSessionRecord)
}

// Prints the batch tag once so interleaved slot lines are attributable
func announceBatch(parent *AIAgent, taskCount int, delegationID string) {
	if taskCount > 1 && delegationID != "" {
		hdr := fmt.Sprintf("  🔀 [%s] delegating %d tasks", formatBatchTag(delegationID, parent), taskCount)
		printCompletionLine(parent, parent.delegateSpinner, hdr, &hdr)
	}
}

// Wake id, ui session, steer authority and history delivery of the originating session, read before any child exists
func captureOrigin(ctx context.Context) delegationOrigin {
	env := tools.ToolEnvFrom(ctx)
	o := delegationOrigin{WakeSID: tools.CurrentOriginSessionID(env)}
	delegateQuiet("", func() {
		o.UISessionID = tools.GetSessionEnv(env, "ICEBATH_UI_SESSION_ID", "")
		o.SessionHistoryDelivery = tools.SessionHistoryDeliverySupported(env)
	})
	o.OwnerTransport, o.OwnerSessionRecord = captureGatewaySteerAuthority(ctx, o.UISessionID)
	return o
}

func entryValue(e *pyjson.Object, key string) any {
	v, _ := e.Get(key)
	return v
}

// One completion line for a finished child plus the spinner's remaining count
func reportChildDone(parent *AIAgent, spinner any, entry *pyjson.Object, tag string, labels []string, nTasks, remaining int) {
	idx := childNumInt(entryValue(entry, "task_index"))
	label := fmt.Sprintf("Task %d", idx)
	if idx < len(labels) {
		label = labels[idx]
	}
	status := "?"
	if s, ok := entry.Get("status"); ok {
		status = pyrepr.Str(s)
	}
	slot := fmt.Sprintf("%d/%d", idx+1, nTasks)
	if tag != "" {
		slot = fmt.Sprintf("%s · %d/%d", tag, idx+1, nTasks)
	}
	schemaInvalid := entryValue(entry, "schema_valid") == false && status == "completed"
	icon := "✗"
	switch {
	case schemaInvalid:
		icon = "⚠"
	case status == "completed":
		icon = "✓"
	}
	duration, ok := entry.Get("duration_seconds")
	if !ok {
		duration = 0
	}
	line := fmt.Sprintf("%s [%s] %s  (%ss)", icon, slot, label, pyrepr.Str(duration))
	errLine := ""
	if subagentFailureStatuses[status] {
		errLine = describeSubagentFailure(entryValue(entry, "failure_reason"), entryValue(entry, "error"), 120)
	}
	if schemaInvalid {
		errLine = "output_schema not satisfied — raw text returned (schema_valid=false)"
	}
	if errLine != "" {
		line += " — " + errLine
	}
	sp, _ := spinner.(interface{ Stop(string) })
	var ts tools.Spinner
	if sp != nil {
		ts = sp
	}
	printCompletionLine(parent, ts, line, nil)
	if u, ok := spinner.(interface{ UpdateText(string) }); ok && spinner != nil && remaining > 0 {
		delegateQuiet("Spinner update_text failed: %v", func() {
			prefix := ""
			if tag != "" {
				prefix = "[" + tag + "] "
			}
			plural := "s"
			if remaining == 1 {
				plural = ""
			}
			u.UpdateText(fmt.Sprintf("🔀 %s%d task%s remaining", prefix, remaining, plural))
		})
	}
}

func objectMap(o *pyjson.Object) map[string]any {
	out := make(map[string]any, len(o.Keys))
	for _, k := range o.Keys {
		out[k] = o.Vals[k]
	}
	return out
}

// Durably records a finished child on a detached unit's row before the unit joins
func recordFinishedChild(batch *delegateBatch, entry *pyjson.Object, honorParentInterrupt bool) {
	if !honorParentInterrupt && batch.UnitID != "" && entry != nil {
		if err := tools.RecordUnitChild(batch.Parent.env, batch.UnitID, objectMap(entry)); err != nil {
			delegateLog().Debug("record_unit_child failed", "error", err)
		}
	}
}

func sortByTaskIndex(results []*pyjson.Object) {
	sort.SliceStable(results, func(i, j int) bool {
		return childNumInt(entryValue(results[i], "task_index")) < childNumInt(entryValue(results[j], "task_index"))
	})
}

type childFuture struct {
	index int
	done  chan struct{}
	entry *pyjson.Object
	err   error
}

// Runs the batch's children in parallel, polling so a parent interrupt abandons wedged children
func runChildrenParallel(ctx context.Context, batch *delegateBatch, results *[]*pyjson.Object, honorParentInterrupt bool) {
	parent, nTasks := batch.Parent, len(batch.TaskList)
	labels := make([]string, len(batch.TaskList))
	for i, t := range batch.TaskList {
		labels[i] = pystr.Slice(goalOf(t), 0, 40)
	}
	spinner := any(parent.delegateSpinner)
	tag := formatBatchTag(batch.LiveDelegID, parent)
	childByIndex := map[int]*AIAgent{}
	for _, c := range batch.Children {
		childByIndex[c.Index] = c.Child
	}
	nHere := len(batch.Children)
	sem := make(chan struct{}, max(1, batch.MaxChildren))
	finished := make(chan *childFuture, len(batch.Children))
	pending := map[*childFuture]bool{}
	for _, c := range batch.Children {
		f := &childFuture{index: c.Index, done: make(chan struct{})}
		pending[f] = true
		go func(c delegateChild) {
			sem <- struct{}{}
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					if err, ok := r.(error); ok {
						f.err = err
					} else {
						f.err = errors.New(fmt.Sprint(r))
					}
				}
				close(f.done)
				finished <- f
			}()
			f.entry = batch.runChild(ctx, c)
		}(c)
	}
	entryOf := func(f *childFuture) *pyjson.Object {
		select {
		case <-f.done:
		default:
			return fabricatedEntry(f.index, "interrupted", "Parent agent interrupted — child did not finish in time", childByIndex[f.index], 0)
		}
		if f.err != nil {
			return fabricatedEntry(f.index, "error", f.err.Error(), childByIndex[f.index], 0)
		}
		return f.entry
	}
	for len(pending) > 0 {
		if honorParentInterrupt && parent.IsInterrupted() {
			var rest []*childFuture
			for f := range pending {
				rest = append(rest, f)
			}
			sort.Slice(rest, func(i, j int) bool { return rest[i].index < rest[j].index })
			for _, f := range rest {
				*results = append(*results, entryOf(f))
			}
			break
		}
		var done []*childFuture
		select {
		case f := <-finished:
			done = append(done, f)
		drain:
			for {
				select {
				case f := <-finished:
					done = append(done, f)
				default:
					break drain
				}
			}
		case <-time.After(delegatePollInterval):
		}
		for _, f := range done {
			delete(pending, f)
			entry := entryOf(f)
			*results = append(*results, entry)
			recordFinishedChild(batch, entry, honorParentInterrupt)
			reportChildDone(parent, spinner, entry, tag, labels, nTasks, nHere-len(*results))
			status := pyrepr.Str(entryValue(entry, "status"))
			if !honorParentInterrupt && batch.UnitID != "" && subagentFailureStatuses[status] && len(*results) < nHere {
				delegateQuiet("task failure notice failed", func() {
					notice := objectMap(entry)
					i, isInt := entryValue(entry, "task_index").(int)
					if isInt && i >= 0 && i < len(batch.LivePaths) && batch.LivePaths[i] != "" {
						notice["live_transcript"] = batch.LivePaths[i]
					}
					tools.PushTaskFailureNotice(batch.Parent.env, batch.UnitID, notice, nTasks)
				})
			}
		}
	}
	sortByTaskIndex(*results)
}

// Identity of the call every unit of one delegate_task shares
func batchCallKey(batch *delegateBatch) any {
	if len(batch.TaskList) == 0 {
		return batch
	}
	return reflect.ValueOf(batch.TaskList).Pointer()
}

// Runs, joins and finalizes the batch's children and returns the combined result
func executeAndAggregate(ctx context.Context, batch *delegateBatch, honorParentInterrupt bool) *pyjson.Object {
	var results []*pyjson.Object
	if len(batch.Children) == 1 {
		results = append(results, batch.runChild(ctx, batch.Children[0]))
		recordFinishedChild(batch, results[0], honorParentInterrupt)
	} else {
		runChildrenParallel(ctx, batch, &results, honorParentInterrupt)
	}
	finalizeChildResults(ctx, results, batch.TaskList, batch.Children, batch.Parent)
	totalDuration := math.Round(time.Since(batch.OverallStart).Seconds()*100) / 100
	for _, entry := range results {
		idx, isInt := entryValue(entry, "task_index").(int)
		if isInt && idx >= 0 && idx < len(batch.LiveWriters) && batch.LiveWriters[idx] != nil {
			delegateQuiet("Live transcript finalize failed", func() {
				if err := batch.LiveWriters[idx].Finalize(entry); err != nil {
					delegateLog().Debug("Live transcript finalize failed", "error", err)
				}
			})
			if idx < len(batch.LivePaths) {
				entry.Set("live_transcript", batch.LivePaths[idx])
			}
		}
	}
	if len(batch.Refused) > 0 {
		results = append(results, batch.Refused...)
		sortByTaskIndex(results)
	}
	updateManifestStatuses(batch.LiveDelegID, results, batch.LiveHome, true)
	anyResults := make([]any, len(results))
	for i, r := range results {
		anyResults[i] = r
	}
	observability.FinishDelegationUnit(ctx, batchCallKey(batch), anyResults, !honorParentInterrupt)
	combined := pyjson.NewObject()
	combined.Set("results", anyResults)
	combined.Set("total_duration_seconds", totalDuration)
	var notes []any
	for _, r := range results {
		for _, line := range tools.ProcessAccountingLines(objectMap(r)) {
			notes = append(notes, line)
		}
	}
	if len(notes) > 0 {
		combined.Set("process_notes", notes)
	}
	var unitPaths []any
	for _, c := range batch.Children {
		if c.Index < len(batch.LivePaths) {
			unitPaths = append(unitPaths, batch.LivePaths[c.Index])
		}
	}
	if len(unitPaths) > 0 {
		combined.Set("live_transcripts", unitPaths)
	}
	if batch.Group != nil {
		combined.Set("group", *batch.Group)
	}
	return combined
}

func dumpsResult(v any) string {
	b, err := pyjson.EncodeWTF8(v)
	if err != nil {
		return tools.ErrorResult(err)
	}
	return string(b)
}

// Runs the batch inline and explains why it was not detached
func runSyncWithNote(ctx context.Context, batch *delegateBatch, note string) string {
	result := executeAndAggregate(ctx, batch, true)
	result.Set("note", note)
	return dumpsResult(result)
}

// Detached result target, empty for push, a resumable api id, or false for inline
func resolveAsyncWakeSID(env *tools.Env, originWakeSID string, historyDelivery, oneshotSession bool) (string, bool) {
	finite, known := false, false
	if tools.FiniteTurnRequired != nil {
		finite, known = tools.FiniteTurnRequired(env)
	}
	if oneshotSession || (known && finite) || (!known && tools.GetSessionEnv(env, "ICEBATH_SINGLE_QUERY_SESSION", "") == "1") {
		return "", false
	}
	supported := true
	func() {
		defer func() {
			if r := recover(); r != nil {
				if isUserInterruptPanic(r) {
					panic(r)
				}
				supported = true
			}
		}()
		supported = tools.SessionAsyncDeliverySupported(env)
	}()
	if supported {
		return "", true
	}
	if originWakeSID != "" && historyDelivery {
		delegateLog().Info(fmt.Sprintf("delegate_task: session %s resumes server history — detached result will be persisted "+
			"for the next client turn (no model wake).", originWakeSID))
		return originWakeSID, true
	}
	if originWakeSID != "" {
		delegateLog().Info(fmt.Sprintf("delegate_task: session %s has no declared server-history consumer — running the batch "+
			"synchronously so the result returns in this turn.", originWakeSID))
	}
	return "", false
}

// Session key and ui session the async registry routes completions by
func resolveAsyncSessionKey(env *tools.Env, parent *AIAgent, originUISessionID string) (string, string) {
	sessionKey := tools.CurrentSessionKeyOr(env, "")
	agentSessionID := parent.sessionID
	delegateQuiet("", func() {
		source := tools.GetSessionEnv(env, "ICEBATH_SESSION_SOURCE", "")
		if ui := tools.GetSessionEnv(env, "ICEBATH_UI_SESSION_ID", ""); ui != "" {
			originUISessionID = ui
		}
		if source == "tui" && agentSessionID != "" {
			sessionKey = agentSessionID
		}
	})
	if sessionKey == "" {
		sessionKey = agentSessionID
	}
	return sessionKey, originUISessionID
}

// Stale monitor token of every child's calls, tool and activity clock, true while any child is in a tool
func batchProgressToken(children []*AIAgent) (any, bool, error) {
	parts := make([]any, 0, len(children))
	inTool := false
	for _, c := range children {
		func() {
			defer func() {
				if r := recover(); r != nil {
					if isUserInterruptPanic(r) {
						panic(r)
					}
					parts = append(parts, nil)
				}
			}()
			s := c.GetActivitySummary()
			apiCalls, ok := s["api_call_count"]
			if !ok {
				apiCalls = 0
			}
			parts = append(parts, []any{apiCalls, s["current_tool"], s["last_activity_ts"]})
			inTool = inTool || pyrepr.Truthy(s["current_tool"])
		}()
	}
	return parts, inTool, nil
}

type dispatchedUnit struct {
	unit *delegateBatch
	id   string
}

// Model facing handle of an accepted background call, one entry per async unit
func dispatchedPayload(batch *delegateBatch, units []dispatchedUnit) *pyjson.Object {
	goals := make([]any, len(batch.TaskList))
	for i, t := range batch.TaskList {
		goals[i] = t["goal"]
	}
	n := len(batch.Children)
	id := batch.LiveDelegID
	if id == "" {
		id = units[0].id
	}
	note := backgroundNoteOne
	if n != 1 {
		note = fmt.Sprintf(backgroundNoteMany, n, len(units))
	}
	p := pyjson.NewObject()
	p.Set("status", "dispatched")
	p.Set("mode", "background")
	p.Set("count", n)
	p.Set("delegation_id", id)
	p.Set("goals", goals)
	p.Set("note", note)
	if len(units) > 1 {
		var list []any
		for _, u := range units {
			o := pyjson.NewObject()
			o.Set("delegation_id", u.id)
			if u.unit.Group != nil {
				o.Set("group", *u.unit.Group)
			} else {
				o.Set("group", nil)
			}
			var idx []any
			for _, c := range u.unit.Children {
				idx = append(idx, c.Index)
			}
			o.Set("task_indexes", idx)
			list = append(list, o)
		}
		p.Set("units", list)
	}
	sids := make([]any, len(batch.Children))
	anySID := false
	for i, c := range batch.Children {
		sids[i] = strOrNone(c.Child.subagentID)
		anySID = anySID || c.Child.subagentID != ""
	}
	if anySID {
		p.Set("subagent_ids", sids)
		p.Set("control_hint", backgroundControlHint)
	}
	if len(batch.Refused) > 0 {
		refused := make([]any, len(batch.Refused))
		for i, r := range batch.Refused {
			refused[i] = r
		}
		p.Set("refused", refused)
	}
	if len(batch.LivePaths) > 0 {
		paths := make([]any, len(batch.LivePaths))
		for i, s := range batch.LivePaths {
			paths[i] = s
		}
		p.Set("live_transcripts", paths)
		p.Set("live_transcripts_hint", backgroundLiveTranscriptsHint)
	}
	return p
}

// Async units of the call, one per task group and one per ungrouped task when independent completions are on
func unitsOf(ctx context.Context, batch *delegateBatch) []*delegateBatch {
	if !tools.IndependentCompletions(ctx) {
		return []*delegateBatch{batch}
	}
	type key struct {
		grouped bool
		name    string
		index   int
	}
	var order []key
	members := map[key][]delegateChild{}
	for _, c := range batch.Children {
		g := c.Task["group"]
		k := key{index: c.Index}
		if g != nil && g != "" {
			k = key{grouped: true, name: pyrepr.Str(g)}
		}
		if _, seen := members[k]; !seen {
			order = append(order, k)
		}
		members[k] = append(members[k], c)
	}
	units := make([]*delegateBatch, 0, len(order))
	for _, k := range order {
		u := *batch
		u.Children = members[k]
		u.Group = nil
		if k.grouped {
			name := k.name
			u.Group = &name
		}
		units = append(units, &u)
	}
	return units
}

// Hands one unit to the async registry whose runner joins only that unit's children
func dispatchUnit(ctx context.Context, unit *delegateBatch, unitID, slotKey, sessionKey, originUISessionID, wakeSID string) (map[string]any, error) {
	children := make([]*AIAgent, len(unit.Children))
	for i, c := range unit.Children {
		children[i] = c.Child
	}
	goals := make([]string, len(unit.TaskList))
	for i, t := range unit.TaskList {
		goals[i] = goalOf(t)
	}
	var contextText *string
	if s, ok := unit.Context.(string); ok {
		contextText = &s
	}
	var model *string
	if m := childStrArg(unit.Creds["model"]); m != "" {
		model = &m
	} else {
		for _, c := range children {
			if c.model != "" {
				m := c.model
				model = &m
				break
			}
		}
	}
	var taskIndexes []int
	if len(unit.Children) < len(unit.TaskList) {
		for _, c := range unit.Children {
			taskIndexes = append(taskIndexes, c.Index)
		}
	}
	transcripts := map[string]string{}
	for _, c := range unit.Children {
		if c.Index < len(unit.LiveWriters) && unit.LiveWriters[c.Index] != nil && unit.LiveWriters[c.Index].Path() != "" {
			transcripts[fmt.Sprint(c.Index)] = unit.LiveWriters[c.Index].Path()
		}
	}
	var parentSID *string
	if unit.Parent.sessionID != "" {
		s := unit.Parent.sessionID
		parentSID = &s
	}
	return tools.DispatchAsyncDelegationBatch(ctx, unit.Parent.env, tools.AsyncDispatch{
		Goals: goals, IsBatch: true, Context: contextText, Role: unit.TopRole, Model: model,
		Runner: func(rctx context.Context, _ *tools.Env) (map[string]any, error) {
			return objectMap(executeAndAggregate(rctx, unit, false)), nil
		},
		InterruptFn: func() error {
			for _, c := range children {
				signalChildStop(c, "Async delegation cancelled", "")
			}
			return nil
		},
		DelegationID: unitID, SlotKey: slotKey, TaskIndexes: taskIndexes, TaskTranscripts: transcripts,
		ProgressFn: func() (any, bool, error) { return batchProgressToken(children) },
		SessionKey: sessionKey, OriginUISessionID: originUISessionID, OriginSessionID: wakeSID,
		ParentSessionID: parentSID, MaxAsyncChildren: tools.MaxAsyncChildren(ctx),
	})
}

// Reattaches rejected children to the parent, replaying a stop that landed while detached
func restoreParentCancellation(unit *delegateBatch) {
	for _, c := range unit.Children {
		attachChild(unit.Parent, c.Child)
	}
}

// Dispatches the call as independent async units, running inline when detached delivery is impossible
func dispatchBackground(ctx context.Context, batch *delegateBatch) string {
	env := batch.Parent.env
	pinned, isPinned := batch.Parent.oneshotSession.Pinned()
	wakeSID, ok := resolveAsyncWakeSID(env, batch.Origin.WakeSID, batch.Origin.SessionHistoryDelivery, isPinned && pinned)
	if !ok {
		delegateLog().Info("delegate_task: async delivery unsupported on this session runtime; running the batch synchronously instead.")
		return runSyncWithNote(ctx, batch, syncNoteNoAsync)
	}
	parent := batch.Parent
	sessionKey, originUI := resolveAsyncSessionKey(env, parent, batch.Origin.UISessionID)
	var units []*delegateBatch
	for _, u := range unitsOf(ctx, batch) {
		cp := *u
		cp.Refused = nil
		units = append(units, &cp)
	}
	var dispatched []dispatchedUnit
	var inline []any
	slotKey := ""
	for k, unit := range units {
		unitID := batch.LiveDelegID
		if len(units) != 1 {
			unitID = ""
			if batch.LiveDelegID != "" {
				unitID = fmt.Sprintf("%s-%d", batch.LiveDelegID, k+1)
			}
		}
		if unitID == "" {
			unitID = tools.NewDelegationID()
		}
		unit.UnitID = unitID
		for _, c := range unit.Children {
			detachChild(parent, c.Child)
		}
		dispatch, err := dispatchUnit(ctx, unit, unitID, slotKey, sessionKey, originUI, wakeSID)
		if err == nil && dispatch["status"] == "dispatched" {
			id := pyrepr.Str(dispatch["delegation_id"])
			if slotKey == "" {
				slotKey = id
			}
			dispatched = append(dispatched, dispatchedUnit{unit, id})
			continue
		}
		reason := "rejected"
		if err != nil {
			reason = err.Error()
		} else if e, ok := dispatch["error"]; ok {
			reason = pyrepr.Str(e)
		}
		restoreParentCancellation(unit)
		if len(dispatched) == 0 {
			delegateLog().Info(fmt.Sprintf("delegate_task: async pool at capacity (%s); running the whole batch synchronously instead.", reason))
			return runSyncWithNote(ctx, batch, syncNoteAtCapacity)
		}
		shown := "None"
		if err != nil {
			shown = err.Error()
		} else if e, ok := dispatch["error"]; ok {
			shown = pyrepr.Str(e)
		}
		delegateLog().Warn(fmt.Sprintf("delegate_task: unit %d/%d not accepted (%s); running it inline.", k+1, len(units), shown))
		results, _ := entryValue(executeAndAggregate(ctx, unit, true), "results").([]any)
		inline = append(inline, results...)
	}
	payload := dispatchedPayload(batch, dispatched)
	if len(inline) > 0 {
		payload.Set("inline_results", inline)
	}
	return dumpsResult(payload)
}

// Tool result JSON of a delegate_task call, a dispatch handle in the background or the joined results
func runDelegateBatch(ctx context.Context, batch *delegateBatch, background bool) (string, error) {
	observability.BeginDelegationRun(ctx, batchCallKey(batch), len(batch.Children), batch.Parent.delegateDepth+1)
	if background && !sessionJoinsDelegation(batch.Parent) {
		return dispatchBackground(ctx, batch), nil
	}
	return dumpsResult(executeAndAggregate(ctx, batch, true)), nil
}
