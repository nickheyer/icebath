package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

var submitPayloadFields = map[string]bool{"text": true, "attachments": true, "finite": true, "unattended": true, "surface": true,
	"voice_context": true, "interrupted": true, "voice_turn": true, "display_kind": true, "title_preview": true, "turn_author": true,
	"api_text": true, "model_override": true, "clarify_batch": true}

// Trusted caller options of a submission, never client payload fields
type SubmitOptions struct {
	AuthorizeWrite       func(ctx context.Context, conn state.SQLiteConn) error
	QueueWhileRestarting bool
	ReplayGuard          func(ctx context.Context, conn state.SQLiteConn) error
	KanbanCard           any
}

// Runner that keeps one resident agent per running key
type residentAgentRunner interface {
	ResidentAgentFor(key string) any
}

// Runner that keeps cached agents per running key
type cachedAgentRunner interface {
	CachedAgentFor(key string) any
}

type nativeWriteStartedKey struct{}

// Context whose admitting caller learns when its shielded ledger write began
func withNativeWriteFlag(ctx context.Context) (context.Context, func() bool) {
	started := new(bool)
	return context.WithValue(ctx, nativeWriteStartedKey{}, started), func() bool { return *started }
}

// Marks the admitting caller's shielded ledger write begun
func markNativeWriteStarted(ctx context.Context) {
	if started, ok := ctx.Value(nativeWriteStartedKey{}).(*bool); ok {
		*started = true
	}
}

// Dataclass dict of an admission receipt
func admissionReceiptDict(r AdmissionReceipt) *pyjson.Object {
	var outcome, generation any
	if r.Outcome != nil {
		outcome = *r.Outcome
	}
	if r.ExecutionGeneration != nil {
		generation = *r.ExecutionGeneration
	}
	return pyjson.ObjectOf("admission_id", r.AdmissionID,
		"ref", pyjson.ObjectOf("profile_id", r.Ref.ProfileID, "session_id", r.Ref.SessionID),
		"sequence", r.Sequence, "status", r.Status, "outcome", outcome, "authority_epoch", r.AuthorityEpoch,
		"execution_generation", generation)
}

// Binds a messaging source to its session, the routing entry reserved and the live entry published
func (a *SessionAuthority) Register(ctx context.Context, source *SessionSource) (SessionRef, error) {
	sid, route, err := a.reserveRoute(ctx, source)
	if err != nil {
		return SessionRef{}, err
	}
	return a.publishRoute(ctx, source, sid, route)
}

// Durable half of a registration, the routing entry and the reserved session row
func (a *SessionAuthority) reserveRoute(ctx context.Context, source *SessionSource) (string, string, error) {
	if err := a.requireAdmissionOpen(false); err != nil {
		return "", "", err
	}
	entry, err := a.Runner.SessionStore().GetOrCreateSession(ctx, source, false, true)
	if err != nil {
		return "", "", err
	}
	sid, err := a.LogicalOwner(ctx, entry.SessionID)
	if err != nil {
		return "", "", err
	}
	row, err := a.DB.GetSession(ctx, sid)
	if err != nil {
		return "", "", err
	}
	if row == nil {
		if err := a.DB.CreateSession(state.NewSession{ID: sid, Source: string(source.Platform)}); err != nil {
			return "", "", err
		}
	}
	return sid, entry.SessionKey, nil
}

// Publishes the live entry of a reserved route and names its lane
func (a *SessionAuthority) publishRoute(ctx context.Context, source *SessionSource, sid, route string) (SessionRef, error) {
	if err := a.requireAdmissionOpen(false); err != nil {
		return SessionRef{}, err
	}
	a.mu.Lock()
	if _, ok := a.Sessions[sid]; !ok {
		a.Sessions[sid] = NewLiveSession(source, route)
	}
	a.mu.Unlock()
	if err := a.nameLane(ctx, route, sid, source); err != nil {
		return SessionRef{}, err
	}
	return SessionRef{a.ProfileID, sid}, nil
}

