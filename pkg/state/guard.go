package state

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"weak"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Environment twin of the in process guard bypass, the one opt out a spawned child can carry
const stateDBGuardBypassEnv = "ICEBATH_STATE_DB_GUARD_BYPASS"

// Marker the hermetic test harness exports with its ICEBATH_HOME redirect, inherited by every child
const testIsolationMarkerEnv = "ICEBATH_TEST_ISOLATION"

// Real platform default icebath root from the account home, ignoring ICEBATH_HOME, false when it cannot resolve
func realPlatformStateRoot() (string, bool) {
	home := expandUser("~")
	var root string
	if runtime.GOOS == "windows" {
		if base := pyStrip(os.Getenv("LOCALAPPDATA")); base != "" {
			root = filepath.Join(base, "icebath")
		} else {
			root = filepath.Join(home, "AppData", "Local", "icebath")
		}
	} else {
		root = filepath.Join(home, ".icebath")
	}
	if !filepath.IsAbs(root) {
		if _, err := os.Getwd(); err != nil {
			return "", false
		}
	}
	return realpathLax(root), true
}

// Whether this process runs under pytest or go test by environment, the isolation marker or the test binary itself
func runningUnderTest() bool {
	return os.Getenv("PYTEST_CURRENT_TEST") != "" || os.Getenv("PYTEST_VERSION") != "" ||
		os.Getenv(testIsolationMarkerEnv) != "" || testing.Testing()
}

// Launcher basenames of a pytest run, matched per argv token so a pytest temp path never matches
var testRunnerLauncherNames = setOf("pytest", "py.test", "pytest.exe", "py.test.exe")

// Process ancestry and command line probes, package vars so tests fake a process tree
var (
	parentPIDs     = proc.Parents
	processCmdline = proc.Cmdline
)

// Memoised ancestry answer, the tree above this process never changes
var testRunnerAncestor struct {
	sync.Mutex
	known, found bool
}

// Whether a command line runs pytest, go test or a go test binary, never when unreadable
func processLooksLikeTestRunner(cmdline func() ([]string, error)) bool {
	argv, err := cmdline()
	if err != nil {
		return false
	}
	for i, arg := range argv {
		name := strings.Trim(strings.Trim(arg, `"`), "'")
		name = strings.ReplaceAll(name, `\`, "/")
		name = strings.ToLower(name[strings.LastIndexByte(name, '/')+1:])
		if testRunnerLauncherNames[name] {
			return true
		}
		if i == 0 && (strings.HasSuffix(name, ".test") || strings.HasSuffix(name, ".test.exe")) {
			return true
		}
		if i == 0 && (name == "go" || name == "go.exe") && len(argv) > 1 && argv[1] == "test" {
			return true
		}
	}
	return false
}

// Whether an ancestor process is a test run, which survives a child spawned with a rebuilt environment
func hasTestRunnerAncestor() bool {
	testRunnerAncestor.Lock()
	defer testRunnerAncestor.Unlock()
	if testRunnerAncestor.known {
		return testRunnerAncestor.found
	}
	found := false
	if parents, err := parentPIDs(os.Getpid()); err == nil {
		found = slices.ContainsFunc(parents, func(pid int) bool {
			return processLooksLikeTestRunner(func() ([]string, error) { return processCmdline(pid) })
		})
	}
	testRunnerAncestor.known, testRunnerAncestor.found = true, found
	return found
}

// Test run by environment or ancestry, environment checked first
func inTestContext() bool {
	return runningUnderTest() || hasTestRunnerAncestor()
}

// Whether resolved is root/state.db or root/profiles/name/state.db, deeper scratch paths never match
func isProductionStateDB(resolved, root string) bool {
	rel, ok := relativeParts(resolved, root)
	if !ok {
		return false
	}
	if len(rel) == 0 {
		clean := filepath.Clean(resolved)
		return filepath.Dir(clean) == clean
	}
	return len(rel) == 1 || (len(rel) == 3 && rel[0] == "profiles")
}

// Components of path below root like Python's relative_to, false when root is not an ancestor or path itself
func relativeParts(path, root string) ([]string, bool) {
	p, r := pathParts(path), pathParts(root)
	if len(p) < len(r) {
		return nil, false
	}
	for i := range r {
		if !samePathPart(p[i], r[i]) {
			return nil, false
		}
	}
	return p[len(r):], true
}

// Anchor and names of a cleaned path, the anchor holding the volume and root separator
func pathParts(path string) []string {
	path = filepath.Clean(path)
	vol := filepath.VolumeName(path)
	rest := path[len(vol):]
	var parts []string
	if strings.HasPrefix(rest, string(filepath.Separator)) {
		parts = append(parts, vol+string(filepath.Separator))
	} else if vol != "" {
		parts = append(parts, vol)
	}
	for _, name := range strings.Split(rest, string(filepath.Separator)) {
		if name != "" {
			parts = append(parts, name)
		}
	}
	return parts
}

// Path component equality, case folded on Windows like PureWindowsPath
func samePathPart(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.ToLower(a) == strings.ToLower(b)
	}
	return a == b
}

// Instances opened under the isolation marker, held weakly so registration never pins one
var testInstances struct {
	sync.Mutex
	set map[weak.Pointer[DB]]struct{}
}

// Tracks d for the suite leak sweep when the isolation marker is set
func registerTestInstance(d *DB) {
	if os.Getenv(testIsolationMarkerEnv) == "" {
		return
	}
	testInstances.Lock()
	defer testInstances.Unlock()
	if testInstances.set == nil {
		testInstances.set = map[weak.Pointer[DB]]struct{}{}
	}
	testInstances.set[weak.Make(d)] = struct{}{}
}

// Live registered instances, dropping the entries whose database was collected
func liveTestInstances() []*DB {
	testInstances.Lock()
	defer testInstances.Unlock()
	var out []*DB
	for ptr := range testInstances.set {
		if d := ptr.Value(); d != nil {
			out = append(out, d)
		} else {
			delete(testInstances.set, ptr)
		}
	}
	return out
}

// Closes every instance a test opened and left open, skipping handles the shared registry owns
func CloseLeakedTestInstances() {
	for _, d := range liveTestInstances() {
		if d.sharedRegistryOwned.Load() {
			continue
		}
		_ = d.Close()
	}
}

// Most recent session database init failure of this process
var lastInitError struct {
	sync.Mutex
	msg string
}

// Records the most recent init failure, the empty string clearing it
func setLastInitError(msg string) {
	lastInitError.Lock()
	lastInitError.msg = msg
	lastInitError.Unlock()
}

// Most recent state.db init failure, empty when none was recorded
func GetLastInitError() string {
	lastInitError.Lock()
	defer lastInitError.Unlock()
	return lastInitError.msg
}
