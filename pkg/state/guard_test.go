package state

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const guardHelperEnv = "ICEBATH_GUARD_ANCESTRY_HELPER"

// Child half of the scrubbed environment test, printing the ancestry verdict of a process whose env lost every test marker
func TestGuardAncestryHelperProcess(t *testing.T) {
	if os.Getenv(guardHelperEnv) != "1" {
		return
	}
	fmt.Printf("VERDICT=%v\n", hasTestRunnerAncestor())
	os.Exit(0)
}

// Pins a fake process tree and forgets the memoised ancestry answer around the test
func fakeProcessTree(t *testing.T, parents func(int) ([]int, error), cmdlines map[int][]string) {
	t.Helper()
	prevParents, prevCmdline := parentPIDs, processCmdline
	parentPIDs = parents
	processCmdline = func(pid int) ([]string, error) {
		argv, ok := cmdlines[pid]
		if !ok {
			return nil, errors.New("access denied")
		}
		return argv, nil
	}
	resetAncestry := func() {
		testRunnerAncestor.Lock()
		testRunnerAncestor.known, testRunnerAncestor.found = false, false
		testRunnerAncestor.Unlock()
	}
	resetAncestry()
	t.Cleanup(func() {
		parentPIDs, processCmdline = prevParents, prevCmdline
		resetAncestry()
	})
}

func argvOf(argv []string) func() ([]string, error) {
	return func() ([]string, error) { return argv, nil }
}

func TestRecognisesPytestInvocations(t *testing.T) {
	for _, argv := range [][]string{
		{"/usr/bin/python", "-m", "pytest", "tests/"},
		{"/venv/bin/pytest", "-q"},
		{`C:\venv\Scripts\pytest.exe`, "-q"},
		{"/usr/bin/py.test", "tests/"},
		{`"C:\venv\Scripts\PYTEST.EXE"`, "-q"},
		{"/tmp/go-build123/b001/state.test", "-test.paniconexit0", "-test.timeout=10m0s"},
		{`C:\Temp\go-build9\b001\state.test.exe`},
		{"/usr/local/go/bin/go", "test", "-race", "./pkg/state/"},
		{"go.exe", "test", "./..."},
	} {
		if !processLooksLikeTestRunner(argvOf(argv)) {
			t.Errorf("%q not recognised as a test run", argv)
		}
	}
}

func TestIgnoresNonPytestInvocations(t *testing.T) {
	for _, argv := range [][]string{
		{"icebath", "gateway", "start"},
		{"/usr/bin/python", "-m", "icebath_cli.main", "sessions", "list"},
		{"icebath", "run", "--file", "/tmp/pytest-of-dev/test0/input.txt"},
		{"icebath", "run", "--file", "fixtures/case.test"},
		{"go", "build", "./..."},
		{"go", "vet", "test"},
		{},
	} {
		if processLooksLikeTestRunner(argvOf(argv)) {
			t.Errorf("%q recognised as a test run", argv)
		}
	}
}

func TestUnreadableProcessIsNotPytest(t *testing.T) {
	denied := func() ([]string, error) { return nil, os.ErrPermission }
	if processLooksLikeTestRunner(denied) {
		t.Fatal("unreadable command line treated as a test run")
	}
}

func TestAncestryFindsATestRunnerPastUnreadableAncestorsAndMemoises(t *testing.T) {
	self := os.Getpid()
	walks := 0
	fakeProcessTree(t, func(pid int) ([]int, error) {
		walks++
		if pid != self {
			t.Errorf("walk started at %d, want %d", pid, self)
		}
		return []int{10, 20, 30}, nil
	}, map[int][]string{10: {"icebath", "chat"}, 30: {"/venv/bin/pytest", "-x"}})
	if !hasTestRunnerAncestor() || !hasTestRunnerAncestor() {
		t.Fatal("pytest ancestor behind an unreadable parent was missed")
	}
	if walks != 1 {
		t.Fatalf("ancestry walked %d times, want the memoised single walk", walks)
	}
}

func TestAncestryFailsOpenOnWalkErrorsAndPlainTrees(t *testing.T) {
	fakeProcessTree(t, func(int) ([]int, error) { return nil, errors.New("walk failed") }, nil)
	if hasTestRunnerAncestor() {
		t.Fatal("a failed walk armed the guard")
	}
	fakeProcessTree(t, func(int) ([]int, error) { return []int{7, 1}, nil }, map[int][]string{7: {"bash"}, 1: {"/sbin/init"}})
	if hasTestRunnerAncestor() {
		t.Fatal("a plain process tree armed the guard")
	}
}

