package gatewayrun

import (
	"context"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// MoA facade progress keys mapped onto the payload keys the TUI gateway emits
var localViewerMoaKeys = map[string][][2]string{
	"moa.reference":   {{"moa_index", "index"}, {"moa_count", "count"}},
	"moa.progress":    {{"moa_refs_done", "refs_done"}, {"moa_refs_total", "refs_total"}},
	"moa.phase":       {{"moa_phase", "phase"}, {"moa_refs_done", "refs_done"}, {"moa_refs_total", "refs_total"}},
	"moa.aggregating": {},
}

// Sinks a local viewer turn replaced and the desktop registration its restore releases
type localViewerPrevious struct {
	sinks agent.LocalViewerSinks
	token *desktopUIToken
}

// Installs this turn's local viewer sinks on the agent and returns what restore needs
func wireLocalViewerEvents(ctx context.Context, t *TurnRunner, a *agent.AIAgent) (*localViewerPrevious, error) {
	previous := &localViewerPrevious{sinks: a.LocalViewerSinks()}
	sinks := previous.sinks
	publish := t.publishExecution
	sinks.ToolGen = func(name string) { publish("tool.generating", map[string]any{"name": name}) }
	sinks.PrintFn = func(text string) { publish("agent.print", map[string]any{"text": text}) }
	sinks.Thinking = func(text string) { publish("thinking.delta", map[string]any{"text": text}) }
	sinks.Reaction = func(kind string) { publish("reaction", map[string]any{"kind": kind}) }
	if owner := t.approvalOwner; owner != nil {
		live, _ := owner.authority.Live(owner.sessionID)
		gateway.SeedCompressionCount(live, a)
	}
	shown, err := showLocalReasoning(ctx, t)
	if err != nil {
		return nil, err
	}
	if shown || gateway.ConfigDefaultLaunch(ctx) {
		sinks.Reasoning = func(text string) {
			show, err := showLocalReasoning(ctx, t)
			if err != nil {
				panic(err)
			}
			if show {
				publish("reasoning.delta", map[string]any{"text": text})
			}
		}
	}
	previous.token = wireLocalDesktopUI(t, a, &sinks)
	a.SetLocalViewerSinks(sinks)
	return previous, nil
}

// Session reasoning display flag, else the launch default of an Ink or Desktop launch
func showLocalReasoning(ctx context.Context, t *TurnRunner) (bool, error) {
	owner := t.approvalOwner
	if owner == nil {
		return false, nil
	}
	live, ok := owner.authority.Live(owner.sessionID)
	if !ok || live == nil {
		return false, nil
	}
	if live.ShowReasoning != nil {
		return *live.ShowReasoning, nil
	}
	return gateway.LaunchShowsReasoningActive(ctx, owner.authority)
}

// Whether a subagent event belongs to a delegate this turn's agent spawned
func isActiveSessionSubagent(t *TurnRunner, payload *pyjson.Object, eventType string) bool {
	subagentID, _ := payload.GetOr("subagent_id", nil).(string)
	if subagentID == "" {
		return false
	}
	turnAgent := t.ctx.currentAgent()
	if turnAgent == nil {
		return false
	}
	parentID, _ := payload.GetOr("parent_id", nil).(string)
	return agent.IsActiveSessionSubagent(turnAgent, subagentID, parentID, eventType == "subagent.spawn_requested")
}

// Publishes a subagent event in turn, or outside it for a background delegate this turn spawned
func publishLocalSubagent(t *TurnRunner, eventType string, payload *pyjson.Object) {
	plain, _ := pyjson.Plain(payload).(map[string]any)
	owner := t.approvalOwner
	if owner == nil {
		t.publishExecution(eventType, plain)
		return
	}
	live, ok := owner.authority.Live(owner.sessionID)
	var stream *gateway.SessionEvents
	if ok && live != nil {
		stream = live.EventStream
	}
	if stream != nil {
		stream.Lock.Lock()
		generation, executing := owner.authority.ExecutingGeneration(owner.sessionID)
		if executing && generation == owner.generation {
			t.publishExecution(eventType, plain)
			stream.Lock.Unlock()
			return
		}
		stream.Lock.Unlock()
	} else if generation, executing := owner.authority.ExecutingGeneration(owner.sessionID); executing && generation == owner.generation {
		t.publishExecution(eventType, plain)
		return
	}
	if stream != nil && isActiveSessionSubagent(t, payload, eventType) {
		stream.Publish(owner.sessionID, payload, eventType, pyjson.NewObject())
	}
}

// Subagent, tool progress, reasoning and MoA facade events of the agent progress callback for a local viewer
func publishLocalProgress(ctx context.Context, t *TurnRunner, eventType, name, preview string, kwargs map[string]any) error {
	if strings.HasPrefix(eventType, "subagent.") {
		if eventType == "subagent.text" {
			return nil
		}
		shown, err := showLocalReasoning(ctx, t)
		if err != nil {
			return err
		}
		payload, err := tuigateway.SubagentPayload(eventType, name, preview, kwargs, shown)
		if err != nil {
			return err
		}
		publishLocalSubagent(t, eventType, payload)
		return nil
	}
	if eventType == "tool.progress" {
		t.publishExecution(eventType, tools.ToolProgressPayload(name, preview, kwargs))
		return nil
	}
	moaKeys, isMoa := localViewerMoaKeys[eventType]
	if eventType != "reasoning.available" && !isMoa {
		return nil
	}
	shown, err := showLocalReasoning(ctx, t)
	if err != nil {
		return err
	}
	if !shown && eventType != "reasoning.available" {
		if shown, err = classicCLITurn(ctx, t); err != nil {
			return err
		}
	}
	if !shown {
		return nil
	}
	payload := pyjson.NewObject()
	switch eventType {
	case "reasoning.available":
		payload.Set("text", preview)
	case "moa.reference":
		payload.Set("label", name)
		payload.Set("text", preview)
	case "moa.aggregating", "moa.phase":
		if name != "" || eventType == "moa.aggregating" {
			payload.Set("aggregator", name)
		}
	default:
		payload.Set("label", name)
	}
	for _, pair := range moaKeys {
		if value := kwargs[pair[0]]; value != nil {
			payload.Set(pair[1], value)
		}
	}
	if eventType == "moa.progress" {
		_, hasDone := payload.Get("refs_done")
		_, hasTotal := payload.Get("refs_total")
		if !hasDone || !hasTotal {
			return nil
		}
	}
	if eventType == "moa.phase" && !pyrepr.Truthy(payload.GetOr("phase", nil)) {
		return nil
	}
	plain, _ := pyjson.Plain(payload).(map[string]any)
	t.publishExecution(eventType, plain)
	return nil
}

// Puts the sinks a local viewer turn replaced back and releases its desktop registration
func restoreLocalViewerEvents(a *agent.AIAgent, previous *localViewerPrevious) {
	if previous == nil {
		return
	}
	releaseLocalDesktopUI(previous.token)
	a.SetLocalViewerSinks(previous.sinks)
}

// Schedules a revision gated control card publish after a tool moved automation state mid turn
func publishLocalControlIfChanged(t *TurnRunner) {
	owner := t.approvalOwner
	t.schedule(func(ctx context.Context) (any, error) {
		return nil, gateway.PublishControlAsync(ctx, owner.authority, owner.sessionID, false, true)
	}, "session.control.update after a tool")
}

// Records the start time of a call and the before state of a write capable tool's local files
func noteLocalToolStart(t *TurnRunner, callID any, toolName *string, args any) {
	key := ""
	if pyrepr.Truthy(callID) {
		key = pyrepr.Str(callID)
	}
	t.localToolMu.Lock()
	if t.localToolStarts == nil {
		t.localToolStarts = map[string]float64{}
	}
	t.localToolStarts[key] = pythread.Monotonic()
	t.localToolMu.Unlock()
	if toolName == nil {
		return
	}
	argMap, _ := args.(map[string]any)
	if argMap == nil {
		argMap = map[string]any{}
	}
	sessionID := t.ctx.sessionID
	var snapshot *tools.LocalEditSnapshot
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug("Edit snapshot capture failed for "+*toolName, "error", rec)
				snapshot = nil
			}
		}()
		snapshot = tools.CaptureLocalEditSnapshot(t.toolsEnv(), *toolName, argMap, &sessionID)
	}()
	if snapshot != nil {
		t.localToolMu.Lock()
		if t.localEditSnapshots == nil {
			t.localEditSnapshots = map[string]*tools.LocalEditSnapshot{}
		}
		t.localEditSnapshots[key] = snapshot
		t.localToolMu.Unlock()
	}
}

