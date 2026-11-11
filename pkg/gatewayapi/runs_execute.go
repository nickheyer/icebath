package gatewayapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Approval bridge redacting, stamping the envelope, parking the run status and queueing the event
func (a *APIServerAdapter) makeApprovalNotify(ctx context.Context, run *runLaunch) tools.GatewayNotifyFunc {
	return func(approvalData map[string]any) error {
		data, _ := pyjson.AsObject(approvalData)
		event := approvalRequestEvent(ctx, run.runID, data)
		a.setRunStatus(ctx, run.runID, "waiting_for_approval", "last_event", "approval.request", "approval", event)
		func() {
			defer func() { _ = recover() }()
			run.queue.putNowait(event)
		}()
		return nil
	}
}

// Field pairs of an object in order
func objectPairs(o *pyjson.Object) []any {
	if o == nil {
		return nil
	}
	out := make([]any, 0, 2*len(o.Keys))
	for _, k := range o.Keys {
		v, _ := o.Get(k)
		out = append(out, k, v)
	}
	return out
}

// Drives a run a live Bot Chat owner executes from its mailbox receipt
func (a *APIServerAdapter) executeRunViaLiveOwner(ctx context.Context, run *runLaunch, profileHome string, record map[string]any) {
	runID := run.runID
	deliveryID := record["delivery_id"]
	finish := func(status string, fields ...any) {
		a.mu.Lock()
		interrupted := a.shutdownInterruptedRunIDs[runID]
		a.mu.Unlock()
		if interrupted {
			status, fields = "interrupted", []any{"error", "Gateway shutdown interrupted the run."}
		}
		a.setRunStatus(ctx, runID, status, append(append([]any(nil), fields...), "last_event", "run."+status)...)
		func() {
			defer func() { _ = recover() }()
			run.putEvent(runEvent(runID, "run."+status, fields...))
		}()
	}
	defer func() {
		func() {
			defer func() { _ = recover() }()
			run.putEvent(nil)
		}()
		a.mu.Lock()
		a.retireLiveRunLocked(runID)
		a.mu.Unlock()
	}()
	a.setRunStatus(ctx, runID, "running", "delivery_id", deliveryID)
	shouldStop := func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.stoppingRunIDs[runID]
	}
	next, err := awaitLiveDeliveryUntil(ctx, a, profileHome, record, nil, shouldStop)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			finish("cancelled", "completed", false, "partial", false, "interrupted", true)
			return
		}
		apiLog().Error(fmt.Sprintf("[api_server] run %s (live Bot Chat) failed", runID), "error", err)
		finish("failed", "completed", false, "partial", false, "interrupted", false, "error", err.Error())
		return
	}
	switch status := pyrepr.Str(next["status"]); status {
	case "queued", "claimed", "cancelled":
		finish("cancelled", "completed", false, "partial", false, "interrupted", true)
	case "settled":
		finish("completed", "completed", true, "partial", false, "interrupted", false, "output", pyrepr.Or(next["reply"], ""),
			"usage", pyjson.NewObject())
	default:
		fields := []any{"completed", false, "partial", false, "interrupted", false,
			"error", pyrepr.Or(next["error"], "Bot Chat delivery "+status)}
		if pyrepr.Truthy(next["reason"]) {
			fields = append(fields, "reason", next["reason"])
		}
		finish("failed", fields...)
	}
}

