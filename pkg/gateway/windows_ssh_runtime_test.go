package gateway

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Builds a state root with live, identity less, tombstoned, rollback and badly named profile directories
func sshRoster(t *testing.T, root string) string {
	t.Helper()
	profiles := filepath.Join(root, "profiles")
	if err := os.MkdirAll(filepath.Join(profiles, ".deleted"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path string) {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "config.yaml"))
	for _, name := range []string{"bob", "win-research", "ghost", "tombstoned", ".rollback-1", "bad name"} {
		if err := os.Mkdir(filepath.Join(profiles, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if name != "ghost" {
			write(filepath.Join(profiles, name, "config.yaml"))
		}
	}
	write(filepath.Join(profiles, ".deleted", "tombstoned"))
	return profiles
}

func sshInventoryResult(t *testing.T) *pyjson.Object {
	t.Helper()
	result, err := DispatchWindowsSSHRuntime(context.Background(), []string{"inventory"})
	if err != nil {
		t.Fatal(err)
	}
	return result.(*pyjson.Object)
}

func inventoryProfiles(result *pyjson.Object) []string {
	names := []string{}
	for _, v := range result.GetOr("profiles", nil).([]any) {
		names = append(names, v.(string))
	}
	return names
}

func TestInventoryListsLiveProfilesWithoutMintingAnIdentity(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		root := filepath.Join(t.TempDir(), "state-root")
		profiles := sshRoster(t, root)
		if pinned {
			t.Setenv("ICEBATH_HOME", filepath.Join(profiles, "bob"))
		} else {
			t.Setenv("ICEBATH_HOME", root)
		}
		result := sshInventoryResult(t)
		if !slices.Equal(inventoryProfiles(result), []string{"default", "bob", "win-research"}) || result.GetOr("installId", "unset") != nil {
			text, _ := pyjson.Dumps(result)
			t.Fatalf("pinned %v: result = %s", pinned, text)
		}
		if _, err := os.Stat(filepath.Join(root, "install_id")); err == nil {
			t.Fatal("an install id was minted")
		}
	}
}

func TestALinkInsideAProfileDoesNotHideTheRoster(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "state-root")
	profiles := sshRoster(t, root)
	if err := os.Symlink(tmp, filepath.Join(profiles, "bob", "workspace")); err != nil {
		t.Skip("symlinks unavailable on this platform")
	}
	t.Setenv("ICEBATH_HOME", root)
	if got := inventoryProfiles(sshInventoryResult(t)); !slices.Equal(got, []string{"default", "bob", "win-research"}) {
		t.Fatalf("profiles = %v", got)
	}
}

