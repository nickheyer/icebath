package provider

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
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

func TestLeftCoreSingularityInUseIsTheConfiguredBackend(t *testing.T) {
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

func TestLeftCoreSingularityTheManagedEnvBeatsTheHomesEnv(t *testing.T) {
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

func TestLeftCoreSingularityDotenvReferencesResolveAsInLoadDotenv(t *testing.T) {
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

func TestLeftCoreSingularityProcessEnvBackendCountsOnlyForTheLaunchHome(t *testing.T) {
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

func TestLeftCoreSingularityAHomeGetsThePluginOnce(t *testing.T) {
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

func TestLeftCoreSingularityWithoutThePluginTheBackendHintNamesTheInstallCommand(t *testing.T) {
	tmp, _ := singularityFresh(t)
	h := leftCoreHome(t, tmp, "home", "", "")
	t.Setenv(home.EnvHome, h)
	home.ResetKeyCache()
	t.Cleanup(home.ResetKeyCache)
	hint := TerminalBackendInstallHint(context.Background(), "singularity")
	if !strings.Contains(hint, "moved out of core into the 'singularity' plugin") || !strings.Contains(hint, "icebath plugins install singularity") {
		t.Fatal(hint)
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
