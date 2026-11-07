package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Write contention budgets and retry jitter in seconds, vars a test may tune like the class attributes
var (
	sessionWritePatienceS           = 20.0
	sessionTranscriptWritePatienceS = 60.0
	sessionActivityWritePatienceS   = 0.5
	sessionCompressionBusyWaitS     = 5.0
	sessionWriteRetryMinS           = 0.020
	sessionWriteRetryMaxS           = 0.150
	sessionWriteRetrySlowAfterS     = 2.0
	sessionWriteRetrySlowMinS       = 0.250
	sessionWriteRetrySlowMaxS       = 1.000
)

// Checkpoint and merge pacing, import caps, token writer idle window and quarantine lock wait
const (
	sessionCheckpointEveryNWrites      = 50
	sessionFTSMergeMaxPagesPerIndex    = 500
	sessionFTSMergeCommandsPerPass     = 4
	importMaxSessions                  = 500
	importMaxMessagesPerSession        = 10_000
	importMaxTotalMessages             = 50_000
	importMaxSessionBytes              = 5 * 1024 * 1024
	importMaxTotalBytes                = 25 * 1024 * 1024
	quarantineCrossProcessLockTimeoutS = 5.0
)

// Pause between read only open attempts after a transient disk IO error, a var tests shorten
var readOnlyIOErrRetryBackoffS = 0.05

// Connection opener, pragma setup, schema init and post commit FTS merge pacing, vars tests replace
var (
	sessionConnectTracked              = ConnectTracked
	sessionApplyDatabasePragmas        = ApplyDatabasePragmas
	sessionInitSchema                  = func(ctx context.Context, d *SessionDB) error { return d.initSchema(ctx) }
	sessionFTSMergeEveryNWrites  int64 = 1000
	sessionMergeFTSIncrementally       = (*sessionSearch).mergeFTSIncrementally
)

// State owned producers that join automatic stale open reconciliation
var autoPruneStaleOpenSources = []string{"cli", "cron", "kanban", "acp", "api_server", "subagent", "tool", "recovered"}

// Token delta fields summed, cost fields summed when present and route fields that must match to coalesce
var (
	tokenDeltaSumFields   = []string{"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens", "api_call_count"}
	tokenDeltaCostFields  = []string{"estimated_cost_usd", "actual_cost_usd"}
	tokenDeltaRouteFields = []string{"model", "cost_status", "cost_source", "pricing_version", "billing_provider", "billing_base_url", "billing_mode", "source", "task"}
)

// Columns of a conversation row read for replay
const conversationRowColumns = "id, role, content, tool_call_id, tool_calls, tool_name, effect_disposition, " +
	"finish_reason, reasoning, reasoning_content, reasoning_details, " +
	"codex_reasoning_items, codex_message_items, platform_message_id, observed, " +
	"_compressed_summary, timestamp, token_count, active, api_content, display_kind, display_metadata, message_uid, " +
	"absorbed_message_uids, tool_call_uids, tool_call_uid"

// Connection, read pool, file identity and quarantine state, kept apart from the mixins so a collection cleanup can close it
type sessionCore struct {
	path       string
	isReadOnly bool
	site       string
	openCtx    context.Context

	mu      sync.Mutex
	conn    SQLiteConn
	hasConn atomic.Bool

	messageColumnsMu    sync.Mutex
	messageColumnsCache []string

	localLineage localLineageCache

	poolMu           sync.Mutex
	readPool         []SQLiteConn
	readBudget       *pathReadBudget
	readConnsMu      sync.Mutex
	readConnsClosed  bool
	readOpenFailedAt float64

	walActive  atomic.Bool
	writeCount atomic.Int64

	identityMu          sync.Mutex
	dbFileIdentity      *FileIdentity
	dbFileApplicationID uint32
	dbSidecarIdentity   map[string]FileIdentity

	dbReplaced          atomic.Bool
	dbWALGenerationLost atomic.Bool

	retiredCaptureMu         sync.Mutex
	retiredGenerationCapture string
	retireConnection         func(conn any)
	leaseSeams               turnLeaseSeams
	richRowSeams             richRowSeams
	neverActiveListSeam      func(ctx context.Context, olderThanDays float64) ([]*pyjson.Object, error)
	connectionPinned         bool
	walLockGuard             lockGuardHeld

	corruptMu       sync.Mutex
	dbCorrupt       bool
	dbCorruptReason string

	sharedRegistryOwned atomic.Bool

	tokens tokenWriterState
}

// True while the process wide registry owns this handle
func (d *SessionDB) SharedRegistryOwned() bool { return d.sharedRegistryOwned.Load() }

// SQLite session store with FTS5 search, many pooled readers and one writer connection
type SessionDB struct {
	*sessionCore
	*sessionFTSSetup
	*sessionSchema
	*sessionSearch
	*sessionTitles
}

// Async door onto a SessionDB running every call on its own goroutine
type AsyncSessionDB struct {
	db *SessionDB
}

// One state_meta row of a prefix listing, Value nil for a NULL value
type MetaEntry struct {
	Key   string
	Value any
}

// Opens the session store at dbPath, the default profile store when empty, read only without schema work when asked
func NewSessionDB(ctx context.Context, dbPath string, readOnly bool) (*SessionDB, error) {
	return newSessionDB(ctx, dbPath, readOnly, 2)
}

func newSessionDB(ctx context.Context, dbPath string, readOnly bool, callerSkip int) (*SessionDB, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if dbPath == "" {
		dbPath = defaultDBPath(ctx)
	}
	if err := ensureTestIsolation(dbPath); err != nil {
		return nil, err
	}
	core := &sessionCore{path: dbPath, isReadOnly: readOnly, site: "unknown", openCtx: ctx}
	if pc, _, line, ok := runtime.Caller(callerSkip); ok {
		if fn := runtime.FuncForPC(pc); fn != nil {
			core.site = fn.Name() + ":" + strconv.Itoa(line)
		}
	}
	core.readBudget = readBudgetFor(dbPath)
	d := &SessionDB{sessionCore: core}
	d.sessionFTSSetup = newSessionFTSSetup(d)
	d.sessionSchema = newSessionSchema(d, d.sessionFTSSetup)
	d.sessionSearch = newSessionSearch(d, d.sessionFTSSetup)
	d.sessionTitles = newSessionTitles(d)
	err := d.initialize(ctx)
	if err != nil {
		setLastInitError(pyExceptionClassName(err) + ": " + errText(err))
		core.mu.Lock()
		conn := core.conn
		core.setConnLocked(nil)
		core.mu.Unlock()
		closeConnectionQuietly(conn)
		return nil, err
	}
	registerReadPoolMember(core.readBudget, d)
	registerTestInstance(d)
	runtime.AddCleanup(d, func(c *sessionCore) {
		if c.hasConn.Load() {
			_ = c.closeHandle()
		}
	}, core)
	return d, nil
}

