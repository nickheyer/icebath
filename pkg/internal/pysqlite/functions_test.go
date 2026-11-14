package pysqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestCreateFunctionRoundTripsEveryValueType(t *testing.T) {
	conn := openConn(t)
	ctx := context.Background()
	var seen []any
	echo := func(args []any) (any, error) {
		seen = append(seen, args[0])
		return args[0], nil
	}
	if err := CreateFunction(conn, "echo", 1, true, echo); err != nil {
		t.Fatal(err)
	}
	var i int64
	var f float64
	var s string
	var b []byte
	var n any
	if err := conn.QueryRowContext(ctx, "SELECT echo(7), echo(1.5), echo(?), echo(?), echo(NULL)", "tx\x00t", []byte{0, 1}).Scan(&i, &f, &s, &b, &n); err != nil {
		t.Fatal(err)
	}
	if i != 7 || f != 1.5 || s != "tx\x00t" || string(b) != "\x00\x01" || n != nil || len(seen) != 5 {
		t.Fatalf("round trip %d %v %q %v %v %v", i, f, s, b, n, seen)
	}
	if seen[2] != "tx\x00t" || string(seen[3].([]byte)) != "\x00\x01" || seen[4] != nil {
		t.Fatalf("decoded args %v", seen)
	}
}

func TestCreateFunctionReportsRaisingAndUnsupportedResults(t *testing.T) {
	conn := openConn(t)
	ctx := context.Background()
	if err := CreateFunction(conn, "boom", 0, false, func([]any) (any, error) { return nil, errors.New("inner") }); err != nil {
		t.Fatal(err)
	}
	if err := CreateFunction(conn, "odd", 0, false, func([]any) (any, error) { return []any{1}, nil }); err != nil {
		t.Fatal(err)
	}
	var v any
	for _, q := range []string{"SELECT boom()", "SELECT odd()"} {
		err := conn.QueryRowContext(ctx, q).Scan(&v)
		if err == nil || !strings.Contains(err.Error(), functionRaisedMsg) {
			t.Fatalf("%s err %v", q, err)
		}
	}
	if err := CreateFunction(conn, "boom", 0, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT boom()").Scan(&v); err == nil || !strings.Contains(err.Error(), "no such function") {
		t.Fatalf("removed function err %v", err)
	}
}

func TestSetAuthorizerDeniesAndClears(t *testing.T) {
	conn := openConn(t)
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, "CREATE TABLE t(a, secret)"); err != nil {
		t.Fatal(err)
	}
	var reads [][2]string
	auth := func(action int, table, column, _, _ string) int {
		if action == ActionRead {
			reads = append(reads, [2]string{table, column})
			if column == "secret" {
				return AuthDeny
			}
		}
		if action == ActionInsert {
			return AuthDeny
		}
		return AuthOK
	}
	if err := SetAuthorizer(conn, auth); err != nil {
		t.Fatal(err)
	}
	var v any
	if err := conn.QueryRowContext(ctx, "SELECT a FROM t").Scan(&v); !errors.Is(err, sql.ErrNoRows) || len(reads) == 0 {
		t.Fatalf("plain read %v reads %v", err, reads)
	}
	if err := conn.QueryRowContext(ctx, "SELECT secret FROM t").Scan(&v); err == nil || !strings.Contains(err.Error(), "prohibited") {
		t.Fatalf("denied column err %v", err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO t VALUES (1, 2)"); err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("denied insert err %v", err)
	}
	if err := SetAuthorizer(conn, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO t VALUES (1, 2)"); err != nil {
		t.Fatalf("cleared authorizer still denies %v", err)
	}
}

func TestSetTraceCallbackReportsExpandedStatementsAndClears(t *testing.T) {
	conn := openConn(t)
	ctx := context.Background()
	var seen []string
	if err := SetTraceCallback(conn, func(sql string) { seen = append(seen, sql) }); err != nil {
		t.Fatal(err)
	}
	var v string
	if err := conn.QueryRowContext(ctx, "SELECT ?", "abc").Scan(&v); err != nil || v != "abc" {
		t.Fatalf("query %q %v", v, err)
	}
	if len(seen) != 1 || seen[0] != "SELECT 'abc'" {
		t.Fatalf("traced %q", seen)
	}
	if err := SetTraceCallback(conn, nil); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT ?", "def").Scan(&v); err != nil || len(seen) != 1 {
		t.Fatalf("cleared trace still records %q %v", seen, err)
	}
}
