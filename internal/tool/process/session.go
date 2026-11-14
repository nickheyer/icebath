package process

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/tool"
)

const (
	maxOutputChars        = 200_000
	completionOutputChars = 2000
	finishedTTLSeconds    = 1800
	maxProcesses          = 64
)

const (
	reasonExited  = "exited"
	reasonKilled  = "killed"
	reasonLost    = "lost"
	sourceLost    = "backend_lost"
	scopeHost     = "host"
	scopeSandbox  = "sandbox"
	purposeJob    = "terminal-background"
	workerUnitTag = "icebath-worker-"
)

type Route struct {
	Platform  string
	ChatID    string
	UserID    string
	UserName  string
	ThreadID  string
	MessageID string
}

type SpawnOptions struct {
	Command          string
	Cwd              string
	TaskID           string
	OwnerTaskID      string
	SessionKey       string
	ParentSessionID  string
	Env              map[string]string
	PTY              bool
	PersistOnRelease bool
	RestoreLoginPath bool
	Notify           bool
	OutputChars      int
	WatchPatterns    []string
	Route            Route
	WatcherInterval  int
}

type outputBuffer struct {
	data  []byte
	head  int
	runes int
	total int
}

func (b *outputBuffer) append(text string, limit int) {
	b.data = append(b.data, text...)
	n := utf8.RuneCountInString(text)
	b.runes += n
	b.total += n
	for ; b.runes > limit; b.runes-- {
		_, size := utf8.DecodeRune(b.data[b.head:])
		b.head += size
	}
	if b.head > 0 && b.head >= len(b.data)/2 {
		b.data = append(b.data[:0:0], b.data[b.head:]...)
		b.head = 0
	}
}

func (b *outputBuffer) replace(text string) {
	b.data = []byte(text)
	b.head = 0
	b.runes = utf8.RuneCountInString(text)
}

func (b *outputBuffer) String() string { return string(b.data[b.head:]) }

type Session struct {
	id              string
	command         string
	taskID          string
	sessionKey      string
	parentSessionID string
	cwd             string
	startedAt       float64
	pidScope        string
	wslChain        bool
	detached        bool
	ctx             context.Context
	env             tool.Environment
	process         tool.Process
	pty             ptyJob

	mu                 sync.Mutex
	ownerTaskID        string
	pid                int
	hostStart          *int64
	exited             bool
	exitedAt           float64
	exitCode           *int
	completionReason   string
	terminationSource  string
	out                outputBuffer
	maxOutput          int
	systemdUnit        string
	handoffNote        string
	persistOnRelease   bool
	outputLog          string
	ptyFallback        string
	route              Route
	watcherInterval    int
	notify             bool
	outputChars        int
	watchPatterns      []string
	heartbeatSeconds   int
	heartbeatLast      float64
	heartbeatTotal     int
	heartbeatSeq       int
	watchHits          int
	watchSuppressed    int
	watchDisabled      bool
	watchCooldownUntil float64
	watchStrikePending bool
	watchStrikes       int
	finishClaimed      bool
	scopeStopPending   bool
	ownedPgid          int

	done         chan struct{}
	doneOnce     sync.Once
	finishReq    chan struct{}
	finishOnce   sync.Once
	readerDone   chan struct{}
	readerActive atomic.Bool
	selectable   atomic.Bool
}

func (s *Session) ID() string { return s.id }

func (s *Session) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pid
}

func (s *Session) SessionKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionKey
}

type Snapshot struct {
	ID                string
	Command           string
	TaskID            string
	OwnerTaskID       string
	SessionKey        string
	ParentSessionID   string
	Cwd               string
	PID               int
	StartedAt         float64
	ExitedAt          float64
	Exited            bool
	ExitCode          *int
	CompletionReason  string
	TerminationSource string
	Notify            bool
	WatchPatterns     []string
	WatchHit          bool
	PersistOnRelease  bool
	Detached          bool
	HandoffNote       string
	Output            string
	TotalOutputChars  int
	Route             Route
	WatcherInterval   int
}

