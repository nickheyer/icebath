package home

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestScratchEnvFollowsHomeAndRespectsUserTmpdir(t *testing.T) {
	scratchPruneSpawns(t)
	tmp := t.TempDir()
	homeA, homeB := filepath.Join(tmp, "a"), filepath.Join(tmp, "b")
	env := map[string]string{"ICEBATH_HOME": homeA}
	if !ApplyScratchTmpEnv(env) {
		t.Fatal("unset temp vars must be written")
	}
	scratchA := filepath.Join(homeA, "cache", "scratch")
	for _, k := range []string{"TMPDIR", "TMP", "TEMP", ScratchDirMarkerEnv} {
		if env[k] != scratchA {
			t.Fatalf("%s = %q, want %q", k, env[k], scratchA)
		}
	}
	if st, err := os.Stat(scratchA); err != nil || !st.IsDir() {
		t.Fatal("scratch dir not created")
	}
	env["ICEBATH_HOME"] = homeB
	if !ApplyScratchTmpEnv(env) || env["TMPDIR"] != filepath.Join(homeB, "cache", "scratch") {
		t.Fatalf("a child under another home must get that home's scratch, got %q", env["TMPDIR"])
	}
	user := map[string]string{"ICEBATH_HOME": homeA, "TMPDIR": "/var/folders/zz"}
	if ApplyScratchTmpEnv(user) || user["TMPDIR"] != "/var/folders/zz" {
		t.Fatal("a user set TMPDIR must be respected")
	}
	if _, ok := user[ScratchDirMarkerEnv]; ok {
		t.Fatal("marker written for a user owned temp dir")
	}
	if _, ok := user["TMP"]; ok {
		t.Fatal("a partially user set triple must be left exactly as found")
	}
}

func TestBootExportPointsProcessAndChildrenAtScratch(t *testing.T) {
	tmp := t.TempDir()
	cmd := testChild("export")
	cmd.Env = slices.DeleteFunc(cmd.Env, func(kv string) bool {
		for _, k := range []string{"TMPDIR=", "TMP=", "TEMP=", ScratchDirMarkerEnv + "="} {
			if strings.HasPrefix(kv, k) {
				return true
			}
		}
		return false
	})
	cmd.Env = append(cmd.Env, "ICEBATH_HOME="+tmp)
	out, err := cmd.Output()
	expected := filepath.Join(tmp, "cache", "scratch")
	if got := strings.Fields(string(out)); err != nil || len(got) != 2 || got[0] != expected || got[1] != expected {
		t.Fatalf("tempdirs %q (%v), want two of %s", out, err, expected)
	}
}

func TestBootHookAndEnvHandThePruneToOneChild(t *testing.T) {
	spawned := scratchPruneSpawns(t)
	h := filepath.Join(t.TempDir(), "launch")
	t.Setenv("ICEBATH_HOME", h)
	scratch := filepath.Join(h, "cache", "scratch")
	env := map[string]string{"ICEBATH_HOME": h}
	if !ApplyScratchTmpEnv(env) || env["TMPDIR"] != scratch {
		t.Fatalf("TMPDIR %q", env["TMPDIR"])
	}
	if _, err := EnsureScratchDir(""); err != nil {
		t.Fatal(err)
	}
	if len(*spawned) != 1 || (*spawned)[0] != scratch {
		t.Fatalf("spawned %v", *spawned)
	}
}

func TestSocketSafeTmpdirFallsBackForLongRoots(t *testing.T) {
	short := t.TempDir()
	t.Setenv("TMPDIR", short)
	got, err := SocketSafeTmpdir()
	if err != nil {
		t.Fatal(err)
	}
	if len(short) <= SocketTmpdirMaxLen && got != short && got != "/tmp" {
		t.Fatalf("short root %q replaced by %q", short, got)
	}
	long := filepath.Join(short, strings.Repeat("d", SocketTmpdirMaxLen))
	if err := os.Mkdir(long, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", long)
	if got, err := SocketSafeTmpdir(); err != nil || (got == long && func() bool { st, e := os.Stat("/tmp"); return e == nil && st.IsDir() }()) {
		t.Fatalf("long root kept: %q %v", got, err)
	}
}
