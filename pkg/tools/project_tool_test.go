package tools

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func projectsEnv(t *testing.T) *Env {
	t.Helper()
	return &Env{Home: t.TempDir(), Config: config.Defaults(), Values: &sync.Map{}}
}

func TestProjectsDBCreateListSwitch(t *testing.T) {
	env := projectsEnv(t)
	db, err := OpenProjectsDB(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	folder := t.TempDir()
	pid, err := db.CreateProject("My App", CreateProjectOptions{Folders: []string{folder}})
	if err != nil || !strings.HasPrefix(pid, "p_") {
		t.Fatal(pid, err)
	}
	proj, _ := db.GetProject("my-app")
	if proj == nil || proj.PrimaryPathOf() != projectNormalizePath(folder) || !proj.Folders[0].IsPrimary {
		t.Fatalf("%+v", proj)
	}
	if _, err := db.CreateProject("Dup", CreateProjectOptions{PrimaryPath: folder}); err == nil || !strings.Contains(err.Error(), "folder already belongs to project 'my-app'") {
		t.Fatal(err)
	}
	pid2, _ := db.CreateProject("My App", CreateProjectOptions{})
	if p, _ := db.GetProject(pid2); p.Slug != "my-app-2" {
		t.Fatal(p.Slug)
	}
	if _, err := db.CreateProject(" ", CreateProjectOptions{}); err == nil {
		t.Fatal("empty name")
	}
	if _, err := NormalizeProjectSlug("-bad"); err == nil || !strings.Contains(err.Error(), "invalid project slug '-bad'") {
		t.Fatal(err)
	}
	owner, _ := db.ProjectForPath(filepath.Join(folder, "src", "x.go"), false)
	if owner == nil || owner.ID != pid {
		t.Fatal("project for path")
	}
	nested := filepath.Join(folder, "nested")
	db.AddFolder(pid2, nested, nil, false)
	owner, _ = db.ProjectForPath(filepath.Join(nested, "a"), false)
	if owner == nil || owner.ID != pid2 {
		t.Fatal("innermost folder wins")
	}
	if active, _ := db.GetActiveID(); active != "" {
		t.Fatal("no active yet")
	}
	db.SetActive(&pid)
	if active, _ := db.GetActiveID(); active != pid {
		t.Fatal("active")
	}
	if ok, _ := db.ArchiveProject(pid2); !ok {
		t.Fatal("archive")
	}
	if list, _ := db.ListProjects(false); len(list) != 1 {
		t.Fatal("archived hidden")
	}
	if list, _ := db.ListProjects(true); len(list) != 2 {
		t.Fatal("archived listed on request")
	}
	if changed, _ := db.UpdateProject(pid, UpdateProjectFields{Name: strPtr("Renamed"), Icon: strPtr("")}); !changed {
		t.Fatal("update")
	}
	if p, _ := db.GetProject(pid); p.Name != "Renamed" || p.Icon != nil {
		t.Fatalf("%+v", p)
	}
	if ok, _ := db.RemoveFolder(pid, folder); !ok {
		t.Fatal("remove folder")
	}
	if p, _ := db.GetProject(pid); p.PrimaryPath != nil {
		t.Fatal("primary cleared")
	}
	if ok, _ := db.DeleteProject(pid); !ok {
		t.Fatal("delete")
	}
	if BranchNameFor(&Project{Slug: "my-app"}, "t1", "Fix Login!") != "my-app/t1-fix-login" {
		t.Fatal(BranchNameFor(&Project{Slug: "my-app"}, "t1", "Fix Login!"))
	}
	n, _ := db.RecordDiscoveredRepos([]DiscoveredRepo{{Root: folder}}, true, strPtr("v1"))
	repos, _ := db.ListDiscoveredRepos()
	if n != 1 || len(repos) != 1 || repos[0].Label != filepath.Base(folder) {
		t.Fatal(repos)
	}
	if cleared, _ := db.ReconcileDiscoveredReposPolicy("v2", false); !cleared {
		t.Fatal("policy change clears")
	}
	if repos, _ := db.ListDiscoveredRepos(); len(repos) != 0 {
		t.Fatal("cleared")
	}
}

func TestProjectToolActions(t *testing.T) {
	env := projectsEnv(t)
	folder := t.TempDir()
	out := mustJSON(t, projectCreate(context.Background(), env, "Work", folder, "task-1"))
	if out["success"] != true || out["slug"] != "work" {
		t.Fatal(out)
	}
	again := mustJSON(t, projectCreate(context.Background(), env, "Other", folder, ""))
	if again["slug"] != "work" {
		t.Fatal("create on an owned folder must reuse the project")
	}
	if r := projectCreate(context.Background(), env, "", "", ""); !strings.Contains(r, "name is required") {
		t.Fatal(r)
	}
	list := mustJSON(t, projectList(context.Background(), env, ""))
	rows := list["projects"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["active"] != true || list["active_id"] == nil {
		t.Fatal(list)
	}
	if r := projectSwitch(context.Background(), env, "nope", ""); !strings.Contains(r, "no project matching 'nope'") {
		t.Fatal(r)
	}
	moved := ""
	SetProjectWorkspaceCallback(func(taskID, path, name string) { moved = taskID + ":" + name })
	t.Cleanup(func() { SetProjectWorkspaceCallback(nil) })
	sw := mustJSON(t, projectSwitch(context.Background(), env, "WORK", "task-2"))
	if sw["success"] != true || moved != "task-2:Work" {
		t.Fatal(sw, moved)
	}
	if r := projectFail("x"); !strings.Contains(r, `"success":false`) {
		t.Fatal(r)
	}
}
