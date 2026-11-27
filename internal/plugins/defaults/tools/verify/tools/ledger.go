package tools

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nickheyer/icebath/internal/config"
	verifyconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/verify/config"
	"github.com/nickheyer/icebath/internal/tools"
)

const (
	// File of the ledger in each profile's folder
	ledgerFile    = "verification_evidence.db"
	schemaVersion = 1
	// Evidence kept: by age, newest of each session and workspace, and in all besides what a workspace's state points at
	evidenceDays     = 30
	eventsPerSession = 100
	eventsKept       = 10_000
	// Changed paths a workspace's state keeps, the newest by name
	changedKept = 200
	// Checks of a session and workspace remembered whose own status the shell hid
	unattributedKept = 256
	// Canonical command of a run of icebath verify
	verifyCommand = "icebath verify"
)

var schema = []string{
	`CREATE TABLE IF NOT EXISTS meta (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS verification_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at TEXT NOT NULL,
		session_id TEXT NOT NULL,
		cwd TEXT NOT NULL,
		root TEXT NOT NULL,
		command TEXT NOT NULL,
		canonical_command TEXT NOT NULL,
		kind TEXT NOT NULL,
		scope TEXT NOT NULL,
		status TEXT NOT NULL,
		exit_code INTEGER NOT NULL,
		output_summary TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS verification_state (
		session_id TEXT NOT NULL,
		root TEXT NOT NULL,
		last_event_id INTEGER,
		last_edit_at TEXT,
		changed_paths_json TEXT NOT NULL DEFAULT '[]',
		PRIMARY KEY (session_id, root)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_verification_events_session_root ON verification_events(session_id, root, id DESC)`,
}

// Evidence each profile keeps that the workspaces its sessions change were checked: the checks run, and per session and workspace the
// newest check and the files changed since
type Ledger struct {
	mu  sync.Mutex
	dbs map[string]*sql.DB
	// Newest check of each session and workspace that did not count, the shell hiding its own status
	unattributed map[[2]string]unattributed
	seq          uint64
}

type unattributed struct {
	command, at string
	seq         uint64
}

func newLedger() *Ledger {
	return &Ledger{dbs: map[string]*sql.DB{}, unattributed: map[[2]string]unattributed{}}
}

// One check as the ledger keeps it
type event struct {
	ID        int64
	Created   string
	Session   string
	Cwd       string
	Root      string
	Command   string
	Canonical string
	Kind      string
	Scope     string
	Status    string
	Code      int
	Summary   string
}

// Now as the ledger writes times, which it compares as text
func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000+00:00") }

func now() string { return stamp(time.Now()) }

// Session a record is kept under, default for none
func sessionKey(id string) string {
	if id == "" {
		return "default"
	}
	return id
}

// Surfaces that are no messaging platform, on which verify on stop runs when it is auto
var nonMessaging = []string{"", "api_server", "cli", "codex", "desktop", "gateway", "kanban", "local", "msgraph_webhook", "tool", "tui", "webhook"}

// Whether the profile asks for the edits of its sessions on the surface to be checked, which is all the ledger records for: on, or auto
// anywhere but a messaging platform
func Enabled(cfg *config.Config, platform string) bool {
	if cfg == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(cfg.GetString(verifyconfig.OnStop))) {
	case verifyconfig.On:
		return true
	case verifyconfig.Auto:
		// The surface the process was started for, else the session's, and the source a gateway hands the commands of its sessions
		surface := cmp.Or(os.Getenv("ICEBATH_PLATFORM"), cmp.Or(platform, os.Getenv("ICEBATH_SESSION_PLATFORM")))
		for _, surface := range []string{surface, os.Getenv("ICEBATH_SESSION_SOURCE")} {
			if surface = strings.ToLower(strings.TrimSpace(surface)); surface != "" && !slices.Contains(nonMessaging, surface) {
				return false
			}
		}
		return true
	}
	return false
}

