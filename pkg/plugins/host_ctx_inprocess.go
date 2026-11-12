package plugins

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Context methods served to a hosted plugin only under in process isolation beyond the object taking ones
var hostInProcessServed = map[string]bool{"register_cli_command": true, "post_message": true, "apost_message": true,
	"register_approval_transport": true}

// Classes the request and decision of a hosted approval transport are restored into and judged against
const (
	approvalRequestRef  = "hermes_cli.approval_transport:ApprovalRequest"
	approvalDecisionRef = "hermes_cli.approval_transport:ApprovalDecision"
)

// Present function running a hosted transport on the request restored as an ApprovalRequest, any non decision answer invalid
func hostedApprovalPresent(f *HostedCallable) tools.ApprovalPresentFunc {
	return func(ctx context.Context, req tools.ApprovalRequest) (tools.ApprovalTransportDecision, error) {
		fields := map[string]any{"schema_version": int64(req.SchemaVersion), "request_id": req.RequestID, "digest": req.Digest,
			"command": req.Command, "description": req.Description, "pattern_key": req.PatternKey, "pattern_keys": anyList(req.PatternKeys),
			"surface": req.Surface, "timeout_seconds": req.TimeoutSeconds, "allowed_choices": anyList(req.AllowedChoices)}
		raw, err := f.host.call(ctx, "invoke", map[string]any{"ref": f.ref, "args": f.host.encodeArgs([]any{fields}), "kwargs": map[string]any{},
			"restore": []any{approvalRequestRef}, "judge": approvalDecisionRef}, &f.generation, 0, nil)
		if err != nil {
			return tools.ApprovalTransportDecision{}, err
		}
		reply, _ := raw.(map[string]any)
		decision, isRecord := reply["value"].(Record)
		if reply["is_instance"] != true || !isRecord {
			return tools.ApprovalTransportDecision{}, nil
		}
		return tools.ApprovalTransportDecision{RequestID: config.PyStr(decision["request_id"]), RequestDigest: config.PyStr(decision["request_digest"]),
			Choice: config.PyStr(decision["choice"])}, nil
	}
}

func init() {
	hostCtxDispatch["register_approval_transport"] = hostMethod("register_approval_transport", []pyParam{req("name"), req("present_fn")},
		func(ctx context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
			var present tools.ApprovalPresentFunc
			if f, ok := a["present_fn"].(*HostedCallable); ok && f != nil {
				present = hostedApprovalPresent(f)
			}
			return nil, c.RegisterApprovalTransport(ctx, config.PyStr(a["name"]), present)
		})
	hostCtxDispatch["post_message"] = hostMethod("post_message", []pyParam{req("text")}, func(ctx context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
		return c.PostMessage(ctx, config.PyStr(a["text"])), nil
	})
	hostCtxDispatch["apost_message"] = hostMethod("apost_message", []pyParam{req("text")}, func(ctx context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
		return c.APostMessage(ctx, config.PyStr(a["text"]))
	})
}