// Makes the chat on a route name one conversation, every other live session there detaching onto its own key
func (a *SessionAuthority) nameLane(ctx context.Context, route, sid string, source *SessionSource) error {
	a.mu.Lock()
	type lane struct {
		sid  string
		live *LiveSession
	}
	var others []lane
	for otherSID, other := range a.Sessions {
		if other.Route == route && otherSID != sid {
			others = append(others, lane{otherSID, other})
		}
	}
	a.mu.Unlock()
	sort.Slice(others, func(i, j int) bool { return others[i].sid < others[j].sid })
	for _, other := range others {
		if err := detach(ctx, a, other.live, other.sid); err != nil {
			return err
		}
	}
	live, ok := a.live(sid)
	if !ok {
		return nil
	}
	a.mu.Lock()
	moved := live.Route != route
	left := ""
	if moved {
		if liveDetached(live) {
			left = live.Route
		}
		live.Source, live.Route = source, route
	}
	a.mu.Unlock()
	if left != "" {
		return forgetDetached(ctx, a, left)
	}
	return nil
}

// Agent the runner holds for a session's running key
func (a *SessionAuthority) agent(ref SessionRef) (any, error) {
	live, ok := a.live(ref.SessionID)
	if !ok {
		return nil, &pyKeyError{ref.SessionID}
	}
	key := runningKey(a.Runner, live.Route, ref.SessionID)
	if resident, ok := a.Runner.(residentAgentRunner); ok {
		return resident.ResidentAgentFor(key), nil
	}
	if cached, ok := a.Runner.(cachedAgentRunner); ok {
		return cached.CachedAgentFor(key), nil
	}
	return nil, fmt.Errorf("'%T' object has no attribute '_cached_agent_for'", a.Runner)
}

// Handle of a session the actor may read
func (a *SessionAuthority) Resolve(ctx context.Context, actor Principal, ref SessionRef) (SessionHandle, error) {
	if err := a.Authorize(ctx, actor, ref, "session:read"); err != nil {
		return SessionHandle{}, err
	}
	return a.handle(ctx, ref)
}

// Runs a read, a refusal because the session was deleted after authorization reading as not found
func unlessDeleted[T any](ctx context.Context, a *SessionAuthority, ref SessionRef, read func() (T, error)) (T, error) {
	value, err := read()
	if err == nil {
		return value, nil
	}
	var refusal *state.RuntimeStoreError
	if !errors.As(err, &refusal) {
		return value, err
	}
	row, readErr := a.DB.GetSession(ctx, ref.SessionID)
	if readErr != nil {
		return value, readErr
	}
	if row == nil {
		return value, runtimeRefusal("not_found")
	}
	return value, err
}

// Commits an automation input of a producer into its owner's FIFO
func (a *SessionAuthority) AdmitAutomation(ctx context.Context, adapter PlatformAdapter, event *MessageEvent, identity string, replaces *string, fold []string) (AdmissionReceipt, error) {
	return admitAutomation(ctx, a, adapter, event, identity, replaces, fold)
}

// Commits a messaging input before its acknowledgement, in the order its route's inputs arrived
func (a *SessionAuthority) AdmitNative(ctx context.Context, event *MessageEvent) (AdmissionReceipt, error) {
	var receipt AdmissionReceipt
	err := a.routeTurn(ctx, event, func(turnCtx context.Context, fence func(ctx context.Context, conn state.SQLiteConn) error) error {
		leaseCtx, scope, release := captureLease(turnCtx, a)
		defer release()
		admitted, err := a.admitNative(leaseCtx, event, fence)
		if err != nil {
			if !scope.wrote() {
				if abandonErr := abandonCapture(leaseCtx, scope); abandonErr != nil {
					return abandonErr
				}
			}
			return err
		}
		receipt = admitted
		return nil
	})
	return receipt, err
}

