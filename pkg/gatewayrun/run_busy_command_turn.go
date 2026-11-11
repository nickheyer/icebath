package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Whether a turn a slash command produced goes to the authority FIFO instead of staying in process
func (r *GatewayRunner) commandTurnAdmitted(ctx context.Context, event *gateway.MessageEvent, sessionKey string) bool {
	if gateway.ActiveAuthority(r) == nil || gateway.ExecutingAdmission(ctx) || event.Internal {
		return false
	}
	if _, isCommand := event.GetCommand(); isCommand {
		return false
	}
	state := r.peekSessionState(sessionKey)
	return !(state != nil && len(state.conversation.oneTurnRestore) > 0)
}

// How many messaging stop and new commands fenced the route, the authority's per route epoch
func (r *GatewayRunner) routeStopEpoch(sessionKey string) int {
	if authority := gateway.ActiveAuthority(r); authority != nil {
		return authority.RouteEpoch(sessionKey)
	}
	return 0
}

// Marks a command produced event as a plain text FIFO only turn admitted under the stop epoch
func asFIFOTurn(event *gateway.MessageEvent, stopEpoch int) *gateway.MessageEvent {
	if event.MessageType == gateway.MessageTypeCommand {
		event.MessageType = gateway.MessageTypeText
	}
	event.SetFIFOOnly(stopEpoch)
	return event
}

// Inline admission of a command produced turn, false with no reply to stay in process
func (r *GatewayRunner) admitCommandTurn(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, sessionKey string, stopEpoch int) (bool, any, error) {
	if !r.commandTurnAdmitted(ctx, event, sessionKey) {
		return false, nil, nil
	}
	return r.hmAuthorityAdmission(ctx, asFIFOTurn(event, stopEpoch), source, sessionKey, false)
}

// Queues a command produced turn after the current run, true when committed in the background with its ack after commit
func (r *GatewayRunner) queueCommandTurn(ctx context.Context, sessionKey string, command, event *gateway.MessageEvent, adapter gateway.PlatformAdapter, ack string, stopEpoch *int) (bool, error) {
	if !r.commandTurnAdmitted(ctx, event, sessionKey) {
		r.enqueueFIFO(sessionKey, event, adapter)
		return false, nil
	}
	gateway.CarryCallback(ctx, command, event)
	epoch := r.routeStopEpoch(sessionKey)
	if stopEpoch != nil {
		epoch = *stopEpoch
	}
	if ack != "" {
		sendCtx := context.WithoutCancel(ctx)
		event.OnCommit = func() { r.sendAfterCommit(sendCtx, adapter, event, ack) }
	}
	if err := gateway.DispatchSharedBusy(ctx, adapter, asFIFOTurn(event, epoch), sessionKey, false); err != nil {
		return false, err
	}
	return true, nil
}

// Sends the queued turn acknowledgement as an adapter background task once its row commits
func (r *GatewayRunner) sendAfterCommit(ctx context.Context, adapter gateway.PlatformAdapter, event *gateway.MessageEvent, text string) {
	task := gateway.SpawnAdapterTask(ctx, func(ctx context.Context) error {
		if err := r.sendBusyReply(ctx, event, adapter, text, false); err != nil {
			runLog().Warn(fmt.Sprintf("Queued-turn acknowledgement failed: %s", err))
		}
		return nil
	})
	adapter.Base().TrackBackgroundTask(task)
}

// Mid run skill or bundle command under a session authority rewritten and queued as a turn, false leaves it to the busy path
func (r *GatewayRunner) busySkillTurn(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, sessionKey string, adapter gateway.PlatformAdapter) (bool, any, error) {
	command, _ := event.GetCommand()
	if command == "" || event.Internal || gateway.ActiveAuthority(r) == nil || gateway.ExecutingAdmission(ctx) {
		return false, nil, nil
	}
	if _, quick := r.hmQuickCommands()[command]; quick || IsGatewayKnownCommand(command) {
		return false, nil, nil
	}
	epochKey, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return false, nil, err
	}
	epoch := r.routeStopEpoch(epochKey)
	type rewritten struct {
		skill bool
		reply *string
	}
	out, err := r.runInExecutorWithContext(ctx, func(ctx context.Context) (any, error) {
		known, err := skillOrBundleCommand(ctx, command)
		if err != nil || !known {
			return rewritten{}, err
		}
		return rewritten{skill: true, reply: r.hmSkillSlashRewrite(ctx, event, source, sessionKey, command)}, nil
	})
	if err != nil {
		return false, nil, err
	}
	result := out.(rewritten)
	if result.reply != nil {
		return result.skill, *result.reply, nil
	}
	if !result.skill {
		return false, nil, nil
	}
	if adapter == nil {
		return r.admitCommandTurn(ctx, event, source, sessionKey, epoch)
	}
	if !r.commandTurnAdmitted(ctx, event, sessionKey) {
		return false, nil, nil
	}
	if _, err := r.queueCommandTurn(ctx, sessionKey, event, event, adapter, "", &epoch); err != nil {
		return false, nil, err
	}
	return true, nil, nil
}

// Whether a typed command names a skill bundle or a skill command
func skillOrBundleCommand(ctx context.Context, command string) (bool, error) {
	scope := skills.NewScope(home.Dir(ctx), nil)
	if _, bundle, err := scope.ResolveBundleCommandKey(command); err != nil || bundle {
		return bundle, err
	}
	_, skill, err := scope.ResolveSkillCommandKey(ctx, command, false)
	return skill, err
}
