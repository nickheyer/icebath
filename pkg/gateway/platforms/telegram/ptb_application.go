package telegram

import (
	"context"
	"errors"
	"fmt"
	"github.com/go-telegram/bot/models"
	"log/slog"
	"slices"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Returned by a handler or a blocking error handler to stop the remaining handler groups
var ErrApplicationHandlerStop = errors.New("ApplicationHandlerStop")

// Chat an update belongs to
type Chat struct {
	ID int64
}

// Bot API update with its id and the chat it belongs to
type Update struct {
	UpdateID int64
	Chat     *Chat
	Raw      *models.Update
}

// Chat of the update, the PTB effective_chat
func (u *Update) EffectiveChat() *Chat { return u.Chat }

// Update value that names its chat, the objects PTB reads effective_chat from
type EffectiveChatter interface {
	EffectiveChat() *Chat
}

// Callback context of a handler or error handler run
type CallbackContext struct {
	Application *Application
	Error       error
}

// Handler callback with the object its method is bound to, the PTB bound method self
type Callback struct {
	Fn       func(ctx context.Context, update any, cbctx *CallbackContext) error
	Owner    any
	admitted bool
}

// Block setting of a handler, the default deferring to the bot defaults
type BlockMode int

const (
	BlockDefault BlockMode = iota
	BlockTrue
	BlockFalse
)

// Update handler of a PTB handler group
type Handler interface {
	CheckUpdate(update any) any
	HandleUpdate(ctx context.Context, update any, app *Application, check any, cbctx *CallbackContext) error
	Block() BlockMode
}

// Handler whose callback admission wraps
type CallbackHandler interface {
	Handler
	HandlerCallback() *Callback
	SetHandlerCallback(cb *Callback)
}

// Handler with no callback of its own whose child handlers carry the callbacks, the ConversationHandler shape
type CompositeHandler interface {
	Handler
	ChildHandlers() []Handler
}

// Handler matching every update of the Go type T, the PTB TypeHandler
type TypeHandler[T any] struct {
	Callback  *Callback
	BlockMode BlockMode
}

// Type handler of callback with the default block
func NewTypeHandler[T any](cb *Callback) *TypeHandler[T] { return &TypeHandler[T]{Callback: cb} }

func (h *TypeHandler[T]) CheckUpdate(update any) any {
	_, ok := update.(T)
	return ok
}

func (h *TypeHandler[T]) HandleUpdate(ctx context.Context, update any, _ *Application, _ any, cbctx *CallbackContext) error {
	return h.Callback.Fn(ctx, update, cbctx)
}

func (h *TypeHandler[T]) Block() BlockMode                { return h.BlockMode }
func (h *TypeHandler[T]) HandlerCallback() *Callback      { return h.Callback }
func (h *TypeHandler[T]) SetHandlerCallback(cb *Callback) { h.Callback = cb }

// Runs update dispatch coroutines under a concurrency bound, the PTB BaseUpdateProcessor
type UpdateProcessor interface {
	MaxConcurrentUpdates() int
	ProcessUpdate(ctx context.Context, update any, coroutine func(ctx context.Context) error) error
}

type boundedProcessor struct {
	max     int
	mu      sync.Mutex
	active  int
	waiters []*updateTurn
}

// One update's place in a processor queue, granted in hand over order like asyncio task start order
type updateTurn struct {
	p       *boundedProcessor
	granted chan struct{}
	holds   bool
	enter   func()
	wait    func(ctx context.Context) error
	finish  func()
}

func newBoundedProcessor(maxConcurrent int) (*boundedProcessor, error) {
	if maxConcurrent < 1 {
		return nil, errors.New("ValueError: `max_concurrent_updates` must be a positive integer!")
	}
	return &boundedProcessor{max: maxConcurrent}, nil
}

// Queues a turn behind every earlier one, granting it at once when a slot is free and nobody waits
func (p *boundedProcessor) reserveTurn(enter func(), wait func(ctx context.Context) error, finish func()) *updateTurn {
	t := &updateTurn{p: p, granted: make(chan struct{}), enter: enter, wait: wait, finish: finish}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active < p.max && len(p.waiters) == 0 {
		p.grantLocked(t)
	} else {
		p.waiters = append(p.waiters, t)
	}
	return t
}

func (p *boundedProcessor) grantLocked(t *updateTurn) {
	p.active++
	t.holds = true
	if t.enter != nil {
		t.enter()
	}
	close(t.granted)
}

// Frees a slot and grants the waiting turns in arrival order while slots remain
func (p *boundedProcessor) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active--
	for p.active < p.max && len(p.waiters) > 0 {
		next := p.waiters[0]
		p.waiters = p.waiters[1:]
		p.grantLocked(next)
	}
}

