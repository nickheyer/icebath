package skills

import "log/slog"

// Logger name every record of this package carries
const logName = "skills"

// Logger named after this package
func pkgLog() *slog.Logger { return slog.Default().With("logger", logName) }
