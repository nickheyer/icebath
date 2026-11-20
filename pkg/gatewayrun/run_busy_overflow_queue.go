package gatewayrun

import (
	"slices"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Number of events in the overflow FIFO of the session, zero when no session state exists yet
func (r *GatewayRunner) overflowLen(sessionKey string) int {
	n := 0
	r.withPeekedSessionState(sessionKey, func(s *SessionState) { n = len(s.conversation.queuedEvents) })
	return n
}

// Appends events to the tail of the overflow FIFO under the state lock, creating the state when absent
func (r *GatewayRunner) appendOverflow(sessionKey string, events ...*gateway.MessageEvent) {
	r.withSessionState(sessionKey, func(s *SessionState) {
		s.conversation.queuedEvents = append(s.conversation.queuedEvents, events...)
	})
}

// Inserts event at the head of the overflow FIFO under the state lock, creating the state when absent
func (r *GatewayRunner) pushOverflowFront(sessionKey string, event *gateway.MessageEvent) {
	r.withSessionState(sessionKey, func(s *SessionState) {
		s.conversation.queuedEvents = slices.Insert(s.conversation.queuedEvents, 0, event)
	})
}

// Pops up to n head events in one hold of the state lock and returns them with the count still queued
func (r *GatewayRunner) popOverflow(sessionKey string, n int) ([]*gateway.MessageEvent, int) {
	var popped []*gateway.MessageEvent
	remaining := 0
	r.withPeekedSessionState(sessionKey, func(s *SessionState) {
		take := min(n, len(s.conversation.queuedEvents))
		popped = slices.Clone(s.conversation.queuedEvents[:take])
		s.conversation.queuedEvents = slices.Delete(s.conversation.queuedEvents, 0, take)
		remaining = len(s.conversation.queuedEvents)
	})
	return popped, remaining
}

// Removes and returns the first overflow event match accepts in one hold of the state lock, nil when none
func (r *GatewayRunner) takeOverflow(sessionKey string, match func(*gateway.MessageEvent) bool) *gateway.MessageEvent {
	var taken *gateway.MessageEvent
	r.withPeekedSessionState(sessionKey, func(s *SessionState) {
		for index, event := range s.conversation.queuedEvents {
			if match(event) {
				taken = event
				s.conversation.queuedEvents = slices.Delete(s.conversation.queuedEvents, index, index+1)
				return
			}
		}
	})
	return taken
}

// Drops every overflow event match accepts in one hold of the state lock and returns how many it dropped
func (r *GatewayRunner) dropOverflow(sessionKey string, match func(*gateway.MessageEvent) bool) int {
	removed := 0
	r.withPeekedSessionState(sessionKey, func(s *SessionState) {
		if len(s.conversation.queuedEvents) == 0 {
			return
		}
		kept := []*gateway.MessageEvent{}
		for _, event := range s.conversation.queuedEvents {
			if !match(event) {
				kept = append(kept, event)
			}
		}
		removed = len(s.conversation.queuedEvents) - len(kept)
		s.conversation.queuedEvents = kept
	})
	return removed
}
