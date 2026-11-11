package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Event the ingress gates let through with its source and whether it is internal
type admittedEvent struct {
	event    *gateway.MessageEvent
	source   *gateway.SessionSource
	internal bool
}

// Ingress gates of an inbound message, nil when dropped, the context carrying a session identity reset to unbound
func (r *GatewayRunner) hmAdmitEvent(ctx context.Context, event *gateway.MessageEvent) (context.Context, *admittedEvent, error) {
	source := event.Source
	if env := tools.ToolEnvFrom(ctx); env != nil {
		child := tools.CopySessionContext(env)
		tools.ResetSessionVars(child)
		ctx = tools.WithToolEnv(ctx, child)
	}
	if r.config != nil && r.config.MultiplexProfiles != nil && *r.config.MultiplexProfiles {
		r.canonicalize(ctx, source, "", "")
	}
	if source != nil && source.ProfileRouteRejected {
		intake, _ := r.intakeAdapterFor(ctx, source)
		gateway.DropRejectedRoute(ctx, intake, source)
		return ctx, nil, nil
	}
	isInternal := event.Internal
	chatID := ""
	if source != nil {
		chatID = source.ChatID
	}
	if !isInternal && source != nil && source.Platform == gateway.PlatformSlack {
		slackAdapter, _ := r.intakeAdapterFor(ctx, source)
		if isSlackIgnoredChannel(ctx, r.config, chatID, slackAdapter) {
			runLog().Info(fmt.Sprintf("Dropping Slack message from configured ignored channel %s", chatID))
			return ctx, nil, nil
		}
	}
	if r.startupRestoreInProgress && !isInternal && !event.StartupRestoreReplay && !gateway.ExecutingAdmission(ctx) {
		r.queueStartupRestoreEvent(event)
		return ctx, nil, nil
	}
	if isInternal {
		return ctx, &admittedEvent{event: event, source: source, internal: true}, nil
	}
	if fifoOnly, _ := event.FIFOOnly(); fifoOnly {
		return ctx, &admittedEvent{event: event, source: source}, nil
	}
	r.scaleToZeroNoteRealInbound(ctx)
	event = r.hmPreGatewayDispatchHook(ctx, event, source)
	if event == nil {
		return ctx, nil, nil
	}
	source = event.Source
	authorized, err := r.isUserAuthorizedForSource(ctx, source, true)
	if err != nil {
		return ctx, nil, err
	}
	if !authorized {
		if source.UserID == nil {
			runLog().Debug(fmt.Sprintf("Ignoring message with no user_id from %s", source.Platform))
			return ctx, nil, nil
		}
		unauthorized := fmt.Sprintf("Unauthorized user: %s (%s) on %s", *source.UserID, derefOrNoneText(source.UserName), source.Platform)
		pairableDM := source.ChatType == "dm" && !source.IsBot
		behavior := ""
		if pairableDM {
			behavior = r.getUnauthorizedDMBehavior(ctx, source.Platform, source.Profile)
		}
		switch {
		case behavior == "pair":
			runLog().Warn(unauthorized)
			err = r.hmOfferPairingCode(ctx, source)
		case behavior == "decline":
			runLog().Warn(unauthorized)
			err = r.hmSendUnauthorizedDecline(ctx, source)
		case pairableDM:
			err = r.hmReportIgnoredDM(ctx, source)
		default:
			runLog().Warn(unauthorized)
		}
		return ctx, nil, err
	}
	if !event.BotLoopAdmitted() {
		admitted, err := r.admitBotMessageForSource(ctx, source)
		if err != nil {
			return ctx, nil, err
		}
		if !admitted {
			return ctx, nil, nil
		}
	}
	return ctx, &admittedEvent{event: event, source: source}, nil
}
