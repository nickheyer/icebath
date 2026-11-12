package files

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Restores the plugin door's file system calls after a case swaps one
func keepPFS(t *testing.T) {
	saved := pfs
	t.Cleanup(func() { pfs = saved })
}

func hardEnv(t *testing.T) *pfEnv {
	e := newPFEnv(t)
	os.Remove(filepath.Join(e.home, ".env"))
	writeFile(t, filepath.Join(e.home, "config.yaml"), "model: secret-config\n")
	e.files = NewPluginFiles(PluginFilesDeps{
		UserDataDir:    func() string { return e.userData },
		ProtectedRoots: func() []string { return []string{e.home} },
		TrashItem:      os.RemoveAll,
	})
	t.Cleanup(func() { os.Chmod(e.scratch, 0o700) })
	return e
}

func unprivileged() bool { return runtime.GOOS != "windows" && os.Getuid() != 0 }

// The case's own platform gate, POSIX only like its runIf(posix)
func posix() bool { return runtime.GOOS != "windows" }

func TestSensitiveFilesCannotBeMovedOutOfTheirNamesRefusesRenamingAndTrashingIt(t *testing.T) {
	for _, tc := range [][2]string{{".env", "notes.txt"}, {"server.pem", "p.txt"}, {"id_ed25519", "key.txt"}} {
		t.Run(tc[0], func(t *testing.T) {
			e := hardEnv(t)
			id := e.grant(t, "plexus", e.vault)
			writeFile(t, filepath.Join(e.vault, tc[0]), "SECRET=sk-live\n")
			expectFail(t, e.run("plexus", map[string]any{"op": "rename", "folder": id, "path": tc[0], "name": tc[1]}), "sensitive-file")
			expectFail(t, e.run("plexus", map[string]any{"op": "trash", "folder": id, "path": tc[0]}), "sensitive-file")
			if !exists(filepath.Join(e.vault, tc[0])) || exists(filepath.Join(e.vault, tc[1])) {
				t.Fatal("moved")
			}
		})
	}
}

func TestSensitiveFilesCannotBeMovedOutOfTheirNamesRefusesRenamingAKeyFolderSoItsFilesLoseTheRule(t *testing.T) {
	e := hardEnv(t)
	id := e.grant(t, "plexus", e.vault)
	writeFile(t, filepath.Join(e.vault, ".ssh", "config"), "Host x\n")
	expectFail(t, e.run("plexus", map[string]any{"op": "rename", "folder": id, "path": ".ssh", "name": "ssh"}), "sensitive-file")
	expectFail(t, e.run("plexus", map[string]any{"op": "readText", "folder": id, "path": ".ssh/config"}), "sensitive-file")
}

func TestOnlyPlainFilesRefusesToGrantTheKernelPseudoFilesystem(t *testing.T) {
	if runtime.GOOS != "linux" {
		return
	}
	for _, folder := range []string{"/proc", "/sys", "/dev", "/proc/self"} {
		e := hardEnv(t)
		got := e.files.AddGrant("plexus", folder)
		if got["ok"] != false || got["error"] != "protected" || !strings.Contains(got["message"].(string), "system folder") {
			t.Fatal(folder, got)
		}
	}
}

func TestOnlyPlainFilesRefusesAFIFOInsteadOfBlockingOnIt(t *testing.T) {
	if !posix() {
		return
	}
	e := hardEnv(t)
	id := e.grant(t, "plexus", e.vault)
	if err := exec.Command("mkfifo", filepath.Join(e.vault, "pipe")).Run(); err != nil {
		t.Fatal(err)
	}
	done := make(chan map[string]any, 1)
	go func() { done <- e.run("plexus", map[string]any{"op": "readText", "folder": id, "path": "pipe"}) }()
	select {
	case got := <-done:
		expectFail(t, got, "not-a-file")
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("blocked")
	}
	expectFail(t, e.run("plexus", map[string]any{"op": "writeText", "folder": id, "path": "pipe", "text": "x"}), "not-a-file")
}