func TestInTestContextUnderGoTestEvenWithoutEnvMarkers(t *testing.T) {
	t.Setenv("PYTEST_CURRENT_TEST", "")
	t.Setenv("PYTEST_VERSION", "")
	t.Setenv(testIsolationMarkerEnv, "")
	if !runningUnderTest() || !inTestContext() {
		t.Fatal("a go test binary did not report a test context")
	}
}

// The scrubbed child keeps no PYTEST_* nor ICEBATH_HOME nor isolation marker, yet its test binary parent arms the guard
func TestChildWithoutTestEnvironmentStillSeesTestRunnerAncestry(t *testing.T) {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "PYTEST_") || key == "ICEBATH_HOME" || key == testIsolationMarkerEnv {
			continue
		}
		env = append(env, kv)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestGuardAncestryHelperProcess$")
	cmd.Env = append(env, guardHelperEnv+"=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "VERDICT=true") {
		t.Fatalf("scrubbed child lost the ancestry signal: %q", out)
	}
}

func TestProductionStateDBShapes(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "home", "dev", ".icebath")
	cases := map[string]bool{
		filepath.Join(root, "state.db"):                                true,
		filepath.Join(root, "profiles", "research", "state.db"):        true,
		filepath.Join(root, "other.db"):                                true,
		filepath.Join(root, "profiles", "research", "sub", "state.db"): false,
		filepath.Join(root, "worktrees", "x", "state.db"):              false,
		filepath.Join(root, "cache", "scratch", "x", "state.db"):       false,
		filepath.Join(root+"-other", "state.db"):                       false,
		root:                                                           false,
		filepath.Join(string(filepath.Separator), "tmp", "state.db"):   false,
	}
	for path, want := range cases {
		if got := isProductionStateDB(path, root); got != want {
			t.Errorf("isProductionStateDB(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestRealPlatformStateRootFollowsTheAccountHome(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", link+"/")
	t.Setenv("USERPROFILE", link)
	t.Setenv("ICEBATH_HOME", filepath.Join(dir, "redirected"))
	root, ok := realPlatformStateRoot()
	want := filepath.Join(realpathLax(dir), ".icebath")
	if !ok || root != want {
		t.Fatalf("root = %q, %v, want %q", root, ok, want)
	}
}

func TestExpandUserMatchesPythonForEmptyAndTrailingHomes(t *testing.T) {
	cases := []struct{ home, path, want string }{
		{"/home/dev/", "~/a", "/home/dev/a"},
		{"/home/dev", "~", "/home/dev"},
		{"", "~/a", "/a"},
		{"", "~", "/"},
		{"/", "~", "/"},
		{"/home/dev", "~other/a", "~other/a"},
		{"/home/dev", "plain/~", "plain/~"},
	}
	for _, c := range cases {
		t.Setenv("HOME", c.home)
		if got := expandUser(c.path); got != c.want {
			t.Errorf("HOME=%q expandUser(%q) = %q, want %q", c.home, c.path, got, c.want)
		}
	}
}

func TestTestInstancesRegisterOnlyUnderTheIsolationMarker(t *testing.T) {
	t.Setenv(testIsolationMarkerEnv, "")
	plain, err := Open(filepath.Join(t.TempDir(), "state.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	for _, d := range liveTestInstances() {
		if d == plain {
			t.Fatal("instance registered without the isolation marker")
		}
	}
	t.Setenv(testIsolationMarkerEnv, "1")
	leaked, err := Open(filepath.Join(t.TempDir(), "state.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	shared, err := Acquire(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer CloseAll()
	CloseLeakedTestInstances()
	if !leaked.closedForTest() {
		t.Fatal("the leak sweep left a registered instance open")
	}
	if shared.closedForTest() {
		t.Fatal("the leak sweep closed a registry shared handle")
	}
	if err := leaked.Close(); err != nil {
		t.Fatalf("a second close after the sweep failed: %v", err)
	}
}

func TestLastInitErrorRecordsAndClears(t *testing.T) {
	defer setLastInitError("")
	if GetLastInitError() != "" {
		t.Fatal("init error recorded before any failure")
	}
	setLastInitError("OperationalError: database is locked")
	if got := GetLastInitError(); got != "OperationalError: database is locked" {
		t.Fatalf("GetLastInitError = %q", got)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			setLastInitError(fmt.Sprintf("OperationalError: attempt %d", i))
			_ = GetLastInitError()
		})
	}
	wg.Wait()
	if !strings.HasPrefix(GetLastInitError(), "OperationalError: attempt ") {
		t.Fatalf("concurrent writers lost the record: %q", GetLastInitError())
	}
	setLastInitError("")
	if GetLastInitError() != "" {
		t.Fatal("clearing left a recorded error")
	}
}
