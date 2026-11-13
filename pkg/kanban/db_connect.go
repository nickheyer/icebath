package kanban

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	initializedPaths = map[string]bool{}
	initMu           sync.Mutex
	initLock         sync.Mutex
)

const sqliteHeader = "SQLite format 3\x00"

// Busy timeout kanban connections use unless ICEBATH_KANBAN_BUSY_TIMEOUT_MS overrides it
const DefaultBusyTimeoutMS = 120_000

var corruptBackupRetention = 10

var (
	initLockTimeout = 10 * time.Second
	initLockPoll    = 50 * time.Millisecond
)

// Reports whether this process already initialized the resolved database path
func pathInitialized(resolved string) bool {
	initMu.Lock()
	defer initMu.Unlock()
	return initializedPaths[resolved]
}

// Drops a resolved path from the init cache so the next connect reruns the schema pass
func forgetInitialized(resolved string) {
	initMu.Lock()
	delete(initializedPaths, resolved)
	initMu.Unlock()
}

// Busy timeout for kanban connections, long so WAL serializes worker stampedes
func resolveBusyTimeoutMS() int64 {
	return envInt("HERMES_KANBAN_BUSY_TIMEOUT_MS", DefaultBusyTimeoutMS, 1)
}

// Opens a tracked kanban connection with its busy timeout, closing it again when the pragma fails
func sqliteConnect(path string) (state.SQLiteConn, error) {
	ms := resolveBusyTimeoutMS()
	conn, err := state.ConnectTracked(path, state.WithTimeout(float64(ms)/1000.0))
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA busy_timeout=%d", ms)); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// One non blocking exclusive lock attempt, false when held elsewhere, a one byte range at offset zero on Windows
func tryLockNB(handle *os.File) (bool, error) {
	return pyfs.TryLockFile(handle)
}

// Releases the lock tryLockNB took on the same byte range or flock, the OS error surfacing
func unlock(handle *os.File) error {
	return pyfs.UnlockFile(handle)
}

var crossProcessInitLockFn = crossProcessInitLock

// Takes the cross process init lock within the bounded wait, proceeding unlocked with a warning after the deadline
func crossProcessInitLock(path string) (func() error, error) {
	if err := os.MkdirAll(pypath.PathParent(path), 0o777); err != nil {
		return nil, err
	}
	lockPath := path + ".init.lock"
	handle, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return nil, err
	}
	acquired := provider.PollUntil(func() (bool, bool) {
		ok, err := tryLockNB(handle)
		ok = ok && err == nil
		return ok, ok
	}, initLockTimeout, initLockPoll)
	if !acquired {
		log.Warn(fmt.Sprintf("kanban init lock for %s not acquired within %.0fs — proceeding "+
			"without the cross-process lock (in-process lock + idempotent "+
			"init are the correctness backstop). A stuck holder is no longer "+
			"able to block this connect indefinitely (#36644).", lockPath, initLockTimeout.Seconds()))
	}
	return func() error {
		defer handle.Close()
		if acquired {
			return unlock(handle)
		}
		return nil
	}, nil
}

// Non blocking single writer guard around one dispatcher tick, acquired true also when the lock file cannot be opened
func dispatchTickLock(dbPath string) (bool, func()) {
	lockPath := dbPath + ".dispatch.lock"
	if err := os.MkdirAll(pypath.PathParent(lockPath), 0o777); err != nil {
		return true, func() {}
	}
	handle, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return true, func() {}
	}
	acquired, err := tryLockNB(handle)
	acquired = acquired && err == nil
	return acquired, func() {
		if acquired {
			_ = unlock(handle)
		}
		handle.Close()
	}
}

var (
	walCheckpointInterval = 300 * time.Second
	lastWALCheckpoint     = map[string]time.Time{}
	walCheckpointMu       sync.Mutex
)

