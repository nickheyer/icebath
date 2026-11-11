package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

const (
	hostedCancelRouteRetries       = 8
	RoomSessionSource              = "bot_room"
	MaxTerminalTextBytes           = 64 * 1024
	hostedTerminalTruncationNotice = "\n\n[Reply truncated. Ask the Bot to share the full result as a file.]"
	hostedStopPending              = "stop retry remains pending: %s"
)

var hostedStopAckStatuses = map[string]bool{"cancelled": true, "interrupted": true}

func hostedRuntimeLog() *slog.Logger {
	return slog.Default().With("logger", "tui_gateway.hosted_room_driver")
}

// Member route that cannot carry the turn's files, refused before submit
type AttachmentsUnsupportedError struct{ Msg string }

func (e *AttachmentsUnsupportedError) Error() string { return e.Msg }

// Python type name of the refusal
func (e *AttachmentsUnsupportedError) PyTypeName() string { return "AttachmentsUnsupportedError" }

// One fenced turn handed to a member transport, its terminal result reported through the callback
type HostedSubmit struct {
	Profile             string
	SessionID           string
	Prompt              string
	Source              string
	Task                TaskIdentity
	ExecutionGeneration int64
	OnTerminal          func(receipt map[string]any) error
	MemberID            *string
	Attachments         []any
}

// Normalized session operations the room driver drives in process or over a peer route
type InternalSessionRPC interface {
	AcceptsAttachments() bool
	ResolveExact(ctx context.Context, profile, title, source string) (map[string]any, error)
	Create(ctx context.Context, profile, title, source string) (map[string]any, error)
	Resume(ctx context.Context, profile, sessionID, source string) (map[string]any, error)
	Submit(ctx context.Context, req HostedSubmit) (map[string]any, error)
	History(ctx context.Context, profile, sessionID, source string) ([]map[string]any, error)
	Info(ctx context.Context, profile, sessionID, source string) (map[string]any, error)
	Interrupt(ctx context.Context, profile, sessionID, source, expectedTaskID string) (map[string]any, error)
}

// Transport of one member turn of a bound room
type MemberTransportResolver func(binding HostedRoomBinding, task map[string]any) (InternalSessionRPC, error)

// Current server issued authority coordinate of one hosted room
type HostedRoomBinding struct {
	RoomID         string
	GatewayID      string
	AuthorityEpoch int64
}

type hostedTerminalReceipt struct {
	status       string
	settlementID string
	result       map[string]any
}

type hostedRecoveryInspection struct {
	terminal *hostedTerminalReceipt
	active   bool
	status   string
}

// Construction arguments of a room runtime, the Python keyword defaults from the options constructor
type HostedRoomRuntimeOptions struct {
	DBPath                     string
	Rooms                      func() ([]HostedRoomBinding, error)
	TurnLock                   func(profile string) (release func(), err error)
	RPC                        InternalSessionRPC
	TransportResolver          MemberTransportResolver
	PrepareRoom                func(binding HostedRoomBinding) error
	PublishTerminal            func(binding HostedRoomBinding, task map[string]any) error
	PendingAction              func(roomID, memberID string, action map[string]any) error
	Clock                      func() float64
	LeaseTTLSeconds            float64
	PollIntervalSeconds        float64
	ActivePollIntervalSeconds  float64
	TurnTimeoutSeconds         float64
	IndeterminateDeferSeconds  float64
	MaxConcurrentRooms         int
	UnavailableRetryMinSeconds float64
	UnavailableRetryMaxSeconds float64
	ProcessGeneration          string
}

// Runtime options carrying the keyword defaults of the room runtime
func DefaultHostedRoomRuntimeOptions() HostedRoomRuntimeOptions {
	return HostedRoomRuntimeOptions{
		Clock: hostedWallClock, LeaseTTLSeconds: 30, PollIntervalSeconds: 5, ActivePollIntervalSeconds: 0.25,
		TurnTimeoutSeconds: 1830, IndeterminateDeferSeconds: 60, MaxConcurrentRooms: 4,
		UnavailableRetryMinSeconds: 1, UnavailableRetryMaxSeconds: 30,
	}
}

