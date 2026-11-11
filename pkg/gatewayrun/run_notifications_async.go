package gatewayrun

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Fills the route fields of an async delegation event from its session key, a CLI origin event left as is
func enrichAsyncDelegationRouting(evt map[string]any) {
	if pyrepr.Truthy(evt["platform"]) {
		return
	}
	sessionKey, _ := pyrepr.Or(evt["session_key"], "").(string)
	parsed := gateway.ParseSessionKey(sessionKey)
	if len(parsed) == 0 {
		return
	}
	evt["platform"] = pyrepr.Or(parsed["platform"], "")
	evt["chat_type"] = pyrepr.Or(parsed["chat_type"], "")
	evt["chat_id"] = pyrepr.Or(parsed["chat_id"], "")
	if pyrepr.Truthy(parsed["thread_id"]) {
		evt["thread_id"] = parsed["thread_id"]
	}
}

// Delivers a same session batch of async completions as one turn inside the group's profile scope
func (r *GatewayRunner) deliverAsyncDelegationGroup(ctx context.Context, group []map[string]any) (*bool, error) {
	scoped, err := r.completionEventScope(ctx, group[0])
	if err != nil {
		return nil, err
	}
	return r.deliverAsyncDelegationGroupScoped(scoped, group)
}

// One consolidated completion turn of every sibling this runner claimed, nil when nothing is deliverable here
func (r *GatewayRunner) deliverAsyncDelegationGroupScoped(ctx context.Context, group []map[string]any) (*bool, error) {
	yes, no := true, false
	if err := gateway.RestoreCompletionRoute(ctx, r, pyrepr.Str(pyrepr.Or(group[0]["session_key"], ""))); err != nil {
		return nil, err
	}
	if len(group) > 0 && pyrepr.Truthy(group[0]["origin_session_id"]) {
		refused := false
		for _, evt := range group {
			if text := gateway.FormatGatewayProcessNotification(evt); text != "" {
				outcome, err := r.deliverCompletionNotification(ctx, text, evt, nil)
				if err != nil {
					return nil, err
				}
				if outcome != nil && !*outcome {
					refused = true
				}
			}
		}
		if refused {
			return &no, nil
		}
		return &yes, nil
	}
	type deliverableEvent struct {
		evt  map[string]any
		text string
	}
	var deliverable []deliverableEvent
	acknowledged := false
	for _, evt := range group {
		synthText := gateway.FormatGatewayProcessNotification(evt)
		if synthText == "" {
			continue
		}
		if r.sessionAuthority != nil {
			admission, err := gateway.CompletionAdmission(ctx, r, evt)
			if err != nil {
				return nil, err
			}
			if admission != nil {
				outcome, err := r.deliverCompletionNotification(ctx, synthText, evt, nil)
				if err != nil {
					return nil, err
				}
				acknowledged = (outcome != nil && *outcome) || acknowledged
				continue
			}
		}
		parent := pyrepr.Str(pyrepr.Or(evt["parent_session_id"], ""))
		if parent != "" {
			target, err := r.classifyCompletionTarget(ctx, parent, pyrepr.Str(pyrepr.Or(evt["session_key"], "")))
			if err != nil {
				return nil, err
			}
			if target == "terminal" {
				if _, err := r.deliverCompletionNotification(ctx, synthText, evt, nil); err != nil {
					return nil, err
				}
				continue
			}
		}
		if identity, ok := completionDeliveryIdentity(evt); ok && r.completionIdentitySeen(identity, false) {
			continue
		}
		deliverable = append(deliverable, deliverableEvent{evt, synthText})
	}
	if len(deliverable) == 0 {
		if acknowledged {
			return &yes, nil
		}
		return nil, nil
	}
	if len(deliverable) == 1 {
		return r.deliverCompletionNotification(ctx, deliverable[0].text, deliverable[0].evt, nil)
	}
	for _, item := range deliverable {
		ready, err := r.completionDeliveryReady(ctx, item.evt)
		if err != nil {
			return nil, err
		}
		if !ready {
			return &no, nil
		}
	}
	env := watcherToolsEnv(ctx)
	primaryEvt, primaryText := deliverable[0].evt, deliverable[0].text
	blocks := []string{primaryText}
	var siblings []siblingClaim
	consumer := fmt.Sprintf("gateway-batch:%d", reflect.ValueOf(r).Pointer())
	for _, item := range deliverable[1:] {
		claimID, claimed, err := tools.ClaimEventDelivery(env, item.evt, consumer)
		if err != nil {
			return nil, err
		}
		if !claimed {
			continue
		}
		siblings = append(siblings, siblingClaim{evt: item.evt, claimID: claimID})
		blocks = append(blocks, item.text)
	}
	if len(siblings) == 0 {
		return r.deliverCompletionNotification(ctx, primaryText, primaryEvt, nil)
	}
	header := fmt.Sprintf("[IMPORTANT: %d background subagent delegations "+
		"completed for this session. Treat these results as one "+
		"completion batch and send at most one consolidated user-facing "+
		"response. If a result does not change the current conclusion, absorb it silently.]", len(blocks))
	consolidated := strings.Join(append([]string{header}, blocks...), "\n\n")
	if r.sessionAuthority != nil {
		identities := []any{}
		for _, event := range append([]map[string]any{primaryEvt}, siblingEvents(siblings)...) {
			identity, err := gateway.ProducerIdentity(ctx, r, event)
			if err != nil {
				return nil, err
			}
			identities = append(identities, identity)
		}
		primaryEvt = maps.Clone(primaryEvt)
		primaryEvt["_automation_identities"] = identities
	}
	delivered, err := r.deliverCompletionNotification(ctx, consolidated, primaryEvt, siblings)
	if err != nil {
		return nil, err
	}
	if delivered == nil {
		queue := tools.DefaultProcessRegistry().CompletionQueue
		for _, sibling := range siblings {
			queue.Put(sibling.evt)
		}
	}
	return delivered, nil
}

