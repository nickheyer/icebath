package tuiserver

import (
	"context"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Runs a claimed notification turn through the execution gate, releasing the claim when it refused or raised
func notifSubmit(ctx context.Context, rid, sid string, session *tuigateway.Session, text, what string, o promptSubmitOptions) bool {
	accepted := false
	if failure := catchPanic(func() { accepted = runPromptSubmit(ctx, rid, sid, session, text, o) }); failure != nil {
		notifLogFailure(what, failure)
		notifReleaseTurn(session)
		panic(failure)
	}
	if accepted {
		session.Pop("_notif_refusal_delay")
	} else {
		notifReleaseTurn(session)
	}
	return accepted
}

// Request id of a notification turn stamped with the wall clock in milliseconds
func notifRID() string { return fmt.Sprintf("__notif__%d", time.Now().UnixMilli()) }

// Runs the claimed agent turn for one notification event
func notifDispatchEvent(ctx context.Context, sid string, session *tuigateway.Session, evt map[string]any, text string) {
	env := configEnv(ctx)
	claim, claimed, err := tools.ClaimEventDelivery(env, evt, "tui-poller")
	if err != nil {
		notifLogFailure("notification delivery claim failed", err)
		claimed = false
	}
	if !claimed {
		notifReleaseTurn(session)
		return
	}
	o := promptSubmitOptions{silentOwnershipRefusal: true}
	switch evt["type"] {
	case "async_delegation":
		o.displayKind, o.displayMetadata = "async_delegation_complete", asyncDelegationDisplayMetadata(evt)
	case "heartbeat":
		o.displayKind = tools.HeartbeatDisplayKind
	}
	if agent.DiagnosticProcessEvent(evt) {
		if o.displayMetadata == nil {
			o.displayMetadata = map[string]any{}
		}
		o.displayMetadata["notification_category"] = "diagnostic"
	}
	accepted := false
	if failure := catchPanic(func() {
		accepted = notifSubmit(ctx, notifRID(), sid, session, text, "notification poller dispatch failed", o)
	}); failure != nil {
		panicIf(tools.ReleaseEventDelivery(env, evt, claim))
		return
	}
	if !accepted {
		notifDeferEvent(ctx, evt, claim, nil)
		notifRefusalBackoff(session)
		return
	}
	panicIf(tools.CompleteEventDelivery(env, evt, claim))
}

// Routes one dequeued event to another owner, the park, a status row and an agent turn, false when the drain must stop
func notifHandleEvent(ctx context.Context, sid string, session *tuigateway.Session, evt map[string]any, emitted map[string]bool, registry *tools.ProcessRegistry, format func(map[string]any) string, deferred *[]map[string]any, completions *[]tools.DrainedNotification, owned bool) bool {
	queue := registry.CompletionQueue
	evtType := pyrepr.Str(evtGet(evt, "type", "completion"))
	isDelegation := evt["type"] == "async_delegation"
	if !owned && notificationEventBelongsElsewhere(ctx, sid, session, evt) {
		if deferred != nil {
			*deferred = append(*deferred, evt)
		} else {
			queue.Put(evt)
			time.Sleep(100 * time.Millisecond)
		}
		return true
	}
	if !owned && notificationEventRequiresOwner(evt) && !sessionOwnsNotificationEvent(ctx, sid, session, evt) {
		notifUnownedEvent(sid, evt, deferred)
		return true
	}
	if evtType == "completion" && registry.IsCompletionConsumed(pyrepr.Str(evtGet(evt, "session_id", ""))) {
		return true
	}
	text := format(evt)
	if text == "" {
		return true
	}
	if dedup := notificationEventDedupKey(evt); !emitted[dedup] {
		displayText := text
		if isDelegation {
			displayText = tools.AsyncDelegationDisplayText(evt)
		} else if evtType == "completion" {
			displayText = tools.ProcessCompletionDisplayText([]map[string]any{evt})
		}
		if _, err := config.RenderNotification(ctx, func() error {
			tuigateway.Emit(ctx, "status.update", sid, map[string]any{"kind": "process", "text": displayText})
			return nil
		}, "tui", agent.DiagnosticProcessEvent(evt), nil); err != nil {
			panic(err)
		}
		emitted[dedup] = true
	}
	if evtType != "async_delegation" && backgroundNotificationsOff(ctx, session) {
		return true
	}
	if evtType == "completion" && completions != nil {
		*completions = append(*completions, tools.DrainedNotification{Event: evt, Text: text})
		return true
	}
	if !notifClaimTurn(session) {
		queue.Put(evt)
		if deferred != nil {
			return false
		}
		time.Sleep(250 * time.Millisecond)
		return true
	}
	notifDispatchEvent(ctx, sid, session, evt, text)
	return true
}

type claimedNotification struct {
	event map[string]any
	text  string
	claim string
}

// Runs one turn for a run of process completions, requeueing them when the session is busy or refuses
func notifDispatchCompletions(ctx context.Context, sid string, session *tuigateway.Session, notifications []tools.DrainedNotification, registry *tools.ProcessRegistry, deferred *[]map[string]any) {
	if len(notifications) == 0 {
		return
	}
	handBack := registry.CompletionQueue.Put
	if deferred != nil {
		handBack = func(evt map[string]any) { *deferred = append(*deferred, evt) }
	}
	if !notifClaimTurn(session) {
		for _, n := range notifications {
			handBack(n.Event)
		}
		if deferred == nil {
			time.Sleep(250 * time.Millisecond)
		}
		return
	}
	env := configEnv(ctx)
	var claimed []claimedNotification
	var batch tools.ProcessNotificationBatch
	text := ""
	if failure := catchPanic(func() {
		for _, n := range notifications {
			claim, ok, err := tools.ClaimEventDelivery(env, n.Event, "tui-completion-batch")
			if err != nil {
				panic(err)
			}
			if ok {
				claimed = append(claimed, claimedNotification{n.Event, n.Text, claim})
			}
		}
		for _, c := range claimed {
			batch.Notifications = append(batch.Notifications, tools.DrainedNotification{Event: c.event, Text: c.text})
		}
		text = batch.Render(registry)
	}); failure != nil {
		notifLogFailure("completion batch preparation failed", failure)
		notifReleaseTurn(session)
		for _, c := range claimed {
			panicIf(tools.ReleaseEventDelivery(env, c.event, c.claim))
		}
		return
	}
	if text == "" {
		notifReleaseTurn(session)
	}
	if failure := catchPanic(func() {
		if text == "" {
			return
		}
		accepted := notifSubmit(ctx, notifRID(), sid, session, text, "completion batch dispatch failed", promptSubmitOptions{
			displayKind: tools.ProcessCompleteDisplayKind, silentOwnershipRefusal: true,
			displayMetadata: map[string]any{"display_text": batch.DisplayText(registry)}})
		if !accepted {
			for _, c := range claimed {
				notifDeferEvent(ctx, c.event, c.claim, handBack)
			}
			if deferred == nil {
				notifRefusalBackoff(session)
			}
			claimed = nil
		}
	}); failure != nil {
		for _, c := range claimed {
			panicIf(tools.ReleaseEventDelivery(env, c.event, c.claim))
		}
		return
	}
	for _, c := range claimed {
		panicIf(tools.CompleteEventDelivery(env, c.event, c.claim))
	}
}

// Routes one ready snapshot, one turn per completion run, handing back what this pass did not route
func notifHandleReady(ctx context.Context, sid string, session *tuigateway.Session, events []map[string]any, emitted map[string]bool, registry *tools.ProcessRegistry, format func(map[string]any) string, deferred *[]map[string]any, owned bool) {
	var completions []tools.DrainedNotification
	handBack := registry.CompletionQueue.Put
	if deferred != nil {
		handBack = func(evt map[string]any) { *deferred = append(*deferred, evt) }
	}
	for index, event := range events {
		if pyrepr.Str(evtGet(event, "type", "completion")) != "completion" {
			notifDispatchCompletions(ctx, sid, session, completions, registry, deferred)
			completions = nil
		}
		routed := false
		if failure := catchPanic(func() {
			routed = notifHandleEvent(ctx, sid, session, event, emitted, registry, format, deferred, &completions, owned)
		}); failure != nil {
			for _, remaining := range events[index+1:] {
				handBack(remaining)
			}
			notifDispatchCompletions(ctx, sid, session, completions, registry, deferred)
			tools.ReturnCompletionOffer(event)
			panic(failure)
		}
		if !routed {
			for _, remaining := range events[index+1:] {
				handBack(remaining)
			}
			break
		}
	}
	notifDispatchCompletions(ctx, sid, session, completions, registry, deferred)
}
