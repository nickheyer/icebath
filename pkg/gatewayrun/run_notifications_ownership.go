package gatewayrun

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Longest model_config _delegate_from chain walked before a delegate pin fails closed
const maxDelegateProvenanceHops = 16

// Value of a session row column, nil when absent
func rowField(row *pyjson.Object, key string) any {
	v, _ := row.Get(key)
	return v
}

// Deliver with the live compression tip, retry while ownership is uncertain, terminal for a proven foreign route
func (r *GatewayRunner) resolveCompressionLineageTarget(ctx context.Context, db *state.AsyncSessionDB, entry *gateway.SessionEntry, pinned string) (string, string) {
	failed := func() (string, string) {
		inboundLog().Debug(fmt.Sprintf("Async-delegation compression ownership lookup failed for %s", pinned))
		return "retry", ""
	}
	target, err := db.DB().GetCompressionTip(ctx, pinned)
	if err != nil {
		return failed()
	}
	if target == "" || target == pinned {
		return "retry", ""
	}
	tipRow, err := db.DB().GetSession(ctx, target)
	if err != nil {
		return failed()
	}
	if tipRow == nil || pyrepr.Truthy(rowField(tipRow, "ended_at")) {
		return "retry", ""
	}
	if entry == nil || entry.SessionID == pinned || entry.SessionID == target {
		return "deliver", target
	}
	routeRow, err := db.DB().GetSession(ctx, entry.SessionID)
	if err != nil {
		return failed()
	}
	if routeRow == nil {
		return "retry", ""
	}
	if pyrepr.Truthy(rowField(routeRow, "ended_at")) && rowField(routeRow, "end_reason") == "compression" {
		routeTip, err := db.DB().GetCompressionTip(ctx, entry.SessionID)
		if err != nil {
			return failed()
		}
		if routeTip == "" || routeTip == entry.SessionID {
			return "retry", ""
		}
		if routeTip == target {
			return "deliver", target
		}
	}
	inboundLog().Warn(fmt.Sprintf("Async-delegation completion for compression lineage %s -> %s does not own current route %s; dropping injection.",
		pinned, target, entry.SessionID))
	return "terminal", ""
}

// Closed physical session of a local conversation the completion is noted on, empty when none
func (r *GatewayRunner) closedLocalTarget(ctx context.Context, db *state.AsyncSessionDB, entry *gateway.SessionEntry, pinned string, pinnedRow *pyjson.Object) (string, error) {
	if entry == nil || entry.Platform != gateway.PlatformLocal || pinnedRow == nil {
		return "", nil
	}
	target, row := pinned, pinnedRow
	if rowField(row, "end_reason") == "compression" {
		tip, err := db.DB().GetCompressionTip(ctx, pinned)
		row = nil
		if err == nil && tip == entry.SessionID {
			row, err = db.DB().GetSession(ctx, tip)
		}
		if err != nil {
			var replaced *state.StateDBReplacedError
			if _, sqliteErr := state.SQLiteClassOf(err); sqliteErr || errors.As(err, &replaced) {
				inboundLog().Debug(fmt.Sprintf("Closed local tip lookup failed for %s", pinned))
				return "", nil
			}
			return "", err
		}
		target = tip
	}
	if target != entry.SessionID || row == nil || !pyrepr.Truthy(rowField(row, "ended_at")) || rowField(row, "end_reason") == "compression" {
		return "", nil
	}
	return target, nil
}