// Database of the profile's ledger, opened once with its schema
func (l *Ledger) db(ctx context.Context, cfg *config.Config) (*sql.DB, error) {
	path := filepath.Join(cfg.ProfileDir(cfg.Profile()), ledgerFile)
	if db, ok := l.dbs[path]; ok {
		return db, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	for _, ddl := range schema {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			db.Close()
			return nil, err
		}
	}
	if _, err := db.ExecContext(ctx, "INSERT OR REPLACE INTO meta(key, value) VALUES ('schema_version', ?)", schemaVersion); err != nil {
		db.Close()
		return nil, err
	}
	l.dbs[path] = db
	return db, nil
}

// Runs body in one transaction of the profile's ledger, one at a time in the process, l.mu held
func (l *Ledger) transaction(ctx context.Context, cfg *config.Config, body func(tx *sql.Tx) error) error {
	db, err := l.db(ctx, cfg)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := body(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Records a check and points the session's state of its workspace at it, its changes since cleared; a full passing icebath verify points
// every other session's state of the workspace at it too
func (l *Ledger) insert(ctx context.Context, cfg *config.Config, e event) (event, error) {
	e.Created = now()
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.unattributed, [2]string{e.Session, e.Root})
	err := l.transaction(ctx, cfg, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "INSERT INTO verification_events(created_at, session_id, cwd, root, command, canonical_command, kind, scope, "+
			"status, exit_code, output_summary) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", e.Created, e.Session, e.Cwd, e.Root, e.Command, e.Canonical,
			e.Kind, e.Scope, e.Status, e.Code, e.Summary)
		if err != nil {
			return err
		}
		if e.ID, err = res.LastInsertId(); err != nil {
			return errors.New("verification event insert did not return an id")
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO verification_state(session_id, root, last_event_id, last_edit_at, changed_paths_json) "+
			"VALUES (?, ?, ?, NULL, '[]') ON CONFLICT(session_id, root) DO UPDATE SET last_event_id = excluded.last_event_id, last_edit_at = NULL, "+
			"changed_paths_json = '[]'", e.Session, e.Root, e.ID); err != nil {
			return err
		}
		if e.Kind == "verify" && e.Status == "passed" && e.Scope == "full" {
			if _, err := tx.ExecContext(ctx, "UPDATE verification_state SET last_event_id = ?, last_edit_at = NULL, changed_paths_json = '[]' "+
				"WHERE root = ? AND session_id != ?", e.ID, e.Root, e.Session); err != nil {
				return err
			}
		}
		return prune(ctx, tx, e.Session, e.Root)
	})
	return e, err
}

// Keeps the ledger bounded: the newest checks of the session and workspace, states and checks within the age, and the newest in all,
// never one a state points at
func prune(ctx context.Context, tx *sql.Tx, session, root string) error {
	cutoff := stamp(time.Now().Add(-evidenceDays * 24 * time.Hour))
	for _, s := range []struct {
		query string
		args  []any
	}{
		{"DELETE FROM verification_events WHERE session_id = ? AND root = ? AND id NOT IN (SELECT id FROM verification_events WHERE " +
			"session_id = ? AND root = ? ORDER BY id DESC LIMIT ?)", []any{session, root, session, root, eventsPerSession}},
		{"DELETE FROM verification_state WHERE (last_edit_at IS NOT NULL AND last_edit_at < ?) OR (last_edit_at IS NULL AND last_event_id IN " +
			"(SELECT id FROM verification_events WHERE created_at < ?))", []any{cutoff, cutoff}},
		{"DELETE FROM verification_events WHERE created_at < ? AND id NOT IN (SELECT last_event_id FROM verification_state WHERE last_event_id " +
			"IS NOT NULL)", []any{cutoff}},
		{"DELETE FROM verification_events WHERE id NOT IN (SELECT id FROM verification_events ORDER BY id DESC LIMIT ?) AND id NOT IN " +
			"(SELECT last_event_id FROM verification_state WHERE last_event_id IS NOT NULL)", []any{eventsKept}},
	} {
		if _, err := tx.ExecContext(ctx, s.query, s.args...); err != nil {
			return err
		}
	}
	return nil
}

