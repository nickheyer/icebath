package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Installs the locked node and npm into a pm store under the runtime override, npm recording an absent PATH dir
func fakePMNodeStore(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	t.Setenv(pm.EnvRuntimeDir, filepath.Join(tmp, "runtime"))
	ctx := context.Background()
	store, err := pm.StoreRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	factsPath, _ := pm.FactsPath(ctx)
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(factsPath, []byte(fmt.Sprintf(`{"schema": %d, "packages": {}}`, pm.LockSchema)), 0o644); err != nil {
		t.Fatal(err)
	}
	facts, err := pm.NewFacts(factsPath, false)
	if err != nil {
		t.Fatal(err)
	}
	target, err := pm.CurrentTarget()
	if err != nil {
		t.Fatal(err)
	}
	lock := pm.NewLockfile(pm.LockfilePath())
	nodeBin := ""
	for _, name := range []string{"node", "npm"} {
		version, ok := lock.Version(name)
		if !ok {
			t.Fatalf("lock.json pins no %s", name)
		}
		var shas []string
		for _, a := range lock.Artifacts(name, target) {
			shas = append(shas, fmt.Sprint(a["sha256"]))
		}
		entry := name + "-" + version
		p, err := pm.GetPackage(name)
		if err != nil {
			t.Fatal(err)
		}
		if binary := p.Binary(filepath.Join(store, entry), target); binary != "" {
			writeExecutable(t, binary, "#!/bin/sh\n")
		}
		dir := filepath.Join(store, entry, "absent-bin")
		if name == "node" {
			dir = filepath.Join(store, entry, "path-bin")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			nodeBin = dir
		}
		if err := facts.Record(name, version, entry, pm.EnvDiff{"PATH": []string{dir}}, store, pm.FactRecord{Target: &target, Artifacts: shas}); err != nil {
			t.Fatal(err)
		}
	}
	return nodeBin
}