func TestANestedHomeReadsTheIdentityOfItsInstallRoot(t *testing.T) {
	tmp := t.TempDir()
	t.Cleanup(home.PatchUserHome(tmp))
	native := home.PlatformDefault()
	if err := os.MkdirAll(filepath.Join(native, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(native, "install_id"), []byte(strings.Repeat("A", 32)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", filepath.Join(native, "work"))
	if got := sshInventoryResult(t).GetOr("installId", nil); got != strings.Repeat("a", 32) {
		t.Fatalf("installId = %v", got)
	}
}

// Value of a name in an environment list, not ok when absent
func envListGet(env []string, name string) (string, bool) {
	for _, entry := range env {
		if k, v, _ := strings.Cut(entry, "="); k == name {
			return v, true
		}
	}
	return "", false
}

func TestSpawnEnvCarriesOnlyThePayloadsDevRenderer(t *testing.T) {
	for _, c := range []struct {
		payload  *pyjson.Object
		expected string
		set      bool
	}{
		{pyjson.ObjectOf("devServer", "http://127.0.0.1:5175"), "http://127.0.0.1:5175", true},
		{pyjson.ObjectOf("devServer", ""), "", false},
		{pyjson.NewObject(), "", false},
	} {
		t.Setenv("ICEBATH_DESKTOP_DEV_SERVER", "http://127.0.0.1:5999")
		t.Setenv("VIRTUAL_ENV", `C:\venv`)
		env, err := sshSpawnEnv(c.payload)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := envListGet(env, "ICEBATH_DESKTOP_DEV_SERVER"); ok != c.set || got != c.expected {
			t.Fatalf("dev server = %q, %v", got, ok)
		}
		if _, ok := envListGet(env, "VIRTUAL_ENV"); ok {
			t.Fatal("VIRTUAL_ENV was handed down")
		}
	}
}

func TestSpawnEnvRefusesAMalformedDevRenderer(t *testing.T) {
	for _, devServer := range []string{"http://127.0.0.1:5175\r\nX=1", "http://a\x00b", strings.Repeat("h", 2049)} {
		if _, err := sshSpawnEnv(pyjson.ObjectOf("devServer", devServer)); err == nil || !strings.Contains(err.Error(), "devServer") {
			t.Fatalf("err = %v", err)
		}
	}
}

// Pins the creation time and argv the runtime reads for every pid, the process counting as running
func pinSSHProcess(t *testing.T, birth float64, argv []string) {
	t.Helper()
	swapSeam(t, &sshProcBirth, func(int) (float64, error) { return birth, nil })
	swapSeam(t, &sshProcCmdline, func(int) ([]string, error) { return argv, nil })
	swapSeam(t, &sshProcExists, func(int) bool { return true })
}

func sshState(t *testing.T, pid int, created int64, launcher, nonce string) *pyjson.Object {
	t.Helper()
	state, err := ProcessState(pid, created, launcher, nonce)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestIsolatedBootstrapIsOwnedWithoutAcceptingForeignSource(t *testing.T) {
	tmp := t.TempDir()
	source := "import os, sys, runpy; sys.path.insert(0, '" + tmp + "'); runpy.run_module('icebath_cli.main', run_name='__main__', alter_sys=True)"
	nonce := strings.Repeat("a", 16)
	argv := []string{"/usr/bin/python3", "-I", "-c", source, "serve", "--isolated", "--ssh-owner-nonce", nonce}
	pinSSHProcess(t, 1_700_000_000.0, argv)
	const created = int64(1_700_000_000_000_000_000)
	launcher := filepath.Join(tmp, "icebath.exe")
	state := sshState(t, 123, created, launcher, nonce)
	if state.GetOr("alive", nil) != true || state.GetOr("owned", nil) != true {
		text, _ := pyjson.Dumps(state)
		t.Fatalf("state = %s", text)
	}
	if sshState(t, 123, created, launcher, strings.Repeat("b", 16)).GetOr("owned", nil) != false {
		t.Fatal("a foreign nonce was owned")
	}
	if sshState(t, 123, created+1, launcher, nonce).GetOr("owned", nil) != false {
		t.Fatal("a different creation time was owned")
	}
}

func TestForeignInlineSourceCarryingAModuleNameIsNotOwnership(t *testing.T) {
	nonce := strings.Repeat("a", 16)
	pinSSHProcess(t, 1_700_000_000.0, []string{"/usr/bin/python3", "-c", "print('icebath_cli.main')", "serve", "--isolated", "--ssh-owner-nonce", nonce})
	state := sshState(t, 123, 1_700_000_000_000_000_000, filepath.Join(t.TempDir(), "icebath.exe"), nonce)
	if state.GetOr("alive", nil) != true || state.GetOr("owned", nil) != false {
		text, _ := pyjson.Dumps(state)
		t.Fatalf("state = %s", text)
	}
}

// The module entry prints the compact JSON result on stdout, or the error on stderr with status one
func TestWindowsSSHRuntimeMainPrintsTheResultOrTheError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state-root")
	sshRoster(t, root)
	t.Setenv("ICEBATH_HOME", root)
	var out, errw strings.Builder
	if code := WindowsSSHRuntimeMain(context.Background(), []string{"inventory"}, &out, &errw); code != 0 || errw.Len() != 0 {
		t.Fatalf("inventory exit %d stderr %q", code, errw.String())
	}
	if !strings.HasPrefix(out.String(), `{"`) || strings.Contains(out.String(), `", "`) || !strings.Contains(out.String(), `"profiles":["default","bob","win-research"]`) {
		t.Fatalf("stdout %q", out.String())
	}
	out.Reset()
	if code := WindowsSSHRuntimeMain(context.Background(), []string{"no-such-op"}, &out, &errw); code != 1 || out.Len() != 0 || errw.String() != "{\"error\":\"invalid operation\"}\n" {
		t.Fatalf("bad op exit %d stdout %q stderr %q", code, out.String(), errw.String())
	}
}