// Opens the writer or read only connection and records the file identity
func (d *SessionDB) initialize(ctx context.Context) error {
	if d.isReadOnly {
		if err := d.openReadOnly(ctx); err != nil {
			return err
		}
	} else {
		if !closeTimeCheckpointConfigurable() {
			d.retireConnection = prepareConnectionRetirement()
		}
		if err := d.openWriter(ctx); err != nil {
			return err
		}
	}
	return d.recordDBFileIdentity(ctx)
}

// Python exception class name of an error for the init failure diagnostic
func pyExceptionClassName(err error) string {
	var typed interface{ PyTypeName() string }
	var intErr *pyint.Error
	var conv *pyConvError
	var corrupt *StateDBCorruptError
	var deleted *DeletedWalGenerationError
	var replaced *StateDBReplacedError
	var capture *RetiredGenerationCaptureError
	var pathErr *os.PathError
	var linkErr *os.LinkError
	var syscallErr *os.SyscallError
	switch {
	case errors.As(err, &typed):
		return typed.PyTypeName()
	case errors.As(err, &intErr):
		return intErr.Kind
	case errors.As(err, &conv):
		return conv.Class
	case errors.As(err, &corrupt):
		return "StateDbCorruptError"
	case errors.As(err, &deleted):
		return "DeletedWalGenerationError"
	case errors.As(err, &replaced):
		return "StateDbReplacedError"
	case errors.As(err, &capture):
		return "RetiredGenerationCaptureError"
	}
	if class, ok := sqliteClassOf(err); ok {
		return sqliteClassNames[class]
	}
	if errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &syscallErr) {
		return pypath.OSErrorClass(err)
	}
	return "RuntimeError"
}

// Python sqlite3 exception class names by class
var sqliteClassNames = map[SQLiteClass]string{
	SQLiteDatabaseError:     "DatabaseError",
	SQLiteIntegrityError:    "IntegrityError",
	SQLiteInterfaceError:    "InterfaceError",
	SQLiteInternalError:     "InternalError",
	SQLiteOperationalError:  "OperationalError",
	SQLiteDataError:         "DataError",
	SQLiteProgrammingError:  "ProgrammingError",
	SQLiteNotSupportedError: "NotSupportedError",
}

// Sets the writer connection and its lock free presence flag, caller holds mu or owns the handle exclusively
func (c *sessionCore) setConnLocked(conn SQLiteConn) {
	c.conn = conn
	c.hasConn.Store(conn != nil)
}

// Closes conn and logs a failing close at warning level, never swallowing it silently
func (c *sessionCore) closeConnLogged(conn SQLiteConn, label string) {
	if err := conn.Close(); err != nil {
		slog.Warn(fmt.Sprintf("%s close failed for %s: %s", label, pathlibStr(c.path), errText(err)))
	}
}

// Reports whether a path exists the way pathlib exists does, false on any stat error
func pathlibExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Writable open with preflight, zero byte quarantine, connect, schema, one malformed schema repair and generation stamp
func (d *SessionDB) openWriter(ctx context.Context) error {
	if _, err := home.Mkdir(filepath.Dir(d.path)); err != nil {
		return err
	}
	err := d.openWriterGuarded(ctx)
	if err != nil {
		if !isDatabaseError(err) || !IsMalformedSchemaError(err) || !claimRepairAttempt(d.path) {
			return err
		}
		slog.Error(fmt.Sprintf("state.db schema is malformed (%s) — attempting automatic repair (a backup copy is made first).", errText(err)))
		d.mu.Lock()
		conn := d.conn
		d.setConnLocked(nil)
		d.mu.Unlock()
		closeConnectionQuietly(conn)
		report, rerr := RepairStateDBSchema(ctx, d.path, true)
		if rerr != nil {
			return rerr
		}
		if report == nil || !report.Repaired {
			return err
		}
		if err := d.connectAndInitWithLockPatience(ctx); err != nil {
			return err
		}
	}
	if err := d.ensureDBFileGeneration(); err != nil {
		return err
	}
	if d.walActive.Load() {
		d.mu.Lock()
		d.walLockGuard = lockGuardHold(d.path, nil)
		d.mu.Unlock()
	}
	return nil
}

// Preflight, quarantine and connect, serialized by the cross process quarantine lock for an absent or headerless file
func (d *SessionDB) openWriterGuarded(ctx context.Context) error {
	if !pathlibExists(d.path) || HasInvalidSQLiteHeaderPreopen(d.path, 100, false) {
		if err := PreflightDBWritability(ctx, d.path, "state.db", true); err != nil {
			return err
		}
		acquired, release, err := QuarantineCrossProcessLock(d.path, quarantineCrossProcessLockTimeoutS)
		if err != nil {
			return err
		}
		defer release()
		if !acquired {
			slog.Warn(fmt.Sprintf("startup quarantine lock for %s not acquired within 5s; proceeding", pathlibStr(d.path)))
		}
		if err := PreflightDBWritability(ctx, d.path, "state.db", false); err != nil {
			return err
		}
		if err := d.handleQuarantineIfInvalid(acquired); err != nil {
			return err
		}
		return d.connectAndInitWithLockPatience(ctx)
	}
	if err := PreflightDBWritability(ctx, d.path, "state.db", false); err != nil {
		return err
	}
	if err := d.handleQuarantineIfInvalid(false); err != nil {
		return err
	}
	return d.connectAndInitWithLockPatience(ctx)
}

// Read only attach with FTS flags probed by SELECT, retrying a transient disk IO error a bounded number of times
func (d *SessionDB) openReadOnly(ctx context.Context) error {
	for attempt := 0; ; attempt++ {
		err := d.openReadOnlyOnce(ctx)
		if err == nil {
			return nil
		}
		if !isOperationalError(err) {
			return err
		}
		transient := strings.Contains(pystr.Lower(errText(err)), diskIOErrorMarker)
		if attempt >= readOnlyIOErrRetryAttempts || !transient {
			return err
		}
		time.Sleep(secondsToDuration(readOnlyIOErrRetryBackoffS))
	}
}

func (d *SessionDB) openReadOnlyOnce(ctx context.Context) error {
	conn, err := d.connectReadOnly(readBusyTimeoutS)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.setConnLocked(conn)
	d.mu.Unlock()
	probe := func() error {
		if err := sessionApplyDatabasePragmas(ctx, conn, "state.db"); err != nil {
			return err
		}
		status, err := d.ftsTableProbe(ctx, conn, "messages_fts")
		if err != nil {
			return err
		}
		d.ftsEnabled.Store(status == ftsTableStatusQueryable)
		if d.ftsEnabled.Load() {
			status, err = d.ftsTableProbe(ctx, conn, "messages_fts_trigram")
			if err != nil {
				return err
			}
			d.trigramAvailable.Store(status == ftsTableStatusQueryable)
		}
		return nil
	}
	if err := probe(); err != nil {
		d.mu.Lock()
		d.setConnLocked(nil)
		d.mu.Unlock()
		closeConnectionQuietly(conn)
		return err
	}
	return nil
}

