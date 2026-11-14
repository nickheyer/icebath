package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/hostinfo"
)

const (
	repairLockTimeout              = 120 * time.Second
	repairLockPoll                 = 100 * time.Millisecond
	repairSnapshotMinThroughput    = 10 * 1024 * 1024
	maxPersistentRepairAttempts    = 3
	maxMalformedBackups            = 3
	fingerprintSampleBytes         = 65536
	repairBackupMinFreeBytes       = 256 * 1024 * 1024
	repairBackupFreeFraction       = 0.02
	repairConnTimeout              = 5 * time.Second
	repairStartupLease             = 900 * time.Second
	repairBackupStepPages          = 256
	repairBackupProgressSleepMilli = 100
)

var repairSidecarSuffixes = []string{"-wal", "-shm", "-journal"}

var repairFTSTables = []string{"messages_fts", "messages_fts_trigram", "messages_fts_cjk"}

const manualRecoverHint = "Free disk space, then retry (or recover manually with " +
	"`icebath sessions recover --source %s --inspect-only` first)."

var (
	ftsObjectName       = regexp.MustCompile(`^messages_fts(_trigram|_cjk)?(_data|_idx|_content|_docsize|_config|_segdir|_segments)?$`)
	integrityTree       = regexp.MustCompile(`\bTree (\d+)\b`)
	integrityMissingIdx = regexp.MustCompile(`missing from index (\S+)`)
)

type RepairOptions struct {
	NoBackup        bool
	Database        Database
	StartupProgress func(expected time.Duration, phase string)
}

type RepairReport struct {
	Repaired          bool
	Strategy          string
	BackupPath        string
	Error             string
	JournalModeBefore string
	attempted         bool
}

type RepairGuard struct {
	c *conn
}

func (g *RepairGuard) Conn() Conn { return g.c.q() }

func (g *RepairGuard) Close() error { return closeUnpinned(context.Background(), g.c) }

type backupError struct {
	code int
	msg  string
}

func (b *backupError) Error() string { return b.msg }

func repairSidecars(path string) []string {
	out := make([]string, len(repairSidecarSuffixes))
	for i, suffix := range repairSidecarSuffixes {
		out[i] = path + suffix
	}
	return out
}

func sqliteOperational(err error) bool {
	code, ok := primaryCode(err)
	return ok && callCode(code)
}

func sqliteFailure(err error) bool {
	var b *backupError
	return familyOf(err) != notSQLite || errors.As(err, &b)
}

func schemaNotBuilt(err error) bool {
	text := lowerText(err)
	return strings.Contains(text, "no such table") || strings.Contains(text, "no such column")
}

func repairConsumesAttempt(err error) bool {
	var b *backupError
	if errors.As(err, &b) {
		code := b.code & 0xff
		return code == sqlite3.SQLITE_CORRUPT || code == sqlite3.SQLITE_NOTADB
	}
	if familyOf(err) == notSQLite {
		return false
	}
	if code, ok := primaryCode(err); ok {
		return code == sqlite3.SQLITE_CORRUPT || code == sqlite3.SQLITE_NOTADB
	}
	text := lowerText(err)
	return strings.Contains(text, "file is not a database") || strings.Contains(text, "database disk image is malformed")
}

func (e *Engine) connectRepairDurable(ctx context.Context, path string, timeout time.Duration) (*conn, error) {
	c, err := e.connect(ctx, fileURI(path), path)
	if err != nil {
		return nil, presentable(err)
	}
	if err := c.raw(func(tls *libc.TLS, db uintptr) error {
		sqlite3.Xsqlite3_busy_timeout(tls, db, int32(timeout.Milliseconds()))
		return nil
	}); err != nil {
		return nil, withCleanup(err, c.close())
	}
	reapplyDurabilityBarriers(ctx, c.q())
	return c, nil
}

func reapplyDurabilityBarriers(ctx context.Context, c Conn) {
	macCheckpointBarrier(ctx, c)
	macSynchronousFull(ctx, c)
}

func ApplyDurabilityBarriers(ctx context.Context, c Conn, d Database) {
	reapplyDurabilityBarriers(ctx, c)
	if d.Synchronous != "" {
		applySynchronous(ctx, c, d.Synchronous, "state.db (guest)")
	}
}

func closeUnpinned(ctx context.Context, c *conn) error {
	_, _ = c.q().ExecContext(ctx, "PRAGMA locking_mode=NORMAL")
	return c.close()
}

func (e *Engine) openExclusive(ctx context.Context, path, begin string) (*conn, error) {
	c, err := e.connectRepairDurable(ctx, path, 0)
	if err != nil {
		return nil, err
	}
	for _, statement := range []string{"PRAGMA locking_mode=EXCLUSIVE", begin, "ROLLBACK"} {
		if _, err := c.q().ExecContext(ctx, statement); err != nil {
			return nil, withCleanup(presentable(err), closeUnpinned(ctx, c))
		}
	}
	return c, nil
}

