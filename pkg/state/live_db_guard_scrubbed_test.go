package state

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const guardProbeHelperEnv = "ICEBATH_GUARD_PROBE_HELPER"

// Child half of the scrubbed environment probes, printing whether the guard refuses the real state root
func TestGuardProbeHelperProcess(t *testing.T) {
	if os.Getenv(guardProbeHelperEnv) != "1" {
		return
	}
	root, ok := realPlatformStateRoot()
	switch {
	case !ok:
		fmt.Println("NO-ROOT")
	case ensureTestIsolation(filepath.Join(root, "state.db")) != nil:
		fmt.Println("REFUSED")
	default:
		fmt.Println("ALLOWED")
	}
	os.Exit(0)
}

// Environment of a child rebuilt from scratch, without PYTEST_* nor ICEBATH_HOME nor the isolation marker
func scrubbedEnv(overrides ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "PYTEST_") || key == "ICEBATH_HOME" || key == testIsolationMarkerEnv || key == stateDBGuardBypassEnv {
			continue
		}
		env = append(env, kv)
	}
	return append(env, overrides...)
}

func runGuardProbe(t *testing.T, env []string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestGuardProbeHelperProcess$")
	cmd.Env = append(env, guardProbeHelperEnv+"=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("probe failed: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	verdict := strings.TrimSpace(lines[len(lines)-1])
	if verdict == "NO-ROOT" {
		t.Skip("no real platform state root resolvable on this machine")
	}
	if verdict != "REFUSED" && verdict != "ALLOWED" {
		t.Fatalf("probe produced no verdict: %q", out)
	}
	return verdict
}

func TestChildWithoutTestEnvStillRefusesProductionDB(t *testing.T) {
	if got := runGuardProbe(t, scrubbedEnv()); got != "REFUSED" {
		t.Fatalf("verdict %s", got)
	}
}

func TestChildInheritingTestEnvStillRefusesProductionDB(t *testing.T) {
	var env []string
	for _, kv := range os.Environ() {
		if key, _, _ := strings.Cut(kv, "="); key != "ICEBATH_HOME" && key != stateDBGuardBypassEnv {
			env = append(env, kv)
		}
	}
	if os.Getenv("PYTEST_CURRENT_TEST") == "" {
		env = append(env, "PYTEST_CURRENT_TEST=tests/x.py::test_x (call)")
	}
	if got := runGuardProbe(t, env); got != "REFUSED" {
		t.Fatalf("verdict %s", got)
	}
}

func TestEnvBypassLetsADeliberateChildThrough(t *testing.T) {
	if got := runGuardProbe(t, scrubbedEnv(stateDBGuardBypassEnv+"=1")); got != "ALLOWED" {
		t.Fatalf("verdict %s", got)
	}
}
