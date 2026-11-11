package gateway

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"math/big"
	"strings"

	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Card button to the typed slash command and argument it stands for
var controlActionCommands = map[string][2]string{
	"goal.pause": {"goal", "pause"}, "goal.resume": {"goal", "resume"},
	"goal.clear": {"goal", "clear"}, "goal.unwait": {"goal", "unwait"},
	"loop.pause": {"loop", "pause"}, "loop.resume": {"loop", "resume"}, "loop.stop": {"loop", "stop"},
	"heartbeat.pause": {"heartbeat", "pause"}, "heartbeat.resume": {"heartbeat", "resume"},
	"heartbeat.clear": {"heartbeat", "clear"},
}

var controlSubgoalActions = map[string]bool{"subgoal.add": true, "subgoal.remove": true, "subgoal.clear": true}

var controlEnvelope = []string{"type", "output", "notice", "message", "display"}

// Runner surface that finds the goal manager of an event's session
type goalManagerRunner interface {
	GoalManagerForEvent(ctx context.Context, event *MessageEvent) (*goals.GoalManager, *SessionEntry)
}

// Refuses a malformed action or its arguments
func validateControlAction(action string, args any) error {
	obj, isDict := args.(*pyjson.Object)
	if !isDict || paramsOutside(obj, "text", "index") {
		return runtimeRefusal("invalid_params")
	}
	if action == "subgoal.remove" {
		switch index := obj.GetOr("index", nil).(type) {
		case int64:
			if index < 1 {
				return runtimeRefusal("invalid_params")
			}
		case *big.Int:
			if index.Sign() < 1 {
				return runtimeRefusal("invalid_params")
			}
		default:
			return runtimeRefusal("invalid_params")
		}
	}
	if action == "subgoal.add" {
		text, isStr := obj.GetOr("text", nil).(string)
		if !isStr || strings.TrimSpace(text) == "" {
			return runtimeRefusal("invalid_params")
		}
	}
	_, known := controlActionCommands[action]
	if !known && !controlSubgoalActions[action] {
		return runtimeRefusal("invalid_params")
	}
	return nil
}

// Criterion button on the session's goal manager, a missing goal or index refused for the card to show
func controlSubgoal(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object, action string, args *pyjson.Object) (*pyjson.Object, error) {
	a := c.authority
	if err := authorizeBusy(ctx, a, c.actor, ref, params, "session:control"); err != nil {
		return nil, err
	}
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return nil, err
	}
	if local, err := authorizeLocalSource(a.Runner, live.Source); err != nil {
		return nil, err
	} else if local == nil || !*local {
		return nil, runtimeRefusal("permission_denied")
	}
	if err := a.requireAdmissionOpen(false); err != nil {
		return nil, err
	}
	scoped := profileRuntimeScope(ctx, a.ProfileID, true)
	runner, ok := a.Runner.(goalManagerRunner)
	if !ok {
		return nil, attributeError("_get_goal_manager_for_event")
	}
	event := NewMessageEvent("/subgoal")
	event.Source = live.Source
	mgr, _ := runner.GoalManagerForEvent(scoped, event)
	if mgr == nil {
		detail := i18n.T(scoped, "gateway.goal.unavailable", "", nil)
		return nil, state.NewRuntimeStoreError("command_unavailable", nil, &detail)
	}
	if !mgr.HasGoal() {
		detail := i18n.T(scoped, "gateway.subgoal.no_goal", "", nil)
		return nil, state.NewRuntimeStoreError("goal_not_active", nil, &detail)
	}
	var output string
	switch action {
	case "subgoal.add":
		text, _ := args.GetOr("text", "").(string)
		added, err := mgr.AddSubgoal(strings.TrimSpace(text))
		if err != nil {
			return nil, subgoalRefusal(err)
		}
		output = i18n.T(scoped, "gateway.subgoal.added", "", map[string]any{"index": len(mgr.State().Subgoals), "text": added})
	case "subgoal.remove":
		raw := args.GetOr("index", nil)
		index, isSmall := raw.(int64)
		if !isSmall {
			index = math.MaxInt64
		}
		removed, err := mgr.RemoveSubgoal(index)
		if err != nil {
			return nil, subgoalRefusal(err)
		}
		output = i18n.T(scoped, "gateway.subgoal.removed", "", map[string]any{"index": raw, "text": removed})
	default:
		count, err := mgr.ClearSubgoals()
		if err != nil {
			return nil, subgoalRefusal(err)
		}
		switch {
		case count == 0:
			output = i18n.T(scoped, "gateway.subgoal.none_to_clear", "", nil)
		case count == 1:
			output = i18n.T(scoped, "gateway.subgoal.cleared_one", "", map[string]any{"count": count})
		default:
			output = i18n.T(scoped, "gateway.subgoal.cleared_other", "", map[string]any{"count": count})
		}
	}
	return pyjson.ObjectOf("type", "exec", "output", output), nil
}

// Index errors refused on the index field, runtime and value errors as invalid params, anything else raised
func subgoalRefusal(err error) error {
	var pyErr *pyint.Error
	if !errors.As(err, &pyErr) {
		return err
	}
	detail := pyErr.Msg
	switch pyErr.Kind {
	case "IndexError":
		field := "index"
		return state.NewRuntimeStoreError("invalid_params", &field, &detail)
	case "RuntimeError", "ValueError":
		return state.NewRuntimeStoreError("invalid_params", nil, &detail)
	}
	return err
}