// Fixed room list as a provider
func FixedHostedRooms(bindings ...HostedRoomBinding) func() ([]HostedRoomBinding, error) {
	fixed := append([]HostedRoomBinding(nil), bindings...)
	return func() ([]HostedRoomBinding, error) { return fixed, nil }
}

// Goroutine standing in for one Python thread, its identity readable from inside
type hostedThread struct {
	gid  atomic.Uint64
	done chan struct{}
}

// Starts fn on a new goroutine handed its own thread
func startHostedThread(fn func(t *hostedThread)) *hostedThread {
	t := &hostedThread{done: make(chan struct{})}
	go func() {
		defer close(t.done)
		t.gid.Store(pythread.GoroutineID())
		fn(t)
	}()
	return t
}

func (t *hostedThread) alive() bool {
	select {
	case <-t.done:
		return false
	default:
		return true
	}
}

// Waits up to timeout seconds for the goroutine to finish
func (t *hostedThread) join(timeout float64) {
	if timeout <= 0 {
		return
	}
	waiter := &pythread.Event{}
	go func() {
		select {
		case <-t.done:
			waiter.Set()
		case <-waiter.Waiter():
		}
	}()
	waiter.Wait(timeout)
	waiter.Set()
}

func (t *hostedThread) isCurrent() bool {
	return t != nil && t.gid.Load() == pythread.GoroutineID()
}

type hostedRouteRetry struct{ delay, nextAttemptAt float64 }

type hostedAttemptKey struct {
	roomID, taskID string
	generation     int64
}

type hostedRouteKey struct{ roomID, memberID string }

// Runs queued hosted room tasks independently of Desktop connections
type HostedRoomRuntime struct {
	ctx                        context.Context
	DBPath                     string
	RPC                        InternalSessionRPC
	transportResolver          MemberTransportResolver
	turnLock                   func(profile string) (func(), error)
	prepareRoom                func(binding HostedRoomBinding) error
	publishTerminal            func(binding HostedRoomBinding, task map[string]any) error
	pendingAction              func(roomID, memberID string, action map[string]any) error
	clock                      func() float64
	LeaseTTLSeconds            float64
	PollIntervalSeconds        float64
	ActivePollIntervalSeconds  float64
	TurnTimeoutSeconds         float64
	IndeterminateDeferSeconds  float64
	MaxConcurrentRooms         int
	UnavailableRetryMinSeconds float64
	UnavailableRetryMaxSeconds float64
	ProcessGeneration          string
	roomsProvider              func() ([]HostedRoomBinding, error)

	stop, wake                pythread.Event
	thread                    *hostedThread
	lastError                 any
	roomThreads               map[string]*hostedThread
	roomsNeedingReschedule    map[string]bool
	leases                    map[string]DriverLease
	recoveredLeases           map[hostedLeaseKey]bool
	inspectedIndeterminate    map[hostedAttemptKey]bool
	ambiguousRooms            map[string]float64
	unavailableRouteRetries   map[hostedRouteKey]hostedRouteRetry
	blockedRooms              map[string]bool
	statusLock                sync.Mutex
	currentTasks              map[string]TaskIdentity
	currentOrder              []string
	roomScheduleCursor, cycle int
}

type hostedLeaseKey struct {
	roomID     string
	generation int64
}

