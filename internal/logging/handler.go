package logging

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/home"
)

// Attribute key that names the source a logger was given
const nameKey = "logger"

// Source name of a record whose logger was given none
const rootName = "root"

// One log event as the sinks receive it, never changed once built
type record struct {
	time    time.Time
	level   slog.Level
	name    string
	message string
	session string
	home    string
	sink    *dedicated
	seq     uint64
	size    int
}

// Record handler every slog logger of the process logs through
type front struct {
	s      *System
	name   string
	prefix string
	bound  string
	sink   *dedicated
}

// Record sink product loggers are built on, feeding the consoles, the shared log files and the last resort
func (s *System) Handler() slog.Handler { return &front{s: s} }

// Reports whether a record at level from this handler can reach any sink
func (h *front) Enabled(_ context.Context, level slog.Level) bool {
	v := h.s.view.Load()
	if level <= v.disabled {
		return false
	}
	switch {
	case h.sink != nil:
		return level >= h.sink.level
	case h.name != "":
		return level >= v.effective(h.name)
	}
	return level >= v.lowest
}

// Builds the record with its bound and own attributes and hands it to the sinks
func (h *front) Handle(ctx context.Context, r slog.Record) error {
	name := h.name
	var b strings.Builder
	b.WriteString(r.Message)
	b.WriteString(h.bound)
	r.Attrs(func(a slog.Attr) bool {
		if h.prefix == "" && a.Key == nameKey {
			name = a.Value.Resolve().String()
			return true
		}
		renderAttr(&b, h.prefix, a)
		return true
	})
	if name == "" {
		name = h.s.callerName(r.PC)
	}
	t := r.Time
	if t.IsZero() {
		t = time.Now()
	}
	h.s.emit(ctx, &record{time: t, level: r.Level, name: name, message: b.String(), session: Session(ctx), sink: h.sink})
	return nil
}

// Import path prefix of every package of this module
const modulePath = "github.com/nickheyer/icebath/"

// Source name of the code at pc, its package path below internal as dotted words, root for foreign code
func (s *System) callerName(pc uintptr) string {
	if pc == 0 {
		return rootName
	}
	if known, ok := s.callers.Load(pc); ok {
		return known.(string)
	}
	frame, _ := runtime.CallersFrames([]uintptr{pc}).Next()
	name := packageName(frame.Function)
	s.callers.Store(pc, name)
	return name
}

// Dotted source name for a fully qualified function name, root when the function is outside this module
func packageName(function string) string {
	path, ok := strings.CutPrefix(function, modulePath)
	if !ok {
		return rootName
	}
	if cut := strings.IndexAny(path, "[("); cut >= 0 {
		path = path[:cut]
	}
	slash := strings.LastIndexByte(path, '/')
	dot := strings.IndexByte(path[slash+1:], '.')
	if dot < 0 {
		return rootName
	}
	path = path[:slash+1+dot]
	if below, ok := strings.CutPrefix(path, "internal/"); ok {
		path = below
	} else if below, ok := strings.CutPrefix(path, "desktop/internal/"); ok {
		path = "desktop/" + below
	}
	return strings.ReplaceAll(path, "/", ".")
}

// Handler binding attrs, a top level logger attribute naming the source instead
func (h *front) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	var b strings.Builder
	for _, a := range attrs {
		if h.prefix == "" && a.Key == nameKey {
			next.name = a.Value.Resolve().String()
			continue
		}
		renderAttr(&b, h.prefix, a)
	}
	next.bound += b.String()
	return &next
}

// Handler whose later attributes are keyed below the group name
func (h *front) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	next.prefix += name + "."
	return &next
}

// Appends a as space separated key=value text with groups flattened into dotted keys
func renderAttr(b *strings.Builder, prefix string, a slog.Attr) {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, m := range v.Group() {
			renderAttr(b, p, m)
		}
		return
	}
	if a.Key == "" && v.Kind() == slog.KindAny && v.Any() == nil {
		return
	}
	b.WriteByte(' ')
	b.WriteString(quoteText(prefix + a.Key))
	b.WriteByte('=')
	b.WriteString(quoteText(v.String()))
}

// Text quoted when empty, invalid UTF-8, or holding a space, equals sign, quote or unprintable rune
func quoteText(s string) string {
	if s == "" || !utf8.ValidString(s) {
		return strconv.Quote(s)
	}
	for _, r := range s {
		if r == '=' || r == '"' || unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return strconv.Quote(s)
		}
	}
	return s
}

// Gates rec by level, writes it to every console, queues it for the files and falls back to stderr
func (s *System) emit(ctx context.Context, rec *record) {
	v := s.view.Load()
	if rec.level <= v.disabled {
		return
	}
	if rec.sink != nil {
		if rec.level >= rec.sink.level {
			rec.home = home.Override(ctx)
			s.q.push(rec)
		}
		return
	}
	if rec.level < v.effective(rec.name) {
		return
	}
	for _, c := range v.consoles {
		if rec.level >= c.level {
			c.write(s, ctx, rec)
		}
	}
	redacted := ""
	for _, o := range v.observers {
		if rec.level < o.level {
			continue
		}
		if redacted == "" {
			redacted = s.redact.Redact(ctx, rec.message)
		}
		o.fn(ctx, Entry{Time: rec.time, Level: rec.level, Logger: rec.name, Message: redacted, Session: rec.session})
	}
	if v.files {
		if h, err := s.resolver.Home(ctx); err == nil {
			rec.home = h
		}
		s.q.push(rec)
	}
	if len(v.consoles) == 0 && len(v.observers) == 0 && !v.files && rec.level >= slog.LevelWarn {
		s.lastResort(ctx, rec)
	}
}

// Writes the redacted bare message to stderr while no console, observer or log file is configured
func (s *System) lastResort(ctx context.Context, rec *record) {
	_, _ = io.WriteString(SafeStderr(), s.redact.Redact(ctx, rec.message)+"\n")
}
