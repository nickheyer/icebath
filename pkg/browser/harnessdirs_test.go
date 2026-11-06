package browser

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

var longestName = strings.Repeat("n", 64)

func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("linux sun_path rule")
	}
}

func sockLen(runtimeDir string) int { return len(runtimeDir + "/bu-" + longestName + ".sock") }

// Asks the kernel whether a socket path fits sun_path, a missing dir still counts as fitting
func kernelAccepts(t *testing.T, path string) bool {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		return err != syscall.EINVAL
	}
	os.Remove(path)
	return true
}

func recordClaims(t *testing.T, result bool) *[]string {
	claimed := &[]string{}
	pin(t, &claimPrivateDir, func(path string) bool { *claimed = append(*claimed, path); return result })
	return claimed
}

func boundSocket(env map[string]string, home string) string {
	rt := env["BH_RUNTIME_DIR"]
	if rt == "" {
		rt = home + "/.config/browser-harness/runtime"
	}
	return resolvePath(rt) + "/bu-" + envOr(env, "BU_NAME", "default") + ".sock"
}

func TestDeepHomeGetsADaemonSocketTheKernelAccepts(t *testing.T) {
	requireLinux(t)
	claimed := recordClaims(t, true)
	deep := filepath.Join(t.TempDir(), "profiles", strings.Repeat("research-assistant-", 3), "home")
	env := map[string]string{"HOME": deep}
	fitDefaultSocketDir(env)
	if env["BH_RUNTIME_DIR_SHARED"] != "1" || len(*claimed) != 1 || (*claimed)[0] != env["BH_RUNTIME_DIR"] {
		t.Fatalf("env %v claimed %v", env, *claimed)
	}
	if !kernelAccepts(t, boundSocket(env, deep)) {
		t.Fatal(boundSocket(env, deep))
	}
}

func TestSocketThatAlreadyBindsKeepsItsPathAndLongNameIsJudgedByItsOwnLength(t *testing.T) {
	requireLinux(t)
	claimed := recordClaims(t, true)
	env := map[string]string{"HOME": "/home/alexander.smith"}
	fitDefaultSocketDir(env)
	if env["BH_RUNTIME_DIR"] != "" || len(*claimed) != 0 {
		t.Fatalf("env %v claimed %v", env, *claimed)
	}
	env = map[string]string{"HOME": "/home/alexander.smith", "BU_NAME": longestName}
	fitDefaultSocketDir(env)
	if env["BH_RUNTIME_DIR"] == "" || !kernelAccepts(t, boundSocket(env, "/home/alexander.smith")) {
		t.Fatal(boundSocket(env, "/home/alexander.smith"))
	}
}

func TestPresetRuntimeOrTmpDirIsLeftAlone(t *testing.T) {
	claimed := recordClaims(t, true)
	for _, key := range []string{"BH_RUNTIME_DIR", "BH_TMP_DIR"} {
		env := map[string]string{"HOME": strings.Repeat("/deep", 30), key: "/x"}
		fitDefaultSocketDir(env)
		if len(env) != 2 || len(*claimed) != 0 {
			t.Fatalf("%s env %v", key, env)
		}
	}
}

func TestRuntimeRootIsTheUsersPrivateXDGDirElseNone(t *testing.T) {
	realClaimPrivateDir(t)
	tmp := t.TempDir()
	private, open, link := filepath.Join(tmp, "rt-private"), filepath.Join(tmp, "rt-open"), filepath.Join(tmp, "rt-link")
	os.Mkdir(private, 0o700)
	os.Mkdir(open, 0o755)
	os.Chmod(open, 0o755)
	os.Symlink(private, link)
	for raw, want := range map[string]string{private: private, open: "", link: "", "relative/rt": "", filepath.Join(tmp, "missing"): ""} {
		t.Setenv("XDG_RUNTIME_DIR", raw)
		if got := privateUserRuntimeDir(); got != want {
			t.Errorf("%s got %q", raw, got)
		}
	}
	t.Setenv("XDG_RUNTIME_DIR", private)
	short := claimShortDir("deep-home\x00tag", 0, false)
	if short == "" || filepath.Dir(short) != private {
		t.Fatalf("short %q", short)
	}
	if info, err := os.Lstat(short); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v %v", info, err)
	}
}

