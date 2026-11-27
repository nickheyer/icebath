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
	"sort"
	"strings"
	"syscall"
	"time"

	sqliteconfig "github.com/nickheyer/icebath/internal/plugins/defaults/sessions/sqlite/config"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
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

func megabytes(n int64) float64 {
	return float64(n) / (1 << 20)
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
	progress("Optimizing session store (FTS merge + VACUUM)…")
	merged := s.optimizeIndexes(ctx, db)
	if err := s.vacuum(ctx); err != nil {
		return fmt.Errorf("optimization failed: %w", err)
	}
	after := size(s.path)
	var pages, pageSize int64
	if db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages) == nil && db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize) == nil {
		after = pages * pageSize
	}
	progress(fmt.Sprintf("Optimized %d FTS index(es).", merged))
	change := fmt.Sprintf("reclaimed %.1f MB", megabytes(before-after))
	if after > before {
		change = fmt.Sprintf("grew by %.1f MB", megabytes(after-before))
	}
	progress(fmt.Sprintf("Database size: %.1f MB -> %.1f MB (%s)", megabytes(before), megabytes(after), change))
	return nil
}

// Why the search indexes need a rebuild, empty when they are in step
func (s *store) indexDrift(ctx context.Context, db *sql.DB) (string, error) {
	if _, stale, err := meta(ctx, db, "fts_stale"); err != nil || stale {
		return "the search index was detached after damage", err
	}
	if _, _, running, err := progressOf(ctx, db, wordBackfill); err != nil || running {
		return "a previous rebuild was interrupted", err
	}
	if _, _, running, err := progressOf(ctx, db, trigramBackfill); err != nil || running {
		return "a previous rebuild of the CJK search index was interrupted", err
	}
	var messages, indexed int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&messages); err != nil {
		return "", err
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages_fts_docsize`).Scan(&indexed); err != nil {
		return "the search index is missing", nil
	}
	if messages > 0 && indexed == 0 {
		return "the search index is empty while messages are stored", nil
	}
	if s.config.GetBool(sqliteconfig.CJKFTS) {
		var trigram int64
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages_fts_trigram_docsize`).Scan(&trigram); err != nil {
			return "the CJK search index is missing", nil
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
		progress("Search index is already on the compact layout — nothing to do.")
		return nil
	}
	if refused := refusal("optimize-storage", s.path, foreignHolders(s.path), r.GetForce()); refused != "" {
		return errors.New(refused)
	}
	current := size(s.path)
	need := current
	label := " incl. VACUUM"
	if r.GetNoVacuum() {
		need, label = current*3/10, ""
	}
	if r.GetDryRun() {
		progress("Search-index optimization for " + s.path + " (" + drift + ")")
		progress(fmt.Sprintf("  Current database size: %.1f MB", megabytes(current)))
		if free, ok := freeSpace(filepath.Dir(s.path)); ok {
			progress(fmt.Sprintf("  Free disk: %.0f MB (need ~%.0f MB to complete%s)", megabytes(free), megabytes(need), label))
			if free < need {
				return errors.New("Not enough free disk to complete safely. Free up space, or run with --no-vacuum (rebuilds the index but doesn't " +
					"reclaim space until a later VACUUM).")
			}
		}
		if current > 500<<20 {
			progress("  This is a large database; the rebuild may take a while…")
		}
		return nil
	}
	// One process rebuilds at a time: a server mending the index in the background holds the same lock
	err = withFlock(s.path+".fts_rebuild.lock", 5*time.Second, func(locked bool) error {
		if !locked {
			return errors.New("Another icebath process is rebuilding the search index right now; run this again once it finishes.")
		}
		stale, running, err := s.unmended(ctx, db)
		if err != nil {
			return err
		}
		// A rebuild left unfinished goes on from where it stopped; anything else starts over
		if stale || !running {
			progress("  Rebuilding index: …")
			if err := s.reindex(ctx); err != nil {
				return err
			}
		} else {
			progress("  Resuming the interrupted index rebuild: …")
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
		return fmt.Errorf("optimization failed: %w\nNo data was lost. Re-run to resume.", err)
	}
	if !r.GetNoVacuum() {
		started := time.Now()
		progress(fmt.Sprintf("  Compacting database (VACUUM): %.0f MB → ~%.0f MB", megabytes(current), megabytes(current)*0.7))
		if err := s.vacuum(ctx); err != nil {
			return fmt.Errorf("optimization failed: %w\nNo data was lost. Re-run to resume.", err)
		}
		elapsed := time.Since(started)
		progress(fmt.Sprintf("  Compacting database (VACUUM): %dm %02ds elapsed, %.0f MB", int(elapsed.Minutes()), int(elapsed.Seconds())%60, megabytes(size(s.path))))
	}
	progress("✓ Search index optimized.")
	progress(fmt.Sprintf("  Database size: %.1f MB -> %.1f MB", megabytes(current), megabytes(size(s.path))))
	if r.GetNoVacuum() {
		progress("  VACUUM was skipped; run `icebath sessions optimize` later to return the freed pages to the disk.")
	}
	return nil
}

func (s *store) cleanMarkers(ctx context.Context, r *sessionsv1.MaintainRequest, progress func(string)) error {
	db, err := s.open(ctx)
	if err != nil {
		return err
	}
	if r.GetDryRun() {
		progress("Dry run — scanning for stale tool-call marker rows (#78148)…")
	} else {
		progress("Scanning for stale tool-call marker rows (#78148)…")
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
		progress("✓ No affected rows found — nothing to clean.")
		return nil
	}
	if r.GetDryRun() {
		progress(fmt.Sprintf("Would clear %d row(s): ids %v", len(ids), ids))
		return nil
	}
	if r.GetBackup() {
		backup := fmt.Sprintf("%s.pre-clean-markers-backup-%s", s.path, time.Now().Format("20060102_150405"))
		if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, backup); err != nil {
			return err
		}
		progress("  backup: " + backup)
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
	progress(fmt.Sprintf("✓ Cleared %d row(s).", len(ids)))
	return nil
}

// Columns each table of the current schema has, read from a fresh database the schema is made in
func declared(ctx context.Context) (map[string][]string, error) {
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, err
	}
	return tableColumns(ctx, db)
}

