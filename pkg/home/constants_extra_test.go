package home

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Clean process for home resolution with the user home at tmp, the home variable unset and the warning latch reset
func hcFresh(t *testing.T) (string, *bytes.Buffer) {
	t.Helper()
	tmp := t.TempDir()
	t.Cleanup(PatchUserHome(tmp))
	t.Setenv(EnvHome, "")
	os.Unsetenv(EnvHome)
	prevOut, prevWarned := fallbackWarnOut, profileFallbackWarned.Load()
	buf := &bytes.Buffer{}
	fallbackWarnOut = buf
	profileFallbackWarned.Store(false)
	ResetKeyCache()
	t.Cleanup(func() {
		fallbackWarnOut = prevOut
		profileFallbackWarned.Store(prevWarned)
		ResetKeyCache()
	})
	return tmp, buf
}

func hcPosixOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX home layout")
	}
}

func hcWindowsOnly(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows home layout")
	}
}

func TestNoHermesHomeReturnsNative(t *testing.T) {
	hcPosixOnly(t)
	tmp, _ := hcFresh(t)
	if got := Root(); got != filepath.Join(tmp, ".icebath") {
		t.Fatalf("Root() = %q", got)
	}
}

func TestDockerProfileActive(t *testing.T) {
	tmp, _ := hcFresh(t)
	dockerRoot := filepath.Join(tmp, "opt", "data")
	profile := filepath.Join(dockerRoot, "profiles", "coder")
	os.MkdirAll(profile, 0o755)
	t.Setenv(EnvHome, profile)
	if got := Root(); got != dockerRoot {
		t.Fatalf("Root() = %q, want %q", got, dockerRoot)
	}
}

func TestExpandedCustomProfileReturnsCustomRoot(t *testing.T) {
	tmp, _ := hcFresh(t)
	t.Setenv("HOME", tmp)
	t.Cleanup(PatchUserHome(filepath.Join(tmp, "native-home")))
	t.Setenv(EnvHome, "$"+"HOME/deployment/profiles/research")
	if got := Root(); got != filepath.Join(tmp, "deployment") {
		t.Fatalf("Root() = %q", got)
	}
}

func TestNoHermesHomeReturnsLocalappdataRootOnWindows(t *testing.T) {
	hcWindowsOnly(t)
	tmp, _ := hcFresh(t)
	t.Cleanup(PatchUserHome(filepath.Join(tmp, "Home")))
	local := filepath.Join(tmp, "LocalAppData")
	t.Setenv("LOCALAPPDATA", local)
	if got := Root(); got != filepath.Join(local, "icebath") {
		t.Fatalf("Root() = %q", got)
	}
}

func TestWindowsFallbackUsesLocalappdata(t *testing.T) {
	hcWindowsOnly(t)
	tmp, _ := hcFresh(t)
	t.Cleanup(PatchUserHome(filepath.Join(tmp, "Home")))
	local := filepath.Join(tmp, "LocalAppData")
	t.Setenv("LOCALAPPDATA", local)
	if got := Dir(context.Background()); got != filepath.Join(local, "icebath") {
		t.Fatalf("Dir() = %q", got)
	}
}

