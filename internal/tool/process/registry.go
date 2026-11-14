// Background processes the agent starts, with their output, owner-death supervision and crash recovery
package process

import (
	"context"
	"errors"
	"io"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/proc"
	"github.com/nickheyer/icebath/internal/tool"
	"github.com/nickheyer/icebath/internal/tool/redact"
)

const (
	ptyRows             = 30
	ptyCols             = 120
	finalDrainMaxChars  = 1 << 20
	pipeIdleWait        = 200 * time.Millisecond
	pipeIdleCycles      = 3
	pipeReadBytes       = 4096
	heartbeatMinSeconds = 60
	heartbeatTick       = 5 * time.Second
	untrackedReapWait   = 5 * time.Second
)

type Host interface {
	Shell(ctx context.Context) (string, error)
	SafeCwd(cwd string) string
	ChildEnv(ctx context.Context, extra map[string]string) (map[string]string, error)
	RestoreLoginPath(command, parentPath string) string
	RewriteBackground(command string) string
}

type Role interface {
	Worker() bool
	SupervisedGateway() bool
}

type Transformer interface {
	TransformTerminalOutput(ctx context.Context, command, output string, exitCode *int, taskID, envType string) (string, bool)
}

type GroupHandoff interface {
	HandOffGroup() (pgid int, ok bool)
	ReturnGroup(pgid int)
}

type OutputSink func(s *Session, chunk string)

type Options struct {
	Homes      *home.Resolver
	Config     *config.Store
	Redactor   *redact.Redactor
	Guard      *proc.Guard
	Ledger     *proc.Ledger
	Scopes     *Scopes
	Host       Host
	Role       Role
	Transform  Transformer
	Delegation Delegation
	Lineage    Lineage
	Terminal   TerminalSettings
}

type CloseSink func(s *Session, id string) error

type WatcherArmer func(w Watcher) bool

type Watcher struct {
	SessionID       string
	CheckInterval   int
	SessionKey      string
	Route           Route
	Notify          bool
	ParentSessionID string
}

type ptyJob interface {
	pid() int
	read(p []byte) (int, error)
	write(p []byte) (int, error)
	sendEOF() error
	terminate(force bool) (bool, error)
	alive() bool
	wait(ctx context.Context) (*int, error)
	exitCode() (*int, bool)
	close() error
	answersQueries() bool
}

type sessionTable struct {
	m     map[string]*Session
	order []string
}

func newSessionTable() sessionTable { return sessionTable{m: map[string]*Session{}} }

func (t *sessionTable) put(s *Session) {
	if _, ok := t.m[s.id]; !ok {
		t.order = append(t.order, s.id)
	}
	t.m[s.id] = s
}

func (t *sessionTable) get(id string) (*Session, bool) {
	s, ok := t.m[id]
	return s, ok
}

func (t *sessionTable) remove(id string) {
	if _, ok := t.m[id]; !ok {
		return
	}
	delete(t.m, id)
	for i, v := range t.order {
		if v == id {
			t.order = append(t.order[:i], t.order[i+1:]...)
			break
		}
	}
}

func (t *sessionTable) list() []*Session {
	out := make([]*Session, 0, len(t.order))
	for _, id := range t.order {
		out = append(out, t.m[id])
	}
	return out
}

func (t *sessionTable) len() int { return len(t.m) }

type Registry struct {
	homes      *home.Resolver
	cfg        *config.Store
	redactor   *redact.Redactor
	guard      *proc.Guard
	ledger     *proc.Ledger
	scopes     *Scopes
	host       Host
	role       Role
	transform  Transformer
	delegation Delegation
	lineage    Lineage
	terminal   TerminalSettings

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu               sync.Mutex
	closed           bool
	running          sessionTable
	finished         sessionTable
	consumed         map[string]bool
	pollObserved     map[string]bool
	heartbeatRunning bool
	watchers         []Watcher

	jobsOutliveHost atomic.Bool
	output          atomic.Pointer[OutputSink]
	closeSink       atomic.Pointer[CloseSink]
	armer           atomic.Pointer[WatcherArmer]
	events          eventQueue
	breaker         watchBreaker
	restoreOnce     sync.Once
	policy          waitPolicy
}

var ErrClosed = errors.New("process registry is closed")

func unfilled(name string) error { return &tool.UnfilledError{Name: name} }

