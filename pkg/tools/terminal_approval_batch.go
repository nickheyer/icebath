package tools

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

// Prepares desktop terminal consent for adjacent terminal calls without running shells ahead of their turn

var errCancelledPreparation = errors.New("Terminal approval preparation cancelled; command was not started")

var errPreparationTimedOut = errors.New("Terminal approval preparation timed out; commands were not started")

type terminalBatchKey struct{}

type terminalSlotKey struct{}

func withTerminalBatch(ctx context.Context, b *terminalBatch) context.Context {
	return context.WithValue(ctx, terminalBatchKey{}, b)
}

func terminalBatchOf(ctx context.Context) *terminalBatch {
	if ctx == nil {
		return nil
	}
	b, _ := ctx.Value(terminalBatchKey{}).(*terminalBatch)
	return b
}

func withTerminalSlot(ctx context.Context, s *terminalSlot) context.Context {
	return WithPreparedApprovalSlot(context.WithValue(ctx, terminalSlotKey{}, s), s)
}

func terminalSlotOf(ctx context.Context) *terminalSlot {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(terminalSlotKey{}).(*terminalSlot)
	return s
}

type preparedGuardKey struct {
	command       string
	envType       string
	hasHostAccess bool
}

// terminalSlot is one terminal call whose approval runs ahead of its turn
type terminalSlot struct {
	batch     *terminalBatch
	parsed    *ParsedCall
	index     int
	ready     chan struct{}
	readyOnce sync.Once
	release   chan struct{}
	relOnce   sync.Once
	done      chan struct{}
	mu        sync.Mutex
	workers   []WorkerID
	preparing bool
	args      map[string]any
	decision  *GuardResult
	guardKey  *preparedGuardKey
	claimed   bool
	res       *sequentialResult
}

func (s *terminalSlot) markReady()   { s.readyOnce.Do(func() { close(s.ready) }) }
func (s *terminalSlot) markRelease() { s.relOnce.Do(func() { close(s.release) }) }

func (s *terminalSlot) workerIDs() []WorkerID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]WorkerID(nil), s.workers...)
}

func (s *terminalSlot) result() *sequentialResult { return s.res }

func (s *terminalSlot) callID() string { return s.parsed.Ref(s.batch.taskID).CallID }

// Cancelled batches and interrupted turns abort preparation before any command starts
func (s *terminalSlot) checkCancelled() error {
	if s.batch.isCancelled() || s.batch.exec.Host.Interrupted() {
		return errCancelledPreparation
	}
	return nil
}

// Preparing reports whether this slot is collecting its approval ahead of execution
func (s *terminalSlot) Preparing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.preparing
}

// RegisterPreparedApproval records a request published while preparing so close can withdraw it
func (s *terminalSlot) RegisterPreparedApproval(sessionKey string, entry *ApprovalEntry) {
	if s.checkCancelled() != nil {
		return
	}
	s.batch.mu.Lock()
	s.batch.pendingApprovals = append(s.batch.pendingApprovals, pendingPrepared{sessionKey, entry})
	s.batch.mu.Unlock()
}

// ApprovalPublished lets the batch proceed to the next slot once a human request is visible
func (s *terminalSlot) ApprovalPublished() { s.markReady() }