func TestRootForAnotherEnvironmentIgnoresThisProcess(t *testing.T) {
	tmp, _ := hcFresh(t)
	t.Setenv("LOCALAPPDATA", filepath.Join(tmp, "sandbox-local"))
	t.Setenv(EnvHome, filepath.Join(tmp, "sandbox-home"))
	local, usr, custom := filepath.Join(tmp, "local"), filepath.Join(tmp, "user"), filepath.Join(tmp, "data")
	type tc struct {
		env  map[string]string
		want string
	}
	var cases []tc
	if runtime.GOOS == "windows" {
		cases = []tc{
			{map[string]string{"LOCALAPPDATA": local}, filepath.Join(local, "icebath")},
			{map[string]string{"LOCALAPPDATA": local, EnvHome: filepath.Join(local, "icebath", "profiles", "work")}, filepath.Join(local, "icebath")},
			{map[string]string{"LOCALAPPDATA": local, EnvHome: filepath.Join(custom, "profiles", "work")}, custom},
			{map[string]string{"LOCALAPPDATA": local, EnvSuffix: "-magic"}, filepath.Join(local, "icebath-magic")},
			{map[string]string{"localappdata": local, EnvHome: `%LocalAppData%\data`}, filepath.Join(local, "data")},
			{map[string]string{"LOCALAPPDATA": local, EnvHome: `$LOCALAPPDATA\data`}, filepath.Join(local, "data")},
			{map[string]string{"LOCALAPPDATA": local, EnvHome: `${LOCALAPPDATA}\data`}, filepath.Join(local, "data")},
			{map[string]string{"LOCALAPPDATA": local, "USERPROFILE": usr, EnvHome: `~\data`}, filepath.Join(usr, "data")},
			{map[string]string{"USERPROFILE": usr}, filepath.Join(usr, "AppData", "Local", "icebath")},
			{map[string]string{"LOCALAPPDATA": local, EnvHome: `relative\data`}, ""},
			{map[string]string{"LOCALAPPDATA": local, EnvHome: `%UNDEFINED%\data`}, ""},
			{map[string]string{"LOCALAPPDATA": local, EnvHome: `~\data`}, ""},
			{map[string]string{}, ""},
		}
	} else {
		cases = []tc{
			{map[string]string{"HOME": usr}, filepath.Join(usr, ".icebath")},
			{map[string]string{"HOME": usr, EnvHome: filepath.Join(usr, ".icebath", "profiles", "work")}, filepath.Join(usr, ".icebath")},
			{map[string]string{"HOME": usr, EnvHome: filepath.Join(custom, "profiles", "work")}, custom},
			{map[string]string{"HOME": usr, EnvSuffix: "-magic"}, filepath.Join(usr, ".icebath-magic")},
			{map[string]string{"home": usr, EnvHome: "${Home}/data"}, filepath.Join(usr, "data")},
			{map[string]string{"HOME": usr, EnvHome: "$HOME/data"}, filepath.Join(usr, "data")},
			{map[string]string{"HOME": usr, EnvHome: "%HOME%/data"}, filepath.Join(usr, "data")},
			{map[string]string{"HOME": usr, EnvHome: "~/data"}, filepath.Join(usr, "data")},
			{map[string]string{"HOME": usr, EnvHome: "relative/data"}, ""},
			{map[string]string{"HOME": usr, EnvHome: "$UNDEFINED/data"}, ""},
			{map[string]string{EnvHome: custom}, ""},
			{map[string]string{}, ""},
		}
	}
	for _, c := range cases {
		if got := RootForEnvironment(c.env); got != c.want {
			t.Errorf("RootForEnvironment(%v) = %q, want %q", c.env, got, c.want)
		}
	}
}

func TestWarnOnceLatchEngagesOnFirstCheckEvenWithoutWarning(t *testing.T) {
	hcFresh(t)
	Dir(context.Background())
	if !profileFallbackWarned.Load() {
		t.Fatal("latch not set by the first home lookup")
	}
}

func TestEnvSetReturnsThatPath(t *testing.T) {
	tmp, _ := hcFresh(t)
	h := filepath.Join(tmp, "launch-home")
	t.Setenv(EnvHome, h)
	if got := Process(); got != h {
		t.Fatalf("Process() = %q", got)
	}
}

func TestProcessAndContextHomesExpandEnvironmentAndUserSyntax(t *testing.T) {
	hcPosixOnly(t)
	tmp, _ := hcFresh(t)
	t.Setenv("HOME", tmp)
	for _, syntax := range []string{"$" + "HOME", "~"} {
		processHome := filepath.Join(tmp, "process-home")
		t.Setenv(EnvHome, syntax+"/process-home")
		if got := Process(); got != processHome {
			t.Fatalf("%s Process() = %q", syntax, got)
		}
		ctx := WithOverride(context.Background(), syntax+"/override-home")
		if got := Dir(ctx); got != filepath.Join(tmp, "override-home") {
			t.Fatalf("%s Dir(override) = %q", syntax, got)
		}
		if got := Process(); got != processHome {
			t.Fatalf("%s Process() under override = %q", syntax, got)
		}
	}
}