func TestExistingShortDirIsClaimedOnlyWhenPrivateAndReal(t *testing.T) {
	tmp := t.TempDir()
	ok, loose, target := filepath.Join(tmp, "hbh-ok"), filepath.Join(tmp, "hbh-loose"), filepath.Join(tmp, "elsewhere")
	os.Mkdir(ok, 0o700)
	os.Mkdir(loose, 0o755)
	os.Chmod(loose, 0o755)
	os.Mkdir(target, 0o700)
	os.Symlink(target, filepath.Join(tmp, "hbh-link"))
	fresh := filepath.Join(tmp, "hbh-new")
	if !realClaim(fresh) || !realClaim(fresh) || !realClaim(ok) || realClaim(loose) || realClaim(filepath.Join(tmp, "hbh-link")) {
		t.Fatal("claim verdicts")
	}
	if info, _ := os.Stat(fresh); info.Mode().Perm() != 0o700 {
		t.Fatalf("fresh mode %v", info.Mode())
	}
}

func TestSocketDirBudgetIsSunPathMinusA64CharSessionSocket(t *testing.T) {
	for platform, want := range map[string]int{"darwin": 103 - 73, "linux": 107 - 73, "win32": -1} {
		got, ok := socketDirBudget(platform)
		if (want < 0 && ok) || (want >= 0 && (!ok || got != want)) {
			t.Errorf("%s got %d %v", platform, got, ok)
		}
	}
}

func TestDefaultMacOSHomeDoesNotFitAndTheShortTempDirDoes(t *testing.T) {
	budget, _ := socketDirBudget("darwin")
	if fitsSocketBudget("/Users/alexander/.config/browser-harness/runtime/p-0123456789ab", budget, true) {
		t.Fatal("home fits")
	}
	short := "/private/tmp/hbh-0123456789ab"
	if !fitsSocketBudget(short, budget, true) || sockLen(short) > 103 {
		t.Fatal("short does not fit")
	}
	if !fitsSocketBudget(strings.Repeat("x", 500), 0, false) {
		t.Fatal("no budget refused")
	}
}

func TestOnMacOSAServedProfileBindsA64CharSessionSocket(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macos sun_path rule")
	}
	realClaimPrivateDir(t)
	env := map[string]string{"HOME": filepath.Join(t.TempDir(), strings.Repeat("u", 40))}
	namespaceHarnessDirs(env, "profile-a")
	path := resolvePath(env["BH_RUNTIME_DIR"]) + "/bu-" + longestName + ".sock"
	if len(path) > 103 || !kernelAccepts(t, path) {
		t.Fatal(path)
	}
}

func TestOnLinuxAServedProfiles64CharSessionSocketFitsSunPath(t *testing.T) {
	requireLinux(t)
	for _, userHome := range []string{"/home/teknium", "/home/alexander.smith"} {
		claimed := recordClaims(t, true)
		env := map[string]string{"HOME": userHome}
		namespaceHarnessDirs(env, "profile-a")
		rt := env["BH_RUNTIME_DIR"]
		if len(*claimed) != 1 || (*claimed)[0] != rt || sockLen(rt) > 107 || !kernelAccepts(t, rt+"/bu-"+longestName+".sock") {
			t.Fatalf("%s rt %q claimed %v", userHome, rt, *claimed)
		}
		if !strings.HasPrefix(filepath.Base(rt), "hbh-") {
			t.Fatal(rt)
		}
	}
}

func TestOnLinuxARuntimeDirThatFitsIsKeptAndAnUnclaimableOneStaysLong(t *testing.T) {
	requireLinux(t)
	claimed := recordClaims(t, true)
	env := map[string]string{"BH_HOME": "/b"}
	namespaceHarnessDirs(env, "profile-a")
	if !strings.HasPrefix(env["BH_RUNTIME_DIR"], "/b/runtime/p-") || len(*claimed) != 0 {
		t.Fatalf("env %v", env)
	}
	pin(t, &claimPrivateDir, func(string) bool { return false })
	env = map[string]string{"HOME": "/home/teknium"}
	namespaceHarnessDirs(env, "profile-a")
	if !strings.HasPrefix(env["BH_RUNTIME_DIR"], "/home/teknium/.config/browser-harness/runtime/p-") {
		t.Fatalf("env %v", env)
	}
}

func TestOnLinuxAShortSpellingOfALongRuntimeDirIsMeasuredResolved(t *testing.T) {
	requireLinux(t)
	for _, via := range []string{"relative", "symlink"} {
		tmp := t.TempDir()
		deep := filepath.Join(tmp, strings.Repeat("d", 60))
		os.Mkdir(deep, 0o755)
		base := "rt"
		if via == "relative" {
			t.Chdir(deep)
		} else {
			os.Symlink(deep, filepath.Join(tmp, "l"))
			t.Chdir(tmp)
			base = "l"
		}
		claimed := recordClaims(t, true)
		env := map[string]string{"BH_RUNTIME_DIR": base}
		namespaceHarnessDirs(env, "profile-a")
		resolved := resolvePath(env["BH_RUNTIME_DIR"])
		if sockLen(resolved) > 107 || !kernelAccepts(t, resolved+"/bu-"+longestName+".sock") || len(*claimed) != 1 || (*claimed)[0] != env["BH_RUNTIME_DIR"] {
			t.Fatalf("%s resolved %q claimed %v", via, resolved, *claimed)
		}
	}
}

