package pm

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Isolated home with an install root holding a fake binary, returned as user home and install root
func lnFakeInstall(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX exposure")
	}
	userHome := resolvePath(isolateHome(t))
	root := filepath.Join(userHome, "install")
	if err := os.MkdirAll(root, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "icebath"), []byte("\x7fELF fake\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvInstallRoot, root)
	return userHome, root
}

func lnExpose(t *testing.T, create bool) ExposeResult {
	t.Helper()
	result, err := ExposeCLI(context.Background(), "", create)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func lnMtimes(t *testing.T, paths ...string) []int64 {
	t.Helper()
	var out []int64
	for _, path := range paths {
		st, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, st.ModTime().UnixNano())
	}
	return out
}

func TestExposeCliRepairOnlyPreservesMissingAndForeignCommands(t *testing.T) {
	for _, icebath := range []string{"missing", "foreign"} {
		t.Run(icebath, func(t *testing.T) {
			userHome, root := lnFakeInstall(t)
			wrapperDir := filepath.Join(userHome, ".local", "bin")
			extra := filepath.Join(home.Root(), "bin")
			if got := lnExpose(t, false); !got.OK || len(got.Written) != 0 || got.Skipped != "" {
				t.Fatalf("first repair %+v", got)
			}
			if lexists(wrapperDir) || lexists(extra) {
				t.Fatal("repair created exposure")
			}
			if !isRegularFile(filepath.Join(root, "icebath-acp")) {
				t.Fatal("own acp command missing")
			}
			if err := os.MkdirAll(extra, 0o777); err != nil {
				t.Fatal(err)
			}
			dangling := filepath.Join(root, "old", "icebath-acp")
			acp := filepath.Join(extra, "icebath-acp")
			if err := os.Symlink(dangling, acp); err != nil {
				t.Fatal(err)
			}
			foreign := []byte("#!/bin/sh\n# user-owned caf\xc3\xa9 command\nexit 19\n")
			if icebath == "foreign" {
				if err := os.WriteFile(filepath.Join(extra, "icebath"), foreign, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(wrapperDir, 0o777); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(wrapperDir, "icebath"), foreign, 0o755); err != nil {
				t.Fatal(err)
			}
			foreignLink := filepath.Join(wrapperDir, "icebath-acp")
			foreignTarget := filepath.Join(userHome, "other-install", "icebath-acp")
			if err := os.Symlink(foreignTarget, foreignLink); err != nil {
				t.Fatal(err)
			}
			watched := []string{wrapperDir, filepath.Join(wrapperDir, "icebath"), foreignLink}
			before := lnMtimes(t, watched...)
			if got := lnExpose(t, false); !got.OK || !reflect.DeepEqual(got.Written, []string{"icebath-acp"}) {
				t.Fatalf("repair %+v", got)
			}
			if isSymlink(acp) || !isRegularFile(acp) || !executable(acp) || lexists(dangling) {
				t.Fatal("owned acp link not replaced by a wrapper")
			}
			if lexists(filepath.Join(extra, "icebath-agent")) || lexists(filepath.Join(wrapperDir, "icebath-agent")) {
				t.Fatal("repair created a new command")
			}
			if data, _ := os.ReadFile(filepath.Join(extra, "icebath")); icebath == "foreign" && string(data) != string(foreign) {
				t.Fatal("foreign command rewritten")
			} else if icebath == "missing" && lexists(filepath.Join(extra, "icebath")) {
				t.Fatal("missing command created")
			}
			if data, _ := os.ReadFile(filepath.Join(wrapperDir, "icebath")); string(data) != string(foreign) {
				t.Fatal("foreign user bin command rewritten")
			}
			if target, _ := os.Readlink(foreignLink); target != foreignTarget {
				t.Fatal("foreign link replaced")
			}
			if after := lnMtimes(t, watched...); !reflect.DeepEqual(before, after) {
				t.Fatal("untouched entries changed")
			}
			if got := lnExpose(t, false); !got.OK || len(got.Written) != 0 {
				t.Fatalf("second repair %+v", got)
			}
		})
	}
}

func TestExposeCliWritesAllThreeWrappersFresh(t *testing.T) {
	userHome, root := lnFakeInstall(t)
	result := lnExpose(t, true)
	written := append([]string{}, result.Written...)
	sort.Strings(written)
	if !result.OK || !reflect.DeepEqual(written, []string{"icebath", "icebath-acp", "icebath-agent"}) {
		t.Fatalf("result %+v", result)
	}
	for _, name := range []string{"icebath", "icebath-agent", "icebath-acp"} {
		wrapper := filepath.Join(userHome, ".local", "bin", name)
		body, err := os.ReadFile(wrapper)
		if err != nil || !strings.Contains(string(body), root) || !executable(wrapper) {
			t.Fatalf("%s body %q err %v", name, body, err)
		}
	}
}

func TestExposeCliLeavesAnotherInstallsWrapperAlone(t *testing.T) {
	userHome, _ := lnFakeInstall(t)
	wrapperDir := filepath.Join(userHome, ".local", "bin")
	if err := os.MkdirAll(wrapperDir, 0o777); err != nil {
		t.Fatal(err)
	}
	foreign := "#!/bin/sh\nexec \"/somewhere/else/install/icebath\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(wrapperDir, "icebath"), []byte(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	result := lnExpose(t, true)
	if data, _ := os.ReadFile(filepath.Join(wrapperDir, "icebath")); string(data) != foreign {
		t.Fatal("foreign wrapper rewritten")
	}
	written := append([]string{}, result.Written...)
	sort.Strings(written)
	if !reflect.DeepEqual(written, []string{"icebath-acp", "icebath-agent"}) {
		t.Fatalf("written %v", written)
	}
}

func TestExposeCliConfigGateDisables(t *testing.T) {
	lnFakeInstall(t)
	if err := os.WriteFile(filepath.Join(home.Dir(context.Background()), "config.yaml"), []byte("cli:\n  expose_on_path: false\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
	if got := lnExpose(t, true); !reflect.DeepEqual(got, ExposeResult{OK: true, Skipped: "config-disabled"}) {
		t.Fatalf("result %+v", got)
	}
}

func TestExposeCliSealedTreeWithoutVenvSkips(t *testing.T) {
	userHome, _ := lnFakeInstall(t)
	bare := filepath.Join(userHome, "sealed")
	if err := os.MkdirAll(bare, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvInstallRoot, bare)
	if got := lnExpose(t, true); !reflect.DeepEqual(got, ExposeResult{OK: true, Skipped: "no-install-binary"}) {
		t.Fatalf("result %+v", got)
	}
}

func TestExposeCliBundledTreeSkipsBundleOwnsLaunchers(t *testing.T) {
	for _, create := range []bool{true, false} {
		if create && runtime.GOOS != "linux" {
			continue
		}
		userHome, _ := lnFakeInstall(t)
		payload := filepath.Join(userHome, "agent-payload")
		repo := filepath.Join(payload, "repo")
		for _, dir := range []string{filepath.Join(payload, "bin"), repo} {
			if err := os.MkdirAll(dir, 0o777); err != nil {
				t.Fatal(err)
			}
		}
		stamp := `{"payload": "bundled", "distribution": "desktop-app", "updateMechanism": "electron-updater"}`
		if err := os.WriteFile(filepath.Join(repo, "install-stamp.json"), []byte(stamp), 0o666); err != nil {
			t.Fatal(err)
		}
		t.Setenv(EnvInstallRoot, repo)
		if got := lnExpose(t, create); !reflect.DeepEqual(got, ExposeResult{OK: true, Skipped: "bundle-owns-launchers"}) {
			t.Fatalf("create %v result %+v", create, got)
		}
		if lexists(filepath.Join(userHome, ".local", "bin")) {
			t.Fatal("bundle exposure wrote the user bin")
		}
	}
}

func TestPublishLaunchersSealedDevAndBuildLaneBug(t *testing.T) {
	_, root := lnFakeInstall(t)
	stampPath := filepath.Join(root, "install-stamp.json")
	if err := os.WriteFile(stampPath, []byte(`{"updateMechanism": "self"}`), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := publishLaunchers(context.Background(), root, false); err != nil || lexists(filepath.Join(root, "icebath-acp")) {
		t.Fatalf("sealed tree published %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := publishLaunchers(context.Background(), root, false); err != nil || !isRegularFile(filepath.Join(root, "icebath-acp")) {
		t.Fatalf("dev tree did not publish %v", err)
	}
	os.Remove(filepath.Join(root, ".git"))
	if err := os.WriteFile(stampPath, []byte(`{"distribution": "x"}`), 0o666); err != nil {
		t.Fatal(err)
	}
	err := publishLaunchers(context.Background(), root, false)
	if err == nil || !strings.Contains(err.Error(), "missing a valid 'updateMechanism' (one of self, app-installer, electron-updater, external, microsoft-store)") {
		t.Fatalf("build lane bug not raised %v", err)
	}
}

func TestLaunchersAcpWrapperExecsTheInstallBinary(t *testing.T) {
	_, root := lnFakeInstall(t)
	if err := publishLaunchers(context.Background(), root, false); err != nil {
		t.Fatal(err)
	}
	acp := filepath.Join(root, "icebath-acp")
	data, _ := os.ReadFile(acp)
	if want := "#!/bin/sh\nexec " + filepath.Join(root, "icebath") + " acp \"$@\"\n"; string(data) != want {
		t.Fatalf("body %q", data)
	}
	if launcherBinary(acp) != filepath.Join(root, "icebath") || !ownsLauncher(acp, root) {
		t.Fatal("wrapper not recognized")
	}
}

func TestLaunchersDoesNotFollowSymlinkElsewhere(t *testing.T) {
	_, root := lnFakeInstall(t)
	elsewhere := filepath.Join(t.TempDir(), "icebath-acp")
	link := filepath.Join(t.TempDir(), "icebath-acp")
	if err := os.WriteFile(elsewhere, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	if ownsLauncher(link, root) {
		t.Fatal("foreign link adopted")
	}
}

func TestLaunchersMergeUserPath(t *testing.T) {
	if merged, changed := mergeUserPath(`C:\a;;C:\Users\X\AppData\Local\icebath\bin\;D:\b`, `c:\users\x\appdata\local\icebath\bin`); changed {
		t.Fatalf("present entry merged %q", merged)
	}
	if merged, changed := mergeUserPath(`C:\a;;D:\b`, `C:\home\bin`); !changed || merged != `C:\home\bin;C:\a;D:\b` {
		t.Fatalf("merged %q", merged)
	}
	if merged, changed := mergeUserPath("", `C:\home\bin`); !changed || merged != `C:\home\bin` {
		t.Fatalf("empty merged %q", merged)
	}
}

func TestLaunchersCmdWrapperNamesItsBinary(t *testing.T) {
	cmd := filepath.Join(t.TempDir(), "icebath.cmd")
	if err := os.WriteFile(cmd, []byte("@echo off\r\n\r\n\"C:\\i b\\icebath.exe\" acp %*\r\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if got := launcherBinary(cmd); got != `C:\i b\icebath.exe` {
		t.Fatalf("binary %q", got)
	}
}

func TestResolveInstallBinaryAndEntryPointCountExported(t *testing.T) {
	root := t.TempDir()
	if got := ResolveInstallBinary(root); got != "" {
		t.Fatalf("empty tree resolved %q", got)
	}
	bin := filepath.Join(root, installBinaryName())
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ResolveInstallBinary(root); got != bin {
		t.Fatalf("got %q want %q", got, bin)
	}
	if EntryPointCount() != 2 {
		t.Fatal(EntryPointCount())
	}
}

func TestStageLauncherAndWindowsBinLaunchersExports(t *testing.T) {
	if names := WindowsBinLaunchers(); len(names) != len(windowsBinLaunchers) || names[0] != windowsBinLaunchers[0] {
		t.Fatalf("names %v", names)
	}
	if path := StageLauncher("icebath", t.TempDir(), t.TempDir()); path != "" {
		t.Fatalf("a root without an install binary staged %q", path)
	}
}