// Tracked mode=ro connection to the store under its filesystem path
func (c *sessionCore) connectReadOnly(timeout float64) (SQLiteConn, error) {
	uri, err := ReadOnlyDBURI(c.path)
	if err != nil {
		return nil, err
	}
	conn, err := sessionConnectTracked(uri, WithTrackingPath(c.path), WithURI(), WithTimeout(timeout))
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// Moves a zero byte or headerless state.db aside, refusing to open it when it could not be moved
func (c *sessionCore) handleQuarantineIfInvalid(alreadyLocked bool) error {
	if !(pathlibExists(c.path) && HasInvalidSQLiteHeaderPreopen(c.path, 100, false)) {
		return nil
	}
	zsize := int64(-1)
	if st, err := os.Stat(c.path); err == nil {
		zsize = st.Size()
	}
	qpath, err := QuarantineInvalidStateDB(c.path, alreadyLocked)
	if err != nil {
		return err
	}
	where := "left in place (it could not be moved aside)"
	source := pathlibStr(c.path)
	if qpath != "" {
		where = "moved aside to " + qpath
		source = qpath
	}
	msg := fmt.Sprintf("state.db was empty or damaged (%d bytes) and has been %s; icebath started with a "+
		"fresh, empty session database. To bring old sessions back, run "+
		"`icebath sessions recover --source %s --inspect-only`, or restore a "+
		"snapshot with `/snapshot list` then `/snapshot restore <id>` (terminal `icebath` chat only).", zsize, where, source)
	slog.Error(msg)
	setLastInitError(msg)
	if qpath == "" && pathlibExists(c.path) && HasInvalidSQLiteHeaderPreopen(c.path, 100, false) {
		return &SQLiteError{Class: SQLiteDatabaseError, Msg: msg}
	}
	return nil
}

// Writer connection with WAL or its fallback, pragmas, foreign keys and the CJK tokenizer, no schema work
func (d *SessionDB) openWriterConn(ctx context.Context) (SQLiteConn, error) {
	conn, err := sessionConnectTracked(d.path, WithTimeout(1.0))
	if err != nil {
		return nil, err
	}
	setup := func() error {
		mode, err := ApplyWALWithFallback(ctx, conn, "state.db", false)
		if err != nil {
			return err
		}
		onDisk := ""
		if mode == "wal" {
			if m, ok, err := OnDiskJournalMode(ctx, conn); err != nil {
				return err
			} else if ok {
				onDisk = m
			}
		}
		d.walActive.Store(mode == "wal" && onDisk == "wal")
		if err := secureStateDBFiles(d.path, false); err != nil {
			return err
		}
		if err := sessionApplyDatabasePragmas(ctx, conn, "state.db"); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
			return err
		}
		d.ftsCJKLoaded.Store(LoadFTS5CJKExtension(ctx, conn))
		return nil
	}
	if err := setup(); err != nil {
		closeConnectionQuietly(conn)
		return nil, err
	}
	return conn, nil
}

// Refuses a deleted WAL generation, secures the main file, connects and runs schema init
func (d *SessionDB) connectAndInit(ctx context.Context) error {
	if err := RefuseDeletedWALGeneration(d.path); err != nil {
		return err
	}
	if err := secureStateDBFiles(d.path, true); err != nil {
		return err
	}
	conn, err := d.openWriterConn(ctx)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.setConnLocked(conn)
	d.mu.Unlock()
	return sessionInitSchema(ctx, d)
}

// Open and init, waiting out a sibling's write lock with jittered patience while other errors propagate at once
func (d *SessionDB) connectAndInitWithLockPatience(ctx context.Context) error {
	deadline := monotonicSeconds() + sessionWritePatienceS
	for {
		err := d.connectAndInit(ctx)
		if err == nil {
			return nil
		}
		if !isOperationalError(err) || !IsSQLiteLockError(err) {
			return err
		}
		d.mu.Lock()
		conn := d.conn
		d.setConnLocked(nil)
		d.mu.Unlock()
		closeConnectionQuietly(conn)
		now := monotonicSeconds()
		if now >= deadline {
			_ = LogWriteLockHolders(d.path, sessionWritePatienceS)
			return err
		}
		jitter := sessionWriteRetrySlowMinS + rand.Float64()*(sessionWriteRetrySlowMaxS-sessionWriteRetrySlowMinS)
		time.Sleep(secondsToDuration(min(jitter, max(deadline-now, 0.001))))
	}
}

// Seconds as a duration
func secondsToDuration(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}

// Fresh read only connection under a read permit, nil when WAL is off, the pool closed, backing off or at capacity
func (d *SessionDB) getReadConn(ctx context.Context) (SQLiteConn, error) {
	if !d.walActive.Load() || d.isReadOnly {
		return nil, nil
	}
	d.readConnsMu.Lock()
	failedAt := d.readOpenFailedAt
	backingOff := failedAt != 0 && monotonicSeconds()-failedAt < readOpenRetrySeconds
	closed := d.readConnsClosed
	d.readConnsMu.Unlock()
	if closed || backingOff {
		return nil, nil
	}
	ok, err := d.readBudget.acquire(d)
	if err != nil {
		return nil, err
	}
	if !ok {
		slog.Debug(fmt.Sprintf("read pool at capacity (%d) for %s; serving this read from the locked writer connection", readPoolMax, pathlibStr(d.path)))
		return nil, nil
	}
	conn, err := d.connectReadOnly(readBusyTimeoutS)
	if err == nil {
		err = sessionApplyDatabasePragmas(ctx, conn, "state.db")
		if err == nil && d.ftsCJKLoaded.Load() {
			LoadFTS5CJKExtension(ctx, conn)
		}
	}
	if err != nil {
		if conn != nil {
			d.closeConnLogged(conn, "partially-opened read conn")
		}
		if rerr := d.readBudget.release(); rerr != nil {
			return nil, rerr
		}
		if _, isSQLite := sqliteClassOf(err); !isSQLite {
			return nil, err
		}
		d.readConnsMu.Lock()
		d.readOpenFailedAt = monotonicSeconds()
		d.readConnsMu.Unlock()
		slog.Debug(fmt.Sprintf("read-only connection open failed for %s: %s", pathlibStr(d.path), errText(err)))
		return nil, nil
	}
	return conn, nil
}

// Pops the most recently returned idle read connection, false when the pool is empty
func (c *sessionCore) popIdleReadConn() (SQLiteConn, bool) {
	c.poolMu.Lock()
	defer c.poolMu.Unlock()
	n := len(c.readPool)
	if n == 0 {
		return nil, false
	}
	conn := c.readPool[n-1]
	c.readPool[n-1] = nil
	c.readPool = c.readPool[:n-1]
	return conn, true
}

// Pushes a read connection back unless the pool is full
func (c *sessionCore) pushIdleReadConn(conn SQLiteConn) bool {
	c.poolMu.Lock()
	defer c.poolMu.Unlock()
	if len(c.readPool) >= readPoolMax {
		return false
	}
	c.readPool = append(c.readPool, conn)
	return true
}