func (e *Engine) ExclusiveRepairGuard(ctx context.Context, path string) (*RepairGuard, error) {
	c, err := e.openExclusive(ctx, path, "BEGIN EXCLUSIVE")
	if err != nil {
		return nil, err
	}
	return &RepairGuard{c: c}, nil
}

func bumpSchemaCookie(ctx context.Context, c Conn) {
	var current int64
	if err := c.QueryRowContext(ctx, "PRAGMA schema_version").Scan(&current); err != nil {
		slog.Warn(fmt.Sprintf("Could not bump state.db schema cookie: %s", errText(err)))
		return
	}
	if _, err := c.ExecContext(ctx, fmt.Sprintf("PRAGMA schema_version=%d", (current+1)&0x7FFFFFFF)); err != nil {
		slog.Warn(fmt.Sprintf("Could not bump state.db schema cookie: %s", errText(err)))
	}
}

func maskVolatileHeader(head []byte) []byte {
	if len(head) < 96 {
		return head
	}
	out := append([]byte{}, head...)
	for _, r := range [][2]int{{24, 28}, {92, 96}} {
		for i := r[0]; i < r[1]; i++ {
			out[i] = 0
		}
	}
	return out
}

func repairBackupHeadroom(total uint64) uint64 {
	return max(uint64(repairBackupMinFreeBytes), uint64(float64(total)*repairBackupFreeFraction))
}

func diskBudget(path, refusal string) (string, uint64, uint64, uint64) {
	dir := filepath.Dir(path)
	fail := func(err error) (string, uint64, uint64, uint64) {
		return fmt.Sprintf("could not determine free space on %s (%v); refusing the %s rather than risk "+
			"filling the volume", dir, err, refusal), 0, 0, 0
	}
	st, err := os.Stat(path)
	if err != nil {
		return fail(err)
	}
	need := uint64(st.Size())
	for _, sidecar := range repairSidecars(path) {
		if !exists(sidecar) {
			continue
		}
		sst, err := os.Stat(sidecar)
		if err != nil {
			return fail(err)
		}
		need += uint64(sst.Size())
	}
	usage, err := hostinfo.DiskUsage(dir)
	if err != nil {
		return fail(err)
	}
	return "", need, usage.Free, repairBackupHeadroom(usage.Total)
}

func repairScratchSpaceError(path string) string {
	problem, snapshot, free, headroom := diskBudget(path, "repair snapshot")
	if problem != "" {
		return problem
	}
	if free >= snapshot+2*snapshot+headroom {
		return ""
	}
	return fmt.Sprintf("only %.2fGB free on %s; the repair snapshot needs up to %.2fGB, VACUUM may need another %.2fGB, "+
		"and %.2fGB must remain as headroom. Free disk space, then retry.", float64(free)/1e9, filepath.Dir(path),
		float64(snapshot)/1e9, float64(2*snapshot)/1e9, float64(headroom)/1e9)
}

func backupFreeSpaceError(path string) string {
	hint := fmt.Sprintf(manualRecoverHint, path)
	problem, need, free, headroom := diskBudget(path, "forensic copy")
	if problem != "" {
		return problem + ". " + hint
	}
	if free >= need+headroom {
		return ""
	}
	return fmt.Sprintf("only %.2fGB free on %s; copying the damaged DB needs %.2fGB and must leave %.2fGB headroom. %s",
		float64(free)/1e9, filepath.Dir(path), float64(need)/1e9, float64(headroom)/1e9, hint)
}

func repairSnapshotTimeout(source string) time.Duration {
	var total int64
	for _, candidate := range append([]string{source}, repairSidecars(source)...) {
		if st, err := os.Stat(candidate); err == nil {
			total += st.Size()
		}
	}
	return max(repairLockTimeout, time.Duration(float64(total)/repairSnapshotMinThroughput*float64(time.Second)))
}

func repairLedgerPath(path string) string { return path + ".repair-attempts.json" }

func (e *Engine) dbFingerprint(path string) (string, bool) {
	var out string
	err := e.OfflineAccess(path, "fingerprint", func() error {
		st, err := os.Stat(path)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		head := make([]byte, fingerprintSampleBytes)
		n, err := readFull(f, head)
		if err != nil {
			return err
		}
		head = head[:n]
		var tail []byte
		if st.Size() > fingerprintSampleBytes {
			tail = make([]byte, fingerprintSampleBytes)
			n, err := f.ReadAt(tail, st.Size()-fingerprintSampleBytes)
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			tail = tail[:n]
		}
		sum := sha256.Sum256(append(maskVolatileHeader(head), tail...))
		out = fmt.Sprintf("%d:%s", st.Size(), hex.EncodeToString(sum[:])[:32])
		return nil
	})
	return out, err == nil
}

