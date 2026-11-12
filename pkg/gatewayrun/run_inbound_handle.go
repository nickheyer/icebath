package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
)

// Inbound message of any platform through auth, commands, the running agent fast path and a claimed agent turn
func (r *GatewayRunner) handleMessage(ctx context.Context, event *gateway.MessageEvent) (any, error) {
	ctx, admitted, err := r.hmAdmitEvent(ctx, event)
	if err != nil || admitted == nil {
		return nil, err
	}
	event, source, isInternal := admitted.event, admitted.source, admitted.internal
	if !isInternal {
		command, _ := event.GetCommand()
		observability.RecordGatewaySlashCommand(ctx, command, event.SlashCommandCounted())
	}
	if notice, paused := r.hmEstopGate(ctx, event, source, isInternal); paused {
		return notice, nil
	}
	quickKey, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return nil, err
	}
	if !isInternal {
		reply, err := r.hmPendingReplyIntercepts(ctx, event, source, quickKey)
		if err != nil {
			return nil, err
		}
		if reply != nil {
			return *reply, nil
		}
	}
	owned, authorityReply, err := r.hmAuthorityAdmission(ctx, event, source, quickKey, isInternal)
	if err != nil {
		return nil, err
	}
	if owned {
		return authorityReply, nil
	}
	r.hmEvictIdleStaleAgent(ctx, quickKey)
	if r.isSessionRunning(quickKey) {
		r.hmEvictReapedAgent(ctx, quickKey)
	}
	if r.isSessionRunning(quickKey) {
		return r.hmHandleRunningSessionMessage(ctx, event, source, quickKey)
	}
	handled, result, err := r.hmDispatchIdleCommands(ctx, event, source, quickKey)
	if err != nil {
		return nil, err
	}
	if handled {
		return result, nil
	}
	if !isInternal {
		if r.isTelegramTopicRootLobby(ctx, source) {
			if r.shouldSendTelegramLobbyReminder(source) {
				return r.telegramTopicRootLobbyMessage(ctx), nil
			}
			return nil, nil
		}
		if r.externalDrainActive {
			runLog().Info(fmt.Sprintf("Refusing new turn for session %s — external drain active.", quickKey))
			return i18n.T(ctx, "gateway.busy.draining_maintenance", "", nil), nil
		}
	}
	lease, refusal := r.claimActiveSessionSlot(ctx, quickKey, source)
	if refusal != nil {
		runLog().Info(fmt.Sprintf("Rejecting new active session %s: max_concurrent_sessions reached", quickKey))
		return refusal.Message, nil
	}
	event, source, isInternal = r.hmRescueOrphanedFIFO(ctx, event, source, isInternal, quickKey)
	runGeneration := 0
	r.withSessionState(quickKey, func(s *SessionState) {
		if lease != nil {
			s.turn.lease = lease
		}
		s.turn.agent = agentPendingSentinel
		s.turn.event = event
		s.turn.startedTS = float64(time.Now().UnixNano()) / 1e9
		if quickKey != "" {
			runGeneration = s.bumpRunGeneration()
		}
	})
	observability.StartReplyClock(ctx, source, isInternal)
	r.persistActiveAgents(ctx)
	defer func() {
		r.restorePendingOneTurnModelOverride(ctx, quickKey, &runGeneration)
		if !event.TurnMarkerHandoff() {
			r.clearDurableActiveTurn(ctx, event)
		}
		r.releaseRunningAgentState(ctx, quickKey, &runGeneration)
		r.releaseTurnLease(quickKey, runGeneration)
	}()
	if !isInternal {
		consumed, consumerReply, err := r.hmPostAdmissionConsume(ctx, event, source, quickKey)
		if err != nil {
			return nil, err
		}
		if consumed {
			if consumerReply == nil {
				return nil, nil
			}
			return *consumerReply, nil
		}
	}
	agentResult, err := r.handleMessageWithAgent(ctx, event, source, quickKey, runGeneration)
	if err != nil {
		var leaseErr *TurnLeaseTimeoutError
		if errors.As(err, &leaseErr) {
			runLog().Error(fmt.Sprintf("Rejecting turn for routing key %s on session %s after turn-lease timeout; transcript load was not started and the user must resend",
				quickKey, leaseErr.SessionID))
			return i18n.T(ctx, "gateway.busy.another_turn_running", "", nil), nil
		}
		return nil, err
	}
	var reply any
	if agentResult != nil {
		reply = *agentResult
	}
	if err := r.runPostTurnHooks(ctx, reply, source, isInternal, event); err != nil {
		runLog().Debug(fmt.Sprintf("post-turn hook failed: %s", err))
	}
	return reply, nil
}

