package sessions

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	sqliteconfig "github.com/nickheyer/icebath/internal/plugins/defaults/sessions/sqlite/config"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/sqlitex"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Assistant reply text that is only a bracketed tool name, left by a tool-call template of older local models
var staleMarker = regexp.MustCompile(`^\[[A-Za-z_][A-Za-z0-9_.-]*\]$`)

// Failed repairs of one damaged file after which repair stops trying it
const repairBudget = 3

func (s *store) Maintain(ctx context.Context, r *sessionsv1.MaintainRequest, progress func(string)) error {
	switch r.GetTask() {
	case sessionsv1.Task_TASK_OPTIMIZE:
		return s.optimize(ctx, r, progress)
	case sessionsv1.Task_TASK_OPTIMIZE_STORAGE:
		return s.optimizeStorage(ctx, r, progress)
	case sessionsv1.Task_TASK_CLEAN_MARKERS:
		return s.cleanMarkers(ctx, r, progress)
	case sessionsv1.Task_TASK_REPAIR:
		return s.repair(ctx, r, progress)
	case sessionsv1.Task_TASK_RECOVER:
		return s.recover(ctx, r, progress)
	case sessionsv1.Task_TASK_JOURNAL_MODE:
		return s.journalMode(ctx, r, progress)
	}
	return errors.New("Unknown maintenance task.")
}

// Count of bytes in words
func bytesOf(n int64) string {
	return strs.Size(float64(n))
}

// What a repair strategy did, in words
func how(strategy string) string {
	switch strategy {
	case "reconcile_schema":
		return "It added the tables and columns the database lacked, and made its indexes, triggers and views as this version of icebath " +
			"makes them."
	case "rebuild_fts":
		return "It rebuilt the search index."
	case "reindex_btree":
		return "It rebuilt the indexes of the database."
	case "dedup_schema":
		return "It removed doubled entries from the layout of the database."
	case "drop_fts_rebuild":
		return "It dropped the search index and built it again."
	case "repaired_by_other_process":
		return "Another icebath repaired it first."
	case "already_healthy":
		return "It was in good shape already."
	}
	return "It used the repair " + strategy + "."
}

func size(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func (s *store) optimize(ctx context.Context, r *sessionsv1.MaintainRequest, progress func(string)) error {
	db, err := s.open(ctx)
	if err != nil {
		return err
	}
	if refused := refusal("optimize", s.path, foreignHolders(s.path), r.GetForce()); refused != "" {
		return errors.New(refused)
	}
	before := size(s.path)
	progress("Tidying the search index and compacting the session database…")
	merged := s.optimizeIndexes(ctx, db)
	if err := s.vacuum(ctx); err != nil {
		return fmt.Errorf("The session database could not be compacted: %s", strs.Sentence(err.Error()))
	}
	after := size(s.path)
	var pages, pageSize int64
	if db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages) == nil && db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize) == nil {
		after = pages * pageSize
	}
	progress("✓ Tidied " + strs.Count(merged, "search index") + ".")
	change := "which freed " + bytesOf(before-after)
	if after > before {
		change = "which grew it by " + bytesOf(after-before)
	}
	progress(fmt.Sprintf("The database went from %s to %s, %s.", bytesOf(before), bytesOf(after), change))
	return nil
}

// Why the search indexes need a rebuild, empty when they are in step
func (s *store) indexDrift(ctx context.Context, db *sql.DB) (string, error) {
	if _, stale, err := meta(ctx, db, "fts_stale"); err != nil || stale {
		return "the search index was set aside after damage", err
	}
	if _, _, running, err := progressOf(ctx, db, wordBackfill); err != nil || running {
		return "an earlier rebuild was interrupted", err
	}
	if _, _, running, err := progressOf(ctx, db, trigramBackfill); err != nil || running {
		return "an earlier rebuild of the search index for East Asian text was interrupted", err
	}
	var messages, indexed int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&messages); err != nil {
		return "", err
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages_fts_docsize`).Scan(&indexed); err != nil {
		return "the search index is missing", nil
	}
	if messages > 0 && indexed == 0 {
		return "the search index is empty while messages are saved", nil
	}
	if s.config.GetBool(sqliteconfig.CJKFTS) {
		var trigram int64
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages_fts_trigram_docsize`).Scan(&trigram); err != nil {
			return "the search index for East Asian text is missing", nil
		}
	}
	return "", nil
}

