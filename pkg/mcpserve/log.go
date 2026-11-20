package mcpserve

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
)

// Threshold every mcp-serve logger shares, WARNING until the server configures logging
var logLevel = func() *slog.LevelVar {
	v := new(slog.LevelVar)
	v.Set(slog.LevelWarn)
	return v
}()

// Serializes stderr writes across loggers
var logMu sync.Mutex

// Stderr lines in the levelname name message layout of logging.basicConfig
type logHandler struct {
	name string
	out  io.Writer
}

func (h *logHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= logLevel.Level() }
func (h *logHandler) WithGroup(string) slog.Handler                { return h }

// Handler printing the package a logger attribute names, other bound attributes dropped
func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	for _, a := range attrs {
		if a.Key == "logger" {
			next.name = a.Value.String()
		}
	}
	return &next
}

func (h *logHandler) Handle(_ context.Context, r slog.Record) error {
	names := map[slog.Level]string{slog.LevelDebug: "DEBUG", slog.LevelInfo: "INFO", slog.LevelWarn: "WARNING", slog.LevelError: "ERROR"}
	msg, name := r.Message, h.name
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "logger" {
			name = a.Value.String()
		} else {
			msg += " " + a.Key + "=" + a.Value.String()
		}
		return true
	})
	logMu.Lock()
	defer logMu.Unlock()
	_, err := io.WriteString(h.out, names[r.Level]+":"+name+":"+msg+"\n")
	return err
}

// Logger writing as name to stderr under the shared threshold
func newLogger(name string) *slog.Logger { return slog.New(&logHandler{name: name, out: os.Stderr}) }

// Logger named after this package, writing to stderr before and after logging is configured
func pkgLog() *slog.Logger { return newLogger("mcpserve") }

// Sets the shared threshold to DEBUG when verbose and WARNING otherwise and routes the process default logger through it
func configureLogging(verbose bool) {
	if verbose {
		logLevel.Set(slog.LevelDebug)
	} else {
		logLevel.Set(slog.LevelWarn)
	}
	slog.SetDefault(newLogger("root"))
}
