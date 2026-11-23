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
	if err := config.SetConfigValue(context.Background(), h, "terminal.singularity_image", "docker://python:3.12", moaCommandSources()); err != nil {
		t.Fatal(err, errOut)
	}
	if err := config.GetConfigValue(context.Background(), h, "terminal.singularity_image", false, false, moaCommandSources()); err != nil {
		t.Fatal(err, errOut)
	}
	if !strings.Contains(out.String(), "docker://python:3.12") || strings.Contains(out.String()+errOut.String(), "not a recognized config key") {
		t.Fatalf("%q %q", out, errOut)
	}
}
