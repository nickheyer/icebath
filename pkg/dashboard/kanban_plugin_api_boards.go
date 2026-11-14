package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	kanbanCreateBoardModel = &Model{ClassName: "CreateBoardBody", Fields: []ModelField{
		Field("slug", Str), FieldDefault("name", optStr, nil), FieldDefault("description", optStr, nil),
		FieldDefault("icon", optStr, nil), FieldDefault("color", optStr, nil), FieldDefault("default_workdir", optStr, nil),
		FieldDefault("project_id", optStr, nil), FieldDefault("switch", Bool, false)}}
	kanbanRenameBoardModel = &Model{ClassName: "RenameBoardBody", Fields: []ModelField{
		FieldDefault("name", optStr, nil), FieldDefault("description", optStr, nil), FieldDefault("icon", optStr, nil),
		FieldDefault("color", optStr, nil), FieldDefault("default_workdir", optStr, nil), FieldDefault("project_id", optStr, nil)}}
	kanbanExportBoardModel = &Model{ClassName: "ExportBoardBody", Fields: []ModelField{
		FieldDefault("output", Str, ""), FieldDefault("attachments", Bool, true), FieldDefault("logs", Bool, false)}}
	kanbanImportBoardModel = &Model{ClassName: "ImportBoardBody", Fields: []ModelField{
		Field("archive", Str), FieldDefault("slug", optStr, nil), FieldDefault("switch", Bool, false)}}
	kanbanDescribeModel      = &Model{ClassName: "DescribeBody", Fields: []ModelField{FieldDefault("description", optStr, nil)}}
	kanbanDescribeAutoModel  = &Model{ClassName: "DescribeAutoBody", Fields: []ModelField{FieldDefault("overwrite", Bool, false)}}
	kanbanDecomposeModel     = &Model{ClassName: "DecomposeBody", Fields: []ModelField{FieldDefault("author", optStr, nil)}}
	kanbanOrchestrationModel = &Model{ClassName: "OrchestrationSettingsBody", Fields: []ModelField{
		FieldDefault("orchestrator_profile", optStr, nil), FieldDefault("default_assignee", optStr, nil),
		FieldDefault("auto_decompose", optBool, nil), FieldDefault("auto_promote_children", optBool, nil)}}
)

var kanbanProfileSettings = []string{"orchestrator_profile", "default_assignee"}

// Runs fn over the projects database of the active home and closes it
func kanbanWithProjects[T any](ctx context.Context, fn func(db *tools.ProjectsDB) (T, error)) (T, error) {
	var zero T
	db, err := tools.OpenProjectsDB(ctx, &tools.Env{Home: home.Dir(ctx)})
	if err != nil {
		return zero, err
	}
	defer db.Close()
	return fn(db)
}

// Id, name and primary path of a project reference, all nil for a blank one and a 400 for an unknown one
func kanbanResolveProject(ctx context.Context, ref *string) (id, name, primaryPath *string, err error) {
	if ref == nil || pystr.Strip(*ref) == "" {
		return nil, nil, nil, nil
	}
	proj, err := kanbanWithProjects(ctx, func(db *tools.ProjectsDB) (*tools.Project, error) {
		return db.GetProject(pystr.Strip(*ref))
	})
	if err != nil {
		return nil, nil, nil, kanbanErrorsTo500("projects unavailable", err)
	}
	if proj == nil {
		return nil, nil, nil, NewHTTPException(400, fmt.Sprintf("project %s does not exist", pyrepr.Repr(*ref)))
	}
	if proj.PrimaryPath != nil && *proj.PrimaryPath != "" {
		primaryPath = proj.PrimaryPath
	}
	return &proj.ID, &proj.Name, primaryPath, nil
}

// Every project by id with archived ones, empty when the projects database is unreadable
func kanbanProjectsByID(ctx context.Context) map[string]*tools.Project {
	out := map[string]*tools.Project{}
	projects, err := kanbanWithProjects(ctx, func(db *tools.ProjectsDB) ([]*tools.Project, error) { return db.ListProjects(true) })
	if err != nil {
		return out
	}
	for _, p := range projects {
		out[p.ID] = p
	}
	return out
}

