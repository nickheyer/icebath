package gateway

import (
	"context"
	"database/sql"
	"errors"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Method to handler of every verb dispatch serves outside the group controls, session_controls.py handlers L215
func (c *AuthorityConnection) handlers() map[string]controlHandler {
	verbs := map[string]controlHandler{
		"session.create": c.create, "ping": c.pingVerb, "runtime.describe": c.describe,
		"commands.catalog": c.commandCatalog, "complete.slash": c.slashCompletions,
		"plugins.list": c.pluginsList, "command.observe": c.commandObserve,
		"slash.exec": c.slashExec, "command.dispatch": c.commandDispatch,
		"session.list": c.listSessions, "session.info": c.info,
		"session.mutate": c.mutate, "bot_relay.deliver": c.botDeliver,
		"bot_relay.settle":      c.botSettle,
		"bot_relay.roster.sync": c.botRoster, "bot_relay.outbox.drain": c.botOutbox,
		"bot_relay.reply": c.botReply, "a2a.forward": c.a2aForward,
		"kanban.run":  c.kanbanRun,
		"cron.submit": c.cronSubmit, "cron.status": c.cronStatus,
		"cron.cancel": c.cronCancel, "cron.recover": c.cronRecover,
		"worker.register": c.workerRegister, "worker.adopt": c.workerAdopt,
		"worker.persist": c.workerPersist,
		"setup.status":   c.setupStatus, "setup.runtime_check": c.setupRuntimeCheck,
		"session.resume": c.resume, "session.detach": c.detach,
		"prompt.submit":  c.submit,
		"prompt.receipt": c.receipt, "prompt.cancel": c.cancel,
		"prompt.resolve_unknown": c.resolveUnknown,
		"session.interrupt":      c.interrupt, "session.events.since": c.eventsSince,
		"approval.respond": c.respond, "clarify.respond": c.respondClarify,
	}
	for _, group := range []map[string]controlHandler{
		busyControlHandlers(c), queuedHandlersOf(c), sessionConfigHandlers(c), tuiConfigHandlers(c),
		ancillaryHandlers(c), shellHandlers(c), tuiOpsHandlers(c), tuiSideHandlers(c), oneshotHandlers(c),
	} {
		for name, handler := range group {
			verbs[name] = handler
		}
	}
	bind := func(fn func(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error)) controlHandler {
		return func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return fn(ctx, c, ref, params)
		}
	}
	verbs["image.attach_bytes"] = bind(attachBytes)
	verbs["complete.path"] = bind(completePath)
	verbs["session.prune"] = bind(prune)
	verbs["session.stats"] = bind(stats)
	verbs["session.active_list"] = bind(activeList)
	verbs["session.automation"] = bind(automation)
	verbs["message.react"] = bind(react)
	verbs["clarify.lock"] = bind(clarifyLock)
	for name, handler := range connectionHandlers(c) {
		verbs[name] = handler
	}
	verbs["session.control"] = bind(control)
	for name, handler := range onboardingHandlers(c) {
		verbs[name] = handler
	}
	return verbs
}

// Queued row verbs in the dispatch handler shape
func queuedHandlersOf(c *AuthorityConnection) map[string]controlHandler {
	out := map[string]controlHandler{}
	for name, handler := range queuedControlHandlers(c) {
		out[name] = func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return handler(ctx, ref, params)
		}
	}
	return out
}

// JSON-RPC error reply with the refusal data
func refusalReply(rid any, code int, message string, data map[string]any) *pyjson.Object {
	return pyjson.ObjectOf("jsonrpc", "2.0", "id", rid, "error", pyjson.ObjectOf("code", code, "message", message, "data", data))
}

// One WS frame of a viewer answered on the owner, session_controls.py dispatch L282
func (c *AuthorityConnection) Dispatch(ctx context.Context, request *pyjson.Object) (*pyjson.Object, error) {
	if IsResponseFrame(request) {
		return nil, c.resolveResponse(ctx, request)
	}
	rid := request.GetOr("id", nil)
	method, _ := request.GetOr("method", nil).(string)
	rawParams := request.GetOr("params", nil)
	if rawParams == nil {
		rawParams = pyjson.NewObject()
	}
	params, ok := rawParams.(*pyjson.Object)
	if !ok {
		return refusalReply(rid, 4001, "invalid_params", map[string]any{"reason": "invalid_params"}), nil
	}
	if c.operator && WSMethodRefusedWhileDraining(c.authority.Runner, method) {
		err := runtimeRefusal("runtime_draining")
		logRefusal(c.actor.ProfileID, method, err)
		return refusalReply(rid, 4001, "runtime_draining", refusalData(err)), nil
	}
	if profile, isStr := params.GetOr("profile", nil).(string); isStr && profile != "" {
		routed, err := c.siblingFor(ctx, profile)
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
			params = params.Copy()
			params.Delete("profile")
		}
	}
	sessionID, _ := params.GetOr("session_id", "").(string)
	ref := SessionRef{ProfileID: c.actor.ProfileID, SessionID: sessionID}
	contract := tuigateway.CanonicalMethods[method]
	if contract != nil {
		if problems := tuigateway.CanonicalParamProblems(contract, params); len(problems) > 0 {
			return refusalReply(rid, 4001, "invalid_params", map[string]any{"reason": "invalid_params", "fields": problems}), nil
		}
	}
	original := params.Copy()
	if contract == nil {
		contract = tuigateway.Methods[method]
	}
	a := c.authority
	if _, held := a.live(ref.SessionID); ref.SessionID != "" && !a.Retiring && ref.ProfileID == a.ProfileID && !held {
		if err := adoptBeforeRestore(ctx, a, ref.SessionID, false); err != nil {
			return nil, err
		}
	}
	verbs := c.handlers()
	scoped := ownerScope(ctx, a, true)
	var result any
	var err error
	switch {
	case groupMethods[method] != "" || method == "profiles.list":
		result, err = dispatchGroupControl(scoped, c, method, params)
	case verbs[method] == nil || (connectionMethods[method] && !sessionOwned(params)):
		return refusalReply(rid, -32601, tuigateway.UnknownMethodMessage(method), map[string]any{"reason": "unknown_method"}), nil
	default:
		result, err = verbs[method](scoped, ref, params)
		if deferred, isDeferred := result.(*DeferredReply); err == nil && isDeferred {
			tuigateway.CheckParamsAccepted(contract, original)
			check := func(result any) error { tuigateway.CheckResult(contract, result); return nil }
			if err = deferred.Start(scoped, c, rid, check); err == nil {
				return nil, nil
			}
		}
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
			logStorageFailure(c.actor.ProfileID, method, params.GetOr("session_id", nil), err, sqliteErrorClass(err))
			return refusalReply(rid, 5001, "storage_unavailable", map[string]any{"reason": "storage_unavailable"}), nil
		}
		return nil, err
	}
	tuigateway.CheckParamsAccepted(contract, original)
	tuigateway.CheckResult(contract, result)
	return pyjson.ObjectOf("jsonrpc", "2.0", "id", rid, "result", result), nil
}
