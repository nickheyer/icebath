package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Owner and platform an async completion was held for, warned once per outage
type noTransportKey struct {
	hasProfile bool
	profile    string
	platform   string
}

// Adapter that opens its own session store, the duck typed _ensure_session_db of a pull adapter
type sessionDBEnsurer interface {
	EnsureSessionDB(ctx context.Context) (*state.SessionDB, error)
}

// Whether the event clears the child process surfacing policy of the active profile
func surfacesNotification(ctx context.Context, evt map[string]any) bool {
	cfg, err := config.Load(home.Dir(ctx), nil)
	if err != nil {
		cfg = config.Defaults()
	}
	return tools.ShouldSurfaceNotification(cfg, evt)
}

// Route key of an event from the named fields, absent and empty values alike
func eventRouteKey(evt map[string]any, fields []string) string {
	parts := make([]string, len(fields))
	for i, field := range fields {
		parts[i] = pyrepr.Str(pyrepr.Or(evt[field], ""))
	}
	return strings.Join(parts, "\x1f")
}

// Fans policy admitted process completions with compatible pinned owners into one short window batch
func (r *GatewayRunner) enqueueProcessCompletionNotification(ctx context.Context, synthText string, evt map[string]any) (*bool, error) {
	scoped, err := r.completionEventScope(ctx, evt)
	if err != nil {
		return nil, err
	}
	if !surfacesNotification(scoped, evt) {
		return nil, nil
	}
	r.completionBatchMu.Lock()
	if r.completionNotificationBatches == nil {
		r.completionNotificationBatches = map[string][]completionBatchItem{}
	}
	if r.completionNotificationBatchTasks == nil {
		r.completionNotificationBatchTasks = map[string]*gatewayTask{}
	}
	if r.completionNotificationBatchFlushTasks == nil {
		r.completionNotificationBatchFlushTasks = map[*gatewayTask]struct{}{}
	}
	if r.completionNotificationBatchesStopping {
		r.completionBatchMu.Unlock()
		return boolp(false), nil
	}
	key := eventRouteKey(evt, append(append([]string(nil), completionBatchKeyFields...), "parent_session_id", "origin_session_id"))
	future := newGatewayFuture()
	r.completionNotificationBatches[key] = append(r.completionNotificationBatches[key], completionBatchItem{synthText: synthText, evt: evt, future: future})
	var task *gatewayTask
	if _, ok := r.completionNotificationBatchTasks[key]; !ok {
		ready := make(chan struct{})
		task = spawnGatewayTask(context.WithoutCancel(ctx), "process-completion-batch", func(taskCtx context.Context) (any, error) {
			<-ready
			return nil, r.flushProcessCompletionBatch(taskCtx, key)
		})
		r.completionNotificationBatchTasks[key] = task
		r.completionBatchMu.Unlock()
		r.retainBackgroundTask(task)
		r.trackTaskIn(r.completionNotificationBatchFlushTasks, task)
		close(ready)
	} else {
		r.completionBatchMu.Unlock()
	}
	result, err := future.wait(ctx)
	if err != nil {
		return nil, err
	}
	delivered, _ := result.(*bool)
	return delivered, nil
}

// One bounded synthetic event built from several redacted completions
func formatCoalescedProcessCompletions(ctx context.Context, entries []completionBatchItem) string {
	lines := []string{
		fmt.Sprintf("[IMPORTANT: %d background processes completed for this session.", len(entries)),
		"Treat these results as one completion batch and send at most one consolidated user-facing response.",
	}
	shown := entries[:min(len(entries), 10)]
	for _, entry := range shown {
		evt := entry.evt
		sessionID := pyrepr.Str(pyrepr.Or(evt["session_id"], "unknown"))
		reason := pyrepr.Str(pyrepr.Or(evt["completion_reason"], "exited"))
		output := []rune(pystr.Strip(redactGatewayUserFacingSecrets(ctx, pyrepr.Str(pyrepr.Or(evt["output"], "")))))
		text := string(output)
		if len(output) > 800 {
			text = "[… truncated …]\n" + string(output[len(output)-800:])
		}
		lines = append(lines, fmt.Sprintf("\n- %s: exit_code=%s, reason=%s", sessionID, pyrepr.Str(evt["exit_code"]), reason))
		if text != "" {
			lines = append(lines, text)
		}
	}
	if omitted := len(entries) - len(shown); omitted > 0 {
		lines = append(lines, fmt.Sprintf("\n- … and %d more completion(s); inspect them with the process tool if they affect the conclusion.", omitted))
	}
	lines = append(lines, "If a result does not change the current conclusion, absorb it silently.]")
	return strings.Join(lines, "\n")
}

