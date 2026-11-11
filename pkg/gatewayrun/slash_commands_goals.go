package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Python class name of an error, the kind of a pyint error first
func pyErrorClass(err error) string {
	var pe *pyint.Error
	if errors.As(err, &pe) && pe.Kind != "" {
		return pe.Kind
	}
	return pyrepr.ExceptionName(err)
}

// Manager error reply prefixed for the command when the error class is one the command answers, else the error itself
func mgrCallError(prefix string, err error, classes ...string) (string, error) {
	if slices.Contains(classes, pyErrorClass(err)) {
		return prefix + ": " + pyrepr.ExceptionMessage(err), nil
	}
	return "", err
}

// Applies one goal command to the session's goal manager, enqueuing any prompt it starts
func (r *GatewayRunner) handleGoalCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	mgr, _ := r.getGoalManagerForEvent(ctx, event)
	if mgr == nil {
		return i18n.T(ctx, "gateway.goal.unavailable", "", nil), nil
	}
	authorizeGate := func() string {
		if event.Source != nil {
			if local, err := gateway.AuthorizeLocalSource(r, event.Source); err == nil && local != nil && *local {
				return ""
			}
		}
		if !r.resumeCallerIsAdmin(event.Source) {
			return i18n.T(ctx, "gateway.goal.gate_add_admin_only", "", nil)
		}
		return ""
	}
	value, err := r.runInExecutorWithContext(ctx, func(workerCtx context.Context) (any, error) {
		return goals.DispatchGoalCommand(mgr, event.GetCommandArgs(), goals.GoalCommandOptions{AuthorizeGate: authorizeGate,
			Render:          func(key, _ string, values map[string]any) string { return i18n.T(workerCtx, key, "", values) },
			LastUserMessage: goals.LastUserMessageFromDB(workerCtx, mgr.SessionID)})
	})
	if err != nil {
		return "", err
	}
	result := value.(goals.GoalCommandResult)
	if result.ClearPending != "" {
		r.clearGoalContinuations(ctx, event, result.ClearPending)
	}
	if result.Prompt != nil && *result.Prompt != "" {
		r.enqueueGoalTurn(ctx, event, *result.Prompt, "command enqueue", result.Kickoff)
	}
	return result.Output, nil
}

// Drops the goal continuations pending in the chat's FIFO
func (r *GatewayRunner) clearGoalContinuations(ctx context.Context, event *gateway.MessageEvent, verb string) {
	adapter, quickKey, err := r.adapterAndKeyFor(ctx, event)
	if err != nil {
		runLog().Debug(fmt.Sprintf("goal %s: pending continuation cleanup failed: %s", verb, pyrepr.ExceptionMessage(err)))
		return
	}
	if adapter != nil && quickKey != "" {
		r.clearGoalPendingContinuations(quickKey, adapter)
	}
}

// Enqueues text as the next turn through the adapter FIFO, a local command sink receiving it instead
func (r *GatewayRunner) enqueueGoalTurn(ctx context.Context, event *gateway.MessageEvent, text, label string, kickoff bool) {
	if sink := gateway.ActiveSink(ctx); sink != nil {
		sink.Prompt = &text
		if !kickoff {
			display := "/goal resume"
			sink.Display = &display
		}
		return
	}
	adapter, quickKey, err := r.adapterAndKeyFor(ctx, event)
	if err != nil {
		runLog().Debug(fmt.Sprintf("goal %s failed: %s", label, pyrepr.ExceptionMessage(err)))
		return
	}
	if text == "" || adapter == nil || quickKey == "" {
		return
	}
	turn := gateway.NewMessageEvent(text)
	turn.Source = event.Source
	if kickoff {
		turn.MessageID, turn.ChannelPrompt, turn.AutoSkill = event.MessageID, event.ChannelPrompt, event.AutoSkill
	}
	turn.InheritChannelPin = !kickoff
	r.enqueueFIFO(quickKey, turn, adapter)
}