// Workspace a folder's records are kept under: its project's root, else the folder itself
func workspaceOf(dir string) (string, bool) {
	project, ok := tools.ProjectOf(dir)
	if !ok {
		return resolve(cmp.Or(dir, ".")), false
	}
	return project.Root, true
}

// Records a command a session ran in the foreground when it is a check of its project: one of the project's own checks whose status the
// shell proves, or a temporary script the agent wrote for a project with none; a check whose status the shell hid is remembered for the
// nudge to say so
func (l *Ledger) recordRun(ctx context.Context, cfg *config.Config, run tools.Run) (*event, error) {
	if run.Command == "" {
		return nil, nil
	}
	project, ok := tools.ProjectOf(run.Cwd)
	if !ok {
		return nil, nil
	}
	session := sessionKey(run.Session)
	var canonical, kind, scope string
	var status int
	if m := findCanonical(run.Command, project.Verify, run.Code, run.Pipeline, true); m != nil && m.known {
		canonical, status, kind, scope = m.canonical, m.status, kindOf(m.canonical), "full"
		if slices.ContainsFunc(m.trailing, target) {
			scope = "targeted"
		}
	} else {
		if len(project.Verify) == 0 && adHoc(run.Command, project.Root, run.Code) {
			canonical, kind, scope, status = adHocCommand, "ad_hoc", "targeted", run.Code
		} else {
			if findCanonical(run.Command, project.Verify, run.Code, nil, false) != nil {
				l.remember(session, project.Root, run.Command)
			}
			return nil, nil
		}
	}
	result := "failed"
	if status == 0 {
		result = "passed"
	}
	e, err := l.insert(ctx, cfg, event{Session: session, Cwd: resolve(cmp.Or(run.Cwd, ".")), Root: project.Root, Command: run.Command,
		Canonical: canonical, Kind: kind, Scope: scope, Status: result, Code: run.Code, Summary: summarize(run.Output)})
	return &e, err
}

// Remembers the newest check of the session and workspace that did not count, the oldest forgotten past the bound
func (l *Ledger) remember(session, root, command string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	l.unattributed[[2]string{session, root}] = unattributed{command, now(), l.seq}
	for len(l.unattributed) > unattributedKept {
		var oldest [2]string
		var seq uint64
		for key, run := range l.unattributed {
			if seq == 0 || run.seq < seq {
				oldest, seq = key, run.seq
			}
		}
		delete(l.unattributed, oldest)
	}
}

// Newest check of the session and workspace that did not count, unless evidence recorded since came after it; empty for none
func (l *Ledger) hidden(session, root, since string) string {
	l.mu.Lock()
	run, ok := l.unattributed[[2]string{sessionKey(session), root}]
	l.mu.Unlock()
	if !ok || since != "" && since >= run.at {
		return ""
	}
	return run.command
}

// Records a finished run of icebath verify in the folder as evidence for its workspace, a partial one as targeted
func (l *Ledger) Verify(ctx context.Context, cfg *config.Config, dir, session string, ok bool, scope, output string) error {
	if !Enabled(cfg, "") {
		return nil
	}
	root, _ := workspaceOf(dir)
	if scope != "full" && scope != "targeted" {
		scope = "full"
	}
	status, code := "failed", 1
	if ok {
		status, code = "passed", 0
	}
	_, err := l.insert(ctx, cfg, event{Session: sessionKey(session), Cwd: resolve(dir), Root: root, Command: verifyCommand, Canonical: verifyCommand,
		Kind: "verify", Scope: scope, Status: status, Code: code, Summary: summarize(output)})
	return err
}

