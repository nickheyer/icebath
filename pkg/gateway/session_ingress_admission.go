package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/state"
)

type admissionAuthorKey struct{}

type executingAdmissionKey struct{}

type viewerTurnKey struct{}

// Author of the input the executing admission carries, nil outside an execution
func admissionAuthorOf(ctx context.Context) any {
	return ctx.Value(admissionAuthorKey{})
}

// True inside the execution of a claimed admission
func executingAdmissionOf(ctx context.Context) bool {
	executing, _ := ctx.Value(executingAdmissionKey{}).(bool)
	return executing
}

// Live session of the executing turn when its own viewer submitted it, nil otherwise
func viewerTurnOf(ctx context.Context) *LiveSession {
	live, _ := ctx.Value(viewerTurnKey{}).(*LiveSession)
	return live
}

// Validation clauses about a message's own content that are safe to name to the sender's chat
var contentClauses = map[string]bool{
	"text": true, "auto_skill": true, "channel_prompt": true, "channel_context": true, "reply_expected": true,
	"media_urls": true, "media_types": true, "media_text_inlined": true, "media_types_length": true, "media_text_inlined_length": true,
	"media_filenames": true, "media_filenames_length": true,
}

// Reply to an input the full queue refused before anything was committed
const queueFullReply = "⏳ Too many messages are already waiting in this conversation; " +
	"send this again after the current reply."

// Reply a settled turn owes its chat, kept until it is sent or dropped
type settledDelivery struct {
	adapter  PlatformAdapter
	event    *MessageEvent
	route    string
	response any
	home     *string
}

// Post delivery slot moved off its adapter onto the admission that settles it
type postDeliveryTake struct {
	base  *BasePlatformAdapter
	route string
	entry postDeliveryEntry
}

// Admits a messaging input and waits for its turn's reply, a refusal answered with a notice for the chat
func admitMessage(ctx context.Context, a *SessionAuthority, event *MessageEvent) (any, error) {
	accepted, committed := busyAdmissionOf(ctx)
	ctx = context.WithValue(ctx, busyAdmissionKey{}, (*busyAdmission)(nil))
	receipt, err := a.AdmitNative(context.WithValue(ctx, busyAdmissionKey{}, &busyAdmission{accepted: accepted}), event)
	if err != nil {
		var refusal *state.RuntimeStoreError
		if !errors.As(err, &refusal) {
			return nil, err
		}
		field := ""
		if refusal.Field != nil {
			field = *refusal.Field
		}
		if refusal.Reason == "stale_generation" && field == "route" {
			return nil, nil
		}
		if refusal.Reason == "invalid_params" && field != "" {
			named := refusal.Reason
			if contentClauses[field] {
				named = refusal.Reason + ": " + field
			}
			return "⚠️ Hermes refused this message (" + named + "), so it was not saved " +
				"or answered. Ask the operator to check gateway.log.", nil
		}
		if refusal.Reason != "queue_full" {
			return nil, err
		}
		return queueFullReply, nil
	}
	if committed != nil && !committed.isDone() {
		committed.settle(nil, false)
	}
	if receipt.Status == AdmissionTerminal {
		return nil, nil
	}
	admissionID := receipt.AdmissionID
	handoff := event.TurnMarkerHandoff()
	a.mu.Lock()
	a.nativeWaiters[admissionID] = true
	waiter := a.waiters[admissionID]
	if waiter == nil {
		waiter = newAdmissionWaiter()
		a.waiters[admissionID] = waiter
	}
	if handoff {
		markerAdopters(a)[admissionID] = event
	}
	a.mu.Unlock()
	dropAdopter := func() {
		a.mu.Lock()
		if markerAdopters(a)[admissionID] == event {
			delete(a.markerAdopters, admissionID)
		}
		a.mu.Unlock()
	}
	row, err := a.DB.GetSessionAdmission(ctx, admissionID)
	if err != nil {
		return nil, err
	}
	status := AdmissionTerminal
	var outcome any
	if row != nil {
		status = rowString(row, "status")
		outcome, _ = row.Get("outcome")
	}
	observability.NoteBusyAdmitted(ctx, busyEventOf(event), admissionID, status, outcome)
	if status == AdmissionTerminal || status == AdmissionUnknown {
		a.mu.Lock()
		delete(a.nativeWaiters, admissionID)
		if a.waiters[admissionID] == waiter {
			delete(a.waiters, admissionID)
		}
		a.mu.Unlock()
		dropAdopter()
		if status == AdmissionTerminal {
			return nil, nil
		}
		return pauseNotice(a, receipt.Ref, "unknown_execution")
	}
	if accepted != nil && !accepted.isDone() {
		accepted.settle(nil, false)
	}
	defer dropAdopter()
	reply, err := waiter.wait(ctx)
	if err == nil {
		return reply, nil
	}
	var refusal *state.RuntimeStoreError
	if !errors.As(err, &refusal) {
		return nil, err
	}
	row, err = a.DB.GetSessionAdmission(ctx, admissionID)
	if err != nil {
		return nil, err
	}
	if row != nil && rowString(row, "status") == AdmissionTerminal {
		return "⚠️ Hermes did not answer this message (" + refusal.Reason + "): the conversation it was " +
			"queued for moved, so it was dropped. Send it again to continue.", nil
	}
	if row != nil {
		observability.NoteBusyParked(busyEventOf(event))
	}
	return pauseNotice(a, receipt.Ref, refusal.Reason)
}

