package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	sqlite3 "modernc.org/sqlite/lib"
)

// Journal mode a database runs under
type JournalMode string

// Journal modes, delete being the rollback journal for filesystems where WAL is unsafe
const (
	JournalWAL    JournalMode = "wal"
	JournalDelete JournalMode = "delete"
)

// Shared database section of config.yaml as the caller resolved it, nil sizing values leaving SQLite defaults
type Database struct {
	// Configured journal mode, anything other than delete meaning WAL
	JournalMode       JournalMode
	WALAutocheckpoint *int64
	JournalSizeLimit  *int64
	CacheSize         *int64
	MmapSize          *int64
	TempStore         *int64
	// Durability level as written in config, empty when unset
	Synchronous string
}

// Error text substrings of a filesystem that cannot host WAL shared memory or locks
var walIncompatMarkers = []string{"locking protocol", "not authorized", diskIOErrorMarker}

// WAL size cap every WAL activation applies so the file returns space after big transactions
const walSizeLimit = 64 * 1024 * 1024

// Pause between retries of a disk I/O error on the journal mode probe and the WAL pragma
const journalIOErrPause = 50 * time.Millisecond

// Refusal of a configured delete journal mode on a database another opener may hold
const cannotVerifyDeleteMsg = "could not verify journal mode before applying configured journal_mode=delete (database " +
	"is locked — possible concurrent openers); refusing to downgrade a database this process " +
	"does not exclusively own"

// Durability levels config may name and their pragma values
var synchronousLevels = map[string]int{"OFF": 0, "NORMAL": 1, "FULL": 2, "EXTRA": 3}

// Pragma value of synchronous FULL, the macOS floor
const synchronousFull = 2

// Configured mode normalized, delete when asked for and WAL for anything else
func (d Database) journalMode() JournalMode {
	if JournalMode(strings.ToLower(strings.TrimSpace(string(d.JournalMode)))) == JournalDelete {
		return JournalDelete
	}
	return JournalWAL
}

// Lower cased mode of a journal_mode pragma result, empty when it returned no row
func scanMode(ctx context.Context, c Conn, query string) (string, error) {
	var mode sql.NullString
	err := c.QueryRowContext(ctx, query).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(mode.String)), nil
}

// Journal mode the database header reports, false when it cannot be read, disk I/O errors retried a few times
func onDiskMode(ctx context.Context, c Conn) (string, bool) {
	var last error
	for range 4 {
		mode, err := scanMode(ctx, c, "PRAGMA journal_mode")
		if err == nil {
			return mode, mode != ""
		}
		if !isDiskIOError(err) || sleepCtx(ctx, journalIOErrPause) != nil {
			return "", false
		}
		last = err
	}
	slog.Debug(fmt.Sprintf("on-disk journal mode probe: retries exhausted on disk read (%s)", errText(last)))
	return "", false
}

// Waits d unless ctx ends first
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Runs a pragma on macOS only, ignoring any refusal
func darwinPragma(ctx context.Context, c Conn, pragma string) {
	if runtime.GOOS == "darwin" {
		_, _ = c.ExecContext(ctx, pragma)
	}
}

// Full fsync at checkpoints on macOS, whose fsync orders nothing on its own
func macCheckpointBarrier(ctx context.Context, c Conn) {
	darwinPragma(ctx, c, "PRAGMA checkpoint_fullfsync=1")
}

// Synchronous FULL on macOS so a checkpoint racing termination cannot leave half written B-tree pages
func macSynchronousFull(ctx context.Context, c Conn) { darwinPragma(ctx, c, "PRAGMA synchronous=FULL") }

// Size cap and macOS barriers every WAL activation carries
func walCompanions(ctx context.Context, c Conn) {
	if _, err := c.ExecContext(ctx, fmt.Sprintf("PRAGMA journal_size_limit=%d", walSizeLimit)); err != nil {
		slog.Debug(fmt.Sprintf("journal_size_limit not applied: %s", errText(err)))
	}
	macCheckpointBarrier(ctx, c)
	macSynchronousFull(ctx, c)
}

