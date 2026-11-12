package gatewayrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Post admission plugin hook once per inbound message, never while an admitted row executes
func (r *GatewayRunner) hmPostAdmissionConsume(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, sessionKey string) (bool, *string, error) {
	if gateway.ExecutingAdmission(ctx) {
		return false, nil, nil
	}
	return runPostAdmissionHook(ctx, r, event, source, sessionKey)
}

// Runs the pre gateway dispatch plugin hook, nil to drop the event, else the event or its rewrite
func (r *GatewayRunner) hmPreGatewayDispatchHook(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource) *gateway.MessageEvent {
	if gateway.ExecutingAdmission(ctx) {
		return event
	}
	results := func() (results []any) {
		defer func() {
			if rec := recover(); rec != nil {
				inboundLog().Warn(fmt.Sprintf("pre_gateway_dispatch invocation failed: %s", pyrepr.Str(rec)))
				results = nil
			}
		}()
		var store any
		if r.sessionStore != nil {
			store = r.sessionStore
		}
		return agent.LifecycleAInvokeHook(ctx, "pre_gateway_dispatch", map[string]any{"event": event, "gateway": r, "session_store": store})
	}()
	for _, raw := range results {
		result, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch result["action"] {
		case "skip":
			platform, chat := "unknown", "unknown"
			if source.Platform != "" {
				platform = string(source.Platform)
			}
			if source.ChatID != "" {
				chat = source.ChatID
			}
			inboundLog().Info(fmt.Sprintf("pre_gateway_dispatch skip: reason=%s platform=%s chat=%s", pyrepr.Str(result["reason"]), platform, chat))
			return nil
		case "rewrite":
			if text, ok := result["text"].(string); ok {
				rewritten := gateway.ReplaceMessageEventText(event, text)
				gateway.CarryCallback(ctx, event, rewritten)
				event = rewritten
			}
			return event
		case "allow":
			return event
		}
	}
	return event
}

// User defined quick commands from config, empty when unset or malformed
func (r *GatewayRunner) hmQuickCommands() map[string]any {
	if r.config == nil || r.config.QuickCommands == nil {
		return map[string]any{}
	}
	return r.config.QuickCommands
}

// Rewrites the event text to an alias quick command target and returns the new command name, false without a target
func hmExpandAliasQuickCommand(event *gateway.MessageEvent, qcmd map[string]any) (string, bool) {
	target := ""
	if raw := qcmd["target"]; pyrepr.Truthy(raw) {
		target = pystr.Strip(pyrepr.Str(raw))
	}
	if target == "" {
		return "", false
	}
	if !strings.HasPrefix(target, "/") {
		target = "/" + target
	}
	event.Text = pystr.Strip(target + " " + pystr.Strip(event.GetCommandArgs()))
	targetCommand := strings.TrimLeft(target, "/")
	if targetCommand == "" {
		return targetCommand, true
	}
	fields := pystr.SplitWhitespace(targetCommand, -1)
	if len(fields) == 0 {
		return "", true
	}
	return fields[0], true
}
