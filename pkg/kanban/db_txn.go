package kanban

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pysqlite"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Python RuntimeError raised by misuse of the write transaction helper
type RuntimeError struct{ Msg string }

func (e *RuntimeError) Error() string { return e.Msg }

// DatabaseError when a rollback journal file is shorter than its header page count, WAL mode exempt
func checkFileLengthInvariant(ctx context.Context, conn state.SQLiteConn) error {
	row, err := QueryOne(ctx, conn, "PRAGMA journal_mode")
	if err != nil {
		if _, ok := state.SQLiteClassOf(err); ok {
			return nil
		}
		return err
	}
	mode := ""
	if row != nil && len(row.Vals) > 0 && row.Vals[0] != nil {
		mode = pystr.Lower(fmt.Sprint(row.Vals[0]))
	}
	if mode == "wal" {
		return nil
	}
	if matches, ok := state.FileLengthMatchesHeader(conn); ok && !matches {
		return &state.SQLiteError{Class: state.SQLiteDatabaseError, Msg: "torn-extend detected: the database file is shorter than its header page count claims"}
	}
	return nil
}

var (
	busyMaxRetries = 5
	busyRetryMin   = 20 * time.Millisecond
	busyRetryMax   = 150 * time.Millisecond
	busySleep      = time.Sleep
)

// Reports whether an error is an OperationalError saying the database is locked or busy
func isBusyError(err error) bool {
	class, ok := state.SQLiteClassOf(err)
	if !ok || class != state.SQLiteOperationalError {
		return false
	}
	text := pystr.Lower(state.ErrText(err))
	return strings.Contains(text, "database is locked") || strings.Contains(text, "database is busy")
}

// Runs a transaction boundary statement with jittered retries on busy errors
func executeBoundaryWithRetry(ctx context.Context, conn state.SQLiteConn, query string) error {
	for attempt := 0; ; attempt++ {
		_, err := conn.ExecContext(ctx, query)
		if err == nil {
			return nil
		}
		if !isBusyError(err) || attempt == busyMaxRetries {
			return err
		}
		busySleep(busyRetryMin + time.Duration(rand.Int64N(int64(busyRetryMax-busyRetryMin)+1)))
	}
}

// Filesystem path of the main database, empty for in memory or unreadable
func mainDBFile(ctx context.Context, conn state.SQLiteConn) string {
	rows, err := QueryRows(ctx, conn, "PRAGMA database_list")
	if err != nil {
		return ""
	}
	for _, r := range rows {
		if len(r.Vals) >= 3 && r.Vals[1] == "main" {
			s, _ := r.Vals[2].(string)
			return s
		}
	}
	return ""
}

// Reports whether a transaction is open on the connection
func inTransaction(conn state.SQLiteConn) bool {
	var inner any = conn
	if t, ok := conn.(*state.TrackedConn); ok {
		inner = t.SQLiteConn
	}
	raw, ok := inner.(pysqlite.RawConn)
	if !ok {
		return false
	}
	open, err := pysqlite.InTransaction(raw)
	return err == nil && open
}

// Runs a statement, swallowing OperationalError and returning any other failure
func execSuppressOperational(ctx context.Context, conn state.SQLiteConn, query string) error {
	_, err := conn.ExecContext(ctx, query)
	if class, ok := state.SQLiteClassOf(err); err != nil && ok && class == state.SQLiteOperationalError {
		return nil
	}
	return err
}

// Runs body in an IMMEDIATE write transaction, a savepoint when nesting is opted into, checking the file length after commit
func WriteTxn(ctx context.Context, conn state.SQLiteConn, allowNested bool, body func() error) (err error) {
	if err := assertNotDelegatedChildMutation(ctx, mainDBFile(ctx, conn)); err != nil {
		return err
	}
	if inTransaction(conn) {
		if !allowNested {
			return &RuntimeError{"write_txn: already inside a transaction. Nested composition " +
				"must opt in explicitly with write_txn(conn, allow_nested=True) " +
				"(savepoint semantics; the inner RELEASE is not durable until " +
				"the outer transaction commits)."}
		}
		token := make([]byte, 8)
		crand.Read(token)
		savepoint := "hermes_nested_" + hex.EncodeToString(token)
		if _, err := conn.ExecContext(ctx, "SAVEPOINT "+savepoint); err != nil {
			return err
		}
		undo := func() error {
			if _, err := conn.ExecContext(ctx, "ROLLBACK TO "+savepoint); err != nil {
				return suppressOperational(err)
			}
			return execSuppressOperational(ctx, conn, "RELEASE "+savepoint)
		}
		defer func() {
			if r := recover(); r != nil {
				undo()
				panic(r)
			}
		}()
		if err := body(); err != nil {
			if uerr := undo(); uerr != nil {
				return uerr
			}
			return err
		}
		_, err := conn.ExecContext(ctx, "RELEASE "+savepoint)
		return err
	}
	if err := executeBoundaryWithRetry(ctx, conn, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			execSuppressOperational(ctx, conn, "ROLLBACK")
			panic(r)
		}
	}()
	if err := body(); err != nil {
		if rerr := execSuppressOperational(ctx, conn, "ROLLBACK"); rerr != nil {
			return rerr
		}
		return err
	}
	if err := executeBoundaryWithRetry(ctx, conn, "COMMIT"); err != nil {
		if rerr := execSuppressOperational(ctx, conn, "ROLLBACK"); rerr != nil {
			return rerr
		}
		return err
	}
	return checkFileLengthInvariant(ctx, conn)
}

// Nil for an OperationalError, the error itself otherwise
func suppressOperational(err error) error {
	if class, ok := state.SQLiteClassOf(err); ok && class == state.SQLiteOperationalError {
		return nil
	}
	return err
}
