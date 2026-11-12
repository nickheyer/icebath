package logging

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Hooks pkg/tools sets in an init so formatters and stdio redact the way agent.redact does
var (
	RedactingHandler     func(next slog.Handler) slog.Handler
	RedactSensitiveText  func(text string) string
	ResolveProfilePolicy func(home string)
)

var (
	errRedactUnlinked = errors.New("ModuleNotFoundError: No module named 'agent.redact' (pkg/tools sets the logging redaction hooks)")
	errRedactDropped  = errors.New("RuntimeError: the redaction hook did not pass the formatted line on")
)

var sessionContext pythread.Local

// Tags every record the calling goroutine logs with the session id
func SetSessionContext(sessionID string) { sessionContext.Set(sessionID) }

// Stops tagging the calling goroutine's records with a session id
func ClearSessionContext() { sessionContext.Delete() }

// Record with the calling goroutine's session tag and the resolved home ctx names
func makeRecord(ctx context.Context, name string, level int, msg string, created time.Time, pc uintptr) *Record {
	rec := &Record{Name: name, Levelno: level, Levelname: LevelName(level), Msg: msg, Created: created, Thread: pythread.GoroutineID(), PC: pc}
	if v, ok := sessionContext.Get(); ok {
		if sid, _ := v.(string); sid != "" {
			rec.SessionTag = " [" + sid + "]"
		}
	}
	rec.HermesHome = recordHome(ctx)
	return rec
}

// Resolved home a record carries so the listener routes it after its context is gone, empty on failure
func recordHome(ctx context.Context) (h string) {
	defer func() {
		if recover() != nil {
			h = ""
		}
	}()
	resolved, err := pypath.Realpath(home.Dir(ctx), pypath.Lax)
	if err != nil {
		return ""
	}
	return resolved
}

// slog handler feeding the logger hierarchy, the logger name taken from the logger attribute
type bridge struct {
	name   string
	prefix string
	bound  string
}

// Installs the bridge as the slog default once, the way setup_logging wires the root logger
func installSlogDefault() {
	if _, ok := slog.Default().Handler().(*bridge); ok {
		return
	}
	slog.SetDefault(slog.New(&bridge{}))
	slog.SetLogLoggerLevel(slog.LevelDebug - 4)
	log.SetOutput(stdLogWriter{})
	log.SetFlags(0)
}

// Level prefix and logger attribute of a line the slog default text handler wrote through the log package
var (
	stdLogLevel  = regexp.MustCompile(`^(DEBUG|INFO|WARN|ERROR)([+-][0-9]+)? `)
	stdLogLogger = regexp.MustCompile(` logger=("(?:[^"\\]|\\.)*"|[^ ]+)`)
)

// Log package output fed back into the bridge, so loggers captured from slog before setup keep their names and levels
type stdLogWriter struct{}

func (stdLogWriter) Write(p []byte) (int, error) {
	text := strings.TrimSuffix(string(p), "\n")
	level := slog.LevelInfo
	if m := stdLogLevel.FindStringSubmatch(text); m != nil {
		base := map[string]slog.Level{"DEBUG": slog.LevelDebug, "INFO": slog.LevelInfo, "WARN": slog.LevelWarn, "ERROR": slog.LevelError}[m[1]]
		offset, _ := strconv.Atoi(m[2])
		level = base + slog.Level(offset)
		text = text[len(m[0]):]
	}
	r := slog.NewRecord(time.Now(), level, text, 0)
	if locs := stdLogLogger.FindAllStringSubmatchIndex(text, -1); len(locs) > 0 {
		loc := locs[len(locs)-1]
		name := text[loc[2]:loc[3]]
		if unq, err := strconv.Unquote(name); err == nil {
			name = unq
		}
		r = slog.NewRecord(r.Time, level, text[:loc[0]]+text[loc[1]:], 0)
		r.AddAttrs(slog.String("logger", name))
	}
	if (&bridge{}).Enabled(context.Background(), level) {
		_ = (&bridge{}).Handle(context.Background(), r)
	}
	return len(p), nil
}

func (b *bridge) Enabled(_ context.Context, level slog.Level) bool {
	py := fromSlog(level)
	if b.name != "" {
		return GetLogger(b.name).IsEnabledFor(py)
	}
	return int64(py) > disableLevel.Load() && int64(py) >= minLevel.Load()
}

func (b *bridge) Handle(ctx context.Context, r slog.Record) error {
	name := b.name
	var sb strings.Builder
	sb.WriteString(r.Message)
	sb.WriteString(b.bound)
	r.Attrs(func(a slog.Attr) bool {
		if b.prefix == "" && a.Key == "logger" {
			name = a.Value.Resolve().String()
			return true
		}
		renderAttr(&sb, b.prefix, a)
		return true
	})
	if name == "" {
		name = "root"
	}
	py := fromSlog(r.Level)
	lg := GetLogger(name)
	if !lg.IsEnabledFor(py) {
		return nil
	}
	created := r.Time
	if created.IsZero() {
		created = time.Now()
	}
	lg.handle(ctx, makeRecord(ctx, name, py, sb.String(), created, r.PC))
	return nil
}

func (b *bridge) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *b
	var sb strings.Builder
	for _, a := range attrs {
		if b.prefix == "" && a.Key == "logger" {
			next.name = a.Value.Resolve().String()
			continue
		}
		renderAttr(&sb, b.prefix, a)
	}
	next.bound += sb.String()
	return &next
}

func (b *bridge) WithGroup(name string) slog.Handler {
	if name == "" {
		return b
	}
	next := *b
	next.prefix += name + "."
	return &next
}

// Appends an attribute as space separated key=value text, groups flattened with dotted keys
func renderAttr(sb *strings.Builder, prefix string, a slog.Attr) {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, m := range v.Group() {
			renderAttr(sb, p, m)
		}
		return
	}
	if a.Key == "" && v.Any() == nil {
		return
	}
	sb.WriteByte(' ')
	sb.WriteString(quoteAttrText(prefix + a.Key))
	sb.WriteByte('=')
	sb.WriteString(quoteAttrText(v.String()))
}

// Text quoted when it is empty or holds a space, equals sign, quote or unprintable rune
func quoteAttrText(s string) string {
	if s == "" {
		return `""`
	}
	for _, r := range s {
		if r == '=' || r == '"' || unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return strconv.Quote(s)
		}
	}
	return s
}
