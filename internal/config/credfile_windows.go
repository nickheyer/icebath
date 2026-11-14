//go:build windows

package config

import "log/slog"

// Reports false, since Windows access lists carry no group and world read bits to judge
func WarnBroadlyReadable(string, string, *slog.Logger) bool { return false }
