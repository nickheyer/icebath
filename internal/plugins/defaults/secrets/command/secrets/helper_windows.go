//go:build windows

package secrets

import (
	"context"
	"log/slog"
	"time"
)

// The helper is a POSIX sh -c command line, so Windows runs none
func helper(context.Context, string, time.Duration, map[string]string) (string, bool) {
	slog.Warn("[secrets:command] the 'command' provider is POSIX-only (needs /bin/sh); resolving no value on Windows")
	return "", false
}
