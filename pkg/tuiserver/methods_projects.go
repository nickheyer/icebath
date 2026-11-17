package tuiserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/tools"
)

// JSON RPC error codes of a generic failure, an id resolving to nothing and an invalid argument
const (
	projectsErr   = 5061
	noProjectErr  = 5062
	projectArgErr = 5063
)

// Raised inside a projects handler when the id param resolves to no project
var errNoProject = errors.New("no such project")

var projectsRegistry = &handlerRegistry{}

func init() {
	projectMutator("update", false, func(pdb *tools.ProjectsDB, id, _ string, params map[string]any) error {
		fields, err := projectUpdateFields(params)
		if err != nil {
			return err
		}
		_, err = pdb.UpdateProject(id, fields)
		return err
	})
	projectMutator("add_folder", true, func(pdb *tools.ProjectsDB, id, path string, params map[string]any) error {
		var label *string
		if raw, present := params["label"]; present && raw != nil {
			text := pyStrOf(raw)
			label = &text
		}
		_, err := pdb.AddFolder(id, path, label, truthy(params["is_primary"]))
		return err
	})
	projectMutator("remove_folder", true, func(pdb *tools.ProjectsDB, id, path string, _ map[string]any) error {
		_, err := pdb.RemoveFolder(id, path)
		return err
	})
	projectMutator("set_primary", true, func(pdb *tools.ProjectsDB, id, path string, _ map[string]any) error {
		_, err := pdb.SetPrimary(id, path)
		return err
	})
	projectsMethod("projects.list", projectsList)
	projectsMethod("projects.get", projectsGet)
	projectsMethod("projects.create", projectsCreate)
	projectsMethod("projects.archive", projectsArchive)
	projectsMethod("projects.delete", projectsDelete)
	projectsMethod("projects.set_active", projectsSetActive)
	projectsMethod("projects.for_cwd", projectsForCwd)
	projectsRegistry.install()
}

// Opens the projects database of the active profile home
func openProjectsDB(ctx context.Context) (*tools.ProjectsDB, error) {
	return tools.OpenProjectsDB(ctx, &tools.Env{Home: home.Dir(ctx)})
}

// Error response of a projects handler failure mapped to its code
func projectsFailure(rid any, err error) map[string]any {
	var valueErr *tools.ProjectsValueError
	switch {
	case errors.Is(err, errNoProject):
		return rpcErr(rid, noProjectErr, "no such project", nil)
	case errors.As(err, &valueErr):
		return rpcErr(rid, projectArgErr, valueErr.Msg, nil)
	}
	return rpcErr(rid, projectsErr, err.Error(), nil)
}

// Registers a profile scoped projects RPC with the database open and the shared error mapping
func projectsMethod(name string, fn func(ctx context.Context, rid any, params map[string]any, pdb *tools.ProjectsDB) (map[string]any, error)) {
	projectsRegistry.scopedMethod(name, func(ctx context.Context, rid any, params map[string]any) (resp map[string]any) {
		defer func() {
			if r := recover(); r != nil {
				resp = rpcErr(rid, projectsErr, panicMessage(r), nil)
			}
		}()
		pdb, err := openProjectsDB(ctx)
		if err != nil {
			return projectsFailure(rid, err)
		}
		defer pdb.Close()
		out, err := fn(ctx, rid, params, pdb)
		if err != nil {
			return projectsFailure(rid, err)
		}
		return out
	})
}

// Every project with archived ones and the active project id
func projectsPayload(pdb *tools.ProjectsDB) (map[string]any, error) {
	projects, err := pdb.ListProjects(true)
	if err != nil {
		return nil, err
	}
	rows := make([]any, 0, len(projects))
	for _, p := range projects {
		rows = append(rows, p.AsMap())
	}
	active, err := activeProjectID(pdb)
	if err != nil {
		return nil, err
	}
	return map[string]any{"projects": rows, "active_id": active}, nil
}

// Active project id, None when unset
func activeProjectID(pdb *tools.ProjectsDB) (any, error) {
	id, err := pdb.GetActiveID()
	if err != nil || id == "" {
		return nil, err
	}
	return id, nil
}

// The project the id param names, errNoProject when none
func requireProject(pdb *tools.ProjectsDB, params map[string]any) (*tools.Project, error) {
	proj, err := pdb.GetProject(strOr(params["id"]))
	if err != nil {
		return nil, err
	}
	if proj == nil {
		return nil, errNoProject
	}
	return proj, nil
}

// str of a param value
func pyStrOf(v any) string {
	if text, isText := v.(string); isText {
		return text
	}
	return pyrepr.Str(v)
}

// Optional string field of a param, nil when missing or None
func optionalParam(params map[string]any, key string) *string {
	raw, present := params[key]
	if !present || raw == nil {
		return nil
	}
	text := pyStrOf(raw)
	return &text
}

// Patchable fields of projects.update, a board slug that is not text failing like its strip call
func projectUpdateFields(params map[string]any) (tools.UpdateProjectFields, error) {
	fields := tools.UpdateProjectFields{Name: optionalParam(params, "name"), Description: optionalParam(params, "description"),
		Icon: optionalParam(params, "icon"), Color: optionalParam(params, "color")}
	if raw, present := params["board_slug"]; present && raw != nil {
		text, isText := raw.(string)
		if !isText {
			return fields, fmt.Errorf("'%s' object has no attribute 'strip'", pyrepr.TypeName(raw))
		}
		fields.BoardSlug = &text
	}
	return fields, nil
}

