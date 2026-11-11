package tuiserver

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/text/unicode/norm"
)

var treeSID atomic.Int64

// Session row of the tree fixtures with overrides applied
func treeSession(cwd any, branch, repoRoot string, over map[string]any) map[string]any {
	row := map[string]any{"id": fmt.Sprintf("s%d", treeSID.Add(1)), "cwd": cwd, "git_branch": branch, "git_repo_root": repoRoot,
		"started_at": int64(1000), "last_active": int64(1000), "title": nil, "preview": nil, "source": "cli"}
	for key, value := range over {
		row[key] = value
	}
	return row
}

// Explicit project row whose first folder is primary
func treeProject(pid, name string, folders []string, over map[string]any) map[string]any {
	var primary any
	if len(folders) > 0 {
		primary = folders[0]
	}
	rows := []any{}
	for i, p := range folders {
		rows = append(rows, map[string]any{"path": p, "is_primary": i == 0})
	}
	row := map[string]any{"id": pid, "name": name, "primary_path": primary, "archived": false, "folders": rows}
	for key, value := range over {
		row[key] = value
	}
	return row
}

// Resolver from cwd to its repo and worktree roots
func treeResolver(mapping map[string][2]string) projectResolve {
	return func(cwd string) map[string]any {
		hit, present := mapping[cwd]
		if !present {
			return nil
		}
		return map[string]any{"repo_root": hit[0], "worktree_root": hit[1]}
	}
}

// Resolver that never finds a repo
func noRepo(string) map[string]any { return nil }

// Projects of a built tree
func treeProjects(tree map[string]any) []map[string]any { return dictList(tree["projects"]) }

// First project with the id, failing the case when absent
func treeProjectByID(t *testing.T, tree map[string]any, id string) map[string]any {
	t.Helper()
	for _, p := range treeProjects(tree) {
		if p["id"] == id {
			return p
		}
	}
	t.Fatalf("no project %s in %v", id, treeProjectIDs(tree))
	return nil
}

// Every lane of a project in order
func treeLanes(project map[string]any) []map[string]any {
	var out []map[string]any
	for _, repo := range dictList(project["repos"]) {
		out = append(out, dictList(repo["groups"])...)
	}
	return out
}

// One field of every lane of a project
func laneField(project map[string]any, key string) []any {
	var out []any
	for _, g := range treeLanes(project) {
		out = append(out, g[key])
	}
	return out
}

// Every session row of every lane of a project
func treeSessionsOf(project map[string]any) []map[string]any {
	var out []map[string]any
	for _, g := range treeLanes(project) {
		out = append(out, dictList(g["sessions"])...)
	}
	return out
}

// Home bucket, nil when nothing went unplaced
func treeHome(tree map[string]any) map[string]any {
	for _, p := range treeProjects(tree) {
		if p["id"] == noProjectID {
			return p
		}
	}
	return nil
}

// Session ids in the Home bucket
func homeSessionIDs(tree map[string]any) []any {
	home := treeHome(tree)
	if home == nil {
		return nil
	}
	var out []any
	for _, s := range treeSessionsOf(home) {
		out = append(out, s["id"])
	}
	return out
}

// Ids of every project
func treeProjectIDs(tree map[string]any) []any {
	var out []any
	for _, p := range treeProjects(tree) {
		out = append(out, p["id"])
	}
	return out
}

// Project ids without the Home bucket
func realProjectIDs(tree map[string]any) []any {
	var out []any
	for _, p := range treeProjects(tree) {
		if p["id"] != noProjectID {
			out = append(out, p["id"])
		}
	}
	return out
}

