package batchrun

import "log/slog"

// Logger named after this package
func pkgLog() *slog.Logger { return slog.Default().With("logger", "batchrun") }
