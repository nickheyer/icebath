package cron

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Notepad size caps, package vars so tests can shrink them
var (
	NotepadMaxValueBytes    = 16 * 1024
	NotepadMaxKeyChars      = 128
	NotepadMaxJobTotalBytes = 64 * 1024
)

var notepadMu sync.Mutex

// NoteRejectedError is a notepad write refused for an empty field or a size cap
type NoteRejectedError struct{ Msg string }

func (e *NoteRejectedError) Error() string { return e.Msg }

// Notepad is one profile home's per job durable key value store next to the executions ledger
type Notepad struct {
	s    *Store
	path string
}

// Notepad of the store home, the store notepad file override when set
func openNotepad(s *Store) *Notepad {
	path := s.notepadFile
	if path == "" {
		path = filepath.Join(s.cronDir, "notepad.db")
	}
	return &Notepad{s: s, path: path}
}

// Context resolving the store home so the journal mode reads the same profile config the notepad lives in
func (n *Notepad) homeContext() context.Context {
	return home.WithOverride(context.Background(), n.s.home)
}

// Opens the notepad database with its schema initialised and the cron dir secured
func (n *Notepad) connect(ctx context.Context) (state.SQLiteConn, error) {
	if err := ensureCronDir(filepath.Dir(n.path)); err != nil {
		return nil, err
	}
	return state.OpenDB(ctx, n.path, "cron/notepad.db", state.WithInitialize(initializeNotepadSchema))
}