// Duration, rendered inline diff and task list snapshot of a completed local viewer tool call
func localCompleteFields(ctx context.Context, t *TurnRunner, callID any, toolName *string, args, result any) *pyjson.Object {
	key := ""
	if pyrepr.Truthy(callID) {
		key = pyrepr.Str(callID)
	}
	t.localToolMu.Lock()
	started, ok := t.localToolStarts[key]
	delete(t.localToolStarts, key)
	t.localToolMu.Unlock()
	fields := pyjson.NewObject()
	if ok {
		fields.Set("duration_s", pythread.Monotonic()-started)
	} else {
		fields.Set("duration_s", nil)
	}
	if toolName == nil {
		return fields
	}
	t.localToolMu.Lock()
	snapshot := t.localEditSnapshots[key]
	delete(t.localEditSnapshots, key)
	t.localToolMu.Unlock()
	argMap, _ := args.(map[string]any)
	if argMap == nil {
		argMap = map[string]any{}
	}
	var rendered []string
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug("Edit diff preview failed for "+*toolName, "error", rec)
				rendered = nil
			}
		}()
		resultText, _ := result.(string)
		tools.RenderEditDiffWithDelta(ctx, *toolName, resultText, argMap, snapshot, func(line string) { rendered = append(rendered, line) })
	}()
	if len(rendered) > 0 {
		fields.Set("inline_diff", strings.Join(rendered, "\n"))
	} else {
		fields.Set("inline_diff", nil)
	}
	if todo := localTodoState(*toolName, result); todo != nil {
		for _, k := range todo.Keys {
			fields.Set(k, todo.Vals[k])
		}
	}
	return fields
}

