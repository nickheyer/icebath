package state

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const sessionDBChildHelperEnv = "ICEBATH_SESSION_DB_CHILD_HELPER"

// Real platform state root or a skip when this machine resolves none
func requireRealStateRoot(t *testing.T) string {
	t.Helper()
	root, ok := realPlatformStateRoot()
	if !ok {
		t.Skip("no real platform state root to assert against")
	}
	return root
}

// Asserts a construction refused by the live system guard as a RuntimeError
func assertGuardRefused(t *testing.T, dbPath string, readOnly bool) {
	t.Helper()
	db, err := NewSessionDB(context.Background(), dbPath, readOnly)
	if err == nil {
		db.Close()
		t.Fatalf("opened production path %s", dbPath)
	}
	if !strings.Contains(err.Error(), "live-system guard") || pyExceptionClassName(err) != "RuntimeError" {
		t.Fatalf("refusal %s: %v", pyExceptionClassName(err), err)
	}
}

func TestExplicitProductionDBPathRaises(t *testing.T) {
	assertGuardRefused(t, filepath.Join(requireRealStateRoot(t), "state.db"), false)
}

func TestProductionProfileDBPathRaises(t *testing.T) {
	assertGuardRefused(t, filepath.Join(requireRealStateRoot(t), "profiles", "work", "state.db"), false)
}

func TestReadOnlyOpenOfProductionDBRaises(t *testing.T) {
	assertGuardRefused(t, filepath.Join(requireRealStateRoot(t), "state.db"), true)
}

func TestUnnormalizedProductionPathRaises(t *testing.T) {
	root := requireRealStateRoot(t)
	sneaky := filepath.Dir(root) + string(filepath.Separator) + "subdir" + string(filepath.Separator) + ".." +
		string(filepath.Separator) + filepath.Base(root) + string(filepath.Separator) + "state.db"
	assertGuardRefused(t, sneaky, false)
}

func TestDefaultResolutionToProductionRaises(t *testing.T) {
	root := requireRealStateRoot(t)
	t.Setenv("ICEBATH_HOME", root)
	saved := DefaultDBPath
	DefaultDBPath = ""
	defer func() { DefaultDBPath = saved }()
	assertGuardRefused(t, "", false)
}

func TestTmpHomeDefaultResolutionWorks(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "hermetic-home"))
	saved := DefaultDBPath
	DefaultDBPath = ""
	defer func() { DefaultDBPath = saved }()
	db, err := NewSessionDB(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if !strings.Contains(db.Path(), tmp) {
		t.Fatalf("db path %s outside %s", db.Path(), tmp)
	}
}

func TestBypassMarkerDisablesStateDBGuard(t *testing.T) {
	root := requireRealStateRoot(t)
	stateDBGuardMu.Lock()
	saved := stateDBGuardBypass
	stateDBGuardBypass = true
	stateDBGuardMu.Unlock()
	defer func() {
		stateDBGuardMu.Lock()
		stateDBGuardBypass = saved
		stateDBGuardMu.Unlock()
	}()
	if err := ensureTestIsolation(filepath.Join(root, "state.db")); err != nil {
		t.Fatal(err)
	}
}

func TestSessionDBChildHelperProcess(t *testing.T) {
	if os.Getenv(sessionDBChildHelperEnv) != "1" {
		return
	}
	db, err := NewSessionDB(context.Background(), "", false)
	if err != nil {
		fmt.Fprintln(os.Stderr, pyExceptionClassName(err)+": "+err.Error())
		os.Exit(1)
	}
	db.Close()
	fmt.Println("OK", db.Path())
	os.Exit(0)
}

// Runs an argless SessionDB construction in a child of the test binary with env
func runSessionDBChild(t *testing.T, env []string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSessionDBChildHelperProcess$")
	cmd.Env = append(env, sessionDBChildHelperEnv+"=1", "PYTEST_CURRENT_TEST=tests/fake.py::test_child (call)")
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, stdout.String(), stderr.String()
}

// Environment without the given keys
func envWithout(keys ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, k := range keys {
			drop = drop || key == k
		}
		if !drop {
			env = append(env, kv)
		}
	}
	return env
}

func TestChildWithoutHomeIsRefused(t *testing.T) {
	requireRealStateRoot(t)
	code, _, stderr := runSessionDBChild(t, envWithout("ICEBATH_HOME", stateDBGuardBypassEnv))
	if code == 0 || !strings.Contains(stderr, "live-system guard") {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
}

func TestChildWithTmpHomeSucceeds(t *testing.T) {
	env := append(envWithout("ICEBATH_HOME"), "ICEBATH_HOME="+filepath.Join(t.TempDir(), "child-home"))
	code, stdout, stderr := runSessionDBChild(t, env)
	if code != 0 || !strings.Contains(stdout, "OK") {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}
