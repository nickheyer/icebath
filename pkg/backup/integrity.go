package backup

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

var sqliteHeader = []byte("SQLite format 3\x00")

// Size above which the full integrity check yields to the header and schema probe
const DefaultIntegrityCheckMaxBytes int64 = 2 << 30

// Outcome of a SQLite verification, Size nil when the file could not be stat'd
type IntegrityResult struct {
	Valid   bool
	Message string
	Size    *int64
}

// Options of a SQLite verification, the zero value meaning header check, pragma and the default size cap
type IntegrityOptions struct {
	SkipHeader bool
	SkipPragma bool
	MaxBytes   *int64
}

// Runs a query function on a read only connection to a database, returning its error instead of failing
func queryROSQLite(path string, fn func(c *sqliteConn) error) error {
	uri, err := state.ReadOnlyDBURI(path)
	if err != nil {
		return err
	}
	c, err := openSQLite(context.Background(), uri+"&_pragma=busy_timeout(1000)")
	if err != nil {
		return err
	}
	defer closeQuietly(c)
	return fn(c)
}

// Reports whether an error is a sqlite3 DatabaseError or one of its subclasses
func isDatabaseError(err error) bool {
	class, ok := state.SQLiteClassOf(err)
	return ok && class != state.SQLiteInterfaceError
}

// First column of every row of a query as Python str
func queryColumn(c *sqliteConn, q string) ([]string, error) {
	rows, err := c.c.QueryContext(context.Background(), q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v any
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		if b, ok := v.([]byte); ok {
			v = string(b)
		}
		out = append(out, pyrepr.Str(v))
	}
	return out, rows.Err()
}

// Verifies a SQLite file by size, header magic, then integrity_check or the cheap probe above the size cap
func VerifySQLiteIntegrity(path string, opts IntegrityOptions) IntegrityResult {
	maxBytes := DefaultIntegrityCheckMaxBytes
	if opts.MaxBytes != nil {
		maxBytes = *opts.MaxBytes
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return IntegrityResult{Message: "not found: " + pypath.PathStr(path)}
	}
	if err != nil {
		return IntegrityResult{Message: "cannot stat: " + excText(err)}
	}
	size := info.Size()
	done := func(message string, valid bool) IntegrityResult {
		return IntegrityResult{Valid: valid, Message: message, Size: &size}
	}
	if size < 100 {
		return done(fmt.Sprintf("too small (%d bytes) to be a valid SQLite database", size), false)
	}
	if !opts.SkipHeader {
		head, ok := state.ReadHeaderBytesPreopen(path, len(sqliteHeader), false)
		if !ok {
			return done("cannot read header", false)
		}
		if !bytes.Equal(head, sqliteHeader) {
			return done("missing SQLite header magic (got "+pyrepr.StrRepr(hex.EncodeToString(head[:min(16, len(head))]))+")", false)
		}
	}
	if maxBytes > 0 && size > maxBytes {
		err := queryROSQLite(path, func(c *sqliteConn) error {
			if _, err := queryColumn(c, "PRAGMA schema_version"); err != nil {
				return err
			}
			_, err := queryColumn(c, "SELECT count(*) FROM sqlite_master")
			return err
		})
		if err != nil {
			kind := "error"
			if isDatabaseError(err) {
				kind = "failed"
			}
			return done(fmt.Sprintf("schema probe %s: %s", kind, excText(err)), false)
		}
		sizeText, _ := pyrepr.Format(size, ",")
		maxText, _ := pyrepr.Format(maxBytes, ",")
		return done("size "+sizeText+" bytes exceeds max_bytes "+maxText+"; "+
			"skipped PRAGMA integrity_check (header + schema probe passed)", true)
	}
	if !opts.SkipPragma {
		var rows []string
		err := queryROSQLite(path, func(c *sqliteConn) error {
			var err error
			rows, err = queryColumn(c, "PRAGMA integrity_check")
			return err
		})
		if err != nil {
			kind := "integrity check error"
			if isDatabaseError(err) {
				kind = "cannot open database"
			}
			return done(kind+": "+excText(err), false)
		}
		if len(rows) == 1 && rows[0] == "ok" {
			return done("integrity check passed", true)
		}
		return done("integrity check failed: "+strings.Join(rows[:min(5, len(rows))], "; "), false)
	}
	return done("header check passed", true)
}
