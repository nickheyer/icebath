package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pysqlite"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyzip"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// OSError the restore code raises with its message
type osError struct{ msg string }

func (e *osError) Error() string      { return e.msg }
func (e *osError) PyTypeName() string { return "OSError" }

// ValueError or TypeError of a Python int() conversion the restore code does not catch
type pyValueError struct {
	class, msg string
}

func (e *pyValueError) Error() string      { return e.msg }
func (e *pyValueError) PyTypeName() string { return e.class }

// Reports whether err is a Python OSError, a filesystem failure or one the restore code raised
func isOSError(err error) bool {
	if err == nil {
		return false
	}
	var own *osError
	var zipOS *pyzip.OSError
	var conflict *gateway.OwnershipConflict
	if errors.As(err, &own) || errors.As(err, &zipOS) {
		return true
	}
	if errors.As(err, &conflict) {
		return false
	}
	return pypath.IsOSError(err)
}

// Reports whether err is a sqlite3.Error of any class
func isSQLiteError(err error) bool {
	var pe *pysqlite.Error
	if errors.As(err, &pe) {
		return true
	}
	_, ok := state.SQLiteClassOf(err)
	return ok
}

// Reports whether err is a ValueError raised for an unrepresentable value or path
func isValueError(err error) bool {
	var ve *pyValueError
	return errors.As(err, &ve) && ve.class == "ValueError"
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
	if pypath.IsAbs(p) {
		return pypath.PathStr(p), nil
	}
	cwd, err := pypath.Getcwd()
	if err != nil {
		return "", err
	}
	return pypath.PathJoin(cwd, p), nil
}

// Python int of a SQLite value, ValueError and TypeError as int() raises them
func sqliteInt(v any) (*big.Int, error) {
	if b, ok := v.([]byte); ok {
		if n, ok := pyint.Parse(string(b)); ok {
			return n, nil
		}
		return nil, &pyValueError{"ValueError", "invalid literal for int() with base 10: " + pyrepr.BytesRepr(b)}
	}
	n, err := pyint.Of(v)
	if err != nil {
		var pe *pyint.Error
		if errors.As(err, &pe) {
			return nil, &pyValueError{pe.Kind, pe.Msg}
		}
		return nil, err
	}
	return n, nil
}