// Extends a successful primary delivery claim to its batched siblings
func (r *GatewayRunner) recordCoalescedCompletionSiblings(events []map[string]any) {
	var identities []completionIdentity
	for _, evt := range events {
		if identity, ok := completionDeliveryIdentity(evt); ok {
			identities = append(identities, identity)
		}
	}
	r.completionDeliveryLock.Lock()
	defer r.completionDeliveryLock.Unlock()
	r.markCompletionsDeliveredLocked(identities)
}

// Detaches the batch entries of key, clearing the key's flush task when it is still the current one
func (r *GatewayRunner) popCompletionBatch(key string, current *gatewayTask) []completionBatchItem {
	r.completionBatchMu.Lock()
	defer r.completionBatchMu.Unlock()
	entries := r.completionNotificationBatches[key]
	delete(r.completionNotificationBatches, key)
	if current != nil && r.completionNotificationBatchTasks[key] == current {
		delete(r.completionNotificationBatchTasks, key)
	}
	return entries
}

// Delivers one short window completion batch and resolves every waiter with the outcome
func (r *GatewayRunner) flushProcessCompletionBatch(ctx context.Context, key string) (err error) {
	current := currentGatewayTask(ctx)
	var entries []completionBatchItem
	delivered := boolp(false)
	defer func() {
		settleBatchWaiters(entries, delivered)
		r.completionBatchMu.Lock()
		if current != nil && r.completionNotificationBatchTasks[key] == current {
			delete(r.completionNotificationBatchTasks, key)
		}
		r.completionBatchMu.Unlock()
	}()
	select {
	case <-ctx.Done():
		entries = r.popCompletionBatch(key, nil)
		return ctx.Err()
	case <-time.After(time.Duration(r.completionNotificationBatchWindow * float64(time.Second))):
	}
	entries = r.popCompletionBatch(key, current)
	if len(entries) == 0 {
		return nil
	}
	result, flushErr := r.deliverCompletionBatch(ctx, entries)
	if flushErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		runLog().Error("Coalesced process completion delivery failed", "error", flushErr)
		return nil
	}
	delivered = result
	return nil
}

// Admission filtered delivery of a detached batch, trying each identity until one is not a duplicate
func (r *GatewayRunner) deliverCompletionBatch(ctx context.Context, entries []completionBatchItem) (*bool, error) {
	toDeliver := entries
	if r.sessionAuthority != nil {
		toDeliver = nil
		for _, item := range entries {
			receipt, err := gateway.CompletionAdmission(ctx, r, item.evt)
			if err != nil {
				return boolp(false), err
			}
			if receipt == nil {
				toDeliver = append(toDeliver, item)
			}
		}
		if len(toDeliver) == 0 {
			return boolp(true), nil
		}
	}
	synthText := toDeliver[0].synthText
	if len(toDeliver) > 1 {
		synthText = formatCoalescedProcessCompletions(ctx, toDeliver)
	}
	events := make([]map[string]any, len(toDeliver))
	for i, item := range toDeliver {
		events[i] = item.evt
	}
	displayText := tools.ProcessCompletionDisplayText(events)
	var identities []any
	if r.sessionAuthority != nil {
		for _, evt := range events {
			identity, err := gateway.ProducerIdentity(ctx, r, evt)
			if err != nil {
				return boolp(false), err
			}
			identities = append(identities, identity)
		}
	}
	var delivered *bool
	for _, item := range toDeliver {
		candidate := cloneEvent(item.evt)
		candidate["_display_text"] = displayText
		if r.sessionAuthority != nil {
			candidate["_automation_identities"] = identities
		}
		result, err := r.deliverCompletionNotification(ctx, synthText, candidate, nil)
		if err != nil {
			return boolp(false), err
		}
		delivered = result
		if delivered != nil {
			break
		}
	}
	if delivered != nil && *delivered && len(entries) > 1 {
		all := make([]map[string]any, len(entries))
		for i, item := range entries {
			all[i] = item.evt
		}
		r.recordCoalescedCompletionSiblings(all)
	}
	return delivered, nil
}

// Shallow copy of an event map
func cloneEvent(evt map[string]any) map[string]any {
	out := make(map[string]any, len(evt)+2)
	for k, v := range evt {
		out[k] = v
	}
	return out
}

// One sibling of a coalesced async group with the durable claim this runner holds on it
type siblingClaim struct {
	evt     map[string]any
	claimID string
}

