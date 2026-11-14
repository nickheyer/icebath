package files

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

type pfEnv struct {
	scratch, userData, home, vault string
	trashed                        []string
	files                          *PluginFiles
	platform                       string
}

func newPFEnv(t *testing.T) *pfEnv {
	e := &pfEnv{scratch: realTemp(t)}
	e.userData = filepath.Join(e.scratch, "userData")
	e.home = filepath.Join(e.scratch, "home", ".icebath")
	e.vault = filepath.Join(e.scratch, "home", "Vault")
	os.MkdirAll(e.userData, 0o755)
	os.MkdirAll(e.home, 0o755)
	writeFile(t, filepath.Join(e.home, ".env"), "OPENROUTER_API_KEY=sk-secret\n")
	writeFile(t, filepath.Join(e.home, "config.yaml"), "model: x\n")
	writeFile(t, filepath.Join(e.vault, "notes", "a.md"), "# A\n")
	e.files = e.make()
	return e
}

func (e *pfEnv) make() *PluginFiles {
	return NewPluginFiles(PluginFilesDeps{
		UserDataDir:    func() string { return e.userData },
		ProtectedRoots: func() []string { return []string{e.home} },
		TrashItem: func(target string) error {
			e.trashed = append(e.trashed, target)
			return os.RemoveAll(target)
		},
		Platform: e.platform,
	})
}

func (e *pfEnv) grant(t *testing.T, plugin, folder string) string {
	t.Helper()
	g := e.files.AddGrant(plugin, folder)
	if g["ok"] != true {
		t.Fatal(g)
	}
	return jsString(rowField(g["folder"], "id"))
}

func (e *pfEnv) run(plugin string, req map[string]any) map[string]any {
	return e.files.Run(plugin, req)
}

func expectFail(t *testing.T, got map[string]any, code string) {
	t.Helper()
	if got["ok"] != false || (code != "" && got["error"] != code) {
		t.Fatal(got)
	}
}

func grantIDs(t *testing.T, f *PluginFiles, plugin string) []string {
	rows, err := f.Grants(plugin)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, r := range rows {
		ids = append(ids, jsString(rowField(r, "id")))
	}
	return ids
}

func TestGrantsPersistsAUserPickedFolderPerPluginAndSurvivesARestart(t *testing.T) {
	e := newPFEnv(t)
	id := e.grant(t, "plexus", e.vault)
	rows, _ := e.make().Grants("plexus")
	if len(rows) != 1 || rowField(rows[0], "id") != id || rowField(rows[0], "name") != "Vault" || rowField(rows[0], "path") != e.vault {
		t.Fatal(rows)
	}
	if other, _ := e.make().Grants("other"); len(other) != 0 {
		t.Fatal(other)
	}
}

func TestGrantsRePickingTheSameFolderReusesTheGrant(t *testing.T) {
	e := newPFEnv(t)
	first := e.grant(t, "plexus", e.vault)
	if e.grant(t, "plexus", e.vault) != first || len(grantIDs(t, e.files, "plexus")) != 1 {
		t.Fatal("not reused")
	}
}

func TestGrantsRefusesToGrant(t *testing.T) {
	for _, tc := range []struct {
		label  string
		folder func(e *pfEnv) string
	}{
		{"the Icebath home", func(e *pfEnv) string { return e.home }},
		{"a folder inside the Icebath home", func(e *pfEnv) string { return filepath.Join(e.home, "skills") }},
		{"a folder that contains the Icebath home", func(e *pfEnv) string { return filepath.Join(e.scratch, "home") }},
		{"the app data folder", func(e *pfEnv) string { return e.userData }},
		{"another plugin data folder", func(e *pfEnv) string { return filepath.Join(e.userData, "plugin-files", "data", "other") }},
	} {
		t.Run("refuses to grant "+tc.label, func(t *testing.T) {
			e := newPFEnv(t)
			os.MkdirAll(tc.folder(e), 0o755)
			expectFail(t, e.files.AddGrant("plexus", tc.folder(e)), "protected")
			if len(grantIDs(t, e.files, "plexus")) != 0 {
				t.Fatal("granted")
			}
		})
	}
}

