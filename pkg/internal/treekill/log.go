package treekill

import (
	"errors"
	"log/slog"
)

// Logger named after this package, every record of the package carries it
func pkgLog() *slog.Logger { return slog.Default().With("logger", "internal/treekill") }

// ErrProcessGone reports a recorded process or tree that exited or whose pid now names another process
var ErrProcessGone = errors.New("no such process")