// Passive WAL checkpoint at most once per interval per board, never failing the tick
func maybeCheckpointWAL(ctx context.Context, conn state.SQLiteConn, dbPath string) {
	key, err := pypath.Resolve(dbPath)
	if err != nil {
		key = dbPath
	}
	now := time.Now()
	walCheckpointMu.Lock()
	if last, ok := lastWALCheckpoint[key]; ok && now.Sub(last) < walCheckpointInterval {
		walCheckpointMu.Unlock()
		return
	}
	lastWALCheckpoint[key] = now
	walCheckpointMu.Unlock()
	row, err := QueryOne(ctx, conn, "PRAGMA wal_checkpoint(PASSIVE)")
	if err != nil {
		log.Debug(fmt.Sprintf("kanban WAL checkpoint on %s skipped: %s", key, state.ErrText(err)))
		return
	}
	shown := "None"
	if row != nil {
		shown = pyrepr.Repr(pyrepr.Tuple(row.Vals))
	}
	log.Debug(fmt.Sprintf("kanban WAL checkpoint (PASSIVE) on %s -> %s (busy, wal_frames, checkpointed_frames)", key, shown))
}

// Reports whether a TLS record header starts at offset
func looksLikeTLSRecordAt(data []byte, offset int) bool {
	if len(data) < offset+5 {
		return false
	}
	contentType, major, minor := data[offset], data[offset+1], data[offset+2]
	length := int(data[offset+3])<<8 | int(data[offset+4])
	return contentType >= 0x14 && contentType <= 0x17 && major == 0x03 && minor <= 0x04 && length > 0 && length <= 18432
}

// DatabaseError for a non empty file without the SQLite header, naming a TLS record signature when present
func validateSQLiteHeader(path string) error {
	st, err := os.Stat(path)
	if err != nil || st.Size() == 0 {
		return nil
	}
	head, ok := state.ReadHeaderBytesPreopen(path, 64, false)
	if !ok || strings.HasPrefix(string(head), sqliteHeader) {
		return nil
	}
	signature := ""
	if strings.HasPrefix(string(head), "SQLit") && looksLikeTLSRecordAt(head, 5) {
		signature = " (TLS record header detected at byte offset 5)"
	} else if looksLikeTLSRecordAt(head, 0) {
		signature = " (TLS record header detected at byte offset 0)"
	}
	first := head[:min(32, len(head))]
	hexed := make([]string, len(first))
	for i, b := range first {
		hexed[i] = hex.EncodeToString([]byte{b})
	}
	return &state.SQLiteError{Class: state.SQLiteDatabaseError, Msg: fmt.Sprintf(
		"file is not a database: invalid SQLite header for %s%s; first_32=%s", path, signature, strings.Join(hexed, " "))}
}

// Existing kanban database that failed integrity checks, carrying the path and the backup made first
type KanbanDBCorruptError struct {
	DBPath     string
	BackupPath string
	Reason     string
}

func (e *KanbanDBCorruptError) Error() string {
	return fmt.Sprintf("Refusing to open corrupt kanban DB at %s: %s. Original preserved; backup at %s.",
		e.DBPath, e.Reason, backupLabel(e.BackupPath))
}

// Backup path or the failed marker
func backupLabel(backupPath string) string {
	if backupPath == "" {
		return "<backup failed>"
	}
	return backupPath
}

// Keeps the newest corrupt backups by mtime plus their sidecars, never pruning keep
func pruneCorruptBackups(parent, baseName, keep string) {
	matches, err := pypath.Glob(parent, baseName+".corrupt.*.bak")
	if err != nil {
		return
	}
	var backups []string
	for _, c := range matches {
		if st, err := os.Stat(c); err == nil && st.Mode().IsRegular() && c != keep {
			backups = append(backups, c)
		}
	}
	budget := corruptBackupRetention
	if keep != "" {
		budget--
	}
	if len(backups) <= budget {
		return
	}
	mtime := func(p string) float64 {
		st, err := os.Stat(p)
		if err != nil {
			return 0
		}
		return float64(st.ModTime().UnixNano()) / 1e9
	}
	slices.SortStableFunc(backups, func(a, b string) int {
		ma, mb := mtime(a), mtime(b)
		switch {
		case ma > mb:
			return -1
		case ma < mb:
			return 1
		}
		return 0
	})
	for _, stale := range backups[budget:] {
		for _, victim := range []string{stale, stale + "-wal", stale + "-shm"} {
			os.Remove(victim)
		}
	}
}