// Task count per status of a board, empty for a missing or unreadable database
func kanbanBoardCounts(ctx context.Context, slug string) *pyjson.Object {
	counts := pyjson.NewObject()
	path, err := kanban.KanbanDBPath(ctx, &slug)
	if err != nil {
		return counts
	}
	if _, err := os.Stat(path); err != nil {
		return counts
	}
	conn, err := kanban.Connect(ctx, "", &slug)
	if err != nil {
		return counts
	}
	defer conn.Close()
	rows, err := kanban.QueryRows(ctx, conn, "SELECT status, COUNT(*) AS n FROM tasks GROUP BY status")
	if err != nil {
		return pyjson.NewObject()
	}
	for _, r := range rows {
		counts.Set(pyrepr.Str(r.Get("status")), kanbanInt(r.Get("n")))
	}
	return counts
}

// Task workspace kind a board recommends, scratch without a workdir and worktree inside a git checkout
func kanbanDefaultWorkspaceKind(board *pyjson.Object) string {
	workdir := ""
	if v := board.GetOr("default_workdir", nil); pyTruthy(v) {
		workdir = pystr.Strip(pyrepr.Str(v))
	}
	if workdir == "" {
		return "scratch"
	}
	if kanban.GitToplevel(workdir) != nil {
		return "worktree"
	}
	return "dir"
}

// Adds the recommended workspace kind and the project name to board metadata
func kanbanAnnotateBoardMeta(ctx context.Context, meta *pyjson.Object) (*pyjson.Object, error) {
	meta.Set("default_workspace_kind", kanbanDefaultWorkspaceKind(meta))
	var ref *string
	if s, ok := meta.GetOr("project_id", nil).(string); ok {
		ref = &s
	}
	_, name, _, err := kanbanResolveProject(ctx, ref)
	if err != nil {
		return nil, err
	}
	meta.Set("project_name", kanbanOptAny(name))
	return meta, nil
}

// Live projects a board can be scoped to
func kanbanListProjects(ctx context.Context) (any, error) {
	projects, err := kanbanWithProjects(ctx, func(db *tools.ProjectsDB) ([]*tools.Project, error) { return db.ListProjects(false) })
	if err != nil {
		return nil, kanbanErrorsTo500("failed to list projects", err)
	}
	out := make([]any, 0, len(projects))
	for _, p := range projects {
		out = append(out, pyjson.ObjectOf("id", p.ID, "slug", p.Slug, "name", p.Name, "primary_path", kanbanText(p.PrimaryPath),
			"icon", kanbanText(p.Icon), "color", kanbanText(p.Color)))
	}
	return pyjson.ObjectOf("projects", out), nil
}

// Every board on disk with its task counts and the active slug
func kanbanListBoards(ctx context.Context, includeArchived bool) (any, error) {
	boards, err := kanban.ListBoards(includeArchived)
	if err != nil {
		return nil, err
	}
	current := kanban.GetCurrentBoard(ctx)
	projects := kanbanProjectsByID(ctx)
	out := make([]any, 0, len(boards))
	for _, b := range boards {
		slug := pyrepr.Str(b.GetOr("slug", nil))
		b.Set("is_current", slug == current)
		counts := kanbanBoardCounts(ctx, slug)
		b.Set("counts", counts)
		total := int64(0)
		for _, status := range counts.Keys {
			if status != "archived" {
				total += kanbanInt(counts.Vals[status])
			}
		}
		b.Set("total", total)
		b.Set("default_workspace_kind", kanbanDefaultWorkspaceKind(b))
		var pid any
		if v := b.GetOr("project_id", nil); pyTruthy(v) {
			pid = v
		}
		b.Set("project_id", pid)
		var projectName any
		if id, ok := pid.(string); ok {
			if proj := projects[id]; proj != nil {
				projectName = proj.Name
			}
		}
		b.Set("project_name", projectName)
		out = append(out, b)
	}
	return pyjson.ObjectOf("boards", out, "current", current), nil
}

