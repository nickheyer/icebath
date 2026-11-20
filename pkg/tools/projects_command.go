package tools

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
)

// Points a bound kanban board's default workdir at a project's primary repo, installed by cmd/icebath over pkg/kanban
var ProjectBoardWorkdirBinder func(ctx context.Context, board, primaryPath string) error

// Subcommand handlers of icebath project keyed by action
type projectHandler func(ctx context.Context, env *Env, ns map[string]any, w, errw io.Writer) (int, error)

var projectHandlers = map[string]projectHandler{
	"create": projectCmdCreate, "list": projectCmdList, "ls": projectCmdList, "show": projectCmdShow, "add-folder": projectCmdAddFolder,
	"remove-folder": projectCmdRemoveFolder, "rename": projectCmdRename, "set-primary": projectCmdSetPrimary, "use": projectCmdUse,
	"archive": projectCmdArchive, "restore": projectCmdRestore, "bind-board": projectCmdBindBoard,
}

// Entry point of icebath project, the bare command printing its usage
func ProjectsCommand(ctx context.Context, env *Env, ns map[string]any, w, errw io.Writer) (int, error) {
	action := nsString(ns, "project_action")
	if action == "" {
		fmt.Fprintln(w, "usage: icebath project <action> [options]\nRun 'icebath project --help' for the full list.")
		return 0, nil
	}
	handler, ok := projectHandlers[action]
	if !ok {
		fmt.Fprintf(errw, "Unknown project action: %s\n", action)
		return 1, nil
	}
	return handler(ctx, env, ns, w, errw)
}

// Prints project: message on stderr and reports exit 1
func projectErr(errw io.Writer, message string) (int, error) {
	fmt.Fprintf(errw, "project: %s\n", message)
	return 1, nil
}

// Opens the profile projects database and runs fn, a text result printed with exit 0 and a value error exit 2
func projectDBCommand(ctx context.Context, env *Env, w, errw io.Writer, fn func(db *ProjectsDB) (string, int, error)) (int, error) {
	db, err := OpenProjectsDB(WithEnvHome(ctx, env), env)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	text, rc, err := fn(db)
	if err != nil {
		fmt.Fprintf(errw, "project: %s\n", err.Error())
		return 2, nil
	}
	if text != "" {
		fmt.Fprintln(w, text)
		return 0, nil
	}
	return rc, nil
}

// Like projectDBCommand with the project of the namespace resolved first, exit 1 when it does not exist
func projectWithProject(ctx context.Context, env *Env, ns map[string]any, w, errw io.Writer, fn func(db *ProjectsDB, proj *Project) (string, int, error)) (int, error) {
	return projectDBCommand(WithEnvHome(ctx, env), env, w, errw, func(db *ProjectsDB) (string, int, error) {
		ident := nsString(ns, "project")
		proj, err := db.GetProject(ident)
		if err != nil {
			return "", 0, err
		}
		if proj == nil {
			fmt.Fprintf(errw, "project: no such project: %s\n", ident)
			return "", 1, nil
		}
		return fn(db, proj)
	})
}

// Multi line description of a project with its folders
func printProject(w io.Writer, proj *Project) {
	archived := ""
	if proj.Archived {
		archived = " (archived)"
	}
	fmt.Fprintf(w, "%s  [%s]%s\n", proj.Slug, proj.ID, archived)
	fmt.Fprintf(w, "  name:    %s\n", proj.Name)
	for _, row := range [][2]string{{"about", derefString(proj.Description)}, {"board", derefString(proj.BoardSlug)}, {"primary", derefString(proj.PrimaryPath)}} {
		if row[1] != "" {
			fmt.Fprintf(w, "  %s:%s%s\n", row[0], strings.Repeat(" ", 8-len(row[0])), row[1])
		}
	}
	if len(proj.Folders) > 0 {
		fmt.Fprintln(w, "  folders:")
		for _, f := range proj.Folders {
			marker := "  "
			if f.IsPrimary {
				marker = " *"
			}
			label := ""
			if f.Label != nil && *f.Label != "" {
				label = fmt.Sprintf(" (%s)", *f.Label)
			}
			fmt.Fprintf(w, "   %s %s%s\n", marker, f.Path, label)
		}
	}
}

