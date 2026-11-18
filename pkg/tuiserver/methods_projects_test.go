package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Work dir beside a fresh launch home with its own shared session store and a git probe reading .git directories
func projectsHome(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	dbMu.Lock()
	savedDB, savedErr := sharedDB, dbError
	sharedDB, dbError = nil, ""
	dbMu.Unlock()
	restore := tools.SwapGitProbeRunner(func(cwd string, _ ...string) string {
		d := cwd
		for d != "" && d != "/" && d != filepath.Dir(d) {
			if isDir(filepath.Join(d, ".git")) {
				return d
			}
			d = filepath.Dir(d)
		}
		return ""
	})
	t.Cleanup(func() {
		restore()
		dbMu.Lock()
		if sharedDB != nil {
			_ = sharedDB.Close()
		}
		sharedDB, dbError = savedDB, savedErr
		dbMu.Unlock()
	})
	return dir, t.TempDir()
}

// Directory created with its parents
func mkdirAll(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProjectInfoForCwdReturnsStatusPayload(t *testing.T) {
	_, tmp := projectsHome(t)
	folder := mkdirAll(t, filepath.Join(tmp, "repo"))
	id := seedProject(t, "Repo", folder)
	nested := mkdirAll(t, filepath.Join(folder, "src"))
	wantEqual(t, projectInfoForCwd(context.Background(), nested),
		map[string]any{"id": id, "slug": "repo", "name": "Repo", "primary_path": folder})
}

func TestSessionInfoCarriesProjectForOwnedCwd(t *testing.T) {
	_, tmp := projectsHome(t)
	folder := mkdirAll(t, filepath.Join(tmp, "proj"))
	seedProject(t, "Proj", folder)
	info := sessionInfo(context.Background(), nil, tuihost.NewSession(map[string]any{"cwd": folder, "session_key": "s1"}))
	project := info["project"].(map[string]any)
	wantEqual(t, project, map[string]any{"id": project["id"], "slug": "proj", "name": "Proj", "primary_path": folder})
	wantEqual(t, project["name"], "Proj")
}

// Fails unless got deep equals want
func wantEqual(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// Project stored with one folder in the projects database of the test home, its id
func seedProject(t *testing.T, name, folder string) string {
	t.Helper()
	db, err := tools.OpenProjectsDB(context.Background(), &tools.Env{Home: os.Getenv("ICEBATH_HOME")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	id, err := db.CreateProject(name, tools.CreateProjectOptions{Folders: []string{folder}})
	if err != nil {
		t.Fatal(err)
	}
	return id
}
