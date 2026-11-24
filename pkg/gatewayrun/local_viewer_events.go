package gatewayrun

import (
	"context"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

// MoA facade progress keys mapped onto the payload keys the TUI gateway emits
var localViewerMoaKeys = map[string][][2]string{
	"moa.reference":   {{"moa_index", "index"}, {"moa_count", "count"}},
	"moa.progress":    {{"moa_refs_done", "refs_done"}, {"moa_refs_total", "refs_total"}},
	"moa.phase":       {{"moa_phase", "phase"}, {"moa_refs_done", "refs_done"}, {"moa_refs_total", "refs_total"}},
	"moa.aggregating": {},
}

// Sinks a local viewer turn replaced, put back by its restore
type localViewerPrevious struct {
	sinks agent.LocalViewerSinks
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
	wireLocalConnections(t, &sinks)
	a.SetLocalViewerSinks(sinks)
	return previous, nil
}

// Session reasoning display flag, else the launch default of a TUI launch
func showLocalReasoning(ctx context.Context, t *TurnRunner) (bool, error) {
	owner := t.approvalOwner
	if owner == nil {
		return false, nil
	}
	live, ok := owner.authority.Live(owner.sessionID)
	if !ok || live == nil {
		return false, nil
	}
	if shown, ok := gateway.SessionOwnSetting(ctx, live, settings.ShowThinking.Key); ok {
		on, _ := shown.(bool)
		return on, nil
	}
	return gateway.LaunchShowsReasoningActive(ctx), nil
}

// Whether a subagent event belongs to a delegate this turn's agent spawned
func isActiveSessionSubagent(t *TurnRunner, payload *jsonutil.Object, eventType string) bool {
	subagentID, _ := payload.ValueOr("subagent_id", nil).(string)
	if subagentID == "" {
		return false
	}
	turnAgent := t.ctx.currentAgent()
	if turnAgent == nil {
		return false
	}
	parentID, _ := payload.ValueOr("parent_id", nil).(string)
	return agent.IsActiveSessionSubagent(turnAgent, subagentID, parentID, eventType == "subagent.spawn_requested")
}

// Publishes a subagent event in turn, or outside it for a background delegate this turn spawned
func publishLocalSubagent(t *TurnRunner, eventType string, payload *jsonutil.Object) {
	plain, _ := jsonutil.Plain(payload).(map[string]any)
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
		stream.Publish(owner.sessionID, payload, eventType, jsonutil.NewObject())
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
		payload, err := tuihost.SubagentPayload(eventType, name, preview, kwargs, shown)
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
	if eventType == "tool.output_risk" {
		if payload := tuihost.OutputRiskPayload(name, kwargs); payload != nil {
			t.publishExecution(eventType, payload)
		}
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
	payload := jsonutil.NewObject()
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
	if eventType == "moa.phase" && !anyutil.IsTrue(payload.ValueOr("phase", nil)) {
		return nil
	}
	plain, _ := jsonutil.Plain(payload).(map[string]any)
	t.publishExecution(eventType, plain)
	return nil
}

// Puts the sinks a local viewer turn replaced back
func restoreLocalViewerEvents(a *agent.AIAgent, previous *localViewerPrevious) {
	if previous == nil {
		return
	}
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
	if anyutil.IsTrue(callID) {
		key = anyutil.String(callID)
	}
	t.localToolMu.Lock()
	if t.localToolStarts == nil {
		t.localToolStarts = map[string]float64{}
	}
	t.localToolStarts[key] = timeutil.Monotonic()
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
				pkgLog().Debug("Edit snapshot capture failed for "+*toolName, "error", rec)
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

// Display metadata stored with a local viewer tool result, its duration, inline diff and image, the fields kept for its tool.complete so the
// stored row and the live row read the same
func (t *TurnRunner) localToolResultMetadata(callID, name string, args map[string]any, result string) map[string]any {
	fields := localCompleteFields(t.turnCtx, t, callID, &name, args, result)
	t.localToolMu.Lock()
	if t.localToolFields == nil {
		t.localToolFields = map[string]*jsonutil.Object{}
	}
	t.localToolFields[callID] = fields
	t.localToolMu.Unlock()
	stored := map[string]any{}
	for _, key := range []string{"duration_s", "inline_diff", "image_path"} {
		if value := fields.Value(key); value != nil {
			stored[key] = value
		}
	}
	return stored
}

// Completion fields of a local viewer tool call, the ones worked out when its result was stored, else worked out now for a call that stored none
func (t *TurnRunner) takeLocalCompleteFields(callID string, toolName *string, args, result any) *jsonutil.Object {
	t.localToolMu.Lock()
	fields, kept := t.localToolFields[callID]
	delete(t.localToolFields, callID)
	t.localToolMu.Unlock()
	if kept {
		return fields
	}
	return localCompleteFields(t.turnCtx, t, callID, toolName, args, result)
}

// Duration, rendered inline diff and task list snapshot of a completed local viewer tool call
func localCompleteFields(ctx context.Context, t *TurnRunner, callID any, toolName *string, args, result any) *jsonutil.Object {
	key := ""
	if anyutil.IsTrue(callID) {
		key = anyutil.String(callID)
	}
	t.localToolMu.Lock()
	started, ok := t.localToolStarts[key]
	delete(t.localToolStarts, key)
	t.localToolMu.Unlock()
	fields := jsonutil.NewObject()
	if ok {
		fields.Set("duration_s", timeutil.Monotonic()-started)
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
				pkgLog().Debug("Edit diff preview failed for "+*toolName, "error", rec)
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
	failed, _, _ := tools.ToolOutcome(ctx, *toolName, result)
	if path := tuigateway.ToolImagePath(*toolName, argMap, result, failed); path != "" {
		fields.Set("image_path", path)
	} else {
		fields.Set("image_path", nil)
	}
	if todo := localTodoState(*toolName, result); todo != nil {
		for _, k := range todo.Keys() {
			fields.Set(k, todo.Value(k))
		}
	}
	return fields
}

// Client safe task items and revision snapshot a task list tool result carries, nil otherwise
func localTodoState(toolName string, result any) *jsonutil.Object {
	if !tools.IsTodoToolName(toolName) {
		return nil
	}
	value := result
	if text, ok := result.(string); ok {
		parsed, err := jsonutil.Decode(text)
		if err != nil {
			return nil
		}
		value = parsed
	}
	return tuihost.NormalizeTodoState(value)
}

// Whether a local viewer turn runs on a cli surface session whose viewer reads the raw result
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
	if _, inboundID, _ := t.runner.turnReplyAnchors(t.ctx); inboundID != nil {
		inbound = *inboundID
	}
	return anyutil.IsTrue(gateway.CLITurn(ctx, inbound).ValueOr("clarify_batch", nil))
}
