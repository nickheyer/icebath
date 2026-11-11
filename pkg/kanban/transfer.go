package kanban

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pysqlite"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	ArchiveFormat        = "hermes-kanban-board"
	ArchiveFormatVersion = 1
)

var (
	dispatchableStatuses = []any{"ready", "running", "todo", "scheduled"}
	countedTables        = []string{"tasks", "task_links", "task_comments", "task_events", "task_runs", "task_attachments"}
)

// FileNotFoundError is the Python FileNotFoundError a missing board database or archive raises
type FileNotFoundError struct{ Msg string }

func (e *FileNotFoundError) Error() string { return e.Msg }

// Comma separated SQL parameter marks, one per item
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// Raw driver connection of a tracked sqlite connection for the online backup API
func rawConnOf(conn state.SQLiteConn) (pysqlite.RawConn, error) {
	if tracked, ok := conn.(*state.TrackedConn); ok {
		conn = tracked.SQLiteConn
	}
	raw, ok := conn.(pysqlite.RawConn)
	if !ok {
		return nil, &TypeError{"connection does not expose its sqlite handle"}
	}
	return raw, nil
}

// Consistent copy of a live database through the online backup API
func snapshotDB(source, target string) error {
	src, err := state.ConnectTracked(source)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := state.ConnectTracked(target)
	if err != nil {
		return err
	}
	defer dst.Close()
	srcRaw, err := rawConnOf(src)
	if err != nil {
		return err
	}
	dstRaw, err := rawConnOf(dst)
	if err != nil {
		return err
	}
	return pysqlite.Backup(srcRaw, dstRaw, -1, nil, 250*time.Millisecond)
}

// Strips claims, pids, sessions and gateway subscriptions from a board copy, the caller owning the transaction
func scrubLocalState(ctx context.Context, conn state.SQLiteConn) error {
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{"DELETE FROM kanban_notify_subs", nil},
		{`
        UPDATE tasks
           SET claim_lock           = NULL,
               claim_expires        = NULL,
               worker_pid           = NULL,
               current_run_id       = NULL,
               last_heartbeat_at    = NULL,
               session_id           = NULL,
               project_id           = NULL,
               consecutive_failures = 0,
               last_failure_error   = NULL
        `, nil},
		{"UPDATE tasks SET status = 'ready' WHERE status = 'running'", nil},
		{`
        UPDATE task_runs
           SET status            = 'released',
               outcome           = COALESCE(outcome, 'reclaimed'),
               ended_at          = COALESCE(ended_at, ?),
               last_heartbeat_at = NULL
         WHERE status = 'running'
        `, []any{time.Now().Unix()}},
		{"UPDATE task_runs SET claim_lock = NULL, worker_pid = NULL", nil},
	} {
		if _, err := conn.ExecContext(ctx, stmt.sql, stmt.args...); err != nil {
			return err
		}
	}
	return nil
}

// Writes a mapping as two space indented UTF-8 JSON with a trailing newline
func writeJSONFile(path string, payload any) error {
	text, err := pyjson.Dumps(payload, pyjson.Indent(2), pyjson.EnsureASCII(false))
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(text+"\n"), 0o666)
}

// Row count of every exported table in order
func countTableRows(ctx context.Context, conn state.SQLiteConn) (*pyjson.Object, error) {
	out := pyjson.NewObject()
	for _, t := range countedTables {
		row, err := QueryOne(ctx, conn, "SELECT COUNT(*) AS n FROM "+t)
		if err != nil {
			return nil, err
		}
		n, err := pyint.Of(row.Get("n"))
		if err != nil {
			return nil, err
		}
		out.Set(t, n.Int64())
	}
	return out, nil
}