// Runs body as its route's turn, a messaging input given the fence a later stop or new trips
func (a *SessionAuthority) routeTurn(ctx context.Context, event *MessageEvent, body func(ctx context.Context, fence func(ctx context.Context, conn state.SQLiteConn) error) error) error {
	ingress, _ := ctx.Value(busyAdmissionKey{}).(*busyAdmission)
	ctx = context.WithValue(ctx, busyAdmissionKey{}, (*busyAdmission)(nil))
	key := ""
	if event.Source != nil {
		generated, err := a.Runner.SessionStore().generateSessionKey(ctx, event.Source, nil)
		if err != nil {
			var attribute *PyError
			if !errors.As(err, &attribute) || attribute.Kind != "AttributeError" {
				return err
			}
		} else {
			key = generated
		}
	}
	if key == "" {
		return body(ctx, nil)
	}
	arrived := a.RouteEpoch(key)
	fence := func(context.Context, state.SQLiteConn) error {
		if a.RouteEpoch(key) != arrived {
			field, detail := "route", "a /stop or /new of this chat came after this message"
			return state.NewRuntimeStoreError("stale_generation", &field, &detail)
		}
		return nil
	}
	queued := func() {
		if ingress != nil && ingress.accepted != nil && !ingress.accepted.isDone() {
			ingress.accepted.settle(nil, false)
		}
	}
	a.mu.Lock()
	if a.routeTurns == nil {
		a.routeTurns = map[string]*keyedTurnEntry{}
	}
	table := a.routeTurns
	a.mu.Unlock()
	return keyedTurn(ctx, &a.mu, table, key, queued, func(turnCtx context.Context) error {
		if ingress == nil {
			return body(turnCtx, nil)
		}
		if err := fence(turnCtx, nil); err != nil {
			return err
		}
		return body(turnCtx, fence)
	})
}

// How many messaging stop and new commands fenced a route
func (a *SessionAuthority) RouteEpoch(route string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.routeEpochs[route]
}

// Fences a route so its messaging input still capturing or waiting never commits
func (a *SessionAuthority) FenceRoute(route string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.routeEpochs == nil {
		a.routeEpochs = map[string]int{}
	}
	a.routeEpochs[route]++
}