// Marks the evidence of the session's workspace stale after edits landed, the paths added to those changed since its newest check
func (l *Ledger) edited(ctx context.Context, cfg *config.Config, session, dir string, paths []string) error {
	if _, ok := tools.ProjectOf(dir); !ok {
		return nil
	}
	root, _ := workspaceOf(dir)
	session = sessionKey(session)
	at := now()
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.transaction(ctx, cfg, func(tx *sql.Tx) error {
		var raw sql.NullString
		err := tx.QueryRowContext(ctx, "SELECT changed_paths_json FROM verification_state WHERE session_id = ? AND root = ?", session, root).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		changed := decodePaths(raw.String)
		for _, path := range paths {
			if path != "" && !slices.Contains(changed, path) {
				changed = append(changed, path)
			}
		}
		slices.Sort(changed)
		changed = changed[max(0, len(changed)-changedKept):]
		encoded, err := json.Marshal(changed)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO verification_state(session_id, root, last_event_id, last_edit_at, changed_paths_json) VALUES "+
			"(?, ?, NULL, ?, ?) ON CONFLICT(session_id, root) DO UPDATE SET last_edit_at = excluded.last_edit_at, changed_paths_json = "+
			"excluded.changed_paths_json", session, root, at, string(encoded))
		return err
	})
}

// Changed paths a state keeps, none when they don't read as a list of text
func decodePaths(raw string) []string {
	var paths []string
	if raw == "" || json.Unmarshal([]byte(raw), &paths) != nil {
		return []string{}
	}
	return paths
}

// What is known of the session's workspace: passed or failed by its newest check, stale when files changed after it, unverified with no
// check; and the workspace, the newest check and the files changed since
type status struct {
	State   string
	Root    string
	Session string
	Changed []string
	Event   *event
}

const eventColumns = "id, created_at, session_id, cwd, root, command, canonical_command, kind, scope, status, exit_code, output_summary"

func scanEvent(row *sql.Row) (*event, error) {
	var e event
	err := row.Scan(&e.ID, &e.Created, &e.Session, &e.Cwd, &e.Root, &e.Command, &e.Canonical, &e.Kind, &e.Scope, &e.Status, &e.Code, &e.Summary)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// Newest full passing icebath verify of the workspace, at or after the time when one is given
func newestVerify(ctx context.Context, tx *sql.Tx, root, since string) (*event, error) {
	query := "SELECT " + eventColumns + " FROM verification_events WHERE root = ? AND kind = 'verify' AND status = 'passed' AND scope = 'full'"
	args := []any{root}
	if since != "" {
		query += " AND created_at >= ?"
		args = append(args, since)
	}
	return scanEvent(tx.QueryRowContext(ctx, query+" ORDER BY id DESC LIMIT 1", args...))
}

// Best known state of the session's workspace: its newest check, stale once files changed after it unless a full icebath verify of the
// workspace passed since
func (l *Ledger) status(ctx context.Context, cfg *config.Config, session, dir string) (status, error) {
	root, _ := workspaceOf(dir)
	out := status{State: "unverified", Root: root, Session: sessionKey(session), Changed: []string{}}
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.transaction(ctx, cfg, func(tx *sql.Tx) error {
		var last sql.NullInt64
		var edit, changed sql.NullString
		err := tx.QueryRowContext(ctx, "SELECT last_event_id, last_edit_at, changed_paths_json FROM verification_state WHERE session_id = ? AND root = ?",
			out.Session, root).Scan(&last, &edit, &changed)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out.Changed = decodePaths(changed.String)
		var e *event
		if last.Valid {
			if e, err = scanEvent(tx.QueryRowContext(ctx, "SELECT "+eventColumns+" FROM verification_events WHERE id = ?", last.Int64)); err != nil {
				return err
			}
		}
		stale := e != nil && edit.String != "" && edit.String > e.Created
		if e == nil || stale {
			shared, err := newestVerify(ctx, tx, root, edit.String)
			if err != nil {
				return err
			}
			if shared != nil {
				e, stale = shared, false
			}
		}
		if e == nil {
			return nil
		}
		out.Event, out.State = e, e.Status
		if stale {
			out.State = "stale"
		}
		return nil
	})
	return out, err
}

// Closes the databases of every profile, as icebath exits
func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var errs []error
	for path, db := range l.dbs {
		errs = append(errs, db.Close())
		delete(l.dbs, path)
	}
	return errors.Join(errs...)
}