// Delegation proof of a pinned session, its verdict, the owning session id and that owner's row
func (r *GatewayRunner) lookupCompletionOwner(ctx context.Context, sessionID, currentSessionID string) (string, string, *pyjson.Object) {
	db := r.sessionDB(ctx)
	if db == nil {
		return "retry", sessionID, nil
	}
	seen := map[string]bool{}
	for hops := 0; hops <= maxDelegateProvenanceHops; hops++ {
		if seen[sessionID] {
			return "terminal", sessionID, nil
		}
		seen[sessionID] = true
		row, err := db.DB().GetSession(ctx, sessionID)
		if err != nil {
			inboundLog().Debug(fmt.Sprintf("Completion ownership lookup failed for %s", sessionID))
			return "retry", sessionID, nil
		}
		if row == nil {
			return "terminal", sessionID, nil
		}
		if pyrepr.Truthy(rowField(row, "ended_at")) {
			if reason, ok := rowField(row, "end_reason").(string); ok && userBoundaryEndReasons[reason] {
				return "terminal", sessionID, row
			}
		}
		if sessionID == currentSessionID {
			return "deliver", sessionID, row
		}
		if currentSessionID != "" && rowField(row, "end_reason") == "compression" {
			tip, err := db.DB().GetCompressionTip(ctx, sessionID)
			if err != nil {
				inboundLog().Debug(fmt.Sprintf("Completion compression lookup failed for %s", sessionID))
				return "retry", sessionID, nil
			}
			if tip == currentSessionID {
				return "deliver", sessionID, row
			}
		}
		config := rowField(row, "model_config")
		if text, ok := config.(string); ok {
			decoded, err := pyjson.Loads(text)
			if err != nil {
				return "terminal", sessionID, nil
			}
			config = decoded
		}
		if config == nil {
			config = pyjson.NewObject()
		}
		object, ok := config.(*pyjson.Object)
		if !ok {
			return "terminal", sessionID, nil
		}
		parent, has := object.Get("_delegate_from")
		if !has {
			if rowField(row, "source") == "subagent" {
				return "terminal", sessionID, row
			}
			return "deliver", sessionID, row
		}
		text, isText := parent.(string)
		if !isText || pystr.Strip(text) == "" || hops == maxDelegateProvenanceHops {
			return "terminal", sessionID, nil
		}
		sessionID = text
	}
	return "terminal", sessionID, nil
}

// True when a refused notice is pinned to a permanently gone session, logged once when asked
func (r *GatewayRunner) watchPinGone(ctx context.Context, evt map[string]any, refusal string, log bool) (bool, error) {
	pinned := pystr.Strip(truthyText(evt["parent_session_id"]))
	if pinned == "" {
		return false, nil
	}
	sessionKey := truthyText(evt["session_key"])
	verdict, _, _, err := r.classifyCompletionOwner(ctx, pinned, sessionKey)
	if err != nil || verdict != "terminal" {
		return false, err
	}
	if !log {
		return true, nil
	}
	kind := truthyText(evt["type"])
	if kind == "" {
		kind = "watch"
	}
	process := truthyText(evt["session_id"])
	if process == "" {
		process = "<unknown>"
	}
	route, _ := r.sessionStore.PeekSessionID(sessionKey)
	if route == "" {
		route = "<none>"
	}
	inboundLog().Warn(fmt.Sprintf("Dropping %s notification of process %s: pinned session %s is permanently gone (closed "+
		"at a user boundary such as /new, or no longer an owner); the route now runs %s and "+
		"refuses it (%s). Output remains available via process(action='log').", kind, process, pinned, route, refusal))
	return true, nil
}

