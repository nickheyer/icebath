package state

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

var walIncompatMarkers = []string{"locking protocol", "not authorized", "disk i/o error"}

const walSizeLimitBytes = 64 * 1024 * 1024

const cannotVerifyDeleteMsg = "could not verify journal mode before applying configured journal_mode=delete (database " +
	"is locked — possible concurrent openers); refusing to downgrade a database this process " +
	"does not exclusively own"

// Platform the darwin barriers and the cross VM mount scan gate on, a var so tests pin it
var walPlatform = runtime.GOOS

// Stands in for sqlite3.OperationalError raised with a message
func operationalError(msg string) error {
	return &SQLiteError{Class: SQLiteOperationalError, Msg: msg}
}

// First row of a query as Python values, nil when the query yields no row
func fetchOne(ctx context.Context, conn SQLiteConn, query string, args ...any) ([]any, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	return vals, nil
}

// Lower cased mode of a journal_mode row, empty when there is no row or value
func modeFromRow(row []any) string {
	if len(row) == 0 || row[0] == nil {
		return ""
	}
	return pyLower(pyStrip(pyStr(row[0])))
}

// Journal mode read from the header, false when undeterminable after retrying transient disk errors
var onDiskJournalMode = func(ctx context.Context, conn SQLiteConn) (string, bool, error) {
	var lastErr error
	for range 4 {
		row, err := fetchOne(ctx, conn, "PRAGMA journal_mode")
		if err != nil {
			if !isOperationalError(err) {
				return "", false, err
			}
			if !strings.Contains(pyLower(errText(err)), "disk i/o error") {
				return "", false, nil
			}
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		var mode any
		if len(row) > 0 {
			mode = row[0]
		}
		if raw, ok := mode.([]byte); ok {
			for _, c := range raw {
				if c > 0x7F {
					return "", false, nil
				}
			}
			mode = string(raw)
		}
		if mode == nil {
			return "", false, nil
		}
		return pyLower(pyStrip(pyStr(mode))), true, nil
	}
	slog.Debug(fmt.Sprintf("_on_disk_journal_mode: retries exhausted on disk read (%s)", errText(lastErr)))
	return "", false, nil
}

// Caps the WAL file at 64 MiB after checkpoints, an operational failure only logged
func applyWALSizeLimit(ctx context.Context, conn SQLiteConn) error {
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA journal_size_limit=%d", walSizeLimitBytes)); err != nil {
		if !isOperationalError(err) {
			return err
		}
		slog.Debug(fmt.Sprintf("journal_size_limit not applied: %s", errText(err)))
	}
	return nil
}

// Runs a pragma on macOS only, swallowing operational errors
func darwinPragma(ctx context.Context, conn SQLiteConn, pragma string) error {
	if walPlatform != "darwin" {
		return nil
	}
	if _, err := conn.ExecContext(ctx, pragma); err != nil && !isOperationalError(err) {
		return err
	}
	return nil
}

// Turns on checkpoint_fullfsync on macOS so checkpoints reach the platter before a launchd shutdown
func applyMacOSCheckpointBarrier(ctx context.Context, conn SQLiteConn) error {
	return darwinPragma(ctx, conn, "PRAGMA checkpoint_fullfsync=1")
}

// Forces synchronous FULL on macOS after each WAL activation so a prior NORMAL never sticks
func enforceMacOSSynchronousFull(ctx context.Context, conn SQLiteConn) error {
	return darwinPragma(ctx, conn, "PRAGMA synchronous=FULL")
}

// Applies the size limit and the macOS barriers every WAL activation carries
func applyWALCompanions(ctx context.Context, conn SQLiteConn) error {
	if err := applyWALSizeLimit(ctx, conn); err != nil {
		return err
	}
	if err := applyMacOSCheckpointBarrier(ctx, conn); err != nil {
		return err
	}
	return enforceMacOSSynchronousFull(ctx, conn)
}

