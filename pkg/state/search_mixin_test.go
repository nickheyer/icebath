package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type fakeSearchHost struct {
	fakeFTSHost
	reader     SQLiteConn
	ro         bool
	holderScan func() ([]Holder, error)
	beforeTx   func(conn SQLiteConn)
	afterWrite func()
}

func (h *fakeSearchHost) readOnly() bool                { return h.ro }
func (h *fakeSearchHost) ftsMergeMaxPagesPerIndex() int { return 500 }
func (h *fakeSearchHost) ftsMergeCommandsPerPass() int  { return 4 }

func (h *fakeSearchHost) foreignStateDBHolders() ([]Holder, error) {
	if h.holderScan == nil {
		return nil, nil
	}
	return h.holderScan()
}

func (h *fakeSearchHost) executeWrite(ctx context.Context, fn func(conn SQLiteConn) error, patienceS *float64) error {
	err := func() error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.beforeTx != nil {
			h.beforeTx(h.conn)
		}
		if _, err := h.conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return err
		}
		if err := fn(h.conn); err != nil {
			endTransaction(ctx, h.conn, "ROLLBACK")
			return err
		}
		return endTransaction(ctx, h.conn, "COMMIT")
	}()
	if err == nil && h.afterWrite != nil {
		h.afterWrite()
	}
	return err
}

func (h *fakeSearchHost) readCtx(ctx context.Context, reopen bool, fn func(conn SQLiteConn) error) error {
	return fn(h.reader)
}

func (h *fakeSearchHost) readAll(ctx context.Context, query string, args ...any) ([]*pyjson.Object, error) {
	return queryObjects(ctx, h.reader, query, args...)
}

