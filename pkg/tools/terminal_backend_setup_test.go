package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Registered backend with setup lines and a scripted post setup hook
type hookedSetupBackend struct {
	TerminalEnvironmentProviderDefaults
	name  string
	lines []string
	hook  func() error
	ran   int
}

func (b *hookedSetupBackend) Name() string                           { return b.name }
func (b *hookedSetupBackend) DisplayName() string                    { return "Hooked Box" }
func (b *hookedSetupBackend) Description() string                    { return "a box with a setup hook" }
func (b *hookedSetupBackend) SetupInstructions(*Env) []string        { return b.lines }
func (b *hookedSetupBackend) SessionIsolatedWhenNonpersistent() bool { return false }
func (b *hookedSetupBackend) IsAvailable() bool                      { return true }
func (b *hookedSetupBackend) CreateEnvironment(context.Context, *Env, CreateOptions) (Environment, error) {
	return nil, nil
}
func (b *hookedSetupBackend) PostSetup(context.Context, *Env) error {
	b.ran++
	return b.hook()
}

// Registers a hooked backend for one test, restoring whatever held its name
func registerHookedBackend(t *testing.T, b *hookedSetupBackend) {
	t.Helper()
	b.Self = b
	previous := SnapshotTerminalBackend(b.name)
	if err := RegisterTerminalBackend(b); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { RestoreTerminalBackend(b.name, b, previous) })
}

// Replaces a package hook for one test
func swapSetupHook[T any](t *testing.T, slot *T, v T) {
	t.Helper()
	prev := *slot
	*slot = v
	t.Cleanup(func() { *slot = prev })
}

func TestSetupTerminalBackendRowsListRegisteredBackendsSortedWithoutBuiltIns(t *testing.T) {
	registerHookedBackend(t, &hookedSetupBackend{name: "zz-hooked", hook: func() error { return nil }})
	registerHookedBackend(t, &hookedSetupBackend{name: "aa-hooked", hook: func() error { return nil }})
	swapSetupHook(t, &EnsurePluginsDiscovered, func(context.Context) error { return nil })
	rows := SetupTerminalBackendRows(context.Background(), &Env{Home: t.TempDir()})
	names := []string{}
	for _, row := range rows {
		names = append(names, row.Name)
		if builtinBackends[row.Name] {
			t.Fatalf("built in %s listed", row.Name)
		}
	}
	first, last := strings.Index(strings.Join(names, ","), "aa-hooked"), strings.Index(strings.Join(names, ","), "zz-hooked")
	if first < 0 || last < first {
		t.Fatalf("rows %v", names)
	}
	for _, row := range rows {
		if row.Name == "aa-hooked" && (row.DisplayName != "Hooked Box" || row.Description != "a box with a setup hook") {
			t.Fatalf("row %+v", row)
		}
	}
	swapSetupHook(t, &EnsurePluginsDiscovered, func(context.Context) error { return errors.New("discovery failed") })
	if rows := SetupTerminalBackendRows(context.Background(), &Env{Home: t.TempDir()}); rows != nil {
		t.Fatalf("failed discovery listed %v", rows)
	}
}

func TestRunTerminalBackendSetupPrintsLinesAndRunsTheHook(t *testing.T) {
	ok := &hookedSetupBackend{name: "hooked-ok", lines: []string{"line one", "line two"}, hook: func() error { return nil }}
	registerHookedBackend(t, ok)
	var out strings.Builder
	if !RunTerminalBackendSetup(context.Background(), &Env{}, &out, "hooked-ok") || ok.ran != 1 {
		t.Fatalf("hook ran %d, output %q", ok.ran, out.String())
	}
	if out.String() != "Terminal backend: Hooked Box\nline one\nline two\n" {
		t.Fatalf("output %q", out.String())
	}
	failing := &hookedSetupBackend{name: "hooked-fail", hook: func() error { return errors.New("token rejected") }}
	registerHookedBackend(t, failing)
	out.Reset()
	if RunTerminalBackendSetup(context.Background(), &Env{}, &out, "hooked-fail") || !strings.HasSuffix(out.String(), "Backend plugin setup hook failed: token rejected\n") {
		t.Fatalf("output %q", out.String())
	}
	panicking := &hookedSetupBackend{name: "hooked-panic", hook: func() error { panic("hook blew up") }}
	registerHookedBackend(t, panicking)
	out.Reset()
	if RunTerminalBackendSetup(context.Background(), &Env{}, &out, "hooked-panic") || !strings.Contains(out.String(), "hook failed: hook blew up") {
		t.Fatalf("output %q", out.String())
	}
	out.Reset()
	if RunTerminalBackendSetup(context.Background(), &Env{}, &out, "no-such-backend") || !strings.Contains(out.String(), "hook failed") {
		t.Fatalf("output %q", out.String())
	}
}