// Version string sqlite3_libversion reports for the SQLite library linked into this binary
func LinkedSQLiteVersion() string {
	errstrMu.Lock()
	defer errstrMu.Unlock()
	if errstrTLS == nil {
		errstrTLS = libc.NewTLS()
	}
	return libc.GoString(sqlite3.Xsqlite3_libversion(errstrTLS))
}

// Integer parts of the linked library version, failing package init as a malformed version fails import
var linkedSQLiteVersionInfo = func() VersionTuple {
	parts := strings.Split(LinkedSQLiteVersion(), ".")
	items := make([]any, len(parts))
	for i, p := range parts {
		items[i] = p
	}
	info, err := versionTuple(items)
	if err != nil {
		panic(err)
	}
	return info
}()

// Version tuple of the linked SQLite library as sqlite3.sqlite_version_info
func LinkedSQLiteVersionInfo() VersionTuple { return linkedSQLiteVersionInfo }

// Reports the WAL reset bug for a version, nil meaning the SQLite library linked into this binary
func WALResetVulnerable(versionInfo *VersionTuple) bool {
	if versionInfo == nil {
		return IsSQLiteWALResetVulnerable(linkedSQLiteVersionInfo)
	}
	return IsSQLiteWALResetVulnerable(*versionInfo)
}

// Gate the journal mode policy consults, a var so tests pin the linked library verdict
var walResetVulnerable = func() bool { return WALResetVulnerable(nil) }

// Source id of the linked SQLite library, empty when an in memory database cannot report it
func SQLiteSourceID() string {
	conn, err := ConnectSQLite(":memory:", SQLiteConfig{Timeout: 5.0})
	if err != nil {
		return ""
	}
	defer conn.Close()
	row, err := fetchOne(context.Background(), conn, "SELECT sqlite_source_id()")
	if err != nil || len(row) == 0 || row[0] == nil {
		return ""
	}
	return pyStr(row[0])
}

// Reports whether the file already holds pages, false whenever the answer is unknown
func databaseHasContent(ctx context.Context, conn SQLiteConn) bool {
	row, err := fetchOne(ctx, conn, "PRAGMA page_count")
	if err != nil || len(row) == 0 || row[0] == nil {
		return false
	}
	n, err := pyIntOf(row[0])
	return err == nil && n.Sign() > 0
}

// Configured database journal_mode, wal unless config names delete
func resolveJournalModeImpl(ctx context.Context) string {
	cfg, err := config.LoadReadonly(ctx, home.Dir(ctx), os.LookupEnv)
	if err != nil {
		return "wal"
	}
	var raw any = "wal"
	if database, present := cfg["database"]; present {
		m := config.AsMap(database)
		if m == nil {
			raw = "wal"
		} else if v, ok := m["journal_mode"]; ok {
			raw = v
		}
	}
	mode := ""
	if s, ok := raw.(string); ok {
		mode = pyLower(pyStrip(s))
	}
	if mode == "wal" || mode == "delete" {
		return mode
	}
	return "wal"
}

// Journal mode resolver the policy consults, a var so tests pin the configured mode
var resolveJournalMode = resolveJournalModeImpl

// Configured database journal_mode, wal by default and for invalid values
func ResolveJournalMode(ctx context.Context) string { return resolveJournalMode(ctx) }

// Raised under require WAL when the filesystem cannot provide WAL or the on disk mode cannot be verified
type WALUnsupportedError struct {
	Msg   string
	Cause error
}

func (e *WALUnsupportedError) Error() string { return e.Msg }

// Lets the error match sqlite3.OperationalError so database init handlers still catch it
func (e *WALUnsupportedError) As(target any) bool {
	if t, ok := target.(**SQLiteError); ok {
		*t = &SQLiteError{Class: SQLiteOperationalError, Msg: e.Msg}
		return true
	}
	return false
}

var crossVMFSTypes = map[string]bool{"virtiofs": true, "fuse.virtiofs": true, "9p": true, "9p2000": true, "9p2000.l": true, "9p2000.u": true}

