package files

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func spellEnv(t *testing.T, platform string) *pfEnv {
	e := &pfEnv{scratch: realTemp(t), platform: platform}
	e.userData = filepath.Join(e.scratch, "userData")
	e.home = filepath.Join(e.scratch, "home", ".icebath")
	e.vault = filepath.Join(e.scratch, "home", "Vault")
	os.MkdirAll(e.userData, 0o755)
	os.MkdirAll(e.home, 0o755)
	writeFile(t, filepath.Join(e.vault, "a.md"), "# A\n")
	e.files = e.make()
	return e
}

func TestCaseOnACaseSensitiveMacOSVolumeALinkFromTheGrantedVaultIntoASiblingVaultIsOutsideTheFolder(t *testing.T) {
	if !posix() {
		return
	}
	e := spellEnv(t, "darwin")
	id := e.grant(t, "plexus", e.vault)
	sibling := filepath.Join(e.scratch, "home", "vault")
	writeFile(t, filepath.Join(sibling, "secret.txt"), "not granted")
	mustSymlink(t, filepath.Join(sibling, "secret.txt"), filepath.Join(e.vault, "link.txt"))
	read := e.run("plexus", map[string]any{"op": "readText", "folder": id, "path": "link.txt"})
	raw, _ := json.Marshal(read)
	if read["error"] != "outside-folder" || strings.Contains(string(raw), "not granted") {
		t.Fatal(read)
	}
}

func TestCaseOnACaseSensitiveMacOSVolumeAGrantedVaultReplacedByADifferentlyCasedVaultMustBeGrantedAgain(t *testing.T) {
	if !posix() {
		return
	}
	e := spellEnv(t, "darwin")
	id := e.grant(t, "plexus", e.vault)
	os.Rename(e.vault, filepath.Join(e.scratch, "home", "vault"))
	mustSymlink(t, filepath.Join(e.scratch, "home", "vault"), e.vault)
	expectFail(t, e.run("plexus", map[string]any{"op": "list", "folder": id, "path": ""}), "folder-changed")
}

func TestCaseOnACaseSensitiveMacOSVolumeADifferentlyCasedSpellingOfTheIcebathHomeStillCannotBeGranted(t *testing.T) {
	if !posix() {
		return
	}
	e := spellEnv(t, "darwin")
	lookalike := filepath.Join(e.scratch, "home", ".ICEBATH")
	os.Mkdir(lookalike, 0o755)
	expectFail(t, e.files.AddGrant("plexus", lookalike), "protected")
}

func TestSensitiveNamesAfterUnicodeFoldingRefusesReadingOrWriting(t *testing.T) {
	for _, name := range []string{"id_r\u017fa", ".ENV", "\uff0eenv", ".env.LOCAL", ".e\u200dnv", "id_\u200brsa", ".np\ufeffmrc"} {
		t.Run(name, func(t *testing.T) {
			e := spellEnv(t, "")
			id := e.grant(t, "plexus", e.vault)
			writeFile(t, filepath.Join(e.vault, name), "SECRET=1\n")
			expectFail(t, e.run("plexus", map[string]any{"op": "readText", "folder": id, "path": name}), "sensitive-file")
			os.Mkdir(filepath.Join(e.vault, "fresh"), 0o755)
			expectFail(t, e.run("plexus", map[string]any{"op": "writeText", "folder": id, "path": "fresh/" + name, "text": "x"}), "sensitive-file")
			if exists(filepath.Join(e.vault, "fresh", name)) {
				t.Fatal("written")
			}
		})
	}
}

func TestSensitiveNamesAfterUnicodeFoldingRefusesAFoldedKeyFolderAsAPathAndAsARenameTarget(t *testing.T) {
	e := spellEnv(t, "")
	id := e.grant(t, "plexus", e.vault)
	writeFile(t, filepath.Join(e.vault, ".\u017f\u017fh", "config"), "Host x\n")
	expectFail(t, e.run("plexus", map[string]any{"op": "readText", "folder": id, "path": ".\u017f\u017fh/config"}), "sensitive-file")
	expectFail(t, e.run("plexus", map[string]any{"op": "rename", "folder": id, "path": "a.md", "name": ".ENV"}), "sensitive-file")
}

func TestMacOSNamedForksRefuses(t *testing.T) {
	for _, rel := range []string{".env/..namedfork/data", "a.md/..namedfork/rsrc", "x/..NamedFork/data"} {
		e := spellEnv(t, "")
		id := e.grant(t, "plexus", e.vault)
		writeFile(t, filepath.Join(e.vault, ".env"), "SECRET=1\n")
		expectFail(t, e.run("plexus", map[string]any{"op": "readText", "folder": id, "path": rel}), "invalid-path")
	}
}

