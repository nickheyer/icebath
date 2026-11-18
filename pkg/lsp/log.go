package lsp

import "log/slog"

// Logger name every record of this package carries
const logName = "lsp"

// Logger named after this package for code without a runtime
func pkgLog() *slog.Logger { return slog.Default().With("logger", logName) }