var (
	crossVMFSCacheMu sync.Mutex
	crossVMFSCache   = map[string]bool{}
)

const procSelfMountinfo = "/proc/self/mountinfo"

// Filesystem type of the longest mount point prefixing the directory, empty when unreadable or unmatched
func mountinfoFstype(directory, mountinfoPath string) (string, error) {
	if strings.ContainsRune(mountinfoPath, 0) {
		return "", &pyConvError{"ValueError", "embedded null byte"}
	}
	data, err := os.ReadFile(mountinfoPath)
	if err != nil {
		return "", nil
	}
	bestLen, bestFstype := -1, ""
	for _, line := range pySplitlines(pyDecodeUTF8Replace(data)) {
		fields, tail, _ := strings.Cut(line, " - ")
		parts := pySplitWhitespace(fields)
		if len(parts) < 5 || tail == "" {
			continue
		}
		mountPoint := parts[4]
		if strings.Contains(mountPoint, `\`) {
			if mountPoint, err = pyUnicodeEscapeDecode(latin1Ignore(mountPoint)); err != nil {
				return "", err
			}
		}
		if directory == mountPoint || strings.HasPrefix(directory, strings.TrimRight(mountPoint, "/")+"/") {
			if n := pyLenWTF8(mountPoint); n > bestLen {
				tailParts := pySplitWhitespace(tail)
				if len(tailParts) == 0 {
					return "", &pyConvError{"IndexError", "list index out of range"}
				}
				bestLen, bestFstype = n, tailParts[0]
			}
		}
	}
	return pyLower(bestFstype), nil
}

// Reports a directory on a virtiofs or 9p mount per the mount table, Linux only
func detectCrossVMFS(directory, mountinfoPath string) (bool, error) {
	if walPlatform != "linux" {
		return false, nil
	}
	fstype, err := mountinfoFstype(directory, mountinfoPath)
	return crossVMFSTypes[fstype], err
}

// Reports a path on a cross VM filesystem, cached per resolved directory, a var so tests pin it
var pathOnCrossVMFS = func(path string) (bool, error) {
	if strings.ContainsRune(path, 0) {
		return false, nil
	}
	resolved, err := pyRealpath(path)
	if err != nil {
		return false, nil
	}
	directory := pyDirname(resolved)
	if directory == "" {
		directory = "/"
	}
	crossVMFSCacheMu.Lock()
	cached, ok := crossVMFSCache[directory]
	crossVMFSCacheMu.Unlock()
	if !ok {
		if cached, err = detectCrossVMFS(directory, procSelfMountinfo); err != nil {
			return false, err
		}
		crossVMFSCacheMu.Lock()
		crossVMFSCache[directory] = cached
		crossVMFSCacheMu.Unlock()
	}
	return cached, nil
}

// Filesystem path of the main database, empty for in memory or unknown
func connectionDBFile(ctx context.Context, conn SQLiteConn) (string, error) {
	row, err := fetchOne(ctx, conn, "PRAGMA database_list")
	if err != nil {
		if isOperationalError(err) {
			return "", nil
		}
		return "", err
	}
	if len(row) < 3 {
		return "", nil
	}
	switch v := row[2].(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case []byte:
		if len(v) == 0 {
			return "", nil
		}
	case int64:
		if v == 0 {
			return "", nil
		}
	case float64:
		if v == 0 {
			return "", nil
		}
	}
	return pyStr(row[2]), nil
}

// Fails unless SQLite reported delete for an explicit operator request
func verifyConfiguredDelete(actual string) (string, error) {
	if actual != "delete" {
		got := actual
		if got == "" {
			got = "no result"
		}
		return "", operationalError(fmt.Sprintf("could not set configured journal_mode=delete (got %s)", got))
	}
	return actual, nil
}

// Sets WAL, falling back to DELETE where the filesystem refuses it and never live downgrading an on disk WAL file
func ApplyWALWithFallback(ctx context.Context, conn SQLiteConn, dbLabel string, requireWAL bool) (string, error) {
	lockCtx, release, acquired := config.TryLockConfig(ctx)
	configured, mode, done, err := func() (string, string, bool, error) {
		defer release()
		if !acquired {
			current, known, err := onDiskJournalMode(ctx, conn)
			if err != nil {
				return "", "", true, err
			}
			if known && current == "wal" {
				if walResetVulnerable() {
					mode, err := applyDeleteForWALResetBug(ctx, conn, dbLabel, false)
					return "", mode, true, err
				}
				if err := applyWALCompanions(ctx, conn); err != nil {
					return "", "", true, err
				}
				return "", "wal", true, nil
			}
			if known && current == "delete" && !requireWAL && databaseHasContent(ctx, conn) {
				if walResetVulnerable() {
					logWALResetBugOnce(ctx, dbLabel, false, false)
				}
				return "", "delete", true, nil
			}
		}
		return resolveJournalMode(lockCtx), "", false, nil
	}()
	if done || err != nil {
		return mode, err
	}
	if walResetVulnerable() {
		return applyDeleteForWALResetBug(ctx, conn, dbLabel, configured == "delete")
	}
	current, known, err := onDiskJournalMode(ctx, conn)
	if err != nil {
		return "", err
	}
	if known && current == "wal" {
		if configured == "delete" {
			logOnce("delete_overridden", dbLabel)
		}
		if err := warnExistingWALOnCrossVMFS(ctx, conn); err != nil {
			return "", err
		}
		if err := applyWALCompanions(ctx, conn); err != nil {
			return "", err
		}
		return "wal", nil
	}
	if configured == "delete" {
		if !known {
			return "", operationalError(cannotVerifyDeleteMsg)
		}
		actual, err := setJournalModeNoWait(ctx, conn, "DELETE")
		if err != nil {
			return "", err
		}
		return verifyConfiguredDelete(actual)
	}
	if !known {
		if requireWAL {
			return "", &WALUnsupportedError{Msg: "could not verify the on-disk journal mode (database is locked — possible " +
				"concurrent openers); cannot guarantee WAL"}
		}
		logOnce("wal_probe_unknown", dbLabel)
		return "wal", nil
	}
	dbFile, err := connectionDBFile(ctx, conn)
	if err != nil {
		return "", err
	}
	if dbFile != "" {
		crossVM, err := pathOnCrossVMFS(dbFile)
		if err != nil {
			return "", err
		}
		if crossVM {
			if requireWAL {
				return "", &WALUnsupportedError{Msg: "journal_mode=WAL refused: database is on a cross-VM filesystem (virtiofs/9p) " +
					"where WAL shared-memory silently corrupts"}
			}
			logOnce("cross_vm_fs", dbLabel)
			mode, err := setJournalModeNoWait(ctx, conn, "DELETE")
			if err != nil {
				return "", err
			}
			if mode == "" {
				mode = "delete"
			}
			return mode, nil
		}
	}
	return enableWAL(ctx, conn, dbLabel, requireWAL, current, known)
}

// Flips a non WAL connection on a safe SQLite to WAL, or falls back to DELETE when the filesystem refuses
func enableWAL(ctx context.Context, conn SQLiteConn, dbLabel string, requireWAL bool, currentMode string, known bool) (string, error) {
	upgradingExistingDB := known && currentMode != "wal" && databaseHasContent(ctx, conn)
	walActivated := func() (string, error) {
		if upgradingExistingDB {
			logJournalModeUpgradeOnce(dbLabel, currentMode)
		}
		if err := applyWALCompanions(ctx, conn); err != nil {
			return "", err
		}
		return "wal", nil
	}
	row, err := fetchOne(ctx, conn, "PRAGMA journal_mode=WAL")
	if err == nil {
		mode := modeFromRow(row)
		if mode == "wal" {
			return walActivated()
		}
		silent := &WALUnsupportedError{Msg: fmt.Sprintf("journal_mode=WAL refused without raising (still %s)", pyRepr(mode))}
		if requireWAL {
			return "", silent
		}
		logOnce("wal_fallback", dbLabel, silent)
		if mode == "" {
			mode = "delete"
		}
		return mode, nil
	}
	if !isOperationalError(err) {
		return "", err
	}
	handled, exc := err, err
	msg := pyLower(errText(exc))
	if !containsAny(msg, walIncompatMarkers) {
		return "", handled
	}
	if strings.Contains(msg, "disk i/o error") {
		for range 2 {
			time.Sleep(50 * time.Millisecond)
			row, retryErr := fetchOne(ctx, conn, "PRAGMA journal_mode=WAL")
			if retryErr != nil {
				if !isOperationalError(retryErr) || !strings.Contains(pyLower(errText(retryErr)), "disk i/o error") {
					return "", retryErr
				}
				exc = retryErr
				continue
			}
			if modeFromRow(row) == "wal" {
				return walActivated()
			}
			break
		}
	}
	onDisk, known, probeErr := onDiskJournalMode(ctx, conn)
	if probeErr != nil {
		return "", probeErr
	}
	if !known || onDisk == "wal" {
		return "", handled
	}
	if requireWAL {
		return "", &WALUnsupportedError{Msg: errText(exc), Cause: exc}
	}
	logOnce("wal_fallback", dbLabel, exc)
	if _, deleteErr := setJournalModeNoWait(ctx, conn, "DELETE"); deleteErr != nil {
		if !isOperationalError(deleteErr) {
			return "", deleteErr
		}
		logOnce("wal_delete_fallback_failed", dbLabel, exc, deleteErr)
		readBack := ""
		row, readErr := fetchOne(ctx, conn, "PRAGMA journal_mode")
		if readErr == nil {
			readBack = modeFromRow(row)
		} else if !isOperationalError(readErr) {
			return "", readErr
		}
		if readBack == "" {
			readBack = "delete"
		}
		return readBack, nil
	}
	return "delete", nil
}

// Runs a journal_mode pragma with busy_timeout zero so any other opener fails it at once, restoring the timeout
func setJournalModeNoWait(ctx context.Context, conn SQLiteConn, mode string) (string, error) {
	previous := big.NewInt(0)
	row, err := fetchOne(ctx, conn, "PRAGMA busy_timeout")
	if err != nil {
		if !isOperationalError(err) {
			return "", err
		}
	} else if len(row) > 0 && row[0] != nil {
		if n, convErr := pyIntOf(row[0]); convErr == nil {
			previous = n
		}
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout=0"); err != nil {
		return "", err
	}
	row, err = fetchOne(ctx, conn, "PRAGMA journal_mode="+mode)
	if _, restoreErr := conn.ExecContext(ctx, "PRAGMA busy_timeout="+previous.String()); restoreErr != nil && !isOperationalError(restoreErr) {
		return "", restoreErr
	}
	if err != nil {
		return "", err
	}
	return modeFromRow(row), nil
}

// Logs once per database path when an already WAL database sits on a cross VM filesystem
func warnExistingWALOnCrossVMFS(ctx context.Context, conn SQLiteConn) error {
	dbFile, err := connectionDBFile(ctx, conn)
	if err != nil || dbFile == "" {
		return err
	}
	crossVM, err := pathOnCrossVMFS(dbFile)
	if err != nil {
		return err
	}
	if crossVM {
		logOnce("cross_vm_fs_existing_wal", dbFile)
	}
	return nil
}

// Keeps new databases out of WAL on a SQLite with the WAL reset bug without live downgrading a WAL file
func applyDeleteForWALResetBug(ctx context.Context, conn SQLiteConn, dbLabel string, requireDelete bool) (string, error) {
	current, known, err := onDiskJournalMode(ctx, conn)
	if err != nil {
		return "", err
	}
	if known && current == "wal" {
		logWALResetBugOnce(ctx, dbLabel, true, false)
		if requireDelete {
			logOnce("delete_overridden", dbLabel)
		}
		if err := warnExistingWALOnCrossVMFS(ctx, conn); err != nil {
			return "", err
		}
		if err := applyWALCompanions(ctx, conn); err != nil {
			return "", err
		}
		return "wal", nil
	}
	if !known {
		if requireDelete {
			return "", operationalError(cannotVerifyDeleteMsg)
		}
		logWALResetBugOnce(ctx, dbLabel, true, true)
		return "wal", nil
	}
	actual, err := setJournalModeNoWait(ctx, conn, "DELETE")
	if err != nil {
		if !isOperationalError(err) || requireDelete {
			return "", err
		}
		if IsSQLiteLockError(err) {
			logWALResetBugOnce(ctx, dbLabel, true, true)
			if current == "" {
				return "delete", nil
			}
			return current, nil
		}
		actual = ""
	}
	if requireDelete {
		if _, err := verifyConfiguredDelete(actual); err != nil {
			return "", err
		}
	}
	logWALResetBugOnce(ctx, dbLabel, false, false)
	return "delete", nil
}

// Install method detector the repair hint consults, a var so tests pin the install type
var detectInstallMethod = config.DetectInstallMethod

// Update command lookup the repair hint consults, a var so tests pin the command per method
var recommendedUpdateCommandForMethod = config.RecommendedUpdateCommandForMethod

// Repair hint naming what icebath update can do for this install type
func walResetRepairHint(ctx context.Context) string {
	method, err := detectInstallMethod(home.Dir(ctx), config.GetProjectRoot())
	if err != nil {
		return "install a Python build bundled with SQLite 3.51.3+ (or backports 3.50.7 / 3.44.6) and restart icebath"
	}
	cmd := recommendedUpdateCommandForMethod(method)
	switch method {
	case "git", "unknown":
		return fmt.Sprintf("icebath-managed installs can repair the embedded runtime with `%s`", cmd)
	case "docker":
		return fmt.Sprintf("update the container image with `%s`", cmd)
	}
	return cmd
}

var walResetBugActions = map[string]string{
	"indeterminate": "journal mode could not be verified or exclusively switched (database is locked — possible " +
		"concurrent openers); leaving the journal mode untouched (no live downgrade under concurrent " +
		"openers)",
	"kept_wal": "is already in WAL mode — leaving WAL in place (no live downgrade under concurrent openers)",
	"delete":   "using journal_mode=DELETE instead of enabling WAL",
}

// A once per process per database log line with its dedupe set
type onceLog struct {
	mu      sync.Mutex
	seen    map[string]bool
	level   slog.Level
	message string
}

var onceLogs = map[string]*onceLog{
	"wal_reset_bug": {seen: map[string]bool{}, level: slog.LevelWarn, message: "%s: linked SQLite %s (interpreter %s) is vulnerable to the WAL-reset corruption bug " +
		"(https://sqlite.org/wal.html#walresetbug) — %s. Upgrade to SQLite 3.51.3+ (or backports 3.50.7 / 3.44.6); " +
		"%s. See `icebath doctor`. This warning fires once per process per database."},
	"journal_upgrade": {seen: map[string]bool{}, level: slog.LevelWarn, message: "%s: on-disk journal_mode was %s and has been switched to WAL. This rewrites the database header and " +
		"persists after this process exits. If %s was a deliberate choice (for example the mitigation for the SQLite " +
		"WAL-reset bug, or a WAL-unsafe filesystem), setting it with PRAGMA on the file will not survive -- every " +
		"open re-applies the configured mode. Set `database.journal_mode: delete` in config.yaml to make it stick. " +
		"This message fires once per process per database."},
	"wal_fallback": {seen: map[string]bool{}, level: slog.LevelError, message: "%s: WAL journal_mode unsupported on this filesystem (%s) — falling back to journal_mode=DELETE (slower " +
		"rollback-journal mode; reduces concurrency but works on NFS/SMB/FUSE/ZFS). See " +
		"https://www.sqlite.org/wal.html for details. This message fires once per process per database."},
	"wal_delete_fallback_failed": {seen: map[string]bool{}, level: slog.LevelWarn, message: "%s: both WAL and DELETE journal_mode failed (WAL: %s; DELETE: %s) — continuing with the connection's " +
		"current journal mode (reads/writes still work). Typically seen on APFS external SSDs under heavy " +
		"contention. This message fires once per process per database."},
	"delete_overridden": {seen: map[string]bool{}, level: slog.LevelError, message: "%s: database.journal_mode=delete is configured but the on-disk database is already WAL; keeping WAL (a live " +
		"downgrade under open connections can corrupt the DB). To apply journal_mode=DELETE, stop every icebath " +
		"process using this database and run `icebath sessions set-journal-mode delete` (add `--db PATH` for a " +
		"store other than state.db). This message fires once per process per database."},
	"wal_probe_unknown": {seen: map[string]bool{}, level: slog.LevelWarn, message: "%s: could not verify the on-disk journal mode (database is locked / busy); not issuing a journal-mode " +
		"set-pragma while another connection may hold the file (it could unlink the -wal/-shm sidecars that " +
		"connection still uses). Leaving the file untouched; this connection inherits the on-disk mode. This message fires once per process per database."},
	"cross_vm_fs": {seen: map[string]bool{}, level: slog.LevelWarn, message: "%s: database directory is on a cross-VM filesystem (virtiofs/9p — typical for Docker Desktop / OrbStack / " +
		"Podman host bind mounts). SQLite WAL shared-memory is not coherent across the VM boundary and can silently " +
		"corrupt the database, so journal_mode=DELETE is used instead. To restore WAL concurrency, move the database " +
		"onto a native volume (e.g. a named Docker volume) instead of a host bind mount. This message fires once per " +
		"process per database."},
	"cross_vm_fs_existing_wal": {seen: map[string]bool{}, level: slog.LevelError, message: "%s: existing WAL-mode database is on a cross-VM filesystem (virtiofs/9p — typical for Docker Desktop / " +
		"OrbStack / Podman host bind mounts). SQLite WAL shared-memory is not coherent across the VM boundary and " +
		"concurrent writers can silently corrupt the database. icebath does not live-downgrade an on-disk WAL database. " +
		"Fix one of two ways: stop every icebath process using this database and run `icebath sessions " +
		"set-journal-mode delete` (set `database.journal_mode: delete` in config.yaml to keep it), " +
		"or move the database onto a native volume (e.g. a named Docker volume). This message fires once per process " +
		"per database."},
}

// Text a log argument formats to under percent s, errors as Python's str of the exception
func logArgText(arg any) string {
	switch a := arg.(type) {
	case func() string:
		return a()
	case error:
		return errText(a)
	}
	return pyFormat(arg)
}

// Emits one table log line once per process per label, resolving function arguments only after the dedupe
func logOnce(kind, dbLabel string, args ...any) {
	entry := onceLogs[kind]
	entry.mu.Lock()
	if entry.seen[dbLabel] {
		entry.mu.Unlock()
		return
	}
	entry.seen[dbLabel] = true
	entry.mu.Unlock()
	texts := make([]any, 0, len(args)+1)
	texts = append(texts, dbLabel)
	for _, a := range args {
		texts = append(texts, logArgText(a))
	}
	slog.Log(context.Background(), entry.level, fmt.Sprintf(entry.message, texts...))
}

// Path of the running executable as sys.executable, empty when the platform cannot name it
func sysExecutable() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}

