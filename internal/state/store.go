package state

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/home"
)

// Label every state database log line and refusal names the file by
const storeLabel = "state.db"

// How a store is opened
type Options struct {
	// Explicit database file, the state database of the home in effect when empty
	Path string
	// Attach without schema work or a write lock, the file having to exist already
	ReadOnly bool
	// Shared database settings of config.yaml
	Database Database
	// Session table behavior the caller resolved from config.yaml and its process wiring
	Sessions SessionOptions
}

// SQLite session store, one serialized writer connection and a bounded pool of read connections on one file
type Store struct {
	eng      *Engine
	path     string
	readOnly bool
	settings Database
	site     string
	tables   layer
	budget   *pathBudget
	// Set once Close let go of the path budget, guarded by the writer lock
	budgetDropped bool
	// Writer lock, one slot so waiting for it can honor a context
	lock      chan struct{}
	conn      *conn
	writerUp  atomic.Bool
	walActive atomic.Bool
	writes    atomic.Int64
	guard     guardHeld
	// Identity of the file and sidecar generation the store opened, guarded by the writer lock
	identity      fileIdentity
	identityKnown bool
	appID         uint32
	sidecars      map[string]fileIdentity
	pinned        bool
	flagMu        sync.Mutex
	replaced      bool
	walLost       bool
	corrupt       bool
	corruptReason string
	corruptCause  error
	captureMu     sync.Mutex
	capture       string
	readMu        sync.Mutex
	pool          []*conn
	readClosed    bool
	readFailedAt  time.Time
	shared        atomic.Bool
}

// Opens the session store, writable with its schema initialized unless read-only is asked for
func Open(ctx context.Context, e *Engine, o Options) (*Store, error) {
	return e.openStore(ctx, o, sessionTables(o.Sessions), callerSite())
}

// Function and line of the code that asked for a store, kept for duplicate handle diagnostics
func callerSite() string {
	pc, _, line, ok := runtime.Caller(2)
	if !ok {
		return "unknown"
	}
	name := "?"
	if fn := runtime.FuncForPC(pc); fn != nil {
		name = fn.Name()
	}
	return name + ":" + strconv.Itoa(line)
}

// Database file Options name, the home state database when no path is given
func (e *Engine) storePath(ctx context.Context, explicit string) (string, error) {
	if explicit != "" {
		return filepath.Clean(explicit), nil
	}
	h, err := e.home.Home(ctx)
	if err != nil {
		return "", err
	}
	return home.StateDB.In(h), nil
}

// Opens a store on the layer t, recording the failure cause for the unavailable-store message
func (e *Engine) openStore(ctx context.Context, o Options, t layer, site string) (*Store, error) {
	path, err := e.storePath(ctx, o.Path)
	if err != nil {
		e.setInitError(errText(err))
		return nil, err
	}
	s := &Store{
		eng: e, path: path, readOnly: o.ReadOnly, settings: o.Database, site: site, tables: t,
		lock: make(chan struct{}, 1), sidecars: map[string]fileIdentity{},
	}
	s.budget = e.budgetFor(path)
	if err := s.initialize(ctx); err != nil {
		e.setInitError(errText(err))
		s.closeWriterQuietly()
		e.dropBudget(s.budget)
		return nil, presentable(err)
	}
	s.budget.register(s)
	return s, nil
}

// Opens the connection the mode asks for and records the file identity it opened
func (s *Store) initialize(ctx context.Context) error {
	var err error
	if s.readOnly {
		err = s.openReadOnly(ctx)
	} else {
		err = s.openWriter(ctx)
	}
	if err != nil {
		return err
	}
	s.recordIdentity(ctx)
	return nil
}

// Database file the store opened
func (s *Store) Path() string { return s.path }

// Reports whether the store was attached read-only
func (s *Store) ReadOnly() bool { return s.readOnly }

// Reports whether the store runs in WAL mode with its header confirming it
func (s *Store) WALActive() bool { return s.walActive.Load() }

// Reports whether the store currently holds a writer connection
func (s *Store) hasWriter() bool { return s.writerUp.Load() }