// Version of the SQLite library linked into this binary
func SQLiteVersion() string { return sqlite3.SQLITE_VERSION }

// Check-in identifier of the SQLite library linked into this binary
func SQLiteSourceID() string { return sqlite3.SQLITE_SOURCE_ID }

// Reports whether the linked SQLite carries the WAL-reset bug, fixed in 3.51.3 with backports 3.50.7 and 3.44.6
func WALResetVulnerable() bool { return walResetVulnerable(versionParts(SQLiteVersion())) }

// Reports whether a SQLite version lies in the WAL-reset bug range
func walResetVulnerable(v [3]int) bool {
	less := func(a, b [3]int) bool {
		for i := range a {
			if a[i] != b[i] {
				return a[i] < b[i]
			}
		}
		return false
	}
	switch {
	case less(v, [3]int{3, 7, 0}), !less(v, [3]int{3, 51, 3}):
		return false
	case !less(v, [3]int{3, 50, 7}) && less(v, [3]int{3, 51, 0}):
		return false
	case !less(v, [3]int{3, 44, 6}) && less(v, [3]int{3, 45, 0}):
		return false
	}
	return true
}

// Major, minor and patch numbers of a dotted version, missing parts zero
func versionParts(s string) [3]int {
	var v [3]int
	for i, part := range strings.SplitN(s, ".", 3) {
		v[i], _ = strconv.Atoi(part)
	}
	return v
}

// Reports whether the database file already holds pages, false when the answer is unknown
func hasContent(ctx context.Context, c Conn) bool {
	var pages sql.NullInt64
	return c.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages) == nil && pages.Int64 > 0
}

// File of the main database as the connection reports it, empty for memory or unknown
func connectionFile(ctx context.Context, c Conn) string {
	rows, err := c.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		return ""
	}
	defer rows.Close()
	if !rows.Next() {
		return ""
	}
	var seq sql.NullInt64
	var name, file sql.NullString
	if rows.Scan(&seq, &name, &file) != nil {
		return ""
	}
	return file.String
}

// Error unless SQLite reported delete for an explicit operator request
func verifyDelete(actual string) (string, error) {
	if actual != string(JournalDelete) {
		if actual == "" {
			actual = "no result"
		}
		return "", callErr(nil, "could not set configured journal_mode=delete (got %s)", actual)
	}
	return actual, nil
}

// Sets the configured journal mode, falling back to delete where WAL fails, never downgrading a live WAL
func (e *Engine) applyJournalMode(ctx context.Context, c Conn, label string, d Database) (string, error) {
	configured := d.journalMode()
	if WALResetVulnerable() {
		return e.deleteForWALResetBug(ctx, c, label, configured == JournalDelete)
	}
	current, known := onDiskMode(ctx, c)
	if current == string(JournalWAL) {
		if configured == JournalDelete {
			e.logOnce("delete_overridden", label)
		}
		e.warnWALOnCrossVM(ctx, c)
		walCompanions(ctx, c)
		return string(JournalWAL), nil
	}
	if configured == JournalDelete {
		if !known {
			return "", callErr(nil, "%s", cannotVerifyDeleteMsg)
		}
		mode, err := setModeNoWait(ctx, c, "DELETE")
		if err != nil {
			return "", err
		}
		return verifyDelete(mode)
	}
	if !known {
		e.logOnce("wal_probe_unknown", label)
		return string(JournalWAL), nil
	}
	if file := connectionFile(ctx, c); file != "" && e.OnCrossVMFilesystem(file) {
		e.logOnce("cross_vm_fs", label)
		mode, err := setModeNoWait(ctx, c, "DELETE")
		if err != nil {
			return "", err
		}
		if mode == "" {
			mode = string(JournalDelete)
		}
		return mode, nil
	}
	return e.enableWAL(ctx, c, label, current)
}

