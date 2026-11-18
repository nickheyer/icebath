package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Decoded kanban context of a bootstrap frame's policy, nil for a chat turn
func WorkerKanbanContext(frame *pyjson.Object) (any, error) {
	policy, _ := frame.GetOr("policy", nil).(*pyjson.Object)
	raw := policy.GetOr("kanban_json", nil)
	if !pyrepr.Truthy(raw) {
		raw = "null"
	}
	text, isText := raw.(string)
	if !isText {
		return nil, &PyError{Kind: "TypeError", Msg: "the worker kanban context is " + errchain.Kind(raw) + ", expected JSON text"}
	}
	return pyjson.Loads(text)
}

// Runs body in the session workspace with its session vars bound, the session id as task id when scoped
func WorkerWorkspace(ctx context.Context, env *tools.Env, frame *pyjson.Object, body func(taskID string) error) error {
	rawScope := frame.GetOr("scope", nil)
	scope, isDict := rawScope.(*pyjson.Object)
	if pyrepr.Truthy(rawScope) && !isDict {
		return &PyError{Kind: "AttributeError", Msg: "the worker workspace scope is " + errchain.Kind(rawScope) + ", expected a mapping"}
	}
	sessionID := ""
	if scope != nil && pyrepr.Truthy(scope.GetOr("session_id", nil)) {
		sessionID = pyrepr.Str(scope.GetOr("session_id", nil))
	}
	if sessionID == "" {
		return body("")
	}
	policy, err := RestorePolicy(frame.GetOr("policy", nil))
	if err != nil {
		return err
	}
	route := sessionID
	if raw := frame.GetOr("route", nil); pyrepr.Truthy(raw) {
		route = pyrepr.Str(raw)
	}
	homeText, _ := frame.GetOr("home", nil).(string)
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