// Takes the writer lock unless ctx ends first
func (s *Store) acquire(ctx context.Context) error {
	select {
	case s.lock <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Returns the writer lock
func (s *Store) release() { <-s.lock }

// Writable open, preflight and zero byte quarantine under the startup lock, then connect with lock patience and one schema repair
func (s *Store) openWriter(ctx context.Context) error {
	if err := s.eng.home.Mkdir(ctx, filepath.Dir(s.path)); err != nil {
		return err
	}
	err := s.openWriterFile(ctx)
	if err != nil {
		if familyOf(err) == notSQLite || !IsMalformedSchema(err) || s.tables.repairSchema == nil || !s.eng.claimRepair(s.path) {
			return err
		}
		slog.Error(fmt.Sprintf("state.db schema is malformed (%s) — attempting automatic repair (a backup copy is made first).", errText(err)))
		s.closeWriterQuietly()
		repaired, rerr := s.tables.repairSchema(ctx, s.eng, s.path)
		if rerr != nil || !repaired {
			return err
		}
		if err := s.connectWithPatience(ctx); err != nil {
			return err
		}
	}
	s.ensureGeneration(ctx)
	if s.walActive.Load() {
		s.guard = s.eng.guardHold(s.path, nil)
	}
	return nil
}

// Preflight, quarantine and connect, serialized under the cross-process quarantine lock when the file is absent or not SQLite
func (s *Store) openWriterFile(ctx context.Context) error {
	if exists(s.path) && !s.eng.hasInvalidHeader(s.path, false) {
		if err := s.eng.preflightWritable(ctx, s.path, storeLabel, false); err != nil {
			return err
		}
		if err := s.quarantineIfInvalid(ctx, false); err != nil {
			return err
		}
		return s.connectWithPatience(ctx)
	}
	if err := s.eng.preflightWritable(ctx, s.path, storeLabel, true); err != nil {
		return err
	}
	lock, err := quarantineLock(ctx, s.path)
	if err != nil {
		return err
	}
	if lock == nil {
		slog.Warn(fmt.Sprintf("startup quarantine lock for %s not acquired within 5s; proceeding", s.path))
	} else {
		defer lock.Release()
	}
	if err := s.eng.preflightWritable(ctx, s.path, storeLabel, false); err != nil {
		return err
	}
	if err := s.quarantineIfInvalid(ctx, lock != nil); err != nil {
		return err
	}
	return s.connectWithPatience(ctx)
}

// Moves an empty or headerless store aside so a fresh one opens, refusing when it could not be moved
func (s *Store) quarantineIfInvalid(ctx context.Context, locked bool) error {
	if !exists(s.path) || !s.eng.hasInvalidHeader(s.path, false) {
		return nil
	}
	size := int64(-1)
	if fi, err := os.Stat(s.path); err == nil {
		size = fi.Size()
	}
	moved, err := s.eng.quarantineInvalid(ctx, s.path, locked)
	if err != nil {
		return err
	}
	where, source := "left in place (it could not be moved aside)", s.path
	if moved != "" {
		where, source = "moved aside to "+moved, moved
	}
	msg := fmt.Sprintf("state.db was empty or damaged (%d bytes) and has been %s; icebath started with a "+
		"fresh, empty session database. To bring old sessions back, run "+
		"`icebath sessions recover --source %s --inspect-only`, or restore a "+
		"snapshot with `/snapshot list` then `/snapshot restore <id>` (terminal `icebath` chat only).", size, where, source)
	slog.Error(msg)
	s.eng.setInitError(msg)
	if moved == "" && exists(s.path) && s.eng.hasInvalidHeader(s.path, false) {
		return contentErr(nil, "%s", msg)
	}
	return nil
}

// Opens and initializes, waiting out a sibling's write lock with slow jitter for the routine patience, other errors at once
func (s *Store) connectWithPatience(ctx context.Context) error {
	deadline := time.Now().Add(WritePatience)
	for {
		err := s.connectAndInit(ctx)
		if err == nil {
			return nil
		}
		if familyOf(err) != callFamily || !isLockError(err) {
			return err
		}
		s.closeWriterQuietly()
		now := time.Now()
		if !now.Before(deadline) {
			logWriteLockHolders(s.path, WritePatience)
			return err
		}
		if serr := sleepCtx(ctx, min(jitter(writeRetrySlowMin, writeRetrySlowMax), max(deadline.Sub(now), time.Millisecond))); serr != nil {
			return serr
		}
	}
}

// Uniform random duration between lo and hi
func jitter(lo, hi time.Duration) time.Duration {
	return lo + time.Duration(rand.Int64N(int64(hi-lo)+1))
}

// Refuses a lost WAL generation, creates the main file owner-only, opens the writer and initializes the tables
func (s *Store) connectAndInit(ctx context.Context) error {
	if err := s.eng.refuseDeletedWAL(s.path); err != nil {
		return err
	}
	if err := s.eng.SecureDBFiles(s.path, true); err != nil {
		return err
	}
	c, err := s.openWriterConn(ctx)
	if err != nil {
		return err
	}
	s.conn = c
	s.writerUp.Store(true)
	return s.initTables(ctx, c)
}

// Refuses to open while a process holds a deleted WAL or SHM generation, before a second WAL is minted
func (e *Engine) refuseDeletedWAL(path string) error {
	if len(DeletedSidecarHolders(path)) == 0 {
		return nil
	}
	slog.Error(deletedWALMsg)
	return ErrDeletedWAL
}

// Writer connection with its journal mode, sidecar modes, configured pragmas, foreign keys and per-connection layer setup
func (s *Store) openWriterConn(ctx context.Context) (*conn, error) {
	c, err := s.eng.connect(ctx, fileURI(s.path), s.path)
	if err != nil {
		return nil, err
	}
	if err := s.setupWriterConn(ctx, c); err != nil {
		closeLogged(c, "writer", s.path)
		return nil, err
	}
	return c, nil
}

// Settings a fresh writer connection receives before any table work
func (s *Store) setupWriterConn(ctx context.Context, c *conn) error {
	q := c.q()
	if _, err := q.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", writerBusyTimeout.Milliseconds())); err != nil {
		return err
	}
	mode, err := s.eng.applyJournalMode(ctx, q, storeLabel, s.settings)
	if err != nil {
		return err
	}
	onDisk, _ := onDiskMode(ctx, q)
	s.walActive.Store(mode == string(JournalWAL) && onDisk == string(JournalWAL))
	if err := s.eng.SecureDBFiles(s.path, false); err != nil {
		return err
	}
	applyDatabasePragmas(ctx, q, s.settings, storeLabel)
	if _, err := q.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		return err
	}
	if s.tables.connSetup == nil {
		return nil
	}
	return lendTo(ctx, c, func(q Conn) error { return s.tables.connSetup(ctx, s, q, true) })
}

