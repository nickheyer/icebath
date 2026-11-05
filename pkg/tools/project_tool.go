package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// ProjectWorkspaceCallback re anchors a live GUI session's workspace to a project path, installed by the GUI gateway
type ProjectWorkspaceCallback func(taskID, primaryPath, projectName string)

var (
	projectWorkspaceMu sync.RWMutex
	projectWorkspaceCb ProjectWorkspaceCallback
)

// SetProjectWorkspaceCallback installs or clears the session workspace mover
func SetProjectWorkspaceCallback(fn ProjectWorkspaceCallback) {
	projectWorkspaceMu.Lock()
	projectWorkspaceCb = fn
	projectWorkspaceMu.Unlock()
}

// The installed workspace mover, nil in CLI and messaging contexts
func projectWorkspaceCallback() ProjectWorkspaceCallback {
	projectWorkspaceMu.RLock()
	defer projectWorkspaceMu.RUnlock()
	return projectWorkspaceCb
}

// True when a live GUI session's workspace will follow the project
func projectMovesSession(taskID, path string) bool {
	return projectWorkspaceCallback() != nil && taskID != "" && path != ""
}

// Moves the session workspace when a mover is installed and the project has a path
func projectApplyWorkspace(taskID, path, name string) {
	cb := projectWorkspaceCallback()
	if cb != nil && taskID != "" && path != "" {
		func() {
			defer func() { recover() }()
			cb(taskID, path, name)
		}()
	}
}

// Project matching the token by exact id, slug or name, then case insensitive slug or name
func projectResolve(db *ProjectsDB, token string) (*Project, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, nil
	}
	projects, err := db.ListProjects(true)
	if err != nil {
		return nil, err
	}
	for _, proj := range projects {
		if token == proj.ID || token == proj.Slug || proj.Name == token {
			return proj, nil
		}
	}
	low := strings.ToLower(token)
	for _, proj := range projects {
		if strings.ToLower(proj.Slug) == low || strings.ToLower(proj.Name) == low {
			return proj, nil
		}
	}
	return nil, nil
}

// Success JSON after moving the session into the project
func projectActivated(proj *Project, taskID string) string {
	primary := proj.PrimaryPathOf()
	projectApplyWorkspace(taskID, primary, proj.Name)
	return jsonCompact(map[string]any{"success": true, "id": proj.ID, "slug": proj.Slug, "name": proj.Name, "primary_path": emptyAsNull(primary)})
}

// Scoped flag and project id of the calling session, unscoped callers fall back to the profile pointer
func projectCallingSessionID(env *Env, db *ProjectsDB, taskID string) (bool, string, error) {
	overrides := map[string]any{}
	if taskID != "" {
		if o, err := ResolveTaskOverrides(env, taskID); err == nil && o != nil {
			overrides = o
		}
	}
	source, ok := overrides["cwd_source"]
	if !ok {
		return false, "", nil
	}
	var cwd string
	if source == "session" {
		cwd, _ = overrides["cwd"].(string)
	} else {
		cwd, _ = overrides["host_workspace"].(string)
	}
	if cwd == "" {
		return true, "", nil
	}
	proj, err := db.ProjectForPath(cwd, false)
	if err != nil || proj == nil {
		return true, "", err
	}
	return true, proj.ID, nil
}

// Failure JSON with the error text
func projectFail(msg string) string {
	return jsonCompact(map[string]any{"success": false, "error": msg})
}

