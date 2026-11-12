package provider

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Fresh attempt memo, no TERMINAL_ENV, and an empty managed dir, returning the managed dir
func singularityFresh(t *testing.T) (string, string) {
	t.Helper()
	leftCoreAttemptedMu.Lock()
	prev := leftCoreAttempted
	leftCoreAttempted = map[string]bool{}
	leftCoreAttemptedMu.Unlock()
	t.Cleanup(func() {
		leftCoreAttemptedMu.Lock()
		leftCoreAttempted = prev
		leftCoreAttemptedMu.Unlock()
	})
	t.Setenv("TERMINAL_ENV", "")
	os.Unsetenv("TERMINAL_ENV")
	tmp := t.TempDir()
	managed := filepath.Join(tmp, "managed")
	if err := os.MkdirAll(managed, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvManagedDir, managed)
	return tmp, managed
}

func TestSingularityInUseIsTheConfiguredBackend(t *testing.T) {
	rows := []struct {
		env, cfg string
		inUse    bool
	}{
		{"", "terminal:\n  backend: singularity\n", true},
		{"", "terminal:\n  env_type: singularity\n", true},
		{"TERMINAL_ENV=singularity\n", "", true},
		{"TERMINAL_ENV=singularity\n", "terminal:\n  backend: docker\n", false},
		{"", "terminal:\n  backend: docker\n  singularity_image: docker://x\n", false},
		{"", "", false},
	}
	for i, row := range rows {
		tmp, _ := singularityFresh(t)
		if got := singularityInUse(leftCoreHome(t, tmp, "home", row.env, row.cfg), false); got != row.inUse {
			t.Fatalf("row %d got %v", i, got)
		}
	}
}

func TestTheManagedEnvBeatsTheHomesEnvAsInLoadHermesDotenv(t *testing.T) {
	rows := []struct {
		homeEnv, managedEnv string
		inUse               bool
	}{
		{"TERMINAL_ENV=local\n", "TERMINAL_ENV=singularity\n", true},
		{"TERMINAL_ENV=singularity\n", "TERMINAL_ENV=local\n", false},
		{"", "TERMINAL_ENV=singularity\n", true},
	}
	for i, row := range rows {
		tmp, managed := singularityFresh(t)
		writeLeftCoreFile(t, filepath.Join(managed, ".env"), row.managedEnv)
		if got := singularityInUse(leftCoreHome(t, tmp, "home", row.homeEnv, ""), false); got != row.inUse {
			t.Fatalf("row %d got %v", i, got)
		}
	}
}

func TestDotenvReferencesResolveAsInLoadHermesDotenv(t *testing.T) {
	for i, env := range []string{
		"HPC_BACKEND=singularity\nTERMINAL_ENV=${HPC_BACKEND}\n",
		"TERMINAL_ENV=${SITE_BACKEND}\n",
		"TERMINAL_ENV=${UNSET_BACKEND:-singularity}\n",
	} {
		tmp, _ := singularityFresh(t)
		t.Setenv("SITE_BACKEND", "singularity")
		t.Setenv("UNSET_BACKEND", "")
		os.Unsetenv("UNSET_BACKEND")
		if !singularityInUse(leftCoreHome(t, tmp, "home", env, ""), false) {
			t.Fatalf("row %d", i)
		}
	}
}

func TestProcessEnvBackendCountsOnlyForTheLaunchHome(t *testing.T) {
	tmp, _ := singularityFresh(t)
	launch := leftCoreHome(t, tmp, "home", "", "")
	secondary := leftCoreHome(t, tmp, filepath.Join("profiles", "work"), "", "")
	t.Setenv(home.EnvHome, launch)
	home.ResetKeyCache()
	t.Cleanup(home.ResetKeyCache)
	t.Setenv("TERMINAL_ENV", "singularity")
	if singularityInUse(launch, false) || !singularityInUse(launch, true) || singularityInUse(secondary, true) {
		t.Fatal(singularityInUse(launch, false), singularityInUse(launch, true), singularityInUse(secondary, true))
	}
}

func TestASingularityHomeGetsThePluginOnce(t *testing.T) {
	tmp, _ := singularityFresh(t)
	h := leftCoreHome(t, tmp, "home", "", "terminal:\n  backend: singularity\n  singularity_image: docker://python:3.12\n")
	var calls, said []string
	install := func(name string) (map[string]any, error) {
		calls = append(calls, name)
		mkPluginDir(t, h, name)
		return map[string]any{"ok": true}, nil
	}
	say := func(m string) { said = append(said, m) }
	if got := MigrateLeftCoreHome(context.Background(), h, install, say, false); len(got) != 1 || got[0] != "singularity" {
		t.Fatalf("installed %v", got)
	}
	if len(calls) != 1 || calls[0] != "singularity" || !strings.Contains(said[0], "Singularity/Apptainer terminal backend moved out of core") ||
		!strings.Contains(said[0], "terminal.singularity_image") {
		t.Fatalf("calls %v said %v", calls, said)
	}
	said = nil
	if got := MigrateLeftCoreHome(context.Background(), h, install, say, false); len(got) != 0 || len(calls) != 1 || len(said) != 0 {
		t.Fatalf("second pass installed %v calls %v said %v", got, calls, said)
	}
}

// Terminal backend stand in a plugin would register under a name core gave up
type standInTerminalBackend struct{ name string }

