package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	charsetutil "github.com/nickheyer/icebath/pkg/utils/charset"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	fsutil "github.com/nickheyer/icebath/pkg/utils/fs"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	sqliteutil "github.com/nickheyer/icebath/pkg/utils/sqlite"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
	ziputil "github.com/nickheyer/icebath/pkg/utils/zip"
)

// Failure the restore code reports with its message
type restoreError struct{ msg string }

func (e *restoreError) Error() string { return e.msg }

// Reports whether err is a failure a restore records and survives: one the restore code reported or one from the operating system, a refused maintenance reservation excluded
func isRestoreFailure(err error) bool {
	if err == nil {
		return false
	}
	var own *restoreError
	var conflict *gateway.OwnershipConflict
	if errors.As(err, &own) {
		return true
	}
	if errors.As(err, &conflict) {
		return false
	}
	return errorutil.IsSystem(err)
}

// Reports whether err is a SQLite failure of any kind
func isSQLiteError(err error) bool {
	var se *sqliteutil.Error
	if errors.As(err, &se) {
		return true
	}
	_, ok := state.SQLiteClassOf(err)
	return ok
}

// Reports whether err is a stored value that does not convert to an integer
func isConversionError(err error) bool {
	return errors.Is(err, anyutil.ErrNotNumber) || errors.Is(err, anyutil.ErrRange)
}

// Reports whether err is the OwnershipConflict of a refused maintenance reservation
func isOwnershipConflict(err error) bool {
	var conflict *gateway.OwnershipConflict
	return errors.As(err, &conflict)
}

// Closes a connection and its pool, returning the first failure as sqlite3 close does
func (s *sqliteConn) close() error {
	err := s.c.Close()
	if derr := s.db.Close(); err == nil {
		err = derr
	}
	return err
}

// Opens one connection to a database file URI with the sqlite3.connect five second busy timeout
func connectURI(ctx context.Context, uri string) (*sqliteConn, error) {
	sep := "?"
	if strings.Contains(uri, "?") {
		sep = "&"
	}
	return openSQLite(ctx, uri+sep+"_pragma=busy_timeout(5000)")
}

// Opens a database read only through the literal path URI the state holders module builds
func connectReadOnly(ctx context.Context, path string) (*sqliteConn, error) {
	uri, err := state.ReadOnlyDBURI(path)
	if err != nil {
		return nil, err
	}
	return connectURI(ctx, uri)
}

// pathlib absolute of a path, joined onto the working directory without normalizing it
func pathAbsolute(p string) (string, error) {
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return pathutil.Join(cwd, p), nil
}

// Integer of a SQLite value, a blob read as decimal text; failures wrap anyutil.ErrNotNumber or anyutil.ErrRange
func sqliteInt(v any) (int64, error) {
	b, isBlob := v.([]byte)
	if !isBlob {
		return anyutil.ToInt(v)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if errors.Is(err, strconv.ErrRange) {
		return n, fmt.Errorf("integer %q: %w", b, anyutil.ErrRange)
	}
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q: %w", b, anyutil.ErrNotNumber)
	}
	return n, nil
}

