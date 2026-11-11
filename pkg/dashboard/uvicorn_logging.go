package dashboard

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/mattn/go-isatty"

	"github.com/nickheyer/icebath/pkg/logging"
)

const uvicornTraceLevel = 5

// ANSI colour codes uvicorn styles level names with
var uvicornLevelColors = map[int]int{uvicornTraceLevel: 34, logging.LevelDebug: 36, logging.LevelInfo: 32,
	logging.LevelWarning: 33, logging.LevelError: 31, logging.LevelCritical: 91}

// Console handler of the uvicorn loggers, the level prefix formatter over a stream
type uvicornStreamHandler struct {
	mu        sync.Mutex
	toStdout  bool
	useColors bool
}

// Writes the level prefixed record line to the stream
func (h *uvicornStreamHandler) Handle(_ context.Context, rec *logging.Record) bool {
	levelname := rec.Levelname
	separator := strings.Repeat(" ", max(0, 8-len([]rune(levelname))))
	if h.useColors {
		if code, ok := uvicornLevelColors[rec.Levelno]; ok {
			levelname = fmt.Sprintf("\033[%dm%s\033[0m", code, levelname)
		}
	}
	var w io.Writer = os.Stderr
	if h.toStdout {
		w = os.Stdout
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(w, "%s:%s %s\n", levelname, separator, rec.Msg)
	return true
}

func (h *uvicornStreamHandler) Level() int   { return 0 }
func (h *uvicornStreamHandler) Close() error { return nil }

// Routes uvicorn to stderr and uvicorn.access to stdout without propagation, handlers added only once per process
func installUvicornConsoleHandlers() {
	useColors := isatty.IsTerminal(os.Stdout.Fd())
	for _, target := range []struct {
		name     string
		toStdout bool
	}{{"uvicorn", false}, {"uvicorn.access", true}} {
		logger := logging.GetLogger(target.name)
		logger.SetPropagate(false)
		if len(logger.Handlers()) == 0 {
			logger.AddHandler(&uvicornStreamHandler{toStdout: target.toStdout, useColors: useColors})
		}
	}
	logging.GetLogger("uvicorn").SetLevel(logging.LevelInfo)
}

// Level the uvicorn config applies to its error, access and asgi loggers
func configureUvicornLogLevel(level int) {
	for _, name := range []string{"uvicorn.error", "uvicorn.access", "uvicorn.asgi"} {
		logging.GetLogger(name).SetLevel(level)
	}
}
