package state

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const markerProbeHelperEnv = "ICEBATH_MARKER_PROBE_HELPER"

// Child half of the marker probes, printing the environment arming verdict and whether the guard fired
func TestMarkerProbeHelperProcess(t *testing.T) {
	if os.Getenv(markerProbeHelperEnv) != "1" {
		return
	}
	root, _ := realPlatformStateRoot()
	fired := ensureTestIsolation(filepath.Join(root, "state.db")) != nil
	out, _ := json.Marshal(map[string]bool{"armed": testEnvSignal(), "fired": fired})
	os.Stdout.Write(append(out, '\n'))
	os.Exit(0)
}

func spawnMarkerProbe(t *testing.T, env []string) map[string]bool {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMarkerProbeHelperProcess$")
	cmd.Env = append(env, markerProbeHelperEnv+"=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("probe failed: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var result map[string]bool
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil {
		t.Fatalf("probe output %q: %v", out, err)
	}
	return result
}

// Environment of a child rebuilt from scratch keeping only the platform basics
func minimalEnv(extra ...string) []string {
	var env []string
	for _, key := range []string{"PATH", "HOME", "SYSTEMROOT", "LOCALAPPDATA"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	return append(env, extra...)
}

func TestMarkerAloneReportsTestContext(t *testing.T) {
	t.Setenv("PYTEST_CURRENT_TEST", "")
	t.Setenv("PYTEST_VERSION", "")
	t.Setenv(testIsolationMarkerEnv, "/tmp/some-isolation-root")
	if !testEnvSignal() || !runningUnderTest() {
		t.Fatal("the isolation marker alone did not arm the environment signal")
	}
}

func TestNoSignalsReportsProduction(t *testing.T) {
	t.Setenv("PYTEST_CURRENT_TEST", "")
	t.Setenv("PYTEST_VERSION", "")
	t.Setenv(testIsolationMarkerEnv, "")
	if testEnvSignal() {
		t.Fatal("an environment without pytest variables or the marker armed the guard")
	}
}

func TestChildWithRebuiltEnvKeepingMarkerRefusesProductionDB(t *testing.T) {
	result := spawnMarkerProbe(t, minimalEnv(testIsolationMarkerEnv+"=/tmp/pytest-isolation-root"))
	if !result["armed"] || !result["fired"] {
		t.Fatalf("marker carrying child %v, the escape is open again", result)
	}
}

func TestChildBypassEnvDisarmsGuardEvenWithMarker(t *testing.T) {
	result := spawnMarkerProbe(t, minimalEnv(testIsolationMarkerEnv+"=/tmp/pytest-isolation-root", stateDBGuardBypassEnv+"=1"))
	if result["fired"] {
		t.Fatalf("bypass child %v", result)
	}
}

func TestChildInheritingFullTestEnvRefusesProductionDB(t *testing.T) {
	t.Setenv(testIsolationMarkerEnv, t.TempDir())
	var env []string
	for _, kv := range os.Environ() {
		if key, _, _ := strings.Cut(kv, "="); key != stateDBGuardBypassEnv {
			env = append(env, kv)
		}
	}
	result := spawnMarkerProbe(t, env)
	if !result["armed"] || !result["fired"] {
		t.Fatalf("inheriting child %v", result)
	}
}