// Resolved absolute existing directory of a board workdir, a 400 otherwise
func kanbanValidateWorkdir(raw string) (string, error) {
	requested, err := pypath.PathExpanduser(raw)
	if err != nil {
		return "", err
	}
	if !pypath.IsAbs(requested) {
		return "", NewHTTPException(400, "Project directory must be an absolute path.")
	}
	if info, err := os.Stat(requested); err != nil || !info.IsDir() {
		return "", NewHTTPException(400, "Project directory must be an existing directory.")
	}
	return pypath.Resolve(requested)
}

// Display metadata fields a create and a rename share
func kanbanBoardDisplay(p *pyjson.Object) (name, description, icon, color *string) {
	return kanbanField(p, "name"), kanbanField(p, "description"), kanbanField(p, "icon"), kanbanField(p, "color")
}

// Creates a board, an existing slug answering the board it names
func kanbanCreateBoard(ctx context.Context, payload *pyjson.Object) (any, error) {
	var defaultWorkdir *string
	if raw := kanbanText(kanbanField(payload, "default_workdir")); raw != "" {
		validated, err := kanbanValidateWorkdir(raw)
		if err != nil {
			return nil, err
		}
		defaultWorkdir = &validated
	}
	projectID, _, primaryPath, err := kanbanResolveProject(ctx, kanbanField(payload, "project_id"))
	if err != nil {
		return nil, err
	}
	if primaryPath != nil && defaultWorkdir == nil {
		defaultWorkdir = primaryPath
	}
	name, description, icon, color := kanbanBoardDisplay(payload)
	meta, err := kanban.CreateBoard(ctx, pyrepr.Str(payload.GetOr("slug", "")), kanban.BoardCreateOptions{Name: name,
		Description: description, Icon: icon, Color: color, DefaultWorkdir: defaultWorkdir, ProjectID: projectID})
	if err != nil {
		return nil, kanbanValueError400(err)
	}
	if payload.GetOr("switch", false) == true {
		if _, err := kanban.SetCurrentBoard(ctx, pyrepr.Str(meta.GetOr("slug", nil))); err != nil {
			return nil, kanbanValueError400(err)
		}
	}
	annotated, err := kanbanAnnotateBoardMeta(ctx, meta)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("board", annotated, "current", kanban.GetCurrentBoard(ctx)), nil
}

// Updates the display metadata, default workdir and project scope of a board
func kanbanRenameBoard(ctx context.Context, slug string, payload *pyjson.Object) (any, error) {
	normed, err := kanbanExistingBoardSlug(slug)
	if err != nil {
		return nil, err
	}
	var defaultWorkdir *string
	if given := kanbanField(payload, "default_workdir"); given != nil {
		raw := pystr.Strip(*given)
		if raw != "" {
			if raw, err = kanbanValidateWorkdir(raw); err != nil {
				return nil, err
			}
		}
		defaultWorkdir = &raw
	}
	var projectID *string
	if given := kanbanField(payload, "project_id"); given != nil {
		if pystr.Strip(*given) != "" {
			var primaryPath *string
			if projectID, _, primaryPath, err = kanbanResolveProject(ctx, given); err != nil {
				return nil, err
			}
			if primaryPath != nil && defaultWorkdir == nil {
				defaultWorkdir = primaryPath
			}
		} else {
			cleared := ""
			projectID = &cleared
		}
	}
	name, description, icon, color := kanbanBoardDisplay(payload)
	meta, err := kanban.WriteBoardMetadata(ctx, &normed, kanban.BoardMetadataUpdate{Name: name, Description: description,
		Icon: icon, Color: color, DefaultWorkdir: defaultWorkdir, ProjectID: projectID})
	if err != nil {
		return nil, err
	}
	annotated, err := kanbanAnnotateBoardMeta(ctx, meta)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("board", annotated), nil
}