func TestGrantsRefusesToGrantAKeyFolderSuchAsSsh(t *testing.T) {
	e := newPFEnv(t)
	ssh := filepath.Join(e.scratch, "home", ".ssh")
	os.MkdirAll(ssh, 0o755)
	expectFail(t, e.files.AddGrant("plexus", ssh), "protected")
}

func TestGrantsRefusesToGrantASymlinkThatResolvesIntoTheIcebathHome(t *testing.T) {
	e := newPFEnv(t)
	link := filepath.Join(e.scratch, "innocent")
	mustSymlink(t, e.home, link)
	expectFail(t, e.files.AddGrant("plexus", link), "protected")
}

func TestGrantsARevokedGrantRefusesEveryLaterOperationAtOnce(t *testing.T) {
	e := newPFEnv(t)
	id := e.grant(t, "plexus", e.vault)
	if r := e.run("plexus", map[string]any{"op": "readText", "folder": id, "path": "notes/a.md"}); r["ok"] != true {
		t.Fatal(r)
	}
	revoked, err := e.files.Revoke("plexus", id, false)
	if err != nil || !reflect.DeepEqual(revoked, map[string]any{"ok": true, "removed": 1}) {
		t.Fatal(revoked, err)
	}
	expectFail(t, e.run("plexus", map[string]any{"op": "readText", "folder": id, "path": "notes/a.md"}), "not-granted")
	if len(grantIDs(t, e.make(), "plexus")) != 0 {
		t.Fatal("revoke not saved")
	}
}

func TestGrantsOnePluginCannotUseAnotherPluginsGrantId(t *testing.T) {
	e := newPFEnv(t)
	id := e.grant(t, "plexus", e.vault)
	expectFail(t, e.run("intruder", map[string]any{"op": "list", "folder": id, "path": ""}), "not-granted")
}

func TestGrantsRefusesAGrantWhoseFolderWasSwappedForALinkIntoTheIcebathHome(t *testing.T) {
	e := newPFEnv(t)
	id := e.grant(t, "plexus", e.vault)
	os.RemoveAll(e.vault)
	mustSymlink(t, e.home, e.vault)
	expectFail(t, e.run("plexus", map[string]any{"op": "readText", "folder": id, "path": ".env"}), "")
	expectFail(t, e.run("plexus", map[string]any{"op": "list", "folder": id, "path": ""}), "")
}

func TestPathContainmentRefuses(t *testing.T) {
	for _, tc := range [][2]string{
		{"parent traversal", "../.icebath/.env"}, {"nested traversal", "notes/../../.icebath/.env"},
		{"backslash traversal", `notes\..\..\.icebath\.env`}, {"an absolute POSIX path", "/etc/passwd"},
		{"an absolute path into the Icebath home", ""}, {"a Windows drive path", `C:\Users\me\.icebath\.env`},
		{"a UNC path", `\\server\share\x`}, {"a NUL byte", "notes/a.md\x00.png"},
	} {
		t.Run("refuses "+tc[0], func(t *testing.T) {
			e := newPFEnv(t)
			id := e.grant(t, "plexus", e.vault)
			target := tc[1]
			if tc[0] == "an absolute path into the Icebath home" {
				target = filepath.Join(e.home, ".env")
			}
			expectFail(t, e.run("plexus", map[string]any{"op": "readText", "folder": id, "path": target}), "invalid-path")
			expectFail(t, e.run("plexus", map[string]any{"op": "writeText", "folder": id, "path": target, "text": "pwned"}), "invalid-path")
			if readText(t, filepath.Join(e.home, "config.yaml")) != "model: x\n" {
				t.Fatal("home written")
			}
		})
	}
}

func TestPathContainmentRefusesToReadThroughASymlinkedFileThatPointsOutsideTheFolder(t *testing.T) {
	e := newPFEnv(t)
	id := e.grant(t, "plexus", e.vault)
	mustSymlink(t, filepath.Join(e.home, ".env"), filepath.Join(e.vault, "notes", "leak.md"))
	expectFail(t, e.run("plexus", map[string]any{"op": "readText", "folder": id, "path": "notes/leak.md"}), "outside-folder")
	expectFail(t, e.run("plexus", map[string]any{"op": "readDataUrl", "folder": id, "path": "notes/leak.md"}), "outside-folder")
}