func (e *Engine) backupContentIdentity(path string) (string, bool) {
	var out string
	err := e.OfflineAccess(path, "backup-identity", func() error {
		hasher := sha256.New()
		members := [][2]string{{"main", path}}
		for i, sidecar := range repairSidecars(path) {
			if exists(sidecar) {
				members = append(members, [2]string{repairSidecarSuffixes[i], sidecar})
			}
		}
		for _, m := range members {
			st, err := os.Stat(m[1])
			if err != nil {
				return err
			}
			fmt.Fprintf(hasher, "\x00%s:%d\x00", m[0], st.Size())
			f, err := os.Open(m[1])
			if err != nil {
				return err
			}
			_, err = io.Copy(hasher, f)
			cerr := f.Close()
			if err != nil {
				return err
			}
			if cerr != nil {
				return cerr
			}
		}
		out = hex.EncodeToString(hasher.Sum(nil))
		return nil
	})
	return out, err == nil
}

type repairLedger struct {
	fingerprint  string
	recordedText bool
	attempts     int64
}

func readRepairLedger(path string) repairLedger {
	data, err := os.ReadFile(repairLedgerPath(path))
	if err != nil {
		return repairLedger{}
	}
	members, ok := objectMembers(json.RawMessage(data))
	if !ok {
		return repairLedger{}
	}
	var l repairLedger
	if fp, ok := memberValue(members, "fingerprint"); ok && jsonIsString(fp) {
		l.fingerprint, l.recordedText = jsonString(fp), true
	}
	if raw, ok := memberValue(members, "failed_attempts"); ok {
		if node, err := parseImportJSON(raw); err == nil {
			if v, outcome := importIntOf(node); outcome == importIntValid {
				l.attempts = v
			}
		}
	}
	return l
}

func (e *Engine) repairAttemptsExhausted(path string) bool {
	ledger := readRepairLedger(path)
	fp, ok := e.dbFingerprint(path)
	var same bool
	if ok {
		same = ledger.recordedText && ledger.fingerprint == fp
	} else {
		st, err := os.Stat(path)
		if err != nil {
			return false
		}
		same = ledger.recordedText && strings.HasPrefix(ledger.fingerprint, strconv.FormatInt(st.Size(), 10)+":")
	}
	return same && ledger.attempts >= maxPersistentRepairAttempts
}

func (e *Engine) repairExhaustedError(ctx context.Context, path string) string {
	profile := e.profileArg(ctx)
	return fmt.Sprintf("automatic repair has already failed %d times on this exact file — the corruption is beyond the "+
		"schema/FTS repair strategies (likely b-tree page damage). Manual recovery required: restore a backup, or salvage "+
		"with `icebath %ssessions recover --source %s --inspect-only`, then (if it reports recoverable) `icebath %ssessions "+
		"recover --source %s --output recovered-state.db` (recovery snapshots the damaged file first, then runs the "+
		"page-level `.recover` lane on the copy; do NOT point a raw `sqlite3` shell at the live database). Delete %s to "+
		"force another automatic attempt.", maxPersistentRepairAttempts, profile, path, profile, path, filepath.Base(repairLedgerPath(path)))
}

func (e *Engine) recordRepairOutcome(path string, repaired bool) {
	ledgerPath := repairLedgerPath(path)
	if repaired {
		if err := os.Remove(ledgerPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn(fmt.Sprintf("Could not update state.db repair ledger: %v", err))
		}
		return
	}
	ledger := readRepairLedger(path)
	fp, ok := e.dbFingerprint(path)
	if !ok {
		if !ledger.recordedText {
			return
		}
		fp = ledger.fingerprint
	}
	attempts := int64(1)
	if ledger.recordedText && ledger.fingerprint == fp {
		attempts = ledger.attempts + 1
	}
	payload, err := objectJSON(ConfigMember{"fingerprint", fp}, ConfigMember{"failed_attempts", attempts},
		ConfigMember{"last_attempt", time.Now().Format("2006-01-02T15:04:05")})
	if err == nil {
		err = os.WriteFile(ledgerPath, payload, 0o666)
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("Could not update state.db repair ledger: %v", err))
	}
}

func existingMalformedBackups(path string) []string {
	prefix := filepath.Base(path) + ".malformed-backup-"
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		sidecar := false
		for _, suffix := range repairSidecarSuffixes {
			sidecar = sidecar || strings.HasSuffix(name, suffix)
		}
		if !sidecar {
			names = append(names, name)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = filepath.Join(filepath.Dir(path), name)
	}
	return out
}

func pruneMalformedBackups(path string) {
	backups := existingMalformedBackups(path)
	if len(backups) <= maxMalformedBackups {
		return
	}
	for _, stale := range backups[maxMalformedBackups:] {
		for _, victim := range append([]string{stale}, repairSidecars(stale)...) {
			if err := os.Remove(victim); err != nil && !errors.Is(err, fs.ErrNotExist) {
				slog.Warn(fmt.Sprintf("Could not prune stale DB backup %s: %v", victim, err))
			}
		}
	}
}