// Registers projects.<suffix> resolving the id param, applying the mutation and answering with the refreshed project
func projectMutator(suffix string, takesPath bool, apply func(pdb *tools.ProjectsDB, id, path string, params map[string]any) error) {
	projectsMethod("projects."+suffix, func(ctx context.Context, rid any, params map[string]any, pdb *tools.ProjectsDB) (map[string]any, error) {
		proj, err := requireProject(pdb, params)
		if err != nil {
			return nil, err
		}
		path := ""
		if takesPath {
			path = strOr(params["path"])
		}
		if err := apply(pdb, proj.ID, path, params); err != nil {
			return nil, err
		}
		refreshed, err := pdb.GetProject(proj.ID)
		if err != nil {
			return nil, err
		}
		if refreshed == nil {
			panic(fmt.Errorf("'NoneType' object has no attribute 'to_dict'"))
		}
		return ok(rid, map[string]any{"project": refreshed.AsMap()}), nil
	})
}

// Every project with the active id
func projectsList(ctx context.Context, rid any, params map[string]any, pdb *tools.ProjectsDB) (map[string]any, error) {
	payload, err := projectsPayload(pdb)
	if err != nil {
		return nil, err
	}
	return ok(rid, payload), nil
}

// One project by id or slug
func projectsGet(ctx context.Context, rid any, params map[string]any, pdb *tools.ProjectsDB) (map[string]any, error) {
	proj, err := requireProject(pdb, params)
	if err != nil {
		return nil, err
	}
	return ok(rid, map[string]any{"project": proj.AsMap()}), nil
}

// Creates a project, optionally making it active, and records the feature use
func projectsCreate(ctx context.Context, rid any, params map[string]any, pdb *tools.ProjectsDB) (map[string]any, error) {
	var folders []string
	if raw := params["folders"]; truthy(raw) {
		list, isList := raw.([]any)
		if !isList {
			return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(raw))
		}
		for _, f := range list {
			text, isText := f.(string)
			if !isText {
				return nil, fmt.Errorf("expected str, bytes or os.PathLike object, not %s", pyrepr.TypeName(f))
			}
			folders = append(folders, text)
		}
	}
	pid, err := pdb.CreateProject(strOr(params["name"]), tools.CreateProjectOptions{Slug: strOr(params["slug"]), Folders: folders,
		PrimaryPath: strOr(params["primary_path"]), Description: optionalParam(params, "description"),
		Icon: optionalParam(params, "icon"), Color: optionalParam(params, "color"), BoardSlug: strOr(params["board_slug"])})
	if err != nil {
		return nil, err
	}
	if truthy(params["use"]) {
		if err := pdb.SetActive(&pid); err != nil {
			return nil, err
		}
	}
	observability.RecordFeatureUsed(ctx, "projects", "")
	proj, err := pdb.GetProject(pid)
	if err != nil {
		return nil, err
	}
	var row any
	if proj != nil {
		row = proj.AsMap()
	}
	return ok(rid, map[string]any{"project": row}), nil
}

// Archives a project, or restores it when restore is set
func projectsArchive(ctx context.Context, rid any, params map[string]any, pdb *tools.ProjectsDB) (map[string]any, error) {
	proj, err := requireProject(pdb, params)
	if err != nil {
		return nil, err
	}
	if truthy(params["restore"]) {
		_, err = pdb.RestoreProject(proj.ID)
	} else {
		_, err = pdb.ArchiveProject(proj.ID)
	}
	if err != nil {
		return nil, err
	}
	return projectsList(ctx, rid, params, pdb)
}

// Hard deletes a project
func projectsDelete(ctx context.Context, rid any, params map[string]any, pdb *tools.ProjectsDB) (map[string]any, error) {
	proj, err := requireProject(pdb, params)
	if err != nil {
		return nil, err
	}
	if _, err := pdb.DeleteProject(proj.ID); err != nil {
		return nil, err
	}
	return projectsList(ctx, rid, params, pdb)
}

// Sets the active project, clearing it when no id is given
func projectsSetActive(ctx context.Context, rid any, params map[string]any, pdb *tools.ProjectsDB) (map[string]any, error) {
	var target *string
	if truthy(params["id"]) {
		proj, err := requireProject(pdb, params)
		if err != nil {
			return nil, err
		}
		target = &proj.ID
	}
	if err := pdb.SetActive(target); err != nil {
		return nil, err
	}
	active, err := activeProjectID(pdb)
	if err != nil {
		return nil, err
	}
	return ok(rid, map[string]any{"active_id": active}), nil
}

// Project owning the resolved cwd with that cwd and its git branch
func projectsForCwd(ctx context.Context, rid any, params map[string]any, pdb *tools.ProjectsDB) (map[string]any, error) {
	request := map[string]any{}
	if truthy(params["cwd"]) {
		request["cwd"] = pystr.Strip(strOr(params["cwd"]))
	}
	cwd, err := completionCwd(ctx, request)
	if err != nil {
		return nil, err
	}
	proj, err := pdb.ProjectForPath(cwd, false)
	if err != nil {
		return nil, err
	}
	var row any
	if proj != nil {
		row = proj.AsMap()
	}
	return ok(rid, map[string]any{"project": row, "cwd": cwd, "branch": tools.GitProbeBranch(cwd)}), nil
}
