package state

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Connection proxy that records every statement text it runs
type trackingConn struct {
	SQLiteConn
	mu    sync.Mutex
	calls []string
}

func (c *trackingConn) record(query string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, query)
}

func (c *trackingConn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	c.record(query)
	return c.SQLiteConn.ExecContext(ctx, query, args...)
}

func (c *trackingConn) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	c.record(query)
	return c.SQLiteConn.QueryContext(ctx, query, args...)
}

func (c *trackingConn) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	c.record(query)
	return c.SQLiteConn.QueryRowContext(ctx, query, args...)
}

func (c *trackingConn) matching(keep func(string) bool) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, q := range c.calls {
		if keep(q) {
			out = append(out, q)
		}
	}
	return out
}

func (c *trackingConn) index(query string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Index(c.calls, query)
}

func TestOptimizeStorageUsesPassiveAfterVacuum(t *testing.T) {
	s, host := searchStore(t, nil)
	tracking := &trackingConn{SQLiteConn: host.conn}
	host.conn = tracking
	result, err := s.OptimizeFTSStorage(context.Background(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := result.Get("ok"); ok != true {
		t.Fatalf("ok %v", objJSON(t, result))
	}
	if vacuumed, _ := result.Get("vacuumed"); vacuumed != true {
		t.Fatalf("vacuumed %v", objJSON(t, result))
	}
	checkpoints := tracking.matching(func(q string) bool { return strings.Contains(strings.ToLower(q), "wal_checkpoint") })
	var truncates, passives []string
	for _, q := range checkpoints {
		if strings.Contains(q, "TRUNCATE") {
			truncates = append(truncates, q)
		}
		if strings.Contains(q, "PASSIVE") {
			passives = append(passives, q)
		}
	}
	vacuums := tracking.matching(func(q string) bool { return strings.ToUpper(strings.TrimSpace(q)) == "VACUUM" })
	if len(truncates) != 0 {
		t.Fatalf("truncate checkpoints %q", truncates)
	}
	if !slices.Equal(passives, []string{"PRAGMA wal_checkpoint(PASSIVE)"}) {
		t.Fatalf("passive checkpoints %q", passives)
	}
	if !slices.Equal(vacuums, []string{"VACUUM"}) {
		t.Fatalf("vacuums %q", vacuums)
	}
	if tracking.index(vacuums[0]) >= tracking.index(passives[0]) {
		t.Fatalf("PASSIVE checkpoint must follow the VACUUM: %q", tracking.calls)
	}
}