func TestTestSSHConnectionBuildsTheBatchProbeAndReportsTheOutcome(t *testing.T) {
	var argv []string
	outcome := struct {
		code   int
		stderr string
		err    error
	}{}
	swapSetupHook(t, &setupRunSSH, func(_ context.Context, a []string) (int, string, error) {
		argv = a
		return outcome.code, outcome.stderr, outcome.err
	})
	var out strings.Builder
	if !TestSSHConnection(context.Background(), &out, "box", "deploy", "2222", "/k") || out.String() != "Testing connection...\nSSH connection successful!\n" {
		t.Fatalf("output %q", out.String())
	}
	want := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-i", "/k", "-p", "2222", "deploy@box", "echo ok"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv %v", argv)
	}
	outcome.code, outcome.stderr = 255, "  Permission denied (publickey).\n"
	out.Reset()
	if TestSSHConnection(context.Background(), &out, "box", "", "22", "") ||
		out.String() != "Testing connection...\nSSH connection failed: Permission denied (publickey).\nCheck your SSH key and host settings.\n" {
		t.Fatalf("output %q", out.String())
	}
	if !reflect.DeepEqual(argv, []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "box", "echo ok"}) {
		t.Fatalf("argv %v", argv)
	}
	outcome.err = errors.New("Command ['ssh'] timed out after 10 seconds")
	out.Reset()
	if TestSSHConnection(context.Background(), &out, "box", "", "", "") || !strings.Contains(out.String(), "failed: Command ['ssh'] timed out") {
		t.Fatalf("output %q", out.String())
	}
}

func TestNearestVercelProjectWalksUpToTheLinkFile(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(filepath.Join(root, ".vercel"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, ".vercel", "project.json")
	cases := []struct {
		body string
		want map[string]string
	}{
		{"\xef\xbb\xbf{\"projectId\": \"prj_1\", \"orgId\": \"team_1\"}", map[string]string{"projectId": "prj_1", "orgId": "team_1"}},
		{`{"projectId": "  ", "orgId": 7}`, map[string]string{}},
		{`["prj_1"]`, map[string]string{}},
		{`{not json`, map[string]string{}},
	}
	for _, c := range cases {
		if err := os.WriteFile(link, []byte(c.body), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := NearestVercelProject(deep)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%q gave %v %v", c.body, got, err)
		}
	}
	file := filepath.Join(deep, "main.go")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := NearestVercelProject(file); err != nil || got == nil {
		t.Fatalf("a file start gave %v %v", got, err)
	}
	if got, err := NearestVercelProject(t.TempDir()); err != nil || len(got) != 0 {
		t.Fatalf("no link file gave %v %v", got, err)
	}
}

func TestSetupDockerRuntimeNamesTheRuntimeOfTheFoundCLI(t *testing.T) {
	dockerExecutable.Lock()
	saved := dockerExecutable.path
	dockerExecutable.path = "/opt/bin/podman"
	dockerExecutable.Unlock()
	t.Cleanup(func() {
		dockerExecutable.Lock()
		dockerExecutable.path = saved
		dockerExecutable.Unlock()
	})
	if path, runtime := SetupDockerRuntime(&Env{}); path != "/opt/bin/podman" || runtime != "Podman" {
		t.Fatalf("%s %s", path, runtime)
	}
}

func TestSetupModalManagedAvailableNeedsTheManagedGate(t *testing.T) {
	swapSetupHook(t, &NousAccount, nil)
	if managed, err := SetupModalManagedAvailable(&Env{Home: t.TempDir()}, map[string]any{}); managed || err != nil {
		t.Fatalf("%v %v", managed, err)
	}
}
