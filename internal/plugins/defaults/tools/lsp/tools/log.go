package tools

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	lspconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/lsp/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
)

// Keys each kind of announcement keeps before it starts over
const announceCap = 512

// Kinds of announcement said once at their level, at debug after
const (
	announceActive = iota
	announceUnavailable
	announceNoRoot
	announceSkipped
	announceExcluded
	announceUntrusted
	announceKinds
)

// What the log was told once already, by kind and by server and root or file
type announcer struct {
	mu     sync.Mutex
	said   [announceKinds]map[[2]string]bool
	warned map[string]bool
}

func (m *Manager) logf(level slog.Level, format string, args ...any) {
	if slog.Default().Enabled(context.Background(), level) {
		slog.Log(context.Background(), level, fmt.Sprintf(format, args...))
	}
}

// Logs a warning the first time its key comes in this process
func (m *Manager) warnOnce(key, message string) {
	m.announce.mu.Lock()
	seen := m.announce.warned[key]
	m.announce.warned[key] = true
	m.announce.mu.Unlock()
	if !seen {
		m.logf(slog.LevelWarn, "%s", message)
	}
}

// Path from the working folder when it lies in it, else as it is
func short(path string) string {
	wd, err := os.Getwd()
	if err != nil || path == "" {
		return path
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(wd, abs)
	if err != nil || !filepath.IsLocal(rel) {
		return path
	}
	return rel
}

func (m *Manager) emit(server string, level slog.Level, message string) {
	m.logf(level, "lsp[%s] %s", server, message)
}

// Says first at the level the first time the key comes, repeat at debug after
func (m *Manager) emitOnce(kind int, key [2]string, server string, level slog.Level, first, repeat string) {
	m.announce.mu.Lock()
	bucket := m.announce.said[kind]
	isFirst := !bucket[key]
	if isFirst && len(bucket) >= announceCap {
		bucket = map[[2]string]bool{}
		m.announce.said[kind] = bucket
	}
	bucket[key] = true
	m.announce.mu.Unlock()
	if isFirst {
		m.emit(server, level, first)
	} else {
		m.emit(server, slog.LevelDebug, repeat)
	}
}

func (m *Manager) logClean(server, path string) {
	m.emit(server, slog.LevelDebug, "clean ("+short(path)+")")
}

func (m *Manager) logDisabled(server, path, reason string) {
	m.emit(server, slog.LevelDebug, "skipped: "+reason+" ("+short(path)+")")
}

func (m *Manager) logActive(server, root string) {
	m.emitOnce(announceActive, [2]string{server, root}, server, slog.LevelInfo, "active for "+root, "reused client for "+root)
}

func (m *Manager) logDiagnostics(server, path string, count int) {
	m.emit(server, slog.LevelInfo, fmt.Sprintf("%d diags (%s)", count, short(path)))
}

func (m *Manager) logNoRoot(server, path string) {
	message := "no project root for " + short(path)
	m.emitOnce(announceNoRoot, [2]string{server, path}, server, slog.LevelInfo, message, message)
}

func (m *Manager) logUnavailable(server string) {
	m.emitOnce(announceUnavailable, [2]string{server, server}, server, slog.LevelWarn, "server unavailable: "+server+
		" not found (install via `icebath lsp install <id>` or set "+lspconfig.Servers+".<id>.command)", "server still unavailable: "+server)
}

func (m *Manager) logTimeout(server, path, kind string) {
	m.emit(server, slog.LevelWarn, kind+" timed out for "+short(path))
}

func (m *Manager) logSpawnFailed(server, root string, err error) {
	m.emit(server, slog.LevelWarn, "spawn/initialize failed for "+root+": "+err.Error())
}

// A file skipped on a root marked broken, with when the root is tried again
func (m *Manager) logSkippedBroken(server, root, path string, retryIn float64, retries bool) {
	until := "no diagnostics for this root until restart"
	if retries {
		until = fmt.Sprintf("retry in %.0fs", retryIn)
	}
	m.emitOnce(announceSkipped, [2]string{server, root}, server, slog.LevelInfo, "skipping "+short(path)+": "+root+
		" marked broken after an earlier failure ("+until+")", "skipping "+short(path)+": "+root+" marked broken")
}

func (m *Manager) logExcluded(server, root, path string, invalid bool) {
	why := "matches " + lspconfig.ExcludeRoots
	if invalid {
		why = lspconfig.ExcludeRoots + " is not a list (fix the key)"
	}
	m.emitOnce(announceExcluded, [2]string{server, root}, server, slog.LevelInfo, "skipping "+short(path)+": "+root+" "+why,
		"skipping "+short(path)+": "+root+" excluded")
}

func (m *Manager) logUntrusted(server, root, path string) {
	m.emitOnce(announceUntrusted, [2]string{server, root}, server, slog.LevelInfo, "skipped: untrusted workspace "+root+" ("+short(path)+"); add it to "+
		toolsconfig.TrustedWorkspaces+" to run this server there", "skipped: untrusted workspace "+root)
}

// Forgets the active announcements of clients that are gone, so a respawn says so again, and names them
func (m *Manager) forget(clients []*client) string {
	m.announce.mu.Lock()
	defer m.announce.mu.Unlock()
	parts := make([]string, len(clients))
	for i, c := range clients {
		delete(m.announce.said[announceActive], [2]string{c.server, c.root})
		parts[i] = c.server + " (" + c.root + ")"
	}
	return strings.Join(parts, ", ")
}

func (m *Manager) logReaped(clients []*client, idle float64) {
	m.emit("reaper", slog.LevelInfo, fmt.Sprintf("reaped %d idle client(s) after %.0fs: %s", len(clients), idle, m.forget(clients)))
}

func (m *Manager) logReleased(clients []*client, reason string) {
	m.emit("reaper", slog.LevelInfo, fmt.Sprintf("released %d client(s) (%s): %s", len(clients), reason, m.forget(clients)))
}
