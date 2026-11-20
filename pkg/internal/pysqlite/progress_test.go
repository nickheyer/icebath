package pysqlite

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

const heavyQuery = "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c WHERE x < 200000) SELECT count(*) FROM c"

func openConn(t *testing.T) *sql.Conn {
	t.Helper()
	pool, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	conn, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestProgressHandlerRunsAbortsAndClears(t *testing.T) {
	conn := openConn(t)
	ctx := context.Background()
	calls := 0
	if err := SetProgressHandler(conn, func() int { calls++; return 0 }, 1000); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := conn.QueryRowContext(ctx, heavyQuery).Scan(&n); err != nil || n != 200000 || calls < 2 {
		t.Fatalf("count %d err %v calls %d", n, err, calls)
	}
	if err := SetProgressHandler(conn, func() int { return 1 }, 1000); err != nil {
		t.Fatal(err)
	}
	err := conn.QueryRowContext(ctx, heavyQuery).Scan(&n)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "interrupt") {
		t.Fatalf("want interrupted, got %v", err)
	}
	if err := SetProgressHandler(conn, nil, 0); err != nil {
		t.Fatal(err)
	}
	before := calls
	if err := conn.QueryRowContext(ctx, heavyQuery).Scan(&n); err != nil || calls != before {
		t.Fatalf("cleared handler err %v calls %d", err, calls-before)
	}
}

func TestProgressHandlerRefusesOtherDrivers(t *testing.T) {
	if _, _, err := driverHandles(&struct{ db uintptr }{}); err == nil {
		t.Fatal("foreign driver connection accepted")
	}
}