// Warns once per label that the linked SQLite carries the WAL reset bug and which path was taken
func logWALResetBugOnce(ctx context.Context, dbLabel string, keptWAL, indeterminate bool) {
	action := walResetBugActions["delete"]
	if indeterminate {
		action = walResetBugActions["indeterminate"]
	} else if keptWAL {
		action = walResetBugActions["kept_wal"]
	}
	logOnce("wal_reset_bug", dbLabel, LinkedSQLiteVersion(), sysExecutable(), action, func() string { return walResetRepairHint(ctx) })
}

// Warns once per label that an existing non WAL database was switched to WAL
func logJournalModeUpgradeOnce(dbLabel, previousMode string) {
	logOnce("journal_upgrade", dbLabel, previousMode, previousMode)
}

var synchronousLevels = map[string]int{"OFF": 0, "NORMAL": 1, "FULL": 2, "EXTRA": 3}

var synchronousNames = map[int]string{0: "OFF", 1: "NORMAL", 2: "FULL", 3: "EXTRA"}

const synchronousFull = 2

// Pragma level of a database synchronous setting, false for anything unrecognised
func ResolveSynchronousLevel(rawValue any) (int, bool) {
	switch v := rawValue.(type) {
	case bool:
		return 0, !v
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, *big.Int:
		n, err := pyIntOf(fmt.Sprint(v))
		if err != nil || !n.IsInt64() {
			return 0, false
		}
		_, ok := synchronousNames[int(n.Int64())]
		return int(n.Int64()), ok
	}
	text := pyStrip(pyStr(rawValue))
	if level, ok := synchronousLevels[pyUpper(text)]; ok {
		return level, true
	}
	n, err := pyIntOf(text)
	if err != nil || !n.IsInt64() {
		return 0, false
	}
	_, ok := synchronousNames[int(n.Int64())]
	return int(n.Int64()), ok
}