// Fast path while the session's agent runs, interrupting by default with queue, steer and protection demotions
func (r *GatewayRunner) hmHandleRunningSessionMessage(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey string) (any, error) {
	command, _ := event.GetCommand()
	if event.Internal && command == "" {
		return nil, r.queueOrReplacePendingEvent(ctx, quickKey, event)
	}
	handled, result, err := r.hmBusySlashOrPhoto(ctx, event, source, quickKey)
	if err != nil {
		return nil, err
	}
	if !handled {
		if handled, result, err = r.busySkillTurn(ctx, event, source, quickKey, nil); err != nil {
			return nil, err
		}
	}
	if handled {
		return result, nil
	}
	mode := r.effectiveBusyInputMode(ctx, source)
	if queued, err := r.hmBusyTelegramGraceQueue(ctx, event, source, quickKey, mode); queued || err != nil {
		return nil, err
	}
	runningAgent := r.runningSlotAgent(quickKey)
	for runningAgent == agentPendingSentinel && command == "stop" {
		if r.releaseRunningSlot(ctx, quickKey, nil, agentPendingSentinel) {
			runLog().Info(fmt.Sprintf("HARD STOP (pending) for session %s — sentinel cleared", quickKey))
			return gateway.NewEphemeralReply(i18n.T(ctx, "gateway.stop.force_stopped_pending", "", nil), nil), nil
		}
		runningAgent = r.runningSlotAgent(quickKey)
	}
	if runningAgent == agentPendingSentinel {
		return nil, r.hmMergePendingForSource(ctx, source, quickKey, event, true)
	}
	if r.draining.Load() {
		queueDuringDrain := r.queueDuringDrainEnabled(mode)
		if queueDuringDrain {
			if err := r.queueOrReplacePendingEvent(ctx, quickKey, event); err != nil {
				return nil, err
			}
			return i18n.T(ctx, "gateway.busy.drain_queued", "", map[string]any{"action": r.statusActionGerund(ctx)}), nil
		}
		return i18n.T(ctx, "gateway.busy.drain_rejected", "", map[string]any{"action": r.statusActionGerund(ctx)}), nil
	}
	if mode == "queue" {
		runLog().Debug(fmt.Sprintf("PRIORITY queue follow-up for session %s", quickKey))
		return nil, r.queueOrReplacePendingEvent(ctx, quickKey, event)
	}
	if mode == "steer" {
		return nil, r.hmBusySteer(ctx, event, runningAgent, quickKey)
	}
	demote := ""
	switch {
	case agentHasActiveSubagents(runningAgent):
		demote = "because the running agent has active subagents (#30170)"
	case r.sessionHasCompressionInFlight(ctx, quickKey):
		demote = "because context compression is in flight (#56391)"
	default:
		return nil, r.hmBusyInterrupt(ctx, event, source, runningAgent, quickKey)
	}
	runLog().Info(fmt.Sprintf("PRIORITY interrupt demoted to queue for session %s %s", quickKey, demote))
	return nil, r.queueOrReplacePendingEvent(ctx, quickKey, event)
}

// Slash command and photo burst handling on the busy fast path, each command per its declared busy policy
func (r *GatewayRunner) hmBusySlashOrPhoto(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey string) (bool, any, error) {
	command, _ := event.GetCommand()
	var def busyCommandDef
	known := false
	if command != "" {
		def.name, def.busyPolicy, def.busyHandler, known = ResolveCommandBusy(command)
	}
	if known {
		switch def.name {
		case "status":
			reply, err := handlerOf(r.handleStatusCommand)(ctx, event)
			return true, reply, err
		case "context":
			reply, err := handlerOf(r.handleContextCommand)(ctx, event)
			return true, reply, err
		}
		if denied := r.checkSlashAccess(ctx, source, def.name); denied != "" {
			return true, denied, nil
		}
		if def.name == "stop" || def.name == "new" {
			if err := r.hmStopOwnerTurn(ctx, quickKey); err != nil {
				return true, nil, err
			}
		}
		reply, err := r.dispatchBusySlashCommand(ctx, event, def, quickKey, source)
		return true, reply, err
	}
	if event.MessageType == gateway.MessageTypePhoto {
		runLog().Debug(fmt.Sprintf("PRIORITY photo follow-up for session %s — queueing without interrupt", quickKey))
		return true, nil, r.hmMergePendingForSource(ctx, source, quickKey, event, false)
	}
	return false, nil, nil
}

// Idle path command resolution and dispatch, a command rewritten into a turn joining the authority's FIFO
func (r *GatewayRunner) hmDispatchIdleCommands(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey string) (bool, any, error) {
	stopEpoch, err := r.routeStopEpoch(ctx, quickKey)
	if err != nil {
		return false, nil, err
	}
	handled, resolved, command, canonical := r.hmResolveCommand(ctx, event, source, quickKey)
	var result any
	if resolved != nil {
		result = *resolved
	}
	if !handled {
		if handled, result, err = r.hmDispatchCanonicalCommand(ctx, event, source, quickKey, canonical); err != nil {
			return false, nil, err
		}
	}
	if !handled {
		var reply *string
		if handled, reply, command, err = r.hmDispatchQuickAndPluginCommands(ctx, event, source, command); err != nil {
			return false, nil, err
		}
		result = nil
		if reply != nil {
			result = *reply
		}
	}
	if !handled {
		rewritten, err := r.runInExecutorWithContext(ctx, func(ctx context.Context) (any, error) {
			return r.hmSkillSlashRewrite(ctx, event, source, quickKey, command), nil
		})
		if err != nil {
			return false, nil, err
		}
		result = nil
		if text, ok := rewritten.(*string); ok && text != nil {
			result, handled = *text, true
		}
	}
	if !handled && command != "" {
		if handled, result, err = r.admitCommandTurn(ctx, event, source, quickKey, stopEpoch); err != nil {
			return false, nil, err
		}
	}
	return handled, result, nil
}

