package plugins

import "log/slog"

// Logger every plugin subsystem message goes through, replaceable by the host
var Logger = slog.Default()

func logWarn(msg string, args ...any)  { Logger.Warn(msg, args...) }
func logInfo(msg string, args ...any)  { Logger.Info(msg, args...) }
func logDebug(msg string, args ...any) { Logger.Debug(msg, args...) }
func logError(msg string, args ...any) { Logger.Error(msg, args...) }
