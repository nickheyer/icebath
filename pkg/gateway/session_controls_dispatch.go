package gateway

import (
	"context"
	"database/sql"
	"errors"

	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Method to handler of every verb dispatch serves outside the group controls, session_controls.py handlers L215
func (c *AuthorityConnection) handlers() map[string]controlHandler {
	verbs := map[string]controlHandler{
		"session.create": c.create, "ping": c.pingVerb, "runtime.describe": c.describe,
		"commands.catalog": c.commandCatalog, "complete.slash": c.slashCompletions,
		"plugins.list": c.pluginsList, "command.observe": c.commandObserve,
		"slash.exec": c.slashExec, "command.dispatch": c.commandDispatch,
		"session.list": c.listSessions, "session.info": c.info, "session.revision": c.revision,
		"session.mutate": c.mutate, "bot_relay.deliver": c.botDeliver,
		"bot_relay.settle": c.botSettle, "a2a.forward": c.a2aForward,
		"kanban.run":  c.kanbanRun,
		"cron.submit": c.cronSubmit, "cron.status": c.cronStatus,
		"cron.cancel": c.cronCancel, "cron.recover": c.cronRecover,
		"worker.register": c.workerRegister, "worker.adopt": c.workerAdopt,
		"worker.persist": c.workerPersist,
		"setup.status":   c.setupStatus,
		"session.resume": c.resume, "session.detach": c.detach,
		"prompt.submit":  c.submit,
		"prompt.receipt": c.receipt, "prompt.cancel": c.cancel,
		"prompt.resolve_unknown": c.resolveUnknown,
		"session.interrupt":      c.interrupt,
		"approval.respond":       c.respond, "clarify.respond": c.respondClarify,
	}
	for _, group := range []map[string]controlHandler{
		busyControlHandlers(c), queuedHandlersOf(c), sessionConfigHandlers(c), tuiConfigHandlers(c),
		ancillaryHandlers(c), shellHandlers(c), tuiOpsHandlers(c), tuiSideHandlers(c), handoffHandlers(c), loginHandlers(c),
	} {
		for name, handler := range group {
			verbs[name] = handler
		}
	}
	bind := func(fn func(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object) (any, error)) controlHandler {
		return func(ctx context.Context, ref SessionRef, params *jsonutil.Object) (any, error) {
			return fn(ctx, c, ref, params)
		}
	}
	verbs["image.attach_bytes"] = bind(attachBytes)
	verbs["complete.path"] = bind(completePath)
	verbs["session.prune"] = bind(prune)
	verbs["session.stats"] = bind(stats)
	verbs["session.active_list"] = bind(activeList)
	verbs["session.automation"] = bind(automation)
	verbs["clarify.lock"] = bind(clarifyLock)
	for name, handler := range connectionHandlers(c) {
		verbs[name] = handler
	}
	return verbs
}

// Queued row verbs in the dispatch handler shape
func queuedHandlersOf(c *AuthorityConnection) map[string]controlHandler {
	out := map[string]controlHandler{}
	for name, handler := range queuedControlHandlers(c) {
		out[name] = func(ctx context.Context, ref SessionRef, params *jsonutil.Object) (any, error) {
			return handler(ctx, ref, params)
		}
	}
	return out
}

// JSON-RPC error reply with the refusal data
func refusalReply(rid any, code int, message string, data map[string]any) *jsonutil.Object {
	return jsonutil.ObjectOf("jsonrpc", "2.0", "id", rid, "error", jsonutil.ObjectOf("code", code, "message", message, "data", data))
}

// One WS frame of a viewer answered on the owner, session_controls.py dispatch L282
func (c *AuthorityConnection) Dispatch(ctx context.Context, request *jsonutil.Object) (*jsonutil.Object, error) {
	if IsResponseFrame(request) {
		return nil, c.resolveResponse(ctx, request)
	}
	rid := request.ValueOr("id", nil)
	method, _ := request.ValueOr("method", nil).(string)
	rawParams := request.ValueOr("params", nil)
	if rawParams == nil {
		rawParams = jsonutil.NewObject()
	}
	params, ok := rawParams.(*jsonutil.Object)
	if !ok {
		return refusalReply(rid, 4001, "invalid_params", map[string]any{"reason": "invalid_params"}), nil
	}
	if c.operator && WSMethodRefusedWhileDraining(c.authority.Runner, method) {
		err := runtimeRefusal("runtime_draining")
		logRefusal(c.actor.ProfileID, method, err)
		return refusalReply(rid, 4001, "runtime_draining", refusalData(err)), nil
	}
	if profile, isStr := params.ValueOr("profile", nil).(string); isStr && profile != "" {
		routed, err := connectionSiblingFor(c, ctx, profile)
		if err != nil {
			var refusal *state.RuntimeStoreError
			if !errors.As(err, &refusal) {
				return nil, err
			}
			logRefusal(c.actor.ProfileID, method, err)
			return refusalReply(rid, 4001, refusal.Reason, refusalData(err)), nil
		}
		if routed != nil {
			return routed.Dispatch(ctx, request)
		}
		if profileImplicitMethods[method] {
			params = params.Clone()
			params.Delete("profile")
		}
	}
	sessionID, _ := params.ValueOr("session_id", "").(string)
	ref := SessionRef{ProfileID: c.actor.ProfileID, SessionID: sessionID}
	typed := tuigateway.Lookup(tuigateway.KindCanonical, method)
	if typed == nil {
		typed = tuigateway.Lookup(tuigateway.KindMethod, method)
	}
	if typed != nil {
		var refused *tuigateway.ParamsRefusal
		if err := typed.FitParams(params); errors.As(err, &refused) {
			return refusalReply(rid, 4001, "invalid_params", map[string]any{"reason": "invalid_params", "fields": refused.Fields(), "detail": refused.Error()}), nil
		}
	}
	a := c.authority
	if _, held := a.live(ref.SessionID); ref.SessionID != "" && !a.Retiring && ref.ProfileID == a.ProfileID && !held {
		if err := adoptBeforeRestore(ctx, a, ref.SessionID, false); err != nil {
			return nil, err
		}
	}
	verbs := c.handlers()
	scoped := settings.WithSession(ownerScope(ctx, a, false), ref.SessionID)
	var result any
	var err error
	switch {
	case groupMethods[method] != "" || method == "profiles.list":
		result, err = dispatchGroupControl(scoped, c, method, params)
	case verbs[method] == nil && ServiceVerbs(method) != nil:
		return c.dispatchServiceVerb(scoped, ServiceVerbs(method), method, rid, request), nil
	case verbs[method] == nil || (connectionMethods[method] && !sessionOwned(params)):
		return refusalReply(rid, -32601, tuigateway.UnknownMethodMessage(method), map[string]any{"reason": "unknown_method"}), nil
	default:
		result, err = verbs[method](scoped, ref, params)
	}
	if err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			logRefusal(c.actor.ProfileID, method, err)
			code, message := 4001, refusal.Reason
			var wire wireCodedRefusal
			if errors.As(err, &wire) {
				code, message = wire.WireCode(), wire.WireMessage()
			}
			return refusalReply(rid, code, message, refusalData(err)), nil
		}
		if errors.Is(err, sql.ErrConnDone) || isSQLiteError(err) {
			logStorageFailure(c.actor.ProfileID, method, params.ValueOr("session_id", nil), err, sqliteErrorClass(err))
			return refusalReply(rid, 5001, "storage_unavailable", map[string]any{"reason": "storage_unavailable"}), nil
		}
		return nil, err
	}
	return jsonutil.ObjectOf("jsonrpc", "2.0", "id", rid, "result", checkCanonicalResult(typed, method, result)), nil
}

// Result of a verb checked against its typed contract, a misfit reported and the result sent as the verb gave it
func checkCanonicalResult(typed *tuigateway.Entry, method string, result any) any {
	if typed == nil {
		return result
	}
	if err := typed.CheckResult(result); err != nil {
		tuigateway.ReportMisfit("result of", method, err)
	}
	return result
}