// Client safe task items and revision snapshot a task list tool result carries, nil otherwise
func localTodoState(toolName string, result any) *pyjson.Object {
	if !tools.IsTodoToolName(toolName) {
		return nil
	}
	value := result
	if text, ok := result.(string); ok {
		parsed, err := pyjson.Loads(text)
		if err != nil {
			return nil
		}
		value = parsed
	}
	return tuigateway.NormalizeTodoState(value)
}

// Whether a local viewer turn runs on a classic CLI surface session whose viewer reads the raw result
func classicCLITurn(ctx context.Context, t *TurnRunner) (bool, error) {
	if !localViewerTurn(t) {
		return false, nil
	}
	policy, err := gateway.LocalPolicyForSource(ctx, t.runner, t.ctx.source)
	if err != nil {
		var storeErr *state.RuntimeStoreError
		if errors.As(err, &storeErr) {
			return false, nil
		}
		return false, err
	}
	return policy != nil && policy.Platform == "cli", nil
}

// Whether a local viewer turn's submitter declared it answers the clarify batch in one panel
func clarifyBatchTurn(ctx context.Context, t *TurnRunner) bool {
	if !localViewerTurn(t) {
		return false
	}
	var inbound any
	if t.ctx.inboundMessageID != nil {
		inbound = *t.ctx.inboundMessageID
	}
	return pyrepr.Truthy(gateway.CLITurn(ctx, inbound).GetOr("clarify_batch", nil))
}
