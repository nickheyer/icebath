package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/observability"
)

type busyAcceptedKey struct{}

// Context whose mid run dispatch awaits hook once it accepts a command and before it acts
func WithBusyAccepted(ctx context.Context, hook func(context.Context) error) context.Context {
	return context.WithValue(ctx, busyAcceptedKey{}, hook)
}

// Busy accepted hook a local session's pre command installed, nil on messaging
func BusyAcceptedHook(ctx context.Context) func(context.Context) error {
	hook, _ := ctx.Value(busyAcceptedKey{}).(func(context.Context) error)
	return hook
}

// Marks the event accepted by the gateway the way enqueuing it into the FIFO does
func (e *MessageEvent) SetGatewayAccepted(v bool) { e.setGatewayAccepted(v) }

// Whether the event is a wake its producer admitted before the drain began
func (e *MessageEvent) AdmittedBeforeDrain() bool { return e.admittedBeforeDrain }

// Event parked in a session's pending slot without taking it, and whether the slot holds a key
func (b *BasePlatformAdapter) PeekPendingMessage(sessionKey string) (*MessageEvent, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	event, ok := b.pendingMessages[sessionKey]
	return event, ok
}

// Takes the event parked in a session's pending slot, false when the slot holds no key
func (b *BasePlatformAdapter) PopPendingMessage(sessionKey string) (*MessageEvent, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	event, ok := b.pendingMessages[sessionKey]
	delete(b.pendingMessages, sessionKey)
	return event, ok
}

// Twelve character hex sha256 prefix of an identifier
func HashID(value string) string { return hashID(value) }

// Sender id rendered as user_ and its twelve hex hash
func HashSenderID(value string) string { return hashSenderID(value) }

// Chat id hashed after its platform prefix, the whole value hashed without one
func HashChatID(value string) string { return hashChatID(value) }

// Whether PII is redacted for the platform when the privacy config enables it
func ShouldRedactPII(ctx context.Context, platform Platform, enabled bool) bool {
	return shouldRedactPII(ctx, platform, enabled)
}

// Merges the event into the session's pending slot under the adapter lock, text joined when mergeText
func (b *BasePlatformAdapter) MergePendingMessage(sessionKey string, event *MessageEvent, mergeText bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	MergePendingMessageEvent(lazyAttr(&b.pendingMessages), sessionKey, event, mergeText)
}

// Whether the adapter class sets SUPPORTS_NATIVE_STREAMING, false for an adapter without the base class
func AdapterSupportsNativeStreaming(adapter PlatformAdapter) bool {
	return adapter != nil && adapter.Base() != nil && adapter.Base().Traits.SupportsNativeStreaming
}

// Text and seconds to live of a handler reply, an ephemeral reply unwrapped under the adapter's ttl rules
func (b *BasePlatformAdapter) UnwrapEphemeral(ctx context.Context, response any) (any, int) {
	return b.unwrapEphemeral(ctx, response)
}

// Whether a busy follow up already spent its bot loop budget on the busy path
func (e *MessageEvent) BotLoopAdmitted() bool { return e.botLoopAdmitted }

// Records that the busy path charged the bot loop budget for the event
func (e *MessageEvent) SetBotLoopAdmitted(v bool) { e.botLoopAdmitted = v }

// Whether the event is a command produced turn that only the authority FIFO may run, and its stop epoch
func (e *MessageEvent) FIFOOnly() (bool, int) { return e.fifoOnly, e.stopEpoch }

// Marks the event as a FIFO only turn admitted under the stop epoch
func (e *MessageEvent) SetFIFOOnly(stopEpoch int) { e.fifoOnly, e.stopEpoch = true, stopEpoch }

// Session id of the route read under the store lock, empty when the store holds no entry
func (s *SessionStore) LookupSessionIDUnderLock(ctx context.Context, sessionKey string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLoadedLocked(ctx)
	if entry := s.entries.get(sessionKey); entry != nil {
		return entry.SessionID
	}
	return ""
}

// Busy metrics view of a message event
func BusyEventOf(e *MessageEvent) observability.BusyEvent { return busyEventOf(e) }

// Admits a messaging input to the authority FIFO and waits for its turn's reply
func AdmitMessage(ctx context.Context, a *SessionAuthority, event *MessageEvent) (any, error) {
	return admitMessage(ctx, a, event)
}

// Commits a busy follow up to the authority FIFO, returning once its place in the chat order is fixed
func DispatchSharedBusy(ctx context.Context, adapter PlatformAdapter, event *MessageEvent, sessionKey string, durable bool) error {
	return dispatchSharedBusy(ctx, adapter, event, sessionKey, durable)
}
