package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/state"
)

// Whether the event is plain text with no media
func hmTextOnly(event *gateway.MessageEvent) bool {
	return event.MessageType == gateway.MessageTypeText && len(event.MediaURLs) == 0 && len(event.MediaTypes) == 0
}

// Steer mode injects the text mid run, falling back to queue semantics when the agent does not take it
func (r *GatewayRunner) hmBusySteer(ctx context.Context, event *gateway.MessageEvent, runningAgent *agent.AIAgent, quickKey string) error {
	steerText := pystr.Strip(event.Text)
	steered := false
	if hmTextOnly(event) && steerText != "" {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Warn(fmt.Sprintf("PRIORITY steer failed for session %s: %s", quickKey, pyrepr.Str(rec)))
				}
			}()
			withOrigin, err := r.steerTextWithOrigin(ctx, steerText, event)
			if err != nil {
				runLog().Warn(fmt.Sprintf("PRIORITY steer failed for session %s: %s", quickKey, err))
				return
			}
			steered = steerRunningAgent(runningAgent, withOrigin)
		}()
	}
	if steered {
		r.foldIntoRunningTurn(runningAgent, quickKey, event)
		runLog().Debug(fmt.Sprintf("PRIORITY steer for session %s", quickKey))
		return nil
	}
	runLog().Debug(fmt.Sprintf("PRIORITY steer-fallback-to-queue for session %s", quickKey))
	return r.queueOrReplacePendingEvent(ctx, quickKey, event)
}

// Redirects a text only correction when the runtime supports it, else interrupts the running agent
func (r *GatewayRunner) hmBusyInterrupt(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, runningAgent *agent.AIAgent, quickKey string) error {
	if hmTextOnly(event) && runningAgent.SupportsActiveTurnRedirect() {
		if r.redirectActiveTurn(ctx, runningAgent, pystr.Strip(event.Text), quickKey, event) {
			runLog().Debug(fmt.Sprintf("PRIORITY redirect for session %s", quickKey))
			return nil
		}
	}
	runLog().Debug(fmt.Sprintf("PRIORITY interrupt for session %s", quickKey))
	interruptText := event.Text
	if len(pendingEventAudioPaths(event)) > 0 {
		adapter, err := r.deliveryAdapterFor(ctx, source)
		if err != nil {
			return err
		}
		interruptText, _ = r.transcribeAndEchoPendingVoice(ctx, event, adapter, source, event.Text, "Voice-priority-interrupt", nil, false)
	} else if interruptText == "" && len(event.MediaURLs) > 0 {
		interruptText = buildMediaOnlyText(event)
	}
	runningAgent.Interrupt(interruptText)
	return nil
}

// Applies busy input mode to a follow up the durable FIFO is about to admit, true when it landed inside the running turn
func (r *GatewayRunner) hmBusyPreempt(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey string) (bool, error) {
	mode := r.effectiveBusyInputMode(ctx, source)
	busyState := r.peekSessionState(quickKey)
	var runningAgent *agent.AIAgent
	if busyState != nil {
		runningAgent = busyState.turn.agent
	}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return false, err
	}
	if r.draining.Load() || adapter == nil || runningAgent == nil || runningAgent == agentPendingSentinel ||
		(event.MessageType == gateway.MessageTypeText && mode != "steer" && r.effectiveBusyTextMode(ctx, source) == "queue") {
		return false, nil
	}
	outcome, err := r.resolveBusySteerOrRedirect(ctx, event, quickKey, mode, runningAgent)
	if err != nil {
		return false, err
	}
	observability.NoteBusyOutcome(ctx, gateway.BusyEventOf(event), observability.BusySteerOutcome{
		Steered: outcome.steered, Redirected: outcome.redirected, EffectiveMode: outcome.effectiveMode})
	if err := r.busyInterruptAndAck(ctx, event, quickKey, adapter, busyState, runningAgent, outcome); err != nil {
		return false, err
	}
	return outcome.steered || outcome.redirected, nil
}

// Hands the message to the routed profile's session authority when it owns it, false to continue in process
func (r *GatewayRunner) hmAuthorityAdmission(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey string, isInternal bool) (bool, any, error) {
	authority, err := gateway.ActiveAuthority(ctx, r)
	if err != nil {
		return false, nil, err
	}
	_, isCommand := event.GetCommand()
	if authority != nil && !isCommand && !isInternal && !gateway.ExecutingAdmission(ctx) {
		fifoOnly, _ := event.FIFOOnly()
		busy := r.isSessionRunning(quickKey) && !fifoOnly
		if busy {
			adapter, err := r.deliveryAdapterFor(ctx, source)
			if err != nil {
				return false, nil, err
			}
			observability.StampBusyArrival(adapter, gateway.BusyEventOf(event))
		}
		defer observability.SettleBusyArrival(ctx, gateway.BusyEventOf(event), false)
		return r.hmAdmitToFIFO(ctx, authority, event, source, quickKey, busy)
	}
	if authority == nil && !isInternal && !isCommand && r.sessionAuthority != nil {
		return false, nil, state.NewRuntimeStoreError("profile_mismatch", nil, nil)
	}
	return false, nil, nil
}

// Authority owned admission of a message outside a FIFO execution, busy follow ups preempting the running turn first
func (r *GatewayRunner) hmAdmitToFIFO(ctx context.Context, authority *gateway.SessionAuthority, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey string, busy bool) (bool, any, error) {
	if busy {
		if r.routePlaintextApprovalWhileBusy(ctx, event, quickKey) {
			return true, nil, nil
		}
		preempted, err := r.hmBusyPreempt(ctx, event, source, quickKey)
		if err != nil {
			return false, nil, err
		}
		if preempted {
			return true, nil, nil
		}
	}
	consumed, consumerReply, err := r.hmPostAdmissionConsume(ctx, event, source, quickKey)
	if err != nil {
		return false, nil, err
	}
	if consumed {
		if consumerReply == nil {
			return true, nil, nil
		}
		return true, *consumerReply, nil
	}
	if fifoOnly, stopEpoch := event.FIFOOnly(); fifoOnly {
		current, err := r.routeStopEpoch(ctx, quickKey)
		if err != nil {
			return false, nil, err
		}
		if stopEpoch != current {
			return true, nil, nil
		}
	}
	observability.NoteBusyAdmitting(ctx, gateway.BusyEventOf(event))
	reply, err := gateway.AdmitMessage(ctx, authority, event)
	if err != nil {
		return false, nil, err
	}
	return true, reply, nil
}
