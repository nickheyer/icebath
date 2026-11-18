package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Python or SQLite version as three unbounded integers, the shape _version_tuple returns
type VersionTuple [3]*big.Int

// Version tuple of plain integers, missing parts zero and extra parts dropped
func NewVersionTuple(parts ...int64) VersionTuple {
	var v VersionTuple
	for i := range v {
		v[i] = new(big.Int)
		if i < len(parts) {
			v[i].SetInt64(parts[i])
		}
	}
	return v
}

// Lexicographic comparison of two version tuples
func (v VersionTuple) Cmp(o VersionTuple) int {
	for i := range v {
		if c := v[i].Cmp(o[i]); c != 0 {
			return c
		}
	}
	return 0
}

// Iterates a decoded JSON value the way Python's for loop does, the TypeError for non iterables
func pyIterate(v any) ([]any, error) {
	switch x := v.(type) {
	case []any:
		return x, nil
	case string:
		out := make([]any, 0, len(x))
		for _, r := range x {
			out = append(out, string(r))
		}
		return out, nil
	case pyObject:
		out := make([]any, 0, len(x))
		for _, kv := range x {
			out = append(out, kv.Key)
		}
		return out, nil
	}
	return nil, &pyConvError{"TypeError", fmt.Sprintf("'%s' object is not iterable", pyTypeName(v))}
}

// Python type name of a decoded JSON value
func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case json.Number, int, int64:
		return "int"
	}
	return "float"
}

// int of every part padded with zeros to three and cut at three, with the errors int and iteration raise
func versionTuple(parts any) (VersionTuple, error) {
	items, err := pyIterate(parts)
	if err != nil {
		return VersionTuple{}, err
	}
	v := NewVersionTuple()
	for i, item := range items {
		n, err := pyIntOf(item)
		if err != nil {
			return VersionTuple{}, err
		}
		if i < len(v) {
			v[i] = n
		}
	}
	return v, nil
}

// Whether a SQLite version carries the WAL reset bug, every release from 3.7.0 before the fixed 3.44.6, 3.50.7 and 3.51.3
func IsSQLiteWALResetVulnerable(info VersionTuple) bool {
	return !(info.Cmp(NewVersionTuple(3, 7, 0)) < 0 ||
		info.Cmp(NewVersionTuple(3, 51, 3)) >= 0 ||
		(info.Cmp(NewVersionTuple(3, 50, 7)) >= 0 && info.Cmp(NewVersionTuple(3, 51, 0)) < 0) ||
		(info.Cmp(NewVersionTuple(3, 44, 6)) >= 0 && info.Cmp(NewVersionTuple(3, 45, 0)) < 0))
}

// Hidden verb the icebath binary answers with the facts of the SQLite linked into it
const SQLiteRuntimeVerb = "sqlite-runtime"

// SQLite facts one icebath binary reports about the library linked into it
type SQLiteRuntimeInfo struct {
	SQLiteVersion       VersionTuple
	SQLiteVersionString string
	MissingFeatures     []string
}

// Whether the binary's linked SQLite carries the WAL reset bug
func (i *SQLiteRuntimeInfo) WALResetVulnerable() bool {
	return IsSQLiteWALResetVulnerable(i.SQLiteVersion)
}

// Whether the linked SQLite is free of the WAL reset bug and has every feature the session store needs
func (i *SQLiteRuntimeInfo) Safe() bool {
	return !i.WALResetVulnerable() && len(i.MissingFeatures) == 0
}

// Wire shape of the report the hidden verb prints
type sqliteRuntimeReport struct {
	SQLiteVersion       []int64  `json:"sqlite_version"`
	SQLiteVersionString string   `json:"sqlite_version_string"`
	SQLiteSourceID      string   `json:"sqlite_source_id"`
	MissingFeatures     []string `json:"missing_features"`
}

