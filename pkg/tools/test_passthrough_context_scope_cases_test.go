package tools

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Passthrough world with the context scope double installed and the names registered, none set in the process env
func contextScopeWorld(t *testing.T, names ...string) *Env {
	t.Helper()
	installTestEnvHooks(t)
	env := passthroughEnv(t, nil)
	prev := ProfileSecrets
	ProfileSecrets = ctxProfileSecrets{}
	t.Cleanup(func() { ProfileSecrets = prev })
	for _, name := range names {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	RegisterEnvPassthrough(env, names)
	return env
}

func TestScrubbedChildEnvUnderMultiplexResolvesPassthroughThroughTheContextScope(t *testing.T) {
	served := contextScopeWorld(t, "SERVICE_TOKEN", "SCOPE_ONLY_TOKEN")
	t.Setenv("SERVICE_TOKEN", "launch-value")
	SetMultiplexActive(true)
	t.Cleanup(func() { SetMultiplexActive(false) })
	ctx := context.WithValue(context.Background(), scopeKey{}, map[string]string{"SERVICE_TOKEN": "served-value", "SCOPE_ONLY_TOKEN": "served-only"})
	child, err := BuildSubprocessEnv(ctx, &Env{Home: served.Home, Config: served.Config}, SubprocessEnvOptions{InheritProfileHome: true, ScrubSecrets: true})
	if err != nil {
		t.Fatalf("child env of a call whose context binds the served scope failed: %v", err)
	}
	if child["SERVICE_TOKEN"] != "served-value" || child["SCOPE_ONLY_TOKEN"] != "served-only" {
		t.Fatalf("child saw SERVICE_TOKEN %q SCOPE_ONLY_TOKEN %q, want the context scope's values", child["SERVICE_TOKEN"], child["SCOPE_ONLY_TOKEN"])
	}
}

func TestScrubbedChildEnvOfASingleProfileTaskOverlaysTheContextScope(t *testing.T) {
	routed := contextScopeWorld(t, "SERVICE_TOKEN")
	t.Setenv("SERVICE_TOKEN", "launch-value")
	ctx := context.WithValue(context.Background(), scopeKey{}, map[string]string{"SERVICE_TOKEN": "scoped-value"})
	child, err := BuildSubprocessEnv(ctx, &Env{Home: routed.Home, Config: routed.Config}, SubprocessEnvOptions{InheritProfileHome: true, ScrubSecrets: true})
	if err != nil || child["SERVICE_TOKEN"] != "scoped-value" {
		t.Fatalf("child saw SERVICE_TOKEN %q (%v), want the bound scope's value over the process env", child["SERVICE_TOKEN"], err)
	}
}

type multiplexTaskKey struct{}

func TestScrubbedChildEnvOfAMultiplexTaskDropsTheLaunchValueItsScopeLacks(t *testing.T) {
	routed := contextScopeWorld(t, "SERVICE_TOKEN")
	t.Setenv("SERVICE_TOKEN", "launch-value")
	prev := config.IsMultiplexActive
	config.IsMultiplexActive = func(ctx context.Context) bool { return ctx.Value(multiplexTaskKey{}) == true }
	t.Cleanup(func() { config.IsMultiplexActive = prev })
	ctx := context.WithValue(context.Background(), scopeKey{}, map[string]string{"OTHER_TOKEN": "x"})
	ctx = context.WithValue(ctx, multiplexTaskKey{}, true)
	child, err := BuildSubprocessEnv(ctx, &Env{Home: routed.Home, Config: routed.Config}, SubprocessEnvOptions{InheritProfileHome: true, ScrubSecrets: true})
	if err != nil {
		t.Fatal(err)
	}
	if value, leaked := child["SERVICE_TOKEN"]; leaked {
		t.Fatalf("a task under multiplex semantics forwarded the launch process value %q its scope does not hold", value)
	}
}

// Context of a task under multiplex semantics in a non multiplexing process, its scope holding the mapping
func multiplexTaskContext(t *testing.T, mapping map[string]string) context.Context {
	t.Helper()
	prev := config.IsMultiplexActive
	config.IsMultiplexActive = func(ctx context.Context) bool { return ctx.Value(multiplexTaskKey{}) == true }
	t.Cleanup(func() { config.IsMultiplexActive = prev })
	return context.WithValue(context.WithValue(context.Background(), scopeKey{}, mapping), multiplexTaskKey{}, true)
}

func TestStripLaunchProfileChildEnvOfAMultiplexTaskOverlaysItsScope(t *testing.T) {
	routed := contextScopeWorld(t, "SERVICE_TOKEN")
	ctx := multiplexTaskContext(t, map[string]string{"SERVICE_TOKEN": "routed-value"})
	child, err := BuildSubprocessEnv(ctx, &Env{Home: routed.Home, Config: routed.Config}, SubprocessEnvOptions{InheritProfileHome: true, StripLaunchProfile: true})
	if err != nil || child["SERVICE_TOKEN"] != "routed-value" {
		t.Fatalf("launch stripped child of a multiplex task saw SERVICE_TOKEN %q (%v), want its scope's routed-value", child["SERVICE_TOKEN"], err)
	}
}

func TestServedChildEnvOfAMultiplexTaskWithNoScopeOrTargetFailsClosed(t *testing.T) {
	routed := contextScopeWorld(t)
	prev := config.IsMultiplexActive
	config.IsMultiplexActive = func(ctx context.Context) bool { return ctx.Value(multiplexTaskKey{}) == true }
	t.Cleanup(func() { config.IsMultiplexActive = prev })
	ctx := context.WithValue(context.Background(), multiplexTaskKey{}, true)
	if child, err := ServedProfileChildEnv(ctx, &Env{Home: home.RoutingProcess(), Config: routed.Config}, nil, "", true); !errors.Is(err, ErrUnscopedSecret) {
		t.Fatalf("served child env of an unscoped multiplex task inherited credentials %d names, error %v", len(child), err)
	}
}

// Fake executable recording its argv one per line and one variable of its environment beside it
func recordingExecutable(t *testing.T, name, variable string) (path, argvFile, valueFile string) {
	t.Helper()
	dir := t.TempDir()
	path, argvFile, valueFile = filepath.Join(dir, name), filepath.Join(dir, "argv"), filepath.Join(dir, "value")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argvFile + "'\nprintf '%s' \"${" + variable + "-unset}\" > '" + valueFile + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, argvFile, valueFile
}

// Waits for a spawned backend command and returns the argv and variable its fake executable recorded
func recordedRun(t *testing.T, h ProcessHandle, err error, argvFile, valueFile string) (string, string) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Wait(10); err != nil {
		t.Fatal(err)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	value, err := os.ReadFile(valueFile)
	if err != nil {
		t.Fatal(err)
	}
	return string(argv), string(value)
}

func TestSSHCommandOfAMultiplexTaskUnsetsTheLaunchValueItsScopeLacks(t *testing.T) {
	routed := contextScopeWorld(t, "SKILL_TOKEN")
	t.Setenv("SKILL_TOKEN", "launch-skill")
	ctx := multiplexTaskContext(t, map[string]string{"OTHER_TOKEN": "x"})
	ssh, argvFile, valueFile := recordingExecutable(t, "ssh", "SKILL_TOKEN")
	t.Setenv("PATH", filepath.Dir(ssh)+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := &sshEnv{host: "h", user: "u", port: 22, controlSocket: filepath.Join(t.TempDir(), "abcdef0123456789.sock")}
	s.env = &Env{Home: routed.Home, Config: routed.Config}
	h, err := s.runBash(ctx, "echo hi", false, 10, nil)
	argv, _ := recordedRun(t, h, err, argvFile, valueFile)
	if strings.Contains(argv, "SendEnv=SKILL_TOKEN") || !strings.Contains(argv, "unset SKILL_TOKEN") {
		t.Fatalf("ssh command of a multiplex task forwarded the launch SKILL_TOKEN instead of unsetting it:\n%s", argv)
	}
}

func TestDockerCommandForwardsThePassthroughValueOfTheContextScope(t *testing.T) {
	routed := contextScopeWorld(t, "SKILL_TOKEN")
	t.Setenv("SKILL_TOKEN", "launch-skill")
	ctx := context.WithValue(context.Background(), scopeKey{}, map[string]string{"SKILL_TOKEN": "scoped-skill"})
	docker, argvFile, valueFile := recordingExecutable(t, "docker", "SKILL_TOKEN")
	d := &dockerEnv{dockerExe: docker, containerID: "c1"}
	d.env, d.profileScopedPassthrough = &Env{Home: routed.Home, Config: routed.Config}, true
	h, err := d.runBash(ctx, "echo hi", false, 10, nil)
	argv, value := recordedRun(t, h, err, argvFile, valueFile)
	if !strings.Contains(argv, "-e\nSKILL_TOKEN\n") || value != "scoped-skill" {
		t.Fatalf("docker exec forwarded SKILL_TOKEN %q with argv\n%s\nwant the context scope's scoped-skill", value, argv)
	}
}

// Context binding a scope that holds the scoped value of SKILL_TOKEN
func scopedSkillContext() context.Context {
	return context.WithValue(context.Background(), scopeKey{}, map[string]string{"SKILL_TOKEN": "scoped-skill"})
}

func TestLocalTerminalCommandOverlaysTheContextScope(t *testing.T) {
	routed := contextScopeWorld(t, "SKILL_TOKEN")
	t.Setenv("SKILL_TOKEN", "launch-skill")
	l := NewLocalEnvironment(context.Background(), &Env{Home: routed.Home, Config: routed.Config}, t.TempDir(), 60, nil, false)
	defer l.Cleanup(false)
	proc, err := l.runBash(scopedSkillContext(), `printf '%s' "$SKILL_TOKEN"`, false, 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(proc.Stdout())
	if _, err := proc.Wait(30); err != nil {
		t.Fatal(err)
	}
	if string(out) != "scoped-skill" {
		t.Fatalf("terminal command saw SKILL_TOKEN %q, want the context scope's scoped-skill", out)
	}
}

func TestBackgroundProcessOverlaysTheContextScope(t *testing.T) {
	routed := contextScopeWorld(t, "SKILL_TOKEN")
	t.Setenv("SKILL_TOKEN", "launch-skill")
	seen := filepath.Join(t.TempDir(), "seen")
	r := NewProcessRegistry()
	s, err := r.SpawnLocal(scopedSkillContext(), &Env{Home: routed.Home, Config: routed.Config}, `printf '%s' "$SKILL_TOKEN" > '`+seen+`'`, SpawnLocalOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.KillProcess(context.Background(), nil, s.ID, "", true)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(seen); err == nil && len(raw) > 0 {
			if string(raw) != "scoped-skill" {
				t.Fatalf("background process saw SKILL_TOKEN %q, want the context scope's scoped-skill", raw)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("background process never wrote what it saw")
}

func TestRipgrepChildOverlaysTheContextScope(t *testing.T) {
	routed := contextScopeWorld(t, "SKILL_TOKEN")
	t.Setenv("SKILL_TOKEN", "launch-skill")
	bin, dir := t.TempDir(), t.TempDir()
	writeSearchFile(t, filepath.Join(bin, "fakeprog"), "#!/bin/sh\nprintf '%s' \"$SKILL_TOKEN\"\n")
	if err := os.Chmod(filepath.Join(bin, "fakeprog"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ops := newShellFileOps(&Env{Home: routed.Home, Config: routed.Config}, searchHostEnv(dir, false, nil), dir).withContext(scopedSkillContext())
	if r := ops.runRgNative([]string{"fakeprog"}, 5, 5, false); r.Stdout != "scoped-skill" {
		t.Fatalf("search child saw SKILL_TOKEN %q, want the context scope's scoped-skill", r.Stdout)
	}
}

func TestDockerInitEnvResolvesThroughTheContextScope(t *testing.T) {
	routed := contextScopeWorld(t, "SKILL_TOKEN")
	t.Setenv("SKILL_TOKEN", "launch-skill")
	d := &dockerEnv{}
	d.env = &Env{Home: routed.Home, Config: routed.Config}
	if err := d.buildInitEnvArgs(scopedSkillContext()); err != nil {
		t.Fatal(err)
	}
	if d.initEnvValues["SKILL_TOKEN"] != "scoped-skill" {
		t.Fatalf("container init env holds SKILL_TOKEN %q, want the context scope's scoped-skill", d.initEnvValues["SKILL_TOKEN"])
	}
}

func TestDuckDuckGoWorkerOfAServedScopeStartsUnderMultiplex(t *testing.T) {
	ddgsWorkerKnobs(t, "empty", 10.0, func(context.Context) bool { return false })
	served := contextScopeWorld(t, "SKILL_TOKEN")
	t.Setenv("SKILL_TOKEN", "launch-skill")
	SetMultiplexActive(true)
	t.Cleanup(func() { SetMultiplexActive(false) })
	if _, err := ddgsSearchBounded(scopedSkillContext(), &Env{Home: served.Home, Config: served.Config}, "probe", 5); err != nil {
		t.Fatalf("search worker under the served scope failed: %v", err)
	}
}
