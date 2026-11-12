package gatewayrun

import (
	"context"
	"fmt"
	"slices"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Awaits the local session's busy accepted hook once a mid run command accepts, nothing on messaging
func busyAccepted(ctx context.Context) error {
	if hook := gateway.BusyAcceptedHook(ctx); hook != nil {
		return hook(ctx)
	}
	return nil
}

// Engages the global emergency stop with an optional reason, or lifts it with off
func (r *GatewayRunner) handlePauseCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	args := pystr.Strip(event.GetCommandArgs())
	if slices.Contains([]string{"off", "resume", "stop", "disengage"}, pystr.Lower(args)) {
		if tools.Disengage(ctx) {
			return i18n.T(ctx, "gateway.pause.resumed", "", nil), nil
		}
		return i18n.T(ctx, "gateway.pause.not_paused", "", nil), nil
	}
	if state := tools.GetState(ctx); state != nil && args == "" {
		suffix := ""
		if state.Reason != "" {
			suffix = i18n.T(ctx, "gateway.pause.reason_suffix", "", map[string]any{"reason": state.Reason})
		}
		return i18n.T(ctx, "gateway.pause.already_paused", "", map[string]any{"suffix": suffix}), nil
	}
	tools.Engage(ctx, args)
	suffix := ""
	if args != "" {
		suffix = i18n.T(ctx, "gateway.pause.reason_suffix", "", map[string]any{"reason": args})
	}
	return i18n.T(ctx, "gateway.pause.paused", "", map[string]any{"suffix": suffix}), nil
}

// Ignores the platform start ping of an active session
func (r *GatewayRunner) busyStartCommand(ctx context.Context, event *gateway.MessageEvent, quickKey string, source *gateway.SessionSource) (any, error) {
	if err := busyAccepted(ctx); err != nil {
		return nil, err
	}
	runLog().Info(fmt.Sprintf("Ignoring /start platform ping for active session %s", quickKey))
	return "", nil
}

// Replies with the egress proxy status mid run
func (r *GatewayRunner) busyEgressCommand(ctx context.Context, event *gateway.MessageEvent, quickKey string, source *gateway.SessionSource) (any, error) {
	if err := busyAccepted(ctx); err != nil {
		return nil, err
	}
	text, err := provider.FormatEgressStatusText(home.Dir(ctx), false)
	if err != nil {
		return nil, err
	}
	return text, nil
}

// Hard stops the running turn and releases the session lock
func (r *GatewayRunner) busyStopCommand(ctx context.Context, event *gateway.MessageEvent, quickKey string, source *gateway.SessionSource) (any, error) {
	if err := busyAccepted(ctx); err != nil {
		return nil, err
	}
	if err := r.interruptAndClearSession(ctx, quickKey, source, interruptReasonStop, "stop_command", true); err != nil {
		return nil, err
	}
	runLog().Info(fmt.Sprintf("STOP for session %s — agent interrupted, session lock released", quickKey))
	return gateway.NewEphemeralReply(i18n.T(ctx, "gateway.stop.stopped", "", nil), nil), nil
}

