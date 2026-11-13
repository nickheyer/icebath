package plugins

import (
	"context"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Context methods served to a hosted plugin only under in process isolation beyond the object taking ones
var hostInProcessServed = map[string]bool{"register_cli_command": true, "post_message": true, "apost_message": true,
	"register_approval_transport": true, "register_platform": true, "register_slack_action_handler": true}

// Passive dependency probe running a hosted check_fn, its exception raised as a panic the way the Go probes recover
func hostedCheck(v any) func() bool {
	return func() bool {
		f, ok := v.(*HostedCallable)
		if !ok {
			panic(&HostBuiltinError{Type: "TypeError", Message: "'" + pyTypeName(v) + "' object is not callable"})
		}
		result, err := f.Call(f.HostContext(), nil, nil)
		if err != nil {
			panic(err)
		}
		return config.PyTruthy(result)
	}
}

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
	hostCtxDispatch["register_platform"] = hostMethod("register_platform", []pyParam{req("name"), req("label"), req("adapter_factory"), req("check_fn"),
		opt("validate_config", nil), opt("required_env", nil), opt("install_hint", ""), varKwargs()},
		func(_ context.Context, _ *PluginHost, c *Context, a, extra map[string]any) (any, error) {
			return c.RegisterPlatform(PlatformSpec{Name: argStr(a["name"]), Label: argStr(a["label"]), AdapterFactory: a["adapter_factory"],
				Check: hostedCheck(a["check_fn"]), ValidateConfig: a["validate_config"], RequiredEnv: argStrings(a["required_env"]),
				InstallHint: argStr(a["install_hint"]), Extra: extra})
		})
	hostCtxDispatch["register_slack_action_handler"] = hostMethod("register_slack_action_handler", []pyParam{req("action_id"), req("callback")},
		func(ctx context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
			actionID, err := slackActionID(a["action_id"])
			if err != nil {
				return nil, err
			}
			callback := a["callback"]
			if f, ok := callback.(*HostedCallable); ok {
				callback = hostedSlackAction(f)
			}
			text, ok := ctx.Value(actionReprKey{}).(string)
			if !ok {
				text = slackActionRepr(a["action_id"])
			}
			return c.registerSlackActionHandler(actionID, callback, text)
		})
	hostCtxDispatch["post_message"] = hostMethod("post_message", []pyParam{req("text")}, func(ctx context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
		return c.PostMessage(ctx, config.PyStr(a["text"])), nil
	})
	hostCtxDispatch["apost_message"] = hostMethod("apost_message", []pyParam{req("text")}, func(ctx context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
		return c.APostMessage(ctx, config.PyStr(a["text"]))
	})
}

// Context key of the repr the child took of a Slack action id before it crossed
type actionReprKey struct{}

// Slack action listener running a guest callback with the acknowledgement as a host callable and plain body and action
func hostedSlackAction(f *HostedCallable) func(context.Context, func(map[string]any) error, map[string]any, map[string]any) error {
	return func(ctx context.Context, ack func(map[string]any) error, body, action map[string]any) error {
		complete := HostCallable(func(_ context.Context, args []any, _ map[string]any) (any, error) {
			var payload map[string]any
			if len(args) > 0 {
				payload = argMap(args[0])
			}
			return nil, ack(payload)
		})
		_, err := f.Call(ctx, []any{complete, body, action}, nil)
		return err
	}
}

// Action id a hosted plugin passed, its compiled patterns as Go regexps and constraint dicts converted per value
func slackActionID(v any) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		if pattern, ok := x["__pattern__"].(string); ok {
			return pythonPattern(pattern, x["flags"])
		}
		out := make(map[string]any, len(x))
		for k, item := range x {
			converted, err := slackActionID(item)
			if err != nil {
				return nil, err
			}
			out[k] = converted
		}
		return out, nil
	case Record:
		return slackActionID(map[string]any(x))
	}
	return v, nil
}

// Go regexp of a Python re pattern and its flags, re.error wording for a pattern RE2 cannot compile
func pythonPattern(pattern string, flags any) (*regexp.Regexp, error) {
	n, _ := wireInt(flags)
	prefix := ""
	for _, f := range []struct {
		bit  int64
		mode string
	}{{2, "i"}, {8, "m"}, {16, "s"}} {
		if n&f.bit != 0 {
			prefix += f.mode
		}
	}
	if n&64 != 0 {
		pattern = verbosePattern(pattern)
	}
	if prefix != "" {
		pattern = "(?" + prefix + ")" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, &HostBuiltinError{Type: "re.error", Message: err.Error()}
	}
	return re, nil
}

// Pattern with re.VERBOSE whitespace and comments removed outside character classes and escapes
func verbosePattern(pattern string) string {
	var b strings.Builder
	inClass := false
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '\\' && i+1 < len(runes):
			b.WriteRune(r)
			i++
			b.WriteRune(runes[i])
		case inClass:
			if r == ']' {
				inClass = false
			}
			b.WriteRune(r)
		case r == '[':
			inClass = true
			b.WriteRune(r)
		case r == '#':
			for i+1 < len(runes) && runes[i+1] != '\n' {
				i++
			}
		case r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\v':
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