// Captured warning lands in the writer once the latch is rearmed and the restore puts both back
func TestCaptureProfileFallbackWarningRearmsTheLatch(t *testing.T) {
	hcPosixOnly(t)
	tmp, outer := hcFresh(t)
	h := filepath.Join(tmp, ".icebath")
	os.MkdirAll(h, 0o755)
	os.WriteFile(filepath.Join(h, "active_profile"), []byte("coder\n"), 0o644)
	Dir(context.Background())
	outer.Reset()
	inner := &bytes.Buffer{}
	restore := CaptureProfileFallbackWarning(inner)
	Dir(context.Background())
	restore()
	if !strings.Contains(inner.String(), "ICEBATH_HOME fallback") || outer.Len() != 0 {
		t.Fatalf("inner %q outer %q", inner.String(), outer.String())
	}
	if fallbackWarnOut != io.Writer(outer) || !profileFallbackWarned.Load() {
		t.Fatal("restore left the capture in place")
	}
}

func TestClassicModeNoActiveProfileNoWarning(t *testing.T) {
	hcPosixOnly(t)
	tmp, buf := hcFresh(t)
	if got := Dir(context.Background()); got != filepath.Join(tmp, ".icebath") {
		t.Fatalf("Dir() = %q", got)
	}
	if strings.Contains(buf.String(), "ICEBATH_HOME fallback") {
		t.Fatal(buf.String())
	}
}

func TestNamedProfileUnsetHomeWarnsOnce(t *testing.T) {
	hcPosixOnly(t)
	tmp, buf := hcFresh(t)
	h := filepath.Join(tmp, ".icebath")
	os.MkdirAll(h, 0o755)
	os.WriteFile(filepath.Join(h, "active_profile"), []byte("coder\n"), 0o644)
	if got := Dir(context.Background()); got != h {
		t.Fatalf("Dir() = %q", got)
	}
	if err := buf.String(); strings.Count(err, "ICEBATH_HOME fallback") != 1 || !strings.Contains(err, "'coder'") {
		t.Fatalf("stderr = %q", err)
	}
	buf.Reset()
	Dir(context.Background())
	Dir(context.Background())
	if strings.Contains(buf.String(), "ICEBATH_HOME fallback") {
		t.Fatal("warned twice")
	}
}

func TestHermesHomeSetSuppressesWarning(t *testing.T) {
	tmp, buf := hcFresh(t)
	profile := filepath.Join(tmp, ".icebath", "profiles", "coder")
	os.MkdirAll(profile, 0o755)
	os.WriteFile(filepath.Join(tmp, ".icebath", "active_profile"), []byte("coder\n"), 0o644)
	t.Setenv(EnvHome, profile)
	if got := Dir(context.Background()); got != profile {
		t.Fatalf("Dir() = %q", got)
	}
	if strings.Contains(buf.String(), "ICEBATH_HOME fallback") {
		t.Fatal(buf.String())
	}
}

func TestUnreadableActiveProfileNoCrash(t *testing.T) {
	hcPosixOnly(t)
	tmp, buf := hcFresh(t)
	h := filepath.Join(tmp, ".icebath")
	os.MkdirAll(h, 0o755)
	os.WriteFile(filepath.Join(h, "active_profile"), []byte{0xff, 0xfe, 0, 0}, 0o644)
	if got := Dir(context.Background()); got != h {
		t.Fatalf("Dir() = %q", got)
	}
	if strings.Contains(buf.String(), "ICEBATH_HOME fallback") {
		t.Fatal(buf.String())
	}
}