// Shows, pauses, resumes, clears or sets the session's one recurring re-entry prompt
func (r *GatewayRunner) handleHeartbeatCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	args := pystr.Strip(event.GetCommandArgs())
	lower := pystr.Lower(args)
	mgr, _ := r.getHeartbeatManagerForEvent(ctx, event)
	if mgr == nil {
		return i18n.T(ctx, "gateway.heartbeat.unavailable", "", nil), nil
	}
	quickKey := ""
	if event.Source != nil {
		key, err := r.sessionKeyForSource(ctx, event.Source)
		if err != nil {
			return "", err
		}
		quickKey = key
	}
	watch := func() {
		if quickKey != "" && event.Source != nil {
			r.registerHeartbeatWatch(ctx, quickKey, event.Source, mgr.SessionID)
		}
	}
	switch lower {
	case "", "status":
		return mgr.StatusLine(), nil
	case "pause":
		if st := mgr.Pause(); st != nil {
			return i18n.T(ctx, "gateway.heartbeat.paused", "", map[string]any{"prompt": st.Prompt}), nil
		}
		return i18n.T(ctx, "gateway.heartbeat.none_set", "", nil), nil
	case "resume":
		st := mgr.Resume()
		if st == nil {
			return i18n.T(ctx, "gateway.heartbeat.no_resume", "", nil), nil
		}
		watch()
		return i18n.T(ctx, "gateway.heartbeat.resumed", "", map[string]any{"interval": agent.FormatHeartbeatInterval(st.IntervalSeconds), "prompt": st.Prompt}), nil
	case "clear", "stop", "off":
		had := mgr.Clear()
		if quickKey != "" {
			r.unregisterHeartbeatWatch(quickKey)
		}
		if had {
			return i18n.T(ctx, "gateway.heartbeat.cleared", "", nil), nil
		}
		return i18n.T(ctx, "gateway.heartbeat.none_set", "", nil), nil
	}
	tokens := pystr.SplitWhitespace(args, 2)
	var interval int64
	parsed, prompt := false, ""
	if pystr.Lower(tokens[0]) == "every" && len(tokens) >= 2 {
		interval, parsed = agent.ParseHeartbeatInterval("every " + tokens[1])
		if len(tokens) > 2 {
			prompt = tokens[2]
		}
	} else {
		interval, parsed = agent.ParseHeartbeatInterval(tokens[0])
		if parsed && interval > 0 {
			prompt = pystr.Strip(args[len(tokens[0]):])
		}
	}
	if !parsed {
		return i18n.T(ctx, "gateway.heartbeat.usage", "", nil), nil
	}
	if interval < 0 {
		return i18n.T(ctx, "gateway.heartbeat.interval_too_small", "", map[string]any{"min_seconds": agent.MinHeartbeatIntervalSeconds}), nil
	}
	if pystr.Strip(prompt) == "" {
		return i18n.T(ctx, "gateway.heartbeat.prompt_required", "", nil), nil
	}
	st, err := mgr.Set(prompt, interval)
	if err != nil {
		return mgrCallError(i18n.T(ctx, "gateway.heartbeat.invalid_prefix", "", nil), err, "ValueError")
	}
	watch()
	return i18n.T(ctx, "gateway.heartbeat.set", "", map[string]any{"interval": agent.FormatHeartbeatInterval(st.IntervalSeconds), "prompt": st.Prompt}), nil
}

// Adds, removes, clears or lists the extra criteria of the active goal
func (r *GatewayRunner) handleSubgoalCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	args := pystr.Strip(event.GetCommandArgs())
	mgr, _ := r.getGoalManagerForEvent(ctx, event)
	if mgr == nil {
		return i18n.T(ctx, "gateway.goal.unavailable", "", nil), nil
	}
	if !mgr.HasGoal() {
		return i18n.T(ctx, "gateway.subgoal.no_goal", "", nil), nil
	}
	if args == "" {
		return mgr.StatusLine() + "\n" + mgr.RenderSubgoals(), nil
	}
	tokens := pystr.SplitWhitespace(args, 1)
	verb := pystr.Lower(tokens[0])
	rest := ""
	if len(tokens) > 1 {
		rest = pystr.Strip(tokens[1])
	}
	switch verb {
	case "remove":
		if rest == "" {
			return i18n.T(ctx, "gateway.subgoal.usage_remove", "", nil), nil
		}
		idx, ok := pystr.IntText(pystr.SplitWhitespace(rest, -1)[0])
		if !ok {
			return i18n.T(ctx, "gateway.subgoal.remove_not_int", "", nil), nil
		}
		removed, err := mgr.RemoveSubgoal(idx.Int64())
		if err != nil {
			return mgrCallError("/subgoal remove", err, "IndexError", "RuntimeError")
		}
		return i18n.T(ctx, "gateway.subgoal.removed", "", map[string]any{"index": idx, "text": removed}), nil
	case "clear":
		prev, err := mgr.ClearSubgoals()
		if err != nil {
			return mgrCallError("/subgoal clear", err, "RuntimeError")
		}
		if prev == 0 {
			return i18n.T(ctx, "gateway.subgoal.none_to_clear", "", nil), nil
		}
		key := "gateway.subgoal.cleared_other"
		if prev == 1 {
			key = "gateway.subgoal.cleared_one"
		}
		return i18n.T(ctx, key, "", map[string]any{"count": prev}), nil
	}
	text, err := mgr.AddSubgoal(args)
	if err != nil {
		return mgrCallError("/subgoal", err, "RuntimeError", "ValueError")
	}
	idx := 0
	if st := mgr.State(); st != nil {
		idx = len(st.Subgoals)
	}
	return i18n.T(ctx, "gateway.subgoal.added", "", map[string]any{"index": idx, "text": text}), nil
}

// Recurring in session wakeups through the shared loop command dispatcher
func (r *GatewayRunner) handleLoopCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	r.warmGoalsSessionDB(ctx, "loop manager")
	sid := ""
	if entry, err := r.slashStore().GetOrCreateSession(ctx, event.Source, false, true); err == nil && entry != nil {
		sid = entry.SessionID
	}
	if sid == "" {
		return i18n.T(ctx, "gateway.loop.no_session", "", nil), nil
	}
	mgr := agent.NewLoopManager(ctx, sid)
	route := pyjson.NewObject()
	if src := event.Source; src != nil {
		for _, kv := range [][2]string{{"platform", string(src.Platform)}, {"chat_id", src.ChatID}, {"chat_type", src.ChatType},
			{"thread_id", sattr(src.ThreadID)}, {"user_id", sattr(src.UserID)}, {"user_name", sattr(src.UserName)}, {"profile", sattr(src.Profile)}} {
			if kv[1] != "" {
				route.Set(kv[0], kv[1])
			}
		}
	}
	result, err := agent.DispatchLoopCommand(ctx, mgr, pystr.Strip(event.GetCommandArgs()), route)
	if err != nil {
		return "", err
	}
	output := result.Output
	if result.Created && agent.GoalBlocksLoopTick(ctx, sid) {
		output += i18n.T(ctx, "gateway.loop.goal_defers_note", "", nil)
	}
	return output, nil
}