// Archives a board, or deletes it for good when asked
func kanbanDeleteBoard(ctx context.Context, slug string, hardDelete bool) (any, error) {
	result, err := kanban.RemoveBoard(ctx, slug, !hardDelete)
	if err != nil {
		return nil, kanbanValueError400(err)
	}
	return pyjson.ObjectOf("result", result, "current", kanban.GetCurrentBoard(ctx)), nil
}

// Maps a board transfer failure to 404 for a missing path, 400 for an invalid one and a logged 500 otherwise
func kanbanTransferError(err error, label string) error {
	var notFound *kanban.FileNotFoundError
	var valueErr *kanban.ValueError
	switch {
	case errors.As(err, &notFound), errors.Is(err, fs.ErrNotExist):
		return NewHTTPException(404, pyExcStr(err))
	case errors.As(err, &valueErr):
		return NewHTTPException(400, pyExcStr(err))
	}
	kanbanLog.Error(label+" failed", "error", err)
	return NewHTTPException(500, pyExcStr(err))
}

// Writes a board to a portable archive and answers the path written
func kanbanExportBoard(ctx context.Context, slug string, body *pyjson.Object) (any, error) {
	output := pystr.Strip(pyrepr.Str(body.GetOr("output", "")))
	if output == "" {
		staging := pypath.PathJoin(kanban.KanbanHome(), "kanban", "board-exports")
		if err := os.MkdirAll(staging, 0o777); err != nil {
			return nil, NewHTTPException(500, "Could not create export directory: "+pypath.OSErrorText(err))
		}
		output = pypath.PathJoin(staging, fmt.Sprintf("%s-%s.tar.gz", slug, time.Now().Format("20060102-150405")))
	}
	result, err := kanban.ExportBoard(ctx, &slug, output, body.GetOr("attachments", true) == true, body.GetOr("logs", false) == true)
	if err != nil {
		return nil, kanbanTransferError(err, fmt.Sprintf("POST /boards/%s/export", slug))
	}
	return result, nil
}

// Imports a board archive as a new board and answers the landed board
func kanbanImportBoard(ctx context.Context, body *pyjson.Object) (any, error) {
	archive := pystr.Strip(pyrepr.Str(body.GetOr("archive", "")))
	if archive == "" {
		return nil, NewHTTPException(400, "archive path is required")
	}
	result, err := kanban.ImportBoard(ctx, archive, kanbanNonEmpty(pystr.Strip(kanbanText(kanbanField(body, "slug")))),
		body.GetOr("switch", false) == true)
	if err != nil {
		return nil, kanbanTransferError(err, "POST /boards/import")
	}
	out := result.Copy()
	out.Set("current", kanban.GetCurrentBoard(ctx))
	return out, nil
}

// Persists a board as the active one for the CLI and slash commands
func kanbanSwitchBoard(ctx context.Context, slug string) (any, error) {
	normed, err := kanbanExistingBoardSlug(slug)
	if err != nil {
		return nil, err
	}
	if _, err := kanban.SetCurrentBoard(ctx, normed); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("current", normed), nil
}

// Text of a profile field that may be unset
func kanbanProfileText(v any) any {
	if pyTruthy(v) {
		return v
	}
	return ""
}

// Every installed profile with its description
func kanbanListProfileRoster(ctx context.Context) (any, error) {
	roster, err := manage.ListProfiles(ctx, false)
	if err != nil {
		return nil, kanbanErrorsTo500("failed to list profiles", err)
	}
	out := make([]any, 0, len(roster))
	for _, p := range roster {
		out = append(out, pyjson.ObjectOf("name", p.Name, "is_default", p.IsDefault, "model", kanbanProfileText(p.Model),
			"provider", kanbanProfileText(p.Provider), "description", p.Description, "description_auto", p.DescriptionAuto,
			"skill_count", int64(p.SkillCount)))
	}
	return pyjson.ObjectOf("profiles", out), nil
}