// PIDs of other processes holding the database or its WAL or SHM open, false when the scan cannot run
func foreignDBHolderPIDs(dbPath string) ([]int, bool) {
	if runtime.GOOS != "linux" {
		return nil, false
	}
	canonical := func(p string) (string, error) {
		abs, err := filepath.Abs(strings.TrimSuffix(p, " (deleted)"))
		if err != nil {
			return "", err
		}
		return pathutil.FoldCase(abs), nil
	}
	db, err := canonical(dbPath)
	if err != nil {
		return nil, false
	}
	watched := map[string]bool{db: true, db + "-wal": true, db + "-shm": true}
	proc, err := os.Open("/proc")
	if err != nil {
		return nil, false
	}
	names, err := proc.Readdirnames(-1)
	proc.Close()
	if err != nil {
		return nil, false
	}
	own := os.Getpid()
	pids := []int{}
	for _, name := range names {
		if !textutil.IsDigits(name) {
			continue
		}
		pid, err := strconv.Atoi(name)
		if err != nil || pid == own {
			continue
		}
		fdDir := fmt.Sprintf("/proc/%d/fd", pid)
		d, err := os.Open(fdDir)
		if err != nil {
			continue
		}
		fds, err := d.Readdirnames(-1)
		d.Close()
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(fdDir + "/" + fd)
			if err != nil {
				continue
			}
			c, err := canonical(target)
			if err != nil {
				return nil, false
			}
			if watched[c] {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids, true
}

// Writable auth store path whose lock and publish name one file, empty when restoring there is refused
func authRestoreTarget(dst string) string {
	if _, err := os.Stat(dst); err == nil {
		links, err := linkCount(dst)
		if err != nil {
			pkgLog().Error(fmt.Sprintf("Refusing auth.json restore to %s: cannot resolve store identity: %s", filepath.Clean(dst), excText(err)))
			return ""
		}
		if links > 1 {
			pkgLog().Error(fmt.Sprintf("Refusing auth.json restore to %s: it has %d hard links; atomic replacement would split a shared auth store", filepath.Clean(dst), links))
			return ""
		}
	}
	if st, err := os.Lstat(dst); err == nil && st.Mode()&os.ModeSymlink != 0 {
		resolved, err := pathutil.Resolve(dst)
		if err != nil {
			pkgLog().Error(fmt.Sprintf("Refusing auth.json restore to %s: cannot resolve store identity: %s", filepath.Clean(dst), excText(err)))
			return ""
		}
		return resolved
	}
	return dst
}

// Values of a decoded JSON document in the encoding/json shapes the auth store code reads
func authStoreValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			t[k] = authStoreValue(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = authStoreValue(val)
		}
		return t
	case int64:
		return float64(t)
	}
	return v
}

// Restores auth.json under the auth store lock without rolling back a live single use OAuth generation
func restoreAuthJSON(src, dst string) bool {
	raw, err := os.ReadFile(src)
	var doc any
	if err == nil {
		var text string
		if text, err = charsetutil.Decode(raw, "utf-8-sig"); err == nil {
			doc, err = jsonutil.Decode(text)
		}
	}
	if err != nil {
		pkgLog().Error(fmt.Sprintf("Refusing auth.json restore from %s: %s", filepath.Clean(src), excText(err)))
		return false
	}
	obj, ok := doc.(*jsonutil.Object)
	if !ok {
		pkgLog().Error(fmt.Sprintf("Refusing auth.json restore from %s: top level is not an object", filepath.Clean(src)))
		return false
	}
	isDict := func(key string) bool {
		v, _ := obj.Get(key)
		_, ok := v.(*jsonutil.Object)
		return ok
	}
	if !isDict("providers") && !isDict("credential_pool") {
		pkgLog().Error(fmt.Sprintf("Refusing auth.json restore from %s: unrecognized auth-store shape", filepath.Clean(src)))
		return false
	}
	snapshot, _ := authStoreValue(jsonutil.Plain(obj)).(map[string]any)
	if err := saveRestoredAuth(snapshot, dst); err != nil {
		if errors.Is(err, errAuthTargetRefused) {
			return false
		}
		pkgLog().Error(fmt.Sprintf("Failed to restore %s safely: %s", filepath.Clean(dst), excText(err)))
		return false
	}
	return true
}

// Refusal of the auth store target already logged by authRestoreTarget
var errAuthTargetRefused = errors.New("auth.json restore target refused")