func TestWindowsNamesThatOpenSomethingElseRefusesTheSegment(t *testing.T) {
	for _, rel := range []string{"CON", "nul.txt", "COM1", "lpt\u00b9", "CONIN$", "conout$.log", "CLOCK$", "a.md:secret", ".env.", ".env ", `notes\..\..`} {
		e := spellEnv(t, "win32")
		id := e.grant(t, "plexus", e.vault)
		expectFail(t, e.run("plexus", map[string]any{"op": "readText", "folder": id, "path": rel}), "invalid-path")
	}
}

func TestWindowsSecondPathsToThisMachineTreatsTheWSLPathAsProtected(t *testing.T) {
	for _, tc := range []struct {
		candidate string
		exposed   bool
	}{
		{`\\wsl.localhost\Ubuntu`, true}, {`\\wsl$\Ubuntu\home`, true}, {`\\wsl.localhost\Ubuntu\home\me`, true},
		{`\\wsl.localhost\Ubuntu\root`, true}, {`\\wsl.localhost\Ubuntu\home\me\.icebath\profiles`, true},
		{`\\WSL.LOCALHOST\Debian\HOME\me\.Icebath`, true}, {`\\wsl.localhost\Ubuntu\proc`, true},
		{`\\wsl$\Ubuntu\proc\1\environ`, true}, {`\\wsl.localhost\Ubuntu\sys`, true}, {`\\wsl.localhost\Ubuntu\dev`, true},
		{`\\wsl.localhost\Ubuntu\run\user`, true}, {`\\wsl.localhost\Ubuntu\mnt\c\Users\me`, true},
		{`\\wsl.localhost\Ubuntu\home\me\Vault`, false}, {`\\server\share\home`, false}, {`C:\Users\me\Vault`, false},
	} {
		if WslProtectedPath(tc.candidate) != tc.exposed {
			t.Fatal(tc.candidate)
		}
	}
}

func TestWindowsSecondPathsToThisMachineTreatsAsASecondPathToThisMachine(t *testing.T) {
	host, _ := os.Hostname()
	for _, tc := range []struct {
		candidate string
		loopback  bool
	}{
		{`\\localhost\C$\Users\me`, true}, {`\\LOCALHOST\share`, true}, {`\\127.0.0.1\c$\Users`, true},
		{`\\127.1.2.3\c$`, true}, {`\\[::1]\c$`, true}, {`\\0--1.ipv6-literal.net\c$`, true},
		{`\\?\UNC\localhost\c$\Users`, true}, {`\\.\C:\Users`, true}, {`\\?\C:\Users`, true},
		{`\\` + host + `\c$`, true}, {`\\fileserver\team\notes`, false}, {`C:\Users\me\Vault`, false},
	} {
		if LoopbackShare(tc.candidate) != tc.loopback {
			t.Fatal(tc.candidate)
		}
	}
}

func TestKernelPseudoFilesystemsWhereverTheyAreMountedRecognizesProcfsAndSysfsByFilesystemType(t *testing.T) {
	if runtime.GOOS != "linux" {
		return
	}
	e := spellEnv(t, "")
	if !onPseudoFilesystem("/proc/self/environ") || !onPseudoFilesystem("/sys/kernel") || onPseudoFilesystem(e.vault) {
		t.Fatal("pseudo")
	}
}

func TestKernelPseudoFilesystemsWhereverTheyAreMountedRefusesAFileWhoseFilesystemIsProcfs(t *testing.T) {
	if runtime.GOOS != "linux" {
		return
	}
	keepPFS(t)
	e := spellEnv(t, "")
	id := e.grant(t, "plexus", e.vault)
	environ := filepath.Join(e.vault, "environ")
	writeFile(t, environ, "OPENAI_API_KEY=sk-live\n")
	orig := pfs.statfs
	pfs.statfs = func(target string) (int64, error) {
		if target == environ {
			return 0x9fa0, nil
		}
		return orig(target)
	}
	for _, op := range []string{"readText", "readDataUrl", "stat"} {
		expectFail(t, e.run("plexus", map[string]any{"op": op, "folder": id, "path": "environ"}), "protected")
	}
	expectFail(t, e.run("plexus", map[string]any{"op": "writeText", "folder": id, "path": "environ", "text": "x"}), "protected")
}

func TestIcebathDataReachedByAnotherSpellingComparesFoldersByFilesystemIdentity(t *testing.T) {
	e := spellEnv(t, "")
	if !OverlapsByIdentity(e.home, []string{e.home}) || !OverlapsByIdentity(filepath.Join(e.scratch, "home"), []string{e.home}) {
		t.Fatal("identity")
	}
	os.Mkdir(filepath.Join(e.home, "skills"), 0o755)
	if !OverlapsByIdentity(filepath.Join(e.home, "skills"), []string{e.home}) || OverlapsByIdentity(e.vault, []string{e.home}) {
		t.Fatal("identity")
	}
}
