package gateway

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runner surface the session authority reads, the gateway runner's drain state, session store and adapters
type AuthorityRunner interface {
	DrainRunner
	SessionStore() *SessionStore
	AdapterForSource(ctx context.Context, source *SessionSource) PlatformAdapter
	MultiplexProfiles() bool
}

// Runner that republishes its in flight agent count once counted owner work ends
type activeAgentsPersister interface {
	PersistActiveAgents()
}

// One gateway owned live session with its route, event stream and drain state
type LiveSession struct {
	Source      *SessionSource
	Route       string
	EventStream *SessionEvents
	drain       chan struct{}
	Rescan      bool
	Settling    map[string]*pyjson.Object
	RetireTimer *time.Timer
}

// Live session on a route with a fresh event stream
func NewLiveSession(source *SessionSource, route string) *LiveSession {
	return &LiveSession{Source: source, Route: route, EventStream: NewSessionEvents(), Settling: map[string]*pyjson.Object{}}
}

// Gateway owned live sessions and canonical durable FIFO scheduling of one profile store
type SessionAuthority struct {
	mu                sync.Mutex
	Runner            AuthorityRunner
	ProfileID         string
	InstanceID        string
	DB                *state.SessionDB
	Epoch             int64
	Sessions          map[string]*LiveSession
	waiters           map[string]chan error
	nativeWaiters     map[string]bool
	PendingResults    map[string]*pyjson.Object
	cancelObligations map[string]bool
	Retiring          bool
	Retired           bool
	retiredRoutes     map[string]string
	rotatedOwners     map[string]string
	cronCancellations map[string]*pythread.Event
	orderedWrites     sync.Mutex
	mutationTasks     map[*trackedTask]bool
}

// Authority over a profile store at its runtime epoch
func NewSessionAuthority(runner AuthorityRunner, profileID, instanceID string, db *state.SessionDB, epoch int64) *SessionAuthority {
	return &SessionAuthority{Runner: runner, ProfileID: profileID, InstanceID: instanceID, DB: db, Epoch: epoch,
		Sessions: map[string]*LiveSession{}, waiters: map[string]chan error{}, nativeWaiters: map[string]bool{},
		PendingResults: map[string]*pyjson.Object{}, cancelObligations: map[string]bool{}, retiredRoutes: map[string]string{},
		rotatedOwners: map[string]string{}, mutationTasks: map[*trackedTask]bool{}}
}

// Path of the authority's state database, the key the cron owner registry binds by
func (a *SessionAuthority) DBPath() string { return a.DB.Path() }

// Live session of an id under the authority lock
func (a *SessionAuthority) live(sessionID string) (*LiveSession, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	live, ok := a.Sessions[sessionID]
	return live, ok
}

// Refusal with a bounded wire reason
func runtimeRefusal(reason string) error {
	return state.NewRuntimeStoreError(reason, nil, nil)
}

// True when err is a runtime store refusal with reason
func isRuntimeRefusal(err error, reason string) bool {
	var refusal *state.RuntimeStoreError
	return errors.As(err, &refusal) && refusal.Reason == reason
}

// Checks serving, profile, capability and ownership of a session, restoring a stored one into memory
func (a *SessionAuthority) Authorize(ctx context.Context, actor Principal, ref SessionRef, capability string) error {
	if err := requireServing(a); err != nil {
		return err
	}
	if actor.ProfileID != a.ProfileID || ref.ProfileID != a.ProfileID {
		return runtimeRefusal("profile_mismatch")
	}
	if !actor.Capabilities[capability] {
		return runtimeRefusal("permission_denied")
	}
	if _, ok := a.live(ref.SessionID); !ok {
		row, err := a.DB.GetSession(ctx, ref.SessionID)
		if err != nil {
			return err
		}
		if row == nil {
			return runtimeRefusal("not_found")
		}
		local := false
		err = a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
			rows, qerr := conn.QueryContext(ctx, "SELECT 1 FROM state_meta WHERE key IN (?,?)",
				state.LocalPolicyPrefix+ref.SessionID, state.LocalLegacyPrefix+ref.SessionID)
			if qerr != nil {
				return qerr
			}
			defer rows.Close()
			local = rows.Next()
			return rows.Err()
		})
		if err != nil {
			return err
		}
		chatID, _ := row.Get("chat_id")
		chatText := ""
		if pyTruthy(chatID) {
			chatText = pyrepr.Str(chatID)
		}
		if local || strings.HasPrefix(chatText, "local-") {
			if _, err := restoreLocalSession(ctx, a, ref.SessionID); err != nil {
				return err
			}
		} else {
			routed, err := restoreRoutedSession(ctx, a, ref.SessionID)
			if err != nil {
				return err
			}
			if routed == nil {
				mounted, err := mountHistory(ctx, a, actor, ref.SessionID)
				if err != nil {
					return err
				}
				if !mounted {
					if err := restoreAPISession(ctx, a, ref.SessionID); err != nil {
						return err
					}
				}
			}
		}
	}
	live, ok := a.live(ref.SessionID)
	if !ok {
		return runtimeRefusal("not_found")
	}
	if isHistoryMount(live) {
		if err := requireHistoryAccess(ctx, a, actor, ref.SessionID, capability); err != nil {
			return err
		}
	}
	source := live.Source
	if source != nil && source.Platform == PlatformLocal && !SameOwner(optStrAny(source.UserID), actor.Subject) &&
		!actor.Capabilities["session:operator"] {
		return runtimeRefusal("permission_denied")
	}
	touchRestored(ctx, a, ref.SessionID)
	return nil
}