// Validated room runtime over the options, a ValueError for a bound out of range
func NewHostedRoomRuntime(ctx context.Context, o HostedRoomRuntimeOptions) (*HostedRoomRuntime, error) {
	for _, positive := range []struct {
		name  string
		value float64
	}{{"lease_ttl_seconds", o.LeaseTTLSeconds}, {"poll_interval_seconds", o.PollIntervalSeconds},
		{"active_poll_interval_seconds", o.ActivePollIntervalSeconds}, {"turn_timeout_seconds", o.TurnTimeoutSeconds},
		{"indeterminate_defer_seconds", o.IndeterminateDeferSeconds}} {
		if positive.value <= 0 {
			return nil, &RoomValueError{Msg: positive.name + " must be positive"}
		}
	}
	if o.MaxConcurrentRooms < 1 {
		return nil, &RoomValueError{Msg: "max_concurrent_rooms must be a positive integer"}
	}
	if !(0 < o.UnavailableRetryMinSeconds && o.UnavailableRetryMinSeconds <= o.UnavailableRetryMaxSeconds) {
		return nil, &RoomValueError{Msg: "unavailable retry bounds are invalid"}
	}
	if o.RPC == nil && o.TransportResolver == nil {
		return nil, &RoomValueError{Msg: "rpc or transport_resolver is required"}
	}
	if o.Clock == nil {
		o.Clock = hostedWallClock
	}
	generation := o.ProcessGeneration
	if generation == "" {
		generation = uuid4HexString()
	}
	rooms := o.Rooms
	if rooms == nil {
		rooms = FixedHostedRooms()
	}
	return &HostedRoomRuntime{
		ctx: ctx, DBPath: o.DBPath, RPC: o.RPC, transportResolver: o.TransportResolver, turnLock: o.TurnLock,
		prepareRoom: o.PrepareRoom, publishTerminal: o.PublishTerminal, pendingAction: o.PendingAction, clock: o.Clock,
		LeaseTTLSeconds: o.LeaseTTLSeconds, PollIntervalSeconds: o.PollIntervalSeconds,
		ActivePollIntervalSeconds: o.ActivePollIntervalSeconds, TurnTimeoutSeconds: o.TurnTimeoutSeconds,
		IndeterminateDeferSeconds: o.IndeterminateDeferSeconds, MaxConcurrentRooms: o.MaxConcurrentRooms,
		UnavailableRetryMinSeconds: o.UnavailableRetryMinSeconds, UnavailableRetryMaxSeconds: o.UnavailableRetryMaxSeconds,
		ProcessGeneration: generation, roomsProvider: rooms,
		roomThreads: map[string]*hostedThread{}, roomsNeedingReschedule: map[string]bool{}, leases: map[string]DriverLease{},
		recoveredLeases: map[hostedLeaseKey]bool{}, inspectedIndeterminate: map[hostedAttemptKey]bool{},
		ambiguousRooms: map[string]float64{}, unavailableRouteRetries: map[hostedRouteKey]hostedRouteRetry{},
		blockedRooms: map[string]bool{}, currentTasks: map[string]TaskIdentity{},
	}, nil
}

func (r *HostedRoomRuntime) driverClock() DriverClock { return func() any { return r.clock() } }

// Starts the bounded room worker supervisor once
func (r *HostedRoomRuntime) Start() {
	r.statusLock.Lock()
	defer r.statusLock.Unlock()
	if r.thread != nil && r.thread.alive() {
		return
	}
	r.stop.Clear()
	r.wake.Set()
	r.thread = startHostedThread(func(*hostedThread) { r.workerLoop() })
}

// Requests a bounded clean stop without interrupting accepted turns, true when every worker ended
func (r *HostedRoomRuntime) Stop(timeout float64) bool {
	r.stop.Set()
	r.wake.Set()
	r.statusLock.Lock()
	thread := r.thread
	r.statusLock.Unlock()
	if thread == nil {
		return true
	}
	deadline := pythread.Monotonic() + max(0.0, timeout)
	thread.join(max(0.0, deadline-pythread.Monotonic()))
	r.statusLock.Lock()
	roomThreads := make([]*hostedThread, 0, len(r.roomThreads))
	for _, t := range r.roomThreads {
		roomThreads = append(roomThreads, t)
	}
	r.statusLock.Unlock()
	for _, t := range roomThreads {
		t.join(max(0.0, deadline-pythread.Monotonic()))
	}
	if thread.alive() {
		return false
	}
	for _, t := range roomThreads {
		if t.alive() {
			return false
		}
	}
	return true
}

// Wakes the worker after task admission or a room state change
func (r *HostedRoomRuntime) Wakeup() {
	r.statusLock.Lock()
	for roomID := range r.roomThreads {
		r.roomsNeedingReschedule[roomID] = true
	}
	r.statusLock.Unlock()
	r.wake.Set()
}

// Transport neutral snapshot of runtime health
func (r *HostedRoomRuntime) Status() map[string]any {
	r.statusLock.Lock()
	defer r.statusLock.Unlock()
	current := make([]any, 0, len(r.currentOrder))
	for _, roomID := range r.currentOrder {
		current = append(current, r.currentTasks[roomID])
	}
	var first any
	if len(current) > 0 {
		first = current[0]
	}
	return map[string]any{
		"running": r.thread != nil && r.thread.alive(), "stopping": r.stop.IsSet(),
		"process_generation": r.ProcessGeneration, "current_task": first, "current_tasks": current,
		"leased_rooms": hostedSortedKeys(r.leases), "blocked_rooms": hostedSortedKeys(r.blockedRooms),
		"last_error": r.lastError, "cycles": r.cycle,
	}
}

