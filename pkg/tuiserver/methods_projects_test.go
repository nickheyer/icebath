package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
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

// Result of a projects call, failing the case on an error response
func projectsCall(t *testing.T, method string, params map[string]any) map[string]any {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	resp := callMethod(t, method, params)
	if errObj, failed := resp["error"]; failed {
		t.Fatalf("%s failed: %v", method, errObj)
	}
	return resultOf(t, resp)
}

// Directory created with its parents
func mkdirAll(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCreateListRoundtrip(t *testing.T) {
	_, tmp := projectsHome(t)
	created := projectsCall(t, "projects.create", map[string]any{"name": "Demo", "folders": []any{tmp}, "use": true})
	project := created["project"].(map[string]any)
	wantEqual(t, project["slug"], "demo")
	listing := projectsCall(t, "projects.list", nil)
	var slugs []any
	for _, p := range dictList(listing["projects"]) {
		slugs = append(slugs, p["slug"])
	}
	wantEqual(t, slugs, []any{"demo"})
	wantEqual(t, listing["active_id"], project["id"])
}

func TestAddFolderAndForCwd(t *testing.T) {
	_, tmp := projectsHome(t)
	folder := mkdirAll(t, filepath.Join(tmp, "repo"))
	pid := projectsCall(t, "projects.create", map[string]any{"name": "Repo", "folders": []any{folder}})["project"].(map[string]any)["id"]
	nested := mkdirAll(t, filepath.Join(folder, "src"))
	resolved := projectsCall(t, "projects.for_cwd", map[string]any{"cwd": nested})
	wantEqual(t, resolved["project"].(map[string]any)["id"], pid)
	if _, present := resolved["branch"]; !present {
		t.Fatal("branch key missing")
	}
}

func TestProjectInfoForCwdReturnsStatusPayload(t *testing.T) {
	_, tmp := projectsHome(t)
	folder := mkdirAll(t, filepath.Join(tmp, "repo"))
	created := projectsCall(t, "projects.create", map[string]any{"name": "Repo", "folders": []any{folder}})["project"].(map[string]any)
	nested := mkdirAll(t, filepath.Join(folder, "src"))
	wantEqual(t, projectInfoForCwd(context.Background(), nested),
		map[string]any{"id": created["id"], "slug": "repo", "name": "Repo", "primary_path": folder})
}

func TestSessionInfoCarriesProjectForOwnedCwd(t *testing.T) {
	_, tmp := projectsHome(t)
	folder := mkdirAll(t, filepath.Join(tmp, "proj"))
	projectsCall(t, "projects.create", map[string]any{"name": "Proj", "folders": []any{folder}})
	info := sessionInfo(context.Background(), nil, tuigateway.NewSession(map[string]any{"cwd": folder, "session_key": "s1"}))
	project := info["project"].(map[string]any)
	wantEqual(t, project, map[string]any{"id": project["id"], "slug": "proj", "name": "Proj", "primary_path": folder})
	wantEqual(t, project["name"], "Proj")
}

func TestUpdateAndArchive(t *testing.T) {
	_, tmp := projectsHome(t)
	pid := projectsCall(t, "projects.create", map[string]any{"name": "Orig", "folders": []any{tmp}})["project"].(map[string]any)["id"]
	updated := projectsCall(t, "projects.update", map[string]any{"id": pid, "name": "Renamed"})
	wantEqual(t, updated["project"].(map[string]any)["name"], "Renamed")
	payload := projectsCall(t, "projects.archive", map[string]any{"id": pid})
	for _, p := range dictList(payload["projects"]) {
		if p["id"] == pid && !truthy(p["archived"]) {
			t.Fatalf("project not archived %v", p)
		}
	}
}

func TestGetUnknownReturnsError(t *testing.T) {
	_, _ = projectsHome(t)
	if _, failed := callMethod(t, "projects.get", map[string]any{"id": "nope"})["error"]; !failed {
		t.Fatal("expected an error")
	}
}

func TestDeleteRemovesProject(t *testing.T) {
	_, tmp := projectsHome(t)
	pid := projectsCall(t, "projects.create", map[string]any{"name": "Doomed", "folders": []any{tmp}})["project"].(map[string]any)["id"]
	for _, p := range dictList(projectsCall(t, "projects.delete", map[string]any{"id": pid})["projects"]) {
		if p["id"] == pid {
			t.Fatal("project still listed")
		}
	}
}

// Fails unless got deep equals want
func wantEqual(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// Dict rows of a list value, other entries dropped
func dictList(v any) []map[string]any {
	switch rows := v.(type) {
	case []map[string]any:
		return rows
	case []any:
		out := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			if dict, isDict := row.(map[string]any); isDict {
				out = append(out, dict)
			}
		}
		return out
	}
	return nil
}