// Exports a board to a tar.gz with a scrubbed database snapshot, optional attachments and logs, returning a summary
func ExportBoard(ctx context.Context, board *string, outputPath string, includeAttachments, includeLogs bool) (*pyjson.Object, error) {
	slug, err := normalizeBoardSlug(board)
	if err != nil {
		return nil, err
	}
	if slug == "" {
		slug = GetCurrentBoard(ctx)
	}
	if !BoardExists(&slug) {
		return nil, &ValueError{fmt.Sprintf("board %s does not exist", pyrepr.Repr(slug))}
	}
	dbPath, err := KanbanDBPath(ctx, &slug)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil, &FileNotFoundError{fmt.Sprintf("board %s has no database at %s", pyrepr.Repr(slug), dbPath)}
	}
	base := strings.TrimSuffix(strings.TrimSuffix(pypath.PathStr(pypath.Expanduser(outputPath)), ".tar.gz"), ".tgz")
	if err := os.MkdirAll(pypath.PathParent(base), 0o777); err != nil {
		return nil, err
	}
	tmpdir, err := os.MkdirTemp("", "")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpdir)
	staged := filepath.Join(tmpdir, slug)
	if err := os.MkdirAll(staged, 0o777); err != nil {
		return nil, err
	}
	snapshotPath := filepath.Join(staged, "kanban.db")
	if err := snapshotDB(dbPath, snapshotPath); err != nil {
		return nil, err
	}
	snapshot, err := state.ConnectTracked(snapshotPath)
	if err != nil {
		return nil, err
	}
	counts, err := func() (*pyjson.Object, error) {
		defer snapshot.Close()
		if _, err := snapshot.ExecContext(ctx, "BEGIN"); err != nil {
			return nil, err
		}
		if err := scrubLocalState(ctx, snapshot); err != nil {
			_, _ = snapshot.ExecContext(ctx, "ROLLBACK")
			return nil, err
		}
		if _, err := snapshot.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, err
		}
		return countTableRows(ctx, snapshot)
	}()
	if err != nil {
		return nil, err
	}
	meta, err := ReadBoardMetadata(&slug)
	if err != nil {
		return nil, err
	}
	meta.Delete("db_path")
	meta.Set("default_workdir", nil)
	meta.Set("project_id", nil)
	if err := writeJSONFile(filepath.Join(staged, "board.json"), meta); err != nil {
		return nil, err
	}
	attachments, logs := 0, 0
	if includeAttachments {
		root, err := AttachmentsRoot(ctx, &slug)
		if err != nil {
			return nil, err
		}
		if attachments, err = pyfs.CopyRegularFiles(root, filepath.Join(staged, "attachments")); err != nil {
			return nil, err
		}
	}
	if includeLogs {
		root, err := WorkerLogsDir(ctx, &slug)
		if err != nil {
			return nil, err
		}
		if logs, err = pyfs.CopyRegularFiles(root, filepath.Join(staged, "logs")); err != nil {
			return nil, err
		}
	}
	info, err := updaterecord.GetVersionInfo()
	if err != nil {
		return nil, err
	}
	boardName := any(slug)
	if v, _ := meta.Get("name"); pyrepr.Truthy(v) {
		boardName = v
	}
	allCounts := counts.Copy()
	allCounts.Set("attachment_files", attachments)
	allCounts.Set("log_files", logs)
	manifest := pyjson.ObjectOf(
		"format", ArchiveFormat, "format_version", ArchiveFormatVersion, "board", slug, "board_name", boardName,
		"exported_at", time.Now().Unix(), "hermes_version", info.BaseVersion,
		"includes", pyjson.ObjectOf("attachments", includeAttachments, "logs", includeLogs),
		"counts", allCounts,
	)
	if err := writeJSONFile(filepath.Join(staged, "manifest.json"), manifest); err != nil {
		return nil, err
	}
	archive, err := pyfs.MakeTargz(base, tmpdir, slug)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(archive)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("board", slug, "archive", archive, "size", st.Size(), "counts", allCounts), nil
}

// Preferred slug or the first free numbered variant of it
func availableSlug(preferred string) string {
	if !BoardExists(&preferred) {
		return preferred
	}
	stem := strings.TrimRight(pystr.Slice(preferred, 0, 58), "-_")
	if stem == "" {
		stem = "board"
	}
	n := 2
	for {
		candidate := fmt.Sprintf("%s-%d", stem, n)
		if !BoardExists(&candidate) {
			return candidate
		}
		n++
	}
}

