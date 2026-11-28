package sessions

import (
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	mathrand "math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	"modernc.org/sqlite"

	"github.com/nickheyer/icebath/internal/config"
	sqliteconfig "github.com/nickheyer/icebath/internal/plugins/defaults/sessions/sqlite/config"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/sqlitex"
	"github.com/nickheyer/icebath/pkg/strs"
)

const name = "sqlite"

const (
	// How long a write waits out another process's transaction, a checkpoint or a VACUUM before it gives up
	writePatience = 20 * time.Second
	// Transcript writes wait longer, since losing one loses what the model said
	transcriptPatience = 60 * time.Second
	// Writes between passive checkpoints and between bounded merges of the search index
	checkpointEvery = 50
	mergeEvery      = 1000
	// Byte cap of the write-ahead log once checkpointed
	walSizeLimit = 64 << 20
)

const replacedText = "The session database was replaced while icebath had it open, so icebath saves nothing more to it. " +
	"Restart icebath so it opens the file that is there now."

const corruptText = "The session database is damaged beyond its search index, so icebath saves nothing more to it. Stop icebath, then run " +
	"icebath sessions repair, or icebath sessions recover --source %s --inspect-only to see what can be rescued. SQLite said: %s."

const lockedText = "The session database is busy, because another icebath has held it for more than %s, such as for a long cleanup. The " +
	"database itself is fine, so try again once the other is done."

// Told the person once when the file at the path of the database was set aside and a new database started there
const (
	otherVersionText = "Your session database was made by another version of icebath, so icebath started a new one. The old database is kept " +
		"at %s."
	damagedText = "Your session database was empty or damaged, so icebath started a new one. To see which sessions can be brought back, run " +
		"icebath sessions recover --source %s --inspect-only."
)

// Told the person once when the journal mode of the file asks something of them
const (
	deleteKeptText = "Journal mode in /settings under Session storage › SQLite is Delete, but the session database already uses a write-ahead log, so " +
		"icebath keeps the write-ahead log. To switch, stop every other icebath and run icebath sessions set-journal-mode delete."
	crossVMText = "The session database is in a folder shared with a virtual machine, where a write-ahead log can damage the database when " +
		"two icebath windows write at once. Stop every other icebath and run icebath sessions set-journal-mode delete, or move the database " +
		"onto a disk of this computer."
)

const settingUpText = "The session database at %s was made by another version of icebath, and another icebath is setting it up right now. " +
	"Try again in a moment."

// What SQLite reports of the file, said so the person can act on it
const (
	readOnlyText = "icebath can't write to the session database at %s, because the file or its folder is read-only for your user. Make " +
		"them writable with chmod u+rw, then try again."
	diskText     = "The disk that holds the session database at %s reported an error, so icebath could not save. Check the disk, then try again."
	fullText     = "The disk that holds the session database is full, so icebath can't save to it. Free some space in %s, then try again."
	cantOpenText = "icebath can't open the session database at %s. Check that the folder %s exists and that your user can write to it."
	failedText   = "icebath can't use the session database at %s, because SQLite said: %s. Run icebath sessions repair to check and mend " +
		"the database."
)

type store struct {
	config *config.Config
	// Registry the store is registered in, whose hooks hear of the sessions it takes out
	registry *sessions.Registry

	mu   sync.Mutex
	db   *sql.DB
	path string
	// Device and inode of the file opened, to tell when it is replaced
	dev, ino uint64
	// Journal mode the file runs in once opened
	mode string
	// Set once the file is found replaced or structurally corrupt; every write is refused from then on
	halted error
	// Set while the search indexes are left out of step after damage, so searches scan instead
	stale   atomic.Bool
	trigram atomic.Bool
	// Set while this process rebuilds the search indexes in the background
	mending atomic.Bool
	// Life of the running server, which background work of the store ends with; nil until it starts
	background context.Context
	writes     atomic.Int64
	feedMu     sync.Mutex
	watchers   map[chan []string]struct{}
	polling    bool
}