func (b *standInTerminalBackend) Name() string                           { return b.name }
func (b *standInTerminalBackend) DisplayName() string                    { return b.name }
func (b *standInTerminalBackend) Description() string                    { return "" }
func (b *standInTerminalBackend) EnvDescription() string                 { return "" }
func (b *standInTerminalBackend) IsRemote() bool                         { return false }
func (b *standInTerminalBackend) IsContainer() bool                      { return false }
func (b *standInTerminalBackend) SkipContainerGuards() bool              { return false }
func (b *standInTerminalBackend) CachePathBase() string                  { return "" }
func (b *standInTerminalBackend) DefaultCwd() string                     { return "" }
func (b *standInTerminalBackend) StripEnvKeys() []string                 { return nil }
func (b *standInTerminalBackend) SessionIsolatedWhenNonpersistent() bool { return false }
func (b *standInTerminalBackend) SupportsStreams() bool                  { return false }
func (b *standInTerminalBackend) IsAvailable() bool                      { return true }
func (b *standInTerminalBackend) CheckRequirements(*tools.Env, map[string]any) error {
	return nil
}
func (b *standInTerminalBackend) Probe() (string, string)               { return "ok", "" }
func (b *standInTerminalBackend) SetupInstructions(*tools.Env) []string { return nil }
func (b *standInTerminalBackend) DoctorChecks() []tools.DoctorCheck     { return nil }
func (b *standInTerminalBackend) CreateEnvironment(context.Context, *tools.Env, tools.CreateOptions) (tools.Environment, error) {
	return nil, nil
}

func TestTheBackendNameIsFreeForThePlugin(t *testing.T) {
	previous := tools.SnapshotTerminalBackend("singularity")
	stand := &standInTerminalBackend{"singularity"}
	if err := tools.RegisterTerminalBackend(stand); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if !tools.RestoreTerminalBackend("singularity", stand, previous) {
			t.Fatal("restore refused")
		}
	}()
	if got := tools.GetTerminalBackend("singularity"); got != tools.BackendProvider(stand) {
		t.Fatalf("got %#v", got)
	}
}

func TestWithoutThePluginTheBackendErrorNamesTheInstallCommand(t *testing.T) {
	tmp, _ := singularityFresh(t)
	h := leftCoreHome(t, tmp, "home", "", "")
	t.Setenv(home.EnvHome, h)
	home.ResetKeyCache()
	t.Cleanup(home.ResetKeyCache)
	prevHint := tools.TerminalBackendInstallHint
	tools.TerminalBackendInstallHint = TerminalBackendInstallHint
	t.Cleanup(func() { tools.TerminalBackendInstallHint = prevHint })
	t.Setenv("TERMINAL_ENV", "singularity")
	env := &tools.Env{Home: h, Values: &sync.Map{}}
	if tools.CheckTerminalRequirements(env) {
		t.Fatal("an unknown backend passes the requirements check")
	}
	reason := tools.TerminalBackendUnavailableReason()
	if !strings.Contains(reason, "moved out of core into the 'singularity' plugin") || !strings.Contains(reason, "icebath plugins install singularity") {
		t.Fatal(reason)
	}
	_, err := tools.CreateTaskEnvironment(context.Background(), env, map[string]any{"env_type": "singularity", "image": "", "cwd": "/root", "timeout": 10}, "task")
	if err == nil || !strings.Contains(err.Error(), "icebath plugins install singularity") {
		t.Fatal(err)
	}
	if hint := TerminalBackendInstallHint(context.Background(), "acmebox"); hint != "" {
		t.Fatal(hint)
	}
}

// Binds the home as the process home with the config facade output captured
func singularityFacadeHome(t *testing.T, env, cfg string) (string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	tmp, _ := singularityFresh(t)
	h := leftCoreHome(t, tmp, "home", env, cfg)
	t.Setenv(home.EnvHome, h)
	home.ResetKeyCache()
	config.ResetCache()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	prevOut, prevErr, prevParse := config.FacadeStdout, config.FacadeNoticeOut, config.ParseFailureOut
	config.FacadeStdout, config.FacadeNoticeOut, config.ParseFailureOut = out, errOut, errOut
	t.Cleanup(func() {
		config.FacadeStdout, config.FacadeNoticeOut, config.ParseFailureOut = prevOut, prevErr, prevParse
		config.ResetCache()
		home.ResetKeyCache()
	})
	return h, out, errOut
}

func TestLeftCoreSingularityConfigUnsetRemovesTheEnvCopyCoreUsedToWrite(t *testing.T) {
	h, _, errOut := singularityFacadeHome(t, "TERMINAL_SINGULARITY_IMAGE=docker://old:1\n",
		"terminal:\n  backend: singularity\n  singularity_image: docker://old:1\n")
	if err := config.UnsetConfigValue(context.Background(), h, "terminal.singularity_image", moaCommandSources()); err != nil {
		t.Fatal(err, errOut)
	}
	cfg, _ := os.ReadFile(filepath.Join(h, "config.yaml"))
	env, _ := os.ReadFile(filepath.Join(h, ".env"))
	if strings.Contains(string(cfg), "singularity_image") || strings.Contains(string(env), "TERMINAL_SINGULARITY_IMAGE") {
		t.Fatalf("%s\n%s", cfg, env)
	}
}

func TestLeftCoreSingularityConfigSetAndGetStillRecognizeTheImageKey(t *testing.T) {
	h, out, errOut := singularityFacadeHome(t, "", "terminal:\n  backend: singularity\n")
	if err := config.SetConfigValue(context.Background(), h, "terminal.singularity_image", "docker://python:3.12", false, moaCommandSources()); err != nil {
		t.Fatal(err, errOut)
	}
	if err := config.GetConfigValue(context.Background(), h, "terminal.singularity_image", false, false, moaCommandSources()); err != nil {
		t.Fatal(err, errOut)
	}
	if !strings.Contains(out.String(), "docker://python:3.12") || strings.Contains(out.String()+errOut.String(), "not a recognized config key") {
		t.Fatalf("%q %q", out, errOut)
	}
}