// Free bytes of the file system the path is on
func freeSpace(path string) (int64, bool) {
	var stat syscall.Statfs_t
	if syscall.Statfs(path, &stat) != nil {
		return 0, false
	}
	return int64(stat.Bavail) * int64(stat.Bsize), true
}

func (s *store) optimizeStorage(ctx context.Context, r *sessionsv1.MaintainRequest, progress func(string)) error {
	db, err := s.open(ctx)
	if err != nil {
		return err
	}
	drift, err := s.indexDrift(ctx, db)
	if err != nil {
		return err
	}
	if drift == "" {
		progress("✓ The search index is in good shape, so there is nothing to do.")
		return nil
	}
	if refused := refusal("optimize-storage", s.path, foreignHolders(s.path), r.GetForce()); refused != "" {
		return errors.New(refused)
	}
	current := size(s.path)
	need := current
	label := ", compacting included"
	if r.GetNoVacuum() {
		need, label = current*3/10, ""
	}
	if r.GetDryRun() {
		progress("The search index of " + s.path + " needs a rebuild, because " + drift + ".")
		progress("  The database takes " + bytesOf(current) + " now.")
		if free, ok := freeSpace(filepath.Dir(s.path)); ok {
			progress(fmt.Sprintf("  The disk has %s free, and the rebuild needs about %s%s.", bytesOf(free), bytesOf(need), label))
			if free < need {
				return errors.New("The disk lacks the space to finish safely. Free some space, or add --no-vacuum, which rebuilds the index without " +
					"compacting the database afterwards.")
			}
		}
		if current > 500<<20 {
			progress("  The database is large, so the rebuild may take a while.")
		}
		return nil
	}
	// One process rebuilds at a time: a server mending the index in the background holds the same lock
	err = sqlitex.Locked(s.path+".fts_rebuild.lock", 5*time.Second, func(locked bool) error {
		if !locked {
			return errors.New("Another icebath is rebuilding the search index right now. Run this again once it finishes.")
		}
		stale, running, err := s.unmended(ctx, db)
		if err != nil {
			return err
		}
		// A rebuild left unfinished goes on from where it stopped; anything else starts over
		if stale || !running {
			progress("  Rebuilding the search index…")
			if err := s.reindex(ctx); err != nil {
				return err
			}
		} else {
			progress("  Going on with the rebuild that was interrupted…")
		}
		last := time.Time{}
		return s.fill(ctx, false, func(label string, indexed, total int64) {
			if time.Since(last) >= time.Second || indexed >= total {
				last = time.Now()
				progress(filled(label, indexed, total))
			}
		})
	})
	if err != nil {
		return fmt.Errorf("The search index could not be rebuilt: %s\nNo data was lost. Run the command again to go on.", strs.Sentence(err.Error()))
	}
	if !r.GetNoVacuum() {
		started := time.Now()
		progress(fmt.Sprintf("  Compacting the database, from %s to about %s…", bytesOf(current), bytesOf(current*7/10)))
		if err := s.vacuum(ctx); err != nil {
			return fmt.Errorf("The database could not be compacted: %s\nNo data was lost. Run the command again to go on.", strs.Sentence(err.Error()))
		}
		progress(fmt.Sprintf("  Compacted the database to %s in %s.", bytesOf(size(s.path)), strs.Span(time.Since(started).Round(time.Second))))
	}
	progress("✓ Rebuilt the search index.")
	progress(fmt.Sprintf("  The database went from %s to %s.", bytesOf(current), bytesOf(size(s.path))))
	if r.GetNoVacuum() {
		progress("  The database was not compacted. Run icebath sessions optimize later to give the freed space back to the disk.")
	}
	return nil
}

