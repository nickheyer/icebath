package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	repairLockPollSeconds       = 0.1
	maxPersistentRepairAttempts = 3
	maxMalformedBackups         = 3
	fingerprintSampleBytes      = 65536
	repairBackupMinFreeBytes    = 256 * 1024 * 1024
	repairBackupFreeFraction    = 0.02
	manualRecoverHint           = "Free disk space, then retry (or recover manually with " +
		"`icebath sessions recover --source %s --inspect-only` first)."
	accessRW = 4 | 2
)

// os.access, a var so tests can race a sibling close against the preflight
var osAccess = pyfs.Access

var (
	dbSidecarSuffixes               = []string{"-wal", "-shm", "-journal"}
	fingerprintVolatileHeaderRanges = [][2]int{{24, 28}, {92, 96}}
	repairFTSTables                 = []string{"messages_fts", "messages_fts_trigram", "messages_fts_cjk"}
	repairFTSObjectRE               = regexp.MustCompile(`^messages_fts(_trigram|_cjk)?(_data|_idx|_content|_docsize|_config|_segdir|_segments)?$`)
	integrityTreeRE                 = mustPyRe(`\bTree (\d+)\b`)
	integrityMissingIndexRE         = mustPyRe(`missing from index (\S+)`)
)

// Outcome of one schema repair pass, empty strings standing for the None values of the hermes report dict
type RepairReport struct {
	Repaired             bool
	Strategy             string
	BackupPath           string
	Error                string
	JournalModeBefore    string
	HasJournalModeBefore bool
	repairAttempted      bool
}

// The three SQLite sidecar paths of a database, present or not
func sidecars(dbPath string) []string {
	out := make([]string, len(dbSidecarSuffixes))
	for i, suffix := range dbSidecarSuffixes {
		out[i] = dbPath + suffix
	}
	return out
}

// Whether a path exists, the errors pathlib treats as absence answering false and any other failing
func pathExists(p string) (bool, error) {
	_, err := os.Stat(p)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP) ||
		errors.Is(err, syscall.EBADF) || (isWindows && (errors.Is(err, syscall.Errno(123)) || errors.Is(err, syscall.Errno(1921)))) {
		return false, nil
	}
	return false, err
}

// Runs a raw file reader only while no connection to the database is live, false when unsafe or unreadable
func readOffline(dbPath, what string, reader func() (string, error)) (string, bool) {
	var out string
	ok := false
	if err := OfflineFileAccess(dbPath, what, func() {
		if s, err := reader(); err == nil {
			out, ok = s, true
		}
	}); err != nil {
		return "", false
	}
	return out, ok
}

// Claims the one shot per process repair attempt for the path, true only for the first caller
func claimRepairAttempt(dbPath string) bool {
	repairAttemptMu.Lock()
	defer repairAttemptMu.Unlock()
	if _, seen := repairAttemptedPaths[dbPath]; seen {
		return false
	}
	repairAttemptedPaths[dbPath] = struct{}{}
	return true
}

// Opens the suffixed lock file beside the database, warning and returning nil on failure
func openLockFile(dbPath, suffix, what, tail string) (string, *os.File) {
	lockPath := dbPath + suffix
	err := os.MkdirAll(filepath.Dir(lockPath), 0o777)
	var handle *os.File
	if err == nil {
		handle, err = os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("Could not open state.db %s lock %s (%s) — %s", what, lockPath, pyOSErrorStr(err), tail))
		return lockPath, nil
	}
	return lockPath, handle
}

// Serializes schema surgery across processes, fn told whether this process holds the repair lock
func crossProcessRepairLock(dbPath string, fn func(holding bool) error) error {
	lockPath, handle := openLockFile(dbPath, ".repair.lock", "repair",
		"skipping schema surgery rather than running it without cross-process authority.")
	if handle == nil {
		return fn(false)
	}
	outcome, handle, err := acquireRepairLock(lockPath, handle, repairLockTimeoutSeconds)
	acquired := err == nil && outcome == lockAcquired
	defer func() {
		if acquired {
			releaseLockHandle(handle, true)
		} else {
			_ = handle.Close()
		}
	}()
	if err != nil {
		return err
	}
	if outcome == lockTimedOut {
		holder, err := describeLockHolder(recordedRepairLockHolder(handle))
		if err != nil {
			return err
		}
		slog.Warn(fmt.Sprintf("state.db repair lock %s held by another process for more than %ss — skipping schema "+
			"surgery in this process to avoid racing the repairer. Recorded holder: %s.",
			lockPath, pyFixed0(repairLockTimeoutSeconds), holder))
	}
	return fn(acquired)
}

// Non blocking cross process lock for one auto maintenance pass, nil meaning skip the pass
func tryAcquireAutoMaintenanceLock(dbPath string) *os.File {
	_, handle := openLockFile(dbPath, ".auto-maintenance.lock", "auto-maintenance", "skipping automatic maintenance.")
	if handle == nil {
		return nil
	}
	if err := tryLockNonblocking(handle); err != nil {
		_ = handle.Close()
		return nil
	}
	return handle
}

// Releases an auto maintenance lock handle and closes it
func releaseAutoMaintenanceLock(handle *os.File) { releaseLockHandle(handle, false) }

// Increments the schema cookie after direct sqlite_master edits so peers drop their cached schema, warning on failure
func bumpSchemaCookie(ctx context.Context, conn SQLiteConn) {
	row, err := fetchOne(ctx, conn, "PRAGMA schema_version")
	if err == nil && row == nil {
		err = errors.New("'NoneType' object is not subscriptable")
	}
	if err == nil {
		var current *big.Int
		if current, err = pyint.Of(row[0]); err == nil {
			next := new(big.Int).And(current.Add(current, big.NewInt(1)), big.NewInt(0x7FFFFFFF))
			_, err = conn.ExecContext(ctx, "PRAGMA schema_version="+next.String())
		}
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("Could not bump state.db schema cookie: %s", errText(err)))
	}
}