// PIDs of other processes holding the database or its WAL or SHM open, false when the scan cannot run
func foreignDBHolderPIDs(dbPath string) ([]int, bool) {
	if runtime.GOOS != "linux" {
		return nil, false
	}
	canonical := func(p string) (string, error) {
		abs, err := pypath.Abspath(strings.TrimSuffix(p, " (deleted)"))
		if err != nil {
			return "", err
		}
		return pypath.Normcase(abs), nil
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
		if !pystr.All(name, pystr.IsDigit) {
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
			slog.Error(fmt.Sprintf("Refusing auth.json restore to %s: cannot resolve store identity: %s", pypath.PathStr(dst), excText(err)))
			return ""
		}
		if links > 1 {
			slog.Error(fmt.Sprintf("Refusing auth.json restore to %s: it has %d hard links; atomic replacement would split a shared auth store", pypath.PathStr(dst), links))
			return ""
		}
	}
	if st, err := os.Lstat(dst); err == nil && st.Mode()&os.ModeSymlink != 0 {
		resolved, err := pypath.Resolve(dst)
		if err != nil {
			slog.Error(fmt.Sprintf("Refusing auth.json restore to %s: cannot resolve store identity: %s", pypath.PathStr(dst), excText(err)))
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
	case *big.Int:
		f, _ := new(big.Float).SetInt(t).Float64()
		return f
	}
	return v
}

// Restores auth.json under the auth store lock without rolling back a live single use OAuth generation
func restoreAuthJSON(src, dst string) bool {
	raw, err := os.ReadFile(src)
	var doc any
	if err == nil {
		var text string
		if text, err = pycodecs.Decode(raw, "utf-8-sig"); err == nil {
			doc, err = pyjson.Loads(text)
		}
	}
	if err != nil {
		slog.Error(fmt.Sprintf("Refusing auth.json restore from %s: %s", pypath.PathStr(src), excText(err)))
		return false
	}
	obj, ok := doc.(*pyjson.Object)
	if !ok {
		slog.Error(fmt.Sprintf("Refusing auth.json restore from %s: top level is not an object", pypath.PathStr(src)))
		return false
	}
	isDict := func(key string) bool {
		v, _ := obj.Get(key)
		_, ok := v.(*pyjson.Object)
		return ok
	}
	if !isDict("providers") && !isDict("credential_pool") && !isDict("systems") {
		slog.Error(fmt.Sprintf("Refusing auth.json restore from %s: unrecognized auth-store shape", pypath.PathStr(src)))
		return false
	}
	snapshot, _ := authStoreValue(pyjson.Plain(obj)).(map[string]any)
	if !isDict("providers") && !isDict("credential_pool") && isDict("systems") {
		systems, _ := snapshot["systems"].(map[string]any)
		providers := map[string]any{}
		var active any
		if nous, ok := systems["nous_portal"]; ok {
			providers["nous"] = nous
			active = "nous"
		}
		snapshot = map[string]any{"providers": providers, "active_provider": active}
	}
	if err := saveRestoredAuth(snapshot, dst); err != nil {
		if errors.Is(err, errAuthTargetRefused) {
			return false
		}
		slog.Error(fmt.Sprintf("Failed to restore %s safely: %s", pypath.PathStr(dst), excText(err)))
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
		slog.Error(fmt.Sprintf("Refusing SQLite restore from %s: %s", pypath.PathStr(src), check.Message))
		return false, nil
	}
	ok, err := func() (bool, error) {
		abs, err := pathAbsolute(dst)
		if err != nil {
			return false, err
		}
		resolved, err := pypath.Resolve(dst)
		if err != nil {
			return false, err
		}
		inner, release, err := gateway.ExclusiveMaintenance(ctx, []string{pypath.PathParent(abs), pypath.PathParent(resolved)}, "")
		if err != nil {
			return false, err
		}
		defer release()
		return restoreEpochSource(inner, src, dst, func(prepared string) (bool, error) {
			return restoreDBPages(inner, prepared, dst)
		})
	}()
	if err != nil {
		if isOwnershipConflict(err) || isOSError(err) || isSQLiteError(err) {
			slog.Error(excText(err))
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
func restoreEpoch(ctx context.Context, path string) (*big.Int, error) {
	if _, err := os.Stat(path); err != nil {
		return big.NewInt(0), nil
	}
	c, err := connectReadOnly(ctx, path)
	if err != nil {
		return nil, err
	}
	defer closeQuietly(c)
	var one any
	err = c.c.QueryRowContext(ctx, "SELECT 1 FROM sqlite_master WHERE name='runtime_epoch'").Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return big.NewInt(0), nil
	}
	if err != nil {
		return nil, err
	}
	var epoch any
	err = c.c.QueryRowContext(ctx, "SELECT epoch FROM runtime_epoch WHERE singleton=1").Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return big.NewInt(0), nil
	}
	if err != nil {
		return nil, err
	}
	return sqliteInt(epoch)
}

// Epoch floor of a restore destination, an OSError naming the salvage route when it cannot be read
func restoreDestinationEpoch(ctx context.Context, dst string) (*big.Int, error) {
	epoch, err := restoreEpoch(ctx, dst)
	if err != nil && (isOSError(err) || isSQLiteError(err) || isValueError(err)) {
		return nil, &osError{fmt.Sprintf("Cannot read the canonical runtime epoch at %s; refusing in-place restore. "+
			"Use `icebath sessions recover --source <snapshot> --output <separate-path>` "+
			"to salvage transcripts into a separate output.", pypath.PathStr(dst))}
	}
	return epoch, err
}

// Hands publish the snapshot, or a staged copy carrying the destination's epoch floor when it is higher
func restoreEpochSource(ctx context.Context, src, dst string, publish func(prepared string) (bool, error)) (bool, error) {
	if pypath.PathName(dst) != "state.db" {
		resolved, err := pypath.Resolve(dst)
		if err != nil {
			return false, err
		}
		if pypath.PathName(resolved) != "state.db" {
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
	if floor.Cmp(srcEpoch) <= 0 {
		return publish(src)
	}
	if !floor.IsInt64() {
		return false, &pyValueError{"OverflowError", "Python int too large to convert to SQLite INTEGER"}
	}
	work, err := os.MkdirTemp(pypath.PathParent(dst), ".restore-epoch-*")
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
			return false, &osError{"Unable to stage a restore with a safe runtime epoch"}
		}
		if err := stampRestoreEpoch(ctx, prepared, floor.Int64()); err != nil {
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
	if rmErr := pyfs.RemoveTempDir(work); rmErr != nil {
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
			slog.Debug(fmt.Sprintf("Pre-restore WAL checkpoint of %s skipped: %s", pypath.PathStr(dst), excText(err)))
		}
		srcConn, err := connectReadOnly(ctx, src)
		if err != nil {
			return err
		}
		err = pysqlite.Backup(srcConn, dstConn, -1, nil, 250*time.Millisecond)
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
			slog.Warn(fmt.Sprintf("Restored %s but could not apply the snapshot's file mode: %s", pypath.PathStr(dst), excText(err)))
		}
		return nil
	}()
	if err == nil {
		return true, nil
	}
	if !isSQLiteError(err) && !isOSError(err) && !isValueError(err) {
		closeQuietly(dstConn)
		return false, err
	}
	slog.Warn(fmt.Sprintf("SQLite safe restore failed for %s -> %s: %s", pypath.PathStr(src), pypath.PathStr(dst), excText(err)))
	if dstConn != nil {
		if err := dstConn.close(); err != nil {
			slog.Debug(fmt.Sprintf("Closing %s before the fallback restore failed: %s", pypath.PathStr(dst), excText(err)))
		}
	}
	return replaceDBFile(src, dst)
}

// Unlink and move fallback of a page restore, refused while any process or in process connection holds dst
func replaceDBFile(src, dst string) (bool, error) {
	holders, _ := foreignDBHolderPIDs(dst)
	if len(holders) > 0 {
		slog.Error(fmt.Sprintf("Refusing unlink+move restore of %s: process(es) %s still hold the database or its WAL open. Stop them and retry.",
			pypath.PathStr(dst), pyrepr.Repr(holders)))
		return false, nil
	}
	var swapErr error
	err := state.OfflineFileAccess(dst, "unlink+move restore of", func() {
		tmp := filepath.Join(pypath.PathParent(dst), "."+pypath.PathName(dst)+".snap_restore")
		if swapErr = pyfs.Copy2(src, tmp); swapErr != nil {
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
		_, swapErr = pyfs.Move(tmp, dst)
	})
	var live *state.LiveConnectionError
	if errors.As(err, &live) {
		slog.Error(fmt.Sprintf("Refusing unlink+move restore of %s: %s Close the in-process database handles (or restart Icebath) and retry.",
			pypath.PathStr(dst), live.Error()))
		return false, nil
	}
	if err == nil {
		err = swapErr
	}
	if err != nil {
		if isOSError(err) || isValueError(err) {
			slog.Error(fmt.Sprintf("Fallback restore also failed for %s -> %s: %s", pypath.PathStr(src), pypath.PathStr(dst), excText(err)))
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Checks that an archive looks like an icebath backup, the reason when it does not
func validateBackupZip(zr *pyzip.ZipReader) (bool, string) {
	names := zr.Namelist()
	if len(names) == 0 {
		return false, "zip archive is empty"
	}
	markers := map[string]bool{"config.yaml": true, ".env": true, "state.db": true}
	for _, n := range names {
		if markers[pypath.PathName(n)] {
			return true, ""
		}
	}
	return false, "zip does not appear to be an Icebath backup (no config.yaml, .env, or state databases found)"
}

// Directory prefix wrapping every archive member when it names an icebath home, empty otherwise
func detectPrefix(zr *pyzip.ZipReader) string {
	var names []string
	for _, n := range zr.Namelist() {
		if !strings.HasSuffix(n, "/") {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return ""
	}
	first := map[string]bool{}
	for _, n := range names {
		anchor, parts := pypath.PathParts(n)
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
var openMember = func(zr *pyzip.ZipReader, member string) (io.ReadCloser, error) { return zr.Open(member) }

// Owner capture of a file about to be replaced, a seam tests replace to force an owner
var preserveFileOwner = pyfs.PreserveFileOwner

// Owner restore of a replaced file, a seam tests replace to record the chown
var restoreFileOwner = pyfs.RestoreFileOwner

// Atomic publish of a staged member, a seam tests wrap to observe the staged mode
var atomicReplace = pyfs.AtomicReplace

// Streams an open member into a file and syncs it
func copyMember(zr *pyzip.ZipReader, member string, dst *os.File) error {
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
	runes := pystr.Runes(name)
	if len(runes) > n {
		runes = runes[:n]
	}
	return pystr.FromRunes(runes)
}

// Restores one archive member onto target through a sibling temp file and an atomic rename
func extractMemberAtomically(zr *pyzip.ZipReader, member, target string, newMode uint32, newModeOK bool) error {
	mode, modeOK := pyfs.PreserveFileMode(target)
	owner := preserveFileOwner(target)
	if !modeOK {
		mode, modeOK = newMode, newModeOK
	} else {
		mode &^= 0o4000 | 0o2000
	}
	tmp, err := pyfs.MkstempBeside(target, "."+nameHead(pypath.PathName(target), 80)+".", ".partial")
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
		pyfs.RestoreFileMode(real, mode, modeOK)
		return nil
	}()
	if err != nil {
		os.Remove(tmpName)
	}
	return err
}

// Sessions and messages stored in a session database, nil when it is missing or not a readable session store
func countSessionRows(ctx context.Context, path string) (*[2]*big.Int, error) {
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
	var counts [2]*big.Int
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
			var ve *pyValueError
			if errors.As(err, &ve) && (ve.class == "ValueError" || ve.class == "TypeError") {
				return nil, nil
			}
			return nil, err
		}
		counts[i] = n
	}
	return &counts, nil
}

// Page restores a database member while the target's maintenance reservation is held
func importDBMember(ctx context.Context, zr *pyzip.ZipReader, member, target string, newMode uint32, newModeOK bool) error {
	abs, err := pathAbsolute(target)
	if err != nil {
		return err
	}
	resolved, err := pypath.Resolve(target)
	if err != nil {
		return err
	}
	inner, release, err := gateway.ExclusiveMaintenance(ctx, []string{pypath.PathParent(abs), pypath.PathParent(resolved)}, "")
	if err != nil {
		if isOwnershipConflict(err) {
			return &osError{err.Error()}
		}
		return err
	}
	defer release()
	return importDBMemberExclusive(inner, zr, member, target, newMode, newModeOK)
}

// Page restore of a database member, a seam tests replace to refuse it
var safeRestoreDBFn = safeRestoreDB

// Publishes a database member onto target without replacing its inode, an OSError when that cannot be done safely
func importDBMemberExclusive(ctx context.Context, zr *pyzip.ZipReader, member, target string, newMode uint32, newModeOK bool) error {
	if _, err := os.Stat(target); err != nil {
		holders, _ := foreignDBHolderPIDs(target)
		if len(holders) > 0 {
			sorted := append([]int(nil), holders...)
			sort.Ints(sorted)
			parts := make([]string, len(sorted))
			for i, pid := range sorted {
				parts[i] = strconv.Itoa(pid)
			}
			return &osError{fmt.Sprintf("%s was deleted but is still open in PID(s) %s; publishing a new file would "+
				"leave them writing an invisible database. Stop those processes and re-run the import.",
				pypath.PathName(target), strings.Join(parts, ", "))}
		}
		return extractMemberAtomically(zr, member, target, newMode, newModeOK)
	}
	mode, modeOK := pyfs.PreserveFileMode(target)
	owner := preserveFileOwner(target)
	tmp, err := os.CreateTemp(pypath.PathParent(target), "."+nameHead(pypath.PathName(target), 80)+".*.dbimport")
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
			return &osError{"the archived database failed its integrity check; the existing database was left untouched."}
		}
		return &osError{"live-safe restore refused or failed; the existing database was left untouched. " +
			"Stop the gateway/dashboard processes holding it open and re-run the import."}
	}
	restoreFileOwner(target, owner)
	pyfs.RestoreFileMode(target, mode, modeOK)
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
	text, err := pycodecs.Decode(raw, "utf-8-sig")
	if err != nil {
		return nil, err
	}
	data, err := pyjson.Loads(text)
	if err != nil {
		var de *pyjson.DecodeError
		if errors.As(err, &de) {
			return nil, nil
		}
		return nil, err
	}
	switch t := data.(type) {
	case *pyjson.Object:
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