// Acknowledges one admitted batch inside the event's profile scope, refunding refusals and releasing failures
func (r *GatewayRunner) deliverCompletionNotification(ctx context.Context, synthText string, evt map[string]any, siblingClaims []siblingClaim) (*bool, error) {
	scoped, err := r.completionEventScope(ctx, evt)
	if err != nil {
		return nil, err
	}
	return r.deliverCompletionNotificationScoped(scoped, synthText, evt, siblingClaims)
}

// Delivery body of one completion, true on adapter admission, nil when deduplicated or terminal, false retryable
func (r *GatewayRunner) deliverCompletionNotificationScoped(ctx context.Context, synthText string, evt map[string]any, siblingClaims []siblingClaim) (result *bool, err error) {
	if !surfacesNotification(ctx, evt) {
		return nil, nil
	}
	if err := gateway.RestoreCompletionRoute(ctx, r, pyrepr.Str(pyrepr.Or(evt["session_key"], ""))); err != nil {
		return nil, err
	}
	evt, err = gateway.FollowDetachedPin(ctx, r, evt)
	if err != nil {
		return nil, err
	}
	identity, hasIdentity := completionDeliveryIdentity(evt)
	claim := newCompletionClaim()
	accepted, identityClaimed, refused := false, false, false
	defer func() {
		if identityClaimed && !accepted {
			r.completionDeliveryLock.Lock()
			delete(r.completionDeliveriesInflight, identity)
			r.completionDeliveryLock.Unlock()
		}
		operation := "release"
		if accepted {
			operation = "complete"
		} else if refused || claim.deferred {
			operation = "defer"
		}
		if claim.claimID != "" {
			settleDurableClaim(ctx, operation, claim.delegationID, claim.claimID)
		}
		for _, sibling := range siblingClaims {
			if sibling.claimID != "" {
				settleDurableClaim(ctx, operation, pyrepr.Str(sibling.evt["delegation_id"]), sibling.claimID)
			}
		}
		if accepted && len(siblingClaims) > 0 {
			events := make([]map[string]any, len(siblingClaims))
			for i, sibling := range siblingClaims {
				events[i] = sibling.evt
			}
			r.recordCoalescedCompletionSiblings(events)
		}
	}()
	preflight, err := r.preflightCompletionDelivery(ctx, evt)
	if err != nil {
		return nil, err
	}
	claim = preflight
	if !claim.proceed {
		return claim.earlyResult, nil
	}
	if r.sessionAuthority != nil {
		receipt, err := gateway.CompletionAdmission(ctx, r, evt)
		if err != nil {
			return nil, err
		}
		if receipt != nil {
			accepted = true
			return boolp(true), nil
		}
	}
	if hasIdentity {
		if r.completionIdentitySeen(identity, true) {
			return nil, nil
		}
		identityClaimed = true
	}
	if claim.ownerSessionID != "" {
		evt = cloneEvent(evt)
		evt["parent_session_id"] = claim.ownerSessionID
		evt["proven_session_id"] = claim.provenSessionID
	}
	completionIDs := []any{}
	if claim.claimID != "" {
		completionIDs = append(completionIDs, claim.delegationID)
	}
	for _, sibling := range siblingClaims {
		if sibling.claimID != "" {
			completionIDs = append(completionIDs, sibling.evt["delegation_id"])
		}
	}
	evt = cloneEvent(evt)
	evt["completion_ids"] = completionIDs
	injection, err := r.injectWatchNotification(ctx, synthText, evt, true)
	if err != nil {
		var notAccepted *gateway.WakeNotAccepted
		if errors.As(err, &notAccepted) {
			refused = !notAccepted.Permanent
			return boolp(false), nil
		}
		return nil, err
	}
	if injection == nil || !*injection {
		return injection, nil
	}
	accepted = true
	if hasIdentity {
		r.completionDeliveryLock.Lock()
		r.markCompletionsDeliveredLocked([]completionIdentity{identity})
		r.completionDeliveryLock.Unlock()
	}
	return boolp(true), nil
}