// Closes one idle pooled connection so a peer on the same file can take its permit, never a live one
func (c *sessionCore) evictOneIdleReadConn() bool {
	conn, ok := c.popIdleReadConn()
	if !ok {
		return false
	}
	if err := c.closeReadConn(conn); err != nil {
		slog.Warn(fmt.Sprintf("read-conn permit release failed for %s: %s", pathlibStr(c.path), errText(err)))
	}
	return true
}

// Closes a pooled read connection and releases its permit even when the close fails
func (c *sessionCore) closeReadConn(conn SQLiteConn) error {
	c.closeConnLogged(conn, "read-conn")
	return c.readBudget.release()
}

// Borrows an idle read connection or opens one, nil when the read path is unavailable
func (d *SessionDB) checkoutReadConn(ctx context.Context) (SQLiteConn, error) {
	if !d.walActive.Load() || d.isReadOnly {
		return nil, nil
	}
	if conn, ok := d.popIdleReadConn(); ok {
		return conn, nil
	}
	return d.getReadConn(ctx)
}

// Runs fn on a pooled read connection without the writer lock under WAL, otherwise on the writer connection under it
func (d *SessionDB) readCtx(ctx context.Context, reopen bool, fn func(conn SQLiteConn) error) error {
	conn, err := d.checkoutReadConn(ctx)
	if err != nil {
		return err
	}
	if conn != nil {
		ferr := fn(conn)
		returned := false
		d.readConnsMu.Lock()
		if !d.readConnsClosed {
			returned = d.pushIdleReadConn(conn)
		}
		d.readConnsMu.Unlock()
		if !returned {
			if rerr := d.closeReadConn(conn); rerr != nil && ferr == nil {
				return rerr
			}
		}
		return ferr
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		if !reopen {
			return &SQLiteError{Class: SQLiteProgrammingError, Msg: fmt.Sprintf("SessionDB for %s is closed", pathlibStr(d.path))}
		}
		if err := d.reopenAfterCloseLocked("read"); err != nil {
			return err
		}
	}
	conn = d.conn
	if d.walActive.Load() || d.isReadOnly {
		return fn(conn)
	}
	var previousMS int64
	if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&previousMS); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", int64(readBusyTimeoutS*1000))); err != nil {
		return err
	}
	defer func() {
		_, _ = conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", previousMS))
	}()
	return fn(conn)
}

// Reopens the writer after close raced a live caller, refusing a read only, quarantined or replaced handle, caller holds mu
func (d *SessionDB) reopenAfterCloseLocked(context string) error {
	if d.isReadOnly {
		return &SQLiteError{Class: SQLiteProgrammingError, Msg: fmt.Sprintf("SessionDB for %s was closed (read-only handle); "+
			"cannot serve a %s after close()", pathlibStr(d.path), context)}
	}
	if d.corrupt() && !(d.dbReplaced.Load() || d.dbFileWasReplaced()) {
		return d.corruptError(fmt.Sprintf("state.db connection for %s is quarantined after "+
			"structural corruption; refusing to reopen for a %s "+
			"after close(). ", pathlibStr(d.path), context))
	}
	if err := d.haltIfDBGenerationChanged(); err != nil {
		return err
	}
	slog.Warn(fmt.Sprintf("state.db connection for %s was closed while a %s was still in "+
		"flight — reopening (teardown/worker race, #94736)", pathlibStr(d.path), context))
	conn, err := d.openWriterConn(d.openCtx)
	if err != nil {
		return &SQLiteError{Class: SQLiteOperationalError, Msg: fmt.Sprintf("state.db connection was closed while a %s was still "+
			"in flight (a session-teardown path called close() before "+
			"this worker finished — #94736) and the automatic reopen failed: %s", context, errText(err))}
	}
	d.setConnLocked(conn)
	if d.walActive.Load() {
		d.walLockGuard = lockGuardHold(d.path, nil)
	}
	return nil
}

// Runs fn inside BEGIN IMMEDIATE with jittered lock retry and commits, fn must stay idempotent under retry
func (d *SessionDB) executeWrite(ctx context.Context, fn func(conn SQLiteConn) error, patienceS *float64) error {
	patience := sessionWritePatienceS
	if patienceS != nil {
		patience = *patienceS
	}
	deadline := monotonicSeconds() + patience
	compressionDeadline := 0.0
	hasCompressionDeadline := false
	ioerrBeginRetried := false
	for {
		if err := d.raiseIfDBCorrupt(true); err != nil {
			return err
		}
		fnStarted := false
		err := d.writeOnce(ctx, fn, &fnStarted)
		if err == nil {
			n := d.writeCount.Add(1)
			if n%sessionCheckpointEveryNWrites == 0 {
				d.tryWALCheckpoint()
			}
			if n%sessionFTSMergeEveryNWrites == 0 {
				d.tryIncrementalMergeFTS(ctx)
			}
			return nil
		}
		var compression *SessionCompressionInProgressError
		if errors.As(err, &compression) {
			if !hasCompressionDeadline {
				compressionDeadline = min(monotonicSeconds()+sessionCompressionBusyWaitS, deadline)
				hasCompressionDeadline = true
			}
			if d.sleepBeforeWriteRetry(compressionDeadline, sessionCompressionBusyWaitS) {
				continue
			}
			return err
		}
		if _, isSQLite := sqliteClassOf(err); !isSQLite {
			return err
		}
		if isNoMoreRows(err) && d.sleepBeforeWriteRetry(deadline, patience) {
			continue
		}
		errMsg := pystr.Lower(errText(err))
		if isOperationalError(err) {
			if IsSQLiteLockError(err) {
				if d.sleepBeforeWriteRetry(deadline, patience) {
					continue
				}
				_ = LogWriteLockHolders(d.path, patience)
				return &SQLiteError{Class: SQLiteOperationalError, Msg: fmt.Sprintf("database is locked (another icebath process held the "+
					"state.db write lock for over %.0fs — "+
					"likely a long maintenance operation such as VACUUM, "+
					"a large WAL checkpoint, or an older pre-update "+
					"process; the database itself is healthy)", patience)}
			}
			if strings.Contains(errMsg, diskIOErrorMarker) && !fnStarted && !ioerrBeginRetried && d.sleepBeforeWriteRetry(deadline, patience) {
				ioerrBeginRetried = true
				continue
			}
			return err
		}
		if isDatabaseError(err) {
			if strings.Contains(errMsg, "not a database") || IsMalformedDBError(err) || isFTSWriteCorruptionError(err) {
				d.mu.Lock()
				rerr := d.raiseIfDBReplaced()
				d.mu.Unlock()
				if rerr != nil {
					return rerr
				}
			}
			detached, ferr := d.enterFTSFailOpen(ctx, err, &deadline, &patience)
			if ferr != nil {
				return ferr
			}
			if detached {
				continue
			}
			if isStructuralCorruptionError(err) {
				return d.haltDBCorrupt(err)
			}
		}
		return err
	}
}

// Runs fn inside the retried write transaction with the default patience
func (d *SessionDB) ExecuteWrite(ctx context.Context, fn func(conn SQLiteConn) error) error {
	return d.executeWrite(ctx, fn, nil)
}