func TestScratchExportUsesTheProcessHomeSilently(t *testing.T) {
	hcPosixOnly(t)
	tmp, buf := hcFresh(t)
	h := filepath.Join(tmp, ".icebath")
	os.MkdirAll(filepath.Join(h, "profiles", "coder"), 0o755)
	os.WriteFile(filepath.Join(h, "active_profile"), []byte("coder\n"), 0o644)
	env := map[string]string{"PATH": "/usr/bin:/bin"}
	if !ApplyScratchTmpEnv(context.Background(), env) || env["TMPDIR"] != filepath.Join(h, "cache", "scratch") {
		t.Fatalf("env = %v", env)
	}
	if strings.Contains(buf.String(), "ICEBATH_HOME fallback") {
		t.Fatal(buf.String())
	}
}

func TestExplicitHomeScratchDirNeverReadsTheEffectiveHome(t *testing.T) {
	hcPosixOnly(t)
	tmp, buf := hcFresh(t)
	scratchPruneSpawns(t)
	h := filepath.Join(tmp, ".icebath")
	profile := filepath.Join(h, "profiles", "coder")
	os.MkdirAll(profile, 0o755)
	os.WriteFile(filepath.Join(h, "active_profile"), []byte("coder\n"), 0o644)
	scratch := EnsureScratchDir(context.Background(), profile)
	if scratch != filepath.Join(profile, "cache", "scratch") {
		t.Fatalf("scratch = %q", scratch)
	}
	if st, err := os.Stat(scratch); err != nil || !st.IsDir() {
		t.Fatal("scratch dir missing")
	}
	if strings.Contains(buf.String(), "ICEBATH_HOME fallback") {
		t.Fatal(buf.String())
	}
}

// Key computed without the cache the way the Python tests compute it
func hcUncached(path string) string {
	expanded, err := pypath.PathExpanduser(path)
	if err != nil {
		panic(err)
	}
	resolved, err := resolve(expanded)
	if err != nil {
		panic(err)
	}
	return normcase(resolved)
}

func TestMatchesTheUncachedCalculation(t *testing.T) {
	tmp, _ := hcFresh(t)
	os.Mkdir(filepath.Join(tmp, "real"), 0o755)
	sep := string(os.PathSeparator)
	for _, c := range []struct{ name, target string }{
		{"real", filepath.Join(tmp, "real")},
		{"missing", filepath.Join(tmp, "not_there")},
		{"trailing_sep", filepath.Join(tmp, "real") + sep},
		{"dot_dot", filepath.Join(tmp, "real") + sep + ".." + sep + "real"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ResetKeyCache()
			if got, want := Key(c.target), hcUncached(c.target); got != want {
				t.Fatalf("Key(%q) = %q, want %q", c.target, got, want)
			}
		})
	}
}

func TestMatchesForTheDefaultHome(t *testing.T) {
	hcFresh(t)
	h := Dir(context.Background())
	if got, want := Key(h), hcUncached(h); got != want {
		t.Fatalf("default home key %q, want %q", got, want)
	}
}

func TestMatchesForATildePath(t *testing.T) {
	hcFresh(t)
	if got, want := Key("~"), hcUncached("~"); got != want {
		t.Fatalf("Key(~) = %q, want %q", got, want)
	}
}

func TestAcceptsAPathObject(t *testing.T) {
	tmp, _ := hcFresh(t)
	real := filepath.Join(tmp, "real")
	os.Mkdir(real, 0o755)
	if got, want := Key(real), hcUncached(real); got != want {
		t.Fatalf("Key(%q) = %q, want %q", real, got, want)
	}
}

func TestAMissingPathIsNotRemembered(t *testing.T) {
	tmp, _ := hcFresh(t)
	Key(filepath.Join(tmp, "not_there"))
	if len(keyCache) != 0 {
		t.Fatalf("missing path remembered: %v", keyCache)
	}
}