// Whether an async completion owner and transport are available, so an outage spends no durable attempt
func (r *GatewayRunner) completionDeliveryReady(ctx context.Context, evt map[string]any) (bool, error) {
	if parent := pystr.Strip(truthyText(evt["parent_session_id"])); parent != "" {
		verdict, err := r.classifyCompletionTarget(ctx, parent, truthyText(evt["session_key"]))
		if err != nil {
			return false, err
		}
		if verdict != "deliver" {
			return verdict == "terminal", nil
		}
	}
	source, err := r.buildProcessEventSource(ctx, evt)
	if err != nil {
		return false, err
	}
	var adapter gateway.PlatformAdapter
	platform := ""
	if source != nil {
		platform = string(source.Platform)
		if adapter, err = r.resolveInjectionAdapter(ctx, platform, source); err != nil {
			return false, err
		}
	} else if rawSID := rawProcessEventSessionID(evt); rawSID != "" && r.adapters != nil {
		adapter, _ = r.adapters.Get(gateway.PlatformAPIServer)
		if adapter != nil && gateway.AdapterSupportsPush(adapter) {
			return false, nil
		}
	}
	var key *noTransportKey
	if source != nil {
		key = &noTransportKey{hasProfile: source.Profile != nil, profile: optText(source.Profile), platform: platform}
	}
	r.completionBatchMu.Lock()
	if r.completionNoTransportWarned == nil {
		r.completionNoTransportWarned = map[noTransportKey]struct{}{}
	}
	if adapter == nil {
		warn := false
		if key != nil {
			if _, seen := r.completionNoTransportWarned[*key]; !seen {
				r.completionNoTransportWarned[*key] = struct{}{}
				warn = true
			}
		}
		r.completionBatchMu.Unlock()
		if warn {
			multiplexed := r.config != nil && r.config.MultiplexProfiles != nil && *r.config.MultiplexProfiles
			runLog().Warn("Async completion held: " + heldReason(source, platform, multiplexed))
		}
		return false, nil
	}
	if key != nil {
		delete(r.completionNoTransportWarned, *key)
	}
	r.completionBatchMu.Unlock()
	if !gateway.AdapterSupportsPush(adapter) {
		ensurer, ok := adapter.(sessionDBEnsurer)
		if !ok {
			return false, nil
		}
		db, err := ensurer.EnsureSessionDB(ctx)
		if err != nil {
			runLog().Debug("Async-completion delivery DB unavailable", "error", err)
			return false, nil
		}
		if db == nil {
			return false, nil
		}
	}
	return true, nil
}

// Claims the durable row of an async delegation and verifies its target before adapter acceptance
func (r *GatewayRunner) preflightCompletionDelivery(ctx context.Context, evt map[string]any) (*completionClaim, error) {
	claim := newCompletionClaim()
	evtType := evt["type"]
	if evtType == "async_delegation" {
		ready, err := r.completionDeliveryReady(ctx, evt)
		if err != nil {
			return nil, err
		}
		if !ready {
			claim.proceed, claim.earlyResult = false, boolp(false)
			return claim, nil
		}
	}
	if evtType == "async_delegation" && !pyrepr.Truthy(evt["task_failure_notice"]) {
		claim.delegationID = truthyText(evt["delegation_id"])
		if claim.delegationID != "" {
			claim.claimID = fmt.Sprintf("gateway:%d:%s", reflect.ValueOf(r).Pointer(), strings.ReplaceAll(uuid.NewString(), "-", ""))
			claimed, err := tools.ClaimCompletionDelivery(&tools.Env{Home: home.Dir(ctx)}, claim.delegationID, claim.claimID)
			if err != nil {
				runLog().Warn(fmt.Sprintf("Could not claim durable async completion %s: %s", claim.delegationID, pyrepr.ExceptionMessage(err)))
				claim.proceed, claim.earlyResult = false, boolp(false)
				return claim, nil
			}
			if !claimed {
				claim.proceed = false
				return claim, nil
			}
		}
	} else if evtType != "completion" {
		return claim, nil
	}
	parent := pystr.Strip(truthyText(evt["parent_session_id"]))
	if parent == "" {
		return claim, nil
	}
	verdict, owner, proven, err := r.classifyCompletionOwner(ctx, parent, truthyText(evt["session_key"]))
	if err != nil {
		return nil, err
	}
	claim.ownerSessionID, claim.provenSessionID = owner, proven
	switch verdict {
	case "terminal":
		if evtType == "async_delegation" {
			delegation := claim.delegationID
			if delegation == "" {
				delegation = "<legacy>"
			}
			runLog().Warn(fmt.Sprintf("Async delegation %s targets permanently-gone session %s; terminally dropping delivery (result remains in the delegation records).", delegation, parent))
			if claim.claimID != "" {
				settleDurableClaim(ctx, "drop", claim.delegationID, claim.claimID)
			}
		} else {
			runLog().Warn(fmt.Sprintf("Background process %s completion targets permanently-gone session %s (user boundary such as /new); dropping notification (output remains available via process(action='log')).",
				pyrepr.Str(pyrepr.Or(evt["session_id"], "<unknown>")), parent))
		}
		claim.proceed = false
	case "retry":
		claim.proceed, claim.earlyResult, claim.deferred = false, boolp(false), true
	}
	return claim, nil
}