// ConsumePreparedGuard hands the single use prepared decision to the live guard when it still applies
func (s *terminalSlot) ConsumePreparedGuard(ctx context.Context, env *Env, command, envType string, hasHostAccess bool) (GuardResult, bool) {
	if s.Preparing() {
		return GuardResult{}, false
	}
	if err := s.checkCancelled(); err != nil {
		return GuardResult{Approved: false, Message: err.Error(), Outcome: "cancelled", DenyReason: err.Error()}, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.batch.failureSeen() && s.decision != nil {
		s.decision = nil
		return GuardResult{}, false
	}
	currentCallID, _ := loadSessionString(env, "approval.tool_call_id")
	if currentCallID != s.callID() || s.guardKey == nil || *s.guardKey != (preparedGuardKey{command, envType, hasHostAccess}) {
		return GuardResult{}, false
	}
	decision := s.decision
	s.decision = nil
	if decision == nil {
		return GuardResult{}, false
	}
	if decision.Approved && !decision.UserApproved {
		if !(decision.SmartApproved && getApprovalMode(env) == "smart") {
			return GuardResult{}, false
		}
	}
	return *decision, true
}

// Reads approval policy only, every real guard still runs when the command executes
func (s *terminalSlot) prepare(ctx context.Context, ref *ToolCallRef) error {
	if err := s.checkCancelled(); err != nil {
		return err
	}
	env := s.batch.exec.Host.Env()
	s.mu.Lock()
	s.args, _ = schemaDeepCopy(ref.Args).(map[string]any)
	s.preparing = true
	s.mu.Unlock()
	func() {
		defer func() {
			s.mu.Lock()
			s.preparing = false
			s.mu.Unlock()
			s.markReady()
		}()
		policy, err := terminalPolicy(env)
		if err != nil {
			return
		}
		command, ok := ref.Args["command"].(string)
		if !ok {
			return
		}
		host := s.batch.exec.Host
		SetCurrentObservabilityContext(env, host.TurnID(), ref.CallID, host.SessionID())
		defer ResetCurrentObservabilityContext(env)
		if !truthy(ref.Args["background"]) {
			if rewritten := trailingAmpBackgroundCommand(command); rewritten != "" {
				command = rewritten
			}
		}
		key := preparedGuardKey{command, policy.Get("TERMINAL_ENV", "local"), dockerHasHostAccess(env, policy)}
		decision := CheckAllCommandGuards(ctx, env, key.command, key.envType, key.hasHostAccess)
		s.mu.Lock()
		s.guardKey, s.decision = &key, &decision
		s.mu.Unlock()
	}()
	for {
		select {
		case <-s.release:
			return s.checkCancelled()
		case <-time.After(100 * time.Millisecond):
			if err := s.checkCancelled(); err != nil {
				return err
			}
		}
	}
}

// Runs the slot's call through the middleware on its own worker
func (s *terminalSlot) run(ctx context.Context) {
	defer close(s.done)
	defer s.markReady()
	e := s.batch.exec
	ctx = withTerminalSlot(ctx, s)
	wctx, id, release := e.registerWorker(ctx)
	defer release()
	s.mu.Lock()
	s.workers = append(s.workers, id)
	s.mu.Unlock()
	if err := s.checkCancelled(); err != nil {
		s.res.err = err
		return
	}
	ref := s.parsed.Ref(s.batch.taskID)
	dispatch := e.resolveSequentialDispatch(wctx, ref, s.batch.messages)
	s.res.managed, s.res.err = e.runToolExecutionMiddleware(wctx, managedInput{name: ref.Name, args: ref.Args, taskID: ref.TaskID, toolCallID: ref.CallID,
		execute: dispatch.execute, scopeBlock: s.parsed.ScopeBlock, displayIndex: s.index + 1, trace: ref.Trace, gate: s.batch.authGate, slot: s})
}

type pendingPrepared struct {
	sessionKey string
	entry      *ApprovalEntry
}

// terminalBatch is one run of adjacent terminal calls prepared together
type terminalBatch struct {
	exec             *Executor
	messages         *[]*state.Message
	taskID           string
	mu               sync.Mutex
	cancelled        bool
	pendingApprovals []pendingPrepared
	authGate         *AuthorizationGate
	slots            []*terminalSlot
	failure          bool
}

func (e *Executor) newTerminalBatch(messages *[]*state.Message, taskID string, parsed []*ParsedCall) *terminalBatch {
	b := &terminalBatch{exec: e, messages: messages, taskID: taskID, authGate: NewAuthorizationGate(e.Host.Env(), 0)}
	for i, pc := range parsed {
		b.slots = append(b.slots, &terminalSlot{batch: b, parsed: pc, index: i, ready: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{}), res: &sequentialResult{}})
	}
	return b
}

func (b *terminalBatch) isCancelled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cancelled
}

func (b *terminalBatch) failureSeen() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failure
}

// Starts every slot in order, proceeding once it publishes a human request or finishes preparation
func (b *terminalBatch) start(ctx context.Context) error {
	env := b.exec.Host.Env()
	for _, slot := range b.slots {
		if err := slot.checkCancelled(); err != nil {
			return err
		}
		go slot.run(ctx)
		timeout := resolveSequentialToolTimeout(env)
		started := time.Now()
		baseline := b.authGate.ExcludedSeconds()
		for {
			select {
			case <-slot.ready:
			case <-time.After(100 * time.Millisecond):
				if err := slot.checkCancelled(); err != nil {
					return err
				}
				elapsed := time.Since(started).Seconds() - (b.authGate.ExcludedSeconds() - baseline)
				if timeout > 0 && elapsed >= timeout.Seconds() {
					return errPreparationTimedOut
				}
				continue
			}
			break
		}
	}
	return nil
}

