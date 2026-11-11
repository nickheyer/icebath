package gatewayrun

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Per session pending follow up cap for busy queue mode and the paths sharing its entry point
const busyQueueMaxPending = 32

// Text prefix of the synthetic goal continuation turns
const goalContinuationPrefix = "[Continuing toward your standing goal]\nGoal:"

// Metadata that must match for two pending events to merge into one slot
var securityMetadataKeys = []string{
	"hermes_plugin_id", "hermes_plugin_injection", "gateway_session_key",
	"gateway_session_id", "gateway_session_strict", "gateway_proven_session_id",
	"notification_category",
}

// Whether a busy message survives a restart in the queue, queue and steer modes during a requested restart
func (r *GatewayRunner) queueDuringDrainEnabled(busyInputMode string) bool {
	mode := busyInputMode
	if mode == "" {
		mode = r.busyInputMode
	}
	return r.restartRequested && (mode == "queue" || mode == "steer")
}

// Appends a queued event to the session FIFO, the empty pending slot first
func (r *GatewayRunner) enqueueFIFO(sessionKey string, queuedEvent *gateway.MessageEvent, adapter gateway.PlatformAdapter) {
	if adapter == nil {
		return
	}
	base := adapter.Base()
	if _, held := base.PeekPendingMessage(sessionKey); held {
		state := r.sessionState(sessionKey)
		state.conversation.queuedEvents = append(state.conversation.queuedEvents, queuedEvent)
	} else {
		base.SetPendingMessage(sessionKey, queuedEvent)
	}
	queuedEvent.SetGatewayAccepted(true)
}

// Promotes the overflow head after the slot drained, staging it in the slot when an event is already pending
func (r *GatewayRunner) promoteQueuedEvent(sessionKey string, adapter gateway.PlatformAdapter, pendingEvent *gateway.MessageEvent) *gateway.MessageEvent {
	overflow := r.overflowQueue(sessionKey)
	if overflow == nil || len(*overflow) == 0 {
		return pendingEvent
	}
	if pendingEvent == nil {
		head := (*overflow)[0]
		*overflow = slices.Delete(*overflow, 0, 1)
		return head
	}
	if adapter != nil {
		head := (*overflow)[0]
		*overflow = slices.Delete(*overflow, 0, 1)
		adapter.Base().SetPendingMessage(sessionKey, head)
	}
	return pendingEvent
}

// Hands the follow up the recursion cap refused back to the adapter FIFO at its head
func (r *GatewayRunner) requeueAtDepthCap(ctx context.Context, sessionKey string, source *gateway.SessionSource, pendingEvent *gateway.MessageEvent, pending string) error {
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return err
	}
	if adapter == nil || sessionKey == "" || (pendingEvent == nil && pending == "") {
		return nil
	}
	if pendingEvent == nil {
		prompt, pinnedSource := r.pinnedChannelInputs(sessionKey, nil, source, true)
		pendingEvent = gateway.NewMessageEvent(pending)
		pendingEvent.Source = pinnedSource
		pendingEvent.ChannelPrompt = prompt
	}
	r.requeueEventAtFront(sessionKey, pendingEvent, adapter)
	return nil
}

// Restores the event at the FIFO head, moving a staged next turn back to the front of overflow first
func (r *GatewayRunner) requeueEventAtFront(sessionKey string, event *gateway.MessageEvent, adapter gateway.PlatformAdapter) {
	if adapter == nil {
		return
	}
	base := adapter.Base()
	if staged, _ := base.PeekPendingMessage(sessionKey); staged != nil {
		state := r.sessionState(sessionKey)
		state.conversation.queuedEvents = slices.Insert(state.conversation.queuedEvents, 0, staged)
	}
	base.SetPendingMessage(sessionKey, event)
}

// Pops the first wake admitted before the drain began from the slot, else from the overflow FIFO
func (r *GatewayRunner) takeWakeAdmittedBeforeDrain(sessionKey string, adapter gateway.PlatformAdapter) *gateway.MessageEvent {
	if adapter != nil {
		base := adapter.Base()
		if staged, _ := base.PeekPendingMessage(sessionKey); staged != nil && staged.AdmittedBeforeDrain() {
			popped, _ := base.PopPendingMessage(sessionKey)
			return r.promoteQueuedEvent(sessionKey, adapter, popped)
		}
	}
	overflow := r.overflowQueue(sessionKey)
	if overflow == nil {
		return nil
	}
	for index, event := range *overflow {
		if event.AdmittedBeforeDrain() {
			*overflow = slices.Delete(*overflow, index, index+1)
			return event
		}
	}
	return nil
}