// Zeroes the commit counter header fields so ordinary writes leave the fingerprint unchanged
func maskVolatileHeader(head []byte) []byte {
	if len(head) < 96 {
		return head
	}
	buf := append([]byte(nil), head...)
	for _, r := range fingerprintVolatileHeaderRanges {
		clear(buf[r[0]:r[1]])
	}
	return buf
}

// Free space required beyond the copy itself for a volume of the given size
func repairBackupHeadroomBytes(totalBytes uint64) int64 {
	return max(int64(repairBackupMinFreeBytes), int64(float64(totalBytes)*repairBackupFreeFraction))
}

// Bundle size, free bytes and headroom of the database volume, a refusal message when either cannot be read
func diskBudget(dbPath, refusal string) (string, int64, int64, int64) {
	parent := filepath.Dir(dbPath)
	fail := func(err error) (string, int64, int64, int64) {
		return fmt.Sprintf("could not determine free space on %s (%s); refusing the %s rather than risk "+
			"filling the volume", parent, pyOSErrorStr(err), refusal), 0, 0, 0
	}
	st, err := os.Stat(dbPath)
	if err != nil {
		return fail(err)
	}
	need := st.Size()
	for _, p := range sidecars(dbPath) {
		exists, err := pathExists(p)
		if err != nil {
			return fail(err)
		}
		if exists {
			sst, err := os.Stat(p)
			if err != nil {
				return fail(err)
			}
			need += sst.Size()
		}
	}
	total, _, free, err := diskUsageFn(parent)
	if err != nil {
		return fail(err)
	}
	return "", need, int64(free), repairBackupHeadroomBytes(total)
}

// Refusal message unless the repair snapshot, its VACUUM and the promotion fit with headroom to spare
func repairScratchSpaceError(dbPath string) string {
	msg, snapshot, free, headroom := diskBudget(dbPath, "repair snapshot")
	if msg != "" {
		return msg
	}
	if free >= snapshot+2*snapshot+headroom {
		return ""
	}
	return fmt.Sprintf("only %.2fGB free on %s; the repair snapshot needs up to %.2fGB, VACUUM may need another "+
		"%.2fGB, and %.2fGB must remain as headroom. Free disk space, then retry.",
		float64(free)/1e9, filepath.Dir(dbPath), float64(snapshot)/1e9, float64(2*snapshot)/1e9, float64(headroom)/1e9)
}

// Refusal message for the forensic copy when it would leave the volume without headroom
func backupFreeSpaceError(dbPath string) string {
	hint := fmt.Sprintf(manualRecoverHint, dbPath)
	msg, need, free, headroom := diskBudget(dbPath, "forensic copy")
	if msg != "" {
		return msg + ". " + hint
	}
	if free-need >= headroom {
		return ""
	}
	return fmt.Sprintf("only %.2fGB free on %s; copying the damaged DB needs %.2fGB and must leave %.2fGB headroom. %s",
		float64(free)/1e9, filepath.Dir(dbPath), float64(need)/1e9, float64(headroom)/1e9, hint)
}

// Snapshot copy bound scaled by source size with sidecars, never under the repair lock timeout
func repairSnapshotTimeoutSeconds(sourcePath string) (float64, error) {
	var sourceBytes int64
	for _, candidate := range append([]string{sourcePath}, sidecars(sourcePath)...) {
		st, err := os.Stat(candidate)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		sourceBytes += st.Size()
	}
	return max(repairLockTimeoutSeconds, float64(sourceBytes)/repairSnapshotMinThroughputBytesPerSecond), nil
}

// Whether a pre strategy failure proves deterministic corruption rather than an environmental fault
func repairFailureConsumesAttempt(err error) bool {
	if !isDatabaseError(err) {
		return false
	}
	if code, ok := sqliteErrorCode(err); ok {
		primary := code & 0xFF
		return primary == 11 || primary == 26
	}
	text := pystr.Lower(errText(err))
	return strings.Contains(text, "file is not a database") || strings.Contains(text, "database disk image is malformed")
}

func repairLedgerPath(dbPath string) string { return dbPath + ".repair-attempts.json" }

// Size plus a hash of the masked head and the tail sample, false while a live connection makes the read unsafe
func dbFingerprint(dbPath string) (string, bool) {
	return readOffline(dbPath, "fingerprint", func() (string, error) {
		st, err := os.Stat(dbPath)
		if err != nil {
			return "", err
		}
		f, err := os.Open(dbPath)
		if err != nil {
			return "", err
		}
		defer f.Close()
		head, err := readUpTo(f, fingerprintSampleBytes)
		if err != nil {
			return "", err
		}
		if _, err := f.Seek(max(0, st.Size()-fingerprintSampleBytes), io.SeekStart); err != nil {
			return "", err
		}
		var tail []byte
		if st.Size() > fingerprintSampleBytes {
			if tail, err = readUpTo(f, fingerprintSampleBytes); err != nil {
				return "", err
			}
		}
		sum := sha256.Sum256(append(maskVolatileHeader(head), tail...))
		return fmt.Sprintf("%d:%s", st.Size(), hex.EncodeToString(sum[:])[:32]), nil
	})
}

// Reads at most n bytes, fewer at end of file, the way a Python file read does
func readUpTo(r io.Reader, n int) ([]byte, error) {
	buf := make([]byte, n)
	got, err := io.ReadFull(r, buf)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		err = nil
	}
	return buf[:got], err
}

// Digest of the whole main file and every present sidecar, length delimited, false when the read is unsafe
func backupContentIdentity(dbPath string) (string, bool) {
	return readOffline(dbPath, "backup-identity", func() (string, error) {
		hasher := sha256.New()
		labels, paths := []string{"main"}, []string{dbPath}
		for i, p := range sidecars(dbPath) {
			exists, err := pathExists(p)
			if err != nil {
				return "", err
			}
			if exists {
				labels, paths = append(labels, dbSidecarSuffixes[i]), append(paths, p)
			}
		}
		for i, p := range paths {
			st, err := os.Stat(p)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(hasher, "\x00%s:%d\x00", labels[i], st.Size())
			f, err := os.Open(p)
			if err != nil {
				return "", err
			}
			_, err = io.Copy(hasher, f)
			f.Close()
			if err != nil {
				return "", err
			}
		}
		return hex.EncodeToString(hasher.Sum(nil)), nil
	})
}