// Lists projects and which one the calling chat is in
func projectList(env *Env, taskID string) string {
	db, err := OpenProjectsDB(env)
	if err != nil {
		return projectFail(err.Error())
	}
	defer db.Close()
	scoped, active, err := projectCallingSessionID(env, db, taskID)
	if err != nil {
		return projectFail(err.Error())
	}
	if !scoped {
		if active, err = db.GetActiveID(); err != nil {
			return projectFail(err.Error())
		}
	}
	projects, err := db.ListProjects(false)
	if err != nil {
		return projectFail(err.Error())
	}
	rows := make([]any, 0, len(projects))
	for _, p := range projects {
		rows = append(rows, map[string]any{"id": p.ID, "slug": p.Slug, "name": p.Name, "primary_path": emptyAsNull(p.PrimaryPathOf()), "active": p.ID == active})
	}
	return jsonCompact(map[string]any{"active_id": emptyAsNull(active), "projects": rows})
}

// Creates a project, reusing one that already owns the folder, and switches the chat into it
func projectCreate(env *Env, name, path, taskID string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return projectFail("name is required")
	}
	folder := strings.TrimSpace(path)
	if folder != "" {
		folder = projectNormalizePath(folder)
	}
	db, err := OpenProjectsDB(env)
	if err != nil {
		return projectFail(err.Error())
	}
	defer db.Close()
	var proj *Project
	if folder != "" {
		if proj, err = db.FindByPrimaryPath(folder, false); err != nil {
			return projectFail(err.Error())
		}
	}
	if proj == nil {
		var folders []string
		if folder != "" {
			folders = []string{folder}
		}
		pid, err := db.CreateProject(name, CreateProjectOptions{Folders: folders, PrimaryPath: folder})
		if err != nil {
			return projectFail(err.Error())
		}
		if proj, err = db.GetProject(pid); err != nil {
			return projectFail(err.Error())
		}
	}
	if proj == nil {
		return projectFail("project vanished after create")
	}
	if !projectMovesSession(taskID, proj.PrimaryPathOf()) {
		id := proj.ID
		if err := db.SetActive(&id); err != nil {
			return projectFail(err.Error())
		}
	}
	return projectActivated(proj, taskID)
}

// Moves the chat into an existing project by name, slug or id
func projectSwitch(env *Env, project, taskID string) string {
	db, err := OpenProjectsDB(env)
	if err != nil {
		return projectFail(err.Error())
	}
	defer db.Close()
	proj, err := projectResolve(db, project)
	if err != nil {
		return projectFail(err.Error())
	}
	if proj == nil {
		return projectFail(fmt.Sprintf("no project matching '%s'", project))
	}
	if !projectMovesSession(taskID, proj.PrimaryPathOf()) {
		id := proj.ID
		if err := db.SetActive(&id); err != nil {
			return projectFail(err.Error())
		}
	}
	return projectActivated(proj, taskID)
}

// Dispatches the desktop_project action
func handleProject(ctx context.Context, call *Call) string {
	switch strings.TrimSpace(argS(call.Args, "action")) {
	case "list":
		return projectList(call.Env, call.TaskID)
	case "create":
		return projectCreate(call.Env, argS(call.Args, "name"), argS(call.Args, "path"), call.TaskID)
	case "switch":
		return projectSwitch(call.Env, argS(call.Args, "name"), call.TaskID)
	}
	return projectFail("action must be one of: create, switch, list.")
}

var projectSchema = map[string]any{
	"name": "desktop_project",
	"description": "Create or switch desktop Projects (named workspaces). create: one and switch " +
		"this chat into it — pass path to anchor it to a repo/folder (the " +
		"chat's workspace moves there, the sidebar follows). switch: move " +
		"this chat into an existing project by name/slug/id — the " +
		"intentional way to move the session, not `cd`. list: all projects + which one this chat is in.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{"type": "string", "enum": []any{"create", "switch", "list"}},
			"name":   map[string]any{"type": "string", "description": "create: human name. switch: name, slug, or id."},
			"path":   map[string]any{"type": "string", "description": "create: repo/folder to anchor to."},
		},
		"required": []any{"action"},
	},
}

// Registers the desktop_project tool of the GUI only project toolset
func registerProjectTools() {
	Register(Def{Name: "desktop_project", Toolset: "project", Schema: projectSchema, Handler: handleProject})
}