// Writes an executable file with its parent directories
func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestManagedRuntimePathEntriesComposePMNodeAndHomeBin(t *testing.T) {
	nodeBin := fakePMNodeStore(t)
	env := newTestToolEnv(t)
	if got := managedRuntimePathEntries(env); !reflect.DeepEqual(got, []string{nodeBin}) {
		t.Fatalf("existing pm dirs only, absent npm dir and missing home bin dropped: %q", got)
	}
	bin := filepath.Join(env.Home, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := managedRuntimePathEntries(env); !reflect.DeepEqual(got, []string{nodeBin, bin}) {
		t.Fatalf("home bin appearing mid process must be picked up on the next call: %q", got)
	}
	other := newTestToolEnv(t)
	otherBin := filepath.Join(other.Home, "bin")
	if err := os.Mkdir(otherBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := managedRuntimePathEntries(other); !reflect.DeepEqual(got, []string{nodeBin, otherBin}) {
		t.Fatalf("home bin must follow the profile scoped home: %q", got)
	}
	if isWindowsHost {
		return
	}
	parts := strings.Split(appendMissingSanePathEntries(env, "/usr/bin"), ":")
	nodeAt, binAt, saneAt := slices.Index(parts, nodeBin), slices.Index(parts, bin), slices.Index(parts, "/bin")
	if saneAt < 0 || nodeAt <= saneAt || binAt <= nodeAt {
		t.Fatalf("pm node dir and home bin follow the sane path in order: %q", parts)
	}
}

func TestManagedRuntimePathEntriesEmptyWhenPMFails(t *testing.T) {
	env := newTestToolEnv(t)
	if err := os.Mkdir(filepath.Join(env.Home, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := managedNodePathEnv
	managedNodePathEnv = func(map[string]string) map[string]string { panic(errors.New("cannot read the pm store")) }
	t.Cleanup(func() { managedNodePathEnv = prev })
	if got := managedRuntimePathEntries(env); got != nil {
		t.Fatalf("a failing pm lookup yields no entries at all: %q", got)
	}
}

func TestManagedRuntimePathEntriesReadTheNodeEnvOverAnEmptyPath(t *testing.T) {
	env := newTestToolEnv(t)
	var base map[string]string
	prev := managedNodePathEnv
	managedNodePathEnv = func(b map[string]string) map[string]string {
		base = b
		return map[string]string{"PATH": "relative/../x" + string(os.PathListSeparator) + string(os.PathListSeparator) + env.Home + string(os.PathSeparator) + "."}
	}
	t.Cleanup(func() { managedNodePathEnv = prev })
	got := managedRuntimePathEntries(env)
	if !reflect.DeepEqual(base, map[string]string{"PATH": ""}) {
		t.Fatalf("base env %q", base)
	}
	if !reflect.DeepEqual(got, []string{env.Home}) {
		t.Fatalf("empty entries dropped and each dir normalized like pathlib: %q", got)
	}
}

// Points the managed runtime seams at a fake icebath bin dir with no pm entries
func stubManagedRuntime(t *testing.T, binDir string) {
	t.Helper()
	prevM, prevB := managedRuntimePathEntries, resolveIcebathBinDir
	managedRuntimePathEntries = func(*Env) []string { return nil }
	resolveIcebathBinDir = func() string { return binDir }
	t.Cleanup(func() { managedRuntimePathEntries, resolveIcebathBinDir = prevM, prevB })
}

func TestExistingUserLocalBinAppendedAfterInheritedEntries(t *testing.T) {
	if isWindowsHost {
		t.Skip("POSIX PATH completion only")
	}
	tmp := t.TempDir()
	localBin := filepath.Join(tmp, ".local", "bin")
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("PATH", "/usr/bin:/bin")
	stubManagedRuntime(t, "")
	env := newTestToolEnv(t)
	runEnv, err := makeRunEnv(env, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	entries := strings.Split(runEnv["PATH"], string(os.PathListSeparator))
	if !reflect.DeepEqual(entries[:2], []string{"/usr/bin", "/bin"}) {
		t.Fatalf("%q", entries)
	}
	if n := countOf(entries, localBin); n != 1 {
		t.Fatalf("user local bin appended %d times: %q", n, entries)
	}
	already := strings.Split(appendMissingSanePathEntries(env, localBin+":/usr/bin"), ":")
	if already[0] != localBin || countOf(already, localBin) != 1 {
		t.Fatalf("already on PATH keeps its position without a duplicate: %q", already)
	}
}

func TestMissingUserLocalBinNotAppended(t *testing.T) {
	if isWindowsHost {
		t.Skip("POSIX PATH completion only")
	}
	t.Setenv("HOME", t.TempDir())
	stubManagedRuntime(t, "")
	if got := appendMissingSanePathEntries(newTestToolEnv(t), "/usr/bin:/bin"); strings.Contains(got, ".local") {
		t.Fatalf("%q", got)
	}
}

// Occurrences of want in items
func countOf(items []string, want string) int {
	n := 0
	for _, it := range items {
		if it == want {
			n++
		}
	}
	return n
}

var snapshotPathLine = regexp.MustCompile(`(?m)^(?:declare -x|export) PATH="(.*)"$`)

// A login profile with body and one fake managed bin dir sourced through the shell init config
func managedLoginSetup(t *testing.T, tmp, profileBody string) (*Env, string) {
	t.Helper()
	requireBash(t)
	profile := filepath.Join(tmp, "profile.sh")
	if err := os.WriteFile(profile, []byte(profileBody), 0o644); err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(tmp, "managed-bin")
	if err := os.Mkdir(managed, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	stubManagedRuntime(t, managed)
	env := newTestToolEnv(t)
	env.Config = config.Defaults()
	env.Config.Set("terminal.shell_init_files", []any{profile})
	return env, managed
}

// PATH entries the session snapshot exported
func snapshotPathValue(t *testing.T, l *localEnv) []string {
	t.Helper()
	data, err := os.ReadFile(l.SnapshotPath())
	if err != nil {
		t.Fatal(err)
	}
	content := string(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
	m := snapshotPathLine.FindStringSubmatch(content)
	if m == nil {
		t.Fatalf("no PATH line in snapshot:\n%s", content)
	}
	return strings.Split(m[1], ":")
}

func TestSnapshotKeepsManagedDirWhenProfileResetsPath(t *testing.T) {
	if isWindowsHost {
		t.Skip("posix")
	}
	tmp := t.TempDir()
	env, managed := managedLoginSetup(t, tmp, "PATH=\"/usr/local/bin:/usr/bin:/bin\"\n")
	l := NewLocalEnvironment(context.Background(), env, tmp, 60, map[string]string{"PATH": managed + ":/usr/local/bin:/usr/bin:/bin"}, true)
	defer l.Cleanup(false)
	if got := snapshotPathValue(t, l); !slices.Contains(got, managed) {
		t.Fatalf("%q", got)
	}
}

func TestSnapshotDoesNotDuplicateManagedDir(t *testing.T) {
	if isWindowsHost {
		t.Skip("posix")
	}
	tmp := t.TempDir()
	env, managed := managedLoginSetup(t, tmp, ": # profile that keeps PATH\n")
	l := NewLocalEnvironment(context.Background(), env, tmp, 60, map[string]string{"PATH": managed + ":/usr/local/bin:/usr/bin:/bin"}, true)
	defer l.Cleanup(false)
	if got := snapshotPathValue(t, l); countOf(got, managed) != 1 {
		t.Fatalf("%q", got)
	}
}

func TestNonLoginPathKeepsManagedDirWithoutProfile(t *testing.T) {
	if isWindowsHost {
		t.Skip("posix")
	}
	tmp := t.TempDir()
	env, managed := managedLoginSetup(t, tmp, "PATH=\"/usr/local/bin:/usr/bin:/bin\"\n")
	l := NewLocalEnvironment(context.Background(), env, tmp, 60, map[string]string{"PATH": managed + ":/usr/local/bin:/usr/bin:/bin"}, true)
	defer l.Cleanup(false)
	proc, err := l.runBash(context.Background(), `printf "%s" "$PATH"`, false, 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(proc.Stdout())
	if _, err := proc.Wait(30); err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(string(out), ":"); !slices.Contains(got, managed) {
		t.Fatalf("%q", got)
	}
}

// Executable script in directory printing the directory's name
func echoDirTool(t *testing.T, directory, name string) {
	t.Helper()
	writeExecutable(t, filepath.Join(directory, name), "#!/bin/sh\necho "+filepath.Base(directory)+"\n")
}

func TestDroppedSpawnPathDirsReturnAfterTheLoginPath(t *testing.T) {
	if isWindowsHost {
		t.Skip("posix")
	}
	tmp := t.TempDir()
	userBin, extraA, extraB, relBin := filepath.Join(tmp, "user-bin"), filepath.Join(tmp, "extra-a"), filepath.Join(tmp, "extra-b"), filepath.Join(tmp, "rel-bin")
	echoDirTool(t, userBin, "shared-tool")
	echoDirTool(t, extraA, "shared-tool")
	echoDirTool(t, extraB, "only-in-parent")
	echoDirTool(t, relBin, "only-relative")
	env, _ := managedLoginSetup(t, tmp, "PATH=\"/usr/local/bin:/usr/bin:/bin\"\nPATH=\""+userBin+":$PATH\"\n")
	t.Chdir(userBin)
	l := NewLocalEnvironment(context.Background(), env, tmp, 60, map[string]string{"PATH": extraA + ":" + extraB + ":rel-bin:/usr/bin:/bin"}, true)
	defer l.Cleanup(false)
	entries := snapshotPathValue(t, l)
	order := []int{slices.Index(entries, userBin), slices.Index(entries, "/usr/bin"), slices.Index(entries, extraA), slices.Index(entries, extraB), slices.Index(entries, "rel-bin")}
	for i := 1; i < len(order); i++ {
		if order[i-1] < 0 || order[i-1] >= order[i] {
			t.Fatalf("login path first, then the dropped spawn dirs in parent order: %q", entries)
		}
	}
	if countOf(entries, "/usr/bin") != 1 {
		t.Fatalf("%q", entries)
	}
	res, err := l.Execute(context.Background(), "only-in-parent; shared-tool; only-relative", ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(res.Output); !reflect.DeepEqual(got, []string{"extra-b", "user-bin", "rel-bin"}) {
		t.Fatalf("%q", res.Output)
	}
}

// Environment carrying only the spawn env a local background spawn reads
type childEnvOnly struct {
	Environment
	vars map[string]string
}

func (c childEnvOnly) ChildEnv() map[string]string { return c.vars }

func TestBackgroundCommandKeepsSpawnPathDirsALoginProfileResets(t *testing.T) {
	if isWindowsHost {
		t.Skip("posix")
	}
	requireBash(t)
	tmp := t.TempDir()
	profileBin, parentBin := filepath.Join(tmp, "profile-bin"), filepath.Join(tmp, "parent-bin")
	if err := os.Mkdir(profileBin, 0o755); err != nil {
		t.Fatal(err)
	}
	echoDirTool(t, parentBin, "only-in-parent")
	loginShell := filepath.Join(tmp, "login-shell", "bash")
	writeExecutable(t, loginShell, "#!/usr/bin/env bash\nPATH=\""+profileBin+":/usr/local/bin:/usr/bin:/bin\"\nexec /bin/bash --noprofile --norc -c \"$2\"\n")
	t.Setenv("SHELL", loginShell)
	env := newTestToolEnv(t)
	s, err := spawnTracked(context.Background(), env, childEnvOnly{vars: map[string]string{"PATH": parentBin + ":/usr/bin:/bin"}}, "local",
		`only-in-parent; printf "PATH=%s\n" "$PATH"`, tmp, "t", "t", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	output := func() (string, bool) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.OutputBuffer, s.Exited
	}
	waitUntil(t, "the background session to exit", func() bool { _, done := output(); return done })
	out, _ := output()
	if !slices.Contains(strings.Fields(out), "parent-bin") {
		t.Fatalf("%q", out)
	}
	pathLine := ""
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "PATH=") {
			pathLine = strings.TrimSpace(strings.TrimPrefix(ln, "PATH="))
			break
		}
	}
	entries := strings.Split(pathLine, ":")
	if entries[0] != profileBin || slices.Index(entries, "/bin") >= slices.Index(entries, parentBin) {
		t.Fatalf("profile dirs first, parent only dirs after them: %q", entries)
	}
}
