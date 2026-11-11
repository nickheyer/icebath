package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"
)

// Logs a swallowed error on gateway.run at level, the error filling the last format argument
func logSuppressed(level slog.Level, err error, format string, args ...any) {
	if err == nil {
		return
	}
	runLog().Log(context.Background(), level, fmt.Sprintf(format, append(args, err)...))
}

// Logs a swallowed error on gateway.run at level with the error attached as its traceback
func logSuppressedExc(level slog.Level, err error, format string, args ...any) {
	if err == nil {
		return
	}
	runLog().Log(context.Background(), level, fmt.Sprintf(format, args...), "error", err)
}