// Features the session store schema relies on, each proven by statements against an in memory database
var storeFeatureProbes = []struct {
	name  string
	check func(ctx context.Context, conn SQLiteConn) error
}{
	{"FTS5", func(ctx context.Context, conn SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "CREATE VIRTUAL TABLE temp.probe_fts USING fts5(content)")
		return err
	}},
	{"the FTS5 trigram tokenizer", func(ctx context.Context, conn SQLiteConn) error {
		if _, err := conn.ExecContext(ctx, "CREATE VIRTUAL TABLE temp.probe_trigram USING fts5(content, tokenize='trigram')"); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO temp.probe_trigram VALUES ('icebath')"); err != nil {
			return err
		}
		var n int64
		if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM temp.probe_trigram WHERE probe_trigram MATCH 'eba'").Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("trigram match found %d rows", n)
		}
		return nil
	}},
	{"the JSON functions", func(ctx context.Context, conn SQLiteConn) error {
		var value, count int64
		err := conn.QueryRowContext(ctx, "SELECT json_extract(json_set('{}', '$.a', 7), '$.a'), "+
			"(SELECT count(*) FROM json_each(json_array(1, 2)))").Scan(&value, &count)
		if err == nil && (value != 7 || count != 2) {
			err = fmt.Errorf("json functions returned %d and %d", value, count)
		}
		return err
	}},
	{"RETURNING", func(ctx context.Context, conn SQLiteConn) error {
		if _, err := conn.ExecContext(ctx, "CREATE TEMP TABLE probe_returning(id INTEGER PRIMARY KEY)"); err != nil {
			return err
		}
		var id int64
		return conn.QueryRowContext(ctx, "INSERT INTO temp.probe_returning DEFAULT VALUES RETURNING id").Scan(&id)
	}},
}

// Writes the linked SQLite version, its source id and the store features it lacks as one JSON line
func WriteSQLiteRuntimeReport(ctx context.Context, w io.Writer) error {
	conn, err := ConnectSQLite(":memory:", SQLiteConfig{Timeout: 5.0})
	if err != nil {
		return err
	}
	defer conn.Close()
	report := sqliteRuntimeReport{MissingFeatures: []string{}}
	if err := conn.QueryRowContext(ctx, "SELECT sqlite_version(), sqlite_source_id()").Scan(&report.SQLiteVersionString, &report.SQLiteSourceID); err != nil {
		return err
	}
	for _, part := range strings.Split(report.SQLiteVersionString, ".") {
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return fmt.Errorf("SQLite reports the malformed version %q", report.SQLiteVersionString)
		}
		report.SQLiteVersion = append(report.SQLiteVersion, n)
	}
	for _, feature := range storeFeatureProbes {
		if feature.check(ctx, conn) != nil {
			report.MissingFeatures = append(report.MissingFeatures, feature.name)
		}
	}
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", data)
	return err
}

// Builds the runtime info from the verb's output, an error naming what makes the report unusable
func sqliteRuntimeFromReport(stdout []byte) (*SQLiteRuntimeInfo, error) {
	var report sqliteRuntimeReport
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &report); err != nil {
		return nil, fmt.Errorf("unreadable report %q: %w", strings.TrimSpace(string(stdout)), err)
	}
	switch {
	case len(report.SQLiteVersion) == 0 || report.SQLiteVersionString == "":
		return nil, errors.New("the report names no SQLite version")
	case report.SQLiteSourceID == "":
		return nil, errors.New("the report names no SQLite source id, so no query ran")
	case report.MissingFeatures == nil:
		return nil, errors.New("the report lists no feature check")
	}
	return &SQLiteRuntimeInfo{
		SQLiteVersion: NewVersionTuple(report.SQLiteVersion...), SQLiteVersionString: report.SQLiteVersionString,
		MissingFeatures: report.MissingFeatures,
	}, nil
}

// Report of an icebath binary's hidden SQLite verb, an error when it fails, times out or prints nothing usable
func ProbeSQLiteRuntime(binary string, timeout float64) (*SQLiteRuntimeInfo, error) {
	limit := time.Duration(timeout * float64(time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, SQLiteRuntimeVerb)
	if errors.Is(cmd.Err, exec.ErrDot) {
		cmd.Err = nil
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%s %s timed out after %s", binary, SQLiteRuntimeVerb, limit)
	}
	if err != nil {
		if lines := strings.Split(strings.TrimSpace(stderr.String()), "\n"); lines[len(lines)-1] != "" {
			return nil, fmt.Errorf("%s %s: %w: %s", binary, SQLiteRuntimeVerb, err, lines[len(lines)-1])
		}
		return nil, fmt.Errorf("%s %s: %w", binary, SQLiteRuntimeVerb, err)
	}
	info, err := sqliteRuntimeFromReport(stdout.Bytes())
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", binary, SQLiteRuntimeVerb, err)
	}
	return info, nil
}
