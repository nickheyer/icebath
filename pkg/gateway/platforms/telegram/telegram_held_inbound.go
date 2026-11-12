package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"unicode/utf8"
	"weak"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Inbound events held across a disconnect window before the oldest is dropped
const HeldInboundMax = 64

// Adapter surface the held inbound queue reads, the TelegramAdapter methods the hermes mixin calls
type HeldInboundHost interface {
	gateway.PlatformAdapter
	IsPermanentFatal() bool
	ShouldDropDelayedDelivery() bool
	AcceptUpdate(ctx context.Context)
	HandleMessage(ctx context.Context, event *gateway.MessageEvent) error
}

// Hold, redispatch and hand over of inbound events PTB already acked that cannot dispatch yet
type HeldInbound struct {
	Max            int
	host           HeldInboundHost
	mu             sync.Mutex
	events         []*gateway.MessageEvent
	redispatchTask *gateway.AdapterTask
	successor      weak.Pointer[HeldInbound]
}

// Held inbound queue of the adapter host with the class default cap
func NewHeldInbound(host HeldInboundHost) *HeldInbound {
	return &HeldInbound{Max: HeldInboundMax, host: host}
}

// Snapshot of the events currently held in arrival order
func (h *HeldInbound) Events() []*gateway.MessageEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*gateway.MessageEvent(nil), h.events...)
}

// Replaces the held events, the assignment hermes tests make to the queue list
func (h *HeldInbound) SetEvents(events []*gateway.MessageEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append([]*gateway.MessageEvent(nil), events...)
}

// Empties the queue and returns how many events it discarded
func (h *HeldInbound) Clear() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := len(h.events)
	h.events = h.events[:0]
	return n
}

// Tracked drain task of the queue, nil before the first schedule
func (h *HeldInbound) RedispatchTask() *gateway.AdapterTask {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.redispatchTask
}

// Live replacement queue later holds forward to, nil when this queue owns its events
func (h *HeldInbound) Successor() *HeldInbound {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.successor.Value()
}

func eventChars(event *gateway.MessageEvent) int {
	if event == nil {
		return 0
	}
	return utf8.RuneCountInString(event.Text)
}

// Starts a tracked drain when events are held and delivery is live unless another drain is in flight
func (h *HeldInbound) ScheduleHeldInboundRedispatch(ctx context.Context) {
	if h.host.IsPermanentFatal() || h.host.ShouldDropDelayedDelivery() {
		return
	}
	current := gateway.CurrentAdapterTask(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.events) == 0 {
		return
	}
	prior := h.redispatchTask
	if prior != nil && !prior.Done() && prior != current {
		return
	}
	if prior == current {
		prior = nil
	}
	h.redispatchTask = gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error {
		return h.redispatchHeldInbound(tctx, prior)
	})
}

// Preserves an undispatchable inbound event capped and identity deduped while a permanent fatal discards it
func (h *HeldInbound) HoldInboundEvent(ctx context.Context, event *gateway.MessageEvent, where string, schedule bool) {
	if h.host.IsPermanentFatal() {
		slog.Warn(fmt.Sprintf("[Telegram] Discarding inbound under non-retryable fatal (%s, %d chars)", where, eventChars(event)))
		return
	}
	h.mu.Lock()
	target := h.successor.Value()
	h.mu.Unlock()
	if target != nil {
		h.host.AcceptUpdate(ctx)
		target.adoptHeldEvent(ctx, event, where+"-forwarded", schedule)
		return
	}
	h.mu.Lock()
	for _, existing := range h.events {
		if existing == event {
			h.mu.Unlock()
			return
		}
	}
	maxN := h.Max
	if maxN == 0 {
		maxN = HeldInboundMax
	}
	for len(h.events) >= maxN {
		if len(h.events) == 0 {
			h.mu.Unlock()
			panic("IndexError: pop from empty list")
		}
		dropped := h.events[0]
		h.events = append(h.events[:0:0], h.events[1:]...)
		slog.Warn(fmt.Sprintf("[Telegram] Held-inbound queue full (%d); dropping oldest (%d chars)", maxN, eventChars(dropped)))
	}
	h.events = append(h.events, event)
	queued := len(h.events)
	h.mu.Unlock()
	h.host.AcceptUpdate(ctx)
	dropping := h.host.ShouldDropDelayedDelivery()
	suffix := ""
	if dropping {
		suffix = " - will redispatch on reconnect"
	} else if schedule {
		suffix = " - scheduling redispatch"
	}
	slog.Warn(fmt.Sprintf("[Telegram] Holding inbound (%s, %d chars, queue=%d)%s", where, eventChars(event), queued, suffix))
	if schedule && !h.host.ShouldDropDelayedDelivery() {
		h.ScheduleHeldInboundRedispatch(ctx)
	}
}