// Content addressed copy of a corrupt database and its sidecars, empty when the copy failed
func backupCorruptDB(path string) string {
	resolved, err := pypath.Resolve(path)
	if err != nil {
		resolved = path
	}
	parent, baseName := pypath.PathParent(resolved), pypath.PathName(resolved)
	if state.HasLiveConnection(resolved) {
		log.Error(fmt.Sprintf("refusing to quarantine %s: a connection to it is still open in "+
			"this process, and fingerprinting the file would cancel that "+
			"connection's POSIX locks. Close all connections first.", resolved))
		return ""
	}
	f, err := os.Open(resolved)
	if err != nil {
		return ""
	}
	digest := sha256.New()
	_, err = io.Copy(digest, f)
	f.Close()
	if err != nil {
		return ""
	}
	candidate := pypath.PathJoin(parent, baseName+".corrupt."+hex.EncodeToString(digest.Sum(nil))[:16]+".bak")
	if !pathExists(candidate) {
		if err := pyfs.Copy2(resolved, candidate); err != nil {
			return ""
		}
		pruneCorruptBackups(parent, baseName, candidate)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		sidecar := pypath.PathJoin(parent, baseName+suffix)
		sidecarBackup := pypath.PathJoin(parent, pypath.PathName(candidate)+suffix)
		if !pathExists(sidecar) || pathExists(sidecarBackup) {
			continue
		}
		pyfs.Copy2(sidecar, sidecarBackup)
	}
	return candidate
}

var repairableIndexErrorPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^wrong # of entries in index (?P<index>.+)$`),
	regexp.MustCompile(`^row \d+ missing from index (?P<index>.+)$`),
}

// Reports whether integrity_check output is the single ok row
func integrityMessagesOK(messages []string) bool {
	return len(messages) == 1 && pystr.Lower(pystr.Strip(messages[0])) == "ok"
}

// Every integrity_check message row as a string
func runIntegrityCheck(ctx context.Context, conn state.SQLiteConn) ([]string, error) {
	rows, err := QueryRows(ctx, conn, "PRAGMA integrity_check")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		if len(r.Vals) > 0 && r.Vals[0] != nil {
			out = append(out, pyrepr.Str(r.Vals[0]))
		}
	}
	return out, nil
}

// integrity_check messages from a read write open so a healthy WAL is recovered before judging
func probeIntegrity(ctx context.Context, path string) ([]string, error) {
	probe, err := sqliteConnect(path)
	if err != nil {
		return nil, err
	}
	defer probe.Close()
	return runIntegrityCheck(ctx, probe)
}

