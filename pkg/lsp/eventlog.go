package lsp

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Logger name the documented grep recipe keys on
const eventLogger = "icebath.lint.lsp"

// Keys per announcement bucket before it resets
var announceCap = 512

// Dedup key of one announcement
type announceKey struct{ a, b string }

var (
	announceMu           sync.Mutex
	announcedActive      = map[announceKey]bool{}
	announcedUnavailable = map[announceKey]bool{}
	announcedNoRoot      = map[announceKey]bool{}
	announcedSkipped     = map[announceKey]bool{}
	announcedExcluded    = map[announceKey]bool{}
	announcedUntrusted   = map[announceKey]bool{}
)

// Path relative to the cwd when inside it, else unchanged
func shortPath(filePath string) string {
	if filePath == "" {
		return filePath
	}
	wd, err := os.Getwd()
	if err != nil {
		return filePath
	}
	abs, err := filepath.Abs(filePath)
	if err != nil {
		return filePath
	}
	rel, err := filepath.Rel(wd, abs)
	if err != nil {
		return filePath
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filePath
	}
	return rel
}

func emit(rt *Runtime, serverID string, level slog.Level, message string) {
	rt.logf(level, eventLogger, "lsp[%s] %s", serverID, message)
}

// Logs first at level the first time a key is seen and repeat at debug after
func emitOnce(rt *Runtime, bucket *map[announceKey]bool, key announceKey, serverID string, level slog.Level, first, repeat string) {
	announceMu.Lock()
	isFirst := !(*bucket)[key]
	if isFirst && len(*bucket) >= announceCap {
		*bucket = map[announceKey]bool{}
	}
	(*bucket)[key] = true
	announceMu.Unlock()
	if isFirst {
		emit(rt, serverID, level, first)
	} else {
		emit(rt, serverID, slog.LevelDebug, repeat)
	}
}

func logClean(rt *Runtime, serverID, filePath string) {
	emit(rt, serverID, slog.LevelDebug, "clean ("+shortPath(filePath)+")")
}

func logDisabled(rt *Runtime, serverID, filePath, reason string) {
	emit(rt, serverID, slog.LevelDebug, "skipped: "+reason+" ("+shortPath(filePath)+")")
}

func logActive(rt *Runtime, serverID, workspaceRoot string) {
	emitOnce(rt, &announcedActive, announceKey{serverID, workspaceRoot}, serverID, slog.LevelInfo,
		"active for "+workspaceRoot, "reused client for "+workspaceRoot)
}

func logDiagnostics(rt *Runtime, serverID, filePath string, count int) {
	emit(rt, serverID, slog.LevelInfo, fmt.Sprintf("%d diags (%s)", count, shortPath(filePath)))
}

func logNoProjectRoot(rt *Runtime, serverID, filePath string) {
	msg := "no project root for " + shortPath(filePath)
	emitOnce(rt, &announcedNoRoot, announceKey{serverID, filePath}, serverID, slog.LevelInfo, msg, msg)
}

func logServerUnavailable(rt *Runtime, serverID, binaryOrPkg string) {
	emitOnce(rt, &announcedUnavailable, announceKey{serverID, binaryOrPkg}, serverID, slog.LevelWarn,
		"server unavailable: "+binaryOrPkg+" not found (install via `icebath lsp install <id>` or set lsp.servers.<id>.command)",
		"server still unavailable: "+binaryOrPkg)
}

func logTimeout(rt *Runtime, serverID, filePath, kind string) {
	emit(rt, serverID, slog.LevelWarn, kind+" timed out for "+shortPath(filePath))
}

func logServerError(rt *Runtime, serverID, filePath string, err error) {
	emit(rt, serverID, slog.LevelWarn, "unexpected error for "+shortPath(filePath)+": "+errTypeName(err)+": "+errText(err))
}

func logSpawnFailed(rt *Runtime, serverID, workspaceRoot string, err error) {
	emit(rt, serverID, slog.LevelWarn, "spawn/initialize failed for "+workspaceRoot+": "+errTypeName(err)+": "+errText(err))
}

// Logs a request skipped on a broken root, naming when the pair is retried
func logSkippedBroken(rt *Runtime, serverID, workspaceRoot, filePath string, retryIn float64, hasRetry bool) {
	until := "no diagnostics for this root until restart"
	if hasRetry {
		until = fmt.Sprintf("retry in %.0fs", retryIn)
	}
	emitOnce(rt, &announcedSkipped, announceKey{serverID, workspaceRoot}, serverID, slog.LevelInfo,
		"skipping "+shortPath(filePath)+": "+workspaceRoot+" marked broken after an earlier failure ("+until+")",
		"skipping "+shortPath(filePath)+": "+workspaceRoot+" marked broken")
}

func logRootExcluded(rt *Runtime, serverID, workspaceRoot, filePath string, invalid bool) {
	why := "matches lsp.exclude_roots"
	if invalid {
		why = "lsp.exclude_roots is not a list (fix the key)"
	}
	emitOnce(rt, &announcedExcluded, announceKey{serverID, workspaceRoot}, serverID, slog.LevelInfo,
		"skipping "+shortPath(filePath)+": "+workspaceRoot+" "+why,
		"skipping "+shortPath(filePath)+": "+workspaceRoot+" excluded")
}

func logUntrustedSkipped(rt *Runtime, serverID, workspaceRoot, filePath string) {
	emitOnce(rt, &announcedUntrusted, announceKey{serverID, workspaceRoot}, serverID, slog.LevelInfo,
		"skipped: untrusted workspace "+workspaceRoot+" ("+shortPath(filePath)+"); "+
			"add it to lsp.trusted_workspaces to run this server there",
		"skipped: untrusted workspace "+workspaceRoot)
}

// Server id and root of one client named in a sweep line
type clientKey struct{ serverID, root string }

// Forgets active announcements so a respawn announces at info again, and renders the summary
func forgetActive(keys []clientKey) string {
	announceMu.Lock()
	parts := make([]string, len(keys))
	for i, k := range keys {
		delete(announcedActive, announceKey{k.serverID, k.root})
		parts[i] = k.serverID + " (" + k.root + ")"
	}
	announceMu.Unlock()
	return strings.Join(parts, ", ")
}

func logReaped(rt *Runtime, keys []clientKey, idleTimeout float64) {
	summary := forgetActive(keys)
	emit(rt, "reaper", slog.LevelInfo, fmt.Sprintf("reaped %d idle client(s) after %.0fs: %s", len(keys), idleTimeout, summary))
}

func logReleased(rt *Runtime, keys []clientKey, reason string) {
	summary := forgetActive(keys)
	emit(rt, "reaper", slog.LevelInfo, fmt.Sprintf("released %d client(s) (%s): %s", len(keys), reason, summary))
}

// Clears every announcement dedup bucket for tests
func resetAnnounceCaches() {
	announceMu.Lock()
	for _, b := range []*map[announceKey]bool{&announcedActive, &announcedUnavailable, &announcedNoRoot,
		&announcedSkipped, &announcedExcluded, &announcedUntrusted} {
		*b = map[announceKey]bool{}
	}
	announceMu.Unlock()
}
