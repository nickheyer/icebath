package kanban

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/state"
)

var realKanbanRoot string

// Captures the real kanban root before sandboxing ICEBATH_HOME and installs the write guard on connect
func TestMain(m *testing.M) {
	realKanbanRoot = captureRealKanbanRoot()
	installTestGatewayStatusProbes()
	sandbox, err := os.MkdirTemp("", "kanban-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("ICEBATH_HOME", sandbox)
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "ICEBATH_KANBAN_") || k == "ICEBATH_DELEGATED_CHILD_CONTEXT" {
			os.Unsetenv(k)
		}
	}
	real := connectFn
	connectFn = func(ctx context.Context, dbPath string, board *string) (state.SQLiteConn, error) {
		if err := kanbanWriteGuard(ctx, dbPath, board); err != nil {
			return nil, err
		}
		return real(ctx, dbPath, board)
	}
	code := m.Run()
	os.RemoveAll(sandbox)
	os.Exit(code)
}

// Real kanban root from the pre sandbox environment, the order KanbanHome resolves it in
func captureRealKanbanRoot() string {
	if override := strings.TrimSpace(os.Getenv("ICEBATH_KANBAN_HOME")); override != "" {
		r, _ := pypath.Resolve(pypath.Expanduser(override))
		return r
	}
	if strings.TrimSpace(os.Getenv("ICEBATH_HOME")) != "" {
		r, _ := pypath.Resolve(home.Root())
		return r
	}
	uh, _ := home.UserHome()
	r, _ := pypath.Resolve(filepath.Join(uh, ".icebath"))
	return r
}

// RuntimeError for a connect whose resolved database lands under the real kanban root
func kanbanWriteGuard(ctx context.Context, dbPath string, board *string) error {
	target := dbPath
	if target == "" {
		p, err := KanbanDBPath(ctx, board)
		if err != nil {
			return err
		}
		target = p
	}
	resolved, _ := pypath.Resolve(pypath.Expanduser(target))
	if resolved == realKanbanRoot || strings.HasPrefix(resolved, realKanbanRoot+string(filepath.Separator)) {
		return &RuntimeError{fmt.Sprintf("kanban_write_guard: refusing to write kanban DB under the real root %s (resolved %s)", realKanbanRoot, resolved)}
	}
	return nil
}

// Temp home with ICEBATH_HOME and ICEBATH_KANBAN_HOME pointing at it and the user home patched
func setupKanbanHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_KANBAN_HOME", h)
	t.Cleanup(home.PatchUserHome(tmp))
	discardInitialized(t, mustDBPath(t, BoardArg("default")))
	return h
}

// Database path of a board, failing the test on error
func mustDBPath(t *testing.T, board *string) string {
	t.Helper()
	p, err := KanbanDBPath(context.Background(), board)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Drops a path from the init cache like _INITIALIZED_PATHS.discard
func discardInitialized(t *testing.T, path string) {
	t.Helper()
	if r, err := pypath.Resolve(path); err == nil {
		forgetInitialized(r)
	}
}

// Reports whether the resolved path is in the init cache
func isInitialized(path string) bool {
	r, _ := pypath.Resolve(path)
	return pathInitialized(r)
}

// Opens a connection or fails the test
func mustConnect(t *testing.T, dbPath string, board *string) state.SQLiteConn {
	t.Helper()
	conn, err := Connect(context.Background(), dbPath, board)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// Executes statements on a connection or fails the test
func mustExec(t *testing.T, conn state.SQLiteConn, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := conn.ExecContext(context.Background(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// Rows of a query or fails the test
func mustRows(t *testing.T, conn state.SQLiteConn, query string, args ...any) []*Row {
	t.Helper()
	rows, err := QueryRows(context.Background(), conn, query, args...)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// Plain sqlite connection outside the kanban connect path
func rawSQLite(t *testing.T, path string) state.SQLiteConn {
	t.Helper()
	conn, err := state.ConnectSQLite(path, state.SQLiteConfig{Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	return conn
}