// Events of the claimed siblings in claim order
func siblingEvents(siblings []siblingClaim) []map[string]any {
	events := make([]map[string]any, 0, len(siblings))
	for _, sibling := range siblings {
		events = append(events, sibling.evt)
	}
	return events
}

// Runs fn once per secondary profile bound to that profile's ledger, logging the completions it handled
func (r *GatewayRunner) eachSecondaryLedger(ctx context.Context, profileHomes []profiles.ServedProfile, fn func(profileHome string) (int, error), verb string) {
	for _, p := range profileHomes {
		if r.primaryProfileName != "" && p.Name == r.primaryProfileName {
			continue
		}
		count, err := func() (count int, err error) {
			defer func() {
				if rec := recover(); rec != nil {
					err = fmt.Errorf("%v", rec)
				}
			}()
			return fn(p.Home)
		}()
		if err != nil {
			runLog().Warn(fmt.Sprintf("Could not replay async completions for profile %s", pyrepr.Repr(p.Name)), "exc_info", err)
			continue
		}
		if count > 0 {
			runLog().Info(fmt.Sprintf("%s %d undelivered async completion(s) for profile %s", verb, count, pyrepr.Repr(p.Name)))
		}
	}
}

// Offers completions whose owner process died, the launch ledger first then each served secondary
func (r *GatewayRunner) sweepOrphanedCompletionLedgers(ctx context.Context) {
	queue := tools.DefaultProcessRegistry().CompletionQueue
	count, err := tools.SweepOrphanedCompletions(watcherToolsEnv(ctx), queue, nil)
	if err != nil {
		runLog().Debug(fmt.Sprintf("Orphaned async completion sweep failed: %s", err))
	} else if count > 0 {
		runLog().Info(fmt.Sprintf("Re-offered %d orphaned async completion(s)", count))
	}
	var served []profiles.ServedProfile
	if r.servedProfileHomes != nil {
		for name, home := range r.servedProfileHomes.All() {
			served = append(served, profiles.ServedProfile{Name: name, Home: home})
		}
	}
	r.eachSecondaryLedger(ctx, served, func(profileHome string) (int, error) {
		return tools.SweepOrphanedCompletions(&tools.Env{Home: profileHome}, queue, nil)
	}, "Re-offered orphaned")
}

// Spaces out an undelivered group's retries with a growing refusal backoff
func backOffRefusedCompletions(group []map[string]any) {
	for _, evt := range group {
		step := 1
		if raw := evt["_watch_retry_step"]; pyrepr.Truthy(raw) {
			if n, err := pyint.Of(raw); err == nil && n.IsInt64() {
				step = int(n.Int64()) + 1
			}
		}
		evt["_watch_retry_step"] = step
		evt["_watch_next_retry_at"] = refusalRetryAt(step)
	}
}

// Seconds on the monotonic clock before which an event must not be offered again
func watchNextRetryAt(evt map[string]any) float64 {
	switch value := evt["_watch_next_retry_at"].(type) {
	case float64:
		return value
	case int:
		return float64(value)
	}
	return 0
}

// Drains async completions and pattern notifications while sessions idle until the runner stops
func (r *GatewayRunner) asyncDelegationWatcher(ctx context.Context, interval float64) error {
	if err := sleepCtx(ctx, 3); err != nil {
		return err
	}
	queue := tools.DefaultProcessRegistry().CompletionQueue
	lastOrphanSweep := -1.0
	pass := func() error {
		if lastOrphanSweep < 0 || pythread.Monotonic()-lastOrphanSweep >= tools.OrphanSweepIntervalS {
			lastOrphanSweep = pythread.Monotonic()
			r.sweepOrphanedCompletionLedgers(ctx)
		}
		if err := r.drainWatchNotifications(ctx, queue); err != nil {
			return err
		}
		var requeue, asyncEvents []map[string]any
		for !queue.Empty() {
			evt, ok := queue.GetNowait()
			if !ok {
				break
			}
			if evt["type"] == "async_delegation" {
				asyncEvents = append(asyncEvents, evt)
			} else {
				requeue = append(requeue, evt)
			}
		}
		for _, evt := range requeue {
			queue.Put(evt)
		}
		var order []string
		groups := map[string][]map[string]any{}
		for _, evt := range asyncEvents {
			enrichAsyncDelegationRouting(evt)
			key := eventRouteKey(evt, asyncGroupKeyFields)
			if _, seen := groups[key]; !seen {
				order = append(order, key)
			}
			groups[key] = append(groups[key], evt)
		}
		for _, key := range order {
			group := groups[key]
			deferred := false
			for _, evt := range group {
				if watchNextRetryAt(evt) > pythread.Monotonic() {
					deferred = true
					break
				}
			}
			if deferred {
				for _, evt := range group {
					queue.Put(evt)
				}
				continue
			}
			delivered, err := r.deliverAsyncDelegationGroup(ctx, group)
			if err != nil {
				for _, evt := range group {
					queue.Put(evt)
				}
				runLog().Error(fmt.Sprintf("Async delegation injection error: %s", err))
				continue
			}
			if delivered != nil && !*delivered {
				backOffRefusedCompletions(group)
				for _, evt := range group {
					queue.Put(evt)
				}
			}
		}
		return nil
	}
	for r.running.Load() {
		if err := pass(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			runLog().Debug(fmt.Sprintf("Async delegation watcher error: %s", err))
		}
		if err := sleepCtx(ctx, interval); err != nil {
			return err
		}
	}
	return nil
}