func (h *fakeSearchHost) readOne(ctx context.Context, query string, args ...any) (*pyjson.Object, error) {
	rows, err := queryObjects(ctx, h.reader, query, args...)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

func (h *fakeSearchHost) decodeContent(content any) any {
	if text, ok := content.(string); ok && strings.HasPrefix(text, "\x00json:") {
		decoded, err := pyjson.Loads(strings.TrimPrefix(text, "\x00json:"))
		if err == nil {
			return decoded
		}
	}
	return content
}

func (h *fakeSearchHost) getMeta(ctx context.Context, key string) (string, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	row, err := fetchOne(ctx, h.conn, "SELECT value FROM state_meta WHERE key = ?", key)
	if err != nil || row == nil {
		return "", false, err
	}
	return errText(row[0]), true, nil
}

func (h *fakeSearchHost) setMeta(ctx context.Context, key, value string, cursor SQLiteConn) error {
	upsert := "INSERT INTO state_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value"
	if cursor != nil {
		_, err := cursor.ExecContext(ctx, upsert, key, value)
		return err
	}
	return h.executeWrite(ctx, func(conn SQLiteConn) error {
		_, err := conn.ExecContext(ctx, upsert, key, value)
		return err
	}, nil)
}

// Search mixin over a WAL store at path with a separate reader, FTS5 and trigram available
func searchFixture(t *testing.T, path string) (*sessionSearch, *fakeSearchHost) {
	t.Helper()
	open := func() SQLiteConn {
		conn, err := ConnectSQLite(path, SQLiteConfig{Timeout: 5})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	writer := open()
	mustExecConn(t, writer, "PRAGMA journal_mode=WAL")
	host := &fakeSearchHost{fakeFTSHost: fakeFTSHost{path: path, conn: writer}, reader: open()}
	fts := newSessionFTSSetup(host)
	fts.ftsEnabled.Store(true)
	fts.trigramAvailable.Store(true)
	return newSessionSearch(host, fts), host
}

const gapAfter, gapIDs = 600, 100_000

// Legacy inline FTS store whose message ids jump by gapIDs after the first gapAfter rows
func legacyInlineStore(t *testing.T, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	conn, err := ConnectSQLite(path, SQLiteConfig{Timeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx := context.Background()
	for _, script := range []string{SchemaSQL, LegacyFTSSQL, LegacyFTSTrigramSQL} {
		if err := executeScript(ctx, conn, script); err != nil {
			t.Fatal(err)
		}
	}
	mustExecConn(t, conn, "BEGIN")
	mustExecConn(t, conn, fmt.Sprintf("INSERT INTO sessions (id, source, started_at) VALUES ('s1', 'cli', %f)", float64(time.Now().Unix())))
	for i := range n {
		id := i + 1
		if i >= gapAfter {
			id += gapIDs
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO messages (id, session_id, timestamp, role, content) VALUES (?, 's1', ?, 'user', ?)",
			id, float64(time.Now().Unix()), fmt.Sprintf("legacy payload %d zebra quokka", id)); err != nil {
			t.Fatal(err)
		}
	}
	mustExecConn(t, conn, "COMMIT")
	return path
}

func legacySearch(t *testing.T) (*sessionSearch, *fakeSearchHost) {
	t.Helper()
	s, host := searchFixture(t, legacyInlineStore(t, 1200))
	if ok, err := s.FTSOptimizeAvailable(context.Background()); err != nil || !ok {
		t.Fatalf("a legacy inline store offers optimize: %v %v", ok, err)
	}
	return s, host
}

func objJSON(t *testing.T, o *pyjson.Object) string {
	t.Helper()
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func objInt(o *pyjson.Object, key string) int64 {
	v, _ := o.Get(key)
	n, _ := pyint.Of(v)
	return n.Int64()
}

func objStr(o *pyjson.Object, key string) string {
	v, _ := o.Get(key)
	s, _ := v.(string)
	return s
}

func objBool(o *pyjson.Object, key string) bool {
	v, _ := o.Get(key)
	b, _ := v.(bool)
	return b
}

func ftsMatchCount(t *testing.T, conn SQLiteConn, table, query string) int64 {
	t.Helper()
	row, err := fetchOne(context.Background(), conn, fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s MATCH ?", table, table), query)
	if err != nil {
		t.Fatal(err)
	}
	return row[0].(int64)
}

func pinSearchSleep(t *testing.T) *[]float64 {
	t.Helper()
	var mu sync.Mutex
	slept := []float64{}
	saved := searchSleep
	searchSleep = func(s float64) { mu.Lock(); slept = append(slept, s); mu.Unlock() }
	t.Cleanup(func() { searchSleep = saved })
	return &slept
}

func pinChunkBounds(t *testing.T, lo, hi int) {
	t.Helper()
	savedLo, savedHi := ftsRebuildChunkMinRows, ftsRebuildChunkMaxRows
	ftsRebuildChunkMinRows, ftsRebuildChunkMaxRows = lo, hi
	t.Cleanup(func() { ftsRebuildChunkMinRows, ftsRebuildChunkMaxRows = savedLo, savedHi })
}

func TestSearchMessageFieldsCanonicalOrder(t *testing.T) {
	got, err := searchMessageFields(nil)
	if got != nil || err != nil {
		t.Fatalf("None selects every field: %v %v", got, err)
	}
	got, err = searchMessageFields([]string{"context", "id", "snippet", "id"})
	if err != nil || strings.Join(got, ",") != "id,snippet,context" {
		t.Fatalf("canonical order: %v %v", got, err)
	}
	_, err = searchMessageFields([]string{"zeta", "id", "alpha"})
	if err == nil || err.Error() != "unknown search result field(s): alpha, zeta" || err.(*pyint.Error).Kind != "ValueError" {
		t.Fatalf("unknown fields: %v", err)
	}
}

func TestRebuildStatusPayload(t *testing.T) {
	ctx := context.Background()
	conn, path := schemaConn(t)
	s, host := searchFixture(t, path)
	conn.Close()
	set := func(k, v string) {
		if err := host.setMeta(ctx, k, v, nil); err != nil {
			t.Fatal(err)
		}
	}
	if st, err := s.FTSRebuildStatus(ctx); st != nil || err != nil {
		t.Fatalf("no markers: %v %v", st, err)
	}
	set("fts_rebuild_high_water", "1200")
	set("fts_rebuild_progress", "500")
	st, err := s.FTSRebuildStatus(ctx)
	if err != nil || objJSON(t, st) != `{"pending":true,"total":1200,"indexed":500,"percent":41}` {
		t.Fatalf("status: %v %v", st, err)
	}
	set("fts_rebuild_progress", "")
	if st, _ := s.FTSRebuildStatus(ctx); objJSON(t, st) != `{"pending":true,"total":1200,"indexed":0,"percent":0}` {
		t.Fatalf("blank progress reads zero: %v", objJSON(t, st))
	}
	set("fts_rebuild_progress", "5000")
	if st, _ := s.FTSRebuildStatus(ctx); objInt(st, "percent") != 100 {
		t.Fatalf("percent caps at 100: %v", objJSON(t, st))
	}
	set("fts_cjk_rebuild_high_water", "0")
	if st, err := s.FTSCJKRebuildStatus(ctx); st != nil || err != nil {
		t.Fatalf("a zero high water is not pending: %v %v", st, err)
	}
	set("fts_cjk_rebuild_high_water", "x")
	if _, err := s.FTSCJKRebuildStatus(ctx); err == nil || !strings.Contains(err.Error(), "invalid literal for int()") {
		t.Fatalf("a corrupt marker raises int's ValueError: %v", err)
	}
}

func TestOptimizeLegacyStoreReportsProgressInEveryPhase(t *testing.T) {
	for _, vacuumFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("vacuum_fails=%v", vacuumFails), func(t *testing.T) {
			ctx := context.Background()
			s, host := legacySearch(t)
			s.vacuumTickS = 0.01
			var mu sync.Mutex
			var events []*pyjson.Object
			inVacuum, ticked := make(chan struct{}), make(chan struct{})
			var inOnce, tickOnce sync.Once
			progress := func(info *pyjson.Object) {
				mu.Lock()
				events = append(events, info)
				mu.Unlock()
				if objStr(info, "phase") == "vacuum" {
					select {
					case <-inVacuum:
						tickOnce.Do(func() { close(ticked) })
					default:
					}
				}
			}
			host.conn = &holdVacuumConn{SQLiteConn: host.conn, hold: func() error {
				inOnce.Do(func() { close(inVacuum) })
				select {
				case <-ticked:
				case <-time.After(10 * time.Second):
				}
				if vacuumFails {
					return &SQLiteError{Class: SQLiteOperationalError, Msg: "interrupted", Code: 9, HasCode: true}
				}
				return nil
			}}
			result, err := s.OptimizeFTSStorage(ctx, progress, true)
			if err != nil {
				t.Fatal(err)
			}
			if want := fmt.Sprintf(`{"ok":true,"vacuumed":%v}`, !vacuumFails); objJSON(t, result) != want {
				t.Fatalf("result %s want %s", objJSON(t, result), want)
			}
			select {
			case <-ticked:
			default:
				t.Fatal("the vacuum ticker reported while VACUUM ran")
			}
			mu.Lock()
			seen := len(events)
			mu.Unlock()
			time.Sleep(50 * time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			if len(events) != seen {
				t.Fatal("the ticker is stopped and joined before optimize returns")
			}
			if objStr(events[len(events)-1], "phase") != "done" {
				t.Fatalf("last event %s", objJSON(t, events[len(events)-1]))
			}
			var backfill, vacuum, teardown, docsize []*pyjson.Object
			for _, e := range events {
				switch objStr(e, "phase") {
				case "backfill":
					backfill = append(backfill, e)
				case "vacuum":
					vacuum = append(vacuum, e)
				case "teardown":
					teardown = append(teardown, e)
					if strings.HasSuffix(objStr(e, "table"), "messages_fts_docsize") {
						docsize = append(docsize, e)
					}
				}
			}
			for _, e := range backfill {
				if objInt(e, "total") != 1200 {
					t.Fatalf("backfill counts messages: %s", objJSON(t, e))
				}
			}
			if objInt(backfill[0], "indexed") != 0 || objInt(backfill[1], "indexed") != 500 || objInt(backfill[1], "percent") != 41 {
				t.Fatalf("the id gap still reads 500 of 1200: %s %s", objJSON(t, backfill[0]), objJSON(t, backfill[1]))
			}
			if size, est := objInt(vacuum[0], "size_bytes"), objInt(vacuum[0], "estimated_bytes"); !(size >= est && est > 0) {
				t.Fatalf("vacuum sizes %d %d", size, est)
			}
			if objInt(docsize[0], "total") != 1200 || objInt(docsize[0], "indexed") != 0 ||
				objInt(docsize[len(docsize)-1], "indexed") != 1200 || objInt(docsize[len(docsize)-1], "percent") != 100 {
				t.Fatalf("docsize drain is visible as rows: %s %s", objJSON(t, docsize[0]), objJSON(t, docsize[len(docsize)-1]))
			}
			count := objInt(teardown[0], "table_count")
			trigramData := false
			for _, e := range teardown {
				if idx := objInt(e, "table_index"); idx < 1 || idx > count {
					t.Fatalf("table index %d of %d", idx, count)
				}
				if strings.HasSuffix(objStr(e, "table"), "messages_fts_trigram_data") && objInt(e, "total") > 0 {
					trigramData = true
				}
			}
			if count != 10 || !trigramData {
				t.Fatalf("both legacy indexes are reclaimed: count=%d trigram=%v", count, trigramData)
			}
			if n := ftsMatchCount(t, host.reader, "messages_fts", "quokka"); n != 1200 {
				t.Fatalf("the rebuilt index serves every message: %d", n)
			}
			if trash := tableNames(t, host.reader, "fts_v22_trash_%"); len(trash) != 0 {
				t.Fatalf("trash left: %v", trash)
			}
			if v, ok := metaValue(t, host.reader, "fts_storage_version"); !ok || v != "3" {
				t.Fatalf("layout stamp %q %v", v, ok)
			}
		})
	}
}

type holdVacuumConn struct {
	SQLiteConn
	hold func() error
}

func (c *holdVacuumConn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.TrimSpace(query) == "VACUUM" {
		if err := c.hold(); err != nil {
			return nil, err
		}
	}
	return c.SQLiteConn.ExecContext(ctx, query, args...)
}

func (c *holdVacuumConn) Raw(f func(driverConn any) error) error {
	inner := c.SQLiteConn
	if tc, ok := inner.(*TrackedConn); ok {
		inner = tc.SQLiteConn
	}
	return inner.(rawConner).Raw(f)
}

func TestOptimizeChunksByRowsAcrossIDGapsWithoutSleeping(t *testing.T) {
	ctx := context.Background()
	slept := pinSearchSleep(t)
	pinChunkBounds(t, 500, 500)
	s, host := legacySearch(t)
	markers := []int64{}
	host.afterWrite = func() {
		v, ok := metaValue(t, host.reader, "fts_rebuild_progress")
		if !ok {
			return
		}
		var n int64
		fmt.Sscan(v, &n)
		if len(markers) == 0 || markers[len(markers)-1] != n {
			markers = append(markers, n)
		}
	}
	result, err := s.OptimizeFTSStorage(ctx, nil, false)
	if err != nil || !objBool(result, "ok") {
		t.Fatalf("%v %v", result, err)
	}
	if len(*slept) != 0 {
		t.Fatalf("a foreground run never sleeps: %v", *slept)
	}
	var windows []int64
	for i := 1; i < len(markers); i++ {
		row, _ := fetchOne(ctx, host.reader, "SELECT COUNT(*) FROM messages WHERE id > ? AND id <= ?", markers[i-1], markers[i])
		windows = append(windows, row[0].(int64))
	}
	if fmt.Sprint(windows) != "[500 500 200]" {
		t.Fatalf("500 row chunks across the id gap: markers %v windows %v", markers, windows)
	}
}

func TestOptimizeChunkThatKeepsFailingEndsTheRunAndRerunResumes(t *testing.T) {
	ctx := context.Background()
	pinSearchSleep(t)
	s, host := legacySearch(t)
	attempts := 0
	limited := false
	host.beforeTx = func(conn SQLiteConn) {
		if _, ok := metaValue(t, conn, "fts_rebuild_progress"); !ok {
			return
		}
		attempts++
		if !limited {
			limited = true
			row, _ := fetchOne(ctx, conn, "PRAGMA page_count")
			mustExecConn(t, conn, fmt.Sprintf("PRAGMA max_page_count = %d", row[0].(int64)+5))
		}
	}
	_, err := s.OptimizeFTSStorage(ctx, nil, false)
	if err == nil || !isOperationalError(err) || !strings.Contains(err.Error(), "full") {
		t.Fatalf("a disk full chunk ends the run: %v", err)
	}
	if attempts != optimizeChunkAttempts {
		t.Fatalf("the chunk was attempted %d times", attempts)
	}
	host.beforeTx = nil
	mustExecConn(t, host.conn, "PRAGMA max_page_count = 1073741823")
	result, err := s.OptimizeFTSStorage(ctx, nil, false)
	if err != nil || !objBool(result, "ok") {
		t.Fatalf("the rerun resumes: %v %v", result, err)
	}
	if n := ftsMatchCount(t, host.reader, "messages_fts", "quokka"); n != 1200 {
		t.Fatalf("every message indexed after the resume: %d", n)
	}
}

func TestOptimizePacesOnceAWriterOpensTheStore(t *testing.T) {
	ctx := context.Background()
	slept := pinSearchSleep(t)
	pinChunkBounds(t, ftsRebuildChunkMinRows, 100)
	s, host := legacySearch(t)
	s.holderScanIntervalS = 0.01
	var scans atomic.Int32
	firstStep, writerOpened := make(chan struct{}), make(chan struct{})
	var firstStepOnce sync.Once
	host.holderScan = func() ([]Holder, error) {
		n := scans.Add(1)
		if n == 1 {
			return nil, nil
		}
		if n == 2 {
			<-firstStep
		}
		if n == 3 {
			close(writerOpened)
		}
		return []Holder{{4242, host.path}}, nil
	}
	host.afterWrite = func() {
		if scans.Load() >= 1 {
			firstStepOnce.Do(func() { close(firstStep) })
			select {
			case <-writerOpened:
			case <-time.After(10 * time.Second):
				t.Error("the holder watch never rescanned")
			}
		}
	}
	var events []*pyjson.Object
	result, err := s.OptimizeFTSStorage(ctx, func(o *pyjson.Object) { events = append(events, o) }, true)
	if err != nil || objJSON(t, result) != `{"ok":true,"vacuumed":false,"vacuum_skipped":"in_use"}` {
		t.Fatalf("a live writer skips VACUUM: %v %v", result, err)
	}
	chunks := events[:len(events)-1]
	teardown := 0
	for i, e := range chunks {
		if objStr(e, "phase") == "vacuum" {
			t.Fatal("no vacuum phase while a writer holds the store")
		}
		if objStr(e, "phase") == "teardown" {
			teardown++
		}
		if objBool(e, "paced") != (i > 0) {
			t.Fatalf("event %d paced=%v: %s", i, objBool(e, "paced"), objJSON(t, e))
		}
	}
	minSlept := 1e9
	for _, d := range *slept {
		minSlept = min(minSlept, d)
	}
	if teardown == 0 || len(*slept) < teardown || minSlept < ftsRebuildMinPause {
		t.Fatalf("every paced chunk yields the lock: teardown=%d slept=%v", teardown, *slept)
	}
}

func TestOptimizeRefusalsAndHolderScanFailure(t *testing.T) {
	ctx := context.Background()
	s, host := legacySearch(t)
	host.ro = true
	if r, _ := s.OptimizeFTSStorage(ctx, nil, true); objJSON(t, r) != `{"ok":false,"reason":"read_only"}` {
		t.Fatalf("read only: %s", objJSON(t, r))
	}
	if ok, _ := s.FTSOptimizeAvailable(ctx); ok {
		t.Fatal("a read only handle offers no optimize")
	}
	host.ro = false
	s.fts.ftsEnabled.Store(false)
	if r, _ := s.OptimizeFTSStorage(ctx, nil, true); objJSON(t, r) != `{"ok":false,"reason":"fts5_unavailable"}` {
		t.Fatalf("no fts5: %s", objJSON(t, r))
	}
	s.fts.ftsEnabled.Store(true)
	scanErr := errors.New("procfs vanished")
	host.holderScan = func() ([]Holder, error) { return nil, scanErr }
	if _, err := s.OptimizeFTSStorage(ctx, nil, true); !errors.Is(err, scanErr) {
		t.Fatalf("a failed first holder scan raises out of optimize: %v", err)
	}
}

func TestOptimizeDriveRetriesOnlyOperationalErrors(t *testing.T) {
	slept := pinSearchSleep(t)
	conn, path := schemaConn(t)
	conn.Close()
	s, _ := searchFixture(t, path)
	w, err := newHolderWatch(func() (bool, error) { return false, nil }, 60).enter()
	if err != nil {
		t.Fatal(err)
	}
	defer w.exit()
	locked := &SQLiteError{Class: SQLiteOperationalError, Msg: "database is locked", Code: 5, HasCode: true}
	calls := 0
	err = s.optimizeDrive(func() (bool, error) {
		calls++
		if calls <= 2 {
			return false, locked
		}
		return calls < 4, nil
	}, nil, func(*pyjson.Object) {}, w)
	if err != nil || calls != 4 || fmt.Sprint(*slept) != "[0.25 0.5]" {
		t.Fatalf("two failures retried with backoff: err=%v calls=%d slept=%v", err, calls, *slept)
	}
	calls = 0
	if err := s.optimizeDrive(func() (bool, error) { calls++; return false, locked }, nil, func(*pyjson.Object) {}, w); err != locked || calls != 3 {
		t.Fatalf("the third failure ends the run: %v %d", err, calls)
	}
	corrupt := &SQLiteError{Class: SQLiteDatabaseError, Msg: "database disk image is malformed", Code: 11, HasCode: true}
	calls = 0
	if err := s.optimizeDrive(func() (bool, error) { calls++; return false, corrupt }, nil, func(*pyjson.Object) {}, w); err != corrupt || calls != 1 {
		t.Fatalf("a non operational error is never retried: %v %d", err, calls)
	}
}

func TestIncrementalMergeAppliesTheUsermergeFloorOnce(t *testing.T) {
	ctx := context.Background()
	conn, path := schemaConn(t)
	mustExecConn(t, conn, "INSERT INTO sessions (id, source, started_at) VALUES ('s1', 'cli', 1.0)")
	for i := range 20 {
		addMessage(t, conn, "user", fmt.Sprintf("segment %d walrus", i), "")
	}
	conn.Close()
	s, host := searchFixture(t, path)
	n, err := s.mergeFTSIncrementally(ctx, 500, nil)
	if err != nil || n < 2 || !s.ftsUsermergeFloorApplied {
		t.Fatalf("merge ran %d commands: %v", n, err)
	}
	row, err := fetchOne(ctx, host.reader, "SELECT v FROM messages_fts_config WHERE k = 'usermerge'")
	if err != nil || row == nil || row[0].(int64) != 2 {
		t.Fatalf("usermerge persisted at 2: %v %v", row, err)
	}
	zero := 0
	if _, err := s.mergeFTSIncrementally(ctx, 500, &zero); err == nil || err.Error() != "max_commands must be greater than zero" {
		t.Fatalf("max_commands validated: %v", err)
	}
	if _, err := s.mergeFTSIncrementally(ctx, 0, nil); err == nil || err.Error() != "max_pages must be greater than zero" {
		t.Fatalf("max_pages validated: %v", err)
	}
	logs := captureSlog(t)
	host.conn = nil
	s.tryIncrementalMergeFTS(ctx)
	if !strings.Contains(logs.String(), "FTS incremental merge failed after commit") {
		t.Fatalf("a failed merge after commit is logged, never raised: %s", logs.String())
	}
}

func TestRepairBookkeepingClaimsABackfillForAnEmptyIndex(t *testing.T) {
	ctx := context.Background()
	conn, path := schemaConn(t)
	mustExecConn(t, conn, "INSERT INTO sessions (id, source, started_at) VALUES ('s1', 'cli', 1.0)")
	for i := range 30 {
		addMessage(t, conn, "user", fmt.Sprintf("narwhal %d", i), "")
	}
	conn.Close()
	s, host := searchFixture(t, path)
	if ok, _ := s.FTSOptimizeAvailable(ctx); ok {
		t.Fatal("a current v23 store offers no optimize")
	}
	mustExecConn(t, host.conn, "INSERT INTO messages_fts(messages_fts) VALUES('delete-all')")
	mustExecConn(t, host.conn, "INSERT INTO state_meta (key, value) VALUES ('fts_storage_version', '3')")
	if ok, _ := s.FTSOptimizeAvailable(ctx); !ok {
		t.Fatal("an empty index over messages offers optimize")
	}
	if err := s.repairOptimizeBookkeeping(ctx); err != nil {
		t.Fatal(err)
	}
	if hw, _ := metaValue(t, host.reader, "fts_rebuild_high_water"); hw != "30" {
		t.Fatalf("full backfill claimed at %q", hw)
	}
	if _, ok := metaValue(t, host.reader, "fts_storage_version"); ok {
		t.Fatal("the layout stamp is cleared until the backfill settles")
	}
	mustExecConn(t, host.conn, "DELETE FROM state_meta WHERE key = 'fts_rebuild_progress'")
	mustExecConn(t, host.conn, "INSERT INTO messages_fts(rowid, content, tool_name, tool_calls) VALUES (1, 'narwhal 0', NULL, NULL)")
	if err := s.repairOptimizeBookkeeping(ctx); err != nil {
		t.Fatal(err)
	}
	if p, _ := metaValue(t, host.reader, "fts_rebuild_progress"); p != "0" {
		t.Fatalf("orphan high water reseeded: %q", p)
	}
	if empty, _ := ftsIndexKnownEmpty(ctx, host.reader); !empty {
		t.Fatal("the reseed resets the index to known empty")
	}
	result, err := s.OptimizeFTSStorage(ctx, nil, false)
	if err != nil || objJSON(t, result) != `{"ok":true,"vacuumed":null}` {
		t.Fatalf("optimize settles the claimed backfill: %v %v", result, err)
	}
	if n := ftsMatchCount(t, host.reader, "messages_fts", "narwhal"); n != 30 {
		t.Fatalf("backfill indexed %d", n)
	}
}

func TestOptimizeSettleRefusals(t *testing.T) {
	ctx := context.Background()
	conn, path := schemaConn(t)
	mustExecConn(t, conn, "INSERT INTO sessions (id, source, started_at) VALUES ('s1', 'cli', 1.0)")
	addMessage(t, conn, "user", "otter", "")
	conn.Close()
	s, host := searchFixture(t, path)
	settle := func() string {
		var reason string
		if err := host.executeWrite(ctx, func(c SQLiteConn) error {
			var err error
			reason, err = s.optimizeSettle(ctx, c)
			return err
		}, nil); err != nil {
			t.Fatal(err)
		}
		return reason
	}
	mustExecConn(t, host.conn, "INSERT INTO state_meta (key, value) VALUES ('fts_rebuild_high_water', '1')")
	if r := settle(); r != "backfill_incomplete" {
		t.Fatalf("pending backfill: %q", r)
	}
	mustExecConn(t, host.conn, "DELETE FROM state_meta")
	mustExecConn(t, host.conn, "CREATE TABLE fts_v22_trash_messages_fts_data (id INTEGER PRIMARY KEY, block BLOB)")
	if r := settle(); r != "teardown_incomplete" {
		t.Fatalf("trash left: %q", r)
	}
	mustExecConn(t, host.conn, "DROP TABLE fts_v22_trash_messages_fts_data")
	mustExecConn(t, host.conn, "INSERT INTO messages_fts(messages_fts) VALUES('delete-all')")
	if r := settle(); r != "backfill_incomplete" {
		t.Fatalf("empty index: %q", r)
	}
	mustExecConn(t, host.conn, "INSERT INTO messages_fts(messages_fts) VALUES('rebuild')")
	mustExecConn(t, host.conn, "DELETE FROM schema_version")
	mustExecConn(t, host.conn, "INSERT INTO schema_version (version) VALUES (1)")
	mustExecConn(t, host.conn, "INSERT INTO state_meta (key, value) VALUES ('fts_optimize_available', '1')")
	if r := settle(); r != "" {
		t.Fatalf("a current layout settles: %q", r)
	}
	if v, _ := metaValue(t, host.reader, "fts_storage_version"); v != "3" {
		t.Fatalf("stamp %q", v)
	}
	if _, ok := metaValue(t, host.reader, "fts_optimize_available"); ok {
		t.Fatal("the available flag is cleared")
	}
	if row, _ := fetchOne(ctx, host.reader, "SELECT version FROM schema_version"); row[0].(int64) != SchemaVersion {
		t.Fatalf("schema version advanced: %v", row)
	}
}

func TestCJKBackfillServesAndAStaleIndexResetsFromScratch(t *testing.T) {
	ctx := context.Background()
	conn, path := schemaConn(t)
	mustExecConn(t, conn, "INSERT INTO sessions (id, source, started_at) VALUES ('s1', 'cli', 1.0)")
	addMessage(t, conn, "user", "한국어 검색 테스트", "")
	addMessage(t, conn, "tool", "도구 결과 한국어", "terminal")
	addMessage(t, conn, "assistant", "한국어 답변", "")
	conn.Close()
	s, host := searchFixture(t, path)
	for _, c := range []SQLiteConn{host.conn, host.reader} {
		if !LoadFTS5CJKExtension(ctx, c) {
			t.Fatal("the built in CJK tokenizer registers")
		}
	}
	if ok, _ := s.FTSCJKRebuildStep(ctx, nil); ok {
		t.Fatal("no CJK step before the tokenizer is loaded")
	}
	s.fts.ftsCJKLoaded.Store(true)
	if err := s.ensureCJKSchemaCommitted(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.FTSOptimizeAvailable(ctx); !ok {
		t.Fatal("a pending CJK backfill offers optimize on a capable host")
	}
	st, err := s.FTSCJKRebuildStatus(ctx)
	if err != nil || objInt(st, "total") != 3 {
		t.Fatalf("cjk backfill pending: %v %v", st, err)
	}
	bf := &backfillProgress{phase: "backfill_cjk"}
	for {
		more, err := s.FTSCJKRebuildStep(ctx, bf)
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
	}
	if !s.fts.ftsCJKAvailable.Load() || bf.indexed != 3 {
		t.Fatalf("backfill finished: available=%v indexed=%d", s.fts.ftsCJKAvailable.Load(), bf.indexed)
	}
	if n := ftsMatchCount(t, host.reader, "messages_fts_cjk", "한국"); n != 2 {
		t.Fatalf("tool rows stay out of the CJK index: %d", n)
	}
	mustExecConn(t, host.conn, "INSERT INTO state_meta (key, value) VALUES ('fts_cjk_stale', '1')")
	if err := s.ftsCJKResetIfStale(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := metaValue(t, host.reader, "fts_cjk_stale"); ok {
		t.Fatal("the breadcrumb is cleared")
	}
	if hw, ok := metaValue(t, host.reader, "fts_cjk_rebuild_high_water"); !ok || hw != "3" {
		t.Fatalf("the recreated index carries fresh markers: %q %v", hw, ok)
	}
	if n := ftsMatchCount(t, host.reader, "messages_fts_cjk", "한국"); n != 0 {
		t.Fatalf("the recreated index starts empty: %d", n)
	}
}