// Stripped pending steer text of an agent turn result, empty when the result carries none
func resultPendingSteer(result map[string]any) string {
	if result == nil {
		return ""
	}
	return pystr.Strip(pyrepr.Str(pyrepr.Or(result["pending_steer"], "")))
}

// Parks the queued event in the head slot so the fresh session after a compression exhausted reset runs it
func (r *GatewayRunner) deferPastExhaustionReset(result map[string]any, adapter gateway.PlatformAdapter, source *gateway.SessionSource, sessionKey string, pendingEvent *gateway.MessageEvent) {
	if leftoverSteer := resultPendingSteer(result); leftoverSteer != "" {
		steerPrompt, steerSource := r.pinnedChannelInputs(sessionKey, nil, source, true)
		state := r.sessionState(sessionKey)
		state.conversation.queuedEvents = slices.Insert(state.conversation.queuedEvents, 0, pendingEvent)
		pendingEvent = gateway.NewMessageEvent(leftoverSteer)
		pendingEvent.Source = steerSource
		pendingEvent.ChannelPrompt = steerPrompt
	}
	adapter.Base().SetPendingMessage(sessionKey, pendingEvent)
	runLog().Info(fmt.Sprintf("Deferring queued follow-up for session %s until after its compression-exhausted reset", sessionKey))
}

// Folds a leftover steer into the post turn drain, the next turn or ahead of or behind the pending event
func (r *GatewayRunner) mergeLeftoverSteer(result map[string]any, adapter gateway.PlatformAdapter, source *gateway.SessionSource, sessionKey string, pendingEvent *gateway.MessageEvent, pending string) (*gateway.MessageEvent, string) {
	leftoverSteer := resultPendingSteer(result)
	if leftoverSteer == "" {
		return pendingEvent, pending
	}
	switch {
	case pending == "" && pendingEvent == nil:
		pending = leftoverSteer
		runLog().Debug(fmt.Sprintf("Delivering leftover /steer as next turn: '%s...'", headRunes(pending, 40)))
	case pendingEvent != nil && adapter != nil && sessionKey != "":
		overflow := r.overflowQueue(sessionKey)
		base := adapter.Base()
		if promoted, _ := base.PeekPendingMessage(sessionKey); promoted != nil {
			if overflow != nil {
				*overflow = slices.Insert(*overflow, 0, promoted)
			} else {
				state := r.sessionState(sessionKey)
				state.conversation.queuedEvents = slices.Insert(state.conversation.queuedEvents, 0, promoted)
			}
		}
		base.SetPendingMessage(sessionKey, pendingEvent)
		pendingEvent = nil
		pending = leftoverSteer
		runLog().Debug(fmt.Sprintf("Delivering leftover /steer before queued event for session %s: '%s...'", sessionKey, headRunes(pending, 40)))
	case pending != "" && pendingEvent == nil:
		steerPrompt, steerSource := r.pinnedChannelInputs(sessionKey, nil, source, true)
		steerEvent := gateway.NewMessageEvent(leftoverSteer)
		steerEvent.Source = steerSource
		steerEvent.ChannelPrompt = steerPrompt
		if sessionKey != "" {
			r.enqueueFIFO(sessionKey, steerEvent, adapter)
		}
		key := sessionKey
		if key == "" {
			key = "?"
		}
		runLog().Debug(fmt.Sprintf("Enqueued leftover /steer behind pending message for session %s: '%s...'", key, headRunes(leftoverSteer, 40)))
	}
	return pendingEvent, pending
}

// Total pending queue items of a session, the slot plus the overflow
func (r *GatewayRunner) queueDepth(sessionKey string, adapter gateway.PlatformAdapter) int {
	depth := 0
	if overflow := r.overflowQueue(sessionKey); overflow != nil {
		depth = len(*overflow)
	}
	if adapter != nil {
		if _, held := adapter.Base().PeekPendingMessage(sessionKey); held {
			depth++
		}
	}
	return depth
}