func TestPathContainmentRefusesToWriteThroughASymlinkedFileOrFolderThatPointsOutside(t *testing.T) {
	e := newPFEnv(t)
	id := e.grant(t, "plexus", e.vault)
	mustSymlink(t, filepath.Join(e.home, "config.yaml"), filepath.Join(e.vault, "cfg.md"))
	mustSymlink(t, e.home, filepath.Join(e.vault, "home"))
	for _, rel := range []string{"cfg.md", "home/config.yaml", "home/new.md"} {
		expectFail(t, e.run("plexus", map[string]any{"op": "writeText", "folder": id, "path": rel, "text": "pwned"}), "outside-folder")
	}
	if readText(t, filepath.Join(e.home, "config.yaml")) != "model: x\n" || exists(filepath.Join(e.home, "new.md")) {
		t.Fatal("home written")
	}
}

func TestPathContainmentRefusesADanglingSymlinkInsteadOfCreatingItsTarget(t *testing.T) {
	e := newPFEnv(t)
	id := e.grant(t, "plexus", e.vault)
	outside := filepath.Join(e.scratch, "created-outside.md")
	mustSymlink(t, outside, filepath.Join(e.vault, "dangling.md"))
	expectFail(t, e.run("plexus", map[string]any{"op": "writeText", "folder": id, "path": "dangling.md", "text": "x"}), "")
	if exists(outside) {
		t.Fatal("target created")
	}
}

func TestPathContainmentRefusesListingRenamingAndTrashingThroughASymlinkedFolderIntoTheIcebathHome(t *testing.T) {
	e := newPFEnv(t)
	id := e.grant(t, "plexus", e.vault)
	mustSymlink(t, e.home, filepath.Join(e.vault, "home"))
	expectFail(t, e.run("plexus", map[string]any{"op": "list", "folder": id, "path": "home"}), "outside-folder")
	expectFail(t, e.run("plexus", map[string]any{"op": "rename", "folder": id, "path": "home/config.yaml", "name": "x.yaml"}), "outside-folder")
	expectFail(t, e.run("plexus", map[string]any{"op": "trash", "folder": id, "path": "home/.env"}), "outside-folder")
	if len(e.trashed) != 0 || !exists(filepath.Join(e.home, ".env")) {
		t.Fatal(e.trashed)
	}
}

func TestPathContainmentAListingHidesLinksThatLeaveTheFolderAndKeepsLinksThatStayInside(t *testing.T) {
	e := newPFEnv(t)
	id := e.grant(t, "plexus", e.vault)
	mustSymlink(t, e.home, filepath.Join(e.vault, "escape"))
	mustSymlink(t, filepath.Join(e.vault, "notes"), filepath.Join(e.vault, "alias"))
	listed := e.run("plexus", map[string]any{"op": "list", "folder": id, "path": ""})
	if listed["ok"] != true {
		t.Fatal(listed)
	}
	var names []string
	for _, entry := range listed["entries"].([]any) {
		names = append(names, entry.(map[string]any)["name"].(string))
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"alias", "notes"}) {
		t.Fatal(names)
	}
}

func TestPathContainmentRenameKeepsTheItemInItsFolderAndRefusesANameWithASeparatorOrTraversal(t *testing.T) {
	e := newPFEnv(t)
	id := e.grant(t, "plexus", e.vault)
	for _, name := range []string{"../../escaped.md", "sub/x.md", "..", ".", ""} {
		expectFail(t, e.run("plexus", map[string]any{"op": "rename", "folder": id, "path": "notes/a.md", "name": name}), "invalid-path")
	}
	got := e.run("plexus", map[string]any{"op": "rename", "folder": id, "path": "notes/a.md", "name": "b.md"})
	if !reflect.DeepEqual(got, map[string]any{"ok": true, "path": "notes/b.md"}) || !exists(filepath.Join(e.vault, "notes", "b.md")) {
		t.Fatal(got)
	}
}

