package state

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// JSON receipt of a local reset lineage as the gateway stores it
func lineageReceipt(t *testing.T, owner string, lineage ...string) string {
	t.Helper()
	text, err := pyDumps(pyjson.ObjectOf("session_id", owner, "lineage", stringsToAny(lineage)), pyJSONDefault)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

// Writer statement through the store's committed write path
func lineageExec(t *testing.T, db *SessionDB, query string, args ...any) {
	t.Helper()
	if err := db.writeSQL(context.Background(), query, args, false, nil); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// Store with an owner reset segment continued by head, the hermes lineage cache fixture
func lineageCacheDB(t *testing.T) *SessionDB {
	t.Helper()
	db := usageDB(t)
	for _, sid := range []string{"owner", "head"} {
		readSession(t, db, NewSession{ID: sid, Source: "cli"})
	}
	lineageExec(t, db, "UPDATE sessions SET end_reason='session_reset' WHERE id='owner'")
	lineageExec(t, db, "INSERT INTO state_meta(key,value) VALUES(?,?)", LocalPolicyPrefix+"owner", lineageReceipt(t, "owner", "owner", "head"))
	return db
}

// Ids of the sessions a rich listing returns
func lineageListIDs(t *testing.T, db *SessionDB, o ListSessionsRichOptions) []string {
	t.Helper()
	rows, err := db.ListSessionsRich(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, row := range rows {
		out = append(out, pyStr(rowValue(row, "id")))
	}
	return out
}

// Sorted copy of ids for set comparison
func sortedIDsOf(ids []string) []string {
	out := append([]string{}, ids...)
	sort.Strings(out)
	return out
}

// Lineage index of a connection, failing the test on error
func lineageIndexOf(t *testing.T, db *SessionDB, conn SQLiteConn) localLineageIndexValue {
	t.Helper()
	value, err := db.cachedLocalLineageIndex(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// Index value with no superseded roots and no heads
func emptyLineageIndex() localLineageIndexValue {
	return localLineageIndexValue{superseded: map[string]bool{}, heads: map[string]localLineageHead{}}
}

// Index value the fixture projects, owner superseded and head owning the two segment lineage
func fixtureLineageIndex() localLineageIndexValue {
	return localLineageIndexValue{superseded: map[string]bool{"owner": true},
		heads: map[string]localLineageHead{"head": {owner: "owner", lineage: []string{"owner", "head"}}}}
}

// Fails unless two index values project the same roots and heads
func requireLineageIndex(t *testing.T, got, want localLineageIndexValue, what string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

// Receipt GLOB scans of state_meta among traced statements
func receiptScans(mu *sync.Mutex, statements *[]tracedStatement) int {
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, s := range *statements {
		if strings.Contains(s.query, "FROM state_meta") && strings.Contains(s.query, "GLOB") {
			for _, arg := range s.args {
				if text, ok := arg.(string); ok && strings.Contains(text, LocalPolicyPrefix) {
					n++
					break
				}
			}
		}
	}
	return n
}

// Routes every tracked connection opened from now on through a statement tracer
func traceTrackedConnections(t *testing.T, mu *sync.Mutex, statements *[]tracedStatement) {
	t.Helper()
	saved := sessionConnectTracked
	sessionConnectTracked = func(p string, opts ...TrackedOption) (*TrackedConn, error) {
		conn, err := saved(p, opts...)
		if conn != nil {
			conn.SQLiteConn = &statementTracer{SQLiteConn: conn.SQLiteConn, mu: mu, log: statements}
		}
		return conn, err
	}
	t.Cleanup(func() { sessionConnectTracked = saved })
}

func TestUnchangedSessionListsAndCountsScanReceiptsOnce(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var statements []tracedStatement
	traceTrackedConnections(t, &mu, &statements)
	db := lineageCacheDB(t)
	mu.Lock()
	statements = nil
	mu.Unlock()
	limit := 10
	for range 5 {
		rows, err := db.ListSessionsRich(ctx, ListSessionsRichOptions{Limit: &limit})
		if err != nil || len(rows) != 1 || rowValue(rows[0], "id") != "head" {
			t.Fatalf("rows %v %v", pyjson.Plain(rows), err)
		}
		ids, _ := rowValue(rows[0], "_lineage_ids").([]any)
		if !pyjson.Equal(sortedAny(ids), []any{"head", "owner"}) {
			t.Fatalf("lineage ids %v", ids)
		}
		if n, err := db.SessionCount(ctx, SessionCountOptions{ExcludeChildren: true}); err != nil || n != 1 {
			t.Fatalf("count %d %v", n, err)
		}
		bySource, err := db.SessionCountBySource(ctx, false, false, true)
		if err != nil || !pyjson.Equal(bySource, pyjson.ObjectOf("cli", int64(1))) {
			t.Fatalf("by source %v %v", pyjson.Plain(bySource), err)
		}
	}
	if n := receiptScans(&mu, &statements); n != 1 {
		t.Fatalf("receipt scans %d", n)
	}
}

// Sorted copy of string items for set comparison
func sortedAny(items []any) []any {
	out := append([]any{}, items...)
	sort.Slice(out, func(i, j int) bool { return pyStr(out[i]) < pyStr(out[j]) })
	return out
}

func TestCommittedWriterChangesRefreshThePooledReader(t *testing.T) {
	for _, mutation := range []string{"extend", "retire", "end_reason", "delete_head"} {
		t.Run(mutation, func(t *testing.T) {
			ctx := context.Background()
			db := lineageCacheDB(t)
			if ids := lineageListIDs(t, db, ListSessionsRichOptions{}); !reflect.DeepEqual(ids, []string{"head"}) {
				t.Fatalf("initial ids %v", ids)
			}
			var expected []string
			switch mutation {
			case "extend":
				readSession(t, db, NewSession{ID: "tail", Source: "cli"})
				lineageExec(t, db, "UPDATE sessions SET end_reason='session_reset' WHERE id='head'")
				lineageExec(t, db, "UPDATE state_meta SET value=? WHERE key=?", lineageReceipt(t, "owner", "owner", "head", "tail"), LocalPolicyPrefix+"owner")
				expected = []string{"tail"}
			case "retire":
				lineageExec(t, db, "DELETE FROM state_meta WHERE key=?", LocalPolicyPrefix+"owner")
				expected = []string{"head", "owner"}
			case "end_reason":
				lineageExec(t, db, "UPDATE sessions SET end_reason='compression' WHERE id='owner'")
				expected = []string{"head", "owner"}
			default:
				lineageExec(t, db, "DELETE FROM sessions WHERE id='head'")
				expected = []string{}
			}
			if ids := sortedIDsOf(lineageListIDs(t, db, ListSessionsRichOptions{})); !reflect.DeepEqual(ids, expected) {
				t.Fatalf("%s listed %v, want %v", mutation, ids, expected)
			}
			if n, err := db.SessionCount(ctx, SessionCountOptions{ExcludeChildren: true}); err != nil || n != len(expected) {
				t.Fatalf("%s count %d %v", mutation, n, err)
			}
		})
	}
}

func TestSameConnectionTransactionsDoNotPublishIntermediateResults(t *testing.T) {
	for _, finish := range []string{"commit", "rollback"} {
		t.Run(finish, func(t *testing.T) {
			ctx := context.Background()
			db := lineageCacheDB(t)
			conn := db.conn
			before := lineageIndexOf(t, db, conn)
			execAll(t, conn, "BEGIN")
			if _, err := conn.ExecContext(ctx, "DELETE FROM state_meta WHERE key=?", LocalPolicyPrefix+"owner"); err != nil {
				t.Fatal(err)
			}
			requireLineageIndex(t, lineageIndexOf(t, db, conn), emptyLineageIndex(), "inside the transaction")
			if err := endTransaction(ctx, conn, strings.ToUpper(finish)); err != nil {
				t.Fatal(err)
			}
			expected := before
			if finish == "commit" {
				expected = emptyLineageIndex()
			}
			requireLineageIndex(t, lineageIndexOf(t, db, conn), expected, "after "+finish)
			if _, err := conn.ExecContext(ctx, "INSERT OR REPLACE INTO state_meta(key,value) VALUES(?,?)",
				LocalPolicyPrefix+"owner", lineageReceipt(t, "owner", "owner", "head")); err != nil {
				t.Fatal(err)
			}
			if err := endTransaction(ctx, conn, "COMMIT"); err != nil {
				t.Fatal(err)
			}
			requireLineageIndex(t, lineageIndexOf(t, db, conn), before, "after the autocommit write")
		})
	}
}

func TestReadSnapshotDoesNotOutliveItsTransaction(t *testing.T) {
	ctx := context.Background()
	db := lineageCacheDB(t)
	err := db.readCtx(ctx, true, func(reader SQLiteConn) error {
		before := lineageIndexOf(t, db, reader)
		execAll(t, reader, "BEGIN")
		requireLineageIndex(t, lineageIndexOf(t, db, reader), before, "inside the read transaction")
		lineageExec(t, db, "DELETE FROM state_meta WHERE key=?", LocalPolicyPrefix+"owner")
		requireLineageIndex(t, lineageIndexOf(t, db, reader), before, "snapshot after the writer commit")
		execAll(t, reader, "ROLLBACK")
		requireLineageIndex(t, lineageIndexOf(t, db, reader), emptyLineageIndex(), "after rollback")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCommitDuringProjectionIsNotCachedAsAStableResult(t *testing.T) {
	ctx := context.Background()
	db := lineageCacheDB(t)
	var mu sync.Mutex
	var statements []tracedStatement
	changed := false
	err := db.readCtx(ctx, true, func(reader SQLiteConn) error {
		traced := &statementTracer{SQLiteConn: reader, mu: &mu, log: &statements}
		traced.before = func(query string) {
			if !changed && strings.Contains(query, "SELECT id,end_reason") {
				changed = true
				lineageExec(t, db, "DELETE FROM state_meta WHERE key=?", LocalPolicyPrefix+"owner")
			}
		}
		lineageIndexOf(t, db, traced)
		if !changed {
			t.Fatal("the end_reason read never ran")
		}
		requireLineageIndex(t, lineageIndexOf(t, db, traced), emptyLineageIndex(), "second projection")
		requireLineageIndex(t, lineageIndexOf(t, db, traced), emptyLineageIndex(), "third projection")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := receiptScans(&mu, &statements); n != 2 {
		t.Fatalf("receipt scans %d", n)
	}
}

func TestCallersCannotMutateTheCachedProjection(t *testing.T) {
	ctx := context.Background()
	db := lineageCacheDB(t)
	err := db.readCtx(ctx, true, func(reader SQLiteConn) error {
		value := lineageIndexOf(t, db, reader)
		clear(value.superseded)
		head := value.heads["head"]
		head.lineage = append(head.lineage, "foreign-session")
		value.heads["head"] = head
		clear(value.heads)
		requireLineageIndex(t, lineageIndexOf(t, db, reader), fixtureLineageIndex(), "after caller mutation")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestConnectionChurnIsBoundedAndOtherDatabasesStayIsolated(t *testing.T) {
	ctx := context.Background()
	db := lineageCacheDB(t)
	for range 12 {
		conn, err := ConnectSQLite("file:"+db.path+"?mode=ro", SQLiteConfig{Timeout: 5, URI: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		if value := lineageIndexOf(t, db, conn); !reflect.DeepEqual(value.superseded, map[string]bool{"owner": true}) {
			t.Fatalf("superseded %v", value.superseded)
		}
	}
	db.localLineage.mu.Lock()
	retained := len(db.localLineage.entries)
	db.localLineage.mu.Unlock()
	if retained > 8 {
		t.Fatalf("cache retained %d connections", retained)
	}
	other := usageOpen(t, filepath.Join(t.TempDir(), "other.db"))
	err := other.readCtx(ctx, true, func(conn SQLiteConn) error {
		requireLineageIndex(t, lineageIndexOf(t, other, conn), emptyLineageIndex(), "other database")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentListsKeepTheSameProjection(t *testing.T) {
	ctx := context.Background()
	db := lineageCacheDB(t)
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([][]string, 6)
	errs := make([]error, 6)
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 5 {
				rows, err := db.ListSessionsRich(ctx, ListSessionsRichOptions{})
				if err != nil {
					errs[i] = err
					return
				}
				for _, row := range rows {
					results[i] = append(results[i], pyStr(rowValue(row, "id")))
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	for i := range 6 {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if !reflect.DeepEqual(results[i], []string{"head", "head", "head", "head", "head"}) {
			t.Fatalf("reader %d listed %v", i, results[i])
		}
	}
}

func TestReadOnlyLegacyDatabaseNeedsNoSchemaChange(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	conn := openRaw(t, path)
	execAll(t, conn, "CREATE TABLE sessions(id TEXT PRIMARY KEY,end_reason TEXT)",
		"CREATE TABLE state_meta(key TEXT PRIMARY KEY,value TEXT)",
		"INSERT INTO sessions VALUES('old','session_reset')", "INSERT INTO sessions VALUES('new',NULL)")
	if _, err := conn.ExecContext(ctx, "INSERT INTO state_meta VALUES(?,?)", LocalPolicyPrefix+"old", lineageReceipt(t, "old", "old", "new")); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	db := mustSessionDB(t, path, true)
	expected := localLineageIndexValue{superseded: map[string]bool{"old": true},
		heads: map[string]localLineageHead{"new": {owner: "old", lineage: []string{"old", "new"}}}}
	err = db.readCtx(ctx, true, func(reader SQLiteConn) error {
		requireLineageIndex(t, lineageIndexOf(t, db, reader), expected, "first legacy read")
		requireLineageIndex(t, lineageIndexOf(t, db, reader), expected, "second legacy read")
		return nil
	})
	if cerr := db.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("read only open changed the legacy file (%d -> %d bytes) %v", len(before), len(after), err)
	}
}
