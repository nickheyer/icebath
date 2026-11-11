package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var promptRespondRegistry = &handlerRegistry{}

func init() {
	promptRespondRegistry.method("clarify.lock", clarifyLock)
	promptRespondRegistry.method("request.answer", requestAnswer)
	promptRespondRegistry.method("approval.pending", func(ctx context.Context, rid any, params map[string]any) map[string]any {
		session, errResp := sess(ctx, params, rid)
		if errResp != nil {
			return errResp
		}
		return approvalReply(rid, "approvals", func() any { return tools.ListGatewayApprovals(strOr(session.Get("session_key"))) })
	})
	promptRespondRegistry.method("approval.received", func(ctx context.Context, rid any, params map[string]any) map[string]any {
		session, errResp := sess(ctx, params, rid)
		if errResp != nil {
			return errResp
		}
		requestID, isStr := params["request_id"].(string)
		if !isStr || requestID == "" {
			return rpcErr(rid, 4006, "request_id required", nil)
		}
		return approvalReply(rid, "acknowledged", func() any { return tools.AckGatewayApproval(strOr(session.Get("session_key")), requestID) })
	})
	promptRespondRegistry.method("approval.respond", approvalRespond)
	promptRespondRegistry.install()
}

// Locks one answer of an open clarify card, expired once the wait already ended
func clarifyLock(ctx context.Context, rid any, params map[string]any) map[string]any {
	requestID, questionID := strOr(params["request_id"]), strOr(params["question_id"])
	if requestID == "" || questionID == "" {
		return rpcErr(rid, 4002, "request_id and question_id required", nil)
	}
	var answer any = ""
	if value, present := params["answer"]; present {
		answer = value
	}
	if _, isStr := answer.(string); answer != nil && !isStr {
		encoded, err := pyjson.Dumps(answer, pyjson.EnsureASCII(false))
		panicIf(err)
		answer = encoded
	}
	if proxied := lockComputeHostClarify(ctx, rid, requestID, questionID, answer); proxied != nil {
		return proxied
	}
	remaining, open, err := tuigateway.LockAnswer(requestID, questionID, answer)
	if err != nil {
		return rpcErr(rid, 4002, err.Error(), nil)
	}
	if !open {
		return ok(rid, map[string]any{"status": "expired"})
	}
	return ok(rid, map[string]any{"status": "ok", "remaining": remaining})
}

// Answers an open server to client request for a client that never received it
func requestAnswer(ctx context.Context, rid any, params map[string]any) map[string]any {
	requestID := strOr(params["id"])
	result, isDict := params["result"].(map[string]any)
	if requestID == "" || !isDict {
		return rpcErr(rid, 4002, "id and an object result required", nil)
	}
	frame := pyjson.NewObject()
	frame.Set("jsonrpc", "2.0")
	frame.Set("id", requestID)
	frame.Set("result", result)
	if tuigateway.ResolveResponse(frame, nil) || relayComputeHostResponse(ctx, frame) {
		return ok(rid, map[string]any{"status": "ok"})
	}
	return ok(rid, map[string]any{"status": "expired"})
}

// Approval call result under its key, 5004 on any failure
func approvalReply(rid any, resultKey string, call func() any) map[string]any {
	var value any
	if failure := catchPanic(func() { value = call() }); failure != nil {
		return rpcErr(rid, 5004, pyrepr.Str(failure), nil)
	}
	return ok(rid, map[string]any{resultKey: value})
}

// Live session of a stale sid found by its pending request id, else by its id as a stored key
func approvalRespondSessionFallback(params map[string]any) *tuigateway.Session {
	if requestID := strOr(params["request_id"]); requestID != "" {
		var found *tuigateway.Session
		if failure := catchPanic(func() {
			for _, live := range tuigateway.LiveSessions() {
				key := strOr(live.Session.Get("session_key"))
				if key == "" {
					continue
				}
				for _, pending := range tools.ListGatewayApprovals(key) {
					if strOr(pending["request_id"]) == requestID {
						found = live.Session
						return
					}
				}
			}
		}); failure != nil {
			logger().Debug("approval.respond request_id fallback failed", "error", failure)
		}
		if found != nil {
			return found
		}
	}
	if target := strOr(params["session_id"]); target != "" {
		var found *tuigateway.Session
		if failure := catchPanic(func() {
			if _, live, ok := findLiveSessionByKey(target, "", true); ok {
				found = live
			}
		}); failure != nil {
			logger().Debug("approval.respond stored-id fallback failed", "error", failure)
		}
		return found
	}
	return nil
}

// Resolves the session's pending approval, a stale sid resolved by durable identity first
func approvalRespond(ctx context.Context, rid any, params map[string]any) map[string]any {
	session, errResp := sess(ctx, params, rid)
	if errResp != nil {
		if detail, _ := errResp["error"].(map[string]any); detail["code"] != 4001 {
			return errResp
		}
		if session = approvalRespondSessionFallback(params); session == nil {
			return errResp
		}
	}
	var choice any = "deny"
	if value, present := params["choice"]; present {
		choice = value
	}
	var resolveAll any = false
	if value, present := params["all"]; present {
		resolveAll = value
	}
	return approvalReply(rid, "resolved", func() any {
		return tools.ResolveGatewayApproval(strOr(session.Get("session_key")), pyrepr.Str(choice), truthy(resolveAll), "", strOr(params["request_id"]))
	})
}