// Prepares a messaging input against current connector policy and commits it in admission order
func (a *SessionAuthority) admitNative(ctx context.Context, event *MessageEvent, fence func(ctx context.Context, conn state.SQLiteConn) error) (AdmissionReceipt, error) {
	if err := a.requireAdmissionOpen(false); err != nil {
		return AdmissionReceipt{}, err
	}
	payload, err := prepareNative(ctx, a.Runner, event)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	nativeValue, _ := payload.Get("native_text_v1")
	native, _ := nativeValue.(*pyjson.Object)
	if native == nil {
		return AdmissionReceipt{}, &pyKeyError{"native_text_v1"}
	}
	if event.ingressSHA256 != "" {
		native.Set("ingress_sha256", event.ingressSHA256)
	}
	if err := a.requireAdmissionOpen(false); err != nil {
		return AdmissionReceipt{}, err
	}
	source, err := restoreNativeSource(ctx, payload, nil)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	type reserved struct{ sid, route string }
	reservation, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
		sid, route, err := a.reserveRoute(taskCtx, source)
		return reserved{sid, route}, err
	}, nil, false, nil)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	ref, err := a.publishRoute(ctx, source, reservation.(reserved).sid, reservation.(reserved).route)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	identity, err := pyjson.Dumps([]any{optStrAny(source.Profile), string(source.Platform), source.ChatID,
		optStrAny(source.ThreadID), optStrAny(source.UserID)}, pyjson.Separators(",", ":"))
	if err != nil {
		return AdmissionReceipt{}, err
	}
	eventValue, _ := native.Get("event")
	nativeEvent, _ := eventValue.(*pyjson.Object)
	if nativeEvent == nil {
		return AdmissionReceipt{}, &pyKeyError{"event"}
	}
	messageID, ok := nativeEvent.Get("message_id")
	if !ok {
		return AdmissionReceipt{}, &pyKeyError{"message_id"}
	}
	requestID := uuid4HexString()
	if pyTruthy(messageID) {
		requestID = pyrepr.Str(messageID)
	}
	if revision, _ := nativeEvent.Get("edit_revision"); pyTruthy(revision) {
		requestID += ":edit:" + pyrepr.Str(revision)
	}
	committed := ref
	rewrite := takeRetryRewrite(ctx, event, a.DB)
	if scope := leaseScope(ctx); scope != nil {
		scope.startWrite()
	}
	markNativeWriteStarted(ctx)
	authorize := composeRetryRewrite(rewrite, a.admissionGate(fence, false))
	principal := "messaging:" + identity
	var scheduleErr error
	result, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
		return a.admitNativeWrite(taskCtx, principal, ref.SessionID, requestID, payload, authorize, event.RawMessage)
	}, func(taskCtx context.Context, written any) (any, error) {
		row := written.(*pyjson.Object)
		target := rowString(row, "target_session_id")
		if target == "" {
			target = ref.SessionID
		}
		a.mu.Lock()
		committed = SessionRef{a.ProfileID, target}
		a.mu.Unlock()
		if rewrite != nil && rewrite.Applied {
			rewrite.OnCommit()
		}
		return a.admitted(taskCtx, SessionRef{a.ProfileID, target}, event, row)
	}, true, func(any) {
		a.mu.Lock()
		target := committed
		a.mu.Unlock()
		scheduleErr = a.scheduleAdmitted(ctx, target)
	})
	if err == nil {
		err = scheduleErr
	}
	if err != nil {
		return AdmissionReceipt{}, err
	}
	return result.(AdmissionReceipt), nil
}