// Decoded repair ledger object, empty when missing, unreadable, malformed or not an object
func readRepairLedger(dbPath string) *pyjson.Object {
	data, err := os.ReadFile(repairLedgerPath(dbPath))
	if err != nil || !utf8.Valid(data) {
		return pyjson.NewObject()
	}
	raw, err := pyjson.Loads(string(data))
	if obj, ok := raw.(*pyjson.Object); err == nil && ok {
		return obj
	}
	return pyjson.NewObject()
}

// Recorded failure count of a ledger, int of a missing key being zero
func ledgerFailedAttempts(ledger *pyjson.Object) (*big.Int, error) {
	raw, ok := ledger.Get("failed_attempts")
	if !ok {
		return big.NewInt(0), nil
	}
	return pyint.Of(raw)
}

// Whether the current file has already burned its cross restart repair budget
func persistentRepairAttemptsExhausted(dbPath string) (bool, error) {
	ledger := readRepairLedger(dbPath)
	raw, _ := ledger.Get("fingerprint")
	recorded, isStr := raw.(string)
	var same bool
	if fp, ok := dbFingerprint(dbPath); ok {
		same = isStr && recorded == fp
	} else {
		st, err := os.Stat(dbPath)
		if err != nil {
			return false, nil
		}
		same = isStr && strings.HasPrefix(recorded, fmt.Sprintf("%d:", st.Size()))
	}
	if !same {
		return false, nil
	}
	n, err := ledgerFailedAttempts(ledger)
	if err != nil {
		return false, err
	}
	return n.Cmp(big.NewInt(maxPersistentRepairAttempts)) >= 0, nil
}

// Operator diagnostic for an exhausted repair budget carrying the profile selector
func persistentRepairExhaustedError(ctx context.Context, dbPath string) string {
	profileArg := home.ProfileCLISelector(home.Dir(ctx))
	return fmt.Sprintf("automatic repair has already failed %d times on this exact file — the corruption is beyond the "+
		"schema/FTS repair strategies (likely b-tree page damage). Manual recovery required: restore a backup, or "+
		"salvage with `icebath %ssessions recover --source %s --inspect-only`, then (if it reports recoverable) "+
		"`icebath %ssessions recover --source %s --output recovered-state.db` (recovery snapshots the damaged file "+
		"first, then runs the page-level `.recover` lane on the copy; do NOT point a raw `sqlite3` shell at the live "+
		"database). Delete %s to force another automatic attempt.",
		maxPersistentRepairAttempts, profileArg, dbPath, profileArg, dbPath, filepath.Base(repairLedgerPath(dbPath)))
}

// Clears the ledger after a repair or counts one more failure against the current fingerprint, warning on failure
func recordRepairOutcome(dbPath string, repaired bool) {
	ledgerPath := repairLedgerPath(dbPath)
	err := func() error {
		if repaired {
			if err := os.Remove(ledgerPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return nil
		}
		ledger := readRepairLedger(dbPath)
		raw, _ := ledger.Get("fingerprint")
		recorded, isStr := raw.(string)
		fp, ok := dbFingerprint(dbPath)
		if !ok {
			if !isStr {
				return nil
			}
			fp = recorded
		}
		attempts := big.NewInt(1)
		if isStr && recorded == fp {
			n, err := ledgerFailedAttempts(ledger)
			if err != nil {
				return err
			}
			attempts = n.Add(n, big.NewInt(1))
		}
		out := pyjson.NewObject()
		out.Set("fingerprint", fp)
		out.Set("failed_attempts", pyint.Native(attempts))
		out.Set("last_attempt", time.Now().Format("2006-01-02T15:04:05"))
		text, err := pyjson.Dumps(out)
		if err != nil {
			return err
		}
		return os.WriteFile(ledgerPath, []byte(text), 0o666)
	}()
	if err != nil {
		slog.Warn(fmt.Sprintf("Could not update state.db repair ledger: %s", pyOSErrorStr(err)))
	}
}

// Timestamped forensic backups of the database, newest first
func existingMalformedBackups(dbPath string) []string {
	prefix := filepath.Base(dbPath) + ".malformed-backup-"
	parent := filepath.Dir(dbPath)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil
	}
	var found []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || hasAnySuffix(name, dbSidecarSuffixes) {
			continue
		}
		found = append(found, filepath.Join(parent, name))
	}
	for i, j := 0, len(found)-1; i < j; i, j = i+1, j-1 {
		found[i], found[j] = found[j], found[i]
	}
	return found
}

func hasAnySuffix(s string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}

// Deletes all but the newest forensic backups with their sidecars
func pruneMalformedBackups(dbPath string, keep int) {
	backups := existingMalformedBackups(dbPath)
	if len(backups) <= keep {
		return
	}
	for _, stale := range backups[keep:] {
		for _, victim := range append([]string{stale}, sidecars(stale)...) {
			if err := os.Remove(victim); err != nil && !errors.Is(err, fs.ErrNotExist) {
				slog.Warn(fmt.Sprintf("Could not prune stale DB backup %s: %s", victim, pyOSErrorStr(err)))
			}
		}
	}
}

// Copies the database and sidecars to staging names, then publishes sidecars first and the main file last
func publishBackupBundle(dbPath, staging, backupPath string) error {
	type member struct{ src, staged, dst string }
	main := member{dbPath, staging, backupPath}
	var side []member
	for i, sidecar := range sidecars(dbPath) {
		exists, err := pathExists(sidecar)
		if err != nil {
			return err
		}
		if exists {
			suffix := dbSidecarSuffixes[i]
			side = append(side, member{sidecar, staging + suffix, backupPath + suffix})
		}
	}
	var published []string
	err := func() error {
		for _, m := range append([]member{main}, side...) {
			if err := copyFileFn(m.src, m.staged); err != nil {
				return err
			}
		}
		for _, m := range append(append([]member(nil), side...), main) {
			if err := replaceFileFn(m.staged, m.dst); err != nil {
				return err
			}
			published = append(published, m.dst)
		}
		return nil
	}()
	if err != nil {
		for _, m := range append(append([]member(nil), side...), main) {
			_ = os.Remove(m.staged)
		}
		for _, p := range published {
			_ = os.Remove(p)
		}
	}
	return err
}