func publishBackupBundle(path, staging, backup string) error {
	type move struct{ src, staged, dst string }
	main := move{path, staging, backup}
	var sidecars []move
	for _, suffix := range repairSidecarSuffixes {
		if exists(path + suffix) {
			sidecars = append(sidecars, move{path + suffix, staging + suffix, backup + suffix})
		}
	}
	copies := append([]move{main}, sidecars...)
	promotions := append(append([]move{}, sidecars...), main)
	var published []string
	fail := func(err error) error {
		for _, m := range copies {
			_ = os.Remove(m.staged)
		}
		for _, p := range published {
			_ = os.Remove(p)
		}
		return err
	}
	for _, m := range copies {
		if err := fileutil.CopyFile(m.src, m.staged); err != nil {
			return fail(err)
		}
	}
	for _, m := range promotions {
		if err := os.Rename(m.staged, m.dst); err != nil {
			return fail(err)
		}
		published = append(published, m.dst)
	}
	return nil
}

func (e *Engine) backupDBFile(path string) (string, string) {
	if e.HasLiveConnection(path) {
		reason := fmt.Sprintf("a connection to %s is still open in this process; raw-copying it would cancel that "+
			"connection's POSIX advisory locks. Close all session store handles first.", path)
		slog.Error(fmt.Sprintf("Refusing to raw-copy %s for backup: %s", path, reason))
		return "", reason
	}
	stamp := time.Now().Format("20060102_150405")
	backup := path + ".malformed-backup-" + stamp
	for seq := 1; exists(backup); seq++ {
		backup = fmt.Sprintf("%s.malformed-backup-%s_%d", path, stamp, seq)
	}
	for _, pattern := range []string{filepath.Base(path) + ".backup-staging-*", filepath.Base(path) + ".malformed-backup-*.incomplete*"} {
		matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), pattern))
		for _, old := range matches {
			_ = os.Remove(old)
		}
	}
	if newest := existingMalformedBackups(path); len(newest) > 0 {
		if src, ok := e.backupContentIdentity(path); ok {
			if candidate, ok := e.backupContentIdentity(newest[0]); ok && candidate == src {
				slog.Info(fmt.Sprintf("Reusing existing forensic backup %s (identical to the damaged DB).", newest[0]))
				return newest[0], ""
			}
		}
	}
	if reason := backupFreeSpaceError(path); reason != "" {
		slog.Error(fmt.Sprintf("Refusing forensic backup of %s: %s", path, reason))
		return "", reason
	}
	if err := publishBackupBundle(path, path+".backup-staging-"+stamp, backup); err != nil {
		slog.Warn(fmt.Sprintf("Could not back up malformed DB %s: %v", path, err))
		return "", fmt.Sprintf("backup copy failed: %v", err)
	}
	pruneMalformedBackups(path)
	return backup, ""
}