func TestShortSocketDirIsClaimedOnlyWhenPrivateAndOurs(t *testing.T) {
	tmp := t.TempDir()
	fresh := filepath.Join(tmp, "hbh-fresh")
	if !realClaim(fresh) {
		t.Fatal("fresh")
	}
	if info, _ := os.Stat(fresh); info.Mode().Perm() != 0o700 || !realClaim(fresh) {
		t.Fatal("reuse")
	}
	openDir := filepath.Join(tmp, "hbh-open")
	os.Mkdir(openDir, 0o755)
	os.Chmod(openDir, 0o755)
	link := filepath.Join(tmp, "hbh-link")
	os.Symlink(fresh, link)
	if realClaim(openDir) || realClaim(link) {
		t.Fatal("open or link claimed")
	}
}

func TestEachServedProfileGetsItsOwnHarnessTmpDir(t *testing.T) {
	tmp := t.TempDir()
	bh := filepath.Join(tmp, "bh")
	seen := map[string]string{}
	for _, tag := range []string{filepath.Join(tmp, "a"), filepath.Join(tmp, "b")} {
		env := map[string]string{"BH_HOME": bh}
		namespaceHarnessDirs(env, tag)
		if env["BH_TMP_DIR_SHARED"] != "1" || !strings.HasPrefix(env["BH_TMP_DIR"], filepath.Join(bh, "tmp")) {
			t.Fatalf("env %v", env)
		}
		seen[tag] = env["BH_TMP_DIR"]
	}
	if seen[filepath.Join(tmp, "a")] == seen[filepath.Join(tmp, "b")] {
		t.Fatal("shared tmp dir")
	}
	unscoped := map[string]string{"BH_HOME": bh}
	namespaceHarnessDirs(unscoped, "")
	if len(unscoped) != 1 || unscoped["BH_HOME"] != bh {
		t.Fatalf("unscoped changed %v", unscoped)
	}
}

func TestServedProfileUnderAnInheritedTmpDirNestsInsideIt(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "shared-tmp")
	env := map[string]string{"BH_TMP_DIR": shared}
	namespaceHarnessDirs(env, "profile-a")
	if !strings.HasPrefix(env["BH_TMP_DIR"], shared+"/p-") || !strings.HasPrefix(env["BH_RUNTIME_DIR"], shared+"/p-") {
		t.Fatalf("env %v", env)
	}
}

func TestUnscopedReloadReachesTheDaemonWithItsOwnTmpDir(t *testing.T) {
	env := map[string]string{"HOME": "/home/u"}
	namespaceHarnessDirs(env, "profile-a")
	withName := map[string]string{"BU_NAME": "s1"}
	for k, v := range env {
		withName[k] = v
	}
	key := daemonIdentity(withName)
	target := daemonEnv(map[string]string{"HOME": "/home/u", "BH_TMP_DIR": "/shared", "PATH": "/bin"}, key)
	if target["BH_TMP_DIR"] != env["BH_TMP_DIR"] || target["BH_TMP_DIR_SHARED"] != "1" || target["BU_NAME"] != "s1" || target["PATH"] != "/bin" {
		t.Fatalf("target %v", target)
	}
	bare := daemonEnv(map[string]string{"BH_TMP_DIR": "/shared"}, daemonIdentity(map[string]string{}))
	if _, ok := bare["BH_TMP_DIR"]; ok || bare["BU_NAME"] != "default" {
		t.Fatalf("bare %v", bare)
	}
}

func TestHarnessPidFile(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"BH_HOME": "/h"}, "/h/runtime/bu-default.pid"},
		{map[string]string{"BH_HOME": "/h", "BU_NAME": "s1"}, "/h/runtime/bu-s1.pid"},
		{map[string]string{"BH_RUNTIME_DIR": "/rt", "BU_NAME": "s1"}, "/rt/bu.pid"},
		{map[string]string{"BH_TMP_DIR": "/tmpd"}, "/tmpd/bu.pid"},
		{map[string]string{"BH_RUNTIME_DIR": "/rt", "BH_RUNTIME_DIR_SHARED": "1", "BU_NAME": "s1"}, "/rt/bu-s1.pid"},
		{map[string]string{"XDG_CONFIG_HOME": "/x"}, "/x/browser-harness/runtime/bu-default.pid"},
		{map[string]string{"HOME": "/home/u"}, "/home/u/.config/browser-harness/runtime/bu-default.pid"},
	}
	for _, c := range cases {
		if got := harnessPidFile(c.env); got != c.want {
			t.Errorf("%v got %q", c.env, got)
		}
	}
}