// Built in idle path commands, prompt rewriting ones changing the event text and falling through to the agent
func (r *GatewayRunner) hmDispatchCanonicalCommand(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey, canonical string) (bool, any, error) {
	handler := r.gatewayPlainCommandHandlers()[canonical]
	if handler == nil {
		handler = r.gatewayIdleCommandHandlers()[canonical]
	}
	if handler != nil {
		scoped, err := r.asyncProfileScopeForSource(ctx, source)
		if err != nil {
			return true, nil, err
		}
		reply, err := handler(scoped, event)
		return true, reply, err
	}
	switch canonical {
	case "new":
		return r.hmCmdNew(ctx, event, source, quickKey)
	case "start":
		return r.hmCmdStart(ctx, event, source, quickKey)
	case "egress":
		return r.hmCmdEgress(ctx, event, source, quickKey)
	case "learn":
		return r.hmCmdLearn(ctx, event, source, quickKey)
	case "plan":
		return r.hmCmdPlan(ctx, event, source, quickKey)
	case "init":
		return r.hmCmdInit(ctx, event, source, quickKey)
	case "blueprint":
		return r.hmCmdBlueprint(ctx, event, source, quickKey)
	case "undo":
		return r.hmCmdUndo(ctx, event, source, quickKey)
	case "queue":
		return r.hmCmdQueue(ctx, event, source, quickKey)
	case "steer":
		return r.hmCmdSteer(ctx, event, source, quickKey)
	case "moa":
		return r.hmCmdMoa(ctx, event, source, quickKey)
	}
	return false, nil, nil
}

// Runs the oldest orphaned overflow event as this turn and parks the incoming event behind the chain
func (r *GatewayRunner) hmRescueOrphanedFIFO(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, isInternal bool, quickKey string) (outEvent *gateway.MessageEvent, outSource *gateway.SessionSource, outInternal bool) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Debug(fmt.Sprintf("FIFO orphan rescue pre-claim failed for %s", quickKey), "error", pyrepr.Str(rec))
			outEvent, outSource, outInternal = event, source, isInternal
		}
	}()
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		runLog().Debug(fmt.Sprintf("FIFO orphan rescue pre-claim failed for %s", quickKey), "error", err)
		return event, source, isInternal
	}
	command, _ := event.GetCommand()
	if adapter == nil || event.Internal || command != "" {
		return event, source, isInternal
	}
	rescued := r.rescueOrphanedOverflow(quickKey, adapter)
	if rescued == nil {
		return event, source, isInternal
	}
	r.enqueueFIFO(quickKey, event, adapter)
	if rescued.Source != nil {
		source = rescued.Source
	}
	return rescued, source, rescued.Internal
}

// Runs a destructive command behind the confirm gate, always handled
func (r *GatewayRunner) hmConfirmDestructive(ctx context.Context, event *gateway.MessageEvent, command, detail string, handler commandHandler) (bool, any, error) {
	reply, err := r.maybeConfirmDestructiveSlash(ctx, event, command, "/"+command, detail, func(ctx context.Context) (any, error) {
		return handler(ctx, event)
	})
	return true, reply, err
}

// Resets the session behind the confirm gate, the parallel chat hint in a Telegram root lobby
func (r *GatewayRunner) hmCmdNew(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey string) (bool, any, error) {
	if r.isTelegramTopicRootLobby(ctx, source) {
		return true, r.telegramTopicRootNewMessage(ctx), nil
	}
	return r.hmConfirmDestructive(ctx, event, "new", i18n.T(ctx, "gateway.confirm.detail_new", "", nil), handlerOf(r.handleResetCommand))
}

// Turns an undo argument asks for, one when absent, malformed or below one
func undoCount(raw string) *big.Int {
	one := big.NewInt(1)
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return one
	}
	n, ok := pyint.Parse(fields[0])
	if !ok || n.Cmp(one) < 0 {
		return one
	}
	return n
}

// Undoes the last turns behind the confirm gate
func (r *GatewayRunner) hmCmdUndo(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, quickKey string) (bool, any, error) {
	n := undoCount(pystr.Strip(event.GetCommandArgs()))
	detail := i18n.T(ctx, "gateway.confirm.detail_undo_one", "", nil)
	if n.Cmp(big.NewInt(1)) != 0 {
		detail = i18n.T(ctx, "gateway.confirm.detail_undo_many", "", map[string]any{"count": pyint.Native(n)})
	}
	return r.hmConfirmDestructive(ctx, event, "undo", detail, handlerOf(r.handleUndoCommand))
}