func tableColumns(ctx context.Context, q querier) (map[string][]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE 'messages_fts%'`)
	if err != nil {
		return nil, err
	}
	tables, err := collect(rows)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, table := range tables {
		rows, err := q.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			return nil, err
		}
		if out[table], err = collect(rows); err != nil {
			return nil, err
		}
	}
	return out, nil
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
			return false, fmt.Sprintf("fts5 read probe failed on %s: %v", table, err)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, "fts5 write probe failed: " + err.Error()
	}
	defer tx.Rollback()
	id := fmt.Sprintf("_icebath_fts_health_probe_%d", time.Now().UnixNano())
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, profile, source, started_at) VALUES (?, '', '_health_probe', ?)`, id, now()); err != nil {
		return false, "fts5 write probe failed: " + err.Error()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages (session_id, role, content, timestamp, message_uid) VALUES (?, 'user', '_fts_health_probe', ?, ?)`,
		id, now(), id); err != nil {
		return false, "fts5 write probe failed: " + err.Error()
	}
	return true, ""
}

// Why the file's tables lack what the schema declares, empty when they hold it all
func drift(ctx context.Context, path string) (string, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return "", err
	}
	defer db.Close()
	want, err := declared(ctx)
	if err != nil {
		return "", err
	}
	have, err := tableColumns(ctx, db)
	if err != nil {
		return "", err
	}
	tables := make([]string, 0, len(want))
	for table := range want {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		columns, ok := have[table]
		if !ok {
			return "no such table: " + table, nil
		}
		for _, column := range want[table] {
			if !strings.Contains(" "+strings.Join(columns, " ")+" ", " "+column+" ") {
				return "no such column: " + table + "." + column, nil
			}
		}
	}
	return "", nil
}

func (s *store) repair(ctx context.Context, r *sessionsv1.MaintainRequest, progress func(string)) error {
	path := cmp.Or(r.GetPath(), s.config.GetString(sqliteconfig.Path))
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		progress("No session database at " + path + " (nothing to repair).")
		return nil
	}
	healthy, reason := probe(ctx, path)
	if healthy {
		why, err := drift(ctx, path)
		if err != nil {
			return fmt.Errorf("✗ Could not check session schema: %w", err)
		}
		if why == "" {
			progress("✓ " + path + " opens cleanly — no repair needed.")
			return nil
		}
		progress("✗ " + path + " does not open cleanly: " + why)
		if r.GetCheckOnly() {
			return errors.New(path + " does not open cleanly")
		}
		progress("Repairing (a backup copy is made first)…")
		if err := s.reconcile(ctx, path); err != nil {
			return fmt.Errorf("✗ Repair failed: %w", err)
		}
		progress("  strategy: reconcile_schema")
		progress(s.recovered(ctx, path))
		return nil
	}
	progress("✗ " + path + " does not open cleanly: " + reason)
	if r.GetCheckOnly() {
		return errors.New(path + " does not open cleanly")
	}
	progress("Repairing (a backup copy is made first)…")
	if path == s.config.GetString(sqliteconfig.Path) {
		// Copying the file opens it, and closing that would drop this process's locks on it
		s.Close()
	}
	backup, strategy, err := s.surgery(ctx, path, r.GetBackup())
	if backup != "" {
		progress("  backup: " + backup)
	}
	if err != nil {
		lines := []string{"✗ Repair failed: " + err.Error()}
		source := path
		if backup != "" {
			lines = append(lines, "  A backup is preserved at: "+backup)
			source = backup
		}
		lines = append(lines, "  Keep the database and the backup; do not delete them.", "",
			"  Next step — offline recovery (never modifies the source):",
			"    icebath sessions recover --source "+source+" \\", "        --inspect-only",
			"  If that reports the data is recoverable, rebuild it into", "  a NEW database (the active one is left untouched):",
			"    icebath sessions recover --source "+source+" \\", "        --output recovered-state.db")
		return errors.New(strings.Join(lines, "\n"))
	}
	progress("  strategy: " + strategy)
	progress(s.recovered(ctx, path))
	return nil
}

func (s *store) recovered(ctx context.Context, path string) string {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return "✓ Repaired."
	}
	defer db.Close()
	var count int64
	if db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&count) != nil {
		return "✓ Repaired."
	}
	return fmt.Sprintf("✓ Repaired — %d sessions recovered.", count)
}

// Adds what the schema declares and the file lacks: its tables, indexes, triggers and columns
func (s *store) reconcile(ctx context.Context, path string) error {
	s.Close()
	db, err := sql.Open("sqlite", "file:"+path+"?"+url.Values{"_pragma": {"busy_timeout(5000)"}}.Encode())
	if err != nil {
		return err
	}
	defer db.Close()
	want, err := declared(ctx)
	if err != nil {
		return err
	}
	have, err := tableColumns(ctx, db)
	if err != nil {
		return err
	}
	types, err := declaredTypes(ctx)
	if err != nil {
		return err
	}
	for table, columns := range want {
		if _, ok := have[table]; !ok {
			continue
		}
		for _, column := range columns {
			if !strings.Contains(" "+strings.Join(have[table], " ")+" ", " "+column+" ") {
				if _, err := db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column+` `+types[table+"."+column]); err != nil {
					return err
				}
			}
		}
	}
	_, err = db.ExecContext(ctx, schema)
	return err
}

// Declared type and default of each column, by table.column, for adding it to a table that lacks it
func declaredTypes(ctx context.Context) (map[string]string, error) {
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, err
	}
	columns, err := tableColumns(ctx, db)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for table := range columns {
		rows, err := db.QueryContext(ctx, `SELECT name, type, "notnull", dflt_value FROM pragma_table_info(?)`, table)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name, kind string
			var notNull int
			var fallback sql.NullString
			if err := rows.Scan(&name, &kind, &notNull, &fallback); err != nil {
				rows.Close()
				return nil, err
			}
			declaration := kind
			if fallback.Valid {
				declaration += " DEFAULT " + fallback.String
				if notNull == 1 {
					declaration += " NOT NULL"
				}
			}
			out[table+"."+name] = declaration
		}
		rows.Close()
	}
	return out, nil
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
		return "", "", fmt.Errorf("automatic repair has already failed %d times on this exact file — the corruption is beyond the schema and search "+
			"index repairs (likely b-tree page damage). Manual recovery required: restore a backup, or salvage with `icebath sessions recover --source %s "+
			"--inspect-only`, then (if it reports recoverable) `icebath sessions recover --source %s --output recovered-state.db`. Delete %s to force "+
			"another automatic attempt.", record.Failed, path, path, filepath.Base(ledger))
	}
	var backup, strategy string
	err := withFlock(path+".repair.lock", 120*time.Second, func(locked bool) error {
		if !locked {
			if healthy, _ := probe(ctx, path); healthy {
				strategy = "repaired_by_other_process"
				return nil
			}
			return errors.New("could not obtain the session database repair lock (held by another process); skipped schema surgery to avoid racing a " +
				"concurrent repairer")
		}
		if holders := foreignHolders(path); len(holders) > 0 {
			return errors.New("a live writer still holds the session database; skipped schema surgery to avoid tearing b-tree pages under a concurrent " +
				"writer. Stop every other icebath process and retry.")
		}
		s.Close()
		if healthy, _ := probe(ctx, path); healthy {
			strategy = "already_healthy"
			return nil
		}
		if keepBackup {
			var err error
			if backup, err = copyBundle(path, fmt.Sprintf("%s.malformed-backup-%s", path, time.Now().Format("20060102_150405"))); err != nil {
				return fmt.Errorf("pre-repair backup refused; aborting schema repair to avoid mutating the only copy of the damaged database: %w", err)
			}
		}
		scratch := path + ".repair-scratch"
		removeBundle(scratch)
		defer removeBundle(scratch)
		if _, err := copyBundle(path, scratch); err != nil {
			return fmt.Errorf("could not stage a complete repair snapshot of %s: %w", path, err)
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
			return fmt.Errorf("repaired snapshot could not be promoted: %w", err)
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
		progress(fmt.Sprintf("✓ %s is already journal_mode=%s.", path, target))
		return nil
	}
	if target == sqliteconfig.WAL && crossVM(filepath.Dir(path)) {
		return fmt.Errorf("✗ Refusing to change the journal mode of %s: it is on a cross-VM file system (virtiofs or 9p) where WAL shared memory "+
			"silently corrupts", path)
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
		lines := []string{fmt.Sprintf("✗ Refusing to change the journal mode of %s: another process is using it.", path)}
		for _, h := range holders {
			if h.pid <= 0 {
				lines = append(lines, "  cannot prove the database is quiet ("+h.target+"); stop every icebath process and pass --force")
				continue
			}
			lines = append(lines, fmt.Sprintf("  PID %d (%s): %s", h.pid, commandLine(h.pid), h.target))
		}
		return errors.New(strings.Join(append(lines, "Stop them first (quit the other icebath windows and servers), then re-run."), "\n"))
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
		return fmt.Errorf("✗ %s still reads journal_mode=%s after the switch", path, after)
	}
	progress(fmt.Sprintf("✓ %s: journal_mode %s → %s (header verified).", path, current, target))
	if path == s.config.GetString(sqliteconfig.Path) && s.config.GetString(sqliteconfig.JournalMode) != target {
		progress(fmt.Sprintf("  Note: the journal mode setting is %s — the next open would switch the file back. Set it to %s in /settings under "+
			"Sessions → SQLite to keep it.", s.config.GetString(sqliteconfig.JournalMode), target))
	}
	return nil
}

// Journal mode the file's header records, read without SQLite
func headerMode(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "missing"
	}
	defer f.Close()
	header := make([]byte, 20)
	if n, _ := io.ReadFull(f, header); n < 20 || string(header[:16]) != "SQLite format 3\x00" {
		return "not-a-database"
	}
	switch {
	case header[18] == 1 && header[19] == 1:
		return sqliteconfig.Delete
	case header[18] == 2 && header[19] == 2:
		return sqliteconfig.WAL
	}
	return fmt.Sprintf("unknown(%d/%d)", header[18], header[19])
}