// Raw copies a damaged database with sidecars to a timestamped backup, returning the path or the refusal reason
func backupDBFile(dbPath string) (string, string) {
	if HasLiveConnection(dbPath) {
		reason := fmt.Sprintf("a connection to %s is still open in this process; raw-copying it would cancel that "+
			"connection's POSIX advisory locks. Close all SessionDB handles first.", dbPath)
		slog.Error(fmt.Sprintf("Refusing to raw-copy %s for backup: %s", dbPath, reason))
		return "", reason
	}
	stamp := time.Now().Format("20060102_150405")
	backupPath := fmt.Sprintf("%s.malformed-backup-%s", dbPath, stamp)
	for seq := 1; ; seq++ {
		exists, err := pathExists(backupPath)
		if err != nil {
			return "", fmt.Sprintf("backup copy failed: %s", pyOSErrorStr(err))
		}
		if !exists {
			break
		}
		backupPath = fmt.Sprintf("%s.malformed-backup-%s_%d", dbPath, stamp, seq)
	}
	base, parent := filepath.Base(dbPath), filepath.Dir(dbPath)
	if entries, err := os.ReadDir(parent); err == nil {
		for _, pattern := range []string{base + ".backup-staging-*", base + ".malformed-backup-*.incomplete*"} {
			for _, e := range entries {
				if fnmatch.Fnmatch(e.Name(), pattern) {
					_ = os.Remove(filepath.Join(parent, e.Name()))
				}
			}
		}
	}
	if newest := existingMalformedBackups(dbPath); len(newest) > 0 {
		if srcID, ok := backupContentIdentity(dbPath); ok {
			if newestID, ok := backupContentIdentity(newest[0]); ok && newestID == srcID {
				slog.Info(fmt.Sprintf("Reusing existing forensic backup %s (identical to the damaged DB).", newest[0]))
				return newest[0], ""
			}
		}
	}
	if reason := backupFreeSpaceError(dbPath); reason != "" {
		slog.Error(fmt.Sprintf("Refusing forensic backup of %s: %s", dbPath, reason))
		return "", reason
	}
	if err := publishBackupBundle(dbPath, fmt.Sprintf("%s.backup-staging-%s", dbPath, stamp), backupPath); err != nil {
		slog.Warn(fmt.Sprintf("Could not back up malformed DB %s: %s", dbPath, pyOSErrorStr(err)))
		return "", fmt.Sprintf("backup copy failed: %s", pyOSErrorStr(err))
	}
	pruneMalformedBackups(dbPath, maxMalformedBackups)
	return backupPath, ""
}

// True when the path is read write for this user or no longer exists
func rwOrVanished(p string) (bool, error) {
	if osAccess(p, accessRW) {
		return true, nil
	}
	exists, err := pathExists(p)
	if err != nil {
		return false, err
	}
	return !exists || osAccess(p, accessRW), nil
}

// Whether a resolved path lies inside a resolved base directory, the pathlib is_relative_to test
func isRelativeTo(p, base string) bool {
	if p == base {
		return true
	}
	if !strings.HasSuffix(base, string(filepath.Separator)) {
		base += string(filepath.Separator)
	}
	return strings.HasPrefix(p, base)
}

// Refuses or repairs read only database files before the first connection opens
func PreflightDBWritability(ctx context.Context, dbPath, dbLabel string, directoryOnly bool) error {
	if dbPath == ":memory:" || strings.HasPrefix(dbPath, "file:") {
		return nil
	}
	resolvedHome, homeErr := pyRealpath(home.Dir(ctx))
	type item struct {
		path  string
		isDir bool
	}
	items := []item{{filepath.Dir(dbPath), true}}
	if !directoryOnly {
		for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				items = append(items, item{p, false})
			}
		}
	}
	for _, it := range items {
		p := it.path
		if it.isDir {
			if st, err := os.Stat(p); err != nil || !st.IsDir() {
				continue
			}
		}
		ok, err := rwOrVanished(p)
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		x := ""
		if it.isDir {
			x = "x"
		}
		inScope := false
		if resolved, err := pyRealpath(p); err == nil && homeErr == nil {
			inScope = isRelativeTo(resolved, resolvedHome)
			if st, err := os.Stat(p); inScope && err == nil {
				mode := st.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) | 0o600
				if it.isDir {
					mode |= 0o100
				}
				_ = os.Chmod(p, mode)
			}
		}
		if inScope && osAccess(p, accessRW) {
			slog.Info(fmt.Sprintf("%s preflight: repaired read-only %s (chmod u+rw%s)", dbLabel, p, x))
			continue
		}
		walNote := ""
		if strings.HasSuffix(filepath.Base(p), "-wal") {
			walNote = " Do NOT delete the -wal file — it contains committed data that will be merged into the " +
				"database once it is writable."
		}
		kind := "file"
		if it.isDir {
			kind = "directory"
		}
		return &SQLiteError{Class: SQLiteOperationalError, Msg: fmt.Sprintf("%s is not writable: %s %s is read-only "+
			"for this user. Icebath needs read-write access to open the database. Fix with: chmod u+rw%s '%s' (files "+
			"owned by another user may need sudo/chown).%s", dbLabel, kind, p, x, p, walNote)}
	}
	return nil
}

// Tracked autocommit connection for the repair and probe paths with the macOS write barriers applied
func connectRepairDurable(ctx context.Context, dbPath string, timeout float64) (*TrackedConn, error) {
	conn, err := ConnectTracked(dbPath, WithTrackingPath(dbPath), WithTimeout(timeout))
	if err != nil {
		return nil, err
	}
	reapplyDurabilityBarriers(ctx, conn)
	return conn, nil
}