func TestAPathCreatedLaterPicksUpTheRealAnswer(t *testing.T) {
	tmp, _ := hcFresh(t)
	later := filepath.Join(tmp, "later")
	before := Key(later)
	os.Mkdir(later, 0o755)
	after := Key(later)
	if after != hcUncached(later) {
		t.Fatalf("after %q, want %q", after, hcUncached(later))
	}
	if len(keyCache) != 1 || keyCache[later] != after {
		t.Fatalf("cache = %v", keyCache)
	}
	if before != after {
		t.Fatalf("before %q after %q", before, after)
	}
}

func TestPointingHermesHomeSomewhereElseGivesANewKey(t *testing.T) {
	tmp, _ := hcFresh(t)
	first, second := filepath.Join(tmp, "home_one"), filepath.Join(tmp, "home_two")
	os.Mkdir(first, 0o755)
	os.Mkdir(second, 0o755)
	t.Setenv(EnvHome, first)
	one := Key(Dir(context.Background()))
	t.Setenv(EnvHome, second)
	two := Key(Dir(context.Background()))
	if one == two || one != hcUncached(first) || two != hcUncached(second) {
		t.Fatalf("keys %q %q", one, two)
	}
}

func TestALinkResolvesToItsTarget(t *testing.T) {
	tmp, _ := hcFresh(t)
	target, link := filepath.Join(tmp, "target"), filepath.Join(tmp, "link")
	os.Mkdir(target, 0o755)
	if err := os.Symlink(target, link); err != nil {
		t.Skip("this platform or account cannot create symlinks")
	}
	if Key(link) != hcUncached(link) || Key(link) != Key(target) {
		t.Fatalf("link key %q target key %q", Key(link), Key(target))
	}
}

func TestScopeKeyResolvesThePathOnce(t *testing.T) {
	tmp, _ := hcFresh(t)
	t.Setenv(EnvHome, tmp)
	calls := 0
	prev := strictResolve
	strictResolve = func(p string) (string, error) { calls++; return prev(p) }
	t.Cleanup(func() { strictResolve = prev })
	Key(Dir(context.Background()))
	first := calls
	for range 50 {
		Key(Dir(context.Background()))
	}
	if calls != first {
		t.Fatalf("current_scope_key resolved the path on the filesystem %d extra times across 50 calls", calls-first)
	}
}

func TestSudoInvokerDefaultHome(t *testing.T) {
	hcPosixOnly(t)
	tmp, _ := hcFresh(t)
	prevUID, prevLookup := geteuid, lookupUser
	t.Cleanup(func() { geteuid, lookupUser = prevUID, prevLookup })
	lookupUser = func(name string) (*user.User, error) {
		if name == "alice" {
			return &user.User{Username: name, HomeDir: tmp}, nil
		}
		return nil, user.UnknownUserError(name)
	}
	geteuid = func() int { return 1000 }
	t.Setenv("SUDO_USER", "alice")
	if got := SudoInvokerDefaultHome(); got != "" {
		t.Fatalf("non root got %q", got)
	}
	geteuid = func() int { return 0 }
	if got := SudoInvokerDefaultHome(); got != filepath.Join(tmp, ".icebath") {
		t.Fatalf("got %q", got)
	}
	for _, u := range []string{"", " ", "root", "ghost"} {
		t.Setenv("SUDO_USER", u)
		if got := SudoInvokerDefaultHome(); got != "" {
			t.Fatalf("SUDO_USER %q got %q", u, got)
		}
	}
}

func TestPinnedProcessHomeRoutesDecisions(t *testing.T) {
	tmp, _ := hcFresh(t)
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Cleanup(func() { PinProcess("", true) })
	t.Setenv(EnvHome, filepath.Join(tmp, "launch"))
	if ProcessPinned() || RoutingProcess() != filepath.Join(tmp, "launch") {
		t.Fatal("unpinned routing should follow the process home")
	}
	PinProcess("~/served", false)
	if !ProcessPinned() || RoutingProcess() != filepath.Join(tmp, "served") || Process() != filepath.Join(tmp, "launch") {
		t.Fatalf("pinned routing = %q", RoutingProcess())
	}
	PinProcess("", false)
	if !ProcessPinned() || RoutingProcess() != filepath.Join(tmp, "launch") {
		t.Fatal("an empty pin is pinned and routes to the process home")
	}
	PinProcess("", true)
	if ProcessPinned() {
		t.Fatal("unpin left the pin")
	}
}

