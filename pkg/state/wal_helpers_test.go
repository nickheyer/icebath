package state

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Restores a package variable to its value at the start of the test
func pinVar[T any](t *testing.T, target *T, value T) {
	t.Helper()
	saved := *target
	*target = value
	t.Cleanup(func() { *target = saved })
}

// Clears every once per process log set and the cross VM cache around a test
func resetWALOnceState(t *testing.T) {
	t.Helper()
	clear := func() {
		for _, entry := range onceLogs {
			entry.mu.Lock()
			entry.seen = map[string]bool{}
			entry.mu.Unlock()
		}
		crossVMFSCacheMu.Lock()
		crossVMFSCache = map[string]bool{}
		crossVMFSCacheMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// Points ICEBATH_HOME at a fresh home whose config.yaml holds the given text
func writeHomeConfig(t *testing.T, yamlText string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "icebath-home")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yamlText), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Opens a Python style sqlite3 connection with the default five second timeout
func openWALTestConn(t *testing.T, path string) SQLiteConn {
	t.Helper()
	conn, err := ConnectSQLite(path, SQLiteConfig{Timeout: 5.0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// Journal mode the connection reports right now, lower cased
func journalModeOf(t *testing.T, conn SQLiteConn) string {
	t.Helper()
	row, err := fetchOne(context.Background(), conn, "PRAGMA journal_mode")
	if err != nil {
		t.Fatal(err)
	}
	return modeFromRow(row)
}

// Connection wrapper recording every pragma statement issued through it
type pragmaSpyConn struct {
	SQLiteConn
	mu      sync.Mutex
	pragmas []string
}

func (c *pragmaSpyConn) note(query string) {
	if strings.HasPrefix(strings.ToLower(strings.TrimLeft(query, " \t\r\n")), "pragma") {
		c.mu.Lock()
		c.pragmas = append(c.pragmas, query)
		c.mu.Unlock()
	}
}

func (c *pragmaSpyConn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	c.note(query)
	return c.SQLiteConn.ExecContext(ctx, query, args...)
}

func (c *pragmaSpyConn) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	c.note(query)
	return c.SQLiteConn.QueryContext(ctx, query, args...)
}

func (c *pragmaSpyConn) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	c.note(query)
	return c.SQLiteConn.QueryRowContext(ctx, query, args...)
}

func (c *pragmaSpyConn) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.pragmas...)
}

// Log lines in a capture buffer at the given level that contain the text
func logLinesWith(buf *syncBuffer, level, text string) []string {
	var out []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "level="+level) && strings.Contains(line, text) {
			out = append(out, line)
		}
	}
	return out
}