func TestOnlyPlainFilesRefusesAHardLinkWhichMayBeASecondNameForIcebathData(t *testing.T) {
	if !posix() {
		return
	}
	e := hardEnv(t)
	id := e.grant(t, "plexus", e.vault)
	if err := os.Link(filepath.Join(e.home, "config.yaml"), filepath.Join(e.vault, "notes", "c.md")); err != nil {
		t.Fatal(err)
	}
	expectFail(t, e.run("plexus", map[string]any{"op": "readText", "folder": id, "path": "notes/c.md"}), "protected")
	expectFail(t, e.run("plexus", map[string]any{"op": "writeText", "folder": id, "path": "notes/c.md", "text": "pwned"}), "protected")
	if readText(t, filepath.Join(e.home, "config.yaml")) != "model: secret-config\n" {
		t.Fatal("written")
	}
}

// Runs swap once right after the door looked at checked with lstat or stat
func swapAfterCheck(t *testing.T, method string, checked string, swap func()) {
	done := false
	wrap := func(orig func(string) (fs.FileInfo, error)) func(string) (fs.FileInfo, error) {
		return func(target string) (fs.FileInfo, error) {
			st, err := orig(target)
			if !done && target == checked {
				done = true
				swap()
			}
			return st, err
		}
	}
	if method == "lstat" {
		pfs.lstat = wrap(pfs.lstat)
	} else {
		pfs.stat = wrap(pfs.stat)
	}
}

func TestASwapBetweenTheCheckAndTheOpenIsRefusedAParentFolderSwappedForALinkIntoTheIcebathHomeNoFileLandsThere(t *testing.T) {
	if !posix() {
		return
	}
	keepPFS(t)
	e := hardEnv(t)
	id := e.grant(t, "plexus", e.vault)
	sub := filepath.Join(e.vault, "sub")
	os.Mkdir(sub, 0o755)
	swapAfterCheck(t, "lstat", filepath.Join(sub, "pwn.txt"), func() {
		os.Rename(sub, filepath.Join(e.vault, "sub-old"))
		os.Symlink(e.home, sub)
	})
	got := e.run("plexus", map[string]any{"op": "writeText", "folder": id, "path": "sub/pwn.txt", "text": "pwned"})
	if !regexp.MustCompile(`^(outside-folder|protected)$`).MatchString(jsString(got["error"])) || exists(filepath.Join(e.home, "pwn.txt")) {
		t.Fatal(got)
	}
}

func TestASwapBetweenTheCheckAndTheOpenIsRefusedACheckedFileSwappedForALinkToIcebathDataIsNotReadOrWritten(t *testing.T) {
	if !posix() {
		return
	}
	keepPFS(t)
	e := hardEnv(t)
	id := e.grant(t, "plexus", e.vault)
	note := filepath.Join(e.vault, "notes", "a.md")
	swap := func() {
		os.Remove(note)
		os.Symlink(filepath.Join(e.home, "config.yaml"), note)
	}
	saved := pfs
	swapAfterCheck(t, "stat", note, swap)
	read := e.run("plexus", map[string]any{"op": "readText", "folder": id, "path": "notes/a.md"})
	raw, _ := json.Marshal(read)
	if read["ok"] != false || strings.Contains(string(raw), "secret-config") {
		t.Fatal(read)
	}
	pfs = saved
	os.Remove(note)
	writeFile(t, note, "# A\n")
	swapAfterCheck(t, "stat", note, swap)
	expectFail(t, e.run("plexus", map[string]any{"op": "writeText", "folder": id, "path": "notes/a.md", "text": "pwned"}), "")
	if readText(t, filepath.Join(e.home, "config.yaml")) != "model: secret-config\n" {
		t.Fatal("written")
	}
}

// Runs swap once right after the door realpath'd checked
func afterRealpath(checked string, swap func()) {
	done := false
	orig := pfs.realpath
	pfs.realpath = func(target string) (string, error) {
		real, err := orig(target)
		if !done && target == checked {
			done = true
			swap()
		}
		return real, err
	}
}