// Python value of an optional string, nil for None
func optStrAny(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// Refuses new work while retiring or draining, a queue only commit passing the restart wait
func (a *SessionAuthority) requireAdmissionOpen(queueOnly bool) error {
	if a.Retiring {
		return runtimeRefusal("runtime_draining")
	}
	if a.Runner.Draining() && !(queueOnly && RestartWaitOpen(a.Runner)) {
		return runtimeRefusal("runtime_draining")
	}
	return nil
}

// Admission gate re-checked inside the off loop write transaction with the caller's own guard
func (a *SessionAuthority) admissionGate(authorize func(ctx context.Context, conn state.SQLiteConn) error, queueOnly bool) func(ctx context.Context, conn state.SQLiteConn) error {
	return func(ctx context.Context, conn state.SQLiteConn) error {
		if err := a.requireAdmissionOpen(queueOnly); err != nil {
			return err
		}
		if authorize != nil {
			return authorize(ctx, conn)
		}
		return nil
	}
}

// Compression lineage root of a session, the FIFO and admission identity of its route
func (a *SessionAuthority) LogicalOwner(ctx context.Context, sessionID string) (string, error) {
	if sessionID == "" {
		return sessionID, nil
	}
	lineage, err := a.DB.GetCompressionLineage(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if len(lineage) > 0 {
		return lineage[0], nil
	}
	return sessionID, nil
}

// Compression tip of a session, the session itself without one
func (a *SessionAuthority) PhysicalTarget(ctx context.Context, ref SessionRef) (string, error) {
	tip, err := a.DB.GetCompressionTip(ctx, ref.SessionID)
	if err != nil || tip != "" {
		return tip, err
	}
	return ref.SessionID, nil
}

// Integer column of a row, nil for NULL
func rowInt(row *pyjson.Object, key string) *int64 {
	v, _ := row.Get(key)
	switch n := v.(type) {
	case int:
		x := int64(n)
		return &x
	case int64:
		return &n
	case float64:
		x := int64(n)
		return &x
	}
	return nil
}

// String column of a row, empty for NULL
func rowString(row *pyjson.Object, key string) string {
	v, _ := row.Get(key)
	s, _ := v.(string)
	return s
}

// Public receipt of an admission row, a steering handoff reading as its running execution
func (a *SessionAuthority) receiptOf(row *pyjson.Object) AdmissionReceipt {
	status := rowString(row, "status")
	outcomeValue, _ := row.Get("outcome")
	var outcome *string
	if s, ok := outcomeValue.(string); ok {
		outcome = &s
	}
	if outcome != nil && *outcome == state.Steering {
		status, outcome = AdmissionStarted, nil
	}
	epoch := a.Epoch
	if owner := rowInt(row, "owner_epoch"); owner != nil && *owner != 0 {
		epoch = *owner
	}
	seq := int64(0)
	if s := rowInt(row, "seq"); s != nil {
		seq = *s
	}
	return AdmissionReceipt{AdmissionID: rowString(row, "admission_id"), Ref: SessionRef{a.ProfileID, rowString(row, "target_session_id")},
		Sequence: seq, Status: status, Outcome: outcome, AuthorityEpoch: epoch, ExecutionGeneration: rowInt(row, "generation")}
}

// Schedules the session's FIFO drain, starting one when none is alive
var scheduleSession = func(ctx context.Context, a *SessionAuthority, ref SessionRef) {
	restoreRetired(ctx, a, ref.SessionID)
	a.mu.Lock()
	live := a.Sessions[ref.SessionID]
	live.Rescan = true
	start := live.drain == nil
	if !start {
		select {
		case <-live.drain:
			start = true
		default:
		}
	}
	var done chan struct{}
	if start {
		done = make(chan struct{})
		live.drain = done
	}
	a.mu.Unlock()
	if !start {
		return
	}
	go func() {
		defer close(done)
		defer persistWorkCount(a)
		if err := drainSessionFIFO(context.WithoutCancel(ctx), a, ref); err != nil {
			logDrainFailure(err)
		}
	}()
}

// Logs a dead pump, the one failure the authority never swallows
func logDrainFailure(err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	slog.Error("Session drain task died: " + pyExceptionRepr(err))
}

// Publishes committed input and returns its receipt, marking a native event accepted
func (a *SessionAuthority) admitted(ctx context.Context, ref SessionRef, event *MessageEvent, row *pyjson.Object) (AdmissionReceipt, error) {
	if event != nil {
		event.setGatewayAccepted(true)
		if rowString(row, "status") == AdmissionQueued && event.OnCommit != nil {
			event.OnCommit()
		}
		markProducerAdmission(a, event, row)
	}
	if err := a.publishPending(ctx, ref); err != nil {
		return AdmissionReceipt{}, err
	}
	return a.receiptOf(row), nil
}

// Drain scheduling an admission's tracked write runs in the admitting caller's own step
func (a *SessionAuthority) scheduleAdmitted(ctx context.Context, ref SessionRef) {
	scheduleSession(ctx, a, ref)
}

// Publishes the pending queue, handle state and revision of a session to its viewers
func (a *SessionAuthority) publishPending(ctx context.Context, ref SessionRef) error {
	restoreRetired(ctx, a, ref.SessionID)
	live, _ := a.live(ref.SessionID)
	live.EventStream.Lock.Lock()
	err := func() error {
		defer live.EventStream.Lock.Unlock()
		handle, err := a.handle(ctx, ref)
		if err != nil {
			return err
		}
		rows, err := pendingRows(ctx, a, ref)
		if err != nil {
			return err
		}
		pending := make([]any, 0, len(rows))
		for _, row := range rows {
			receipt, err := a.pendingReceipt(ctx, row)
			if err != nil {
				return err
			}
			pending = append(pending, pendingAdmissionDict(receipt))
		}
		live.EventStream.Publish(ref.SessionID, pyjson.ObjectOf(
			"stored_session_id", ref.SessionID, "pending", pending, "authority_epoch", a.Epoch,
			"desktop_protocol", CanonicalGatewayProtocol, "running", handle.ExecutionState == "running",
			"execution_generation", handle.ExecutionGeneration, "revision", handle.Revision), "session.info", nil)
		return nil
	}()
	if err != nil {
		return err
	}
	wakeBotReceipts(ctx, a, ref.SessionID)
	return nil
}

// Handle of a session with its revision, generation and execution state
func (a *SessionAuthority) handle(ctx context.Context, ref SessionRef) (SessionHandle, error) {
	row, err := a.DB.GetSession(ctx, ref.SessionID)
	if err != nil {
		return SessionHandle{}, err
	}
	if row == nil {
		return SessionHandle{}, runtimeRefusal("not_found")
	}
	pending, err := a.pendingRowsMasked(ctx, ref)
	if err != nil {
		return SessionHandle{}, err
	}
	executionState := "idle"
	for _, r := range pending {
		if rowString(r, "status") == AdmissionUnknown {
			executionState = "unknown"
			break
		}
	}
	if executionState == "idle" {
		for _, r := range pending {
			if rowString(r, "status") == AdmissionStarted {
				executionState = "running"
				break
			}
		}
	}
	var revision, generation int64
	if v := rowInt(row, "runtime_revision"); v != nil {
		revision = *v
	}
	if v := rowInt(row, "runtime_generation"); v != nil {
		generation = *v
	}
	return SessionHandle{Ref: ref, InstanceID: a.InstanceID, AuthorityEpoch: a.Epoch, Revision: revision,
		ExecutionGeneration: generation, ExecutionState: executionState}, nil
}

// Pending admissions as the event stream presents them, an unpublished settlement reading as its claim
func (a *SessionAuthority) pendingRowsMasked(ctx context.Context, ref SessionRef) ([]*pyjson.Object, error) {
	rows, err := a.DB.ListSessionAdmissions(ctx, ref.SessionID, true)
	if err != nil {
		return nil, err
	}
	live, ok := a.live(ref.SessionID)
	if !ok || len(live.Settling) == 0 {
		return rows, nil
	}
	byID := map[string]*pyjson.Object{}
	var order []string
	for _, row := range rows {
		id := rowString(row, "admission_id")
		byID[id] = row
		order = append(order, id)
	}
	for id, row := range live.Settling {
		if _, seen := byID[id]; !seen {
			order = append(order, id)
		}
		byID[id] = row
	}
	out := make([]*pyjson.Object, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	sortRowsBySeq(out)
	return out, nil
}

// Stable sort of admission rows by their FIFO sequence
func sortRowsBySeq(rows []*pyjson.Object) {
	seqOf := func(r *pyjson.Object) int64 {
		if s := rowInt(r, "seq"); s != nil {
			return *s
		}
		return 0
	}
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && seqOf(rows[j]) < seqOf(rows[j-1]); j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}

// Viewer facing pending admission with only its public input text
func (a *SessionAuthority) pendingReceipt(ctx context.Context, row *pyjson.Object) (PendingAdmission, error) {
	payloadValue, _ := row.Get("payload")
	payload, _ := payloadValue.(*pyjson.Object)
	var text any
	if payload != nil {
		text, _ = payload.Get("text")
	}
	if text == nil {
		text = ""
		if payload != nil {
			if native, ok := payload.Get("native_text_v1"); ok {
				if obj, isObj := native.(*pyjson.Object); isObj {
					if event, ok := obj.Get("event"); ok {
						if eventObj, isObj := event.(*pyjson.Object); isObj {
							if t, ok := eventObj.Get("text"); ok {
								text = t
							}
						}
					}
				}
			}
		}
	}
	controls, err := queuedControls(ctx, a, row)
	if err != nil {
		return PendingAdmission{}, err
	}
	textStr, _ := text.(string)
	outcome, _ := row.Get("outcome")
	return PendingAdmission{AdmissionReceipt: a.receiptOf(row), InputID: rowString(row, "request_id"), Text: textStr,
		Controls: controls, Steering: outcome == state.Steering}, nil
}

// Dataclass dict of a pending admission as viewers receive it
func pendingAdmissionDict(p PendingAdmission) *pyjson.Object {
	var outcome, generation any
	if p.Outcome != nil {
		outcome = *p.Outcome
	}
	if p.ExecutionGeneration != nil {
		generation = *p.ExecutionGeneration
	}
	controls := make([]any, len(p.Controls))
	for i, c := range p.Controls {
		controls[i] = c
	}
	return pyjson.ObjectOf("admission_id", p.AdmissionID,
		"ref", pyjson.ObjectOf("profile_id", p.Ref.ProfileID, "session_id", p.Ref.SessionID),
		"sequence", p.Sequence, "status", p.Status, "outcome", outcome, "authority_epoch", p.AuthorityEpoch,
		"execution_generation", generation, "input_id", p.InputID, "text", p.Text, "controls", controls, "steering", p.Steering)
}

// Receipt of an admission the caller owns or controls, a terminal row awaiting publication reading as its claim
func (a *SessionAuthority) Receipt(ctx context.Context, actor Principal, ref SessionRef, admissionID string) (AdmissionReceipt, error) {
	if err := a.Authorize(ctx, actor, ref, "session:submit"); err != nil {
		return AdmissionReceipt{}, err
	}
	row, err := a.DB.GetSessionAdmission(ctx, admissionID)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	if row == nil || rowString(row, "target_session_id") != ref.SessionID {
		return AdmissionReceipt{}, runtimeRefusal("not_found")
	}
	principal, _ := row.Get("principal_id")
	if !SameOwner(principal, actor.Subject) && !actor.Capabilities["session:control"] {
		return AdmissionReceipt{}, runtimeRefusal("permission_denied")
	}
	live, _ := a.live(ref.SessionID)
	live.EventStream.Lock.Lock()
	if settled, ok := live.Settling[admissionID]; ok {
		row = settled
	}
	live.EventStream.Lock.Unlock()
	return a.receiptOf(row), nil
}

// Cancels a queued admission, committing and settling its observers as one tracked task
func (a *SessionAuthority) CancelQueued(ctx context.Context, actor Principal, ref SessionRef, admissionID string) (AdmissionReceipt, error) {
	before, err := a.Receipt(ctx, actor, ref, admissionID)
	if err != nil {
		return AdmissionReceipt{}, err
	}
	task := trackMutation(a, func(taskCtx context.Context) (any, error) {
		return a.commitCancel(taskCtx, ref, admissionID, before)
	})
	result, err := task.wait()
	if err != nil {
		return AdmissionReceipt{}, err
	}
	if err := releaseAdmissionMedia(ctx, a.DB, admissionID); err != nil {
		return AdmissionReceipt{}, err
	}
	return a.receiptOf(result.(*pyjson.Object)), nil
}

// Cancels a row read queued, returning the started row when the drain claimed it first
func (a *SessionAuthority) CancelOrClaimed(ctx context.Context, actor Principal, ref SessionRef, admissionID string) (*pyjson.Object, error) {
	_, err := a.CancelQueued(ctx, actor, ref, admissionID)
	if err == nil {
		return nil, nil
	}
	if !isRuntimeRefusal(err, "stale_generation") {
		return nil, err
	}
	row, err := a.DB.GetSessionAdmission(ctx, admissionID)
	if err != nil {
		return nil, err
	}
	if row == nil || rowString(row, "status") != AdmissionStarted {
		return nil, runtimeRefusal("stale_generation")
	}
	return row, nil
}

// Commits a cancellation off the loop and settles its observers once it lands
func (a *SessionAuthority) commitCancel(ctx context.Context, ref SessionRef, admissionID string, before AdmissionReceipt) (*pyjson.Object, error) {
	if before.Status == AdmissionQueued {
		a.mu.Lock()
		a.cancelObligations[admissionID] = true
		a.mu.Unlock()
	}
	row, err := a.DB.CancelSessionInput(ctx, a.Epoch, admissionID)
	if err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			a.mu.Lock()
			delete(a.cancelObligations, admissionID)
			a.mu.Unlock()
		}
		return nil, err
	}
	outcome, _ := row.Get("outcome")
	a.mu.Lock()
	owed := a.cancelObligations[admissionID]
	a.mu.Unlock()
	if rowString(row, "status") == AdmissionTerminal && outcome == "cancelled" && owed {
		if err := a.settleCancelled(ctx, ref, admissionID, "cancelled"); err != nil {
			return nil, err
		}
		return row, nil
	}
	if rowString(row, "status") != AdmissionQueued {
		a.mu.Lock()
		delete(a.cancelObligations, admissionID)
		a.mu.Unlock()
	}
	if err := a.publishPending(ctx, ref); err != nil {
		return nil, err
	}
	return row, nil
}

// Settles every observer of a queued row made terminal and schedules a fresh drain for its successors
func (a *SessionAuthority) settleCancelled(ctx context.Context, ref SessionRef, admissionID, outcome string) error {
	live, _ := a.live(ref.SessionID)
	stream := live.EventStream
	stream.Lock.Lock()
	err := func() error {
		defer stream.Lock.Unlock()
		if err := a.publishPending(ctx, ref); err != nil {
			return err
		}
		running := stream.Execution
		stream.Execution = pyjson.ObjectOf("authority_epoch", a.Epoch, "admission_id", admissionID)
		defer func() { stream.Execution = running }()
		stream.Publish(ref.SessionID, pyjson.ObjectOf("text", "", "content", "", "admission_id", admissionID, "outcome", outcome),
			"message.complete", nil)
		return nil
	}()
	if err != nil {
		return err
	}
	a.mu.Lock()
	delete(a.cancelObligations, admissionID)
	delete(a.nativeWaiters, admissionID)
	waiter, hasWaiter := a.waiters[admissionID]
	delete(a.waiters, admissionID)
	a.mu.Unlock()
	dropProducerTurn(a, admissionID)
	if hasWaiter {
		select {
		case waiter <- nil:
		default:
		}
	}
	scheduleSession(ctx, a, ref)
	observability.BusyRowSettled(ctx, admissionID, outcome)
	return nil
}

// True for a cold mount of a stored conversation, a live session with no source on a history route
func isHistoryMount(live *LiveSession) bool {
	return live.Source == nil && strings.HasPrefix(live.Route, historyMountRoutePrefix)
}

const historyMountRoutePrefix = "history:"

// Seconds a restored conversation stays in memory with no authorize before it retires again
const restoredIdleS = 30.0

type listingReadKey struct{}

// Context marking a listing read that must not push back a restored conversation's retirement
func WithListingRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, listingReadKey{}, true)
}

// True inside a listing read
func listingRead(ctx context.Context) bool {
	v, _ := ctx.Value(listingReadKey{}).(bool)
	return v
}

// Rearms the re-retirement check of a conversation retirement let go that an RPC brought back
func touchRestored(ctx context.Context, a *SessionAuthority, sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	live, ok := a.Sessions[sessionID]
	if !ok {
		return
	}
	if _, retired := a.retiredRoutes[sessionID]; !retired {
		return
	}
	if live.RetireTimer != nil {
		if listingRead(ctx) {
			return
		}
		live.RetireTimer.Stop()
	}
	live.RetireTimer = time.AfterFunc(time.Duration(restoredIdleS*float64(time.Second)), func() {
		retireRestored(context.WithoutCancel(ctx), a, sessionID, live)
	})
}
