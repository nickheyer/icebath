package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/state"
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

// Directory that the fake git probe and the disk scan both read as a repository
func fakeGitRepo(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Directory created with its parents
func mkdirAll(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Writes a config.yaml into the home
func writeHomeConfig(t *testing.T, h, text string) {
	t.Helper()
	writeTestFile(t, filepath.Join(h, "config.yaml"), text)
}

// Roots of the discovered repo cache of the active home
func cachedRoots(t *testing.T) []string {
	t.Helper()
	pdb, err := openProjectsDB(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer pdb.Close()
	rows, err := pdb.ListDiscoveredRepos()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r.Root)
	}
	return out
}

// Records repos into the active home cache the way projects_db.record_discovered_repos does
func seedDiscovered(t *testing.T, root, label string) {
	t.Helper()
	pdb, err := openProjectsDB(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer pdb.Close()
	if _, err := pdb.RecordDiscoveredRepos([]tools.DiscoveredRepo{{Root: root, Label: label}}, false, nil); err != nil {
		t.Fatal(err)
	}
}

// Session row in the shared launch store with a cwd
func createLaunchSession(t *testing.T, id, cwd string) {
	t.Helper()
	db := getDB(context.Background())
	if db == nil {
		t.Fatal("no session store")
	}
	if err := db.CreateSession(state.NewSession{ID: id, Source: "cli", Cwd: cwd}); err != nil {
		t.Fatal(err)
	}
}

// Discovered repo rows of a result keyed by root
func reposByKey(result map[string]any, key string) map[any]map[string]any {
	out := map[any]map[string]any{}
	for _, r := range dictList(result["repos"]) {
		out[r[key]] = r
	}
	return out
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

func TestRecordReposPersistsAndShowsZeroSessionRepo(t *testing.T) {
	_, tmp := projectsHome(t)
	repo := mkdirAll(t, filepath.Join(tmp, "fresh-repo"))
	projectsCall(t, "projects.record_repos", map[string]any{"repos": []any{map[string]any{"root": repo, "label": "fresh-repo"}}})
	byLabel := reposByKey(projectsCall(t, "projects.discover_repos", nil), "label")
	fresh, present := byLabel["fresh-repo"]
	if !present {
		t.Fatalf("fresh-repo missing from %v", byLabel)
	}
	wantEqual(t, fresh["sessions"], int64(0))
}

func TestScanTimeIsNotTreatedAsSessionActivity(t *testing.T) {
	_, tmp := projectsHome(t)
	workedIn := fakeGitRepo(t, filepath.Join(tmp, "worked-in"))
	createLaunchSession(t, "worked-in-session", workedIn)
	neverOpened := mkdirAll(t, filepath.Join(tmp, "never-opened"))
	projectsCall(t, "projects.record_repos", map[string]any{"repos": []any{map[string]any{"root": neverOpened}, map[string]any{"root": workedIn}}})
	byRoot := reposByKey(projectsCall(t, "projects.discover_repos", nil), "root")
	idle, active := byRoot[neverOpened], byRoot[workedIn]
	wantEqual(t, idle["sessions"], int64(0))
	wantEqual(t, idle["last_active"], 0.0)
	if !(active["last_active"].(float64) > idle["last_active"].(float64)) {
		t.Fatalf("active %v not ahead of idle %v", active, idle)
	}
}

func TestRemoteScanFailureMergesInsteadOfReplacingCache(t *testing.T) {
	_, tmp := projectsHome(t)
	seed := fakeGitRepo(t, filepath.Join(tmp, "elsewhere", "seed-repo"))
	seedDiscovered(t, seed, "seed-repo")
	if !slices.Contains(cachedRoots(t), seed) {
		t.Fatal("seed not cached")
	}
	good := fakeGitRepo(t, filepath.Join(tmp, "good-repo"))
	badRoot := filepath.Join(tmp, "unwalkable")
	policy := map[string]any{"enabled": true, "roots": []any{good, badRoot}, "exclude_paths": []any{}}
	pdb, err := openProjectsDB(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	authoritative := scanDiscoveredReposRemote(context.Background(), pdb, policy)
	pdb.Close()
	joined := cachedRoots(t)
	if authoritative {
		t.Fatal("partial scan reported authoritative")
	}
	if !slices.Contains(joined, good) || !slices.Contains(joined, seed) {
		t.Fatalf("joined %v", joined)
	}
}

func TestRemoteScanMissingRootDoesNotWipeCache(t *testing.T) {
	_, tmp := projectsHome(t)
	seed := fakeGitRepo(t, filepath.Join(tmp, "elsewhere", "seed-repo"))
	seedDiscovered(t, seed, "seed-repo")
	if !slices.Contains(cachedRoots(t), seed) {
		t.Fatal("seed not cached")
	}
	good := fakeGitRepo(t, filepath.Join(tmp, "good-repo"))
	policy := map[string]any{"enabled": true, "roots": []any{good, filepath.Join(tmp, "missing-root")}, "exclude_paths": []any{}}
	pdb, err := openProjectsDB(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	authoritative := scanDiscoveredReposRemote(context.Background(), pdb, policy)
	pdb.Close()
	joined := cachedRoots(t)
	if authoritative {
		t.Fatal("scan with a missing root reported authoritative")
	}
	if !slices.Contains(joined, good) || !slices.Contains(joined, seed) {
		t.Fatalf("joined %v", joined)
	}
}

func TestRemoteScanFullAuthoritativeReplacesCache(t *testing.T) {
	_, tmp := projectsHome(t)
	stale := fakeGitRepo(t, filepath.Join(tmp, "outside", "stale-repo"))
	scandir := mkdirAll(t, filepath.Join(tmp, "scandir"))
	fresh := fakeGitRepo(t, filepath.Join(scandir, "fresh-repo"))
	seedDiscovered(t, stale, "stale-repo")
	policy := map[string]any{"enabled": true, "roots": []any{scandir}, "exclude_paths": []any{}}
	pdb, err := openProjectsDB(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	authoritative := scanDiscoveredReposRemote(context.Background(), pdb, policy)
	pdb.Close()
	joined := cachedRoots(t)
	if !authoritative {
		t.Fatal("full scan not authoritative")
	}
	if !slices.Contains(joined, fresh) || slices.Contains(joined, stale) {
		t.Fatalf("joined %v", joined)
	}
}

func TestHomeContainerDirsAreNeverAWorkspace(t *testing.T) {
	_, tmp := projectsHome(t)
	ctx := context.Background()
	userHome := realpathLax(pypath.Expanduser("~"))
	for _, path := range []string{string(os.PathSeparator), userHome, filepath.Dir(userHome), "/home", "/Users"} {
		if !isSessionCwdJunk(ctx, path) || !isRepoJunk(ctx, path) {
			t.Fatalf("%s is a workspace", path)
		}
	}
	workspace := mkdirAll(t, filepath.Join(tmp, "a-repo"))
	if isSessionCwdJunk(ctx, workspace) || isRepoJunk(ctx, workspace) {
		t.Fatal("ordinary directory treated as junk")
	}
}

func TestDisabledDiscoveryClearsCacheAndRejectsNewScan(t *testing.T) {
	h, tmp := projectsHome(t)
	repo := mkdirAll(t, filepath.Join(tmp, "cached-repo"))
	sessionRepo := fakeGitRepo(t, filepath.Join(tmp, "session-repo"))
	createLaunchSession(t, "session-repo", sessionRepo)
	projectsCall(t, "projects.record_repos", map[string]any{"repos": []any{map[string]any{"root": repo}}})
	writeHomeConfig(t, h, "desktop:\n  repo_scan_enabled: false\n  repo_scan_roots: []\n  repo_scan_exclude_paths: []\n")
	result := projectsCall(t, "projects.record_repos", map[string]any{"repos": []any{map[string]any{"root": repo}},
		"discovery_policy": map[string]any{"enabled": false, "roots": []any{}, "exclude_paths": []any{}}})
	wantEqual(t, result["accepted"], false)
	byRoot := reposByKey(result, "root")
	if _, present := byRoot[repo]; present {
		t.Fatal("cleared repo still listed")
	}
	if _, present := byRoot[sessionRepo]; !present {
		t.Fatalf("session repo missing from %v", byRoot)
	}
}

func TestNondefaultPolicyRejectsStaleOrLegacyResults(t *testing.T) {
	h, tmp := projectsHome(t)
	root := mkdirAll(t, filepath.Join(tmp, "allowed"))
	policy := map[string]any{"enabled": true, "roots": []any{root}, "exclude_paths": []any{}}
	writeHomeConfig(t, h, "desktop:\n  repo_scan_enabled: true\n  repo_scan_roots: ['"+root+"']\n  repo_scan_exclude_paths: []\n")
	legacy := projectsCall(t, "projects.record_repos", map[string]any{"repos": []any{map[string]any{"root": root}}})
	stale := projectsCall(t, "projects.record_repos", map[string]any{"repos": []any{map[string]any{"root": root}},
		"discovery_policy": map[string]any{"enabled": true, "roots": []any{filepath.Join(tmp, "other")}, "exclude_paths": []any{}}})
	accepted := projectsCall(t, "projects.record_repos", map[string]any{"repos": []any{map[string]any{"root": root}}, "discovery_policy": policy})
	wantEqual(t, legacy["accepted"], false)
	wantEqual(t, stale["accepted"], false)
	wantEqual(t, accepted["accepted"], true)
	if _, present := reposByKey(accepted, "root")[root]; !present {
		t.Fatal("accepted repo missing")
	}
}
