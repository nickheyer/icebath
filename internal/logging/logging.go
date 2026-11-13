// Rotating log files, slog handlers, per-profile routing, log readers and session export for every icebath process
package logging

import (
	"context"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/internal/home"
)

// Level above error that the log lines name CRITICAL
const LevelCritical = slog.Level(12)

// Level below every record, letting all records through when a source or the root carries it
const levelNotSet = slog.Level(math.MinInt32)

// Secret scrubbing every formatted line passes through, supplied by the redaction package
type Redactor interface {
	Redact(ctx context.Context, text string) string
	ResolveHomePolicy(home string)
}

// Logging state of one process, its sinks, the record queue and the per-profile routing
type System struct {
	resolver    *home.Resolver
	redact      Redactor
	view        atomic.Pointer[view]
	q           *queue
	done        chan struct{}
	inline      sync.Mutex
	closeOnce   sync.Once
	mu          sync.Mutex
	levels      map[string]slog.Level
	root        slog.Level
	disabled    slog.Level
	consoles    []*console
	observers   []observer
	sets        []*fileSet
	routing     bool
	dedicated   map[string]*dedicated
	initialized bool
	stdio       []*redirect
	warned      map[string]bool
	callers     sync.Map
}

// Record path configuration the logging goroutines read without taking a lock
type view struct {
	consoles  []*console
	observers []observer
	levels    map[string]slog.Level
	root      slog.Level
	lowest    slog.Level
	disabled  slog.Level
	files     bool
	cache     sync.Map
}

// Logging for one process that resolves homes with r and scrubs every line with redact, its listener started
func New(r *home.Resolver, redact Redactor) *System {
	s := &System{
		resolver:  r,
		redact:    redact,
		q:         newQueue(),
		done:      make(chan struct{}),
		levels:    map[string]slog.Level{},
		root:      slog.LevelWarn,
		disabled:  levelNotSet,
		dedicated: map[string]*dedicated{},
		warned:    map[string]bool{},
	}
	s.publish()
	go s.listen()
	return s
}

// Publishes the current sources, consoles and level gates to the logging goroutines, caller holding mu
func (s *System) publish() {
	lowest := s.root
	for _, lv := range s.levels {
		lowest = min(lowest, lv)
	}
	s.view.Store(&view{
		consoles:  slices.Clone(s.consoles),
		observers: slices.Clone(s.observers),
		levels:    maps.Clone(s.levels),
		root:      s.root,
		lowest:    lowest,
		disabled:  s.disabled,
		files:     len(s.sets) > 0,
	})
}

// One record as an observer receives it, the message redacted with its attributes appended
type Entry struct {
	Time    time.Time
	Level   slog.Level
	Logger  string
	Message string
	Session string
}

// Callback taking every record at or above its level that clears the source level
type observer struct {
	level slog.Level
	fn    func(context.Context, Entry)
}

// Hands fn each record at or above level that clears its source level, on the goroutine that logged it
func (s *System) Observe(level slog.Level, fn func(context.Context, Entry)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observers = append(s.observers, observer{level: level, fn: fn})
	s.publish()
}

// Level a source name must reach, its own setting or the nearest dotted ancestor setting, else the root level
func (v *view) effective(name string) slog.Level {
	if lv, ok := v.cache.Load(name); ok {
		return lv.(slog.Level)
	}
	lv := v.root
	for n := name; ; {
		if set, ok := v.levels[n]; ok {
			lv = set
			break
		}
		i := strings.LastIndexByte(n, '.')
		if i < 0 {
			break
		}
		n = n[:i]
	}
	v.cache.Store(name, lv)
	return lv
}

// Sets the level a source and the names below it must reach, an empty name or root meaning the root
func (s *System) SetLevel(name string, level slog.Level) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setLevelLocked(name, level)
	s.publish()
}

// Records a source or root level, a not-set level on a source deferring to its ancestors, caller holding mu
func (s *System) setLevelLocked(name string, level slog.Level) {
	switch {
	case name == "" || name == "root":
		s.root = level
	case level == levelNotSet:
		delete(s.levels, name)
	default:
		s.levels[name] = level
	}
}

// Drops every record at or below level from every source and sink
func (s *System) Disable(level slog.Level) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.disabled = level
	s.publish()
}

// Context key carrying the session id records are tagged with
type sessionKey struct{}

// Context whose records carry the session id tag, an empty id clearing it
func WithSession(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionKey{}, id)
}

// Id the records logged with ctx are tagged with, empty when none is set
func Session(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(sessionKey{}).(string)
	return id
}

// Logger whose records carry name, the dotted source name the component filters and log lines use
func Named(l *slog.Logger, name string) *slog.Logger { return l.With(slog.String(nameKey, name)) }

// Records a one-time warning key, true the first time it is seen
func (s *System) firstWarning(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.warned[key] {
		return false
	}
	s.warned[key] = true
	return true
}