// Sets the configured synchronous level, refusing unknown values and anything below FULL on macOS
func applySynchronousPragma(ctx context.Context, conn SQLiteConn, rawValue any, dbLabel string) error {
	level, ok := ResolveSynchronousLevel(rawValue)
	if !ok {
		slog.Warn(fmt.Sprintf("%s: ignoring unrecognized database.synchronous=%s (expected OFF, NORMAL, FULL, EXTRA, or 0-3)",
			dbLabel, pyReprValue(rawValue)))
		return nil
	}
	if walPlatform == "darwin" && level < synchronousFull {
		slog.Warn(fmt.Sprintf("%s: refusing database.synchronous=%s on macOS; keeping FULL. Darwin's fsync() does not "+
			"guarantee write ordering, so a lower level readmits the half-written btree pages FULL exists "+
			"to prevent.", dbLabel, synchronousNames[level]))
		return nil
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA synchronous=%d", level)); err != nil && !isOperationalError(err) {
		return err
	}
	return nil
}

// Applies the optional sizing pragmas and then the synchronous level from the database config section
func ApplyDatabasePragmas(ctx context.Context, conn SQLiteConn, dbLabel string) error {
	cfg, err := config.LoadReadonly(ctx, home.Dir(ctx), os.LookupEnv)
	if err != nil {
		return nil
	}
	for _, name := range []string{"cache_size", "mmap_size", "temp_store", "wal_autocheckpoint", "journal_size_limit"} {
		rawValue, _ := config.CfgGet(cfg, "database", name)
		if rawValue == nil {
			continue
		}
		value, convErr := pyIntOf(pyStr(rawValue))
		if convErr != nil {
			slog.Warn(fmt.Sprintf("%s: ignoring non-integer database.%s=%s", dbLabel, name, pyReprValue(rawValue)))
			continue
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA %s=%s", name, value.String())); err != nil && !isOperationalError(err) {
			return err
		}
	}
	if rawSynchronous, _ := config.CfgGet(cfg, "database", "synchronous"); rawSynchronous != nil {
		return applySynchronousPragma(ctx, conn, rawSynchronous, dbLabel)
	}
	return nil
}
