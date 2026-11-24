package sqliteutil

import (
	"context"
	"errors"
	"math"
	"time"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// Error of a backup asked to copy a connection onto itself
var ErrSameConnection = errors.New("backup source and target are the same connection")

// Copies the main database of src over the main database of dst with SQLite's online backup: pages per step (every page in one step when not positive), progress called after each step with the step's result code and the pages remaining and in total (its error ends the copy and is returned), sleep paused after a step that found the source busy or locked; a *Error when SQLite fails, also when dst is inside a transaction
func Backup(src, dst RawConn, pages int, progress func(status, remaining, total int) error, sleep time.Duration) error {
	return BackupContext(context.Background(), src, dst, pages, progress, sleep)
}

// Backup that also stops before a step and during a pause once ctx is done, returning the context's error
func BackupContext(ctx context.Context, src, dst RawConn, pages int, progress func(status, remaining, total int) error, sleep time.Duration) error {
	step := int32(-1)
	if pages > 0 {
		step = int32(min(pages, math.MaxInt32))
	}
	// The source's driver connection is looked up on its own first: borrowing one connection twice, through two values wrapping it, would wait forever on its lock
	var source unsafe.Pointer
	if err := withHandle(src, func(s handle) error {
		source = s.owner
		return nil
	}); err != nil {
		return err
	}
	return withHandle(dst, func(d handle) error {
		if d.owner == source {
			return ErrSameConnection
		}
		return withHandle(src, func(s handle) error {
			if sqlite3.Xsqlite3_get_autocommit(d.tls, d.db) == 0 {
				return &Error{code: sqlite3.SQLITE_ERROR, msg: "backup target is in a transaction"}
			}
			return backup(ctx, d.tls, s.db, d.db, step, progress, sleep)
		})
	})
}

// One backup from start to finish on the target's thread state, the target's error when SQLite fails
func backup(ctx context.Context, tls *libc.TLS, srcDB, dstDB uintptr, step int32, progress func(status, remaining, total int) error, sleep time.Duration) error {
	name, err := libc.CString("main")
	if err != nil {
		return err
	}
	defer libc.Xfree(tls, name)
	b := sqlite3.Xsqlite3_backup_init(tls, dstDB, name, srcDB, name)
	if b == 0 {
		return handleError(tls, dstDB)
	}
	stopped := backupSteps(ctx, tls, b, step, progress, sleep)
	rc := sqlite3.Xsqlite3_backup_finish(tls, b)
	if stopped != nil {
		return stopped
	}
	if rc != sqlite3.SQLITE_OK {
		return codeError(tls, dstDB, rc)
	}
	return nil
}

// Steps a backup until SQLite reports it done or failed, nil then for the finish call to judge; the progress or context error that stopped it early otherwise
func backupSteps(ctx context.Context, tls *libc.TLS, b uintptr, step int32, progress func(status, remaining, total int) error, sleep time.Duration) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rc := sqlite3.Xsqlite3_backup_step(tls, b, step)
		if progress != nil {
			remaining := int(sqlite3.Xsqlite3_backup_remaining(tls, b))
			total := int(sqlite3.Xsqlite3_backup_pagecount(tls, b))
			if err := progress(int(rc), remaining, total); err != nil {
				return err
			}
		}
		switch rc & 0xff {
		case sqlite3.SQLITE_OK:
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			if err := pause(ctx, sleep); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

// Waits d unless ctx ends first, the context's error then
func pause(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