// Distinct index names in first appearance order when every message is index repairable, else nil
func repairableIndexNames(messages []string) []string {
	var names []string
	for _, raw := range messages {
		message := pystr.Strip(raw)
		if message == "" {
			continue
		}
		var m []string
		var re *regexp.Regexp
		for _, re = range repairableIndexErrorPatterns {
			if m = re.FindStringSubmatch(message); m != nil {
				break
			}
		}
		if m == nil {
			return nil
		}
		name := pystr.Strip(m[re.SubexpIndex("index")])
		if name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// REINDEX of the named indexes, bare REINDEX when one fails, then the clean flag and post repair messages
func attemptIndexReindexRepair(ctx context.Context, path string, indexNames []string) (bool, []string) {
	conn, err := sqliteConnect(path)
	if err != nil {
		return false, []string{"could not reopen for REINDEX: " + state.ErrText(err)}
	}
	defer conn.Close()
	for _, name := range indexNames {
		if _, err = conn.ExecContext(ctx, `REINDEX "`+strings.ReplaceAll(name, `"`, `""`)+`"`); err != nil {
			break
		}
	}
	if err != nil {
		if _, err := conn.ExecContext(ctx, "REINDEX"); err != nil {
			return false, []string{"REINDEX failed: " + state.ErrText(err)}
		}
	}
	messages, err := runIntegrityCheck(ctx, conn)
	if err != nil {
		return false, []string{"REINDEX failed: " + state.ErrText(err)}
	}
	return integrityMessagesOK(messages), messages
}

// Reports whether the database file is missing, empty or cannot be stat'ed
func missingOrEmpty(resolved string) bool {
	st, err := os.Stat(resolved)
	return err != nil || st.Size() == 0
}

// Messages and corruption reason of an integrity probe, operational errors propagating since a lock is no corruption
func probeForCorruption(ctx context.Context, resolved string) ([]string, string, bool, error) {
	messages, err := probeIntegrity(ctx, resolved)
	if err != nil {
		class, ok := state.SQLiteClassOf(err)
		if !ok || class == state.SQLiteOperationalError || class == state.SQLiteInterfaceError {
			return nil, "", false, err
		}
		return nil, "sqlite refused to open file: " + state.ErrText(err), false, nil
	}
	if integrityMessagesOK(messages) {
		return messages, "", true, nil
	}
	return messages, "integrity_check returned " + firstMessageRepr(messages), true, nil
}

// Repr of the first message or the no row marker
func firstMessageRepr(messages []string) string {
	if len(messages) == 0 {
		return pyrepr.StrRepr("<no row>")
	}
	return pyrepr.StrRepr(messages[0])
}

// Fails closed on a corrupt existing database after backing it up, auto repairing index only damage through REINDEX
func guardExistingDBIsHealthy(ctx context.Context, path string) error {
	resolved, err := pypath.Resolve(path)
	if err != nil {
		return nil
	}
	if missingOrEmpty(resolved) || pathInitialized(resolved) {
		return nil
	}
	messages, reason, _, err := probeForCorruption(ctx, resolved)
	if err != nil {
		return err
	}
	if reason == "" {
		return nil
	}
	backup := backupCorruptDB(resolved)
	if names := repairableIndexNames(messages); len(names) > 0 {
		joined := strings.Join(names, ", ")
		log.Warn(fmt.Sprintf("kanban DB %s failed integrity_check with index-only errors "+
			"(%s); pre-repair backup at %s — attempting REINDEX auto-repair.", resolved, joined, backupLabel(backup)))
		repaired, post := attemptIndexReindexRepair(ctx, resolved, names)
		if repaired {
			log.Warn(fmt.Sprintf("kanban DB %s auto-repaired via REINDEX (%s); "+
				"integrity_check now clean. Pre-repair copy kept at %s.", resolved, joined, backupLabel(backup)))
			return nil
		}
		reason = fmt.Sprintf("%s; REINDEX auto-repair attempted but integrity_check still returned %s", reason, firstMessageRepr(post))
	}
	return &KanbanDBCorruptError{DBPath: resolved, BackupPath: backup, Reason: reason}
}

// Outcome of RepairDB with status ok, repaired, corrupt or missing
type RepairResult struct {
	Status             string
	DBPath             string
	Messages           []string
	PostRepairMessages []string
	BackupPath         string
	Reindexed          []string
}

// Probes a kanban database and applies the narrow REINDEX repair, reporting instead of raising
func RepairDB(ctx context.Context, dbPath string, board *string) (result *RepairResult, err error) {
	path := dbPath
	if path == "" {
		p, err := KanbanDBPath(ctx, board)
		if err != nil {
			return nil, err
		}
		path = p
	}
	resolved, err := pypath.Resolve(path)
	if err != nil {
		resolved = path
	}
	if missingOrEmpty(resolved) {
		return &RepairResult{Status: "missing", DBPath: resolved}, nil
	}
	release, err := crossProcessInitLockFn(resolved)
	if err != nil {
		return nil, err
	}
	defer func() {
		if uerr := release(); uerr != nil {
			result, err = nil, uerr
		}
	}()
	messages, reason, opened, err := probeForCorruption(ctx, resolved)
	if err != nil {
		return nil, err
	}
	if !opened {
		return &RepairResult{Status: "corrupt", DBPath: resolved, Messages: []string{reason}, BackupPath: backupCorruptDB(resolved)}, nil
	}
	if reason == "" {
		return &RepairResult{Status: "ok", DBPath: resolved, Messages: messages}, nil
	}
	backup := backupCorruptDB(resolved)
	names := repairableIndexNames(messages)
	if len(names) == 0 {
		return &RepairResult{Status: "corrupt", DBPath: resolved, Messages: messages, BackupPath: backup}, nil
	}
	repaired, post := attemptIndexReindexRepair(ctx, resolved, names)
	initLock.Lock()
	forgetInitialized(resolved)
	initLock.Unlock()
	status := "corrupt"
	if repaired {
		status = "repaired"
	}
	return &RepairResult{Status: status, DBPath: resolved, Messages: messages, PostRepairMessages: post, BackupPath: backup, Reindexed: names}, nil
}

// Reports whether the connection sees the tasks table, the schema sentinel
func schemaIsPresent(ctx context.Context, conn state.SQLiteConn) (bool, error) {
	row, err := QueryOne(ctx, conn, "SELECT 1 FROM sqlite_master WHERE type='table' AND name='tasks' LIMIT 1")
	if err != nil {
		if class, ok := state.SQLiteClassOf(err); ok && class != state.SQLiteInterfaceError {
			return false, nil
		}
		return false, err
	}
	return row != nil, nil
}

// Opens path with the kanban pragma set and runs underLock in the same init critical section, closing on failure
func openConfigured(ctx context.Context, path string, underLock func(state.SQLiteConn) error) (state.SQLiteConn, error) {
	conn, err := sqliteConnect(path)
	if err != nil {
		return nil, err
	}
	err = func() error {
		initLock.Lock()
		defer initLock.Unlock()
		if _, err := state.ApplyWALWithFallback(ctx, conn, fmt.Sprintf("kanban.db (%s)", pypath.PathName(path)), false); err != nil {
			return err
		}
		for _, pragma := range []string{
			"PRAGMA synchronous=FULL", "PRAGMA wal_autocheckpoint=100", "PRAGMA journal_size_limit=8388608",
			"PRAGMA foreign_keys=ON", "PRAGMA secure_delete=ON", "PRAGMA cell_size_check=ON",
		} {
			if _, err := conn.ExecContext(ctx, pragma); err != nil {
				return err
			}
		}
		return underLock(conn)
	}()
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// ValueError instead of creating a fresh database for an archived or missing board
func refuseDeadBoardResurrection(path string, board *string) error {
	if pathExists(path) {
		return nil
	}
	if board == nil {
		tombstone := pypath.PathJoin(pypath.PathParent(path), "board.json")
		if !pathExists(tombstone) {
			return nil
		}
		data, err := os.ReadFile(tombstone)
		if err != nil {
			return nil
		}
		text, err := pystr.DecodeUTF8Sig(data)
		if err != nil {
			return nil
		}
		raw, err := pyjson.Loads(text)
		if err != nil {
			return nil
		}
		if obj, ok := raw.(*pyjson.Object); ok && pyrepr.Truthy(objGet(obj, "archived")) {
			return &ValueError{fmt.Sprintf("kanban board %s is archived; refusing to recreate its database",
				pyrepr.StrRepr(pypath.PathName(pypath.PathParent(path))))}
		}
		return nil
	}
	slug, err := normalizeBoardSlug(board)
	if err != nil || slug == "" || slug == DefaultBoard {
		return nil
	}
	meta, err := ReadBoardMetadata(&slug)
	if err != nil {
		return err
	}
	if pyrepr.Truthy(objGet(meta, "archived")) {
		return &ValueError{fmt.Sprintf("kanban board %s is archived; refusing to recreate its database", pyrepr.StrRepr(slug))}
	}
	metaPath, err := BoardMetadataPath(&slug)
	if err != nil {
		return err
	}
	if !pathExists(metaPath) {
		return &ValueError{fmt.Sprintf("kanban board %s does not exist; create it with `icebath kanban boards create %s`", pyrepr.StrRepr(slug), slug)}
	}
	return nil
}

var connectFn = connectImpl

// Opens and on first use per path initializes the kanban database, read only for a fenced descendant
func Connect(ctx context.Context, dbPath string, board *string) (state.SQLiteConn, error) {
	return connectFn(ctx, dbPath, board)
}

// Connect behind the connectFn seam
func connectImpl(ctx context.Context, dbPath string, board *string) (conn state.SQLiteConn, err error) {
	path := dbPath
	if path == "" {
		p, err := KanbanDBPath(ctx, board)
		if err != nil {
			return nil, err
		}
		path = p
	}
	if tools.KanbanPathIsFenced(ctx, path) {
		return connectFencedReadOnly(ctx, path)
	}
	if err := refuseDeadBoardResurrection(path, board); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(pypath.PathParent(path), 0o777); err != nil {
		return nil, err
	}
	resolved, err := pypath.Resolve(path)
	if err != nil {
		return nil, err
	}
	if pathInitialized(resolved) {
		present := false
		conn, err := openConfigured(ctx, path, func(c state.SQLiteConn) error {
			var err error
			present, err = schemaIsPresent(ctx, c)
			return err
		})
		if err != nil {
			return nil, err
		}
		if present {
			return conn, nil
		}
		conn.Close()
		initLock.Lock()
		forgetInitialized(resolved)
		initLock.Unlock()
		log.Warn(fmt.Sprintf("kanban DB %s lost its schema after this process initialized it "+
			"(deleted or replaced externally); re-initializing.", path))
	}
	release, err := crossProcessInitLockFn(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if uerr := release(); uerr != nil {
			if conn != nil {
				conn.Close()
			}
			conn, err = nil, uerr
		}
	}()
	if err := state.PreflightDBWritability(ctx, path, fmt.Sprintf("kanban.db (%s)", pypath.PathName(path)), false); err != nil {
		return nil, err
	}
	if err := validateSQLiteHeader(path); err != nil {
		return nil, err
	}
	if err := guardExistingDBIsHealthy(ctx, path); err != nil {
		return nil, err
	}
	if resolved, err = pypath.Resolve(path); err != nil {
		return nil, err
	}
	return openConfigured(ctx, path, func(c state.SQLiteConn) error {
		if pathInitialized(resolved) {
			return nil
		}
		if _, err := c.ExecContext(ctx, SchemaSQL); err != nil {
			return err
		}
		if err := migrateAddOptionalColumns(ctx, c); err != nil {
			return err
		}
		initMu.Lock()
		initializedPaths[resolved] = true
		initMu.Unlock()
		return nil
	})
}

// Read only open of a fenced board that refuses a board without its schema
func connectFencedReadOnly(ctx context.Context, path string) (state.SQLiteConn, error) {
	resolved, err := pypath.Resolve(path)
	if err != nil {
		return nil, err
	}
	uri, err := pypath.AsURI(resolved)
	if err != nil {
		return nil, err
	}
	conn, err := state.ConnectSQLite(uri+"?mode=ro", state.SQLiteConfig{Timeout: 5.0, URI: true})
	if err != nil {
		return nil, err
	}
	present, err := schemaIsPresent(ctx, conn)
	if err != nil || !present {
		conn.Close()
		if err != nil {
			return nil, err
		}
		return nil, &PermissionError{"Kanban descendants require an initialized board; ask its owner to initialize it"}
	}
	return conn, nil
}

// Runs fn on a fresh connection that is always closed afterwards
func ConnectClosing(ctx context.Context, dbPath string, board *string, fn func(conn state.SQLiteConn) error) error {
	conn, err := Connect(ctx, dbPath, board)
	if err != nil {
		return err
	}
	defer conn.Close()
	return fn(conn)
}

// Creates the schema if missing and always reruns the migration pass, returning the path used
func InitDB(ctx context.Context, dbPath string, board *string) (string, error) {
	path := dbPath
	if path == "" {
		p, err := KanbanDBPath(ctx, board)
		if err != nil {
			return "", err
		}
		path = p
	}
	if err := refuseDeadBoardResurrection(path, board); err != nil {
		return "", err
	}
	if err := os.MkdirAll(pypath.PathParent(path), 0o777); err != nil {
		return "", err
	}
	if resolved, err := pypath.Resolve(path); err == nil {
		initLock.Lock()
		forgetInitialized(resolved)
		initLock.Unlock()
	}
	conn, err := Connect(ctx, path, nil)
	if err != nil {
		return "", err
	}
	conn.Close()
	return path, nil
}