// One BEGIN IMMEDIATE attempt under the writer lock, rolling back when fn or the commit fails
func (d *SessionDB) writeOnce(ctx context.Context, fn func(conn SQLiteConn) error, fnStarted *bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.raiseIfDBReplaced(); err != nil {
		return err
	}
	if d.conn == nil {
		if err := d.reopenAfterCloseLocked("write"); err != nil {
			return err
		}
	}
	conn := d.conn
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	*fnStarted = true
	err := fn(conn)
	if err == nil {
		err = commitIfInTransaction(ctx, conn)
	}
	if err != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
	}
	return err
}

// Runs one statement through executeWrite, each parameter row of params when many is set
func (d *SessionDB) writeSQL(ctx context.Context, query string, params []any, many bool, patienceS *float64) error {
	return d.executeWrite(ctx, func(conn SQLiteConn) error {
		if !many {
			_, err := conn.ExecContext(ctx, query, params...)
			return err
		}
		for _, row := range params {
			rowArgs, ok := row.([]any)
			if !ok {
				return &pyConvError{"ProgrammingError", "parameters are of unsupported type"}
			}
			if _, err := conn.ExecContext(ctx, query, rowArgs...); err != nil {
				return err
			}
		}
		return nil
	}, patienceS)
}

// Runs one UPDATE or DELETE through executeWrite and returns the rows it changed
func (d *SessionDB) writeRowcount(ctx context.Context, query string, params []any, patienceS *float64) (int64, error) {
	var rowcount int64
	err := d.executeWrite(ctx, func(conn SQLiteConn) error {
		res, err := conn.ExecContext(ctx, query, params...)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n < 0 {
			if err := conn.QueryRowContext(ctx, "SELECT changes()").Scan(&n); err != nil {
				return err
			}
		}
		rowcount = n
		return nil
	}, patienceS)
	return rowcount, err
}

// First row of one read only statement, nil when it returns none
func (d *SessionDB) readOne(ctx context.Context, query string, args ...any) (*pyjson.Object, error) {
	var row *pyjson.Object
	err := d.readRetryingIOErr(ctx, func(conn SQLiteConn) error {
		rows, err := queryObjects(ctx, conn, query, args...)
		if err != nil {
			return err
		}
		row = nil
		if len(rows) > 0 {
			row = rows[0]
		}
		return nil
	})
	return row, err
}

// Every row of one read only statement
func (d *SessionDB) readAll(ctx context.Context, query string, args ...any) ([]*pyjson.Object, error) {
	var out []*pyjson.Object
	err := d.readRetryingIOErr(ctx, func(conn SQLiteConn) error {
		rows, err := queryObjects(ctx, conn, query, args...)
		out = rows
		return err
	})
	return out, err
}

// Runs an idempotent read through readCtx, replaying a transient disk IO error on the same connection within budget
func (d *SessionDB) readRetryingIOErr(ctx context.Context, fn func(conn SQLiteConn) error) error {
	for attempt := 0; ; attempt++ {
		err := d.readCtx(ctx, true, fn)
		if err == nil {
			return nil
		}
		if isOperationalError(err) {
			if attempt >= readOnlyIOErrRetryAttempts || !strings.Contains(pystr.Lower(errText(err)), diskIOErrorMarker) {
				NoteStorageError(d.path, err)
				return err
			}
			time.Sleep(secondsToDuration(readOnlyIOErrRetryBackoffS))
			continue
		}
		if isDatabaseError(err) {
			NoteStorageError(d.path, err)
		}
		return err
	}
}

