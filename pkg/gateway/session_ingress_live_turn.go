package gateway

import (
	"context"
	"fmt"
	"sync"

	"github.com/nickheyer/icebath/pkg/tools"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Runner seams a producer delivery answers or redirects the chat's running turn through, run_inbound _hm_pending_reply_intercepts and _hm_busy_preempt and run_busy_steer _route_plaintext_approval_while_busy
type LiveTurnRunner interface {
	IsSessionRunning(route string) bool
	HasPendingSlashConfirm(route string) bool
	UpdatePromptPending(route string) bool
	HMPendingReplyIntercepts(ctx context.Context, event *MessageEvent, source *SessionSource, sessionKey string) (*string, error)
	RoutePlaintextApprovalWhileBusy(ctx context.Context, event *MessageEvent, sessionKey string) bool
	HMBusyPreempt(ctx context.Context, event *MessageEvent, source *SessionSource, sessionKey string) (bool, error)
}

const liveTurnConsumedMax = 512

// Deliveries a running turn or pending prompt consumed, never committed so the FIFO replay check cannot recognize a relay re-send of one
var (
	liveTurnConsumedMu sync.Mutex
	liveTurnConsumed   = newRelayOrderedSet()
)

// Runner seams of the authority's runner, an attribute error for a runner that cannot answer a running turn
func liveTurnRunner(runner AuthorityRunner) (LiveTurnRunner, error) {
	live, ok := runner.(LiveTurnRunner)
	if !ok {
		return nil, &PyError{Kind: "AttributeError", Msg: fmt.Sprintf("the gateway runner (%T) cannot answer a running turn", runner)}
	}
	return live, nil
}

// Whether the chat's in flight work could consume the event right now, a running turn, a pending clarify, slash confirm or update prompt
func mayAnswerLiveTurn(ctx context.Context, runner AuthorityRunner, event *MessageEvent) (bool, error) {
	live, err := liveTurnRunner(runner)
	if err != nil {
		return false, err
	}
	key, err := runnerSessionKeyForSource(ctx, runner, event.Source)
	if err != nil {
		return false, err
	}
	if live.IsSessionRunning(key) || tools.HasPendingClarify(key) || live.HasPendingSlashConfirm(key) {
		return true, nil
	}
	return live.UpdatePromptPending(key), nil
}

// Whether the event was consumed by the chat's in flight work and so is never admitted, a reply to send riding its consumer reply
func answersLiveTurn(ctx context.Context, runner AuthorityRunner, event *MessageEvent) (bool, error) {
	if _, isCommand := event.GetCommand(); isCommand || event.Internal || event.Source.IsBot {
		return false, nil
	}
	replay, hasReplay, err := liveTurnReplayKey(event)
	if err != nil {
		return false, err
	}
	consumed, err := routeLiveTurn(ctx, runner, event)
	if err != nil {
		return false, err
	}
	if consumed && hasReplay {
		liveTurnConsumedMu.Lock()
		liveTurnConsumed.add(replay)
		liveTurnConsumed.evict(liveTurnConsumedMax)
		liveTurnConsumedMu.Unlock()
	}
	return consumed, nil
}

// Profile, platform, chat and provider message id of a delivery as one key, false for a delivery without a message id
func liveTurnReplayKey(event *MessageEvent) (string, bool, error) {
	if event.MessageID == nil || *event.MessageID == "" {
		return "", false, nil
	}
	var profile any
	if event.Source.Profile != nil {
		profile = *event.Source.Profile
	}
	key, err := jsonutil.Encode([]any{profile, string(event.Source.Platform), event.Source.ChatID, *event.MessageID}, jsonutil.Spaced(), jsonutil.ASCII())
	if err != nil {
		return "", false, err
	}
	return key, true, nil
}

// Whether the event is a re-send of a delivery already consumed here, whether or not the work that consumed it still runs
func wasConsumed(event *MessageEvent) (bool, error) {
	replay, hasReplay, err := liveTurnReplayKey(event)
	if err != nil || !hasReplay {
		return false, err
	}
	liveTurnConsumedMu.Lock()
	defer liveTurnConsumedMu.Unlock()
	return liveTurnConsumed.has(replay), nil
}

// Offers the event to a pending prompt, then to a blocking approval and the busy input mode of the running turn
func routeLiveTurn(ctx context.Context, runner AuthorityRunner, event *MessageEvent) (bool, error) {
	live, err := liveTurnRunner(runner)
	if err != nil {
		return false, err
	}
	source := event.Source
	key, err := runnerSessionKeyForSource(ctx, runner, source)
	if err != nil {
		return false, err
	}
	reply, err := live.HMPendingReplyIntercepts(ctx, event, source, key)
	if err != nil {
		return false, err
	}
	if reply != nil {
		event.consumerReply = nil
		if *reply != "" {
			event.consumerReply = reply
		}
		return true, nil
	}
	if !live.IsSessionRunning(key) {
		return false, nil
	}
	if live.RoutePlaintextApprovalWhileBusy(ctx, event, key) {
		return true, nil
	}
	return live.HMBusyPreempt(ctx, event, source, key)
}
