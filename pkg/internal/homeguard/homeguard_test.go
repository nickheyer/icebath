package homeguard

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// Environment variable a re-executed child of this binary reads to print its private home
const childEnv = "HOMEGUARD_TEST_CHILD"

func TestMain(m *testing.M) {
	if os.Getenv(childEnv) == "1" {
		os.Stdout.WriteString(Home() + "\n" + os.Getenv("HOME") + "\n" + SharedCache() + "\n")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Saves every variable isolate rewrites so the test can run it again over a fake developer environment
func saveIsolationEnv(t *testing.T) {
	names := append([]string{"HOME", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "HOMEDRIVE", "HOMEPATH", "XDG_RUNTIME_DIR",
		"DBUS_SESSION_BUS_ADDRESS", "ICEBATH_HOME", "HERMES_HOME", "ICEBATH_REAL_HOME", "ICEBATH_ELSEWHERE", "ICEBATH_TMP_HOME", sharedCacheEnv},
		append(xdgHomes, xdgSearchPaths...)...)
	for _, dir := range toolchainDirs {
		names = append(names, dir.name)
	}
	for _, name := range names {
		value, ok := os.LookupEnv(name)
		t.Setenv(name, value)
		if !ok {
			os.Unsetenv(name)
		}
	}
	saved := guard
	t.Cleanup(func() { guard = saved })
}

func TestTestBinaryRunsUnderAPrivateHomeItNeverCreates(t *testing.T) {
	if !guard.active {
		t.Fatal("the guard is not active inside a go test binary")
	}
	if !underTemp(resolve(Home())) {
		t.Fatalf("private home %s is not under a temp dir", Home())
	}
	if got, err := os.UserHomeDir(); err != nil || got != Home() {
		t.Fatalf("os.UserHomeDir = %q %v, private home %q", got, err, Home())
	}
	if underTemp(resolve(Original())) {
		if Original() != Home() {
			t.Fatalf("a temp developer home %s was replaced by %s", Original(), Home())
		}
		return
	}
	want := privatePrefix + pidNamespace() + "-" + strconv.Itoa(os.Getpid())
	if filepath.Base(Home()) != want {
		t.Fatalf("private home %s is not named %s", Home(), want)
	}
	if _, err := os.Lstat(Home()); !os.IsNotExist(err) {
		t.Fatalf("private home %s exists before any test wrote to it: %v", Home(), err)
	}
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" || os.Getenv("XDG_RUNTIME_DIR") != filepath.Join(Home(), ".run") {
		t.Fatalf("session bus %q runtime dir %q", os.Getenv("DBUS_SESSION_BUS_ADDRESS"), os.Getenv("XDG_RUNTIME_DIR"))
	}
}

func TestAccountLookupsAnswerThePrivateHomeForThisProcessOnly(t *testing.T) {
	me, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	if me.HomeDir != Home() {
		t.Fatalf("Current home %q, private home %q", me.HomeDir, Home())
	}
	byID, err := LookupID(strconv.Itoa(os.Getuid()))
	if err != nil || byID.HomeDir != Home() {
		t.Fatalf("LookupID home %v %v", byID, err)
	}
	byName, err := Lookup(me.Username)
	if err != nil || byName.HomeDir != Home() {
		t.Fatalf("Lookup home %v %v", byName, err)
	}
	if AccountDir(os.Getuid(), "/passwd/home") != Home() || AccountDir(os.Getuid()+1, "/passwd/other") != "/passwd/other" {
		t.Fatal("AccountDir swapped the wrong account")
	}
	if runtime.GOOS != "windows" && os.Getuid() != 0 {
		root, err := user.LookupId("0")
		if err != nil {
			t.Fatal(err)
		}
		if swapped, _ := LookupID("0"); swapped.HomeDir != root.HomeDir {
			t.Fatalf("root home became %q", swapped.HomeDir)
		}
	}
}

func TestChildTestBinaryKeepsTheHomeItsParentGaveIt(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 3 || lines[0] != Home() || lines[1] != Home() {
		t.Fatalf("child reported %q, parent private home %q", lines, Home())
	}
	if SharedCache() == "" || lines[2] != SharedCache() || within(resolve(SharedCache()), resolve(Home())) {
		t.Fatalf("child shared cache %q, parent shared cache %q, private home %q", lines[2], SharedCache(), Home())
	}
}

func TestIsolateReplacesADeveloperHomeAndEveryPointerIntoIt(t *testing.T) {
	saveIsolationEnv(t)
	dev := filepath.Join(string(filepath.Separator), "homeguard-dev-home")
	os.Setenv("HOME", dev)
	os.Setenv("USERPROFILE", dev)
	os.Setenv("ICEBATH_HOME", filepath.Join(dev, ".icebath"))
	os.Setenv("ICEBATH_REAL_HOME", dev)
	elsewhere := filepath.Join(string(filepath.Separator), "srv", "icebath")
	os.Setenv("ICEBATH_ELSEWHERE", elsewhere)
	os.Setenv("ICEBATH_TMP_HOME", filepath.Join(dev, "code", "work"))
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(dev, ".config"))
	os.Setenv("XDG_CONFIG_DIRS", filepath.Join(dev, ".config", "kdedefaults")+string(os.PathListSeparator)+"/etc/xdg")
	os.Setenv("XDG_DATA_DIRS", filepath.Join(dev, ".local", "share"))
	os.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus")
	for _, name := range append(xdgHomes[1:], "GOCACHE", "GOPATH", "GOENV", "LOCALAPPDATA", "APPDATA") {
		os.Unsetenv(name)
	}
	os.Setenv("XDG_CACHE_HOME", filepath.Join(dev, ".cache"))
	os.Unsetenv(sharedCacheEnv)
	developerCache, _ := os.UserCacheDir()
	isolate()
	private := Home()
	if SharedCache() != developerCache || os.Getenv(sharedCacheEnv) != developerCache ||
		(runtime.GOOS == "linux" && developerCache != filepath.Join(dev, ".cache")) {
		t.Fatalf("shared cache %q handed down as %q, developer cache %q", SharedCache(), os.Getenv(sharedCacheEnv), developerCache)
	}
	if Original() != dev || !underTemp(resolve(private)) || os.Getenv("HOME") != private {
		t.Fatalf("original %q private %q HOME %q", Original(), private, os.Getenv("HOME"))
	}
	for name, want := range map[string]string{"ICEBATH_HOME": "", "ICEBATH_REAL_HOME": "", "ICEBATH_ELSEWHERE": elsewhere,
		"ICEBATH_TMP_HOME": filepath.Join(dev, "code", "work"), "XDG_CONFIG_HOME": "", "XDG_CACHE_HOME": "", "XDG_CONFIG_DIRS": "/etc/xdg",
		"XDG_DATA_DIRS": "", "DBUS_SESSION_BUS_ADDRESS": "", "XDG_RUNTIME_DIR": filepath.Join(private, ".run"),
		"GOPATH": filepath.Join(dev, "go")} {
		if got := os.Getenv(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if runtime.GOOS == "linux" && (os.Getenv("GOENV") != filepath.Join(dev, ".config", "go", "env") || os.Getenv("GOCACHE") != filepath.Join(dev, ".cache", "go-build")) {
		t.Errorf("GOENV = %q GOCACHE = %q", os.Getenv("GOENV"), os.Getenv("GOCACHE"))
	}
}

func TestIsolateKeepsAHomeATestAlreadyPutUnderTheTempDir(t *testing.T) {
	saveIsolationEnv(t)
	dir := t.TempDir()
	os.Setenv("HOME", dir)
	os.Setenv("USERPROFILE", dir)
	os.Setenv("ICEBATH_HOME", filepath.Join(dir, ".icebath"))
	isolate()
	if Home() != dir || Original() != dir || os.Getenv("ICEBATH_HOME") != filepath.Join(dir, ".icebath") {
		t.Fatalf("home %q original %q ICEBATH_HOME %q", Home(), Original(), os.Getenv("ICEBATH_HOME"))
	}
}

func TestReapRemovesOnlyThePrivateHomesOfExitedBinariesInThisNamespace(t *testing.T) {
	tmp := t.TempDir()
	exited := exec.Command(os.Args[0], "-test.run=^$")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	ns := pidNamespace()
	names := map[string]bool{
		privatePrefix + ns + "-" + strconv.Itoa(exited.Process.Pid):  false,
		privatePrefix + ns + "-" + strconv.Itoa(os.Getpid()):         true,
		privatePrefix + ns + "-" + strconv.Itoa(os.Getppid()):        true,
		privatePrefix + ns + "1-" + strconv.Itoa(exited.Process.Pid): true,
		privatePrefix + ns + "-notapid":                              true,
		"unrelated-" + strconv.Itoa(exited.Process.Pid):              true,
	}
	for name := range names {
		if err := os.MkdirAll(filepath.Join(tmp, name, ".icebath"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	reapStale(tmp)
	for name, kept := range names {
		_, err := os.Stat(filepath.Join(tmp, name))
		if kept != (err == nil) {
			t.Errorf("%s kept=%v stat err %v", name, kept, err)
		}
	}
}

func TestEveryTestBinaryOfTheModuleLinksTheGuard(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s: %v", root, err)
	}
	list := exec.Command("go", "list", "-tags=goolm", "-test", "-f", `{{.ImportPath}}|{{join .Deps ","}}`, "./...")
	list.Dir = root
	list.Stderr = os.Stderr
	out, err := list.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	const guard = "github.com/nickheyer/icebath/pkg/internal/homeguard"
	binaries := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, deps, _ := strings.Cut(line, "|")
		binary, isTest := strings.CutSuffix(name, ".test")
		if !isTest {
			continue
		}
		binaries++
		linked := binary == guard
		for _, dep := range strings.Split(deps, ",") {
			if dep, _, _ = strings.Cut(dep, " "); dep == guard {
				linked = true
			}
		}
		if !linked {
			t.Errorf("the tests of %s run without the private home, add a homeguard_import_test.go", binary)
		}
	}
	if binaries < 100 {
		t.Fatalf("go list named %d test binaries", binaries)
	}
}