func TestASwapBeforeTheFinalLookIsRefusedTooACheckedFileSwappedForALinkToIcebathDataNotReadNotWritten(t *testing.T) {
	if !posix() {
		return
	}
	keepPFS(t)
	e := hardEnv(t)
	id := e.grant(t, "plexus", e.vault)
	note := filepath.Join(e.vault, "notes", "a.md")
	swap := func() {
		os.Remove(note)
		os.Symlink(filepath.Join(e.home, "config.yaml"), note)
	}
	saved := pfs
	afterRealpath(note, swap)
	read := e.run("plexus", map[string]any{"op": "readText", "folder": id, "path": "notes/a.md"})
	raw, _ := json.Marshal(read)
	if read["ok"] != false || strings.Contains(string(raw), "secret-config") {
		t.Fatal(read)
	}
	pfs = saved
	os.Remove(note)
	writeFile(t, note, "# A\n")
	afterRealpath(note, swap)
	expectFail(t, e.run("plexus", map[string]any{"op": "writeText", "folder": id, "path": "notes/a.md", "text": "pwned"}), "")
	if readText(t, filepath.Join(e.home, "config.yaml")) != "model: secret-config\n" {
		t.Fatal("written")
	}
}

func TestASwapBeforeTheFinalLookIsRefusedTooAParentFolderSwappedForALinkIntoTheIcebathHomeNotReadNotWritten(t *testing.T) {
	if !posix() {
		return
	}
	keepPFS(t)
	e := hardEnv(t)
	id := e.grant(t, "plexus", e.vault)
	writeFile(t, filepath.Join(e.home, "a.md"), "hermes-secret\n")
	notes := filepath.Join(e.vault, "notes")
	note := filepath.Join(notes, "a.md")
	swap := func() {
		os.Rename(notes, filepath.Join(e.vault, "notes-old"))
		os.Symlink(e.home, notes)
	}
	saved := pfs
	afterRealpath(note, swap)
	read := e.run("plexus", map[string]any{"op": "readText", "folder": id, "path": "notes/a.md"})
	raw, _ := json.Marshal(read)
	if read["ok"] != false || strings.Contains(string(raw), "hermes-secret") {
		t.Fatal(read)
	}
	pfs = saved
	os.Remove(notes)
	os.Rename(filepath.Join(e.vault, "notes-old"), notes)
	afterRealpath(note, swap)
	expectFail(t, e.run("plexus", map[string]any{"op": "writeText", "folder": id, "path": "notes/a.md", "text": "pwned"}), "")
	if readText(t, filepath.Join(e.home, "a.md")) != "hermes-secret\n" {
		t.Fatal("written")
	}
}

// Swaps notes for a link into the home right after the second lstat of entry, the end of resolve
func swapParentAfterResolve(entry, notes, home string) {
	seen := 0
	orig := pfs.lstat
	pfs.lstat = func(target string) (fs.FileInfo, error) {
		st, err := orig(target)
		if target == entry {
			seen++
			if seen == 2 {
				os.Rename(notes, notes+"-old")
				os.Symlink(home, notes)
			}
		}
		return st, err
	}
}

func parentSwapEnv(t *testing.T) (*pfEnv, string) {
	keepPFS(t)
	e := hardEnv(t)
	writeFile(t, filepath.Join(e.home, "a.md"), "hermes-file\n")
	return e, e.grant(t, "plexus", e.vault)
}

func TestRenameTrashAndListAfterAParentSwapRenameDoesNotMoveAIcebathFile(t *testing.T) {
	if !posix() {
		return
	}
	e, id := parentSwapEnv(t)
	notes := filepath.Join(e.vault, "notes")
	swapParentAfterResolve(filepath.Join(notes, "a.md"), notes, e.home)
	expectFail(t, e.run("plexus", map[string]any{"op": "rename", "folder": id, "path": "notes/a.md", "name": "b.md"}), "")
	if readText(t, filepath.Join(e.home, "a.md")) != "hermes-file\n" || exists(filepath.Join(e.home, "b.md")) {
		t.Fatal("moved")
	}
}