// Events adopting a turn's crash marker keyed by admission, read and written under the authority lock
func markerAdopters(a *SessionAuthority) map[string]*MessageEvent {
	return lazyAttr(&a.markerAdopters)
}

// Hands the executed turn's crash marker to its adopter or the drain, released when no reply is owed
func handOverTurnMarker(ctx context.Context, a *SessionAuthority, admissionID string, event *MessageEvent) error {
	a.mu.Lock()
	if adopter := markerAdopters(a)[admissionID]; adopter != nil {
		a.mu.Unlock()
		moveMarker(event, adopter)
		return nil
	}
	if _, owed := a.pendingDeliveries[admissionID]; owed {
		heldMarkers(a)[admissionID] = event
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()
	return releaseAdmissionTurnMarker(ctx, a, event)
}

// Events holding a crash marker until the drain delivers their reply, read and written under the authority lock
func heldMarkers(a *SessionAuthority) map[string]*MessageEvent {
	return lazyAttr(&a.heldTurnMarkers)
}

// Named delivery table of the authority created on first use, read and written under the authority lock
func deliveryRegistry(table *map[string]*settledDelivery) map[string]*settledDelivery {
	return lazyAttr(table)
}

// Replies live native waiters are about to send keyed by admission, kept until the drain settles the turn
func waiterReplies(a *SessionAuthority) map[string]*settledDelivery {
	return deliveryRegistry(&a.waiterReplyRegistry)
}

// Keeps the reply and crash marker of a turn fenced unknown owed to its destination until a discard resolves it
func retainUnsettledDelivery(a *SessionAuthority, admissionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	replies := waiterReplies(a)
	delivery := replies[admissionID]
	delete(replies, admissionID)
	if delivery != nil {
		if adopter := markerAdopters(a)[admissionID]; adopter != nil {
			moveMarker(adopter, delivery.event)
			heldMarkers(a)[admissionID] = delivery.event
		}
	} else {
		delivery = a.pendingDeliveries[admissionID]
		delete(a.pendingDeliveries, admissionID)
	}
	if delivery != nil {
		deliveryRegistry(&a.unsettledDeliveries)[admissionID] = delivery
	}
}

// Sends the retained reply of a discarded turn once when its answer committed, else releases its marker and drops it
func deliverResolved(ctx context.Context, a *SessionAuthority, admissionID string, owed bool) error {
	a.mu.Lock()
	unsettled := deliveryRegistry(&a.unsettledDeliveries)
	delivery := unsettled[admissionID]
	delete(unsettled, admissionID)
	if delivery == nil {
		a.mu.Unlock()
		return nil
	}
	if owed {
		deliveryRegistry(&a.pendingDeliveries)[admissionID] = delivery
	}
	a.mu.Unlock()
	return deliverSettled(ctx, a, admissionID)
}

// Moves the turn's post delivery slot off its adapter onto the admission, a native waiter keeping its own
func takePostDelivery(ctx context.Context, a *SessionAuthority, admissionID string, source *SessionSource, route string) {
	a.mu.Lock()
	native := a.nativeWaiters[admissionID]
	a.mu.Unlock()
	if source == nil || native {
		return
	}
	adapter := a.Runner.AdapterForSource(ctx, source)
	if adapter == nil || adapter.Base() == nil {
		return
	}
	base := adapter.Base()
	base.mu.Lock()
	entry, held := base.postDeliveryCallbacks[route]
	delete(base.postDeliveryCallbacks, route)
	base.mu.Unlock()
	if !held {
		return
	}
	a.mu.Lock()
	lazyAttr(&a.postDeliveryTaken)[admissionID] = &postDeliveryTake{base: base, route: route, entry: entry}
	a.mu.Unlock()
}

// Takes the post delivery slot an admission holds, nil when it holds none
func popPostDelivery(a *SessionAuthority, admissionID string) *postDeliveryTake {
	a.mu.Lock()
	defer a.mu.Unlock()
	taken := a.postDeliveryTaken[admissionID]
	delete(a.postDeliveryTaken, admissionID)
	return taken
}

// Gives the slot back to its adapter for the native waiter to run, unless a newer turn already took it
func handBackPostDelivery(a *SessionAuthority, admissionID string) {
	taken := popPostDelivery(a, admissionID)
	if taken == nil {
		return
	}
	taken.base.mu.Lock()
	defer taken.base.mu.Unlock()
	slots := lazyAttr(&taken.base.postDeliveryCallbacks)
	if _, held := slots[taken.route]; !held {
		slots[taken.route] = taken.entry
	}
}

// Runs a settled turn's post delivery callbacks after its reply was sent, a native waiter running its own
func fireAdmissionPostDelivery(ctx context.Context, a *SessionAuthority, admissionID string) error {
	a.mu.Lock()
	native := a.nativeWaiters[admissionID]
	a.mu.Unlock()
	if native {
		handBackPostDelivery(a, admissionID)
		return nil
	}
	taken := popPostDelivery(a, admissionID)
	if taken == nil {
		return nil
	}
	_, callback := splitPostDeliveryEntry(taken.entry)
	RunPostDeliveryCallback(executionScope(ctx, a, nil), callback, postDeliveryCallbackTimeoutSeconds)
	return nil
}

// Clears the durable crash marker an event still carries
func releaseAdmissionTurnMarker(ctx context.Context, a *SessionAuthority, event *MessageEvent) error {
	if !pyTruthy(event.GatewayActiveTurnToken()) {
		return nil
	}
	clearer, ok := a.Runner.(DurableActiveTurnClearer)
	if !ok {
		return &PyError{Kind: "AttributeError", Msg: "'" + pyTypeName(a.Runner) + "' object has no attribute '_clear_durable_active_turn'"}
	}
	return clearer.ClearDurableActiveTurn(ctx, event)
}

// Notice a platform user reads once per pause episode for input queued behind a paused FIFO, nil afterwards
func pauseNotice(a *SessionAuthority, ref SessionRef, reason string) (any, error) {
	a.mu.Lock()
	live, ok := a.Sessions[ref.SessionID]
	if !ok {
		a.mu.Unlock()
		return nil, &pyKeyError{ref.SessionID}
	}
	if live.PauseNotified {
		a.mu.Unlock()
		return nil, nil
	}
	live.PauseNotified = true
	source := live.Source
	a.mu.Unlock()
	switch reason {
	case "runtime_draining":
		return "⏳ Hermes is restarting — your message is saved and will be answered when it is back.", nil
	case "session_busy":
		return "⏳ Your message is saved and will be answered when the work running in this conversation finishes.", nil
	case "unknown_execution":
		discard := "/discard"
		if source != nil && source.Platform == PlatformSlack {
			discard = "/hermes discard"
		}
		return "⏸️ This conversation is paused: a previous turn did not finish when Hermes restarted, " +
			"and only you know whether to send it again. Your message is saved. Send " + discard + " to drop " +
			"the lost turn and continue (it is not re-run), or /reset to start a fresh conversation.", nil
	}
	cause := "a queued message could not be re-authorized (" + reason + ")"
	return "⏸️ This conversation is paused: " + cause + ". Your message is saved but will not be answered here. " +
		"Send /reset to start a fresh conversation, or ask the operator to resume this one.", nil
}

// Sends a recovered turn's reply after its terminal outcome committed, a send failure logged and never rerun
func deliverSettled(ctx context.Context, a *SessionAuthority, admissionID string) error {
	a.mu.Lock()
	held := heldMarkers(a)[admissionID]
	delete(a.heldTurnMarkers, admissionID)
	delivery := a.pendingDeliveries[admissionID]
	var adopter *MessageEvent
	waited := delivery != nil && a.nativeWaiters[admissionID]
	if waited {
		delete(a.nativeWaiters, admissionID)
		delete(a.pendingDeliveries, admissionID)
		adopter = markerAdopters(a)[admissionID]
		delivery = nil
	}
	a.mu.Unlock()
	if waited {
		handBackPostDelivery(a, admissionID)
		if held != nil && adopter != nil {
			moveMarker(held, adopter)
			held = nil
		}
	}
	if delivery == nil {
		if held != nil {
			if err := releaseAdmissionTurnMarker(ctx, a, held); err != nil {
				return err
			}
		}
		closeProducerTurn(ctx, a, admissionID, ProcessingSuccess)
		return nil
	}
	delivered, sendErr := false, error(&PyError{Kind: "AttributeError", Msg: "'" + pyTypeName(delivery.adapter) + "' object has no attribute '_unwrap_ephemeral'"})
	if base := delivery.adapter.Base(); base != nil {
		delivered, sendErr = base.deliverResponse(executionScope(ctx, a, delivery.home), delivery.event, delivery.route, delivery.response)
	}
	cancelled := sendErr != nil && errors.Is(sendErr, context.Canceled)
	if sendErr != nil {
		delivered = false
		if !cancelled {
			slog.Error(fmt.Sprintf("Delivery of settled admission %s failed", admissionID), "error", sendErr)
		}
	}
	closing := context.WithoutCancel(ctx)
	a.mu.Lock()
	delete(a.pendingDeliveries, admissionID)
	a.mu.Unlock()
	if err := releaseAdmissionTurnMarker(closing, a, delivery.event); err != nil {
		return err
	}
	outcome := ProcessingFailure
	if delivered {
		outcome = ProcessingSuccess
	}
	closeProducerTurn(closing, a, admissionID, outcome)
	if cancelled {
		return sendErr
	}
	return nil
}