func hostedSortedKeys[V any](m map[string]V) []any {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]any, len(keys))
	for i, k := range keys {
		out[i] = k
	}
	return out
}

// Persists a stop intent, then commits cancellation after acknowledgement, rerouting lost races
func (r *HostedRoomRuntime) Cancel(identity TaskIdentity, cancelID string) (map[string]any, error) {
	for range hostedCancelRouteRetries {
		before, err := DriverGetTask(r.ctx, r.DBPath, identity)
		if err != nil {
			return nil, err
		}
		status, _ := before["status"].(string)
		if status == "cancelled" {
			return before, nil
		}
		if DriverTerminalStatuses[status] {
			return nil, invalidTransition(fmt.Sprintf("cannot cancel task in state '%s'", status))
		}
		direct := status == "queued" || status == "deferred"
		op := DriverBeginTaskCancel
		if direct {
			op = DriverCancelTask
		}
		result, err := op(r.ctx, r.DBPath, identity, cancelID, hrTaskInt(before, "cancel_generation"), r.driverClock())
		if err != nil {
			if errAs[*InvalidTaskTransitionError](err) || errAs[*StaleTaskError](err) {
				continue
			}
			return nil, err
		}
		if !direct {
			if err := r.cancelStopping(identity, result, cancelID); err != nil {
				r.recordError(fmt.Sprintf("stop remains pending: %s", err.Error()))
			}
		}
		r.Wakeup()
		if direct {
			return result, nil
		}
		return DriverGetTask(r.ctx, r.DBPath, identity)
	}
	final, err := DriverGetTask(r.ctx, r.DBPath, identity)
	if err != nil {
		return nil, err
	}
	if final["status"] == "cancelled" {
		return final, nil
	}
	return nil, invalidTransition(fmt.Sprintf("cancel kept losing races with task transitions (last observed state '%v')", final["status"]))
}

func (r *HostedRoomRuntime) cancelStopping(identity TaskIdentity, result map[string]any, cancelID string) error {
	binding, err := r.bindingForRoom(identity.RoomID)
	if err != nil || binding == nil {
		return err
	}
	lease, err := r.ensureLease(*binding)
	if err != nil {
		return err
	}
	acked, err := r.peerStopAcknowledged(*binding, result)
	if err != nil {
		return err
	}
	if !acked {
		settled, err := r.settleStoppingCompletion(*binding, result, lease)
		if err != nil {
			return err
		}
		if settled {
			return nil
		}
		if acked, err = r.interruptStoppingTask(*binding, result); err != nil || !acked {
			return err
		}
	}
	_, err = r.completeCancel(result, &cancelID)
	return err
}

// Retries one uncertain attempt under the current room lease, pinned to the selected generation
func (r *HostedRoomRuntime) RetryIndeterminate(identity TaskIdentity, executionGeneration *int64) (map[string]any, error) {
	task, err := DriverGetTask(r.ctx, r.DBPath, identity)
	if err != nil {
		return nil, err
	}
	if executionGeneration != nil && *executionGeneration != hrTaskInt(task, "execution_generation") {
		return nil, staleTask("task attempt changed before retry")
	}
	status, _ := task["status"].(string)
	if status != "indeterminate" && status != "deferred" {
		return nil, invalidTransition(fmt.Sprintf("cannot retry task in state '%s'", status))
	}
	binding, err := r.bindingForRoom(identity.RoomID)
	if err != nil {
		return nil, err
	}
	if binding == nil {
		return nil, roomUnavailable("hosted room is unavailable")
	}
	lease, err := r.ensureLease(*binding)
	if err != nil {
		return nil, err
	}
	if status == "deferred" {
		return r.requeue(r.opRequeueDeferred(), task, lease, identity.RoomID)
	}
	inspection, err := r.inspectRecoverySession(*binding, task)
	if err != nil {
		return nil, err
	}
	if inspection.terminal != nil {
		return r.resolveIndeterminate(binding, task, lease, *inspection.terminal, true)
	}
	if inspection.status == "cancelled" {
		return r.fenced(r.opResolveCancellation(fmt.Sprintf("remote-cancel:%d", hrTaskInt(task, "execution_generation"))), binding, task, lease, true, nil)
	}
	if inspection.active {
		r.setBlocked(identity.RoomID, true)
		return nil, invalidTransition("cannot retry while the original task attempt is still active")
	}
	return r.requeue(r.opRequeueIndeterminate(), task, lease, identity.RoomID)
}

