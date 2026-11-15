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
func (h *logHandler) WithAttrs([]slog.Attr) slog.Handler           { return h }
func (h *logHandler) WithGroup(string) slog.Handler                { return h }
func (h *logHandler) Handle(_ context.Context, r slog.Record) error {
	names := map[slog.Level]string{slog.LevelDebug: "DEBUG", slog.LevelInfo: "INFO", slog.LevelWarn: "WARNING", slog.LevelError: "ERROR"}
	msg := r.Message
	r.Attrs(func(a slog.Attr) bool { msg += " " + a.Key + "=" + a.Value.String(); return true })
	logMu.Lock()
	defer logMu.Unlock()
	_, err := io.WriteString(h.out, names[r.Level]+":"+h.name+":"+msg+"\n")
	return err
}

// Logger writing as name to stderr under the shared threshold
func newLogger(name string) *slog.Logger { return slog.New(&logHandler{name: name, out: os.Stderr}) }

var (
	bridgeLog = newLogger("icebath.mcp_serve")
	toolsLog  = newLogger("icebath.mcp_serve.tools")
)

// Sets the shared threshold to DEBUG when verbose and WARNING otherwise and routes the process default logger through it
func configureLogging(verbose bool) {
	if verbose {
		logLevel.Set(slog.LevelDebug)
	} else {
		logLevel.Set(slog.LevelWarn)
	}
	slog.SetDefault(newLogger("root"))
}
