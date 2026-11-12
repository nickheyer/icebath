package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const postAdmissionHook = "post_gateway_admission"

// Logger carrying the record name of the hermes consumer module
func consumerLog() *slog.Logger { return slog.Default().With("logger", "gateway.run_inbound_consumer") }

// Context scoped to the routed profile of the source when the runner multiplexes profiles
func (r *GatewayRunner) routedScope(ctx context.Context, source *gateway.SessionSource) (context.Context, error) {
	if r.config == nil || r.config.MultiplexProfiles == nil || !*r.config.MultiplexProfiles {
		return ctx, nil
	}
	profileHome, err := r.resolveProfileHomeForSource(ctx, source)
	if err != nil {
		return nil, err
	}
	return gateway.ProfileRuntimeScope(ctx, profileHome, true), nil
}

// Offers an admitted message to the post admission plugin hook, handled with an optional reply or fail open
func runPostAdmissionHook(ctx context.Context, r *GatewayRunner, event *gateway.MessageEvent, source *gateway.SessionSource, sessionKey string) (handled bool, reply *string, cancelled error) {
	results, err := func() (results []any, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%s", pyrepr.Str(rec))
			}
		}()
		scoped, err := r.routedScope(ctx, source)
		if err != nil {
			return nil, err
		}
		if !agent.LifecycleHasHook(scoped, postAdmissionHook) {
			return nil, nil
		}
		var messageID any
		if event.MessageID != nil {
			messageID = *event.MessageID
		}
		return agent.LifecycleAInvokeHook(scoped, postAdmissionHook, map[string]any{
			"session_key": sessionKey, "platform": string(source.Platform), "source": source.ToDict(),
			"message_id": messageID, "text": event.Text,
		}), nil
	}()
	if err != nil {
		if ctx.Err() != nil {
			return false, nil, ctx.Err()
		}
		consumerLog().Warn(postAdmissionHook+" failed; continuing with the agent turn", "exc_info", err)
		return false, nil, nil
	}
	for _, result := range results {
		dict, ok := result.(map[string]any)
		if !ok || dict["action"] != "handled" {
			continue
		}
		consumerLog().Info(fmt.Sprintf("%s: a plugin handled the message for session %s", postAdmissionHook, sessionKey))
		if text, ok := dict["reply"].(string); ok && pystr.Strip(text) != "" {
			return true, &text, nil
		}
		return true, nil, nil
	}
	return false, nil, nil
}