// Sets or clears the user authored description of a profile
func kanbanUpdateProfileDescription(ctx context.Context, profileName string, payload *pyjson.Object) (any, error) {
	canon, text, err := func() (string, string, error) {
		canon, err := profiles.NormalizeProfileName(profileName)
		if err != nil {
			return "", "", err
		}
		profileDir := home.Dir(ctx)
		if canon != "default" {
			if profileDir, err = profiles.GetProfileDir(canon); err != nil {
				return "", "", err
			}
		}
		if info, err := os.Stat(profileDir); err != nil || !info.IsDir() {
			return "", "", NewHTTPException(404, fmt.Sprintf("profile '%s' not found", profileName))
		}
		text := pystr.Strip(kanbanText(kanbanField(payload, "description")))
		auto := false
		return canon, text, profiles.WriteProfileMeta(ctx, profileDir, profiles.MetaUpdate{Description: &text, DescriptionAuto: &auto})
	}()
	if err != nil {
		return nil, kanbanErrorsTo500("failed to update profile", err)
	}
	return pyjson.ObjectOf("ok", true, "profile", canon, "description", text), nil
}

// Generates a profile description through the auxiliary model, a refusal answered inline
func kanbanAutoDescribeProfile(ctx context.Context, profileName string, payload *pyjson.Object) (any, error) {
	outcome, err := manage.DescribeProfile(ctx, profileName, payload.GetOr("overwrite", false) == true, 0)
	if err != nil {
		return nil, kanbanErrorsTo500("describer crashed", err)
	}
	return pyjson.ObjectOf("ok", outcome.OK, "profile", outcome.ProfileName, "reason", outcome.Reason,
		"description", kanbanOptAny(outcome.Description)), nil
}

// Fans a triage task out into child tasks through the auxiliary model, a refusal answered inline
func kanbanDecomposeTask(ctx context.Context, taskID string, payload *pyjson.Object, board *string) (any, error) {
	pinned, err := kanbanBoardPinned(ctx, board)
	if err != nil {
		return nil, err
	}
	outcome, err := kanban.DecomposeTask(pinned, taskID, kanbanNonEmpty(kanbanText(kanbanField(payload, "author"))), nil)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", outcome.OK, "task_id", outcome.TaskID, "reason", outcome.Reason, "fanout", outcome.Fanout,
		"child_ids", kanbanStrings(outcome.ChildIDs), "new_title", kanbanOptAny(outcome.NewTitle)), nil
}

// Orchestration knobs of the config with the profiles they resolve to
func kanbanGetOrchestrationSettings(ctx context.Context) (any, error) {
	section, err := kanbanConfigSection(kanbanLoadConfigOrEmpty(ctx)["kanban"])
	if err != nil {
		return nil, err
	}
	explicit := map[string]string{}
	for _, key := range kanbanProfileSettings {
		value := section[key]
		if !pyTruthy(value) {
			explicit[key] = ""
			continue
		}
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute 'strip'", pyrepr.TypeName(value))
		}
		explicit[key] = pystr.Strip(text)
	}
	activeDefault := kanbanActiveProfileName(ctx)
	resolved := map[string]string{}
	for _, key := range kanbanProfileSettings {
		resolved[key] = explicit[key]
		if explicit[key] == "" || !profiles.ProfileExists(explicit[key]) {
			resolved[key] = activeDefault
		}
	}
	return pyjson.ObjectOf("orchestrator_profile", explicit["orchestrator_profile"], "default_assignee", explicit["default_assignee"],
		"auto_decompose", kanbanConfigBool(section, "auto_decompose", true),
		"auto_promote_children", kanbanConfigBool(section, "auto_promote_children", true),
		"resolved_orchestrator_profile", resolved["orchestrator_profile"], "resolved_default_assignee", resolved["default_assignee"],
		"active_profile", activeDefault), nil
}