// Lease fenced transition over one task identity and its expected generations
type hostedFencedOp func(identity TaskIdentity, lease DriverLease, executionGeneration, cancelGeneration int64) (map[string]any, error)

func (r *HostedRoomRuntime) opRequeueDeferred() hostedFencedOp {
	return func(identity TaskIdentity, lease DriverLease, e, c int64) (map[string]any, error) {
		return DriverRequeueDeferredTask(r.ctx, r.DBPath, identity, lease, e, c, r.driverClock())
	}
}

func (r *HostedRoomRuntime) opRequeueIndeterminate() hostedFencedOp {
	return func(identity TaskIdentity, lease DriverLease, e, c int64) (map[string]any, error) {
		return DriverRequeueIndeterminateTask(r.ctx, r.DBPath, identity, lease, e, c, r.driverClock())
	}
}

func (r *HostedRoomRuntime) opResolveCancellation(cancelID string) hostedFencedOp {
	return func(identity TaskIdentity, lease DriverLease, e, c int64) (map[string]any, error) {
		return DriverResolveIndeterminateCancellation(r.ctx, r.DBPath, identity, lease, e, c, cancelID, r.driverClock())
	}
}

func (r *HostedRoomRuntime) opResolve(receipt hostedTerminalReceipt) hostedFencedOp {
	return func(identity TaskIdentity, lease DriverLease, e, c int64) (map[string]any, error) {
		return DriverResolveIndeterminateTask(r.ctx, r.DBPath, identity, lease, e, c, receipt.settlementID, receipt.status, receipt.result, r.driverClock())
	}
}

func (r *HostedRoomRuntime) opSettleStopping(receipt hostedTerminalReceipt) hostedFencedOp {
	return func(identity TaskIdentity, lease DriverLease, e, c int64) (map[string]any, error) {
		return DriverSettleStoppingTask(r.ctx, r.DBPath, identity, lease, e, c, receipt.settlementID, receipt.status, receipt.result, r.driverClock())
	}
}

func (r *HostedRoomRuntime) opDefer(reason string) hostedFencedOp {
	return func(identity TaskIdentity, lease DriverLease, e, c int64) (map[string]any, error) {
		return DriverDeferIndeterminateTask(r.ctx, r.DBPath, identity, lease, e, c, reason, r.driverClock())
	}
}

func (r *HostedRoomRuntime) publish(binding HostedRoomBinding, task map[string]any) (map[string]any, error) {
	if r.publishTerminal != nil {
		if err := r.publishTerminal(binding, task); err != nil {
			return nil, err
		}
	}
	return task, nil
}

func (r *HostedRoomRuntime) setBlocked(roomID string, blocked bool) {
	r.statusLock.Lock()
	defer r.statusLock.Unlock()
	if blocked {
		r.blockedRooms[roomID] = true
	} else {
		delete(r.blockedRooms, roomID)
	}
}

// Runs one lease fenced transition on the task, an execution generation override replacing its own
func (r *HostedRoomRuntime) fenced(op hostedFencedOp, binding *HostedRoomBinding, task map[string]any, lease DriverLease, publish bool, executionOverride *int64) (map[string]any, error) {
	execution := hrTaskInt(task, "execution_generation")
	if executionOverride != nil {
		execution = *executionOverride
	}
	result, err := op(hrTaskIdentity(task), lease, execution, hrTaskInt(task, "cancel_generation"))
	if err != nil {
		return nil, err
	}
	if publish && binding != nil {
		return r.publish(*binding, result)
	}
	return result, nil
}

func (r *HostedRoomRuntime) requeue(op hostedFencedOp, task map[string]any, lease DriverLease, roomID string) (map[string]any, error) {
	retried, err := r.fenced(op, nil, task, lease, true, nil)
	if err != nil {
		return nil, err
	}
	r.setBlocked(roomID, false)
	r.Wakeup()
	return retried, nil
}