// Runs fn on a lent runner, then returns the connection to autocommit with no result left open
func lendTo(ctx context.Context, c *conn, fn func(q Conn) error) error {
	r := c.lend(false)
	err := fn(r)
	if serr := r.settle(ctx); err == nil {
		err = serr
	}
	return err
}

// Schema the table layer owns, the engine meta table alone when no layer declares one
func (s *Store) initTables(ctx context.Context, c *conn) error {
	if s.tables.init == nil {
		_, err := c.q().ExecContext(ctx, "CREATE TABLE IF NOT EXISTS state_meta (\n    key TEXT PRIMARY KEY,\n    value TEXT\n)")
		return err
	}
	return lendTo(ctx, c, func(q Conn) error { return s.tables.init(ctx, s, q) })
}

// Read-only attach with no schema work and no write lock, transient disk I/O errors retried a few times
func (s *Store) openReadOnly(ctx context.Context) error {
	for attempt := 0; ; attempt++ {
		err := s.attachReadOnly(ctx)
		if err == nil {
			return nil
		}
		if familyOf(err) != callFamily || attempt >= readIOErrRetries || !isDiskIOError(err) {
			return err
		}
		if serr := sleepCtx(ctx, readIOErrBackoff); serr != nil {
			return serr
		}
	}
}