// Flips a non-WAL connection to WAL or falls back to delete on a filesystem that cannot host it
func (e *Engine) enableWAL(ctx context.Context, c Conn, label, current string) (string, error) {
	upgrading := current != "" && current != string(JournalWAL) && hasContent(ctx, c)
	activated := func() string {
		if upgrading {
			e.logOnce("journal_upgrade", label, current, current)
		}
		walCompanions(ctx, c)
		return string(JournalWAL)
	}
	mode, err := scanMode(ctx, c, "PRAGMA journal_mode=WAL")
	if err == nil {
		if mode == string(JournalWAL) {
			return activated(), nil
		}
		e.logOnce("wal_fallback", label, fmt.Sprintf("journal_mode=WAL refused without raising (still '%s')", mode))
		if mode == "" {
			mode = string(JournalDelete)
		}
		return mode, nil
	}
	if familyOf(err) != callFamily || !containsAny(lowerText(err), walIncompatMarkers) {
		return "", err
	}
	if isDiskIOError(err) {
		for range 2 {
			if serr := sleepCtx(ctx, journalIOErrPause); serr != nil {
				return "", serr
			}
			retried, retryErr := scanMode(ctx, c, "PRAGMA journal_mode=WAL")
			if retryErr != nil {
				if !isDiskIOError(retryErr) {
					return "", retryErr
				}
				err = retryErr
				continue
			}
			if retried == string(JournalWAL) {
				return activated(), nil
			}
			break
		}
	}
	if now, known := onDiskMode(ctx, c); !known || now == string(JournalWAL) {
		return "", err
	}
	e.logOnce("wal_fallback", label, errText(err))
	if _, deleteErr := setModeNoWait(ctx, c, "DELETE"); deleteErr != nil {
		if familyOf(deleteErr) != callFamily {
			return "", deleteErr
		}
		e.logOnce("wal_delete_fallback_failed", label, errText(err), errText(deleteErr))
		readBack, rerr := scanMode(ctx, c, "PRAGMA journal_mode")
		if rerr != nil || readBack == "" {
			readBack = string(JournalDelete)
		}
		return readBack, nil
	}
	return string(JournalDelete), nil
}

// Runs journal_mode=mode with a zero busy timeout so any other opener refuses the switch instead of waiting it out
func setModeNoWait(ctx context.Context, c Conn, mode string) (string, error) {
	previous := int64(0)
	var v sql.NullInt64
	if c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&v) == nil {
		previous = v.Int64
	}
	if _, err := c.ExecContext(ctx, "PRAGMA busy_timeout=0"); err != nil {
		return "", err
	}
	defer func() {
		_, _ = c.ExecContext(context.WithoutCancel(ctx), fmt.Sprintf("PRAGMA busy_timeout=%d", previous))
	}()
	return scanMode(ctx, c, "PRAGMA journal_mode="+mode)
}

// Logs once per database file when an existing WAL database sits on a cross-VM filesystem
func (e *Engine) warnWALOnCrossVM(ctx context.Context, c Conn) {
	if file := connectionFile(ctx, c); file != "" && e.OnCrossVMFilesystem(file) {
		e.logOnce("cross_vm_fs_existing_wal", file)
	}
}

// Keeps WAL off files a WAL-reset-bug build has not yet put in WAL, never downgrading one already in WAL
func (e *Engine) deleteForWALResetBug(ctx context.Context, c Conn, label string, requireDelete bool) (string, error) {
	current, known := onDiskMode(ctx, c)
	if current == string(JournalWAL) {
		e.logResetBug(label, resetBugKeptWAL)
		if requireDelete {
			e.logOnce("delete_overridden", label)
		}
		e.warnWALOnCrossVM(ctx, c)
		walCompanions(ctx, c)
		return string(JournalWAL), nil
	}
	if !known {
		if requireDelete {
			return "", callErr(nil, "%s", cannotVerifyDeleteMsg)
		}
		e.logResetBug(label, resetBugIndeterminate)
		return string(JournalWAL), nil
	}
	actual, err := setModeNoWait(ctx, c, "DELETE")
	if err != nil {
		if requireDelete || familyOf(err) != callFamily {
			return "", err
		}
		if isLockError(err) {
			e.logResetBug(label, resetBugIndeterminate)
			return current, nil
		}
	}
	if requireDelete {
		if _, err := verifyDelete(actual); err != nil {
			return "", err
		}
	}
	e.logResetBug(label, resetBugDelete)
	return string(JournalDelete), nil
}