func (r *HostedRoomRuntime) completeCancel(task map[string]any, cancelID *string) (map[string]any, error) {
	var id any = task["cancel_id"]
	if cancelID != nil {
		id = *cancelID
	}
	return DriverCompleteTaskCancel(r.ctx, r.DBPath, hrTaskIdentity(task), id, hrTaskInt(task, "cancel_generation"), r.driverClock())
}

func (r *HostedRoomRuntime) resolveIndeterminate(binding *HostedRoomBinding, task map[string]any, lease DriverLease, terminal hostedTerminalReceipt, publish bool) (map[string]any, error) {
	return r.fenced(r.opResolve(terminal), binding, task, lease, publish, nil)
}

// Terminalizes a stopping task from its receipt or an acknowledged interrupt
func (r *HostedRoomRuntime) finishStop(binding HostedRoomBinding, task map[string]any, lease DriverLease) (bool, error) {
	settled, err := r.settleStoppingCompletion(binding, task, lease)
	if err != nil || settled {
		return settled, err
	}
	acked, err := r.interruptStoppingTask(binding, task)
	if err != nil || !acked {
		return false, err
	}
	if _, err := r.completeAcknowledgedStop(binding, task, lease); err != nil {
		return false, err
	}
	return true, nil
}

// Runtime id of the resumed canonical room session, empty when absent
func (r *HostedRoomRuntime) resumeExact(transport InternalSessionRPC, roomID, profile string) (string, error) {
	session, err := r.resolveOrCreate(transport, profile, roomID, false)
	if err != nil || session == nil {
		return "", err
	}
	return hostedSessionID(session)
}

// Transport, profile and resumed session id, no id when the transport is missing, local under peerOnly, or the session absent
func (r *HostedRoomRuntime) openSession(binding HostedRoomBinding, task map[string]any, peerOnly bool) (InternalSessionRPC, string, string, error) {
	transport, err := r.transportFor(binding, task)
	if err != nil {
		return nil, "", "", err
	}
	if transport == nil || (peerOnly && transport == r.RPC) {
		return transport, "", "", nil
	}
	profile := hrTaskProfile(task)
	sessionID, err := r.resumeExact(transport, binding.RoomID, profile)
	return transport, profile, sessionID, err
}

func (r *HostedRoomRuntime) terminalFromHistory(transport InternalSessionRPC, profile, sessionID string, task map[string]any) (*hostedTerminalReceipt, error) {
	history, err := transport.History(r.ctx, profile, sessionID, RoomSessionSource)
	if err != nil {
		return nil, err
	}
	return hostedFindTerminalReceipt(history, hrTaskIdentity(task), hrTaskInt(task, "execution_generation")), nil
}

// Probes a peer's exact durable terminal Stop receipt before reading history
func (r *HostedRoomRuntime) peerStopAcknowledged(binding HostedRoomBinding, task map[string]any) (bool, error) {
	transport, profile, sessionID, err := r.openSession(binding, task, true)
	if err != nil || sessionID == "" {
		return false, err
	}
	info, err := transport.Info(r.ctx, profile, sessionID, RoomSessionSource)
	if err != nil {
		return false, err
	}
	generation, err := hostedIntOrZero(info["execution_generation"])
	if err != nil {
		return false, err
	}
	return !hostedInfoActive(info) && hostedStopAckStatuses[hostedStrOrEmpty(info["status"])] &&
		hostedStrOrEmpty(info["task_id"]) == hrTaskIdentity(task).TaskID &&
		generation == hrTaskInt(task, "execution_generation"), nil
}

// Interrupts the exact stopping turn, true when the stop is acknowledged
func (r *HostedRoomRuntime) interruptStoppingTask(binding HostedRoomBinding, task map[string]any) (bool, error) {
	transport, profile, sessionID, err := r.openSession(binding, task, false)
	if err != nil {
		return false, err
	}
	if sessionID == "" {
		return transport != nil && transport == r.RPC, nil
	}
	info, err := transport.Info(r.ctx, profile, sessionID, RoomSessionSource)
	if err != nil {
		return false, err
	}
	if !hostedInfoActive(info) {
		return true, nil
	}
	if !hostedInfoIsActiveFor(info, hrTaskIdentity(task), true) {
		return false, nil
	}
	result, err := transport.Interrupt(r.ctx, profile, sessionID, RoomSessionSource, hrTaskIdentity(task).TaskID)
	if err != nil {
		return false, err
	}
	return result != nil && (result["interrupted"] == true || hostedStopAckStatuses[hostedStrOrEmpty(result["status"])]), nil
}