func New(ctx context.Context, o Options) (*Registry, error) {
	switch {
	case o.Homes == nil:
		return nil, unfilled("process.Options.Homes")
	case o.Config == nil:
		return nil, unfilled("process.Options.Config")
	case o.Redactor == nil:
		return nil, unfilled("process.Options.Redactor")
	case o.Guard == nil:
		return nil, unfilled("process.Options.Guard")
	case o.Ledger == nil:
		return nil, unfilled("process.Options.Ledger")
	case o.Scopes == nil:
		return nil, unfilled("process.Options.Scopes")
	case o.Host == nil:
		return nil, unfilled("process.Host")
	case o.Role == nil:
		return nil, unfilled("process.Role")
	case o.Transform == nil:
		return nil, unfilled("process.Transformer")
	case o.Delegation == nil:
		return nil, unfilled("process.Delegation")
	case o.Lineage == nil:
		return nil, unfilled("process.Lineage")
	case o.Terminal == nil:
		return nil, unfilled("process.TerminalSettings")
	}
	r := &Registry{
		homes:        o.Homes,
		cfg:          o.Config,
		redactor:     o.Redactor,
		guard:        o.Guard,
		ledger:       o.Ledger,
		scopes:       o.Scopes,
		host:         o.Host,
		role:         o.Role,
		transform:    o.Transform,
		delegation:   o.Delegation,
		lineage:      o.Lineage,
		terminal:     o.Terminal,
		running:      newSessionTable(),
		finished:     newSessionTable(),
		consumed:     map[string]bool{},
		pollObserved: map[string]bool{},
		events:       eventQueue{ready: make(chan struct{}, 1), inFlight: map[string]int{}},
		policy:       waitPolicy{state: map[string]waitObservation{}},
	}
	r.ctx, r.cancel = context.WithCancel(context.WithoutCancel(ctx))
	return r, nil
}

func (r *Registry) Close(ctx context.Context) error {
	r.cancel()
	r.mu.Lock()
	r.closed = true
	live := r.running.list()
	r.mu.Unlock()
	for _, s := range live {
		s.requestFinish()
		closeHandles(s)
	}
	stopped := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Registry) SetOutputSink(fn OutputSink) {
	if fn == nil {
		r.output.Store(nil)
		return
	}
	r.output.Store(&fn)
}

func (r *Registry) SetCloseSink(fn CloseSink) {
	if fn == nil {
		r.closeSink.Store(nil)
		return
	}
	r.closeSink.Store(&fn)
}

func (r *Registry) SetWatcherArmer(fn WatcherArmer) {
	if fn == nil {
		r.armer.Store(nil)
		return
	}
	r.armer.Store(&fn)
}

func (r *Registry) RegisterWatcher(s *Session, sessionKey string) {
	s.mu.Lock()
	s.watcherInterval = completionWatcherInterval
	w := Watcher{
		SessionID:       s.id,
		CheckInterval:   completionWatcherInterval,
		SessionKey:      sessionKey,
		Route:           s.route,
		Notify:          true,
		ParentSessionID: s.parentSessionID,
	}
	s.mu.Unlock()
	if arm := r.armer.Load(); arm != nil && (*arm)(w) {
		return
	}
	r.AddWatcher(w)
}

func (r *Registry) PendingWatchers() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.watchers)
}

func (r *Registry) AddWatcher(w Watcher) {
	r.mu.Lock()
	r.watchers = append(r.watchers, w)
	r.mu.Unlock()
}

func (r *Registry) TakeWatchers() []Watcher {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.watchers
	r.watchers = nil
	return out
}

func (r *Registry) bind(ctx context.Context) (context.Context, func()) {
	c, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(r.ctx, cancel)
	return c, func() {
		stop()
		cancel()
	}
}

func (r *Registry) termGrace(ctx context.Context) time.Duration { return TermGrace(ctx, r.cfg) }

func (r *Registry) trackStarted(ctx context.Context, s *Session, reader func(ctx context.Context)) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrClosed
	}
	s.readerActive.Store(true)
	r.pruneLocked()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer close(s.readerDone)
		rctx, release := r.bind(s.ctx)
		defer release()
		reader(rctx)
	}()
	r.running.put(s)
	r.mu.Unlock()
	r.writeCheckpoint(ctx, nil)
	return nil
}

func (r *Registry) reapUntracked(ctx context.Context, s *Session) {
	if s.systemdUnit != "" {
		stopUnit(ctx, s.systemdUnit)
	}
	if s.pid != 0 {
		Terminate(ctx, s.pid, s.hostStart, r.termGrace(ctx))
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), untrackedReapWait)
	defer cancel()
	if s.process != nil {
		_, _ = s.process.Wait(wctx)
	}
	if s.pty != nil {
		_, _ = s.pty.wait(wctx)
	}
	closeHandles(s)
	r.releaseJobGroup(s)
}

func (r *Registry) pruneLocked() {
	t := now()
	var prunable []*Session
	for _, s := range r.finished.list() {
		s.mu.Lock()
		keep := s.scopeStopPending || s.finishClaimed && !s.isDone()
		s.mu.Unlock()
		if !keep {
			prunable = append(prunable, s)
		}
	}
	expired := map[string]bool{}
	var order []*Session
	for _, s := range prunable {
		if t-s.startedAt > finishedTTLSeconds {
			expired[s.id] = true
			order = append(order, s)
		}
	}
	if r.running.len()+r.finished.len()-len(expired) >= maxProcesses {
		var oldest *Session
		for _, s := range prunable {
			if !expired[s.id] && (oldest == nil || s.startedAt < oldest.startedAt) {
				oldest = s
			}
		}
		if oldest != nil {
			order = append(order, oldest)
		}
	}
	for _, s := range order {
		r.releaseFinishedHandles(s, false)
		r.finished.remove(s.id)
	}
	for id := range r.consumed {
		if !r.tracked(id) {
			delete(r.consumed, id)
		}
	}
	for id := range r.pollObserved {
		if !r.tracked(id) {
			delete(r.pollObserved, id)
		}
	}
}

