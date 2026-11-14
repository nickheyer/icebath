package state

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// One statement a traced connection ran, with its bound arguments
type tracedStatement struct {
	query string
	args  []any
}

// Connection recording every statement SQLite prepared and ran, the set_trace_callback view, exposing the raw handle
type statementTracer struct {
	SQLiteConn
	mu     *sync.Mutex
	log    *[]tracedStatement
	before func(query string)
}

// Runs the before hook of the tracer when one is installed
func (s *statementTracer) starting(query string) {
	if s.before != nil {
		s.before(query)
	}
}

func (s *statementTracer) record(query string, args []any) {
	s.mu.Lock()
	*s.log = append(*s.log, tracedStatement{query, args})
	s.mu.Unlock()
}

func (s *statementTracer) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	s.starting(query)
	res, err := s.SQLiteConn.ExecContext(ctx, query, args...)
	if err == nil {
		s.record(query, args)
	}
	return res, err
}

func (s *statementTracer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	s.starting(query)
	rows, err := s.SQLiteConn.QueryContext(ctx, query, args...)
	if err == nil {
		s.record(query, args)
	}
	return rows, err
}

func (s *statementTracer) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	s.starting(query)
	row := s.SQLiteConn.QueryRowContext(ctx, query, args...)
	if row.Err() == nil {
		s.record(query, args)
	}
	return row
}

func (s *statementTracer) Raw(f func(driverConn any) error) error {
	inner := s.SQLiteConn
	for {
		switch c := inner.(type) {
		case *TrackedConn:
			inner = c.SQLiteConn
			continue
		case rawConner:
			return c.Raw(f)
		}
		return fmt.Errorf("%w: %T", errNoSQLiteHandle, inner)
	}
}

var indexChoiceReaders = map[string]func(ctx context.Context, db *SessionDB) ([]*pyjson.Object, error){
	"sidebar": func(ctx context.Context, db *SessionDB) ([]*pyjson.Object, error) {
		limit := 2
		return db.ListSessionsRich(ctx, ListSessionsRichOptions{Limit: &limit, OrderByLastActive: true, IncludePinned: true})
	},
	"session_search": func(ctx context.Context, db *SessionDB) ([]*pyjson.Object, error) {
		limit := 2
		return db.ListRecentSessionsBounded(ctx, RecentSessionsBoundedOptions{Limit: &limit})
	},
	"telegram_unlinked": func(ctx context.Context, db *SessionDB) ([]*pyjson.Object, error) {
		return db.ListUnlinkedTelegramSessionsForUser(ctx, "u", "", 2)
	},
}

// Four telegram sessions of forty messages each whose planner stats lack the session id index
func staleStatsDB(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	db := mustSessionDB(t, path, false)
	for i := range 4 {
		sid := fmt.Sprintf("s%d", i)
		if err := db.CreateSession(NewSession{ID: sid, Source: "telegram", UserID: "u"}); err != nil {
			t.Fatal(err)
		}
		batch := make([]map[string]any, 40)
		for j := range batch {
			role := "assistant"
			if j%2 == 0 {
				role = "user"
			}
			batch[j] = map[string]any{"role": role, "content": fmt.Sprintf("%s message %d", sid, j), "timestamp": float64(1_000 + j)}
		}
		if _, err := db.AppendMessagesBatch(ctx, sid, batch, AppendGuards{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.SetSessionPinned(ctx, "s0", true); err != nil {
		t.Fatal(err)
	}
	db.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, stmt := range []string{"ANALYZE", "DELETE FROM sqlite_stat1 WHERE idx = 'idx_messages_session_id'"} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
}

var messageSearchPlan = regexp.MustCompile(`^(SEARCH|SCAN) (m|_act_m)\b`)

func TestSessionListingsSearchMessagesOnlyThroughTheTimestampIndex(t *testing.T) {
	names := make([]string, 0, len(indexChoiceReaders))
	for name := range indexChoiceReaders {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "state.db")
			staleStatsDB(t, path)
			var mu sync.Mutex
			var statements []tracedStatement
			saved := sessionConnectTracked
			sessionConnectTracked = func(p string, opts ...TrackedOption) (*TrackedConn, error) {
				conn, err := saved(p, opts...)
				if conn != nil {
					conn.SQLiteConn = &statementTracer{SQLiteConn: conn.SQLiteConn, mu: &mu, log: &statements}
				}
				return conn, err
			}
			db := mustSessionDB(t, path, false)
			mu.Lock()
			statements = nil
			mu.Unlock()
			rows, err := indexChoiceReaders[name](ctx, db)
			db.Close()
			sessionConnectTracked = saved
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) == 0 {
				t.Fatal("no rows listed")
			}
			for _, row := range rows {
				if preview, _ := row.Get("preview"); !pyTruthy(preview) {
					t.Fatalf("row without preview %v", row)
				}
			}
			planConn, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
			if err != nil {
				t.Fatal(err)
			}
			defer planConn.Close()
			var plans []string
			for _, st := range statements {
				fields := strings.Fields(st.query)
				if len(fields) == 0 || (strings.ToUpper(fields[0]) != "SELECT" && strings.ToUpper(fields[0]) != "WITH") {
					continue
				}
				planRows, err := planConn.QueryContext(ctx, "EXPLAIN QUERY PLAN "+st.query, st.args...)
				if err != nil {
					t.Fatalf("plan of %q: %v", st.query, err)
				}
				for planRows.Next() {
					var id, parent, notused int64
					var detail string
					if err := planRows.Scan(&id, &parent, &notused, &detail); err != nil {
						t.Fatal(err)
					}
					plans = append(plans, detail)
				}
				planRows.Close()
			}
			aliases := map[string]bool{}
			var searches []string
			for _, p := range plans {
				if messageSearchPlan.MatchString(p) {
					searches = append(searches, p)
					aliases[strings.Fields(p)[1]] = true
				}
			}
			if len(aliases) != 2 || !aliases["m"] || !aliases["_act_m"] {
				t.Fatalf("message searches %v of plans %v", searches, plans)
			}
			for _, p := range searches {
				if !strings.Contains(p, "INDEX idx_messages_session (") {
					t.Fatalf("message search off the timestamp index: %v", searches)
				}
			}
		})
	}
}