// String of an optional text, empty for None
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Optional text of a namespace value, nil for None
func nsOptString(ns map[string]any, key string) *string {
	if ns[key] == nil {
		return nil
	}
	s := nsString(ns, key)
	return &s
}

func projectCmdCreate(ctx context.Context, env *Env, ns map[string]any, w, errw io.Writer) (int, error) {
	return projectDBCommand(WithEnvHome(ctx, env), env, w, errw, func(db *ProjectsDB) (string, int, error) {
		pid, err := db.CreateProject(nsString(ns, "name"), CreateProjectOptions{Slug: nsString(ns, "slug"), Folders: nsStringList(ns, "folders"),
			PrimaryPath: nsString(ns, "primary"), Description: nsOptString(ns, "description"), Icon: nsOptString(ns, "icon"),
			Color: nsOptString(ns, "color"), BoardSlug: nsString(ns, "board")})
		if err != nil {
			return "", 0, err
		}
		if config.PyTruthy(ns["use"]) {
			if err := db.SetActive(&pid); err != nil {
				return "", 0, err
			}
		}
		proj, err := db.GetProject(pid)
		if err != nil {
			return "", 0, err
		}
		if proj == nil {
			fmt.Fprintln(errw, "project: vanished after create")
			return "", 2, nil
		}
		fmt.Fprintf(w, "Created project %s (%s)\n", proj.Slug, pid)
		printProject(w, proj)
		return "", 0, nil
	})
}

func projectCmdList(ctx context.Context, env *Env, ns map[string]any, w, errw io.Writer) (int, error) {
	return projectDBCommand(WithEnvHome(ctx, env), env, w, errw, func(db *ProjectsDB) (string, int, error) {
		active, err := db.GetActiveID()
		if err != nil {
			return "", 0, err
		}
		projs, err := db.ListProjects(config.PyTruthy(ns["include_archived"]))
		if err != nil {
			return "", 0, err
		}
		if len(projs) == 0 {
			return "No projects yet. Create one with `icebath project create <name>`.", 0, nil
		}
		for _, p := range projs {
			flags := ""
			if p.Archived {
				flags = " (archived)"
			}
			marker := " "
			if p.ID == active {
				marker = "*"
			}
			fmt.Fprintf(w, "%s %-24s %s%s  [%d folder(s)]\n", marker, p.Slug, p.Name, flags, len(p.Folders))
		}
		return "", 0, nil
	})
}

func projectCmdShow(ctx context.Context, env *Env, ns map[string]any, w, errw io.Writer) (int, error) {
	return projectWithProject(WithEnvHome(ctx, env), env, ns, w, errw, func(db *ProjectsDB, proj *Project) (string, int, error) {
		printProject(w, proj)
		return "", 0, nil
	})
}

func projectCmdAddFolder(ctx context.Context, env *Env, ns map[string]any, w, errw io.Writer) (int, error) {
	return projectWithProject(WithEnvHome(ctx, env), env, ns, w, errw, func(db *ProjectsDB, proj *Project) (string, int, error) {
		path, err := db.AddFolder(proj.ID, nsString(ns, "path"), nsOptString(ns, "label"), config.PyTruthy(ns["primary"]))
		if err != nil {
			return "", 0, err
		}
		return fmt.Sprintf("Added %s to %s", path, proj.Slug), 0, nil
	})
}

func projectCmdRemoveFolder(ctx context.Context, env *Env, ns map[string]any, w, errw io.Writer) (int, error) {
	return projectWithProject(WithEnvHome(ctx, env), env, ns, w, errw, func(db *ProjectsDB, proj *Project) (string, int, error) {
		path := nsString(ns, "path")
		removed, err := db.RemoveFolder(proj.ID, path)
		if err != nil {
			return "", 0, err
		}
		if !removed {
			rc, _ := projectErr(errw, "folder not in project: "+path)
			return "", rc, nil
		}
		return fmt.Sprintf("Removed %s from %s", path, proj.Slug), 0, nil
	})
}