// What a WAL-reset-bug build did with the journal mode
type resetBugAction string

// Mode left untouched under concurrent openers, an existing WAL kept, or delete used instead of enabling WAL
const (
	resetBugIndeterminate resetBugAction = "journal mode could not be verified or exclusively switched (database is locked — " +
		"possible concurrent openers); leaving the journal mode untouched (no live downgrade under concurrent openers)"
	resetBugKeptWAL resetBugAction = "is already in WAL mode — leaving WAL in place (no live downgrade under concurrent openers)"
	resetBugDelete  resetBugAction = "using journal_mode=DELETE instead of enabling WAL"
)

// Logs the WAL-reset vulnerability once per database with the action taken
func (e *Engine) logResetBug(label string, action resetBugAction) {
	exe, err := os.Executable()
	if err != nil {
		exe = "unknown binary"
	}
	e.logOnce("wal_reset_bug", label, SQLiteVersion(), exe, string(action))
}

// Level and text of each once-per-database journal mode log, the label filling the first verb
var onceLogs = map[string]struct {
	level slog.Level
	text  string
}{
	"wal_reset_bug": {slog.LevelWarn, "%s: linked SQLite %s (binary %s) is vulnerable to the WAL-reset corruption bug " +
		"(https://sqlite.org/wal.html#walresetbug) — %s. Upgrade to SQLite 3.51.3+ (or backports 3.50.7 / 3.44.6); " +
		"run `icebath update` to get a build linking a fixed SQLite. See `icebath doctor`. This warning fires once per " +
		"process per database."},
	"journal_upgrade": {slog.LevelWarn, "%s: on-disk journal_mode was %s and has been switched to WAL. This rewrites the " +
		"database header and persists after this process exits. If %s was a deliberate choice (for example the " +
		"mitigation for the SQLite WAL-reset bug, or a WAL-unsafe filesystem), setting it with PRAGMA on the file will " +
		"not survive -- every open re-applies the configured mode. Set `database.journal_mode: delete` in config.yaml " +
		"to make it stick. This message fires once per process per database."},
	"wal_fallback": {slog.LevelError, "%s: WAL journal_mode unsupported on this filesystem (%s) — falling back to " +
		"journal_mode=DELETE (slower rollback-journal mode; reduces concurrency but works on NFS/SMB/FUSE/ZFS). See " +
		"https://www.sqlite.org/wal.html for details. This message fires once per process per database."},
	"wal_delete_fallback_failed": {slog.LevelWarn, "%s: both WAL and DELETE journal_mode failed (WAL: %s; DELETE: %s) " +
		"— continuing with the connection's current journal mode (reads/writes still work). Typically seen on APFS " +
		"external SSDs under heavy contention. This message fires once per process per database."},
	"delete_overridden": {slog.LevelError, "%s: database.journal_mode=delete is configured but the on-disk database " +
		"is already WAL; keeping WAL (a live downgrade under open connections can corrupt the DB). To apply " +
		"journal_mode=DELETE, stop every icebath process using this database and run `icebath sessions " +
		"set-journal-mode delete` (add `--db PATH` for a store other than state.db). This message fires once per " +
		"process per database."},
	"wal_probe_unknown": {slog.LevelWarn, "%s: could not verify the on-disk journal mode (database is locked / busy); " +
		"not issuing a journal-mode set-pragma while another connection may hold the file (it could unlink the " +
		"-wal/-shm sidecars that connection still uses). Leaving the file untouched; this connection inherits the " +
		"on-disk mode. This message fires once per process per database."},
	"cross_vm_fs": {slog.LevelWarn, "%s: database directory is on a cross-VM filesystem (virtiofs/9p — typical for " +
		"Docker Desktop / OrbStack / Podman host bind mounts). SQLite WAL shared-memory is not coherent across the VM " +
		"boundary and can silently corrupt the database, so journal_mode=DELETE is used instead. To restore WAL " +
		"concurrency, move the database onto a native volume (e.g. a named Docker volume) instead of a host bind " +
		"mount. This message fires once per process per database."},
	"cross_vm_fs_existing_wal": {slog.LevelError, "%s: existing WAL-mode database is on a cross-VM filesystem " +
		"(virtiofs/9p — typical for Docker Desktop / OrbStack / Podman host bind mounts). SQLite WAL shared-memory is " +
		"not coherent across the VM boundary and concurrent writers can silently corrupt the database. icebath does " +
		"not live-downgrade an on-disk WAL database. Fix one of two ways: stop every icebath process using this " +
		"database and run `icebath sessions set-journal-mode delete` (set `database.journal_mode: delete` in " +
		"config.yaml to keep it), or move the database onto a native volume (e.g. a named Docker volume). This " +
		"message fires once per process per database."},
}

