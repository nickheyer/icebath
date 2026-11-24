package tuiserver

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

var hubProjectsRegistry = &handlerRegistry{}

func init() {
	scopedRPC(hubProjectsRegistry, tuigateway.ProjectsList.Name(), 5190, rpcShape{}, projectsList)
	scopedRPC(hubProjectsRegistry, tuigateway.ProjectsChange.Name(), 5190, rpcShape{}, projectsChange)
	hubProjectsRegistry.install()
}

// Refusal of a projects request that cannot be carried out as asked
func projectsRefused(rid any, message string) map[string]any {
	return rpcErr(rid, 4190, message, nil)
}

// Text of an optional column, empty for none
func projectText(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Every project of the open database as the projects screen shows it, archived ones included
func projectRows(db *tools.ProjectsDB) ([]tuigateway.ProjectRow, error) {
	active, err := db.GetActiveID()
	if err != nil {
		return nil, err
	}
	projects, err := db.ListProjects(true)
	if err != nil {
		return nil, err
	}
	rows := make([]tuigateway.ProjectRow, 0, len(projects))
	for _, p := range projects {
		row := tuigateway.ProjectRow{ID: p.ID, Slug: p.Slug, Name: p.Name, Description: projectText(p.Description), Board: projectText(p.BoardSlug),
			Archived: p.Archived, Active: p.ID == active, Folders: make([]tuigateway.ProjectFolderRow, 0, len(p.Folders))}
		for _, f := range p.Folders {
			row.Folders = append(row.Folders, tuigateway.ProjectFolderRow{Path: f.Path, Label: projectText(f.Label), Primary: f.IsPrimary})
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// Projects of the profile for the projects screen
func projectsList(ctx context.Context, rid any, raw map[string]any, _ *tuihost.Session) (map[string]any, error) {
	if _, err := tuigateway.ProjectsList.Params(raw); err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	db, err := tools.OpenProjectsDB(ctx, &tools.Env{Home: home.Dir(ctx)})
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	rows, err := projectRows(db)
	if err != nil {
		return nil, err
	}
	return ok(rid, tuigateway.Plain(tuigateway.ProjectsListResult{Projects: rows})), nil
}

// Carries out one change on the open database, answering the id of the project it made or changed and why it was refused, empty when it was not
func projectChange(ctx context.Context, db *tools.ProjectsDB, p tuigateway.ProjectsChangeParams) (string, string, error) {
	if p.Verb == "create" {
		opts := tools.CreateProjectOptions{Folders: p.Folders, BoardSlug: strings.TrimSpace(p.Board)}
		if text := strings.TrimSpace(p.Description); text != "" {
			opts.Description = &text
		}
		id, err := db.CreateProject(p.Name, opts)
		if err != nil {
			return "", err.Error(), nil
		}
		if p.Use {
			if err := db.SetActive(&id); err != nil {
				return "", "", err
			}
		}
		return id, "", nil
	}
	if p.Verb == "clear" {
		return "", "", db.SetActive(nil)
	}
	project, err := db.GetProject(p.Project)
	if err != nil {
		return "", "", err
	}
	if project == nil {
		return "", "no such project: " + p.Project, nil
	}
	switch p.Verb {
	case "rename":
		name := strings.TrimSpace(p.Name)
		if name == "" {
			return "", "project name must not be empty", nil
		}
		_, err = db.UpdateProject(project.ID, tools.UpdateProjectFields{Name: &name})
	case "describe":
		text := strings.TrimSpace(p.Description)
		_, err = db.UpdateProject(project.ID, tools.UpdateProjectFields{Description: &text})
	case "add-folder":
		var label *string
		if text := strings.TrimSpace(p.Label); text != "" {
			label = &text
		}
		if _, err := db.AddFolder(project.ID, p.Folder, label, p.Primary); err != nil {
			return "", err.Error(), nil
		}
	case "remove-folder":
		removed, err := db.RemoveFolder(project.ID, p.Folder)
		if err != nil {
			return "", "", err
		}
		if !removed {
			return "", "folder not in project: " + p.Folder, nil
		}
	case "set-primary":
		set, err := db.SetPrimary(project.ID, p.Folder)
		if err != nil {
			return "", "", err
		}
		if !set {
			return "", "folder not in project: " + p.Folder, nil
		}
	case "use":
		err = db.SetActive(&project.ID)
	case "archive":
		_, err = db.ArchiveProject(project.ID)
	case "restore":
		_, err = db.RestoreProject(project.ID)
	case "bind-board":
		board := strings.TrimSpace(p.Board)
		if _, err := db.UpdateProject(project.ID, tools.UpdateProjectFields{BoardSlug: &board}); err != nil {
			return "", "", err
		}
		if primary := project.PrimaryPathOf(); board != "" && primary != "" && tools.ProjectBoardWorkdirBinder != nil {
			if err := tools.ProjectBoardWorkdirBinder(ctx, board, primary); err != nil {
				return "", err.Error(), nil
			}
		}
	default:
		return "", "unknown project change: " + p.Verb, nil
	}
	return project.ID, "", err
}

// One change to the projects of the profile, answered with the projects after it
func projectsChange(ctx context.Context, rid any, raw map[string]any, _ *tuihost.Session) (map[string]any, error) {
	p, err := tuigateway.ProjectsChange.Params(raw)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	db, err := tools.OpenProjectsDB(ctx, &tools.Env{Home: home.Dir(ctx)})
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	id, refusal, err := projectChange(ctx, db, p)
	if err != nil {
		return nil, err
	}
	if refusal != "" {
		return projectsRefused(rid, refusal), nil
	}
	rows, err := projectRows(db)
	if err != nil {
		return nil, err
	}
	return ok(rid, tuigateway.Plain(tuigateway.ProjectsChangeResult{Project: id, Projects: rows})), nil
}