// Applies the macOS checkpoint barrier and synchronous FULL, false when the schema still refuses pragmas
func reapplyDurabilityBarriers(ctx context.Context, conn SQLiteConn) bool {
	if err := applyMacOSCheckpointBarrier(ctx, conn); err != nil {
		return false
	}
	return enforceMacOSSynchronousFull(ctx, conn) == nil
}

// Durability barriers plus the configured synchronous level for guest users of the state database
func ApplyDurabilityBarriers(ctx context.Context, conn SQLiteConn) bool {
	ok := reapplyDurabilityBarriers(ctx, conn)
	if cfg, err := config.LoadReadonly(ctx, home.Dir(ctx), os.LookupEnv); err == nil {
		if rawSynchronous, _ := config.CfgGet(cfg, "database", "synchronous"); rawSynchronous != nil {
			_ = applySynchronousPragma(ctx, conn, rawSynchronous, "state.db (guest)")
		}
	}
	return ok
}

// Leaves exclusive locking mode so the file is never left pinned, then closes
func closeUnpinned(ctx context.Context, conn SQLiteConn) error {
	_, _ = conn.ExecContext(ctx, "PRAGMA locking_mode=NORMAL")
	return conn.Close()
}

// Zero timeout connection holding exclusive locking mode after a rolled back begin, closed when exclusion fails
func openExclusive(ctx context.Context, dbPath, begin string) (*TrackedConn, error) {
	conn, err := connectRepairDurable(ctx, dbPath, 0.0)
	if err != nil {
		return nil, err
	}
	for _, statement := range []string{"PRAGMA locking_mode=EXCLUSIVE", begin, "ROLLBACK"} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			_ = closeUnpinned(ctx, conn)
			return nil, err
		}
	}
	return conn, nil
}

// Runs fn with one connection excluding writers for repair surgery, or with the error that kept exclusion away
func exclusiveRepairDBGuard(ctx context.Context, dbPath string, fn func(guard SQLiteConn, guardErr error) error) error {
	guard, err := openExclusive(ctx, dbPath, "BEGIN EXCLUSIVE")
	if err != nil {
		if _, ok := sqliteClassOf(err); !ok && !isOSError(err) {
			return err
		}
		return fn(nil, err)
	}
	ferr := fn(guard, nil)
	if cerr := closeUnpinned(ctx, guard); ferr == nil {
		return cerr
	}
	return ferr
}

// Copies one complete SQLite snapshot through the online backup API without replacing either file inode
func copyDatabaseSnapshot(ctx context.Context, sourcePath, destinationPath string, sourceConn, destinationConn SQLiteConn) (err error) {
	deadlineSeconds, err := repairSnapshotTimeoutSeconds(sourcePath)
	if err != nil {
		return err
	}
	deadline := snapshotClock().Add(time.Duration(deadlineSeconds * float64(time.Second)))
	source := sourceConn
	if source == nil {
		owned, err := repairConnectFn(ctx, sourcePath, 5.0)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := owned.Close(); err == nil {
				err = cerr
			}
		}()
		source = owned
	}
	if destinationConn != nil {
		auto, err := sqliteAutocommit(destinationConn)
		if err != nil {
			return err
		}
		if !auto {
			return &SQLiteError{Class: SQLiteProgrammingError, Msg: "SQLite repair backup destination has an active transaction"}
		}
	}
	destination := destinationConn
	if destination == nil {
		owned, err := repairConnectFn(ctx, destinationPath, 5.0)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := owned.Close(); err == nil {
				err = cerr
			}
		}()
		destination = owned
	}
	return sqliteBackup(source, destination, 256, func(int, int, int) error {
		if !snapshotClock().Before(deadline) {
			return fmt.Errorf("timed out copying SQLite repair snapshot after %ss", pyFixed0(deadlineSeconds))
		}
		return nil
	}, repairLockPollSeconds)
}

// Whether an error says the core or FTS tables are not built yet
func schemaNotBuilt(err error) bool {
	text := pystr.Lower(errText(err))
	return strings.Contains(text, "no such table") || strings.Contains(text, "no such column")
}

// One sqlite_master row of the rootpage map an integrity check names its damaged trees through
type SQLiteMasterRow struct {
	RootPage int64
	Type     string
	Name     string
}

// True when integrity_check output names damage outside the FTS shadow set or in the freelist
func IntegrityDamageIsStructural(integrityLines []string, masterRows []SQLiteMasterRow) bool {
	nameByRootpage := map[int64]string{}
	for _, row := range masterRows {
		if row.RootPage != 0 {
			nameByRootpage[row.RootPage] = row.Name
		}
	}
	for _, text := range integrityLines {
		if strings.HasPrefix(text, "Freelist") {
			return true
		}
		if m, _ := integrityTreeRE.FindStringMatch(text); m != nil {
			if n, err := pyint.Of(m.GroupByNumber(1).String()); err == nil && n.IsInt64() {
				if name := nameByRootpage[n.Int64()]; name != "" && !repairFTSObjectRE.MatchString(name) {
					return true
				}
			}
		}
		if m, _ := integrityMissingIndexRE.FindStringMatch(text); m != nil && !repairFTSObjectRE.MatchString(m.GroupByNumber(1).String()) {
			return true
		}
	}
	return false
}

// Read only integrity check classifying damage as structural, a check that raises counting as structural
func StateDBHasStructuralDamage(dbPath string) (bool, error) {
	uri, err := ReadOnlyDBURI(dbPath)
	if err != nil {
		return false, err
	}
	conn, err := ConnectSQLite(uri, SQLiteConfig{Timeout: 1.0, URI: true})
	if err != nil {
		if _, ok := sqliteClassOf(err); ok {
			return false, nil
		}
		return false, err
	}
	defer conn.Close()
	ctx := context.Background()
	var masterRows []SQLiteMasterRow
	var lines []string
	err = func() error {
		rows, err := conn.QueryContext(ctx, "SELECT rootpage, type, name FROM sqlite_master WHERE rootpage > 0")
		if err != nil {
			return err
		}
		for rows.Next() {
			var r SQLiteMasterRow
			var typ, name any
			if err := rows.Scan(&r.RootPage, &typ, &name); err != nil {
				rows.Close()
				return err
			}
			r.Type, r.Name = pyStr(typ), pyStr(name)
			masterRows = append(masterRows, r)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = conn.QueryContext(ctx, "PRAGMA integrity_check")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v any
			if err := rows.Scan(&v); err != nil {
				return err
			}
			lines = append(lines, pystr.Splitlines(pyStr(v), false)...)
		}
		return rows.Err()
	}()
	if err != nil {
		if isOperationalError(err) {
			return false, nil
		}
		if isDatabaseError(err) {
			return true, nil
		}
		return false, err
	}
	return IntegrityDamageIsStructural(lines, masterRows), nil
}