func unlinkDBTriple(path string) string {
	var failures []string
	for _, victim := range append([]string{path}, repairSidecars(path)...) {
		for attempt := 0; attempt < 10; attempt++ {
			err := os.Remove(victim)
			if err == nil || errors.Is(err, fs.ErrNotExist) {
				break
			}
			if runtime.GOOS == "windows" && errors.Is(err, fs.ErrPermission) && attempt < 9 {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			failures = append(failures, fmt.Sprintf("%s: %v", victim, err))
			break
		}
	}
	return strings.Join(failures, "; ")
}

func runBackup(ctx context.Context, src, dst *conn, deadline time.Time, timeout time.Duration) error {
	return dst.raw(func(tls *libc.TLS, dstDB uintptr) error {
		return src.raw(func(_ *libc.TLS, srcDB uintptr) error {
			name, err := libc.CString("main")
			if err != nil {
				return err
			}
			defer libc.Xfree(tls, name)
			p := sqlite3.Xsqlite3_backup_init(tls, dstDB, name, srcDB, name)
			if p == 0 {
				return &backupError{code: int(sqlite3.Xsqlite3_extended_errcode(tls, dstDB)),
					msg: libc.GoString(sqlite3.Xsqlite3_errmsg(tls, dstDB))}
			}
			for {
				rc := sqlite3.Xsqlite3_backup_step(tls, p, repairBackupStepPages)
				if !time.Now().Before(deadline) {
					sqlite3.Xsqlite3_backup_finish(tls, p)
					return fmt.Errorf("timed out copying SQLite repair snapshot after %.0fs", timeout.Seconds())
				}
				if err := ctx.Err(); err != nil {
					sqlite3.Xsqlite3_backup_finish(tls, p)
					return err
				}
				if rc == sqlite3.SQLITE_BUSY || rc == sqlite3.SQLITE_LOCKED {
					sqlite3.Xsqlite3_sleep(tls, repairBackupProgressSleepMilli)
				}
				if rc != sqlite3.SQLITE_OK && rc != sqlite3.SQLITE_BUSY && rc != sqlite3.SQLITE_LOCKED {
					break
				}
			}
			if rc := sqlite3.Xsqlite3_backup_finish(tls, p); rc != sqlite3.SQLITE_OK {
				return &backupError{code: int(sqlite3.Xsqlite3_extended_errcode(tls, dstDB)),
					msg: libc.GoString(sqlite3.Xsqlite3_errmsg(tls, dstDB))}
			}
			return nil
		})
	})
}

func (e *Engine) copyDatabaseSnapshot(ctx context.Context, sourcePath, destPath string, source, dest *conn) error {
	timeout := repairSnapshotTimeout(sourcePath)
	deadline := time.Now().Add(timeout)
	var owned []*conn
	defer func() {
		for i := len(owned) - 1; i >= 0; i-- {
			_ = owned[i].close()
		}
	}()
	if source == nil {
		c, err := e.connectRepairDurable(ctx, sourcePath, repairConnTimeout)
		if err != nil {
			return err
		}
		owned, source = append(owned, c), c
	}
	if dest != nil {
		open, err := dest.inTransaction()
		if err != nil {
			return err
		}
		if open {
			return errors.New("SQLite repair backup destination has an active transaction")
		}
	} else {
		c, err := e.connectRepairDurable(ctx, destPath, repairConnTimeout)
		if err != nil {
			return err
		}
		owned, dest = append(owned, c), c
	}
	return runBackup(ctx, source, dest, deadline, timeout)
}

func (e *Engine) DBOpensCleanly(ctx context.Context, path string) (string, error) {
	c, err := e.connectRepairDurable(ctx, path, repairConnTimeout)
	if err != nil {
		return "", err
	}
	reason, err := dbHealthReason(ctx, c.q())
	cerr := c.close()
	if err != nil {
		if sqliteFailure(err) {
			return errText(err), nil
		}
		return "", err
	}
	if cerr != nil && reason == "" {
		if sqliteFailure(cerr) {
			return errText(cerr), nil
		}
		return "", cerr
	}
	return reason, nil
}

func dbHealthReason(ctx context.Context, q Conn) (string, error) {
	_ = registerCJKTokenizer(q)
	if _, err := scanMode(ctx, q, "PRAGMA journal_mode"); err != nil {
		return "", err
	}
	lines, err := queryStrings(ctx, q, "PRAGMA integrity_check")
	if err != nil {
		return "", err
	}
	var problems []string
	for _, line := range lines {
		if strings.ToLower(line) != "ok" {
			problems = append(problems, line)
		}
	}
	if len(problems) > 0 {
		return strings.Join(problems[:min(3, len(problems))], "; "), nil
	}
	var count int64
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions").Scan(&count); err != nil {
		return "", err
	}
	for _, table := range repairFTSTables {
		var one int
		err := q.QueryRowContext(ctx, "SELECT 1 FROM "+table+" WHERE "+table+" MATCH '\"\"' LIMIT 1").Scan(&one)
		if err == nil || errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if !sqliteFailure(err) {
			return "", err
		}
		benign := isFTS5UnavailableError(err) || schemaNotBuilt(err)
		if !(sqliteOperational(err) && benign) {
			return fmt.Sprintf("fts5 read probe failed on %s: %s", table, errText(err)), nil
		}
	}
	return ftsWriteProbe(ctx, q)
}

func ftsWriteProbe(ctx context.Context, q Conn) (string, error) {
	probeID := fmt.Sprintf("_icebath_fts_health_probe_%d", time.Now().UnixNano())
	defer func() { _, _ = q.ExecContext(ctx, "ROLLBACK") }()
	err := func() error {
		if _, err := q.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, "INSERT INTO sessions (id, source, started_at) VALUES (?, ?, ?)", probeID, "_health_probe", nowEpoch()); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, "INSERT INTO messages (session_id, role, content, timestamp) VALUES (?, ?, ?, ?)",
			probeID, "user", "_fts_health_probe", nowEpoch()); err != nil {
			return err
		}
		for _, table := range repairFTSTables {
			if _, err := q.ExecContext(ctx, "INSERT INTO "+table+"("+table+") VALUES('flush')"); err != nil {
				if !sqliteOperational(err) || !(isFTS5UnavailableError(err) || schemaNotBuilt(err)) {
					return err
				}
			}
		}
		return nil
	}()
	if err == nil {
		return "", nil
	}
	if !sqliteFailure(err) {
		return "", err
	}
	if schemaNotBuilt(err) || strings.Contains(lowerText(err), "no such tokenizer: cjk_unicode61") {
		return "", nil
	}
	return "fts5 write probe failed: " + errText(err), nil
}

type masterRoot struct {
	rootpage int64
	name     string
}

func integrityDamageIsStructural(lines []string, master []masterRoot) bool {
	nameByRoot := map[int64]string{}
	for _, m := range master {
		if m.rootpage != 0 {
			nameByRoot[m.rootpage] = m.name
		}
	}
	for _, text := range lines {
		if strings.HasPrefix(text, "Freelist") {
			return true
		}
		if tree := integrityTree.FindStringSubmatch(text); tree != nil {
			root, _ := strconv.ParseInt(tree[1], 10, 64)
			if name := nameByRoot[root]; name != "" && !ftsObjectName.MatchString(name) {
				return true
			}
		}
		if missing := integrityMissingIdx.FindStringSubmatch(text); missing != nil && !ftsObjectName.MatchString(missing[1]) {
			return true
		}
	}
	return false
}

