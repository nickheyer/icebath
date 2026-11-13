package logging

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Line shape a sink renders its records in
type Layout int

// Record line shapes the log files and consoles use
const (
	// Timestamp with milliseconds, level, session tag, source name and message, the log file shape
	FileLayout Layout = iota
	// Clock time, source name, level with session tag and message joined by dashes, the verbose console shape
	VerboseLayout
	// Timestamp with milliseconds, level, source name and message, the gateway console shape
	GatewayConsoleLayout
	// Timestamp in whole seconds, bracketed level, source name and message, the editor protocol console shape
	ACPConsoleLayout
	// Message alone with its attributes
	MessageLayout
)

// Line of rec in the layout before redaction
func (l Layout) line(rec *record) string {
	t := rec.time.Local()
	switch l {
	case VerboseLayout:
		return t.Format("15:04:05") + " - " + rec.name + " - " + LevelName(rec.level) + sessionTag(rec.session) + " - " + rec.message
	case GatewayConsoleLayout:
		return Timestamp(t) + " " + LevelName(rec.level) + " " + rec.name + ": " + rec.message
	case ACPConsoleLayout:
		return t.Format("2006-01-02 15:04:05") + " [" + LevelName(rec.level) + "] " + rec.name + ": " + rec.message
	case MessageLayout:
		return rec.message
	}
	return Timestamp(t) + " " + LevelName(rec.level) + sessionTag(rec.session) + " " + rec.name + ": " + rec.message
}

// Local time t in the shape every log line opens with, milliseconds after a comma
func Timestamp(t time.Time) string {
	t = t.Local()
	return t.Format("2006-01-02 15:04:05") + fmt.Sprintf(",%03d", t.Nanosecond()/int(time.Millisecond))
}

// Session tag a log line carries after the level, a space and the bracketed id, empty without a session
func sessionTag(id string) string {
	if id == "" {
		return ""
	}
	return " [" + id + "]"
}

// Upper case word a log line names level by, the one the line filters match
func LevelName(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "DEBUG"
	case l < slog.LevelWarn:
		return "INFO"
	case l < slog.LevelError:
		return "WARNING"
	case l < LevelCritical:
		return "ERROR"
	}
	return "CRITICAL"
}

// Level a configured level name selects, false for a name no level answers to
func parseLevel(name string) (slog.Level, bool) {
	switch strings.ToUpper(name) {
	case "DEBUG":
		return slog.LevelDebug, true
	case "INFO":
		return slog.LevelInfo, true
	case "WARNING", "WARN":
		return slog.LevelWarn, true
	case "ERROR":
		return slog.LevelError, true
	case "CRITICAL", "FATAL":
		return LevelCritical, true
	case "NOTSET":
		return levelNotSet, true
	}
	return 0, false
}