// Merges the snapshot into the live store and saves it while holding the canonical store lock
func saveRestoredAuth(snapshot map[string]any, dst string) error {
	target := authRestoreTarget(dst)
	if target == "" {
		return errAuthTargetRefused
	}
	store := provider.NewStore(filepath.Dir(target))
	unlock, err := store.LockPath(target, provider.AuthLockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	live, err := store.LoadFile(target)
	if err != nil {
		return err
	}
	restored := provider.MergeSnapshotAuthPreservingLiveSingleUseGrants(snapshot, live)
	return store.SaveTo(provider.AuthData(restored), target)
}

// Restores a database page by page under the destination's maintenance reservation, false when refused
func safeRestoreDB(ctx context.Context, src, dst string) (bool, error) {
	check := VerifySQLiteIntegrity(src, IntegrityOptions{})
	if !check.Valid {
		pkgLog().Error(fmt.Sprintf("Refusing SQLite restore from %s: %s", filepath.Clean(src), check.Message))
		return false, nil
	}
	ok, err := func() (bool, error) {
		abs, err := pathAbsolute(dst)
		if err != nil {
			return false, err
		}
		resolved, err := pathutil.Resolve(dst)
		if err != nil {
			return false, err
		}
		inner, release, err := gateway.ExclusiveMaintenance(ctx, []string{filepath.Dir(filepath.Clean(abs)), filepath.Dir(filepath.Clean(resolved))}, "")
		if err != nil {
			return false, err
		}
		defer release()
		return restoreEpochSource(inner, src, dst, func(prepared string) (bool, error) {
			return restoreDBPages(inner, prepared, dst)
		})
	}()
	if err != nil {
		if isOwnershipConflict(err) || isRestoreFailure(err) || isSQLiteError(err) {
			pkgLog().Error(excText(err))
			return false, nil
		}
		return false, err
	}
	return ok, nil
}

// SafeRestoreDB exposes the page restore under the destination reservation to the update auto restore
func SafeRestoreDB(ctx context.Context, src, dst string) (bool, error) {
	return safeRestoreDB(ctx, src, dst)
}

// Runtime epoch floor stored in a database, zero when it has none
func restoreEpoch(ctx context.Context, path string) (int64, error) {
	if _, err := os.Stat(path); err != nil {
		return 0, nil
	}
	c, err := connectReadOnly(ctx, path)
	if err != nil {
		return 0, err
	}
	defer closeQuietly(c)
	var one any
	err = c.c.QueryRowContext(ctx, "SELECT 1 FROM sqlite_master WHERE name='runtime_epoch'").Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var epoch any
	err = c.c.QueryRowContext(ctx, "SELECT epoch FROM runtime_epoch WHERE singleton=1").Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return sqliteInt(epoch)
}

// Epoch floor of a restore destination, a restore failure naming the salvage route when it cannot be read
func restoreDestinationEpoch(ctx context.Context, dst string) (int64, error) {
	epoch, err := restoreEpoch(ctx, dst)
	if err != nil && (isRestoreFailure(err) || isSQLiteError(err) || isConversionError(err)) {
		return 0, &restoreError{fmt.Sprintf("Cannot read the canonical runtime epoch at %s; refusing in-place restore. "+
			"Use `icebath sessions recover --source <snapshot> --output <separate-path>` "+
			"to salvage transcripts into a separate output.", filepath.Clean(dst))}
	}
	return epoch, err
}

// Hands publish the snapshot, or a staged copy carrying the destination's epoch floor when it is higher
func restoreEpochSource(ctx context.Context, src, dst string, publish func(prepared string) (bool, error)) (bool, error) {
	if pathutil.Name(dst) != "state.db" {
		resolved, err := pathutil.Resolve(dst)
		if err != nil {
			return false, err
		}
		if pathutil.Name(resolved) != "state.db" {
			return publish(src)
		}
	}
	floor, err := restoreDestinationEpoch(ctx, dst)
	if err != nil {
		return false, err
	}
	srcEpoch, err := restoreEpoch(ctx, src)
	if err != nil {
		return false, err
	}
	if floor <= srcEpoch {
		return publish(src)
	}
	work, err := os.MkdirTemp(filepath.Dir(filepath.Clean(dst)), ".restore-epoch-*")
	if err != nil {
		return false, err
	}
	ok, err := func() (bool, error) {
		prepared := filepath.Join(work, "state.db")
		copied, err := safeCopyDB(ctx, src, prepared, 10.0)
		if err != nil {
			return false, err
		}
		if !copied {
			return false, &restoreError{"Unable to stage a restore with a safe runtime epoch"}
		}
		if err := stampRestoreEpoch(ctx, prepared, floor); err != nil {
			return false, err
		}
		st, err := os.Stat(src)
		if err != nil {
			return false, err
		}
		if err := chmodFull(prepared, statMode(st)); err != nil {
			return false, err
		}
		return publish(prepared)
	}()
	if rmErr := fsutil.RemoveTempDir(work); rmErr != nil {
		return false, rmErr
	}
	return ok, err
}

// Writes the epoch floor into a staged image as an offline restore owner
func stampRestoreEpoch(ctx context.Context, path string, floor int64) error {
	c, err := connectPath(ctx, path)
	if err != nil {
		return err
	}
	defer closeQuietly(c)
	if _, err := c.c.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS runtime_epoch (
                singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
                epoch INTEGER NOT NULL CHECK (epoch > 0), instance_id TEXT NOT NULL)`); err != nil {
		return err
	}
	_, err = c.c.ExecContext(ctx, `INSERT INTO runtime_epoch VALUES(1,?, 'offline-restore')
                ON CONFLICT(singleton) DO UPDATE SET epoch=excluded.epoch,
                instance_id=excluded.instance_id`, floor)
	return err
}

// Writes snapshot pages into the live database keeping its inode, replacing the file only when nothing holds it
func restoreDBPages(ctx context.Context, src, dst string) (bool, error) {
	var dstConn *sqliteConn
	err := func() error {
		var err error
		if dstConn, err = connectPath(ctx, dst); err != nil {
			return err
		}
		if _, err := dstConn.c.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			if !isSQLiteError(err) {
				return err
			}
			pkgLog().Debug(fmt.Sprintf("Pre-restore WAL checkpoint of %s skipped: %s", filepath.Clean(dst), excText(err)))
		}
		srcConn, err := connectReadOnly(ctx, src)
		if err != nil {
			return err
		}
		err = sqliteutil.Backup(srcConn, dstConn, -1, nil, 250*time.Millisecond)
		closeQuietly(srcConn)
		if err != nil {
			return err
		}
		conn := dstConn
		dstConn = nil
		if err := conn.close(); err != nil {
			dstConn = conn
			return err
		}
		st, err := os.Stat(src)
		if err == nil {
			err = chmodFull(dst, statMode(st))
		}
		if err != nil {
			pkgLog().Warn(fmt.Sprintf("Restored %s but could not apply the snapshot's file mode: %s", filepath.Clean(dst), excText(err)))
		}
		return nil
	}()
	if err == nil {
		return true, nil
	}
	if !isSQLiteError(err) && !isRestoreFailure(err) && !isConversionError(err) {
		closeQuietly(dstConn)
		return false, err
	}
	pkgLog().Warn(fmt.Sprintf("SQLite safe restore failed for %s -> %s: %s", filepath.Clean(src), filepath.Clean(dst), excText(err)))
	if dstConn != nil {
		if err := dstConn.close(); err != nil {
			pkgLog().Debug(fmt.Sprintf("Closing %s before the fallback restore failed: %s", filepath.Clean(dst), excText(err)))
		}
	}
	return replaceDBFile(src, dst)
}

// Unlink and move fallback of a page restore, refused while any process or in process connection holds dst
func replaceDBFile(src, dst string) (bool, error) {
	holders, _ := foreignDBHolderPIDs(dst)
	if len(holders) > 0 {
		pkgLog().Error(fmt.Sprintf("Refusing unlink+move restore of %s: process(es) %s still hold the database or its WAL open. Stop them and retry.",
			filepath.Clean(dst), anyutil.Quote(holders)))
		return false, nil
	}
	var swapErr error
	err := state.OfflineFileAccess(dst, "unlink+move restore of", func() {
		tmp := filepath.Join(filepath.Dir(filepath.Clean(dst)), "."+pathutil.Name(dst)+".snap_restore")
		if swapErr = fsutil.CopyFile(src, tmp); swapErr != nil {
			return
		}
		if swapErr = unlinkMissingOK(dst); swapErr != nil {
			return
		}
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if swapErr = unlinkMissingOK(dst + suffix); swapErr != nil {
				return
			}
		}
		_, swapErr = fsutil.Move(tmp, dst)
	})
	var live *state.LiveConnectionError
	if errors.As(err, &live) {
		pkgLog().Error(fmt.Sprintf("Refusing unlink+move restore of %s: %s Close the in-process database handles (or restart Icebath) and retry.",
			filepath.Clean(dst), live.Error()))
		return false, nil
	}
	if err == nil {
		err = swapErr
	}
	if err != nil {
		if isRestoreFailure(err) || isConversionError(err) {
			pkgLog().Error(fmt.Sprintf("Fallback restore also failed for %s -> %s: %s", filepath.Clean(src), filepath.Clean(dst), excText(err)))
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Checks that an archive looks like an icebath backup, the reason when it does not
func validateBackupZip(zr *ziputil.Reader) (bool, string) {
	names := zr.Names()
	if len(names) == 0 {
		return false, "zip archive is empty"
	}
	markers := map[string]bool{"config.yaml": true, ".env": true, "state.db": true}
	for _, n := range names {
		if markers[pathutil.Name(n)] {
			return true, ""
		}
	}
	return false, "zip does not appear to be an Icebath backup (no config.yaml, .env, or state databases found)"
}

// Directory prefix wrapping every archive member when it names an icebath home, empty otherwise
func detectPrefix(zr *ziputil.Reader) string {
	var names []string
	for _, n := range zr.Names() {
		if !strings.HasSuffix(n, "/") {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return ""
	}
	first := map[string]bool{}
	for _, n := range names {
		anchor, parts := pathutil.Parts(n)
		if anchor != "" {
			parts = append([]string{anchor}, parts...)
		}
		if len(parts) > 1 {
			first[parts[0]] = true
		}
	}
	if len(first) == 1 {
		for prefix := range first {
			if prefix == ".icebath" || prefix == "icebath" {
				return prefix + "/"
			}
		}
	}
	return ""
}

// Mode a plain create gives a new file, false when the umask cannot be read
func defaultNewFileMode() (uint32, bool) { return processNewFileMode() }

// Archive member open, a seam tests replace with a stream that fails mid restore
var openMember = func(zr *ziputil.Reader, member string) (io.ReadCloser, error) { return zr.Open(member) }

// Owner capture of a file about to be replaced, a seam tests replace to force an owner
var preserveFileOwner = fsutil.OwnerOf

// Owner restore of a replaced file, a seam tests replace to record the chown
var restoreFileOwner = fsutil.RestoreOwner

// Atomic publish of a staged member, a seam tests wrap to observe the staged mode
var atomicReplace = fsutil.Replace

// Streams an open member into a file and syncs it
func copyMember(zr *ziputil.Reader, member string, dst *os.File) error {
	src, err := openMember(zr, member)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, src)
	if cerr := src.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return dst.Sync()
}

// First n code points of a name
func nameHead(name string, n int) string {
	runes := []rune(name)
	if len(runes) > n {
		runes = runes[:n]
	}
	return string(runes)
}

// Restores one archive member onto target through a sibling temp file and an atomic rename
func extractMemberAtomically(zr *ziputil.Reader, member, target string, newMode uint32, newModeOK bool) error {
	mode, modeOK := fsutil.PermBits(target)
	owner := preserveFileOwner(target)
	if !modeOK {
		mode, modeOK = newMode, newModeOK
	} else {
		mode &^= 0o4000 | 0o2000
	}
	tmp, err := fsutil.CreateTempBeside(target, "."+nameHead(pathutil.Name(target), 80)+".", ".partial")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	err = func() error {
		if modeOK {
			if err := fchmodStaged(tmp, tmpName, mode); err != nil {
				tmp.Close()
				return err
			}
		}
		if err := copyMember(zr, member, tmp); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		real, err := atomicReplace(tmpName, target)
		if err != nil {
			return err
		}
		restoreFileOwner(real, owner)
		if modeOK {
			_ = fsutil.SetPermBits(real, mode)
		}
		return nil
	}()
	if err != nil {
		os.Remove(tmpName)
	}
	return err
}

// Sessions and messages stored in a session database, nil when it is missing or not a readable session store
func countSessionRows(ctx context.Context, path string) (*[2]int64, error) {
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return nil, nil
	}
	uri, err := state.ReadOnlyDBURI(path)
	if err != nil {
		return nil, err
	}
	c, err := connectURI(ctx, uri)
	if err != nil {
		if isSQLiteError(err) {
			return nil, nil
		}
		return nil, err
	}
	defer closeQuietly(c)
	var counts [2]int64
	for i, q := range []string{"SELECT COUNT(*) FROM sessions", "SELECT COUNT(*) FROM messages"} {
		var v any
		if err := c.c.QueryRowContext(ctx, q).Scan(&v); err != nil {
			if isSQLiteError(err) || errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			return nil, err
		}
		n, err := sqliteInt(v)
		if err != nil {
			if errors.Is(err, anyutil.ErrNotNumber) {
				return nil, nil
			}
			return nil, err
		}
		counts[i] = n
	}
	return &counts, nil
}

// Page restores a database member while the target's maintenance reservation is held
func importDBMember(ctx context.Context, zr *ziputil.Reader, member, target string, newMode uint32, newModeOK bool) error {
	abs, err := pathAbsolute(target)
	if err != nil {
		return err
	}
	resolved, err := pathutil.Resolve(target)
	if err != nil {
		return err
	}
	inner, release, err := gateway.ExclusiveMaintenance(ctx, []string{filepath.Dir(filepath.Clean(abs)), filepath.Dir(filepath.Clean(resolved))}, "")
	if err != nil {
		if isOwnershipConflict(err) {
			return &restoreError{err.Error()}
		}
		return err
	}
	defer release()
	return importDBMemberExclusive(inner, zr, member, target, newMode, newModeOK)
}

// Page restore of a database member, a seam tests replace to refuse it
var safeRestoreDBFn = safeRestoreDB

// Publishes a database member onto target without replacing its inode, a restore failure when that cannot be done safely
func importDBMemberExclusive(ctx context.Context, zr *ziputil.Reader, member, target string, newMode uint32, newModeOK bool) error {
	if _, err := os.Stat(target); err != nil {
		holders, _ := foreignDBHolderPIDs(target)
		if len(holders) > 0 {
			sorted := append([]int(nil), holders...)
			sort.Ints(sorted)
			parts := make([]string, len(sorted))
			for i, pid := range sorted {
				parts[i] = strconv.Itoa(pid)
			}
			return &restoreError{fmt.Sprintf("%s was deleted but is still open in PID(s) %s; publishing a new file would "+
				"leave them writing an invisible database. Stop those processes and re-run the import.",
				pathutil.Name(target), strings.Join(parts, ", "))}
		}
		return extractMemberAtomically(zr, member, target, newMode, newModeOK)
	}
	mode, modeOK := fsutil.PermBits(target)
	owner := preserveFileOwner(target)
	tmp, err := os.CreateTemp(filepath.Dir(filepath.Clean(target)), "."+nameHead(pathutil.Name(target), 80)+".*.dbimport")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := copyMember(zr, member, tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	restored, err := safeRestoreDBFn(ctx, tmpName, target)
	if err != nil {
		return err
	}
	if !restored {
		if !VerifySQLiteIntegrity(tmpName, IntegrityOptions{}).Valid {
			return &restoreError{"the archived database failed its integrity check; the existing database was left untouched."}
		}
		return &restoreError{"live-safe restore refused or failed; the existing database was left untouched. " +
			"Stop the gateway and TUI processes holding it open and re-run the import."}
	}
	restoreFileOwner(target, owner)
	if modeOK {
		_ = fsutil.SetPermBits(target, mode)
	}
	return nil
}

// Value at a dotted path of a config mapping, nil when any step is missing or not a mapping
func getConfigPathValue(data map[string]any, dotted []string) any {
	var node any = data
	for _, key := range dotted {
		m, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node = m[key]
	}
	return node
}

// Sets the value at a dotted path, replacing any non mapping step with a new mapping
func setConfigPathValue(data map[string]any, dotted []string, value any) {
	node := data
	for _, key := range dotted[:len(dotted)-1] {
		child, ok := node[key].(map[string]any)
		if !ok {
			child = map[string]any{}
			node[key] = child
		}
		node = child
	}
	node[dotted[len(dotted)-1]] = value
}

// Number of cron jobs stored in a jobs file, nil when it is missing, unparseable or of an unknown shape
func countCronJobs(path string) (*int, error) {
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	text, err := charsetutil.Decode(raw, "utf-8-sig")
	if err != nil {
		return nil, err
	}
	data, err := jsonutil.Decode(text)
	if err != nil {
		var de *jsonutil.SyntaxError
		if errors.As(err, &de) {
			return nil, nil
		}
		return nil, err
	}
	switch t := data.(type) {
	case *jsonutil.Object:
		jobs, ok := t.Get("jobs")
		if !ok {
			n := 0
			return &n, nil
		}
		if list, ok := jobs.([]any); ok {
			n := len(list)
			return &n, nil
		}
		return nil, nil
	case []any:
		n := len(t)
		return &n, nil
	}
	return nil, nil
}