// Mints the once per file generation stamp in state_meta and application_id, first opener wins
func (c *sessionCore) ensureDBFileGeneration() error {
	if c.isReadOnly {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	ctx := c.openCtx
	conn := c.conn
	stamp := func() error {
		token := NewUID()
		var value any
		readStamp := func() error {
			value = nil
			err := conn.QueryRowContext(ctx, "SELECT value FROM state_meta WHERE key = ?", stateDBGenerationKey).Scan(&value)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if err := readStamp(); err != nil {
			return err
		}
		if !pyTruthy(value) {
			if _, err := conn.ExecContext(ctx, "INSERT OR IGNORE INTO state_meta (key, value) VALUES (?, ?)", stateDBGenerationKey, token); err != nil {
				return err
			}
			if err := readStamp(); err != nil {
				return err
			}
		}
		if pyTruthy(value) {
			token = pyrepr.Str(value)
		}
		var appID int64
		if err := conn.QueryRowContext(ctx, "PRAGMA application_id").Scan(&appID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		current := uint32(appID)
		if current == 0 {
			head := []rune(token)
			if len(head) > 8 {
				head = head[:8]
			}
			prefix, ok := pyint.ParseBase(string(head), 16)
			if !ok {
				return &pyConvError{"ValueError", fmt.Sprintf("invalid literal for int() with base 16: %s", pyrepr.Repr(string(head)))}
			}
			current = uint32(new(big.Int).And(prefix, big.NewInt(0x7FFFFFFF)).Int64())
			if current == 0 {
				current = 1
			}
			if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id=%d", current)); err != nil {
				return err
			}
		}
		c.identityMu.Lock()
		c.dbFileApplicationID = current
		c.identityMu.Unlock()
		_, _ = conn.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)")
		return nil
	}
	err := stamp()
	if _, isSQLite := sqliteClassOf(err); err != nil && isSQLite {
		slog.Debug(fmt.Sprintf("state.db generation stamp skipped: %s", errText(err)))
		return nil
	}
	return err
}

// Snapshots the inode and the on disk generation header when present
func (d *SessionDB) recordDBFileIdentity(ctx context.Context) error {
	ident, ok := StatDBFileIdentity(d.path)
	sidecars := statSQLiteSidecarIdentity(d.path)
	diskID, _ := readSQLiteApplicationID(d.path)
	d.identityMu.Lock()
	d.dbFileIdentity = nil
	if ok {
		d.dbFileIdentity = &ident
	}
	d.dbSidecarIdentity = sidecars
	if diskID != 0 {
		d.dbFileApplicationID = diskID
		d.identityMu.Unlock()
		return nil
	}
	needProbe := d.dbFileApplicationID == 0
	d.identityMu.Unlock()
	if !needProbe || !d.hasConn.Load() {
		return nil
	}
	row, err := d.readOne(ctx, "PRAGMA application_id")
	if err != nil {
		if _, isSQLite := sqliteClassOf(err); isSQLite {
			return nil
		}
		return err
	}
	if row != nil && len(row.Keys) > 0 {
		v, _ := row.Get(row.Keys[0])
		if n, ok := v.(int64); ok && n != 0 {
			d.identityMu.Lock()
			d.dbFileApplicationID = uint32(n)
			d.identityMu.Unlock()
		}
	}
	return nil
}

// Reports whether the path no longer names the file this handle opened
func (c *sessionCore) dbFileWasReplaced() bool {
	c.identityMu.Lock()
	recorded := c.dbFileIdentity
	recordedApp := c.dbFileApplicationID
	c.identityMu.Unlock()
	if recorded != nil {
		current, ok := StatDBFileIdentity(c.path)
		if !ok || current != *recorded {
			return true
		}
	}
	if recordedApp == 0 {
		return false
	}
	diskApp, _ := readSQLiteApplicationID(c.path)
	return diskApp != 0 && diskApp != recordedApp
}

// Reports whether the WAL and SHM generation this handle opened is gone, adopting the current one once clean
func (c *sessionCore) walGenerationWasLost() (bool, error) {
	c.identityMu.Lock()
	recorded := c.dbSidecarIdentity
	c.identityMu.Unlock()
	if len(recorded) > 0 {
		for suffix, ident := range recorded {
			current, ok := StatDBFileIdentity(c.path + suffix)
			if !ok || current != ident {
				return true, nil
			}
		}
		return false, nil
	}
	if !c.walActive.Load() {
		return false, nil
	}
	if runtime.GOOS == "linux" {
		watched, err := watchedSQLiteSidecarPaths(c.path)
		if err != nil {
			return false, err
		}
		targets, err := procFDTargets(os.Getpid())
		if err != nil {
			return false, nil
		}
		for _, t := range targets {
			canonical, cerr := CanonicalSQLitePath(t.target)
			if cerr != nil {
				return false, nil
			}
			if want, ok := watched[canonical]; ok && strings.Contains(t.target, " (deleted)") && fdIsTrulyUnlinked(t.fdPath, want) {
				return true, nil
			}
		}
	}
	if current := statSQLiteSidecarIdentity(c.path); len(current) > 0 {
		c.identityMu.Lock()
		c.dbSidecarIdentity = current
		c.identityMu.Unlock()
	}
	return false, nil
}

// Stops writes when the file was replaced or its WAL generation is gone, both flags sticky
func (c *sessionCore) haltIfDBGenerationChanged() error {
	if c.dbReplaced.Load() || c.dbFileWasReplaced() {
		c.dbReplaced.Store(true)
		c.disableCloseTimeCheckpoint()
		slog.Error(StateDBReplacedMsg)
		return &StateDBReplacedError{Msg: StateDBReplacedMsg}
	}
	lost := c.dbWALGenerationLost.Load()
	if !lost {
		var err error
		if lost, err = c.walGenerationWasLost(); err != nil {
			return err
		}
	}
	if lost {
		c.dbWALGenerationLost.Store(true)
		c.disableCloseTimeCheckpoint()
		if _, err := c.captureRetiredGeneration("halt"); err != nil {
			var capture *RetiredGenerationCaptureError
			if !errors.As(err, &capture) {
				return err
			}
			slog.Error(fmt.Sprintf("Could not capture the retired WAL generation of %s at halt: %s. close() retries "+
				"the capture and refuses to settle without it.", pathlibStr(c.path), errText(err)))
		}
		slog.Error(DeletedWalGenerationMsg)
		return &DeletedWalGenerationError{Msg: DeletedWalGenerationMsg}
	}
	return nil
}

// Durably captures the lost WAL generation this handle still holds, once per handle
func (c *sessionCore) captureRetiredGeneration(trigger string) (string, error) {
	c.retiredCaptureMu.Lock()
	if c.retiredGenerationCapture != "" {
		artifact := c.retiredGenerationCapture
		c.retiredCaptureMu.Unlock()
		return artifact, nil
	}
	c.identityMu.Lock()
	identity := make(map[string]FileIdentity, len(c.dbSidecarIdentity))
	for k, v := range c.dbSidecarIdentity {
		identity[k] = v
	}
	c.identityMu.Unlock()
	artifact, err := CaptureRetiredWALGeneration(c.path, identity, trigger)
	if err != nil {
		c.retiredCaptureMu.Unlock()
		return "", err
	}
	c.retiredGenerationCapture = artifact
	c.retiredCaptureMu.Unlock()
	slog.Warn(fmt.Sprintf("Captured the retired WAL generation of %s at %s to %s; read its manifest.json before deciding "+
		"whether those frames belong on top of the file now at the path.", pathlibStr(c.path), trigger, artifact))
	return artifact, nil
}

// Sticky flag fast path, then the live replaced and generation probe
func (c *sessionCore) raiseIfDBReplaced() error {
	if c.dbReplaced.Load() {
		return &StateDBReplacedError{Msg: StateDBReplacedMsg}
	}
	if c.dbWALGenerationLost.Load() {
		return &DeletedWalGenerationError{Msg: DeletedWalGenerationMsg}
	}
	return c.haltIfDBGenerationChanged()
}

// Reports bare canonical corruption with no FTS provenance, never repairable from the live write path
func isStructuralCorruptionError(err error) bool {
	var corrupt *StateDBCorruptError
	return isDatabaseError(err) && !errors.As(err, &corrupt) && !isFTSWriteCorruptionError(err) && ClassifyPersistenceError(err) == "corrupt"
}

// Reports whether this handle is quarantined for structural corruption
func (c *sessionCore) corrupt() bool {
	c.corruptMu.Lock()
	defer c.corruptMu.Unlock()
	return c.dbCorrupt
}

// Quarantine error of this handle carrying the recorded cause
func (c *sessionCore) corruptError(prefix string) *StateDBCorruptError {
	c.corruptMu.Lock()
	reason := c.dbCorruptReason
	c.corruptMu.Unlock()
	return &StateDBCorruptError{Msg: fmt.Sprintf("%s%s (cause: %s)", prefix, StateDBCorruptMsg, reason)}
}

// Quarantines this handle, publishes the profile level corrupt state and returns the quarantine error
func (c *sessionCore) haltDBCorrupt(exc error) error {
	c.corruptMu.Lock()
	c.dbCorrupt = true
	c.dbCorruptReason = errText(exc)
	c.corruptMu.Unlock()
	MarkStorageCorrupt(c.path, exc)
	c.disableCloseTimeCheckpoint()
	slog.Error(fmt.Sprintf("state.db %s reported structural corruption outside the FTS "+
		"indexes (%s); quarantining this handle: no further writes, no "+
		"automatic reopen, no explicit WAL checkpoint at close. Stop the "+
		"gateway and run `icebath sessions recover --source %s --inspect-only`.", pathlibStr(c.path), errText(exc), pathlibStr(c.path)))
	err := c.corruptError("")
	err.Cause = exc
	return err
}

// Switches off SQLite's close time checkpoint on the writer connection, false when that is impossible
func (c *sessionCore) disableCloseTimeCheckpoint() bool {
	conn := c.conn
	if !closeTimeCheckpointConfigurable() || conn == nil {
		return false
	}
	if err := setNoCheckpointOnClose(conn); err != nil {
		slog.Error(fmt.Sprintf("Could not disable SQLite's close-time checkpoint on the quarantined handle for %s; "+
			"closing it may checkpoint retired frames over the newer generation: %s", pathlibStr(c.path), errText(err)))
		return false
	}
	return true
}

// Retains the exact quarantined connection past collection and process teardown, once per handle
func (c *sessionCore) pinConnection(conn SQLiteConn) {
	if !c.connectionPinned {
		c.retireConnection(conn)
		c.connectionPinned = true
	}
}

// Captures the retired generation and reports whether the handle must be retired unclosed, caller holds mu
func (c *sessionCore) settleLostGenerationLocked() (bool, error) {
	c.dbWALGenerationLost.Store(true)
	retireWithoutClose := !c.disableCloseTimeCheckpoint() && c.retireConnection != nil
	artifact, err := c.captureRetiredGeneration("close")
	if err != nil {
		var capture *RetiredGenerationCaptureError
		if errors.As(err, &capture) {
			if retireWithoutClose {
				c.pinConnection(c.conn)
			}
			slog.Error(fmt.Sprintf("Could not capture the retired WAL generation of %s at close: %s. The handle stays open "+
				"and close() retries the capture; those frames are NOT yet preserved.", pathlibStr(c.path), errText(err)))
		}
		return false, err
	}
	slog.Warn(fmt.Sprintf("Skipping the close-time WAL checkpoint for %s: this handle's WAL/SHM generation "+
		"was deleted or replaced; the retired generation is captured at %s. Stop the other "+
		"writers before reopening and inspect the capture before deciding its disposition.", pathlibStr(c.path), artifact))
	if retireWithoutClose {
		slog.Warn(fmt.Sprintf("Retaining the quarantined connection for %s unclosed: this runtime cannot "+
			"switch off SQLite's close-time checkpoint.", pathlibStr(c.path)))
	}
	return retireWithoutClose, nil
}

// Refuses a quarantined handle, and with storage set any handle whose file another handle saw corrupt
func (c *sessionCore) raiseIfDBCorrupt(storage bool) error {
	if c.corrupt() {
		return c.corruptError("")
	}
	if storage && StorageState(c.path) == StorageCorrupt {
		reason, _ := StorageCorruptReason(c.path)
		return c.haltDBCorrupt(&SQLiteError{Class: SQLiteDatabaseError, Msg: "database disk image is malformed (reported earlier in this process: " + reason + ")"})
	}
	return nil
}

// Sleeps before a write retry through the test override when one is installed
func (c *sessionCore) sleepBeforeWriteRetry(deadline, patienceS float64) bool {
	if c.leaseSeams.sleepRetry != nil {
		return c.leaseSeams.sleepRetry(deadline, patienceS)
	}
	return c.sleepBeforeWriteRetryJitter(deadline, patienceS)
}

// Sleeps one jitter interval inside the budget, fast jitter first then slow, false once the deadline passed
func (c *sessionCore) sleepBeforeWriteRetryJitter(deadline, patienceS float64) bool {
	now := monotonicSeconds()
	if now >= deadline {
		return false
	}
	lo, hi := sessionWriteRetryMinS, sessionWriteRetryMaxS
	if now-(deadline-patienceS) >= sessionWriteRetrySlowAfterS {
		lo, hi = sessionWriteRetrySlowMinS, sessionWriteRetrySlowMaxS
	}
	jitter := lo + rand.Float64()*(hi-lo)
	time.Sleep(secondsToDuration(min(jitter, max(deadline-now, 0.001))))
	return true
}

// Foreign processes holding this store or its WAL sidecars
func (c *sessionCore) foreignStateDBHolders() ([]Holder, error) {
	return foreignStateDBHoldersOf(c.path)
}

// Foreign processes holding this store for callers outside this package
func (c *sessionCore) ForeignStateDBHolders() ([]Holder, error) { return c.foreignStateDBHolders() }

// Terminates inactive orphan desktop backends among holders of this store
func (c *sessionCore) reapInactiveOrphanDesktopHolders(holders []Holder, minAgeSeconds float64) ([]int, error) {
	return reapInactiveOrphanDesktopHolders(c.path, holders, minAgeSeconds)
}

// Why this handle must not checkpoint or run in file repair, false when it may
func (c *sessionCore) quarantineReason() (string, bool) {
	c.corruptMu.Lock()
	corrupt, reason := c.dbCorrupt, c.dbCorruptReason
	c.corruptMu.Unlock()
	switch {
	case corrupt:
		return "structural corruption (" + reason + ")", true
	case c.dbReplaced.Load():
		return "a replaced state.db file", true
	case c.dbWALGenerationLost.Load():
		return "a deleted WAL generation (split-brain)", true
	}
	return "", false
}

// Passive WAL checkpoint that never raises and never blocks writers, skipped on a quarantined handle
func (c *sessionCore) tryWALCheckpoint() {
	if _, quarantined := c.quarantineReason(); quarantined {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return
	}
	if c.walLockGuard != nil {
		c.walLockGuard = lockGuardHold(c.path, c.walLockGuard)
	}
	var busy, logPages, checkpointed int64
	if err := c.conn.QueryRowContext(c.openCtx, "PRAGMA wal_checkpoint(PASSIVE)").Scan(&busy, &logPages, &checkpointed); err != nil {
		slog.Warn(fmt.Sprintf("WAL checkpoint (PASSIVE) failed: %s", errText(err)))
		return
	}
	if logPages > 0 {
		slog.Debug(fmt.Sprintf("WAL checkpoint: %d/%d pages checkpointed", checkpointed, logPages))
	}
}

// Closes the handle, or releases one reference of a registry shared handle
func (d *SessionDB) Close() error {
	if d.sharedRegistryOwned.Load() {
		Release(d)
		return nil
	}
	d.closeTokenWriter()
	err := d.closeHandle()
	unregisterReadPoolMember(d.readBudget, d)
	return err
}

// Drains the read pool, settles a lost generation, checkpoints a healthy writable handle and closes the writer
func (c *sessionCore) closeHandle() error {
	c.readConnsMu.Lock()
	c.readConnsClosed = true
	c.readConnsMu.Unlock()
	for c.evictOneIdleReadConn() {
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	generationLost := false
	if !c.isReadOnly {
		c.identityMu.Lock()
		recorded := len(c.dbSidecarIdentity) > 0
		c.identityMu.Unlock()
		generationLost = c.dbWALGenerationLost.Load()
		if !generationLost && recorded {
			var err error
			if generationLost, err = c.walGenerationWasLost(); err != nil {
				return err
			}
		}
	}
	retireWithoutClose := false
	if generationLost {
		var err error
		if retireWithoutClose, err = c.settleLostGenerationLocked(); err != nil {
			return err
		}
	}
	reason, quarantined := "", false
	if !generationLost {
		reason, quarantined = c.quarantineReason()
	}
	if quarantined {
		slog.Warn(fmt.Sprintf("Skipping the close-time WAL checkpoint for %s: this "+
			"handle observed %s. Take a snapshot of state.db, -wal and -shm "+
			"before restarting, then run `icebath sessions recover --source %s --inspect-only`.", pathlibStr(c.path), reason, pathlibStr(c.path)))
	} else if !c.isReadOnly && !generationLost {
		if _, err := c.conn.ExecContext(c.openCtx, "PRAGMA wal_checkpoint(PASSIVE)"); err != nil {
			slog.Debug(fmt.Sprintf("WAL checkpoint (PASSIVE) at close failed: %s", errText(err)))
		}
	}
	lockGuardRelease(c.walLockGuard)
	if retireWithoutClose {
		c.pinConnection(c.conn)
		c.setConnLocked(nil)
		return nil
	}
	conn := c.conn
	c.setConnLocked(nil)
	closeConnectionQuietly(conn)
	c.identityMu.Lock()
	c.dbSidecarIdentity = map[string]FileIdentity{}
	c.identityMu.Unlock()
	return nil
}

// Path of the database file
func (c *sessionCore) dbPath() string { return c.path }

// Path of the database file
func (c *sessionCore) Path() string { return c.path }

// Reports a handle opened read only
func (c *sessionCore) readOnly() bool { return c.isReadOnly }

// Reports a handle opened read only, the read budget diagnostic
func (c *sessionCore) readOnlyHandle() bool { return c.isReadOnly }

// Reports whether the handle still holds its writer connection
func (c *sessionCore) hasWriterConn() bool { return c.hasConn.Load() }

// Call site that opened the handle
func (c *sessionCore) creationSite() string { return c.site }

// Writer lock serializing every statement on the writer connection
func (c *sessionCore) writerLock() sync.Locker { return &c.mu }

// Writer connection, nil after close
func (c *sessionCore) writerConn() SQLiteConn { return c.conn }

// Default write patience in seconds
func (c *sessionCore) writePatienceS() float64 { return sessionWritePatienceS }

// Pages each bounded FTS merge command may move
func (c *sessionCore) ftsMergeMaxPagesPerIndex() int { return sessionFTSMergeMaxPagesPerIndex }

// Bounded merge commands per index in one pass
func (c *sessionCore) ftsMergeCommandsPerPass() int { return sessionFTSMergeCommandsPerPass }

// Decodes a stored message content value
func (d *SessionDB) decodeContent(content any) any { return decodeMessageContent(content) }

// Message dict of one row
func (d *SessionDB) rowToMessageDict(row *pyjson.Object, warnContext string, summaryFlag bool) (*pyjson.Object, error) {
	return rowToMessageDict(row, warnContext, summaryFlag)
}

// Session dict of one row with resolved prompt columns
func (d *SessionDB) sessionRowDict(row *pyjson.Object) *pyjson.Object { return sessionRowDict(row) }

// Messages around one message id within window
func (d *SessionDB) GetMessagesAround(ctx context.Context, sessionID string, aroundMessageID int64, window int) (*pyjson.Object, error) {
	return getMessagesAround(ctx, d.readCtx, sessionID, aroundMessageID, window)
}

// Value of a state_meta key read on the writer connection so a pending rebuild sees its own progress
func (c *sessionCore) getMeta(ctx context.Context, key string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return "", false, &pyConvError{"AttributeError", "'NoneType' object has no attribute 'execute'"}
	}
	var value any
	err := c.conn.QueryRowContext(ctx, "SELECT value FROM state_meta WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if value == nil {
		return "", false, nil
	}
	if b, ok := value.([]byte); ok {
		return string(b), true, nil
	}
	return pyrepr.Str(value), true, nil
}

// Value of a state_meta key, false when absent or NULL
func (d *SessionDB) GetMeta(ctx context.Context, key string) (string, bool, error) {
	return d.getMeta(ctx, key)
}

// Upserts a state_meta key, inline on cursor when the caller already holds a transaction
func (d *SessionDB) setMeta(ctx context.Context, key, value string, cursor SQLiteConn) error {
	const upsert = "INSERT INTO state_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value"
	if cursor != nil {
		_, err := cursor.ExecContext(ctx, upsert, key, value)
		return err
	}
	return d.writeSQL(ctx, upsert, []any{key, value}, false, nil)
}

// Upserts a state_meta key in its own write transaction
func (d *SessionDB) SetMeta(ctx context.Context, key, value string) error {
	return d.setMeta(ctx, key, value, nil)
}

// Retags legacy kanban worker rows from cli to kanban by cwd under the workspaces root, once per root
func (d *SessionDB) RetagKanbanWorkerSessions(ctx context.Context, workspacesRoot string) (int64, error) {
	prefix := strings.TrimRight(workspacesRoot, "/\\")
	if prefix == "" {
		return 0, nil
	}
	gate := "kanban_worker_source_retagged:" + prefix
	value, ok, err := d.getMeta(ctx, gate)
	if err != nil {
		return 0, err
	}
	if ok && value == "1" {
		return 0, nil
	}
	var retagged int64
	err = d.executeWrite(ctx, func(conn SQLiteConn) error {
		esc := EscapeLike(prefix)
		res, err := conn.ExecContext(ctx, "UPDATE sessions SET source = 'kanban' "+
			"WHERE source = 'cli' AND (cwd = ? OR cwd LIKE ? ESCAPE '\\' "+
			"OR cwd LIKE ? ESCAPE '\\')", prefix, esc+"/%", esc+`\\%`)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		retagged = n
		return d.setMeta(ctx, gate, "1", conn)
	}, nil)
	if err != nil {
		return 0, err
	}
	return retagged, nil
}

// Key and value of every state_meta row whose key starts with the literal prefix
func (d *SessionDB) ListMetaPrefix(ctx context.Context, prefix string) ([]MetaEntry, error) {
	if prefix == "" {
		return []MetaEntry{}, nil
	}
	rows, err := d.readAll(ctx, "SELECT key, value FROM state_meta WHERE key LIKE ? ESCAPE '\\'", EscapeLike(prefix)+"%")
	if err != nil {
		return nil, err
	}
	out := make([]MetaEntry, 0, len(rows))
	for _, row := range rows {
		k, _ := row.Get("key")
		v, _ := row.Get("value")
		key, _ := k.(string)
		if b, ok := v.([]byte); ok {
			v = string(b)
		}
		out = append(out, MetaEntry{Key: key, Value: v})
	}
	return out, nil
}

// Async door onto db
func NewAsyncSessionDB(db *SessionDB) *AsyncSessionDB { return &AsyncSessionDB{db: db} }

// Wrapped store, the attribute access of a non callable member
func (a *AsyncSessionDB) DB() *SessionDB { return a.db }

// Runs fn against the wrapped store on its own goroutine, returning early with the context error when ctx ends first
func AsyncCall[T any](ctx context.Context, a *AsyncSessionDB, fn func(db *SessionDB) (T, error)) (T, error) {
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	go func() {
		v, err := fn(a.db)
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.value, r.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// Sets SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE on the connection, the sqlite3 Connection setconfig call
func setNoCheckpointOnClose(conn SQLiteConn) error {
	return withRawSQLite(conn, func(tls *libc.TLS, db uintptr) error {
		va := libc.NewVaList(int32(1), uintptr(0))
		defer libc.Xfree(tls, va)
		if rc := sqlite3.Xsqlite3_db_config(tls, db, sqlite3.SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE, va); rc != sqlite3.SQLITE_OK {
			return sqliteHandleError(tls, db)
		}
		return nil
	})
}