// Waits for the slot, then the turn's own wait, then runs the coroutine and settles the turn
func (t *updateTurn) run(ctx context.Context, coroutine func(ctx context.Context) error) error {
	if err := t.acquire(ctx); err != nil {
		return err
	}
	defer t.p.release()
	if t.finish != nil {
		defer t.finish()
	}
	if t.wait != nil {
		if err := t.wait(ctx); err != nil {
			return err
		}
	}
	return coroutine(ctx)
}

// Slot of the turn, a cancelled waiter leaving the queue or giving back a slot granted as it was cancelled
func (t *updateTurn) acquire(ctx context.Context) error {
	select {
	case <-t.granted:
		return nil
	default:
	}
	select {
	case <-t.granted:
		return nil
	case <-ctx.Done():
	}
	p := t.p
	p.mu.Lock()
	if !t.holds {
		p.waiters = slices.DeleteFunc(p.waiters, func(w *updateTurn) bool { return w == t })
		p.mu.Unlock()
		return ctx.Err()
	}
	p.mu.Unlock()
	if t.finish != nil {
		t.finish()
	}
	p.release()
	return ctx.Err()
}

// Processor awaiting each coroutine inline under the bound, the PTB SimpleUpdateProcessor
type SimpleUpdateProcessor struct {
	*boundedProcessor
}

// Simple processor of the bound, a ValueError below one
func NewSimpleUpdateProcessor(maxConcurrent int) (*SimpleUpdateProcessor, error) {
	b, err := newBoundedProcessor(maxConcurrent)
	if err != nil {
		return nil, err
	}
	return &SimpleUpdateProcessor{b}, nil
}

func (p *SimpleUpdateProcessor) MaxConcurrentUpdates() int { return p.max }

func (p *SimpleUpdateProcessor) ProcessUpdate(ctx context.Context, update any, coroutine func(ctx context.Context) error) error {
	return p.reserve(update).run(ctx, coroutine)
}

func (p *SimpleUpdateProcessor) reserve(any) *updateTurn { return p.reserveTurn(nil, nil, nil) }

// Processor whose queue place is fixed when the fetcher hands it the update, the asyncio task start order
type sequencedProcessor interface {
	reserve(update any) *updateTurn
}

// Overridable dispatch steps of the application, the methods a PTB subclass replaces
type applicationImpl interface {
	addHandler(h Handler, group int)
	addErrorHandler(cb *Callback, block BlockMode)
	processUpdate(ctx context.Context, update any) error
	processError(ctx context.Context, update any, err error) (bool, error)
	createTask(ctx context.Context, fn func(ctx context.Context) error, update any, isErrorHandler bool) *gateway.AdapterTask
}

type errorHandlerEntry struct {
	key   *Callback
	call  *Callback
	block BlockMode
}

// Handler groups, error handlers and task seam of a PTB Application
type Application struct {
	BotID           int64
	DefaultsBlock   *bool
	UpdateProcessor UpdateProcessor
	mu              sync.Mutex
	handlers        map[int][]Handler
	errorHandlers   []errorHandlerEntry
	impl            applicationImpl
	running         bool
	createdTasks    map[*gateway.AdapterTask]struct{}
}

// Application of the bot dispatching through the processor, the PTB default one update at a time
func NewApplication(botID int64, processor UpdateProcessor) *Application {
	a := &Application{BotID: botID, UpdateProcessor: processor, handlers: map[int][]Handler{}}
	if a.UpdateProcessor == nil {
		a.UpdateProcessor, _ = NewSimpleUpdateProcessor(1)
	}
	a.impl = a
	return a
}

// Registers the handler in its group, groups running in ascending order
func (a *Application) AddHandler(h Handler, group int) { a.impl.addHandler(h, group) }