func projectCmdRename(ctx context.Context, env *Env, ns map[string]any, w, errw io.Writer) (int, error) {
	return projectWithProject(WithEnvHome(ctx, env), env, ns, w, errw, func(db *ProjectsDB, proj *Project) (string, int, error) {
		name := nsString(ns, "name")
		if _, err := db.UpdateProject(proj.ID, UpdateProjectFields{Name: &name}); err != nil {
			return "", 0, err
		}
		return fmt.Sprintf("Renamed %s -> %s", proj.Slug, name), 0, nil
	})
}

func projectCmdSetPrimary(ctx context.Context, env *Env, ns map[string]any, w, errw io.Writer) (int, error) {
	return projectWithProject(WithEnvHome(ctx, env), env, ns, w, errw, func(db *ProjectsDB, proj *Project) (string, int, error) {
		path := nsString(ns, "path")
		ok, err := db.SetPrimary(proj.ID, path)
		if err != nil {
			return "", 0, err
		}
		if !ok {
			rc, _ := projectErr(errw, fmt.Sprintf("'%s' is not a folder of %s; add it first with `icebath project add-folder`.", path, proj.Slug))
			return "", rc, nil
		}
		return fmt.Sprintf("Set primary of %s -> %s", proj.Slug, path), 0, nil
	})
}

func projectCmdUse(ctx context.Context, env *Env, ns map[string]any, w, errw io.Writer) (int, error) {
	return projectDBCommand(WithEnvHome(ctx, env), env, w, errw, func(db *ProjectsDB) (string, int, error) {
		ident := nsString(ns, "project")
		if ident == "" {
			if err := db.SetActive(nil); err != nil {
				return "", 0, err
			}
			return "Cleared active project", 0, nil
		}
		proj, err := db.GetProject(ident)
		if err != nil {
			return "", 0, err
		}
		if proj == nil {
			fmt.Fprintf(errw, "project: no such project: %s\n", ident)
			return "", 1, nil
		}
		if err := db.SetActive(&proj.ID); err != nil {
			return "", 0, err
		}
		return "Active project: " + proj.Slug, 0, nil
	})
}

// Archive or restore handler reporting verb and slug
func projectCmdFlagCommand(op func(db *ProjectsDB, id string) (bool, error), verb string) projectHandler {
	return func(ctx context.Context, env *Env, ns map[string]any, w, errw io.Writer) (int, error) {
		return projectWithProject(WithEnvHome(ctx, env), env, ns, w, errw, func(db *ProjectsDB, proj *Project) (string, int, error) {
			if _, err := op(db, proj.ID); err != nil {
				return "", 0, err
			}
			return verb + " " + proj.Slug, 0, nil
		})
	}
}

var projectCmdArchive = projectCmdFlagCommand(func(db *ProjectsDB, id string) (bool, error) { return db.ArchiveProject(id) }, "Archived")

var projectCmdRestore = projectCmdFlagCommand(func(db *ProjectsDB, id string) (bool, error) { return db.RestoreProject(id) }, "Restored")

func projectCmdBindBoard(ctx context.Context, env *Env, ns map[string]any, w, errw io.Writer) (int, error) {
	return projectWithProject(WithEnvHome(ctx, env), env, ns, w, errw, func(db *ProjectsDB, proj *Project) (string, int, error) {
		board := nsString(ns, "board")
		if _, err := db.UpdateProject(proj.ID, UpdateProjectFields{BoardSlug: &board}); err != nil {
			return "", 0, err
		}
		if strings.TrimSpace(board) == "" {
			return "Unbound board from " + proj.Slug, 0, nil
		}
		if primary := derefString(proj.PrimaryPath); primary != "" {
			if ProjectBoardWorkdirBinder == nil {
				panic("tools: ProjectBoardWorkdirBinder is nil, cmd/icebath must install it")
			}
			func() {
				defer func() { recover() }()
				ProjectBoardWorkdirBinder(ctx, board, primary)
			}()
		}
		return fmt.Sprintf("Bound %s -> board %s", proj.Slug, board), 0, nil
	})
}