// Publishes a terminal receipt that arrived before Stop was acknowledged
func (r *HostedRoomRuntime) settleStoppingCompletion(binding HostedRoomBinding, task map[string]any, lease DriverLease) (bool, error) {
	transport, profile, sessionID, err := r.openSession(binding, task, false)
	if err != nil || sessionID == "" {
		return false, err
	}
	receipt, err := r.terminalFromHistory(transport, profile, sessionID, task)
	if err != nil || receipt == nil {
		return false, err
	}
	if _, err := r.fenced(r.opSettleStopping(*receipt), &binding, task, lease, true, nil); err != nil {
		return false, err
	}
	return true, nil
}

func (r *HostedRoomRuntime) reportPendingAction(task map[string]any, sessionID string, info map[string]any) error {
	if r.pendingAction == nil {
		return nil
	}
	approvalValue := info["pending_approval"]
	if !pyrepr.Truthy(approvalValue) {
		approvalValue = info["approval"]
	}
	var action map[string]any
	if approval, ok := hostedMapping(approvalValue); ok {
		choices := []any{}
		if raw, _ := approval["choices"].([]any); raw != nil {
			for _, c := range raw {
				if c == "once" || c == "deny" {
					choices = append(choices, c)
				}
			}
		}
		if len(choices) == 0 {
			choices = []any{"once", "deny"}
		}
		safe := make(map[string]any, len(approval)+1)
		for k, v := range approval {
			safe[k] = v
		}
		safe["choices"] = choices
		action = map[string]any{
			"kind": "approval", "task_id": hrTaskIdentity(task).TaskID,
			"execution_generation": hrTaskInt(task, "execution_generation"),
			"run_id":               info["run_id"], "session_id": sessionID, "request_id": safe["request_id"], "approval": safe,
		}
	}
	return r.pendingAction(hrTaskIdentity(task).RoomID, hostedMemberID(task), action)
}

func (r *HostedRoomRuntime) retryStoppingTasks(binding HostedRoomBinding, lease DriverLease) (bool, error) {
	tasks, err := r.tasks(binding, "stopping")
	if err != nil {
		return false, err
	}
	for _, task := range tasks {
		pending, err := r.retryStoppingTask(binding, task, &lease)
		if err != nil {
			r.recordError(fmt.Sprintf(hostedStopPending, err.Error()))
			return true, nil
		}
		if pending {
			return true, nil
		}
	}
	return false, nil
}

func (r *HostedRoomRuntime) retryStoppingTask(binding HostedRoomBinding, task map[string]any, lease *DriverLease) (bool, error) {
	renewed, err := r.renewLeaseIfNeeded(*lease, false)
	if err != nil {
		return false, err
	}
	*lease = renewed
	acked, err := r.peerStopAcknowledged(binding, task)
	if err != nil {
		return false, err
	}
	if acked {
		_, err := r.completeCancel(task, nil)
		return false, err
	}
	finished, err := r.finishStop(binding, task, renewed)
	return !finished, err
}

func (r *HostedRoomRuntime) recordTaskError(attempt TaskAttempt, message string) {
	r.recordError(fmt.Sprintf("task %s %s", attempt.Identity.TaskID, message))
}

func (r *HostedRoomRuntime) recordError(message string) {
	r.statusLock.Lock()
	defer r.statusLock.Unlock()
	r.lastError = message
}

// Canonical hidden session title of one hosted room
func RoomSessionTitle(roomID string) string { return "Group: " + roomID }

func hostedMemberID(task map[string]any) string {
	payload, _ := hostedMapping(task["payload"])
	if v := payload["target_member_id"]; pyrepr.Truthy(v) {
		return pyrepr.Str(v)
	}
	if v := payload["target_profile"]; pyrepr.Truthy(v) {
		return pyrepr.Str(v)
	}
	return ""
}