// Registers an error handler once, a repeat registration warning and ignored
func (a *Application) AddErrorHandler(cb *Callback, block BlockMode) {
	a.impl.addErrorHandler(cb, block)
}

// Unregisters the error handler by its registration key
func (a *Application) RemoveErrorHandler(cb *Callback) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.errorHandlers = slices.DeleteFunc(a.errorHandlers, func(e errorHandlerEntry) bool { return e.key == cb })
}

// Dispatches the update to the first matching handler of every group
func (a *Application) ProcessUpdate(ctx context.Context, update any) error {
	return a.impl.processUpdate(ctx, update)
}

// Runs the error handlers for err, true when one stopped the remaining groups
func (a *Application) ProcessError(ctx context.Context, update any, err error) (bool, error) {
	return a.impl.processError(ctx, update, err)
}

// Starts fn as a task whose failure reaches the error handlers
func (a *Application) CreateTask(ctx context.Context, fn func(ctx context.Context) error, update any) *gateway.AdapterTask {
	return a.impl.createTask(ctx, fn, update, false)
}

// One update fetcher step, concurrent dispatch above a bound of one and inline otherwise
func (a *Application) FetchUpdate(ctx context.Context, update any) error {
	coroutine := func(c context.Context) error { return a.impl.processUpdate(c, update) }
	if a.UpdateProcessor.MaxConcurrentUpdates() <= 1 {
		return a.UpdateProcessor.ProcessUpdate(ctx, update, coroutine)
	}
	wrapper := func(c context.Context) error { return a.UpdateProcessor.ProcessUpdate(c, update, coroutine) }
	if sp, ok := a.UpdateProcessor.(sequencedProcessor); ok {
		turn := sp.reserve(update)
		wrapper = func(c context.Context) error { return turn.run(c, coroutine) }
	}
	a.impl.createTask(ctx, wrapper, update, false)
	return nil
}

func (a *Application) addHandler(h Handler, group int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.handlers[group] = append(a.handlers[group], h)
}

func (a *Application) addErrorHandler(cb *Callback, block BlockMode) {
	a.addErrorHandlerEntry(cb, cb, block)
}

func (a *Application) addErrorHandlerEntry(key, call *Callback, block BlockMode) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.errorHandlers {
		if e.key == key {
			slog.Warn("The callback is already registered as an error handler. Ignoring.")
			return
		}
	}
	a.errorHandlers = append(a.errorHandlers, errorHandlerEntry{key: key, call: call, block: block})
}

func (a *Application) nonBlocking(block BlockMode) bool {
	return block == BlockFalse || (block == BlockDefault && a.DefaultsBlock != nil && !*a.DefaultsBlock)
}

func (a *Application) groupSnapshot() [][]Handler {
	a.mu.Lock()
	defer a.mu.Unlock()
	groups := make([]int, 0, len(a.handlers))
	for g := range a.handlers {
		groups = append(groups, g)
	}
	slices.Sort(groups)
	out := make([][]Handler, 0, len(groups))
	for _, g := range groups {
		out = append(out, append([]Handler(nil), a.handlers[g]...))
	}
	return out
}

func checkMatched(check any) bool {
	if check == nil {
		return false
	}
	b, isBool := check.(bool)
	return !isBool || b
}

func (a *Application) processUpdate(ctx context.Context, update any) error {
	var cbctx *CallbackContext
	for _, handlers := range a.groupSnapshot() {
		err := func() error {
			for _, h := range handlers {
				check := h.CheckUpdate(update)
				if !checkMatched(check) {
					continue
				}
				if cbctx == nil {
					cbctx = &CallbackContext{Application: a}
				}
				run := func(c context.Context) error { return h.HandleUpdate(c, update, a, check, cbctx) }
				if a.nonBlocking(h.Block()) {
					a.impl.createTask(ctx, run, update, false)
					return nil
				}
				return run(ctx)
			}
			return nil
		}()
		if err == nil {
			continue
		}
		if errors.Is(err, ErrApplicationHandlerStop) {
			slog.Debug("Stopping further handlers due to ApplicationHandlerStop")
			break
		}
		if errors.Is(err, context.Canceled) {
			return err
		}
		stop, perr := a.impl.processError(ctx, update, err)
		if perr != nil {
			return perr
		}
		if stop {
			slog.Debug("Error handler stopped further handlers.")
			break
		}
	}
	return nil
}