// Verified owning gateway session of an async completion, nil when ownership fails closed
func (r *GatewayRunner) resolveAsyncDelegationSession(ctx context.Context, entry *gateway.SessionEntry, pinned, proven string, completionIDs []string) (*gateway.SessionEntry, error) {
	db := r.sessionDB(ctx)
	if db == nil {
		inboundLog().Warn("Async-delegation completion has no session database; dropping injection (#55578 fail-closed).")
		return nil, nil
	}
	if proven != "" && proven == entry.SessionID {
		return entry, nil
	}
	generation := r.currentSessionRunGeneration(entry.SessionKey)
	verdict, pinned, pinnedRow := r.lookupCompletionOwner(ctx, pinned, entry.SessionID)
	if verdict != "deliver" || pinnedRow == nil {
		r.requeueUncertainCompletion(ctx, verdict, completionIDs)
		return nil, nil
	}
	target := pinned
	followsCompression := false
	if pyrepr.Truthy(rowField(pinnedRow, "ended_at")) {
		endReason := truthyText(rowField(pinnedRow, "end_reason"))
		if userBoundaryEndReasons[endReason] {
			inboundLog().Warn(fmt.Sprintf("Async-delegation completion pinned to user-closed session %s (end_reason=%s); dropping injection instead of resurrecting it (#55578 fail-closed).",
				pinned, pyrepr.Repr(endReason)))
			return nil, nil
		}
		if endReason != "compression" {
			shown := endReason
			if shown == "" {
				shown = "idle"
			}
			inboundLog().Info(fmt.Sprintf("Async-delegation completion pinned to %s-ended session %s; retargeting to the chat's current session %s.",
				shown, pinned, entry.SessionID))
			return entry, nil
		}
		followsCompression = true
		verdict, target = r.resolveCompressionLineageTarget(ctx, db, entry, pinned)
		if verdict != "deliver" || target == "" {
			r.requeueUncertainCompletion(ctx, verdict, completionIDs)
			return nil, nil
		}
	}
	if target == entry.SessionID {
		return entry, nil
	}
	prior := entry.SessionID
	if !r.isSessionRunCurrent(entry.SessionKey, generation) {
		inboundLog().Warn(fmt.Sprintf("Async-delegation completion for routing key %s was invalidated while resolving pinned session %s; leaving the route on %s and dropping injection.",
			entry.SessionKey, pinned, prior))
		return nil, nil
	}
	store := r.asyncStore().Store()
	var switched *gateway.SessionEntry
	var err error
	if followsCompression {
		switched, err = store.AdvanceCompressionSession(ctx, entry.SessionKey, prior, target)
	} else {
		switched, err = store.SwitchSession(ctx, entry.SessionKey, target, &prior, true)
	}
	if err != nil {
		return nil, err
	}
	if switched == nil {
		inboundLog().Warn(fmt.Sprintf("Async-delegation completion could not bind routing key %s to owning session %s (route moved or unknown); dropping injection.",
			entry.SessionKey, target))
		return nil, nil
	}
	inboundLog().Info(fmt.Sprintf("Pinned async-delegation completion to owning session %s (was %s) for routing key %s (#57498)", target, prior, entry.SessionKey))
	return switched, nil
}

// Offers admitted completions again when their owner could not be confirmed, boundary verdicts stay dropped
func (r *GatewayRunner) requeueUncertainCompletion(ctx context.Context, verdict string, completionIDs []string) {
	if verdict != "retry" || len(completionIDs) == 0 {
		return
	}
	r.completionDeliveryLock.Lock()
	for _, id := range completionIDs {
		r.completionDeliveriesDelivered.Delete(completionIdentity{evtType: "async_delegation", producerID: id, incarnation: ""})
	}
	r.completionDeliveryLock.Unlock()
	for _, id := range completionIDs {
		requeued, err := tools.RequeueAdmittedCompletion(inboundToolsEnv(ctx), id, tools.DefaultProcessRegistry().CompletionQueue)
		if err != nil {
			inboundLog().Debug(fmt.Sprintf("Could not requeue admitted completion %s", id))
			requeued = false
		}
		outcome := "not offered again (result remains queryable)"
		if requeued {
			outcome = "offered again"
		}
		inboundLog().Warn(fmt.Sprintf("Async-delegation completion %s: owner not confirmed after admission; %s.", id, outcome))
	}
}

// Completion target verdict with the proven owning session and the session delivery reaches
func (r *GatewayRunner) classifyCompletionOwner(ctx context.Context, parentSessionID, sessionKey string) (string, string, string, error) {
	db := r.sessionDB(ctx)
	if db == nil {
		return "retry", "", "", nil
	}
	var entry *gateway.SessionEntry
	if sessionKey != "" {
		entry = r.sessionStore.LoadedEntry(ctx, sessionKey)
	}
	current := ""
	if entry != nil {
		current = entry.SessionID
	}
	verdict, parentSessionID, parent := r.lookupCompletionOwner(ctx, parentSessionID, current)
	target := parentSessionID
	closedTip, err := r.closedLocalTarget(ctx, db, entry, parentSessionID, parent)
	if err != nil {
		return "", "", "", err
	}
	if closedTip != "" {
		verdict, target = "deliver", closedTip
	} else if verdict == "deliver" && parent != nil && pyrepr.Truthy(rowField(parent, "ended_at")) {
		endReason := truthyText(rowField(parent, "end_reason"))
		if endReason != "compression" {
			verdict = "deliver"
			if userBoundaryEndReasons[endReason] {
				verdict = "terminal"
			}
		} else {
			verdict, target = r.resolveCompressionLineageTarget(ctx, db, entry, parentSessionID)
		}
	}
	if verdict != "deliver" {
		return verdict, "", "", nil
	}
	if target == "" {
		target = parentSessionID
	}
	return verdict, parentSessionID, target, nil
}