// Deep equality assertion
func wantEqual(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// Tree of the given inputs with hydrated lanes
func hydratedTree(projects, sessions []map[string]any, resolve projectResolve) map[string]any {
	return buildTree(projects, sessions, nil, resolve, treeOptions{previewLimit: 3, hydrate: true})
}

func TestMainCheckoutGroupsByRecordedBranchWithStableLaneIds(t *testing.T) {
	resolve := treeResolver(map[string][2]string{"/repo": {"/repo", "/repo"}})
	tree := hydratedTree(nil, []map[string]any{treeSession("/repo", "main", "", nil), treeSession("/repo", "feature", "", nil)}, resolve)
	project := treeProjectByID(t, tree, "/repo")
	wantEqual(t, project["isAuto"], true)
	wantEqual(t, laneField(project, "id"), []any{"/repo::branch::main", "/repo::branch::feature"})
	wantEqual(t, laneField(project, "label"), []any{"main", "feature"})
	for _, isMain := range laneField(project, "isMain") {
		wantEqual(t, isMain, true)
	}
}

func TestLinkedWorktreesFoldUnderTheirCommonRepoRoot(t *testing.T) {
	resolve := treeResolver(map[string][2]string{"/repo": {"/repo", "/repo"}, "/elsewhere/wt": {"/repo", "/elsewhere/wt"}})
	tree := hydratedTree(nil, []map[string]any{treeSession("/repo", "main", "", nil), treeSession("/elsewhere/wt", "feature", "", nil)}, resolve)
	wantEqual(t, treeProjectIDs(tree), []any{"/repo"})
	project := treeProjects(tree)[0]
	wantEqual(t, dictList(project["repos"])[0]["id"], "/repo")
	if !slices.Contains(laneField(project, "id"), any("/repo::branch::main")) {
		t.Fatal("main lane missing")
	}
	for _, g := range treeLanes(project) {
		if g["isMain"] == false {
			wantEqual(t, g["id"], "/elsewhere/wt")
			wantEqual(t, g["path"], "/elsewhere/wt")
			return
		}
	}
	t.Fatal("no linked lane")
}

func TestOverviewOrdersLanesByRecencyNotAlphabetically(t *testing.T) {
	resolve := treeResolver(map[string][2]string{"/repo": {"/repo", "/repo"}, "/wt-aaa": {"/repo", "/wt-aaa"}, "/wt-zzz": {"/repo", "/wt-zzz"}})
	sessions := []map[string]any{
		treeSession("/repo", "main", "", map[string]any{"last_active": int64(5000)}),
		treeSession("/wt-aaa", "", "", map[string]any{"last_active": int64(1000)}),
		treeSession("/wt-zzz", "", "", map[string]any{"last_active": int64(9000)}),
	}
	nonTrunk := func(hydrate bool) []any {
		tree := buildTree(nil, sessions, nil, resolve, treeOptions{previewLimit: 3, hydrate: hydrate})
		var out []any
		for _, g := range treeLanes(treeProjects(tree)[0]) {
			if g["isMain"] == false {
				out = append(out, g["label"])
			}
		}
		return out
	}
	wantEqual(t, nonTrunk(false), []any{"wt-zzz", "wt-aaa"})
	wantEqual(t, nonTrunk(true), []any{"wt-zzz", "wt-aaa"})
}

func TestKanbanTaskWorktreesCollapseIntoOneBucket(t *testing.T) {
	resolve := treeResolver(map[string][2]string{"/repo": {"/repo", "/repo"},
		"/repo/.worktrees/t_aaaaaaaa": {"/repo", "/repo/.worktrees/t_aaaaaaaa"},
		"/repo/.worktrees/t_bbbbbbbb": {"/repo", "/repo/.worktrees/t_bbbbbbbb"}})
	tree := hydratedTree(nil, []map[string]any{treeSession("/repo", "main", "", nil),
		treeSession("/repo/.worktrees/t_aaaaaaaa", "", "", nil), treeSession("/repo/.worktrees/t_bbbbbbbb", "", "", nil)}, resolve)
	project := treeProjects(tree)[0]
	var kanban []map[string]any
	for _, g := range treeLanes(project) {
		if truthy(g["isKanban"]) {
			kanban = append(kanban, g)
		}
	}
	wantEqual(t, len(kanban), 1)
	wantEqual(t, kanban[0]["id"], "/repo::kanban")
	wantEqual(t, kanban[0]["path"], "/repo/.worktrees")
	wantEqual(t, len(dictList(kanban[0]["sessions"])), 2)
	ids := laneField(project, "id")
	wantEqual(t, ids[len(ids)-1], "/repo::kanban")
}

func TestUserWorktreeUnderDotworktreesIsItsOwnLaneNotKanban(t *testing.T) {
	resolve := treeResolver(map[string][2]string{"/repo": {"/repo", "/repo"},
		"/repo/.worktrees/test-gui-stuff": {"/repo", "/repo/.worktrees/test-gui-stuff"}})
	tree := hydratedTree(nil, []map[string]any{treeSession("/repo", "main", "", nil),
		treeSession("/repo/.worktrees/test-gui-stuff", "icebath/test-gui-stuff", "", nil)}, resolve)
	lanes := map[any]map[string]any{}
	for _, g := range treeLanes(treeProjects(tree)[0]) {
		lanes[g["id"]] = g
	}
	lane, present := lanes["/repo/.worktrees/test-gui-stuff"]
	if !present || truthy(lane["isKanban"]) {
		t.Fatalf("user worktree lane %v", lane)
	}
	if _, present := lanes["/repo::kanban"]; present {
		t.Fatal("kanban lane present")
	}
}

func TestUnrecordedAndRecordedMainShareOneLane(t *testing.T) {
	resolve := treeResolver(map[string][2]string{"/repo": {"/repo", "/repo"}})
	tree := hydratedTree(nil, []map[string]any{treeSession("/repo", "", "", nil), treeSession("/repo", "main", "", nil)}, resolve)
	var mains []map[string]any
	for _, g := range treeLanes(treeProjects(tree)[0]) {
		if g["label"] == "main" {
			mains = append(mains, g)
		}
	}
	wantEqual(t, len(mains), 1)
	wantEqual(t, mains[0]["id"], "/repo::branch::main")
	wantEqual(t, len(dictList(mains[0]["sessions"])), 2)
}

// Project whose id has the same path key as the given path
func projectByPathKey(t *testing.T, tree map[string]any, path string) map[string]any {
	t.Helper()
	for _, p := range treeProjects(tree) {
		if treePathKey(strOr(p["id"])) == treePathKey(path) {
			return p
		}
	}
	t.Fatalf("no project for %s", path)
	return nil
}

func TestMainCheckoutDetectedWhenRootsDifferOnlyInPathSpelling(t *testing.T) {
	resolve := treeResolver(map[string][2]string{"C:/repo": {`C:\repo`, "C:/repo"}})
	tree := hydratedTree(nil, []map[string]any{treeSession("C:/repo", "main", "", nil)}, resolve)
	lanes := treeLanes(projectByPathKey(t, tree, "C:/repo"))
	wantEqual(t, len(lanes), 1)
	wantEqual(t, lanes[0]["isMain"], true)
	wantEqual(t, lanes[0]["label"], "main")
}

func TestMainAndLinkedWorktreeDoNotDuplicateOneCheckout(t *testing.T) {
	resolve := treeResolver(map[string][2]string{"C:/repo": {`C:\repo`, "C:/repo"}, "C:/repo-wt": {`C:\repo`, "C:/repo-wt"}})
	tree := hydratedTree(nil, []map[string]any{treeSession("C:/repo", "main", "", nil), treeSession("C:/repo-wt", "feature", "", nil)}, resolve)
	project := projectByPathKey(t, tree, "C:/repo")
	wantEqual(t, len(treeLanes(project)), 2)
	wantEqual(t, laneField(project, "label"), []any{"main", "repo-wt"})
	wantEqual(t, laneField(project, "isMain"), []any{true, false})
}

func TestPersistedRepoRootUsedWhenNoLiveProbe(t *testing.T) {
	tree := hydratedTree(nil, []map[string]any{treeSession("/repo/src", "main", "/repo", nil)}, nil)
	wantEqual(t, laneField(treeProjectByID(t, tree, "/repo"), "id"), []any{"/repo::branch::main"})
}

func TestNonGitCwdPreservesLegacyWorkspaceGrouping(t *testing.T) {
	legacy := treeSession("/work/notes", "", "", map[string]any{"title": "Research notes"})
	tree := hydratedTree(nil, []map[string]any{legacy}, noRepo)
	wantEqual(t, treeProjectIDs(tree), []any{"/work/notes"})
	project := treeProjects(tree)[0]
	wantEqual(t, project["isAuto"], true)
	wantEqual(t, project["label"], "notes")
	wantEqual(t, project["sessionCount"], 1)
	wantEqual(t, laneField(project, "id"), []any{"/work/notes::branch::main"})
	wantEqual(t, tree["scoped_session_ids"], []any{legacy["id"]})
}

func TestEquivalentWindowsCwdsCollapseIntoOneAutoProject(t *testing.T) {
	tree := hydratedTree(nil, []map[string]any{treeSession("C:/work/notes", "", "", nil), treeSession(`c:\WORK\notes`, "", "", nil),
		treeSession("C:/work/notes/", "", "", nil)}, noRepo)
	wantEqual(t, len(treeProjects(tree)), 1)
	project := treeProjects(tree)[0]
	wantEqual(t, project["id"], "C:/work/notes")
	wantEqual(t, project["sessionCount"], 3)
	repos := dictList(project["repos"])
	wantEqual(t, len(repos), 1)
	wantEqual(t, len(dictList(repos[0]["groups"])), 1)
	wantEqual(t, len(dictList(dictList(repos[0]["groups"])[0]["sessions"])), 3)
}

func TestWindowsPathIdentityPreservesExplicitProjectPriority(t *testing.T) {
	session := treeSession(`c:\work\notes\`, "", "", nil)
	tree := hydratedTree([]map[string]any{treeProject("p_notes", "Notes", []string{"C:/Work/Notes"}, nil)}, []map[string]any{session}, noRepo)
	wantEqual(t, treeProjectIDs(tree), []any{"p_notes"})
	wantEqual(t, treeProjects(tree)[0]["sessionCount"], 1)
	wantEqual(t, tree["scoped_session_ids"], []any{session["id"]})
}

func TestWslLocalhostCwdsCollapseIntoOneAutoProject(t *testing.T) {
	tree := hydratedTree(nil, []map[string]any{treeSession(`\wsl.localhost\Ubuntu\home\alice\proj`, "", "", nil),
		treeSession("//wsl.localhost/Ubuntu/home/alice/PROJ", "", "", nil)}, noRepo)
	wantEqual(t, len(treeProjects(tree)), 1)
	wantEqual(t, treeProjects(tree)[0]["sessionCount"], 2)
}

func TestPosixPathIdentityRemainsCaseSensitive(t *testing.T) {
	tree := hydratedTree([]map[string]any{treeProject("p_notes", "Notes", []string{"/Work/Notes"}, nil)},
		[]map[string]any{treeSession("/work/notes", "", "", nil)}, noRepo)
	var got []any
	for _, p := range treeProjects(tree) {
		got = append(got, [2]any{p["id"], p["sessionCount"]})
	}
	wantEqual(t, got, []any{[2]any{"p_notes", 0}, [2]any{"/work/notes", 1}})
}

func TestUnicodeNormalizationFormsShareOneProjectIdentity(t *testing.T) {
	nfc := norm.NFC.String("/projects/sv/bist\u00e5nd")
	nfd := norm.NFD.String(nfc)
	if nfc == nfd {
		t.Fatal("premise")
	}
	tree := hydratedTree([]map[string]any{treeProject("p_bistand", "Bist\u00e5nd", []string{nfc}, nil)},
		[]map[string]any{treeSession(nfd, "", "", nil)}, noRepo)
	wantEqual(t, treeProjectByID(t, tree, "p_bistand")["sessionCount"], 1)
	for _, p := range treeProjects(tree) {
		if truthy(p["isAuto"]) && strings.Contains(strOr(p["id"]), "bist") {
			t.Fatalf("stray auto project %v", p["id"])
		}
	}
}

func TestWindowsUnicodeNormalizationAndCaseShareOneIdentity(t *testing.T) {
	folderNFC := norm.NFC.String("D:/Projects/SV/Bist\u00e5nd")
	cwdNFD := norm.NFD.String("d:/projects/sv/bist\u00e5nd")
	if folderNFC == cwdNFD {
		t.Fatal("premise")
	}
	tree := hydratedTree([]map[string]any{treeProject("p_sv", "SV", []string{folderNFC}, nil)},
		[]map[string]any{treeSession(cwdNFD, "", "", nil)}, noRepo)
	wantEqual(t, treeProjectByID(t, tree, "p_sv")["sessionCount"], 1)
}

func TestExplicitProjectClaimsSessionsAndBeatsAuto(t *testing.T) {
	resolve := treeResolver(map[string][2]string{"/www/app": {"/www/app", "/www/app"}, "/www/other": {"/www/other", "/www/other"}})
	tree := hydratedTree([]map[string]any{treeProject("p_app", "App", []string{"/www/app"}, nil)},
		[]map[string]any{treeSession("/www/app", "main", "", nil), treeSession("/www/other", "main", "", nil)}, resolve)
	explicit := treeProjectByID(t, tree, "p_app")
	wantEqual(t, explicit["isAuto"], false)
	wantEqual(t, explicit["sessionCount"], 1)
	wantEqual(t, treeProjectByID(t, tree, "/www/other")["isAuto"], true)
}

func TestScopedSessionIdsIsUnionOfPlacedSessions(t *testing.T) {
	resolve := treeResolver(map[string][2]string{"/www/app": {"/www/app", "/www/app"}, "/www/repo": {"/www/repo", "/www/repo"}})
	owned, auto, homeless := treeSession("/www/app", "main", "", nil), treeSession("/www/repo", "main", "", nil), treeSession(nil, "", "", nil)
	tree := hydratedTree([]map[string]any{treeProject("p_app", "App", []string{"/www/app"}, nil)},
		[]map[string]any{owned, auto, homeless}, resolve)
	scoped := tree["scoped_session_ids"].([]any)
	slices.SortFunc(scoped, func(a, b any) int { return strings.Compare(a.(string), b.(string)) })
	want := []any{owned["id"], auto["id"], homeless["id"]}
	slices.SortFunc(want, func(a, b any) int { return strings.Compare(a.(string), b.(string)) })
	wantEqual(t, scoped, want)
	wantEqual(t, homeSessionIDs(tree), []any{homeless["id"]})
}

func TestOverviewDropsSessionRowsButKeepsCountsAndPreviews(t *testing.T) {
	resolve := treeResolver(map[string][2]string{"/repo": {"/repo", "/repo"}})
	var sessions []map[string]any
	var want []string
	for range 4 {
		s := treeSession("/repo", "main", "", nil)
		sessions = append(sessions, s)
		want = append(want, s["id"].(string))
	}
	tree := buildTree(nil, sessions, nil, resolve, treeOptions{previewLimit: 3})
	project := treeProjects(tree)[0]
	wantEqual(t, project["sessionCount"], 4)
	wantEqual(t, len(dictList(project["previewSessions"])), 3)
	var got []string
	for _, id := range project["sessionIds"].([]any) {
		got = append(got, id.(string))
	}
	slices.Sort(got)
	slices.Sort(want)
	wantEqual(t, got, want)
	for _, g := range treeLanes(project) {
		wantEqual(t, g["sessions"], []any{})
	}
	wantEqual(t, dictList(project["repos"])[0]["sessionCount"], 4)
}

func TestDiscoveredRepoWithNoSessionsBecomesZeroSessionProject(t *testing.T) {
	discovered := []map[string]any{{"root": "/www/fresh", "label": "fresh", "sessions": int64(0), "last_active": int64(5)}}
	tree := buildTree(nil, nil, discovered, nil, treeOptions{previewLimit: 3})
	fresh := treeProjectByID(t, tree, "/www/fresh")
	wantEqual(t, fresh["isAuto"], true)
	wantEqual(t, fresh["sessionCount"], 0)
	wantEqual(t, dictList(fresh["repos"])[0]["groups"], []any{})
}

func TestSeededFolderRepoDoesNotDuplicateASessionDerivedRepo(t *testing.T) {
	resolve := treeResolver(map[string][2]string{"/www/app": {"/www/app", "/www/app"}})
	tree := hydratedTree([]map[string]any{treeProject("p_app", "App", []string{"/www/app"}, nil)},
		[]map[string]any{treeSession("/www/app", "main", "", nil)}, resolve)
	var paths []any
	for _, r := range dictList(treeProjectByID(t, tree, "p_app")["repos"]) {
		paths = append(paths, r["path"])
	}
	wantEqual(t, paths, []any{"/www/app"})
}

func TestNestedProjectFoldersPickTheDeepestMatch(t *testing.T) {
	resolve := treeResolver(map[string][2]string{"/work/app": {"/work/app", "/work/app"}, "/work/other": {"/work/other", "/work/other"}})
	tree := hydratedTree([]map[string]any{treeProject("p_outer", "Outer", []string{"/work"}, nil), treeProject("p_inner", "Inner", []string{"/work/app"}, nil)},
		[]map[string]any{treeSession("/work/app", "main", "", nil), treeSession("/work/other", "main", "", nil)}, resolve)
	wantEqual(t, treeProjectByID(t, tree, "p_inner")["sessionCount"], 1)
	wantEqual(t, treeProjectByID(t, tree, "p_outer")["sessionCount"], 1)
}

func TestJunkRootNeverBecomesAnAutoProject(t *testing.T) {
	resolve := treeResolver(map[string][2]string{"/home/me/.icebath": {"/home/me/.icebath", "/home/me/.icebath"}, "/www/app": {"/www/app", "/www/app"}})
	junk, real := treeSession("/home/me/.icebath", "main", "", nil), treeSession("/www/app", "main", "", nil)
	tree := buildTree(nil, []map[string]any{junk, real}, nil, resolve, treeOptions{previewLimit: 3, hydrate: true,
		isJunkRoot: func(root string) bool { return root == "/home/me/.icebath" }})
	wantEqual(t, realProjectIDs(tree), []any{"/www/app"})
	wantEqual(t, homeSessionIDs(tree), []any{junk["id"]})
	if !slices.Contains(tree["scoped_session_ids"].([]any), real["id"]) {
		t.Fatal("real session not scoped")
	}
}

func TestBroadDefaultNonGitCwdStaysUnscoped(t *testing.T) {
	detached := treeSession("/home/test/.icebath", "", "", nil)
	tree := buildTree(nil, []map[string]any{detached}, nil, noRepo, treeOptions{previewLimit: 3, hydrate: true,
		isJunkCwd: func(path string) bool { return path == "/home/test" || path == "/home/test/.icebath" }})
	wantEqual(t, realProjectIDs(tree), []any(nil))
	wantEqual(t, homeSessionIDs(tree), []any{detached["id"]})
}

func TestDeletedSiblingWorktreeFoldsIntoParentHomeCheckout(t *testing.T) {
	resolve := treeResolver(map[string][2]string{"/www/icebath-agent": {"/www/icebath-agent", "/www/icebath-agent"}})
	tree := hydratedTree(nil, []map[string]any{treeSession("/www/icebath-agent", "main", "", nil),
		treeSession("/www/icebath-agent-session-links", "", "", nil)}, resolve)
	project := treeProjects(tree)[0]
	wantEqual(t, treeProjectIDs(tree), []any{"/www/icebath-agent"})
	wantEqual(t, laneField(project, "id"), []any{"/www/icebath-agent::branch::main"})
	main := treeLanes(project)[0]
	if !truthy(main["isMain"]) || main["path"] != "/www/icebath-agent" {
		t.Fatalf("main lane %v", main)
	}
	wantEqual(t, len(dictList(main["sessions"])), 2)
}

func TestExistingNonGitWorkspaceStillBecomesAProject(t *testing.T) {
	tree := buildTree(nil, []map[string]any{treeSession("/www/notes", "", "", nil)}, nil, noRepo, treeOptions{previewLimit: 3, hydrate: true,
		exists: func(string) bool { return true }})
	wantEqual(t, treeProjectIDs(tree), []any{"/www/notes"})
}

func TestGitLanesAreFlaggedIsGit(t *testing.T) {
	probed := hydratedTree(nil, []map[string]any{treeSession("/repo", "main", "", nil)}, treeResolver(map[string][2]string{"/repo": {"/repo", "/repo"}}))
	persisted := hydratedTree(nil, []map[string]any{treeSession("/repo", "main", "/repo", nil)}, nil)
	for _, tree := range []map[string]any{probed, persisted} {
		lane := treeLanes(treeProjects(tree)[0])[0]
		if !truthy(lane["isMain"]) || !truthy(lane["isGit"]) {
			t.Fatalf("lane %v", lane)
		}
	}
}

func TestNonGitHeuristicLaneIsFlaggedNotGit(t *testing.T) {
	tree := hydratedTree(nil, []map[string]any{treeSession("/www/notes", "", "", nil)}, noRepo)
	lane := treeLanes(treeProjects(tree)[0])[0]
	if !truthy(lane["isMain"]) || lane["id"] != "/www/notes::branch::main" {
		t.Fatalf("lane %v", lane)
	}
	wantEqual(t, lane["isGit"], false)
}

func TestStalePersistedRepoRootDoesNotBecomeAProject(t *testing.T) {
	stale := treeSession("/tmp/gone/sub", "", "/tmp/gone", nil)
	tree := buildTree(nil, []map[string]any{stale}, nil, noRepo, treeOptions{previewLimit: 3, hydrate: true,
		exists: func(string) bool { return false }})
	wantEqual(t, realProjectIDs(tree), []any(nil))
	wantEqual(t, homeSessionIDs(tree), []any{stale["id"]})
}

func TestExistsDefaultsToKeepingEverything(t *testing.T) {
	tree := hydratedTree(nil, []map[string]any{treeSession("/remote/workspace", "", "", nil)}, noRepo)
	wantEqual(t, treeProjectIDs(tree), []any{"/remote/workspace"})
}

func TestSiblingProbeIsBounded(t *testing.T) {
	var probed []string
	resolve := func(cwd string) map[string]any {
		probed = append(probed, cwd)
		return nil
	}
	wantEqual(t, probeSiblingWorktree("/a-b-c/d-e-f/g-h-i/j-k-l", resolve), "")
	if len(probed) > maxSiblingProbes {
		t.Fatalf("%d probes", len(probed))
	}
}

func TestHomeBucketLeadsTheTreeAndIsLossless(t *testing.T) {
	resolve := treeResolver(map[string][2]string{"/www/app": {"/www/app", "/www/app"}, "/home/me": {"/home/me", "/home/me"}})
	owned, cwdless, junked := treeSession("/www/app", "main", "", nil), treeSession(nil, "", "", nil), treeSession("/home/me", "main", "", nil)
	tree := buildTree(nil, []map[string]any{owned, cwdless, junked}, nil, resolve, treeOptions{previewLimit: 3, hydrate: true,
		isJunkRoot: func(root string) bool { return root == "/home/me" }})
	wantEqual(t, treeProjects(tree)[0]["id"], noProjectID)
	home := map[any]bool{}
	for _, id := range homeSessionIDs(tree) {
		home[id] = true
	}
	wantEqual(t, home, map[any]bool{cwdless["id"]: true, junked["id"]: true})
	all := map[any]bool{}
	for _, p := range treeProjects(tree) {
		for _, s := range treeSessionsOf(p) {
			all[s["id"]] = true
		}
	}
	wantEqual(t, all, map[any]bool{owned["id"]: true, cwdless["id"]: true, junked["id"]: true})
}

func TestCollidingRepoBasenamesDisambiguateLabels(t *testing.T) {
	resolve := treeResolver(map[string][2]string{"/x/proj": {"/x/proj", "/x/proj"}, "/y/proj": {"/y/proj", "/y/proj"}})
	tree := hydratedTree(nil, []map[string]any{treeSession("/x/proj", "main", "", nil), treeSession("/y/proj", "main", "", nil)}, resolve)
	var labels []string
	for _, p := range treeProjects(tree) {
		labels = append(labels, p["label"].(string))
	}
	slices.Sort(labels)
	wantEqual(t, labels, []string{"x/proj", "y/proj"})
}

func TestNonGitFolderUsesBranchLaneId(t *testing.T) {
	result := placeByHeuristic("/home/user/my-project")
	if result == nil {
		t.Fatal("no placement")
	}
	wantEqual(t, result["lane_key"], branchLaneID("/home/user/my-project", defaultBranchLabel))
	wantEqual(t, result["lane_label"], "my-project")
	wantEqual(t, result["is_main"], true)
}

func TestNonGitFolderLaneMatchesOverlayScheme(t *testing.T) {
	result := placeByHeuristic("/data/work/folder-x")
	if result == nil {
		t.Fatal("no placement")
	}
	wantEqual(t, result["lane_key"], "/data/work/folder-x::branch::main")
}

func TestHeuristicLaneIdsForKanbanAndWtSuffixAreUnchanged(t *testing.T) {
	kanban := placeByHeuristic("/www/app/.worktrees/t_1a2b3c")
	if kanban == nil {
		t.Fatal("no kanban placement")
	}
	wantEqual(t, kanban["lane_key"], kanbanLaneID("/www/app"))
	wantEqual(t, kanban["is_kanban"], true)
	wt := placeByHeuristic("/www/app-wt-feature")
	if wt == nil {
		t.Fatal("no wt placement")
	}
	wantEqual(t, wt["lane_key"], "/www/app-wt-feature")
	wantEqual(t, wt["lane_label"], "feature")
	wantEqual(t, wt["is_main"], false)
}

func TestEquivalentWindowsSpellingsDeriveOneLaneKey(t *testing.T) {
	a, b := placeByHeuristic("C:/work/notes"), placeByHeuristic(`C:\work\notes\`)
	if a == nil || b == nil {
		t.Fatal("no placement")
	}
	wantEqual(t, treeLaneKey(a["lane_key"].(string)), treeLaneKey(b["lane_key"].(string)))
}

func TestCwdlessSessionWithRepoRootStaysInItsExplicitProject(t *testing.T) {
	owned, detached := treeSession(nil, "main", "/www/app", nil), treeSession(nil, "", "", nil)
	tree := hydratedTree([]map[string]any{treeProject("p_app", "App", []string{"/www/app"}, nil)}, []map[string]any{owned, detached}, noRepo)
	explicit := treeProjectByID(t, tree, "p_app")
	if !slices.Contains(explicit["sessionIds"].([]any), owned["id"]) {
		t.Fatal("owned id not claimed")
	}
	var ids []any
	for _, s := range treeSessionsOf(explicit) {
		ids = append(ids, s["id"])
	}
	if !slices.Contains(ids, owned["id"]) {
		t.Fatal("owned row not in lanes")
	}
	wantEqual(t, homeSessionIDs(tree), []any{detached["id"]})
}