// Creates the cron_notepad table keyed by job and key
func initializeNotepadSchema(ctx context.Context, conn state.SQLiteConn) error {
	_, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS cron_notepad (
             job_id TEXT NOT NULL,
             key TEXT NOT NULL,
             value TEXT NOT NULL,
             updated_at TEXT NOT NULL,
             PRIMARY KEY (job_id, key)
           )`)
	return err
}

// Runs body in one notepad transaction under the process wide notepad lock
func (n *Notepad) transaction(ctx context.Context, body func(conn state.SQLiteConn) error) error {
	notepadMu.Lock()
	defer notepadMu.Unlock()
	conn, err := n.connect(ctx)
	if err != nil {
		return err
	}
	return state.Transaction(ctx, conn, false, body)
}

// Strict utf-8 encode error of the first bound text holding a lone surrogate, as sqlite3 binding raises
func bindableText(values ...string) error {
	for _, v := range values {
		if err := pystr.StrictUTF8EncodeError(v); err != nil {
			return err
		}
	}
	return nil
}

// Refuses an empty job id or key, an over long key and an over cap or unencodable value
func validateNote(jobID, key, value string) error {
	if jobID == "" {
		return &NoteRejectedError{"job_id must be non-empty"}
	}
	if key == "" {
		return &NoteRejectedError{"key must be non-empty"}
	}
	if pystr.Len(key) > NotepadMaxKeyChars {
		return &NoteRejectedError{fmt.Sprintf("key too long (max %d characters)", NotepadMaxKeyChars)}
	}
	if err := pystr.StrictUTF8EncodeError(value); err != nil {
		return err
	}
	if len(value) > NotepadMaxValueBytes {
		return &NoteRejectedError{fmt.Sprintf("value too large (max %d bytes per key)", NotepadMaxValueBytes)}
	}
	return nil
}

// Upserts one key and returns the stored entry, refusing a write past a size cap
func (n *Notepad) Set(jobID, key, value string) (*pyjson.Object, error) {
	if err := validateNote(jobID, key, value); err != nil {
		return nil, err
	}
	now := FormatISO(n.s.clk.now())
	ctx := n.homeContext()
	err := n.transaction(ctx, func(conn state.SQLiteConn) error {
		if err := bindableText(jobID, key); err != nil {
			return err
		}
		var other int64
		if err := conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(LENGTH(CAST(key AS BLOB))
                 + LENGTH(CAST(value AS BLOB))), 0)
               FROM cron_notepad WHERE job_id=? AND key<>?`, jobID, key).Scan(&other); err != nil {
			return err
		}
		if other+int64(len(key)+len(value)) > int64(NotepadMaxJobTotalBytes) {
			return &NoteRejectedError{fmt.Sprintf("notepad full: job '%s' would exceed %d bytes total; delete unused keys first", jobID, NotepadMaxJobTotalBytes)}
		}
		_, err := conn.ExecContext(ctx, `INSERT INTO cron_notepad (job_id, key, value, updated_at)
               VALUES (?, ?, ?, ?)
               ON CONFLICT(job_id, key)
               DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, jobID, key, value, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	entry := pyjson.NewObject()
	entry.Set("job_id", jobID)
	entry.Set("key", key)
	entry.Set("value", value)
	entry.Set("updated_at", now)
	return entry, nil
}

// Stored value of one key, nil when the key is absent
func (n *Notepad) Get(jobID, key string) (*string, error) {
	var value *string
	ctx := n.homeContext()
	err := n.transaction(ctx, func(conn state.SQLiteConn) error {
		if err := bindableText(jobID, key); err != nil {
			return err
		}
		var v string
		err := conn.QueryRowContext(ctx, "SELECT value FROM cron_notepad WHERE job_id=? AND key=?", jobID, key).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err == nil {
			value = &v
		}
		return err
	})
	return value, err
}

// Deletes one key and reports whether a row went away
func (n *Notepad) Delete(jobID, key string) (bool, error) {
	var removed int64
	ctx := n.homeContext()
	err := n.transaction(ctx, func(conn state.SQLiteConn) error {
		if err := bindableText(jobID, key); err != nil {
			return err
		}
		res, err := conn.ExecContext(ctx, "DELETE FROM cron_notepad WHERE job_id=? AND key=?", jobID, key)
		if err != nil {
			return err
		}
		removed, err = res.RowsAffected()
		return err
	})
	return removed > 0, err
}

// Every entry of one job sorted by key, each in column order
func (n *Notepad) List(jobID string) ([]*pyjson.Object, error) {
	notes := []*pyjson.Object{}
	ctx := n.homeContext()
	err := n.transaction(ctx, func(conn state.SQLiteConn) error {
		if err := bindableText(jobID); err != nil {
			return err
		}
		rows, err := conn.QueryContext(ctx, "SELECT job_id, key, value, updated_at FROM cron_notepad WHERE job_id=? ORDER BY key", jobID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var job, key, value, updated string
			if err := rows.Scan(&job, &key, &value, &updated); err != nil {
				return err
			}
			entry := pyjson.NewObject()
			entry.Set("job_id", job)
			entry.Set("key", key)
			entry.Set("value", value)
			entry.Set("updated_at", updated)
			notes = append(notes, entry)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return notes, nil
}

// Deletes every key of one job and returns the row count, creating no database when none exists yet
func (n *Notepad) Clear(jobID string) (int64, error) {
	if _, err := os.Stat(n.path); err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EBADF) || errors.Is(err, syscall.ELOOP) {
			return 0, nil
		}
		return 0, err
	}
	var removed int64
	ctx := n.homeContext()
	err := n.transaction(ctx, func(conn state.SQLiteConn) error {
		if err := bindableText(jobID); err != nil {
			return err
		}
		res, err := conn.ExecContext(ctx, "DELETE FROM cron_notepad WHERE job_id=?", jobID)
		if err != nil {
			return err
		}
		removed, err = res.RowsAffected()
		return err
	})
	return removed, err
}

// Prompt section listing a job's notes, empty for an empty or unreadable notepad
func (n *Notepad) RenderSection(jobID string) string {
	notes, err := func() (notes []*pyjson.Object, err error) {
		defer func() {
			if r := recover(); r != nil {
				notes, err = nil, fmt.Errorf("%v", r)
			}
		}()
		return notepadLister(n, jobID)
	}()
	if err != nil || len(notes) == 0 {
		return ""
	}
	lines := make([]string, 0, len(notes))
	for _, note := range notes {
		key, _ := note.Get("key")
		value, _ := note.Get("value")
		lines = append(lines, fmt.Sprintf("- %v: %v", key, value))
	}
	return "## Job notepad (persistent across runs)\n" +
		"This durable scratchpad survives between scheduled runs of this " +
		"job. Update it via the CLI, e.g.:\n" +
		fmt.Sprintf("`icebath cron notepad %s set <key> <value>` ", jobID) +
		fmt.Sprintf("(also: get/delete/list; `icebath cron notepad %s delete ", jobID) +
		"<key>` removes an entry).\n\n" + strings.Join(lines, "\n") + "\n\n"
}

// List reader the prompt section goes through, swapped by tests to fail the read
var notepadLister = (*Notepad).List

// Clears a removed job's notepad rows in the store home
func clearNotepad(s *Store, jobID string) error {
	_, err := openNotepad(s).Clear(jobID)
	return err
}