func (e *Engine) HasStructuralDamage(ctx context.Context, path string) bool {
	c, err := e.connect(ctx, ReadOnlyURI(path), path)
	if err != nil {
		return false
	}
	defer func() { _ = c.close() }()
	_ = c.raw(func(tls *libc.TLS, db uintptr) error {
		sqlite3.Xsqlite3_busy_timeout(tls, db, 1000)
		return nil
	})
	q := c.q()
	verdict := func(err error) bool { return sqliteFailure(err) && !sqliteOperational(err) }
	rows, err := q.QueryContext(ctx, "SELECT rootpage, type, name FROM sqlite_master WHERE rootpage > 0")
	if err != nil {
		return verdict(err)
	}
	var master []masterRoot
	for rows.Next() {
		var root, kind, name any
		if err := rows.Scan(&root, &kind, &name); err != nil {
			_ = rows.Close()
			return verdict(err)
		}
		r, _ := cellInt(root)
		n, _ := cellText(name)
		master = append(master, masterRoot{r, n})
	}
	if err := rowsDone(rows); err != nil {
		return verdict(err)
	}
	results, err := queryStrings(ctx, q, "PRAGMA integrity_check")
	if err != nil {
		return verdict(err)
	}
	var lines []string
	for _, r := range results {
		lines = append(lines, strings.Split(strings.ReplaceAll(strings.ReplaceAll(r, "\r\n", "\n"), "\r", "\n"), "\n")...)
	}
	return integrityDamageIsStructural(lines, master)
}

func (e *Engine) LiveWriterHoldsDB(ctx context.Context, path string) (bool, error) {
	if len(ForeignHolders(path)) > 0 {
		return true, nil
	}
	verdict := func(err error) (bool, error) {
		switch {
		case sqliteOperational(err):
			return isLockError(err), nil
		case sqliteFailure(err):
			return false, nil
		}
		return false, err
	}
	c, err := e.connectRepairDurable(ctx, path, 0)
	if err != nil {
		return verdict(err)
	}
	defer func() { _ = closeUnpinned(ctx, c) }()
	for _, statement := range []string{"PRAGMA locking_mode=EXCLUSIVE", "BEGIN IMMEDIATE", "ROLLBACK"} {
		if _, err := c.q().ExecContext(ctx, statement); err != nil {
			return verdict(presentable(err))
		}
	}
	return false, nil
}

func (e *Engine) crossProcessRepairLock(ctx context.Context, path string) (func(), bool) {
	lockPath := path + ".repair.lock"
	f, err := func() (*os.File, error) {
		if err := os.MkdirAll(filepath.Dir(lockPath), 0o777); err != nil {
			return nil, err
		}
		return os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o666)
	}()
	if err != nil {
		slog.Warn(fmt.Sprintf("Could not open state.db repair lock %s (%v) — skipping schema surgery rather than running "+
			"it without cross-process authority.", lockPath, err))
		return func() {}, false
	}
	outcome, f := e.acquireRepairLock(ctx, lockPath, f, repairLockTimeout)
	switch outcome {
	case lockHeld:
		return func() {
			releaseRebuildLock(f)
			_ = f.Close()
		}, true
	case lockBusy:
		slog.Warn(fmt.Sprintf("state.db repair lock %s held by another process for more than %.0fs — skipping schema "+
			"surgery in this process to avoid racing the repairer. Recorded holder: %s.", lockPath,
			repairLockTimeout.Seconds(), describeLockHolder(readHolderRecord(f))))
	case lockAbandoned:
		slog.Warn(fmt.Sprintf("Stopped waiting for state.db repair lock %s before its %.0fs bound (%v) — skipping schema surgery.",
			lockPath, repairLockTimeout.Seconds(), context.Cause(ctx)))
	}
	_ = f.Close()
	return func() {}, false
}

func repairSkip(report *RepairReport, verb, problem string, cause error) {
	if cause != nil && repairConsumesAttempt(cause) {
		report.attempted = true
	}
	report.Error = problem
	slog.Error(fmt.Sprintf("state.db repair %s: %s", verb, problem))
}

func (e *Engine) repairStateDBSchema(ctx context.Context, path string, d Database, progress func(time.Duration, string)) (bool, error) {
	report, err := e.RepairStateDBSchema(ctx, path, RepairOptions{Database: d, StartupProgress: progress})
	return report.Repaired, err
}

