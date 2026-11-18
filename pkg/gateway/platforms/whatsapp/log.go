package whatsapp

import "log/slog"

// Logger named after this package, every record of the package carries it
func pkgLog() *slog.Logger { return slog.Default().With("logger", "gateway/platforms/whatsapp") }
