package kanban

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Optional string of a literal
func sp(s string) *string { return &s }

// Task id of a new task, failing the test on error
func mustCreate(t *testing.T, conn state.SQLiteConn, o CreateTaskOptions) string {
	t.Helper()
	id, err := CreateTask(context.Background(), conn, o)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Task by id, failing the test when it is missing
func mustTask(t *testing.T, conn state.SQLiteConn, id string) *Task {
	t.Helper()
	task, err := GetTask(context.Background(), conn, id)
	if err != nil || task == nil {
		t.Fatal("task missing", id, err)
	}
	return task
}

// Titles of the live tasks on a connection in sorted order
func taskTitles(t *testing.T, conn state.SQLiteConn) []string {
	t.Helper()
	tasks, err := ListTasks(context.Background(), conn, ListTasksOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, task := range tasks {
		out = append(out, task.Title)
	}
	sort.Strings(out)
	return out
}

// Events of one kind on a task
func eventsOfKind(t *testing.T, conn state.SQLiteConn, id, kind string) []*Event {
	t.Helper()
	events, err := ListEvents(context.Background(), conn, id)
	if err != nil {
		t.Fatal(err)
	}
	var out []*Event
	for _, e := range events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// Registers a capturing hook callback on the active manager for the test duration
func captureHook(t *testing.T, name string, fn plugins.HookFunc) {
	t.Helper()
	ctx := context.Background()
	mgr := plugins.GetPluginManager(ctx)
	mgr.DiscoverAndLoad(ctx, false)
	reg := plugins.NewPluginContext(&plugins.Manifest{Name: "kanban-test", Key: "kanban-test"}, mgr).RegisterHook(name, fn, false)
	t.Cleanup(func() { reg.Dispose() })
}

func TestSlugValidationAcceptsValidAndEmptyReturnsNone(t *testing.T) {
	for _, good := range []string{"default", "atm10-server", "hermes-agent", "proj_1", "a", "very-long-but-still-ok-slug-with-hyphens-and-numbers-1234"} {
		if got, err := normalizeBoardSlug(&good); err != nil || got != good {
			t.Fatal(good, got, err)
		}
	}
	for _, empty := range []*string{nil, sp(""), sp("   ")} {
		if got, err := normalizeBoardSlug(empty); err != nil || got != "" {
			t.Fatal("empty slug normalized to", got, err)
		}
	}
}

func TestPathResolutionDefaultNamedAndEnvPins(t *testing.T) {
	h := setupKanbanHome(t)
	ctx := context.Background()
	path := func(board *string) string {
		p, err := KanbanDBPath(ctx, board)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if path(nil) != filepath.Join(h, "kanban.db") || path(BoardArg("default")) != filepath.Join(h, "kanban.db") {
		t.Fatal("default board not at the legacy path", path(nil))
	}
	named := filepath.Join(h, "kanban", "boards", "atm10-server", "kanban.db")
	if path(BoardArg("atm10-server")) != named {
		t.Fatal("named board path", path(BoardArg("atm10-server")))
	}
	forced := filepath.Join(t.TempDir(), "custom.db")
	t.Setenv("ICEBATH_KANBAN_DB", forced)
	if path(nil) != forced {
		t.Fatal("env db pin lost without a board")
	}
	if p := path(BoardArg("atm10-server")); p != named || p == forced {
		t.Fatal("explicit board did not trump the env pin for an unfenced caller", p)
	}
	t.Setenv("ICEBATH_KANBAN_TASK", "t_fence_probe")
	if path(BoardArg("ignored")) != forced {
		t.Fatal("dispatched worker escaped the env pin")
	}
	t.Setenv("ICEBATH_KANBAN_TASK", "")
	t.Setenv(tools.DelegatedChildEnvMarker, "1")
	if path(BoardArg("ignored")) != forced {
		t.Fatal("delegated child escaped the env pin")
	}
}

func TestCurrentBoardStalePointerAndPathReadsCurrent(t *testing.T) {
	h := setupKanbanHome(t)
	ctx := context.Background()
	current := filepath.Join(h, "kanban", "current")
	os.MkdirAll(filepath.Dir(current), 0o755)
	os.WriteFile(current, []byte("missing-board\n"), 0o644)
	if GetCurrentBoard(ctx) != "default" || BoardExists(BoardArg("missing-board")) {
		t.Fatal("stale pointer did not fall back to default")
	}
	boards, err := ListBoards(false)
	if err != nil || len(boards) != 1 || objGet(boards[0], "slug") != "default" {
		t.Fatal("boards", err)
	}
	if _, err := CreateBoard(ctx, "my-proj", BoardCreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := SetCurrentBoard(ctx, "my-proj"); err != nil {
		t.Fatal(err)
	}
	if p, _ := KanbanDBPath(ctx, nil); p != filepath.Join(h, "kanban", "boards", "my-proj", "kanban.db") {
		t.Fatal("path ignores the current pointer", p)
	}
}

func TestRemoveBoardClearsInitCacheForRecreatedDB(t *testing.T) {
	for _, archive := range []bool{true, false} {
		setupKanbanHome(t)
		ctx := context.Background()
		if _, err := CreateBoard(ctx, "recycle", BoardCreateOptions{}); err != nil {
			t.Fatal(err)
		}
		err := ConnectClosing(ctx, "", BoardArg("recycle"), func(conn state.SQLiteConn) error {
			_, err := CreateTask(ctx, conn, CreateTaskOptions{Title: "t1", Assignee: sp("dev")})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		dir, _ := BoardDir(BoardArg("recycle"))
		dbPath := filepath.Join(dir, "kanban.db")
		if !isInitialized(dbPath) {
			t.Fatal("first connect did not cache the init pass")
		}
		if _, err := RemoveBoard(ctx, "recycle", archive); err != nil {
			t.Fatal(err)
		}
		if isInitialized(dbPath) {
			t.Fatal("remove_board kept the init cache entry", archive)
		}
		if _, err := CreateBoard(ctx, "recycle", BoardCreateOptions{}); err != nil {
			t.Fatal(err)
		}
		err = ConnectClosing(ctx, "", BoardArg("recycle"), func(conn state.SQLiteConn) error {
			tables := map[string]bool{}
			for _, r := range mustRows(t, conn, "SELECT name FROM sqlite_master WHERE type='table'") {
				tables[cellStr(r.Get("name"))] = true
			}
			if !tables["task_events"] || !tables["tasks"] {
				t.Fatal("schema not reapplied on the recreated board", archive)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestConnectionIsolationAcrossBoards(t *testing.T) {
	setupKanbanHome(t)
	ctx := context.Background()
	for _, b := range []string{"alpha", "beta", "curr", "persist", "envwin"} {
		if _, err := CreateBoard(ctx, b, BoardCreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	create := func(board *string, titles ...string) {
		conn := mustConnect(t, "", board)
		defer conn.Close()
		for _, title := range titles {
			mustCreate(t, conn, CreateTaskOptions{Title: title, Assignee: sp("dev")})
		}
	}
	titles := func(board *string) string {
		conn := mustConnect(t, "", board)
		defer conn.Close()
		return strings.Join(taskTitles(t, conn), ",")
	}
	create(BoardArg("alpha"), "alpha-task-1", "alpha-task-2")
	create(BoardArg("beta"), "beta-only")
	if titles(BoardArg("alpha")) != "alpha-task-1,alpha-task-2" || titles(BoardArg("beta")) != "beta-only" || titles(BoardArg("default")) != "" {
		t.Fatal("tasks leaked across boards")
	}
	SetCurrentBoard(ctx, "curr")
	create(nil, "implicit")
	if titles(BoardArg("curr")) != "implicit" {
		t.Fatal("connect without args ignored the current board")
	}
	SetCurrentBoard(ctx, "persist")
	t.Setenv("ICEBATH_KANBAN_BOARD", "envwin")
	create(nil, "via-env")
	if titles(BoardArg("envwin")) != "via-env" || titles(BoardArg("persist")) != "" {
		t.Fatal("env board did not override the current board")
	}
}

func TestBoardResurrectionRefusedAndDBOnlyDirsHidden(t *testing.T) {
	setupKanbanHome(t)
	ctx := context.Background()
	CreateBoard(ctx, "gone", BoardCreateOptions{})
	RemoveBoard(ctx, "gone", true)
	meta, _ := ReadBoardMetadata(BoardArg("gone"))
	tombstone, _ := BoardMetadataPath(BoardArg("gone"))
	if _, err := os.Stat(tombstone); err != nil || objGet(meta, "archived") != true {
		t.Fatal("archive tombstone missing", err)
	}
	if _, err := Connect(ctx, "", BoardArg("gone")); err == nil || !strings.Contains(err.Error(), "archived") {
		t.Fatal("connect reopened an archived board", err)
	}
	goneDir, _ := BoardDir(BoardArg("gone"))
	if _, err := os.Stat(filepath.Join(goneDir, "kanban.db")); err == nil {
		t.Fatal("archived board database recreated")
	}
	CreateBoard(ctx, "nuked", BoardCreateOptions{})
	RemoveBoard(ctx, "nuked", false)
	nukedDir, _ := BoardDir(BoardArg("nuked"))
	if _, err := Connect(ctx, "", BoardArg("nuked")); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatal("connect recreated a deleted board", err)
	}
	if _, err := os.Stat(nukedDir); err == nil {
		t.Fatal("deleted board directory recreated")
	}
	CreateBoard(ctx, "live", BoardCreateOptions{})
	if err := ConnectClosing(ctx, "", BoardArg("live"), func(conn state.SQLiteConn) error {
		_, err := QueryOne(ctx, conn, "SELECT 1")
		return err
	}); err != nil || !BoardExists(BoardArg("live")) {
		t.Fatal("live board did not open", err)
	}
	ghost, _ := BoardDir(BoardArg("ghost"))
	os.MkdirAll(ghost, 0o755)
	os.WriteFile(filepath.Join(ghost, "kanban.db"), nil, 0o644)
	CreateBoard(ctx, "keepname", BoardCreateOptions{Name: sp("My Board")})
	RemoveBoard(ctx, "keepname", true)
	meta, _ = ReadBoardMetadata(BoardArg("keepname"))
	if objGet(meta, "archived") != true || objGet(meta, "name") != "My Board" {
		t.Fatal("tombstone lost the name")
	}
	slugs := func(includeArchived bool) map[string]bool {
		boards, err := ListBoards(includeArchived)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, b := range boards {
			out[objGet(b, "slug").(string)] = true
		}
		return out
	}
	if slugs(false)["ghost"] || slugs(true)["ghost"] {
		t.Fatal("db-only directory surfaced as a board")
	}
	if !slugs(true)["keepname"] || slugs(false)["keepname"] {
		t.Fatal("archived board listing wrong")
	}
}

func TestBoardProjectMetadataAndCreateInheritance(t *testing.T) {
	h := setupKanbanHome(t)
	ctx := context.Background()
	def := BoardArg("default")
	if meta, _ := ReadBoardMetadata(def); objGet(meta, "project_id") != nil {
		t.Fatal("fresh default board has a project")
	}
	WriteBoardMetadata(ctx, def, BoardMetadataUpdate{ProjectID: sp("p_abc123")})
	WriteBoardMetadata(ctx, def, BoardMetadataUpdate{Name: sp("Still Here")})
	if meta, _ := ReadBoardMetadata(def); objGet(meta, "project_id") != "p_abc123" {
		t.Fatal("project id not kept across an unrelated update")
	}
	WriteBoardMetadata(ctx, def, BoardMetadataUpdate{ProjectID: sp("")})
	if meta, _ := ReadBoardMetadata(def); objGet(meta, "project_id") != nil {
		t.Fatal("empty project id did not clear")
	}
	meta, err := CreateBoard(ctx, "proj-board", BoardCreateOptions{Name: sp("Proj Board"), ProjectID: sp("p_xyz")})
	if err != nil || objGet(meta, "project_id") != "p_xyz" {
		t.Fatal("create_board dropped project_id", err)
	}
	if meta, _ := ReadBoardMetadata(BoardArg("proj-board")); objGet(meta, "project_id") != "p_xyz" {
		t.Fatal("project id not persisted")
	}
	pdb, err := tools.OpenProjectsDB(&tools.Env{Home: h})
	if err != nil {
		t.Fatal(err)
	}
	project := func(name string) string {
		dir := filepath.Join(t.TempDir(), name)
		os.MkdirAll(dir, 0o755)
		id, err := pdb.CreateProject(name, tools.CreateProjectOptions{PrimaryPath: dir})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	widget, boardProj, taskProj := project("Widget"), project("BoardProj"), project("TaskProj")
	pdb.Close()
	CreateBoard(ctx, "scoped", BoardCreateOptions{Name: sp("Scoped"), ProjectID: &widget})
	CreateBoard(ctx, "scoped2", BoardCreateOptions{Name: sp("Scoped2"), ProjectID: &boardProj})
	conn := mustConnect(t, "", BoardArg("scoped"))
	defer conn.Close()
	inherit := mustTask(t, conn, mustCreate(t, conn, CreateTaskOptions{Title: "inherit me", Board: BoardArg("scoped")}))
	if inherit.ProjectID == nil || *inherit.ProjectID != widget {
		t.Fatal("task did not inherit the board project")
	}
	explicit := mustTask(t, conn, mustCreate(t, conn, CreateTaskOptions{Title: "explicit", Board: BoardArg("scoped2"), ProjectID: &taskProj}))
	if explicit.ProjectID == nil || *explicit.ProjectID != taskProj {
		t.Fatal("explicit project lost to the board project")
	}
	scratch := mustTask(t, conn, mustCreate(t, conn, CreateTaskOptions{Title: "explicit scratch", Board: BoardArg("scoped"), WorkspaceKind: sp("scratch")}))
	if scratch.WorkspaceKind != "scratch" || scratch.ProjectID != nil {
		t.Fatal("explicit scratch inherited the board project")
	}
	dflt := mustTask(t, conn, mustCreate(t, conn, CreateTaskOptions{Title: "default", Board: BoardArg("scoped")}))
	if dflt.WorkspaceKind != "worktree" || dflt.ProjectID == nil || *dflt.ProjectID != widget {
		t.Fatal("omitted kind did not inherit the project worktree", dflt.WorkspaceKind)
	}
}

func TestConnectMigratesLegacyDBBeforeOptionalColumnIndexes(t *testing.T) {
	setupKanbanHome(t)
	dbPath := filepath.Join(t.TempDir(), "legacy-kanban.db")
	seed := rawSQLite(t, dbPath)
	mustExec(t, seed, `CREATE TABLE tasks (id TEXT PRIMARY KEY, title TEXT NOT NULL, body TEXT, assignee TEXT, status TEXT NOT NULL,
            priority INTEGER NOT NULL DEFAULT 0, created_by TEXT, created_at INTEGER NOT NULL, started_at INTEGER, completed_at INTEGER,
            workspace_kind TEXT NOT NULL DEFAULT 'scratch', workspace_path TEXT, claim_lock TEXT, claim_expires INTEGER)`,
		`CREATE TABLE task_events (id INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT NOT NULL, kind TEXT NOT NULL, payload TEXT, created_at INTEGER NOT NULL)`,
		`INSERT INTO tasks (id, title, status, created_at) VALUES ('legacy', 'old board task', 'ready', 1)`)
	seed.Close()
	conn := mustConnect(t, dbPath, nil)
	defer conn.Close()
	taskCols, eventCols := tableInfo(t, conn, "tasks"), tableInfo(t, conn, "task_events")
	indexes := map[string]bool{}
	for _, r := range mustRows(t, conn, "SELECT name FROM sqlite_master WHERE type = 'index'") {
		indexes[cellStr(r.Get("name"))] = true
	}
	for _, c := range []string{"session_id", "tenant", "idempotency_key"} {
		if taskCols[c] == nil {
			t.Fatal("tasks column not migrated", c)
		}
	}
	if eventCols["run_id"] == nil {
		t.Fatal("task_events.run_id not migrated")
	}
	for _, i := range []string{"idx_tasks_session_id", "idx_tasks_tenant", "idx_tasks_idempotency", "idx_events_run"} {
		if !indexes[i] {
			t.Fatal("index missing", i)
		}
	}
}

func TestConnectHealsReducedTasksSchemaSeededByExternalHarness(t *testing.T) {
	h := setupKanbanHome(t)
	dbPath := filepath.Join(h, "foreign.db")
	seed := rawSQLite(t, dbPath)
	var runsSQL string
	for _, spec := range rebuildSpecs {
		if spec.table == "task_runs" {
			runsSQL = spec.createSQL
		}
	}
	mustExec(t, seed, "CREATE TABLE tasks (id TEXT PRIMARY KEY, title TEXT NOT NULL, status TEXT NOT NULL, created_at INTEGER NOT NULL)", runsSQL)
	seed.Close()
	conn := mustConnect(t, dbPath, nil)
	healedInfo := tableInfo(t, conn, "tasks")
	fresh := rawSQLite(t, ":memory:")
	mustExec(t, fresh, SchemaSQL)
	freshInfo := tableInfo(t, fresh, "tasks")
	for _, c := range []string{"body", "assignee", "priority", "created_by", "started_at", "completed_at", "workspace_kind", "workspace_path", "claim_lock", "claim_expires"} {
		got, want := healedInfo[c], freshInfo[c]
		if got == nil {
			t.Fatal("column not healed", c)
		}
		for _, k := range []string{"type", "notnull", "dflt_value", "pk"} {
			if got.Get(k) != want.Get(k) {
				t.Fatal("healed column differs from the schema", c, k, got.Get(k), want.Get(k))
			}
		}
	}
	fresh.Close()
	conn.Close()
	conn = mustConnect(t, dbPath, nil)
	defer conn.Close()
	if len(taskTitles(t, conn)) != 0 {
		t.Fatal("healed board not empty")
	}
}

func TestRepeatedCorruptOpenReusesSingleBackup(t *testing.T) {
	setupKanbanHome(t)
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "kanban.db")
	writePageCorruptDB(t, dbPath)
	original, _ := os.ReadFile(dbPath)
	backups := map[string]bool{}
	for range 10 {
		discardInitialized(t, dbPath)
		_, err := Connect(ctx, dbPath, nil)
		var ce *KanbanDBCorruptError
		if !errors.As(err, &ce) || ce.BackupPath == "" {
			t.Fatal("corrupt open did not raise with a backup", err)
		}
		backups[ce.BackupPath] = true
	}
	if len(backups) != 1 {
		t.Fatal("expected one deterministic backup, got", len(backups))
	}
	var backup string
	for b := range backups {
		backup = b
	}
	if data, err := os.ReadFile(backup); err != nil || string(data) != string(original) {
		t.Fatal("backup bytes differ", err)
	}
	f, _ := os.OpenFile(dbPath, os.O_RDWR, 0)
	f.WriteAt([]byte(strings.Repeat("\xAB", 64)), 4096)
	f.Close()
	discardInitialized(t, dbPath)
	_, err := Connect(ctx, dbPath, nil)
	var ce *KanbanDBCorruptError
	if !errors.As(err, &ce) || ce.BackupPath == "" || ce.BackupPath == backup {
		t.Fatal("changed bytes reused the old backup", err)
	}
	if _, err := os.Stat(ce.BackupPath); err != nil {
		t.Fatal(err)
	}
}

func TestConnectSetsSecureDeleteOn(t *testing.T) {
	setupKanbanHome(t)
	dbPath := filepath.Join(t.TempDir(), "kanban.db")
	discardInitialized(t, dbPath)
	conn := mustConnect(t, dbPath, nil)
	defer conn.Close()
	if rows := mustRows(t, conn, "PRAGMA secure_delete"); cellInt(rows[0].Get("secure_delete")) != 1 {
		t.Fatal("secure_delete not on")
	}
}

func TestRecomputeReadyHonoursDispatcherFailureLimit(t *testing.T) {
	setupKanbanHome(t)
	ctx := context.Background()
	conn := mustConnect(t, "", nil)
	defer conn.Close()
	id := mustCreate(t, conn, CreateTaskOptions{Title: "lenient", Assignee: sp("a")})
	if _, err := conn.ExecContext(ctx, "UPDATE tasks SET status='blocked', consecutive_failures=? WHERE id=?", DefaultFailureLimit, id); err != nil {
		t.Fatal(err)
	}
	if n, err := RecomputeReady(ctx, conn, nil); err != nil || n != 0 || mustTask(t, conn, id).Status != "blocked" {
		t.Fatal("default limit promoted a tripped task", n, err)
	}
	lenient := int64(DefaultFailureLimit + 2)
	if n, err := RecomputeReady(ctx, conn, &lenient); err != nil || n != 1 {
		t.Fatal("raised limit did not promote", n, err)
	}
	if task := mustTask(t, conn, id); task.Status != "ready" || task.ConsecutiveFailures != DefaultFailureLimit {
		t.Fatal("promotion changed the counter", task.Status, task.ConsecutiveFailures)
	}
	strictID := mustCreate(t, conn, CreateTaskOptions{Title: "strict", Assignee: sp("a")})
	conn.ExecContext(ctx, "UPDATE tasks SET status='blocked', consecutive_failures=1 WHERE id=?", strictID)
	strict := int64(1)
	if n, err := RecomputeReady(ctx, conn, &strict); err != nil || n != 0 || mustTask(t, conn, strictID).Status != "blocked" {
		t.Fatal("lowered limit let a tripped task escape", n, err)
	}
}

func TestLinkTasksEmitsDependencyWaitWhenDemotingReadyChild(t *testing.T) {
	setupKanbanHome(t)
	ctx := context.Background()
	conn := mustConnect(t, "", nil)
	defer conn.Close()
	parent := mustCreate(t, conn, CreateTaskOptions{Title: "blocked parent"})
	child := mustCreate(t, conn, CreateTaskOptions{Title: "support card"})
	conn.ExecContext(ctx, "UPDATE tasks SET status = 'ready' WHERE id = ?", child)
	gated, err := LinkTasks(ctx, conn, parent, child, nil)
	if err != nil || !gated || mustTask(t, conn, child).Status != "todo" {
		t.Fatal("link_tasks did not report the demotion", gated, err)
	}
	wait := eventsOfKind(t, conn, child, "dependency_wait")
	if len(wait) == 0 {
		t.Fatal("demotion not recorded")
	}
	p := wait[len(wait)-1].Payload.(*pyjson.Object)
	if objGet(p, "reason") != "parent_not_done" || objGet(p, "demoted") != true || objGet(p, "parent") != parent {
		t.Fatal("dependency_wait payload", p)
	}
}

func TestLinkTasksRejectsUnownedRunningChildWithoutRecordingEdge(t *testing.T) {
	setupKanbanHome(t)
	ctx := context.Background()
	conn := mustConnect(t, "", nil)
	defer conn.Close()
	parent := mustCreate(t, conn, CreateTaskOptions{Title: "unfinished parent"})
	child := mustCreate(t, conn, CreateTaskOptions{Title: "claimed child"})
	if claimed, err := ClaimTask(ctx, conn, child, nil, sp("worker")); err != nil || claimed == nil {
		t.Fatal("claim failed", err)
	}
	var ve *ValueError
	if _, err := LinkTasks(ctx, conn, parent, child, nil); !errors.As(err, &ve) || !strings.Contains(err.Error(), "child is already running") {
		t.Fatal("running child accepted an unowned edge", err)
	}
	if parents, _ := ParentIDs(ctx, conn, child); len(parents) != 0 || len(eventsOfKind(t, conn, child, "linked")) != 0 {
		t.Fatal("refused edge was recorded")
	}
}

func TestAssignFiresUpdatedWithChangedFieldsAfterCommit(t *testing.T) {
	setupKanbanHome(t)
	ctx := context.Background()
	if _, err := InitDB(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
	var captured []map[string]any
	captureHook(t, "on_kanban_task_updated", func(_ context.Context, kw map[string]any) (any, error) {
		captured = append(captured, kw)
		return nil, nil
	})
	var atFire []any
	captureHook(t, "on_kanban_task_updated", func(_ context.Context, kw map[string]any) (any, error) {
		c2 := rawSQLite(t, mustDBPath(t, nil))
		defer c2.Close()
		row, err := QueryOne(ctx, c2, "SELECT assignee FROM tasks WHERE id = ?", kw["task_id"])
		if err == nil && row != nil {
			atFire = append(atFire, row.Get("assignee"))
		}
		return nil, nil
	})
	conn := mustConnect(t, "", nil)
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "t", Assignee: sp("alice")})
	captured, atFire = nil, nil
	ok, err := AssignTask(ctx, conn, tid, sp("bob"))
	conn.Close()
	if err != nil || !ok || len(captured) != 1 {
		t.Fatal("assign did not fire once", ok, err, len(captured))
	}
	kw := captured[0]
	fields, _ := kw["changed_fields"].([]any)
	if kw["task_id"] != tid || len(fields) != 1 || fields[0] != "assignee" || kw["assignee"] != "bob" {
		t.Fatal("hook kwargs", kw)
	}
	for _, k := range []string{"profile_name", "board", "run_id"} {
		if _, ok := kw[k]; !ok {
			t.Fatal("hook kwargs miss", k)
		}
	}
	if len(atFire) != 1 || atFire[0] != "bob" {
		t.Fatal("hook fired before the assignment committed", atFire)
	}
}

func TestRaisingUpdatedCallbackDoesNotBreakAssign(t *testing.T) {
	setupKanbanHome(t)
	ctx := context.Background()
	InitDB(ctx, "", nil)
	captureHook(t, "on_kanban_task_updated", func(context.Context, map[string]any) (any, error) {
		panic("plugin exploded")
	})
	conn := mustConnect(t, "", nil)
	defer conn.Close()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "t", Assignee: sp("alice")})
	if ok, err := AssignTask(ctx, conn, tid, sp("bob")); err != nil || !ok {
		t.Fatal("raising observer broke assign", err)
	}
	if a := mustTask(t, conn, tid).Assignee; a == nil || *a != "bob" {
		t.Fatal("assignment lost")
	}
}

func TestClaimFiresHook(t *testing.T) {
	setupKanbanHome(t)
	ctx := context.Background()
	InitDB(ctx, "", nil)
	var fired []map[string]any
	captureHook(t, "kanban_task_claimed", func(_ context.Context, kw map[string]any) (any, error) {
		fired = append(fired, kw)
		return nil, nil
	})
	conn := mustConnect(t, "", nil)
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "t", Assignee: sp("worker")})
	claimed, err := ClaimTask(ctx, conn, tid, nil, nil)
	conn.Close()
	if err != nil || claimed == nil || len(fired) != 1 {
		t.Fatal("claim hook", err, len(fired))
	}
	kw := fired[0]
	if _, ok := kw["profile_name"]; kw["task_id"] != tid || kw["assignee"] != "worker" || !ok || kw["run_id"] == nil {
		t.Fatal("claim hook kwargs", kw)
	}
}