// Validated manifest of an extracted archive root
func readManifest(root string) (*pyjson.Object, error) {
	path := filepath.Join(root, "manifest.json")
	if _, err := os.Stat(path); err != nil {
		return nil, &ValueError{"archive is not an Icebath kanban board export (no manifest.json)"}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoded, err := pyjson.Loads(pystr.DecodeUTF8SigReplace(raw))
	if err != nil {
		var de *pyjson.DecodeError
		if errors.As(err, &de) {
			return nil, &ValueError{"archive manifest is not valid JSON: " + err.Error()}
		}
		return nil, err
	}
	manifest, ok := decoded.(*pyjson.Object)
	var format any
	if ok {
		format, _ = manifest.Get("format")
	}
	if !ok || format != ArchiveFormat {
		var shown any
		if ok {
			shown = format
		}
		return nil, &ValueError{"archive is not an Icebath kanban board export (format=" + pyrepr.Repr(shown) + ")"}
	}
	version, _ := manifest.Get("format_version")
	n, isInt := pyint.Instance(version)
	if !isInt || n.Cmp(big.NewInt(ArchiveFormatVersion)) > 0 {
		return nil, &ValueError{fmt.Sprintf("archive format version %s is newer than this Icebath understands (max %d) — update Icebath and retry",
			pyrepr.Repr(version), ArchiveFormatVersion)}
	}
	return manifest, nil
}

// board.json of an archive, empty when missing, unreadable or not a mapping
func readArchiveBoardMetadata(path string) *pyjson.Object {
	raw, err := os.ReadFile(path)
	if err != nil {
		return pyjson.NewObject()
	}
	decoded, err := pyjson.Loads(pystr.DecodeUTF8SigReplace(raw))
	if err != nil {
		return pyjson.NewObject()
	}
	if obj, ok := decoded.(*pyjson.Object); ok {
		return obj
	}
	return pyjson.NewObject()
}

// Re-anchors imported rows to this machine, returning the restored attachment and parked task counts with warnings
func relocateImportedRows(ctx context.Context, conn state.SQLiteConn, slug string) (int, int, []string, error) {
	var warnings []string
	now := time.Now().Unix()
	attachmentsDir, err := AttachmentsRoot(ctx, &slug)
	if err != nil {
		return 0, 0, nil, err
	}
	rehomed, parkedCount := 0, 0
	err = WriteTxn(ctx, conn, false, func() error {
		if err := scrubLocalState(ctx, conn); err != nil {
			return err
		}
		rows, err := QueryRows(ctx, conn, "SELECT id, task_id, stored_path FROM task_attachments")
		if err != nil {
			return err
		}
		dropped := 0
		for _, row := range rows {
			landed := filepath.Join(attachmentsDir, pyrepr.Str(row.Get("task_id")), pypath.PathName(pyrepr.Str(row.Get("stored_path"))))
			if st, err := os.Stat(landed); err == nil && st.Mode().IsRegular() {
				if _, err := conn.ExecContext(ctx, "UPDATE task_attachments SET stored_path = ? WHERE id = ?", landed, row.Get("id")); err != nil {
					return err
				}
				rehomed++
			} else {
				if _, err := conn.ExecContext(ctx, "DELETE FROM task_attachments WHERE id = ?", row.Get("id")); err != nil {
					return err
				}
				dropped++
			}
		}
		if dropped > 0 {
			warnings = append(warnings, fmt.Sprintf("%d attachment record(s) dropped — the files were not in the archive", dropped))
		}
		parkedRows, err := QueryRows(ctx, conn, "SELECT id FROM tasks WHERE workspace_kind IN ('dir', 'worktree') "+
			"AND status IN ("+placeholders(len(dispatchableStatuses))+")", dispatchableStatuses...)
		if err != nil {
			return err
		}
		parked := map[string]bool{}
		parkedIDs := make([]any, 0, len(parkedRows))
		for _, r := range parkedRows {
			id := pyrepr.Str(r.Get("id"))
			parked[id] = true
			parkedIDs = append(parkedIDs, id)
		}
		parkedCount = len(parkedIDs)
		if _, err := conn.ExecContext(ctx, "UPDATE tasks SET workspace_path = NULL, branch_name = NULL"); err != nil {
			return err
		}
		if len(parkedIDs) > 0 {
			if _, err := conn.ExecContext(ctx, "UPDATE tasks SET status = 'triage' WHERE id IN ("+placeholders(len(parkedIDs))+")", parkedIDs...); err != nil {
				return err
			}
			warnings = append(warnings, fmt.Sprintf("%d task(s) moved to triage — their workspace was a directory or git "+
				"worktree on the exporting machine and needs to be pointed somewhere on this one", len(parkedIDs)))
		}
		all, err := QueryRows(ctx, conn, "SELECT id FROM tasks")
		if err != nil {
			return err
		}
		for _, r := range all {
			id := pyrepr.Str(r.Get("id"))
			body, err := pyjson.Dumps(pyjson.ObjectOf("board", slug, "parked", parked[id]), pyjson.EnsureASCII(false))
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, "INSERT INTO task_events (task_id, run_id, kind, payload, created_at) "+
				"VALUES (?, NULL, 'imported', ?, ?)", id, body, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, 0, nil, err
	}
	return rehomed, parkedCount, warnings, nil
}

// shutil.move of src to dst, into dst when it is a directory, copying across filesystems
func shutilMove(src, dst string) error {
	if st, err := os.Stat(dst); err == nil && st.IsDir() {
		dst = filepath.Join(dst, filepath.Base(strings.TrimRight(src, string(os.PathSeparator))))
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	st, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if st.IsDir() {
		if err := pyshutil.CopyTree(src, dst, true, false); err != nil {
			return err
		}
		return pyshutil.Rmtree(src)
	}
	if err := pyshutil.Copy2(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// Imports an archive as a new board, the slug suffixed when taken, returning a summary
func ImportBoard(ctx context.Context, archivePath string, slug *string, activate bool) (*pyjson.Object, error) {
	archive := pypath.PathStr(pypath.Expanduser(archivePath))
	if _, err := os.Stat(archive); err != nil {
		return nil, &FileNotFoundError{"archive not found: " + archive}
	}
	roots, err := pyfs.ArchiveRootDirs(archive)
	if err != nil {
		return nil, err
	}
	if len(roots) != 1 {
		return nil, &ValueError{"a kanban board archive must contain exactly one top-level directory"}
	}
	var archiveRoot string
	for r := range roots {
		archiveRoot = r
	}
	tmpdir, err := os.MkdirTemp("", "")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpdir)
	if err := pyfs.SafeExtractTargz(archive, tmpdir); err != nil {
		return nil, err
	}
	extracted := filepath.Join(tmpdir, archiveRoot)
	manifest, err := readManifest(extracted)
	if err != nil {
		return nil, err
	}
	stagedDB := filepath.Join(extracted, "kanban.db")
	if st, err := os.Stat(stagedDB); err != nil || !st.Mode().IsRegular() {
		return nil, &ValueError{"archive is missing kanban.db"}
	}
	var want any = archiveRoot
	if slug != nil && *slug != "" {
		want = *slug
	} else if v, _ := manifest.Get("board"); pyrepr.Truthy(v) {
		want = v
	}
	wantText := pyrepr.Str(want)
	requested, err := normalizeBoardSlug(&wantText)
	if err != nil {
		return nil, err
	}
	if requested == "" {
		return nil, &ValueError{"cannot determine a board name from the archive — pass one explicitly with --as <slug>"}
	}
	target := availableSlug(requested)
	stagedMeta := readArchiveBoardMetadata(filepath.Join(extracted, "board.json"))
	boardRoot, err := BoardDir(&target)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(boardRoot, 0o777); err != nil {
		return nil, err
	}
	if err := shutilMove(stagedDB, filepath.Join(boardRoot, "kanban.db")); err != nil {
		return nil, err
	}
	for _, tree := range []string{"attachments", "logs"} {
		src := filepath.Join(extracted, tree)
		if st, err := os.Stat(src); err == nil && st.IsDir() {
			if err := shutilMove(src, filepath.Join(boardRoot, tree)); err != nil {
				return nil, err
			}
		}
	}
	firstTruthy := func(vals ...any) string {
		for _, v := range vals {
			if pyrepr.Truthy(v) {
				return pyrepr.Str(v)
			}
		}
		return ""
	}
	metaVal := func(key string) any { v, _ := stagedMeta.Get(key); return v }
	boardName, _ := manifest.Get("board_name")
	name := firstTruthy(metaVal("name"), boardName, target)
	description, icon, color := firstTruthy(metaVal("description")), firstTruthy(metaVal("icon")), firstTruthy(metaVal("color"))
	archived := false
	if _, err := WriteBoardMetadata(ctx, &target, BoardMetadataUpdate{Name: &name, Description: &description, Icon: &icon, Color: &color, Archived: &archived}); err != nil {
		return nil, err
	}
	if _, err := InitDB(ctx, "", &target); err != nil {
		return nil, err
	}
	var rehomed, parked int
	var warnings []string
	var counts *pyjson.Object
	if err := ConnectClosing(ctx, "", &target, func(conn state.SQLiteConn) error {
		var err error
		if rehomed, parked, warnings, err = relocateImportedRows(ctx, conn, target); err != nil {
			return err
		}
		counts, err = countTableRows(ctx, conn)
		return err
	}); err != nil {
		return nil, err
	}
	if activate {
		if _, err := SetCurrentBoard(ctx, target); err != nil {
			return nil, err
		}
	}
	dbPath, err := KanbanDBPath(ctx, &target)
	if err != nil {
		return nil, err
	}
	srcBoard, _ := manifest.Get("board")
	exportedAt, _ := manifest.Get("exported_at")
	version, _ := manifest.Get("hermes_version")
	return pyjson.ObjectOf(
		"board", target, "requested_board", requested, "renamed", target != requested, "name", name,
		"path", boardRoot, "db_path", dbPath,
		"source", pyjson.ObjectOf("board", srcBoard, "exported_at", exportedAt, "hermes_version", version),
		"counts", counts, "attachments_restored", rehomed, "tasks_parked", parked,
		"warnings", stringsAny(warnings), "activated", activate,
	), nil
}
