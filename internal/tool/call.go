package tool

import (
	"context"
	"encoding/json"
	"slices"
	"sync"

	"github.com/nickheyer/icebath/internal/model/secret"
)

type Call struct {
	ID           string
	Name         string
	Bridged      bool
	Arguments    string
	TaskID       string
	SessionID    string
	SessionKey   string
	TurnID       string
	APIRequestID string
	UserTask     string
	Origin       Origin
	Presence     Presence
	Cwd          string
	Env          EnvSource
	Secrets      *secret.Scope
	Approver     Approver
	Surface      Surface
	Progress     ProgressSink
	Notices      NoticeSink
	Activity     func(label string)
	Yield        *Yield
	State        *State
	Selection    *Selection
	SandboxTools []string
	Trace        []Trace
	Scope        Scope
}

type Origin struct {
	Platform         string
	Source           string
	Profile          string
	ProfileHome      string
	ChatID           string
	ChatName         string
	ChatType         string
	ThreadID         string
	MessageID        string
	ParentChatID     string
	UserID           string
	UserIDAlt        string
	UserName         string
	ScopeID          string
	UISessionID      string
	AsyncDelivery    bool
	HistoryDelivery  bool
	CronJob          string
	CronDeliver      Target
	KanbanTask       string
	DelegatedChild   bool
	RoomApprovalMode string
}

type Target struct {
	Platform string
	ChatID   string
	ThreadID string
}

type Presence struct {
	Interactive bool
	Gateway     bool
	Ask         bool
	Cron        bool
	SingleQuery bool
	Oneshot     bool
	Unattended  bool
	Viewer      bool
}

type Scope struct {
	Profile string
	Session string
}

type Trace = json.RawMessage

type State struct {
	mu       sync.Mutex
	slots    map[*keyID]*slot
	cleanups []func(ctx context.Context)
	closed   bool
}

type keyID struct{ name string }

type slot struct {
	once  sync.Once
	value any
}

type Key[T any] struct{ id *keyID }

func NewKey[T any](name string) Key[T] {
	return Key[T]{id: &keyID{name: name}}
}

func NewState() *State {
	return &State{slots: map[*keyID]*slot{}}
}

func Load[T any](s *State, k Key[T], create func() T) T {
	s.mu.Lock()
	if s.slots == nil {
		s.slots = map[*keyID]*slot{}
	}
	sl, ok := s.slots[k.id]
	if !ok {
		sl = &slot{}
		s.slots[k.id] = sl
	}
	s.mu.Unlock()
	sl.once.Do(func() { sl.value = create() })
	return sl.value.(T)
}

func (s *State) OnClose(fn func(ctx context.Context)) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		fn(context.Background())
		return
	}
	s.cleanups = append(s.cleanups, fn)
	s.mu.Unlock()
}

func (s *State) Close(ctx context.Context) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	fns := slices.Clone(s.cleanups)
	s.cleanups = nil
	s.mu.Unlock()
	for i := len(fns) - 1; i >= 0; i-- {
		fns[i](ctx)
	}
}
