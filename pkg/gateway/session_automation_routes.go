package gateway

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Producers whose queued route wakes follow the route to a fresh session
var routeWakeProducers = map[string]bool{"kanban-wake": true, "loop-tick": true}

// Mutation lock of a session's live owner held across a route rotation, nil without one
func routeOwnerLock(ctx context.Context, runner any, sessionID string) (*sync.Mutex, error) {
	a := activeAuthority(runner)
	if a == nil || sessionID == "" {
		return nil, nil
	}
	owner, err := a.LogicalOwner(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if live, ok := a.live(owner); ok {
		return &live.mutationLock, nil
	}
	return nil, nil
}

// Live session of an id, created on the route when absent, the Python setdefault
func setdefaultLive(a *SessionAuthority, sessionID string, source *SessionSource, route string) *LiveSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	if live, ok := a.Sessions[sessionID]; ok {
		return live
	}
	live := NewLiveSession(source, route)
	a.Sessions[sessionID] = live
	return live
}

// Carries the old owner's still queued route wakes to the route's fresh session after a rotation
func moveRouteWakes(ctx context.Context, runner AuthorityRunner, sessionKey, oldSessionID string) error {
	a := activeAuthority(runner)
	if a == nil || oldSessionID == "" {
		return nil
	}
	entry := runner.SessionStore().LookupBySessionKey(ctx, sessionKey)
	if entry == nil {
		return nil
	}
	oldOwner, err := a.LogicalOwner(ctx, oldSessionID)
	if err != nil {
		return err
	}
	newOwner, err := a.LogicalOwner(ctx, entry.SessionID)
	if err != nil {
		return err
	}
	old, fresh := SessionRef{a.ProfileID, oldOwner}, SessionRef{a.ProfileID, newOwner}
	if _, ok := a.live(old.SessionID); !ok || old == fresh {
		return nil
	}
	rows, err := a.DB.ListSessionAdmissions(ctx, old.SessionID, true)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if rowString(row, "status") == "unknown" {
			return nil
		}
	}
	actor := Principal{Subject: "messaging-control", ProfileID: a.ProfileID,
		Capabilities: map[string]bool{"session:submit": true, "session:control": true}, TransportID: "reset"}
	for _, row := range rows {
		native, _ := rowPayload(row).GetOr("native_text_v1", pyjson.NewObject()).(*pyjson.Object)
		automation := native.GetOr("automation", pyjson.NewObject())
		requestID := rowString(row, "request_id")
		var identity []any
		if pyTruthy(automation) && strings.HasPrefix(requestID, "[") {
			decoded, err := pyjson.Loads(requestID)
			if err != nil {
				return err
			}
			identity, _ = decoded.([]any)
		}
		if rowString(row, "status") != "queued" || rowString(row, "principal_id") != "automation:"+sessionKey || len(identity) == 0 {
			continue
		}
		if producer, isText := identity[0].(string); !isText || !routeWakeProducers[producer] {
			continue
		}
		if _, err := a.CancelQueued(ctx, actor, old, rowString(row, "admission_id")); err != nil {
			var refusal *state.RuntimeStoreError
			if errors.As(err, &refusal) {
				continue
			}
			return err
		}
		payload := pyjson.Clone(rowPayload(row)).(*pyjson.Object)
		movedNative := payload.Vals["native_text_v1"].(*pyjson.Object)
		movedNative.Vals["automation"].(*pyjson.Object).Set("owner", fresh.SessionID)
		oldLive, _ := a.live(old.SessionID)
		setdefaultLive(a, fresh.SessionID, laneSource(oldLive.Source), sessionKey)
		principal, request := rowString(row, "principal_id"), requestID
		_, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
			return a.DB.AdmitSessionInput(taskCtx, state.SessionInput{Epoch: a.Epoch, PrincipalID: principal, SessionID: fresh.SessionID,
				RequestID: request, Payload: payload, AuthorizeWrite: a.admissionGate(nil, false)})
		}, func(taskCtx context.Context, result any) (any, error) {
			return a.admitted(taskCtx, fresh, nil, result.(*pyjson.Object))
		}, true, func(any) { _ = a.scheduleAdmitted(context.WithoutCancel(ctx), fresh) })
		if err != nil {
			return err
		}
	}
	return nil
}

// Local FIFO serving a session key, its owner being the receipt id a reset keeps, nil when none
func localOwner(a *SessionAuthority, sessionKey string) *SessionRef {
	a.mu.Lock()
	ids := make([]string, 0, len(a.Sessions))
	for sid := range a.Sessions {
		ids = append(ids, sid)
	}
	a.mu.Unlock()
	sort.Strings(ids)
	for _, sid := range ids {
		live, ok := a.live(sid)
		if ok && live.Route == sessionKey && live.Source != nil && live.Source.Platform == PlatformLocal {
			return &SessionRef{a.ProfileID, sid}
		}
	}
	return nil
}

