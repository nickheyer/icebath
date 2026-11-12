package app

import (
	"context"
	"encoding/json"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/state"
)

// Multiplexer and terminal emulator identity env vars checked in order when no tty path is available
var terminalEnvVars = []string{"ZELLIJ_PANE_ID", "TMUX_PANE", "KITTY_WINDOW_ID", "WEZTERM_PANE", "TERM_SESSION_ID", "WT_SESSION"}

// Breadcrumbs older than this are pruned on each write
const staleBreadcrumbAfter = 30 * 24 * 60 * 60

var breadcrumbSanitizeRe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// Reads the tty device path of a file descriptor, swapped by tests the way pytest patches os.ttyname
var ttyName = ttyNameOS

// Time source of the breadcrumb timestamps, swapped by tests
var breadcrumbNow = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func breadcrumbsDir(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "terminal-sessions")
}

// Makes an id safe to use as a filename, /dev/pts/3 becoming dev-pts-3
func sanitizeTerminalID(raw string) string {
	out := breadcrumbSanitizeRe.ReplaceAllString(strings.Trim(strings.TrimSpace(raw), "/"), "-")
	if len(out) > 120 {
		out = out[:120]
	}
	return out
}

// Stable identity for this terminal, the tty device path or the first present multiplexer env var, empty when neither exists
func GetTerminalID() string {
	for _, f := range []*os.File{os.Stdin, os.Stdout} {
		name, err := ttyName(int(f.Fd()))
		if err != nil || name == "" {
			continue
		}
		return "tty-" + sanitizeTerminalID(name)
	}
	for _, v := range terminalEnvVars {
		if val := os.Getenv(v); val != "" {
			return strings.ToLower(v) + "-" + sanitizeTerminalID(val)
		}
	}
	return ""
}

// Config gate session.terminal_continue, default true
func breadcrumbsEnabled(ctx context.Context) bool {
	cfg, err := config.Load(home.Dir(ctx), nil)
	if err != nil {
		return true
	}
	return cfg.Bool("session.terminal_continue", true)
}

// Records that this terminal's live session is sessionID, never raising
func WriteBreadcrumb(ctx context.Context, sessionID, cwd string) {
	if sessionID == "" || !breadcrumbsEnabled(ctx) {
		return
	}
	terminalID := GetTerminalID()
	if terminalID == "" {
		return
	}
	dir := breadcrumbsDir(ctx)
	if _, err := home.Mkdir(ctx, dir); err != nil {
		return
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	now := breadcrumbNow()
	payload := pyjson.ObjectOf("session_id", sessionID, "cwd", cwd, "ts", now)
	if err := config.AtomicJSONWriteWith(ctx, filepath.Join(dir, terminalID), payload); err != nil {
		return
	}
	pyfs.UnlinkFilesOlderThan(dir, "*", staleBreadcrumbAfter, &now)
}

// This terminal's breadcrumb payload, nil when missing, corrupt or stale
func ReadBreadcrumb(ctx context.Context) map[string]any {
	terminalID := GetTerminalID()
	if terminalID == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(breadcrumbsDir(ctx), terminalID))
	if err != nil {
		return nil
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(string(raw), "\ufeff")), &data); err != nil || data == nil {
		return nil
	}
	if strings.TrimSpace(config.PyStr(data["session_id"])) == "" || data["session_id"] == nil {
		return nil
	}
	if ts, ok := data["ts"].(float64); ok && breadcrumbNow()-ts > staleBreadcrumbAfter {
		return nil
	}
	return data
}

// Resolves a bare -c for this terminal to its live session id, empty to fall back to the latest session
func ResolveBreadcrumbSession(ctx context.Context) string {
	if !breadcrumbsEnabled(ctx) {
		return ""
	}
	crumb := ReadBreadcrumb(ctx)
	if crumb == nil {
		return ""
	}
	sessionID := strings.TrimSpace(config.PyStr(crumb["session_id"]))
	if sessionID == "" {
		return ""
	}
	db, err := state.NewSessionDB(ctx, home.StatePath(home.Dir(ctx)), true)
	if err != nil {
		return ""
	}
	defer db.Close()
	session, err := db.GetSession(ctx, sessionID)
	if err != nil || session == nil {
		return ""
	}
	tip, err := db.GetCompressionTip(ctx, sessionID)
	if err != nil || tip == "" {
		return sessionID
	}
	return tip
}