func (e *Engine) RepairStateDBSchema(ctx context.Context, path string, o RepairOptions) (RepairReport, error) {
	var report RepairReport
	if o.StartupProgress != nil {
		o.StartupProgress(repairStartupLease, "state_db_repair")
	}
	if !exists(path) {
		report.Error = path + " does not exist"
		return report, nil
	}
	if e.repairAttemptsExhausted(path) {
		repairSkip(&report, "skipped", e.repairExhaustedError(ctx, path), nil)
		return report, nil
	}
	release, held := e.crossProcessRepairLock(ctx, path)
	defer release()
	if !held {
		reason, err := e.DBOpensCleanly(ctx, path)
		if err != nil {
			return report, err
		}
		if reason == "" {
			report.Repaired, report.Strategy = true, "repaired_by_other_process"
		} else {
			report.Error = "could not obtain the state.db repair lock (held by another process, or the lock file was " +
				"unopenable); skipped schema surgery to avoid racing a concurrent repairer"
		}
		return report, nil
	}
	if e.repairAttemptsExhausted(path) {
		repairSkip(&report, "skipped", e.repairExhaustedError(ctx, path), nil)
	} else {
		writer, err := e.LiveWriterHoldsDB(ctx, path)
		if err != nil {
			return report, err
		}
		if writer {
			repairSkip(&report, "skipped", "a live writer still holds state.db; skipped schema surgery to avoid tearing "+
				"b-tree pages under a concurrent writer. Stop the gateway (icebath gateway stop) and retry.", nil)
		} else {
			before := e.probeJournalModeForRepair(ctx, path)
			if err := e.repairLocked(ctx, path, o, &report, before); err != nil {
				return report, err
			}
			if report.Repaired {
				report.JournalModeBefore = before
			}
		}
	}
	if report.attempted || report.Repaired {
		e.recordRepairOutcome(path, report.Repaired)
	}
	report.attempted = false
	return report, nil
}

func (e *Engine) probeJournalModeForRepair(ctx context.Context, path string) string {
	c, err := e.connectRepairDurable(ctx, path, repairConnTimeout)
	if err != nil {
		return ""
	}
	defer func() { _ = c.close() }()
	mode, ok := onDiskMode(ctx, c.q())
	if !ok {
		return ""
	}
	return mode
}

func (e *Engine) restoreJournalModeAfterRepair(ctx context.Context, path, before string, c Conn, d Database) {
	after, err := e.applyJournalMode(ctx, c, filepath.Base(path), d)
	if err != nil {
		slog.Warn(fmt.Sprintf("state.db repair at %s: post-surgery journal-mode restore failed (%s); verify with PRAGMA "+
			"journal_mode on the next open", path, errText(err)))
		return
	}
	if before != "" && after != before {
		slog.Warn(fmt.Sprintf("state.db repair changed journal_mode '%s' -> '%s' (pre-surgery probe '%s'; restore resolved "+
			"through the journal mode policy per database.journal_mode and the WAL-reset gate)", before, after, before))
	}
}

func (e *Engine) repairLocked(ctx context.Context, path string, o RepairOptions, report *RepairReport, before string) error {
	scratch := path + ".repair-scratch"
	if cleanup := unlinkDBTriple(scratch); cleanup != "" {
		repairSkip(report, "aborted", "could not remove a stale repair snapshot before probing state.db: "+cleanup, nil)
		return nil
	}
	reason, err := e.DBOpensCleanly(ctx, path)
	if err != nil {
		return err
	}
	if reason == "" {
		report.Repaired, report.Strategy = true, "already_healthy"
		return nil
	}
	if !o.NoBackup {
		backup, refusal := e.backupDBFile(path)
		report.BackupPath = backup
		if backup == "" {
			repairSkip(report, "aborted", "pre-repair backup refused; aborting schema repair to avoid mutating the only "+
				"copy of the damaged DB: "+refusal, nil)
			return nil
		}
	}
	guard, guardErr := e.openExclusive(ctx, path, "BEGIN EXCLUSIVE")
	if guardErr != nil {
		repairSkip(report, "skipped", "could not acquire exclusive state.db repair ownership; skipped schema surgery to "+
			"avoid overwriting a concurrent writer. Stop the gateway and retry: "+errText(guardErr), guardErr)
		return nil
	}
	defer func() { _ = closeUnpinned(ctx, guard) }()
	if space := repairScratchSpaceError(path); space != "" {
		repairSkip(report, "aborted", space, nil)
		return nil
	}
	if err := e.copyDatabaseSnapshot(ctx, path, scratch, guard, nil); err != nil {
		unlinkDBTriple(scratch)
		repairSkip(report, "aborted", fmt.Sprintf("could not stage a complete SQLite repair snapshot of %s: %s", path, errText(err)), err)
		return nil
	}
	defer func() {
		if cleanup := unlinkDBTriple(scratch); cleanup != "" {
			slog.Warn("Could not remove state.db repair snapshot after repair: " + cleanup)
		}
	}()
	report.attempted = true
	if err := e.runRepairStrategies(ctx, scratch, report); err != nil {
		return err
	}
	if report.Repaired {
		if err := e.copyDatabaseSnapshot(ctx, scratch, path, nil, guard); err != nil {
			report.Repaired, report.Strategy, report.attempted = false, "", repairConsumesAttempt(err)
			report.Error = "repaired snapshot could not be promoted transactionally: " + errText(err)
			slog.Error("state.db repair promotion failed: " + errText(err))
		} else {
			slog.Warn(fmt.Sprintf("state.db repaired via '%s' and promoted transactionally: %s", report.Strategy, path))
			e.restoreJournalModeAfterRepair(ctx, path, before, guard.q(), o.Database)
		}
	}
	if !report.Repaired {
		backup := report.BackupPath
		if backup == "" {
			backup = "none"
		}
		slog.Error(fmt.Sprintf("state.db schema repair could not recover %s automatically (no committed canonical data "+
			"was modified or lost; backup: %s); manual restore from backup may be required.", path, backup))
	}
	return nil
}