// Adapter or queue whose held inbound a replacement adopts, the queue method promoted onto the embedding adapter
type heldInboundCarrier interface {
	HeldInboundQueue() *HeldInbound
}

var (
	_ gateway.HeldInboundAdopter = (*HeldInbound)(nil)
	_ heldInboundCarrier         = (*HeldInbound)(nil)
)

// The queue itself, so an adapter embedding it hands its queue to the adopting replacement
func (h *HeldInbound) HeldInboundQueue() *HeldInbound { return h }

// Takes over the hold queue of the replaced adapter and forwards its later holds here
func (h *HeldInbound) AdoptHeldInbound(ctx context.Context, source any) {
	h.mu.Lock()
	h.successor = weak.Pointer[HeldInbound]{}
	h.mu.Unlock()
	var events []*gateway.MessageEvent
	if carrier, ok := source.(heldInboundCarrier); ok {
		if predecessor := carrier.HeldInboundQueue(); predecessor != nil {
			predecessor.mu.Lock()
			predecessor.successor = weak.Make(h)
			events = append(events, predecessor.events...)
			predecessor.events = predecessor.events[:0]
			predecessor.mu.Unlock()
		}
	}
	for _, event := range events {
		h.adoptHeldEvent(ctx, event, "adopted", false)
	}
	h.ScheduleHeldInboundRedispatch(ctx)
}

// Holds a predecessor event here moving only its transport ref to this adapter
func (h *HeldInbound) adoptHeldEvent(ctx context.Context, event *gateway.MessageEvent, where string, schedule bool) {
	if event != nil && event.Source != nil {
		event.Source.SetTransportAdapterRef(h.host)
	}
	h.HoldInboundEvent(ctx, event, where, schedule)
}

// Re holds the events from idx on without rescheduling after an interrupted or failed drain
func (h *HeldInbound) reholdFrom(ctx context.Context, events []*gateway.MessageEvent, idx int, where string) {
	for _, rest := range events[idx:] {
		h.HoldInboundEvent(ctx, rest, where, false)
	}
}

// Drains the queue after a cancel and wait of the prior drain, re holding whatever could not dispatch
func (h *HeldInbound) redispatchHeldInbound(ctx context.Context, prior *gateway.AdapterTask) error {
	if prior != gateway.CurrentAdapterTask(ctx) {
		gateway.CancelTask(ctx, prior)
	}
	if h.host.IsPermanentFatal() {
		if n := h.Clear(); n > 0 {
			slog.Warn(fmt.Sprintf("[Telegram] Redispatch aborted; discarded %d held inbound under non-retryable fatal", n))
		}
		return nil
	}
	h.mu.Lock()
	if len(h.events) == 0 {
		h.mu.Unlock()
		return nil
	}
	events := append([]*gateway.MessageEvent(nil), h.events...)
	h.events = h.events[:0]
	h.mu.Unlock()
	slog.Warn(fmt.Sprintf("[Telegram] Redispatching %d held inbound message(s)", len(events)))
	allowFollowupSchedule := true
	defer func() {
		h.mu.Lock()
		pending := len(h.events) > 0
		h.mu.Unlock()
		if allowFollowupSchedule && pending && !h.host.ShouldDropDelayedDelivery() && !h.host.IsPermanentFatal() {
			h.ScheduleHeldInboundRedispatch(ctx)
		}
	}()
	for idx, event := range events {
		if h.host.IsPermanentFatal() || h.host.ShouldDropDelayedDelivery() {
			h.reholdFrom(ctx, events, idx, "redispatch-interrupted")
			return nil
		}
		err := h.host.HandleMessage(ctx, event)
		if err != nil && (ctx.Err() != nil || errors.Is(err, context.Canceled)) {
			h.reholdFrom(ctx, events, idx, "redispatch-cancelled")
			return err
		}
		if err != nil {
			slog.Error(fmt.Sprintf("[Telegram] Failed to redispatch held inbound (%d chars); re-holding", eventChars(event)), "error", err)
			h.reholdFrom(ctx, events, idx, "redispatch-failed")
			allowFollowupSchedule = false
			return nil
		}
	}
	return nil
}