// Pushes one live session's control snapshot to every viewer as session.control.update
func PublishControl(ctx context.Context, a *SessionAuthority, sessionID string, onlyIfPresent, onlyIfChanged bool) error {
	if a == nil {
		return nil
	}
	live, ok := a.live(sessionID)
	if !ok || live == nil {
		return nil
	}
	ticket := takeControlTicket(live)
	snapshot, err := readControlSnapshot(ctx, a, sessionID, false)
	if err != nil {
		return skippedControlPublish(sessionID, err)
	}
	publishControlSnapshot(live, sessionID, snapshot, ticket, onlyIfPresent, onlyIfChanged)
	return nil
}

// Publish with the snapshot read under the owner scope, dropped when a later ticket painted or the session was remounted
func PublishControlAsync(ctx context.Context, a *SessionAuthority, sessionID string, onlyIfPresent, onlyIfChanged bool) error {
	if a == nil {
		return nil
	}
	live, ok := a.live(sessionID)
	if !ok || live == nil {
		return nil
	}
	ticket := takeControlTicket(live)
	snapshot, err := readControlSnapshot(ctx, a, sessionID, true)
	if err != nil {
		return skippedControlPublish(sessionID, err)
	}
	if current, ok := a.live(sessionID); !ok || current != live {
		return nil
	}
	publishControlSnapshot(live, sessionID, snapshot, ticket, onlyIfPresent, onlyIfChanged)
	return nil
}

// Unreadable row skipped at debug, any other failure raised to the caller
func skippedControlPublish(sessionID string, err error) error {
	var refusal *state.RuntimeStoreError
	if errors.As(err, &refusal) {
		slog.Debug("session.control.update skipped for "+sessionID, "error", err)
		return nil
	}
	return err
}

// Takes the next control publish ticket of a live session
func takeControlTicket(live *LiveSession) int {
	live.ControlMu.Lock()
	defer live.ControlMu.Unlock()
	live.ControlSeq++
	return live.ControlSeq
}

// Control snapshot of one session, read under the owner profile scope when scoped
func readControlSnapshot(ctx context.Context, a *SessionAuthority, sessionID string, scoped bool) (*pyjson.Object, error) {
	ref := SessionRef{ProfileID: a.ProfileID, SessionID: sessionID}
	if scoped {
		ctx = ownerScope(ctx, a, false)
	}
	return controlSnapshot(ctx, a, ref)
}

// Paints the snapshot unless a newer ticket published or the revision gate holds it back
func publishControlSnapshot(live *LiveSession, sessionID string, snapshot *pyjson.Object, ticket int, onlyIfPresent, onlyIfChanged bool) {
	live.ControlMu.Lock()
	if live.ControlPublishedSeq > ticket {
		live.ControlMu.Unlock()
		return
	}
	revision := snapshot.GetOr("revision", nil)
	text := pyrepr.Str(revision)
	if (onlyIfPresent && !pyrepr.Truthy(revision)) || (onlyIfChanged && text == live.ControlRevision) {
		live.ControlMu.Unlock()
		return
	}
	live.ControlPublishedSeq = ticket
	live.ControlRevision = text
	live.ControlMu.Unlock()
	live.EventStream.Publish(sessionID, pyjson.ObjectOf("control", snapshot), "session.control.update", nil)
}

// Card button run as its typed command, answered with the owner's control snapshot and the command envelope
func control(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	if paramsOutside(params, "session_id", "profile", "action", "args") || ref.SessionID == "" {
		return nil, runtimeRefusal("invalid_params")
	}
	action, isStr := params.GetOr("action", nil).(string)
	if !isStr {
		return nil, runtimeRefusal("invalid_params")
	}
	var args any = pyjson.NewObject()
	if raw := params.GetOr("args", nil); raw != nil {
		args = raw
	}
	action = strings.TrimSpace(action)
	if err := validateControlAction(action, args); err != nil {
		return nil, err
	}
	if _, ok := c.authority.live(ref.SessionID); !ok {
		return nil, runtimeRefusal("not_found")
	}
	scope := pyjson.NewObject()
	for _, key := range []string{"session_id", "profile"} {
		if value, ok := params.Get(key); ok {
			scope.Set(key, value)
		}
	}
	var result *pyjson.Object
	if controlSubgoalActions[action] {
		var err error
		if result, err = controlSubgoal(ctx, c, ref, scope, action, args.(*pyjson.Object)); err != nil {
			return nil, err
		}
		if err := PublishControl(ctx, c.authority, ref.SessionID, false, false); err != nil {
			return nil, err
		}
	} else {
		command := controlActionCommands[action]
		request := scope.Copy()
		request.Set("name", command[0])
		request.Set("arg", command[1])
		var err error
		if result, err = executeCommand(ctx, c, ref, request, true); err != nil {
			return nil, err
		}
	}
	read, err := ancillaryRead(ctx, c, ref, scope, "control")
	if err != nil {
		return nil, err
	}
	snapshot, _ := read.(*pyjson.Object)
	out := pyjson.NewObject()
	if snapshot != nil {
		out = snapshot.Copy()
	}
	dispatch := pyjson.NewObject()
	for _, key := range controlEnvelope {
		dispatch.Set(key, result.GetOr(key, nil))
	}
	out.Set("dispatch", dispatch)
	return out, nil
}