func (s *store) cleanMarkers(ctx context.Context, r *sessionsv1.MaintainRequest, progress func(string)) error {
	db, err := s.open(ctx)
	if err != nil {
		return err
	}
	if r.GetDryRun() {
		progress("Looking for replies saved as a bare tool marker, without changing anything…")
	} else {
		progress("Looking for replies saved as a bare tool marker…")
	}
	find := func(q querier) ([]int64, error) {
		rows, err := q.QueryContext(ctx, `SELECT id, COALESCE(content, '') FROM messages WHERE role = 'assistant' AND tool_calls IS NOT NULL AND tool_calls <> ''`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			var content string
			if err := rows.Scan(&id, &content); err != nil {
				return nil, err
			}
			if staleMarker.MatchString(strings.TrimSpace(content)) {
				ids = append(ids, id)
			}
		}
		return ids, rows.Err()
	}
	ids, err := find(db)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		progress("✓ No reply is saved as a bare tool marker, so there is nothing to clean.")
		return nil
	}
	if r.GetDryRun() {
		shown := make([]string, len(ids))
		for i, id := range ids {
			shown[i] = strconv.FormatInt(id, 10)
		}
		progress("Would clear the text of " + strs.Count(len(ids), "reply") + ", messages " + strings.Join(shown, ", ") + ".")
		return nil
	}
	if r.GetBackup() {
		backup := fmt.Sprintf("%s.pre-clean-markers-backup-%s", s.path, time.Now().Format("20060102_150405"))
		if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, backup); err != nil {
			return err
		}
		progress("  Backed up the database to " + backup + ".")
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		found, err := find(tx)
		if err != nil {
			return err
		}
		ids = found
		for _, id := range found {
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET content = '' WHERE id = ?`, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	progress("✓ Cleared the text of " + strs.Count(len(ids), "reply") + ".")
	return nil
}

// Whether the file opens and works cleanly, and why not: its integrity, its tables, its search indexes read and written
func probe(ctx context.Context, path string) (bool, string) {
	db, err := sql.Open("sqlite", "file:"+path+"?"+url.Values{"_pragma": {"busy_timeout(5000)"}, "_txlock": {"immediate"}}.Encode())
	if err != nil {
		return false, err.Error()
	}
	defer db.Close()
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return false, err.Error()
	}
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return false, err.Error()
	}
	problems, err := collect(rows)
	if err != nil {
		return false, err.Error()
	}
	if len(problems) != 1 || problems[0] != "ok" {
		return false, strings.Join(problems[:min(3, len(problems))], "; ")
	}
	var count int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil {
		return false, err.Error()
	}
	for _, table := range []string{"messages_fts", "messages_fts_trigram"} {
		var one int
		err := db.QueryRowContext(ctx, `SELECT 1 FROM `+table+` WHERE `+table+` MATCH '""' LIMIT 1`).Scan(&one)
		if err != nil && !errors.Is(err, sql.ErrNoRows) && !strings.Contains(err.Error(), "no such table") && !strings.Contains(err.Error(), "no such column") {
			return false, fmt.Sprintf("reading the search index %s failed: %v", table, err)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, "writing to the search index failed: " + err.Error()
	}
	defer tx.Rollback()
	id := fmt.Sprintf("_icebath_fts_health_probe_%d", time.Now().UnixNano())
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, profile, source, started_at) VALUES (?, '', '_health_probe', ?)`, id, now()); err != nil {
		return false, "writing to the search index failed: " + err.Error()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages (session_id, role, content, timestamp, message_uid) VALUES (?, 'user', '_fts_health_probe', ?, ?)`,
		id, now(), id); err != nil {
		return false, "writing to the search index failed: " + err.Error()
	}
	return true, ""
}

// What bringing the file into the layout of this version takes, read without changing it: a *sqlitex.Mismatch when it is of another layout
func inspect(ctx context.Context, path string) (sqlitex.Plan, error) {
	want, err := declare(ctx)
	if err != nil {
		return sqlitex.Plan{}, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return sqlitex.Plan{}, err
	}
	defer db.Close()
	return sqlitex.Inspect(ctx, db, want)
}

// What a plan changes, in words; empty when it changes nothing
func changes(plan sqlitex.Plan) string {
	var why []string
	for _, table := range plan.Missing {
		why = append(why, "the table "+table+" is missing")
	}
	for _, column := range plan.Added {
		table, name, _ := strings.Cut(column, ".")
		why = append(why, "the column "+name+" of the table "+table+" is missing")
	}
	for _, o := range plan.Dropped {
		kind := o.Type
		if kind == sqlitex.Virtual {
			kind = "virtual table"
		}
		why = append(why, "the "+kind+" "+o.Name+" is not as this version of icebath makes it")
	}
	return strings.Join(why, ", ")
}

func (s *store) repair(ctx context.Context, r *sessionsv1.MaintainRequest, progress func(string)) error {
	path := cmp.Or(r.GetPath(), s.config.GetString(sqliteconfig.Path))
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		progress("There is no session database at " + path + ", so there is nothing to repair.")
		return nil
	}
	checkOnly := "✗ " + path + " does not open cleanly. Run icebath sessions repair without --check-only to repair it."
	// A file of another layout is no damage to mend: it is set aside whole and a new database started, as opening it does
	plan, layout := inspect(ctx, path)
	if mismatch, other := errors.AsType[*sqlitex.Mismatch](layout); other {
		progress("✗ " + path + " does not open, because it was made by another version of icebath: " + strings.Join(mismatch.Reasons, ", ") + ".")
		if r.GetCheckOnly() {
			return errors.New(checkOnly)
		}
		return s.setAside(ctx, path, progress)
	}
	healthy, reason := probe(ctx, path)
	if healthy {
		if layout != nil {
			return fmt.Errorf("✗ icebath could not check the layout of the session database: %s", strs.Sentence(layout.Error()))
		}
		why := changes(plan)
		if why == "" {
			progress("✓ " + path + " opens cleanly, so it needs no repair.")
			return nil
		}
		progress("✗ " + path + " does not open cleanly, because " + why + ".")
		if r.GetCheckOnly() {
			return errors.New(checkOnly)
		}
		return s.relayout(ctx, path, r.GetBackup(), progress)
	}
	progress("✗ " + path + " does not open cleanly: " + strs.Sentence(reason))
	if r.GetCheckOnly() {
		return errors.New(checkOnly)
	}
	if r.GetBackup() {
		progress("Repairing it, after making a backup…")
	} else {
		progress("Repairing it…")
	}
	if path == s.config.GetString(sqliteconfig.Path) {
		// Copying the file opens it, and closing that would drop this process's locks on it
		s.Close()
	}
	backup, strategy, err := s.surgery(ctx, path, r.GetBackup())
	if backup != "" {
		progress("  Backed up the database to " + backup + ".")
	}
	if err != nil {
		lines := []string{"✗ The repair failed: " + strs.Sentence(err.Error())}
		source := path
		if backup != "" {
			lines = append(lines, "  The backup is kept at "+backup+".")
			source = backup
		}
		lines = append(lines, "  Keep both the database and the backup, and do not delete them.", "",
			"  Next, check what can be rescued, which never changes the database:",
			"    icebath sessions recover --source "+source+" \\", "        --inspect-only",
			"  If it says the data can be rescued, rescue it into a new database,", "  which leaves the one in use as it is:",
			"    icebath sessions recover --source "+source+" \\", "        --output recovered-state.db")
		return errors.New(strings.Join(lines, "\n"))
	}
	progress("  " + how(strategy))
	progress(s.recovered(ctx, path))
	return nil
}

func (s *store) recovered(ctx context.Context, path string) string {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return "✓ Repaired the database."
	}
	defer db.Close()
	var count int64
	if db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&count) != nil {
		return "✓ Repaired the database."
	}
	return "✓ Repaired the database, which holds " + strs.Count(int(count), "session") + "."
}

// Brings a database that opens cleanly into the layout of this version in place, under the lock of its first open, after a backup when
// asked for
func (s *store) relayout(ctx context.Context, path string, backup bool, progress func(string)) error {
	progress("Repairing it…")
	err := s.reopen(ctx, path, func() error {
		if !backup {
			return nil
		}
		kept := fmt.Sprintf("%s.pre-repair-backup-%s", path, time.Now().Format("20060102_150405"))
		db, err := sql.Open("sqlite", "file:"+path+"?"+url.Values{"_pragma": {"busy_timeout(5000)"}}.Encode())
		if err != nil {
			return err
		}
		defer db.Close()
		// A copy SQLite writes from one read holds the database as it was at one moment, writes of other processes aside
		if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, kept); err != nil {
			return fmt.Errorf("the backup could not be made, so the repair stopped rather than change the only copy of the database: %w", err)
		}
		progress("  Backed up the database to " + kept + ".")
		return nil
	})
	if err != nil {
		return fmt.Errorf("✗ The repair failed: %s", strs.Sentence(err.Error()))
	}
	progress("  " + how("reconcile_schema"))
	progress(s.recovered(ctx, path))
	return nil
}

// Moves a database of another layout aside, under the lock of its first open, and starts a new one in its place
func (s *store) setAside(ctx context.Context, path string, progress func(string)) error {
	err := s.reopen(ctx, path, func() error {
		backup, err := sqlitex.Aside(path, sqlitex.OtherVersion)
		if err != nil {
			return fmt.Errorf("the database could not be moved aside: %w", err)
		}
		progress("  Moved the database to " + backup + ". To see which sessions can be brought back, run icebath sessions recover --source " +
			backup + " --inspect-only.")
		return nil
	})
	if err != nil {
		return fmt.Errorf("✗ The repair failed: %s", strs.Sentence(err.Error()))
	}
	progress("✓ Started a new session database at " + path + ".")
	return nil
}

// Opens the file at the path afresh under the lock of its first open, after the function readies it, the store letting go of it first
// when it is the one in use; the store opens it again when it is next asked for
func (s *store) reopen(ctx context.Context, path string, before func() error) error {
	if path == s.config.GetString(sqliteconfig.Path) {
		s.Close()
	}
	return sqlitex.FirstOpen(path, 5*time.Second, func(locked bool) error {
		if !locked {
			return errors.New("another icebath is opening the session database right now. Run the repair again in a moment")
		}
		if err := before(); err != nil {
			return err
		}
		// A store of its own opens it, so the one in use keeps its state for its own file
		db, err := (&store{config: s.config, registry: s.registry}).connect(ctx, path)
		if err != nil {
			return err
		}
		return db.Close()
	})
}

// Failed repairs of the file as it is now, by a fingerprint of its head and tail
type attempts struct {
	Fingerprint string `json:"fingerprint"`
	Failed      int    `json:"failed_attempts"`
	Last        string `json:"last_attempt"`
}

func fingerprint(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	hash := sha256.New()
	head := make([]byte, 64<<10)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	for _, span := range [][2]int{{24, 28}, {92, 96}} {
		for i := span[0]; i < span[1] && i < len(head); i++ {
			head[i] = 0
		}
	}
	hash.Write(head)
	if info.Size() > 64<<10 {
		tail := make([]byte, 64<<10)
		if n, err := f.ReadAt(tail, max(info.Size()-64<<10, 0)); err == nil || n > 0 {
			hash.Write(tail[:n])
		}
	}
	return fmt.Sprintf("%d:%s", info.Size(), hex.EncodeToString(hash.Sum(nil))[:32])
}

// Repairs a damaged file: a backup first, then each strategy on a scratch copy until one leaves it working, which then takes the file's place
func (s *store) surgery(ctx context.Context, path string, keepBackup bool) (string, string, error) {
	ledger := path + ".repair-attempts.json"
	print := fingerprint(path)
	var record attempts
	if data, err := os.ReadFile(ledger); err == nil && json.Unmarshal(data, &record) == nil && record.Fingerprint == print && record.Failed >= repairBudget {
		return "", "", fmt.Errorf("the automatic repair already failed %s on this file, so the damage is deeper than it can mend.\n"+
			"  Bring back a backup, or rescue what you can with icebath sessions recover --source %s --inspect-only,\n"+
			"  then, if it says the data can be rescued, icebath sessions recover --source %s --output recovered-state.db.\n"+
			"  To try the automatic repair again anyway, delete %s.", strs.Count(record.Failed, "time"), path, path, filepath.Base(ledger))
	}
	var backup, strategy string
	err := sqlitex.Locked(path+".repair.lock", 120*time.Second, func(locked bool) error {
		if !locked {
			if healthy, _ := probe(ctx, path); healthy {
				strategy = "repaired_by_other_process"
				return nil
			}
			return errors.New("another icebath is repairing the session database right now, so this repair stopped. Run it again once the other " +
				"finishes")
		}
		if holders := foreignHolders(path); len(holders) > 0 {
			return errors.New("another process is writing to the session database, and repairing it now could damage it. Stop every other " +
				"icebath, then try again")
		}
		s.Close()
		if healthy, _ := probe(ctx, path); healthy {
			strategy = "already_healthy"
			return nil
		}
		if keepBackup {
			var err error
			if backup, err = copyBundle(path, fmt.Sprintf("%s.malformed-backup-%s", path, time.Now().Format("20060102_150405"))); err != nil {
				return fmt.Errorf("the backup could not be made, so the repair stopped rather than change the only copy of the database: %w", err)
			}
		}
		scratch := path + ".repair-scratch"
		removeBundle(scratch)
		defer removeBundle(scratch)
		if _, err := copyBundle(path, scratch); err != nil {
			return fmt.Errorf("a working copy of %s could not be made: %w", path, err)
		}
		var failure error
		for _, attempt := range []struct {
			name string
			run  func(db *sql.DB) error
		}{
			{"rebuild_fts", func(db *sql.DB) error {
				for _, table := range []string{"messages_fts", "messages_fts_trigram"} {
					if _, err := db.ExecContext(ctx, `INSERT INTO `+table+`(`+table+`) VALUES ('rebuild')`); err != nil && !strings.Contains(err.Error(), "no such table") {
						return err
					}
				}
				return nil
			}},
			{"reindex_btree", func(db *sql.DB) error { _, err := db.ExecContext(ctx, "REINDEX"); return err }},
			{"dedup_schema", func(db *sql.DB) error {
				if _, err := db.ExecContext(ctx, "PRAGMA writable_schema=ON"); err != nil {
					return err
				}
				if _, err := db.ExecContext(ctx, `DELETE FROM sqlite_master WHERE rowid NOT IN (SELECT MIN(rowid) FROM sqlite_master GROUP BY type, name)`); err != nil {
					return err
				}
				var version int64
				if err := db.QueryRowContext(ctx, "PRAGMA schema_version").Scan(&version); err != nil {
					return err
				}
				_, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA schema_version=%d", (version+1)%(1<<31)))
				return err
			}},
			{"drop_fts_rebuild", func(db *sql.DB) error {
				if _, err := db.ExecContext(ctx, "PRAGMA writable_schema=ON"); err != nil {
					return err
				}
				if _, err := db.ExecContext(ctx, `DELETE FROM sqlite_master WHERE name LIKE 'messages_fts%'`); err != nil {
					return err
				}
				_, err := db.ExecContext(ctx, "VACUUM")
				return err
			}},
		} {
			db, err := sql.Open("sqlite", "file:"+scratch)
			if err != nil {
				return err
			}
			err = attempt.run(db)
			db.Close()
			if err == nil {
				if healthy, why := probe(ctx, scratch); healthy {
					strategy = attempt.name
					break
				} else {
					err = errors.New(why)
				}
			}
			failure = err
		}
		if strategy == "" {
			return failure
		}
		for _, sidecar := range []string{"-wal", "-shm", "-journal"} {
			os.Remove(path + sidecar)
		}
		if err := os.Rename(scratch, path); err != nil {
			return fmt.Errorf("the repaired copy could not take the place of the database: %w", err)
		}
		return nil
	})
	if err != nil {
		record = attempts{Fingerprint: print, Failed: record.Failed + 1, Last: time.Now().Format("2006-01-02T15:04:05")}
		if record.Fingerprint != print {
			record.Failed = 1
		}
		if data, jsonErr := json.Marshal(record); jsonErr == nil {
			os.WriteFile(ledger, data, 0o600)
		}
		return backup, "", err
	}
	os.Remove(ledger)
	return backup, strategy, nil
}

// Copies the database and its journals to the target, the main file last; returns the target
func copyBundle(source, target string) (string, error) {
	for _, sidecar := range []string{"-wal", "-shm", "-journal", ""} {
		in, err := os.Open(source + sidecar)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		out, err := os.OpenFile(target+sidecar, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			in.Close()
			return "", err
		}
		_, err = io.Copy(out, in)
		in.Close()
		if syncErr := out.Sync(); err == nil {
			err = syncErr
		}
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return "", err
		}
	}
	return target, nil
}

func removeBundle(path string) {
	for _, sidecar := range []string{"", "-wal", "-shm", "-journal"} {
		os.Remove(path + sidecar)
	}
}

func (s *store) journalMode(ctx context.Context, r *sessionsv1.MaintainRequest, progress func(string)) error {
	path := cmp.Or(r.GetPath(), s.config.GetString(sqliteconfig.Path))
	target := strings.ToLower(r.GetMode())
	if target != sqliteconfig.WAL && target != sqliteconfig.Delete {
		return errors.New("The journal mode must be delete or wal.")
	}
	if path == s.config.GetString(sqliteconfig.Path) {
		// Reading the header opens the file, and closing that would drop this process's locks on it
		s.Close()
	}
	current := headerMode(path)
	if current == target {
		progress(fmt.Sprintf("✓ The journal mode of %s is already %s.", path, target))
		return nil
	}
	if target == sqliteconfig.WAL && crossVM(filepath.Dir(path)) {
		return fmt.Errorf("✗ The journal mode of %s stays as it is, because the file is on a file system shared with a virtual machine, virtiofs "+
			"or 9p, where wal damages data without warning.", path)
	}
	holders := foreignHolders(path)
	if r.GetForce() {
		kept := holders[:0]
		for _, h := range holders {
			if h.pid > 0 {
				kept = append(kept, h)
			}
		}
		holders = kept
	}
	if len(holders) > 0 {
		lines := []string{fmt.Sprintf("✗ The journal mode of %s stays as it is, because another process is using the file.", path)}
		for _, h := range holders {
			if h.pid <= 0 {
				lines = append(lines, "  icebath can't tell whether the file is in use: "+strs.Sentence(h.target)+" Stop every icebath, then pass --force.")
				continue
			}
			lines = append(lines, fmt.Sprintf("  Process %d, %s, holds %s.", h.pid, commandLine(h.pid), h.target))
		}
		return errors.New(strings.Join(append(lines, "Stop them first by quitting the other icebath windows and servers, then run the command again."), "\n"))
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+url.Values{"_pragma": {"busy_timeout(0)"}}.Encode())
	if err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return err
	}
	_, err = setMode(ctx, conn, strings.ToUpper(target))
	conn.Close()
	db.Close()
	if err != nil {
		return err
	}
	after := headerMode(path)
	if after != target {
		return fmt.Errorf("✗ The journal mode of %s is still %s after the switch.", path, after)
	}
	progress(fmt.Sprintf("✓ The journal mode of %s changed from %s to %s, and the file confirms it.", path, current, target))
	if path == s.config.GetString(sqliteconfig.Path) && s.config.GetString(sqliteconfig.JournalMode) != target {
		progress(fmt.Sprintf("  ⚠ Journal mode in /settings under Session storage › SQLite is %s, so icebath switches the file back the next time it "+
			"opens it. Set it to %s to keep this.", s.config.GetString(sqliteconfig.JournalMode), target))
	}
	return nil
}

// Journal mode the file's header records, read without SQLite
func headerMode(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "none, because the file is missing"
	}
	defer f.Close()
	header := make([]byte, 20)
	if n, _ := io.ReadFull(f, header); n < 20 || string(header[:16]) != "SQLite format 3\x00" {
		return "none, because the file is not a database"
	}
	switch {
	case header[18] == 1 && header[19] == 1:
		return sqliteconfig.Delete
	case header[18] == 2 && header[19] == 2:
		return sqliteconfig.WAL
	}
	return fmt.Sprintf("unknown, with the marks %d and %d", header[18], header[19])
}