func hostedSessionID(session map[string]any) (string, error) {
	value, ok := session["session_id"]
	if !ok {
		value = session["id"]
	}
	text, isStr := value.(string)
	if !isStr || text == "" {
		return "", &RoomValueError{Msg: "session adapter returned no session_id"}
	}
	return text, nil
}

func hostedTruncateUTF8(value any, maxBytes int) (string, bool) {
	text := ""
	if pyrepr.Truthy(value) {
		text = pyrepr.Str(value)
	}
	encoded := []byte(text)
	if len(encoded) <= maxBytes {
		return text, false
	}
	prefix := encoded[:max(0, maxBytes-len(hostedTerminalTruncationNotice))]
	for len(prefix) > 0 {
		if utf8.Valid(prefix) {
			return string(prefix) + hostedTerminalTruncationNotice, true
		}
		prefix = prefix[:len(prefix)-1]
	}
	return "[Reply truncated. Ask the Bot to share the full result as a file.]", true
}

func hostedBoundedTerminalResult(receipt map[string]any) map[string]any {
	textValue, ok := receipt["text"]
	if !ok {
		textValue = ""
	}
	errValue, ok := receipt["error"]
	if !ok {
		errValue = ""
	}
	text, truncated := hostedTruncateUTF8(textValue, MaxTerminalTextBytes)
	errText, errTruncated := hostedTruncateUTF8(errValue, 4096)
	out := map[string]any{"message_id": receipt["message_id"], "text": text}
	if errText != "" {
		out["error"] = errText
	}
	if truncated || errTruncated {
		out["truncated"] = true
	}
	return out
}

func hostedFindTerminalReceipt(history []map[string]any, identity TaskIdentity, executionGeneration int64) *hostedTerminalReceipt {
	for i := len(history) - 1; i >= 0; i-- {
		message := history[i]
		status, _ := message["status"].(string)
		if !pyrepr.Equal(message["task_id"], identity.TaskID) || !pyrepr.Equal(message["execution_generation"], executionGeneration) ||
			!pyrepr.Equal(message["role"], "assistant") || (status != "settled" && status != "failed") {
			continue
		}
		receiptID, ok := message["message_id"].(string)
		if !ok || receiptID == "" {
			receiptID = fmt.Sprintf("reply:%s:%d", identity.TaskID, executionGeneration)
		}
		content, has := message["content"]
		if !has {
			content = ""
		}
		return &hostedTerminalReceipt{status: status, settlementID: receiptID,
			result: hostedBoundedTerminalResult(map[string]any{"message_id": receiptID, "text": content})}
	}
	return nil
}

func hostedInfoActive(info map[string]any) bool {
	if v, ok := info["active"]; ok {
		return pyrepr.Truthy(v)
	}
	if v, ok := info["running"]; ok {
		return pyrepr.Truthy(v)
	}
	return false
}

func hostedInfoIsActiveFor(info map[string]any, identity TaskIdentity, requireExact bool) bool {
	if !hostedInfoActive(info) {
		return false
	}
	taskID := info["task_id"]
	if taskID == nil {
		return !requireExact
	}
	return pyrepr.Equal(taskID, identity.TaskID)
}

func hrTaskIdentity(task map[string]any) TaskIdentity {
	identity, _ := task["identity"].(TaskIdentity)
	return identity
}

func hrTaskInt(task map[string]any, key string) int64 {
	n, _ := pyint.Int64(task[key])
	return n
}

func hrTaskProfile(task map[string]any) string {
	payload, _ := hostedMapping(task["payload"])
	profile, _ := payload["target_profile"].(string)
	return profile
}

// Python int of a value or zero for a falsy one
func hostedIntOrZero(value any) (int64, error) {
	if !pyrepr.Truthy(value) {
		return 0, nil
	}
	return pyint.Int64(value)
}

// Python str of a value or empty for a falsy one
func hostedStrOrEmpty(value any) string {
	if !pyrepr.Truthy(value) {
		return ""
	}
	return pyrepr.Str(value)
}

func errAs[T error](err error) bool {
	var target T
	return errors.As(err, &target)
}

// Wall clock seconds, the time.time of the runtime
func hostedWallClock() float64 { return float64(time.Now().UnixNano()) / 1e9 }
