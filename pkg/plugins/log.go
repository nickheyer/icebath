package plugins

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// Logger every plugin subsystem message goes through, replaceable by the host
var Logger = slog.Default()

func logWarn(msg string, args ...any)  { Logger.Warn(msg, args...) }
func logInfo(msg string, args ...any)  { Logger.Info(msg, args...) }
func logDebug(msg string, args ...any) { Logger.Debug(msg, args...) }
func logError(msg string, args ...any) { Logger.Error(msg, args...) }

// EnvPluginsDebug tees verbose plugin discovery logs to stderr when truthy
const EnvPluginsDebug = "ICEBATH_PLUGINS_DEBUG"

var pluginsDebugState struct {
	sync.Mutex
	enabled   bool
	installed bool
}

var debugStderr io.Writer = os.Stderr

func init() {
	pluginsDebugState.enabled = envEnabled(EnvPluginsDebug)
	installPluginDebugHandler(false)
}

// Whether plugin debug logging is on, which also attaches error detail to plugin failure warnings
func pluginsDebug() bool {
	pluginsDebugState.Lock()
	defer pluginsDebugState.Unlock()
	return pluginsDebugState.enabled
}

// Tees plugin logs to stderr at debug once per process when ICEBATH_PLUGINS_DEBUG is on, force rereads it
func installPluginDebugHandler(force bool) {
	pluginsDebugState.Lock()
	if force {
		pluginsDebugState.enabled = envEnabled(EnvPluginsDebug)
	}
	if !pluginsDebugState.enabled || pluginsDebugState.installed {
		pluginsDebugState.Unlock()
		return
	}
	Logger = slog.New(&debugTeeHandler{base: Logger.Handler(), out: debugStderr, mu: &sync.Mutex{}})
	pluginsDebugState.installed = true
	pluginsDebugState.Unlock()
	logDebug("ICEBATH_PLUGINS_DEBUG=1 — verbose plugin discovery logging enabled")
}

// Error detail a debug run attaches to a failure warning, the panic stack for a recovered plugin panic
func excInfo(err error) []any {
	if err == nil || !pluginsDebug() {
		return nil
	}
	detail := fmt.Sprintf("%T: %+v", err, err)
	if p, ok := err.(*PluginPanicError); ok && len(p.Stack) > 0 {
		detail += "\n" + string(p.Stack)
	}
	return []any{"exc_info", detail}
}

type debugTeeHandler struct {
	base  slog.Handler
	out   io.Writer
	mu    *sync.Mutex
	attrs []slog.Attr
}

func (h *debugTeeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelDebug || h.base.Enabled(ctx, level)
}

// Writes the record to stderr in the plugins debug format and forwards it to the base handler when it accepts the level
func (h *debugTeeHandler) Handle(ctx context.Context, r slog.Record) error {
	level := r.Level.String()
	if r.Level == slog.LevelWarn {
		level = "WARNING"
	}
	var line strings.Builder
	fmt.Fprintf(&line, "[plugins] %s %s", level, r.Message)
	for _, a := range h.attrs {
		fmt.Fprintf(&line, " %s=%v", a.Key, a.Value)
	}
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&line, " %s=%v", a.Key, a.Value)
		return true
	})
	line.WriteString("\n")
	h.mu.Lock()
	_, werr := io.WriteString(h.out, line.String())
	h.mu.Unlock()
	if h.base.Enabled(ctx, r.Level) {
		if err := h.base.Handle(ctx, r); err != nil {
			return err
		}
	}
	return werr
}

func (h *debugTeeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &debugTeeHandler{base: h.base.WithAttrs(attrs), out: h.out, mu: h.mu, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
}

func (h *debugTeeHandler) WithGroup(name string) slog.Handler {
	return &debugTeeHandler{base: h.base.WithGroup(name), out: h.out, mu: h.mu, attrs: h.attrs}
}
