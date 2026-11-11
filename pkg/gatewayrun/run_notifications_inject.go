package gatewayrun

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Injects a watch or completion notification as a synthetic event, true on acceptance, false to retry, nil with no route
func (r *GatewayRunner) InjectWatchNotification(ctx context.Context, text string, event map[string]any) (*bool, error) {
	return r.injectWatchNotification(ctx, text, event, false)
}

// Optional bool of a literal
func boolp(v bool) *bool { return &v }

// Injection routed from the queued event, a refusal raised to durable callers when raiseNotAccepted is set
func (r *GatewayRunner) injectWatchNotification(ctx context.Context, synthText string, evt map[string]any, raiseNotAccepted bool) (*bool, error) {
	if err := gateway.RestoreCompletionRoute(ctx, r, pyrepr.Str(pyrepr.Or(evt["session_key"], ""))); err != nil {
		return nil, err
	}
	queued := evt
	evt, err := gateway.FollowDetachedPin(ctx, r, evt)
	if err != nil {
		return nil, err
	}
	source, err := r.buildProcessEventSource(ctx, evt)
	if err != nil {
		return nil, err
	}
	if source == nil {
		if rawSID := rawProcessEventSessionID(evt); rawSID != "" {
			adapter, _ := r.adapters.Get(gateway.PlatformAPIServer)
			if adapter != nil && !gateway.AdapterSupportsPush(adapter) {
				delivered, err := r.selfPostAPIServer(ctx, adapter, synthText, rawSID, evt)
				if err != nil {
					return nil, err
				}
				return boolp(delivered), nil
			}
			runLog().Debug(fmt.Sprintf("Deferring watch notification for raw session %s: no api_server adapter to self-post through", rawSID))
			return boolp(false), nil
		}
		runLog().Warn(fmt.Sprintf("Dropping watch notification with no routing metadata for process %s", pyrepr.Str(evtGet(evt, "session_id", "unknown"))))
		return nil, nil
	}
	platformName := string(source.Platform)
	adapter, err := r.resolveInjectionAdapter(ctx, platformName, source)
	if err != nil {
		return nil, err
	}
	if adapter == nil && platformName == string(gateway.PlatformAPIServer) && source.Profile != nil && *source.Profile != "" {
		adapter, _ = r.adapters.Get(gateway.PlatformAPIServer)
	}
	if adapter == nil {
		return boolp(false), nil
	}
	if !gateway.AdapterSupportsPush(adapter) {
		rawSID := pystr.Strip(pyrepr.Str(pyrepr.Or(evt["origin_session_id"], "")))
		if rawSID == "" {
			rawSID = source.ChatID
		}
		delivered, err := r.selfPostAPIServer(ctx, adapter, synthText, rawSID, evt)
		if err != nil {
			return nil, err
		}
		return boolp(delivered), nil
	}
	admitErr := r.admitWatchNotification(ctx, adapter, synthText, evt, source, platformName)
	if admitErr == nil {
		return boolp(true), nil
	}
	var refused *gateway.WakeNotAccepted
	if !errors.As(admitErr, &refused) {
		runLog().Error(fmt.Sprintf("Watch notification injection error: %s", admitErr))
		return boolp(false), nil
	}
	pinGone, err := r.watchPinGone(ctx, evt, refused.Error(), !raiseNotAccepted)
	if err != nil {
		return nil, err
	}
	refused.Permanent = pinGone || (refused.Reason != nil && gateway.PermanentRefusalReasons[*refused.Reason])
	if pinGone && !raiseNotAccepted {
		return nil, nil
	}
	if !raiseNotAccepted {
		queued["_refusal_permanent"] = refused.Permanent
	}
	runLog().Warn(fmt.Sprintf("Watch notification not admitted for %s chat=%s: %s", platformName, source.ChatID, refused))
	if raiseNotAccepted {
		return nil, refused
	}
	return boolp(false), nil
}

// Builds the synthetic internal event and admits it through the authority or the adapter's wake path
func (r *GatewayRunner) admitWatchNotification(ctx context.Context, adapter gateway.PlatformAdapter, synthText string, evt map[string]any, source *gateway.SessionSource, platformName string) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("%s", pyrepr.Str(rec))
		}
	}()
	synthText = gateway.MarkInternalNotification(synthText)
	metadata := watchNotificationMetadata(evt, source)
	if trigger := pystr.Strip(pyrepr.Str(pyrepr.Or(evt["message_id"], ""))); trigger != "" {
		metadata["original_trigger_message_id"] = trigger
	}
	if source.MessageID != nil && *source.MessageID != "" {
		source = gateway.ReplaceSource(source, func(s *gateway.SessionSource) { s.MessageID = nil })
	}
	synthEvent := gateway.NewMessageEvent(synthText)
	synthEvent.Source = source
	synthEvent.Internal = true
	synthEvent.Metadata = metadata
	threadID := "None"
	if source.ThreadID != nil {
		threadID = *source.ThreadID
	}
	runLog().Info(fmt.Sprintf("Watch pattern notification — injecting for %s chat=%s thread=%s", platformName, source.ChatID, threadID))
	if primer, ok := adapter.(gateway.RoutingCachePrimer); ok {
		primer.PrimeRoutingCache(synthEvent)
	}
	authority, err := gateway.ActiveAuthority(ctx, r)
	if err != nil {
		return err
	}
	if authority != nil {
		if _, err := gateway.AdmitProcessNotice(ctx, r, authority, adapter, synthEvent, evt); err != nil {
			var refusal *state.RuntimeStoreError
			if errors.As(err, &refusal) {
				reason := refusal.Reason
				return &gateway.WakeNotAccepted{Msg: refusal.Error(), Reason: &reason}
			}
			return err
		}
		return nil
	}
	return gateway.AdmitInternalEvent(ctx, adapter, synthEvent)
}