func (r *Registry) tracked(id string) bool {
	if _, ok := r.running.get(id); ok {
		return true
	}
	_, ok := r.finished.get(id)
	return ok
}

func cleanShellNoise(text string) string {
	shellNoise := [...]string{
		"no job control in this shell", "cannot set terminal process group",
		"tcsetattr: Inappropriate ioctl for device",
	}
	lines := strings.Split(text, "\n")
	for len(lines) > 0 {
		noisy := false
		for _, n := range shellNoise {
			if strings.Contains(lines[0], n) {
				noisy = true
				break
			}
		}
		if !noisy {
			break
		}
		lines = lines[1:]
	}
	return strings.Join(lines, "\n")
}

func (r *Registry) emitOutput(s *Session, chunk string) {
	sink := r.output.Load()
	if sink == nil || chunk == "" {
		return
	}
	(*sink)(s, chunk)
}

func (r *Registry) ingest(s *Session, text string, unlessExited bool) {
	if !unlessExited {
		s.appendOutput(text)
	} else if !s.appendOutputIfRunning(text) {
		return
	}
	r.checkWatch(s, text)
	r.emitOutput(s, text)
}

func (r *Registry) finishExited(ctx context.Context, s *Session, code *int, reason, source string, fromReader bool) {
	s.mu.Lock()
	s.markExitedLocked(code, reason, source)
	s.mu.Unlock()
	r.moveToFinished(ctx, s, fromReader)
}

func (r *Registry) moveToFinished(ctx context.Context, s *Session, fromReader bool) bool {
	r.mu.Lock()
	_, wasRunning := r.running.get(s.id)
	if wasRunning {
		s.mu.Lock()
		s.finishClaimed = true
		s.exitedAt = now()
		s.mu.Unlock()
		r.saveCompletedResult(ctx, s)
		r.running.remove(s.id)
	}
	r.finished.put(s)
	r.mu.Unlock()
	r.releaseJobGroup(s)
	defer func() {
		s.mu.Lock()
		claimed := s.finishClaimed
		s.mu.Unlock()
		if wasRunning || !claimed {
			s.markDone()
		}
	}()
	r.publishFinished(ctx, s, wasRunning, fromReader)
	return wasRunning
}

func (r *Registry) publishFinished(ctx context.Context, s *Session, wasRunning, fromReader bool) {
	r.releaseFinishedHandles(s, fromReader)
	r.writeCheckpoint(ctx, nil)
	s.mu.Lock()
	if !wasRunning || !s.notify {
		s.mu.Unlock()
		return
	}
	output, cut := s.completionOutputLocked()
	ev := Event{
		Type:              EventCompletion,
		SessionID:         s.id,
		SessionKey:        s.sessionKey,
		TaskID:            s.taskID,
		OwnerTaskID:       s.ownerTaskID,
		Command:           s.command,
		HandoffNote:       s.handoffNote,
		ExitCode:          s.exitCode,
		CompletionReason:  s.completionReason,
		TerminationSource: s.terminationSource,
		Output:            output,
		OutputCut:         max(cut, 0),
		StartedAt:         s.startedAt,
	}
	s.mu.Unlock()
	r.redactEvent(ctx, &ev)
	r.events.put(ev)
}

func closeHandles(s *Session) {
	if s.process != nil {
		if c, ok := s.process.(io.Closer); ok {
			_ = c.Close()
		} else if out := s.process.Output(); out != nil {
			if c, ok := out.(io.Closer); ok {
				_ = c.Close()
			}
		}
		if in := s.process.Stdin(); in != nil {
			_ = in.Close()
		}
	}
	if s.pty != nil {
		_ = s.pty.close()
	}
}

func (r *Registry) releaseFinishedHandles(s *Session, fromReader bool) {
	if s.pty != nil && runtime.GOOS != "windows" && !fromReader && s.readerAlive() {
		return
	}
	closeHandles(s)
}

func FormatUptimeShort(seconds int) string {
	s := max(0, seconds)
	if s < 60 {
		return strconv.Itoa(s) + "s"
	}
	mins, secs := s/60, s%60
	if mins < 60 {
		return strconv.Itoa(mins) + "m " + strconv.Itoa(secs) + "s"
	}
	return strconv.Itoa(mins/60) + "h " + strconv.Itoa(mins%60) + "m"
}

type notFoundError struct{ id string }

func (e *notFoundError) Error() string { return "No process with ID " + e.id }