func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	var patterns []string
	if !s.watchDisabled {
		patterns = append(patterns, s.watchPatterns...)
	}
	return Snapshot{
		ID:                s.id,
		Command:           s.command,
		TaskID:            s.taskID,
		OwnerTaskID:       s.ownerTaskID,
		SessionKey:        s.sessionKey,
		ParentSessionID:   s.parentSessionID,
		Cwd:               s.cwd,
		PID:               s.pid,
		StartedAt:         s.startedAt,
		ExitedAt:          s.exitedAt,
		Exited:            s.exited,
		ExitCode:          s.exitCode,
		CompletionReason:  s.completionReason,
		TerminationSource: s.terminationSource,
		Notify:            s.notify,
		WatchPatterns:     patterns,
		WatchHit:          len(patterns) > 0 && s.watchHits > 0,
		PersistOnRelease:  s.persistOnRelease,
		Detached:          s.detached,
		HandoffNote:       s.handoffNote,
		Output:            s.out.String(),
		TotalOutputChars:  s.out.total,
		Route:             s.route,
		WatcherInterval:   s.watcherInterval,
	}
}

func (s *Session) Done() <-chan struct{} { return s.done }

func (s *Session) ExitEvent() Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	output, cut := s.completionOutputLocked()
	return Event{
		Type:              EventCompletion,
		SessionID:         s.id,
		Command:           s.command,
		ExitCode:          s.exitCode,
		CompletionReason:  s.completionReason,
		TerminationSource: s.terminationSource,
		Output:            output,
		OutputCut:         max(cut, 0),
	}
}

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func newProcessID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "proc_" + hex.EncodeToString(b[:])
}

func newSession(ctx context.Context, id string, o SpawnOptions, cwd string) *Session {
	owner := o.OwnerTaskID
	if owner == "" {
		owner = o.TaskID
	}
	s := &Session{
		id:               id,
		command:          o.Command,
		taskID:           o.TaskID,
		ownerTaskID:      owner,
		sessionKey:       o.SessionKey,
		parentSessionID:  o.ParentSessionID,
		cwd:              cwd,
		startedAt:        now(),
		pidScope:         scopeHost,
		wslChain:         wslLauncherCommand(o.Command),
		ctx:              context.WithoutCancel(ctx),
		completionReason: reasonExited,
		maxOutput:        maxOutputChars,
		persistOnRelease: o.PersistOnRelease,
		route:            o.Route,
		watcherInterval:  o.WatcherInterval,
		notify:           o.Notify,
		outputChars:      o.OutputChars,
		watchPatterns:    append([]string(nil), o.WatchPatterns...),
	}
	s.initSignals()
	return s
}

func (s *Session) initSignals() {
	s.done = make(chan struct{})
	s.finishReq = make(chan struct{})
	s.readerDone = make(chan struct{})
}

func (s *Session) markDone() { s.doneOnce.Do(func() { close(s.done) }) }

func (s *Session) isDone() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *Session) requestFinish() { s.finishOnce.Do(func() { close(s.finishReq) }) }

func (s *Session) finishRequested() bool {
	select {
	case <-s.finishReq:
		return true
	default:
		return false
	}
}

func (s *Session) readerAlive() bool {
	if !s.readerActive.Load() {
		return false
	}
	select {
	case <-s.readerDone:
		return false
	default:
		return true
	}
}

func (s *Session) isExited() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exited
}

func (s *Session) appendOutput(text string) {
	s.mu.Lock()
	s.out.append(text, s.maxOutput)
	s.mu.Unlock()
}

func (s *Session) appendOutputIfRunning(text string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited {
		return false
	}
	s.out.append(text, s.maxOutput)
	return true
}

func (s *Session) markExitedLocked(code *int, reason, source string) {
	s.exited = true
	if s.completionReason != reasonKilled {
		s.exitCode = code
		s.completionReason = reason
		if source != "" {
			s.terminationSource = source
		}
	}
}

func (s *Session) outputTailLocked(n int) string {
	return tool.StripANSI(runeTail(s.out.String(), n))
}

func (s *Session) completionOutputLocked() (string, int) {
	limit := s.outputChars
	if limit == 0 {
		limit = completionOutputChars
	}
	return s.outputTailLocked(limit), s.out.runes - limit
}

func intPtr(v int) *int { return &v }