// Probes a database on a fresh connection, returning the reason it is unhealthy or false when it opens cleanly
func dbOpensCleanly(ctx context.Context, dbPath string) (string, bool, error) {
	conn, err := repairConnectFn(ctx, dbPath, 5.0)
	if err != nil {
		return "", false, err
	}
	reason, sick, err := dbProbeOn(ctx, conn)
	if cerr := conn.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil && isDatabaseError(err) {
		return errText(err), true, nil
	}
	return reason, sick, err
}

// Health probe body of dbOpensCleanly, database errors returned for the caller to report as the reason
func dbProbeOn(ctx context.Context, conn SQLiteConn) (string, bool, error) {
	LoadFTS5CJKExtension(ctx, conn)
	if _, err := fetchOne(ctx, conn, "PRAGMA journal_mode"); err != nil {
		return "", false, err
	}
	rows, err := conn.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return "", false, err
	}
	var problems []string
	for rows.Next() {
		var v any
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return "", false, err
		}
		if text := pyStr(v); pystr.Lower(text) != "ok" {
			problems = append(problems, text)
		}
	}
	if err := rows.Close(); err != nil {
		return "", false, err
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	if len(problems) > 0 {
		return strings.Join(problems[:min(3, len(problems))], "; "), true, nil
	}
	if _, err := fetchOne(ctx, conn, "SELECT COUNT(*) FROM sessions"); err != nil {
		return "", false, err
	}
	for _, table := range repairFTSTables {
		_, err := fetchOne(ctx, conn, fmt.Sprintf(`SELECT 1 FROM %s WHERE %s MATCH '""' LIMIT 1`, table, table))
		if err == nil {
			continue
		}
		if !isDatabaseError(err) {
			return "", false, err
		}
		benign := isFTS5UnavailableError(err) || schemaNotBuilt(err)
		if !(isOperationalError(err) && benign) {
			return fmt.Sprintf("fts5 read probe failed on %s: %s", table, errText(err)), true, nil
		}
	}
	probeSessionID := fmt.Sprintf("_hermes_fts_health_probe_%d", time.Now().UnixNano())
	defer func() { _, _ = conn.ExecContext(ctx, "ROLLBACK") }()
	err = func() error {
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO sessions (id, source, started_at) VALUES (?, ?, ?)",
			probeSessionID, "_health_probe", now()); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO messages (session_id, role, content, timestamp) VALUES (?, ?, ?, ?)",
			probeSessionID, "user", "_fts_health_probe", now()); err != nil {
			return err
		}
		for _, table := range repairFTSTables {
			_, err := conn.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s(%s) VALUES('flush')", table, table))
			if err != nil && !(isOperationalError(err) && (isFTS5UnavailableError(err) || schemaNotBuilt(err))) {
				return err
			}
		}
		return nil
	}()
	if err != nil {
		if !isDatabaseError(err) {
			return "", false, err
		}
		if schemaNotBuilt(err) || strings.Contains(pystr.Lower(errText(err)), "no such tokenizer: cjk_unicode61") {
			return "", false, nil
		}
		return fmt.Sprintf("fts5 write probe failed: %s", errText(err)), true, nil
	}
	return "", false, nil
}

// Whether a foreign holder or a positive exclusive lock probe says repair lacks exclusive ownership
func liveWriterHoldsDB(ctx context.Context, dbPath string) (bool, error) {
	return LiveWriterHoldsDB(dbPath, func(p string, timeout float64) (RepairConn, error) {
		conn, err := connectRepairDurable(ctx, p, timeout)
		if err != nil {
			return nil, err
		}
		return conn, nil
	})
}

// Records the error on the report and logs it, a corruption proving cause consuming the persistent budget
func repairSkip(report *RepairReport, verb, message string, cause error) *RepairReport {
	if cause != nil && repairFailureConsumesAttempt(cause) {
		report.repairAttempted = true
	}
	report.Error = message
	slog.Error(fmt.Sprintf("state.db repair %s: %s", verb, message))
	return report
}

