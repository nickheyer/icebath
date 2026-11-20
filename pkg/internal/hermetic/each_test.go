package hermetic

import (
	"os"
	"testing"
)

// What the leaking test below left for the teardown check to find
var leak struct {
	setupSeen  bool
	cleanupRan bool
	tempDir    string
	startDir   string
}

const leakTestName = "TestTeardownsFollowTheCleanupsAndPrecedeTempDirRemoval"

// Names of the tests and subtests whose fixture setup ran, in order
var setups []string

// First fixture of the binary, so its teardown runs after the environment and working directory are put back
func verifyLeakTeardown(t *testing.T) func() {
	setups = append(setups, t.Name())
	if t.Name() != leakTestName {
		return nil
	}
	leak.setupSeen = true
	return func() {
		if !leak.cleanupRan {
			t.Error("the fixture teardown ran ahead of the cleanup the test registered")
		}
		if _, err := os.Stat(leak.tempDir); err != nil {
			t.Errorf("the temp dir was removed ahead of the fixture teardown: %v", err)
		}
		if v, ok := os.LookupEnv("HERMETIC_LEAKED"); ok {
			t.Errorf("leaked variable survived the test as %q", v)
		}
		if os.Getenv("HERMETIC_BASELINE") != "kept" {
			t.Errorf("baseline variable left as %q", os.Getenv("HERMETIC_BASELINE"))
		}
		if now, _ := os.Getwd(); now != leak.startDir {
			t.Errorf("working directory left at %s, started at %s", now, leak.startDir)
		}
	}
}

func TestMain(m *testing.M) {
	os.Setenv("HERMETIC_BASELINE", "kept")
	AroundEachTest(m, verifyLeakTeardown, Environ, WorkingDir)
	os.Exit(m.Run())
}

func TestTeardownsFollowTheCleanupsAndPrecedeTempDirRemoval(t *testing.T) {
	if !leak.setupSeen {
		t.Fatal("the fixture setup did not run ahead of the test")
	}
	leak.startDir, _ = os.Getwd()
	leak.tempDir = t.TempDir()
	t.Cleanup(func() { leak.cleanupRan = true })
	os.Setenv("HERMETIC_LEAKED", "1")
	os.Setenv("HERMETIC_BASELINE", "overwritten")
	if err := os.Chdir(leak.tempDir); err != nil {
		t.Fatal(err)
	}
}

func TestRunPutsASubtestInsideTheFixtures(t *testing.T) {
	leaked := false
	Run(t, "case", func(t *testing.T) {
		if last := setups[len(setups)-1]; last != t.Name() {
			t.Fatalf("the last fixture setup was for %s", last)
		}
		os.Setenv("HERMETIC_SUBTEST_LEAK", "1")
		leaked = true
	})
	if _, ok := os.LookupEnv("HERMETIC_SUBTEST_LEAK"); ok || !leaked {
		t.Fatalf("subtest ran %v and its leaked variable survived it", leaked)
	}
}

func TestSplitEnvironKeepsAWindowsDriveKeyWhole(t *testing.T) {
	for entry, want := range map[string][2]string{"A=b": {"A", "b"}, "A=": {"A", ""}, "A=b=c": {"A", "b=c"}, "=C:=C:\\x": {"=C:", "C:\\x"}, "A": {"A", ""}, "": {"", ""}} {
		if key, value := splitEnviron(entry); key != want[0] || value != want[1] {
			t.Errorf("%q split into %q %q", entry, key, value)
		}
	}
}