func TestRenameTrashAndListAfterAParentSwapASwapInTheLastInstantBeforeTheRenameIsUndone(t *testing.T) {
	if !posix() {
		return
	}
	e, id := parentSwapEnv(t)
	notes := filepath.Join(e.vault, "notes")
	swapped := false
	orig := pfs.rename
	pfs.rename = func(from, to string) error {
		if !swapped && from == filepath.Join(notes, "a.md") {
			swapped = true
			os.Rename(notes, notes+"-old")
			os.Symlink(e.home, notes)
		}
		return orig(from, to)
	}
	expectFail(t, e.run("plexus", map[string]any{"op": "rename", "folder": id, "path": "notes/a.md", "name": "b.md"}), "outside-folder")
	if readText(t, filepath.Join(e.home, "a.md")) != "hermes-file\n" || exists(filepath.Join(e.home, "b.md")) {
		t.Fatal("not undone")
	}
}

func TestRenameTrashAndListAfterAParentSwapTheUndoNeverOverwritesAFileRecreatedMeanwhile(t *testing.T) {
	if !posix() {
		return
	}
	e, id := parentSwapEnv(t)
	notes := filepath.Join(e.vault, "notes")
	calls := 0
	orig := pfs.rename
	pfs.rename = func(from, to string) error {
		calls++
		if calls == 1 {
			os.Rename(notes, notes+"-old")
			os.Symlink(e.home, notes)
			err := orig(from, to)
			os.WriteFile(filepath.Join(e.home, "a.md"), []byte("newer\n"), 0o644)
			return err
		}
		return orig(from, to)
	}
	expectFail(t, e.run("plexus", map[string]any{"op": "rename", "folder": id, "path": "notes/a.md", "name": "b.md"}), "outside-folder")
	if readText(t, filepath.Join(e.home, "a.md")) != "newer\n" || readText(t, filepath.Join(e.home, "b.md")) != "hermes-file\n" {
		t.Fatal("overwrote")
	}
}

func TestRenameTrashAndListAfterAParentSwapTrashDoesNotRemoveAIcebathFile(t *testing.T) {
	if !posix() {
		return
	}
	e, id := parentSwapEnv(t)
	notes := filepath.Join(e.vault, "notes")
	swapParentAfterResolve(filepath.Join(notes, "a.md"), notes, e.home)
	expectFail(t, e.run("plexus", map[string]any{"op": "trash", "folder": id, "path": "notes/a.md"}), "")
	if readText(t, filepath.Join(e.home, "a.md")) != "hermes-file\n" {
		t.Fatal("trashed")
	}
}

func TestRenameTrashAndListAfterAParentSwapListDoesNotReturnTheIcebathHome(t *testing.T) {
	if !posix() {
		return
	}
	e, id := parentSwapEnv(t)
	sub := filepath.Join(e.vault, "notes", "sub")
	os.Mkdir(sub, 0o755)
	writeFile(t, filepath.Join(e.home, "sub", "config.yaml"), "x\n")
	swapParentAfterResolve(sub, filepath.Join(e.vault, "notes"), e.home)
	listed := e.run("plexus", map[string]any{"op": "list", "folder": id, "path": "notes/sub"})
	raw, _ := json.Marshal(listed)
	if listed["ok"] != false || strings.Contains(string(raw), "config.yaml") {
		t.Fatal(listed)
	}
}

type idInfo struct {
	fs.FileInfo
	dev, ino uint64
}

func (i idInfo) Identity() (uint64, uint64) { return i.dev, i.ino }

func TestIcebathDataMountedInsideAGrantedFolderRefusesEveryOpUnderIt(t *testing.T) {
	keepPFS(t)
	e := hardEnv(t)
	mounted := filepath.Join(e.vault, "mounted")
	writeFile(t, filepath.Join(mounted, "config.yaml"), "mirror\n")
	id := e.grant(t, "plexus", e.vault)
	homeSt, _ := os.Stat(e.home)
	dev, ino, _ := fileIdentity(homeSt, e.home)
	orig := pfs.stat
	pfs.stat = func(target string) (fs.FileInfo, error) {
		st, err := orig(target)
		if err == nil && target == mounted {
			return idInfo{st, dev, ino}, nil
		}
		return st, err
	}
	for _, req := range []map[string]any{
		{"op": "readText", "path": "mounted/config.yaml"}, {"op": "list", "path": "mounted"},
		{"op": "stat", "path": "mounted/config.yaml"}, {"op": "writeText", "path": "mounted/new.md", "text": "x"},
	} {
		req["folder"] = id
		expectFail(t, e.run("plexus", req), "protected")
	}
	if exists(filepath.Join(mounted, "new.md")) {
		t.Fatal("written")
	}
}