// Reconciles a redelivery against the ledger and commits the row, a refused admission releasing its capture
func (a *SessionAuthority) admitNativeWrite(ctx context.Context, principalID, sessionID, requestID string, payload *pyjson.Object,
	authorize func(ctx context.Context, conn state.SQLiteConn) error, raw any) (*pyjson.Object, error) {
	fresh := nativeMedia(payload)
	owner, err := a.currentOwner(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	row, err := func() (*pyjson.Object, error) {
		reconciled, err := reconcileNativeRetry(ctx, a.DB, principalID, owner, requestID, payload, raw)
		if err != nil {
			return nil, err
		}
		return a.DB.AdmitSessionInput(ctx, state.SessionInput{Epoch: a.Epoch, PrincipalID: principalID, SessionID: owner,
			RequestID: requestID, Payload: reconciled, AuthorizeWrite: authorize})
	}()
	if err != nil && !errors.Is(err, context.Canceled) {
		dropCaptureLease(ctx, nil, nil)
		if _, releaseErr := releaseUnheldMedia(ctx, a.DB, fresh, false); releaseErr != nil {
			return nil, releaseErr
		}
	}
	return row, err
}

// Session an admission lands on once the exhaustion auto reset rotated its reserved owner away
func (a *SessionAuthority) currentOwner(ctx context.Context, sessionID string) (string, error) {
	a.mu.Lock()
	route, rotated := a.rotatedOwners[sessionID]
	a.mu.Unlock()
	if !rotated {
		return sessionID, nil
	}
	current := sessionID
	if entry := a.Runner.SessionStore().LookupBySessionKey(ctx, route); entry != nil {
		owner, err := a.LogicalOwner(ctx, entry.SessionID)
		if err != nil {
			return "", err
		}
		current = owner
	}
	if _, ok := a.live(current); ok {
		return current, nil
	}
	return sessionID, nil
}

// Server observed route of one native session to recover, its available source and adapter
type NativeBinding struct {
	SessionID string
	Source    *SessionSource
	Adapter   PlatformAdapter
}

// Binds only server observed native routes and schedules them, each session answering ready or its refusal reason
func (a *SessionAuthority) RecoverNativeSessions(ctx context.Context, bindings []NativeBinding) (*pyjson.Object, error) {
	counts := map[string]int{}
	for _, binding := range bindings {
		counts[binding.SessionID]++
	}
	results := pyjson.NewObject()
	for _, binding := range bindings {
		err := a.recoverNativeSession(ctx, binding, counts[binding.SessionID])
		var refusal *state.RuntimeStoreError
		switch {
		case err == nil:
			results.Set(binding.SessionID, "ready")
		case errors.As(err, &refusal):
			results.Set(binding.SessionID, refusal.Reason)
		default:
			return nil, err
		}
	}
	return results, nil
}

// Revalidates the queued rows of one native session against its observed route and schedules its drain
func (a *SessionAuthority) recoverNativeSession(ctx context.Context, binding NativeBinding, count int) error {
	sid := binding.SessionID
	if count != 1 {
		return runtimeRefusal("admission_conflict")
	}
	rows, err := a.DB.ListSessionAdmissions(ctx, sid, false)
	if err != nil {
		return err
	}
	var last *pyjson.Object
	for _, row := range rows {
		if _, ok := rowPayload(row).Get("native_text_v1"); ok {
			last = row
		}
	}
	if last == nil {
		return runtimeRefusal("not_found")
	}
	ref := SessionRef{a.ProfileID, sid}
	target, err := a.PhysicalTarget(ctx, ref)
	if err != nil {
		return err
	}
	source, route, err := checkNativeRoute(ctx, a.Runner, rowPayload(last), target, binding.Source, binding.Adapter)
	if err != nil {
		return err
	}
	unknown := false
	for _, row := range rows {
		if rowString(row, "status") == AdmissionUnknown {
			unknown = true
		}
		if rowString(row, "status") != AdmissionQueued {
			continue
		}
		payload := rowPayload(row)
		if _, ok := payload.Get("native_text_v1"); ok {
			if _, _, err := checkNativeRoute(ctx, a.Runner, payload, target, binding.Source, binding.Adapter); err != nil {
				return err
			}
		} else if _, ok := payload.Get("local_automation_v1"); ok {
			if err := checkLocalAutomation(ctx, a, ref, row, route); err != nil {
				return err
			}
		} else if err := checkLocalInput(a, ref, row, source); err != nil {
			return err
		}
	}
	if err := a.requireAdmissionOpen(false); err != nil {
		return err
	}
	a.mu.Lock()
	if _, ok := a.Sessions[sid]; !ok {
		a.Sessions[sid] = NewLiveSession(source, route)
	}
	a.mu.Unlock()
	if unknown {
		return runtimeRefusal("unknown_execution")
	}
	return scheduleSession(ctx, a, ref)
}

// Admits a viewer's input into its session's FIFO, an interactive caller queueing through a restart wait
func (a *SessionAuthority) Submit(ctx context.Context, actor Principal, request Submission, opts SubmitOptions) (AdmissionReceipt, error) {
	if err := a.Authorize(ctx, actor, request.Ref, "session:submit"); err != nil {
		return AdmissionReceipt{}, err
	}
	live, ok := a.live(request.Ref.SessionID)
	if !ok {
		return AdmissionReceipt{}, &pyKeyError{request.Ref.SessionID}
	}
	live.mutationLock.Lock()
	defer live.mutationLock.Unlock()
	if err := a.Authorize(ctx, actor, request.Ref, "session:submit"); err != nil {
		return AdmissionReceipt{}, err
	}
	leaseCtx, _, release := captureLease(ctx, nil)
	defer release()
	return a.submit(leaseCtx, actor, request, opts)
}

// Validates and commits a submission's payload, a refused write releasing the attachments it captured
func (a *SessionAuthority) submit(ctx context.Context, actor Principal, request Submission, opts SubmitOptions) (AdmissionReceipt, error) {
	queueOnly := opts.QueueWhileRestarting
	if err := a.requireAdmissionOpen(queueOnly); err != nil {
		return AdmissionReceipt{}, err
	}
	text, isText := request.Payload["text"].(string)
	if request.Intent != "queue" || !isText {
		return AdmissionReceipt{}, runtimeRefusal("invalid_params")
	}
	for key := range request.Payload {
		if !submitPayloadFields[key] {
			return AdmissionReceipt{}, runtimeRefusal("invalid_params")
		}
	}
	payload := pyjson.ObjectOf("text", text)
	finite, err := admitFinite(request.Payload)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	payload.Update(finite)
	surface, err := admitSurface(request.Payload)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	payload.Update(surface)
	display, err := AdmitDisplay(request.Payload)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	names := make([]string, 0, len(display))
	for name := range display {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		payload.Set(name, display[name])
	}
	cliTurn, err := admitCLITurn(request.Payload)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	payload.Update(cliTurn)
	attachments, err := admitAttachments(ctx, request.Payload["attachments"], func() (*pyjson.Object, error) {
		return a.DB.FindSessionAdmission(ctx, actor.Subject, request.Ref.SessionID, request.RequestID)
	})
	if err != nil {
		return AdmissionReceipt{}, err
	}
	payload.Update(attachments)
	if opts.KanbanCard != nil {
		payload.Set("kanban_card_v1", opts.KanbanCard)
	}
	live, ok := a.live(request.Ref.SessionID)
	if !ok {
		return AdmissionReceipt{}, &pyKeyError{request.Ref.SessionID}
	}
	if source := live.Source; source != nil && !SameOwner(optStrAny(source.UserID), actor.Subject) {
		payload.Set("local_operator_v1", pyjson.ObjectOf("profile_id", a.ProfileID, "session_id", request.Ref.SessionID,
			"principal_id", actor.Subject))
	}
	var scheduleErr error
	result, err := trackedWrite(ctx, a, func(taskCtx context.Context) (any, error) {
		row, err := a.DB.AdmitSessionInput(taskCtx, state.SessionInput{Epoch: a.Epoch, PrincipalID: actor.Subject,
			SessionID: request.Ref.SessionID, RequestID: request.RequestID, Payload: payload, Intent: request.Intent,
			ReplayGuard: opts.ReplayGuard, AuthorizeWrite: a.admissionGate(opts.AuthorizeWrite, queueOnly)})
		if err != nil && !errors.Is(err, context.Canceled) {
			a.releaseRefusedCapture(ctx, request, payload)
		}
		return row, err
	}, func(taskCtx context.Context, written any) (any, error) {
		return a.admitted(taskCtx, request.Ref, nil, written.(*pyjson.Object))
	}, true, func(any) { scheduleErr = a.scheduleAdmitted(ctx, request.Ref) })
	if err == nil {
		err = scheduleErr
	}
	if err != nil {
		return AdmissionReceipt{}, err
	}
	return result.(AdmissionReceipt), nil
}

// Retires and collects the bytes a refused submission captured, a failure logged and never replacing the refusal
func (a *SessionAuthority) releaseRefusedCapture(ctx context.Context, request Submission, payload *pyjson.Object) {
	value, _ := payload.Get("attachments_v1")
	attachments, _ := value.(*pyjson.Object)
	if attachments == nil {
		return
	}
	mediaValue, _ := attachments.Get("media")
	media, _ := mediaValue.([]any)
	if len(media) == 0 {
		return
	}
	dropCaptureLease(ctx, nil, nil)
	retired := pyjson.ObjectOf("attachments_v1", pyjson.ObjectOf("media", media))
	err := a.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error { return state.RetireMedia(ctx, conn, retired, "") })
	if err == nil {
		err = a.DB.CollectRetiredMedia(ctx)
	}
	if err != nil {
		slog.Error(fmt.Sprintf("Releasing refused attachments of request %s failed", request.RequestID), "error", err)
	}
}