type repairStrategy struct {
	name    string
	body    func(ctx context.Context, c Conn) error
	success string
	failure string
}

var repairStrategies = []repairStrategy{
	{"rebuild_fts", strategyRebuildFTS, "state.db FTS indexes rebuilt in place (schema preserved): %s",
		"state.db FTS in-place rebuild pass failed: %s"},
	{"reindex_btree", strategyReindex, "state.db B-tree indexes rebuilt via REINDEX: %s", "state.db REINDEX pass failed: %s"},
	{"dedup_schema", strategyDedupSchema, "state.db schema repaired by de-duplicating sqlite_master (FTS index preserved): %s",
		"state.db dedup repair pass failed: %s"},
	{"drop_fts_rebuild", strategyDropFTSVacuum,
		"state.db schema repaired by dropping FTS schema; indexes will rebuild from messages on next open: %s", ""},
}

func editSQLiteMaster(ctx context.Context, c Conn, edit func() (bool, error)) error {
	if _, err := c.ExecContext(ctx, "PRAGMA writable_schema=ON"); err != nil {
		return err
	}
	changed, err := edit()
	if err != nil {
		return err
	}
	if changed {
		bumpSchemaCookie(ctx, c)
	}
	_, err = c.ExecContext(ctx, "PRAGMA writable_schema=OFF")
	return err
}

func strategyRebuildFTS(ctx context.Context, c Conn) error {
	_ = registerCJKTokenizer(c)
	for _, table := range repairFTSTables {
		if _, err := c.ExecContext(ctx, "INSERT INTO "+table+"("+table+") VALUES('rebuild')"); err != nil && !sqliteOperational(err) {
			return err
		}
	}
	return nil
}

func strategyReindex(ctx context.Context, c Conn) error {
	reapplyDurabilityBarriers(ctx, c)
	_, err := c.ExecContext(ctx, "REINDEX")
	return err
}

func strategyDedupSchema(ctx context.Context, c Conn) error {
	return editSQLiteMaster(ctx, c, func() (bool, error) {
		rows, err := c.QueryContext(ctx, "SELECT type, name, COUNT(*) AS c, MIN(rowid) AS keep FROM sqlite_master GROUP BY type, name HAVING c > 1")
		if err != nil {
			return false, err
		}
		type dupe struct {
			kind, name, count, keep any
		}
		var dupes []dupe
		for rows.Next() {
			var d dupe
			if err := rows.Scan(&d.kind, &d.name, &d.count, &d.keep); err != nil {
				_ = rows.Close()
				return false, err
			}
			dupes = append(dupes, d)
		}
		if err := rowsDone(rows); err != nil {
			return false, err
		}
		for _, d := range dupes {
			if _, err := c.ExecContext(ctx, "DELETE FROM sqlite_master WHERE type IS ? AND name IS ? AND rowid <> ?", d.kind, d.name, d.keep); err != nil {
				return false, err
			}
		}
		return len(dupes) > 0, nil
	})
}

func strategyDropFTSVacuum(ctx context.Context, c Conn) error {
	if err := editSQLiteMaster(ctx, c, func() (bool, error) {
		_, err := c.ExecContext(ctx, "DELETE FROM sqlite_master WHERE name LIKE 'messages_fts%'")
		return true, err
	}); err != nil {
		return err
	}
	reapplyDurabilityBarriers(ctx, c)
	_, err := c.ExecContext(ctx, "VACUUM")
	return err
}

func (e *Engine) runRepairStrategies(ctx context.Context, scratch string, report *RepairReport) error {
	for _, st := range repairStrategies {
		reason, err := func() (string, error) {
			c, err := e.connectRepairDurable(ctx, scratch, repairConnTimeout)
			if err != nil {
				return "", err
			}
			berr := st.body(ctx, c.q())
			cerr := c.close()
			if berr != nil {
				return "", berr
			}
			if cerr != nil {
				return "", cerr
			}
			return e.DBOpensCleanly(ctx, scratch)
		}()
		if err != nil {
			if !sqliteFailure(err) {
				return err
			}
			reason = errText(err)
			if st.failure != "" {
				slog.Warn(fmt.Sprintf(st.failure, errText(err)))
			}
		}
		if reason == "" {
			report.Repaired, report.Strategy = true, st.name
			slog.Warn(fmt.Sprintf(st.success, scratch))
			return nil
		}
		if st.failure == "" {
			report.Error = reason
		}
	}
	return nil
}