func TestTheGrantsRecordARecordThatCannotBeReadIsNotOverwrittenWithNothing(t *testing.T) {
	if !unprivileged() {
		return
	}
	e := hardEnv(t)
	e.grant(t, "plexus", e.vault)
	record := filepath.Join(e.userData, "plugin-files", "grants.json")
	before := readText(t, record)
	os.Chmod(record, 0)
	other := filepath.Join(e.scratch, "Other")
	os.Mkdir(other, 0o755)
	expectFail(t, e.make().AddGrant("plexus", other), "io-error")
	os.Chmod(record, 0o600)
	if readText(t, record) != before {
		t.Fatal("overwritten")
	}
}

func TestTheGrantsRecordARevokeThatCannotBeSavedDoesNotTakeEffectNowOrAfterRestart(t *testing.T) {
	if !unprivileged() {
		return
	}
	e := hardEnv(t)
	id := e.grant(t, "plexus", e.vault)
	base := filepath.Join(e.userData, "plugin-files")
	os.Chmod(base, 0o500)
	if _, err := e.files.Revoke("plexus", id, false); err == nil {
		t.Fatal("revoke saved")
	}
	os.Chmod(base, 0o700)
	if ids := grantIDs(t, e.files, "plexus"); len(ids) != 1 || ids[0] != id {
		t.Fatal(ids)
	}
	if ids := grantIDs(t, e.make(), "plexus"); len(ids) != 1 || ids[0] != id {
		t.Fatal(ids)
	}
}

func TestTheGrantsRecordAFirstReadStillInFlightCannotUndoARevokeMadeMeanwhile(t *testing.T) {
	keepPFS(t)
	e := hardEnv(t)
	id := e.grant(t, "plexus", e.vault)
	fresh := e.make()
	release := make(chan struct{})
	calls := 0
	orig := pfs.readFile
	pfs.readFile = func(p string) ([]byte, error) {
		data, err := orig(p)
		if strings.HasSuffix(p, "grants.json") {
			calls++
			if calls == 1 {
				<-release
			}
		}
		return data, err
	}
	reading := make(chan []string, 1)
	revoking := make(chan map[string]any, 1)
	go func() { reading <- grantIDs(t, fresh, "plexus") }()
	time.Sleep(50 * time.Millisecond)
	go func() { r, _ := fresh.Revoke("plexus", id, false); revoking <- r }()
	time.Sleep(200 * time.Millisecond)
	close(release)
	<-reading
	if r := <-revoking; r["removed"] != 1 {
		t.Fatal(r)
	}
	if ids := grantIDs(t, fresh, "plexus"); len(ids) != 0 {
		t.Fatal(ids)
	}
	expectFail(t, fresh.Run("plexus", map[string]any{"op": "list", "folder": id, "path": ""}), "not-granted")
}

func TestTheGrantsRecordTreatsAnIdSuchAsConstructorAsAPlainKey(t *testing.T) {
	e := hardEnv(t)
	if got := e.files.AddGrant("constructor", e.vault); got["ok"] != true {
		t.Fatal(got)
	}
	if ids := grantIDs(t, e.files, "toString"); len(ids) != 0 {
		t.Fatal(ids)
	}
}

func TestTheGrantsRecordRefusesThePluginIdWhichWindowsWouldAlias(t *testing.T) {
	for _, id := range []string{"foo.", "con", "NUL.txt", "com1"} {
		e := hardEnv(t)
		expectFail(t, e.run(id, map[string]any{"op": "list", "folder": "data", "path": ""}), "invalid-plugin")
	}
}
