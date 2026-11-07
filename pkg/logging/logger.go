package logging

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

const (
	LevelNotSet   = 0
	LevelDebug    = 10
	LevelInfo     = 20
	LevelWarning  = 30
	LevelError    = 40
	LevelCritical = 50
)

var levelNames = map[int]string{0: "NOTSET", 10: "DEBUG", 20: "INFO", 30: "WARNING", 40: "ERROR", 50: "CRITICAL"}

var levelByName = map[string]int{"CRITICAL": 50, "FATAL": 50, "ERROR": 40, "WARN": 30, "WARNING": 30, "INFO": 20, "DEBUG": 10, "NOTSET": 0}

// Name of a numeric level, Level N for one without a name
func LevelName(level int) string {
	if n, ok := levelNames[level]; ok {
		return n
	}
	return fmt.Sprintf("Level %d", level)
}

// One log event with the attributes the formatters render
type Record struct {
	Name       string
	Levelno    int
	Levelname  string
	Msg        string
	Created    time.Time
	SessionTag string
	HermesHome string
	Thread     uint64
	PC         uintptr
}

// Gate deciding whether a handler takes a record
type Filter interface {
	Filter(rec *Record) bool
}

// Destination for records, Handle applying the handler's filters before it emits
type Handler interface {
	Handle(ctx context.Context, rec *Record) bool
	Level() int
	Close() error
}

// Named node of the dotted logger hierarchy with its level, handlers and propagation switch
type Logger struct {
	name      string
	mu        sync.Mutex
	level     int
	handlers  []Handler
	propagate bool
}

var (
	registryMu sync.Mutex
	loggers    = map[string]*Logger{}
	root       = &Logger{name: "root", level: LevelWarning, propagate: true}
	minLevel   atomic.Int64
	moduleLock pythread.RLock
)

func init() { minLevel.Store(LevelWarning) }

// Logger for a dotted name, the root logger for an empty name or root
func GetLogger(name string) *Logger {
	if name == "" || name == "root" {
		return root
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	l, ok := loggers[name]
	if !ok {
		l = &Logger{name: name, propagate: true}
		loggers[name] = l
	}
	return l
}

// Nearest existing ancestor in the dotted hierarchy, root at the top
func parentOf(l *Logger) *Logger {
	if l == root {
		return nil
	}
	name := l.name
	registryMu.Lock()
	defer registryMu.Unlock()
	for i := strings.LastIndexByte(name, '.'); i > 0; i = strings.LastIndexByte(name, '.') {
		name = name[:i]
		if p, ok := loggers[name]; ok {
			return p
		}
	}
	return root
}

// Dotted name of the logger
func (l *Logger) Name() string { return l.name }

// Level set on this logger, zero when it defers to its ancestors
func (l *Logger) Level() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.level
}

// Sets the logger level and refreshes the cheapest level any logger accepts
func (l *Logger) SetLevel(level int) {
	l.mu.Lock()
	l.level = level
	l.mu.Unlock()
	lowest := root.Level()
	registryMu.Lock()
	all := make([]*Logger, 0, len(loggers))
	for _, other := range loggers {
		all = append(all, other)
	}
	registryMu.Unlock()
	for _, other := range all {
		if lv := other.Level(); lv != LevelNotSet && lv < lowest {
			lowest = lv
		}
	}
	minLevel.Store(int64(lowest))
}

// Turns propagation of records to ancestor handlers on or off
func (l *Logger) SetPropagate(on bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.propagate = on
}

// Attaches a handler to this logger
func (l *Logger) AddHandler(h Handler) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, existing := range l.handlers {
		if existing == h {
			return
		}
	}
	l.handlers = append(l.handlers, h)
}

// Detaches a handler from this logger
func (l *Logger) RemoveHandler(h Handler) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, existing := range l.handlers {
		if existing == h {
			l.handlers = append(l.handlers[:i:i], l.handlers[i+1:]...)
			return
		}
	}
}

// Handlers attached directly to this logger
func (l *Logger) Handlers() []Handler {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Handler(nil), l.handlers...)
}

// First level set walking up the hierarchy, zero when none is set
func (l *Logger) EffectiveLevel() int {
	for c := l; c != nil; c = parentOf(c) {
		if lv := c.Level(); lv != LevelNotSet {
			return lv
		}
	}
	return LevelNotSet
}

// Reports whether a record at level passes this logger's effective level
func (l *Logger) IsEnabledFor(level int) bool { return level >= l.EffectiveLevel() }

// Logs msg at level with the record attributes resolved from ctx
func (l *Logger) LogContext(ctx context.Context, level int, msg string) {
	if !l.IsEnabledFor(level) {
		return
	}
	l.handle(ctx, makeRecord(ctx, l.name, level, msg, time.Now(), 0))
}

// Logs msg at level
func (l *Logger) Log(level int, msg string) { l.LogContext(context.Background(), level, msg) }

// Logs msg at DEBUG
func (l *Logger) Debug(msg string) { l.Log(LevelDebug, msg) }

// Logs msg at INFO
func (l *Logger) Info(msg string) { l.Log(LevelInfo, msg) }

// Logs msg at WARNING
func (l *Logger) Warning(msg string) { l.Log(LevelWarning, msg) }

// Logs msg at ERROR
func (l *Logger) Error(msg string) { l.Log(LevelError, msg) }

// Passes rec to the handlers of this logger and each ancestor until propagation stops
func (l *Logger) handle(ctx context.Context, rec *Record) {
	found := 0
	for c := l; c != nil; c = parentOf(c) {
		c.mu.Lock()
		hs := append([]Handler(nil), c.handlers...)
		propagate := c.propagate
		c.mu.Unlock()
		for _, h := range hs {
			found++
			if rec.Levelno >= h.Level() {
				h.Handle(ctx, rec)
			}
		}
		if !propagate {
			break
		}
	}
	if found == 0 && rec.Levelno >= LevelWarning {
		fmt.Fprintln(os.Stderr, rec.Msg)
	}
}

var (
	handlerListMu sync.Mutex
	handlerList   []Handler
)

// Records a live handler for Shutdown, under the module lock as Handler init takes it
func registerHandler(h Handler) {
	moduleLock.Lock()
	defer moduleLock.Unlock()
	handlerListMu.Lock()
	defer handlerListMu.Unlock()
	handlerList = append(handlerList, h)
}

// Drops a closed handler from the Shutdown list under the module lock
func unregisterHandler(h Handler) {
	moduleLock.Lock()
	defer moduleLock.Unlock()
	handlerListMu.Lock()
	defer handlerListMu.Unlock()
	for i, existing := range handlerList {
		if existing == h {
			handlerList = append(handlerList[:i:i], handlerList[i+1:]...)
			return
		}
	}
}

// Stops the log listener after it writes every queued record, then closes every handler newest first
func Shutdown() {
	stopQueueListener()
	flushDetachedStdio()
	moduleLock.Lock()
	defer moduleLock.Unlock()
	handlerListMu.Lock()
	hs := append([]Handler(nil), handlerList...)
	handlerListMu.Unlock()
	for i := len(hs) - 1; i >= 0; i-- {
		quietly(hs[i].Close)
	}
}

// Calls a close or stop function and swallows its error and panic, teardown never raises
func quietly(fn func() error) {
	defer func() { _ = recover() }()
	_ = fn()
}