func TestPathContainmentNeverTrashesOrRenamesTheGrantedFolderItself(t *testing.T) {
	e := newPFEnv(t)
	id := e.grant(t, "plexus", e.vault)
	expectFail(t, e.run("plexus", map[string]any{"op": "trash", "folder": id, "path": ""}), "invalid-path")
	expectFail(t, e.run("plexus", map[string]any{"op": "rename", "folder": id, "path": ".", "name": "x"}), "invalid-path")
	if !exists(e.vault) {
		t.Fatal("vault gone")
	}
}

func TestPluginDataFolderReadsAndWritesInsideItsOwnDataFolderWithoutAnyGrant(t *testing.T) {
	e := newPFEnv(t)
	w := e.run("plexus", map[string]any{"op": "writeText", "folder": "data", "path": "state.json", "text": `{"a":1}`})
	if w["ok"] != true || w["path"] != "state.json" {
		t.Fatal(w)
	}
	r := e.run("plexus", map[string]any{"op": "readText", "folder": "data", "path": "state.json"})
	if r["ok"] != true || r["text"] != `{"a":1}` || readText(t, filepath.Join(e.userData, "plugin-files", "data", "plexus", "state.json")) != `{"a":1}` {
		t.Fatal(r)
	}
}

func TestPluginDataFolderCannotReachAnotherPluginsDataFolderOrTheGrantsRecord(t *testing.T) {
	e := newPFEnv(t)
	e.run("other", map[string]any{"op": "writeText", "folder": "data", "path": "secret.json", "text": "theirs"})
	e.run("plexus", map[string]any{"op": "writeText", "folder": "data", "path": "mine.json", "text": "mine"})
	for _, rel := range []string{"../other/secret.json", "../../grants.json"} {
		expectFail(t, e.run("plexus", map[string]any{"op": "readText", "folder": "data", "path": rel}), "invalid-path")
	}
	data := filepath.Join(e.userData, "plugin-files", "data")
	mustSymlink(t, filepath.Join(data, "other"), filepath.Join(data, "plexus", "peek"))
	expectFail(t, e.run("plexus", map[string]any{"op": "readText", "folder": "data", "path": "peek/secret.json"}), "outside-folder")
}

func TestPluginDataFolderKeepsIdsThatDifferOnlyInCaseApartOnAnyFilesystem(t *testing.T) {
	e := newPFEnv(t)
	e.run("plexus", map[string]any{"op": "writeText", "folder": "data", "path": "who.txt", "text": "lower"})
	e.run("Plexus", map[string]any{"op": "writeText", "folder": "data", "path": "who.txt", "text": "upper"})
	if r := e.run("plexus", map[string]any{"op": "readText", "folder": "data", "path": "who.txt"}); r["text"] != "lower" {
		t.Fatal(r)
	}
}

func TestPluginDataFolderRefusesAPluginIdThatIsNotAPlainSlug(t *testing.T) {
	for _, id := range []string{"../x", "a/b", "..", "", strings.Repeat("x", 200)} {
		e := newPFEnv(t)
		expectFail(t, e.run(id, map[string]any{"op": "list", "folder": "data", "path": ""}), "invalid-plugin")
	}
}

func TestCapsAndShapesRefusesOversizedWritesAndReadsInsteadOfTruncating(t *testing.T) {
	e := newPFEnv(t)
	id := e.grant(t, "plexus", e.vault)
	big := strings.Repeat("x", 4*1024*1024+1)
	expectFail(t, e.run("plexus", map[string]any{"op": "writeText", "folder": id, "path": "big.md", "text": big}), "too-large")
	if exists(filepath.Join(e.vault, "big.md")) {
		t.Fatal("written")
	}
	writeFile(t, filepath.Join(e.vault, "big.md"), big)
	expectFail(t, e.run("plexus", map[string]any{"op": "readText", "folder": id, "path": "big.md"}), "too-large")
}