func Register(registry *sessions.Registry, cfg *config.Config) {
	registry.Register(&store{config: cfg, registry: registry, watchers: map[chan []string]struct{}{}})
}

// Tells the session hooks of the sessions taken out of the store, by the profile each was of
func (s *store) deleted(ctx context.Context, listed []*sessionsv1.Session, removed []string, reason, primary string) {
	owners := map[string]string{}
	for _, session := range listed {
		owners[session.GetId()] = session.GetProfile()
	}
	byProfile := map[string][]string{}
	for _, id := range removed {
		byProfile[owners[id]] = append(byProfile[owners[id]], id)
	}
	for profile, ids := range byProfile {
		s.registry.Deleted(ctx, s.config.In(profile), ids, reason, primary)
	}
}

func (s *store) Stores() map[string]sessions.Store {
	return map[string]sessions.Store{name: s}
}

// Name of the store where a person picks one
func (s *store) Title() string { return "SQLite" }

// What the store keeps sessions in, where a person picks one
func (s *store) About() string { return "A database file on this computer" }

func (s *store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	if s.halted == nil && s.mode == sqliteconfig.WAL {
		s.db.Exec("PRAGMA wal_checkpoint(PASSIVE)")
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// Database of the store, opened the first time it is asked for
func (s *store) open(ctx context.Context) (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		return s.db, nil
	}
	path := s.config.GetString(sqliteconfig.Path)
	if path == "" {
		return nil, errors.New("No file is set for the session database. Set Database file in /settings under Session storage › SQLite.")
	}
	if !filepath.IsAbs(path) {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		path = absolute
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("The folder %s for the session database can't be made: %w.", filepath.Dir(path), err)
	}
	if err := preflight(path); err != nil {
		return nil, err
	}
	var db *sql.DB
	// The first open of a file, and the moving aside of a damaged one or one of another layout, happen one process at a time
	err := sqlitex.FirstOpen(path, 5*time.Second, func(locked bool) error {
		if !locked {
			slog.Warn("The lock of the session database's first open was not taken within 5s; opening without it, and a damaged file or one of "+
				"another layout is left in place", "path", path)
		} else if err := s.quarantine(path); err != nil {
			return err
		}
		var err error
		db, err = s.connect(ctx, path)
		if _, other := errors.AsType[*sqlitex.Mismatch](err); other {
			if !locked {
				return fmt.Errorf(settingUpText, path)
			}
			db, err = s.replace(ctx, path, err)
		}
		return err
	})
	if err != nil {
		return nil, plain(path, err)
	}
	s.db, s.path = db, path
	if info, err := os.Stat(path); err == nil {
		s.dev, s.ino = identity(info)
	}
	return db, nil
}

// Opens the file and readies it, made owner only before SQLite first opens it
func (s *store) connect(ctx context.Context, path string) (*sql.DB, error) {
	if err := secure(path, true); err != nil {
		return nil, err
	}
	dsn, err := s.dsn(path, 5*time.Second)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := s.init(ctx, db, path); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Moves a file of another layout aside, tells the person where it went, and starts a new database in its place
func (s *store) replace(ctx context.Context, path string, cause error) (*sql.DB, error) {
	backup, err := sqlitex.Aside(path, sqlitex.OtherVersion)
	if err != nil {
		return nil, fmt.Errorf("The session database at %s was made by another version of icebath, and it could not be moved aside: %s Move it "+
			"somewhere else, then start icebath again.", path, strs.Sentence(err.Error()))
	}
	slog.Warn("The session database has another layout, so it was moved aside and a new one started", "backup", backup, "reason", cause)
	s.registry.Tell(fmt.Sprintf(otherVersionText, backup))
	return s.connect(ctx, path)
}

// Failure of the session database said so the person can act on it, by what SQLite reported; any other failure as it is
func plain(path string, err error) error {
	var problem *sqlite.Error
	if !errors.As(err, &problem) {
		return err
	}
	switch problem.Code() & 0xff {
	case 3, 8, 23:
		return fmt.Errorf(readOnlyText, path)
	case 5, 6:
		return fmt.Errorf(lockedText, strs.Span(writePatience))
	case 10:
		return fmt.Errorf(diskText, path)
	case 11, 26:
		return fmt.Errorf(corruptText, path, said(problem))
	case 13:
		return fmt.Errorf(fullText, filepath.Dir(path))
	case 14:
		return fmt.Errorf(cantOpenText, path, filepath.Dir(path))
	}
	return fmt.Errorf(failedText, path, said(problem))
}

// What SQLite said, without the number of its code
func said(err error) string {
	text := err.Error()
	if open := strings.LastIndex(text, " ("); open > 0 && strings.HasSuffix(text, ")") {
		if _, err := strconv.Atoi(text[open+2 : len(text)-1]); err == nil {
			return text[:open]
		}
	}
	return text
}

// Connection string of the store: the pragmas every connection runs, and writes that take the write lock as they begin
func (s *store) dsn(path string, busy time.Duration) (string, error) {
	pragmas := []string{fmt.Sprintf("busy_timeout(%d)", busy.Milliseconds()), "foreign_keys(1)"}
	whole := func(key, pragma string, least *int) error {
		text := strings.TrimSpace(s.config.GetString(key))
		if text == "" {
			return nil
		}
		n, err := strconv.Atoi(text)
		if err != nil {
			_, setting, _ := s.config.Setting(key)
			return fmt.Errorf("%s takes a whole number, not %q.", setting.Title, text)
		}
		if least != nil {
			n = max(n, *least)
		}
		pragmas = append(pragmas, fmt.Sprintf("%s(%d)", pragma, n))
		return nil
	}
	zero := 0
	if err := whole(sqliteconfig.CacheSize, "cache_size", nil); err != nil {
		return "", err
	}
	if err := whole(sqliteconfig.MmapSize, "mmap_size", &zero); err != nil {
		return "", err
	}
	if store := s.config.GetString(sqliteconfig.TempStore); store != "" {
		pragmas = append(pragmas, "temp_store("+store+")")
	}
	if err := whole(sqliteconfig.WALAutocheckpoint, "wal_autocheckpoint", nil); err != nil {
		return "", err
	}
	if err := whole(sqliteconfig.JournalSizeLimit, "journal_size_limit", nil); err != nil {
		return "", err
	}
	if level := strings.ToUpper(s.config.GetString(sqliteconfig.Synchronous)); level != "" {
		if runtime.GOOS == "darwin" && level != "FULL" && level != "EXTRA" {
			slog.Warn("Refusing the configured synchronous level on macOS; keeping FULL. Darwin's fsync() does not guarantee write ordering, so a lower level "+
				"readmits the half-written btree pages FULL exists to prevent.", "synchronous", level)
		} else {
			pragmas = append(pragmas, "synchronous("+level+")")
		}
	}
	query := url.Values{"_pragma": pragmas, "_txlock": {"immediate"}}
	return "file:" + path + "?" + query.Encode(), nil
}

// Journal mode, schema, search indexes and the file's generation stamp, waiting out another opener's write lock
func (s *store) init(ctx context.Context, db *sql.DB, path string) error {
	deadline := time.Now().Add(writePatience)
	for {
		err := s.prepare(ctx, db, path)
		if err == nil || !busy(err) {
			return err
		}
		if time.Now().After(deadline) {
			slog.Warn("Session database write lock unavailable at open", "path", path, "holders", describeHolders(lockHolders(path)))
			return err
		}
		time.Sleep(time.Duration(250+mathrand.IntN(750)) * time.Millisecond)
	}
}

// Readies the file: found to be of the store's layout before anything of it changes, its journal mode, then its shape, tables and search
// indexes in one transaction, so another process never sees them half made; a *sqlitex.Mismatch for a file of another layout
func (s *store) prepare(ctx context.Context, db *sql.DB, path string) error {
	want, err := declare(ctx)
	if err != nil {
		return err
	}
	if _, err := sqlitex.Inspect(ctx, db, want); err != nil {
		return err
	}
	mode, err := s.journal(ctx, db, path)
	if err != nil {
		return err
	}
	s.mode = mode
	if err := secure(path, false); err != nil {
		return err
	}
	cjk := s.config.GetBool(sqliteconfig.CJKFTS)
	stale := false
	err = s.once(ctx, db, func(tx *sql.Tx) error {
		plan, err := sqlitex.Reconcile(ctx, tx, want)
		if err != nil {
			return err
		}
		if len(plan.Added)+len(plan.Dropped) > 0 {
			remade := make([]string, len(plan.Dropped))
			for i, o := range plan.Dropped {
				remade[i] = o.Name
			}
			slog.Info("Brought the session database into the current layout", "path", path, "added", plan.Added, "remade", remade)
		}
		if _, err := tx.ExecContext(ctx, schema); err != nil {
			return err
		}
		if _, stale, err = meta(ctx, tx, "fts_stale"); err != nil {
			return err
		}
		return searchable(ctx, tx, plan, cjk, stale)
	})
	if err != nil {
		return err
	}
	s.trigram.Store(cjk)
	s.stale.Store(stale)
	return stamp(ctx, db)
}

// Makes the search indexes the settings ask for. An index whose view, table or trigger the reconcile dropped is dropped whole and made again,
// and an index made over messages already stored starts a backfill of them in the same transaction, so its triggers and the backfill never
// index one message twice. The trigram index is dropped while it is off, and no triggers are made while the indexes are detached after
// damage, until a rebuild makes them again
func searchable(ctx context.Context, tx *sql.Tx, plan sqlitex.Plan, cjk, stale bool) error {
	for _, i := range []index{wordIndex, trigramIndex} {
		off := i == trigramIndex && !cjk
		if off || slices.ContainsFunc(plan.Dropped, func(o sqlitex.Object) bool { return i.owns(o.Name) }) {
			if err := i.drop(ctx, tx); err != nil {
				return err
			}
		}
		if off {
			if _, err := tx.ExecContext(ctx, `DELETE FROM state_meta WHERE key IN (?, ?)`, i.prefix+"_high_water", i.prefix+"_progress"); err != nil {
				return err
			}
			continue
		}
		var present int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, i.table).Scan(&present); err != nil {
			return err
		}
		if present == 0 {
			if err := seed(ctx, tx, i.prefix); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, i.body); err != nil {
			return err
		}
		if stale {
			continue
		}
		if _, err := tx.ExecContext(ctx, i.triggers); err != nil {
			return err
		}
	}
	return nil
}

// Generation of the file, minted by its first opener, and the header id it lets a copy be told apart by
func stamp(ctx context.Context, db *sql.DB) error {
	generation := rand.Text()
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO state_meta (key, value) VALUES ('db_file_generation', ?)`, generation); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `SELECT value FROM state_meta WHERE key = 'db_file_generation'`).Scan(&generation); err != nil {
		return err
	}
	var application int64
	if err := db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&application); err != nil {
		return err
	}
	if application == 0 {
		sum := hex.EncodeToString([]byte(generation))
		id, _ := strconv.ParseInt(sum[:8], 16, 64)
		if id &= 0x7FFFFFFF; id == 0 {
			id = 1
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id=%d", id)); err != nil {
			slog.Debug("Session database generation stamp skipped", "error", err)
		}
	}
	return nil
}

// Journal mode the file runs in: a WAL file always stays one, since switching it under other processes would unlink the log they still use;
// delete is set only on a file not in WAL, WAL only where the file system can share its memory
func (s *store) journal(ctx context.Context, db *sql.DB, path string) (string, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	current, known := diskMode(ctx, conn)
	configured := s.config.GetString(sqliteconfig.JournalMode)
	if current == sqliteconfig.WAL {
		if configured == sqliteconfig.Delete && once("delete_overridden", "The session database is configured for journal_mode=delete but the "+
			"file is already in WAL; keeping WAL, since a live downgrade under open connections can corrupt it. To apply journal_mode=delete, stop "+
			"every icebath process using it and run `icebath sessions set-journal-mode delete`.") {
			s.registry.Tell(deleteKeptText)
		}
		if crossVM(filepath.Dir(path)) && once("cross_vm_fs_existing_wal", "The WAL session database is on a cross-VM file system (virtiofs or 9p, "+
			"as Docker Desktop, OrbStack and Podman bind mounts are), whose shared memory is not coherent across the VM boundary, so concurrent "+
			"writers can silently corrupt it. Stop every icebath process using it and run `icebath sessions set-journal-mode delete`, or move it "+
			"onto a native volume.") {
			s.registry.Tell(crossVMText)
		}
		return sqliteconfig.WAL, companions(ctx, conn)
	}
	if configured == sqliteconfig.Delete {
		if !known {
			return "", errors.New("icebath could not check the journal mode of the session database, which another process may have open, so it " +
				"keeps the mode as it is instead of switching it to delete. Close the other icebath windows and try again.")
		}
		mode, err := setMode(ctx, conn, "DELETE")
		if err != nil {
			return "", err
		}
		if mode != sqliteconfig.Delete {
			return "", fmt.Errorf("The journal mode of the session database could not be set to delete, as Journal mode in /settings under Session storage › "+
				"SQLite asks. It is %s.", cmp.Or(mode, "unknown"))
		}
		return mode, nil
	}
	if !known {
		once("wal_probe_unknown", "Could not read the session database's journal mode (database is locked or busy); leaving the file as it is rather than "+
			"setting a mode while another connection may hold it.")
		return current, nil
	}
	if crossVM(filepath.Dir(path)) {
		once("cross_vm_fs", "The session database is on a cross-VM file system (virtiofs or 9p, as Docker Desktop, OrbStack and Podman bind mounts are), "+
			"whose WAL shared memory is not coherent across the VM boundary, so journal_mode=delete is used instead. Move it onto a native volume to "+
			"restore WAL.")
		mode, err := setMode(ctx, conn, "DELETE")
		return cmp.Or(mode, sqliteconfig.Delete), err
	}
	mode, err := setMode(ctx, conn, "WAL")
	if err != nil {
		text := strings.ToLower(err.Error())
		if !strings.Contains(text, "locking protocol") && !strings.Contains(text, "not authorized") && !strings.Contains(text, "disk i/o error") {
			return "", err
		}
		once("wal_fallback", fmt.Sprintf("WAL journal mode is not supported on this file system (%s); falling back to journal_mode=delete, slower but "+
			"working on NFS, SMB, FUSE and ZFS.", err))
		if mode, err = setMode(ctx, conn, "DELETE"); err != nil {
			once("wal_delete_fallback_failed", "Both WAL and delete journal modes failed; going on in the connection's current journal mode.")
			mode, _ = diskMode(ctx, conn)
		}
		return cmp.Or(mode, sqliteconfig.Delete), nil
	}
	if mode != sqliteconfig.WAL {
		once("wal_fallback", "WAL journal mode is not supported on this file system; falling back to journal_mode="+mode+".")
		return mode, nil
	}
	if current != "" && current != sqliteconfig.WAL && hasContent(ctx, conn) {
		once("journal_upgrade", "The session database was in journal_mode="+current+" and has been switched to WAL. Set the journal mode to delete in "+
			"/settings under Session storage › SQLite to keep it as it was.")
	}
	return mode, companions(ctx, conn)
}

// Journal mode of the file, false when it can't be read
func diskMode(ctx context.Context, conn *sql.Conn) (string, bool) {
	for range 4 {
		var mode string
		err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode)
		if err == nil {
			return strings.ToLower(mode), true
		}
		if !strings.Contains(strings.ToLower(err.Error()), "disk i/o error") {
			return "", false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "", false
}

// Sets the journal mode with no wait for the lock, so any other opener makes it fail rather than have its log unlinked underneath it
func setMode(ctx context.Context, conn *sql.Conn, mode string) (string, error) {
	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout=0"); err != nil {
		return "", err
	}
	defer conn.ExecContext(ctx, "PRAGMA busy_timeout=5000")
	var got string
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode="+mode).Scan(&got); err != nil {
		return "", err
	}
	return strings.ToLower(got), nil
}

// What every WAL activation carries: the log's size cap, and on macOS checkpoints that reach the disk
func companions(ctx context.Context, conn *sql.Conn) error {
	conn.ExecContext(ctx, fmt.Sprintf("PRAGMA journal_size_limit=%d", walSizeLimit))
	if runtime.GOOS == "darwin" {
		conn.ExecContext(ctx, "PRAGMA checkpoint_fullfsync=1")
		conn.ExecContext(ctx, "PRAGMA synchronous=FULL")
	}
	return nil
}

func hasContent(ctx context.Context, conn *sql.Conn) bool {
	var pages int64
	return conn.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages) == nil && pages > 1
}

// Whether the folder is on a file system shared across a virtual machine boundary, whose shared memory WAL can't trust
func crossVM(dir string) bool {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	best, kind := -1, ""
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		dash := -1
		for i, field := range fields {
			if field == "-" {
				dash = i
				break
			}
		}
		if len(fields) < 5 || dash < 0 || dash+1 >= len(fields) {
			continue
		}
		mount := unescapeMount(fields[4])
		if (dir == mount || strings.HasPrefix(dir, strings.TrimSuffix(mount, "/")+"/") || mount == "/") && len(mount) >= best {
			best, kind = len(mount), fields[dash+1]
		}
	}
	switch kind {
	case "virtiofs", "fuse.virtiofs", "9p", "9p2000", "9p2000.l", "9p2000.u":
		return true
	}
	return false
}

// Path of a mount table field, its octal escapes such as \040 decoded
func unescapeMount(field string) string {
	var out strings.Builder
	for i := 0; i < len(field); i++ {
		if field[i] == '\\' && i+3 < len(field) {
			if n, err := strconv.ParseUint(field[i+1:i+4], 8, 8); err == nil {
				out.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		out.WriteByte(field[i])
	}
	return out.String()
}

var logged sync.Map

// Logs a warning about the database once per process; whether it did so now
func once(key, text string) bool {
	if _, seen := logged.LoadOrStore(key, true); seen {
		return false
	}
	slog.Warn(text)
	return true
}

// Fails with a plain account when the folder or a file of the database is not writable, rather than SQLite's
func preflight(path string) error {
	check := func(target, kind, extra string) error {
		if err := syscall.Access(target, 0x2|0x4); err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOENT) {
				return nil
			}
			x, noun := "", "file"
			if kind == "directory" {
				x, noun = "x", "folder"
			}
			return fmt.Errorf("icebath can't open %s, because the %s %s is read-only for your user and the session database must be written. "+
				"Make it writable with chmod u+rw%s '%s', or with sudo chown when another user owns it.%s", filepath.Base(path), noun, target, x, target, extra)
		}
		return nil
	}
	if err := check(filepath.Dir(path), "directory", ""); err != nil {
		return err
	}
	if err := check(path, "file", ""); err != nil {
		return err
	}
	wal := " Do not delete the -wal file, because it holds saved data that goes into the database once the file is writable."
	if err := check(path+"-wal", "file", wal); err != nil {
		return err
	}
	return check(path+"-shm", "file", "")
}

// Moves an empty or damaged file aside, so icebath starts on a fresh database rather than failing every open, and tells the person
func (s *store) quarantine(path string) error {
	kind, size, err := sqlitex.Damaged(path)
	if err != nil || kind == "" {
		return err
	}
	backup, err := sqlitex.Aside(path, kind)
	if err != nil {
		return fmt.Errorf("The session database at %s is empty or damaged, and it could not be moved aside: %s Run icebath sessions recover "+
			"--source %s --inspect-only to bring old sessions back.", path, strs.Sentence(err.Error()), path)
	}
	slog.Error("The session database was empty or damaged, so it was moved aside and a fresh one started", "bytes", size, "backup", backup)
	s.registry.Tell(fmt.Sprintf(damagedText, backup))
	return nil
}

// Makes the database owner only whatever the umask: created that way before SQLite opens it, its logs tightened after
func secure(path string, create bool) error {
	if create {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
		switch {
		case err == nil:
			f.Close()
		case !errors.Is(err, fs.ErrExist) && !errors.Is(err, syscall.EISDIR):
			return err
		}
	}
	for _, target := range []string{path, path + "-wal", path + "-shm"} {
		if info, err := os.Lstat(target); err == nil && info.Mode().IsRegular() && info.Mode().Perm() != 0o600 {
			if err := os.Chmod(target, 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

func identity(info fs.FileInfo) (uint64, uint64) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Dev), uint64(stat.Ino)
	}
	return 0, 0
}

// Error that stops writes: the file at the path is no longer the one opened, or it was found corrupt
func (s *store) health() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.halted != nil {
		return s.halted
	}
	info, err := os.Stat(s.path)
	if err == nil {
		if dev, ino := identity(info); dev == s.dev && ino == s.ino {
			return nil
		}
	}
	s.halted = sessions.Halted(errors.New(replacedText))
	slog.Error(replacedText, "path", s.path)
	return s.halted
}

// Runs the function in one transaction that holds the write lock from its start, retried with jitter while another process holds the lock
func (s *store) write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return s.writeWithin(ctx, writePatience, fn)
}

func (s *store) writeWithin(ctx context.Context, patience time.Duration, fn func(tx *sql.Tx) error) error {
	db, err := s.open(ctx)
	if err != nil {
		return err
	}
	start := time.Now()
	ioRetried := false
	for {
		if err := s.health(); err != nil {
			return err
		}
		err := s.once(ctx, db, fn)
		if err == nil {
			s.committed(ctx, db)
			return nil
		}
		var problem *sqlite.Error
		if !errors.As(err, &problem) {
			return err
		}
		elapsed := time.Since(start)
		switch code := problem.Code() & 0xff; {
		case code == 5 || code == 6:
			if elapsed < patience {
				if err := pause(ctx, elapsed); err != nil {
					return err
				}
				continue
			}
			slog.Warn("Session database write lock unavailable", "path", s.path, "after", patience, "holders", describeHolders(lockHolders(s.path)))
			return fmt.Errorf(lockedText, strs.Span(patience.Round(time.Second)))
		case code == 10 && !ioRetried:
			ioRetried = true
			if err := pause(ctx, elapsed); err != nil {
				return err
			}
			continue
		case code == 11 || code == 26:
			if indexDamage(err) {
				if s.detachIndexes(ctx, db, err) == nil {
					continue
				}
				return err
			}
			s.mu.Lock()
			if s.halted == nil {
				s.halted = sessions.Halted(fmt.Errorf(corruptText, s.path, said(problem)))
				slog.Error("The session database reported structural corruption outside the search index; refusing further writes", "path", s.path, "error", err)
			}
			halted := s.halted
			s.mu.Unlock()
			return halted
		case code == 1 || code == 3 || code == 8 || code == 10 || code == 13 || code == 14 || code == 23:
			return plain(s.path, err)
		}
		return err
	}
}

func (s *store) once(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Sleeps a jittered while before a write tries again: short in the first two seconds, longer after
func pause(ctx context.Context, elapsed time.Duration) error {
	low, high := 20, 150
	if elapsed >= 2*time.Second {
		low, high = 250, 1000
	}
	timer := time.NewTimer(time.Duration(low+mathrand.IntN(high-low)) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// After a commit: a passive checkpoint every so often, and a bounded merge of the search index
func (s *store) committed(ctx context.Context, db *sql.DB) {
	n := s.writes.Add(1)
	if n%checkpointEvery == 0 && s.mode == sqliteconfig.WAL {
		if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)"); err != nil {
			slog.Warn("WAL checkpoint (PASSIVE) failed", "error", err)
		}
	}
	if n%mergeEvery == 0 && !s.stale.Load() {
		for _, table := range s.indexes() {
			if _, err := db.ExecContext(ctx, "INSERT INTO "+table+"("+table+", rank) VALUES ('merge', 500)"); err != nil {
				slog.Debug("Search index merge failed", "index", table, "error", err)
			}
		}
	}
}

// Search indexes the store keeps
func (s *store) indexes() []string {
	if s.trigram.Load() {
		return []string{"messages_fts", "messages_fts_trigram"}
	}
	return []string{"messages_fts"}
}

// Whether a corruption error is of a search index, which a rebuild repairs, rather than of the messages
func indexDamage(err error) bool {
	var problem *sqlite.Error
	if errors.As(err, &problem) && problem.Code() == 267 {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "messages_fts") || strings.Contains(text, "fts5") && strings.Contains(text, "corrupt")
}

// Drops the triggers of the damaged search indexes and marks them stale, so writes go on and searches scan until a repair rebuilds them
func (s *store) detachIndexes(ctx context.Context, db *sql.DB, cause error) error {
	if s.isStale(ctx, db) {
		return cause
	}
	slog.Error("The session search index is damaged; messages are kept and searches scan them until it is rebuilt from them in the background",
		"error", cause)
	err := s.once(ctx, db, func(tx *sql.Tx) error {
		for _, trigger := range indexTriggers {
			if _, err := tx.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+trigger); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO state_meta (key, value) VALUES ('fts_stale', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			cause.Error())
		return err
	})
	if err == nil {
		s.stale.Store(true)
		s.remend()
	}
	return err
}

// Whether the error is SQLite saying the database is busy or locked
func busy(err error) bool {
	if err == nil {
		return false
	}
	var problem *sqlite.Error
	if errors.As(err, &problem) {
		code := problem.Code() & 0xff
		return code == 5 || code == 6
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "database is locked") || strings.Contains(text, "database is busy")
}

// Database for reading, which under WAL never waits for the writer, and still reads a file whose writes are stopped
func (s *store) read(ctx context.Context) (*sql.DB, error) {
	return s.open(ctx)
}

// Value of a state_meta key, false when absent
func meta(ctx context.Context, q querier, key string) (string, bool, error) {
	var value sql.NullString
	err := q.QueryRowContext(ctx, `SELECT value FROM state_meta WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value.String, value.Valid, err
}

func setMeta(ctx context.Context, q querier, key, value string) error {
	_, err := q.ExecContext(ctx, `INSERT INTO state_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// What runs statements: the database or a transaction
type querier = sqlitex.Querier

// Seconds since the epoch, as times are stored
func now() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

func seconds(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}

func fromSeconds(value float64) time.Time {
	whole, fraction := math.Modf(value)
	return time.Unix(int64(whole), int64(fraction*1e9))
}

func timestampOf(t time.Time) *timestamppb.Timestamp {
	return timestamppb.New(t)
}

// Tells every watcher which sessions changed, none for all of them
func (s *store) changed(ids ...string) {
	s.feedMu.Lock()
	defer s.feedMu.Unlock()
	for watcher := range s.watchers {
		select {
		case watcher <- ids:
		default:
		}
	}
}

func (s *store) Subscribe() (<-chan []string, func()) {
	changes := make(chan []string, 64)
	s.feedMu.Lock()
	s.watchers[changes] = struct{}{}
	start := !s.polling
	s.polling = true
	s.feedMu.Unlock()
	if start {
		go s.poll()
	}
	return changes, func() {
		s.feedMu.Lock()
		defer s.feedMu.Unlock()
		delete(s.watchers, changes)
	}
}

// Watches for commits of other processes while anyone watches the store, telling the watchers anything may have changed
func (s *store) poll() {
	ctx := context.Background()
	var conn *sql.Conn
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	last := int64(-1)
	for range time.Tick(2 * time.Second) {
		s.feedMu.Lock()
		if len(s.watchers) == 0 {
			s.polling = false
			s.feedMu.Unlock()
			return
		}
		s.feedMu.Unlock()
		if conn == nil {
			db, err := s.read(ctx)
			if err != nil {
				continue
			}
			if conn, err = db.Conn(ctx); err != nil {
				continue
			}
		}
		var version int64
		if err := conn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&version); err != nil {
			conn.Close()
			conn = nil
			continue
		}
		if last >= 0 && version != last {
			s.changed()
		}
		last = version
	}
}