func (a *Application) processError(ctx context.Context, update any, failure error) (bool, error) {
	a.mu.Lock()
	entries := append([]errorHandlerEntry(nil), a.errorHandlers...)
	a.mu.Unlock()
	if len(entries) == 0 {
		slog.Error("No error handlers are registered, logging exception.", "error", failure)
		return false, nil
	}
	for _, e := range entries {
		cbctx := &CallbackContext{Application: a, Error: failure}
		call := e.call
		run := func(c context.Context) error { return call.Fn(c, update, cbctx) }
		if a.nonBlocking(e.block) {
			a.impl.createTask(ctx, run, update, true)
			continue
		}
		err := run(ctx)
		switch {
		case err == nil:
		case errors.Is(err, ErrApplicationHandlerStop):
			return true, nil
		case errors.Is(err, context.Canceled):
			return false, err
		default:
			slog.Error("An error was raised and an uncaught error was raised while handling the error with an error_handler.", "error", err)
		}
	}
	return false, nil
}

func (a *Application) createTask(ctx context.Context, fn func(ctx context.Context) error, update any, isErrorHandler bool) *gateway.AdapterTask {
	return a.spawnTask(ctx, fn, update, isErrorHandler, nil)
}

// Spawns the task wrapper routing a failure to the error handlers, then runs done with whether it was cancelled
func (a *Application) spawnTask(ctx context.Context, fn func(ctx context.Context) error, update any, isErrorHandler bool, done func(ctx context.Context, cancelled bool)) *gateway.AdapterTask {
	task := gateway.SpawnAdapterTask(ctx, func(tctx context.Context) (err error) {
		defer a.forgetTask(gateway.CurrentAdapterTask(tctx))
		if done != nil {
			defer func() { done(tctx, errors.Is(err, context.Canceled)) }()
		}
		if tctx.Err() != nil {
			return tctx.Err()
		}
		err = fn(tctx)
		if err == nil || errors.Is(err, context.Canceled) {
			return err
		}
		switch {
		case errors.Is(err, ErrApplicationHandlerStop):
			slog.Warn("ApplicationHandlerStop is not supported with handlers running non-blocking.")
		case isErrorHandler:
			slog.Error("An error was raised and an uncaught error was raised while handling the error with an error_handler.", "error", err)
		default:
			if _, perr := a.impl.processError(tctx, update, err); perr != nil {
				return fmt.Errorf("%w (while handling %w)", perr, err)
			}
		}
		return err
	})
	a.mu.Lock()
	if a.createdTasks == nil {
		a.createdTasks = map[*gateway.AdapterTask]struct{}{}
	}
	for t := range a.createdTasks {
		if t.Done() {
			delete(a.createdTasks, t)
		}
	}
	if !task.Done() {
		a.createdTasks[task] = struct{}{}
	}
	a.mu.Unlock()
	return task
}

// Drops a finished task from the set Stop waits for
func (a *Application) forgetTask(task *gateway.AdapterTask) {
	a.mu.Lock()
	delete(a.createdTasks, task)
	a.mu.Unlock()
}

// Marks the application running, the PTB start after initialize
func (a *Application) Start() {
	a.mu.Lock()
	a.running = true
	a.mu.Unlock()
}

// Whether the application was started and not yet stopped
func (a *Application) Running() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

// Stops the application and waits for every task its handlers created, the PTB stop
func (a *Application) Stop(ctx context.Context) error {
	a.mu.Lock()
	if !a.running {
		a.mu.Unlock()
		return &runtimeError{msg: "This Application is not running!"}
	}
	a.running = false
	tasks := make([]*gateway.AdapterTask, 0, len(a.createdTasks))
	for t := range a.createdTasks {
		tasks = append(tasks, t)
	}
	a.mu.Unlock()
	slog.Info("Application is stopping. This might take a moment.")
	if len(tasks) > 0 {
		slog.Debug("Waiting for `create_task` calls to be processed")
		for _, t := range tasks {
			if err := t.Wait(ctx); err != nil && ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	slog.Info("Application.stop() complete")
	return nil
}