func TestPackagedDirsHonorWrappersThenDefaultThenHome(t *testing.T) {
	tmp, _ := hcFresh(t)
	t.Setenv(EnvHome, tmp)
	ctx := context.Background()
	t.Setenv("ICEBATH_OPTIONAL_SKILLS", "")
	t.Setenv("ICEBATH_OPTIONAL_MCPS", " ")
	t.Setenv("ICEBATH_BUNDLED_SKILLS", filepath.Join(tmp, "nix", "skills")+" ")
	if got := OptionalSkillsDir(ctx, ""); got != filepath.Join(tmp, "optional-skills") {
		t.Fatalf("optional skills = %q", got)
	}
	if got := OptionalMCPsDir(ctx, filepath.Join(tmp, "checkout")); got != filepath.Join(tmp, "checkout") {
		t.Fatalf("optional mcps = %q", got)
	}
	if got := BundledSkillsDir(ctx, filepath.Join(tmp, "checkout")); got != filepath.Join(tmp, "nix", "skills") {
		t.Fatalf("bundled skills = %q", got)
	}
	over := WithOverride(ctx, filepath.Join(tmp, "profiles", "x"))
	if got := OptionalSkillsDir(over, ""); got != filepath.Join(tmp, "profiles", "x", "optional-skills") {
		t.Fatalf("override optional skills = %q", got)
	}
}

func TestNodeLayoutHelpers(t *testing.T) {
	tmp, _ := hcFresh(t)
	nodeDir := filepath.Join(tmp, "node")
	dirs := NodeDirs(context.Background(), tmp)
	names := CandidateNodeCommandNames(filepath.Join("x", "npm"))
	if runtime.GOOS == "windows" {
		if dirs[0] != nodeDir || dirs[1] != filepath.Join(nodeDir, "bin") {
			t.Fatalf("dirs = %v", dirs)
		}
		if strings.Join(names, ",") != "npm.cmd,npm.exe,npm" || strings.Join(CandidateNodeCommandNames("Tool"), ",") != "Tool.cmd,Tool.exe,Tool" {
			t.Fatalf("names = %v", names)
		}
	} else {
		if dirs[0] != filepath.Join(nodeDir, "bin") || dirs[1] != nodeDir {
			t.Fatalf("dirs = %v", dirs)
		}
		if strings.Join(names, ",") != "npm" {
			t.Fatalf("names = %v", names)
		}
	}
	if got := CandidateNodeCommandNames("node.exe"); len(got) != 1 || got[0] != "node.exe" {
		t.Fatalf("dotted names = %v", got)
	}
	if ManagedNodeTreePresent(context.Background(), tmp) {
		t.Fatal("empty home reports a node tree")
	}
	os.MkdirAll(dirs[0], 0o755)
	os.MkdirAll(dirs[1], 0o755)
	os.Mkdir(filepath.Join(dirs[0], CandidateNodeCommandNames("npx")[0]), 0o755)
	if ManagedNodeTreePresent(context.Background(), tmp) {
		t.Fatal("a directory named npx is not a node tree")
	}
	os.WriteFile(filepath.Join(dirs[1], CandidateNodeCommandNames("node")[0]), nil, 0o644)
	if !ManagedNodeTreePresent(context.Background(), tmp) {
		t.Fatal("node file not detected")
	}
}