// Queues the payload as its own full turn after the current run, never merged
func (r *GatewayRunner) busyQueueCommand(ctx context.Context, event *gateway.MessageEvent, quickKey string, source *gateway.SessionSource) (any, error) {
	queuedText := pystr.Strip(event.GetCommandArgs())
	hasMedia := len(event.MediaURLs) > 0
	if queuedText == "" && !hasMedia {
		return i18n.T(ctx, "gateway.queue.usage", "", nil), nil
	}
	if err := busyAccepted(ctx); err != nil {
		return nil, err
	}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return nil, err
	}
	queuedAck := i18n.T(ctx, "gateway.queue.queued", "", nil)
	if adapter != nil {
		queued := gateway.NewMessageEvent(queuedText)
		if hasMedia {
			queued.MessageType = event.MessageType
		}
		queued.Source, queued.RawMessage, queued.MessageID = event.Source, event.RawMessage, event.MessageID
		queued.MediaURLs = slices.Clone(event.MediaURLs)
		queued.MediaTypes = slices.Clone(event.MediaTypes)
		queued.MediaTextInlined = slices.Clone(event.MediaTextInlined)
		queued.MediaFilenames = slices.Clone(event.MediaFilenames)
		queued.ReplyToMessageID, queued.ReplyToText = event.ReplyToMessageID, event.ReplyToText
		queued.ReplyToAuthorID, queued.ReplyToAuthorName = event.ReplyToAuthorID, event.ReplyToAuthorName
		queued.ReplyToIsOwnMessage, queued.AutoSkill = event.ReplyToIsOwnMessage, event.AutoSkill
		queued.ChannelPrompt, queued.ChannelContext = event.ChannelPrompt, event.ChannelContext
		queued.Internal, queued.Timestamp = event.Internal, event.Timestamp
		committed, err := r.queueCommandTurn(ctx, quickKey, event, queued, adapter, queuedAck, nil)
		if err != nil {
			return nil, err
		}
		if committed {
			return nil, nil
		}
	}
	depth := r.queueDepth(quickKey, adapter)
	if depth > 1 {
		return queuedAck + i18n.T(ctx, "gateway.queue.queued_depth", "", map[string]any{"depth": depth}), nil
	}
	return queuedAck, nil
}

// Steers the running agent between tool iterations, queueing a follow up turn when no agent can take it
func (r *GatewayRunner) busySteerCommand(ctx context.Context, event *gateway.MessageEvent, quickKey string, source *gateway.SessionSource) (any, error) {
	steerText := pystr.Strip(event.GetCommandArgs())
	if steerText == "" {
		return i18n.T(ctx, "gateway.steer.usage", "", nil), nil
	}
	if err := busyAccepted(ctx); err != nil {
		return nil, err
	}
	runningAgent := r.runningSlotAgent(quickKey)
	queueFallback := func(reply string) (any, error) {
		adapter, err := r.deliveryAdapterFor(ctx, source)
		if err != nil {
			return nil, err
		}
		if adapter != nil {
			followUp := gateway.NewMessageEvent(steerText)
			followUp.Source, followUp.MessageID = event.Source, event.MessageID
			followUp.ChannelPrompt, followUp.ChannelContext = event.ChannelPrompt, event.ChannelContext
			committed, err := r.queueCommandTurn(ctx, quickKey, event, followUp, adapter, reply, nil)
			if err != nil {
				return nil, err
			}
			if committed {
				return nil, nil
			}
		}
		return reply, nil
	}
	if runningAgent == agentPendingSentinel {
		return queueFallback(i18n.T(ctx, "gateway.steer.queued_starting", "", nil))
	}
	if runningAgent == nil {
		return queueFallback(i18n.T(ctx, "gateway.steer.queued_no_agent", "", nil))
	}
	accepted, failure := func() (accepted bool, failure string) {
		defer func() {
			if rec := recover(); rec != nil {
				failure = pyrepr.Str(rec)
			}
		}()
		withOrigin, err := r.steerTextWithOrigin(ctx, steerText, event)
		if err != nil {
			return false, err.Error()
		}
		return steerRunningAgent(runningAgent, withOrigin), ""
	}()
	if failure != "" {
		runLog().Warn(fmt.Sprintf("Steer failed for session %s: %s", quickKey, failure))
		return i18n.T(ctx, "gateway.steer.failed", "", map[string]any{"error": failure}), nil
	}
	if !accepted {
		return i18n.T(ctx, "gateway.steer.rejected_empty", "", nil), nil
	}
	r.foldIntoRunningTurn(runningAgent, quickKey, event)
	preview := steerText
	if runes := []rune(steerText); len(runes) > 60 {
		preview = string(runes[:60]) + "..."
	}
	target := i18n.T(ctx, "gateway.steer.target_run", "", nil)
	if agentHasActiveSubagents(runningAgent) {
		target = i18n.T(ctx, "gateway.steer.target_subagents", "", nil)
	}
	return i18n.T(ctx, "gateway.steer.queued", "", map[string]any{"target": target, "preview": preview}), nil
}