// Emits the once-per-database log of kind for label the first time this process reaches it
func (e *Engine) logOnce(kind, label string, args ...any) {
	if !e.firstLog(kind, label) {
		return
	}
	entry := onceLogs[kind]
	slog.Log(context.Background(), entry.level, fmt.Sprintf(entry.text, append([]any{label}, args...)...))
}

// Pragma value of a configured durability level, false for anything unrecognized
func synchronousLevel(raw string) (int, bool) {
	text := strings.TrimSpace(raw)
	if level, ok := synchronousLevels[strings.ToUpper(text)]; ok {
		return level, true
	}
	if n, err := strconv.Atoi(text); err == nil && n >= 0 && n <= 3 {
		return n, true
	}
	return 0, false
}

// Applies the configured durability level, refusing anything below FULL on macOS
func applySynchronous(ctx context.Context, c Conn, raw, label string) {
	level, ok := synchronousLevel(raw)
	if !ok {
		slog.Warn(fmt.Sprintf("%s: ignoring unrecognized database.synchronous=%q (expected OFF, NORMAL, FULL, EXTRA, or 0-3)", label, raw))
		return
	}
	if runtime.GOOS == "darwin" && level < synchronousFull {
		names := []string{"OFF", "NORMAL", "FULL", "EXTRA"}
		slog.Warn(fmt.Sprintf("%s: refusing database.synchronous=%s on macOS; keeping FULL. Darwin's fsync() does not "+
			"guarantee write ordering, so a lower level readmits the half-written btree pages FULL exists "+
			"to prevent.", label, names[level]))
		return
	}
	_, _ = c.ExecContext(ctx, fmt.Sprintf("PRAGMA synchronous=%d", level))
}

// Applies the configured sizing pragmas and then the durability level, a refused pragma left at its default
func applyDatabasePragmas(ctx context.Context, c Conn, d Database, label string) {
	for _, p := range []struct {
		name  string
		value *int64
	}{
		{"cache_size", d.CacheSize}, {"mmap_size", d.MmapSize}, {"temp_store", d.TempStore},
		{"wal_autocheckpoint", d.WALAutocheckpoint}, {"journal_size_limit", d.JournalSizeLimit},
	} {
		if p.value != nil {
			_, _ = c.ExecContext(ctx, fmt.Sprintf("PRAGMA %s=%d", p.name, *p.value))
		}
	}
	if d.Synchronous != "" {
		applySynchronous(ctx, c, d.Synchronous, label)
	}
}

// Reports whether path sits on a virtiofs or 9p filesystem, cached per resolved directory
func (e *Engine) OnCrossVMFilesystem(path string) bool {
	dir := filepath.Dir(resolvePath(path))
	e.mu.Lock()
	cached, ok := e.crossVM[dir]
	e.mu.Unlock()
	if ok {
		return cached
	}
	onVM := crossVMFSTypes[mountFSType(dir)]
	e.mu.Lock()
	e.crossVM[dir] = onVM
	e.mu.Unlock()
	return onVM
}

// Filesystem types that expose host bind mounts across a VM boundary, where WAL shared memory corrupts silently
var crossVMFSTypes = map[string]bool{"virtiofs": true, "fuse.virtiofs": true, "9p": true, "9p2000": true, "9p2000.l": true, "9p2000.u": true}