// Drives one admitted run, publishes its terminal event and status and releases live state
func (a *APIServerAdapter) executeRun(ctx context.Context, run *runLaunch) {
	runID := run.runID
	started := time.Now()
	streaming := func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, ok := a.runStreams[runID]
		return ok
	}
	textCB := func(delta string) {
		if !streaming() {
			return
		}
		run.putEvent(runEvent(runID, "message.delta", "delta", delta))
	}
	interimCB := func(text string, alreadyStreamed bool) {
		if strings.TrimSpace(text) == "" || !streaming() {
			return
		}
		run.putEvent(runEvent(runID, "message.interim", "text", text, "already_streamed", alreadyStreamed))
	}
	finish := func(status string, extra *pyjson.Object, fields ...any) {
		a.mu.Lock()
		interrupted := a.shutdownInterruptedRunIDs[runID]
		a.mu.Unlock()
		if interrupted {
			status, fields, extra = "interrupted", []any{"error", "Gateway shutdown interrupted the run."}, nil
		}
		statusFields := append(append(append([]any(nil), fields...), "last_event", "run."+status), objectPairs(extra)...)
		a.setRunStatus(ctx, runID, status, statusFields...)
		func() {
			defer func() { _ = recover() }()
			run.putEvent(runEvent(runID, "run."+status, append(append([]any(nil), fields...), objectPairs(extra)...)...))
		}()
	}
	defer func() {
		unregisterApprovalNotify(run.approvalSessionKey())
		func() {
			defer func() { _ = recover() }()
			run.putEvent(nil)
		}()
		a.mu.Lock()
		a.retireLiveRunLocked(runID)
		a.mu.Unlock()
	}()
	a.mu.Lock()
	interrupted, stopping := a.shutdownInterruptedRunIDs[runID], a.stoppingRunIDs[runID]
	a.mu.Unlock()
	if interrupted {
		finish("interrupted", nil)
		return
	}
	a.setRunStatus(ctx, runID, "running")
	if stopping {
		finish("cancelled", nil)
		return
	}
	var result map[string]any
	var usage, served *pyjson.Object
	err := func() error {
		if run.admission != nil {
			toolCB := a.makeRunEventCallback(ctx, runID)
			env := &tools.Env{}
			controlCB := func(eventType string, prompt *pyjson.Object) {
				func() {
					defer func() { _ = recover() }()
					run.putEvent(runEvent(runID, eventType, objectPairs(prompt)...))
				}()
			}
			toolStart := func(callID, name string, args map[string]any) {
				if args == nil {
					args = map[string]any{}
				}
				toolCB("tool.started", name, tools.BuildToolPreview(env, name, args, nil), args, map[string]any{"tool_call_id": callID})
			}
			toolComplete := func(callID, name string, args map[string]any, result string) {
				isErr, _ := tools.DetectToolFailure(ctx, name, result)
				toolCB("tool.completed", name, "", args, map[string]any{"tool_call_id": callID, "is_error": isErr})
			}
			res, use, err := observeAPIRun(ctx, run.admission, controlCB, textCB, toolStart, toolComplete)
			if err != nil {
				return err
			}
			result = res
			usage, _ = pyjson.AsObject(use)
			if rt, ok := use["runtime"].(*pyjson.Object); ok && rt.Len() > 0 {
				served = rt
			} else if rt, ok := res["runtime"].(*pyjson.Object); ok && rt.Len() > 0 {
				served = rt
			} else {
				served = pyjson.NewObject()
			}
			return nil
		}
		scoped, err := profileScope(withRequestProfile(ctx, run.requestProfile), run.requestProfile)
		if err != nil {
			return err
		}
		env := runEnv(scoped)
		opts := run.agentOptions
		opts.streamDeltaCallback = textCB
		opts.toolProgressCallback = a.makeRunEventCallback(ctx, runID)
		opts.interimAssistantCb = interimCB
		opts.env = env
		ag, err := a.createAgent(scoped, opts)
		if err != nil {
			return err
		}
		a.mu.Lock()
		a.setActiveRunAgentLocked(runID, ag)
		a.mu.Unlock()
		notify := a.makeApprovalNotify(ctx, run)
		type syncOutcome struct {
			result         map[string]any
			usage, runtime *pyjson.Object
		}
		res := <-submitAPIWorker(func() (syncOutcome, error) {
			r, u, s, err := a.runAgentSync(scoped, run, ag, env, notify)
			return syncOutcome{r, u, s}, err
		})
		if res.Panic != nil {
			panic(res.Panic)
		}
		if res.Err != nil {
			return res.Err
		}
		result, usage, served = res.Value.result, res.Value.usage, res.Value.runtime
		return nil
	}()
	if err != nil {
		var authErr *providerAuthResolutionError
		var runtimeErr *state.RuntimeStoreError
		switch {
		case errors.Is(err, context.Canceled):
			finish("cancelled", nil)
		case errors.As(err, &authErr):
			apiLog().Warn(fmt.Sprintf("Provider resolution failed for run=%s: %v", runID, authErr))
			finish("failed", nil, "error", authErr.userText(ctx))
		case errors.As(err, &runtimeErr) && runtimeErr.Reason == "admission_queued":
			a.setRunStatus(ctx, runID, "queued", "last_event", "run.queued")
			run.putEvent(runEvent(runID, "run.queued", "message", err.Error()))
		default:
			apiLog().Error(fmt.Sprintf("[api_server] run %s failed", runID), "error", err)
			finish("failed", nil, "error", redactAPIErrorText(err.Error(), -1))
		}
		return
	}
	a.recordAPIMetrics(ctx, usageMap(usage), since(started))
	if result == nil {
		result = map[string]any{}
	}
	status, fields := terminalRunStatus(result)
	switch {
	case status == "cancelled":
		finish("cancelled", fields)
	case pyrepr.Truthy(result["failed"]):
		finish("failed", fields, "error", redactAPIErrorText(firstTruthy(result["error"], "agent run failed"), -1))
	default:
		requested := pyjson.ObjectOf("provider", nullableString(run.agentOptions.requestedProvider), "model", nullableString(run.agentOptions.requestedModel))
		anyRequested := run.agentOptions.requestedProvider != "" || run.agentOptions.requestedModel != ""
		var requestedRuntime *pyjson.Object
		source := "global"
		if anyRequested {
			requestedRuntime = requested
			source = "raw_request"
		}
		if run.agentOptions.route != nil {
			source = "model_routes"
		}
		runtime := sanitizeRuntimeMetadata(served, requestedRuntime, source, "")
		finish(status, fields, "output", pyrepr.Or(result["final_response"], ""), "usage", usage, "runtime", runtime)
	}
}

// Usage object as the metrics map
func usageMap(usage *pyjson.Object) map[string]any {
	if usage == nil {
		return nil
	}
	return usage.Vals
}