// Pops the oldest orphaned overflow event of an idle session, staging the next one in the slot
func (r *GatewayRunner) rescueOrphanedOverflow(sessionKey string, adapter gateway.PlatformAdapter) *gateway.MessageEvent {
	overflow := r.overflowQueue(sessionKey)
	if overflow == nil || len(*overflow) == 0 || adapter == nil {
		return nil
	}
	base := adapter.Base()
	if occupied, _ := base.PeekPendingMessage(sessionKey); occupied != nil {
		return nil
	}
	head := (*overflow)[0]
	*overflow = slices.Delete(*overflow, 0, 1)
	if len(*overflow) > 0 {
		next := (*overflow)[0]
		*overflow = slices.Delete(*overflow, 0, 1)
		base.SetPendingMessage(sessionKey, next)
	}
	runLog().Warn(fmt.Sprintf("Rescued orphaned FIFO overflow event for idle session %s — it was queued during a busy window but the post-turn drain never promoted it (#99882)", sessionKey))
	if len(*overflow) > 0 {
		runLog().Warn(fmt.Sprintf("%d overflow event(s) still queued for session %s after rescue staging (will drain via normal promotion)", len(*overflow), sessionKey))
	}
	return head
}

// Whether an event or text is a synthetic goal continuation turn
func isGoalContinuationEvent(eventOrText any) bool {
	var text string
	switch v := eventOrText.(type) {
	case *gateway.MessageEvent:
		if v == nil {
			return false
		}
		text = v.Text
	case string:
		text = v
	case nil:
		return false
	default:
		text = pyrepr.Str(v)
	}
	return strings.HasPrefix(text, goalContinuationPrefix)
}

// Removes the queued synthetic goal continuations of a session and keeps real queued items
func (r *GatewayRunner) clearGoalPendingContinuations(sessionKey string, adapter gateway.PlatformAdapter) int {
	removed := 0
	if adapter != nil {
		base := adapter.Base()
		if pendingEvent, _ := base.PeekPendingMessage(sessionKey); isGoalContinuationEvent(pendingEvent) {
			base.PopPendingMessage(sessionKey)
			removed++
		}
	}
	if overflow := r.overflowQueue(sessionKey); overflow != nil && len(*overflow) > 0 {
		kept := []*gateway.MessageEvent{}
		for _, e := range *overflow {
			if !isGoalContinuationEvent(e) {
				kept = append(kept, e)
			}
		}
		removed += len(*overflow) - len(kept)
		r.peekSessionState(sessionKey).conversation.queuedEvents = kept
	}
	return removed
}

// Fresh session store check that the session's goal is still active before a queued continuation runs
func (r *GatewayRunner) goalStillActiveForSession(ctx context.Context, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	return goals.NewGoalManager(ctx, sessionID, 0).IsActive()
}

// Whether two pending events share the security context a merge into one slot needs
func sameSecurityContext(existing, event *gateway.MessageEvent) bool {
	if existing.Internal != event.Internal || existing.InheritChannelPin != event.InheritChannelPin ||
		existing.AllowGatewayControl != event.AllowGatewayControl {
		return false
	}
	for _, key := range securityMetadataKeys {
		if !pyrepr.Equal(existing.Metadata[key], event.Metadata[key]) {
			return false
		}
	}
	return true
}

// Queues a busy follow up as its own FIFO turn, merging only a photo burst into the head slot
func (r *GatewayRunner) queueOrReplacePendingEvent(ctx context.Context, sessionKey string, event *gateway.MessageEvent) error {
	adapter, err := r.deliveryAdapterFor(ctx, event.Source)
	if err != nil || adapter == nil {
		return err
	}
	base := adapter.Base()
	existing, _ := base.PeekPendingMessage(sessionKey)
	if existing != nil && sameSecurityContext(existing, event) {
		types := map[gateway.MessageType]bool{existing.MessageType: true, event.MessageType: true}
		onlyTextOrPhoto := true
		for kind := range types {
			if kind != gateway.MessageTypeText && kind != gateway.MessageTypePhoto {
				onlyTextOrPhoto = false
			}
		}
		if types[gateway.MessageTypePhoto] && onlyTextOrPhoto {
			base.MergePendingMessage(sessionKey, event, event.MessageType == gateway.MessageTypeText)
			event.SetGatewayAccepted(true)
			return nil
		}
	}
	if r.queueDepth(sessionKey, adapter) >= busyQueueMaxPending {
		runLog().Warn(fmt.Sprintf("Dropping busy-mode follow-up for session %s — pending queue at cap (%d).", sessionKey, busyQueueMaxPending))
		return nil
	}
	r.enqueueFIFO(sessionKey, event, adapter)
	return nil
}

// First n code points of a string, the Python slice of its head
func headRunes(s string, n int) string {
	if runes := []rune(s); len(runes) > n {
		return string(runes[:n])
	}
	return s
}