// Repairs a state database whose schema is malformed or whose FTS indexes reject writes, on a snapshot promoted back
func RepairStateDBSchema(ctx context.Context, dbPath string, backup bool) (*RepairReport, error) {
	report := &RepairReport{}
	reportStartupProgress(900.0, "state_db_repair")
	exists, err := pathExists(dbPath)
	if err != nil {
		return nil, err
	}
	if !exists {
		report.Error = fmt.Sprintf("%s does not exist", dbPath)
		return report, nil
	}
	exhausted, err := repairExhaustedFn(dbPath)
	if err != nil {
		return nil, err
	}
	if exhausted {
		return repairSkip(report, "skipped", persistentRepairExhaustedError(ctx, dbPath), nil), nil
	}
	result := report
	err = repairLockFn(dbPath, func(holding bool) error {
		if !holding {
			_, sick, err := repairProbeFn(ctx, dbPath)
			if err != nil {
				return err
			}
			if !sick {
				report.Repaired, report.Strategy = true, "repaired_by_other_process"
			} else {
				report.Error = "could not obtain the state.db repair lock (held by another process, or the lock " +
					"file was unopenable); skipped schema surgery to avoid racing a concurrent repairer"
			}
			return nil
		}
		exhausted, err := repairExhaustedFn(dbPath)
		if err != nil {
			return err
		}
		if exhausted {
			repairSkip(report, "skipped", persistentRepairExhaustedError(ctx, dbPath), nil)
		} else if held, err := repairLiveWriterFn(ctx, dbPath); err != nil {
			return err
		} else if held {
			repairSkip(report, "skipped", "a live writer still holds state.db; skipped schema surgery to avoid tearing "+
				"b-tree pages under a concurrent writer. Stop the gateway (icebath gateway stop) and retry.", nil)
		} else {
			beforeMode := repairJournalProbeFn(ctx, dbPath)
			if result, err = repairLockedFn(ctx, dbPath, backup, report, beforeMode); err != nil {
				return err
			}
			if result.Repaired {
				result.JournalModeBefore, result.HasJournalModeBefore = beforeMode, true
			}
		}
		attempted := result.repairAttempted
		result.repairAttempted = false
		if attempted || result.Repaired {
			repairRecordFn(dbPath, result.Repaired)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// On disk journal mode before surgery, empty when the file cannot be opened or probed
func probeJournalModeForRepair(ctx context.Context, dbPath string) string {
	conn, err := repairConnectFn(ctx, dbPath, 5.0)
	if err != nil {
		return ""
	}
	defer conn.Close()
	mode, ok, err := OnDiskJournalMode(ctx, conn)
	if err != nil || !ok {
		return ""
	}
	return mode
}

// Reapplies the configured journal mode after surgery on the guard connection, warning when it changed or failed
func restoreJournalModeAfterRepair(ctx context.Context, dbPath, beforeMode string, conn SQLiteConn) error {
	label := filepath.Base(dbPath)
	var after string
	var err error
	if conn == nil {
		var owned *TrackedConn
		if owned, err = repairConnectFn(ctx, dbPath, 5.0); err == nil {
			after, err = applyWALFn(ctx, owned, label, false)
			if cerr := owned.Close(); err == nil {
				err = cerr
			}
		}
	} else {
		after, err = applyWALFn(ctx, conn, label, false)
	}
	if err != nil {
		if _, ok := sqliteClassOf(err); !ok && !isOSError(err) {
			return err
		}
		slog.Warn(fmt.Sprintf("state.db repair at %s: post-surgery journal-mode restore failed (%s); verify with PRAGMA "+
			"journal_mode on the next open", dbPath, errText(err)))
		return nil
	}
	if beforeMode != "" && after != beforeMode {
		slog.Warn(fmt.Sprintf("state.db repair changed journal_mode %s -> %s (pre-surgery probe %s; restore resolved "+
			"through apply_wal_with_fallback per database.journal_mode and the WAL-reset gate)",
			pyRepr(beforeMode), pyRepr(after), pyRepr(beforeMode)))
	}
	return nil
}

func noneOr(s string) string {
	if s == "" {
		return "None"
	}
	return s
}

// Runs the repair strategies on a scratch snapshot and promotes a success transactionally, under the repair lock
func repairStateDBSchemaLocked(ctx context.Context, dbPath string, backup bool, report *RepairReport, journalModeBefore string) (*RepairReport, error) {
	scratch := dbPath + ".repair-scratch"
	if cleanupError := repairUnlinkFn(scratch); cleanupError != "" {
		return repairSkip(report, "aborted", "could not remove a stale repair snapshot before probing state.db: "+cleanupError, nil), nil
	}
	if _, sick, err := repairProbeFn(ctx, dbPath); err != nil {
		return nil, err
	} else if !sick {
		report.Repaired, report.Strategy = true, "already_healthy"
		return report, nil
	}
	if backup {
		bpath, backupError := repairBackupFn(dbPath)
		report.BackupPath = bpath
		if bpath == "" {
			return repairSkip(report, "aborted", "pre-repair backup refused; aborting schema repair to avoid "+
				"mutating the only copy of the damaged DB: "+backupError, nil), nil
		}
	}
	err := repairGuardFn(ctx, dbPath, func(liveGuard SQLiteConn, guardErr error) error {
		if liveGuard == nil {
			repairSkip(report, "skipped", "could not acquire exclusive state.db repair ownership; skipped schema "+
				"surgery to avoid overwriting a concurrent writer. Stop the gateway and retry: "+errText(guardErr), guardErr)
			return nil
		}
		if spaceError := repairScratchSpaceFn(dbPath); spaceError != "" {
			repairSkip(report, "aborted", spaceError, nil)
			return nil
		}
		if err := repairSnapshotFn(ctx, dbPath, scratch, liveGuard, nil); err != nil {
			repairUnlinkFn(scratch)
			repairSkip(report, "aborted", fmt.Sprintf("could not stage a complete SQLite repair snapshot of %s: %s",
				dbPath, errText(err)), err)
			return nil
		}
		defer func() {
			if cleanupError := repairUnlinkFn(scratch); cleanupError != "" {
				slog.Warn(fmt.Sprintf("Could not remove state.db repair snapshot after repair: %s", cleanupError))
			}
		}()
		report.repairAttempted = true
		if err := repairStrategiesFn(ctx, scratch, report); err != nil {
			return err
		}
		if report.Repaired {
			if err := repairSnapshotFn(ctx, scratch, dbPath, nil, liveGuard); err != nil {
				report.Repaired, report.Strategy = false, ""
				report.repairAttempted = repairFailureConsumesAttempt(err)
				report.Error = fmt.Sprintf("repaired snapshot could not be promoted transactionally: %s", errText(err))
				slog.Error(fmt.Sprintf("state.db repair promotion failed: %s", errText(err)))
			} else {
				slog.Warn(fmt.Sprintf("state.db repaired via '%s' and promoted transactionally: %s", report.Strategy, dbPath))
				if err := restoreJournalModeAfterRepair(ctx, dbPath, journalModeBefore, liveGuard); err != nil {
					return err
				}
			}
		}
		if !report.Repaired {
			slog.Error(fmt.Sprintf("state.db schema repair could not recover %s automatically (no committed canonical "+
				"data was modified or lost; backup: %s); manual restore from backup may be required.",
				dbPath, noneOr(report.BackupPath)))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return report, nil
}

// Removes a path and every SQLite sidecar, returning the joined cleanup failures
func unlinkDBTriple(path string) string {
	var failures []string
	for _, victim := range append([]string{path}, sidecars(path)...) {
		for attempt := 0; attempt < 10; attempt++ {
			err := os.Remove(victim)
			if err == nil || errors.Is(err, fs.ErrNotExist) {
				break
			}
			if errors.Is(err, fs.ErrPermission) && isWindows && attempt < 9 {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			failures = append(failures, fmt.Sprintf("%s: %s", victim, pyOSErrorStr(err)))
			break
		}
	}
	return strings.Join(failures, "; ")
}

// Commits the open transaction if any, what sqlite3 commit does on an autocommit connection
func commitIfInTransaction(ctx context.Context, conn SQLiteConn) error {
	auto, err := sqliteAutocommit(conn)
	if err != nil || auto {
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}

// Runs edit under writable_schema, bumping the schema cookie when it reports a change
func editSQLiteMaster(ctx context.Context, conn SQLiteConn, edit func() (bool, error)) error {
	if _, err := conn.ExecContext(ctx, "PRAGMA writable_schema=ON"); err != nil {
		return err
	}
	changed, err := edit()
	if err != nil {
		return err
	}
	if changed {
		bumpSchemaCookie(ctx, conn)
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA writable_schema=OFF"); err != nil {
		return err
	}
	return commitIfInTransaction(ctx, conn)
}

// Rebuilds each FTS index from its content table, absent tables skipped
func strategyRebuildFTS(ctx context.Context, conn SQLiteConn) error {
	LoadFTS5CJKExtension(ctx, conn)
	for _, table := range repairFTSTables {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s(%s) VALUES('rebuild')", table, table)); err != nil && !isOperationalError(err) {
			return err
		}
	}
	return nil
}

// Rewrites every index b-tree from its canonical table rows
func strategyReindex(ctx context.Context, conn SQLiteConn) error {
	reapplyDurabilityBarriers(ctx, conn)
	if _, err := conn.ExecContext(ctx, "REINDEX"); err != nil {
		return err
	}
	return commitIfInTransaction(ctx, conn)
}

// Deduplicates sqlite_master keeping the lowest rowid per type and name
func strategyDedupSchema(ctx context.Context, conn SQLiteConn) error {
	return editSQLiteMaster(ctx, conn, func() (bool, error) {
		rows, err := conn.QueryContext(ctx, "SELECT type, name, COUNT(*) AS c, MIN(rowid) AS keep FROM sqlite_master "+
			"GROUP BY type, name HAVING c > 1")
		if err != nil {
			return false, err
		}
		type dupe struct {
			typ, name any
			keep      int64
		}
		var dupes []dupe
		for rows.Next() {
			var d dupe
			var count int64
			if err := rows.Scan(&d.typ, &d.name, &count, &d.keep); err != nil {
				rows.Close()
				return false, err
			}
			dupes = append(dupes, d)
		}
		if err := rows.Close(); err != nil {
			return false, err
		}
		if err := rows.Err(); err != nil {
			return false, err
		}
		for _, d := range dupes {
			if _, err := conn.ExecContext(ctx, "DELETE FROM sqlite_master WHERE type IS ? AND name IS ? AND rowid <> ?",
				d.typ, d.name, d.keep); err != nil {
				return false, err
			}
		}
		return len(dupes) > 0, nil
	})
}

// Drops every FTS schema entry and VACUUMs, the indexes rebuilding on the next open
func strategyDropFTSVacuum(ctx context.Context, conn SQLiteConn) error {
	if err := editSQLiteMaster(ctx, conn, func() (bool, error) {
		_, err := conn.ExecContext(ctx, "DELETE FROM sqlite_master WHERE name LIKE 'messages_fts%'")
		return err == nil, err
	}); err != nil {
		return err
	}
	reapplyDurabilityBarriers(ctx, conn)
	_, err := conn.ExecContext(ctx, "VACUUM")
	return err
}

type repairStrategy struct {
	name       string
	body       func(ctx context.Context, conn SQLiteConn) error
	successMsg string
	failureMsg string
}

// Repair strategies in escalation order, the final one with no failure log so its failure lands in the report
var repairStrategies = []repairStrategy{
	{"rebuild_fts", strategyRebuildFTS, "state.db FTS indexes rebuilt in place (schema preserved): %s",
		"state.db FTS in-place rebuild pass failed: %s"},
	{"reindex_btree", strategyReindex, "state.db B-tree indexes rebuilt via REINDEX: %s", "state.db REINDEX pass failed: %s"},
	{"dedup_schema", strategyDedupSchema,
		"state.db schema repaired by de-duplicating sqlite_master (FTS index preserved): %s",
		"state.db dedup repair pass failed: %s"},
	{"drop_fts_rebuild", strategyDropFTSVacuum,
		"state.db schema repaired by dropping FTS schema; indexes will rebuild from messages on next open: %s", ""},
}

// Applies the escalating strategies in place to a scratch copy until one leaves it opening cleanly
func runRepairStrategies(ctx context.Context, dbPath string, report *RepairReport) error {
	for _, s := range repairStrategies {
		err := func() error {
			conn, err := repairConnectFn(ctx, dbPath, 5.0)
			if err != nil {
				return err
			}
			berr := s.body(ctx, conn)
			if cerr := conn.Close(); berr == nil {
				return cerr
			}
			return berr
		}()
		var reason string
		var sick bool
		if err == nil {
			reason, sick, err = repairProbeFn(ctx, dbPath)
		}
		if err != nil {
			if !isDatabaseError(err) {
				return err
			}
			reason, sick = errText(err), true
			if s.failureMsg != "" {
				slog.Warn(fmt.Sprintf(s.failureMsg, errText(err)))
			}
		}
		if !sick {
			report.Repaired, report.Strategy = true, s.name
			slog.Warn(fmt.Sprintf(s.successMsg, dbPath))
			return nil
		}
		if s.failureMsg == "" {
			report.Error = reason
		}
	}
	return nil
}