// Withdraws only this batch's requests and releases every slot
func (b *terminalBatch) close() {
	b.mu.Lock()
	b.cancelled = true
	pending := b.pendingApprovals
	b.pendingApprovals = nil
	b.mu.Unlock()
	for _, p := range pending {
		WithdrawGatewayApproval(p.sessionKey, p.entry.RequestID(), "deny")
	}
	for _, slot := range b.slots {
		slot.markRelease()
		select {
		case <-slot.done:
		default:
			interruptWorkerIDs(slot.workerIDs(), "")
		}
	}
}

// Claims the prepared slot for a call id and releases it into execution
func takePreparedCall(ctx context.Context, callID string) *terminalSlot {
	b := terminalBatchOf(ctx)
	if b == nil {
		return nil
	}
	for _, slot := range b.slots {
		slot.mu.Lock()
		matches := slot.callID() == callID && !slot.claimed
		if matches {
			slot.claimed = true
		}
		slot.mu.Unlock()
		if matches {
			slot.markRelease()
			return slot
		}
	}
	return nil
}

// Records a published failure so later prepared slots re gate live
func markTerminalBatchOutcome(ctx context.Context, failed bool) {
	b := terminalBatchOf(ctx)
	if b != nil && failed {
		b.mu.Lock()
		b.failure = true
		b.mu.Unlock()
	}
}

// PreparingTerminalApproval reports whether the context's slot is collecting its approval
func PreparingTerminalApproval(ctx context.Context) bool {
	s := terminalSlotOf(ctx)
	return s != nil && s.Preparing()
}

// ValidatePreparedTerminal refuses arguments that changed after approval preparation
func ValidatePreparedTerminal(ctx context.Context, args map[string]any) error {
	s := terminalSlotOf(ctx)
	if s == nil {
		return nil
	}
	if err := s.checkCancelled(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !reflect.DeepEqual(args, s.args) {
		s.decision = nil
		return fmt.Errorf("Terminal arguments changed after approval preparation; command was not started")
	}
	return nil
}

// Keeps nonterminal barriers but batches adjacent terminals in mixed segments
func (e *Executor) terminalApprovalRuns(ctx context.Context, calls []state.ToolCall) [][]state.ToolCall {
	var runs [][]state.ToolCall
	var current []state.ToolCall
	currentTerminal := false
	for _, call := range calls {
		pc := e.ParseToolCall(ctx, call, true)
		isTerminal := pc.Name == "terminal" && pc.ParseError == ""
		if len(current) > 0 && isTerminal != currentTerminal {
			runs = append(runs, current)
			current = nil
		}
		currentTerminal = isTerminal
		current = append(current, call)
	}
	if len(current) > 0 {
		runs = append(runs, current)
	}
	return runs
}

// Prepares a run of desktop terminal calls together, returning the batch context and its closer
func (e *Executor) terminalApprovalBatch(ctx context.Context, calls []state.ToolCall, messages *[]*state.Message, taskID string) (context.Context, func()) {
	host := e.Host
	env := host.Env()
	if len(calls) < 2 || host.SessionSource() != "desktop" || gatewayNotifyCb(getCurrentSessionKey(env, "")) == nil {
		return ctx, func() {}
	}
	parsed := make([]*ParsedCall, len(calls))
	ids := map[string]bool{}
	for i, call := range calls {
		parsed[i] = e.ParseToolCall(ctx, call, true)
		if parsed[i].Name != "terminal" || parsed[i].ParseError != "" {
			return ctx, func() {}
		}
		id := parsed[i].Ref(taskID).CallID
		if id == "" || ids[id] {
			return ctx, func() {}
		}
		ids[id] = true
	}
	batch := e.newTerminalBatch(messages, taskID, parsed)
	bctx := withTerminalBatch(ctx, batch)
	if !host.Interrupted() && !host.IncrementalPersistenceFailed() {
		if err := batch.start(bctx); err != nil {
			batch.close()
			if errors.Is(err, errPreparationTimedOut) {
				host.Interrupt("", "terminal batch preparation timeout")
			}
		}
	}
	return bctx, batch.close
}