// Resets a local route keeping its FIFO owner and retargets its queued automation rows at the fresh transcript
func resetAndRetargetLocal(ctx context.Context, a *SessionAuthority, runner AuthorityRunner, sessionKey, oldSessionID string, owner SessionRef) (*SessionEntry, error) {
	live, ok := a.live(owner.SessionID)
	if !ok {
		return nil, &pyKeyError{owner.SessionID}
	}
	type rotated struct {
		entry *SessionEntry
		moved []string
	}
	live.mutationLock.Lock()
	result, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
		oldTargets, err := a.DB.GetCompressionLineage(taskCtx, oldSessionID)
		if err != nil {
			return nil, err
		}
		if len(oldTargets) == 0 {
			oldTargets = []string{oldSessionID}
		}
		entry, err := runner.SessionStore().ResetSession(taskCtx, sessionKey, nil)
		if err != nil {
			return nil, err
		}
		if entry == nil || containsText(oldTargets, entry.SessionID) {
			return rotated{entry, nil}, nil
		}
		moved, err := a.DB.RetargetLocalAutomation(taskCtx, a.Epoch, owner.SessionID, "automation:"+sessionKey, oldTargets, entry.SessionID)
		return rotated{entry, moved}, err
	}, nil, true, nil)
	live.mutationLock.Unlock()
	if err != nil {
		return nil, err
	}
	done := result.(rotated)
	if len(done.moved) > 0 {
		if err := a.publishPending(ctx, owner); err != nil {
			return nil, err
		}
	}
	return done.entry, nil
}

// Whether a list holds a text
func containsText(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// Compression exhaustion reset rotating the route and moving the old owner's queued messaging rows onto the fresh session
func resetAndRehomeMessages(ctx context.Context, runner AuthorityRunner, sessionKey, oldSessionID string) (*SessionEntry, func(), error) {
	noop := func() {}
	a := activeAuthority(runner)
	var old SessionRef
	var live *LiveSession
	if a != nil {
		ownerID, err := a.LogicalOwner(ctx, oldSessionID)
		if err != nil {
			return nil, nil, err
		}
		old = SessionRef{a.ProfileID, ownerID}
		if oldSessionID != "" {
			live, _ = a.live(old.SessionID)
		}
		if local := localOwner(a, sessionKey); local != nil {
			entry, err := resetAndRetargetLocal(ctx, a, runner, sessionKey, oldSessionID, *local)
			return entry, noop, err
		}
	}
	if live == nil {
		entry, err := runner.SessionStore().ResetSession(ctx, sessionKey, nil)
		return entry, noop, err
	}
	chatSource := laneSource(live.Source)
	type rehomed struct {
		entry *SessionEntry
		fresh *SessionRef
		moved []string
		gate  *LiveSession
	}
	operation := func(taskCtx context.Context) (any, error) {
		a.orderedWrites.Lock()
		out, err := func() (rehomed, error) {
			entry, err := runner.SessionStore().ResetSession(taskCtx, sessionKey, nil)
			if err != nil || entry == nil {
				return rehomed{}, err
			}
			ownerID, err := a.LogicalOwner(taskCtx, entry.SessionID)
			if err != nil {
				return rehomed{}, err
			}
			fresh := SessionRef{a.ProfileID, ownerID}
			if fresh == old {
				return rehomed{entry: entry}, nil
			}
			row, err := a.DB.GetSession(taskCtx, fresh.SessionID)
			if err != nil {
				return rehomed{}, err
			}
			if row == nil {
				if err := a.DB.CreateSession(state.NewSession{ID: fresh.SessionID, Source: string(chatSource.Platform)}); err != nil {
					return rehomed{}, err
				}
			}
			a.mu.Lock()
			a.rotatedOwners[old.SessionID] = sessionKey
			a.mu.Unlock()
			moved, err := a.DB.RehomeQueuedInputs(taskCtx, a.Epoch, old.SessionID, fresh.SessionID, "messaging:")
			return rehomed{entry: entry, fresh: &fresh, moved: moved}, err
		}()
		a.orderedWrites.Unlock()
		if err != nil || out.fresh == nil {
			return out, err
		}
		out.gate = setdefaultLive(a, out.fresh.SessionID, chatSource, sessionKey)
		out.gate.claimGate.Lock()
		return out, nil
	}
	live.mutationLock.Lock()
	task := trackMutation(a, operation)
	select {
	case <-task.done:
	case <-ctx.Done():
		live.mutationLock.Unlock()
		go func() {
			result, err := task.wait()
			if done, ok := result.(rehomed); err == nil && ok && done.gate != nil {
				done.gate.claimGate.Unlock()
				_ = scheduleSession(context.Background(), a, *done.fresh)
			}
		}()
		return nil, nil, ctx.Err()
	}
	live.mutationLock.Unlock()
	if task.err != nil {
		return nil, nil, task.err
	}
	done := task.result.(rehomed)
	if done.gate == nil {
		return done.entry, noop, nil
	}
	if len(done.moved) > 0 {
		if err := a.publishPending(ctx, old); err != nil {
			return nil, nil, err
		}
		if err := a.publishPending(ctx, *done.fresh); err != nil {
			return nil, nil, err
		}
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			done.gate.claimGate.Unlock()
			if current, ok := a.live(done.fresh.SessionID); ok && current == done.gate {
				_ = scheduleSession(context.WithoutCancel(ctx), a, *done.fresh)
			}
		})
	}
	return done.entry, release, nil
}

// Route wake move after a rotation, for the runner's reset paths
func MoveRouteWakes(ctx context.Context, runner AuthorityRunner, sessionKey, oldSessionID string) error {
	return moveRouteWakes(ctx, runner, sessionKey, oldSessionID)
}

// Owner mutation lock held across a route rotation, for the runner's reset paths
func RouteOwnerLock(ctx context.Context, runner any, sessionID string) (*sync.Mutex, error) {
	return routeOwnerLock(ctx, runner, sessionID)
}

// Compression exhaustion reset with its messaging rows rehomed, for the runner's turn recovery
func ResetAndRehomeMessages(ctx context.Context, runner AuthorityRunner, sessionKey, oldSessionID string) (*SessionEntry, func(), error) {
	return resetAndRehomeMessages(ctx, runner, sessionKey, oldSessionID)
}