// Stripped profile name, a 400 when it is set and names no profile
func kanbanValidatedProfileName(raw string) (string, error) {
	name := pystr.Strip(raw)
	if name != "" && !profiles.ProfileExists(name) {
		return "", NewHTTPException(400, fmt.Sprintf("profile '%s' does not exist", name))
	}
	return name, nil
}

// Writes the orchestration knobs a payload names into the config and answers the resolved state
func kanbanSetOrchestrationSettings(ctx context.Context, payload *pyjson.Object) (any, error) {
	tree, err := kanban.LoadConfigTree(ctx)
	if err != nil {
		return nil, kanbanErrorsTo500("failed to load config", err)
	}
	cfg := map[string]any(tree)
	if cfg == nil {
		cfg = map[string]any{}
	}
	section, ok := cfg["kanban"].(map[string]any)
	if !ok {
		section = map[string]any{}
		cfg["kanban"] = section
	}
	for _, key := range []string{"orchestrator_profile", "default_assignee", "auto_decompose", "auto_promote_children"} {
		value := payload.GetOr(key, nil)
		if value == nil {
			continue
		}
		if key == "orchestrator_profile" || key == "default_assignee" {
			name, err := kanbanValidatedProfileName(pyrepr.Str(value))
			if err != nil {
				return nil, err
			}
			section[key] = name
		} else {
			section[key] = pyTruthy(value)
		}
	}
	if err := webSaveConfig(ctx, cfg, config.SaveOptions{}); err != nil {
		return nil, kanbanErrorsTo500("failed to save config", err)
	}
	return kanbanGetOrchestrationSettings(ctx)
}

// Tail of the task events of one board over a single reused connection
type kanbanEventTail struct {
	board *string
	conn  state.SQLiteConn
}

// Connection of the tail, opened on first use
func (t *kanbanEventTail) open(ctx context.Context) (state.SQLiteConn, error) {
	if t.conn == nil {
		conn, err := kanban.Connect(ctx, "", t.board)
		if err != nil {
			return nil, err
		}
		t.conn = conn
	}
	return t.conn, nil
}

// Current tail of the board, the cursor a client without one starts from
func (t *kanbanEventTail) latest(ctx context.Context) (int64, error) {
	conn, err := t.open(ctx)
	if err != nil {
		return 0, err
	}
	rows, err := kanban.QueryRows(ctx, conn, "SELECT COALESCE(MAX(id), 0) AS m FROM task_events")
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	return kanbanInt(rows[0].Get("m")), nil
}

// Up to 200 events after the cursor with their decoded payloads and the cursor past them
func (t *kanbanEventTail) poll(ctx context.Context, cursor int64) (int64, []any, error) {
	conn, err := t.open(ctx)
	if err != nil {
		return cursor, nil, err
	}
	rows, err := kanban.QueryRows(ctx, conn,
		"SELECT id, task_id, run_id, kind, payload, created_at "+
			"FROM task_events WHERE id > ? ORDER BY id ASC LIMIT 200", cursor)
	if err != nil {
		return cursor, nil, err
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		var payload any
		if raw := r.Get("payload"); pyTruthy(raw) {
			if decoded, err := pyjson.Loads(pyrepr.Str(raw)); err == nil {
				payload = decoded
			}
		}
		out = append(out, pyjson.ObjectOf("id", r.Get("id"), "task_id", r.Get("task_id"), "run_id", r.Get("run_id"),
			"kind", r.Get("kind"), "payload", payload, "created_at", r.Get("created_at")))
	}
	if len(rows) > 0 {
		cursor = kanbanInt(rows[len(rows)-1].Get("id"))
	}
	return cursor, out, nil
}

// Closes the connection of the tail
func (t *kanbanEventTail) shutdown() {
	if t.conn == nil {
		return
	}
	if err := t.conn.Close(); err != nil {
		kanbanLog.Warn(fmt.Sprintf("Kanban event stream connection cleanup failed: %s", pyExcStr(err)))
	}
	t.conn = nil
}