func TestIsExecutableFile(t *testing.T) {
	hcPosixOnly(t)
	tmp, _ := hcFresh(t)
	exe, plain := filepath.Join(tmp, "exe"), filepath.Join(tmp, "plain")
	os.WriteFile(exe, nil, 0o755)
	os.WriteFile(plain, nil, 0o644)
	os.Symlink(filepath.Join(tmp, "gone"), filepath.Join(tmp, "dangling"))
	if !IsExecutableFile(exe) || IsExecutableFile(plain) || IsExecutableFile(filepath.Join(tmp, "dangling")) {
		t.Fatal("executable verdicts wrong")
	}
}

func TestPartialUpdateHintOnlyForFirstPartyImportErrors(t *testing.T) {
	for name, want := range map[string]bool{
		"agent": true, "agent.foo": true, "hermes_cli.main": true, "tui_gateway.x": true,
		"agentops": false, "": false, "requests": false, ".agent": false,
	} {
		if IsFirstPartyModule(name) != want {
			t.Errorf("IsFirstPartyModule(%q) != %v", name, want)
		}
	}
	half := &ImportError{Name: "agent.turn", Msg: "cannot import name 'x' from 'agent.turn'"}
	if lines := PartialUpdateHint(half); len(lines) != 5 || lines[3] != "    icebath update" {
		t.Fatalf("hint = %q", lines)
	}
	for _, err := range []error{
		&ImportError{Name: "agent.turn", Msg: "No module named 'agent.turn'", ModuleNotFound: true},
		&ImportError{Name: "requests", Msg: "cannot import"},
		errors.New("plain"),
		nil,
	} {
		if PartialUpdateHint(err) != nil {
			t.Errorf("hint for %v", err)
		}
	}
	var buf bytes.Buffer
	if !EmitPartialUpdateHint(half, &buf) || !strings.HasPrefix(buf.String(), "Error: cannot import name 'x' from 'agent.turn'\n\nThis looks like") {
		t.Fatalf("emitted %q", buf.String())
	}
	buf.Reset()
	if EmitPartialUpdateHint(errors.New("plain"), &buf) || buf.Len() != 0 {
		t.Fatal("emitted for a plain error")
	}
}

func TestLongScratchTmpdirFallsBackToASocketSafeRoot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("AF_UNIX path budget of Linux")
	}
	tmp, _ := hcFresh(t)
	scratch := filepath.Join(tmp, strings.Repeat("long-home-", 8), "cache", "scratch")
	os.MkdirAll(scratch, 0o755)
	for _, k := range []string{"TMPDIR", "TMP", "TEMP", ScratchDirMarkerEnv} {
		t.Setenv(k, scratch)
	}
	root, err := SocketSafeTmpdir()
	if err != nil || len(root) > SocketTmpdirMaxLen {
		t.Fatalf("root = %q, %v", root, err)
	}
	dir, err := os.MkdirTemp(root, "scoped_dir")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	l, err := net.Listen("unix", filepath.Join(dir, "SingletonSocket"))
	if err != nil {
		t.Fatalf("singleton socket did not bind under %q: %v", root, err)
	}
	l.Close()
}

func TestSuffixDoesNotChangeExplicitOrContextHome(t *testing.T) {
	tmp, _ := hcFresh(t)
	t.Setenv("LOCALAPPDATA", filepath.Join(tmp, "AppData", "Local"))
	explicit, scoped := filepath.Join(tmp, "explicit"), filepath.Join(tmp, "scoped")
	t.Setenv(EnvHome, explicit)
	t.Setenv(EnvSuffix, "magic-test")
	if Dir(context.Background()) != explicit || Process() != explicit || Root() != explicit {
		t.Fatalf("Dir %q Process %q Root %q, want %q", Dir(context.Background()), Process(), Root(), explicit)
	}
	ctx := WithOverride(context.Background(), scoped)
	if got := Dir(ctx); got != scoped {
		t.Fatalf("Dir(override) = %q", got)
	}
	if Process() != explicit || Root() != explicit {
		t.Fatalf("Process %q Root %q under the override", Process(), Root())
	}
}
