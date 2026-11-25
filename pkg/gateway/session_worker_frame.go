package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Decoded kanban context of a bootstrap frame's policy, nil for a chat turn
func WorkerKanbanContext(frame *jsonutil.Object) (any, error) {
	policy, _ := frame.ValueOr("policy", nil).(*jsonutil.Object)
	raw := policy.ValueOr("kanban_json", nil)
	if !anyutil.IsTrue(raw) {
		raw = "null"
	}
	text, isText := raw.(string)
	if !isText {
		return nil, &errorutil.Error{Kind: "TypeError", Msg: "the worker kanban context is " + errchain.Kind(raw) + ", expected JSON text"}
	}
	return jsonutil.Decode(text)
}

// Runs body in the session workspace with its session vars bound, the session id as task id when scoped
func WorkerWorkspace(ctx context.Context, env *tools.Env, frame *jsonutil.Object, body func(taskID string) error) error {
	rawScope := frame.ValueOr("scope", nil)
	scope, isDict := rawScope.(*jsonutil.Object)
	if anyutil.IsTrue(rawScope) && !isDict {
		return &errorutil.Error{Kind: "AttributeError", Msg: "the worker workspace scope is " + errchain.Kind(rawScope) + ", expected a mapping"}
	}
	sessionID := ""
	if scope != nil && anyutil.IsTrue(scope.ValueOr("session_id", nil)) {
		sessionID = anyutil.String(scope.ValueOr("session_id", nil))
	}
	if sessionID == "" {
		return body("")
	}
	policy, err := RestorePolicy(frame.ValueOr("policy", nil))
	if err != nil {
		return err
	}
	route := sessionID
	if raw := frame.ValueOr("route", nil); anyutil.IsTrue(raw) {
		route = anyutil.String(raw)
	}
	homeText, _ := frame.ValueOr("home", nil).(string)
	profile := servedProfileName(homeText)
	bound := ""
	if keyed, ok := state.ProfileFromSessionKey(route); ok && keyed == profile {
		bound = profile
	}
	tools.SetSessionVars(env, tools.SessionVars{SessionKey: route, SessionID: sessionID, Profile: bound})
	defer tools.ClearSessionVars(env)
	if err := BindTurnWorkspace(env, policy, sessionID); err != nil {
		return err
	}
	return body(sessionID)
}