// One read-only connect with pragmas and the layer probe, closed on any failure
func (s *Store) attachReadOnly(ctx context.Context) error {
	c, err := s.connectReadOnly(ctx)
	if err != nil {
		return err
	}
	applyDatabasePragmas(ctx, c.q(), s.settings, storeLabel)
	if s.tables.probe != nil {
		if err := lendTo(ctx, c, func(q Conn) error { return s.tables.probe(ctx, s, q) }); err != nil {
			closeLogged(c, "read-only attach", s.path)
			return err
		}
	}
	s.conn = c
	s.writerUp.Store(false)
	return nil
}

// Read-only connection waiting out a sibling's commit with the read busy budget
func (s *Store) connectReadOnly(ctx context.Context) (*conn, error) {
	c, err := s.eng.connect(ctx, ReadOnlyURI(s.path), s.path)
	if err != nil {
		return nil, err
	}
	if _, err := c.q().ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", readBusyTimeout.Milliseconds())); err != nil {
		closeLogged(c, "read conn", s.path)
		return nil, err
	}
	return c, nil
}

// Closes a connection, a failed close logged at warning since it leaks a tracked descriptor
func closeLogged(c *conn, label, path string) {
	if err := c.close(); err != nil {
		slog.Warn(fmt.Sprintf("%s close failed for %s: %s", label, path, errText(err)))
	}
}

// Closes a half-initialized writer without masking the error that stopped it
func (s *Store) closeWriterQuietly() {
	if s.conn == nil {
		return
	}
	if err := s.conn.close(); err != nil {
		slog.Debug(fmt.Sprintf("Could not close a state store connection: %s", errText(err)))
	}
	s.conn = nil
	s.writerUp.Store(false)
}

// Mints the once-per-file generation stamp and application id, the first opener winning and racers converging
func (s *Store) ensureGeneration(ctx context.Context) {
	if s.readOnly || s.conn == nil {
		return
	}
	u, err := uuid.NewRandom()
	if err != nil {
		slog.Debug(fmt.Sprintf("state.db generation stamp skipped: %v", err))
		return
	}
	token := hex.EncodeToString(u[:])
	if err := s.acquire(ctx); err != nil {
		slog.Debug(fmt.Sprintf("state.db generation stamp skipped: %v", err))
		return
	}
	defer s.release()
	q := s.conn.q()
	read := func() (string, error) {
		var v sql.NullString
		err := q.QueryRowContext(ctx, "SELECT value FROM state_meta WHERE key = ?", generationKey).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return v.String, err
	}
	stamp, err := read()
	if err == nil && stamp == "" {
		if _, err = q.ExecContext(ctx, "INSERT OR IGNORE INTO state_meta (key, value) VALUES (?, ?)", generationKey, token); err == nil {
			stamp, err = read()
		}
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("state.db generation stamp skipped: %s", errText(err)))
		return
	}
	if stamp != "" {
		token = stamp
	}
	var current sql.NullInt64
	if err := q.QueryRowContext(ctx, "PRAGMA application_id").Scan(&current); err != nil {
		slog.Debug(fmt.Sprintf("state.db generation stamp skipped: %s", errText(err)))
		return
	}
	id := uint32(current.Int64)
	if id == 0 {
		head, perr := strconv.ParseUint(token[:min(8, len(token))], 16, 32)
		if perr != nil {
			slog.Debug(fmt.Sprintf("state.db generation stamp skipped: %v", perr))
			return
		}
		if id = uint32(head & 0x7FFFFFFF); id == 0 {
			id = 1
		}
		if _, err := q.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id=%d", id)); err != nil {
			slog.Debug(fmt.Sprintf("state.db generation stamp skipped: %s", errText(err)))
			return
		}
	}
	s.appID = id
	_, _ = q.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)")
}

// Snapshots the file identity, the sidecar generation and the header application id the store opened
func (s *Store) recordIdentity(ctx context.Context) {
	s.identity, s.identityKnown = statIdentity(s.path)
	s.sidecars = sidecarIdentities(s.path)
	if id, ok := s.eng.readApplicationID(s.path); ok && id != 0 {
		s.appID = id
		return
	}
	if s.conn != nil && s.appID == 0 {
		var id sql.NullInt64
		if err := s.Read(ctx, func(c Conn) error {
			return c.QueryRowContext(ctx, "PRAGMA application_id").Scan(&id)
		}); err == nil && id.Int64 > 0 {
			s.appID = uint32(id.Int64)
		}
	}
}