func TestCapsAndShapesReadsBinaryFilesOnlyAsDataURLs(t *testing.T) {
	e := newPFEnv(t)
	id := e.grant(t, "plexus", e.vault)
	os.WriteFile(filepath.Join(e.vault, "pic.png"), []byte{0x89, 0x50, 0x4e, 0x47, 0, 1, 2}, 0o644)
	expectFail(t, e.run("plexus", map[string]any{"op": "readText", "folder": id, "path": "pic.png"}), "binary")
	got := e.run("plexus", map[string]any{"op": "readDataUrl", "folder": id, "path": "pic.png"})
	if !reflect.DeepEqual(got, map[string]any{"ok": true, "url": "data:image/png;base64,iVBORwABAg=="}) {
		t.Fatal(got)
	}
}

func TestCapsAndShapesRefusesSensitiveFilesInsideAGrantedFolder(t *testing.T) {
	e := newPFEnv(t)
	id := e.grant(t, "plexus", e.vault)
	writeFile(t, filepath.Join(e.vault, ".env"), "TOKEN=x\n")
	expectFail(t, e.run("plexus", map[string]any{"op": "readText", "folder": id, "path": ".env"}), "sensitive-file")
	expectFail(t, e.run("plexus", map[string]any{"op": "writeText", "folder": id, "path": "notes/id_ed25519", "text": "k"}), "sensitive-file")
	expectFail(t, e.run("plexus", map[string]any{"op": "rename", "folder": id, "path": "notes/a.md", "name": ".env"}), "sensitive-file")
}

func TestCapsAndShapesAnExclusiveWriteNeverReplacesAnExistingFile(t *testing.T) {
	e := newPFEnv(t)
	id := e.grant(t, "plexus", e.vault)
	expectFail(t, e.run("plexus", map[string]any{"op": "writeText", "folder": id, "path": "notes/a.md", "text": "new", "exclusive": true}), "exists")
	if readText(t, filepath.Join(e.vault, "notes", "a.md")) != "# A\n" {
		t.Fatal("replaced")
	}
}

func TestCapsAndShapesAnswersAnUnknownOpAsAResultNeverAThrow(t *testing.T) {
	e := newPFEnv(t)
	id := e.grant(t, "plexus", e.vault)
	expectFail(t, e.run("plexus", map[string]any{"op": "unlink", "folder": id, "path": "notes/a.md"}), "invalid-op")
}

func TestCapsAndShapesStatReportsAMissingEntryWithoutFailing(t *testing.T) {
	e := newPFEnv(t)
	id := e.grant(t, "plexus", e.vault)
	if got := e.run("plexus", map[string]any{"op": "stat", "folder": id, "path": "notes/missing.md"}); !reflect.DeepEqual(got, map[string]any{"ok": true, "exists": false}) {
		t.Fatal(got)
	}
	got := e.run("plexus", map[string]any{"op": "stat", "folder": id, "path": "notes/a.md"})
	if got["ok"] != true || got["exists"] != true || got["isDirectory"] != false || got["size"] != int64(4) {
		t.Fatal(got)
	}
}

func TestCapsAndShapesTrashMovesOnlyTheNamedEntry(t *testing.T) {
	e := newPFEnv(t)
	id := e.grant(t, "plexus", e.vault)
	if got := e.run("plexus", map[string]any{"op": "trash", "folder": id, "path": "notes/a.md"}); !reflect.DeepEqual(got, map[string]any{"ok": true}) {
		t.Fatal(got)
	}
	if !reflect.DeepEqual(e.trashed, []string{filepath.Join(e.vault, "notes", "a.md")}) {
		t.Fatal(e.trashed)
	}
}

func TestProtectedRootsAnOpInsideAGrantedFolderNeverReachesAProtectedRootNestedUnderIt(t *testing.T) {
	e := newPFEnv(t)
	id := e.grant(t, "plexus", e.vault)
	nested := filepath.Join(e.vault, "icebath")
	os.Mkdir(nested, 0o755)
	writeFile(t, filepath.Join(nested, "config.yaml"), "model: y\n")
	files := NewPluginFiles(PluginFilesDeps{
		UserDataDir:    func() string { return e.userData },
		ProtectedRoots: func() []string { return []string{e.home, nested} },
		TrashItem:      func(string) error { return nil },
	})
	expectFail(t, files.Run("plexus", map[string]any{"op": "readText", "folder": id, "path": "icebath/config.yaml"}), "protected")
}
